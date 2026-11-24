package main

import (
	"encoding/base64"
	"io"
	"log"
	"net/http"
	"net/url"
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
	if got := strings.Count(out, "http://100.78.70.17:8642/tap/"); got != len(q.Options)+1 {
		t.Fatalf("%d links for %d options + free text:\n%s", got, len(q.Options), out)
	}
	for _, want := range []string{"is marked needs_me [" + q.Hash + "]", "Ship to all regions", "1 All regions at once — ", "2 One region first, then the rest — ", "free text (up to 2000 chars) — ", "CLASS: 7"} {
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

	// A free-text needs_me question (no OPTIONS) mints a single free-text tap link.
	free := newFakeQuestion("idle")
	free.bead.Notes = "Should we ship everywhere at once? I recommend not."
	fq, ok := parseTappableNeedsMeQuestion(free.bead.Notes)
	if !ok {
		t.Fatal("free-text question did not parse")
	}
	freeOut, err := tapLinksText(cfg, free.env(), "backend-ab12", time.Now())
	if err != nil {
		t.Fatalf("free-text links failed: %v", err)
	}
	if got := strings.Count(freeOut, "http://100.78.70.17:8642/tap/"); got != 1 {
		t.Fatalf("want 1 link for free-text question, got %d:\n%s", got, freeOut)
	}
	if !strings.Contains(freeOut, "free text (up to 2000 chars) — ") || strings.Contains(freeOut, "OPTIONS:") {
		t.Errorf("free-text links output unexpected:\n%s", freeOut)
	}
	for _, line := range strings.Split(freeOut, "\n") {
		if i := strings.Index(line, "/tap/"); i >= 0 {
			c, err := parseTapToken(secret, strings.TrimSpace(line[i+5:]), time.Now())
			if err != nil || c.Kind != tapKindQuestion || c.Choice != tapChoiceFreeText || c.Hash != fq.Hash {
				t.Errorf("free-text link claims = %+v, err %v", c, err)
			}
		}
	}

	// A RUN: bead gets no tap links and returns the prompt error.
	runFake := newFakeQuestion("idle")
	runFake.bead.Notes = "RUN: aws sso login\nwhy: expired"
	if _, err := tapLinksText(cfg, runFake.env(), "backend-ab12", time.Now()); err == nil || !strings.Contains(err.Error(), "no prompt") {
		t.Errorf("RUN bead links: err = %v", err)
	}

	b := bead(StatusNeedsMe)
	b.Notes = questionNotes
	th := thread(b, agent("idle", 1), nil)
	tk := Ticker{cfg: cfg, log: log.New(io.Discard, "", 0)}
	suffix := tk.tapQuestionEventSuffix(th)
	if strings.Count(suffix, "/tap/") != len(q.Options)+1 || !strings.Contains(suffix, "question: Ship to all regions") || !strings.Contains(suffix, "free text http://") {
		t.Errorf("event suffix = %q", suffix)
	}
	freeTh := thread(bead(StatusNeedsMe), agent("idle", 1), nil)
	freeTh.Bead.Notes = "Should we ship everywhere at once?"
	freeSuffix := tk.tapQuestionEventSuffix(freeTh)
	if strings.Count(freeSuffix, "/tap/") != 1 || !strings.Contains(freeSuffix, "question: Should we ship everywhere at once?") || !strings.Contains(freeSuffix, "free text http://") {
		t.Errorf("free-text suffix = %q", freeSuffix)
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

func TestParseFreeTextNeedsMeQuestion(t *testing.T) {
	cases := []struct {
		name      string
		notes     string
		wantLines []string
		wantClass int
	}{
		{
			name:      "plain prose with class",
			notes:     "looked at the rollout\nShould we ship to all regions or one first? I recommend one first.\nCLASS: 7 — production rollout",
			wantLines: []string{"Should we ship to all regions or one first? I recommend one first."},
			wantClass: 7,
		},
		{
			name:      "QUESTION without OPTIONS and CRITICAL",
			notes:     "earlier progress\nCRITICAL: deleting legacy data\nQUESTION: Which customer-hosted accounts may endpoint scanning cover?\nMore context about the agreement.\nCLASS: 8 — prod scope",
			wantLines: []string{"CRITICAL: deleting legacy data", "Which customer-hosted accounts may endpoint scanning cover?", "More context about the agreement."},
			wantClass: 8,
		},
		{
			name:      "new free-text question after prior answer",
			notes:     "QUESTION: Old?\nOPTIONS:\n1. A\n2. B\nNEEDS_ME ANSWERED: 1 A to Old? [11223344] by TJ via tap (device iphone181)\nCan you confirm the rollout window?",
			wantLines: []string{"Can you confirm the rollout window?"},
			wantClass: -1,
		},
		{
			name:      "plain prose preceded by CRITICAL",
			notes:     "PR: https://github.com/example/repo/pull/9\nCRITICAL: rotating prod key\nConfirm whether to rotate primary key now?\nCLASS: 8 — secret write",
			wantLines: []string{"CRITICAL: rotating prod key", "Confirm whether to rotate primary key now?"},
			wantClass: 8,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, ok := parseTappableNeedsMeQuestion(tc.notes)
			if !ok {
				t.Fatalf("did not parse: %q", tc.notes)
			}
			if len(q.Options) != 0 {
				t.Fatalf("unexpected options: %+v", q.Options)
			}
			if !slices.Equal(q.Lines, tc.wantLines) || q.ClassN != tc.wantClass {
				t.Fatalf("got lines=%q class=%d, want lines=%q class=%d", q.Lines, q.ClassN, tc.wantLines, tc.wantClass)
			}
			if len(q.Hash) != 8 {
				t.Fatalf("hash = %q", q.Hash)
			}
			if strings.Contains(q.String(), "OPTIONS:") {
				t.Fatalf("free-text String() emitted OPTIONS:\n%s", q.String())
			}
		})
	}

	// Verify hash matches promptHash with empty options and detects edits.
	q1, _ := parseTappableNeedsMeQuestion("Should we rename the flag?\nCLASS: 2 — cli")
	q2, _ := parseTappableNeedsMeQuestion("Should we rename the flag and env var?\nCLASS: 2 — cli")
	wantHash := promptHash(ApprovalPrompt{Lines: []string{"Should we rename the flag?", "CLASS: 2 — cli"}})
	if q1.Hash != wantHash {
		t.Errorf("hash = %q, want %q", q1.Hash, wantHash)
	}
	if q1.Hash == q2.Hash {
		t.Errorf("edited question kept same hash %q", q1.Hash)
	}

	// Rejects RUN:, empty, bookkeeping-only, and already-answered notes.
	for name, notes := range map[string]string{
		"empty":             "",
		"whitespace":        "   \n\t\n",
		"RUN command":       "RUN: aws sso login\nwhy: session expired",
		"RUN with class":    "CLASS: 6 — auth\nRUN: aws sso login\nwhy: expired",
		"already answered":  "Should we ship?\nNEEDS_ME ANSWERED: free text to Should we ship? [abcd1234] by TJ via tap (device iphone181): yes",
		"not prompted tail": "Should we ship?\nNEEDS_ME ANSWERED: free text to Should we ship? [abcd1234] by TJ via tap (device iphone181): yes\nthe tap answer above was recorded but the worker was not prompted (no worker is running); prompt it with the answer",
		"auto resumed":      "Should we ship?\nauto: agent resumed after needs_me",
		"bookkeeping only":  "PR: https://github.com/example/repo/pull/1\nAUTO: [worker L4 C2] pushed",
		"class only":        "CLASS: 7 — prod",
	} {
		if q, ok := parseTappableNeedsMeQuestion(notes); ok {
			t.Errorf("%s: unexpectedly parsed as %+v", name, q)
		}
	}
}

func TestTapFreeTextOnlyNoOptionsEndToEnd(t *testing.T) {
	freeNotes := "CRITICAL: scope change\nQUESTION: Which customer-hosted accounts may endpoint scanning cover?\nCLASS: 7 — production scope"
	q, ok := parseTappableNeedsMeQuestion(freeNotes)
	if !ok || len(q.Options) != 0 {
		t.Fatalf("parse failed: %+v ok=%v", q, ok)
	}
	f := newFakeQuestion("idle")
	f.bead.ID = "task-free"
	f.bead.Notes = freeNotes
	f.agent.Name = "task-free"
	s := testQuestionTapServer(t, f, "iphone181")
	tok, _, err := mintTapTokenKind(s.secret, tapKindQuestion, "task-free", q.Hash, tapChoiceFreeText, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	// GET confirm page: text-only (one textarea, one submit button, limit shown, no OPTIONS, mutates nothing).
	code, body := tapRequest(s, http.MethodGet, tok, tapPhoneAddr)
	if code != http.StatusOK {
		t.Fatalf("GET confirm = %d %q", code, body)
	}
	for _, want := range []string{
		"task-free",
		"Which customer-hosted accounts may endpoint scanning cover?",
		"CLASS: 7 — production scope",
		`<textarea id="tap-text" name="text"`,
		`maxlength="2000"`,
		`required`,
		"Send free-text answer",
		"2000",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("confirm page missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "OPTIONS:") || strings.Count(body, "<button ") != 1 {
		t.Errorf("no-OPTIONS confirm page should have no OPTIONS and exactly 1 button:\n%s", body)
	}
	if len(f.notes) != 0 || len(f.told) != 0 || f.bead.Status != StatusNeedsMe {
		t.Fatalf("GET mutated state: notes=%q told=%q status=%s", f.notes, f.told, f.bead.Status)
	}

	// Empty and whitespace-only submissions are refused BEFORE spending the nonce.
	for _, bad := range []string{"", "   \n\t  "} {
		code, body := tapFormRequest(s, http.MethodPost, tok, url.Values{"text": {bad}}, tapPhoneAddr)
		if code != http.StatusBadRequest || !strings.Contains(body, "cannot be empty") {
			t.Fatalf("empty text %q = %d %q", bad, code, body)
		}
	}

	// Submit verbatim text with leading/trailing spaces, punctuation, unicode, internal newline,
	// CRLF, and one trailing newline added by form submission.
	rawSubmit := "  Include accounts A & B — signed 2026-10.\r\nLeave the rest off.  \r\n"
	wantVerbatim := "  Include accounts A & B — signed 2026-10.\nLeave the rest off.  "
	code, body = tapFormRequest(s, http.MethodPost, tok, url.Values{"text": {rawSubmit}}, tapPhoneAddr)
	if code != http.StatusOK || !strings.Contains(body, "Answered") || !strings.Contains(body, "worker has been prompted") {
		t.Fatalf("POST answer = %d %q", code, body)
	}
	if f.bead.Status != StatusInProgress {
		t.Fatalf("status = %s, want in_progress", f.bead.Status)
	}
	wantNote := "NEEDS_ME ANSWERED: free text to " + shortTitle(q.Summary(), 80) + " [" + q.Hash + "] by TJ via tap (device iphone181): " + wantVerbatim
	if !slices.Equal(f.notes, []string{wantNote}) {
		t.Fatalf("notes =\n%q\nwant:\n%q", f.notes, []string{wantNote})
	}
	if len(f.told) != 1 || !strings.Contains(f.told[0], "TJ answered your needs_me question") || !strings.Contains(f.told[0], wantVerbatim) {
		t.Fatalf("told = %q", f.told)
	}
	recs, err := readJournal(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	foundJournal := false
	for _, r := range recs {
		if r.Kind == journalNeedsMe && r.Bead == "task-free" && r.Text == wantNote {
			foundJournal = true
		}
	}
	if !foundJournal {
		t.Fatalf("verbatim free-text answer not journaled: %+v", recs)
	}

	// Nonce was spent once: replay is refused.
	code, body = tapFormRequest(s, http.MethodPost, tok, url.Values{"text": {"second try"}}, tapPhoneAddr)
	if code != http.StatusConflict || !strings.Contains(body, "already used") {
		t.Fatalf("replay = %d %q", code, body)
	}
	if len(f.notes) != 1 || len(f.told) != 1 {
		t.Fatalf("replay acted again: notes=%q told=%q", f.notes, f.told)
	}
}

func TestTapOptionPlusTextAndOverLimitRefusedBeforeSpend(t *testing.T) {
	q, _ := parseNeedsMeQuestion(questionNotes)

	t.Run("option link confirm page and option+text verbatim", func(t *testing.T) {
		f := newFakeQuestion("idle")
		s := testQuestionTapServer(t, f, "iphone181")
		tok := questionTokenFor(t, s, q, "2")

		// Confirm page shows textarea + option button + text-only fallback button.
		code, body := tapRequest(s, http.MethodGet, tok, tapPhoneAddr)
		if code != http.StatusOK || !strings.Contains(body, `<textarea id="tap-text" name="text"`) ||
			!strings.Contains(body, "Answer 2. One region first, then the rest") ||
			!strings.Contains(body, "Send free-text answer only") {
			t.Fatalf("option confirm page = %d %q", code, body)
		}

		// Mismatched option choice on an option-bound link is refused BEFORE spending nonce.
		code, body = tapFormRequest(s, http.MethodPost, tok, url.Values{"choice": {"1"}, "text": {"trying option 1"}}, tapPhoneAddr)
		if code != http.StatusBadRequest || !strings.Contains(body, "for option 2, not 1") {
			t.Fatalf("mismatched option = %d %q", code, body)
		}

		// Over-limit ASCII text (2001 runes) refused BEFORE spending nonce.
		tooLongASCII := strings.Repeat("a", tapMaxFreeTextRunes+1)
		code, body = tapFormRequest(s, http.MethodPost, tok, url.Values{"text": {tooLongASCII}}, tapPhoneAddr)
		if code != http.StatusBadRequest || !strings.Contains(body, "2001 characters") || !strings.Contains(body, "not spent") {
			t.Fatalf("over-limit ASCII = %d %q", code, body)
		}

		// Over-limit multi-byte UTF-8 text (2001 runes) refused BEFORE spending nonce.
		tooLongUTF8 := strings.Repeat("—", tapMaxFreeTextRunes+1)
		code, body = tapFormRequest(s, http.MethodPost, tok, url.Values{"text": {tooLongUTF8}}, tapPhoneAddr)
		if code != http.StatusBadRequest || !strings.Contains(body, "2001 characters") {
			t.Fatalf("over-limit UTF-8 = %d %q", code, body)
		}

		// Oversized body (>64KiB) refused BEFORE spending nonce.
		huge := strings.Repeat("x", tapPostMaxBytes+100)
		code, body = tapFormRequest(s, http.MethodPost, tok, url.Values{"text": {huge}}, tapPhoneAddr)
		if code != http.StatusBadRequest || !strings.Contains(body, "not spent") {
			t.Fatalf("oversized body = %d %q", code, body)
		}
		if len(f.notes) != 0 || len(f.told) != 0 || f.bead.Status != StatusNeedsMe {
			t.Fatalf("refused submissions mutated state: notes=%q told=%q status=%s", f.notes, f.told, f.bead.Status)
		}

		// Same token STILL works after pre-spend refusals! Submit option 2 + verbatim text
		// preserving leading/trailing spaces and exact formatting (with double trailing newline
		// keeping one intentional trailing newline).
		rawText := "  Start with us-east-1; check p99 after 15m.  \n\n"
		wantText := "  Start with us-east-1; check p99 after 15m.  \n"
		code, body = tapFormRequest(s, http.MethodPost, tok, url.Values{"text": {rawText}}, tapPhoneAddr)
		if code != http.StatusOK || !strings.Contains(body, "Answered") {
			t.Fatalf("valid option+text after refusals = %d %q", code, body)
		}
		wantNote := "NEEDS_ME ANSWERED: 2 One region first, then the rest to " + shortTitle(q.Summary(), 80) + " [" + q.Hash + "] by TJ via tap (device iphone181): " + wantText
		if !slices.Equal(f.notes, []string{wantNote}) {
			t.Fatalf("notes =\n%q\nwant:\n%q", f.notes, []string{wantNote})
		}
		if len(f.told) != 1 || !strings.Contains(f.told[0], "2. One region first, then the rest") || !strings.Contains(f.told[0], wantText) {
			t.Fatalf("told = %q", f.told)
		}
		if f.bead.Status != StatusInProgress {
			t.Fatalf("status = %s", f.bead.Status)
		}
	})

	t.Run("exact 2000 multi-byte runes accepted", func(t *testing.T) {
		f := newFakeQuestion("idle")
		s := testQuestionTapServer(t, f, "iphone181")
		tok := questionTokenFor(t, s, q, tapChoiceFreeText)
		exact2000 := strings.Repeat("✓", tapMaxFreeTextRunes) // 6000 bytes, 2000 runes
		code, body := tapFormRequest(s, http.MethodPost, tok, url.Values{"text": {exact2000 + "\r\n"}}, tapPhoneAddr)
		if code != http.StatusOK || !strings.Contains(body, "Answered") {
			t.Fatalf("exact 2000 runes = %d %q", code, body)
		}
		if len(f.notes) != 1 || !strings.HasSuffix(f.notes[0], ": "+exact2000) {
			t.Fatalf("note did not preserve exact 2000 runes: %q", f.notes)
		}
	})

	t.Run("free-text link on OPTIONS question supports all three modes", func(t *testing.T) {
		// Verify confirm page offers textarea + free-text button + every option button.
		f := newFakeQuestion("idle")
		s := testQuestionTapServer(t, f, "iphone181")
		tok := questionTokenFor(t, s, q, tapChoiceFreeText)
		code, body := tapRequest(s, http.MethodGet, tok, tapPhoneAddr)
		if code != http.StatusOK {
			t.Fatalf("GET = %d %q", code, body)
		}
		for _, want := range []string{"Send free-text answer", "Answer 1. All regions at once", "Answer 2. One region first, then the rest", `maxlength="2000"`} {
			if !strings.Contains(body, want) {
				t.Errorf("confirm page missing %q:\n%s", want, body)
			}
		}

		// Mode 1: option alone via free-text link (whitespace-only text treated as no text).
		code, body = tapFormRequest(s, http.MethodPost, tok, url.Values{"choice": {"1"}, "text": {"   "}}, tapPhoneAddr)
		if code != http.StatusOK {
			t.Fatalf("option alone via free-text link = %d %q", code, body)
		}
		wantOptOnly := "NEEDS_ME ANSWERED: 1 All regions at once to " + shortTitle(q.Summary(), 80) + " [" + q.Hash + "] by TJ via tap (device iphone181)"
		if !slices.Equal(f.notes, []string{wantOptOnly}) {
			t.Fatalf("option alone notes = %q, want %q", f.notes, wantOptOnly)
		}

		// Mode 2: option + text via free-text link.
		f2 := newFakeQuestion("idle")
		s2 := testQuestionTapServer(t, f2, "iphone181")
		tok2 := questionTokenFor(t, s2, q, tapChoiceFreeText)
		code, body = tapFormRequest(s2, http.MethodPost, tok2, url.Values{"choice": {"2"}, "text": {"canary first"}}, tapPhoneAddr)
		if code != http.StatusOK || len(f2.notes) != 1 || !strings.HasSuffix(f2.notes[0], "by TJ via tap (device iphone181): canary first") || !strings.Contains(f2.notes[0], "NEEDS_ME ANSWERED: 2 ") {
			t.Fatalf("option+text via free-text link = %d notes=%q body=%q", code, f2.notes, body)
		}

		// Mode 3: free text alone via option link (choice=text).
		f3 := newFakeQuestion("idle")
		s3 := testQuestionTapServer(t, f3, "iphone181")
		tok3 := questionTokenFor(t, s3, q, "2")
		code, body = tapFormRequest(s3, http.MethodPost, tok3, url.Values{"choice": {tapChoiceFreeText}, "text": {"hold until Monday"}}, tapPhoneAddr)
		if code != http.StatusOK || len(f3.notes) != 1 || !strings.HasPrefix(f3.notes[0], "NEEDS_ME ANSWERED: free text to ") || !strings.HasSuffix(f3.notes[0], ": hold until Monday") {
			t.Fatalf("text-only via option link = %d notes=%q body=%q", code, f3.notes, body)
		}
	})

	t.Run("racing option link and free-text link answers at most once", func(t *testing.T) {
		f := newFakeQuestion("idle")
		s := testQuestionTapServer(t, f, "iphone181")
		optTok := questionTokenFor(t, s, q, "1")
		ftTok := questionTokenFor(t, s, q, tapChoiceFreeText)

		if code, body := tapFormRequest(s, http.MethodPost, ftTok, url.Values{"text": {"wait for signoff"}}, tapPhoneAddr); code != http.StatusOK {
			t.Fatalf("first (free-text) tap = %d %q", code, body)
		}
		if code, body := tapRequest(s, http.MethodPost, optTok, tapPhoneAddr); code != http.StatusConflict || !strings.Contains(body, "Not answered") {
			t.Fatalf("second (option) tap = %d %q, want 409", code, body)
		}
		if len(f.notes) != 1 || len(f.told) != 1 {
			t.Fatalf("second tap acted: notes=%q told=%q", f.notes, f.told)
		}
	})
}

func TestTapFreeTextStaleGuardsAndRunExclusion(t *testing.T) {
	baseNotes := "Should we rename the CLI subcommand before tagging v1.2?\nCLASS: 2 — cli naming"
	q, ok := parseTappableNeedsMeQuestion(baseNotes)
	if !ok {
		t.Fatal("base free-text question did not parse")
	}

	cases := []struct {
		name   string
		agent  string
		mutate func(f *fakeQuestion)
	}{
		{"question text changed", "idle", func(f *fakeQuestion) {
			f.bead.Notes = "Should we rename both the CLI subcommand and config key?\nCLASS: 2 — cli naming"
		}},
		{"class changed", "idle", func(f *fakeQuestion) {
			f.bead.Notes = "Should we rename the CLI subcommand before tagging v1.2?\nCLASS: 4 — breaking change"
		}},
		{"bead left needs_me", "idle", func(f *fakeQuestion) {
			f.bead.Status = StatusInProgress
		}},
		{"later note appended", "idle", func(f *fakeQuestion) {
			f.bead.Notes += "\nnever mind, decided in pane"
		}},
		{"already answered", "idle", func(f *fakeQuestion) {
			f.bead.Notes += "\nNEEDS_ME ANSWERED: free text to Should we rename... [" + q.Hash + "] by TJ via tap (device iphone181): keep it"
		}},
		{"worker already working", "working", func(f *fakeQuestion) {}},
		{"became RUN command", "idle", func(f *fakeQuestion) {
			f.bead.Notes = "RUN: aws sso login\nwhy: session expired"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeQuestion(tc.agent)
			f.bead.Notes = baseNotes
			tc.mutate(f)
			s := testQuestionTapServer(t, f, "iphone181")
			tok := questionTokenFor(t, s, q, tapChoiceFreeText)

			// GET confirm refused (409).
			if code, body := tapRequest(s, http.MethodGet, tok, tapPhoneAddr); code != http.StatusConflict || !strings.Contains(body, "Not answered") {
				t.Fatalf("GET = %d %q, want 409", code, body)
			}
			// POST answer refused (409) and mutates nothing.
			if code, body := tapFormRequest(s, http.MethodPost, tok, url.Values{"text": {"keep current name"}}, tapPhoneAddr); code != http.StatusConflict || !strings.Contains(body, "Not answered") {
				t.Fatalf("POST = %d %q, want 409", code, body)
			}
			if len(f.notes) != 0 || len(f.told) != 0 || (f.bead.Status == StatusInProgress && tc.name != "bead left needs_me") {
				t.Fatalf("stale free-text tap mutated state: notes=%q told=%q status=%s", f.notes, f.told, f.bead.Status)
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
				t.Fatalf("no TAP REFUSED journal entry: %+v", recs)
			}
		})
	}

	// Prompt token with free-text sentinel is rejected at mint and parse.
	secret := []byte("0123456789abcdef0123456789abcdef")
	if _, _, err := mintTapTokenKind(secret, tapKindPrompt, "task-1", "abcd1234", tapChoiceFreeText, time.Now()); err == nil {
		t.Error("minted prompt token with free-text choice")
	}

	// Worker not prompted (no worker / blocked) for free-text tap leaves follow-up note.
	for name, agentStatus := range map[string]string{"no worker": "", "blocked at prompt": "blocked"} {
		t.Run("not prompted "+name, func(t *testing.T) {
			f := newFakeQuestion(agentStatus)
			f.bead.Notes = baseNotes
			s := testQuestionTapServer(t, f, "iphone181")
			tok := questionTokenFor(t, s, q, tapChoiceFreeText)
			code, body := tapFormRequest(s, http.MethodPost, tok, url.Values{"text": {"keep it as-is"}}, tapPhoneAddr)
			if code != http.StatusOK || !strings.Contains(body, "was not prompted") {
				t.Fatalf("answer = %d %q", code, body)
			}
			if f.bead.Status != StatusInProgress || len(f.told) != 0 || len(f.notes) != 2 || !strings.HasSuffix(f.notes[0], ": keep it as-is") {
				t.Fatalf("status=%s told=%q notes=%q", f.bead.Status, f.told, f.notes)
			}
		})
	}
}
