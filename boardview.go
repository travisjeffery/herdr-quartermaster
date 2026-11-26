package main

import (
	"fmt"
	"strings"
	"time"
)

// renderBoard is the whole screen for a w×hgt terminal. Each line clears to
// its end instead of the screen clearing first, so a redraw doesn't flicker.
func renderBoard(m *board, w, hgt int) string {
	w, hgt = max(w, 20), max(hgt, 6)
	m.hits.reset(w, hgt)
	layout := effectiveLayout(m.layout, w)
	var lines []string
	add := func(style, text string) { lines = append(lines, style+fit(text, w)+ansiReset) }

	add(ansiBold, m.titleLine())
	if line := filterLine(m.filter); line != "" && m.view != viewGC && m.view != viewEvents && m.view != viewAnswer && m.view != viewQueue {
		add(ansiCyan, line)
	}
	// The bottom three lines: the selected bead's latest note, the status or
	// question being asked, and the key hint.
	rowOffset := len(lines)
	body := hgt - len(lines) - 3
	switch m.view {
	case viewHelp:
		for i, l := range helpLinesFor(m.back) {
			if i < body {
				add("", l)
			}
		}
	case viewGC:
		lines = append(lines, m.gcLines(w, body, rowOffset)...)
	case viewEvents:
		lines = append(lines, m.eventsLines(w, body, rowOffset)...)
	case viewAnswer:
		lines = append(lines, m.answerLines(w, body, rowOffset)...)
	case viewQueue:
		if layout == layoutWide {
			lines = append(lines, m.wideSplitLines(w, body, rowOffset, true)...)
		} else {
			lines = append(lines, m.queueLines(w, body, rowOffset)...)
		}
	default:
		switch {
		case layout == layoutWide:
			lines = append(lines, m.wideSplitLines(w, body, rowOffset, false)...)
		case m.view == viewList && layout == layoutCompact:
			lines = append(lines, m.compactListLines(w, body, rowOffset)...)
		default:
			var detail []styledLine
			if m.view == viewDetail && m.sel() != nil {
				r := m.sel()
				if r.bead.ID != m.scrolledOn {
					m.scroll, m.scrolledOn = 0, r.bead.ID
				}
				detail = append(rpcBoardDetail(*r, m.now()), detailLines(*r, m.details[r.bead.ID], w)...)
				m.scroll = min(m.scroll, max(0, len(detail)-1))
				detail = detail[m.scroll:]
			}
			lines = append(lines, m.listLinesWithHits(w, body, rowOffset, detail)...)
		}
	}
	for len(lines) < hgt-3 {
		lines = append(lines, "")
	}

	preview := ""
	if r := m.sel(); r != nil && m.view == viewList {
		if latest, _ := notesSplit(r.bead.Notes); latest != "" {
			preview = latest
		}
	} else if it := m.queueSel(); it != nil && m.view == viewQueue {
		if it.rec != nil && it.recLabel != "" {
			preview = fmt.Sprintf("★ rec %d: %s  (f follow · ↵ open · z snooze)", *it.rec, it.recLabel)
		} else if it.runCmd != "" {
			preview = "RUN: " + it.runCmd + "  (↵ copy/complete · p copy · z snooze)"
		} else if it.summary != "" {
			preview = it.summary
		}
	} else if r := m.evSel(); r != nil && m.view == viewEvents {
		if r.inboxFile != "" {
			preview = "inbox: " + r.inboxFile + "  (i ack · ↵ jump)"
		} else if r.summary != "" {
			preview = r.summary
		}
	}
	add(ansiDim, preview)
	if a := m.ask; a != nil {
		text := a.prompt
		if a.text {
			text += a.buf + "▏"
		}
		// The end of what's typed stays in view.
		if r := []rune(text); len(r) > w {
			text = "…" + string(r[len(r)-w+1:])
		}
		if !a.text {
			m.recordConfirmHits(len(lines), text, a.keys)
		}
		add(ansiYellow+ansiBold, text)
	} else {
		status := m.status
		if m.view == viewList || m.view == viewDetail {
			status = statusFor(m.rows, m.cur, status)
		}
		add(ansiRed, status)
	}
	add(ansiDim, m.hintFor(w))

	var s strings.Builder
	s.WriteString("\x1b[H")
	for i, l := range lines[:hgt] {
		s.WriteString(l)
		s.WriteString("\x1b[K")
		if i < hgt-1 {
			s.WriteString("\r\n")
		}
	}
	return s.String()
}

// recordConfirmHits records clickable/tappable column spans on a single-key
// confirmation line (such as "…  y/n" or "…  y/n · e +note") for touch screens.
func (m *board) recordConfirmHits(screenRow int, text, keys string) {
	m.hits.confirmRow = screenRow
	runes := []rune(text)
	findToken := func(tok string) (int, int, bool) {
		idx := strings.LastIndex(text, tok)
		if idx < 0 {
			return 0, 0, false
		}
		col := len([]rune(text[:idx]))
		return col, col + len([]rune(tok)), true
	}
	if strings.Contains(keys, "y") {
		if s, e, ok := findToken("y/n"); ok {
			m.hits.confirmKeys["y"] = [2]int{s, s + 2}
			m.hits.confirmKeys["n"] = [2]int{s + 2, min(len(runes), e+1)}
		}
	}
	if strings.Contains(keys, "e") {
		if s, e, ok := findToken("e +note"); ok {
			m.hits.confirmKeys["e"] = [2]int{s, e}
		}
	}
}

// titleLine says how much is open, how much needs the user, and how fresh
// the beads are: a first paint from the cache says so until bd answers.
func (m *board) titleLine() string {
	total, needs := boardCounts(m.all)
	parts := []string{"quartermaster", fmt.Sprintf("%d open", total)}
	if needs > 0 {
		parts = append(parts, fmt.Sprintf("%d need you", needs))
	}
	switch m.view {
	case viewGC:
		parts[0] = "quartermaster tidy"
		parts = parts[:1]
		switch {
		case m.gcLoading:
			parts = append(parts, "looking for leftovers…")
		case m.gcLoaded:
			parts = append(parts, fmt.Sprintf("%d to look at", len(m.gc)))
			if m.gcTook >= time.Second {
				parts = append(parts, fmt.Sprintf("scanned in %.1fs", m.gcTook.Seconds()))
			}
			if n := len(m.gcMarked); n > 0 {
				parts = append(parts, fmt.Sprintf("%d marked", n))
			}
		}
	case viewEvents:
		parts = []string{
			"quartermaster events",
			fmt.Sprintf("%d unread inbox", len(m.data.Inbox)),
			fmt.Sprintf("%d recent", len(m.feed)+len(m.autoRecs)),
		}
		if m.evLoading {
			parts = append(parts, "refreshing…")
		} else if m.evErr != "" {
			parts = append(parts, "events failed: "+m.evErr)
		}
		return strings.Join(parts, " · ")
	case viewQueue:
		parts = []string{
			"quartermaster queue",
			fmt.Sprintf("%d waiting", len(m.queue)),
		}
		if m.queueSnoozed > 0 {
			parts = append(parts, fmt.Sprintf("%d snoozed", m.queueSnoozed))
		}
		if m.layout != layoutAuto {
			parts = append(parts, m.layout.String())
		}
	case viewAnswer:
		parts = []string{"quartermaster answer"}
		if m.ans != nil {
			if m.ans.fromQueue && m.ans.queueTotal > 0 {
				parts = append(parts, queuePositionTag(m.ans.queueIdx, m.ans.queueTotal))
			}
			parts = append(parts, m.ans.beadID)
			switch m.ans.kind {
			case answerKindPrompt:
				parts = append(parts, "approval prompt")
				if m.ans.prompt != nil {
					parts = append(parts, "["+m.ans.prompt.Hash+"]")
				}
			case answerKindRun:
				parts = append(parts, "RUN command")
			default:
				parts = append(parts, "needs_me")
				if m.ans.question != nil && m.ans.question.ClassN >= 0 {
					parts = append(parts, fmt.Sprintf("C%d", m.ans.question.ClassN))
				}
			}
			if m.ans.loading {
				parts = append(parts, "loading…")
			}
		}
		return strings.Join(parts, " · ")
	default:
		if n := len(m.marked); n > 0 {
			parts = append(parts, fmt.Sprintf("%d marked", n))
		}
		if m.layout != layoutAuto {
			parts = append(parts, m.layout.String())
		}
		parts = append(parts, "by "+m.sort.String())
	}
	switch {
	case m.beadsErr != "":
		parts = append(parts, "bd failed: "+m.beadsErr)
	case m.data.BeadsAt.IsZero() && m.beadsLoading:
		parts = append(parts, "loading beads…")
	case m.beadsLoading:
		parts = append(parts, "beads "+strings.TrimSuffix(ago(m.data.BeadsAt, m.now()), " ago")+" old, refreshing…")
	case !m.data.BeadsAt.IsZero():
		parts = append(parts, "beads "+ago(m.data.BeadsAt, m.now()))
	}
	return strings.Join(parts, " · ")
}

func (m *board) hint() string {
	return m.hintFor(100)
}

func (m *board) hintFor(w int) string {
	compact := w <= boardCompactMaxW
	switch {
	case m.ask != nil && m.ask.text:
		if compact {
			return "↵ send  esc cancel"
		}
		return "↵ send  esc cancel  ⌫ ^w ^u edit"
	case m.ask != nil:
		return "press a key above (or tap y/n), anything else cancels"
	case m.filter.typing:
		return "type to filter  ↵ keep  esc clear  ⌫ edit"
	case m.view == viewHelp:
		return "any key returns"
	case m.view == viewGC:
		if compact {
			return "spc mark  C close  d resolve  esc back"
		}
		return "space mark  * all  C close  u release  d resolve  i dismiss  ↵ focus  r rescan  esc back  ? help"
	case m.view == viewEvents:
		if compact {
			return "j/k move  ↵ jump  i ack  esc back"
		}
		return "j/k move  ↵ jump to bead  i ack inbox  r refresh  E/esc back  ? help"
	case m.view == viewQueue:
		if compact {
			return "↵ open  f ★rec  z snooze  H all  Q board  ?"
		}
		return "↵/A answer  f ★follow rec  z snooze  H snoozed  o PR  @ coord  L layout  Q board  ? help"
	case m.view == viewAnswer:
		if m.ans != nil && m.ans.kind == answerKindPrompt {
			if compact {
				return "1-9 pick  ]/[ next  r reload  esc back"
			}
			return "1-9 pick option  ]/[ queue  j/k scroll  r reload  esc back  ? help"
		}
		if m.ans != nil && m.ans.kind == answerKindRun {
			if compact {
				return "c copy  d done  z snooze  ]/[ next  esc"
			}
			return "c copy cmd  d/↵ record RAN & resume  z snooze  ]/[ queue  esc back  ? help"
		}
		if compact {
			return "f ★rec  1-9 pick  t text  z snooze  ]/[  esc"
		}
		return "f/↵ ★rec  1-9 pick (e +note)  t custom  z snooze  ]/[ queue  r reload  esc back  ? help"
	case m.view == viewDetail:
		if compact {
			return "J/K scroll  A answer  f ★rec  z snooze  esc"
		}
		return "J/K scroll  V RPC view  a note  A answer  f ★rec  z snooze  o PR  l Linear  b bd show  esc back"
	}
	if compact {
		return "Q queue  A act  f ★rec  z snooze  N new  @ coord  ?"
	}
	return "Q queue  ↵ focus/start  space detail  A answer  f ★rec  z snooze  D role  E events  N new  @ coord  L layout  ? help"
}

// agentGlyph is the agent column: working, idle or done, waiting on an
// approval, or none.
func agentGlyph(a *Agent) (string, string) {
	switch {
	case a == nil:
		return " ", ""
	case a.Status == "working":
		return "●", ansiGreen
	case a.Status == "blocked":
		return "!", ansiRed
	default:
		return "○", ansiDim
	}
}

// listLinesWithHits renders the classic 1-line list (and optional stacked
// detail pane) while recording screen-row hit targets for mouse/touch selection.
func (m *board) listLinesWithHits(w, body, rowOffset int, detail ...[]styledLine) []string {
	var det []styledLine
	if len(detail) > 0 {
		det = detail[0]
	}
	lines := listLines(m.rows, m.cur, m.marked, det, m.now(), w, body)
	listH := body
	if det != nil {
		listH = min(len(m.rows), max(3, body/3))
	}
	start := 0
	if m.cur >= listH {
		start = m.cur - listH + 1
	}
	if start > 0 && start == m.cur && m.rows[start-1].header != "" {
		start--
	}
	for i := start; i < len(m.rows) && i-start < listH; i++ {
		if m.rows[i].header == "" {
			m.hits.recordRow(rowOffset+(i-start), i)
		}
	}
	return lines
}

// listLines is the list, scrolled to keep the selection in view, and below it
// the detail pane when one is open: the list then gets a third of the body.
func listLines(rows []boardRow, cur int, marked map[string]bool, detail []styledLine, now time.Time, w, body int) []string {
	var out []string
	if len(rows) == 0 {
		return []string{ansiDim + fit("  nothing open", w) + ansiReset}
	}
	listH := body
	if detail != nil {
		listH = min(len(rows), max(3, body/3))
	}
	start := 0
	if cur >= listH {
		start = cur - listH + 1
	}
	// Keep a section's header in view when its first row is selected.
	if start > 0 && start == cur && rows[start-1].header != "" {
		start--
	}
	idW := 6
	for _, r := range rows {
		idW = max(idW, len([]rune(r.bead.ID)))
	}
	idW = min(idW, 18)
	// marker, id, priority, agent, age: "  backend-ab12 P1 ● 12m "
	fixed := 2 + idW + 1 + 2 + 1 + 1 + 1 + 3 + 1
	stateW := min(40, max(14, (w-fixed)/2))
	titleW := w - fixed - stateW - 1
	if titleW < 10 {
		stateW, titleW = max(0, w-fixed), 0
	}
	for i := start; i < len(rows) && i-start < listH; i++ {
		r := rows[i]
		if r.header != "" {
			out = append(out, ansiBold+groupColor(r.group)+fit(fmt.Sprintf("%s (%d)", r.header, r.count), w)+ansiReset)
			continue
		}
		glyph, gColor := agentGlyph(r.agent)
		if r.native != nil {
			r.line = rpcViewOneLine(rpcBoardBrief(*r.native, now))
			r.bead.Title = rpcViewOneLine(r.bead.Title)
			if r.linear != "" && !strings.Contains(r.bead.Title, r.linear) {
				r.bead.Title = r.linear + " " + r.bead.Title
			}
			glyph, gColor = "○", ansiDim
			if r.native.State == "running" {
				glyph, gColor = "●", ansiGreen
			}
			if r.native.State == "waiting on approval" {
				glyph, gColor = "!", ansiRed
			}
		}
		when := fmt.Sprintf("%3s", age(r.bead.UpdatedAt, now))
		title := ""
		if titleW > 0 {
			title = " " + rpcViewFit(r.bead.Title, titleW)
		}
		head := fit(r.bead.ID, idW) + " " + fmt.Sprintf("P%d", r.bead.Priority) + " "
		isMarked := marked[r.bead.ID]
		if i == cur {
			prefix := "▌ "
			if isMarked {
				prefix = "▌*"
			}
			out = append(out, ansiInvert+rpcViewFit(prefix+head+glyph+" "+when+" "+rpcViewFit(r.line, stateW)+title, w)+ansiReset)
			continue
		}
		prefix := "  "
		if isMarked {
			prefix = ansiYellow + ansiBold + "* " + ansiReset
		}
		out = append(out, prefix+head+gColor+glyph+ansiReset+" "+ansiDim+when+ansiReset+" "+r.color+rpcViewFit(r.line, stateW)+ansiReset+title)
	}
	if detail != nil {
		out = append(out, ansiDim+strings.Repeat("─", w)+ansiReset)
		room := body - len(out)
		if len(detail) > room && room > 0 {
			detail = append(detail[:room-1:room-1], styledLine{"… more below (J scrolls, b shows the bead in full)", ansiDim})
		}
		for i, l := range detail {
			if i >= room {
				break
			}
			out = append(out, l.style+rpcViewFit(l.text, w)+ansiReset)
		}
	}
	return out
}

// gcLines is the tidy view: each item's line, then its evidence, so nothing
// is acted on without its reasons in sight.
func (m *board) gcLines(w, body, rowOffset int) []string {
	switch {
	case m.gcErr != "":
		return []string{ansiRed + fit("  "+m.gcErr, w) + ansiReset}
	case !m.gcLoaded:
		return []string{ansiDim + fit("  reading closed beads, worktrees and the inbox…", w) + ansiReset}
	case len(m.gc) == 0:
		return []string{ansiDim + fit("  nothing to tidy: no stale claims, flagged beads or leftovers", w) + ansiReset}
	}
	type block struct{ lines []string }
	var blocks []block
	for i, it := range m.gc {
		mark := "[ ]"
		if m.gcMarked[it.bead.ID] {
			mark = "[x]"
		}
		var acts []string
		for _, a := range []struct {
			ok   bool
			name string
		}{{it.canClose, "C close"}, {it.canRelease, "u release"}, {it.canResolve, "d resolve"}, {it.canDismiss, "i dismiss"}, {it.agent != "", "↵ focus"}} {
			if a.ok {
				acts = append(acts, a.name)
			}
		}
		head := fmt.Sprintf("%s %s %s · %s", mark, it.bead.ID, strings.Join(it.kinds, ", "), it.bead.Title)
		var b block
		if i == m.gcCur {
			b.lines = append(b.lines, ansiInvert+fit(head, w)+ansiReset)
		} else {
			b.lines = append(b.lines, ansiBold+fit(head, w)+ansiReset)
		}
		for _, ev := range it.evidence {
			for j, l := range wrap(ev, w-6) {
				if j >= 2 {
					break
				}
				b.lines = append(b.lines, ansiDim+fit("    "+l, w)+ansiReset)
			}
		}
		if len(acts) > 0 {
			b.lines = append(b.lines, ansiCyan+fit("    "+strings.Join(acts, " · "), w)+ansiReset)
		}
		blocks = append(blocks, b)
	}
	// Scroll by whole items so the selected one is shown entire.
	first := 0
	for {
		n := 0
		for i := first; i <= m.gcCur && i < len(blocks); i++ {
			n += len(blocks[i].lines)
		}
		if n <= body || first >= m.gcCur {
			break
		}
		first++
	}
	var out []string
	for idx, b := range blocks[first:] {
		itemIdx := first + idx
		for _, l := range b.lines {
			if len(out) < body {
				m.hits.recordRow(rowOffset+len(out), itemIdx)
				out = append(out, l)
			}
		}
	}
	return out
}

func helpLines(gc bool) []string {
	if gc {
		return helpLinesFor(viewGC)
	}
	return helpLinesFor(viewList)
}

func helpLinesFor(v boardView) []string {
	switch v {
	case viewGC:
		return []string{
			"Tidy: what looks finished, with the evidence for each.",
			"",
			"  j/k ↑/↓      move                 space   mark or unmark",
			"  *            mark all / none      r       look again",
			"  C            close with a reason (asks, then confirms)",
			"  u            release a stale claim: a note, then status open",
			"  d            quartermaster resolve: remove the worktree, never --force",
			"  i            mark the item's inbox entry handled",
			"  ↵            focus a leftover agent",
			"  esc, T       back to the board",
			"",
			"Actions apply to the marked items, or the selected one when none are.",
		}
	case viewEvents:
		return []string{
			"Events / Inbox: unread inbox/ files and recent notify/events.json + AUTO: steps.",
			"",
			"  j/k ↑/↓      move                 ^d/^u   page        g/G  top/bottom",
			"  ↵            jump to the event's bead on the main board",
			"  i            acknowledge the selected unread inbox file",
			"  r            refresh inbox and recent events now",
			"  esc, q, E    back to the board",
		}
	case viewQueue:
		return []string{
			"Needs You Decision Queue: every item waiting on your input, prioritized.",
			"Launch directly with `quartermaster board --queue` (ideal for mobile/Moshi).",
			"",
			"  j/k ↑/↓      move                 J/K   scroll right preview (wide desktop)",
			"  ↵, A, space  open full prompt / question / RUN card (auto-advances after answer)",
			"  f            follow the worker's ★ recommendation in one confirm (e adds a note)",
			"  z            snooze (2h, 4h, 1d, 3d, 1w, custom, or clear)",
			"  H            show/hide snoozed items in queue",
			"  p / y        copy RUN command / bead id (OSC 52 works over SSH)",
			"  o / l        open PR / Linear (or copy URL over SSH via OSC 52)",
			"  L            cycle layout: auto, compact (<80), standard, wide (>=130 split)",
			"  Q, esc       return to full board",
		}
	case viewAnswer:
		return []string{
			"Answer view: full approval prompt, structured needs_me question, or RUN command.",
			"",
			"  f, ↵         follow the stated ★ recommendation (needs_me with recommendation)",
			"  1-9          pick a numbered option (press y to confirm, or e to attach a note)",
			"               approval prompts verify --expect <hash> and refuse on mismatch;",
			"               needs_me records NEEDS_ME ANSWERED, sets in_progress, prompts worker",
			"  t            enter a custom free-text answer (needs_me questions only)",
			"  c / d        on RUN cards: copy command (OSC 52) / record RAN: & resume worker",
			"  z            snooze this bead       ]/[   next/prev item in Decision Queue",
			"  j/k, J/K     scroll                 r     reload live prompt / question",
			"  esc, q       back",
		}
	}
	return []string{
		"Every open bead, by what it needs. Agents refresh every 2s, beads every 30s",
		"and after each action; the first paint is the last board's beads.",
		"",
		"  Q         Needs You Decision Queue (or launch `quartermaster board --queue`)",
		"  j/k ↑/↓   move           ^d/^u  page       g/G   top/bottom",
		"  ]/[ tab   next/prev section                 :     go to a bead id",
		"  /         filter (every word, any field)    s     sort: priority, recent, oldest",
		"  space v   detail pane    J/K  scroll it     b     bd show in a pager",
		"  L         cycle layout (auto / compact 2-line / standard / wide split pane)",
		"  ; M       mark/unmark row                   *     toggle/clear section marks",
		"  esc q     close pane, clear filter/marks, or quit",
		"  r         refresh now    T    tidy view     E     live events / inbox view",
		"",
		"  ↵         view native RPC history; otherwise focus/start (confirms; r picks role)",
		"  V         read-only native history (including parked/stopped workers)",
		"  c x m i t start claude, codex, muse, pi or agy (confirms; r picks role; bulk)",
		"  D         start with a [roles.<name>] role picker (defaults to role: label)",
		"  R         resume exited agent(s) (confirms; a or c/x/m/i/t overrides agent)",
		"  a         add a note                        A   answer prompt / needs_me / RUN",
		"  f         follow ★ recommendation           z   snooze bead (2h/4h/1d/3d/1w)",
		"  n         toggle next (single or marked)    S   set status         P  priority",
		"  N         create top-level bead (repo/role) +   create child task  @  ask coord",
		"  U 0-9     autonomy level: U cycles 2 4 7, a digit sets it (confirms; 8 and 9",
		"            ask how long); the worker is told when idle. L4 on a row is its level",
		"  C         close --by TJ (single or marked)  d   resolve worktree (never --force)",
		"  o l       open PR / Linear (OSC 52 over SSH) y p copy id / RUN command (OSC 52)",
		"",
		"Mouse/touch (SGR 1006): tap/click rows or answer options, wheel to scroll.",
		"Approval prompts verify --expect <hash> --by TJ; native RPC viewing is read-only.",
	}
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
