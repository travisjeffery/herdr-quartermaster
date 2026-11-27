package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadConfigVerifierValidation(t *testing.T) {
	writeCfg := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		t.Setenv("QUARTERMASTER_CONFIG_DIR", dir)
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("default strict config loads cleanly", func(t *testing.T) {
		writeCfg(t, "repo = \"/tmp/repo\"\n")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig failed: %v", err)
		}
		if cfg.Verifier.isLiveReadOnly() {
			t.Fatalf("default mode should stay strict, got %q", cfg.Verifier.effectiveMode())
		}
	})

	t.Run("opt-in live-readonly agy config loads and expands deny_paths", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeCfg(t, `
repo = "/tmp/repo"

[verifier]
backend = "agy"
mode = "live-readonly"
model = "example-model"
allow_commands = [
  "aws --profile viewer * describe-*",
  "aws --profile viewer * list-*",
  "cf --profile personal * get",
  "deployer get environments",
  "kubectl logs",
  "dig",
]
allow_http = true
allow_domains = ["example.com", "status.example.org"]
allow_mcp = ["grafana/query_prometheus", "victorialogs/query"]
deny_commands = ["helm"]
deny_mcp = ["slack/*"]
deny_paths = ["~/private-tokens"]
max_rounds = 3
minutes = 10
`)
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig failed: %v", err)
		}
		if !cfg.Verifier.isLiveReadOnly() || cfg.Verifier.Backend != "agy" || cfg.Verifier.Model != "example-model" {
			t.Fatalf("unexpected verifier config: %+v", cfg.Verifier)
		}
		wantPath := filepath.Join(home, "private-tokens")
		if !slices.Equal(cfg.Verifier.DenyPaths, []string{wantPath}) {
			t.Fatalf("DenyPaths = %v, want [%s]", cfg.Verifier.DenyPaths, wantPath)
		}
		argv, err := cfg.Verifier.command()
		if err != nil || len(argv) == 0 || argv[0] != "agy" {
			t.Fatalf("command() = %v, %v", argv, err)
		}
	})

	t.Run("opt-in live-readonly claude config expands mcp_config", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeCfg(t, `
repo = "/tmp/repo"

[verifier]
backend = "claude"
mode = "live-readonly"
allow_commands = ["kubectl logs"]
allow_mcp = ["victorialogs/query"]
mcp_config = "~/.config/quartermaster/verifier-mcp.json"
`)
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig failed: %v", err)
		}
		wantMCP := filepath.Join(home, ".config/quartermaster/verifier-mcp.json")
		if cfg.Verifier.MCPConfig != wantMCP {
			t.Fatalf("MCPConfig = %q, want %q", cfg.Verifier.MCPConfig, wantMCP)
		}
	})

	invalid := map[string]struct {
		toml string
		want string
	}{
		"allow_commands without live-readonly mode": {
			toml: "repo = \"/tmp/repo\"\n[verifier]\nallow_commands = [\"dig\"]\n",
			want: `require mode = "live-readonly"`,
		},
		"allow_http without live-readonly mode": {
			toml: "repo = \"/tmp/repo\"\n[verifier]\nallow_http = true\n",
			want: `require mode = "live-readonly"`,
		},
		"pi backend rejects live-readonly": {
			toml: "repo = \"/tmp/repo\"\n[verifier]\nbackend = \"pi\"\nmode = \"live-readonly\"\n",
			want: "strictly filesystem-only",
		},
		"custom command rejects live-readonly": {
			toml: "repo = \"/tmp/repo\"\n[verifier]\ncommand = [\"/bin/true\"]\nmode = \"live-readonly\"\n",
			want: "cannot be combined",
		},
		"mutating allow_commands rejected": {
			toml: "repo = \"/tmp/repo\"\n[verifier]\nbackend = \"agy\"\nmode = \"live-readonly\"\nallow_commands = [\"kubectl delete\"]\n",
			want: "mutating subcommand",
		},
		"wildcard allow_mcp rejected": {
			toml: "repo = \"/tmp/repo\"\n[verifier]\nbackend = \"agy\"\nmode = \"live-readonly\"\nallow_mcp = [\"grafana/*\"]\n",
			want: "wildcards are forbidden",
		},
		"unknown mode rejected": {
			toml: "repo = \"/tmp/repo\"\n[verifier]\nmode = \"permissive\"\n",
			want: "unknown verifier mode",
		},
		"negative max_rounds rejected": {
			toml: "repo = \"/tmp/repo\"\n[verifier]\nmax_rounds = -1\n",
			want: "max_rounds",
		},
	}
	for name, tc := range invalid {
		t.Run("reject/"+name, func(t *testing.T) {
			writeCfg(t, tc.toml)
			if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("loadConfig() err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestLoadConfigIgnoreChecksValidation(t *testing.T) {
	writeCfg := func(t *testing.T, body string) {
		t.Helper()
		dir := t.TempDir()
		t.Setenv("QUARTERMASTER_CONFIG_DIR", dir)
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("defaults empty and fails closed", func(t *testing.T) {
		writeCfg(t, "repo = \"/tmp/repo\"\n")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig failed: %v", err)
		}
		if len(cfg.IgnoreChecks) != 0 {
			t.Fatalf("IgnoreChecks = %v, want empty", cfg.IgnoreChecks)
		}
	})

	t.Run("explicit empty list loads cleanly", func(t *testing.T) {
		writeCfg(t, "repo = \"/tmp/repo\"\nignore_checks = []\n")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig failed: %v", err)
		}
		if len(cfg.IgnoreChecks) != 0 {
			t.Fatalf("IgnoreChecks = %v, want empty", cfg.IgnoreChecks)
		}
	})

	t.Run("exact names and valid globs load cleanly", func(t *testing.T) {
		writeCfg(t, `
repo = "/tmp/repo"
ignore_checks = [
  "* (report-only)",
  "advisory/*",
  "Coverage Report",
  "check-[0-9]",
  "build [linux]",
]
`)
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig failed: %v", err)
		}
		want := []string{"* (report-only)", "advisory/*", "Coverage Report", "check-[0-9]", "build [linux]"}
		if !slices.Equal(cfg.IgnoreChecks, want) {
			t.Fatalf("IgnoreChecks = %v, want %v", cfg.IgnoreChecks, want)
		}
	})

	invalid := map[string]struct {
		toml string
		want string
	}{
		"empty pattern": {
			toml: "repo = \"/tmp/repo\"\nignore_checks = [\"\"]\n",
			want: "ignore_checks",
		},
		"whitespace-only pattern": {
			toml: "repo = \"/tmp/repo\"\nignore_checks = [\"   \"]\n",
			want: "ignore_checks",
		},
		"unclosed bracket": {
			toml: "repo = \"/tmp/repo\"\nignore_checks = [\"[unclosed\"]\n",
			want: "ignore_checks",
		},
		"unclosed bracket after prefix": {
			toml: "repo = \"/tmp/repo\"\nignore_checks = [\"scan-[report-only\"]\n",
			want: "ignore_checks",
		},
		"empty character class": {
			toml: "repo = \"/tmp/repo\"\nignore_checks = [\"scan-[]\"]\n",
			want: "ignore_checks",
		},
		"trailing escape": {
			toml: "repo = \"/tmp/repo\"\nignore_checks = [\"scan\\\\\"]\n",
			want: "ignore_checks",
		},
	}
	for name, tc := range invalid {
		t.Run("reject/"+name, func(t *testing.T) {
			writeCfg(t, tc.toml)
			if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("loadConfig() err = %v, want substring %q", err, tc.want)
			}
		})
	}
}
