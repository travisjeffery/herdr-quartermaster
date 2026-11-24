package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// defaultNeedsMeParkedDays is how long an agentless, non-critical needs_me bead
// may go untouched before views group it under Parked / Dormant instead of Active.
const defaultNeedsMeParkedDays = 7

// SnoozeEntry records an active temporary park on a needs_me bead. A snooze
// stays in effect until Until passes, the bead's UpdatedAt advances past
// BeadUpdated (or CreatedAt when BeadUpdated is zero), or the bead's active
// question fingerprint changes.
type SnoozeEntry struct {
	Until       time.Time `json:"until"`
	CreatedAt   time.Time `json:"created_at"`
	BeadUpdated time.Time `json:"bead_updated,omitempty"`
	Asked       string    `json:"asked,omitempty"`
	Reason      string    `json:"reason,omitempty"`
}

// SnoozeState is the durable set of per-bead snooze entries in snooze.json.
type SnoozeState struct {
	Version int                    `json:"version"`
	Beads   map[string]SnoozeEntry `json:"beads"`
}

func snoozePath() string     { return filepath.Join(stateDir(), "snooze.json") }
func snoozeLockPath() string { return filepath.Join(stateDir(), "snooze.lock") }

func loadSnoozeState() SnoozeState {
	st := SnoozeState{Version: 1, Beads: map[string]SnoozeEntry{}}
	b, err := os.ReadFile(snoozePath())
	if err != nil {
		return st
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return SnoozeState{Version: 1, Beads: map[string]SnoozeEntry{}}
	}
	if st.Version == 0 {
		st.Version = 1
	}
	if st.Beads == nil {
		st.Beads = map[string]SnoozeEntry{}
	}
	return st
}

func saveSnoozeState(st SnoozeState) error {
	if st.Version == 0 {
		st.Version = 1
	}
	if st.Beads == nil {
		st.Beads = map[string]SnoozeEntry{}
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(snoozePath(), append(b, '\n'))
}

func mutateSnoozeState(now time.Time, fn func(*SnoozeState) error) error {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(snoozeLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	st := loadSnoozeState()
	// Prune already-expired entries so snooze.json stays bounded.
	for id, e := range st.Beads {
		if !now.Before(e.Until) {
			delete(st.Beads, id)
		}
	}
	if err := fn(&st); err != nil {
		return err
	}
	return saveSnoozeState(st)
}

// activeFor reports whether b currently has a live, unwoken snooze at now.
// A snooze ends automatically when its timer expires, when the bead is updated
// after it was snoozed, or when a different question is asked.
func (s SnoozeState) activeFor(b Bead, now time.Time) (SnoozeEntry, bool) {
	if len(s.Beads) == 0 {
		return SnoozeEntry{}, false
	}
	e, ok := s.Beads[b.ID]
	if !ok {
		return SnoozeEntry{}, false
	}
	if e.Until.IsZero() || !now.Before(e.Until) {
		return SnoozeEntry{}, false
	}
	if !b.UpdatedAt.IsZero() {
		if !e.BeadUpdated.IsZero() && b.UpdatedAt.After(e.BeadUpdated) {
			return SnoozeEntry{}, false
		}
		if e.BeadUpdated.IsZero() && !e.CreatedAt.IsZero() && b.UpdatedAt.After(e.CreatedAt) {
			return SnoozeEntry{}, false
		}
	}
	if e.Asked != "" {
		if cur := asked(b); cur != "" && cur != e.Asked {
			return SnoozeEntry{}, false
		}
	}
	return e, true
}

// parseSnoozeDuration parses a positive duration for `quartermaster snooze`:
// standard Go durations (e.g. 30m, 4h, 1h30m) plus day (Nd) and week (Nw)
// suffixes.
func parseSnoozeDuration(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, errors.New("snooze duration is required (e.g. 4h, 3d, 1w)")
	}
	if nStr, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(nStr)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid snooze duration %q: want positive duration like 4h, 3d, or 1w", raw)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	if nStr, ok := strings.CutSuffix(s, "w"); ok {
		n, err := strconv.Atoi(nStr)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid snooze duration %q: want positive duration like 4h, 3d, or 1w", raw)
		}
		return time.Duration(n) * 7 * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid snooze duration %q: want positive duration like 4h, 3d, or 1w", raw)
	}
	return d, nil
}

type snoozeEnv struct {
	show func(string) (Bead, error)
	now  func() time.Time
}

func liveSnoozeEnv(cfg Config) snoozeEnv {
	return snoozeEnv{
		show: cfg.readBeads().Show,
		now:  time.Now,
	}
}

func snoozeCmd(_ Config, env snoozeEnv, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("snooze", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	clearFlag := fs.Bool("clear", false, "clear an active snooze on the bead")
	reasonFlag := fs.String("reason", "", "optional note explaining why the bead was snoozed")
	jsonFlag := fs.Bool("json", false, "emit snoozes as JSON (when listing)")
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	now := time.Now()
	if env.now != nil {
		now = env.now()
	}

	if *clearFlag {
		if len(pos) != 1 || *reasonFlag != "" || *jsonFlag {
			return errors.New("usage: quartermaster snooze <bead> --clear")
		}
		id := strings.TrimSpace(pos[0])
		if id == "" {
			return errors.New("usage: quartermaster snooze <bead> --clear")
		}
		var existed bool
		if err := mutateSnoozeState(now, func(st *SnoozeState) error {
			_, existed = st.Beads[id]
			delete(st.Beads, id)
			return nil
		}); err != nil {
			return err
		}
		if existed {
			fmt.Fprintf(out, "cleared snooze on %s\n", id)
		} else {
			fmt.Fprintf(out, "%s was not snoozed\n", id)
		}
		return nil
	}

	// No positional args: list currently unexpired snoozes.
	if len(pos) == 0 {
		if *reasonFlag != "" {
			return errors.New("usage: quartermaster snooze <bead> <duration> [--reason R] | quartermaster snooze <bead> --clear")
		}
		st := loadSnoozeState()
		type row struct {
			Bead   string    `json:"bead"`
			Until  time.Time `json:"until"`
			Reason string    `json:"reason,omitempty"`
		}
		rows := make([]row, 0, len(st.Beads))
		for id, e := range st.Beads {
			if now.Before(e.Until) {
				rows = append(rows, row{Bead: id, Until: e.Until, Reason: e.Reason})
			}
		}
		sort.Slice(rows, func(i, j int) bool {
			if !rows[i].Until.Equal(rows[j].Until) {
				return rows[i].Until.Before(rows[j].Until)
			}
			return rows[i].Bead < rows[j].Bead
		})
		if *jsonFlag {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(rows)
		}
		if len(rows) == 0 {
			fmt.Fprintln(out, "(no snoozed beads)")
			return nil
		}
		for _, r := range rows {
			line := fmt.Sprintf("- %s until %s", r.Bead, r.Until.Local().Format("2006-01-02 15:04 MST"))
			if r.Reason != "" {
				line += " — " + r.Reason
			}
			fmt.Fprintln(out, line)
		}
		return nil
	}

	if *jsonFlag || len(pos) != 2 {
		return errors.New("usage: quartermaster snooze <bead> <duration> [--reason R] | quartermaster snooze <bead> --clear")
	}
	id := strings.TrimSpace(pos[0])
	if id == "" {
		return errors.New("bead id is required")
	}
	dur, err := parseSnoozeDuration(pos[1])
	if err != nil {
		return err
	}
	var b Bead
	if env.show != nil {
		b, err = env.show(id)
		if err != nil {
			return err
		}
		if b.Status == StatusClosed {
			return fmt.Errorf("cannot snooze closed bead %s", id)
		}
	}
	until := now.Add(dur)
	entry := SnoozeEntry{
		Until:       until,
		CreatedAt:   now,
		BeadUpdated: b.UpdatedAt,
		Asked:       asked(b),
		Reason:      strings.TrimSpace(*reasonFlag),
	}
	if err := mutateSnoozeState(now, func(st *SnoozeState) error {
		st.Beads[id] = entry
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "snoozed %s until %s (%s)\n", id, until.Local().Format("2006-01-02 15:04 MST"), pos[1])
	return nil
}

// hasLiveUnparkedAgent reports whether t has an active (non-parked) worker.
// Native pi-rpc workers parked by migration/handoff carry Status="blocked" to
// prevent automated prompts, but are not live interactive agents.
func hasLiveUnparkedAgent(t Thread) bool {
	if t.Agent == nil {
		return false
	}
	if t.Agent.Backend == rpcBackend && rpcParked(t.Bead.ID) {
		return false
	}
	return true
}

// hasActivePrompt reports whether t has a live worker approval/question prompt.
func hasActivePrompt(t Thread) bool {
	if t.Prompt != nil {
		return true
	}
	return hasLiveUnparkedAgent(t) && t.Agent.Status == "blocked"
}

// isCriticalNeedsMe reports whether b carries a currently unanswered CRITICAL:
// note or structured critical question card.
func isCriticalNeedsMe(b Bead) bool {
	if b.Status != StatusNeedsMe {
		return false
	}
	if q, ok := parseNeedsMeQuestion(b.Notes); ok && len(q.Lines) > 0 && strings.HasPrefix(q.Lines[0], "CRITICAL:") {
		return true
	}
	if card, ok := loadQuestionCard(b.ID); ok && card.Critical && (card.Hash == "" || !questionCardAnswered(b.Notes, card.Hash)) {
		return true
	}
	return extractNoteCritical(b.Notes)
}

// needsYouParkedKind identifies why a needs_me thread is Parked / Dormant.
const (
	parkedReasonSnoozed = "snoozed"
	parkedReasonDormant = "dormant"
)

// classifyNeedsYou splits needs-you threads into Active vs Parked / Dormant.
//
// Precedence:
//  1. Active approval prompt (blocked live agent or parsed prompt) -> Active.
//  2. Live, unwoken snooze -> Parked ("snoozed").
//  3. Live unparked agent -> Active.
//  4. Unsnoozed CRITICAL: question/card -> Active.
//  5. No live agent and untouched at least needs_me_parked_days (default 7d;
//     0 disables age-based parking) -> Parked ("dormant").
//  6. Otherwise (recent activity or unknown UpdatedAt) -> Active.
func classifyNeedsYou(cfg Config, t Thread, snoozes SnoozeState, now time.Time) (parked bool, reason string, snoozeUntil time.Time) {
	if hasActivePrompt(t) {
		return false, "", time.Time{}
	}
	if e, ok := snoozes.activeFor(t.Bead, now); ok {
		return true, parkedReasonSnoozed, e.Until
	}
	if hasLiveUnparkedAgent(t) {
		return false, "", time.Time{}
	}
	if isCriticalNeedsMe(t.Bead) {
		return false, "", time.Time{}
	}
	threshold := cfg.needsMeParkedAfter()
	if threshold > 0 && !t.Bead.UpdatedAt.IsZero() && now.Sub(t.Bead.UpdatedAt) >= threshold {
		return true, parkedReasonDormant, time.Time{}
	}
	return false, "", time.Time{}
}

// formatParkedReason renders a concise human-readable reason why a needs_me
// thread is parked (e.g. "snoozed until 2026-11-27 16:00" or "dormant 12d, no agent").
func formatParkedReason(kind string, snoozeUntil, updatedAt, now time.Time) string {
	switch kind {
	case parkedReasonSnoozed:
		if !snoozeUntil.IsZero() {
			return "snoozed until " + snoozeUntil.Local().Format("2006-01-02 15:04")
		}
		return "snoozed"
	case parkedReasonDormant:
		if !updatedAt.IsZero() && !now.IsZero() {
			if d := age(updatedAt, now); d != "" {
				return fmt.Sprintf("dormant %s, no agent", d)
			}
		}
		return "dormant, no agent"
	default:
		return kind
	}
}

// formatParkedBreakdown formats a parked count summary such as "3: 1 snoozed, 2 dormant"
// or "2 dormant".
func formatParkedBreakdown(snoozed, dormant int) string {
	total := snoozed + dormant
	var parts []string
	if snoozed > 0 {
		parts = append(parts, fmt.Sprintf("%d snoozed", snoozed))
	}
	if dormant > 0 {
		parts = append(parts, fmt.Sprintf("%d dormant", dormant))
	}
	if len(parts) == 0 {
		return strconv.Itoa(total)
	}
	if len(parts) == 1 {
		return fmt.Sprintf("%d: %s", total, parts[0])
	}
	return fmt.Sprintf("%d: %s", total, strings.Join(parts, ", "))
}

// linesAfterAnswer returns the lines after the last answer boundary in lines,
// so already-answered questions never resurface as live.
func linesAfterAnswer(lines []string) []string {
	base := 0
	for i, l := range lines {
		if isAnswerBoundaryLine(l) {
			base = i + 1
		}
	}
	return lines[base:]
}

// isClassLine reports whether l is a CLASS: line.
func isClassLine(l string) bool {
	_, _, ok := parseClassLine(l)
	return ok
}

// isExplicitQuestionDirective reports whether l begins with one of the explicit
// needs_me directive prefixes: CRITICAL:, QUESTION: (case-insensitive), or RUN:.
func isExplicitQuestionDirective(l string) bool {
	if strings.HasPrefix(l, "CRITICAL:") || strings.HasPrefix(l, "RUN:") {
		return true
	}
	return len(l) >= len("QUESTION:") && strings.EqualFold(l[:len("QUESTION:")], "QUESTION:")
}

// isNonAnswerHousekeepingNote reports whether l is a housekeeping note appended
// while a bead waits (e.g. AUTO: handoff, likely_stale rejected, PR:, Linear:,
// MILESTONE:) that does not answer or supersede an open question.
func isNonAnswerHousekeepingNote(l string) bool {
	if isAnswerBoundaryLine(l) || isExplicitQuestionDirective(l) || l == "OPTIONS:" || isClassLine(l) {
		return false
	}
	return isHousekeepingNote(l)
}

// needsMeSummaryNote picks the best 1-line summary for a needs_me bead,
// preferring the latest unanswered CRITICAL: / QUESTION: / RUN: / OPTIONS:
// note over housekeeping notes (AUTO: ... handoff ..., likely_stale rejected,
// etc.), and falling back to lastNote only when no non-housekeeping note exists.
func needsMeSummaryNote(notes string, maxLen int) string {
	if q, ok := parseNeedsMeQuestion(notes); ok && len(q.Lines) > 0 {
		return shortTitle(strings.Join(q.Lines, " "), maxLen)
	}
	if active := strings.TrimSpace(activeQuestionText(notes)); active != "" {
		lines := noteLines(active)
		if idx := slices.Index(lines, "OPTIONS:"); idx > 0 {
			lines = lines[:idx]
		}
		if len(lines) > 0 {
			return shortTitle(strings.Join(lines, " "), maxLen)
		}
	}
	return lastNote(notes, maxLen)
}
