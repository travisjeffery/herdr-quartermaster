package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Visibility is separate from the ticker's live-agent set: a stopped worker
// remains inspectable but must not become a delivery target or a live agent.
type rpcVisibility struct {
	State      string    `json:"state"`
	SavedPhase string    `json:"saved_phase"`
	Connected  bool      `json:"connected"`
	Observed   time.Time `json:"observed"`
	Started    time.Time `json:"started,omitempty"`
	Ended      time.Time `json:"ended,omitempty"`
	Activity   string    `json:"activity,omitempty"`
	ActivityAt time.Time `json:"activity_at,omitempty"`
	Current    string    `json:"current,omitempty"`
	Provider   string    `json:"provider,omitempty"`
	Model      string    `json:"model,omitempty"`
}

// Only the existing status operation is sent, with a short independent
// deadline. Failure is disconnected, NOT stopped or parked based on PID/idle.
func rpcViewStatus(id string) (rpcVisibility, error) {
	v := rpcVisibility{State: "unknown", Observed: time.Now()}
	if !rpcIDRE.MatchString(id) || !rpcExists(id) {
		return v, nil
	}
	var st rpcStatus
	_ = rpcLoad(filepath.Join(rpcDir(id), "status.json"), &st)
	c, err := net.DialTimeout("unix", rpcSocket(id), 150*time.Millisecond)
	if err == nil {
		_ = c.SetDeadline(time.Now().Add(250 * time.Millisecond))
		var reply rpcReply
		err = json.NewEncoder(c).Encode(rpcRequest{Op: "status"})
		if err == nil {
			err = json.NewDecoder(io.LimitReader(c, 1<<20)).Decode(&reply)
		}
		c.Close()
		if err == nil && reply.OK && reply.Status != nil {
			st = *reply.Status
			v.Connected = true
		}
	}
	v.SavedPhase = rpcViewRedact(st.Phase)
	switch {
	case !v.Connected && (st.Phase == "stopped" || st.Phase == "failed"):
		v.State = "stopped"
	case !v.Connected:
		v.State = "disconnected"
	case st.Phase == "working":
		v.State = "running"
	case st.Phase == "blocked" && st.Error != "":
		v.State = "blocked (intervention)"
	case st.Phase == "blocked":
		v.State = "waiting on approval"
	case st.Phase == "parked":
		v.State = "parked"
	case st.Phase == "idle":
		v.State = "idle (not parked)"
	case st.Phase == "starting" || st.Phase == "stopping":
		v.State = st.Phase
	default:
		v.State = "unknown"
	}
	v.Started, v.Ended, v.ActivityAt = st.RunStarted, st.RunEnded, st.ActivityAt
	v.Activity, _ = rpcViewBound(st.Activity)
	v.Current, _ = rpcViewBound(st.Current)
	v.Provider, _ = rpcViewBound(st.Provider)
	v.Model, _ = rpcViewBound(st.Model)
	if !v.Connected || v.State == "parked" || st.Phase == "idle" {
		v.Current = ""
	}
	return v, nil
}

// Called only by the existing stdout owner. Values here are metadata, not
// copied arguments/output. Persist at most once per second during streaming;
// event boundaries still use the supervisor's existing durable status writes.
func (s *rpcSupervisor) observeView(raw []byte, rec rpcRecord, now time.Time) bool {
	var e struct {
		ToolName string `json:"toolName"`
		ToolID   string `json:"toolCallId"`
		Message  struct {
			Role     string `json:"role"`
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"message"`
		Data struct {
			Model struct {
				ID       string `json:"id"`
				Provider string `json:"provider"`
			} `json:"model"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &e)
	toolKey := fmt.Sprintf("%x", sha256.Sum256([]byte(e.ToolID)))
	activity := ""
	switch rec.Type {
	case "agent_start":
		s.status.RunStarted = now
		s.status.RunEnded = time.Time{}
		activity = "run started"
		s.status.Current = "agent run"
	case "agent_settled":
		s.status.RunEnded = now
		activity = "run settled (not task completion)"
		s.status.Current = ""
		s.viewTools = nil
		s.viewToolsOverflow = false
	case "message_start", "message_update":
		if rec.Type == "message_update" || e.Message.Role == "assistant" {
			activity = "assistant streaming"
			if len(s.viewTools) == 0 && !s.viewToolsOverflow {
				s.status.Current = activity
			}
		}
	case "message_end":
		activity = e.Message.Role + " message completed"
	case "tool_execution_start":
		if s.viewTools == nil {
			s.viewTools = map[string]string{}
		}
		if len(s.viewTools) < 64 {
			s.viewTools[toolKey], _ = rpcViewBound(e.ToolName)
		} else {
			s.viewToolsOverflow = true
		}
		activity = "tool started: " + e.ToolName
	case "tool_execution_update":
		activity = "tool output received: " + e.ToolName
	case "tool_execution_end":
		delete(s.viewTools, toolKey)
		activity = "tool finished: " + e.ToolName
	case "compaction_start", "auto_retry_start":
		activity = strings.ReplaceAll(rec.Type, "_", " ")
		s.status.Current = activity
	case "extension_ui_request":
		if rec.Method == "setStatus" && rec.StatusKey == "quartermaster.ask" {
			activity = "approval wait ended"
			if rec.StatusText != "" {
				activity = "waiting on approval"
			}
			s.status.Current = activity
		}
	case "response":
		if rec.Command == "get_state" && rec.Success {
			s.status.Provider, _ = rpcViewBound(e.Data.Model.Provider)
			s.status.Model, _ = rpcViewBound(e.Data.Model.ID)
		}
	}
	if e.Message.Role == "assistant" && e.Message.Model != "" {
		s.status.Provider, _ = rpcViewBound(e.Message.Provider)
		s.status.Model, _ = rpcViewBound(e.Message.Model)
	}
	if rec.Type == "tool_execution_start" || rec.Type == "tool_execution_end" {
		var names []string
		for _, name := range s.viewTools {
			names = append(names, name)
		}
		sort.Strings(names)
		s.status.Current = "agent run"
		if len(names) > 0 {
			s.status.Current = "tools: " + strings.Join(names, ", ")
		}
		if s.viewToolsOverflow {
			s.status.Current = "tool activity (tracking limit; current set unknown)"
		}
	}
	if activity == "" {
		return false
	}
	s.status.Activity, _ = rpcViewBound(activity)
	s.status.ActivityAt = now
	s.status.Current, _ = rpcViewBound(s.status.Current)
	return (rec.Type != "message_update" && rec.Type != "tool_execution_update") || now.Sub(s.status.Updated) >= time.Second
}

func rpcVisibilityLine(v rpcVisibility, now time.Time) string {
	line := v.State
	stale := !v.Observed.IsZero() && now.Sub(v.Observed) > 3*time.Second
	if stale {
		line = "stale snapshot: " + line
	}
	if v.Current != "" {
		line += " · " + v.Current
	}
	if !v.Started.IsZero() {
		until := v.Ended
		if until.IsZero() {
			if v.Connected && v.State == "running" {
				until = now
				if stale {
					until = v.Observed
				}
			} else {
				until = v.ActivityAt
			}
		}
		if !until.Before(v.Started) {
			label := "run "
			if v.Ended.IsZero() && (!v.Connected || v.State != "running" || stale) {
				label = "run observed "
			}
			line += " · " + label + age(v.Started, until)
		}
	}
	if v.ActivityAt.IsZero() {
		return line + " · activity unknown"
	}
	return line + " · last " + age(v.ActivityAt, now) + " ago"
}
