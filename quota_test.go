package main

import (
	"os"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseClaudeStatusline(t *testing.T) {
	at := time.Unix(1790346889, 0)
	r := parseClaudeStatusline(fixture(t, "claude-statusline.observation.json"), at)
	left, ok := headroom(r, at)
	if !ok || left != 66 {
		t.Fatalf("want 66%% left in the tightest (weekly) window, got %v %v", left, ok)
	}
}

func TestParseAgyStatusline(t *testing.T) {
	fallbackAt := time.Unix(1795470000, 0)
	r := parseAgyStatusline(fixture(t, "agy-statusline.observation.json"), fallbackAt)
	if r == nil || !r.At.Equal(time.Unix(1795474884, 0)) {
		t.Fatalf("want fetched_at_unix timestamp, got %+v", r)
	}
	left, resets, ok := headroomDetail(r, r.At)
	if !ok || left < 94.07 || left > 94.09 {
		t.Fatalf("want ~94.08%% left in the tightest (five_hour) window, got %v %v", left, ok)
	}
	if !resets.Equal(time.Unix(1795485226, 0)) {
		t.Fatalf("want five_hour resets_at, got %v", resets)
	}
	// Falls back to remaining_percent and file modtime when used_percent and fetched_at_unix are absent.
	remOnly := []byte(`{"snapshot":{"windows":[{"kind":"five_hour","remaining_percent":12.5,"resets_at":1795485226}]}}`)
	r2 := parseAgyStatusline(remOnly, fallbackAt)
	if r2 == nil || !r2.At.Equal(fallbackAt) {
		t.Fatalf("fallback modtime: %+v", r2)
	}
	if left, ok := headroom(r2, fallbackAt); !ok || left != 12.5 {
		t.Fatalf("remaining_percent fallback: got %v %v, want 12.5", left, ok)
	}
	if parseAgyStatusline([]byte(`{not json`), fallbackAt) != nil {
		t.Fatal("malformed observation should return nil")
	}
	if r3 := parseAgyStatusline([]byte(`{"snapshot":{"windows":[]}}`), fallbackAt); r3 == nil || len(r3.Windows) != 0 {
		t.Fatalf("empty windows: %+v", r3)
	}
}

func TestParseCodexRolloutTakesTheLatestLimits(t *testing.T) {
	r := parseCodexRollout(fixture(t, "codex-rollout.jsonl"))
	if r == nil || !r.At.Equal(time.Date(2026, 9, 25, 12, 2, 0, 0, time.UTC)) {
		t.Fatalf("want the 12:02 reading, skipping the null one after it: %+v", r)
	}
	if left, ok := headroom(r, r.At); !ok || left != 86 {
		t.Fatalf("got %v %v", left, ok)
	}
	if parseCodexRollout([]byte(`{"type":"session_meta"}`)) != nil {
		t.Fatal("a rollout without limits is no reading")
	}
}

func TestHeadroomCountsAResetWindowAsFull(t *testing.T) {
	now := time.Unix(1790350000, 0)
	r := &quotaReading{At: now.Add(-time.Hour), Windows: []quotaWindow{
		{Used: 95, ResetsAt: now.Add(-time.Minute)},
		{Used: 30, ResetsAt: now.Add(24 * time.Hour)},
	}}
	if left, ok := headroom(r, now); !ok || left != 70 {
		t.Fatalf("got %v %v", left, ok)
	}
}

func TestPickAgent(t *testing.T) {
	now := time.Unix(1790350000, 0)
	reading := func(used float64, age time.Duration) *quotaReading {
		return &quotaReading{At: now.Add(-age), Windows: []quotaWindow{{Used: used, ResetsAt: now.Add(time.Hour)}}}
	}
	cases := []struct {
		name          string
		claude, codex *quotaReading
		want          string
	}{
		{"codex has more left", reading(80, time.Minute), reading(10, time.Minute), "codex"},
		{"claude has more left", reading(10, time.Minute), reading(80, time.Minute), "claude"},
		{"a tie stays on claude", reading(50, time.Minute), reading(50, time.Minute), "claude"},
		{"no codex reading", reading(99, time.Minute), nil, "claude"},
		{"no claude reading", nil, reading(0, time.Minute), "claude"},
		{"stale codex reading", reading(99, time.Minute), reading(0, 7*time.Hour), "claude"},
		{"stale claude reading", reading(99, 7*time.Hour), reading(0, time.Minute), "claude"},
		{"reading with no windows", reading(99, time.Minute), &quotaReading{At: now}, "claude"},
	}
	for _, c := range cases {
		if got := pickAgent(c.claude, c.codex, now); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}
