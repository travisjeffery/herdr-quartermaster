package main

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

type boardRow struct {
	header string
	bead   Bead
	agent  *Agent
	line   string
	color  string
	run    string
	// search is the lowercased text '/' matches: id, title, agent, state,
	// PR, Linear key and the section the row sits in.
	search string
	// prs, linear and moves are for the detail pane: the bead's PRs as the
	// ticker last saw them, its Linear key and where that issue should be.
	prs    []PR
	linear string
	moves  []string
}

func searchText(parts ...string) string {
	return strings.ToLower(strings.Join(parts, "\x00"))
}

const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[2m"
	ansiBold   = "\x1b[1m"
	ansiRed    = "\x1b[31m"
	ansiYellow = "\x1b[33m"
	ansiGreen  = "\x1b[32m"
	ansiCyan   = "\x1b[36m"
	ansiInvert = "\x1b[7m"
)

func groupColor(g Group) string {
	switch g {
	case GroupNeedsYou, GroupFailing:
		return ansiRed
	case GroupReview, GroupMerged, GroupRollingOut:
		return ansiYellow
	case GroupWorking, GroupReady:
		return ansiGreen
	default:
		return ansiDim
	}
}

// loadBoard builds the rows from live bd and herdr state plus the ticker's
// last PR pass, so opening the board never waits on GitHub.
func loadBoard(cfg Config) ([]boardRow, error) {
	st := loadState()
	t := newTicker(cfg, nil)
	st.LastGH = time.Now() // never call gh from the board
	threads, _, err := t.gather(&st, time.Now())
	if err != nil {
		return nil, err
	}
	next, _ := Beads{}.Next()
	ready, _ := Beads{}.Ready()
	return boardRows(cfg, st, threads, next, ready), nil
}

// boardRows lays out the threads, the beads labelled next and the other ready
// beads as the board's rows.
func boardRows(cfg Config, st TickerState, threads []Thread, next, ready []Bead) []boardRow {
	sort.SliceStable(threads, func(i, j int) bool {
		gi, gj := classify(threads[i]), classify(threads[j])
		if gi != gj {
			return gi < gj
		}
		return threads[i].Bead.ID < threads[j].Bead.ID
	})
	var rows []boardRow
	rows = append(rows, boardRow{header: "threads"})
	for _, th := range threads {
		r := boardRow{bead: th.Bead, agent: th.Agent, line: stateLine(th), color: groupColor(classify(th)), run: runCommand(th),
			prs: linkedPRs(st, th.Bead), linear: th.Linear, moves: linearMoves([]Thread{th})}
		agent, pr := "", ""
		if th.Agent != nil {
			agent = th.Agent.Name
		}
		if th.PR != nil {
			pr = fmt.Sprintf("#%d %s", th.PR.Number, th.PR.URL)
		}
		r.search = searchText("threads", th.Bead.ID, th.Bead.Title, agent, r.line, pr, th.Linear)
		rows = append(rows, r)
	}
	rows = append(rows, boardRow{header: "next"})
	seen := map[string]bool{}
	for _, b := range next {
		seen[b.ID] = true
		line := fmt.Sprintf("P%d %s", b.Priority, b.Type)
		rows = append(rows, boardRow{bead: b, line: line, color: ansiCyan, search: searchText("next", b.ID, b.Title, line),
			prs: linkedPRs(st, b), linear: linearKey(b, cfg.LinearPrefixes)})
	}
	rows = append(rows, boardRow{header: "ready"})
	n := 0
	for _, b := range ready {
		if seen[b.ID] || b.Status != StatusOpen || n >= 25 {
			continue
		}
		n++
		line := fmt.Sprintf("P%d %s", b.Priority, b.Type)
		rows = append(rows, boardRow{bead: b, line: line, color: ansiDim, search: searchText("ready", b.ID, b.Title, line),
			prs: linkedPRs(st, b), linear: linearKey(b, cfg.LinearPrefixes)})
	}
	return rows
}

// filterRows keeps the rows whose search text contains every word of query,
// case-insensitively, and the section headers that still have a row under them.
func filterRows(rows []boardRow, query string) []boardRow {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return rows
	}
	var out []boardRow
	var header *boardRow
	for _, r := range rows {
		if r.header != "" {
			header = &r
			continue
		}
		if !matchesAll(r.search, words) {
			continue
		}
		if header != nil {
			out = append(out, *header)
			header = nil
		}
		out = append(out, r)
	}
	return out
}

func matchesAll(s string, words []string) bool {
	for _, w := range words {
		if !strings.Contains(s, w) {
			return false
		}
	}
	return true
}

// boardFilter is the '/' prompt: typing is true while keys edit the query
// rather than drive the board.
type boardFilter struct {
	query  string
	typing bool
}

// key applies one keypress while typing and reports whether the query changed.
// Enter keeps the query and returns to navigation; Esc clears it.
func (f *boardFilter) key(k string) bool {
	switch k {
	case "\r", "\n":
		f.typing = false
		return false
	case "\x1b":
		f.typing = false
		f.query = ""
		return true
	case "\x7f", "\b":
		if r := []rune(f.query); len(r) > 0 {
			f.query = string(r[:len(r)-1])
			return true
		}
		return false
	case "\x15": // ctrl-u
		f.query = ""
		return true
	}
	if r, _ := utf8.DecodeRuneInString(k); len(k) == 0 || !unicode.IsPrint(r) || utf8.RuneCountInString(k) != 1 {
		return false // arrows and other controls
	}
	f.query += k
	return true
}

// splitKeys cuts one read's bytes into keypresses: a read can hold several
// (a paste, fast typing) or end inside a UTF-8 rune, which comes back as rest
// to prefix the next read. Escape sequences (arrows) stay whole; a lone ESC
// is the Esc key.
func splitKeys(b []byte) (keys []string, rest []byte) {
	for len(b) > 0 {
		n := 1
		switch {
		case b[0] == 0x1b && len(b) >= 2 && (b[1] == '[' || b[1] == 'O'):
			n = 2
			if b[1] == '[' {
				for n < len(b) && (b[n] < 0x40 || b[n] > 0x7e) {
					n++
				}
			}
			n = min(n+1, len(b))
		case b[0] >= utf8.RuneSelf:
			if !utf8.FullRune(b) {
				return keys, b
			}
			_, n = utf8.DecodeRune(b)
		}
		keys = append(keys, string(b[:n]))
		b = b[n:]
	}
	return keys, nil
}

func nextSelectable(rows []boardRow, from, dir int) int {
	for i := from; i >= 0 && i < len(rows); i += dir {
		if rows[i].header == "" {
			return i
		}
	}
	return -1
}

// waitInput waits up to timeout ms (-1: forever) for a key on fd. Polling
// rather than a reader goroutine leaves stdin alone while a pager has it.
func waitInput(fd, timeout int) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, timeout)
		if err == unix.EINTR {
			continue
		}
		return n > 0, err
	}
}

func runBoard(cfg Config) error {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, old)
	fmt.Print("\x1b[?1049h\x1b[?25l")
	defer fmt.Print("\x1b[?25h\x1b[?1049l")

	// The same server loadBoard reads, so focus and dispatch act on the agents shown.
	h := newHerdr().on(cfg.coordSocket())
	all, err := loadBoard(cfg)
	status := ""
	if err != nil {
		status = err.Error()
	}
	// rows is what's shown and what selection and actions index: all, narrowed
	// by the '/' filter.
	var filter boardFilter
	rows := all
	cur := nextSelectable(rows, 0, 1)
	refilter := func() {
		rows = filterRows(all, filter.query)
		cur = nextSelectable(rows, 0, 1)
	}
	// The detail pane, while open, shows the selected bead and follows the
	// selection. details caches each bead's slow lookups until a refresh;
	// pending counts those still running.
	showDetail := false
	// scroll is how far the pane is scrolled down; it starts over on each bead.
	scroll, scrolledOn := 0, ""
	details := map[string]*beadDetail{}
	results := make(chan detailResult, 16)
	pending := 0
	sel := func() *boardRow {
		if cur < 0 || cur >= len(rows) {
			return nil
		}
		return &rows[cur]
	}
	detail := func() *beadDetail {
		r := sel()
		if !showDetail || r == nil {
			return nil
		}
		d, ok := details[r.bead.ID]
		if !ok {
			d = &beadDetail{inbox: beadInbox(r.bead.ID)}
			details[r.bead.ID] = d
			pending += loadDetail(h, *r, results)
		}
		return d
	}
	draw := func() {
		var lines []styledLine
		if d := detail(); d != nil {
			if id := sel().bead.ID; id != scrolledOn {
				scroll, scrolledOn = 0, id
			}
			lines = detailLines(*sel(), d, termWidth())
			scroll = min(scroll, len(lines)-1)
			lines = lines[scroll:]
		}
		drawBoard(rows, cur, status, filter, lines)
	}
	buf := make([]byte, 256)
	var keys []string
	var partial []byte
	for {
		if len(keys) == 0 {
			draw()
			timeout := -1
			if pending > 0 {
				timeout = 100
			}
			ready, err := waitInput(fd, timeout)
			if err != nil {
				return err
			}
			for drained := false; !drained; {
				select {
				case res := <-results:
					pending--
					if d, ok := details[res.id]; ok {
						res.apply(d)
					}
				default:
					drained = true
				}
			}
			if !ready {
				continue
			}
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return err
			}
			keys, partial = splitKeys(append(partial, buf[:n]...))
			status = ""
			if len(keys) == 0 {
				continue
			}
		}
		key := keys[0]
		keys = keys[1:]
		if key == "\x03" {
			return nil
		}
		if filter.typing {
			if filter.key(key) {
				refilter()
			}
			continue
		}
		reload := func() {
			if r, err := loadBoard(cfg); err == nil {
				all = r
				rows = filterRows(all, filter.query)
				if cur >= len(rows) || cur < 0 || rows[cur].header != "" {
					cur = nextSelectable(rows, 0, 1)
				}
				// Results still running land on no entry and are dropped.
				details = map[string]*beadDetail{}
			} else {
				status = err.Error()
			}
		}
		if showDetail {
			handled := true
			switch key {
			case "\x1b", "q", " ", "v":
				showDetail = false
			case "J", "\x1b[6~":
				scroll += 5
			case "K", "\x1b[5~":
				scroll = max(0, scroll-5)
			case "o":
				if r := sel(); r != nil && len(r.prs) > 0 {
					status = openURL(r.prs[0].URL)
				} else {
					status = "no PR known"
				}
			case "l":
				if r := sel(); r != nil {
					status = openLinear(cfg, *r)
				}
			case "s":
				if r := sel(); r != nil {
					fmt.Print("\x1b[?25h\x1b[?1049l")
					term.Restore(fd, old)
					if err := pageBead(r.bead.ID); err != nil {
						status = err.Error()
					}
					if _, err := term.MakeRaw(fd); err != nil {
						return err
					}
					fmt.Print("\x1b[?1049h\x1b[?25l")
				}
			default:
				handled = false
			}
			if handled {
				continue
			}
		}
		switch key {
		case " ", "v":
			if sel() != nil {
				showDetail = true
			}
		case "/":
			filter.typing = true
		case "\x1b":
			// Esc clears a kept filter before it closes the board.
			if filter.query == "" {
				return nil
			}
			filter.query = ""
			refilter()
		case "q":
			return nil
		case "j", "\x1b[B":
			if i := nextSelectable(rows, cur+1, 1); i >= 0 {
				cur = i
			}
		case "k", "\x1b[A":
			if i := nextSelectable(rows, cur-1, -1); i >= 0 {
				cur = i
			}
		case "r":
			reload()
		case "\r", "c", "x", "m":
			r := sel()
			if r == nil {
				continue
			}
			if key == "\r" && r.agent != nil {
				_ = h.Focus(r.agent.Name)
				return nil
			}
			kind := ""
			switch key {
			case "c":
				kind = "claude"
			case "x":
				kind = "codex"
			case "m":
				kind = "muse"
			}
			status = "starting " + r.bead.ID + "…"
			drawBoard(rows, cur, status, filter, nil)
			msg, err := dispatch(cfg, h, r.bead.ID, DispatchOpts{Kind: kind, Focus: true})
			if err != nil {
				status = err.Error()
				continue
			}
			_ = msg
			return nil
		case "n":
			if r := sel(); r != nil {
				on := !r.bead.HasLabel(LabelNext)
				if err := (Beads{}).SetLabel(r.bead.ID, LabelNext, on); err != nil {
					status = err.Error()
				}
				reload()
			}
		case "y":
			if r := sel(); r != nil {
				status = copyText(r.bead.ID, "copied "+r.bead.ID)
			}
		case "p":
			if r := sel(); r != nil && r.run != "" {
				status = copyText(r.run, "copied the command")
			}
		}
	}
}

// openLinear opens the row's Linear issue in the browser.
func openLinear(cfg Config, r boardRow) string {
	if r.linear == "" {
		return "no Linear issue known"
	}
	u := linearURL(r.linear, r.bead.Notes, cfg.LinearWorkspace)
	if u == "" {
		return "set linear_workspace in config.toml to open " + r.linear
	}
	return openURL(u)
}

func copyText(text, done string) string {
	if err := exec.Command("wl-copy", text).Run(); err != nil {
		return "wl-copy: " + err.Error()
	}
	return done
}

// statusFor is the status line: the last action's message, else the command
// the selected thread is waiting on the user to run.
func statusFor(rows []boardRow, cur int, status string) string {
	if status != "" || cur < 0 || cur >= len(rows) || rows[cur].run == "" {
		return status
	}
	return "run: " + rows[cur].run + "  (p copies)"
}

func termWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 100
	}
	return w
}

func drawBoard(rows []boardRow, cur int, status string, filter boardFilter, detail []styledLine) {
	w, hgt, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		w, hgt = 100, 30
	}
	fmt.Print(renderBoard(rows, cur, status, filter, detail, w, hgt))
}

// renderBoard is the whole screen for a w×hgt terminal. With detail lines the
// list shrinks to a third of the body, around the selection, and the detail
// pane fills the rest.
func renderBoard(rows []boardRow, cur int, status string, filter boardFilter, detail []styledLine, w, hgt int) string {
	var s strings.Builder
	s.WriteString("\x1b[H\x1b[2J")
	body := hgt - 3
	if line := filterLine(filter); line != "" {
		fmt.Fprintf(&s, "%s%s%s\r\n", ansiCyan, fit(line, w), ansiReset)
		body--
	}
	listH := body
	if detail != nil {
		listH = min(len(rows), max(3, body/3))
	}
	start := 0
	if cur >= listH {
		start = cur - listH + 1
	}
	stateW := min(34, max(12, w/3))
	titleW := w - 2 - 17 - stateW - 1
	for i := start; i < len(rows) && i-start < listH; i++ {
		r := rows[i]
		if r.header != "" {
			fmt.Fprintf(&s, "%s%s%s\r\n", ansiBold, fit(r.header, w), ansiReset)
			continue
		}
		id := fit(r.bead.ID, 16) + " "
		state := fit(r.line, stateW)
		title := ""
		if titleW >= 8 {
			title = " " + fit(r.bead.Title, titleW)
		}
		if i == cur {
			fmt.Fprintf(&s, "%s  %s%s%s%s\r\n", ansiInvert, id, state, title, ansiReset)
		} else {
			fmt.Fprintf(&s, "  %s%s%s%s%s\r\n", id, r.color, state, ansiReset, title)
		}
	}
	if detail != nil {
		fmt.Fprintf(&s, "%s%s%s\r\n", ansiDim, strings.Repeat("─", max(w, 1)), ansiReset)
		room := body - min(listH, len(rows)-start) - 1
		if len(detail) > room && room > 0 {
			detail = append(detail[:room-1:room-1], styledLine{"… more below (s shows the bead in full)", ansiDim})
		}
		for i, l := range detail {
			if i >= room {
				break
			}
			fmt.Fprintf(&s, "%s%s%s\r\n", l.style, fit(l.text, w), ansiReset)
		}
	}
	fmt.Fprintf(&s, "\x1b[%d;1H%s%s%s", hgt-1, ansiRed, fit(statusFor(rows, cur, status), w-1), ansiReset)
	hint := "↵ focus/start  space detail  c claude  x codex  m muse  n next  y copy  p copy command  / filter  r refresh  q quit"
	switch {
	case filter.typing:
		hint = "type to filter  ↵ keep  esc clear  ⌫ edit"
	case detail != nil:
		hint = "j/k move  J/K scroll  o open PR  l Linear  s bd show  ↵ focus/start  esc/space back"
	}
	fmt.Fprintf(&s, "\x1b[%d;1H%s%s%s", hgt, ansiDim, fit(hint, w-1), ansiReset)
	return s.String()
}

// filterLine is the header shown while a filter is being typed or kept.
func filterLine(f boardFilter) string {
	switch {
	case f.typing:
		return "/" + f.query + "▏"
	case f.query != "":
		return "/" + f.query + "  (esc clears)"
	}
	return ""
}

// fit pads or cuts s to exactly n characters (runes, not bytes: the state
// lines carry "·" and "…").
func fit(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) > n {
		if n == 1 {
			return "…"
		}
		return string(r[:n-1]) + "…"
	}
	return s + strings.Repeat(" ", n-len(r))
}
