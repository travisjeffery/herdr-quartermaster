package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rivo/uniseg"
)

func viewFixture(t *testing.T) string {
	t.Helper()
	t.Setenv("QUARTERMASTER_STATE_DIR", rpcTestDir(t))
	dir := rpcDir("demo-1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(dir, "session.jsonl")
	if err := rpcSave(filepath.Join(dir, "worker.json"), rpcWorker{Bead: "demo-1", Cwd: dir}); err != nil {
		t.Fatal(err)
	}
	if err := rpcSave(filepath.Join(dir, "status.json"), rpcStatus{Bead: "demo-1", Phase: "stopped", SessionFile: session, SessionID: "demo-1"}); err != nil {
		t.Fatal(err)
	}
	viewWrite(t, session, "")
	viewWrite(t, filepath.Join(dir, "events.jsonl"), "")
	return session
}
func viewWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}
func viewMessage(i int, text string) string {
	b, _ := json.Marshal(map[string]any{"type": "message", "id": fmt.Sprintf("entry-%d", i), "parentId": fmt.Sprintf("entry-%d", i-1), "timestamp": "2026-01-02T03:04:05Z", "message": map[string]any{"role": "assistant", "provider": "example", "model": "fixture-model", "content": []map[string]string{{"type": "text", "text": text}}, "usage": map[string]any{"input": 10, "output": 2, "cacheRead": 3, "cacheWrite": 4, "totalTokens": 19, "cost": map[string]float64{"total": .01}}}})
	return string(b) + "\n"
}
func TestRPCViewPaginationReconnectAndBounds(t *testing.T) {
	path := viewFixture(t)
	var data strings.Builder
	for i := 0; i < 200; i++ {
		data.WriteString(viewMessage(i, strings.Repeat("output ", 400)))
	}
	viewWrite(t, path, data.String())
	var cursor string
	seen := map[int64]bool{}
	count := 0
	for {
		p, err := rpcViewRead("demo-1", "history", cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Events) > rpcViewRecords {
			t.Fatal("unbounded page")
		}
		encoded, _ := json.Marshal(p)
		if len(encoded) > rpcViewBytes+rpcViewTextBytes+8192 {
			t.Fatalf("unbounded page bytes %d", len(encoded))
		}
		for _, e := range p.Events {
			if seen[e.Seq] {
				t.Fatal("replayed cursor")
			}
			seen[e.Seq] = true
			count++
			if e.At.IsZero() || e.EntryID == "" {
				t.Fatal("lost durable identity")
			}
		}
		cursor = p.Cursor
		if !p.More {
			break
		}
	}
	if count != 200 {
		t.Fatalf("got %d", count)
	}
	p, err := rpcViewRead("demo-1", "history", cursor)
	if err != nil || len(p.Events) != 0 {
		t.Fatal(p, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(viewMessage(201, "after reconnect"))
	f.Close()
	p, err = rpcViewRead("demo-1", "history", cursor)
	if err != nil || len(p.Events) != 1 || p.Events[0].Text != "after reconnect" {
		t.Fatal(p, err)
	}
}
func TestRPCViewPartialReplacementAndWrongCursor(t *testing.T) {
	path := viewFixture(t)
	line := viewMessage(1, "complete")
	viewWrite(t, path, line[:len(line)-1])
	p, err := rpcViewRead("demo-1", "history", "")
	if err != nil || len(p.Events) != 0 || !strings.Contains(p.Gap, "incomplete") {
		t.Fatal(p, err)
	}
	viewWrite(t, path, line)
	p, err = rpcViewRead("demo-1", "history", p.Cursor)
	if err != nil || len(p.Events) != 1 {
		t.Fatal(p, err)
	}
	cursor := p.Cursor
	// Same inode, same length, new content: anchor detects rewrite, not just size.
	viewWrite(t, path, viewMessage(1, "replaced"))
	p, err = rpcViewRead("demo-1", "history", cursor)
	if err != nil || !strings.Contains(p.Gap, "replay restarted") {
		t.Fatal(p, err)
	}
	p, err = rpcViewRead("demo-1", "events", p.Cursor)
	if err != nil || p.Gap == "" {
		t.Fatal(p, err)
	}
	for _, bad := range []string{"!", base64.RawURLEncoding.EncodeToString([]byte(`{"offset":-1}`)), strings.Repeat("x", 9000)} {
		if _, err := rpcViewRead("demo-1", "history", bad); err == nil {
			t.Fatal("invalid cursor admitted")
		}
	}
}
func TestRPCViewRedactionAndTerminalSafety(t *testing.T) {
	cases := []string{
		"password=fixture-private-value", `{"api_key":"fixture-private-value"}`,
		"Authorization: Bearer fixture-private-value", "Cookie: session=fixture-private-value",
		"Cookie: theme=light; sid=fixture-private-value",
		"Set-Cookie: sid=fixture-private-value; Secure; HttpOnly",
		"https://user:fixture-private-value@example.test/a", "https://example.test/a?token=fixture-private-value",
		"-----BEGIN PRIVATE KEY-----\nfixture-private-value\n-----END PRIVATE KEY-----",
	}
	for _, s := range cases {
		e, ok := rpcViewNormalize([]byte(viewMessage(1, s)), "history", 0)
		b, _ := json.Marshal(e)
		if !ok || bytes.Contains(b, []byte("fixture-private-value")) {
			t.Fatalf("credential leaked: %q", b)
		}
	}
	raw := viewMessage(1, "hello\x1b]52;c;clipboard\a\x1b[2J\rforged\u009b31m\u202eevil\nhttps://example.test/pull/1")
	e, ok := rpcViewNormalize([]byte(raw), "history", 0)
	if !ok {
		t.Fatal("missing message")
	}
	for _, bad := range []string{"\x1b", "\a", "\r", "\u009b", "\u202e"} {
		if strings.Contains(e.Text, bad) {
			t.Fatalf("unsafe control %q", bad)
		}
	}
	if len(e.Links) != 1 || e.Links[0] != "https://example.test/pull/1" {
		t.Fatal(e.Links)
	}
	// Credentials fragmented over streaming records must never be displayed.
	for _, s := range []string{`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Bearer "}}`, `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"fixture-private-value"}}`, `{"type":"tool_execution_update","partialResult":{"content":[{"type":"text","text":"fixture-private-value"}]}}`} {
		if _, ok := rpcViewNormalize([]byte(s), "events", 0); ok {
			t.Fatal("streaming fragment exposed")
		}
	}
	secret := strings.Repeat("q", rpcViewTextBytes+100)
	e, _ = rpcViewNormalize([]byte(viewMessage(1, "password="+secret)), "history", 0)
	if strings.Contains(e.Text, "qqqq") {
		t.Fatal("truncated before redacting")
	}
	e, _ = rpcViewNormalize([]byte(viewMessage(1, strings.Repeat("x", rpcViewTextBytes+100))), "history", 0)
	if !e.Truncated || len(e.Text) > rpcViewTextBytes+30 {
		t.Fatal("unbounded text")
	}
}
func TestRPCViewAllowlistAndUsage(t *testing.T) {
	path := viewFixture(t)
	data := viewMessage(1, "done") +
		`{"type":"message","id":"nested","message":{"role":"toolResult","toolName":"helper","content":[{"type":"text","text":"ok"}],"usage":{"totalTokens":5,"output":5,"reasoning":5}}}` + "\n" +
		`{"type":"compaction","id":"compact","summary":"saved summary","usage":{"totalTokens":7}}` + "\n" +
		`{"type":"branch_summary","id":"branch","summary":"saved branch","usage":{"totalTokens":11}}` + "\n" +
		`{"type":"usage","id":"warm","kind":"future-kind","usage":{"totalTokens":13}}` + "\n" +
		`{"type":"custom","data":{"token":"do-not-project"}}` + "\n" +
		`{"type":"message","message":{"role":"system","content":"do-not-project"}}` + "\n"
	viewWrite(t, path, data)
	p, err := rpcViewRead("demo-1", "history", "")
	if err != nil {
		t.Fatal(err)
	}
	var total rpcViewUsage
	for _, e := range p.Events {
		if e.Usage != nil {
			total.add(*e.Usage)
		}
	}
	if total.Total != 55 || total.Output != 7 {
		t.Fatalf("double/missing usage: %+v", total)
	}
	b, _ := json.Marshal(p)
	if bytes.Contains(b, []byte("do-not-project")) {
		t.Fatal("opaque content projected")
	}
	e, ok := rpcViewNormalize([]byte(`{"type":"message_end","message":{"role":"assistant","content":[],"usage":{"totalTokens":999}}}`), "events", 0)
	if !ok || e.Usage != nil {
		t.Fatal("event usage double counted")
	}
	e, ok = rpcViewNormalize([]byte(`{"type":"tool_execution_end","toolName":"test","toolCallId":"call-1","isError":true,"result":{"content":[{"type":"text","text":"failure"}],"details":{"token":"do-not-project"}}}`), "events", 9)
	if !ok || !e.Error || e.ToolID != "call-1" || e.Text != "failure" {
		t.Fatal(e)
	}
}
func TestRPCViewStoppedAdoptedAndMissingMetadata(t *testing.T) {
	path := viewFixture(t)
	external := filepath.Join(t.TempDir(), "adopted.jsonl")
	viewWrite(t, external, viewMessage(1, "adopted history"))
	var st rpcStatus
	rpcLoad(filepath.Join(rpcDir("demo-1"), "status.json"), &st)
	st.SessionFile = external
	rpcSave(filepath.Join(rpcDir("demo-1"), "status.json"), st)
	before, _ := os.ReadFile(filepath.Join(rpcDir("demo-1"), "status.json"))
	p, err := rpcViewRead("demo-1", "history", "")
	if err != nil || len(p.Events) != 1 || p.Events[0].Text != "adopted history" {
		t.Fatal(p, err)
	}
	v, _ := rpcViewStatus("demo-1")
	if v.State != "stopped" || v.Connected {
		t.Fatal(v)
	}
	after, _ := os.ReadFile(filepath.Join(rpcDir("demo-1"), "status.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("view changed status")
	}
	st.Phase = "working"
	rpcSave(filepath.Join(rpcDir("demo-1"), "status.json"), st)
	v, _ = rpcViewStatus("demo-1")
	if v.State != "disconnected" {
		t.Fatal("cached liveness treated as progress", v)
	}
	rpcSave(filepath.Join(rpcDir("demo-1"), "worker.json"), rpcWorker{Bead: "demo-1", Adoption: &rpcAdoption{SessionFile: external}})
	viewWrite(t, filepath.Join(rpcDir("demo-1"), "status.json"), "broken")
	if _, err := rpcViewRead("demo-1", "history", ""); err == nil {
		t.Fatal("repaired corrupt metadata")
	}
	os.Remove(filepath.Join(rpcDir("demo-1"), "status.json"))
	p, err = rpcViewRead("demo-1", "history", "")
	if err != nil || len(p.Events) != 1 {
		t.Fatal("prepared adoption unreadable", p, err)
	}
	os.Remove(path)
	if _, err := rpcViewRead("../elsewhere", "history", ""); err == nil {
		t.Fatal("unsafe worker id")
	}
}
func TestRPCViewOnlyStatusOperationAndTruthfulStates(t *testing.T) {
	viewFixture(t)
	l, err := net.Listen("unix", rpcSocket("demo-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	phases := []string{"idle", "parked", "working", "blocked", "mystery"}
	done := make(chan error, 1)
	go func() {
		for _, phase := range phases {
			c, err := l.Accept()
			if err != nil {
				done <- err
				return
			}
			var req rpcRequest
			err = json.NewDecoder(c).Decode(&req)
			if err != nil || req.Op != "status" {
				c.Close()
				done <- fmt.Errorf("viewer sent mutation: %+v %v", req, err)
				return
			}
			json.NewEncoder(c).Encode(rpcReply{OK: true, Status: &rpcStatus{Phase: phase}})
			c.Close()
		}
		done <- nil
	}()
	wants := []string{"idle (not parked)", "parked", "running", "waiting on approval", "unknown"}
	for i := range phases {
		v, err := rpcViewStatus("demo-1")
		if err != nil || v.State != wants[i] || !v.Connected {
			t.Fatal(v, err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestRPCViewerBoundedReplayAndKeys(t *testing.T) {
	var reads int
	env := rpcViewEnv{
		page: func(id, source, cursor string) (rpcViewPage, error) {
			reads++
			return rpcViewPage{Cursor: "next", Events: []rpcViewEvent{{Label: "assistant", Text: "example"}}}, nil
		},
		status: func(string) (rpcVisibility, error) { return rpcVisibility{State: "parked"}, nil },
	}
	v := newRPCViewer("demo-1", "Example task", nil, env)
	c := v.load()
	if v.load() != nil {
		t.Fatal("unbounded in-flight reads")
	}
	v.apply(c().(rpcViewResult), time.Now())
	if reads != 2 {
		t.Fatal(reads)
	}
	for _, k := range []string{"A", "R", "S", "U", "c", "x", "m", "i", "a", "9", "o", "l", "p"} {
		if v.key(k) != nil {
			t.Fatalf("control %q escaped viewer", k)
		}
	}
	v.key("\r")
	if !v.history.expanded {
		t.Fatal("Enter did not expand")
	}
	for i := 0; i < 300; i++ {
		v.history.append(rpcViewPage{Cursor: "next", Events: []rpcViewEvent{{Text: strings.Repeat("a", rpcViewTextBytes)}}}, true)
	}
	if len(v.history.events) > rpcViewKeep || !v.history.dropped {
		t.Fatal("slow view unbounded")
	}
	v.key("e")
	if v.source != "events" {
		t.Fatal("events tab")
	}
	old := v.generation
	c = v.key("g")
	v.apply(rpcViewResult{generation: old, visibility: rpcVisibility{State: "wrong"}}, time.Now())
	if v.visibility.State == "wrong" {
		t.Fatal("stale result applied")
	}
	v.apply(c().(rpcViewResult), time.Now())
	if v.follow {
		t.Fatal("replay auto-skips pages")
	}
	v.key("q")
	if !v.quit {
		t.Fatal("view did not close")
	}
}
func TestRPCViewerUIControlsAndNarrowRender(t *testing.T) {
	v := newRPCViewer("demo-1", "title\x1b]52;bad\a", []string{"https://example.test/pull/1"}, rpcViewEnv{})
	v.visibility = rpcVisibility{State: "parked", Observed: time.Now()}
	v.history.events = []rpcViewEvent{{Label: "tool result", Text: rpcViewRedact("a\x1b[2J\npassword=fixture-private-value\nline")}}
	v.history.expanded = true
	for _, w := range []int{1, 9, 20, 80, 150} {
		v.title = "Example 界界界 👩‍💻 é"
		out := v.render(w, 16, time.Now())
		plain := strings.NewReplacer("\x1b[H", "", "\x1b[K", "").Replace(out)
		if strings.ContainsAny(plain, "\x1b\a") || strings.Contains(plain, "fixture-private-value") {
			t.Fatal("untrusted terminal controls leaked")
		}
		if len(strings.Split(plain, "\r\n")) != 16 {
			t.Fatal("wrong view height")
		}
		for _, line := range strings.Split(plain, "\r\n") {
			if uniseg.StringWidth(line) > w {
				t.Fatalf("overflow: %q width %d", line, w)
			}
		}
	}
}
func TestRPCBoardOpensStoppedHistoryNeverFocusOrStart(t *testing.T) {
	f := &fakeBoard{beads: []Bead{boardBead("demo-1", StatusInProgress, 2)}}
	m := testModel(t, f)
	m.env.isNative = func(id string) bool { return id == "demo-1" }
	opens := 0
	m.env.viewNative = func(id, title string, links []string) error { opens++; return nil }
	for _, key := range []string{"\r", "V"} {
		press(t, m, key)
		if m.handoff == nil {
			t.Fatal("stopped history was not opened")
		}
		m.handoff()
		m.handoff = nil
	}
	if opens != 2 || len(f.calls) != 0 || m.ask != nil {
		t.Fatal("opening view mutated worker/task", f.calls)
	}
	// Data joins do not make stopped workers live ticker agents.
	m.apply(nativeViewMsg{map[string]rpcVisibility{"demo-1": {State: "stopped", Activity: "tool finished", ActivityAt: t0}}})
	if m.sel().agent != nil || m.sel().native == nil {
		t.Fatal("visibility changed liveness")
	}
	if out := renderBoard(m, 140, 20); !strings.Contains(out, "stopped") {
		t.Fatal("board lacks native execution state")
	}
}
func TestRPCViewSupervisorMetadataBoundaries(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", rpcTestDir(t))
	os.MkdirAll(rpcDir("demo-1"), 0700)
	log, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	s := &rpcSupervisor{worker: rpcWorker{Bead: "demo-1"}, status: rpcStatus{Phase: "idle"}, log: log}
	for _, raw := range []string{`{"type":"agent_start"}`, `{"type":"tool_execution_start","toolName":"read","toolCallId":"a","args":{"path":"example.go"}}`} {
		var r rpcRecord
		json.Unmarshal([]byte(raw), &r)
		if err := s.event([]byte(raw), r); err != nil {
			t.Fatal(err)
		}
	}
	var st rpcStatus
	rpcLoad(filepath.Join(rpcDir("demo-1"), "status.json"), &st)
	if st.Current != "tools: read" || st.RunStarted.IsZero() || st.ActivityAt.IsZero() {
		t.Fatal("short-boundary tool start not persisted", st)
	}
	raw := []byte(`{"type":"agent_settled"}`)
	s.event(raw, rpcRecord{Type: "agent_settled"})
	st = rpcStatus{}
	rpcLoad(filepath.Join(rpcDir("demo-1"), "status.json"), &st)
	if st.Phase != "idle" || st.Current != "" || st.RunEnded.IsZero() {
		t.Fatal(st)
	}
	b, _ := os.ReadFile(log.Name())
	if !bytes.Contains(b, []byte(`"_quartermaster":{"at":`)) {
		t.Fatal("event timestamps absent")
	}
}
