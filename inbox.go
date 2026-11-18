package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// InboxCondition identifies the observation that keeps an item actionable.
// An empty key on an older event means its resource identity is unknown, not
// that an arbitrary current prompt/head is the same one. Archives keep the
// original bytes and filenames: coordinator acknowledgements rely on them.
type InboxCondition struct {
	Kind string `json:"kind"`
	Bead string `json:"bead,omitempty"`
	Key  string `json:"key,omitempty"`
}

func defaultInboxCondition(e Event) *InboxCondition {
	kind := "active"
	switch e.Kind {
	case "closed", EventAutoClosed:
		kind = "manual"
	case EventLikelyStale, EventLeaseExpired, EventBreaker:
		// Verify, lease expiry and breakers can raise notices on backlog beads,
		// which were never in the active-thread set.
		kind = "bead_closed"
	case EventNeedsYou:
		kind = "needs_you" // legacy: either a bead question or an agent prompt
	case EventAgentGone:
		kind = "agent_gone"
	case EventFailing:
		kind = "checks_failing"
	case EventReady:
		kind = "ready_to_merge"
	}
	if e.Bead == "" {
		kind = "manual" // e.g. the global lease-expired notice
	}
	return &InboxCondition{Kind: kind, Bead: e.Bead}
}

func eventCondition(t Thread, kind EventKind) *InboxCondition {
	c := defaultInboxCondition(Event{Kind: kind, Bead: t.Bead.ID})
	switch kind {
	case EventNeedsYou:
		switch {
		case t.Prompt != nil:
			c.Kind, c.Key = "prompt", t.Prompt.Hash
		case t.Agent != nil && t.Agent.Status == "blocked":
			c.Kind = "agent_blocked"
		default:
			c.Kind, c.Key = "needs_me", asked(t.Bead)
		}
	case EventFailing, EventReady:
		c.Key = inboxPRKey(t)
	}
	return c
}

func inboxPRKey(t Thread) string {
	if t.PR == nil {
		return ""
	}
	return fmt.Sprintf("%s#%d@%s", t.PR.URL, t.PR.Number, t.PR.HeadSHA)
}

// inboxObsolete only uses a successful, complete active-thread observation.
// Unknown prompt/PR detail is not evidence of resolution. This deliberately
// does not classify a view: it tests the specific cause recorded on the item.
func inboxObsolete(it InboxItem, active map[string]Thread) bool {
	c := it.Clears
	if c == nil {
		c = defaultInboxCondition(it.Event)
	}
	if c.Kind == "manual" || it.Bead == "" {
		return false
	}
	if c.Bead != it.Bead {
		return false // malformed metadata is not evidence of resolution
	}
	t, ok := active[it.Bead]
	if c.Kind == "bead_closed" {
		return ok && t.Bead.Status == StatusClosed
	}
	if !ok || (t.Bead.Status != StatusInProgress && t.Bead.Status != StatusNeedsMe && t.Bead.Status != StatusBlocked) {
		return true
	}
	blocked := t.Agent != nil && t.Agent.Status == "blocked"
	switch c.Kind {
	case "needs_you":
		return t.Bead.Status != StatusNeedsMe && !t.AgentUnknown && !blocked
	case "needs_me":
		return t.Bead.Status != StatusNeedsMe || (c.Key != "" && c.Key != asked(t.Bead))
	case "agent_blocked":
		return !t.AgentUnknown && !blocked
	case "prompt":
		return !t.AgentUnknown && (!blocked || (t.Prompt != nil && c.Key != "" && c.Key != t.Prompt.Hash))
	case "agent_gone":
		return t.Agent != nil
	case "checks_failing", "ready_to_merge":
		if t.PR == nil {
			return false
		}
		if t.PR.State != "OPEN" || (c.Key != "" && c.Key != inboxPRKey(t)) {
			return true
		}
		if c.Kind == "checks_failing" {
			return len(t.Checks.Failing) == 0
		}
		// Readiness can flicker while GitHub recomputes checks/gates. The
		// ticker announces it once per head, so retain that observation
		// until its resource changes or a human decision supersedes it.
		return t.Bead.Status == StatusNeedsMe
	}
	return false
}

func reconcileInbox(threads []Thread, show func(string) (Bead, error)) error {
	items, err := readInbox()
	if err != nil {
		return err
	}
	active := make(map[string]Thread, len(threads))
	for _, th := range threads {
		active[th.Bead.ID] = th
	}
	// Only notices raised outside the thread lifecycle need an extra Beads
	// read. Cache failures too: unavailable truth must not archive evidence.
	looked := map[string]bool{}
	var files []string
	for _, it := range items {
		c := it.Clears
		if c == nil {
			c = defaultInboxCondition(it.Event)
		}
		if _, known := active[it.Bead]; !known && c.Kind == "bead_closed" && show != nil && !looked[it.Bead] {
			looked[it.Bead] = true
			if b, err := show(it.Bead); err == nil && b.ID == it.Bead {
				active[it.Bead] = Thread{Bead: b}
			}
		}
		if inboxObsolete(it, active) {
			files = append(files, it.File)
		}
	}
	_, err = inboxDoneFiles(files)
	return err
}

// inboxDoneFiles archives exactly the displayed files. Empty means none, and
// a new item for the same bead arriving after rendering is never selected.
func inboxDoneFiles(files []string) (int, error) {
	for _, file := range files {
		if file == "" || file == "." || filepath.Base(file) != file {
			return 0, fmt.Errorf("invalid inbox filename %q", file)
		}
	}
	items, err := readInbox()
	if err != nil {
		return 0, err
	}
	want := make(map[string]bool, len(files))
	for _, file := range files {
		want[file] = true
	}
	n := 0
	var errs []error
	for _, it := range items {
		if !want[it.File] || headlessOwned(it.Bead) {
			continue // verifier-owned events require verified outcomes
		}
		done := filepath.Join(inboxDir(), "done")
		if err := os.MkdirAll(done, 0700); err != nil {
			return n, err
		}
		if err := os.Rename(filepath.Join(inboxDir(), it.File), filepath.Join(done, it.File)); err == nil {
			n++
		} else if !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return n, errors.Join(errs...)
}
