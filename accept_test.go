package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestAcceptanceCriteria(t *testing.T) {
	desc := `Do the thing.

## Acceptance

- The flag is on in prod
  for every tenant.
* Dashboard shows it
1. Runbook updated

## Notes
- not a criterion
`
	got := acceptanceCriteria(desc)
	want := []string{"The flag is on in prod for every tenant.", "Dashboard shows it", "Runbook updated"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := acceptanceCriteria("## Problem\n- x\n"); got != nil {
		t.Fatalf("no Acceptance section: got %q", got)
	}
	// A ### subheading stays inside the section.
	if got := acceptanceCriteria("## Acceptance:\n### Must\n- a\n"); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("got %q", got)
	}
}

func TestVerifierScope(t *testing.T) {
	pr := "PR: https://github.com/o/r/pull/7"
	run := "run https://github.com/o/r/actions/runs/123"
	for _, c := range []struct {
		name   string
		labels []string
		notes  string
		want   bool
	}{
		{"PR only", nil, pr, false},
		{"no PR", nil, "audit written up", true},
		{"rolling out", []string{LabelRollingOut}, pr, true},
		{"runs noted, label already removed", nil, pr + "\n" + run, true},
	} {
		got, why := verifierScope(Bead{Labels: c.labels, Notes: c.notes})
		if got != c.want {
			t.Errorf("%s: got %v (%s), want %v", c.name, got, why, c.want)
		}
	}
}

func TestParseVerdict(t *testing.T) {
	out := "checked things\nVERDICT {\"verdict\":\"fail\"}\nmore\nVERDICT {\"verdict\":\"pass\",\"criteria\":[{\"criterion\":\"a\",\"pass\":true,\"evidence\":\"e\"}]}\n"
	r, err := parseVerdict(out)
	if err != nil || !r.passed() {
		t.Fatalf("the last VERDICT line counts: %+v %v", r, err)
	}
	if _, err := parseVerdict("no verdict here"); err == nil {
		t.Fatal("want an error without a VERDICT line")
	}
	// "pass" with a failing criterion, or none, is a fail.
	for _, r := range []verifierResult{
		{Verdict: "pass", Criteria: []criterionResult{{Pass: true}, {Pass: false}}},
		{Verdict: "pass"},
	} {
		if r.passed() {
			t.Fatalf("%+v should not pass", r)
		}
	}
}

func TestParseVerdictFormattingAndSafety(t *testing.T) {
	payload := `{"verdict":"pass","derived":false,"criteria":[{"criterion":"run green","pass":true,"evidence":"checked ` + "`main`" + `"}]}`
	accepted := map[string]string{
		"plain":                  "summary\nVERDICT " + payload + "\n",
		"crlf":                   "summary\r\nVERDICT " + payload + "\r\n",
		"colon":                  "summary\nVERDICT: " + payload,
		"tab":                    "summary\nVERDICT\t" + payload,
		"inline backticks":       "summary\n`VERDICT " + payload + "`\n",
		"bold whole line":        "summary\n**VERDICT " + payload + "**\n",
		"bold label":             "summary\n**VERDICT** " + payload,
		"bold label colon":       "summary\n**VERDICT:** " + payload,
		"bold label outer colon": "summary\n**VERDICT**: " + payload,
		"backticked label":       "summary\n`VERDICT` " + payload,
		"backticked json":        "summary\nVERDICT `" + payload + "`",
		"fenced plain":           "summary\n```\nVERDICT " + payload + "\n```\n",
		"fenced json":            "summary\n```json\nVERDICT " + payload + "\n```\n",
		"tilde fence":            "summary\n~~~\nVERDICT: " + payload + "\n~~~\n",
		"header before fence":    "summary\n**VERDICT:**\n```json\n{\n  \"verdict\": \"pass\",\n  \"derived\": false,\n  \"criteria\": [{\"criterion\": \"run green\", \"pass\": true, \"evidence\": \"ok\"}]\n}\n```\n",
		"multiline json":         "summary\nVERDICT {\n  \"verdict\": \"pass\",\n  \"derived\": false,\n  \"criteria\": [{\"criterion\": \"run green\", \"pass\": true, \"evidence\": \"ok\"}]\n}\n",
		"header then multiline":  "summary\nVERDICT:\n{\n  \"verdict\": \"pass\",\n  \"derived\": false,\n  \"criteria\": [{\"criterion\": \"run green\", \"pass\": true, \"evidence\": \"ok\"}]\n}\n",
	}
	for name, out := range accepted {
		t.Run("accept/"+name, func(t *testing.T) {
			r, err := parseVerdict(out)
			if err != nil || !r.passed() {
				t.Fatalf("parseVerdict(%s) = %+v, %v", name, r, err)
			}
		})
	}

	rejected := map[string]struct {
		out  string
		want string
	}{
		"empty":                  {out: "   \n\t ", want: "empty output"},
		"prose no verdict":       {out: "checked everything\nlooks good", want: "tail: checked everything looks good"},
		"trailing prose retract": {out: passOut + "\nActually, do not close.", want: "verifier gave no VERDICT line"},
		"quoted file pass":       {out: "File contained:\n> " + passOut, want: "verifier gave no VERDICT line"},
		"bulleted pass":          {out: "- " + passOut, want: "verifier gave no VERDICT line"},
		"midline pass":           {out: "Saw " + passOut, want: "verifier gave no VERDICT line"},
		"trailing tokens":        {out: passOut + " extra", want: "not JSON"},
		"invalid json":           {out: "VERDICT {broken", want: "not JSON"},
		"null payload":           {out: "VERDICT null", want: "not JSON"},
		"array payload":          {out: "VERDICT []", want: "not JSON"},
		"missing verdict field":  {out: `VERDICT {"criteria":[]}`, want: `must be "pass" or "fail"`},
		"invalid verdict enum":   {out: `VERDICT {"verdict":"maybe","criteria":[]}`, want: `must be "pass" or "fail"`},
		"retracted before brace": {out: passOut + "\nWait, config has {\"broken\": true}", want: "not JSON"},
		"unclosed fence":         {out: "```\n" + passOut + "\n", want: "verifier gave no VERDICT line"},
		"fence without header":   {out: "```json\n" + payload + "\n```", want: "verifier gave no VERDICT line"},
		"prose after fence":      {out: "```\n" + passOut + "\n```\nNot done yet.", want: "verifier gave no VERDICT line"},
	}
	for name, tc := range rejected {
		t.Run("reject/"+name, func(t *testing.T) {
			if r, err := parseVerdict(tc.out); err == nil {
				t.Fatalf("accepted %s: %+v", name, r)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s: err = %q, want substring %q", name, err, tc.want)
			}
		})
	}
}

func TestVerifierDiagnosticSanitization(t *testing.T) {
	raw := "\x1b[31mERROR\x1b[0m\n" +
		"Authorization: Bearer top-secret-bearer-token-12345\n" +
		"token=ghp_1234567890abcdef123456\n" +
		"github_pat_abcdef1234567890\n" +
		"AKIAIOSFODNN7EXAMPLE\n" +
		"AIzaSyD-1234567890abcdefGHIJKL\n" +
		"lin_api_secretkey1234567890\n" +
		"ya29.a0AfH6SMBx1234567890abcdef\n" +
		"https://alice:hunter2@example.com/path\n" +
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----\n" +
		strings.Repeat("x", verifierDiagnosticLimit+200)

	for _, got := range []string{
		verifierHeadExcerpt(raw, verifierDiagnosticLimit),
		verifierTailExcerpt(raw, verifierDiagnosticLimit),
	} {
		for _, leak := range []string{
			"\x1b", "[31m", "\n",
			"top-secret-bearer", "ghp_123456", "github_pat_abcdef",
			"AKIAIOSFODNN7EXAMPLE", "AIzaSyD-", "lin_api_secretkey",
			"ya29.a0AfH6", "hunter2", "MIIEowIBAAKCAQEA", "PRIVATE KEY",
		} {
			if strings.Contains(got, leak) {
				t.Errorf("diagnostic leaked %q: %s", leak, got)
			}
		}
		if utf8.RuneCountInString(got) > verifierDiagnosticLimit+3 {
			t.Errorf("diagnostic exceeded bound: %d runes", utf8.RuneCountInString(got))
		}
	}
	// Redaction happens before truncation even when a secret straddles the boundary.
	straddle := strings.Repeat("a", verifierDiagnosticLimit-5) + " ghp_secretvalue1234567890"
	got := verifierHeadExcerpt(straddle, verifierDiagnosticLimit)
	if strings.Contains(got, "ghp_") || strings.Contains(got, "secretvalue") {
		t.Fatalf("truncated before redacting: %q", got)
	}
}

func TestVerifierEnvIsolation(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"HOME=/home/test",
		"BEADS_DIR=/repo/.beads",
		"BEADS_ACTOR=worker",
		"HERDR_ENV=1",
		"HERDR_PANE_ID=w1:p1",
		"HERDR_TAB_ID=w1:t1",
		"HERDR_WORKSPACE_ID=w1",
		"HERDR_SOCKET_PATH=/tmp/herdr.sock",
		"QUARTERMASTER_BEAD=b-1",
		"ANTIGRAVITY_AGENT=1",
		"ANTIGRAVITY_APP_DATA_DIR=/tmp/acp",
		"ANTIGRAVITY_CONVERSATION_ID=conv-1",
		"ANTIGRAVITY_LS_ADDRESS=localhost:12345",
		"ANTIGRAVITY_CSRF_TOKEN=secret",
		"ANTIGRAVITY_SOURCE_METADATA={}",
		"CLAUDECODE=1",
		"CLAUDE_CODE_ENTRYPOINT=cli",
		"PI_CODING_AGENT=true",
		"PI_EMPHASIS_GUIDANCE=0",
		"PI_MODEL=worker-model",
		"PI_SESSION_ID=sess-1",
		"PI_CODING_AGENT_DIR=/auth-home",
		"RIPGREP_CONFIG_PATH=/unsafe/rg",
		"GEMINI_API_KEY=keep-auth",
		"ANTHROPIC_API_KEY=keep-auth",
	}
	got := verifierEnv(in)
	want := []string{
		"PATH=/usr/bin",
		"HOME=/home/test",
		"BEADS_DIR=/repo/.beads",
		"HERDR_SOCKET_PATH=/tmp/herdr.sock",
		"PI_CODING_AGENT_DIR=/auth-home",
		"GEMINI_API_KEY=keep-auth",
		"ANTHROPIC_API_KEY=keep-auth",
		"BEADS_ACTOR=verifier",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("verifierEnv:\ngot  %v\nwant %v", got, want)
	}
}

func TestRunVerifierScrubsEnvAndSurfacesExitZeroStderr(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "verifier.sh")
	script := `#!/bin/sh
env > "$FIXTURE/env.txt"
printf '%s' "$FIXTURE_STDOUT"
printf '%s' "$FIXTURE_STDERR" >&2
exit "$FIXTURE_EXIT"
`
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIXTURE", dir)
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "parent-conv")
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", "localhost:9999")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("PI_CODING_AGENT", "true")
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p9")
	t.Setenv("QUARTERMASTER_BEAD", "b-parent")
	t.Setenv("BEADS_ACTOR", "worker")

	cfg := VerifierConfig{Command: []string{stub}}

	// Case 1: exit 0, empty stdout, permission auto-deny on stderr (plus secret).
	t.Setenv("FIXTURE_EXIT", "0")
	t.Setenv("FIXTURE_STDOUT", "")
	t.Setenv("FIXTURE_STDERR", "tool required \"command\" permission, auto-denied Bearer secret-token-abc\n")
	out, err := runVerifier(cfg, dir, "prompt")
	if !errors.Is(err, errVerifierOutput) || out != "" {
		t.Fatalf("exit-0 empty stdout with stderr: out=%q err=%v", out, err)
	}
	if !strings.Contains(err.Error(), "empty output") || !strings.Contains(err.Error(), "auto-denied") {
		t.Fatalf("missing diagnostic details: %v", err)
	}
	if strings.Contains(err.Error(), "secret-token-abc") {
		t.Fatalf("stderr leaked secret: %v", err)
	}

	// Verify nested worker/agent env vars were scrubbed and BEADS_ACTOR=verifier set.
	envBytes, err := os.ReadFile(filepath.Join(dir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	envText := string(envBytes)
	for _, forbidden := range []string{
		"ANTIGRAVITY_CONVERSATION_ID=", "ANTIGRAVITY_LS_ADDRESS=",
		"CLAUDECODE=", "PI_CODING_AGENT=", "HERDR_ENV=", "HERDR_PANE_ID=",
		"QUARTERMASTER_BEAD=", "BEADS_ACTOR=worker",
	} {
		if strings.Contains(envText, forbidden) {
			t.Errorf("verifier inherited %q", forbidden)
		}
	}
	if !strings.Contains(envText, "BEADS_ACTOR=verifier\n") {
		t.Errorf("BEADS_ACTOR=verifier not set in child env")
	}

	// Case 2: valid VERDICT succeeds even when stderr has benign progress output.
	t.Setenv("FIXTURE_STDOUT", "checked\n```json\n"+passOut+"\n```\n")
	t.Setenv("FIXTURE_STDERR", "Working...\n")
	out, err = runVerifier(cfg, dir, "prompt")
	if err != nil {
		t.Fatalf("valid fenced verdict with benign stderr failed: %v", err)
	}
	res, err := parseVerdict(out)
	if err != nil || !res.passed() {
		t.Fatalf("parseVerdict failed: %+v %v", res, err)
	}

	// Case 3: nonzero exit redacts secrets in stderr.
	t.Setenv("FIXTURE_EXIT", "1")
	t.Setenv("FIXTURE_STDOUT", "")
	t.Setenv("FIXTURE_STDERR", "failed with token=ghp_1234567890abcdef123456\n")
	_, err = runVerifier(cfg, dir, "prompt")
	if err == nil || errors.Is(err, errVerifierOutput) {
		t.Fatalf("nonzero exit should be a launch/provider error, got %v", err)
	}
	if strings.Contains(err.Error(), "ghp_123456") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("nonzero stderr not sanitized: %v", err)
	}
}

func TestRunVerifierCustomCommandTimeoutKillsHelpers(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "hang.sh")
	script := "#!/bin/sh\n(sleep 1; printf escaped > \"$FIXTURE/escaped\") &\nwait\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIXTURE", dir)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := runVerifierContext(ctx, VerifierConfig{Command: []string{stub}}, dir, "prompt")
	if err == nil || errors.Is(err, errVerifierOutput) {
		t.Fatalf("timeout should return a launch/timeout error, got %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "escaped")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("helper survived custom verifier timeout")
	}
}

// fakeClose is an in-memory closeEnv: one bead, a journal, and a verifier
// that returns outs in turn.
type fakeClose struct {
	bead    Bead
	notes   []string
	closed  string
	status  string
	labels  map[string]bool
	journal []journalRecord
	outs    []string
	runErr  error
	prompts []string
	denied  bool
}

func (f *fakeClose) env() closeEnv {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	return closeEnv{
		show:      func(string) (Bead, error) { return f.bead, nil },
		close:     func(_, reason string) error { f.closed = reason; return nil },
		note:      func(_, text string) error { f.notes = append(f.notes, text); return nil },
		setStatus: func(_, s string) error { f.status = s; return nil },
		setLabel: func(_, l string, on bool) error {
			if f.labels == nil {
				f.labels = map[string]bool{}
			}
			f.labels[l] = on
			return nil
		},
		enforce: func(_ Bead, by string) (string, error) {
			if f.denied && by != "TJ" {
				return "", errNotAutonomous
			}
			if by == "TJ" {
				return "", nil
			}
			return "AUTO: [" + by + " L4 C3]", nil
		},
		verify: func(_, prompt string) (string, error) {
			f.prompts = append(f.prompts, prompt)
			if f.runErr != nil {
				return "", f.runErr
			}
			out := f.outs[0]
			f.outs = f.outs[1:]
			return out, nil
		},
		journal: func(time.Time) ([]journalRecord, error) { return f.journal, nil },
		record:  func(r journalRecord) error { f.journal = append(f.journal, r); return nil },
		now:     func() time.Time { return now },
	}
}

const (
	passOut = `VERDICT {"verdict":"pass","criteria":[{"criterion":"run green","pass":true,"evidence":"run 123 succeeded"}]}`
	failOut = `VERDICT {"verdict":"fail","criteria":[{"criterion":"run green","pass":false,"evidence":"run 123 failed"}]}`
)

func noPRBead() Bead {
	return Bead{ID: "backend-ab12", Title: "Audit the thing", Status: StatusInProgress,
		Description: "Audit it.\n\n## Acceptance\n- run green\n", Notes: "SECRET-CONCLUSION: it is all done"}
}

func hasNote(notes []string, prefix string) bool {
	return slices.ContainsFunc(notes, func(n string) bool { return strings.HasPrefix(n, prefix) })
}

func TestClosePass(t *testing.T) {
	f := &fakeClose{bead: noPRBead(), outs: []string{passOut}}
	if err := closeBead(defaultConfig(), f.env(), "backend-ab12", "audited", "worker", io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.closed != "audited" || !hasNote(f.notes, "VERIFY: pass [verifier]") || !hasNote(f.notes, "AUTO: [worker L4 C3] close") {
		t.Fatalf("closed=%q notes=%q", f.closed, f.notes)
	}
	if len(f.journal) != 1 || f.journal[0].Kind != journalVerify || !f.journal[0].Pass {
		t.Fatalf("journal: %+v", f.journal)
	}
	// Fresh context: the verifier sees the criteria, not the worker's notes.
	p := f.prompts[0]
	if !strings.Contains(p, "1. run green") || strings.Contains(p, "SECRET-CONCLUSION") {
		t.Fatalf("prompt: %s", p)
	}
}

func TestCloseFailReturnsToWorkerThenEscalates(t *testing.T) {
	f := &fakeClose{bead: noPRBead(), outs: []string{failOut, failOut}}
	var out strings.Builder
	err := closeBead(defaultConfig(), f.env(), "backend-ab12", "audited", "worker", &out)
	if !errors.Is(err, errVerifyFailed) || f.closed != "" || f.status != "" {
		t.Fatalf("round 1: err=%v closed=%q status=%q", err, f.closed, f.status)
	}
	if !strings.Contains(out.String(), "run green — run 123 failed") || !hasNote(f.notes, "VERIFY: fail [verifier] round 1/2") {
		t.Fatalf("round 1 should hand back the failing criteria: %s %q", out.String(), f.notes)
	}
	err = closeBead(defaultConfig(), f.env(), "backend-ab12", "audited", "worker", io.Discard)
	if !errors.Is(err, errVerifyFailed) || f.closed != "" || f.status != StatusNeedsMe {
		t.Fatalf("round 2: err=%v closed=%q status=%q", err, f.closed, f.status)
	}
	last := f.notes[len(f.notes)-1]
	if !strings.Contains(last, "escalated to TJ") || !strings.Contains(last, "Worker's close reason: audited") || !strings.Contains(last, "CLASS: 3") {
		t.Fatalf("escalation note: %s", last)
	}
}

func TestCloseFailThenPassResetsRounds(t *testing.T) {
	f := &fakeClose{bead: noPRBead(), outs: []string{failOut, passOut}}
	_ = closeBead(defaultConfig(), f.env(), "backend-ab12", "r", "worker", io.Discard)
	if err := closeBead(defaultConfig(), f.env(), "backend-ab12", "r", "worker", io.Discard); err != nil || f.closed != "r" {
		t.Fatalf("err=%v closed=%q", err, f.closed)
	}
	if fails, _ := verifyRounds(f.journal, "backend-ab12", time.Time{}); fails != 0 {
		t.Fatalf("a pass resets the rounds, got %d", fails)
	}
}

func TestCloseRetryBoundConfigurable(t *testing.T) {
	cfg := defaultConfig()
	cfg.Verifier.MaxRounds = 1
	f := &fakeClose{bead: noPRBead(), outs: []string{failOut}}
	if err := closeBead(cfg, f.env(), "backend-ab12", "r", "worker", io.Discard); !errors.Is(err, errVerifyFailed) || f.status != StatusNeedsMe {
		t.Fatalf("err=%v status=%q", err, f.status)
	}
}

func TestCloseOutOfScopeSkipsVerifier(t *testing.T) {
	b := noPRBead()
	b.Notes = "PR: https://github.com/o/r/pull/7"
	f := &fakeClose{bead: b}
	if err := closeBead(defaultConfig(), f.env(), b.ID, "merged", "worker", io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.closed != "merged" || len(f.prompts) != 0 || len(f.journal) != 0 || hasNote(f.notes, "VERIFY:") {
		t.Fatalf("a PR-only bead closes as before: closed=%q prompts=%d notes=%q", f.closed, len(f.prompts), f.notes)
	}
}

func TestCloseWithoutAcceptanceSection(t *testing.T) {
	b := noPRBead()
	b.Description = "Investigate why the thing is slow and write it up."
	out := `VERDICT {"verdict":"pass","derived":true,"criteria":[{"criterion":"write-up exists","pass":true,"evidence":"docs/x.md"}]}`
	f := &fakeClose{bead: b, outs: []string{out}}
	if err := closeBead(defaultConfig(), f.env(), b.ID, "written up", "worker", io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.prompts[0], "Derive 2-4 checkable criteria") {
		t.Fatalf("prompt should ask to derive criteria: %s", f.prompts[0])
	}
	if !slices.ContainsFunc(f.notes, func(n string) bool {
		return strings.HasPrefix(n, "VERIFY: pass") && strings.Contains(n, "criteria derived from the description")
	}) {
		t.Fatalf("notes: %q", f.notes)
	}
}

func TestCloseRollingOutRemovesLabel(t *testing.T) {
	b := noPRBead()
	b.Labels = []string{LabelRollingOut}
	b.Notes = "PR: https://github.com/o/r/pull/7"
	f := &fakeClose{bead: b, outs: []string{passOut}}
	if err := closeBead(defaultConfig(), f.env(), b.ID, "rolled out", "worker", io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.prompts) != 1 || f.labels[LabelRollingOut] || f.closed == "" {
		t.Fatalf("prompts=%d labels=%v closed=%q", len(f.prompts), f.labels, f.closed)
	}
	if !strings.Contains(f.prompts[0], "PR: https://github.com/o/r/pull/7") {
		t.Fatalf("PR URL is a pointer for the verifier: %s", f.prompts[0])
	}
}

func TestCloseByTJSkipsVerifier(t *testing.T) {
	f := &fakeClose{bead: noPRBead()}
	if err := closeBead(defaultConfig(), f.env(), "backend-ab12", "TJ checked", "TJ", io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.closed == "" || len(f.prompts) != 0 || !hasNote(f.notes, "VERIFY: skipped [TJ]") {
		t.Fatalf("closed=%q prompts=%d notes=%q", f.closed, len(f.prompts), f.notes)
	}
}

func TestCloseRefusals(t *testing.T) {
	f := &fakeClose{bead: noPRBead(), denied: true}
	if err := closeBead(defaultConfig(), f.env(), "backend-ab12", "r", "worker", io.Discard); !errors.Is(err, errNotAutonomous) || len(f.prompts) != 0 {
		t.Fatalf("below the dial: err=%v", err)
	}
	if err := closeBead(defaultConfig(), (&fakeClose{bead: noPRBead()}).env(), "backend-ab12", " ", "worker", io.Discard); err == nil {
		t.Fatal("want an error without a reason")
	}
	// A verifier that fails to run spends no round and closes nothing.
	f = &fakeClose{bead: noPRBead(), runErr: errors.New("claude: not found")}
	if err := closeBead(defaultConfig(), f.env(), "backend-ab12", "r", "worker", io.Discard); err == nil || f.closed != "" || len(f.journal) != 0 {
		t.Fatalf("err=%v closed=%q journal=%v", err, f.closed, f.journal)
	}
	// Exit-0 verifier output failure carrying stderr spends a round and surfaces
	// the diagnostic in both stdout and the VERIFY: fail note.
	f = &fakeClose{bead: noPRBead(), runErr: errors.Join(errVerifierOutput, errors.New(`verifier gave no VERDICT line (empty output) (stderr: tool required "command" permission)`))}
	var diagOut strings.Builder
	if err := closeBead(defaultConfig(), f.env(), "backend-ab12", "r", "worker", &diagOut); !errors.Is(err, errVerifyFailed) || f.closed != "" || len(f.journal) != 1 {
		t.Fatalf("err=%v closed=%q journal=%v", err, f.closed, f.journal)
	}
	if !strings.Contains(diagOut.String(), `tool required "command" permission`) || !strings.Contains(f.notes[0], `tool required "command" permission`) {
		t.Fatalf("stderr diagnostic not surfaced: out=%q notes=%q", diagOut.String(), f.notes)
	}
}

func TestCloseReusesFreshPass(t *testing.T) {
	f := &fakeClose{bead: noPRBead()}
	now := f.env().now()
	f.journal = []journalRecord{{At: now.Add(-10 * time.Minute), Kind: journalVerify, Bead: "backend-ab12", Pass: true}}
	if err := closeBead(defaultConfig(), f.env(), "backend-ab12", "r", "worker", io.Discard); err != nil || len(f.prompts) != 0 {
		t.Fatalf("a fresh pass needs no rerun: err=%v prompts=%d", err, len(f.prompts))
	}
	// An old one does.
	f = &fakeClose{bead: noPRBead(), outs: []string{passOut}}
	f.journal = []journalRecord{{At: now.Add(-2 * time.Hour), Kind: journalVerify, Bead: "backend-ab12", Pass: true}}
	if err := closeBead(defaultConfig(), f.env(), "backend-ab12", "r", "worker", io.Discard); err != nil || len(f.prompts) != 1 {
		t.Fatalf("a stale pass reruns: err=%v prompts=%d", err, len(f.prompts))
	}
}

func TestBriefsDescribeAcceptanceAndVerifier(t *testing.T) {
	b := Bead{ID: "backend-ab12", Title: "t"}
	for name, text := range map[string]string{
		"brief": brief(b, "tj/x", "/w", "", ""),
		"guide": coordinatorGuide(defaultConfig()),
	} {
		for _, want := range []string{"## Acceptance", "quartermaster close", "read-only verifier", "needs_me", "rolling-out"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
		if strings.Contains(text, "`bd close ") {
			t.Errorf("%s still tells agents to bd close", name)
		}
	}
}
