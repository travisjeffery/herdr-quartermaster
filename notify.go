package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	notifyEnvelopeVersion   = 1
	notifyStatusVersion     = 1
	notifyMaxInactiveEp     = 256
	notifyMaxPendingPerSink = 64
	notifyMaxTitleRunes     = 200
	notifyMaxBodyRunes      = 1000
	notifyMaxSummaryRunes   = 240
	notifyMaxErrorRunes     = 240
	notifyHTTPMaxRespBytes  = 64 << 10
	notifyCmdMaxOutputBytes = 16 << 10
	notifyMaxConcurrent     = 16
)

// NotifyEnvelope is the versioned, privacy-safe notification payload delivered
// to configured [[notify.webhooks]] (JSON or text) and [[notify.commands]]
// (JSON on stdin).
type NotifyEnvelope struct {
	Version  int               `json:"version"`
	Event    string            `json:"event"`
	At       string            `json:"at"`
	Title    string            `json:"title"`
	Body     string            `json:"body"`
	InboxURL string            `json:"inbox_url,omitempty"`
	Items    []NotifyItem      `json:"items,omitempty"`
	Runner   *NotifyRunnerInfo `json:"runner,omitempty"`
}

// NotifyItem summarizes one sanitized ticker milestone or blocker item inside
// a NotifyEnvelope.
type NotifyItem struct {
	Bead    string `json:"bead,omitempty"`
	Reason  string `json:"reason"`
	Summary string `json:"summary,omitempty"`
	PR      string `json:"pr,omitempty"`
}

// NotifyRunnerInfo summarizes one headless inbox runner outcome inside an
// inbox_runner NotifyEnvelope.
type NotifyRunnerInfo struct {
	ExitStatus string   `json:"exit_status"`
	Acked      int      `json:"acked"`
	Eligible   int      `json:"eligible"`
	Kinds      []string `json:"kinds,omitempty"`
	DurationMs int64    `json:"duration_ms"`
}

type notifyStatusFile struct {
	Version  int                        `json:"version"`
	Sinks    map[string]notifySinkState `json:"sinks"`
	Episodes map[string]notifyEpisode   `json:"episodes,omitempty"`
}

type notifySinkState struct {
	LastAttemptAt time.Time             `json:"last_attempt_at,omitempty"`
	LastSuccessAt time.Time             `json:"last_success_at,omitempty"`
	LastState     string                `json:"last_state,omitempty"` // submitted, failed, timeout, skipped_cooldown
	LastError     string                `json:"last_error,omitempty"`
	TotalSent     int                   `json:"total_sent"`
	TotalFailed   int                   `json:"total_failed"`
	PendingItems  []notifyPendingItem   `json:"pending_items,omitempty"`
	PendingRunner []notifyPendingRunner `json:"pending_runner,omitempty"`
}

type notifyEpisode struct {
	Bead       string    `json:"bead,omitempty"`
	Reason     string    `json:"reason"`
	PromptHash string    `json:"prompt_hash,omitempty"`
	Detected   time.Time `json:"detected"`
	Active     bool      `json:"active"`
	Ended      time.Time `json:"ended,omitempty"`
}

type notifyPendingItem struct {
	Key     string    `json:"key"`
	Bead    string    `json:"bead,omitempty"`
	Reason  string    `json:"reason"`
	Summary string    `json:"summary,omitempty"`
	PR      string    `json:"pr,omitempty"`
	At      time.Time `json:"at"`
}

type notifyPendingRunner struct {
	Title  string           `json:"title"`
	Body   string           `json:"body"`
	Runner NotifyRunnerInfo `json:"runner"`
	At     time.Time        `json:"at"`
}

type notifySink struct {
	Name     string
	Type     string // "webhook" or "command"
	Webhook  *NotifyWebhookConfig
	Command  *NotifyCommandConfig
	Events   []string
	Timeout  time.Duration
	Cooldown time.Duration
}

type notifyDeliveryResult struct {
	State    string // submitted, failed, timeout
	Err      string // sanitized, never raw URLs/tokens/headers
	Duration time.Duration
}

type notifyJob struct {
	Sink      notifySink
	Envelope  NotifyEnvelope
	AttemptAt time.Time
}

// notifyEnv holds injectable dependencies for deterministic testing.
type notifyEnv struct {
	now        func() time.Time
	getenv     func(string) string
	httpClient func(timeout time.Duration) *http.Client
	inboxURL   func(Config) (string, error)
	logf       func(format string, args ...any)
}

func liveNotifyEnv(cfg Config, logf func(string, ...any)) notifyEnv {
	_ = cfg
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return notifyEnv{
		now:        time.Now,
		getenv:     os.Getenv,
		httpClient: defaultNotifyHTTPClient,
		inboxURL:   tapInboxURL,
		logf:       logf,
	}
}

func (e notifyEnv) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

func (e notifyEnv) envVal(key string) string {
	if e.getenv != nil {
		return e.getenv(key)
	}
	return os.Getenv(key)
}

func (e notifyEnv) client(timeout time.Duration) *http.Client {
	if e.httpClient != nil {
		return e.httpClient(timeout)
	}
	return defaultNotifyHTTPClient(timeout)
}

func (e notifyEnv) log(format string, args ...any) {
	if e.logf != nil {
		e.logf(format, args...)
	}
}

func defaultNotifyHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("stopped after 3 redirects")
			}
			scheme := strings.ToLower(req.URL.Scheme)
			if scheme != "http" && scheme != "https" {
				return fmt.Errorf("disallowed redirect scheme %q", scheme)
			}
			if len(via) > 0 && !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
				req.Header.Del("Authorization")
				req.Header.Del("Cookie")
				req.Header.Del("X-Api-Key")
			}
			return nil
		},
	}
}

func notifyDir() string        { return filepath.Join(stateDir(), "notify") }
func notifyStatusPath() string { return filepath.Join(notifyDir(), "status.json") }
func notifyLeasePath() string  { return filepath.Join(notifyDir(), "lease") }

func withNotifyLock(fn func() error) error {
	if err := rpcPrivateDir(notifyDir()); err != nil {
		return err
	}
	lock, err := os.OpenFile(notifyLeasePath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	_ = lock.Chmod(0o600)
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock notify state: %w", err)
	}
	return fn()
}

func loadNotifyStatus() (notifyStatusFile, error) {
	var st notifyStatusFile
	b, err := os.ReadFile(notifyStatusPath())
	if errors.Is(err, os.ErrNotExist) {
		return notifyStatusFile{
			Version:  notifyStatusVersion,
			Sinks:    map[string]notifySinkState{},
			Episodes: map[string]notifyEpisode{},
		}, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("invalid notify status file; inspect %s: %w", notifyStatusPath(), err)
	}
	if st.Version != notifyStatusVersion {
		return st, fmt.Errorf("unsupported notify status version %d; inspect %s", st.Version, notifyStatusPath())
	}
	if st.Sinks == nil {
		st.Sinks = map[string]notifySinkState{}
	}
	if st.Episodes == nil {
		st.Episodes = map[string]notifyEpisode{}
	}
	return st, nil
}

func saveNotifyStatus(st notifyStatusFile) error {
	if st.Version == 0 {
		st.Version = notifyStatusVersion
	}
	if st.Sinks == nil {
		st.Sinks = map[string]notifySinkState{}
	}
	if st.Episodes == nil {
		st.Episodes = map[string]notifyEpisode{}
	}
	if err := rpcPrivateDir(notifyDir()); err != nil {
		return err
	}
	path := notifyStatusPath()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	f, err := os.CreateTemp(filepath.Dir(path), ".notify-status-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func configuredNotifySinks(cfg Config) []notifySink {
	out := make([]notifySink, 0, len(cfg.Notify.Webhooks)+len(cfg.Notify.Commands))
	for i := range cfg.Notify.Webhooks {
		w := cfg.Notify.Webhooks[i]
		out = append(out, notifySink{
			Name:     w.Name,
			Type:     "webhook",
			Webhook:  &w,
			Events:   w.effectiveEvents(),
			Timeout:  w.timeout(),
			Cooldown: w.cooldown(),
		})
	}
	for i := range cfg.Notify.Commands {
		c := cfg.Notify.Commands[i]
		out = append(out, notifySink{
			Name:     c.Name,
			Type:     "command",
			Command:  &c,
			Events:   c.effectiveEvents(),
			Timeout:  c.timeout(),
			Cooldown: c.cooldown(),
		})
	}
	return out
}

func (s notifySink) subscribes(event string) bool {
	return slices.Contains(s.Events, event)
}

// webhookTargetDisplay returns a strictly redacted target label for display or
// logs: "env:<VAR>" for url_env, or "scheme://host" only (stripping userinfo,
// path, query, and fragment) for literal URLs.
func webhookTargetDisplay(w NotifyWebhookConfig) string {
	if w.URLEnv != "" {
		return "env:" + w.URLEnv
	}
	u, err := url.Parse(w.URL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "[redacted]"
	}
	return strings.ToLower(u.Scheme) + "://" + u.Host
}

func webhookHeaderSummary(w NotifyWebhookConfig) []string {
	if len(w.Headers) == 0 && len(w.HeaderEnvs) == 0 {
		return nil
	}
	out := make([]string, 0, len(w.Headers)+len(w.HeaderEnvs))
	for k := range w.Headers {
		out = append(out, k)
	}
	for k, envVar := range w.HeaderEnvs {
		out = append(out, fmt.Sprintf("%s (env:%s)", k, envVar))
	}
	sort.Strings(out)
	return out
}

func commandTargetDisplay(c NotifyCommandConfig) string {
	if len(c.Argv) == 0 {
		return ""
	}
	if s := sanitizeDesktopLine(strings.Join(c.Argv, " "), 120); s != "" {
		return s
	}
	return c.Argv[0]
}

func (s notifySink) redactedTarget() string {
	if s.Webhook != nil {
		return webhookTargetDisplay(*s.Webhook)
	}
	if s.Command != nil {
		return commandTargetDisplay(*s.Command)
	}
	return ""
}

// validateSafeInboxURL accepts ONLY the non-secret bookmarkable /inbox URL
// (http:// or https://, host present, no userinfo, no query, no fragment,
// exact path "/inbox"). Signed /tap/<token> or /tap-links URLs are always rejected.
func validateSafeInboxURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	if u.Path != "/inbox" {
		return ""
	}
	return u.String()
}

func resolveSafeTapInboxURL(cfg Config, env notifyEnv) string {
	if !cfg.Tap.Enabled {
		return ""
	}
	fn := env.inboxURL
	if fn == nil {
		fn = tapInboxURL
	}
	raw, err := fn(cfg)
	if err != nil {
		return ""
	}
	return validateSafeInboxURL(raw)
}

// sanitizeNotifyEnvelope enforces privacy sanitization across every text field
// of a NotifyEnvelope while preserving only a validated non-secret /inbox URL.
func sanitizeNotifyEnvelope(ev NotifyEnvelope) NotifyEnvelope {
	ev.Version = notifyEnvelopeVersion
	ev.Event = sanitizeDesktopLine(ev.Event, 64)
	ev.Title = sanitizeDesktopLine(ev.Title, notifyMaxTitleRunes)
	ev.Body = sanitizeDesktopLine(ev.Body, notifyMaxBodyRunes)
	ev.InboxURL = validateSafeInboxURL(ev.InboxURL)
	if len(ev.Items) > 0 {
		clean := make([]NotifyItem, 0, len(ev.Items))
		for _, it := range ev.Items {
			clean = append(clean, NotifyItem{
				Bead:    sanitizeDesktopLine(it.Bead, 120),
				Reason:  sanitizeDesktopLine(it.Reason, 64),
				Summary: sanitizeDesktopLine(it.Summary, notifyMaxSummaryRunes),
				PR:      sanitizeDesktopLine(it.PR, 120),
			})
		}
		ev.Items = clean
	}
	if ev.Runner != nil {
		r := *ev.Runner
		r.ExitStatus = sanitizeDesktopLine(r.ExitStatus, 32)
		if len(r.Kinds) > 0 {
			kinds := make([]string, 0, len(r.Kinds))
			for _, k := range r.Kinds {
				if sk := sanitizeDesktopLine(k, 64); sk != "" {
					kinds = append(kinds, sk)
				}
			}
			r.Kinds = kinds
		}
		ev.Runner = &r
	}
	return ev
}

// sanitizeNotifyError redacts literal secrets/URLs/headers first and then runs
// desktop.go's sanitizer so no URL, path secret, query token, bearer token,
// cookie, private key, or signed tap link can leak into status.json or logs.
func sanitizeNotifyError(raw string, secrets ...string) string {
	s := raw
	if len(secrets) > 0 {
		filtered := make([]string, 0, len(secrets))
		for _, sec := range secrets {
			if sec != "" {
				filtered = append(filtered, sec)
			}
		}
		sort.Slice(filtered, func(i, j int) bool { return len(filtered[i]) > len(filtered[j]) })
		for _, sec := range filtered {
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
	}
	s = sanitizeDesktopLine(s, notifyMaxErrorRunes)
	if s == "" {
		return "delivery error"
	}
	return s
}

func encodeNotifyJSON(ev NotifyEnvelope) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ev); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func formatNotifyText(ev NotifyEnvelope) string {
	if ev.Body == "" {
		return ev.Title
	}
	if ev.Title == "" {
		return ev.Body
	}
	return ev.Title + "\n" + ev.Body
}

func deliverWebhook(ctx context.Context, w NotifyWebhookConfig, ev NotifyEnvelope, env notifyEnv) notifyDeliveryResult {
	start := time.Now()
	ev = sanitizeNotifyEnvelope(ev)

	rawURL := w.URL
	if w.URLEnv != "" {
		rawURL = env.envVal(w.URLEnv)
		if strings.TrimSpace(rawURL) == "" {
			return notifyDeliveryResult{
				State:    "failed",
				Err:      fmt.Sprintf("url_env %s is not set or empty", w.URLEnv),
				Duration: time.Since(start),
			}
		}
	}
	parsedURL, err := validateNotifyURL(rawURL)
	if err != nil {
		if w.URLEnv != "" {
			return notifyDeliveryResult{
				State:    "failed",
				Err:      fmt.Sprintf("url_env %s: %v", w.URLEnv, err),
				Duration: time.Since(start),
			}
		}
		return notifyDeliveryResult{
			State:    "failed",
			Err:      err.Error(),
			Duration: time.Since(start),
		}
	}

	secrets := []string{rawURL}
	if w.URLEnv != "" {
		if parsedURL.Host != "" {
			secrets = append(secrets, parsedURL.Host)
		}
		if hn := parsedURL.Hostname(); hn != "" {
			secrets = append(secrets, hn)
		}
	}
	if parsedURL.User != nil {
		if u := parsedURL.User.String(); u != "" {
			secrets = append(secrets, u)
		}
		if p, ok := parsedURL.User.Password(); ok && p != "" {
			secrets = append(secrets, p)
		}
	}
	if len(parsedURL.Path) > 1 {
		secrets = append(secrets, parsedURL.Path)
	}
	if parsedURL.RawQuery != "" {
		secrets = append(secrets, parsedURL.RawQuery)
	}

	headers := make(http.Header, len(w.Headers)+len(w.HeaderEnvs)+2)
	for k, v := range w.Headers {
		headers.Set(k, v)
		if len(v) >= 3 {
			secrets = append(secrets, v)
		}
	}
	for k, envVar := range w.HeaderEnvs {
		v := env.envVal(envVar)
		if v == "" {
			return notifyDeliveryResult{
				State:    "failed",
				Err:      fmt.Sprintf("header_envs %s for header %s is not set or empty", envVar, k),
				Duration: time.Since(start),
			}
		}
		if strings.ContainsAny(v, "\r\n") {
			return notifyDeliveryResult{
				State:    "failed",
				Err:      fmt.Sprintf("header_envs %s for header %s contains invalid newline characters", envVar, k),
				Duration: time.Since(start),
			}
		}
		headers.Set(k, v)
		if len(v) >= 3 {
			secrets = append(secrets, v)
		}
	}

	var body []byte
	defaultCT := "application/json; charset=utf-8"
	if w.effectiveFormat() == "text" {
		body = []byte(formatNotifyText(ev))
		defaultCT = "text/plain; charset=utf-8"
	} else {
		var merr error
		body, merr = encodeNotifyJSON(ev)
		if merr != nil {
			return notifyDeliveryResult{
				State:    "failed",
				Err:      sanitizeNotifyError(merr.Error(), secrets...),
				Duration: time.Since(start),
			}
		}
	}
	if headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", defaultCT)
	}
	if headers.Get("User-Agent") == "" {
		headers.Set("User-Agent", toolName+"/"+version)
	}

	timeout := w.timeout()
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, parsedURL.String(), bytes.NewReader(body))
	if err != nil {
		return notifyDeliveryResult{
			State:    "failed",
			Err:      sanitizeNotifyError(err.Error(), secrets...),
			Duration: time.Since(start),
		}
	}
	req.Header = headers

	resp, err := env.client(timeout).Do(req)
	if err != nil {
		inner := err
		var ue *url.Error
		if errors.As(err, &ue) && ue.Err != nil {
			inner = ue.Err
		}
		if errors.Is(reqCtx.Err(), context.DeadlineExceeded) || errors.Is(inner, context.DeadlineExceeded) || os.IsTimeout(inner) {
			return notifyDeliveryResult{
				State:    "timeout",
				Err:      fmt.Sprintf("request timed out after %ds", int(timeout/time.Second)),
				Duration: time.Since(start),
			}
		}
		return notifyDeliveryResult{
			State:    "failed",
			Err:      sanitizeNotifyError(inner.Error(), secrets...),
			Duration: time.Since(start),
		}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, notifyHTTPMaxRespBytes))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return notifyDeliveryResult{
			State:    "submitted",
			Duration: time.Since(start),
		}
	}
	errMsg := fmt.Sprintf("HTTP %d", resp.StatusCode)
	if snippet := sanitizeNotifyError(string(respBody), secrets...); snippet != "" && snippet != "delivery error" && !isOnlyRedacted(snippet) {
		errMsg = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncateDesktopLine(snippet, 180))
	}
	return notifyDeliveryResult{
		State:    "failed",
		Err:      errMsg,
		Duration: time.Since(start),
	}
}

func deliverCommand(ctx context.Context, c NotifyCommandConfig, ev NotifyEnvelope) notifyDeliveryResult {
	start := time.Now()
	if len(c.Argv) == 0 || strings.TrimSpace(c.Argv[0]) == "" {
		return notifyDeliveryResult{
			State:    "failed",
			Err:      "empty command argv",
			Duration: time.Since(start),
		}
	}
	ev = sanitizeNotifyEnvelope(ev)
	payload, err := encodeNotifyJSON(ev)
	if err != nil {
		return notifyDeliveryResult{
			State:    "failed",
			Err:      sanitizeNotifyError(err.Error()),
			Duration: time.Since(start),
		}
	}

	timeout := c.timeout()
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(reqCtx, c.Argv[0], c.Argv[1:]...)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		kerr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(kerr, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return kerr
	}
	cmd.WaitDelay = 2 * time.Second

	stdout := &boundedBuffer{limit: notifyCmdMaxOutputBytes}
	stderr := &boundedBuffer{limit: notifyCmdMaxOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err = cmd.Run()
	if err == nil {
		return notifyDeliveryResult{
			State:    "submitted",
			Duration: time.Since(start),
		}
	}
	if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
		return notifyDeliveryResult{
			State:    "timeout",
			Err:      fmt.Sprintf("command timed out after %ds", int(timeout/time.Second)),
			Duration: time.Since(start),
		}
	}
	detail := strings.TrimSpace(stderr.String())
	if detail == "" {
		detail = strings.TrimSpace(stdout.String())
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		msg := fmt.Sprintf("exit code %d", exitErr.ExitCode())
		if detail != "" {
			if clean := sanitizeNotifyError(detail); clean != "" && clean != "delivery error" && !isOnlyRedacted(clean) {
				msg = fmt.Sprintf("exit code %d: %s", exitErr.ExitCode(), truncateDesktopLine(clean, 180))
			}
		}
		return notifyDeliveryResult{
			State:    "failed",
			Err:      msg,
			Duration: time.Since(start),
		}
	}
	return notifyDeliveryResult{
		State:    "failed",
		Err:      sanitizeNotifyError(err.Error()),
		Duration: time.Since(start),
	}
}

func deliverNotifySink(ctx context.Context, sink notifySink, ev NotifyEnvelope, env notifyEnv) notifyDeliveryResult {
	if sink.Webhook != nil {
		return deliverWebhook(ctx, *sink.Webhook, ev, env)
	}
	if sink.Command != nil {
		return deliverCommand(ctx, *sink.Command, ev)
	}
	return notifyDeliveryResult{State: "failed", Err: "unconfigured sink"}
}

func appendPendingNotifyItem(list []notifyPendingItem, it notifyPendingItem) []notifyPendingItem {
	for i := range list {
		if list[i].Key == it.Key {
			list[i] = it
			return list
		}
	}
	list = append(list, it)
	if len(list) > notifyMaxPendingPerSink {
		list = list[len(list)-notifyMaxPendingPerSink:]
	}
	return list
}

func appendPendingNotifyRunner(list []notifyPendingRunner, r notifyPendingRunner) []notifyPendingRunner {
	list = append(list, r)
	if len(list) > notifyMaxPendingPerSink {
		list = list[len(list)-notifyMaxPendingPerSink:]
	}
	return list
}

func notifyItemSummaryForThread(th Thread, reason string) string {
	switch reason {
	case "needs_me":
		return desktopNeedsMeSummary(th)
	case "blocked":
		return "bead is blocked"
	case "worker_blocked":
		return "worker awaiting approval"
	default:
		return ""
	}
}

func formatNotifyItemsMessage(items []notifyPendingItem) (string, string) {
	if len(items) == 0 {
		return "", ""
	}
	if len(items) == 1 {
		it := items[0]
		switch it.Reason {
		case "needs_me":
			summary := it.Summary
			if summary == "" {
				summary = "needs decision"
			}
			return "Quartermaster: needs_me", fmt.Sprintf("%s: %s", it.Bead, summary)
		case "worker_blocked":
			return "Quartermaster: worker attention required", fmt.Sprintf("%s: worker awaiting approval", it.Bead)
		case "blocked":
			return "Quartermaster: bead blocked", fmt.Sprintf("%s: blocked", it.Bead)
		case "pr_merged":
			return "Quartermaster: PR merged", it.Summary
		case "pr_ready":
			return "Quartermaster: PR ready to merge", it.Summary
		case "pr_green":
			return "Quartermaster: PR green, awaiting review", it.Summary
		default:
			return "Quartermaster: notification", it.Summary
		}
	}

	var lines []string
	needsMeCount := 0
	prCount := 0
	for _, it := range items {
		switch it.Reason {
		case "needs_me":
			needsMeCount++
			summary := it.Summary
			if summary == "" {
				summary = "needs decision"
			}
			lines = append(lines, fmt.Sprintf("needs_me %s: %s", it.Bead, summary))
		case "worker_blocked":
			lines = append(lines, fmt.Sprintf("worker_blocked %s: awaiting approval", it.Bead))
		case "blocked":
			lines = append(lines, fmt.Sprintf("blocked %s", it.Bead))
		case "pr_merged":
			prCount++
			lines = append(lines, "merged "+it.Summary)
		case "pr_ready":
			prCount++
			lines = append(lines, "ready "+it.Summary)
		case "pr_green":
			prCount++
			lines = append(lines, "green "+it.Summary)
		default:
			lines = append(lines, it.Reason+" "+it.Summary)
		}
	}
	if len(lines) > desktopMaxBodyLines {
		omitted := len(lines) - desktopMaxBodyLines
		lines = append(lines[:desktopMaxBodyLines], fmt.Sprintf("+%d more — run quartermaster context", omitted))
	}

	var title string
	switch {
	case needsMeCount == len(items):
		title = fmt.Sprintf("Quartermaster: %d beads need you", needsMeCount)
	case prCount == len(items):
		title = fmt.Sprintf("Quartermaster: %d PR updates", prCount)
	case needsMeCount > 0:
		title = fmt.Sprintf("Quartermaster: %d updates (%d needs_me)", len(items), needsMeCount)
	default:
		title = fmt.Sprintf("Quartermaster: %d updates", len(items))
	}
	return title, strings.Join(lines, " | ")
}

func coalescePendingRunners(runs []notifyPendingRunner) (string, string, NotifyRunnerInfo) {
	if len(runs) == 1 {
		return runs[0].Title, runs[0].Body, runs[0].Runner
	}
	var totalAcked, totalEligible int
	var totalDuration int64
	kindSet := map[string]bool{}
	status := "ok"
	rank := func(s string) int {
		switch s {
		case "timeout":
			return 2
		case "error":
			return 1
		default:
			return 0
		}
	}
	for _, r := range runs {
		totalAcked += r.Runner.Acked
		totalEligible += r.Runner.Eligible
		totalDuration += r.Runner.DurationMs
		for _, k := range r.Runner.Kinds {
			if k != "" {
				kindSet[k] = true
			}
		}
		if rank(r.Runner.ExitStatus) > rank(status) {
			status = r.Runner.ExitStatus
		}
	}
	kinds := make([]string, 0, len(kindSet))
	for k := range kindSet {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	info := NotifyRunnerInfo{
		ExitStatus: status,
		Acked:      totalAcked,
		Eligible:   totalEligible,
		Kinds:      kinds,
		DurationMs: totalDuration,
	}
	detail := ""
	if len(kinds) > 0 {
		detail = " (" + strings.Join(kinds, ", ") + ")"
	}
	title := fmt.Sprintf("Quartermaster: %d inbox runner updates", len(runs))
	body := fmt.Sprintf("Status %s across %d runs: handled %d/%d inbox %s%s.", status, len(runs), totalAcked, totalEligible, plural(totalEligible, "item"), detail)
	return title, body, info
}

func buildNotifyEnvelopeForSink(pendingItems []notifyPendingItem, pendingRunner []notifyPendingRunner, now time.Time) NotifyEnvelope {
	ev := NotifyEnvelope{
		Version: notifyEnvelopeVersion,
		At:      now.UTC().Format(time.RFC3339),
	}
	ordered := append([]notifyPendingItem(nil), pendingItems...)
	sort.Slice(ordered, func(i, j int) bool {
		pi, pj := desktopReasonPriority(ordered[i].Reason), desktopReasonPriority(ordered[j].Reason)
		if pi != pj {
			return pi < pj
		}
		if ordered[i].Bead != ordered[j].Bead {
			return ordered[i].Bead < ordered[j].Bead
		}
		return ordered[i].Key < ordered[j].Key
	})

	if len(ordered) > 0 {
		ev.Event = ordered[0].Reason
		ev.Title, ev.Body = formatNotifyItemsMessage(ordered)
		ev.Items = make([]NotifyItem, 0, len(ordered))
		for _, it := range ordered {
			ev.Items = append(ev.Items, NotifyItem{
				Bead:    it.Bead,
				Reason:  it.Reason,
				Summary: it.Summary,
				PR:      it.PR,
			})
		}
	}

	if len(pendingRunner) > 0 {
		rTitle, rBody, rInfo := coalescePendingRunners(pendingRunner)
		ev.Runner = &rInfo
		if len(ordered) == 0 {
			ev.Event = "inbox_runner"
			ev.Title = rTitle
			ev.Body = rBody
		} else {
			if ev.Body != "" && rBody != "" {
				ev.Body = ev.Body + " | inbox_runner: " + rBody
			} else if rBody != "" {
				ev.Body = rBody
			}
		}
	}
	return sanitizeNotifyEnvelope(ev)
}

var notifySem = make(chan struct{}, notifyMaxConcurrent)

func recordNotifyOutcome(name string, attemptAt, finishedAt time.Time, res notifyDeliveryResult) error {
	return withNotifyLock(func() error {
		st, err := loadNotifyStatus()
		if err != nil {
			return err
		}
		ss := st.Sinks[name]
		if res.State == "submitted" {
			ss.TotalSent++
			ss.LastSuccessAt = finishedAt
		} else {
			ss.TotalFailed++
		}
		if !ss.LastAttemptAt.After(attemptAt) {
			ss.LastAttemptAt = attemptAt
			ss.LastState = res.State
			ss.LastError = res.Err
		}
		st.Sinks[name] = ss
		return saveNotifyStatus(st)
	})
}

func runNotifyJobs(ctx context.Context, cfg Config, env notifyEnv, jobs []notifyJob, wg *sync.WaitGroup) {
	if len(jobs) == 0 {
		return
	}
	inboxURL := resolveSafeTapInboxURL(cfg, env)
	for i := range jobs {
		jobs[i].Envelope.InboxURL = inboxURL
		jobs[i].Envelope = sanitizeNotifyEnvelope(jobs[i].Envelope)
	}

	execOne := func(job notifyJob) {
		res := deliverNotifySink(ctx, job.Sink, job.Envelope, env)
		finishedAt := env.clock()
		if finishedAt.Before(job.AttemptAt) {
			finishedAt = job.AttemptAt
		}
		if err := recordNotifyOutcome(job.Sink.Name, job.AttemptAt, finishedAt, res); err != nil {
			env.log("notify status save (%s): %v", job.Sink.Name, err)
		}
		if res.State != "submitted" {
			env.log("notify sink %s (%s): %s (%s)", job.Sink.Name, job.Sink.redactedTarget(), res.State, res.Err)
		}
	}

	if wg != nil {
		for _, job := range jobs {
			select {
			case notifySem <- struct{}{}:
				wg.Add(1)
				go func(j notifyJob) {
					defer wg.Done()
					defer func() { <-notifySem }()
					execOne(j)
				}(job)
			default:
				env.log("notify sink %s: skipped dispatch (concurrency limit reached)", job.Sink.Name)
			}
		}
		return
	}

	var localWG sync.WaitGroup
	for _, job := range jobs {
		localWG.Add(1)
		go func(j notifyJob) {
			defer localWG.Done()
			execOne(j)
		}(job)
	}
	localWG.Wait()
}

func collectDueNotifyJobs(st *notifyStatusFile, sinks []notifySink, now time.Time) []notifyJob {
	var jobs []notifyJob
	for _, sink := range sinks {
		ss := st.Sinks[sink.Name]
		if len(ss.PendingItems) == 0 && len(ss.PendingRunner) == 0 {
			continue
		}
		if sink.Cooldown > 0 && !ss.LastAttemptAt.IsZero() && now.Sub(ss.LastAttemptAt) < sink.Cooldown {
			ss.LastState = "skipped_cooldown"
			st.Sinks[sink.Name] = ss
			continue
		}
		ev := buildNotifyEnvelopeForSink(ss.PendingItems, ss.PendingRunner, now)
		ss.PendingItems = nil
		ss.PendingRunner = nil
		ss.LastAttemptAt = now
		st.Sinks[sink.Name] = ss
		jobs = append(jobs, notifyJob{
			Sink:      sink,
			Envelope:  ev,
			AttemptAt: now,
		})
	}
	return jobs
}

// reconcileNotifyWith updates durable episode deduplication and per-sink
// cooldown queues in <state>/notify/status.json and dispatches any due
// outbound sink notifications independently of [desktop].
func reconcileNotifyWith(ctx context.Context, cfg Config, env notifyEnv, threads []Thread, milestones []desktopMilestone, now time.Time, wg *sync.WaitGroup) error {
	if !cfg.Notify.enabled() {
		return nil
	}
	sinks := configuredNotifySinks(cfg)
	if len(sinks) == 0 {
		return nil
	}
	var jobs []notifyJob
	err := withNotifyLock(func() error {
		st, err := loadNotifyStatus()
		if err != nil {
			return err
		}
		active := map[string]bool{}
		var newlyDue []notifyPendingItem

		for _, th := range threads {
			reason := desktopReason(th)
			if reason == "" {
				// Preserve worker_blocked through transient Herdr observation gaps.
				if ep, ok := st.Episodes[th.Bead.ID]; ok && ep.Active && ep.Reason == "worker_blocked" && th.Agent == nil && th.Bead.Status == StatusInProgress {
					active[th.Bead.ID] = true
				}
				continue
			}
			id := th.Bead.ID
			hash := desktopEpisodeHash(th, reason)
			ep, exists := st.Episodes[id]
			isNew := !exists || !ep.Active || ep.Reason != reason || (hash != "" && ep.PromptHash != "" && hash != ep.PromptHash)
			if isNew {
				ep = notifyEpisode{
					Bead:       id,
					Reason:     reason,
					PromptHash: hash,
					Detected:   now,
					Active:     true,
				}
				prRef := ""
				if th.PR != nil {
					prRef = prName(*th.PR)
				}
				newlyDue = append(newlyDue, notifyPendingItem{
					Key:     fmt.Sprintf("episode:%s:%s", id, reason),
					Bead:    id,
					Reason:  reason,
					Summary: sanitizeDesktopLine(notifyItemSummaryForThread(th, reason), notifyMaxSummaryRunes),
					PR:      prRef,
					At:      now,
				})
			} else if hash != "" {
				ep.PromptHash = hash
			}
			st.Episodes[id] = ep
			active[id] = true
		}

		for _, m := range milestones {
			if _, exists := st.Episodes[m.Key]; exists {
				continue
			}
			st.Episodes[m.Key] = notifyEpisode{
				Bead:     m.Bead,
				Reason:   m.Reason,
				Detected: now,
				Active:   false,
				Ended:    now,
			}
			newlyDue = append(newlyDue, notifyPendingItem{
				Key:     m.Key,
				Bead:    m.Bead,
				Reason:  m.Reason,
				Summary: sanitizeDesktopLine(m.Summary, notifyMaxSummaryRunes),
				PR:      sanitizeDesktopLine(m.PR, 120),
				At:      now,
			})
		}

		for id, ep := range st.Episodes {
			if ep.Active && !active[id] {
				ep.Active = false
				ep.Ended = now
				st.Episodes[id] = ep
			}
		}

		// Retain all active episodes plus the latest notifyMaxInactiveEp inactive episodes.
		var inactive []string
		for id, ep := range st.Episodes {
			if !ep.Active {
				inactive = append(inactive, id)
			}
		}
		sort.Slice(inactive, func(i, j int) bool {
			ei, ej := st.Episodes[inactive[i]], st.Episodes[inactive[j]]
			if !ei.Ended.Equal(ej.Ended) {
				return ei.Ended.After(ej.Ended)
			}
			return inactive[i] < inactive[j]
		})
		for i := notifyMaxInactiveEp; i < len(inactive); i++ {
			delete(st.Episodes, inactive[i])
		}

		for _, it := range newlyDue {
			for _, sink := range sinks {
				if !sink.subscribes(it.Reason) {
					continue
				}
				ss := st.Sinks[sink.Name]
				ss.PendingItems = appendPendingNotifyItem(ss.PendingItems, it)
				st.Sinks[sink.Name] = ss
			}
		}

		jobs = collectDueNotifyJobs(&st, sinks, now)
		return saveNotifyStatus(st)
	})
	if err != nil {
		return err
	}
	runNotifyJobs(ctx, cfg, env, jobs, wg)
	return nil
}

// dispatchNotifyInboxRunner enqueues an inbox_runner event for all subscribed
// outbound sinks and delivers every sink not currently in cooldown. Any sink
// in cooldown retains the event durably in <state>/notify/status.json for
// subsequent flushing.
func dispatchNotifyInboxRunner(ctx context.Context, cfg Config, env notifyEnv, title, body string, runner NotifyRunnerInfo, now time.Time) error {
	if !cfg.Notify.enabled() {
		return nil
	}
	sinks := configuredNotifySinks(cfg)
	if len(sinks) == 0 {
		return nil
	}
	cleanEnv := sanitizeNotifyEnvelope(NotifyEnvelope{
		Version: notifyEnvelopeVersion,
		Event:   "inbox_runner",
		At:      now.UTC().Format(time.RFC3339),
		Title:   title,
		Body:    body,
		Runner:  &runner,
	})
	entry := notifyPendingRunner{
		Title:  cleanEnv.Title,
		Body:   cleanEnv.Body,
		Runner: *cleanEnv.Runner,
		At:     now,
	}

	var jobs []notifyJob
	err := withNotifyLock(func() error {
		st, err := loadNotifyStatus()
		if err != nil {
			return err
		}
		for _, sink := range sinks {
			if !sink.subscribes("inbox_runner") {
				continue
			}
			ss := st.Sinks[sink.Name]
			ss.PendingRunner = appendPendingNotifyRunner(ss.PendingRunner, entry)
			st.Sinks[sink.Name] = ss
		}
		jobs = collectDueNotifyJobs(&st, sinks, now)
		return saveNotifyStatus(st)
	})
	if err != nil {
		return err
	}
	runNotifyJobs(ctx, cfg, env, jobs, nil)
	return nil
}

// NotifyStatusReport is emitted by `quartermaster notify status [--json]`.
type NotifyStatusReport struct {
	Enabled bool               `json:"enabled"`
	Sinks   []NotifySinkStatus `json:"sinks"`
}

// NotifySinkStatus describes one configured sink's redacted config and latest
// durable delivery state.
type NotifySinkStatus struct {
	Name            string    `json:"name"`
	Type            string    `json:"type"`
	Target          string    `json:"target"`
	Format          string    `json:"format,omitempty"`
	Headers         []string  `json:"headers,omitempty"`
	Events          []string  `json:"events"`
	TimeoutSeconds  int       `json:"timeout_seconds"`
	CooldownSeconds int       `json:"cooldown_seconds"`
	Pending         int       `json:"pending"`
	LastState       string    `json:"last_state,omitempty"`
	LastAttemptAt   time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt   time.Time `json:"last_success_at,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
	TotalSent       int       `json:"total_sent"`
	TotalFailed     int       `json:"total_failed"`
}

func buildNotifyStatus(cfg Config) (NotifyStatusReport, error) {
	st, err := loadNotifyStatus()
	if err != nil {
		return NotifyStatusReport{}, err
	}
	sinks := configuredNotifySinks(cfg)
	rep := NotifyStatusReport{
		Enabled: cfg.Notify.enabled(),
		Sinks:   make([]NotifySinkStatus, 0, len(sinks)),
	}
	for _, s := range sinks {
		ss := st.Sinks[s.Name]
		item := NotifySinkStatus{
			Name:            s.Name,
			Type:            s.Type,
			Target:          s.redactedTarget(),
			Events:          s.Events,
			TimeoutSeconds:  int(s.Timeout / time.Second),
			CooldownSeconds: int(s.Cooldown / time.Second),
			Pending:         len(ss.PendingItems) + len(ss.PendingRunner),
			LastState:       ss.LastState,
			LastAttemptAt:   ss.LastAttemptAt,
			LastSuccessAt:   ss.LastSuccessAt,
			LastError:       ss.LastError,
			TotalSent:       ss.TotalSent,
			TotalFailed:     ss.TotalFailed,
		}
		if s.Webhook != nil {
			item.Format = s.Webhook.effectiveFormat()
			item.Headers = webhookHeaderSummary(*s.Webhook)
		}
		rep.Sinks = append(rep.Sinks, item)
	}
	return rep, nil
}

func writeNotifyStatus(w io.Writer, rep NotifyStatusReport, asJSON bool, now time.Time) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	if len(rep.Sinks) == 0 {
		fmt.Fprintln(w, "notify: disabled (no sinks configured; add [[notify.webhooks]] or [[notify.commands]] to config.toml)")
		return nil
	}
	state := "enabled"
	if !rep.Enabled {
		state = "disabled ([notify] enabled = false)"
	}
	fmt.Fprintf(w, "notify: %s · %d %s\n", state, len(rep.Sinks), plural(len(rep.Sinks), "sink"))
	for _, s := range rep.Sinks {
		kindLabel := s.Type
		if s.Format != "" {
			kindLabel = s.Type + " · " + s.Format
		}
		fmt.Fprintf(w, "- %s (%s · %s)\n", s.Name, kindLabel, s.Target)
		fmt.Fprintf(w, "  events:   %s\n", strings.Join(s.Events, ", "))
		fmt.Fprintf(w, "  timing:   timeout=%ds cooldown=%ds\n", s.TimeoutSeconds, s.CooldownSeconds)
		if len(s.Headers) > 0 {
			fmt.Fprintf(w, "  headers:  %s\n", strings.Join(s.Headers, ", "))
		}
		delivery := "never"
		if s.LastState != "" {
			delivery = s.LastState
			if !s.LastAttemptAt.IsZero() {
				delivery += " · last_attempt=" + ago(s.LastAttemptAt, now)
			}
			if !s.LastSuccessAt.IsZero() {
				delivery += " · last_success=" + ago(s.LastSuccessAt, now)
			}
			delivery += fmt.Sprintf(" · sent=%d failed=%d", s.TotalSent, s.TotalFailed)
		}
		fmt.Fprintf(w, "  delivery: %s\n", delivery)
		if s.Pending > 0 {
			fmt.Fprintf(w, "  pending:  %d queued (waiting for cooldown)\n", s.Pending)
		}
		if s.LastError != "" {
			fmt.Fprintf(w, "  error:    %s\n", s.LastError)
		}
	}
	return nil
}

type notifyTestSinkResult struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Target     string `json:"target"`
	State      string `json:"state"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

func runNotifyTest(ctx context.Context, cfg Config, env notifyEnv, sinkFilter string, out io.Writer, asJSON bool) error {
	sinks := configuredNotifySinks(cfg)
	if len(sinks) == 0 {
		return errors.New("no notification sinks configured; add [[notify.webhooks]] or [[notify.commands]] to config.toml")
	}
	var targets []notifySink
	if sinkFilter != "" {
		for _, s := range sinks {
			if s.Name == sinkFilter {
				targets = append(targets, s)
				break
			}
		}
		if len(targets) == 0 {
			return fmt.Errorf("unknown notification sink %q", sinkFilter)
		}
	} else {
		targets = sinks
	}

	now := env.clock()
	ev := sanitizeNotifyEnvelope(NotifyEnvelope{
		Version:  notifyEnvelopeVersion,
		Event:    "test",
		At:       now.UTC().Format(time.RFC3339),
		Title:    "Quartermaster: test notification",
		Body:     "Synthetic test notification from quartermaster notify test.",
		InboxURL: resolveSafeTapInboxURL(cfg, env),
	})

	results := make([]notifyTestSinkResult, len(targets))
	failed := 0
	for i, sink := range targets {
		res := deliverNotifySink(ctx, sink, ev, env)
		finishedAt := env.clock()
		if finishedAt.Before(now) {
			finishedAt = now
		}
		_ = recordNotifyOutcome(sink.Name, now, finishedAt, res)
		results[i] = notifyTestSinkResult{
			Name:       sink.Name,
			Type:       sink.Type,
			Target:     sink.redactedTarget(),
			State:      res.State,
			DurationMs: res.Duration.Milliseconds(),
			Error:      res.Err,
		}
		if res.State != "submitted" {
			failed++
		}
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		_ = enc.Encode(struct {
			Event   string                 `json:"event"`
			Results []notifyTestSinkResult `json:"results"`
		}{Event: "test", Results: results})
	} else {
		for _, r := range results {
			mark := "ok  "
			detail := fmt.Sprintf("%s (%dms)", r.State, r.DurationMs)
			if r.State != "submitted" {
				mark = "FAIL"
				if r.Error != "" {
					detail = fmt.Sprintf("%s (%dms): %s", r.State, r.DurationMs, r.Error)
				}
			}
			fmt.Fprintf(out, "%s  %s (%s · %s) — %s\n", mark, r.Name, r.Type, r.Target, detail)
		}
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d notification %s failed", failed, len(results), plural(len(results), "sink"))
	}
	return nil
}

const notifyUsage = "usage: quartermaster notify status [--json] | quartermaster notify test [--sink <name>] [--json]"

func notifyCmd(cfg Config, env notifyEnv, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(notifyUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "status":
		fs := flag.NewFlagSet("notify status", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		asJSON := fs.Bool("json", false, "emit JSON output")
		pos, err := interspersed(fs, rest)
		if err != nil {
			return fmt.Errorf("%v\n%s", err, notifyUsage)
		}
		if len(pos) != 0 {
			return errors.New(notifyUsage)
		}
		rep, err := buildNotifyStatus(cfg)
		if err != nil {
			return err
		}
		return writeNotifyStatus(out, rep, *asJSON, env.clock())
	case "test":
		fs := flag.NewFlagSet("notify test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		sink := fs.String("sink", "", "target one configured sink by name")
		asJSON := fs.Bool("json", false, "emit JSON output")
		pos, err := interspersed(fs, rest)
		if err != nil {
			return fmt.Errorf("%v\n%s", err, notifyUsage)
		}
		if len(pos) != 0 {
			return errors.New(notifyUsage)
		}
		return runNotifyTest(context.Background(), cfg, env, *sink, out, *asJSON)
	default:
		return fmt.Errorf("unknown notify command %q\n%s", sub, notifyUsage)
	}
}
