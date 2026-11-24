package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// Default pi physical models: Antigravity Neon XHigh first for both workers
// and the coordinator, failing over through Claude (Opus 5.5 for workers,
// Fable 5.2 for the coordinator) and OpenAI Codex (GPT-6.2 Astra). The pi
// kind's default --model is the role's virtual auto model, which routes
// through that ordered chain per turn (pi/router.ts); [agents.pi] router_*
// settings replace the physical chains/models and thresholds, and a physical
// --model opts out of routing.
// pi's built-in catalog has claude-opus-5-5 but not claude-fable-5-2, which
// needs a models entry in ~/.pi/agent/models.json (docs/pi.md).
const (
	piAntigravityModel = "antigravity/gemini-4-neon-xhigh"
	piWorkerModel      = "anthropic/claude-opus-5-5"
	piCoordinatorModel = "anthropic/claude-fable-5-2"
	piFallbackModel    = "openai-codex/gpt-6.2-astra"

	piWorkerVirtualModel      = "quartermaster/worker-auto"
	piCoordinatorVirtualModel = "quartermaster/coordinator-auto"

	piRouterThresholdDefault = 20.0 // percent provider headroom left
	piRouterRecoverDefault   = 30.0
)

// isPiVirtualModel reports whether model is one of quartermaster's virtual
// auto models, and which role's chain it routes through.
func isPiVirtualModel(model string) (role string, ok bool) {
	switch model {
	case piWorkerVirtualModel:
		return "worker", true
	case piCoordinatorVirtualModel:
		return "coordinator", true
	}
	return "", false
}

// piRouterConfig is router.json beside the extension files: the ordered
// physical model chains (with optional :thinking suffixes), global thresholds,
// and optional per-provider thresholds pi/router.ts routes with, read on each
// request. WorkerModel, CoordinatorModel and FallbackModel stay populated for
// backward compatibility with older router extensions.
type piRouterConfig struct {
	WorkerModels      []string                          `json:"worker_models"`
	CoordinatorModels []string                          `json:"coordinator_models"`
	WorkerModel       string                            `json:"worker_model"`
	CoordinatorModel  string                            `json:"coordinator_model"`
	FallbackModel     string                            `json:"fallback_model"`
	ThresholdPct      float64                           `json:"threshold_pct"`
	RecoverPct        float64                           `json:"recover_pct"`
	Providers         map[string]piRouterProviderConfig `json:"providers,omitempty"`
	QuotaMaxAgeSecs   int64                             `json:"quota_max_age_seconds"`
}

type piRouterProviderConfig struct {
	ThresholdPct float64 `json:"threshold_pct"`
	RecoverPct   float64 `json:"recover_pct"`
}

// piThinkingLevels matches pi's accepted thinking levels for :thinking suffixes.
var piThinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

func isValidPiThinkingLevel(level string) bool {
	return slices.Contains(piThinkingLevels, level)
}

// parsePiPhysicalModel splits a physical model entry ("provider/id" or
// "provider/id:thinking") into its components. Rejects whitespace, "@",
// colons in provider, empty segments, multiple colons, or unknown thinking levels.
func parsePiPhysicalModel(m string) (provider, id, thinking string, ok bool) {
	if m == "" || strings.ContainsAny(m, " \t\r\n@") {
		return "", "", "", false
	}
	p, rest, hasSlash := strings.Cut(m, "/")
	if !hasSlash || p == "" || rest == "" || strings.Contains(p, ":") {
		return "", "", "", false
	}
	id = rest
	if base, suffix, hasColon := strings.Cut(rest, ":"); hasColon {
		if base == "" || !isValidPiThinkingLevel(suffix) {
			return "", "", "", false
		}
		id, thinking = base, suffix
	}
	if id == "" || strings.HasPrefix(id, "/") || strings.HasSuffix(id, "/") || strings.Contains(id, "//") {
		return "", "", "", false
	}
	return p, id, thinking, true
}

// piModelBase returns the canonical "provider/id" without any :thinking suffix.
func piModelBase(m string) string {
	if p, id, _, ok := parsePiPhysicalModel(m); ok {
		return p + "/" + id
	}
	base, _, _ := strings.Cut(m, ":")
	return base
}

// dedupePiModels deduplicates physical model entries by canonical provider/id,
// keeping the first occurrence along with its optional :thinking override.
func dedupePiModels(models []string) []string {
	out := make([]string, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		if m == "" {
			continue
		}
		key := piModelBase(m)
		if !seen[key] {
			seen[key] = true
			out = append(out, m)
		}
	}
	return out
}

func (c Config) routerConfig() piRouterConfig {
	pc := c.Agents["pi"]
	fallback := piFallbackModel
	if pc.RouterFallbackModel != "" {
		fallback = pc.RouterFallbackModel
	}
	var workerChain []string
	switch {
	case len(pc.RouterWorkerModels) > 0:
		workerChain = dedupePiModels(pc.RouterWorkerModels)
	case pc.RouterWorkerModel != "":
		workerChain = dedupePiModels([]string{pc.RouterWorkerModel, fallback})
	default:
		workerChain = dedupePiModels([]string{piAntigravityModel, piWorkerModel, fallback})
	}
	var coordChain []string
	switch {
	case len(pc.RouterCoordinatorModels) > 0:
		coordChain = dedupePiModels(pc.RouterCoordinatorModels)
	case pc.RouterCoordinatorModel != "":
		coordChain = dedupePiModels([]string{pc.RouterCoordinatorModel, fallback})
	default:
		coordChain = dedupePiModels([]string{piAntigravityModel, piCoordinatorModel, fallback})
	}
	rc := piRouterConfig{
		WorkerModels:      workerChain,
		CoordinatorModels: coordChain,
		WorkerModel:       workerChain[0],
		CoordinatorModel:  coordChain[0],
		FallbackModel:     fallback,
		ThresholdPct:      piRouterThresholdDefault,
		RecoverPct:        piRouterRecoverDefault,
		QuotaMaxAgeSecs:   int64(quotaMaxAge.Seconds()),
	}
	if pc.RouterThreshold != nil {
		rc.ThresholdPct = *pc.RouterThreshold
	}
	if pc.RouterRecover != nil {
		rc.RecoverPct = *pc.RouterRecover
	}
	if len(pc.RouterProviders) > 0 {
		providers := make(map[string]piRouterProviderConfig, len(pc.RouterProviders))
		for rawKey, pcfg := range pc.RouterProviders {
			key := strings.ToLower(strings.TrimSpace(rawKey))
			if key == "" {
				continue
			}
			eff := piRouterProviderConfig{
				ThresholdPct: rc.ThresholdPct,
				RecoverPct:   rc.RecoverPct,
			}
			if pcfg.Threshold != nil {
				eff.ThresholdPct = *pcfg.Threshold
			}
			if pcfg.Recover != nil {
				eff.RecoverPct = *pcfg.Recover
			}
			providers[key] = eff
		}
		if len(providers) > 0 {
			rc.Providers = providers
		}
	}
	return rc
}

// modelsFor is the ordered physical model chain a virtual model for routerRole
// routes through.
func (rc piRouterConfig) modelsFor(routerRole string) []string {
	if routerRole == "coordinator" {
		return rc.CoordinatorModels
	}
	return rc.WorkerModels
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

func validatePiPhysicalModel(name, m string) error {
	p, id, _, ok := parsePiPhysicalModel(m)
	if !ok {
		return fmt.Errorf("[agents.pi] %s %q: want provider/model or provider/model:thinking (thinking: %s)", name, m, strings.Join(piThinkingLevels, ", "))
	}
	if _, virtual := isPiVirtualModel(p + "/" + id); virtual {
		return fmt.Errorf("[agents.pi] %s %q: a virtual model cannot route to another virtual model; name a physical model", name, m)
	}
	return nil
}

func validPercent(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 100
}

// validatePiRouter checks [agents.pi] router_*: physical models are
// provider/id[:thinking], thresholds are percents with recover not below
// threshold (a lower recover would flap back the turn after switching),
// per-provider overrides normalize cleanly without duplicates, and the
// settings apply only to pi.
func (c Config) validatePiRouter() error {
	for kind, a := range c.Agents {
		set := a.RouterWorkerModels != nil || a.RouterCoordinatorModels != nil ||
			a.RouterWorkerModel != "" || a.RouterCoordinatorModel != "" || a.RouterFallbackModel != "" ||
			a.RouterThreshold != nil || a.RouterRecover != nil || len(a.RouterProviders) > 0
		if kind != "pi" && set {
			return fmt.Errorf("[agents.%s] router_* applies only to pi", kind)
		}
	}
	pc := c.Agents["pi"]
	if pc.RouterWorkerModels != nil && len(pc.RouterWorkerModels) == 0 {
		return fmt.Errorf("[agents.pi] router_worker_models: must list at least one physical model")
	}
	if pc.RouterCoordinatorModels != nil && len(pc.RouterCoordinatorModels) == 0 {
		return fmt.Errorf("[agents.pi] router_coordinator_models: must list at least one physical model")
	}
	if len(pc.RouterWorkerModels) > 0 && pc.RouterWorkerModel != "" {
		return fmt.Errorf("[agents.pi] set router_worker_models or router_worker_model, not both")
	}
	if len(pc.RouterCoordinatorModels) > 0 && pc.RouterCoordinatorModel != "" {
		return fmt.Errorf("[agents.pi] set router_coordinator_models or router_coordinator_model, not both")
	}
	for _, list := range []struct {
		name   string
		models []string
	}{
		{"router_worker_models", pc.RouterWorkerModels},
		{"router_coordinator_models", pc.RouterCoordinatorModels},
	} {
		for _, m := range list.models {
			if err := validatePiPhysicalModel(list.name, m); err != nil {
				return err
			}
		}
	}
	for name, m := range map[string]string{
		"router_worker_model":      pc.RouterWorkerModel,
		"router_coordinator_model": pc.RouterCoordinatorModel,
		"router_fallback_model":    pc.RouterFallbackModel,
	} {
		if m != "" {
			if err := validatePiPhysicalModel(name, m); err != nil {
				return err
			}
		}
	}
	rc := c.routerConfig()
	for name, v := range map[string]float64{"router_threshold": rc.ThresholdPct, "router_recover": rc.RecoverPct} {
		if !validPercent(v) {
			return fmt.Errorf("[agents.pi] %s %v: want a percent from 0 to 100", name, v)
		}
	}
	if rc.RecoverPct < rc.ThresholdPct {
		return fmt.Errorf("[agents.pi] router_recover %v is below router_threshold %v: it would switch back while still under the threshold", rc.RecoverPct, rc.ThresholdPct)
	}
	seenProviders := make(map[string]string, len(pc.RouterProviders))
	for rawKey, pcfg := range pc.RouterProviders {
		key := strings.ToLower(strings.TrimSpace(rawKey))
		if key == "" || strings.ContainsAny(key, " \t\r\n/:@") {
			return fmt.Errorf("[agents.pi.router_providers] %q: want a non-empty provider name without spaces, '/', ':' or '@'", rawKey)
		}
		if prev, dup := seenProviders[key]; dup {
			return fmt.Errorf("[agents.pi.router_providers] duplicate provider %q (conflicts with %q)", rawKey, prev)
		}
		seenProviders[key] = rawKey
		effTh, effRec := rc.ThresholdPct, rc.RecoverPct
		if pcfg.Threshold != nil {
			effTh = *pcfg.Threshold
		}
		if pcfg.Recover != nil {
			effRec = *pcfg.Recover
		}
		if !validPercent(effTh) {
			return fmt.Errorf("[agents.pi.router_providers.%s] threshold %v: want a percent from 0 to 100", key, effTh)
		}
		if !validPercent(effRec) {
			return fmt.Errorf("[agents.pi.router_providers.%s] recover %v: want a percent from 0 to 100", key, effRec)
		}
		if effRec < effTh {
			return fmt.Errorf("[agents.pi.router_providers.%s] effective recover %v is below effective threshold %v", key, effRec, effTh)
		}
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
// extensions the start will) and at least one physical model in its ordered
// chain is listed by pi. That fails closed with actionable per-model errors
// when nothing in the chain is usable, while allowing environments that only
// have a subset of providers configured (such as Antigravity alone, or Claude
// and OpenAI without the Antigravity bridge).
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
	extArgs := piExtensionArgs(args)
	if len(extArgs) == 0 {
		return fmt.Errorf("pi's %s model %s is registered by quartermaster's pi extension, which only a pi start on this machine loads: set [agents.pi] %s to a physical model for a remote pi", role, model, map[bool]string{true: "coordinator_args", false: "args"}[role == "coordinator"])
	}
	provider, id, _ := strings.Cut(model, "/")
	out, _, err := run(append(slices.Clone(extArgs), "--list-models", id)...)
	if err != nil {
		return fmt.Errorf("pi: %w (is pi installed?)", err)
	}
	if !piListed(out, provider, id) {
		return fmt.Errorf("pi doesn't list the %s model %s even with quartermaster's extension loaded (a pi too old for virtual models, which need pi 1.6?): check `pi --version`, or set [agents.pi] %s to a physical model", role, model, map[bool]string{true: "coordinator_args", false: "args"}[role == "coordinator"])
	}
	rc := c.routerConfig()
	chain := rc.modelsFor(routerRole)
	for _, physical := range chain {
		p, mid, _, ok := parsePiPhysicalModel(physical)
		if !ok {
			if pp, mm, hasSlash := strings.Cut(physical, "/"); hasSlash {
				p, mid = pp, mm
			} else {
				p, mid = "", physical
			}
		}
		out, _, err := run(append(slices.Clone(extArgs), "--list-models", mid)...)
		if err != nil {
			return fmt.Errorf("pi: %w (is pi installed?)", err)
		}
		if piListed(out, p, mid) {
			return nil
		}
	}
	var errs []string
	for _, physical := range chain {
		p, mid, _, ok := parsePiPhysicalModel(physical)
		base := physical
		if ok {
			base = p + "/" + mid
		} else if pp, _, hasSlash := strings.Cut(physical, "/"); hasSlash {
			p = pp
		}
		errs = append(errs, fmt.Sprintf("%s: %v", physical, explainMissingPiModel(run, role, base, p)))
	}
	return fmt.Errorf("%s has no usable physical model in [%s]: %s", model, strings.Join(chain, ", "), strings.Join(errs, "; "))
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
	return explainMissingPiModel(run, role, model, provider)
}

func explainMissingPiModel(run piRunner, role, model, provider string) error {
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
