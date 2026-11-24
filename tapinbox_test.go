package main

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func inboxRequest(s *tapServer, method, path, body, remoteAddr string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, r)
	return rec
}

func newTestInboxServer(t *testing.T, cfg Config, penv promptEnv, ienv *tapInboxEnv, device string) *tapServer {
	t.Helper()
	if cfg.Tap.Devices == nil {
		cfg.Tap.Devices = []string{"iphone181"}
	}
	cfg.Tap.Enabled = true
	s, err := newTapServerWithConfig(cfg, penv, ienv)
	if err != nil {
		t.Fatal(err)
	}
	s.base = "http://100.78.70.17:8642"
	s.whois = func(string) (tapIdentity, error) {
		return tapIdentity{Device: device, User: "tj@example.com"}, nil
	}
	return s
}

func writeCheckedPlanFixture(t *testing.T, cfg Config, b Bead, p Plan, extraEvents []planEvent, now time.Time) string {
	t.Helper()
	digest, err := p.digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := storePlan(p, digest); err != nil {
		t.Fatal(err)
	}
	show := func(string) (Bead, error) { return b, nil }
	class, _ := classifyPlan(cfg, show, p)
	if len(extraEvents) == 0 {
		if err := appendPlanEvent(digest, planEvent{
			At:    now,
			Kind:  planChecked,
			Class: class,
			Level: 4,
			Pass:  false,
			Why:   "class above bead level",
		}); err != nil {
			t.Fatal(err)
		}
		return digest
	}
	for _, ev := range extraEvents {
		if ev.Class == 0 {
			ev.Class = class
		}
		if err := appendPlanEvent(digest, ev); err != nil {
			t.Fatal(err)
		}
	}
	return digest
}

func writePulumiPreviewFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "preview.json")
	content := `{
  "steps": [
    {"op": "create", "urn": "urn:pulumi:prod::app::aws:ec2/securityGroup:SecurityGroup::sg-new"},
    {"op": "update", "urn": "urn:pulumi:prod::app::aws:iam/role:Role::app-role"},
    {"op": "delete", "urn": "urn:pulumi:prod::app::aws:s3/bucket:Bucket::old-logs"}
  ]
}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeCardFile(t *testing.T, bead, name, content string) {
	t.Helper()
	dir := tapBeadCardDir(bead)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func extractHiddenToken(t *testing.T, html string) string {
	t.Helper()
	re := regexp.MustCompile(`name="token"\s+value="([^"]+)"`)
	m := re.FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("no hidden token found in confirm HTML:\n%s", html)
	}
	return m[1]
}

func TestTapInboxFailClosedAuth(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := time.Now().UTC().Truncate(time.Second)

	cfg := defaultConfig()
	cfg.Tap = TapConfig{Enabled: true, Devices: []string{"iphone181"}}
	planBead := Bead{ID: "task-plan", Status: StatusNeedsMe, Title: "Deploy stack"}
	p := Plan{
		Bead: planBead.ID,
		Steps: []Step{{
			Op:     opOpaque,
			Text:   "pulumi up --stack prod",
			Target: "pulumi:prod",
		}},
	}
	digest := writeCheckedPlanFixture(t, cfg, planBead, p, nil, now)

	routes := []struct {
		method string
		path   string
		isJSON bool
	}{
		{http.MethodGet, "/", false},
		{http.MethodGet, "/inbox", false},
		{http.MethodGet, "/inbox.json", true},
		{http.MethodGet, "/inbox/plan/" + digest, false},
		{http.MethodPost, "/inbox/plan/" + digest, false},
		{http.MethodGet, "/unknown-path", false},
	}

	t.Run("non-tailnet IP refused without calling whois", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		digest := writeCheckedPlanFixture(t, cfg, planBead, p, nil, now)
		whoisCalled := false
		s := newTestInboxServer(t, cfg, promptEnv{}, &tapInboxEnv{}, "iphone181")
		s.whois = func(string) (tapIdentity, error) {
			whoisCalled = true
			return tapIdentity{Device: "iphone181"}, nil
		}
		for _, rt := range routes {
			path := strings.ReplaceAll(rt.path, digest, digest)
			for _, addr := range []string{"127.0.0.1:49152", "192.168.1.25:49152", "bad-addr"} {
				rec := inboxRequest(s, rt.method, path, "", addr)
				if rec.Code != http.StatusForbidden {
					t.Errorf("%s %s from %s = %d, want 403", rt.method, path, addr, rec.Code)
				}
				if rt.isJSON {
					var errBody struct {
						Error string `json:"error"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil || errBody.Error == "" {
						t.Errorf("%s %s from %s returned non-JSON error %q", rt.method, path, addr, rec.Body.String())
					}
				}
			}
		}
		if whoisCalled {
			t.Error("whois was called for non-tailnet addresses")
		}
		recs, err := readJournal(time.Time{})
		if err != nil || len(recs) < len(routes)*3 {
			t.Fatalf("expected TAP REFUSED journal records, got %d (err %v)", len(recs), err)
		}
		for _, r := range recs {
			if r.Kind != journalTap || !strings.HasPrefix(r.Text, "TAP REFUSED:") {
				t.Errorf("unexpected journal record: %+v", r)
			}
		}
	})

	t.Run("whois error refused and journaled", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		digest := writeCheckedPlanFixture(t, cfg, planBead, p, nil, now)
		s := newTestInboxServer(t, cfg, promptEnv{}, &tapInboxEnv{}, "iphone181")
		s.whois = func(string) (tapIdentity, error) { return tapIdentity{}, errors.New("tailscaled down") }
		for _, rt := range routes {
			path := rt.path
			if strings.HasPrefix(path, "/inbox/plan/") {
				path = "/inbox/plan/" + digest
			}
			rec := inboxRequest(s, rt.method, path, "", tapPhoneAddr)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s with whois error = %d, want 403", rt.method, path, rec.Code)
			}
		}
		recs, _ := readJournal(time.Time{})
		if len(recs) != len(routes) {
			t.Fatalf("expected %d TAP REFUSED records, got %d", len(routes), len(recs))
		}
	})

	t.Run("disallowed device including local host refused and journaled", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		digest := writeCheckedPlanFixture(t, cfg, planBead, p, nil, now)
		s := newTestInboxServer(t, cfg, promptEnv{}, &tapInboxEnv{}, "desktop")
		for _, rt := range routes {
			path := rt.path
			if strings.HasPrefix(path, "/inbox/plan/") {
				path = "/inbox/plan/" + digest
			}
			rec := inboxRequest(s, rt.method, path, "", "100.78.70.17:49152")
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "device desktop is not allowed") {
				t.Errorf("%s %s from desktop = %d %q, want 403", rt.method, path, rec.Code, rec.Body.String())
			}
		}
		recs, _ := readJournal(time.Time{})
		if len(recs) != len(routes) {
			t.Fatalf("expected %d TAP REFUSED records, got %d", len(routes), len(recs))
		}
	})

	t.Run("root redirects to /inbox only when authenticated and rejects POST", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		s := newTestInboxServer(t, cfg, promptEnv{}, &tapInboxEnv{}, "iphone181")
		rec := inboxRequest(s, http.MethodGet, "/", "", tapPhoneAddr)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/inbox" {
			t.Fatalf("GET / = %d Location=%q, want 302 /inbox", rec.Code, rec.Header().Get("Location"))
		}
		for _, p := range []string{"/", "/inbox", "/inbox.json"} {
			rec := inboxRequest(s, http.MethodPost, p, "", tapPhoneAddr)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("POST %s = %d, want 405", p, rec.Code)
			}
		}
		rec = inboxRequest(s, http.MethodGet, "/not-a-route", "", tapPhoneAddr)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET /not-a-route = %d, want 404", rec.Code)
		}
	})
}

func TestTapInboxRendersAllFourSectionsHTMLAndJSON(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("QUARTERMASTER_STATE_DIR", tmp)
	now := time.Now().UTC().Truncate(time.Second)

	bashScreen := screen(t, "claude-bash-4.txt")
	bashPrompt, ok := parsePrompt(bashScreen)
	if !ok {
		t.Fatal("failed to parse bash fixture")
	}

	// Structured question bead with matching question.json + milestones.jsonl.
	structuredNotes := `QUESTION: Roll out the production database index now?
OPTIONS:
1. Wait for maintenance window
2. Roll out canary shard first
CLASS: 7 — production rollout`
	structuredQ, ok := parseNeedsMeQuestion(structuredNotes)
	if !ok {
		t.Fatal("structured question did not parse")
	}
	class7 := 7
	optClass4 := 4
	cardJSON, err := json.Marshal(tapQuestionCard{
		Version:     1,
		Bead:        "task-2",
		CreatedAt:   now.Format(time.RFC3339),
		Hash:        structuredQ.Hash,
		Question:    "Roll out the production index now?",
		Details:     "Staging canary showed zero lock contention across 12M rows.",
		Critical:    false,
		Class:       &class7,
		ClassReason: "production rollout",
		Recommendation: &tapCardRecommendation{
			Option:    2,
			Rationale: "Canary shard isolates blast radius before full rollout.",
		},
		Options: []tapCardOption{
			{Number: 1, Label: "Wait for maintenance window", Description: "Defers rollout to Sunday 02:00 UTC", Class: &optClass4},
			{Number: 2, Label: "Roll out canary shard first", Description: "Applies to shard-0 with live metrics check", Class: &class7},
		},
		Evidence: &tapCardEvidence{
			PRURL:        "https://github.com/example/repo/pull/101",
			CostEstimate: "$25/month",
			RunURLs:      []string{"https://github.com/example/repo/actions/runs/999"},
			IaC: &tapIaCEvidence{
				Tool:    "pulumi",
				Target:  "prod-db",
				Create:  1,
				Update:  2,
				Replace: 0,
				Delete:  0,
				Summary: "1 create, 2 update, 0 destroy",
				Excerpt: "+ aws:rds:ParameterGroup canary-pg\n~ aws:rds:Cluster shard-0",
			},
			Links: []tapCardLink{
				{Label: "Dashboard", URL: "https://metrics.example.com/db"},
				{Label: "Blocked XSS", URL: "javascript:alert(1)"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeCardFile(t, "task-2", "question.json", string(cardJSON))
	writeCardFile(t, "task-2", "milestones.jsonl", strings.Join([]string{
		`{"version":1,"at":"2026-11-24T00:10:00Z","bead":"task-2","phase":"staging","status":"ok","summary":"Staging migration passed"}`,
		`not valid json line that must be skipped`,
		`{"version":1,"at":"2026-11-24T00:25:00Z","bead":"task-2","phase":"prod-gate","status":"blocked","summary":"Awaiting rollout choice"}`,
	}, "\n"))

	// Plain OPTIONS question with CRITICAL + detected gate counts and plan digest
	// (evidence sits before OPTIONS:, matching parseNeedsMeQuestion's tail rule).
	optionsNotesHeader := `preview: create=0,update=1,delete=1`
	optionsNotesBody := `CRITICAL: deleting legacy bucket
QUESTION: Delete the unused staging archive bucket? Recommendation: 2 — keep 7 days first
OPTIONS:
1. Delete immediately
2. Keep for 7 more days
CLASS: 8 — one prod delete`

	// RUN: command bead (RUN: sits within the last two lines per pendingCommand).
	runNotes := "CLASS: 6 — non-prod auth\nRUN: aws sso login --profile dev-admin\nSSO token expired while previewing stack"

	// Free-text needs_me bead (with malformed question.json to verify graceful fallback).
	freeNotes := "Should we rename the CLI subcommand before tagging v1.2?"
	writeCardFile(t, "task-5", "question.json", "{malformed json")

	// Ready-to-merge bead.
	readySHA := "0123456789abcdef0123456789abcdef01234567"
	readyPR := PR{
		Number:         42,
		Title:          "Add Tailnet Web Inbox",
		URL:            "https://github.com/example/repo/pull/42",
		Head:           "tj/task-6-web-inbox",
		State:          "OPEN",
		ReviewDecision: "APPROVED",
		HeadSHA:        readySHA,
		MergeState:     "CLEAN",
		DetailsFresh:   true,
		Checks:         []Check{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}},
		Gate: &MergeGate{
			Head:   readySHA,
			Viewer: "author",
			Author: "author",
			At:     now,
			Reviews: []Review{{
				State:       "APPROVED",
				SubmittedAt: now.Add(-10 * time.Minute),
				CanPush:     true,
				Author: struct {
					Login string `json:"login"`
				}{Login: "reviewer"},
				Commit: struct {
					OID string `json:"oid"`
				}{OID: readySHA},
			}},
		},
	}

	// Non-ready PR (must NOT appear in ready_to_merge).
	unreadyPR := PR{
		Number:         43,
		Title:          "WIP refactor",
		URL:            "https://github.com/example/repo/pull/43",
		Head:           "tj/task-7-wip",
		State:          "OPEN",
		ReviewDecision: "REVIEW_REQUIRED",
		HeadSHA:        "fedcba9876543210fedcba9876543210fedcba98",
		MergeState:     "BLOCKED",
		DetailsFresh:   true,
	}

	// Pending checked plans + non-pending plans to verify filtering.
	cfg := defaultConfig()
	cfg.Autonomy.Level = 4
	cfg.Autonomy.Max = 7
	cfg.Autonomy.Prod = []string{"*prod*"}
	cfg.Tap = TapConfig{Enabled: true, Devices: []string{"iphone181"}}

	previewPath := writePulumiPreviewFile(t, tmp)
	planBead := Bead{ID: "task-8", Status: StatusNeedsMe, Title: "Apply prod network change", Notes: "EXEC: stopped for TJ"}
	iacPlan := Plan{
		Bead: planBead.ID,
		Steps: []Step{{
			Op:     "iac.apply",
			Target: "pulumi:org/net/prod",
			Args:   map[string]string{"preview": previewPath, "expect": "create=1,update=1,delete=1"},
		}},
	}
	pendingDigest := writeCheckedPlanFixture(t, cfg, planBead, iacPlan, nil, now)

	// Stopped needs_me plan (must appear).
	stoppedPlan := Plan{
		Bead:  "task-9",
		Steps: []Step{{Op: opOpaque, Text: "tofu apply -auto-approve", Target: "tofu:prod"}},
	}
	stoppedDigest := writeCheckedPlanFixture(t, cfg, Bead{ID: "task-9", Status: StatusNeedsMe}, stoppedPlan, []planEvent{
		{At: now.Add(-10 * time.Minute), Kind: planChecked, Class: 9, Level: 4, Pass: true},
		{At: now.Add(-5 * time.Minute), Kind: planNeedsMe, Class: 9, Why: "probe could not confirm"},
	}, now)

	// Done plan (must NOT appear).
	donePlan := Plan{
		Bead:  "task-done",
		Steps: []Step{{Op: opOpaque, Text: "echo done"}},
	}
	_ = writeCheckedPlanFixture(t, cfg, Bead{ID: "task-done", Status: StatusInProgress}, donePlan, []planEvent{
		{At: now.Add(-10 * time.Minute), Kind: planChecked, Class: 9, Pass: true},
		{At: now.Add(-5 * time.Minute), Kind: planDone, Class: 9},
	}, now)

	// Closed bead plan (must NOT appear).
	closedBead := Bead{ID: "task-closed", Status: StatusClosed, Title: "Already closed"}
	closedPlan := Plan{
		Bead:  closedBead.ID,
		Steps: []Step{{Op: opOpaque, Text: "echo old"}},
	}
	_ = writeCheckedPlanFixture(t, cfg, closedBead, closedPlan, nil, now)

	beads := []Bead{
		{ID: "task-1", Status: StatusInProgress, Title: "Run setup command"},
		{ID: "task-2", Status: StatusNeedsMe, Title: "Prod index rollout", Notes: structuredNotes},
		{ID: "task-3", Status: StatusNeedsMe, Title: "Bucket cleanup", Notes: optionsNotesHeader + "\nplan approve " + pendingDigest + "\n" + optionsNotesBody},
		{ID: "task-4", Status: StatusNeedsMe, Title: "Refresh AWS SSO", Notes: runNotes},
		{ID: "task-5", Status: StatusNeedsMe, Title: "CLI naming check", Notes: freeNotes},
		{ID: "task-6", Status: StatusInProgress, Title: "Ship Web Inbox"},
		{ID: "task-7", Status: StatusInProgress, Title: "WIP refactor"},
		planBead,
	}
	byID := map[string]Bead{closedBead.ID: closedBead}
	for _, b := range beads {
		byID[b.ID] = b
	}

	var sentKeys [][]string
	penv := promptEnv{
		show: func(id string) (Bead, error) {
			if b, ok := byID[id]; ok {
				return b, nil
			}
			return Bead{}, errors.New("not found")
		},
		agent: func(_, name string) (*Agent, error) {
			if name == "task-1" {
				return &Agent{Name: "task-1", Kind: "claude", Status: "blocked", PaneID: "w1:p1"}, nil
			}
			return nil, nil
		},
		read: func(_, pane string) (string, error) {
			if pane == "w1:p1" {
				return bashScreen, nil
			}
			return "", nil
		},
		keys: func(_, _ string, keys ...string) error {
			sentKeys = append(sentKeys, keys)
			return nil
		},
		note:      func(string, string) error { return nil },
		setStatus: func(string, string) error { return nil },
		sleep:     func(time.Duration) {},
		level:     func(string) (int, string) { return 4, "" },
	}
	ienv := &tapInboxEnv{
		active: func() ([]Bead, error) { return beads, nil },
		agents: func() ([]Agent, error) {
			return []Agent{
				{Name: "task-1", Kind: "claude", Status: "blocked", PaneID: "w1:p1"},
				{Name: "task-2", Kind: "pi", Status: "idle", PaneID: "w2:p1"},
				{Name: "task-6", Kind: "pi", Status: "idle", PaneID: "w6:p1"},
			}, nil
		},
		loadState: func() TickerState {
			return TickerState{
				PRs: map[string]PR{
					"task-6": readyPR,
					"task-7": unreadyPR,
				},
			}
		},
	}

	s := newTestInboxServer(t, cfg, penv, ienv, "iphone181")
	s.now = func() time.Time { return now }

	// 1. Test GET /inbox.json structurally.
	jsonRec := inboxRequest(s, http.MethodGet, "/inbox.json", "", tapPhoneAddr)
	if jsonRec.Code != http.StatusOK {
		t.Fatalf("GET /inbox.json = %d %q", jsonRec.Code, jsonRec.Body.String())
	}
	if ct := jsonRec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	rawJSON := jsonRec.Body.String()
	// Secret must never appear in any encoding.
	for name, enc := range map[string]string{
		"raw":    string(s.secret),
		"hex":    hex.EncodeToString(s.secret),
		"base64": base64.StdEncoding.EncodeToString(s.secret),
		"rawurl": base64.RawURLEncoding.EncodeToString(s.secret),
	} {
		if enc != "" && strings.Contains(rawJSON, enc) {
			t.Errorf("/inbox.json leaked tap secret (%s)", name)
		}
	}

	var snap inboxSnapshot
	if err := json.Unmarshal(jsonRec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("invalid JSON from /inbox.json: %v\n%s", err, rawJSON)
	}
	if snap.Viewer.As != "TJ" || snap.Viewer.Device != "iphone181" {
		t.Errorf("viewer = %+v", snap.Viewer)
	}
	if snap.Counts.Prompts != 1 || snap.Counts.Questions != 5 || snap.Counts.Plans != 2 || snap.Counts.ReadyToMerge != 1 {
		t.Fatalf("counts = %+v (questions=%d, plans=%d)", snap.Counts, len(snap.Questions), len(snap.Plans))
	}

	// Verify Section 1: Approval prompt includes actual command lines and valid tap tokens.
	prItem := snap.Prompts[0]
	if prItem.Bead != "task-1" || prItem.Hash != bashPrompt.Hash || len(prItem.Options) != len(bashPrompt.Options) {
		t.Fatalf("prompt item = %+v", prItem)
	}
	if !strings.Contains(strings.Join(prItem.Lines, "\n"), "mkdir -p out2 && touch out2/hello.txt") {
		t.Errorf("prompt lines missing actual command: %q", prItem.Lines)
	}
	if tok := strings.TrimPrefix(prItem.Options[0].Path, "/tap/"); tok == prItem.Options[0].Path {
		t.Errorf("prompt option path = %q", prItem.Options[0].Path)
	} else if c, err := parseTapToken(s.secret, tok, now); err != nil || c.Kind != tapKindPrompt || c.Bead != "task-1" || c.Hash != bashPrompt.Hash {
		t.Errorf("minted prompt token invalid: claims=%+v err=%v", c, err)
	}

	// Verify Section 2: questions keep question hash out of JSON payload (two hash worlds clean).
	var rawMap map[string]any
	if err := json.Unmarshal(jsonRec.Body.Bytes(), &rawMap); err != nil {
		t.Fatal(err)
	}
	for _, rawQ := range rawMap["questions"].([]any) {
		qm := rawQ.(map[string]any)
		if _, hasHash := qm["hash"]; hasHash {
			t.Errorf("question JSON exposed raw hash field: %+v", qm)
		}
	}

	// Verify questions sorted critical first, then highest class descending.
	qByBead := map[string]inboxQuestion{}
	for _, q := range snap.Questions {
		qByBead[q.Bead] = q
	}
	if snap.Questions[0].Bead != "task-3" || !snap.Questions[0].Critical {
		t.Errorf("expected CRITICAL task-3 first, got %+v", snap.Questions[0])
	}
	if q2 := qByBead["task-2"]; q2.Kind != "structured" || !q2.Tappable || q2.FreeText == nil || q2.FreeText.MaxChars != tapMaxFreeTextRunes || q2.Class == nil || *q2.Class != 7 ||
		q2.Recommendation == nil || q2.Recommendation.Option != 2 ||
		q2.Evidence == nil || q2.Evidence.IaC == nil || q2.Evidence.IaC.Create != 1 || q2.Evidence.IaC.Update != 2 ||
		q2.Evidence.CostEstimate != "$25/month" || len(q2.Milestones) != 2 {
		t.Fatalf("structured question task-2 = %+v", q2)
	} else {
		// Malicious javascript: link must be stripped.
		if len(q2.Evidence.Links) != 1 || q2.Evidence.Links[0].Label != "Dashboard" {
			t.Errorf("expected unsafe link stripped, got %+v", q2.Evidence.Links)
		}
	}
	if q3 := qByBead["task-3"]; q3.Kind != "options" || !q3.Tappable || q3.FreeText == nil || q3.FreeText.MaxChars != tapMaxFreeTextRunes || q3.Class == nil || *q3.Class != 8 ||
		q3.Evidence == nil || q3.Evidence.PlanDigest != pendingDigest || q3.Evidence.PlanApprovePath != "/inbox/plan/"+pendingDigest ||
		q3.Evidence.IaC == nil || q3.Evidence.IaC.Delete != 1 {
		t.Fatalf("options question task-3 = %+v", q3)
	}
	if q4 := qByBead["task-4"]; q4.Kind != "run" || q4.Tappable || q4.FreeText != nil || q4.RunCommand != "aws sso login --profile dev-admin" {
		t.Fatalf("run question task-4 = %+v", q4)
	}
	if q5 := qByBead["task-5"]; q5.Kind != "free_text" || !q5.Tappable || q5.FreeText == nil || q5.FreeText.MaxChars != tapMaxFreeTextRunes || !strings.HasPrefix(q5.FreeText.Path, "/tap/") || !strings.Contains(q5.Question, "rename the CLI subcommand") {
		t.Fatalf("free_text task-5 = %+v", q5)
	}

	// Verify Section 3: Pending plans (iacPlan + stoppedPlan; donePlan and closedPlan excluded).
	planByDigest := map[string]inboxPlan{}
	for _, pl := range snap.Plans {
		planByDigest[pl.Digest] = pl
	}
	if iacItem, ok := planByDigest[pendingDigest]; !ok || iacItem.Class != 8 || len(iacItem.Steps) != 1 || iacItem.Steps[0].IaC == nil || iacItem.Steps[0].IaC.Delete != 1 || len(iacItem.Steps[0].IaC.Blocked) != 1 {
		t.Fatalf("pending iac plan = %+v (ok=%v)", iacItem, ok)
	}
	if stItem, ok := planByDigest[stoppedDigest]; !ok || stItem.Status != planNeedsMe || stItem.Class != 9 {
		t.Fatalf("stopped plan = %+v (ok=%v)", stItem, ok)
	}

	// Verify Section 4: Ready-to-merge PRs (task-6 only).
	if len(snap.ReadyToMerge) != 1 || snap.ReadyToMerge[0].Bead != "task-6" || snap.ReadyToMerge[0].PRNumber != 42 || snap.ReadyToMerge[0].ShortSHA != short(readySHA) || snap.ReadyToMerge[0].MergeClass != 4 {
		t.Fatalf("ready_to_merge = %+v", snap.ReadyToMerge)
	}

	// 2. Test GET /inbox HTML rendering.
	htmlRec := inboxRequest(s, http.MethodGet, "/inbox", "", tapPhoneAddr)
	if htmlRec.Code != http.StatusOK {
		t.Fatalf("GET /inbox = %d %q", htmlRec.Code, htmlRec.Body.String())
	}
	html := htmlRec.Body.String()
	for _, want := range []string{
		"Quartermaster Inbox",
		"Viewing as TJ from iphone181",
		"mkdir -p out2 &amp;&amp; touch out2/hello.txt",
		"Roll out the production index now?",
		"Recommendation: Option 2",
		"+1 create",
		"~2 update",
		"$25/month",
		"Staging migration passed",
		"CRITICAL",
		"C8 — one prod delete",
		"aws sso login --profile dev-admin",
		"rename the CLI subcommand",
		"Answer with free text →",
		"Answer with free text…",
		"/inbox/plan/" + pendingDigest,
		"PR #42: Add Tailnet Web Inbox",
		"https://github.com/example/repo/pull/42",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("/inbox HTML missing %q", want)
		}
	}
	// Zero external script/stylesheet dependencies and zero raw pointer formatting.
	for _, forbidden := range []string{"<script src=", `<link rel="stylesheet"`, "javascript:alert", "0xc000"} {
		if strings.Contains(html, forbidden) {
			t.Errorf("/inbox HTML contains forbidden substring %q", forbidden)
		}
	}
	// GET must never mutate anything.
	if len(sentKeys) != 0 {
		t.Fatalf("GET /inbox sent keys: %v", sentKeys)
	}
	if _, err := os.Stat(tapUsedPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("GET /inbox touched used nonces file: err=%v", err)
	}
}

func TestTapInboxStructuredCardStaleAndFallback(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := time.Now().UTC().Truncate(time.Second)

	notes := `QUESTION: Deploy to eu-west?
OPTIONS:
1. Yes
2. No
CLASS: 6 — non-prod deploy`
	q, ok := parseNeedsMeQuestion(notes)
	if !ok {
		t.Fatal("question did not parse")
	}
	// Stale question.json with an outdated hash must be ignored in favor of live bd notes.
	writeCardFile(t, "task-stale", "question.json", `{"version":1,"bead":"task-stale","hash":"deadbeef","question":"Old stale question"}`)
	// Already-answered card on a bead now holding a free-text note must also fall back.
	answeredNotes := "NEEDS_ME ANSWERED: 1 Yes to Old [11223344] by TJ via tap (device iphone181)\nCan you confirm the rollout window?"
	writeCardFile(t, "task-ans", "question.json", `{"version":1,"bead":"task-ans","hash":"11223344","question":"Old answered question"}`)
	// Fresh card with no OPTIONS note renders read-only structured card (no broken tap links).
	writeCardFile(t, "task-ro", "question.json", `{"version":1,"bead":"task-ro","question":"Inspect staging metrics","class":6,"options":[{"number":1,"label":"Continue"},{"number":2,"label":"Rollback"}]}`)

	cfg := defaultConfig()
	cfg.Tap = TapConfig{Enabled: true, Devices: []string{"iphone181"}}
	beads := []Bead{
		{ID: "task-stale", Status: StatusNeedsMe, Title: "Stale card test", Notes: notes},
		{ID: "task-ans", Status: StatusNeedsMe, Title: "Answered card test", Notes: answeredNotes},
		{ID: "task-ro", Status: StatusNeedsMe, Title: "Read-only card test", Notes: "Waiting on review"},
	}
	s := newTestInboxServer(t, cfg, promptEnv{}, &tapInboxEnv{
		active: func() ([]Bead, error) { return beads, nil },
	}, "iphone181")
	s.now = func() time.Time { return now }

	rec := inboxRequest(s, http.MethodGet, "/inbox.json", "", tapPhoneAddr)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /inbox.json = %d %q", rec.Code, rec.Body.String())
	}
	var snap inboxSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	byBead := map[string]inboxQuestion{}
	for _, item := range snap.Questions {
		byBead[item.Bead] = item
	}
	if st := byBead["task-stale"]; st.Kind != "options" || !st.Tappable || !strings.Contains(st.Question, "Deploy to eu-west?") {
		t.Errorf("stale card should fall back to live OPTIONS note: %+v", st)
	}
	// Verify minted link still carries live q.Hash.
	tok := strings.TrimPrefix(byBead["task-stale"].Options[0].Path, "/tap/")
	if c, err := parseTapToken(s.secret, tok, now); err != nil || c.Hash != q.Hash {
		t.Errorf("fallback token hash = %q, want %q (err=%v)", c.Hash, q.Hash, err)
	}
	if ans := byBead["task-ans"]; ans.Kind != "free_text" || !strings.Contains(ans.Question, "confirm the rollout window") {
		t.Errorf("already-answered card should fall back to free text: %+v", ans)
	}
	if ro := byBead["task-ro"]; ro.Kind != "structured" || ro.Tappable || len(ro.Options) != 2 || ro.Options[0].Path != "" {
		t.Errorf("card without OPTIONS note should be read-only structured: %+v", ro)
	}
}

func TestTapInboxPlanConfirmAndPostApproval(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("QUARTERMASTER_STATE_DIR", tmp)
	now := time.Now().UTC().Truncate(time.Second)

	cfg := defaultConfig()
	cfg.Autonomy.Level = 4
	cfg.Autonomy.Prod = []string{"*prod*"}
	cfg.Tap = TapConfig{Enabled: true, Devices: []string{"iphone181", "ipad"}}

	b := Bead{ID: "task-apply", Status: StatusNeedsMe, Title: "Apply production change"}
	previewPath := writePulumiPreviewFile(t, tmp)
	p := Plan{
		Bead: b.ID,
		Steps: []Step{{
			Op:     "iac.apply",
			Target: "pulumi:org/app/prod",
			Args:   map[string]string{"preview": previewPath, "expect": "create=1,update=1,delete=1"},
		}},
	}
	digest := writeCheckedPlanFixture(t, cfg, b, p, []planEvent{
		{At: now.Add(-5 * time.Minute), Kind: planChecked, Class: 8, Level: 4, Pass: false, Why: "class 8 above level 4"},
		{At: now.Add(-2 * time.Minute), Kind: planNeedsMe, Class: 8, Why: "waiting for TJ"},
	}, now)

	otherPlan := Plan{Bead: "task-other", Steps: []Step{{Op: opOpaque, Text: "echo other"}}}
	otherDigest := writeCheckedPlanFixture(t, cfg, Bead{ID: "task-other", Status: StatusNeedsMe}, otherPlan, nil, now)

	penv := promptEnv{
		show: func(id string) (Bead, error) {
			if id == b.ID {
				return b, nil
			}
			return Bead{ID: id, Status: StatusNeedsMe}, nil
		},
	}
	s := newTestInboxServer(t, cfg, penv, &tapInboxEnv{
		active: func() ([]Bead, error) { return []Bead{b}, nil },
	}, "iphone181")
	curNow := now
	s.now = func() time.Time { return curNow }

	// 1. GET confirm page mutates nothing and returns a signed token.
	getRec := inboxRequest(s, http.MethodGet, "/inbox/plan/"+digest, "", tapPhoneAddr)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET confirm = %d %q", getRec.Code, getRec.Body.String())
	}
	confirmHTML := getRec.Body.String()
	for _, want := range []string{digest, "iac.apply", "pulumi:org/app/prod", "-1 delete", "Approve plan " + short(digest) + " as TJ"} {
		if !strings.Contains(confirmHTML, want) {
			t.Errorf("confirm page missing %q", want)
		}
	}
	eventsBefore, _ := readPlanEvents(digest)
	if _, approved := liveApproval(eventsBefore, curNow); approved {
		t.Fatal("GET /inbox/plan/<digest> approved the plan")
	}
	token := extractHiddenToken(t, confirmHTML)

	// 2. Bad signature refused (400), spends nothing.
	badSigRec := inboxRequest(s, http.MethodPost, "/inbox/plan/"+digest, url.Values{"token": {token + "tamper"}}.Encode(), tapPhoneAddr)
	if badSigRec.Code != http.StatusBadRequest {
		t.Fatalf("bad signature POST = %d %q, want 400", badSigRec.Code, badSigRec.Body.String())
	}

	// 3. Mismatched digest refused (400), spends nothing.
	wrongPlanRec := inboxRequest(s, http.MethodPost, "/inbox/plan/"+otherDigest, url.Values{"token": {token}}.Encode(), tapPhoneAddr)
	if wrongPlanRec.Code != http.StatusBadRequest {
		t.Fatalf("wrong digest POST = %d %q, want 400", wrongPlanRec.Code, wrongPlanRec.Body.String())
	}

	// 4. Different allowed device refused (403) because token is bound to iphone181, spends nothing.
	s.whois = func(string) (tapIdentity, error) { return tapIdentity{Device: "ipad"}, nil }
	wrongDevRec := inboxRequest(s, http.MethodPost, "/inbox/plan/"+digest, url.Values{"token": {token}}.Encode(), tapPhoneAddr)
	if wrongDevRec.Code != http.StatusForbidden || !strings.Contains(wrongDevRec.Body.String(), "minted for device iphone181") {
		t.Fatalf("wrong bound device POST = %d %q, want 403", wrongDevRec.Code, wrongDevRec.Body.String())
	}
	s.whois = func(string) (tapIdentity, error) { return tapIdentity{Device: "iphone181"}, nil }

	// 5. Expired token refused (410), spends nothing.
	curNow = now.Add(tapTTL + time.Second)
	expRec := inboxRequest(s, http.MethodPost, "/inbox/plan/"+digest, url.Values{"token": {token}}.Encode(), tapPhoneAddr)
	if expRec.Code != http.StatusGone {
		t.Fatalf("expired token POST = %d %q, want 410", expRec.Code, expRec.Body.String())
	}
	curNow = now

	// 6. Valid POST succeeds: appends TJ approval with device identity and journals it.
	postRec := inboxRequest(s, http.MethodPost, "/inbox/plan/"+digest, url.Values{"token": {token}}.Encode(), tapPhoneAddr)
	if postRec.Code != http.StatusOK || !strings.Contains(postRec.Body.String(), "approved by TJ via tap (device iphone181)") || !strings.Contains(postRec.Body.String(), "quartermaster plan resume "+digest) {
		t.Fatalf("valid POST = %d %q", postRec.Code, postRec.Body.String())
	}
	eventsAfter, err := readPlanEvents(digest)
	if err != nil {
		t.Fatal(err)
	}
	appr, ok := liveApproval(eventsAfter, curNow)
	if !ok || appr.By != "TJ" || appr.Evidence != "via tap (device iphone181)" {
		t.Fatalf("plan approval event = %+v (ok=%v)", appr, ok)
	}
	recs, err := readJournal(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	foundApproval := false
	for _, r := range recs {
		if r.Kind == journalCheck && r.Action == "plan" && r.Pass && r.Bead == b.ID && strings.Contains(r.Text, "approved by TJ via tap (device iphone181)") {
			foundApproval = true
		}
	}
	if !foundApproval {
		t.Fatalf("approval not journaled with device identity: %+v", recs)
	}

	// 7. Replay of spent token refused (409) and does not append a second approval.
	replayRec := inboxRequest(s, http.MethodPost, "/inbox/plan/"+digest, url.Values{"token": {token}}.Encode(), tapPhoneAddr)
	if replayRec.Code != http.StatusConflict || !strings.Contains(replayRec.Body.String(), "already used") {
		t.Fatalf("replay POST = %d %q, want 409", replayRec.Code, replayRec.Body.String())
	}
	eventsAfterReplay, _ := readPlanEvents(digest)
	if len(eventsAfterReplay) != len(eventsAfter) {
		t.Fatalf("replay appended extra plan events: %d vs %d", len(eventsAfterReplay), len(eventsAfter))
	}

	// 8. Once approved, plan no longer appears in /inbox.json pending plans.
	afterJSON := inboxRequest(s, http.MethodGet, "/inbox.json", "", tapPhoneAddr)
	var snap inboxSnapshot
	if err := json.Unmarshal(afterJSON.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	for _, pl := range snap.Plans {
		if pl.Digest == digest {
			t.Errorf("approved plan %s still listed as pending", short(digest))
		}
	}
}

func TestTapURLCommand(t *testing.T) {
	testTapOwnIP(t)

	// Off by default: fails closed.
	if _, err := tapInboxURL(Config{}); err == nil || !strings.Contains(err.Error(), "taps are off") {
		t.Errorf("tapInboxURL while off = %v", err)
	}
	// Enabled without device allowlist: fails closed.
	if _, err := tapInboxURL(Config{Tap: TapConfig{Enabled: true}}); err == nil || !strings.Contains(err.Error(), "devices") {
		t.Errorf("tapInboxURL without devices = %v", err)
	}
	// Enabled with default port.
	got, err := tapInboxURL(Config{Tap: TapConfig{Enabled: true, Devices: []string{"iphone181"}}})
	if err != nil || got != "http://100.78.70.17:8642/inbox" {
		t.Fatalf("tapInboxURL default port = %q, err %v", got, err)
	}
	// Custom port.
	got, err = tapInboxURL(Config{Tap: TapConfig{Enabled: true, Port: 9001, Devices: []string{"iphone181"}}})
	if err != nil || got != "http://100.78.70.17:9001/inbox" {
		t.Fatalf("tapInboxURL custom port = %q, err %v", got, err)
	}
	// Tailscale IP unresolved: fails closed.
	tapOwnIP = func() (string, error) { return "", errors.New("tailscale down") }
	if _, err := tapInboxURL(Config{Tap: TapConfig{Enabled: true, Devices: []string{"iphone181"}}}); err == nil {
		t.Error("tapInboxURL succeeded without a tailnet IP")
	}
}

func TestTapInboxPiAskExpiredPlanAndEndToEndTap(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := time.Now().UTC().Truncate(time.Second)

	cfg := defaultConfig()
	cfg.Tap = TapConfig{Enabled: true, Devices: []string{"iphone181"}}

	// Expired plan (never ran, prior approval lapsed): must appear in Section 3.
	expPlan := Plan{Bead: "task-exp", Steps: []Step{{Op: opOpaque, Text: "deploy staging"}}}
	expDigest := writeCheckedPlanFixture(t, cfg, Bead{ID: "task-exp", Status: StatusInProgress}, expPlan, []planEvent{
		{At: now.Add(-2 * time.Hour), Kind: planChecked, Class: 9, Pass: false, Why: "above level"},
		{At: now.Add(-2 * time.Hour), Kind: planApproved, Class: 9, Pass: true, By: "TJ", Expires: now.Add(-1 * time.Hour)},
	}, now)

	// Blocked pi worker waiting on a control-socket ask.
	piBead := Bead{ID: "task-pi", Status: StatusInProgress, Title: "Pi ask worker"}
	piAgent := Agent{Name: "task-pi", Kind: "pi", Status: "blocked", PaneID: "w9:p1"}
	ask := &piAsk{
		ID:      "ask-42",
		Command: "git push origin feat/demo",
		Reason:  "pushing to remote branch",
		Options: []string{"Allow once", "Deny"},
	}
	var answeredID string
	var answeredChoice int
	var notes []string

	penv := promptEnv{
		show: func(id string) (Bead, error) {
			if id == piBead.ID {
				return piBead, nil
			}
			return Bead{ID: id, Status: StatusInProgress}, nil
		},
		agent: func(_, name string) (*Agent, error) {
			if name == piAgent.Name {
				return &piAgent, nil
			}
			return nil, nil
		},
		piAsk: func(_ string, a *Agent) (*piAsk, bool, error) {
			if a != nil && a.Name == piAgent.Name {
				return ask, true, nil
			}
			return nil, false, nil
		},
		piAnswer: func(_ *Agent, id string, choice int, _ string) error {
			answeredID, answeredChoice = id, choice
			return nil
		},
		note: func(_ string, text string) error {
			notes = append(notes, text)
			return nil
		},
		sleep: func(time.Duration) {},
		level: func(string) (int, string) { return 4, "" },
	}
	s := newTestInboxServer(t, cfg, penv, &tapInboxEnv{
		active: func() ([]Bead, error) { return []Bead{piBead}, nil },
		agents: func() ([]Agent, error) { return []Agent{piAgent}, nil },
	}, "iphone181")
	s.now = func() time.Time { return now }

	rec := inboxRequest(s, http.MethodGet, "/inbox.json", "", tapPhoneAddr)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /inbox.json = %d %q", rec.Code, rec.Body.String())
	}
	var snap inboxSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Prompts) != 1 || snap.Prompts[0].Bead != "task-pi" || !strings.Contains(strings.Join(snap.Prompts[0].Lines, "\n"), "git push origin feat/demo") {
		t.Fatalf("pi ask prompt = %+v", snap.Prompts)
	}
	if len(snap.Plans) != 1 || snap.Plans[0].Digest != expDigest || snap.Plans[0].Status != planExpired {
		t.Fatalf("expired plan = %+v", snap.Plans)
	}

	// Follow the minted /tap/<token> link from /inbox.json through GET confirm -> POST answer.
	optPath := snap.Prompts[0].Options[0].Path
	confirmRec := inboxRequest(s, http.MethodGet, optPath, "", tapPhoneAddr)
	if confirmRec.Code != http.StatusOK || !strings.Contains(confirmRec.Body.String(), "git push origin feat/demo") || answeredID != "" {
		t.Fatalf("confirm from inbox link = %d answeredID=%q body=%q", confirmRec.Code, answeredID, confirmRec.Body.String())
	}
	postRec := inboxRequest(s, http.MethodPost, optPath, "", tapPhoneAddr)
	if postRec.Code != http.StatusOK || answeredID != "ask-42" || answeredChoice != 1 {
		t.Fatalf("post from inbox link = %d answeredID=%q choice=%d body=%q", postRec.Code, answeredID, answeredChoice, postRec.Body.String())
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "by TJ via tap (device iphone181)") {
		t.Fatalf("notes = %q", notes)
	}
}

func TestTapInboxFreeTextAndOptionPlusTextEndToEnd(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := time.Now().UTC().Truncate(time.Second)

	cfg := defaultConfig()
	cfg.Tap = TapConfig{Enabled: true, Devices: []string{"iphone181"}}

	freeBead := Bead{
		ID:     "task-free",
		Status: StatusNeedsMe,
		Title:  "Clarify naming",
		Notes:  "Should we keep the existing subcommand name?\nCLASS: 2 — cli naming",
	}
	optsBead := Bead{
		ID:     "task-opts",
		Status: StatusNeedsMe,
		Title:  "Choose rollout order",
		Notes:  questionNotes,
	}
	emptyBead := Bead{
		ID:     "task-empty",
		Status: StatusNeedsMe,
		Title:  "No notes yet",
		Notes:  "",
	}
	runBead := Bead{
		ID:     "task-run",
		Status: StatusNeedsMe,
		Title:  "Login needed",
		Notes:  "RUN: aws sso login\nwhy: token expired",
	}

	byID := map[string]*Bead{
		freeBead.ID:  &freeBead,
		optsBead.ID:  &optsBead,
		emptyBead.ID: &emptyBead,
		runBead.ID:   &runBead,
	}
	agents := map[string]*Agent{
		"task-free": {Name: "task-free", Kind: "pi", Status: "idle", PaneID: "w1:p1"},
		"task-opts": {Name: "task-opts", Kind: "pi", Status: "idle", PaneID: "w1:p2"},
	}
	notesByBead := map[string][]string{}
	toldByBead := map[string][]string{}

	penv := promptEnv{
		show: func(id string) (Bead, error) {
			if b, ok := byID[id]; ok {
				return *b, nil
			}
			return Bead{}, errors.New("not found")
		},
		agent: func(_, name string) (*Agent, error) {
			return agents[name], nil
		},
		note: func(id, text string) error {
			notesByBead[id] = append(notesByBead[id], text)
			if b, ok := byID[id]; ok {
				b.Notes += "\n" + text
			}
			return nil
		},
		setStatus: func(id, status string) error {
			if b, ok := byID[id]; ok {
				b.Status = status
			}
			return nil
		},
		tell: func(_ string, a *Agent, text string) error {
			toldByBead[a.Name] = append(toldByBead[a.Name], text)
			return nil
		},
		sleep: func(time.Duration) {},
		level: func(string) (int, string) { return 4, "" },
	}
	ienv := &tapInboxEnv{
		active: func() ([]Bead, error) {
			return []Bead{*byID["task-free"], *byID["task-opts"], *byID["task-empty"], *byID["task-run"]}, nil
		},
		agents: func() ([]Agent, error) {
			return []Agent{*agents["task-free"], *agents["task-opts"]}, nil
		},
	}

	s := newTestInboxServer(t, cfg, penv, ienv, "iphone181")
	s.now = func() time.Time { return now }

	rec := inboxRequest(s, http.MethodGet, "/inbox.json", "", tapPhoneAddr)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /inbox.json = %d %q", rec.Code, rec.Body.String())
	}
	var snap inboxSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	qByBead := map[string]inboxQuestion{}
	for _, q := range snap.Questions {
		qByBead[q.Bead] = q
	}
	if qEmpty := qByBead["task-empty"]; qEmpty.Kind != "free_text" || qEmpty.Tappable || qEmpty.FreeText != nil {
		t.Fatalf("empty needs_me bead should stay read-only: %+v", qEmpty)
	}
	if qRun := qByBead["task-run"]; qRun.Kind != "run" || qRun.Tappable || qRun.FreeText != nil {
		t.Fatalf("RUN bead must never be tappable: %+v", qRun)
	}

	// 1. Free-text-only bead: follow /inbox free_text.path -> GET confirm -> over-limit refusal -> valid POST -> replay refused.
	ftPath := qByBead["task-free"].FreeText.Path
	confirmRec := inboxRequest(s, http.MethodGet, ftPath, "", tapPhoneAddr)
	if confirmRec.Code != http.StatusOK || !strings.Contains(confirmRec.Body.String(), "Should we keep the existing subcommand name?") || !strings.Contains(confirmRec.Body.String(), `maxlength="2000"`) {
		t.Fatalf("free-text confirm = %d %q", confirmRec.Code, confirmRec.Body.String())
	}
	// Over-limit refused without spending nonce.
	overRec := inboxRequest(s, http.MethodPost, ftPath, url.Values{"text": {strings.Repeat("z", 2001)}}.Encode(), tapPhoneAddr)
	if overRec.Code != http.StatusBadRequest {
		t.Fatalf("over-limit POST = %d %q, want 400", overRec.Code, overRec.Body.String())
	}
	// Resubmit valid verbatim text on same link.
	verbatim := "Keep the current name; add an alias in v2."
	postRec := inboxRequest(s, http.MethodPost, ftPath, url.Values{"text": {verbatim}}.Encode(), tapPhoneAddr)
	if postRec.Code != http.StatusOK || !strings.Contains(postRec.Body.String(), "Answered") {
		t.Fatalf("free-text POST = %d %q", postRec.Code, postRec.Body.String())
	}
	if byID["task-free"].Status != StatusInProgress {
		t.Fatalf("task-free status = %s, want in_progress", byID["task-free"].Status)
	}
	if len(notesByBead["task-free"]) != 1 || !strings.HasPrefix(notesByBead["task-free"][0], "NEEDS_ME ANSWERED: free text to ") || !strings.HasSuffix(notesByBead["task-free"][0], ": "+verbatim) {
		t.Fatalf("task-free notes = %q", notesByBead["task-free"])
	}
	if len(toldByBead["task-free"]) != 1 || !strings.Contains(toldByBead["task-free"][0], verbatim) {
		t.Fatalf("task-free told = %q", toldByBead["task-free"])
	}
	if replayRec := inboxRequest(s, http.MethodPost, ftPath, url.Values{"text": {"again"}}.Encode(), tapPhoneAddr); replayRec.Code != http.StatusConflict {
		t.Fatalf("replay POST = %d, want 409", replayRec.Code)
	}

	// 2. OPTIONS bead: follow option 2 link -> GET confirm -> POST option 2 + verbatim text.
	opt2Path := qByBead["task-opts"].Options[1].Path
	confirm2 := inboxRequest(s, http.MethodGet, opt2Path, "", tapPhoneAddr)
	if confirm2.Code != http.StatusOK || !strings.Contains(confirm2.Body.String(), "Answer 2. One region first, then the rest") || !strings.Contains(confirm2.Body.String(), `<textarea`) {
		t.Fatalf("option 2 confirm = %d %q", confirm2.Code, confirm2.Body.String())
	}
	extra := "  use us-west-2 first  "
	post2 := inboxRequest(s, http.MethodPost, opt2Path, url.Values{"text": {extra}}.Encode(), tapPhoneAddr)
	if post2.Code != http.StatusOK {
		t.Fatalf("option 2 + text POST = %d %q", post2.Code, post2.Body.String())
	}
	if byID["task-opts"].Status != StatusInProgress {
		t.Fatalf("task-opts status = %s", byID["task-opts"].Status)
	}
	if len(notesByBead["task-opts"]) != 1 || !strings.HasPrefix(notesByBead["task-opts"][0], "NEEDS_ME ANSWERED: 2 ") || !strings.HasSuffix(notesByBead["task-opts"][0], ": "+extra) {
		t.Fatalf("task-opts notes = %q", notesByBead["task-opts"])
	}
	if len(toldByBead["task-opts"]) != 1 || !strings.Contains(toldByBead["task-opts"][0], "2. One region first, then the rest") || !strings.Contains(toldByBead["task-opts"][0], extra) {
		t.Fatalf("task-opts told = %q", toldByBead["task-opts"])
	}
}
