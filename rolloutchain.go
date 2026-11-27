package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// First-class chained implement -> rollout-to-target workflow.
//
// An implementer bead can attach a dependent worktree-free role:rollout bead
// carrying a rollout-target:<target> label and a `blocks` dependency on the
// implementer bead (either via `quartermaster dispatch <bead> --rollout-target
// <target>` or `quartermaster rollout chain <bead> --target <target>`).
//
// Crucially, the rollout bead shares the implementer's parent (if any) rather
// than being parented under the implementer bead itself, so the implementer
// has OpenChildren == 0, transitions to merged when its PR merges, and closes
// cleanly — which immediately unblocks the rollout bead.
//
// Dispatching a targeted role:rollout bead (manually, via ticker auto-dispatch
// on unblock, or via inbox-runner) enforces:
//   max(actionClasses["dispatch"], cfg.Autonomy.targetClass(target))
// so a configured [autonomy] dev target is Class 5 (auto-dispatches at L5+),
// other non-prod targets are Class 6 (L6+), and production or unmatched
// targets fail closed at Class 7 (L7+).

var errRolloutChain = errors.New("rollout chain")

// dispatchAutonomyClass computes the impact class for dispatching bead b with
// role. A role:rollout bead carrying rollout-target:<target> takes at least
// the target's rollout class (5 for configured [autonomy] dev, 6 for other
// non-prod, 7 for prod or when prod patterns are unset). All other dispatches
// keep class 3 (actionClasses["dispatch"]).
func (c Config) dispatchAutonomyClass(b Bead, role EffectiveRole) (int, string, error) {
	target, err := rolloutTarget(b)
	if err != nil {
		return 0, "", err
	}
	if role.Name == roleRollout && target != "" {
		return max(actionClasses["dispatch"], c.Autonomy.targetClass(target)), target, nil
	}
	return actionClasses["dispatch"], target, nil
}

// enforceDispatch enforces the autonomy dial and hourly budget for dispatching
// bead b with role.
func (c Config) enforceDispatch(b Bead, role EffectiveRole, by string) (string, error) {
	cls, target, err := c.dispatchAutonomyClass(b, role)
	if err != nil {
		return "", err
	}
	if by == "TJ" {
		return "", nil
	}
	what := "dispatch"
	if role.Name == roleRollout && target != "" {
		what = fmt.Sprintf("rollout dispatch (%s)", target)
	}
	prefix, _, err := c.enforceAtClass(b, "dispatch", what, cls, by)
	return prefix, err
}

// enforceRolloutDispatch enforces the target-aware autonomy dial and budget for
// dispatching a chained role:rollout bead targeting target.
func (c Config) enforceRolloutDispatch(b Bead, target, by string) (string, int, error) {
	if err := validateRolloutTarget(target); err != nil {
		return "", 0, err
	}
	cls := max(actionClasses["dispatch"], c.Autonomy.targetClass(target))
	if by == "TJ" {
		return "", cls, nil
	}
	what := fmt.Sprintf("chained rollout dispatch (%s)", target)
	return c.enforceAtClass(b, "dispatch", what, cls, by)
}

func (c Config) enforceAtClass(b Bead, action, what string, class int, by string) (string, int, error) {
	if by == "TJ" {
		return "", class, nil
	}
	lvl, why := c.beadLevel(b, autonomyStopped())
	prefix, err := allow(b.ID, by, what, class, lvl, why)
	if err != nil {
		return "", class, err
	}
	now := time.Now()
	recs, err := readJournal(now.Add(-budgetWindow))
	if err != nil {
		return "", class, err
	}
	if spent := budgetSpent(c.Autonomy.Budget, recs, b.ID, class, action); spent != "" {
		return "", class, fmt.Errorf("%w: %s; ask TJ, and on their go rerun with --by TJ", errNotAutonomous, spent)
	}
	if err := appendJournal(journalRecord{At: now, Kind: journalCheck, Bead: b.ID, Class: class, Action: action, Pass: true, Text: "by " + by}); err != nil {
		return "", class, err
	}
	return prefix, class, nil
}

// isChainedRolloutBead reports whether b resolves cleanly to role:rollout and
// carries a valid rollout-target:<target> label.
func isChainedRolloutBead(cfg Config, b Bead) (string, bool, error) {
	role, err := cfg.resolveRoleForBead(b, "")
	if err != nil {
		return "", false, err
	}
	if role.Name != roleRollout {
		return "", false, nil
	}
	target, err := rolloutTarget(b)
	if err != nil {
		return "", false, err
	}
	if target == "" {
		return "", false, nil
	}
	return target, true, nil
}

// RolloutChainOpts configures creating or reusing a dependent role:rollout bead
// blocked on an implementer bead.
type RolloutChainOpts struct {
	Target     string
	Title      string
	Acceptance []string
	By         string
	// SkipEnforce skips the class-3 bead-creation autonomy check when the
	// caller already enforced and journaled a higher/matching check (e.g.
	// `dispatch --rollout-target`).
	SkipEnforce bool
}

// RolloutChainResult describes the chained implement -> rollout bead pair.
type RolloutChainResult struct {
	ImplBead    string `json:"impl_bead"`
	RolloutBead string `json:"rollout_bead"`
	Target      string `json:"target"`
	TargetClass int    `json:"target_class"`
	Created     bool   `json:"created"`
	Message     string `json:"message"`
}

type rolloutChainEnv struct {
	show       func(id string) (Bead, error)
	nonClosed  func() ([]Bead, error)
	createBead func(opts BeadCreateOpts) (Bead, error)
	note       func(id, text string) error
	enforce    func(b Bead, action, by string) (string, error)
}

func liveRolloutChainEnv(cfg Config) rolloutChainEnv {
	bd := Beads{}
	return rolloutChainEnv{
		show:       bd.Show,
		nonClosed:  bd.NonClosed,
		createBead: bd.Create,
		note:       bd.Note,
		enforce: func(b Bead, action, by string) (string, error) {
			return cfg.enforce(b, action, by)
		},
	}
}

func beadBlockedBy(b Bead, depID string) bool {
	for _, d := range b.Dependencies {
		if d.Type == "blocks" && d.DependsOn == depID {
			return true
		}
	}
	return false
}

// findChainedRollout returns an existing non-closed role:rollout bead blocked
// by implID for target, if one already exists.
func findChainedRollout(cfg Config, all []Bead, implID, target string) (Bead, bool) {
	for _, b := range all {
		if b.Status == StatusClosed || b.ID == implID {
			continue
		}
		if !beadBlockedBy(b, implID) {
			continue
		}
		t, ok, err := isChainedRolloutBead(cfg, b)
		if err == nil && ok && t == target {
			return b, true
		}
	}
	return Bead{}, false
}

// chainRolloutBead idempotently attaches a worktree-free role:rollout bead
// targeting opts.Target and blocked on implID.
func chainRolloutBead(cfg Config, env rolloutChainEnv, implID string, opts RolloutChainOpts) (RolloutChainResult, error) {
	target := strings.TrimSpace(opts.Target)
	if err := validateRolloutTarget(target); err != nil {
		return RolloutChainResult{}, fmt.Errorf("%w: %v", errRolloutChain, err)
	}
	if env.show == nil {
		return RolloutChainResult{}, fmt.Errorf("%w: bead lookup unavailable", errRolloutChain)
	}
	impl, err := env.show(implID)
	if err != nil {
		return RolloutChainResult{}, err
	}
	if impl.Status == StatusClosed {
		return RolloutChainResult{}, fmt.Errorf("%w: %s is already closed", errRolloutChain, implID)
	}
	if role, err := cfg.resolveRoleForBead(impl, ""); err != nil {
		return RolloutChainResult{}, err
	} else if role.Name != roleImplementer {
		return RolloutChainResult{}, fmt.Errorf("%w: %s has role %q; rollout chaining attaches to an implementer bead", errRolloutChain, implID, role.Name)
	}

	targetCls := max(actionClasses["dispatch"], cfg.Autonomy.targetClass(target))
	if env.nonClosed != nil {
		if all, err := env.nonClosed(); err == nil {
			if existing, ok := findChainedRollout(cfg, all, implID, target); ok {
				return RolloutChainResult{
					ImplBead:    implID,
					RolloutBead: existing.ID,
					Target:      target,
					TargetClass: targetCls,
					Created:     false,
					Message:     fmt.Sprintf("reused existing chained rollout bead %s for %s (target %s, class %d)", existing.ID, implID, target, targetCls),
				}, nil
			}
		}
	}

	by := strings.TrimSpace(opts.By)
	if by == "" {
		by = "coordinator"
	}
	if !opts.SkipEnforce && env.enforce != nil && by != "TJ" {
		if _, err := env.enforce(impl, "dispatch", by); err != nil {
			return RolloutChainResult{}, err
		}
	}
	if env.createBead == nil {
		return RolloutChainResult{}, fmt.Errorf("%w: bead creation unavailable", errRolloutChain)
	}

	title := strings.TrimSpace(opts.Title)
	if title == "" {
		title = fmt.Sprintf("Rollout %s to %s", shortTitle(impl.Title, 48), target)
	}
	labels := []string{roleLabel + roleRollout, rolloutTargetLabel + target}
	if repoName, ok := cfg.repoName(impl); ok && repoName != "" {
		labels = append(labels, repoLabel+repoName)
	}
	// Parent under impl.Parent (sibling) rather than impl.ID so impl keeps
	// OpenChildren == 0 and closes cleanly when its PR merges.
	created, err := env.createBead(BeadCreateOpts{
		Title:       title,
		Type:        "task",
		Priority:    impl.Priority,
		Parent:      impl.Parent,
		Labels:      labels,
		BlockedBy:   []string{impl.ID},
		Description: buildChainedRolloutDescription(impl, target, opts.Acceptance),
	})
	if err != nil {
		return RolloutChainResult{}, fmt.Errorf("create chained rollout bead for %s: %w", implID, err)
	}

	if env.note != nil {
		_ = env.note(impl.ID, fmt.Sprintf("CHAINED ROLLOUT: %s (role:rollout, target=%s, class=%d) blocked on %s; auto-dispatches once %s closes within the autonomy dial",
			created.ID, target, targetCls, impl.ID, impl.ID))
	}
	return RolloutChainResult{
		ImplBead:    impl.ID,
		RolloutBead: created.ID,
		Target:      target,
		TargetClass: targetCls,
		Created:     true,
		Message:     fmt.Sprintf("chained rollout bead %s (target %s, class %d) blocked on %s", created.ID, target, targetCls, impl.ID),
	}, nil
}

func buildChainedRolloutDescription(impl Bead, target string, extraAcceptance []string) string {
	var s strings.Builder
	fmt.Fprintf(&s, "Post-merge rollout and live verification for upstream implementer bead `%s` (%s) targeting `%s`.\n\n", impl.ID, impl.Title, target)
	s.WriteString("## Steps\n\n")
	fmt.Fprintf(&s, "1. Confirm `%s` merged and inspect its merged PR / commit SHA from bead notes.\n", impl.ID)
	s.WriteString("2. Add the `rolling-out` label and record all post-merge build/deploy workflow run URLs in `bd note`.\n")
	fmt.Fprintf(&s, "3. Run `quartermaster autonomy check <this-bead> --action rollout --target %s` (and `quartermaster gate plan --target %s` for any IaC preview) before applying or deploying.\n", target, target)
	fmt.Fprintf(&s, "4. Verify live health and behavior on `%s` against the acceptance criteria below, then close via `quartermaster close`.\n\n", target)
	s.WriteString("## Acceptance\n\n")
	if len(extraAcceptance) > 0 {
		for _, a := range extraAcceptance {
			if a = strings.TrimSpace(a); a != "" {
				fmt.Fprintf(&s, "- %s\n", a)
			}
		}
	} else {
		fmt.Fprintf(&s, "- Upstream change from `%s` is merged and rolled out to `%s`\n", impl.ID, target)
		s.WriteString("- Post-merge build and deploy workflow run(s) completed green with run URL(s) noted\n")
		fmt.Fprintf(&s, "- Live verification on `%s` passed with observed evidence recorded before closing\n", target)
	}
	return s.String()
}

// rolloutChainStatusRow summarizes one chained rollout bead for CLI status.
type rolloutChainStatusRow struct {
	Bead        string   `json:"bead"`
	Status      string   `json:"status"`
	Target      string   `json:"target"`
	TargetClass int      `json:"target_class"`
	Level       int      `json:"level"`
	AutoAllowed bool     `json:"auto_allowed"`
	BlockedBy   []string `json:"blocked_by,omitempty"`
	Title       string   `json:"title"`
}

func buildRolloutStatus(cfg Config, all []Bead, filterID string) []rolloutChainStatusRow {
	nonClosed := make(map[string]bool, len(all))
	for _, b := range all {
		if b.Status != StatusClosed {
			nonClosed[b.ID] = true
		}
	}
	stopped := autonomyStopped()
	var rows []rolloutChainStatusRow
	for _, b := range all {
		if b.Status == StatusClosed {
			continue
		}
		target, ok, err := isChainedRolloutBead(cfg, b)
		if err != nil || !ok {
			continue
		}
		var blockers []string
		for _, d := range b.Dependencies {
			if d.Type == "blocks" && d.DependsOn != "" && nonClosed[d.DependsOn] && !slices.Contains(blockers, d.DependsOn) {
				blockers = append(blockers, d.DependsOn)
			}
		}
		sort.Strings(blockers)
		if filterID != "" && b.ID != filterID && !beadBlockedBy(b, filterID) {
			continue
		}
		cls := max(actionClasses["dispatch"], cfg.Autonomy.targetClass(target))
		lvl, _ := cfg.beadLevel(b, stopped)
		rows = append(rows, rolloutChainStatusRow{
			Bead:        b.ID,
			Status:      b.Status,
			Target:      target,
			TargetClass: cls,
			Level:       lvl,
			AutoAllowed: cls <= lvl,
			BlockedBy:   blockers,
			Title:       b.Title,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Bead < rows[j].Bead })
	return rows
}

const rolloutUsage = `usage:
  quartermaster rollout chain <impl-bead> --target <env|stack> [--title "..."] [--acceptance "..."]... [--by WHO] [--json]
  quartermaster rollout status [<bead>] [--json]`

func rolloutCmd(cfg Config, env rolloutChainEnv, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(rolloutUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "chain":
		fs := flag.NewFlagSet("rollout chain", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		target := fs.String("target", "", "rollout target environment or stack (e.g. dev, latest, staging)")
		title := fs.String("title", "", "optional custom rollout bead title")
		var acceptance stringListFlag
		fs.Var(&acceptance, "acceptance", "custom acceptance criterion (repeatable)")
		by := fs.String("by", "coordinator", "who decided (coordinator, inbox-runner, TJ)")
		asJSON := fs.Bool("json", false, "emit JSON output")
		pos, err := interspersed(fs, rest)
		if err != nil {
			return fmt.Errorf("%v\n%s", err, rolloutUsage)
		}
		if len(pos) != 1 || *target == "" {
			return errors.New(rolloutUsage)
		}
		res, err := chainRolloutBead(cfg, env, pos[0], RolloutChainOpts{
			Target:     *target,
			Title:      *title,
			Acceptance: acceptance,
			By:         *by,
		})
		if err != nil {
			return err
		}
		if *asJSON {
			return writePrettyJSON(out, res)
		}
		fmt.Fprintln(out, res.Message)
		return nil

	case "status":
		fs := flag.NewFlagSet("rollout status", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		asJSON := fs.Bool("json", false, "emit JSON output")
		pos, err := interspersed(fs, rest)
		if err != nil {
			return fmt.Errorf("%v\n%s", err, rolloutUsage)
		}
		if len(pos) > 1 {
			return errors.New(rolloutUsage)
		}
		if env.nonClosed == nil {
			return fmt.Errorf("%w: bead list unavailable", errRolloutChain)
		}
		all, err := env.nonClosed()
		if err != nil {
			return err
		}
		filter := ""
		if len(pos) == 1 {
			filter = pos[0]
		}
		rows := buildRolloutStatus(cfg, all, filter)
		if *asJSON {
			if rows == nil {
				rows = []rolloutChainStatusRow{}
			}
			return writePrettyJSON(out, rows)
		}
		if len(rows) == 0 {
			fmt.Fprintln(out, "(no chained rollout beads)")
			return nil
		}
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "BEAD\tSTATUS\tTARGET\tCLASS\tLEVEL\tAUTO\tBLOCKED_BY\tTITLE")
		for _, r := range rows {
			auto := "no"
			if r.AutoAllowed {
				auto = "yes"
			}
			blk := "-"
			if len(r.BlockedBy) > 0 {
				blk = strings.Join(r.BlockedBy, ",")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\tC%d\tL%d\t%s\t%s\t%s\n",
				r.Bead, r.Status, r.Target, r.TargetClass, r.Level, auto, blk, shortTitle(r.Title, 50))
		}
		return tw.Flush()

	default:
		return fmt.Errorf("unknown rollout command %q\n%s", sub, rolloutUsage)
	}
}
