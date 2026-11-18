package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed pi/headless.ts
var headlessExtension []byte

type headlessModel struct {
	p       *rpcProcess
	log     *os.File
	mu      sync.Mutex
	text    string
	failed  bool
	settled chan struct{}
}

func (m *headlessModel) observe(raw []byte, rec rpcRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.log.Write(append(raw, '\n')); err != nil {
		return err
	}
	if rec.Type == "extension_error" {
		m.failed = true
	}
	if rec.Type == "extension_ui_request" {
		switch rec.Method {
		case "confirm", "select", "input", "editor":
			return errors.New("consumer requires human interaction")
		}
	}
	if rec.Type == "message_end" {
		var msg struct {
			Role string `json:"role"`
			Stop string `json:"stopReason"`
		}
		_ = json.Unmarshal(rec.Message, &msg)
		if msg.Role == "assistant" {
			m.text = rpcAssistantText(raw)
			if msg.Stop == "error" || msg.Stop == "aborted" {
				m.failed = true
			}
		}
	}
	if rec.Type == "agent_settled" && m.settled != nil {
		close(m.settled)
		m.settled = nil
	}
	return nil
}
func headlessModelArgs(cfg Config) ([]string, error) {
	args := cfg.coordinatorArgs("pi")
	for _, arg := range args {
		key, _, _ := strings.Cut(arg, "=")
		switch key {
		case "--mode", "--print", "-p", "--session", "--session-id", "--session-dir", "--continue", "-c", "--resume", "-r", "--fork", "--no-session", "--quartermaster-control", "--quartermaster-coordinator":
			return nil, fmt.Errorf("headless owns mode/session/control; conflicting coordinator argument %s", key)
		}
	}
	// Normal guard baseline, but no worker control socket or TUI inbox endpoint.
	args, err := cfg.piLaunch(localHost, args, "headless", "")
	// Do not use worker identity or its bd-prime injection for the consumer.
	if err != nil {
		return nil, err
	}
	return args, nil
}
func newHeadlessModel(cfg Config, lease *os.File) (*headlessModel, error) {
	args, err := headlessModelArgs(cfg)
	if err != nil {
		return nil, err
	}
	ext, err := writePiExtension("headless.ts", headlessExtension)
	if err != nil {
		return nil, err
	}
	home := headlessPath("consumer")
	if err = rpcPrivateDir(home); err != nil {
		return nil, err
	}
	sessions := headlessPath("sessions")
	if err = rpcPrivateDir(sessions); err != nil {
		return nil, err
	}
	args = append(args, "--mode", "rpc", "--session-dir", sessions, "--session-id", "consumer-"+rpcID(), "--no-tools", "-e", ext)
	f, err := os.OpenFile(headlessPath("events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	stderr, err := os.OpenFile(headlessPath("stderr.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		f.Close()
		return nil, err
	}
	defer stderr.Close()
	cmd := exec.Command("pi", args...)
	cmd.Dir = home
	for _, kv := range rpcWorkerEnv(cfg, "") {
		if !strings.HasPrefix(kv, "BEADS_ACTOR=") && !strings.HasPrefix(kv, "QUARTERMASTER_EXEC_SOCKET=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	m := &headlessModel{log: f}
	m.p, err = startRPC(cmd, lease, stderr, m.observe)
	if err != nil {
		f.Close()
		return nil, err
	}
	// Launch readiness is not processing. There is no model request until an
	// explicitly enrolled event is due. Each restart gets a fresh proposal-only
	// conversation; operational receipts, never model memory, drive recovery.
	if _, err = m.p.call(rpcID(), "get_state", nil, 30*time.Second); err != nil {
		m.close()
		return nil, err
	}
	return m, nil
}
func (m *headlessModel) close() { m.p.stop(); <-m.p.exited; <-m.p.drained; m.log.Close() }
func (m *headlessModel) propose(ctx context.Context, e *headlessEvent, timeout time.Duration, delivered func() error) (string, error) {
	m.mu.Lock()
	m.text = ""
	m.failed = false
	done := make(chan struct{})
	m.settled = done
	m.mu.Unlock()
	payload, _ := json.Marshal(struct {
		Source   string    `json:"source"`
		Kind     EventKind `json:"kind"`
		Verified string    `json:"verified"`
	}{"automated; not the user; approves nothing", e.Kind, "enrolled resource was freshly read; supervisor will recheck before action"})
	if _, err := m.p.call("consume-"+rpcID(), "prompt", map[string]any{"message": "/quartermaster-consume " + string(payload)}, rpcCommandTimeout); err != nil {
		return "", err
	}
	if err := delivered(); err != nil {
		return "", err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-m.p.done:
		return "", errors.New("consumer exited")
	case <-timer.C:
		return "", errors.New("consumer deadline")
	case <-done:
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failed {
		return "", errors.New("consumer failed")
	}
	return headlessDecision(m.text)
}
func headlessLiveDeps(cfg Config, propose func(*headlessEvent, func() error) (string, error)) headlessDeps {
	return headlessDeps{
		now: time.Now, show: (Beads{}).Show, propose: propose, send: rpcClient,
		status: func(id string) (rpcStatus, error) {
			r, err := rpcClient(id, rpcRequest{Op: "status"})
			if err != nil {
				return rpcStatus{}, err
			}
			if r.Status == nil || !r.Status.CoordinatorFence {
				return rpcStatus{}, errors.New("worker supervisor lacks coordinator fencing")
			}
			return *r.Status, nil
		},
		receipt: func(id, request string) (rpcReceipt, error) {
			r, err := rpcClient(id, rpcRequest{Op: "receipt", ID: request})
			if err != nil {
				return rpcReceipt{}, err
			}
			if r.Receipt == nil {
				return rpcReceipt{}, errors.New("missing receipt")
			}
			return *r.Receipt, nil
		},
		authorize: func(b Bead) error {
			fresh, err := loadConfig()
			if err != nil {
				return err
			}
			_, err = fresh.enforce(b, "resume", "coordinator")
			return err
		},
		resource: func(en headlessEnrollment, b Bead) error {
			fresh, err := loadConfig()
			if err != nil {
				return err
			}
			if headlessScope(b, fresh.repoFor(b)) != en.Scope {
				return errors.New("task scope changed")
			}
			var w rpcWorker
			if err = rpcLoad(filepath.Join(rpcDir(b.ID), "worker.json"), &w); err != nil {
				return err
			}
			if headlessHash(w) != en.Worker || rpcParked(b.ID) {
				return errors.New("worker reservation changed or parked")
			}
			pr, err := (GH{repo: fresh.repoFor(b)}).View(en.PR, false)
			if err != nil {
				return err
			}
			if pr.URL != en.PR || pr.HeadSHA != en.Head || pr.State != "MERGED" || pr.Head != w.Branch {
				return errors.New("enrolled PR/head not merged")
			}
			return nil
		},
	}
}
func headlessRun(cfg Config) error {
	lease, err := headlessLock("consumer.lease", true)
	if err != nil {
		return err
	}
	defer lease.Close()
	c, err := headlessReadControl()
	if err != nil {
		return err
	}
	if len(c.Enrolled) == 0 {
		return errors.New("enroll explicit native workers first")
	}
	j, err := headlessLoadJournal()
	if err != nil {
		return err
	}
	hc := cfg.Headless.bounded()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	var model *headlessModel
	defer func() {
		if model != nil {
			model.close()
		}
	}()
	deps := headlessLiveDeps(cfg, func(e *headlessEvent, delivered func() error) (string, error) {
		if model == nil {
			var err error
			model, err = newHeadlessModel(cfg, lease)
			if err != nil {
				return "", err
			}
		}
		decision, err := model.propose(ctx, e, time.Duration(hc.ModelSeconds)*time.Second, delivered)
		if err != nil {
			model.close()
			model = nil
		} // only the restricted consumer, never a worker
		return decision, err
	})
	// Interrupted proposal work is safe to retry: there are no model action
	// tools. Interrupted downstream requests are reconciled, never resubmitted.
	for _, e := range j.Events {
		if e.State == "processing" || e.State == "delivered" {
			if err = j.retry(e, "consumer-interrupted", hc, time.Now()); err != nil {
				return err
			}
		}
	}
	tick := time.NewTicker(time.Duration(hc.PollSeconds) * time.Second)
	defer tick.Stop()
	for {
		c, err = headlessReadControl()
		if err != nil {
			return err
		}
		j.LastLoop = time.Now()
		items, err := readInbox()
		if err != nil {
			return err
		}
		j.receive(c, items)
		if err = j.save(); err != nil {
			return err
		}
		for _, id := range headlessEventOrder(j) {
			if ctx.Err() != nil {
				return nil
			}
			// Refresh ownership between every event, including after takeover.
			c, err = headlessReadControl()
			if err != nil {
				return err
			}
			if err = j.step(c, hc, j.Events[id], deps); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}
func headlessEnroll(cfg Config, id, url, by string) error {
	if !rpcIDRE.MatchString(id) || prURL.FindString(url) != url {
		return errors.New("enroll needs an exact native bead and GitHub PR URL")
	}
	gate, err := headlessLock("admission", true)
	if err != nil {
		return err
	}
	defer gate.Close()
	c, err := headlessReadControl()
	if errors.Is(err, os.ErrNotExist) {
		c = headlessControl{Mode: "paused", Epoch: rpcID(), Enrolled: map[string]headlessEnrollment{}}
	} else if err != nil {
		return err
	}
	if c.Mode == "background" {
		return errors.New("pause before changing enrollment")
	}
	existing, exists := c.Enrolled[id]
	if !exists && len(c.Enrolled) >= 32 {
		return errors.New("bounded trial limit: 32 enrolled workers")
	}
	b, err := (Beads{}).Show(id)
	if err != nil {
		return err
	}
	if b.Status != StatusInProgress || b.Assignee == "" {
		return errors.New("only explicitly active claimed work can be enrolled")
	}
	auto, err := cfg.enforce(b, "resume", by)
	if err != nil {
		return err
	}
	var w rpcWorker
	if err = rpcLoad(filepath.Join(rpcDir(id), "worker.json"), &w); err != nil {
		return err
	}
	if rpcParked(id) {
		return errors.New("parked workers cannot be enrolled")
	}
	st, err := rpcClient(id, rpcRequest{Op: "status"})
	if err != nil {
		return err
	}
	if st.Status == nil || !st.Status.CoordinatorFence || st.Status.Phase != "idle" {
		return errors.New("requires idle worker with coordinator-fencing support; never restart an active worker implicitly")
	}
	pr, err := (GH{repo: cfg.repoFor(b)}).View(url, false)
	if err != nil {
		return err
	}
	if pr.URL != url || pr.HeadSHA == "" || pr.Head != w.Branch || pr.State != "MERGED" {
		return errors.New("post-merge enrollment requires the exact native worker branch/head already merged; pre-merge workers remain on the existing ticker path")
	}
	en := headlessEnrollment{Bead: id, Scope: headlessScope(b, cfg.repoFor(b)), Worker: headlessHash(w), PR: url, Head: pr.HeadSHA, Since: time.Now()}
	if exists {
		if existing.Scope != en.Scope || existing.Worker != en.Worker || existing.PR != en.PR || existing.Head != en.Head {
			return errors.New("enrollment cannot replace scope or prior action receipts")
		}
		// Idempotent recovery if enrollment persisted before event publication.
		en = existing
		return writeEvent(Event{Bead: id, Kind: EventMerged, At: en.Since, Summary: "Explicitly enrolled post-merge follow-through; automated observation, not approval"})
	}
	c.Enrolled[id] = en
	c.Epoch = rpcID()
	if auto != "" {
		if err = (Beads{}).Note(id, auto+" enrolled exact native worker/PR for headless follow-through; no new scope or approval, no accumulated backlog"); err != nil {
			return err
		}
	}
	if err := rpcSave(headlessPath("control.json"), c); err != nil {
		return err
	}
	return writeEvent(Event{Bead: id, Kind: EventMerged, At: en.Since, Summary: "Explicitly enrolled post-merge follow-through; automated observation, not approval"})
}
func headlessMode(cfg Config, mode, by string) error {
	gate, err := headlessLock("admission", true)
	if err != nil {
		return err
	}
	defer gate.Close()
	c, err := headlessReadControl()
	if err != nil {
		return err
	}
	for id := range c.Enrolled {
		if mode == "paused" {
			break
		} // stopping admission never needs permission to resume work
		b, err := (Beads{}).Show(id)
		if err != nil {
			return err
		}
		if _, err = cfg.enforce(b, "resume", by); err != nil {
			return err
		}
	}
	// Foreground is explicit takeover, not a fallback on consumer death.
	c.Mode = mode
	c.Epoch = rpcID()
	return rpcSave(headlessPath("control.json"), c)
}
func headlessCmd(cfg Config, pos []string, by string, out io.Writer) error {
	if len(pos) == 0 {
		return errors.New("usage: headless enroll <bead> <PR-url> | run | pause | takeover | resume | status | watch | reports | ack <report-id>")
	}
	switch pos[0] {
	case "enroll":
		if len(pos) != 3 {
			return errors.New("enroll needs bead and exact PR URL")
		}
		return headlessEnroll(cfg, pos[1], pos[2], by)
	case "run":
		if len(pos) != 1 {
			return errors.New("run takes no arguments")
		}
		return headlessRun(cfg)
	case "pause", "takeover", "resume":
		if len(pos) != 1 {
			return errors.New("ownership command takes no arguments")
		}
		mode := map[string]string{"pause": "paused", "takeover": "foreground", "resume": "background"}[pos[0]]
		return headlessMode(cfg, mode, by)
	case "status":
		return headlessStatus(cfg, out)
	case "watch":
		return headlessWatch(cfg, out)
	case "reports":
		return headlessReports(out)
	case "ack":
		if len(pos) != 2 || len(pos[1]) != 64 || !rpcIDRE.MatchString(pos[1]) {
			return errors.New("ack needs exact report id")
		}
		var r headlessReport
		if err := rpcLoad(filepath.Join(headlessPath("reports"), pos[1]+".json"), &r); err != nil {
			return err
		}
		if err := rpcPrivateDir(headlessPath("acks")); err != nil {
			return err
		}
		return rpcSave(filepath.Join(headlessPath("acks"), pos[1]+".json"), map[string]any{"report": pos[1], "at": time.Now(), "receipt": "explicit-readback"})
	}
	return errors.New("unknown headless command")
}
