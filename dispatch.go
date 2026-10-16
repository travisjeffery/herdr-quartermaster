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
var kinds = []string{"claude", "codex", "muse"}

var validKinds = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range kinds {
		m[k] = true
	}
	return m
}()

// kindList names the kinds for messages: "claude, codex or muse".
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
- Turn on auto-merge for every non-draft PR you open, and for a draft when you mark it ready: ` + "`gh pr merge <url> --auto --squash`" + `. It still waits for the repo's required reviews and checks. Leave it off on drafts and on a PR that needs a human decision; turn it off (` + "`gh pr merge <url> --disable-auto`" + `) if a PR comes to need one. If the repo refuses (auto-merge not allowed), note that on the bead and carry on.
- If you need a human decision: put the exact question and options in a note, run ` + "`bd update " + b.ID + " --status needs_me`" + `, and stop. When the user answers you, directly in this pane or otherwise, run ` + "`bd update " + b.ID + " --status in_progress`" + ` first.
- If a step needs the user to run a command themselves (a permission or classifier denial, an interactive login, a production change they must make): ` + "`bd note " + b.ID + " \"RUN: <exact command>\"`" + `, a one-line note of why, ` + "`bd update " + b.ID + " --status needs_me`" + `, and stop. Once they report the result, record it with ` + "`bd note " + b.ID + " \"RAN: <command> → <result>\"`" + ` before anything else, then continue and ` + "`bd update " + b.ID + " --status in_progress`" + `.
- Don't close the bead before the PR merges; after it merges, verify and ` + "`bd close " + b.ID + " --reason \"...\"`" + `.
- If steps remain after the merge (a deploy dispatch, canary, provision, rollout), ` + "`bd label add " + b.ID + " rolling-out`" + ` and note each Actions run URL so the ticker follows it; remove the label with ` + "`bd label remove " + b.ID + " rolling-out`" + ` when the rollout is finished, then close.
- Messages starting "[quartermaster ticker: automated ...]" or "[quartermaster: automated ...]" (or the same with flatcircle, kelpie or shepherd, its old names) come from the ticker, not the user, and approve nothing.
- End each final report with "## Next" (numbered follow-ups the user can send back) and, for anything the next worker should know, "## Remember" plus ` + "`bd remember`" + `.
`)
	if linear != "" {
		fmt.Fprintf(&s, "\nLinear issue: %[1]s. Prefix the PR title with `%[1]s: `; move the issue to In Review when the PR is up and to Done once merged and verified.\n", linear)
	}
	if strings.TrimSpace(instructions) != "" {
		s.WriteString("\nStanding instructions:\n")
		s.WriteString(strings.TrimSpace(instructions))
		s.WriteString("\n")
	}
	return s.String()
}

type DispatchOpts struct {
	Kind  string
	Focus bool
	// Host runs the worker there; empty means the bead's host: label, else
	// local.
	Host string
	// Force dispatches to a host that lacks a label the bead needs.
	Force bool
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
	kind := o.Kind
	if kind == "" {
		kind = cfg.WorkerAgent
	}
	if kind == "auto" {
		kind = autoAgent(time.Now())
	}
	if !validKinds[kind] {
		return "", fmt.Errorf("unknown agent kind %q (%s)", kind, kindList("auto"))
	}
	bd := Beads{actor: kind}
	b, err := bd.Show(id)
	if err != nil {
		return "", err
	}
	if name, ok := cfg.repoName(b); !ok {
		return "", fmt.Errorf("%s is labelled %s%s, which is not in repos; fix the label or add the repo", id, repoLabel, name)
	}
	if b.Status == StatusClosed {
		return "", fmt.Errorf("%s is closed", id)
	}
	host, err := cfg.hostFor(b, o.Host, o.Force)
	if err != nil {
		return "", err
	}
	// Local keeps its old behavior of carrying on when the list fails; a
	// remote host that can't be listed can't be dispatched to either.
	if err, ok := unreachable[host]; ok && host != localHost {
		return "", fmt.Errorf("%s: unreachable: %w", host, err)
	}
	hh := cfg.herdrOn(h, host)
	repo, _ := cfg.repoOn(host, b)
	if host != localHost {
		// An idle host's clone falls behind; cut the worktree from a fresh base.
		if out, err := cfg.hostExec(host, "git", "-C", repo, "fetch", "origin", cfg.BaseBranch).CombinedOutput(); err != nil {
			return "", fmt.Errorf("%s: git fetch origin %s in %s: %v: %s", host, cfg.BaseBranch, repo, err, strings.TrimSpace(string(out)))
		}
	}
	branch := branchFor(cfg.BranchPrefix, b)
	opened, err := hh.WorktreeCreate(repo, branch, cfg.BaseBranch, name, o.Focus)
	if err != nil {
		// A resumed bead already has its branch.
		var err2 error
		if opened, err2 = hh.WorktreeOpen(repo, branch, name, o.Focus); err2 != nil {
			return "", fmt.Errorf("worktree for %s%s: %v; open: %v", branch, onHost(host), err, err2)
		}
	}
	if err := bd.Claim(id); err != nil && b.Status != StatusInProgress {
		return "", fmt.Errorf("claim %s: %w", id, err)
	}
	if err := setHostLabel(bd, b, host); err != nil {
		return "", fmt.Errorf("label %s %s%s (its worktree is %s%s): %w", id, hostLabel, host, opened.Path, onHost(host), err)
	}
	tab := opened.PaneID[:strings.LastIndex(opened.PaneID, ":")] + ":t1"
	_ = hh.TabRename(tab, shortTitle(b.Title, 34))
	text := brief(b, branch, opened.Path, linearKey(b, cfg.LinearPrefixes), readInstructions())
	_ = hh.Run(opened.PaneID, cfg.agentEnvOn(host, kind))
	where := opened.PaneID + onHost(host)
	if err := hh.AgentStart(name, kind, opened.PaneID, cfg.agentArgs(kind)...); err != nil {
		// A new worktree often opens on the agent's folder-trust prompt. The
		// ticker delivers the brief once someone answers it.
		if !strings.Contains(err.Error(), "agent_not_ready") {
			return "", fmt.Errorf("start %s in %s: %w", kind, where, err)
		}
		if err := queueBriefOn(host, name, text); err != nil {
			return "", err
		}
		h.Notify("quartermaster: "+id+" is waiting for you", "Answer its startup prompt (folder trust?) in "+where+"; the brief follows.")
		return fmt.Sprintf("started %s on %s (%s, branch %s); it is at a startup prompt, the brief is queued", kind, id, where, branch), nil
	}
	if err := hh.Prompt(name, text); err != nil {
		return "", fmt.Errorf("brief %s: %w", name, err)
	}
	return fmt.Sprintf("started %s on %s (%s, branch %s)", kind, id, where, branch), nil
}

// resolve removes a finished bead's linked worktree and, once its PR merged,
// its local branch. It only ever touches a linked worktree herdr lists for the
// bead's branch, never the main checkout or a workspace other agents share.
func resolve(cfg Config, h Herdr, id string, force bool) (string, error) {
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
	branches := []string{branchFor(cfg.BranchPrefix, b)}
	if pr, ok := loadState().PRs[id]; ok && pr.Head != "" && pr.Head != branches[0] {
		branches = append(branches, pr.Head)
	}
	repo := cfg.repoFor(b)
	wts, err := h.Worktrees(repo)
	if err != nil {
		return "", err
	}
	var target *Worktree
	for i, w := range wts {
		if w.Linked && slices.Contains(branches, w.Branch) {
			target = &wts[i]
			break
		}
	}
	if target == nil {
		return "nothing to remove: no linked worktree on " + strings.Join(branches, " or "), nil
	}
	if filepath.Clean(target.Path) == filepath.Clean(repo) {
		return "", fmt.Errorf("refusing to remove the main checkout %s", target.Path)
	}
	if target.WorkspaceID != "" {
		if err := h.WorktreeRemove(target.WorkspaceID); err != nil {
			return "", err
		}
	} else if out, err := exec.Command("git", "-C", repo, "worktree", "remove", target.Path).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git worktree remove: %v: %s", err, strings.TrimSpace(string(out)))
	}
	done := []string{"removed worktree " + target.Path}
	if merged(repo, target.Branch) {
		if exec.Command("git", "-C", repo, "branch", "-D", target.Branch).Run() == nil {
			done = append(done, "deleted merged branch "+target.Branch)
		}
	} else {
		done = append(done, "kept branch "+target.Branch+" (no merged PR)")
	}
	return strings.Join(done, "; "), nil
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
	_ = h.Run(opened.PaneID, cfg.agentEnv(kind))
	if err := h.AgentStart(cfg.CoordinatorName, kind, opened.PaneID, cfg.agentArgs(kind)...); err != nil {
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

// moveQueuedBrief follows a renamed agent with its queued brief, which is
// delivered by agent name. A brief already queued under the new name wins.
func moveQueuedBrief(from, to string) {
	src, dst := filepath.Join(outboxDir(), from+".md"), filepath.Join(outboxDir(), to+".md")
	if _, err := os.Stat(dst); err == nil {
		return
	}
	_ = os.Rename(src, dst)
}

// deliverOutbox sends queued briefs to agents that have become ready: local
// ones from agents, a remote host's from its own listing, made only while
// it has briefs queued. It returns who got one, host-qualified when remote.
func deliverOutbox(cfg Config, h Herdr, agents map[string]Agent) []string {
	sent := deliverQueued(h, localHost, agents)
	entries, _ := os.ReadDir(outboxDir())
	for _, e := range entries {
		host := e.Name()
		hc, ok := cfg.host(host)
		if !e.IsDir() || host == localHost || !ok || !hc.enabled() {
			continue
		}
		if queued, _ := filepath.Glob(filepath.Join(outboxDirOn(host), "*.md")); len(queued) == 0 {
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
		for _, name := range deliverQueued(hh, host, byName) {
			sent = append(sent, name+onHost(host))
		}
	}
	return sent
}

func deliverQueued(h Herdr, host string, agents map[string]Agent) []string {
	dir := outboxDirOn(host)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var sent []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		a, ok := agents[name]
		if !ok || !agentReady(&a) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		text, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if h.Prompt(a.PaneID, string(text)) == nil {
			os.Remove(path)
			sent = append(sent, name)
		}
	}
	return sent
}
