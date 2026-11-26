package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// layoutMode controls how the board adapts across mobile (Moshi <80 cols),
// standard (80-129 cols), and wide desktop (Ghostty/Rex >=130 cols) terminals.
type layoutMode int

const (
	layoutAuto layoutMode = iota
	layoutCompact
	layoutStandard
	layoutWide
)

const (
	boardCompactMaxW = 79
	boardWideMinW    = 130
)

func (l layoutMode) String() string {
	switch l {
	case layoutCompact:
		return "compact"
	case layoutStandard:
		return "standard"
	case layoutWide:
		return "wide"
	default:
		return "auto"
	}
}

func effectiveLayout(mode layoutMode, w int) layoutMode {
	if mode != layoutAuto {
		return mode
	}
	switch {
	case w <= boardCompactMaxW:
		return layoutCompact
	case w >= boardWideMinW:
		return layoutWide
	default:
		return layoutStandard
	}
}

func (m *board) cycleLayout() {
	m.layout = (m.layout + 1) % 4
	m.status = "layout: " + m.layout.String()
}

// mouseEvent is a parsed SGR 1006 mouse/touch sequence (\x1b[<btn;col;rowM/m).
type mouseEvent struct {
	btn     int
	col     int // 0-based screen column
	row     int // 0-based screen row
	release bool
}

var sgrMouseRE = regexp.MustCompile(`^\x1b\[<(\d+);(\d+);(\d+)([Mm])$`)

func parseSGRMouse(k string) (mouseEvent, bool) {
	m := sgrMouseRE.FindStringSubmatch(k)
	if m == nil {
		return mouseEvent{}, false
	}
	btn, err1 := strconv.Atoi(m[1])
	col, err2 := strconv.Atoi(m[2])
	row, err3 := strconv.Atoi(m[3])
	if err1 != nil || err2 != nil || err3 != nil || col < 1 || row < 1 {
		return mouseEvent{}, false
	}
	return mouseEvent{
		btn:     btn,
		col:     col - 1,
		row:     row - 1,
		release: m[4] == "m",
	}, true
}

// boardHits tracks screen-coordinate hit targets recorded by the last render so
// touch taps (Moshi) and mouse clicks/scrolls (Ghostty/Rex) map cleanly to rows,
// answer options, and confirmation actions.
type boardHits struct {
	width       int
	height      int
	splitCol    int // >0 when side-by-side wide split is active
	rowTargets  map[int]int
	optionRows  map[int]int
	confirmRow  int
	confirmKeys map[string][2]int // key -> [startCol, endCol)
}

func (h *boardHits) reset(w, hgt int) {
	h.width = w
	h.height = hgt
	h.splitCol = 0
	h.rowTargets = map[int]int{}
	h.optionRows = map[int]int{}
	h.confirmRow = -1
	h.confirmKeys = map[string][2]int{}
}

func (h *boardHits) recordRow(screenRow, idx int) {
	if h.rowTargets == nil {
		h.rowTargets = map[int]int{}
	}
	h.rowTargets[screenRow] = idx
}

func (h *boardHits) recordOption(screenRow, optNum int) {
	if h.optionRows == nil {
		h.optionRows = map[int]int{}
	}
	h.optionRows[screenRow] = optNum
}

// handleMouse maps a parsed SGR mouse/touch event to board navigation/actions.
func (m *board) handleMouse(ev mouseEvent) []cmd {
	if ev.release {
		return nil
	}
	inRightPane := m.hits.splitCol > 0 && ev.col > m.hits.splitCol

	// Mouse wheel / two-finger scroll (SGR buttons 64=up, 65=down).
	switch ev.btn {
	case 64: // wheel up
		if inRightPane || m.view == viewDetail || m.view == viewAnswer {
			if m.view == viewAnswer {
				return m.key("k")
			}
			m.scroll = max(0, m.scroll-3)
			return nil
		}
		return m.key("k")
	case 65: // wheel down
		if inRightPane || m.view == viewDetail || m.view == viewAnswer {
			if m.view == viewAnswer {
				return m.key("j")
			}
			m.scroll += 3
			return nil
		}
		return m.key("j")
	}

	// Only left-click / primary touch tap (btn == 0).
	if ev.btn != 0 {
		return nil
	}

	// Active single-key confirmation bar: allow tapping y / n / e directly.
	if m.ask != nil && !m.ask.text {
		if ev.row == m.hits.confirmRow {
			for k, span := range m.hits.confirmKeys {
				if ev.col >= span[0] && ev.col < span[1] {
					return m.key(k)
				}
			}
		}
		return nil
	}
	if m.ask != nil || m.filter.typing {
		return nil
	}

	// Tapping row 0 ("need you" count) jumps straight into/out of Decision Queue.
	if ev.row == 0 {
		switch m.view {
		case viewQueue:
			return m.key("Q")
		case viewList, viewDetail, viewEvents, viewGC:
			if m.view == viewList || m.view == viewDetail {
				return m.openQueue()
			}
			m.view = viewList
			return nil
		}
		return nil
	}

	switch m.view {
	case viewAnswer:
		if opt, ok := m.hits.optionRows[ev.row]; ok && opt >= 1 && opt <= 9 {
			return m.key(strconv.Itoa(opt))
		}
		return nil
	case viewQueue:
		if inRightPane {
			if opt, ok := m.hits.optionRows[ev.row]; ok && opt >= 1 && opt <= 9 {
				return m.key(strconv.Itoa(opt))
			}
			return nil
		}
		if idx, ok := m.hits.rowTargets[ev.row]; ok && idx >= 0 && idx < len(m.queue) {
			if idx == m.queueCur {
				return m.key("\r")
			}
			m.queueCur = idx
			return m.loadDetailIfWide()
		}
	case viewList, viewDetail:
		if inRightPane {
			if opt, ok := m.hits.optionRows[ev.row]; ok && opt >= 1 && opt <= 9 {
				return m.key(strconv.Itoa(opt))
			}
			return nil
		}
		if idx, ok := m.hits.rowTargets[ev.row]; ok && idx >= 0 && idx < len(m.rows) && m.rows[idx].header == "" {
			if idx == m.cur {
				r := m.rows[idx]
				if r.group == GroupNeedsYou || (r.agent != nil && r.agent.Status == "blocked") {
					return m.openAnswer(r)
				}
				if m.view == viewList {
					m.view = viewDetail
					return m.loadDetail()
				}
				return nil
			}
			m.cur = idx
			if m.view == viewDetail {
				return m.loadDetail()
			}
			return m.loadDetailIfWide()
		}
	case viewEvents:
		if idx, ok := m.hits.rowTargets[ev.row]; ok && idx >= 0 && idx < len(m.evRows) && m.evRows[idx].header == "" {
			if idx == m.evCur {
				return m.key("\r")
			}
			m.evCur = idx
		}
	case viewGC:
		if idx, ok := m.hits.rowTargets[ev.row]; ok && idx >= 0 && idx < len(m.gc) {
			if idx == m.gcCur {
				return m.key(" ")
			}
			m.gcCur = idx
		}
	}
	return nil
}

// osc52Sequence builds a standard OSC 52 clipboard escape sequence supported
// natively over SSH by Ghostty, Moshi, iTerm2, WezTerm, Kitty, and Herdr.
func osc52Sequence(text string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"
}

// copyViaTerminal emits OSC 52 to out (so remote SSH terminals like Moshi and
// Ghostty copy straight to the client clipboard) and also invokes wl-copy when
// a local Wayland display is active.
func copyViaTerminal(out io.Writer, text string) error {
	var oscErr error
	if out != nil {
		_, oscErr = io.WriteString(out, osc52Sequence(text))
	}
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		if err := exec.Command("wl-copy", text).Run(); err == nil {
			return nil
		} else if out == nil {
			return err
		}
	}
	return oscErr
}

// openBoardURL opens u locally when running in a desktop GUI session, or copies
// u via OSC 52 when running over SSH / headless (Moshi, remote Ghostty/Rex).
func openBoardURL(out io.Writer, u string) string {
	overSSH := os.Getenv("SSH_TTY") != "" || os.Getenv("SSH_CONNECTION") != ""
	noGUI := os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == ""
	if overSSH || noGUI {
		if err := copyViaTerminal(out, u); err != nil {
			return "URL: " + u
		}
		return "copied URL: " + u
	}
	msg := openURL(u)
	if strings.HasPrefix(msg, "xdg-open:") {
		if err := copyViaTerminal(out, u); err == nil {
			return "copied URL: " + u
		}
	}
	return msg
}

func openLinearWith(cfg Config, r boardRow, opener func(string) string) string {
	if r.linear == "" {
		return "no Linear issue known"
	}
	u := linearURL(r.linear, r.bead.Notes, cfg.LinearWorkspace)
	if u == "" {
		return "set linear_workspace in config.toml to open " + r.linear
	}
	if opener != nil {
		return opener(u)
	}
	return openURL(u)
}

// visibleANSI matches CSI color/style escapes so we can compute exact display
// width for side-by-side split-pane padding without breaking ANSI sequences.
var visibleANSI = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func visibleRuneLen(s string) int {
	return utf8.RuneCountInString(visibleANSI.ReplaceAllString(s, ""))
}

// padVisible pads a single styled line with trailing spaces so its visible
// width equals exactly w runes (truncating plain fallback if over w).
func padVisible(styled string, w int) string {
	n := visibleRuneLen(styled)
	if n == w {
		return styled
	}
	if n < w {
		return styled + strings.Repeat(" ", w-n)
	}
	plain := visibleANSI.ReplaceAllString(styled, "")
	return fit(plain, w)
}

// compactListLines renders viewList as 2-line touch-friendly cards for narrow
// mobile viewports (<80 cols, e.g. Moshi) so titles and badges stay readable.
func (m *board) compactListLines(w, body, rowOffset int) []string {
	rows := m.rows
	if len(rows) == 0 {
		return []string{ansiDim + fit("  nothing open", w) + ansiReset}
	}
	if body <= 0 {
		return nil
	}

	rowHeight := func(i int) int {
		if rows[i].header != "" {
			return 1
		}
		return 2
	}
	start := 0
	for start < m.cur {
		used := 0
		for i := start; i <= m.cur && i < len(rows); i++ {
			used += rowHeight(i)
		}
		if used <= body {
			break
		}
		start++
	}
	if start > 0 && start == m.cur && rows[start-1].header != "" {
		used := 1 + rowHeight(m.cur)
		if used <= body {
			start--
		}
	}

	now := m.now()
	var out []string
	for i := start; i < len(rows) && len(out) < body; i++ {
		r := rows[i]
		if r.header != "" {
			out = append(out, ansiBold+groupColor(r.group)+fit(fmt.Sprintf("%s (%d)", r.header, r.count), w)+ansiReset)
			continue
		}
		glyph, gColor := agentGlyph(r.agent)
		if r.native != nil {
			glyph, gColor = "○", ansiDim
			if r.native.State == "running" {
				glyph, gColor = "●", ansiGreen
			} else if r.native.State == "waiting on approval" {
				glyph, gColor = "!", ansiRed
			}
		}
		when := age(r.bead.UpdatedAt, now)
		badge := compactRowBadge(r)
		isMarked := m.marked[r.bead.ID]
		headPlain := fmt.Sprintf("%s %s P%d %s%s", glyph, r.bead.ID, r.bead.Priority, when, badge)
		subPlain := strings.TrimSpace(rpcViewOneLine(r.bead.Title))
		if state := strings.TrimSpace(rpcViewOneLine(r.line)); state != "" {
			subPlain = shortTitle(subPlain, max(14, w-18)) + " · " + state
		}

		m.hits.recordRow(rowOffset+len(out), i)
		if i == m.cur {
			prefix := "▌ "
			if isMarked {
				prefix = "▌*"
			}
			out = append(out, ansiInvert+rpcViewFit(prefix+headPlain, w)+ansiReset)
		} else {
			prefix := "  "
			if isMarked {
				prefix = ansiYellow + ansiBold + "* " + ansiReset
			}
			styled := prefix + gColor + glyph + ansiReset + " " + ansiBold + r.bead.ID + ansiReset + " " + ansiDim + fmt.Sprintf("P%d %s", r.bead.Priority, when) + ansiReset + badge
			out = append(out, clampStyledLine(styled, "  "+headPlain, w))
		}
		if len(out) < body {
			m.hits.recordRow(rowOffset+len(out), i)
			if i == m.cur {
				out = append(out, ansiBold+rpcViewFit("   "+subPlain, w)+ansiReset)
			} else {
				out = append(out, ansiDim+rpcViewFit("   "+subPlain, w)+ansiReset)
			}
		}
	}
	return out
}

func compactRowBadge(r boardRow) string {
	if r.agent != nil && r.agent.Status == "blocked" {
		return " [PROMPT]"
	}
	if r.bead.Status == StatusNeedsMe {
		if pendingCommand(r.bead.Notes) != "" {
			return " [RUN]"
		}
		if q, ok := parseNeedsMeQuestion(r.bead.Notes); ok {
			var parts []string
			if q.isCritical() {
				parts = append(parts, "CRIT")
			}
			if q.ClassN >= 0 {
				parts = append(parts, fmt.Sprintf("C%d", q.ClassN))
			}
			if rec := q.recommendation(); rec != nil {
				parts = append(parts, fmt.Sprintf("★%d", *rec))
			}
			if len(parts) > 0 {
				return " [" + strings.Join(parts, " ") + "]"
			}
		}
	}
	if len(r.prs) > 0 && r.prs[0].Number > 0 {
		return fmt.Sprintf(" #%d", r.prs[0].Number)
	}
	return ""
}

type previewLine struct {
	styledLine
	optNum int
}

// wideSplitLines renders a side-by-side desktop split pane: left pane holds the
// main list or Decision Queue, right pane shows the selected bead's live
// decision card (if needs_me/RUN, without duplicating Waiting in detailLines)
// followed by Done milestones, PRs, compact Description, and scannable History.
func (m *board) wideSplitLines(w, body, rowOffset int, forQueue bool) []string {
	leftW := max(48, min(72, w*46/100))
	rightW := w - leftW - 1
	m.hits.splitCol = leftW

	var left []string
	var selRow *boardRow
	if forQueue {
		left = m.queueLines(leftW, body, rowOffset)
		if it := m.queueSel(); it != nil {
			r := it.row
			selRow = &r
		}
	} else {
		left = m.listLinesWithHits(leftW, body, rowOffset)
		selRow = m.sel()
	}

	var right []string
	if selRow == nil {
		right = []string{ansiDim + fit("  select a bead to inspect", rightW) + ansiReset}
	} else {
		if selRow.bead.ID != m.scrolledOn {
			m.scroll, m.scrolledOn = 0, selRow.bead.ID
		}
		dec := m.decisionPreviewLines(*selRow, rightW)
		detail := make([]previewLine, 0, len(dec)+32)
		detail = append(detail, dec...)
		for _, l := range rpcBoardDetail(*selRow, m.now()) {
			detail = append(detail, previewLine{styledLine: l})
		}
		for _, l := range detailLinesWithOpts(*selRow, m.details[selRow.bead.ID], rightW, detailOpts{
			omitWaiting:   len(dec) > 0,
			decisionFirst: true,
			compactDesc:   5,
		}) {
			detail = append(detail, previewLine{styledLine: l})
		}
		m.scroll = min(m.scroll, max(0, len(detail)-1))
		vis := detail[m.scroll:]
		if len(vis) > body && body > 1 {
			vis = append(vis[:body-1:body-1], previewLine{styledLine: styledLine{"… more below (J/K scrolls, A opens full view)", ansiDim}})
		}
		for i := 0; i < len(vis) && i < body; i++ {
			if vis[i].optNum > 0 {
				m.hits.recordOption(rowOffset+i, vis[i].optNum)
			}
			right = append(right, vis[i].style+rpcViewFit(vis[i].text, rightW)+ansiReset)
		}
	}

	out := make([]string, 0, body)
	sep := ansiDim + "│" + ansiReset
	for i := 0; i < body; i++ {
		l := ""
		if i < len(left) {
			l = left[i]
		}
		r := ""
		if i < len(right) {
			r = right[i]
		}
		out = append(out, padVisible(l, leftW)+sep+padVisible(r, rightW))
	}
	return out
}

// decisionPreviewLines puts an actionable Open Question / RUN / approval card
// at the top of the wide desktop right-hand preview pane so you can read the
// question, recommendation, and clickable 1-9 options immediately without
// scrolling past Details or duplicating the Waiting section below.
func (m *board) decisionPreviewLines(r boardRow, w int) []previewLine {
	var out []previewLine
	add := func(style, text string) { out = append(out, previewLine{styledLine: styledLine{text, style}}) }
	para := func(style, indent, text string) {
		for _, l := range wrap(text, max(1, w-utf8.RuneCountInString(indent))) {
			add(style, indent+l)
		}
	}
	paraOpt := func(optNum int, style, indent, text string) {
		for _, l := range wrap(text, max(1, w-utf8.RuneCountInString(indent))) {
			out = append(out, previewLine{styledLine: styledLine{indent + l, style}, optNum: optNum})
		}
	}
	addCapped := func(firstStyle, contStyle, firstIndent, contIndent, text string, maxLines int) {
		var tmp []styledLine
		appendWrappedCapped(&tmp, firstStyle, contStyle, firstIndent, contIndent, text, w, maxLines)
		for _, l := range tmp {
			out = append(out, previewLine{styledLine: l})
		}
	}

	if r.agent != nil && r.agent.Status == "blocked" {
		add(ansiRed+ansiBold, "Waiting: approval prompt (press A to inspect & answer)")
		add("", "")
		return out
	}
	if r.bead.Status != StatusNeedsMe {
		return nil
	}
	notes := r.bead.Notes
	if d := m.details[r.bead.ID]; d != nil && d.full != nil {
		notes = d.full.Notes
	}
	if cmd := pendingCommand(notes); cmd != "" {
		add(ansiCyan+ansiBold, "Waiting: RUN command (c/p copy · A record RAN · z snooze)")
		para(ansiYellow+ansiBold, "  $ ", cmd)
		if why := pendingCommandReason(notes); why != "" {
			addCapped(ansiDim, ansiDim, "  Why: ", "    ", why, 3)
		}
		add("", "")
		return out
	}
	if q, ok := parseNeedsMeQuestion(notes); ok {
		hdr := "Open Question"
		var meta []string
		if q.ClassN >= 0 {
			meta = append(meta, fmt.Sprintf("C%d", q.ClassN))
		}
		meta = append(meta, fmt.Sprintf("1-%d pick", len(q.Options)))
		rec := q.recommendation()
		if rec != nil {
			meta = append(meta, fmt.Sprintf("f ★%d", *rec))
		}
		meta = append(meta, "t text", "A full")
		hdr += " (" + strings.Join(meta, " · ") + ")"
		add(ansiYellow+ansiBold, hdr)

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
		add("", "")
		pending := 0
		if m.pendingOptBead == r.bead.ID && m.pendingOpt > 0 {
			pending = m.pendingOpt
		}
		for _, o := range q.Options {
			prefix := "  "
			st := ""
			suf := ""
			if rec != nil && *rec == o.Number {
				st = ansiGreen + ansiBold
				suf = " ★"
			}
			if pending == o.Number {
				prefix = "❯ "
				st = ansiInvert + ansiBold
			}
			paraOpt(o.Number, st, "  ", fmt.Sprintf("%s%d. %s%s", prefix, o.Number, o.Label, suf))
		}
		if pending > 0 {
			add("", "")
			para(ansiYellow+ansiBold, "  ", fmt.Sprintf("▸ Option %d selected — y / ↵ send · e +note · 1-%d switch · esc cancel", pending, len(q.Options)))
		}
		if len(parts.details) > 0 {
			add("", "")
			for _, det := range parts.details {
				addCapped(ansiDim, ansiDim, "  Details: ", "    ", det, 3)
			}
		}
		if len(parts.evidence) > 0 {
			for _, ev := range parts.evidence {
				addCapped(ansiDim, ansiDim, "  Evidence: ", "    ", ev, 2)
			}
		}
		add("", "")
		return out
	}
	if q, ok := parseFreeTextNeedsMeQuestion(notes); ok {
		hdr := "Open Question (free text · t answer · A full · z snooze)"
		if q.ClassN >= 0 {
			hdr = fmt.Sprintf("Open Question (C%d · free text · t answer · A full)", q.ClassN)
		}
		add(ansiYellow+ansiBold, hdr)
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
		for _, det := range parts.details {
			addCapped(ansiDim, ansiDim, "  ", "    ", det, 3)
		}
		for _, ev := range parts.evidence {
			addCapped(ansiDim, ansiDim, "  Evidence: ", "    ", ev, 2)
		}
		add("", "")
		return out
	}
	return nil
}

// loadDetailIfWide triggers background detail fetching when the wide desktop
// split pane is active so moving j/k keeps the right-hand pane live.
func (m *board) loadDetailIfWide() []cmd {
	if effectiveLayout(m.layout, m.hits.width) != layoutWide {
		return nil
	}
	if m.view == viewQueue {
		if it := m.queueSel(); it != nil {
			return m.loadDetailFor(it.row)
		}
		return nil
	}
	return m.loadDetail()
}
