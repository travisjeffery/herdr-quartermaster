package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// transcript lines as Claude Code writes them, trimmed to the fields read.
const (
	tHuman   = `{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"go with option B"},"timestamp":"%s"}`
	tTask    = `{"type":"user","origin":{"kind":"task-notification","producer":"session-task"},"message":{"role":"user","content":"<task-notification>done</task-notification>"},"timestamp":"%s"}`
	tCompact = `{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"This session is being continued from a previous conversation; the human asked for X"},"timestamp":"%s"}`
	tBound   = `{"type":"system","subtype":"compact_boundary","compactMetadata":{"trigger":"auto"},"timestamp":"%s"}`
	tMeta    = `{"type":"user","isMeta":true,"origin":{"kind":"human"},"message":{"role":"user","content":"<local-command-caveat>"},"timestamp":"%s"}`
)

func at(line string, ts time.Time) string {
	return strings.Replace(line, "%s", ts.UTC().Format(time.RFC3339Nano), 1)
}

func TestHumanPromptAfter(t *testing.T) {
	asked := t0
	cases := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"the question's own prompt is before", []string{at(tHuman, asked.Add(-time.Minute))}, false},
		{"idle compaction", []string{at(tHuman, asked.Add(-time.Minute)), at(tBound, asked.Add(time.Minute)), at(tCompact, asked.Add(time.Minute))}, false},
		{"a background task finished", []string{at(tTask, asked.Add(time.Minute))}, false},
		{"a slash command's caveat", []string{at(tMeta, asked.Add(time.Minute))}, false},
		{"an answer", []string{at(tCompact, asked.Add(time.Minute)), at(tHuman, asked.Add(2*time.Minute))}, true},
	}
	for _, c := range cases {
		// Joined without a trailing newline: the last line is read too.
		if got := humanPromptAfter(strings.NewReader(strings.Join(c.lines, "\n")), asked); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPromptedSinceFindsTheTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	dir := filepath.Join(home, "projects", "-wt-backend-ab12")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s-1.jsonl"), []byte(at(tCompact, t0.Add(time.Minute))+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := agent("working", 6)
	a.Session = AgentSession{Agent: "claude", Kind: "id", Value: "s-1"}
	if prompted, known := promptedSince(a, t0); prompted || !known {
		t.Fatalf("compaction only: prompted=%v known=%v", prompted, known)
	}
	a.Session.Value = "s-2"
	if _, known := promptedSince(a, t0); known {
		t.Fatal("no transcript should be unknown")
	}
	a.Session = AgentSession{Agent: "codex", Kind: "id", Value: "s-1"}
	if _, known := promptedSince(a, t0); known {
		t.Fatal("another kind of agent should be unknown")
	}
}

func TestAgentListCarriesTheSession(t *testing.T) {
	var out struct{ Agents []Agent }
	raw := `{"agents":[{"name":"backend-ab12","agent":"claude","agent_status":"idle","agent_session":{"agent":"claude","kind":"id","source":"herdr:claude","value":"s-1"}}]}`
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out.Agents[0].Session.Value != "s-1" || out.Agents[0].Session.Agent != "claude" {
		t.Fatalf("got %+v, %v", out.Agents, err)
	}
}

// A needs_me worker that compacts while idle, or wakes for a finished
// background task, is not answered: the bead stays needs_me and nothing new
// reaches the inbox, there and back.
func TestWakingOnItsOwnAtNeedsMeIsNotAResume(t *testing.T) {
	b := bead(StatusNeedsMe)
	b.Notes = "Question: roll to all regions or one first?"
	settled := thread(b, agent("idle", 5), nil)
	prev := snapshot(settled, Snapshot{Group: GroupWorking, AgentStatus: "working", AgentSeq: 4}, t0)

	woke := thread(b, agent("working", 6), nil)
	woke.Unprompted = true
	o := transition(woke, prev, false, t0.Add(time.Minute))
	if o.Resume || o.Notify || len(o.Events) != 0 || len(o.Prompts) != 0 {
		t.Fatalf("waking on its own: %+v", o)
	}
	prev = snapshot(woke, prev, t0.Add(time.Minute))

	back := thread(b, agent("idle", 7), nil)
	if o := transition(back, prev, false, t0.Add(2*time.Minute)); o.Resume || o.Notify || len(o.Events) != 0 {
		t.Fatalf("settling again with the same question: %+v", o)
	}
	prev = snapshot(back, prev, t0.Add(2*time.Minute))

	// A new question, asked without leaving needs_me, is announced.
	b.Notes += "\nQuestion: and the canary first?"
	woke = thread(b, agent("working", 8), nil)
	woke.Unprompted = true
	prev = snapshot(woke, prev, t0.Add(3*time.Minute))
	o = transition(thread(b, agent("idle", 9), nil), prev, false, t0.Add(4*time.Minute))
	if !o.Notify || len(o.Events) != 1 || o.Events[0].Kind != EventNeedsYou {
		t.Fatalf("a new question: %+v", o)
	}
}

func TestAPromptAtNeedsMeStillResumes(t *testing.T) {
	b := bead(StatusNeedsMe)
	prev := snapshot(thread(b, agent("idle", 5), nil), Snapshot{Group: GroupWorking, AgentStatus: "working", AgentSeq: 4}, t0)
	if o := transition(thread(b, agent("working", 6), nil), prev, false, t0.Add(time.Minute)); !o.Resume {
		t.Fatalf("an answered worker (or one the ticker can't read) should resume: %+v", o)
	}
}
