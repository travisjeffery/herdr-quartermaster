package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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

// UnmarshalJSON accepts both bd list's {depends_on_id, type} shape and bd
// show's expanded {id, dependency_type} shape.
func (d *Dependency) UnmarshalJSON(data []byte) error {
	var raw struct {
		DependsOn      string `json:"depends_on_id"`
		Type           string `json:"type"`
		ID             string `json:"id"`
		DependencyType string `json:"dependency_type"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	d.DependsOn = raw.DependsOn
	if d.DependsOn == "" {
		d.DependsOn = raw.ID
	}
	d.Type = raw.Type
	if d.Type == "" {
		d.Type = raw.DependencyType
	}
	return nil
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

var (
	errBeadNotFound   = errors.New("bead not found")
	errMemoryNotFound = errors.New("memory not found")
)

type Beads struct {
	// actor is recorded as the claimer: the agent being started, not whoever
	// pressed the key.
	actor string
	// dir pins BEADS_DIR (scrubbing competing BEADS_* database overrides) when
	// set, so callers resolve quartermaster's configured workspace regardless
	// of cwd or caller environment.
	dir string
	// readonly passes --readonly to bd for read-only wrappers.
	readonly bool
}

// beadsDir is the configured Beads database directory for c: beads_dir when
// set, else repo/.beads.
func (c Config) beadsDir() string {
	if c.BeadsDir != "" {
		return c.BeadsDir
	}
	if c.Repo != "" {
		return filepath.Join(c.Repo, ".beads")
	}
	return ""
}

// readBeads returns a read-only Beads handle pinned to c's configured database.
func (c Config) readBeads() Beads {
	return Beads{dir: c.beadsDir(), readonly: true}
}

// beadsEnv returns environ with BD_JSON_ENVELOPE removed and, when beadsDir is
// set, every BEADS_* variable replaced by the pinned BEADS_DIR (plus
// BEADS_ACTOR when actor is set).
func beadsEnv(environ []string, beadsDir, actor string) []string {
	env := slices.Clone(environ)
	if beadsDir != "" {
		env = slices.DeleteFunc(env, func(kv string) bool {
			return strings.HasPrefix(kv, "BEADS_") || strings.HasPrefix(kv, "BD_JSON_ENVELOPE=")
		})
		env = append(env, "BEADS_DIR="+beadsDir)
		if actor != "" {
			env = append(env, "BEADS_ACTOR="+actor)
		}
		return env
	}
	env = slices.DeleteFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, "BD_JSON_ENVELOPE=")
	})
	if actor != "" {
		env = append(env, "BEADS_ACTOR="+actor)
	}
	return env
}

func (b Beads) runRaw(args ...string) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bd", args...)
	cmd.Env = beadsEnv(os.Environ(), b.dir, b.actor)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func (b Beads) run(args ...string) ([]byte, error) {
	stdout, stderr, err := b.runRaw(args...)
	if err != nil {
		return nil, fmt.Errorf("bd %s: %v: %s", args[0], err, strings.TrimSpace(string(stderr)))
	}
	return stdout, nil
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
	if strings.TrimSpace(id) == "" {
		return Bead{}, errors.New("bead id is required")
	}
	args := []string{"show", id, "--json"}
	if b.readonly {
		args = []string{"show", "--json", "--readonly", "--", id}
	}
	out, stderr, err := b.runRaw(args...)
	if err != nil {
		if isBeadNotFound(out, stderr) {
			return Bead{}, fmt.Errorf("%w: %s", errBeadNotFound, id)
		}
		return Bead{}, fmt.Errorf("bd show: %v: %s", err, strings.TrimSpace(string(stderr)))
	}
	var beads []Bead
	if err := json.Unmarshal(out, &beads); err != nil {
		return Bead{}, err
	}
	if len(beads) == 0 {
		return Bead{}, fmt.Errorf("%w: %s", errBeadNotFound, id)
	}
	return beads[0], nil
}

func isBeadNotFound(stdout, stderr []byte) bool {
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(bytes.TrimSpace(stdout), &payload) == nil && payload.Error != "" {
		msg := strings.ToLower(payload.Error)
		return strings.Contains(msg, "no issues found") || strings.Contains(msg, "issue not found")
	}
	low := strings.ToLower(string(stderr))
	if strings.Contains(low, "database") {
		return false
	}
	return strings.Contains(low, "no issues found") || (strings.Contains(low, "issue ") && strings.Contains(low, " not found"))
}

// Recall retrieves a stored bd remember memory by exact key without writing
// any state.
func (b Beads) Recall(key string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", errors.New("memory key is required")
	}
	args := []string{"recall", "--json"}
	if b.readonly {
		args = append(args, "--readonly")
	}
	args = append(args, "--", key)
	out, stderr, err := b.runRaw(args...)
	var resp struct {
		Found *bool  `json:"found"`
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	parsed := json.Unmarshal(bytes.TrimSpace(out), &resp) == nil
	if err != nil {
		if parsed && resp.Found != nil && !*resp.Found {
			return "", fmt.Errorf("%w: %s", errMemoryNotFound, key)
		}
		if strings.Contains(strings.ToLower(string(stderr)), "no memory with key") {
			return "", fmt.Errorf("%w: %s", errMemoryNotFound, key)
		}
		return "", fmt.Errorf("bd recall: %v: %s", err, strings.TrimSpace(string(stderr)))
	}
	if !parsed || resp.Found == nil {
		return "", fmt.Errorf("bd recall: unexpected output for key %q", key)
	}
	if !*resp.Found {
		return "", fmt.Errorf("%w: %s", errMemoryNotFound, key)
	}
	return resp.Value, nil
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
