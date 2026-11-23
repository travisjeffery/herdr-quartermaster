package main

import (
	"encoding/base64"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const questionNotes = `looked at the rollout
QUESTION: Ship to all regions or one first? Recommendation: 2 — one region first, because the alerts are unproven.
OPTIONS:
1. All regions at once
2. One region first, then the rest
CLASS: 7 — production rollout`

func TestParseNeedsMeQuestion(t *testing.T) {
	q, ok := parseNeedsMeQuestion(questionNotes)
	if !ok {
		t.Fatal("no question parsed")
	}
	if len(q.Options) != 2 || q.Options[1].Label != "One region first, then the rest" || q.ClassN != 7 || q.Class != "7 — production rollout" {
		t.Fatalf("question = %+v", q)
	}
	if q.Summary() != "Ship to all regions or one first? Recommendation: 2 — one region first, because the alerts are…" && !strings.HasPrefix(q.Summary(), "Ship to all regions") {
		t.Errorf("summary = %q", q.Summary())
	}
	if len(q.Hash) != 8 {
		t.Errorf("hash = %q", q.Hash)
	}
	// The same notes hash the same; an edited option is another question.
	again, _ := parseNeedsMeQuestion(questionNotes)
	edited, _ := parseNeedsMeQuestion(strings.Replace(questionNotes, "All regions at once", "Every region now", 1))
	if again.Hash != q.Hash || edited.Hash == q.Hash {
		t.Errorf("hashes: same %s/%s, edited %s", q.Hash, again.Hash, edited.Hash)
	}
	o, err := q.choose("2")
	if err != nil || o.Number != 2 {
		t.Errorf("choose 2 = %+v, %v", o, err)
	}
	if _, err := q.choose("3"); err == nil {
		t.Error("choose 3 of 2 options worked")
	}
}

func TestParseNeedsMeQuestionVariants(t *testing.T) {
	for name, notes := range map[string]string{
		"class in the question lines": "QUESTION: Which key?\nCLASS: 8 — read a secret\nOPTIONS:\n1. The old key\n2. A new key",
		"critical":                    "CRITICAL: deleting the old bucket\nQUESTION: Delete it now?\nOPTIONS:\n1. Delete now\n2. Keep it a week\nCLASS: 8 — one prod delete",
		"no class":                    "QUESTION: Tabs or spaces?\nOPTIONS:\n1. Tabs\n2. Spaces",
		"older block superseded":      "QUESTION: First?\nOPTIONS:\n1. A\n2. B\nNEEDS_ME ANSWERED: 1 A [abcd1234] by TJ via tap\nQUESTION: Second?\nOPTIONS:\n1. C\n2. D",
		"blank lines in the block":    "QUESTION: Tabs or spaces?\n\nOPTIONS:\n\n1. Tabs\n\n2. Spaces\n\nCLASS: 2 — style only",
	} {
		if q, ok := parseNeedsMeQuestion(notes); !ok || len(q.Options) != 2 {
			t.Errorf("%s: parsed %+v, ok %v", name, q, ok)
		}
	}
	q, _ := parseNeedsMeQuestion("CRITICAL: deleting the old bucket\nQUESTION: Delete it now?\nOPTIONS:\n1. Delete now\n2. Keep it a week\nCLASS: 8 — one prod delete")
	if q.Lines[0] != "CRITICAL: deleting the old bucket" || q.ClassN != 8 {
		t.Errorf("critical question = %+v", q)
	}
	q, _ = parseNeedsMeQuestion("QUESTION: Which key?\nCLASS: 8 — read a secret\nOPTIONS:\n1. The old key\n2. A new key")
	if q.ClassN != 8 || len(q.Lines) != 1 {
		t.Errorf("class-in-question = %+v", q)
	}
	q, _ = parseNeedsMeQuestion("QUESTION: First?\nOPTIONS:\n1. A\n2. B\nNEEDS_ME ANSWERED: 1 A [abcd1234] by TJ via tap\nQUESTION: Second?\nOPTIONS:\n1. C\n2. D")
	if q.Summary() != "Second?" {
		t.Errorf("superseded: summary = %q", q.Summary())
	}
}

func TestParseNeedsMeQuestionRejectsFreeText(t *testing.T) {
	for name, notes := range map[string]string{
		"free text":            "Should we ship to all regions or one first? I recommend one first.\nCLASS: 7 — production rollout",
		"options, no question": "Ship?\nOPTIONS:\n1. All\n2. One",
		"one option":           "QUESTION: Ship?\nOPTIONS:\n1. All",
		"not in sequence":      "QUESTION: Ship?\nOPTIONS:\n1. All\n3. One",
		"starts at two":        "QUESTION: Ship?\nOPTIONS:\n2. All\n3. One",
		"a later note":         questionNotes + "\nmeanwhile the canary finished",
		"already answered":     questionNotes + "\nNEEDS_ME ANSWERED: 2 One region first [abcd1234] by TJ via tap (device iphone181)",
		"empty":                "",
	} {
		if q, ok := parseNeedsMeQuestion(notes); ok {
			t.Errorf("%s: parsed as %+v", name, q)
		}
	}
}

func TestLiveQuestion(t *testing.T) {
	b := bead(StatusNeedsMe)
	b.Notes = questionNotes
	if _, ok := liveQuestion(thread(b, agent("idle", 1), nil)); !ok {
		t.Fatal("a settled needs_me OPTIONS bead has no live question")
	}
	if _, ok := liveQuestion(thread(b, agent("working", 1), nil)); ok {
		t.Error("a worker working again still has a live question")
	}
	run := bead(StatusNeedsMe)
	run.Notes = "RUN: aws sso login\nwhy: expired"
	if _, ok := liveQuestion(thread(run, agent("idle", 1), nil)); ok {
		t.Error("a RUN bead has a live question")
	}
	open := bead(StatusInProgress)
	open.Notes = questionNotes
	if _, ok := liveQuestion(thread(open, agent("idle", 1), nil)); ok {
		t.Error("an in_progress bead has a live question")
	}
}

// fakeQuestion is a needs_me bead, its worker and what a tap did to both.
type fakeQuestion struct {
	bead    Bead
	agent   *Agent // nil: no worker running
	notes   []string
	told    []string
	tellErr error
}

func newFakeQuestion(agentStatus string) *fakeQuestion {
	f := &fakeQuestion{bead: Bead{ID: "backend-ab12", Status: StatusNeedsMe, Notes: questionNotes}}
	if agentStatus != "" {
		f.agent = &Agent{Name: "backend-ab12", Status: agentStatus, PaneID: "w1:p1"}
	}
	return f
}

func (f *fakeQuestion) env() promptEnv {
	return promptEnv{
		show:  func(string) (Bead, error) { return f.bead, nil },
		agent: func(string, string) (*Agent, error) { return f.agent, nil },
		read:  func(string, string) (string, error) { return "nothing on screen", nil },
		keys:  func(string, string, ...string) error { return nil },
		note: func(_ string, text string) error {
			f.notes = append(f.notes, text)
			f.bead.Notes += "\n" + text
			return nil
		},
		setStatus: func(_ string, status string) error { f.bead.Status = status; return nil },
		tell:      func(_ string, _ *Agent, text string) error { f.told = append(f.told, text); return f.tellErr },
		sleep:     func(time.Duration) {},
		level:     func(string) (int, string) { return 0, "" },
	}
}

func testQuestionTapServer(t *testing.T, f *fakeQuestion, device string) *tapServer {
	t.Helper()
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	s, err := newTapServer(TapConfig{Enabled: true, Devices: []string{"iphone181"}}, f.env())
	if err != nil {
		t.Fatal(err)
	}
	s.whois = func(string) (tapIdentity, error) { return tapIdentity{Device: device, User: "tj@"}, nil }
	return s
}

func questionTokenFor(t *testing.T, s *tapServer, q NeedsMeQuestion, choice string) string {
	t.Helper()
	tok, _, err := mintTapTokenKind(s.secret, tapKindQuestion, "backend-ab12", q.Hash, choice, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestTapQuestionTokenKinds(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	now := time.Now()
	tok, minted, err := mintTapTokenKind(secret, tapKindQuestion, "backend-ab12", "abcd1234", "2", now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseTapToken(secret, tok, now)
	if err != nil || got.Kind != tapKindQuestion || got.Hash != "abcd1234" || got.Nonce != minted.Nonce {
		t.Fatalf("claims = %+v, err %v", got, err)
	}
	// A pre-question link (no kind field) still parses, as a prompt tap.
	old := strings.Join([]string{"backend-ab12", "abcd1234", "1", strconv.FormatInt(now.Add(time.Minute).Unix(), 10), "cafe"}, "|")
	oldTok := base64.RawURLEncoding.EncodeToString([]byte(old)) + "." + tapSign(secret, old)
	got, err = parseTapToken(secret, oldTok, now)
	if err != nil || got.Kind != tapKindPrompt || got.Bead != "backend-ab12" {
		t.Fatalf("legacy claims = %+v, err %v", got, err)
	}
	if _, _, err := mintTapTokenKind(secret, "x", "backend-ab12", "abcd1234", "1", now); err == nil {
		t.Error("an unknown tap kind minted")
	}
}

func TestTapAnswersQuestionEndToEnd(t *testing.T) {
	q, _ := parseNeedsMeQuestion(questionNotes)
	f := newFakeQuestion("idle")
	s := testQuestionTapServer(t, f, "iphone181")
	tok := questionTokenFor(t, s, q, "2")

	code, body := tapRequest(s, http.MethodGet, tok, tapPhoneAddr)
	if code != http.StatusOK || !strings.Contains(body, "Ship to all regions or one first?") || !strings.Contains(body, "Answer 2. One region first, then the rest") || !strings.Contains(body, "CLASS: 7") {
		t.Fatalf("confirm page = %d %q", code, body)
	}
	if len(f.notes) != 0 || len(f.told) != 0 || f.bead.Status != StatusNeedsMe {
		t.Fatalf("a GET changed something: notes %q, told %q, status %s", f.notes, f.told, f.bead.Status)
	}

	code, body = tapRequest(s, http.MethodPost, tok, tapPhoneAddr)
	if code != http.StatusOK || !strings.Contains(body, "Answered") || !strings.Contains(body, "worker has been prompted") {
		t.Fatalf("answer = %d %q", code, body)
	}
	if f.bead.Status != StatusInProgress {
		t.Fatalf("status = %s", f.bead.Status)
	}
	want := "NEEDS_ME ANSWERED: 2 One region first, then the rest to " + shortTitle(q.Summary(), 80) + " [" + q.Hash + "] by TJ via tap (device iphone181)"
	if !slices.Equal(f.notes, []string{want}) {
		t.Fatalf("notes = %q", f.notes)
	}
	if len(f.told) != 1 || !strings.Contains(f.told[0], "TJ answered your needs_me question") || !strings.Contains(f.told[0], "2. One region first, then the rest") {
		t.Fatalf("told = %q", f.told)
	}
	if strings.HasPrefix(f.told[0], "[quartermaster") {
		t.Errorf("the worker prompt reads as an automated notice, which approves nothing: %q", f.told[0])
	}
	recs, err := readJournal(time.Time{})
	if err != nil || len(recs) != 1 || recs[0].Kind != journalNeedsMe || recs[0].Text != want {
		t.Fatalf("journal = %+v, err %v", recs, err)
	}
	// The answered block is history now: it no longer parses as live.
	if _, ok := parseNeedsMeQuestion(f.bead.Notes); ok {
		t.Error("the answered question still parses")
	}
}

func TestTapQuestionStaleRefused(t *testing.T) {
	q, _ := parseNeedsMeQuestion(questionNotes)
	cases := []struct {
		name   string
		agent  string
		mutate func(f *fakeQuestion)
	}{
		{"question changed", "idle", func(f *fakeQuestion) {
			f.bead.Notes = strings.Replace(questionNotes, "All regions at once", "Every region now", 1)
		}},
		{"bead answered", "idle", func(f *fakeQuestion) { f.bead.Status = StatusInProgress }},
		{"question replaced by notes", "idle", func(f *fakeQuestion) { f.bead.Notes += "\nnever mind, decided in the pane" }},
		{"worker working again", "working", func(f *fakeQuestion) {}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeQuestion(c.agent)
			c.mutate(f)
			s := testQuestionTapServer(t, f, "iphone181")
			tok := questionTokenFor(t, s, q, "1")
			// A wrong-device attempt is refused first and spends nothing.
			s.whois = func(string) (tapIdentity, error) { return tapIdentity{Device: "desktop"}, nil }
			if code, _ := tapRequest(s, http.MethodPost, tok, tapPhoneAddr); code != http.StatusForbidden {
				t.Fatalf("wrong device = %d", code)
			}
			s.whois = func(string) (tapIdentity, error) { return tapIdentity{Device: "iphone181"}, nil }
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				code, body := tapRequest(s, method, tok, tapPhoneAddr)
				if code != http.StatusConflict || !strings.Contains(body, "Not answered") {
					t.Fatalf("%s = %d %q", method, code, body)
				}
			}
			if len(f.notes) != 0 || len(f.told) != 0 || f.bead.Status == StatusInProgress && c.name != "bead answered" {
				t.Fatalf("a stale tap changed something: notes %q, told %q, status %s", f.notes, f.told, f.bead.Status)
			}
			recs, err := readJournal(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, r := range recs {
				if r.Kind == journalTap && strings.HasPrefix(r.Text, "TAP REFUSED:") && r.Bead == "backend-ab12" {
					found = true
				}
			}
			if !found {
				t.Fatalf("no refusal journalled: %+v", recs)
			}
		})
	}
}

func TestTapQuestionReplayRefused(t *testing.T) {
	q, _ := parseNeedsMeQuestion(questionNotes)
	f := newFakeQuestion("idle")
	s := testQuestionTapServer(t, f, "iphone181")
	tok := questionTokenFor(t, s, q, "1")
	if code, _ := tapRequest(s, http.MethodPost, tok, tapPhoneAddr); code != http.StatusOK {
		t.Fatalf("first tap = %d", code)
	}
	code, body := tapRequest(s, http.MethodPost, tok, tapPhoneAddr)
	if code != http.StatusConflict || !strings.Contains(body, "already used") {
		t.Fatalf("replay = %d %q", code, body)
	}
	if len(f.notes) != 1 || len(f.told) != 1 {
		t.Fatalf("the replay acted again: notes %q, told %q", f.notes, f.told)
	}
}

func TestTapQuestionWorkerNotPrompted(t *testing.T) {
	q, _ := parseNeedsMeQuestion(questionNotes)
	for name, agent := range map[string]string{"no worker": "", "blocked at a prompt": "blocked"} {
		t.Run(name, func(t *testing.T) {
			f := newFakeQuestion(agent)
			s := testQuestionTapServer(t, f, "iphone181")
			tok := questionTokenFor(t, s, q, "2")
			code, body := tapRequest(s, http.MethodPost, tok, tapPhoneAddr)
			if code != http.StatusOK || !strings.Contains(body, "was not prompted") {
				t.Fatalf("answer = %d %q", code, body)
			}
			if f.bead.Status != StatusInProgress || len(f.told) != 0 {
				t.Fatalf("status %s, told %q", f.bead.Status, f.told)
			}
			if len(f.notes) != 2 || !strings.Contains(f.notes[1], "was not prompted") {
				t.Fatalf("notes = %q", f.notes)
			}
		})
	}
}

func TestTapQuestionLinksAndEventSuffix(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	testTapOwnIP(t)
	q, _ := parseNeedsMeQuestion(questionNotes)
	cfg := Config{Tap: TapConfig{Enabled: true, Devices: []string{"iphone181"}}}
	f := newFakeQuestion("idle")

	out, err := tapLinksText(cfg, f.env(), "backend-ab12", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out, "http://100.78.70.17:8642/tap/"); got != len(q.Options) {
		t.Fatalf("%d links for %d options:\n%s", got, len(q.Options), out)
	}
	for _, want := range []string{"is marked needs_me [" + q.Hash + "]", "Ship to all regions", "1 All regions at once — ", "2 One region first, then the rest — ", "CLASS: 7"} {
		if !strings.Contains(out, want) {
			t.Errorf("links lack %q:\n%s", want, out)
		}
	}
	secret, err := tapSecret()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, "/tap/"); i >= 0 {
			c, err := parseTapToken(secret, strings.TrimSpace(line[i+5:]), time.Now())
			if err != nil || c.Kind != tapKindQuestion || c.Hash != q.Hash {
				t.Errorf("link in %q: claims %+v, err %v", line, c, err)
			}
		}
	}

	// A free-text needs_me question: no links, the old prompt error.
	free := newFakeQuestion("idle")
	free.bead.Notes = "Should we ship everywhere at once? I recommend not."
	if _, err := tapLinksText(cfg, free.env(), "backend-ab12", time.Now()); err == nil || !strings.Contains(err.Error(), "no prompt") {
		t.Errorf("free-text links: err = %v", err)
	}

	b := bead(StatusNeedsMe)
	b.Notes = questionNotes
	th := thread(b, agent("idle", 1), nil)
	tk := Ticker{cfg: cfg, log: log.New(io.Discard, "", 0)}
	suffix := tk.tapQuestionEventSuffix(th)
	if strings.Count(suffix, "/tap/") != len(q.Options) || !strings.Contains(suffix, "question: Ship to all regions") {
		t.Errorf("event suffix = %q", suffix)
	}
	freeTh := thread(bead(StatusNeedsMe), agent("idle", 1), nil)
	freeTh.Bead.Notes = "Should we ship everywhere at once?"
	if got := tk.tapQuestionEventSuffix(freeTh); got != "" {
		t.Errorf("free-text suffix = %q", got)
	}
	runTh := thread(bead(StatusNeedsMe), agent("idle", 1), nil)
	runTh.Bead.Notes = "RUN: aws sso login"
	if got := tk.tapQuestionEventSuffix(runTh); got != "" {
		t.Errorf("RUN suffix = %q", got)
	}
	tk.cfg.Tap.Enabled = false
	if got := tk.tapQuestionEventSuffix(th); got != "" {
		t.Errorf("suffix while off = %q", got)
	}
}

func TestContextShowsQuestionOptions(t *testing.T) {
	b := bead(StatusNeedsMe)
	b.Notes = questionNotes
	out := renderContext(Config{Repo: "/r"}, []Thread{thread(b, agent("idle", 1), nil)}, nil, nil, nil, TickerState{}, nil, t0)
	if !strings.Contains(out, "question: Ship to all regions or one first?") || !strings.Contains(out, "options: 1 All regions at once · 2 One region first, then the rest") {
		t.Fatalf("context lacks the question and options:\n%s", out)
	}
	// Free text renders as before: the latest note alone.
	b.Notes = "earlier note\nShould we ship everywhere at once?"
	out = renderContext(Config{Repo: "/r"}, []Thread{thread(b, agent("idle", 1), nil)}, nil, nil, nil, TickerState{}, nil, t0)
	if !strings.Contains(out, "question: Should we ship everywhere at once?") || strings.Contains(out, "options:") {
		t.Fatalf("free-text context changed:\n%s", out)
	}
}

func TestBriefAsksForOptionsConvention(t *testing.T) {
	s := brief(bead(StatusInProgress), "tj/x", "/w/x", "", "")
	for _, want := range []string{"QUESTION: <question> Recommendation: <n> — <why>", "OPTIONS:", "NEEDS_ME ANSWERED:", "Free text still works"} {
		if !strings.Contains(s, want) {
			t.Fatalf("brief lacks %q:\n%s", want, s)
		}
	}
}
