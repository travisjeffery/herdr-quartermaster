package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// compactContextBudget is the strict upper bound (in bytes) for `context --compact`.
const compactContextBudget = 4096

type contextSection string

const (
	ctxSummary  contextSection = "summary"
	ctxAutonomy contextSection = "autonomy"
	ctxInbox    contextSection = "inbox"
	ctxThreads  contextSection = "threads"
	ctxLinear   contextSection = "linear"
	ctxNext     contextSection = "next"
	ctxReady    contextSection = "ready"
)

var contextSectionOrder = []contextSection{
	ctxSummary, ctxAutonomy, ctxInbox, ctxThreads, ctxLinear, ctxNext, ctxReady,
}

var contextSectionNames = []string{
	string(ctxSummary), string(ctxAutonomy), string(ctxInbox),
	string(ctxThreads), string(ctxLinear), string(ctxNext), string(ctxReady),
}

// sectionList collects repeatable comma-separated --section flag values.
type sectionList struct {
	values []string
	set    bool
}

func (s *sectionList) String() string { return strings.Join(s.values, ",") }

func (s *sectionList) Set(v string) error {
	s.set = true
	s.values = append(s.values, v)
	return nil
}

// contextSections records which sections of `quartermaster context` to emit.
type contextSections struct {
	explicit bool
	set      map[contextSection]bool
}

func (s contextSections) has(sec contextSection) bool {
	if !s.explicit {
		return true
	}
	return s.set[sec]
}

// parseContextSections parses one or more comma-separated --section values.
// When explicit is false (flag not passed), all sections are included.
func parseContextSections(specs []string, explicit bool) (contextSections, error) {
	if !explicit {
		return contextSections{}, nil
	}
	out := contextSections{explicit: true, set: map[contextSection]bool{}}
	for _, spec := range specs {
		for _, raw := range strings.Split(spec, ",") {
			name := strings.TrimSpace(raw)
			if name == "" {
				continue
			}
			sec := contextSection(name)
			if !slices.Contains(contextSectionOrder, sec) {
				return contextSections{}, fmt.Errorf("--section %q: want any of %s", name, strings.Join(contextSectionNames, ","))
			}
			out.set[sec] = true
		}
	}
	if len(out.set) == 0 {
		return contextSections{}, fmt.Errorf("--section: want any of %s", strings.Join(contextSectionNames, ","))
	}
	return out, nil
}

// ContextOptions controls how `quartermaster context` renders its sections.
type ContextOptions struct {
	Compact  bool
	Sections contextSections
	JSON     bool
}

func groupKey(g Group) string {
	switch g {
	case GroupNeedsYou:
		return "needs_you"
	case GroupMerged:
		return "merged"
	case GroupReady:
		return "ready_to_merge"
	case GroupReview:
		return "review"
	case GroupRollingOut:
		return "rolling_out"
	case GroupFailing:
		return "checks_failing"
	case GroupIdle:
		return "idle"
	case GroupWorking:
		return "working"
	case GroupBlocked:
		return "blocked"
	case GroupNoAgent:
		return "no_agent"
	default:
		return strings.ReplaceAll(g.String(), " ", "_")
	}
}

type contextSummary struct {
	NeedsYou       int      `json:"needs_you,omitempty"`
	NeedsYouActive int      `json:"needs_you_active,omitempty"`
	NeedsYouParked int      `json:"needs_you_parked,omitempty"`
	Merged         int      `json:"merged,omitempty"`
	ReadyToMerge   int      `json:"ready_to_merge,omitempty"`
	Review         int      `json:"review,omitempty"`
	RollingOut     int      `json:"rolling_out,omitempty"`
	ChecksFailing  int      `json:"checks_failing,omitempty"`
	Idle           int      `json:"idle,omitempty"`
	Working        int      `json:"working,omitempty"`
	Blocked        int      `json:"blocked,omitempty"`
	NoAgent        int      `json:"no_agent,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
}

type contextAutonomyLease struct {
	Target  string    `json:"target"`
	Level   int       `json:"level"`
	Until   time.Time `json:"until"`
	Prev    *int      `json:"prev,omitempty"`
	Expired bool      `json:"expired,omitempty"`
}

type contextAutonomy struct {
	Level     int                    `json:"level"`
	Source    string                 `json:"source,omitempty"`
	CappedBy  string                 `json:"capped_by,omitempty"`
	Max       int                    `json:"max"`
	Stopped   bool                   `json:"stopped,omitempty"`
	Reporting string                 `json:"reporting,omitempty"`
	Leases    []contextAutonomyLease `json:"leases,omitempty"`
	Warning   string                 `json:"warning,omitempty"`
}

type contextInboxItem struct {
	Bead    string    `json:"bead"`
	Kind    EventKind `json:"kind"`
	Summary string    `json:"summary,omitempty"`
	At      time.Time `json:"at"`
	File    string    `json:"file,omitempty"`
}

type contextPR struct {
	Number         int    `json:"number"`
	State          string `json:"state,omitempty"`
	URL            string `json:"url,omitempty"`
	IsDraft        bool   `json:"isDraft,omitempty"`
	ReviewDecision string `json:"reviewDecision,omitempty"`
}

type contextThread struct {
	Bead          string     `json:"bead,omitempty"`
	Group         string     `json:"group"`
	Status        string     `json:"status,omitempty"`
	Parked        bool       `json:"parked,omitempty"`
	ParkedReason  string     `json:"parked_reason,omitempty"`
	SnoozedUntil  *time.Time `json:"snoozed_until,omitempty"`
	PR            *contextPR `json:"pr"`
	Repo          string     `json:"repo,omitempty"`
	Role          string     `json:"role,omitempty"`
	RolloutTarget string     `json:"rollout_target,omitempty"`
	ReviewLoop    string     `json:"review_loop,omitempty"`
	State         string     `json:"state,omitempty"`
	Agent         string     `json:"agent,omitempty"`
	Title         string     `json:"title,omitempty"`
	Linear        string     `json:"linear,omitempty"`
	Level         string     `json:"level,omitempty"`
	Run           string     `json:"run,omitempty"`
	Question      string     `json:"question,omitempty"`
	Options       string     `json:"options,omitempty"`
	Watches       string     `json:"watches,omitempty"`
	NoAgentKind   string     `json:"no_agent_kind,omitempty"`

	Folded bool     `json:"folded,omitempty"`
	Count  int      `json:"count,omitempty"`
	Beads  []string `json:"beads,omitempty"`

	group      Group  `json:"-"`
	repoTag    string `json:"-"`
	roleTag    string `json:"-"`
	targetTag  string `json:"-"`
	isNeedsMe  bool   `json:"-"`
	parkedKind string `json:"-"`
	cardLine   string `json:"-"`
}

func (t contextThread) MarshalJSON() ([]byte, error) {
	if t.Folded {
		type foldedThread struct {
			Group  string   `json:"group"`
			Status string   `json:"status,omitempty"`
			Parked bool     `json:"parked,omitempty"`
			Count  int      `json:"count"`
			Folded bool     `json:"folded"`
			Beads  []string `json:"beads,omitempty"`
		}
		return json.Marshal(foldedThread{
			Group:  t.Group,
			Status: t.Status,
			Parked: t.Parked,
			Count:  t.Count,
			Folded: true,
			Beads:  t.Beads,
		})
	}
	type plain contextThread
	return json.Marshal(plain(t))
}

type contextBead struct {
	ID       string `json:"id"`
	Priority int    `json:"priority"`
	Type     string `json:"issue_type,omitempty"`
	Title    string `json:"title,omitempty"`
}

// contextDoc is the single structured representation from which both text and
// JSON context forms are rendered, keeping their sections and compact
// truncation identical.
type contextDoc struct {
	Summary  *contextSummary     `json:"summary,omitempty"`
	Autonomy *contextAutonomy    `json:"autonomy,omitempty"`
	Inbox    *[]contextInboxItem `json:"inbox,omitempty"`
	Threads  *[]contextThread    `json:"threads,omitempty"`
	Linear   *[]string           `json:"linear,omitempty"`
	Next     *[]contextBead      `json:"next,omitempty"`
	Ready    *[]contextBead      `json:"ready,omitempty"`

	now             time.Time
	compact         bool
	summaryCounts   map[Group]int
	needsYouActive  int
	needsYouParked  int
	needsYouSnoozed int
	needsYouDormant int
	autonomyFull    string
	noAgentRes      []string
	noAgentStale    []string
	noAgentBare     []string
	extraWatches    []watchLine
	workflowsText   string
	inboxOmitted    int
	linearOmitted   int
	nextOmitted     int
	readyOmitted    int
}

type watchLine struct {
	bead    string
	summary string
}

// renderContextWith renders context according to opts (text or JSON, full or
// compact, all sections or a subset).
func renderContextWith(cfg Config, threads []Thread, next, ready []Bead, inbox []InboxItem, st TickerState, worktreed map[string]bool, now time.Time, opts ContextOptions) (string, error) {
	doc := buildContextDoc(cfg, threads, next, ready, inbox, st, worktreed, now, opts)
	if opts.Compact {
		var measure func(*contextDoc) int
		if opts.JSON {
			measure = func(d *contextDoc) int {
				b, err := marshalContextJSON(d, true)
				if err != nil {
					return compactContextBudget
				}
				return len(b)
			}
		} else {
			measure = func(d *contextDoc) int {
				return len(renderContextText(d))
			}
		}
		enforceCompactBudget(&doc, measure)
	}
	if opts.JSON {
		b, err := marshalContextJSON(&doc, opts.Compact)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return renderContextText(&doc), nil
}

func buildContextDoc(cfg Config, rawThreads []Thread, next, ready []Bead, inbox []InboxItem, st TickerState, worktreed map[string]bool, now time.Time, opts ContextOptions) contextDoc {
	threads := slices.Clone(rawThreads)
	seenBeads := make(map[string]Bead, len(threads)+len(next)+len(ready))
	for _, t := range threads {
		seenBeads[t.Bead.ID] = t.Bead
	}
	for _, b := range next {
		if _, ok := seenBeads[b.ID]; !ok {
			seenBeads[b.ID] = b
		}
	}
	for _, b := range ready {
		if _, ok := seenBeads[b.ID]; !ok {
			seenBeads[b.ID] = b
		}
	}
	allSeen := make([]Bead, 0, len(seenBeads))
	for _, b := range seenBeads {
		allSeen = append(allSeen, b)
	}
	if children := openChildCounts(allSeen); len(children) > 0 {
		for i := range threads {
			if n := children[threads[i].Bead.ID]; n > threads[i].OpenChildren {
				threads[i].OpenChildren = n
				threads[i] = threads[i].derive()
			}
		}
	}
	snoozes := loadSnoozeState()
	type parkedMeta struct {
		parked bool
		kind   string
		until  time.Time
	}
	parkedByID := map[string]parkedMeta{}
	counts := map[Group]int{}
	needsYouActive, needsYouParked, needsYouSnoozed, needsYouDormant := 0, 0, 0, 0
	for _, t := range threads {
		g := classify(t)
		counts[g]++
		if g == GroupNeedsYou {
			parked, kind, until := classifyNeedsYou(cfg, t, snoozes, now)
			parkedByID[t.Bead.ID] = parkedMeta{parked: parked, kind: kind, until: until}
			if parked {
				needsYouParked++
				if kind == parkedReasonSnoozed {
					needsYouSnoozed++
				} else {
					needsYouDormant++
				}
			} else {
				needsYouActive++
			}
		}
	}
	sort.SliceStable(threads, func(i, j int) bool {
		gi, gj := classify(threads[i]), classify(threads[j])
		if gi != gj {
			return gi < gj
		}
		if gi == GroupNeedsYou {
			pi, pj := parkedByID[threads[i].Bead.ID].parked, parkedByID[threads[j].Bead.ID].parked
			if pi != pj {
				return !pi
			}
		}
		return threads[i].Bead.ID < threads[j].Bead.ID
	})

	doc := contextDoc{
		now:             now,
		compact:         opts.Compact,
		summaryCounts:   counts,
		needsYouActive:  needsYouActive,
		needsYouParked:  needsYouParked,
		needsYouSnoozed: needsYouSnoozed,
		needsYouDormant: needsYouDormant,
	}

	if opts.Sections.has(ctxSummary) {
		sum := &contextSummary{
			NeedsYou:       counts[GroupNeedsYou],
			NeedsYouActive: needsYouActive,
			NeedsYouParked: needsYouParked,
			Merged:         counts[GroupMerged],
			ReadyToMerge:   counts[GroupReady],
			Review:         counts[GroupReview],
			RollingOut:     counts[GroupRollingOut],
			ChecksFailing:  counts[GroupFailing],
			Idle:           counts[GroupIdle],
			Working:        counts[GroupWorking],
			Blocked:        counts[GroupBlocked],
			NoAgent:        counts[GroupNoAgent],
		}
		if !opts.Compact {
			sum.Warnings = contextWarnings(cfg, threads, next, ready, st)
		}
		doc.Summary = sum
	}

	if opts.Sections.has(ctxAutonomy) {
		a, full := buildContextAutonomy(cfg.Autonomy, now, opts.Compact)
		doc.Autonomy = &a
		doc.autonomyFull = full
	}

	if opts.Sections.has(ctxInbox) {
		items := make([]contextInboxItem, 0, len(inbox))
		for _, it := range inbox {
			ci := contextInboxItem{
				Bead: it.Bead,
				Kind: it.Kind,
				At:   it.At,
			}
			if !opts.Compact {
				ci.Summary = it.Summary
				ci.File = it.File
			}
			items = append(items, ci)
		}
		doc.Inbox = &items
	}

	if opts.Sections.has(ctxThreads) {
		var watchesByBead map[string][]Watch
		shownWatches := map[string]bool{}
		if !opts.Compact {
			watchesByBead = activeWatchesByBead()
		}
		items := make([]contextThread, 0, len(threads))
		noAgentCount := 0
		for _, t := range threads {
			g := classify(t)
			isNeedsMe := t.Bead.Status == StatusNeedsMe
			if g == GroupNoAgent {
				if opts.Compact && !isNeedsMe {
					noAgentCount++
					continue
				}
				kind := "bare"
				switch {
				case worktreed[t.Bead.ID]:
					kind = "resumable"
					doc.noAgentRes = append(doc.noAgentRes, t.Bead.ID)
				case worktreed != nil && len(staleClaims([]Bead{t.Bead}, st.PRs, now, staleAge)) > 0:
					kind = "stale"
					doc.noAgentStale = append(doc.noAgentStale, t.Bead.ID)
				default:
					doc.noAgentBare = append(doc.noAgentBare, t.Bead.ID)
				}
				ct := buildContextThread(cfg, t, g, kind, opts.Compact)
				if ws := watchesByBead[t.Bead.ID]; len(ws) > 0 {
					ct.Watches = formatThreadWatches(ws)
				}
				items = append(items, ct)
				continue
			}
			ct := buildContextThread(cfg, t, g, "", opts.Compact)
			if g == GroupNeedsYou {
				if pm := parkedByID[t.Bead.ID]; pm.parked {
					ct.Parked = true
					ct.parkedKind = pm.kind
					if !opts.Compact {
						ct.ParkedReason = formatParkedReason(pm.kind, pm.until, t.Bead.UpdatedAt, now)
						if !pm.until.IsZero() {
							u := pm.until
							ct.SnoozedUntil = &u
						}
					}
				}
			}
			if ws := watchesByBead[t.Bead.ID]; len(ws) > 0 {
				ct.Watches = formatThreadWatches(ws)
				shownWatches[t.Bead.ID] = true
			}
			items = append(items, ct)
		}
		for _, id := range slices.Sorted(maps.Keys(watchesByBead)) {
			if !shownWatches[id] {
				doc.extraWatches = append(doc.extraWatches, watchLine{
					bead:    id,
					summary: formatThreadWatches(watchesByBead[id]),
				})
			}
		}
		if opts.Compact && noAgentCount > 0 {
			items = append(items, contextThread{
				Group:  groupKey(GroupNoAgent),
				group:  GroupNoAgent,
				Folded: true,
				Count:  noAgentCount,
			})
		}
		doc.Threads = &items
		if !opts.Compact {
			doc.workflowsText = workflowContextSection()
		}
	}

	if opts.Sections.has(ctxLinear) {
		moves := linearMoves(threads)
		if len(moves) > 0 || opts.Sections.explicit {
			if moves == nil {
				moves = []string{}
			}
			doc.Linear = &moves
		}
	}

	if opts.Sections.has(ctxNext) {
		items := make([]contextBead, 0, len(next))
		for _, b := range next {
			items = append(items, buildContextBead(b, opts.Compact))
		}
		doc.Next = &items
	}

	if opts.Sections.has(ctxReady) {
		nextIDs := map[string]bool{}
		for _, b := range next {
			nextIDs[b.ID] = true
		}
		var rest []Bead
		for _, b := range ready {
			if !nextIDs[b.ID] && b.Status == StatusOpen {
				rest = append(rest, b)
			}
		}
		if len(rest) > 15 {
			rest = rest[:15]
		}
		items := make([]contextBead, 0, len(rest))
		for _, b := range rest {
			items = append(items, buildContextBead(b, opts.Compact))
		}
		doc.Ready = &items
	}

	return doc
}

func contextWarnings(cfg Config, threads []Thread, next, ready []Bead, st TickerState) []string {
	var out []string
	if !st.GHFailingFor.IsZero() {
		out = append(out, fmt.Sprintf("gh has been failing since %s (%s); PR state below is stale.", st.GHFailingFor.Format(time.Kitchen), strings.Join(st.GHFailingIn, ", ")))
	}
	out = append(out, unknownRepos(cfg, threads, next, ready)...)
	out = append(out, unknownRoles(cfg, threads, next, ready)...)
	if pid := runningPID(); pid == 0 {
		out = append(out, "the ticker is not running (`quartermaster ticker start`); nothing follows PRs or updates the sidebar.")
	} else if w := socketWarning(runningSocket(), cfg.coordSocket(), os.Getenv("HERDR_SOCKET_PATH")); w != "" {
		out = append(out, w+".")
	}
	return out
}

func buildContextAutonomy(a AutonomyConfig, now time.Time, compact bool) (contextAutonomy, string) {
	s, err := loadAutonomy()
	if err != nil {
		warn := fmt.Sprintf("%v; every bead reads as level 0 until it is fixed", err)
		return contextAutonomy{Level: 0, Max: a.Max, Warning: warn},
			fmt.Sprintf("WARNING: %s\n", warn)
	}
	stopped := autonomyStopped()
	g, why := a.capped(s, "", a.global(s), stopped, now)
	src := "[autonomy] level"
	if s.Level != nil {
		src = "autonomy level"
	}
	ca := contextAutonomy{
		Level:   g,
		Max:     a.Max,
		Stopped: stopped,
	}
	if !compact {
		ca.Source = src
		ca.CappedBy = why
		ca.Reporting = reportingMode(g)
	}
	for _, key := range slices.Sorted(maps.Keys(s.Leases)) {
		l := s.Leases[key]
		target := key
		if target == "" {
			target = "global"
		}
		ca.Leases = append(ca.Leases, contextAutonomyLease{
			Target:  target,
			Level:   l.Level,
			Until:   l.Until,
			Prev:    l.Prev,
			Expired: !now.Before(l.Until),
		})
	}
	full := autonomyStatus(a, s, stopped, nil, now) + "reporting: " + reportingMode(g) + "\n"
	return ca, full
}

func buildContextThread(cfg Config, t Thread, g Group, noAgentKind string, compact bool) contextThread {
	ct := contextThread{
		Bead:      t.Bead.ID,
		Group:     groupKey(g),
		Status:    t.Bead.Status,
		group:     g,
		isNeedsMe: t.Bead.Status == StatusNeedsMe,
	}
	if t.PR != nil {
		pr := &contextPR{Number: t.PR.Number}
		if !compact {
			pr.State = t.PR.State
			pr.URL = t.PR.URL
			pr.IsDraft = t.PR.IsDraft
			pr.ReviewDecision = t.PR.ReviewDecision
		}
		ct.PR = pr
	}
	if compact {
		return ct
	}
	ct.repoTag = repoTag(cfg, t.Bead)
	if name, ok := cfg.repoName(t.Bead); ok && name != "" {
		ct.Repo = name
	}
	ct.roleTag = roleTag(cfg, t.Bead)
	if role, err := cfg.resolveRoleForBead(t.Bead, ""); err == nil && role.Name != roleImplementer {
		ct.Role = role.Name
	}
	ct.targetTag = rolloutTargetTag(t.Bead)
	if target, err := rolloutTarget(t.Bead); err == nil && target != "" {
		ct.RolloutTarget = target
	}
	ct.ReviewLoop = reviewLoopContextLine(t.Bead.ID)
	ct.State = stateLine(t)
	agent := "no agent"
	if t.Agent != nil {
		agent = t.Agent.Kind + " " + t.Agent.Status
	}
	ct.Agent = agent
	ct.Title = shortTitle(t.Bead.Title, 70)
	ct.Linear = t.Linear
	ct.Level = t.Level
	ct.NoAgentKind = noAgentKind
	if g == GroupNeedsYou && t.Bead.Status == StatusNeedsMe {
		cmd := runCommand(t)
		ct.Run = cmd
		if q, ok := liveQuestion(t); ok {
			ct.Question = shortTitle(strings.Join(q.Lines, " "), 200)
			ct.Options = q.Choices()
			if !compact {
				ct.cardLine = cardContextLines(t.Bead)
			}
		} else if cmd != "" {
			if reason := pendingCommandReason(t.Bead.Notes); reason != "" {
				ct.Question = shortTitle(reason, 200)
			}
		} else {
			ct.Question = needsMeSummaryNote(t.Bead.Notes, 200)
		}
	}
	return ct
}

func buildContextBead(b Bead, compact bool) contextBead {
	cb := contextBead{
		ID:       b.ID,
		Priority: b.Priority,
		Type:     b.Type,
	}
	if !compact {
		cb.Title = shortTitle(b.Title, 80)
	}
	return cb
}

// enforceCompactBudget applies the spec's truncation order until sizeFn(doc)
// fits strictly under compactContextBudget (< 4 KB), never dropping any
// needs_me bead.
func enforceCompactBudget(doc *contextDoc, sizeFn func(*contextDoc) int) {
	over := func() bool { return sizeFn(doc) >= compactContextBudget }
	if !over() {
		return
	}

	// 1. Ready list first.
	if doc.Ready != nil && len(*doc.Ready) > 0 {
		doc.readyOmitted += len(*doc.Ready)
		empty := []contextBead{}
		doc.Ready = &empty
		if !over() {
			return
		}
	}

	// 2. Non-live thread groups folded to counts (GroupNoAgent is already folded
	// in compact mode).
	for _, g := range []Group{
		GroupBlocked, GroupIdle, GroupMerged, GroupRollingOut, GroupReview,
	} {
		if foldThreadGroup(doc, g) && !over() {
			return
		}
	}

	// 3. Selected next and linear reminders if still over budget.
	if doc.Next != nil && len(*doc.Next) > 0 {
		doc.nextOmitted += len(*doc.Next)
		empty := []contextBead{}
		doc.Next = &empty
		if !over() {
			return
		}
	}
	if doc.Linear != nil && len(*doc.Linear) > 0 {
		doc.linearOmitted += len(*doc.Linear)
		empty := []string{}
		doc.Linear = &empty
		if !over() {
			return
		}
	}

	// 4. Remaining non-needs_you thread groups if still over budget.
	for _, g := range []Group{
		GroupWorking, GroupFailing, GroupReady,
	} {
		if foldThreadGroup(doc, g) && !over() {
			return
		}
	}

	// 5. Inbox summaries (drop oldest non-needs_you items first, then oldest
	// needs_you items, keeping as many as fit under budget).
	if doc.Inbox != nil && len(*doc.Inbox) > 0 {
		trimCompactInbox(doc, over)
		if !over() {
			return
		}
	}

	// 6. Non-needs_me items inside GroupNeedsYou (e.g. prompt-blocked in_progress).
	if foldThreadGroup(doc, GroupNeedsYou) && !over() {
		return
	}

	// 7. Extreme fallback: drop autonomy leases, pack parked needs_me IDs first
	// (keeping active needs_me threads individual when possible), then pack any
	// remaining needs_me IDs onto a single folded entry so every needs_me bead
	// stays named under 4 KB.
	if doc.Autonomy != nil && len(doc.Autonomy.Leases) > 0 {
		doc.Autonomy.Leases = nil
		if !over() {
			return
		}
	}
	if packParkedNeedsMeThreads(doc) && !over() {
		return
	}
	packNeedsMeThreads(doc)
}

// foldThreadGroup folds non-needs_me individual threads in group g into a
// single count entry. Returns true if any thread was folded.
func foldThreadGroup(doc *contextDoc, g Group) bool {
	if doc.Threads == nil {
		return false
	}
	var kept []contextThread
	var foldedCount int
	var foldedOther []contextThread
	for _, t := range *doc.Threads {
		if t.Folded {
			if t.group == g && t.Status == "" {
				foldedCount += t.Count
			} else {
				foldedOther = append(foldedOther, t)
			}
			continue
		}
		if t.group == g && !t.isNeedsMe {
			foldedCount++
			continue
		}
		kept = append(kept, t)
	}
	if foldedCount == 0 {
		return false
	}
	foldedOther = append(foldedOther, contextThread{
		Group:  groupKey(g),
		group:  g,
		Folded: true,
		Count:  foldedCount,
	})
	sort.SliceStable(foldedOther, func(i, j int) bool {
		return foldedOther[i].group < foldedOther[j].group
	})
	out := append(kept, foldedOther...)
	doc.Threads = &out
	return true
}

func trimCompactInbox(doc *contextDoc, over func() bool) {
	items := *doc.Inbox
	// Drop oldest non-needs_you items first.
	for over() && len(items) > 0 {
		dropIdx := -1
		for i, it := range items {
			if it.Kind != EventNeedsYou {
				dropIdx = i
				break
			}
		}
		if dropIdx < 0 {
			// Only needs_you items left: drop oldest.
			dropIdx = 0
		}
		items = append(items[:dropIdx], items[dropIdx+1:]...)
		doc.inboxOmitted++
		cur := items
		doc.Inbox = &cur
	}
}

// packParkedNeedsMeThreads compacts parked needs_me threads into a single ID
// list entry before touching active needs_me threads.
func packParkedNeedsMeThreads(doc *contextDoc) bool {
	if doc.Threads == nil {
		return false
	}
	var ids []string
	var rest []contextThread
	for _, t := range *doc.Threads {
		if !t.Folded && t.isNeedsMe && t.Parked {
			ids = append(ids, t.Bead)
			continue
		}
		rest = append(rest, t)
	}
	if len(ids) == 0 {
		return false
	}
	packed := contextThread{
		Group:  groupKey(GroupNeedsYou),
		group:  GroupNeedsYou,
		Status: StatusNeedsMe,
		Parked: true,
		Folded: true,
		Count:  len(ids),
		Beads:  ids,
	}
	out := append(rest, packed)
	doc.Threads = &out
	return true
}

// packNeedsMeThreads compacts individual needs_me threads into a single ID
// list entry while keeping every needs_me bead named.
func packNeedsMeThreads(doc *contextDoc) {
	if doc.Threads == nil {
		return
	}
	var ids []string
	var rest []contextThread
	for _, t := range *doc.Threads {
		if !t.Folded && t.isNeedsMe {
			ids = append(ids, t.Bead)
			continue
		}
		if t.Folded && t.group == GroupNeedsYou && t.Status == StatusNeedsMe && len(t.Beads) > 0 {
			ids = append(ids, t.Beads...)
			continue
		}
		rest = append(rest, t)
	}
	if len(ids) == 0 {
		return
	}
	packed := contextThread{
		Group:  groupKey(GroupNeedsYou),
		group:  GroupNeedsYou,
		Status: StatusNeedsMe,
		Folded: true,
		Count:  len(ids),
		Beads:  ids,
	}
	out := append([]contextThread{packed}, rest...)
	doc.Threads = &out
}

func marshalContextJSON(doc *contextDoc, compact bool) ([]byte, error) {
	var b []byte
	var err error
	if compact {
		b, err = json.Marshal(doc)
	} else {
		b, err = json.MarshalIndent(doc, "", "  ")
	}
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func renderContextText(doc *contextDoc) string {
	var parts []string
	if doc.Summary != nil {
		parts = append(parts, renderSummaryText(doc))
	}
	if doc.Autonomy != nil {
		parts = append(parts, renderAutonomyText(doc))
	}
	if doc.Inbox != nil {
		parts = append(parts, renderInboxText(doc))
	}
	if doc.Threads != nil {
		parts = append(parts, renderThreadsText(doc))
	}
	if doc.workflowsText != "" {
		parts = append(parts, strings.TrimPrefix(doc.workflowsText, "\n"))
	}
	if doc.Linear != nil {
		parts = append(parts, renderLinearText(doc))
	}
	if doc.Next != nil {
		parts = append(parts, renderBeadSectionText("## Selected next", " (open, label `next`)", *doc.Next, doc.nextOmitted, doc.compact))
	}
	if doc.Ready != nil {
		parts = append(parts, renderBeadSectionText("## Ready", " (open, unblocked)", *doc.Ready, doc.readyOmitted, doc.compact))
	}
	return strings.Join(parts, "\n")
}

func renderSummaryText(doc *contextDoc) string {
	var s strings.Builder
	var summary []string
	for g := GroupNeedsYou; g <= GroupNoAgent; g++ {
		if n := doc.summaryCounts[g]; n > 0 {
			if g == GroupNeedsYou && doc.needsYouParked > 0 {
				summary = append(summary, fmt.Sprintf("%d %s (%d active, %d parked)", n, g, doc.needsYouActive, doc.needsYouParked))
			} else {
				summary = append(summary, fmt.Sprintf("%d %s", n, g))
			}
		}
	}
	if len(summary) == 0 {
		summary = []string{"no active threads"}
	}
	if doc.compact {
		fmt.Fprintf(&s, "%s\n", strings.Join(summary, " · "))
		return s.String()
	}
	fmt.Fprintf(&s, "# quartermaster context (%s)\n\n", doc.now.Format("2006-01-02 15:04 MST"))
	fmt.Fprintf(&s, "%s\n", strings.Join(summary, " · "))
	for _, w := range doc.Summary.Warnings {
		fmt.Fprintf(&s, "\nWARNING: %s\n", w)
	}
	return s.String()
}

func renderAutonomyText(doc *contextDoc) string {
	var s strings.Builder
	if doc.compact {
		s.WriteString("## Autonomy\n")
		a := doc.Autonomy
		if a.Warning != "" {
			fmt.Fprintf(&s, "warning: %s\n", a.Warning)
			return s.String()
		}
		fmt.Fprintf(&s, "level: %d (max %d)", a.Level, a.Max)
		if a.Stopped {
			s.WriteString(", off")
		}
		if len(a.Leases) > 0 {
			fmt.Fprintf(&s, ", %d lease(s)", len(a.Leases))
		}
		s.WriteByte('\n')
		return s.String()
	}
	s.WriteString("## Autonomy (`quartermaster autonomy level`)\n")
	s.WriteString(doc.autonomyFull)
	return s.String()
}

func renderInboxText(doc *contextDoc) string {
	var s strings.Builder
	items := *doc.Inbox
	if doc.compact {
		s.WriteString("## Inbox\n")
		if len(items) == 0 && doc.inboxOmitted == 0 {
			s.WriteString("(empty)\n")
			return s.String()
		}
		for _, it := range items {
			if it.Bead == "" {
				fmt.Fprintf(&s, "- [%s]\n", it.Kind)
			} else {
				fmt.Fprintf(&s, "- %s [%s]\n", it.Bead, it.Kind)
			}
		}
		if doc.inboxOmitted > 0 {
			if len(items) == 0 {
				fmt.Fprintf(&s, "(%d omitted)\n", doc.inboxOmitted)
			} else {
				fmt.Fprintf(&s, "- … (+%d omitted)\n", doc.inboxOmitted)
			}
		}
		return s.String()
	}
	s.WriteString("## Inbox (unhandled; `quartermaster inbox done <file>` for one item, [bead...] for bulk)\n")
	if len(items) == 0 {
		s.WriteString("(empty)\n")
	}
	for _, it := range items {
		fmt.Fprintf(&s, "- %s %s [%s] %s\n", it.At.Local().Format("15:04"), it.Bead, it.Kind, it.Summary)
		if it.File != "" {
			fmt.Fprintf(&s, "  file: %s\n", it.File)
		}
	}
	return s.String()
}

func renderThreadsText(doc *contextDoc) string {
	var s strings.Builder
	items := *doc.Threads
	if doc.compact {
		s.WriteString("## Threads\n")
		if len(items) == 0 {
			s.WriteString("(none)\n")
			return s.String()
		}
		for _, t := range items {
			if t.Folded {
				if len(t.Beads) > 0 {
					label := t.Status
					if t.Parked {
						label += " parked"
					}
					fmt.Fprintf(&s, "- %s (%d): %s\n", label, t.Count, strings.Join(t.Beads, ", "))
				} else {
					fmt.Fprintf(&s, "- %s: %d\n", t.Group, t.Count)
				}
				continue
			}
			line := fmt.Sprintf("- %s · %s", t.Bead, t.Group)
			if t.Parked {
				line += " · parked"
			}
			if t.PR != nil {
				line += " · PR#" + strconv.Itoa(t.PR.Number)
			}
			s.WriteString(line + "\n")
		}
		return s.String()
	}

	writeThread := func(t contextThread) {
		state := t.State
		if t.Parked && t.ParkedReason != "" {
			state = "parked (" + t.ParkedReason + ")"
		}
		fmt.Fprintf(&s, "- %s%s%s%s · %s · %s · %s\n", t.Bead, t.repoTag, t.roleTag, t.targetTag, state, t.Agent, t.Title)
		if t.Run != "" {
			fmt.Fprintf(&s, "  run: %s\n", t.Run)
		}
		if t.Options != "" {
			fmt.Fprintf(&s, "  question: %s\n  options: %s\n", t.Question, t.Options)
			if t.cardLine != "" {
				s.WriteString(t.cardLine)
			}
		} else if t.Question != "" {
			fmt.Fprintf(&s, "  question: %s\n", t.Question)
		}
		if t.PR != nil && t.PR.URL != "" {
			fmt.Fprintf(&s, "  %s\n", t.PR.URL)
		}
		if t.ReviewLoop != "" {
			fmt.Fprintf(&s, "  review: %s\n", t.ReviewLoop)
		}
		if t.Watches != "" {
			fmt.Fprintf(&s, "  watches: %s\n", t.Watches)
		}
	}

	s.WriteString("## Threads (bead · state · agent)\n")
	if len(items) == 0 {
		s.WriteString("(none)\n")
	}
	if doc.needsYouParked == 0 {
		for _, t := range items {
			if t.group == GroupNoAgent {
				continue
			}
			writeThread(t)
		}
	} else {
		var activeNeeds, otherThreads, parkedNeeds []contextThread
		for _, t := range items {
			if t.group == GroupNoAgent {
				continue
			}
			switch {
			case t.group == GroupNeedsYou && t.Parked:
				parkedNeeds = append(parkedNeeds, t)
			case t.group == GroupNeedsYou:
				activeNeeds = append(activeNeeds, t)
			default:
				otherThreads = append(otherThreads, t)
			}
		}
		fmt.Fprintf(&s, "### Active needs you (%d)\n", len(activeNeeds))
		if len(activeNeeds) == 0 {
			s.WriteString("(none)\n")
		}
		for _, t := range activeNeeds {
			writeThread(t)
		}
		if len(otherThreads) > 0 {
			s.WriteString("### Other threads\n")
			for _, t := range otherThreads {
				writeThread(t)
			}
		}
		fmt.Fprintf(&s, "### Parked / Dormant needs you (%s)\n", formatParkedBreakdown(doc.needsYouSnoozed, doc.needsYouDormant))
		for _, t := range parkedNeeds {
			writeThread(t)
		}
	}
	if len(doc.noAgentRes) > 0 {
		fmt.Fprintf(&s, "- resumable (%d): %s  → quartermaster resume\n", len(doc.noAgentRes), strings.Join(doc.noAgentRes, ", "))
	}
	if len(doc.noAgentStale) > 0 {
		fmt.Fprintf(&s, "- stale claims (%d): %s — no agent, no worktree, no PR, untouched %dd+  → quartermaster stale\n", len(doc.noAgentStale), strings.Join(doc.noAgentStale, ", "), int(staleAge.Hours()/24))
	}
	if len(doc.noAgentBare) > 0 {
		fmt.Fprintf(&s, "- claimed, no agent (%d): %s\n", len(doc.noAgentBare), strings.Join(doc.noAgentBare, ", "))
	}
	for _, w := range doc.extraWatches {
		fmt.Fprintf(&s, "- %s · watches: %s\n", w.bead, w.summary)
	}
	return s.String()
}

func renderLinearText(doc *contextDoc) string {
	var s strings.Builder
	moves := *doc.Linear
	if doc.compact {
		s.WriteString("## Linear\n")
		if len(moves) == 0 && doc.linearOmitted > 0 {
			fmt.Fprintf(&s, "(%d omitted)\n", doc.linearOmitted)
			return s.String()
		}
		if len(moves) == 0 {
			s.WriteString("(none)\n")
			return s.String()
		}
		for _, m := range moves {
			fmt.Fprintf(&s, "- %s\n", m)
		}
		return s.String()
	}
	s.WriteString("## Linear (expected issue status)\n")
	if len(moves) == 0 {
		s.WriteString("(none)\n")
		return s.String()
	}
	for _, m := range moves {
		fmt.Fprintf(&s, "- %s\n", m)
	}
	return s.String()
}

func renderBeadSectionText(heading, detail string, beads []contextBead, omitted int, compact bool) string {
	var s strings.Builder
	if compact {
		s.WriteString(heading + "\n")
		if len(beads) == 0 && omitted > 0 {
			fmt.Fprintf(&s, "(%d omitted)\n", omitted)
			return s.String()
		}
		if len(beads) == 0 {
			s.WriteString("(none)\n")
			return s.String()
		}
		for _, b := range beads {
			fmt.Fprintf(&s, "- %s P%d\n", b.ID, b.Priority)
		}
		return s.String()
	}
	s.WriteString(heading + detail + "\n")
	if len(beads) == 0 {
		s.WriteString("(none)\n")
		return s.String()
	}
	for _, b := range beads {
		fmt.Fprintf(&s, "- %s P%d %s: %s\n", b.ID, b.Priority, b.Type, b.Title)
	}
	return s.String()
}
