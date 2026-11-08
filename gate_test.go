package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

func readPlan(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/plans/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCheckPlanPasses(t *testing.T) {
	for _, name := range []string{"pulumi-update.json", "tofu-update.json"} {
		c, err := checkPlan(readPlan(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !c.ok() {
			t.Errorf("%s blocked:\n%s", name, renderPlanCheck(c))
		}
		if c.Counts["create"] != 1 || c.Counts["update"] != 1 {
			t.Errorf("%s counts %v", name, c.Counts)
		}
	}
}

func TestCheckPlanPulumiBlocks(t *testing.T) {
	c, err := checkPlan(readPlan(t, "pulumi-replace.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, b := range c.Blocked {
		ops = append(ops, b.Op)
	}
	if want := []string{"replace", "create-replacement", "delete-replaced", "delete"}; !slices.Equal(ops, want) {
		t.Errorf("blocked %q, want %q", ops, want)
	}
	if b := c.Blocked[0]; b.Type != "aws:rds/instance:Instance" || b.Name != "main" || !b.Stateful {
		t.Errorf("replace %+v", b)
	}
	if b := c.Blocked[3]; b.Stateful {
		t.Errorf("an SNS topic is not stateful: %+v", b)
	}
	out := renderPlanCheck(c)
	if !strings.Contains(out, "BLOCKED") || !strings.Contains(out, "- replace aws:rds/instance:Instance main (stateful)") {
		t.Errorf("render:\n%s", out)
	}
}

func TestCheckPlanTofuBlocks(t *testing.T) {
	c, err := checkPlan(readPlan(t, "tofu-destroy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, b := range c.Blocked {
		ops = append(ops, b.Op)
	}
	// forget only drops a resource from state, but an operation the gate does
	// not allow fails like a destroy.
	if want := []string{"delete", "replace", "replace", "forget"}; !slices.Equal(ops, want) {
		t.Errorf("blocked %q, want %q", ops, want)
	}
}

func TestCheckPlanFailsClosed(t *testing.T) {
	for name, data := range map[string]string{
		"not json":        "Previewing update (prod)...",
		"other json":      `{"hello": "world"}`,
		"unknown op":      `{"steps": [{"op": "frobnicate", "urn": "urn:pulumi:s::p::aws:s3/bucket:Bucket::b"}]}`,
		"summary only":    `{"changeSummary": {"update": 2, "delete": 1}}`,
		"errored tofu":    `{"format_version": "1.2", "errored": true, "resource_changes": []}`,
		"no-action tofu":  `{"format_version": "1.2", "resource_changes": [{"address": "x", "type": "y", "change": {"actions": []}}]}`,
		"bad steps shape": `{"steps": {"op": "update"}}`,
	} {
		c, err := checkPlan([]byte(data))
		if err == nil && c.ok() {
			t.Errorf("%s passed", name)
		}
	}
}

func TestCheckPlanEmpty(t *testing.T) {
	c, err := checkPlan([]byte(`{"steps": [], "changeSummary": {}}`))
	if err != nil || !c.ok() {
		t.Errorf("an empty plan should pass: %v %+v", err, c)
	}
}

func TestPlanExpect(t *testing.T) {
	cases := []struct {
		expect string
		ok     bool
	}{
		{"create=1,update=1", true},
		{" update=1 , create=1 ", true},
		{"create=1,update=1,same=1", true}, // quiet ops count when asked for
		{"create=1,update=1,same=2", false},
		{"update=1", false},
		{"create=2,update=1", false},
		{"create=1,update=1,delete=0", true},
	}
	for _, tc := range cases {
		c, err := checkPlan(readPlan(t, "pulumi-update.json"))
		if err != nil {
			t.Fatal(err)
		}
		want, err := parseExpect(tc.expect)
		if err != nil {
			t.Fatalf("%q: %v", tc.expect, err)
		}
		c.expect(want)
		if c.ok() != tc.ok {
			t.Errorf("--expect %q: ok %v, want %v; problems %q", tc.expect, c.ok(), tc.ok, c.Problems)
		}
	}
	for _, bad := range []string{"create", "create=x", "create=-1"} {
		if _, err := parseExpect(bad); err == nil {
			t.Errorf("parseExpect(%q) accepted", bad)
		}
	}
}

func TestGateCmd(t *testing.T) {
	var out strings.Builder
	if err := gateCmd([]string{"plan", "testdata/plans/tofu-update.json"}, "create=1,update=1", "dev", AutonomyConfig{Prod: []string{"prod"}, Widespread: 5}, nil, &out); err != nil {
		t.Fatalf("pass: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "class: 6 (dev: not production)\nok: no delete") {
		t.Errorf("output:\n%s", out.String())
	}
	out.Reset()
	if err := gateCmd([]string{"plan", "-"}, "", "", defaultConfig().Autonomy, strings.NewReader(string(readPlan(t, "tofu-destroy.json"))), &out); !errors.Is(err, errGateBlocked) || !strings.Contains(out.String(), "class: 9 (no --target, so production)\nBLOCKED") {
		t.Errorf("destroy from stdin: %v\n%s", err, out.String())
	}
	if err := gateCmd([]string{"plan", "-"}, "", "", defaultConfig().Autonomy, strings.NewReader("garbage"), &out); !errors.Is(err, errGateBlocked) {
		t.Errorf("garbage: %v", err)
	}
	if err := gateCmd([]string{"plan"}, "", "", defaultConfig().Autonomy, nil, &out); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("usage: %v", err)
	}
}

// tofuDeletes is a tofu plan deleting n stateless resources.
func tofuDeletes(n int) string {
	var rcs []string
	for i := range n {
		rcs = append(rcs, fmt.Sprintf(`{"address": "aws_sns_topic.t%d", "type": "aws_sns_topic", "change": {"actions": ["delete"]}}`, i))
	}
	return `{"format_version": "1.2", "resource_changes": [` + strings.Join(rcs, ",") + `]}`
}

func TestPlanClass(t *testing.T) {
	a := AutonomyConfig{Prod: []string{"*prod*"}, Widespread: 5}
	for _, tc := range []struct {
		name, plan, target string
		a                  AutonomyConfig
		class              int
		pass               bool
	}{
		{"update off prod", "tofu-update.json", "org/app/staging", a, 6, true},
		{"update in prod", "tofu-update.json", "org/app/prod", a, 7, true},
		{"no target is prod", "pulumi-update.json", "", a, 7, true},
		{"no patterns is prod", "pulumi-update.json", "staging", AutonomyConfig{Widespread: 5}, 7, true},
		{"delete off prod", tofuDeletes(1), "staging", a, 7, false},
		{"delete in prod", tofuDeletes(1), "prod", a, 8, false},
		{"stateful replace off prod", "pulumi-replace.json", "staging", a, 8, false},
		{"deletes up to widespread", tofuDeletes(5), "prod", a, 8, false},
		{"deletes past widespread", tofuDeletes(6), "staging", a, 9, false},
		{"unknown op", "tofu-destroy.json", "staging", a, 9, false},
		{"summary-only delete is untyped", `{"changeSummary": {"delete": 1}}`, "staging", a, 8, false},
	} {
		data := []byte(tc.plan)
		if strings.HasSuffix(tc.plan, ".json") {
			data = readPlan(t, tc.plan)
		}
		c, err := checkPlan(data)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		c.classify(tc.a, tc.target)
		if c.Class != tc.class || c.ok() != tc.pass {
			t.Errorf("%s: class %d pass %v, want %d %v\n%s", tc.name, c.Class, c.ok(), tc.class, tc.pass, renderPlanCheck(c))
		}
		if out := renderPlanCheck(c); !strings.Contains(out, fmt.Sprintf("class: %d (", tc.class)) {
			t.Errorf("%s: render:\n%s", tc.name, out)
		}
	}
}

func TestPlanRefused(t *testing.T) {
	a := AutonomyConfig{Prod: []string{"*prod*"}, Widespread: 5}
	for name, tc := range map[string]struct{ plan, expect string }{
		"expect mismatch": {"testdata/plans/tofu-update.json", "create=2,update=1"},
		"errored":         {"-", ""},
	} {
		var out strings.Builder
		in := strings.NewReader(`{"format_version": "1.2", "errored": true, "resource_changes": []}`)
		err := gateCmd([]string{"plan", tc.plan}, tc.expect, "staging", a, in, &out)
		if !errors.Is(err, errGateBlocked) || !strings.Contains(out.String(), "REFUSED") || strings.Contains(out.String(), "class:") {
			t.Errorf("%s: %v\n%s", name, err, out.String())
		}
	}
}
