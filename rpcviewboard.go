package main

import (
	"os"
	"strings"
	"sync"
	"time"
)

type nativeViewMsg struct{ workers map[string]rpcVisibility }

func rpcVisibilityAll() map[string]rpcVisibility {
	entries, _ := os.ReadDir(rpcRoot())
	out := map[string]rpcVisibility{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	jobs := make(chan string)
	// Bound concurrency, not one goroutine/socket per reservation.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				v, err := rpcViewStatus(id)
				if err == nil {
					mu.Lock()
					out[id] = v
					mu.Unlock()
				}
			}
		}()
	}
	for _, e := range entries {
		if e.IsDir() && rpcExists(e.Name()) {
			jobs <- e.Name()
		}
	}
	close(jobs)
	wg.Wait()
	return out
}

func (m *board) openNative(r boardRow) bool {
	native := r.native != nil || r.agent != nil && r.agent.Backend == rpcBackend
	if m.env.isNative != nil {
		native = native || m.env.isNative(r.bead.ID)
	}
	if !native {
		return false
	}
	if m.env.viewNative == nil {
		m.status = "native viewer unavailable"
		return true
	}
	var links []string
	for _, p := range r.prs {
		links = append(links, p.URL)
	}
	for _, run := range r.runs {
		links = append(links, run.URL)
	}
	if r.linear != "" {
		links = append(links, linearURL(r.linear, r.bead.Notes, m.cfg.LinearWorkspace))
	}
	// This in-process screen borrows the existing plugin pane, not a worker
	// pane. Returning merely repaints the board; it never focuses/starts pi.
	m.handoff = func() error { return m.env.viewNative(r.bead.ID, r.bead.Title, links) }
	return true
}

// Keep both elapsed and freshness visible in the board's narrow state column;
// the unabridged action and timestamp are in detail/the conversation header.
func rpcBoardBrief(v rpcVisibility, now time.Time) string {
	if !v.Observed.IsZero() && now.Sub(v.Observed) > 3*time.Second {
		return "stale snapshot · " + v.State
	}
	state := v.State
	if state == "idle (not parked)" {
		state = "idle"
	}
	if state == "waiting on approval" {
		state = "approval wait"
	}
	if state == "blocked (intervention)" {
		state = "blocked"
	}
	elapsed := "?"
	if !v.Started.IsZero() {
		until := v.Ended
		if until.IsZero() && v.Connected && v.State == "running" {
			until = now
		}
		if !until.IsZero() {
			elapsed = age(v.Started, until)
		}
	}
	fresh := "?"
	if !v.ActivityAt.IsZero() {
		fresh = age(v.ActivityAt, now)
	}
	action := ""
	if v.Current != "" && v.State == "running" {
		action = " " + strings.TrimSpace(fit(rpcViewOneLine(v.Current), 9))
	}
	return state + action + " · run " + elapsed + " · last " + fresh
}

func rpcBoardDetail(r boardRow, now time.Time) []styledLine {
	if r.native == nil {
		return nil
	}
	v := *r.native
	lines := []styledLine{{rpcVisibilityLine(v, now), ansiCyan}}
	when := "unknown"
	if !v.ActivityAt.IsZero() {
		when = v.ActivityAt.Format(time.RFC3339)
	}
	lines = append(lines, styledLine{"last activity: " + v.Activity + " @ " + when, ansiDim})
	lines = append(lines, styledLine{"transport observed: " + v.Observed.Format(time.RFC3339) + "; V opens read-only history", ansiDim})
	for _, p := range r.prs {
		if c := summarizeChecks(p.Checks); p.State == "OPEN" && c.Pending > 0 {
			lines = append(lines, styledLine{"external checks pending (cached PR evidence; not worker progress)", ansiYellow})
		}
	}
	for i := range lines {
		lines[i].text = rpcViewOneLine(lines[i].text)
	}
	return lines
}
