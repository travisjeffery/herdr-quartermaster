package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDispatchAutonomyClassGatingForRolloutTargets(t *testing.T) {
	dir := fakeRoleTools(t)
	cfg := hostsConfig()
	cfg.Autonomy.Prod = []string{"*prod*"}
	cfg.Autonomy.Dev = []string{"dev*", "latest"}
	cfg.Autonomy.Max = 7

	cases := []struct {
		name      string
		beadJSON  string
		role      string
		level     int
		by        string
		wantAllow bool
		wantErr   string
	}{
		{
			name:      "dev target class 5 blocked at L4",
			beadJSON:  `[{"id":"roll-dev-4","title":"Rollout to dev","status":"open","labels":["role:rollout","rollout-target:dev"]}]`,
			level:     4,
			by:        "coordinator",
			wantAllow: false,
			wantErr:   "class 5",
		},
		{
			name:      "dev target class 5 allowed at L5",
			beadJSON:  `[{"id":"roll-dev-5","title":"Rollout to dev","status":"open","labels":["role:rollout","rollout-target:dev"]}]`,
			level:     5,
			by:        "coordinator",
			wantAllow: true,
		},
		{
			name:      "latest dev target class 5 allowed at L5",
			beadJSON:  `[{"id":"roll-latest-5","title":"Rollout to latest","status":"open","labels":["role:rollout","rollout-target:latest"]}]`,
			level:     5,
			by:        "coordinator",
			wantAllow: true,
		},
		{
			name:      "staging target class 6 blocked at L5",
			beadJSON:  `[{"id":"roll-stg-5","title":"Rollout to staging","status":"open","labels":["role:rollout","rollout-target:staging"]}]`,
			level:     5,
			by:        "coordinator",
			wantAllow: false,
			wantErr:   "class 6",
		},
		{
			name:      "staging target class 6 allowed at L6",
			beadJSON:  `[{"id":"roll-stg-6","title":"Rollout to staging","status":"open","labels":["role:rollout","rollout-target:staging"]}]`,
			level:     6,
			by:        "coordinator",
			wantAllow: true,
		},
		{
			name:      "prod target class 7 blocked at L6",
			beadJSON:  `[{"id":"roll-prod-6","title":"Rollout to prod","status":"open","labels":["role:rollout","rollout-target:prod"]}]`,
			level:     6,
			by:        "coordinator",
			wantAllow: false,
			wantErr:   "class 7",
		},
		{
			name:      "prod target class 7 allowed by TJ",
			beadJSON:  `[{"id":"roll-prod-tj","title":"Rollout to prod","status":"open","labels":["role:rollout","rollout-target:prod"]}]`,
			level:     4,
			by:        "TJ",
			wantAllow: true,
		},
		{
			name:      "untargeted rollout remains class 3 at L4",
			beadJSON:  `[{"id":"roll-plain","title":"Rollout check","status":"open","labels":["role:rollout"]}]`,
			level:     4,
			by:        "coordinator",
			wantAllow: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(filepath.Join(dir, "bd.log"))
			_ = os.Remove(filepath.Join(dir, "herdr.log"))
			c := cfg
			c.Autonomy.Level = tc.level
			t.Setenv("FAKE_BEAD", tc.beadJSON)
			var raw []Bead
			if err := json.Unmarshal([]byte(tc.beadJSON), &raw); err != nil {
				t.Fatal(err)
			}
			msg, err := dispatch(c, newHerdr(), raw[0].ID, DispatchOpts{Role: tc.role, By: tc.by})
			if tc.wantAllow {
				if err != nil {
					t.Fatalf("expected dispatch allowed, got err=%v", err)
				}
				if !strings.Contains(msg, "role rollout") {
					t.Errorf("msg = %q, want role rollout", msg)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got err=%v msg=%q", tc.wantErr, err, msg)
			}
			if hasLine(logLines(t, dir, "bd"), "update "+raw[0].ID+" --claim") {
				t.Error("denied rollout dispatch should not claim bead")
			}
		})
	}
}

func TestDispatchWithRolloutTargetFlagAndIdempotency(t *testing.T) {
	dir := fakeRoleTools(t)
	// Extend fake bd to handle `create` returning JSON and `list` returning FAKE_BEADS_LIST.
	bdScript := `#!/bin/sh
echo "$*" >> "$LOG/bd.log"
case "$1" in
show) echo "$FAKE_BEAD" ;;
list) echo "${FAKE_BEADS_LIST:-[]}" ;;
create) echo '{"id":"roll-99","title":"Rollout Ship cache to dev","status":"open","labels":["role:rollout","rollout-target:dev"]}' ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte(bdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := hostsConfig()
	cfg.Autonomy.Level = 5
	cfg.Autonomy.Prod = []string{"*prod*"}
	cfg.Autonomy.Dev = []string{"dev*", "latest"}

	// Fail closed if --rollout-target is invalid or combined with a non-implementer role.
	t.Setenv("FAKE_BEAD", `[{"id":"impl-1","title":"Ship cache","status":"open"}]`)
	if _, err := dispatch(cfg, newHerdr(), "impl-1", DispatchOpts{RolloutTarget: "bad target"}); err == nil || !strings.Contains(err.Error(), "invalid rollout target") {
		t.Fatalf("expected invalid rollout target error, got %v", err)
	}
	if _, err := dispatch(cfg, newHerdr(), "impl-1", DispatchOpts{Role: roleReviewer, RolloutTarget: "dev"}); err == nil || !strings.Contains(err.Error(), "implementer dispatches") {
		t.Fatalf("expected non-implementer --rollout-target error, got %v", err)
	}

	// Happy path: dispatch implementer with --rollout-target dev creates sibling rollout bead blocked on impl-1.
	t.Setenv("FAKE_BEAD", `[{"id":"impl-1","title":"Ship cache","status":"open","priority":2,"parent":"epic-7","labels":["repo:infra"]}]`)
	t.Setenv("FAKE_BEADS_LIST", `[]`)
	msg, err := dispatch(cfg, newHerdr(), "impl-1", DispatchOpts{RolloutTarget: "dev", By: "coordinator"})
	if err != nil {
		t.Fatalf("dispatch --rollout-target dev: %v", err)
	}
	if !strings.Contains(msg, "chained rollout bead roll-99 (target dev, class 5) blocked on impl-1") {
		t.Fatalf("dispatch msg missing chained rollout summary: %q", msg)
	}
	fullBDLog := strings.Join(logLines(t, dir, "bd"), "\n")
	if !strings.Contains(fullBDLog, "create Rollout Ship cache to dev") {
		t.Fatalf("expected bd create call in log:\n%s", fullBDLog)
	}
	for _, want := range []string{"--parent epic-7", "role:rollout,rollout-target:dev,repo:infra", "--deps blocked-by:impl-1"} {
		if !strings.Contains(fullBDLog, want) {
			t.Errorf("bd create log missing %q:\n%s", want, fullBDLog)
		}
	}
	if strings.Contains(fullBDLog, "--parent impl-1") {
		t.Errorf("rollout bead must not be parented under impl-1:\n%s", fullBDLog)
	}
}

func TestRolloutChainCLIIdempotentAndStatus(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := defaultConfig()
	cfg.Autonomy.Level = 5
	cfg.Autonomy.Prod = []string{"*prod*"}
	cfg.Autonomy.Dev = []string{"dev*", "latest"}

	beads := []Bead{
		{ID: "impl-10", Title: "Add queue worker", Status: StatusInProgress, Priority: 1, Parent: "epic-2"},
	}
	var created []BeadCreateOpts
	var notes []string
	env := rolloutChainEnv{
		show: func(id string) (Bead, error) {
			for _, b := range beads {
				if b.ID == id {
					return b, nil
				}
			}
			return Bead{}, errBeadNotFound
		},
		nonClosed: func() ([]Bead, error) { return slices.Clone(beads), nil },
		createBead: func(opts BeadCreateOpts) (Bead, error) {
			created = append(created, opts)
			b := Bead{
				ID:           "roll-10",
				Title:        opts.Title,
				Status:       StatusOpen,
				Priority:     opts.Priority,
				Parent:       opts.Parent,
				Labels:       opts.Labels,
				Description:  opts.Description,
				Dependencies: []Dependency{{DependsOn: opts.BlockedBy[0], Type: "blocks"}},
			}
			beads = append(beads, b)
			return b, nil
		},
		note: func(id, text string) error {
			notes = append(notes, id+": "+text)
			return nil
		},
		enforce: func(b Bead, action, by string) (string, error) {
			return allow(b.ID, by, action, classHousekeeping, 5, "")
		},
	}

	// First chain creates roll-10.
	var buf bytes.Buffer
	if err := rolloutCmd(cfg, env, []string{"chain", "impl-10", "--target", "dev", "--json"}, &buf); err != nil {
		t.Fatalf("rollout chain: %v", err)
	}
	var res1 RolloutChainResult
	if err := json.Unmarshal(buf.Bytes(), &res1); err != nil {
		t.Fatalf("unmarshal chain JSON: %v", err)
	}
	if !res1.Created || res1.RolloutBead != "roll-10" || res1.TargetClass != 5 {
		t.Fatalf("unexpected first chain result: %+v", res1)
	}
	if len(created) != 1 || created[0].Parent != "epic-2" {
		t.Fatalf("unexpected created opts: %+v", created)
	}

	// Second chain for same (impl-10, dev) reuses roll-10 without creating a duplicate.
	buf.Reset()
	if err := rolloutCmd(cfg, env, []string{"chain", "impl-10", "--target", "dev", "--json"}, &buf); err != nil {
		t.Fatalf("second rollout chain: %v", err)
	}
	var res2 RolloutChainResult
	if err := json.Unmarshal(buf.Bytes(), &res2); err != nil {
		t.Fatalf("unmarshal second chain JSON: %v", err)
	}
	if res2.Created || res2.RolloutBead != "roll-10" || len(created) != 1 {
		t.Fatalf("second chain should reuse roll-10: res=%+v created=%d", res2, len(created))
	}

	// Status reports target class, level, auto_allowed, and blocker.
	buf.Reset()
	if err := rolloutCmd(cfg, env, []string{"status", "impl-10", "--json"}, &buf); err != nil {
		t.Fatalf("rollout status --json: %v", err)
	}
	var rows []rolloutChainStatusRow
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("unexpected rollout status JSON: %v\n%s", err, buf.String())
	}
	if rows[0].Bead != "roll-10" || rows[0].Target != "dev" || rows[0].TargetClass != 5 || !rows[0].AutoAllowed || !slices.Equal(rows[0].BlockedBy, []string{"impl-10"}) {
		t.Fatalf("unexpected status row: %+v", rows[0])
	}
}
