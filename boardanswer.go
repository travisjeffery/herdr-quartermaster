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
)

// answerState holds the live approval prompt or structured needs_me question
// shown in viewAnswer so the user can inspect the full text, hash, and options
// before confirming an answer.
type answerState struct {
	beadID    string
	beadTitle string
	kind      answerKind
	back      boardView
	loading   bool
	loadErr   string
	prompt    *ApprovalPrompt
	question  *NeedsMeQuestion
	scroll    int
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
	id := r.bead.ID
	if r.agent != nil && r.agent.Status == "blocked" {
		if m.env.readPrompt == nil {
			m.status = id + ": " + unanswerable(r.agent)
			return nil
		}
		m.ans = &answerState{
			beadID:    id,
			beadTitle: r.bead.Title,
			kind:      answerKindPrompt,
			back:      m.view,
			loading:   true,
		}
		m.view = viewAnswer
		return []cmd{m.loadPromptCmd(id)}
	}

	notes := r.bead.Notes
	if d := m.details[id]; d != nil && d.full != nil {
		notes = d.full.Notes
	}
	if r.bead.Status == StatusNeedsMe && pendingCommand(notes) == "" && (r.agent == nil || r.agent.Status != "working") {
		if q, ok := parseNeedsMeQuestion(notes); ok {
			qCopy := q
			m.ans = &answerState{
				beadID:    id,
				beadTitle: r.bead.Title,
				kind:      answerKindNeedsMe,
				back:      m.view,
				question:  &qCopy,
			}
			m.view = viewAnswer
			return m.refreshNeedsMeQuestionCmd(id)
		}
		if r.agent == nil {
			if q, ok := parseFreeTextNeedsMeQuestion(notes); ok {
				qCopy := q
				m.ans = &answerState{
					beadID:    id,
					beadTitle: r.bead.Title,
					kind:      answerKindNeedsMe,
					back:      m.view,
					question:  &qCopy,
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
		a.loading = true
		m.status = "reloading…"
		if a.kind == answerKindPrompt {
			return []cmd{m.loadPromptCmd(a.beadID)}
		}
		return m.refreshNeedsMeQuestionCmd(a.beadID)
	}

	if a.loading {
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
			id := a.beadID
			m.ask = confirm(fmt.Sprintf("answer %s prompt [%s]: %d. %s%s?", id, p.Hash, o.Number, shortTitle(o.Label, 45), warn), func() []cmd {
				m.status = fmt.Sprintf("answering %s [%s]…", id, p.Hash)
				return []cmd{func() msg {
					if m.env.answerPrompt == nil {
						return doneMsg{err: fmt.Errorf("prompt answering is unavailable")}
					}
					text, err := m.env.answerPrompt(id, strconv.Itoa(o.Number), p.Hash)
					return doneMsg{text: text, err: err, reload: true, reloadAgents: true, closeAnswer: err == nil}
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
			id := a.beadID
			m.ask = confirm(fmt.Sprintf("answer %s: %d. %s (NEEDS_ME ANSWERED → in_progress)?", id, o.Number, shortTitle(o.Label, 45)), func() []cmd {
				m.status = fmt.Sprintf("recording answer on %s…", id)
				return []cmd{func() msg {
					if m.env.answerNeedsMe == nil {
						return doneMsg{err: fmt.Errorf("needs_me answering is unavailable")}
					}
					text, err := m.env.answerNeedsMe(id, strconv.Itoa(o.Number), "", q.Hash)
					return doneMsg{text: text, err: err, reload: true, reloadAgents: true, closeAnswer: err == nil}
				}}
			})
			return nil
		}
	}

	// Custom free-text answer (needs_me questions only; never approval prompts).
	if k == "t" && a.kind == answerKindNeedsMe && a.question != nil {
		q := *a.question
		id := a.beadID
		m.ask = &ask{prompt: "custom answer for " + id + ": ", text: true, then: func(text string) []cmd {
			m.ask = confirm(fmt.Sprintf("answer %s with custom text: %q?", id, shortTitle(text, 45)), func() []cmd {
				m.status = fmt.Sprintf("recording custom answer on %s…", id)
				return []cmd{func() msg {
					if m.env.answerNeedsMe == nil {
						return doneMsg{err: fmt.Errorf("needs_me answering is unavailable")}
					}
					res, err := m.env.answerNeedsMe(id, tapChoiceFreeText, text, q.Hash)
					return doneMsg{text: res, err: err, reload: true, reloadAgents: true, closeAnswer: err == nil}
				}}
			})
			return nil
		}}
		return nil
	}

	return nil
}

func (m *board) answerLines(w, body int) []string {
	a := m.ans
	if a == nil {
		return nil
	}
	if a.loading {
		return []string{ansiDim + fit("  reading live prompt…", w) + ansiReset}
	}

	var raw []styledLine
	add := func(style, text string) { raw = append(raw, styledLine{text, style}) }
	para := func(style, indent, text string) {
		for _, l := range wrap(text, max(1, w-len([]rune(indent)))) {
			add(style, indent+l)
		}
	}

	switch a.kind {
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
		add(ansiBold, "Options (press 1-"+strconv.Itoa(len(p.Options))+")")
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
			para(style, "  ", fmt.Sprintf("%s%d. %s%s", cur, o.Number, o.Label, tag))
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
			add(ansiBold, "Options (press 1-"+strconv.Itoa(len(q.Options))+", or t for custom text)")
			for _, o := range q.Options {
				style := ""
				suffix := ""
				if rec != nil && *rec == o.Number {
					style = ansiGreen + ansiBold
					suffix = "  ★ recommended"
				}
				para(style, "  ", fmt.Sprintf("%d. %s%s", o.Number, o.Label, suffix))
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
		visible = append(visible[:body-1:body-1], styledLine{"… more below (j/k or J/K scrolls)", ansiDim})
	}
	out := make([]string, 0, min(len(visible), body))
	for i := 0; i < len(visible) && i < body; i++ {
		out = append(out, visible[i].style+rpcViewFit(visible[i].text, w)+ansiReset)
	}
	return out
}
