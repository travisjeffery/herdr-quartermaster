package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const rpcStrandedSteering = "steering may remain queued after the run settled; inspect receipt/events and explicitly rpc cancel before further prompts (no automatic replay)"

// Empty operation is the original follow-up receipt format. Never reinterpret
// an old prompt receipt as permission to deliver a steer (or vice versa).
func (r rpcReceipt) operation() string {
	if r.Operation == "" {
		return "prompt"
	}
	return r.Operation
}

func rpcMessageHash(op, text string) string {
	if op == "steer" {
		text = "steer\x00" + text
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// submit admits only the two typed message operations. Cancellation, parking
// and stop share this admission lock; a disconnect never withdraws a receipt.
func (s *rpcSupervisor) submit(req rpcRequest) rpcReply {
	fail := func(err error) rpcReply { return rpcReply{Error: err.Error()} }
	if (req.Op != "prompt" && req.Op != "steer") || !rpcIDRE.MatchString(req.ID) || strings.TrimSpace(req.Text) == "" || len(req.Text) > 1<<20 {
		return fail(errors.New("message requires a supported operation, id and 1..1MiB text"))
	}
	gate, err := headlessAdmission(s.worker.Bead, req.Owner)
	if err != nil {
		return fail(err)
	}
	if gate != nil {
		defer gate.Close()
	}
	s.promptMu.Lock()
	defer s.promptMu.Unlock()
	hash := rpcMessageHash(req.Op, req.Text)
	path := filepath.Join(rpcDir(s.worker.Bead), "requests", req.ID+".json")
	var old rpcReceipt
	if err := rpcLoad(path, &old); err == nil {
		if old.Hash != hash || old.operation() != req.Op {
			return fail(errors.New("request id reused with different text or operation"))
		}
		return rpcReply{OK: true, Receipt: &old} // reconcile, NEVER replay
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	validate := func() error { // caller holds s.mu
		if s.stopping || s.status.Error != "" || s.status.Phase == "blocked" || rpcParked(s.worker.Bead) {
			return errors.New("worker blocked/stopping; inspect status and bead before continuing")
		}
		if req.Op == "steer" && (!s.status.Steering || s.status.Phase != "working") {
			return errors.New("steering requires a capable running worker; use tell for idle work")
		}
		return nil
	}
	s.mu.Lock()
	err = validate()
	s.mu.Unlock()
	if err != nil {
		return fail(err)
	}
	if req.Op == "steer" {
		// Unlike prompt(streamingBehavior=steer), steer never starts an idle
		// run. Refuse known-idle/compacting state; a turn can still settle
		// after this preflight, leaving a queued steer for explicit inspection.
		r, err := s.proc.call("steer-state-"+rpcID(), "get_state", nil, rpcCommandTimeout)
		if err != nil {
			return fail(err)
		}
		var state struct {
			SessionID   string `json:"sessionId"`
			SessionFile string `json:"sessionFile"`
			Streaming   *bool  `json:"isStreaming"`
			Compacting  *bool  `json:"isCompacting"`
		}
		if err := json.Unmarshal(r.Data, &state); err != nil {
			return fail(err)
		}
		s.mu.Lock()
		matches := state.SessionID == s.status.SessionID && state.SessionFile == s.status.SessionFile
		s.mu.Unlock()
		if !matches || state.Streaming == nil || !*state.Streaming || state.Compacting == nil || *state.Compacting {
			return fail(errors.New("pi is not streaming, is compacting, or has changed session; no steer sent"))
		}
	}
	s.mu.Lock()
	if err := validate(); err != nil {
		s.mu.Unlock()
		return fail(err)
	}
	receipt := rpcReceipt{ID: req.ID, Operation: req.Op, Hash: hash, State: "intent", PreviousPhase: s.status.Phase, Epoch: s.status.Seq + 1}
	if err := rpcSave(path, receipt); err != nil {
		s.mu.Unlock()
		return fail(err)
	}
	if req.Op == "steer" {
		s.steeringInFlight, s.steeringQueueSeen = true, false
	}
	s.status.Phase = "working"
	s.status.Seq++
	err = s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return fail(err)
	}
	fields := map[string]any{"message": req.Text}
	if req.Op == "prompt" {
		fields["streamingBehavior"] = "followUp"
	}
	_, err = s.proc.call(req.ID, req.Op, fields, rpcCommandTimeout)
	if err != nil {
		return fail(fmt.Errorf("%w; reconcile with quartermaster rpc receipt %s %s (never retry with a new id blindly)", err, s.worker.Bead, req.ID))
	}
	if err := rpcLoad(path, &receipt); err != nil {
		return fail(err)
	}
	return rpcReply{OK: true, Receipt: &receipt}
}

func rpcSteerCmd(cfg Config, id, request, text, by string) (rpcReply, error) {
	if !rpcIDRE.MatchString(request) || strings.TrimSpace(text) == "" || len(text) > 1<<20 {
		return rpcReply{}, errors.New("steer requires request-id and 1..1MiB text")
	}
	b, err := (Beads{}).Show(id)
	if err != nil {
		return rpcReply{}, err
	}
	if b.Status != StatusInProgress {
		return rpcReply{}, errors.New("steering requires an in_progress bead; it cannot answer a blocker")
	}
	auto, err := cfg.enforce(b, "steer", by)
	if err != nil {
		return rpcReply{}, err
	}
	// This is an attempted operation, not proof of model consumption. Even a
	// human invocation has a durable note; never put prompt text in the note.
	note := fmt.Sprintf("%s pi-rpc steer attempt %s by %s; text hash %s; reconcile receipt/events, never replay", auto, request, by, rpcMessageHash("steer", text))
	if err := (Beads{}).Note(id, strings.TrimSpace(note)); err != nil {
		return rpcReply{}, err
	}
	return rpcClient(id, rpcRequest{Op: "steer", ID: request, Text: text})
}
