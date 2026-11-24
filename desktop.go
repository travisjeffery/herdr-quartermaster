package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

// Operational delivery receipts only. Beads/worker state remains task truth;
// neither desktop submission nor reading this file answers a question.
const (
	desktopMaxAttempts     = 3
	desktopMaxSummaryRunes = 140
	desktopMaxPRTitleRunes = 70
	desktopMaxBodyLines    = 5
)

const desktopTitle = "Quartermaster: worker attention required"
const desktopBody = "Workers are blocked or need a decision. Run quartermaster context and quartermaster ticker notifications. Desktop submission is not human readback."

type desktopReceipt struct {
	Bead       string    `json:"bead,omitempty"`
	Reason     string    `json:"reason"`
	PromptHash string    `json:"prompt_hash,omitempty"`
	Summary    string    `json:"summary,omitempty"` // sanitized 1-line text, never secrets/tap URLs
	Detected   time.Time `json:"detected"`
	Active     bool      `json:"active"`
	Ended      time.Time `json:"ended,omitempty"`
	State      string    `json:"state"` // detected, attempting, submitted, failed, unknown
	Attempts   int       `json:"attempts"`
	LastTry    time.Time `json:"last_attempt,omitempty"`
	NextTry    time.Time `json:"next_attempt,omitempty"`
	Result     string    `json:"result,omitempty"` // fixed codes, never subprocess output
}

type desktopJournal struct {
	Version int                       `json:"version"`
	LastTry time.Time                 `json:"last_attempt,omitempty"`
	Items   map[string]desktopReceipt `json:"items"`
}

type desktopResult struct{ State, Reason string }

type desktopMilestone struct {
	Key     string
	Bead    string
	Reason  string // pr_merged, pr_green, pr_ready
	Summary string
}

func desktopDir() string  { return filepath.Join(stateDir(), "desktop") }
func desktopPath() string { return filepath.Join(desktopDir(), "notifications.json") }

func loadDesktop() (desktopJournal, error) {
	var j desktopJournal
	err := rpcLoad(desktopPath(), &j)
	if errors.Is(err, os.ErrNotExist) {
		return desktopJournal{Version: 1, Items: map[string]desktopReceipt{}}, nil
	}
	if err != nil {
		return j, err
	}
	if j.Version != 1 || j.Items == nil {
		return j, fmt.Errorf("invalid desktop notification journal; inspect %s", desktopPath())
	}
	for _, r := range j.Items {
		switch r.State {
		case "detected", "attempting", "submitted", "failed", "unknown":
		default:
			return j, fmt.Errorf("invalid desktop notification state")
		}
		if r.Attempts < 0 || r.Attempts > desktopMaxAttempts || r.Detected.IsZero() {
			return j, fmt.Errorf("invalid desktop notification receipt")
		}
	}
	return j, nil
}

func desktopReason(th Thread) string {
	switch {
	case th.Bead.Status == StatusNeedsMe:
		return "needs_me"
	case th.Bead.Status == StatusBlocked:
		return "blocked"
	case th.Agent != nil && th.Agent.Status == "blocked":
		// Migration parking is not a new approval question. A real task
		// blocker above still counts; never unpark or probe the worker.
		if th.Agent.Backend == rpcBackend && rpcParked(th.Bead.ID) {
			return ""
		}
		return "worker_blocked"
	default:
		return ""
	}
}

func desktopReasonFor(th Thread, cfg DesktopConfig) string {
	reason := desktopReason(th)
	switch reason {
	case "needs_me", "worker_blocked":
		if !cfg.notifyNeedsMe() {
			return ""
		}
	case "blocked":
		if !cfg.notifyBlocked() {
			return ""
		}
	}
	return reason
}

func isDesktopMilestoneReason(reason string) bool {
	switch reason {
	case "pr_merged", "pr_green", "pr_ready":
		return true
	default:
		return false
	}
}

func desktopReceiptDeliverable(r desktopReceipt) bool {
	return (r.State == "detected" || r.State == "failed") && r.Attempts < desktopMaxAttempts
}

func desktopEpisodeHash(th Thread, reason string) string {
	if th.Prompt != nil && th.Prompt.Hash != "" {
		return th.Prompt.Hash
	}
	if reason == "needs_me" {
		return asked(th.Bead)
	}
	return ""
}

func desktopMilestoneKey(reason, beadID, identity string) string {
	return "milestone:" + reason + ":" + beadID + ":" + identity
}

func desktopPRIdentity(pr PR) string {
	if pr.URL != "" {
		return pr.URL
	}
	return fmt.Sprintf("#%d", pr.Number)
}

// desktopMilestonesForTransition derives event-driven PR desktop milestones
// from a thread transition. First sight is a baseline so startup never replays
// historical PR states.
func desktopMilestonesForTransition(th Thread, prev Snapshot, first bool, cfg DesktopConfig) []desktopMilestone {
	if first || !cfg.enabled() {
		return nil
	}
	var out []desktopMilestone
	if cfg.notifyPRMerged() {
		for _, pr := range newlyMerged(th, prev) {
			out = append(out, desktopMilestone{
				Key:     desktopMilestoneKey("pr_merged", th.Bead.ID, desktopPRIdentity(pr)),
				Bead:    th.Bead.ID,
				Reason:  "pr_merged",
				Summary: formatPRMergedSummary(th.Bead.ID, pr),
			})
		}
	}
	if th.PR != nil && th.PR.State == "OPEN" && th.Bead.Status != StatusNeedsMe {
		rk := readyKey(th)
		gk := greenKey(th)
		readyFired := false
		if rk != "" && rk != prev.ReadyKey {
			readyFired = true
			if cfg.notifyPRReady() {
				out = append(out, desktopMilestone{
					Key:     desktopMilestoneKey("pr_ready", th.Bead.ID, rk),
					Bead:    th.Bead.ID,
					Reason:  "pr_ready",
					Summary: formatPRReadySummary(th.Bead.ID, *th.PR),
				})
			}
		}
		if !readyFired && rk == "" && gk != "" && prev.GreenTracked && gk != prev.GreenKey {
			if cfg.notifyPRGreen() {
				out = append(out, desktopMilestone{
					Key:     desktopMilestoneKey("pr_green", th.Bead.ID, gk),
					Bead:    th.Bead.ID,
					Reason:  "pr_green",
					Summary: formatPRGreenSummary(th.Bead.ID, *th.PR, th.Checks),
				})
			}
		}
	}
	return out
}

func formatPRMergedSummary(beadID string, pr PR) string {
	ref := prName(pr)
	if title := sanitizeDesktopLine(pr.Title, desktopMaxPRTitleRunes); title != "" && !isOnlyRedacted(title) {
		return fmt.Sprintf("%s: %s merged — %s", beadID, ref, title)
	}
	return fmt.Sprintf("%s: %s merged", beadID, ref)
}

func formatPRReadySummary(beadID string, pr PR) string {
	ref := prName(pr)
	detail := approval(&pr)
	if pr.AutoMerge != nil {
		detail = "auto-merge on"
	}
	return fmt.Sprintf("%s: %s ready to merge (%s)", beadID, ref, detail)
}

func formatPRGreenSummary(beadID string, pr PR, checks Checks) string {
	ref := prName(pr)
	status := "green, awaiting review"
	if pr.ReviewDecision == "" && mergeable(&pr, checks) {
		status = "green, ready for review"
	}
	if title := sanitizeDesktopLine(pr.Title, desktopMaxPRTitleRunes); title != "" && !isOnlyRedacted(title) {
		return fmt.Sprintf("%s: %s %s — %s", beadID, ref, status, title)
	}
	return fmt.Sprintf("%s: %s %s", beadID, ref, status)
}

var (
	desktopANSIRe         = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\))`)
	desktopTapLabeledRe   = regexp.MustCompile(`(?i)\btap(?:\s+(?:link|links|url))?\s*[:=]\s*(?:(?:https?|wss?|ftp)://|/(?:tap|tap-links|inbox(?:/plan)?)/)[^\s<>"'\x60)\]]+`)
	desktopTapURLRe       = regexp.MustCompile(`(?i)(?:https?|wss?|ftp)://[^\s<>"'\x60)\]]*/(?:tap|tap-links|inbox(?:/plan)?)(?:/[^\s<>"'\x60)\]]*)?`)
	desktopTapPathRe      = regexp.MustCompile(`(?i)(?:^|\s)/(?:tap|tap-links|inbox/plan)/[^\s<>"'\x60)\]]+`)
	desktopTailnetAddrRe  = regexp.MustCompile(`(?i)\b100\.\d{1,3}\.\d{1,3}\.\d{1,3}:\d+(?:/[^\s<>"'\x60)\]]*)?`)
	desktopTailnetDNSRe   = regexp.MustCompile(`(?i)\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.ts\.net(?::\d+)?(?:/[^\s<>"'\x60)\]]*)?`)
	desktopTailnetV6URLRe = regexp.MustCompile(`(?i)(?:https?|wss?)://\[fd7a:115c:a1e0:[^\]]*\][^\s<>"'\x60)\]]*`)
	desktopConnStringRe   = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s/@]+:[^\s/@]+@[^\s<>"'\x60)\]]+`)
	desktopGenericURLRe   = regexp.MustCompile(`(?i)\b(?:https?|wss?|ftp)://[^\s<>"'\x60)\]]+`)
	desktopExtraSecrets   = []*regexp.Regexp{
		regexp.MustCompile(`(?i)--(?:token|password|passwd|secret|api[-_]?key|apikey|auth|cookie|credential|private[-_]?key)(?:=|\s+)(?:"[^"\n]*"|'[^'\n]*'|[^\s,;&]+)`),
		regexp.MustCompile(`(?i)(?:[a-z0-9_.-]*(?:access[_-]?key|private[_-]?key|client[_-]?secret|session[_-]?id|webhook|dsn|auth[_-]?token|bearer)[a-z0-9_.-]*)["']?\s*[:=]\s*(?:"[^"\n]*"|'[^'\n]*'|[^\s,;&]+)`),
		regexp.MustCompile(`\b(?:sk_(?:live|test)_[A-Za-z0-9]{12,}|xapp-[A-Za-z0-9-]+|lin_api_[A-Za-z0-9_-]{12,}|AIza[0-9A-Za-z_-]{20,}|glpat-[A-Za-z0-9_-]{12,}|npm_[A-Za-z0-9]{12,}|pypi-[A-Za-z0-9_-]{12,}|hf_[A-Za-z0-9]{12,}|dop_v1_[A-Za-z0-9]{12,})\b`),
		// Signed 2-part tokens (e.g. bare approval tap tokens <b64url>.<hmac-sha256>).
		regexp.MustCompile(`\b[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{32,}\b`),
	}
	desktopDupRedactedRe = regexp.MustCompile(`(?:\[redacted\]\s*){2,}`)
)

// sanitizeDesktopText redacts secrets, tokens, URLs, tap links, ANSI escapes,
// and control/bidi characters BEFORE any truncation, collapsing the result into
// a clean single line safe for desktop notifications and private receipts.
func sanitizeDesktopText(raw string) string {
	s := desktopANSIRe.ReplaceAllString(raw, "")
	// Strip control/bidi characters while preserving newlines so line-scoped
	// credential patterns (e.g. Cookie headers) redact only their own line.
	s = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	// Strip tap links and tailnet tap addresses completely so they never appear
	// in desktop alerts or receipts.
	s = desktopTapLabeledRe.ReplaceAllString(s, " ")
	s = desktopTapURLRe.ReplaceAllString(s, " ")
	s = desktopTailnetV6URLRe.ReplaceAllString(s, " ")
	s = desktopTapPathRe.ReplaceAllString(s, " ")
	s = desktopTailnetAddrRe.ReplaceAllString(s, " ")
	s = desktopTailnetDNSRe.ReplaceAllString(s, " ")
	// Redact credential-bearing connection strings and all remaining URLs so
	// query-string tokens, presigned links, and tap URLs on any host can never leak.
	s = desktopConnStringRe.ReplaceAllString(s, "[redacted]")
	s = desktopGenericURLRe.ReplaceAllString(s, "[redacted]")
	// Redact credentials using the shared viewer credential patterns plus
	// desktop-specific token/flag/signed-token patterns.
	for _, re := range rpcViewCredentials {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	for _, re := range desktopExtraSecrets {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	s = strings.Join(strings.Fields(s), " ")
	s = desktopDupRedactedRe.ReplaceAllString(s, "[redacted] ")
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, ":-—;")
	return strings.TrimSpace(s)
}

func sanitizeDesktopLine(raw string, maxRunes int) string {
	return truncateDesktopLine(sanitizeDesktopText(raw), maxRunes)
}

func truncateDesktopLine(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	cut := string(runes[:maxRunes])
	if i := strings.LastIndex(cut, " "); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,:;-—") + "…"
}

func isOnlyRedacted(s string) bool {
	cleaned := strings.TrimSpace(strings.ReplaceAll(s, "[redacted]", ""))
	cleaned = strings.Trim(cleaned, " ,.:;!?-—()[]{}")
	return cleaned == ""
}

// desktopNeedsMeSummary extracts a concise, sanitized 1-line summary of why a
// bead is in needs_me. Never emits tap URLs, tokens, or secrets.
func desktopNeedsMeSummary(th Thread) string {
	notes := th.Bead.Notes
	if cmd := pendingCommand(notes); cmd != "" {
		const runReqPrefix = "RUN requested — "
		if reason := pendingCommandReason(notes); reason != "" {
			if s := sanitizeDesktopLine(reason, desktopMaxSummaryRunes-utf8.RuneCountInString(runReqPrefix)); s != "" && !isOnlyRedacted(s) {
				return runReqPrefix + s
			}
		}
		if s := sanitizeDesktopLine(cmd, desktopMaxSummaryRunes-len("RUN: ")); s != "" && !isOnlyRedacted(s) {
			return "RUN: " + s
		}
		return "RUN command requested"
	}
	if q, ok := parseNeedsMeQuestion(notes); ok && len(q.Lines) > 0 {
		if s := sanitizeDesktopLine(strings.Join(q.Lines, " "), desktopMaxSummaryRunes); s != "" && !isOnlyRedacted(s) {
			return s
		}
	}
	if q, ok := parseFreeTextNeedsMeQuestion(notes); ok && len(q.Lines) > 0 {
		if s := sanitizeDesktopLine(strings.Join(q.Lines, " "), desktopMaxSummaryRunes); s != "" && !isOnlyRedacted(s) {
			return s
		}
	}
	if raw := activeQuestionText(notes); raw != "" {
		if s := sanitizeDesktopLine(raw, desktopMaxSummaryRunes); s != "" && !isOnlyRedacted(s) {
			return s
		}
	}
	if title := sanitizeDesktopLine(th.Bead.Title, desktopMaxSummaryRunes); title != "" && !isOnlyRedacted(title) {
		return title
	}
	return "needs decision"
}

// pendingCommandReason returns the one-line explanation paired with a live
// RUN: note when present (the non-bookkeeping note immediately following RUN:).
func pendingCommandReason(notes string) string {
	lines := noteLines(notes)
	runIdx := -1
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-2; i-- {
		l := lines[i]
		if strings.HasPrefix(l, "RAN:") || strings.HasPrefix(l, "DONE:") {
			return ""
		}
		if strings.HasPrefix(l, "RUN:") {
			runIdx = i
			break
		}
	}
	if runIdx < 0 || runIdx+1 >= len(lines) {
		return ""
	}
	next := lines[runIdx+1]
	if isBookkeepingNote(next) || strings.HasPrefix(next, "RUN:") || strings.HasPrefix(next, "CLASS:") {
		return ""
	}
	return next
}

func desktopReasonPriority(reason string) int {
	switch reason {
	case "needs_me":
		return 0
	case "worker_blocked":
		return 1
	case "blocked":
		return 2
	case "pr_ready":
		return 3
	case "pr_merged":
		return 4
	case "pr_green":
		return 5
	default:
		return 6
	}
}

func formatDesktopNotification(due []string, items map[string]desktopReceipt, cfg DesktopConfig) (string, string) {
	ordered := append([]string(nil), due...)
	sort.Slice(ordered, func(i, k int) bool {
		ri, rk := items[ordered[i]], items[ordered[k]]
		pi, pk := desktopReasonPriority(ri.Reason), desktopReasonPriority(rk.Reason)
		if pi != pk {
			return pi < pk
		}
		return ordered[i] < ordered[k]
	})

	isGeneric := func(r desktopReceipt) bool {
		switch r.Reason {
		case "blocked", "worker_blocked":
			return true
		case "needs_me":
			return !cfg.includeNeedsMeSummary()
		default:
			return false
		}
	}

	allGeneric := true
	for _, id := range ordered {
		if !isGeneric(items[id]) {
			allGeneric = false
			break
		}
	}
	if allGeneric {
		return desktopTitle, desktopBody
	}

	if len(ordered) == 1 {
		id := ordered[0]
		r := items[id]
		bead := r.Bead
		if bead == "" {
			bead = id
		}
		switch r.Reason {
		case "needs_me":
			summary := r.Summary
			if summary == "" {
				summary = "needs decision"
			}
			return "Quartermaster: needs_me", fmt.Sprintf("%s: %s", bead, summary)
		case "pr_merged":
			return "Quartermaster: PR merged", r.Summary
		case "pr_ready":
			return "Quartermaster: PR ready to merge", r.Summary
		case "pr_green":
			return "Quartermaster: PR green, awaiting review", r.Summary
		default:
			return desktopTitle, desktopBody
		}
	}

	var lines []string
	needsMeCount := 0
	prCount := 0
	genericCount := 0
	for _, id := range ordered {
		r := items[id]
		bead := r.Bead
		if bead == "" {
			bead = id
		}
		switch {
		case isGeneric(r):
			genericCount++
		case r.Reason == "needs_me":
			needsMeCount++
			summary := r.Summary
			if summary == "" {
				summary = "needs decision"
			}
			lines = append(lines, fmt.Sprintf("needs_me %s: %s", bead, summary))
		case r.Reason == "pr_merged":
			prCount++
			lines = append(lines, "merged "+r.Summary)
		case r.Reason == "pr_ready":
			prCount++
			lines = append(lines, "ready "+r.Summary)
		case r.Reason == "pr_green":
			prCount++
			lines = append(lines, "green "+r.Summary)
		}
	}
	if genericCount > 0 {
		lines = append(lines, fmt.Sprintf("attention: %d %s blocked or awaiting approval", genericCount, plural(genericCount, "worker")))
	}
	if len(lines) > desktopMaxBodyLines {
		omitted := len(lines) - desktopMaxBodyLines
		lines = append(lines[:desktopMaxBodyLines], fmt.Sprintf("+%d more — run quartermaster context", omitted))
	}

	var title string
	switch {
	case needsMeCount == len(ordered):
		title = fmt.Sprintf("Quartermaster: %d beads need you", needsMeCount)
	case prCount == len(ordered):
		title = fmt.Sprintf("Quartermaster: %d PR updates", prCount)
	case needsMeCount > 0:
		title = fmt.Sprintf("Quartermaster: %d updates (%d needs_me)", len(ordered), needsMeCount)
	default:
		title = fmt.Sprintf("Quartermaster: %d updates", len(ordered))
	}
	return title, strings.Join(lines, " | ")
}

// submitDesktop uses the same local notify-send convention as the digest,
// without Herdr or any external delivery target. Never include task text.
// Failure to START is safe to retry. Once started, any unsuccessful result
// (including timeout) is ambiguous: it may have displayed before failing.
func submitDesktop() desktopResult {
	return desktopCommand(3 * time.Second)
}

func submitDesktopMessage(title, body string) desktopResult {
	return desktopCommandMessage(3*time.Second, title, body)
}

func desktopCommand(timeout time.Duration) desktopResult {
	return desktopCommandMessage(timeout, desktopTitle, desktopBody)
}

func desktopCommandMessage(timeout time.Duration, title, body string) desktopResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "notify-send", "--app-name", toolName, "--", title, body)
	if err := cmd.Start(); err != nil {
		return desktopResult{"failed", "notifier-unavailable"}
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return desktopResult{"unknown", "notifier-timeout"}
		}
		return desktopResult{"unknown", "notifier-nonzero-exit"}
	}
	return desktopResult{"submitted", "notifier-exited-zero"}
}

// reconcileDesktop runs independently of transition suppression and state.json.
// Save detection and intent before the side effect; a lost result becomes
// unknown, never an unbounded retry. All due episodes coalesce into ONE
// notification, at most once per cooldown interval, including initial existing
// blockers. We inspect current threads, never replay the historical inbox backlog.
func reconcileDesktop(threads []Thread, now time.Time, submit func() desktopResult) error {
	return reconcileDesktopWith(threads, nil, DesktopConfig{}, now, func(_, _ string) desktopResult {
		return submit()
	})
}

func reconcileDesktopWith(threads []Thread, milestones []desktopMilestone, cfg DesktopConfig, now time.Time, submit func(title, body string) desktopResult) error {
	if !cfg.enabled() {
		return nil
	}
	if err := rpcPrivateDir(desktopDir()); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(desktopDir(), "lease"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("desktop notifications already reconciling: %w", err)
	}
	j, err := loadDesktop()
	if err != nil {
		return err
	}
	active := map[string]bool{}
	for _, th := range threads {
		reason := desktopReasonFor(th, cfg)
		if reason == "" {
			// A missing worker observation is not proof its question was
			// answered. Keep the episode through a transient status failure.
			if cfg.notifyNeedsMe() {
				if r, ok := j.Items[th.Bead.ID]; ok && r.Active && r.Reason == "worker_blocked" && th.Agent == nil && th.Bead.Status == StatusInProgress {
					active[th.Bead.ID] = true
				}
			}
			continue
		}
		id := th.Bead.ID
		hash := desktopEpisodeHash(th, reason)
		summary := ""
		if reason == "needs_me" && cfg.includeNeedsMeSummary() {
			summary = desktopNeedsMeSummary(th)
		}
		r, exists := j.Items[id]
		// A temporarily unreadable prompt (or pre-upgrade receipt without hash)
		// must not turn the same blocker into a new episode, nor reset its retry budget.
		if !exists || !r.Active || r.Reason != reason || (hash != "" && r.PromptHash != "" && hash != r.PromptHash) {
			r = desktopReceipt{Bead: id, Reason: reason, PromptHash: hash, Summary: summary, Detected: now, Active: true, State: "detected"}
		} else {
			if hash != "" {
				r.PromptHash = hash
			}
			if summary != "" && r.State != "submitted" && r.State != "unknown" {
				r.Summary = summary
			}
		}
		j.Items[id], active[id] = r, true
	}
	for _, m := range milestones {
		if !cfg.allowsMilestone(m.Reason) {
			continue
		}
		if _, exists := j.Items[m.Key]; exists {
			continue
		}
		j.Items[m.Key] = desktopReceipt{
			Bead:     m.Bead,
			Reason:   m.Reason,
			Summary:  m.Summary,
			Detected: now,
			Active:   true,
			State:    "detected",
		}
		active[m.Key] = true
	}
	for id, r := range j.Items {
		if r.State == "attempting" {
			r.State, r.Result = "unknown", "interrupted-attempt"
			r.NextTry = time.Time{}
		}
		if isDesktopMilestoneReason(r.Reason) {
			if r.Active && !desktopReceiptDeliverable(r) {
				r.Active, r.Ended = false, now
				r.NextTry = time.Time{}
			}
		} else if r.Active && !active[id] {
			r.Active, r.Ended = false, now
			r.NextTry = time.Time{}
		}
		j.Items[id] = r
	}
	// Retain all current active receipts plus the latest 256 inactive receipts.
	var inactive []string
	for id, r := range j.Items {
		if !r.Active {
			inactive = append(inactive, id)
		}
	}
	sort.Slice(inactive, func(i, k int) bool { return j.Items[inactive[i]].Ended.After(j.Items[inactive[k]].Ended) })
	for i := 256; i < len(inactive); i++ {
		delete(j.Items, inactive[i])
	}
	if err := rpcSave(desktopPath(), j); err != nil {
		return err // no submission without durable detection
	}
	if cooldown := cfg.cooldown(); cooldown > 0 && !j.LastTry.IsZero() && now.Sub(j.LastTry) < cooldown {
		return nil
	}
	var due []string
	for id, r := range j.Items {
		if r.Active && desktopReceiptDeliverable(r) && !now.Before(r.NextTry) {
			due = append(due, id)
		}
	}
	if len(due) == 0 {
		return nil
	}
	title, body := formatDesktopNotification(due, j.Items, cfg)
	j.LastTry = now
	for _, id := range due {
		r := j.Items[id]
		r.State, r.LastTry, r.NextTry = "attempting", now, time.Time{}
		r.Attempts++
		j.Items[id] = r
	}
	if err := rpcSave(desktopPath(), j); err != nil {
		return err // no submission without durable intent
	}
	result := submit(title, body)
	if result.State != "submitted" && result.State != "failed" && result.State != "unknown" {
		result = desktopResult{"unknown", "invalid-adapter-result"}
	}
	for _, id := range due {
		r := j.Items[id]
		r.State, r.Result = result.State, result.Reason
		if r.State == "failed" && r.Attempts < desktopMaxAttempts {
			delay := time.Minute
			if r.Attempts > 1 {
				delay = 5 * time.Minute
			}
			r.NextTry = now.Add(delay)
		} else if isDesktopMilestoneReason(r.Reason) {
			r.Active, r.Ended = false, now
			r.NextTry = time.Time{}
		}
		j.Items[id] = r
	}
	if err := rpcSave(desktopPath(), j); err != nil {
		return err // durable intent will report unknown on recovery
	}
	if result.State != "submitted" {
		return fmt.Errorf("desktop notification %s (%s); inspect `quartermaster ticker notifications`", result.State, result.Reason)
	}
	return nil
}

func desktopNotifications(out io.Writer) error {
	j, err := loadDesktop()
	if err != nil {
		return err
	}
	// A read-only status must not rewrite an in-flight intent, but must not
	// present it as known submission either.
	return json.NewEncoder(out).Encode(struct {
		HumanReadback string         `json:"human_readback"`
		Journal       desktopJournal `json:"delivery"`
	}{"unknown; submission/attempting is not readback or task completion", j})
}
