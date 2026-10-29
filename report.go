package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MergedPR is a merged PR as the report lists it. It stays apart from PR so
// the merged search asks gh for light fields only.
type MergedPR struct {
	Number   int       `json:"number"`
	Title    string    `json:"title"`
	URL      string    `json:"url"`
	Head     string    `json:"headRefName"`
	MergedAt time.Time `json:"mergedAt"`
}

type Shipped struct {
	Bead Bead
	PRs  []string
}

type InFlight struct {
	Bead  Bead
	PR    PR
	State string
}

// AutoSteps are the steps a bead's worker took without TJ: its AUTO: notes.
type AutoSteps struct {
	Bead  Bead
	Notes []string
}

type Report struct {
	Since    time.Time
	Shipped  []Shipped
	Unbeaded []MergedPR
	InFlight []InFlight
	NeedsYou []Bead
	Auto     []AutoSteps
	Warnings []string
}

// parseSince reads --since: Nh, Nd, or a YYYY-MM-DD date at midnight in now's
// location.
func parseSince(s string, now time.Time) (time.Time, error) {
	if s == "" {
		s = "24h"
	}
	if n, ok := strings.CutSuffix(s, "h"); ok {
		if h, err := strconv.Atoi(n); err == nil && h > 0 {
			return now.Add(-time.Duration(h) * time.Hour), nil
		}
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		if d, err := strconv.Atoi(n); err == nil && d > 0 {
			return now.AddDate(0, 0, -d), nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", s, now.Location()); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--since %q: want Nh, Nd or YYYY-MM-DD", s)
}

// MergedSince lists the user's PRs merged at or after since.
func (g GH) MergedSince(since time.Time) ([]MergedPR, error) {
	out, err := g.run("pr", "list", "--author", "@me", "--state", "merged",
		"--search", "merged:>="+since.UTC().Format(time.RFC3339),
		"--json", "number,title,url,mergedAt,headRefName", "--limit", "100")
	if err != nil {
		return nil, err
	}
	var prs []MergedPR
	return prs, json.Unmarshal(out, &prs)
}

// gatherReport reads what the report needs: closed and active beads, the
// ticker's last PR pass, and merged PRs in every followed repository.
func gatherReport(cfg Config, since time.Time) (Report, error) {
	closed, err := Beads{}.list("--status", StatusClosed, "--closed-after", since.UTC().Format(time.RFC3339))
	if err != nil {
		return Report{}, err
	}
	active, err := Beads{}.Active()
	if err != nil {
		return Report{}, err
	}
	var merged []MergedPR
	var warnings []string
	for _, repo := range cfg.allRepos() {
		prs, err := GH{repo: repo}.MergedSince(since)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("merged PRs in %s: %v", repo, err))
			continue
		}
		merged = append(merged, prs...)
	}
	r := buildReport(since, closed, active, merged, loadState(), cfg.BranchPrefix)
	r.Warnings = append(r.Warnings, warnings...)
	return r, nil
}

// buildReport sorts beads and PRs into the report's sections. A merged PR
// belongs to a bead when the bead's notes link it, the ticker last saw it as
// the bead's PR, or it is on the bead's branch.
func buildReport(since time.Time, closed, active []Bead, merged []MergedPR, st TickerState, branchPrefix string) Report {
	r := Report{Since: since}
	links := func(b Bead) []string {
		urls := NotedPRs(b.Notes)
		if pr, ok := st.PRs[b.ID]; ok && pr.URL != "" {
			urls = append(urls, pr.URL)
		}
		return urls
	}
	var shipped []Bead
	for _, b := range closed {
		if !b.ClosedAt.Before(since) {
			shipped = append(shipped, b)
		}
	}
	sort.SliceStable(shipped, func(i, j int) bool { return shipped[i].ClosedAt.Before(shipped[j].ClosedAt) })
	beads := append(append([]Bead{}, shipped...), active...)
	linked := map[string]bool{}
	for _, b := range beads {
		for _, u := range links(b) {
			linked[u] = true
		}
	}

	onBranch := map[string][]string{}
	seen := map[string]bool{}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].MergedAt.Before(merged[j].MergedAt) })
	for _, pr := range merged {
		if seen[pr.URL] || pr.MergedAt.Before(since) || linked[pr.URL] {
			continue
		}
		seen[pr.URL] = true
		if b, ok := beadForBranch(branchPrefix, pr.Head, beads); ok {
			onBranch[b.ID] = append(onBranch[b.ID], pr.URL)
		} else {
			r.Unbeaded = append(r.Unbeaded, pr)
		}
	}
	for _, b := range shipped {
		r.Shipped = append(r.Shipped, Shipped{Bead: b, PRs: dedupe(append(links(b), onBranch[b.ID]...))})
	}

	for _, b := range active {
		if b.Status == StatusNeedsMe {
			r.NeedsYou = append(r.NeedsYou, b)
		}
		pr, ok := st.PRs[b.ID]
		if !ok || pr.State != "OPEN" {
			continue
		}
		f := InFlight{Bead: b, PR: pr}
		if snap, ok := st.Threads[b.ID]; ok {
			th := Thread{Bead: b, PR: &pr, Checks: summarizeChecks(pr.Checks), Reviews: reviewsNotBy(pr.Reviews, st.Login)}
			if snap.AgentStatus != "" {
				th.Agent = &Agent{Status: snap.AgentStatus}
			}
			f.State = stateLine(th)
		}
		r.InFlight = append(r.InFlight, f)
	}
	// Notes carry no time, so a bead touched in the window shows all of its
	// AUTO: notes.
	for _, b := range beads {
		if notes := autoNotes(b.Notes); len(notes) > 0 && !b.UpdatedAt.Before(since) {
			r.Auto = append(r.Auto, AutoSteps{Bead: b, Notes: notes})
		}
	}
	sort.SliceStable(r.InFlight, func(i, j int) bool { return r.InFlight[i].Bead.ID < r.InFlight[j].Bead.ID })
	sort.SliceStable(r.NeedsYou, func(i, j int) bool { return r.NeedsYou[i].ID < r.NeedsYou[j].ID })
	return r
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func renderReport(r Report, now time.Time) string {
	var s strings.Builder
	fmt.Fprintf(&s, "# Report: %s to %s\n", r.Since.Format("2006-01-02 15:04"), now.Format("2006-01-02 15:04 MST"))
	for _, w := range r.Warnings {
		fmt.Fprintf(&s, "\n> WARNING: %s\n", w)
	}

	fmt.Fprintf(&s, "\n## Shipped (%d)\n\n", len(r.Shipped))
	if len(r.Shipped) == 0 {
		s.WriteString("_None._\n")
	}
	for _, it := range r.Shipped {
		fmt.Fprintf(&s, "- **%s** (`%s`)", oneLine(it.Bead.Title), it.Bead.ID)
		if reason := oneLine(it.Bead.CloseReason); reason != "" {
			fmt.Fprintf(&s, ": %s", reason)
		}
		s.WriteString("\n")
		for _, u := range it.PRs {
			fmt.Fprintf(&s, "  - %s\n", u)
		}
	}

	fmt.Fprintf(&s, "\n## Merged without a bead (%d)\n\n", len(r.Unbeaded))
	if len(r.Unbeaded) == 0 {
		s.WriteString("_None._\n")
	}
	for _, pr := range r.Unbeaded {
		fmt.Fprintf(&s, "- %s: %s\n", oneLine(pr.Title), pr.URL)
	}

	fmt.Fprintf(&s, "\n## In flight (%d)\n\n", len(r.InFlight))
	if len(r.InFlight) == 0 {
		s.WriteString("_None._\n")
	}
	for _, f := range r.InFlight {
		fmt.Fprintf(&s, "- **%s** (`%s`): %s", oneLine(f.Bead.Title), f.Bead.ID, f.PR.URL)
		if f.State != "" {
			fmt.Fprintf(&s, " · %s", f.State)
		}
		s.WriteString("\n")
	}

	fmt.Fprintf(&s, "\n## Needs you (%d)\n\n", len(r.NeedsYou))
	if len(r.NeedsYou) == 0 {
		s.WriteString("_None._\n")
	}
	for _, b := range r.NeedsYou {
		fmt.Fprintf(&s, "- **%s** (`%s`): %s\n", oneLine(b.Title), b.ID, lastNote(b.Notes, 300))
	}

	// Only autonomous beads have these, so the section shows only when there
	// are some.
	if len(r.Auto) > 0 {
		fmt.Fprintf(&s, "\n## Done without you (%d)\n\n", len(r.Auto))
		for _, a := range r.Auto {
			fmt.Fprintf(&s, "- **%s** (`%s`)\n", oneLine(a.Bead.Title), a.Bead.ID)
			for _, n := range a.Notes {
				fmt.Fprintf(&s, "  - %s\n", oneLine(n))
			}
		}
	}
	return s.String()
}
