package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testWatchEnv(t *testing.T, now *time.Time, beads map[string]Bead) (WatchEnv, *[]string, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var notes []string
	var tells []string
	env := WatchEnv{
		Show: func(id string) (Bead, error) {
			mu.Lock()
			defer mu.Unlock()
			b, ok := beads[id]
			if !ok {
				return Bead{}, fmt.Errorf("no bead %s", id)
			}
			return b, nil
		},
		Note: func(id, text string) error {
			mu.Lock()
			defer mu.Unlock()
			notes = append(notes, id+": "+text)
			if b, ok := beads[id]; ok {
				if b.Notes != "" {
					b.Notes += "\n"
				}
				b.Notes += text
				beads[id] = b
			}
			return nil
		},
		Tell: func(b Bead, a Agent, text string) error {
			mu.Lock()
			defer mu.Unlock()
			tells = append(tells, b.ID+"@"+a.Name+": "+text)
			return nil
		},
		Now:  func() time.Time { return *now },
		Logf: func(string, ...any) {},
	}
	return env, &notes, &tells
}

func TestWatchPersistencePermissionsAndCLI(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := t0
	beads := map[string]Bead{
		"task-1": {ID: "task-1", Title: "Wait on upstream release", Status: StatusBlocked},
		"task-2": {ID: "task-2", Title: "Already done", Status: StatusClosed},
	}
	env, _, _ := testWatchEnv(t, &now, beads)
	cfg := defaultConfig()

	// Closed bead and invalid options must be refused.
	var out bytes.Buffer
	if err := watchCmd(cfg, env, []string{"add", "task-2", "--kind", "gh-release", "--repo", "acme/app", "--after", "v1.0.0"}, &out); err == nil {
		t.Fatal("accepted watch on closed bead")
	}
	if err := watchCmd(cfg, env, []string{"add", "missing-1", "--kind", "gh-release", "--repo", "acme/app", "--after", "v1.0.0"}, &out); err == nil {
		t.Fatal("accepted watch on unknown bead")
	}
	if err := watchCmd(cfg, env, []string{"add", "task-1", "--kind", "gh-release", "--repo", "acme/app", "--after", "v1.0.0", "--every", "10s"}, &out); err == nil {
		t.Fatal("accepted interval below minimum 30s")
	}
	if err := watchCmd(cfg, env, []string{"add", "task-1", "--kind", "gh-release", "--repo", "../bad", "--after", "v1.0.0"}, &out); err == nil {
		t.Fatal("accepted malformed repo")
	}
	if err := watchCmd(cfg, env, []string{"add", "task-1", "--kind", "gh-release", "--repo", "acme/app", "--after", "v1.0.0", "--url", "https://example.com"}, &out); err == nil {
		t.Fatal("accepted cross-kind flag --url on gh-release")
	}
	if err := watchCmd(cfg, env, []string{"add", "task-1", "--kind", "http", "--url", "file:///etc/passwd"}, &out); err == nil {
		t.Fatal("accepted non-http URL scheme")
	}
	if err := watchCmd(cfg, env, []string{"add", "task-1", "--kind", "http", "--url", "https://example.com/health", "--on-trigger", "bogus"}, &out); err == nil {
		t.Fatal("accepted invalid on-trigger mode")
	}

	// Valid add persists with private 0700 dir and 0600 file permissions.
	out.Reset()
	if err := watchCmd(cfg, env, []string{
		"add", "task-1",
		"--kind", "gh-release",
		"--repo", "acme/app",
		"--after", "v1.2.0",
		"--every", "2m",
		"--expires", "7d",
		"--on-trigger", "wake",
		"--label", "upstream v1.3",
	}, &out); err != nil {
		t.Fatalf("watch add failed: %v", err)
	}
	if !strings.Contains(out.String(), "added watch ") || !strings.Contains(out.String(), "upstream v1.3") {
		t.Fatalf("unexpected add output: %s", out.String())
	}

	dirInfo, err := os.Stat(watchesDir())
	if err != nil {
		t.Fatalf("stat watches dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("watches dir perm = %o, want 0700", perm)
	}
	fileInfo, err := os.Stat(watchesPath())
	if err != nil {
		t.Fatalf("stat watches.json: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("watches.json perm = %o, want 0600", perm)
	}

	// List JSON returns the active watch with typed params and expiry.
	out.Reset()
	if err := watchCmd(cfg, env, []string{"list", "task-1", "--json"}, &out); err != nil {
		t.Fatalf("watch list --json: %v", err)
	}
	var listed []Watch
	if err := json.Unmarshal(out.Bytes(), &listed); err != nil {
		t.Fatalf("unmarshal list json: %v\n%s", err, out.String())
	}
	if len(listed) != 1 {
		t.Fatalf("got %d watches, want 1", len(listed))
	}
	w := listed[0]
	if w.Bead != "task-1" || w.Kind != WatchKindGHRelease || w.Status != WatchStatusActive ||
		w.OnTrigger != WatchOnTriggerWake || w.Every != "2m" || w.Params.GHRelease == nil ||
		w.Params.GHRelease.Repo != "acme/app" || w.Params.GHRelease.After != "v1.2.0" {
		t.Fatalf("unexpected persisted watch: %+v", w)
	}
	if w.ExpiresAt == nil || !w.ExpiresAt.Equal(now.Add(7*24*time.Hour)) {
		t.Fatalf("unexpected expires_at: %v", w.ExpiresAt)
	}

	// Remove by ID marks status=removed; default list hides it, --all shows it.
	out.Reset()
	if err := watchCmd(cfg, env, []string{"remove", "task-1", w.ID}, &out); err != nil {
		t.Fatalf("watch remove: %v", err)
	}
	out.Reset()
	if err := watchCmd(cfg, env, []string{"list", "task-1", "--json"}, &out); err != nil {
		t.Fatalf("watch list after remove: %v", err)
	}
	listed = nil
	if err := json.Unmarshal(out.Bytes(), &listed); err != nil || len(listed) != 0 {
		t.Fatalf("expected empty active list after remove, got %+v (err=%v)", listed, err)
	}
	out.Reset()
	if err := watchCmd(cfg, env, []string{"list", "task-1", "--all", "--json"}, &out); err != nil {
		t.Fatalf("watch list --all: %v", err)
	}
	if err := json.Unmarshal(out.Bytes(), &listed); err != nil || len(listed) != 1 || listed[0].Status != WatchStatusRemoved {
		t.Fatalf("expected removed watch with --all, got %+v (err=%v)", listed, err)
	}

	// Add two more and test remove --all.
	for _, lbl := range []string{"w1", "w2"} {
		if err := watchCmd(cfg, env, []string{"add", "task-1", "--kind", "http", "--url", "https://example.com/ok", "--label", lbl}, &out); err != nil {
			t.Fatalf("add %s: %v", lbl, err)
		}
	}
	out.Reset()
	if err := watchCmd(cfg, env, []string{"remove", "task-1", "--all"}, &out); err != nil {
		t.Fatalf("remove --all: %v", err)
	}
	if !strings.Contains(out.String(), "removed 2 active watch(es)") {
		t.Fatalf("unexpected remove --all output: %s", out.String())
	}
}

func TestWatchKindGHRelease(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := t0
	beads := map[string]Bead{"task-rel": {ID: "task-rel", Status: StatusBlocked}}
	env, notes, _ := testWatchEnv(t, &now, beads)

	var releaseJSON string
	var gotArgs []string
	env.RunGH = func(ctx context.Context, args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return []byte(releaseJSON), nil
	}
	cfg := defaultConfig()

	var out bytes.Buffer
	if err := watchCmd(cfg, env, []string{
		"add", "task-rel",
		"--kind", "gh-release",
		"--repo", "acme/tool",
		"--after", "v2.0.0",
		"--match", "v2.*",
		"--every", "1m",
	}, &out); err != nil {
		t.Fatal(err)
	}

	// Same tag does not trigger.
	releaseJSON = `{"tag_name":"v2.0.0","html_url":"https://github.com/acme/tool/releases/tag/v2.0.0"}`
	outcomes, err := runWatchesPass(cfg, env, nil, nil, "task-rel", true)
	if err != nil || len(outcomes) != 1 || outcomes[0].Action != "pending" {
		t.Fatalf("same tag: outcomes=%+v err=%v", outcomes, err)
	}
	if strings.Join(gotArgs, " ") != "api repos/acme/tool/releases/latest" {
		t.Fatalf("unexpected gh args: %v", gotArgs)
	}

	// Empty tag or draft does not trigger.
	releaseJSON = `{"tag_name":""}`
	outcomes, err = runWatchesPass(cfg, env, nil, nil, "task-rel", true)
	if err != nil || outcomes[0].Action != "pending" {
		t.Fatalf("empty tag triggered: %+v %v", outcomes, err)
	}
	releaseJSON = `{"tag_name":"v2.1.0","draft":true}`
	outcomes, err = runWatchesPass(cfg, env, nil, nil, "task-rel", true)
	if err != nil || outcomes[0].Action != "pending" {
		t.Fatalf("draft release triggered: %+v %v", outcomes, err)
	}

	// Tag not matching glob does not trigger.
	releaseJSON = `{"tag_name":"v3.0.0"}`
	outcomes, err = runWatchesPass(cfg, env, nil, nil, "task-rel", true)
	if err != nil || outcomes[0].Action != "pending" {
		t.Fatalf("non-matching tag triggered: %+v %v", outcomes, err)
	}

	// Matching newer tag triggers once.
	releaseJSON = `{"tag_name":"v2.1.0","html_url":"https://github.com/acme/tool/releases/tag/v2.1.0"}`
	outcomes, err = runWatchesPass(cfg, env, nil, nil, "task-rel", true)
	if err != nil || len(outcomes) != 1 || outcomes[0].Action != "triggered" {
		t.Fatalf("expected trigger, got outcomes=%+v err=%v", outcomes, err)
	}
	if !strings.Contains(outcomes[0].Watch.Evidence, "v2.1.0") {
		t.Fatalf("missing version in evidence: %q", outcomes[0].Watch.Evidence)
	}
	if len(*notes) != 1 || !strings.Contains((*notes)[0], "WATCH TRIGGERED [") {
		t.Fatalf("expected bead note, got %+v", *notes)
	}

	// Prefix match without glob metacharacters.
	res, err := evalGHReleaseWatch(context.Background(), env, &GHReleaseWatchParams{Repo: "acme/tool", Match: "v2."})
	if err != nil || !res.Triggered {
		t.Fatalf("prefix match failed: res=%+v err=%v", res, err)
	}
}

func TestWatchKindGHPR(t *testing.T) {
	now := t0
	beads := map[string]Bead{"task-pr": {ID: "task-pr", Status: StatusBlocked}}
	env, _, _ := testWatchEnv(t, &now, beads)

	var prJSON string
	env.RunGH = func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte(prJSON), nil
	}

	cases := []struct {
		name   string
		params GHPRWatchParams
		json   string
		want   bool
	}{
		{"merged pending when open", GHPRWatchParams{Repo: "acme/app", PR: 10, Expect: "merged"}, `{"number":10,"state":"OPEN"}`, false},
		{"merged triggers on MERGED", GHPRWatchParams{Repo: "acme/app", PR: 10, Expect: "merged"}, `{"number":10,"state":"MERGED","url":"https://github.com/acme/app/pull/10"}`, true},
		{"closed triggers on CLOSED", GHPRWatchParams{Repo: "acme/app", PR: 10, Expect: "closed"}, `{"number":10,"state":"CLOSED"}`, true},
		{"approved pending when review required", GHPRWatchParams{Repo: "acme/app", PR: 10, Expect: "approved"}, `{"number":10,"state":"OPEN","reviewDecision":"REVIEW_REQUIRED"}`, false},
		{"approved triggers on APPROVED", GHPRWatchParams{Repo: "acme/app", PR: 10, Expect: "approved"}, `{"number":10,"state":"OPEN","reviewDecision":"APPROVED"}`, true},
		{"checks-green false on empty checks", GHPRWatchParams{Repo: "acme/app", PR: 10, Expect: "checks-green"}, `{"number":10,"state":"OPEN","statusCheckRollup":[]}`, false},
		{"checks-green false on pending check", GHPRWatchParams{Repo: "acme/app", PR: 10, Expect: "checks-green"}, `{"number":10,"state":"OPEN","statusCheckRollup":[{"name":"test","status":"IN_PROGRESS"}]}`, false},
		{"checks-green false on cancelled check", GHPRWatchParams{Repo: "acme/app", PR: 10, Expect: "checks-green"}, `{"number":10,"state":"OPEN","statusCheckRollup":[{"name":"test","status":"COMPLETED","conclusion":"CANCELLED"}]}`, false},
		{"checks-green true when all pass", GHPRWatchParams{Repo: "acme/app", PR: 10, Expect: "checks-green"}, `{"number":10,"state":"OPEN","statusCheckRollup":[{"name":"test","status":"COMPLETED","conclusion":"SUCCESS"},{"name":"lint","status":"COMPLETED","conclusion":"SKIPPED"}]}`, true},
		{"head-changed false on same sha prefix", GHPRWatchParams{Repo: "acme/app", PR: 10, HeadChangedFrom: "abc1234"}, `{"number":10,"state":"OPEN","headRefOid":"abc1234567890"}`, false},
		{"head-changed true on new sha", GHPRWatchParams{Repo: "acme/app", PR: 10, HeadChangedFrom: "abc1234"}, `{"number":10,"state":"OPEN","headRefOid":"def5678901234"}`, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prJSON = tc.json
			res, err := evalGHPRWatch(context.Background(), env, &tc.params)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if res.Triggered != tc.want {
				t.Fatalf("triggered=%v want %v (evidence=%q)", res.Triggered, tc.want, res.Evidence)
			}
		})
	}
}

func TestWatchKindGHRun(t *testing.T) {
	now := t0
	beads := map[string]Bead{"task-run": {ID: "task-run", Status: StatusInProgress}}
	env, _, _ := testWatchEnv(t, &now, beads)

	var runJSON string
	env.RunGH = func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte(runJSON), nil
	}

	cases := []struct {
		name   string
		expect string
		json   string
		want   bool
	}{
		{"in_progress never triggers", "completed", `{"databaseId":99,"status":"in_progress","conclusion":""}`, false},
		{"completed triggers on failure", "completed", `{"databaseId":99,"status":"completed","conclusion":"failure","workflowName":"Deploy"}`, true},
		{"completed triggers on success", "completed", `{"databaseId":99,"status":"completed","conclusion":"success","workflowName":"Deploy"}`, true},
		{"success ignores failure", "success", `{"databaseId":99,"status":"completed","conclusion":"failure"}`, false},
		{"success triggers on success", "success", `{"databaseId":99,"status":"completed","conclusion":"success","workflowName":"Deploy","url":"https://github.com/acme/app/actions/runs/99"}`, true},
		{"failure ignores success", "failure", `{"databaseId":99,"status":"completed","conclusion":"success"}`, false},
		{"failure triggers on timed_out", "failure", `{"databaseId":99,"status":"completed","conclusion":"timed_out","workflowName":"Deploy"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runJSON = tc.json
			res, err := evalGHRunWatch(context.Background(), env, &GHRunWatchParams{Repo: "acme/app", RunID: 99, Expect: tc.expect})
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if res.Triggered != tc.want {
				t.Fatalf("triggered=%v want %v", res.Triggered, tc.want)
			}
		})
	}
}

func TestWatchKindHTTP(t *testing.T) {
	var (
		mu         sync.Mutex
		statusCode = http.StatusServiceUnavailable
		body       = `{"service":{"ready":false},"items":[{"state":"booting"}],"count":0}`
		lastMethod string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastMethod = r.Method
		sc := statusCode
		b := body
		mu.Unlock()
		switch r.URL.Path {
		case "/status":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(sc)
			_, _ = w.Write([]byte(b))
		case "/same-scheme-redir":
			http.Redirect(w, r, "/status", http.StatusFound)
		case "/cross-scheme-redir":
			http.Redirect(w, r, "https://example.com/status", http.StatusFound)
		case "/huge":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(bytes.Repeat([]byte("a"), watchHTTPMaxBody+64))
		}
	}))
	defer srv.Close()

	// TLS server redirecting down to plain HTTP (cross-scheme downgrade).
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/status", http.StatusFound)
	}))
	defer tlsSrv.Close()

	env := WatchEnv{HTTPClient: srv.Client()}

	// Non-matching status (503) is pending, not an error.
	res, err := evalHTTPWatch(context.Background(), env, &HTTPWatchParams{
		URL:          srv.URL + "/status",
		ExpectStatus: "2xx",
		ExpectJSON:   "service.ready=true",
	})
	if err != nil || res.Triggered {
		t.Fatalf("503 should be pending without error: res=%+v err=%v", res, err)
	}
	mu.Lock()
	if lastMethod != http.MethodGet {
		t.Fatalf("method = %q, want GET", lastMethod)
	}
	mu.Unlock()

	// 200 with non-matching JSON stays pending.
	mu.Lock()
	statusCode = http.StatusOK
	mu.Unlock()
	res, err = evalHTTPWatch(context.Background(), env, &HTTPWatchParams{
		URL:          srv.URL + "/status",
		ExpectStatus: "200",
		ExpectJSON:   "service.ready=true",
	})
	if err != nil || res.Triggered {
		t.Fatalf("unready json should stay pending: res=%+v err=%v", res, err)
	}

	// Ready JSON + array index + substring all match.
	mu.Lock()
	statusCode = http.StatusCreated
	body = `{"service":{"ready":true},"items":[{"state":"healthy"}],"count":3,"msg":"deployment complete"}`
	mu.Unlock()
	res, err = evalHTTPWatch(context.Background(), env, &HTTPWatchParams{
		URL:            srv.URL + "/status",
		ExpectStatus:   "2xx",
		ExpectJSON:     "items.0.state=healthy",
		ExpectContains: "deployment complete",
	})
	if err != nil || !res.Triggered {
		t.Fatalf("expected HTTP match: res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Evidence, "HTTP 201") || !strings.Contains(res.Evidence, "items.0.state=healthy") {
		t.Fatalf("unexpected evidence: %q", res.Evidence)
	}

	// Same-scheme redirect succeeds.
	res, err = evalHTTPWatch(context.Background(), env, &HTTPWatchParams{
		URL:          srv.URL + "/same-scheme-redir",
		ExpectStatus: "2xx",
		ExpectJSON:   "count=3",
	})
	if err != nil || !res.Triggered {
		t.Fatalf("same-scheme redirect failed: res=%+v err=%v", res, err)
	}

	// Cross-scheme redirects (http->https and https->http) are refused.
	if _, err := evalHTTPWatch(context.Background(), env, &HTTPWatchParams{
		URL:          srv.URL + "/cross-scheme-redir",
		ExpectStatus: "200",
	}); err == nil || !strings.Contains(err.Error(), "cross-scheme") {
		t.Fatalf("expected cross-scheme error for http->https, got %v", err)
	}
	tlsEnv := WatchEnv{HTTPClient: tlsSrv.Client()}
	if _, err := evalHTTPWatch(context.Background(), tlsEnv, &HTTPWatchParams{
		URL:          tlsSrv.URL + "/",
		ExpectStatus: "200",
	}); err == nil || !strings.Contains(err.Error(), "cross-scheme") {
		t.Fatalf("expected cross-scheme error for https->http, got %v", err)
	}

	// Non-http(s) scheme refused at eval time too.
	if _, err := evalHTTPWatch(context.Background(), env, &HTTPWatchParams{
		URL:          "file:///etc/passwd",
		ExpectStatus: "200",
	}); err == nil {
		t.Fatal("accepted file:// URL at eval time")
	}

	// Oversized response body refused when body inspection is requested.
	if _, err := evalHTTPWatch(context.Background(), env, &HTTPWatchParams{
		URL:            srv.URL + "/huge",
		ExpectStatus:   "200",
		ExpectContains: "a",
	}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected body size limit error, got %v", err)
	}
}

func TestWatchKindCmdAllowGateAndNoShell(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := t0
	beads := map[string]Bead{"task-cmd": {ID: "task-cmd", Status: StatusBlocked}}
	env, _, _ := testWatchEnv(t, &now, beads)
	env.RunCmd = runWatchCmd

	// Build a tiny executable probe script in TempDir so we test real argv
	// execution with zero shell interpolation.
	dir := t.TempDir()
	probeScript := filepath.Join(dir, "probe.sh")
	scriptBody := "#!/bin/sh\n" +
		"if [ \"$1\" = \"sleep\" ]; then sleep 5; exit 0; fi\n" +
		"for a in \"$@\"; do printf 'ARG=[%s]\\n' \"$a\"; done\n" +
		"if [ \"$1\" = \"fail\" ]; then exit 2; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(probeScript, []byte(scriptBody), 0o700); err != nil {
		t.Fatal(err)
	}

	// Default config has allow_cmd = false -> refused at add time.
	cfgOff := defaultConfig()
	if cfgOff.Watches.AllowCmd {
		t.Fatal("default AllowCmd must be false")
	}
	var out bytes.Buffer
	if err := watchCmd(cfgOff, env, []string{"add", "task-cmd", "--kind", "cmd", "--cmd", probeScript}, &out); err == nil {
		t.Fatal("allowed adding cmd watch with allow_cmd=false")
	}

	// Enable allow_cmd = true -> add succeeds, timeout > 60s still rejected.
	cfgOn := defaultConfig()
	cfgOn.Watches.AllowCmd = true
	if err := watchCmd(cfgOn, env, []string{"add", "task-cmd", "--kind", "cmd", "--cmd", probeScript, "--timeout", "61s"}, &out); err == nil {
		t.Fatal("accepted cmd timeout above 60s")
	}

	// Verify shell metacharacters are passed verbatim as argv without interpolation.
	metacharArg := "$(echo injected); echo pwned"
	if err := watchCmd(cfgOn, env, []string{
		"add", "task-cmd",
		"--kind", "cmd",
		"--cmd", probeScript,
		"--arg", metacharArg,
		"--expect-exit", "0",
		"--expect-stdout", "ARG=[" + metacharArg + "]",
		"--timeout", "5s",
	}, &out); err != nil {
		t.Fatalf("add cmd watch: %v", err)
	}

	// Fail-closed at eval time if config later disables allow_cmd: must NOT execute.
	executed := false
	guardedEnv := env
	guardedEnv.RunCmd = func(ctx context.Context, prog string, args []string) (int, string, error) {
		executed = true
		return 0, "", nil
	}
	outcomes, err := runWatchesPass(cfgOff, guardedEnv, nil, nil, "task-cmd", true)
	if err != nil {
		t.Fatalf("eval with allow_cmd=false returned pass error: %v", err)
	}
	if executed {
		t.Fatal("cmd binary executed despite allow_cmd=false")
	}
	if len(outcomes) != 1 || outcomes[0].Action != "error" || !strings.Contains(outcomes[0].Watch.LastError, "allow_cmd") {
		t.Fatalf("expected allow_cmd refusal error, got %+v", outcomes)
	}

	// With allow_cmd=true, real execution matches exact literal argv without shell expansion.
	outcomes, err = runWatchesPass(cfgOn, env, nil, nil, "task-cmd", true)
	if err != nil || len(outcomes) != 1 || outcomes[0].Action != "triggered" {
		t.Fatalf("expected cmd trigger with literal argv, got outcomes=%+v err=%v", outcomes, err)
	}

	// Non-matching exit code stays pending (not error).
	res, err := evalCmdWatch(context.Background(), cfgOn, env, &CmdWatchParams{
		Cmd:        probeScript,
		Args:       []string{"fail"},
		ExpectExit: 0,
		Timeout:    "5s",
	})
	if err != nil || res.Triggered {
		t.Fatalf("non-matching exit code should stay pending: res=%+v err=%v", res, err)
	}

	// Matching nonzero exit code triggers.
	res, err = evalCmdWatch(context.Background(), cfgOn, env, &CmdWatchParams{
		Cmd:        probeScript,
		Args:       []string{"fail"},
		ExpectExit: 2,
		Timeout:    "5s",
	})
	if err != nil || !res.Triggered {
		t.Fatalf("expected exit 2 match: res=%+v err=%v", res, err)
	}

	// Timeout enforcement kills the slow command promptly and returns an error (never triggers).
	start := time.Now()
	res, err = evalCmdWatch(context.Background(), cfgOn, env, &CmdWatchParams{
		Cmd:        probeScript,
		Args:       []string{"sleep"},
		ExpectExit: 0,
		Timeout:    "150ms",
	})
	if err == nil || res.Triggered {
		t.Fatalf("expected timeout error without trigger, got res=%+v err=%v", res, err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cmd timeout took too long: %s", elapsed)
	}
}

func TestWatchOneShotExpiryRetirementAndBackoff(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := t0
	beads := map[string]Bead{
		"task-once":    {ID: "task-once", Status: StatusBlocked},
		"task-expire":  {ID: "task-expire", Status: StatusBlocked},
		"task-closed":  {ID: "task-closed", Status: StatusInProgress},
		"task-backoff": {ID: "task-backoff", Status: StatusBlocked},
	}
	env, notes, _ := testWatchEnv(t, &now, beads)

	var ghCalls atomic.Int32
	failBackoff := true
	env.RunGH = func(ctx context.Context, args ...string) ([]byte, error) {
		ghCalls.Add(1)
		target := strings.Join(args, " ")
		if strings.Contains(target, "acme/backoff") {
			if failBackoff {
				return nil, errors.New("HTTP 502 temporary outage")
			}
			return []byte(`{"tag_name":"v1.0.0"}`), nil
		}
		return []byte(`{"tag_name":"v2.0.0"}`), nil
	}

	cfg := defaultConfig()
	var out bytes.Buffer
	mustAdd := func(args ...string) {
		t.Helper()
		if err := watchCmd(cfg, env, append([]string{"add"}, args...), &out); err != nil {
			t.Fatalf("add %v: %v", args, err)
		}
	}
	mustAdd("task-once", "--kind", "gh-release", "--repo", "acme/once", "--after", "v1.0.0", "--every", "1m")
	mustAdd("task-expire", "--kind", "gh-release", "--repo", "acme/exp", "--after", "v1.0.0", "--every", "1m", "--expires", "10m")
	mustAdd("task-closed", "--kind", "gh-release", "--repo", "acme/closed", "--after", "v1.0.0", "--every", "1m")
	mustAdd("task-backoff", "--kind", "gh-release", "--repo", "acme/backoff", "--after", "v1.0.0", "--every", "1m")

	// 1) One-shot deduplication: first pass triggers task-once; second pass does not re-trigger.
	outcomes, err := runWatchesPass(cfg, env, nil, nil, "task-once", false)
	if err != nil || len(outcomes) != 1 || outcomes[0].Action != "triggered" {
		t.Fatalf("first check failed: %+v %v", outcomes, err)
	}
	callsAfterFirst := ghCalls.Load()
	outcomes, err = runWatchesPass(cfg, env, nil, nil, "task-once", true)
	if err != nil || len(outcomes) != 0 {
		t.Fatalf("second check re-ran triggered watch: %+v %v", outcomes, err)
	}
	if ghCalls.Load() != callsAfterFirst {
		t.Fatal("triggered watch invoked probe a second time")
	}

	// Verify inbox event and journal entry were written once.
	inboxItems, err := readInbox()
	if err != nil || len(inboxItems) != 1 || inboxItems[0].Kind != EventWatchTriggered {
		t.Fatalf("unexpected inbox items: %+v err=%v", inboxItems, err)
	}
	recs, err := readJournal(t0.Add(-time.Minute))
	if err != nil || len(recs) != 1 || recs[0].Kind != journalWatch || recs[0].Action != "triggered" {
		t.Fatalf("unexpected journal records: %+v err=%v", recs, err)
	}

	// 2) Closed-bead auto-retirement: mark task-closed closed before checking.
	beads["task-closed"] = Bead{ID: "task-closed", Status: StatusClosed}
	callsBeforeRetire := ghCalls.Load()
	outcomes, err = runWatchesPass(cfg, env, nil, nil, "task-closed", false)
	if err != nil || len(outcomes) != 1 || outcomes[0].Action != "retired" || outcomes[0].Watch.Status != WatchStatusRemoved {
		t.Fatalf("closed bead watch not retired: %+v err=%v", outcomes, err)
	}
	if ghCalls.Load() != callsBeforeRetire {
		t.Fatal("closed bead watch was probed before retirement")
	}

	// 3) Expiry: advance clock past expires_at; watch expires without probing.
	now = t0.Add(11 * time.Minute)
	callsBeforeExpire := ghCalls.Load()
	outcomes, err = runWatchesPass(cfg, env, nil, nil, "task-expire", false)
	if err != nil || len(outcomes) != 1 || outcomes[0].Action != "expired" || outcomes[0].Watch.Status != WatchStatusExpired {
		t.Fatalf("expired watch not marked expired: %+v err=%v", outcomes, err)
	}
	if ghCalls.Load() != callsBeforeExpire {
		t.Fatal("expired watch was probed")
	}
	inboxItems, _ = readInbox()
	foundExpired := false
	for _, it := range inboxItems {
		if it.Bead == "task-expire" && it.Kind == EventWatchExpired {
			foundExpired = true
		}
	}
	if !foundExpired {
		t.Fatalf("missing EventWatchExpired in inbox: %+v", inboxItems)
	}

	// 4) Error backoff: consecutive failures double backoff and never trigger.
	now = t0.Add(12 * time.Minute)
	outcomes, err = runWatchesPass(cfg, env, nil, nil, "task-backoff", false)
	if err != nil || len(outcomes) != 1 || outcomes[0].Action != "error" {
		t.Fatalf("first error pass: %+v err=%v", outcomes, err)
	}
	w1 := outcomes[0].Watch
	if w1.ConsecutiveErrors != 1 || w1.LastError == "" || !w1.NextCheck.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("unexpected 1st backoff: %+v", w1)
	}
	// Not due yet at +1m.
	now = now.Add(time.Minute)
	outcomes, err = runWatchesPass(cfg, env, nil, nil, "task-backoff", false)
	if err != nil || len(outcomes) != 0 {
		t.Fatalf("watch ran during backoff window: %+v %v", outcomes, err)
	}
	// Second error at +2m pushes backoff to 4m.
	now = now.Add(time.Minute)
	outcomes, err = runWatchesPass(cfg, env, nil, nil, "task-backoff", false)
	if err != nil || len(outcomes) != 1 || outcomes[0].Action != "error" {
		t.Fatalf("second error pass: %+v %v", outcomes, err)
	}
	if outcomes[0].Watch.ConsecutiveErrors != 2 || !outcomes[0].Watch.NextCheck.Equal(now.Add(4*time.Minute)) {
		t.Fatalf("unexpected 2nd backoff: %+v", outcomes[0].Watch)
	}
	// Backoff is strictly bounded even at high error counts.
	if b := watchBackoff(time.Minute, 50); b != time.Hour {
		t.Fatalf("watchBackoff(1m, 50) = %s, want 1h cap", b)
	}
	// Recovery clears consecutive_errors and last_error.
	failBackoff = false
	now = now.Add(4 * time.Minute)
	outcomes, err = runWatchesPass(cfg, env, nil, nil, "task-backoff", false)
	if err != nil || len(outcomes) != 1 || outcomes[0].Action != "pending" {
		t.Fatalf("recovery pass: %+v %v", outcomes, err)
	}
	if outcomes[0].Watch.ConsecutiveErrors != 0 || outcomes[0].Watch.LastError != "" || !outcomes[0].Watch.NextCheck.Equal(now.Add(time.Minute)) {
		t.Fatalf("recovery did not reset error state: %+v", outcomes[0].Watch)
	}
	if len(*notes) < 2 {
		t.Fatalf("expected trigger and expiry notes, got %+v", *notes)
	}
}

func TestWatchWakeSafety(t *testing.T) {
	cases := []struct {
		name           string
		beadStatus     string
		beadNotes      string
		agent          *Agent
		wantWoken      bool
		wantReasonPart string
	}{
		{
			name:       "wakes idle worker on in_progress bead",
			beadStatus: StatusInProgress,
			agent:      &Agent{Name: "task-wake", Kind: "pi", Status: "idle", PaneID: "w1:p1"},
			wantWoken:  true,
		},
		{
			name:       "wakes idle worker on blocked bead",
			beadStatus: StatusBlocked,
			agent:      &Agent{Name: "task-wake", Kind: "claude", Status: "done", PaneID: "w1:p1"},
			wantWoken:  true,
		},
		{
			name:           "refuses to wake needs_me bead even with idle worker",
			beadStatus:     StatusNeedsMe,
			beadNotes:      "QUESTION: Deploy now?\nOPTIONS:\n1. Yes\n2. No\nCLASS: 6 — deploy",
			agent:          &Agent{Name: "task-wake", Kind: "pi", Status: "idle", PaneID: "w1:p1"},
			wantWoken:      false,
			wantReasonPart: "needs_me",
		},
		{
			name:           "refuses to interrupt working worker",
			beadStatus:     StatusInProgress,
			agent:          &Agent{Name: "task-wake", Kind: "pi", Status: "working", PaneID: "w1:p1"},
			wantWoken:      false,
			wantReasonPart: "working",
		},
		{
			name:           "refuses to poke blocked worker at approval prompt",
			beadStatus:     StatusInProgress,
			agent:          &Agent{Name: "task-wake", Kind: "claude", Status: "blocked", PaneID: "w1:p1"},
			wantWoken:      false,
			wantReasonPart: "blocked",
		},
		{
			name:           "handles missing worker gracefully",
			beadStatus:     StatusBlocked,
			agent:          nil,
			wantWoken:      false,
			wantReasonPart: "no worker running",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
			now := t0
			beads := map[string]Bead{
				"task-wake": {ID: "task-wake", Title: "Wait for upstream", Status: tc.beadStatus, Notes: tc.beadNotes},
			}
			questionBefore := asked(beads["task-wake"])

			env, notes, tells := testWatchEnv(t, &now, beads)
			env.RunGH = func(ctx context.Context, args ...string) ([]byte, error) {
				return []byte(`{"tag_name":"v2.0.0"}`), nil
			}
			cfg := defaultConfig()
			var out bytes.Buffer
			if err := watchCmd(cfg, env, []string{
				"add", "task-wake",
				"--kind", "gh-release",
				"--repo", "acme/app",
				"--after", "v1.0.0",
				"--on-trigger", "wake",
				"--label", "upstream v2",
			}, &out); err != nil {
				t.Fatal(err)
			}

			threads := []Thread{{Bead: beads["task-wake"], Agent: tc.agent}}
			agents := map[string]Agent{}
			if tc.agent != nil {
				agents[tc.agent.Name] = *tc.agent
			}

			outcomes, err := runWatchesPass(cfg, env, threads, agents, "task-wake", true)
			if err != nil || len(outcomes) != 1 || outcomes[0].Action != "triggered" {
				t.Fatalf("unexpected outcomes=%+v err=%v", outcomes, err)
			}

			// Inbox event is ALWAYS emitted regardless of whether worker was woken.
			items, err := readInbox()
			if err != nil || len(items) != 1 || items[0].Kind != EventWatchTriggered {
				t.Fatalf("expected 1 watch_triggered inbox event, got %+v err=%v", items, err)
			}
			if len(*notes) != 1 || !strings.Contains((*notes)[0], "WATCH TRIGGERED [") {
				t.Fatalf("expected bead note, got %+v", *notes)
			}

			if tc.wantWoken {
				if len(*tells) != 1 {
					t.Fatalf("expected 1 wake prompt, got %+v", *tells)
				}
				if !strings.Contains((*tells)[0], watchWakeHeaderPrefix) {
					t.Fatalf("wake prompt missing non-approving automated header: %q", (*tells)[0])
				}
			} else {
				if len(*tells) != 0 {
					t.Fatalf("worker was woken unsafely: %+v", *tells)
				}
				if !strings.Contains(items[0].Summary, "worker not prompted") || !strings.Contains(items[0].Summary, tc.wantReasonPart) {
					t.Fatalf("inbox summary missing skip reason %q: %q", tc.wantReasonPart, items[0].Summary)
				}
			}

			// For needs_me beads, watch note must NOT alter the active question fingerprint.
			if tc.beadStatus == StatusNeedsMe {
				if got := asked(beads["task-wake"]); got != questionBefore {
					t.Fatalf("watch note clobbered needs_me question fingerprint: %q -> %q", questionBefore, got)
				}
			}
		})
	}
}

func TestWatchConcurrentStoreSafetyAndBoundedConcurrency(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := t0
	beads := map[string]Bead{}
	for i := range 8 {
		id := fmt.Sprintf("task-%d", i)
		beads[id] = Bead{ID: id, Status: StatusBlocked}
	}
	env, _, _ := testWatchEnv(t, &now, beads)

	var inFlight, maxSeen atomic.Int32
	env.RunGH = func(ctx context.Context, args ...string) ([]byte, error) {
		cur := inFlight.Add(1)
		for {
			prev := maxSeen.Load()
			if cur <= prev || maxSeen.CompareAndSwap(prev, cur) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
		inFlight.Add(-1)
		return []byte(`{"tag_name":"v2.0.0"}`), nil
	}

	cfg := defaultConfig()
	cfg.Watches.MaxConcurrent = 2

	// Concurrently add 8 watches from separate goroutines.
	var addWG sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		addWG.Add(1)
		go func(i int) {
			defer addWG.Done()
			var buf bytes.Buffer
			err := watchCmd(cfg, env, []string{
				"add", fmt.Sprintf("task-%d", i),
				"--kind", "gh-release",
				"--repo", fmt.Sprintf("acme/repo-%d", i),
				"--after", "v1.0.0",
			}, &buf)
			if err != nil {
				errs <- err
			}
		}(i)
	}
	addWG.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent add failed: %v", err)
	}

	ws, err := loadWatches()
	if err != nil || len(ws) != 8 {
		t.Fatalf("expected 8 watches after concurrent add, got %d (err=%v)", len(ws), err)
	}

	// Run two overlapping evaluation passes concurrently: each watch must trigger
	// at most once and bounded concurrency must cap per-pass parallelism.
	var evalWG sync.WaitGroup
	var triggeredTotal atomic.Int32
	for range 2 {
		evalWG.Add(1)
		go func() {
			defer evalWG.Done()
			outcomes, err := runWatchesPass(cfg, env, nil, nil, "", true)
			if err != nil {
				t.Errorf("concurrent runWatchesPass: %v", err)
				return
			}
			for _, o := range outcomes {
				if o.Action == "triggered" {
					triggeredTotal.Add(1)
				}
			}
		}()
	}
	evalWG.Wait()
	if got := triggeredTotal.Load(); got != 8 {
		t.Fatalf("expected exactly 8 one-shot triggers across overlapping passes, got %d", got)
	}

	// Single-pass bounded concurrency check with MaxConcurrent=2.
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	maxSeen.Store(0)
	inFlight.Store(0)
	for i := range 6 {
		var buf bytes.Buffer
		if err := watchCmd(cfg, env, []string{
			"add", fmt.Sprintf("task-%d", i),
			"--kind", "gh-release",
			"--repo", fmt.Sprintf("acme/cap-%d", i),
			"--after", "v1.0.0",
		}, &buf); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runWatchesPass(cfg, env, nil, nil, "", true); err != nil {
		t.Fatal(err)
	}
	if got := maxSeen.Load(); got > 2 {
		t.Fatalf("max concurrent watch probes = %d, want <= 2", got)
	}
}

func TestWatchInboxConditionAndContextAndTicker(t *testing.T) {
	tk, st, _ := inboxTickerFixture(t)
	now := time.Now().UTC()
	t.Setenv("FIXTURE_ACTIVE", `[{"id":"task-one","title":"Deploy API","status":"blocked"}]`)
	t.Setenv("FAKE_LOCAL_AGENTS", `[{"name":"task-one","agent":"pi","agent_status":"idle","pane_id":"w1:p1"}]`)

	beads := map[string]Bead{
		"task-one": {ID: "task-one", Title: "Deploy API", Status: StatusBlocked},
	}
	env, notes, tells := testWatchEnv(t, &now, beads)

	ready := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ready {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ready":true}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"ready":false}`))
	}))
	defer srv.Close()
	env.HTTPClient = srv.Client()

	var out bytes.Buffer
	if err := watchCmd(tk.cfg, env, []string{
		"add", "task-one",
		"--kind", "http",
		"--url", srv.URL + "/health",
		"--expect-status", "200",
		"--expect-json", "ready=true",
		"--every", "1m",
		"--on-trigger", "wake",
		"--label", "api health",
	}, &out); err != nil {
		t.Fatalf("watch add: %v", err)
	}

	// quartermaster context shows a concise watches: line on the thread.
	th := Thread{
		Bead:  beads["task-one"],
		Agent: &Agent{Name: "task-one", Kind: "pi", Status: "idle", PaneID: "w1:p1"},
	}
	ctxText := renderContext(tk.cfg, []Thread{th}, nil, nil, nil, *st, nil, now)
	if !strings.Contains(ctxText, "  watches: [") || !strings.Contains(ctxText, "api health") || !strings.Contains(ctxText, "wake") {
		t.Fatalf("context missing watches line:\n%s", ctxText)
	}

	// First ticker pass: endpoint not ready -> remains active, no inbox event yet.
	tk.watchEnv = &env
	if err := tk.once(st); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if items, _ := readInbox(); len(items) != 0 {
		t.Fatalf("unexpected inbox event before ready: %+v", items)
	}

	// Endpoint becomes ready and interval elapses -> ticker triggers, wakes idle worker, writes inbox+note.
	ready = true
	now = now.Add(2 * time.Minute)
	if err := tk.once(st); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	items, err := readInbox()
	if err != nil || len(items) != 1 || items[0].Kind != EventWatchTriggered {
		t.Fatalf("expected watch_triggered inbox item, got %+v err=%v", items, err)
	}
	if len(*tells) != 1 || !strings.Contains((*tells)[0], watchWakeHeaderPrefix) {
		t.Fatalf("expected worker wake prompt, got %+v", *tells)
	}
	if len(*notes) != 1 || !strings.Contains((*notes)[0], "WATCH TRIGGERED [") {
		t.Fatalf("expected watch note, got %+v", *notes)
	}

	// Watch event stays unread while bead is open/blocked, and archives once bead closes.
	if err := reconcileInbox(nil, func(id string) (Bead, error) {
		return Bead{ID: id, Status: StatusOpen}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if items, _ := readInbox(); len(items) != 1 {
		t.Fatalf("watch_triggered archived prematurely on open bead: %+v", items)
	}
	if err := reconcileInbox(nil, func(id string) (Bead, error) {
		return Bead{ID: id, Status: StatusClosed}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if items, _ := readInbox(); len(items) != 0 {
		t.Fatalf("watch_triggered not archived after bead closed: %+v", items)
	}

	// Once triggered, context no longer renders an active watches line for task-one.
	ctxAfter := renderContext(tk.cfg, []Thread{th}, nil, nil, nil, *st, nil, now)
	if strings.Contains(ctxAfter, "watches:") {
		t.Fatalf("triggered watch still shown as active in context:\n%s", ctxAfter)
	}
}

func TestWatchConfigTOML(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QUARTERMASTER_CONFIG_DIR", dir)

	// Default omits [watches] -> allow_cmd is false.
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("repo = \"/tmp/repo\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig default: %v", err)
	}
	if cfg.Watches.AllowCmd {
		t.Fatal("default [watches] allow_cmd must be false")
	}

	// Explicit [watches] allow_cmd = true and max_concurrent.
	tomlCfg := "repo = \"/tmp/repo\"\n\n[watches]\nallow_cmd = true\nmax_concurrent = 3\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(tomlCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadConfig()
	if err != nil {
		t.Fatalf("loadConfig allow_cmd=true: %v", err)
	}
	if !cfg.Watches.AllowCmd || cfg.Watches.concurrency() != 3 {
		t.Fatalf("unexpected watches config: %+v", cfg.Watches)
	}

	// Negative max_concurrent is rejected.
	badCfg := "repo = \"/tmp/repo\"\n\n[watches]\nmax_concurrent = -1\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(badCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil {
		t.Fatal("accepted negative [watches] max_concurrent")
	}
}

func TestWatchCLICheckAndHumanListOutput(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := t0
	beads := map[string]Bead{"task-cli": {ID: "task-cli", Status: StatusBlocked}}
	env, _, _ := testWatchEnv(t, &now, beads)

	tag := "v1.0.0"
	env.RunGH = func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte(fmt.Sprintf(`{"tag_name":%q}`, tag)), nil
	}
	cfg := defaultConfig()

	var out bytes.Buffer
	if err := watchCmd(cfg, env, []string{"help"}, &out); err != nil || !strings.Contains(out.String(), "quartermaster watch add") {
		t.Fatalf("watch help: out=%q err=%v", out.String(), err)
	}
	out.Reset()
	if err := watchCmd(cfg, env, []string{"add", "task-cli", "--kind", "gh-release", "--repo", "acme/app", "--after", "v1.0.0", "--every", "5m"}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := watchCmd(cfg, env, []string{"list"}, &out); err != nil || !strings.Contains(out.String(), "task-cli") || !strings.Contains(out.String(), "active") {
		t.Fatalf("human list output: %q err=%v", out.String(), err)
	}

	// First check is due immediately -> PENDING.
	out.Reset()
	if err := watchCmd(cfg, env, []string{"check", "task-cli"}, &out); err != nil || !strings.Contains(out.String(), "PENDING") {
		t.Fatalf("first check: %q err=%v", out.String(), err)
	}
	// Second check without --force before interval -> no due watches.
	out.Reset()
	if err := watchCmd(cfg, env, []string{"check", "task-cli"}, &out); err != nil || !strings.Contains(out.String(), "no due watches") {
		t.Fatalf("not-due check: %q err=%v", out.String(), err)
	}
	// With --force after release lands -> TRIGGERED.
	tag = "v1.1.0"
	out.Reset()
	if err := watchCmd(cfg, env, []string{"check", "task-cli", "--force"}, &out); err != nil || !strings.Contains(out.String(), "TRIGGERED") {
		t.Fatalf("forced trigger check: %q err=%v", out.String(), err)
	}
}
