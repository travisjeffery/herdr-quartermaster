package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIAC answers lowering's git and pulumi questions for a repository at
// /repo (origin acme/app) whose HEAD is sha.
type fakeIAC struct {
	dirty, untracked string
	env              map[string]string
	files            map[string]string
}

func (f *fakeIAC) lookup(dir string) iacLookup {
	return iacLookup{
		git: func(dir string, args ...string) (string, error) {
			switch strings.Join(args, " ") {
			case "rev-parse --show-toplevel":
				if !strings.HasPrefix(dir, "/repo") {
					return "", errors.New("not a git repository")
				}
				return "/repo", nil
			case "rev-parse HEAD":
				return sha, nil
			case "status --porcelain --untracked-files=no":
				return f.dirty, nil
			case "remote get-url origin":
				return "git@github.com:acme/app.git", nil
			}
			if strings.HasPrefix(strings.Join(args, " "), "status --porcelain --untracked-files=all") {
				return f.untracked, nil
			}
			return "", fmt.Errorf("unexpected git %v", args)
		},
		pulumiStack: func(dir, stack string) (string, error) {
			if stack == "" {
				stack = "dev"
			}
			if strings.Count(stack, "/") == 2 {
				return stack, nil
			}
			return "acme/app/" + stack, nil
		},
		readFile: func(p string) ([]byte, error) {
			if b, ok := f.files[p]; ok {
				return []byte(b), nil
			}
			return nil, os.ErrNotExist
		},
		getenv: func(k string) string { return f.env[k] },
		dir:    dir,
	}
}

func TestLowerIAC(t *testing.T) {
	cases := []struct {
		tool, argv string
		dir        string // the worker's directory, /repo/infra if empty
		fake       fakeIAC
		op         string // "" passes through
		target     string
		args       map[string]string
		wantErr    string
	}{
		{tool: "pulumi", argv: "up --yes -s staging", op: "iac.apply", target: "pulumi:acme/app/staging", args: map[string]string{"repo": "/repo", "dir": "infra", "stack": "acme/app/staging"}},
		{tool: "pulumi", argv: "up", op: "iac.apply", target: "pulumi:acme/app/dev"},
		{tool: "pulumi", argv: "--non-interactive -C infra update -y --stack=acme/app/prod --refresh -m ship -p 4 --diff", dir: "/repo", op: "iac.apply", target: "pulumi:acme/app/prod", args: map[string]string{"repo": "/repo", "dir": "infra", "stack": "acme/app/prod", "refresh": "true", "message": "ship", "parallel": "4"}},
		{tool: "pulumi", argv: "up --yes", fake: fakeIAC{env: map[string]string{"AWS_PROFILE": "dev", "AWS_SECRET_ACCESS_KEY": "no", expectEnv: "create=1,update=1"}}, op: "iac.apply", args: map[string]string{"repo": "/repo", "dir": "infra", "stack": "acme/app/dev", "env": `{"AWS_PROFILE":"dev"}`, "expect": "create=1,update=1"}},
		{tool: "pulumi", argv: "up --yes --target urn:x", op: opOpaque},
		{tool: "pulumi", argv: "up --replace urn:x", op: opOpaque},
		{tool: "pulumi", argv: "up -c region=us-east-1", op: opOpaque},
		{tool: "pulumi", argv: "up --plan p.json", op: opOpaque},
		{tool: "pulumi", argv: "up my-template", op: opOpaque},
		{tool: "pulumi", argv: "destroy --yes", op: opOpaque},
		{tool: "pulumi", argv: "--odd up", op: opOpaque},
		{tool: "pulumi", argv: "preview --json"},
		{tool: "pulumi", argv: "stack history"},
		{tool: "pulumi", argv: "config set region us-east-1"},
		{tool: "pulumi", argv: "--version"},
		{tool: "pulumi", argv: "up", fake: fakeIAC{dirty: " M infra/index.ts"}, wantErr: "uncommitted changes (M infra/index.ts)"},
		{tool: "pulumi", argv: "up", fake: fakeIAC{untracked: "?? infra/new.ts"}, wantErr: "uncommitted"},
		{tool: "pulumi", argv: "up", fake: fakeIAC{env: map[string]string{expectEnv: "lots"}}, wantErr: "QUARTERMASTER_EXPECT"},
		{tool: "pulumi", argv: "up", dir: "/elsewhere", wantErr: "isn't in a git repository"},

		{tool: "tofu", argv: "apply -auto-approve", op: "iac.apply", target: "tofu:app/infra", args: map[string]string{"repo": "/repo", "dir": "infra"}},
		{tool: "tofu", argv: "-chdir=infra apply -input=false -var region=us -var=a=b -var-file prod.tfvars --lock=false -parallelism=2", dir: "/repo", op: "iac.apply", target: "tofu:app/infra",
			args: map[string]string{"repo": "/repo", "dir": "infra", "vars": `["region=us","a=b"]`, "var_files": `["prod.tfvars"]`, "flags": `["-lock=false","-parallelism=2"]`}},
		{tool: "tofu", argv: "apply", dir: "/repo", op: "iac.apply", target: "tofu:app"},
		{tool: "tofu", argv: "apply", fake: fakeIAC{files: map[string]string{"/repo/infra/.terraform/environment": "staging\n"}}, op: "iac.apply", target: "tofu:app/infra/staging", args: map[string]string{"repo": "/repo", "dir": "infra", "env": `{"TF_WORKSPACE":"staging"}`}},
		{tool: "tofu", argv: "apply", fake: fakeIAC{env: map[string]string{"TF_WORKSPACE": "prod"}}, op: "iac.apply", target: "tofu:app/infra/prod"},
		{tool: "tofu", argv: "apply plan.tfplan", op: opOpaque},
		{tool: "tofu", argv: "apply -target=aws_s3_bucket.x", op: opOpaque},
		{tool: "tofu", argv: "apply -destroy", op: opOpaque},
		{tool: "tofu", argv: "apply -replace=x", op: opOpaque},
		{tool: "tofu", argv: "apply -lock false", op: opOpaque},
		{tool: "tofu", argv: "apply -var-file=/etc/secret.tfvars", op: opOpaque},
		{tool: "tofu", argv: "destroy -auto-approve", op: opOpaque},
		{tool: "tofu", argv: "plan -out=p"},
		{tool: "tofu", argv: "init"},
		{tool: "tofu", argv: "-version"},
		{tool: "tofu", argv: "state list"},
		{tool: "tofu", argv: "apply", fake: fakeIAC{dirty: " M modules/x/main.tf"}, wantErr: "commit them first"},
		{tool: "tofu", argv: "apply -var", wantErr: "needs a value"},
	}
	for _, c := range cases {
		t.Run(c.tool+" "+c.argv, func(t *testing.T) {
			dir := cmpOr(c.dir, "/repo/infra")
			l := c.fake.lookup(dir)
			args := strings.Fields(c.argv)
			lower := map[string]func([]string, iacLookup) (Step, bool, error){"pulumi": lowerPulumi, "tofu": lowerTofu}[c.tool]
			s, ok, err := lower(args, l)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.op == "" {
				if ok {
					t.Fatalf("lowered to %+v; want it passed through", s)
				}
				return
			}
			if !ok || s.Op != c.op {
				t.Fatalf("lowered to %v %+v, want %s", ok, s, c.op)
			}
			if c.op == opOpaque {
				var argv []string
				if err := json.Unmarshal([]byte(s.Args["argv"]), &argv); err != nil || strings.Join(argv, " ") != c.argv || s.Args["tool"] != c.tool || s.Args["dir"] != dir {
					t.Errorf("opaque args %v", s.Args)
				}
				if !strings.HasPrefix(s.Text, c.tool+" ") {
					t.Errorf("text %q", s.Text)
				}
				return
			}
			if c.target != "" && s.Target != c.target {
				t.Errorf("target %s, want %s", s.Target, c.target)
			}
			if s.Pre["rev"] != sha {
				t.Errorf("pre %v, want rev %s", s.Pre, sha)
			}
			if c.args != nil && fmt.Sprint(s.Args) != fmt.Sprint(c.args) {
				t.Errorf("args %v\nwant %v", s.Args, c.args)
			}
			// What the shim lowers is a plan the executor takes.
			if _, err := parsePlan(mustJSON(t, Plan{Bead: "b-1", Steps: []Step{s}})); err != nil {
				t.Errorf("lowered step doesn't parse: %v", err)
			}
		})
	}
}

// iacRepo is a git repository with an infra directory, committed, and its
// HEAD.
func iacRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet")
	git("remote", "add", "origin", "git@github.com:acme/app.git")
	if err := os.MkdirAll(filepath.Join(dir, "infra"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "infra", "main.tf"), []byte("# infra\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "--quiet", "-m", "infra")
	return dir, git("rev-parse", "HEAD")
}

// fakeIACTools runs git for real and stands in for pulumi and tofu: previews
// come from testdata, and applies and probes answer from its fields.
type fakeIACTools struct {
	t        *testing.T
	mu       sync.Mutex
	calls    []string
	preview  string // testdata preview a fresh pulumi preview prints
	noSave   bool   // pulumi preview --save-plan fails
	applyErr bool
	history  string // pulumi stack history --json
	planCode int    // tofu plan -detailed-exitcode
}

func (f *fakeIACTools) run(ctx context.Context, c toolCmd) (toolOut, error) {
	if c.Tool == "git" {
		return toolExec(ctx, c)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	line := c.Tool + " " + strings.Join(c.Args, " ")
	f.calls = append(f.calls, line)
	if !slices.Contains(c.Env, "PULUMI_EXPERIMENTAL=true") && c.Tool == "pulumi" || !slices.Contains(c.Env, "TF_IN_AUTOMATION=1") && c.Tool == "tofu" {
		f.t.Errorf("%s without its unattended env: %v", c.Tool, c.Env)
	}
	if _, err := os.Stat(c.Dir); err != nil {
		f.t.Errorf("%s in %s: %v", c.Tool, c.Dir, err)
	}
	flag := func(name string) string {
		for i, a := range c.Args {
			if v, ok := strings.CutPrefix(a, name+"="); ok {
				return v
			}
			if a == name && i+1 < len(c.Args) {
				return c.Args[i+1]
			}
		}
		return ""
	}
	ok := func(out string) (toolOut, error) { return toolOut{Stdout: []byte(out)}, nil }
	switch {
	case c.Tool == "pulumi" && c.Args[0] == "install":
		return ok("installed")
	case c.Tool == "pulumi" && c.Args[0] == "preview":
		if p := flag("--save-plan"); p != "" {
			if f.noSave {
				return toolOut{Code: 255, Stderr: []byte("unknown flag: --save-plan")}, nil
			}
			if err := os.WriteFile(p, []byte(`{"plan":1}`), 0o600); err != nil {
				return toolOut{}, err
			}
			return ok(string(readPlan(f.t, "pulumi-update.json")))
		}
		return ok(string(readPlan(f.t, cmpOr(f.preview, "pulumi-update.json"))))
	case c.Tool == "pulumi" && c.Args[0] == "up":
		if f.applyErr {
			return toolOut{Code: 1, Stderr: []byte("error: update failed")}, nil
		}
		f.history = fmt.Sprintf(`[{"message":%q,"result":"succeeded"}]`, flag("--message"))
		return ok("Resources: 1 created, 1 updated")
	case c.Tool == "pulumi" && c.Args[0] == "stack":
		return ok(cmpOr(f.history, "[]"))
	case c.Tool == "tofu" && c.Args[0] == "init":
		return ok("initialized")
	case c.Tool == "tofu" && c.Args[0] == "plan" && slices.Contains(c.Args, "-detailed-exitcode"):
		return toolOut{Code: f.planCode}, nil
	case c.Tool == "tofu" && c.Args[0] == "plan":
		if err := os.WriteFile(flag("-out"), []byte("tfplan"), 0o600); err != nil {
			return toolOut{}, err
		}
		return ok("Plan: 1 to add")
	case c.Tool == "tofu" && c.Args[0] == "show":
		return ok(string(readPlan(f.t, "tofu-update.json")))
	case c.Tool == "tofu" && c.Args[0] == "apply":
		if f.applyErr {
			return toolOut{Code: 1, Stderr: []byte("Error: Saved plan is stale")}, nil
		}
		f.planCode = 0
		return ok("Apply complete! Resources: 1 added, 1 changed")
	}
	return toolOut{Code: 1, Stderr: []byte("fake: unexpected " + line)}, nil
}

func (f *fakeIACTools) ran(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// iacStep is an apply of infra at rev on target: pulumi:<stack>, or
// tofu:app/infra/<workspace>.
func iacStep(repo, rev, target string) Step {
	s := Step{Op: "iac.apply", Target: target, Args: map[string]string{"repo": repo, "dir": "infra", "expect": "create=1,update=1", "env": `{"AWS_PROFILE":"dev","PATH":"/evil"}`}, Pre: map[string]string{"rev": rev}}
	tool, name, _ := strings.Cut(target, ":")
	if tool == "pulumi" {
		s.Args["stack"] = name
	} else if ws := strings.TrimPrefix(name, "app/infra/"); ws != name {
		s.Args["env"] = `{"AWS_PROFILE":"dev","PATH":"/evil","TF_WORKSPACE":"` + ws + `"}`
	}
	return s
}

func TestIACRunner(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	ctx := context.Background()
	repo, rev := iacRepo(t)
	// An uncommitted change in the worktree isn't what gets applied.
	if err := os.WriteFile(filepath.Join(repo, "infra", "main.tf"), []byte("# not committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tool := range []string{"tofu", "pulumi"} {
		t.Run(tool, func(t *testing.T) {
			f := &fakeIACTools{t: t, planCode: 2}
			r := iacRunner{run: f.run}
			s := iacStep(repo, rev, map[string]string{"pulumi": "pulumi:acme/app/staging", "tofu": "tofu:app/infra/staging"}[tool])
			if err := r.prepare(ctx, &s); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(s.Args["workdir"], iacDir()+"/") || s.Args["preview"] == "" || s.Args["planfile"] == "" || s.Pre["planfile_digest"] == "" {
				t.Fatalf("prepared %v %v", s.Args, s.Pre)
			}
			b, err := os.ReadFile(filepath.Join(iacProj(s), "main.tf"))
			if err != nil || string(b) != "# infra\n" {
				t.Errorf("checkout has %q, %v; want the commit's file", b, err)
			}
			for _, c := range f.calls {
				if strings.Contains(c, repo+"/infra") {
					t.Errorf("ran in the worktree: %s", c)
				}
			}
			// parsePlan binds the preview's digest, and the class is gate's.
			p, err := parsePlan(mustJSON(t, Plan{Bead: "b-1", Steps: []Step{s}}))
			if err != nil {
				t.Fatal(err)
			}
			s = p.Steps[0]
			cfg := defaultConfig()
			cfg.Autonomy.Prod = []string{"prod"}
			if c, why := cfg.stepClass(Bead{ID: "b-1"}, s); c != 6 || !strings.Contains(why, "as expected") {
				t.Errorf("class %d (%s), want 6 as expected", c, why)
			}
			// A production selector makes it production, whatever the target says.
			prodProfile := s
			prodProfile.Args = maps.Clone(s.Args)
			prodProfile.Args["env"] = `{"AWS_PROFILE":"prod"}`
			if c, why := cfg.stepClass(Bead{ID: "b-1"}, prodProfile); c != 7 || !strings.Contains(why, "AWS_PROFILE=prod") {
				t.Errorf("prod profile: class %d (%s), want 7", c, why)
			}
			if err := r.pre(ctx, s); err != nil {
				t.Fatalf("pre: %v", err)
			}
			if happened, known, _, err := r.probe(ctx, s, "qm:abc:1"); err != nil || !known || happened {
				t.Errorf("probe before: %v %v %v", happened, known, err)
			}
			ev, err := r.do(ctx, s, "qm:abc:1")
			if err != nil || !strings.Contains(ev, "applied "+s.Target+" at "+short(rev)+" (saved plan, qm:abc:1)") {
				t.Fatalf("do: %q, %v", ev, err)
			}
			applied := f.ran(tool + " " + map[string]string{"pulumi": "up", "tofu": "apply"}[tool])
			if len(applied) != 1 || !strings.Contains(applied[0], s.Args["planfile"]) {
				t.Errorf("applied %v, want the saved plan", applied)
			}
			if tool == "pulumi" && (!strings.Contains(applied[0], "--message qm:abc:1") || !strings.Contains(applied[0], "--stack acme/app/staging")) {
				t.Errorf("pulumi up %q isn't tagged on its stack", applied[0])
			}
			if happened, known, ev, err := r.probe(ctx, s, "qm:abc:1"); err != nil || !known || !happened {
				t.Errorf("probe after: %v %v %q %v", happened, known, ev, err)
			}
			// Only the carried selectors reach the tool.
			if env := iacEnvList(s); !slices.Contains(env, "AWS_PROFILE=dev") || slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, "PATH=") }) {
				t.Errorf("env %v", env)
			}

			// A changed preview or saved plan, or a checkout moved off rev, is stale.
			tamper := func(path string) {
				b, _ := os.ReadFile(path)
				if err := os.WriteFile(path, append(b, ' '), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := r.pre(ctx, s); !errors.Is(err, errStale) {
					t.Errorf("%s changed: %v", filepath.Base(path), err)
				}
				_ = os.WriteFile(path, b, 0o600)
			}
			tamper(s.Args["preview"])
			tamper(s.Args["planfile"])
			moved := s
			moved.Pre = map[string]string{"rev": strings.Repeat("e", 40), "plan_digest": s.Pre["plan_digest"], "planfile_digest": s.Pre["planfile_digest"]}
			if err := r.pre(ctx, moved); !errors.Is(err, errStale) {
				t.Errorf("checkout off rev: %v", err)
			}
			// A step the executor didn't prepare never applies.
			foreign := s
			foreign.Args = map[string]string{"workdir": repo, "preview": s.Args["preview"]}
			if err := r.pre(ctx, foreign); !errors.Is(err, errStale) {
				t.Errorf("foreign workdir: %v", err)
			}
			// A failed apply fails the step; it isn't unknown.
			f.applyErr = true
			if _, err := r.do(ctx, s, "qm:abc:2"); err == nil || errors.Is(err, errUnknown) {
				t.Errorf("failed apply: %v", err)
			}
			r.cleanup(s)
			if _, err := os.Stat(filepath.Join(s.Args["workdir"], "src")); !os.IsNotExist(err) {
				t.Errorf("cleanup left the checkout: %v", err)
			}
			if _, err := os.Stat(s.Args["preview"]); err != nil {
				t.Errorf("cleanup removed the preview: %v", err)
			}
		})
	}

	t.Run("pulumi re-preview fallback", func(t *testing.T) {
		f := &fakeIACTools{t: t, noSave: true}
		r := iacRunner{run: f.run}
		s := iacStep(repo, rev, "pulumi:acme/app/staging")
		if err := r.prepare(ctx, &s); err != nil {
			t.Fatal(err)
		}
		if s.Args["cas"] != "re-preview" || s.Args["planfile"] != "" || s.Pre["planfile_digest"] != "" {
			t.Fatalf("prepared %v %v", s.Args, s.Pre)
		}
		p, err := parsePlan(mustJSON(t, Plan{Bead: "b-1", Steps: []Step{s}}))
		if err != nil {
			t.Fatal(err)
		}
		s = p.Steps[0]
		if err := r.pre(ctx, s); err != nil {
			t.Fatalf("pre with the same counts: %v", err)
		}
		ev, err := r.do(ctx, s, "qm:abc:1")
		if err != nil || !strings.Contains(ev, "re-preview fallback") {
			t.Errorf("do: %q %v", ev, err)
		}
		if up := f.ran("pulumi up"); len(up) != 1 || strings.Contains(up[0], "--plan") {
			t.Errorf("up %v", up)
		}
		// Counts that moved since the approval refuse the apply.
		f.preview = "pulumi-replace.json"
		if err := r.pre(ctx, s); !errors.Is(err, errStale) || !strings.Contains(err.Error(), "differs from the approved one") {
			t.Errorf("moved counts: %v", err)
		}
	})

	t.Run("bad steps", func(t *testing.T) {
		r := iacRunner{run: (&fakeIACTools{t: t}).run}
		for name, s := range map[string]Step{
			"no rev":      {Op: "iac.apply", Target: "tofu:app/infra", Args: map[string]string{"repo": repo, "dir": "infra"}, Pre: map[string]string{}},
			"no such dir": iacStep(repo, rev, "tofu:app/nope"),
			"tofu, wrong name": func() Step {
				s := iacStep(repo, rev, "tofu:app/infra/dev")
				s.Args["env"] = `{"TF_WORKSPACE":"prod"}`
				return s
			}(),
			"pulumi, other stack": func() Step {
				s := iacStep(repo, rev, "pulumi:acme/app/dev")
				s.Args["stack"] = "acme/app/prod"
				return s
			}(),
			"pulumi, no stack": {Op: "iac.apply", Target: "pulumi:", Args: map[string]string{"repo": repo, "dir": "infra"}, Pre: map[string]string{"rev": rev}},
		} {
			if name == "no such dir" {
				s.Args["dir"] = "nope"
			}
			before, _ := os.ReadDir(iacDir())
			if err := r.prepare(ctx, &s); err == nil {
				t.Errorf("%s: prepared", name)
			}
			if after, _ := os.ReadDir(iacDir()); len(after) != len(before) {
				t.Errorf("%s: a failed prepare left its directory", name)
			}
		}
	})
}

// TestExecutorIACApply runs applies through the executor's socket path:
// previewed from the commit, checked against the dial, applied, cleaned up.
func TestExecutorIACApply(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	repo, rev := iacRepo(t)
	tools := &fakeIACTools{t: t, planCode: 2}
	bead := Bead{ID: "b-1", Status: StatusInProgress, Labels: []string{"autonomy:6"}}
	cfg := defaultConfig()
	cfg.Autonomy.Prod = []string{"prod"}
	var asked []string
	x := &executor{
		cfg:     cfg,
		show:    func(string) (Bead, error) { return bead, nil },
		active:  func() ([]Bead, error) { return []Bead{bead}, nil },
		needsMe: func(id, note string) error { asked = append(asked, note); return nil },
		runners: map[string]opRunner{"iac.apply": iacRunner{run: tools.run}},
		now:     time.Now,
		sleep:   func(context.Context, time.Duration) {},
		logf:    log.New(io.Discard, "", 0).Printf,
	}
	ctx := context.Background()
	submit := func(target string, edit func(*Step)) execResponse {
		s := iacStep(repo, rev, target)
		if edit != nil {
			edit(&s)
		}
		return x.submit(ctx, "b-1", mustJSON(t, Plan{Source: "shim", Steps: []Step{s}}))
	}

	// Staging at level 6: approved by the dial and applied.
	resp := submit("tofu:app/infra/staging", nil)
	if resp.State != planDone || !strings.Contains(resp.Output, "applied tofu:app/infra/staging") || !strings.Contains(resp.Output, "class 6") {
		t.Fatalf("staging: %+v", resp)
	}
	p, _, err := loadPlan(resp.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if s := p.Steps[0]; s.Pre["plan_digest"] == "" || s.Pre["planfile_digest"] == "" {
		t.Errorf("stored plan doesn't bind the preview: %v", s.Pre)
	}
	if _, err := os.Stat(filepath.Join(p.Steps[0].Args["workdir"], "src")); !os.IsNotExist(err) {
		t.Errorf("a done apply kept its checkout: %v", err)
	}

	// Production is class 7, above level 6: previewed, then waiting for TJ
	// with nothing applied and the checkout kept for the resume.
	applies := len(tools.ran("tofu apply"))
	resp = submit("tofu:app/infra/prod", nil)
	if resp.State != "waiting" || !strings.Contains(resp.Why, "class 7, above b-1's level 6") || len(tools.ran("tofu apply")) != applies {
		t.Fatalf("prod: %+v", resp)
	}
	p, digest, err := loadPlan(resp.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(iacProj(p.Steps[0])); err != nil {
		t.Errorf("a waiting apply lost its checkout: %v", err)
	}
	if _, err := (&planCLI{t: t, cfg: cfg, bead: bead}).run("TJ", 0, "approve", digest); err != nil {
		t.Fatal(err)
	}
	tools.planCode = 2
	if kind, err := x.execPlan(ctx, digest); err != nil || kind != planDone || len(tools.ran("tofu apply")) != applies+1 {
		t.Errorf("resume after TJ's approval: %s %v", kind, err)
	}

	// Without expected counts, staging is one class up.
	resp = submit("tofu:app/infra/staging", func(s *Step) { delete(s.Args, "expect") })
	if resp.State != "waiting" || !strings.Contains(resp.Why, "class 7") {
		t.Errorf("no expect: %+v", resp)
	}

	// A preview the worker brings isn't taken: the executor makes its own.
	resp = submit("tofu:app/infra/staging", func(s *Step) { s.Args["preview"] = copyPreview(t, "tofu-update.json") })
	if resp.State != "error" || !strings.Contains(resp.Why, "args.preview is the executor's to set") {
		t.Errorf("worker's preview: %+v", resp)
	}
	// A preview that fails stops before any check, and leaves nothing.
	before, _ := os.ReadDir(iacDir())
	resp = submit("tofu:app/infra/staging", func(s *Step) { s.Args["dir"] = "missing" })
	if resp.State != "error" || !strings.Contains(resp.Why, "preview") {
		t.Errorf("bad dir: %+v", resp)
	}
	if after, _ := os.ReadDir(iacDir()); len(after) != len(before) {
		t.Error("a failed preview left its directory")
	}
	if len(asked) != 0 {
		t.Errorf("needs_me: %v", asked)
	}
}

func TestPruneIAC(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	old, fresh := filepath.Join(iacDir(), "old"), filepath.Join(iacDir(), "fresh")
	for _, d := range []string{old, fresh} {
		if err := os.MkdirAll(filepath.Join(d, "src"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	then := time.Now().Add(-iacKeep - time.Hour)
	if err := os.Chtimes(old, then, then); err != nil {
		t.Fatal(err)
	}
	pruneIAC(time.Now())
	if _, err := os.Stat(filepath.Join(old, "src")); !os.IsNotExist(err) {
		t.Errorf("old checkout kept: %v", err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("old work directory (its preview and plan) removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fresh, "src")); err != nil {
		t.Errorf("fresh checkout removed: %v", err)
	}
}

func TestRunCmdIAC(t *testing.T) {
	newFixture := func() *runFixture {
		f := newRunFixture(t, "/run/b-1.sock")
		f.r.iac = (&fakeIAC{}).lookup
		f.r.dir = "/repo/infra"
		t.Setenv("PATH", os.Getenv("PATH")+string(os.PathListSeparator)+fakeTool(t, "pulumi", "exit 0")+string(os.PathListSeparator)+fakeTool(t, "tofu", "exit 0"))
		return f
	}
	// An apply goes to the executor as a one-step plan.
	f := newFixture()
	if err := runCmd([]string{"--", "pulumi", "up", "--yes"}, f.r); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.sent[0].Steps[0].Op != "iac.apply" || f.sent[0].Steps[0].Target != "pulumi:acme/app/dev" || len(f.replaced) != 0 {
		t.Fatalf("sent %+v, replaced %v", f.sent, f.replaced)
	}
	// Waiting for TJ: an apply says how to run it once approved.
	f = newFixture()
	f.resp = execResponse{State: "waiting", Digest: "d1g", Why: "not autonomous: class 7"}
	if err := runCmd([]string{"tofu", "apply", "-auto-approve"}, f.r); err == nil || !strings.Contains(f.out.String(), "quartermaster plan resume d1g") {
		t.Errorf("waiting: %v, %q", err, f.out.String())
	}
	// Everything else runs the real tool.
	for _, argv := range [][]string{{"pulumi", "preview"}, {"tofu", "plan"}} {
		f = newFixture()
		if err := runCmd(argv, f.r); err != nil || len(f.sent) != 0 || len(f.replaced) == 0 || !strings.HasSuffix(f.replaced[0], "/"+argv[0]) {
			t.Errorf("%v: err %v, sent %d, replaced %v", argv, err, len(f.sent), f.replaced)
		}
	}
	// No executor: the real tool runs, with a warning naming it.
	f = newFixture()
	f.callErr = fmt.Errorf("dial: %w", os.ErrNotExist)
	if err := runCmd([]string{"tofu", "apply"}, f.r); err != nil || len(f.replaced) == 0 || !strings.Contains(f.err.String(), "running tofu directly") {
		t.Errorf("no executor: %v, %v, %q", err, f.replaced, f.err.String())
	}
}

// TestIACProvenance: a step names only what prepare made, and cleanup
// touches nothing else.
func TestIACProvenance(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	ctx := context.Background()
	repo, rev := iacRepo(t)
	r := iacRunner{run: (&fakeIACTools{t: t}).run}
	s := iacStep(repo, rev, "tofu:app/infra/staging")
	if err := r.prepare(ctx, &s); err != nil {
		t.Fatal(err)
	}
	p, err := parsePlan(mustJSON(t, Plan{Bead: "b-1", Steps: []Step{s}}))
	if err != nil {
		t.Fatal(err)
	}
	s = p.Steps[0]
	// A benign preview elsewhere, digest and all, doesn't stand in for the
	// executor's.
	fake := copyPreview(t, "tofu-update.json")
	b, _ := os.ReadFile(fake)
	swapped := s
	swapped.Args = maps.Clone(s.Args)
	swapped.Pre = maps.Clone(s.Pre)
	swapped.Args["preview"], swapped.Pre["plan_digest"] = fake, sha256Hex(b)
	if err := r.pre(ctx, swapped); !errors.Is(err, errStale) {
		t.Errorf("swapped preview: %v", err)
	}
	// cleanup never leaves iacDir's direct children.
	victim := filepath.Join(filepath.Dir(iacDir()), "victim")
	if err := os.MkdirAll(filepath.Join(victim, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{iacDir() + "/../victim", iacDir(), victim} {
		r.cleanup(Step{Op: "iac.apply", Args: map[string]string{"workdir": w}})
	}
	if _, err := os.Stat(filepath.Join(victim, "src")); err != nil {
		t.Errorf("cleanup reached outside iacDir: %v", err)
	}
}

func TestIACProbeTag(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	s := Step{Op: "iac.apply", Target: "pulumi:a/b/c", Args: map[string]string{"stack": "a/b/c", "workdir": filepath.Join(iacDir(), "w"), "dir": "."}}
	if err := os.MkdirAll(iacProj(s), 0o700); err != nil {
		t.Fatal(err)
	}
	f := &fakeIACTools{t: t, history: `[{"message":"qm:abc:10 ship","result":"succeeded"}]`}
	r := iacRunner{run: f.run}
	if happened, _, _, _ := r.probe(context.Background(), s, "qm:abc:1"); happened {
		t.Error("step 10's update counted for step 1")
	}
	if happened, _, _, _ := r.probe(context.Background(), s, "qm:abc:10"); !happened {
		t.Error("step 10's own update not found")
	}
}
