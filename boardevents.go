package main

import (
	"fmt"
	"sort"
	"time"
)

// eventRow is one row in the live Events / Inbox view (E): either a section
// header ("unread inbox" / "recent events"), an unread inbox/ file, a durable
// notification event from <state>/notify/events.json, or a recent AUTO: step
// from journal.jsonl.
type eventRow struct {
	header    string
	count     int
	source    string // "inbox", "notify", "auto"
	at        time.Time
	kind      string
	bead      string
	class     int // -1 when none
	seq       uint64
	inboxFile string
	summary   string
}

type eventsMsg struct {
	feed    []NotifyFeedEvent
	journal []journalRecord
	err     error
}

const (
	eventsJournalWindow = 24 * time.Hour
	eventsMaxAuto       = 50
	eventsMaxFeed       = 200
)

func loadBoardEvents(now time.Time) ([]NotifyFeedEvent, []journalRecord, error) {
	feed, err := loadNotifyFeed()
	if err != nil {
		return nil, nil, err
	}
	evts := feed.Events
	if len(evts) > eventsMaxFeed {
		evts = evts[len(evts)-eventsMaxFeed:]
	}
	recs, _ := readJournal(now.Add(-eventsJournalWindow))
	var autos []journalRecord
	for _, r := range recs {
		if r.Kind == journalAuto {
			autos = append(autos, r)
		}
	}
	if len(autos) > eventsMaxAuto {
		autos = autos[len(autos)-eventsMaxAuto:]
	}
	return evts, autos, nil
}

// buildEventRows builds the two sections of the Events / Inbox view:
// unread inbox items first (newest first, acknowledgeable with i), then recent
// notify/events.json and AUTO: journal entries (newest first).
func buildEventRows(inbox []InboxItem, feed []NotifyFeedEvent, journal []journalRecord) []eventRow {
	var inboxRows []eventRow
	for _, it := range inbox {
		kind := string(it.Kind)
		if it.Clears != nil && it.Clears.Kind == "prompt" {
			kind = "prompt"
		}
		inboxRows = append(inboxRows, eventRow{
			source:    "inbox",
			at:        it.At,
			kind:      kind,
			bead:      it.Bead,
			class:     -1,
			inboxFile: it.File,
			summary:   oneLine(it.Summary),
		})
	}
	sort.SliceStable(inboxRows, func(i, j int) bool {
		if !inboxRows[i].at.Equal(inboxRows[j].at) {
			return inboxRows[i].at.After(inboxRows[j].at)
		}
		return inboxRows[i].inboxFile > inboxRows[j].inboxFile
	})

	var recent []eventRow
	for _, ev := range feed {
		at, _ := time.Parse(time.RFC3339Nano, ev.At)
		if at.IsZero() {
			at, _ = time.Parse(time.RFC3339, ev.At)
		}
		bead := ""
		class := -1
		for _, it := range ev.Items {
			if bead == "" && it.Bead != "" {
				bead = it.Bead
			}
			if class < 0 && it.Class != nil {
				class = *it.Class
			}
		}
		summary := ev.Body
		if summary == "" {
			summary = ev.Title
		}
		recent = append(recent, eventRow{
			source:  "notify",
			at:      at,
			kind:    ev.Event,
			bead:    bead,
			class:   class,
			seq:     ev.Seq,
			summary: oneLine(summary),
		})
	}
	for _, r := range journal {
		if r.Kind != journalAuto {
			continue
		}
		summary := autoWhat(r.Text)
		if summary == "" {
			summary = oneLine(r.Text)
		}
		recent = append(recent, eventRow{
			source:  "auto",
			at:      r.At,
			kind:    "AUTO",
			bead:    r.Bead,
			class:   r.Class,
			summary: summary,
		})
	}
	sort.SliceStable(recent, func(i, j int) bool {
		if !recent[i].at.Equal(recent[j].at) {
			return recent[i].at.After(recent[j].at)
		}
		return recent[i].seq > recent[j].seq
	})

	var out []eventRow
	if len(inboxRows) > 0 {
		out = append(out, eventRow{header: "unread inbox", count: len(inboxRows)})
		out = append(out, inboxRows...)
	}
	if len(recent) > 0 {
		out = append(out, eventRow{header: "recent events", count: len(recent)})
		out = append(out, recent...)
	}
	return out
}

func (m *board) rebuildEvents() {
	var prevFile string
	var prevSeq uint64
	var prevBead string
	var prevKind string
	if r := m.evSel(); r != nil {
		prevFile, prevSeq, prevBead, prevKind = r.inboxFile, r.seq, r.bead, r.kind
	}
	m.evRows = buildEventRows(m.data.Inbox, m.feed, m.autoRecs)
	m.evCur = -1
	for i, r := range m.evRows {
		if r.header != "" {
			continue
		}
		if prevFile != "" && r.inboxFile == prevFile {
			m.evCur = i
			break
		}
		if prevSeq != 0 && r.seq == prevSeq {
			m.evCur = i
			break
		}
		if prevFile == "" && prevSeq == 0 && prevBead != "" && r.bead == prevBead && r.kind == prevKind {
			m.evCur = i
			break
		}
	}
	if m.evCur < 0 {
		m.evCur = nextSelectableEvent(m.evRows, 0, 1)
	}
}

func (m *board) evSel() *eventRow {
	if m.evCur < 0 || m.evCur >= len(m.evRows) || m.evRows[m.evCur].header != "" {
		return nil
	}
	return &m.evRows[m.evCur]
}

func nextSelectableEvent(rows []eventRow, from, dir int) int {
	for i := from; i >= 0 && i < len(rows); i += dir {
		if rows[i].header == "" {
			return i
		}
	}
	return -1
}

func (m *board) evMove(n int) {
	dir := 1
	if n < 0 {
		dir, n = -1, -n
	}
	for ; n > 0; n-- {
		i := nextSelectableEvent(m.evRows, m.evCur+dir, dir)
		if i < 0 {
			return
		}
		m.evCur = i
	}
}

func (m *board) loadEvents() cmd {
	if m.env.events == nil {
		m.evLoaded = true
		return nil
	}
	m.evLoading = true
	return func() msg {
		feed, recs, err := m.env.events()
		return eventsMsg{feed: feed, journal: recs, err: err}
	}
}

func (m *board) eventsKey(k string) []cmd {
	switch k {
	case "q", "\x1b", "E":
		m.view = viewList
		return nil
	case "?":
		m.back, m.view = viewEvents, viewHelp
		return nil
	case "j", "\x1b[B":
		m.evMove(1)
	case "k", "\x1b[A":
		m.evMove(-1)
	case "\x04", "\x1b[6~":
		m.evMove(page)
	case "\x15", "\x1b[5~":
		m.evMove(-page)
	case "g", "\x1b[H":
		m.evCur = nextSelectableEvent(m.evRows, 0, 1)
	case "G", "\x1b[F":
		m.evCur = nextSelectableEvent(m.evRows, len(m.evRows)-1, -1)
	case "r":
		m.status = "refreshing events…"
		m.localAt = time.Time{}
		m.evAt = time.Time{}
		m.evLoaded = false
		return m.tick()
	case "\r":
		r := m.evSel()
		if r == nil {
			return nil
		}
		if r.bead == "" {
			m.status = "event has no bead id"
			return nil
		}
		m.view = viewList
		m.jump(r.bead)
	case "i":
		r := m.evSel()
		if r == nil {
			return nil
		}
		if r.inboxFile == "" {
			m.status = "only unread inbox items can be acknowledged"
			return nil
		}
		file := r.inboxFile
		// Optimistically remove from the displayed inbox list.
		nextInbox := make([]InboxItem, 0, len(m.data.Inbox))
		for _, it := range m.data.Inbox {
			if it.File != file {
				nextInbox = append(nextInbox, it)
			}
		}
		m.data.Inbox = nextInbox
		m.rebuildEvents()
		return []cmd{func() msg {
			err := m.env.inboxDone([]string{file})
			return doneMsg{text: "acknowledged " + file, err: err, reloadLocal: true}
		}}
	}
	return nil
}

func eventKindColor(kind string) string {
	switch kind {
	case "needs_you", "needs_me", "prompt", "worker_blocked", "checks_failing", "breaker_tripped", "unsafe_auto_merge", "blocked":
		return ansiRed
	case "ready_for_review", "review_bump", "ready_to_merge", "pr_ready", "pr_green", "pr_merged", "merged":
		return ansiYellow
	case "AUTO", "inbox_runner", "unblocked", "orphan_finish":
		return ansiGreen
	default:
		return ansiCyan
	}
}

func (m *board) eventsLines(w, body int) []string {
	switch {
	case m.evErr != "" && len(m.evRows) == 0:
		return []string{ansiRed + fit("  "+m.evErr, w) + ansiReset}
	case m.evLoading && !m.evLoaded && len(m.evRows) == 0:
		return []string{ansiDim + fit("  reading inbox and notify/events.json…", w) + ansiReset}
	case len(m.evRows) == 0:
		return []string{ansiDim + fit("  no unread inbox items or recent events", w) + ansiReset}
	}

	start := 0
	if m.evCur >= body && body > 0 {
		start = m.evCur - body + 1
	}
	if start > 0 && start == m.evCur && m.evRows[start-1].header != "" {
		start--
	}

	now := m.now()
	beadW := 6
	kindW := 8
	for _, r := range m.evRows {
		if r.header != "" {
			continue
		}
		if n := len([]rune(r.bead)); n > beadW {
			beadW = n
		}
		if n := len([]rune(r.kind)); n > kindW {
			kindW = n
		}
	}
	beadW = min(beadW, 18)
	kindW = min(kindW, 16)

	var out []string
	for i := start; i < len(m.evRows) && len(out) < body; i++ {
		r := m.evRows[i]
		if r.header != "" {
			color := ansiCyan
			if r.header == "unread inbox" {
				color = ansiYellow
			}
			out = append(out, ansiBold+color+fit(fmt.Sprintf("%s (%d)", r.header, r.count), w)+ansiReset)
			continue
		}
		when := fmt.Sprintf("%3s", age(r.at, now))
		bead := r.bead
		if bead == "" {
			bead = "-"
		}
		tag := ""
		if r.class >= 0 {
			tag = fmt.Sprintf(" [C%d]", r.class)
		}
		if r.seq > 0 {
			tag = fmt.Sprintf(" #%d%s", r.seq, tag)
		}
		summary := r.summary + tag
		// Fixed width: marker(2) + age(3) + " " + kind(kindW) + " " + bead(beadW) + " "
		fixed := 2 + 3 + 1 + kindW + 1 + beadW + 1
		sumW := max(0, w-fixed)
		if i == m.evCur {
			line := "▌ " + when + " " + fit(r.kind, kindW) + " " + fit(bead, beadW)
			if sumW > 0 {
				line += " " + rpcViewFit(summary, sumW)
			}
			out = append(out, ansiInvert+rpcViewFit(line, w)+ansiReset)
			continue
		}
		kColor := eventKindColor(r.kind)
		line := "  " + ansiDim + when + ansiReset + " " + kColor + fit(r.kind, kindW) + ansiReset + " " + fit(bead, beadW)
		if sumW > 0 {
			line += " " + rpcViewFit(summary, sumW)
		}
		out = append(out, line)
	}
	return out
}
