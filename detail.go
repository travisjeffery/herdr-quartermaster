package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// beadDetail is what the board's detail pane shows beyond its row: the
// slower lookups, loaded once the pane opens on a bead.
type beadDetail struct {
	// full is the bead from bd show, which alone carries the description and
	// parent; nil until it loads.
	full    *Bead
	fullErr string
	// tail is the end of the agent's screen; tailDone once the read returned.
	tail     []string
	tailErr  string
	tailDone bool
	inbox    []InboxItem
}

// detailResult is one lazy lookup finishing, applied to the bead's detail.
type detailResult struct {
	id    string
	apply func(*beadDetail)
}

const (
	detailDescLines           = 10
	detailHistoryLines        = 12
	detailHistoryNoteMaxLines = 2
	detailDoneMax             = 5
	detailDoneNoteMaxLines    = 2
	detailTailLines           = 10
	detailTimeout             = 5 * time.Second
)

// linkedPRs is every PR the ticker's last pass knows a bead by: the one it is
// delivered through, its merged ones, and those its notes link. A linked PR
// the ticker never looked up still comes back, by URL alone, so it can be
// opened. Nothing here asks GitHub.
func linkedPRs(st TickerState, b Bead) []PR {
	var out []PR
	seen := map[string]bool{}
	add := func(pr PR) {
		if pr.URL == "" || !seen[pr.URL] {
			seen[pr.URL] = true
			out = append(out, pr)
		}
	}
	if pr, ok := st.PRs[b.ID]; ok {
		add(pr)
	}
	for _, pr := range st.MergedPRs[b.ID] {
		add(pr)
	}
	for _, u := range NotedPRs(b.Notes) {
		if pr, ok := st.Noted[u]; ok {
			add(pr)
		} else if !seen[u] {
			add(PR{URL: u, Number: prNumber(u)})
		}
	}
	return out
}

func prNumber(url string) int {
	var n int
	if m := prURL.FindStringSubmatch(url); m != nil {
		fmt.Sscan(m[2], &n)
	}
	return n
}

// prName is owner/repo#n, or the bare #n when the URL isn't a PR's.
func prName(pr PR) string {
	if m := prURL.FindStringSubmatch(pr.URL); m != nil {
		return m[1] + "#" + m[2]
	}
	return fmt.Sprintf("#%d", pr.Number)
}

// prSummary is one PR's state in a line: what the ticker last saw of it.
func prSummary(pr PR) string {
	parts := []string{prName(pr)}
	if pr.State == "" {
		return prName(pr) + " · not looked up yet"
	}
	parts = append(parts, strings.ToLower(pr.State))
	if pr.IsDraft {
		parts = append(parts, "draft")
	}
	if pr.State == "OPEN" {
		if pr.MergeState != "" {
			parts = append(parts, strings.ToLower(pr.MergeState))
		}
		if pr.ReviewDecision != "" {
			parts = append(parts, strings.ToLower(strings.ReplaceAll(pr.ReviewDecision, "_", " ")))
		}
		if c := checkSummary(summarizeChecks(pr.Checks)); c != "" {
			parts = append(parts, c)
		}
	}
	return strings.Join(parts, " · ")
}

func checkSummary(c Checks) string {
	var s []string
	if n := len(c.Failing); n > 0 {
		s = append(s, fmt.Sprintf("%d failing (%s)", n, strings.Join(c.Failing, ", ")))
	}
	if c.Pending > 0 {
		s = append(s, fmt.Sprintf("%d running", c.Pending))
	}
	if c.Passing > 0 {
		s = append(s, fmt.Sprintf("%d passed", c.Passing))
	}
	if len(s) == 0 {
		return ""
	}
	return "checks " + strings.Join(s, ", ")
}

// noteLines is a bead's notes, oldest first: bd keeps one note per line.
func noteLines(notes string) []string {
	var lines []string
	for _, l := range strings.Split(notes, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// waitingNotes is how many of a needs_me bead's last notes say what it
// waits on: a full structured QUESTION:/Details:/Evidence:/OPTIONS:/CLASS:
// block (never chopping it across Waiting and History), a RUN: and its why,
// or the last two notes reaching back up to four for the latest note that
// asks something.
func waitingNotes(notes []string) int {
	if len(notes) == 0 {
		return 0
	}
	// Skip trailing CLASS: and non-answer housekeeping lines (e.g. autonomy
	// level changes or AUTO handoff notes) when checking for a live structured
	// QUESTION:/OPTIONS: block at the tail.
	end := len(notes) - 1
	for end >= 0 && (isClassLine(notes[end]) || isNonAnswerHousekeepingNote(notes[end])) {
		end--
	}
	if end >= 0 && questionOptionRe.MatchString(notes[end]) {
		optStart := end
		for optStart > 0 && questionOptionRe.MatchString(notes[optStart-1]) {
			optStart--
		}
		if optStart > 0 && notes[optStart-1] == "OPTIONS:" {
			for j := optStart - 2; j >= 0; j-- {
				if isAnswerBoundaryLine(notes[j]) {
					break
				}
				if strings.HasPrefix(notes[j], "QUESTION:") {
					start := j
					if start > 0 && strings.HasPrefix(notes[start-1], "CRITICAL:") {
						start--
					}
					return len(notes) - start
				}
			}
		}
	}
	// Structured free-text QUESTION: (with Details:/Evidence:/CLASS:) without
	// OPTIONS: also stays intact up to 10 lines back unless an answer intervened.
	for j := end; j >= 0 && j >= len(notes)-10; j-- {
		if isAnswerBoundaryLine(notes[j]) {
			break
		}
		if strings.HasPrefix(notes[j], "QUESTION:") {
			start := j
			if start > 0 && strings.HasPrefix(notes[start-1], "CRITICAL:") {
				start--
			}
			return len(notes) - start
		}
	}

	n := min(2, len(notes))
	for i := len(notes) - 1; i >= 0 && i >= len(notes)-4; i-- {
		if asks(notes[i]) {
			return max(n, len(notes)-i)
		}
	}
	return n
}

var questionRecSplitRe = regexp.MustCompile(`(?i)\bRecommendation:\s*`)

// questionParts separates a structured NeedsMeQuestion into scannable pieces so
// the board can show the crisp question, highlighted recommendation, and
// numbered options before longer Details/Evidence prose.
type questionParts struct {
	critical       string
	question       string
	recommendation string
	details        []string
	evidence       []string
}

func splitQuestionParts(q NeedsMeQuestion) questionParts {
	var p questionParts
	for _, raw := range q.Lines {
		l := strings.TrimSpace(raw)
		if l == "" {
			continue
		}
		switch {
		case strings.HasPrefix(l, "CRITICAL:"):
			if p.critical == "" {
				p.critical = l
			} else {
				p.details = append(p.details, l)
			}
		case len(l) >= len("Details:") && strings.EqualFold(l[:len("Details:")], "Details:"):
			if body := strings.TrimSpace(l[len("Details:"):]); body != "" {
				p.details = append(p.details, body)
			}
		case len(l) >= len("Evidence:") && strings.EqualFold(l[:len("Evidence:")], "Evidence:"):
			if body := strings.TrimSpace(l[len("Evidence:"):]); body != "" {
				p.evidence = append(p.evidence, body)
			}
		case p.question == "":
			if loc := questionRecSplitRe.FindStringIndex(l); loc != nil {
				qText := strings.TrimSpace(l[:loc[0]])
				recText := strings.TrimSpace(l[loc[1]:])
				if qText != "" {
					p.question = qText
				} else {
					p.question = l
				}
				if recText != "" {
					p.recommendation = recText
				}
			} else {
				p.question = l
			}
		default:
			if loc := questionRecSplitRe.FindStringIndex(l); loc != nil && p.recommendation == "" && loc[0] == 0 {
				p.recommendation = strings.TrimSpace(l[loc[1]:])
			} else {
				p.details = append(p.details, l)
			}
		}
	}
	return p
}

// extractDoneHighlights picks high-signal completed milestones from earlier
// bead notes and merged PRs so you can scan what has already been accomplished
// without reading raw agent logs.
func extractDoneHighlights(r boardRow, earlier []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] || len(out) >= detailDoneMax {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for i := len(earlier) - 1; i >= 0 && len(out) < detailDoneMax; i-- {
		l := strings.TrimSpace(earlier[i])
		if l == "" {
			continue
		}
		switch {
		case strings.HasPrefix(l, "MILESTONE ["):
			add(strings.TrimSpace(strings.TrimPrefix(l, "MILESTONE ")))
		case strings.HasPrefix(l, "MILESTONE:"):
			add(strings.TrimSpace(strings.TrimPrefix(l, "MILESTONE:")))
		case strings.HasPrefix(l, "APPLIED"),
			strings.HasPrefix(l, "VERIFIED:"),
			strings.HasPrefix(l, "RAN:"),
			strings.HasPrefix(l, "DONE:"),
			strings.HasPrefix(l, "NEEDS_ME ANSWERED:"),
			strings.HasPrefix(l, "PROMPT ANSWERED:"),
			strings.HasPrefix(l, "DECISION:"),
			strings.HasPrefix(l, "FINDINGS"):
			add(l)
		case strings.HasPrefix(l, "TJ ") && strings.Contains(l, "DECISION"):
			add(l)
		}
	}
	for _, pr := range r.prs {
		if len(out) >= detailDoneMax {
			break
		}
		if pr.State != "MERGED" {
			continue
		}
		tag := fmt.Sprintf("#%d", pr.Number)
		covered := false
		for _, item := range out {
			if pr.Number > 0 && strings.Contains(item, tag) {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		label := "Merged " + prName(pr)
		if pr.Title != "" {
			label += " — " + pr.Title
		}
		add(label)
	}
	return out
}

func historyNoteStyle(note string) string {
	switch {
	case strings.HasPrefix(note, "MILESTONE"),
		strings.HasPrefix(note, "VERIFIED:"),
		strings.HasPrefix(note, "APPLIED"),
		strings.HasPrefix(note, "RAN:"),
		strings.HasPrefix(note, "DONE:"):
		return ansiGreen
	case strings.HasPrefix(note, "NEEDS_ME ANSWERED:"),
		strings.HasPrefix(note, "PROMPT ANSWERED:"),
		strings.HasPrefix(note, "DECISION:"),
		strings.HasPrefix(note, "TJ "):
		return ansiYellow
	case strings.HasPrefix(note, "QUESTION:"),
		strings.HasPrefix(note, "CRITICAL:"),
		strings.HasPrefix(note, "RUN:"):
		return ansiCyan
	case isHousekeepingNote(note):
		return ansiDim
	default:
		return ""
	}
}

func appendWrappedCapped(out *[]styledLine, firstStyle, contStyle, firstIndent, contIndent, text string, w, maxLines int) {
	firstW := max(1, w-utf8.RuneCountInString(firstIndent))
	lines := wrap(text, firstW)
	if len(lines) == 0 {
		return
	}
	limit := len(lines)
	truncated := false
	if maxLines > 0 && limit > maxLines {
		limit = maxLines
		truncated = true
	}
	for i := 0; i < limit; i++ {
		indent := firstIndent
		style := firstStyle
		if i > 0 {
			indent = contIndent
			style = contStyle
		}
		line := lines[i]
		avail := max(1, w-utf8.RuneCountInString(indent))
		if truncated && i == limit-1 {
			r := []rune(line)
			if len(r) >= avail {
				if avail > 1 {
					line = string(r[:avail-1]) + "…"
				} else {
					line = "…"
				}
			} else {
				line += "…"
			}
		} else if utf8.RuneCountInString(line) > avail {
			line = string([]rune(line)[:avail])
		}
		*out = append(*out, styledLine{text: indent + line, style: style})
	}
}

var asksRE = regexp.MustCompile(`\?|(?i)\bquestion\b|\boptions?\b|\(a\)|^RUN:`)

func asks(note string) bool { return asksRE.MatchString(note) }

// notesSplit is a bead's latest note and how many came before it.
func notesSplit(notes string) (latest string, earlier int) {
	lines := noteLines(notes)
	if len(lines) == 0 {
		return "", 0
	}
	return lines[len(lines)-1], len(lines) - 1
}

// tailLines is the last n lines of an agent's screen worth reading: trailing
// blanks and the TUI's box-drawing rules dropped.
func tailLines(screen string, n int) []string {
	var out []string
	for _, l := range strings.Split(screen, "\n") {
		l = strings.TrimRight(l, " \t\r")
		if strings.TrimSpace(strings.Map(func(r rune) rune {
			if r >= 0x2500 && r <= 0x257f {
				return -1
			}
			return r
		}, l)) == "" {
			continue
		}
		out = append(out, l)
	}
	return out[max(0, len(out)-n):]
}

var linearURLs = regexp.MustCompile(`https://linear\.app/[\w-]+/issue/([A-Z][A-Z0-9]*-\d+)`)

// linearURL is where a bead's Linear issue lives: a link its notes already
// carry, else one built from the configured workspace; "" when neither.
func linearURL(key, notes, workspace string) string {
	if key == "" {
		return ""
	}
	for _, m := range linearURLs.FindAllStringSubmatch(notes, -1) {
		if m[1] == key {
			return m[0]
		}
	}
	if workspace == "" {
		return ""
	}
	return "https://linear.app/" + workspace + "/issue/" + key
}

// styledLine is one line of the detail pane and the ANSI style it is drawn in.
type styledLine struct {
	text  string
	style string
}

type detailOpts struct {
	omitWaiting   bool
	decisionFirst bool
	compactDesc   int
}

// detailLines renders the pane for row r in width w. d holds whatever of the
// slow lookups has finished.
func detailLines(r boardRow, d *beadDetail, w int) []styledLine {
	return detailLinesWithOpts(r, d, w, detailOpts{})
}

func detailLinesWithOpts(r boardRow, d *beadDetail, w int, opts detailOpts) []styledLine {
	if d == nil {
		d = &beadDetail{}
	}
	b := r.bead
	if d.full != nil {
		b = *d.full
	}
	var out []styledLine
	add := func(style, text string) { out = append(out, styledLine{text, style}) }
	para := func(style, indent, text string) {
		for _, l := range wrap(text, max(1, w-utf8.RuneCountInString(indent))) {
			add(style, indent+l)
		}
	}
	section := func(name string) {
		add("", "")
		para(ansiBold, "", name)
	}

	head := []string{b.ID, b.Status, fmt.Sprintf("P%d", b.Priority)}
	if b.Type != "" {
		head = append(head, b.Type)
	}
	if len(b.Labels) > 0 {
		head = append(head, "labels "+strings.Join(b.Labels, ", "))
	}
	if b.Parent != "" {
		head = append(head, "parent "+b.Parent)
	}
	para(ansiCyan, "", strings.Join(head, " · "))
	para(ansiBold, "", b.Title)
	if r.line != "" {
		para(r.color, "", r.line)
	}
	var when []string
	for _, t := range []struct {
		name string
		at   time.Time
	}{{"created", b.CreatedAt}, {"updated", b.UpdatedAt}} {
		if !t.at.IsZero() {
			when = append(when, t.name+" "+t.at.Local().Format("Jan 2 15:04"))
		}
	}
	if b.Assignee != "" {
		when = append(when, "assignee "+b.Assignee)
	}
	if len(when) > 0 {
		para(ansiDim, "", strings.Join(when, " · "))
	}

	notes := noteLines(b.Notes)
	shown := 1
	state := r.state
	if state == nil {
		derived := deriveThreadState(Thread{Bead: r.bead, Agent: r.agent})
		state = &derived
	}
	if state.Kind == ThreadWaiting {
		shown = waitingNotes(notes)
	}
	earlier := notes[:max(0, len(notes)-shown)]
	done := extractDoneHighlights(r, earlier)

	descLimit := detailDescLines
	if opts.compactDesc > 0 {
		descLimit = opts.compactDesc
	}
	renderDesc := func() {
		section("Description")
		switch {
		case d.full != nil && strings.TrimSpace(b.Description) == "":
			add(ansiDim, "  (none)")
		case d.full != nil:
			lines := wrap(strings.TrimSpace(b.Description), max(1, w-2))
			for _, l := range lines[:min(len(lines), descLimit)] {
				add("", "  "+l)
			}
			if more := len(lines) - descLimit; more > 0 {
				para(ansiDim, "  ", fmt.Sprintf("… %d more lines (b shows all)", more))
			}
		case d.fullErr != "":
			para(ansiRed, "  ", d.fullErr)
		default:
			add(ansiDim, "  loading…")
		}
	}

	renderWaiting := func() {
		if opts.omitWaiting {
			return
		}
		if state.Kind == ThreadWaiting {
			section("Waiting on you")
			if q, ok := parseNeedsMeQuestion(b.Notes); ok && r.run == "" {
				parts := splitQuestionParts(q)
				if parts.critical != "" {
					para(ansiRed+ansiBold, "  ", parts.critical)
				}
				if parts.question != "" {
					para(ansiBold, "  ", parts.question)
				}
				if parts.recommendation != "" {
					para(ansiGreen+ansiBold, "  ", "★ Recommendation: "+parts.recommendation)
				}
				rec := q.recommendation()
				for _, o := range q.Options {
					st, suf := "", ""
					if rec != nil && *rec == o.Number {
						st, suf = ansiGreen+ansiBold, " ★"
					}
					para(st, "  ", fmt.Sprintf("%d. %s%s", o.Number, o.Label, suf))
				}
				for _, det := range parts.details {
					appendWrappedCapped(&out, ansiDim, ansiDim, "  Details: ", "    ", det, w, 3)
				}
				for _, ev := range parts.evidence {
					appendWrappedCapped(&out, ansiDim, ansiDim, "  Evidence: ", "    ", ev, w, 2)
				}
				if q.Class != "" {
					para(ansiDim, "  ", "Class "+q.Class)
				}
				para(ansiDim, "  ", fmt.Sprintf("1-%d pick · f ★rec · t text · A opens the structured question", len(q.Options)))
				return
			}
			if len(notes) == 0 {
				add(ansiDim, "  (no note says what for)")
			} else {
				tail := notes[len(notes)-shown:]
				var clean []string
				for _, n := range tail {
					if !isNonAnswerHousekeepingNote(n) {
						clean = append(clean, n)
					}
				}
				if len(clean) == 0 {
					clean = tail
				}
				for _, n := range clean {
					appendWrappedCapped(&out, "", ansiDim, "  ", "    ", n, w, 4)
				}
			}
			if cmd := r.run; cmd != "" {
				para(ansiRed, "  ", "run: "+cmd+" (p copies)")
			}
			if r.agent != nil && r.agent.Status == "blocked" {
				para(ansiDim, "  ", "A opens the approval prompt (enter focuses pane)")
			} else if why := unanswerable(r.agent); why != "" {
				para(ansiDim, "  ", why)
			} else {
				para(ansiDim, "  ", "A answers "+r.agent.Name)
			}
			return
		}
		section("Latest note")
		if len(notes) == 0 {
			add(ansiDim, "  (none)")
		} else {
			para("", "  ", notes[len(notes)-1])
		}
		if cmd := r.run; cmd != "" {
			para(ansiRed, "  ", "run: "+cmd+" (p copies)")
		}
	}

	renderDone := func() {
		if len(done) == 0 {
			return
		}
		add("", "")
		para(ansiGreen+ansiBold, "", "Done")
		for _, item := range done {
			appendWrappedCapped(&out, ansiGreen, ansiDim, "  ✓ ", "    ", item, w, detailDoneNoteMaxLines)
		}
	}

	renderHistory := func() {
		if len(earlier) == 0 {
			return
		}
		section(fmt.Sprintf("History (%d earlier notes, newest first)", len(earlier)))
		for i := len(earlier) - 1; i >= 0 && i >= len(earlier)-detailHistoryLines; i-- {
			n := earlier[i]
			appendWrappedCapped(&out, historyNoteStyle(n), ansiDim, "  ", "    ", n, w, detailHistoryNoteMaxLines)
		}
		if older := len(earlier) - detailHistoryLines; older > 0 {
			para(ansiDim, "  ", fmt.Sprintf("… %d older (b shows all)", older))
		}
	}

	renderPRs := func() {
		section("PRs")
		if len(r.prs) == 0 {
			add(ansiDim, "  (none known)")
		}
		for _, pr := range r.prs {
			para("", "  ", prSummary(pr))
			if pr.Title != "" {
				para(ansiDim, "    ", pr.Title)
			}
		}
	}

	renderLinear := func() {
		if r.linear == "" {
			return
		}
		section("Linear")
		para("", "  ", r.linear)
		for _, m := range r.moves {
			para(ansiDim, "  ", "expected: "+m)
		}
	}

	renderRuns := func() {
		if len(r.runs) == 0 {
			return
		}
		section("Runs")
		for _, run := range r.runs {
			state := run.Status
			if run.Conclusion != "" {
				state = run.Conclusion
			}
			para("", "  ", run.Workflow+" · "+strings.ToLower(state))
			if run.URL != "" {
				para(ansiDim, "    ", run.URL)
			}
		}
	}

	renderAgent := func() {
		if r.agent == nil {
			return
		}
		a := r.agent
		section(fmt.Sprintf("Agent %s · %s %s", a.Name, a.Kind, a.Status))
		switch {
		case len(d.tail) > 0:
			for _, l := range d.tail {
				add(ansiDim, "  "+l)
			}
		case d.tailErr != "":
			para(ansiRed, "  ", d.tailErr)
		case !d.tailDone:
			add(ansiDim, "  reading…")
		}
	}

	renderInbox := func() {
		if len(d.inbox) == 0 {
			return
		}
		section(fmt.Sprintf("Inbox (%d)", len(d.inbox)))
		for _, it := range d.inbox {
			para("", "  ", fmt.Sprintf("%s [%s] %s", it.At.Local().Format("15:04"), it.Kind, it.Summary))
		}
	}

	if opts.decisionFirst {
		renderWaiting()
		renderDone()
		renderPRs()
		renderLinear()
		renderRuns()
		renderDesc()
		renderHistory()
		renderAgent()
		renderInbox()
		return out
	}

	renderDesc()
	renderWaiting()
	renderDone()
	renderHistory()
	renderPRs()
	renderLinear()
	renderRuns()
	renderAgent()
	renderInbox()
	return out
}

// wrap breaks s into lines of at most w runes at spaces, keeping its own
// line breaks; a word longer than w (a URL) is cut.
func wrap(s string, w int) []string {
	w = max(w, 1)
	var out []string
	for _, p := range strings.Split(s, "\n") {
		line := ""
		for _, word := range strings.Fields(p) {
			for utf8.RuneCountInString(word) > w {
				if line != "" {
					out = append(out, line)
					line = ""
				}
				r := []rune(word)
				out = append(out, string(r[:w]))
				word = string(r[w:])
			}
			switch {
			case line == "":
				line = word
			case utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) <= w:
				line += " " + word
			default:
				out = append(out, line)
				line = word
			}
		}
		out = append(out, line)
	}
	return out
}

// openURL hands u to the desktop's opener without waiting on it.
func openURL(u string) string {
	cmd := exec.Command("xdg-open", u)
	if err := cmd.Start(); err != nil {
		return "xdg-open: " + err.Error()
	}
	go cmd.Wait()
	return "opened " + u
}

// pageBead shows bd show for id in $PAGER (less by default). The caller has
// left raw mode and the alternate screen.
func pageBead(id string) error {
	cmd := exec.Command("sh", "-c", `bd show "$1" | ${PAGER:-less -R}`, "sh", id)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
