package main

import (
	"maps"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestContextStaleNeedsAgeAndNoPR(t *testing.T) {
	now := t0
	fresh := Bead{ID: "b-fresh", Status: StatusInProgress, UpdatedAt: now.Add(-2 * time.Hour)}
	old := Bead{ID: "b-old", Status: StatusInProgress, UpdatedAt: now.Add(-10 * 24 * time.Hour)}
	oldPR := Bead{ID: "b-oldpr", Status: StatusInProgress, UpdatedAt: now.Add(-10 * 24 * time.Hour)}
	threads := []Thread{{Bead: fresh}, {Bead: old}, {Bead: oldPR}}
	st := TickerState{PRs: map[string]PR{"b-oldpr": {Number: 9, State: "MERGED"}}}
	out := renderContext(Config{Repo: "/r"}, threads, nil, nil, nil, st, map[string]bool{}, now)
	if !strings.Contains(out, "- stale claims (1): b-old ") {
		t.Fatalf("only the old bead without a PR is stale:\n%s", out)
	}
	if !strings.Contains(out, "- claimed, no agent (2): b-fresh, b-oldpr") {
		t.Fatalf("fresh and PR-carrying claims are not stale:\n%s", out)
	}
}

func TestLinearTitleCheckWaitsForTitle(t *testing.T) {
	th := Thread{Bead: Bead{ID: "b1", Status: StatusInProgress}, Linear: "ENG-1", PR: &PR{Number: 3, State: "OPEN"}}
	for _, m := range linearMoves([]Thread{th}) {
		if strings.Contains(m, "title lacks") {
			t.Fatalf("flagged a PR whose title is not fetched yet: %v", m)
		}
	}
}

func TestCoordinatorGuideAppendsConfiguredRules(t *testing.T) {
	base := coordinatorGuide(Config{Repo: "/r"})
	got := coordinatorGuide(Config{Repo: "/r", CoordinatorRules: []string{"Rule one.", "  ", "Rule two."}})
	if want := base + "- Rule one.\n- Rule two.\n"; got != want {
		t.Errorf("guide tail:\n%s", got[len(got)-200:])
	}
}

func TestContextShowsDial(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := Config{Repo: "/r", Autonomy: AutonomyConfig{Level: 4, Max: 7}}
	if err := saveAutonomy(autonomyState{Leases: map[string]autonomyLease{"b-1": {Level: 8, Until: t0.Add(time.Hour)}}}); err != nil {
		t.Fatal(err)
	}
	out := renderContext(cfg, nil, nil, nil, nil, TickerState{}, nil, t0)
	for _, want := range []string{
		"## Autonomy",
		"level: 4 (global, from [autonomy] level), max 7",
		"lease: b-1 at 8 until",
		"reporting: one line per update listing the AUTO: steps since the last",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("context missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "## Autonomy") > strings.Index(out, "## Inbox") {
		t.Error("the dial is not at the top")
	}
}

func TestReportingMode(t *testing.T) {
	for g, want := range map[int]string{0: "each AUTO: step", 3: "each AUTO: step", 4: "one line per update", 6: "one line per update", 7: "between digests", 9: "between digests"} {
		if got := reportingMode(g); !strings.Contains(got, want) {
			t.Errorf("level %d: %q", g, got)
		}
	}
}

func TestCoordinatorGuidePointsAtCheck(t *testing.T) {
	g := coordinatorGuide(Config{Repo: "/r"})
	for _, want := range []string{
		"quartermaster autonomy check <bead> --action A [--target T]",
		"4 main\n  (merging an approved, green PR) and spending up to $500 (one charge, or per month for a recurring cost)",
		"5 internal messages and configured dev targets (`[autonomy] dev`, when\n  `prod` is set and does not match)",
		"7 production additive or in place and spending more, or an amount\n  no one can estimate (`--action spend --amount <USD>[/month]`)",
		"AUTO: [coordinator L<n> C<c>]",
		"never lower than the worker's `CLASS:`",
		"a question of taste or voice, involves other people",
		"it never widens a task or\n  authorizes an action, at any level",
		"A classifier denial (a prompt counting down to an\n    automatic deny) is class 9",
		"--action merge` passes; otherwise tell TJ",
	} {
		if !strings.Contains(g, want) {
			t.Errorf("guide missing %q", want)
		}
	}
	for _, gone := range []string{"outside messages, money or approving a PR", "wait for TJ's go-ahead", "only on TJ's go-ahead", "autonomy grant|revoke", "never answer it for TJ"} {
		if strings.Contains(g, gone) {
			t.Errorf("guide still says %q", gone)
		}
	}
}

func TestCoordinatorGuideAppConventionsAndStatusBlock(t *testing.T) {
	g := coordinatorGuide(Config{Repo: "/r"})

	for _, verb := range []string{"MERGE", "DISPATCH", "CLOSE", "HOLD", "RESUME", "STATUS", "STOP", "ANSWER"} {
		if !strings.Contains(g, "| `"+verb+"` |") {
			t.Errorf("guide verb table missing %s", verb)
		}
	}
	for _, want := range []string{
		"Explicit target",
		"Bare verbs resolve narrowly",
		"Bare-verb echo at class ≥ 4",
		"`STATUS` with no target",
		"quartermaster context --compact",
		"[req:<id>] <VERB> <target> — <one-line why>",
		"[a-z0-9-]{1,64}",
		"answered: <bead> option <n>",
		"block   := \"```qm-status\" NL field+ \"```\"",
		"field   := key \"=\" value NL",
		"value   := [^ \\t\\r\\n]+",
		"`done` · `blocked` · `needs_tj`",
	} {
		if !strings.Contains(g, want) {
			t.Errorf("guide missing %q", want)
		}
	}
	for _, key := range []string{"req", "state", "bead", "bead_status", "pr", "pr_state", "checks", "head", "class", "job"} {
		if !strings.Contains(g, "| `"+key+"` |") {
			t.Errorf("guide status-block table missing key %q", key)
		}
	}

	// Match the exact regex from docs/design/coordinator-api.md.
	re := regexp.MustCompile("(?s)```qm-status\n(.*?)```")
	matches := re.FindAllStringSubmatch(g, -1)
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 fenced qm-status example block, found %d", len(matches))
	}
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(matches[0][1], "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" || v == "" {
			t.Fatalf("malformed status-block line %q", line)
		}
		if strings.ContainsAny(k+v, " \t\r\n") {
			t.Fatalf("status-block line %q violates single-token grammar", line)
		}
		fields[k] = v
	}
	want := map[string]string{
		"req":         "app-20261123-0017",
		"state":       "done",
		"bead":        "b-123",
		"bead_status": "in_progress",
		"class":       "2",
	}
	if !maps.Equal(fields, want) {
		t.Fatalf("parsed status block = %v, want %v", fields, want)
	}
	if !strings.Contains(g, "[req:"+want["req"]+"] Answered with option 2; "+want["bead"]) {
		t.Errorf("worked example first line does not echo [req:%s] and bead %s", want["req"], want["bead"])
	}
}
