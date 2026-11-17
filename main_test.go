package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain points every directory this tool could touch at a scratch home, so
// no test reaches the live config or state: HOME alone is not enough, since
// configBase prefers XDG_CONFIG_HOME, and an unisolated migration test
// once moved a live config dir. Tests that need other dirs set their own.
func TestMain(m *testing.M) {
	// Protocol fixtures are already isolated by their parent test. Avoid
	// replacing their private supervisor state, or writing Go's PASS to RPC.
	if os.Getenv("QM_RPC_SUPERVISOR") == "1" && os.Getenv("QM_RPC_CHILD") != "1" {
		TestRPCSupervisorProcess(nil)
	}
	if os.Getenv("QM_RPC_CHILD") == "1" {
		TestRPCChildProcess(nil)
	}
	home, err := os.MkdirTemp("", "quartermaster-test-home")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	for _, suffix := range []string{"CONFIG_DIR", "STATE_DIR"} {
		for _, k := range envNames(suffix) {
			os.Unsetenv(k)
		}
	}
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// A fresh install has no config yet; version and help must still work.
func TestRunVersionWithoutConfig(t *testing.T) {
	t.Setenv("QUARTERMASTER_CONFIG_DIR", t.TempDir())
	for _, arg := range []string{"version", "--version", "-V", "help"} {
		if err := run([]string{arg}); err != nil {
			t.Errorf("run(%q) = %v, want nil without a config", arg, err)
		}
	}
	if err := run([]string{"report"}); err == nil {
		t.Error("run(report) without a config succeeded, want errNoRepo")
	}
}
