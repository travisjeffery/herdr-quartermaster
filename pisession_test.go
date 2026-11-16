package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A pi session as pi writes it: a header, then entries, one JSON object a
// line with no spaces.
const piSession = `{"type":"session","version":3,"id":"b-1","timestamp":"2026-11-16T10:00:00.000Z","cwd":"/w"}
{"type":"message","id":"u1","parentId":null,"timestamp":"2026-11-16T10:00:01.000Z","message":{"role":"user","content":"brief","timestamp":1}}
{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-11-16T10:00:02.000Z","message":{"role":"assistant","content":[],"stopReason":"toolUse","timestamp":2}}
{"type":"message","id":"t1","parentId":"a1","timestamp":"2026-11-16T10:00:03.000Z","message":{"role":"toolResult","toolCallId":"c","content":[],"isError":false,"timestamp":3}}
{"type":"message","id":"a2","parentId":"t1","timestamp":"2026-11-16T10:00:04.000Z","message":{"role":"assistant","content":[],"stopReason":"stop","timestamp":4}}
`

var piT0 = time.Date(2026, 11, 16, 10, 0, 5, 0, time.UTC)

func TestPiPromptedSince(t *testing.T) {
	at := func(d time.Duration) string { return piT0.Add(d).Format(time.RFC3339Nano) }
	user := func(d time.Duration) string {
		return `{"type":"message","id":"u9","timestamp":"` + at(d) + `","message":{"role":"user","content":"x"}}` + "\n"
	}
	mark := func(d time.Duration, source string) string {
		return `{"type":"custom","id":"m9","timestamp":"` + at(d) + `","customType":"quartermaster.input","data":{"source":"` + source + `"}}` + "\n"
	}
	custom := `{"type":"custom_message","id":"c9","timestamp":"` + at(time.Minute) + `","customType":"other","content":"wake","display":false}` + "\n"
	compaction := `{"type":"compaction","id":"k9","timestamp":"` + at(time.Minute) + `","summary":"s","firstKeptEntryId":"u1","tokensBefore":1}` + "\n"
	for _, c := range []struct {
		name, tail string
		want       bool
	}{
		{"nothing since", "", false},
		{"an extension's message woke it", custom, false},
		{"compaction", compaction, false},
		{"a user message, no markers", user(time.Minute), true},
		{"a user message before since", user(-time.Minute), false},
		{"typed, marked", mark(time.Minute, "interactive") + user(time.Minute), true},
		{"rpc, marked", mark(time.Minute, "rpc") + user(time.Minute), true},
		{"sendUserMessage, marked", mark(time.Minute, "extension") + user(time.Minute), false},
		{"quartermaster over the control socket, marked", mark(time.Minute, "quartermaster") + user(time.Minute), true},
		{"marked before since, extension after", mark(-time.Minute, "interactive") + mark(time.Minute, "extension") + user(time.Minute), false},
	} {
		if got := piPromptedSince(strings.NewReader(piSession+c.tail), piT0); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLastTurnIn(t *testing.T) {
	turn := lastTurnIn(strings.NewReader(piSession))
	if turn == nil || turn.ID != "a2" || turn.StopReason != "stop" || turn.Failed() {
		t.Fatalf("last turn = %+v", turn)
	}
	failed := piSession + `{"type":"message","id":"a3","timestamp":"2026-11-16T10:01:00.000Z","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"429 rate limited"}}` + "\n"
	if turn := lastTurnIn(strings.NewReader(failed)); turn == nil || turn.ID != "a3" || !turn.Failed() || turn.Error != "429 rate limited" {
		t.Fatalf("failed turn = %+v", turn)
	}
	// Mid-turn: the tool call doesn't count; the last finished turn stands.
	midTurn := piSession + `{"type":"message","id":"a4","timestamp":"2026-11-16T10:02:00.000Z","message":{"role":"assistant","content":[],"stopReason":"toolUse"}}` + "\n"
	if turn := lastTurnIn(strings.NewReader(midTurn)); turn == nil || turn.ID != "a2" {
		t.Fatalf("mid-turn = %+v", turn)
	}
	// A seek can land mid-line.
	if turn := lastTurnIn(strings.NewReader(`e":"user"}}` + "\n" + piSession)); turn == nil || turn.ID != "a2" {
		t.Fatalf("after a partial line = %+v", turn)
	}
}

func TestPiSessionFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", dir)
	sub := filepath.Join(dir, "--w--")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "2026-11-16T10-00-00-000Z_b-1.jsonl")
	if err := os.WriteFile(path, []byte(piSession), 0o644); err != nil {
		t.Fatal(err)
	}
	byID := &Agent{Session: AgentSession{Agent: "pi", Kind: "id", Value: "b-1"}}
	if got := piSessionFile(byID); got != path {
		t.Errorf("by id: %q", got)
	}
	byPath := &Agent{Session: AgentSession{Agent: "pi", Kind: "path", Value: path}}
	if got := piSessionFile(byPath); got != path {
		t.Errorf("by path: %q", got)
	}
	for _, a := range []*Agent{nil, {Session: AgentSession{Agent: "claude", Kind: "id", Value: "b-1"}}, {Session: AgentSession{Agent: "pi", Kind: "id", Value: "*"}}} {
		if got := piSessionFile(a); got != "" {
			t.Errorf("%+v: %q", a, got)
		}
	}
	if turn := piLastTurn(byPath); turn == nil || turn.ID != "a2" {
		t.Errorf("piLastTurn = %+v", turn)
	}

	// The wake check reads the same file.
	if prompted, known := promptedSince(byPath, piT0); prompted || !known {
		t.Errorf("no prompt since: prompted %v, known %v", prompted, known)
	}
	if prompted, known := promptedSince(byPath, piT0.Add(-5*time.Second)); !prompted || !known {
		t.Errorf("the brief after since: prompted %v, known %v", prompted, known)
	}
	if _, known := promptedSince(&Agent{Session: AgentSession{Agent: "pi", Kind: "id", Value: "gone"}}, piT0); known {
		t.Error("knew about a session it couldn't find")
	}
}

// A pi worker's turn counts as finished when its session shows a new turn
// end, even if herdr's state never showed it working between two passes.
func TestPiTurnFinishes(t *testing.T) {
	now := time.Now()
	idle := &Agent{Name: "b-1", Kind: "pi", Status: "idle", Seq: 5}
	b := Bead{ID: "b-1", Status: StatusInProgress}
	prev := Snapshot{AgentStatus: "idle", AgentSeq: 5, TurnID: "a2"}
	finished := func(o Outcome) string {
		for _, e := range o.Events {
			if e.Kind == EventFinished {
				return e.Summary
			}
		}
		return ""
	}
	same := Thread{Bead: b, Agent: idle, Turn: &PiTurn{ID: "a2", StopReason: "stop"}}
	if s := finished(transition(same, prev, false, now)); s != "" {
		t.Errorf("same turn reported again: %q", s)
	}
	next := Thread{Bead: b, Agent: idle, Turn: &PiTurn{ID: "a5", StopReason: "stop"}}
	if s := finished(transition(next, prev, false, now)); !strings.Contains(s, "finished a turn") {
		t.Errorf("new turn end not reported: %q", s)
	}
	failed := Thread{Bead: b, Agent: idle, Turn: &PiTurn{ID: "a6", StopReason: "error", Error: "429 rate limited"}}
	if s := finished(transition(failed, prev, false, now)); !strings.Contains(s, "stopped on error: 429 rate limited") {
		t.Errorf("failed turn: %q", s)
	}
	// The first look only sets the cursor.
	if s := finished(transition(next, Snapshot{AgentStatus: "idle", AgentSeq: 5}, false, now)); s != "" {
		t.Errorf("baseline reported a turn: %q", s)
	}
	if got := snapshot(next, prev, now).TurnID; got != "a5" {
		t.Errorf("snapshot TurnID = %q", got)
	}
	if got := snapshot(Thread{Bead: b, Agent: idle}, prev, now).TurnID; got != "a2" {
		t.Errorf("an unreadable session lost the cursor: %q", got)
	}
}
