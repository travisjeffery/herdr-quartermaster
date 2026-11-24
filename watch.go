package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Declarative per-bead watches (`quartermaster watch`).
//
// A watch attaches a read-only external condition to an open or active bead so
// the ticker can notify the coordinator (and optionally wake an idle worker)
// once the condition is met, without hardcoding custom pollers. State lives in
// <state dir>/watches/watches.json (0600 inside a 0700 directory) guarded by a
// cross-process flock so CLI commands and the ticker never race.

type WatchKind string

const (
	WatchKindGHRelease WatchKind = "gh-release"
	WatchKindGHPR      WatchKind = "gh-pr"
	WatchKindGHRun     WatchKind = "gh-run"
	WatchKindHTTP      WatchKind = "http"
	WatchKindCmd       WatchKind = "cmd"
)

type WatchTriggerAction string

const (
	WatchOnTriggerInbox WatchTriggerAction = "inbox"
	WatchOnTriggerWake  WatchTriggerAction = "wake"
)

type WatchStatus string

const (
	WatchStatusActive    WatchStatus = "active"
	WatchStatusTriggered WatchStatus = "triggered"
	WatchStatusExpired   WatchStatus = "expired"
	WatchStatusRemoved   WatchStatus = "removed"
)

const (
	defaultWatchEvery     = 5 * time.Minute
	minWatchEvery         = 30 * time.Second
	defaultWatchTimeout   = 15 * time.Second
	maxWatchCmdTimeout    = 60 * time.Second
	defaultWatchParallel  = 4
	watchHTTPMaxBody      = 1 << 20 // 1 MiB
	watchCmdMaxStdout     = 1 << 20 // 1 MiB
	watchCmdMaxStderr     = 64 << 10
	watchMaxRedirects     = 10
	watchBackoffCeiling   = 6 * time.Hour
	watchBackoffMinCap    = time.Hour
	watchStoreVersion     = 1
	watchWakeHeaderPrefix = "[" + toolName + ": automated watch triggered, not the user, approves nothing]"
)

// WatchesConfig is [watches] in config.toml. AllowCmd fails closed (false by
// default): cmd watches are refused at both add and evaluation time unless
// explicitly enabled.
type WatchesConfig struct {
	AllowCmd      bool `toml:"allow_cmd"`
	MaxConcurrent int  `toml:"max_concurrent"`
}

func (c WatchesConfig) concurrency() int {
	if c.MaxConcurrent > 0 {
		return c.MaxConcurrent
	}
	return defaultWatchParallel
}

// WatchParams holds typed parameters for one watch kind.
type WatchParams struct {
	GHRelease *GHReleaseWatchParams `json:"gh_release,omitempty"`
	GHPR      *GHPRWatchParams      `json:"gh_pr,omitempty"`
	GHRun     *GHRunWatchParams     `json:"gh_run,omitempty"`
	HTTP      *HTTPWatchParams      `json:"http,omitempty"`
	Cmd       *CmdWatchParams       `json:"cmd,omitempty"`
}

type GHReleaseWatchParams struct {
	Repo  string `json:"repo"`
	After string `json:"after,omitempty"`
	Match string `json:"match,omitempty"`
}

type GHPRWatchParams struct {
	Repo            string `json:"repo"`
	PR              int    `json:"pr"`
	Expect          string `json:"expect,omitempty"`
	HeadChangedFrom string `json:"head_changed_from,omitempty"`
}

type GHRunWatchParams struct {
	Repo   string `json:"repo"`
	RunID  int64  `json:"run_id"`
	Expect string `json:"expect"`
}

type HTTPWatchParams struct {
	URL            string `json:"url"`
	ExpectStatus   string `json:"expect_status"`
	ExpectJSON     string `json:"expect_json,omitempty"`
	ExpectContains string `json:"expect_contains,omitempty"`
}

type CmdWatchParams struct {
	Cmd          string   `json:"cmd"`
	Args         []string `json:"args,omitempty"`
	ExpectExit   int      `json:"expect_exit"`
	ExpectStdout string   `json:"expect_stdout,omitempty"`
	Timeout      string   `json:"timeout,omitempty"`
}

// Watch is one persisted declarative watch attached to a bead.
type Watch struct {
	ID                string             `json:"id"`
	Bead              string             `json:"bead"`
	Kind              WatchKind          `json:"kind"`
	Label             string             `json:"label"`
	Params            WatchParams        `json:"params"`
	Every             string             `json:"every"`
	CreatedAt         time.Time          `json:"created_at"`
	ExpiresAt         *time.Time         `json:"expires_at,omitempty"`
	OnTrigger         WatchTriggerAction `json:"on_trigger"`
	Status            WatchStatus        `json:"status"`
	LastChecked       *time.Time         `json:"last_checked,omitempty"`
	NextCheck         time.Time          `json:"next_check"`
	LastError         string             `json:"last_error,omitempty"`
	ConsecutiveErrors int                `json:"consecutive_errors,omitempty"`
	TriggeredAt       *time.Time         `json:"triggered_at,omitempty"`
	Evidence          string             `json:"evidence,omitempty"`
}

func (w Watch) everyDuration() time.Duration {
	d, err := parseWatchDuration(w.Every)
	if err != nil || d < minWatchEvery {
		return defaultWatchEvery
	}
	return d
}

type watchStore struct {
	Version int     `json:"version"`
	Watches []Watch `json:"watches"`
}

func watchesDir() string      { return filepath.Join(stateDir(), "watches") }
func watchesPath() string     { return filepath.Join(watchesDir(), "watches.json") }
func watchesLockPath() string { return filepath.Join(watchesDir(), "watches.lock") }

// watchStoreMu serializes in-process callers alongside the cross-process flock.
var watchStoreMu sync.Mutex

// withWatchStoreLock runs fn while holding an exclusive flock on watches.lock.
// Never call slow network or subprocess probes inside fn.
func withWatchStoreLock(fn func(*watchStore) (bool, error)) error {
	watchStoreMu.Lock()
	defer watchStoreMu.Unlock()

	if err := rpcPrivateDir(watchesDir()); err != nil {
		return err
	}
	lf, err := os.OpenFile(watchesLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := os.Chmod(watchesLockPath(), 0o600); err != nil {
		return err
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock watches store: %w", err)
	}
	defer func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }()

	store, err := readWatchStoreLocked()
	if err != nil {
		return err
	}
	dirty, err := fn(store)
	if err != nil {
		return err
	}
	if !dirty {
		return nil
	}
	return writeWatchStoreLocked(store)
}

func readWatchStoreLocked() (*watchStore, error) {
	b, err := os.ReadFile(watchesPath())
	if errors.Is(err, fs.ErrNotExist) {
		return &watchStore{Version: watchStoreVersion, Watches: []Watch{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var s watchStore
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", watchesPath(), err)
	}
	if s.Version == 0 {
		s.Version = watchStoreVersion
	}
	if s.Watches == nil {
		s.Watches = []Watch{}
	}
	return &s, nil
}

func writeWatchStoreLocked(s *watchStore) error {
	if s.Version == 0 {
		s.Version = watchStoreVersion
	}
	if s.Watches == nil {
		s.Watches = []Watch{}
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := watchesDir()
	f, err := os.CreateTemp(dir, ".watches-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, watchesPath()); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// loadWatches returns a snapshot of persisted watches without mutating them.
func loadWatches() ([]Watch, error) {
	if _, err := os.Stat(watchesPath()); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var out []Watch
	err := withWatchStoreLock(func(s *watchStore) (bool, error) {
		out = slices.Clone(s.Watches)
		return false, nil
	})
	return out, err
}

// activeWatchesByBead loads active watches grouped by bead ID for read-only
// views such as `quartermaster context`. Missing or unreadable stores return nil.
func activeWatchesByBead() map[string][]Watch {
	ws, err := loadWatches()
	if err != nil || len(ws) == 0 {
		return nil
	}
	byBead := map[string][]Watch{}
	for _, w := range ws {
		if w.Status == WatchStatusActive {
			byBead[w.Bead] = append(byBead[w.Bead], w)
		}
	}
	if len(byBead) == 0 {
		return nil
	}
	for id := range byBead {
		slices.SortFunc(byBead[id], compareWatches)
	}
	return byBead
}

func compareWatches(a, b Watch) int {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		if a.CreatedAt.Before(b.CreatedAt) {
			return -1
		}
		return 1
	}
	return strings.Compare(a.ID, b.ID)
}

func formatThreadWatches(ws []Watch) string {
	parts := make([]string, 0, len(ws))
	for _, w := range ws {
		meta := []string{string(w.Kind), "every " + w.Every}
		if w.OnTrigger == WatchOnTriggerWake {
			meta = append(meta, "wake")
		}
		if w.ConsecutiveErrors > 0 {
			meta = append(meta, fmt.Sprintf("err×%d", w.ConsecutiveErrors))
		}
		parts = append(parts, fmt.Sprintf("[%s] %s (%s)", w.ID, shortTitle(w.Label, 48), strings.Join(meta, ", ")))
	}
	return strings.Join(parts, "; ")
}

func newWatchID(existing []Watch) (string, error) {
	used := make(map[string]bool, len(existing))
	for _, w := range existing {
		used[w.ID] = true
	}
	var buf [4]byte
	for range 16 {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", err
		}
		id := hex.EncodeToString(buf[:])
		if !used[id] {
			return id, nil
		}
	}
	return "", errors.New("could not allocate unique watch id")
}

// WatchEnv injects external dependencies for watch CLI commands and ticker
// evaluation so unit tests never run real gh/bd/herdr/network calls unless
// explicitly configured.
type WatchEnv struct {
	Show       func(id string) (Bead, error)
	Note       func(id, text string) error
	RunGH      func(ctx context.Context, args ...string) ([]byte, error)
	RunCmd     func(ctx context.Context, prog string, args []string) (exitCode int, stdout string, err error)
	HTTPClient *http.Client
	AgentFor   func(b Bead) (*Agent, error)
	Tell       func(b Bead, a Agent, text string) error
	Now        func() time.Time
	Logf       func(format string, args ...any)
}

func liveWatchEnv(cfg Config, h Herdr) WatchEnv {
	return WatchEnv{
		Show: Beads{}.Show,
		Note: Beads{}.Note,
		RunGH: func(ctx context.Context, args ...string) ([]byte, error) {
			return runWatchGH(ctx, cfg.Repo, args...)
		},
		RunCmd: runWatchCmd,
		AgentFor: func(b Bead) (*Agent, error) {
			host := beadHost(b)
			if host == "" {
				host = localHost
			}
			return agentOn(cfg, h)(host, agentName(b.ID))
		},
		Tell: func(b Bead, a Agent, text string) error {
			_, err := tellCmd(cfg, h, func(id string) (Bead, error) {
				if id == b.ID {
					return b, nil
				}
				return Beads{}.Show(id)
			}, b.ID, text)
			return err
		},
		Now:  time.Now,
		Logf: func(string, ...any) {},
	}
}

func (e WatchEnv) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e WatchEnv) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

func runWatchGH(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("gh %s timed out", strings.Join(args[:min(2, len(args))], " "))
		}
		return nil, fmt.Errorf("gh %s: %v: %s", strings.Join(args[:min(2, len(args))], " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// limitedBuffer caps captured bytes while draining any excess so a noisy
// subprocess cannot OOM or stall on a full pipe.
type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if rem := l.limit - l.buf.Len(); rem > 0 {
		if len(p) > rem {
			l.buf.Write(p[:rem])
			l.truncated = true
		} else {
			l.buf.Write(p)
		}
	} else if n > 0 {
		l.truncated = true
	}
	return n, nil
}

// runWatchCmd executes prog with args directly (never through a shell), killing
// its process group on timeout.
func runWatchCmd(ctx context.Context, prog string, args []string) (int, string, error) {
	cmd := exec.CommandContext(ctx, prog, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second
	stdout := &limitedBuffer{limit: watchCmdMaxStdout}
	stderr := &limitedBuffer{limit: watchCmdMaxStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return -1, "", fmt.Errorf("command %q timed out: %w", prog, ctx.Err())
	}
	if err == nil {
		return 0, stdout.buf.String(), nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), stdout.buf.String(), nil
	}
	msg := strings.TrimSpace(stderr.buf.String())
	if msg != "" {
		return -1, "", fmt.Errorf("exec %q: %w: %s", prog, err, msg)
	}
	return -1, "", fmt.Errorf("exec %q: %w", prog, err)
}

var (
	watchRepoRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	watchDayRE  = regexp.MustCompile(`^(\d+)d(.*)$`)
)

// parseWatchDuration parses Go durations plus day prefixes such as "7d" or "1d12h".
func parseWatchDuration(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, errors.New("duration is empty")
	}
	var total time.Duration
	if m := watchDayRE.FindStringSubmatch(s); m != nil {
		days, err := strconv.Atoi(m[1])
		if err != nil || days <= 0 || days > 3650 {
			return 0, fmt.Errorf("invalid duration %q", raw)
		}
		total = time.Duration(days) * 24 * time.Hour
		s = m[2]
		if s == "" {
			return total, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid duration %q (examples: 30s, 5m, 2h, 7d)", raw)
	}
	return total + d, nil
}

func validateWatchRepo(repo string) error {
	if !watchRepoRE.MatchString(repo) || strings.Contains(repo, "..") {
		return fmt.Errorf("invalid --repo %q (want owner/repo)", repo)
	}
	return nil
}

func validateHTTPStatusSpec(spec string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(spec))
	if s == "" {
		return "200", nil
	}
	if len(s) == 3 && s[1] == 'x' && s[2] == 'x' && s[0] >= '1' && s[0] <= '5' {
		return s, nil
	}
	code, err := strconv.Atoi(s)
	if err != nil || code < 100 || code > 599 {
		return "", fmt.Errorf("invalid --expect-status %q (want 100-599 or 1xx-5xx)", spec)
	}
	return strconv.Itoa(code), nil
}

func matchHTTPStatus(spec string, code int) bool {
	if len(spec) == 3 && spec[1] == 'x' && spec[2] == 'x' {
		class := int(spec[0] - '0')
		return code >= class*100 && code < (class+1)*100
	}
	want, err := strconv.Atoi(spec)
	return err == nil && code == want
}

func expectsHTTPRedirectStatus(spec string) bool {
	if spec == "3xx" {
		return true
	}
	code, err := strconv.Atoi(spec)
	return err == nil && code >= 300 && code <= 399
}

func validateHTTPWatchURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" || strings.ContainsAny(raw, "\r\n\x00") {
		return nil, fmt.Errorf("invalid --url %q", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid --url %q: %w", raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("--url %q: only http and https schemes are allowed", raw)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("--url %q: host is required", raw)
	}
	return u, nil
}

func sanitizeURLForDisplay(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	return u.String()
}

func validateWatch(cfg Config, w *Watch) error {
	if strings.TrimSpace(w.Bead) == "" {
		return errors.New("bead id is required")
	}
	if w.Every == "" {
		w.Every = "5m"
	}
	every, err := parseWatchDuration(w.Every)
	if err != nil {
		return fmt.Errorf("--every: %w", err)
	}
	if every < minWatchEvery {
		return fmt.Errorf("--every %s is below the minimum of %s", w.Every, minWatchEvery)
	}
	switch w.OnTrigger {
	case "":
		w.OnTrigger = WatchOnTriggerInbox
	case WatchOnTriggerInbox, WatchOnTriggerWake:
	default:
		return fmt.Errorf("invalid --on-trigger %q (want inbox or wake)", w.OnTrigger)
	}
	switch w.Kind {
	case WatchKindGHRelease:
		p := w.Params.GHRelease
		if p == nil {
			return errors.New("gh-release params are required")
		}
		if err := validateWatchRepo(p.Repo); err != nil {
			return err
		}
		p.After = strings.TrimSpace(p.After)
		p.Match = strings.TrimSpace(p.Match)
		if p.After == "" && p.Match == "" {
			return errors.New("gh-release requires --after <tag> and/or --match <prefix-or-glob>")
		}
		if p.Match != "" {
			if _, err := path.Match(p.Match, ""); err != nil {
				return fmt.Errorf("invalid --match %q: %w", p.Match, err)
			}
		}
		if strings.TrimSpace(w.Label) == "" {
			w.Label = defaultWatchLabel(*w)
		}
	case WatchKindGHPR:
		p := w.Params.GHPR
		if p == nil {
			return errors.New("gh-pr params are required")
		}
		if err := validateWatchRepo(p.Repo); err != nil {
			return err
		}
		if p.PR <= 0 {
			return errors.New("gh-pr requires --pr <number>")
		}
		p.Expect = strings.ToLower(strings.TrimSpace(p.Expect))
		p.HeadChangedFrom = strings.TrimSpace(p.HeadChangedFrom)
		if p.Expect == "" && p.HeadChangedFrom == "" {
			return errors.New("gh-pr requires --expect merged|closed|approved|checks-green or --head-changed-from <sha>")
		}
		switch p.Expect {
		case "", "merged", "closed", "approved", "checks-green":
		default:
			return fmt.Errorf("invalid gh-pr --expect %q (want merged, closed, approved or checks-green)", p.Expect)
		}
		if strings.TrimSpace(w.Label) == "" {
			w.Label = defaultWatchLabel(*w)
		}
	case WatchKindGHRun:
		p := w.Params.GHRun
		if p == nil {
			return errors.New("gh-run params are required")
		}
		if err := validateWatchRepo(p.Repo); err != nil {
			return err
		}
		if p.RunID <= 0 {
			return errors.New("gh-run requires --run <id>")
		}
		p.Expect = strings.ToLower(strings.TrimSpace(p.Expect))
		if p.Expect == "" {
			p.Expect = "completed"
		}
		switch p.Expect {
		case "completed", "success", "failure":
		default:
			return fmt.Errorf("invalid gh-run --expect %q (want completed, success or failure)", p.Expect)
		}
		if strings.TrimSpace(w.Label) == "" {
			w.Label = defaultWatchLabel(*w)
		}
	case WatchKindHTTP:
		p := w.Params.HTTP
		if p == nil {
			return errors.New("http params are required")
		}
		if _, err := validateHTTPWatchURL(p.URL); err != nil {
			return err
		}
		st, err := validateHTTPStatusSpec(p.ExpectStatus)
		if err != nil {
			return err
		}
		p.ExpectStatus = st
		if p.ExpectJSON != "" {
			key, _, ok := strings.Cut(p.ExpectJSON, "=")
			if !ok || strings.TrimSpace(key) == "" {
				return fmt.Errorf("invalid --expect-json %q (want dot.path=value)", p.ExpectJSON)
			}
		}
		if strings.TrimSpace(w.Label) == "" {
			w.Label = defaultWatchLabel(*w)
		}
	case WatchKindCmd:
		if !cfg.Watches.AllowCmd {
			return errors.New("cmd watches are disabled; set [watches] allow_cmd = true in config.toml to enable them")
		}
		p := w.Params.Cmd
		if p == nil || strings.TrimSpace(p.Cmd) == "" {
			return errors.New("cmd requires --cmd <program>")
		}
		if strings.ContainsAny(p.Cmd, "\r\n\x00") {
			return fmt.Errorf("invalid --cmd %q", p.Cmd)
		}
		for _, a := range p.Args {
			if strings.ContainsRune(a, 0) {
				return errors.New("cmd --arg must not contain NUL bytes")
			}
		}
		if p.Timeout == "" {
			p.Timeout = "15s"
		}
		td, err := parseWatchDuration(p.Timeout)
		if err != nil {
			return fmt.Errorf("--timeout: %w", err)
		}
		if td > maxWatchCmdTimeout {
			return fmt.Errorf("--timeout %s exceeds maximum %s", p.Timeout, maxWatchCmdTimeout)
		}
		if strings.TrimSpace(w.Label) == "" {
			w.Label = defaultWatchLabel(*w)
		}
	default:
		return fmt.Errorf("unknown watch kind %q (want gh-release, gh-pr, gh-run, http or cmd)", w.Kind)
	}
	w.Label = strings.TrimSpace(w.Label)
	return nil
}

func defaultWatchLabel(w Watch) string {
	switch w.Kind {
	case WatchKindGHRelease:
		p := w.Params.GHRelease
		var conds []string
		if p.After != "" {
			conds = append(conds, "after "+p.After)
		}
		if p.Match != "" {
			conds = append(conds, "match "+p.Match)
		}
		return fmt.Sprintf("release %s (%s)", p.Repo, strings.Join(conds, ", "))
	case WatchKindGHPR:
		p := w.Params.GHPR
		var conds []string
		if p.Expect != "" {
			conds = append(conds, p.Expect)
		}
		if p.HeadChangedFrom != "" {
			conds = append(conds, "head!="+shortTitle(p.HeadChangedFrom, 12))
		}
		return fmt.Sprintf("PR %s#%d (%s)", p.Repo, p.PR, strings.Join(conds, ", "))
	case WatchKindGHRun:
		p := w.Params.GHRun
		return fmt.Sprintf("run %s#%d (%s)", p.Repo, p.RunID, p.Expect)
	case WatchKindHTTP:
		p := w.Params.HTTP
		return fmt.Sprintf("HTTP %s %s", p.ExpectStatus, sanitizeURLForDisplay(p.URL))
	case WatchKindCmd:
		p := w.Params.Cmd
		return fmt.Sprintf("cmd %s (exit %d)", filepath.Base(p.Cmd), p.ExpectExit)
	default:
		return string(w.Kind)
	}
}

// watchEvalResult is the outcome of running one read-only watch probe.
type watchEvalResult struct {
	Triggered bool
	Evidence  string
}

func evaluateWatch(ctx context.Context, cfg Config, env WatchEnv, w Watch) (watchEvalResult, error) {
	switch w.Kind {
	case WatchKindGHRelease:
		return evalGHReleaseWatch(ctx, env, w.Params.GHRelease)
	case WatchKindGHPR:
		return evalGHPRWatch(ctx, env, w.Params.GHPR)
	case WatchKindGHRun:
		return evalGHRunWatch(ctx, env, w.Params.GHRun)
	case WatchKindHTTP:
		return evalHTTPWatch(ctx, env, w.Params.HTTP)
	case WatchKindCmd:
		return evalCmdWatch(ctx, cfg, env, w.Params.Cmd)
	default:
		return watchEvalResult{}, fmt.Errorf("unknown watch kind %q", w.Kind)
	}
}

func watchMatchTag(pattern, tag string) bool {
	if strings.ContainsAny(pattern, "*?[") {
		ok, err := path.Match(pattern, tag)
		return err == nil && ok
	}
	return strings.HasPrefix(tag, pattern)
}

func evalGHReleaseWatch(ctx context.Context, env WatchEnv, p *GHReleaseWatchParams) (watchEvalResult, error) {
	if p == nil {
		return watchEvalResult{}, errors.New("missing gh-release params")
	}
	if err := validateWatchRepo(p.Repo); err != nil {
		return watchEvalResult{}, err
	}
	if env.RunGH == nil {
		return watchEvalResult{}, errors.New("gh runner unavailable")
	}
	cctx, cancel := context.WithTimeout(ctx, defaultWatchTimeout)
	defer cancel()
	out, err := env.RunGH(cctx, "api", "repos/"+p.Repo+"/releases/latest")
	if err != nil {
		return watchEvalResult{}, err
	}
	var rel struct {
		TagName     string    `json:"tag_name"`
		Name        string    `json:"name"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
		Draft       bool      `json:"draft"`
	}
	if err := json.Unmarshal(out, &rel); err != nil {
		return watchEvalResult{}, fmt.Errorf("parse release JSON: %w", err)
	}
	tag := strings.TrimSpace(rel.TagName)
	if tag == "" || rel.Draft {
		return watchEvalResult{}, nil
	}
	if p.After != "" && tag == p.After {
		return watchEvalResult{}, nil
	}
	if p.Match != "" && !watchMatchTag(p.Match, tag) {
		return watchEvalResult{}, nil
	}
	ev := fmt.Sprintf("latest release %s in %s", tag, p.Repo)
	if rel.HTMLURL != "" {
		ev += " (" + rel.HTMLURL + ")"
	}
	return watchEvalResult{Triggered: true, Evidence: ev}, nil
}

func watchChecksGreen(checks []Check) (bool, int) {
	if len(checks) == 0 {
		return false, 0
	}
	passing := 0
	for _, c := range checks {
		status := strings.ToUpper(strings.TrimSpace(c.Status))
		if status != "" && status != "COMPLETED" {
			return false, 0
		}
		result := strings.ToUpper(strings.TrimSpace(c.Conclusion))
		if result == "" {
			result = strings.ToUpper(strings.TrimSpace(c.State))
		}
		switch result {
		case "SUCCESS", "SKIPPED", "NEUTRAL":
			passing++
		default:
			return false, 0
		}
	}
	return passing > 0, passing
}

func evalGHPRWatch(ctx context.Context, env WatchEnv, p *GHPRWatchParams) (watchEvalResult, error) {
	if p == nil {
		return watchEvalResult{}, errors.New("missing gh-pr params")
	}
	if err := validateWatchRepo(p.Repo); err != nil {
		return watchEvalResult{}, err
	}
	if p.PR <= 0 {
		return watchEvalResult{}, errors.New("invalid PR number")
	}
	if env.RunGH == nil {
		return watchEvalResult{}, errors.New("gh runner unavailable")
	}
	cctx, cancel := context.WithTimeout(ctx, defaultWatchTimeout)
	defer cancel()
	out, err := env.RunGH(cctx, "pr", "view", strconv.Itoa(p.PR), "-R", p.Repo,
		"--json", "number,state,url,reviewDecision,headRefOid,statusCheckRollup,mergedAt")
	if err != nil {
		return watchEvalResult{}, err
	}
	var pr PR
	if err := json.Unmarshal(out, &pr); err != nil {
		return watchEvalResult{}, fmt.Errorf("parse PR JSON: %w", err)
	}
	var reasons []string
	if p.Expect != "" {
		switch p.Expect {
		case "merged":
			if !strings.EqualFold(pr.State, "MERGED") {
				return watchEvalResult{}, nil
			}
			reasons = append(reasons, "state=MERGED")
		case "closed":
			if !strings.EqualFold(pr.State, "CLOSED") {
				return watchEvalResult{}, nil
			}
			reasons = append(reasons, "state=CLOSED")
		case "approved":
			if !strings.EqualFold(pr.ReviewDecision, "APPROVED") || strings.EqualFold(pr.State, "CLOSED") {
				return watchEvalResult{}, nil
			}
			reasons = append(reasons, "reviewDecision=APPROVED")
		case "checks-green":
			ok, n := watchChecksGreen(pr.Checks)
			if !ok {
				return watchEvalResult{}, nil
			}
			reasons = append(reasons, fmt.Sprintf("checks green (%d passing)", n))
		default:
			return watchEvalResult{}, fmt.Errorf("unsupported gh-pr expect %q", p.Expect)
		}
	}
	if p.HeadChangedFrom != "" {
		head := strings.TrimSpace(pr.HeadSHA)
		from := strings.TrimSpace(p.HeadChangedFrom)
		if head == "" || strings.EqualFold(head, from) || strings.HasPrefix(strings.ToLower(head), strings.ToLower(from)) {
			return watchEvalResult{}, nil
		}
		reasons = append(reasons, fmt.Sprintf("head=%s (was %s)", shortSHA(head), shortSHA(from)))
	}
	ev := fmt.Sprintf("PR %s#%d %s", p.Repo, p.PR, strings.Join(reasons, ", "))
	if pr.URL != "" {
		ev += " (" + pr.URL + ")"
	}
	return watchEvalResult{Triggered: true, Evidence: ev}, nil
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func evalGHRunWatch(ctx context.Context, env WatchEnv, p *GHRunWatchParams) (watchEvalResult, error) {
	if p == nil {
		return watchEvalResult{}, errors.New("missing gh-run params")
	}
	if err := validateWatchRepo(p.Repo); err != nil {
		return watchEvalResult{}, err
	}
	if p.RunID <= 0 {
		return watchEvalResult{}, errors.New("invalid run id")
	}
	if env.RunGH == nil {
		return watchEvalResult{}, errors.New("gh runner unavailable")
	}
	cctx, cancel := context.WithTimeout(ctx, defaultWatchTimeout)
	defer cancel()
	out, err := env.RunGH(cctx, "run", "view", strconv.FormatInt(p.RunID, 10), "-R", p.Repo, "--json", runFields)
	if err != nil {
		return watchEvalResult{}, err
	}
	var r Run
	if err := json.Unmarshal(out, &r); err != nil {
		return watchEvalResult{}, fmt.Errorf("parse run JSON: %w", err)
	}
	if !strings.EqualFold(r.Status, "completed") {
		return watchEvalResult{}, nil
	}
	expect := p.Expect
	if expect == "" {
		expect = "completed"
	}
	conclusion := strings.ToLower(strings.TrimSpace(r.Conclusion))
	switch expect {
	case "completed":
	case "success":
		if conclusion != "success" {
			return watchEvalResult{}, nil
		}
	case "failure":
		r.Conclusion = conclusion
		if !runFailed(r) && conclusion != "startup_failure" && conclusion != "action_required" {
			return watchEvalResult{}, nil
		}
	default:
		return watchEvalResult{}, fmt.Errorf("unsupported gh-run expect %q", expect)
	}
	name := r.Workflow
	if name == "" {
		name = p.Repo
	}
	ev := fmt.Sprintf("run %d (%s) completed with conclusion=%s", p.RunID, name, conclusion)
	if r.URL != "" {
		ev += " (" + r.URL + ")"
	}
	return watchEvalResult{Triggered: true, Evidence: ev}, nil
}

func evalHTTPWatch(ctx context.Context, env WatchEnv, p *HTTPWatchParams) (watchEvalResult, error) {
	if p == nil {
		return watchEvalResult{}, errors.New("missing http params")
	}
	u, err := validateHTTPWatchURL(p.URL)
	if err != nil {
		return watchEvalResult{}, err
	}
	statusSpec, err := validateHTTPStatusSpec(p.ExpectStatus)
	if err != nil {
		return watchEvalResult{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, defaultWatchTimeout)
	defer cancel()

	var transport http.RoundTripper
	if env.HTTPClient != nil {
		transport = env.HTTPClient.Transport
	}
	origScheme := strings.ToLower(u.Scheme)
	client := &http.Client{
		Transport: transport,
		Timeout:   defaultWatchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if expectsHTTPRedirectStatus(statusSpec) {
				return http.ErrUseLastResponse
			}
			if len(via) >= watchMaxRedirects {
				return fmt.Errorf("stopped after %d redirects", watchMaxRedirects)
			}
			nextScheme := strings.ToLower(req.URL.Scheme)
			if nextScheme != "http" && nextScheme != "https" {
				return fmt.Errorf("redirect to disallowed scheme %q refused", req.URL.Scheme)
			}
			if nextScheme != origScheme {
				return fmt.Errorf("cross-scheme redirect from %s to %s refused", origScheme, nextScheme)
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return watchEvalResult{}, err
	}
	req.Header.Set("User-Agent", toolName+"-watch/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return watchEvalResult{}, err
	}
	defer resp.Body.Close()

	needBody := p.ExpectJSON != "" || p.ExpectContains != ""
	var body []byte
	if needBody {
		body, err = io.ReadAll(io.LimitReader(resp.Body, watchHTTPMaxBody+1))
		if err != nil {
			return watchEvalResult{}, err
		}
		if len(body) > watchHTTPMaxBody {
			return watchEvalResult{}, fmt.Errorf("response body exceeded %d-byte limit", watchHTTPMaxBody)
		}
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	}

	if !matchHTTPStatus(statusSpec, resp.StatusCode) {
		return watchEvalResult{}, nil
	}

	var details []string
	if p.ExpectJSON != "" {
		pathExpr, wantVal, ok := strings.Cut(p.ExpectJSON, "=")
		if !ok || strings.TrimSpace(pathExpr) == "" {
			return watchEvalResult{}, fmt.Errorf("invalid expect_json %q", p.ExpectJSON)
		}
		gotVal, found, jerr := extractJSONDotPath(body, strings.TrimSpace(pathExpr))
		if jerr != nil {
			return watchEvalResult{}, fmt.Errorf("invalid JSON response: %w", jerr)
		}
		if !found || gotVal != wantVal {
			return watchEvalResult{}, nil
		}
		details = append(details, fmt.Sprintf("json %s=%s", strings.TrimSpace(pathExpr), wantVal))
	}
	if p.ExpectContains != "" {
		if !strings.Contains(string(body), p.ExpectContains) {
			return watchEvalResult{}, nil
		}
		details = append(details, fmt.Sprintf("body contains %q", shortTitle(p.ExpectContains, 40)))
	}
	ev := fmt.Sprintf("HTTP %d from %s", resp.StatusCode, sanitizeURLForDisplay(p.URL))
	if len(details) > 0 {
		ev += " (" + strings.Join(details, ", ") + ")"
	}
	return watchEvalResult{Triggered: true, Evidence: ev}, nil
}

func extractJSONDotPath(body []byte, dotPath string) (string, bool, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var cur any
	if err := dec.Decode(&cur); err != nil {
		return "", false, err
	}
	for _, seg := range strings.Split(dotPath, ".") {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return "", false, nil
			}
			cur = next
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return "", false, nil
			}
			cur = node[idx]
		default:
			return "", false, nil
		}
	}
	switch v := cur.(type) {
	case string:
		return v, true, nil
	case json.Number:
		return v.String(), true, nil
	case bool:
		return strconv.FormatBool(v), true, nil
	case nil:
		return "null", true, nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", false, err
		}
		return string(b), true, nil
	}
}

func evalCmdWatch(ctx context.Context, cfg Config, env WatchEnv, p *CmdWatchParams) (watchEvalResult, error) {
	if !cfg.Watches.AllowCmd {
		return watchEvalResult{}, errors.New("cmd watches are disabled ([watches] allow_cmd = false)")
	}
	if p == nil || strings.TrimSpace(p.Cmd) == "" {
		return watchEvalResult{}, errors.New("missing cmd params")
	}
	if strings.ContainsAny(p.Cmd, "\r\n\x00") {
		return watchEvalResult{}, fmt.Errorf("invalid cmd %q", p.Cmd)
	}
	timeout := defaultWatchTimeout
	if p.Timeout != "" {
		d, err := parseWatchDuration(p.Timeout)
		if err != nil || d <= 0 {
			return watchEvalResult{}, fmt.Errorf("invalid cmd timeout %q", p.Timeout)
		}
		if d > maxWatchCmdTimeout {
			return watchEvalResult{}, fmt.Errorf("cmd timeout %s exceeds maximum %s", d, maxWatchCmdTimeout)
		}
		timeout = d
	}
	runner := env.RunCmd
	if runner == nil {
		runner = runWatchCmd
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	code, stdout, err := runner(cctx, p.Cmd, p.Args)
	if err != nil {
		return watchEvalResult{}, err
	}
	if code != p.ExpectExit {
		return watchEvalResult{}, nil
	}
	if p.ExpectStdout != "" && !strings.Contains(stdout, p.ExpectStdout) {
		return watchEvalResult{}, nil
	}
	ev := fmt.Sprintf("cmd %s exited %d", filepath.Base(p.Cmd), code)
	if p.ExpectStdout != "" {
		ev += fmt.Sprintf(" (stdout matched %q)", shortTitle(p.ExpectStdout, 40))
	}
	return watchEvalResult{Triggered: true, Evidence: ev}, nil
}

// watchBackoff applies bounded exponential backoff after consecutive probe errors.
func watchBackoff(every time.Duration, consecutiveErrors int) time.Duration {
	if every < minWatchEvery {
		every = minWatchEvery
	}
	capDur := max(every*4, watchBackoffMinCap)
	if capDur > watchBackoffCeiling {
		capDur = watchBackoffCeiling
	}
	if capDur < every {
		capDur = every
	}
	d := every
	for i := 0; i < consecutiveErrors && d < capDur; i++ {
		if d > capDur/2 {
			return capDur
		}
		d *= 2
	}
	return min(d, capDur)
}

// watchCheckOutcome summarizes what a check pass did for one watch.
type watchCheckOutcome struct {
	Watch      Watch
	Action     string // "triggered", "expired", "retired", "pending", "error"
	WakeResult string // "" when inbox-only, "woken", or "skipped: <reason>"
	Err        error
}

// runWatchesPass retires watches on closed beads, expires watches past their
// TTL, evaluates due active watches with bounded concurrency, commits state
// changes atomically BEFORE notifying, and dispatches notes/inbox/journal/wake
// side effects.
//
// If filterBead is non-empty, only watches for that bead are considered.
// If force is true, active watches are evaluated regardless of NextCheck.
func runWatchesPass(cfg Config, env WatchEnv, threads []Thread, agents map[string]Agent, filterBead string, force bool) ([]watchCheckOutcome, error) {
	now := env.now()
	// Fast path when no store exists yet so quiet ticks do zero extra work.
	if _, err := os.Stat(watchesPath()); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	// Build a lookup of known beads from the ticker's active threads so we do
	// not issue redundant `bd show` calls for active threads.
	knownBeads := make(map[string]Bead, len(threads))
	for _, th := range threads {
		knownBeads[th.Bead.ID] = th.Bead
	}

	// Step 1: Read active watches under lock (without running probes).
	var candidates []Watch
	if err := withWatchStoreLock(func(s *watchStore) (bool, error) {
		for _, w := range s.Watches {
			if w.Status != WatchStatusActive {
				continue
			}
			if filterBead != "" && w.Bead != filterBead {
				continue
			}
			candidates = append(candidates, w)
		}
		return false, nil
	}); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	// Look up bead state for any candidate bead not already in knownBeads.
	beadLookupErr := map[string]bool{}
	for _, w := range candidates {
		if _, ok := knownBeads[w.Bead]; ok || beadLookupErr[w.Bead] {
			continue
		}
		if env.Show == nil {
			continue
		}
		b, err := env.Show(w.Bead)
		if err != nil {
			env.logf("watch show %s: %v", w.Bead, err)
			beadLookupErr[w.Bead] = true
			continue
		}
		knownBeads[w.Bead] = b
	}

	// Step 2: Atomically retire closed-bead watches and expire past-TTL watches,
	// collecting the remaining due watches to probe outside the lock.
	var (
		retired []Watch
		expired []Watch
		due     []Watch
	)
	if err := withWatchStoreLock(func(s *watchStore) (bool, error) {
		dirty := false
		for i := range s.Watches {
			w := &s.Watches[i]
			if w.Status != WatchStatusActive {
				continue
			}
			if filterBead != "" && w.Bead != filterBead {
				continue
			}
			if b, ok := knownBeads[w.Bead]; ok && b.Status == StatusClosed {
				w.Status = WatchStatusRemoved
				w.Evidence = "retired: bead closed"
				w.LastError = ""
				retired = append(retired, *w)
				dirty = true
				continue
			}
			if w.ExpiresAt != nil && !now.Before(*w.ExpiresAt) {
				w.Status = WatchStatusExpired
				w.Evidence = "expired at " + w.ExpiresAt.UTC().Format(time.RFC3339)
				w.LastError = ""
				expired = append(expired, *w)
				dirty = true
				continue
			}
			if force || !now.Before(w.NextCheck) {
				due = append(due, *w)
			}
		}
		return dirty, nil
	}); err != nil {
		return nil, err
	}

	var outcomes []watchCheckOutcome
	for _, w := range retired {
		_ = appendJournal(journalRecord{
			At: now, Kind: journalWatch, Bead: w.Bead, Class: -1, Action: "retired",
			Text: fmt.Sprintf("[%s] %s — %s", w.ID, w.Label, w.Evidence),
		})
		outcomes = append(outcomes, watchCheckOutcome{Watch: w, Action: "retired"})
	}
	for _, w := range expired {
		notifyWatchExpired(env, w, now)
		outcomes = append(outcomes, watchCheckOutcome{Watch: w, Action: "expired"})
	}

	if len(due) == 0 {
		return outcomes, nil
	}

	// Step 3: Probe due watches concurrently with bounded parallelism, outside lock.
	type probeResult struct {
		watch Watch
		res   watchEvalResult
		err   error
	}
	results := make([]probeResult, len(due))
	eachLimit(len(due), cfg.Watches.concurrency(), func(i int) {
		res, err := evaluateWatch(context.Background(), cfg, env, due[i])
		results[i] = probeResult{watch: due[i], res: res, err: err}
	})

	// Step 4: Re-acquire lock and commit state transitions atomically BEFORE
	// firing any notifications (guarantees one-shot edge-triggered semantics
	// even if CLI `watch check` and ticker overlap).
	var newlyTriggered []Watch
	var pendingOrErr []watchCheckOutcome
	if err := withWatchStoreLock(func(s *watchStore) (bool, error) {
		index := make(map[string]int, len(s.Watches))
		for i := range s.Watches {
			index[s.Watches[i].ID] = i
		}
		dirty := false
		for _, r := range results {
			idx, ok := index[r.watch.ID]
			if !ok {
				continue
			}
			w := &s.Watches[idx]
			if w.Status != WatchStatusActive {
				// Already removed, expired, or triggered concurrently.
				continue
			}
			checkedAt := now
			w.LastChecked = &checkedAt
			dirty = true
			switch {
			case r.err != nil:
				w.ConsecutiveErrors++
				w.LastError = shortTitle(r.err.Error(), 300)
				w.NextCheck = now.Add(watchBackoff(w.everyDuration(), w.ConsecutiveErrors))
				pendingOrErr = append(pendingOrErr, watchCheckOutcome{Watch: *w, Action: "error", Err: r.err})
			case r.res.Triggered:
				w.Status = WatchStatusTriggered
				triggeredAt := now
				w.TriggeredAt = &triggeredAt
				w.LastError = ""
				w.ConsecutiveErrors = 0
				w.Evidence = r.res.Evidence
				newlyTriggered = append(newlyTriggered, *w)
			default:
				w.LastError = ""
				w.ConsecutiveErrors = 0
				w.NextCheck = now.Add(w.everyDuration())
				pendingOrErr = append(pendingOrErr, watchCheckOutcome{Watch: *w, Action: "pending"})
			}
		}
		return dirty, nil
	}); err != nil {
		return outcomes, err
	}

	// Step 5: Emit side effects only for watches durably committed as triggered.
	for _, w := range newlyTriggered {
		b, hasBead := knownBeads[w.Bead]
		if !hasBead && env.Show != nil {
			if fetched, err := env.Show(w.Bead); err == nil {
				b, hasBead = fetched, true
			}
		}
		wakeResult := notifyWatchTriggered(env, w, b, hasBead, agents, now)
		outcomes = append(outcomes, watchCheckOutcome{Watch: w, Action: "triggered", WakeResult: wakeResult})
	}
	outcomes = append(outcomes, pendingOrErr...)
	return outcomes, nil
}

func notifyWatchExpired(env WatchEnv, w Watch, now time.Time) {
	note := fmt.Sprintf("WATCH EXPIRED [%s]: %s — %s", w.ID, w.Label, w.Evidence)
	if env.Note != nil {
		if err := env.Note(w.Bead, note); err != nil {
			env.logf("watch note %s: %v", w.Bead, err)
		}
	}
	if err := appendJournal(journalRecord{
		At: now, Kind: journalWatch, Bead: w.Bead, Class: -1, Action: "expired",
		Text: fmt.Sprintf("[%s] %s — %s", w.ID, w.Label, w.Evidence),
	}); err != nil {
		env.logf("watch journal %s: %v", w.Bead, err)
	}
	if err := writeEvent(Event{
		Bead:    w.Bead,
		Kind:    EventWatchExpired,
		Summary: fmt.Sprintf("watch [%s] expired (%s): %s", w.ID, w.Label, w.Evidence),
		At:      now,
		Clears:  &InboxCondition{Kind: "bead_closed", Bead: w.Bead, Key: w.ID},
	}); err != nil {
		env.logf("watch inbox %s: %v", w.Bead, err)
	}
}

func notifyWatchTriggered(env WatchEnv, w Watch, b Bead, hasBead bool, agents map[string]Agent, now time.Time) string {
	wakeResult := ""
	wakeSkipReason := ""
	if w.OnTrigger == WatchOnTriggerWake {
		woken, reason := tryWakeWatchWorker(env, w, b, hasBead, agents)
		if woken {
			wakeResult = "woken"
		} else {
			wakeResult = "skipped: " + reason
			wakeSkipReason = reason
		}
	}

	note := fmt.Sprintf("WATCH TRIGGERED [%s]: %s — %s", w.ID, w.Label, w.Evidence)
	if wakeSkipReason != "" {
		note += fmt.Sprintf(" (worker not prompted: %s)", wakeSkipReason)
	}
	if env.Note != nil {
		if err := env.Note(w.Bead, note); err != nil {
			env.logf("watch note %s: %v", w.Bead, err)
		}
	}

	journalText := fmt.Sprintf("[%s] %s — %s", w.ID, w.Label, w.Evidence)
	if wakeResult != "" {
		journalText += " (wake: " + wakeResult + ")"
	}
	if err := appendJournal(journalRecord{
		At: now, Kind: journalWatch, Bead: w.Bead, Class: -1, Action: "triggered", Pass: true, Text: journalText,
	}); err != nil {
		env.logf("watch journal %s: %v", w.Bead, err)
	}

	summary := fmt.Sprintf("watch [%s] triggered (%s): %s", w.ID, w.Label, w.Evidence)
	if wakeResult == "woken" {
		summary += "; worker prompted"
	} else if wakeSkipReason != "" {
		summary += fmt.Sprintf("; worker not prompted (%s)", wakeSkipReason)
	}
	if err := writeEvent(Event{
		Bead:    w.Bead,
		Kind:    EventWatchTriggered,
		Summary: summary,
		At:      now,
		Clears:  &InboxCondition{Kind: "bead_closed", Bead: w.Bead, Key: w.ID},
	}); err != nil {
		env.logf("watch inbox %s: %v", w.Bead, err)
	}
	return wakeResult
}

// tryWakeWatchWorker wakes the bead's worker ONLY when strictly safe:
//   - bead is known, not needs_me, not closed, not open
//   - bead is not owned by the headless coordinator
//   - an alive worker exists and is at rest/idle (never working, never blocked,
//     never parked)
//
// Delivered via the existing worker tell path with a clear automated
// non-approving header.
func tryWakeWatchWorker(env WatchEnv, w Watch, b Bead, hasBead bool, agents map[string]Agent) (bool, string) {
	if !hasBead {
		return false, "bead state unavailable"
	}
	switch b.Status {
	case StatusNeedsMe:
		return false, "bead is needs_me"
	case StatusClosed:
		return false, "bead is closed"
	case StatusOpen:
		return false, "bead is open"
	}
	if headlessOwned(w.Bead) {
		return false, "headless-owned bead"
	}
	var a *Agent
	if agents != nil && (beadHost(b) == "" || beadHost(b) == localHost) {
		if found, ok := agents[agentName(w.Bead)]; ok {
			cp := found
			a = &cp
		}
	}
	if a == nil && env.AgentFor != nil {
		found, err := env.AgentFor(b)
		if err != nil {
			return false, "worker lookup failed: " + err.Error()
		}
		a = found
	}
	if a == nil {
		return false, "no worker running"
	}
	if a.Backend == rpcBackend && rpcParked(w.Bead) {
		return false, "worker is parked"
	}
	if !agentReady(a) {
		return false, "worker is " + a.Status
	}
	if env.Tell == nil {
		return false, "worker tell unavailable"
	}
	msg := fmt.Sprintf("%s Watch [%s] (%s) triggered: %s. Inspect the bead (`bd show %s`) and continue your work.",
		watchWakeHeaderPrefix, w.ID, w.Label, w.Evidence, w.Bead)
	if err := env.Tell(b, *a, msg); err != nil {
		return false, "tell failed: " + err.Error()
	}
	return true, ""
}

// stringSliceFlag lets `--arg` be repeated cleanly on `quartermaster watch add`.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

const watchUsage = `usage:
  quartermaster watch add <bead> --kind <gh-release|gh-pr|gh-run|http|cmd> [flags...]
                          [--every 5m] [--expires 7d] [--on-trigger inbox|wake] [--label "..."]
  quartermaster watch list [<bead>] [--all] [--json]
  quartermaster watch remove <bead> <watch-id|--all>
  quartermaster watch check [<bead>] [--force]

Kind flags:
  gh-release: --repo owner/repo [--after <tag>] [--match <prefix-or-glob>]
  gh-pr:      --repo owner/repo --pr <number> [--expect merged|closed|approved|checks-green] [--head-changed-from <sha>]
  gh-run:     --repo owner/repo --run <id> [--expect completed|success|failure]
  http:       --url <http|https> [--expect-status <code|2xx>] [--expect-json <dot.path=value>] [--expect-contains <substr>]
  cmd:        --cmd <prog> [--arg <val>...] [--expect-exit 0] [--expect-stdout <substr>] [--timeout 15s]
              (requires [watches] allow_cmd = true in config.toml)
`

func watchCmd(cfg Config, env WatchEnv, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(out, watchUsage)
		return nil
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		return watchAddCmd(cfg, env, rest, out)
	case "list":
		return watchListCmd(rest, out)
	case "remove":
		return watchRemoveCmd(rest, out)
	case "check":
		return watchCheckCmd(cfg, env, rest, out)
	default:
		return fmt.Errorf("unknown watch subcommand %q\n\n%s", sub, strings.TrimSpace(watchUsage))
	}
}

func watchAddCmd(cfg Config, env WatchEnv, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("watch add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	kind := fs.String("kind", "", "watch kind: gh-release, gh-pr, gh-run, http or cmd")
	label := fs.String("label", "", "human label for the watch")
	every := fs.String("every", "5m", "check interval (minimum 30s)")
	expires := fs.String("expires", "", "optional TTL (e.g. 24h, 7d)")
	onTrigger := fs.String("on-trigger", string(WatchOnTriggerInbox), "inbox (default) or wake")

	repo := fs.String("repo", "", "GitHub owner/repo")
	after := fs.String("after", "", "gh-release: trigger when latest tag differs from this tag")
	match := fs.String("match", "", "gh-release: prefix or glob pattern")
	prNum := fs.Int("pr", 0, "gh-pr: pull request number")
	runID := fs.Int64("run", 0, "gh-run: Actions run ID")
	expect := fs.String("expect", "", "gh-pr or gh-run expected state")
	headChangedFrom := fs.String("head-changed-from", "", "gh-pr: trigger when head SHA differs")

	rawURL := fs.String("url", "", "http: http or https URL")
	expectStatus := fs.String("expect-status", "200", "http: expected status code or 2xx class")
	expectJSON := fs.String("expect-json", "", "http: dot.path=value JSON match")
	expectContains := fs.String("expect-contains", "", "http: substring expected in body")

	cmdProg := fs.String("cmd", "", "cmd: executable to run directly without a shell")
	var cmdArgs stringSliceFlag
	fs.Var(&cmdArgs, "arg", "cmd: argument to pass (repeatable)")
	expectExit := fs.Int("expect-exit", 0, "cmd: expected exit code")
	expectStdout := fs.String("expect-stdout", "", "cmd: substring expected in stdout")
	cmdTimeout := fs.String("timeout", "15s", "cmd: probe timeout (max 60s)")

	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: quartermaster watch add <bead> --kind <gh-release|gh-pr|gh-run|http|cmd> [flags...]")
	}
	beadID := strings.TrimSpace(pos[0])

	setFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	if err := rejectMismatchedWatchFlags(WatchKind(*kind), setFlags); err != nil {
		return err
	}

	now := env.now().UTC()
	w := Watch{
		Bead:      beadID,
		Kind:      WatchKind(strings.TrimSpace(*kind)),
		Label:     *label,
		Every:     *every,
		CreatedAt: now,
		OnTrigger: WatchTriggerAction(strings.TrimSpace(*onTrigger)),
		Status:    WatchStatusActive,
		NextCheck: now,
	}
	if *expires != "" {
		ttl, err := parseWatchDuration(*expires)
		if err != nil {
			return fmt.Errorf("--expires: %w", err)
		}
		exp := now.Add(ttl)
		w.ExpiresAt = &exp
	}

	switch w.Kind {
	case WatchKindGHRelease:
		w.Params.GHRelease = &GHReleaseWatchParams{Repo: *repo, After: *after, Match: *match}
	case WatchKindGHPR:
		w.Params.GHPR = &GHPRWatchParams{Repo: *repo, PR: *prNum, Expect: *expect, HeadChangedFrom: *headChangedFrom}
	case WatchKindGHRun:
		w.Params.GHRun = &GHRunWatchParams{Repo: *repo, RunID: *runID, Expect: *expect}
	case WatchKindHTTP:
		w.Params.HTTP = &HTTPWatchParams{URL: *rawURL, ExpectStatus: *expectStatus, ExpectJSON: *expectJSON, ExpectContains: *expectContains}
	case WatchKindCmd:
		w.Params.Cmd = &CmdWatchParams{Cmd: *cmdProg, Args: slices.Clone(cmdArgs), ExpectExit: *expectExit, ExpectStdout: *expectStdout, Timeout: *cmdTimeout}
	}

	if err := validateWatch(cfg, &w); err != nil {
		return err
	}

	// Verify the bead exists and is not closed before persisting.
	if env.Show == nil {
		return errors.New("beads reader unavailable")
	}
	b, err := env.Show(beadID)
	if err != nil {
		return fmt.Errorf("bead %s: %w", beadID, err)
	}
	if b.Status == StatusClosed {
		return fmt.Errorf("bead %s is already closed", beadID)
	}
	w.Bead = b.ID

	if err := withWatchStoreLock(func(s *watchStore) (bool, error) {
		id, err := newWatchID(s.Watches)
		if err != nil {
			return false, err
		}
		w.ID = id
		s.Watches = append(s.Watches, w)
		return true, nil
	}); err != nil {
		return err
	}

	expText := ""
	if w.ExpiresAt != nil {
		expText = fmt.Sprintf(", expires %s", w.ExpiresAt.Format(time.RFC3339))
	}
	fmt.Fprintf(out, "added watch %s for %s: %s (kind=%s, every=%s, on_trigger=%s%s)\n",
		w.ID, w.Bead, w.Label, w.Kind, w.Every, w.OnTrigger, expText)
	return nil
}

func rejectMismatchedWatchFlags(kind WatchKind, set map[string]bool) error {
	allowed := map[WatchKind]map[string]bool{
		WatchKindGHRelease: {"repo": true, "after": true, "match": true},
		WatchKindGHPR:      {"repo": true, "pr": true, "expect": true, "head-changed-from": true},
		WatchKindGHRun:     {"repo": true, "run": true, "expect": true},
		WatchKindHTTP:      {"url": true, "expect-status": true, "expect-json": true, "expect-contains": true},
		WatchKindCmd:       {"cmd": true, "arg": true, "expect-exit": true, "expect-stdout": true, "timeout": true},
	}
	common := map[string]bool{"kind": true, "label": true, "every": true, "expires": true, "on-trigger": true}
	kindAllowed, ok := allowed[kind]
	if !ok {
		return nil // validateWatch will report the invalid/missing kind
	}
	for name := range set {
		if !common[name] && !kindAllowed[name] {
			return fmt.Errorf("flag --%s does not apply to watch kind %q", name, kind)
		}
	}
	return nil
}

func watchListCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("watch list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	all := fs.Bool("all", false, "include triggered, expired and removed watches")
	asJSON := fs.Bool("json", false, "output JSON")
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return errors.New("usage: quartermaster watch list [<bead>] [--all] [--json]")
	}
	filterBead := ""
	if len(pos) == 1 {
		filterBead = pos[0]
	}

	ws, err := loadWatches()
	if err != nil {
		return err
	}
	filtered := make([]Watch, 0, len(ws))
	for _, w := range ws {
		if filterBead != "" && w.Bead != filterBead {
			continue
		}
		if !*all && w.Status != WatchStatusActive {
			continue
		}
		filtered = append(filtered, w)
	}
	slices.SortFunc(filtered, compareWatches)

	if *asJSON {
		b, err := json.MarshalIndent(filtered, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(b))
		return nil
	}
	if len(filtered) == 0 {
		fmt.Fprintln(out, "no watches")
		return nil
	}
	for _, w := range filtered {
		line := fmt.Sprintf("%s  %-14s  %-9s  %-10s  every=%-4s  on=%-5s  %s",
			w.ID, w.Bead, w.Status, w.Kind, w.Every, w.OnTrigger, w.Label)
		switch {
		case w.Status == WatchStatusTriggered && w.Evidence != "":
			line += "  (" + w.Evidence + ")"
		case w.Status == WatchStatusActive && w.LastError != "":
			line += fmt.Sprintf("  [err×%d: %s]", w.ConsecutiveErrors, shortTitle(w.LastError, 80))
		case w.Evidence != "":
			line += "  (" + w.Evidence + ")"
		}
		fmt.Fprintln(out, line)
	}
	return nil
}

func watchRemoveCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("watch remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	all := fs.Bool("all", false, "remove all active watches for the bead")
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if *all {
		if len(pos) != 1 {
			return errors.New("usage: quartermaster watch remove <bead> --all")
		}
	} else {
		if len(pos) != 2 {
			return errors.New("usage: quartermaster watch remove <bead> <watch-id|--all>")
		}
	}
	beadID := pos[0]
	watchID := ""
	if len(pos) == 2 {
		watchID = pos[1]
	}

	removed := 0
	var removedLabel string
	err = withWatchStoreLock(func(s *watchStore) (bool, error) {
		dirty := false
		found := false
		for i := range s.Watches {
			w := &s.Watches[i]
			if w.Bead != beadID {
				continue
			}
			if *all {
				if w.Status == WatchStatusActive {
					w.Status = WatchStatusRemoved
					w.Evidence = "removed"
					removed++
					dirty = true
				}
				continue
			}
			if w.ID == watchID {
				found = true
				removedLabel = w.Label
				if w.Status != WatchStatusRemoved {
					w.Status = WatchStatusRemoved
					w.Evidence = "removed"
					removed++
					dirty = true
				}
				break
			}
		}
		if !*all && !found {
			return false, fmt.Errorf("watch %q not found for bead %s", watchID, beadID)
		}
		return dirty, nil
	})
	if err != nil {
		return err
	}
	if *all {
		fmt.Fprintf(out, "removed %d active watch(es) for %s\n", removed, beadID)
		return nil
	}
	if removed == 0 {
		fmt.Fprintf(out, "watch %s for %s was already removed\n", watchID, beadID)
		return nil
	}
	fmt.Fprintf(out, "removed watch %s (%s) for %s\n", watchID, removedLabel, beadID)
	return nil
}

func watchCheckCmd(cfg Config, env WatchEnv, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("watch check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	force := fs.Bool("force", false, "evaluate active watches even if next_check has not arrived")
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return errors.New("usage: quartermaster watch check [<bead>] [--force]")
	}
	filterBead := ""
	if len(pos) == 1 {
		filterBead = pos[0]
	}

	outcomes, err := runWatchesPass(cfg, env, nil, nil, filterBead, *force)
	if err != nil {
		return err
	}
	if len(outcomes) == 0 {
		fmt.Fprintln(out, "no due watches")
		return nil
	}
	for _, o := range outcomes {
		switch o.Action {
		case "triggered":
			extra := ""
			if o.WakeResult != "" {
				extra = " [wake: " + o.WakeResult + "]"
			}
			fmt.Fprintf(out, "%s  %s  TRIGGERED  %s — %s%s\n", o.Watch.ID, o.Watch.Bead, o.Watch.Label, o.Watch.Evidence, extra)
		case "expired":
			fmt.Fprintf(out, "%s  %s  EXPIRED    %s — %s\n", o.Watch.ID, o.Watch.Bead, o.Watch.Label, o.Watch.Evidence)
		case "retired":
			fmt.Fprintf(out, "%s  %s  RETIRED    %s — %s\n", o.Watch.ID, o.Watch.Bead, o.Watch.Label, o.Watch.Evidence)
		case "error":
			fmt.Fprintf(out, "%s  %s  ERROR      %s — %v (retry %s)\n", o.Watch.ID, o.Watch.Bead, o.Watch.Label, o.Err, o.Watch.NextCheck.Format(time.RFC3339))
		default:
			fmt.Fprintf(out, "%s  %s  PENDING    %s (next %s)\n", o.Watch.ID, o.Watch.Bead, o.Watch.Label, o.Watch.NextCheck.Format(time.RFC3339))
		}
	}
	return nil
}
