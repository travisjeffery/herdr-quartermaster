package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
	// Explicit ordered chains override defaults.
	cfg.Agents = map[string]AgentConfig{"pi": {
		RouterWorkerModels:      []string{"antigravity/gemini-4-neon-xhigh", "anthropic/claude-opus-5-5"},
		RouterCoordinatorModels: []string{"anthropic/claude-fable-5-2", "openai-codex/gpt-6.2-astra"},
	}}
	if _, err := cfg.writePiRouterConfig(); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if err := json.Unmarshal(b, &got); err != nil ||
		strings.Join(got.WorkerModels, ",") != "antigravity/gemini-4-neon-xhigh,anthropic/claude-opus-5-5" ||
		strings.Join(got.CoordinatorModels, ",") != "anthropic/claude-fable-5-2,openai-codex/gpt-6.2-astra" {
		t.Errorf("explicit chain router config = %+v, %v", got, err)
	}
}

func TestValidatePiRouter(t *testing.T) {
	cfg := defaultConfig()
	if err := cfg.validateAgents(); err != nil {
		t.Fatal(err)
	}
	lo, hi, over := 10.0, 25.0, 101.0
	cfg.Agents = map[string]AgentConfig{"pi": {
		RouterWorkerModels:      []string{piAntigravityModel, piWorkerModel},
		RouterCoordinatorModels: []string{piAntigravityModel, piCoordinatorModel},
		RouterThreshold:         &lo,
		RouterRecover:           &hi,
	}}
	if err := cfg.validateAgents(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]map[string]AgentConfig{
		"recover below threshold":       {"pi": {RouterThreshold: &hi, RouterRecover: &lo}},
		"percent over 100":              {"pi": {RouterThreshold: &over}},
		"model without provider":        {"pi": {RouterWorkerModel: "opus"}},
		"virtual as physical":           {"pi": {RouterFallbackModel: piWorkerVirtualModel}},
		"router on another kind":        {"claude": {RouterThreshold: &lo}},
		"empty worker chain":            {"pi": {RouterWorkerModels: []string{}}},
		"empty coordinator chain":       {"pi": {RouterCoordinatorModels: []string{}}},
		"chain entry without provider":  {"pi": {RouterWorkerModels: []string{"bad"}}},
		"virtual model in chain":        {"pi": {RouterCoordinatorModels: []string{piCoordinatorVirtualModel}}},
		"both worker chain and single":  {"pi": {RouterWorkerModels: []string{piAntigravityModel}, RouterWorkerModel: piWorkerModel}},
		"both coord chain and single":   {"pi": {RouterCoordinatorModels: []string{piAntigravityModel}, RouterCoordinatorModel: piCoordinatorModel}},
		"chain on another kind":         {"claude": {RouterWorkerModels: []string{piAntigravityModel}}},
	} {
		cfg.Agents = bad
		if err := cfg.validateAgents(); err == nil {
			t.Errorf("%s passed", name)
		}
	}
}
