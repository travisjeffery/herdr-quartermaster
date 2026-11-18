package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// A generation is minted by the in-memory supervisor, not inferred from a PID
// or saved metadata. The server revalidates the entire selection at admission.
type rpcStopIdentity struct {
	Generation  string `json:"generation"`
	PID         int    `json:"supervisor_pid"`
	ChildPID    int    `json:"pi_pid"`
	SessionID   string `json:"session_id"`
	SessionFile string `json:"session_file"`
	Seq         int64  `json:"seq"`
	Parked      bool   `json:"parked"`
}

func rpcStopSelection(st rpcStatus, parked bool) rpcStopIdentity {
	return rpcStopIdentity{st.Generation, st.PID, st.ChildPID, st.SessionID, st.SessionFile, st.Seq, parked}
}

func rpcSettledReceipts(id string) error {
	entries, err := os.ReadDir(filepath.Join(rpcDir(id), "requests"))
	if err != nil {
		return err
	}
	for _, e := range entries {
		var r rpcReceipt
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			return errors.New("unexpected request artifact; inspect before stopping")
		}
		if err := rpcLoad(filepath.Join(rpcDir(id), "requests", e.Name()), &r); err != nil {
			return err
		}
		if r.ID+".json" != e.Name() || (r.State != "accepted" && r.State != "rejected") {
			return errors.New("uncertain request receipt; reconcile before stopping (never replay)")
		}
	}
	return nil
}

func (s *rpcSupervisor) stopGraceful(req rpcRequest) rpcReply {
	fail := func(err error) rpcReply { return rpcReply{Error: err.Error()} }
	// Do not wait behind a prompt/cancel/activation whose outcome is unknown.
	if !s.promptMu.TryLock() {
		return fail(errors.New("operation pending; graceful stop refused"))
	}
	defer s.promptMu.Unlock()
	validate := func() error { // caller holds s.mu
		st := s.status
		if !st.GracefulStop || st.Generation == "" || req.Expected == nil || *req.Expected != rpcStopSelection(st, rpcParked(s.worker.Bead)) {
			return errors.New("worker identity changed or graceful-stop capability absent; re-inspect")
		}
		if s.stopping || st.Phase != "idle" || st.Error != "" || st.SessionID == "" || st.SessionFile == "" || st.ChildPID <= 0 {
			return errors.New("graceful stop requires a healthy idle worker (parking does not prove idle)")
		}
		return nil
	}
	s.mu.Lock()
	err := validate()
	s.mu.Unlock()
	if err != nil {
		return fail(err)
	}
	if err := rpcSettledReceipts(s.worker.Bead); err != nil {
		return fail(err)
	}
	p := s.proc
	p.mu.Lock()
	pending := len(p.pending) != 0
	p.mu.Unlock()
	if pending {
		return fail(errors.New("pi operation pending; graceful stop refused"))
	}
	r, err := p.call("quiescence-"+rpcID(), "get_state", nil, rpcCommandTimeout)
	if err != nil {
		return fail(err)
	}
	// Pointer fields fail closed on older/incomplete state responses.
	var state struct {
		SessionID   string `json:"sessionId"`
		SessionFile string `json:"sessionFile"`
		Streaming   *bool  `json:"isStreaming"`
		Compacting  *bool  `json:"isCompacting"`
		Pending     *int   `json:"pendingMessageCount"`
	}
	if err := json.Unmarshal(r.Data, &state); err != nil {
		return fail(err)
	}
	if state.Streaming == nil || *state.Streaming || state.Compacting == nil || *state.Compacting || state.Pending == nil || *state.Pending != 0 || state.SessionID != req.Expected.SessionID || state.SessionFile != req.Expected.SessionFile {
		return fail(errors.New("pi state is busy, incomplete, or has a different session; graceful stop refused"))
	}
	// Managed workers also expose pi\'s broader idle predicate (retry/input
	// preflight included) through their existing guard. No extension upgrade
	// or configuration change is part of stopping a worker.
	for _, arg := range s.worker.Args {
		if arg != "--quartermaster-control" {
			continue
		}
		c, ok := piControlFor(agentName(s.worker.Bead))
		if !ok {
			return fail(errors.New("pi guard unavailable; graceful stop refused"))
		}
		ping, err := c.call(piRequest{Op: "ping"})
		if err != nil {
			return fail(err)
		}
		if !ping.Idle {
			return fail(errors.New("pi guard reports active work; graceful stop refused"))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validate(); err != nil {
		return fail(err)
	}
	select {
	case <-p.done:
		return fail(errors.New("pi transport failed during preflight; preserve ownership and inspect"))
	default:
	}
	// Latch admission before EOF. Lifecycle events cannot undo it, and even a
	// failed status write leaves it closed. No cancel, clear_queue or signal.
	s.stopping = true
	s.status.Phase = "stopping"
	s.status.GracefulStopRequested = true
	if err := s.saveLocked(); err != nil {
		return fail(err)
	}
	s.stopOnce.Do(func() { close(s.stop) })
	return rpcReply{OK: true}
}

// Never fall back to the old stop operation: old binaries silently ignore new
// request fields, so a flag attached to "stop" would be unsafe.
func rpcGracefulStop(id string, timeout time.Duration) (rpcReply, error) {
	r, err := rpcClient(id, rpcRequest{Op: "status"})
	if err != nil {
		return r, err
	}
	if r.Status == nil || !r.Status.GracefulStop || r.Status.Generation == "" {
		return r, errors.New("legacy supervisor: no guarded graceful-stop capability; untouched. Installing a new CLI cannot upgrade this process. Keep it owned until natural exit or obtain separate transition authorization; do not use stop, cancel, signals, or metadata edits as a fallback")
	}
	st := *r.Status
	selection := rpcStopSelection(st, st.Phase == "parked")
	if _, err := rpcClient(id, rpcRequest{Op: "stop-graceful", Expected: &selection}); err != nil {
		return rpcReply{}, fmt.Errorf("%w; stop may be refused or accepted: inspect live status/receipts before any further lifecycle action; never replay or force cleanup", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		// Only the free inherited lease plus the same generation's durable
		// terminal state proves completion. A disconnected socket is not exit.
		lease, err := rpcLease(id)
		if err == nil {
			var final rpcStatus
			err = rpcLoad(filepath.Join(rpcDir(id), "status.json"), &final)
			lease.Close()
			if err != nil {
				return rpcReply{}, err
			}
			if final.Generation != st.Generation || final.SessionID != st.SessionID || final.SessionFile != st.SessionFile || final.Phase != "stopped" || final.ChildPID != 0 {
				return rpcReply{}, errors.New("shutdown outcome/identity changed; inspect, do not resume automatically")
			}
			return rpcReply{OK: true, Status: &final}, nil
		}
		if !time.Now().Before(deadline) {
			return rpcReply{}, fmt.Errorf("graceful stop not confirmed within %s; no forced termination: supervisor/child retain ownership while alive. Inspect status and receipts; no automatic resume or replay", timeout)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
