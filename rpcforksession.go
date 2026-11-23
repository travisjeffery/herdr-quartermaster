package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

const rpcForkMaxSession = 64 << 20

// A fork selects an explicit completed assistant checkpoint, not an inferred
// active leaf or a prompt to replay. Keep raw ancestor entries (including
// compaction, context edits and opaque extension state) byte-for-byte.
func rpcForkSession(data []byte, source rpcStatus, cwd, newCwd, newID, through, context string) ([]byte, error) {
	if len(context) > 1<<20 {
		return nil, errors.New("fork destination context exceeds 1 MiB")
	}
	if len(data) > rpcForkMaxSession {
		return nil, errors.New("fork source exceeds 64 MiB; no partial history is copied")
	}
	type entry struct {
		Type     string  `json:"type"`
		ID       string  `json:"id"`
		ParentID *string `json:"parentId"`
		Message  struct {
			Role       string          `json:"role"`
			StopReason string          `json:"stopReason"`
			ToolCallID string          `json:"toolCallId"`
			Content    json.RawMessage `json:"content"`
		} `json:"message"`
	}
	entries := map[string]entry{}
	raw := map[string][]byte{}
	header := true
	err := readRPC(bytes.NewReader(data), func(line []byte) error {
		if header {
			header = false
			var h struct {
				Type, ID, Cwd string
				Version       int
			}
			if err := json.Unmarshal(line, &h); err != nil {
				return err
			}
			if h.Type != "session" || h.Version != 3 || h.ID != source.SessionID || h.Cwd != cwd || newID == h.ID {
				return errors.New("fork requires exact source v3 session identity/cwd and a new identity")
			}
			return nil
		}
		var e entry
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		if err := json.Unmarshal(line, &fields); err != nil {
			return err
		}
		if e.Type == "session" || e.ID == "" || fields["parentId"] == nil || raw[e.ID] != nil {
			return errors.New("invalid or duplicate fork entry")
		}
		if e.ParentID != nil && raw[*e.ParentID] == nil {
			return errors.New("fork entry has a missing/forward parent")
		}
		entries[e.ID], raw[e.ID] = e, bytes.Clone(line)
		return nil
	})
	if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read fork source: %w", err)
	}
	selected, ok := entries[through]
	if !ok || selected.Type != "message" || selected.Message.Role != "assistant" || selected.Message.StopReason != "stop" {
		return nil, errors.New("fork checkpoint must be an explicit completed assistant message with stopReason=stop")
	}
	var branch []entry
	for e := selected; ; e = entries[*e.ParentID] {
		branch = append(branch, e)
		if e.ParentID == nil {
			break
		}
	}
	pending := map[string]bool{}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := enc.Encode(map[string]any{"type": "session", "version": 3, "id": newID, "cwd": newCwd, "timestamp": now, "parentSession": source.SessionFile}); err != nil {
		return nil, err
	}
	for i := len(branch) - 1; i >= 0; i-- {
		e := branch[i]
		if e.Type == "message" && e.Message.Role == "assistant" {
			var content []struct{ Type, ID string }
			if err := json.Unmarshal(e.Message.Content, &content); err != nil {
				return nil, fmt.Errorf("invalid assistant content: %w", err)
			}
			for _, c := range content {
				if c.Type == "toolCall" {
					if c.ID == "" || pending[c.ID] {
						return nil, errors.New("ambiguous tool call in fork history")
					}
					pending[c.ID] = true
				}
			}
		}
		if e.Type == "message" && e.Message.Role == "toolResult" {
			if !pending[e.Message.ToolCallID] {
				return nil, errors.New("orphan tool result in fork history")
			}
			delete(pending, e.Message.ToolCallID)
		}
		out.Write(raw[e.ID])
		out.WriteByte('\n')
	}
	if len(pending) != 0 {
		return nil, errors.New("fork checkpoint has unfinished tool calls; no work is replayed")
	}
	// This is stored context, not a dispatched user turn. The new task must
	// explicitly activate and tell its parked worker what to do next.
	if err := enc.Encode(map[string]any{"type": "custom_message", "id": rpcID(), "parentId": through, "timestamp": now, "customType": "quartermaster.fork", "content": context, "display": true}); err != nil {
		return nil, err
	}
	if out.Len() > rpcForkMaxSession+(1<<20) {
		return nil, errors.New("fork seed exceeds 65 MiB; no reservation published")
	}
	return out.Bytes(), nil
}
