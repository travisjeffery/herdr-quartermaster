package main

// Native pi transport. A supervisor, not a coordinator/client, owns these
// pipes. No network client can write raw RPC commands or switch sessions.
import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

const rpcMaxRecord = 16 << 20
const rpcCommandTimeout = 15 * time.Second

type rpcRecord struct {
	Type       string          `json:"type"`
	ID         string          `json:"id,omitempty"`
	Command    string          `json:"command,omitempty"`
	Success    bool            `json:"success,omitempty"`
	Error      string          `json:"error,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
	Method     string          `json:"method,omitempty"`
	Title      string          `json:"title,omitempty"`
	Message    json.RawMessage `json:"message,omitempty"`
	StatusKey  string          `json:"statusKey,omitempty"`
	StatusText string          `json:"statusText,omitempty"`
}

// readRPC splits only on LF (including CRLF), never Unicode separators. A
// partial final record is a protocol failure, not a completed response.
func readRPC(r io.Reader, consume func([]byte) error) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var line []byte
	for {
		part, err := br.ReadSlice('\n')
		if len(line)+len(part) > rpcMaxRecord {
			return errors.New("pi RPC record exceeds 16 MiB")
		}
		line = append(line, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if len(line) > 0 {
				return fmt.Errorf("partial pi RPC record at EOF: %w", io.ErrUnexpectedEOF)
			}
			return err
		}
		line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte{'\n'}), []byte{'\r'})
		if !json.Valid(line) || len(line) == 0 || line[0] != '{' {
			return errors.New("invalid pi RPC JSON object")
		}
		if err := consume(line); err != nil {
			return err
		}
		line = nil
	}
}

type rpcResult struct {
	record rpcRecord
	err    error
}
type rpcProcess struct {
	cmd          *exec.Cmd
	stdin        *os.File
	mu           sync.Mutex
	pending      map[string]chan rpcResult
	writeMu      sync.Mutex
	writeTimeout time.Duration
	done         chan struct{}
	exited       chan struct{}
	drained      chan struct{}
	stopOnce     sync.Once
	endOnce      sync.Once
	err          error
	observe      func([]byte, rpcRecord) error
}

// startRPC starts the drain before the first command; stderr is never parsed.
// The inherited lease keeps a second supervisor out even if this one dies
// while pi is still shutting down from stdin EOF.
func startRPC(cmd *exec.Cmd, lease *os.File, stderr io.Writer, observe func([]byte, rpcRecord) error) (*rpcProcess, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, stderr
	if lease != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, lease)
	}
	if err = cmd.Start(); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, err
	}
	inR.Close()
	outW.Close()
	p := &rpcProcess{cmd: cmd, stdin: inW, pending: map[string]chan rpcResult{}, writeTimeout: rpcCommandTimeout, done: make(chan struct{}), exited: make(chan struct{}), drained: make(chan struct{}), observe: observe}
	go func() {
		defer close(p.drained)
		err := readRPC(outR, func(raw []byte) error {
			var rec rpcRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				return err
			}
			if rec.Type == "" {
				return errors.New("pi RPC record has no type")
			}
			if p.observe != nil {
				if err := p.observe(raw, rec); err != nil {
					return err
				}
			}
			if rec.Type == "response" {
				p.mu.Lock()
				ch := p.pending[rec.ID]
				delete(p.pending, rec.ID)
				p.mu.Unlock()
				if ch != nil {
					ch <- rpcResult{record: rec}
				}
			}
			return nil
		})
		outR.Close()
		p.finish(fmt.Errorf("pi stdout closed: %w", err))
		// Bad framing or EOF with a process still running must not leave an
		// unobserved worker doing work. Close stdin, then bounded shutdown.
		p.stop()
	}()
	go func() {
		err := cmd.Wait()
		close(p.exited)
		// Do not let a descendant holding stdout open wedge supervisor exit.
		timer := time.AfterFunc(time.Second, func() { _ = outR.Close() })
		defer timer.Stop()
		<-p.drained
		if err == nil {
			err = errors.New("pi exited")
		}
		p.finish(err)
	}()
	return p, nil
}
func (p *rpcProcess) finish(err error) {
	p.endOnce.Do(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.err = err
		close(p.done)
		for id, ch := range p.pending {
			ch <- rpcResult{err: err}
			delete(p.pending, id)
		}
	})
}

// write serializes complete records, with a pipe deadline for backpressure.
// A write failure is ambiguous: some/all bytes may have reached pi.
func (p *rpcProcess) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > rpcMaxRecord {
		return errors.New("command too large")
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err = p.stdin.SetWriteDeadline(time.Now().Add(p.writeTimeout)); err != nil {
		return err
	}
	b = append(b, '\n')
	n, err := p.stdin.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err != nil {
		p.finish(fmt.Errorf("pi stdin write failed (delivery unknown): %w", err))
		p.stop()
	}
	return err
}
func (p *rpcProcess) call(id, typ string, fields map[string]any, timeout time.Duration) (rpcRecord, error) {
	ch := make(chan rpcResult, 1)
	p.mu.Lock()
	select {
	case <-p.done:
		err := p.err
		p.mu.Unlock()
		return rpcRecord{}, err
	default:
	}
	if _, exists := p.pending[id]; exists {
		p.mu.Unlock()
		return rpcRecord{}, errors.New("request already pending")
	}
	p.pending[id] = ch
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, id); p.mu.Unlock() }()
	req := map[string]any{"id": id, "type": typ}
	for k, v := range fields {
		req[k] = v
	}
	if err := p.write(req); err != nil {
		return rpcRecord{}, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		if r.err != nil {
			return r.record, r.err
		}
		if r.record.Command != typ {
			return r.record, errors.New("pi response command mismatch")
		}
		if !r.record.Success {
			return r.record, fmt.Errorf("pi %s: %s", typ, r.record.Error)
		}
		return r.record, nil
	case <-timer.C:
		return rpcRecord{}, fmt.Errorf("pi %s timed out; delivery unknown (request %s); inspect status/events, do not replay", typ, id)
	}
}
func (p *rpcProcess) stop() {
	p.stopOnce.Do(func() {
		p.stdin.Close()
		go func() {
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			select {
			case <-p.exited:
			case <-timer.C:
				_ = p.cmd.Process.Kill()
			}
		}()
	})
}
