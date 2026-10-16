package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// bd-proxy is the forced command behind a worker host's Beads key on the
// desktop (docs/design/worker-hosts.md §5):
//
//	command="quartermaster bd-proxy --host server",restrict ssh-ed25519 AAAA… qm-beads@server
//
// The host's bd shim sends its arguments, each single-quoted, as the SSH
// command; sshd hands them over in SSH_ORIGINAL_COMMAND. bd-proxy splits them
// without a shell, refuses anything but an allowlisted bd subcommand and the
// flags that would point bd at another database, pins BEADS_DIR to the one
// configured here and execs bd, so stdin, stdout and the exit code pass
// straight through. The host name comes from the key line, never the request.

// bdProxyCommands are the bd subcommands a worker may run: reading and
// updating issues, notes, labels, dependencies and memories. Everything else
// (dolt, sync, init, config, flatten, gc, sql, delete, edit, ...) is refused.
var bdProxyCommands = []string{
	"assign", "blocked", "children", "close", "comment", "comments", "count",
	"create", "defer", "dep", "duplicate", "forget", "graph", "heartbeat",
	"help", "history", "label", "link", "list", "memories", "note", "ping",
	"prime", "priority", "q", "query", "ready", "recall", "remember", "reopen",
	"search", "show", "stale", "status", "statuses", "supersede", "tag",
	"types", "unclaim", "undefer", "update", "version",
}

// bdProxyGlobals are the global flags allowed ahead of the subcommand; any
// other flag there is refused.
var bdProxyGlobals = []string{"--json", "-q", "--quiet", "-v", "--verbose", "--no-color", "--readonly", "--sandbox"}

// bdProxyDenied are bd's persistent flags that would leave the pinned
// database or write files on the desktop. Persistent flags parse anywhere on
// the command line, so they are refused anywhere before a "--".
var bdProxyDenied = []string{
	"--db", "--database", "--directory", "--global", "--dolt-auto-commit",
	"--cpu-profile", "--mem-profile", "--ignore-schema-skew",
}

var hostNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// bdRequest is what the proxy decided to run.
type bdRequest struct {
	Command string   // the bd subcommand
	Actor   string   // --actor as sent, else the host
	Argv    []string // bd's arguments, without the program name
}

// planBD checks a request's words and returns the bd invocation for it. An
// optional leading "bd" is dropped; a request without --actor acts as host.
func planBD(host string, words []string) (bdRequest, error) {
	if len(words) > 0 && words[0] == "bd" {
		words = words[1:]
	}
	var req bdRequest
	i := 0
	for ; i < len(words) && strings.HasPrefix(words[i], "-"); i++ {
		w := words[i]
		switch {
		case w == "--actor":
			if i+1 >= len(words) {
				return req, errors.New("--actor needs a value")
			}
			i++
			req.Actor = words[i]
		case strings.HasPrefix(w, "--actor="):
			req.Actor = strings.TrimPrefix(w, "--actor=")
		case slices.Contains(bdProxyGlobals, w):
		default:
			return req, fmt.Errorf("flag %s is not allowed before the subcommand", w)
		}
	}
	if i == len(words) {
		return req, errors.New("no bd subcommand")
	}
	req.Command = words[i]
	if !slices.Contains(bdProxyCommands, req.Command) {
		return req, fmt.Errorf("bd %s is not allowed through the proxy", req.Command)
	}
	for _, w := range words[i+1:] {
		if w == "--" {
			break
		}
		if f := deniedFlag(w); f != "" {
			return req, fmt.Errorf("flag %s is not allowed through the proxy", f)
		}
		if w == "--actor" || strings.HasPrefix(w, "--actor=") {
			return req, errors.New("--actor goes before the subcommand")
		}
	}
	if req.Actor == "" {
		req.Actor = host
	}
	// --actor is re-added in one place so the logged actor is the one bd uses.
	req.Argv = append([]string{"--actor", req.Actor}, dropActor(words[:i])...)
	req.Argv = append(req.Argv, words[i:]...)
	return req, nil
}

// dropActor removes --actor and its value from the global flags.
func dropActor(globals []string) []string {
	var out []string
	for i := 0; i < len(globals); i++ {
		switch {
		case globals[i] == "--actor":
			i++
		case strings.HasPrefix(globals[i], "--actor="):
		default:
			out = append(out, globals[i])
		}
	}
	return out
}

// deniedFlag is the denied persistent flag w sets, "" if none. -C (the short
// --directory) counts inside a cluster of short flags such as -qC/tmp. Words
// are checked without knowing which are flag values, so a value that looks
// like one of these flags is refused too: failing closed is the point.
func deniedFlag(w string) string {
	if name, ok := strings.CutPrefix(w, "--"); ok {
		name, _, _ = strings.Cut(name, "=")
		if slices.Contains(bdProxyDenied, "--"+name) {
			return "--" + name
		}
		return ""
	}
	if strings.HasPrefix(w, "-") && strings.Contains(w, "C") {
		return "-C"
	}
	return ""
}

// splitCommand splits an SSH command into words the way a POSIX shell would
// for quoting alone: single quotes, double quotes and backslashes. Nothing is
// expanded, so characters a shell would act on are refused unquoted (and $
// and ` inside double quotes) rather than passed on with a different meaning.
func splitCommand(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, errors.New("unterminated single quote")
			}
			cur.WriteString(s[i+1 : i+1+end])
			i += end + 1
			inWord = true
		case c == '"':
			inWord = true
			for i++; ; i++ {
				if i >= len(s) {
					return nil, errors.New("unterminated double quote")
				}
				d := s[i]
				if d == '"' {
					break
				}
				if d == '$' || d == '`' {
					return nil, fmt.Errorf("%c is not allowed in double quotes", d)
				}
				if d == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`\n", s[i+1]) >= 0 {
					i++
					if s[i] == '\n' {
						continue
					}
					d = s[i]
				}
				cur.WriteByte(d)
			}
		case c == '\\':
			if i+1 >= len(s) {
				return nil, errors.New("trailing backslash")
			}
			i++
			if s[i] != '\n' {
				cur.WriteByte(s[i])
				inWord = true
			}
		case strings.IndexByte("$`;|&<>(){}*?[]~#!", c) >= 0:
			return nil, fmt.Errorf("unquoted %c is not allowed", c)
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

// bdProxyEnv is environ with every BEADS_ variable replaced by the pinned
// database and the request's actor.
func bdProxyEnv(environ []string, beadsDir, actor string) []string {
	env := slices.DeleteFunc(slices.Clone(environ), func(kv string) bool { return strings.HasPrefix(kv, "BEADS_") })
	return append(env, "BEADS_DIR="+beadsDir, "BEADS_ACTOR="+actor)
}

func bdProxyLog() string { return filepath.Join(stateDir(), "bd-proxy.log") }

// logBDProxy appends one line per request: when, which host, the actor, the
// subcommand and the verdict. Arguments are not logged; notes can be long.
func logBDProxy(host string, req bdRequest, verdict string) {
	f, err := os.OpenFile(bdProxyLog(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s host=%s actor=%q cmd=%q %s\n", time.Now().UTC().Format(time.RFC3339), host, req.Actor, req.Command, verdict)
}

// bdProxy runs as the forced command: quartermaster bd-proxy --host <name>
// [--bd <path>]. It returns only on refusal or failure; otherwise bd replaces it.
func bdProxy(cfg Config, args []string) error {
	fs := flag.NewFlagSet("bd-proxy", flag.ContinueOnError)
	host := fs.String("host", "", "the worker host this key belongs to (from authorized_keys)")
	bdPath := fs.String("bd", "", "bd binary (default: bd on PATH)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !hostNameRE.MatchString(*host) {
		return fmt.Errorf("bd-proxy: --host %q is not a host name", *host)
	}
	if cfg.BeadsDir == "" {
		return errors.New("bd-proxy: no beads_dir configured")
	}
	words, err := splitCommand(os.Getenv("SSH_ORIGINAL_COMMAND"))
	if err == nil {
		var req bdRequest
		req, err = planBD(*host, words)
		if err == nil {
			return execBD(*host, *bdPath, cfg.BeadsDir, req)
		}
		logBDProxy(*host, req, "denied: "+err.Error())
	} else {
		logBDProxy(*host, bdRequest{}, "denied: "+err.Error())
	}
	return fmt.Errorf("bd-proxy: %w", err)
}

func execBD(host, bdPath, beadsDir string, req bdRequest) error {
	if bdPath == "" {
		p, err := exec.LookPath("bd")
		if err != nil {
			logBDProxy(host, req, "failed: bd not on PATH")
			return errors.New("bd-proxy: bd is not on PATH; pass --bd in the authorized_keys command")
		}
		bdPath = p
	}
	logBDProxy(host, req, "allowed")
	env := bdProxyEnv(os.Environ(), beadsDir, req.Actor)
	if err := syscall.Exec(bdPath, append([]string{"bd"}, req.Argv...), env); err != nil {
		logBDProxy(host, req, "failed: "+err.Error())
		return fmt.Errorf("bd-proxy: exec %s: %w", bdPath, err)
	}
	return nil
}
