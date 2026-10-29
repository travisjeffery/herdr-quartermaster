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
	var lines []string
	add := func(style, text string) { lines = append(lines, style+fit(text, w)+ansiReset) }

	add(ansiBold, m.titleLine())
	if line := filterLine(m.filter); line != "" && m.view != viewGC {
		add(ansiCyan, line)
	}
	// The bottom three lines: the selected bead's latest note, the status or
	// question being asked, and the key hint.
	body := hgt - len(lines) - 3
	switch m.view {
	case viewHelp:
		for i, l := range helpLines(m.back == viewGC) {
			if i < body {
				add("", l)
			}
		}
	case viewGC:
		lines = append(lines, m.gcLines(w, body)...)
	default:
		var detail []styledLine
		if m.view == viewDetail && m.sel() != nil {
			r := m.sel()
			if r.bead.ID != m.scrolledOn {
				m.scroll, m.scrolledOn = 0, r.bead.ID
			}
			detail = detailLines(*r, m.details[r.bead.ID], w)
			m.scroll = min(m.scroll, max(0, len(detail)-1))
			detail = detail[m.scroll:]
		}
		lines = append(lines, listLines(m.rows, m.cur, detail, m.now(), w, body)...)
	}
	for len(lines) < hgt-3 {
		lines = append(lines, "")
	}

	preview := ""
	if r := m.sel(); r != nil && m.view == viewList {
		if latest, _ := notesSplit(r.bead.Notes); latest != "" {
			preview = latest
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
		add(ansiYellow+ansiBold, text)
	} else {
		status := m.status
		if m.view == viewList || m.view == viewDetail {
			status = statusFor(m.rows, m.cur, status)
		}
		add(ansiRed, status)
	}
	add(ansiDim, m.hint())

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

// titleLine says how much is open, how much needs the user, and how fresh
// the beads are: a first paint from the cache says so until bd answers.
func (m *board) titleLine() string {
	total, needs := boardCounts(m.all)
	parts := []string{"quartermaster", fmt.Sprintf("%d open", total)}
	if needs > 0 {
		parts = append(parts, fmt.Sprintf("%d need you", needs))
	}
	if m.view == viewGC {
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
	} else {
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
	switch {
	case m.ask != nil && m.ask.text:
		return "↵ send  esc cancel  ⌫ ^w ^u edit"
	case m.ask != nil:
		return "press a key above, anything else cancels"
	case m.filter.typing:
		return "type to filter  ↵ keep  esc clear  ⌫ edit"
	case m.view == viewHelp:
		return "any key returns"
	case m.view == viewGC:
		return "space mark  * all  C close  u release  d resolve  i dismiss  ↵ focus  r rescan  esc back  ? help"
	case m.view == viewDetail:
		return "J/K scroll  a note  A answer  o PR  l Linear  b bd show  ↵ focus/start  esc back  ? help"
	}
	return "↵ focus/start  space detail  a note  A answer  n next  S status  T tidy  / filter  ? help"
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

// listLines is the list, scrolled to keep the selection in view, and below it
// the detail pane when one is open: the list then gets a third of the body.
func listLines(rows []boardRow, cur int, detail []styledLine, now time.Time, w, body int) []string {
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
		when := fmt.Sprintf("%3s", age(r.bead.UpdatedAt, now))
		title := ""
		if titleW > 0 {
			title = " " + fit(r.bead.Title, titleW)
		}
		head := fit(r.bead.ID, idW) + " " + fmt.Sprintf("P%d", r.bead.Priority) + " "
		if i == cur {
			out = append(out, ansiInvert+fit("▌ "+head+glyph+" "+when+" "+fit(r.line, stateW)+title, w)+ansiReset)
			continue
		}
		out = append(out, "  "+head+gColor+glyph+ansiReset+" "+ansiDim+when+ansiReset+" "+r.color+fit(r.line, stateW)+ansiReset+title)
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
			out = append(out, l.style+fit(l.text, w)+ansiReset)
		}
	}
	return out
}

// gcLines is the tidy view: each item's line, then its evidence, so nothing
// is acted on without its reasons in sight.
func (m *board) gcLines(w, body int) []string {
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
	for _, b := range blocks[first:] {
		for _, l := range b.lines {
			if len(out) < body {
				out = append(out, l)
			}
		}
	}
	return out
}

func helpLines(gc bool) []string {
	if gc {
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
	}
	return []string{
		"Every open bead, by what it needs. Agents refresh every 2s, beads every 30s",
		"and after each action; the first paint is the last board's beads.",
		"",
		"  j/k ↑/↓   move           ^d/^u  page       g/G   top/bottom",
		"  ]/[ tab   next/prev section                 :     go to a bead id",
		"  /         filter (every word, any field)    s     sort: priority, recent, oldest",
		"  space v   detail pane    J/K  scroll it     b     bd show in a pager",
		"  esc q     close the pane, clear the filter, or quit",
		"  r         refresh now    T    tidy view     q     quit",
		"",
		"  ↵         focus the agent, or start one (confirms)",
		"  c x m     start claude, codex or muse (confirms)",
		"  R         resume an exited agent in its worktree (confirms)",
		"  a         add a note                        A   answer an idle worker (confirms)",
		"  n         toggle the next label             S   set status",
		"  P         set priority                      +   create a child task (confirms)",
		"  U         grant or revoke autonomy (confirms); the worker is told when idle",
		"  C         close with a reason (confirms)",
		"  o l       open the PR / Linear issue        y p copy the id / RUN command",
		"",
		"A worker at an approval prompt (!) is never answered from here: ↵ focuses it.",
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
