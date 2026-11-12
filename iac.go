package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// IaC applies through the executor (docs/design/execution-plan.md, the
// iac.apply row). The pulumi and tofu shims lower `pulumi up` and `tofu
// apply` to an iac.apply step bound to the commit the worker's worktree is
// at. The executor never plans from that worktree: it clones the commit into
// its own directory, previews there and saves the plan (`pulumi preview
// --save-plan`, `tofu plan -out`), classifies the preview with gate's
// classifier, and applies exactly that saved plan (`pulumi up --plan`, `tofu
// apply <planfile>`). The preview's digest is in the plan the dial or TJ
// approves, so an approval covers that preview and no other.
//
// Pulumi's update plans are experimental (E7). When a preview can't save
// one, the step records cas=re-preview, and before applying the executor
// previews again and refuses if the counts moved.
//
// This is routing, not custody (S1): the executor runs the real binaries as
// the worker's user, with its own environment and only the worker's
// selectors (AWS_PROFILE, TF_WORKSPACE, ...) carried over in the step.

func init() {
	opRunners["iac.apply"] = iacRunner{run: toolExec}
}

// iacEnv are the worker's environment variables an apply carries to the
// executor: selectors that change what is planned, never credentials.
var iacEnv = []string{"AWS_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION", "KUBECONFIG", "PULUMI_BACKEND_URL", "TF_WORKSPACE"}

// expectEnv is where a worker states the counts it expects, as gate plan's
// --expect: create=1,update=2.
const expectEnv = "QUARTERMASTER_EXPECT"

// iacLookup is what lowering asks git, the tool and the environment.
type iacLookup struct {
	// git runs git in dir and returns its trimmed stdout.
	git func(dir string, args ...string) (string, error)
	// pulumiStack is the fully qualified name of stack ("" for the selected
	// one) for the project in dir.
	pulumiStack func(dir, stack string) (string, error)
	readFile    func(string) ([]byte, error)
	getenv      func(string) string
	dir         string
}

func liveIACLookup(dir string) iacLookup {
	return iacLookup{
		git: func(dir string, args ...string) (string, error) {
			cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
			}
			return strings.TrimSpace(string(out)), nil
		},
		pulumiStack: func(dir, stack string) (string, error) {
			bin, err := realBinary("pulumi")
			if err != nil {
				return "", err
			}
			args := []string{"stack", "--show-name", "--fully-qualify-stack-names", "--non-interactive", "--cwd", dir}
			if stack != "" {
				args = append(args, "--stack", stack)
			}
			cmd := exec.Command(bin, args...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				return "", fmt.Errorf("pulumi stack --show-name: %v: %s", err, strings.TrimSpace(stderr.String()))
			}
			return strings.TrimSpace(string(out)), nil
		},
		readFile: os.ReadFile,
		getenv:   os.Getenv,
		dir:      dir,
	}
}

// opaqueTool is argv as an opaque step, as opaqueGH is for gh, carrying
// the worker's selectors (iacEnv) so it runs against what the worker meant.
func opaqueTool(tool string, args []string, l iacLookup, why string) (Step, bool, error) {
	argv, _ := json.Marshal(args)
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellWord(a)
	}
	s := Step{
		Op:   opOpaque,
		Text: tool + " " + strings.Join(quoted, " "),
		Args: map[string]string{"tool": tool, "argv": string(argv), "dir": l.dir, "why": why},
	}
	if env := carriedEnv(l); env != "" {
		s.Args["env"] = env
	}
	return s, true, nil
}

// carriedEnv is the worker's iacEnv selectors as JSON, "" for none.
func carriedEnv(l iacLookup) string {
	env := map[string]string{}
	for _, k := range iacEnv {
		if v := l.getenv(k); v != "" {
			env[k] = v
		}
	}
	if len(env) == 0 {
		return ""
	}
	b, _ := json.Marshal(env)
	return string(b)
}

// pulumiGlobal are pulumi's persistent flags that take a value; the rest
// (--non-interactive, --emoji, ...) take none.
var pulumiGlobal = map[string]bool{"--color": true, "--cwd": true, "-C": true, "--verbose": true, "-v": true, "--profiling": true, "--tracing": true, "--memprofilerate": true}

var pulumiGlobalBool = map[string]bool{"--non-interactive": true, "--emoji": true, "-e": true, "--logtostderr": true, "--logflow": true, "--disable-integrity-checking": true}

// pulumiUpQuiet are up's flags that only change how it displays; the
// executor drops them and runs with its own.
var pulumiUpQuiet = map[string]bool{"--yes": true, "-y": true, "--skip-preview": true, "-f": true, "--diff": true, "--json": true, "-j": true, "--debug": true, "-d": true, "--suppress-outputs": true, "--suppress-progress": true, "--show-sames": true, "--show-reads": true, "--show-config": true, "--show-replacement-steps": true, "--show-full-output": true}

// lowerPulumi lowers pulumi's argv: up (or update) becomes iac.apply, destroy
// and any up flag outside the table (--target, --replace, --config, --plan)
// become opaque, and every other subcommand runs the real pulumi.
func lowerPulumi(args []string, l iacLookup) (Step, bool, error) {
	var sub, cwd, stack, message, refresh, parallel string
	f := &flagArgs{args: args}
	for {
		name, word, ok := f.next()
		if !ok {
			break
		}
		if name == "" {
			if sub == "" {
				sub = word
				switch sub {
				case "up", "update":
				case "destroy":
					return opaqueTool("pulumi", args, l, "pulumi destroy")
				default:
					return Step{}, false, nil
				}
				continue
			}
			return opaqueTool("pulumi", args, l, "pulumi up with a template argument")
		}
		switch {
		case name == "--cwd" || name == "-C":
			v, ok := f.value()
			if !ok {
				return Step{}, false, fmt.Errorf("pulumi: %s needs a value", name)
			}
			cwd = v
			continue
		case pulumiGlobal[name]:
			f.value()
			continue
		case pulumiGlobalBool[name]:
			continue
		case sub == "":
			// A flag before the subcommand that isn't pulumi's: its value
			// could be the word that looks like the subcommand.
			if slices.ContainsFunc(args, func(a string) bool { return a == "up" || a == "update" || a == "destroy" }) {
				return opaqueTool("pulumi", args, l, "pulumi "+name+" isn't in the lowering table")
			}
			return Step{}, false, nil
		case pulumiUpQuiet[name]:
			continue
		case name == "--refresh" || name == "-r":
			refresh = "true"
			if f.eq {
				refresh = f.val
			}
			continue
		case name == "--stack" || name == "-s", name == "--message" || name == "-m", name == "--parallel" || name == "-p":
		default:
			return opaqueTool("pulumi", args, l, "pulumi up "+name+" isn't in the lowering table")
		}
		v, ok := f.value()
		if !ok {
			return Step{}, false, fmt.Errorf("pulumi up: %s needs a value", name)
		}
		switch name {
		case "--stack", "-s":
			stack = v
		case "--message", "-m":
			message = v
		default:
			parallel = v
		}
	}
	if sub == "" {
		return Step{}, false, nil
	}
	s, dir, err := iacSource("pulumi up", cwd, l)
	if err != nil {
		return Step{}, false, err
	}
	fq, err := l.pulumiStack(dir, stack)
	if err != nil {
		return Step{}, false, fmt.Errorf("pulumi up: which stack: %w", err)
	}
	if strings.Count(fq, "/") != 2 {
		return Step{}, false, fmt.Errorf("pulumi up: stack %q isn't org/project/stack", fq)
	}
	s.Target = "pulumi:" + fq
	s.Args["stack"] = fq
	s.Args["message"] = message
	s.Args["refresh"] = refresh
	s.Args["parallel"] = parallel
	dropEmpty(s.Args)
	return s, true, nil
}

// tofuApplyPass are apply flags the executor passes to plan and apply
// alike; tofuApplyQuiet only change the display or prompting, and it drops
// them.
var (
	tofuApplyPass  = map[string]bool{"-lock": true, "-lock-timeout": true, "-parallelism": true, "-refresh": true}
	tofuApplyQuiet = map[string]bool{"-auto-approve": true, "-input": true, "-no-color": true, "-compact-warnings": true, "-json": true, "-concise": true}
)

// lowerTofu lowers tofu's argv: apply becomes iac.apply; destroy, apply with
// a planfile, and any apply flag outside the table (-target, -replace,
// -destroy, -refresh-only) become opaque; every other subcommand runs the real
// tofu.
func lowerTofu(args []string, l iacLookup) (Step, bool, error) {
	var sub, chdir string
	var vars, varFiles, pass []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") {
			a = a[1:] // Go's flag package reads --x as -x
		}
		name, val, eq := strings.Cut(a, "=")
		value := func() (string, error) {
			if eq {
				return val, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("tofu %s: %s needs a value", sub, name)
			}
			i++
			return args[i], nil
		}
		if sub == "" {
			switch {
			case name == "-chdir":
				v, err := value()
				if err != nil {
					return Step{}, false, err
				}
				chdir = v
			case !strings.HasPrefix(a, "-"):
				sub = a
				switch sub {
				case "apply":
				case "destroy":
					return opaqueTool("tofu", args, l, "tofu destroy")
				default:
					return Step{}, false, nil
				}
			default:
				return Step{}, false, nil // -help, -version
			}
			continue
		}
		switch {
		case !strings.HasPrefix(a, "-") || a == "-":
			return opaqueTool("tofu", args, l, "tofu apply of a planfile made outside the executor")
		case tofuApplyQuiet[name]:
		case tofuApplyPass[name]:
			if !eq {
				return opaqueTool("tofu", args, l, name+" without =")
			}
			pass = append(pass, name+"="+val)
		case name == "-var":
			v, err := value()
			if err != nil {
				return Step{}, false, err
			}
			vars = append(vars, v)
		case name == "-var-file":
			v, err := value()
			if err != nil {
				return Step{}, false, err
			}
			varFiles = append(varFiles, v)
		default:
			return opaqueTool("tofu", args, l, "tofu apply "+name+" isn't in the lowering table")
		}
	}
	if sub == "" {
		return Step{}, false, nil
	}
	s, dir, err := iacSource("tofu apply", chdir, l)
	if err != nil {
		return Step{}, false, err
	}
	top := s.Args["repo"]
	for i, vf := range varFiles {
		abs := vf
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(dir, abs)
		}
		rel, err := filepath.Rel(top, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return opaqueTool("tofu", args, l, "-var-file "+vf+" is outside the repository, so not in the commit")
		}
		// Relative to the project directory, where the executor runs tofu.
		varFiles[i], _ = filepath.Rel(dir, abs)
	}
	var env map[string]string
	_ = json.Unmarshal([]byte(s.Args["env"]), &env)
	workspace := env["TF_WORKSPACE"]
	if workspace == "" {
		// tofu workspace select writes the choice here, outside the commit.
		if b, err := l.readFile(filepath.Join(dir, ".terraform", "environment")); err == nil {
			workspace = strings.TrimSpace(string(b))
		}
		if workspace != "" && workspace != "default" {
			if env == nil {
				env = map[string]string{}
			}
			env["TF_WORKSPACE"] = workspace
			b, _ := json.Marshal(env)
			s.Args["env"] = string(b)
		}
	}
	s.Target = "tofu:" + tofuName(repoName(top, l), s.Args["dir"], workspace)
	for k, list := range map[string][]string{"vars": vars, "var_files": varFiles, "flags": pass} {
		if len(list) > 0 {
			b, _ := json.Marshal(list)
			s.Args[k] = string(b)
		}
	}
	dropEmpty(s.Args)
	return s, true, nil
}

// tofuName names a tofu target: the repository, the project directory in it,
// and a workspace other than default.
func tofuName(repo, dir, workspace string) string {
	name := repo
	if dir != "." {
		name += "/" + dir
	}
	if workspace != "" && workspace != "default" {
		name += "/" + workspace
	}
	return name
}

// tofuName is the name the executor gives s's target, from what it applies.
func (r iacRunner) tofuName(ctx context.Context, s Step) string {
	var env map[string]string
	_ = json.Unmarshal([]byte(s.Args["env"]), &env)
	l := iacLookup{git: func(dir string, args ...string) (string, error) {
		out, err := r.call(ctx, toolCmd{Tool: "git", Args: args, Dir: dir})
		return strings.TrimSpace(string(out)), err
	}}
	return tofuName(repoName(s.Args["repo"], l), s.Args["dir"], env["TF_WORKSPACE"])
}

// repoName is the repository's name: origin's, else the top directory's.
func repoName(top string, l iacLookup) string {
	if url, err := l.git(top, "remote", "get-url", "origin"); err == nil && url != "" {
		url = strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git")
		if i := strings.LastIndexAny(url, "/:"); i >= 0 && i+1 < len(url) {
			return url[i+1:]
		}
	}
	return filepath.Base(top)
}

// iacSource is an iac.apply step bound to the commit the project directory
// (cd, relative to the worker's) is at: args.repo, args.dir, args.env,
// args.expect and pre.rev. The worktree must have no uncommitted changes,
// since the executor applies the commit and not the files on disk.
func iacSource(what, cd string, l iacLookup) (Step, string, error) {
	dir := l.dir
	if cd != "" {
		if filepath.IsAbs(cd) {
			dir = cd
		} else {
			dir = filepath.Join(l.dir, cd)
		}
	}
	top, err := l.git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return Step{}, "", fmt.Errorf("%s: the executor applies a commit, and %s isn't in a git repository: %w", what, dir, err)
	}
	rel, err := filepath.Rel(top, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return Step{}, "", fmt.Errorf("%s: %s is outside its repository %s", what, dir, top)
	}
	rev, err := l.git(top, "rev-parse", "HEAD")
	if err != nil {
		return Step{}, "", fmt.Errorf("%s: %w", what, err)
	}
	// Tracked changes anywhere (a module next door counts), and new files in
	// the project.
	dirty, err := l.git(top, "status", "--porcelain", "--untracked-files=no")
	if err == nil && dirty == "" {
		dirty, err = l.git(top, "status", "--porcelain", "--untracked-files=all", "--", rel)
	}
	if err != nil {
		return Step{}, "", fmt.Errorf("%s: %w", what, err)
	}
	if dirty != "" {
		first, _, _ := strings.Cut(dirty, "\n")
		return Step{}, "", fmt.Errorf("%s: %s has uncommitted changes (%s); the executor applies commit %s from a clean checkout, so commit them first", what, top, strings.TrimSpace(first), short(rev))
	}
	s := Step{Op: "iac.apply", Args: map[string]string{"repo": top, "dir": rel}, Pre: map[string]string{"rev": rev}}
	s.Args["env"] = carriedEnv(l)
	s.Args["expect"] = strings.TrimSpace(l.getenv(expectEnv))
	if e := s.Args["expect"]; e != "" {
		if _, err := parseExpect(e); err != nil {
			return Step{}, "", fmt.Errorf("%s: $%s: %w", what, expectEnv, err)
		}
	}
	return s, dir, nil
}

// The executor's side.

func iacDir() string { return filepath.Join(stateDir(), "iac") }

// iacPrepared are the args only the executor sets, from its own preview.
var iacPrepared = []string{"preview", "planfile", "workdir", "cas"}

// toolCmd is one run of a real binary.
type toolCmd struct {
	Tool string
	Args []string
	Dir  string
	Env  []string // added to the executor's own
}

type toolOut struct {
	Stdout, Stderr []byte
	Code           int
}

// toolExec runs c. A nonzero exit is Code, not an error; an error is a
// binary that didn't run, or a run ctx cut short, which wraps errUnknown.
func toolExec(ctx context.Context, c toolCmd) (toolOut, error) {
	bin := c.Tool
	if c.Tool != "git" {
		var err error
		if bin, err = realBinary(c.Tool); err != nil {
			return toolOut{}, err
		}
	}
	cmd := exec.CommandContext(ctx, bin, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "QUARTERMASTER_EXEC_SOCKET=")
	}), c.Env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := toolOut{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return out, fmt.Errorf("%w: %s %s: %v", errUnknown, c.Tool, strings.Join(c.Args[:min(1, len(c.Args))], " "), ctx.Err())
	case errors.As(err, &exit):
		out.Code = exit.ExitCode()
		return out, nil
	}
	return out, err
}

type iacRunner struct {
	run func(ctx context.Context, c toolCmd) (toolOut, error)
}

func (iacRunner) retrySafe() bool { return true }

// tool and proj are the step's binary and its project directory in the
// executor's checkout.
func iacTool(s Step) string {
	tool, _, _ := strings.Cut(s.Target, ":")
	return tool
}

func iacProj(s Step) string { return filepath.Join(s.Args["workdir"], "src", s.Args["dir"]) }

// env is the step's carried environment as KEY=value, plus what the tool
// needs to run unattended.
func iacEnvList(s Step) []string {
	var env map[string]string
	_ = json.Unmarshal([]byte(s.Args["env"]), &env)
	var out []string
	for _, k := range sortedKeys(env) {
		if slices.Contains(iacEnv, k) {
			out = append(out, k+"="+env[k])
		}
	}
	if iacTool(s) == "pulumi" {
		return append(out, "PULUMI_EXPERIMENTAL=true", "PULUMI_SKIP_UPDATE_CHECK=true", "PULUMI_SKIP_CONFIRMATIONS=true")
	}
	return append(out, "TF_IN_AUTOMATION=1", "TF_INPUT=0")
}

func jsonList(s string) []string {
	var out []string
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

// tofuPlanArgs are the -var, -var-file and passed flags for plan.
func tofuPlanArgs(s Step) []string {
	var args []string
	for _, v := range jsonList(s.Args["vars"]) {
		args = append(args, "-var", v)
	}
	for _, v := range jsonList(s.Args["var_files"]) {
		args = append(args, "-var-file="+v)
	}
	return append(args, jsonList(s.Args["flags"])...)
}

// tofuApplyArgs are the passed flags apply takes: everything but -refresh,
// which a saved plan already settled.
func tofuApplyArgs(s Step) []string {
	return slices.DeleteFunc(jsonList(s.Args["flags"]), func(f string) bool { return strings.HasPrefix(f, "-refresh=") })
}

// pulumiArgs runs pulumi's sub (up, preview, or stack history) on the step's
// stack in its checkout, with extra flags after.
func pulumiArgs(s Step, sub string, extra ...string) []string {
	args := strings.Fields(sub)
	args = append(args, "--cwd", iacProj(s), "--stack", s.Args["stack"], "--non-interactive", "--color", "never")
	if r := s.Args["refresh"]; r != "" && (sub == "up" || sub == "preview") {
		args = append(args, "--refresh="+r)
	}
	return append(args, extra...)
}

// call runs c and turns a nonzero exit into an error carrying stderr's tail.
func (r iacRunner) call(ctx context.Context, c toolCmd) ([]byte, error) {
	out, err := r.run(ctx, c)
	if err != nil {
		return out.Stdout, err
	}
	if out.Code != 0 {
		return out.Stdout, fmt.Errorf("%s %s exited %d: %s", c.Tool, c.Args[0], out.Code, tail(string(out.Stderr)+string(out.Stdout), 1500))
	}
	return out.Stdout, nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// prepare makes an iac.apply step's preview: a clean clone of pre.rev from
// args.repo into a fresh directory under iacDir, the tool's install or init
// there, then the preview with its plan saved. It sets args.workdir,
// args.preview and args.planfile, and pre.planfile_digest; parsePlan binds
// the preview's digest as pre.plan_digest.
func (r iacRunner) prepare(ctx context.Context, s *Step) (err error) {
	rev := s.Pre["rev"]
	if !fullSHA.MatchString(rev) {
		return fmt.Errorf("%w: iac.apply needs pre.rev, a full commit SHA", errPlan)
	}
	if s.Args["repo"] == "" || s.Args["dir"] == "" {
		return fmt.Errorf("%w: iac.apply needs args.repo and args.dir", errPlan)
	}
	if tool := iacTool(*s); tool == "pulumi" && (s.Args["stack"] == "" || s.Target != "pulumi:"+s.Args["stack"]) {
		return fmt.Errorf("%w: a pulumi apply's target must be pulumi:<args.stack>, so the class is the stack's", errPlan)
	}
	if iacTool(*s) == "tofu" {
		if want := r.tofuName(ctx, *s); s.Target != "tofu:"+want {
			return fmt.Errorf("%w: the target must be tofu:%s, named from the repository, args.dir and TF_WORKSPACE", errPlan, want)
		}
	}
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	work := filepath.Join(iacDir(), time.Now().UTC().Format("20060102T150405")+"-"+hex.EncodeToString(id))
	if err := os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(work)
		}
	}()
	s.Args["workdir"] = work
	src := filepath.Join(work, "src")
	git := func(dir string, args ...string) (string, error) {
		out, err := r.call(ctx, toolCmd{Tool: "git", Args: args, Dir: dir})
		return strings.TrimSpace(string(out)), err
	}
	common, err := git(s.Args["repo"], "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if _, err := git(work, "clone", "--quiet", "--shared", "--no-checkout", common, src); err != nil {
		return err
	}
	if _, err := git(src, "-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", rev); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(src, ".gitmodules")); err == nil {
		if _, err := git(src, "submodule", "update", "--init", "--recursive", "--quiet"); err != nil {
			return err
		}
	}
	proj := iacProj(*s)
	if fi, err := os.Stat(proj); err != nil || !fi.IsDir() {
		return fmt.Errorf("%w: %s isn't a directory at %s", errPlan, s.Args["dir"], short(rev))
	}
	env := iacEnvList(*s)
	preview := filepath.Join(work, "preview.json")
	var out []byte
	switch iacTool(*s) {
	case "pulumi":
		if _, err := r.call(ctx, toolCmd{Tool: "pulumi", Args: []string{"install", "--cwd", proj, "--non-interactive", "--color", "never"}, Dir: proj, Env: env}); err != nil {
			return err
		}
		planfile := iacPlanfile(*s, work)
		out, err = r.call(ctx, toolCmd{Tool: "pulumi", Args: pulumiArgs(*s, "preview", "--json", "--save-plan", planfile), Dir: proj, Env: env})
		if err == nil {
			s.Args["planfile"] = planfile
			break
		}
		// E7: a preview that can't save an update plan falls back to
		// previewing again just before the apply.
		var again error
		if out, again = r.call(ctx, toolCmd{Tool: "pulumi", Args: pulumiArgs(*s, "preview", "--json"), Dir: proj, Env: env}); again != nil {
			return again
		}
		s.Args["cas"] = "re-preview"
	case "tofu":
		if _, err := r.call(ctx, toolCmd{Tool: "tofu", Args: []string{"init", "-input=false", "-no-color"}, Dir: proj, Env: env}); err != nil {
			return err
		}
		planfile := iacPlanfile(*s, work)
		if _, err := r.call(ctx, toolCmd{Tool: "tofu", Args: append([]string{"plan", "-input=false", "-no-color", "-out=" + planfile}, tofuPlanArgs(*s)...), Dir: proj, Env: env}); err != nil {
			return err
		}
		s.Args["planfile"] = planfile
		if out, err = r.call(ctx, toolCmd{Tool: "tofu", Args: []string{"show", "-json", "-no-color", planfile}, Dir: proj, Env: env}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: iac.apply target %q", errPlan, s.Target)
	}
	if _, err := checkPlan(out); err != nil {
		return fmt.Errorf("the preview: %w", err)
	}
	if err := writeAtomic(preview, out); err != nil {
		return err
	}
	s.Args["preview"] = preview
	if pf := s.Args["planfile"]; pf != "" {
		b, err := os.ReadFile(pf)
		if err != nil {
			return err
		}
		s.Pre["planfile_digest"] = sha256Hex(b)
	}
	return nil
}

// pre checks the step is the one approved: its checkout, preview and saved
// plan are the executor's and unchanged, and the checkout is still at rev.
// Without a saved plan (cas=re-preview) it previews again and refuses counts
// that moved.
func (r iacRunner) pre(ctx context.Context, s Step) error {
	work, ok := iacWork(s)
	if !ok {
		return fmt.Errorf("%w: the apply wasn't previewed by the executor; run it again through the shim or plan run", errStale)
	}
	// The preview and the saved plan are the ones prepare wrote in that
	// directory, never files the step names elsewhere.
	if s.Args["preview"] != filepath.Join(work, "preview.json") || s.Args["planfile"] != "" && s.Args["planfile"] != iacPlanfile(s, work) {
		return fmt.Errorf("%w: the step's preview or saved plan isn't the executor's", errStale)
	}
	for _, f := range [][2]string{{"preview", "plan_digest"}, {"planfile", "planfile_digest"}} {
		path, want := s.Args[f[0]], s.Pre[f[1]]
		if want == "" && f[0] == "planfile" && s.Args["cas"] == "re-preview" {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("%w: the %s is gone (%v); run the apply again", errStale, f[0], err)
		}
		if want == "" || sha256Hex(b) != want {
			return fmt.Errorf("%w: the %s doesn't match pre.%s", errStale, f[0], f[1])
		}
	}
	out, err := r.call(ctx, toolCmd{Tool: "git", Args: []string{"rev-parse", "HEAD"}, Dir: filepath.Join(work, "src")})
	if err != nil {
		return err
	}
	if head := strings.TrimSpace(string(out)); head != s.Pre["rev"] {
		return fmt.Errorf("%w: the checkout is at %s, not %s", errStale, short(head), short(s.Pre["rev"]))
	}
	if s.Args["cas"] != "re-preview" {
		return nil
	}
	approved, err := os.ReadFile(s.Args["preview"])
	if err != nil {
		return err
	}
	was, err := checkPlan(approved)
	if err != nil {
		return err
	}
	fresh, err := r.call(ctx, toolCmd{Tool: "pulumi", Args: pulumiArgs(s, "preview", "--json"), Dir: iacProj(s), Env: iacEnvList(s)})
	if err != nil {
		return err
	}
	now, err := checkPlan(fresh)
	if err != nil {
		return fmt.Errorf("the re-preview: %w", err)
	}
	if counts(now) != counts(was) || len(now.Blocked) != len(was.Blocked) {
		return fmt.Errorf("%w: the re-preview (%s) differs from the approved one (%s)", errStale, counts(now), counts(was))
	}
	return nil
}

func (r iacRunner) do(ctx context.Context, s Step, tag string) (string, error) {
	proj := iacProj(s)
	var c toolCmd
	switch iacTool(s) {
	case "pulumi":
		msg := tag
		if m := s.Args["message"]; m != "" {
			msg += " " + m
		}
		// Outputs stay out of the evidence, which the journal keeps.
		extra := []string{"--yes", "--skip-preview", "--message", msg, "--suppress-progress", "--suppress-outputs"}
		if s.Args["cas"] != "re-preview" {
			// The saved plan constrains the update; it previews again
			// under it, so no --skip-preview.
			extra = []string{"--yes", "--plan", s.Args["planfile"], "--message", msg, "--suppress-progress", "--suppress-outputs"}
		}
		if p := s.Args["parallel"]; p != "" {
			extra = append(extra, "--parallel", p)
		}
		c = toolCmd{Tool: "pulumi", Args: pulumiArgs(s, "up", extra...), Dir: proj, Env: iacEnvList(s)}
	case "tofu":
		args := append([]string{"apply", "-input=false", "-no-color"}, tofuApplyArgs(s)...)
		c = toolCmd{Tool: "tofu", Args: append(args, s.Args["planfile"]), Dir: proj, Env: iacEnvList(s)}
	default:
		return "", fmt.Errorf("%w: iac.apply target %q", errPlan, s.Target)
	}
	out, err := r.run(ctx, c)
	if err != nil {
		return "", err
	}
	text := tail(string(out.Stdout), 2000)
	if out.Code != 0 {
		// It ran and failed, perhaps partway: the plan fails, and applying
		// again means a new preview and a new approval.
		return "", fmt.Errorf("%s %s exited %d: %s", c.Tool, c.Args[0], out.Code, tail(string(out.Stderr)+"\n"+text, 2000))
	}
	how := "saved plan"
	if s.Args["cas"] == "re-preview" {
		how = "re-preview fallback"
	}
	return fmt.Sprintf("applied %s at %s (%s, %s)%s", s.Target, short(s.Pre["rev"]), how, tag, prefix("\n", text)), nil
}

// probe: for Pulumi, the stack's history has an update tagged with this
// step; for OpenTofu, which keeps no history, a fresh plan shows no changes.
func (r iacRunner) probe(ctx context.Context, s Step, tag string) (bool, bool, string, error) {
	proj := iacProj(s)
	if _, err := os.Stat(proj); err != nil {
		return false, false, "", fmt.Errorf("the checkout: %w", err)
	}
	switch iacTool(s) {
	case "pulumi":
		out, err := r.call(ctx, toolCmd{Tool: "pulumi", Args: pulumiArgs(s, "stack history", "--json", "--page-size", "20"), Dir: proj, Env: iacEnvList(s)})
		if err != nil {
			return false, false, "", err
		}
		var history []struct {
			Message string `json:"message"`
			Result  string `json:"result"`
		}
		if err := json.Unmarshal(out, &history); err != nil {
			return false, false, "", fmt.Errorf("pulumi stack history: %w", err)
		}
		for _, h := range history {
			if h.Message != tag && !strings.HasPrefix(h.Message, tag+" ") {
				continue
			}
			switch h.Result {
			case "succeeded":
				return true, true, fmt.Sprintf("update %s on %s succeeded", tag, s.Target), nil
			case "failed":
				return false, true, "", nil
			}
			return false, false, "", nil // in progress: can't tell yet
		}
		return false, true, "", nil
	case "tofu":
		args := append([]string{"plan", "-input=false", "-no-color", "-lock=false", "-detailed-exitcode"}, tofuPlanArgs(s)...)
		out, err := r.run(ctx, toolCmd{Tool: "tofu", Args: args, Dir: proj, Env: iacEnvList(s)})
		if err != nil {
			return false, false, "", err
		}
		switch out.Code {
		case 0:
			return true, true, "a fresh plan of " + s.Target + " shows no changes", nil
		case 2:
			return false, true, "", nil
		}
		return false, false, "", fmt.Errorf("tofu plan exited %d: %s", out.Code, tail(string(out.Stderr), 500))
	}
	return false, false, "", fmt.Errorf("%w: iac.apply target %q", errPlan, s.Target)
}

// cleanup removes the step's checkout once its plan is done or failed; the
// preview and saved plan stay for the record.
func (iacRunner) cleanup(s Step) {
	if work, ok := iacWork(s); ok {
		_ = os.RemoveAll(filepath.Join(work, "src"))
	}
}

// iacWork is the step's work directory, if it is one prepare could have
// made: a direct child of iacDir.
func iacWork(s Step) (string, bool) {
	work := s.Args["workdir"]
	rel, err := filepath.Rel(iacDir(), work)
	if work == "" || err != nil || rel == "." || strings.HasPrefix(rel, "..") || strings.ContainsRune(rel, filepath.Separator) {
		return "", false
	}
	return filepath.Join(iacDir(), rel), true
}

// iacPlanfile is where prepare saves the step's plan in work.
func iacPlanfile(s Step, work string) string {
	if iacTool(s) == "pulumi" {
		return filepath.Join(work, "plan.json")
	}
	return filepath.Join(work, "plan.tfplan")
}

// iacKeep is how long an unfinished apply's checkout is kept for a resume.
// The preview and saved plan stay, so the plan still classifies and says
// what it was; a resume after that can't probe and stops for TJ.
const iacKeep = 7 * 24 * time.Hour

// pruneIAC removes the checkouts of work directories older than iacKeep.
func pruneIAC(now time.Time) {
	entries, err := os.ReadDir(iacDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !e.IsDir() || now.Sub(info.ModTime()) < iacKeep {
			continue
		}
		_ = os.RemoveAll(filepath.Join(iacDir(), e.Name(), "src"))
	}
}

// A stepPreparer completes a step the executor must make itself before it
// is checked; a stepCleaner removes what that left once the plan ends.
type (
	stepPreparer interface {
		prepare(ctx context.Context, s *Step) error
	}
	stepCleaner interface{ cleanup(s Step) }
)

// prepare completes plan p's steps whose runner prepares them, and returns
// it parsed again, so their previews' digests are bound in.
func (x *executor) prepare(ctx context.Context, p Plan) (Plan, error) {
	changed := false
	for i := range p.Steps {
		s := &p.Steps[i]
		r, ok := x.runners[s.Op].(stepPreparer)
		if !ok {
			continue
		}
		for _, k := range iacPrepared {
			if s.Args[k] != "" {
				return p, fmt.Errorf("%w: step %d: args.%s is the executor's to set; it previews %s itself from a clean checkout", errPlan, i+1, k, s.Op)
			}
		}
		if s.Pre["plan_digest"] != "" || s.Pre["planfile_digest"] != "" {
			return p, fmt.Errorf("%w: step %d: the executor binds the preview's digests itself", errPlan, i+1)
		}
		if s.Args == nil {
			s.Args = map[string]string{}
		}
		if s.Pre == nil {
			s.Pre = map[string]string{}
		}
		if err := r.prepare(ctx, s); err != nil {
			// The failed step removed its own; the earlier ones are ours.
			x.cleanupPlan(Plan{Steps: p.Steps[:i]})
			return p, fmt.Errorf("step %d (%s): preview: %w", i+1, s.Op, err)
		}
		changed = true
	}
	if !changed {
		return p, nil
	}
	data, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	return parsePlan(data)
}

// cleanupPlan lets each step's runner remove what it left.
func (x *executor) cleanupPlan(p Plan) {
	for _, s := range p.Steps {
		if c, ok := x.runners[s.Op].(stepCleaner); ok {
			c.cleanup(s)
		}
	}
}

// iacWaiting is what the shim adds to a plan waiting for TJ when it applies:
// running the same command again previews again, so it makes a new digest.
func iacWaiting(digest string) string {
	return fmt.Sprintf("once TJ approves it, run it with: quartermaster plan resume %s (running the apply again previews again, under a new digest)", digest)
}
