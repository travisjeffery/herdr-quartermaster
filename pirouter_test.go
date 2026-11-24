package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The router decision itself is TypeScript (pi/router.ts); its offline
// cases live in testdata/pi-router-test.mjs and run under node, like the
// control-socket harness in picontrol_test.go.
func TestPiRouterDecisions(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("no node")
	}
	out, err := exec.Command("node", "testdata/pi-router-test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	if !strings.Contains(string(out), "passed") {
		t.Fatalf("router tests: %s", out)
	}
	t.Log(strings.TrimSpace(string(out)))
}

func TestQuotaSnapshot(t *testing.T) {
	now := time.Unix(1790350000, 0)
	reading := &quotaReading{At: now.Add(-time.Minute), Windows: []quotaWindow{
		{Used: 60, ResetsAt: now.Add(time.Hour)},
		{Used: 90, ResetsAt: now.Add(24 * time.Hour)},
	}}
	p := snapshotProvider(reading, now)
	if p == nil || !p.Fresh || p.HeadroomPct == nil || *p.HeadroomPct != 10 {
		t.Fatalf("fresh reading: %+v", p)
	}
	if p.TightResetsAt == nil || !p.TightResetsAt.Equal(now.Add(24*time.Hour)) {
		t.Errorf("tight reset = %v, want the 90%%-used window's", p.TightResetsAt)
	}
	stale := &quotaReading{At: now.Add(-7 * time.Hour), Windows: reading.Windows}
	if p := snapshotProvider(stale, now); p == nil || p.Fresh || p.HeadroomPct != nil {
		t.Errorf("stale reading should be present but unfresh: %+v", p)
	}
	if snapshotProvider(nil, now) != nil || snapshotProvider(&quotaReading{At: now}, now) != nil {
		t.Error("no reading is a null provider")
	}
	// Every window reset: full headroom, no tight reset.
	reset := &quotaReading{At: now, Windows: []quotaWindow{{Used: 95, ResetsAt: now.Add(-time.Minute)}}}
	if p := snapshotProvider(reset, now); p == nil || !p.Fresh || *p.HeadroomPct != 100 || p.TightResetsAt != nil {
		t.Errorf("reset windows: %+v", p)
	}
}

func TestWriteQuotaSnapshot(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	now := time.Unix(1790350000, 0)
	if err := writeQuotaSnapshot(now); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(piQuotaSnapshotPath())
	if err != nil {
		t.Fatal(err)
	}
	var snap quotaSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Antigravity != nil || snap.Claude != nil || snap.OpenAI != nil {
		t.Errorf("no local readings should snapshot as null: %s", b)
	}
	// Same readings a minute later: no rewrite (mtime stands).
	fi1, _ := os.Stat(piQuotaSnapshotPath())
	if err := writeQuotaSnapshot(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if fi2, _ := os.Stat(piQuotaSnapshotPath()); !fi2.ModTime().Equal(fi1.ModTime()) {
		t.Error("unchanged snapshot was rewritten")
	}
	// Writing an Antigravity observation updates quota.json with fresh antigravity headroom.
	obsDir := filepath.Join(home, ".local", "state", "herdr", "plugins", "herdr-agent-quota")
	if err := os.MkdirAll(obsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	obsData, err := os.ReadFile("testdata/agy-statusline.observation.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(obsDir, "agy-statusline.observation.json"), obsData, 0o644); err != nil {
		t.Fatal(err)
	}
	obsNow := time.Unix(1795474900, 0)
	if err := writeQuotaSnapshot(obsNow); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(piQuotaSnapshotPath())
	if err != nil {
		t.Fatal(err)
	}
	var snap2 quotaSnapshot
	if err := json.Unmarshal(b, &snap2); err != nil {
		t.Fatal(err)
	}
	if snap2.Antigravity == nil || !snap2.Antigravity.Fresh || snap2.Antigravity.HeadroomPct == nil || *snap2.Antigravity.HeadroomPct < 94.07 || *snap2.Antigravity.HeadroomPct > 94.09 {
		t.Errorf("antigravity snapshot = %+v, want fresh ~94.08%%", snap2.Antigravity)
	}
}

func TestWritePiRouterConfig(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := defaultConfig()
	path, err := cfg.writePiRouterConfig()
	if err != nil {
		t.Fatal(err)
	}
	var got piRouterConfig
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	wantWorker := []string{piAntigravityModel, piWorkerModel, piFallbackModel}
	wantCoord := []string{piAntigravityModel, piCoordinatorModel, piFallbackModel}
	if strings.Join(got.WorkerModels, ",") != strings.Join(wantWorker, ",") ||
		strings.Join(got.CoordinatorModels, ",") != strings.Join(wantCoord, ",") ||
		got.WorkerModel != piAntigravityModel || got.CoordinatorModel != piAntigravityModel ||
		got.FallbackModel != piFallbackModel || got.ThresholdPct != 20 || got.RecoverPct != 30 ||
		got.QuotaMaxAgeSecs != int64(quotaMaxAge.Seconds()) {
		t.Errorf("router config = %+v", got)
	}
	lo, hi := 15.0, 45.0
	cfg.Agents = map[string]AgentConfig{"pi": {RouterThreshold: &lo, RouterRecover: &hi, RouterFallbackModel: "openai/gpt-6.2-astra"}}
	if _, err := cfg.writePiRouterConfig(); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if err := json.Unmarshal(b, &got); err != nil || got.ThresholdPct != 15 || got.RecoverPct != 45 || got.FallbackModel != "openai/gpt-6.2-astra" ||
		strings.Join(got.WorkerModels, ",") != strings.Join([]string{piAntigravityModel, piWorkerModel, "openai/gpt-6.2-astra"}, ",") {
		t.Errorf("configured fallback router config = %+v, %v", got, err)
	}
	// Explicit ordered chains override defaults (including :thinking overrides,
	// canonical deduplication, and per-provider threshold/recover overrides).
	agyLo, agyHi := 10.0, 25.0
	anthLo := 15.0
	cfg.Agents = map[string]AgentConfig{"pi": {
		RouterWorkerModels:      []string{"antigravity/gemini-4-neon-xhigh", "anthropic/claude-opus-5-5:high", "anthropic/claude-opus-5-5:low"},
		RouterCoordinatorModels: []string{"anthropic/claude-fable-5-2:medium", "openai-codex/gpt-6.2-astra:xhigh"},
		RouterProviders: map[string]RouterProviderConfig{
			"Antigravity": {Threshold: &agyLo, Recover: &agyHi},
			"anthropic":   {Threshold: &anthLo},
		},
	}}
	if _, err := cfg.writePiRouterConfig(); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	got = piRouterConfig{}
	if err := json.Unmarshal(b, &got); err != nil ||
		strings.Join(got.WorkerModels, ",") != "antigravity/gemini-4-neon-xhigh,anthropic/claude-opus-5-5:high" ||
		strings.Join(got.CoordinatorModels, ",") != "anthropic/claude-fable-5-2:medium,openai-codex/gpt-6.2-astra:xhigh" ||
		len(got.Providers) != 2 ||
		got.Providers["antigravity"].ThresholdPct != 10 || got.Providers["antigravity"].RecoverPct != 25 ||
		got.Providers["anthropic"].ThresholdPct != 15 || got.Providers["anthropic"].RecoverPct != 30 {
		t.Errorf("explicit chain + providers router config = %+v, %v", got, err)
	}
}

func TestValidatePiRouter(t *testing.T) {
	cfg := defaultConfig()
	if err := cfg.validateAgents(); err != nil {
		t.Fatal(err)
	}
	lo, hi, over, neg := 10.0, 25.0, 101.0, -1.0
	provLo, provHi, aboveGlobalRecover := 12.0, 22.0, 35.0
	cfg.Agents = map[string]AgentConfig{"pi": {
		RouterWorkerModels:      []string{piAntigravityModel, piWorkerModel + ":high", piFallbackModel + ":xhigh"},
		RouterCoordinatorModels: []string{piAntigravityModel + ":off", piCoordinatorModel + ":minimal", piFallbackModel + ":low"},
		RouterThreshold:         &lo,
		RouterRecover:           &hi,
		RouterProviders: map[string]RouterProviderConfig{
			"Antigravity": {Threshold: &provLo, Recover: &provHi},
			"anthropic":   {Threshold: &provLo},
			"google":      {Recover: &provHi},
		},
	}}
	if err := cfg.validateAgents(); err != nil {
		t.Fatal(err)
	}
	// All valid pi thinking levels accepted on both chains and legacy singular fields.
	for _, lvl := range piThinkingLevels {
		cfg.Agents = map[string]AgentConfig{"pi": {
			RouterWorkerModel:      piWorkerModel + ":" + lvl,
			RouterCoordinatorModel: piCoordinatorModel + ":" + lvl,
			RouterFallbackModel:    piFallbackModel + ":" + lvl,
		}}
		if err := cfg.validateAgents(); err != nil {
			t.Errorf("thinking level %q should be valid: %v", lvl, err)
		}
	}
	for name, bad := range map[string]map[string]AgentConfig{
		"recover below threshold":             {"pi": {RouterThreshold: &hi, RouterRecover: &lo}},
		"percent over 100":                    {"pi": {RouterThreshold: &over}},
		"negative percent":                    {"pi": {RouterThreshold: &neg}},
		"model without provider":              {"pi": {RouterWorkerModel: "opus"}},
		"virtual as physical":                 {"pi": {RouterFallbackModel: piWorkerVirtualModel}},
		"virtual with thinking":               {"pi": {RouterWorkerModels: []string{piWorkerVirtualModel + ":high"}}},
		"invalid thinking suffix":             {"pi": {RouterWorkerModels: []string{piWorkerModel + ":turbo"}}},
		"empty thinking suffix":               {"pi": {RouterWorkerModels: []string{piWorkerModel + ":"}}},
		"uppercase thinking suffix":           {"pi": {RouterWorkerModels: []string{piWorkerModel + ":HIGH"}}},
		"double colon suffix":                 {"pi": {RouterWorkerModels: []string{piWorkerModel + ":high:low"}}},
		"colon in provider":                   {"pi": {RouterWorkerModels: []string{"bad:prov/model"}}},
		"legacy worker invalid thinking":      {"pi": {RouterWorkerModel: piWorkerModel + ":bogus"}},
		"legacy fallback invalid thinking":    {"pi": {RouterFallbackModel: piFallbackModel + ":bogus"}},
		"router on another kind":              {"claude": {RouterThreshold: &lo}},
		"router_providers on another kind":    {"claude": {RouterProviders: map[string]RouterProviderConfig{"anthropic": {Threshold: &lo}}}},
		"empty worker chain":                  {"pi": {RouterWorkerModels: []string{}}},
		"empty coordinator chain":             {"pi": {RouterCoordinatorModels: []string{}}},
		"chain entry without provider":        {"pi": {RouterWorkerModels: []string{"bad"}}},
		"virtual model in chain":              {"pi": {RouterCoordinatorModels: []string{piCoordinatorVirtualModel}}},
		"both worker chain and single":        {"pi": {RouterWorkerModels: []string{piAntigravityModel}, RouterWorkerModel: piWorkerModel}},
		"both coord chain and single":         {"pi": {RouterCoordinatorModels: []string{piAntigravityModel}, RouterCoordinatorModel: piCoordinatorModel}},
		"chain on another kind":               {"claude": {RouterWorkerModels: []string{piAntigravityModel}}},
		"provider threshold over 100":         {"pi": {RouterProviders: map[string]RouterProviderConfig{"anthropic": {Threshold: &over}}}},
		"provider recover below threshold":    {"pi": {RouterProviders: map[string]RouterProviderConfig{"anthropic": {Threshold: &hi, Recover: &lo}}}},
		"provider threshold above global rec": {"pi": {RouterProviders: map[string]RouterProviderConfig{"anthropic": {Threshold: &aboveGlobalRecover}}}},
		"provider recover below global th":    {"pi": {RouterProviders: map[string]RouterProviderConfig{"anthropic": {Recover: &lo}}}},
		"provider duplicate ignoring case":    {"pi": {RouterProviders: map[string]RouterProviderConfig{"Anthropic": {Threshold: &lo}, "anthropic": {Threshold: &lo}}}},
		"provider empty key":                  {"pi": {RouterProviders: map[string]RouterProviderConfig{"   ": {Threshold: &lo}}}},
		"provider invalid key with slash":     {"pi": {RouterProviders: map[string]RouterProviderConfig{"foo/bar": {Threshold: &lo}}}},
	} {
		cfg.Agents = bad
		if err := cfg.validateAgents(); err == nil {
			t.Errorf("%s passed", name)
		}
	}
}

func TestTickerHotReloadPiRouterConfig(t *testing.T) {
	cfgDir := t.TempDir()
	stDir := t.TempDir()
	home := t.TempDir()
	t.Setenv("QUARTERMASTER_CONFIG_DIR", cfgDir)
	t.Setenv("QUARTERMASTER_STATE_DIR", stDir)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))

	cfgPath := filepath.Join(cfgDir, "config.toml")
	writeCfg := func(body string) {
		t.Helper()
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Initial startup config.
	writeCfg(`repo = "/tmp/test-repo"

[agents.pi]
router_worker_models = ["antigravity/gemini-4-neon-xhigh", "anthropic/claude-opus-5-5:high"]
router_threshold = 20
router_recover = 30
`)
	startupCfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	routerCfg := startupCfg
	var logs bytes.Buffer
	logf := func(format string, args ...any) { fmt.Fprintf(&logs, format+"\n", args...) }
	now := time.Unix(1795474900, 0)

	refreshPiRouter(&routerCfg, loadConfig, now, logf)
	readRouter := func() piRouterConfig {
		t.Helper()
		b, err := os.ReadFile(piRouterConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		var rc piRouterConfig
		if err := json.Unmarshal(b, &rc); err != nil {
			t.Fatal(err)
		}
		return rc
	}

	got1 := readRouter()
	if !slices.Equal(got1.WorkerModels, []string{"antigravity/gemini-4-neon-xhigh", "anthropic/claude-opus-5-5:high"}) {
		t.Fatalf("initial router.json = %+v", got1)
	}

	// 2. Edit config.toml without restarting ticker -> next refresh updates router.json.
	writeCfg(`repo = "/tmp/test-repo"

[agents.pi]
router_worker_models = ["openai-codex/gpt-6.2-astra:xhigh", "anthropic/claude-opus-5-5:medium"]
router_threshold = 15
router_recover = 35

[agents.pi.router_providers.openai-codex]
threshold = 12
recover = 28
`)
	refreshPiRouter(&routerCfg, loadConfig, now.Add(time.Minute), logf)
	got2 := readRouter()
	if !slices.Equal(got2.WorkerModels, []string{"openai-codex/gpt-6.2-astra:xhigh", "anthropic/claude-opus-5-5:medium"}) ||
		got2.ThresholdPct != 15 || got2.RecoverPct != 35 ||
		got2.Providers["openai-codex"].ThresholdPct != 12 || got2.Providers["openai-codex"].RecoverPct != 28 {
		t.Fatalf("hot-reloaded router.json = %+v", got2)
	}

	// 3. Transient syntax error in config.toml -> logs error and preserves last valid router.json.
	logs.Reset()
	writeCfg(`repo = "/tmp/test-repo"
[agents.pi]
router_worker_models = [unclosed
`)
	refreshPiRouter(&routerCfg, loadConfig, now.Add(2*time.Minute), logf)
	if !strings.Contains(logs.String(), "pi router config reload:") {
		t.Errorf("expected reload error log, got: %q", logs.String())
	}
	got3 := readRouter()
	if !slices.Equal(got3.WorkerModels, got2.WorkerModels) || got3.ThresholdPct != 15 || got3.Providers["openai-codex"].ThresholdPct != 12 {
		t.Fatalf("invalid syntax clobbered last valid router.json: got %+v, want %+v", got3, got2)
	}

	// 4. Transient validation error (bad :thinking) -> logs error and preserves last valid router.json.
	logs.Reset()
	writeCfg(`repo = "/tmp/test-repo"
[agents.pi]
router_worker_models = ["anthropic/claude-opus-5-5:bogus"]
`)
	refreshPiRouter(&routerCfg, loadConfig, now.Add(3*time.Minute), logf)
	if !strings.Contains(logs.String(), "pi router config reload:") {
		t.Errorf("expected validation reload error log, got: %q", logs.String())
	}
	got4 := readRouter()
	if !slices.Equal(got4.WorkerModels, got2.WorkerModels) {
		t.Fatalf("invalid validation clobbered last valid router.json: got %+v, want %+v", got4, got2)
	}

	// 5. Fixing config.toml resumes live updates.
	writeCfg(`repo = "/tmp/test-repo"
[agents.pi]
router_worker_models = ["anthropic/claude-opus-5-5:high"]
`)
	refreshPiRouter(&routerCfg, loadConfig, now.Add(4*time.Minute), logf)
	got5 := readRouter()
	if !slices.Equal(got5.WorkerModels, []string{"anthropic/claude-opus-5-5:high"}) {
		t.Fatalf("recovered router.json = %+v", got5)
	}

	// 6. If router.json was missing when load failed, writes lastGood rather than leaving nothing.
	if err := os.Remove(piRouterConfigPath()); err != nil {
		t.Fatal(err)
	}
	refreshPiRouter(&routerCfg, func() (Config, error) { return Config{}, errors.New("boom") }, now.Add(5*time.Minute), logf)
	got6 := readRouter()
	if !slices.Equal(got6.WorkerModels, []string{"anthropic/claude-opus-5-5:high"}) {
		t.Fatalf("fallback to lastGood when router.json missing = %+v", got6)
	}
}

func TestUpdateRouterModelsTOMLPreservesFormattingAndComments(t *testing.T) {
	original := `# Top-level comment
repo = "~/src/myproject"
worker_agent = "pi"

[agents.claude]
args = ["--model", "claude-opus-5-5"]

[agents.pi]
# Keep this comment!
args = ["--no-approve", "--model", "quartermaster/worker-auto", "--thinking", "xhigh"]
# Multi-line ask with brackets and regexes
ask = [
  '\bkubectl\b.*\bdelete\b',
  '\baws\b.*\b(delete|rm)\b',
]
router_threshold = 20
router_recover = 30
# Commented example should stay untouched:
# router_worker_models = ["example/old"]

[agents.pi.router_providers.antigravity]
threshold = 10
recover = 25

[autonomy]
level = 4
`
	// 1. Insert router_worker_models into existing [agents.pi] without touching anything else.
	updated, cleared, err := updateRouterModelsTOML(original, "worker", []string{
		"openai-codex/gpt-6.2-astra:xhigh",
		"anthropic/claude-opus-5-5:high",
	})
	if err != nil || cleared {
		t.Fatalf("insert failed: err=%v cleared=%v", err, cleared)
	}
	for _, mustKeep := range []string{
		"# Top-level comment",
		"# Keep this comment!",
		"# Multi-line ask with brackets and regexes",
		`'\bkubectl\b.*\bdelete\b'`,
		"# Commented example should stay untouched:",
		`# router_worker_models = ["example/old"]`,
		"[agents.pi.router_providers.antigravity]",
		"threshold = 10",
		"[autonomy]\nlevel = 4",
	} {
		if !strings.Contains(updated, mustKeep) {
			t.Errorf("lost %q in updated TOML:\n%s", mustKeep, updated)
		}
	}
	// Verify it stayed inside [agents.pi], before [agents.pi.router_providers.antigravity].
	if strings.Index(updated, "router_worker_models = [") > strings.Index(updated, "[agents.pi.router_providers.antigravity]") {
		t.Fatalf("router_worker_models placed after subtable:\n%s", updated)
	}

	// 2. Replace an existing multi-line array (including inline comment with ']') cleanly.
	withMulti := strings.Replace(updated,
		`  "openai-codex/gpt-6.2-astra:xhigh",`,
		`  "openai-codex/gpt-6.2-astra:xhigh", # comment with ] inside`,
		1,
	)
	replaced, cleared, err := updateRouterModelsTOML(withMulti, "worker", []string{"anthropic/claude-opus-5-5:high"})
	if err != nil || cleared {
		t.Fatalf("replace failed: err=%v cleared=%v", err, cleared)
	}
	if !strings.Contains(replaced, `router_worker_models = ["anthropic/claude-opus-5-5:high"]`) ||
		strings.Contains(replaced, "openai-codex/gpt-6.2-astra:xhigh") {
		t.Fatalf("unexpected replace result:\n%s", replaced)
	}
	if !strings.Contains(replaced, "# Keep this comment!") || !strings.Contains(replaced, "[agents.pi.router_providers.antigravity]") {
		t.Fatalf("replace clobbered neighbors:\n%s", replaced)
	}

	// 3. Clearing conflicting legacy router_worker_model without touching router_coordinator_model.
	withLegacy := `repo = "/tmp/repo"

[agents.pi]
# legacy settings
router_worker_model = "anthropic/claude-opus-5-5"
router_coordinator_model = "anthropic/claude-fable-5-2"
`
	migrated, cleared, err := updateRouterModelsTOML(withLegacy, "worker", []string{"antigravity/gemini-4-neon-xhigh", "anthropic/claude-opus-5-5:high"})
	if err != nil || !cleared {
		t.Fatalf("legacy migration: err=%v cleared=%v", err, cleared)
	}
	if strings.Contains(migrated, "router_worker_model =") ||
		!strings.Contains(migrated, `router_coordinator_model = "anthropic/claude-fable-5-2"`) ||
		!strings.Contains(migrated, "# legacy settings") {
		t.Fatalf("unexpected legacy migration result:\n%s", migrated)
	}

	// 4. Appending [agents.pi] when absent.
	minimal := "repo = \"/tmp/repo\"\n"
	appended, cleared, err := updateRouterModelsTOML(minimal, "coordinator", []string{"anthropic/claude-fable-5-2:high"})
	if err != nil || cleared {
		t.Fatalf("append failed: err=%v cleared=%v", err, cleared)
	}
	wantAppended := "repo = \"/tmp/repo\"\n\n[agents.pi]\nrouter_coordinator_models = [\"anthropic/claude-fable-5-2:high\"]\n"
	if appended != wantAppended {
		t.Fatalf("appended =\n%q\nwant:\n%q", appended, wantAppended)
	}
}

func TestRouterCLIStatusSyncSet(t *testing.T) {
	cfgDir := t.TempDir()
	stDir := t.TempDir()
	home := t.TempDir()
	t.Setenv("QUARTERMASTER_CONFIG_DIR", cfgDir)
	t.Setenv("QUARTERMASTER_STATE_DIR", stDir)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))

	cfgPath := filepath.Join(cfgDir, "config.toml")
	initialTOML := `# My custom header
repo = "/tmp/test-repo"

[agents.pi]
# Preserve this comment
router_worker_model = "anthropic/claude-opus-5-5"
router_threshold = 20
router_recover = 30

[agents.pi.router_providers.antigravity]
threshold = 10
recover = 25
`
	if err := os.WriteFile(cfgPath, []byte(initialTOML), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}

	// Status before sync reports router.json missing and shows chains + per-provider thresholds.
	var out bytes.Buffer
	now := time.Unix(1795474900, 0)
	if err := routerStatus(cfg, now, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"worker:      anthropic/claude-opus-5-5 -> openai-codex/gpt-6.2-astra",
		"thresholds:  switch <= 20%, recover >= 30%",
		"antigravity: switch <= 10%, recover >= 25%",
		"router.json: missing",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("initial status missing %q:\n%s", want, s)
		}
	}

	// Sync writes extension files, router.json, and quota.json.
	out.Reset()
	if err := routerSync(cfg, now, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "synced "+piRouterConfigPath()) {
		t.Errorf("sync output = %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(stDir, "pi", "quartermaster.ts")); err != nil {
		t.Errorf("sync did not write quartermaster.ts: %v", err)
	}

	// Write a quota.json including a generic extra provider ("google") and check status output.
	quotaBody := fmt.Sprintf(`{
  "generated_at": %q,
  "antigravity": {"headroom_pct": 88.5, "fresh": true, "at": %q, "tight_resets_at": %q},
  "claude": {"headroom_pct": null, "fresh": false, "at": %q, "tight_resets_at": null},
  "openai": null,
  "google": {"headroom_pct": 64.0, "fresh": true, "at": %q, "tight_resets_at": null}
}`, now.Add(-30*time.Second).UTC().Format(time.RFC3339),
		now.Add(-45*time.Second).UTC().Format(time.RFC3339),
		now.Add(2*time.Hour).UTC().Format(time.RFC3339),
		now.Add(-8*time.Hour).UTC().Format(time.RFC3339),
		now.Add(-2*time.Minute).UTC().Format(time.RFC3339))
	if err := os.WriteFile(piQuotaSnapshotPath(), []byte(quotaBody), 0o644); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := routerStatus(cfg, now, &out); err != nil {
		t.Fatal(err)
	}
	s = out.String()
	for _, want := range []string{
		"router.json: in sync",
		"antigravity: 88.5% left (fresh, 45s ago",
		"claude:      stale (8h ago)",
		"openai:      no reading",
		"google:      64.0% left (fresh, 2m ago)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("synced status missing %q:\n%s", want, s)
		}
	}

	// `router set` rejects invalid model without modifying config.toml.
	out.Reset()
	if err := routerSet("worker", []string{"anthropic/claude-opus-5-5:bogus"}, now, &out); err == nil {
		t.Fatal("router set accepted invalid :thinking suffix")
	}
	afterBad, _ := os.ReadFile(cfgPath)
	if string(afterBad) != initialTOML {
		t.Fatal("invalid router set modified config.toml")
	}

	// `router set worker` updates config.toml preserving comments + mode 0600, clears legacy key, and syncs router.json.
	out.Reset()
	if err := routerSet("worker", []string{
		"openai-codex/gpt-6.2-astra:xhigh",
		"anthropic/claude-opus-5-5:high",
		"antigravity/gemini-4-neon-xhigh",
	}, now, &out); err != nil {
		t.Fatalf("router set worker failed: %v", err)
	}
	if !strings.Contains(out.String(), "removed conflicting legacy [agents.pi] router_worker_model") ||
		!strings.Contains(out.String(), "synced "+piRouterConfigPath()) {
		t.Errorf("router set output = %q", out.String())
	}
	fi, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config.toml mode = %o, want 0600", fi.Mode().Perm())
	}
	afterSet, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(afterSet), "# My custom header") ||
		!strings.Contains(string(afterSet), "# Preserve this comment") ||
		!strings.Contains(string(afterSet), "[agents.pi.router_providers.antigravity]") {
		t.Errorf("router set clobbered comments/sections:\n%s", afterSet)
	}
	reloaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	wantWorkers := []string{
		"openai-codex/gpt-6.2-astra:xhigh",
		"anthropic/claude-opus-5-5:high",
		"antigravity/gemini-4-neon-xhigh",
	}
	if !slices.Equal(reloaded.Agents["pi"].RouterWorkerModels, wantWorkers) || reloaded.Agents["pi"].RouterWorkerModel != "" {
		t.Fatalf("reloaded pi config = %+v", reloaded.Agents["pi"])
	}

	// Confirm router.json was immediately synced and status reports in sync.
	out.Reset()
	if err := routerStatus(reloaded, now, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "router.json: in sync") ||
		!strings.Contains(out.String(), "openai-codex/gpt-6.2-astra:xhigh -> anthropic/claude-opus-5-5:high -> antigravity/gemini-4-neon-xhigh") {
		t.Errorf("status after set =\n%s", out.String())
	}

	// Out-of-sync detection when router.json differs from config.toml.
	if err := os.WriteFile(piRouterConfigPath(), []byte(`{"worker_models":["anthropic/claude-opus-5-5"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := routerStatus(reloaded, now, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "router.json: out of sync with config.toml") {
		t.Errorf("expected out-of-sync status, got:\n%s", out.String())
	}
}
