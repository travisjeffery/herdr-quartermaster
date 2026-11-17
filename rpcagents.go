package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Native agents join the existing ticker/thread model, without pretending
// there is a terminal. The rpc: handle is routed before any Herdr operation.
func rpcReservedPath(path string) bool {
	entries, _ := os.ReadDir(rpcRoot())
	for _, e := range entries {
		var w rpcWorker
		if rpcLoad(filepath.Join(rpcDir(e.Name()), "worker.json"), &w) == nil && w.Cwd == path {
			return true
		}
	}
	rel, err := filepath.Rel(rpcRoot(), path)
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	return len(parts) > 1 && rpcExists(parts[0])
}

func rpcAgentID(target string) (string, bool) {
	id, ok := strings.CutPrefix(target, "rpc:")
	return id, ok && rpcIDRE.MatchString(id)
}
func rpcAgents() []Agent {
	entries, _ := os.ReadDir(rpcRoot())
	var out []Agent
	for _, e := range entries {
		if !e.IsDir() || !rpcIDRE.MatchString(e.Name()) {
			continue
		}
		id := e.Name()
		r, err := rpcClient(id, rpcRequest{Op: "status"})
		if err != nil || r.Status == nil {
			continue
		}
		var w rpcWorker
		if rpcLoad(filepath.Join(rpcDir(id), "worker.json"), &w) != nil {
			continue
		}
		st := r.Status
		if st.Phase == "parked" {
			st.Phase = "blocked"
		}
		out = append(out, Agent{Name: agentName(id), Kind: "pi", Backend: rpcBackend, Status: st.Phase, PaneID: "rpc:" + id, Cwd: w.Cwd, Seq: st.Seq, Host: localHost, Session: AgentSession{Agent: "pi", Kind: "path", Value: st.SessionFile}})
	}
	return out
}
func rpcTell(id, text string) (string, error) {
	request := "tell-" + rpcID()
	_, err := rpcClient(id, rpcRequest{Op: "prompt", ID: request, Text: text})
	if err != nil {
		return "", fmt.Errorf("request %s: %w; inspect rpc receipt %s %s before sending again", request, err, id, request)
	}
	return "sent to " + id + " over native pi RPC; request " + request, nil
}

// Automated deliveries use a stable id across ticker/client restarts. An
// ambiguous receipt is an error, never success and never another send.
func rpcPromptOnce(id, text string) error {
	sum := sha256.Sum256([]byte(text))
	request := fmt.Sprintf("auto-%x", sum[:])
	r, err := rpcClient(id, rpcRequest{Op: "prompt", ID: request, Text: text})
	if err != nil {
		return err
	}
	if r.Receipt == nil || r.Receipt.State != "accepted" {
		return fmt.Errorf("automated prompt %s unresolved/rejected; inspect rpc receipt; not replayed", request)
	}
	return nil
}

func rpcRead(id string, lines int) (string, error) {
	f, err := os.Open(filepath.Join(rpcDir(id), "events.jsonl"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := max(int64(0), fi.Size()-rpcMaxRecord)
	r := bufio.NewReader(io.NewSectionReader(f, start, fi.Size()-start))
	if start > 0 {
		_, _ = r.ReadBytes('\n')
	} // discard a partial first record
	var b bytes.Buffer
	err = readRPC(r, func(raw []byte) error { _, err := b.WriteString(rpcAssistantText(raw)); return err })
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	parts := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n"), nil
}
