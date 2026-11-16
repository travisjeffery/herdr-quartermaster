package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

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
	out, _, err := run("--list-models", id)
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
