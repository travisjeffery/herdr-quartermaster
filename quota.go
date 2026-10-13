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
	if r == nil || len(r.Windows) == 0 || now.Sub(r.At) > quotaMaxAge {
		return 0, false
	}
	left := 100.0
	for _, w := range r.Windows {
		if w.ResetsAt.After(now) {
			left = min(left, 100-w.Used)
		}
	}
	return left, true
}

// pickAgent is worker_agent = "auto": codex only when both readings are
// usable and codex has strictly more left. It never picks muse, which leaves
// no usage reading to compare.
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
