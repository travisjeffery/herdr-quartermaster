package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// InboxCondition identifies the observation that keeps an item actionable.
// An empty key on an older event means its resource identity is unknown, not
// that an arbitrary current prompt/head is the same one. Archives keep the
// original bytes and filenames: coordinator acknowledgements rely on them.
type InboxCondition struct {
	Kind string `json:"kind"`
	Bead string `json:"bead,omitempty"`
	Key  string `json:"key,omitempty"`
}

func defaultInboxCondition(e Event) *InboxCondition {
	kind := "active"
	switch e.Kind {
	case "closed", EventAutoClosed:
		kind = "manual"
	case EventLikelyStale, EventLeaseExpired, EventBreaker, EventWatchTriggered, EventWatchExpired:
		// Verify, lease expiry, breakers and watches can raise notices on
		// open/backlog beads, which were never in the active-thread set.
		kind = "bead_closed"
	case EventOrphanFinish:
		// Orphan finisher notices can apply to open or in_progress agentless
		// beads and clear once the bead closes or a live agent picks it up.
		kind = "orphan_finish"
	case EventNeedsYou:
		kind = "needs_you" // legacy: either a bead question or an agent prompt
	case EventAgentGone:
		kind = "agent_gone"
	case EventFailing:
		kind = "checks_failing"
	case EventReady:
		kind = "ready_to_merge"
	case EventReadyForReview:
		kind = "ready_for_review"
	case EventReviewBump:
		kind = "review_bump"
	case EventUnsafeAutoMerge:
		kind = "unsafe_auto_merge"
	}
	if e.Bead == "" {
		kind = "manual" // e.g. the global lease-expired notice
	}
	return &InboxCondition{Kind: kind, Bead: e.Bead}
}

func eventCondition(t Thread, kind EventKind) *InboxCondition {
	c := defaultInboxCondition(Event{Kind: kind, Bead: t.Bead.ID})
	switch kind {
	case EventNeedsYou:
		switch {
		case t.Prompt != nil:
			c.Kind, c.Key = "prompt", t.Prompt.Hash
		case t.Agent != nil && t.Agent.Status == "blocked":
			c.Kind = "agent_blocked"
		default:
			c.Kind, c.Key = "needs_me", asked(t.Bead)
		}
	case EventFailing, EventReady, EventReadyForReview, EventReviewBump, EventUnsafeAutoMerge:
		c.Key = inboxPRKey(t)
	}
	return c
}

func inboxPRKey(t Thread) string {
	if t.PR == nil {
		return ""
	}
	return fmt.Sprintf("%s#%d@%s", t.PR.URL, t.PR.Number, t.PR.HeadSHA)
}

// inboxObsolete only uses a successful, complete active-thread observation.
// Unknown prompt/PR detail is not evidence of resolution. This deliberately
// does not classify a view: it tests the specific cause recorded on the item.
func inboxObsolete(it InboxItem, active map[string]Thread) bool {
	c := it.Clears
	if c == nil {
		c = defaultInboxCondition(it.Event)
	}
	if c.Kind == "manual" || it.Bead == "" {
		return false
	}
	if c.Bead != it.Bead {
		return false // malformed metadata is not evidence of resolution
	}
	t, ok := active[it.Bead]
	if c.Kind == "bead_closed" {
		return ok && t.Bead.Status == StatusClosed
	}
	if c.Kind == "orphan_finish" {
		return ok && (t.Bead.Status == StatusClosed || t.Agent != nil)
	}
	if !ok || (t.Bead.Status != StatusInProgress && t.Bead.Status != StatusNeedsMe && t.Bead.Status != StatusBlocked) {
		return true
	}
	blocked := t.Agent != nil && t.Agent.Status == "blocked"
	switch c.Kind {
	case "needs_you":
		return t.Bead.Status != StatusNeedsMe && !t.AgentUnknown && !blocked
	case "needs_me":
		return t.Bead.Status != StatusNeedsMe || (!t.Unprompted && c.Key != "" && c.Key != asked(t.Bead))
	case "agent_blocked":
		return !t.AgentUnknown && !blocked
	case "prompt":
		return !t.AgentUnknown && (!blocked || (t.Prompt != nil && c.Key != "" && c.Key != t.Prompt.Hash))
	case "agent_gone":
		return t.Agent != nil
	case "checks_failing", "ready_to_merge":
		if t.PR == nil {
			return false
		}
		if t.PR.State != "OPEN" || (c.Key != "" && c.Key != inboxPRKey(t)) {
			return true
		}
		if c.Kind == "checks_failing" {
			return len(t.Checks.Failing) == 0
		}
		// Readiness can flicker while GitHub recomputes checks/gates. The
		// ticker announces it once per head, so retain that observation
		// until its resource changes or a human decision supersedes it.
		return t.Bead.Status == StatusNeedsMe
	case "ready_for_review", "review_bump":
		if t.PR == nil {
			return false
		}
		if t.PR.State != "OPEN" || (c.Key != "" && c.Key != inboxPRKey(t)) {
			return true
		}
		if t.Bead.Status == StatusNeedsMe || t.PR.IsDraft || t.PR.ReviewDecision != "REVIEW_REQUIRED" {
			return true
		}
		if len(t.Checks.Failing) > 0 || t.PR.MergeState == "DIRTY" || t.PR.Mergeable == "CONFLICTING" {
			return true
		}
		if t.PR.Gate != nil && t.PR.Gate.Head == t.PR.HeadSHA && (t.PR.Gate.OpenThreads > 0 || t.PR.Gate.MoreThreads) {
			return true
		}
		return false
	case "unsafe_auto_merge":
		if t.PR == nil {
			return false
		}
		if t.PR.State != "OPEN" || (c.Key != "" && c.Key != inboxPRKey(t)) {
			return true
		}
		return unsafeAutoKey(t) == ""
	}
	return false
}

func reconcileInbox(threads []Thread, show func(string) (Bead, error)) error {
	items, err := readInbox()
	if err != nil {
		return err
	}
	active := make(map[string]Thread, len(threads))
	for _, th := range threads {
		active[th.Bead.ID] = th
	}
	// Only notices raised outside the thread lifecycle need an extra Beads
	// read. Cache failures too: unavailable truth must not archive evidence.
	looked := map[string]bool{}
	var files []string
	for _, it := range items {
		c := it.Clears
		if c == nil {
			c = defaultInboxCondition(it.Event)
		}
		if _, known := active[it.Bead]; !known && (c.Kind == "bead_closed" || c.Kind == "orphan_finish") && show != nil && !looked[it.Bead] {
			looked[it.Bead] = true
			if b, err := show(it.Bead); err == nil && b.ID == it.Bead {
				active[it.Bead] = Thread{Bead: b}
			}
		}
		if inboxObsolete(it, active) {
			files = append(files, it.File)
		}
	}
	_, err = inboxDoneFiles(files)
	return err
}

// inboxDoneFiles archives exactly the displayed files. Empty means none, and
// a new item for the same bead arriving after rendering is never selected.
func inboxDoneFiles(files []string) (int, error) {
	for _, file := range files {
		if file == "" || file == "." || filepath.Base(file) != file {
			return 0, fmt.Errorf("invalid inbox filename %q", file)
		}
	}
	items, err := readInbox()
	if err != nil {
		return 0, err
	}
	want := make(map[string]bool, len(files))
	for _, file := range files {
		want[file] = true
	}
	n := 0
	var errs []error
	for _, it := range items {
		if !want[it.File] || headlessOwned(it.Bead) {
			continue // verifier-owned events require verified outcomes
		}
		done := filepath.Join(inboxDir(), "done")
		if err := os.MkdirAll(done, 0700); err != nil {
			return n, err
		}
		if err := os.Rename(filepath.Join(inboxDir(), it.File), filepath.Join(done, it.File)); err == nil {
			n++
		} else if !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return n, errors.Join(errs...)
}

const (
	questionKindNeedsMe = "needs_me"
	questionKindPrompt  = "prompt"
)

// InboxQuestion is one live decision waiting on a human: either a worker's
// parseable approval prompt or a needs_me bead's OPTIONS question.
// Pointer fields omit omitempty so JSON always emits explicit nulls.
type InboxQuestion struct {
	Bead           string                `json:"bead"`
	Kind           string                `json:"kind"`
	Question       string                `json:"question"`
	Recommendation *int                  `json:"recommendation"`
	Options        []InboxQuestionOption `json:"options"`
	Class          *int                  `json:"class"`
	BeadStatus     string                `json:"bead_status"`
	Hash           *string               `json:"hash"`
	Expires        *string               `json:"expires"`
}

// InboxQuestionOption is one numbered choice of an InboxQuestion.
type InboxQuestionOption struct {
	N     int    `json:"n"`
	Label string `json:"label"`
}

type inboxQuestionsEnv struct {
	active func() ([]Bead, error)
	agents func(host string) ([]Agent, error)
	prompt promptEnv
	state  func() TickerState
	warnf  func(format string, args ...any)
}

func liveInboxQuestionsEnv(cfg Config, h Herdr, errOut io.Writer) inboxQuestionsEnv {
	hh := h.on(cfg.coordSocket())
	return inboxQuestionsEnv{
		active: Beads{}.Active,
		agents: func(host string) ([]Agent, error) {
			if host != localHost {
				hc, ok := cfg.host(host)
				if !ok || !hc.enabled() {
					return nil, fmt.Errorf("host %q is not configured or enabled", host)
				}
			}
			return cfg.herdrOn(hh, host).Agents()
		},
		prompt: livePromptEnv(cfg, hh),
		state:  loadState,
		warnf: func(format string, args ...any) {
			if errOut != nil {
				fmt.Fprintf(errOut, format+"\n", args...)
			}
		},
	}
}

// inboxQuestions gathers the two live question sources — parseable worker
// approval prompts and live needs_me OPTIONS questions — without minting tap
// links or modifying inbox state. RUN: beads and already-answered questions
// never appear.
func inboxQuestions(env inboxQuestionsEnv) ([]InboxQuestion, error) {
	beads, err := env.active()
	if err != nil {
		return nil, err
	}
	out := make([]InboxQuestion, 0)
	if len(beads) == 0 {
		return out, nil
	}
	var st TickerState
	if env.state != nil {
		st = env.state()
	}
	hostSeen := map[string]bool{}
	var hosts []string
	for _, b := range beads {
		host := beadHost(b)
		if host == "" {
			host = localHost
		}
		if !hostSeen[host] {
			hostSeen[host] = true
			hosts = append(hosts, host)
		}
	}
	sort.Strings(hosts)
	byHost := make(map[string]map[string]Agent, len(hosts))
	if env.agents != nil {
		for _, host := range hosts {
			list, err := env.agents(host)
			if err != nil {
				if env.warnf != nil {
					env.warnf("quartermaster: list agents on %s: %v", host, err)
				}
				continue
			}
			m := make(map[string]Agent, len(list))
			for _, a := range list {
				if a.Name != "" {
					m[a.Name] = a
				}
			}
			byHost[host] = m
		}
	}
	for _, b := range beads {
		host := beadHost(b)
		if host == "" {
			host = localHost
		}
		var a *Agent
		if ag, ok := byHost[host][agentName(b.ID)]; ok {
			agCopy := ag
			a = &agCopy
		}
		th := restoreThreadEvidence(Thread{Bead: b, Agent: a}, st.Threads[b.ID]).derive()
		if runCommand(th) != "" {
			continue
		}
		if a != nil && a.Status == "blocked" {
			if p, err := readAgentPrompt(env.prompt, host, a); err == nil {
				out = append(out, promptInboxQuestion(b, p))
				continue
			}
		}
		if q, ok := liveQuestion(th); ok {
			out = append(out, needsMeInboxQuestion(b, q))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bead < out[j].Bead })
	return out, nil
}

func promptInboxQuestion(b Bead, p ApprovalPrompt) InboxQuestion {
	h := p.Hash
	return InboxQuestion{
		Bead:       b.ID,
		Kind:       questionKindPrompt,
		Question:   promptQuestionText(p),
		Options:    questionOptions(p.Options),
		BeadStatus: b.Status,
		Hash:       &h,
	}
}

func needsMeInboxQuestion(b Bead, q NeedsMeQuestion) InboxQuestion {
	var class *int
	if q.ClassN >= 0 {
		c := q.ClassN
		class = &c
	}
	// Hash stays nil for needs_me: its hash lives only bound inside signed
	// tap tokens, never exposed as an --expect equivalent here.
	return InboxQuestion{
		Bead:           b.ID,
		Kind:           questionKindNeedsMe,
		Question:       strings.Join(q.Lines, "\n"),
		Recommendation: q.recommendation(),
		Options:        questionOptions(q.Options),
		Class:          class,
		BeadStatus:     b.Status,
	}
}

// promptQuestionText formats a parsed approval prompt's header lines without
// dropping the actual command behind a tool description or truncating it.
func promptQuestionText(p ApprovalPrompt) string {
	var lines []string
	for _, l := range p.Lines {
		if !strings.HasPrefix(l, "Tip:") {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		lines = p.Lines
	}
	if len(lines) == 0 {
		return p.Summary()
	}
	return strings.Join(lines, "\n")
}

func questionOptions(opts []PromptOption) []InboxQuestionOption {
	out := make([]InboxQuestionOption, len(opts))
	for i, o := range opts {
		out[i] = InboxQuestionOption{N: o.Number, Label: o.Label}
	}
	return out
}

func writeInboxQuestions(w io.Writer, qs []InboxQuestion, asJSON bool) error {
	if qs == nil {
		qs = []InboxQuestion{}
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(qs)
	}
	if len(qs) == 0 {
		_, err := fmt.Fprintln(w, "(none)")
		return err
	}
	for _, q := range qs {
		meta := []string{q.Kind}
		if q.Hash != nil {
			meta = append(meta, *q.Hash)
		}
		if q.Class != nil {
			meta = append(meta, fmt.Sprintf("C%d", *q.Class))
		}
		if q.Recommendation != nil {
			meta = append(meta, fmt.Sprintf("rec %d", *q.Recommendation))
		}
		opts := make([]string, len(q.Options))
		for i, o := range q.Options {
			opts[i] = fmt.Sprintf("%d %s", o.N, shortTitle(o.Label, 50))
		}
		if _, err := fmt.Fprintf(w, "- %s [%s] %s (%s)\n", q.Bead, strings.Join(meta, " · "), shortTitle(q.Question, 140), strings.Join(opts, " · ")); err != nil {
			return err
		}
	}
	return nil
}

func inboxCmd(cfg Config, h Herdr, pos []string, questions, asJSON bool, out, errOut io.Writer) error {
	if questions {
		if len(pos) != 0 {
			return errors.New("usage: quartermaster inbox --questions [--json]")
		}
		qs, err := inboxQuestions(liveInboxQuestionsEnv(cfg, h, errOut))
		if err != nil {
			return err
		}
		return writeInboxQuestions(out, qs, asJSON)
	}
	if asJSON {
		return errors.New("--json applies to `quartermaster inbox --questions`, not `inbox done`")
	}
	if len(pos) == 0 || pos[0] != "done" {
		return errors.New("usage: quartermaster inbox done [<bead>|<file>...] | quartermaster inbox --questions [--json]")
	}
	n, err := inboxDone(pos[1:])
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%d item(s) marked done\n", n)
	return nil
}
