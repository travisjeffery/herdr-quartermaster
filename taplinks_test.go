package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func tapLinksRequest(s *tapServer, method, path, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	return rec
}

func tokenFromTapURL(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse url %q: %v", rawURL, err)
	}
	if u.Scheme != "http" || u.Host != "100.78.70.17:8642" {
		t.Fatalf("url %q has unexpected scheme/host", rawURL)
	}
	tok, ok := strings.CutPrefix(u.Path, "/tap/")
	if !ok || tok == "" {
		t.Fatalf("url %q does not point to /tap/<token>", rawURL)
	}
	return tok
}

func decodeTapLinksError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("error Content-Type = %q, want application/json", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("error Cache-Control = %q, want no-store", cc)
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error JSON %q: %v", rec.Body.String(), err)
	}
	if payload.Error == "" {
		t.Fatalf("empty error message in %q", rec.Body.String())
	}
	return payload.Error
}

func TestTapLinksPromptFreshMintAndEndToEnd(t *testing.T) {
	testTapOwnIP(t)
	bash := screen(t, "claude-bash-4.txt")
	wantPrompt, ok := parsePrompt(bash)
	if !ok {
		t.Fatal("claude-bash-4 fixture did not parse")
	}
	f := &fakePane{screens: []string{bash, screen(t, "claude-idle.txt")}}
	s := testTapServer(t, f, "iphone181")
	loc := time.FixedZone("EST", -5*3600)
	now := time.Date(2026, 11, 24, 10, 0, 0, 500_000_000, loc)
	s.now = func() time.Time { return now }

	// First mint.
	rec1 := tapLinksRequest(s, http.MethodGet, "/tap-links/task-prompt", tapPhoneAddr)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first GET = %d %s", rec1.Code, rec1.Body.String())
	}
	if ct := rec1.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := rec1.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
	}

	// Verify exact JSON schema and RFC3339 expires formatting.
	var raw map[string]any
	if err := json.Unmarshal(rec1.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, rec1.Body.String())
	}
	if got := raw["expires"]; got != "2026-11-24T10:15:00-05:00" {
		t.Fatalf("expires = %v, want 2026-11-24T10:15:00-05:00", got)
	}
	if _, hasFreeText := raw["free_text"]; hasFreeText {
		t.Fatalf("prompt response must not include free_text: %v", raw["free_text"])
	}

	var resp1 tapLinksResponse
	if err := json.Unmarshal(rec1.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("unmarshal resp1: %v", err)
	}
	if resp1.Bead != "task-prompt" || resp1.Kind != questionKindPrompt {
		t.Fatalf("resp1 bead/kind = %+v", resp1)
	}
	if !strings.Contains(resp1.Question, "mkdir -p out2 && touch out2/hello.txt") {
		t.Fatalf("resp1 question omitted command: %q", resp1.Question)
	}
	if strings.Contains(rec1.Body.String(), `\u0026`) {
		t.Fatalf("response HTML-escaped ampersand in JSON:\n%s", rec1.Body.String())
	}
	if len(resp1.Links) != len(wantPrompt.Options) {
		t.Fatalf("links count = %d, want %d", len(resp1.Links), len(wantPrompt.Options))
	}
	for i, l := range resp1.Links {
		if l.N != wantPrompt.Options[i].Number || l.Label != wantPrompt.Options[i].Label {
			t.Errorf("link[%d] = %+v, want %+v", i, l, wantPrompt.Options[i])
		}
		tok := tokenFromTapURL(t, l.URL)
		claims, err := parseTapToken(s.secret, tok, now)
		if err != nil || claims.Kind != tapKindPrompt || claims.Bead != "task-prompt" || claims.Hash != wantPrompt.Hash {
			t.Errorf("link[%d] claims = %+v, err %v", i, claims, err)
		}
	}

	// Second mint produces different links with fresh nonces.
	now = now.Add(30 * time.Second)
	rec2 := tapLinksRequest(s, http.MethodGet, "/tap-links/task-prompt", tapPhoneAddr)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second GET = %d %s", rec2.Code, rec2.Body.String())
	}
	var resp2 tapLinksResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("unmarshal resp2: %v", err)
	}
	if len(resp2.Links) != len(resp1.Links) {
		t.Fatalf("resp2 links count = %d, want %d", len(resp2.Links), len(resp1.Links))
	}
	for i := range resp1.Links {
		if resp1.Links[i].URL == resp2.Links[i].URL {
			t.Fatalf("second call reused link[%d]: %s", i, resp1.Links[i].URL)
		}
	}

	// First set STILL works end-to-end (GET confirm, then POST answer).
	tok1 := tokenFromTapURL(t, resp1.Links[0].URL)
	if code, body := tapRequest(s, http.MethodGet, tok1, tapPhoneAddr); code != http.StatusOK || !strings.Contains(body, "Answer 1. Yes") {
		t.Fatalf("confirm with first set = %d %q", code, body)
	}
	if len(f.sent) != 0 {
		t.Fatalf("confirm GET sent keys: %q", f.sent)
	}
	if code, body := tapRequest(s, http.MethodPost, tok1, tapPhoneAddr); code != http.StatusOK || !strings.Contains(body, "Answered") {
		t.Fatalf("answer with first set = %d %q", code, body)
	}
	if len(f.sent) != 1 || !slices.Equal(f.sent[0], []string{"1"}) {
		t.Fatalf("sent = %q", f.sent)
	}

	// After the first set answered, replaying it or using the second set is refused.
	if code, _ := tapRequest(s, http.MethodPost, tok1, tapPhoneAddr); code != http.StatusConflict {
		t.Fatalf("replay of first set = %d, want 409", code)
	}
	tok2 := tokenFromTapURL(t, resp2.Links[0].URL)
	if code, _ := tapRequest(s, http.MethodPost, tok2, tapPhoneAddr); code != http.StatusConflict {
		t.Fatalf("second set after answer = %d, want 409", code)
	}
	if len(f.sent) != 1 {
		t.Fatalf("answer was not at-most-once: sent %q", f.sent)
	}
}

func TestTapLinksPiControlAskPrompt(t *testing.T) {
	testTapOwnIP(t)
	ask := &piAsk{
		ID:      "ask-7",
		Command: "git push origin feat/demo",
		Reason:  "push requires approval",
		Options: []string{"Yes, run it", "No, block it"},
	}
	env := promptEnv{
		show: func(id string) (Bead, error) {
			return Bead{ID: id, Status: StatusInProgress}, nil
		},
		agent: func(host, name string) (*Agent, error) {
			return &Agent{Name: name, Status: "blocked", PaneID: "w1:p2", Session: AgentSession{Agent: "pi"}}, nil
		},
		piAsk: func(host string, a *Agent) (*piAsk, bool, error) {
			return ask, true, nil
		},
	}
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	s, err := newTapServer(TapConfig{Enabled: true, Devices: []string{"iphone181"}}, env)
	if err != nil {
		t.Fatal(err)
	}
	s.whois = func(string) (tapIdentity, error) { return tapIdentity{Device: "iphone181"}, nil }

	rec := tapLinksRequest(s, http.MethodGet, "/tap-links/task-pi", tapPhoneAddr)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET pi prompt = %d %s", rec.Code, rec.Body.String())
	}
	var resp tapLinksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Kind != questionKindPrompt || len(resp.Links) != 2 || !strings.Contains(resp.Question, "git push origin feat/demo") {
		t.Fatalf("resp = %+v", resp)
	}
	tok := tokenFromTapURL(t, resp.Links[0].URL)
	claims, err := parseTapToken(s.secret, tok, time.Now())
	if err != nil || claims.Hash != ask.prompt().Hash || claims.Kind != tapKindPrompt {
		t.Fatalf("claims = %+v, err %v", claims, err)
	}
}

func TestTapLinksNeedsMeQuestionFreshMintAndEndToEnd(t *testing.T) {
	testTapOwnIP(t)
	wantQ, ok := parseNeedsMeQuestion(questionNotes)
	if !ok {
		t.Fatal("questionNotes did not parse")
	}

	for _, tc := range []struct {
		name        string
		agentStatus string
		agentErr    error
	}{
		{name: "idle worker", agentStatus: "idle"},
		{name: "no worker running", agentStatus: ""},
		{name: "agent lookup error still mints needs_me", agentErr: errors.New("herdr unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeQuestion(tc.agentStatus)
			f.bead.ID = "task-42.1"
			if f.agent != nil {
				f.agent.Name = agentName(f.bead.ID)
			}
			env := f.env()
			if tc.agentErr != nil {
				env.agent = func(string, string) (*Agent, error) { return nil, tc.agentErr }
			}
			t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
			s, err := newTapServer(TapConfig{Enabled: true, Devices: []string{"iphone181"}}, env)
			if err != nil {
				t.Fatal(err)
			}
			s.whois = func(string) (tapIdentity, error) { return tapIdentity{Device: "iphone181", User: "tj@"}, nil }
			now := time.Date(2026, 11, 24, 14, 0, 0, 0, time.UTC)
			s.now = func() time.Time { return now }

			rec1 := tapLinksRequest(s, http.MethodGet, "/tap-links/task-42.1", tapPhoneAddr)
			if rec1.Code != http.StatusOK {
				t.Fatalf("first GET = %d %s", rec1.Code, rec1.Body.String())
			}
			var resp1 tapLinksResponse
			if err := json.Unmarshal(rec1.Body.Bytes(), &resp1); err != nil {
				t.Fatal(err)
			}
			if resp1.Bead != "task-42.1" || resp1.Kind != questionKindNeedsMe {
				t.Fatalf("resp1 = %+v", resp1)
			}
			if !strings.Contains(resp1.Question, "Ship to all regions or one first?") {
				t.Fatalf("question = %q", resp1.Question)
			}
			if !resp1.Expires.Equal(now.Add(tapTTL)) {
				t.Fatalf("expires = %v, want %v", resp1.Expires, now.Add(tapTTL))
			}
			if len(resp1.Links) != len(wantQ.Options) {
				t.Fatalf("links = %+v, want %d", resp1.Links, len(wantQ.Options))
			}

			// Second mint gives different URLs.
			rec2 := tapLinksRequest(s, http.MethodGet, "/tap-links/task-42.1", tapPhoneAddr)
			if rec2.Code != http.StatusOK {
				t.Fatalf("second GET = %d %s", rec2.Code, rec2.Body.String())
			}
			var resp2 tapLinksResponse
			if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
				t.Fatal(err)
			}
			for i := range resp1.Links {
				if resp1.Links[i].URL == resp2.Links[i].URL {
					t.Fatalf("second call reused link[%d]: %s", i, resp1.Links[i].URL)
				}
			}

			// First set still works through GET confirm and POST answer.
			tok1 := tokenFromTapURL(t, resp1.Links[1].URL)
			if code, body := tapRequest(s, http.MethodGet, tok1, tapPhoneAddr); code != http.StatusOK || !strings.Contains(body, "Answer 2. One region first, then the rest") {
				t.Fatalf("confirm with first set = %d %q", code, body)
			}
			if f.bead.Status != StatusNeedsMe || len(f.notes) != 0 {
				t.Fatalf("confirm GET mutated state: status=%s notes=%q", f.bead.Status, f.notes)
			}
			if code, body := tapRequest(s, http.MethodPost, tok1, tapPhoneAddr); code != http.StatusOK || !strings.Contains(body, "Answered") {
				t.Fatalf("answer with first set = %d %q", code, body)
			}
			if f.bead.Status != StatusInProgress {
				t.Fatalf("status = %s, want in_progress", f.bead.Status)
			}

			// Second set is now refused because the question was answered.
			tok2 := tokenFromTapURL(t, resp2.Links[0].URL)
			if code, _ := tapRequest(s, http.MethodPost, tok2, tapPhoneAddr); code != http.StatusConflict {
				t.Fatalf("second set after answer = %d, want 409", code)
			}
		})
	}
}

func TestTapLinksExpiryBelongsToLinksNotEndpoint(t *testing.T) {
	testTapOwnIP(t)
	bash := screen(t, "claude-bash-4.txt")
	f := &fakePane{screens: []string{bash}}
	s := testTapServer(t, f, "iphone181")
	now := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	rec1 := tapLinksRequest(s, http.MethodGet, "/tap-links/task-exp", tapPhoneAddr)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first GET = %d %s", rec1.Code, rec1.Body.String())
	}
	var resp1 tapLinksResponse
	if err := json.Unmarshal(rec1.Body.Bytes(), &resp1); err != nil {
		t.Fatal(err)
	}
	tok1 := tokenFromTapURL(t, resp1.Links[0].URL)

	// Past the 15-minute TTL, the minted link returns 410 Gone, while
	// /tap-links/<bead> mints a fresh working set (never 410).
	now = now.Add(tapTTL + time.Second)
	if code, _ := tapRequest(s, http.MethodGet, tok1, tapPhoneAddr); code != http.StatusGone {
		t.Fatalf("expired link GET = %d, want 410", code)
	}

	rec2 := tapLinksRequest(s, http.MethodGet, "/tap-links/task-exp", tapPhoneAddr)
	if rec2.Code != http.StatusOK {
		t.Fatalf("fresh mint after expiry = %d %s", rec2.Code, rec2.Body.String())
	}
	var resp2 tapLinksResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatal(err)
	}
	tok2 := tokenFromTapURL(t, resp2.Links[0].URL)
	if code, _ := tapRequest(s, http.MethodGet, tok2, tapPhoneAddr); code != http.StatusOK {
		t.Fatalf("fresh link GET = %d, want 200", code)
	}
}

func TestTapLinksRefusalsAuthAndDisabled(t *testing.T) {
	testTapOwnIP(t)
	bash := screen(t, "claude-bash-4.txt")

	t.Run("disallowed device refused and journaled", func(t *testing.T) {
		f := &fakePane{screens: []string{bash}}
		s := testTapServer(t, f, "desktop")
		rec := tapLinksRequest(s, http.MethodGet, "/tap-links/task-1", "100.78.70.17:49152")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
		}
		errMsg := decodeTapLinksError(t, rec)
		if !strings.Contains(errMsg, "device desktop is not allowed") {
			t.Fatalf("error = %q", errMsg)
		}
		if strings.Contains(rec.Body.String(), "/tap/") {
			t.Fatalf("refused response minted a link: %s", rec.Body.String())
		}
		recs, err := readJournal(time.Time{})
		if err != nil || len(recs) != 1 || recs[0].Kind != journalTap || recs[0].Bead != "task-1" || !strings.HasPrefix(recs[0].Text, "TAP REFUSED:") {
			t.Fatalf("journal = %+v, err %v", recs, err)
		}
	})

	t.Run("non-tailnet source refused without whois and journaled", func(t *testing.T) {
		f := &fakePane{screens: []string{bash}}
		s := testTapServer(t, f, "iphone181")
		whoisCalled := false
		s.whois = func(string) (tapIdentity, error) {
			whoisCalled = true
			return tapIdentity{Device: "iphone181"}, nil
		}
		for _, addr := range []string{"127.0.0.1:49152", "192.168.4.8:49152", "bad-addr"} {
			for _, path := range []string{"/tap-links/task-1", "/tap-links"} {
				rec := tapLinksRequest(s, http.MethodGet, path, addr)
				if rec.Code != http.StatusForbidden {
					t.Errorf("GET %s from %s = %d, want 403", path, addr, rec.Code)
				}
				_ = decodeTapLinksError(t, rec)
				if strings.Contains(rec.Body.String(), "/tap/") {
					t.Errorf("refused response minted a link: %s", rec.Body.String())
				}
			}
		}
		if whoisCalled {
			t.Fatal("whois ran for non-tailnet source")
		}
		recs, err := readJournal(time.Time{})
		if err != nil || len(recs) != 6 {
			t.Fatalf("journal count = %d, want 6 (err %v)", len(recs), err)
		}
		for _, r := range recs {
			if r.Kind != journalTap || !strings.HasPrefix(r.Text, "TAP REFUSED:") {
				t.Errorf("unexpected journal record: %+v", r)
			}
		}
	})

	t.Run("whois error refused and journaled", func(t *testing.T) {
		f := &fakePane{screens: []string{bash}}
		s := testTapServer(t, f, "iphone181")
		s.whois = func(string) (tapIdentity, error) { return tapIdentity{}, errors.New("tailscaled down") }
		rec := tapLinksRequest(s, http.MethodGet, "/tap-links/task-1", tapPhoneAddr)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		errMsg := decodeTapLinksError(t, rec)
		if !strings.Contains(errMsg, "tailscaled down") {
			t.Fatalf("error = %q", errMsg)
		}
		recs, err := readJournal(time.Time{})
		if err != nil || len(recs) != 1 || recs[0].Kind != journalTap || !strings.HasPrefix(recs[0].Text, "TAP REFUSED:") {
			t.Fatalf("journal = %+v, err %v", recs, err)
		}
	})

	t.Run("tap disabled mints nothing and journals refusal", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		f := &fakePane{screens: []string{bash}}
		s, err := newTapServer(TapConfig{Enabled: false, Devices: []string{"iphone181"}}, f.env())
		if err != nil {
			t.Fatal(err)
		}
		whoisCalled := false
		s.whois = func(string) (tapIdentity, error) {
			whoisCalled = true
			return tapIdentity{Device: "iphone181"}, nil
		}
		rec := tapLinksRequest(s, http.MethodGet, "/tap-links/task-1", tapPhoneAddr)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("disabled status = %d, want 404 (%s)", rec.Code, rec.Body.String())
		}
		errMsg := decodeTapLinksError(t, rec)
		if !strings.Contains(errMsg, "taps are off") {
			t.Fatalf("error = %q", errMsg)
		}
		if whoisCalled {
			t.Fatal("whois ran while [tap] disabled")
		}
		if strings.Contains(rec.Body.String(), "/tap/") {
			t.Fatalf("disabled response minted a link: %s", rec.Body.String())
		}
		recs, err := readJournal(time.Time{})
		if err != nil || len(recs) != 1 || recs[0].Kind != journalTap || recs[0].Bead != "task-1" || !strings.HasPrefix(recs[0].Text, "TAP REFUSED:") {
			t.Fatalf("journal = %+v, err %v", recs, err)
		}
	})

	t.Run("empty devices allowlist fails closed", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		f := &fakePane{screens: []string{bash}}
		s, err := newTapServer(TapConfig{Enabled: true, Devices: nil}, f.env())
		if err != nil {
			t.Fatal(err)
		}
		s.whois = func(string) (tapIdentity, error) { return tapIdentity{Device: "iphone181"}, nil }
		rec := tapLinksRequest(s, http.MethodGet, "/tap-links/task-1", tapPhoneAddr)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("empty allowlist status = %d, want 403", rec.Code)
		}
		_ = decodeTapLinksError(t, rec)
		recs, err := readJournal(time.Time{})
		if err != nil || len(recs) != 1 || !strings.HasPrefix(recs[0].Text, "TAP REFUSED:") {
			t.Fatalf("journal = %+v, err %v", recs, err)
		}
	})

	t.Run("non-GET methods and listing paths refused", func(t *testing.T) {
		f := &fakePane{screens: []string{bash}}
		s := testTapServer(t, f, "iphone181")
		for _, method := range []string{http.MethodPost, http.MethodHead, http.MethodPut} {
			rec := tapLinksRequest(s, method, "/tap-links/task-1", tapPhoneAddr)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s /tap-links/task-1 = %d, want 405", method, rec.Code)
			}
			if rec.Header().Get("Allow") != http.MethodGet {
				t.Errorf("%s Allow = %q, want GET", method, rec.Header().Get("Allow"))
			}
		}
		for _, path := range []string{"/tap-links", "/tap-links/", "/tap-links/task-1/extra", "/tap-links/bad!id"} {
			rec := tapLinksRequest(s, http.MethodGet, path, tapPhoneAddr)
			if rec.Code != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404 (%s)", path, rec.Code, rec.Body.String())
			}
			_ = decodeTapLinksError(t, rec)
		}
		// Unauthenticated POST must still be 403, not 405.
		rec := tapLinksRequest(s, http.MethodPost, "/tap-links/task-1", "127.0.0.1:49152")
		if rec.Code != http.StatusForbidden {
			t.Errorf("unauth POST = %d, want 403", rec.Code)
		}
	})
}

func TestTapLinksNonLiveTargetsReturn404(t *testing.T) {
	testTapOwnIP(t)
	bash := screen(t, "claude-bash-4.txt")
	idle := screen(t, "claude-idle.txt")

	cases := []struct {
		name     string
		env      func() promptEnv
		wantPart string
	}{
		{
			name: "bead not found",
			env: func() promptEnv {
				return promptEnv{
					show: func(id string) (Bead, error) {
						return Bead{}, fmt.Errorf("%w: %s", errBeadNotFound, id)
					},
				}
			},
			wantPart: "not found",
		},
		{
			name: "closed bead",
			env: func() promptEnv {
				return promptEnv{
					show: func(id string) (Bead, error) { return Bead{ID: id, Status: StatusClosed}, nil },
					agent: func(string, string) (*Agent, error) {
						return &Agent{Name: "task-1", Status: "blocked", PaneID: "w1:p1"}, nil
					},
					read: func(string, string) (string, error) { return bash, nil },
				}
			},
			wantPart: "closed",
		},
		{
			name: "RUN bead idle",
			env: func() promptEnv {
				f := newFakeQuestion("idle")
				f.bead.ID = "task-1"
				f.bead.Notes = "RUN: aws sso login\nwhy: token expired"
				return f.env()
			},
			wantPart: "RUN:",
		},
		{
			name: "RUN bead even with blocked pane",
			env: func() promptEnv {
				return promptEnv{
					show: func(id string) (Bead, error) {
						return Bead{ID: id, Status: StatusNeedsMe, Notes: "RUN: aws sso login\nwhy: expired"}, nil
					},
					agent: func(string, string) (*Agent, error) {
						return &Agent{Name: "task-1", Status: "blocked", PaneID: "w1:p1"}, nil
					},
					read: func(string, string) (string, error) { return bash, nil },
				}
			},
			wantPart: "RUN:",
		},
		{
			name: "needs_me worker working again",
			env: func() promptEnv {
				f := newFakeQuestion("working")
				f.bead.ID = "task-1"
				return f.env()
			},
			wantPart: "working again",
		},
		{
			name: "needs_me already answered",
			env: func() promptEnv {
				f := newFakeQuestion("idle")
				f.bead.ID = "task-1"
				f.bead.Notes = questionNotes + "\nNEEDS_ME ANSWERED: 1 All regions at once [abcd1234] by TJ via tap"
				return f.env()
			},
			wantPart: "gone",
		},
		{
			name: "needs_me free text without OPTIONS",
			env: func() promptEnv {
				f := newFakeQuestion("idle")
				f.bead.ID = "task-1"
				f.bead.Notes = "Should we rename this flag before shipping?"
				return f.env()
			},
			wantPart: "OPTIONS",
		},
		{
			name: "in_progress worker with no prompt showing",
			env: func() promptEnv {
				f := &fakePane{screens: []string{idle}}
				return f.env()
			},
			wantPart: "no prompt showing",
		},
		{
			name: "in_progress bead with no worker running",
			env: func() promptEnv {
				return promptEnv{
					show:  func(id string) (Bead, error) { return Bead{ID: id, Status: StatusInProgress}, nil },
					agent: func(string, string) (*Agent, error) { return nil, nil },
				}
			},
			wantPart: "no worker running",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
			s, err := newTapServer(TapConfig{Enabled: true, Devices: []string{"iphone181"}}, tc.env())
			if err != nil {
				t.Fatal(err)
			}
			s.whois = func(string) (tapIdentity, error) { return tapIdentity{Device: "iphone181"}, nil }
			rec := tapLinksRequest(s, http.MethodGet, "/tap-links/task-1", tapPhoneAddr)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body.String())
			}
			errMsg := decodeTapLinksError(t, rec)
			if !strings.Contains(errMsg, tc.wantPart) {
				t.Errorf("error = %q, want substring %q", errMsg, tc.wantPart)
			}
			recs, err := readJournal(time.Time{})
			if err != nil || len(recs) != 1 || recs[0].Kind != journalTap || !strings.HasPrefix(recs[0].Text, "TAP REFUSED:") {
				t.Fatalf("journal = %+v, err %v", recs, err)
			}
		})
	}
}

func TestTapLinksPromptPrecedenceOverNeedsMe(t *testing.T) {
	testTapOwnIP(t)
	bash := screen(t, "claude-bash-4.txt")
	wantBash, _ := parsePrompt(bash)

	env := promptEnv{
		show: func(id string) (Bead, error) {
			return Bead{ID: id, Status: StatusNeedsMe, Notes: questionNotes}, nil
		},
		agent: func(string, string) (*Agent, error) {
			return &Agent{Name: "task-both", Status: "blocked", PaneID: "w1:p1"}, nil
		},
		read: func(string, string) (string, error) { return bash, nil },
	}
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	s, err := newTapServer(TapConfig{Enabled: true, Devices: []string{"iphone181"}}, env)
	if err != nil {
		t.Fatal(err)
	}
	s.whois = func(string) (tapIdentity, error) { return tapIdentity{Device: "iphone181"}, nil }

	rec := tapLinksRequest(s, http.MethodGet, "/tap-links/task-both", tapPhoneAddr)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body.String())
	}
	var resp tapLinksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Kind != questionKindPrompt || len(resp.Links) != len(wantBash.Options) {
		t.Fatalf("resp = %+v, want prompt with %d links", resp, len(wantBash.Options))
	}
}

func TestTapLinksMintedURLsNeverPersistedOrLogged(t *testing.T) {
	testTapOwnIP(t)
	stateRoot := t.TempDir()
	t.Setenv("QUARTERMASTER_STATE_DIR", stateRoot)

	bash := screen(t, "claude-bash-4.txt")
	qBead := Bead{ID: "task-q", Status: StatusNeedsMe, Notes: questionNotes}
	pBead := Bead{ID: "task-p", Status: StatusInProgress}
	var notesWritten []string
	var logLines []string

	env := promptEnv{
		show: func(id string) (Bead, error) {
			switch id {
			case pBead.ID:
				return pBead, nil
			case qBead.ID:
				return qBead, nil
			default:
				return Bead{}, fmt.Errorf("%w: %s", errBeadNotFound, id)
			}
		},
		agent: func(host, name string) (*Agent, error) {
			switch name {
			case agentName(pBead.ID):
				return &Agent{Name: name, Status: "blocked", PaneID: "w1:p1"}, nil
			case agentName(qBead.ID):
				return &Agent{Name: name, Status: "idle", PaneID: "w1:p2"}, nil
			default:
				return nil, nil
			}
		},
		read: func(host, pane string) (string, error) {
			if pane == "w1:p1" {
				return bash, nil
			}
			return "idle", nil
		},
		note: func(id, text string) error {
			notesWritten = append(notesWritten, id+": "+text)
			return nil
		},
	}

	s, err := newTapServer(TapConfig{Enabled: true, Devices: []string{"iphone181"}}, env)
	if err != nil {
		t.Fatal(err)
	}
	s.whois = func(string) (tapIdentity, error) { return tapIdentity{Device: "iphone181", User: "tj@"}, nil }
	s.logf = func(format string, args ...any) {
		logLines = append(logLines, fmt.Sprintf(format, args...))
	}

	var secretsToCheck []string
	for _, id := range []string{pBead.ID, pBead.ID, qBead.ID, qBead.ID} {
		rec := tapLinksRequest(s, http.MethodGet, "/tap-links/"+id, tapPhoneAddr)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /tap-links/%s = %d %s", id, rec.Code, rec.Body.String())
		}
		var resp tapLinksResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Links) == 0 {
			t.Fatalf("no links minted for %s", id)
		}
		for _, l := range resp.Links {
			tok := tokenFromTapURL(t, l.URL)
			claims, err := parseTapToken(s.secret, tok, time.Now())
			if err != nil {
				t.Fatalf("parse minted token: %v", err)
			}
			secretsToCheck = append(secretsToCheck, l.URL, tok, claims.Nonce)
		}
	}

	// Verify journal recorded the mint events without any URLs or tokens.
	recs, err := readJournal(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 4 {
		t.Fatalf("journal records = %d, want 4: %+v", len(recs), recs)
	}
	for i, r := range recs {
		if r.Kind != journalTap || r.Action != "mint" || !r.Pass {
			t.Errorf("journal[%d] = %+v, want kind=tap action=mint pass=true", i, r)
		}
		if !strings.HasPrefix(r.Text, "TAP MINTED:") || !strings.Contains(r.Text, "iphone181") {
			t.Errorf("journal[%d].Text = %q, want TAP MINTED with device", i, r.Text)
		}
	}

	// Inbox must remain untouched (no files written).
	if items, err := readInbox(); err != nil || len(items) != 0 {
		t.Fatalf("inbox items = %+v, err %v; minting must not write inbox events", items, err)
	}

	// No bead notes written.
	if len(notesWritten) != 0 {
		t.Fatalf("minting wrote bead notes: %q", notesWritten)
	}

	// No minted URL, token, nonce, or "/tap/" appears in ANY file under stateDir.
	fileCount := 0
	err = filepath.WalkDir(stateRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		fileCount++
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		content := string(raw)
		if strings.Contains(content, "/tap/") {
			t.Errorf("file %s contains /tap/ URL fragment:\n%s", path, content)
		}
		for _, needle := range secretsToCheck {
			if strings.Contains(content, needle) {
				t.Errorf("file %s leaked minted bearer credential %q", path, needle)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if fileCount == 0 {
		t.Fatal("expected at least journal.jsonl and tap/secret under state dir")
	}

	// Logs must also never contain any minted URL, token, nonce, or "/tap/".
	joinedLogs := strings.Join(logLines, "\n")
	if strings.Contains(joinedLogs, "/tap/") {
		t.Errorf("logs contain /tap/ URL fragment:\n%s", joinedLogs)
	}
	for _, needle := range secretsToCheck {
		if strings.Contains(joinedLogs, needle) {
			t.Errorf("logs leaked minted bearer credential %q", needle)
		}
	}
}

func TestTapLinksBaseURLHandlingAndChangedPrompt(t *testing.T) {
	bash := screen(t, "claude-bash-4.txt")
	write := screen(t, "claude-write-3.txt")
	wantWrite, _ := parsePrompt(write)

	t.Run("prefers listener base URL without shelling out to tailscale", func(t *testing.T) {
		old := tapOwnIP
		tapOwnIP = func() (string, error) { return "", errors.New("tapOwnIP must not be called when s.base is set") }
		t.Cleanup(func() { tapOwnIP = old })

		f := &fakePane{screens: []string{bash}}
		s := testTapServer(t, f, "iphone181")
		s.base = "http://100.78.70.17:8642"

		rec := tapLinksRequest(s, http.MethodGet, "/tap-links/task-base", tapPhoneAddr)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET = %d %s", rec.Code, rec.Body.String())
		}
		var resp tapLinksResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Links) == 0 || !strings.HasPrefix(resp.Links[0].URL, "http://100.78.70.17:8642/tap/") {
			t.Fatalf("links = %+v", resp.Links)
		}
	})

	t.Run("fails closed when base URL cannot resolve", func(t *testing.T) {
		old := tapOwnIP
		tapOwnIP = func() (string, error) { return "", errors.New("tailscale is down") }
		t.Cleanup(func() { tapOwnIP = old })

		f := &fakePane{screens: []string{bash}}
		s := testTapServer(t, f, "iphone181")
		rec := tapLinksRequest(s, http.MethodGet, "/tap-links/task-down", tapPhoneAddr)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("GET = %d, want 500 (%s)", rec.Code, rec.Body.String())
		}
		errMsg := decodeTapLinksError(t, rec)
		if !strings.Contains(errMsg, "tailscale is down") {
			t.Fatalf("error = %q", errMsg)
		}
		recs, err := readJournal(time.Time{})
		if err != nil || len(recs) != 1 || !strings.HasPrefix(recs[0].Text, "TAP REFUSED:") {
			t.Fatalf("journal = %+v, err %v", recs, err)
		}
	})

	t.Run("changed prompt mints for new prompt while old link is refused", func(t *testing.T) {
		testTapOwnIP(t)
		f := &fakePane{screens: []string{bash}}
		s := testTapServer(t, f, "iphone181")

		rec1 := tapLinksRequest(s, http.MethodGet, "/tap-links/task-change", tapPhoneAddr)
		if rec1.Code != http.StatusOK {
			t.Fatalf("first GET = %d %s", rec1.Code, rec1.Body.String())
		}
		var resp1 tapLinksResponse
		if err := json.Unmarshal(rec1.Body.Bytes(), &resp1); err != nil {
			t.Fatal(err)
		}
		tok1 := tokenFromTapURL(t, resp1.Links[0].URL)

		// Worker moves to a different prompt.
		f.screens = []string{write, screen(t, "claude-idle.txt")}
		if code, _ := tapRequest(s, http.MethodGet, tok1, tapPhoneAddr); code != http.StatusConflict {
			t.Fatalf("old link on changed prompt = %d, want 409", code)
		}

		rec2 := tapLinksRequest(s, http.MethodGet, "/tap-links/task-change", tapPhoneAddr)
		if rec2.Code != http.StatusOK {
			t.Fatalf("second GET = %d %s", rec2.Code, rec2.Body.String())
		}
		var resp2 tapLinksResponse
		if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
			t.Fatal(err)
		}
		if len(resp2.Links) != len(wantWrite.Options) {
			t.Fatalf("new links = %d, want %d", len(resp2.Links), len(wantWrite.Options))
		}
		tok2 := tokenFromTapURL(t, resp2.Links[0].URL)
		if code, _ := tapRequest(s, http.MethodPost, tok2, tapPhoneAddr); code != http.StatusOK {
			t.Fatalf("new link POST = %d, want 200", code)
		}
	})
}
