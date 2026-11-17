package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRPCViewResetWithPartialAppend(t *testing.T) {
	path := viewFixture(t)
	viewWrite(t, path, viewMessage(1, "old history"))
	p, err := rpcViewRead("demo-1", "history", "")
	if err != nil {
		t.Fatal(err)
	}
	var track rpcViewTrack
	track.append(p, false)
	if track.usage.Total != 19 {
		t.Fatal(track.usage)
	}
	replacement := path + ".new"
	viewWrite(t, replacement, viewMessage(2, "new history")+`{"type":"message"`)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	p, err = rpcViewRead("demo-1", "history", p.Cursor)
	if err != nil || !p.Reset || !strings.Contains(p.Gap, "incomplete") {
		t.Fatal(p, err)
	}
	track.append(p, false)
	if track.usage.Total != 19 || len(track.events) != 1 || track.events[0].Text != "new history" {
		t.Fatal("mixed history on reset", track)
	}
}
func TestRPCViewStaleSnapshotDoesNotInventElapsedProgress(t *testing.T) {
	v := rpcVisibility{State: "running", Connected: true, Started: t0, ActivityAt: t0.Add(time.Second), Observed: t0.Add(2 * time.Second)}
	line := rpcVisibilityLine(v, t0.Add(time.Hour))
	if !strings.Contains(line, "stale snapshot") || !strings.Contains(line, "run observed 2s") {
		t.Fatal(line)
	}
	if !strings.Contains(rpcBoardBrief(v, t0.Add(time.Hour)), "stale snapshot") {
		t.Fatal("board hid stale sample")
	}
}
func TestRPCViewLogTimestampHeadroom(t *testing.T) {
	// Adding the timestamp must not make a valid maximum-sized pi frame
	// unreadable by legacy log readers or the viewer.
	prefix, suffix := `{"type":"unknown","text":"`, `"}`
	raw := prefix + strings.Repeat("x", rpcMaxRecord-len(prefix)-len(suffix)-1) + suffix
	logged := `{"_quartermaster":{"at":"2026-01-02T03:04:05.123456789Z"},` + raw[1:] + "\n"
	called := false
	err := readRPCLog(strings.NewReader(logged), func([]byte) error { called = true; return nil })
	if err != io.EOF || !called {
		t.Fatal(err, called)
	}
}
func TestRPCViewModelSurvivesEvictionAndUnknownUsage(t *testing.T) {
	var track rpcViewTrack
	track.append(rpcViewPage{Cursor: "first", Events: []rpcViewEvent{{Provider: "example", Model: "model-one"}}}, false)
	for i := 0; i < 200; i++ {
		track.append(rpcViewPage{Cursor: "next", Events: []rpcViewEvent{{Label: "tool start"}}}, false)
	}
	if track.model != "model-one" || track.provider != "example" {
		t.Fatal("model lost to retention")
	}
	track.append(rpcViewPage{Cursor: "next", Events: []rpcViewEvent{{Kind: "gap", Label: "malformed record"}}}, false)
	if !track.incomplete {
		t.Fatal("missing usage called complete")
	}
	v := newRPCViewer("demo-1", "Fixture", nil, rpcViewEnv{})
	v.history = track
	v.apply(rpcViewResult{generation: 0, history: rpcViewPage{Cursor: "next"}}, time.Now())
	if v.visibility.Model != "model-one" {
		t.Fatal("recorded model not surfaced")
	}
	out := v.render(100, 20, time.Now())
	if !strings.Contains(out, "usage unknown") {
		t.Fatal("missing usage invented")
	}
	track.append(rpcViewPage{Cursor: "new", Reset: true}, false)
	if track.model != "" || track.incomplete {
		t.Fatal("replacement kept old model/completeness")
	}
}
func TestRPCViewFreshHandoffMessageIsHistoryNotPrompt(t *testing.T) {
	path := viewFixture(t)
	viewWrite(t, path, `{"type":"custom_message","id":"handoff","timestamp":"2026-01-02T03:04:05Z","customType":"quartermaster.handoff","display":true,"content":"Unanswered question: proceed with the example change?","details":{"token":"do-not-project"}}`+"\n")
	before, _ := os.ReadFile(path)
	p, err := rpcViewRead("demo-1", "history", "")
	if err != nil || len(p.Events) != 1 || p.Events[0].Label != "saved extension message" || !strings.Contains(p.Events[0].Text, "Unanswered question") {
		t.Fatal(p, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("handoff view wrote session")
	}
	if _, err := os.Stat(filepath.Join(rpcDir("demo-1"), "requests")); !os.IsNotExist(err) {
		t.Fatal("view dispatched prompt")
	}
}
func TestRPCViewSkippedAndOversizedRecordsBounded(t *testing.T) {
	path := viewFixture(t)
	var b strings.Builder
	// A page of irrelevant system/custom records cannot scan the entire file.
	for i := 0; i < 1000; i++ {
		b.WriteString(`{"type":"custom","data":"ignored"}` + "\n")
	}
	b.WriteString(viewMessage(1, "after skipped records"))
	viewWrite(t, path, b.String())
	p, err := rpcViewRead("demo-1", "history", "")
	if err != nil || !p.More || len(p.Events) != 0 {
		t.Fatal(p, err)
	}
	viewWrite(t, path, strings.Repeat("x", rpcMaxRecord+1)+"\n")
	if _, err := rpcViewRead("demo-1", "history", ""); err == nil {
		t.Fatal("oversized record accepted")
	}
}
func TestRPCViewSlowConsumerLeavesWorkerIndependent(t *testing.T) {
	launch := fixtureSupervisor(t)
	launch()
	if _, err := rpcClient("b-1", rpcRequest{Op: "prompt", ID: "first", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	p, err := rpcViewRead("b-1", "events", "")
	if err != nil {
		t.Fatal(err)
	}
	// Hold this cursor without reading while the real pipe fixture continues.
	// No subscriber queue or worker lock exists on this path.
	for i := 0; i < 20; i++ {
		if _, err := rpcClient("b-1", rpcRequest{Op: "prompt", ID: rpcID(), Text: "hello"}); err != nil {
			t.Fatal(err)
		}
	}
	waitRPC(t, func() bool {
		b, _ := os.ReadFile(filepath.Join(rpcDir("b-1"), "events.jsonl"))
		return strings.Count(string(b), `"type":"agent_settled"`) == 21
	})
	cursor := p.Cursor
	starts := 0
	for {
		next, err := rpcViewRead("b-1", "events", cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range next.Events {
			if e.Kind == "agent_start" {
				starts++
			}
		}
		cursor = next.Cursor
		if !next.More {
			break
		}
	}
	if starts < 20 {
		t.Fatal("slow-reader replay lost starts", starts)
	}
	// New clients use only status/read pages; raw lifecycle epoch is unchanged.
	before, _ := rpcClient("b-1", rpcRequest{Op: "status"})
	for i := 0; i < 4; i++ {
		rpcViewStatus("b-1")
		rpcViewRead("b-1", "events", cursor)
	}
	after, _ := rpcClient("b-1", rpcRequest{Op: "status"})
	a, _ := json.Marshal(before.Status)
	b, _ := json.Marshal(after.Status)
	if string(a) != string(b) {
		t.Fatal("view changed supervisor status")
	}
}
