package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// rpcLaunchArgs reuses model preflight, guard extension and ask configuration.
// Native sessions get a private directory and their exact file on resume.
func rpcLaunchArgs(cfg Config, id string) ([]string, error) {
	return rpcLaunchArgsForRole(cfg, id, builtinRolePreset(roleImplementer))
}

func rpcLaunchArgsForRole(cfg Config, id string, role EffectiveRole) ([]string, error) {
	args := cfg.workerArgs("pi", false, role)
	for _, a := range args {
		key, _, _ := strings.Cut(a, "=")
		switch key {
		case "--mode", "--print", "-p", "--session", "--session-id", "--session-dir", "--continue", "-c", "--resume", "-r", "--fork", "--no-session", "--quartermaster-control":
			return nil, fmt.Errorf("pi-rpc owns mode/session/control; remove %s from [agents.pi] args", key)
		}
	}
	args, err := cfg.piLaunch(localHost, args, "worker", id)
	if err != nil {
		return nil, err
	}
	args = args[:len(args)-2] // piLaunch appends piSessionArgs(id)
	return append(args, "--mode", "rpc"), nil
}

func dispatchRPC(cfg Config, h Herdr, id string, o DispatchOpts) (string, error) {
	if !rpcIDRE.MatchString(id) {
		return "", errors.New("invalid bead id")
	}
	if !filepath.IsAbs(stateDir()) {
		return "", errors.New("pi-rpc requires an absolute state directory")
	}
	if len(rpcSocket(id)) > 100 {
		return "", errors.New("RPC socket path too long; use a shorter QUARTERMASTER_STATE_DIR")
	}
	if o.Kind != "" && o.Kind != "pi" {
		return "", errors.New("pi-rpc requires --agent pi")
	}
	if o.Focus {
		return "", errors.New("pi-rpc has no pane; use quartermaster rpc status/output/events")
	}
	bd := Beads{actor: "pi"}
	b, err := bd.Show(id)
	if err != nil {
		return "", err
	}
	role, err := cfg.resolveRoleForBead(b, o.Role)
	if err != nil {
		return "", err
	}
	if role.Agent != "" && role.Agent != "pi" && role.Agent != "auto" {
		return "", fmt.Errorf("role %q sets agent %q, but pi-rpc requires pi", role.Name, role.Agent)
	}
	if role.Thinking != "" && !isValidPiThinkingLevel(role.Thinking) {
		return "", fmt.Errorf("role %q: invalid pi thinking level %q (want %s)", role.Name, role.Thinking, strings.Join(piThinkingLevels, ", "))
	}
	if b.Status == StatusClosed {
		return "", fmt.Errorf("%s is closed", id)
	}
	if role.Worktree {
		if _, ok := cfg.repoName(b); !ok {
			return "", errors.New("unknown repository label")
		}
	}
	host, err := cfg.hostFor(b, o.Host, o.Force)
	if err != nil {
		return "", err
	}
	if host != localHost {
		return "", errors.New("pi-rpc is local only")
	}
	auto, err := cfg.enforce(b, "dispatch", o.By)
	if err != nil {
		return "", err
	}
	if rpcExists(id) {
		return "", fmt.Errorf("%s already has a pi-rpc reservation; use rpc status or rpc resume (no implicit replay)", id)
	}
	// Beads plus git's branch/worktree ownership protect existing TUI workers,
	// even when Herdr is absent. Do not adopt or migrate another checkout.
	listed, _ := agentsOnHosts(cfg, h)
	for _, agents := range listed {
		for _, a := range agents {
			if a.Name == agentName(id) {
				return "", fmt.Errorf("%s already has an agent", id)
			}
		}
	}
	roleInstr, err := loadRoleInstructions(role)
	if err != nil {
		return "", err
	}
	args, err := rpcLaunchArgsForRole(cfg, id, role)
	if err != nil {
		return "", err
	}
	lease, err := rpcLease(id)
	if err != nil {
		return "", err
	}
	// serialize both the reservation and creation; release only after durable
	// worker.json, before the detached supervisor takes over the same lease.
	defer lease.Close()
	if rpcExists(id) {
		return "", errors.New("RPC reservation already exists")
	}
	var (
		branch  string
		cwd     string
		repoRef string
	)
	if role.Worktree {
		repo := cfg.repoFor(b)
		branch = branchFor(cfg.BranchPrefix, b)
		cwd = filepath.Join(rpcDir(id), "worktree")
		cwd, err = filepath.Abs(cwd)
		if err != nil {
			return "", err
		}
		out, err := exec.Command("git", "-C", repo, "worktree", "add", "-b", branch, cwd, cfg.BaseBranch).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("new isolated RPC worktree (existing branches are never adopted): %w: %s", err, out)
		}
	} else {
		cwd, err = ensureRoleDir(id)
		if err != nil {
			return "", fmt.Errorf("role dir for %s: %w", id, err)
		}
		if rn, ok := cfg.repoName(b); ok {
			if rn != "" {
				repoRef = cfg.Repos[rn]
			} else {
				repoRef = cfg.Repo
			}
		}
	}
	w := rpcWorker{Bead: id, Cwd: cwd, Branch: branch, Args: args}
	if err = rpcSave(filepath.Join(rpcDir(id), "worker.json"), w); err != nil {
		return "", err
	}
	if err = bd.Claim(id); err != nil && b.Status != StatusInProgress {
		return "", err
	}
	if err = setHostLabel(bd, b, localHost); err != nil {
		return "", err
	}
	if err = setRoleLabel(bd, b, role.Name); err != nil {
		return "", err
	}
	if auto != "" {
		_ = bd.Note(id, auto+" dispatched pi-rpc in "+cwd+" — native local supervisor; no Herdr pane")
	}
	lease.Close()
	if err = rpcStart(id); err != nil {
		return "", err
	}
	text := briefForRole(b, role, branch, cwd, repoRef, linearKey(b, cfg.LinearPrefixes), readInstructions(), roleInstr)
	text += "\nTransport: native pi RPC, no Herdr pane. Do not run herdr commands. Inspect with quartermaster rpc status/output/events; use quartermaster tell for follow-ups.\n"
	reqID := "brief-" + rpcID()
	if _, err = rpcClient(id, rpcRequest{Op: "prompt", ID: reqID, Text: text}); err != nil {
		return "", fmt.Errorf("worker started; brief request %s may have been delivered: %w", reqID, err)
	}
	if !role.Worktree {
		return fmt.Sprintf("started pi-rpc on %s (role %s, dir %s); request %s", id, role.Name, cwd, reqID), nil
	}
	return fmt.Sprintf("started pi-rpc on %s (branch %s, worktree %s); request %s", id, branch, cwd, reqID), nil
}

func rpcStart(id string) error {
	// A live supervisor is a reconnection, not a launch or another prompt.
	if r, err := rpcClient(id, rpcRequest{Op: "status"}); err == nil && r.Status != nil {
		return nil
	}
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(rpcDir(id), "supervisor.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.Command(bin, "rpc", "serve", id)
	cmd.Env = append(os.Environ(), "QUARTERMASTER_STATE_DIR="+stateDir(), "QUARTERMASTER_CONFIG_DIR="+configDir())
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.NewTimer(35 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			return fmt.Errorf("RPC supervisor exited (%v); see %s", err, f.Name())
		case <-deadline.C:
			return fmt.Errorf("RPC startup timed out; inspect %s and rpc status before retrying; no prompt sent", f.Name())
		case <-tick.C:
			if _, err := rpcClient(id, rpcRequest{Op: "status"}); err == nil {
				return nil
			}
		}
	}
}

func rpcResume(cfg Config, id, by string) (string, error) {
	if !rpcExists(id) {
		return "", errors.New("no native RPC worker reservation")
	}
	if r, err := rpcClient(id, rpcRequest{Op: "status"}); err == nil && r.Status != nil {
		if r.Status.Phase == "stopping" {
			return "", errors.New("worker still owns its session while stopping; inspect, do not restart or replay")
		}
		return fmt.Sprintf("reconnected to %s (%s); no prompt sent", id, r.Status.Phase), nil
	}
	b, err := (Beads{}).Show(id)
	if err != nil {
		return "", err
	}
	if b.Status == StatusClosed {
		return "", errors.New("bead is closed")
	}
	auto, err := cfg.enforce(b, "resume", by)
	if err != nil {
		return "", err
	}
	if err = rpcStart(id); err != nil {
		return "", err
	}
	if auto != "" {
		_ = (Beads{}).Note(id, auto+" resumed saved pi-rpc conversation; in-flight work and queued prompts not replayed")
	}
	return "resumed " + id + " from saved session; inspect receipts/events, then tell it explicitly what to continue", nil
}

func rpcAssistantText(raw []byte) string {
	var e struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	var text strings.Builder
	if json.Unmarshal(raw, &e) == nil && e.Type == "message_end" && e.Message.Role == "assistant" {
		for _, c := range e.Message.Content {
			if c.Type == "text" {
				text.WriteString(c.Text)
				text.WriteByte('\n')
			}
		}
	}
	return text.String()
}

// rpcCmd is deliberately bounded: no raw command forwarding, session
// switching, arbitrary shell execution, or approval responses.
func rpcCmd(cfg Config, pos []string, by string, out io.Writer) error {
	if len(pos) < 2 {
		return errors.New("usage: quartermaster rpc view|inspect|history|feed|status|output|events|receipt|tell|steer|fork|fork-status|cancel|stop|stop-graceful|resume <bead> [arguments]")
	}
	op, id := pos[0], pos[1]
	if !rpcIDRE.MatchString(id) {
		return errors.New("invalid bead id")
	}
	switch op {
	case "view":
		return runRPCView(id, "", nil)
	case "inspect":
		v, err := rpcViewStatus(id)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(v)
	case "history", "feed":
		if len(pos) > 3 {
			return errors.New("usage: rpc history|feed <bead> [cursor]")
		}
		cursor := ""
		if len(pos) == 3 {
			cursor = pos[2]
		}
		source := "history"
		if op == "feed" {
			source = "events"
		}
		p, err := rpcViewRead(id, source, cursor)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(p)
	case "handoff-prepare", "handoff":
		msg, err := rpcHandoffCmd(cfg, cfg.herdrOn(newHerdr(), localHost), pos, by)
		if msg != "" {
			fmt.Fprintln(out, msg)
		}
		return err
	case "prepare", "adopt":
		msg, err := rpcAdoptionCmd(cfg, cfg.herdrOn(newHerdr(), localHost), pos, by)
		if msg != "" {
			fmt.Fprintln(out, msg)
		}
		return err
	case "fork", "fork-status":
		return rpcForkCmd(cfg, pos, by, out)
	case "serve":
		return rpcServe(cfg, id)
	case "resume":
		msg, err := rpcResume(cfg, id, by)
		if msg != "" {
			fmt.Fprintln(out, msg)
		}
		return err
	case "events", "output":
		f, err := os.Open(filepath.Join(rpcDir(id), "events.jsonl"))
		if err != nil {
			return err
		}
		defer f.Close()
		if op == "events" {
			_, err = io.Copy(out, f)
			return err
		}
		// Only authoritative completed assistant messages, not duplicate deltas.
		err = readRPCLog(f, func(raw []byte) error {
			_, err := io.WriteString(out, rpcAssistantText(raw))
			return err
		})
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		return err
	case "status":
		r, err := rpcClient(id, rpcRequest{Op: op})
		if err != nil {
			var st rpcStatus
			if loadErr := rpcLoad(filepath.Join(rpcDir(id), "status.json"), &st); loadErr != nil {
				return err
			}
			st.Phase = "disconnected"
			st.Error = "supervisor unreachable; saved status only; use rpc resume, never replay uncertain requests"
			r = rpcReply{OK: true, Status: &st}
		}
		return json.NewEncoder(out).Encode(r)
	case "receipt":
		if len(pos) != 3 || !rpcIDRE.MatchString(pos[2]) {
			return errors.New("receipt needs request-id")
		}
		var r rpcReceipt
		if err := rpcLoad(filepath.Join(rpcDir(id), "requests", pos[2]+".json"), &r); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(r)
	case "tell", "steer":
		if len(pos) != 4 {
			return errors.New("usage: quartermaster rpc tell|steer <bead> <request-id> <text>")
		}
		var r rpcReply
		var err error
		if op == "steer" {
			r, err = rpcSteerCmd(cfg, id, pos[2], pos[3], by)
		} else {
			r, err = rpcClient(id, rpcRequest{Op: "prompt", ID: pos[2], Text: pos[3]})
		}
		if err != nil {
			return err
		}
		if err := json.NewEncoder(out).Encode(r); err != nil {
			return err
		}
		if r.Receipt == nil || r.Receipt.State != "accepted" {
			return errors.New("request unresolved or rejected; inspect receipt/events, not replayed")
		}
		return nil
	case "activate", "cancel", "stop", "stop-graceful":
		if len(pos) != 2 {
			return errors.New("usage: rpc activate|cancel|stop|stop-graceful <bead>")
		}
		b, err := (Beads{}).Show(id)
		if err != nil {
			return err
		}
		auto, err := cfg.enforce(b, "resume", by)
		if err != nil {
			return err
		} // local process control is class 3
		var r rpcReply
		if op == "stop-graceful" {
			if auto != "" {
				_ = (Beads{}).Note(id, auto+" pi-rpc stop-graceful attempt; timeout/disconnection is not proof of refusal or exit, inspect status")
			}
			r, err = rpcGracefulStop(id, 5*time.Second)
		} else {
			r, err = rpcClient(id, rpcRequest{Op: op})
		}
		if err != nil {
			return err
		}
		if auto != "" {
			_ = (Beads{}).Note(id, auto+" pi-rpc "+op)
		}
		return json.NewEncoder(out).Encode(r)
	}
	return errors.New("unknown rpc command")
}
