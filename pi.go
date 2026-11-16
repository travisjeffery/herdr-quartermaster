package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// piExtension is quartermaster's pi extension (pi/quartermaster.ts): the
// bash hooks and subagent guard, the prompt-source entries the ticker reads,
// and dialogs reported to herdr as blocked.
//
//go:embed pi/quartermaster.ts
var piExtension []byte

// piExtensionFile writes the embedded extension to the state directory, when
// it differs, and returns its path. pi loads it with -e, so it needs no
// install and always matches the binary.
func piExtensionFile() (string, error) {
	path := filepath.Join(stateDir(), "pi", "quartermaster.ts")
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, piExtension) {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, piExtension, 0o644); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// piLaunch checks a pi start's model and adds what every quartermaster pi
// gets: the extension, on this machine, and the bead's session for a worker
// (id "" for the coordinator).
func (c Config) piLaunch(host string, args []string, role, id string) ([]string, error) {
	if err := checkPiModel(c.piOn(host), args, role); err != nil {
		return nil, err
	}
	if host == localHost {
		ext, err := piExtensionFile()
		if err != nil {
			return nil, fmt.Errorf("pi extension: %w", err)
		}
		args = append(args, "-e", ext)
	}
	if id != "" {
		args = append(args, piSessionArgs(id)...)
	}
	return args, nil
}

// Default pi models: workers on Opus 5.5, the coordinator on Fable 5.2.
// [agents.pi] args and coordinator_args replace them. pi's built-in catalog
// has claude-opus-5-5 but not claude-fable-5-2, which needs a models entry in
// ~/.pi/agent/models.json (docs/pi.md).
const (
	piWorkerModel      = "anthropic/claude-opus-5-5"
	piCoordinatorModel = "anthropic/claude-fable-5-2"
)

// piSessionArgs pin a pi worker to the session named for its bead: pi opens
// the worktree's session with that id, or creates it. Dispatch and resume
// pass the same id, so a resume reopens the bead's conversation by id, not
// "the most recent session in this folder", which a manual pi run in the
// worktree would change.
func piSessionArgs(id string) []string { return []string{"--session-id", id} }

// piModel is the model a pi command line selects with --model, without a
// :thinking suffix; "" when it names none and pi uses its default.
func piModel(args []string) string {
	for i, a := range args {
		v, ok := strings.CutPrefix(a, "--model=")
		if !ok && a == "--model" && i+1 < len(args) {
			v, ok = args[i+1], true
		}
		if ok {
			m, _, _ := strings.Cut(v, ":")
			return m
		}
	}
	return ""
}

// piRunner runs a pi subcommand on a host and returns its stdout and exit
// code; err is set only when pi couldn't run at all.
type piRunner func(args ...string) (out []byte, code int, err error)

func (c Config) piOn(host string) piRunner {
	return func(args ...string) ([]byte, int, error) {
		out, err := c.hostExec(host, append([]string{"pi"}, args...)...).Output()
		if ee, ok := err.(*exec.ExitError); ok {
			return out, ee.ExitCode(), nil
		}
		return out, 0, err
	}
}

// checkPiModel fails, with what to do about it, when pi can't use the model
// args select: pi has no credentials for its provider, or no such model.
// `pi --list-models` lists only models whose provider has credentials, so a
// listed model is ready; otherwise `pi auth check` tells a missing sign-in
// from a model pi doesn't know. Neither prints a credential.
func checkPiModel(run piRunner, args []string, role string) error {
	model := piModel(args)
	if model == "" {
		return nil
	}
	provider, id, ok := strings.Cut(model, "/")
	if !ok {
		provider, id = "", model
	}
	out, _, err := run(append(piExtensionArgs(args), "--list-models", id)...)
	if err != nil {
		return fmt.Errorf("pi: %w (is pi installed?)", err)
	}
	if piListed(out, provider, id) {
		return nil
	}
	fix := fmt.Sprintf("set [agents.pi] %s to a model `pi --list-models` shows", map[bool]string{true: "coordinator_args", false: "args"}[role == "coordinator"])
	if provider != "" {
		if _, code, err := run("auth", "check", "--provider", provider); err == nil && code != 0 {
			return fmt.Errorf("pi has no credentials for %s, so it can't run the %s model %s: sign in with `pi auth login %s`, or %s", provider, role, model, provider, fix)
		}
	}
	return fmt.Errorf("pi has no model %s for the %s: add it to ~/.pi/agent/models.json (docs/pi.md), or %s", model, role, fix)
}

// piExtensionArgs are the extensions a pi command line loads, which can
// register providers and models of their own.
func piExtensionArgs(args []string) []string {
	var out []string
	for i, a := range args {
		switch {
		case (a == "-e" || a == "--extension") && i+1 < len(args):
			out = append(out, "-e", args[i+1])
		case strings.HasPrefix(a, "--extension="):
			out = append(out, "-e", strings.TrimPrefix(a, "--extension="))
		}
	}
	return out
}

// piListed reports whether `pi --list-models` output has a row for the
// model: provider and model are its first two columns.
func piListed(out []byte, provider, id string) bool {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 && f[1] == id && (provider == "" || f[0] == provider) {
			return true
		}
	}
	return false
}
