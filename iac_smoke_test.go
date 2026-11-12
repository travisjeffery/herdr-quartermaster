package main

import (
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIACSmoke applies through the executor with the real pulumi and tofu,
// against local backends: QM_IAC_SMOKE=1 go test -run IACSmoke. It needs
// both on PATH (for mise's tofu shim, MISE_OPENTOFU_VERSION set).
func TestIACSmoke(t *testing.T) {
	if os.Getenv("QM_IAC_SMOKE") == "" {
		t.Skip("QM_IAC_SMOKE unset")
	}
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	t.Setenv("PULUMI_CONFIG_PASSPHRASE", "smoke")
	back := t.TempDir()
	repo := t.TempDir()
	run := func(dir string, env []string, name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), append(env, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v: %s", name, args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pulumiEnv := []string{"PULUMI_BACKEND_URL=file://" + back, "PULUMI_SKIP_UPDATE_CHECK=true"}
	run(repo, nil, "git", "init", "--quiet")
	run(repo, nil, "git", "remote", "add", "origin", "https://example.com/acme/smoke.git")
	write(filepath.Join(repo, "pulumi", "Pulumi.yaml"), "name: smoke\nruntime: yaml\noutputs:\n  greeting: hello\n")
	run(filepath.Join(repo, "pulumi"), pulumiEnv, "pulumi", "stack", "init", "dev", "--non-interactive")
	write(filepath.Join(repo, "tofu", "main.tf"), `terraform {
  backend "local" {
    path = "`+filepath.Join(back, "tofu.tfstate")+`"
  }
}
resource "terraform_data" "x" {
  input = "a"
}
`)
	run(repo, nil, "git", "add", ".")
	run(repo, nil, "git", "commit", "--quiet", "-m", "smoke")
	rev := run(repo, nil, "git", "rev-parse", "HEAD")

	bead := Bead{ID: "b-1", Status: StatusInProgress, Labels: []string{"autonomy:9"}}
	x := &executor{
		cfg:     defaultConfig(),
		show:    func(string) (Bead, error) { return bead, nil },
		active:  func() ([]Bead, error) { return nil, nil },
		needsMe: func(id, note string) error { t.Errorf("needs_me: %s", note); return nil },
		runners: map[string]opRunner{"iac.apply": iacRunner{run: toolExec}},
		now:     time.Now,
		sleep:   func(context.Context, time.Duration) {},
		logf:    log.New(io.Discard, "", 0).Printf,
	}
	for _, c := range []struct{ target, dir, expect, env string }{
		{"pulumi:organization/smoke/dev", "pulumi", "create=1", `{"PULUMI_BACKEND_URL":"file://` + back + `"}`},
		{"tofu:smoke/tofu", "tofu", "create=1", ""},
	} {
		t.Run(c.dir, func(t *testing.T) {
			s := Step{Op: "iac.apply", Target: c.target, Args: map[string]string{"repo": repo, "dir": c.dir, "expect": c.expect, "env": c.env}, Pre: map[string]string{"rev": rev}}
			if tool, stack, _ := strings.Cut(c.target, ":"); tool == "pulumi" {
				s.Args["stack"] = stack
			}
			dropEmpty(s.Args)
			resp := x.submit(context.Background(), "b-1", mustJSON(t, Plan{Steps: []Step{s}}))
			if resp.State != planDone || !strings.Contains(resp.Output, "applied "+c.target) {
				t.Fatalf("first apply: %+v", resp)
			}
			t.Logf("%s", resp.Output)
			p, digest, err := loadPlan(resp.Digest)
			if err != nil {
				t.Fatal(err)
			}
			// The probe sees it applied. The checkout is gone, so probe from a
			// fresh preparation of the same commit.
			r := iacRunner{run: toolExec}
			again := p.Steps[0]
			again.Args = map[string]string{}
			for k, v := range p.Steps[0].Args {
				if k != "workdir" && k != "preview" && k != "planfile" && k != "cas" {
					again.Args[k] = v
				}
			}
			again.Pre = map[string]string{"rev": rev}
			if err := r.prepare(context.Background(), &again); err != nil {
				t.Fatal(err)
			}
			if happened, known, ev, err := r.probe(context.Background(), again, stepTag(digest, 1)); err != nil || !known || !happened {
				t.Errorf("probe: %v %v %q %v", happened, known, ev, err)
			}
			// Applying the same commit again changes nothing.
			s.Args["expect"] = "create=0"
			if resp := x.submit(context.Background(), "b-1", mustJSON(t, Plan{Steps: []Step{s}})); resp.State != planDone {
				t.Errorf("second apply: %+v", resp)
			}
		})
	}
}
