package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// source tags every sidebar token this tool writes, so clearing ours never
// touches another plugin's.
const source = toolName

type Agent struct {
	Name        string `json:"name"`
	Backend     string `json:"backend,omitempty"`
	Kind        string `json:"agent"`
	Status      string `json:"agent_status"`
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Cwd         string `json:"cwd"`
	Seq         int64  `json:"state_change_seq"`
	// Session is the conversation running in the pane, when herdr knows it.
	Session AgentSession `json:"agent_session"`
	// Host is the quartermaster host whose herdr server listed the agent.
	// Names and pane ids are only unique per server.
	Host string `json:"-"`
}

type Herdr struct {
	bin string
	// socket, when set, pins every call to that server instead of the one in
	// the environment.
	socket string
	// machine, when set, forwards every call to that herdr machine over SSH
	// (herdr --machine <label> ...); host is the quartermaster host it is.
	machine string
	host    string
}

func newHerdr() Herdr {
	bin := os.Getenv("HERDR_BIN_PATH")
	if bin == "" {
		bin = "herdr"
	}
	return Herdr{bin: bin}
}

// on returns h pinned to the herdr server at socket.
func (h Herdr) on(socket string) Herdr {
	h.socket = socket
	return h
}

// onMachine returns h forwarding every call to the herdr server on a saved
// machine. Herdr never falls back to local, so a call either reaches that
// server or fails. An empty label is the local server.
func (h Herdr) onMachine(label string) Herdr {
	h.machine = label
	return h
}

// hostName is the host this client talks to.
func (h Herdr) hostName() string {
	if h.host != "" {
		return h.host
	}
	if h.machine != "" {
		return h.machine
	}
	return localHost
}

// pinSocket rewrites env so herdr targets socket. HERDR_SESSION goes too: herdr
// resolves a session name to its own socket when HERDR_SOCKET_PATH is unset.
func pinSocket(env []string, socket string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "HERDR_SOCKET_PATH=") || strings.HasPrefix(kv, "HERDR_SESSION=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HERDR_SOCKET_PATH="+socket)
}

// output runs a herdr command and returns what it printed.
func (h Herdr) output(timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	argv := args
	if h.machine != "" {
		argv = append([]string{"--machine", h.machine}, args...)
	}
	cmd := exec.CommandContext(ctx, h.bin, argv...)
	if h.socket != "" {
		cmd.Env = pinSocket(os.Environ(), h.socket)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		where := ""
		if h.machine != "" {
			where = " on " + h.hostName()
		}
		return nil, fmt.Errorf("herdr %s%s: %v: %s", strings.Join(args[:min(2, len(args))], " "), where, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// call runs a herdr API command and returns its .result object.
func (h Herdr) call(timeout time.Duration, args ...string) (json.RawMessage, error) {
	out, err := h.output(timeout, args...)
	if err != nil {
		return nil, err
	}
	// Some commands (pane report-metadata) print nothing on success.
	if len(bytes.TrimSpace(out)) == 0 {
		return json.RawMessage("{}"), nil
	}
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("herdr %s: decoding: %w", strings.Join(args[:min(2, len(args))], " "), err)
	}
	return env.Result, nil
}

const callTimeout = 10 * time.Second

func (h Herdr) Agents() ([]Agent, error) {
	agents, _, err := h.observedAgents()
	return agents, err
}

// observedAgents keeps native workers usable during a Herdr outage, while
// reporting that absence from a partial list is not evidence of an exit.
func (h Herdr) observedAgents() ([]Agent, bool, error) {
	var native []Agent
	if h.hostName() == localHost {
		native = rpcAgents()
	}
	return h.agentsWithNative(native)
}

func (h Herdr) agentsWithNative(native []Agent) ([]Agent, bool, error) {
	res, err := h.call(callTimeout, "agent", "list")
	if err != nil {
		if len(native) > 0 {
			return native, false, nil
		}
		return nil, false, err
	}
	var out struct {
		Agents []Agent `json:"agents"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, false, err
	}
	for i := range out.Agents {
		out.Agents[i].Host = h.hostName()
	}
	return append(out.Agents, native...), true, nil
}

type Opened struct {
	WorkspaceID string
	PaneID      string
	Path        string
}

func parseOpened(res json.RawMessage) (Opened, error) {
	var r struct {
		Workspace struct {
			ID       string `json:"workspace_id"`
			Worktree struct {
				Path string `json:"checkout_path"`
			} `json:"worktree"`
		} `json:"workspace"`
		Worktree struct {
			Path string `json:"path"`
		} `json:"worktree"`
		RootPane struct {
			ID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return Opened{}, err
	}
	o := Opened{WorkspaceID: r.Workspace.ID, PaneID: r.RootPane.ID, Path: r.Worktree.Path}
	if o.Path == "" {
		o.Path = r.Workspace.Worktree.Path
	}
	if o.PaneID == "" {
		return o, fmt.Errorf("herdr returned no root pane")
	}
	return o, nil
}

func focusFlag(focus bool) string {
	if focus {
		return "--focus"
	}
	return "--no-focus"
}

func (h Herdr) WorktreeCreate(repo, branch, base, label string, focus bool) (Opened, error) {
	res, err := h.call(60*time.Second, "worktree", "create", "--cwd", repo, "--branch", branch, "--base", base, "--label", label, focusFlag(focus))
	if err != nil {
		return Opened{}, err
	}
	return parseOpened(res)
}

func (h Herdr) WorktreeOpen(repo, branch, label string, focus bool) (Opened, error) {
	res, err := h.call(60*time.Second, "worktree", "open", "--cwd", repo, "--branch", branch, "--label", label, focusFlag(focus))
	if err != nil {
		return Opened{}, err
	}
	return parseOpened(res)
}

// WorktreeRemove removes a workspace's worktree checkout. herdr refuses one
// with modified or untracked files unless force is set.
func (h Herdr) WorktreeRemove(workspace string, force bool) error {
	args := []string{"worktree", "remove", "--workspace", workspace}
	if force {
		args = append(args, "--force")
	}
	_, err := h.call(30*time.Second, args...)
	return err
}

type Worktree struct {
	Branch      string `json:"branch"`
	Path        string `json:"path"`
	Linked      bool   `json:"is_linked_worktree"`
	Prunable    bool   `json:"is_prunable"`
	WorkspaceID string `json:"open_workspace_id"`
}

func (h Herdr) Worktrees(repo string) ([]Worktree, error) {
	res, err := h.call(callTimeout, "worktree", "list", "--cwd", repo)
	if err != nil {
		return nil, err
	}
	var out struct {
		Worktrees []Worktree `json:"worktrees"`
	}
	return out.Worktrees, json.Unmarshal(res, &out)
}

type Workspace struct {
	ID          string `json:"workspace_id"`
	Label       string `json:"label"`
	HasWorktree bool   `json:"-"`
}

func (h Herdr) Workspaces() ([]Workspace, error) {
	res, err := h.call(callTimeout, "workspace", "list")
	if err != nil {
		return nil, err
	}
	var out struct {
		Workspaces []struct {
			ID       string          `json:"workspace_id"`
			Label    string          `json:"label"`
			Worktree json.RawMessage `json:"worktree"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	ws := make([]Workspace, 0, len(out.Workspaces))
	for _, w := range out.Workspaces {
		wt := bytes.TrimSpace(w.Worktree)
		ws = append(ws, Workspace{
			ID:          w.ID,
			Label:       w.Label,
			HasWorktree: len(wt) > 0 && string(wt) != "null",
		})
	}
	return ws, nil
}

func (h Herdr) WorkspaceClose(workspace string) error {
	_, err := h.call(callTimeout, "workspace", "close", workspace)
	return err
}

func (h Herdr) WorkspaceCreate(cwd, label string, focus bool) (Opened, error) {
	res, err := h.call(callTimeout, "workspace", "create", "--cwd", cwd, "--label", label, focusFlag(focus))
	if err != nil {
		return Opened{}, err
	}
	return parseOpened(res)
}

// WorkspaceLabel is the label of a workspace.
func (h Herdr) WorkspaceLabel(workspace string) (string, error) {
	res, err := h.call(callTimeout, "workspace", "get", workspace)
	if err != nil {
		return "", err
	}
	var out struct {
		Workspace struct {
			Label string `json:"label"`
		} `json:"workspace"`
	}
	return out.Workspace.Label, json.Unmarshal(res, &out)
}

func (h Herdr) WorkspaceRename(workspace, label string) error {
	_, err := h.call(callTimeout, "workspace", "rename", workspace, label)
	return err
}

func (h Herdr) TabRename(tab, label string) error {
	_, err := h.call(callTimeout, "tab", "rename", tab, label)
	return err
}

// Run types a command into a pane's shell and presses Enter.
func (h Herdr) Run(pane, command string) error {
	_, err := h.call(10*time.Second, "pane", "run", pane, command)
	return err
}

// AgentStart starts an agent in a pane; agentArgs go to the agent's own CLI.
func (h Herdr) AgentStart(name, kind, pane string, agentArgs ...string) error {
	args := []string{"agent", "start", name, "--kind", kind, "--pane", pane, "--timeout", "30000"}
	if len(agentArgs) > 0 {
		args = append(append(args, "--"), agentArgs...)
	}
	_, err := h.call(40*time.Second, args...)
	return err
}

// ForegroundArgv is the argv of each process in a pane's foreground group:
// for an idle agent, the agent itself.
func (h Herdr) ForegroundArgv(pane string) ([][]string, error) {
	res, err := h.call(callTimeout, "pane", "process-info", "--pane", pane)
	if err != nil {
		return nil, err
	}
	var out struct {
		Info struct {
			Procs []struct {
				Argv []string `json:"argv"`
			} `json:"foreground_processes"`
		} `json:"process_info"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	var argv [][]string
	for _, p := range out.Info.Procs {
		argv = append(argv, p.Argv)
	}
	return argv, nil
}

type Pane struct {
	ID    string `json:"pane_id"`
	Agent string `json:"agent"`
	Cwd   string `json:"cwd"`
}

func (h Herdr) Panes(workspace string) ([]Pane, error) {
	res, err := h.call(callTimeout, "pane", "list", "--workspace", workspace)
	if err != nil {
		return nil, err
	}
	var out struct {
		Panes []Pane `json:"panes"`
	}
	return out.Panes, json.Unmarshal(res, &out)
}

func (h Herdr) Prompt(target, text string) error {
	if h.hostName() == localHost {
		gate, err := adoptionAdmission(false)
		if err != nil {
			return err
		}
		defer gate.Close()
	}
	if h.hostName() == localHost && adoptionHeldAgent(Agent{PaneID: target, Name: target}) {
		return fmt.Errorf("worker reserved for migration; no legacy prompt sent")
	}
	if id, ok := rpcAgentID(target); ok {
		_, err := rpcTell(id, text)
		return err
	}
	_, err := h.call(callTimeout, "agent", "prompt", target, text)
	return err
}

// Read is the last lines of what an agent's pane shows, as plain text.
func (h Herdr) Read(target string, lines int, timeout time.Duration) (string, error) {
	if id, ok := rpcAgentID(target); ok {
		return rpcRead(id, lines)
	}
	out, err := h.output(timeout, "agent", "read", target, "--source", "visible", "--lines", strconv.Itoa(lines), "--format", "text")
	return string(out), err
}

// SendKeys presses keys in an agent's pane: names such as enter, down and esc,
// or literal characters.
func (h Herdr) SendKeys(target string, keys ...string) error {
	if _, ok := rpcAgentID(target); ok {
		return fmt.Errorf("native RPC workers have no terminal; use rpc cancel/stop")
	}
	_, err := h.call(callTimeout, append([]string{"agent", "send-keys", target}, keys...)...)
	return err
}

func (h Herdr) Rename(pane, name string) error {
	_, err := h.call(callTimeout, "agent", "rename", pane, name)
	return err
}

func (h Herdr) Focus(target string) error {
	if _, ok := rpcAgentID(target); ok {
		return fmt.Errorf("native RPC workers have no terminal; use rpc output/events")
	}
	_, err := h.call(callTimeout, "agent", "focus", target)
	return err
}

// Notify reports submission errors, not evidence that a human saw the alert.
func (h Herdr) Notify(title, body string) error {
	_, err := h.call(callTimeout, "notification", "show", title, "--body", body)
	return err
}

// ReportState sets this tool's sidebar tokens on a pane. A TTL makes stale rows
// disappear on their own if the ticker dies.
func (h Herdr) ReportState(pane string, tokens map[string]string, ttl time.Duration) error {
	if _, ok := rpcAgentID(pane); ok {
		return nil
	}
	args := []string{"pane", "report-metadata", pane, "--source", source, "--ttl-ms", strconv.FormatInt(ttl.Milliseconds(), 10)}
	for k, v := range tokens {
		args = append(args, "--token", k+"="+v)
	}
	_, err := h.call(callTimeout, args...)
	return err
}

func (h Herdr) ClearState(pane string) {
	if _, ok := rpcAgentID(pane); ok {
		return
	}
	for _, src := range append([]string{source}, legacyNames...) {
		_, _ = h.call(callTimeout, "pane", "report-metadata", pane, "--source", src, "--clear-token", "sh_state", "--clear-token", "sh_rank")
	}
}

func (h Herdr) OpenPane(entrypoint string) error {
	_, err := h.call(callTimeout, "plugin", "pane", "open", "--plugin", source, "--entrypoint", entrypoint)
	return err
}

// agentName maps a bead id onto herdr's agent-name alphabet
// [a-z][a-z0-9_-]{0,31}: the dot of a sub-bead becomes a dash.
func agentName(beadID string) string {
	return strings.ReplaceAll(strings.ToLower(beadID), ".", "-")
}
