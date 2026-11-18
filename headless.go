package main

// The headless coordinator is an opt-in event consumer, not a second task
// board. Beads owns outcomes. Its model can propose follow-up or blocking; only
// this deterministic reconciler can admit a fixed, fenced worker notification.
import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type headlessEnrollment struct {
	Bead   string    `json:"bead"`
	Scope  string    `json:"scope"`
	Worker string    `json:"worker"`
	PR     string    `json:"pr"`
	Head   string    `json:"head"`
	Since  time.Time `json:"since"`
}
type headlessControl struct {
	Mode     string                        `json:"mode"` // background, paused, foreground
	Epoch    string                        `json:"epoch"`
	Enrolled map[string]headlessEnrollment `json:"enrolled"`
}
type headlessEvent struct {
	ID        string    `json:"id"`
	Bead      string    `json:"bead"`
	Kind      EventKind `json:"kind"`
	At        time.Time `json:"at"`
	State     string    `json:"state"`            // received, delivered, processing, action-requested, verified-handled, explicitly-blocked
	Reason    string    `json:"reason,omitempty"` // fixed codes, never arbitrary model/tool text
	Attempt   int       `json:"attempt"`
	Next      time.Time `json:"next,omitempty"`
	Delivered time.Time `json:"delivered,omitempty"`
	Processed time.Time `json:"processed,omitempty"`
	Requested time.Time `json:"requested,omitempty"`
	Request   string    `json:"request,omitempty"`
	Receipt   string    `json:"receipt,omitempty"`
	Episode   int       `json:"blocker_episode,omitempty"`
}
type headlessJournal struct {
	LastLoop      time.Time                 `json:"last_loop"`
	LastDelivery  time.Time                 `json:"last_delivery"`
	LastProcessed time.Time                 `json:"last_processed"`
	Events        map[string]*headlessEvent `json:"events"`
}
type headlessReport struct {
	ID        string    `json:"id"`
	Bead      string    `json:"bead,omitempty"`
	Milestone string    `json:"milestone"`
	Reason    string    `json:"reason"`
	At        time.Time `json:"at"`
}

type HeadlessConfig struct {
	PollSeconds    int `toml:"poll_seconds"`
	StallSeconds   int `toml:"stall_seconds"`
	FollowSeconds  int `toml:"follow_seconds"`
	ModelSeconds   int `toml:"model_seconds"`
	BackoffSeconds int `toml:"backoff_seconds"`
	MaxAttempts    int `toml:"max_attempts"`
}

func (c HeadlessConfig) bounded() HeadlessConfig {
	if c.PollSeconds == 0 {
		c.PollSeconds = 15
	}
	c.PollSeconds = min(300, max(1, c.PollSeconds))
	if c.StallSeconds == 0 {
		c.StallSeconds = 300
	}
	c.StallSeconds = min(86400, max(5, c.StallSeconds))
	if c.FollowSeconds == 0 {
		c.FollowSeconds = 1800
	}
	c.FollowSeconds = min(86400, max(5, c.FollowSeconds))
	if c.ModelSeconds == 0 {
		c.ModelSeconds = 120
	}
	c.ModelSeconds = min(600, max(1, c.ModelSeconds))
	if c.BackoffSeconds == 0 {
		c.BackoffSeconds = 60
	}
	c.BackoffSeconds = min(3600, max(1, c.BackoffSeconds))
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 3
	}
	c.MaxAttempts = min(5, max(1, c.MaxAttempts))
	return c
}
func headlessDir() string             { return filepath.Join(stateDir(), "headless") }
func headlessPath(name string) string { return filepath.Join(headlessDir(), name) }
func headlessHash(v any) string       { b, _ := json.Marshal(v); return fmt.Sprintf("%x", sha256.Sum256(b)) }
func headlessScope(b Bead, repo string) string {
	return headlessHash([]string{b.ID, b.Title, b.Description, b.Assignee, repo, beadHost(b)})
}
func headlessLock(name string, exclusive bool) (*os.File, error) {
	if err := rpcPrivateDir(headlessDir()); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(headlessPath(name), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	if err = syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("headless %s busy; wait for admitted work to settle", name)
	}
	return f, nil
}
func headlessReadControl() (headlessControl, error) {
	var c headlessControl
	err := rpcLoad(headlessPath("control.json"), &c)
	if err == nil && (c.Epoch == "" || c.Enrolled == nil || (c.Mode != "background" && c.Mode != "paused" && c.Mode != "foreground")) {
		err = errors.New("invalid headless ownership record")
	}
	return c, err
}

// Shared admission is held through prompt acceptance. Ownership changes take
// it exclusively, so an old epoch cannot enqueue work after takeover returns.
// No record means the historical worker path; a damaged record fails closed.
func headlessAdmission(id, epoch string) (*os.File, error) {
	gate, err := headlessLock("admission", false)
	if err != nil {
		return nil, err
	}
	c, err := headlessReadControl()
	if errors.Is(err, os.ErrNotExist) && epoch == "" {
		return gate, nil
	}
	if err == nil {
		_, enrolled := c.Enrolled[id]
		if enrolled && !((c.Mode == "background" && epoch == c.Epoch) || (c.Mode == "foreground" && epoch == "")) {
			err = errors.New("worker owned by headless coordinator; pause/takeover before foreground follow-up")
		}
		if !enrolled && epoch != "" {
			err = errors.New("worker not enrolled")
		}
	}
	if err != nil {
		gate.Close()
		return nil, err
	}
	return gate, nil
}
func headlessOwned(id string) bool {
	c, err := headlessReadControl()
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	_, ok := c.Enrolled[id]
	return ok && c.Mode != "foreground"
}
func headlessLoadJournal() (headlessJournal, error) {
	var j headlessJournal
	err := rpcLoad(headlessPath("journal.json"), &j)
	if errors.Is(err, os.ErrNotExist) {
		j.Events = map[string]*headlessEvent{}
		return j, nil
	}
	if err == nil && j.Events == nil {
		err = errors.New("invalid headless event journal")
	}
	return j, err
}
func (j *headlessJournal) save() error { return rpcSave(headlessPath("journal.json"), j) }
func headlessTerminal(e *headlessEvent) bool {
	return e.State == "verified-handled" || e.State == "explicitly-blocked"
}

// Duplicate/out-of-order merge notices for the same enrolled resource converge
// on one request. Summaries and timestamps never supply authorization or text.
func (j *headlessJournal) receive(c headlessControl, items []InboxItem) {
	for _, it := range items {
		en, ok := c.Enrolled[it.Bead]
		if !ok || it.At.Before(en.Since) || it.At.IsZero() {
			continue
		}
		switch it.Kind {
		case EventMerged, EventNeedsYou, "closed":
		default:
			continue
		}
		key := headlessHash([]string{en.Bead, en.PR, en.Head, string(it.Kind), en.Scope})
		if _, ok := j.Events[key]; !ok {
			j.Events[key] = &headlessEvent{ID: key, Bead: it.Bead, Kind: it.Kind, At: it.At, State: "received"}
		}
	}
}
func headlessPublish(e *headlessEvent, now time.Time) error {
	milestone := "blocked"
	if e.State == "verified-handled" {
		milestone = "completed"
	}
	if e.Reason == "needs_me" {
		milestone = "decision-needed"
	}
	return headlessReportOnce(headlessReport{ID: headlessHash([]string{e.Bead, milestone, e.Reason, fmt.Sprint(e.Episode)}), Bead: e.Bead, Milestone: milestone, Reason: e.Reason, At: now})
}
func headlessReportOnce(r headlessReport) error {
	dir := headlessPath("reports")
	if err := rpcPrivateDir(dir); err != nil {
		return err
	}
	path := filepath.Join(dir, r.ID+".json")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return rpcSave(path, r)
}

// The seams keep deterministic transition/failure tests independent of models,
// GitHub, Beads and any live workers.
type headlessDeps struct {
	show      func(string) (Bead, error)
	resource  func(headlessEnrollment, Bead) error
	status    func(string) (rpcStatus, error)
	authorize func(Bead) error
	propose   func(*headlessEvent, func() error) (string, error)
	send      func(string, rpcRequest) (rpcReply, error)
	receipt   func(string, string) (rpcReceipt, error)
	now       func() time.Time
}

func (j *headlessJournal) block(e *headlessEvent, reason string, now time.Time) error {
	if e.State != "explicitly-blocked" || e.Reason != reason {
		e.Episode++
	}
	e.State = "explicitly-blocked"
	e.Reason = reason
	if err := j.save(); err != nil {
		return err
	}
	return headlessPublish(e, now)
}
func (j *headlessJournal) retry(e *headlessEvent, reason string, c HeadlessConfig, now time.Time) error {
	e.Reason = reason
	if e.Attempt >= c.MaxAttempts {
		return j.block(e, reason, now)
	}
	e.State = "received"
	e.Next = now.Add(time.Duration(c.BackoffSeconds*(1<<min(e.Attempt, 5))) * time.Second)
	return j.save()
}
func headlessWorkerText(en headlessEnrollment) string {
	return fmt.Sprintf("[quartermaster: automated follow-through, not the user, approves nothing] Fresh read confirms the enrolled PR %s at %s is merged. Inspect your bead, saved work and any uncertain action receipts first. Continue only the already-authorized remaining scope, rechecking autonomy and exact resource state before each action. Never replay an action with unknown outcome. This is not approval, an answer to a blocker, or permission to install/deploy. Record verified outcomes or the exact blocker in Beads. Delivery and ending a turn are not completion.", en.PR, en.Head)
}
func (j *headlessJournal) step(c headlessControl, hc HeadlessConfig, e *headlessEvent, d headlessDeps) error {
	now := d.now()
	en, ok := c.Enrolled[e.Bead]
	if !ok {
		return errors.New("event no longer enrolled")
	}
	// Reports survive a crash between saving a terminal outcome and publishing.
	if e.State == "verified-handled" {
		if err := headlessPublish(e, now); err != nil {
			return err
		}
		return headlessArchive(en, e)
	}
	if c.Mode != "background" || now.Before(e.Next) {
		return nil
	}
	b, err := d.show(e.Bead)
	if err != nil {
		if e.State == "explicitly-blocked" {
			return headlessPublish(e, now)
		}
		e.Attempt++
		return j.retry(e, "task-unavailable", hc, now)
	}
	if b.Status == StatusClosed {
		// Task truth, not a model claim. A worker must use the existing close verifier.
		e.State = "verified-handled"
		e.Episode = 0
		e.Reason = "bead-closed"
		if err := j.save(); err != nil {
			return err
		}
		return headlessPublish(e, now)
	}
	if b.Status == StatusNeedsMe || b.Status == StatusBlocked {
		return j.block(e, b.Status, now)
	}
	if b.Status != StatusInProgress {
		return j.block(e, "task-not-active", now)
	}
	if e.State == "explicitly-blocked" && e.Request == "" {
		// Only a changed task blocker or newly passing autonomy is evidence to
		// requeue. Provider failures, changed scope and model refusals require
		// explicit foreground investigation, never periodic model retries.
		switch e.Reason {
		case StatusNeedsMe, StatusBlocked:
			e.State = "received"
			e.Attempt = 0
		case "authorization-required":
			if err := d.authorize(b); err != nil {
				return headlessPublish(e, now)
			}
			e.State = "received"
			e.Attempt = 0
		default:
			return headlessPublish(e, now)
		}
	}
	if e.Kind != EventMerged {
		return j.block(e, "event-state-changed", now)
	}
	if e.Request != "" {
		// Reconcile receipts, never infer success from an idle worker/model turn.
		r, err := d.receipt(e.Bead, e.Request)
		if err != nil {
			return j.block(e, "delivery-unknown", now)
		}
		if r.ID != e.Request || r.Hash != fmt.Sprintf("%x", sha256.Sum256([]byte(headlessWorkerText(en)))) {
			return j.block(e, "receipt-mismatch", now)
		}
		e.Receipt = r.State
		if r.State == "intent" {
			return j.block(e, "delivery-unknown", now)
		}
		if r.State != "accepted" {
			return j.block(e, "delivery-rejected", now)
		}
		if now.Sub(e.Requested) > time.Duration(hc.FollowSeconds)*time.Second {
			return j.block(e, "follow-through-overdue", now)
		}
		e.State = "action-requested"
		e.Reason = ""
		return j.save()
	}
	if err := d.resource(en, b); err != nil {
		return j.block(e, "scope-or-resource-changed", now)
	}
	st, err := d.status(e.Bead)
	if err != nil {
		e.Attempt++
		return j.retry(e, "worker-unavailable", hc, now)
	}
	if st.Phase == "parked" || st.Phase == "blocked" {
		return j.block(e, "worker-blocked", now)
	}
	if st.Phase != "idle" {
		return nil
	} // never restart or queue behind an active worker
	if err := d.authorize(b); err != nil {
		return j.block(e, "authorization-required", now)
	}
	e.Attempt++
	if err := j.save(); err != nil {
		return err
	}
	decision, err := d.propose(e, func() error {
		// A launch attempt is not delivery. The transport invokes this only
		// after pi has acknowledged its automated custom-message command.
		e.State = "delivered"
		e.Delivered = d.now()
		j.LastDelivery = e.Delivered
		if err := j.save(); err != nil {
			return err
		}
		e.State = "processing"
		return j.save()
	})
	now = d.now()
	if err != nil {
		return j.retry(e, "consumer-unavailable", hc, now)
	}
	e.Processed = now
	j.LastProcessed = now
	if err := j.save(); err != nil {
		return err
	}
	if decision != "follow_up" {
		return j.block(e, "consumer-blocked", now)
	}
	// Revalidate after the model turn. No model field can expand this action.
	gate, err := headlessAdmission(e.Bead, c.Epoch)
	if err != nil {
		return err
	}
	if gate != nil {
		defer gate.Close()
	}
	b, err = d.show(e.Bead)
	if err != nil || b.Status != StatusInProgress {
		return j.block(e, "task-state-changed", now)
	}
	if err = d.resource(en, b); err != nil {
		return j.block(e, "scope-or-resource-changed", now)
	}
	if err = d.authorize(b); err != nil {
		return j.block(e, "authorization-required", now)
	}
	st, err = d.status(e.Bead)
	if err != nil || st.Phase != "idle" {
		return j.block(e, "worker-state-changed", now)
	}
	e.Request = "follow-" + headlessHash([]string{en.Bead, en.PR, en.Head, en.Scope})
	e.Requested = now
	e.State = "action-requested"
	if err = j.save(); err != nil {
		return err
	} // durable intent BEFORE any downstream action
	r, err := d.send(e.Bead, rpcRequest{Op: "prompt", ID: e.Request, Text: headlessWorkerText(en), Owner: c.Epoch})
	if err != nil || r.Receipt == nil {
		return j.block(e, "delivery-unknown", now)
	}
	if r.Receipt.ID != e.Request || r.Receipt.Hash != fmt.Sprintf("%x", sha256.Sum256([]byte(headlessWorkerText(en)))) {
		return j.block(e, "receipt-mismatch", now)
	}
	e.Receipt = r.Receipt.State
	if e.Receipt != "accepted" {
		return j.block(e, "delivery-unknown", now)
	}
	return j.save()
}
func headlessEventOrder(j headlessJournal) []string {
	keys := make([]string, 0, len(j.Events))
	for k := range j.Events {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool {
		x, y := j.Events[keys[a]], j.Events[keys[b]]
		if x.At.Equal(y.At) {
			return keys[a] < keys[b]
		}
		return x.At.Before(y.At)
	})
	return keys
}

// Strict model output. No commands, arbitrary prompt text, reports, evidence or
// approvals are accepted from the consumer. Whitespace is the only tolerance.
func headlessDecision(text string) (string, error) {
	var v struct {
		Decision string `json:"decision"`
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	if strings.TrimSpace(text) != "{\"decision\":\"follow_up\"}" && strings.TrimSpace(text) != "{\"decision\":\"block\"}" {
		// Accept ordinary JSON spacing, but not trailing records.
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return "", errors.New("extra consumer output")
		}
	}
	if v.Decision != "follow_up" && v.Decision != "block" {
		return "", errors.New("invalid consumer decision")
	}
	return v.Decision, nil
}
