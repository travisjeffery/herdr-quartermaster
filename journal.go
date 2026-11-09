package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The journal is the dial's audit trail (docs/design/autonomy-dial.md,
// "Auditing"): bead notes can be edited by the agents being audited and carry
// no time, so every AUTO: step the ticker sees, every prompt answer, level
// change, lease expiry, breaker trip and check is appended to journal.jsonl in
// the state dir with its own timestamp. An agent on this machine can still
// rewrite it; shipping it off the machine is out of scope.
const (
	journalAuto    = "auto"
	journalPrompt  = "prompt"
	journalLevel   = "level"
	journalLease   = "lease"
	journalBreaker = "breaker"
	journalCheck   = "check"
)

type journalRecord struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Bead string    `json:"bead,omitempty"`
	// Class is an AUTO step's or a check's class, -1 when its note has none.
	Class  int    `json:"class"`
	Action string `json:"action,omitempty"`
	Pass   bool   `json:"pass,omitempty"`
	Text   string `json:"text,omitempty"`
}

func journalPath() string { return filepath.Join(stateDir(), "journal.jsonl") }

// appendJournal adds r as one line. O_APPEND keeps concurrent writers (the
// ticker and workers' checks) from overwriting each other's lines.
func appendJournal(r journalRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(journalPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return errors.Join(err, f.Close())
}

// readJournal is the records at or after since, oldest first. A line that
// doesn't parse is skipped rather than failing every check. shortcut: it reads
// the whole file; rotate it if checks get slow.
func readJournal(since time.Time) ([]journalRecord, error) {
	f, err := os.Open(journalPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []journalRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var r journalRecord
		if json.Unmarshal(sc.Bytes(), &r) == nil && !r.At.Before(since) {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

// autoTag is the head of an AUTO: note: "AUTO: [<actor> L<n> C<c>] ".
var autoTag = regexp.MustCompile(`^AUTO:\s*\[(\w+)\s+L(\d)\s+C(\d)\]\s*`)

// autoClass is an AUTO: note's class, -1 when it carries no tag (notes from
// before the dial).
func autoClass(note string) int {
	m := autoTag.FindStringSubmatch(note)
	if m == nil {
		return -1
	}
	c, _ := strconv.Atoi(m[3])
	return c
}

// autoWhat is what an AUTO: note did, without its tag or its evidence.
func autoWhat(note string) string {
	s := strings.TrimPrefix(autoTag.ReplaceAllString(note, ""), "AUTO:")
	s, _, _ = strings.Cut(s, " — ")
	return strings.TrimSpace(s)
}

// autoStep is one AUTO: note and its bead.
type autoStep struct {
	Bead, Note string
}

// autoLines groups steps by class, highest first: 7 and up (and notes with no
// class, whose impact is unknown) each with its evidence, 4–6 one line each
// without it, 3 and below as a count per bead.
func autoLines(steps []autoStep) []string {
	byClass := func(s autoStep) int {
		if c := autoClass(s.Note); c >= 0 {
			return c
		}
		return 10 // unknown sorts above 9
	}
	sorted := slices.Clone(steps)
	slices.SortStableFunc(sorted, func(a, b autoStep) int { return byClass(b) - byClass(a) })
	var out, beads []string
	low := map[string]int{}
	for _, s := range sorted {
		switch c := byClass(s); {
		case c >= 7:
			label := "C?"
			if c <= 9 {
				label = "C" + strconv.Itoa(c)
			}
			out = append(out, fmt.Sprintf("%s %s: %s", s.Bead, label, oneLine(autoTag.ReplaceAllString(s.Note, "AUTO: "))))
		case c >= 4:
			out = append(out, fmt.Sprintf("%s C%d: %s", s.Bead, c, autoWhat(s.Note)))
		default:
			if low[s.Bead] == 0 {
				beads = append(beads, s.Bead)
			}
			low[s.Bead]++
		}
	}
	for _, b := range beads {
		out = append(out, fmt.Sprintf("%s: %d step(s) at class 3 or below", b, low[b]))
	}
	return out
}

// topClass is the highest class among steps, -1 when none has one.
func topClass(steps []autoStep) int {
	top := -1
	for _, s := range steps {
		top = max(top, autoClass(s.Note))
	}
	return top
}

// BudgetConfig is [autonomy.budget]: how many steps a bead may take without TJ
// per rolling hour. 0 is no limit.
type BudgetConfig struct {
	Steps      int `toml:"steps"`      // AUTO steps of any class, per bead
	Applies    int `toml:"applies"`    // class 6 and up, per bead
	Dispatches int `toml:"dispatches"` // coordinator dispatches, global
}

const budgetWindow = time.Hour

// budgetSpent is why a class step on bead id (action, if any) would go over
// budget, "" if it wouldn't. recs is the journal over the last budgetWindow.
// Dispatches are counted by their passed checks, since the coordinator checks
// before every dispatch it makes without TJ.
func budgetSpent(b BudgetConfig, recs []journalRecord, id string, class int, action string) string {
	var steps, applies, dispatches int
	for _, r := range recs {
		switch {
		case r.Kind == journalAuto && r.Bead == id:
			steps++
			if r.Class >= 6 {
				applies++
			}
		case r.Kind == journalCheck && r.Pass && r.Action == "dispatch":
			dispatches++
		}
	}
	switch {
	case b.Steps > 0 && steps >= b.Steps:
		return fmt.Sprintf("budget: %s took %d steps in the last hour ([autonomy.budget] steps = %d)", id, steps, b.Steps)
	case class >= 6 && b.Applies > 0 && applies >= b.Applies:
		return fmt.Sprintf("budget: %s took %d class 6+ steps in the last hour ([autonomy.budget] applies = %d)", id, applies, b.Applies)
	case action == "dispatch" && b.Dispatches > 0 && dispatches >= b.Dispatches:
		return fmt.Sprintf("budget: %d dispatches in the last hour ([autonomy.budget] dispatches = %d)", dispatches, b.Dispatches)
	}
	return ""
}

// The circuit breaker caps a bead at breakerCap when it loops: breakerRun
// failed checks in a row within budgetWindow, or the same AUTO: step
// breakerRun times running. It only lowers; `autonomy level` by TJ lifts it.
const (
	breakerCap = 2
	breakerRun = 3
)

type autonomyBreaker struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// failedRun says whether bead id's last breakerRun checks in recs all failed.
func failedRun(recs []journalRecord, id string) bool {
	n := 0
	for _, r := range slices.Backward(recs) {
		if r.Kind != journalCheck || r.Bead != id {
			continue
		}
		if r.Pass {
			return false
		}
		if n++; n == breakerRun {
			return true
		}
	}
	return false
}

// repeatedRun says whether the last breakerRun AUTO: notes did the same thing.
func repeatedRun(notes []string) bool {
	if len(notes) < breakerRun {
		return false
	}
	last := notes[len(notes)-breakerRun:]
	for _, n := range last[1:] {
		if autoWhat(n) != autoWhat(last[0]) {
			return false
		}
	}
	return true
}

// tripBreaker caps bead id at breakerCap, journals it and raises an inbox
// item. A bead already tripped stays as it was.
func tripBreaker(id, reason string, now time.Time) error {
	s, err := loadAutonomy()
	if err != nil {
		return err
	}
	if _, ok := s.Breakers[id]; ok {
		return nil
	}
	if s.Breakers == nil {
		s.Breakers = map[string]autonomyBreaker{}
	}
	s.Breakers[id] = autonomyBreaker{At: now, Reason: reason}
	if err := saveAutonomy(s); err != nil {
		return err
	}
	summary := fmt.Sprintf("circuit breaker: %s capped at level %d (%s); `quartermaster autonomy level N %s` restores it", id, breakerCap, reason, id)
	return errors.Join(
		appendJournal(journalRecord{At: now, Kind: journalBreaker, Bead: id, Class: -1, Text: reason}),
		writeEvent(Event{Bead: id, Kind: EventBreaker, Summary: summary, At: now}),
	)
}

// journalAuto journals the AUTO: notes th's bead gained since prev, notifies
// TJ of class 7 and up without blocking, and trips the breaker on a repeated
// step. A bead seen for the first time only sets the count, so a restart or
// a long-running bead doesn't land its old notes in the last hour.
func (t Ticker) journalAuto(th Thread, prev Snapshot, seen bool, snap *Snapshot, now time.Time) {
	notes := autoNotes(th.Bead.Notes)
	snap.Auto = len(notes)
	if !seen || prev.Auto >= len(notes) {
		return
	}
	id := th.Bead.ID
	for _, n := range notes[prev.Auto:] {
		c := autoClass(n)
		if err := appendJournal(journalRecord{At: now, Kind: journalAuto, Bead: id, Class: c, Text: n}); err != nil {
			t.log.Printf("journal: %v", err)
		}
		if c >= 7 {
			t.herdr.Notify(fmt.Sprintf("%s: class %d step", id, c), shortTitle(autoWhat(n), 80))
		}
	}
	if !repeatedRun(notes) {
		return
	}
	if lvl, _ := t.cfg.beadLevel(th.Bead, autonomyStopped()); lvl > breakerCap {
		if err := tripBreaker(id, fmt.Sprintf("the same step %d times running: %s", breakerRun, shortTitle(autoWhat(notes[len(notes)-1]), 60)), now); err != nil {
			t.log.Printf("breaker %s: %v", id, err)
		}
	}
}
