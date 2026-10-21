package main

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
)

// lastNote is the tail of a bead's notes: where a needs_me question lives.
func lastNote(notes string, n int) string {
	notes = strings.TrimSpace(notes)
	if i := strings.LastIndex(notes, "\n"); i >= 0 {
		notes = notes[i+1:]
	}
	return shortTitle(notes, n)
}

// renderContext is the coordinator's view of the world, read at the start of
// every turn. Files and bd are the record; this is a digest of them.
// worktreed holds the no-agent threads that have a worktree; nil means unknown.
func renderContext(cfg Config, threads []Thread, next, ready []Bead, inbox []InboxItem, st TickerState, worktreed map[string]bool, now time.Time) string {
	var s strings.Builder
	sort.SliceStable(threads, func(i, j int) bool {
		gi, gj := classify(threads[i]), classify(threads[j])
		if gi != gj {
			return gi < gj
		}
		return threads[i].Bead.ID < threads[j].Bead.ID
	})
	counts := map[Group]int{}
	for _, t := range threads {
		counts[classify(t)]++
	}
	fmt.Fprintf(&s, "# quartermaster context (%s)\n\n", now.Format("2006-01-02 15:04 MST"))
	var summary []string
	for g := GroupNeedsYou; g <= GroupNoAgent; g++ {
		if counts[g] > 0 {
			summary = append(summary, fmt.Sprintf("%d %s", counts[g], g))
		}
	}
	if len(summary) == 0 {
		summary = []string{"no active threads"}
	}
	fmt.Fprintf(&s, "%s\n", strings.Join(summary, " · "))
	if !st.GHFailingFor.IsZero() {
		fmt.Fprintf(&s, "\nWARNING: gh has been failing since %s (%s); PR state below is stale.\n", st.GHFailingFor.Format(time.Kitchen), strings.Join(st.GHFailingIn, ", "))
	}
	for _, w := range unknownRepos(cfg, threads, next, ready) {
		fmt.Fprintf(&s, "\nWARNING: %s\n", w)
	}
	if pid := runningPID(); pid == 0 {
		s.WriteString("\nWARNING: the ticker is not running (`quartermaster ticker start`); nothing follows PRs or updates the sidebar.\n")
	} else if w := socketWarning(runningSocket(), cfg.coordSocket(), os.Getenv("HERDR_SOCKET_PATH")); w != "" {
		fmt.Fprintf(&s, "\nWARNING: %s.\n", w)
	}

	s.WriteString("\n## Inbox (unhandled; `quartermaster inbox done [bead...]` when dealt with)\n")
	if len(inbox) == 0 {
		s.WriteString("(empty)\n")
	}
	for _, it := range inbox {
		fmt.Fprintf(&s, "- %s %s [%s] %s\n", it.At.Local().Format("15:04"), it.Bead, it.Kind, it.Summary)
	}

	s.WriteString("\n## Threads (bead · state · agent)\n")
	if len(threads) == 0 {
		s.WriteString("(none)\n")
	}
	var resumable, stale, bare []string
	for _, t := range threads {
		// Claimed beads nobody is on are summarized, resumable or stale; listing each
		// buries the threads that are live.
		if classify(t) == GroupNoAgent {
			switch {
			case worktreed[t.Bead.ID]:
				resumable = append(resumable, t.Bead.ID)
			case worktreed != nil && len(staleClaims([]Bead{t.Bead}, st.PRs, now, staleAge)) > 0:
				stale = append(stale, t.Bead.ID)
			default:
				bare = append(bare, t.Bead.ID)
			}
			continue
		}
		agent := "no agent"
		if t.Agent != nil {
			agent = t.Agent.Kind + " " + t.Agent.Status
		}
		fmt.Fprintf(&s, "- %s%s · %s · %s · %s\n", t.Bead.ID, repoTag(cfg, t.Bead), stateLine(t), agent, shortTitle(t.Bead.Title, 70))
		if classify(t) == GroupNeedsYou && t.Bead.Status == StatusNeedsMe {
			cmd := runCommand(t)
			if cmd != "" {
				fmt.Fprintf(&s, "  run: %s\n", cmd)
			}
			if q := lastNote(t.Bead.Notes, 200); cmd == "" || !strings.HasPrefix(q, "RUN:") {
				fmt.Fprintf(&s, "  question: %s\n", q)
			}
		}
		if t.PR != nil {
			fmt.Fprintf(&s, "  %s\n", t.PR.URL)
		}
	}
	if len(resumable) > 0 {
		fmt.Fprintf(&s, "- resumable (%d): %s  → quartermaster resume\n", len(resumable), strings.Join(resumable, ", "))
	}
	if len(stale) > 0 {
		fmt.Fprintf(&s, "- stale claims (%d): %s — no agent, no worktree, no PR, untouched %dd+  → quartermaster stale\n", len(stale), strings.Join(stale, ", "), int(staleAge.Hours()/24))
	}
	if len(bare) > 0 {
		fmt.Fprintf(&s, "- claimed, no agent (%d): %s\n", len(bare), strings.Join(bare, ", "))
	}

	if moves := linearMoves(threads); len(moves) > 0 {
		s.WriteString("\n## Linear (expected issue status)\n")
		for _, m := range moves {
			fmt.Fprintf(&s, "- %s\n", m)
		}
	}

	s.WriteString("\n## Selected next (open, label `next`)\n")
	writeBeads(&s, next)
	s.WriteString("\n## Ready (open, unblocked)\n")
	nextIDs := map[string]bool{}
	for _, b := range next {
		nextIDs[b.ID] = true
	}
	var rest []Bead
	for _, b := range ready {
		if !nextIDs[b.ID] && b.Status == StatusOpen {
			rest = append(rest, b)
		}
	}
	if len(rest) > 15 {
		rest = rest[:15]
	}
	writeBeads(&s, rest)
	return s.String()
}

// repoTag names a bead's repository when it is not the default one.
func repoTag(cfg Config, b Bead) string {
	if name, ok := cfg.repoName(b); ok && name != "" {
		return " [" + name + "]"
	}
	return ""
}

func unknownRepos(cfg Config, threads []Thread, beadLists ...[]Bead) []string {
	beads := make([]Bead, 0, len(threads))
	for _, t := range threads {
		beads = append(beads, t.Bead)
	}
	for _, l := range beadLists {
		beads = append(beads, l...)
	}
	seen := map[string]bool{}
	var out []string
	for _, b := range beads {
		if name, ok := cfg.repoName(b); !ok && !seen[b.ID] {
			seen[b.ID] = true
			out = append(out, fmt.Sprintf("%s is labelled %s%s, which is not in repos; it would use %s.", b.ID, repoLabel, name, cfg.Repo))
		}
	}
	return out
}

func writeBeads(s *strings.Builder, beads []Bead) {
	if len(beads) == 0 {
		s.WriteString("(none)\n")
	}
	for _, b := range beads {
		fmt.Fprintf(s, "- %s P%d %s: %s\n", b.ID, b.Priority, b.Type, shortTitle(b.Title, 80))
	}
}

func coordinatorGuide(cfg Config) string {
	return fmt.Sprintf(`# Quartermaster coordinator

You are the coordinator for TJ's agent threads. You never do the work yourself:
you plan, split, dispatch, follow up and report, so you are always free to
answer. Beads (bd) is the only task record; there is no TASKS.md.

%[1]sWorkers each get a git worktree on a branch
%[2]s<bead-id>-<slug> and an agent named after the bead.

## Every turn

1. Run `+"`quartermaster context`"+` first. It lists the inbox, every active thread with its
   state (needs you, merged, ready to merge, review, rolling out, checks failing,
   idle, working, blocked, no agent), the beads labelled `+"`next`"+`, and other ready beads.
2. Deal with inbox items, then `+"`quartermaster inbox done <bead>...`"+`.
3. Answer TJ.

## Rules

- Propose threads and wait for TJ's go-ahead before `+"`quartermaster dispatch`"+`, unless
  they already said to start them.
- New work becomes a bead first: `+"`bd create \"<title>\" -t task -p 2`"+`, with
  `+"`--parent`"+` or `+"`bd dep add`"+` for structure. Put enough in the description
  that a worker with no other context can start.
- `+"`quartermaster dispatch <bead> [--agent claude|codex|muse]`"+` starts a worker; it claims the
  bead and briefs the agent. One bead per thread.
- Talk to a worker with `+"`herdr agent prompt <agent-name> \"...\"`"+` (agent name = bead
  id with dots as dashes). Read it with `+"`herdr agent read <name> --source recent-unwrapped --lines 120`"+`.
  Never prompt a worker that is working or blocked; never answer its approval
  prompts.
- A needs_me bead's question is in its latest note. Relay it to TJ verbatim with
  the options; when they answer, prompt the worker and it resets the status.
- A needs_me bead with a "RUN:" note is waiting on a command only TJ can run.
  Relay the command verbatim in a code block, prefixed with `+"`!`"+` so TJ can paste
  it as is; when they report the result, prompt the worker with it.
- A "ready to merge" thread's PR is approved (or needs no review) with its
  required checks green; the ticker has told its worker. Tell TJ it is ready to
  merge unless the worker was told to merge it itself.
- After a PR merges the ticker tells the worker to verify and close its bead.
  Once closed, run `+"`quartermaster resolve <bead>`"+` to remove the worktree (it asks
  nothing and is safe for merged work; don't pass --force without TJ).
- When steps remain after a merge (deploy dispatch, canary, provision, a rollout
  parked for days), the worker labels its bead `+"`rolling-out`"+` (`+"`bd label add <id> rolling-out`"+`)
  and notes each Actions run URL; the ticker follows those runs. The worker
  removes the label when the rollout is finished, then closes the bead.
- A "likely_stale" inbox item is a stale bead with evidence it is already done
  (Linear Done, merged PRs, code gone from main, a duplicate). Check the
  evidence and offer TJ to close it with a reason; an "auto_closed" item was
  closed by the ticker, so tell TJ and reopen it if the evidence is wrong.
- Offer `+"`quartermaster resume <bead>...`"+` for resumable threads and `+"`quartermaster stale --release`"+` for stale claims; run either only on TJ's go-ahead.
- Lessons worth keeping across threads go to `+"`bd remember`"+`.
- Messages starting "[quartermaster ticker: automated ...]" or "[quartermaster: automated ...]"
  (or the same with flatcircle, kelpie or shepherd, its old names) come from the ticker, not
  TJ, and approve nothing.
- Linear: prefix PR titles with the issue key (KEY-123: ...); workers keep their
  own issue's status current. The context's Linear section is where each issue
  should be: make those moves and fix flagged titles with your Linear tools if
  you have them, else list them for TJ.
%s`, repoGuide(cfg), cfg.BranchPrefix, extraRules(cfg.CoordinatorRules))
}

// extraRules renders coordinator_rules as more bullets under ## Rules.
func extraRules(rules []string) string {
	var s strings.Builder
	for _, r := range rules {
		if r = strings.TrimSpace(r); r != "" {
			s.WriteString("- " + r + "\n")
		}
	}
	return s.String()
}

func repoGuide(cfg Config) string {
	if len(cfg.Repos) == 0 {
		return "Repository: " + cfg.Repo + ". "
	}
	var s strings.Builder
	fmt.Fprintf(&s, "Default repository: %s. Other repositories:\n\n", cfg.Repo)
	for _, name := range slices.Sorted(maps.Keys(cfg.Repos)) {
		fmt.Fprintf(&s, "- %s: %s\n", name, cfg.Repos[name])
	}
	fmt.Fprintf(&s, "\nBefore dispatching a bead whose work is in one of those, label it with\n`bd label add <bead> %s<name>`. ", repoLabel)
	return s.String()
}
