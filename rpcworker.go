package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const rpcBackend = "pi-rpc"

var rpcIDRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,79}$`)

func rpcRoot() string            { return filepath.Join(stateDir(), "rpc") }
func rpcDir(id string) string    { return filepath.Join(rpcRoot(), id) }
func rpcSocket(id string) string { return filepath.Join(rpcDir(id), "control.sock") }
func rpcPrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("not a private directory: %s", dir)
	}
	return os.Chmod(dir, 0700)
}
func rpcSave(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".rpc-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func rpcLoad(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
func rpcID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Each record is a backend reservation, including when stopped. Dispatching
// the same bead through Herdr must not open a second writer to its worktree.
type rpcWorker struct {
	Bead     string       `json:"bead"`
	Cwd      string       `json:"cwd"`
	Branch   string       `json:"branch"`
	Args     []string     `json:"args"`
	Adoption *rpcAdoption `json:"adoption,omitempty"`
}
type rpcStatus struct {
	Bead        string    `json:"bead"`
	Phase       string    `json:"phase"`
	PID         int       `json:"supervisor_pid"`
	ChildPID    int       `json:"pi_pid"`
	SessionID   string    `json:"session_id"`
	SessionFile string    `json:"session_file"`
	Seq         int64     `json:"seq"`
	Error       string    `json:"error,omitempty"`
	Updated     time.Time `json:"updated"`
}
type rpcReceipt struct {
	ID            string    `json:"id"`
	Hash          string    `json:"hash"`
	State         string    `json:"state"` // intent/accepted/rejected; intent remains ambiguous after death
	Response      rpcRecord `json:"response"`
	PreviousPhase string    `json:"previous_phase,omitempty"`
	Epoch         int64     `json:"epoch,omitempty"`
}
type rpcRequest struct {
	Op   string `json:"op"`
	ID   string `json:"id,omitempty"`
	Text string `json:"text,omitempty"`
}
type rpcReply struct {
	OK      bool        `json:"ok"`
	Error   string      `json:"error,omitempty"`
	Status  *rpcStatus  `json:"status,omitempty"`
	Receipt *rpcReceipt `json:"receipt,omitempty"`
}

func rpcExists(id string) bool {
	if !rpcIDRE.MatchString(id) {
		return false
	}
	_, err := os.Stat(filepath.Join(rpcDir(id), "worker.json"))
	return err == nil
}
func rpcLease(id string) (*os.File, error) {
	if !rpcIDRE.MatchString(id) {
		return nil, errors.New("invalid bead id")
	}
	if err := rpcPrivateDir(rpcRoot()); err != nil {
		return nil, err
	}
	if err := rpcPrivateDir(rpcDir(id)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(rpcDir(id), "lease"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s already has a supervisor or pi holding its session lease", id)
	}
	return f, nil
}
func rpcClient(id string, req rpcRequest) (rpcReply, error) {
	if !rpcIDRE.MatchString(id) {
		return rpcReply{}, errors.New("invalid bead id")
	}
	c, err := net.DialTimeout("unix", rpcSocket(id), time.Second)
	if err != nil {
		return rpcReply{}, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(rpcCommandTimeout + 3*time.Second))
	if err = json.NewEncoder(c).Encode(req); err != nil {
		return rpcReply{}, err
	}
	var r rpcReply
	err = json.NewDecoder(io.LimitReader(c, 1<<20)).Decode(&r)
	if err == nil && !r.OK {
		err = errors.New(r.Error)
	}
	return r, err
}

// rpcSupervisor lives until explicitly stopped or its pi child dies. Client
// disconnects do not cancel accepted work. The socket is 0600 inside 0700;
// like the existing pi control channel this is NOT a same-user sandbox.
type rpcSupervisor struct {
	worker   rpcWorker
	mu       sync.Mutex
	status   rpcStatus
	proc     *rpcProcess
	log      *os.File
	promptMu sync.Mutex
	stop     chan struct{}
	stopOnce sync.Once
	stopping bool // monotonic admission barrier, independent of pi lifecycle events
}

func (s *rpcSupervisor) saveLocked() error {
	s.status.Updated = time.Now()
	return rpcSave(filepath.Join(rpcDir(s.worker.Bead), "status.json"), s.status)
}
func (s *rpcSupervisor) phaseLocked(phase string) {
	if !s.stopping {
		s.status.Phase = phase
	}
}
func (s *rpcSupervisor) event(raw []byte, rec rpcRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.log.Write(append(append([]byte(nil), raw...), '\n')); err != nil {
		return err
	}
	switch rec.Type {
	case "agent_start":
		if s.status.Error == "" {
			s.phaseLocked("working")
		}
		s.status.Seq++
	case "agent_settled":
		if s.status.Phase != "blocked" {
			s.phaseLocked("idle")
		}
		s.status.Seq++
	case "response":
		if rec.Command == "get_state" && rec.Success {
			var st struct {
				SessionID   string `json:"sessionId"`
				SessionFile string `json:"sessionFile"`
			}
			if err := json.Unmarshal(rec.Data, &st); err != nil {
				return err
			}
			if st.SessionID != "" {
				s.status.SessionID = st.SessionID
				s.status.SessionFile = st.SessionFile
			}
		}
		// Persist late prompt responses even after the request/client timed out.
		if rec.Command == "prompt" && rpcIDRE.MatchString(rec.ID) {
			path := filepath.Join(rpcDir(s.worker.Bead), "requests", rec.ID+".json")
			var receipt rpcReceipt
			if err := rpcLoad(path, &receipt); err != nil {
				return err
			}
			receipt.Response = rec
			receipt.State = "rejected"
			if rec.Success {
				receipt.State = "accepted"
			}
			var disposition struct {
				Disposition string `json:"disposition"`
			}
			_ = json.Unmarshal(rec.Data, &disposition)
			if (!rec.Success || disposition.Disposition == "handled") && receipt.Epoch == s.status.Seq && s.status.Error == "" {
				s.phaseLocked(receipt.PreviousPhase)
			}
			if err := rpcSave(path, receipt); err != nil {
				return err
			}
		}
	case "extension_ui_request":
		if rec.Method == "setStatus" && rec.StatusKey == "quartermaster.ask" {
			if rec.StatusText != "" {
				s.phaseLocked("blocked")
			} else if s.status.Error == "" {
				s.phaseLocked("working")
			}
		}
		switch rec.Method {
		case "confirm", "select", "input", "editor":
			// No blanket yes, no unattended dialog approval API. The existing
			// configured ask gates still use prompt/answer and its autonomy check.
			s.phaseLocked("blocked")
			s.status.Error = "Unsupported RPC extension dialog: " + rec.Title + ". Stop this worker; resolve the request explicitly before resuming. See events.jsonl."
			go s.blockDialog(rec)
		}
	}
	// Persist on boundaries, not every token. events.jsonl remains continuously
	// drained regardless of how slowly a client reads it.
	if rec.Type == "agent_start" || rec.Type == "agent_settled" || rec.Type == "response" || rec.Type == "extension_ui_request" {
		return s.saveLocked()
	}
	return nil
}
func (s *rpcSupervisor) blockDialog(rec rpcRecord) {
	// Wait until startRPC has published the process pointer.
	s.mu.Lock()
	p := s.proc
	s.mu.Unlock()
	if p == nil {
		return
	} // startup dialog remains unanswered (fail closed)
	_ = p.write(map[string]any{"type": "extension_ui_response", "id": rec.ID, "cancelled": true})
	// Unblock the UI first, then drain every already-admitted prompt before
	// clearing/aborting. New prompts see the latched error and cannot enter.
	s.promptMu.Lock()
	defer s.promptMu.Unlock()
	_, _ = p.call("block-"+rpcID(), "clear_queue", nil, rpcCommandTimeout)
	_, _ = p.call("block-"+rpcID(), "abort", nil, rpcCommandTimeout)
	bd := Beads{actor: "quartermaster"}
	_ = bd.Note(s.worker.Bead, "RPC worker needs a human: unsupported extension dialog "+rec.Method+": "+rec.Title+". Inspect quartermaster rpc events "+s.worker.Bead+"; stop before an explicit TUI fallback. No approval was granted.")
	_ = bd.SetStatus(s.worker.Bead, StatusNeedsMe)
}
func (s *rpcSupervisor) handle(req rpcRequest) rpcReply {
	fail := func(err error) rpcReply { return rpcReply{Error: err.Error()} }
	switch req.Op {
	case "status":
		s.mu.Lock()
		st := s.status
		s.mu.Unlock()
		if rpcParked(s.worker.Bead) {
			st.Phase = "parked"
		}
		return rpcReply{OK: true, Status: &st}
	case "receipt":
		if !rpcIDRE.MatchString(req.ID) {
			return fail(errors.New("invalid request id"))
		}
		var r rpcReceipt
		if err := rpcLoad(filepath.Join(rpcDir(s.worker.Bead), "requests", req.ID+".json"), &r); err != nil {
			return fail(err)
		}
		return rpcReply{OK: true, Receipt: &r}
	case "prompt":
		if !rpcIDRE.MatchString(req.ID) || strings.TrimSpace(req.Text) == "" || len(req.Text) > 1<<20 {
			return fail(errors.New("prompt requires id and 1..1MiB text"))
		}
		// Serialize submissions and cancellation; status remains independent.
		s.promptMu.Lock()
		defer s.promptMu.Unlock()
		sum := sha256.Sum256([]byte(req.Text))
		hash := hex.EncodeToString(sum[:])
		path := filepath.Join(rpcDir(s.worker.Bead), "requests", req.ID+".json")
		var old rpcReceipt
		if err := rpcLoad(path, &old); err == nil {
			if old.Hash != hash {
				return fail(errors.New("request id reused with different text"))
			}
			return rpcReply{OK: true, Receipt: &old} // reconcile, NEVER replay
		} else if !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
		s.mu.Lock()
		if s.stopping || s.status.Error != "" || s.status.Phase == "blocked" || rpcParked(s.worker.Bead) {
			s.mu.Unlock()
			return fail(errors.New("worker blocked/stopping; inspect status and bead before continuing"))
		}
		receipt := rpcReceipt{ID: req.ID, Hash: hash, State: "intent", PreviousPhase: s.status.Phase, Epoch: s.status.Seq + 1}
		if err := rpcSave(path, receipt); err != nil {
			s.mu.Unlock()
			return fail(err)
		}
		// Busy during acceptance too; a quick status client must not mistake
		// the gap before agent_start for completion. handled/rejected restores
		// the old phase only if no lifecycle event advanced the epoch.
		s.status.Phase = "working"
		s.status.Seq++
		err := s.saveLocked()
		s.mu.Unlock()
		if err != nil {
			return fail(err)
		}
		_, err = s.proc.call(req.ID, "prompt", map[string]any{"message": req.Text, "streamingBehavior": "followUp"}, rpcCommandTimeout)
		if err != nil {
			return fail(fmt.Errorf("%w; reconcile with quartermaster rpc receipt %s %s (never retry with a new id blindly)", err, s.worker.Bead, req.ID))
		}
		if err := rpcLoad(path, &receipt); err != nil {
			return fail(err)
		}
		return rpcReply{OK: true, Receipt: &receipt}
	case "activate":
		s.promptMu.Lock()
		defer s.promptMu.Unlock()
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.stopping || s.status.Error != "" || s.status.Phase != "idle" || s.worker.Adoption == nil {
			return fail(errors.New("only an idle, healthy adopted worker can leave migration parking"))
		}
		b, err := (Beads{}).Show(s.worker.Bead)
		if err != nil {
			return fail(err)
		}
		if b.Status != StatusInProgress {
			return fail(errors.New("resolve the bead blocker explicitly first; migration never grants approval"))
		}
		if err := os.Remove(rpcParkPath(s.worker.Bead)); err != nil {
			return fail(err)
		}
		return rpcReply{OK: true}
	case "cancel":
		// No new follow-up may slip between clear_queue and abort, which
		// would otherwise let pi continue it after the abort.
		s.promptMu.Lock()
		defer s.promptMu.Unlock()
		if _, err := s.proc.call(rpcID(), "clear_queue", nil, rpcCommandTimeout); err != nil {
			return fail(err)
		}
		if _, err := s.proc.call(rpcID(), "abort", nil, rpcCommandTimeout); err != nil {
			return fail(err)
		}
		return rpcReply{OK: true}
	case "stop":
		s.mu.Lock()
		s.stopping = true
		s.status.Phase = "stopping"
		_ = s.saveLocked()
		s.mu.Unlock()
		// Finish an admitted submission before acknowledging stop. Later
		// submissions fail against the barrier even if pi emits idle.
		s.promptMu.Lock()
		defer s.promptMu.Unlock()
		s.stopOnce.Do(func() { close(s.stop) })
		return rpcReply{OK: true}
	}
	return fail(errors.New("unknown RPC supervisor operation"))
}
func rpcServe(cfg Config, id string) error {
	lease, err := rpcLease(id)
	if err != nil {
		return err
	}
	defer lease.Close()
	var w rpcWorker
	if err = rpcLoad(filepath.Join(rpcDir(id), "worker.json"), &w); err != nil {
		return err
	}
	if w.Adoption != nil {
		if w.Adoption.GuardEnv != "" && w.Adoption.GuardEnv != adoptionGuardEnv(append(os.Environ(), cfg.Agents["pi"].Env...)) {
			return errors.New("adoption guard environment changed; refusing launch")
		}
		if !w.Adoption.Ready {
			return errors.New("adoption prepared but not verified; use rpc adopt after old owner exits")
		}
		if !w.Adoption.Completed {
			b, err := (Beads{}).Show(id)
			if err != nil {
				return err
			}
			if err := adoptionValidate(cfg.herdrOn(newHerdr(), localHost), b, w); err != nil {
				return err
			}
		}
		if err := adoptionPrefix(w.Adoption); err != nil {
			return err
		}
	}
	prev, err := rpcPreviousStatus(id)
	if err != nil {
		return err
	}
	if w.Adoption != nil && prev.SessionFile == "" {
		if err := adoptionPrefix(w.Adoption); err != nil {
			return err
		}
		prev.SessionID, prev.SessionFile = w.Adoption.SessionID, w.Adoption.SessionFile
	}
	if prev.ChildPID > 0 && processAlive(prev.ChildPID) {
		return fmt.Errorf("previous pi PID %d may still own the session; refusing resume", prev.ChildPID)
	}
	log, err := os.OpenFile(filepath.Join(rpcDir(id), "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	stderr, err := os.OpenFile(filepath.Join(rpcDir(id), "stderr.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer stderr.Close()
	s := &rpcSupervisor{worker: w, status: rpcStatus{Bead: id, Phase: "starting", PID: os.Getpid(), Seq: prev.Seq + 1, SessionID: prev.SessionID, SessionFile: prev.SessionFile}, log: log, stop: make(chan struct{})}
	if err = rpcPrivateDir(filepath.Join(rpcDir(id), "requests")); err != nil {
		return err
	}
	args := append([]string(nil), w.Args...)
	for _, arg := range w.Args {
		if arg == "--quartermaster-control" {
			if _, err := piExtensionFile(); err != nil {
				return err
			}
			if err := cfg.writePiControlConfig(); err != nil {
				return err
			}
		}
	}
	if prev.SessionFile != "" {
		if _, err = os.Stat(prev.SessionFile); err != nil {
			return fmt.Errorf("saved pi session unavailable: %w", err)
		}
		args = append(args, "--session", prev.SessionFile)
	} else {
		args = append(args, "--session-id", id, "--session-dir", filepath.Join(rpcDir(id), "sessions"))
	}
	if err = rpcPrivateDir(filepath.Join(rpcDir(id), "sessions")); err != nil {
		return err
	}
	cmd := exec.Command("pi", args...)
	cmd.Dir = w.Cwd
	cmd.Env = rpcWorkerEnv(cfg, id)
	if w.Adoption != nil {
		cmd.Env = append(cmd.Env, "QUARTERMASTER_RPC_PARK_FILE="+rpcParkPath(id))
	}
	// Persist the launch intent before a child can create a session. If we
	// die before get_state, the next launch refuses to guess its identity.
	if err := s.saveLocked(); err != nil {
		return err
	}
	s.mu.Lock()
	p, err := startRPC(cmd, lease, stderr, s.event)
	s.proc = p
	if err == nil {
		s.status.ChildPID = p.cmd.Process.Pid
		err = s.saveLocked()
	}
	s.mu.Unlock()
	if err != nil {
		if p != nil {
			p.stop()
			<-p.exited
			<-p.drained
		}
		return err
	}
	defer func() { p.stop(); <-p.exited; <-p.drained }()
	// Ready only after pi identifies the real session. Never invent a path.
	if _, err = p.call(rpcID(), "get_state", nil, 30*time.Second); err != nil {
		return err
	}
	// The explicitly loaded guard must be alive before any managed prompt.
	// Transport-only fixtures omit this flag; dispatch always supplies it.
	for _, arg := range w.Args {
		if arg == "--quartermaster-control" {
			c, ok := piControlFor(agentName(id))
			if !ok {
				return errors.New("pi guard control socket did not start; refusing prompts")
			}
			if _, err = c.call(piRequest{Op: "ping"}); err != nil {
				return fmt.Errorf("pi guard not ready: %w", err)
			}
		}
	}
	s.mu.Lock()
	if s.status.SessionID == "" || s.status.SessionFile == "" {
		s.mu.Unlock()
		return errors.New("pi did not identify its persistent session")
	}
	if w.Adoption != nil && (s.status.SessionID != w.Adoption.SessionID || s.status.SessionFile != w.Adoption.SessionFile) {
		s.mu.Unlock()
		return errors.New("adopted pi returned a different session identity")
	}
	if w.Adoption != nil && !w.Adoption.Completed {
		if err = adoptionPrefix(w.Adoption); err != nil {
			s.mu.Unlock()
			return err
		}
		w.Adoption.Completed = true
		if err = rpcSave(filepath.Join(rpcDir(id), "worker.json"), w); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	if s.status.Phase == "starting" {
		s.status.Phase = "idle"
	}
	err = s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	_ = os.Remove(rpcSocket(id)) // lease proves nobody else can own this endpoint
	l, err := net.Listen("unix", rpcSocket(id))
	if err != nil {
		return err
	}
	defer l.Close()
	defer os.Remove(rpcSocket(id))
	if err = os.Chmod(rpcSocket(id), 0600); err != nil {
		return err
	}
	go func() {
		slots := make(chan struct{}, 16)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
			default:
				c.Close()
				continue
			}
			go func() {
				defer func() { <-slots; c.Close() }()
				_ = c.SetDeadline(time.Now().Add(rpcCommandTimeout + 5*time.Second))
				var req rpcRequest
				if err := json.NewDecoder(io.LimitReader(c, 2<<20)).Decode(&req); err != nil {
					return
				}
				_ = json.NewEncoder(c).Encode(s.handle(req))
			}()
		}
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(signals)
	select {
	case <-s.stop:
	case <-signals:
	case <-p.done:
	}
	s.mu.Lock()
	s.stopping = true
	s.status.Phase = "stopping"
	_ = s.saveLocked()
	s.mu.Unlock()
	p.stop()
	<-p.exited
	<-p.drained
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Phase = "stopped"
	s.status.ChildPID = 0
	select {
	case <-s.stop:
	default:
		s.status.Phase = "failed"
		s.status.Error = "pi/supervisor interrupted; inspect events/stderr; resume restores saved conversation, not in-flight work"
	}
	return s.saveLocked()
}

// Absence is a first launch only when no launch artifacts exist. Neither
// corrupt/missing status nor a crash before get_state authorizes a new session.
func rpcPreviousStatus(id string) (rpcStatus, error) {
	var prev rpcStatus
	err := rpcLoad(filepath.Join(rpcDir(id), "status.json"), &prev)
	if err == nil {
		if prev.SessionID == "" || prev.SessionFile == "" {
			return prev, errors.New("prior launch has no confirmed session identity; inspect events/sessions and repair metadata explicitly before resuming")
		}
		return prev, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return prev, fmt.Errorf("cannot read saved session identity: %w", err)
	}
	for _, name := range []string{"events.jsonl", "sessions", "requests"} {
		_, err := os.Stat(filepath.Join(rpcDir(id), name))
		if err == nil {
			return prev, errors.New("saved session status missing but launch artifacts exist; refusing to create another session")
		}
		if !errors.Is(err, os.ErrNotExist) {
			return prev, err
		}
	}
	return prev, nil
}

func processAlive(pid int) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	// An orphan can remain a zombie until init reaps it, but cannot write
	// a session. On systems without /proc retain the conservative PID check.
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		if end := strings.LastIndex(string(b), ") "); end >= 0 && len(b) > end+2 && b[end+2] == 'Z' {
			return false
		}
	}
	return true
}

// Preserve the same Beads attribution and executor shims as TUI workers.
// Clear inherited pane/session identity, so neither pi nor tools attribute
// this worker to the coordinator's terminal or session.
func rpcWorkerEnv(cfg Config, id string) []string {
	env := []string{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "HERDR_") || strings.HasPrefix(k, "PI_SESSION_") || k == "PI_MODEL" || k == "PI_PROVIDER" || k == "PI_REASONING_LEVEL" || k == "QUARTERMASTER_EXEC_SOCKET" || k == "QUARTERMASTER_RPC_PARK_FILE" {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, cfg.Agents["pi"].Env...)
	env = append(env, "BEADS_ACTOR=pi", "QUARTERMASTER_RPC=1", "QUARTERMASTER_STATE_DIR="+stateDir())
	if cfg.BeadsDir != "" {
		env = append(env, "BEADS_DIR="+cfg.BeadsDir)
	}
	if execOpen(id) {
		env = append(env, "QUARTERMASTER_EXEC_SOCKET="+beadSocket(id))
		if qm, err := shimBinary(); err == nil {
			if dir, err := writeShims(qm); err == nil {
				env = append(env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
		}
	}
	return env
}
