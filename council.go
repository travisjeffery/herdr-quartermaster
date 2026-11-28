package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"
)

// Multi-model planning council (`quartermaster council`).
//
// Runs N independent read-only planning roles in parallel as worktree-free
// sibling helper beads with zero cross-talk, then automatically fans in on the
// ticker once all members submit (or min_quorum submit by deadline) to a blind,
// fact-checking Chair role. When anonymize = true, member role names, model
// names, agent kinds, and helper bead IDs are withheld from all Chair inputs
// (presented in shuffled Plan A, Plan B, ... order); after Chair submission, a
// post-scoring Attribution Table is appended to synthesis.md, a summary note is
// recorded on the target bead, a council_done inbox event is emitted, and
// helper beads are automatically closed and resolved (or drained asynchronously
// via PendingResolveBeads when an agent is still mid-turn).

const (
	councilSchemaVersion = 1

	councilPhaseFanout       = "fanout"
	councilPhaseSynthesizing = "synthesizing"
	councilPhaseDone         = "done"
	councilPhaseFailed       = "failed"
	councilPhaseCanceled     = "canceled"

	councilRunPending   = "pending"
	councilRunRunning   = "running"
	councilRunSubmitted = "submitted"
	councilRunFailed    = "failed"
	councilRunTimedOut  = "timed_out"
	councilRunCanceled  = "canceled"

	councilLabelPrefix = "council:"
	councilRoleLabel   = "council-role:"
	councilRoleMember  = "member"
	councilRoleChair   = "chair"

	councilMaxSubmitBytes = 4 << 20 // 4 MiB safety cap for submitted markdown
	councilLockTimeout    = 5 * time.Second
)

var (
	errCouncil   = errors.New("council")
	councilLocks sync.Map // lock file path -> *sync.Mutex
)

// CouncilMemberRun tracks one independent panelist run inside a council.
type CouncilMemberRun struct {
	Role           string    `json:"role"`
	Label          string    `json:"label"`
	HelperBead     string    `json:"helper_bead,omitempty"`
	ArtifactPath   string    `json:"artifact_path,omitempty"`
	ChairInputPath string    `json:"chair_input_path,omitempty"`
	Status         string    `json:"status"`
	StartedAt      time.Time `json:"started_at,omitzero"`
	SubmittedAt    time.Time `json:"submitted_at,omitzero"`
	Error          string    `json:"error,omitempty"`
}

// CouncilChairRun tracks the blind fact-checking Chair synthesis run.
type CouncilChairRun struct {
	Role          string    `json:"role"`
	HelperBead    string    `json:"helper_bead,omitempty"`
	SynthesisPath string    `json:"synthesis_path,omitempty"`
	Status        string    `json:"status"`
	StartedAt     time.Time `json:"started_at,omitzero"`
	SubmittedAt   time.Time `json:"submitted_at,omitzero"`
	Error         string    `json:"error,omitempty"`
}

// CouncilState is the durable per-bead state stored atomically at
// <state>/councils/<bead>/state.json.
type CouncilState struct {
	Version             int                `json:"version"`
	Bead                string             `json:"bead"`
	Panel               string             `json:"panel,omitempty"`
	Phase               string             `json:"phase"`
	Anonymize           bool               `json:"anonymize"`
	AutoCleanup         bool               `json:"auto_cleanup"`
	MinQuorum           int                `json:"min_quorum"`
	TimeoutMinutes      int                `json:"timeout_minutes"`
	StartedAt           time.Time          `json:"started_at"`
	Deadline            time.Time          `json:"deadline"`
	ChairStartedAt      time.Time          `json:"chair_started_at,omitzero"`
	CompletedAt         time.Time          `json:"completed_at,omitzero"`
	UpdatedAt           time.Time          `json:"updated_at"`
	StartedBy           string             `json:"started_by,omitempty"`
	FailureReason       string             `json:"failure_reason,omitempty"`
	TargetNoted         bool               `json:"target_noted,omitempty"`
	DoneEventEmitted    bool               `json:"done_event_emitted,omitempty"`
	Members             []CouncilMemberRun `json:"members,omitempty"`
	Chair               CouncilChairRun    `json:"chair"`
	PendingResolveBeads []string           `json:"pending_resolve_beads,omitempty"`
}

func (s CouncilState) isActive() bool {
	return s.Phase == councilPhaseFanout || s.Phase == councilPhaseSynthesizing
}

func (s CouncilState) submittedCount() int {
	n := 0
	for _, m := range s.Members {
		if m.Status == councilRunSubmitted {
			n++
		}
	}
	return n
}

func isCouncilHelperBead(b Bead) bool {
	return b.HasLabel(councilRoleLabel+councilRoleMember) || b.HasLabel(councilRoleLabel+councilRoleChair)
}

func isCouncilHelperFor(b Bead, targetID string) bool {
	return targetID != "" && b.ID != "" && b.ID != targetID &&
		b.HasLabel(councilLabelPrefix+targetID) && isCouncilHelperBead(b)
}

func councilsDir() string { return filepath.Join(stateDir(), "councils") }

func councilDir(id string) (string, error) {
	if !beadName.MatchString(id) {
		return "", fmt.Errorf("%w: invalid bead id %q", errCouncil, id)
	}
	return filepath.Join(councilsDir(), id), nil
}

func councilStatePath(id string) (string, error) {
	dir, err := councilDir(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "state.json"), nil
}

func councilMemberArtifactPath(id, role string) (string, error) {
	dir, err := councilDir(id)
	if err != nil {
		return "", err
	}
	if !roleNameRE.MatchString(role) {
		return "", fmt.Errorf("%w: invalid member role %q", errCouncil, role)
	}
	return filepath.Join(dir, "members", role+".md"), nil
}

func councilChairInputPath(id, label string) (string, error) {
	dir, err := councilDir(id)
	if err != nil {
		return "", err
	}
	slug := councilLabelSlug(label)
	if slug == "" {
		return "", fmt.Errorf("%w: invalid plan label %q", errCouncil, label)
	}
	return filepath.Join(dir, "chair", slug+".md"), nil
}

func councilSynthesisPath(id string) (string, error) {
	dir, err := councilDir(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "synthesis.md"), nil
}

func councilPlanLabel(idx int) string {
	// Bijective base-26 uppercase letters: 0->"A", 25->"Z", 26->"AA", ...
	n := idx + 1
	var letters []byte
	for n > 0 {
		n--
		letters = append(letters, byte('A'+(n%26)))
		n /= 26
	}
	slices.Reverse(letters)
	return "Plan " + string(letters)
}

func councilLabelSlug(label string) string {
	s := strings.ToLower(strings.TrimSpace(label))
	s = strings.Join(strings.Fields(s), "-")
	if s == "" || strings.ContainsAny(s, `/\.`) {
		return ""
	}
	return s
}

func compareCouncilLabels(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

func cryptoShuffle(n int, swap func(i, j int)) {
	for i := n - 1; i > 0; i-- {
		v, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			continue
		}
		swap(i, int(v.Int64()))
	}
}

func councilProcessMutex(lockPath string) *sync.Mutex {
	v, _ := councilLocks.LoadOrStore(lockPath, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// lockCouncil acquires both an in-process per-path mutex and an exclusive
// cross-process flock with a bounded wait so parallel member submissions never
// race or fail spuriously while still avoiding indefinite deadlocks.
func lockCouncil(id string) (func(), error) {
	if !beadName.MatchString(id) {
		return nil, fmt.Errorf("%w: invalid bead id %q", errCouncil, id)
	}
	dir := filepath.Join(runDir(), "locks")
	if err := ensurePrivateDir(dir); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, "council-"+id+".lock")
	mu := councilProcessMutex(lockPath)
	mu.Lock()
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	deadline := time.Now().Add(councilLockTimeout)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
				mu.Unlock()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			mu.Unlock()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			mu.Unlock()
			return nil, fmt.Errorf("%w: council for %s is busy", errBusy, id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func loadCouncil(id string) (CouncilState, bool, error) {
	p, err := councilStatePath(id)
	if err != nil {
		return CouncilState{}, false, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return CouncilState{}, false, nil
	}
	if err != nil {
		return CouncilState{}, false, err
	}
	var st CouncilState
	if err := json.Unmarshal(b, &st); err != nil {
		return CouncilState{}, false, fmt.Errorf("%w: parse %s: %v", errCouncil, p, err)
	}
	if st.Version == 0 {
		st.Version = councilSchemaVersion
	}
	return st, true, nil
}

func saveCouncil(st CouncilState) error {
	p, err := councilStatePath(st.Bead)
	if err != nil {
		return err
	}
	if st.Version == 0 {
		st.Version = councilSchemaVersion
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicPrivate(p, append(b, '\n'))
}

func listCouncils() ([]CouncilState, error) {
	entries, err := os.ReadDir(councilsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []CouncilState
	for _, e := range entries {
		if !e.IsDir() || !beadName.MatchString(e.Name()) {
			continue
		}
		st, ok, err := loadCouncil(e.Name())
		if err == nil && ok {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bead < out[j].Bead })
	return out, nil
}

// councilContextLine returns a compact single-line summary for quartermaster
// context output, or "" when bead id has no recorded council.
func councilContextLine(id string) string {
	st, ok, err := loadCouncil(id)
	if err != nil || !ok || st.Phase == "" {
		return ""
	}
	s := fmt.Sprintf("%s %d/%d", st.Phase, st.submittedCount(), len(st.Members))
	if st.Panel != "" {
		s += " [" + st.Panel + "]"
	}
	if st.Phase == councilPhaseFanout {
		s += fmt.Sprintf(" (min %d, chair %s)", st.MinQuorum, st.Chair.Role)
	} else if st.Chair.Role != "" {
		if st.Chair.HelperBead != "" && st.Phase == councilPhaseSynthesizing {
			s += fmt.Sprintf(" (chair %s, bead %s)", st.Chair.Role, st.Chair.HelperBead)
		} else {
			s += fmt.Sprintf(" (chair %s)", st.Chair.Role)
		}
	}
	return s
}

// councilEnv injects external dependencies so fan-out, submission, ticker
// fan-in, anonymization, attribution, and auto-cleanup are fully testable
// without live bd, herdr, or worker processes.
type councilEnv struct {
	show           func(id string) (Bead, error)
	note           func(id, text string) error
	createBead     func(opts BeadCreateOpts) (Bead, error)
	closeBead      func(id, reason string) error
	resolveHelper  func(id string) (string, error)
	dispatchHelper func(id, role, by string) (string, error)
	enforce        func(b Bead, action, by string) (string, error)
	event          func(Event) error
	journal        func(journalRecord) error
	now            func() time.Time
	shuffle        func(n int, swap func(i, j int))
	logf           func(format string, args ...any)
}

func liveCouncilEnv(cfg Config, h Herdr, logf func(string, ...any)) councilEnv {
	hh := h.on(cfg.coordSocket())
	bd := Beads{}
	return councilEnv{
		show:       bd.Show,
		note:       bd.Note,
		createBead: bd.Create,
		closeBead: func(id, reason string) error {
			_, err := bd.run("close", id, "--reason", reason)
			return err
		},
		resolveHelper: func(id string) (string, error) {
			return resolve(cfg, hh, id, false)
		},
		dispatchHelper: func(id, role, by string) (string, error) {
			return dispatch(cfg, hh, id, DispatchOpts{Role: role, By: by})
		},
		enforce: func(b Bead, action, by string) (string, error) {
			return cfg.enforce(b, action, by)
		},
		event:   writeEvent,
		journal: appendJournal,
		now:     time.Now,
		shuffle: cryptoShuffle,
		logf:    logf,
	}
}

func (e councilEnv) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

func (e councilEnv) shuffler() func(n int, swap func(i, j int)) {
	if e.shuffle != nil {
		return e.shuffle
	}
	return cryptoShuffle
}

// CouncilStartOpts configures `quartermaster council start <bead>`.
type CouncilStartOpts struct {
	Panel       string
	Members     []string
	Chair       string
	MinQuorum   int
	Timeout     time.Duration
	Anonymize   *bool
	AutoCleanup *bool
	By          string
}

// CouncilStartResult describes the outcome of starting a planning council.
type CouncilStartResult struct {
	State    CouncilState `json:"state"`
	Warnings []string     `json:"warnings,omitempty"`
	Message  string       `json:"message"`
}

type resolvedCouncilParams struct {
	Panel       string
	Members     []string
	Chair       string
	Anonymize   bool
	AutoCleanup bool
	MinQuorum   int
	Timeout     time.Duration
}

func resolveCouncilStartParams(cfg Config, opts CouncilStartOpts) (resolvedCouncilParams, error) {
	var p resolvedCouncilParams
	panel := strings.TrimSpace(opts.Panel)

	if panel == "" && len(opts.Members) == 0 && strings.TrimSpace(opts.Chair) == "" {
		if _, ok := cfg.Councils["default"]; ok {
			panel = "default"
		} else if len(cfg.Councils) == 1 {
			for k := range cfg.Councils {
				panel = k
			}
		} else {
			return p, fmt.Errorf("%w: specify --panel <name> or both --members r1,r2,... and --chair <role>", errCouncil)
		}
	}

	if panel != "" {
		cc, ok := cfg.Councils[panel]
		if !ok {
			if len(cfg.Councils) == 0 {
				return p, fmt.Errorf("%w: unknown council panel %q (no [councils.*] configured)", errCouncil, panel)
			}
			names := make([]string, 0, len(cfg.Councils))
			for k := range cfg.Councils {
				names = append(names, k)
			}
			sort.Strings(names)
			return p, fmt.Errorf("%w: unknown council panel %q (configured: %s)", errCouncil, panel, strings.Join(names, ", "))
		}
		p.Panel = panel
		p.Members = slices.Clone(cc.Members)
		p.Chair = cc.Chair
		p.Anonymize = cc.anonymizeEnabled()
		p.AutoCleanup = cc.autoCleanupEnabled()
		p.MinQuorum = cc.effectiveMinQuorum()
		p.Timeout = cc.effectiveTimeout()
	} else {
		p.Anonymize = true
		p.AutoCleanup = true
		p.Timeout = defaultCouncilTimeoutMinutes * time.Minute
	}

	if len(opts.Members) > 0 {
		p.Members = slices.Clone(opts.Members)
		if opts.MinQuorum == 0 {
			p.MinQuorum = len(p.Members)
		}
	}
	if chair := strings.TrimSpace(opts.Chair); chair != "" {
		p.Chair = chair
	}
	if opts.Anonymize != nil {
		p.Anonymize = *opts.Anonymize
	}
	if opts.AutoCleanup != nil {
		p.AutoCleanup = *opts.AutoCleanup
	}
	if opts.MinQuorum != 0 {
		p.MinQuorum = opts.MinQuorum
	} else if p.MinQuorum == 0 {
		p.MinQuorum = len(p.Members)
	}
	if opts.Timeout != 0 {
		p.Timeout = opts.Timeout
	}

	if err := cfg.validateCouncilRoles("council", p.Members, p.Chair); err != nil {
		return p, fmt.Errorf("%w: %v", errCouncil, err)
	}
	if p.MinQuorum < 1 || p.MinQuorum > len(p.Members) {
		return p, fmt.Errorf("%w: min_quorum %d must be between 1 and %d", errCouncil, p.MinQuorum, len(p.Members))
	}
	if p.Timeout <= 0 {
		return p, fmt.Errorf("%w: timeout %v must be > 0", errCouncil, p.Timeout)
	}
	return p, nil
}

func councilRepoRef(cfg Config, b Bead) string {
	if rn, ok := cfg.repoName(b); ok {
		if rn != "" {
			return cfg.Repos[rn]
		}
		return cfg.Repo
	}
	return ""
}

func councilHelperLabels(cfg Config, target Bead, roleName, councilRole string) []string {
	labels := []string{
		roleLabel + roleName,
		councilLabelPrefix + target.ID,
		councilRoleLabel + councilRole,
	}
	if rn, ok := cfg.repoName(target); ok && rn != "" {
		labels = append(labels, repoLabel+rn)
	}
	return labels
}

func buildCouncilMemberDescription(target Bead, roleName, repoRef string, anonymize bool) string {
	var s strings.Builder
	fmt.Fprintf(&s, "Independent planning council pass for target bead `%s`: %s\n\n", target.ID, target.Title)
	fmt.Fprintf(&s, "- Target bead: `%s`\n", target.ID)
	fmt.Fprintf(&s, "- Member role: `%s`\n", roleName)
	if repoRef != "" {
		fmt.Fprintf(&s, "- Repository (read-only inspection only — never edit, checkout, commit, or dirty it): `%s`\n", repoRef)
	}
	if desc := strings.TrimSpace(target.Description); desc != "" {
		fmt.Fprintf(&s, "\n## Target Bead Description\n\n%s\n", desc)
	}
	s.WriteString("\n## Planning Council Rules\n\n")
	s.WriteString("- Inspect the target bead and repository strictly read-only. Do NOT edit files, create commits, open PRs, or modify the target bead.\n")
	s.WriteString("- Work completely independently with zero cross-talk: do NOT inspect other council helper beads, sibling plans, or council state files.\n")
	if anonymize {
		s.WriteString("- Blind evaluation is active: do NOT mention your role name, model name, provider, agent kind, or helper bead ID anywhere in your plan.\n")
	}
	s.WriteString("- Ground every file path, symbol, config key, and invariant in code you actually read in the repository.\n")
	s.WriteString("\n## Required Plan Structure\n\n")
	s.WriteString("Submit a thorough, concrete markdown plan covering all four sections:\n")
	s.WriteString("1. **Architecture & Trade-offs**: proposed design, key data flows/interfaces, alternatives considered, and why this design wins.\n")
	s.WriteString("2. **Hidden Risks / Failure Modes / Blind Spots**: concurrency/races, edge cases, failure recovery, migration/backwards-compat hazards, security/privacy traps.\n")
	s.WriteString("3. **Proposed File-Disjoint Bead DAG**: break the implementation into parallelizable or sequenced child beads where every bead defines explicit `## Boundaries` (owned files/packages) and default-FAIL checkable `## Acceptance` criteria.\n")
	s.WriteString("4. **Open Decisions / Unknowns**: unresolved ambiguities, assumptions needing confirmation, and recommended defaults.\n")
	s.WriteString("\n## How to Submit\n\n")
	s.WriteString("Submit your plan via stdin (no file-write tools required):\n\n")
	fmt.Fprintf(&s, "```sh\nquartermaster council submit %s --member %s --file - <<'EOF'\n<your markdown plan>\nEOF\n```\n", target.ID, roleName)
	s.WriteString("\nOnce submitted, stop cleanly; Quartermaster cleans up helper beads automatically when the council completes.\n")
	s.WriteString("\n## Acceptance\n\n")
	fmt.Fprintf(&s, "- Target bead `%s` and codebase inspected read-only with zero mutations or cross-talk\n", target.ID)
	fmt.Fprintf(&s, "- Structured plan covering (a) Architecture & Trade-offs, (b) Hidden Risks / Failure Modes / Blind Spots, (c) Proposed File-Disjoint Bead DAG (`## Boundaries` + default-FAIL `## Acceptance`), and (d) Open Decisions / Unknowns submitted via `quartermaster council submit %s --member %s --file -`\n", target.ID, roleName)
	return s.String()
}

// buildCouncilChairDescription builds the Chair helper bead's prompt.
// When st.Anonymize is true, it MUST NOT contain any member role name, model
// name, agent kind, or member helper bead ID.
func buildCouncilChairDescription(target Bead, st CouncilState, repoRef string, submitted []CouncilMemberRun) string {
	var s strings.Builder
	fmt.Fprintf(&s, "Planning council Chair synthesis for target bead `%s`: %s\n\n", target.ID, target.Title)
	fmt.Fprintf(&s, "- Target bead: `%s`\n", target.ID)
	fmt.Fprintf(&s, "- Submitted plans: %d of %d\n", len(submitted), len(st.Members))
	if repoRef != "" {
		fmt.Fprintf(&s, "- Repository (read-only fact-checking reference only — never edit, checkout, commit, or dirty it): `%s`\n", repoRef)
	}
	if desc := strings.TrimSpace(target.Description); desc != "" {
		fmt.Fprintf(&s, "\n## Target Bead Description\n\n%s\n", desc)
	}
	s.WriteString("\n## Candidate Plans to Evaluate\n\n")
	for _, m := range submitted {
		if st.Anonymize {
			fmt.Fprintf(&s, "- **%s**: `%s`\n", m.Label, m.ChairInputPath)
		} else {
			fmt.Fprintf(&s, "- **%s** (role: `%s`): `%s`\n", m.Label, m.Role, m.ChairInputPath)
		}
	}
	s.WriteString("\n## Chair Synthesis Mandate\n\n")
	if st.Anonymize {
		s.WriteString("- **Blind Evaluation**: candidate plans are anonymized (`Plan A`, `Plan B`, ...). Judge strictly on technical merit and verified codebase evidence; do not attempt to identify authors or inspect files outside the listed plan paths and repository.\n")
	}
	s.WriteString("- Keep all repository inspection strictly read-only (zero edits, commits, PRs, or target-bead mutations).\n\n")
	s.WriteString("Follow these 5 steps:\n")
	s.WriteString("1. **Independently Fact-Check Against the Live Repo**: verify every cited file path, symbol, function signature, config key, and behavioral claim against the live repository and explicitly drop hallucinations or unverified assertions.\n")
	s.WriteString("2. **Compare Consensus vs Genuine Divergences**: identify where candidate plans agree versus where they genuinely diverge, with concrete pros and cons for each approach.\n")
	s.WriteString("3. **Highlight Unique Catches**: call out real bugs, failure modes, edge cases, or security/concurrency risks caught by only a single plan (citing the plan label, e.g. `Plan B`).\n")
	s.WriteString("4. **Synthesize the Best Merged Canonical Plan + File-Disjoint Bead DAG**: produce the single best merged plan and an actionable file-disjoint child-bead DAG where every child bead includes explicit `## Boundaries` and default-FAIL checkable `## Acceptance` criteria, plus any remaining open decisions.\n")
	s.WriteString("5. **Submit Your Synthesis**: submit your markdown synthesis via stdin (no file-write tools required):\n\n")
	fmt.Fprintf(&s, "```sh\nquartermaster council submit %s --chair --file - <<'EOF'\n<your canonical markdown synthesis>\nEOF\n```\n", target.ID)
	s.WriteString("\nQuartermaster automatically appends a post-scoring Attribution Table after your submission, records a summary note on the target bead, and cleans up helper beads when configured.\n")
	s.WriteString("\n## Acceptance\n\n")
	s.WriteString("- Every candidate plan's code, file, and symbol claims fact-checked against the live repository with hallucinations dropped\n")
	s.WriteString("- Synthesis covers Consensus vs Divergences, Unique Catches, Canonical Merged Plan, and File-Disjoint Child-Bead DAG (`## Boundaries` + default-FAIL `## Acceptance`)\n")
	fmt.Fprintf(&s, "- Final synthesis submitted via `quartermaster council submit %s --chair --file -`\n", target.ID)
	return s.String()
}

func cleanCouncilArtifacts(id string) error {
	dir, err := councilDir(id)
	if err != nil {
		return err
	}
	for _, sub := range []string{"members", "chair", "synthesis.md"} {
		if err := os.RemoveAll(filepath.Join(dir, sub)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// startCouncil initiates a planning council for bead id: enforces Class 3
// autonomy (unless --by TJ), refuses duplicate active councils, spawns isolated
// worktree-free sibling helper beads for every member, and persists durable
// state under <state>/councils/<bead>/state.json.
func startCouncil(cfg Config, env councilEnv, id string, opts CouncilStartOpts) (CouncilStartResult, error) {
	if !beadName.MatchString(id) {
		return CouncilStartResult{}, fmt.Errorf("%w: invalid bead id %q", errCouncil, id)
	}
	params, err := resolveCouncilStartParams(cfg, opts)
	if err != nil {
		return CouncilStartResult{}, err
	}

	unlock, err := lockCouncil(id)
	if err != nil {
		return CouncilStartResult{}, err
	}
	defer unlock()

	if env.show == nil || env.createBead == nil {
		return CouncilStartResult{}, fmt.Errorf("%w: bead operations unavailable", errCouncil)
	}
	target, err := env.show(id)
	if err != nil {
		return CouncilStartResult{}, err
	}
	if target.Status == StatusClosed {
		return CouncilStartResult{}, fmt.Errorf("%w: %s is closed", errCouncil, id)
	}

	prev, exists, err := loadCouncil(id)
	if err != nil {
		return CouncilStartResult{}, err
	}
	if exists && prev.isActive() {
		return CouncilStartResult{}, fmt.Errorf("%w: council already active for %s (phase %s, started %s)", errCouncil, id, prev.Phase, prev.StartedAt.Format(time.RFC3339))
	}

	by := strings.TrimSpace(opts.By)
	if by == "" {
		by = "coordinator"
	}

	var autoPrefix string
	if env.enforce != nil && by != "TJ" {
		prefix, eerr := env.enforce(target, "dispatch", by)
		if eerr != nil {
			return CouncilStartResult{}, eerr
		}
		autoPrefix = prefix
	}

	if err := cleanCouncilArtifacts(id); err != nil {
		return CouncilStartResult{}, err
	}

	now := env.clock()
	labels := make([]string, len(params.Members))
	for i := range params.Members {
		labels[i] = councilPlanLabel(i)
	}
	if params.Anonymize {
		env.shuffler()(len(labels), func(i, j int) {
			labels[i], labels[j] = labels[j], labels[i]
		})
	}

	timeoutMins := max(1, int(math.Ceil(params.Timeout.Minutes())))
	st := CouncilState{
		Version:        councilSchemaVersion,
		Bead:           id,
		Panel:          params.Panel,
		Phase:          councilPhaseFanout,
		Anonymize:      params.Anonymize,
		AutoCleanup:    params.AutoCleanup,
		MinQuorum:      params.MinQuorum,
		TimeoutMinutes: timeoutMins,
		StartedAt:      now,
		Deadline:       now.Add(params.Timeout),
		UpdatedAt:      now,
		StartedBy:      by,
		Members:        make([]CouncilMemberRun, len(params.Members)),
		Chair: CouncilChairRun{
			Role:   params.Chair,
			Status: councilRunPending,
		},
	}
	if exists && len(prev.PendingResolveBeads) > 0 {
		st.PendingResolveBeads = slices.Clone(prev.PendingResolveBeads)
	}
	for i, roleName := range params.Members {
		st.Members[i] = CouncilMemberRun{
			Role:   roleName,
			Label:  labels[i],
			Status: councilRunPending,
		}
	}

	repoRef := councilRepoRef(cfg, target)
	var warnings []string
	startedCount := 0
	var firstErr error
	for i := range st.Members {
		m := &st.Members[i]
		// Never parent helper beads under the target bead; share target.Parent
		// so helper beads never block target bead lifecycle transitions.
		created, cerr := env.createBead(BeadCreateOpts{
			Title:       fmt.Sprintf("Council plan (%s) %s (%s)", m.Role, shortTitle(target.Title, 38), target.ID),
			Type:        "task",
			Priority:    target.Priority,
			Parent:      target.Parent,
			Labels:      councilHelperLabels(cfg, target, m.Role, councilRoleMember),
			Description: buildCouncilMemberDescription(target, m.Role, repoRef, st.Anonymize),
		})
		if cerr != nil {
			m.Status = councilRunFailed
			m.Error = cerr.Error()
			warnings = append(warnings, fmt.Sprintf("member %s create failed: %v", m.Role, cerr))
			if firstErr == nil {
				firstErr = fmt.Errorf("create council member bead (%s): %w", m.Role, cerr)
			}
			continue
		}
		m.HelperBead = created.ID
		m.StartedAt = now
		if env.dispatchHelper != nil {
			// Autonomy Class 3 was already enforced once for this council start.
			if _, derr := env.dispatchHelper(created.ID, m.Role, "TJ"); derr != nil {
				m.Status = councilRunFailed
				m.Error = derr.Error()
				warnings = append(warnings, fmt.Sprintf("member %s (%s) dispatch failed: %v", m.Role, created.ID, derr))
				if firstErr == nil {
					firstErr = fmt.Errorf("dispatch council member %s (%s): %w", created.ID, m.Role, derr)
				}
				continue
			}
		}
		m.Status = councilRunRunning
		startedCount++
	}

	if startedCount < st.MinQuorum {
		st.Phase = councilPhaseFailed
		st.CompletedAt = now
		st.UpdatedAt = now
		st.FailureReason = fmt.Sprintf("only %d/%d members started (min_quorum=%d)", startedCount, len(st.Members), st.MinQuorum)
		if st.AutoCleanup {
			cleanupBy := by
			if autoPrefix != "" {
				cleanupBy = "TJ"
			}
			cleanupCouncilHelpers(env, &st, "Council start failed for "+id+": "+st.FailureReason, cleanupBy, now)
		}
		_ = saveCouncil(st)
		if firstErr != nil {
			return CouncilStartResult{}, fmt.Errorf("%w: %s: %v", errCouncil, st.FailureReason, firstErr)
		}
		return CouncilStartResult{}, fmt.Errorf("%w: %s", errCouncil, st.FailureReason)
	}

	st.UpdatedAt = now
	if err := saveCouncil(st); err != nil {
		return CouncilStartResult{}, err
	}

	panelTag := st.Panel
	if panelTag == "" {
		panelTag = "ad-hoc"
	}
	noteText := fmt.Sprintf("COUNCIL START: panel=%s members=%s chair=%s quorum=%d/%d anonymize=%t deadline=%s",
		panelTag, strings.Join(params.Members, ","), st.Chair.Role, st.MinQuorum, len(st.Members), st.Anonymize, st.Deadline.UTC().Format(time.RFC3339))
	if env.note != nil {
		_ = env.note(id, noteText)
		if autoPrefix != "" {
			_ = env.note(id, fmt.Sprintf("%s started planning council (%s, %d members, chair %s) — quartermaster council start", autoPrefix, panelTag, len(st.Members), st.Chair.Role))
		}
	}
	if env.journal != nil {
		_ = env.journal(journalRecord{
			At:     now,
			Kind:   journalCouncil,
			Bead:   id,
			Class:  classHousekeeping,
			Action: "start",
			Pass:   true,
			Text:   noteText,
		})
		if autoPrefix != "" {
			_ = env.journal(journalRecord{
				At:     now,
				Kind:   journalAuto,
				Bead:   id,
				Class:  classHousekeeping,
				Action: "council_start",
				Pass:   true,
				Text:   noteText,
			})
		}
	}

	msg := fmt.Sprintf("started council for %s (panel %s, %d/%d members running, min_quorum %d, chair %s, deadline %s)",
		id, panelTag, startedCount, len(st.Members), st.MinQuorum, st.Chair.Role, st.Deadline.UTC().Format(time.RFC3339))
	return CouncilStartResult{
		State:    st,
		Warnings: warnings,
		Message:  msg,
	}, nil
}

// CouncilSubmitOpts configures `quartermaster council submit <bead>`.
type CouncilSubmitOpts struct {
	Member  string
	Chair   bool
	Content string
	By      string
}

// CouncilSubmitResult describes the outcome of submitting a member plan or chair synthesis.
type CouncilSubmitResult struct {
	State          CouncilState `json:"state"`
	ArtifactPath   string       `json:"artifact_path"`
	HelpersClosed  []string     `json:"helpers_closed,omitempty"`
	HelpersResolve []string     `json:"helpers_resolved,omitempty"`
	Cleanup        string       `json:"cleanup,omitempty"`
	Message        string       `json:"message"`
}

func extractSynthesisSummary(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || l == "---" || l == "***" || strings.HasPrefix(l, "```") {
			continue
		}
		l = strings.TrimLeft(l, "#")
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		return shortTitle(l, 140)
	}
	return "synthesis submitted"
}

func buildCouncilAttributionTable(cfg Config, st CouncilState) string {
	sorted := slices.Clone(st.Members)
	sort.Slice(sorted, func(i, j int) bool {
		return compareCouncilLabels(sorted[i].Label, sorted[j].Label) < 0
	})
	var s strings.Builder
	s.WriteString("\n\n---\n\n## Attribution Table\n\n")
	if st.Anonymize {
		s.WriteString("_Appended automatically after blind chair synthesis._\n\n")
	} else {
		s.WriteString("_Appended automatically after chair synthesis._\n\n")
	}
	s.WriteString("| Label | Role | Model | Status | Helper Bead | Artifact |\n")
	s.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	for _, m := range sorted {
		model := "-"
		if eff, err := cfg.resolveRole(m.Role); err == nil && eff.Model != "" {
			model = "`" + eff.Model + "`"
		}
		helper := "-"
		if m.HelperBead != "" {
			helper = "`" + m.HelperBead + "`"
		}
		artifact := "-"
		if m.ArtifactPath != "" {
			artifact = "`" + m.ArtifactPath + "`"
		}
		fmt.Fprintf(&s, "| %s | `%s` | %s | %s | %s | %s |\n", m.Label, m.Role, model, m.Status, helper, artifact)
	}
	chairModel := "-"
	if eff, err := cfg.resolveRole(st.Chair.Role); err == nil && eff.Model != "" {
		chairModel = eff.Model
	}
	panelTag := st.Panel
	if panelTag == "" {
		panelTag = "ad-hoc"
	}
	fmt.Fprintf(&s, "\nChair: `%s` (model: `%s`, helper bead: `%s`, panel: `%s`, anonymized: `%t`)\n",
		st.Chair.Role, chairModel, orDash(st.Chair.HelperBead), panelTag, st.Anonymize)
	return s.String()
}

// submitCouncilPlan atomically records a member plan or Chair synthesis.
// Chair submission appends the post-scoring Attribution Table, records the
// summary note on the target bead, emits council_done, and runs auto_cleanup
// when enabled (queueing PendingResolveBeads if an agent is still mid-turn).
func submitCouncilPlan(cfg Config, env councilEnv, id string, opts CouncilSubmitOpts) (CouncilSubmitResult, error) {
	if !beadName.MatchString(id) {
		return CouncilSubmitResult{}, fmt.Errorf("%w: invalid bead id %q", errCouncil, id)
	}
	memberRole := strings.TrimSpace(opts.Member)
	if (memberRole == "") == !opts.Chair {
		return CouncilSubmitResult{}, fmt.Errorf("%w: specify exactly one of --member <role> or --chair", errCouncil)
	}
	content := strings.TrimSpace(opts.Content)
	if content == "" {
		return CouncilSubmitResult{}, fmt.Errorf("%w: submitted plan content must not be empty", errCouncil)
	}

	unlock, err := lockCouncil(id)
	if err != nil {
		return CouncilSubmitResult{}, err
	}
	defer unlock()

	st, ok, err := loadCouncil(id)
	if err != nil {
		return CouncilSubmitResult{}, err
	}
	if !ok {
		return CouncilSubmitResult{}, fmt.Errorf("%w: no council found for %s", errCouncil, id)
	}

	now := env.clock()
	if !opts.Chair {
		if st.Phase != councilPhaseFanout {
			return CouncilSubmitResult{}, fmt.Errorf("%w: cannot submit member plan for %s in phase %s (must be %s)", errCouncil, id, st.Phase, councilPhaseFanout)
		}
		idx := -1
		for i := range st.Members {
			if st.Members[i].Role == memberRole {
				idx = i
				break
			}
		}
		if idx < 0 {
			return CouncilSubmitResult{}, fmt.Errorf("%w: role %q is not a member of council %s", errCouncil, memberRole, id)
		}
		artPath, err := councilMemberArtifactPath(id, memberRole)
		if err != nil {
			return CouncilSubmitResult{}, err
		}
		if err := ensurePrivateDir(councilsDir()); err != nil {
			return CouncilSubmitResult{}, err
		}
		if err := writeAtomicPrivate(artPath, []byte(content+"\n")); err != nil {
			return CouncilSubmitResult{}, err
		}
		st.Members[idx].ArtifactPath = artPath
		st.Members[idx].Status = councilRunSubmitted
		st.Members[idx].SubmittedAt = now
		st.Members[idx].Error = ""
		st.UpdatedAt = now
		if err := saveCouncil(st); err != nil {
			return CouncilSubmitResult{}, err
		}
		if env.journal != nil {
			_ = env.journal(journalRecord{
				At:     now,
				Kind:   journalCouncil,
				Bead:   id,
				Class:  0,
				Action: "member_submit",
				Pass:   true,
				Text:   fmt.Sprintf("member=%s submitted (%d/%d)", memberRole, st.submittedCount(), len(st.Members)),
			})
		}
		msg := fmt.Sprintf("recorded council plan for %s member %s (%d/%d submitted): %s",
			id, memberRole, st.submittedCount(), len(st.Members), artPath)
		return CouncilSubmitResult{
			State:        st,
			ArtifactPath: artPath,
			Message:      msg,
		}, nil
	}

	// Chair submission.
	if st.Phase != councilPhaseSynthesizing {
		return CouncilSubmitResult{}, fmt.Errorf("%w: cannot submit chair synthesis for %s in phase %s (must be %s)", errCouncil, id, st.Phase, councilPhaseSynthesizing)
	}
	synthPath, err := councilSynthesisPath(id)
	if err != nil {
		return CouncilSubmitResult{}, err
	}
	if err := ensurePrivateDir(councilsDir()); err != nil {
		return CouncilSubmitResult{}, err
	}
	fullSynthesis := content + buildCouncilAttributionTable(cfg, st)
	if err := writeAtomicPrivate(synthPath, []byte(fullSynthesis)); err != nil {
		return CouncilSubmitResult{}, err
	}

	st.Chair.SynthesisPath = synthPath
	st.Chair.Status = councilRunSubmitted
	st.Chair.SubmittedAt = now
	st.Chair.Error = ""
	st.Phase = councilPhaseDone
	st.CompletedAt = now
	st.UpdatedAt = now

	summary := extractSynthesisSummary(content)
	by := strings.TrimSpace(opts.By)
	if by == "" {
		by = "chair"
	}
	closed, resolved, cleanupMsg := finalizeCouncilDone(env, &st, summary, by, now)
	if err := saveCouncil(st); err != nil {
		return CouncilSubmitResult{}, err
	}
	if env.journal != nil {
		_ = env.journal(journalRecord{
			At:     now,
			Kind:   journalCouncil,
			Bead:   id,
			Class:  0,
			Action: "chair_submit",
			Pass:   true,
			Text:   fmt.Sprintf("chair=%s synthesis=%s — %s", st.Chair.Role, synthPath, summary),
		})
	}
	msg := fmt.Sprintf("recorded chair synthesis for %s: %s", id, synthPath)
	if cleanupMsg != "" {
		msg += "; " + cleanupMsg
	}
	return CouncilSubmitResult{
		State:          st,
		ArtifactPath:   synthPath,
		HelpersClosed:  closed,
		HelpersResolve: resolved,
		Cleanup:        cleanupMsg,
		Message:        msg,
	}, nil
}

func finalizeCouncilDone(env councilEnv, st *CouncilState, summary, by string, now time.Time) ([]string, []string, string) {
	if summary == "" {
		summary = "synthesis submitted"
	}
	panelTag := st.Panel
	if panelTag == "" {
		panelTag = "ad-hoc"
	}
	submitted := st.submittedCount()
	if !st.TargetNoted && env.note != nil {
		noteText := fmt.Sprintf("COUNCIL DONE: panel=%s (%d/%d plans, chair=%s) synthesis=%s — %s",
			panelTag, submitted, len(st.Members), st.Chair.Role, st.Chair.SynthesisPath, summary)
		if err := env.note(st.Bead, noteText); err == nil {
			st.TargetNoted = true
		}
	}
	if !st.DoneEventEmitted && env.event != nil {
		ev := Event{
			Bead: st.Bead,
			Kind: EventCouncilDone,
			Summary: fmt.Sprintf("planning council done for %s (%d/%d plans, chair %s): %s (synthesis: %s)",
				st.Bead, submitted, len(st.Members), st.Chair.Role, summary, st.Chair.SynthesisPath),
			At:     now,
			Clears: &InboxCondition{Kind: "bead_closed", Bead: st.Bead, Key: "council_done"},
		}
		if err := env.event(ev); err == nil {
			st.DoneEventEmitted = true
		}
	}
	if !st.AutoCleanup {
		return nil, nil, ""
	}
	closeReason := fmt.Sprintf("Planning council completed for %s (synthesis: %s)", st.Bead, st.Chair.SynthesisPath)
	return cleanupCouncilHelpers(env, st, closeReason, by, now)
}

// cleanupCouncilHelpers closes and resolves all spawned council member + chair
// helper beads for st without ever touching st.Bead (or any non-helper bead).
// Any helper whose agent is still mid-turn ("still working") is queued into
// st.PendingResolveBeads for automatic drain on subsequent ticker passes.
func cleanupCouncilHelpers(env councilEnv, st *CouncilState, closeReason, by string, now time.Time) ([]string, []string, string) {
	var targets []string
	for _, m := range st.Members {
		if m.HelperBead != "" && m.HelperBead != st.Bead && !slices.Contains(targets, m.HelperBead) {
			targets = append(targets, m.HelperBead)
		}
	}
	if st.Chair.HelperBead != "" && st.Chair.HelperBead != st.Bead && !slices.Contains(targets, st.Chair.HelperBead) {
		targets = append(targets, st.Chair.HelperBead)
	}
	if len(targets) == 0 {
		return nil, nil, ""
	}

	skipEnforce := by == "TJ" || st.StartedBy == "TJ"
	var closedIDs, resolvedIDs, msgs []string
	for _, helperID := range targets {
		if helperID == "" || helperID == st.Bead || env.show == nil {
			continue
		}
		hb, err := env.show(helperID)
		if err != nil {
			msgs = append(msgs, fmt.Sprintf("cleanup lookup %s failed: %v", helperID, err))
			continue
		}
		if !isCouncilHelperFor(hb, st.Bead) {
			msgs = append(msgs, fmt.Sprintf("cleanup refused for %s (not a council helper for %s)", helperID, st.Bead))
			continue
		}

		closed := hb.Status == StatusClosed
		enforced := false
		if !closed {
			if env.closeBead == nil {
				continue
			}
			var prefix string
			if !skipEnforce && env.enforce != nil {
				p, eerr := env.enforce(hb, "close", by)
				if eerr != nil {
					msgs = append(msgs, fmt.Sprintf("cleanup skipped for %s: %v", helperID, eerr))
					continue
				}
				prefix = p
				enforced = true
			}
			if err := env.closeBead(helperID, closeReason); err != nil {
				msgs = append(msgs, fmt.Sprintf("close helper %s failed: %v", helperID, err))
				continue
			}
			closed = true
			closedIDs = append(closedIDs, helperID)
			msgs = append(msgs, "closed helper "+helperID)
			if prefix != "" && env.note != nil {
				_ = env.note(helperID, prefix+" close — "+closeReason)
			}
			if env.journal != nil {
				_ = env.journal(journalRecord{
					At:     now,
					Kind:   journalCouncil,
					Bead:   helperID,
					Class:  classHousekeeping,
					Action: "cleanup_close",
					Pass:   true,
					Text:   closeReason,
				})
				if prefix != "" {
					_ = env.journal(journalRecord{
						At:     now,
						Kind:   journalAuto,
						Bead:   helperID,
						Class:  classHousekeeping,
						Action: "close",
						Pass:   true,
						Text:   closeReason,
					})
				}
			}
		}

		if closed && env.resolveHelper != nil {
			if !skipEnforce && !enforced && env.enforce != nil {
				if _, eerr := env.enforce(hb, "resolve", by); eerr != nil {
					msgs = append(msgs, fmt.Sprintf("resolve skipped for %s: %v", helperID, eerr))
					continue
				}
			}
			rmsg, rerr := env.resolveHelper(helperID)
			if rerr == nil {
				resolvedIDs = append(resolvedIDs, helperID)
				if rmsg != "" {
					msgs = append(msgs, fmt.Sprintf("resolved %s (%s)", helperID, rmsg))
				} else {
					msgs = append(msgs, "resolved "+helperID)
				}
				if idx := slices.Index(st.PendingResolveBeads, helperID); idx >= 0 {
					st.PendingResolveBeads = slices.Delete(st.PendingResolveBeads, idx, idx+1)
				}
			} else if strings.Contains(rerr.Error(), "still working") {
				if !slices.Contains(st.PendingResolveBeads, helperID) {
					st.PendingResolveBeads = append(st.PendingResolveBeads, helperID)
				}
				msgs = append(msgs, fmt.Sprintf("resolve deferred for %s (%v)", helperID, rerr))
			} else {
				msgs = append(msgs, fmt.Sprintf("resolve %s failed: %v", helperID, rerr))
			}
		}
	}
	return closedIDs, resolvedIDs, strings.Join(msgs, "; ")
}

// CouncilCancelResult describes the outcome of canceling an active council.
type CouncilCancelResult struct {
	State          CouncilState `json:"state"`
	HelpersClosed  []string     `json:"helpers_closed,omitempty"`
	HelpersResolve []string     `json:"helpers_resolved,omitempty"`
	Cleanup        string       `json:"cleanup,omitempty"`
	Message        string       `json:"message"`
}

// cancelCouncil cancels an active council (fanout or synthesizing) and cleans
// up all spawned helper beads without modifying the target bead's status.
func cancelCouncil(env councilEnv, id, by string) (CouncilCancelResult, error) {
	if !beadName.MatchString(id) {
		return CouncilCancelResult{}, fmt.Errorf("%w: invalid bead id %q", errCouncil, id)
	}
	unlock, err := lockCouncil(id)
	if err != nil {
		return CouncilCancelResult{}, err
	}
	defer unlock()

	st, ok, err := loadCouncil(id)
	if err != nil {
		return CouncilCancelResult{}, err
	}
	if !ok {
		return CouncilCancelResult{}, fmt.Errorf("%w: no council found for %s", errCouncil, id)
	}
	if !st.isActive() {
		return CouncilCancelResult{}, fmt.Errorf("%w: council for %s is not active (phase %s)", errCouncil, id, st.Phase)
	}

	by = strings.TrimSpace(by)
	if by == "" {
		by = "coordinator"
	}

	var autoPrefix string
	if env.enforce != nil && by != "TJ" && env.show != nil {
		target, serr := env.show(id)
		if serr != nil {
			return CouncilCancelResult{}, serr
		}
		prefix, eerr := env.enforce(target, "close", by)
		if eerr != nil {
			return CouncilCancelResult{}, eerr
		}
		autoPrefix = prefix
	}

	now := env.clock()
	st.Phase = councilPhaseCanceled
	st.CompletedAt = now
	st.UpdatedAt = now
	for i := range st.Members {
		if st.Members[i].Status == councilRunPending || st.Members[i].Status == councilRunRunning {
			st.Members[i].Status = councilRunCanceled
		}
	}
	if st.Chair.Status == councilRunPending || st.Chair.Status == councilRunRunning {
		st.Chair.Status = councilRunCanceled
	}

	closeReason := fmt.Sprintf("Planning council canceled for %s", id)
	// On explicit cancel, pass "TJ" to helper cleanup when we already enforced
	// Class 3 above so cleanup never double-charges the budget.
	cleanupBy := by
	if autoPrefix != "" {
		cleanupBy = "TJ"
	}
	closed, resolved, cleanupMsg := cleanupCouncilHelpers(env, &st, closeReason, cleanupBy, now)
	if err := saveCouncil(st); err != nil {
		return CouncilCancelResult{}, err
	}
	if env.note != nil {
		_ = env.note(id, fmt.Sprintf("COUNCIL CANCELED: by=%s (%d/%d plans submitted)", by, st.submittedCount(), len(st.Members)))
		if autoPrefix != "" {
			_ = env.note(id, fmt.Sprintf("%s canceled planning council — quartermaster council cancel", autoPrefix))
		}
	}
	if env.journal != nil {
		_ = env.journal(journalRecord{
			At:     now,
			Kind:   journalCouncil,
			Bead:   id,
			Class:  classHousekeeping,
			Action: "cancel",
			Pass:   true,
			Text:   closeReason,
		})
	}
	msg := fmt.Sprintf("canceled council for %s", id)
	if cleanupMsg != "" {
		msg += "; " + cleanupMsg
	}
	return CouncilCancelResult{
		State:          st,
		HelpersClosed:  closed,
		HelpersResolve: resolved,
		Cleanup:        cleanupMsg,
		Message:        msg,
	}, nil
}

// assembleChairInputs writes the Chair's candidate plan files under
// <state>/councils/<bead>/chair/ sorted by anonymous label (Plan A, Plan B...).
// When st.Anonymize is true, filenames and headers contain ZERO role names,
// model names, agent kinds, or helper bead IDs.
func assembleChairInputs(cfg Config, st *CouncilState) ([]CouncilMemberRun, error) {
	if err := ensurePrivateDir(councilsDir()); err != nil {
		return nil, err
	}
	var submittedIdx []int
	for i := range st.Members {
		if st.Members[i].Status == councilRunSubmitted && st.Members[i].ArtifactPath != "" {
			submittedIdx = append(submittedIdx, i)
		}
	}
	sort.Slice(submittedIdx, func(a, b int) bool {
		return compareCouncilLabels(st.Members[submittedIdx[a]].Label, st.Members[submittedIdx[b]].Label) < 0
	})

	assembled := make([]CouncilMemberRun, 0, len(submittedIdx))
	for _, idx := range submittedIdx {
		m := &st.Members[idx]
		raw, err := os.ReadFile(m.ArtifactPath)
		if err != nil {
			return nil, fmt.Errorf("read member artifact %s: %w", m.ArtifactPath, err)
		}
		outPath, err := councilChairInputPath(st.Bead, m.Label)
		if err != nil {
			return nil, err
		}
		var body strings.Builder
		if st.Anonymize {
			fmt.Fprintf(&body, "# %s\n\n%s\n", m.Label, strings.TrimSpace(string(raw)))
		} else {
			header := fmt.Sprintf("# %s (role: %s", m.Label, m.Role)
			if eff, rerr := cfg.resolveRole(m.Role); rerr == nil && eff.Model != "" {
				header += ", model: " + eff.Model
			}
			header += ")"
			fmt.Fprintf(&body, "%s\n\n%s\n", header, strings.TrimSpace(string(raw)))
		}
		if err := writeAtomicPrivate(outPath, []byte(body.String())); err != nil {
			return nil, err
		}
		m.ChairInputPath = outPath
		assembled = append(assembled, *m)
	}
	return assembled, nil
}

// drainPendingCouncilResolves completes deferred role-dir / workspace cleanup
// for closed council helper beads whose agents were still mid-turn when the
// council completed or was canceled.
func drainPendingCouncilResolves(env councilEnv, now time.Time) []string {
	if env.resolveHelper == nil {
		return nil
	}
	councils, err := listCouncils()
	if err != nil {
		return nil
	}
	var lines []string
	for _, item := range councils {
		if len(item.PendingResolveBeads) == 0 {
			continue
		}
		unlock, err := lockCouncil(item.Bead)
		if err != nil {
			continue
		}
		st, ok, lerr := loadCouncil(item.Bead)
		if lerr != nil || !ok || len(st.PendingResolveBeads) == 0 {
			unlock()
			continue
		}
		var remaining []string
		changed := false
		for _, helperID := range st.PendingResolveBeads {
			if helperID == "" || helperID == st.Bead {
				changed = true
				continue
			}
			if env.show != nil {
				hb, serr := env.show(helperID)
				if serr != nil {
					if errors.Is(serr, errBeadNotFound) {
						changed = true
						continue
					}
					remaining = append(remaining, helperID)
					continue
				}
				if hb.Status != StatusClosed {
					remaining = append(remaining, helperID)
					continue
				}
				if !isCouncilHelperFor(hb, st.Bead) {
					changed = true
					continue
				}
			}
			rmsg, rerr := env.resolveHelper(helperID)
			if rerr == nil {
				changed = true
				lines = append(lines, fmt.Sprintf("resolved finished council helper %s for %s: %s", helperID, st.Bead, rmsg))
				continue
			}
			if strings.Contains(rerr.Error(), "still working") {
				remaining = append(remaining, helperID)
				continue
			}
			// Avoid looping indefinitely on permanent resolve errors; log and clear.
			changed = true
			if env.logf != nil {
				env.logf("council %s deferred resolve %s: %v", st.Bead, helperID, rerr)
			}
		}
		if changed {
			st.PendingResolveBeads = remaining
			st.UpdatedAt = now
			if serr := saveCouncil(st); serr != nil && env.logf != nil {
				env.logf("council %s save pending resolves: %v", st.Bead, serr)
			}
		}
		unlock()
	}
	return lines
}

// reconcileCouncilsPass advances all active councils on each tick:
//   - Drains deferred PendingResolveBeads once helper agents go idle.
//   - Completes any interrupted post-synthesis finalization idempotently.
//   - Fans in from fanout -> synthesizing once all members submit OR
//     (submitted >= min_quorum && now >= deadline) OR
//     (submitted >= min_quorum && no remaining active members).
//   - Marks fanout councils failed and emits council_failed when < min_quorum
//     submit by deadline (or quorum is no longer reachable).
func reconcileCouncilsPass(cfg Config, env councilEnv, now time.Time) []string {
	lines := drainPendingCouncilResolves(env, now)
	councils, err := listCouncils()
	if err != nil {
		if env.logf != nil {
			env.logf("council list: %v", err)
		}
		return lines
	}
	for _, item := range councils {
		switch item.Phase {
		case councilPhaseDone:
			if item.TargetNoted && item.DoneEventEmitted {
				continue
			}
			unlock, err := lockCouncil(item.Bead)
			if err != nil {
				continue
			}
			st, ok, lerr := loadCouncil(item.Bead)
			if lerr == nil && ok && st.Phase == councilPhaseDone && (!st.TargetNoted || !st.DoneEventEmitted) {
				summary := ""
				if st.Chair.SynthesisPath != "" {
					if raw, rerr := os.ReadFile(st.Chair.SynthesisPath); rerr == nil {
						summary = extractSynthesisSummary(string(raw))
					}
				}
				finalizeCouncilDone(env, &st, summary, "ticker", now)
				st.UpdatedAt = now
				_ = saveCouncil(st)
			}
			unlock()

		case councilPhaseFanout:
			if msg := reconcileFanoutCouncil(cfg, env, item.Bead, now); msg != "" {
				lines = append(lines, msg)
			}
		}
	}
	return lines
}

func reconcileFanoutCouncil(cfg Config, env councilEnv, id string, now time.Time) string {
	unlock, err := lockCouncil(id)
	if err != nil {
		return ""
	}
	defer unlock()

	st, ok, err := loadCouncil(id)
	if err != nil || !ok || st.Phase != councilPhaseFanout {
		return ""
	}

	submitted := 0
	active := 0
	for _, m := range st.Members {
		switch m.Status {
		case councilRunSubmitted:
			submitted++
		case councilRunPending, councilRunRunning:
			active++
		}
	}
	deadlineReached := !now.Before(st.Deadline)

	// Failure: deadline passed without min_quorum, or remaining viable members
	// can no longer satisfy min_quorum.
	if (deadlineReached && submitted < st.MinQuorum) || (submitted+active < st.MinQuorum) {
		if deadlineReached {
			for i := range st.Members {
				if st.Members[i].Status == councilRunPending || st.Members[i].Status == councilRunRunning {
					st.Members[i].Status = councilRunTimedOut
				}
			}
		}
		reason := fmt.Sprintf("quorum not met by deadline (%d/%d submitted, min_quorum %d)", submitted, len(st.Members), st.MinQuorum)
		if !deadlineReached {
			reason = fmt.Sprintf("quorum unreachable (%d/%d submitted, %d active, min_quorum %d)", submitted, len(st.Members), active, st.MinQuorum)
		}
		st.Phase = councilPhaseFailed
		st.CompletedAt = now
		st.UpdatedAt = now
		st.FailureReason = reason
		if err := saveCouncil(st); err != nil {
			if env.logf != nil {
				env.logf("council %s save failure: %v", id, err)
			}
			return ""
		}
		if env.note != nil {
			_ = env.note(id, fmt.Sprintf("COUNCIL FAILED: %s", reason))
		}
		if env.event != nil {
			_ = env.event(Event{
				Bead:    id,
				Kind:    EventCouncilFailed,
				Summary: fmt.Sprintf("planning council failed for %s: %s", id, reason),
				At:      now,
				Clears:  &InboxCondition{Kind: "bead_closed", Bead: id, Key: "council_failed"},
			})
		}
		if env.journal != nil {
			_ = env.journal(journalRecord{
				At:     now,
				Kind:   journalCouncil,
				Bead:   id,
				Class:  0,
				Action: "failed",
				Pass:   false,
				Text:   reason,
			})
		}
		return fmt.Sprintf("council %s failed: %s", id, reason)
	}

	readyToFanIn := submitted == len(st.Members) ||
		(submitted >= st.MinQuorum && deadlineReached) ||
		(submitted >= st.MinQuorum && active == 0)
	if !readyToFanIn {
		return ""
	}

	if env.show == nil || env.createBead == nil {
		if env.logf != nil {
			env.logf("council %s fan-in skipped: bead operations unavailable", id)
		}
		return ""
	}
	target, err := env.show(id)
	if err != nil {
		if env.logf != nil {
			env.logf("council %s fan-in lookup target: %v", id, err)
		}
		return ""
	}
	if target.Status == StatusClosed {
		st.Phase = councilPhaseCanceled
		st.CompletedAt = now
		st.UpdatedAt = now
		st.FailureReason = "target bead closed before chair synthesis"
		_ = saveCouncil(st)
		return fmt.Sprintf("council %s canceled: target bead closed", id)
	}

	if st.StartedBy != "TJ" && env.enforce != nil {
		if _, eerr := env.enforce(target, "dispatch", "ticker"); eerr != nil {
			if env.logf != nil {
				env.logf("council %s fan-in deferred: %v", id, eerr)
			}
			return ""
		}
	}

	// Mark any straggling unsubmitted members as timed_out now that fan-in is committed.
	for i := range st.Members {
		if st.Members[i].Status == councilRunPending || st.Members[i].Status == councilRunRunning {
			st.Members[i].Status = councilRunTimedOut
		}
	}

	assembled, err := assembleChairInputs(cfg, &st)
	if err != nil {
		if env.logf != nil {
			env.logf("council %s assemble chair inputs: %v", id, err)
		}
		return ""
	}

	chairID := st.Chair.HelperBead
	if chairID == "" {
		repoRef := councilRepoRef(cfg, target)
		created, cerr := env.createBead(BeadCreateOpts{
			Title:       fmt.Sprintf("Council synthesis %s (%s)", shortTitle(target.Title, 42), target.ID),
			Type:        "task",
			Priority:    target.Priority,
			Parent:      target.Parent,
			Labels:      councilHelperLabels(cfg, target, st.Chair.Role, councilRoleChair),
			Description: buildCouncilChairDescription(target, st, repoRef, assembled),
		})
		if cerr != nil {
			st.Chair.Error = cerr.Error()
			st.UpdatedAt = now
			_ = saveCouncil(st)
			if env.logf != nil {
				env.logf("council %s create chair bead: %v", id, cerr)
			}
			return ""
		}
		chairID = created.ID
		st.Chair.HelperBead = chairID
	}

	if env.dispatchHelper != nil {
		if _, derr := env.dispatchHelper(chairID, st.Chair.Role, "TJ"); derr != nil {
			st.Chair.Error = derr.Error()
			st.UpdatedAt = now
			_ = saveCouncil(st)
			if env.logf != nil {
				env.logf("council %s dispatch chair %s: %v", id, chairID, derr)
			}
			return ""
		}
	}

	st.Phase = councilPhaseSynthesizing
	st.Chair.Status = councilRunRunning
	st.Chair.StartedAt = now
	st.Chair.Error = ""
	st.ChairStartedAt = now
	st.UpdatedAt = now
	if err := saveCouncil(st); err != nil {
		if env.logf != nil {
			env.logf("council %s save synthesizing state: %v", id, err)
		}
		return ""
	}
	if env.note != nil {
		_ = env.note(id, fmt.Sprintf("COUNCIL SYNTHESIZING: %d/%d plans assembled; chair=%s bead=%s anonymize=%t",
			len(assembled), len(st.Members), st.Chair.Role, chairID, st.Anonymize))
	}
	if env.journal != nil {
		_ = env.journal(journalRecord{
			At:     now,
			Kind:   journalCouncil,
			Bead:   id,
			Class:  classHousekeeping,
			Action: "fanin",
			Pass:   true,
			Text:   fmt.Sprintf("%d/%d plans assembled; chair=%s bead=%s", len(assembled), len(st.Members), st.Chair.Role, chairID),
		})
	}
	return fmt.Sprintf("council %s fanned in (%d/%d plans) -> chair %s (%s)", id, len(assembled), len(st.Members), st.Chair.Role, chairID)
}

func (t Ticker) reconcileCouncils(now time.Time) {
	var env councilEnv
	if t.councilEnv != nil {
		env = *t.councilEnv
	} else {
		env = liveCouncilEnv(t.cfg, t.herdr, t.log.Printf)
	}
	if env.logf == nil && t.log != nil {
		env.logf = t.log.Printf
	}
	for _, line := range reconcileCouncilsPass(t.cfg, env, now) {
		t.log.Printf("council: %s", line)
	}
}

const councilUsage = `usage:
  quartermaster council start <bead> [--panel <name>] [--members r1,r2,...] [--chair <role>] [--min-quorum N] [--timeout 20m] [--anonymize=true|false] [--auto-cleanup=true|false] [--by WHO] [--json]
  quartermaster council submit <bead> (--member <role> | --chair) [--file <path|->] [--by WHO] [--json]
  quartermaster council status [<bead>] [--json]
  quartermaster council cancel <bead> [--by WHO] [--json]`

func parseCouncilMembersFlag(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%w: --members must list at least 2 role names", errCouncil)
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("%w: --members contains an empty role entry (%q)", errCouncil, raw)
		}
		out = append(out, part)
	}
	return out, nil
}

func parseCouncilTimeout(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return d, nil
	}
	if mins, err := strconv.Atoi(raw); err == nil {
		return time.Duration(mins) * time.Minute, nil
	}
	return 0, fmt.Errorf("%w: invalid --timeout %q (want Go duration like 20m or positive minutes)", errCouncil, raw)
}

func readCouncilPayload(filePath string, stdin io.Reader) (string, error) {
	var r io.Reader
	if filePath == "" || filePath == "-" {
		if stdin == nil {
			return "", fmt.Errorf("%w: stdin unavailable", errCouncil)
		}
		r = stdin
	} else {
		f, err := os.Open(filePath)
		if err != nil {
			return "", err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, councilMaxSubmitBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > councilMaxSubmitBytes {
		return "", fmt.Errorf("%w: submitted content exceeds %d byte limit", errCouncil, councilMaxSubmitBytes)
	}
	return string(data), nil
}

func councilCmd(cfg Config, env councilEnv, args []string, stdin io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(councilUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "start":
		fs := flag.NewFlagSet("council start", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		panel := fs.String("panel", "", "configured [councils.<name>] panel name")
		membersRaw := fs.String("members", "", "comma-separated member role names (>=2 unique)")
		chair := fs.String("chair", "", "chair role name")
		minQuorum := fs.Int("min-quorum", 0, "minimum submitted members required at deadline")
		timeoutRaw := fs.String("timeout", "", "fan-out deadline duration (e.g. 20m)")
		anonymize := fs.Bool("anonymize", true, "anonymize member plans as Plan A..N for blind chair synthesis")
		autoCleanup := fs.Bool("auto-cleanup", true, "automatically close and resolve helper beads after synthesis")
		by := fs.String("by", "coordinator", "actor starting council (coordinator, inbox-runner, TJ)")
		asJSON := fs.Bool("json", false, "emit JSON output")
		pos, err := interspersed(fs, rest)
		if err != nil {
			return fmt.Errorf("%v\n%s", err, councilUsage)
		}
		if len(pos) != 1 {
			return errors.New(councilUsage)
		}
		visited := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })

		var opts CouncilStartOpts
		opts.Panel = *panel
		if visited["members"] {
			members, merr := parseCouncilMembersFlag(*membersRaw)
			if merr != nil {
				return merr
			}
			opts.Members = members
		}
		opts.Chair = *chair
		if visited["min-quorum"] {
			if *minQuorum <= 0 {
				return fmt.Errorf("%w: --min-quorum must be >= 1", errCouncil)
			}
			opts.MinQuorum = *minQuorum
		}
		if visited["timeout"] {
			d, err := parseCouncilTimeout(*timeoutRaw)
			if err != nil {
				return err
			}
			if d <= 0 {
				return fmt.Errorf("%w: --timeout must be > 0", errCouncil)
			}
			opts.Timeout = d
		}
		if visited["anonymize"] {
			v := *anonymize
			opts.Anonymize = &v
		}
		if visited["auto-cleanup"] {
			v := *autoCleanup
			opts.AutoCleanup = &v
		}
		opts.By = *by

		res, err := startCouncil(cfg, env, pos[0], opts)
		if err != nil {
			return err
		}
		if *asJSON {
			return writePrettyJSON(out, res)
		}
		if _, err := fmt.Fprintln(out, res.Message); err != nil {
			return err
		}
		for _, w := range res.Warnings {
			if _, err := fmt.Fprintf(out, "warning: %s\n", w); err != nil {
				return err
			}
		}
		return nil

	case "submit":
		fs := flag.NewFlagSet("council submit", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		member := fs.String("member", "", "panelist role name submitting a plan")
		chair := fs.Bool("chair", false, "submit the Chair synthesis")
		filePath := fs.String("file", "-", "path to markdown plan/synthesis or - for stdin")
		by := fs.String("by", "", "actor submitting (optional)")
		asJSON := fs.Bool("json", false, "emit JSON output")
		pos, err := interspersed(fs, rest)
		if err != nil {
			return fmt.Errorf("%v\n%s", err, councilUsage)
		}
		if len(pos) != 1 {
			return errors.New(councilUsage)
		}
		if (*member == "") == !*chair {
			return fmt.Errorf("%w: specify exactly one of --member <role> or --chair\n%s", errCouncil, councilUsage)
		}
		visited := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
		if visited["file"] && strings.TrimSpace(*filePath) == "" {
			return fmt.Errorf("%w: --file must not be empty (use - for stdin)", errCouncil)
		}
		content, err := readCouncilPayload(*filePath, stdin)
		if err != nil {
			return err
		}
		res, err := submitCouncilPlan(cfg, env, pos[0], CouncilSubmitOpts{
			Member:  *member,
			Chair:   *chair,
			Content: content,
			By:      *by,
		})
		if err != nil {
			return err
		}
		if *asJSON {
			return writePrettyJSON(out, res)
		}
		_, err = fmt.Fprintln(out, res.Message)
		return err

	case "status":
		fs := flag.NewFlagSet("council status", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		asJSON := fs.Bool("json", false, "emit JSON output")
		pos, err := interspersed(fs, rest)
		if err != nil {
			return fmt.Errorf("%v\n%s", err, councilUsage)
		}
		if len(pos) > 1 {
			return errors.New(councilUsage)
		}
		if len(pos) == 1 {
			st, ok, err := loadCouncil(pos[0])
			if err != nil {
				return err
			}
			if !ok {
				if *asJSON {
					return writePrettyJSON(out, CouncilState{Version: councilSchemaVersion, Bead: pos[0]})
				}
				_, err = fmt.Fprintf(out, "%s: no council recorded\n", pos[0])
				return err
			}
			if *asJSON {
				return writePrettyJSON(out, st)
			}
			writeCouncilStatusText(out, st, env.clock())
			return nil
		}
		councils, err := listCouncils()
		if err != nil {
			return err
		}
		if *asJSON {
			if councils == nil {
				councils = []CouncilState{}
			}
			return writePrettyJSON(out, councils)
		}
		if len(councils) == 0 {
			_, err = fmt.Fprintln(out, "(no councils recorded)")
			return err
		}
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "BEAD\tPANEL\tPHASE\tQUORUM\tDEADLINE\tCHAIR\tSYNTHESIS")
		for _, st := range councils {
			panel := orDash(st.Panel)
			deadline := "-"
			if !st.Deadline.IsZero() {
				deadline = st.Deadline.UTC().Format(time.RFC3339)
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d (min %d)\t%s\t%s\t%s\n",
				st.Bead, panel, orDash(st.Phase), st.submittedCount(), len(st.Members), st.MinQuorum, deadline, orDash(st.Chair.Role), orDash(st.Chair.SynthesisPath))
		}
		return tw.Flush()

	case "cancel":
		fs := flag.NewFlagSet("council cancel", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		by := fs.String("by", "coordinator", "actor canceling council (coordinator, TJ)")
		asJSON := fs.Bool("json", false, "emit JSON output")
		pos, err := interspersed(fs, rest)
		if err != nil {
			return fmt.Errorf("%v\n%s", err, councilUsage)
		}
		if len(pos) != 1 {
			return errors.New(councilUsage)
		}
		res, err := cancelCouncil(env, pos[0], *by)
		if err != nil {
			return err
		}
		if *asJSON {
			return writePrettyJSON(out, res)
		}
		_, err = fmt.Fprintln(out, res.Message)
		return err

	default:
		return fmt.Errorf("unknown council command %q\n%s", sub, councilUsage)
	}
}

func writeCouncilStatusText(w io.Writer, st CouncilState, now time.Time) {
	_, _ = fmt.Fprintf(w, "bead:      %s\n", st.Bead)
	_, _ = fmt.Fprintf(w, "panel:     %s\n", orDash(st.Panel))
	_, _ = fmt.Fprintf(w, "phase:     %s\n", orDash(st.Phase))
	_, _ = fmt.Fprintf(w, "anonymize: %t\n", st.Anonymize)
	_, _ = fmt.Fprintf(w, "quorum:    %d/%d (min %d)\n", st.submittedCount(), len(st.Members), st.MinQuorum)
	if !st.Deadline.IsZero() {
		dl := st.Deadline.UTC().Format(time.RFC3339)
		if st.Phase == councilPhaseFanout {
			if rem := st.Deadline.Sub(now); rem > 0 {
				dl += fmt.Sprintf(" (%s left)", rem.Round(time.Second))
			} else {
				dl += " (expired)"
			}
		}
		_, _ = fmt.Fprintf(w, "deadline:  %s\n", dl)
	}
	if st.FailureReason != "" {
		_, _ = fmt.Fprintf(w, "failure:   %s\n", st.FailureReason)
	}
	if len(st.Members) > 0 {
		_, _ = fmt.Fprintln(w, "members:")
		for _, m := range st.Members {
			line := fmt.Sprintf("  %s  %s  [%s]", m.Label, m.Role, orDash(m.Status))
			if m.HelperBead != "" {
				line += "  bead=" + m.HelperBead
			}
			if m.ArtifactPath != "" {
				line += "  artifact=" + m.ArtifactPath
			}
			if m.Error != "" {
				line += "  error=" + m.Error
			}
			_, _ = fmt.Fprintln(w, line)
		}
	}
	chairLine := fmt.Sprintf("chair:     %s  [%s]", orDash(st.Chair.Role), orDash(st.Chair.Status))
	if st.Chair.HelperBead != "" {
		chairLine += "  bead=" + st.Chair.HelperBead
	}
	if st.Chair.SynthesisPath != "" {
		chairLine += "  synthesis=" + st.Chair.SynthesisPath
	}
	if st.Chair.Error != "" {
		chairLine += "  error=" + st.Chair.Error
	}
	_, _ = fmt.Fprintln(w, chairLine)
	if len(st.PendingResolveBeads) > 0 {
		_, _ = fmt.Fprintf(w, "cleanup:   pending resolve (%s)\n", strings.Join(st.PendingResolveBeads, ", "))
	}
}
