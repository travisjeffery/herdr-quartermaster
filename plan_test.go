package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

func mergePlan(bead string) string {
	return `{"bead": "` + bead + `", "steps": [{"op": "github.pr.merge", "target": "github:acme/app#55", "pre": {"head_sha": "` + sha + `"}}]}`
}

func mustParse(t *testing.T, s string) Plan {
	t.Helper()
	p, err := parsePlan([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustDigest(t *testing.T, p Plan) string {
	t.Helper()
	d, err := p.digest()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// copyPreview puts a gate fixture where a step can name it.
func copyPreview(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, readPlan(t, name), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPlanDigestStable(t *testing.T) {
	a := mustParse(t, mergePlan("b-1"))
	// The same plan with its keys in another order and other spacing.
	b := mustParse(t, `{"steps":[{"pre":{"head_sha":"`+sha+`"},"target":"github:acme/app#55","op":"github.pr.merge"}],"bead":"b-1"}`)
	da, db := mustDigest(t, a), mustDigest(t, b)
	if da != db {
		t.Errorf("key order changed the digest: %s vs %s", da, db)
	}
	// Pinned, so a change to the canonical form is a deliberate one: it voids
	// every stored approval.
	canon, _ := a.canonical()
	if string(canon) != `{"bead":"b-1","steps":[{"op":"github.pr.merge","pre":{"head_sha":"`+sha+`"},"target":"github:acme/app#55"}]}` {
		t.Errorf("canonical form: %s", canon)
	}
	if want := "17d872b25513168a56d45a35880fef76798d198210ad63725ba7564157342291"; da != want {
		t.Errorf("digest %s, pinned %s", da, want)
	}
	// Empty maps and missing ones are the same plan.
	c := a
	c.Steps = []Step{a.Steps[0]}
	c.Steps[0].Args = map[string]string{}
	if mustDigest(t, c) != da {
		t.Error("an empty args map changed the digest")
	}
	// A new head is a new plan.
	d := mustParse(t, strings.Replace(mergePlan("b-1"), sha, strings.Repeat("f", 40), 1))
	if mustDigest(t, d) == da {
		t.Error("a new head_sha kept the digest")
	}
	// No HTML escaping: < in text hashes as itself.
	o := mustParse(t, `{"bead":"b-1","steps":[{"op":"opaque","text":"echo <x> && y"}]}`)
	if canon, _ := o.canonical(); !strings.Contains(string(canon), "<x> && y") {
		t.Errorf("escaped: %s", canon)
	}
}

func TestPlanDigestBindsPreview(t *testing.T) {
	path := copyPreview(t, "tofu-update.json")
	plan := `{"bead":"b-1","steps":[{"op":"iac.apply","target":"tofu:org/app/staging","args":{"preview":"` + path + `"}}]}`
	p := mustParse(t, plan)
	want := sha256Hex(readPlan(t, "tofu-update.json"))
	if got := p.Steps[0].Pre["plan_digest"]; got != want {
		t.Errorf("plan_digest %q, want the preview's %q", got, want)
	}
	before := mustDigest(t, p)
	// The same path with other content is another plan.
	if err := os.WriteFile(path, readPlan(t, "pulumi-update.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if mustDigest(t, mustParse(t, plan)) == before {
		t.Error("a changed preview kept the digest")
	}
	// A plan_digest that doesn't match its preview is refused.
	stale := strings.Replace(plan, `"args"`, `"pre":{"plan_digest":"`+want+`"},"args"`, 1)
	if _, err := parsePlan([]byte(stale)); err == nil || !strings.Contains(err.Error(), "doesn't match") {
		t.Errorf("stale plan_digest: %v", err)
	}
}

func TestParsePlanRefuses(t *testing.T) {
	for _, tc := range []struct{ name, plan, want string }{
		{"not json", `{`, "not a plan"},
		{"unknown field", `{"bead":"b-1","steps":[{"op":"opaque","text":"x"}],"class":1}`, "unknown field"},
		{"no bead", `{"steps":[{"op":"opaque","text":"x"}]}`, "no bead"},
		{"no steps", `{"bead":"b-1","steps":[]}`, "no steps"},
		{"no op", `{"bead":"b-1","steps":[{}]}`, "no op"},
		{"class 10", `{"bead":"b-1","steps":[{"op":"opaque","text":"x","class":10}]}`, "0-9"},
		{"no head", `{"bead":"b-1","steps":[{"op":"github.pr.merge","target":"github:a/b#1"}]}`, "pre.head_sha"},
		{"short head", `{"bead":"b-1","steps":[{"op":"github.pr.automerge","target":"github:a/b#1","pre":{"head_sha":"abc1234"}}]}`, "40-character"},
		{"bad pr target", `{"bead":"b-1","steps":[{"op":"github.pr.merge","target":"a/b#1","pre":{"head_sha":"` + sha + `"}}]}`, "github:owner/repo#N"},
		{"bad iac target", `{"bead":"b-1","steps":[{"op":"iac.apply","target":"helm:x"}]}`, "pulumi:<stack>"},
		{"missing preview", `{"bead":"b-1","steps":[{"op":"iac.apply","target":"tofu:x","args":{"preview":"/nonexistent/p.json"}}]}`, "preview"},
		{"opaque without text", `{"bead":"b-1","steps":[{"op":"opaque"}]}`, "needs text"},
	} {
		if _, err := parsePlan([]byte(tc.plan)); !errors.Is(err, errPlan) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
	// An unknown op is not refused: it is opaque, class 9.
	if _, err := parsePlan([]byte(`{"bead":"b-1","steps":[{"op":"helm.upgrade"}]}`)); err != nil {
		t.Errorf("unknown op: %v", err)
	}
}

func TestStepClass(t *testing.T) {
	cfg := defaultConfig()
	cfg.Autonomy.Prod = []string{"*prod*"}
	cfg.Autonomy.Widespread = 5
	cfg.Autonomy.MergeClass = map[string]int{"infra": 7, "docs": 1}
	tofu, pulumi := copyPreview(t, "tofu-update.json"), copyPreview(t, "pulumi-update.json")
	replace := copyPreview(t, "pulumi-replace.json")
	apply := func(target, preview, expect string) Step {
		s := Step{Op: "iac.apply", Target: target, Args: map[string]string{"preview": preview}}
		if expect != "" {
			s.Args["expect"] = expect
		}
		return s
	}
	merge := func(op, repo string) Step {
		return Step{Op: op, Target: "github:acme/" + repo + "#5", Pre: map[string]string{"head_sha": sha}}
	}
	for _, tc := range []struct {
		name string
		s    Step
		want int
		why  string
	}{
		{"merge", merge("github.pr.merge", "app"), 4, "merge at 0123456789ab"},
		{"automerge", merge("github.pr.automerge", "app"), 4, "merge"},
		{"merge_class raises by target repo", merge("github.pr.merge", "infra"), 7, "merge"},
		{"merge_class can't lower", merge("github.pr.merge", "docs"), 4, "merge"},
		{"stated class raises", Step{Op: "github.pr.merge", Target: "github:acme/app#5", Pre: map[string]string{"head_sha": sha}, Class: 6}, 6, "raised to 6 as stated"},
		{"stated class can't lower", Step{Op: "opaque", Text: "x", Class: 2}, 9, "opaque"},
		{"apply off prod", apply("tofu:org/app/staging", tofu, "create=1,update=1"), 6, "not production, preview create 1, update 1, as expected"},
		{"apply in prod", apply("tofu:org/app/prod", tofu, "create=1,update=1"), 7, "production"},
		{"no expected counts is one class up", apply("tofu:org/app/staging", tofu, ""), 7, "no expected counts, so one class up"},
		{"apply as expected", apply("pulumi:org/app/staging", pulumi, "create=1,update=1"), 6, "staging"},
		{"apply not as expected", apply("pulumi:org/app/staging", pulumi, "create=2"), 9, "expected"},
		{"preview from the other tool", apply("pulumi:org/app/staging", tofu, ""), 9, "the preview is tofu output, the target pulumi"},
		{"stateful replace", apply("pulumi:org/app/staging", replace, "create-replacement=1,delete=1,delete-replaced=1,replace=1,update=1"), 8, "staging"},
		{"stateful replace, no expected counts", apply("pulumi:org/app/staging", replace, ""), 9, "one class up"},
		{"no preview", Step{Op: "iac.apply", Target: "tofu:org/app/staging"}, 9, "no preview"},
		{"opaque", Step{Op: "opaque", Text: "gh api -X DELETE repos/a/b"}, 9, "opaque"},
		{"unknown op", Step{Op: "helm.upgrade", Target: "x"}, 9, "unknown op"},
	} {
		got, why := cfg.stepClass(Bead{ID: "b-1"}, tc.s)
		if got != tc.want || !strings.Contains(why, tc.why) {
			t.Errorf("%s: class %d (%s), want %d (%s)", tc.name, got, why, tc.want, tc.why)
		}
	}
}

// planCLI runs plan subcommands against one bead in a fresh state dir.
type planCLI struct {
	t    *testing.T
	cfg  Config
	bead Bead
}

func newPlanCLI(t *testing.T, labels ...string) *planCLI {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	return &planCLI{t: t, cfg: defaultConfig(), bead: Bead{ID: "b-1", Status: StatusInProgress, Labels: labels}}
}

func (c *planCLI) run(by string, ttl time.Duration, args ...string) (string, error) {
	var out strings.Builder
	show := func(id string) (Bead, error) {
		if id != c.bead.ID {
			return Bead{}, errors.New("no bead " + id)
		}
		return c.bead, nil
	}
	err := planCmd(c.cfg, show, args, by, ttl, nil, &out)
	return out.String(), err
}

func (c *planCLI) file(plan string) string {
	path := filepath.Join(c.t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(plan), 0o644); err != nil {
		c.t.Fatal(err)
	}
	return path
}

func TestPlanCheckLevelBoundary(t *testing.T) {
	// A merge is class 4: level 4 approves it, 3 doesn't.
	c := newPlanCLI(t, "autonomy:4")
	out, err := c.run("worker", 0, "check", c.file(mergePlan("b-1")))
	if err != nil {
		t.Fatalf("merge at 4: %v\n%s", err, out)
	}
	if !strings.Contains(out, "class 4") || !strings.Contains(out, "approved by the dial (level 4)") {
		t.Errorf("output:\n%s", out)
	}
	c.bead.Labels = []string{"autonomy:3"}
	out, err = c.run("worker", 0, "check", c.file(mergePlan("b-1")))
	if !errors.Is(err, errNotAutonomous) || !strings.Contains(err.Error(), "class 4, above b-1's level 3") {
		t.Errorf("merge at 3: %v", err)
	}
	digest := mustDigest(t, mustParse(t, mergePlan("b-1")))
	if !strings.Contains(out, "quartermaster plan approve "+digest+" --by TJ") {
		t.Errorf("a failed check names the digest to approve:\n%s", out)
	}
	// The plan's class is its highest step's: an opaque step makes it 9,
	// which no standing level reaches.
	c.bead.Labels = []string{"autonomy:7"}
	two := `{"bead":"b-1","steps":[` + strings.TrimSuffix(strings.SplitN(mergePlan("b-1"), `"steps": [`, 2)[1], "]}") + `,{"op":"opaque","text":"kubectl delete ns x"}]}`
	if _, err := c.run("worker", 0, "check", c.file(two)); err == nil || !strings.Contains(err.Error(), "class 9") {
		t.Errorf("merge + opaque at 7: %v", err)
	}
	// A closed bead approves nothing.
	c.bead.Status = StatusClosed
	if _, err := c.run("worker", 0, "check", c.file(mergePlan("b-1"))); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("closed: %v", err)
	}
}

func TestPlanCheckJournalsAndStores(t *testing.T) {
	c := newPlanCLI(t, "autonomy:4")
	if _, err := c.run("worker", 0, "check", c.file(mergePlan("b-1"))); err != nil {
		t.Fatal(err)
	}
	digest := mustDigest(t, mustParse(t, mergePlan("b-1")))
	recs, err := readJournal(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Kind != journalCheck || recs[0].Action != "plan" || !recs[0].Pass || recs[0].Class != 4 || !strings.Contains(recs[0].Text, short(digest)) {
		t.Errorf("journal %+v", recs)
	}
	events, err := readPlanEvents(digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != planChecked || events[1].Kind != planApproved || events[1].By != "dial" {
		t.Fatalf("events %+v", events)
	}
	if ttl := events[1].Expires.Sub(events[1].At); ttl != approvalTTL {
		t.Errorf("dial approval lasts %v, want %v", ttl, approvalTTL)
	}
	// Stored under its digest, and found again by a prefix.
	p, d, err := loadPlan(digest[:10])
	if err != nil || d != digest || p.Bead != "b-1" {
		t.Errorf("loadPlan: %v %s %+v", err, d, p)
	}
	// A plan file edited after it was stored no longer loads.
	path := filepath.Join(planDir(digest), "plan.json")
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), "#55", "#56", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadPlan(digest); err == nil || !strings.Contains(err.Error(), "doesn't hash") {
		t.Errorf("tampered plan: %v", err)
	}
}

func TestPlanCheckBudget(t *testing.T) {
	c := newPlanCLI(t, "autonomy:4")
	c.cfg.Autonomy.Budget.Steps = 2
	for range 2 {
		if err := appendJournal(journalRecord{At: time.Now(), Kind: journalAuto, Bead: "b-1", Class: 2}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.run("worker", 0, "check", c.file(mergePlan("b-1"))); !errors.Is(err, errNotAutonomous) || !strings.Contains(err.Error(), "budget") {
		t.Errorf("over budget: %v", err)
	}
}

func TestPlanApproveAndStatus(t *testing.T) {
	c := newPlanCLI(t, "autonomy:3")
	digest := mustDigest(t, mustParse(t, mergePlan("b-1")))
	// Approving needs a checked plan.
	if _, err := c.run("TJ", 0, "approve", digest); err == nil || !strings.Contains(err.Error(), "plan check first") {
		t.Errorf("unchecked: %v", err)
	}
	if _, err := c.run("worker", 0, "check", c.file(mergePlan("b-1"))); err == nil {
		t.Fatal("merge passed at 3")
	}
	out, err := c.run("worker", 0, "status", digest[:12])
	if err != nil || !strings.Contains(out, "waiting for TJ") || !strings.Contains(out, "1. github.pr.merge github:acme/app#55: class 4") {
		t.Errorf("status before approval: %v\n%s", err, out)
	}
	// Only TJ approves.
	for _, by := range []string{"coordinator", "worker"} {
		if _, err := c.run(by, 0, "approve", digest); !errors.Is(err, errNotAutonomous) {
			t.Errorf("approve by %s: %v", by, err)
		}
	}
	if _, err := c.run("TJ", 0, "approve", "abc"); err == nil || !strings.Contains(err.Error(), "8 characters") {
		t.Errorf("short ref: %v", err)
	}
	out, err = c.run("TJ", 30*time.Minute, "approve", digest[:8])
	if err != nil || !strings.Contains(out, "approved by TJ") {
		t.Fatalf("approve: %v %s", err, out)
	}
	out, err = c.run("worker", 0, "status", digest)
	if err != nil || !strings.Contains(out, "approved by TJ until") {
		t.Errorf("status after approval: %v\n%s", err, out)
	}
	// The approval expires.
	events, _ := readPlanEvents(digest)
	a, ok := liveApproval(events, time.Now())
	if !ok || a.Expires.Sub(a.At) != 30*time.Minute {
		t.Fatalf("live approval %+v %v", a, ok)
	}
	if _, ok := liveApproval(events, time.Now().Add(31*time.Minute)); ok {
		t.Error("an approval outlived --for")
	}
	if s := planState(events, digest, time.Now().Add(2*time.Hour)); !strings.HasPrefix(s, "expired") {
		t.Errorf("state after expiry: %s", s)
	}
	if s := planState(nil, digest, time.Now()); !strings.HasPrefix(s, "not checked") {
		t.Errorf("no events: %s", s)
	}
}
