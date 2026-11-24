package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// On-demand tap link minting: GET /tap-links/<bead> on the existing tailnet
// tap listener mints fresh, short-lived (15m) signed one-time /tap/<token>
// links for a single bead's live approval prompt or live needs_me question.
// Every request enforces the same fail-closed guards as a tap: [tap] enabled,
// tailnet source IP, `tailscale whois`, and explicit membership in [tap]
// devices. Minted URLs are bearer credentials returned only in the HTTP
// response — never stored, logged, journalled, or written to the inbox.

type tapLinksResponse struct {
	Bead     string            `json:"bead"`
	Kind     string            `json:"kind"`
	Question string            `json:"question"`
	Expires  time.Time         `json:"expires"`
	Links    []tapLinksItem    `json:"links"`
	FreeText *tapLinksFreeText `json:"free_text,omitempty"`
}

type tapLinksItem struct {
	N     int    `json:"n"`
	Label string `json:"label"`
	URL   string `json:"url"`
}

type tapLinksFreeText struct {
	URL      string `json:"url"`
	MaxChars int    `json:"max_chars"`
}

func (s *tapServer) tapLinksBaseURL() (string, error) {
	if s.base != "" {
		return strings.TrimRight(s.base, "/"), nil
	}
	return tapBaseURL(s.cfg)
}

func (s *tapServer) journalMint(bead, kind, device string, count int) {
	text := fmt.Sprintf("TAP MINTED: %d %s link(s) for device %s", count, kind, device)
	rec := journalRecord{
		At:     s.now(),
		Kind:   journalTap,
		Bead:   bead,
		Class:  -1,
		Action: "mint",
		Pass:   true,
		Text:   text,
	}
	if err := appendJournal(rec); err != nil {
		s.logf("tap: journal a mint: %v", err)
	}
	s.logf("tap minted: bead=%s kind=%s device=%s count=%d", bead, kind, device, count)
}

func (s *tapServer) handleTapLinks(w http.ResponseWriter, r *http.Request) {
	rawBead := ""
	if rest, ok := strings.CutPrefix(r.URL.Path, "/tap-links/"); ok {
		rawBead = rest
	}
	journalBead := ""
	if rawBead != "" && !strings.Contains(rawBead, "/") && beadName.MatchString(rawBead) {
		journalBead = rawBead
	}

	if !s.cfg.Enabled {
		s.refuseJSON(w, http.StatusNotFound, journalBead, "approval taps are off")
		return
	}
	ident, ok := s.requireAuth(w, r, journalBead, true)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		s.refuseJSON(w, http.StatusMethodNotAllowed, journalBead, "tap-links is GET only")
		return
	}
	if rawBead == "" {
		s.refuseJSON(w, http.StatusNotFound, "", "bead id is required")
		return
	}
	if journalBead == "" {
		s.refuseJSON(w, http.StatusNotFound, "", "invalid bead id")
		return
	}
	bead := journalBead

	if s.env.show == nil {
		s.refuseJSON(w, http.StatusInternalServerError, bead, "bead lookup is unavailable")
		return
	}
	b, err := s.env.show(bead)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errBeadNotFound) || strings.Contains(strings.ToLower(err.Error()), "not found") {
			status = http.StatusNotFound
		}
		s.refuseJSON(w, status, bead, err.Error())
		return
	}
	if b.ID == "" {
		b.ID = bead
	}
	if b.Status == StatusClosed {
		s.refuseJSON(w, http.StatusNotFound, b.ID, fmt.Sprintf("the bead is gone: %s is closed", b.ID))
		return
	}
	if b.Status == StatusNeedsMe && pendingCommand(b.Notes) != "" {
		s.refuseJSON(w, http.StatusNotFound, b.ID, fmt.Sprintf("%s is a RUN: bead; commands cannot be tapped", b.ID))
		return
	}

	host := beadHost(b)
	if host == "" {
		host = localHost
	}
	var (
		a        *Agent
		agentErr error
	)
	if s.env.agent != nil {
		a, agentErr = s.env.agent(host, agentName(b.ID))
	}

	var (
		livePrompt *ApprovalPrompt
		promptErr  error
	)
	switch {
	case agentErr != nil:
		promptErr = fmt.Errorf("find the worker%s: %w", onHost(host), agentErr)
	case a == nil:
		promptErr = fmt.Errorf("%s has no worker running%s", b.ID, onHost(host))
	default:
		p, err := readAgentPrompt(s.env, host, a)
		if err == nil {
			livePrompt = &p
		} else {
			promptErr = err
		}
	}

	var (
		kind     string
		question string
		liveQ    *NeedsMeQuestion
	)
	if livePrompt != nil {
		kind = questionKindPrompt
		question = promptQuestionText(*livePrompt)
	} else if b.Status == StatusNeedsMe {
		q, ok := parseNeedsMeQuestion(b.Notes)
		if !ok {
			if strings.Contains(b.Notes, "NEEDS_ME ANSWERED:") || strings.Contains(b.Notes, "OPTIONS:") {
				s.refuseJSON(w, http.StatusNotFound, b.ID, fmt.Sprintf("the question is gone: %s no longer ends in a live OPTIONS question", b.ID))
			} else {
				s.refuseJSON(w, http.StatusNotFound, b.ID, fmt.Sprintf("%s has no live prompt or tappable OPTIONS question", b.ID))
			}
			return
		}
		if a != nil && a.Status == "working" {
			s.refuseJSON(w, http.StatusNotFound, b.ID, errWorkerWorking.Error())
			return
		}
		kind = questionKindNeedsMe
		question = strings.Join(q.Lines, "\n")
		liveQ = &q
	} else {
		reason := "no prompt showing"
		if promptErr != nil {
			reason = promptErr.Error()
		}
		s.refuseJSON(w, http.StatusNotFound, b.ID, reason)
		return
	}

	if len(s.secret) < 16 {
		s.refuseJSON(w, http.StatusInternalServerError, b.ID, "tap secret is unavailable")
		return
	}
	base, err := s.tapLinksBaseURL()
	if err != nil {
		s.refuseJSON(w, http.StatusInternalServerError, b.ID, "tap base URL unavailable: "+err.Error())
		return
	}

	now := s.now()
	var minted []tapLink
	if livePrompt != nil {
		minted, err = mintTapLinks(s.secret, base, b.ID, *livePrompt, now)
	} else {
		minted, err = mintQuestionTapLinks(s.secret, base, b.ID, *liveQ, now)
	}
	if err != nil || len(minted) == 0 {
		reason := "could not mint tap links"
		if err != nil {
			reason += ": " + err.Error()
		}
		s.refuseJSON(w, http.StatusInternalServerError, b.ID, reason)
		return
	}

	items := make([]tapLinksItem, len(minted))
	for i, l := range minted {
		items[i] = tapLinksItem{N: l.Option.Number, Label: l.Option.Label, URL: l.URL}
	}
	resp := tapLinksResponse{
		Bead:     b.ID,
		Kind:     kind,
		Question: question,
		Expires:  time.Unix(minted[0].Exp.Unix(), 0).In(now.Location()),
		Links:    items,
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(resp); err != nil {
		s.logf("tap: encode tap-links: %v", err)
		s.refuseJSON(w, http.StatusInternalServerError, b.ID, "could not encode tap links JSON")
		return
	}

	s.journalMint(b.ID, kind, ident.Device, len(items))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
