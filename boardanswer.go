package main

import (
	"fmt"
	"strconv"
	"strings"
)

type answerKind int

const (
	answerKindPrompt answerKind = iota
	answerKindNeedsMe
	answerKindRun
	answerKindDirect
)

type pendingAnswerPhase int

const (
	pendingAnswerSending pendingAnswerPhase = iota
	pendingAnswerSent
)

// pendingAnswer tracks an optimistic in-flight or just-recorded answer so the
// board row, detail pane, and answer view immediately confirm registration and
// prevent accidental duplicate submissions until the reload catches up.
type pendingAnswer struct {
	beadID        string
	kind          answerKind
	option        int // 1-9 for numbered choices; 0 for free text / RUN / direct reply
	label         string
	phase         pendingAnswerPhase
	clearAfterSeq int
}

func (p pendingAnswer) rowLine() string {
	if p.phase == pendingAnswerSent {
		return "✓ answered: " + p.label
	}
	return "✓ sending: " + p.label
}

func (p pendingAnswer) rowColor() string {
	return ansiGreen + ansiBold
}

func (p pendingAnswer) compactBadge() string {
	if p.phase == pendingAnswerSent {
		return " [✓ SENT]"
	}
	return " [SENDING]"
}

func (p pendingAnswer) queueBadge() string {
	if p.phase == pendingAnswerSent {
		return "SENT"
	}
	return "SENDING"
}

func (p pendingAnswer) bannerLine() string {
	if p.phase == pendingAnswerSent {
		return "✓ Answered: " + p.label + " (updating board…)"
	}
	return "✓ Submitting: " + p.label + " (saving…)"
}

func (p pendingAnswer) optionTag() string {
	if p.phase == pendingAnswerSent {
		return "answered"
	}
	return "sending…"
}

func (p pendingAnswer) inFlightStatus(id string) string {
	if p.phase == pendingAnswerSent {
		return fmt.Sprintf("%s: already answered (%s); refreshing…", id, p.label)
	}
	return fmt.Sprintf("%s: answer already in flight (%s)", id, p.label)
}

func (m *board) pendingAnswerFor(id string) *pendingAnswer {
	if id == "" || m.pendingAnswers == nil {
		return nil
	}
	return m.pendingAnswers[id]
}

func (m *board) guardAnswerInFlight(id string) bool {
	if pa := m.pendingAnswerFor(id); pa != nil {
		m.status = pa.inFlightStatus(id)
		return true
	}
	return false
}

func (m *board) startPendingAnswer(id string, pa pendingAnswer) {
	if id == "" {
		return
	}
	if m.pendingAnswers == nil {
		m.pendingAnswers = map[string]*pendingAnswer{}
	}
	pa.beadID = id
	pa.phase = pendingAnswerSending
	cp := pa
	m.pendingAnswers[id] = &cp
	m.rebuild()
}

// answerState holds the live approval prompt, structured needs_me question, or
// pending RUN: command shown in viewAnswer so the user can inspect the full
// text, hash, and options before confirming an answer.
type answerState struct {
	beadID     string
	beadTitle  string
	kind       answerKind
	back       boardView
	fromQueue  bool
	queueIdx   int
	queueTotal int
	loading    bool
	loadErr    string
	prompt     *ApprovalPrompt
	question   *NeedsMeQuestion
	runCmd     string
	runReason  string
	scroll     int
}

type answerLoadMsg struct {
	id       string
	kind     answerKind
	prompt   *ApprovalPrompt
	question *NeedsMeQuestion
	err      error
}

// openAnswer handles pressing A on row r:
//   - A worker blocked at an approval prompt (!) loads the live prompt and
//     opens viewAnswer with its command, hash, and numbered options.
//   - A needs_me bead with a structured QUESTION:/OPTIONS: block (or a free-text
//     needs_me question with no running worker) opens viewAnswer and routes
//     confirmed answers through the NEEDS_ME ANSWERED --by TJ via board path.
//   - Any other idle worker keeps the direct free-text reply prompt.
func (m *board) openAnswer(r boardRow) []cmd {
	return m.openAnswerFrom(r, false, 0, 0)
}

func (m *board) openAnswerFrom(r boardRow, fromQueue bool, qIdx, qTotal int) []cmd {
	id := r.bead.ID
	backView := m.view
	if fromQueue {
		backView = viewQueue
	}
	if r.agent != nil && r.agent.Status == "blocked" {
		if m.env.readPrompt == nil {
			m.status = id + ": " + unanswerable(r.agent)
			return nil
		}
		m.ans = &answerState{
			beadID:     id,
			beadTitle:  r.bead.Title,
			kind:       answerKindPrompt,
			back:       backView,
			fromQueue:  fromQueue,
			queueIdx:   qIdx,
			queueTotal: qTotal,
			loading:    true,
		}
		m.view = viewAnswer
		return []cmd{m.loadPromptCmd(id)}
	}

	notes := r.bead.Notes
	if d := m.details[id]; d != nil && d.full != nil {
		notes = d.full.Notes
	}
	if r.bead.Status == StatusNeedsMe && (r.agent == nil || r.agent.Status != "working") {
		if runCmd := pendingCommand(notes); runCmd != "" {
			m.ans = &answerState{
				beadID:     id,
				beadTitle:  r.bead.Title,
				kind:       answerKindRun,
				back:       backView,
				fromQueue:  fromQueue,
				queueIdx:   qIdx,
				queueTotal: qTotal,
				runCmd:     runCmd,
				runReason:  pendingCommandReason(notes),
			}
			m.view = viewAnswer
			return nil
		}
		if q, ok := parseNeedsMeQuestion(notes); ok {
			qCopy := q
			m.ans = &answerState{
				beadID:     id,
				beadTitle:  r.bead.Title,
				kind:       answerKindNeedsMe,
				back:       backView,
				fromQueue:  fromQueue,
				queueIdx:   qIdx,
				queueTotal: qTotal,
				question:   &qCopy,
			}
			m.view = viewAnswer
			return m.refreshNeedsMeQuestionCmd(id)
		}
		if r.agent == nil || fromQueue {
			if q, ok := parseFreeTextNeedsMeQuestion(notes); ok {
				qCopy := q
				m.ans = &answerState{
					beadID:     id,
					beadTitle:  r.bead.Title,
					kind:       answerKindNeedsMe,
					back:       backView,
					fromQueue:  fromQueue,
					queueIdx:   qIdx,
					queueTotal: qTotal,
					question:   &qCopy,
				}
				m.view = viewAnswer
				return m.refreshNeedsMeQuestionCmd(id)
			}
		}
	}

	m.answer(r)
	return nil
}

func (m *board) loadPromptCmd(id string) cmd {
	return func() msg {
		p, err := m.env.readPrompt(id)
		if err != nil {
			return answerLoadMsg{id: id, kind: answerKindPrompt, err: err}
		}
		pCopy := p
		return answerLoadMsg{id: id, kind: answerKindPrompt, prompt: &pCopy}
	}
}

func (m *board) refreshNeedsMeQuestionCmd(id string) []cmd {
	if m.env.show == nil {
		return nil
	}
	return []cmd{func() msg {
		b, err := m.env.show(id)
		if err != nil {
			return answerLoadMsg{id: id, kind: answerKindNeedsMe, err: err}
		}
		if b.Status != StatusNeedsMe {
			return answerLoadMsg{id: id, kind: answerKindNeedsMe, err: fmt.Errorf("%s is %s, not needs_me", id, b.Status)}
		}
		q, ok := parseTappableNeedsMeQuestion(b.Notes)
		if !ok || pendingCommand(b.Notes) != "" {
			return answerLoadMsg{id: id, kind: answerKindNeedsMe, err: fmt.Errorf("%s no longer has a live question", id)}
		}
		qCopy := q
		return answerLoadMsg{id: id, kind: answerKindNeedsMe, question: &qCopy}
	}}
}

func (m *board) applyAnswerLoad(x answerLoadMsg) {
	if m.ans == nil || m.ans.beadID != x.id {
		return
	}
	m.ans.loading = false
	if x.err != nil {
		if x.kind == answerKindPrompt {
			m.view = m.ans.back
			m.ans = nil
			m.status = fmt.Sprintf("%s: approval prompt not readable (%v); answer it in its pane (enter focuses)", x.id, x.err)
			return
		}
		// Keep an already-rendered question visible unless reload failed on a fresh load.
		if m.ans.question == nil {
			m.view = m.ans.back
			m.ans = nil
		}
		m.status = x.id + ": " + x.err.Error()
		return
	}
	m.ans.loadErr = ""
	if x.prompt != nil {
		m.ans.prompt = x.prompt
	}
	if x.question != nil {
		m.ans.question = x.question
	}
}

func (m *board) answerKey(k string) []cmd {
	a := m.ans
	if a == nil {
		m.view = viewList
		return nil
	}
	switch k {
	case "q", "\x1b":
		m.view = a.back
		m.ans = nil
		return nil
	case "?":
		m.back, m.view = viewAnswer, viewHelp
		return nil
	case "j", "\x1b[B":
		a.scroll++
		return nil
	case "k", "\x1b[A":
		a.scroll = max(0, a.scroll-1)
		return nil
	case "J", "\x04", "\x1b[6~":
		a.scroll += 5
		return nil
	case "K", "\x15", "\x1b[5~":
		a.scroll = max(0, a.scroll-5)
		return nil
	case "r":
		if a.kind == answerKindRun {
			return nil
		}
		a.loading = true
		m.status = "reloading…"
		if a.kind == answerKindPrompt {
			return []cmd{m.loadPromptCmd(a.beadID)}
		}
		return m.refreshNeedsMeQuestionCmd(a.beadID)
	case "]":
		if len(m.queue) > 0 {
			next := (m.queueCur + 1) % len(m.queue)
			return m.openQueueStep(next)
		}
		return nil
	case "[":
		if len(m.queue) > 0 {
			prev := (m.queueCur - 1 + len(m.queue)) % len(m.queue)
			return m.openQueueStep(prev)
		}
		return nil
	case "z":
		if a.kind == answerKindNeedsMe || a.kind == answerKindRun {
			m.snoozeAsk(a.beadID, a.fromQueue)
			return nil
		}
	}

	if a.loading {
		if (len(k) == 1 && k[0] >= '1' && k[0] <= '9') || k == "f" || k == "t" || k == "\r" {
			m.status = "still loading live prompt…"
		}
		return nil
	}

	// RUN: command view keys: c/p copy command; d/Enter record RAN: + resume worker.
	if a.kind == answerKindRun {
		switch k {
		case "c", "p":
			m.status = m.copied(a.runCmd, "copied RUN command")
			return nil
		case "d", "\r":
			if m.guardAnswerInFlight(a.beadID) {
				return nil
			}
			id, runCmd, fromQueue := a.beadID, a.runCmd, a.fromQueue
			m.ask = &ask{prompt: "RAN result for " + id + " (enter for ok): ", text: true, allowEmpty: true, then: func(res string) []cmd {
				if strings.TrimSpace(res) == "" {
					res = "ok"
				}
				m.ask = confirm(fmt.Sprintf("record RAN on %s (%s) and resume worker?", id, shortTitle(res, 36)), func() []cmd {
					if m.guardAnswerInFlight(id) {
						return nil
					}
					m.startPendingAnswer(id, pendingAnswer{
						kind:  answerKindRun,
						label: "RAN: " + shortTitle(res, 34),
					})
					m.status = "recording RAN on " + id + "…"
					return []cmd{func() msg {
						if m.env.completeRun == nil {
							return doneMsg{err: fmt.Errorf("RUN completion is unavailable"), answeredID: id}
						}
						text, err := m.env.completeRun(id, runCmd, res)
						return doneMsg{
							text:         text,
							err:          err,
							reload:       true,
							reloadAgents: true,
							closeAnswer:  err == nil && !fromQueue,
							advanceQueue: err == nil && fromQueue,
							answeredID:   id,
						}
					}}
				})
				return nil
			}}
			return nil
		}
		return nil
	}

	// One-key accept stated recommendation: f or Enter inside viewAnswer.
	if (k == "f" || k == "\r") && a.kind == answerKindNeedsMe && a.question != nil {
		if m.guardAnswerInFlight(a.beadID) {
			return nil
		}
		rec := a.question.recommendation()
		if rec == nil || *rec < 1 || *rec > len(a.question.Options) {
			m.status = "no stated recommendation; pick 1-" + strconv.Itoa(len(a.question.Options)) + " or t"
			return nil
		}
		m.confirmNeedsMeOption(a.beadID, *a.question, a.question.Options[*rec-1], true, a.fromQueue)
		return nil
	}

	// Numbered option pick: 1-9.
	if len(k) == 1 && k[0] >= '1' && k[0] <= '9' {
		if m.guardAnswerInFlight(a.beadID) {
			return nil
		}
		n := int(k[0] - '0')
		switch a.kind {
		case answerKindPrompt:
			if a.prompt == nil || n > len(a.prompt.Options) {
				m.status = fmt.Sprintf("no option %d", n)
				return nil
			}
			m.confirmPromptOption(a.beadID, *a.prompt, a.prompt.Options[n-1], a.fromQueue)
			return nil
		case answerKindNeedsMe:
			if a.question == nil || n > len(a.question.Options) {
				m.status = fmt.Sprintf("no option %d", n)
				return nil
			}
			return m.pickNeedsMeOption(a.beadID, *a.question, n, a.fromQueue)
		}
	}

	// Custom free-text answer (needs_me questions only; never approval prompts).
	if k == "t" && a.kind == answerKindNeedsMe && a.question != nil {
		if m.guardAnswerInFlight(a.beadID) {
			return nil
		}
		m.askCustomNeedsMe(a.beadID, *a.question, a.fromQueue)
		return nil
	}

	return nil
}

// activeStructuredQuestion returns r's live structured QUESTION/OPTIONS block
// when r is a needs_me bead (not RUN: and not currently working) so 1-9 can
// answer it directly from the board or queue without an extra view switch.
func (m *board) activeStructuredQuestion(r boardRow) (NeedsMeQuestion, bool) {
	if r.bead.Status != StatusNeedsMe || (r.agent != nil && r.agent.Status == "working") {
		return NeedsMeQuestion{}, false
	}
	notes := r.bead.Notes
	if d := m.details[r.bead.ID]; d != nil && d.full != nil {
		notes = d.full.Notes
	}
	if pendingCommand(notes) != "" {
		return NeedsMeQuestion{}, false
	}
	return parseNeedsMeQuestion(notes)
}

func (m *board) pickNeedsMeOption(id string, q NeedsMeQuestion, n int, fromQueue bool) []cmd {
	if m.guardAnswerInFlight(id) {
		return nil
	}
	if n < 1 || n > len(q.Options) {
		m.status = fmt.Sprintf("no option %d (1-%d)", n, len(q.Options))
		return nil
	}
	o := q.Options[n-1]
	rec := q.recommendation()
	isRec := rec != nil && *rec == o.Number
	m.confirmNeedsMeOption(id, q, o, isRec, fromQueue)
	return nil
}

func (m *board) askFreeTextNeedsMe(r boardRow, fromQueue bool) {
	if m.guardAnswerInFlight(r.bead.ID) {
		return
	}
	notes := r.bead.Notes
	if d := m.details[r.bead.ID]; d != nil && d.full != nil {
		notes = d.full.Notes
	}
	q, ok := parseTappableNeedsMeQuestion(notes)
	if !ok {
		m.status = r.bead.ID + ": no live needs_me question to answer"
		return
	}
	m.askCustomNeedsMe(r.bead.ID, q, fromQueue)
}

func (m *board) askCustomNeedsMe(id string, q NeedsMeQuestion, fromQueue bool) {
	if m.guardAnswerInFlight(id) {
		return
	}
	m.ask = &ask{prompt: "custom answer for " + id + ": ", text: true, then: func(text string) []cmd {
		m.ask = confirm(fmt.Sprintf("answer %s with custom text: %q?", id, shortTitle(text, 45)), func() []cmd {
			if m.guardAnswerInFlight(id) {
				return nil
			}
			m.startPendingAnswer(id, pendingAnswer{
				kind:  answerKindNeedsMe,
				label: "custom: " + shortTitle(text, 34),
			})
			m.status = fmt.Sprintf("recording custom answer on %s…", id)
			return []cmd{func() msg {
				if m.env.answerNeedsMe == nil {
					return doneMsg{err: fmt.Errorf("needs_me answering is unavailable"), answeredID: id}
				}
				res, err := m.env.answerNeedsMe(id, tapChoiceFreeText, text, q.Hash)
				return doneMsg{
					text:         res,
					err:          err,
					reload:       true,
					reloadAgents: true,
					closeAnswer:  err == nil && !fromQueue,
					advanceQueue: err == nil && fromQueue,
					answeredID:   id,
				}
			}}
		})
		return nil
	}}
}

func (m *board) confirmPromptOption(id string, p ApprovalPrompt, o PromptOption, fromQueue bool) {
	if m.guardAnswerInFlight(id) {
		return
	}
	m.pendingOpt = o.Number
	m.pendingOptBead = id
	warn := ""
	if answerClass(p, o, 0) == 9 {
		warn = " [class 9]"
	}
	var keys strings.Builder
	keys.WriteString("y\r")
	for _, opt := range p.Options {
		if opt.Number >= 1 && opt.Number <= 9 {
			keys.WriteByte(byte('0' + opt.Number))
		}
	}
	prompt := fmt.Sprintf("answer %s prompt [%s]: %d. %s%s?  y/n · ↵ send", id, p.Hash, o.Number, shortTitle(o.Label, 42), warn)
	m.ask = &ask{prompt: prompt, keys: keys.String(), then: func(k string) []cmd {
		if len(k) == 1 && k[0] >= '1' && k[0] <= '9' {
			n := int(k[0] - '0')
			if n >= 1 && n <= len(p.Options) {
				m.confirmPromptOption(id, p, p.Options[n-1], fromQueue)
				if n == o.Number {
					m.status = fmt.Sprintf("option %d selected — press y or ↵ to send (esc cancels)", n)
				}
			}
			return nil
		}
		if m.guardAnswerInFlight(id) {
			return nil
		}
		m.startPendingAnswer(id, pendingAnswer{
			kind:   answerKindPrompt,
			option: o.Number,
			label:  fmt.Sprintf("%d. %s", o.Number, shortTitle(o.Label, 34)),
		})
		m.status = fmt.Sprintf("answering %s [%s]…", id, p.Hash)
		return []cmd{func() msg {
			if m.env.answerPrompt == nil {
				return doneMsg{err: fmt.Errorf("prompt answering is unavailable"), answeredID: id}
			}
			text, err := m.env.answerPrompt(id, strconv.Itoa(o.Number), p.Hash)
			return doneMsg{
				text:         text,
				err:          err,
				reload:       true,
				reloadAgents: true,
				closeAnswer:  err == nil && !fromQueue,
				advanceQueue: err == nil && fromQueue,
				answeredID:   id,
			}
		}}
	}}
}

// confirmNeedsMeOption confirms a numbered needs_me choice with y/↵, an
// optional e (+note) shortcut, and 1-9 option switching so extra context can
// ride along and pressing another digit updates the choice instead of cancelling.
func (m *board) confirmNeedsMeOption(id string, q NeedsMeQuestion, o PromptOption, isRec, fromQueue bool) {
	if m.guardAnswerInFlight(id) {
		return
	}
	m.pendingOpt = o.Number
	m.pendingOptBead = id
	tag := ""
	if isRec {
		tag = "★ "
	}
	send := func(extra string) []cmd {
		if m.guardAnswerInFlight(id) {
			return nil
		}
		lbl := fmt.Sprintf("%d. %s", o.Number, shortTitle(o.Label, 34))
		if extra != "" {
			lbl = fmt.Sprintf("%d. %s (+note)", o.Number, shortTitle(o.Label, 26))
		}
		m.startPendingAnswer(id, pendingAnswer{
			kind:   answerKindNeedsMe,
			option: o.Number,
			label:  lbl,
		})
		m.status = fmt.Sprintf("recording answer on %s…", id)
		return []cmd{func() msg {
			if m.env.answerNeedsMe == nil {
				return doneMsg{err: fmt.Errorf("needs_me answering is unavailable"), answeredID: id}
			}
			text, err := m.env.answerNeedsMe(id, strconv.Itoa(o.Number), extra, q.Hash)
			return doneMsg{
				text:         text,
				err:          err,
				reload:       true,
				reloadAgents: true,
				closeAnswer:  err == nil && !fromQueue,
				advanceQueue: err == nil && fromQueue,
				answeredID:   id,
			}
		}}
	}
	var keys strings.Builder
	keys.WriteString("y\re")
	for _, opt := range q.Options {
		if opt.Number >= 1 && opt.Number <= 9 {
			keys.WriteByte(byte('0' + opt.Number))
		}
	}
	prompt := fmt.Sprintf("answer %s: %s%d. %s?  y/n · ↵ send · e +note", id, tag, o.Number, shortTitle(o.Label, 40))
	m.ask = &ask{prompt: prompt, keys: keys.String(), then: func(k string) []cmd {
		if k == "e" {
			m.ask = &ask{prompt: fmt.Sprintf("note with option %d for %s: ", o.Number, id), text: true, then: func(extra string) []cmd {
				return send(extra)
			}}
			return nil
		}
		if len(k) == 1 && k[0] >= '1' && k[0] <= '9' {
			n := int(k[0] - '0')
			m.pickNeedsMeOption(id, q, n, fromQueue)
			if n == o.Number {
				m.status = fmt.Sprintf("option %d selected — press y or ↵ to send (e +note, esc cancels)", n)
			}
			return nil
		}
		return send("")
	}}
}

func (m *board) answerLines(w, body, rowOffset int) []string {
	a := m.ans
	if a == nil {
		return nil
	}
	if a.loading {
		return []string{ansiDim + fit("  reading live prompt…", w) + ansiReset}
	}

	type answerLine struct {
		text   string
		style  string
		optNum int // 1-9 when tapping this line selects that option
	}
	var raw []answerLine
	add := func(style, text string) { raw = append(raw, answerLine{text: text, style: style}) }
	para := func(style, indent, text string) {
		for _, l := range wrap(text, max(1, w-len([]rune(indent)))) {
			add(style, indent+l)
		}
	}
	paraOpt := func(optNum int, style, indent, text string) {
		for _, l := range wrap(text, max(1, w-len([]rune(indent)))) {
			raw = append(raw, answerLine{text: indent + l, style: style, optNum: optNum})
		}
	}

	pa := m.pendingAnswerFor(a.beadID)

	switch a.kind {
	case answerKindRun:
		meta := fmt.Sprintf("%s · RUN command · manual step", a.beadID)
		para(ansiCyan+ansiBold, "", meta)
		if a.beadTitle != "" {
			para(ansiBold, "", a.beadTitle)
		}
		if pa != nil {
			add("", "")
			para(ansiGreen+ansiBold, "  ", pa.bannerLine())
		}
		add("", "")
		add(ansiBold, "Command to run (press c to copy via clipboard/OSC 52)")
		para(ansiYellow+ansiBold, "  $ ", a.runCmd)
		if a.runReason != "" {
			add("", "")
			add(ansiBold, "Why")
			para("", "  ", a.runReason)
		}
		add("", "")
		add(ansiGreen+ansiBold, "Actions")
		para("", "  ", "c / p   copy command to clipboard (works over SSH via OSC 52)")
		para("", "  ", "d / ↵   record RAN: <cmd> → <result>, set in_progress, and prompt worker")
		para("", "  ", "z       snooze this bead for later")

	case answerKindPrompt:
		if a.prompt == nil {
			add(ansiDim, "  (no approval prompt loaded)")
			break
		}
		p := a.prompt
		meta := fmt.Sprintf("%s · approval prompt · hash %s · --by TJ", a.beadID, p.Hash)
		para(ansiCyan+ansiBold, "", meta)
		if a.beadTitle != "" {
			para(ansiBold, "", a.beadTitle)
		}
		if pa != nil {
			add("", "")
			para(ansiGreen+ansiBold, "  ", pa.bannerLine())
		}
		if p.Countdown != "" {
			para(ansiRed+ansiBold, "", "auto-deny in "+p.Countdown+" (class 9)")
		}
		add("", "")
		add(ansiBold, "Prompt")
		if len(p.Lines) == 0 {
			add(ansiDim, "  (no prompt text)")
		}
		for _, l := range p.Lines {
			para("", "  ", l)
		}
		add("", "")
		add(ansiBold, "Options (press or tap 1-"+strconv.Itoa(len(p.Options))+")")
		pending := 0
		if pa == nil && m.pendingOptBead == a.beadID && m.pendingOpt > 0 {
			pending = m.pendingOpt
		}
		for _, o := range p.Options {
			cur := "  "
			style := ""
			if o.Selected {
				cur = "❯ "
				style = ansiBold
			}
			tag := ""
			if answerClass(*p, o, 0) == 9 {
				tag = " [class 9]"
				style = ansiYellow + ansiBold
			}
			if pa != nil && pa.option == o.Number {
				cur = "✓ "
				style = ansiGreen + ansiBold
				tag += "  (" + pa.optionTag() + ")"
			} else if pending == o.Number {
				cur = "❯ "
				style = ansiInvert + ansiBold
			}
			optTarget := o.Number
			if pa != nil {
				optTarget = 0
			}
			paraOpt(optTarget, style, "  ", fmt.Sprintf("%s%d. %s%s", cur, o.Number, o.Label, tag))
		}
		if pending > 0 {
			add("", "")
			para(ansiYellow+ansiBold, "  ", fmt.Sprintf("▸ Option %d selected — y / ↵ confirm · 1-%d switch · esc cancel", pending, len(p.Options)))
		}

	case answerKindNeedsMe:
		if a.question == nil {
			add(ansiDim, "  (no question loaded)")
			break
		}
		q := a.question
		meta := []string{a.beadID, "needs_me", "hash " + q.Hash, "by TJ via board"}
		if q.Class != "" {
			meta = append(meta, "CLASS: "+q.Class)
		}
		para(ansiCyan+ansiBold, "", strings.Join(meta, " · "))
		if a.beadTitle != "" {
			para(ansiBold, "", a.beadTitle)
		}
		if pa != nil {
			add("", "")
			para(ansiGreen+ansiBold, "  ", pa.bannerLine())
		}
		parts := splitQuestionParts(*q)
		add("", "")
		if parts.critical != "" {
			para(ansiRed+ansiBold, "  ", parts.critical)
		}
		add(ansiBold, "Question")
		if parts.question != "" {
			para(ansiBold, "  ", parts.question)
		} else {
			for _, l := range q.Lines {
				para("", "  ", l)
			}
		}
		if parts.recommendation != "" {
			para(ansiGreen+ansiBold, "  ", "★ Recommendation: "+parts.recommendation)
		}
		add("", "")
		rec := q.recommendation()
		pending := 0
		if pa == nil && m.pendingOptBead == a.beadID && m.pendingOpt > 0 {
			pending = m.pendingOpt
		}
		if len(q.Options) > 0 {
			hdr := "Options (press or tap 1-" + strconv.Itoa(len(q.Options)) + ", t custom"
			if rec != nil {
				hdr += ", f/↵ follow ★" + strconv.Itoa(*rec)
			}
			hdr += ")"
			add(ansiBold, hdr)
			for _, o := range q.Options {
				style := ""
				prefix := "  "
				suffix := ""
				if rec != nil && *rec == o.Number {
					style = ansiGreen + ansiBold
					suffix = "  ★ recommended"
				}
				if pa != nil && pa.option == o.Number {
					prefix = "✓ "
					style = ansiGreen + ansiBold
					suffix += "  (" + pa.optionTag() + ")"
				} else if pending == o.Number {
					prefix = "❯ "
					style = ansiInvert + ansiBold
				}
				optTarget := o.Number
				if pa != nil {
					optTarget = 0
				}
				paraOpt(optTarget, style, "  ", fmt.Sprintf("%s%d. %s%s", prefix, o.Number, o.Label, suffix))
			}
			if pending > 0 {
				add("", "")
				para(ansiYellow+ansiBold, "  ", fmt.Sprintf("▸ Option %d selected — y / ↵ confirm · e +note · 1-%d switch · esc cancel", pending, len(q.Options)))
			}
		} else {
			add(ansiBold, "Free-text question (press t to enter your answer)")
		}
		if len(parts.details) > 0 {
			add("", "")
			add(ansiBold, "Details")
			for _, det := range parts.details {
				para(ansiDim, "  ", det)
			}
		}
		if len(parts.evidence) > 0 {
			add("", "")
			add(ansiBold, "Evidence")
			for _, ev := range parts.evidence {
				para(ansiDim, "  ", ev)
			}
		}
	}

	if body <= 0 {
		return nil
	}
	a.scroll = min(a.scroll, max(0, len(raw)-1))
	visible := raw[a.scroll:]
	if len(visible) > body && body > 1 {
		visible = append(visible[:body-1:body-1], answerLine{text: "… more below (j/k or J/K scrolls)", style: ansiDim})
	}
	out := make([]string, 0, min(len(visible), body))
	for i := 0; i < len(visible) && i < body; i++ {
		if visible[i].optNum > 0 {
			m.hits.recordOption(rowOffset+i, visible[i].optNum)
		}
		out = append(out, visible[i].style+rpcViewFit(visible[i].text, w)+ansiReset)
	}
	return out
}
