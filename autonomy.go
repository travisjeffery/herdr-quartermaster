package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// A bead labeled autonomous carries itself to done: its worker goes ahead on
// its own recommendation where it would otherwise stop for TJ, and stops only
// for critical operations. supervised opts a bead out when [autonomy] default
// is on. See docs/design/autonomous-mode.md.
const (
	LabelAutonomous = "autonomous"
	LabelSupervised = "supervised"
)

// AutonomyConfig is the [autonomy] table. Enabled is the lasting kill switch;
// `quartermaster autonomy off` is the quick one. Merge, Apply and Decide turn
// off one kind of step for every autonomous bead.
type AutonomyConfig struct {
	Enabled bool `toml:"enabled"`
	// Default makes every bead autonomous unless it is labeled supervised.
	Default bool `toml:"default"`
	Merge   bool `toml:"merge"`
	Apply   bool `toml:"apply"`
	Decide  bool `toml:"decide"`
}

// autonomyActions are the steps `autonomy check --action` asks about.
var autonomyActions = []string{"decide", "merge", "apply"}

func (a AutonomyConfig) allows(action string) bool {
	switch action {
	case "decide":
		return a.Decide
	case "merge":
		return a.Merge
	case "apply":
		return a.Apply
	}
	return true
}

// autonomyStopPath is the global kill switch: while it exists no bead is
// autonomous, whatever the config says.
func autonomyStopPath() string { return filepath.Join(stateDir(), "autonomy-off") }

func autonomyStopped() bool {
	_, err := os.Stat(autonomyStopPath())
	return err == nil
}

// autonomous says whether b may take a step of kind action ("" for any)
// without TJ right now, and if not, why not.
func autonomous(a AutonomyConfig, b Bead, stopped bool, action string) (bool, string) {
	switch {
	case stopped:
		return false, "autonomy is off (`quartermaster autonomy on` turns it back on)"
	case !a.Enabled:
		return false, "[autonomy] enabled = false in config.toml"
	case b.Status == StatusClosed:
		return false, b.ID + " is closed"
	case b.HasLabel(LabelSupervised):
		return false, b.ID + " is labeled " + LabelSupervised
	case !b.HasLabel(LabelAutonomous) && !a.Default:
		return false, b.ID + " is not labeled " + LabelAutonomous
	case !a.allows(action):
		return false, fmt.Sprintf("[autonomy] %s = false in config.toml", action)
	}
	return true, ""
}

var errNotAutonomous = errors.New("not autonomous")

// autonomyCmd is `quartermaster autonomy on|off|status|check <bead>|grant
// <bead>|revoke <bead>`. check fails (exit 1) unless the bead may take the
// step now; a worker runs it before every step it takes without TJ, so the
// switch reaches a running worker at its next step. grant and revoke change
// one bead's labels mid-flight and tell its worker.
func autonomyCmd(cfg Config, env autonomyEnv, pos []string, action string, w io.Writer) error {
	const use = "usage: quartermaster autonomy on|off|status|check <bead> [--action decide|merge|apply]|grant <bead>|revoke <bead>"
	if len(pos) == 0 {
		return errors.New(use)
	}
	switch pos[0] {
	case "on":
		if err := os.Remove(autonomyStopPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if !cfg.Autonomy.Enabled {
			fmt.Fprintln(w, "autonomy switch on, but [autonomy] enabled = false in config.toml keeps it off")
			return nil
		}
		fmt.Fprintln(w, "autonomy on: beads labeled "+LabelAutonomous+" run without TJ, except critical operations")
	case "off":
		if err := os.MkdirAll(stateDir(), 0o755); err != nil {
			return err
		}
		if err := writeAtomic(autonomyStopPath(), []byte(time.Now().Format(time.RFC3339)+"\n")); err != nil {
			return err
		}
		fmt.Fprintln(w, "autonomy off: every worker stops to needs_me at its next autonomous step")
	case "status":
		active, err := Beads{}.Active()
		if err != nil {
			return err
		}
		fmt.Fprint(w, autonomyStatus(cfg.Autonomy, autonomyStopped(), active))
	case "check":
		if len(pos) != 2 {
			return errors.New(use)
		}
		if action != "" && !slices.Contains(autonomyActions, action) {
			return fmt.Errorf("--action must be one of %s", strings.Join(autonomyActions, ", "))
		}
		b, err := Beads{}.Show(pos[1])
		if err != nil {
			return err
		}
		if ok, why := autonomous(cfg.Autonomy, b, autonomyStopped(), action); !ok {
			return fmt.Errorf("%w: %s; stop for TJ as usual", errNotAutonomous, why)
		}
		what := "act"
		if action != "" {
			what = action
		}
		fmt.Fprintf(w, "%s may %s without TJ; critical operations still stop\n", b.ID, what)
	case "grant", "revoke":
		if len(pos) != 2 {
			return errors.New(use)
		}
		msg, err := setAutonomy(cfg, env, pos[1], pos[0] == "grant")
		if msg != "" {
			fmt.Fprintln(w, msg)
		}
		return err
	default:
		return errors.New(use)
	}
	return nil
}

func autonomyStatus(a AutonomyConfig, stopped bool, active []Bead) string {
	var s strings.Builder
	switch {
	case stopped:
		s.WriteString("autonomy: off (`quartermaster autonomy on`)\n")
	case !a.Enabled:
		s.WriteString("autonomy: off ([autonomy] enabled = false)\n")
	default:
		s.WriteString("autonomy: on\n")
	}
	var steps []string
	for _, act := range autonomyActions {
		if a.allows(act) {
			steps = append(steps, act)
		}
	}
	fmt.Fprintf(&s, "steps: %s\n", orDash(strings.Join(steps, ", ")))
	if a.Default {
		s.WriteString("default: every bead unless labeled " + LabelSupervised + "\n")
	} else {
		s.WriteString("default: only beads labeled " + LabelAutonomous + "\n")
	}
	var auto []string
	for _, b := range active {
		if ok, _ := autonomous(a, b, stopped, ""); ok {
			auto = append(auto, b.ID)
		}
	}
	fmt.Fprintf(&s, "autonomous now: %s\n", orDash(strings.Join(auto, " ")))
	return s.String()
}

// autoNotes are a bead's AUTO: notes, oldest first: each step its worker took
// without TJ. The prefix is case-sensitive; the ticker's own "auto:" notes
// are not decisions.
func autoNotes(notes string) []string {
	var out []string
	for l := range strings.SplitSeq(notes, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "AUTO:") {
			out = append(out, l)
		}
	}
	return out
}

// autonomyGrant is the part of an autonomous bead's brief that lets its
// worker go ahead without TJ. It is the only grant: automated messages from
// the ticker or coordinator still approve nothing.
func autonomyGrant(id string) string {
	check := func(action string) string {
		return "`quartermaster autonomy check " + id + " --action " + action + "`"
	}
	return `
Autonomous mode is on for this bead: where the rules above say to stop for the user, go ahead on your own recommendation instead, except for critical operations. Before each such step run its check below. If the check fails or the command is missing, follow the rules above as usual. After each step, ` + "`bd note " + id + " \"AUTO: <what you did> — <evidence>\"`" + `, and comment on the Linear issue for a decision, an apply or a merge.
- A decision you have a recommendation for (` + check("decide") + `): take it and name the alternatives in the AUTO note. The next step of a staged rollout counts as a decision once the previous step's verification passed.
- Merging (` + check("merge") + `): auto-merge as above, for drafts too once ready. For a stack, when the lower PR merges, retarget the next one (` + "`gh pr edit <url> --base <base>`" + `), and close and reopen it if its checks don't start. If auto-merge isn't allowed, merge with ` + "`gh pr merge <url> --squash --match-head-commit <sha>`" + ` only after the ticker reports it ready to merge. Never approve a PR, yours or anyone's, and never use sneaky-approve or any bot approval: required review stays human.
- Applying a provision or deploy (` + check("apply") + `): first note the change you expect. Then write the preview as JSON (` + "`pulumi preview --json > plan.json`" + ` or ` + "`tofu show -json <planfile> > plan.json`" + `) and run ` + "`quartermaster gate plan plan.json --expect create=N,update=N`" + `. Apply only if it passes, then put its counts in the AUTO note.
- ` + "`/plan`" + `-style trigger comments on the user's own PRs need no check.
Always stop for critical operations, autonomous or not. Start the needs_me question with ` + "`CRITICAL: <category>`" + `. The categories:
  - any delete or destroy, including a replace of a stateful resource
  - deleting data, a bucket, table, secret or volume
  - revoking credentials
  - force-pushing to or deleting a shared branch
  - irreversible operations (object lock/WORM, KMS key deletion)
  - spending money
  - messages to people outside the team
  - anything ` + "`gate plan`" + ` blocks
If you are unsure whether a step is critical, treat it as critical. Never answer Claude Code permission prompts or work around the auto-mode classifier: a denial is a RUN: as above.
`
}

// Mid-flight grant and revoke. TJ runs `quartermaster autonomy grant|revoke
// <bead>` (or the board's U key, or the coordinator on TJ's explicit word);
// the ticker never does. The labels are the grant: the notice only tells a
// running worker, which trusts it only as far as `autonomy check` passes.
const (
	grantTag  = "[quartermaster autonomy: granted by TJ]"
	revokeTag = "[quartermaster autonomy: revoked by TJ]"
)

func grantNotice(id string) string {
	return grantTag + " TJ ran `quartermaster autonomy grant " + id + "`. This is valid only while `quartermaster autonomy check " + id + "` passes; if it fails, ignore this message.\n" +
		autonomyGrant(id) +
		"If you are stopped at needs_me on a question that is not critical and you have a recommendation for, run the decide check, set the bead back to in_progress, take your recommendation, and carry on.\n"
}

func revokeNotice(id string) string {
	return revokeTag + " TJ ran `quartermaster autonomy revoke " + id + "`. Autonomous mode is off for this bead from now on, and `quartermaster autonomy check " + id + "` fails: stop for the user wherever the rules say to, as if your brief had no autonomous-mode section.\n"
}

// noticePath is where a notice for agent waits on host until the agent is
// idle. There is one per agent: a revoke replaces a grant not yet delivered.
func noticePath(host, agent string) string {
	return filepath.Join(outboxDirOn(host), agent+".autonomy")
}

// heldNotice says a queued notice must wait: a revoke for a worker whose bead
// is at needs_me would read as TJ's answer and resume it, and its next check
// fails anyway. A grant goes through, so the worker can take its
// recommendation.
func heldNotice(text string, needsMe bool) bool {
	return needsMe && strings.HasPrefix(text, revokeTag)
}

// autonomyEnv is what setAutonomy reads and changes, so it is testable.
type autonomyEnv struct {
	show     func(id string) (Bead, error)
	setLabel func(id, label string, on bool) error
	note     func(id, text string) error
	// agent is the bead's worker on host, if one is running.
	agent  func(host, name string) (*Agent, error)
	prompt func(host, pane, text string) error
}

func liveAutonomyEnv(cfg Config, h Herdr) autonomyEnv {
	bd := Beads{}
	return autonomyEnv{
		show:     bd.Show,
		setLabel: bd.SetLabel,
		note:     bd.Note,
		agent: func(host, name string) (*Agent, error) {
			agents, err := cfg.herdrOn(h, host).Agents()
			if err != nil {
				return nil, err
			}
			for _, a := range agents {
				if a.Name == name {
					return &a, nil
				}
			}
			return nil, nil
		},
		prompt: func(host, pane, text string) error { return cfg.herdrOn(h, host).Prompt(pane, text) },
	}
}

// setAutonomy grants (on) or revokes autonomy on a bead mid-flight: it sets
// the labels, notes the bead, and tells the worker now if it is idle, else
// queues the notice for the ticker to deliver once it is.
func setAutonomy(cfg Config, env autonomyEnv, id string, on bool) (string, error) {
	b, err := env.show(id)
	if err != nil {
		return "", err
	}
	if b.Status == StatusClosed {
		return "", fmt.Errorf("%s is closed", id)
	}
	add, drop, verb, notice := LabelAutonomous, LabelSupervised, "granted", grantNotice(id)
	if !on {
		add, drop, verb, notice = LabelSupervised, LabelAutonomous, "revoked", revokeNotice(id)
	}
	if b.HasLabel(drop) {
		if err := env.setLabel(id, drop, false); err != nil {
			return "", err
		}
		b.Labels = without(b.Labels, drop)
	}
	if !b.HasLabel(add) {
		if err := env.setLabel(id, add, true); err != nil {
			return "", err
		}
		b.Labels = append(b.Labels, add)
	}
	text := "autonomy revoked by TJ (`quartermaster autonomy revoke`): supervised from now on"
	if on {
		text = "AUTO: autonomy granted by TJ (`quartermaster autonomy grant`) mid-flight"
	}
	if err := env.note(id, text); err != nil {
		return "", err
	}
	out := fmt.Sprintf("autonomy %s on %s", verb, id)
	if ok, why := autonomous(cfg.Autonomy, b, autonomyStopped(), ""); on && !ok {
		out += " (labelled, but not autonomous yet: " + why + ")"
	}
	told, err := tellWorker(env, b, notice, on)
	return out + "; " + told, err
}

// tellWorker delivers the notice to the bead's worker now if it is idle,
// else queues it. An open bead has no worker yet: its brief carries the
// grant at dispatch.
func tellWorker(env autonomyEnv, b Bead, notice string, on bool) (string, error) {
	host, name := beadHost(b), agentName(b.ID)
	if host == "" {
		host = localHost
	}
	path := noticePath(host, name)
	if b.Status == StatusOpen {
		_ = os.Remove(path)
		return "no worker yet; its brief will carry it", nil
	}
	a, err := env.agent(host, name)
	if err != nil {
		return "", fmt.Errorf("find the worker%s: %w", onHost(host), err)
	}
	if a != nil && agentReady(a) && !heldNotice(notice, b.Status == StatusNeedsMe) {
		if err := env.prompt(host, a.PaneID, notice); err != nil {
			return "", fmt.Errorf("tell %s: %w", name, err)
		}
		_ = os.Remove(path)
		return "told " + name + onHost(host), nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := writeAtomic(path, []byte(notice)); err != nil {
		return "", err
	}
	switch {
	case a == nil:
		return "no worker running; queued for when one is resumed", nil
	case b.Status == StatusNeedsMe && !on:
		return name + " waits on you at needs_me; queued until it is answered", nil
	}
	return name + " is " + a.Status + "; queued until it is idle", nil
}
