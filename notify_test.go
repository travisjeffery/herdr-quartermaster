package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func boolPtr(v bool) *bool { return &v }

func setupNotifyTestEnv(t *testing.T) (Config, string, string) {
	t.Helper()
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "config")
	stDir := filepath.Join(dir, "state")
	repoDir := filepath.Join(dir, "repo")
	for _, d := range []string{cfgDir, stDir, repoDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("QUARTERMASTER_CONFIG_DIR", cfgDir)
	t.Setenv("QUARTERMASTER_STATE_DIR", stDir)
	cfg := defaultConfig()
	cfg.Repo = repoDir
	cfg.BeadsDir = filepath.Join(repoDir, ".beads")
	return cfg, cfgDir, stDir
}

func TestNotifyConfigValidation(t *testing.T) {
	_, cfgDir, _ := setupNotifyTestEnv(t)
	repoDir := filepath.Join(filepath.Dir(cfgDir), "repo")

	writeCfg := func(body string) (Config, error) {
		full := fmt.Sprintf("repo = %q\n%s\n", repoDir, body)
		if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(full), 0o644); err != nil {
			t.Fatal(err)
		}
		return loadConfig()
	}

	// Default config has [notify] empty and disabled.
	cfg, err := writeCfg("")
	if err != nil {
		t.Fatalf("default loadConfig: %v", err)
	}
	if cfg.Notify.enabled() || cfg.Notify.hasSinks() {
		t.Fatalf("expected [notify] disabled by default, got %+v", cfg.Notify)
	}

	// Valid webhooks (url, url_env, json, text, timeout capped at 30, cooldown 0) and command.
	cfg, err = writeCfg(`
[[notify.webhooks]]
name = "ops-json"
url_env = "OPS_WEBHOOK_URL"
headers = { X-Source = "quartermaster" }
header_envs = { Authorization = "OPS_AUTH_HEADER" }
events = ["pr_merged", "needs_me", "inbox_runner"]
format = "json"
timeout_seconds = 60
cooldown_seconds = 0

[[notify.webhooks]]
name = "ntfy-text"
url = "https://ntfy.example.com/alerts"
format = "text"

[[notify.commands]]
name = "local-cli"
argv = ["/usr/local/bin/notify-bridge", "--stdin"]
events = ["blocked", "worker_blocked"]
timeout_seconds = 10
cooldown_seconds = 45
`)
	if err != nil {
		t.Fatalf("valid [notify] config failed: %v", err)
	}
	if !cfg.Notify.enabled() || len(cfg.Notify.Webhooks) != 2 || len(cfg.Notify.Commands) != 1 {
		t.Fatalf("unexpected parsed [notify]: %+v", cfg.Notify)
	}
	if got := cfg.Notify.Webhooks[0].timeout(); got != 30*time.Second {
		t.Fatalf("expected timeout_seconds capped at 30s, got %s", got)
	}
	if got := cfg.Notify.Webhooks[0].cooldown(); got != 0 {
		t.Fatalf("expected cooldown 0, got %s", got)
	}
	if got := cfg.Notify.Webhooks[1].timeout(); got != 5*time.Second {
		t.Fatalf("expected default timeout 5s, got %s", got)
	}
	if got := cfg.Notify.Webhooks[1].cooldown(); got != 30*time.Second {
		t.Fatalf("expected default cooldown 30s, got %s", got)
	}
	if !slices.Equal(cfg.Notify.Webhooks[1].effectiveEvents(), notifyAllEvents) {
		t.Fatalf("expected default all events, got %v", cfg.Notify.Webhooks[1].effectiveEvents())
	}

	// Explicit enabled = false disables delivery while preserving config validation.
	cfg, err = writeCfg(`
[notify]
enabled = false

[[notify.webhooks]]
name = "paused"
url = "https://hooks.example.com/p"
`)
	if err != nil {
		t.Fatalf("paused [notify] failed: %v", err)
	}
	if !cfg.Notify.hasSinks() || cfg.Notify.enabled() {
		t.Fatalf("expected hasSinks=true, enabled=false; got %+v", cfg.Notify)
	}

	// Invalid configs must fail closed.
	invalid := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "missing webhook name",
			toml: "[[notify.webhooks]]\nurl = \"https://hooks.example.com/a\"",
			want: "name must be non-empty",
		},
		{
			name: "duplicate webhook names",
			toml: "[[notify.webhooks]]\nname = \"dup\"\nurl = \"https://hooks.example.com/a\"\n[[notify.webhooks]]\nname = \"dup\"\nurl = \"https://hooks.example.com/b\"",
			want: "duplicate sink name",
		},
		{
			name: "duplicate name across webhook and command",
			toml: "[[notify.webhooks]]\nname = \"shared\"\nurl = \"https://hooks.example.com/a\"\n[[notify.commands]]\nname = \"shared\"\nargv = [\"true\"]",
			want: "duplicate sink name",
		},
		{
			name: "neither url nor url_env",
			toml: "[[notify.webhooks]]\nname = \"missing-url\"",
			want: "exactly one of url or url_env",
		},
		{
			name: "both url and url_env",
			toml: "[[notify.webhooks]]\nname = \"both-url\"\nurl = \"https://hooks.example.com/a\"\nurl_env = \"HOOK_URL\"",
			want: "exactly one of url or url_env",
		},
		{
			name: "disallowed file scheme",
			toml: "[[notify.webhooks]]\nname = \"bad-scheme\"\nurl = \"file:///etc/passwd\"",
			want: "disallowed URL scheme",
		},
		{
			name: "disallowed ftp scheme",
			toml: "[[notify.webhooks]]\nname = \"ftp-scheme\"\nurl = \"ftp://hooks.example.com/a\"",
			want: "disallowed URL scheme",
		},
		{
			name: "missing host",
			toml: "[[notify.webhooks]]\nname = \"no-host\"\nurl = \"https:///path\"",
			want: "host",
		},
		{
			name: "invalid url_env name",
			toml: "[[notify.webhooks]]\nname = \"bad-env\"\nurl_env = \"NOT-VALID\"",
			want: "invalid url_env",
		},
		{
			name: "unknown event",
			toml: "[[notify.webhooks]]\nname = \"bad-ev\"\nurl = \"https://hooks.example.com/a\"\nevents = [\"pr_merged\", \"bogus_event\"]",
			want: "unknown event",
		},
		{
			name: "empty events list",
			toml: "[[notify.webhooks]]\nname = \"empty-ev\"\nurl = \"https://hooks.example.com/a\"\nevents = []",
			want: "events must not be empty",
		},
		{
			name: "unknown format",
			toml: "[[notify.webhooks]]\nname = \"bad-fmt\"\nurl = \"https://hooks.example.com/a\"\nformat = \"xml\"",
			want: "unknown format",
		},
		{
			name: "negative timeout",
			toml: "[[notify.webhooks]]\nname = \"neg-t\"\nurl = \"https://hooks.example.com/a\"\ntimeout_seconds = -1",
			want: "timeout_seconds",
		},
		{
			name: "negative cooldown",
			toml: "[[notify.webhooks]]\nname = \"neg-cd\"\nurl = \"https://hooks.example.com/a\"\ncooldown_seconds = -5",
			want: "cooldown_seconds",
		},
		{
			name: "conflicting header and header_envs",
			toml: "[[notify.webhooks]]\nname = \"hdr-conflict\"\nurl = \"https://hooks.example.com/a\"\nheaders = { authorization = \"Bearer x\" }\nheader_envs = { Authorization = \"AUTH_TOKEN\" }",
			want: "conflicts",
		},
		{
			name: "invalid header_envs var",
			toml: "[[notify.webhooks]]\nname = \"bad-hdr-env\"\nurl = \"https://hooks.example.com/a\"\nheader_envs = { Authorization = \"123BAD\" }",
			want: "invalid env var",
		},
		{
			name: "header newline injection",
			toml: "[[notify.webhooks]]\nname = \"crlf-hdr\"\nurl = \"https://hooks.example.com/a\"\nheaders = { X-Test = \"ok\\r\\nInjected: 1\" }",
			want: "must not contain CR or LF",
		},
		{
			name: "command empty argv",
			toml: "[[notify.commands]]\nname = \"no-argv\"\nargv = []",
			want: "argv must be non-empty",
		},
		{
			name: "command negative timeout",
			toml: "[[notify.commands]]\nname = \"cmd-neg-t\"\nargv = [\"true\"]\ntimeout_seconds = -2",
			want: "timeout_seconds",
		},
		{
			name: "command unknown event",
			toml: "[[notify.commands]]\nname = \"cmd-ev\"\nargv = [\"true\"]\nevents = [\"nope\"]",
			want: "unknown event",
		},
	}

	for _, tc := range invalid {
		if _, err := writeCfg(tc.toml); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want substring %q", tc.name, err, tc.want)
		}
	}
}

func TestNotifyWebhookJSONTextAndRedaction(t *testing.T) {
	cfg, _, stDir := setupNotifyTestEnv(t)
	now := time.Date(2026, 11, 24, 16, 0, 0, 0, time.UTC)

	var (
		mu          sync.Mutex
		gotJSON     NotifyEnvelope
		gotJSONCT   string
		gotAuth     string
		gotSource   string
		gotTextBody string
		gotTextCT   string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/secret/json-hook":
			gotJSONCT = r.Header.Get("Content-Type")
			gotAuth = r.Header.Get("Authorization")
			gotSource = r.Header.Get("X-Source")
			_ = json.Unmarshal(body, &gotJSON)
			w.WriteHeader(http.StatusAccepted)
		case "/secret/text-hook":
			gotTextCT = r.Header.Get("Content-Type")
			gotTextBody = string(body)
			w.WriteHeader(http.StatusOK)
		case "/secret/fail-hook":
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, "rejected %s?%s with %s", r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"))
		case "/secret/slow-hook":
			select {
			case <-time.After(3 * time.Second):
				w.WriteHeader(http.StatusOK)
			case <-r.Context().Done():
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	secretToken := "Bearer super-secret-bearer-token-999"
	secretQuery := "api_token=query-secret-777"
	t.Setenv("TEST_JSON_WEBHOOK_URL", srv.URL+"/secret/json-hook?"+secretQuery)
	t.Setenv("TEST_AUTH_HEADER", secretToken)
	t.Setenv("TEST_FAIL_WEBHOOK_URL", srv.URL+"/secret/fail-hook?"+secretQuery)

	cfg.Notify = NotifyConfig{
		Webhooks: []NotifyWebhookConfig{
			{
				Name:            "json-sink",
				URLEnv:          "TEST_JSON_WEBHOOK_URL",
				Headers:         map[string]string{"X-Source": "qm-test"},
				HeaderEnvs:      map[string]string{"Authorization": "TEST_AUTH_HEADER"},
				Format:          "json",
				CooldownSeconds: intPtr(0),
			},
			{
				Name:            "text-sink",
				URL:             srv.URL + "/secret/text-hook?" + secretQuery,
				Format:          "text",
				CooldownSeconds: intPtr(0),
			},
			{
				Name:            "fail-sink",
				URLEnv:          "TEST_FAIL_WEBHOOK_URL",
				HeaderEnvs:      map[string]string{"Authorization": "TEST_AUTH_HEADER"},
				CooldownSeconds: intPtr(0),
			},
			{
				Name:            "timeout-sink",
				URL:             srv.URL + "/secret/slow-hook?" + secretQuery,
				TimeoutSeconds:  intPtr(1),
				CooldownSeconds: intPtr(0),
			},
			{
				Name:            "empty-env-sink",
				URLEnv:          "UNSET_NOTIFY_WEBHOOK_URL",
				CooldownSeconds: intPtr(0),
			},
		},
	}

	var (
		logMu sync.Mutex
		logs  bytes.Buffer
	)
	env := liveNotifyEnv(cfg, func(format string, args ...any) {
		logMu.Lock()
		fmt.Fprintf(&logs, format+"\n", args...)
		logMu.Unlock()
	})
	env.now = func() time.Time { return now }

	milestone := desktopMilestone{
		Key:     desktopMilestoneKey("pr_merged", "task-42", "https://github.com/acme/app/pull/42"),
		Bead:    "task-42",
		Reason:  "pr_merged",
		Summary: "task-42: acme/app#42 merged — Add caching",
		PR:      "acme/app#42",
	}
	if err := reconcileNotifyWith(context.Background(), cfg, env, nil, []desktopMilestone{milestone}, now, nil); err != nil {
		t.Fatalf("reconcileNotifyWith: %v", err)
	}

	mu.Lock()
	if gotJSON.Version != 1 || gotJSON.Event != "pr_merged" || gotJSON.At != now.Format(time.RFC3339) {
		t.Fatalf("unexpected JSON envelope: %+v", gotJSON)
	}
	if gotJSON.Title != "Quartermaster: PR merged" || !strings.Contains(gotJSON.Body, "acme/app#42 merged") {
		t.Fatalf("unexpected JSON title/body: %q / %q", gotJSON.Title, gotJSON.Body)
	}
	if len(gotJSON.Items) != 1 || gotJSON.Items[0].Bead != "task-42" || gotJSON.Items[0].PR != "acme/app#42" {
		t.Fatalf("unexpected JSON items: %+v", gotJSON.Items)
	}
	if !strings.HasPrefix(gotJSONCT, "application/json") || gotAuth != secretToken || gotSource != "qm-test" {
		t.Fatalf("unexpected headers: ct=%q auth=%q source=%q", gotJSONCT, gotAuth, gotSource)
	}
	wantText := "Quartermaster: PR merged\ntask-42: acme/app#42 merged — Add caching"
	if gotTextBody != wantText || !strings.HasPrefix(gotTextCT, "text/plain") {
		t.Fatalf("unexpected text delivery: ct=%q body=%q (want %q)", gotTextCT, gotTextBody, wantText)
	}
	mu.Unlock()

	// Verify directory 0700 and status.json 0600 permissions.
	dirInfo, err := os.Stat(filepath.Join(stDir, "notify"))
	if err != nil {
		t.Fatalf("stat notify dir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("notify dir mode = %o, want 0700", dirInfo.Mode().Perm())
	}
	stInfo, err := os.Stat(notifyStatusPath())
	if err != nil {
		t.Fatalf("stat status.json: %v", err)
	}
	if stInfo.Mode().Perm() != 0o600 {
		t.Fatalf("status.json mode = %o, want 0600", stInfo.Mode().Perm())
	}

	// Verify status file states and zero secret/URL/query/path leaks anywhere.
	rawStatus, err := os.ReadFile(notifyStatusPath())
	if err != nil {
		t.Fatalf("read status.json: %v", err)
	}
	var statusOut, statusJSON bytes.Buffer
	if err := notifyCmd(cfg, env, []string{"status"}, &statusOut); err != nil {
		t.Fatalf("notify status: %v", err)
	}
	if err := notifyCmd(cfg, env, []string{"status", "--json"}, &statusJSON); err != nil {
		t.Fatalf("notify status --json: %v", err)
	}
	allSurfaces := string(rawStatus) + "\n" + statusOut.String() + "\n" + statusJSON.String() + "\n" + logs.String()
	for _, leak := range []string{
		"super-secret-bearer-token-999",
		"query-secret-777",
		"/secret/json-hook",
		"/secret/text-hook",
		"/secret/fail-hook",
		"/secret/slow-hook",
	} {
		if strings.Contains(allSurfaces, leak) {
			t.Fatalf("secret/URL leaked %q into status/logs:\n%s", leak, allSurfaces)
		}
	}

	var rep NotifyStatusReport
	if err := json.Unmarshal(statusJSON.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal status --json: %v", err)
	}
	byName := map[string]NotifySinkStatus{}
	for _, s := range rep.Sinks {
		byName[s.Name] = s
	}
	if byName["json-sink"].Target != "env:TEST_JSON_WEBHOOK_URL" || byName["json-sink"].LastState != "submitted" || byName["json-sink"].TotalSent != 1 {
		t.Fatalf("unexpected json-sink status: %+v", byName["json-sink"])
	}
	if byName["text-sink"].Target != srv.URL || byName["text-sink"].LastState != "submitted" {
		t.Fatalf("unexpected text-sink status: %+v (want target %q)", byName["text-sink"], srv.URL)
	}
	if byName["fail-sink"].LastState != "failed" || byName["fail-sink"].TotalFailed != 1 || !strings.Contains(byName["fail-sink"].LastError, "HTTP 401") {
		t.Fatalf("unexpected fail-sink status: %+v", byName["fail-sink"])
	}
	if byName["timeout-sink"].LastState != "timeout" || byName["timeout-sink"].TotalFailed != 1 {
		t.Fatalf("unexpected timeout-sink status: %+v", byName["timeout-sink"])
	}
	if byName["empty-env-sink"].LastState != "failed" || !strings.Contains(byName["empty-env-sink"].LastError, "UNSET_NOTIFY_WEBHOOK_URL") {
		t.Fatalf("unexpected empty-env-sink status: %+v", byName["empty-env-sink"])
	}

	// Runtime fail-closed on disallowed scheme in url_env.
	t.Setenv("BAD_SCHEME_URL", "file:///etc/passwd")
	res := deliverWebhook(context.Background(), NotifyWebhookConfig{
		Name:   "bad-runtime",
		URLEnv: "BAD_SCHEME_URL",
	}, NotifyEnvelope{Version: 1, Event: "test", Title: "hi"}, env)
	if res.State != "failed" || !strings.Contains(res.Err, "disallowed URL scheme") || strings.Contains(res.Err, "/etc/passwd") {
		t.Fatalf("expected fail-closed scheme check without path leak, got %+v", res)
	}
}

func TestNotifyCommandSinkStdinExitAndTimeoutCleanup(t *testing.T) {
	setupNotifyTestEnv(t)
	tmp := t.TempDir()

	// 1. Successful script captures stdin JSON and verifies no shell evaluation of argv.
	captureFile := filepath.Join(tmp, "payload.json")
	argFile := filepath.Join(tmp, "arg.txt")
	okScript := filepath.Join(tmp, "ok.sh")
	if err := os.WriteFile(okScript, []byte(fmt.Sprintf("#!/bin/sh\nprintf '%%s' \"$1\" > %q\ncat > %q\n", argFile, captureFile)), 0o700); err != nil {
		t.Fatal(err)
	}
	shellMetaArg := "safe; echo should_not_exec"
	ev := NotifyEnvelope{
		Version:  1,
		Event:    "pr_ready",
		At:       "2026-11-24T16:00:00Z",
		Title:    "Quartermaster: PR ready to merge",
		Body:     "task-9: acme/app#9 ready to merge (approved)",
		InboxURL: "http://100.64.0.5:8642/inbox",
		Items:    []NotifyItem{{Bead: "task-9", Reason: "pr_ready", Summary: "task-9: acme/app#9 ready to merge (approved)", PR: "acme/app#9"}},
	}
	res := deliverCommand(context.Background(), NotifyCommandConfig{
		Name: "ok-cmd",
		Argv: []string{okScript, shellMetaArg},
	}, ev)
	if res.State != "submitted" {
		t.Fatalf("ok command failed: %+v", res)
	}
	gotArg, err := os.ReadFile(argFile)
	if err != nil || string(gotArg) != shellMetaArg {
		t.Fatalf("argv was shell-evaluated or altered: got %q err=%v", string(gotArg), err)
	}
	rawIn, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read captured stdin: %v", err)
	}
	var gotEv NotifyEnvelope
	if err := json.Unmarshal(rawIn, &gotEv); err != nil {
		t.Fatalf("unmarshal stdin JSON: %v (raw=%q)", err, string(rawIn))
	}
	if gotEv.Version != 1 || gotEv.Event != "pr_ready" || gotEv.InboxURL != "http://100.64.0.5:8642/inbox" || len(gotEv.Items) != 1 {
		t.Fatalf("unexpected stdin envelope: %+v", gotEv)
	}

	// 2. Nonzero exit sanitizes stderr secrets.
	failScript := filepath.Join(tmp, "fail.sh")
	if err := os.WriteFile(failScript, []byte("#!/bin/sh\necho 'auth failed Bearer secret-cmd-tok-123 https://hooks.example.com/x?k=1' >&2\nexit 42\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	failRes := deliverCommand(context.Background(), NotifyCommandConfig{
		Name: "fail-cmd",
		Argv: []string{failScript},
	}, ev)
	if failRes.State != "failed" || !strings.Contains(failRes.Err, "exit code 42") {
		t.Fatalf("expected exit code 42 failure, got %+v", failRes)
	}
	if strings.Contains(failRes.Err, "secret-cmd-tok-123") || strings.Contains(failRes.Err, "hooks.example.com") {
		t.Fatalf("command error leaked secret/URL: %q", failRes.Err)
	}

	// 3. Process-group timeout kills spawned grandchild process.
	pidFile := filepath.Join(tmp, "child.pid")
	slowScript := filepath.Join(tmp, "slow.sh")
	if err := os.WriteFile(slowScript, []byte(fmt.Sprintf("#!/bin/sh\nsleep 60 &\necho $! > %q\nwait\n", pidFile)), 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	toRes := deliverCommand(context.Background(), NotifyCommandConfig{
		Name:           "slow-cmd",
		Argv:           []string{slowScript},
		TimeoutSeconds: intPtr(1),
	}, ev)
	elapsed := time.Since(start)
	if toRes.State != "timeout" {
		t.Fatalf("expected timeout state, got %+v", toRes)
	}
	if elapsed > 6*time.Second {
		t.Fatalf("command timeout took too long: %s", elapsed)
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("expected child pid file: %v", err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || childPID <= 0 {
		t.Fatalf("invalid child pid %q", string(pidBytes))
	}
	// Give the OS a brief moment to reap the SIGKILLed process group.
	dead := false
	for i := 0; i < 30; i++ {
		if err := syscall.Kill(childPID, 0); errors.Is(err, syscall.ESRCH) {
			dead = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !dead {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
		t.Fatalf("grandchild pid %d survived process-group timeout cleanup", childPID)
	}
}

func TestNotifySanitizationStripsSecretsAndTapTokensPreservesInboxURL(t *testing.T) {
	cfg, _, _ := setupNotifyTestEnv(t)
	now := time.Date(2026, 11, 24, 17, 0, 0, 0, time.UTC)

	var gotRaw []byte
	var gotEv NotifyEnvelope
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRaw, _ = io.ReadAll(r.Body)
		_ = json.Unmarshal(gotRaw, &gotEv)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg.Desktop.Enabled = boolPtr(false)
	cfg.Tap = TapConfig{Enabled: true, Port: 8642, Devices: []string{"phone"}}
	cfg.Notify = NotifyConfig{
		Webhooks: []NotifyWebhookConfig{{
			Name:            "sec-check",
			URL:             srv.URL + "/hook",
			CooldownSeconds: intPtr(0),
		}},
	}

	env := liveNotifyEnv(cfg, nil)
	env.now = func() time.Time { return now }
	env.inboxURL = func(Config) (string, error) {
		return "http://100.64.9.10:8642/inbox", nil
	}

	secretNotes := strings.Join([]string{
		"QUESTION: Rotate key with Authorization: Bearer topsecret_bearer_xyz123, sk_live_1234567890abcdef, password=hunter2pass, Cookie: sid=cookie_secret_val, and tap link http://100.64.9.10:8642/tap/YWJjZGVmZ2hpamtsbW5vcA.0123456789abcdef0123456789abcdef0123456789ab?",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAtotallysecretkeymaterial\n-----END RSA PRIVATE KEY-----",
		"OPTIONS:",
		"1. Rotate now",
		"2. Wait",
		"CLASS: 8 — secret rotation",
	}, "\n")

	th := Thread{
		Bead: Bead{
			ID:     "task-sec",
			Title:  "Rotate credentials",
			Status: StatusNeedsMe,
			Notes:  secretNotes,
		},
	}

	if err := reconcileNotifyWith(context.Background(), cfg, env, []Thread{th}, nil, now, nil); err != nil {
		t.Fatalf("reconcileNotifyWith: %v", err)
	}

	if gotEv.Event != "needs_me" {
		t.Fatalf("event = %q, want needs_me", gotEv.Event)
	}
	if gotEv.InboxURL != "http://100.64.9.10:8642/inbox" {
		t.Fatalf("inbox_url = %q, want non-secret /inbox URL", gotEv.InboxURL)
	}
	payloadStr := string(gotRaw)
	for _, secret := range []string{
		"topsecret_bearer_xyz123",
		"sk_live_1234567890abcdef",
		"hunter2pass",
		"cookie_secret_val",
		"/tap/",
		"YWJjZGVmZ2hpamtsbW5vcA",
		"0123456789abcdef0123456789abcdef",
		"MIIEowIBAAKCAQEAtotallysecretkeymaterial",
		"BEGIN RSA PRIVATE KEY",
	} {
		if strings.Contains(payloadStr, secret) {
			t.Fatalf("sanitization failed: payload leaked %q:\n%s", secret, payloadStr)
		}
	}

	// Defense in depth: validateSafeInboxURL must reject any /tap/<token> URL.
	if u := validateSafeInboxURL("http://100.64.9.10:8642/tap/token.sig"); u != "" {
		t.Fatalf("validateSafeInboxURL accepted /tap/ link: %q", u)
	}
	if u := validateSafeInboxURL("http://100.64.9.10:8642/inbox?token=abc"); u != "" {
		t.Fatalf("validateSafeInboxURL accepted query string: %q", u)
	}
}

func TestNotifyEventFilteringCooldownRestartAndDesktopDisabled(t *testing.T) {
	cfg, _, _ := setupNotifyTestEnv(t)
	now := time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC)

	var (
		mu       sync.Mutex
		mergedEv []NotifyEnvelope
		otherEv  []NotifyEnvelope
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev NotifyEnvelope
		_ = json.NewDecoder(r.Body).Decode(&ev)
		mu.Lock()
		switch r.URL.Path {
		case "/merged":
			mergedEv = append(mergedEv, ev)
		case "/other":
			otherEv = append(otherEv, ev)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Disable desktop notifications completely; outbound [notify] must still work.
	cfg.Desktop.Enabled = boolPtr(false)
	cfg.Notify = NotifyConfig{
		Webhooks: []NotifyWebhookConfig{
			{
				Name:            "merged-only",
				URL:             srv.URL + "/merged",
				Events:          []string{"pr_merged"},
				CooldownSeconds: intPtr(60),
			},
			{
				Name:            "blockers-and-green",
				URL:             srv.URL + "/other",
				Events:          []string{"pr_green", "needs_me", "blocked", "worker_blocked"},
				CooldownSeconds: intPtr(0),
			},
		},
	}

	env := liveNotifyEnv(cfg, nil)
	env.now = func() time.Time { return now }

	// Derive milestones via transitionMilestones even though [desktop] is disabled.
	openPR := &PR{Number: 10, Title: "Ship parser", URL: "https://github.com/acme/app/pull/10", State: "OPEN", HeadSHA: "s1"}
	mergedPR := *openPR
	mergedPR.State = "MERGED"
	prev := snapshot(thread(Bead{ID: "task-10", Status: StatusInProgress}, agent("idle", 1), openPR), Snapshot{}, now)
	thMerged := thread(Bead{ID: "task-10", Status: StatusInProgress}, agent("idle", 1), &mergedPR)

	if dm := desktopMilestonesForTransition(thMerged, prev, false, cfg.Desktop); len(dm) != 0 {
		t.Fatalf("expected desktopMilestonesForTransition empty when [desktop] disabled, got %+v", dm)
	}
	ms1 := transitionMilestones(thMerged, prev, false)
	if len(ms1) != 1 || ms1[0].Reason != "pr_merged" {
		t.Fatalf("expected 1 pr_merged milestone, got %+v", ms1)
	}
	blockedTh := Thread{Bead: Bead{ID: "task-blk", Title: "Waiting on vendor", Status: StatusBlocked}}

	// Tick 1: desktop stays untouched; merged-only gets pr_merged; blockers-and-green gets blocked.
	if err := reconcileDesktopWith([]Thread{thMerged, blockedTh}, ms1, cfg.Desktop, now, func(_, _ string) desktopResult {
		t.Fatal("desktop submit must not fire when [desktop] enabled = false")
		return desktopResult{}
	}); err != nil {
		t.Fatal(err)
	}
	if err := reconcileNotifyWith(context.Background(), cfg, env, []Thread{thMerged, blockedTh}, ms1, now, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(desktopPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("desktop journal should not exist when [desktop] disabled")
	}

	mu.Lock()
	if len(mergedEv) != 1 || mergedEv[0].Event != "pr_merged" {
		t.Fatalf("merged-only got %+v", mergedEv)
	}
	if len(otherEv) != 1 || otherEv[0].Event != "blocked" {
		t.Fatalf("blockers-and-green got %+v", otherEv)
	}
	mu.Unlock()

	// Duplicate tick with same blocked thread and same milestone does NOT re-emit.
	if err := reconcileNotifyWith(context.Background(), cfg, env, []Thread{thMerged, blockedTh}, ms1, now.Add(5*time.Second), nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(mergedEv) != 1 || len(otherEv) != 1 {
		t.Fatalf("duplicate tick re-emitted: merged=%d other=%d", len(mergedEv), len(otherEv))
	}
	mu.Unlock()

	// Tick 2 at t+15s (inside merged-only's 60s cooldown): new pr_merged must queue as skipped_cooldown.
	t2 := now.Add(15 * time.Second)
	env.now = func() time.Time { return t2 }
	m2 := desktopMilestone{
		Key:     desktopMilestoneKey("pr_merged", "task-11", "https://github.com/acme/app/pull/11"),
		Bead:    "task-11",
		Reason:  "pr_merged",
		Summary: "task-11: acme/app#11 merged — Fix race",
		PR:      "acme/app#11",
	}
	if err := reconcileNotifyWith(context.Background(), cfg, env, []Thread{blockedTh}, []desktopMilestone{m2}, t2, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(mergedEv) != 1 {
		t.Fatalf("cooldown violated: mergedEv len = %d", len(mergedEv))
	}
	mu.Unlock()
	st, err := loadNotifyStatus()
	if err != nil {
		t.Fatal(err)
	}
	if ss := st.Sinks["merged-only"]; ss.LastState != "skipped_cooldown" || len(ss.PendingItems) != 1 {
		t.Fatalf("expected skipped_cooldown with 1 pending item, got %+v", ss)
	}

	// Simulate ticker restart at t+65s (fresh env, no new milestones): pending item flushes without dropping!
	t3 := now.Add(65 * time.Second)
	freshEnv := liveNotifyEnv(cfg, nil)
	freshEnv.now = func() time.Time { return t3 }
	if err := reconcileNotifyWith(context.Background(), cfg, freshEnv, []Thread{blockedTh}, nil, t3, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(mergedEv) != 2 || len(mergedEv[1].Items) != 1 || mergedEv[1].Items[0].Bead != "task-11" {
		t.Fatalf("pending item not flushed after cooldown/restart: %+v", mergedEv)
	}
	mu.Unlock()
}

func TestNotifyTickerAsyncDoesNotBlockSlowWebhook(t *testing.T) {
	cfg, _, _ := setupNotifyTestEnv(t)
	now := time.Date(2026, 11, 24, 18, 30, 0, 0, time.UTC)

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	cfg.Notify = NotifyConfig{
		Webhooks: []NotifyWebhookConfig{{
			Name:            "slow-async",
			URL:             srv.URL + "/slow",
			TimeoutSeconds:  intPtr(5),
			CooldownSeconds: intPtr(0),
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	env := liveNotifyEnv(cfg, nil)
	env.now = func() time.Time { return now }

	th := Thread{Bead: Bead{ID: "task-async", Title: "Slow hook test", Status: StatusBlocked}}
	t0 := time.Now()
	if err := reconcileNotifyWith(ctx, cfg, env, []Thread{th}, nil, now, &wg); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > 500*time.Millisecond {
		t.Fatalf("async reconcileNotifyWith stalled ticker loop for %s", d)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("async webhook request did not start")
	}
	// Canceling context (ticker shutdown) cleanly unblocks Wait().
	cancel()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notifyWG did not finish promptly on context cancel")
	}
}

func TestNotifyInboxRunnerIntegrationDesktopDisabled(t *testing.T) {
	cfg, _ := setupInboxRunnerTestEnv(t)
	now := time.Date(2026, 11, 24, 19, 0, 0, 0, time.UTC)

	var (
		mu      sync.Mutex
		got     []NotifyEnvelope
		desktop atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev NotifyEnvelope
		_ = json.NewDecoder(r.Body).Decode(&ev)
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg.Desktop.Enabled = boolPtr(false)
	cfg.Notify = NotifyConfig{
		Webhooks: []NotifyWebhookConfig{{
			Name:            "runner-hook",
			URL:             srv.URL + "/runner",
			Events:          []string{"inbox_runner"},
			CooldownSeconds: intPtr(0),
		}},
	}

	// Case 1: acked > 0 emits inbox_runner event with counts, kinds, and duration.
	f1 := writeTestInboxEvent(t, Event{Bead: "task-r1", Kind: EventMerged, Summary: "PR merged", At: now.Add(-5 * time.Minute)})
	curTime := now
	nEnv := liveNotifyEnv(cfg, nil)
	nEnv.now = func() time.Time { return curTime }
	env := inboxRunnerEnv{
		now: func() time.Time { return curTime },
		listAgents: func() (map[string]Agent, bool, error) {
			return map[string]Agent{}, true, nil
		},
		readInbox:     readInbox,
		headlessOwned: func(string) bool { return false },
		coordDir:      func() string { return filepath.Join(t.TempDir(), "coord") },
		notify: func(_, _ string) desktopResult {
			desktop.Add(1)
			return desktopResult{"submitted", "ok"}
		},
		notifyEnv: &nEnv,
		execCmd: func(_ context.Context, _ Config, _ string, _ []string, _ string) inboxRunnerExecResult {
			curTime = curTime.Add(250 * time.Millisecond)
			_, _ = inboxDoneFiles([]string{f1})
			return inboxRunnerExecResult{ExitCode: 0, Output: "handled"}
		},
	}
	res, err := runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: true})
	if err != nil {
		t.Fatalf("runInboxRunner ok: %v", err)
	}
	if res.Acked != 1 {
		t.Fatalf("acked = %d, want 1", res.Acked)
	}
	if desktop.Load() != 0 {
		t.Fatalf("desktop notify fired despite [desktop] enabled = false")
	}

	// Case 2: ok with acked == 0 emits nothing.
	_ = writeTestInboxEvent(t, Event{Bead: "task-r2", Kind: EventUnblocked, Summary: "unblocked", At: now.Add(-5 * time.Minute)})
	env.execCmd = func(_ context.Context, _ Config, _ string, _ []string, _ string) inboxRunnerExecResult {
		curTime = curTime.Add(100 * time.Millisecond)
		return inboxRunnerExecResult{ExitCode: 0, Output: "nothing acked"}
	}
	if _, err := runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: true}); err != nil {
		t.Fatalf("runInboxRunner noop: %v", err)
	}

	// Case 3: failure emits inbox_runner error event.
	env.execCmd = func(_ context.Context, _ Config, _ string, _ []string, _ string) inboxRunnerExecResult {
		curTime = curTime.Add(400 * time.Millisecond)
		return inboxRunnerExecResult{ExitCode: 1, Err: errors.New("exit status 1")}
	}
	if _, err := runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: true}); err == nil {
		t.Fatal("expected error from failing runner")
	}

	// Case 4: timeout emits inbox_runner timeout event.
	f3 := writeTestInboxEvent(t, Event{Bead: "task-r3", Kind: EventFailing, Summary: "ci failed", At: now.Add(-5 * time.Minute)})
	_ = f3
	env.execCmd = func(_ context.Context, _ Config, _ string, _ []string, _ string) inboxRunnerExecResult {
		curTime = curTime.Add(time.Second)
		return inboxRunnerExecResult{ExitCode: -1, TimedOut: true, Err: errors.New("timed out after 300s")}
	}
	if _, err := runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: true}); err == nil {
		t.Fatal("expected timeout error from runner")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("expected 3 inbox_runner events (ok+acked, error, timeout), got %d: %+v", len(got), got)
	}
	if got[0].Event != "inbox_runner" || got[0].Runner == nil || got[0].Runner.ExitStatus != "ok" || got[0].Runner.Acked != 1 || got[0].Runner.Eligible != 1 || got[0].Runner.DurationMs != 250 || !slices.Equal(got[0].Runner.Kinds, []string{"merged"}) {
		t.Fatalf("unexpected first runner event: %+v", got[0])
	}
	if got[1].Event != "inbox_runner" || got[1].Runner == nil || got[1].Runner.ExitStatus != "error" || got[1].Runner.DurationMs != 400 {
		t.Fatalf("unexpected error runner event: %+v", got[1])
	}
	if got[2].Event != "inbox_runner" || got[2].Runner == nil || got[2].Runner.ExitStatus != "timeout" || got[2].Runner.DurationMs != 1000 {
		t.Fatalf("unexpected timeout runner event: %+v", got[2])
	}
}

func TestNotifyCLIStatusAndTest(t *testing.T) {
	cfg, _, _ := setupNotifyTestEnv(t)
	now := time.Date(2026, 11, 24, 20, 0, 0, 0, time.UTC)
	env := liveNotifyEnv(cfg, nil)
	env.now = func() time.Time { return now }

	// Empty config: status succeeds cleanly; test exits nonzero.
	var out bytes.Buffer
	if err := notifyCmd(cfg, env, []string{"status"}, &out); err != nil {
		t.Fatalf("empty notify status: %v", err)
	}
	if !strings.Contains(out.String(), "no sinks configured") {
		t.Fatalf("unexpected empty status output: %q", out.String())
	}
	out.Reset()
	if err := notifyCmd(cfg, env, []string{"status", "--json"}, &out); err != nil {
		t.Fatalf("empty notify status --json: %v", err)
	}
	var emptyRep NotifyStatusReport
	if err := json.Unmarshal(out.Bytes(), &emptyRep); err != nil || emptyRep.Enabled || len(emptyRep.Sinks) != 0 {
		t.Fatalf("unexpected empty JSON report: %+v err=%v", emptyRep, err)
	}
	out.Reset()
	if err := notifyCmd(cfg, env, []string{"test"}, &out); err == nil {
		t.Fatal("expected notify test with no sinks to fail")
	}

	// Configure one passing webhook (url_env), one passing command, one failing webhook.
	var gotTestEvents []NotifyEnvelope
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev NotifyEnvelope
		_ = json.NewDecoder(r.Body).Decode(&ev)
		mu.Lock()
		gotTestEvents = append(gotTestEvents, ev)
		mu.Unlock()
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("internal error"))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("CLI_TEST_HOOK", srv.URL+"/ok?secret=hidden-cli-token")
	okCmd := filepath.Join(t.TempDir(), "sink.sh")
	if err := os.WriteFile(okCmd, []byte("#!/bin/sh\ncat >/dev/null\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	cfg.Notify = NotifyConfig{
		Webhooks: []NotifyWebhookConfig{
			{Name: "hook-ok", URLEnv: "CLI_TEST_HOOK"},
			{Name: "hook-bad", URL: srv.URL + "/fail?secret=hidden-fail-token"},
		},
		Commands: []NotifyCommandConfig{
			{Name: "cmd-ok", Argv: []string{okCmd}},
		},
	}

	// Unknown --sink exits nonzero.
	out.Reset()
	if err := notifyCmd(cfg, env, []string{"test", "--sink", "does-not-exist"}, &out); err == nil {
		t.Fatal("expected unknown --sink error")
	}

	// Targeting one healthy sink succeeds and sends synthetic event: "test".
	out.Reset()
	if err := notifyCmd(cfg, env, []string{"test", "--sink", "hook-ok"}, &out); err != nil {
		t.Fatalf("notify test --sink hook-ok: %v (out=%q)", err, out.String())
	}
	if !strings.Contains(out.String(), "hook-ok") || !strings.Contains(out.String(), "env:CLI_TEST_HOOK") || strings.Contains(out.String(), "hidden-cli-token") {
		t.Fatalf("unexpected notify test output: %q", out.String())
	}
	mu.Lock()
	if len(gotTestEvents) != 1 || gotTestEvents[0].Event != "test" || gotTestEvents[0].Version != 1 {
		t.Fatalf("unexpected synthetic test envelope: %+v", gotTestEvents)
	}
	mu.Unlock()

	// Testing all sinks fails nonzero because hook-bad returns 500, while still redacting secrets.
	out.Reset()
	if err := notifyCmd(cfg, env, []string{"test"}, &out); err == nil {
		t.Fatalf("expected notify test to fail when one sink fails; out=%q", out.String())
	}
	if !strings.Contains(out.String(), "hook-ok") || !strings.Contains(out.String(), "hook-bad") || !strings.Contains(out.String(), "cmd-ok") {
		t.Fatalf("missing per-sink lines: %q", out.String())
	}
	if strings.Contains(out.String(), "hidden-fail-token") || strings.Contains(out.String(), "/fail") {
		t.Fatalf("notify test leaked raw URL/query: %q", out.String())
	}

	// notify status --json reflects persisted delivery outcomes cleanly.
	out.Reset()
	if err := notifyCmd(cfg, env, []string{"status", "--json"}, &out); err != nil {
		t.Fatalf("notify status --json: %v", err)
	}
	var rep NotifyStatusReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if !rep.Enabled || len(rep.Sinks) != 3 {
		t.Fatalf("unexpected report: %+v", rep)
	}
	if rep.Sinks[0].LastState != "submitted" || rep.Sinks[0].TotalSent != 2 {
		t.Fatalf("unexpected hook-ok status: %+v", rep.Sinks[0])
	}
	if rep.Sinks[1].LastState != "failed" || rep.Sinks[1].TotalFailed != 1 || rep.Sinks[1].Target != srv.URL {
		t.Fatalf("unexpected hook-bad status: %+v", rep.Sinks[1])
	}
	if rep.Sinks[2].LastState != "submitted" || rep.Sinks[2].TotalSent != 1 {
		t.Fatalf("unexpected cmd-ok status: %+v", rep.Sinks[2])
	}
}

func TestNotifyTickerOnceEndToEndDesktopDisabled(t *testing.T) {
	dir := fakeTools(t)
	t.Setenv("PATH", dir)
	write := func(name, script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	var (
		mu       sync.Mutex
		received []NotifyEnvelope
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev NotifyEnvelope
		_ = json.NewDecoder(r.Body).Decode(&ev)
		mu.Lock()
		received = append(received, ev)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	beads := []Bead{
		{ID: "task-ticker-1", Title: "Deploy API", Status: StatusNeedsMe, Notes: "QUESTION: Approve deploy?\nOPTIONS:\n1. Yes\n2. No\nCLASS: 7 — prod"},
	}
	data, _ := json.Marshal(beads)
	t.Setenv("FIXTURE_BEADS", string(data))
	write("bd", "printf '%s\\n' \"$FIXTURE_BEADS\"\n")
	write("herdr", "printf '{\"result\":{\"agents\":[]}}\\n'\n")

	cfg := defaultConfig()
	cfg.Desktop.Enabled = boolPtr(false)
	cfg.Notify = NotifyConfig{
		Webhooks: []NotifyWebhookConfig{{
			Name:            "ticker-hook",
			URL:             srv.URL + "/ticker",
			CooldownSeconds: intPtr(0),
		}},
	}

	var logs bytes.Buffer
	now := time.Date(2026, 11, 24, 21, 0, 0, 0, time.UTC)
	ticker := newTicker(cfg, log.New(&logs, "", 0))
	ticker.now = func() time.Time { return now }
	st := loadState()
	st.LastGH = now // skip gh pass in this fixture

	if err := ticker.once(&st); err != nil {
		t.Fatalf("ticker.once: %v (logs: %s)", err, logs.String())
	}
	if _, err := os.Stat(desktopPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("desktop journal created despite [desktop] enabled = false")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 || received[0].Event != "needs_me" || len(received[0].Items) != 1 || received[0].Items[0].Bead != "task-ticker-1" {
		t.Fatalf("unexpected ticker.once notify payloads: %+v (logs: %s)", received, logs.String())
	}
}
