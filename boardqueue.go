package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

type queueKind int

const (
	queueKindPrompt queueKind = iota
	queueKindCritical
	queueKindQuestion
	queueKindRun
	queueKindFreeText
)

func (k queueKind) badge() string {
	switch k {
	case queueKindPrompt:
		return "PROMPT"
	case queueKindCritical:
		return "CRITICAL"
	case queueKindQuestion:
		return "DECIDE"
	case queueKindRun:
		return "RUN"
	default:
		return "QUESTION"
	}
}

func (k queueKind) color() string {
	switch k {
	case queueKindPrompt, queueKindCritical:
		return ansiRed + ansiBold
	case queueKindQuestion:
		return ansiYellow + ansiBold
	case queueKindRun:
		return ansiCyan + ansiBold
	default:
		return ansiYellow
	}
}

// queueItem is one actionable decision in the Needs You Decision Queue (Q).
type queueItem struct {
	row       boardRow
	kind      queueKind
	question  *NeedsMeQuestion
	rec       *int
	recLabel  string
	classN    int
	runCmd    string
	runReason string
	snoozed   bool
	snoozeExp time.Time
	summary   string
}

// buildQueueItems extracts every bead currently waiting on user input, sorted
// by urgency (live approval prompts, CRITICAL questions, structured decisions,
// RUN commands, free-text questions), priority, and oldest-first age so no
// decision starves. Snoozed non-prompt, non-CRITICAL items are hidden unless
// includeSnoozed is true.
func buildQueueItems(rows []boardRow, snoozes SnoozeState, now time.Time, includeSnoozed bool) ([]queueItem, int) {
	var items []queueItem
	snoozedHidden := 0

	for _, r := range rows {
		if r.header != "" {
			continue
		}
		isBlocked := r.agent != nil && r.agent.Status == "blocked"
		if !isBlocked && r.group != GroupNeedsYou {
			continue
		}
		it := classifyQueueRow(r)
		if it.kind != queueKindPrompt && it.kind != queueKindCritical {
			if e, ok := snoozes.activeFor(r.bead, now); ok {
				it.snoozed = true
				it.snoozeExp = e.Until
				if !includeSnoozed {
					snoozedHidden++
					continue
				}
			}
		}
		items = append(items, it)
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].snoozed != items[j].snoozed {
			return !items[i].snoozed
		}
		if items[i].kind != items[j].kind {
			return items[i].kind < items[j].kind
		}
		if items[i].row.bead.Priority != items[j].row.bead.Priority {
			return items[i].row.bead.Priority < items[j].row.bead.Priority
		}
		if !items[i].row.bead.UpdatedAt.Equal(items[j].row.bead.UpdatedAt) {
			return items[i].row.bead.UpdatedAt.Before(items[j].row.bead.UpdatedAt)
		}
		return items[i].row.bead.ID < items[j].row.bead.ID
	})
	return items, snoozedHidden
}

func classifyQueueRow(r boardRow) queueItem {
	it := queueItem{
		row:    r,
		classN: -1,
	}
	if r.agent != nil && r.agent.Status == "blocked" {
		it.kind = queueKindPrompt
		it.summary = "worker blocked at approval prompt"
		return it
	}
	notes := r.bead.Notes
	if cmd := pendingCommand(notes); cmd != "" {
		it.kind = queueKindRun
		it.runCmd = cmd
		it.runReason = pendingCommandReason(notes)
		it.summary = "RUN: " + cmd
		return it
	}
	if q, ok := parseNeedsMeQuestion(notes); ok {
		qCopy := q
		it.question = &qCopy
		it.classN = q.ClassN
		it.rec = q.recommendation()
		if it.rec != nil && *it.rec >= 1 && *it.rec <= len(q.Options) {
			it.recLabel = q.Options[*it.rec-1].Label
		}
		if q.isCritical() {
			it.kind = queueKindCritical
		} else {
			it.kind = queueKindQuestion
		}
		it.summary = q.Summary()
		return it
	}
	if q, ok := parseFreeTextNeedsMeQuestion(notes); ok {
		qCopy := q
		it.question = &qCopy
		it.classN = q.ClassN
		if q.isCritical() {
			it.kind = queueKindCritical
		} else {
			it.kind = queueKindFreeText
		}
		it.summary = q.Summary()
		return it
	}
	it.kind = queueKindFreeText
	if latest, _ := notesSplit(notes); latest != "" {
		it.summary = latest
	} else {
		it.summary = r.line
	}
	return it
}

func (m *board) rebuildQueue() {
	prevCur := m.queueCur
	var prevID string
	if it := m.queueSel(); it != nil {
		prevID = it.row.bead.ID
	}
	prevScroll := m.queueScroll
	var prevTopID string
	if prevScroll > 0 && prevScroll < len(m.queue) {
		prevTopID = m.queue[prevScroll].row.bead.ID
	}
	items, hidden := buildQueueItems(m.all, m.data.Snooze, m.now(), m.queueShowSnoozed)
	if len(m.queueDone) > 0 {
		filtered := items[:0]
		for _, it := range items {
			if _, done := m.queueDone[it.row.bead.ID]; !done {
				filtered = append(filtered, it)
			}
		}
		items = filtered
	}
	m.queue = items
	m.queueSnoozed = hidden
	if len(m.queue) == 0 {
		m.queueCur = 0
		m.queueScroll = 0
		return
	}
	found := -1
	if prevID != "" {
		for i, it := range m.queue {
			if it.row.bead.ID == prevID {
				found = i
				break
			}
		}
	}
	if found >= 0 {
		m.queueCur = found
	} else {
		m.queueCur = max(0, min(prevCur, len(m.queue)-1))
	}
	scroll := prevScroll
	if prevTopID != "" {
		topFound := -1
		for i, it := range m.queue {
			if it.row.bead.ID == prevTopID {
				topFound = i
				break
			}
		}
		if topFound >= 0 {
			scroll = topFound
		} else {
			scroll = min(prevScroll, len(m.queue)-1)
		}
	}
	if m.queueCardsFit > 0 {
		m.queueScroll = clampIndexScroll(len(m.queue), m.queueCur, scroll, m.queueCardsFit)
	} else {
		m.queueScroll = max(0, min(scroll, m.queueCur))
	}
}

func (m *board) queueSel() *queueItem {
	if m.queueCur < 0 || m.queueCur >= len(m.queue) {
		return nil
	}
	return &m.queue[m.queueCur]
}

func (m *board) openQueue() []cmd {
	m.view = viewQueue
	m.rebuildQueue()
	return m.tick()
}

// followRecommendation confirms and submits the worker's stated Recommendation
// option on a structured needs_me question in one keystroke (f), optionally
// attaching an extra note (e) before sending.
func (m *board) followRecommendation(r boardRow, fromQueue bool) []cmd {
	if m.guardAnswerInFlight(r.bead.ID) {
		return nil
	}
	notes := r.bead.Notes
	if d := m.details[r.bead.ID]; d != nil && d.full != nil {
		notes = d.full.Notes
	}
	if r.agent != nil && r.agent.Status == "blocked" {
		m.status = r.bead.ID + ": approval prompt requires inspecting options (press A)"
		return nil
	}
	if r.bead.Status != StatusNeedsMe || pendingCommand(notes) != "" {
		m.status = r.bead.ID + ": no structured recommendation to follow"
		return nil
	}
	q, ok := parseNeedsMeQuestion(notes)
	if !ok {
		m.status = r.bead.ID + ": no structured recommendation to follow"
		return nil
	}
	rec := q.recommendation()
	if rec == nil || *rec < 1 || *rec > len(q.Options) {
		m.status = r.bead.ID + ": question has no stated recommendation (press A)"
		return nil
	}
	m.confirmNeedsMeOption(r.bead.ID, q, q.Options[*rec-1], true, fromQueue)
	return nil
}

// snoozeAsk opens the quick snooze picker (z) for bead id.
func (m *board) snoozeAsk(id string, fromQueue bool) {
	prompt := fmt.Sprintf("snooze %s: 1 2h · 2 4h · 3 1d · 4 3d · 5 1w · 0 clear · t custom", id)
	m.ask = &ask{prompt: prompt, keys: "123450t", then: func(k string) []cmd {
		if k == "0" {
			return m.runClearSnooze(id)
		}
		if k == "t" {
			m.ask = &ask{prompt: "snooze " + id + " duration (e.g. 6h, 2d): ", text: true, then: func(raw string) []cmd {
				return m.runSnooze(id, raw, fromQueue)
			}}
			return nil
		}
		dur := map[string]string{"1": "2h", "2": "4h", "3": "1d", "4": "3d", "5": "1w"}[k]
		return m.runSnooze(id, dur, fromQueue)
	}}
}

func (m *board) runSnooze(id, dur string, fromQueue bool) []cmd {
	if m.env.snooze == nil {
		m.status = "snooze unavailable"
		return nil
	}
	m.status = fmt.Sprintf("snoozing %s (%s)…", id, dur)
	return []cmd{func() msg {
		text, err := m.env.snooze(id, dur)
		return doneMsg{
			text:         text,
			err:          err,
			reloadLocal:  true,
			closeAnswer:  err == nil && !fromQueue,
			advanceQueue: err == nil && fromQueue,
			answeredID:   id,
		}
	}}
}

func (m *board) runClearSnooze(id string) []cmd {
	if m.env.clearSnooze == nil {
		m.status = "snooze clear unavailable"
		return nil
	}
	m.status = fmt.Sprintf("clearing snooze on %s…", id)
	return []cmd{func() msg {
		text, err := m.env.clearSnooze(id)
		return doneMsg{text: text, err: err, reloadLocal: true}
	}}
}

func (m *board) queueKey(k string) []cmd {
	switch k {
	case "Q":
		m.view = viewList
		return nil
	case "q", "\x1b":
		if m.queueHome && k == "q" {
			m.quit = true
			return nil
		}
		m.view = viewList
		return nil
	case "?":
		m.back, m.view = viewQueue, viewHelp
		return nil
	case "L":
		m.cycleLayout()
		return nil
	case "H":
		m.queueShowSnoozed = !m.queueShowSnoozed
		m.rebuildQueue()
		if m.queueShowSnoozed {
			m.status = "showing snoozed items in queue"
		} else {
			m.status = "hiding snoozed items in queue"
		}
		return nil
	case "j", "\x1b[B":
		if len(m.queue) > 0 {
			m.queueCur = min(m.queueCur+1, len(m.queue)-1)
		}
		return m.loadDetailIfWide()
	case "k", "\x1b[A":
		m.queueCur = max(m.queueCur-1, 0)
		return m.loadDetailIfWide()
	case "\x04":
		if len(m.queue) > 0 {
			m.queueCur = min(m.queueCur+page, len(m.queue)-1)
		}
		return m.loadDetailIfWide()
	case "\x15":
		m.queueCur = max(m.queueCur-page, 0)
		return m.loadDetailIfWide()
	case "g", "\x1b[H":
		m.queueCur = 0
		return m.loadDetailIfWide()
	case "G", "\x1b[F":
		if len(m.queue) > 0 {
			m.queueCur = len(m.queue) - 1
		}
		return m.loadDetailIfWide()
	case "J", "\x1b[6~":
		m.scroll += 5
		return nil
	case "K", "\x1b[5~":
		m.scroll = max(0, m.scroll-5)
		return nil
	case "r":
		m.status = "refreshing queue…"
		m.agentsAt, m.localAt = time.Time{}, time.Time{}
		return append(m.tick(), m.loadBeads())
	case "E":
		m.view = viewEvents
		m.rebuildEvents()
		m.evAt = time.Time{}
		m.localAt = time.Time{}
		return m.tick()
	case "T":
		m.view = viewGC
		return m.tick()
	case "N":
		m.createAsk()
		return nil
	case "@":
		m.askCoordinatorBar()
		return nil
	}

	it := m.queueSel()
	if it == nil {
		return nil
	}
	r := it.row
	if len(k) == 1 && k[0] >= '1' && k[0] <= '9' {
		if m.guardAnswerInFlight(r.bead.ID) {
			return nil
		}
		n := int(k[0] - '0')
		switch it.kind {
		case queueKindPrompt:
			cmds := m.openAnswerFrom(r, true, m.queueCur, len(m.queue))
			m.status = "loading approval prompt…"
			return cmds
		case queueKindRun:
			m.status = "RUN command: press c/p to copy or ↵/A to record RAN"
			return nil
		default:
			if q, ok := m.activeStructuredQuestion(r); ok {
				return m.pickNeedsMeOption(r.bead.ID, q, n, true)
			}
			if it.question != nil && len(it.question.Options) > 0 {
				return m.pickNeedsMeOption(r.bead.ID, *it.question, n, true)
			}
			m.status = "free-text question: press t to type an answer (or ↵ to view)"
			return nil
		}
	}
	switch k {
	case "\r", "A", " ":
		return m.openAnswerFrom(r, true, m.queueCur, len(m.queue))
	case "f":
		return m.followRecommendation(r, true)
	case "t":
		if m.guardAnswerInFlight(r.bead.ID) {
			return nil
		}
		switch it.kind {
		case queueKindPrompt:
			m.status = "approval prompts require picking a numbered option (press ↵)"
		case queueKindRun:
			m.status = "RUN command: press c/p to copy or ↵/A to record RAN"
		default:
			m.askFreeTextNeedsMe(r, true)
		}
		return nil
	case "z":
		m.snoozeAsk(r.bead.ID, true)
		return nil
	case "p", "c":
		if it.runCmd != "" {
			m.status = m.copied(it.runCmd, "copied RUN command")
		}
		return nil
	case "y":
		m.status = m.copied(r.bead.ID, "copied "+r.bead.ID)
		return nil
	case "o":
		if len(r.prs) == 0 {
			m.status = "no PR known"
			return nil
		}
		m.status = m.env.open(r.prs[0].URL)
		return nil
	case "l":
		m.status = openLinearWith(m.cfg, r, m.env.open)
		return nil
	case "b":
		m.handoff = func() error { return pageBead(r.bead.ID) }
		return nil
	case "a", "C", "U", "S", "P":
		return m.beadKey(k, r)
	}
	return nil
}

// openQueueStep steps to index idx in m.queue while staying inside viewAnswer.
func (m *board) openQueueStep(idx int) []cmd {
	if len(m.queue) == 0 {
		m.view = viewQueue
		m.ans = nil
		m.status = "all caught up — 0 items waiting on you"
		return nil
	}
	idx = max(0, min(idx, len(m.queue)-1))
	m.queueCur = idx
	return m.openAnswerFrom(m.queue[idx].row, true, idx, len(m.queue))
}

// queueLines renders the Needs You Decision Queue as 2-line cards (readable on
// narrow mobile terminals and clean inside wide desktop split panes). It also
// records screen-row hit targets for touch/mouse interaction.
func (m *board) queueLines(w, body, rowOffset int) []string {
	if len(m.queue) == 0 {
		msg := "  all caught up — nothing waiting on you"
		if m.queueSnoozed > 0 {
			msg = fmt.Sprintf("  all caught up (%d snoozed — press H to show)", m.queueSnoozed)
		}
		return []string{ansiGreen + fit(msg, w) + ansiReset}
	}
	if body <= 0 {
		return nil
	}

	now := m.now()
	linesPerCard := 2
	cardsFit := max(1, body/linesPerCard)
	m.queueCardsFit = cardsFit
	m.queueScroll = clampIndexScroll(len(m.queue), m.queueCur, m.queueScroll, cardsFit)
	start := m.queueScroll

	var out []string
	for i := start; i < len(m.queue) && len(out) < body; i++ {
		it := m.queue[i]
		r := it.row
		when := age(r.bead.UpdatedAt, now)
		badge := it.kind.badge()
		badgeColor := it.kind.color()
		if r.pending != nil {
			badge = r.pending.queueBadge()
			badgeColor = r.pending.rowColor()
		}
		var tags []string
		tags = append(tags, badge)
		if it.classN >= 0 {
			tags = append(tags, fmt.Sprintf("C%d", it.classN))
		}
		if it.rec != nil {
			tags = append(tags, fmt.Sprintf("★rec%d", *it.rec))
		}
		if it.snoozed {
			tags = append(tags, "zzz")
		}
		meta := fmt.Sprintf("%s P%d %s [%s]", r.bead.ID, r.bead.Priority, when, strings.Join(tags, " "))
		sub := it.summary
		if sub == "" {
			sub = r.bead.Title
		}
		if r.pending != nil {
			sub = r.pending.rowLine()
		} else if it.recLabel != "" && w >= 55 {
			sub = fmt.Sprintf("%s → ★%d %s", shortTitle(sub, max(16, w/2)), *it.rec, it.recLabel)
		}

		scrRow1 := rowOffset + len(out)
		m.hits.recordRow(scrRow1, i)
		if i == m.queueCur {
			out = append(out, ansiInvert+rpcViewFit("▌ "+meta, w)+ansiReset)
		} else {
			out = append(out, "  "+badgeColor+fit(badge, 8)+ansiReset+" "+ansiBold+r.bead.ID+ansiReset+" "+ansiDim+fmt.Sprintf("P%d %s", r.bead.Priority, when)+ansiReset+queueTagSuffix(it, w))
			out[len(out)-1] = clampStyledLine(out[len(out)-1], "  "+meta, w)
		}
		if len(out) < body {
			scrRow2 := rowOffset + len(out)
			m.hits.recordRow(scrRow2, i)
			subStyle := ansiDim
			if r.pending != nil {
				subStyle = ansiGreen + ansiBold
			} else if i == m.queueCur {
				subStyle = ansiDim + ansiBold
			}
			out = append(out, subStyle+rpcViewFit("   "+sub, w)+ansiReset)
		}
	}
	return out
}

func queueTagSuffix(it queueItem, w int) string {
	var parts []string
	if it.classN >= 0 {
		parts = append(parts, fmt.Sprintf("C%d", it.classN))
	}
	if it.rec != nil {
		parts = append(parts, ansiGreen+ansiBold+fmt.Sprintf("★rec%d", *it.rec)+ansiReset)
	}
	if it.snoozed {
		parts = append(parts, ansiDim+"zzz"+ansiReset)
	}
	if len(parts) == 0 || w < 44 {
		return ""
	}
	return " " + strings.Join(parts, " ")
}

// clampStyledLine falls back to a cleanly fitted plain line if styled's visible
// rune count would exceed w (preventing wrapping on narrow mobile terminals).
func clampStyledLine(styled, plain string, w int) string {
	if visibleRuneLen(styled) <= w {
		return styled
	}
	return rpcViewFit(plain, w)
}

func queuePositionTag(idx, total int) string {
	if total <= 0 {
		return ""
	}
	return "[" + strconv.Itoa(idx+1) + "/" + strconv.Itoa(total) + "]"
}
