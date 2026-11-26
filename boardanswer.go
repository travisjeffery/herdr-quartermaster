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
)

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
		return nil
	}

	// RUN: command view keys: c/p copy command; d/Enter record RAN: + resume worker.
	if a.kind == answerKindRun {
		switch k {
		case "c", "p":
			m.status = m.copied(a.runCmd, "copied RUN command")
			return nil
		case "d", "\r":
			id, runCmd, fromQueue := a.beadID, a.runCmd, a.fromQueue
			m.ask = &ask{prompt: "RAN result for " + id + " (enter for ok): ", text: true, allowEmpty: true, then: func(res string) []cmd {
				if strings.TrimSpace(res) == "" {
					res = "ok"
				}
				m.ask = confirm(fmt.Sprintf("record RAN on %s (%s) and resume worker?", id, shortTitle(res, 36)), func() []cmd {
					m.status = "recording RAN on " + id + "…"
					return []cmd{func() msg {
						if m.env.completeRun == nil {
							return doneMsg{err: fmt.Errorf("RUN completion is unavailable")}
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
		n := int(k[0] - '0')
		switch a.kind {
		case answerKindPrompt:
			if a.prompt == nil || n > len(a.prompt.Options) {
				m.status = fmt.Sprintf("no option %d", n)
				return nil
			}
			p := *a.prompt
			o := p.Options[n-1]
			warn := ""
			if answerClass(p, o, 0) == 9 {
				warn = " [class 9]"
			}
			id, fromQueue := a.beadID, a.fromQueue
			m.ask = confirm(fmt.Sprintf("answer %s prompt [%s]: %d. %s%s?", id, p.Hash, o.Number, shortTitle(o.Label, 45), warn), func() []cmd {
				m.status = fmt.Sprintf("answering %s [%s]…", id, p.Hash)
				return []cmd{func() msg {
					if m.env.answerPrompt == nil {
						return doneMsg{err: fmt.Errorf("prompt answering is unavailable")}
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
			})
			return nil
		case answerKindNeedsMe:
			if a.question == nil || n > len(a.question.Options) {
				m.status = fmt.Sprintf("no option %d", n)
				return nil
			}
			q := *a.question
			o := q.Options[n-1]
			rec := q.recommendation()
			isRec := rec != nil && *rec == o.Number
			m.confirmNeedsMeOption(a.beadID, q, o, isRec, a.fromQueue)
			return nil
		}
	}

	// Custom free-text answer (needs_me questions only; never approval prompts).
	if k == "t" && a.kind == answerKindNeedsMe && a.question != nil {
		q := *a.question
		id, fromQueue := a.beadID, a.fromQueue
		m.ask = &ask{prompt: "custom answer for " + id + ": ", text: true, then: func(text string) []cmd {
			m.ask = confirm(fmt.Sprintf("answer %s with custom text: %q?", id, shortTitle(text, 45)), func() []cmd {
				m.status = fmt.Sprintf("recording custom answer on %s…", id)
				return []cmd{func() msg {
					if m.env.answerNeedsMe == nil {
						return doneMsg{err: fmt.Errorf("needs_me answering is unavailable")}
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
		return nil
	}

	return nil
}

// confirmNeedsMeOption confirms a numbered needs_me choice with y/n and an
// optional e (+note) shortcut so extra context can ride along with the option.
func (m *board) confirmNeedsMeOption(id string, q NeedsMeQuestion, o PromptOption, isRec, fromQueue bool) {
	tag := ""
	if isRec {
		tag = "★ "
	}
	send := func(extra string) []cmd {
		m.status = fmt.Sprintf("recording answer on %s…", id)
		return []cmd{func() msg {
			if m.env.answerNeedsMe == nil {
				return doneMsg{err: fmt.Errorf("needs_me answering is unavailable")}
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
	prompt := fmt.Sprintf("answer %s: %s%d. %s (NEEDS_ME ANSWERED → in_progress)?  y/n · e +note", id, tag, o.Number, shortTitle(o.Label, 42))
	m.ask = &ask{prompt: prompt, keys: "ye", then: func(k string) []cmd {
		if k == "e" {
			m.ask = &ask{prompt: fmt.Sprintf("note with option %d for %s: ", o.Number, id), text: true, then: func(extra string) []cmd {
				return send(extra)
			}}
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

	switch a.kind {
	case answerKindRun:
		meta := fmt.Sprintf("%s · RUN command · manual step", a.beadID)
		para(ansiCyan+ansiBold, "", meta)
		if a.beadTitle != "" {
			para(ansiBold, "", a.beadTitle)
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
			paraOpt(o.Number, style, "  ", fmt.Sprintf("%s%d. %s%s", cur, o.Number, o.Label, tag))
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
		add("", "")
		add(ansiBold, "Question")
		for _, l := range q.Lines {
			style := ""
			if strings.HasPrefix(l, "CRITICAL:") {
				style = ansiRed + ansiBold
			}
			para(style, "  ", l)
		}
		add("", "")
		rec := q.recommendation()
		if len(q.Options) > 0 {
			hdr := "Options (press or tap 1-" + strconv.Itoa(len(q.Options)) + ", t custom"
			if rec != nil {
				hdr += ", f/↵ follow ★" + strconv.Itoa(*rec)
			}
			hdr += ")"
			add(ansiBold, hdr)
			for _, o := range q.Options {
				style := ""
				suffix := ""
				if rec != nil && *rec == o.Number {
					style = ansiGreen + ansiBold
					suffix = "  ★ recommended"
				}
				paraOpt(o.Number, style, "  ", fmt.Sprintf("%d. %s%s", o.Number, o.Label, suffix))
			}
		} else {
			add(ansiBold, "Free-text question (press t to enter your answer)")
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
