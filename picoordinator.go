package main

import (
	"fmt"
	"path/filepath"
	"strconv"
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
		if e.At.After(latest) {
			latest = e.At
		}
	}
	if !latest.After(st.LastNudge) {
		return
	}
	c, ok := piControlFor("coordinator-inbox")
	if !ok {
		return
	} // events remain in the durable inbox, even across restart
	res, err := c.call(piRequest{Op: "inbox", Session: coord.Session.Value, Through: fmt.Sprintf("%019d", latest.UnixNano())})
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
	if !ack.After(st.LastNudge) || ack.After(latest) {
		return
	}
	st.LastNudge = ack
	st.CoordReady = time.Time{}
}
