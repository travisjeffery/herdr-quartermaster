package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ApprovalPrompt is a selection prompt an agent's pane is showing: a tool
// approval, a folder-trust question. It is parsed from the screen text, which
// the agent controls, so it describes what is shown, never what is safe.
type ApprovalPrompt struct {
	// Lines are the prompt above its options: its title ("Bash command"), the
	// command or file it is about, and its question. Separators are dropped.
	Lines   []string
	Options []PromptOption
	// Numbered menus answer with the option's digit; the others by moving the
	// cursor and pressing enter.
	Numbered bool
	// Countdown is how long until the agent denies the request on its own
	// (Claude Code's auto-deny), "" when it shows none.
	Countdown string
	// Hash identifies the prompt across reads: the cursor, the footer and the
	// countdown, which change while the same prompt shows, are left out.
	Hash string
}

type PromptOption struct {
	Number   int // 1-based position
	Label    string
	Selected bool
}

var (
	optionRe    = regexp.MustCompile(`^(\s*)([❯›>]\s*)?(\d+)\.\s+(.*)$`)
	cursorRe    = regexp.MustCompile(`^(\s*)[❯›>]\s+(.*)$`)
	footerRe    = regexp.MustCompile(`(?i)\b(esc|enter) to\b|press enter|enter to confirm`)
	countdownRe = regexp.MustCompile(`will automatically deny this request in (.+?), to avoid`)
)

// promptHeaderMax bounds how far above its options a prompt's text is looked
// for when no rule line marks its top.
const promptHeaderMax = 20

// parsePrompt finds the selection prompt at the bottom of a pane's text. A
// prompt ends in a footer naming esc or enter ("Esc to cancel"), right below
// its options, with one of them under the cursor; anything else on screen, a
// numbered list in the conversation included, is not a prompt.
func parsePrompt(screen string) (ApprovalPrompt, bool) {
	lines := strings.Split(strings.ReplaceAll(screen, "\r", ""), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	end := len(lines) - 1
	for end >= 0 && lines[end] == "" {
		end--
	}
	if end < 0 || !footerRe.MatchString(lines[end]) {
		return ApprovalPrompt{}, false
	}
	// The option block: the non-blank lines right above the footer.
	last := end - 1
	for last >= 0 && lines[last] == "" {
		last--
	}
	if last < 0 || isRule(lines[last]) {
		return ApprovalPrompt{}, false
	}
	first := last
	for first > 0 && lines[first-1] != "" && !isRule(lines[first-1]) {
		first--
	}
	opts, numbered, skip, ok := parseOptions(lines[first : last+1])
	if !ok {
		return ApprovalPrompt{}, false
	}
	// The question can sit right above the options with no blank line between.
	first += skip
	var p ApprovalPrompt
	p.Options, p.Numbered = opts, numbered
	// The prompt's text: up to the rule line above it, or a bounded look-up.
	top := first - promptHeaderMax
	for i := first - 1; i >= 0 && i >= first-promptHeaderMax; i-- {
		if isRule(lines[i]) {
			top = i + 1
			break
		}
	}
	top = max(top, 0)
	for _, l := range lines[top:first] {
		l = strings.TrimSpace(l)
		if l == "" || isSeparator(l) {
			continue
		}
		if m := countdownRe.FindStringSubmatch(l); m != nil {
			p.Countdown = m[1]
			continue
		}
		p.Lines = append(p.Lines, l)
	}
	p.Hash = promptHash(p)
	return p, true
}

// parseOptions reads an option block: a numbered menu (1. Yes, 2. No, with
// wrapped labels continuing on indented lines) or a plain one, each option on
// its own line, one of them under the cursor. Lines before the first option
// are the question; skip is how many there are.
func parseOptions(block []string) (opts []PromptOption, numbered bool, skip int, ok bool) {
	// The menu is the last "1." that starts one: a file or command shown above
	// it can hold numbered lines of its own.
	for start := len(block) - 1; start >= 0; start-- {
		if m := optionRe.FindStringSubmatch(block[start]); m != nil && m[3] == "1" {
			if opts, ok := numberedMenu(block[start:]); ok {
				return opts, true, start, true
			}
		}
	}
	// A plain menu: the cursor line and the lines at its label's column, at the
	// end of the block.
	col := -1
	for _, l := range block {
		if m := cursorRe.FindStringSubmatch(l); m != nil {
			col = len([]rune(l)) - len([]rune(m[2]))
			break
		}
	}
	if col < 0 {
		return nil, false, 0, false
	}
	for i, l := range block {
		if m := cursorRe.FindStringSubmatch(l); m != nil && len([]rune(l))-len([]rune(m[2])) == col {
			opts = append(opts, PromptOption{Number: len(opts) + 1, Label: strings.TrimSpace(m[2]), Selected: true})
		} else if indent(l) == col {
			opts = append(opts, PromptOption{Number: len(opts) + 1, Label: strings.TrimSpace(l)})
		} else {
			// The question above the options, at another indent.
			opts, skip = nil, i+1
		}
	}
	return opts, false, skip, validMenu(opts)
}

// numberedMenu reads lines from option 1 on: each option numbered in turn, a
// wrapped label continuing on lines indented at least to its own column.
func numberedMenu(lines []string) ([]PromptOption, bool) {
	var opts []PromptOption
	textCol := 0
	for _, l := range lines {
		if m := optionRe.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[3])
			if n != len(opts)+1 {
				return nil, false
			}
			opts = append(opts, PromptOption{Number: n, Label: strings.TrimSpace(m[4]), Selected: m[2] != ""})
			textCol = len([]rune(l)) - len([]rune(m[4]))
			continue
		}
		if indent(l) < textCol {
			return nil, false
		}
		opts[len(opts)-1].Label += " " + strings.TrimSpace(l)
	}
	return opts, validMenu(opts)
}

// validMenu is at least two options with exactly one under the cursor.
func validMenu(opts []PromptOption) bool {
	sel := 0
	for _, o := range opts {
		if o.Selected {
			sel++
		}
	}
	return len(opts) >= 2 && sel == 1
}

func indent(l string) int { return len([]rune(l)) - len([]rune(strings.TrimLeft(l, " "))) }

// isRule is a box-drawing line that marks the top of a prompt.
func isRule(l string) bool {
	l = strings.TrimSpace(l)
	return len([]rune(l)) >= 20 && strings.Trim(l, "─━") == ""
}

// isSeparator is a dashed line inside a prompt, around the command it shows.
func isSeparator(l string) bool {
	return strings.Trim(l, "─━╌┄┈-") == ""
}

// promptHash is a short digest of the prompt's text and options, whitespace
// collapsed, so the same prompt hashes the same on every read.
func promptHash(p ApprovalPrompt) string {
	var b strings.Builder
	for _, l := range p.Lines {
		b.WriteString(strings.Join(strings.Fields(l), " "))
		b.WriteByte('\n')
	}
	for _, o := range p.Options {
		fmt.Fprintf(&b, "%d. %s\n", o.Number, strings.Join(strings.Fields(o.Label), " "))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:8]
}

// Summary is the prompt in one line: its title and what it is about.
func (p ApprovalPrompt) Summary() string {
	switch len(p.Lines) {
	case 0:
		return "a prompt"
	case 1:
		return shortTitle(p.Lines[0], 100)
	}
	// The second line is the command or file, unless it is a tip.
	detail := p.Lines[1]
	for _, l := range p.Lines[1:] {
		if !strings.HasPrefix(l, "Tip:") {
			detail = l
			break
		}
	}
	return shortTitle(p.Lines[0]+": "+detail, 100)
}

// Choices lists the options in one line: 1 Yes · 2 No.
func (p ApprovalPrompt) Choices() string {
	var parts []string
	for _, o := range p.Options {
		parts = append(parts, fmt.Sprintf("%d %s", o.Number, shortTitle(o.Label, 50)))
	}
	return strings.Join(parts, " · ")
}

// String is the prompt as the prompt command prints it.
func (p ApprovalPrompt) String() string {
	var s strings.Builder
	for _, l := range p.Lines {
		s.WriteString(l + "\n")
	}
	for _, o := range p.Options {
		cur := "  "
		if o.Selected {
			cur = "❯ "
		}
		fmt.Fprintf(&s, "%s%d. %s\n", cur, o.Number, o.Label)
	}
	if p.Countdown != "" {
		fmt.Fprintf(&s, "auto-deny in %s\n", p.Countdown)
	}
	return s.String()
}

// choose resolves an answer to an option: its number, its label, or a prefix
// of exactly one label, ignoring case.
func (p ApprovalPrompt) choose(choice string) (PromptOption, error) {
	if n, err := strconv.Atoi(choice); err == nil {
		if n < 1 || n > len(p.Options) {
			return PromptOption{}, fmt.Errorf("no option %d; the prompt has %d", n, len(p.Options))
		}
		return p.Options[n-1], nil
	}
	c := strings.ToLower(strings.TrimSpace(choice))
	var prefixed []PromptOption
	for _, o := range p.Options {
		l := strings.ToLower(o.Label)
		if l == c {
			return o, nil
		}
		if c != "" && strings.HasPrefix(l, c) {
			prefixed = append(prefixed, o)
		}
	}
	if len(prefixed) == 1 {
		return prefixed[0], nil
	}
	return PromptOption{}, fmt.Errorf("%q matches %d options (%s); answer with its number", choice, len(prefixed), p.Choices())
}

// keys are the key presses that pick o: its digit in a numbered menu, else
// the cursor moved onto it and enter.
func (p ApprovalPrompt) keys(o PromptOption) []string {
	if p.Numbered {
		return []string{strconv.Itoa(o.Number)}
	}
	cur := 0
	for i, x := range p.Options {
		if x.Selected {
			cur = i
		}
	}
	var keys []string
	for i := cur; i < o.Number-1; i++ {
		keys = append(keys, "down")
	}
	for i := cur; i > o.Number-1; i-- {
		keys = append(keys, "up")
	}
	return append(keys, "enter")
}

// promptEnv is what the prompt and answer commands read and change.
type promptEnv struct {
	show  func(id string) (Bead, error)
	agent func(host, name string) (*Agent, error)
	read  func(host, pane string) (string, error)
	keys  func(host, pane string, keys ...string) error
	note  func(id, text string) error
	sleep func(time.Duration)
}

func livePromptEnv(cfg Config, h Herdr) promptEnv {
	bd := Beads{}
	return promptEnv{
		show:  bd.Show,
		agent: agentOn(cfg, h),
		read: func(host, pane string) (string, error) {
			return cfg.herdrOn(h, host).Read(pane, promptReadLines, callTimeout)
		},
		keys: func(host, pane string, keys ...string) error {
			return cfg.herdrOn(h, host).SendKeys(pane, keys...)
		},
		note:  bd.Note,
		sleep: time.Sleep,
	}
}

// promptReadLines is how much of the screen is read: a prompt with a long
// command still fits.
const promptReadLines = 200

var errNoPrompt = errors.New("no prompt showing")

// workerPrompt reads the prompt a bead's worker is showing.
func workerPrompt(env promptEnv, id string) (ApprovalPrompt, string, *Agent, error) {
	b, err := env.show(id)
	if err != nil {
		return ApprovalPrompt{}, "", nil, err
	}
	host, name := beadHost(b), agentName(b.ID)
	if host == "" {
		host = localHost
	}
	a, err := env.agent(host, name)
	if err != nil {
		return ApprovalPrompt{}, host, nil, fmt.Errorf("find the worker%s: %w", onHost(host), err)
	}
	if a == nil {
		return ApprovalPrompt{}, host, nil, fmt.Errorf("%s has no worker running%s", id, onHost(host))
	}
	screen, err := env.read(host, a.PaneID)
	if err != nil {
		return ApprovalPrompt{}, host, a, err
	}
	p, ok := parsePrompt(screen)
	if !ok {
		return ApprovalPrompt{}, host, a, fmt.Errorf("%s (%s): %w", name, a.Status, errNoPrompt)
	}
	return p, host, a, nil
}

// showPrompt is the prompt command: the worker's prompt, its hash, and how to
// answer it.
func showPrompt(env promptEnv, id string) (string, error) {
	p, _, a, err := workerPrompt(env, id)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s (%s) is at a prompt [%s]\n%s\nanswer: quartermaster answer %s <choice> --expect %s\n",
		a.Name, a.Status, p.Hash, p.String(), id, p.Hash), nil
}

// answerWait is how long answer waits for the prompt to clear.
const answerWait = 5 * time.Second

// answerPrompt is the answer command. It re-reads the worker's screen and
// presses the keys only if the prompt with hash expect is still the one
// showing, so an answer never lands on a prompt it wasn't meant for. It then
// waits for that prompt to go and notes the answer on the bead.
func answerPrompt(env promptEnv, id, choice, expect, by string) (string, error) {
	if expect == "" {
		return "", fmt.Errorf("--expect <hash> is required: read the prompt with `quartermaster prompt %s` first", id)
	}
	p, host, a, err := workerPrompt(env, id)
	if err != nil {
		return "", err
	}
	if p.Hash != expect {
		return "", fmt.Errorf("the prompt changed: %s now shows [%s] %s (%s), not [%s]; read it again with `quartermaster prompt %s`",
			a.Name, p.Hash, p.Summary(), p.Choices(), expect, id)
	}
	o, err := p.choose(choice)
	if err != nil {
		return "", err
	}
	if err := env.keys(host, a.PaneID, p.keys(o)...); err != nil {
		return "", fmt.Errorf("send keys to %s: %w", a.Name, err)
	}
	cleared := false
	for waited := time.Duration(0); waited < answerWait; waited += 250 * time.Millisecond {
		env.sleep(250 * time.Millisecond)
		screen, err := env.read(host, a.PaneID)
		if err != nil {
			continue
		}
		if now, ok := parsePrompt(screen); !ok || now.Hash != expect {
			cleared = true
			break
		}
	}
	if !cleared {
		return "", fmt.Errorf("sent %q to %s but prompt [%s] is still showing", strings.Join(p.keys(o), " "), a.Name, expect)
	}
	msg := fmt.Sprintf("answered %s: %d %s to %s", a.Name, o.Number, o.Label, p.Summary())
	note := fmt.Sprintf("PROMPT ANSWERED: %d %s to %s [%s] by %s", o.Number, shortTitle(o.Label, 60), p.Summary(), p.Hash, by)
	if err := env.note(id, note); err != nil {
		return msg, fmt.Errorf("answered, but noting it on %s failed: %w", id, err)
	}
	return msg, nil
}
