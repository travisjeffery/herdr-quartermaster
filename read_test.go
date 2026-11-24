package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBeadsEnvPinsDirAndStripsEnvelope(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"BEADS_DIR=/wrong/.beads",
		"BEADS_DB=/wrong/db",
		"BEADS_ACTOR=old",
		"BD_JSON_ENVELOPE=1",
		"HOME=/home/example",
	}

	got := beadsEnv(in, "/configured/.beads", "")
	want := []string{
		"PATH=/usr/bin",
		"HOME=/home/example",
		"BEADS_DIR=/configured/.beads",
	}
	if !slices.Equal(got, want) {
		t.Errorf("beadsEnv pinned = %q, want %q", got, want)
	}

	gotActor := beadsEnv(in, "/configured/.beads", "worker")
	wantActor := []string{
		"PATH=/usr/bin",
		"HOME=/home/example",
		"BEADS_DIR=/configured/.beads",
		"BEADS_ACTOR=worker",
	}
	if !slices.Equal(gotActor, wantActor) {
		t.Errorf("beadsEnv pinned+actor = %q, want %q", gotActor, wantActor)
	}

	gotDefault := beadsEnv(in, "", "")
	if slices.Contains(gotDefault, "BD_JSON_ENVELOPE=1") {
		t.Errorf("beadsEnv unpinned kept BD_JSON_ENVELOPE: %q", gotDefault)
	}
	if !slices.Contains(gotDefault, "BEADS_DIR=/wrong/.beads") {
		t.Errorf("beadsEnv unpinned dropped BEADS_DIR: %q", gotDefault)
	}
}

func TestDependencyUnmarshalBothShapes(t *testing.T) {
	var listDep Dependency
	if err := json.Unmarshal([]byte(`{"depends_on_id":"task-1","type":"blocks"}`), &listDep); err != nil {
		t.Fatal(err)
	}
	if listDep != (Dependency{DependsOn: "task-1", Type: "blocks"}) {
		t.Errorf("list shape = %+v, want {task-1 blocks}", listDep)
	}

	var showDep Dependency
	if err := json.Unmarshal([]byte(`{"id":"task-2","title":"ignored","dependency_type":"discovered-from"}`), &showDep); err != nil {
		t.Fatal(err)
	}
	if showDep != (Dependency{DependsOn: "task-2", Type: "discovered-from"}) {
		t.Errorf("show shape = %+v, want {task-2 discovered-from}", showDep)
	}
}

func TestShowCmdJSONAndText(t *testing.T) {
	created := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 11, 21, 15, 30, 0, 0, time.UTC)
	closed := time.Date(2026, 11, 21, 16, 0, 0, 0, time.UTC)

	sample := Bead{
		ID:          "task-42",
		Title:       "Add workspace read wrappers",
		Status:      StatusClosed,
		Priority:    2,
		Type:        "task",
		Assignee:    "alice",
		Parent:      "task-1",
		Labels:      []string{"cli", "read-only"},
		Description: "Read a bead and a memory without cwd dependence.",
		Notes:       "n1\nn2\nn3\nn4\nn5\nn6\n\nn7",
		CreatedAt:   created,
		UpdatedAt:   updated,
		ClosedAt:    closed,
		CloseReason: "Shipped and verified.",
		Dependencies: []Dependency{
			{DependsOn: "task-1", Type: "discovered-from"},
			{},
		},
	}

	stub := func(id string) (Bead, error) {
		if id != sample.ID {
			return Bead{}, errors.New("unexpected id: " + id)
		}
		return sample, nil
	}

	var jsonBuf bytes.Buffer
	if err := showCmd(stub, []string{"task-42"}, true, &jsonBuf); err != nil {
		t.Fatalf("showCmd(--json): %v", err)
	}
	var got showOutput
	if err := json.Unmarshal(jsonBuf.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, jsonBuf.String())
	}
	if got.ID != sample.ID || got.Title != sample.Title || got.Status != sample.Status || got.Priority != sample.Priority {
		t.Errorf("core fields = %+v, want id=%q title=%q status=%q priority=%d", got, sample.ID, sample.Title, sample.Status, sample.Priority)
	}
	if got.Type != sample.Type || got.Assignee != sample.Assignee || got.Parent != sample.Parent {
		t.Errorf("metadata = %+v", got)
	}
	if !slices.Equal(got.Labels, sample.Labels) || got.Notes != sample.Notes || got.CloseReason != sample.CloseReason {
		t.Errorf("labels/notes/close_reason mismatch: %+v", got)
	}
	wantTail := []string{"n3", "n4", "n5", "n6", "n7"}
	if !slices.Equal(got.NotesTail, wantTail) {
		t.Errorf("notes_tail = %q, want %q", got.NotesTail, wantTail)
	}
	wantDeps := []Dependency{{DependsOn: "task-1", Type: "discovered-from"}}
	if !slices.Equal(got.Dependencies, wantDeps) {
		t.Errorf("dependencies = %+v, want %+v", got.Dependencies, wantDeps)
	}

	// An open bead omits zero closed_at and empty close_reason.
	openStub := func(id string) (Bead, error) {
		return Bead{ID: id, Title: "Open task", Status: StatusOpen, Priority: 0}, nil
	}
	var openBuf bytes.Buffer
	if err := showCmd(openStub, []string{"task-0"}, true, &openBuf); err != nil {
		t.Fatalf("showCmd open --json: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(openBuf.Bytes(), &raw); err != nil {
		t.Fatalf("open json: %v", err)
	}
	if _, hasClosedAt := raw["closed_at"]; hasClosedAt {
		t.Errorf("open bead emitted closed_at: %s", openBuf.String())
	}
	if raw["priority"] != float64(0) {
		t.Errorf("open bead priority = %v, want 0", raw["priority"])
	}

	// Text mode includes key metadata, description, dependencies, and notes.
	var textBuf bytes.Buffer
	if err := showCmd(stub, []string{"task-42"}, false, &textBuf); err != nil {
		t.Fatalf("showCmd(text): %v", err)
	}
	text := textBuf.String()
	for _, want := range []string{
		"task-42 · closed · P2 · task · assignee alice · parent task-1 · labels cli, read-only",
		"Add workspace read wrappers",
		"Close reason:\nShipped and verified.",
		"Description:\nRead a bead and a memory without cwd dependence.",
		"- task-1 (discovered-from)",
		"- n7",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text output missing %q:\n%s", want, text)
		}
	}

	// Bad invocations never call show.
	called := false
	never := func(string) (Bead, error) {
		called = true
		return Bead{}, nil
	}
	for _, args := range [][]string{nil, {""}, {"   "}, {"task-1", "task-2"}} {
		if err := showCmd(never, args, false, &bytes.Buffer{}); err == nil {
			t.Errorf("showCmd(%q) succeeded, want usage error", args)
		}
	}
	if called {
		t.Error("show callback was called on invalid arguments")
	}
}

func TestMemoryCmdGetAndErrors(t *testing.T) {
	store := map[string]string{
		"release-note": "line one\nline two\n",
		"plain-key":    "remembered value",
	}
	recall := func(key string) (string, error) {
		v, ok := store[key]
		if !ok {
			return "", errors.Join(errMemoryNotFound, errors.New(key))
		}
		return v, nil
	}

	var out bytes.Buffer
	if err := memoryCmd(recall, []string{"get", "plain-key"}, false, &out); err != nil {
		t.Fatalf("memory get plain-key: %v", err)
	}
	if out.String() != "remembered value\n" {
		t.Errorf("plain-key output = %q, want %q", out.String(), "remembered value\n")
	}

	out.Reset()
	if err := memoryCmd(recall, []string{"get", "release-note"}, false, &out); err != nil {
		t.Fatalf("memory get release-note: %v", err)
	}
	if out.String() != "line one\nline two\n" {
		t.Errorf("release-note output = %q, want single trailing newline", out.String())
	}

	out.Reset()
	if err := memoryCmd(recall, []string{"get", "plain-key"}, true, &out); err != nil {
		t.Fatalf("memory get --json: %v", err)
	}
	var mem memoryOutput
	if err := json.Unmarshal(out.Bytes(), &mem); err != nil {
		t.Fatalf("memory --json unmarshal: %v", err)
	}
	if mem.Key != "plain-key" || mem.Value != "remembered value" {
		t.Errorf("memory --json = %+v", mem)
	}

	out.Reset()
	err := memoryCmd(recall, []string{"get", "missing-key"}, false, &out)
	if !errors.Is(err, errMemoryNotFound) {
		t.Errorf("missing key error = %v, want errMemoryNotFound", err)
	}
	if out.Len() != 0 {
		t.Errorf("missing key wrote to stdout: %q", out.String())
	}

	// Non-read subcommands and bad arity are rejected without invoking recall.
	called := false
	never := func(string) (string, error) {
		called = true
		return "", nil
	}
	for _, args := range [][]string{
		nil,
		{"set", "k", "v"},
		{"remember", "k", "v"},
		{"forget", "k"},
		{"delete", "k"},
		{"get"},
		{"get", ""},
		{"get", "a", "b"},
	} {
		if err := memoryCmd(never, args, false, &bytes.Buffer{}); err == nil {
			t.Errorf("memoryCmd(%q) succeeded, want error", args)
		}
	}
	if called {
		t.Error("recall callback was called for rejected subcommand/args")
	}
}

func TestShowAndMemoryResolveWorkspaceFromUnrelatedCWD(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "config")
	stateDir := filepath.Join(tmp, "state")
	binDir := filepath.Join(tmp, "bin")
	repoDir := filepath.Join(tmp, "repo")
	beadsDir := filepath.Join(tmp, "configured-beads")
	otherDir := filepath.Join(tmp, "unrelated-cwd")
	logFile := filepath.Join(tmp, "bd-invocations.jsonl")

	for _, d := range []string{cfgDir, stateDir, binDir, repoDir, beadsDir, filepath.Join(otherDir, ".beads")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(cfgDir, "config.toml"),
		"repo = "+jsonString(repoDir)+"\nbeads_dir = "+jsonString(beadsDir)+"\n", 0o644)

	// Fake bd logs every invocation's argv and relevant env, enforces --readonly,
	// rejects any write subcommand, and returns realistic JSON for show/recall.
	fakeBD := filepath.Join(binDir, "bd")
	writeFile(t, fakeBD, `#!/bin/sh
set -eu
printf '%s\t%s\t%s\t%s\n' "$BEADS_DIR" "${BEADS_DB:-}" "${BD_JSON_ENVELOPE:-}" "$*" >> "$QM_BD_LOG"
case "$1" in
  show)
    has_ro=0
    has_json=0
    id=""
    shift
    while [ $# -gt 0 ]; do
      case "$1" in
        --readonly) has_ro=1 ;;
        --json) has_json=1 ;;
        --) shift; id="${1:-}"; break ;;
        *) id="$1" ;;
      esac
      shift
    done
    [ "$has_ro" = "1" ] || { echo "missing --readonly" >&2; exit 2; }
    [ "$has_json" = "1" ] || { echo "missing --json" >&2; exit 2; }
    case "$id" in
      task-100)
        cat <<'JSON'
[
  {
    "id": "task-100",
    "title": "Sample bead from configured workspace",
    "status": "in_progress",
    "priority": 2,
    "issue_type": "task",
    "assignee": "worker",
    "notes": "first note\nsecond note",
    "description": "Body text",
    "created_at": "2026-11-23T01:02:03Z",
    "updated_at": "2026-11-24T04:05:06Z",
    "labels": ["alpha"],
    "dependencies": [
      {"id": "task-1", "title": "Parent spec", "dependency_type": "discovered-from"}
    ]
  }
]
JSON
        exit 0
        ;;
      db-broken)
        echo "Error: no beads database found" >&2
        exit 1
        ;;
      *)
        printf '{"error":"no issues found matching the provided IDs","schema_version":1}\n'
        printf 'warning: beads.role not configured\nIssue %s not found\n' "$id" >&2
        exit 1
        ;;
    esac
    ;;
  recall)
    has_ro=0
    has_json=0
    key=""
    shift
    while [ $# -gt 0 ]; do
      case "$1" in
        --readonly) has_ro=1 ;;
        --json) has_json=1 ;;
        --) shift; key="${1:-}"; break ;;
        *) key="$1" ;;
      esac
      shift
    done
    [ "$has_ro" = "1" ] || { echo "missing --readonly" >&2; exit 2; }
    [ "$has_json" = "1" ] || { echo "missing --json" >&2; exit 2; }
    case "$key" in
      deploy-note)
        printf '{"found":true,"key":"deploy-note","schema_version":1,"value":"Always verify health before switchover."}\n'
        exit 0
        ;;
      db-broken)
        echo "Error: no beads database found" >&2
        exit 1
        ;;
      *)
        printf '{"found":false,"key":"%s","schema_version":1,"value":""}\n' "$key"
        exit 1
        ;;
    esac
    ;;
  *)
    echo "write or disallowed command invoked: $1" >&2
    exit 99
    ;;
esac
`, 0o755)

	t.Setenv("QUARTERMASTER_CONFIG_DIR", cfgDir)
	t.Setenv("QUARTERMASTER_STATE_DIR", stateDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("QM_BD_LOG", logFile)
	// Poison caller environment with wrong database overrides and envelope mode.
	t.Setenv("BEADS_DIR", filepath.Join(otherDir, ".beads"))
	t.Setenv("BEADS_DB", "/wrong/beads.db")
	t.Setenv("BD_JSON_ENVELOPE", "1")

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(otherDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	rb := cfg.readBeads()

	// 1. show --json from unrelated cwd resolves configured workspace and matches bd show fields.
	var showBuf bytes.Buffer
	if err := showCmd(rb.Show, []string{"task-100"}, true, &showBuf); err != nil {
		t.Fatalf("showCmd --json: %v", err)
	}
	var gotShow showOutput
	if err := json.Unmarshal(showBuf.Bytes(), &gotShow); err != nil {
		t.Fatalf("unmarshal show --json: %v", err)
	}
	if gotShow.ID != "task-100" || gotShow.Title != "Sample bead from configured workspace" || gotShow.Status != StatusInProgress {
		t.Errorf("show --json fields = %+v", gotShow)
	}
	if !slices.Equal(gotShow.NotesTail, []string{"first note", "second note"}) {
		t.Errorf("show --json notes_tail = %q", gotShow.NotesTail)
	}
	if !slices.Equal(gotShow.Dependencies, []Dependency{{DependsOn: "task-1", Type: "discovered-from"}}) {
		t.Errorf("show --json dependencies = %+v", gotShow.Dependencies)
	}

	// 2. Missing bead returns clean errBeadNotFound; DB failure does not.
	if err := showCmd(rb.Show, []string{"missing-bead"}, true, &bytes.Buffer{}); !errors.Is(err, errBeadNotFound) {
		t.Errorf("missing bead err = %v, want errBeadNotFound", err)
	}
	if err := showCmd(rb.Show, []string{"db-broken"}, true, &bytes.Buffer{}); err == nil || errors.Is(err, errBeadNotFound) || !strings.Contains(err.Error(), "no beads database found") {
		t.Errorf("db-broken show err = %v, want underlying db error", err)
	}

	// 3. memory get returns stored memory; missing key returns clean errMemoryNotFound; DB failure does not.
	var memBuf bytes.Buffer
	if err := memoryCmd(rb.Recall, []string{"get", "deploy-note"}, false, &memBuf); err != nil {
		t.Fatalf("memory get: %v", err)
	}
	if memBuf.String() != "Always verify health before switchover.\n" {
		t.Errorf("memory get = %q", memBuf.String())
	}
	if err := memoryCmd(rb.Recall, []string{"get", "absent-key"}, false, &bytes.Buffer{}); !errors.Is(err, errMemoryNotFound) {
		t.Errorf("absent memory err = %v, want errMemoryNotFound", err)
	}
	if err := memoryCmd(rb.Recall, []string{"get", "db-broken"}, false, &bytes.Buffer{}); err == nil || errors.Is(err, errMemoryNotFound) || !strings.Contains(err.Error(), "no beads database found") {
		t.Errorf("db-broken recall err = %v, want underlying db error", err)
	}

	// 4. Full run() dispatch accepts interspersed flags, resolves default repo/.beads,
	// writes expected stdout, and rejects non-read memory subcommands.
	if got := (Config{Repo: "/example/repo"}).beadsDir(); got != filepath.Join("/example/repo", ".beads") {
		t.Errorf("beadsDir default = %q", got)
	}
	if got := (Config{Repo: "/example/repo", BeadsDir: "/custom/beads"}).beadsDir(); got != "/custom/beads" {
		t.Errorf("beadsDir explicit = %q", got)
	}
	runOut := captureStdout(t, func() {
		if err := run([]string{"show", "task-100", "--json"}); err != nil {
			t.Errorf("run(show task-100 --json): %v", err)
		}
	})
	var runShow showOutput
	if err := json.Unmarshal([]byte(runOut), &runShow); err != nil {
		t.Fatalf("run(show --json) invalid json: %v\n%s", err, runOut)
	}
	if runShow.ID != "task-100" || runShow.Status != StatusInProgress {
		t.Errorf("run(show --json) = %+v", runShow)
	}

	memRunOut := captureStdout(t, func() {
		if err := run([]string{"memory", "get", "deploy-note"}); err != nil {
			t.Errorf("run(memory get deploy-note): %v", err)
		}
	})
	if memRunOut != "Always verify health before switchover.\n" {
		t.Errorf("run(memory get) stdout = %q", memRunOut)
	}
	if err := run([]string{"memory", "set", "k", "v"}); err == nil {
		t.Error("run(memory set) succeeded, want error")
	}

	// 5. Inspect every bd invocation: pinned BEADS_DIR, scrubbed overrides, --readonly, read-only verbs only.
	rawLog, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(rawLog)), "\n")
	if len(lines) != 8 {
		t.Fatalf("expected 8 bd calls, got %d:\n%s", len(lines), string(rawLog))
	}
	for i, line := range lines {
		parts := strings.Split(line, "\t")
		if len(parts) != 4 {
			t.Fatalf("line %d malformed: %q", i, line)
		}
		gotDir, gotDB, gotEnv, gotArgs := parts[0], parts[1], parts[2], parts[3]
		if gotDir != beadsDir {
			t.Errorf("call %d BEADS_DIR = %q, want %q", i, gotDir, beadsDir)
		}
		if gotDB != "" {
			t.Errorf("call %d BEADS_DB = %q, want scrubbed", i, gotDB)
		}
		if gotEnv != "" {
			t.Errorf("call %d BD_JSON_ENVELOPE = %q, want scrubbed", i, gotEnv)
		}
		if !strings.HasPrefix(gotArgs, "show --json --readonly -- ") && !strings.HasPrefix(gotArgs, "recall --json --readonly -- ") {
			t.Errorf("call %d unexpected read-only args: %q", i, gotArgs)
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.String()
	}()
	defer func() {
		os.Stdout = orig
		_ = w.Close()
		_ = r.Close()
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	out := <-done
	_ = r.Close()
	return out
}
