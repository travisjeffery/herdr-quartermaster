package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestInboxQuestionsJSONBothKindsAndHashSeparation(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	bashScreen := screen(t, "claude-bash-4.txt")
	wantBash, ok := parsePrompt(bashScreen)
	if !ok {
		t.Fatal("claude-bash-4 fixture did not parse")
	}
	museScreen := screen(t, "muse-bash-wide.txt")
	wantMuse, ok := parsePrompt(museScreen)
	if !ok {
		t.Fatal("muse-bash-wide fixture did not parse")
	}
	piAskFixture := &piAsk{
		ID:      "1-abc",
		Command: "git push origin feat/demo",
		Reason:  "push requires approval",
		Options: []string{"Yes, run it", "No, block it"},
	}
	wantPi := piAskFixture.prompt()
	wantNeedsMe, ok := parseNeedsMeQuestion(questionNotes)
	if !ok {
		t.Fatal("questionNotes did not parse")
	}

	// Keep an unhandled inbox item around to prove --questions marks nothing done.
	existing := inboxFixture(t, Event{Bead: "task-ask", Kind: EventNeedsYou, Summary: "waiting", At: t0})

	readCalled := map[string]int{}
	env := inboxQuestionsEnv{
		active: func() ([]Bead, error) {
			return []Bead{
				{ID: "task-ask", Status: StatusNeedsMe, Notes: questionNotes},
				{ID: "task-bash", Status: StatusInProgress},
				{ID: "task-muse", Status: StatusBlocked},
				{ID: "task-pi", Status: StatusInProgress},
				// Idle worker on in_progress must never appear or trigger pane reads.
				{ID: "task-idle", Status: StatusInProgress},
			}, nil
		},
		agents: func(host string) ([]Agent, error) {
			if host != localHost {
				t.Fatalf("unexpected host %q", host)
			}
			return []Agent{
				{Name: "task-ask", Status: "idle", PaneID: "w1:p1"},
				{Name: "task-bash", Status: "blocked", PaneID: "w1:p2"},
				{Name: "task-muse", Status: "blocked", PaneID: "w1:p3"},
				{Name: "task-pi", Status: "blocked", PaneID: "w1:p4", Session: AgentSession{Agent: "pi"}},
				{Name: "task-idle", Status: "idle", PaneID: "w1:p5"},
			}, nil
		},
		prompt: promptEnv{
			piAsk: func(host string, a *Agent) (*piAsk, bool, error) {
				if a.Name == "task-pi" {
					return piAskFixture, true, nil
				}
				return nil, false, nil
			},
			read: func(host, pane string) (string, error) {
				readCalled[pane]++
				switch pane {
				case "w1:p2":
					return bashScreen, nil
				case "w1:p3":
					return museScreen, nil
				default:
					return "", errors.New("unexpected pane read: " + pane)
				}
			},
		},
		state: func() TickerState { return TickerState{Threads: map[string]Snapshot{}} },
	}

	qs, err := inboxQuestions(env)
	if err != nil {
		t.Fatal(err)
	}
	if readCalled["w1:p1"] != 0 || readCalled["w1:p4"] != 0 || readCalled["w1:p5"] != 0 {
		t.Fatalf("unexpected pane reads: %+v", readCalled)
	}

	var buf bytes.Buffer
	if err := writeInboxQuestions(&buf, qs, true); err != nil {
		t.Fatal(err)
	}
	rawJSON := buf.String()

	// Two hash worlds stay separate: needs_me's tap-token hash never appears,
	// and no tap links or secret files are minted.
	if strings.Contains(rawJSON, wantNeedsMe.Hash) {
		t.Fatalf("needs_me tap hash %q leaked into output:\n%s", wantNeedsMe.Hash, rawJSON)
	}
	if strings.Contains(rawJSON, "/tap/") {
		t.Fatalf("tap link leaked into output:\n%s", rawJSON)
	}
	if _, err := os.Stat(tapSecretPath()); !os.IsNotExist(err) {
		t.Fatalf("inbox --questions created tap secret: %v", err)
	}
	if items, err := readInbox(); err != nil || len(items) != 1 || items[0].File != existing.File {
		t.Fatalf("inbox --questions modified inbox items: %+v, %v", items, err)
	}

	// Verify exact schema keys and explicit null fields structurally.
	var rawRows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rawRows); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, rawJSON)
	}
	wantKeys := []string{"bead", "bead_status", "class", "expires", "hash", "kind", "options", "question", "recommendation"}
	if len(rawRows) != 4 {
		t.Fatalf("want 4 rows, got %d:\n%s", len(rawRows), rawJSON)
	}
	for i, row := range rawRows {
		keys := make([]string, 0, len(row))
		for k := range row {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, wantKeys) {
			t.Fatalf("row %d keys = %v, want %v", i, keys, wantKeys)
		}
		if row["expires"] != nil {
			t.Errorf("row %d expires = %v, want null", i, row["expires"])
		}
	}

	var got []InboxQuestion
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	byBead := map[string]InboxQuestion{}
	for _, q := range got {
		if _, dup := byBead[q.Bead]; dup {
			t.Fatalf("duplicate bead %s in %+v", q.Bead, got)
		}
		byBead[q.Bead] = q
	}

	// 1. needs_me OPTIONS question
	ask := byBead["task-ask"]
	if ask.Kind != questionKindNeedsMe || ask.BeadStatus != StatusNeedsMe {
		t.Fatalf("task-ask kind/status = %+v", ask)
	}
	if ask.Hash != nil || ask.Expires != nil {
		t.Fatalf("task-ask hash/expires must be nil: %+v", ask)
	}
	if ask.Class == nil || *ask.Class != 7 {
		t.Fatalf("task-ask class = %v, want 7", ask.Class)
	}
	if ask.Recommendation == nil || *ask.Recommendation != 2 {
		t.Fatalf("task-ask recommendation = %v, want 2", ask.Recommendation)
	}
	if !strings.Contains(ask.Question, "Ship to all regions or one first?") {
		t.Errorf("task-ask question = %q", ask.Question)
	}
	wantAskOpts := []InboxQuestionOption{
		{N: 1, Label: "All regions at once"},
		{N: 2, Label: "One region first, then the rest"},
	}
	if !slices.Equal(ask.Options, wantAskOpts) {
		t.Errorf("task-ask options = %+v, want %+v", ask.Options, wantAskOpts)
	}

	// 2. Claude Bash prompt
	bash := byBead["task-bash"]
	if bash.Kind != questionKindPrompt || bash.BeadStatus != StatusInProgress {
		t.Fatalf("task-bash kind/status = %+v", bash)
	}
	if bash.Hash == nil || *bash.Hash != wantBash.Hash {
		t.Fatalf("task-bash hash = %v, want %q", bash.Hash, wantBash.Hash)
	}
	if bash.Class != nil || bash.Recommendation != nil || bash.Expires != nil {
		t.Fatalf("task-bash class/rec/expires must be nil: %+v", bash)
	}
	if !strings.Contains(bash.Question, "mkdir -p out2 && touch out2/hello.txt") {
		t.Errorf("task-bash question omitted actual command: %q", bash.Question)
	}
	if strings.Contains(bash.Question, "Tip:") {
		t.Errorf("task-bash question kept Tip noise: %q", bash.Question)
	}
	if len(bash.Options) != 4 || bash.Options[0].N != 1 || bash.Options[0].Label != "Yes" || bash.Options[3].Label != "No" {
		t.Errorf("task-bash options = %+v", bash.Options)
	}

	// 3. Muse approval prompt
	muse := byBead["task-muse"]
	if muse.Kind != questionKindPrompt || muse.BeadStatus != StatusBlocked || muse.Hash == nil || *muse.Hash != wantMuse.Hash {
		t.Fatalf("task-muse = %+v, want hash %q", muse, wantMuse.Hash)
	}
	if len(muse.Options) != 2 || muse.Options[0].Label != "Yes, proceed (y)" {
		t.Errorf("task-muse options = %+v", muse.Options)
	}

	// 4. Pi control-socket ask prompt
	piRow := byBead["task-pi"]
	if piRow.Kind != questionKindPrompt || piRow.BeadStatus != StatusInProgress || piRow.Hash == nil || *piRow.Hash != wantPi.Hash {
		t.Fatalf("task-pi = %+v, want hash %q", piRow, wantPi.Hash)
	}
	if !strings.Contains(piRow.Question, "git push origin feat/demo") {
		t.Errorf("task-pi question = %q", piRow.Question)
	}
	wantPiOpts := []InboxQuestionOption{{N: 1, Label: "Yes, run it"}, {N: 2, Label: "No, block it"}}
	if !slices.Equal(piRow.Options, wantPiOpts) {
		t.Errorf("task-pi options = %+v", piRow.Options)
	}
}

func TestInboxQuestionsExcludesRunAnsweredFreeTextAndUnparseable(t *testing.T) {
	idleScreen := screen(t, "claude-idle.txt")
	bashScreen := screen(t, "claude-bash-4.txt")
	wantBash, _ := parsePrompt(bashScreen)
	unpromptedBead := Bead{ID: "task-unprompted", Status: StatusNeedsMe, Notes: questionNotes}

	env := inboxQuestionsEnv{
		active: func() ([]Bead, error) {
			return []Bead{
				// RUN: beads must never appear (idle or blocked).
				{ID: "task-run-idle", Status: StatusNeedsMe, Notes: "RUN: aws sso login\nwhy: session expired"},
				{ID: "task-run-blocked", Status: StatusNeedsMe, Notes: "RUN: aws sso login\nwhy: session expired"},
				// Already answered: status returned to in_progress.
				{ID: "task-answered-status", Status: StatusInProgress, Notes: questionNotes},
				// Already answered: NEEDS_ME ANSWERED note appended after OPTIONS.
				{ID: "task-answered-note", Status: StatusNeedsMe, Notes: questionNotes + "\nNEEDS_ME ANSWERED: 2 One region first [abcd1234] by TJ via tap"},
				// Already answered: later note moved past OPTIONS.
				{ID: "task-moved-on", Status: StatusNeedsMe, Notes: questionNotes + "\nnever mind, resolved in pane"},
				// Already answered: worker is actively working again.
				{ID: "task-working", Status: StatusNeedsMe, Notes: questionNotes},
				// Unprompted wake on needs_me is NOT answered: must still appear.
				unpromptedBead,
				// Superseded earlier question followed by a new OPTIONS block: appears once with new question.
				{ID: "task-superseded", Status: StatusNeedsMe, Notes: "QUESTION: First?\nOPTIONS:\n1. A\n2. B\nNEEDS_ME ANSWERED: 1 A [11111111] by TJ via tap\nQUESTION: Second? Recommendation: option 2 — better\nOPTIONS:\n1. C\n2. D\nCLASS: 0 — read-only"},
				// Free-text needs_me without OPTIONS never appears.
				{ID: "task-freetext", Status: StatusNeedsMe, Notes: "Should we rename this flag?"},
				// Blocked worker with unparseable screen never appears.
				{ID: "task-unparseable", Status: StatusInProgress},
				// needs_me with OPTIONS + blocked worker with unparseable screen falls through to needs_me.
				{ID: "task-fallback-needs-me", Status: StatusNeedsMe, Notes: "CRITICAL: deleting bucket\nQUESTION: Delete now?\nOPTIONS:\n1. Delete\n2. Keep"},
				// needs_me with OPTIONS + blocked worker at a parseable prompt appears once as prompt.
				{ID: "task-both", Status: StatusNeedsMe, Notes: questionNotes},
				// Pi worker with control socket but no active ask (already answered).
				{ID: "task-pi-cleared", Status: StatusInProgress},
			}, nil
		},
		agents: func(host string) ([]Agent, error) {
			return []Agent{
				{Name: "task-run-idle", Status: "idle", PaneID: "p1"},
				{Name: "task-run-blocked", Status: "blocked", PaneID: "p2"},
				{Name: "task-working", Status: "working", PaneID: "p3", Seq: 5},
				{Name: "task-unprompted", Status: "working", PaneID: "p4", Seq: 7},
				{Name: "task-unparseable", Status: "blocked", PaneID: "p5"},
				{Name: "task-fallback-needs-me", Status: "blocked", PaneID: "p6"},
				{Name: "task-both", Status: "blocked", PaneID: "p7"},
				{Name: "task-pi-cleared", Status: "blocked", PaneID: "p8", Session: AgentSession{Agent: "pi"}},
			}, nil
		},
		prompt: promptEnv{
			piAsk: func(host string, a *Agent) (*piAsk, bool, error) {
				if a.Name == "task-pi-cleared" {
					return nil, true, nil
				}
				return nil, false, nil
			},
			read: func(host, pane string) (string, error) {
				switch pane {
				case "p2", "p7":
					return bashScreen, nil
				case "p5", "p6":
					return idleScreen, nil
				default:
					return "", errors.New("unexpected read: " + pane)
				}
			},
		},
		state: func() TickerState {
			return TickerState{
				Threads: map[string]Snapshot{
					"task-unprompted": {
						NeedsMe:     true,
						Unprompted:  true,
						AgentStatus: "working",
						AgentSeq:    7,
						Asked:       asked(unpromptedBead),
					},
				},
			}
		},
	}

	qs, err := inboxQuestions(env)
	if err != nil {
		t.Fatal(err)
	}
	gotIDs := make([]string, len(qs))
	byID := map[string]InboxQuestion{}
	for i, q := range qs {
		gotIDs[i] = q.Bead
		byID[q.Bead] = q
	}
	wantIDs := []string{"task-both", "task-fallback-needs-me", "task-superseded", "task-unprompted"}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("beads = %v, want %v", gotIDs, wantIDs)
	}

	both := byID["task-both"]
	if both.Kind != questionKindPrompt || both.BeadStatus != StatusNeedsMe || both.Hash == nil || *both.Hash != wantBash.Hash {
		t.Errorf("task-both = %+v", both)
	}

	fb := byID["task-fallback-needs-me"]
	if fb.Kind != questionKindNeedsMe || fb.Class != nil || fb.Recommendation != nil || fb.Hash != nil {
		t.Errorf("task-fallback-needs-me = %+v", fb)
	}
	if !strings.HasPrefix(fb.Question, "CRITICAL: deleting bucket\nDelete now?") {
		t.Errorf("task-fallback-needs-me question = %q", fb.Question)
	}

	sup := byID["task-superseded"]
	if sup.Kind != questionKindNeedsMe || sup.Class == nil || *sup.Class != 0 || sup.Recommendation == nil || *sup.Recommendation != 2 {
		t.Errorf("task-superseded = %+v", sup)
	}
	if !strings.HasPrefix(sup.Question, "Second?") {
		t.Errorf("task-superseded question = %q", sup.Question)
	}

	unp := byID["task-unprompted"]
	if unp.Kind != questionKindNeedsMe || unp.Hash != nil {
		t.Errorf("task-unprompted = %+v", unp)
	}
}

func TestNeedsMeRecommendationParsing(t *testing.T) {
	cases := []struct {
		name  string
		notes string
		want  *int
	}{
		{"inline number", "QUESTION: Ship? Recommendation: 2 — safer\nOPTIONS:\n1. A\n2. B", intPtr(2)},
		{"separate line", "QUESTION: Ship?\nRecommendation: 1 — faster\nOPTIONS:\n1. A\n2. B", intPtr(1)},
		{"option prefix", "QUESTION: Ship? recommendation: option 2\nOPTIONS:\n1. A\n2. B", intPtr(2)},
		{"hash prefix", "QUESTION: Ship? Recommendation: #1 — first\nOPTIONS:\n1. A\n2. B", intPtr(1)},
		{"markdown bold", "QUESTION: Ship? Recommendation: **2** — safer\nOPTIONS:\n1. A\n2. B", intPtr(2)},
		{"markdown code", "QUESTION: Ship? Recommendation: `1` — faster\nOPTIONS:\n1. A\n2. B", intPtr(1)},
		{"absent", "QUESTION: Ship?\nOPTIONS:\n1. A\n2. B", nil},
		{"out of range", "QUESTION: Ship? Recommendation: 5 — nope\nOPTIONS:\n1. A\n2. B", nil},
		{"zero", "QUESTION: Ship? Recommendation: 0 — nope\nOPTIONS:\n1. A\n2. B", nil},
		{"non numeric", "QUESTION: Ship? Recommendation: wait 3 days\nOPTIONS:\n1. A\n2. B", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, ok := parseNeedsMeQuestion(tc.notes)
			if !ok {
				t.Fatalf("did not parse: %q", tc.notes)
			}
			got := q.recommendation()
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Errorf("recommendation = %v, want %v", got, tc.want)
			}
		})
	}
}

func intPtr(n int) *int { return &n }

func TestInboxQuestionsRemoteHostAndAgentErrorDegradation(t *testing.T) {
	bashScreen := screen(t, "claude-bash-4.txt")
	wantBash, _ := parsePrompt(bashScreen)
	var warned []string

	env := inboxQuestionsEnv{
		active: func() ([]Bead, error) {
			return []Bead{
				{ID: "task-remote", Status: StatusInProgress, Labels: []string{"host:server"}},
				{ID: "task-local-ask", Status: StatusNeedsMe, Notes: questionNotes},
			}, nil
		},
		agents: func(host string) ([]Agent, error) {
			if host == localHost {
				return nil, errors.New("herdr unavailable")
			}
			if host == "server" {
				return []Agent{{Name: "task-remote", Status: "blocked", PaneID: "r1:p1", Host: "server"}}, nil
			}
			return nil, errors.New("unexpected host: " + host)
		},
		prompt: promptEnv{
			read: func(host, pane string) (string, error) {
				if host == "server" && pane == "r1:p1" {
					return bashScreen, nil
				}
				return "", errors.New("unexpected read")
			},
		},
		warnf: func(format string, args ...any) {
			warned = append(warned, format)
		},
	}

	qs, err := inboxQuestions(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(warned) != 1 {
		t.Fatalf("expected 1 warning for local herdr failure, got %v", warned)
	}
	if len(qs) != 2 || qs[0].Bead != "task-local-ask" || qs[0].Kind != questionKindNeedsMe || qs[1].Bead != "task-remote" || qs[1].Kind != questionKindPrompt || qs[1].Hash == nil || *qs[1].Hash != wantBash.Hash {
		t.Fatalf("qs = %+v", qs)
	}
}

func TestWriteInboxQuestionsEmptyAndTextAndCmdErrors(t *testing.T) {
	var jsonBuf bytes.Buffer
	if err := writeInboxQuestions(&jsonBuf, nil, true); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(jsonBuf.String()) != "[]" {
		t.Fatalf("empty json = %q, want []", jsonBuf.String())
	}

	var textBuf bytes.Buffer
	if err := writeInboxQuestions(&textBuf, nil, false); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(textBuf.String()) != "(none)" {
		t.Fatalf("empty text = %q, want (none)", textBuf.String())
	}

	c := 7
	r := 2
	h := "abcd1234"
	textBuf.Reset()
	err := writeInboxQuestions(&textBuf, []InboxQuestion{
		{
			Bead:           "task-ask",
			Kind:           questionKindNeedsMe,
			Question:       "Ship now?",
			Recommendation: &r,
			Class:          &c,
			Options:        []InboxQuestionOption{{N: 1, Label: "Yes"}, {N: 2, Label: "Later"}},
			BeadStatus:     StatusNeedsMe,
		},
		{
			Bead:       "task-cmd",
			Kind:       questionKindPrompt,
			Question:   "Bash command\nrm -f tmp",
			Options:    []InboxQuestionOption{{N: 1, Label: "Yes"}, {N: 2, Label: "No"}},
			BeadStatus: StatusInProgress,
			Hash:       &h,
		},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	out := textBuf.String()
	for _, want := range []string{"task-ask [needs_me · C7 · rec 2]", "Ship now?", "1 Yes · 2 Later", "task-cmd [prompt · abcd1234]"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output lacks %q:\n%s", want, out)
		}
	}

	cfg := defaultConfig()
	if err := inboxCmd(cfg, Herdr{}, []string{"done"}, true, true, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Error("--questions done succeeded")
	}
	if err := inboxCmd(cfg, Herdr{}, []string{"done"}, false, true, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Error("done --json succeeded")
	}
	if err := inboxCmd(cfg, Herdr{}, nil, false, false, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Error("bare inbox succeeded")
	}
}

func TestInboxQuestionsEndToEndCLIAndJQ(t *testing.T) {
	dir := fakeTools(t)
	bashScreen := screen(t, "claude-bash-4.txt")
	wantBash, _ := parsePrompt(bashScreen)
	wantAsk, _ := parseNeedsMeQuestion(questionNotes)

	beadsJSON, err := json.Marshal([]Bead{
		{ID: "task-ask", Status: StatusNeedsMe, Notes: questionNotes},
		{ID: "task-bash", Status: StatusInProgress},
		{ID: "task-run", Status: StatusNeedsMe, Notes: "RUN: aws sso login\nwhy: expired"},
		{ID: "task-answered", Status: StatusNeedsMe, Notes: questionNotes + "\nNEEDS_ME ANSWERED: 1 All [deadbeef] by TJ via tap"},
	})
	if err != nil {
		t.Fatal(err)
	}
	bdScript := "#!/bin/sh\nif [ \"$1\" = list ]; then cat <<'EOF'\n" + string(beadsJSON) + "\nEOF\nfi\n"
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte(bdScript), 0o700); err != nil {
		t.Fatal(err)
	}
	screenFile := filepath.Join(dir, "bash.txt")
	if err := os.WriteFile(screenFile, []byte(bashScreen), 0o600); err != nil {
		t.Fatal(err)
	}
	herdrScript := "#!/bin/sh\n" +
		"if [ \"$1\" = agent ] && [ \"$2\" = list ]; then\n" +
		"  printf '%s\\n' '{\"result\":{\"agents\":[{\"name\":\"task-ask\",\"agent_status\":\"idle\",\"pane_id\":\"w1:p1\"},{\"name\":\"task-bash\",\"agent_status\":\"blocked\",\"pane_id\":\"w1:p2\"}]}}'\n" +
		"elif [ \"$1\" = agent ] && [ \"$2\" = read ] && [ \"$3\" = w1:p2 ]; then\n" +
		"  cat " + shellQuote(screenFile) + "\n" +
		"fi\n"
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte(herdrScript), 0o700); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	cfg := defaultConfig()
	cfg.Repo = t.TempDir()
	if err := inboxCmd(cfg, newHerdr(), nil, true, true, &out, &errOut); err != nil {
		t.Fatalf("inboxCmd failed: %v (stderr: %s)", err, errOut.String())
	}

	var got []InboxQuestion
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out.String())
	}
	if len(got) != 2 {
		t.Fatalf("want 2 questions, got %d:\n%s", len(got), out.String())
	}
	if got[0].Bead != "task-ask" || got[0].Kind != questionKindNeedsMe || got[0].Hash != nil || got[0].Class == nil || *got[0].Class != 7 || got[0].Recommendation == nil || *got[0].Recommendation != 2 {
		t.Fatalf("got[0] = %+v", got[0])
	}
	if got[1].Bead != "task-bash" || got[1].Kind != questionKindPrompt || got[1].Hash == nil || *got[1].Hash != wantBash.Hash || got[1].Class != nil || got[1].Recommendation != nil {
		t.Fatalf("got[1] = %+v", got[1])
	}
	if strings.Contains(out.String(), wantAsk.Hash) {
		t.Fatalf("needs_me hash leaked into CLI output")
	}

	// Verify output parses with jq when jq is installed on the host.
	if jqPath, err := exec.LookPath("jq"); err == nil {
		cmd := exec.Command(jqPath, "-e", `type == "array" and length == 2 and (.[0].kind == "needs_me" and .[0].hash == null and .[0].class == 7 and .[0].recommendation == 2 and .[0].expires == null) and (.[1].kind == "prompt" and (.[1].hash | type == "string") and .[1].class == null and .[1].recommendation == null and .[1].expires == null)`)
		cmd.Stdin = bytes.NewReader(out.Bytes())
		if jqOut, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("jq check failed: %v\n%s\ninput:\n%s", err, string(jqOut), out.String())
		}
	}

	// Verify main.go flag wiring (`run([]string{"inbox", "--questions", "--json"})`).
	cfgDir := t.TempDir()
	t.Setenv("QUARTERMASTER_CONFIG_DIR", cfgDir)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("repo = \""+cfg.Repo+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var captured bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = captured.ReadFrom(r)
		close(done)
	}()
	os.Stdout = w
	runErr := run([]string{"inbox", "--questions", "--json"})
	_ = w.Close()
	os.Stdout = origStdout
	<-done
	_ = r.Close()
	if runErr != nil {
		t.Fatalf("run(inbox --questions --json): %v", runErr)
	}
	if !bytes.Equal(captured.Bytes(), out.Bytes()) {
		t.Fatalf("run output differed from inboxCmd:\ngot:\n%s\nwant:\n%s", captured.String(), out.String())
	}
}
