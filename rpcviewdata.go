package main

// Read-only projections over the supervisor's durable log and pi's saved
// session. None of these functions creates a directory, starts a process,
// connects to pi, or submits a command. Old supervisors remain readable.
import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"
)

const (
	rpcViewRecords   = 64
	rpcViewBytes     = 256 << 10
	rpcViewTextBytes = 16 << 10
	rpcViewScanBytes = 2 << 20
)

type rpcViewUsage struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"totalTokens"`
	Cost       struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

func (u *rpcViewUsage) add(v rpcViewUsage) {
	u.Input += v.Input
	u.Output += v.Output
	u.CacheRead += v.CacheRead
	u.CacheWrite += v.CacheWrite
	u.Total += v.Total
	u.Cost.Total += v.Cost.Total
}

type rpcViewEvent struct {
	Seq       int64         `json:"seq"` // byte position: stable across reconnect and supervisor restart
	At        time.Time     `json:"at,omitempty"`
	Kind      string        `json:"kind"`
	Label     string        `json:"label"`
	Text      string        `json:"text,omitempty"`
	EntryID   string        `json:"entry_id,omitempty"`
	ParentID  string        `json:"parent_id,omitempty"`
	ToolID    string        `json:"tool_id,omitempty"`
	Error     bool          `json:"error,omitempty"`
	Truncated bool          `json:"truncated,omitempty"`
	Provider  string        `json:"provider,omitempty"`
	Model     string        `json:"model,omitempty"`
	Usage     *rpcViewUsage `json:"usage,omitempty"`
	Links     []string      `json:"links,omitempty"`
}
type rpcViewPage struct {
	Events []rpcViewEvent `json:"events"`
	Cursor string         `json:"cursor"`
	More   bool           `json:"more"`
	Gap    string         `json:"gap,omitempty"`
	Reset  bool           `json:"reset,omitempty"`
}
type rpcViewCursor struct {
	Source   string `json:"source"`
	Identity string `json:"identity"`
	Offset   int64  `json:"offset"`
	Anchor   string `json:"anchor,omitempty"`
}

// No ANSI, C0/C1, bidi overrides, zero-width formatting or terminal hyperlinks
// from worker output survive. The UI adds its own styles after this boundary.
func rpcViewSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}

var rpcViewCredentials = []*regexp.Regexp{
	// A cookie header is a list: removing only its first assignment leaks
	// opaque names such as sid in the rest of the header.
	regexp.MustCompile(`(?i)\b(?:set-cookie|cookie)\s*:\s*[^\r\n]+`),
	regexp.MustCompile(`(?is)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`),
	regexp.MustCompile(`(?i)\b(?:Bearer|Basic)\s+[A-Za-z0-9+/_=.\-]+`),
	regexp.MustCompile(`(?i)(?:[a-z0-9_.-]*(?:password|passwd|secret|token|api[_-]?key|authorization|cookie|credential|signature)[a-z0-9_.-]*)["']?\s*[:=]\s*(?:"[^"\n]*"|'[^'\n]*'|[^\s,;&]+)`),
	regexp.MustCompile(`(?i)https?://[^\s/@]+:[^\s/@]+@`),
	regexp.MustCompile(`\b(?:sk-[a-zA-Z0-9_-]{12,}|gh[pousr]_[a-zA-Z0-9_]{12,}|github_pat_[a-zA-Z0-9_]+|xox[baprs]-[a-zA-Z0-9-]+|(?:AKIA|ASIA)[A-Z0-9]{16}|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)\b`),
}

func rpcViewRedact(s string) string {
	s = rpcViewSafe(s)
	for _, re := range rpcViewCredentials {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	return s
}
func rpcViewBound(s string) (string, bool) {
	s = rpcViewRedact(s)
	if len(s) <= rpcViewTextBytes {
		return s, false
	}
	s = strings.ToValidUTF8(s[:rpcViewTextBytes], "")
	return s + "\n[output truncated]", true
}

var rpcViewURL = regexp.MustCompile(`https?://[^\s<>"\x60]+`)

// Only text blocks are projected. Provider signatures, images, custom data,
// system prompts, headers, tool details and streaming deltas are not copied.
// Redact complete values BEFORE truncating, never token deltas (a credential
// can straddle any number of deltas).
func rpcViewContent(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Name string `json:"name"`
	}
	_ = json.Unmarshal(raw, &blocks)
	var out []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, b.Text)
		case "toolCall":
			out = append(out, "[tool call: "+b.Name+"]")
		case "image":
			out = append(out, "[image omitted]")
		}
	}
	return strings.Join(out, "\n")
}

type rpcViewMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	ToolName     string          `json:"toolName"`
	ToolID       string          `json:"toolCallId"`
	IsError      bool            `json:"isError"`
	ErrorMessage string          `json:"errorMessage"`
	StopReason   string          `json:"stopReason"`
	Provider     string          `json:"provider"`
	Model        string          `json:"model"`
	Usage        *rpcViewUsage   `json:"usage"`
	Timestamp    int64           `json:"timestamp"`
}

func rpcViewNormalize(raw []byte, source string, seq int64) (rpcViewEvent, bool) {
	var v struct {
		Type      string    `json:"type"`
		ID        string    `json:"id"`
		ParentID  string    `json:"parentId"`
		Timestamp time.Time `json:"timestamp"`
		QM        struct {
			At time.Time `json:"at"`
		} `json:"_quartermaster"`
		Message  rpcViewMessage  `json:"message"`
		Content  json.RawMessage `json:"content"`
		ToolName string          `json:"toolName"`
		ToolID   string          `json:"toolCallId"`
		Args     json.RawMessage `json:"args"`
		Result   struct {
			Content json.RawMessage `json:"content"`
		} `json:"result"`
		IsError      bool          `json:"isError"`
		ErrorMessage string        `json:"errorMessage"`
		Method       string        `json:"method"`
		StatusKey    string        `json:"statusKey"`
		StatusText   string        `json:"statusText"`
		Summary      string        `json:"summary"`
		Provider     string        `json:"provider"`
		Model        string        `json:"model"`
		ModelID      string        `json:"modelId"`
		Usage        *rpcViewUsage `json:"usage"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return rpcViewEvent{Seq: seq, Kind: "gap", Label: "unreadable record omitted"}, true
	}
	e := rpcViewEvent{Seq: seq, At: v.QM.At, Kind: v.Type}
	if source == "history" {
		e.At, e.EntryID, e.ParentID = v.Timestamp, v.ID, v.ParentID
	}
	switch v.Type {
	case "message", "message_end":
		m := v.Message
		switch m.Role {
		case "user", "assistant", "toolResult", "custom":
		default:
			return e, false
		}
		e.Label = m.Role
		if m.ToolName != "" {
			e.Label += " " + m.ToolName
		}
		e.ToolID, e.Error, e.Provider, e.Model = m.ToolID, m.IsError || m.StopReason == "error" || m.StopReason == "aborted", m.Provider, m.Model
		e.Text = rpcViewContent(m.Content)
		if m.ErrorMessage != "" {
			e.Text += "\n" + m.ErrorMessage
		}
		if e.At.IsZero() && m.Timestamp > 0 {
			e.At = time.UnixMilli(m.Timestamp)
		}
		if source == "history" {
			e.Usage = m.Usage
		}
	case "custom_message":
		e.Label, e.Text = "saved extension message", rpcViewContent(v.Content)
	case "compaction", "branch_summary":
		e.Label, e.Text, e.Usage = v.Type, v.Summary, v.Usage
	case "usage":
		e.Label, e.Usage, e.Provider, e.Model = "recorded usage", v.Usage, v.Provider, v.Model
	case "model_change":
		e.Label, e.Provider, e.Model = "model selected", v.Provider, v.ModelID
	case "tool_execution_start":
		e.Label, e.ToolID = "tool start: "+v.ToolName, v.ToolID
		// Arguments can contain arbitrary nested secrets; retain no opaque JSON.
		var args struct {
			Command string `json:"command"`
			Path    string `json:"path"`
		}
		_ = json.Unmarshal(v.Args, &args)
		e.Text = args.Command
		if args.Path != "" {
			e.Text += "\npath: " + args.Path
		}
	case "tool_execution_end":
		e.Label, e.ToolID, e.Text, e.Error = "tool result: "+v.ToolName, v.ToolID, rpcViewContent(v.Result.Content), v.IsError
	case "agent_start", "agent_settled", "turn_start", "compaction_start", "compaction_end", "auto_retry_start", "auto_retry_end":
		e.Label = strings.ReplaceAll(v.Type, "_", " ")
	case "extension_ui_request":
		if v.Method != "setStatus" || v.StatusKey != "quartermaster.ask" {
			return e, false
		}
		e.Label = "approval wait ended"
		if v.StatusText != "" {
			e.Label = "waiting on approval"
		}
	default:
		return e, false
	}
	e.Text, e.Truncated = rpcViewBound(e.Text)
	e.Label, _ = rpcViewBound(e.Label)
	e.Provider, _ = rpcViewBound(e.Provider)
	e.Model, _ = rpcViewBound(e.Model)
	e.ToolID, _ = rpcViewBound(e.ToolID)
	e.EntryID, _ = rpcViewBound(e.EntryID)
	e.ParentID, _ = rpcViewBound(e.ParentID)
	for _, u := range rpcViewURL.FindAllString(e.Text, 16) {
		// Query strings, fragments and userinfo are never made into links.
		u = strings.TrimRight(u, ").,]")
		if !strings.ContainsAny(u, "?@#") && !strings.Contains(u, "[redacted]") {
			e.Links = append(e.Links, u)
		}
	}
	return e, true
}

func rpcViewPath(id, source string) (string, error) {
	if !rpcIDRE.MatchString(id) || !rpcExists(id) {
		return "", errors.New("no native worker reservation")
	}
	if source == "events" {
		return filepath.Join(rpcDir(id), "events.jsonl"), nil
	}
	if source != "history" {
		return "", errors.New("source must be history or events")
	}
	var st rpcStatus
	if err := rpcLoad(filepath.Join(rpcDir(id), "status.json"), &st); err == nil {
		if filepath.IsAbs(st.SessionFile) {
			return st.SessionFile, nil
		}
		return "", errors.New("saved session identity unavailable")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("saved status unreadable; viewing never repairs metadata")
	}
	var w rpcWorker
	if rpcLoad(filepath.Join(rpcDir(id), "worker.json"), &w) == nil && w.Adoption != nil && filepath.IsAbs(w.Adoption.SessionFile) {
		return w.Adoption.SessionFile, nil
	}
	return "", errors.New("saved session identity unavailable; viewing never launches a worker")
}

// A cursor binds the source, file identity and byte offset. Each call opens a
// fresh read-only descriptor and snapshots its size: slow readers retain no
// server resources and cannot backpressure pi. Limits include skipped records.
func rpcViewRead(id, source, cursor string) (rpcViewPage, error) {
	page := rpcViewPage{Events: []rpcViewEvent{}}
	path, err := rpcViewPath(id, source)
	if err != nil {
		return page, err
	}
	f, err := os.Open(path)
	if err != nil {
		return page, errors.New("history/log unavailable (no worker was started)")
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return page, err
	}
	if !fi.Mode().IsRegular() {
		return page, errors.New("not a regular history file")
	}
	// Inode identity plus path prevents applying a cursor to another worker or
	// replacement file. Append does not change this identity.
	identity := rpcViewFileIdentity(path, fi)
	cur := rpcViewCursor{Source: source, Identity: identity}
	if cursor != "" {
		if len(cursor) > 8192 {
			return page, errors.New("invalid view cursor")
		}
		b, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil || len(b) > 4096 || json.Unmarshal(b, &cur) != nil || cur.Offset < 0 {
			return page, errors.New("invalid view cursor")
		}
		if cur.Source != source || cur.Identity != identity || cur.Offset > fi.Size() || (cur.Anchor != "" && cur.Anchor != rpcViewAnchor(f, cur.Offset)) {
			page.Reset = true
			page.Gap = "history replaced/truncated or cursor belongs to another source; replay restarted"
			cur = rpcViewCursor{Source: source, Identity: identity}
		}
	}
	start := cur.Offset
	recordLimit := rpcMaxRecord
	if source == "events" {
		recordLimit = rpcLogMaxRecord
	}
	r := bufio.NewReaderSize(io.NewSectionReader(f, start, fi.Size()-start), 64<<10)
	size := 0
	partial := false
	for scanned := 0; scanned < 256 && len(page.Events) < rpcViewRecords && size < rpcViewBytes && cur.Offset-start < rpcViewScanBytes; scanned++ {
		var line []byte
		for {
			part, e := r.ReadSlice('\n')
			if len(line)+len(part) > recordLimit {
				return page, errors.New("history record exceeds 16 MiB; replay refused")
			}
			line = append(line, part...)
			if e == bufio.ErrBufferFull {
				continue
			}
			if e != nil {
				if e != io.EOF {
					return page, e
				}
				// An incomplete append is retried from the same cursor, never consumed.
				if len(line) > 0 {
					partial = true
					page.Gap = "incomplete final record; waiting for a complete append"
				}
				goto complete
			}
			break
		}
		seq := cur.Offset
		cur.Offset += int64(len(line))
		if e, ok := rpcViewNormalize(bytes.TrimSpace(line), source, seq); ok {
			b, _ := json.Marshal(e)
			size += len(b)
			page.Events = append(page.Events, e)
		}
	}
complete:
	cur.Anchor = rpcViewAnchor(f, cur.Offset)
	b, _ := json.Marshal(cur)
	page.Cursor = base64.RawURLEncoding.EncodeToString(b)
	page.More = cur.Offset < fi.Size() && !partial
	return page, nil
}
func rpcViewAnchor(f *os.File, offset int64) string {
	b := make([]byte, min(offset, 128))
	n, _ := f.ReadAt(b, offset-int64(len(b)))
	prefix := make([]byte, min(offset, 256))
	pn, _ := f.ReadAt(prefix, 0)
	return fmt.Sprintf("%x", sha256.Sum256(append(prefix[:pn], b[:n]...)))
}
func rpcViewInode(fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fi.Name()
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}
func rpcViewFileIdentity(path string, fi os.FileInfo) string {
	// os.SameFile's identity fields are platform-specific; use the helper shared
	// by supported Unix builds, without incorporating mutable size/mtime.
	return fmt.Sprintf("%x", sha256.Sum256([]byte(path+"\x00"+rpcViewInode(fi))))
}
