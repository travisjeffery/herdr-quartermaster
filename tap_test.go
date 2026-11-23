package main

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

const tapPhoneAddr = "100.98.26.119:49152" // any tailnet address; whois is faked

// testTapServer builds a tap server over a fake pane, with whois reporting
// device. The nonce store and journal are the real ones, in a temp state dir.
func testTapServer(t *testing.T, f *fakePane, device string) *tapServer {
	t.Helper()
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	s, err := newTapServer(TapConfig{Enabled: true, Devices: []string{"iphone181"}}, f.env())
	if err != nil {
		t.Fatal(err)
	}
	s.whois = func(string) (tapIdentity, error) { return tapIdentity{Device: device, User: "tj@"}, nil }
	return s
}

func tapRequest(s *tapServer, method, token, remoteAddr string) (int, string) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/tap/"+token, nil)
	req.RemoteAddr = remoteAddr
	s.handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func tapTokenFor(t *testing.T, s *tapServer, p ApprovalPrompt, choice string) string {
	t.Helper()
	tok, _, err := mintTapToken(s.secret, "backend-ab12", p.Hash, choice, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func testTapOwnIP(t *testing.T) {
	t.Helper()
	old := tapOwnIP
	tapOwnIP = func() (string, error) { return "100.78.70.17", nil }
	t.Cleanup(func() { tapOwnIP = old })
}

func TestTapTokenRoundTrip(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	now := time.Now()
	tok, minted, err := mintTapToken(secret, "backend-ab12", "abcd1234", "2", now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseTapToken(secret, tok, now)
	if err != nil || got.Bead != "backend-ab12" || got.Hash != "abcd1234" || got.Choice != "2" || got.Nonce != minted.Nonce {
		t.Fatalf("claims = %+v, err %v", got, err)
	}
	if _, err := parseTapToken(secret, tok, now.Add(tapTTL+time.Second)); !errors.Is(err, errTapExpired) {
		t.Errorf("expired: err = %v", err)
	}
	if _, err := parseTapToken([]byte("another-secret-0123456789abcdef"), tok, now); err == nil {
		t.Error("a link signed with another secret parsed")
	}
	if _, err := parseTapToken(secret, tok+"x", now); err == nil {
		t.Error("a tampered link parsed")
	}
	if _, err := parseTapToken(secret, "not-a-link", now); err == nil {
		t.Error("garbage parsed as a tap link")
	}
	// A link names exactly one prompt instance: another hash needs another link.
	other, _, _ := mintTapToken(secret, "backend-ab12", "ffff9999", "2", now)
	if other == tok {
		t.Error("links for different prompts collided")
	}
}

func TestTapAnswersPromptEndToEnd(t *testing.T) {
	bash := screen(t, "claude-bash-4.txt")
	p, _ := parsePrompt(bash)
	f := &fakePane{screens: []string{bash, screen(t, "claude-idle.txt")}}
	s := testTapServer(t, f, "iphone181")
	tok := tapTokenFor(t, s, p, "1")

	// The tap target first shows the actual command; opening it answers nothing.
	code, body := tapRequest(s, http.MethodGet, tok, tapPhoneAddr)
	// The command is the agent's screen text, so it arrives HTML-escaped.
	if code != http.StatusOK || !strings.Contains(body, "mkdir -p out2 &amp;&amp; touch out2/hello.txt") || !strings.Contains(body, "Answer 1. Yes") {
		t.Fatalf("confirm page = %d %q", code, body)
	}
	if len(f.sent) != 0 {
		t.Fatalf("a GET sent keys: %q", f.sent)
	}

	code, body = tapRequest(s, http.MethodPost, tok, tapPhoneAddr)
	if code != http.StatusOK || !strings.Contains(body, "Answered") {
		t.Fatalf("answer = %d %q", code, body)
	}
	if len(f.sent) != 1 || !slices.Equal(f.sent[0], []string{"1"}) {
		t.Fatalf("sent %q", f.sent)
	}
	want := "PROMPT ANSWERED: 1 Yes to Bash command: Create out2 directory and touch hello.txt [" + p.Hash + "] by TJ via tap (device iphone181)"
	if !slices.Equal(f.notes, []string{want}) {
		t.Fatalf("notes = %q", f.notes)
	}
	recs, err := readJournal(time.Time{})
	if err != nil || len(recs) != 1 || recs[0].Kind != journalPrompt || recs[0].Text != want {
		t.Fatalf("journal = %+v, err %v", recs, err)
	}
}

func TestTapStalePromptRefused(t *testing.T) {
	bash, _ := parsePrompt(screen(t, "claude-bash-4.txt"))
	for name, current := range map[string]string{
		"changed":          "claude-write-3.txt", // a different prompt is showing now
		"already answered": "claude-idle.txt",    // the prompt is gone entirely
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakePane{screens: []string{screen(t, current)}}
			s := testTapServer(t, f, "iphone181")
			tok := tapTokenFor(t, s, bash, "1")
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				code, body := tapRequest(s, method, tok, tapPhoneAddr)
				if code != http.StatusConflict || !strings.Contains(body, "Not answered") {
					t.Fatalf("%s = %d %q", method, code, body)
				}
			}
			if len(f.sent) != 0 || len(f.notes) != 0 {
				t.Fatalf("a stale tap changed something: sent %q, notes %q", f.sent, f.notes)
			}
			recs, err := readJournal(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			var refused []journalRecord
			for _, r := range recs {
				if r.Kind == journalTap && strings.HasPrefix(r.Text, "TAP REFUSED:") && r.Bead == "backend-ab12" {
					refused = append(refused, r)
				}
			}
			if len(refused) == 0 {
				t.Fatalf("no refusal journalled: %+v", recs)
			}
		})
	}
}

func TestTapReplayRefused(t *testing.T) {
	bash := screen(t, "claude-bash-4.txt")
	p, _ := parsePrompt(bash)
	f := &fakePane{screens: []string{bash, screen(t, "claude-idle.txt")}}
	s := testTapServer(t, f, "iphone181")
	tok := tapTokenFor(t, s, p, "1")
	if code, _ := tapRequest(s, http.MethodPost, tok, tapPhoneAddr); code != http.StatusOK {
		t.Fatalf("first tap = %d", code)
	}
	code, body := tapRequest(s, http.MethodPost, tok, tapPhoneAddr)
	if code != http.StatusConflict || !strings.Contains(body, "already used") {
		t.Fatalf("replay = %d %q", code, body)
	}
	if len(f.sent) != 1 || len(f.notes) != 1 {
		t.Fatalf("the replay acted again: sent %q, notes %q", f.sent, f.notes)
	}
}

func TestTapWrongDeviceRefused(t *testing.T) {
	bash := screen(t, "claude-bash-4.txt")
	p, _ := parsePrompt(bash)
	f := &fakePane{screens: []string{bash, screen(t, "claude-idle.txt")}}
	// This machine itself: a worker that read the link out of the inbox and
	// curled it is the wrong device.
	s := testTapServer(t, f, "desktop")
	tok := tapTokenFor(t, s, p, "1")
	code, body := tapRequest(s, http.MethodPost, tok, "100.78.70.17:49152")
	if code != http.StatusForbidden || !strings.Contains(body, "device desktop is not allowed") {
		t.Fatalf("wrong device = %d %q", code, body)
	}
	if len(f.sent) != 0 || len(f.notes) != 0 {
		t.Fatalf("a wrong-device tap changed something: sent %q, notes %q", f.sent, f.notes)
	}
	// The refusal did not spend the link: TJ's phone can still use it.
	s.whois = func(string) (tapIdentity, error) { return tapIdentity{Device: "iphone181"}, nil }
	if code, body := tapRequest(s, http.MethodPost, tok, tapPhoneAddr); code != http.StatusOK {
		t.Fatalf("tap after a refused one = %d %q", code, body)
	}
}

func TestTapOffTailnetRefused(t *testing.T) {
	bash := screen(t, "claude-bash-4.txt")
	p, _ := parsePrompt(bash)
	f := &fakePane{screens: []string{bash, screen(t, "claude-idle.txt")}}
	s := testTapServer(t, f, "iphone181")
	asked := false
	s.whois = func(string) (tapIdentity, error) { asked = true; return tapIdentity{Device: "iphone181"}, nil }
	tok := tapTokenFor(t, s, p, "1")
	for _, addr := range []string{"192.168.4.8:49152", "127.0.0.1:49152"} {
		if code, _ := tapRequest(s, http.MethodPost, tok, addr); code != http.StatusForbidden {
			t.Errorf("tap from %s = %d, want 403", addr, code)
		}
	}
	if asked {
		t.Error("whois ran for a non-tailnet source")
	}
	if len(f.sent) != 0 {
		t.Fatalf("an off-tailnet tap sent keys: %q", f.sent)
	}
}

func TestTapDisabledByDefault(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	testTapOwnIP(t)
	if defaultConfig().Tap.Enabled {
		t.Fatal("taps are on in the default config")
	}
	f := &fakePane{screens: []string{screen(t, "claude-bash-4.txt")}}
	if _, err := tapLinksText(Config{}, f.env(), "backend-ab12", time.Now()); err == nil || !strings.Contains(err.Error(), "taps are off") {
		t.Errorf("links while off: err = %v", err)
	}
	if _, err := startTapServer(TapConfig{}, f.env(), nil); err == nil {
		t.Error("the tap server started while off")
	}
	// Enabled without a device allowlist fails closed.
	on := Config{Tap: TapConfig{Enabled: true}}
	if err := on.validateTap(); err == nil || !strings.Contains(err.Error(), "devices") {
		t.Errorf("validateTap without devices: err = %v", err)
	}
	if _, err := tapLinksText(on, f.env(), "backend-ab12", time.Now()); err == nil {
		t.Error("links minted without a device allowlist")
	}
	// No tailnet address, no listener and no links.
	tapOwnIP = func() (string, error) { return "", errors.New("tailscale is down") }
	on.Tap.Devices = []string{"iphone181"}
	if _, err := startTapServer(on.Tap, f.env(), nil); err == nil {
		t.Error("the tap server started without a tailnet address")
	}
	if _, err := tapLinksText(on, f.env(), "backend-ab12", time.Now()); err == nil {
		t.Error("links minted without a tailnet address")
	}
}

func TestTapLinksAndEventSuffix(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	testTapOwnIP(t)
	bash := screen(t, "claude-bash-4.txt")
	p, _ := parsePrompt(bash)
	cfg := Config{Tap: TapConfig{Enabled: true, Devices: []string{"iphone181"}}}
	f := &fakePane{screens: []string{bash}}

	out, err := tapLinksText(cfg, f.env(), "backend-ab12", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out, "http://100.78.70.17:8642/tap/"); got != len(p.Options) {
		t.Fatalf("%d links for %d options:\n%s", got, len(p.Options), out)
	}
	for _, want := range []string{"[" + p.Hash + "]", "mkdir -p out2", "1 Yes — ", "4 No — "} {
		if !strings.Contains(out, want) {
			t.Errorf("links lack %q:\n%s", want, out)
		}
	}
	// Every minted link parses back to its own option of this prompt.
	secret, err := tapSecret()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, "/tap/"); i >= 0 {
			c, err := parseTapToken(secret, strings.TrimSpace(line[i+5:]), time.Now())
			if err != nil || c.Bead != "backend-ab12" || c.Hash != p.Hash {
				t.Errorf("link in %q: claims %+v, err %v", line, c, err)
			}
		}
	}

	// The ticker appends the same links to the prompt's inbox event — and
	// nothing when taps are off or the prompt could not be parsed, which is
	// where muse workers stand today.
	tk := Ticker{cfg: cfg, log: log.New(io.Discard, "", 0)}
	th := Thread{Bead: Bead{ID: "backend-ab12"}, Prompt: &p}
	suffix := tk.tapEventSuffix(th)
	if strings.Count(suffix, "/tap/") != len(p.Options) || !strings.Contains(suffix, "tap to answer as TJ") {
		t.Errorf("event suffix = %q", suffix)
	}
	tk.cfg.Tap.Enabled = false
	if got := tk.tapEventSuffix(th); got != "" {
		t.Errorf("suffix while off = %q", got)
	}
	tk.cfg.Tap.Enabled = true
	th.Prompt = nil
	if got := tk.tapEventSuffix(th); got != "" {
		t.Errorf("suffix without a parsed prompt = %q", got)
	}
}

func TestTapStartServerBindsOnlyTheTailnetAddress(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	testTapOwnIP(t)
	tapOwnIP = func() (string, error) { return "100.64.0.99", nil }
	// 100.64.0.99 is in the tailnet range but not assigned here, so binding
	// must fail rather than fall back to another interface.
	f := &fakePane{screens: []string{screen(t, "claude-bash-4.txt")}}
	if ln, err := startTapServer(TapConfig{Enabled: true, Devices: []string{"iphone181"}}, f.env(), nil); err == nil {
		ln.Close()
		t.Fatal("the server bound an address this machine does not own")
	}
	tapOwnIP = func() (string, error) { return "127.0.0.1", nil }
	ln, err := startTapServer(TapConfig{Enabled: true, Port: 0, Devices: []string{"iphone181"}}, f.env(), nil)
	// Port 0 takes the default port, so this binds 127.0.0.1:8642 in the test
	// only because tapOwnIP is faked; the point is it binds exactly that.
	if err != nil {
		t.Skipf("cannot bind the default tap port here: %v", err)
	}
	defer ln.Close()
	if got := ln.Addr().String(); got != "127.0.0.1:8642" {
		t.Errorf("listener = %s", got)
	}
}
