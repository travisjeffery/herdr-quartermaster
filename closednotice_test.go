package main

import (
	"io"
	"log"
	"strings"
	"testing"
)

// closedNotice runs leftActive for a closed b-1 whose PR merged and returns
// the summary of the closed item it queued.
func closedNotice(t *testing.T, worktrees string) string {
	t.Helper()
	fakeTools(t)
	t.Setenv("FAKE_BEAD", `[{"id":"b-1","title":"Do it","status":"closed"}]`)
	t.Setenv("FAKE_WORKTREES", worktrees)
	tk := Ticker{cfg: hostsConfig(), herdr: newHerdr(), log: log.New(io.Discard, "", 0)}
	st := TickerState{Threads: map[string]Snapshot{"b-1": {PRNumber: 7, PRState: "MERGED"}}}
	tk.leftActive(&st, "b-1", st.Threads["b-1"], nil, t0)
	items, err := readInbox()
	if err != nil || len(items) != 1 || items[0].Kind != "closed" {
		t.Fatalf("inbox %+v, %v; want one closed item", items, err)
	}
	return items[0].Summary
}

func TestClosedNoticeWhenTheWorktreeIsAlreadyGone(t *testing.T) {
	got := closedNotice(t, `[]`)
	if got != "b-1 closed after PR #7 merged; its worktree is already removed" {
		t.Fatalf("summary %q", got)
	}
}

func TestClosedNoticeStillOffersResolve(t *testing.T) {
	got := closedNotice(t, `[{"branch":"tj/b-1-do-it","path":"/wt/b-1","is_linked_worktree":true,"open_workspace_id":"w7"}]`)
	if strings.Contains(got, "already removed") || strings.Contains(got, "nothing to remove") || !strings.Contains(got, "b-1 closed after PR #7 merged; ") {
		t.Fatalf("summary %q", got)
	}
}
