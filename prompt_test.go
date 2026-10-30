package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// screen is a pane capture from testdata/prompts.
func screen(t *testing.T, name string) string {
	t.Helper()
	return string(fixture(t, "prompts/"+name))
}

func labels(p ApprovalPrompt) []string {
	var out []string
	for _, o := range p.Options {
		out = append(out, o.Label)
	}
	return out
}

func TestParsePromptClaudeBash(t *testing.T) {
	p, ok := parsePrompt(screen(t, "claude-bash-4.txt"))
	if !ok {
		t.Fatal("no prompt found")
	}
	if !p.Numbered || len(p.Options) != 4 || !p.Options[0].Selected {
		t.Fatalf("options = %+v", p.Options)
	}
	if got := p.Options[1].Label; !strings.HasPrefix(got, "Yes, and don't ask again for mkdir") || !strings.HasSuffix(got, "/0123 4567/worktree") {
		t.Errorf("wrapped option 2 = %q", got)
	}
	if p.Options[3].Label != "No" {
		t.Errorf("option 4 = %q", p.Options[3].Label)
	}
	if p.Lines[0] != "Bash command" || p.Lines[len(p.Lines)-1] != "Do you want to proceed?" {
		t.Errorf("lines = %q", p.Lines)
	}
	if got := p.Summary(); got != "Bash command: Create out2 directory and touch hello.txt" {
		t.Errorf("summary = %q", got)
	}
	if !slices.Contains(p.Lines, "mkdir -p out2 && touch out2/hello.txt") {
		t.Errorf("command missing from %q", p.Lines)
	}
	if p.Countdown != "" || len(p.Hash) != 8 {
		t.Errorf("countdown %q hash %q", p.Countdown, p.Hash)
	}
}

func TestParsePromptClaudeWrite(t *testing.T) {
	p, ok := parsePrompt(screen(t, "claude-write-3.txt"))
	if !ok {
		t.Fatal("no prompt found")
	}
	want := []string{"Yes", "Yes, and switch to accept edits (auto-approve file edits and common file commands) for this session (shift+tab)", "No"}
	if !slices.Equal(labels(p), want) {
		t.Errorf("options = %q", labels(p))
	}
	if got := p.Summary(); got != "Create file: notes2.md" {
		t.Errorf("summary = %q", got)
	}
}

func TestParsePromptAutoDeny(t *testing.T) {
	p, ok := parsePrompt(screen(t, "claude-autodeny.txt"))
	if !ok {
		t.Fatal("no prompt found")
	}
	if p.Countdown != "1:42" {
		t.Errorf("countdown = %q", p.Countdown)
	}
	// The countdown ticks while the prompt stays the same one.
	plain, _ := parsePrompt(screen(t, "claude-bash-4.txt"))
	if p.Hash != plain.Hash {
		t.Errorf("countdown changed the hash: %s vs %s", p.Hash, plain.Hash)
	}
	if !strings.Contains(p.String(), "auto-deny in 1:42") {
		t.Errorf("String() = %q", p.String())
	}
}

func TestParsePromptTrust(t *testing.T) {
	p, ok := parsePrompt(screen(t, "claude-trust.txt"))
	if !ok {
		t.Fatal("no prompt found")
	}
	if p.Numbered || !slices.Equal(labels(p), []string{"No, exit", "Yes, I trust this folder"}) || !p.Options[0].Selected {
		t.Fatalf("options = %+v", p.Options)
	}
	if p.Lines[0] != "Accessing workspace:" {
		t.Errorf("lines = %q", p.Lines)
	}
	o, err := p.choose("yes")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.keys(o); !slices.Equal(got, []string{"down", "enter"}) {
		t.Errorf("keys = %q", got)
	}
}

func TestParsePromptNone(t *testing.T) {
	if p, ok := parsePrompt(screen(t, "claude-idle.txt")); ok {
		t.Errorf("idle screen parsed as %+v", p)
	}
	working := "● Here is the plan:\n  1. Read the code\n  2. Fix it\n\n────────────────────────────────\n❯ \n────────────────────────────────\n  ⏸ manual mode on · esc to interrupt · ← for agents\n"
	if p, ok := parsePrompt(working); ok {
		t.Errorf("working screen parsed as %+v", p)
	}
	if _, ok := parsePrompt(""); ok {
		t.Error("empty screen parsed as a prompt")
	}
}

func TestPromptHashIgnoresCursorAndFooter(t *testing.T) {
	screen := screen(t, "claude-bash-4.txt")
	moved := strings.Replace(strings.Replace(screen, " ❯ 1. Yes", "   1. Yes", 1), "   2. Yes, and", " ❯ 2. Yes, and", 1)
	moved = strings.Replace(moved, "Esc to cancel · Tab to amend", "Esc to cancel", 1)
	a, _ := parsePrompt(screen)
	b, ok := parsePrompt(moved)
	if !ok || a.Hash != b.Hash || !b.Options[1].Selected {
		t.Errorf("hash %s vs %s (ok %v)", a.Hash, b.Hash, ok)
	}
	other, _ := parsePrompt(strings.Replace(screen, "\n mkdir -p out2", "\n rm -rf out2", 1))
	if other.Hash == a.Hash {
		t.Error("a different command hashed the same")
	}
}

func TestChoose(t *testing.T) {
	p, _ := parsePrompt(screen(t, "claude-bash-4.txt"))
	for choice, want := range map[string]int{"1": 1, "4": 4, "no": 4, "No": 4, "yes": 1, "yes, and switch": 3} {
		o, err := p.choose(choice)
		if err != nil || o.Number != want {
			t.Errorf("choose(%q) = %d, %v; want %d", choice, o.Number, err, want)
		}
	}
	for _, bad := range []string{"0", "5", "yes,", "maybe"} {
		if _, err := p.choose(bad); err == nil {
			t.Errorf("choose(%q) succeeded", bad)
		}
	}
	o, _ := p.choose("4")
	if got := p.keys(o); !slices.Equal(got, []string{"4"}) {
		t.Errorf("keys = %q", got)
	}
}

// fakePane is a worker's screen: keys replace it with what follows them.
type fakePane struct {
	screens []string
	sent    [][]string
	notes   []string
}

func (f *fakePane) env() promptEnv {
	return promptEnv{
		show: func(id string) (Bead, error) { return Bead{ID: id, Status: StatusInProgress}, nil },
		agent: func(host, name string) (*Agent, error) {
			return &Agent{Name: name, Status: "blocked", PaneID: "w1:p1"}, nil
		},
		read: func(host, pane string) (string, error) { return f.screens[0], nil },
		keys: func(host, pane string, keys ...string) error {
			f.sent = append(f.sent, keys)
			if len(f.screens) > 1 {
				f.screens = f.screens[1:]
			}
			return nil
		},
		note:  func(id, text string) error { f.notes = append(f.notes, text); return nil },
		sleep: func(time.Duration) {},
	}
}

func TestAnswerPrompt(t *testing.T) {
	bash := screen(t, "claude-bash-4.txt")
	p, _ := parsePrompt(bash)
	f := &fakePane{screens: []string{bash, screen(t, "claude-idle.txt")}}
	msg, err := answerPrompt(f.env(), "backend-ab12", "1", p.Hash, "TJ")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || !slices.Equal(f.sent[0], []string{"1"}) {
		t.Errorf("sent %q", f.sent)
	}
	if !strings.Contains(msg, "1 Yes") {
		t.Errorf("msg = %q", msg)
	}
	want := "PROMPT ANSWERED: 1 Yes to Bash command: Create out2 directory and touch hello.txt [" + p.Hash + "] by TJ"
	if !slices.Equal(f.notes, []string{want}) {
		t.Errorf("notes = %q", f.notes)
	}
}

func TestAnswerPromptRefusesChangedPrompt(t *testing.T) {
	bash, _ := parsePrompt(screen(t, "claude-bash-4.txt"))
	f := &fakePane{screens: []string{screen(t, "claude-write-3.txt")}}
	_, err := answerPrompt(f.env(), "backend-ab12", "1", bash.Hash, "coordinator")
	if err == nil || !strings.Contains(err.Error(), "the prompt changed") {
		t.Fatalf("err = %v", err)
	}
	if len(f.sent) != 0 || len(f.notes) != 0 {
		t.Errorf("sent %q, noted %q", f.sent, f.notes)
	}
}

func TestAnswerPromptGone(t *testing.T) {
	f := &fakePane{screens: []string{screen(t, "claude-idle.txt")}}
	_, err := answerPrompt(f.env(), "backend-ab12", "1", "abcd1234", "coordinator")
	if !errors.Is(err, errNoPrompt) || len(f.sent) != 0 {
		t.Fatalf("err = %v, sent %q", err, f.sent)
	}
	if _, err := answerPrompt(f.env(), "backend-ab12", "1", "", "coordinator"); err == nil || !strings.Contains(err.Error(), "--expect") {
		t.Errorf("no --expect: err = %v", err)
	}
}

func TestAnswerPromptStillShowing(t *testing.T) {
	bash := screen(t, "claude-bash-4.txt")
	p, _ := parsePrompt(bash)
	f := &fakePane{screens: []string{bash}}
	_, err := answerPrompt(f.env(), "backend-ab12", "no", p.Hash, "coordinator")
	if err == nil || !strings.Contains(err.Error(), "still showing") {
		t.Fatalf("err = %v", err)
	}
	if len(f.notes) != 0 {
		t.Errorf("noted an unconfirmed answer: %q", f.notes)
	}
}

func TestShowPrompt(t *testing.T) {
	f := &fakePane{screens: []string{screen(t, "claude-write-3.txt")}}
	out, err := showPrompt(f.env(), "backend-ab12")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := parsePrompt(f.screens[0])
	for _, want := range []string{"[" + p.Hash + "]", "Create file\nnotes2.md\n", "❯ 1. Yes\n", "  3. No\n", "quartermaster answer backend-ab12 <choice> --expect " + p.Hash} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestBlockedPromptReachesInboxOncePerPrompt(t *testing.T) {
	bash, _ := parsePrompt(screen(t, "claude-autodeny.txt"))
	th := thread(bead(StatusInProgress), agent("blocked", 2), nil)
	th.Prompt = &bash
	prev := Snapshot{Group: GroupWorking, AgentStatus: "working"}
	o := transition(th, prev, false, t0)
	if len(o.Events) != 1 || !o.Notify {
		t.Fatalf("events = %+v", o.Events)
	}
	want := "backend-ab12 is at an approval prompt [" + bash.Hash + "]: Bash command: Create out2 directory and touch hello.txt (1 Yes · 2 Yes, and don't ask again for mkdir -p out2 and… · 3 Yes, and switch to auto mode · auto mode handles… · 4 No, auto-denies in 1:42); answer with `quartermaster answer backend-ab12 <choice> --expect " + bash.Hash + "`"
	if o.Events[0].Summary != want {
		t.Errorf("summary =\n%s\nwant\n%s", o.Events[0].Summary, want)
	}
	prev = snapshot(th, prev, t0)
	if o := transition(th, prev, false, t0.Add(time.Minute)); len(o.Events) != 0 {
		t.Errorf("same prompt announced again: %+v", o.Events)
	}
	write, _ := parsePrompt(screen(t, "claude-write-3.txt"))
	th.Prompt = &write
	if o := transition(th, prev, false, t0.Add(2*time.Minute)); len(o.Events) != 1 || !strings.Contains(o.Events[0].Summary, "["+write.Hash+"]: Create file: notes2.md") {
		t.Errorf("the next prompt: %+v", o.Events)
	}
	// Unreadable, the prompt is still announced as before.
	th.Prompt = nil
	if o := transition(th, Snapshot{Group: GroupWorking}, false, t0); len(o.Events) != 1 || !strings.Contains(o.Events[0].Summary, "waiting at an approval or question prompt") {
		t.Errorf("no prompt read: %+v", o.Events)
	}
}
