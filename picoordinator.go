package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Separate from worker prompt/answer sockets. The coordinator endpoint accepts
// only a watermark for the existing durable inbox, never arbitrary user text.
func piCoordinatorSocket() string { return piControlSocket("coordinator-inbox") }

func (t Ticker) nudgePiCoordinator(st *TickerState, coord Agent, now time.Time) {
	if !isLocalPi(&coord) || coord.Session.Kind != "path" || !filepath.IsAbs(coord.Session.Value) {
		// Unknown identity is not permission to type into a terminal.
		return
	}
	events, err := readInbox()
	if err != nil {
		t.log.Printf("coordinator inbox: %v", err)
		return
	}
	latest := st.LastNudge
	for _, e := range events {
		if t.cfg.suppressInteractiveNudgeFor(e) {
			continue
		}
		if e.At.After(latest) && !e.At.After(now) {
			latest = e.At
		}
	}
	c, ok := piControlFor("coordinator-inbox")
	if !ok {
		return
	} // events remain in the durable inbox, even across restart
	// Reconcile even when all events were archived before state.json was saved.
	// A status request does not enqueue a message or start a model turn.
	req := piRequest{Op: "inbox_status", Session: coord.Session.Value}
	if latest.After(st.LastNudge) && !latest.After(now) {
		req.Op, req.Through = "inbox", fmt.Sprintf("%019d", latest.UnixNano())
	}
	res, err := c.call(req)
	if err != nil {
		t.log.Printf("coordinator inbox retained: %v", err)
		return
	}
	if res.Session != coord.Session.Value {
		t.log.Printf("coordinator inbox: session mismatch")
		return
	}
	// Only a transcript-backed delivery ack advances the ticker. Acceptance
	// alone must be retried/re-addressed if the coordinator switches sessions.
	// The receiver shares a bounded ack journal across those replacements.
	if len(res.Ack) != 19 {
		return
	}
	ns, err := strconv.ParseInt(res.Ack, 10, 64)
	if err != nil {
		return
	}
	ack := time.Unix(0, ns)
	if !ack.After(st.LastNudge) {
		return
	}
	// The unread set is not an upper bound: it shrinks on archival. Instead
	// require an exact durable event (unread OR done), a nonfuture watermark,
	// and the receipt in the authenticated response's exact current session.
	// Never trust a bare journal cursor, a maximum timestamp, or acceptance.
	if ns <= 0 || fmt.Sprintf("%019d", ns) != res.Ack || ack.After(now) ||
		!coordinatorEventExists(res.Ack, ack) || !coordinatorReceiptExists(coord.Session.Value, res.Ack) {
		t.log.Printf("coordinator inbox: unverified acknowledgement retained")
		return
	}
	st.LastNudge = ack
	st.CoordReady = time.Time{}
}

func coordinatorEventExists(cursor string, at time.Time) bool {
	for _, dir := range []string{inboxDir(), filepath.Join(inboxDir(), "done")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasPrefix(entry.Name(), cursor+"-") || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			var e Event
			if rpcLoad(filepath.Join(dir, entry.Name()), &e) == nil && e.At.Equal(at) &&
				entry.Name() == fmt.Sprintf("%d-%s-%s.json", e.At.UnixNano(), agentName(e.Bead), e.Kind) {
				return true
			}
		}
	}
	return false
}

func coordinatorReceiptExists(session, cursor string) bool {
	f, err := os.Open(session)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	// Snapshot the append-only file's length. Bound individual records and
	// require a complete line; partial writes must not certify persistence.
	scan := bufio.NewScanner(io.LimitReader(f, fi.Size()))
	scan.Buffer(make([]byte, 4096), 16<<20)
	scan.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i], nil
		}
		return 0, nil, nil
	})
	for scan.Scan() {
		var e struct {
			Type       string `json:"type"`
			CustomType string `json:"customType"`
			Details    struct {
				Session string `json:"session"`
				Through string `json:"through"`
			} `json:"details"`
		}
		if json.Unmarshal(scan.Bytes(), &e) == nil && e.Type == "custom_message" &&
			e.CustomType == "quartermaster.inbox" && e.Details.Session == session && e.Details.Through == cursor {
			return true
		}
	}
	return false
}
