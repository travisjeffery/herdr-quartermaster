package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rivo/uniseg"
	"golang.org/x/term"
)

const rpcViewKeep = 128

// Deliberately cannot express worker/task controls. This is also the boundary
// used by the embedded board view and isolated UI tests.
type rpcViewEnv struct {
	page   func(id, source, cursor string) (rpcViewPage, error)
	status func(id string) (rpcVisibility, error)
}
type rpcViewTrack struct {
	cursor          string
	events          []rpcViewEvent
	gap             string
	more            bool
	dropped         bool
	selected        int
	expanded        bool
	scroll          int
	usage           rpcViewUsage
	usageRecords    int
	incomplete      bool
	provider, model string
}
type rpcViewer struct {
	id, title       string
	links           []string
	env             rpcViewEnv
	source          string
	history, events rpcViewTrack
	visibility      rpcVisibility
	follow          bool
	loading         bool
	generation      int
	at              time.Time
	err             string
	quit            bool
}
type rpcViewResult struct {
	generation      int
	history, events rpcViewPage
	visibility      rpcVisibility
	err             error
}

func newRPCViewer(id, title string, links []string, env rpcViewEnv) *rpcViewer {
	return &rpcViewer{id: id, title: title, links: links, env: env, source: "history", follow: true}
}
func (v *rpcViewer) track() *rpcViewTrack {
	if v.source == "events" {
		return &v.events
	}
	return &v.history
}
func (v *rpcViewer) load() cmd {
	if v.loading {
		return nil
	}
	v.loading = true
	id, hc, ec, g := v.id, v.history.cursor, v.events.cursor, v.generation
	return func() msg {
		st, err := v.env.status(id)
		h, he := v.env.page(id, "history", hc)
		e, ee := v.env.page(id, "events", ec)
		// Missing event history on a prepared adoption does not hide its session.
		if err == nil {
			err = he
		}
		if err == nil && ee != nil {
			e.Gap = ee.Error()
		}
		return rpcViewResult{g, h, e, st, err}
	}
}
func (t *rpcViewTrack) append(p rpcViewPage, follow bool) {
	// Identity resets explicitly invalidate retained records and accounting,
	// including a replacement whose first read then fails.
	if p.Reset {
		*t = rpcViewTrack{}
	}
	if p.Cursor == "" {
		t.gap = p.Gap
		return
	}
	t.cursor, t.gap, t.more = p.Cursor, p.Gap, p.More
	for _, e := range p.Events {
		if e.Kind == "gap" {
			t.incomplete = true
		}
		if e.Model != "" {
			t.provider, t.model = e.Provider, e.Model
		}
		if e.Usage != nil {
			t.usage.add(*e.Usage)
			t.usageRecords++
		}
		t.events = append(t.events, e)
	}
	size := 0
	for _, e := range t.events {
		size += len(e.Text) + len(e.Label)
	}
	for len(t.events) > rpcViewKeep || size > 512<<10 {
		size -= len(t.events[0].Text) + len(t.events[0].Label)
		t.events = t.events[1:]
		t.selected = max(0, t.selected-1)
		t.dropped = true
	}
	if follow && len(p.Events) > 0 {
		t.selected = max(0, len(t.events)-1)
		t.scroll = 0
	}
	t.selected = min(t.selected, max(0, len(t.events)-1))
}
func (v *rpcViewer) apply(r rpcViewResult, now time.Time) {
	if r.generation != v.generation {
		return
	}
	v.loading = false
	v.at = now
	v.err = ""
	v.visibility = r.visibility
	if r.err != nil {
		v.err = rpcViewRedact(r.err.Error())
	}
	v.history.append(r.history, v.follow)
	v.events.append(r.events, v.follow)
	// Old supervisors did not persist model metadata. Saved assistant/model
	// entries still report the recorded model without querying pi.
	if v.visibility.Model == "" {
		v.visibility.Model, v.visibility.Provider = v.history.model, v.history.provider
	}
}
func (v *rpcViewer) tick(now time.Time) cmd {
	if !v.follow || v.loading {
		return nil
	}
	every := time.Second
	if v.history.more || v.events.more {
		every = 100 * time.Millisecond
	}
	if now.Sub(v.at) >= every {
		return v.load()
	}
	return nil
}
func (v *rpcViewer) key(k string) cmd {
	t := v.track()
	switch k {
	case "q", "\x1b", "\x03":
		v.quit = true
	case "h":
		v.source = "history"
	case "e":
		v.source = "events"
	case "\t":
		if v.source == "history" {
			v.source = "events"
		} else {
			v.source = "history"
		}
	case "j", "\x1b[B":
		v.follow = false
		t.selected = min(max(0, len(t.events)-1), t.selected+1)
		t.scroll = 0
	case "k", "\x1b[A":
		v.follow = false
		t.selected = max(0, t.selected-1)
		t.scroll = 0
	case "\r", " ":
		t.expanded = !t.expanded
		t.scroll = 0
	case "J", "\x1b[6~":
		v.follow = false
		t.scroll += 5
	case "K", "\x1b[5~":
		v.follow = false
		t.scroll = max(0, t.scroll-5)
	case "g":
		// No concurrent readers after reset. Ignore stale results by generation.
		if v.loading {
			return nil
		}
		v.generation++
		v.history = rpcViewTrack{}
		v.events = rpcViewTrack{}
		v.follow = false
		return v.load()
	case "n", "r":
		return v.load()
	case "f":
		v.follow = true
		return v.load()
	}
	return nil
}

// Render untrusted values as plain text, not Markdown/ANSI/OSC. Tabs/newlines
// cannot inject rows into headers. Narrow views clip independently of output.
func rpcViewOneLine(s string) string { return strings.Join(strings.Fields(rpcViewRedact(s)), " ") }
func rpcViewLines(s string, w int) []string {
	var lines []string
	for _, line := range strings.Split(rpcViewSafe(s), "\n") {
		line = strings.ReplaceAll(line, "\t", "    ")
		var b strings.Builder
		cells := 0
		g := uniseg.NewGraphemes(line)
		for g.Next() {
			if cells+g.Width() > w {
				lines = append(lines, b.String())
				b.Reset()
				cells = 0
			}
			b.WriteString(g.Str())
			cells += g.Width()
		}
		lines = append(lines, b.String())
	}
	return lines
}
func rpcViewFit(s string, w int) string {
	var b strings.Builder
	cells := 0
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		if cells+g.Width() > w {
			break
		}
		b.WriteString(g.Str())
		cells += g.Width()
	}
	return b.String() + strings.Repeat(" ", max(0, w-cells))
}
func (v *rpcViewer) render(w, h int, now time.Time) string {
	w, h = max(1, w), max(1, h)
	if h < 8 {
		lines := make([]string, h)
		lines[0] = rpcViewFit("READ ONLY: enlarge terminal; q returns", w)
		for i := 1; i < h; i++ {
			lines[i] = strings.Repeat(" ", w)
		}
		return "\x1b[H" + strings.Join(lines, "\x1b[K\r\n") + "\x1b[K"
	}
	var lines []string
	add := func(s string) { lines = append(lines, rpcViewFit(rpcViewOneLine(s), w)) }
	add("READ ONLY · " + v.id + " · " + v.title)
	add(rpcVisibilityLine(v.visibility, now))
	when := "unknown"
	if !v.visibility.ActivityAt.IsZero() {
		when = v.visibility.ActivityAt.Format(time.RFC3339)
	}
	add("last: " + v.visibility.Activity + " @ " + when + " · observed " + age(v.visibility.Observed, now) + " ago")
	model := v.visibility.Provider + "/" + v.visibility.Model
	if model == "/" {
		model = "unknown"
	}
	u := v.history.usage
	scope := "recorded session usage"
	if v.history.more || v.history.gap != "" || v.history.cursor == "" || v.history.incomplete || v.err != "" {
		scope = "usage so far (incomplete)"
	}
	if v.history.usageRecords == 0 {
		add("model " + model + " · usage unknown (no records read)")
	} else {
		add(fmt.Sprintf("%s · %s: in %.0f out %.0f cache %.0f/%.0f total %.0f cost $%.4f", model, scope, u.Input, u.Output, u.CacheRead, u.CacheWrite, u.Total, u.Cost.Total))
	}
	t := v.track()
	mode := "paused"
	if v.follow {
		mode = "following"
	}
	label := "saved session log (all branches, append order)"
	if v.source == "events" {
		label = "live execution log (completed text, no token deltas)"
	}
	add(label + " · " + mode)
	if t.gap != "" {
		add("GAP: " + t.gap)
	}
	if t.dropped {
		add("Earlier rows evicted from view; g replays saved history, n next page")
	}
	if len(v.links) > 0 {
		add("Task links: " + strings.Join(v.links, " "))
	}
	room := max(0, h-len(lines)-2)
	if t.expanded && len(t.events) > 0 {
		e := t.events[t.selected]
		text := e.Label + "\n" + e.Text
		if len(e.Links) > 0 {
			text += "\nLinks: " + strings.Join(e.Links, " ")
		}
		if e.EntryID != "" {
			text += "\nentry " + e.EntryID + " parent " + e.ParentID
		}
		if e.ToolID != "" {
			text += "\ntool call " + e.ToolID
		}
		detail := rpcViewLines(text, w)
		t.scroll = min(t.scroll, max(0, len(detail)-1))
		for _, s := range detail[t.scroll:] {
			if room <= 0 {
				break
			}
			lines = append(lines, rpcViewFit(s, w))
			room--
		}
	} else {
		start := max(0, t.selected-room+1)
		for i := start; i < len(t.events) && room > 0; i++ {
			e := t.events[i]
			mark := "  "
			if i == t.selected {
				mark = "> "
			}
			at := "time unknown"
			if !e.At.IsZero() {
				at = e.At.Format("15:04:05")
			}
			failed := ""
			if e.Error {
				failed = " ERROR"
			}
			add(fmt.Sprintf("%s%s #%d %s%s %s", mark, at, e.Seq, e.Label, failed, e.Text))
			room--
		}
	}
	for len(lines) < h-2 {
		lines = append(lines, strings.Repeat(" ", w))
	}
	if len(lines) > h-2 {
		lines = lines[:h-2]
	}
	status := v.err
	if status == "" {
		status = "No worker controls. Saved usage only; no model calls. f follows, g replays, n next page."
	}
	add(status)
	add("h history e events j/k select Enter expand J/K scroll f follow g replay n next q back")
	return "\x1b[H" + strings.Join(lines, "\x1b[K\r\n") + "\x1b[K"
}

func runRPCView(id, title string, links []string) error {
	if !rpcExists(id) {
		return fmt.Errorf("no native history for %s; viewing never starts a worker", id)
	}
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, old)
	fmt.Print("\x1b[?1049h\x1b[?25l")
	defer fmt.Print("\x1b[?25h\x1b[?1049l")
	v := newRPCViewer(id, title, links, rpcViewEnv{rpcViewRead, rpcViewStatus})
	// At most one read in flight and one result; closing cannot retain a growing
	// output queue, and never cancels anything in the worker.
	results := make(chan rpcViewResult, 1)
	start := func(c cmd) {
		if c != nil {
			go func() { results <- c().(rpcViewResult) }()
		}
	}
	start(v.load())
	buf := make([]byte, 256)
	var partial []byte
	for !v.quit {
		select {
		case r := <-results:
			v.apply(r, time.Now())
		default:
		}
		start(v.tick(time.Now()))
		w, h, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil {
			w, h = 100, 30
		}
		fmt.Print(v.render(w, h, time.Now()))
		ready, err := waitInput(fd, 100)
		if err != nil {
			return err
		}
		if !ready {
			continue
		}
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		var keys []string
		keys, partial = splitKeys(append(partial, buf[:n]...))
		for _, k := range keys {
			start(v.key(k))
			if v.quit {
				break
			}
		}
	}
	return nil
}
