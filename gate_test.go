package main

import (
	"errors"
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
	if err := gateCmd([]string{"plan", "testdata/plans/tofu-update.json"}, "create=1,update=1", nil, &out); err != nil {
		t.Fatalf("pass: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "ok: no delete") {
		t.Errorf("output:\n%s", out.String())
	}
	out.Reset()
	if err := gateCmd([]string{"plan", "-"}, "", strings.NewReader(string(readPlan(t, "tofu-destroy.json"))), &out); !errors.Is(err, errGateBlocked) {
		t.Errorf("destroy from stdin: %v", err)
	}
	if err := gateCmd([]string{"plan", "-"}, "", strings.NewReader("garbage"), &out); !errors.Is(err, errGateBlocked) {
		t.Errorf("garbage: %v", err)
	}
	if err := gateCmd([]string{"plan"}, "", nil, &out); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("usage: %v", err)
	}
}
