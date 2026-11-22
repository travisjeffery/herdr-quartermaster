package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// A reading older than this may miss usage from other machines or the web
// apps, which draw on the same subscription.
const quotaMaxAge = 6 * time.Hour

type quotaWindow struct {
	Used     float64 // percent
	ResetsAt time.Time
}

type quotaReading struct {
	At      time.Time
	Windows []quotaWindow
}

// headroom is the percent left in the tightest window, or false when there is
// no reading recent enough to trust.
func headroom(r *quotaReading, now time.Time) (float64, bool) {
	left, _, ok := headroomDetail(r, now)
	return left, ok
}

// headroomDetail also reports when the tightest live window resets (zero
// when every window has reset), which the pi quota router needs for its
// switch-back rule.
func headroomDetail(r *quotaReading, now time.Time) (float64, time.Time, bool) {
	if r == nil || len(r.Windows) == 0 || now.Sub(r.At) > quotaMaxAge {
		return 0, time.Time{}, false
	}
	left := 100.0
	var resets time.Time
	for _, w := range r.Windows {
		if w.ResetsAt.After(now) && 100-w.Used < left {
			left, resets = 100-w.Used, w.ResetsAt
		}
	}
	return left, resets, true
}

// pickAgent is worker_agent = "auto": codex only when both readings are
// usable and codex has strictly more left. It never picks muse, pi or agy,
// which leave no usage reading to compare.
func pickAgent(claude, codex *quotaReading, now time.Time) string {
	cl, ok1 := headroom(claude, now)
	cx, ok2 := headroom(codex, now)
	if ok1 && ok2 && cx > cl {
		return "codex"
	}
	return "claude"
}

func autoAgent(now time.Time) string {
	return pickAgent(claudeQuota(), codexQuota(now), now)
}

// quotaSnapshot is the normalized headroom quartermaster writes for its pi
// extension's virtual-model router (pi/router.ts), so the Claude and Codex
// readings are parsed once, here, with the same freshness semantics as
// worker_agent=auto. A provider is null when it has no reading at all; a
// reading too old to trust is present with fresh false.
type quotaSnapshot struct {
	GeneratedAt time.Time              `json:"generated_at"`
	Claude      *quotaSnapshotProvider `json:"claude"`
	OpenAI      *quotaSnapshotProvider `json:"openai"`
}

type quotaSnapshotProvider struct {
	HeadroomPct   *float64   `json:"headroom_pct"`
	Fresh         bool       `json:"fresh"`
	At            time.Time  `json:"at"`
	TightResetsAt *time.Time `json:"tight_resets_at"`
}

func snapshotProvider(r *quotaReading, now time.Time) *quotaSnapshotProvider {
	if r == nil || len(r.Windows) == 0 {
		return nil
	}
	p := &quotaSnapshotProvider{At: r.At.UTC()}
	if left, resets, ok := headroomDetail(r, now); ok {
		p.HeadroomPct, p.Fresh = &left, true
		if !resets.IsZero() {
			u := resets.UTC()
			p.TightResetsAt = &u
		}
	}
	return p
}

func buildQuotaSnapshot(now time.Time) quotaSnapshot {
	return quotaSnapshot{
		GeneratedAt: now.UTC(),
		Claude:      snapshotProvider(claudeQuota(), now),
		OpenAI:      snapshotProvider(codexQuota(now), now),
	}
}

// piQuotaSnapshotPath is quota.json beside the pi extension files, which the
// router reads through the --quartermaster-router flag's directory.
func piQuotaSnapshotPath() string { return filepath.Join(stateDir(), "pi", "quota.json") }

// writeQuotaSnapshot atomically writes the current snapshot, skipping the
// write when only generated_at would change. Written on every local pi
// launch and refreshed by the ticker, so a long-lived session routes on
// fresh readings at each turn boundary.
func writeQuotaSnapshot(now time.Time) error {
	snap := buildQuotaSnapshot(now)
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	path := piQuotaSnapshotPath()
	if cur, err := os.ReadFile(path); err == nil {
		var old quotaSnapshot
		if json.Unmarshal(cur, &old) == nil {
			old.GeneratedAt, snap.GeneratedAt = time.Time{}, time.Time{}
			if ob, err := json.Marshal(old); err == nil {
				if nb, err := json.Marshal(snap); err == nil && bytes.Equal(ob, nb) {
					return nil
				}
			}
		}
	}
	return writeAtomic(path, b)
}

// claudeQuota reads the rate limits Claude Code hands its status line, which
// the herdr-agent-quota status line command saves after every refresh.
func claudeQuota() *quotaReading {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".local", "state", "herdr", "plugins", "herdr-agent-quota", "claude-statusline.observation.json")
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseClaudeStatusline(data, fi.ModTime())
}

func parseClaudeStatusline(data []byte, at time.Time) *quotaReading {
	var obs struct {
		Payload struct {
			RateLimits map[string]*struct {
				UsedPercentage float64 `json:"used_percentage"`
				ResetsAt       int64   `json:"resets_at"`
			} `json:"rate_limits"`
		} `json:"payload"`
	}
	if json.Unmarshal(data, &obs) != nil {
		return nil
	}
	r := &quotaReading{At: at}
	for _, w := range obs.Payload.RateLimits {
		if w != nil {
			r.Windows = append(r.Windows, quotaWindow{Used: w.UsedPercentage, ResetsAt: time.Unix(w.ResetsAt, 0)})
		}
	}
	return r
}

// codexQuota reads the rate limits Codex records with each turn in its latest
// session rollout.
func codexQuota(now time.Time) *quotaReading {
	dir := os.Getenv("CODEX_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".codex")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "sessions", "*", "*", "*", "rollout-*.jsonl"))
	var latest string
	var mod time.Time
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil && fi.ModTime().After(mod) {
			latest, mod = f, fi.ModTime()
		}
	}
	if latest == "" || now.Sub(mod) > quotaMaxAge {
		return nil
	}
	data, err := os.ReadFile(latest)
	if err != nil {
		return nil
	}
	return parseCodexRollout(data)
}

func parseCodexRollout(data []byte) *quotaReading {
	lines := bytes.Split(data, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if !bytes.Contains(lines[i], []byte(`"rate_limits"`)) {
			continue
		}
		type window struct {
			UsedPercent float64 `json:"used_percent"`
			ResetsAt    int64   `json:"resets_at"`
		}
		var ev struct {
			Timestamp time.Time `json:"timestamp"`
			Payload   struct {
				Type       string `json:"type"`
				RateLimits *struct {
					Primary   *window `json:"primary"`
					Secondary *window `json:"secondary"`
				} `json:"rate_limits"`
			} `json:"payload"`
		}
		if json.Unmarshal(lines[i], &ev) != nil || ev.Payload.Type != "token_count" || ev.Payload.RateLimits == nil {
			continue
		}
		r := &quotaReading{At: ev.Timestamp}
		for _, w := range []*window{ev.Payload.RateLimits.Primary, ev.Payload.RateLimits.Secondary} {
			if w != nil {
				r.Windows = append(r.Windows, quotaWindow{Used: w.UsedPercent, ResetsAt: time.Unix(w.ResetsAt, 0)})
			}
		}
		return r
	}
	return nil
}
