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

func TestLoadConfigReviewValidation(t *testing.T) {
	writeCfg := func(t *testing.T, body string) {
		t.Helper()
		dir := t.TempDir()
		t.Setenv("QUARTERMASTER_CONFIG_DIR", dir)
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("defaults load cleanly", func(t *testing.T) {
		writeCfg(t, "repo = \"/tmp/repo\"\n")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig failed: %v", err)
		}
		if len(cfg.Review.SecurityPaths) != 0 {
			t.Fatalf("SecurityPaths = %v, want empty", cfg.Review.SecurityPaths)
		}
		if got := cfg.Review.effectiveSecurityRole(); got != defaultReviewSecurityRole {
			t.Fatalf("effectiveSecurityRole() = %q, want %q", got, defaultReviewSecurityRole)
		}
		if !cfg.Review.autoTriggerEnabled() {
			t.Fatal("autoTriggerEnabled() = false, want true by default")
		}
	})

	t.Run("valid security_paths, custom security_role, and auto_trigger=false load cleanly", func(t *testing.T) {
		writeCfg(t, `
repo = "/tmp/repo"

[review]
security_paths = [
  "**/auth/**",
  "**/iam/**",
  "**/.github/workflows/**",
  "**/migrations/**",
  "infra/*.tf",
]
security_role = "sec-reviewer"
auto_trigger = false
`)
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig failed: %v", err)
		}
		wantPaths := []string{
			"**/auth/**",
			"**/iam/**",
			"**/.github/workflows/**",
			"**/migrations/**",
			"infra/*.tf",
		}
		if !slices.Equal(cfg.Review.SecurityPaths, wantPaths) {
			t.Fatalf("SecurityPaths = %v, want %v", cfg.Review.SecurityPaths, wantPaths)
		}
		if got := cfg.Review.effectiveSecurityRole(); got != "sec-reviewer" {
			t.Fatalf("effectiveSecurityRole() = %q, want sec-reviewer", got)
		}
		if cfg.Review.autoTriggerEnabled() {
			t.Fatal("autoTriggerEnabled() = true, want false")
		}
	})

	invalid := map[string]struct {
		toml string
		want string
	}{
		"empty security_paths entry": {
			toml: "repo = \"/tmp/repo\"\n[review]\nsecurity_paths = [\"\"]\n",
			want: "security_paths",
		},
		"whitespace security_paths entry": {
			toml: "repo = \"/tmp/repo\"\n[review]\nsecurity_paths = [\"   \"]\n",
			want: "security_paths",
		},
		"surrounding whitespace security_paths entry": {
			toml: "repo = \"/tmp/repo\"\n[review]\nsecurity_paths = [\" **/auth/** \"]\n",
			want: "surrounding whitespace",
		},
		"slashes-only security_paths entry": {
			toml: "repo = \"/tmp/repo\"\n[review]\nsecurity_paths = [\"///\"]\n",
			want: "no path segments",
		},
		"malformed glob bracket": {
			toml: "repo = \"/tmp/repo\"\n[review]\nsecurity_paths = [\"**/auth/[unclosed\"]\n",
			want: "security_paths",
		},
		"invalid security_role uppercase": {
			toml: "repo = \"/tmp/repo\"\n[review]\nsecurity_role = \"SecurityReview\"\n",
			want: "invalid security_role",
		},
		"invalid security_role spaces": {
			toml: "repo = \"/tmp/repo\"\n[review]\nsecurity_role = \"security review\"\n",
			want: "invalid security_role",
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

func TestMatchPathGlob(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/auth/**", "auth/login.go", true},
		{"**/auth/**", "pkg/auth/login.go", true},
		{"**/auth/**", "services/api/auth/v2/jwt.go", true},
		{"**/auth/**", "pkg/authz/login.go", false},
		{"**/auth/**", "myauth/login.go", false},
		{"**/iam/**", "infra/iam/policy.tf", true},
		{"**/iam/**", "iam/role.json", true},
		{"**/.github/workflows/**", ".github/workflows/ci.yml", true},
		{"**/.github/workflows/**", "sub/.github/workflows/deploy/prod.yml", true},
		{"**/.github/workflows/**", ".github/CODEOWNERS", false},
		{"**/migrations/**", "migrations/001_init.sql", true},
		{"**/migrations/**", "db/migrations/2026/up.sql", true},
		{"**/migrations/*.sql", "db/migrations/001.sql", true},
		{"**/migrations/*.sql", "db/migrations/sub/001.sql", false},
		{"**/*.tf", "main.tf", true},
		{"**/*.tf", "infra/network/main.tf", true},
		{"*.tf", "main.tf", true},
		{"*.tf", "infra/main.tf", false},
		{"infra/iam/**", "infra/iam/role.tf", true},
		{"infra/iam/**", "other/infra/iam/role.tf", false},
		{"**/auth/**", "./pkg/auth/token.go", true},
		{"**/auth/**", "", false},
	}
	for _, tc := range cases {
		if got := matchPathGlob(tc.pattern, tc.path); got != tc.want {
			t.Errorf("matchPathGlob(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestLoadConfigCouncilsValidation(t *testing.T) {
	writeCfg := func(t *testing.T, body string) {
		t.Helper()
		dir := t.TempDir()
		t.Setenv("QUARTERMASTER_CONFIG_DIR", dir)
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("defaults empty and loads cleanly", func(t *testing.T) {
		writeCfg(t, "repo = \"/tmp/repo\"\n")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig failed: %v", err)
		}
		if len(cfg.Councils) != 0 {
			t.Fatalf("Councils = %v, want empty", cfg.Councils)
		}
	})

	t.Run("valid panel with custom and built-in roles and defaults loads cleanly", func(t *testing.T) {
		writeCfg(t, `
repo = "/tmp/repo"

[roles.planner-a]
worktree = false
agent = "pi"
model = "provider/model-a"
brief = "investigator"
exclude_tools = ["edit", "write"]

[roles.planner-b]
worktree = false
agent = "pi"
model = "provider/model-b"
brief = "investigator"
exclude_tools = ["edit", "write"]

[roles.planner-chair]
worktree = false
agent = "pi"
model = "provider/model-chair"
brief = "investigator"
exclude_tools = ["edit", "write"]

[councils.default]
members = ["planner-a", "planner-b", "investigator"]
chair = "planner-chair"

[councils.fast]
members = ["planner-a", "planner-b"]
chair = "planner-a"
anonymize = false
min_quorum = 1
timeout_minutes = 10
auto_cleanup = false
`)
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig failed: %v", err)
		}
		def, ok := cfg.Councils["default"]
		if !ok {
			t.Fatal("missing default council")
		}
		if !def.anonymizeEnabled() {
			t.Fatal("default anonymize should be true")
		}
		if !def.autoCleanupEnabled() {
			t.Fatal("default auto_cleanup should be true")
		}
		if got := def.effectiveMinQuorum(); got != 3 {
			t.Fatalf("effectiveMinQuorum() = %d, want 3", got)
		}
		if got := def.effectiveTimeoutMinutes(); got != 20 {
			t.Fatalf("effectiveTimeoutMinutes() = %d, want 20", got)
		}

		fast, ok := cfg.Councils["fast"]
		if !ok {
			t.Fatal("missing fast council")
		}
		if fast.anonymizeEnabled() {
			t.Fatal("fast anonymize should be false")
		}
		if fast.autoCleanupEnabled() {
			t.Fatal("fast auto_cleanup should be false")
		}
		if got := fast.effectiveMinQuorum(); got != 1 {
			t.Fatalf("fast effectiveMinQuorum() = %d, want 1", got)
		}
		if got := fast.effectiveTimeoutMinutes(); got != 10 {
			t.Fatalf("fast effectiveTimeoutMinutes() = %d, want 10", got)
		}
	})

	rolesHeader := `
repo = "/tmp/repo"

[roles.planner-a]
worktree = false
brief = "investigator"

[roles.planner-b]
worktree = false
brief = "investigator"

[roles.planner-chair]
worktree = false
brief = "investigator"
`

	invalid := map[string]struct {
		toml string
		want string
	}{
		"invalid council name": {
			toml: rolesHeader + "\n[councils.\"Bad Name\"]\nmembers = [\"planner-a\", \"planner-b\"]\nchair = \"planner-chair\"\n",
			want: "invalid council name",
		},
		"missing members": {
			toml: rolesHeader + "\n[councils.default]\nchair = \"planner-chair\"\n",
			want: "at least 2 unique roles",
		},
		"single member": {
			toml: rolesHeader + "\n[councils.default]\nmembers = [\"planner-a\"]\nchair = \"planner-chair\"\n",
			want: "at least 2 unique roles",
		},
		"duplicate members": {
			toml: rolesHeader + "\n[councils.default]\nmembers = [\"planner-a\", \"planner-a\"]\nchair = \"planner-chair\"\n",
			want: "duplicate member role",
		},
		"empty member role": {
			toml: rolesHeader + "\n[councils.default]\nmembers = [\"planner-a\", \"\"]\nchair = \"planner-chair\"\n",
			want: "must be non-empty",
		},
		"whitespace member role": {
			toml: rolesHeader + "\n[councils.default]\nmembers = [\"planner-a\", \" planner-b \"]\nchair = \"planner-chair\"\n",
			want: "surrounding whitespace",
		},
		"unknown member role": {
			toml: rolesHeader + "\n[councils.default]\nmembers = [\"planner-a\", \"does-not-exist\"]\nchair = \"planner-chair\"\n",
			want: "unknown role",
		},
		"missing chair": {
			toml: rolesHeader + "\n[councils.default]\nmembers = [\"planner-a\", \"planner-b\"]\n",
			want: "chair role is required",
		},
		"unknown chair role": {
			toml: rolesHeader + "\n[councils.default]\nmembers = [\"planner-a\", \"planner-b\"]\nchair = \"mystery-chair\"\n",
			want: "unknown role",
		},
		"explicit zero min_quorum": {
			toml: rolesHeader + "\n[councils.default]\nmembers = [\"planner-a\", \"planner-b\"]\nchair = \"planner-chair\"\nmin_quorum = 0\n",
			want: "min_quorum",
		},
		"negative min_quorum": {
			toml: rolesHeader + "\n[councils.default]\nmembers = [\"planner-a\", \"planner-b\"]\nchair = \"planner-chair\"\nmin_quorum = -1\n",
			want: "min_quorum",
		},
		"min_quorum exceeds member count": {
			toml: rolesHeader + "\n[councils.default]\nmembers = [\"planner-a\", \"planner-b\"]\nchair = \"planner-chair\"\nmin_quorum = 3\n",
			want: "min_quorum",
		},
		"explicit zero timeout_minutes": {
			toml: rolesHeader + "\n[councils.default]\nmembers = [\"planner-a\", \"planner-b\"]\nchair = \"planner-chair\"\ntimeout_minutes = 0\n",
			want: "timeout_minutes",
		},
		"negative timeout_minutes": {
			toml: rolesHeader + "\n[councils.default]\nmembers = [\"planner-a\", \"planner-b\"]\nchair = \"planner-chair\"\ntimeout_minutes = -5\n",
			want: "timeout_minutes",
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
