package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/term"
)

// Slack delivery is off unless one of these is set, in the environment or in
// the digest's EnvironmentFile (~/.config/quartermaster/digest.env).
const (
	envDeliver      = "QUARTERMASTER_DIGEST_DELIVER"
	envSlackWebhook = "QUARTERMASTER_DIGEST_SLACK_WEBHOOK"
	envSlackToken   = "QUARTERMASTER_DIGEST_SLACK_TOKEN"
	envSlackChannel = "QUARTERMASTER_DIGEST_SLACK_CHANNEL"
)

var deliveryTargets = []string{"desktop", "slack", "stdout", "none"}

// parseDeliver reads --deliver: a comma-separated list of targets.
func parseDeliver(s string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, t := range strings.Split(s, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if !slices.Contains(deliveryTargets, t) {
			return nil, fmt.Errorf("--deliver %q: want any of %s", t, strings.Join(deliveryTargets, ","))
		}
		out[t] = true
	}
	return out, nil
}

// defaultDeliver is --deliver's default: QUARTERMASTER_DIGEST_DELIVER, else
// a desktop notification.
func defaultDeliver() string {
	if v := os.Getenv(envDeliver); v != "" {
		return v
	}
	return "desktop"
}

// runDigest builds the digest, writes it to digest.md, remembers this run
// and delivers it. The file is written every run; notifications only when
// something changed or something needs the user, unless always is set.
func runDigest(cfg Config, deliver string, always bool, stdout io.Writer) error {
	targets, err := parseDeliver(deliver)
	if err != nil {
		return err
	}
	start := time.Now()
	prev := loadDigestState()
	in, warnings, err := gatherDigest(liveDigestEnv(cfg), prev)
	if err != nil {
		return err
	}
	d, next := buildDigest(in, prev, cfg.LinearPrefixes, time.Now())
	d.Warnings = warnings
	text := renderDigest(d)
	if err := writeAtomic(digestPath(), []byte(text)); err != nil {
		return err
	}
	if err := saveDigestState(next); err != nil {
		return err
	}
	if targets["stdout"] {
		fmt.Fprint(stdout, text)
	}
	notify := always || d.worthNotifying()
	var sent []string
	var errs []string
	if notify && targets["desktop"] {
		title, body := d.summary(3)
		if err := notifyDesktop(title, body+"\n\nFull digest: "+digestPath()); err != nil {
			errs = append(errs, "desktop: "+err.Error())
		} else {
			sent = append(sent, "desktop")
		}
	}
	if notify && targets["slack"] {
		title, body := d.summary(10)
		msg := title + "\n" + body + "\n\nFull digest: " + digestPath()
		if err := sendSlack(msg, os.Getenv, httpPost); err != nil {
			errs = append(errs, "slack: "+err.Error())
		} else {
			sent = append(sent, "slack")
		}
	}
	status := "not notified (nothing changed, nothing needs you)"
	if len(sent) > 0 {
		status = "notified " + strings.Join(sent, ", ")
	} else if notify {
		status = "no notification target"
	}
	if !targets["stdout"] {
		fmt.Fprintf(stdout, "%s · %d changes · %d PRs in %d GraphQL queries · %s · wrote %s in %s\n",
			d.headline(), len(d.Changes), in.asked, in.queries, status, digestPath(), time.Since(start).Round(10*time.Millisecond))
	}
	if len(errs) > 0 {
		return fmt.Errorf("delivery failed: %s", strings.Join(errs, "; "))
	}
	return nil
}

func notifyDesktop(title, body string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "notify-send", "--app-name", toolName, title, body).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sendSlack posts msg through an incoming webhook, or as a bot to a channel
// or user id (a DM). Neither configured is an error: slack was asked for.
func sendSlack(msg string, getenv func(string) string, post func(url string, header http.Header, body []byte) ([]byte, error)) error {
	if hook := getenv(envSlackWebhook); hook != "" {
		body, _ := json.Marshal(map[string]string{"text": msg})
		_, err := post(hook, http.Header{"Content-Type": {"application/json"}}, body)
		return err
	}
	token, channel := getenv(envSlackToken), getenv(envSlackChannel)
	if token == "" || channel == "" {
		return fmt.Errorf("set %s, or %s and %s", envSlackWebhook, envSlackToken, envSlackChannel)
	}
	body, _ := json.Marshal(map[string]string{"channel": channel, "text": msg})
	out, err := post("https://slack.com/api/chat.postMessage", http.Header{
		"Content-Type":  {"application/json; charset=utf-8"},
		"Authorization": {"Bearer " + token},
	}, body)
	if err != nil {
		return err
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("chat.postMessage: %s", resp.Error)
	}
	return nil
}

func httpPost(url string, header http.Header, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return out, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// showDigest pages the last digest, writing one first if there is none. It
// is the plugin's digest pane.
func showDigest(cfg Config) error {
	if _, err := os.Stat(digestPath()); err != nil {
		if err := runDigest(cfg, "none", false, io.Discard); err != nil {
			return err
		}
	}
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		b, err := os.ReadFile(digestPath())
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(b)
		return err
	}
	pager := os.Getenv("PAGER")
	if pager == "" {
		pager = "less -R"
	}
	cmd := exec.Command("sh", "-c", pager+` "$1"`, "pager", digestPath())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

//go:embed systemd/quartermaster-digest.service systemd/quartermaster-digest.timer
var digestUnits embed.FS

const (
	digestService = "quartermaster-digest.service"
	digestTimer   = "quartermaster-digest.timer"
)

func userUnitDir() string { return filepath.Join(configBase(), "systemd", "user") }

// digestDropIn is the unit's machine-specific part: where bd's database is
// and a PATH that finds bd, gh and notify-send, which a user service started
// before the desktop session imports its environment would not have.
func digestDropIn(beadsDir string, path []string) string {
	var s strings.Builder
	s.WriteString("# Written by `quartermaster digest install`; run it again to refresh.\n[Service]\n")
	if beadsDir != "" {
		fmt.Fprintf(&s, "Environment=%s\n", systemdQuote("BEADS_DIR="+beadsDir))
	}
	fmt.Fprintf(&s, "Environment=%s\n", systemdQuote("PATH="+strings.Join(path, ":")))
	return s.String()
}

func systemdQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// digestSearchPath lists the directories of the tools the digest runs, then
// mise's shims, if there are any, then the system directories. A version
// manager's install directory goes away on upgrade; its shims stay.
func digestSearchPath(lookPath func(string) (string, error), shims string) []string {
	var dirs []string
	for _, tool := range []string{"bd", "gh", "herdr", "notify-send", "git"} {
		if p, err := lookPath(tool); err == nil {
			dirs = append(dirs, filepath.Dir(p))
		}
	}
	if fi, err := os.Stat(shims); err == nil && fi.IsDir() {
		dirs = append(dirs, shims)
	}
	return dedupe(append(dirs, "/usr/local/bin", "/usr/bin", "/bin"))
}

func miseShims() string {
	if d := os.Getenv("MISE_DATA_DIR"); d != "" {
		return filepath.Join(d, "shims")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "mise", "shims")
}

// digestInstall writes the digest's systemd user units and drop-in into dir
// and starts the hourly timer through systemctl.
func digestInstall(cfg Config, dir string, lookPath func(string) (string, error), systemctl func(args ...string) error) ([]string, error) {
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, ".local", "bin", toolName)
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("%s is missing; run `quartermaster configure` first", bin)
	}
	var lines []string
	for _, name := range []string{digestService, digestTimer} {
		b, err := digestUnits.ReadFile("systemd/" + name)
		if err != nil {
			return lines, err
		}
		if err := writeAtomic(filepath.Join(dir, name), b); err != nil {
			return lines, err
		}
		lines = append(lines, "wrote "+filepath.Join(dir, name))
	}
	dropIn := filepath.Join(dir, digestService+".d", toolName+".conf")
	if err := writeAtomic(dropIn, []byte(digestDropIn(cfg.BeadsDir, digestSearchPath(lookPath, miseShims())))); err != nil {
		return lines, err
	}
	lines = append(lines, "wrote "+dropIn)
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", digestTimer}} {
		if err := systemctl(args...); err != nil {
			return lines, err
		}
		lines = append(lines, "systemctl --user "+strings.Join(args, " "))
	}
	return lines, nil
}

// digestUninstall stops the timer and removes what digestInstall wrote.
func digestUninstall(dir string, systemctl func(args ...string) error) ([]string, error) {
	var lines []string
	if err := systemctl("disable", "--now", digestTimer); err == nil {
		lines = append(lines, "systemctl --user disable --now "+digestTimer)
	}
	for _, p := range []string{
		filepath.Join(dir, digestService+".d", toolName+".conf"),
		filepath.Join(dir, digestService),
		filepath.Join(dir, digestTimer),
	} {
		if err := os.Remove(p); err == nil {
			lines = append(lines, "removed "+p)
		} else if !os.IsNotExist(err) {
			return lines, err
		}
	}
	_ = os.Remove(filepath.Join(dir, digestService+".d"))
	return lines, systemctl("daemon-reload")
}

func systemctlUser(args ...string) error {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
