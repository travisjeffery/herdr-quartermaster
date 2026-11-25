package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	notifyFeedVersion      = 1
	notifyFeedMaxEvents    = 500
	notifyFeedDefaultLimit = 100
	notifyFeedMaxLimit     = 500
)

// NotifyFeedEvent is one sequenced, privacy-sanitized notification stored in
// <state>/notify/events.json and returned by GET /events and
// `quartermaster notify events`.
type NotifyFeedEvent struct {
	Seq uint64 `json:"seq"`
	NotifyEnvelope
}

type notifyFeedFile struct {
	Version   int               `json:"version"`
	LatestSeq uint64            `json:"latest_seq"`
	Events    []NotifyFeedEvent `json:"events"`
}

// NotifyEventsQuery specifies cursor, pagination, and event-kind filters for
// querying the durable notification feed.
type NotifyEventsQuery struct {
	Since  uint64
	Limit  int
	Events map[string]bool
}

// NotifyEventsResponse is the versioned JSON payload returned by GET /events
// and `quartermaster notify events --json`.
type NotifyEventsResponse struct {
	Version   int               `json:"version"`
	LatestSeq uint64            `json:"latest_seq"`
	NextSince uint64            `json:"next_since"`
	HasMore   bool              `json:"has_more"`
	Truncated bool              `json:"truncated"`
	Events    []NotifyFeedEvent `json:"events"`
}

func notifyEventsPath() string { return filepath.Join(notifyDir(), "events.json") }

func cloneIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// extractActiveNoteClass scans lines after the last answer boundary in reverse
// for a valid CLASS: N (0-9) line, avoiding stale classes from earlier answered
// questions or commands.
func extractActiveNoteClass(notes string) int {
	lines := linesAfterAnswer(noteLines(notes))
	for i := len(lines) - 1; i >= 0; i-- {
		if _, n, ok := parseClassLine(lines[i]); ok && n >= 0 && n <= 9 {
			return n
		}
	}
	return -1
}

// notifyNeedsMeClass extracts the live autonomy impact class (0-9) for a
// needs_me thread from its live CLASS: note or matching structured question
// card so polling consumers can prioritize urgent items without an extra lookup.
func notifyNeedsMeClass(th Thread) *int {
	notes := th.Bead.Notes
	if pendingCommand(notes) != "" {
		if n := extractActiveNoteClass(notes); n >= 0 {
			return inboxIntPtr(n)
		}
		return nil
	}

	card, hasCard := loadQuestionCard(th.Bead.ID)
	if q, hasLiveOpts := parseNeedsMeQuestion(notes); hasLiveOpts {
		if hasCard && (card.Hash == "" || card.Hash == q.Hash) && card.Class != nil {
			return cloneIntPtr(card.Class)
		}
		if q.ClassN >= 0 && q.ClassN <= 9 {
			return inboxIntPtr(q.ClassN)
		}
		if n := extractActiveNoteClass(notes); n >= 0 {
			return inboxIntPtr(n)
		}
		return nil
	}

	if hasCard && (card.Hash == "" || !questionCardAnswered(notes, card.Hash)) && card.Class != nil {
		return cloneIntPtr(card.Class)
	}
	if fq, ok := parseFreeTextNeedsMeQuestion(notes); ok && fq.ClassN >= 0 && fq.ClassN <= 9 {
		return inboxIntPtr(fq.ClassN)
	}
	if n := extractActiveNoteClass(notes); n >= 0 {
		return inboxIntPtr(n)
	}
	return nil
}

// loadNotifyFeed reads <state>/notify/events.json without creating files or
// taking locks so GET /events and `quartermaster notify events` stay strictly
// read-only. Atomically renamed writes guarantee a complete JSON document.
func loadNotifyFeed() (notifyFeedFile, error) {
	var f notifyFeedFile
	b, err := os.ReadFile(notifyEventsPath())
	if errors.Is(err, os.ErrNotExist) {
		return notifyFeedFile{
			Version: notifyFeedVersion,
			Events:  []NotifyFeedEvent{},
		}, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("invalid notify events file; inspect %s: %w", notifyEventsPath(), err)
	}
	if f.Version != notifyFeedVersion {
		return f, fmt.Errorf("unsupported notify events version %d; inspect %s", f.Version, notifyEventsPath())
	}
	if f.Events == nil {
		f.Events = []NotifyFeedEvent{}
	}
	for _, ev := range f.Events {
		if ev.Seq > f.LatestSeq {
			f.LatestSeq = ev.Seq
		}
	}
	return f, nil
}

func saveNotifyFeed(f notifyFeedFile) error {
	if f.Version == 0 {
		f.Version = notifyFeedVersion
	}
	if f.Events == nil {
		f.Events = []NotifyFeedEvent{}
	}
	if err := rpcPrivateDir(notifyDir()); err != nil {
		return err
	}
	path := notifyEventsPath()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(f); err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp(filepath.Dir(path), ".notify-events-*")
	if err != nil {
		return err
	}
	tmp := tmpFile.Name()
	defer os.Remove(tmp)
	if err := tmpFile.Chmod(0o600); err != nil {
		tmpFile.Close()
		return err
	}
	if _, err := tmpFile.Write(buf.Bytes()); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// appendNotifyFeedEvents appends sanitized envelopes to the bounded ring buffer
// under the shared notify flock, incrementing latest_seq monotonically and
// retaining at most notifyFeedMaxEvents most recent entries.
func appendNotifyFeedEvents(envelopes []NotifyEnvelope) error {
	if len(envelopes) == 0 {
		return nil
	}
	return withNotifyLock(func() error {
		f, err := loadNotifyFeed()
		if err != nil {
			return err
		}
		for _, ev := range envelopes {
			clean := sanitizeNotifyEnvelope(ev)
			f.LatestSeq++
			f.Events = append(f.Events, NotifyFeedEvent{
				Seq:            f.LatestSeq,
				NotifyEnvelope: clean,
			})
		}
		if len(f.Events) > notifyFeedMaxEvents {
			f.Events = append([]NotifyFeedEvent(nil), f.Events[len(f.Events)-notifyFeedMaxEvents:]...)
		}
		return saveNotifyFeed(f)
	})
}

func parseNotifyEventFilter(raw string) (map[string]bool, error) {
	parts := strings.Split(raw, ",")
	out := make(map[string]bool, len(parts))
	for _, p := range parts {
		kind := strings.TrimSpace(p)
		if kind == "" {
			return nil, errors.New("invalid event parameter: event kind must not be empty")
		}
		if !notifyKnownEvents[kind] {
			return nil, fmt.Errorf("invalid event parameter: unknown event kind %q", kind)
		}
		out[kind] = true
	}
	return out, nil
}

func parseNotifyEventsQuery(rawQuery string) (NotifyEventsQuery, error) {
	q := NotifyEventsQuery{
		Limit: notifyFeedDefaultLimit,
	}
	if rawQuery == "" {
		return q, nil
	}
	vals, err := url.ParseQuery(rawQuery)
	if err != nil {
		return NotifyEventsQuery{}, fmt.Errorf("malformed query string: %v", err)
	}
	if sVals, ok := vals["since"]; ok {
		if len(sVals) > 1 {
			return NotifyEventsQuery{}, errors.New("invalid since parameter: must not be repeated")
		}
		raw := strings.TrimSpace(sVals[0])
		if raw == "" {
			return NotifyEventsQuery{}, errors.New("invalid since parameter: must be a non-negative integer")
		}
		v, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return NotifyEventsQuery{}, errors.New("invalid since parameter: must be a non-negative integer")
		}
		q.Since = v
	}
	if lVals, ok := vals["limit"]; ok {
		if len(lVals) > 1 {
			return NotifyEventsQuery{}, errors.New("invalid limit parameter: must not be repeated")
		}
		raw := strings.TrimSpace(lVals[0])
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return NotifyEventsQuery{}, errors.New("invalid limit parameter: must be a positive integer")
		}
		if n > notifyFeedMaxLimit {
			n = notifyFeedMaxLimit
		}
		q.Limit = n
	}
	if eVals, ok := vals["event"]; ok {
		filter := map[string]bool{}
		for _, evParam := range eVals {
			parsed, err := parseNotifyEventFilter(evParam)
			if err != nil {
				return NotifyEventsQuery{}, err
			}
			for k := range parsed {
				filter[k] = true
			}
		}
		q.Events = filter
	}
	return q, nil
}

func queryNotifyFeed(f notifyFeedFile, q NotifyEventsQuery) NotifyEventsResponse {
	limit := q.Limit
	if limit <= 0 {
		limit = notifyFeedDefaultLimit
	}
	if limit > notifyFeedMaxLimit {
		limit = notifyFeedMaxLimit
	}
	latestSeq := f.LatestSeq
	if n := len(f.Events); n > 0 && f.Events[n-1].Seq > latestSeq {
		latestSeq = f.Events[n-1].Seq
	}

	truncated := false
	if q.Since > 0 {
		if q.Since > latestSeq {
			truncated = true
		} else if len(f.Events) > 0 {
			oldestSeq := f.Events[0].Seq
			if oldestSeq > 1 && q.Since < oldestSeq-1 {
				truncated = true
			}
		} else if q.Since < latestSeq {
			truncated = true
		}
	}

	events := make([]NotifyFeedEvent, 0, min(limit, len(f.Events)))
	hasMore := false
	for _, ev := range f.Events {
		if ev.Seq <= q.Since {
			continue
		}
		if len(q.Events) > 0 && !q.Events[ev.Event] {
			continue
		}
		if len(events) < limit {
			ev.NotifyEnvelope = sanitizeNotifyEnvelope(ev.NotifyEnvelope)
			events = append(events, ev)
		} else {
			hasMore = true
			break
		}
	}

	nextSince := latestSeq
	if len(events) > 0 {
		nextSince = events[len(events)-1].Seq
	}
	return NotifyEventsResponse{
		Version:   notifyFeedVersion,
		LatestSeq: latestSeq,
		NextSince: nextSince,
		HasMore:   hasMore,
		Truncated: truncated,
		Events:    events,
	}
}

func writeTapJSONError(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: reason})
}

// handleEvents serves GET /events on the tailnet tap server behind the exact
// same fail-closed auth guard as /inbox, /inbox.json, and /tap-links/. It is
// strictly read-only and returns cursor-paginated sanitized events.
func (s *tapServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAuth(w, r, "", true); !ok {
		return
	}
	if !s.cfg.Enabled {
		s.refuseJSON(w, http.StatusNotFound, "", "approval taps are off")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		s.refuseJSON(w, http.StatusMethodNotAllowed, "", "events is read-only (GET only)")
		return
	}
	q, err := parseNotifyEventsQuery(r.URL.RawQuery)
	if err != nil {
		writeTapJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	feed, err := loadNotifyFeed()
	if err != nil {
		s.logf("tap: load notify events: %v", err)
		writeTapJSONError(w, http.StatusInternalServerError, "could not read notification events")
		return
	}
	resp := queryNotifyFeed(feed, q)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(resp); err != nil {
		s.logf("tap: encode notify events: %v", err)
		writeTapJSONError(w, http.StatusInternalServerError, "could not encode notification events")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func writeNotifyEvents(w io.Writer, resp NotifyEventsResponse, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}
	fmt.Fprintf(w, "notify events: %d %s (latest_seq=%d, next_since=%d, has_more=%t, truncated=%t)\n",
		len(resp.Events), plural(len(resp.Events), "event"), resp.LatestSeq, resp.NextSince, resp.HasMore, resp.Truncated)
	for _, ev := range resp.Events {
		classTag := ""
		for _, it := range ev.Items {
			if it.Class != nil {
				classTag = fmt.Sprintf(" [C%d]", *it.Class)
				break
			}
		}
		summary := ev.Body
		if summary == "" {
			summary = ev.Title
		}
		fmt.Fprintf(w, "#%d  %s  %s%s  %s\n", ev.Seq, ev.At, ev.Event, classTag, summary)
	}
	return nil
}
