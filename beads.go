package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Bead struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Notes string `json:"notes"`
	// Description is only in bd show; bd list leaves it out.
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	Status      string    `json:"status"`
	Priority    int       `json:"priority"`
	Type        string    `json:"issue_type"`
	Assignee    string    `json:"assignee"`
	Labels      []string  `json:"labels"`
	Parent      string    `json:"parent"`
	UpdatedAt   time.Time `json:"updated_at"`

	ClosedAt    time.Time `json:"closed_at"`
	CloseReason string    `json:"close_reason"`

	Dependencies []Dependency `json:"dependencies"`
}

// Dependency is one edge out of a bead: DependsOn blocks it when Type is
// "blocks"; parent-child and related edges never do.
type Dependency struct {
	DependsOn string `json:"depends_on_id"`
	Type      string `json:"type"`
}

func (b Bead) HasLabel(l string) bool {
	for _, x := range b.Labels {
		if x == l {
			return true
		}
	}
	return false
}

const (
	StatusOpen       = "open"
	StatusInProgress = "in_progress"
	StatusBlocked    = "blocked"
	StatusNeedsMe    = "needs_me"
	StatusClosed     = "closed"
	LabelNext        = "next"
	LabelRollingOut  = "rolling-out"
)

type Beads struct {
	// actor is recorded as the claimer: the agent being started, not whoever
	// pressed the key.
	actor string
}

func (b Beads) run(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bd", args...)
	if b.actor != "" {
		cmd.Env = append(cmd.Environ(), "BEADS_ACTOR="+b.actor)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("bd %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (b Beads) list(args ...string) ([]Bead, error) {
	out, err := b.run(append([]string{"list", "--json", "--limit", "0"}, args...)...)
	if err != nil {
		return nil, err
	}
	var beads []Bead
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	return beads, json.Unmarshal(out, &beads)
}

// Active is every bead a worker could be on or a human could be owed.
func (b Beads) Active() ([]Bead, error) {
	return b.list("--status", strings.Join([]string{StatusInProgress, StatusNeedsMe, StatusBlocked}, ","))
}

func (b Beads) Next() ([]Bead, error) {
	return b.list("--status", StatusOpen, "--label", LabelNext)
}

func (b Beads) Ready() ([]Bead, error) {
	out, err := b.run("ready", "--json", "--limit", "30")
	if err != nil {
		return nil, err
	}
	var beads []Bead
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	return beads, json.Unmarshal(out, &beads)
}

func (b Beads) Show(id string) (Bead, error) {
	out, err := b.run("show", id, "--json")
	if err != nil {
		return Bead{}, err
	}
	var beads []Bead
	if err := json.Unmarshal(out, &beads); err != nil {
		return Bead{}, err
	}
	if len(beads) == 0 {
		return Bead{}, fmt.Errorf("no bead %s", id)
	}
	return beads[0], nil
}

func (b Beads) Claim(id string) error {
	_, err := b.run("update", id, "--claim")
	return err
}

func (b Beads) SetStatus(id, status string) error {
	_, err := b.run("update", id, "--status", status)
	return err
}

func (b Beads) Note(id, text string) error {
	_, err := b.run("note", id, text)
	return err
}

func (b Beads) SetLabel(id, label string, on bool) error {
	op := "add"
	if !on {
		op = "remove"
	}
	_, err := b.run("label", op, id, label)
	return err
}

var prURL = regexp.MustCompile(`https://github\.com/([\w.-]+/[\w.-]+)/pull/(\d+)`)

// NotedPRs returns the PR URLs a bead's notes mention, most recent last.
func NotedPRs(notes string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range prURL.FindAllString(notes, -1) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

var slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// branchFor names a bead's branch: the id keeps it unique and traceable, the
// slug keeps it readable.
func branchFor(prefix string, b Bead) string {
	slug := strings.Trim(slugNonAlnum.ReplaceAllString(strings.ToLower(b.Title), "-"), "-")
	if len(slug) > 28 {
		slug = slug[:28]
		if i := strings.LastIndex(slug, "-"); i > 0 {
			slug = slug[:i]
		}
	}
	name := prefix + agentName(b.ID)
	if slug != "" {
		name += "-" + slug
	}
	return name
}

// onBeadBranch reports whether head is a branch cut for b. Branches are
// <prefix><id>-<slug> and a sub-bead's id gains a digit segment (backend-x.7
// is tj/backend-x-7-…), so after b's own stem a digit segment belongs to a
// child and only counts when head is exactly the branch quartermaster names for b.
func onBeadBranch(prefix, head string, b Bead) bool {
	want := prefix + agentName(b.ID)
	if head == want || head == branchFor(prefix, b) {
		return true
	}
	rest, ok := strings.CutPrefix(head, want+"-")
	if !ok {
		return false
	}
	seg, _, _ := strings.Cut(rest, "-")
	_, err := strconv.Atoi(seg)
	return err != nil
}

func shortTitle(t string, n int) string {
	t = strings.Join(strings.Fields(t), " ")
	if len(t) <= n {
		return t
	}
	t = t[:n]
	if i := strings.LastIndex(t, " "); i > 0 {
		t = t[:i]
	}
	return strings.TrimRight(t, " ,:;-") + "…"
}
