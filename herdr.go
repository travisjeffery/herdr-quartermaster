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
	Kind        string `json:"agent"`
	Status      string `json:"agent_status"`
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Cwd         string `json:"cwd"`
	Seq         int64  `json:"state_change_seq"`
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
	res, err := h.call(callTimeout, "agent", "list")
	if err != nil {
		return nil, err
	}
	var out struct {
		Agents []Agent `json:"agents"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	for i := range out.Agents {
		out.Agents[i].Host = h.hostName()
	}
	return out.Agents, nil
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

type Pane struct {
	ID    string `json:"pane_id"`
	Agent string `json:"agent"`
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
	_, err := h.call(callTimeout, "agent", "prompt", target, text)
	return err
}

// Read is the last lines of what an agent's pane shows, as plain text.
func (h Herdr) Read(target string, lines int, timeout time.Duration) (string, error) {
	out, err := h.output(timeout, "agent", "read", target, "--source", "visible", "--lines", strconv.Itoa(lines), "--format", "text")
	return string(out), err
}

func (h Herdr) Rename(pane, name string) error {
	_, err := h.call(callTimeout, "agent", "rename", pane, name)
	return err
}

func (h Herdr) Focus(target string) error {
	_, err := h.call(callTimeout, "agent", "focus", target)
	return err
}

func (h Herdr) Notify(title, body string) {
	_, _ = h.call(callTimeout, "notification", "show", title, "--body", body)
}

// ReportState sets this tool's sidebar tokens on a pane. A TTL makes stale rows
// disappear on their own if the ticker dies.
func (h Herdr) ReportState(pane string, tokens map[string]string, ttl time.Duration) error {
	args := []string{"pane", "report-metadata", pane, "--source", source, "--ttl-ms", strconv.FormatInt(ttl.Milliseconds(), 10)}
	for k, v := range tokens {
		args = append(args, "--token", k+"="+v)
	}
	_, err := h.call(callTimeout, args...)
	return err
}

func (h Herdr) ClearState(pane string) {
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
