package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// piExtension is quartermaster's pi extension (pi/quartermaster.ts): the
// bash hooks and subagent guard, the prompt-source entries the ticker reads,
// and dialogs reported to herdr as blocked.
//
//go:embed pi/quartermaster.ts
var piExtension []byte

//go:embed pi/coordinator.ts
var piCoordinatorExtension []byte

//go:embed pi/router.ts
var piRouterExtension []byte

// piExtensionFile writes the embedded extension to the state directory, when
// it differs, and returns its path. pi loads it with -e, so it needs no
// install and always matches the binary.
func piExtensionFile() (string, error) {
	if _, err := writePiExtension("coordinator.ts", piCoordinatorExtension); err != nil {
		return "", err
	}
	if _, err := writePiExtension("router.ts", piRouterExtension); err != nil {
		return "", err
	}
	return writePiExtension("quartermaster.ts", piExtension)
}

func writePiExtension(name string, content []byte) (string, error) {
	path := filepath.Join(stateDir(), "pi", name)
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, content) {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// piLaunch checks a pi start's model and adds what every quartermaster pi
// gets: the extension, on this machine, and the bead's session for a worker
// (id "" for the coordinator). On this machine the extension and the
// router/quota files are written before the model check, so the check's
// `pi --list-models` runs with the extension loaded and sees the virtual
// models it registers.
func (c Config) piLaunch(host string, args []string, role, id string) ([]string, error) {
	if host == localHost {
		ext, err := piExtensionFile()
		if err != nil {
			return nil, fmt.Errorf("pi extension: %w", err)
		}
		routerPath, err := c.writePiRouterConfig()
		if err != nil {
			return nil, fmt.Errorf("pi router: %w", err)
		}
		if err := writeQuotaSnapshot(time.Now()); err != nil {
			return nil, fmt.Errorf("pi quota snapshot: %w", err)
		}
		args = append(args, "-e", ext, "--quartermaster-router", routerPath)
		// Worker prompts and coordinator notifications use separate protocols.
		if role == "coordinator" {
			args = append(args, "--quartermaster-coordinator", piCoordinatorSocket())
		}
		if id != "" {
			if err := c.writePiControlConfig(); err != nil {
				return nil, fmt.Errorf("pi control: %w", err)
			}
			args = append(args, "--quartermaster-control", piControlSocket(agentName(id)))
		}
	}
	if err := c.checkPiModel(c.piOn(host), args, role); err != nil {
		return nil, err
	}
	if id != "" {
		args = append(args, piSessionArgs(id)...)
	}
	return args, nil
}

// Default pi physical models: workers on Opus 5.5, the coordinator on
// Fable 5.2, with OpenAI Codex as the quota fallback. The pi kind's default
// --model is the role's virtual auto model, which routes between them per
// turn (pi/router.ts); [agents.pi] router_* settings replace the physical
// models and thresholds, and a physical --model opts out of routing.
// pi's built-in catalog has claude-opus-5-5 but not claude-fable-5-2, which
// needs a models entry in ~/.pi/agent/models.json (docs/pi.md).
const (
	piWorkerModel      = "anthropic/claude-opus-5-5"
	piCoordinatorModel = "anthropic/claude-fable-5-2"
	piFallbackModel    = "openai-codex/gpt-6.2-astra"

	piWorkerVirtualModel      = "quartermaster/worker-auto"
	piCoordinatorVirtualModel = "quartermaster/coordinator-auto"

	piRouterThresholdDefault = 20.0 // percent Claude headroom left
	piRouterRecoverDefault   = 30.0
)

// isPiVirtualModel reports whether model is one of quartermaster's virtual
// auto models, and which role's Claude physical model it routes from.
func isPiVirtualModel(model string) (role string, ok bool) {
	switch model {
	case piWorkerVirtualModel:
		return "worker", true
	case piCoordinatorVirtualModel:
		return "coordinator", true
	}
	return "", false
}

// piRouterConfig is router.json beside the extension files: the physical
// models and thresholds pi/router.ts routes with, read on each request.
type piRouterConfig struct {
	WorkerModel      string  `json:"worker_model"`
	CoordinatorModel string  `json:"coordinator_model"`
	FallbackModel    string  `json:"fallback_model"`
	ThresholdPct     float64 `json:"threshold_pct"`
	RecoverPct       float64 `json:"recover_pct"`
	QuotaMaxAgeSecs  int64   `json:"quota_max_age_seconds"`
}

func (c Config) routerConfig() piRouterConfig {
	pc := c.Agents["pi"]
	rc := piRouterConfig{
		WorkerModel:      piWorkerModel,
		CoordinatorModel: piCoordinatorModel,
		FallbackModel:    piFallbackModel,
		ThresholdPct:     piRouterThresholdDefault,
		RecoverPct:       piRouterRecoverDefault,
		QuotaMaxAgeSecs:  int64(quotaMaxAge.Seconds()),
	}
	if pc.RouterWorkerModel != "" {
		rc.WorkerModel = pc.RouterWorkerModel
	}
	if pc.RouterCoordinatorModel != "" {
		rc.CoordinatorModel = pc.RouterCoordinatorModel
	}
	if pc.RouterFallbackModel != "" {
		rc.FallbackModel = pc.RouterFallbackModel
	}
	if pc.RouterThreshold != nil {
		rc.ThresholdPct = *pc.RouterThreshold
	}
	if pc.RouterRecover != nil {
		rc.RecoverPct = *pc.RouterRecover
	}
	return rc
}

// claudeModelFor is the Claude-side physical model a virtual model for
// routerRole routes from.
func (rc piRouterConfig) claudeModelFor(routerRole string) string {
	if routerRole == "coordinator" {
		return rc.CoordinatorModel
	}
	return rc.WorkerModel
}

func piRouterConfigPath() string { return filepath.Join(stateDir(), "pi", "router.json") }

// writePiRouterConfig writes router.json from [agents.pi] router_* (else the
// defaults), when it differs, and returns its path.
func (c Config) writePiRouterConfig() (string, error) {
	b, err := json.MarshalIndent(c.routerConfig(), "", "  ")
	if err != nil {
		return "", err
	}
	path := piRouterConfigPath()
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, b) {
		return path, nil
	}
	return path, writeAtomic(path, b)
}

// validatePiRouter checks [agents.pi] router_*: physical models are
// provider/id, thresholds are percents with recover not below threshold
// (a lower recover would flap back the turn after switching), and the
// settings apply only to pi.
func (c Config) validatePiRouter() error {
	for kind, a := range c.Agents {
		set := a.RouterWorkerModel != "" || a.RouterCoordinatorModel != "" || a.RouterFallbackModel != "" ||
			a.RouterThreshold != nil || a.RouterRecover != nil
		if kind != "pi" && set {
			return fmt.Errorf("[agents.%s] router_* applies only to pi", kind)
		}
	}
	pc := c.Agents["pi"]
	for name, m := range map[string]string{
		"router_worker_model":      pc.RouterWorkerModel,
		"router_coordinator_model": pc.RouterCoordinatorModel,
		"router_fallback_model":    pc.RouterFallbackModel,
	} {
		if m != "" {
			if p, id, ok := strings.Cut(m, "/"); !ok || p == "" || id == "" || strings.ContainsAny(m, " \t") {
				return fmt.Errorf("[agents.pi] %s %q: want provider/model", name, m)
			}
			if _, virtual := isPiVirtualModel(m); virtual {
				return fmt.Errorf("[agents.pi] %s %q: a virtual model cannot route to another virtual model; name a physical model", name, m)
			}
		}
	}
	rc := c.routerConfig()
	for name, v := range map[string]float64{"router_threshold": rc.ThresholdPct, "router_recover": rc.RecoverPct} {
		if v < 0 || v > 100 {
			return fmt.Errorf("[agents.pi] %s %v: want a percent from 0 to 100", name, v)
		}
	}
	if rc.RecoverPct < rc.ThresholdPct {
		return fmt.Errorf("[agents.pi] router_recover %v is below router_threshold %v: it would switch back while still under the threshold", rc.RecoverPct, rc.ThresholdPct)
	}
	return nil
}

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
//
// A virtual auto model is ready when pi lists it (the check loads the same
// extensions the start will) and both physical models it routes between
// pass this same check: a start whose Claude model has no credentials
// fails here, at dispatch, instead of silently starting on the fallback.
func (c Config) checkPiModel(run piRunner, args []string, role string) error {
	model := piModel(args)
	if model == "" {
		return nil
	}
	if routerRole, virtual := isPiVirtualModel(model); virtual {
		return c.checkPiVirtualModel(run, args, role, model, routerRole)
	}
	return checkPiPhysicalModel(run, args, role, model)
}

func (c Config) checkPiVirtualModel(run piRunner, args []string, role, model, routerRole string) error {
	if len(piExtensionArgs(args)) == 0 {
		return fmt.Errorf("pi's %s model %s is registered by quartermaster's pi extension, which only a pi start on this machine loads: set [agents.pi] %s to a physical model for a remote pi", role, model, map[bool]string{true: "coordinator_args", false: "args"}[role == "coordinator"])
	}
	provider, id, _ := strings.Cut(model, "/")
	out, _, err := run(append(piExtensionArgs(args), "--list-models", id)...)
	if err != nil {
		return fmt.Errorf("pi: %w (is pi installed?)", err)
	}
	if !piListed(out, provider, id) {
		return fmt.Errorf("pi doesn't list the %s model %s even with quartermaster's extension loaded (a pi too old for virtual models, which need pi 1.6?): check `pi --version`, or set [agents.pi] %s to a physical model", role, model, map[bool]string{true: "coordinator_args", false: "args"}[role == "coordinator"])
	}
	rc := c.routerConfig()
	for _, physical := range []string{rc.claudeModelFor(routerRole), rc.FallbackModel} {
		if err := checkPiPhysicalModel(run, args, role, physical); err != nil {
			return fmt.Errorf("%s routes to %s: %w", model, physical, err)
		}
	}
	return nil
}

func checkPiPhysicalModel(run piRunner, args []string, role, model string) error {
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
