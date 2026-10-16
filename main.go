package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// version is set from the release tag at build time.
var version = "dev"

const usage = `quartermaster — one coordinator, a worktree and agent per bead, beads as the only record.
qm is a short alias: qm context is quartermaster context.

Usage:
  quartermaster coordinator [--agent K]                open (or focus) the coordinator agent
  quartermaster dispatch <bead> [--agent K] [--focus]  start a worker on a bead in its own worktree
                [--host H] [--force]                   on host H (default: the bead's host: label, else
                                                       local); --force ignores the bead's needs: labels
  quartermaster host list                              the worker hosts: machine, slots, labels, repos
  quartermaster focus [<bead>] [--agent K]             focus the bead's worker, else dispatch it
                                                       (no bead: read it from the clipboard)
  quartermaster context                                the coordinator's per-turn digest
  quartermaster report [--since 24h|7d|YYYY-MM-DD]     Markdown of what shipped, merged, is in flight, needs you
  quartermaster inbox done [<bead>...]                 mark inbox items handled (all if none given)
  quartermaster resolve <bead> [--force]               remove a finished bead's worktree and merged branch
  quartermaster sweep [--yes]                          list finished linked worktrees; --yes removes the safe ones
  quartermaster resume [<bead>...] [--agent K]         restart exited agents in their worktrees, continuing
                                                       their last conversation (all resumable if none given)
  quartermaster stale [--days N] [--release]           claims with no agent or worktree untouched N days
                                                       (default 7); --release reopens them
  quartermaster verify [<bead>...] [--yes]             stale beads with evidence they are already done; --yes
                                                       closes (with auto_close) or flags them like the ticker
  quartermaster board                                  the board (runs as the plugin popup)
  quartermaster ticker run|start|stop|status           the background loop: sidebar, PR follow-up, nudges
  quartermaster tick                                   one ticker pass in the foreground
  quartermaster configure [--repo PATH]                first-time setup: config, ~/.local/bin link, agent view
  quartermaster unconfigure                            stop the ticker, remove the view and sidebar tokens
  quartermaster version

K is the agent kind: claude, codex or muse; dispatch and focus also take auto.
`

func main() {
	// flatcircle, kelpie and shepherd are old names, kept as deprecated aliases
	// for the transition; qm is a short alias and says nothing.
	if msg := aliasNotice(os.Args[0]); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "quartermaster:", err)
		os.Exit(1)
	}
}

// aliasNotice is the deprecation notice for a binary run under an old name,
// "" for quartermaster or qm.
func aliasNotice(argv0 string) string {
	name := filepath.Base(argv0)
	if !isLegacyName(name) {
		return ""
	}
	return fmt.Sprintf("%s is now %s; the %s name is deprecated and will be removed", name, toolName, name)
}

// interspersed lets flags follow positional arguments (dispatch <bead> --agent codex).
func interspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	return pos, nil
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	// version and help need no config, so they work right after install.
	switch args[0] {
	case "version", "--version", "-V":
		fmt.Println("quartermaster", version)
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	}
	for _, msg := range migrateDirs() {
		fmt.Fprintln(os.Stderr, "quartermaster:", msg)
	}
	cfg, err := loadConfig()
	// configure is how a first-time install gets its config.
	if err != nil && !(errors.Is(err, errNoRepo) && len(args) > 0 && args[0] == "configure") {
		return err
	}
	h := newHerdr()
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	kind := fs.String("agent", "", "agent kind: claude, codex, muse or auto")
	focus := fs.Bool("focus", false, "focus the new workspace")
	repoFlag := fs.String("repo", "", "configure: the repository quartermaster works in, written to config.toml if it has none")
	force := fs.Bool("force", false, "resolve even if the bead is open or its agent is working; dispatch to a host lacking a needed label")
	hostFlag := fs.String("host", "", "dispatch: the worker host to run on")
	sinceFlag := fs.String("since", "24h", "report window: Nh, Nd or YYYY-MM-DD")
	yes := fs.Bool("yes", false, "sweep: remove the clean candidates; verify: act on the verdicts")
	days := fs.Int("days", 7, "stale: untouched for at least this many days")
	release := fs.Bool("release", false, "stale: set each stale claim back to open")
	pos, err := interspersed(fs, rest)
	if err != nil {
		return err
	}
	switch cmd {
	case "coordinator":
		msg, err := openCoordinator(cfg, h, *kind)
		if err != nil {
			h.Notify("quartermaster: coordinator failed", err.Error())
			return err
		}
		fmt.Println(msg)
	case "dispatch":
		if len(pos) != 1 {
			return fmt.Errorf("usage: quartermaster dispatch <bead> [--agent claude|codex|muse] [--focus] [--host H] [--force]")
		}
		msg, err := dispatch(cfg, h, pos[0], DispatchOpts{Kind: *kind, Focus: *focus, Host: *hostFlag, Force: *force})
		if err != nil {
			return err
		}
		fmt.Println(msg)
	case "host":
		if len(pos) != 1 || pos[0] != "list" {
			return fmt.Errorf("usage: quartermaster host list")
		}
		fmt.Print(hostList(cfg))
	case "focus":
		id := ""
		if len(pos) > 0 {
			id = pos[0]
		} else if id, err = clipboardBead(); err != nil {
			h.Notify("quartermaster: no bead", err.Error())
			return err
		}
		msg, err := dispatch(cfg, h, id, DispatchOpts{Kind: *kind, Focus: true})
		if err != nil {
			h.Notify("quartermaster: "+id, err.Error())
			return err
		}
		h.Notify("quartermaster: "+id, msg)
		fmt.Println(msg)
	case "context":
		st := loadState()
		t := newTicker(cfg, log.New(os.Stderr, "", 0))
		// The ticker refreshes PRs; context reads its last pass unless it is down.
		if runningPID() != 0 {
			st.LastGH = time.Now()
		}
		threads, _, err := t.gather(&st, time.Now())
		if err != nil {
			return err
		}
		next, _ := Beads{}.Next()
		ready, _ := Beads{}.Ready()
		inbox, _ := readInbox()
		fmt.Print(renderContext(cfg, threads, next, ready, inbox, st, worktreed(cfg, h, threads, st.PRs), time.Now()))
	case "report":
		now := time.Now()
		since, err := parseSince(*sinceFlag, now)
		if err != nil {
			return err
		}
		r, err := gatherReport(cfg, since)
		if err != nil {
			return err
		}
		fmt.Print(renderReport(r, now))
	case "inbox":
		if len(pos) == 0 || pos[0] != "done" {
			return fmt.Errorf("usage: quartermaster inbox done [<bead>...]")
		}
		n, err := inboxDone(pos[1:])
		if err != nil {
			return err
		}
		fmt.Printf("%d item(s) marked done\n", n)
	case "resolve":
		if len(pos) != 1 {
			return fmt.Errorf("usage: quartermaster resolve <bead> [--force]")
		}
		msg, err := resolve(cfg, h, pos[0], *force)
		if err != nil {
			return err
		}
		fmt.Println(msg)
	case "sweep":
		return sweep(cfg, liveSweepEnv(h), *yes, os.Stdout)
	case "resume":
		lines, err := resume(cfg, h, pos, *kind)
		fmt.Print(strings.Join(append(lines, ""), "\n"))
		return err
	case "stale":
		lines, err := stale(cfg, h, *days, *release)
		fmt.Print(strings.Join(append(lines, ""), "\n"))
		return err
	case "verify":
		st := loadState()
		// The ticker's server: a worker running elsewhere isn't seen otherwise,
		// and its bead would look unattended.
		lines, err := verifyPass(cfg, liveVerifyEnv(cfg, h.on(cfg.coordSocket())), &st, pos, *yes, time.Now())
		fmt.Print(strings.Join(append(lines, ""), "\n"))
		if *yes && !errors.Is(err, errBudget) {
			if serr := saveState(st); serr != nil {
				return serr
			}
		}
		return err
	case "board":
		return runBoard(cfg)
	case "action":
		// Plugin actions: open one of this plugin's panes.
		if len(pos) != 1 {
			return fmt.Errorf("usage: quartermaster action board")
		}
		return h.OpenPane(pos[0])
	case "ticker":
		if len(pos) != 1 {
			return fmt.Errorf("usage: quartermaster ticker run|start|stop|status")
		}
		switch pos[0] {
		case "run":
			return tickerRun(cfg)
		case "start":
			return tickerStart(cfg)
		case "stop":
			return tickerStop()
		case "status":
			tickerStatus(cfg)
		default:
			return fmt.Errorf("unknown ticker command %q", pos[0])
		}
	case "tick":
		st := loadState()
		t := newTicker(cfg, log.New(os.Stderr, "", log.LstdFlags))
		if err := t.once(&st); err != nil {
			return err
		}
		return saveState(st)
	case "startup":
		// Herdr runs this when the plugin loads. Views don't survive a server
		// restart, so the view is set again here.
		if err := setView(socketRPC{herdrSocket()}); err != nil {
			fmt.Fprintln(os.Stderr, "quartermaster: agent view:", err)
		}
		return tickerStart(cfg)
	case "configure":
		lines, err := configure(cfg, *repoFlag)
		fmt.Print(strings.Join(append(lines, ""), "\n"))
		// Run as a plugin action, stdout only reaches herdr's plugin log.
		if os.Getenv("HERDR_PLUGIN_CONTEXT_JSON") != "" {
			if err != nil {
				h.Notify("quartermaster: setup failed", err.Error())
			} else {
				h.Notify("quartermaster: set up", strings.Join(lines, "\n"))
			}
		}
		return err
	case "unconfigure":
		if err := tickerStop(); err != nil {
			return err
		}
		if err := unconfigure(socketRPC{herdrSocket()}, h.Agents, h.ClearState); err != nil {
			return err
		}
		fmt.Println("agent view and sidebar tokens removed")
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, strings.TrimSpace(usage))
	}
	return nil
}
