package main

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// A needs_me question with enumerated options, written by a worker as one
// note at the tail of its bead:
//
//	QUESTION: Ship to all regions or one first? Recommendation: 2 — one
//	region first, because the new alerts are unproven.
//	OPTIONS:
//	1. All regions at once
//	2. One region first, then the rest
//	CLASS: 7 — production rollout
//
// The CLASS line may sit in the question lines instead of after the options,
// and a CRITICAL: line right before QUESTION: is part of the question.
// Anything else — a free-text question, a RUN: command, a block the thread
// has since written past — parses to nothing, and the bead behaves exactly
// as a needs_me bead always has. A parsed question is what approval taps
// mint one link per option from (tap.go): its hash binds each link to this
// question instance, as a prompt's hash binds a prompt tap.

type NeedsMeQuestion struct {
	// Lines are the question as written: the QUESTION line (its prefix
	// stripped) and any lines between it and OPTIONS, a CRITICAL line
	// first when the worker marked it so.
	Lines   []string
	Options []PromptOption
	// Class is the CLASS line's text after "CLASS:", "" when there is none;
	// ClassN its number, -1 when it carries none.
	Class  string
	ClassN int
	Hash   string
}

var (
	questionOptionRe         = regexp.MustCompile(`^(\d+)\.\s+(.*)$`)
	questionClassRe          = regexp.MustCompile(`^CLASS:\s*(\d)\b`)
	questionRecommendationRe = regexp.MustCompile(`(?i)\bRecommendation:\s*(?:option\s+|#|[*` + "`" + `]+)*(\d+)\b`)
)

// maxQuestionOptions bounds a tappable question: more reads as a numbered
// list in prose, not a decision.
const maxQuestionOptions = 9

// tapChoiceFreeText is the Choice claim on a free-text question tap token,
// and the form choice value that submits free text without picking an option.
const tapChoiceFreeText = "text"

// tapMaxFreeTextRunes caps free-text tap answers (counted in runes), enforced
// before the nonce is spent.
const tapMaxFreeTextRunes = 2000

// normalizeTapFreeText canonicalizes browser form CRLF line endings and strips
// at most one trailing newline added by form submission, preserving everything
// else verbatim.
func normalizeTapFreeText(raw string) string {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	return strings.TrimSuffix(s, "\n")
}

func isAnswerBoundaryLine(l string) bool {
	for _, prefix := range []string{
		"NEEDS_ME ANSWERED:",
		"PROMPT ANSWERED:",
		"the tap answer above was recorded",
		"RAN:",
		"DONE:",
		"JOB CANCELED",
		"WORKFLOW CANCELED",
		"WORKFLOW APPROVED",
	} {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	if l == resumeNote {
		return true
	}
	if strings.HasPrefix(l, "AUTO:") && strings.Contains(l, "] answered ") {
		return true
	}
	return false
}

// parseFreeTextNeedsMeQuestion reads a needs_me bead's live free-text question
// when it has no valid OPTIONS block. RUN: commands, empty notes, and notes
// already answered return false. The hash is taken over the question lines
// (and CLASS when present) with an empty option set.
func parseFreeTextNeedsMeQuestion(notes string) (NeedsMeQuestion, bool) {
	if pendingCommand(notes) != "" {
		return NeedsMeQuestion{}, false
	}
	lines := noteLines(notes)
	if len(lines) == 0 {
		return NeedsMeQuestion{}, false
	}
	base := 0
	for i, l := range lines {
		if isAnswerBoundaryLine(l) {
			base = i + 1
		}
	}
	after := lines[base:]
	if len(after) == 0 {
		return NeedsMeQuestion{}, false
	}

	var q NeedsMeQuestion
	q.ClassN = -1

	qIdx := -1
	for j := len(after) - 1; j >= 0; j-- {
		if strings.HasPrefix(after[j], "QUESTION:") {
			qIdx = j
			break
		}
	}
	if qIdx >= 0 {
		start := qIdx
		if start > 0 && strings.HasPrefix(after[start-1], "CRITICAL:") {
			start--
		}
		for _, l := range after[start:] {
			if class, n, ok := parseClassLine(l); ok {
				if q.Class == "" {
					q.Class, q.ClassN = class, n
				}
				continue
			}
			if isBookkeepingNote(l) {
				continue
			}
			if text := strings.TrimSpace(strings.TrimPrefix(l, "QUESTION:")); text != "" {
				q.Lines = append(q.Lines, text)
			}
		}
	} else {
		var meaningful []string
		for _, l := range after {
			if !isBookkeepingNote(l) {
				meaningful = append(meaningful, l)
			}
		}
		if len(meaningful) == 0 {
			return NeedsMeQuestion{}, false
		}
		n := waitingNotes(meaningful)
		start := len(meaningful) - n
		if start > 0 && strings.HasPrefix(meaningful[start-1], "CRITICAL:") {
			start--
		}
		for _, l := range meaningful[start:] {
			if class, cn, ok := parseClassLine(l); ok {
				if q.Class == "" {
					q.Class, q.ClassN = class, cn
				}
				continue
			}
			q.Lines = append(q.Lines, l)
		}
	}
	if len(q.Lines) == 0 {
		return NeedsMeQuestion{}, false
	}
	hashLines := q.Lines
	if q.Class != "" {
		hashLines = append(append([]string{}, q.Lines...), "CLASS: "+q.Class)
	}
	q.Hash = promptHash(ApprovalPrompt{Lines: hashLines, Options: q.Options})
	return q, true
}

// parseTappableNeedsMeQuestion parses a needs_me bead's live question for
// approval taps: an OPTIONS block when present, otherwise a free-text question.
// RUN: beads never parse as tappable questions.
func parseTappableNeedsMeQuestion(notes string) (NeedsMeQuestion, bool) {
	if pendingCommand(notes) != "" {
		return NeedsMeQuestion{}, false
	}
	if q, ok := parseNeedsMeQuestion(notes); ok {
		return q, true
	}
	return parseFreeTextNeedsMeQuestion(notes)
}

// parseNeedsMeQuestion reads the question a needs_me bead's notes end with.
// Only the last OPTIONS block can be live, and only while nothing but an
// optional CLASS line follows it: a later note — an answer included — is
// the thread moving on, and the block is history.
func parseNeedsMeQuestion(notes string) (NeedsMeQuestion, bool) {
	var lines []string
	for _, l := range strings.Split(notes, "\n") {
		lines = append(lines, strings.TrimSpace(l))
	}
	at := -1
	for i, l := range lines {
		if l == "OPTIONS:" {
			at = i
		}
	}
	if at < 0 {
		return NeedsMeQuestion{}, false
	}
	var q NeedsMeQuestion
	q.ClassN = -1
	i := at + 1
	for i < len(lines) {
		if lines[i] == "" {
			i++
			continue
		}
		m := questionOptionRe.FindStringSubmatch(lines[i])
		if m == nil {
			break
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n != len(q.Options)+1 || strings.TrimSpace(m[2]) == "" {
			return NeedsMeQuestion{}, false
		}
		q.Options = append(q.Options, PromptOption{Number: n, Label: strings.TrimSpace(m[2])})
		i++
	}
	if len(q.Options) < 2 || len(q.Options) > maxQuestionOptions {
		return NeedsMeQuestion{}, false
	}
	// After the options: at most a CLASS line, plus trailing non-answer
	// housekeeping notes (e.g. AUTO: handoff, likely_stale rejected).
	for ; i < len(lines); i++ {
		l := lines[i]
		if l == "" {
			continue
		}
		if class, n, ok := parseClassLine(l); ok && q.Class == "" {
			q.Class, q.ClassN = class, n
			continue
		}
		if isNonAnswerHousekeepingNote(l) {
			continue
		}
		return NeedsMeQuestion{}, false
	}
	// The question: the last QUESTION: line above the block, everything
	// between it and OPTIONS, and a CRITICAL: line right before it.
	start := -1
	for j := at - 1; j >= 0; j-- {
		if strings.HasPrefix(lines[j], "QUESTION:") {
			start = j
			break
		}
	}
	if start < 0 {
		return NeedsMeQuestion{}, false
	}
	if start > 0 && strings.HasPrefix(lines[start-1], "CRITICAL:") {
		q.Lines = append(q.Lines, lines[start-1])
	}
	for _, l := range lines[start:at] {
		if l == "" {
			continue
		}
		if class, n, ok := parseClassLine(l); ok {
			if q.Class == "" {
				q.Class, q.ClassN = class, n
			}
			continue
		}
		if text := strings.TrimSpace(strings.TrimPrefix(l, "QUESTION:")); text != "" {
			q.Lines = append(q.Lines, text)
		}
	}
	if len(q.Lines) == 0 {
		return NeedsMeQuestion{}, false
	}
	hashLines := q.Lines
	if q.Class != "" {
		hashLines = append(append([]string{}, q.Lines...), "CLASS: "+q.Class)
	}
	q.Hash = promptHash(ApprovalPrompt{Lines: hashLines, Options: q.Options})
	return q, true
}

// parseClassLine reads "CLASS: 7 — production rollout": its text after the
// prefix and its number (-1 when it has none in range).
func parseClassLine(l string) (string, int, bool) {
	text, ok := strings.CutPrefix(l, "CLASS:")
	if !ok {
		return "", -1, false
	}
	text = strings.TrimSpace(text)
	n := -1
	if m := questionClassRe.FindStringSubmatch(l); m != nil {
		n, _ = strconv.Atoi(m[1])
	}
	return text, n, true
}

// recommendation extracts the recommended option number from q.Lines when
// one is stated ("Recommendation: 2 — ...") and names one of q.Options.
func (q NeedsMeQuestion) recommendation() *int {
	for _, l := range q.Lines {
		m := questionRecommendationRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err == nil && n >= 1 && n <= len(q.Options) {
			return &n
		}
	}
	return nil
}

// Summary is the question in one line: its first line, critical or not.
func (q NeedsMeQuestion) Summary() string {
	if len(q.Lines) == 0 {
		return "a question"
	}
	return shortTitle(q.Lines[0], 100)
}

// Choices lists the options in one line: 1 All at once · 2 One first.
func (q NeedsMeQuestion) Choices() string {
	return ApprovalPrompt{Options: q.Options}.Choices()
}

// String is the question as the tap links command prints it.
func (q NeedsMeQuestion) String() string {
	var s strings.Builder
	for _, l := range q.Lines {
		s.WriteString(l + "\n")
	}
	if len(q.Options) > 0 {
		s.WriteString("OPTIONS:\n")
		for _, o := range q.Options {
			fmt.Fprintf(&s, "%d. %s\n", o.Number, o.Label)
		}
	}
	if q.Class != "" {
		fmt.Fprintf(&s, "CLASS: %s\n", q.Class)
	}
	return s.String()
}

// choose resolves a tap's choice — always an option number — to its option.
func (q NeedsMeQuestion) choose(choice string) (PromptOption, error) {
	n, err := strconv.Atoi(choice)
	if err != nil || n < 1 || n > len(q.Options) {
		if len(q.Options) == 0 {
			return PromptOption{}, fmt.Errorf("no option %s; this question has no options (use free text)", choice)
		}
		return PromptOption{}, fmt.Errorf("no option %s; the question has %d (%s)", choice, len(q.Options), q.Choices())
	}
	return q.Options[n-1], nil
}

// liveQuestion is the enumerated OPTIONS question of a thread: a needs_me bead
// whose notes end in the OPTIONS convention, not a RUN: command and not a
// worker already working again.
func liveQuestion(t Thread) (NeedsMeQuestion, bool) {
	if t.Bead.Status != StatusNeedsMe || answered(t) || runCommand(t) != "" {
		return NeedsMeQuestion{}, false
	}
	return parseNeedsMeQuestion(t.Bead.Notes)
}

// liveTapQuestion is any tappable question of a thread (with or without an
// OPTIONS block), excluding RUN: commands and workers already working again.
func liveTapQuestion(t Thread) (NeedsMeQuestion, bool) {
	if t.Bead.Status != StatusNeedsMe || answered(t) || runCommand(t) != "" {
		return NeedsMeQuestion{}, false
	}
	return parseTappableNeedsMeQuestion(t.Bead.Notes)
}

// beadQuestion re-reads a bead and its live question, the tap handler's
// view: the bead must still be needs_me and still end in the same question
// a link was minted for (with or without OPTIONS; never RUN:).
func beadQuestion(env promptEnv, id string) (Bead, NeedsMeQuestion, error) {
	b, err := env.show(id)
	if err != nil {
		return Bead{}, NeedsMeQuestion{}, err
	}
	if b.Status != StatusNeedsMe {
		return b, NeedsMeQuestion{}, fmt.Errorf("the question is gone: %s is %s, not needs_me", id, b.Status)
	}
	q, ok := parseTappableNeedsMeQuestion(b.Notes)
	if !ok || pendingCommand(b.Notes) != "" {
		return b, NeedsMeQuestion{}, fmt.Errorf("the question is gone: %s no longer has a live question", id)
	}
	return b, q, nil
}

// questionWorker is a question bead's worker, nil when it has none running.
// A lookup failure reads as none: the answer is still TJ's to record, and
// the caller says the worker was not prompted.
func questionWorker(env promptEnv, b Bead) (string, *Agent) {
	host := beadHost(b)
	if host == "" {
		host = localHost
	}
	a, err := env.agent(host, agentName(b.ID))
	if err != nil {
		return host, nil
	}
	return host, a
}

var errWorkerWorking = errors.New("the worker is working again; it may already have its answer")

// answerNeedsMeVia records TJ's tap answer to a bead's question, mirroring
// what the coordinator does when TJ answers in the coordinator session: the
// answer goes on the bead as NEEDS_ME ANSWERED (by TJ, via naming how it
// arrived), the bead returns to in_progress, and the worker is prompted
// with the answer. The question is re-read first and must still hash to
// expect, and a worker that is working again refuses the tap — it may
// already have its answer another way, and typing into it either way would
// land mid-turn. A worker that is at rest is prompted; one that is blocked
// at a prompt, or gone, is not typed into — the answer stands on the bead
// and a follow-up note says it was not prompted.
func answerNeedsMeVia(env promptEnv, id, choice, expect, via string) (string, error) {
	return answerNeedsMeTextVia(env, id, choice, "", expect, via)
}

// answerNeedsMeTextVia records TJ's tap answer to a bead's needs_me question:
// an option alone, free text alone (choice == "" or tapChoiceFreeText), or an
// option with verbatim free text. Text is assumed already form-normalized and
// is recorded verbatim without trimming or paraphrasing.
func answerNeedsMeTextVia(env promptEnv, id, choice, text, expect, via string) (string, error) {
	if expect == "" {
		return "", fmt.Errorf("--expect <hash> is required: read the question with `quartermaster tap links %s` first", id)
	}
	if n := utf8.RuneCountInString(text); n > tapMaxFreeTextRunes {
		return "", fmt.Errorf("free-text answer is %d characters, over the %d-character limit", n, tapMaxFreeTextRunes)
	}
	freeTextOnly := choice == "" || choice == tapChoiceFreeText
	if freeTextOnly {
		if strings.TrimSpace(text) == "" {
			return "", errors.New("free-text answer cannot be empty")
		}
	} else if strings.TrimSpace(text) == "" {
		text = ""
	}
	b, q, err := beadQuestion(env, id)
	if err != nil {
		return "", err
	}
	if q.Hash != expect {
		if len(q.Options) > 0 {
			return "", fmt.Errorf("the question changed: %s now asks [%s] %s (%s), not [%s]", id, q.Hash, q.Summary(), q.Choices(), expect)
		}
		return "", fmt.Errorf("the question changed: %s now asks [%s] %s, not [%s]", id, q.Hash, q.Summary(), expect)
	}
	var o PromptOption
	if !freeTextOnly {
		o, err = q.choose(choice)
		if err != nil {
			return "", err
		}
	}
	host, a := questionWorker(env, b)
	if a != nil && a.Status == "working" {
		return "", errWorkerWorking
	}
	var note, msg, prompt string
	switch {
	case freeTextOnly:
		note = fmt.Sprintf("NEEDS_ME ANSWERED: free text to %s [%s] by TJ via %s: %s", shortTitle(q.Summary(), 80), q.Hash, via, text)
		msg = fmt.Sprintf("answered %s: free text (%s); the bead is in_progress", id, shortTitle(text, 80))
		prompt = fmt.Sprintf("TJ answered your needs_me question (via %s) with free text:\n%s\nThe question was: %s\nThe bead is back to in_progress and the answer is noted on it as NEEDS_ME ANSWERED. Carry on with that answer.", via, text, q.Summary())
	case text != "":
		note = fmt.Sprintf("NEEDS_ME ANSWERED: %d %s to %s [%s] by TJ via %s: %s", o.Number, shortTitle(o.Label, 60), shortTitle(q.Summary(), 80), q.Hash, via, text)
		msg = fmt.Sprintf("answered %s: %d %s (%s); the bead is in_progress", id, o.Number, o.Label, shortTitle(text, 80))
		prompt = fmt.Sprintf("TJ answered your needs_me question (via %s): %d. %s\nTJ's note:\n%s\nThe question was: %s\nThe bead is back to in_progress and the answer is noted on it as NEEDS_ME ANSWERED. Carry on with that answer.", via, o.Number, o.Label, text, q.Summary())
	default:
		note = fmt.Sprintf("NEEDS_ME ANSWERED: %d %s to %s [%s] by TJ via %s", o.Number, shortTitle(o.Label, 60), shortTitle(q.Summary(), 80), q.Hash, via)
		msg = fmt.Sprintf("answered %s: %d %s; the bead is in_progress", id, o.Number, o.Label)
		prompt = fmt.Sprintf("TJ answered your needs_me question (via %s): %d. %s\nThe question was: %s\nThe bead is back to in_progress and the answer is noted on it as NEEDS_ME ANSWERED. Carry on with that choice.", via, o.Number, o.Label, q.Summary())
	}
	if err := env.note(id, note); err != nil {
		return "", fmt.Errorf("noting the answer on %s failed: %w", id, err)
	}
	if env.setStatus == nil {
		return "", errors.New("the answer is noted but this process cannot set bead status")
	}
	if err := env.setStatus(id, StatusInProgress); err != nil {
		return "", fmt.Errorf("the answer is noted on %s but returning it to in_progress failed: %w", id, err)
	}
	if err := appendJournal(journalRecord{At: time.Now(), Kind: journalNeedsMe, Bead: id, Class: -1, Text: note}); err != nil {
		return "", fmt.Errorf("answered, but journaling it failed: %w", err)
	}
	switch {
	case a == nil:
		return msg + notPrompted(env, id, "no worker is running"), nil
	case !agentReady(a):
		return msg + notPrompted(env, id, fmt.Sprintf("its worker is %s", a.Status)), nil
	case env.tell == nil:
		return msg + notPrompted(env, id, "this process cannot prompt it"), nil
	}
	if err := env.tell(host, a, prompt); err != nil {
		return msg + notPrompted(env, id, "prompting it failed: "+err.Error()), nil
	}
	return msg + "; its worker has been prompted with the answer", nil
}

// notPrompted leaves the follow-up note for an answer whose worker could
// not be prompted, and is the tail of the tap's result saying so.
func notPrompted(env promptEnv, id, why string) string {
	_ = env.note(id, "the tap answer above was recorded but the worker was not prompted ("+why+"); prompt it with the answer")
	return "; its worker was not prompted (" + why + ") — the answer is on the bead"
}
