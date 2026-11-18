package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type headlessHealth struct {
	Mode            string    `json:"mode"`
	LastTick        time.Time `json:"last_tick"`
	LastLoop        time.Time `json:"last_consumer_loop"`
	LastDelivery    time.Time `json:"last_event_delivery"`
	LastProcessed   time.Time `json:"last_successful_processing"`
	Oldest          time.Time `json:"oldest_actionable_unhandled"`
	FollowSince     time.Time `json:"oldest_worker_follow_through"`
	Backlog         int       `json:"backlog"`
	Blocked         int       `json:"blocked"`
	UnreadReports   int       `json:"unacknowledged_reports"`
	ConsumerStalled bool      `json:"consumer_stalled"`
	FollowOverdue   bool      `json:"follow_through_overdue"`
}

func headlessHealthOf(c headlessControl, j headlessJournal, tick, now time.Time, hc HeadlessConfig) headlessHealth {
	h := headlessHealth{Mode: c.Mode, LastTick: tick, LastLoop: j.LastLoop, LastDelivery: j.LastDelivery, LastProcessed: j.LastProcessed}
	for _, e := range j.Events {
		if e.State == "verified-handled" {
			continue
		}
		if e.State == "explicitly-blocked" {
			h.Blocked++
			continue
		}
		h.Backlog++
		// An unrelated event's progress must not hide this event's stall.
		if c.Mode == "background" && e.Requested.IsZero() && now.Sub(e.At) > time.Duration(hc.StallSeconds)*time.Second {
			h.ConsumerStalled = true
		}
		if h.Oldest.IsZero() || e.At.Before(h.Oldest) {
			h.Oldest = e.At
		}
		if !e.Requested.IsZero() && (h.FollowSince.IsZero() || e.Requested.Before(h.FollowSince)) {
			h.FollowSince = e.Requested
		}
	}
	if c.Mode == "background" && h.Backlog > 0 {
		h.ConsumerStalled = h.ConsumerStalled || j.LastLoop.IsZero() || now.Sub(j.LastLoop) > time.Duration(hc.StallSeconds)*time.Second
		h.FollowOverdue = !h.FollowSince.IsZero() && now.Sub(h.FollowSince) > time.Duration(hc.FollowSeconds)*time.Second
	}
	return h
}
func headlessHealthRead(cfg Config) (headlessHealth, error) {
	c, err := headlessReadControl()
	if err != nil {
		return headlessHealth{}, err
	}
	j, err := headlessLoadJournal()
	if err != nil {
		return headlessHealth{}, err
	}
	// The watchdog also sees events not yet ingested by a dead consumer.
	items, err := readInbox()
	if err != nil {
		return headlessHealth{}, err
	}
	j.receive(c, items)
	h := headlessHealthOf(c, j, loadState().LastTick, time.Now(), cfg.Headless.bounded())
	entries, err := os.ReadDir(headlessPath("reports"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return h, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(headlessPath("acks"), entry.Name())); errors.Is(err, os.ErrNotExist) {
			h.UnreadReports++
		}
	}
	return h, nil
}
func headlessStatus(cfg Config, out io.Writer) error {
	h, err := headlessHealthRead(cfg)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(h)
}
func headlessReports(out io.Writer) error {
	entries, err := os.ReadDir(headlessPath("reports"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		var r headlessReport
		if err := rpcLoad(filepath.Join(headlessPath("reports"), entry.Name()), &r); err != nil {
			return err
		}
		_, err := os.Stat(filepath.Join(headlessPath("acks"), entry.Name()))
		if err := json.NewEncoder(out).Encode(struct {
			headlessReport
			Acknowledged bool `json:"acknowledged"`
		}{r, err == nil}); err != nil {
			return err
		}
	}
	return nil
}

// Run from a separate timer, not from the ticker or consumer. It never restarts
// either process and cannot approve anything. Desktop text contains no task,
// repo, model output, resource names or transcript text.
func headlessWatch(cfg Config, out io.Writer) error {
	gate, err := headlessLock("watch.lease", true)
	if err != nil {
		return err
	}
	defer gate.Close()
	h, err := headlessHealthRead(cfg)
	if err != nil {
		return err
	}
	var prev struct {
		Active bool   `json:"active"`
		ID     string `json:"id"`
	}
	if err := rpcLoad(headlessPath("watch.json"), &prev); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	active := h.ConsumerStalled || h.FollowOverdue
	if active && !prev.Active {
		// Persist a fresh episode BEFORE publication. The same stalled event
		// can recur after pause/recovery; its old readback must not hide it.
		prev.ID = headlessHash([]string{"stall", rpcID()})
		prev.Active = true
		if err := rpcSave(headlessPath("watch.json"), prev); err != nil {
			return err
		}
	}
	if active {
		// Also repairs a crash between the episode intent and publication.
		if err := headlessReportOnce(headlessReport{ID: prev.ID, Milestone: "blocked", Reason: "consumer-or-follow-through-stalled", At: time.Now()}); err != nil {
			return err
		}
	}
	if !active && prev.Active {
		milestone, reason := "recovered", "health-recovered"
		if h.Blocked > 0 {
			milestone, reason = "blocked", "stall-now-explicitly-blocked"
		}
		if err := headlessReportOnce(headlessReport{ID: headlessHash([]string{prev.ID, "recovery"}), Milestone: milestone, Reason: reason, At: time.Now()}); err != nil {
			return err
		}
	}
	prev.Active = active
	if err := rpcSave(headlessPath("watch.json"), prev); err != nil {
		return err
	}
	// Each durable report has one attempted notification. A crash after intent
	// leaves delivery unknown, not retried/spammed; explicit readback still works.
	if err := rpcPrivateDir(headlessPath("notifications")); err != nil {
		return err
	}
	entries, err := os.ReadDir(headlessPath("reports"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(headlessPath("notifications"), entry.Name())
		if _, err := os.Stat(path); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := rpcSave(path, map[string]any{"state": "attempted", "at": time.Now()}); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := exec.CommandContext(ctx, "notify-send", "Quartermaster", "A coordination milestone needs readback. Run quartermaster headless reports.").Run()
		cancel()
		state := "submitted"
		if err != nil {
			state = "unavailable"
		}
		if err := rpcSave(path, map[string]any{"state": state, "at": time.Now()}); err != nil {
			return err
		}
		fmt.Fprintln(out, "coordination milestone available; use headless reports and ack (notification is not readback)")
		break // one desktop notification per watchdog invocation
	}
	return json.NewEncoder(out).Encode(h)
}
