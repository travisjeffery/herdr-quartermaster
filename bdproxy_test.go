package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"show backend-1", []string{"show", "backend-1"}},
		{"  note  x 'two words' ", []string{"note", "x", "two words"}},
		{`'it'\''s' ''`, []string{"it's", ""}},
		{`"a \"b\" \\ \$c" d\ e`, []string{`a "b" \ $c`, "d e"}},
		{"'line one\nline two'", []string{"line one\nline two"}},
		{`'$(rm -rf ~); ` + "`x`" + ` | & > <'`, []string{"$(rm -rf ~); `x` | & > <"}},
		{"", nil},
	} {
		got, err := splitCommand(tc.in)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("splitCommand(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{
		"show x; dolt push", "show $(id)", "show `id`", "show x | sh", "show x > f",
		`"$HOME"`, "'open", `"open`, `trailing\`, "show *", "show ~/x",
	} {
		if got, err := splitCommand(in); err == nil {
			t.Errorf("splitCommand(%q) = %q, want an error", in, got)
		}
	}
}

func TestPlanBDAllowsWorkerCommands(t *testing.T) {
	for _, tc := range []struct {
		words     []string
		wantArgv  []string
		wantActor string
	}{
		{[]string{"show", "b-1"}, []string{"--actor", "server", "show", "b-1"}, "server"},
		{[]string{"bd", "--actor", "claude", "--json", "note", "b-1", "done"}, []string{"--actor", "claude", "--json", "note", "b-1", "done"}, "claude"},
		{[]string{"--actor=codex", "-q", "update", "b-1", "--claim"}, []string{"--actor", "codex", "-q", "update", "b-1", "--claim"}, "codex"},
		{[]string{"--actor", "a", "--actor", "b", "prime"}, []string{"--actor", "b", "prime"}, "b"},
		// After "--" a word is an argument, not a flag.
		{[]string{"note", "b-1", "--", "--db", "-C"}, []string{"--actor", "server", "note", "b-1", "--", "--db", "-C"}, "server"},
		{[]string{"codex-hook", "SessionStart"}, []string{"--actor", "server", "codex-hook", "SessionStart"}, "server"},
		{[]string{"create", "t", "--body-file", "-"}, []string{"--actor", "server", "create", "t", "--body-file", "-"}, "server"},
	} {
		req, err := planBD("server", tc.words)
		if err != nil {
			t.Errorf("planBD(%q) = %v", tc.words, err)
			continue
		}
		if !slices.Equal(req.Argv, tc.wantArgv) || req.Actor != tc.wantActor {
			t.Errorf("planBD(%q) = %q as %q; want %q as %q", tc.words, req.Argv, req.Actor, tc.wantArgv, tc.wantActor)
		}
	}
}

func TestPlanBDRefuses(t *testing.T) {
	for _, words := range [][]string{
		nil,
		{"--json"},
		{"--actor"},
		{"dolt", "push"},
		{"sync"},
		{"init"},
		{"sql", "drop table issues"},
		{"config", "set", "x", "y"},
		{"delete", "b-1"},
		{"flatten"},
		{"gc"},
		{"edit", "b-1"},
		{"--db", "/tmp/x", "show", "b-1"},
		{"-C", "/tmp", "show", "b-1"},
		{"show", "b-1", "--db", "/tmp/x"},
		{"show", "b-1", "--db=/tmp/x"},
		{"show", "b-1", "--database", "other"},
		{"show", "b-1", "--directory=/tmp"},
		{"show", "b-1", "-qC/tmp"},
		{"list", "--global"},
		{"update", "b-1", "--dolt-auto-commit", "off"},
		{"list", "--mem-profile", "/home/me/.bashrc"},
		{"note", "b-1", "x", "--actor", "someone"},
	} {
		if req, err := planBD("server", words); err == nil {
			t.Errorf("planBD(%q) = %q, want a refusal", words, req.Argv)
		}
	}
}

func TestBDProxyEnvPinsBeads(t *testing.T) {
	env := bdProxyEnv([]string{"PATH=/bin", "BEADS_DIR=/elsewhere", "BEADS_DB=/x.db", "HOME=/h"}, "/repo/.beads", "claude")
	want := []string{"PATH=/bin", "HOME=/h", "BEADS_DIR=/repo/.beads", "BEADS_ACTOR=claude"}
	if !slices.Equal(env, want) {
		t.Errorf("bdProxyEnv = %q, want %q", env, want)
	}
}

// TestBDProxyHelper is the forced command in TestBDShimThroughProxy's
// round trip: the test binary re-run as quartermaster.
func TestBDProxyHelper(t *testing.T) {
	if os.Getenv("QM_BDPROXY_HELPER") != "1" {
		t.Skip("helper process")
	}
	// TestMain scrubbed the real names; the round trip passes its own.
	t.Setenv("QUARTERMASTER_CONFIG_DIR", os.Getenv("QM_TEST_CONFIG_DIR"))
	t.Setenv("QUARTERMASTER_STATE_DIR", os.Getenv("QM_TEST_STATE_DIR"))
	err := run(strings.Fields(os.Getenv("QM_BDPROXY_ARGS")))
	os.Stderr.WriteString("proxy: " + err.Error() + "\n")
	os.Exit(3)
}

// TestBDShimThroughProxy runs scripts/bd-shim with a fake ssh that hands the
// joined command to bd-proxy as sshd would, and a fake bd that prints what it
// got: arguments, environment and stdin survive the trip unchanged.
func TestBDShimThroughProxy(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	shim, err := filepath.Abs("scripts/bd-shim")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "config")
	stateDir := filepath.Join(dir, "state")
	for _, d := range []string{cfgDir, stateDir, filepath.Join(dir, "bin")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(cfgDir, "config.toml"), "repo = \"/repo\"\nbeads_dir = \"/repo/.beads\"\n", 0o644)
	key := filepath.Join(dir, "beads-key")
	writeFile(t, key, "not a key\n", 0o600)
	fakeBD := filepath.Join(dir, "fake-bd")
	writeFile(t, fakeBD, `#!/bin/sh
for a in "$@"; do printf '[%s]' "$a"; done
printf '\nBEADS_DIR=%s BEADS_ACTOR=%s\n' "$BEADS_DIR" "$BEADS_ACTOR"
cat
exit 7
`, 0o755)
	// The fake ssh skips its options up to "--", checks the destination, and
	// joins the rest with spaces into SSH_ORIGINAL_COMMAND, as ssh and sshd do.
	writeFile(t, filepath.Join(dir, "bin", "ssh"), `#!/bin/sh
while [ "$1" != "--" ]; do shift; done
shift
[ "$1" = "desktop.example" ] || { echo "ssh: wrong target $1" >&2; exit 255; }
shift
SSH_ORIGINAL_COMMAND="$*" exec "$QM_TEST_BIN" -test.run='^TestBDProxyHelper$'
`, 0o755)

	shimRun := func(actor string, stdin string, args ...string) (string, string, int) {
		cmd := exec.Command("bash", append([]string{shim}, args...)...)
		cmd.Env = append(os.Environ(),
			"PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"),
			"QM_TEST_BIN="+os.Args[0],
			"QM_BDPROXY_HELPER=1",
			"QM_BDPROXY_ARGS=bd-proxy --host server --bd "+fakeBD,
			"QM_TEST_CONFIG_DIR="+cfgDir,
			"QM_TEST_STATE_DIR="+stateDir,
			"QM_BEADS_TARGET=desktop.example",
			"QM_BEADS_KEY="+key,
			"BEADS_ACTOR="+actor,
			"BEADS_DIR=/wrong",
		)
		cmd.Stdin = strings.NewReader(stdin)
		var out, errb strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return out.String(), errb.String(), code
	}

	note := "it's done:\n$(rm -rf ~); `id` | tee > x & \"quoted\" \\ *"
	out, errOut, code := shimRun("claude", "body from stdin\n", "note", "b-1", note, "")
	want := "[--actor][claude][note][b-1][" + note + "][]\nBEADS_DIR=/repo/.beads BEADS_ACTOR=claude\nbody from stdin\n"
	if out != want || code != 7 {
		t.Errorf("shim note = %q (exit %d, stderr %q)\nwant %q (exit 7)", out, code, errOut, want)
	}

	// Without BEADS_ACTOR the host name is the actor.
	out, _, _ = shimRun("", "", "show", "b-1")
	if !strings.HasPrefix(out, "[--actor][server][show][b-1]\n") || !strings.Contains(out, "BEADS_ACTOR=server") {
		t.Errorf("shim without actor = %q, want the host as actor", out)
	}

	out, errOut, code = shimRun("claude", "", "dolt", "push")
	if out != "" || code != 3 || !strings.Contains(errOut, "bd dolt is not allowed") {
		t.Errorf("shim dolt push = %q (exit %d, stderr %q), want a refusal", out, code, errOut)
	}

	log, err := os.ReadFile(filepath.Join(stateDir, "bd-proxy.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{`host=server actor="claude" cmd="note" allowed`, `host=server actor="server" cmd="show" allowed`, `host=server actor="claude" cmd="dolt" denied: bd dolt is not allowed`} {
		if !strings.Contains(string(log), line) {
			t.Errorf("bd-proxy.log lacks %q:\n%s", line, log)
		}
	}
	if strings.Contains(string(log), "rm -rf") {
		t.Errorf("bd-proxy.log records arguments:\n%s", log)
	}
}

func TestBDProxyRejectsBadHost(t *testing.T) {
	for _, host := range []string{"", "Server", "a b", "../x", "-x"} {
		if err := bdProxy(Config{BeadsDir: "/b"}, []string{"--host", host}); err == nil || !strings.Contains(err.Error(), "not a host name") {
			t.Errorf("bdProxy(--host %q) = %v, want a host-name error", host, err)
		}
	}
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
