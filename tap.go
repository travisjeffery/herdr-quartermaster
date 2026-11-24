package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// Approval taps: TJ answers a worker's approval prompt — or its needs_me
// question (with or without enumerated options, tapquestion.go) — from his
// phone by tapping a signed, one-time link. The link is bound to one bead, one
// prompt or question instance (its --expect hash) and an option (or free-text
// choice for needs_me questions); the handler runs on this machine's Tailscale
// address only, checks the tapping device with `tailscale whois` against
// [tap] devices, and answers through answerPromptVia or answerNeedsMeTextVia,
// so every guard of the guarded paths — the hash re-read, the wait for the
// prompt to clear, the audit note and the journal — applies unchanged. A
// tap is TJ's answer (`--by TJ`); the note records the device as the
// identity evidence. See docs/approval-taps.md. Off unless [tap] enabled;
// only prompts parsePrompt recognizes (or a pi worker's ask) and live
// needs_me questions (never RUN: commands) can be tapped.

const (
	defaultTapPort  = 8642
	tapPostMaxBytes = 64 * 1024
)

// tapTTL bounds a link's life. The prompt hash is the real guard — a link
// for a prompt that has changed or cleared is refused whenever it arrives —
// so the TTL only retires links left lying around in old messages.
const tapTTL = 15 * time.Minute

func tapDir() string        { return filepath.Join(stateDir(), "tap") }
func tapSecretPath() string { return filepath.Join(tapDir(), "secret") }
func tapUsedPath() string   { return filepath.Join(tapDir(), "used.json") }

// tapSecret is the HMAC key for tap links: created on first use, 0600, in
// the state dir. Deleting it revokes every outstanding link.
func tapSecret() ([]byte, error) {
	if b, err := os.ReadFile(tapSecretPath()); err == nil {
		if len(b) < 16 {
			return nil, fmt.Errorf("tap secret at %s is too short; delete it and quartermaster will make a new one", tapSecretPath())
		}
		return b, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := rpcPrivateDir(tapDir()); err != nil {
		return nil, err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	// O_EXCL: two first runs racing must not leave different keys behind.
	f, err := os.OpenFile(tapSecretPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ReadFile(tapSecretPath())
		}
		return nil, err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return nil, err
	}
	return b, f.Close()
}

// What a tap answers: a worker's approval prompt, or its needs_me question.
const (
	tapKindPrompt   = "p"
	tapKindQuestion = "q"
)

// tapClaims is what a link carries, signed: one prompt or question instance
// of one bead, one option, an expiry and a nonce spent on first use.
type tapClaims struct {
	Kind   string // tapKindPrompt or tapKindQuestion
	Bead   string
	Hash   string
	Choice string // the option's number, as answerPrompt takes it
	Exp    time.Time
	Nonce  string
}

func (c tapClaims) payload() string {
	return strings.Join([]string{c.Kind, c.Bead, c.Hash, c.Choice, strconv.FormatInt(c.Exp.Unix(), 10), c.Nonce}, "|")
}

func tapSign(secret []byte, payload string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func mintTapToken(secret []byte, bead, hash, choice string, now time.Time) (string, tapClaims, error) {
	return mintTapTokenKind(secret, tapKindPrompt, bead, hash, choice, now)
}

func mintTapTokenKind(secret []byte, kind, bead, hash, choice string, now time.Time) (string, tapClaims, error) {
	if kind != tapKindPrompt && kind != tapKindQuestion {
		return "", tapClaims{}, fmt.Errorf("tap link kind %q is not usable", kind)
	}
	if kind == tapKindPrompt && choice == tapChoiceFreeText {
		return "", tapClaims{}, errors.New("approval prompt tap links do not allow free text")
	}
	for _, f := range []string{bead, hash, choice} {
		if f == "" || strings.Contains(f, "|") {
			return "", tapClaims{}, fmt.Errorf("tap link field %q is not usable", f)
		}
	}
	var n [8]byte
	if _, err := rand.Read(n[:]); err != nil {
		return "", tapClaims{}, err
	}
	c := tapClaims{Kind: kind, Bead: bead, Hash: hash, Choice: choice, Exp: now.Add(tapTTL), Nonce: hex.EncodeToString(n[:])}
	p := c.payload()
	return base64.RawURLEncoding.EncodeToString([]byte(p)) + "." + tapSign(secret, p), c, nil
}

var errTapExpired = errors.New("this tap link has expired")

func parseTapToken(secret []byte, token string, now time.Time) (tapClaims, error) {
	enc, sig, ok := strings.Cut(token, ".")
	if !ok {
		return tapClaims{}, errors.New("not a tap link")
	}
	p, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return tapClaims{}, errors.New("not a tap link")
	}
	if !hmac.Equal([]byte(sig), []byte(tapSign(secret, string(p)))) {
		return tapClaims{}, errors.New("this tap link's signature does not check out")
	}
	f := strings.Split(string(p), "|")
	// Links minted before question taps carry no kind: five fields, a prompt.
	if len(f) == 5 {
		f = append([]string{tapKindPrompt}, f...)
	}
	if len(f) != 6 || (f[0] != tapKindPrompt && f[0] != tapKindQuestion) || f[1] == "" || f[2] == "" || f[3] == "" || f[5] == "" {
		return tapClaims{}, errors.New("not a tap link")
	}
	if f[0] == tapKindPrompt && f[3] == tapChoiceFreeText {
		return tapClaims{}, errors.New("not a tap link")
	}
	exp, err := strconv.ParseInt(f[4], 10, 64)
	if err != nil {
		return tapClaims{}, errors.New("not a tap link")
	}
	c := tapClaims{Kind: f[0], Bead: f[1], Hash: f[2], Choice: f[3], Exp: time.Unix(exp, 0), Nonce: f[5]}
	if now.After(c.Exp) {
		return c, errTapExpired
	}
	return c, nil
}

// consumeTapNonce spends nonce until exp, reporting whether it was already
// spent. Spent nonces are kept only until their link would have expired:
// past that the expiry check refuses the replay on its own.
func consumeTapNonce(nonce string, exp time.Time, now time.Time) (bool, error) {
	if err := rpcPrivateDir(tapDir()); err != nil {
		return false, err
	}
	lock, err := os.OpenFile(filepath.Join(tapDir(), "lease"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return false, err
	}
	used := map[string]int64{}
	if err := rpcLoad(tapUsedPath(), &used); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	for n, e := range used {
		if now.Unix() > e {
			delete(used, n)
		}
	}
	if _, seen := used[nonce]; seen {
		return true, nil
	}
	used[nonce] = exp.Unix()
	if err := rpcSave(tapUsedPath(), used); err != nil {
		return false, err
	}
	return false, nil
}

// tapIdentity is who a tap came from, by Tailscale: the node's name and its
// owner's login.
type tapIdentity struct {
	Device string
	User   string
}

func tailscaleWhois(ip string) (tapIdentity, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", "whois", "--json", ip).Output()
	if err != nil {
		return tapIdentity{}, fmt.Errorf("tailscale whois %s: %w", ip, err)
	}
	var w struct {
		Node struct {
			Name string `json:"Name"`
		} `json:"Node"`
		UserProfile struct {
			LoginName string `json:"LoginName"`
		} `json:"UserProfile"`
	}
	if err := json.Unmarshal(out, &w); err != nil {
		return tapIdentity{}, fmt.Errorf("tailscale whois %s: %w", ip, err)
	}
	if w.Node.Name == "" {
		return tapIdentity{}, fmt.Errorf("tailscale whois %s: no node", ip)
	}
	return tapIdentity{Device: nodeShortName(w.Node.Name), User: w.UserProfile.LoginName}, nil
}

// nodeShortName is a Tailscale node name without its tailnet domain:
// "iphone181.shire.example.ts.net." is "iphone181".
func nodeShortName(name string) string {
	name = strings.TrimSuffix(name, ".")
	short, _, _ := strings.Cut(name, ".")
	return short
}

func tapDeviceAllowed(devices []string, device string) bool {
	for _, d := range devices {
		if strings.EqualFold(nodeShortName(d), device) {
			return true
		}
	}
	return false
}

var (
	tailnetV4 = net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
	tailnetV6 = net.IPNet{IP: net.ParseIP("fd7a:115c:a1e0::"), Mask: net.CIDRMask(48, 128)}
)

func isTailnetIP(ip net.IP) bool {
	return ip != nil && (tailnetV4.Contains(ip) || tailnetV6.Contains(ip))
}

// tapOwnIP resolves this machine's own Tailscale address; tests replace it.
var tapOwnIP = tailscaleIPv4

// tailscaleIPv4 is this machine's own Tailscale address, the only one the
// tap endpoint listens on. No tailnet, no listener: it fails closed rather
// than falling back to every interface.
func tailscaleIPv4() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", "ip", "-4").Output()
	if err != nil {
		return "", fmt.Errorf("tailscale ip -4: %w", err)
	}
	s := strings.TrimSpace(string(out))
	s, _, _ = strings.Cut(s, "\n")
	if ip := net.ParseIP(s); ip == nil || !isTailnetIP(ip) {
		return "", fmt.Errorf("tailscale ip -4 said %q, not a tailnet address", s)
	}
	return s, nil
}

type tapServer struct {
	cfg     TapConfig
	fullCfg Config
	env     promptEnv
	inbox   *tapInboxEnv
	base    string // "http://<tailnet-ip>:<port>" when known from listener bind
	secret  []byte
	whois   func(ip string) (tapIdentity, error)
	consume func(nonce string, exp time.Time, now time.Time) (bool, error)
	now     func() time.Time
	logf    func(format string, args ...any)
	// mu serializes answers: two links for one prompt are both live until the
	// prompt clears, and without it two near-simultaneous taps could both
	// read the prompt before either answers it.
	mu sync.Mutex
}

func newTapServer(cfg TapConfig, env promptEnv) (*tapServer, error) {
	return newTapServerWithConfig(Config{Tap: cfg}, env, nil)
}

func newTapServerWithConfig(cfg Config, env promptEnv, inbox *tapInboxEnv) (*tapServer, error) {
	secret, err := tapSecret()
	if err != nil {
		return nil, err
	}
	return &tapServer{
		cfg: cfg.Tap, fullCfg: cfg, env: env, inbox: inbox, secret: secret,
		whois: tailscaleWhois, consume: consumeTapNonce,
		now:  time.Now,
		logf: func(string, ...any) {},
	}, nil
}

func (s *tapServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/tap/", s.handleTap)
	mux.HandleFunc("/tap-links", s.handleTapLinks)
	mux.HandleFunc("/tap-links/", s.handleTapLinks)
	mux.HandleFunc("/inbox", s.handleInbox)
	mux.HandleFunc("/inbox.json", s.handleInboxJSON)
	mux.HandleFunc("/inbox/plan/", s.handleInboxPlan)
	mux.HandleFunc("/", s.handleRoot)
	return mux
}

// authenticate checks the fail-closed Tailnet identity boundary shared by
// every tap and Web Inbox route: tailnet source IP, successful `tailscale
// whois`, and explicit membership in [tap] devices.
func (s *tapServer) authenticate(r *http.Request) (tapIdentity, string, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !isTailnetIP(net.ParseIP(host)) {
		return tapIdentity{}, "the tap did not come over the tailnet", false
	}
	ident, err := s.whois(host)
	if err != nil {
		return tapIdentity{}, "the tapping device could not be identified (" + err.Error() + ")", false
	}
	if !tapDeviceAllowed(s.cfg.Devices, ident.Device) {
		return tapIdentity{}, fmt.Sprintf("device %s is not allowed to tap", ident.Device), false
	}
	return ident, "", true
}

func (s *tapServer) journalRefusal(bead, reason string) {
	rec := journalRecord{At: s.now(), Kind: journalTap, Bead: bead, Class: -1, Text: "TAP REFUSED: " + reason}
	if err := appendJournal(rec); err != nil {
		s.logf("tap: journal a refusal: %v", err)
	}
	s.logf("tap refused: %s", reason)
}

// refuse journals why a tap changed nothing, then says so. A refusal after
// the signature checked out names its bead; a forged or garbled link has no
// bead to name.
func (s *tapServer) refuse(w http.ResponseWriter, status int, bead, reason string) {
	s.journalRefusal(bead, reason)
	tapPage(w, status, "Not answered", reason+".")
}

func (s *tapServer) refuseJSON(w http.ResponseWriter, status int, bead, reason string) {
	s.journalRefusal(bead, reason)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: reason})
}

// requireAuth runs authenticate and writes + journals a 403 refusal on failure.
func (s *tapServer) requireAuth(w http.ResponseWriter, r *http.Request, bead string, jsonResp bool) (tapIdentity, bool) {
	ident, reason, ok := s.authenticate(r)
	if !ok {
		if jsonResp {
			s.refuseJSON(w, http.StatusForbidden, bead, reason)
		} else {
			s.refuse(w, http.StatusForbidden, bead, reason)
		}
		return tapIdentity{}, false
	}
	return ident, true
}

func (s *tapServer) handleTap(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/tap/")
	claims, err := parseTapToken(s.secret, token, s.now())
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errTapExpired) {
			status = http.StatusGone
		}
		s.refuse(w, status, claims.Bead, err.Error())
		return
	}
	ident, ok := s.requireAuth(w, r, claims.Bead, false)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		if claims.Kind == tapKindQuestion {
			s.confirmQuestion(w, claims, ident)
		} else {
			s.confirm(w, claims, ident)
		}
	case http.MethodPost:
		s.answer(w, r, claims, ident)
	default:
		s.refuse(w, http.StatusMethodNotAllowed, claims.Bead, "taps are GET to review, POST to answer")
	}
}

// confirm shows the prompt as it stands now, re-read from the worker, with a
// button that answers it. A GET never spends the nonce or presses a key, so
// a link unfurled or prefetched by a chat client answers nothing, and a
// stale link is refused here before TJ can confirm it.
func (s *tapServer) confirm(w http.ResponseWriter, c tapClaims, ident tapIdentity) {
	p, _, _, err := workerPrompt(s.env, c.Bead)
	if err != nil {
		s.refuse(w, http.StatusConflict, c.Bead, "the prompt is gone: "+err.Error())
		return
	}
	if p.Hash != c.Hash {
		s.refuse(w, http.StatusConflict, c.Bead, fmt.Sprintf("the prompt changed: the worker now shows [%s] %s, not [%s]", p.Hash, p.Summary(), c.Hash))
		return
	}
	o, err := p.choose(c.Choice)
	if err != nil {
		s.refuse(w, http.StatusConflict, c.Bead, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	data := struct {
		Bead, Summary, Text, Option, Device string
		Number                              int
	}{c.Bead, p.Summary(), p.String(), o.Label, ident.Device, o.Number}
	if err := tapConfirmTmpl.Execute(w, data); err != nil {
		s.logf("tap: confirm page: %v", err)
	}
}

// confirmQuestion shows the bead's question as it stands now, re-read from
// its notes, with a free-text field and option button(s) when the question has
// options. Like a prompt's confirm page, a GET spends nothing, and a stale
// link — the bead answered, the question changed, the worker working again —
// is refused here first.
func (s *tapServer) confirmQuestion(w http.ResponseWriter, c tapClaims, ident tapIdentity) {
	b, q, err := beadQuestion(s.env, c.Bead)
	if err != nil {
		s.refuse(w, http.StatusConflict, c.Bead, err.Error())
		return
	}
	if q.Hash != c.Hash {
		s.refuse(w, http.StatusConflict, c.Bead, fmt.Sprintf("the question changed: the bead now asks [%s] %s, not [%s]", q.Hash, q.Summary(), c.Hash))
		return
	}
	if _, a := questionWorker(s.env, b); a != nil && a.Status == "working" {
		s.refuse(w, http.StatusConflict, c.Bead, errWorkerWorking.Error())
		return
	}
	type questionConfirmData struct {
		Bead         string
		Summary      string
		Text         string
		Device       string
		MaxChars     int
		FreeTextLink bool
		Number       int
		Option       string
		Options      []PromptOption
	}
	data := questionConfirmData{
		Bead:         c.Bead,
		Summary:      q.Summary(),
		Text:         q.String(),
		Device:       ident.Device,
		MaxChars:     tapMaxFreeTextRunes,
		FreeTextLink: c.Choice == tapChoiceFreeText,
		Options:      q.Options,
	}
	if !data.FreeTextLink {
		o, err := q.choose(c.Choice)
		if err != nil {
			s.refuse(w, http.StatusConflict, c.Bead, err.Error())
			return
		}
		data.Number = o.Number
		data.Option = o.Label
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := tapQuestionConfirmTmpl.Execute(w, data); err != nil {
		s.logf("tap: confirm page: %v", err)
	}
}

// answer validates form input before spending the nonce, spends the nonce
// atomically, then hands over to the same guarded paths as
// `quartermaster answer ... --by TJ` for a prompt and a coordinator's
// relayed answer for a question: each re-reads its target, refuses a
// changed one, and notes the answer on the bead with the tapping device as
// the identity evidence.
func (s *tapServer) answer(w http.ResponseWriter, r *http.Request, c tapClaims, ident tapIdentity) {
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, tapPostMaxBytes)
	}
	if err := r.ParseForm(); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.refuse(w, http.StatusBadRequest, c.Bead, fmt.Sprintf("free-text answer exceeds the %d-character limit (this link was not spent — shorten it and submit again)", tapMaxFreeTextRunes))
			return
		}
		s.refuse(w, http.StatusBadRequest, c.Bead, "could not read tap form: "+err.Error())
		return
	}
	text := normalizeTapFreeText(r.PostFormValue("text"))
	formChoice := strings.TrimSpace(r.PostFormValue("choice"))
	choice := c.Choice

	if c.Kind == tapKindPrompt {
		if strings.TrimSpace(text) != "" {
			s.refuse(w, http.StatusBadRequest, c.Bead, "approval prompts do not accept free text")
			return
		}
		if formChoice != "" && formChoice != c.Choice {
			s.refuse(w, http.StatusBadRequest, c.Bead, fmt.Sprintf("this tap link is for option %s, not %s", c.Choice, formChoice))
			return
		}
	} else {
		if n := utf8.RuneCountInString(text); n > tapMaxFreeTextRunes {
			s.refuse(w, http.StatusBadRequest, c.Bead, fmt.Sprintf("free-text answer is %d characters, over the %d-character limit (this link was not spent — edit and submit again)", n, tapMaxFreeTextRunes))
			return
		}
		if c.Choice == tapChoiceFreeText {
			if formChoice == "" || formChoice == tapChoiceFreeText {
				choice = tapChoiceFreeText
			} else {
				n, err := strconv.Atoi(formChoice)
				if err != nil || n < 1 || n > maxQuestionOptions {
					s.refuse(w, http.StatusBadRequest, c.Bead, fmt.Sprintf("invalid option %q (this link was not spent)", formChoice))
					return
				}
				choice = formChoice
			}
		} else {
			switch {
			case formChoice == "" || formChoice == c.Choice:
				choice = c.Choice
			case formChoice == tapChoiceFreeText:
				choice = tapChoiceFreeText
			default:
				s.refuse(w, http.StatusBadRequest, c.Bead, fmt.Sprintf("this tap link is for option %s, not %s (this link was not spent)", c.Choice, formChoice))
				return
			}
		}
		if choice == tapChoiceFreeText && strings.TrimSpace(text) == "" {
			s.refuse(w, http.StatusBadRequest, c.Bead, "free-text answer cannot be empty (this link was not spent)")
			return
		}
		if choice != tapChoiceFreeText && strings.TrimSpace(text) == "" {
			text = ""
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	replay, err := s.consume(c.Nonce, c.Exp, s.now())
	if err != nil {
		s.refuse(w, http.StatusInternalServerError, c.Bead, "the tap could not be recorded: "+err.Error())
		return
	}
	if replay {
		s.refuse(w, http.StatusConflict, c.Bead, "this tap link was already used")
		return
	}
	via := "tap (device " + ident.Device + ")"
	var msg string
	if c.Kind == tapKindQuestion {
		msg, err = answerNeedsMeTextVia(s.env, c.Bead, choice, text, c.Hash, via)
		if err != nil {
			s.refuse(w, http.StatusConflict, c.Bead, "the tap was recorded but the question was not answered: "+err.Error())
			return
		}
	} else {
		msg, err = answerPromptVia(s.env, c.Bead, c.Choice, c.Hash, "TJ", -1, via)
		if err != nil {
			s.refuse(w, http.StatusConflict, c.Bead, "the tap was recorded but the prompt was not answered: "+err.Error())
			return
		}
	}
	s.logf("tap: %s", msg)
	tapPage(w, http.StatusOK, "Answered", msg+".")
}

var tapConfirmTmpl = template.Must(template.New("confirm").Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Answer {{.Bead}}?</title>
<style>body{font-family:system-ui;margin:1.2em;max-width:44rem}pre{white-space:pre-wrap;word-break:break-word;background:#f4f4f4;padding:.8em;border-radius:.5em}button{font-size:1.15em;padding:.75em 1.2em;width:100%;cursor:pointer}.nav{margin-top:1em;font-size:.95em}@media (prefers-color-scheme:dark){body{background:#111418;color:#e6edf3}pre{background:#1b2129;color:#e6edf3}a{color:#79c0ff}}</style>
</head><body>
<h1>{{.Bead}}: {{.Summary}}</h1>
<pre>{{.Text}}</pre>
<form method="post"><button type="submit">Answer {{.Number}}. {{.Option}}</button></form>
<p>You are answering as TJ from {{.Device}}. Anything else: close this page.</p>
<p class="nav"><a href="/inbox">Back to Inbox</a></p>
</body></html>`))

var tapQuestionConfirmTmpl = template.Must(template.New("question-confirm").Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Answer {{.Bead}}?</title>
<style>
*{box-sizing:border-box}
body{font-family:system-ui,-apple-system,sans-serif;margin:1.2em auto;max-width:44rem;padding:0 12px;line-height:1.45}
pre{white-space:pre-wrap;word-break:break-word;background:#f4f4f4;padding:.8em;border-radius:.5em}
label{display:block;font-weight:600;margin:.9em 0 .35em}
textarea{width:100%;font-family:inherit;font-size:1rem;padding:.65em;border:1px solid #c9d1d9;border-radius:.5em;resize:vertical}
.hint{font-size:.85em;color:#59636e;margin:.3em 0 .85em;display:flex;justify-content:space-between;gap:8px;flex-wrap:wrap}
.btns{display:flex;flex-direction:column;gap:.55em;margin-top:.6em}
button{font-size:1.05em;font-weight:600;padding:.75em 1.1em;width:100%;min-height:46px;border-radius:.55em;border:1px solid #c9d1d9;background:#f4f4f4;color:inherit;cursor:pointer;text-align:center}
button.primary{background:#0969da;border-color:#0969da;color:#fff}
.nav{margin-top:1em;font-size:.95em}
@media (prefers-color-scheme:dark){
  body{background:#111418;color:#e6edf3}
  pre{background:#1b2129;color:#e6edf3}
  textarea{background:#0d1117;color:#e6edf3;border-color:#30363d}
  .hint{color:#8b949e}
  button{background:#21262d;border-color:#30363d;color:#e6edf3}
  button.primary{background:#1f6feb;border-color:#1f6feb;color:#fff}
  a{color:#79c0ff}
}
</style>
</head><body>
<h1>{{.Bead}}: {{.Summary}}</h1>
<pre>{{.Text}}</pre>
<form method="post">
  {{if .FreeTextLink}}
    {{if gt (len .Options) 0}}
    <label for="tap-text">Free-text answer or note (optional when picking an option)</label>
    <textarea id="tap-text" name="text" rows="5" maxlength="{{.MaxChars}}" placeholder="Write your answer verbatim, or add detail for an option below (up to {{.MaxChars}} characters)…"></textarea>
    <div class="hint"><span>Recorded verbatim. Send text alone, pick an option alone, or pick an option with text.</span><span id="tap-count">0 / {{.MaxChars}}</span></div>
    <div class="btns">
      <button type="submit" class="primary" name="choice" value="text">Send free-text answer</button>
      {{range .Options}}
      <button type="submit" name="choice" value="{{.Number}}">Answer {{.Number}}. {{.Label}}</button>
      {{end}}
    </div>
    {{else}}
    <label for="tap-text">Your answer</label>
    <textarea id="tap-text" name="text" rows="5" maxlength="{{.MaxChars}}" required placeholder="Write your answer verbatim (up to {{.MaxChars}} characters)…"></textarea>
    <div class="hint"><span>Recorded verbatim (up to {{.MaxChars}} characters).</span><span id="tap-count">0 / {{.MaxChars}}</span></div>
    <div class="btns">
      <button type="submit" class="primary">Send free-text answer</button>
    </div>
    {{end}}
  {{else}}
    <label for="tap-text">Optional free-text note (recorded verbatim)</label>
    <textarea id="tap-text" name="text" rows="4" maxlength="{{.MaxChars}}" placeholder="Add details alongside option {{.Number}}, or send free text only (up to {{.MaxChars}} characters)…"></textarea>
    <div class="hint"><span>Up to {{.MaxChars}} characters. Leave blank to answer option {{.Number}} alone.</span><span id="tap-count">0 / {{.MaxChars}}</span></div>
    <div class="btns">
      <button type="submit" class="primary">Answer {{.Number}}. {{.Option}}</button>
      <button type="submit" name="choice" value="text">Send free-text answer only</button>
    </div>
  {{end}}
</form>
<p>You are answering as TJ from {{.Device}}. Anything else: close this page.</p>
<p class="nav"><a href="/inbox">Back to Inbox</a></p>
<script>(function(){var ta=document.getElementById('tap-text'),c=document.getElementById('tap-count');if(ta&&c){var max={{.MaxChars}};function u(){var n=Array.from(ta.value.replace(/\r\n/g,'\n')).length;c.textContent=n+' / '+max;}ta.addEventListener('input',u);u();}})();</script>
</body></html>`))

var tapPageTmpl = template.Must(template.New("page").Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>body{font-family:system-ui;margin:1.2em;max-width:44rem}@media (prefers-color-scheme:dark){body{background:#111418;color:#e6edf3}a{color:#79c0ff}}</style>
</head><body><h1>{{.Title}}</h1><p>{{.Body}}</p>{{if .ShowInbox}}<p><a href="/inbox">Back to Inbox</a></p>{{end}}</body></html>`))

func tapPage(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = tapPageTmpl.Execute(w, struct {
		Title, Body string
		ShowInbox   bool
	}{Title: title, Body: body, ShowInbox: status != http.StatusForbidden})
}

// tapBaseURL is where tap links point: this machine's tailnet address. It is
// unresolved when tailscale is down, and links are then not minted at all.
func tapBaseURL(cfg TapConfig) (string, error) {
	ip, err := tapOwnIP()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("http://%s:%d", ip, cfg.port()), nil
}

type tapLink struct {
	Option PromptOption
	URL    string
	Exp    time.Time
}

func mintTapLinks(secret []byte, base, bead string, p ApprovalPrompt, now time.Time) ([]tapLink, error) {
	return mintTapLinksFor(secret, base, tapKindPrompt, bead, p.Hash, p.Options, now)
}

func mintQuestionTapLinks(secret []byte, base, bead string, q NeedsMeQuestion, now time.Time) ([]tapLink, error) {
	return mintTapLinksFor(secret, base, tapKindQuestion, bead, q.Hash, q.Options, now)
}

func mintQuestionFreeTextTapLink(secret []byte, base, bead string, q NeedsMeQuestion, now time.Time) (tapLink, error) {
	tok, c, err := mintTapTokenKind(secret, tapKindQuestion, bead, q.Hash, tapChoiceFreeText, now)
	if err != nil {
		return tapLink{}, err
	}
	return tapLink{Option: PromptOption{Label: "Free text"}, URL: base + "/tap/" + tok, Exp: c.Exp}, nil
}

func mintTapLinksFor(secret []byte, base, kind, bead, hash string, opts []PromptOption, now time.Time) ([]tapLink, error) {
	var links []tapLink
	for _, o := range opts {
		tok, c, err := mintTapTokenKind(secret, kind, bead, hash, strconv.Itoa(o.Number), now)
		if err != nil {
			return nil, err
		}
		links = append(links, tapLink{Option: o, URL: base + "/tap/" + tok, Exp: c.Exp})
	}
	return links, nil
}

// tapLinksText is `quartermaster tap links <bead>`: one signed link per
// option of the prompt the worker is showing now — or, when it shows none,
// of the OPTIONS question its needs_me bead ends with — for the coordinator
// to relay verbatim or TJ to open himself. It mints nothing when taps are
// off, when neither is waiting, or when this machine has no tailnet address
// for the links to point at.
func tapLinksText(cfg Config, env promptEnv, id string, now time.Time) (string, error) {
	if !cfg.Tap.Enabled {
		return "", errors.New("approval taps are off: set [tap] enabled with devices in config.toml (docs/approval-taps.md)")
	}
	if err := cfg.validateTap(); err != nil {
		return "", err
	}
	p, _, a, perr := workerPrompt(env, id)
	if perr != nil {
		// No prompt waiting: a needs_me bead's OPTIONS question is the
		// other thing a tap can answer. Without one the prompt error is
		// the answer, exactly as before questions could be tapped.
		if _, _, qerr := beadQuestion(env, id); qerr != nil {
			return "", perr
		}
		return questionLinksText(cfg, env, id, now)
	}
	secret, err := tapSecret()
	if err != nil {
		return "", err
	}
	base, err := tapBaseURL(cfg.Tap)
	if err != nil {
		return "", err
	}
	links, err := mintTapLinks(secret, base, id, p, now)
	if err != nil {
		return "", err
	}
	var s strings.Builder
	fmt.Fprintf(&s, "%s (%s) is at a prompt [%s]\n%s", a.Name, a.Status, p.Hash, p.String())
	fmt.Fprintf(&s, "tap to answer as TJ (one use each, expires %s):\n", links[0].Exp.Format("15:04"))
	for _, l := range links {
		fmt.Fprintf(&s, "%d %s — %s\n", l.Option.Number, l.Option.Label, l.URL)
	}
	return s.String(), nil
}

// questionLinksText is tapLinksText for a bead's needs_me question (with or
// without an OPTIONS block).
func questionLinksText(cfg Config, env promptEnv, id string, now time.Time) (string, error) {
	_, q, err := beadQuestion(env, id)
	if err != nil {
		return "", err
	}
	secret, err := tapSecret()
	if err != nil {
		return "", err
	}
	base, err := tapBaseURL(cfg.Tap)
	if err != nil {
		return "", err
	}
	links, err := mintQuestionTapLinks(secret, base, id, q, now)
	if err != nil {
		return "", err
	}
	ft, err := mintQuestionFreeTextTapLink(secret, base, id, q, now)
	if err != nil {
		return "", err
	}
	exp := ft.Exp
	if len(links) > 0 {
		exp = links[0].Exp
	}
	var s strings.Builder
	fmt.Fprintf(&s, "%s is marked needs_me [%s]\n%s", id, q.Hash, q.String())
	fmt.Fprintf(&s, "tap to answer as TJ (one use each, expires %s):\n", exp.Format("15:04"))
	for _, l := range links {
		fmt.Fprintf(&s, "%d %s — %s\n", l.Option.Number, l.Option.Label, l.URL)
	}
	fmt.Fprintf(&s, "free text (up to %d chars) — %s\n", tapMaxFreeTextRunes, ft.URL)
	return s.String(), nil
}

// tapEventSuffix is the tap links appended to a thread's approval-prompt
// inbox event, "": taps off, no parsable prompt, or links that could not be
// minted (logged, never a tick failure — the event itself still lands).
func (t Ticker) tapEventSuffix(th Thread) string {
	if !t.cfg.Tap.Enabled || th.Prompt == nil {
		return ""
	}
	secret, err := tapSecret()
	if err != nil {
		t.log.Printf("tap: %v", err)
		return ""
	}
	base, err := tapBaseURL(t.cfg.Tap)
	if err != nil {
		t.log.Printf("tap: %v", err)
		return ""
	}
	links, err := mintTapLinks(secret, base, th.Bead.ID, *th.Prompt, time.Now())
	if err != nil {
		t.log.Printf("tap: %v", err)
		return ""
	}
	parts := make([]string, 0, len(links))
	for _, l := range links {
		parts = append(parts, fmt.Sprintf("%d %s %s", l.Option.Number, shortTitle(l.Option.Label, 50), l.URL))
	}
	// The event's summary is the agent's description of its command; carry
	// the prompt's own lines too, so what reaches the phone shows the actual
	// command, not only its description. The tap's confirm page re-reads and
	// shows them again before anything is answered.
	actual := shortTitle(strings.Join(th.Prompt.Lines, " / "), 300)
	return "; prompt: " + actual + "; tap to answer as TJ (one use each, expires " + links[0].Exp.Format("15:04") + "): " + strings.Join(parts, " · ")
}

// tapQuestionEventSuffix is tapEventSuffix for a needs_me thread's question
// (with or without an OPTIONS block): the question and signed tap links ride
// its needs_you inbox event, so what the coordinator relays is tappable as it
// stands. "" for RUN: beads, answered threads, or empty notes.
func (t Ticker) tapQuestionEventSuffix(th Thread) string {
	if !t.cfg.Tap.Enabled {
		return ""
	}
	q, ok := liveTapQuestion(th)
	if !ok {
		return ""
	}
	secret, err := tapSecret()
	if err != nil {
		t.log.Printf("tap: %v", err)
		return ""
	}
	base, err := tapBaseURL(t.cfg.Tap)
	if err != nil {
		t.log.Printf("tap: %v", err)
		return ""
	}
	now := time.Now()
	links, err := mintQuestionTapLinks(secret, base, th.Bead.ID, q, now)
	if err != nil {
		t.log.Printf("tap: %v", err)
		return ""
	}
	ft, err := mintQuestionFreeTextTapLink(secret, base, th.Bead.ID, q, now)
	if err != nil {
		t.log.Printf("tap: %v", err)
		return ""
	}
	parts := make([]string, 0, len(links)+1)
	for _, l := range links {
		parts = append(parts, fmt.Sprintf("%d %s %s", l.Option.Number, shortTitle(l.Option.Label, 50), l.URL))
	}
	parts = append(parts, "free text "+ft.URL)
	exp := ft.Exp
	if len(links) > 0 {
		exp = links[0].Exp
	}
	question := shortTitle(strings.Join(q.Lines, " / "), 300)
	return "; question: " + question + "; tap to answer as TJ (one use each, expires " + exp.Format("15:04") + "): " + strings.Join(parts, " · ")
}

// startTapServer binds the tap endpoint to the tailnet address and serves in
// the background; the listener is returned so the caller can close it.
// Binding happens here, not in the goroutine, so a failure is the caller's
// to log at once.
func startTapServer(cfg TapConfig, env promptEnv, logf func(format string, args ...any)) (net.Listener, error) {
	return startTapServerWithConfig(Config{Tap: cfg}, env, nil, logf)
}

func startTapServerWithConfig(cfg Config, env promptEnv, inbox *tapInboxEnv, logf func(format string, args ...any)) (net.Listener, error) {
	if !cfg.Tap.Enabled {
		return nil, errors.New("approval taps are off")
	}
	if err := cfg.validateTap(); err != nil {
		return nil, err
	}
	ip, err := tapOwnIP()
	if err != nil {
		return nil, err
	}
	s, err := newTapServerWithConfig(cfg, env, inbox)
	if err != nil {
		return nil, err
	}
	s.base = fmt.Sprintf("http://%s:%d", ip, cfg.Tap.port())
	if logf != nil {
		s.logf = logf
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(cfg.Tap.port())))
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil {
			s.logf("tap: serve: %v", err)
		}
	}()
	s.logf("tap: listening on http://%s (devices %s)", ln.Addr(), strings.Join(cfg.Tap.Devices, ", "))
	return ln, nil
}

// tapInboxURL returns the bookmarkable Tailnet Web Inbox URL when [tap] is
// enabled, valid, and this machine's Tailscale IPv4 address resolves.
func tapInboxURL(cfg Config) (string, error) {
	if !cfg.Tap.Enabled {
		return "", errors.New("approval taps are off: set [tap] enabled with devices in config.toml (docs/approval-taps.md)")
	}
	if err := cfg.validateTap(); err != nil {
		return "", err
	}
	base, err := tapBaseURL(cfg.Tap)
	if err != nil {
		return "", err
	}
	return base + "/inbox", nil
}

// tapCmd is `quartermaster tap links|url|serve`.
func tapCmd(cfg Config, env promptEnv, pos []string) error {
	if len(pos) == 0 {
		return errors.New("usage: quartermaster tap links <bead> | quartermaster tap url | quartermaster tap serve")
	}
	switch pos[0] {
	case "links":
		if len(pos) != 2 {
			return errors.New("usage: quartermaster tap links <bead>")
		}
		out, err := tapLinksText(cfg, env, pos[1], time.Now())
		if err != nil {
			return err
		}
		fmt.Print(out)
		return nil
	case "url":
		if len(pos) != 1 {
			return errors.New("usage: quartermaster tap url")
		}
		u, err := tapInboxURL(cfg)
		if err != nil {
			return err
		}
		fmt.Println(u)
		return nil
	case "serve":
		if len(pos) != 1 {
			return errors.New("usage: quartermaster tap serve")
		}
		ln, err := startTapServerWithConfig(cfg, env, liveTapInboxEnv(cfg, newHerdr().on(cfg.coordSocket())), func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "quartermaster: "+format+"\n", args...)
		})
		if err != nil {
			return err
		}
		defer ln.Close()
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig // the ticker runs this itself when [tap] is on; serve is for a staged run
		return nil
	default:
		return fmt.Errorf("unknown tap command %q (links, url or serve)", pos[0])
	}
}
