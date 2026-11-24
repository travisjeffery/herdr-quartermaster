package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Structured ask_user and report_milestone cards.
//
// Each bead's active question card lives at <state dir>/cards/<bead>/question.json
// and its append-only milestone log at <state dir>/cards/<bead>/milestones.jsonl.
// Question cards mirror the QUESTION:/OPTIONS:/CLASS: bead-note convention and
// compute their Hash through parseNeedsMeQuestion so approval taps and web views
// share one identity for a live question.

const cardSchemaVersion = 1

type QuestionCard struct {
	Version        int                 `json:"version"`
	Bead           string              `json:"bead"`
	CreatedAt      time.Time           `json:"created_at"`
	Hash           string              `json:"hash"`
	Question       string              `json:"question"`
	Details        string              `json:"details,omitempty"`
	Critical       bool                `json:"critical"`
	Class          int                 `json:"class"`
	ClassReason    string              `json:"class_reason,omitempty"`
	Recommendation *CardRecommendation `json:"recommendation,omitempty"`
	Options        []CardOption        `json:"options"`
	Evidence       *CardEvidence       `json:"evidence,omitempty"`
}

type CardRecommendation struct {
	Option    int    `json:"option"`
	Rationale string `json:"rationale,omitempty"`
}

type CardOption struct {
	Number      int    `json:"number"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Class       *int   `json:"class,omitempty"`
}

type CardEvidence struct {
	PRURL        string           `json:"pr_url,omitempty"`
	PlanDigest   string           `json:"plan_digest,omitempty"`
	RunURLs      []string         `json:"run_urls,omitempty"`
	CostEstimate string           `json:"cost_estimate,omitempty"`
	IAC          *CardIACEvidence `json:"iac,omitempty"`
	Links        []CardLink       `json:"links,omitempty"`
}

type CardIACEvidence struct {
	Tool    string `json:"tool,omitempty"`
	Target  string `json:"target,omitempty"`
	Create  int    `json:"create"`
	Update  int    `json:"update"`
	Replace int    `json:"replace"`
	Delete  int    `json:"delete"`
	Summary string `json:"summary,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

type CardLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type MilestoneRecord struct {
	Version  int                `json:"version"`
	At       time.Time          `json:"at"`
	Bead     string             `json:"bead"`
	Phase    string             `json:"phase"`
	Status   string             `json:"status"`
	Summary  string             `json:"summary"`
	Evidence *MilestoneEvidence `json:"evidence,omitempty"`
}

type MilestoneEvidence struct {
	PRURL      string     `json:"pr_url,omitempty"`
	RunURL     string     `json:"run_url,omitempty"`
	SHA        string     `json:"sha,omitempty"`
	PlanDigest string     `json:"plan_digest,omitempty"`
	Links      []CardLink `json:"links,omitempty"`
}

var validMilestoneStatuses = map[string]bool{
	"info":    true,
	"ok":      true,
	"warn":    true,
	"blocked": true,
}

type cardEnv struct {
	show      func(id string) (Bead, error)
	note      func(id, text string) error
	setStatus func(id, status string) error
	now       func() time.Time
}

func liveCardEnv() cardEnv {
	bd := Beads{}
	return cardEnv{
		show:      bd.Show,
		note:      bd.Note,
		setStatus: bd.SetStatus,
		now:       time.Now,
	}
}

func (e cardEnv) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

func cardsDir() string { return filepath.Join(stateDir(), "cards") }

func beadCardDir(bead string) (string, error) {
	if !beadName.MatchString(bead) {
		return "", fmt.Errorf("invalid bead id %q", bead)
	}
	return filepath.Join(cardsDir(), bead), nil
}

func questionCardPath(bead string) (string, error) {
	dir, err := beadCardDir(bead)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "question.json"), nil
}

func milestonesPath(bead string) (string, error) {
	dir, err := beadCardDir(bead)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "milestones.jsonl"), nil
}

func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

func writeAtomicPrivate(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := ensurePrivateDir(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := ensurePrivateDir(dir); err != nil {
		return err
	}
	if err := writeAtomicMode(path, data, 0o600); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func appendPrivateJSONL(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := ensurePrivateDir(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := ensurePrivateDir(dir); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// readQuestionCard loads a bead's question.json, returning fs.ErrNotExist if absent.
func readQuestionCard(bead string) (QuestionCard, error) {
	p, err := questionCardPath(bead)
	if err != nil {
		return QuestionCard{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return QuestionCard{}, err
	}
	var c QuestionCard
	if err := json.Unmarshal(b, &c); err != nil {
		return QuestionCard{}, fmt.Errorf("question card for %s: %w", bead, err)
	}
	if c.Version != cardSchemaVersion {
		return QuestionCard{}, fmt.Errorf("question card for %s: unsupported version %d", bead, c.Version)
	}
	return c, nil
}

// clearQuestionCard removes a bead's active question.json.
func clearQuestionCard(bead string) error {
	p, err := questionCardPath(bead)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// questionCardLive reports whether card still matches bead b's live needs_me
// question note. When a bead leaves needs_me or its note hash no longer matches,
// callers treat the card as stale/superseded.
func questionCardLive(card QuestionCard, b Bead) (bool, string) {
	if card.Bead != "" && b.ID != "" && card.Bead != b.ID {
		return false, fmt.Sprintf("card bead %s does not match %s", card.Bead, b.ID)
	}
	if b.Status != StatusNeedsMe {
		return false, fmt.Sprintf("bead is %s, not %s", b.Status, StatusNeedsMe)
	}
	if pendingCommand(b.Notes) != "" {
		return false, "bead is waiting on a RUN command"
	}
	q, ok := parseNeedsMeQuestion(b.Notes)
	if !ok {
		return false, "bead notes no longer end in an OPTIONS question"
	}
	if card.Hash == "" || q.Hash != card.Hash {
		return false, fmt.Sprintf("note question hash [%s] does not match card hash [%s]", q.Hash, card.Hash)
	}
	return true, ""
}

// readMilestones reads all valid MilestoneRecord lines for bead.
func readMilestones(bead string) ([]MilestoneRecord, error) {
	p, err := milestonesPath(bead)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []MilestoneRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m MilestoneRecord
		if json.Unmarshal(line, &m) == nil && m.Version == cardSchemaVersion && m.Phase != "" {
			out = append(out, m)
		}
	}
	return out, sc.Err()
}

// cardIACFromPreview classifies a pulumi/tofu preview file with gate.go and
// returns the structured IaC evidence, the preview's SHA-256 digest, and the
// minimum impact class (never lower than gate plan's classification; problems
// force class 9).
func cardIACFromPreview(a AutonomyConfig, previewPath, target, expect string) (*CardIACEvidence, string, int, PlanCheck, error) {
	data, err := os.ReadFile(previewPath)
	if err != nil {
		return nil, "", 9, PlanCheck{}, fmt.Errorf("iac preview: %w", err)
	}
	pc, err := checkPlan(data)
	if err != nil {
		return nil, "", 9, PlanCheck{}, fmt.Errorf("iac preview: %w", err)
	}
	if expect != "" {
		want, err := parseExpect(expect)
		if err != nil {
			return nil, "", 9, PlanCheck{}, err
		}
		pc.expect(want)
	}
	pc.classify(a, target)
	class := pc.Class
	if len(pc.Problems) > 0 {
		class = 9
	}
	excerpt := strings.TrimSpace(renderPlanCheck(pc))
	if len(excerpt) > 2000 {
		excerpt = excerpt[:2000] + "…"
	}
	iac := &CardIACEvidence{
		Tool:    pc.Format,
		Target:  target,
		Create:  pc.Counts["create"],
		Update:  pc.Counts["update"],
		Replace: pc.Counts["replace"],
		Delete:  pc.Counts["delete"],
		Summary: iacSummaryCounts(pc),
		Excerpt: excerpt,
	}
	return iac, sha256Hex(data), class, pc, nil
}

func iacSummaryCounts(pc PlanCheck) string {
	return fmt.Sprintf("%d create, %d update, %d replace, %d destroy",
		pc.Counts["create"], pc.Counts["update"], pc.Counts["replace"], pc.Counts["delete"])
}

// iacEvidenceMinClass computes a conservative floor from attached CardIACEvidence
// when no preview file was passed, so raw JSON evidence cannot understate class.
func iacEvidenceMinClass(a AutonomyConfig, iac *CardIACEvidence) int {
	if iac == nil {
		return 0
	}
	class := a.targetClass(iac.Target)
	prod := class == 7
	deletes := iac.Delete + iac.Replace
	if deletes > 0 {
		if prod {
			class = max(class, 8)
		} else {
			class = max(class, 7)
		}
		if a.Widespread > 0 && deletes > a.Widespread {
			class = 9
		}
	}
	return class
}

// formatQuestionCardNote formats card into the exact QUESTION:/OPTIONS:/CLASS:
// (and optional CRITICAL:) bead note parsed by tapquestion.go.
func formatQuestionCardNote(card QuestionCard) string {
	var b strings.Builder
	if card.Critical {
		crit := oneLine(card.ClassReason)
		if crit == "" {
			crit = oneLine(card.Question)
		}
		fmt.Fprintf(&b, "CRITICAL: %s\n", crit)
	}
	qLine := oneLine(card.Question)
	if card.Recommendation != nil && card.Recommendation.Option > 0 {
		qLine += fmt.Sprintf(" Recommendation: %d", card.Recommendation.Option)
		if r := oneLine(card.Recommendation.Rationale); r != "" {
			qLine += " — " + r
		}
	}
	fmt.Fprintf(&b, "QUESTION: %s\n", qLine)
	if d := oneLine(card.Details); d != "" {
		fmt.Fprintf(&b, "Details: %s\n", d)
	}
	if ev := formatCardEvidenceLine(card.Evidence); ev != "" {
		fmt.Fprintf(&b, "Evidence: %s\n", ev)
	}
	b.WriteString("OPTIONS:\n")
	for _, o := range card.Options {
		fmt.Fprintf(&b, "%d. %s\n", o.Number, oneLine(o.Label))
	}
	if r := oneLine(card.ClassReason); r != "" {
		fmt.Fprintf(&b, "CLASS: %d — %s", card.Class, r)
	} else {
		fmt.Fprintf(&b, "CLASS: %d", card.Class)
	}
	return b.String()
}

func formatCardEvidenceLine(ev *CardEvidence) string {
	if ev == nil {
		return ""
	}
	var parts []string
	if ev.IAC != nil {
		s := ev.IAC.Summary
		if s == "" {
			s = fmt.Sprintf("%d create, %d update, %d replace, %d destroy", ev.IAC.Create, ev.IAC.Update, ev.IAC.Replace, ev.IAC.Delete)
		}
		label := "IaC"
		if ev.IAC.Tool != "" {
			label = ev.IAC.Tool
		}
		if ev.IAC.Target != "" {
			parts = append(parts, fmt.Sprintf("%s %s (%s)", label, oneLine(ev.IAC.Target), oneLine(s)))
		} else {
			parts = append(parts, fmt.Sprintf("%s (%s)", label, oneLine(s)))
		}
	}
	if ev.PlanDigest != "" {
		parts = append(parts, "plan "+short(ev.PlanDigest))
	}
	if ev.PRURL != "" {
		parts = append(parts, "PR "+oneLine(ev.PRURL))
	}
	if ev.CostEstimate != "" {
		parts = append(parts, "cost "+oneLine(ev.CostEstimate))
	}
	for _, u := range ev.RunURLs {
		if u = oneLine(u); u != "" {
			parts = append(parts, "run "+u)
		}
	}
	return strings.Join(parts, " · ")
}

type questionCardInput struct {
	Version        int                 `json:"version,omitempty"`
	Bead           string              `json:"bead,omitempty"`
	Question       string              `json:"question"`
	Details        string              `json:"details,omitempty"`
	Critical       bool                `json:"critical,omitempty"`
	Class          *int                `json:"class,omitempty"`
	ClassReason    string              `json:"class_reason,omitempty"`
	Recommendation *CardRecommendation `json:"recommendation,omitempty"`
	Options        []CardOption        `json:"options"`
	Evidence       *CardEvidence       `json:"evidence,omitempty"`
}

func parseQuestionCardInput(data []byte) (questionCardInput, error) {
	var in questionCardInput
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid question card JSON: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return in, errors.New("invalid question card JSON: unexpected trailing data")
	}
	if in.Version != 0 && in.Version != cardSchemaVersion {
		return in, fmt.Errorf("unsupported card version %d (want %d)", in.Version, cardSchemaVersion)
	}
	return in, nil
}

// buildQuestionCard validates input, applies IaC preview/evidence class floors
// (never lowering class), formats the mirrored bead note, and computes its exact
// tapquestion.go hash.
func buildQuestionCard(cfg Config, bead string, in questionCardInput, iacPreviewPath, iacTarget, iacExpect string, now time.Time) (QuestionCard, string, error) {
	if !beadName.MatchString(bead) {
		return QuestionCard{}, "", fmt.Errorf("invalid bead id %q", bead)
	}
	if in.Bead != "" && in.Bead != bead {
		return QuestionCard{}, "", fmt.Errorf("card bead %q does not match %q", in.Bead, bead)
	}
	qText := strings.TrimSpace(in.Question)
	if qText == "" {
		return QuestionCard{}, "", errors.New("question is required")
	}
	if len(in.Options) < 2 || len(in.Options) > maxQuestionOptions {
		return QuestionCard{}, "", fmt.Errorf("options must contain 2-%d items, got %d", maxQuestionOptions, len(in.Options))
	}
	opts := make([]CardOption, len(in.Options))
	allUnnumbered := true
	for _, o := range in.Options {
		if o.Number != 0 {
			allUnnumbered = false
			break
		}
	}
	maxOptClass := -1
	for i, o := range in.Options {
		num := o.Number
		if allUnnumbered {
			num = i + 1
		}
		if num != i+1 {
			return QuestionCard{}, "", fmt.Errorf("option %d must have number %d, got %d", i+1, i+1, num)
		}
		label := oneLine(o.Label)
		if label == "" {
			return QuestionCard{}, "", fmt.Errorf("option %d has an empty label", num)
		}
		if o.Class != nil {
			if *o.Class < 0 || *o.Class > 9 {
				return QuestionCard{}, "", fmt.Errorf("option %d class %d must be 0-9", num, *o.Class)
			}
			maxOptClass = max(maxOptClass, *o.Class)
		}
		opts[i] = CardOption{
			Number:      num,
			Label:       label,
			Description: strings.TrimSpace(o.Description),
			Class:       o.Class,
		}
	}
	var rec *CardRecommendation
	if in.Recommendation != nil {
		if in.Recommendation.Option < 1 || in.Recommendation.Option > len(opts) {
			return QuestionCard{}, "", fmt.Errorf("recommendation option %d out of range 1-%d", in.Recommendation.Option, len(opts))
		}
		rec = &CardRecommendation{
			Option:    in.Recommendation.Option,
			Rationale: strings.TrimSpace(in.Recommendation.Rationale),
		}
	}
	var ev *CardEvidence
	if in.Evidence != nil {
		cp := *in.Evidence
		if cp.IAC != nil {
			iacCopy := *cp.IAC
			if iacCopy.Create < 0 || iacCopy.Update < 0 || iacCopy.Replace < 0 || iacCopy.Delete < 0 {
				return QuestionCard{}, "", errors.New("evidence.iac counts must not be negative")
			}
			cp.IAC = &iacCopy
		}
		for _, l := range cp.Links {
			if strings.TrimSpace(l.Label) == "" || strings.TrimSpace(l.URL) == "" {
				return QuestionCard{}, "", errors.New("evidence.links entries require label and url")
			}
		}
		ev = &cp
	}

	previewClass := -1
	var previewWhy string
	if iacPreviewPath != "" {
		iac, digest, pClass, pc, err := cardIACFromPreview(cfg.Autonomy, iacPreviewPath, iacTarget, iacExpect)
		if err != nil {
			return QuestionCard{}, "", err
		}
		previewClass = pClass
		previewWhy = fmt.Sprintf("%s (%s)", targetWord(pc), counts(pc))
		if ev == nil {
			ev = &CardEvidence{}
		}
		ev.IAC = iac
		if ev.PlanDigest == "" {
			ev.PlanDigest = digest
		}
	} else if ev != nil && ev.IAC != nil {
		previewClass = iacEvidenceMinClass(cfg.Autonomy, ev.IAC)
	}

	if in.Class == nil && previewClass < 0 {
		return QuestionCard{}, "", errors.New("class (0-9) is required")
	}
	class := 0
	if in.Class != nil {
		if *in.Class < 0 || *in.Class > 9 {
			return QuestionCard{}, "", fmt.Errorf("class %d must be 0-9", *in.Class)
		}
		class = *in.Class
	}
	if maxOptClass > class {
		class = maxOptClass
	}
	classReason := strings.TrimSpace(in.ClassReason)
	if previewClass > class {
		class = previewClass
		if classReason == "" && previewWhy != "" {
			classReason = previewWhy
		}
	}

	card := QuestionCard{
		Version:        cardSchemaVersion,
		Bead:           bead,
		CreatedAt:      now.UTC(),
		Question:       qText,
		Details:        strings.TrimSpace(in.Details),
		Critical:       in.Critical || class >= 8,
		Class:          class,
		ClassReason:    classReason,
		Recommendation: rec,
		Options:        opts,
		Evidence:       ev,
	}
	note := formatQuestionCardNote(card)
	parsed, ok := parseNeedsMeQuestion(note)
	if !ok {
		return QuestionCard{}, "", errors.New("formatted question note failed parseNeedsMeQuestion round-trip")
	}
	if parsed.ClassN != card.Class || len(parsed.Options) != len(card.Options) {
		return QuestionCard{}, "", fmt.Errorf("formatted question note mismatch: class %d vs %d, options %d vs %d", parsed.ClassN, card.Class, len(parsed.Options), len(card.Options))
	}
	card.Hash = parsed.Hash
	return card, note, nil
}

// writeQuestionCard validates, writes question.json atomically, mirrors the
// hash-compatible QUESTION:/OPTIONS:/CLASS: bead note, sets needs_me, and journals it.
func writeQuestionCard(cfg Config, env cardEnv, bead string, in questionCardInput, iacPreviewPath, iacTarget, iacExpect string) (QuestionCard, error) {
	if env.show != nil {
		b, err := env.show(bead)
		if err != nil {
			return QuestionCard{}, err
		}
		if b.Status == StatusClosed {
			return QuestionCard{}, fmt.Errorf("%s is closed", bead)
		}
	}
	now := env.clock()
	card, note, err := buildQuestionCard(cfg, bead, in, iacPreviewPath, iacTarget, iacExpect, now)
	if err != nil {
		return QuestionCard{}, err
	}
	p, err := questionCardPath(bead)
	if err != nil {
		return QuestionCard{}, err
	}
	data, err := json.MarshalIndent(card, "", "  ")
	if err != nil {
		return QuestionCard{}, err
	}
	if err := writeAtomicPrivate(p, append(data, '\n')); err != nil {
		return QuestionCard{}, err
	}
	if env.note != nil {
		if err := env.note(bead, note); err != nil {
			return card, fmt.Errorf("question card written, but noting %s failed: %w", bead, err)
		}
	}
	if env.setStatus != nil {
		if err := env.setStatus(bead, StatusNeedsMe); err != nil {
			return card, fmt.Errorf("question card written, but setting %s to needs_me failed: %w", bead, err)
		}
	}
	jText := fmt.Sprintf("ask [%s] C%d: %s", card.Hash, card.Class, shortTitle(card.Question, 120))
	if err := appendJournal(journalRecord{At: now.UTC(), Kind: journalCard, Bead: bead, Class: card.Class, Action: "ask", Pass: true, Text: jText}); err != nil {
		return card, fmt.Errorf("question card written, but journaling failed: %w", err)
	}
	return card, nil
}

type milestoneInput struct {
	Version  int                `json:"version,omitempty"`
	Bead     string             `json:"bead,omitempty"`
	Phase    string             `json:"phase"`
	Status   string             `json:"status,omitempty"`
	Summary  string             `json:"summary"`
	Evidence *MilestoneEvidence `json:"evidence,omitempty"`
}

func parseMilestoneInput(data []byte) (milestoneInput, error) {
	var in milestoneInput
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid milestone JSON: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return in, errors.New("invalid milestone JSON: unexpected trailing data")
	}
	if in.Version != 0 && in.Version != cardSchemaVersion {
		return in, fmt.Errorf("unsupported milestone version %d (want %d)", in.Version, cardSchemaVersion)
	}
	return in, nil
}

// appendMilestone persists a MilestoneRecord to milestones.jsonl, appends a
// concise MILESTONE [<phase>]: <summary> bead note, and records it in journal.jsonl.
func appendMilestone(env cardEnv, bead string, in milestoneInput) (MilestoneRecord, error) {
	if !beadName.MatchString(bead) {
		return MilestoneRecord{}, fmt.Errorf("invalid bead id %q", bead)
	}
	if in.Bead != "" && in.Bead != bead {
		return MilestoneRecord{}, fmt.Errorf("milestone bead %q does not match %q", in.Bead, bead)
	}
	if env.show != nil {
		if _, err := env.show(bead); err != nil {
			return MilestoneRecord{}, err
		}
	}
	phase := oneLine(in.Phase)
	if phase == "" {
		return MilestoneRecord{}, errors.New("milestone phase is required")
	}
	summary := oneLine(in.Summary)
	if summary == "" {
		return MilestoneRecord{}, errors.New("milestone summary is required")
	}
	status := strings.ToLower(strings.TrimSpace(in.Status))
	if status == "" {
		status = "ok"
	}
	if !validMilestoneStatuses[status] {
		return MilestoneRecord{}, fmt.Errorf("invalid milestone status %q (want info, ok, warn or blocked)", status)
	}
	if in.Evidence != nil {
		for _, l := range in.Evidence.Links {
			if strings.TrimSpace(l.Label) == "" || strings.TrimSpace(l.URL) == "" {
				return MilestoneRecord{}, errors.New("milestone evidence.links entries require label and url")
			}
		}
	}
	now := env.clock().UTC()
	rec := MilestoneRecord{
		Version:  cardSchemaVersion,
		At:       now,
		Bead:     bead,
		Phase:    phase,
		Status:   status,
		Summary:  summary,
		Evidence: in.Evidence,
	}
	p, err := milestonesPath(bead)
	if err != nil {
		return MilestoneRecord{}, err
	}
	if err := appendPrivateJSONL(p, rec); err != nil {
		return MilestoneRecord{}, err
	}
	note := fmt.Sprintf("MILESTONE [%s]: %s", phase, summary)
	if env.note != nil {
		if err := env.note(bead, note); err != nil {
			return rec, fmt.Errorf("milestone written, but noting %s failed: %w", bead, err)
		}
	}
	if err := appendJournal(journalRecord{At: now, Kind: journalCard, Bead: bead, Class: -1, Action: "milestone", Pass: true, Text: fmt.Sprintf("milestone [%s] (%s): %s", phase, status, shortTitle(summary, 120))}); err != nil {
		return rec, fmt.Errorf("milestone written, but journaling failed: %w", err)
	}
	return rec, nil
}

type cardView struct {
	Bead        string            `json:"bead"`
	Live        bool              `json:"live"`
	StaleReason string            `json:"stale_reason,omitempty"`
	Question    *QuestionCard     `json:"question,omitempty"`
	Milestones  []MilestoneRecord `json:"milestones,omitempty"`
}

func loadCardView(env cardEnv, bead string) (cardView, error) {
	if !beadName.MatchString(bead) {
		return cardView{}, fmt.Errorf("invalid bead id %q", bead)
	}
	v := cardView{Bead: bead}
	q, qerr := readQuestionCard(bead)
	if qerr != nil && !errors.Is(qerr, fs.ErrNotExist) {
		return v, qerr
	}
	ms, merr := readMilestones(bead)
	if merr != nil {
		return v, merr
	}
	v.Milestones = ms
	if qerr == nil {
		v.Question = &q
		if env.show != nil {
			b, err := env.show(bead)
			if err != nil {
				v.Live = false
				v.StaleReason = "bead unavailable: " + err.Error()
			} else {
				v.Live, v.StaleReason = questionCardLive(q, b)
			}
		}
	}
	return v, nil
}

func listCardBeads() ([]string, error) {
	entries, err := os.ReadDir(cardsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && beadName.MatchString(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

const cardUsage = "usage: quartermaster card ask <bead> (--file <json|-> | --question Q --option A --option B --class N [...]) | milestone <bead> --phase P --summary S [--status info|ok|warn|blocked] [...] | show <bead> [--json] | list [--json]"

func cardCmd(cfg Config, env cardEnv, args []string, stdin io.Reader, w io.Writer) error {
	if len(args) == 0 {
		return errors.New(cardUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ask":
		return cardAskCmd(cfg, env, rest, stdin, w)
	case "milestone":
		return cardMilestoneCmd(env, rest, stdin, w)
	case "show":
		return cardShowCmd(env, rest, w)
	case "list":
		return cardListCmd(env, rest, w)
	default:
		return errors.New(cardUsage)
	}
}

func cardAskCmd(cfg Config, env cardEnv, args []string, stdin io.Reader, w io.Writer) error {
	fs := flag.NewFlagSet("card ask", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fileFlag := fs.String("file", "", "JSON question card file (or - for stdin)")
	questionFlag := fs.String("question", "", "short question line")
	detailsFlag := fs.String("details", "", "optional extra context")
	var optionFlags stringSliceFlag
	fs.Var(&optionFlags, "option", "repeatable option label (2-9)")
	classFlag := fs.Int("class", -1, "impact class 0-9")
	classReasonFlag := fs.String("class-reason", "", "reason for class")
	recommendFlag := fs.Int("recommend", 0, "recommended option number (1-based)")
	rationaleFlag := fs.String("rationale", "", "rationale for recommendation")
	criticalFlag := fs.Bool("critical", false, "mark question critical")
	prURLFlag := fs.String("pr-url", "", "PR URL evidence")
	planDigestFlag := fs.String("plan-digest", "", "execution plan digest evidence")
	var runURLFlags stringSliceFlag
	fs.Var(&runURLFlags, "run-url", "repeatable GitHub Actions run URL evidence")
	iacPreviewFlag := fs.String("iac-preview", "", "path to pulumi preview --json or tofu show -json file")
	iacTargetFlag := fs.String("iac-target", "", "IaC target stack or workspace")
	iacExpectFlag := fs.String("iac-expect", "", "expected IaC counts (create=1,update=1)")
	costFlag := fs.String("cost", "", "estimated cost evidence")
	jsonOut := fs.Bool("json", false, "output recorded card as JSON")

	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: quartermaster card ask <bead> (--file <json|-> | --question Q --option A --option B --class N [...])")
	}
	bead := pos[0]
	var in questionCardInput
	if *fileFlag != "" {
		data, err := readPlanArg(*fileFlag, stdin)
		if err != nil {
			return err
		}
		in, err = parseQuestionCardInput(data)
		if err != nil {
			return err
		}
	}
	if *questionFlag != "" {
		in.Question = *questionFlag
	}
	if *detailsFlag != "" {
		in.Details = *detailsFlag
	}
	if len(optionFlags) > 0 {
		in.Options = make([]CardOption, len(optionFlags))
		for i, lbl := range optionFlags {
			in.Options[i] = CardOption{Number: i + 1, Label: lbl}
		}
	}
	if *classFlag < -1 || *classFlag > 9 {
		return fmt.Errorf("class %d must be 0-9", *classFlag)
	}
	if *classFlag >= 0 {
		c := *classFlag
		in.Class = &c
	}
	if *classReasonFlag != "" {
		in.ClassReason = *classReasonFlag
	}
	if *recommendFlag > 0 {
		in.Recommendation = &CardRecommendation{Option: *recommendFlag, Rationale: *rationaleFlag}
	} else if *rationaleFlag != "" && in.Recommendation != nil {
		in.Recommendation.Rationale = *rationaleFlag
	}
	if *criticalFlag {
		in.Critical = true
	}
	if *prURLFlag != "" || *planDigestFlag != "" || len(runURLFlags) > 0 || *costFlag != "" {
		if in.Evidence == nil {
			in.Evidence = &CardEvidence{}
		}
		if *prURLFlag != "" {
			in.Evidence.PRURL = *prURLFlag
		}
		if *planDigestFlag != "" {
			in.Evidence.PlanDigest = *planDigestFlag
		}
		if len(runURLFlags) > 0 {
			in.Evidence.RunURLs = append(in.Evidence.RunURLs, runURLFlags...)
		}
		if *costFlag != "" {
			in.Evidence.CostEstimate = *costFlag
		}
	}

	card, err := writeQuestionCard(cfg, env, bead, in, *iacPreviewFlag, *iacTargetFlag, *iacExpectFlag)
	if err != nil {
		return err
	}
	if *jsonOut {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		return enc.Encode(card)
	}
	fmt.Fprintf(w, "card %s [%s] C%d (%d options): %s\n", card.Bead, card.Hash, card.Class, len(card.Options), shortTitle(card.Question, 100))
	return nil
}

func cardMilestoneCmd(env cardEnv, args []string, stdin io.Reader, w io.Writer) error {
	fs := flag.NewFlagSet("card milestone", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fileFlag := fs.String("file", "", "JSON milestone file (or - for stdin)")
	phaseFlag := fs.String("phase", "", "milestone phase (e.g. pr_opened, merged, verified)")
	summaryFlag := fs.String("summary", "", "one-line milestone summary")
	statusFlag := fs.String("status", "", "milestone status: info, ok, warn, blocked (default ok)")
	prURLFlag := fs.String("pr-url", "", "PR URL evidence")
	runURLFlag := fs.String("run-url", "", "GitHub Actions run URL evidence")
	shaFlag := fs.String("sha", "", "commit SHA evidence")
	planDigestFlag := fs.String("plan-digest", "", "plan digest evidence")
	var linkFlags stringSliceFlag
	fs.Var(&linkFlags, "link", "repeatable label=url evidence link")
	jsonOut := fs.Bool("json", false, "output recorded milestone as JSON")

	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: quartermaster card milestone <bead> --phase P --summary S [--status info|ok|warn|blocked] [--pr-url ...] [--run-url ...] [--sha ...] [--plan-digest ...]")
	}
	bead := pos[0]
	var in milestoneInput
	if *fileFlag != "" {
		data, err := readPlanArg(*fileFlag, stdin)
		if err != nil {
			return err
		}
		in, err = parseMilestoneInput(data)
		if err != nil {
			return err
		}
	}
	if *phaseFlag != "" {
		in.Phase = *phaseFlag
	}
	if *summaryFlag != "" {
		in.Summary = *summaryFlag
	}
	if *statusFlag != "" {
		in.Status = *statusFlag
	}
	if *prURLFlag != "" || *runURLFlag != "" || *shaFlag != "" || *planDigestFlag != "" || len(linkFlags) > 0 {
		if in.Evidence == nil {
			in.Evidence = &MilestoneEvidence{}
		}
		if *prURLFlag != "" {
			in.Evidence.PRURL = *prURLFlag
		}
		if *runURLFlag != "" {
			in.Evidence.RunURL = *runURLFlag
		}
		if *shaFlag != "" {
			in.Evidence.SHA = *shaFlag
		}
		if *planDigestFlag != "" {
			in.Evidence.PlanDigest = *planDigestFlag
		}
		for _, raw := range linkFlags {
			lbl, u, ok := strings.Cut(raw, "=")
			if !ok || strings.TrimSpace(lbl) == "" || strings.TrimSpace(u) == "" {
				return fmt.Errorf("--link %q: want label=url", raw)
			}
			in.Evidence.Links = append(in.Evidence.Links, CardLink{Label: strings.TrimSpace(lbl), URL: strings.TrimSpace(u)})
		}
	}

	rec, err := appendMilestone(env, bead, in)
	if err != nil {
		return err
	}
	if *jsonOut {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		return enc.Encode(rec)
	}
	fmt.Fprintf(w, "milestone %s [%s] (%s): %s\n", rec.Bead, rec.Phase, rec.Status, rec.Summary)
	return nil
}

func cardShowCmd(env cardEnv, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("card show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: quartermaster card show <bead> [--json]")
	}
	v, err := loadCardView(env, pos[0])
	if err != nil {
		return err
	}
	if *jsonOut {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	if v.Question == nil && len(v.Milestones) == 0 {
		fmt.Fprintf(w, "no cards for %s\n", v.Bead)
		return nil
	}
	if q := v.Question; q != nil {
		state := "live"
		if !v.Live {
			state = "stale"
			if v.StaleReason != "" {
				state += ": " + v.StaleReason
			}
		}
		crit := ""
		if q.Critical {
			crit = " CRITICAL"
		}
		fmt.Fprintf(w, "question %s [%s] (%s) C%d%s: %s\n", q.Bead, q.Hash, state, q.Class, crit, q.Question)
		if q.Details != "" {
			fmt.Fprintf(w, "  details: %s\n", q.Details)
		}
		if q.Recommendation != nil {
			fmt.Fprintf(w, "  recommendation: %d", q.Recommendation.Option)
			if q.Recommendation.Rationale != "" {
				fmt.Fprintf(w, " — %s", q.Recommendation.Rationale)
			}
			fmt.Fprintln(w)
		}
		for _, o := range q.Options {
			line := fmt.Sprintf("  %d. %s", o.Number, o.Label)
			if o.Description != "" {
				line += " — " + o.Description
			}
			if o.Class != nil {
				line += fmt.Sprintf(" [C%d]", *o.Class)
			}
			fmt.Fprintln(w, line)
		}
		if ev := formatCardEvidenceLine(q.Evidence); ev != "" {
			fmt.Fprintf(w, "  evidence: %s\n", ev)
		}
	}
	if len(v.Milestones) > 0 {
		fmt.Fprintf(w, "milestones (%d):\n", len(v.Milestones))
		for _, m := range v.Milestones {
			fmt.Fprintf(w, "  - %s [%s] (%s) %s\n", m.At.UTC().Format(time.RFC3339), m.Phase, m.Status, m.Summary)
		}
	}
	return nil
}

func cardListCmd(env cardEnv, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("card list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New("usage: quartermaster card list [--json]")
	}
	ids, err := listCardBeads()
	if err != nil {
		return err
	}
	views := make([]cardView, 0, len(ids))
	for _, id := range ids {
		v, err := loadCardView(env, id)
		if err != nil {
			continue
		}
		if v.Question == nil && len(v.Milestones) == 0 {
			continue
		}
		views = append(views, v)
	}
	if *jsonOut {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(views)
	}
	if len(views) == 0 {
		fmt.Fprintln(w, "(no cards)")
		return nil
	}
	for _, v := range views {
		var parts []string
		if v.Question != nil {
			st := "stale"
			if v.Live {
				st = "live"
			}
			parts = append(parts, fmt.Sprintf("question [%s] C%d %s: %s", v.Question.Hash, v.Question.Class, st, shortTitle(v.Question.Question, 60)))
		}
		if len(v.Milestones) > 0 {
			last := v.Milestones[len(v.Milestones)-1]
			parts = append(parts, fmt.Sprintf("%d milestone(s), latest [%s]: %s", len(v.Milestones), last.Phase, shortTitle(last.Summary, 50)))
		}
		fmt.Fprintf(w, "- %s · %s\n", v.Bead, strings.Join(parts, " · "))
	}
	return nil
}

// cardContextLines returns concise card summary lines for a needs_me bead in
// quartermaster context when a live QuestionCard exists.
func cardContextLines(b Bead) string {
	card, err := readQuestionCard(b.ID)
	if err != nil {
		return ""
	}
	if live, _ := questionCardLive(card, b); !live {
		return ""
	}
	var parts []string
	crit := ""
	if card.Critical {
		crit = " CRITICAL"
	}
	parts = append(parts, fmt.Sprintf("C%d%s", card.Class, crit))
	if card.Recommendation != nil {
		parts = append(parts, fmt.Sprintf("rec %d", card.Recommendation.Option))
	}
	if ev := formatCardEvidenceLine(card.Evidence); ev != "" {
		parts = append(parts, ev)
	}
	return fmt.Sprintf("  card: %s\n", strings.Join(parts, " · "))
}
