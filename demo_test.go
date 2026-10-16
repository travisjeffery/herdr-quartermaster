package main

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestDemoScreenshots draws the board for the README from made-up data: a
// small open-source notes app, its beads, agents and PRs. It runs the real
// layout (boardRows, renderBoard, detailLines) and replays the terminal output
// into SVG, so the pictures can't drift from the board. Nothing is read from
// bd, herdr or GitHub. scripts/demo/screenshots.sh runs it and makes the PNGs.
func TestDemoScreenshots(t *testing.T) {
	dir := os.Getenv("QM_DEMO_DIR")
	if dir == "" {
		t.Skip("set QM_DEMO_DIR to write the demo screenshots (scripts/demo/screenshots.sh)")
	}
	threads, next, ready, st := demoData()
	rows := boardRows(Config{}, st, threads, next, ready)

	const w = 100
	// The list, with the thread that waits on a command selected: its command
	// shows in the status line.
	list := renderBoard(rows, 2, "", boardFilter{}, nil, w, 24)
	writeDemo(t, dir, "board", list, w, 24)

	// The detail pane on the thread with a question for you.
	r := rows[1]
	d := &beadDetail{full: demoFull(r.bead), tail: demoTail, tailDone: true, inbox: demoInbox[:1]}
	detail := renderBoard(rows, 1, "", boardFilter{}, detailLines(r, d, w), w, 46)
	writeDemo(t, dir, "board-detail", detail, w, 46)
}

func writeDemo(t *testing.T, dir, name, screen string, w, h int) {
	t.Helper()
	svg := screenSVG(replay(screen, w, h), "quartermaster board")
	if err := os.WriteFile(filepath.Join(dir, name+".svg"), []byte(svg), 0o644); err != nil {
		t.Fatal(err)
	}
}

// demoAt is the made-up afternoon the demo happens on.
var demoAt = time.Date(2026, 3, 12, 14, 0, 0, 0, time.Local)

const demoRepo = "https://github.com/acme/notes/pull/"

func demoPR(n int, title, state string) *PR {
	return &PR{Number: n, Title: title, URL: demoRepo + strconv.Itoa(n), State: state, HeadSHA: "c0ffee" + strconv.Itoa(n)}
}

func demoAgent(id, kind, status string) *Agent {
	return &Agent{Name: id, Kind: kind, Status: status}
}

func demoData() (threads []Thread, next, ready []Bead, st TickerState) {
	bead := func(id, title, status string, labels ...string) Bead {
		return Bead{ID: id, Title: title, Status: status, Priority: 2, Type: "task", Labels: labels, UpdatedAt: demoAt}
	}

	question := bead("notes-7qk", "Add rate limiting to the API", StatusNeedsMe)
	question.Priority = 1
	question.Notes = strings.Join([]string{
		"Linear: APP-14",
		"Token bucket in internal/ratelimit with tests; not wired into the router yet.",
		"Draft PR: " + demoRepo + "50",
		"Question: limit per API key or per client IP? (a) per key, 100 req/min, " +
			"anonymous calls share one bucket (b) per IP, 60 req/min (c) both, whichever trips first.",
	}, "\n")
	draft := demoPR(50, "APP-14: Add rate limiting to the API", "OPEN")
	draft.IsDraft = true
	draft.MergeState = "CLEAN"
	draft.Checks = []Check{{Name: "test", Conclusion: "SUCCESS", Status: "COMPLETED"}, {Name: "lint", Conclusion: "SUCCESS", Status: "COMPLETED"}}

	rotate := bead("notes-m3d", "Rotate the staging database password", StatusNeedsMe)
	rotate.Notes = "RUN: make rotate-db-password ENV=staging\n" +
		"It needs the vault login only you have; the app picks the new secret up on its next deploy."

	ready2 := demoPR(45, "APP-11: Upgrade the Postgres driver to v5", "OPEN")
	ready2.ReviewDecision = "APPROVED"
	ready2.MergeState = "CLEAN"
	ready2.Gate = &MergeGate{Head: ready2.HeadSHA, At: demoAt}
	ready2.Reviews = []Review{{State: "APPROVED", SubmittedAt: demoAt.Add(-time.Hour)}}
	ready2.Reviews[0].Commit.OID = ready2.HeadSHA

	review := demoPR(47, "Write the v2 migration guide", "OPEN")
	review.ReviewDecision = "REVIEW_REQUIRED"
	review.MergeState = "BLOCKED"

	failing := demoPR(48, "APP-17: Dark mode for the settings page", "OPEN")
	failing.MergeState = "UNSTABLE"

	running := demoPR(49, "Cache rendered Markdown previews", "OPEN")
	running.IsDraft = true

	threads = []Thread{
		{Bead: question, Agent: demoAgent(question.ID, "claude", "idle"), PR: draft, Checks: Checks{Passing: 2}, Linear: "APP-14"},
		{Bead: rotate, Agent: demoAgent(rotate.ID, "codex", "idle")},
		{Bead: bead("notes-2fz", "Fix the flaky login test", StatusInProgress), Agent: demoAgent("notes-2fz", "claude", "idle"),
			PR: demoPR(42, "APP-9: Fix the flaky login test", "MERGED"), Linear: "APP-9"},
		{Bead: bead("notes-9wc", "Upgrade the Postgres driver to v5", StatusInProgress), Agent: demoAgent("notes-9wc", "claude", "idle"),
			PR: ready2, Checks: Checks{Passing: 6}, Linear: "APP-11"},
		{Bead: bead("notes-r8t", "Write the v2 migration guide", StatusInProgress), Agent: demoAgent("notes-r8t", "muse", "idle"),
			PR: review, Checks: Checks{Passing: 3}},
		{Bead: bead("notes-x5n", "Move image uploads to object storage", StatusInProgress, LabelRollingOut),
			Agent: demoAgent("notes-x5n", "claude", "idle"), PR: demoPR(39, "Move image uploads to object storage", "MERGED"),
			Runs: []Run{{Workflow: "Deploy production", Status: "in_progress"}}},
		{Bead: bead("notes-h6p", "Dark mode for the settings page", StatusInProgress), Agent: demoAgent("notes-h6p", "codex", "idle"),
			PR: failing, Checks: Checks{Failing: []string{"e2e (firefox)"}, Passing: 5}, Linear: "APP-17"},
		{Bead: bead("notes-c4j", "Paginate the search endpoint", StatusInProgress), Agent: demoAgent("notes-c4j", "claude", "working")},
		{Bead: bead("notes-u2e", "Cache rendered Markdown previews", StatusInProgress), Agent: demoAgent("notes-u2e", "muse", "working"),
			PR: running, Checks: Checks{Pending: 4, Passing: 2}},
		{Bead: bead("notes-b7v", "Translate the onboarding emails", StatusInProgress), Agent: demoAgent("notes-b7v", "codex", "idle")},
		{Bead: bead("notes-k9s", "Single sign-on for the admin panel", StatusBlocked)},
	}

	next = []Bead{
		{ID: "notes-d1f", Title: "CSV export for reports", Status: StatusOpen, Priority: 1, Type: "feature", Labels: []string{LabelNext}},
		{ID: "notes-p5g", Title: "Invoices show the wrong time zone", Status: StatusOpen, Priority: 2, Type: "bug", Labels: []string{LabelNext}},
	}
	ready = []Bead{
		{ID: "notes-w8m", Title: "Webhooks for new comments", Status: StatusOpen, Priority: 2, Type: "feature"},
		{ID: "notes-e3r", Title: "Remove feature flags that shipped", Status: StatusOpen, Priority: 3, Type: "chore"},
		{ID: "notes-s6y", Title: "Document the release process", Status: StatusOpen, Priority: 3, Type: "task"},
	}
	st = TickerState{PRs: map[string]PR{question.ID: *draft}}
	return threads, next, ready, st
}

// demoFull is the bead as bd show has it, with its description.
func demoFull(b Bead) *Bead {
	b.Description = "One noisy client can slow the API down for everyone. Add a limiter in front of /api " +
		"that answers 429 with Retry-After, and document the limits."
	b.Labels = []string{"api"}
	return &b
}

// demoTail is the end of the worker's screen.
var demoTail = []string{
	"● The limiter is in internal/ratelimit (token bucket, 14 tests), and the draft",
	"  PR is up. Before I wire it into the router I need one decision from you:",
	"  should the limit be per API key, per client IP, or both? Noted on the bead.",
	"❯",
}

var demoInbox = []InboxItem{
	{Event: Event{Bead: "notes-7qk", Kind: EventNeedsYou, At: demoAt.Add(-4 * time.Minute),
		Summary: "notes-7qk needs you: limit per API key or per client IP?"}},
}

// cell is one character on the replayed screen and the SGR state it was
// drawn in.
type cell struct {
	r                 rune
	bold, dim, invert bool
	fg                int // 30-37, 0 for the default
}

// replay runs the board's escape sequences against a w×h screen: the few it
// writes are cursor home, clear, cursor position and SGR.
func replay(s string, w, h int) [][]cell {
	scr := make([][]cell, h)
	for i := range scr {
		scr[i] = make([]cell, w)
		for j := range scr[i] {
			scr[i][j].r = ' '
		}
	}
	var cur cell
	row, col := 0, 0
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == 0x1b && i+1 < len(s) && s[i+1] == '[':
			j := i + 2
			for j < len(s) && (s[j] == ';' || s[j] == '?' || s[j] >= '0' && s[j] <= '9') {
				j++
			}
			params, cmd := s[i+2:j], s[j]
			i = j + 1
			switch cmd {
			case 'H':
				row, col = 0, 0
				if r, c, ok := strings.Cut(params, ";"); ok {
					row, _ = strconv.Atoi(r)
					col, _ = strconv.Atoi(c)
					row, col = row-1, col-1
				}
			case 'm':
				for p := range strings.SplitSeq(params, ";") {
					switch n, _ := strconv.Atoi(p); {
					case n == 0:
						cur = cell{}
					case n == 1:
						cur.bold = true
					case n == 2:
						cur.dim = true
					case n == 7:
						cur.invert = true
					case n >= 30 && n <= 37:
						cur.fg = n
					}
				}
			}
		case c == '\r':
			col = 0
			i++
		case c == '\n':
			row++
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			i += size
			if row >= 0 && row < h && col >= 0 && col < w {
				cur.r = r
				scr[row][col] = cur
			}
			col++
		}
	}
	return scr
}

// The palette: Tokyo Night.
var (
	demoBg     = "#1a1b26"
	demoFg     = "#c0caf5"
	demoDim    = "#565f89"
	demoChrome = "#16161e"
	demoColors = map[int]string{31: "#f7768e", 32: "#9ece6a", 33: "#e0af68", 36: "#7dcfff"}
)

const (
	cellW   = 8.4 // 0.6em: a monospace advance at 14px
	cellH   = 20.0
	padX    = 20.0
	padY    = 16.0
	chromeH = 34.0
)

// screenSVG draws the screen in a window with a title bar. Each run of
// same-styled text is placed at its column and stretched to its cells, so a
// fallback font can't shift the columns.
func screenSVG(scr [][]cell, title string) string {
	w, h := len(scr[0]), len(scr)
	width := float64(w)*cellW + 2*padX
	height := chromeH + float64(h)*cellH + 2*padY
	var s strings.Builder
	fmt.Fprintf(&s, `<svg xmlns="http://www.w3.org/2000/svg" width="%g" height="%g" viewBox="0 0 %g %g">`+"\n", width, height, width, height)
	fmt.Fprintf(&s, `<rect width="%g" height="%g" rx="10" fill="%s"/>`+"\n", width, height, demoBg)
	fmt.Fprintf(&s, `<path d="M0 10a10 10 0 0 1 10-10h%g a10 10 0 0 1 10 10v%gh-%gz" fill="%s"/>`+"\n", width-20, chromeH-10, width, demoChrome)
	for i, c := range []string{"#ff5f57", "#febc2e", "#28c840"} {
		fmt.Fprintf(&s, `<circle cx="%g" cy="%g" r="6" fill="%s"/>`+"\n", 20+float64(i)*20, chromeH/2, c)
	}
	fmt.Fprintf(&s, `<text x="%g" y="%g" fill="%s" font-family="ui-sans-serif, -apple-system, 'Segoe UI', Helvetica, Arial, sans-serif" font-size="13" text-anchor="middle">%s</text>`+"\n",
		width/2, chromeH/2+4.5, demoDim, html.EscapeString(title))
	fmt.Fprintf(&s, `<g font-family="'JetBrains Mono', 'JetBrainsMono Nerd Font', ui-monospace, SFMono-Regular, Menlo, Consolas, 'DejaVu Sans Mono', monospace" font-size="14" xml:space="preserve">`+"\n")
	for y, line := range scr {
		top := chromeH + padY + float64(y)*cellH
		for x := 0; x < w; {
			st := line[x]
			end := x + 1
			for end < w && sameStyle(line[end], st) {
				end++
			}
			text := strings.TrimRight(runText(line[x:end]), " ")
			fg, bg := demoFg, ""
			switch {
			case st.fg != 0:
				fg = demoColors[st.fg]
			case st.dim:
				fg = demoDim
			}
			if st.invert {
				fg, bg = demoBg, demoFg
				text = runText(line[x:end])
			}
			if bg != "" {
				fmt.Fprintf(&s, `<rect x="%g" y="%g" width="%g" height="%g" fill="%s"/>`+"\n", padX+float64(x)*cellW, top, float64(end-x)*cellW, cellH, bg)
			}
			if n := utf8.RuneCountInString(text); strings.TrimSpace(text) != "" {
				weight := ""
				if st.bold {
					weight = ` font-weight="bold"`
				}
				fmt.Fprintf(&s, `<text x="%g" y="%g" fill="%s"%s textLength="%g" lengthAdjust="spacingAndGlyphs">%s</text>`+"\n",
					padX+float64(x)*cellW, top+cellH*0.72, fg, weight, float64(n)*cellW, html.EscapeString(text))
			}
			x = end
		}
	}
	s.WriteString("</g>\n</svg>\n")
	return s.String()
}

func sameStyle(a, b cell) bool {
	a.r, b.r = 0, 0
	return a == b
}

func runText(cells []cell) string {
	var b strings.Builder
	for _, c := range cells {
		b.WriteRune(c.r)
	}
	return b.String()
}
