package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The board shows every bead that isn't closed. Threads (in_progress,
// needs_me, blocked) sit in their Group; open beads follow, those labelled
// next first, then the backlog.
const (
	sectionNext    = GroupNoAgent + 1
	sectionBacklog = GroupNoAgent + 2
)

func sectionName(g Group) string {
	switch g {
	case sectionNext:
		return "next"
	case sectionBacklog:
		return "backlog"
	}
	return g.String()
}

// boardStatuses are what one bd list asks for: every status the board shows.
// bd costs a second or more per call whatever it returns, and calls queue on
// its lock behind every worker's, so the board makes one.
var boardStatuses = []string{StatusOpen, StatusInProgress, StatusNeedsMe, StatusBlocked}

// boardData is what the rows are built from. Beads come from bd, slowly and
// rarely; agents, ticker state and inbox from herdr and local files, cheaply
// and often.
type boardData struct {
	Beads   []Bead    `json:"beads"`
	BeadsAt time.Time `json:"beads_at"`
	Agents  []Agent   `json:"-"`
	State   TickerState
	Inbox   []InboxItem
}

func boardCachePath() string { return filepath.Join(stateDir(), "board.json") }

// loadBoardCache is the beads the last board saw, for a first paint that
// doesn't wait on bd. A missing or unreadable cache is an empty board.
func loadBoardCache(path string) ([]Bead, time.Time) {
	var c boardData
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &c) != nil {
		return nil, time.Time{}
	}
	return c.Beads, c.BeadsAt
}

func saveBoardCache(path string, beads []Bead, at time.Time) error {
	b, err := json.Marshal(boardData{Beads: beads, BeadsAt: at})
	if err != nil {
		return err
	}
	return writeAtomic(path, b)
}

// sortMode orders rows within a section.
type sortMode int

const (
	sortPriority sortMode = iota // P0 first, then most recently active
	sortRecent                   // most recently active first
	sortOldest                   // least recently active first: what's stuck
)

func (s sortMode) String() string {
	return [...]string{"priority", "recent", "oldest"}[s]
}

// buildRows lays out every bead in data as sections of rows, in Group order
// and then next and backlog, each sorted by mode. Empty sections are left out.
func buildRows(cfg Config, data boardData, mode sortMode) []boardRow {
	byName := map[string]Agent{}
	for _, a := range data.Agents {
		if a.Name != "" {
			byName[a.Name] = a
		}
	}
	live := map[string]bool{}
	for _, b := range data.Beads {
		live[b.ID] = true
	}
	sections := map[Group][]boardRow{}
	for _, b := range data.Beads {
		var r boardRow
		switch b.Status {
		case StatusInProgress, StatusNeedsMe, StatusBlocked:
			r = threadRow(data.State, threadFor(cfg, data.State, b, byName))
		case StatusOpen:
			r = openRow(cfg, data.State, b, live)
			if a, ok := byName[agentName(b.ID)]; ok {
				r.agent = &a
			}
		default:
			continue
		}
		sections[r.group] = append(sections[r.group], r)
	}
	var rows []boardRow
	for g := GroupNeedsYou; g <= sectionBacklog; g++ {
		list := sections[g]
		if len(list) == 0 {
			continue
		}
		sortRows(list, mode)
		rows = append(rows, boardRow{header: sectionName(g), group: g, count: len(list)})
		rows = append(rows, list...)
	}
	return rows
}

func threadRow(st TickerState, th Thread) boardRow {
	g := classify(th)
	r := boardRow{bead: th.Bead, agent: th.Agent, group: g, line: stateLine(th), color: groupColor(g), run: runCommand(th),
		prs: linkedPRs(st, th.Bead), linear: th.Linear, moves: linearMoves([]Thread{th}), runs: th.Runs}
	agent, pr := "", ""
	if th.Agent != nil {
		agent = th.Agent.Name + " " + th.Agent.Status
	}
	if th.PR != nil {
		pr = fmt.Sprintf("#%d %s", th.PR.Number, th.PR.URL)
	}
	r.search = searchText(sectionName(g), th.Bead.ID, th.Bead.Title, th.Bead.Status, agent, r.line, pr, th.Linear, labelText(th.Bead))
	return r
}

// openRow is an open bead: what kind it is, what it waits on, and what PR or
// Linear issue its notes already link.
func openRow(cfg Config, st TickerState, b Bead, live map[string]bool) boardRow {
	g, color := sectionBacklog, ansiDim
	if b.HasLabel(LabelNext) {
		g, color = sectionNext, ansiCyan
	}
	parts := []string{orDash(b.Type)}
	if w := waitsOn(b, live); len(w) > 0 {
		parts = append(parts, "waits on "+strings.Join(w, ", "))
	}
	prs := linkedPRs(st, b)
	if len(prs) > 0 {
		parts = append(parts, fmt.Sprintf("PR #%d", prs[len(prs)-1].Number))
	}
	key := linearKey(b, cfg.LinearPrefixes)
	if key != "" {
		parts = append(parts, key)
	}
	if ok, _ := autonomous(cfg.Autonomy, b, autonomyStopped(), ""); ok {
		parts = append(parts, "auto")
	}
	line := strings.Join(parts, " · ")
	var urls []string
	for _, pr := range prs {
		urls = append(urls, pr.URL)
	}
	return boardRow{bead: b, group: g, line: line, color: color, prs: prs, linear: key,
		search: searchText(sectionName(g), b.ID, b.Title, b.Status, line, strings.Join(urls, " "), labelText(b))}
}

// waitsOn is the beads b's "blocks" dependencies name that aren't closed yet:
// what keeps it out of bd ready. A dependency outside live is closed.
func waitsOn(b Bead, live map[string]bool) []string {
	var out []string
	for _, d := range b.Dependencies {
		if d.Type == "blocks" && live[d.DependsOn] {
			out = append(out, d.DependsOn)
		}
	}
	return out
}

func labelText(b Bead) string { return strings.Join(b.Labels, " ") }

func sortRows(rows []boardRow, mode sortMode) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].bead, rows[j].bead
		switch mode {
		case sortPriority:
			if a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
			if !a.UpdatedAt.Equal(b.UpdatedAt) {
				return a.UpdatedAt.After(b.UpdatedAt)
			}
		case sortRecent:
			if !a.UpdatedAt.Equal(b.UpdatedAt) {
				return a.UpdatedAt.After(b.UpdatedAt)
			}
		case sortOldest:
			if !a.UpdatedAt.Equal(b.UpdatedAt) {
				return a.UpdatedAt.Before(b.UpdatedAt)
			}
		}
		return a.ID < b.ID
	})
}

// age is a column-width age: 45s, 12m, 5h, 3d, 6w; "" for a zero time.
func age(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dw", int(d.Hours()/24/7))
	}
}

// boardCounts is the title bar's tally: open beads and how many need the user.
func boardCounts(rows []boardRow) (total, needsYou int) {
	for _, r := range rows {
		if r.header != "" {
			continue
		}
		total++
		if r.group == GroupNeedsYou {
			needsYou++
		}
	}
	return total, needsYou
}
