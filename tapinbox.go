package main

import (
	"bytes"
	"cmp"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Tailnet Web Inbox: a bookmarkable, mobile-first web view (/inbox) and JSON
// snapshot (/inbox.json) served on Quartermaster's existing Tailscale-bound
// tap server (tap.go). Every route enforces the exact same fail-closed auth
// guard (tailnet IP + `tailscale whois` + [tap] devices allowlist), GET never
// mutates state, and every actionable tap/approval routes through confirm+POST
// with signed one-time nonces.

const (
	tapKindPlan        = "plan"
	inboxMaxMilestones = 8
	planPostMaxBytes   = 16 * 1024
)

// tapInboxEnv supplies the read-only live sources rendered on /inbox and
// /inbox.json. Tests replace it with deterministic fakes.
type tapInboxEnv struct {
	active     func() ([]Bead, error)
	agents     func() ([]Agent, error)
	loadState  func() TickerState
	loadSnooze func() SnoozeState
}

func liveTapInboxEnv(cfg Config, h Herdr) *tapInboxEnv {
	bd := Beads{}
	hh := h.on(cfg.coordSocket())
	return &tapInboxEnv{
		active: bd.Active,
		agents: func() ([]Agent, error) {
			agents, _, err := hh.observedAgents()
			return agents, err
		},
		loadState:  loadState,
		loadSnooze: loadSnoozeState,
	}
}

// Structured card schema stored under <state dir>/cards/<bead>/question.json
// and <state dir>/cards/<bead>/milestones.jsonl. Missing or malformed files
// fall back cleanly to bd notes and never break /inbox.

type tapQuestionCard struct {
	Version        int                    `json:"version"`
	Bead           string                 `json:"bead"`
	CreatedAt      string                 `json:"created_at,omitempty"`
	Hash           string                 `json:"hash,omitempty"`
	Question       string                 `json:"question"`
	Details        string                 `json:"details,omitempty"`
	Critical       bool                   `json:"critical,omitempty"`
	Class          *int                   `json:"class,omitempty"`
	ClassReason    string                 `json:"class_reason,omitempty"`
	Recommendation *tapCardRecommendation `json:"recommendation,omitempty"`
	Options        []tapCardOption        `json:"options,omitempty"`
	Evidence       *tapCardEvidence       `json:"evidence,omitempty"`
}

type tapCardRecommendation struct {
	Option    int    `json:"option"`
	Rationale string `json:"rationale,omitempty"`
}

type tapCardOption struct {
	Number      int    `json:"number"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Class       *int   `json:"class,omitempty"`
}

type tapCardEvidence struct {
	PRURL        string          `json:"pr_url,omitempty"`
	PlanDigest   string          `json:"plan_digest,omitempty"`
	RunURLs      []string        `json:"run_urls,omitempty"`
	CostEstimate string          `json:"cost_estimate,omitempty"`
	IaC          *tapIaCEvidence `json:"iac,omitempty"`
	Links        []tapCardLink   `json:"links,omitempty"`
}

type tapIaCEvidence struct {
	Tool    string `json:"tool,omitempty"`
	Target  string `json:"target,omitempty"`
	Create  int    `json:"create"`
	Update  int    `json:"update"`
	Replace int    `json:"replace"`
	Delete  int    `json:"delete"`
	Summary string `json:"summary,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

type tapCardLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type tapMilestoneCard struct {
	Version  int            `json:"version"`
	At       string         `json:"at,omitempty"`
	Bead     string         `json:"bead,omitempty"`
	Phase    string         `json:"phase,omitempty"`
	Status   string         `json:"status,omitempty"`
	Summary  string         `json:"summary"`
	Evidence map[string]any `json:"evidence,omitempty"`
}

func tapCardsDir() string             { return filepath.Join(stateDir(), "cards") }
func tapBeadCardDir(id string) string { return filepath.Join(tapCardsDir(), id) }

func loadQuestionCard(id string) (*tapQuestionCard, bool) {
	if !beadName.MatchString(id) {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(tapBeadCardDir(id), "question.json"))
	if err != nil {
		return nil, false
	}
	var card tapQuestionCard
	if err := json.Unmarshal(b, &card); err != nil {
		return nil, false
	}
	if card.Version != 0 && card.Version != 1 {
		return nil, false
	}
	if card.Bead != "" && card.Bead != id {
		return nil, false
	}
	if strings.TrimSpace(card.Question) == "" {
		return nil, false
	}
	if card.Class != nil && (*card.Class < 0 || *card.Class > 9) {
		card.Class = nil
	}
	for i := range card.Options {
		if card.Options[i].Class != nil && (*card.Options[i].Class < 0 || *card.Options[i].Class > 9) {
			card.Options[i].Class = nil
		}
	}
	return &card, true
}

func loadMilestones(id string, limit int) []tapMilestoneCard {
	if !beadName.MatchString(id) {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(tapBeadCardDir(id), "milestones.jsonl"))
	if err != nil {
		return nil
	}
	var out []tapMilestoneCard
	for _, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var m tapMilestoneCard
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		if m.Version != 0 && m.Version != 1 {
			continue
		}
		if m.Bead != "" && m.Bead != id {
			continue
		}
		if strings.TrimSpace(m.Summary) == "" && strings.TrimSpace(m.Phase) == "" {
			continue
		}
		out = append(out, m)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func isSafeHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// planTapClaims binds a one-time plan approval confirmation form POST to one
// exact checked plan digest, its bead, the authenticated Tailscale device, an
// expiry and a one-time nonce.
type planTapClaims struct {
	Kind   string
	Digest string
	Bead   string
	Device string
	Exp    time.Time
	Nonce  string
}

func (c planTapClaims) payload() string {
	return strings.Join([]string{c.Kind, c.Digest, c.Bead, c.Device, strconv.FormatInt(c.Exp.Unix(), 10), c.Nonce}, "|")
}

func mintPlanTapToken(secret []byte, digest, bead, device string, now time.Time) (string, planTapClaims, error) {
	for _, f := range []string{digest, bead, device} {
		if f == "" || strings.Contains(f, "|") {
			return "", planTapClaims{}, fmt.Errorf("plan approval field %q is not usable", f)
		}
	}
	var n [8]byte
	if _, err := rand.Read(n[:]); err != nil {
		return "", planTapClaims{}, err
	}
	c := planTapClaims{
		Kind:   tapKindPlan,
		Digest: digest,
		Bead:   bead,
		Device: device,
		Exp:    now.Add(tapTTL),
		Nonce:  hex.EncodeToString(n[:]),
	}
	p := c.payload()
	return base64.RawURLEncoding.EncodeToString([]byte(p)) + "." + tapSign(secret, p), c, nil
}

func parsePlanTapToken(secret []byte, token string, now time.Time) (planTapClaims, error) {
	enc, sig, ok := strings.Cut(token, ".")
	if !ok {
		return planTapClaims{}, errors.New("not a plan approval token")
	}
	p, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return planTapClaims{}, errors.New("not a plan approval token")
	}
	if !hmac.Equal([]byte(sig), []byte(tapSign(secret, string(p)))) {
		return planTapClaims{}, errors.New("this plan approval token's signature does not check out")
	}
	f := strings.Split(string(p), "|")
	if len(f) != 6 || f[0] != tapKindPlan || f[1] == "" || f[2] == "" || f[3] == "" || f[5] == "" {
		return planTapClaims{}, errors.New("not a plan approval token")
	}
	exp, err := strconv.ParseInt(f[4], 10, 64)
	if err != nil {
		return planTapClaims{}, errors.New("not a plan approval token")
	}
	c := planTapClaims{
		Kind:   f[0],
		Digest: f[1],
		Bead:   f[2],
		Device: f[3],
		Exp:    time.Unix(exp, 0),
		Nonce:  f[5],
	}
	if now.After(c.Exp) {
		return c, errTapExpired
	}
	return c, nil
}

// View/JSON models for GET /inbox and GET /inbox.json.

type inboxSnapshot struct {
	Version         int             `json:"version"`
	GeneratedAt     time.Time       `json:"generated_at"`
	Viewer          inboxViewer     `json:"viewer"`
	Counts          inboxCounts     `json:"counts"`
	Warnings        []string        `json:"warnings,omitempty"`
	Prompts         []inboxPrompt   `json:"prompts"`
	Questions       []inboxQuestion `json:"questions"`
	ParkedQuestions []inboxQuestion `json:"parked_questions,omitempty"`
	Plans           []inboxPlan     `json:"plans"`
	ReadyToMerge    []inboxReadyPR  `json:"ready_to_merge"`

	ParkedSummary string `json:"-"`
}

type inboxViewer struct {
	As     string `json:"as"`
	Device string `json:"device"`
	User   string `json:"user,omitempty"`
}

type inboxCounts struct {
	Prompts         int `json:"prompts"`
	Questions       int `json:"questions"`
	ParkedQuestions int `json:"parked_questions,omitempty"`
	Plans           int `json:"plans"`
	ReadyToMerge    int `json:"ready_to_merge"`
}

func (c inboxCounts) Total() int {
	return c.Prompts + c.Questions + c.Plans + c.ReadyToMerge
}

type inboxPrompt struct {
	Bead        string              `json:"bead"`
	Title       string              `json:"title"`
	Level       string              `json:"level,omitempty"`
	AgentKind   string              `json:"agent_kind,omitempty"`
	AgentStatus string              `json:"agent_status,omitempty"`
	Hash        string              `json:"hash"`
	Summary     string              `json:"summary"`
	Lines       []string            `json:"lines"`
	Countdown   string              `json:"countdown,omitempty"`
	Expires     time.Time           `json:"expires"`
	Options     []inboxPromptOption `json:"options"`
}

type inboxPromptOption struct {
	Number   int    `json:"number"`
	Label    string `json:"label"`
	Selected bool   `json:"selected,omitempty"`
	Path     string `json:"path"`
	URL      string `json:"url"`
}

type inboxQuestion struct {
	Bead           string                 `json:"bead"`
	Title          string                 `json:"title"`
	Level          string                 `json:"level,omitempty"`
	Kind           string                 `json:"kind"` // "structured", "options", "run", "free_text"
	Tappable       bool                   `json:"tappable"`
	Critical       bool                   `json:"critical,omitempty"`
	Parked         bool                   `json:"parked,omitempty"`
	ParkedReason   string                 `json:"parked_reason,omitempty"`
	SnoozedUntil   *time.Time             `json:"snoozed_until,omitempty"`
	Class          *int                   `json:"class,omitempty"`
	ClassText      string                 `json:"class_text,omitempty"`
	Question       string                 `json:"question"`
	Details        string                 `json:"details,omitempty"`
	RunCommand     string                 `json:"run_command,omitempty"`
	Lines          []string               `json:"lines,omitempty"`
	Recommendation *tapCardRecommendation `json:"recommendation,omitempty"`
	Options        []inboxQuestionOption  `json:"options,omitempty"`
	FreeText       *inboxFreeTextLink     `json:"free_text,omitempty"`
	Expires        *time.Time             `json:"expires,omitempty"`
	Evidence       *inboxEvidence         `json:"evidence,omitempty"`
	Milestones     []tapMilestoneCard     `json:"milestones,omitempty"`

	parkedKind string `json:"-"`
}

type inboxFreeTextLink struct {
	Path     string `json:"path"`
	URL      string `json:"url"`
	MaxChars int    `json:"max_chars"`
}

type inboxQuestionOption struct {
	Number      int    `json:"number"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Class       *int   `json:"class,omitempty"`
	Recommended bool   `json:"recommended,omitempty"`
	Path        string `json:"path,omitempty"`
	URL         string `json:"url,omitempty"`
}

type inboxEvidence struct {
	PRURL           string          `json:"pr_url,omitempty"`
	PlanDigest      string          `json:"plan_digest,omitempty"`
	PlanApprovePath string          `json:"plan_approve_path,omitempty"`
	RunURLs         []string        `json:"run_urls,omitempty"`
	CostEstimate    string          `json:"cost_estimate,omitempty"`
	IaC             *tapIaCEvidence `json:"iac,omitempty"`
	Links           []tapCardLink   `json:"links,omitempty"`
}

func (e *inboxEvidence) empty() bool {
	return e == nil || (e.PRURL == "" && e.PlanDigest == "" && len(e.RunURLs) == 0 && e.CostEstimate == "" && e.IaC == nil && len(e.Links) == 0)
}

type inboxPlan struct {
	Bead        string          `json:"bead"`
	Title       string          `json:"title,omitempty"`
	Level       string          `json:"level,omitempty"`
	Digest      string          `json:"digest"`
	ShortDigest string          `json:"short_digest"`
	Status      string          `json:"status"` // "waiting", "needs_me", "expired"
	Why         string          `json:"why,omitempty"`
	State       string          `json:"state"`
	Class       int             `json:"class"`
	UpdatedAt   time.Time       `json:"updated_at"`
	Steps       []inboxPlanStep `json:"steps"`
	ApprovePath string          `json:"approve_path"`
	ApproveURL  string          `json:"approve_url"`
}

type inboxPlanStep struct {
	Number int               `json:"number"`
	Op     string            `json:"op"`
	Target string            `json:"target,omitempty"`
	Text   string            `json:"text,omitempty"`
	Class  int               `json:"class"`
	Why    string            `json:"why,omitempty"`
	IaC    *inboxPlanStepIaC `json:"iac,omitempty"`
}

type inboxPlanStepIaC struct {
	Tool    string         `json:"tool,omitempty"`
	Target  string         `json:"target,omitempty"`
	Create  int            `json:"create"`
	Update  int            `json:"update"`
	Replace int            `json:"replace"`
	Delete  int            `json:"delete"`
	Counts  map[string]int `json:"counts,omitempty"`
	Summary string         `json:"summary"`
	Blocked []string       `json:"blocked,omitempty"`
}

type inboxReadyPR struct {
	Bead           string   `json:"bead"`
	Title          string   `json:"title"`
	Level          string   `json:"level,omitempty"`
	MergeClass     int      `json:"merge_class"`
	PRNumber       int      `json:"pr_number"`
	PRTitle        string   `json:"pr_title,omitempty"`
	PRURL          string   `json:"pr_url"`
	HeadSHA        string   `json:"head_sha"`
	ShortSHA       string   `json:"short_sha"`
	ChecksSummary  string   `json:"checks_summary,omitempty"`
	ChecksPassing  int      `json:"checks_passing"`
	ChecksPending  int      `json:"checks_pending"`
	ChecksFailing  []string `json:"checks_failing,omitempty"`
	ReviewState    string   `json:"review_state"`
	ReviewDecision string   `json:"review_decision,omitempty"`
	AutoMerge      bool     `json:"auto_merge,omitempty"`
}

func (s *tapServer) linkURL(path string) string {
	if s.base != "" {
		return s.base + path
	}
	return path
}

func (s *tapServer) handleRoot(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAuth(w, r, "", false); !ok {
		return
	}
	if r.URL.Path != "/" {
		tapPage(w, http.StatusNotFound, "Nothing here", "No page exists at this path.")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.refuse(w, http.StatusMethodNotAllowed, "", "only GET is allowed here")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/inbox", http.StatusFound)
}

func (s *tapServer) handleInbox(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.requireAuth(w, r, "", false)
	if !ok {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.refuse(w, http.StatusMethodNotAllowed, "", "inbox is read-only (GET only)")
		return
	}
	snap := s.buildInbox(ident, s.now())
	var buf bytes.Buffer
	if err := tapInboxTmpl.Execute(&buf, snap); err != nil {
		s.logf("tap: render inbox: %v", err)
		tapPage(w, http.StatusInternalServerError, "Inbox error", "Could not render the inbox page.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

func (s *tapServer) handleInboxJSON(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.requireAuth(w, r, "", true)
	if !ok {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.refuseJSON(w, http.StatusMethodNotAllowed, "", "inbox.json is read-only (GET only)")
		return
	}
	snap := s.buildInbox(ident, s.now())
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snap); err != nil {
		s.logf("tap: encode inbox.json: %v", err)
		s.refuseJSON(w, http.StatusInternalServerError, "", "could not encode inbox JSON")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

func (s *tapServer) handleInboxPlan(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.requireAuth(w, r, "", false)
	if !ok {
		return
	}
	ref := strings.TrimPrefix(r.URL.Path, "/inbox/plan/")
	if ref == "" || strings.Contains(ref, "/") {
		s.refuse(w, http.StatusNotFound, "", "plan digest is missing")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.confirmPlan(w, ref, ident)
	case http.MethodPost:
		s.approvePlanPost(w, r, ref, ident)
	default:
		s.refuse(w, http.StatusMethodNotAllowed, "", "plan approval is GET to review, POST to approve")
	}
}

func (s *tapServer) confirmPlan(w http.ResponseWriter, ref string, ident tapIdentity) {
	now := s.now()
	p, digest, err := loadPlan(ref)
	if err != nil {
		s.refuse(w, http.StatusNotFound, "", err.Error())
		return
	}
	events, err := readPlanEvents(digest)
	if err != nil {
		s.refuse(w, http.StatusInternalServerError, p.Bead, err.Error())
		return
	}
	var b Bead
	if s.env.show != nil {
		if shown, serr := s.env.show(p.Bead); serr == nil {
			b = shown
		}
	}
	item, pending, reason := s.describePlan(p, digest, events, b, now)
	if !pending {
		s.refuse(w, http.StatusConflict, p.Bead, fmt.Sprintf("plan %s is not awaiting approval (%s)", short(digest), reason))
		return
	}
	tok, claims, err := mintPlanTapToken(s.secret, digest, p.Bead, ident.Device, now)
	if err != nil {
		s.refuse(w, http.StatusInternalServerError, p.Bead, "could not mint approval token: "+err.Error())
		return
	}
	var buf bytes.Buffer
	data := struct {
		Plan    inboxPlan
		Token   string
		Device  string
		Expires time.Time
	}{Plan: item, Token: tok, Device: ident.Device, Expires: claims.Exp}
	if err := tapPlanConfirmTmpl.Execute(&buf, data); err != nil {
		s.logf("tap: render plan confirm: %v", err)
		tapPage(w, http.StatusInternalServerError, "Plan error", "Could not render plan confirmation page.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

func (s *tapServer) approvePlanPost(w http.ResponseWriter, r *http.Request, ref string, ident tapIdentity) {
	now := s.now()
	r.Body = http.MaxBytesReader(w, r.Body, planPostMaxBytes)
	if err := r.ParseForm(); err != nil {
		s.refuse(w, http.StatusBadRequest, "", "could not read approval form: "+err.Error())
		return
	}
	token := strings.TrimSpace(r.FormValue("token"))
	claims, err := parsePlanTapToken(s.secret, token, now)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errTapExpired) {
			status = http.StatusGone
		}
		s.refuse(w, status, claims.Bead, err.Error())
		return
	}
	p, digest, err := loadPlan(ref)
	if err != nil {
		s.refuse(w, http.StatusNotFound, claims.Bead, err.Error())
		return
	}
	if claims.Digest != digest || claims.Bead != p.Bead {
		s.refuse(w, http.StatusBadRequest, p.Bead, "approval token does not match this plan")
		return
	}
	if !strings.EqualFold(claims.Device, ident.Device) {
		s.refuse(w, http.StatusForbidden, p.Bead, fmt.Sprintf("approval token was minted for device %s, not %s", claims.Device, ident.Device))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	replay, err := s.consume(claims.Nonce, claims.Exp, now)
	if err != nil {
		s.refuse(w, http.StatusInternalServerError, p.Bead, "the approval could not be recorded: "+err.Error())
		return
	}
	if replay {
		s.refuse(w, http.StatusConflict, p.Bead, "this plan approval token was already used")
		return
	}
	events, err := readPlanEvents(digest)
	if err != nil {
		s.refuse(w, http.StatusInternalServerError, p.Bead, err.Error())
		return
	}
	var b Bead
	if s.env.show != nil {
		if shown, serr := s.env.show(p.Bead); serr == nil {
			b = shown
		}
	}
	if _, pending, reason := s.describePlan(p, digest, events, b, now); !pending {
		s.refuse(w, http.StatusConflict, p.Bead, fmt.Sprintf("plan %s is no longer awaiting approval (%s)", short(digest), reason))
		return
	}
	via := "tap (device " + ident.Device + ")"
	show := s.env.show
	if show == nil {
		show = func(string) (Bead, error) { return Bead{ID: p.Bead}, nil }
	}
	msg, err := approvePlan(s.fullCfg, show, digest, "TJ", approvalTTL, via, now)
	if err != nil {
		s.refuse(w, http.StatusInternalServerError, p.Bead, "plan approval failed: "+err.Error())
		return
	}
	s.logf("tap: %s", strings.TrimSpace(msg))
	var buf bytes.Buffer
	data := struct {
		Digest  string
		Short   string
		Bead    string
		Device  string
		Message string
	}{Digest: digest, Short: short(digest), Bead: p.Bead, Device: ident.Device, Message: strings.TrimSpace(msg)}
	if err := tapPlanApprovedTmpl.Execute(&buf, data); err != nil {
		tapPage(w, http.StatusOK, "Plan approved", strings.TrimSpace(msg))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// buildInbox gathers all four live queues without mutating any state.
func (s *tapServer) buildInbox(ident tapIdentity, now time.Time) inboxSnapshot {
	snap := inboxSnapshot{
		Version:      1,
		GeneratedAt:  now,
		Viewer:       inboxViewer{As: "TJ", Device: ident.Device, User: ident.User},
		Prompts:      []inboxPrompt{},
		Questions:    []inboxQuestion{},
		Plans:        []inboxPlan{},
		ReadyToMerge: []inboxReadyPR{},
	}
	var active []Bead
	byName := map[string]Agent{}
	var st TickerState
	var snoozes SnoozeState
	if s.inbox != nil {
		if s.inbox.loadState != nil {
			st = s.inbox.loadState()
		}
		if s.inbox.loadSnooze != nil {
			snoozes = s.inbox.loadSnooze()
		} else {
			snoozes = loadSnoozeState()
		}
		if s.inbox.active != nil {
			var err error
			active, err = s.inbox.active()
			if err != nil {
				snap.Warnings = append(snap.Warnings, "active beads unavailable: "+err.Error())
			}
		}
		if s.inbox.agents != nil {
			agents, err := s.inbox.agents()
			if err != nil {
				snap.Warnings = append(snap.Warnings, "agent list unavailable: "+err.Error())
			} else {
				for _, a := range agents {
					if a.Name != "" {
						byName[a.Name] = a
					}
				}
			}
		}
	}
	activeByID := make(map[string]Bead, len(active))
	for _, b := range active {
		activeByID[b.ID] = b
		if _, ok := byName[agentName(b.ID)]; !ok && s.env.agent != nil {
			if host := beadHost(b); host != "" && host != localHost {
				if a, err := s.env.agent(host, agentName(b.ID)); err == nil && a != nil {
					byName[agentName(b.ID)] = *a
				}
			}
		}
	}

	// Collect pending checked plans first so question cards referencing a
	// pending plan digest can link straight to its confirm page.
	plans := s.collectPendingPlans(activeByID, now)
	pendingPlans := make(map[string]string, len(plans))
	for _, pl := range plans {
		pendingPlans[pl.Digest] = pl.ApprovePath
		pendingPlans[pl.ShortDigest] = pl.ApprovePath
	}
	snap.Plans = plans

	children := openChildCounts(active)
	parkedSnoozed, parkedDormant := 0, 0
	for _, b := range active {
		th := threadFor(s.fullCfg, st, b, byName)
		if n := children[b.ID]; n > 0 {
			th.OpenChildren = n
			th = th.derive()
		}
		if th.Agent != nil && th.Agent.Status == "blocked" {
			if p := s.readLivePrompt(b, th.Agent); p != nil {
				if item, ok := s.buildPromptItem(th, *p, now); ok {
					snap.Prompts = append(snap.Prompts, item)
				}
			}
		}
		if b.Status == StatusNeedsMe && !answered(th) {
			qItem := s.buildQuestionItem(th, pendingPlans, now)
			if parked, kind, until := classifyNeedsYou(s.fullCfg, th, snoozes, now); parked {
				qItem.Parked = true
				qItem.parkedKind = kind
				qItem.ParkedReason = formatParkedReason(kind, until, b.UpdatedAt, now)
				if !until.IsZero() {
					u := until
					qItem.SnoozedUntil = &u
				}
				if kind == parkedReasonSnoozed {
					parkedSnoozed++
				} else {
					parkedDormant++
				}
				snap.ParkedQuestions = append(snap.ParkedQuestions, qItem)
			} else {
				snap.Questions = append(snap.Questions, qItem)
			}
		}
		if classify(th) == GroupReady && th.PR != nil {
			snap.ReadyToMerge = append(snap.ReadyToMerge, s.buildReadyPRItem(th))
		}
	}

	sort.Slice(snap.Prompts, func(i, j int) bool {
		return snap.Prompts[i].Bead < snap.Prompts[j].Bead
	})
	sortQuestions := func(qs []inboxQuestion) {
		sort.Slice(qs, func(i, j int) bool {
			qi, qj := qs[i], qs[j]
			if qi.Critical != qj.Critical {
				return qi.Critical
			}
			ci, cj := -1, -1
			if qi.Class != nil {
				ci = *qi.Class
			}
			if qj.Class != nil {
				cj = *qj.Class
			}
			if ci != cj {
				return ci > cj
			}
			return qi.Bead < qj.Bead
		})
	}
	sortQuestions(snap.Questions)
	sortQuestions(snap.ParkedQuestions)
	sort.Slice(snap.ReadyToMerge, func(i, j int) bool {
		return snap.ReadyToMerge[i].Bead < snap.ReadyToMerge[j].Bead
	})

	snap.Counts = inboxCounts{
		Prompts:         len(snap.Prompts),
		Questions:       len(snap.Questions),
		ParkedQuestions: len(snap.ParkedQuestions),
		Plans:           len(snap.Plans),
		ReadyToMerge:    len(snap.ReadyToMerge),
	}
	if len(snap.ParkedQuestions) > 0 {
		snap.ParkedSummary = formatParkedBreakdown(parkedSnoozed, parkedDormant)
	}
	return snap
}

func (s *tapServer) readLivePrompt(b Bead, a *Agent) *ApprovalPrompt {
	if a == nil {
		return nil
	}
	host := beadHost(b)
	if host == "" {
		host = localHost
	}
	p, err := readAgentPrompt(s.env, host, a)
	if err != nil {
		return nil
	}
	return &p
}

func (s *tapServer) buildPromptItem(th Thread, p ApprovalPrompt, now time.Time) (inboxPrompt, bool) {
	links, err := mintTapLinks(s.secret, "", th.Bead.ID, p, now)
	if err != nil || len(links) == 0 {
		return inboxPrompt{}, false
	}
	opts := make([]inboxPromptOption, 0, len(links))
	for _, l := range links {
		opts = append(opts, inboxPromptOption{
			Number:   l.Option.Number,
			Label:    l.Option.Label,
			Selected: l.Option.Selected,
			Path:     l.URL,
			URL:      s.linkURL(l.URL),
		})
	}
	item := inboxPrompt{
		Bead:      th.Bead.ID,
		Title:     th.Bead.Title,
		Level:     th.Level,
		Hash:      p.Hash,
		Summary:   p.Summary(),
		Lines:     slices.Clone(p.Lines),
		Countdown: p.Countdown,
		Expires:   links[0].Exp,
		Options:   opts,
	}
	if th.Agent != nil {
		item.AgentKind = th.Agent.Kind
		item.AgentStatus = th.Agent.Status
	}
	return item, true
}

func (s *tapServer) buildQuestionItem(th Thread, pendingPlans map[string]string, now time.Time) inboxQuestion {
	b := th.Bead
	item := inboxQuestion{
		Bead:       b.ID,
		Title:      b.Title,
		Level:      th.Level,
		Milestones: loadMilestones(b.ID, inboxMaxMilestones),
	}

	// 1. RUN: commands take precedence and stay strictly read-only.
	if cmd := pendingCommand(b.Notes); cmd != "" {
		item.Kind = "run"
		item.RunCommand = cmd
		notes := substantiveWaitingNotes(b.Notes)
		if n := waitingNotes(notes); n > 0 {
			item.Lines = notes[len(notes)-n:]
		}
		if reason := pendingCommandReason(b.Notes); reason != "" {
			item.Question = reason
		} else {
			item.Question = "Command waiting for you to run"
			for _, l := range item.Lines {
				if !strings.HasPrefix(l, "RUN:") && !isClassLine(l) {
					item.Question = l
					break
				}
			}
		}
		item.Critical = extractNoteCritical(b.Notes)
		if text, cn := extractNoteClass(b.Notes); cn >= 0 {
			item.Class = inboxIntPtr(cn)
			item.ClassText = text
		}
		if ev := buildNoteEvidence(b.Notes, pendingPlans); !ev.empty() {
			item.Evidence = ev
		}
		return item
	}

	// 2. Check for a live QUESTION:/OPTIONS: block in bd notes.
	q, hasLiveOpts := parseNeedsMeQuestion(b.Notes)
	card, hasCard := loadQuestionCard(b.ID)

	if hasLiveOpts {
		// Structured card applies when it matches the live question hash (or
		// carries no hash and matches the option count).
		cardMatches := hasCard && (card.Hash == "" || card.Hash == q.Hash)
		links, err := mintQuestionTapLinks(s.secret, "", b.ID, q, now)
		if err == nil && len(links) == len(q.Options) {
			exp := links[0].Exp
			item.Expires = &exp
			item.Tappable = true
		}
		if ft, err := mintQuestionFreeTextTapLink(s.secret, "", b.ID, q, now); err == nil {
			item.FreeText = &inboxFreeTextLink{
				Path:     ft.URL,
				URL:      s.linkURL(ft.URL),
				MaxChars: tapMaxFreeTextRunes,
			}
			if item.Expires == nil {
				exp := ft.Exp
				item.Expires = &exp
			}
			item.Tappable = true
		}

		if cardMatches {
			item.Kind = "structured"
			item.Question = card.Question
			item.Details = card.Details
			item.Critical = card.Critical || (len(q.Lines) > 0 && strings.HasPrefix(q.Lines[0], "CRITICAL:"))
			if card.Class != nil {
				item.Class = inboxIntPtr(*card.Class)
				item.ClassText = formatClassText(*card.Class, card.ClassReason)
			} else if q.ClassN >= 0 {
				item.Class = inboxIntPtr(q.ClassN)
				item.ClassText = q.Class
			}
			item.Recommendation = card.Recommendation
			if item.Recommendation == nil {
				item.Recommendation = extractNoteRecommendation(q.Lines)
			}
			cardOpts := make(map[int]tapCardOption, len(card.Options))
			for _, co := range card.Options {
				cardOpts[co.Number] = co
			}
			for i, o := range q.Options {
				opt := inboxQuestionOption{
					Number: o.Number,
					Label:  o.Label,
				}
				if co, ok := cardOpts[o.Number]; ok {
					if strings.TrimSpace(co.Label) != "" {
						opt.Label = co.Label
					}
					opt.Description = co.Description
					if co.Class != nil {
						opt.Class = inboxIntPtr(*co.Class)
					}
				}
				if item.Recommendation != nil && item.Recommendation.Option == o.Number {
					opt.Recommended = true
				}
				if item.Tappable && i < len(links) {
					opt.Path = links[i].URL
					opt.URL = s.linkURL(links[i].URL)
				}
				item.Options = append(item.Options, opt)
			}
			item.Evidence = mergeEvidence(card.Evidence, b.Notes, pendingPlans)
			return item
		}

		// Standard OPTIONS: question from bd notes (or fallback when card is
		// missing, malformed, or stale).
		item.Kind = "options"
		item.Lines = slices.Clone(q.Lines)
		item.Question = q.Summary()
		if len(q.Lines) > 0 {
			item.Question = strings.TrimSpace(strings.TrimPrefix(q.Lines[0], "CRITICAL:"))
			if len(q.Lines) > 1 {
				item.Details = strings.Join(q.Lines[1:], "\n")
			}
		}
		item.Critical = len(q.Lines) > 0 && strings.HasPrefix(q.Lines[0], "CRITICAL:")
		if q.ClassN >= 0 {
			item.Class = inboxIntPtr(q.ClassN)
			item.ClassText = q.Class
		}
		item.Recommendation = extractNoteRecommendation(q.Lines)
		for i, o := range q.Options {
			opt := inboxQuestionOption{
				Number: o.Number,
				Label:  o.Label,
			}
			if item.Recommendation != nil && item.Recommendation.Option == o.Number {
				opt.Recommended = true
			}
			if item.Tappable && i < len(links) {
				opt.Path = links[i].URL
				opt.URL = s.linkURL(links[i].URL)
			}
			item.Options = append(item.Options, opt)
		}
		if ev := buildNoteEvidence(b.Notes, pendingPlans); !ev.empty() {
			item.Evidence = ev
		}
		return item
	}

	// 3. No live OPTIONS: block in bd notes. If a fresh structured card exists
	// for this active needs_me bead (and wasn't already answered in notes),
	// render its rich card read-only (never minting tap links that would fail
	// beadQuestion's live-note check).
	if hasCard && (card.Hash == "" || !questionCardAnswered(b.Notes, card.Hash)) {
		item.Kind = "structured"
		item.Question = card.Question
		item.Details = card.Details
		item.Critical = card.Critical || extractNoteCritical(b.Notes)
		if card.Class != nil {
			item.Class = inboxIntPtr(*card.Class)
			item.ClassText = formatClassText(*card.Class, card.ClassReason)
		} else if text, cn := extractNoteClass(b.Notes); cn >= 0 {
			item.Class = inboxIntPtr(cn)
			item.ClassText = text
		}
		item.Recommendation = card.Recommendation
		for _, co := range card.Options {
			opt := inboxQuestionOption{
				Number:      co.Number,
				Label:       co.Label,
				Description: co.Description,
			}
			if co.Class != nil {
				opt.Class = inboxIntPtr(*co.Class)
			}
			if item.Recommendation != nil && item.Recommendation.Option == co.Number {
				opt.Recommended = true
			}
			item.Options = append(item.Options, opt)
		}
		item.Evidence = mergeEvidence(card.Evidence, b.Notes, pendingPlans)
		return item
	}

	// 4. Free-text needs_me note: tappable via free-text confirm page when a
	// live question is present; read-only fallback when the bead has no note text.
	item.Kind = "free_text"
	if fq, ok := parseFreeTextNeedsMeQuestion(b.Notes); ok {
		if ft, err := mintQuestionFreeTextTapLink(s.secret, "", b.ID, fq, now); err == nil {
			exp := ft.Exp
			item.Expires = &exp
			item.Tappable = true
			item.FreeText = &inboxFreeTextLink{
				Path:     ft.URL,
				URL:      s.linkURL(ft.URL),
				MaxChars: tapMaxFreeTextRunes,
			}
		}
	}
	notes := substantiveWaitingNotes(b.Notes)
	if n := waitingNotes(notes); n > 0 {
		item.Lines = notes[len(notes)-n:]
	}
	if active := strings.TrimSpace(activeQuestionText(b.Notes)); active != "" {
		item.Question = active
	} else if len(item.Lines) > 0 {
		item.Question = item.Lines[len(item.Lines)-1]
	} else {
		item.Question = "(needs_me — no note text recorded)"
	}
	item.Critical = extractNoteCritical(b.Notes)
	if text, cn := extractNoteClass(b.Notes); cn >= 0 {
		item.Class = inboxIntPtr(cn)
		item.ClassText = text
	}
	if ev := buildNoteEvidence(b.Notes, pendingPlans); !ev.empty() {
		item.Evidence = ev
	}
	return item
}

func (s *tapServer) buildReadyPRItem(th Thread) inboxReadyPR {
	pr := th.PR
	class, _ := s.fullCfg.actionClass(th.Bead, "merge", "", "")
	if m := prURL.FindStringSubmatch(pr.URL); m != nil {
		_, name, _ := strings.Cut(m[1], "/")
		class = max(class, s.fullCfg.Autonomy.MergeClass[name])
	}
	safeURL := ""
	if isSafeHTTPURL(pr.URL) {
		safeURL = pr.URL
	}
	return inboxReadyPR{
		Bead:           th.Bead.ID,
		Title:          th.Bead.Title,
		Level:          th.Level,
		MergeClass:     class,
		PRNumber:       pr.Number,
		PRTitle:        pr.Title,
		PRURL:          safeURL,
		HeadSHA:        pr.HeadSHA,
		ShortSHA:       short(pr.HeadSHA),
		ChecksSummary:  checkSummary(th.Checks),
		ChecksPassing:  th.Checks.Passing,
		ChecksPending:  th.Checks.Pending,
		ChecksFailing:  slices.Clone(th.Checks.Failing),
		ReviewState:    approval(pr),
		ReviewDecision: pr.ReviewDecision,
		AutoMerge:      pr.AutoMerge != nil,
	}
}

func (s *tapServer) collectPendingPlans(activeByID map[string]Bead, now time.Time) []inboxPlan {
	entries, err := os.ReadDir(plansDir())
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.logf("tap: read plans dir: %v", err)
		}
		return []inboxPlan{}
	}
	out := []inboxPlan{}
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) < 8 {
			continue
		}
		p, digest, err := loadPlan(e.Name())
		if err != nil {
			continue
		}
		events, err := readPlanEvents(digest)
		if err != nil || len(events) == 0 {
			continue
		}
		b, known := activeByID[p.Bead]
		if !known && s.env.show != nil {
			if shown, serr := s.env.show(p.Bead); serr == nil {
				b = shown
			}
		}
		item, pending, _ := s.describePlan(p, digest, events, b, now)
		if pending {
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Class != out[j].Class {
			return out[i].Class > out[j].Class
		}
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].Digest < out[j].Digest
	})
	return out
}

// describePlan classifies a stored plan's approval state at now, returning
// pending=true when it is waiting for TJ approval (needs_me, expired, or
// checked with pass == false, and neither done/failed nor belonging to a
// closed bead).
func (s *tapServer) describePlan(p Plan, digest string, events []planEvent, b Bead, now time.Time) (inboxPlan, bool, string) {
	if len(events) == 0 {
		return inboxPlan{}, false, "plan has not been checked"
	}
	if b.Status == StatusClosed {
		return inboxPlan{}, false, p.Bead + " is closed"
	}
	status, why, pending, reason := planPendingStatus(events, now)
	if !pending {
		return inboxPlan{}, false, reason
	}

	if b.ID == "" {
		b.ID = p.Bead
	}
	steps := make([]inboxPlanStep, 0, len(p.Steps))
	topClass := 0
	for i, st := range p.Steps {
		c, stepWhy := s.fullCfg.stepClass(b, st)
		topClass = max(topClass, c)
		ps := inboxPlanStep{
			Number: i + 1,
			Op:     st.Op,
			Target: st.Target,
			Text:   st.Text,
			Class:  c,
			Why:    stepWhy,
		}
		if st.Op == "iac.apply" {
			ps.IaC = readPlanStepIaC(st)
		}
		steps = append(steps, ps)
	}
	// If the latest recorded event carried a higher class, don't under-report.
	lastEv := events[len(events)-1]
	if lastEv.Class > topClass {
		topClass = lastEv.Class
	}
	level := ""
	if b.Title != "" || b.Status != "" {
		level = s.fullCfg.beadMark(b)
	}
	approvePath := "/inbox/plan/" + digest
	return inboxPlan{
		Bead:        p.Bead,
		Title:       b.Title,
		Level:       level,
		Digest:      digest,
		ShortDigest: short(digest),
		Status:      status,
		Why:         why,
		State:       planState(events, digest, now),
		Class:       topClass,
		UpdatedAt:   lastEv.At,
		Steps:       steps,
		ApprovePath: approvePath,
		ApproveURL:  s.linkURL(approvePath),
	}, true, ""
}

func planPendingStatus(events []planEvent, now time.Time) (status, why string, pending bool, notPendingReason string) {
	if runEv, hasRun := lastRun(events); hasRun {
		switch runEv.Kind {
		case planDone:
			return "", "", false, "already done"
		case planFailed:
			return "", "", false, "run failed"
		case planNeedsMe, planExpired:
			if a, ok := liveApproval(events, now); ok && !a.At.Before(runEv.At) {
				return "", "", false, fmt.Sprintf("already approved by %s until %s", a.By, a.Expires.Format("15:04"))
			}
			return runEv.Kind, runEv.Why, true, ""
		default:
			return "", "", false, "run is in progress"
		}
	}
	if a, ok := liveApproval(events, now); ok {
		return "", "", false, fmt.Sprintf("already approved by %s until %s", a.By, a.Expires.Format("15:04"))
	}
	for _, e := range slices.Backward(events) {
		switch e.Kind {
		case planApproved:
			return planExpired, fmt.Sprintf("approval by %s expired at %s", e.By, e.Expires.Format("15:04")), true, ""
		case planChecked:
			if !e.Pass {
				return "waiting", e.Why, true, ""
			}
			return "", "", false, "passed check"
		}
	}
	return "", "", false, "not checked"
}

func readPlanStepIaC(st Step) *inboxPlanStepIaC {
	path := st.Args["preview"]
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	pc, err := checkPlan(data)
	if err != nil {
		return nil
	}
	_, stack, _ := strings.Cut(st.Target, ":")
	out := &inboxPlanStepIaC{
		Tool:    pc.Format,
		Target:  stack,
		Create:  pc.Counts["create"],
		Update:  pc.Counts["update"],
		Replace: pc.Counts["replace"],
		Delete:  pc.Counts["delete"] + pc.Counts["discard"],
		Counts:  pc.Counts,
		Summary: counts(pc),
	}
	for _, bl := range pc.Blocked {
		line := fmt.Sprintf("%s %s %s", bl.Op, orDash(bl.Type), bl.Name)
		if bl.Stateful {
			line += " (stateful)"
		}
		out.Blocked = append(out.Blocked, line)
	}
	return out
}

// Note parsing helpers for non-structured needs_me notes.

var (
	noteRecRe        = regexp.MustCompile(`(?i)\brecommendation:\s*(\d+)(?:\s*[—:-]\s*(.+))?`)
	notePlanDigestRe = regexp.MustCompile(`\b(?:plan\s+(?:approve|resume|status)\s+|digest[:\s]+)([0-9a-f]{8,64})\b`)
	noteGateEqRe     = regexp.MustCompile(`(?i)\b(create|update|replace|delete|destroy)\s*=\s*(\d+)\b`)
	noteGateNumOpRe  = regexp.MustCompile(`(?i)\b(\d+)\s+(create|update|replace|delete|destroy)s?\b`)
	noteGateOpNumRe  = regexp.MustCompile(`(?i)\b(create|update|replace|delete|destroy)\s+(\d+)\b`)
)

func extractNoteClass(notes string) (string, int) {
	lines := noteLines(notes)
	for i := len(lines) - 1; i >= 0; i-- {
		if text, n, ok := parseClassLine(lines[i]); ok && n >= 0 {
			return text, n
		}
	}
	return "", -1
}

func substantiveWaitingNotes(notes string) []string {
	after := linesAfterAnswer(noteLines(notes))
	var out []string
	for _, l := range after {
		if isNonAnswerHousekeepingNote(l) {
			continue
		}
		out = append(out, l)
	}
	return out
}

func extractNoteCritical(notes string) bool {
	if strings.HasPrefix(strings.TrimSpace(activeQuestionText(notes)), "CRITICAL:") {
		return true
	}
	lines := substantiveWaitingNotes(notes)
	start := max(0, len(lines)-waitingNotes(lines)-2)
	for i := len(lines) - 1; i >= start; i-- {
		if strings.HasPrefix(lines[i], "CRITICAL:") {
			return true
		}
	}
	return false
}

func extractNoteRecommendation(lines []string) *tapCardRecommendation {
	for _, l := range lines {
		if m := noteRecRe.FindStringSubmatch(l); m != nil {
			n, err := strconv.Atoi(m[1])
			if err == nil && n > 0 {
				return &tapCardRecommendation{
					Option:    n,
					Rationale: strings.TrimSpace(m[2]),
				}
			}
		}
	}
	return nil
}

func detectPlanDigest(notes string) string {
	lines := noteLines(notes)
	for i := len(lines) - 1; i >= 0; i-- {
		if m := notePlanDigestRe.FindStringSubmatch(lines[i]); m != nil {
			return m[1]
		}
	}
	return ""
}

func detectGateCounts(notes string) *tapIaCEvidence {
	lines := noteLines(notes)
	start := max(0, len(lines)-12)
	chunk := strings.Join(lines[start:], "\n")
	var ev tapIaCEvidence
	found := false
	add := func(op string, n int) {
		found = true
		switch strings.ToLower(op) {
		case "create":
			ev.Create = n
		case "update":
			ev.Update = n
		case "replace":
			ev.Replace = n
		case "delete", "destroy":
			ev.Delete = n
		}
	}
	for _, m := range noteGateEqRe.FindAllStringSubmatch(chunk, -1) {
		if n, err := strconv.Atoi(m[2]); err == nil {
			add(m[1], n)
		}
	}
	if !found {
		for _, m := range noteGateNumOpRe.FindAllStringSubmatch(chunk, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil {
				add(m[2], n)
			}
		}
	}
	if !found {
		for _, m := range noteGateOpNumRe.FindAllStringSubmatch(chunk, -1) {
			if n, err := strconv.Atoi(m[2]); err == nil {
				add(m[1], n)
			}
		}
	}
	if !found {
		return nil
	}
	ev.Summary = fmt.Sprintf("%d create, %d update, %d replace, %d delete", ev.Create, ev.Update, ev.Replace, ev.Delete)
	return &ev
}

func questionCardAnswered(notes, hash string) bool {
	if hash == "" {
		return false
	}
	needle := "[" + hash + "]"
	for _, l := range noteLines(notes) {
		if strings.HasPrefix(l, "NEEDS_ME ANSWERED:") && strings.Contains(l, needle) {
			return true
		}
	}
	return false
}

func buildNoteEvidence(notes string, pendingPlans map[string]string) *inboxEvidence {
	ev := &inboxEvidence{}
	if prs := NotedPRs(notes); len(prs) > 0 {
		last := prs[len(prs)-1]
		if isSafeHTTPURL(last) {
			ev.PRURL = last
		}
	}
	if d := detectPlanDigest(notes); d != "" {
		ev.PlanDigest = d
		if path := resolvePendingPlanPath(d, pendingPlans); path != "" {
			ev.PlanApprovePath = path
		}
	}
	if iac := detectGateCounts(notes); iac != nil {
		ev.IaC = iac
	}
	return ev
}

func mergeEvidence(cardEv *tapCardEvidence, notes string, pendingPlans map[string]string) *inboxEvidence {
	ev := buildNoteEvidence(notes, pendingPlans)
	if cardEv == nil {
		if ev.empty() {
			return nil
		}
		return ev
	}
	if isSafeHTTPURL(cardEv.PRURL) {
		ev.PRURL = cardEv.PRURL
	}
	if cardEv.PlanDigest != "" {
		ev.PlanDigest = cardEv.PlanDigest
		ev.PlanApprovePath = resolvePendingPlanPath(cardEv.PlanDigest, pendingPlans)
	}
	if cardEv.CostEstimate != "" {
		ev.CostEstimate = cardEv.CostEstimate
	}
	for _, u := range cardEv.RunURLs {
		if isSafeHTTPURL(u) {
			ev.RunURLs = append(ev.RunURLs, u)
		}
	}
	for _, l := range cardEv.Links {
		if isSafeHTTPURL(l.URL) {
			ev.Links = append(ev.Links, tapCardLink{Label: cmp.Or(strings.TrimSpace(l.Label), l.URL), URL: l.URL})
		}
	}
	if cardEv.IaC != nil {
		cp := *cardEv.IaC
		if cp.Summary == "" {
			cp.Summary = fmt.Sprintf("%d create, %d update, %d replace, %d delete", cp.Create, cp.Update, cp.Replace, cp.Delete)
		}
		ev.IaC = &cp
	}
	if ev.empty() {
		return nil
	}
	return ev
}

func resolvePendingPlanPath(digest string, pendingPlans map[string]string) string {
	if path, ok := pendingPlans[digest]; ok {
		return path
	}
	if len(digest) >= 8 {
		for k, path := range pendingPlans {
			if strings.HasPrefix(k, digest) {
				return path
			}
		}
	}
	return ""
}

func formatClassText(n int, reason string) string {
	if reason != "" {
		return fmt.Sprintf("%d — %s", n, reason)
	}
	return strconv.Itoa(n)
}

func inboxIntPtr(v int) *int { return &v }

var tapInboxFuncs = template.FuncMap{
	"joinLines": func(lines []string) string { return strings.Join(lines, "\n") },
	"classBadge": func(c *int, text string) string {
		if c == nil {
			return ""
		}
		num := strconv.Itoa(*c)
		reason := strings.TrimSpace(text)
		if rest, ok := strings.CutPrefix(reason, num); ok {
			reason = strings.TrimSpace(strings.TrimLeft(rest, "—-:· "))
		}
		if reason != "" {
			return fmt.Sprintf("C%d — %s", *c, reason)
		}
		return "C" + num
	},
	"classNum": func(c *int) string {
		if c == nil {
			return ""
		}
		return fmt.Sprintf("C%d", *c)
	},
	"classTone": func(c *int, critical bool) string {
		if critical {
			return "crit"
		}
		if c == nil {
			return "neutral"
		}
		switch {
		case *c >= 8:
			return "crit"
		case *c == 7:
			return "high"
		case *c >= 4:
			return "med"
		default:
			return "low"
		}
	},
	"planClassTone": func(c int) string {
		switch {
		case c >= 8:
			return "crit"
		case c == 7:
			return "high"
		case c >= 4:
			return "med"
		default:
			return "low"
		}
	},
	"shortDigest": short,
	"fmtTime": func(v any) string {
		switch t := v.(type) {
		case time.Time:
			if t.IsZero() {
				return ""
			}
			return t.Local().Format("15:04")
		case *time.Time:
			if t == nil || t.IsZero() {
				return ""
			}
			return t.Local().Format("15:04")
		default:
			return ""
		}
	},
	"fmtStamp": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Local().Format("2006-01-02 15:04:05 MST")
	},
	"milestoneTone": func(status string) string {
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "ok":
			return "ok"
		case "warn":
			return "warn"
		case "blocked":
			return "crit"
		default:
			return "info"
		}
	},
}

var tapInboxTmpl = template.Must(template.New("inbox").Funcs(tapInboxFuncs).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Quartermaster Inbox{{if gt .Counts.Total 0}} ({{.Counts.Total}}){{end}}</title>
<style>
:root{
  --bg:#f6f8fa;--surface:#ffffff;--surface-alt:#f0f3f6;--border:#d0d7de;
  --text:#1f2328;--muted:#59636e;--accent:#0969da;--accent-bg:#ddf4ff;
  --ok:#1a7f37;--ok-bg:#dafbe1;--warn:#9a6700;--warn-bg:#fff8c5;
  --high:#bc4c00;--high-bg:#fff1e5;--crit:#cf222e;--crit-bg:#ffebe9;
  --radius:12px;
}
@media (prefers-color-scheme:dark){
  :root{
    --bg:#0d1117;--surface:#161b22;--surface-alt:#1f2630;--border:#30363d;
    --text:#e6edf3;--muted:#8b949e;--accent:#58a6ff;--accent-bg:#122d4a;
    --ok:#3fb950;--ok-bg:#122f1e;--warn:#d29922;--warn-bg:#33260b;
    --high:#f0883e;--high-bg:#3b200c;--crit:#f85149;--crit-bg:#3d1418;
  }
}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);font-family:system-ui,-apple-system,Segoe UI,Roboto,sans-serif;line-height:1.45;-webkit-text-size-adjust:100%}
a{color:var(--accent);text-decoration:none}
a:hover{text-decoration:underline}
.wrap{max-width:52rem;margin:0 auto;padding:12px 14px 36px}
header{position:sticky;top:0;z-index:10;background:var(--bg);padding:10px 0 12px;border-bottom:1px solid var(--border);margin-bottom:16px}
.top{display:flex;align-items:center;justify-content:space-between;gap:10px;flex-wrap:wrap}
h1{font-size:1.25rem;margin:0;letter-spacing:-0.01em}
.controls{display:flex;align-items:center;gap:8px;flex-wrap:wrap}
.btn{display:inline-flex;align-items:center;justify-content:center;gap:6px;min-height:38px;padding:7px 13px;border-radius:9px;border:1px solid var(--border);background:var(--surface);color:var(--text);font-size:.9rem;font-weight:600;cursor:pointer;text-decoration:none}
.btn:hover{text-decoration:none;border-color:var(--accent)}
.btn-primary{background:var(--accent);border-color:var(--accent);color:#fff}
.btn-primary:hover{opacity:.92}
.toggle{display:inline-flex;align-items:center;gap:5px;font-size:.82rem;color:var(--muted);padding:4px 8px;border-radius:8px;border:1px solid var(--border);background:var(--surface);cursor:pointer;user-select:none}
.badges{display:flex;gap:8px;margin-top:10px;overflow-x:auto;padding-bottom:2px}
.pill{display:inline-flex;align-items:center;gap:6px;padding:5px 10px;border-radius:999px;border:1px solid var(--border);background:var(--surface);color:var(--text);font-size:.82rem;font-weight:600;white-space:nowrap}
.pill .n{display:inline-flex;align-items:center;justify-content:center;min-width:20px;height:20px;padding:0 6px;border-radius:999px;background:var(--surface-alt);font-size:.76rem}
.pill.has .n{background:var(--accent);color:#fff}
.warn-banner{background:var(--warn-bg);color:var(--warn);border:1px solid var(--warn);border-radius:var(--radius);padding:10px 12px;margin-bottom:14px;font-size:.88rem}
.empty-all{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:24px 16px;text-align:center;color:var(--muted);margin:14px 0}
section{margin:18px 0 24px}
.sec-head{display:flex;align-items:baseline;justify-content:space-between;margin:0 0 10px}
.sec-head h2{font-size:1rem;margin:0;text-transform:uppercase;letter-spacing:.04em;color:var(--muted)}
.sec-empty{color:var(--muted);font-size:.88rem;padding:10px 12px;background:var(--surface);border:1px dashed var(--border);border-radius:var(--radius)}
.card{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:14px;margin-bottom:12px;box-shadow:0 1px 2px rgba(0,0,0,.04)}
.card.crit{border-color:var(--crit);box-shadow:0 0 0 1px var(--crit)}
.card.high{border-color:var(--high)}
.card-meta{display:flex;align-items:center;gap:6px;flex-wrap:wrap;margin-bottom:6px;font-size:.8rem;color:var(--muted)}
.bead{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-weight:700;color:var(--text);font-size:.85rem}
.tag{display:inline-flex;align-items:center;padding:2px 7px;border-radius:6px;font-size:.75rem;font-weight:700;background:var(--surface-alt);color:var(--muted);border:1px solid var(--border)}
.tag.low{background:var(--ok-bg);color:var(--ok);border-color:transparent}
.tag.med{background:var(--accent-bg);color:var(--accent);border-color:transparent}
.tag.high{background:var(--high-bg);color:var(--high);border-color:transparent}
.tag.crit{background:var(--crit-bg);color:var(--crit);border-color:var(--crit)}
.tag.warn{background:var(--warn-bg);color:var(--warn);border-color:transparent}
.tag.ok{background:var(--ok-bg);color:var(--ok);border-color:transparent}
.card-title{font-size:1.02rem;font-weight:700;margin:2px 0 6px}
.card-sub{font-size:.88rem;color:var(--muted);margin:0 0 8px}
.details{font-size:.92rem;margin:6px 0 10px;white-space:pre-wrap}
pre{margin:8px 0;padding:10px 12px;background:var(--surface-alt);border:1px solid var(--border);border-radius:9px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.84rem;white-space:pre-wrap;word-break:break-word;overflow-x:auto}
.rec{margin:10px 0;padding:10px 12px;border-radius:9px;background:var(--accent-bg);border-left:4px solid var(--accent);font-size:.9rem}
.rec strong{display:block;margin-bottom:2px}
.ev-card{margin:10px 0;padding:10px 12px;border-radius:9px;background:var(--surface-alt);border:1px solid var(--border);font-size:.86rem}
.ev-title{font-weight:700;margin-bottom:6px;display:flex;align-items:center;justify-content:space-between;gap:8px;flex-wrap:wrap}
.iac-counts{display:flex;gap:6px;flex-wrap:wrap;margin:6px 0}
.iac-pill{display:inline-flex;align-items:center;padding:2px 8px;border-radius:999px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.78rem;font-weight:700}
.iac-c{background:var(--ok-bg);color:var(--ok)}
.iac-u{background:var(--accent-bg);color:var(--accent)}
.iac-r{background:var(--high-bg);color:var(--high)}
.iac-d{background:var(--crit-bg);color:var(--crit)}
.ev-links{display:flex;gap:8px;flex-wrap:wrap;margin-top:6px}
.ev-link{display:inline-flex;align-items:center;padding:4px 9px;border-radius:7px;border:1px solid var(--border);background:var(--surface);font-size:.82rem;font-weight:600}
.opts{display:flex;flex-direction:column;gap:8px;margin-top:10px}
.opt-btn{display:flex;flex-direction:column;align-items:flex-start;gap:3px;width:100%;min-height:46px;padding:10px 13px;border-radius:10px;border:1px solid var(--border);background:var(--surface-alt);color:var(--text);text-decoration:none}
.opt-btn:hover{border-color:var(--accent);text-decoration:none}
.opt-btn.rec-opt{border-color:var(--accent);background:var(--accent-bg)}
.opt-btn.readonly{cursor:default}
.opt-top{display:flex;align-items:center;gap:6px;flex-wrap:wrap;width:100%;font-weight:600;font-size:.94rem}
.opt-desc{font-size:.82rem;color:var(--muted)}
.run-box{position:relative}
.copy-btn{min-height:34px;padding:4px 10px;font-size:.8rem}
.steps{margin:8px 0;padding-left:0;list-style:none;display:flex;flex-direction:column;gap:6px}
.step{padding:8px 10px;border-radius:8px;background:var(--surface-alt);border:1px solid var(--border);font-size:.86rem}
.step-head{display:flex;align-items:center;gap:6px;flex-wrap:wrap;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-weight:600}
.step-why{font-size:.82rem;color:var(--muted);margin-top:3px}
.milestones{margin-top:10px;padding-top:8px;border-top:1px solid var(--border)}
.ms-title{font-size:.78rem;font-weight:700;text-transform:uppercase;letter-spacing:.04em;color:var(--muted);margin-bottom:6px}
.ms-list{list-style:none;margin:0;padding:0;display:flex;flex-direction:column;gap:5px;font-size:.83rem}
.ms-item{display:flex;align-items:baseline;gap:7px;flex-wrap:wrap}
.ms-dot{width:8px;height:8px;border-radius:50%;flex:none;background:var(--muted);display:inline-block}
.ms-dot.ok{background:var(--ok)}
.ms-dot.warn{background:var(--warn)}
.ms-dot.crit{background:var(--crit)}
.ms-dot.info{background:var(--accent)}
.hint{font-size:.78rem;color:var(--muted);margin-top:8px}
.actions{display:flex;gap:8px;flex-wrap:wrap;margin-top:10px}
.parked-box{margin-top:14px;border:1px solid var(--border);border-radius:var(--radius);background:var(--surface);padding:10px 12px}
.parked-box summary{cursor:pointer;font-weight:700;font-size:.9rem;color:var(--muted);list-style:none;display:flex;align-items:center;justify-content:space-between;gap:8px}
.parked-box summary::-webkit-details-marker{display:none}
.parked-box summary::after{content:"Show";font-size:.78rem;font-weight:600;color:var(--accent)}
.parked-box[open] summary::after{content:"Hide"}
.parked-box[open] summary{margin-bottom:12px;padding-bottom:8px;border-bottom:1px solid var(--border)}
.card.parked{opacity:.92;border-style:dashed}
footer{margin-top:24px;padding-top:14px;border-top:1px solid var(--border);font-size:.82rem;color:var(--muted);display:flex;justify-content:space-between;gap:10px;flex-wrap:wrap}
</style>
</head>
<body>
<div class="wrap">
<header>
  <div class="top">
    <h1>Quartermaster Inbox</h1>
    <div class="controls">
      <a class="btn" href="/inbox">Refresh</a>
      <label class="toggle"><input type="checkbox" id="qm-autorefresh"> Auto (30s)</label>
    </div>
  </div>
  <nav class="badges" aria-label="Queue summary">
    <a class="pill{{if gt .Counts.Prompts 0}} has{{end}}" href="#prompts">Prompts <span class="n">{{.Counts.Prompts}}</span></a>
    <a class="pill{{if gt .Counts.Questions 0}} has{{end}}" href="#questions">Questions <span class="n">{{.Counts.Questions}}</span></a>
    {{if gt .Counts.ParkedQuestions 0}}<a class="pill" href="#parked">Parked <span class="n">{{.Counts.ParkedQuestions}}</span></a>{{end}}
    <a class="pill{{if gt .Counts.Plans 0}} has{{end}}" href="#plans">Plans <span class="n">{{.Counts.Plans}}</span></a>
    <a class="pill{{if gt .Counts.ReadyToMerge 0}} has{{end}}" href="#ready">Ready to Merge <span class="n">{{.Counts.ReadyToMerge}}</span></a>
  </nav>
</header>

{{range .Warnings}}
<div class="warn-banner">{{.}}</div>
{{end}}

{{if eq .Counts.Total 0}}
<div class="empty-all">All caught up — no active approval prompts, questions, pending plans, or ready-to-merge PRs right now.{{if gt .Counts.ParkedQuestions 0}} ({{.Counts.ParkedQuestions}} parked / dormant below.){{end}}</div>
{{end}}

<section id="prompts">
  <div class="sec-head"><h2>Approval Prompts ({{.Counts.Prompts}})</h2></div>
  {{if eq (len .Prompts) 0}}
    <div class="sec-empty">No workers blocked at a parsed approval prompt.</div>
  {{end}}
  {{range .Prompts}}
  <article class="card{{if .Countdown}} crit{{end}}">
    <div class="card-meta">
      <span class="bead">{{.Bead}}</span>
      {{if .Level}}<span class="tag">{{.Level}}</span>{{end}}
      {{if .AgentKind}}<span class="tag">{{.AgentKind}} {{.AgentStatus}}</span>{{end}}
      <span class="tag">[{{.Hash}}]</span>
      {{if .Countdown}}<span class="tag crit">auto-deny in {{.Countdown}}</span>{{end}}
    </div>
    {{if .Title}}<div class="card-title">{{.Title}}</div>{{end}}
    <pre>{{joinLines .Lines}}</pre>
    <div class="opts">
      {{range .Options}}
      <a class="opt-btn" href="{{.Path}}">
        <span class="opt-top">{{.Number}}. {{.Label}}</span>
      </a>
      {{end}}
    </div>
    <div class="hint">Tapping opens a live re-read confirm page before answering (one-time links expire {{fmtTime .Expires}}).</div>
  </article>
  {{end}}
</section>

<section id="questions">
  <div class="sec-head"><h2>Needs-Me Questions &amp; Cards{{if gt .Counts.ParkedQuestions 0}} — Active{{end}} ({{.Counts.Questions}})</h2></div>
  {{if eq (len .Questions) 0}}
    <div class="sec-empty">No active beads waiting on a human decision or command.</div>
  {{end}}
  {{range .Questions}}{{template "questionCard" .}}{{end}}
  {{if gt .Counts.ParkedQuestions 0}}
  <details id="parked" class="parked-box">
    <summary>Parked / Dormant ({{.ParkedSummary}})</summary>
    {{range .ParkedQuestions}}{{template "questionCard" .}}{{end}}
  </details>
  {{end}}
</section>
{{define "questionCard"}}
  {{$tone := classTone .Class .Critical}}
  <article class="card{{if eq $tone "crit"}} crit{{else if eq $tone "high"}} high{{end}}{{if .Parked}} parked{{end}}">
    <div class="card-meta">
      <span class="bead">{{.Bead}}</span>
      {{if .Level}}<span class="tag">{{.Level}}</span>{{end}}
      {{if .Parked}}<span class="tag">{{if .ParkedReason}}{{.ParkedReason}}{{else}}parked{{end}}</span>{{end}}
      {{if .Critical}}<span class="tag crit">CRITICAL</span>{{end}}
      {{if .Class}}<span class="tag {{$tone}}">{{classBadge .Class .ClassText}}</span>{{end}}
      {{if eq .Kind "run"}}<span class="tag warn">RUN</span>
      {{else if eq .Kind "structured"}}<span class="tag med">card</span>
      {{else if eq .Kind "options"}}<span class="tag med">options</span>
      {{else if .Tappable}}<span class="tag med">free text</span>
      {{else}}<span class="tag">read-only</span>{{end}}
    </div>
    {{if .Title}}<div class="card-title">{{.Title}}</div>{{end}}
    <div class="details">{{.Question}}</div>
    {{if .Details}}<div class="details">{{.Details}}</div>{{end}}

    {{if .RunCommand}}
    <div class="run-box">
      <pre id="run-{{.Bead}}">{{.RunCommand}}</pre>
      <div class="actions">
        <button type="button" class="btn copy-btn" data-copy="{{.RunCommand}}">Copy command</button>
      </div>
    </div>
    {{end}}

    {{if .Recommendation}}
    <div class="rec">
      <strong>Recommendation: Option {{.Recommendation.Option}}</strong>
      {{if .Recommendation.Rationale}}{{.Recommendation.Rationale}}{{end}}
    </div>
    {{end}}

    {{if .Evidence}}
    <div class="ev-card">
      {{if .Evidence.IaC}}
      <div class="ev-title">
        <span>IaC Preview{{if .Evidence.IaC.Tool}} · {{.Evidence.IaC.Tool}}{{end}}{{if .Evidence.IaC.Target}} ({{.Evidence.IaC.Target}}){{end}}</span>
        {{if .Evidence.IaC.Summary}}<span class="tag">{{.Evidence.IaC.Summary}}</span>{{end}}
      </div>
      <div class="iac-counts">
        <span class="iac-pill iac-c">+{{.Evidence.IaC.Create}} create</span>
        <span class="iac-pill iac-u">~{{.Evidence.IaC.Update}} update</span>
        <span class="iac-pill iac-r">↻{{.Evidence.IaC.Replace}} replace</span>
        <span class="iac-pill iac-d">-{{.Evidence.IaC.Delete}} delete</span>
      </div>
      {{if .Evidence.IaC.Excerpt}}<pre>{{.Evidence.IaC.Excerpt}}</pre>{{end}}
      {{end}}

      {{if .Evidence.CostEstimate}}
      <div class="card-sub"><strong>Cost estimate:</strong> {{.Evidence.CostEstimate}}</div>
      {{end}}

      {{if .Evidence.PlanDigest}}
      <div class="card-sub">
        <strong>Plan digest:</strong> <code>{{shortDigest .Evidence.PlanDigest}}</code>
        {{if .Evidence.PlanApprovePath}} · <a href="{{.Evidence.PlanApprovePath}}">Review &amp; approve plan →</a>{{end}}
      </div>
      {{end}}

      <div class="ev-links">
        {{if .Evidence.PRURL}}<a class="ev-link" href="{{.Evidence.PRURL}}" target="_blank" rel="noopener noreferrer">Open PR ↗</a>{{end}}
        {{range $idx, $u := .Evidence.RunURLs}}<a class="ev-link" href="{{$u}}" target="_blank" rel="noopener noreferrer">Run ↗</a>{{end}}
        {{range .Evidence.Links}}<a class="ev-link" href="{{.URL}}" target="_blank" rel="noopener noreferrer">{{.Label}} ↗</a>{{end}}
      </div>
    </div>
    {{end}}

    {{if gt (len .Options) 0}}
    <div class="opts">
      {{range .Options}}
        {{if .Path}}
        <a class="opt-btn{{if .Recommended}} rec-opt{{end}}" href="{{.Path}}">
          <span class="opt-top">
            <span>{{.Number}}. {{.Label}}</span>
            {{if .Recommended}}<span class="tag med">Recommended</span>{{end}}
            {{if .Class}}<span class="tag {{classTone .Class false}}">{{classNum .Class}}</span>{{end}}
          </span>
          {{if .Description}}<span class="opt-desc">{{.Description}}</span>{{end}}
        </a>
        {{else}}
        <div class="opt-btn readonly{{if .Recommended}} rec-opt{{end}}">
          <span class="opt-top">
            <span>{{.Number}}. {{.Label}}</span>
            {{if .Recommended}}<span class="tag med">Recommended</span>{{end}}
            {{if .Class}}<span class="tag {{classTone .Class false}}">{{classNum .Class}}</span>{{end}}
          </span>
          {{if .Description}}<span class="opt-desc">{{.Description}}</span>{{end}}
        </div>
        {{end}}
      {{end}}
      {{if .FreeText}}
      <a class="opt-btn" href="{{.FreeText.Path}}">
        <span class="opt-top">Answer with free text…</span>
        <span class="opt-desc">Write prose instead of or alongside an option (up to {{.FreeText.MaxChars}} chars)</span>
      </a>
      {{end}}
    </div>
    {{if .Expires}}
    <div class="hint">Tapping opens a live re-read confirm page with an optional free-text field (one-time links expire {{fmtTime .Expires}}).</div>
    {{else}}
    <div class="hint">Read-only card (no live OPTIONS note on bead for one-time tap links).</div>
    {{end}}
    {{else if eq .Kind "free_text"}}
    {{if gt (len .Lines) 1}}<pre>{{joinLines .Lines}}</pre>{{end}}
    {{if .FreeText}}
    <div class="actions">
      <a class="btn btn-primary" href="{{.FreeText.Path}}">Answer with free text →</a>
    </div>
    <div class="hint">Opens a live re-read confirm page with a free-text field (up to {{.FreeText.MaxChars}} chars; one-time link expires {{fmtTime .Expires}}).</div>
    {{else}}
    <div class="hint">Read-only needs_me note — reply via coordinator or worker terminal.</div>
    {{end}}
    {{end}}

    {{if gt (len .Milestones) 0}}
    <div class="milestones">
      <div class="ms-title">Recent Milestones</div>
      <ul class="ms-list">
        {{range .Milestones}}
        <li class="ms-item">
          <span class="ms-dot {{milestoneTone .Status}}"></span>
          {{if .Phase}}<strong>{{.Phase}}</strong>{{end}}
          <span>{{.Summary}}</span>
          {{if .At}}<span class="card-sub" style="margin:0">{{.At}}</span>{{end}}
        </li>
        {{end}}
      </ul>
    </div>
    {{end}}
  </article>
{{end}}

<section id="plans">
  <div class="sec-head"><h2>Pending Checked Plans ({{.Counts.Plans}})</h2></div>
  {{if eq (len .Plans) 0}}
    <div class="sec-empty">No checked execution plans awaiting TJ approval.</div>
  {{end}}
  {{range .Plans}}
  {{$tone := planClassTone .Class}}
  <article class="card{{if eq $tone "crit"}} crit{{else if eq $tone "high"}} high{{end}}">
    <div class="card-meta">
      <span class="bead">{{.Bead}}</span>
      {{if .Level}}<span class="tag">{{.Level}}</span>{{end}}
      <span class="tag {{$tone}}">C{{.Class}}</span>
      <span class="tag warn">{{.Status}}</span>
      <span class="tag">{{.ShortDigest}}</span>
    </div>
    {{if .Title}}<div class="card-title">{{.Title}}</div>{{end}}
    {{if .Why}}<div class="card-sub">{{.Why}}</div>{{end}}
    <ul class="steps">
      {{range .Steps}}
      <li class="step">
        <div class="step-head">
          <span>{{.Number}}. {{.Op}}</span>
          {{if .Target}}<span>{{.Target}}</span>{{end}}
          <span class="tag {{planClassTone .Class}}">C{{.Class}}</span>
        </div>
        {{if .Text}}<pre>{{.Text}}</pre>{{end}}
        {{if .Why}}<div class="step-why">{{.Why}}</div>{{end}}
        {{if .IaC}}
        <div class="iac-counts">
          <span class="iac-pill iac-c">+{{.IaC.Create}} create</span>
          <span class="iac-pill iac-u">~{{.IaC.Update}} update</span>
          <span class="iac-pill iac-r">↻{{.IaC.Replace}} replace</span>
          <span class="iac-pill iac-d">-{{.IaC.Delete}} delete</span>
        </div>
        {{if gt (len .IaC.Blocked) 0}}<pre>{{joinLines .IaC.Blocked}}</pre>{{end}}
        {{end}}
      </li>
      {{end}}
    </ul>
    <div class="actions">
      <a class="btn btn-primary" href="{{.ApprovePath}}">Review &amp; Approve Plan →</a>
    </div>
  </article>
  {{end}}
</section>

<section id="ready">
  <div class="sec-head"><h2>Ready-to-Merge PRs ({{.Counts.ReadyToMerge}})</h2></div>
  {{if eq (len .ReadyToMerge) 0}}
    <div class="sec-empty">No PRs currently in ready-to-merge state.</div>
  {{end}}
  {{range .ReadyToMerge}}
  <article class="card">
    <div class="card-meta">
      <span class="bead">{{.Bead}}</span>
      {{if .Level}}<span class="tag">{{.Level}}</span>{{end}}
      <span class="tag med">merge C{{.MergeClass}}</span>
      <span class="tag ok">{{.ReviewState}}</span>
      {{if .AutoMerge}}<span class="tag">auto-merge on</span>{{end}}
    </div>
    <div class="card-title">PR #{{.PRNumber}}{{if .PRTitle}}: {{.PRTitle}}{{else if .Title}}: {{.Title}}{{end}}</div>
    <div class="card-sub">
      Head <code>{{.ShortSHA}}</code>{{if .ChecksSummary}} · {{.ChecksSummary}}{{end}}
    </div>
    <div class="actions">
      {{if .PRURL}}<a class="btn" href="{{.PRURL}}" target="_blank" rel="noopener noreferrer">Open on GitHub ↗</a>{{end}}
    </div>
  </article>
  {{end}}
</section>

<footer>
  <span>Viewing as TJ from {{.Viewer.Device}}</span>
  <span>Updated {{fmtStamp .GeneratedAt}} · <a href="/inbox.json">JSON</a></span>
</footer>
</div>
<script>
(function(){
  var cb = document.getElementById('qm-autorefresh');
  if (cb) {
    try {
      cb.checked = sessionStorage.getItem('qm_inbox_autorefresh') === '1';
      var timer = null;
      function sync(){
        if (timer) { clearInterval(timer); timer = null; }
        if (cb.checked) { timer = setInterval(function(){ location.reload(); }, 30000); }
      }
      cb.addEventListener('change', function(){
        try { sessionStorage.setItem('qm_inbox_autorefresh', cb.checked ? '1' : '0'); } catch(e) {}
        sync();
      });
      sync();
    } catch(e) {}
  }
  document.querySelectorAll('[data-copy]').forEach(function(btn){
    btn.addEventListener('click', function(){
      var text = btn.getAttribute('data-copy') || '';
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(function(){
          var old = btn.textContent;
          btn.textContent = 'Copied';
          setTimeout(function(){ btn.textContent = old; }, 1500);
        });
      }
    });
  });
})();
</script>
</body>
</html>`))

var tapPlanConfirmTmpl = template.Must(template.New("plan-confirm").Funcs(tapInboxFuncs).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Approve plan {{.Plan.ShortDigest}}?</title>
<style>
body{font-family:system-ui,-apple-system,sans-serif;margin:1.2em auto;max-width:46rem;padding:0 12px;line-height:1.45}
.meta{display:flex;gap:8px;flex-wrap:wrap;margin:8px 0 12px;font-size:.86rem}
.tag{padding:2px 8px;border-radius:6px;background:#eef2f6;font-weight:700}
.tag.crit{background:#ffebe9;color:#cf222e}
.tag.high{background:#fff1e5;color:#bc4c00}
pre{white-space:pre-wrap;word-break:break-word;background:#f4f4f4;padding:.8em;border-radius:.5em}
.step{border:1px solid #d0d7de;border-radius:8px;padding:10px;margin:8px 0}
.iac{display:flex;gap:8px;flex-wrap:wrap;margin-top:6px;font-family:ui-monospace,monospace;font-size:.82rem;font-weight:700}
button{font-size:1.1em;font-weight:700;padding:.8em 1.2em;width:100%;border-radius:10px;border:none;background:#0969da;color:#fff;cursor:pointer;margin-top:12px}
@media (prefers-color-scheme:dark){body{background:#0d1117;color:#e6edf3}.tag{background:#21262d;color:#e6edf3}.tag.crit{background:#3d1418;color:#f85149}.tag.high{background:#3b200c;color:#f0883e}pre{background:#161b22;color:#e6edf3}.step{border-color:#30363d}a{color:#58a6ff}}
</style>
</head>
<body>
<h1>Approve plan {{.Plan.ShortDigest}}?</h1>
<div class="meta">
  <span class="tag">{{.Plan.Bead}}</span>
  <span class="tag {{planClassTone .Plan.Class}}">C{{.Plan.Class}}</span>
  <span class="tag">{{.Plan.Status}}</span>
</div>
{{if .Plan.Title}}<p><strong>{{.Plan.Title}}</strong></p>{{end}}
{{if .Plan.Why}}<p>{{.Plan.Why}}</p>{{end}}
<p><strong>Full digest:</strong> <code>{{.Plan.Digest}}</code></p>
{{range .Plan.Steps}}
<div class="step">
  <div><strong>{{.Number}}. {{.Op}}</strong> {{.Target}} <span class="tag {{planClassTone .Class}}">C{{.Class}}</span></div>
  {{if .Text}}<pre>{{.Text}}</pre>{{end}}
  {{if .Why}}<div>{{.Why}}</div>{{end}}
  {{if .IaC}}
  <div class="iac">
    <span>+{{.IaC.Create}} create</span>
    <span>~{{.IaC.Update}} update</span>
    <span>↻{{.IaC.Replace}} replace</span>
    <span>-{{.IaC.Delete}} delete</span>
  </div>
  {{if gt (len .IaC.Blocked) 0}}<pre>{{joinLines .IaC.Blocked}}</pre>{{end}}
  {{end}}
</div>
{{end}}
<form method="post" action="/inbox/plan/{{.Plan.Digest}}">
  <input type="hidden" name="token" value="{{.Token}}">
  <button type="submit">Approve plan {{.Plan.ShortDigest}} as TJ (1h)</button>
</form>
<p>You are approving as TJ from {{.Device}} (confirmation token expires {{fmtTime .Expires}}). Anything else: close this page.</p>
<p><a href="/inbox">Back to Inbox</a></p>
</body>
</html>`))

var tapPlanApprovedTmpl = template.Must(template.New("plan-approved").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Plan {{.Short}} approved</title>
<style>
body{font-family:system-ui,-apple-system,sans-serif;margin:1.2em auto;max-width:44rem;padding:0 12px;line-height:1.45}
pre{white-space:pre-wrap;word-break:break-word;background:#f4f4f4;padding:.8em;border-radius:.5em}
@media (prefers-color-scheme:dark){body{background:#0d1117;color:#e6edf3}pre{background:#161b22;color:#e6edf3}a{color:#58a6ff}}
</style>
</head>
<body>
<h1>Plan {{.Short}} approved</h1>
<pre>{{.Message}}</pre>
<p>Approved as TJ from {{.Device}}.</p>
<p><a href="/inbox">Back to Inbox</a></p>
</body>
</html>`))
