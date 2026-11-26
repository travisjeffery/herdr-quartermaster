package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// kinds are the agents quartermaster starts, by their Herdr kind, which is
// also the executable and the actor a worker claims its bead as.
var kinds = []string{"claude", "codex", "muse", "pi", "agy"}

var validKinds = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range kinds {
		m[k] = true
	}
	return m
}()

// kindList names the kinds for messages: "claude, codex, muse, pi or agy".
func kindList(extra ...string) string {
	all := append(slices.Clone(kinds), extra...)
	return strings.Join(all[:len(all)-1], ", ") + " or " + all[len(all)-1]
}

// brief is the first prompt a worker gets. It stays short: the bead holds the
// task, bd prime holds the workflow and memories. Claude and Codex get bd prime
// from a SessionStart hook; an agent without one is told to run it, and an
// agent with one isn't asked to load its large output twice.
func brief(b Bead, branch, path, linear, instructions string) string {
	var s strings.Builder
	fmt.Fprintf(&s, "You are the quartermaster worker for bead %s: %s\n", b.ID, b.Title)
	fmt.Fprintf(&s, "It is already claimed for you. Worktree %s on branch %s.\n", path, branch)
	fmt.Fprintf(&s, "Start with `bd prime` unless its output (\"Beads Workflow Context\") is already in your context from a session hook, then `bd show %s`.\n\n", b.ID)
	s.WriteString(`How this thread reports (the quartermaster ticker and coordinator read these, not your chat):
- Keep the bead current with ` + "`bd note`" + `: findings, decisions, blockers.
- When you open a PR, ` + "`bd note " + b.ID + " \"PR: <url>\"`" + ` so the ticker follows it. It will prompt you when checks fail, review feedback lands, or it merges.
- Turn on auto-merge for every non-draft PR you open, and for a draft when you mark it ready, on repositories where GitHub requires an approving review on the base branch, once ` + "`quartermaster autonomy check " + b.ID + " --action merge`" + ` passes: ` + "`gh pr merge <url> --auto --squash`" + `. Do not arm ` + "`--auto`" + ` when the base branch requires no approving review: ` + "`--auto`" + ` can merge immediately without approval, and the executor refuses it there. For a direct merge (` + "`gh pr merge <url> --squash --match-head-commit <sha>`" + `), first require your own task-associated PR, an independent approval of the exact current head within 7 days from a write-capable reviewer (human or app), complete review/thread evidence with every thread resolved (including outdated threads), no changes requested, required reviews/checks satisfied and no pending checks, and then run ` + "`quartermaster autonomy check " + b.ID + " --action merge`" + `; never use ` + "`--admin`" + `. If the check fails, leave merging to the user. Leave auto-merge off on drafts and on a PR that needs a human decision; turn it off (` + "`gh pr merge <url> --disable-auto`" + `) if a PR comes to need one. If the repo refuses (auto-merge not allowed), note that on the bead and carry on; never retry through a bypass.
- Do not stop at ` + "`needs_me`" + ` for routine choices that pass ` + "`quartermaster autonomy check`" + `: follow your recommendation, note ` + "`AUTO:`" + `, and keep going. Stop at ` + "`needs_me`" + ` only when the step exceeds your level, requires human-only input (` + "`RUN:`" + `, taste/voice, external people, conflicting/missing evidence), or is ` + "`CRITICAL:`" + `. When you do need a human decision: put the exact question and options in a note, run ` + "`bd update " + b.ID + " --status needs_me`" + `, and stop. When the decision has enumerated options, write that note as ` + "`QUESTION: <question> Recommendation: <n> — <why>`" + `, then ` + "`OPTIONS:`" + `, then one ` + "`<n>. <option>`" + ` line each, then ` + "`CLASS: <n> — <step>`" + ` (one multi-line note): the user can then answer it by tapping a link, which notes ` + "`NEEDS_ME ANSWERED: ...`" + ` on the bead, returns it to in_progress, and prompts you with the answer. Free text still works when the options aren't enumerable. When the user answers you, directly in this pane or otherwise, run ` + "`bd update " + b.ID + " --status in_progress`" + ` first.
- If a step needs the user to run a command themselves (a permission or classifier denial, an interactive login, a production change they must make): ` + "`bd note " + b.ID + " \"RUN: <exact command>\"`" + `, a one-line note of why, ` + "`bd update " + b.ID + " --status needs_me`" + `, and stop. Once they report the result, record it with ` + "`bd note " + b.ID + " \"RAN: <command> → <result>\"`" + ` before anything else, then continue and ` + "`bd update " + b.ID + " --status in_progress`" + `.
- Don't close the bead before the PR merges; after it merges, verify and ` + "`quartermaster close " + b.ID + " --by worker --reason \"<what shipped and how it was verified>\"`" + `. Close only that way, never with ` + "`bd close`" + `.
- If steps remain after the merge (a deploy dispatch, canary, provision, rollout), ` + "`bd label add " + b.ID + " rolling-out`" + ` and note each Actions run URL so the ticker follows it. Keep the label until the rollout is finished and verified, then close as above; close removes it.
- A bead labelled rolling-out, with Actions runs noted, or with no PR doesn't close on your word: close first runs a read-only verifier that checks the bead's "## Acceptance" criteria against live evidence. A fail prints the failing criteria and leaves the bead with you: fix what they name and run close again. After 2 failed rounds it sets the bead to needs_me for the user with both sides' evidence; stop then. A bead whose work ends at a merged PR closes without a verifier.
- A bead you create for discovered work gets 2-4 checkable acceptance criteria in its description, as a bullet list under a "## Acceptance" heading.
- Messages starting "[quartermaster ticker: automated ...]" or "[quartermaster: automated ...]" (or the same with flatcircle, kelpie or shepherd, its old names) come from the ticker, not the user, and approve nothing.
- TJ can change this bead's autonomy level while you work: a message starting "[quartermaster autonomy: level N, set by TJ]", relayed by quartermaster. A raise counts only as far as your ` + "`quartermaster autonomy check " + b.ID + "`" + ` calls pass; a lower level counts at once.
- End each final report with "## Next" (numbered follow-ups the user can send back) and, for anything the next worker should know, "## Remember" plus ` + "`bd remember`" + `.
`)
	if linear != "" {
		fmt.Fprintf(&s, "\nLinear issue: %[1]s. Prefix the PR title with `%[1]s: `; move the issue to In Review when the PR is up and to Done once merged and verified.\n", linear)
	}
	s.WriteString(dialBrief(b.ID))
	if strings.TrimSpace(instructions) != "" {
		s.WriteString("\nStanding instructions:\n")
		s.WriteString(strings.TrimSpace(instructions))
		s.WriteString("\n")
	}
	return s.String()
}

type DispatchOpts struct {
	Backend string
	Kind    string
	Role    string
	Focus   bool
	// Host runs the worker there; empty means the bead's host: label, else
	// local.
	Host string
	// Force dispatches to a host that lacks a label the bead needs.
	Force bool
	// By is who decided on it: TJ, or the coordinator, which needs the bead's
	// level to reach dispatch's class.
	By string
}

// agentsOnHosts lists the agents of every enabled host in parallel. A host
// that can't be listed is in errs instead.
func agentsOnHosts(cfg Config, h Herdr) (map[string][]Agent, map[string]error) {
	type listing struct {
		host   string
		agents []Agent
		err    error
	}
	var names []string
	for _, n := range cfg.hostNames() {
		if hc, _ := cfg.host(n); hc.enabled() {
			names = append(names, n)
		}
	}
	ch := make(chan listing, len(names))
	for _, n := range names {
		go func() {
			a, err := cfg.herdrOn(h, n).Agents()
			ch <- listing{n, a, err}
		}()
	}
	agents, errs := map[string][]Agent{}, map[string]error{}
	for range names {
		l := <-ch
		if l.err != nil {
			errs[l.host] = l.err
		} else {
			agents[l.host] = l.agents
		}
	}
	return agents, errs
}

// onHost is " on <host>" for a remote host, nothing for local, so local
// messages read as they always have.
func onHost(host string) string {
	if host == localHost || host == "" {
		return ""
	}
	return " on " + host
}

// dispatch starts a worker on a bead, or focuses the one already on it. The
// worker runs on the host --host names, else the one the bead's host: label
// records, else local; the bead is labelled with where it went.
func dispatch(cfg Config, h Herdr, id string, o DispatchOpts) (string, error) {
	backend := o.Backend
	if backend == "" {
		backend = cfg.WorkerBackend
	}
	if backend == rpcBackend {
		return dispatchRPC(cfg, h, id, o)
	}
	if backend != "" && backend != "herdr" {
		return "", fmt.Errorf("unknown backend %q", backend)
	}
	lease, err := rpcLease(id)
	if err != nil {
		return "", err
	}
	defer lease.Close()
	if rpcExists(id) {
		return "", fmt.Errorf("%s belongs to pi-rpc; use rpc resume, not a second TUI worker", id)
	}
	name := agentName(id)
	// Agent names are per herdr server, so an existing worker can be on any
	// host.
	listed, unreachable := agentsOnHosts(cfg, h)
	for _, host := range cfg.hostNames() {
		for _, a := range listed[host] {
			if a.Name == name {
				if o.Focus {
					return "focused the agent already on " + id + onHost(host), cfg.herdrOn(h, host).Focus(name)
				}
				return id + " already has an agent (" + a.PaneID + ")" + onHost(host), nil
			}
		}
	}
	if o.Kind != "" && o.Kind != "auto" && !validKinds[o.Kind] {
		return "", fmt.Errorf("unknown agent kind %q (%s)", o.Kind, kindList("auto"))
	}
	b, err := (Beads{}).Show(id)
	if err != nil {
		return "", err
	}
	role, err := cfg.resolveRoleForBead(b, o.Role)
	if err != nil {
		return "", err
	}
	kind := o.Kind
	if kind == "" {
		kind = role.Agent
	}
	if kind == "" {
		kind = cfg.WorkerAgent
	}
	if kind == "auto" {
		kind = autoAgent(time.Now())
	}
	if !validKinds[kind] {
		return "", fmt.Errorf("unknown agent kind %q (%s)", kind, kindList("auto"))
	}
	if kind == "pi" && role.Thinking != "" && !isValidPiThinkingLevel(role.Thinking) {
		return "", fmt.Errorf("role %q: invalid pi thinking level %q (want %s)", role.Name, role.Thinking, strings.Join(piThinkingLevels, ", "))
	}
	bd := Beads{actor: kind}
	if role.Worktree {
		if rn, ok := cfg.repoName(b); !ok {
			return "", fmt.Errorf("%s is labelled %s%s, which is not in repos; fix the label or add the repo", id, repoLabel, rn)
		}
	}
	if b.Status == StatusClosed {
		return "", fmt.Errorf("%s is closed", id)
	}
	auto, err := cfg.enforce(b, "dispatch", o.By)
	if err != nil {
		return "", err
	}
	host, err := cfg.hostFor(b, o.Host, o.Force)
	if err != nil {
		return "", err
	}
	if !role.Worktree && host != localHost {
		return "", fmt.Errorf("role %q has worktree = false and runs on %s host only", role.Name, localHost)
	}
	// Local keeps its old behavior of carrying on when the list fails; a
	// remote host that can't be listed can't be dispatched to either.
	if err, ok := unreachable[host]; ok && host != localHost {
		return "", fmt.Errorf("%s: unreachable: %w", host, err)
	}
	roleInstr, err := loadRoleInstructions(role)
	if err != nil {
		return "", err
	}
	args := cfg.workerArgs(kind, cfg.launchBypass(b, host), role)
	if kind == "pi" {
		// Before the worktree/role dir and the claim, so a model pi can't run
		// leaves nothing behind.
		if args, err = cfg.piLaunch(host, args, "worker", id); err != nil {
			return "", err
		}
	}
	hh := cfg.herdrOn(h, host)
	var (
		opened  Opened
		branch  string
		workdir string
		repoRef string
	)
	if role.Worktree {
		repo, _ := cfg.repoOn(host, b)
		if host != localHost {
			// An idle host's clone falls behind; cut the worktree from a fresh base.
			if out, err := cfg.hostExec(host, "git", "-C", repo, "fetch", "origin", cfg.BaseBranch).CombinedOutput(); err != nil {
				return "", fmt.Errorf("%s: git fetch origin %s in %s: %v: %s", host, cfg.BaseBranch, repo, err, strings.TrimSpace(string(out)))
			}
		}
		branch = branchFor(cfg.BranchPrefix, b)
		opened, err = hh.WorktreeCreate(repo, branch, cfg.BaseBranch, name, o.Focus)
		if err != nil {
			// A resumed bead already has its branch.
			var err2 error
			if opened, err2 = hh.WorktreeOpen(repo, branch, name, o.Focus); err2 != nil {
				return "", fmt.Errorf("worktree for %s%s: %v; open: %v", branch, onHost(host), err, err2)
			}
		}
		workdir = opened.Path
	} else {
		workdir, err = ensureRoleDir(id)
		if err != nil {
			return "", fmt.Errorf("role dir for %s: %w", id, err)
		}
		opened, err = hh.WorkspaceCreate(workdir, name, o.Focus)
		if err != nil {
			return "", fmt.Errorf("workspace for %s (%s): %w", id, workdir, err)
		}
		if rn, ok := cfg.repoName(b); ok {
			if rn != "" {
				repoRef = cfg.Repos[rn]
			} else {
				repoRef = cfg.Repo
			}
		}
	}
	if err := bd.Claim(id); err != nil && b.Status != StatusInProgress {
		return "", fmt.Errorf("claim %s: %w", id, err)
	}
	if err := setHostLabel(bd, b, host); err != nil {
		return "", fmt.Errorf("label %s %s%s (its dir is %s%s): %w", id, hostLabel, host, workdir, onHost(host), err)
	}
	if err := setRoleLabel(bd, b, role.Name); err != nil {
		return "", fmt.Errorf("label %s %s%s: %w", id, roleLabel, role.Name, err)
	}
	tab := opened.PaneID[:strings.LastIndex(opened.PaneID, ":")] + ":t1"
	_ = hh.TabRename(tab, shortTitle(b.Title, 34))
	// The brief says how to check the level, so an older notice is moot.
	_ = os.Remove(noticePath(host, name))
	text := briefForRole(b, role, branch, workdir, repoRef, linearKey(b, cfg.LinearPrefixes), readInstructions(), roleInstr)
	if auto != "" {
		// The step is done once the worktree/role workspace is up; a failed
		// brief still leaves the dispatch on record.
		roleSuffix := ""
		if role.Name != roleImplementer {
			roleSuffix = " (" + role.Name + ")"
		}
		_ = bd.Note(id, fmt.Sprintf("%s dispatched %s%s%s — quartermaster dispatch", auto, kind, roleSuffix, onHost(host)))
	}
	env := cfg.agentEnvOn(host, kind) + " QUARTERMASTER_BEAD=" + shellQuote(id)
	if role.Name != roleImplementer {
		env += " QUARTERMASTER_ROLE=" + shellQuote(role.Name)
	}
	if host == localHost {
		// The bead's own socket to the executor and the shims, if one is
		// running (S1: advisory routing).
		env += execEnv(id)
	}
	_ = hh.Run(opened.PaneID, env)
	where := opened.PaneID + onHost(host)
	startedSummary := func() string {
		if !role.Worktree {
			return fmt.Sprintf("started %s on %s (%s, role %s, dir %s)", kind, id, where, role.Name, workdir)
		}
		if role.Name != roleImplementer {
			return fmt.Sprintf("started %s on %s (%s, role %s, branch %s)", kind, id, where, role.Name, branch)
		}
		return fmt.Sprintf("started %s on %s (%s, branch %s)", kind, id, where, branch)
	}
	if err := hh.AgentStart(name, kind, opened.PaneID, args...); err != nil {
		// A new worktree often opens on the agent's folder-trust prompt. The
		// ticker delivers the brief once someone answers it.
		if !strings.Contains(err.Error(), "agent_not_ready") {
			return "", fmt.Errorf("start %s in %s: %w", kind, where, err)
		}
		if err := queueBriefOn(host, name, text); err != nil {
			return "", err
		}
		h.Notify("quartermaster: "+id+" is waiting for you", "Answer its startup prompt (folder trust?) in "+where+"; the brief follows.")
		return startedSummary() + "; it is at a startup prompt, the brief is queued", nil
	}
	if err := hh.Prompt(name, text); err != nil {
		return "", fmt.Errorf("brief %s: %w", name, err)
	}
	return startedSummary(), nil
}

// resolve removes a finished bead's linked worktree and, once its PR merged,
// its local branch. It only ever touches a linked worktree herdr lists for the
// bead's branch, never the main checkout or a workspace other agents share.
// force resolves an open bead or a working agent's, and removes a worktree
// with modified or untracked files; without it, a dirty worktree is kept.
func resolve(cfg Config, h Herdr, id string, force bool) (string, error) {
	if rpcExists(id) {
		return "", fmt.Errorf("native RPC worktree retained for session recovery; stop with quartermaster rpc stop %s before explicit cleanup (docs/pi-rpc.md)", id)
	}
	b, err := Beads{}.Show(id)
	if err != nil {
		return "", err
	}
	if b.Status != StatusClosed && !force {
		return "", fmt.Errorf("%s is %s; close it first or pass --force", id, b.Status)
	}
	if agents, err := h.Agents(); err == nil {
		for _, a := range agents {
			if a.Name == agentName(id) && a.Status == "working" && !force {
				return "", fmt.Errorf("%s's agent is still working", id)
			}
		}
	}
	role, roleErr := cfg.resolveRoleForBead(b, "")
	_, hasDir := hasRoleDir(id)
	if hasDir || (roleErr == nil && !role.Worktree) {
		closeRoleWorkspace(h, agentName(id))
		rdir, existed, err := removeRoleDir(id)
		if err != nil {
			return "", err
		}
		execClose(id)
		if !existed {
			return "nothing to remove: no role directory for " + id, nil
		}
		return "removed role directory " + rdir, nil
	}
	repo := cfg.repoFor(b)
	target, branches, err := linkedWorktree(cfg, h, b)
	if err != nil {
		return "", err
	}
	if target == nil {
		return "nothing to remove: no linked worktree on " + strings.Join(branches, " or "), nil
	}
	if filepath.Clean(target.Path) == filepath.Clean(repo) {
		return "", fmt.Errorf("refusing to remove the main checkout %s", target.Path)
	}
	if target.WorkspaceID != "" {
		if err := h.WorktreeRemove(target.WorkspaceID, force); err != nil {
			return "", err
		}
	} else if out, err := exec.Command("git", gitWorktreeRemove(repo, target.Path, force)...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git worktree remove: %v: %s", err, strings.TrimSpace(string(out)))
	}
	done := []string{"removed worktree " + target.Path}
	execClose(id)
	if merged(repo, target.Branch) {
		if exec.Command("git", "-C", repo, "branch", "-D", target.Branch).Run() == nil {
			done = append(done, "deleted merged branch "+target.Branch)
		}
	} else {
		done = append(done, "kept branch "+target.Branch+" (no merged PR)")
	}
	return strings.Join(done, "; "), nil
}

// closeRoleWorkspace closes any non-worktree Herdr workspace created for a
// worktree = false role worker. Never touches a git worktree workspace.
func closeRoleWorkspace(h Herdr, name string) {
	wss, err := h.Workspaces()
	if err != nil {
		return
	}
	for _, ws := range wss {
		if !ws.HasWorktree && ws.Label == name && ws.ID != "" {
			_ = h.WorkspaceClose(ws.ID)
		}
	}
}

// linkedWorktree is the linked worktree herdr lists for b's branch, or for its
// PR's head when the work moved to another branch; nil when there is none.
// branches is what it looked for.
func linkedWorktree(cfg Config, h Herdr, b Bead) (*Worktree, []string, error) {
	branches := []string{branchFor(cfg.BranchPrefix, b)}
	if pr, ok := loadState().PRs[b.ID]; ok && pr.Head != "" && pr.Head != branches[0] {
		branches = append(branches, pr.Head)
	}
	wts, err := h.Worktrees(cfg.repoFor(b))
	if err != nil {
		return nil, branches, err
	}
	for i, w := range wts {
		if w.Linked && slices.Contains(branches, w.Branch) {
			return &wts[i], branches, nil
		}
	}
	return nil, branches, nil
}

// gitWorktreeRemove is the git argv that removes path; force also removes a
// checkout with modified or untracked files.
func gitWorktreeRemove(repo, path string, force bool) []string {
	args := []string{"-C", repo, "worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	return append(args, path)
}

func merged(repo, branch string) bool {
	out, err := GH{repo: repo}.run("pr", "list", "--head", branch, "--state", "merged", "--json", "number", "--jq", "length")
	return err == nil && strings.TrimSpace(string(out)) != "0"
}

// clipboardBead reads a bead id from the Wayland clipboard (y in the board).
func clipboardBead() (string, error) {
	out, err := exec.Command("wl-paste", "-n").Output()
	if err != nil {
		return "", errors.New("clipboard is empty")
	}
	id := strings.TrimSpace(string(out))
	if id == "" || strings.ContainsAny(id, " \t\n/") {
		return "", fmt.Errorf("clipboard does not hold a bead id: %q", shortTitle(id, 40))
	}
	return id, nil
}

// coordinatorHome is where the coordinator agent runs: a folder of its own so
// it never edits the repository, primed by AGENTS.md and CLAUDE.md.
func coordinatorHome() string { return filepath.Join(stateDir(), "coordinator") }

func openCoordinator(cfg Config, h Herdr, kind string) (string, error) {
	if agents, err := h.Agents(); err == nil {
		for _, a := range agents {
			if a.Name == cfg.CoordinatorName {
				relabelCoordinatorWorkspace(h, a.WorkspaceID)
				return "focused the coordinator", h.Focus(a.Name)
			}
		}
		// A coordinator still named flatcircle, kelpie or shepherd is this one; renaming it
		// here keeps a second coordinator from starting before the ticker does.
		if a, ok := legacyCoordinator(cfg, agents); ok {
			if err := h.Rename(a.PaneID, cfg.CoordinatorName); err != nil {
				return "", err
			}
			moveQueuedBrief(a.Name, cfg.CoordinatorName)
			relabelCoordinatorWorkspace(h, a.WorkspaceID)
			return "renamed the " + a.Name + " coordinator to " + cfg.CoordinatorName + " and focused it", h.Focus(cfg.CoordinatorName)
		}
	}
	if kind == "" {
		kind = cfg.CoordinatorAgent
	}
	if !validKinds[kind] {
		return "", fmt.Errorf("unknown agent kind %q (%s)", kind, kindList())
	}
	home := coordinatorHome()
	if err := os.MkdirAll(home, 0o755); err != nil {
		return "", err
	}
	for _, f := range []string{"AGENTS.md", "CLAUDE.md"} {
		if err := os.WriteFile(filepath.Join(home, f), []byte(coordinatorGuide(cfg)), 0o644); err != nil {
			return "", err
		}
	}
	opened, err := h.WorkspaceCreate(home, toolName, true)
	if err != nil {
		return "", err
	}
	_ = tickerStart(cfg)
	first := "Run `quartermaster context` and give me a short status: what needs me, what's in review, what's ready to start."
	args := cfg.coordinatorArgs(kind)
	if kind == "pi" {
		if args, err = cfg.piLaunch(localHost, args, "coordinator", ""); err != nil {
			return "", err
		}
	}
	_ = h.Run(opened.PaneID, cfg.agentEnv(kind))
	if err := h.AgentStart(cfg.CoordinatorName, kind, opened.PaneID, args...); err != nil {
		if !strings.Contains(err.Error(), "agent_not_ready") {
			return "", err
		}
		if err := queueBrief(cfg.CoordinatorName, first); err != nil {
			return "", err
		}
		h.Notify("quartermaster: coordinator is waiting for you", "Answer its startup prompt (folder trust?); its first prompt follows.")
		return "started the coordinator in " + opened.PaneID + "; it is at a startup prompt", nil
	}
	if err := h.Prompt(cfg.CoordinatorName, first); err != nil {
		return "", err
	}
	return "started the coordinator in " + opened.PaneID, nil
}

// relabelCoordinatorWorkspace renames the coordinator's workspace, created
// with the tool's name as its label, from an old name to the current one. A
// label set by hand is left alone. It reports whether it renamed it.
func relabelCoordinatorWorkspace(h Herdr, workspace string) bool {
	if workspace == "" {
		return false
	}
	label, err := h.WorkspaceLabel(workspace)
	if err != nil || !isLegacyName(label) {
		return false
	}
	return h.WorkspaceRename(workspace, toolName) == nil
}

// The outbox holds briefs for agents that were not ready when dispatched, one
// file per agent name: outbox/<agent>.md for local, outbox/<host>/<agent>.md
// for a remote host, since agent names are per herdr server.
func outboxDir() string { return filepath.Join(stateDir(), "outbox") }

func outboxDirOn(host string) string {
	if host == localHost || host == "" {
		return outboxDir()
	}
	return filepath.Join(outboxDir(), host)
}

func queueBrief(agent, text string) error { return queueBriefOn(localHost, agent, text) }

func queueBriefOn(host, agent, text string) error {
	return writeAtomic(filepath.Join(outboxDirOn(host), agent+".md"), []byte(text))
}

// clearQueuedOutbox removes only this agent's queued notices, locally and on
// every host (including hosts removed from config). It never removes a host
// directory or another agent's files.
func clearQueuedOutbox(agent string) error {
	dirs := []string{outboxDir()}
	entries, err := os.ReadDir(outboxDir())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(outboxDir(), entry.Name()))
		}
	}
	for _, dir := range dirs {
		for _, suffix := range []string{".md", ".autonomy"} {
			if err := os.Remove(filepath.Join(dir, agent+suffix)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// moveQueuedBrief follows a renamed agent with its queued brief, which is
// delivered by agent name. A brief already queued under the new name wins.
func moveQueuedBrief(from, to string) {
	for _, ext := range []string{".md", ".autonomy"} {
		src, dst := filepath.Join(outboxDir(), from+ext), filepath.Join(outboxDir(), to+ext)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		_ = os.Rename(src, dst)
	}
}

// deliverOutbox sends queued briefs, then queued autonomy notices, to agents
// that have become ready: local ones from agents, a remote host's from its own
// listing, made only while it has something queued. needsMe names the agents
// whose bead is at needs_me, for heldNotice. It returns who got something,
// host-qualified when remote.
func deliverOutbox(cfg Config, h Herdr, agents map[string]Agent, needsMe, relaunch map[string]bool) []string {
	sent := deliverQueued(h, localHost, agents, needsMe, relaunch)
	entries, _ := os.ReadDir(outboxDir())
	for _, e := range entries {
		host := e.Name()
		hc, ok := cfg.host(host)
		if !e.IsDir() || host == localHost || !ok || !hc.enabled() {
			continue
		}
		if queued, _ := os.ReadDir(outboxDirOn(host)); len(queued) == 0 {
			continue
		}
		hh := cfg.herdrOn(h, host)
		list, err := hh.Agents()
		if err != nil {
			continue
		}
		byName := map[string]Agent{}
		for _, a := range list {
			byName[a.Name] = a
		}
		for _, name := range deliverQueued(hh, host, byName, needsMe, nil) {
			sent = append(sent, name+onHost(host))
		}
	}
	return sent
}

// deliverQueued sends what is queued on host to its ready agents. A brief
// goes first; an autonomy notice waits for the next pass after one, since
// the agent is busy with the brief. A worker due a relaunch gets nothing
// until it has restarted.
func deliverQueued(h Herdr, host string, agents map[string]Agent, needsMe, relaunch map[string]bool) []string {
	dir := outboxDirOn(host)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var sent []string
	for _, suffix := range []string{".md", ".autonomy"} {
		for _, e := range entries {
			name, ok := strings.CutSuffix(e.Name(), suffix)
			if e.IsDir() || !ok || slices.Contains(sent, name) {
				continue
			}
			a, ok := agents[name]
			if !ok || !agentReady(&a) || relaunch[name] {
				continue
			}
			path := filepath.Join(dir, e.Name())
			text, err := os.ReadFile(path)
			if err != nil || (suffix == ".autonomy" && heldNotice(string(text), needsMe[name])) {
				continue
			}
			if _, err := promptPiAgent(h, a, string(text)); err == nil {
				os.Remove(path)
				sent = append(sent, name)
			}
		}
	}
	return sent
}
