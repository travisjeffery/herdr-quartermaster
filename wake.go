package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"
)

// AgentSession is the conversation herdr reports for an agent's pane.
type AgentSession struct {
	Agent string `json:"agent"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// claudeHome is Claude Code's config directory, where it keeps transcripts.
func claudeHome() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// promptedSince reports whether a person (or anything typing into the pane, as
// a person would) prompted a's conversation after since, as opposed to the
// agent waking on its own: Claude Code compacts an idle conversation and
// reports finished background tasks, and both make the pane work again. pi's
// session file answers the same question for a pi worker (piPromptedSince).
// known is false when it can't tell: another kind of agent, or no transcript
// found.
func promptedSince(a *Agent, since time.Time) (prompted, known bool) {
	if a != nil && !since.IsZero() && a.Session.Agent == "pi" {
		f, err := os.Open(piSessionFile(a))
		if err != nil {
			return false, false
		}
		defer f.Close()
		return piPromptedSince(f, since), true
	}
	if a == nil || since.IsZero() || a.Session.Agent != "claude" || a.Session.Kind != "id" || a.Session.Value == "" {
		return false, false
	}
	paths, _ := filepath.Glob(filepath.Join(claudeHome(), "projects", "*", a.Session.Value+".jsonl"))
	if len(paths) != 1 {
		return false, false
	}
	f, err := os.Open(paths[0])
	if err != nil {
		return false, false
	}
	defer f.Close()
	return humanPromptAfter(f, since), true
}

// humanPromptAfter scans a Claude Code transcript for a user prompt, written
// after since, that came from the input box: origin kind "human". Compaction
// summaries carry no origin and task notifications carry their own kind.
func humanPromptAfter(r io.Reader, since time.Time) bool {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"human"`)) {
			var e struct {
				Type      string    `json:"type"`
				IsMeta    bool      `json:"isMeta"`
				Timestamp time.Time `json:"timestamp"`
				Origin    struct {
					Kind string `json:"kind"`
				} `json:"origin"`
			}
			if json.Unmarshal(line, &e) == nil && e.Type == "user" && !e.IsMeta && e.Origin.Kind == "human" && e.Timestamp.After(since) {
				return true
			}
		}
		if err != nil {
			return false
		}
	}
}
