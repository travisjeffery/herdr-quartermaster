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
)

// Approval taps: TJ answers a worker's approval prompt from his phone by
// tapping a signed, one-time link. The link is bound to one bead, one prompt
// instance (its --expect hash) and one option; the handler runs on this
// machine's Tailscale address only, checks the tapping device with
// `tailscale whois` against [tap] devices, and answers through
// answerPromptVia, so every guard of `quartermaster answer` — the hash
// re-read, the wait for the prompt to clear, the PROMPT ANSWERED note and
// the journal — applies unchanged. A tap is TJ's answer (`--by TJ`); the
// note records the device as the identity evidence. See
// docs/approval-taps.md. Off unless [tap] enabled; only prompts parsePrompt
// recognizes (or a pi worker's ask) can be tapped.

const defaultTapPort = 8642

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

// tapClaims is what a link carries, signed: one prompt instance of one bead,
// one option, an expiry and a nonce spent on first use.
type tapClaims struct {
	Bead   string
	Hash   string
	Choice string // the option's number, as answerPrompt takes it
	Exp    time.Time
	Nonce  string
}

func (c tapClaims) payload() string {
	return strings.Join([]string{c.Bead, c.Hash, c.Choice, strconv.FormatInt(c.Exp.Unix(), 10), c.Nonce}, "|")
}

func tapSign(secret []byte, payload string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func mintTapToken(secret []byte, bead, hash, choice string, now time.Time) (string, tapClaims, error) {
	for _, f := range []string{bead, hash, choice} {
		if f == "" || strings.Contains(f, "|") {
			return "", tapClaims{}, fmt.Errorf("tap link field %q is not usable", f)
		}
	}
	var n [8]byte
	if _, err := rand.Read(n[:]); err != nil {
		return "", tapClaims{}, err
	}
	c := tapClaims{Bead: bead, Hash: hash, Choice: choice, Exp: now.Add(tapTTL), Nonce: hex.EncodeToString(n[:])}
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
	if len(f) != 5 || f[0] == "" || f[1] == "" || f[2] == "" || f[4] == "" {
		return tapClaims{}, errors.New("not a tap link")
	}
	exp, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil {
		return tapClaims{}, errors.New("not a tap link")
	}
	c := tapClaims{Bead: f[0], Hash: f[1], Choice: f[2], Exp: time.Unix(exp, 0), Nonce: f[4]}
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
	env     promptEnv
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
	secret, err := tapSecret()
	if err != nil {
		return nil, err
	}
	return &tapServer{
		cfg: cfg, env: env, secret: secret,
		whois: tailscaleWhois, consume: consumeTapNonce,
		now:  time.Now,
		logf: func(string, ...any) {},
	}, nil
}

func (s *tapServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/tap/", s.handleTap)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		tapPage(w, http.StatusNotFound, "Nothing here", "Approval links arrive with the prompt they answer; there is no index.")
	})
	return mux
}

// refuse journals why a tap changed nothing, then says so. A refusal after
// the signature checked out names its bead; a forged or garbled link has no
// bead to name.
func (s *tapServer) refuse(w http.ResponseWriter, status int, bead, reason string) {
	rec := journalRecord{At: s.now(), Kind: journalTap, Bead: bead, Class: -1, Text: "TAP REFUSED: " + reason}
	if err := appendJournal(rec); err != nil {
		s.logf("tap: journal a refusal: %v", err)
	}
	s.logf("tap refused: %s", reason)
	tapPage(w, status, "Not answered", reason+".")
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
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !isTailnetIP(net.ParseIP(host)) {
		s.refuse(w, http.StatusForbidden, claims.Bead, "the tap did not come over the tailnet")
		return
	}
	ident, err := s.whois(host)
	if err != nil {
		s.refuse(w, http.StatusForbidden, claims.Bead, "the tapping device could not be identified ("+err.Error()+")")
		return
	}
	if !tapDeviceAllowed(s.cfg.Devices, ident.Device) {
		s.refuse(w, http.StatusForbidden, claims.Bead, fmt.Sprintf("device %s is not allowed to tap", ident.Device))
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.confirm(w, claims, ident)
	case http.MethodPost:
		s.answer(w, claims, ident)
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
	data := struct {
		Bead, Summary, Text, Option, Device string
		Number                              int
	}{c.Bead, p.Summary(), p.String(), o.Label, ident.Device, o.Number}
	if err := tapConfirmTmpl.Execute(w, data); err != nil {
		s.logf("tap: confirm page: %v", err)
	}
}

// answer spends the nonce, then hands over to the same guarded path as
// `quartermaster answer ... --by TJ`: it re-reads the prompt, refuses a
// changed one, waits for it to clear, and notes the answer on the bead with
// the tapping device as the identity evidence.
func (s *tapServer) answer(w http.ResponseWriter, c tapClaims, ident tapIdentity) {
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
	msg, err := answerPromptVia(s.env, c.Bead, c.Choice, c.Hash, "TJ", -1, via)
	if err != nil {
		s.refuse(w, http.StatusConflict, c.Bead, "the tap was recorded but the prompt was not answered: "+err.Error())
		return
	}
	s.logf("tap: %s", msg)
	tapPage(w, http.StatusOK, "Answered", msg+".")
}

var tapConfirmTmpl = template.Must(template.New("confirm").Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Answer {{.Bead}}?</title>
<style>body{font-family:system-ui;margin:1.2em}pre{white-space:pre-wrap;background:#f4f4f4;padding:.8em;border-radius:.5em}button{font-size:1.2em;padding:.7em 1.2em;width:100%}</style>
</head><body>
<h1>{{.Bead}}: {{.Summary}}</h1>
<pre>{{.Text}}</pre>
<form method="post"><button type="submit">Answer {{.Number}}. {{.Option}}</button></form>
<p>You are answering as TJ from {{.Device}}. Anything else: close this page.</p>
</body></html>`))

var tapPageTmpl = template.Must(template.New("page").Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>body{font-family:system-ui;margin:1.2em}</style>
</head><body><h1>{{.Title}}</h1><p>{{.Body}}</p></body></html>`))

func tapPage(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = tapPageTmpl.Execute(w, struct{ Title, Body string }{title, body})
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
	var links []tapLink
	for _, o := range p.Options {
		tok, c, err := mintTapToken(secret, bead, p.Hash, strconv.Itoa(o.Number), now)
		if err != nil {
			return nil, err
		}
		links = append(links, tapLink{Option: o, URL: base + "/tap/" + tok, Exp: c.Exp})
	}
	return links, nil
}

// tapLinksText is `quartermaster tap links <bead>`: one signed link per
// option of the prompt the worker is showing now, for the coordinator to
// relay verbatim or TJ to open himself. It mints nothing when taps are off,
// when no parsable prompt is showing, or when this machine has no tailnet
// address for the links to point at.
func tapLinksText(cfg Config, env promptEnv, id string, now time.Time) (string, error) {
	if !cfg.Tap.Enabled {
		return "", errors.New("approval taps are off: set [tap] enabled with devices in config.toml (docs/approval-taps.md)")
	}
	if err := cfg.validateTap(); err != nil {
		return "", err
	}
	p, _, a, err := workerPrompt(env, id)
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

// startTapServer binds the tap endpoint to the tailnet address and serves in
// the background; the listener is returned so the caller can close it.
// Binding happens here, not in the goroutine, so a failure is the caller's
// to log at once.
func startTapServer(cfg TapConfig, env promptEnv, logf func(format string, args ...any)) (net.Listener, error) {
	if !cfg.Enabled {
		return nil, errors.New("approval taps are off")
	}
	ip, err := tapOwnIP()
	if err != nil {
		return nil, err
	}
	s, err := newTapServer(cfg, env)
	if err != nil {
		return nil, err
	}
	if logf != nil {
		s.logf = logf
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(cfg.port())))
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil {
			s.logf("tap: serve: %v", err)
		}
	}()
	s.logf("tap: listening on http://%s (devices %s)", ln.Addr(), strings.Join(cfg.Devices, ", "))
	return ln, nil
}

// tapCmd is `quartermaster tap links|serve`.
func tapCmd(cfg Config, env promptEnv, pos []string) error {
	if len(pos) == 0 {
		return errors.New("usage: quartermaster tap links <bead> | quartermaster tap serve")
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
	case "serve":
		if len(pos) != 1 {
			return errors.New("usage: quartermaster tap serve")
		}
		ln, err := startTapServer(cfg.Tap, env, func(format string, args ...any) {
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
		return fmt.Errorf("unknown tap command %q (links or serve)", pos[0])
	}
}
