package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// `quartermaster gate plan <file>` is the check an autonomous worker runs on
// an infrastructure preview before applying it. It passes only a plan that
// creates, updates or reads: any delete, any replace, any operation it does
// not know, a plan that does not parse, and a plan that differs from the
// change the worker expected all fail. Failing means stop for TJ.

// planSafe are the only operations a plan may contain and still pass: Pulumi
// step ops and OpenTofu actions alike.
var planSafe = []string{"same", "no-op", "create", "update", "read", "refresh", "import"}

// planQuiet are safe operations that change nothing, left out of --expect.
var planQuiet = []string{"same", "no-op", "read", "refresh"}

// statefulHints mark a resource type as holding data, so a refusal says how
// serious it is. Every delete and replace fails regardless; this only labels.
var statefulHints = []string{
	"rds", "db_instance", "dbinstance", "database", "dynamodb", "table", "s3", "bucket",
	"volume", "ebs", "efs", "fsx", "kms", "secret", "ssm", "route53", "zone", "cluster",
	"elasticache", "redis", "kafka", "msk", "sqs", "queue", "persistentvolume", "namespace",
	"opensearch", "elasticsearch", "redshift", "backup", "ecr", "snapshot", "clickhouse",
}

type PlanChange struct {
	Op       string
	Type     string
	Name     string
	Stateful bool
}

type PlanCheck struct {
	Format  string
	Counts  map[string]int
	Blocked []PlanChange
	// Problems are reasons the plan fails that are not a single change: it
	// errored, or it differs from what was expected.
	Problems []string
}

func (c PlanCheck) ok() bool { return len(c.Blocked) == 0 && len(c.Problems) == 0 }

func (c *PlanCheck) add(op, typ, name string) {
	c.Counts[op]++
	if !slices.Contains(planSafe, op) {
		c.Blocked = append(c.Blocked, PlanChange{Op: op, Type: typ, Name: name, Stateful: stateful(typ)})
	}
}

func stateful(typ string) bool {
	t := strings.ToLower(typ)
	for _, h := range statefulHints {
		if strings.Contains(t, h) {
			return true
		}
	}
	return false
}

// checkPlan reads `pulumi preview --json` or `tofu show -json <plan>` output.
func checkPlan(data []byte) (PlanCheck, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return PlanCheck{}, fmt.Errorf("plan is not JSON: %w", err)
	}
	c := PlanCheck{Counts: map[string]int{}}
	_, steps := top["steps"]
	_, summary := top["changeSummary"]
	_, changes := top["resource_changes"]
	_, tofuVersion := top["format_version"]
	switch {
	case steps || summary:
		c.Format = "pulumi"
		return c, pulumiPlan(data, &c)
	case changes || tofuVersion:
		c.Format = "tofu"
		return c, tofuPlan(data, &c)
	}
	return c, errors.New("plan is neither `pulumi preview --json` nor `tofu show -json` output")
}

func pulumiPlan(data []byte, c *PlanCheck) error {
	var p struct {
		Steps []struct {
			Op       string `json:"op"`
			URN      string `json:"urn"`
			NewState *struct {
				Type string `json:"type"`
			} `json:"newState"`
			OldState *struct {
				Type string `json:"type"`
			} `json:"oldState"`
		} `json:"steps"`
		ChangeSummary map[string]int `json:"changeSummary"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("pulumi preview: %w", err)
	}
	if p.Steps == nil {
		// No steps, only the summary: judge by its counts.
		for op, n := range p.ChangeSummary {
			for range n {
				c.add(op, "", "(from changeSummary)")
			}
		}
		return nil
	}
	for _, s := range p.Steps {
		typ, name := urnParts(s.URN)
		if s.NewState != nil && s.NewState.Type != "" {
			typ = s.NewState.Type
		} else if s.OldState != nil && s.OldState.Type != "" {
			typ = s.OldState.Type
		}
		c.add(s.Op, typ, name)
	}
	return nil
}

// urnParts is a Pulumi URN's resource type and name:
// urn:pulumi:<stack>::<project>::<parent$...$type>::<name>.
func urnParts(urn string) (string, string) {
	parts := strings.Split(urn, "::")
	if len(parts) < 4 {
		return "", urn
	}
	typ := parts[len(parts)-2]
	if i := strings.LastIndex(typ, "$"); i >= 0 {
		typ = typ[i+1:]
	}
	return typ, parts[len(parts)-1]
}

func tofuPlan(data []byte, c *PlanCheck) error {
	var p struct {
		Errored         bool `json:"errored"`
		ResourceChanges []struct {
			Address string `json:"address"`
			Type    string `json:"type"`
			Change  struct {
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("tofu plan: %w", err)
	}
	if p.Errored {
		c.Problems = append(c.Problems, "the plan errored, so it may be incomplete")
	}
	for _, rc := range p.ResourceChanges {
		c.add(tofuOp(rc.Change.Actions), rc.Type, rc.Address)
	}
	return nil
}

// tofuOp names a resource change's action list: one action as itself, a
// delete and a create together as replace, anything else joined with +.
func tofuOp(actions []string) string {
	switch {
	case len(actions) == 1:
		return actions[0]
	case len(actions) == 2 && slices.Contains(actions, "delete") && slices.Contains(actions, "create"):
		return "replace"
	case len(actions) == 0:
		return "(no action)"
	}
	return strings.Join(actions, "+")
}

// parseExpect reads --expect: op=count pairs, comma-separated
// ("create=2,update=1").
func parseExpect(s string) (map[string]int, error) {
	want := map[string]int{}
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		op, n, ok := strings.Cut(p, "=")
		count, err := strconv.Atoi(strings.TrimSpace(n))
		if !ok || err != nil || count < 0 {
			return nil, fmt.Errorf("--expect: %q is not op=count", p)
		}
		want[strings.TrimSpace(op)] = count
	}
	return want, nil
}

// expect fails the check where the plan's changes differ from want. Quiet
// operations are not compared.
func (c *PlanCheck) expect(want map[string]int) {
	ops := map[string]bool{}
	for op := range want {
		ops[op] = true
	}
	for op := range c.Counts {
		if !slices.Contains(planQuiet, op) {
			ops[op] = true
		}
	}
	for _, op := range sortedKeys(ops) {
		if c.Counts[op] != want[op] {
			c.Problems = append(c.Problems, fmt.Sprintf("expected %d %s, the plan has %d", want[op], op, c.Counts[op]))
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func renderPlanCheck(c PlanCheck) string {
	var s strings.Builder
	var counts []string
	for _, op := range sortedKeys(c.Counts) {
		counts = append(counts, fmt.Sprintf("%d %s", c.Counts[op], op))
	}
	fmt.Fprintf(&s, "plan: %s, %s\n", c.Format, orDash(strings.Join(counts, ", ")))
	if c.ok() {
		s.WriteString("ok: no delete, replace or unknown operation\n")
		return s.String()
	}
	s.WriteString("BLOCKED: stop for TJ (CRITICAL: delete/replace)\n")
	for _, p := range c.Problems {
		fmt.Fprintf(&s, "- %s\n", p)
	}
	for _, b := range c.Blocked {
		line := fmt.Sprintf("- %s %s %s", b.Op, orDash(b.Type), b.Name)
		if b.Stateful {
			line += " (stateful)"
		}
		fmt.Fprintln(&s, line)
	}
	return s.String()
}

var errGateBlocked = errors.New("gate: plan blocked")

// gateCmd is `quartermaster gate plan <file|-> [--expect op=n,...]`. It exits
// nonzero unless the plan passes.
func gateCmd(pos []string, expect string, stdin io.Reader, w io.Writer) error {
	if len(pos) != 2 || pos[0] != "plan" {
		return errors.New("usage: quartermaster gate plan <file|-> [--expect create=N,update=N,...]")
	}
	var data []byte
	var err error
	if pos[1] == "-" {
		data, err = io.ReadAll(stdin)
	} else {
		data, err = os.ReadFile(pos[1])
	}
	if err != nil {
		return err
	}
	c, err := checkPlan(data)
	if err != nil {
		return fmt.Errorf("%w: %v", errGateBlocked, err)
	}
	if expect != "" {
		want, err := parseExpect(expect)
		if err != nil {
			return err
		}
		c.expect(want)
	}
	fmt.Fprint(w, renderPlanCheck(c))
	if !c.ok() {
		return errGateBlocked
	}
	return nil
}
