package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The control channel to a local pi worker: quartermaster's pi extension
// (pi/quartermaster.ts) listens on a unix socket quartermaster names with
// --quartermaster-control, one per worker, named for its bead. quartermaster
// is always the client. It sends a prompt there instead of typing it into the
// pane, and reads and answers the question the extension's guard is waiting
// on (an "ask"), the way it reads and answers a Claude Code approval prompt
// on screen.
//
// Each request is one line of JSON carrying the token the extension wrote
// beside the socket (<socket>.token); the reply is one line of JSON. The
// socket and token are mode 600 in a 700 directory. That keeps out other
// users and stale clients. It is not a boundary against the worker itself,
// which runs as the same user, as the pane is not: an answer still passes the
// same autonomy check as answering a Claude prompt.
//
// With no socket, or one that doesn't answer, quartermaster falls back to the
// pane, and the extension's guards block as they always did.

// piControlDir holds the sockets, their tokens and config.json.
func piControlDir() string { return filepath.Join(stateDir(), "pi", "control") }

// piControlSocket is the socket of the worker on a bead's agent name.
func piControlSocket(name string) string { return filepath.Join(piControlDir(), name+".sock") }

// piControlTimeout bounds one request: the extension answers from memory.
const piControlTimeout = 3 * time.Second

// piControl is a client for one worker's socket.
type piControl struct {
	sock, token string
}

// piControlFor is the control channel of the local pi worker named name, if
// its extension is listening: the token is written once the socket is.
func piControlFor(name string) (piControl, bool) {
	sock := piControlSocket(name)
	tok, err := os.ReadFile(sock + ".token")
	if err != nil || len(tok) == 0 {
		return piControl{}, false
	}
	if fi, err := os.Stat(sock); err != nil || fi.Mode()&os.ModeSocket == 0 {
		return piControl{}, false
	}
	return piControl{sock: sock, token: strings.TrimSpace(string(tok))}, true
}

type piRequest struct {
	Token   string `json:"token"`
	Op      string `json:"op"`
	Text    string `json:"text,omitempty"`
	ID      string `json:"id,omitempty"`
	Choice  int    `json:"choice,omitempty"`
	By      string `json:"by,omitempty"`
	Session string `json:"session,omitempty"`
	Through string `json:"through,omitempty"`
}

type piResponse struct {
	OK          bool   `json:"ok"`
	Error       string `json:"error"`
	Idle        bool   `json:"idle"`
	Queued      bool   `json:"queued"`
	HandoffHash string `json:"handoff_hash,omitempty"`
	Session     string `json:"session,omitempty"`
	Ack         string `json:"ack,omitempty"`
	Ask         *piAsk `json:"ask"`
}

// piAsk is the question a worker's guard is waiting on: a bash command a hook
// would ask the user about, which the control configuration lists as one to
// ask quartermaster about rather than block.
type piAsk struct {
	ID      string   `json:"id"`
	Command string   `json:"command"`
	Reason  string   `json:"reason"`
	Options []string `json:"options"`
}

func (c piControl) call(req piRequest) (piResponse, error) {
	req.Token = c.token
	conn, err := net.DialTimeout("unix", c.sock, piControlTimeout)
	if err != nil {
		return piResponse{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(piControlTimeout))
	b, err := json.Marshal(req)
	if err != nil {
		return piResponse{}, err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return piResponse{}, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return piResponse{}, err
	}
	var res piResponse
	if err := json.Unmarshal(line, &res); err != nil {
		return piResponse{}, fmt.Errorf("bad reply from %s: %w", filepath.Base(c.sock), err)
	}
	if !res.OK {
		return res, fmt.Errorf("%s: %s", filepath.Base(c.sock), res.Error)
	}
	return res, nil
}

// Prompt delivers text as a user message: at once when the worker is idle,
// else queued behind its running turn. queued reports which.
func (c piControl) Prompt(text string) (queued bool, err error) {
	res, err := c.call(piRequest{Op: "prompt", Text: text})
	return res.Queued, err
}

// Ask is the question the worker is waiting on, nil when there is none.
func (c piControl) Ask() (*piAsk, error) {
	res, err := c.call(piRequest{Op: "ask"})
	return res.Ask, err
}

// Answer answers ask id with option choice (1-based), recording by.
func (c piControl) Answer(id string, choice int, by string) error {
	_, err := c.call(piRequest{Op: "answer", ID: id, Choice: choice, By: by})
	return err
}

// prompt is the ask as quartermaster shows any approval prompt. Its hash
// covers the ask's id, so the same command asked twice is two prompts, and an
// answer meant for one never lands on the next.
func (a piAsk) prompt() ApprovalPrompt {
	p := ApprovalPrompt{
		Lines:    []string{"pi guard asks: Bash command", a.Command},
		Numbered: true,
		askID:    a.ID,
	}
	if a.Reason != "" {
		p.Lines = append(p.Lines, a.Reason)
	}
	for i, o := range a.Options {
		p.Options = append(p.Options, PromptOption{Number: i + 1, Label: o, Selected: i == 0})
	}
	sum := sha256.Sum256([]byte(promptHash(p) + "\n" + a.ID))
	p.Hash = hex.EncodeToString(sum[:])[:8]
	return p
}

// promptPiAgent delivers text to a over its control socket when it is a
// local pi worker whose extension is listening, else types it into its pane
// as herdr agent prompt does. viaSocket reports which.
func promptPiAgent(h Herdr, a Agent, text string) (viaSocket bool, err error) {
	if h.hostName() == localHost && a.Backend != rpcBackend {
		gate, e := adoptionAdmission(false)
		if e != nil {
			return false, e
		}
		defer gate.Close()
	}
	if h.hostName() == localHost && a.Backend != rpcBackend && adoptionHeldAgent(a) {
		return false, errors.New("worker reserved for migration; no legacy prompt sent")
	}
	if id, ok := rpcAgentID(a.PaneID); ok {
		return true, rpcPromptOnce(id, text)
	}
	if h.hostName() == localHost && isLocalPi(&a) {
		if c, ok := piControlFor(a.Name); ok {
			if _, err := c.Prompt(text); err == nil {
				return true, nil
			}
		}
	}
	return false, h.Prompt(a.PaneID, text)
}

// isLocalPi is a pi agent listed by this machine's herdr: only it can have a
// control socket here.
func isLocalPi(a *Agent) bool {
	return a != nil && a.Session.Agent == "pi" && (a.Host == "" || a.Host == localHost)
}

// piAskOf reads the ask a local pi worker is waiting on: nil, false when a
// isn't one or has no control socket, or one nothing listens on (a pi that
// was killed leaves its socket behind); nil, true when it has none waiting.
func piAskOf(host string, a *Agent) (*piAsk, bool, error) {
	if host != localHost || !isLocalPi(a) {
		return nil, false, nil
	}
	c, ok := piControlFor(a.Name)
	if !ok {
		return nil, false, nil
	}
	ask, err := c.Ask()
	if oe := (*net.OpError)(nil); errors.As(err, &oe) && oe.Op == "dial" {
		return nil, false, nil
	}
	return ask, true, err
}

// piControlConfig is config.json beside the sockets, which the extension
// reads on each ask.
type piControlConfig struct {
	Ask        []string `json:"ask"`
	AskTimeout int      `json:"ask_timeout_seconds,omitempty"`
}

// piAskTimeoutDefault is how long a worker waits on quartermaster's answer
// before it blocks the command as it would have without asking.
const piAskTimeoutDefault = 10 * time.Minute

// writePiControlConfig writes [agents.pi] ask and ask_timeout for the
// extension, when they differ from what is there.
func (c Config) writePiControlConfig() error {
	pc := c.Agents["pi"]
	cc := piControlConfig{Ask: pc.Ask}
	if cc.Ask == nil {
		cc.Ask = []string{}
	}
	if pc.AskTimeout != "" {
		d, err := time.ParseDuration(pc.AskTimeout)
		if err != nil {
			return fmt.Errorf("[agents.pi] ask_timeout: %w", err)
		}
		cc.AskTimeout = int(d.Seconds())
	}
	b, err := json.MarshalIndent(cc, "", "  ")
	if err != nil {
		return err
	}
	dir := piControlDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "config.json")
	if cur, err := os.ReadFile(path); err == nil && string(cur) == string(b) {
		return nil
	}
	return writeAtomic(path, b)
}

// validatePiAsk checks [agents.pi]: each ask pattern compiles and
// ask_timeout is a positive duration. The extension matches the patterns as
// JavaScript regular expressions; checking them as Go's catches typos, so
// keep to the syntax both share.
func (c Config) validatePiAsk() error {
	pc := c.Agents["pi"]
	for _, p := range pc.Ask {
		if _, err := regexp.Compile(p); err != nil {
			return fmt.Errorf("[agents.pi] ask: %q: %w", p, err)
		}
	}
	if pc.AskTimeout != "" {
		if d, err := time.ParseDuration(pc.AskTimeout); err != nil || d <= 0 {
			return fmt.Errorf("[agents.pi] ask_timeout %q: want a positive duration such as 10m", pc.AskTimeout)
		}
	}
	for kind, a := range c.Agents {
		if kind != "pi" && (len(a.Ask) > 0 || a.AskTimeout != "") {
			return fmt.Errorf("[agents.%s] ask and ask_timeout apply only to pi", kind)
		}
	}
	return nil
}

// tellCmd is the tell command: text to a bead's worker as a prompt.
// A local pi worker gets it over its control socket, queued behind a turn it
// is running; any other worker gets it typed into its pane, and only when it
// is at rest, since typing into a working or blocked pane can land in the
// middle of something.
func tellCmd(cfg Config, h Herdr, show func(string) (Bead, error), id, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", errors.New("nothing to tell")
	}
	b, err := show(id)
	if err != nil {
		return "", err
	}
	if rpcExists(b.ID) {
		return rpcTell(b.ID, text)
	}
	gate, err := adoptionAdmission(false)
	if err != nil {
		return "", err
	}
	defer gate.Close()
	// Recheck after admission: prepare may have published while we waited.
	if rpcExists(b.ID) {
		return rpcTell(b.ID, text)
	}
	host, name := beadHost(b), agentName(b.ID)
	if host == "" {
		host = localHost
	}
	a, err := agentOn(cfg, h)(host, name)
	if err != nil {
		return "", fmt.Errorf("find the worker%s: %w", onHost(host), err)
	}
	if a == nil {
		return "", fmt.Errorf("%s has no worker running%s", id, onHost(host))
	}
	if host == localHost && isLocalPi(a) {
		if c, ok := piControlFor(a.Name); ok {
			queued, err := c.Prompt(text)
			if err == nil {
				if queued {
					return "sent to " + name + " over its control socket, queued behind its running turn", nil
				}
				return "sent to " + name + " over its control socket", nil
			}
			if !agentReady(a) {
				return "", fmt.Errorf("%s is %s and its control socket failed: %w", name, a.Status, err)
			}
		}
	}
	if !agentReady(a) {
		return "", fmt.Errorf("%s is %s; tell it once it is idle", name, a.Status)
	}
	if err := cfg.herdrOn(h, host).Prompt(a.PaneID, text); err != nil {
		return "", err
	}
	return "typed into " + name + "'s pane" + onHost(host), nil
}
