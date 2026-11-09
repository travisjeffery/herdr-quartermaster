package main

import (
	"strings"
	"testing"
	"time"
)

func TestLinearKey(t *testing.T) {
	cases := []struct {
		name     string
		b        Bead
		prefixes []string
		want     string
	}{
		{"title beats notes", Bead{Title: "ENG-1102: root the hosts", Notes: "Linear ENG-900"}, nil, "ENG-1102"},
		{"a Linear: note when the title has none", Bead{Title: "Root the hosts", Notes: "blocked by ENG-900\nLinear: OPS-3"}, nil, "OPS-3"},
		{"passing mentions in notes are not the bead's issue", Bead{Title: "Root the hosts", Notes: "blocked by ENG-900"}, nil, ""},
		{"prefixes skip other teams", Bead{Title: "OPS-3 follow-up", Notes: "Linear ENG-900"}, []string{"ENG"}, "ENG-900"},
		{"prefixes reject everything else", Bead{Title: "P0-1 outage"}, []string{"ENG"}, ""},
		{"denylist without prefixes", Bead{Title: "Pin SHA-256 and UTF-8 handling", Notes: "Linear: RFC-9110 then WEB-12"}, nil, "WEB-12"},
		{"parts of longer tokens", Bead{Title: "us-west US-EAST-2 RSA-OAEP-256 [a-zA-Z0-9] GLM-5.3", Notes: "tj/eng-1102-fix"}, nil, ""},
		{"none", Bead{Title: "Fix the thing"}, nil, ""},
	}
	for _, c := range cases {
		if got := linearKey(c.b, c.prefixes); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestStateLineEndsWithLinearKey(t *testing.T) {
	pr := openPR()
	pr.ReviewDecision = "APPROVED"
	th := thread(bead(StatusInProgress), agent("idle", 1), pr)
	th.Linear = "ENG-1102"
	if got := stateLine(th); got != "review · PR #7 approved · ENG-1102" {
		t.Errorf("got %q", got)
	}
}

func linearSection(threads []Thread) string {
	out := renderContext(Config{Repo: "/r"}, threads, nil, nil, nil, TickerState{}, nil, t0)
	_, sec, ok := strings.Cut(out, "## Linear")
	if !ok {
		return ""
	}
	sec, _, _ = strings.Cut(sec, "\n## ")
	return sec
}

func TestContextLinearSection(t *testing.T) {
	open := thread(Bead{ID: "backend-a1", Status: StatusInProgress}, agent("idle", 1), &PR{Number: 7, State: "OPEN", Title: "ENG-1102: root the hosts"})
	open.Linear = "ENG-1102"
	merged := thread(Bead{ID: "backend-a2", Status: StatusInProgress}, agent("idle", 1), &PR{Number: 8, State: "MERGED", Title: "ENG-900: pin it", MergedAt: t0.Add(-time.Hour)})
	merged.Linear = "ENG-900"
	untitled := thread(Bead{ID: "backend-a3", Status: StatusInProgress}, agent("idle", 1), &PR{Number: 9, State: "OPEN", IsDraft: true, Title: "Tune retries"})
	untitled.Linear = "ENG-11"
	sec := linearSection([]Thread{open, merged, untitled})
	for _, want := range []string{"- ENG-1102 → In Review (PR #7 open)\n", "- ENG-900 → Done (PR #8 merged)\n", "- PR #9 title lacks ENG-11\n"} {
		if !strings.Contains(sec, want) {
			t.Errorf("missing %q in:%s", want, sec)
		}
	}
	if strings.Contains(sec, "ENG-11 → In Review") {
		t.Errorf("a draft PR is not in review:%s", sec)
	}
}

func TestContextLinearSectionOmittedWithoutKeys(t *testing.T) {
	th := thread(bead(StatusInProgress), agent("idle", 1), openPR())
	if out := renderContext(Config{Repo: "/r"}, []Thread{th}, nil, nil, nil, TickerState{}, nil, t0); strings.Contains(out, "## Linear") {
		t.Errorf("got a Linear section:\n%s", out)
	}
}

func TestBriefNamesLinearIssue(t *testing.T) {
	b := Bead{ID: "backend-a1", Title: "ENG-1102: root the hosts"}
	if got := brief(b, "tj/x", "/w", "ENG-1102", ""); !strings.Contains(got, "Linear issue: ENG-1102. Prefix the PR title with `ENG-1102: `") {
		t.Errorf("got %q", got)
	}
	if got := brief(b, "tj/x", "/w", "", ""); strings.Contains(got, "Linear issue") {
		t.Errorf("got %q", got)
	}
}
