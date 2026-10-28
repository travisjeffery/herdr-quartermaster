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
	detailDescLines    = 10
	detailHistoryLines = 12
	detailTailLines    = 10
	detailTimeout      = 5 * time.Second
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
// waits on: the last two (a question and its options, or a RUN: and its
// why), reaching back up to four for the latest note that asks something,
// since a worker often notes progress after posing the question.
func waitingNotes(notes []string) int {
	n := min(2, len(notes))
	for i := len(notes) - 1; i >= 0 && i >= len(notes)-4; i-- {
		if asks(notes[i]) {
			return max(n, len(notes)-i)
		}
	}
	return n
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

// detailLines renders the pane for row r in width w. d holds whatever of the
// slow lookups has finished.
func detailLines(r boardRow, d *beadDetail, w int) []styledLine {
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
		for _, l := range wrap(text, w-len(indent)) {
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

	section("Description")
	switch {
	case d.full != nil && strings.TrimSpace(b.Description) == "":
		add(ansiDim, "  (none)")
	case d.full != nil:
		lines := wrap(strings.TrimSpace(b.Description), w-2)
		for _, l := range lines[:min(len(lines), detailDescLines)] {
			add("", "  "+l)
		}
		if more := len(lines) - detailDescLines; more > 0 {
			para(ansiDim, "  ", fmt.Sprintf("… %d more lines (b shows all)", more))
		}
	case d.fullErr != "":
		para(ansiRed, "  ", d.fullErr)
	default:
		add(ansiDim, "  loading…")
	}

	notes := noteLines(b.Notes)
	shown := 1
	if b.Status == StatusNeedsMe {
		shown = waitingNotes(notes)
		section("Waiting on you")
		if len(notes) == 0 {
			add(ansiDim, "  (no note says what for)")
		}
		for _, n := range notes[len(notes)-shown:] {
			para("", "  ", n)
		}
		if cmd := r.run; cmd != "" {
			para(ansiRed, "  ", "run: "+cmd+" (p copies)")
		}
		if why := unanswerable(r.agent); why != "" {
			para(ansiDim, "  ", why)
		} else {
			para(ansiDim, "  ", "A answers "+r.agent.Name)
		}
	} else {
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

	if earlier := notes[:max(0, len(notes)-shown)]; len(earlier) > 0 {
		section(fmt.Sprintf("History (%d earlier notes, newest first)", len(earlier)))
		for i := len(earlier) - 1; i >= 0 && i >= len(earlier)-detailHistoryLines; i-- {
			para(ansiDim, "  ", earlier[i])
		}
		if older := len(earlier) - detailHistoryLines; older > 0 {
			para(ansiDim, "  ", fmt.Sprintf("… %d older (b shows all)", older))
		}
	}

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

	if r.linear != "" {
		section("Linear")
		para("", "  ", r.linear)
		for _, m := range r.moves {
			para(ansiDim, "  ", "expected: "+m)
		}
	}

	if len(r.runs) > 0 {
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

	if r.agent != nil {
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

	if len(d.inbox) > 0 {
		section(fmt.Sprintf("Inbox (%d)", len(d.inbox)))
		for _, it := range d.inbox {
			para("", "  ", fmt.Sprintf("%s [%s] %s", it.At.Local().Format("15:04"), it.Kind, it.Summary))
		}
	}
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
