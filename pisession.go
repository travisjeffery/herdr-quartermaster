package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// pi keeps each conversation as an append-only JSONL tree of entries with
// stable ids (docs/session-format.md in pi). quartermaster reads that file,
// not the pane, for what happened in a pi worker's conversation. It reads the
// file rather than talking to pi over RPC: a worker runs pi's TUI in a herdr
// pane, which RPC mode replaces, and the file is pi's own durable record,
// written as each entry is appended, readable whether pi is running or not.

// piInputType is the custom entry the quartermaster pi extension
// (pi/quartermaster.ts) appends for every prompt, carrying where it came from:
// "interactive" (typed into the TUI, as herdr agent prompt does), "rpc",
// "quartermaster" (sent over the control socket, picontrol.go), or
// "extension" (pi.sendUserMessage from any other extension). Custom entries
// never reach the model.
const piInputType = "quartermaster.input"

// piSessionDir is where pi keeps sessions: PI_CODING_AGENT_SESSION_DIR, else
// ~/.pi/agent/sessions. A sessionDir setting or --session-dir isn't read.
func piSessionDir() string {
	if d := os.Getenv("PI_CODING_AGENT_SESSION_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pi", "agent", "sessions")
}

// piSessionFile is the session file of a's pi conversation: herdr's pi
// extension reports its path, or its id when pi has no file yet.
func piSessionFile(a *Agent) string {
	if a == nil || a.Session.Agent != "pi" || a.Session.Value == "" {
		return ""
	}
	if a.Session.Kind == "path" {
		if filepath.IsAbs(a.Session.Value) {
			return a.Session.Value
		}
		return ""
	}
	return piSessionByID(a.Session.Value)
}

// piSessionByID finds the one session file named <timestamp>_<id>.jsonl.
func piSessionByID(id string) string {
	if id == "" || strings.ContainsAny(id, "/*?[") {
		return ""
	}
	paths, _ := filepath.Glob(filepath.Join(piSessionDir(), "*", "*_"+id+".jsonl"))
	if len(paths) != 1 {
		return ""
	}
	return paths[0]
}

// piEntry is the part of a session entry quartermaster reads.
type piEntry struct {
	Type       string    `json:"type"`
	ID         string    `json:"id"`
	Timestamp  time.Time `json:"timestamp"`
	CustomType string    `json:"customType"`
	Data       struct {
		Source string `json:"source"`
	} `json:"data"`
	Message struct {
		Role         string `json:"role"`
		StopReason   string `json:"stopReason"`
		ErrorMessage string `json:"errorMessage"`
	} `json:"message"`
}

// piPromptedSince is promptedSince for a pi session: a prompt typed, sent
// over RPC or sent by quartermaster over the control socket after since,
// rather than pi waking on its own (an extension's message or
// pi.sendUserMessage). When the quartermaster extension's input
// entries are there they decide; a session without them falls back to any
// user message after since, which counts an extension's sendUserMessage as a
// prompt, as the old behaviour did.
func piPromptedSince(r io.Reader, since time.Time) bool {
	marked, markedPrompt, userAfter := false, false, false
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if bytes.Contains(line, []byte(piInputType)) || bytes.Contains(line, []byte(`"role":"user"`)) {
			var e piEntry
			if json.Unmarshal(line, &e) == nil {
				switch {
				case e.Type == "custom" && e.CustomType == piInputType:
					marked = true
					if e.Timestamp.After(since) && e.Data.Source != "extension" {
						markedPrompt = true
					}
				case e.Type == "message" && e.Message.Role == "user" && e.Timestamp.After(since):
					userAfter = true
				}
			}
		}
		if err != nil {
			break
		}
	}
	if marked {
		return markedPrompt
	}
	return userAfter
}

// PiTurn is how a pi worker's last turn ended: its last assistant message
// that wasn't a tool call. ID is the entry's id, a durable cursor: a new ID
// means a new turn ended, however briefly the agent showed working.
type PiTurn struct {
	ID         string
	At         time.Time
	StopReason string // stop, length, error or aborted
	Error      string
}

// Failed reports a turn that ended on an error or was aborted rather than
// finishing.
func (t *PiTurn) Failed() bool {
	return t != nil && (t.StopReason == "error" || t.StopReason == "aborted")
}

// piTailBytes bounds how much of a session file the ticker reads each pass
// for the last turn: assistant entries are small next to tool output.
const piTailBytes = 1 << 20

// piLastTurn reads the end of a's pi session for its last turn end; nil when
// it can't find one.
func piLastTurn(a *Agent) *PiTurn {
	path := piSessionFile(a)
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > piTailBytes {
		if _, err := f.Seek(fi.Size()-piTailBytes, io.SeekStart); err != nil {
			return nil
		}
	}
	return lastTurnIn(f)
}

// lastTurnIn scans session entries for the last assistant message with a
// terminal stop reason. A partial first line from a seek fails to parse and
// is skipped.
func lastTurnIn(r io.Reader) *PiTurn {
	var last *PiTurn
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"role":"assistant"`)) {
			var e piEntry
			if json.Unmarshal(line, &e) == nil && e.Type == "message" && e.Message.Role == "assistant" {
				switch e.Message.StopReason {
				case "stop", "length", "error", "aborted":
					last = &PiTurn{ID: e.ID, At: e.Timestamp, StopReason: e.Message.StopReason, Error: e.Message.ErrorMessage}
				}
			}
		}
		if err != nil {
			return last
		}
	}
}
