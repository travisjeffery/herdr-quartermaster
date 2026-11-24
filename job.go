package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// Durable true-async job queue for coordinator requests (docs/design/coordinator-api.md).
//
// Each submitted request persists under <state dir>/jobs/<id>/ (private 0700 dirs,
// 0600 files), following the executor's planEvent pattern (plan.go / executor.go):
//   - job.json: immutable JobSpec whose ID is the SHA-256 of canonical (key, request)
//   - events.jsonl: append-only fsynced lifecycle log (queued, claimed, intent,
//     outcome, parked, answer_applied, done, blocked, needs_tj, canceled)
//
// Supporting indexes under <state dir>/jobs/:
//   - keys/<key>.json: idempotency-key -> job ID mapping (written atomically under jobs.lock)
//   - claims/<digest>.json: atomic at-most-once answer claims keyed by (kind, bead, hash)
//   - holds/<bead>.json: durable HOLD markers cleared by RESUME
//   - jobs.lock: cross-process flock guarding fast metadata mutations
//
// The ticker is the sole background executor (no second daemon). Status, result,
// and list reads work directly from disk with no daemon or coordinator session alive.

const (
	jobSchemaVersion   = 1
	jobMaxAttempts     = 3
	jobMaxFreeTextRune = 2000
	jobTickerTimeout   = 15 * time.Second
)

// Job states match the shared coordinator-api vocabulary. Terminal states end in
// the qm-status block's state= values (done, blocked, needs_tj).
const (
	JobStateQueued  = "queued"
	JobStateRunning = "running"
	JobStateParked  = "parked"
	JobStateDone    = "done"
	JobStateBlocked = "blocked"
	JobStateNeedsTJ = "needs_tj"
)

// Gate verbs from docs/design/coordinator-api.md.
const (
	JobVerbMerge    = "MERGE"
	JobVerbDispatch = "DISPATCH"
	JobVerbClose    = "CLOSE"
	JobVerbHold     = "HOLD"
	JobVerbResume   = "RESUME"
	JobVerbStatus   = "STATUS"
	JobVerbStop     = "STOP"
	JobVerbAnswer   = "ANSWER"
)

var validJobVerbs = []string{
	JobVerbMerge,
	JobVerbDispatch,
	JobVerbClose,
	JobVerbHold,
	JobVerbResume,
	JobVerbStatus,
	JobVerbStop,
	JobVerbAnswer,
}

const (
	jobEventQueued        = "queued"
	jobEventClaimed       = "claimed"
	jobEventIntent        = "intent"
	jobEventOutcome       = "outcome"
	jobEventParked        = "parked"
	jobEventAnswerApplied = "answer_applied"
	jobEventDone          = "done"
	jobEventBlocked       = "blocked"
	jobEventNeedsTJ       = "needs_tj"
	jobEventCanceled      = "canceled"
)

var (
	errJob            = errors.New("job")
	errJobKeyConflict = errors.New("idempotency key conflict")
	errJobNotTerminal = errors.New("job has not finished")
	errJobNotCancel   = errors.New("job cannot be canceled")
)

var (
	jobKeyRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	jobIDRE     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	jobPrefixRE = regexp.MustCompile(`^[0-9a-f]{6,64}$`)
	reqTagRE    = regexp.MustCompile(`^\[req:([A-Za-z0-9][A-Za-z0-9._-]{0,63})\]$`)
)

// JobRequest is the normalized, immutable coordinator request carried by a job.
type JobRequest struct {
	Verb     string `json:"verb"`
	Target   string `json:"target,omitempty"`
	Choice   string `json:"choice,omitempty"`
	FreeText string `json:"free_text,omitempty"`
	Why      string `json:"why,omitempty"`
	Expect   string `json:"expect,omitempty"`
	Class    int    `json:"class"`
	By       string `json:"by"`
}

// JobSpec is stored immutably at <state dir>/jobs/<id>/job.json.
type JobSpec struct {
	Version   int        `json:"version"`
	ID        string     `json:"id"`
	Key       string     `json:"key"`
	Request   JobRequest `json:"request"`
	CreatedAt time.Time  `json:"created_at"`
}

func (r JobRequest) digest() (string, error) {
	b, err := canonicalJSON(r)
	if err != nil {
		return "", err
	}
	return sha256Hex(b), nil
}

// digest hashes (version, key, request) canonically, excluding CreatedAt and ID,
// so the same (key, request) deterministically maps to the same job ID.
func (s JobSpec) digest() (string, error) {
	body := struct {
		Version int        `json:"version"`
		Key     string     `json:"key"`
		Request JobRequest `json:"request"`
	}{
		Version: s.Version,
		Key:     s.Key,
		Request: s.Request,
	}
	b, err := canonicalJSON(body)
	if err != nil {
		return "", err
	}
	return sha256Hex(b), nil
}

// jobEvent is one fsynced JSONL record in <state dir>/jobs/<id>/events.jsonl.
type jobEvent struct {
	At           time.Time `json:"at"`
	Kind         string    `json:"kind"`
	Attempt      int       `json:"attempt,omitempty"`
	Bead         string    `json:"bead,omitempty"`
	BeadStatus   string    `json:"bead_status,omitempty"`
	PR           string    `json:"pr,omitempty"`
	PRState      string    `json:"pr_state,omitempty"`
	Checks       string    `json:"checks,omitempty"`
	Head         string    `json:"head,omitempty"`
	Class        int       `json:"class"`
	Level        int       `json:"level"`
	TargetKind   string    `json:"target_kind,omitempty"`
	TargetHash   string    `json:"target_hash,omitempty"`
	EffectiveBy  string    `json:"effective_by,omitempty"`
	Result       string    `json:"result,omitempty"`
	AuditNote    string    `json:"audit_note,omitempty"`
	Summary      string    `json:"summary,omitempty"`
	Why          string    `json:"why,omitempty"`
	By           string    `json:"by,omitempty"`
	AnswerChoice int       `json:"answer_choice,omitempty"`
}

// QMStatus holds the fields rendered in a fenced qm-status block.
type QMStatus struct {
	Req        string `json:"req"`
	State      string `json:"state"`
	Bead       string `json:"bead"`
	BeadStatus string `json:"bead_status,omitempty"`
	PR         string `json:"pr,omitempty"`
	PRState    string `json:"pr_state,omitempty"`
	Checks     string `json:"checks,omitempty"`
	Head       string `json:"head,omitempty"`
	Class      *int   `json:"class,omitempty"`
	Job        string `json:"job"`
}

// JobView is the reconstructed state of a job derived solely from job.json and events.jsonl.
type JobView struct {
	Job         string     `json:"job"`
	Key         string     `json:"key"`
	State       string     `json:"state"`
	Canceled    bool       `json:"canceled,omitempty"`
	Terminal    bool       `json:"terminal"`
	Deduped     bool       `json:"deduped,omitempty"`
	Verb        string     `json:"verb"`
	Target      string     `json:"target,omitempty"`
	Choice      string     `json:"choice,omitempty"`
	FreeText    string     `json:"free_text,omitempty"`
	Why         string     `json:"why,omitempty"`
	RequestWhy  string     `json:"request_why,omitempty"`
	By          string     `json:"by"`
	EffectiveBy string     `json:"effective_by,omitempty"`
	Bead        string     `json:"bead,omitempty"`
	BeadStatus  string     `json:"bead_status,omitempty"`
	PR          string     `json:"pr,omitempty"`
	PRState     string     `json:"pr_state,omitempty"`
	Checks      string     `json:"checks,omitempty"`
	Head        string     `json:"head,omitempty"`
	Class       int        `json:"class"`
	Level       int        `json:"level"`
	Attempts    int        `json:"attempts,omitempty"`
	TargetKind  string     `json:"target_kind,omitempty"`
	TargetHash  string     `json:"target_hash,omitempty"`
	ParkHash    string     `json:"park_hash,omitempty"`
	ParkReason  string     `json:"park_reason,omitempty"`
	ParkedAt    *time.Time `json:"parked_at,omitempty"`
	AuditNote   string     `json:"audit_note,omitempty"`
	Summary     string     `json:"summary,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	Age         string     `json:"age,omitempty"`
	ParkedAge   string     `json:"parked_age,omitempty"`
	QMStatus    *QMStatus  `json:"qm_status,omitempty"`

	intentPending  bool
	lastOutcome    string
	parkedAnswered bool
	answerChoice   int
	hasEvent       bool
}

type jobKeyRecord struct {
	Key           string    `json:"key"`
	JobID         string    `json:"job_id"`
	RequestDigest string    `json:"request_digest"`
	CreatedAt     time.Time `json:"created_at"`
}

type jobClaimRecord struct {
	Kind  string    `json:"kind"`
	Bead  string    `json:"bead"`
	Hash  string    `json:"hash"`
	JobID string    `json:"job_id"`
	At    time.Time `json:"at"`
}

type jobHoldRecord struct {
	Bead  string    `json:"bead"`
	JobID string    `json:"job_id"`
	By    string    `json:"by"`
	Why   string    `json:"why,omitempty"`
	At    time.Time `json:"at"`
}

func jobsDir() string         { return filepath.Join(stateDir(), "jobs") }
func jobsLockPath() string    { return filepath.Join(jobsDir(), "jobs.lock") }
func jobKeysDir() string      { return filepath.Join(jobsDir(), "keys") }
func jobClaimsDir() string    { return filepath.Join(jobsDir(), "claims") }
func jobHoldsDir() string     { return filepath.Join(jobsDir(), "holds") }
func jobDir(id string) string { return filepath.Join(jobsDir(), id) }

func jobKeyPath(key string) (string, error) {
	if !jobKeyRE.MatchString(key) || strings.Contains(key, "..") {
		return "", fmt.Errorf("%w: invalid idempotency key %q (want [A-Za-z0-9][A-Za-z0-9._-]{0,63})", errJob, key)
	}
	return filepath.Join(jobKeysDir(), key+".json"), nil
}

func jobClaimPath(kind, bead, hash string) string {
	d := sha256Hex([]byte(kind + "|" + bead + "|" + hash))
	return filepath.Join(jobClaimsDir(), d+".json")
}

func jobHoldPath(bead string) (string, error) {
	if !beadName.MatchString(bead) {
		return "", fmt.Errorf("%w: invalid bead id %q", errJob, bead)
	}
	return filepath.Join(jobHoldsDir(), bead+".json"), nil
}

// jobStoreMu serializes in-process callers alongside the cross-process flock.
var jobStoreMu sync.Mutex

// withJobStoreLock runs fn while holding an exclusive flock on jobs.lock.
// Never run slow subprocesses or network calls inside fn.
func withJobStoreLock(fn func() error) error {
	jobStoreMu.Lock()
	defer jobStoreMu.Unlock()

	if err := ensurePrivateDir(jobsDir()); err != nil {
		return err
	}
	lf, err := os.OpenFile(jobsLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := os.Chmod(jobsLockPath(), 0o600); err != nil {
		return err
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock jobs store: %w", err)
	}
	defer func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }()

	return fn()
}

// lockJob takes a per-job non-blocking flock so two concurrent ticker passes
// or cancel operations never race on the same job.
func lockJob(id string) (func(), error) {
	if !jobIDRE.MatchString(id) {
		return nil, fmt.Errorf("%w: invalid job id %q", errJob, id)
	}
	dir := filepath.Join(runDir(), "locks")
	if err := ensurePrivateDir(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "job-"+id+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: job %s is currently executing", errBusy, short(id))
		}
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

func newJobKey(now time.Time) (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("job-%s-%s", now.UTC().Format("20060102-150405"), hex.EncodeToString(b[:])), nil
}

func storeJobSpec(spec JobSpec) error {
	p := filepath.Join(jobDir(spec.ID), "job.json")
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicPrivate(p, append(b, '\n'))
}

func appendJobEvent(spec JobSpec, e jobEvent) error {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	if e.Kind == "" {
		return fmt.Errorf("%w: empty job event kind", errJob)
	}
	p := filepath.Join(jobDir(spec.ID), "events.jsonl")
	if err := appendPrivateJSONL(p, e); err != nil {
		return err
	}
	text := fmt.Sprintf("job %s (%s %s) %s", short(spec.ID), spec.Request.Verb, orDash(e.Bead), e.Kind)
	for _, s := range []string{e.AuditNote, e.Summary, e.Why} {
		if s != "" {
			text += ": " + shortTitle(oneLine(s), 140)
			break
		}
	}
	bead := e.Bead
	if bead == "" && beadName.MatchString(spec.Request.Target) {
		bead = spec.Request.Target
	}
	rec := journalRecord{
		At:     e.At,
		Kind:   journalJob,
		Bead:   bead,
		Class:  e.Class,
		Action: strings.ToLower(spec.Request.Verb),
		Pass:   e.Kind == jobEventDone || (e.Kind == jobEventOutcome && e.Result == resultOK) || e.Kind == jobEventQueued,
		Text:   text,
	}
	return appendJournal(rec)
}

func readJobEvents(id string) ([]jobEvent, error) {
	if !jobIDRE.MatchString(id) {
		return nil, fmt.Errorf("%w: invalid job id %q", errJob, id)
	}
	f, err := os.Open(filepath.Join(jobDir(id), "events.jsonl"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []jobEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e jobEvent
		if json.Unmarshal(line, &e) == nil && e.Kind != "" {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

func readJobSpecByID(id string) (JobSpec, error) {
	if !jobIDRE.MatchString(id) {
		return JobSpec{}, fmt.Errorf("%w: invalid job id %q", errJob, id)
	}
	b, err := os.ReadFile(filepath.Join(jobDir(id), "job.json"))
	if err != nil {
		return JobSpec{}, err
	}
	var spec JobSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return JobSpec{}, fmt.Errorf("%w: %s/job.json: %v", errJob, short(id), err)
	}
	if spec.Version != jobSchemaVersion {
		return JobSpec{}, fmt.Errorf("%w: %s/job.json: unsupported version %d", errJob, short(id), spec.Version)
	}
	if spec.ID != id {
		return JobSpec{}, fmt.Errorf("%w: %s/job.json id mismatch (%s)", errJob, short(id), spec.ID)
	}
	want, err := spec.digest()
	if err != nil || want != id {
		return JobSpec{}, fmt.Errorf("%w: %s/job.json doesn't hash to its id", errJob, short(id))
	}
	return spec, nil
}

// loadJob finds a stored job by full 64-char hex ID or a unique hex prefix of
// at least 6 characters.
func loadJob(ref string) (JobSpec, []jobEvent, error) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if !jobPrefixRE.MatchString(ref) {
		return JobSpec{}, nil, fmt.Errorf("%w: give at least 6 hex characters of the job id, not %q", errJob, ref)
	}
	entries, err := os.ReadDir(jobsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return JobSpec{}, nil, fmt.Errorf("%w: no job %s", errJob, ref)
	}
	if err != nil {
		return JobSpec{}, nil, err
	}
	var found []string
	for _, e := range entries {
		if !e.IsDir() || !jobIDRE.MatchString(e.Name()) {
			continue
		}
		if e.Name() == ref {
			found = []string{e.Name()}
			break
		}
		if strings.HasPrefix(e.Name(), ref) {
			found = append(found, e.Name())
		}
	}
	switch len(found) {
	case 0:
		return JobSpec{}, nil, fmt.Errorf("%w: no job %s", errJob, ref)
	case 1:
	default:
		return JobSpec{}, nil, fmt.Errorf("%w: %s matches %d jobs; give more of the id", errJob, ref, len(found))
	}
	spec, err := readJobSpecByID(found[0])
	if err != nil {
		return JobSpec{}, nil, err
	}
	events, err := readJobEvents(spec.ID)
	if err != nil {
		return JobSpec{}, nil, err
	}
	return spec, events, nil
}

// listJobSpecs loads all valid jobs, sorted newest first.
func listJobSpecs() ([]JobSpec, error) {
	entries, err := os.ReadDir(jobsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var specs []JobSpec
	for _, e := range entries {
		if !e.IsDir() || !jobIDRE.MatchString(e.Name()) {
			continue
		}
		spec, err := readJobSpecByID(e.Name())
		if err != nil {
			continue
		}
		specs = append(specs, spec)
	}
	sort.Slice(specs, func(i, j int) bool {
		if !specs[i].CreatedAt.Equal(specs[j].CreatedAt) {
			return specs[i].CreatedAt.After(specs[j].CreatedAt)
		}
		return specs[i].ID > specs[j].ID
	})
	return specs, nil
}

// claimJobAnswer atomically binds (kind, bead, hash) to jobID under jobs.lock.
// If another job already claimed this target, claimed is false and owner names it.
func claimJobAnswer(kind, bead, hash, jobID string, now time.Time) (owner string, claimed bool, err error) {
	if kind == "" || bead == "" || hash == "" || jobID == "" {
		return "", false, fmt.Errorf("%w: incomplete claim key", errJob)
	}
	err = withJobStoreLock(func() error {
		p := jobClaimPath(kind, bead, hash)
		if b, rerr := os.ReadFile(p); rerr == nil {
			var rec jobClaimRecord
			if jerr := json.Unmarshal(b, &rec); jerr == nil && rec.JobID != "" {
				owner = rec.JobID
				claimed = rec.JobID == jobID
				return nil
			}
		} else if !errors.Is(rerr, fs.ErrNotExist) {
			return rerr
		}
		rec := jobClaimRecord{
			Kind:  kind,
			Bead:  bead,
			Hash:  hash,
			JobID: jobID,
			At:    now.UTC(),
		}
		b, merr := json.MarshalIndent(rec, "", "  ")
		if merr != nil {
			return merr
		}
		if werr := writeAtomicPrivate(p, append(b, '\n')); werr != nil {
			return werr
		}
		owner = jobID
		claimed = true
		return nil
	})
	return owner, claimed, err
}

func isJobHeld(bead string) (jobHoldRecord, bool) {
	p, err := jobHoldPath(bead)
	if err != nil {
		return jobHoldRecord{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return jobHoldRecord{}, false
	}
	var rec jobHoldRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return jobHoldRecord{}, false
	}
	return rec, true
}

func setJobHold(rec jobHoldRecord) error {
	p, err := jobHoldPath(rec.Bead)
	if err != nil {
		return err
	}
	return withJobStoreLock(func() error {
		b, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			return err
		}
		return writeAtomicPrivate(p, append(b, '\n'))
	})
}

func clearJobHold(bead string) (bool, error) {
	p, err := jobHoldPath(bead)
	if err != nil {
		return false, err
	}
	had := false
	err = withJobStoreLock(func() error {
		if _, serr := os.Stat(p); serr == nil {
			had = true
		} else if errors.Is(serr, fs.ErrNotExist) {
			return nil
		} else {
			return serr
		}
		if rerr := os.Remove(p); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return rerr
		}
		return nil
	})
	return had, err
}

// reconstructJob folds job.json and events.jsonl into a JobView.
func reconstructJob(spec JobSpec, events []jobEvent, now time.Time) JobView {
	v := JobView{
		Job:         spec.ID,
		Key:         spec.Key,
		State:       JobStateQueued,
		Verb:        spec.Request.Verb,
		Target:      spec.Request.Target,
		Choice:      spec.Request.Choice,
		FreeText:    spec.Request.FreeText,
		RequestWhy:  spec.Request.Why,
		By:          spec.Request.By,
		EffectiveBy: spec.Request.By,
		Class:       -1,
		Level:       -1,
		CreatedAt:   spec.CreatedAt,
		UpdatedAt:   spec.CreatedAt,
	}
	if beadName.MatchString(spec.Request.Target) {
		v.Bead = spec.Request.Target
	}
	for _, e := range events {
		v.hasEvent = true
		if !e.At.IsZero() {
			v.UpdatedAt = e.At
		}
		if e.Bead != "" {
			v.Bead = e.Bead
		}
		if e.BeadStatus != "" {
			v.BeadStatus = e.BeadStatus
		}
		if e.PR != "" {
			v.PR = e.PR
		}
		if e.PRState != "" {
			v.PRState = e.PRState
		}
		if e.Checks != "" {
			v.Checks = e.Checks
		}
		if e.Head != "" {
			v.Head = e.Head
		}
		if e.Class >= 0 {
			v.Class = e.Class
		}
		if e.Level >= 0 {
			v.Level = e.Level
		}
		if e.TargetKind != "" {
			v.TargetKind = e.TargetKind
		}
		if e.TargetHash != "" {
			v.TargetHash = e.TargetHash
		}
		if e.EffectiveBy != "" {
			v.EffectiveBy = e.EffectiveBy
		}
		if e.AuditNote != "" {
			v.AuditNote = e.AuditNote
		}
		if e.Summary != "" {
			v.Summary = e.Summary
		}
		switch e.Kind {
		case jobEventQueued:
			v.State = JobStateQueued
		case jobEventClaimed:
			v.State = JobStateRunning
		case jobEventIntent:
			v.State = JobStateRunning
			v.intentPending = true
			v.Attempts = max(v.Attempts, e.Attempt)
			v.lastOutcome = ""
		case jobEventOutcome:
			v.lastOutcome = e.Result
			if e.Result == resultUnknown {
				v.intentPending = true
			} else {
				v.intentPending = false
			}
			if e.Why != "" {
				v.Why = e.Why
			}
		case jobEventParked:
			v.State = JobStateParked
			v.intentPending = false
			at := e.At
			v.ParkedAt = &at
			v.ParkHash = e.TargetHash
			v.ParkReason = e.Why
			v.Why = e.Why
		case jobEventAnswerApplied:
			v.State = JobStateRunning
			v.parkedAnswered = true
			v.answerChoice = e.AnswerChoice
			if e.EffectiveBy != "" {
				v.EffectiveBy = e.EffectiveBy
			}
			v.Why = ""
		case jobEventDone:
			v.State = JobStateDone
			v.Terminal = true
			v.intentPending = false
			at := e.At
			v.FinishedAt = &at
			v.Why = ""
		case jobEventBlocked:
			v.State = JobStateBlocked
			v.Terminal = true
			v.intentPending = false
			at := e.At
			v.FinishedAt = &at
			v.Why = e.Why
		case jobEventNeedsTJ:
			v.State = JobStateNeedsTJ
			v.Terminal = true
			v.intentPending = false
			at := e.At
			v.FinishedAt = &at
			v.Why = e.Why
		case jobEventCanceled:
			v.State = JobStateBlocked
			v.Terminal = true
			v.Canceled = true
			v.intentPending = false
			at := e.At
			v.FinishedAt = &at
			v.Why = e.Why
		}
	}
	if !now.IsZero() {
		v.Age = age(v.CreatedAt, now)
		if v.ParkedAt != nil && v.State == JobStateParked {
			v.ParkedAge = age(*v.ParkedAt, now)
		}
	}
	if v.Terminal {
		st := buildQMStatus(v)
		v.QMStatus = &st
	}
	return v
}

func qmStatusToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "-"
	}
	// qm-status grammar forbids whitespace inside values; collapse if any slipped in.
	s = strings.Join(strings.Fields(s), "_")
	if s == "" {
		return "-"
	}
	return s
}

func buildQMStatus(v JobView) QMStatus {
	st := QMStatus{
		Req:   qmStatusToken(v.Key),
		State: qmStatusToken(v.State),
		Bead:  qmStatusToken(v.Bead),
		Job:   qmStatusToken(v.Job),
	}
	if v.BeadStatus != "" {
		st.BeadStatus = qmStatusToken(v.BeadStatus)
	}
	if v.PR != "" {
		st.PR = qmStatusToken(v.PR)
		if v.PRState != "" {
			st.PRState = qmStatusToken(strings.ToLower(v.PRState))
		}
		if v.Head != "" {
			st.Head = qmStatusToken(v.Head)
		}
		if v.Checks != "" {
			st.Checks = qmStatusToken(v.Checks)
		}
	}
	if v.Class >= 0 {
		c := v.Class
		st.Class = &c
	}
	return st
}

// formatQMStatus renders the fenced qm-status block per docs/design/coordinator-api.md.
func formatQMStatus(s QMStatus) string {
	var b strings.Builder
	b.WriteString("```qm-status\n")
	fmt.Fprintf(&b, "req=%s\n", qmStatusToken(s.Req))
	fmt.Fprintf(&b, "state=%s\n", qmStatusToken(s.State))
	fmt.Fprintf(&b, "bead=%s\n", qmStatusToken(s.Bead))
	if s.BeadStatus != "" {
		fmt.Fprintf(&b, "bead_status=%s\n", qmStatusToken(s.BeadStatus))
	}
	if s.PR != "" {
		fmt.Fprintf(&b, "pr=%s\n", qmStatusToken(s.PR))
	}
	if s.PRState != "" {
		fmt.Fprintf(&b, "pr_state=%s\n", qmStatusToken(s.PRState))
	}
	if s.Checks != "" {
		fmt.Fprintf(&b, "checks=%s\n", qmStatusToken(s.Checks))
	}
	if s.Head != "" {
		fmt.Fprintf(&b, "head=%s\n", qmStatusToken(s.Head))
	}
	if s.Class != nil {
		fmt.Fprintf(&b, "class=%d\n", *s.Class)
	}
	fmt.Fprintf(&b, "job=%s\n", qmStatusToken(s.Job))
	b.WriteString("```\n")
	return b.String()
}

// parseJobRequest parses positional request tokens (after flags / --) plus flag
// overrides into a validated JobRequest and optional extracted [req:id] key.
func parseJobRequest(tokens []string, flagKey, flagWhy, flagFreeText, flagExpect, flagBy string, flagClass int) (JobRequest, string, error) {
	if flagClass < -1 || flagClass > 9 {
		return JobRequest{}, "", fmt.Errorf("%w: --class must be 0-9", errJob)
	}
	if len(tokens) == 0 {
		return JobRequest{}, "", fmt.Errorf("%w: request is required (verbs: %s)", errJob, strings.Join(validJobVerbs, ", "))
	}

	// Split out an em-dash separator (" — " in a single quoted string, or a
	// standalone "—" token) so trailing why or verbatim free text stays intact.
	var cmdTokens []string
	var dashText string
	hasDash := false
	if len(tokens) == 1 {
		raw := strings.TrimSpace(tokens[0])
		if left, right, ok := strings.Cut(raw, " — "); ok {
			cmdTokens = strings.Fields(left)
			dashText = right
			hasDash = true
		} else if left, ok := strings.CutSuffix(raw, " —"); ok {
			cmdTokens = strings.Fields(left)
			hasDash = true
		} else {
			cmdTokens = strings.Fields(raw)
		}
	} else {
		dashIdx := -1
		for i, tok := range tokens {
			if tok == "—" {
				dashIdx = i
				break
			}
		}
		if dashIdx >= 0 {
			cmdTokens = tokens[:dashIdx]
			dashText = strings.Join(tokens[dashIdx+1:], " ")
			hasDash = true
		} else {
			cmdTokens = tokens
		}
	}

	if len(cmdTokens) == 0 {
		return JobRequest{}, "", fmt.Errorf("%w: request is required (verbs: %s)", errJob, strings.Join(validJobVerbs, ", "))
	}

	// Optional leading [req:<id>] correlation tag.
	key := strings.TrimSpace(flagKey)
	if m := reqTagRE.FindStringSubmatch(cmdTokens[0]); m != nil {
		tagKey := m[1]
		if key != "" && key != tagKey {
			return JobRequest{}, "", fmt.Errorf("%w: --key %q conflicts with [req:%s]", errJob, key, tagKey)
		}
		key = tagKey
		cmdTokens = cmdTokens[1:]
		if len(cmdTokens) == 0 {
			return JobRequest{}, "", fmt.Errorf("%w: verb is required after [req:%s]", errJob, tagKey)
		}
	}

	verb := strings.ToUpper(strings.TrimSpace(cmdTokens[0]))
	if !slices.Contains(validJobVerbs, verb) {
		return JobRequest{}, "", fmt.Errorf("%w: unknown verb %q (want %s)", errJob, cmdTokens[0], strings.Join(validJobVerbs, ", "))
	}

	by := strings.TrimSpace(flagBy)
	if by == "" {
		by = "coordinator"
	}

	req := JobRequest{
		Verb:   verb,
		Expect: strings.TrimSpace(flagExpect),
		Class:  flagClass,
		By:     by,
	}

	rest := cmdTokens[1:]
	why := strings.TrimSpace(flagWhy)
	freeText := flagFreeText

	switch verb {
	case JobVerbStatus:
		if len(rest) > 1 {
			return JobRequest{}, "", fmt.Errorf("%w: STATUS takes at most one target (<bead> or PR URL)", errJob)
		}
		if len(rest) == 1 {
			if err := validateJobTarget(rest[0], true); err != nil {
				return JobRequest{}, "", err
			}
			req.Target = rest[0]
		}
		if hasDash && why == "" {
			why = strings.TrimSpace(dashText)
		}
		req.Why = why

	case JobVerbMerge:
		if len(rest) != 1 {
			return JobRequest{}, "", fmt.Errorf("%w: MERGE requires an explicit target (<bead> or PR URL)", errJob)
		}
		if err := validateJobTarget(rest[0], true); err != nil {
			return JobRequest{}, "", err
		}
		req.Target = rest[0]
		if hasDash && why == "" {
			why = strings.TrimSpace(dashText)
		}
		req.Why = why

	case JobVerbDispatch, JobVerbHold, JobVerbResume, JobVerbStop:
		if len(rest) != 1 {
			return JobRequest{}, "", fmt.Errorf("%w: %s requires an explicit <bead> target", errJob, verb)
		}
		if err := validateJobTarget(rest[0], false); err != nil {
			return JobRequest{}, "", err
		}
		req.Target = rest[0]
		if hasDash && why == "" {
			why = strings.TrimSpace(dashText)
		}
		req.Why = why

	case JobVerbClose:
		if len(rest) != 1 {
			return JobRequest{}, "", fmt.Errorf("%w: CLOSE requires an explicit <bead> target and a reason (CLOSE <bead> — <reason> or --why)", errJob)
		}
		if err := validateJobTarget(rest[0], false); err != nil {
			return JobRequest{}, "", err
		}
		req.Target = rest[0]
		if hasDash && why == "" {
			why = strings.TrimSpace(dashText)
		}
		if why == "" {
			return JobRequest{}, "", fmt.Errorf("%w: CLOSE requires a reason (CLOSE <bead> — <reason> or --why)", errJob)
		}
		req.Why = why

	case JobVerbAnswer:
		if len(rest) < 1 {
			return JobRequest{}, "", fmt.Errorf("%w: ANSWER requires <bead> and an option or free text (ANSWER <bead> <choice> | ANSWER <bead> — <text>)", errJob)
		}
		if err := validateJobTarget(rest[0], false); err != nil {
			return JobRequest{}, "", err
		}
		req.Target = rest[0]
		if len(rest) >= 2 {
			req.Choice = strings.TrimSpace(strings.Join(rest[1:], " "))
			if hasDash && why == "" {
				why = strings.TrimSpace(dashText)
			}
		} else if hasDash && freeText == "" {
			// "ANSWER <bead> — <verbatim text>" carries free text verbatim (trim only trailing newline).
			freeText = strings.TrimSuffix(dashText, "\n")
		}
		if freeText != "" {
			freeText = strings.TrimSuffix(freeText, "\n")
			if utf8.RuneCountInString(freeText) > jobMaxFreeTextRune {
				return JobRequest{}, "", fmt.Errorf("%w: free text exceeds maximum %d characters", errJob, jobMaxFreeTextRune)
			}
		}
		if req.Choice == "" && strings.TrimSpace(freeText) == "" {
			return JobRequest{}, "", fmt.Errorf("%w: ANSWER requires an option choice or non-empty free text", errJob)
		}
		req.FreeText = freeText
		req.Why = why
	}

	return req, key, nil
}

func validateJobTarget(target string, allowPRURL bool) error {
	if beadName.MatchString(target) {
		return nil
	}
	if allowPRURL && prURLRE.MatchString(target) {
		return nil
	}
	if allowPRURL {
		return fmt.Errorf("%w: invalid target %q (want bead id or GitHub PR URL)", errJob, target)
	}
	return fmt.Errorf("%w: invalid bead id %q", errJob, target)
}

// submitJob durably persists a job under its content-addressed ID and binds its
// idempotency key atomically. Re-submitting the same key with the same request
// returns the original job with Deduped=true without creating or duplicating work.
func submitJob(req JobRequest, key string, now time.Time) (JobView, error) {
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	if strings.TrimSpace(key) == "" {
		var err error
		key, err = newJobKey(now)
		if err != nil {
			return JobView{}, err
		}
	}
	keyPath, err := jobKeyPath(key)
	if err != nil {
		return JobView{}, err
	}
	reqDigest, err := req.digest()
	if err != nil {
		return JobView{}, err
	}
	spec := JobSpec{
		Version:   jobSchemaVersion,
		Key:       key,
		Request:   req,
		CreatedAt: now,
	}
	id, err := spec.digest()
	if err != nil {
		return JobView{}, err
	}
	spec.ID = id

	var view JobView
	err = withJobStoreLock(func() error {
		// 1. Check existing idempotency key binding.
		if b, rerr := os.ReadFile(keyPath); rerr == nil {
			var krec jobKeyRecord
			if jerr := json.Unmarshal(b, &krec); jerr != nil {
				return fmt.Errorf("%w: corrupt key index %s: %v", errJob, key, jerr)
			}
			if krec.RequestDigest != reqDigest {
				return fmt.Errorf("%w: key %q is already bound to job %s with a different request", errJobKeyConflict, key, short(krec.JobID))
			}
			existingSpec, lerr := readJobSpecByID(krec.JobID)
			if lerr != nil {
				return lerr
			}
			events, eerr := readJobEvents(existingSpec.ID)
			if eerr != nil {
				return eerr
			}
			view = reconstructJob(existingSpec, events, now)
			view.Deduped = true
			return nil
		} else if !errors.Is(rerr, fs.ErrNotExist) {
			return rerr
		}

		// 2. Crash-window repair: if job.json was written before a crash prevented
		// writing keys/<key>.json, reuse it and repair the key index.
		deduped := false
		if existingSpec, lerr := readJobSpecByID(spec.ID); lerr == nil {
			spec = existingSpec
			deduped = true
		} else {
			if serr := storeJobSpec(spec); serr != nil {
				return serr
			}
		}

		events, eerr := readJobEvents(spec.ID)
		if eerr != nil {
			return eerr
		}
		if len(events) == 0 {
			bead := ""
			if beadName.MatchString(spec.Request.Target) {
				bead = spec.Request.Target
			}
			ev := jobEvent{
				At:          now,
				Kind:        jobEventQueued,
				Bead:        bead,
				Class:       -1,
				Level:       -1,
				EffectiveBy: spec.Request.By,
			}
			if aerr := appendJobEvent(spec, ev); aerr != nil {
				return aerr
			}
			events = append(events, ev)
		}

		krec := jobKeyRecord{
			Key:           spec.Key,
			JobID:         spec.ID,
			RequestDigest: reqDigest,
			CreatedAt:     spec.CreatedAt,
		}
		kb, merr := json.MarshalIndent(krec, "", "  ")
		if merr != nil {
			return merr
		}
		if werr := writeAtomicPrivate(keyPath, append(kb, '\n')); werr != nil {
			return werr
		}
		view = reconstructJob(spec, events, now)
		view.Deduped = deduped
		return nil
	})
	return view, err
}

// cancelJob cancels a job only before its effect runs (queued or parked).
func cancelJob(env JobEnv, ref, reason, by string) (JobView, error) {
	spec, _, err := loadJob(ref)
	if err != nil {
		return JobView{}, err
	}
	unlock, err := lockJob(spec.ID)
	if err != nil {
		if errors.Is(err, errBusy) {
			return JobView{}, fmt.Errorf("%w: job %s is currently running", errJobNotCancel, short(spec.ID))
		}
		return JobView{}, err
	}
	defer unlock()

	now := env.clock().UTC()
	if strings.TrimSpace(reason) == "" {
		reason = "canceled"
	}
	if strings.TrimSpace(by) == "" {
		by = "TJ"
	}

	var view JobView
	var wasParked bool
	var parkBead, parkHash string
	err = withJobStoreLock(func() error {
		events, rerr := readJobEvents(spec.ID)
		if rerr != nil {
			return rerr
		}
		cur := reconstructJob(spec, events, now)
		if cur.State != JobStateQueued && cur.State != JobStateParked {
			return fmt.Errorf("%w: job %s is already %s", errJobNotCancel, short(spec.ID), cur.State)
		}
		wasParked = cur.State == JobStateParked
		parkBead = cur.Bead
		parkHash = cur.ParkHash
		ev := jobEvent{
			At:         now,
			Kind:       jobEventCanceled,
			Bead:       cur.Bead,
			BeadStatus: cur.BeadStatus,
			Class:      -1,
			Level:      -1,
			By:         by,
			Why:        fmt.Sprintf("canceled by %s: %s", by, oneLine(reason)),
		}
		if aerr := appendJobEvent(spec, ev); aerr != nil {
			return aerr
		}
		events = append(events, ev)
		view = reconstructJob(spec, events, now)
		return nil
	})
	if err != nil {
		return JobView{}, err
	}
	// If the canceled job had parked a needs_me approval question on its bead,
	// note the cancellation so any outstanding tap links immediately go stale.
	if wasParked && parkBead != "" && parkHash != "" && env.Note != nil {
		_ = env.Note(parkBead, fmt.Sprintf("JOB CANCELED [%s]: %s (cleared question [%s])", short(spec.ID), oneLine(reason), parkHash))
		if env.Show != nil && env.SetStatus != nil {
			if b, serr := env.Show(parkBead); serr == nil && b.Status == StatusNeedsMe {
				_ = env.SetStatus(parkBead, StatusInProgress)
			}
		}
	}
	return view, nil
}

// JobEnv injects external dependencies for job execution and ticker processing
// so tests never shell out to real bd/gh/herdr unless explicitly configured.
type JobEnv struct {
	Cfg         Config
	Show        func(id string) (Bead, error)
	Active      func() ([]Bead, error)
	Note        func(id, text string) error
	SetStatus   func(id, status string) error
	Prompt      promptEnv
	ViewPR      func(ctx context.Context, url string) (PR, error)
	PRForBead   func(id string) (PR, bool)
	BeadForPR   func(url string) (string, bool)
	MergePR     func(ctx context.Context, b Bead, pr PR, by string) (string, error)
	Dispatch    func(b Bead, by string) (string, error)
	Resume      func(b Bead, by string) (string, error)
	Close       func(b Bead, reason, by string) (string, error)
	StopWorker  func(b Bead) (string, error)
	FleetStatus func() (string, error)
	Now         func() time.Time
	Logf        func(format string, args ...any)

	// Crash-window test hooks (nil in production).
	OnAfterIntent func(jobID string) error
	OnAfterEffect func(jobID string) error
}

func (e JobEnv) clock() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e JobEnv) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

func liveJobEnv(cfg Config, h Herdr, st *TickerState, threads []Thread, now time.Time, logf func(string, ...any)) JobEnv {
	bd := Beads{}
	gh := GH{repo: cfg.Repo}
	prByBead := map[string]PR{}
	beadByPR := map[string]string{}
	byBead := map[string]Bead{}
	for _, th := range threads {
		byBead[th.Bead.ID] = th.Bead
		if th.PR != nil && th.PR.URL != "" {
			prByBead[th.Bead.ID] = *th.PR
			beadByPR[th.PR.URL] = th.Bead.ID
		}
		for _, m := range th.Merged {
			if m.URL != "" {
				if _, ok := beadByPR[m.URL]; !ok {
					beadByPR[m.URL] = th.Bead.ID
				}
			}
		}
	}
	if st != nil {
		for id, pr := range st.PRs {
			if _, ok := prByBead[id]; !ok && pr.URL != "" {
				prByBead[id] = pr
			}
			if pr.URL != "" {
				if _, ok := beadByPR[pr.URL]; !ok {
					beadByPR[pr.URL] = id
				}
			}
		}
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	clock := time.Now
	if !now.IsZero() {
		clock = func() time.Time { return now }
	}
	pEnv := livePromptEnv(cfg, h)
	return JobEnv{
		Cfg:       cfg,
		Show:      bd.Show,
		Active:    bd.Active,
		Note:      bd.Note,
		SetStatus: bd.SetStatus,
		Prompt:    pEnv,
		ViewPR: func(_ context.Context, url string) (PR, error) {
			pr, err := gh.View(url, true)
			if err == nil {
				pr.DetailsFresh = true
			}
			return pr, err
		},
		PRForBead: func(id string) (PR, bool) {
			if pr, ok := prByBead[id]; ok {
				return pr, true
			}
			b, err := bd.Show(id)
			if err != nil {
				return PR{}, false
			}
			noted := NotedPRs(b.Notes)
			if len(noted) == 0 {
				return PR{}, false
			}
			return PR{URL: noted[len(noted)-1]}, true
		},
		BeadForPR: func(url string) (string, bool) {
			if id, ok := beadByPR[url]; ok {
				return id, true
			}
			active, err := bd.Active()
			if err != nil {
				return "", false
			}
			for _, b := range active {
				if slices.Contains(NotedPRs(b.Notes), url) {
					return b.ID, true
				}
			}
			return "", false
		},
		MergePR: func(ctx context.Context, b Bead, pr PR, by string) (string, error) {
			return runJobMergePlan(ctx, cfg, b, pr, by, clock())
		},
		Dispatch: func(b Bead, by string) (string, error) {
			return dispatch(cfg, h, b.ID, DispatchOpts{By: by})
		},
		Resume: func(b Bead, by string) (string, error) {
			lines, err := resume(cfg, h, []string{b.ID}, "", by)
			return strings.Join(lines, "; "), err
		},
		Close: func(b Bead, reason, by string) (string, error) {
			var buf bytes.Buffer
			err := closeBead(cfg, liveCloseEnv(cfg), b.ID, reason, by, &buf)
			return strings.TrimSpace(buf.String()), err
		},
		StopWorker: func(b Bead) (string, error) {
			return stopWorkerForJob(cfg, h, b)
		},
		FleetStatus: func() (string, error) {
			s := loadState()
			if st != nil {
				s = *st
			}
			var inbox []InboxItem
			inbox, _ = readInbox()
			return renderContextWith(cfg, threads, nil, nil, inbox, s, nil, clock(), ContextOptions{Compact: true})
		},
		Now:  clock,
		Logf: logf,
	}
}

// runJobMergePlan executes a checked github.pr.merge plan through the executor
// (or in-process executor when the daemon is not running), always binding head_sha.
func runJobMergePlan(ctx context.Context, cfg Config, b Bead, pr PR, by string, now time.Time) (string, error) {
	parsed, err := parsePRURL(pr.URL, pr.HeadSHA)
	if err != nil {
		return "", err
	}
	if !fullSHA.MatchString(pr.HeadSHA) {
		return "", fmt.Errorf("PR %s has no full 40-char head SHA", pr.URL)
	}
	p := Plan{
		Bead:   b.ID,
		Actor:  "job:" + by,
		Source: "job",
		Steps: []Step{{
			Op:     "github.pr.merge",
			Target: parsed.target(),
			Args:   map[string]string{"method": "squash"},
			Pre:    map[string]string{"head_sha": pr.HeadSHA},
		}},
	}
	digest, err := p.digest()
	if err != nil {
		return "", err
	}
	if err := storePlan(p, digest); err != nil {
		return "", err
	}
	events, err := readPlanEvents(digest)
	if err != nil {
		return "", err
	}
	if _, ok := liveApproval(events, now); !ok {
		if by == "TJ" {
			class, _ := classifyPlan(cfg, func(string) (Bead, error) { return b, nil }, p)
			if err := appendPlanEvent(digest, planEvent{At: now, Kind: planChecked, Class: class, Level: -1, Pass: true}); err != nil {
				return "", err
			}
			if err := appendPlanEvent(digest, planEvent{At: now, Kind: planApproved, Class: class, Level: -1, Pass: true, By: "TJ", Expires: now.Add(approvalTTL)}); err != nil {
				return "", err
			}
		} else {
			if err := checkPlanFor(cfg, func(string) (Bead, error) { return b, nil }, p, now, io.Discard); err != nil {
				return "", err
			}
		}
	}
	if last, ok := lastRun(events); ok && last.Kind == planFailed {
		if err := appendPlanEvent(digest, planEvent{At: now, Kind: planRerun}); err != nil {
			return "", err
		}
	}
	var kind string
	if _, perr := execCall(controlSocket(), execRequest{Ping: true}, time.Second); perr == nil {
		resp, cerr := execCall(controlSocket(), execRequest{Resume: digest}, 0)
		if cerr != nil {
			return "", cerr
		}
		kind = resp.State
	} else {
		x := newExecutor(cfg, log.New(io.Discard, "", 0))
		kind, err = x.execPlan(ctx, digest)
		if err != nil {
			return "", err
		}
	}
	after, _ := readPlanEvents(digest)
	last, _ := lastRun(after)
	switch kind {
	case planDone:
		for _, pe := range slices.Backward(currentRun(after)) {
			if pe.Kind == planOutcome && pe.Result == resultOK && pe.Evidence != "" {
				return pe.Evidence, nil
			}
		}
		return fmt.Sprintf("merged %s at %s", pr.URL, short(pr.HeadSHA)), nil
	case planNeedsMe, planExpired:
		return "", fmt.Errorf("%w: %s", errNotAutonomous, last.Why)
	default:
		why := last.Why
		if why == "" {
			why = "merge plan " + short(digest) + " ended " + kind
		}
		return "", errors.New(why)
	}
}

func stopWorkerForJob(cfg Config, h Herdr, b Bead) (string, error) {
	if rpcExists(b.ID) {
		if _, err := rpcGracefulStop(b.ID, 5*time.Second); err == nil {
			return "stopped native pi-rpc worker gracefully", nil
		}
		if _, err := rpcClient(b.ID, rpcRequest{Op: "stop"}); err != nil {
			return "", err
		}
		return "stopped native pi-rpc worker", nil
	}
	host := beadHost(b)
	if host == "" {
		host = localHost
	}
	a, err := agentOn(cfg, h)(host, agentName(b.ID))
	if err != nil {
		return "", err
	}
	if a == nil {
		return "no worker running; bead stays claimed", nil
	}
	if err := cfg.herdrOn(h, host).Prompt(a.PaneID, "/exit"); err != nil {
		return "", err
	}
	return fmt.Sprintf("asked worker %s (%s) to exit; bead stays claimed", a.Name, a.PaneID), nil
}

// processJobs runs inside the ticker loop: first crash-reconciling any unfinished
// running jobs, then executing queued jobs FIFO, then checking parked jobs FIFO
// (so an ANSWER submitted in the same pass wakes its parked job immediately).
func (t Ticker) processJobs(st *TickerState, threads []Thread, _ map[string]Agent, now time.Time) {
	var env JobEnv
	if t.jobEnv != nil {
		env = *t.jobEnv
		if env.Now == nil {
			env.Now = func() time.Time { return now }
		}
		if env.Logf == nil {
			env.Logf = t.log.Printf
		}
	} else {
		env = liveJobEnv(t.cfg, t.herdr, st, threads, now, t.log.Printf)
	}
	ctx, cancel := context.WithTimeout(context.Background(), jobTickerTimeout)
	defer cancel()
	processPendingJobs(ctx, env)
}

// processPendingJobs advances all non-terminal jobs in deterministic order:
// 1. running (crash-recovery after ticker restart)
// 2. queued (FIFO)
// 3. parked (FIFO — never expires silently)
func processPendingJobs(ctx context.Context, env JobEnv) int {
	specs, err := listJobSpecs()
	if err != nil || len(specs) == 0 {
		return 0
	}
	// Execute FIFO (oldest first). listJobSpecs returns newest first.
	slices.Reverse(specs)
	now := env.clock().UTC()

	var running, queued []JobSpec
	for _, spec := range specs {
		events, err := readJobEvents(spec.ID)
		if err != nil {
			continue
		}
		v := reconstructJob(spec, events, now)
		switch v.State {
		case JobStateRunning:
			running = append(running, spec)
		case JobStateQueued:
			queued = append(queued, spec)
		}
	}

	progressed := 0
	runBatch := func(batch []JobSpec) {
		for _, spec := range batch {
			if ctx.Err() != nil {
				return
			}
			beforeEvents, _ := readJobEvents(spec.ID)
			before := reconstructJob(spec, beforeEvents, now)
			after, err := stepJob(ctx, env, spec.ID)
			if err != nil {
				if !errors.Is(err, errBusy) {
					env.logf("job %s (%s %s): %v", short(spec.ID), spec.Request.Verb, spec.Request.Target, err)
				}
				continue
			}
			if after.State != before.State || after.Terminal != before.Terminal {
				progressed++
				env.logf("job %s (%s %s): %s -> %s", short(spec.ID), spec.Request.Verb, spec.Request.Target, before.State, after.State)
			}
		}
	}

	runBatch(running)
	runBatch(queued)

	// Re-scan parked jobs after running/queued so an ANSWER submitted in the same
	// pass wakes its parked job immediately.
	var parked []JobSpec
	for _, spec := range specs {
		events, err := readJobEvents(spec.ID)
		if err != nil {
			continue
		}
		if reconstructJob(spec, events, now).State == JobStateParked {
			parked = append(parked, spec)
		}
	}
	runBatch(parked)
	return progressed
}

// stepJob runs one durable advancement step on job id under its per-job flock.
func stepJob(ctx context.Context, env JobEnv, id string) (JobView, error) {
	spec, _, err := loadJob(id)
	if err != nil {
		return JobView{}, err
	}
	unlock, err := lockJob(spec.ID)
	if err != nil {
		return JobView{}, err
	}
	defer unlock()

	now := env.clock().UTC()
	events, err := readJobEvents(spec.ID)
	if err != nil {
		return JobView{}, err
	}
	v := reconstructJob(spec, events, now)
	if v.Terminal {
		return v, nil
	}

	record := func(e jobEvent) error {
		if e.At.IsZero() {
			e.At = env.clock().UTC()
		}
		if err := appendJobEvent(spec, e); err != nil {
			return err
		}
		events = append(events, e)
		v = reconstructJob(spec, events, env.clock().UTC())
		return nil
	}

	// Ensure a crash right after job.json creation still has a queued marker.
	if !v.hasEvent {
		if err := record(jobEvent{Kind: jobEventQueued, Bead: v.Bead, Class: -1, Level: -1, EffectiveBy: spec.Request.By}); err != nil {
			return v, err
		}
	}

	// 1. Crash recovery: an outcome(ok) was persisted right before done.
	if v.State == JobStateRunning && !v.intentPending && v.lastOutcome == resultOK {
		if err := record(jobEvent{
			Kind:       jobEventDone,
			Bead:       v.Bead,
			BeadStatus: v.BeadStatus,
			PR:         v.PR,
			PRState:    v.PRState,
			Checks:     v.Checks,
			Head:       v.Head,
			Class:      v.Class,
			Level:      v.Level,
			AuditNote:  v.AuditNote,
			Summary:    v.Summary,
		}); err != nil {
			return v, err
		}
		return v, nil
	}

	// 2. Crash recovery: an intent was persisted with no outcome (or unknown outcome).
	if v.State == JobStateRunning && v.intentPending {
		return reconcilePendingJob(ctx, env, spec, v, record)
	}

	// 3. Parked job: check whether TJ answered its parked needs_me question.
	if v.State == JobStateParked {
		return advanceParkedJob(ctx, env, spec, v, record)
	}

	// 4. Crash recovery after parked answer_applied (before intent): handle decline directly.
	if v.State == JobStateRunning && v.parkedAnswered && v.answerChoice == 2 {
		if err := record(jobEvent{
			Kind:       jobEventBlocked,
			Bead:       v.Bead,
			BeadStatus: v.BeadStatus,
			Class:      v.Class,
			Level:      v.Level,
			AuditNote:  v.AuditNote,
			Why:        "declined by TJ on parked needs_me question",
		}); err != nil {
			return v, err
		}
		return v, nil
	}

	// 5. Execute queued (or newly resumed) job.
	return executeJob(ctx, env, spec, v, record)
}

// advanceParkedJob inspects a parked job waiting on TJ's needs_me answer.
// Nothing ever expires it silently; once TJ answers, the answer is claimed
// atomically (at most once) and applied.
func advanceParkedJob(ctx context.Context, env JobEnv, spec JobSpec, v JobView, record func(jobEvent) error) (JobView, error) {
	if v.Bead == "" || v.ParkHash == "" {
		return v, nil
	}
	var b Bead
	if env.Show != nil {
		var err error
		b, err = env.Show(v.Bead)
		if err != nil {
			return v, nil // transient read error: stay parked
		}
		if b.Status == StatusClosed {
			if err := record(jobEvent{
				Kind:       jobEventBlocked,
				Bead:       b.ID,
				BeadStatus: b.Status,
				Class:      v.Class,
				Level:      v.Level,
				Why:        fmt.Sprintf("%s was closed while job was parked", b.ID),
			}); err != nil {
				return v, err
			}
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
	}

	choice, answerText, found := findParkedNeedsMeAnswer(v.Bead, v.ParkHash, b.Notes)
	if !found {
		return v, nil
	}

	// Atomically claim the parked-answer application so it applies at most once.
	owner, claimed, err := claimJobAnswer("parked_answer", v.Bead, v.ParkHash, spec.ID, env.clock().UTC())
	if err != nil {
		return v, err
	}
	if !claimed {
		if err := record(jobEvent{
			Kind:       jobEventBlocked,
			Bead:       v.Bead,
			BeadStatus: b.Status,
			Class:      v.Class,
			Level:      v.Level,
			Why:        fmt.Sprintf("parked needs_me answer [%s] already applied by job %s", v.ParkHash, short(owner)),
		}); err != nil {
			return v, err
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}

	if err := record(jobEvent{
		Kind:         jobEventAnswerApplied,
		Bead:         v.Bead,
		BeadStatus:   b.Status,
		Class:        v.Class,
		Level:        v.Level,
		TargetKind:   "needs_me",
		TargetHash:   v.ParkHash,
		EffectiveBy:  "TJ",
		By:           "TJ",
		AnswerChoice: choice,
		AuditNote:    answerText,
	}); err != nil {
		return v, err
	}

	updated := reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC())
	switch choice {
	case 2:
		if err := record(jobEvent{
			Kind:       jobEventBlocked,
			Bead:       v.Bead,
			BeadStatus: b.Status,
			Class:      v.Class,
			Level:      v.Level,
			AuditNote:  answerText,
			Why:        "declined by TJ on parked needs_me question",
		}); err != nil {
			return updated, err
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	case 1:
		return executeJob(ctx, env, spec, updated, record)
	default:
		if err := record(jobEvent{
			Kind:       jobEventNeedsTJ,
			Bead:       v.Bead,
			BeadStatus: b.Status,
			Class:      v.Class,
			Level:      v.Level,
			AuditNote:  answerText,
			Why:        fmt.Sprintf("ambiguous answer to parked needs_me question [%s]: %s", v.ParkHash, shortTitle(answerText, 120)),
		}); err != nil {
			return updated, err
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
}

// findParkedNeedsMeAnswer checks journalNeedsMe and bead notes for TJ's answer
// to question hash. Only answers credited directly to TJ ("[<hash>] by TJ")
// satisfy a parked approval gate.
func findParkedNeedsMeAnswer(bead, hash, notes string) (choice int, text string, ok bool) {
	if hash == "" {
		return 0, "", false
	}
	wantTag := "[" + hash + "] by TJ"
	if recs, err := readJournal(time.Time{}); err == nil {
		for _, r := range slices.Backward(recs) {
			if r.Kind != journalNeedsMe || r.Bead != bead || !strings.Contains(r.Text, wantTag) {
				continue
			}
			if c, matched := parseNeedsMeChoice(r.Text); matched {
				return c, r.Text, true
			}
		}
	}
	// Also scan bead notes (newest line first) for direct or job-applied TJ answers.
	lines := strings.Split(notes, "\n")
	for _, raw := range slices.Backward(lines) {
		l := strings.TrimSpace(raw)
		if !strings.Contains(l, "NEEDS_ME ANSWERED:") || !strings.Contains(l, wantTag) {
			continue
		}
		if c, matched := parseNeedsMeChoice(l); matched {
			return c, l, true
		}
	}
	return 0, "", false
}

func parseNeedsMeChoice(line string) (int, bool) {
	_, rest, ok := strings.Cut(line, "NEEDS_ME ANSWERED: ")
	if !ok {
		return 0, false
	}
	switch {
	case strings.HasPrefix(rest, "1 "):
		return 1, true
	case strings.HasPrefix(rest, "2 "):
		return 2, true
	default:
		return 0, true
	}
}

func mustReadJobEvents(id string) []jobEvent {
	evs, _ := readJobEvents(id)
	return evs
}

// hasParkedJobForHash reports whether any currently parked job owns (bead, hash).
func hasParkedJobForHash(bead, hash string) bool {
	if bead == "" || hash == "" {
		return false
	}
	specs, err := listJobSpecs()
	if err != nil {
		return false
	}
	now := time.Now().UTC()
	for _, spec := range specs {
		events, err := readJobEvents(spec.ID)
		if err != nil {
			continue
		}
		v := reconstructJob(spec, events, now)
		if v.State == JobStateParked && v.Bead == bead && v.ParkHash == hash {
			return true
		}
	}
	return false
}

// jobAutonomyResult captures the result of checking the autonomy dial for a job.
type jobAutonomyResult struct {
	class   int
	level   int
	autoTag string
	failWhy string
}

func checkJobAutonomy(cfg Config, b Bead, action string, explicitClass int, by string, now time.Time) (jobAutonomyResult, error) {
	class := explicitClass
	if action != "" {
		ac, err := cfg.actionClass(b, action, "", "")
		if err != nil {
			return jobAutonomyResult{class: max(class, 9), level: -1, failWhy: err.Error()}, nil
		}
		class = max(class, ac)
	}
	if class < 0 {
		class = 9
	}
	if by == "TJ" {
		return jobAutonomyResult{class: class, level: -1}, nil
	}
	lvl, capped := cfg.beadLevel(b, autonomyStopped())
	if class > lvl {
		why := fmt.Sprintf("%s (class %d) is above %s's level %d", orDash(action), class, b.ID, lvl)
		if capped != "" {
			why += ", capped by " + capped
		}
		return jobAutonomyResult{class: class, level: lvl, failWhy: why}, nil
	}
	recs, err := readJournal(now.Add(-budgetWindow))
	if err != nil {
		return jobAutonomyResult{}, err
	}
	if spent := budgetSpent(cfg.Autonomy.Budget, recs, b.ID, class, action); spent != "" {
		return jobAutonomyResult{class: class, level: lvl, failWhy: spent}, nil
	}
	return jobAutonomyResult{
		class:   class,
		level:   lvl,
		autoTag: fmt.Sprintf("AUTO: [%s L%d C%d]", by, lvl, class),
	}, nil
}

// parkJobForApproval writes a hash-compatible QUESTION:/OPTIONS:/CLASS: note on
// b, sets b to needs_me, and transitions the job to parked. If b is already at
// needs_me, it does not clobber the live question and stops in needs_tj instead.
func parkJobForApproval(env JobEnv, spec JobSpec, b Bead, ar jobAutonomyResult, record func(jobEvent) error) (JobView, error) {
	if b.Status == StatusNeedsMe {
		if err := record(jobEvent{
			Kind:       jobEventNeedsTJ,
			Bead:       b.ID,
			BeadStatus: b.Status,
			Class:      ar.class,
			Level:      ar.level,
			Why:        fmt.Sprintf("%s; %s already has an open needs_me wait", ar.failWhy, b.ID),
		}); err != nil {
			return JobView{}, err
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	actionDesc := fmt.Sprintf("%s %s", spec.Request.Verb, spec.Request.Target)
	var nb strings.Builder
	if ar.class >= 8 {
		fmt.Fprintf(&nb, "CRITICAL: %s (class %d)\n", actionDesc, ar.class)
		fmt.Fprintf(&nb, "QUESTION: Approve %s? (job %s)\n", actionDesc, short(spec.ID))
	} else {
		fmt.Fprintf(&nb, "QUESTION: Approve %s? (job %s) Recommendation: 1 — approve and execute\n", actionDesc, short(spec.ID))
	}
	nb.WriteString("OPTIONS:\n")
	fmt.Fprintf(&nb, "1. Approve and run %s\n", actionDesc)
	fmt.Fprintf(&nb, "2. Decline and cancel job %s\n", short(spec.ID))
	fmt.Fprintf(&nb, "CLASS: %d — %s", ar.class, oneLine(ar.failWhy))
	note := nb.String()

	q, ok := parseNeedsMeQuestion(note)
	if !ok {
		return JobView{}, fmt.Errorf("%w: internal error: parked approval note failed to parse", errJob)
	}
	if env.Note != nil {
		if err := env.Note(b.ID, note); err != nil {
			return JobView{}, fmt.Errorf("write parked approval note on %s: %w", b.ID, err)
		}
	}
	if env.SetStatus != nil {
		if err := env.SetStatus(b.ID, StatusNeedsMe); err != nil {
			return JobView{}, fmt.Errorf("set %s to needs_me for parked job: %w", b.ID, err)
		}
	}
	if err := record(jobEvent{
		Kind:       jobEventParked,
		Bead:       b.ID,
		BeadStatus: StatusNeedsMe,
		Class:      ar.class,
		Level:      ar.level,
		TargetKind: "needs_me",
		TargetHash: q.Hash,
		AuditNote:  note,
		Why:        ar.failWhy,
	}); err != nil {
		return JobView{}, err
	}
	return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
}

// executeJob runs a queued or freshly unparked job through claim -> intent -> effect -> outcome -> end.
func executeJob(ctx context.Context, env JobEnv, spec JobSpec, v JobView, record func(jobEvent) error) (JobView, error) {
	req := spec.Request
	effectiveBy := v.EffectiveBy
	if effectiveBy == "" {
		effectiveBy = req.By
	}
	now := env.clock().UTC()

	// Record claimed transition if coming from queued.
	if v.State == JobStateQueued {
		if err := record(jobEvent{
			Kind:        jobEventClaimed,
			Bead:        v.Bead,
			Class:       -1,
			Level:       -1,
			EffectiveBy: effectiveBy,
		}); err != nil {
			return v, err
		}
		v = reconstructJob(spec, mustReadJobEvents(spec.ID), now)
	}

	switch req.Verb {
	case JobVerbStatus:
		return execStatusJob(ctx, env, spec, v, record)
	case JobVerbAnswer:
		return execAnswerJob(ctx, env, spec, v, effectiveBy, record)
	case JobVerbHold:
		return execHoldJob(ctx, env, spec, v, effectiveBy, record)
	case JobVerbDispatch:
		return execDispatchJob(ctx, env, spec, v, effectiveBy, record)
	case JobVerbResume:
		return execResumeJob(ctx, env, spec, v, effectiveBy, record)
	case JobVerbStop:
		return execStopJob(ctx, env, spec, v, effectiveBy, record)
	case JobVerbClose:
		return execCloseJob(ctx, env, spec, v, effectiveBy, record)
	case JobVerbMerge:
		return execMergeJob(ctx, env, spec, v, effectiveBy, record)
	default:
		if err := record(jobEvent{Kind: jobEventBlocked, Bead: v.Bead, Class: -1, Level: -1, Why: "unknown verb " + req.Verb}); err != nil {
			return v, err
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
}

func prStateToken(pr PR) string {
	if pr.State == "OPEN" && pr.IsDraft {
		return "draft"
	}
	return strings.ToLower(pr.State)
}

func checksToken(c Checks) string {
	switch {
	case len(c.Failing) > 0:
		return "fail"
	case c.Pending > 0:
		return "pending"
	default:
		return "pass"
	}
}

func freshBeadStatus(env JobEnv, bead, fallback string) string {
	if bead == "" || env.Show == nil {
		return fallback
	}
	if b, err := env.Show(bead); err == nil && b.Status != "" {
		return b.Status
	}
	return fallback
}

func execStatusJob(ctx context.Context, env JobEnv, spec JobSpec, v JobView, record func(jobEvent) error) (JobView, error) {
	target := spec.Request.Target
	if target == "" {
		if env.FleetStatus == nil {
			if err := record(jobEvent{Kind: jobEventBlocked, Class: 0, Level: -1, Why: "fleet status provider unavailable"}); err != nil {
				return v, err
			}
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
		summary, err := env.FleetStatus()
		if err != nil {
			if rerr := record(jobEvent{Kind: jobEventBlocked, Class: 0, Level: -1, Why: err.Error()}); rerr != nil {
				return v, rerr
			}
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
		if err := record(jobEvent{Kind: jobEventDone, Bead: "-", Class: 0, Level: -1, Summary: strings.TrimSpace(summary)}); err != nil {
			return v, err
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}

	beadID := target
	prURL := ""
	if prURLRE.MatchString(target) {
		prURL = target
		if env.BeadForPR != nil {
			if id, ok := env.BeadForPR(prURL); ok {
				beadID = id
			} else {
				beadID = ""
			}
		} else {
			beadID = ""
		}
	}
	var b Bead
	if beadID != "" && env.Show != nil {
		var err error
		b, err = env.Show(beadID)
		if err != nil {
			if rerr := record(jobEvent{Kind: jobEventBlocked, Bead: beadID, Class: 0, Level: -1, Why: err.Error()}); rerr != nil {
				return v, rerr
			}
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
	}
	if prURL == "" && beadID != "" && env.PRForBead != nil {
		if pr, ok := env.PRForBead(beadID); ok {
			prURL = pr.URL
		}
	}
	var prState, checks, head string
	if prURL != "" && env.ViewPR != nil {
		if pr, err := env.ViewPR(ctx, prURL); err == nil {
			prState = prStateToken(pr)
			checks = checksToken(summarizeChecks(pr.Checks))
			head = pr.HeadSHA
		}
	}
	summary := fmt.Sprintf("%s is %s", orDash(beadID), orDash(b.Status))
	if prURL != "" {
		summary += fmt.Sprintf(" (PR %s: %s)", prURL, orDash(prState))
	}
	if err := record(jobEvent{
		Kind:       jobEventDone,
		Bead:       beadID,
		BeadStatus: b.Status,
		PR:         prURL,
		PRState:    prState,
		Checks:     checks,
		Head:       head,
		Class:      0,
		Level:      -1,
		Summary:    summary,
	}); err != nil {
		return v, err
	}
	return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
}

// execAnswerJob answers either a live worker approval prompt or a live needs_me
// question with strict at-most-once claim semantics.
func execAnswerJob(_ context.Context, env JobEnv, spec JobSpec, v JobView, effectiveBy string, record func(jobEvent) error) (JobView, error) {
	req := spec.Request
	beadID := req.Target
	now := env.clock().UTC()

	if env.Show == nil {
		if err := record(jobEvent{Kind: jobEventBlocked, Bead: beadID, Class: -1, Level: -1, Why: "bead reader unavailable"}); err != nil {
			return v, err
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	b, err := env.Show(beadID)
	if err != nil {
		if rerr := record(jobEvent{Kind: jobEventBlocked, Bead: beadID, Class: -1, Level: -1, Why: err.Error()}); rerr != nil {
			return v, rerr
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}

	// 1. Check if a live worker approval prompt is showing (only when no free text).
	pEnv := env.Prompt
	if pEnv.show == nil {
		pEnv.show = env.Show
	}
	if pEnv.note == nil {
		pEnv.note = env.Note
	}
	if pEnv.setStatus == nil {
		pEnv.setStatus = env.SetStatus
	}
	if p, _, _, perr := workerPrompt(pEnv, beadID); perr == nil {
		if req.FreeText != "" {
			if err := record(jobEvent{Kind: jobEventBlocked, Bead: beadID, BeadStatus: b.Status, Class: -1, Level: -1, Why: "tool approval prompts accept enumerated options only, not free text"}); err != nil {
				return v, err
			}
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
		if req.Expect != "" && p.Hash != req.Expect {
			if err := record(jobEvent{Kind: jobEventBlocked, Bead: beadID, BeadStatus: b.Status, Class: -1, Level: -1, Why: fmt.Sprintf("prompt changed: showing [%s], expected [%s]", p.Hash, req.Expect)}); err != nil {
				return v, err
			}
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
		opt, oerr := p.choose(req.Choice)
		if oerr != nil {
			if err := record(jobEvent{Kind: jobEventBlocked, Bead: beadID, BeadStatus: b.Status, Class: -1, Level: -1, Why: oerr.Error()}); err != nil {
				return v, err
			}
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
		cls := answerClass(p, opt, req.Class)
		if effectiveBy != "TJ" {
			lvl, why := env.Cfg.beadLevel(b, autonomyStopped())
			if pEnv.level != nil {
				lvl, why = pEnv.level(beadID)
			}
			if _, aerr := allow(beadID, effectiveBy, fmt.Sprintf("answering prompt [%s] option %d", p.Hash, opt.Number), cls, lvl, why); aerr != nil {
				if rerr := record(jobEvent{Kind: jobEventNeedsTJ, Bead: beadID, BeadStatus: b.Status, Class: cls, Level: lvl, TargetKind: "prompt", TargetHash: p.Hash, Why: aerr.Error()}); rerr != nil {
					return v, rerr
				}
				return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
			}
		}
		if alreadyAnsweredInJournal(journalPrompt, beadID, p.Hash) || noteHasAnswer(b.Notes, "PROMPT ANSWERED:", p.Hash) {
			if err := record(jobEvent{Kind: jobEventBlocked, Bead: beadID, BeadStatus: b.Status, Class: cls, Level: -1, TargetKind: "prompt", TargetHash: p.Hash, Why: "already answered"}); err != nil {
				return v, err
			}
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
		owner, claimed, cerr := claimJobAnswer("prompt", beadID, p.Hash, spec.ID, now)
		if cerr != nil {
			return v, cerr
		}
		if !claimed {
			if err := record(jobEvent{Kind: jobEventBlocked, Bead: beadID, BeadStatus: b.Status, Class: cls, Level: -1, TargetKind: "prompt", TargetHash: p.Hash, Why: fmt.Sprintf("already answered by job %s", short(owner))}); err != nil {
				return v, err
			}
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
		attempt := v.Attempts + 1
		if err := record(jobEvent{
			Kind:        jobEventIntent,
			Attempt:     attempt,
			Bead:        beadID,
			BeadStatus:  b.Status,
			Class:       cls,
			Level:       -1,
			TargetKind:  "prompt",
			TargetHash:  p.Hash,
			EffectiveBy: effectiveBy,
		}); err != nil {
			return v, err
		}
		if env.OnAfterIntent != nil {
			if herr := env.OnAfterIntent(spec.ID); herr != nil {
				return v, herr
			}
		}
		via := "job " + short(spec.ID)
		if req.Why != "" {
			via += " — " + oneLine(req.Why)
		}
		msg, aerr := answerPromptVia(pEnv, beadID, req.Choice, p.Hash, effectiveBy, req.Class, via)
		if aerr != nil {
			_ = record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: beadID, Class: cls, Level: -1, Result: resultFailed, Why: aerr.Error()})
			_ = record(jobEvent{Kind: jobEventBlocked, Bead: beadID, BeadStatus: freshBeadStatus(env, beadID, b.Status), Class: cls, Level: -1, Why: aerr.Error()})
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
		if env.OnAfterEffect != nil {
			if herr := env.OnAfterEffect(spec.ID); herr != nil {
				return v, herr
			}
		}
		noteLine := fmt.Sprintf("PROMPT ANSWERED: %d %s to %s [%s] by %s via %s", opt.Number, shortTitle(opt.Label, 60), p.Summary(), p.Hash, effectiveBy, via)
		bs := freshBeadStatus(env, beadID, b.Status)
		if err := record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: beadID, BeadStatus: bs, Class: cls, Level: -1, Result: resultOK, AuditNote: noteLine, Summary: msg}); err != nil {
			return v, err
		}
		if err := record(jobEvent{Kind: jobEventDone, Bead: beadID, BeadStatus: bs, Class: cls, Level: -1, AuditNote: noteLine, Summary: msg}); err != nil {
			return v, err
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}

	// 2. Otherwise target a live needs_me question (with or without OPTIONS).
	if b.Status != StatusNeedsMe {
		why := fmt.Sprintf("no live prompt or needs_me question on %s (status=%s)", beadID, b.Status)
		if req.Expect != "" && (alreadyAnsweredInJournal(journalNeedsMe, beadID, req.Expect) || noteHasAnswer(b.Notes, "NEEDS_ME ANSWERED:", req.Expect)) {
			why = "already answered"
		} else if strings.Contains(b.Notes, "NEEDS_ME ANSWERED:") {
			why = "already answered (bead is " + b.Status + ")"
		}
		if err := record(jobEvent{Kind: jobEventBlocked, Bead: beadID, BeadStatus: b.Status, Class: -1, Level: -1, Why: why}); err != nil {
			return v, err
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	if pendingCommand(b.Notes) != "" {
		if err := record(jobEvent{Kind: jobEventBlocked, Bead: beadID, BeadStatus: b.Status, Class: -1, Level: -1, Why: "bead is waiting on a RUN: command, not an answerable question"}); err != nil {
			return v, err
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}

	q, ok := parseTappableNeedsMeQuestion(b.Notes)
	if !ok {
		if err := record(jobEvent{Kind: jobEventBlocked, Bead: beadID, BeadStatus: b.Status, Class: -1, Level: -1, Why: "no live question found on " + beadID}); err != nil {
			return v, err
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	qHash := q.Hash
	qSummary := q.Summary()
	qClass := q.ClassN
	qCritical := len(q.Lines) > 0 && strings.HasPrefix(q.Lines[0], "CRITICAL:")
	var chosenOpt *PromptOption
	if req.Choice != "" {
		if len(q.Options) == 0 {
			if err := record(jobEvent{Kind: jobEventBlocked, Bead: beadID, BeadStatus: b.Status, Class: max(qClass, -1), Level: -1, Why: "question has no OPTIONS block; answer with free text"}); err != nil {
				return v, err
			}
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
		o, oerr := q.choose(req.Choice)
		if oerr != nil {
			if err := record(jobEvent{Kind: jobEventBlocked, Bead: beadID, BeadStatus: b.Status, Class: max(qClass, -1), Level: -1, Why: oerr.Error()}); err != nil {
				return v, err
			}
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
		chosenOpt = &o
	}

	if req.Expect != "" && qHash != req.Expect {
		if err := record(jobEvent{Kind: jobEventBlocked, Bead: beadID, BeadStatus: b.Status, Class: max(qClass, -1), Level: -1, TargetKind: "needs_me", TargetHash: qHash, Why: fmt.Sprintf("question changed: [%s] != expected [%s]", qHash, req.Expect)}); err != nil {
			return v, err
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}

	// Autonomy check for non-TJ answers.
	effClass := max(qClass, req.Class)
	if qCritical {
		effClass = max(effClass, 8)
	}
	autoPrefix := ""
	if effectiveBy != "TJ" {
		if effClass < 0 {
			effClass = 9
		}
		lvl, why := env.Cfg.beadLevel(b, autonomyStopped())
		if qCritical {
			if err := record(jobEvent{Kind: jobEventNeedsTJ, Bead: beadID, BeadStatus: b.Status, Class: effClass, Level: lvl, TargetKind: "needs_me", TargetHash: qHash, Why: "CRITICAL question requires TJ"}); err != nil {
				return v, err
			}
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
		var aerr error
		if autoPrefix, aerr = allow(beadID, effectiveBy, "answering needs_me question ["+qHash+"]", effClass, lvl, why); aerr != nil {
			if rerr := record(jobEvent{Kind: jobEventNeedsTJ, Bead: beadID, BeadStatus: b.Status, Class: effClass, Level: lvl, TargetKind: "needs_me", TargetHash: qHash, Why: aerr.Error()}); rerr != nil {
				return v, rerr
			}
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
	}

	// Check if worker is already working again (matches tapquestion.go guard).
	host, worker := "", (*Agent)(nil)
	if pEnv.agent != nil {
		host, worker = questionWorker(pEnv, b)
		if worker != nil && worker.Status == "working" {
			if err := record(jobEvent{Kind: jobEventBlocked, Bead: beadID, BeadStatus: b.Status, Class: effClass, Level: -1, TargetKind: "needs_me", TargetHash: qHash, Why: errWorkerWorking.Error()}); err != nil {
				return v, err
			}
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
	}

	// At-most-once guard: check journal/notes AND atomically claim (needs_me, bead, qHash).
	if alreadyAnsweredInJournal(journalNeedsMe, beadID, qHash) || noteHasAnswer(b.Notes, "NEEDS_ME ANSWERED:", qHash) {
		if err := record(jobEvent{Kind: jobEventBlocked, Bead: beadID, BeadStatus: b.Status, Class: effClass, Level: -1, TargetKind: "needs_me", TargetHash: qHash, Why: "already answered"}); err != nil {
			return v, err
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	owner, claimed, cerr := claimJobAnswer("needs_me", beadID, qHash, spec.ID, now)
	if cerr != nil {
		return v, cerr
	}
	if !claimed {
		if err := record(jobEvent{Kind: jobEventBlocked, Bead: beadID, BeadStatus: b.Status, Class: effClass, Level: -1, TargetKind: "needs_me", TargetHash: qHash, Why: fmt.Sprintf("already answered by job %s", short(owner))}); err != nil {
			return v, err
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}

	attempt := v.Attempts + 1
	if err := record(jobEvent{
		Kind:        jobEventIntent,
		Attempt:     attempt,
		Bead:        beadID,
		BeadStatus:  b.Status,
		Class:       effClass,
		Level:       -1,
		TargetKind:  "needs_me",
		TargetHash:  qHash,
		EffectiveBy: effectiveBy,
	}); err != nil {
		return v, err
	}
	if env.OnAfterIntent != nil {
		if herr := env.OnAfterIntent(spec.ID); herr != nil {
			return v, herr
		}
	}

	via := "job " + short(spec.ID)
	note := formatJobNeedsMeAnswerNote(chosenOpt, req.FreeText, req.Why, qSummary, qHash, effectiveBy, via, autoPrefix)
	if env.Note != nil {
		if err := env.Note(beadID, note); err != nil {
			_ = record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: beadID, Class: effClass, Level: -1, Result: resultFailed, Why: err.Error()})
			_ = record(jobEvent{Kind: jobEventBlocked, Bead: beadID, BeadStatus: b.Status, Class: effClass, Level: -1, Why: "noting answer failed: " + err.Error()})
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
	}
	if env.SetStatus != nil {
		if err := env.SetStatus(beadID, StatusInProgress); err != nil {
			_ = record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: beadID, Class: effClass, Level: -1, Result: resultFailed, Why: err.Error()})
			_ = record(jobEvent{Kind: jobEventBlocked, Bead: beadID, BeadStatus: b.Status, Class: effClass, Level: -1, AuditNote: note, Why: "returning bead to in_progress failed: " + err.Error()})
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
	}
	_ = appendJournal(journalRecord{At: env.clock().UTC(), Kind: journalNeedsMe, Bead: beadID, Class: effClass, Text: note})

	// Prompt idle worker unless this question belongs to a parked job on the ticker.
	summary := fmt.Sprintf("answered %s [%s]; bead is in_progress", beadID, qHash)
	if hasParkedJobForHash(beadID, qHash) {
		summary += "; parked job will resume"
	} else if worker != nil && agentReady(worker) && pEnv.tell != nil {
		promptText := formatJobNeedsMeWorkerPrompt(chosenOpt, req.FreeText, qSummary, effectiveBy, via)
		if terr := pEnv.tell(host, worker, promptText); terr != nil {
			summary += "; worker not prompted (" + terr.Error() + ")"
		} else {
			summary += "; worker prompted"
		}
	}

	if env.OnAfterEffect != nil {
		if herr := env.OnAfterEffect(spec.ID); herr != nil {
			return v, herr
		}
	}

	bs := freshBeadStatus(env, beadID, StatusInProgress)
	if err := record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: beadID, BeadStatus: bs, Class: effClass, Level: -1, Result: resultOK, AuditNote: note, Summary: summary}); err != nil {
		return v, err
	}
	if err := record(jobEvent{Kind: jobEventDone, Bead: beadID, BeadStatus: bs, Class: effClass, Level: -1, AuditNote: note, Summary: summary}); err != nil {
		return v, err
	}
	return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
}

func formatJobNeedsMeAnswerNote(opt *PromptOption, freeText, why, qSummary, qHash, by, via, autoPrefix string) string {
	var body string
	switch {
	case opt != nil && freeText != "":
		body = fmt.Sprintf("NEEDS_ME ANSWERED: %d %s to %s [%s] by %s via %s: %s",
			opt.Number, shortTitle(opt.Label, 60), shortTitle(qSummary, 80), qHash, by, via, freeText)
	case opt != nil:
		body = fmt.Sprintf("NEEDS_ME ANSWERED: %d %s to %s [%s] by %s via %s",
			opt.Number, shortTitle(opt.Label, 60), shortTitle(qSummary, 80), qHash, by, via)
		if why != "" {
			body += " — " + oneLine(why)
		}
	default:
		body = fmt.Sprintf("NEEDS_ME ANSWERED: free text to %s [%s] by %s via %s: %s",
			shortTitle(qSummary, 80), qHash, by, via, freeText)
	}
	if autoPrefix != "" {
		return autoPrefix + " " + body
	}
	return body
}

func formatJobNeedsMeWorkerPrompt(opt *PromptOption, freeText, qSummary, by, via string) string {
	var choice string
	switch {
	case opt != nil && freeText != "":
		choice = fmt.Sprintf("%d. %s\nAdditional text: %s", opt.Number, opt.Label, freeText)
	case opt != nil:
		choice = fmt.Sprintf("%d. %s", opt.Number, opt.Label)
	default:
		choice = freeText
	}
	return fmt.Sprintf("%s answered your needs_me question (via %s): %s\nThe question was: %s\nThe bead is back to in_progress and the answer is noted on it as NEEDS_ME ANSWERED. Carry on with that choice.", by, via, choice, qSummary)
}

func noteHasAnswer(notes, prefix, hash string) bool {
	if hash == "" {
		return false
	}
	needle := "[" + hash + "]"
	for _, raw := range strings.Split(notes, "\n") {
		if strings.Contains(raw, prefix) && strings.Contains(raw, needle) {
			return true
		}
	}
	return false
}

func alreadyAnsweredInJournal(kind, bead, hash string) bool {
	if hash == "" {
		return false
	}
	recs, err := readJournal(time.Time{})
	if err != nil {
		return false
	}
	needle := "[" + hash + "]"
	for _, r := range slices.Backward(recs) {
		if r.Kind == kind && r.Bead == bead && strings.Contains(r.Text, needle) {
			return true
		}
	}
	return false
}

func execHoldJob(_ context.Context, env JobEnv, spec JobSpec, v JobView, effectiveBy string, record func(jobEvent) error) (JobView, error) {
	req := spec.Request
	b, err := env.Show(req.Target)
	if err != nil {
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: req.Target, Class: -1, Level: -1, Why: err.Error()})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	if b.Status == StatusClosed {
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: b.Status, Class: -1, Level: -1, Why: b.ID + " is closed"})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	ar, err := checkJobAutonomy(env.Cfg, b, "", max(3, req.Class), effectiveBy, env.clock().UTC())
	if err != nil {
		return v, err
	}
	if ar.failWhy != "" {
		return parkJobForApproval(env, spec, b, ar, record)
	}
	attempt := v.Attempts + 1
	if err := record(jobEvent{Kind: jobEventIntent, Attempt: attempt, Bead: b.ID, BeadStatus: b.Status, Class: ar.class, Level: ar.level, EffectiveBy: effectiveBy}); err != nil {
		return v, err
	}
	if env.OnAfterIntent != nil {
		if herr := env.OnAfterIntent(spec.ID); herr != nil {
			return v, herr
		}
	}
	if err := setJobHold(jobHoldRecord{Bead: b.ID, JobID: spec.ID, By: effectiveBy, Why: req.Why, At: env.clock().UTC()}); err != nil {
		_ = record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: b.ID, Class: ar.class, Level: ar.level, Result: resultFailed, Why: err.Error()})
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: b.Status, Class: ar.class, Level: ar.level, Why: err.Error()})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	note := fmt.Sprintf("HOLD [job:%s]: parked by %s", short(spec.ID), effectiveBy)
	if req.Why != "" {
		note += " — " + oneLine(req.Why)
	}
	if ar.autoTag != "" {
		note = ar.autoTag + " " + note
	}
	if env.Note != nil {
		if err := env.Note(b.ID, note); err != nil {
			_ = record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: b.ID, Class: ar.class, Level: ar.level, Result: resultFailed, Why: err.Error()})
			_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: b.Status, Class: ar.class, Level: ar.level, Why: err.Error()})
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
	}
	if env.OnAfterEffect != nil {
		if herr := env.OnAfterEffect(spec.ID); herr != nil {
			return v, herr
		}
	}
	bs := freshBeadStatus(env, b.ID, b.Status)
	summary := fmt.Sprintf("held %s; no dispatch/resume until RESUME", b.ID)
	if err := record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: b.ID, BeadStatus: bs, Class: ar.class, Level: ar.level, Result: resultOK, AuditNote: note, Summary: summary}); err != nil {
		return v, err
	}
	if err := record(jobEvent{Kind: jobEventDone, Bead: b.ID, BeadStatus: bs, Class: ar.class, Level: ar.level, AuditNote: note, Summary: summary}); err != nil {
		return v, err
	}
	return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
}

func execDispatchJob(_ context.Context, env JobEnv, spec JobSpec, v JobView, effectiveBy string, record func(jobEvent) error) (JobView, error) {
	req := spec.Request
	b, err := env.Show(req.Target)
	if err != nil {
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: req.Target, Class: -1, Level: -1, Why: err.Error()})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	if b.Status == StatusClosed {
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: b.Status, Class: -1, Level: -1, Why: b.ID + " is closed"})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	if hrec, held := isJobHeld(b.ID); held {
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: b.Status, Class: -1, Level: -1, Why: fmt.Sprintf("%s is on hold (job %s); submit RESUME first", b.ID, short(hrec.JobID))})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	ar, err := checkJobAutonomy(env.Cfg, b, "dispatch", req.Class, effectiveBy, env.clock().UTC())
	if err != nil {
		return v, err
	}
	if ar.failWhy != "" {
		return parkJobForApproval(env, spec, b, ar, record)
	}
	attempt := v.Attempts + 1
	if err := record(jobEvent{Kind: jobEventIntent, Attempt: attempt, Bead: b.ID, BeadStatus: b.Status, Class: ar.class, Level: ar.level, EffectiveBy: effectiveBy}); err != nil {
		return v, err
	}
	if env.OnAfterIntent != nil {
		if herr := env.OnAfterIntent(spec.ID); herr != nil {
			return v, herr
		}
	}
	if env.Dispatch == nil {
		_ = record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: b.ID, Class: ar.class, Level: ar.level, Result: resultFailed, Why: "dispatch handler unavailable"})
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: b.Status, Class: ar.class, Level: ar.level, Why: "dispatch handler unavailable"})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	msg, derr := env.Dispatch(b, effectiveBy)
	if derr != nil {
		_ = record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: b.ID, Class: ar.class, Level: ar.level, Result: resultFailed, Why: derr.Error()})
		if errors.Is(derr, errNotAutonomous) {
			_ = record(jobEvent{Kind: jobEventNeedsTJ, Bead: b.ID, BeadStatus: freshBeadStatus(env, b.ID, b.Status), Class: ar.class, Level: ar.level, Why: derr.Error()})
		} else {
			_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: freshBeadStatus(env, b.ID, b.Status), Class: ar.class, Level: ar.level, Why: derr.Error()})
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	note := fmt.Sprintf("DISPATCH [job:%s]: %s", short(spec.ID), oneLine(msg))
	if req.Why != "" {
		note += " — " + oneLine(req.Why)
	}
	if ar.autoTag != "" {
		note = ar.autoTag + " " + note
	}
	if env.Note != nil {
		_ = env.Note(b.ID, note)
	}
	if env.OnAfterEffect != nil {
		if herr := env.OnAfterEffect(spec.ID); herr != nil {
			return v, herr
		}
	}
	bs := freshBeadStatus(env, b.ID, StatusInProgress)
	if err := record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: b.ID, BeadStatus: bs, Class: ar.class, Level: ar.level, Result: resultOK, AuditNote: note, Summary: msg}); err != nil {
		return v, err
	}
	if err := record(jobEvent{Kind: jobEventDone, Bead: b.ID, BeadStatus: bs, Class: ar.class, Level: ar.level, AuditNote: note, Summary: msg}); err != nil {
		return v, err
	}
	return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
}

func execResumeJob(_ context.Context, env JobEnv, spec JobSpec, v JobView, effectiveBy string, record func(jobEvent) error) (JobView, error) {
	req := spec.Request
	b, err := env.Show(req.Target)
	if err != nil {
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: req.Target, Class: -1, Level: -1, Why: err.Error()})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	if b.Status == StatusClosed {
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: b.Status, Class: -1, Level: -1, Why: b.ID + " is closed"})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	ar, err := checkJobAutonomy(env.Cfg, b, "resume", req.Class, effectiveBy, env.clock().UTC())
	if err != nil {
		return v, err
	}
	if ar.failWhy != "" {
		return parkJobForApproval(env, spec, b, ar, record)
	}
	attempt := v.Attempts + 1
	if err := record(jobEvent{Kind: jobEventIntent, Attempt: attempt, Bead: b.ID, BeadStatus: b.Status, Class: ar.class, Level: ar.level, EffectiveBy: effectiveBy}); err != nil {
		return v, err
	}
	if env.OnAfterIntent != nil {
		if herr := env.OnAfterIntent(spec.ID); herr != nil {
			return v, herr
		}
	}
	wasHeld, err := clearJobHold(b.ID)
	if err != nil {
		_ = record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: b.ID, Class: ar.class, Level: ar.level, Result: resultFailed, Why: err.Error()})
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: b.Status, Class: ar.class, Level: ar.level, Why: err.Error()})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	var msg string
	if env.Resume != nil {
		var rerr error
		msg, rerr = env.Resume(b, effectiveBy)
		if rerr != nil {
			if wasHeld && strings.Contains(rerr.Error(), "nothing to resume") {
				msg = "cleared hold on " + b.ID
			} else {
				_ = record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: b.ID, Class: ar.class, Level: ar.level, Result: resultFailed, Why: rerr.Error()})
				if errors.Is(rerr, errNotAutonomous) {
					_ = record(jobEvent{Kind: jobEventNeedsTJ, Bead: b.ID, BeadStatus: freshBeadStatus(env, b.ID, b.Status), Class: ar.class, Level: ar.level, Why: rerr.Error()})
				} else {
					_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: freshBeadStatus(env, b.ID, b.Status), Class: ar.class, Level: ar.level, Why: rerr.Error()})
				}
				return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
			}
		}
	} else if wasHeld {
		msg = "cleared hold on " + b.ID
	}
	note := fmt.Sprintf("RESUME [job:%s]: %s", short(spec.ID), oneLine(msg))
	if req.Why != "" {
		note += " — " + oneLine(req.Why)
	}
	if ar.autoTag != "" {
		note = ar.autoTag + " " + note
	}
	if env.Note != nil {
		_ = env.Note(b.ID, note)
	}
	if env.OnAfterEffect != nil {
		if herr := env.OnAfterEffect(spec.ID); herr != nil {
			return v, herr
		}
	}
	bs := freshBeadStatus(env, b.ID, b.Status)
	if err := record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: b.ID, BeadStatus: bs, Class: ar.class, Level: ar.level, Result: resultOK, AuditNote: note, Summary: msg}); err != nil {
		return v, err
	}
	if err := record(jobEvent{Kind: jobEventDone, Bead: b.ID, BeadStatus: bs, Class: ar.class, Level: ar.level, AuditNote: note, Summary: msg}); err != nil {
		return v, err
	}
	return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
}

func execStopJob(_ context.Context, env JobEnv, spec JobSpec, v JobView, effectiveBy string, record func(jobEvent) error) (JobView, error) {
	req := spec.Request
	b, err := env.Show(req.Target)
	if err != nil {
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: req.Target, Class: -1, Level: -1, Why: err.Error()})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	ar, err := checkJobAutonomy(env.Cfg, b, "resume", req.Class, effectiveBy, env.clock().UTC())
	if err != nil {
		return v, err
	}
	if ar.failWhy != "" {
		return parkJobForApproval(env, spec, b, ar, record)
	}
	attempt := v.Attempts + 1
	if err := record(jobEvent{Kind: jobEventIntent, Attempt: attempt, Bead: b.ID, BeadStatus: b.Status, Class: ar.class, Level: ar.level, EffectiveBy: effectiveBy}); err != nil {
		return v, err
	}
	if env.OnAfterIntent != nil {
		if herr := env.OnAfterIntent(spec.ID); herr != nil {
			return v, herr
		}
	}
	if env.StopWorker == nil {
		_ = record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: b.ID, Class: ar.class, Level: ar.level, Result: resultFailed, Why: "stop handler unavailable"})
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: b.Status, Class: ar.class, Level: ar.level, Why: "stop handler unavailable"})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	msg, serr := env.StopWorker(b)
	if serr != nil {
		_ = record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: b.ID, Class: ar.class, Level: ar.level, Result: resultFailed, Why: serr.Error()})
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: freshBeadStatus(env, b.ID, b.Status), Class: ar.class, Level: ar.level, Why: serr.Error()})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	note := fmt.Sprintf("STOP [job:%s]: %s", short(spec.ID), oneLine(msg))
	if req.Why != "" {
		note += " — " + oneLine(req.Why)
	}
	if ar.autoTag != "" {
		note = ar.autoTag + " " + note
	}
	if env.Note != nil {
		_ = env.Note(b.ID, note)
	}
	if env.OnAfterEffect != nil {
		if herr := env.OnAfterEffect(spec.ID); herr != nil {
			return v, herr
		}
	}
	bs := freshBeadStatus(env, b.ID, b.Status)
	if err := record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: b.ID, BeadStatus: bs, Class: ar.class, Level: ar.level, Result: resultOK, AuditNote: note, Summary: msg}); err != nil {
		return v, err
	}
	if err := record(jobEvent{Kind: jobEventDone, Bead: b.ID, BeadStatus: bs, Class: ar.class, Level: ar.level, AuditNote: note, Summary: msg}); err != nil {
		return v, err
	}
	return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
}

func execCloseJob(_ context.Context, env JobEnv, spec JobSpec, v JobView, effectiveBy string, record func(jobEvent) error) (JobView, error) {
	req := spec.Request
	b, err := env.Show(req.Target)
	if err != nil {
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: req.Target, Class: -1, Level: -1, Why: err.Error()})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	if b.Status == StatusClosed {
		_ = record(jobEvent{Kind: jobEventDone, Bead: b.ID, BeadStatus: StatusClosed, Class: 3, Level: -1, Summary: b.ID + " is already closed"})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	ar, err := checkJobAutonomy(env.Cfg, b, "close", req.Class, effectiveBy, env.clock().UTC())
	if err != nil {
		return v, err
	}
	if ar.failWhy != "" {
		return parkJobForApproval(env, spec, b, ar, record)
	}
	attempt := v.Attempts + 1
	if err := record(jobEvent{Kind: jobEventIntent, Attempt: attempt, Bead: b.ID, BeadStatus: b.Status, Class: ar.class, Level: ar.level, EffectiveBy: effectiveBy}); err != nil {
		return v, err
	}
	if env.OnAfterIntent != nil {
		if herr := env.OnAfterIntent(spec.ID); herr != nil {
			return v, herr
		}
	}
	if env.Close == nil {
		_ = record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: b.ID, Class: ar.class, Level: ar.level, Result: resultFailed, Why: "close handler unavailable"})
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: b.Status, Class: ar.class, Level: ar.level, Why: "close handler unavailable"})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	msg, cerr := env.Close(b, req.Why, effectiveBy)
	if cerr != nil {
		_ = record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: b.ID, Class: ar.class, Level: ar.level, Result: resultFailed, Why: cerr.Error()})
		bs := freshBeadStatus(env, b.ID, b.Status)
		if errors.Is(cerr, errNotAutonomous) || (errors.Is(cerr, errVerifyFailed) && bs == StatusNeedsMe) {
			_ = record(jobEvent{Kind: jobEventNeedsTJ, Bead: b.ID, BeadStatus: bs, Class: ar.class, Level: ar.level, Why: cerr.Error()})
		} else {
			_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: bs, Class: ar.class, Level: ar.level, Why: cerr.Error()})
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	if env.OnAfterEffect != nil {
		if herr := env.OnAfterEffect(spec.ID); herr != nil {
			return v, herr
		}
	}
	note := fmt.Sprintf("CLOSED [job:%s]: %s", short(spec.ID), oneLine(req.Why))
	if ar.autoTag != "" {
		note = ar.autoTag + " " + note
	}
	bs := freshBeadStatus(env, b.ID, StatusClosed)
	if msg == "" {
		msg = fmt.Sprintf("closed %s", b.ID)
	}
	if err := record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: b.ID, BeadStatus: bs, Class: ar.class, Level: ar.level, Result: resultOK, AuditNote: note, Summary: msg}); err != nil {
		return v, err
	}
	if err := record(jobEvent{Kind: jobEventDone, Bead: b.ID, BeadStatus: bs, Class: ar.class, Level: ar.level, AuditNote: note, Summary: msg}); err != nil {
		return v, err
	}
	return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
}

func execMergeJob(ctx context.Context, env JobEnv, spec JobSpec, v JobView, effectiveBy string, record func(jobEvent) error) (JobView, error) {
	req := spec.Request
	beadID := req.Target
	prURL := ""
	if prURLRE.MatchString(req.Target) {
		prURL = req.Target
		if env.BeadForPR == nil {
			_ = record(jobEvent{Kind: jobEventBlocked, PR: prURL, Class: -1, Level: -1, Why: "PR-to-bead lookup unavailable"})
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
		var ok bool
		beadID, ok = env.BeadForPR(prURL)
		if !ok || beadID == "" {
			_ = record(jobEvent{Kind: jobEventBlocked, PR: prURL, Class: -1, Level: -1, Why: fmt.Sprintf("no bead found for %s", prURL)})
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
	}
	b, err := env.Show(beadID)
	if err != nil {
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: beadID, PR: prURL, Class: -1, Level: -1, Why: err.Error()})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	if b.Status == StatusClosed {
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: b.Status, PR: prURL, Class: -1, Level: -1, Why: b.ID + " is closed"})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	if prURL == "" {
		if env.PRForBead != nil {
			if pr, ok := env.PRForBead(b.ID); ok {
				prURL = pr.URL
			}
		}
		if prURL == "" {
			_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: b.Status, Class: -1, Level: -1, Why: "no PR found for " + b.ID})
			return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
		}
	}
	ar, err := checkJobAutonomy(env.Cfg, b, "merge", req.Class, effectiveBy, env.clock().UTC())
	if err != nil {
		return v, err
	}
	if ar.failWhy != "" {
		return parkJobForApproval(env, spec, b, ar, record)
	}
	if env.ViewPR == nil {
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: b.Status, PR: prURL, Class: ar.class, Level: ar.level, Why: "PR reader unavailable"})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	pr, perr := env.ViewPR(ctx, prURL)
	if perr != nil {
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: b.Status, PR: prURL, Class: ar.class, Level: ar.level, Why: perr.Error()})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	if pr.URL == "" {
		pr.URL = prURL
	}
	checks := checksToken(summarizeChecks(pr.Checks))
	if pr.State == "MERGED" {
		_ = record(jobEvent{
			Kind:       jobEventDone,
			Bead:       b.ID,
			BeadStatus: b.Status,
			PR:         pr.URL,
			PRState:    "merged",
			Checks:     checks,
			Head:       pr.HeadSHA,
			Class:      ar.class,
			Level:      ar.level,
			Summary:    fmt.Sprintf("PR %s already merged at %s", pr.URL, short(pr.HeadSHA)),
		})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	if pr.State != "OPEN" || pr.IsDraft {
		_ = record(jobEvent{
			Kind:       jobEventBlocked,
			Bead:       b.ID,
			BeadStatus: b.Status,
			PR:         pr.URL,
			PRState:    prStateToken(pr),
			Checks:     checks,
			Head:       pr.HeadSHA,
			Class:      ar.class,
			Level:      ar.level,
			Why:        fmt.Sprintf("PR %s is %s", pr.URL, prStateToken(pr)),
		})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}

	attempt := v.Attempts + 1
	if err := record(jobEvent{
		Kind:        jobEventIntent,
		Attempt:     attempt,
		Bead:        b.ID,
		BeadStatus:  b.Status,
		PR:          pr.URL,
		PRState:     prStateToken(pr),
		Checks:      checks,
		Head:        pr.HeadSHA,
		Class:       ar.class,
		Level:       ar.level,
		EffectiveBy: effectiveBy,
	}); err != nil {
		return v, err
	}
	if env.OnAfterIntent != nil {
		if herr := env.OnAfterIntent(spec.ID); herr != nil {
			return v, herr
		}
	}
	if env.MergePR == nil {
		_ = record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: b.ID, PR: pr.URL, Head: pr.HeadSHA, Class: ar.class, Level: ar.level, Result: resultFailed, Why: "merge handler unavailable"})
		_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: b.Status, PR: pr.URL, PRState: prStateToken(pr), Checks: checks, Head: pr.HeadSHA, Class: ar.class, Level: ar.level, Why: "merge handler unavailable"})
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	evidence, merr := env.MergePR(ctx, b, pr, effectiveBy)
	if merr != nil {
		_ = record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: b.ID, PR: pr.URL, Head: pr.HeadSHA, Class: ar.class, Level: ar.level, Result: resultFailed, Why: merr.Error()})
		bs := freshBeadStatus(env, b.ID, b.Status)
		if errors.Is(merr, errNotAutonomous) {
			_ = record(jobEvent{Kind: jobEventNeedsTJ, Bead: b.ID, BeadStatus: bs, PR: pr.URL, PRState: prStateToken(pr), Checks: checks, Head: pr.HeadSHA, Class: ar.class, Level: ar.level, Why: merr.Error()})
		} else {
			_ = record(jobEvent{Kind: jobEventBlocked, Bead: b.ID, BeadStatus: bs, PR: pr.URL, PRState: prStateToken(pr), Checks: checks, Head: pr.HeadSHA, Class: ar.class, Level: ar.level, Why: merr.Error()})
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	note := fmt.Sprintf("MERGED [job:%s]: %s", short(spec.ID), oneLine(evidence))
	if req.Why != "" {
		note += " — " + oneLine(req.Why)
	}
	if ar.autoTag != "" {
		note = ar.autoTag + " " + note
	}
	if env.Note != nil {
		_ = env.Note(b.ID, note)
	}
	if env.OnAfterEffect != nil {
		if herr := env.OnAfterEffect(spec.ID); herr != nil {
			return v, herr
		}
	}
	bs := freshBeadStatus(env, b.ID, b.Status)
	if err := record(jobEvent{Kind: jobEventOutcome, Attempt: attempt, Bead: b.ID, BeadStatus: bs, PR: pr.URL, PRState: "merged", Checks: checks, Head: pr.HeadSHA, Class: ar.class, Level: ar.level, Result: resultOK, AuditNote: note, Summary: evidence}); err != nil {
		return v, err
	}
	if err := record(jobEvent{Kind: jobEventDone, Bead: b.ID, BeadStatus: bs, PR: pr.URL, PRState: "merged", Checks: checks, Head: pr.HeadSHA, Class: ar.class, Level: ar.level, AuditNote: note, Summary: evidence}); err != nil {
		return v, err
	}
	return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
}

// reconcilePendingJob settles a job whose previous ticker process crashed after
// fsyncing an intent event and before recording an outcome. It probes live state
// first so an effect that already landed is never executed a second time; ops
// that are not retry-safe (like ANSWER) stop in needs_tj rather than double-acting.
func reconcilePendingJob(ctx context.Context, env JobEnv, spec JobSpec, v JobView, record func(jobEvent) error) (JobView, error) {
	happened, known, retrySafe, evText, prState, beadStatus, otherAnswered := probeJobEffect(ctx, env, spec, v)
	if otherAnswered {
		if err := record(jobEvent{
			Kind:       jobEventOutcome,
			Attempt:    v.Attempts,
			Bead:       v.Bead,
			BeadStatus: beadStatus,
			Class:      v.Class,
			Level:      v.Level,
			Result:     resultFailed,
			Why:        "already answered",
		}); err != nil {
			return v, err
		}
		if err := record(jobEvent{
			Kind:       jobEventBlocked,
			Bead:       v.Bead,
			BeadStatus: beadStatus,
			Class:      v.Class,
			Level:      v.Level,
			Why:        "already answered",
		}); err != nil {
			return v, err
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	if happened {
		if err := record(jobEvent{
			Kind:       jobEventOutcome,
			Attempt:    v.Attempts,
			Bead:       v.Bead,
			BeadStatus: beadStatus,
			PR:         v.PR,
			PRState:    prState,
			Checks:     v.Checks,
			Head:       v.Head,
			Class:      v.Class,
			Level:      v.Level,
			Result:     resultOK,
			AuditNote:  evText,
			Summary:    "recovered via probe: " + evText,
		}); err != nil {
			return v, err
		}
		if err := record(jobEvent{
			Kind:       jobEventDone,
			Bead:       v.Bead,
			BeadStatus: beadStatus,
			PR:         v.PR,
			PRState:    prState,
			Checks:     v.Checks,
			Head:       v.Head,
			Class:      v.Class,
			Level:      v.Level,
			AuditNote:  evText,
			Summary:    "recovered via probe: " + evText,
		}); err != nil {
			return v, err
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	if !known {
		if err := record(jobEvent{
			Kind:       jobEventNeedsTJ,
			Bead:       v.Bead,
			BeadStatus: beadStatus,
			PR:         v.PR,
			PRState:    prState,
			Head:       v.Head,
			Class:      v.Class,
			Level:      v.Level,
			Why:        fmt.Sprintf("interrupted mid-%s and probe could not determine outcome; verify before retrying", spec.Request.Verb),
		}); err != nil {
			return v, err
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	if !retrySafe || v.Attempts >= jobMaxAttempts {
		why := fmt.Sprintf("interrupted mid-%s; not retry-safe, verify state before retrying", spec.Request.Verb)
		if retrySafe {
			why = fmt.Sprintf("%s did not complete after %d attempts", spec.Request.Verb, v.Attempts)
		}
		if err := record(jobEvent{
			Kind:       jobEventNeedsTJ,
			Bead:       v.Bead,
			BeadStatus: beadStatus,
			PR:         v.PR,
			PRState:    prState,
			Head:       v.Head,
			Class:      v.Class,
			Level:      v.Level,
			Why:        why,
		}); err != nil {
			return v, err
		}
		return reconstructJob(spec, mustReadJobEvents(spec.ID), env.clock().UTC()), nil
	}
	// Retry-safe op confirmed not to have happened yet: re-execute.
	return executeJob(ctx, env, spec, v, record)
}

func probeJobEffect(ctx context.Context, env JobEnv, spec JobSpec, v JobView) (happened, known, retrySafe bool, evidence, prState, beadStatus string, otherAnswered bool) {
	jobTag := short(spec.ID)
	switch spec.Request.Verb {
	case JobVerbStatus:
		return false, true, true, "", "", "", false

	case JobVerbAnswer:
		if env.Show == nil || v.Bead == "" {
			return false, false, false, "", "", "", false
		}
		b, err := env.Show(v.Bead)
		if err != nil {
			return false, false, false, "", "", "", false
		}
		beadStatus = b.Status
		if v.TargetKind == "needs_me" && v.TargetHash != "" {
			needle := "[" + v.TargetHash + "]"
			// Search bead notes for NEEDS_ME ANSWERED for this exact hash.
			for _, raw := range slices.Backward(strings.Split(b.Notes, "\n")) {
				l := strings.TrimSpace(raw)
				if strings.Contains(l, "NEEDS_ME ANSWERED:") && strings.Contains(l, needle) {
					if strings.Contains(l, "via job "+jobTag) {
						// Complete status flip if crashed between bd note and bd setStatus.
						if b.Status == StatusNeedsMe && env.SetStatus != nil {
							_ = env.SetStatus(b.ID, StatusInProgress)
							beadStatus = StatusInProgress
						}
						if !alreadyAnsweredInJournal(journalNeedsMe, b.ID, v.TargetHash) {
							_ = appendJournal(journalRecord{At: env.clock().UTC(), Kind: journalNeedsMe, Bead: b.ID, Class: v.Class, Text: l})
						}
						return true, true, false, l, "", beadStatus, false
					}
					return false, true, false, l, "", beadStatus, true
				}
			}
			// Also check journalNeedsMe.
			if recs, rerr := readJournal(time.Time{}); rerr == nil {
				for _, r := range slices.Backward(recs) {
					if r.Kind == journalNeedsMe && r.Bead == b.ID && strings.Contains(r.Text, needle) {
						if strings.Contains(r.Text, "via job "+jobTag) {
							return true, true, false, r.Text, "", beadStatus, false
						}
						return false, true, false, r.Text, "", beadStatus, true
					}
				}
			}
			// Not noted: strictly at-most-once -> never blind-retry after intent.
			return false, true, false, "", "", beadStatus, false
		}
		if v.TargetKind == "prompt" && v.TargetHash != "" {
			needle := "[" + v.TargetHash + "]"
			for _, raw := range slices.Backward(strings.Split(b.Notes, "\n")) {
				l := strings.TrimSpace(raw)
				if strings.Contains(l, "PROMPT ANSWERED:") && strings.Contains(l, needle) {
					if strings.Contains(l, "via job "+jobTag) {
						return true, true, false, l, "", beadStatus, false
					}
					return false, true, false, l, "", beadStatus, true
				}
			}
			return false, false, false, "", "", beadStatus, false
		}
		return false, false, false, "", "", beadStatus, false

	case JobVerbMerge:
		if v.PR == "" || env.ViewPR == nil {
			return false, false, false, "", "", freshBeadStatus(env, v.Bead, v.BeadStatus), false
		}
		pr, err := env.ViewPR(ctx, v.PR)
		if err != nil {
			return false, false, false, "", "", freshBeadStatus(env, v.Bead, v.BeadStatus), false
		}
		bs := freshBeadStatus(env, v.Bead, v.BeadStatus)
		if pr.State == "MERGED" && (v.Head == "" || pr.HeadSHA == v.Head) {
			return true, true, true, fmt.Sprintf("merged %s at %s", v.PR, short(pr.HeadSHA)), "merged", bs, false
		}
		return false, true, true, "", prStateToken(pr), bs, false

	case JobVerbClose:
		if env.Show == nil || v.Bead == "" {
			return false, false, false, "", "", "", false
		}
		b, err := env.Show(v.Bead)
		if err != nil {
			return false, false, false, "", "", "", false
		}
		if b.Status == StatusClosed {
			return true, true, true, fmt.Sprintf("closed %s", b.ID), "", StatusClosed, false
		}
		return false, true, true, "", "", b.Status, false

	case JobVerbHold:
		if env.Show == nil || v.Bead == "" {
			return false, false, false, "", "", "", false
		}
		b, err := env.Show(v.Bead)
		if err != nil {
			return false, false, false, "", "", "", false
		}
		marker := "HOLD [job:" + jobTag + "]:"
		for _, raw := range slices.Backward(strings.Split(b.Notes, "\n")) {
			if strings.Contains(raw, marker) {
				return true, true, true, strings.TrimSpace(raw), "", b.Status, false
			}
		}
		return false, true, true, "", "", b.Status, false

	case JobVerbDispatch, JobVerbResume, JobVerbStop:
		if env.Show == nil || v.Bead == "" {
			return false, false, false, "", "", "", false
		}
		b, err := env.Show(v.Bead)
		if err != nil {
			return false, false, false, "", "", "", false
		}
		marker := spec.Request.Verb + " [job:" + jobTag + "]:"
		for _, raw := range slices.Backward(strings.Split(b.Notes, "\n")) {
			if strings.Contains(raw, marker) {
				return true, true, false, strings.TrimSpace(raw), "", b.Status, false
			}
		}
		return false, true, false, "", "", b.Status, false
	}
	return false, false, false, "", "", "", false
}

// CLI: quartermaster job submit|status|result|list|cancel

const jobUsage = `usage: quartermaster job submit [--key K] [--why W] [--by WHO] [--expect HASH] [--class N] [--text T] [--json] -- <VERB> [<target> ...]
       quartermaster job status <id> [--json]
       quartermaster job result <id> [--json]
       quartermaster job list [--bead B] [--state S] [--limit N] [--json]
       quartermaster job cancel <id> [--reason R] [--by WHO] [--json]`

func jobCmd(cfg Config, env JobEnv, args []string, w io.Writer) error {
	if len(args) == 0 {
		return errors.New(jobUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "submit":
		return jobSubmitCmd(rest, env.clock, w)
	case "status":
		return jobStatusCmd(rest, env.clock, w)
	case "result":
		return jobResultCmd(rest, env.clock, w)
	case "list":
		return jobListCmd(rest, env.clock, w)
	case "cancel":
		return jobCancelCmd(env, rest, w)
	default:
		return errors.New(jobUsage)
	}
}

func jobSubmitCmd(args []string, clock func() time.Time, w io.Writer) error {
	fs := flag.NewFlagSet("job submit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	keyFlag := fs.String("key", "", "idempotency key (e.g. app-20261123-0017)")
	whyFlag := fs.String("why", "", "one-line audit reason")
	textFlag := fs.String("text", "", "free-text answer for ANSWER on a needs_me question (verbatim, max 2000 chars)")
	expectFlag := fs.String("expect", "", "expected prompt or question hash for ANSWER")
	classFlag := fs.Int("class", -1, "explicit impact class floor (0-9)")
	byFlag := fs.String("by", "coordinator", "who decided: coordinator (default) or TJ")
	jsonFlag := fs.Bool("json", false, "emit JSON output")

	// Stop flag parsing at explicit "--" so request tokens (including prose with
	// dashes) stay completely untouched.
	var flagArgs, reqTokens []string
	if sep := slices.Index(args, "--"); sep >= 0 {
		flagArgs = args[:sep]
		reqTokens = args[sep+1:]
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		if extra := fs.Args(); len(extra) > 0 {
			reqTokens = append(extra, reqTokens...)
		}
	} else {
		pos, err := interspersed(fs, args)
		if err != nil {
			return err
		}
		reqTokens = pos
	}

	req, key, err := parseJobRequest(reqTokens, *keyFlag, *whyFlag, *textFlag, *expectFlag, *byFlag, *classFlag)
	if err != nil {
		return err
	}
	now := time.Now()
	if clock != nil {
		now = clock()
	}
	view, err := submitJob(req, key, now)
	if err != nil {
		return err
	}
	if *jsonFlag {
		return writeJSONIndent(w, view)
	}
	fmt.Fprintf(w, "job=%s state=%s\n", view.Job, view.State)
	return nil
}

func jobStatusCmd(args []string, clock func() time.Time, w io.Writer) error {
	fs := flag.NewFlagSet("job status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: quartermaster job status <id> [--json]")
	}
	spec, events, err := loadJob(pos[0])
	if err != nil {
		return err
	}
	now := time.Now()
	if clock != nil {
		now = clock()
	}
	v := reconstructJob(spec, events, now.UTC())
	if *jsonFlag {
		return writeJSONIndent(w, v)
	}
	fmt.Fprintf(w, "job=%s\n", v.Job)
	fmt.Fprintf(w, "key=%s\n", v.Key)
	fmt.Fprintf(w, "state=%s\n", v.State)
	if v.Canceled {
		fmt.Fprintln(w, "canceled=true")
	}
	fmt.Fprintf(w, "verb=%s\n", v.Verb)
	if v.Target != "" {
		fmt.Fprintf(w, "target=%s\n", v.Target)
	}
	if v.Bead != "" {
		fmt.Fprintf(w, "bead=%s\n", v.Bead)
	}
	if v.BeadStatus != "" {
		fmt.Fprintf(w, "bead_status=%s\n", v.BeadStatus)
	}
	if v.Age != "" {
		fmt.Fprintf(w, "age=%s\n", v.Age)
	}
	if v.State == JobStateParked {
		if v.ParkedAge != "" {
			fmt.Fprintf(w, "parked_age=%s\n", v.ParkedAge)
		}
		if v.ParkHash != "" {
			fmt.Fprintf(w, "park_hash=%s\n", v.ParkHash)
		}
	}
	if v.Why != "" {
		fmt.Fprintf(w, "why=%s\n", v.Why)
	}
	if v.Summary != "" {
		fmt.Fprintf(w, "summary=%s\n", oneLine(v.Summary))
	}
	return nil
}

func jobResultCmd(args []string, clock func() time.Time, w io.Writer) error {
	fs := flag.NewFlagSet("job result", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: quartermaster job result <id> [--json]")
	}
	spec, events, err := loadJob(pos[0])
	if err != nil {
		return err
	}
	now := time.Now()
	if clock != nil {
		now = clock()
	}
	v := reconstructJob(spec, events, now.UTC())
	if !v.Terminal {
		return fmt.Errorf("%w: job %s is %s", errJobNotTerminal, short(v.Job), v.State)
	}
	if *jsonFlag {
		return writeJSONIndent(w, struct {
			Job        string     `json:"job"`
			Key        string     `json:"key"`
			State      string     `json:"state"`
			Canceled   bool       `json:"canceled,omitempty"`
			QMStatus   QMStatus   `json:"qm_status"`
			AuditNote  string     `json:"audit_note,omitempty"`
			Summary    string     `json:"summary,omitempty"`
			Why        string     `json:"why,omitempty"`
			FinishedAt *time.Time `json:"finished_at,omitempty"`
		}{
			Job:        v.Job,
			Key:        v.Key,
			State:      v.State,
			Canceled:   v.Canceled,
			QMStatus:   *v.QMStatus,
			AuditNote:  v.AuditNote,
			Summary:    v.Summary,
			Why:        v.Why,
			FinishedAt: v.FinishedAt,
		})
	}
	headline := v.Summary
	if headline == "" {
		headline = v.Why
	}
	if headline == "" {
		headline = v.State
	}
	fmt.Fprintf(w, "[req:%s] %s\n", v.Key, oneLine(headline))
	if v.AuditNote != "" {
		fmt.Fprintf(w, "audit_note: %s\n", v.AuditNote)
	}
	fmt.Fprintln(w)
	fmt.Fprint(w, formatQMStatus(*v.QMStatus))
	return nil
}

func jobListCmd(args []string, clock func() time.Time, w io.Writer) error {
	fs := flag.NewFlagSet("job list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	beadFlag := fs.String("bead", "", "filter to jobs for bead B")
	stateFlag := fs.String("state", "", "filter to comma-separated states (queued,running,parked,done,blocked,needs_tj)")
	limitFlag := fs.Int("limit", 0, "maximum jobs to print (0 = all)")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New("usage: quartermaster job list [--bead B] [--state S] [--limit N] [--json]")
	}
	if *limitFlag < 0 {
		return errors.New("--limit must not be negative")
	}
	wantStates := map[string]bool{}
	if strings.TrimSpace(*stateFlag) != "" {
		for _, s := range strings.Split(*stateFlag, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			switch s {
			case JobStateQueued, JobStateRunning, JobStateParked, JobStateDone, JobStateBlocked, JobStateNeedsTJ:
				wantStates[s] = true
			default:
				return fmt.Errorf("unknown --state %q (want queued, running, parked, done, blocked, or needs_tj)", s)
			}
		}
	}

	specs, err := listJobSpecs()
	if err != nil {
		return err
	}
	now := time.Now()
	if clock != nil {
		now = clock()
	}
	now = now.UTC()

	out := make([]JobView, 0)
	for _, spec := range specs {
		events, err := readJobEvents(spec.ID)
		if err != nil {
			continue
		}
		v := reconstructJob(spec, events, now)
		if *beadFlag != "" && v.Bead != *beadFlag {
			continue
		}
		if len(wantStates) > 0 && !wantStates[v.State] {
			continue
		}
		out = append(out, v)
		if *limitFlag > 0 && len(out) >= *limitFlag {
			break
		}
	}
	if *jsonFlag {
		return writeJSONIndent(w, out)
	}
	if len(out) == 0 {
		fmt.Fprintln(w, "(none)")
		return nil
	}
	for _, v := range out {
		stateLabel := v.State
		if v.Canceled {
			stateLabel = "blocked(canceled)"
		}
		ageStr := v.Age
		if v.State == JobStateParked && v.ParkedAge != "" {
			ageStr = "parked " + v.ParkedAge
		}
		target := v.Target
		if target == "" {
			target = "-"
		}
		line := fmt.Sprintf("%s  %-17s  %-11s  %-8s  %s  key=%s", short(v.Job), stateLabel, ageStr, v.Verb, target, v.Key)
		if detail := v.Why; detail != "" {
			line += "  — " + shortTitle(oneLine(detail), 80)
		} else if detail := v.Summary; detail != "" {
			line += "  — " + shortTitle(oneLine(detail), 80)
		}
		fmt.Fprintln(w, line)
	}
	return nil
}

func jobCancelCmd(env JobEnv, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("job cancel", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	reasonFlag := fs.String("reason", "", "cancellation reason")
	byFlag := fs.String("by", "TJ", "who canceled the job")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: quartermaster job cancel <id> [--reason R] [--by WHO] [--json]")
	}
	v, err := cancelJob(env, pos[0], *reasonFlag, *byFlag)
	if err != nil {
		return err
	}
	if *jsonFlag {
		return writeJSONIndent(w, v)
	}
	fmt.Fprintf(w, "job=%s state=%s canceled=true\n", v.Job, v.State)
	return nil
}

func writeJSONIndent(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
