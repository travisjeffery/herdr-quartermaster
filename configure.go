package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// configure is first-time setup, safe to repeat: a config pointing at a repo,
// the CLI on PATH, and the agent view.
func configure(cfg Config, repoFlag string) ([]string, error) {
	var out []string
	path := filepath.Join(configDir(), "config.toml")
	msg, err := ensureConfig(path, repoFlag, pluginWorkspaceCwd(), gitTopLevel)
	if err != nil {
		return out, err
	}
	out = append(out, msg)

	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		return out, err
	}
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, ".local", "bin")
	for _, n := range []string{toolName, shortName} {
		msg, err = linkCLI(self, filepath.Join(bin, n))
		if err != nil {
			return out, err
		}
		out = append(out, msg)
	}
	// Existing flatcircle, kelpie and shepherd links keep working as deprecated
	// aliases; no new ones are made.
	for _, n := range legacyNames {
		if fi, err := os.Lstat(filepath.Join(bin, n)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			msg, err = linkCLI(self, filepath.Join(bin, n))
			if err != nil {
				return out, err
			}
			out = append(out, msg+" (deprecated alias)")
		}
	}

	if err := setView(socketRPC{herdrSocket()}); err != nil {
		return append(out, "agent view not set (is herdr running?): "+err.Error()), nil
	}
	return append(out, `agent view set: label "quartermaster", sorted by thread state`), nil
}

// ensureConfig writes a config with repo when there is none. The repo comes
// from --repo, else the git repository of the herdr workspace the action ran
// in, else of the current directory. An existing config is never rewritten.
func ensureConfig(path, repoFlag, workspaceCwd string, toplevel func(dir string) string) (string, error) {
	if _, err := os.Stat(path); err == nil {
		if repoFlag != "" {
			return fmt.Sprintf("%s exists; left it alone (edit repo there to change it)", path), nil
		}
		return path + " exists", nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	repo := expandHome(repoFlag)
	for _, dir := range []string{workspaceCwd, cwd()} {
		if repo != "" {
			break
		}
		if dir != "" {
			repo = toplevel(dir)
		}
	}
	if repo == "" {
		return "", errors.New("no repo: run `quartermaster configure --repo <path to your repository>`")
	}
	if abs, err := filepath.Abs(repo); err == nil {
		repo = abs
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(configTemplate(repo)), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %s (repo = %s)", path, repo), nil
}

func configTemplate(repo string) string {
	return fmt.Sprintf(`repo = %q
# See the README for everything else: branch_prefix, worker_agent, repos,
# linear_prefixes, tick_seconds, idle_seconds, nudge, [autonomy].
`, repo)
}

// linkCLI points target at self. It replaces a symlink (an older install) but
// never a real file someone put there.
func linkCLI(self, target string) (string, error) {
	fi, err := os.Lstat(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return "", err
	case fi.Mode()&os.ModeSymlink == 0:
		return fmt.Sprintf("%s is a real file, not a link; left it alone (quartermaster is at %s)", target, self), nil
	default:
		// Resolve self too: a path through a symlink (macOS's /var, a linked
		// ~/src) never equals the fully resolved target.
		realSelf, err := filepath.EvalSymlinks(self)
		if err != nil {
			realSelf = self
		}
		if dest, err := filepath.EvalSymlinks(target); err == nil && dest == realSelf {
			return target + " already links to " + self, nil
		}
		if err := os.Remove(target); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	if err := os.Symlink(self, target); err != nil {
		return "", err
	}
	msg := "linked " + target + " -> " + self
	if !onPath(filepath.Dir(target)) {
		msg += " (add " + filepath.Dir(target) + " to PATH)"
	}
	return msg, nil
}

func onPath(dir string) bool {
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(d) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

// pluginWorkspaceCwd is the workspace a plugin action was invoked from.
func pluginWorkspaceCwd() string {
	var ctx struct {
		WorkspaceCwd string `json:"workspace_cwd"`
	}
	if json.Unmarshal([]byte(os.Getenv("HERDR_PLUGIN_CONTEXT_JSON")), &ctx) != nil {
		return ""
	}
	return ctx.WorkspaceCwd
}

func gitTopLevel(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func cwd() string {
	d, _ := os.Getwd()
	return d
}
