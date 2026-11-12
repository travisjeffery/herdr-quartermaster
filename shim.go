package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The shims (docs/design/execution-plan.md, "Front ends") route a worker's
// credentialed commands through the executor. dispatch puts the shim
// directory first on the worker's PATH; its gh is a two-line script that runs
// `quartermaster run -- gh <argv>`, so the executor sees the argv after the
// shell expanded it. run lowers that argv to a plan step: `gh pr merge` and
// `gh api -X PUT repos/o/r/pulls/N/merge` become github.pr.merge or
// github.pr.automerge, a form of them it can't read becomes opaque (class 9),
// and every other subcommand runs the real gh unchanged. This is routing, not
// custody (S1): the worker still holds gh's token.

// shimMarker is in every shim, so the real binary's lookup never finds one.
const shimMarker = "quartermaster shim"

// shimTools are the binaries with a shim. pulumi and tofu join with their
// applies.
var shimTools = []string{"gh"}

func shimDir() string { return filepath.Join(stateDir(), "shims") }

// shimScript is the shim for tool, running the quartermaster binary at qm.
func shimScript(qm, tool string) string {
	return "#!/bin/sh\n# " + shimMarker + " (docs/design/execution-plan.md): " + tool + "'s argv goes to the executor.\nexec " + shellQuote(qm) + " run -- " + tool + " \"$@\"\n"
}

// writeShims writes the shims into shimDir, running the quartermaster binary
// at qm, and returns the directory.
func writeShims(qm string) (string, error) {
	dir := shimDir()
	for _, tool := range shimTools {
		path := filepath.Join(dir, tool)
		want := shimScript(qm, tool)
		if b, err := os.ReadFile(path); err == nil && string(b) == want {
			continue
		}
		if err := writeAtomic(path, []byte(want)); err != nil {
			return "", err
		}
		if err := os.Chmod(path, 0o755); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// shimBinary is the quartermaster a shim runs: the ~/.local/bin link
// configure makes, else this binary.
func shimBinary() (string, error) {
	if home, err := os.UserHomeDir(); err == nil {
		link := filepath.Join(home, ".local", "bin", toolName)
		if _, err := os.Stat(link); err == nil {
			return link, nil
		}
	}
	return os.Executable()
}

// execEnv is what a local worker's pane exports beyond agentEnv to route
// through the executor: its bead's socket, and the shims first on PATH. With
// no executor running it is empty, and the worker runs its tools directly
// (S1: routing is advisory).
func execEnv(id string) string {
	if !execOpen(id) {
		return ""
	}
	env := " QUARTERMASTER_EXEC_SOCKET=" + shellQuote(beadSocket(id))
	qm, err := shimBinary()
	if err != nil {
		return env
	}
	dir, err := writeShims(qm)
	if err != nil {
		return env
	}
	return env + " PATH=" + shellQuote(dir) + `:"$PATH"`
}

// realBinary is tool's path on PATH, skipping the shim directory and any shim
// found elsewhere, so a shim never runs itself.
func realBinary(tool string) (string, error) {
	shims := filepath.Clean(shimDir())
	if d, err := filepath.EvalSymlinks(shims); err == nil {
		shims = d
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		if d, err := filepath.EvalSymlinks(dir); err == nil {
			dir = d
		}
		if filepath.Clean(dir) == shims {
			continue
		}
		path := filepath.Join(dir, tool)
		fi, err := os.Stat(path)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 || isShim(path) {
			continue
		}
		return path, nil
	}
	return "", fmt.Errorf("no %s on PATH outside the shims in %s", tool, shimDir())
}

func isShim(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 256)
	n, _ := io.ReadFull(f, head)
	return bytes.Contains(head[:n], []byte(shimMarker))
}

// ghExec runs the real gh from dir. A run that ctx or its timeout cut short
// wraps errUnknown: the effect may have happened.
func ghExec(ctx context.Context, dir string, args ...string) ([]byte, error) {
	bin, err := realBinary("gh")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		err = fmt.Errorf("gh %s: %v: %s", strings.Join(args[:min(2, len(args))], " "), err, strings.TrimSpace(stderr.String()))
		if ctx.Err() != nil {
			err = fmt.Errorf("%w: %w", errUnknown, err)
		}
		return stdout.Bytes(), err
	}
	return stdout.Bytes(), nil
}

// ghPR is a pull request a gh command names, resolved against GitHub.
type ghPR struct {
	Owner, Repo string
	Number      int
	Head        string
}

func (p ghPR) target() string { return fmt.Sprintf("github:%s/%s#%d", p.Owner, p.Repo, p.Number) }

var prURLRE = regexp.MustCompile(`^https://github\.com/([\w.-]+)/([\w.-]+)/pull/([1-9][0-9]*)`)

func parsePRURL(url, head string) (ghPR, error) {
	m := prURLRE.FindStringSubmatch(url)
	if m == nil {
		return ghPR{}, fmt.Errorf("%q isn't a GitHub pull request URL", url)
	}
	n, _ := strconv.Atoi(m[3])
	return ghPR{Owner: m[1], Repo: m[2], Number: n, Head: head}, nil
}

// stepPRURL is a merge step's target as a pull request URL.
func stepPRURL(s Step) (string, error) {
	m := githubTarget.FindStringSubmatch(s.Target)
	if m == nil {
		return "", fmt.Errorf("%w: target %q isn't github:owner/repo#N", errPlan, s.Target)
	}
	return fmt.Sprintf("https://github.com/%s/%s/pull/%s", m[1], m[2], m[3]), nil
}

// ghLookup is what lowering asks GitHub and the worker's environment.
type ghLookup struct {
	// pr resolves a gh pr selector (a number, URL or branch; "" for the
	// current branch's) in repo ("" for the current directory's).
	pr func(sel, repo string) (ghPR, error)
	// repo is the current directory's repository, owner/name.
	repo     func() (string, error)
	readFile func(string) ([]byte, error)
	stdin    io.Reader
	dir      string
}

// liveLookup asks the real gh, from dir.
func liveLookup(dir string, stdin io.Reader) ghLookup {
	return ghLookup{
		pr: func(sel, repo string) (ghPR, error) {
			args := []string{"pr", "view"}
			if sel != "" {
				args = append(args, sel)
			}
			if repo != "" {
				args = append(args, "--repo", repo)
			}
			out, err := ghExec(context.Background(), dir, append(args, "--json", "url,headRefOid")...)
			if err != nil {
				return ghPR{}, err
			}
			var v struct {
				URL  string `json:"url"`
				Head string `json:"headRefOid"`
			}
			if err := json.Unmarshal(out, &v); err != nil {
				return ghPR{}, fmt.Errorf("gh pr view: %w", err)
			}
			return parsePRURL(v.URL, v.Head)
		},
		repo: func() (string, error) {
			out, err := ghExec(context.Background(), dir, "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
			return strings.TrimSpace(string(out)), err
		},
		readFile: os.ReadFile,
		stdin:    stdin,
		dir:      dir,
	}
}

// lowerGH lowers gh's argv (without "gh" itself) to a plan step. ok is false
// for argv that runs the real gh unchanged: a subcommand outside the table.
func lowerGH(args []string, l ghLookup) (s Step, ok bool, err error) {
	switch {
	case len(args) >= 2 && args[0] == "pr" && args[1] == "merge":
		return lowerPRMerge(args, l)
	case len(args) >= 1 && args[0] == "api":
		return lowerAPI(args, l)
	}
	return Step{}, false, nil
}

// opaqueGH is argv as an opaque step: class 9, run only with TJ's approval,
// exactly as given, from the worker's directory.
func opaqueGH(args []string, l ghLookup, why string) (Step, bool, error) {
	argv, _ := json.Marshal(args)
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellWord(a)
	}
	return Step{
		Op:   opOpaque,
		Text: "gh " + strings.Join(quoted, " "),
		Args: map[string]string{"tool": "gh", "argv": string(argv), "dir": l.dir, "why": why},
	}, true, nil
}

var plainWord = regexp.MustCompile(`^[\w@%+=:,./-]+$`)

// shellWord quotes a only when the shell would read it differently.
func shellWord(a string) string {
	if plainWord.MatchString(a) {
		return a
	}
	return shellQuote(a)
}

// flagArgs walks argv: next yields a flag's name (with any =value split off
// into value) or a positional word with name "".
type flagArgs struct {
	args []string
	i    int
	val  string
	eq   bool
	rest bool // after --, everything is positional
}

func (f *flagArgs) next() (name, word string, ok bool) {
	if f.i >= len(f.args) {
		return "", "", false
	}
	a := f.args[f.i]
	f.i++
	f.val, f.eq = "", false
	if f.rest || a == "-" || !strings.HasPrefix(a, "-") {
		return "", a, true
	}
	if a == "--" {
		f.rest = true
		return f.next()
	}
	if strings.HasPrefix(a, "--") {
		if n, v, eq := strings.Cut(a, "="); eq {
			f.val, f.eq = v, true
			return n, a, true
		}
	}
	return a, a, true
}

// value is the current flag's value: after = or the next word.
func (f *flagArgs) value() (string, bool) {
	if f.eq {
		return f.val, true
	}
	if f.i >= len(f.args) {
		return "", false
	}
	f.i++
	return f.args[f.i-1], true
}

var mergeMethods = map[string]string{"--squash": "squash", "-s": "squash", "--merge": "merge", "-m": "merge", "--rebase": "rebase", "-r": "rebase"}

// lowerPRMerge lowers gh pr merge. A flag it doesn't know, such as --admin,
// makes the step opaque; --disable-auto, which only turns auto-merge off,
// runs unchanged.
func lowerPRMerge(args []string, l ghLookup) (Step, bool, error) {
	s := Step{Op: "github.pr.merge", Args: map[string]string{}, Pre: map[string]string{}}
	var sel, repo, head string
	f := &flagArgs{args: args[2:]}
	for {
		name, word, ok := f.next()
		if !ok {
			break
		}
		if name == "" {
			if sel != "" {
				return opaqueGH(args, l, "more than one pull request")
			}
			sel = word
			continue
		}
		if m, isMethod := mergeMethods[name]; isMethod {
			if f.eq {
				return opaqueGH(args, l, name+" takes no value")
			}
			if s.Args["method"] != "" && s.Args["method"] != m {
				return Step{}, false, errors.New("gh pr merge: only one of --merge, --rebase or --squash")
			}
			s.Args["method"] = m
			continue
		}
		switch name {
		case "--auto":
			s.Op = "github.pr.automerge"
			continue
		case "--disable-auto":
			return Step{}, false, nil
		case "--delete-branch", "-d":
			s.Args["delete_branch"] = "true"
			continue
		case "--match-head-commit", "--subject", "-t", "--body", "-b", "--body-file", "-F", "--author-email", "-A", "--repo", "-R":
		default:
			return opaqueGH(args, l, "gh pr merge "+name+" isn't in the lowering table")
		}
		v, ok := f.value()
		if !ok {
			return Step{}, false, fmt.Errorf("gh pr merge: %s needs a value", name)
		}
		switch name {
		case "--match-head-commit":
			head = v
		case "--subject", "-t":
			s.Args["subject"] = v
		case "--body", "-b":
			s.Args["body"] = v
		case "--body-file", "-F":
			b, err := readBodyFile(v, l)
			if err != nil {
				return Step{}, false, fmt.Errorf("gh pr merge --body-file: %w", err)
			}
			s.Args["body"] = string(b)
		case "--author-email", "-A":
			s.Args["author_email"] = v
		default:
			repo = v
		}
	}
	pr, err := l.pr(sel, repo)
	if err != nil {
		return Step{}, false, fmt.Errorf("gh pr merge: which pull request: %w", err)
	}
	s.Target = pr.target()
	s.Pre["head_sha"] = fullHead(head, pr.Head)
	dropEmpty(s.Args)
	return s, true, nil
}

func readBodyFile(v string, l ghLookup) ([]byte, error) {
	if v == "-" {
		return io.ReadAll(l.stdin)
	}
	if !filepath.IsAbs(v) {
		v = filepath.Join(l.dir, v)
	}
	return l.readFile(v)
}

// fullHead is the head SHA a step binds: the one given (a prefix of the PR's
// current head expands to it), else the current head.
func fullHead(given, current string) string {
	if given == "" || len(given) < 40 && strings.HasPrefix(current, given) {
		return current
	}
	return given
}

func dropEmpty(m map[string]string) {
	for k, v := range m {
		if v == "" {
			delete(m, k)
		}
	}
}

var apiMergePath = regexp.MustCompile(`^repos/([\w.{}-]+)/([\w.{}-]+)/pulls/([1-9][0-9]*)/merge$`)

// apiMergeFields are the merge endpoint's body fields and each one's step
// arg; sha binds pre.head_sha.
var apiMergeFields = map[string]string{"sha": "", "merge_method": "method", "commit_title": "subject", "commit_message": "body"}

// mergeMutations are GraphQL mutations that merge or turn on auto-merge.
// They don't lower yet, so they're opaque.
var mergeMutations = regexp.MustCompile(`\b(mergePullRequest|enablePullRequestAutoMerge|mergeBranch)\b`)

// lowerAPI lowers gh api: PUT repos/{o}/{r}/pulls/{n}/merge is
// github.pr.merge, and a GraphQL merge mutation is opaque. Everything else
// runs unchanged.
func lowerAPI(args []string, l ghLookup) (Step, bool, error) {
	var method, path, unknown string
	fields := map[string]string{}
	fromFile := false
	f := &flagArgs{args: args[1:]}
	for {
		name, word, ok := f.next()
		if !ok {
			break
		}
		// A short flag with its value attached: -XPUT, -fsha=...
		if len(name) > 2 && name[0] == '-' && name[1] != '-' {
			f.val, f.eq = name[2:], true
			name = name[:2]
		}
		switch name {
		case "":
			if path == "" {
				path = word
			}
		case "-X", "--method":
			v, _ := f.value()
			method = strings.ToUpper(v)
		case "-f", "--raw-field", "-F", "--field":
			v, _ := f.value()
			k, val, _ := strings.Cut(v, "=")
			fields[k] = val
			if name == "-F" || name == "--field" {
				fromFile = fromFile || strings.HasPrefix(val, "@")
			}
		case "--silent":
		default:
			if unknown == "" {
				unknown = name
			}
		}
	}
	path = strings.TrimPrefix(path, "/")
	if path == "graphql" {
		if mergeMutations.MatchString(fields["query"]) {
			return opaqueGH(args, l, "a GraphQL merge mutation")
		}
		return Step{}, false, nil
	}
	m := apiMergePath.FindStringSubmatch(path)
	if m == nil {
		return Step{}, false, nil
	}
	if method == "" {
		method = "GET"
		if len(fields) > 0 {
			method = "POST"
		}
	}
	if method != "PUT" {
		return Step{}, false, nil // GET asks whether it merged
	}
	if unknown != "" || fromFile {
		return opaqueGH(args, l, "gh api on the merge endpoint with "+cmpOr(unknown, "a field read from a file"))
	}
	s := Step{Op: "github.pr.merge", Args: map[string]string{}, Pre: map[string]string{}}
	for k, v := range fields {
		arg, known := apiMergeFields[k]
		if !known {
			return opaqueGH(args, l, "merge field "+k+" isn't in the lowering table")
		}
		if arg != "" {
			s.Args[arg] = v
		}
	}
	owner, name := m[1], m[2]
	if strings.Contains(owner+name, "{") {
		cur, err := l.repo()
		if err != nil {
			return Step{}, false, fmt.Errorf("gh api: the current repository: %w", err)
		}
		o, r, _ := strings.Cut(cur, "/")
		owner, name = strings.ReplaceAll(owner, "{owner}", o), strings.ReplaceAll(name, "{repo}", r)
	}
	pr, err := l.pr(fmt.Sprintf("https://github.com/%s/%s/pull/%s", owner, name, m[3]), "")
	if err != nil {
		return Step{}, false, fmt.Errorf("gh api: the pull request: %w", err)
	}
	s.Target = pr.target()
	s.Pre["head_sha"] = fullHead(fields["sha"], pr.Head)
	dropEmpty(s.Args)
	return s, true, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// shimRun is what `quartermaster run` needs from the process, so tests can
// stand in.
type shimRun struct {
	lookup func(dir string) ghLookup
	// replace runs the real tool in place of this process; it returns only
	// on error.
	replace func(path string, argv []string) error
	call    func(sock string, req execRequest) (execResponse, error)
	getenv  func(string) string
	dir     string
	stdout  io.Writer
	stderr  io.Writer
}

func liveShimRun() shimRun {
	dir, _ := os.Getwd()
	return shimRun{
		lookup:  func(dir string) ghLookup { return liveLookup(dir, os.Stdin) },
		replace: func(path string, argv []string) error { return syscall.Exec(path, argv, os.Environ()) },
		call:    func(sock string, req execRequest) (execResponse, error) { return execCall(sock, req, 0) },
		getenv:  os.Getenv,
		dir:     dir,
		stdout:  os.Stdout,
		stderr:  os.Stderr,
	}
}

// runCmd is `quartermaster run -- <tool> <argv>`, what a shim runs: lower the
// argv and send it to the executor on the bead's socket, or run the real tool
// when the argv is outside the table, the worker has no socket, or no
// executor answers.
func runCmd(args []string, r shimRun) error {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return errors.New("usage: quartermaster run -- <tool> [args...]")
	}
	tool := args[0]
	passThrough := func() error {
		bin, err := realBinary(tool)
		if err != nil {
			return err
		}
		return r.replace(bin, args)
	}
	sock := r.getenv("QUARTERMASTER_EXEC_SOCKET")
	if tool != "gh" || sock == "" {
		return passThrough()
	}
	s, ok, err := lowerGH(args[1:], r.lookup(r.dir))
	if err != nil {
		return err
	}
	if !ok {
		return passThrough()
	}
	p := Plan{Actor: strings.TrimSpace("worker " + r.getenv("HERDR_PANE_ID")), Source: "shim", Steps: []Step{s}}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	resp, err := r.call(sock, execRequest{Run: data})
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		fmt.Fprintf(r.stderr, "quartermaster: no executor on %s; running gh directly (advisory)\n", sock)
		return passThrough()
	}
	if err != nil {
		return err
	}
	err = reportRun(resp, r.stdout)
	if resp.State == "waiting" {
		fmt.Fprintf(r.stdout, "waiting for approval: quartermaster plan status %s\n", resp.Digest)
	}
	return err
}
