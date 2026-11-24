package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const routerUsage = "usage: quartermaster router [status|sync|set worker|coordinator <model>...]"

func routerCmd(cfg Config, pos []string, out io.Writer) error {
	sub := "status"
	var args []string
	if len(pos) > 0 {
		sub, args = pos[0], pos[1:]
	}
	switch sub {
	case "status":
		if len(args) != 0 {
			return errors.New("usage: quartermaster router [status]")
		}
		return routerStatus(cfg, time.Now(), out)
	case "sync":
		if len(args) != 0 {
			return errors.New("usage: quartermaster router sync")
		}
		return routerSync(cfg, time.Now(), out)
	case "set":
		if len(args) < 2 || (args[0] != "worker" && args[0] != "coordinator") {
			return errors.New("usage: quartermaster router set worker|coordinator <model>...")
		}
		return routerSet(args[0], args[1:], time.Now(), out)
	default:
		return fmt.Errorf("unknown router command %q (%s)", sub, routerUsage)
	}
}

func routerSync(cfg Config, now time.Time, out io.Writer) error {
	if _, err := piExtensionFile(); err != nil {
		return fmt.Errorf("pi extension: %w", err)
	}
	routerPath, err := cfg.writePiRouterConfig()
	if err != nil {
		return fmt.Errorf("pi router config: %w", err)
	}
	if err := writeQuotaSnapshot(now); err != nil {
		return fmt.Errorf("pi quota snapshot: %w", err)
	}
	rc := cfg.routerConfig()
	fmt.Fprintf(out, "synced %s\n", routerPath)
	fmt.Fprintf(out, "worker:      %s\n", strings.Join(rc.WorkerModels, " -> "))
	fmt.Fprintf(out, "coordinator: %s\n", strings.Join(rc.CoordinatorModels, " -> "))
	return nil
}

func routerStatus(cfg Config, now time.Time, out io.Writer) error {
	rc := cfg.routerConfig()
	fmt.Fprintf(out, "worker:      %s\n", strings.Join(rc.WorkerModels, " -> "))
	fmt.Fprintf(out, "coordinator: %s\n", strings.Join(rc.CoordinatorModels, " -> "))
	fmt.Fprintf(out, "thresholds:  switch <= %g%%, recover >= %g%%\n", rc.ThresholdPct, rc.RecoverPct)
	if len(rc.Providers) > 0 {
		fmt.Fprintln(out, "providers:")
		for _, k := range slices.Sorted(maps.Keys(rc.Providers)) {
			p := rc.Providers[k]
			fmt.Fprintf(out, "  %-12s switch <= %g%%, recover >= %g%%\n", k+":", p.ThresholdPct, p.RecoverPct)
		}
	}

	routerPath := piRouterConfigPath()
	syncState := routerSyncState(rc, routerPath)
	fmt.Fprintf(out, "router.json: %s (%s)\n", syncState, routerPath)

	quotaPath := piQuotaSnapshotPath()
	writeQuotaStatus(quotaPath, time.Duration(rc.QuotaMaxAgeSecs)*time.Second, now, out)
	return nil
}

func routerSyncState(want piRouterConfig, path string) string {
	cur, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "missing (run `quartermaster router sync`)"
		}
		return fmt.Sprintf("unreadable (%v)", err)
	}
	var got piRouterConfig
	if err := json.Unmarshal(cur, &got); err != nil {
		return "invalid JSON (run `quartermaster router sync`)"
	}
	wb, err1 := json.Marshal(want)
	gb, err2 := json.Marshal(got)
	if err1 == nil && err2 == nil && bytes.Equal(wb, gb) {
		return "in sync"
	}
	return "out of sync with config.toml (run `quartermaster router sync`)"
}

func writeQuotaStatus(quotaPath string, maxAge time.Duration, now time.Time, out io.Writer) {
	b, err := os.ReadFile(quotaPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(out, "quota.json:  missing (%s)\n", quotaPath)
			return
		}
		fmt.Fprintf(out, "quota.json:  unreadable (%v)\n", err)
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		fmt.Fprintf(out, "quota.json:  invalid JSON (%s)\n", quotaPath)
		return
	}
	var genAt time.Time
	if g, ok := raw["generated_at"]; ok {
		_ = json.Unmarshal(g, &genAt)
	}
	if !genAt.IsZero() {
		fmt.Fprintf(out, "quota.json:  updated %s (%s)\n", formatRouterAge(now.Sub(genAt)), quotaPath)
	} else {
		fmt.Fprintf(out, "quota.json:  %s\n", quotaPath)
	}
	// Always show the standard providers first, followed by any extra keys in
	// quota.json sorted alphabetically.
	keys := []string{"antigravity", "claude", "openai"}
	var extra []string
	for k := range raw {
		lk := strings.ToLower(strings.TrimSpace(k))
		if lk == "" || lk == "generated_at" || slices.Contains(keys, lk) {
			continue
		}
		extra = append(extra, k)
	}
	slices.Sort(extra)
	keys = append(keys, extra...)

	for _, k := range keys {
		val, ok := raw[k]
		if !ok || len(bytes.TrimSpace(val)) == 0 || string(bytes.TrimSpace(val)) == "null" {
			fmt.Fprintf(out, "  %-12s no reading\n", k+":")
			continue
		}
		var p quotaSnapshotProvider
		if err := json.Unmarshal(val, &p); err != nil || p.At.IsZero() {
			fmt.Fprintf(out, "  %-12s invalid reading\n", k+":")
			continue
		}
		age := now.Sub(p.At)
		fresh := p.Fresh && p.HeadroomPct != nil && age <= maxAge && age >= -5*time.Minute
		if fresh {
			if p.TightResetsAt != nil {
				fmt.Fprintf(out, "  %-12s %.1f%% left (fresh, %s, tight window resets %s)\n", k+":", *p.HeadroomPct, formatRouterAge(age), p.TightResetsAt.UTC().Format(time.RFC3339))
			} else {
				fmt.Fprintf(out, "  %-12s %.1f%% left (fresh, %s)\n", k+":", *p.HeadroomPct, formatRouterAge(age))
			}
		} else {
			fmt.Fprintf(out, "  %-12s stale (%s)\n", k+":", formatRouterAge(age))
		}
	}
}

func formatRouterAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh ago", h)
		}
		return fmt.Sprintf("%dh%dm ago", h, m)
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func routerSet(role string, rawModels []string, now time.Time, out io.Writer) error {
	key := "router_worker_models"
	if role == "coordinator" {
		key = "router_coordinator_models"
	}
	for _, m := range rawModels {
		if err := validatePiPhysicalModel(key, m); err != nil {
			return err
		}
	}
	models := dedupePiModels(rawModels)
	if len(models) == 0 {
		return fmt.Errorf("[agents.pi] %s: must list at least one physical model", key)
	}

	cfgPath := filepath.Join(configDir(), "config.toml")
	mode := fs.FileMode(0o600)
	var src string
	if fi, err := os.Stat(cfgPath); err == nil {
		mode = fi.Mode().Perm()
		b, rerr := os.ReadFile(cfgPath)
		if rerr != nil {
			return rerr
		}
		src = string(b)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	updated, clearedLegacy, err := updateRouterModelsTOML(src, role, models)
	if err != nil {
		return err
	}

	// Validate the updated TOML before touching config.toml on disk.
	checkCfg := defaultConfig()
	if _, err := toml.Decode(updated, &checkCfg); err != nil {
		return fmt.Errorf("updated config.toml failed to parse: %w", err)
	}
	if err := checkCfg.validatePiRouter(); err != nil {
		return fmt.Errorf("updated config.toml failed validation: %w", err)
	}
	gotModels := checkCfg.Agents["pi"].RouterWorkerModels
	if role == "coordinator" {
		gotModels = checkCfg.Agents["pi"].RouterCoordinatorModels
	}
	if !slices.Equal(gotModels, models) {
		return fmt.Errorf("updated config.toml did not produce expected [agents.pi] %s (check for conflicting dotted keys)", key)
	}

	if err := writeAtomicMode(cfgPath, []byte(updated), mode); err != nil {
		return err
	}

	reloaded, err := loadConfig()
	if err != nil {
		return fmt.Errorf("wrote %s but reload failed: %w", cfgPath, err)
	}
	if _, err := piExtensionFile(); err != nil {
		return fmt.Errorf("pi extension: %w", err)
	}
	routerPath, err := reloaded.writePiRouterConfig()
	if err != nil {
		return fmt.Errorf("pi router config: %w", err)
	}
	if err := writeQuotaSnapshot(now); err != nil {
		return fmt.Errorf("pi quota snapshot: %w", err)
	}

	if clearedLegacy {
		legacy := "router_worker_model"
		if role == "coordinator" {
			legacy = "router_coordinator_model"
		}
		fmt.Fprintf(out, "removed conflicting legacy [agents.pi] %s\n", legacy)
	}
	fmt.Fprintf(out, "updated %s ([agents.pi] %s)\n", cfgPath, key)
	fmt.Fprintf(out, "synced %s\n", routerPath)
	fmt.Fprintf(out, "%s: %s\n", role, strings.Join(models, " -> "))
	return nil
}

// writeAtomicMode atomically writes data to path preserving perm.
func writeAtomicMode(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

type byteSpan struct {
	start int
	end   int
}

// updateRouterModelsTOML surgically updates [agents.pi] router_worker_models
// or router_coordinator_models in src, preserving existing comments, formatting
// and unrelated keys/sections. If the conflicting legacy router_worker_model
// or router_coordinator_model key is present under [agents.pi], it is removed.
func updateRouterModelsTOML(src, role string, models []string) (string, bool, error) {
	key, legacyKey := "router_worker_models", "router_worker_model"
	if role == "coordinator" {
		key, legacyKey = "router_coordinator_models", "router_coordinator_model"
	}
	formatted := formatTOMLStringArray(key, models)

	// Scan top-level TOML structure to locate [agents.pi] and any target keys.
	inPi := false
	foundPi := false
	piHeaderEnd := -1
	piSectionEnd := len(src)
	var keySpan *byteSpan
	var legacySpan *byteSpan

	for pos := 0; pos < len(src); {
		lineStart := pos
		lineEnd := nextLineEnd(src, pos)
		trimmed := strings.TrimLeft(src[lineStart:lineEnd], " \t")
		if trimmed == "" || trimmed[0] == '#' || trimmed[0] == '\n' || trimmed[0] == '\r' {
			pos = lineEnd
			continue
		}
		if trimmed[0] == '[' {
			tablePath, ok := parseTOMLTableHeader(trimmed)
			if ok {
				if inPi {
					piSectionEnd = lineStart
					inPi = false
				}
				if tablePath == "agents.pi" {
					inPi = true
					foundPi = true
					piHeaderEnd = lineEnd
					piSectionEnd = len(src)
				}
			}
			pos = lineEnd
			continue
		}
		// Assignment statement: find '=' on the first line (outside quotes).
		eqRel := findTOMLEquals(src[lineStart:lineEnd])
		if eqRel < 0 {
			pos = lineEnd
			continue
		}
		stmtEnd := scanTOMLValueEnd(src, lineStart+eqRel+1)
		if inPi {
			kName := parseTOMLSimpleKey(src[lineStart : lineStart+eqRel])
			switch kName {
			case key:
				s := byteSpan{start: lineStart, end: stmtEnd}
				keySpan = &s
			case legacyKey:
				s := byteSpan{start: lineStart, end: stmtEnd}
				legacySpan = &s
			}
		}
		pos = stmtEnd
	}

	if !foundPi {
		var b strings.Builder
		b.WriteString(src)
		if len(src) > 0 {
			if !strings.HasSuffix(src, "\n") {
				b.WriteByte('\n')
			}
			if !strings.HasSuffix(b.String(), "\n\n") {
				b.WriteByte('\n')
			}
		}
		b.WriteString("[agents.pi]\n")
		b.WriteString(formatted)
		return b.String(), false, nil
	}

	type textEdit struct {
		start int
		end   int
		text  string
	}
	var edits []textEdit
	clearedLegacy := false
	switch {
	case keySpan != nil:
		if legacySpan != nil {
			edits = append(edits, textEdit{start: legacySpan.start, end: legacySpan.end, text: ""})
			clearedLegacy = true
		}
		edits = append(edits, textEdit{start: keySpan.start, end: keySpan.end, text: formatted})
	case legacySpan != nil:
		// Replace the conflicting legacy singular key in-place so surrounding
		// comments and layout stay intact.
		edits = append(edits, textEdit{start: legacySpan.start, end: legacySpan.end, text: formatted})
		clearedLegacy = true
	default:
		insertAt := trimTrailingBlankLines(src, piHeaderEnd, piSectionEnd)
		prefix := ""
		if insertAt > 0 && src[insertAt-1] != '\n' {
			prefix = "\n"
		}
		edits = append(edits, textEdit{start: insertAt, end: insertAt, text: prefix + formatted})
	}

	// Apply edits from end to start so earlier byte offsets remain valid.
	slices.SortFunc(edits, func(a, b textEdit) int {
		if a.start != b.start {
			return b.start - a.start
		}
		return b.end - a.end
	})
	out := src
	for _, e := range edits {
		out = out[:e.start] + e.text + out[e.end:]
	}
	return out, clearedLegacy, nil
}

func formatTOMLStringArray(key string, items []string) string {
	if len(items) == 1 {
		return fmt.Sprintf("%s = [%q]\n", key, items[0])
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s = [\n", key)
	for _, item := range items {
		fmt.Fprintf(&b, "  %q,\n", item)
	}
	b.WriteString("]\n")
	return b.String()
}

func nextLineEnd(s string, pos int) int {
	if nl := strings.IndexByte(s[pos:], '\n'); nl >= 0 {
		return pos + nl + 1
	}
	return len(s)
}

func trimTrailingBlankLines(src string, minPos, end int) int {
	cur := end
	for cur > minPos {
		prevStart := strings.LastIndexByte(src[:cur-1], '\n')
		if prevStart < 0 {
			prevStart = 0
		} else {
			prevStart++
		}
		if prevStart < minPos {
			break
		}
		if strings.TrimSpace(src[prevStart:cur]) == "" {
			cur = prevStart
			continue
		}
		break
	}
	return cur
}

// parseTOMLTableHeader parses a `[table]` or `[[array]]` header line and
// returns its dot-joined path (e.g. "agents.pi"). Array headers return "" so
// they close the current table without matching `[agents.pi]`.
func parseTOMLTableHeader(trimmedLine string) (string, bool) {
	if strings.HasPrefix(trimmedLine, "[[") {
		return "", true
	}
	if !strings.HasPrefix(trimmedLine, "[") {
		return "", false
	}
	inDouble, inSingle, esc := false, false, false
	closeIdx := -1
	for i := 1; i < len(trimmedLine); i++ {
		c := trimmedLine[i]
		if esc {
			esc = false
			continue
		}
		if inDouble {
			if c == '\\' {
				esc = true
			} else if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case ']':
			closeIdx = i
		case '#':
			return "", false
		}
		if closeIdx >= 0 {
			break
		}
	}
	if closeIdx < 0 {
		return "", false
	}
	after := strings.TrimSpace(trimmedLine[closeIdx+1:])
	if after != "" && !strings.HasPrefix(after, "#") {
		return "", false
	}
	inner := strings.TrimSpace(trimmedLine[1:closeIdx])
	parts := splitTOMLKeyPath(inner)
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "."), true
}

func splitTOMLKeyPath(s string) []string {
	var parts []string
	for i := 0; i < len(s); {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			break
		}
		var seg string
		switch s[i] {
		case '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' && j+1 < len(s) {
					j += 2
					continue
				}
				j++
			}
			if j >= len(s) {
				return nil
			}
			seg = s[i+1 : j]
			i = j + 1
		case '\'':
			j := i + 1
			for j < len(s) && s[j] != '\'' {
				j++
			}
			if j >= len(s) {
				return nil
			}
			seg = s[i+1 : j]
			i = j + 1
		default:
			j := i
			for j < len(s) && s[j] != '.' && s[j] != ' ' && s[j] != '\t' {
				j++
			}
			seg = s[i:j]
			i = j
		}
		parts = append(parts, seg)
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i < len(s) {
			if s[i] != '.' {
				return nil
			}
			i++
		}
	}
	return parts
}

func findTOMLEquals(line string) int {
	inDouble, inSingle, esc := false, false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if esc {
			esc = false
			continue
		}
		if inDouble {
			if c == '\\' {
				esc = true
			} else if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case '#':
			return -1
		case '=':
			return i
		}
	}
	return -1
}

func parseTOMLSimpleKey(raw string) string {
	parts := splitTOMLKeyPath(strings.TrimSpace(raw))
	if len(parts) != 1 {
		return ""
	}
	return parts[0]
}

// scanTOMLValueEnd scans from immediately after '=' to the end of the line
// (including its '\n') where the TOML value completes, handling multi-line
// arrays, inline tables, comments, and single/multi-line basic & literal strings.
func scanTOMLValueEnd(src string, start int) int {
	bracketDepth, braceDepth := 0, 0
	inBasic, inMLBasic, inLit, inMLLit, inComment := false, false, false, false, false
	for i := start; i < len(src); i++ {
		c := src[i]
		if inComment {
			if c == '\n' {
				inComment = false
				if bracketDepth == 0 && braceDepth == 0 {
					return i + 1
				}
			}
			continue
		}
		if inMLBasic {
			if c == '\\' && i+1 < len(src) {
				i++
				continue
			}
			if c == '"' && i+2 < len(src) && src[i+1] == '"' && src[i+2] == '"' {
				inMLBasic = false
				i += 2
			}
			continue
		}
		if inMLLit {
			if c == '\'' && i+2 < len(src) && src[i+1] == '\'' && src[i+2] == '\'' {
				inMLLit = false
				i += 2
			}
			continue
		}
		if inBasic {
			if c == '\\' && i+1 < len(src) {
				i++
				continue
			}
			if c == '"' {
				inBasic = false
			} else if c == '\n' {
				inBasic = false
				if bracketDepth == 0 && braceDepth == 0 {
					return i + 1
				}
			}
			continue
		}
		if inLit {
			if c == '\'' {
				inLit = false
			} else if c == '\n' {
				inLit = false
				if bracketDepth == 0 && braceDepth == 0 {
					return i + 1
				}
			}
			continue
		}
		switch c {
		case '#':
			inComment = true
		case '"':
			if i+2 < len(src) && src[i+1] == '"' && src[i+2] == '"' {
				inMLBasic = true
				i += 2
			} else {
				inBasic = true
			}
		case '\'':
			if i+2 < len(src) && src[i+1] == '\'' && src[i+2] == '\'' {
				inMLLit = true
				i += 2
			} else {
				inLit = true
			}
		case '[':
			bracketDepth++
		case ']':
			if bracketDepth > 0 {
				bracketDepth--
			}
		case '{':
			braceDepth++
		case '}':
			if braceDepth > 0 {
				braceDepth--
			}
		case '\n':
			if bracketDepth == 0 && braceDepth == 0 {
				return i + 1
			}
		}
	}
	return len(src)
}
