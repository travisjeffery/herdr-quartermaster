package main

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeCardStore struct {
	beads map[string]Bead
	notes map[string][]string
	now   time.Time
}

func newFakeCardStore(beads ...Bead) *fakeCardStore {
	s := &fakeCardStore{
		beads: map[string]Bead{},
		notes: map[string][]string{},
		now:   time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC),
	}
	for _, b := range beads {
		s.beads[b.ID] = b
	}
	return s
}

func (s *fakeCardStore) env() cardEnv {
	return cardEnv{
		show: func(id string) (Bead, error) {
			b, ok := s.beads[id]
			if !ok {
				return Bead{}, os.ErrNotExist
			}
			return b, nil
		},
		note: func(id, text string) error {
			b := s.beads[id]
			if b.Notes == "" {
				b.Notes = text
			} else {
				b.Notes += "\n" + text
			}
			s.beads[id] = b
			s.notes[id] = append(s.notes[id], text)
			return nil
		},
		setStatus: func(id, status string) error {
			b := s.beads[id]
			b.Status = status
			b.UpdatedAt = s.now
			s.beads[id] = b
			return nil
		},
		now: func() time.Time { return s.now },
	}
}

func TestQuestionCardHashParityAndTapCompat(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := defaultConfig()
	store := newFakeCardStore(Bead{
		ID:     "b-1",
		Title:  "Roll out feature",
		Status: StatusInProgress,
		Notes:  "PR: https://github.com/acme/app/pull/42",
	})

	input := `{
  "question": "Roll out to one region first or all regions?",
  "details": "Canary metrics are still warming up in region A.",
  "class": 7,
  "class_reason": "production rollout",
  "recommendation": {"option": 2, "rationale": "limit blast radius"},
  "options": [
    {"number": 1, "label": "All regions at once", "description": "Faster rollout", "class": 7},
    {"number": 2, "label": "One region first", "description": "Canary first", "class": 7}
  ],
  "evidence": {
    "pr_url": "https://github.com/acme/app/pull/42",
    "cost_estimate": "$120/month",
    "links": [{"label": "PR", "url": "https://github.com/acme/app/pull/42"}]
  }
}`
	var out strings.Builder
	if err := cardCmd(cfg, store.env(), []string{"ask", "b-1", "--file", "-", "--json"}, strings.NewReader(input), &out); err != nil {
		t.Fatalf("card ask failed: %v", err)
	}

	var got QuestionCard
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
		t.Fatalf("decode output JSON: %v\n%s", err, out.String())
	}
	if got.Version != 1 || got.Bead != "b-1" || got.Class != 7 || got.Critical {
		t.Fatalf("unexpected card header: %+v", got)
	}
	if len(got.Hash) != 8 {
		t.Fatalf("expected 8-char hash, got %q", got.Hash)
	}

	// Verify private file and directory permissions.
	p, err := questionCardPath("b-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		path string
		perm os.FileMode
	}{
		{cardsDir(), 0o700},
		{filepath.Dir(p), 0o700},
		{p, 0o600},
	} {
		fi, err := os.Stat(check.path)
		if err != nil {
			t.Fatalf("stat %s: %v", check.path, err)
		}
		if fi.Mode().Perm() != check.perm {
			t.Errorf("%s perm = %o, want %o", check.path, fi.Mode().Perm(), check.perm)
		}
	}

	// Verify persisted JSON matches returned card.
	disk, err := readQuestionCard("b-1")
	if err != nil {
		t.Fatalf("readQuestionCard: %v", err)
	}
	if disk.Hash != got.Hash || disk.Class != 7 || disk.Recommendation == nil || disk.Recommendation.Option != 2 {
		t.Fatalf("disk card mismatch: %+v", disk)
	}

	// Verify bead status and exact hash parity with tapquestion.go.
	b := store.beads["b-1"]
	if b.Status != StatusNeedsMe {
		t.Fatalf("bead status = %q, want %q", b.Status, StatusNeedsMe)
	}
	parsed, ok := parseNeedsMeQuestion(b.Notes)
	if !ok {
		t.Fatalf("parseNeedsMeQuestion failed on mirrored notes:\n%s", b.Notes)
	}
	if parsed.Hash != got.Hash {
		t.Fatalf("hash mismatch: parseNeedsMeQuestion=%q vs card=%q\nnotes:\n%s", parsed.Hash, got.Hash, b.Notes)
	}
	if parsed.ClassN != 7 || len(parsed.Options) != 2 {
		t.Fatalf("parsed question = %+v", parsed)
	}
	if live, why := questionCardLive(disk, b); !live {
		t.Fatalf("expected card to be live, got stale: %s", why)
	}

	// Context renders the card summary line alongside question/options.
	ctxOut := renderContext(cfg, []Thread{thread(b, agent("idle", 1), nil)}, nil, nil, nil, TickerState{}, nil, store.now)
	if !strings.Contains(ctxOut, "card: C7 · rec 2") {
		t.Errorf("context missing live card summary:\n%s", ctxOut)
	}

	// End-to-end tap answer using the card's Hash.
	fq := &fakeQuestion{
		bead:  b,
		agent: &Agent{Name: "b-1", Status: "idle", PaneID: "w1:p1"},
	}
	s, err := newTapServer(TapConfig{Enabled: true, Devices: []string{"iphone181"}}, fq.env())
	if err != nil {
		t.Fatal(err)
	}
	s.whois = func(string) (tapIdentity, error) { return tapIdentity{Device: "iphone181", User: "u@"}, nil }
	tok, _, err := mintTapTokenKind(s.secret, tapKindQuestion, "b-1", got.Hash, "2", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	code, body := tapRequest(s, http.MethodPost, tok, tapPhoneAddr)
	if code != http.StatusOK || !strings.Contains(body, "Answered") {
		t.Fatalf("tap answer failed: %d %q", code, body)
	}
	if fq.bead.Status != StatusInProgress {
		t.Fatalf("after tap, status = %s", fq.bead.Status)
	}
	// After answering, the card is treated as stale.
	if live, why := questionCardLive(disk, fq.bead); live {
		t.Fatalf("card should be stale after tap answer")
	} else if !strings.Contains(why, "in_progress") {
		t.Errorf("unexpected stale reason: %q", why)
	}
}

func TestQuestionCardIACPreviewClassElevation(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := defaultConfig()
	cfg.Autonomy.Prod = []string{"*prod*"}
	cfg.Autonomy.Widespread = 5

	cases := []struct {
		name         string
		preview      string
		target       string
		expect       string
		statedClass  string
		wantClass    int
		wantCritical bool
	}{
		{
			name:         "staging update cannot be lowered to 2",
			preview:      "testdata/plans/tofu-update.json",
			target:       "org/app/staging",
			expect:       "create=1,update=1",
			statedClass:  "2",
			wantClass:    6,
			wantCritical: false,
		},
		{
			name:         "prod update cannot be lowered to 4",
			preview:      "testdata/plans/tofu-update.json",
			target:       "org/app/prod",
			expect:       "create=1,update=1",
			statedClass:  "4",
			wantClass:    7,
			wantCritical: false,
		},
		{
			name:         "stateful replace elevates to 8 and critical",
			preview:      "testdata/plans/pulumi-replace.json",
			target:       "org/app/staging",
			expect:       "",
			statedClass:  "3",
			wantClass:    8,
			wantCritical: true,
		},
		{
			name:         "expect mismatch elevates to 9 and critical",
			preview:      "testdata/plans/tofu-update.json",
			target:       "org/app/staging",
			expect:       "create=99",
			statedClass:  "5",
			wantClass:    9,
			wantCritical: true,
		},
		{
			name:         "unknown op elevates to 9 and critical",
			preview:      "testdata/plans/tofu-destroy.json",
			target:       "org/app/staging",
			expect:       "",
			statedClass:  "4",
			wantClass:    9,
			wantCritical: true,
		},
		{
			name:         "stated class higher than preview is preserved",
			preview:      "testdata/plans/tofu-update.json",
			target:       "org/app/staging",
			expect:       "create=1,update=1",
			statedClass:  "8",
			wantClass:    8,
			wantCritical: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeCardStore(Bead{ID: "b-iac", Status: StatusInProgress})
			args := []string{
				"ask", "b-iac",
				"--question", "Apply infrastructure change?",
				"--option", "Approve apply",
				"--option", "Abort",
				"--class", tc.statedClass,
				"--iac-preview", tc.preview,
				"--iac-target", tc.target,
				"--json",
			}
			if tc.expect != "" {
				args = append(args, "--iac-expect", tc.expect)
			}
			var out strings.Builder
			if err := cardCmd(cfg, store.env(), args, nil, &out); err != nil {
				t.Fatalf("card ask: %v", err)
			}
			var got QuestionCard
			if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Class != tc.wantClass || got.Critical != tc.wantCritical {
				t.Fatalf("got class=%d critical=%v, want class=%d critical=%v", got.Class, got.Critical, tc.wantClass, tc.wantCritical)
			}
			if got.Evidence == nil || got.Evidence.IAC == nil {
				t.Fatalf("missing structured IaC evidence: %+v", got.Evidence)
			}
			rawPreview, err := os.ReadFile(tc.preview)
			if err != nil {
				t.Fatal(err)
			}
			if got.Evidence.PlanDigest != sha256Hex(rawPreview) {
				t.Errorf("plan_digest = %q, want %q", got.Evidence.PlanDigest, sha256Hex(rawPreview))
			}
			parsed, ok := parseNeedsMeQuestion(store.beads["b-iac"].Notes)
			if !ok || parsed.Hash != got.Hash || parsed.ClassN != tc.wantClass {
				t.Fatalf("mirrored note mismatch: ok=%v parsed=%+v card=%+v", ok, parsed, got)
			}
		})
	}

	// Raw JSON evidence.iac with prod delete also cannot be lowered below 8.
	store := newFakeCardStore(Bead{ID: "b-raw", Status: StatusInProgress})
	rawIn := `{
		"question": "Apply raw IaC?",
		"class": 2,
		"options": [{"label": "Apply"}, {"label": "Cancel"}],
		"evidence": {"iac": {"tool": "tofu", "target": "org/app/prod", "create": 0, "update": 0, "replace": 0, "delete": 1}}
	}`
	var out strings.Builder
	if err := cardCmd(cfg, store.env(), []string{"ask", "b-raw", "--file", "-", "--json"}, strings.NewReader(rawIn), &out); err != nil {
		t.Fatalf("raw iac ask: %v", err)
	}
	var got QuestionCard
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
		t.Fatal(err)
	}
	if got.Class < 8 || !got.Critical {
		t.Fatalf("raw prod delete was not elevated: class=%d critical=%v", got.Class, got.Critical)
	}
}

func TestQuestionCardValidationAndStaleDetection(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := defaultConfig()
	store := newFakeCardStore(
		Bead{ID: "b-1", Status: StatusInProgress},
		Bead{ID: "b-closed", Status: StatusClosed},
	)

	badCases := []struct {
		name string
		bead string
		json string
	}{
		{"unknown field", "b-1", `{"question":"Q?","class":2,"options":[{"label":"A"},{"label":"B"}],"extra":1}`},
		{"too few options", "b-1", `{"question":"Q?","class":2,"options":[{"label":"A"}]}`},
		{"too many options", "b-1", `{"question":"Q?","class":2,"options":[{"label":"1"},{"label":"2"},{"label":"3"},{"label":"4"},{"label":"5"},{"label":"6"},{"label":"7"},{"label":"8"},{"label":"9"},{"label":"10"}]}`},
		{"missing class", "b-1", `{"question":"Q?","options":[{"label":"A"},{"label":"B"}]}`},
		{"class out of range", "b-1", `{"question":"Q?","class":10,"options":[{"label":"A"},{"label":"B"}]}`},
		{"bad recommendation", "b-1", `{"question":"Q?","class":2,"recommendation":{"option":5},"options":[{"label":"A"},{"label":"B"}]}`},
		{"closed bead", "b-closed", `{"question":"Q?","class":2,"options":[{"label":"A"},{"label":"B"}]}`},
		{"path traversal bead", "../bad", `{"question":"Q?","class":2,"options":[{"label":"A"},{"label":"B"}]}`},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			if err := cardCmd(cfg, store.env(), []string{"ask", tc.bead, "--file", "-"}, strings.NewReader(tc.json), &out); err == nil {
				t.Errorf("%s unexpectedly succeeded: %s", tc.name, out.String())
			}
		})
	}

	// Write a valid card, then verify stale detection for multiple mutation cases.
	var out strings.Builder
	if err := cardCmd(cfg, store.env(), []string{"ask", "b-1", "--question", "Pick one?", "--option", "One", "--option", "Two", "--class", "3"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	card, err := readQuestionCard("b-1")
	if err != nil {
		t.Fatal(err)
	}
	if live, _ := questionCardLive(card, store.beads["b-1"]); !live {
		t.Fatal("fresh card should be live")
	}
	// Appending an unrelated trailing note supersedes the OPTIONS block.
	bTrail := store.beads["b-1"]
	bTrail.Notes += "\nactually never mind"
	if live, _ := questionCardLive(card, bTrail); live {
		t.Error("card with trailing note should be stale")
	}
	// Replacing with a different question mismatches hash.
	bReplaced := store.beads["b-1"]
	bReplaced.Notes = "QUESTION: Different?\nOPTIONS:\n1. X\n2. Y\nCLASS: 3"
	if live, why := questionCardLive(card, bReplaced); live || !strings.Contains(why, "does not match") {
		t.Errorf("replaced question: live=%v why=%q", live, why)
	}
	// Leaving needs_me makes it stale.
	bProg := store.beads["b-1"]
	bProg.Status = StatusInProgress
	if live, _ := questionCardLive(card, bProg); live {
		t.Error("in_progress bead should mark card stale")
	}
	// clearQuestionCard deletes question.json cleanly.
	if err := clearQuestionCard("b-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := readQuestionCard("b-1"); !os.IsNotExist(err) {
		t.Errorf("expected not-exist after clearQuestionCard, got %v", err)
	}
}

func TestMilestonesPersistenceAndCLIShowList(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := defaultConfig()
	store := newFakeCardStore(
		Bead{ID: "b-1", Status: StatusInProgress},
		Bead{ID: "b-2", Status: StatusInProgress},
	)

	var out strings.Builder
	if err := cardCmd(cfg, store.env(), []string{
		"milestone", "b-1",
		"--phase", "pr_opened",
		"--status", "ok",
		"--summary", "Opened PR #10",
		"--pr-url", "https://github.com/acme/app/pull/10",
		"--sha", sha,
	}, nil, &out); err != nil {
		t.Fatalf("milestone 1: %v", err)
	}
	out.Reset()
	if err := cardCmd(cfg, store.env(), []string{
		"milestone", "b-1",
		"--phase", "deploy_check",
		"--status", "warn",
		"--summary", "Canary latency elevated",
		"--run-url", "https://github.com/acme/app/actions/runs/999",
		"--link", "dashboard=https://example.invalid/dash",
	}, nil, &out); err != nil {
		t.Fatalf("milestone 2: %v", err)
	}
	if err := cardCmd(cfg, store.env(), []string{
		"ask", "b-2",
		"--question", "Proceed?",
		"--option", "Yes",
		"--option", "No",
		"--class", "4",
	}, nil, &out); err != nil {
		t.Fatalf("ask b-2: %v", err)
	}

	ms, err := readMilestones("b-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("len(milestones) = %d, want 2", len(ms))
	}
	if ms[0].Version != 1 || ms[0].Phase != "pr_opened" || ms[0].Status != "ok" || ms[0].Evidence == nil || ms[0].Evidence.PRURL != "https://github.com/acme/app/pull/10" {
		t.Errorf("milestone[0] = %+v", ms[0])
	}
	if ms[1].Phase != "deploy_check" || ms[1].Status != "warn" || len(ms[1].Evidence.Links) != 1 {
		t.Errorf("milestone[1] = %+v", ms[1])
	}
	mp, _ := milestonesPath("b-1")
	if fi, err := os.Stat(mp); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("milestones.jsonl perm = %v, err=%v", fi, err)
	}
	if notes := store.notes["b-1"]; len(notes) != 2 || notes[0] != "MILESTONE [pr_opened]: Opened PR #10" || notes[1] != "MILESTONE [deploy_check]: Canary latency elevated" {
		t.Errorf("bead notes = %q", notes)
	}

	// card show --json
	out.Reset()
	if err := cardCmd(cfg, store.env(), []string{"show", "b-1", "--json"}, nil, &out); err != nil {
		t.Fatalf("card show --json: %v", err)
	}
	var view cardView
	if err := json.Unmarshal([]byte(out.String()), &view); err != nil {
		t.Fatalf("decode show json: %v", err)
	}
	if view.Bead != "b-1" || len(view.Milestones) != 2 || view.Question != nil {
		t.Errorf("unexpected view: %+v", view)
	}

	// card list --json
	out.Reset()
	if err := cardCmd(cfg, store.env(), []string{"list", "--json"}, nil, &out); err != nil {
		t.Fatalf("card list --json: %v", err)
	}
	var list []cardView
	if err := json.Unmarshal([]byte(out.String()), &list); err != nil {
		t.Fatalf("decode list json: %v", err)
	}
	if len(list) != 2 || list[0].Bead != "b-1" || list[1].Bead != "b-2" || !list[1].Live {
		t.Errorf("unexpected list: %+v", list)
	}
}

func TestPiExtensionCardTools(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("no node")
	}
	ext, err := filepath.Abs("pi/quartermaster.ts")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	fakeQM := filepath.Join(tmp, "quartermaster")
	logFile := filepath.Join(tmp, "qm.log")
	script := `#!/bin/sh
echo "$*" >> "$FAKE_QM_LOG"
cat > "$FAKE_QM_STDIN"
case "$2" in
  ask)
    printf '{"version":1,"bead":"%s","hash":"deadbeef","class":7,"critical":false,"question":"Ship it?","options":[{"number":1,"label":"Yes"},{"number":2,"label":"No"}]}\n' "$3"
    ;;
  milestone)
    printf '{"version":1,"at":"2026-11-24T12:00:00Z","bead":"%s","phase":"pr_opened","status":"ok","summary":"PR opened"}\n' "$3"
    ;;
  *)
    echo "unexpected subcommand $*" >&2
    exit 1
    ;;
esac
`
	if err := os.WriteFile(fakeQM, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	stdinFile := filepath.Join(tmp, "qm.stdin")

	driver := filepath.Join(tmp, "driver.mjs")
	driverSrc := `
import { readFileSync } from "node:fs";
const [extPath] = process.argv.slice(2);
const { default: factory } = await import(extPath);
const tools = new Map();
const entries = [];
const pi = {
  registerFlag() {},
  registerCommand() {},
  getFlag() { return ""; },
  on() {},
  appendEntry(type, data) { entries.push({ type, data }); },
  registerTool(def) { tools.set(def.name, def); },
  events: { emit() {} },
};
factory(pi);
const registered = [...tools.keys()].sort();
if (process.env.EXPECT_NONE === "1") {
  console.log(JSON.stringify({ registered }));
  process.exit(0);
}
const askTool = tools.get("ask_user");
const msTool = tools.get("report_milestone");
const askRes = await askTool.execute("c1", {
  question: "Ship it?",
  class: 7,
  class_reason: "prod deploy",
  recommendation: { option: 1, rationale: "tests green" },
  options: [{ label: "Yes" }, { label: "No" }],
});
const askStdin = JSON.parse(readFileSync(process.env.FAKE_QM_STDIN, "utf8"));
const msRes = await msTool.execute("c2", {
  phase: "pr_opened",
  status: "ok",
  summary: "PR opened",
  evidence: { pr_url: "https://github.com/acme/app/pull/5" },
});
const msStdin = JSON.parse(readFileSync(process.env.FAKE_QM_STDIN, "utf8"));
console.log(JSON.stringify({ registered, askRes, askStdin, msRes, msStdin, entries }));
`
	if err := os.WriteFile(driver, []byte(driverSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	// Worker mode with QUARTERMASTER_BEAD registers and executes both tools.
	cmd := exec.Command("node", driver, ext)
	cmd.Env = append(os.Environ(),
		"QUARTERMASTER_BEAD=b-42",
		"QUARTERMASTER_BIN="+fakeQM,
		"FAKE_QM_LOG="+logFile,
		"FAKE_QM_STDIN="+stdinFile,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node worker driver failed: %v\n%s", err, out)
	}
	var res struct {
		Registered []string `json:"registered"`
		AskRes     struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"askRes"`
		AskStdin struct {
			Question string `json:"question"`
			Class    int    `json:"class"`
		} `json:"askStdin"`
		MsStdin struct {
			Phase   string `json:"phase"`
			Summary string `json:"summary"`
		} `json:"msStdin"`
		Entries []struct {
			Type string         `json:"type"`
			Data map[string]any `json:"data"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("decode driver output: %v\n%s", err, out)
	}
	if strings.Join(res.Registered, ",") != "ask_user,report_milestone" {
		t.Fatalf("registered tools = %v", res.Registered)
	}
	if res.AskStdin.Question != "Ship it?" || res.AskStdin.Class != 7 {
		t.Errorf("ask stdin = %+v", res.AskStdin)
	}
	if res.MsStdin.Phase != "pr_opened" || res.MsStdin.Summary != "PR opened" {
		t.Errorf("milestone stdin = %+v", res.MsStdin)
	}
	if len(res.AskRes.Content) == 0 || !strings.Contains(res.AskRes.Content[0].Text, "[deadbeef]") || !strings.Contains(res.AskRes.Content[0].Text, "Stop now") {
		t.Errorf("ask response = %+v", res.AskRes)
	}
	if len(res.Entries) != 2 || res.Entries[0].Type != "quartermaster.ask_user" || res.Entries[1].Type != "quartermaster.milestone" {
		t.Errorf("session entries = %+v", res.Entries)
	}
	logBytes, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	logStr := string(logBytes)
	if !strings.Contains(logStr, "card ask b-42 --file - --json") || !strings.Contains(logStr, "card milestone b-42 --file - --json") {
		t.Errorf("unexpected quartermaster CLI calls:\n%s", logStr)
	}

	// Coordinator mode must NOT register worker card tools.
	cmdCoord := exec.Command("node", driver, ext, "--quartermaster-coordinator", "/tmp/coord.sock")
	cmdCoord.Env = append(os.Environ(), "QUARTERMASTER_BEAD=b-42", "EXPECT_NONE=1")
	outCoord, err := cmdCoord.CombinedOutput()
	if err != nil {
		t.Fatalf("node coordinator driver failed: %v\n%s", err, outCoord)
	}
	var coordRes struct {
		Registered []string `json:"registered"`
	}
	if err := json.Unmarshal(outCoord, &coordRes); err != nil {
		t.Fatalf("decode coord output: %v\n%s", err, outCoord)
	}
	if len(coordRes.Registered) != 0 {
		t.Errorf("coordinator should not register worker card tools, got %v", coordRes.Registered)
	}
}
