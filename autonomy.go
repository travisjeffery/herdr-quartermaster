package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The autonomy dial: every bead has a level from 0 to 9, and every action an
// impact class, how far its output reaches. A bead may take an action without
// TJ when its class is at most the bead's effective level. See
// docs/design/autonomy-dial.md.
const (
	levelLabel = "autonomy:"
	// The on/off labels from before the dial, read as levels.
	LabelAutonomous = "autonomous"
	LabelSupervised = "supervised"
	levelAutonomous = 7 // what autonomous reads as, and grant sets
	levelSupervised = 2 // what supervised reads as, and revoke sets
	// maxStanding is the highest level that is not a lease: 8 and 9 expire.
	maxStanding = 7
	// stopCap is the most any bead gets while autonomy is off: workers can
	// still push their work and stop cleanly.
	stopCap = 2
	// leaseDefault is how long an 8 lasts without --for.
	leaseDefault = 4 * time.Hour
	// classHousekeeping is dispatch, resume, close and resolve: the ticker
	// auto-closes and auto-resolves a bead at this level or above.
	classHousekeeping = 3
)

// AutonomyConfig is the [autonomy] table.
type AutonomyConfig struct {
	// Level is the global dial for beads without their own; `quartermaster
	// autonomy level N` overrides it from the state dir.
	Level int `toml:"level"`
	// Max caps standing levels, global and per bead; only a lease goes past it.
	Max int `toml:"max"`
	// Prod are glob patterns naming production targets for apply, deploy and
	// rollout.
	Prod []string `toml:"prod"`
	// Widespread is how many deletes make an action class 9.
	Widespread int `toml:"widespread"`
	// MergeClass raises merging in a repo, by directory name, whose main
	// deploys on merge.
	MergeClass map[string]int `toml:"merge_class"`

	// The keys from before the dial, folded in by migrate.
	Enabled *bool `toml:"enabled"`
	Default *bool `toml:"default"`
	Merge   *bool `toml:"merge"`
	Apply   *bool `toml:"apply"`
	Decide  *bool `toml:"decide"`
	Install *bool `toml:"install"`
}

// migrate folds the pre-dial keys into the dial: default = true made every
// bead autonomous (7) unless level is set, and enabled = false or a step
// switched off lowers max below that step's class.
func (a *AutonomyConfig) migrate(levelSet bool) {
	if a.Default != nil && *a.Default && !levelSet {
		a.Level = levelAutonomous
	}
	for _, k := range []struct {
		on  *bool
		max int
	}{{a.Enabled, stopCap}, {a.Decide, 1}, {a.Install, 2}, {a.Merge, 3}, {a.Apply, 5}} {
		if k.on != nil && !*k.on {
			a.Max = min(a.Max, k.max)
		}
	}
}

func (a AutonomyConfig) validate() error {
	if a.Level < 0 || a.Level > maxStanding || a.Max < 0 || a.Max > maxStanding {
		return fmt.Errorf("[autonomy] level and max must be 0-%d; 8 and 9 are leases (`quartermaster autonomy level 8 <bead> --for 4h`)", maxStanding)
	}
	for _, p := range a.Prod {
		if _, err := filepath.Match(p, ""); err != nil {
			return fmt.Errorf("[autonomy] prod %q: %w", p, err)
		}
	}
	return nil
}

// autonomyStopPath is the global stop: while it exists every bead is capped
// at stopCap, whatever the config and labels say.
func autonomyStopPath() string { return filepath.Join(stateDir(), "autonomy-off") }

func autonomyStopped() bool {
	_, err := os.Stat(autonomyStopPath())
	return err == nil
}

// autonomyState is the dial's state file: the global level when `autonomy
// level N` set it, and the leases on levels 8 and 9.
type autonomyState struct {
	Level *int `json:"level,omitempty"`
	// Leases are by bead; "" is the global level's.
	Leases map[string]autonomyLease `json:"leases,omitempty"`
}

type autonomyLease struct {
	Level int       `json:"level"`
	Until time.Time `json:"until"`
	// Prev is the level to go back to at expiry; nil drops the bead's own (or
	// the global override).
	Prev *int `json:"prev,omitempty"`
}

func autonomyStatePath() string { return filepath.Join(stateDir(), "autonomy.json") }

func loadAutonomy() (autonomyState, error) {
	var s autonomyState
	b, err := os.ReadFile(autonomyStatePath())
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", autonomyStatePath(), err)
	}
	return s, nil
}

// saveAutonomy replaces the state file. shortcut: two writers at once (the
// ticker expiring a lease while TJ sets a level) lose one write; lock it if
// that is ever seen.
func saveAutonomy(s autonomyState) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	return writeAtomic(autonomyStatePath(), append(b, '\n'))
}

// ownLevel is the level b's labels set, if any: autonomy:N (the lowest of
// several), else supervised (2), which beats autonomous (7).
func ownLevel(b Bead) (int, bool) {
	n, ok := 9, false
	for _, l := range b.Labels {
		if v, cut := strings.CutPrefix(l, levelLabel); cut {
			if d, err := strconv.Atoi(v); err == nil && d >= 0 && d <= 9 {
				n, ok = min(n, d), true
			}
		}
	}
	switch {
	case ok:
		return n, true
	case b.HasLabel(LabelSupervised):
		return levelSupervised, true
	case b.HasLabel(LabelAutonomous):
		return levelAutonomous, true
	}
	return 0, false
}

// withLevel is labels with every level label replaced by autonomy:n, or
// removed for n < 0.
func withLevel(labels []string, n int) []string {
	out := slices.DeleteFunc(slices.Clone(labels), func(l string) bool {
		return strings.HasPrefix(l, levelLabel) || l == LabelAutonomous || l == LabelSupervised
	})
	if n >= 0 {
		out = append(out, levelLabel+strconv.Itoa(n))
	}
	return out
}

func (a AutonomyConfig) global(s autonomyState) int {
	if s.Level != nil {
		return *s.Level
	}
	return a.Level
}

// level is b's effective level: its own or the global one, capped by max
// unless a lease lifts it, and by the stop file. why names the cap that
// lowered it.
func (a AutonomyConfig) level(s autonomyState, b Bead, stopped bool, now time.Time) (int, string) {
	if own, ok := ownLevel(b); ok {
		return a.capped(s, b.ID, own, stopped, now)
	}
	return a.capped(s, "", a.global(s), stopped, now)
}

// capped is level set under the lease kept at key.
func (a AutonomyConfig) capped(s autonomyState, key string, set int, stopped bool, now time.Time) (int, string) {
	limit, why := a.Max, fmt.Sprintf("[autonomy] max = %d", a.Max)
	if l, ok := s.Leases[key]; ok && now.Before(l.Until) && l.Level > limit {
		limit, why = l.Level, "its lease"
	}
	if stopped && limit > stopCap {
		limit, why = stopCap, "autonomy is off (`quartermaster autonomy on`)"
	}
	if set > limit {
		return limit, why
	}
	return set, ""
}

// beadLevel is b's effective level now. A state file that can't be read
// counts as level 0, so nothing goes ahead on a guess.
func (c Config) beadLevel(b Bead, stopped bool) (int, string) {
	s, err := loadAutonomy()
	if err != nil {
		return 0, err.Error()
	}
	return c.Autonomy.level(s, b, stopped, time.Now())
}

// levelMark is b's effective level as the board and sidebar show it: L4, or
// L8 3h left while a lease lifts it past max.
func (a AutonomyConfig) levelMark(s autonomyState, b Bead, stopped bool, now time.Time) string {
	lvl, _ := a.level(s, b, stopped, now)
	key := ""
	if _, own := ownLevel(b); own {
		key = b.ID
	}
	mark := "L" + strconv.Itoa(lvl)
	if l, ok := s.Leases[key]; ok && lvl > a.Max && now.Before(l.Until) {
		mark += " " + age(now, l.Until) + " left"
	}
	return mark
}

// beadMark is b's levelMark now; L0 when the state file can't be read, as
// beadLevel counts it.
func (c Config) beadMark(b Bead) string {
	s, err := loadAutonomy()
	if err != nil {
		return "L0"
	}
	return c.Autonomy.levelMark(s, b, autonomyStopped(), time.Now())
}

// actionClasses are the classes `autonomy check --action` looks up. Apply,
// deploy, rollout, install and merge also depend on the target or the repo;
// see actionClass.
var actionClasses = map[string]int{
	"push":     2,
	"dispatch": 3, "resume": 3, "close": 3, "resolve": 3, "install": 3, "release": 3,
	"merge": 4,
	// decide is the pre-dial brief's check before a worker takes its own
	// recommendation, read as the default dial's reach until briefs carry
	// CLASS: lines.
	"decide": 4,
	"post":   5,
	"apply":  7, "deploy": 7, "rollout": 7,
	"delete": 8, "secret": 8, "message-external": 8, "spend": 8, "approve": 8,
	"classifier": 9, "loosen-settings": 9,
}

// actionClass is the impact class of action on b, at target for an apply,
// deploy or rollout.
func (c Config) actionClass(b Bead, action, target string) (int, error) {
	switch action {
	case "apply", "deploy", "rollout":
		return c.Autonomy.targetClass(target), nil
	case "install":
		if !c.installsSelf(b) {
			return 0, fmt.Errorf("%w: install covers only %s beads; other installs and deploys go through --action apply", errNotAutonomous, selfRepo)
		}
	case "merge":
		// A configured class can raise a merge, never lower it.
		return max(actionClasses[action], c.Autonomy.MergeClass[filepath.Base(filepath.Clean(expandHome(c.repoFor(b))))]), nil
	}
	return actionClasses[action], nil
}

// targetClass is 6 for a target that no prod pattern matches, else 7. With no
// target, or no patterns, nothing tells it isn't production, so it is. A
// pattern matches the whole target or one of its /-separated parts, so
// "*prod*" catches the stack org/app/prod.
func (a AutonomyConfig) targetClass(target string) int {
	if target == "" || len(a.Prod) == 0 {
		return 7
	}
	for _, part := range append(strings.Split(target, "/"), target) {
		for _, p := range a.Prod {
			if ok, _ := filepath.Match(p, part); ok {
				return 7
			}
		}
	}
	return 6
}

var errNotAutonomous = errors.New("not autonomous")

// enforce is the check quartermaster's own commands run before a step on b
// that by takes for TJ (D6). TJ's own steps go ahead; anyone else's need b's
// level to reach the action's class. It returns the AUTO: prefix to note the
// step with, "" for TJ.
func (c Config) enforce(b Bead, action, by string) (string, error) {
	if by == "TJ" {
		return "", nil
	}
	class, err := c.actionClass(b, action, "")
	if err != nil {
		return "", err
	}
	lvl, why := c.beadLevel(b, autonomyStopped())
	return allow(b.ID, by, action, class, lvl, why)
}

// allow says whether by may take what, of class, on bead id at level lvl
// (capped by why), and the AUTO: prefix to note it with.
func allow(id, by, what string, class, lvl int, why string) (string, error) {
	if by == "TJ" {
		return "", nil
	}
	if class > lvl {
		if why != "" {
			why = ", capped by " + why
		}
		return "", fmt.Errorf("%w: %s (class %d) is above %s's level %d%s; ask TJ, and on their go rerun with --by TJ", errNotAutonomous, what, class, id, lvl, why)
	}
	return fmt.Sprintf("AUTO: [%s L%d C%d]", by, lvl, class), nil
}

// selfRepo is quartermaster's own repository. Only its beads may install
// without TJ: installing a merged PR there is a local pull, build and ticker
// restart, not a deploy. Every other repo's installs and deploys go through
// apply and gate plan.
const selfRepo = "herdr-quartermaster"

// installsSelf says whether b's repo is quartermaster's own.
func (c Config) installsSelf(b Bead) bool {
	return filepath.Base(filepath.Clean(expandHome(c.repoFor(b)))) == selfRepo
}

// autonomyOpts are autonomy's flags: --class or --action (with --target)
// for check, --for for level.
type autonomyOpts struct {
	Class  int // -1 when not given
	Action string
	Target string
	For    time.Duration
}

const autonomyUsage = "usage: quartermaster autonomy level [N|- [<bead>...] [--for D]] | check <bead> [--class N | --action A [--target T]] | on | off | grant|revoke <bead>"

// autonomyCmd is `quartermaster autonomy`. check fails (exit 1) unless the
// bead may take the step now; a worker runs it before every step it takes
// without TJ, so a level change reaches a running worker at its next step.
// level shows or sets the dial; grant and revoke are level 7 and 2.
func autonomyCmd(cfg Config, env autonomyEnv, pos []string, o autonomyOpts, w io.Writer) error {
	if len(pos) == 0 {
		return errors.New(autonomyUsage)
	}
	now := time.Now()
	switch pos[0] {
	case "on":
		if err := os.Remove(autonomyStopPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		fmt.Fprintln(w, "autonomy on: beads go ahead up to their level (`quartermaster autonomy level`)")
	case "off":
		if err := os.MkdirAll(stateDir(), 0o755); err != nil {
			return err
		}
		if err := writeAtomic(autonomyStopPath(), []byte(now.Format(time.RFC3339)+"\n")); err != nil {
			return err
		}
		fmt.Fprintf(w, "autonomy off: every bead is capped at level %d, so its worker stops for TJ at its next step above it\n", stopCap)
	case "status", "level":
		if len(pos) == 1 {
			s, err := loadAutonomy()
			if err != nil {
				return err
			}
			active, err := Beads{}.Active()
			if err != nil {
				return err
			}
			fmt.Fprint(w, autonomyStatus(cfg.Autonomy, s, autonomyStopped(), active, now))
			return nil
		}
		if pos[0] == "status" {
			return errors.New(autonomyUsage)
		}
		n := -1
		if pos[1] != "-" {
			var err error
			if n, err = strconv.Atoi(pos[1]); err != nil || n < 0 || n > 9 {
				return fmt.Errorf("level must be 0-9 or -, not %q", pos[1])
			}
		}
		if len(pos) == 2 {
			msg, err := setGlobalLevel(cfg, n, o.For, now)
			if msg != "" {
				fmt.Fprintln(w, msg)
			}
			return err
		}
		for _, id := range pos[2:] {
			msg, err := setLevel(cfg, env, id, n, o.For, "TJ", now)
			if msg != "" {
				fmt.Fprintln(w, msg)
			}
			if err != nil {
				return err
			}
		}
	case "check":
		if len(pos) != 2 {
			return errors.New(autonomyUsage)
		}
		_, known := actionClasses[o.Action]
		switch {
		case o.Action != "" && o.Class >= 0:
			return errors.New("check takes --class or --action, not both")
		case o.Action != "" && !known:
			return fmt.Errorf("unknown --action %q: give its class with --class N instead (actions: %s)", o.Action, strings.Join(slices.Sorted(maps.Keys(actionClasses)), ", "))
		case o.Class > 9:
			return errors.New("--class must be 0-9")
		}
		b, err := env.show(pos[1])
		if err != nil {
			return err
		}
		if b.Status == StatusClosed {
			return fmt.Errorf("%w: %s is closed", errNotAutonomous, b.ID)
		}
		// A bare check is the pre-dial "is this bead autonomous?", so it asks
		// for the level autonomous reads as.
		class, what := levelAutonomous, "an autonomous step"
		switch {
		case o.Class >= 0:
			class, what = o.Class, fmt.Sprintf("a class %d step", o.Class)
		case o.Action != "":
			if class, err = cfg.actionClass(b, o.Action, o.Target); err != nil {
				return err
			}
			what = fmt.Sprintf("%s (class %d)", o.Action, class)
		}
		lvl, why := cfg.beadLevel(b, autonomyStopped())
		if class > lvl {
			if why != "" {
				why = ", capped by " + why
			}
			return fmt.Errorf("%w: %s is above %s's level %d%s; stop for TJ as usual", errNotAutonomous, what, b.ID, lvl, why)
		}
		fmt.Fprintf(w, "%s may take %s without TJ (level %d; advisory)\n", b.ID, what, lvl)
	case "grant", "revoke":
		if len(pos) != 2 {
			return errors.New(autonomyUsage)
		}
		n := levelAutonomous
		if pos[0] == "revoke" {
			n = levelSupervised
		}
		msg, err := setLevel(cfg, env, pos[1], n, 0, "TJ", now)
		if msg != "" {
			fmt.Fprintln(w, msg)
		}
		return err
	default:
		return errors.New(autonomyUsage)
	}
	return nil
}

// advisory is why a passing check is not a guarantee, in every status.
const advisory = "advisory: workers hold their own credentials, so only quartermaster's own commands and the agents' checks hold them to it"

func autonomyStatus(a AutonomyConfig, s autonomyState, stopped bool, active []Bead, now time.Time) string {
	var out strings.Builder
	g, why := a.capped(s, "", a.global(s), stopped, now)
	src := "[autonomy] level"
	if s.Level != nil {
		src = "`autonomy level`"
	}
	fmt.Fprintf(&out, "level: %d (global, from %s", g, src)
	if why != "" {
		fmt.Fprintf(&out, "; capped by %s", why)
	}
	fmt.Fprintf(&out, "), max %d\n%s\n", a.Max, advisory)
	for _, key := range slices.Sorted(maps.Keys(s.Leases)) {
		l := s.Leases[key]
		name, back := key, "its global level"
		if key == "" {
			name, back = "global", "[autonomy] level"
		}
		if l.Prev != nil {
			back = strconv.Itoa(*l.Prev)
		}
		state := "until " + l.Until.Local().Format("Jan 2 15:04")
		if !now.Before(l.Until) {
			state = "expired"
		}
		fmt.Fprintf(&out, "lease: %s at %d %s, then %s\n", name, l.Level, state, back)
	}
	for _, b := range active {
		lvl, why := a.level(s, b, stopped, now)
		line := fmt.Sprintf("%s %d", b.ID, lvl)
		if _, own := ownLevel(b); own {
			line += " (own)"
		}
		if why != "" {
			line += " (capped by " + why + ")"
		}
		out.WriteString(line + "\n")
	}
	return out.String()
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
- Applying a provision or deploy (` + check("apply") + ` ` + "`--target <stack>`" + `): first note the change you expect. Then write the preview as JSON (` + "`pulumi preview --json > plan.json`" + ` or ` + "`tofu show -json <planfile> > plan.json`" + `) and run ` + "`quartermaster gate plan plan.json --expect create=N,update=N --target <stack>`" + `. Apply only if it passes, then put its counts in the AUTO note.
- Installing a merged PR, only for a bead in ` + selfRepo + ` (` + check("install") + `): in the main checkout the installed binary is built from (` + "`readlink -f \"$(command -v quartermaster)\"`" + ` is its ` + "`bin/quartermaster`" + `), run ` + "`git pull --ff-only`" + ` and ` + "`go build -o bin/quartermaster .`" + `, then ` + "`quartermaster ticker stop && quartermaster ticker start`" + `. Smoke-test with ` + "`quartermaster autonomy level`" + ` and a command your change touched, and put the new HEAD and the result in the AUTO note. Other repos' installs and deploys go through the apply step.
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

// A level change mid-flight. TJ runs `quartermaster autonomy level N <bead>`
// (or grant/revoke, the board's U key, or the coordinator on TJ's explicit
// word); the ticker only lowers one, when a lease expires. The labels are the
// grant: the notice only tells a running worker, which trusts it only as far
// as `autonomy check` passes.
const levelTag = "[quartermaster autonomy: level "

// levelNotice tells bead id's worker its level is now n. At the autonomous
// level it carries the brief's grant; below, it takes it back.
func levelNotice(id string, n int, by string) string {
	s := fmt.Sprintf("%s%d, set by %s] %s's autonomy level is %d now.", levelTag, n, by, id, n)
	if n >= levelAutonomous {
		return s + " This is valid only while `quartermaster autonomy check " + id + "` passes; if it fails, ignore this message.\n" +
			autonomyGrant(id) +
			"If you are stopped at needs_me on a question that is not critical and you have a recommendation for, run the decide check, set the bead back to in_progress, take your recommendation, and carry on.\n"
	}
	return s + " Autonomous mode is off for this bead from now on, and `quartermaster autonomy check " + id + "` fails: stop for the user wherever the rules say to, as if your brief had no autonomous-mode section. Steps whose `autonomy check " + id + " --action A` still passes may go ahead.\n"
}

// heldNotice says a queued notice must wait: one that lowers a worker whose
// bead is at needs_me would read as TJ's answer and resume it, and its next
// check fails anyway. One at the autonomous level goes through, so the worker
// can take its recommendation.
func heldNotice(text string, needsMe bool) bool {
	var n int
	_, err := fmt.Sscanf(strings.TrimPrefix(text, levelTag), "%d", &n)
	return needsMe && (!strings.HasPrefix(text, levelTag) || err != nil || n < levelAutonomous)
}

// leaseFor is how long level n lasts: 8 for --for or 4h, 9 only with --for;
// lower levels stand until changed.
func leaseFor(n int, d time.Duration) (time.Duration, error) {
	switch {
	case d < 0:
		return 0, errors.New("--for must be positive")
	case n == 9 && d == 0:
		return 0, errors.New("level 9 needs --for <duration>")
	case n == 8 && d == 0:
		return leaseDefault, nil
	case n < 8 && d > 0:
		return 0, errors.New("--for is only for levels 8 and 9; lower levels stand until changed")
	}
	return d, nil
}

// lease records level n at key for d, keeping what an earlier lease there
// would have gone back to; d == 0 drops any lease at key.
func (s *autonomyState) lease(key string, n int, d time.Duration, prev *int, now time.Time) {
	old, had := s.Leases[key]
	delete(s.Leases, key)
	if d == 0 {
		return
	}
	if had {
		prev = old.Prev
	}
	if s.Leases == nil {
		s.Leases = map[string]autonomyLease{}
	}
	s.Leases[key] = autonomyLease{Level: n, Until: now.Add(d), Prev: prev}
}

func levelWord(n int) string {
	if n < 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

// setGlobalLevel sets the global level, n < 0 going back to [autonomy] level.
// 9 is never global.
func setGlobalLevel(cfg Config, n int, d time.Duration, now time.Time) (string, error) {
	if n == 9 {
		return "", errors.New("level 9 is set only on beads, with --for, never globally")
	}
	d, err := leaseFor(n, d)
	if err != nil {
		return "", err
	}
	s, err := loadAutonomy()
	if err != nil {
		return "", err
	}
	s.lease("", n, d, s.Level, now)
	s.Level = nil
	if n >= 0 {
		s.Level = &n
	}
	if err := saveAutonomy(s); err != nil {
		return "", err
	}
	g, why := cfg.Autonomy.capped(s, "", cfg.Autonomy.global(s), autonomyStopped(), now)
	out := fmt.Sprintf("global autonomy level %d", g)
	if d > 0 {
		out += " until " + now.Add(d).Local().Format("Jan 2 15:04")
	}
	if why != "" {
		out += " (capped by " + why + ")"
	}
	return out + "; " + advisory, nil
}

// autonomyEnv is what setLevel reads and changes, so it is testable.
type autonomyEnv struct {
	show     func(id string) (Bead, error)
	setLabel func(id, label string, on bool) error
	note     func(id, text string) error
	// agent is the bead's worker on host, if one is running.
	agent  func(host, name string) (*Agent, error)
	prompt func(host, pane, text string) error
	// bypassed reports a worker running with its kind's bypass args; nil
	// reads as no.
	bypassed func(host string, a Agent) bool
}

func liveAutonomyEnv(cfg Config, h Herdr) autonomyEnv {
	bd := Beads{}
	return autonomyEnv{
		show:     bd.Show,
		setLabel: bd.SetLabel,
		note:     bd.Note,
		agent:    agentOn(cfg, h),
		prompt:   func(host, pane, text string) error { return cfg.herdrOn(h, host).Prompt(pane, text) },
		bypassed: func(host string, a Agent) bool {
			if host != localHost {
				return false
			}
			argv, err := cfg.herdrOn(h, host).ForegroundArgv(a.PaneID)
			return err == nil && launchedWith(argv, cfg.bypassArgs(a.Kind))
		},
	}
}

// agentOn finds the agent called name on a host's herdr server, nil when none
// is running.
func agentOn(cfg Config, h Herdr) func(host, name string) (*Agent, error) {
	return func(host, name string) (*Agent, error) {
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
	}
}

// setLevel sets bead id's own level mid-flight (n < 0 drops it, back to the
// global level): it records any lease, sets the label, notes the bead, and
// tells the worker now if it is idle, else queues the notice for the ticker
// to deliver once it is. by is who set it.
func setLevel(cfg Config, env autonomyEnv, id string, n int, d time.Duration, by string, now time.Time) (string, error) {
	b, err := env.show(id)
	if err != nil {
		return "", err
	}
	if b.Status == StatusClosed {
		return "", fmt.Errorf("%s is closed", id)
	}
	if d, err = leaseFor(n, d); err != nil {
		return "", err
	}
	s, err := loadAutonomy()
	if err != nil {
		return "", err
	}
	var prev *int
	if p, ok := ownLevel(b); ok {
		prev = &p
	}
	s.lease(id, n, d, prev, now)
	// The lease goes first: a level label without its lease is capped at max.
	if err := saveAutonomy(s); err != nil {
		return "", err
	}
	labels := withLevel(b.Labels, n)
	for _, l := range b.Labels {
		if !slices.Contains(labels, l) {
			if err := env.setLabel(id, l, false); err != nil {
				return "", err
			}
		}
	}
	for _, l := range labels {
		if !b.HasLabel(l) {
			if err := env.setLabel(id, l, true); err != nil {
				return "", err
			}
		}
	}
	b.Labels = labels
	text := fmt.Sprintf("autonomy level %s set by %s", levelWord(n), by)
	if d > 0 {
		text += " until " + now.Add(d).UTC().Format(time.RFC3339)
	}
	if err := env.note(id, text); err != nil {
		return "", err
	}
	lvl, why := cfg.Autonomy.level(s, b, autonomyStopped(), now)
	out := fmt.Sprintf("%s autonomy level %d", id, lvl)
	if why != "" {
		out += " (capped by " + why + ")"
	}
	told, err := tellWorker(env, b, levelNotice(id, lvl, by))
	return out + "; " + told, err
}

// expireLeases puts each lapsed 8 or 9 back to the level it replaced, notes
// it, and returns an inbox event for each, so expiry doesn't wait on the
// worker. The effective level already ignored a lease once it lapsed; this
// brings the labels and TJ up to date.
func expireLeases(cfg Config, env autonomyEnv, now time.Time) ([]Event, error) {
	s, err := loadAutonomy()
	if err != nil {
		return nil, err
	}
	var events []Event
	for _, key := range slices.Sorted(maps.Keys(s.Leases)) {
		l := s.Leases[key]
		if now.Before(l.Until) {
			continue
		}
		back := -1
		if l.Prev != nil {
			back = *l.Prev
		}
		var msg string
		if key == "" {
			msg, err = setGlobalLevel(cfg, back, 0, now)
		} else {
			msg, err = setLevel(cfg, env, key, back, 0, "the ticker (lease expired)", now)
		}
		if err != nil {
			// A closed bead, or bd down: drop the lease anyway, since the
			// effective level already ignores it.
			msg = "lease dropped: " + err.Error()
			s, err := loadAutonomy()
			if err == nil {
				delete(s.Leases, key)
				err = saveAutonomy(s)
			}
			if err != nil {
				return events, err
			}
		}
		events = append(events, Event{Bead: key, Kind: EventLeaseExpired, Summary: fmt.Sprintf("autonomy lease at %d expired: %s", l.Level, msg), At: now})
	}
	return events, nil
}

// tellWorker delivers the notice to the bead's worker now if it is idle,
// else queues it. An open bead has no worker yet: its brief carries the
// level at dispatch.
func tellWorker(env autonomyEnv, b Bead, notice string) (string, error) {
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
	held := heldNotice(notice, b.Status == StatusNeedsMe)
	// A worker in bypass hears of a level change only after the ticker has
	// relaunched it, if the change calls for that (D8).
	bypass := a != nil && env.bypassed != nil && env.bypassed(host, *a)
	if a != nil && agentReady(a) && !held && !bypass {
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
	case held:
		return name + " waits on you at needs_me; queued until it is answered", nil
	case bypass:
		return name + " runs with bypass args; queued for after the ticker relaunches it, if its level calls for that", nil
	}
	return name + " is " + a.Status + "; queued until it is idle", nil
}

// noticePath is where a notice for agent waits on host until the agent is
// idle. There is one per agent: a newer level replaces one not yet delivered.
func noticePath(host, agent string) string {
	return filepath.Join(outboxDirOn(host), agent+".autonomy")
}
