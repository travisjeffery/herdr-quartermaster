package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const pr = "https://github.com/acme/app/pull/"

func fixtureBeads(t *testing.T) []Bead {
	t.Helper()
	b, err := os.ReadFile("testdata/digest-beads.json")
	if err != nil {
		t.Fatal(err)
	}
	var beads []Bead
	if err := json.Unmarshal(b, &beads); err != nil {
		t.Fatal(err)
	}
	return beads
}

func fixturePRs() map[string]DigestPR {
	return map[string]DigestPR{
		pr + "1":  {Number: 1, URL: pr + "1", State: "MERGED", MergedAt: t0.Add(-48 * time.Hour)},
		pr + "2":  {Number: 2, URL: pr + "2", State: "OPEN", Checks: "SUCCESS", ReviewDecision: "REVIEW_REQUIRED"},
		pr + "3":  {Number: 3, URL: pr + "3", State: "OPEN", Checks: "PENDING", ReviewDecision: "APPROVED"},
		pr + "4":  {Number: 4, URL: pr + "4", State: "MERGED", MergedAt: t0.Add(-time.Hour)},
		pr + "6":  {Number: 6, URL: pr + "6", State: "OPEN", Mergeable: "CONFLICTING"},
		pr + "10": {Number: 10, URL: pr + "10", State: "MERGED", MergedAt: t0.Add(-24 * time.Hour)},
	}
}

func fixtureInputs(t *testing.T) digestInputs {
	return digestInputs{
		open: fixtureBeads(t),
		agents: map[string]Agent{
			"app-answered": {Name: "app-answered", Status: "working"},
			"app-prompt":   {Name: "app-prompt", Status: "blocked"},
			"app-wip":      {Name: "app-wip", Status: "idle"},
		},
		prs:    fixturePRs(),
		branch: map[string]string{"app-run": pr + "6"},
	}
}

func sectionOf(d Digest) map[string]Section {
	out := map[string]Section{}
	for s, items := range d.Sections {
		for _, it := range items {
			out[it.Bead.ID] = s
		}
	}
	return out
}

func item(t *testing.T, d Digest, id string) DigestItem {
	t.Helper()
	for _, items := range d.Sections {
		for _, it := range items {
			if it.Bead.ID == id {
				return it
			}
		}
	}
	t.Fatalf("%s is in no section", id)
	return DigestItem{}
}

func TestDigestSections(t *testing.T) {
	d, next := buildDigest(fixtureInputs(t), digestState{}, nil, t0)
	want := map[string]Section{
		"app-ask":      SecNeedsYou,
		"app-run":      SecNeedsYou,
		"app-prompt":   SecNeedsYou,   // in progress, but its agent is waiting at a prompt
		"app-answered": SecInProgress, // needs_me, but answered: its agent is working again
		"app-review":   SecInProgress, // the PR is open, but the agent is missing
		"app-held":     SecBlocked,    // blocked status outranks its open PR
		"app-dep":      SecBlocked,    // marked next, but an open bead blocks it
		"app-wip":      SecRollingOut, // merged, rolling out; its blocker is closed
		"app-next":     SecReady,
		"app-epic":     SecBacklog,
		"app-later":    SecBacklog,
	}
	got := sectionOf(d)
	for id, s := range want {
		if got[id] != s {
			t.Errorf("%s in %q, want %q", id, got[id], s)
		}
		if next.Beads[id] != s {
			t.Errorf("state remembers %s in %q, want %q", id, next.Beads[id], s)
		}
	}
	if len(got) != len(want) || d.Total != len(want) {
		t.Errorf("sections hold %d beads (total %d), want %d", len(got), d.Total, len(want))
	}

	needs := d.Sections[SecNeedsYou]
	if ids := []string{needs[0].Bead.ID, needs[1].Bead.ID, needs[2].Bead.ID}; !slices.Equal(ids, []string{"app-ask", "app-run", "app-prompt"}) {
		t.Errorf("needs you in order %v, want priority first, then the longest waiting", ids)
	}
	if it := item(t, d, "app-review"); it.PR == nil || it.PR.Number != 2 || it.OpenPRs != 1 || it.Linear != "ENG-1024" {
		t.Errorf("app-review shows PR %+v (%d open), Linear %q; want the open #2 and ENG-1024", it.PR, it.OpenPRs, it.Linear)
	}
	if it := item(t, d, "app-dep"); !slices.Equal(it.Blockers, []string{"app-review (in_progress)"}) {
		t.Errorf("app-dep blockers = %v", it.Blockers)
	}
	if it := item(t, d, "app-next"); it.PR != nil {
		t.Errorf("an open bead's PR is never looked up, got %+v", it.PR)
	}
}

func TestDigestStaleness(t *testing.T) {
	d, _ := buildDigest(fixtureInputs(t), digestState{}, nil, t0)
	want := map[string]bool{
		"app-ask":    true,  // needs_me for 6h
		"app-run":    false, // needs_me for 1h
		"app-review": true,  // in progress, untouched 2 days
		"app-wip":    false,
		"app-held":   true, // blocked, untouched 14 days
		"app-dep":    false,
		"app-later":  false, // backlog is never stale
	}
	for id, stale := range want {
		if it := item(t, d, id); it.Stale != stale {
			t.Errorf("%s stale = %v, want %v", id, it.Stale, stale)
		}
	}
	if !strings.Contains(d.headline(), "3 stale") {
		t.Errorf("headline %q lacks the stale count", d.headline())
	}
}

func TestDigestDetailLines(t *testing.T) {
	d, _ := buildDigest(fixtureInputs(t), digestState{}, nil, t0)
	cases := map[string]string{
		"app-ask":    "QUESTION: one region first, or all at once? · acme/app#10 merged",
		"app-run":    "RUN: aws sso login --profile admin · acme/app#6 · conflicts · no checks · no review required",
		"app-prompt": "agent stopped at a prompt",
		"app-review": "no agent · acme/app#2 · checks green · review required",
		"app-held":   "BLOCKED until 2026-10-01 (soak). · acme/app#3 · checks running · approved",
		"app-dep":    "blocked by app-review (in_progress)",
		"app-wip":    "agent idle · rolling out · acme/app#4 merged",
		"app-next":   "",
	}
	for id, want := range cases {
		if got := itemDetail(item(t, d, id)); got != want {
			t.Errorf("%s detail\n got %q\nwant %q", id, got, want)
		}
	}
	line := itemLine(item(t, d, "app-review"), t0)
	if line != "**app-review** P1 Raise the client timeout · @claude · STALE since 2d ago · ENG-1024" {
		t.Errorf("line = %q", line)
	}
}

func TestDigestChanges(t *testing.T) {
	in := fixtureInputs(t)
	in.prs[pr+"3"] = DigestPR{Number: 3, URL: pr + "3", State: "OPEN", Checks: "PENDING", ReviewDecision: "APPROVED"}
	in.closed = []Bead{{ID: "app-gone", Title: "Fix the flaky test", Status: StatusClosed, ClosedAt: t0.Add(-30 * time.Minute), CloseReason: "PR #9 merged, CI green"}}
	prev := digestState{
		At: t0.Add(-time.Hour),
		Beads: map[string]Section{
			"app-ask": SecNeedsYou, "app-run": SecInProgress, "app-prompt": SecInProgress,
			"app-answered": SecNeedsYou, "app-review": SecWaiting, "app-held": SecBlocked,
			"app-dep": SecReady, "app-wip": SecWaiting, "app-next": SecReady,
		},
		PRs: map[string]DigestPR{
			pr + "2": {Number: 2, URL: pr + "2", State: "OPEN", Checks: "PENDING", ReviewDecision: "REVIEW_REQUIRED"},
			pr + "3": {Number: 3, URL: pr + "3", State: "OPEN", Checks: "PENDING", ReviewDecision: "REVIEW_REQUIRED"},
			pr + "4": {Number: 4, URL: pr + "4", State: "OPEN", Checks: "SUCCESS"},
		},
	}
	d, _ := buildDigest(in, prev, nil, t0)
	want := []string{
		"blocked: app-dep Postmortem timeline",
		"needs you: app-prompt Agent at a permission prompt",
		"needs you: app-run Rotate the deploy key",
		"checks green: acme/app#2 (app-review)",
		"approved: acme/app#3 (app-held)",
		"merged: acme/app#4 (app-wip)",
		"closed: app-gone Fix the flaky test — PR #9 merged, CI green",
	}
	if !slices.Equal(d.Changes, want) {
		t.Errorf("changes\n got %q\nwant %q", d.Changes, want)
	}

	// A digest identical to the last one has nothing to say.
	_, next := buildDigest(in, prev, nil, t0)
	in.closed = nil
	again, _ := buildDigest(in, next, nil, t0.Add(time.Hour))
	if len(again.Changes) != 0 {
		t.Errorf("an unchanged digest reports %q", again.Changes)
	}

	// The first digest has nothing to compare with.
	first, _ := buildDigest(in, digestState{}, nil, t0)
	if len(first.Changes) != 0 {
		t.Errorf("the first digest reports %q", first.Changes)
	}
}

func TestGatherDigestAsksOnlyForUnsettledPRs(t *testing.T) {
	prev := digestState{At: t0.Add(-time.Hour), PRs: map[string]DigestPR{
		pr + "1": {Number: 1, URL: pr + "1", State: "MERGED"},
		pr + "3": {Number: 3, URL: pr + "3", State: "OPEN"},
		pr + "4": {Number: 4, URL: pr + "4", State: "CLOSED"},
	}}
	var asked []string
	var closedSince time.Time
	env := digestEnv{
		open:   func() ([]Bead, error) { return fixtureBeads(t), nil },
		closed: func(since time.Time) ([]Bead, error) { closedSince = since; return nil, nil },
		agents: func() ([]Agent, error) { return nil, nil },
		ticker: func() TickerState {
			return TickerState{PRs: map[string]PR{"app-run": {URL: pr + "6"}, "app-next": {URL: pr + "7"}}}
		},
		lookup: func(urls []string) (map[string]DigestPR, int, error) {
			asked = slices.Sorted(slices.Values(urls))
			out := map[string]DigestPR{}
			for _, u := range urls {
				out[u] = fixturePRs()[u]
			}
			return out, 1, nil
		},
	}
	in, warnings, err := gatherDigest(env, prev)
	if err != nil || len(warnings) > 0 {
		t.Fatal(err, warnings)
	}
	// #1 merged and #4 closed last time; #7 belongs to an open bead.
	if want := []string{pr + "10", pr + "2", pr + "3", pr + "6"}; !slices.Equal(asked, want) {
		t.Errorf("asked GitHub about %v, want %v", asked, want)
	}
	if in.prs[pr+"1"].State != "MERGED" || in.asked != 4 || in.queries != 1 {
		t.Errorf("settled PR %+v, asked %d in %d queries", in.prs[pr+"1"], in.asked, in.queries)
	}
	if !closedSince.Equal(prev.At) {
		t.Errorf("closed beads since %v, want the last digest %v", closedSince, prev.At)
	}

	env.lookup = func([]string) (map[string]DigestPR, int, error) { return nil, 1, errors.New("rate limited") }
	in, warnings, err = gatherDigest(env, prev)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "rate limited") {
		t.Fatalf("err %v, warnings %q", err, warnings)
	}
	if in.prs[pr+"3"].State != "OPEN" {
		t.Errorf("a failed lookup keeps the last digest's PRs, got %+v", in.prs)
	}

	env.open = func() ([]Bead, error) { return nil, errors.New("bd: no database") }
	if _, _, err := gatherDigest(env, prev); err == nil {
		t.Error("a failed bd list is an error")
	}
}

func TestParseDigestPRs(t *testing.T) {
	q, alias := digestQuery([]string{pr + "2", "https://example.com/nope", pr + "9"})
	if len(alias) != 2 || !strings.Contains(q, `p0: repository(owner: "acme", name: "app") { pullRequest(number: 2)`) {
		t.Fatalf("query %s, alias %v", q, alias)
	}
	out := []byte(`{"data": {
		"p0": {"pullRequest": {"number": 2, "url": "` + pr + `2", "state": "OPEN", "isDraft": true, "reviewDecision": "APPROVED",
			"mergeable": "MERGEABLE", "commits": {"nodes": [{"commit": {"statusCheckRollup": {"state": "FAILURE"}}}]}}},
		"p2": {"pullRequest": {"number": 9, "url": "` + pr + `9", "state": "MERGED", "mergedAt": "2026-09-24T10:00:00Z",
			"commits": {"nodes": [{"commit": {"statusCheckRollup": null}}]}}}}}`)
	prs := map[string]DigestPR{}
	if err := parseDigestPRs(out, alias, prs); err != nil {
		t.Fatal(err)
	}
	if got := prs[pr+"2"]; got.Checks != "FAILURE" || !got.IsDraft || got.ReviewDecision != "APPROVED" {
		t.Errorf("#2 = %+v", got)
	}
	if got := prs[pr+"9"]; got.State != "MERGED" || got.Checks != "" || !got.MergedAt.Equal(t0.Add(-2*time.Hour)) {
		t.Errorf("#9 = %+v", got)
	}
}

func TestRenderDigestAndSummary(t *testing.T) {
	in := fixtureInputs(t)
	for i := range 7 {
		in.open = append(in.open, Bead{ID: "app-b" + string(rune('a'+i)), Title: "Backlog item", Status: StatusOpen, Priority: 4})
	}
	d, _ := buildDigest(in, digestState{}, nil, t0)
	out := renderDigest(d)
	for _, want := range []string{
		"# Beads digest: 2026-09-24 12:00 UTC",
		"3 need you · 0 waiting · 2 blocked · 2 in progress · 0 merged · 1 rolling out · 1 next · 9 backlog · 3 stale (18 open)",
		"## Changed since the last digest (0)\n\n_Nothing._",
		"## Needs you (3)\n\n- **app-ask** P1 Pick a rollout order · @claude · STALE since 6h ago\n  - QUESTION:",
		"## Backlog (9)",
		"- _and 4 more; `bd list --status open` for all of them_",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("digest lacks %q:\n%s", want, out)
		}
	}
	if !d.worthNotifying() {
		t.Error("a digest with beads needing the user should notify")
	}
	title, body := d.summary(2)
	if title != "Beads: "+d.headline() {
		t.Errorf("title %q", title)
	}
	if want := "3 need you; no steps without you\n\nNeeds you:\napp-ask P1 Pick a rollout order (stale 6h ago)\napp-run P2 Rotate the deploy key\n…and 1 more"; body != want {
		t.Errorf("body\n got %q\nwant %q", body, want)
	}

	quiet := Digest{At: t0, Sections: map[Section][]DigestItem{SecBacklog: d.Sections[SecBacklog]}}
	if quiet.worthNotifying() {
		t.Error("nothing changed and nothing needs the user: no notification")
	}
	if _, body := quiet.summary(2); body != "nothing needed you; no steps without you" {
		t.Errorf("quiet body %q", body)
	}
	quiet.Changes = []string{"closed: app-x"}
	if !quiet.worthNotifying() {
		t.Error("a change is worth a notification")
	}
}

func TestParseDeliver(t *testing.T) {
	got, err := parseDeliver("desktop, slack")
	if err != nil || !got["desktop"] || !got["slack"] || len(got) != 2 {
		t.Errorf("parseDeliver = %v, %v", got, err)
	}
	if _, err := parseDeliver("email"); err == nil {
		t.Error("an unknown target is an error")
	}
}

func TestSendSlack(t *testing.T) {
	type call struct {
		url, auth, body string
	}
	var calls []call
	post := func(reply string) func(string, http.Header, []byte) ([]byte, error) {
		return func(url string, h http.Header, body []byte) ([]byte, error) {
			calls = append(calls, call{url, h.Get("Authorization"), string(body)})
			return []byte(reply), nil
		}
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	if err := sendSlack("hi", env(map[string]string{envSlackWebhook: "https://hooks.slack.test/T/B/x"}), post("ok")); err != nil {
		t.Fatal(err)
	}
	if calls[0].url != "https://hooks.slack.test/T/B/x" || calls[0].body != `{"text":"hi"}` || calls[0].auth != "" {
		t.Errorf("webhook call %+v", calls[0])
	}

	bot := env(map[string]string{envSlackToken: "xoxb-1", envSlackChannel: "U123"})
	if err := sendSlack("hi", bot, post(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if c := calls[1]; c.url != "https://slack.com/api/chat.postMessage" || c.auth != "Bearer xoxb-1" || c.body != `{"channel":"U123","text":"hi"}` {
		t.Errorf("bot call %+v", c)
	}
	if err := sendSlack("hi", bot, post(`{"ok":false,"error":"channel_not_found"}`)); err == nil || !strings.Contains(err.Error(), "channel_not_found") {
		t.Errorf("a refused post is an error, got %v", err)
	}
	if err := sendSlack("hi", env(nil), post("")); err == nil {
		t.Error("slack without a webhook or token is an error")
	}
}

func TestDigestInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MISE_DATA_DIR", filepath.Join(home, "no-mise"))
	dir := filepath.Join(home, "units")
	var ran []string
	systemctl := func(args ...string) error { ran = append(ran, strings.Join(args, " ")); return nil }
	lookPath := func(tool string) (string, error) {
		if tool == "bd" {
			return "/opt/mise/bd/bd", nil
		}
		return "", errors.New("not found")
	}
	cfg := Config{BeadsDir: "/home/me/src/app/.beads"}

	if _, err := digestInstall(cfg, dir, lookPath, systemctl); err == nil {
		t.Fatal("install without the ~/.local/bin link should fail")
	}
	bin := filepath.Join(home, ".local", "bin", toolName)
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := digestInstall(cfg, dir, lookPath, systemctl); err != nil {
		t.Fatal(err)
	}
	svc, _ := os.ReadFile(filepath.Join(dir, digestService))
	for _, want := range []string{"Type=oneshot", "KillMode=process", "ExecStart=%h/.local/bin/quartermaster digest"} {
		if !strings.Contains(string(svc), want) {
			t.Errorf("service lacks %q", want)
		}
	}
	timer, _ := os.ReadFile(filepath.Join(dir, digestTimer))
	if !strings.Contains(string(timer), "OnCalendar=hourly") || !strings.Contains(string(timer), "Persistent=true") {
		t.Errorf("timer:\n%s", timer)
	}
	drop, _ := os.ReadFile(filepath.Join(dir, digestService+".d", toolName+".conf"))
	if !strings.Contains(string(drop), "Environment=BEADS_DIR=/home/me/src/app/.beads\n") ||
		!strings.Contains(string(drop), "Environment=PATH=/opt/mise/bd:/usr/local/bin:/usr/bin:/bin\n") {
		t.Errorf("drop-in:\n%s", drop)
	}
	shims := filepath.Join(home, "mise", "shims")
	if err := os.MkdirAll(shims, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := digestSearchPath(lookPath, shims); !slices.Equal(got, []string{"/opt/mise/bd", shims, "/usr/local/bin", "/usr/bin", "/bin"}) {
		t.Errorf("search path with shims = %v", got)
	}
	if want := []string{"daemon-reload", "enable --now " + digestTimer}; !slices.Equal(ran, want) {
		t.Errorf("systemctl %q, want %q", ran, want)
	}

	ran = nil
	if _, err := digestUninstall(dir, systemctl); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("uninstall left %v", left)
	}
	if want := []string{"disable --now " + digestTimer, "daemon-reload"}; !slices.Equal(ran, want) {
		t.Errorf("systemctl %q, want %q", ran, want)
	}
}

func TestSystemdQuote(t *testing.T) {
	if got := systemdQuote("PATH=/a:/b"); got != "PATH=/a:/b" {
		t.Errorf("plain value quoted: %s", got)
	}
	if got := systemdQuote(`BEADS_DIR=/my dir/"x"`); got != `"BEADS_DIR=/my dir/\"x\""` {
		t.Errorf("got %s", got)
	}
}

func TestRenderDigestJSON(t *testing.T) {
	in := fixtureInputs(t)
	for i := range 7 {
		in.open = append(in.open, Bead{ID: "app-b" + string(rune('a'+i)), Title: "Backlog item", Status: StatusOpen, Priority: 4})
	}
	d, _ := buildDigest(in, digestState{}, nil, t0)
	d.Warnings = []string{"GitHub: rate limited"}

	raw, err := renderDigestJSON(d)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		At      time.Time `json:"at"`
		Summary struct {
			Total      int    `json:"total"`
			Stale      int    `json:"stale"`
			NeedsYou   int    `json:"needs_you"`
			Waiting    int    `json:"waiting"`
			Merged     int    `json:"merged"`
			RollingOut int    `json:"rolling_out"`
			Blocked    int    `json:"blocked"`
			InProgress int    `json:"in_progress"`
			Ready      int    `json:"ready"`
			Backlog    int    `json:"backlog"`
			Headline   string `json:"headline"`
			Quiet      string `json:"quiet"`
		} `json:"summary"`
		Warnings   []string         `json:"warnings"`
		Changes    []string         `json:"changes"`
		NeedsYou   []digestItemJSON `json:"needs_you"`
		Waiting    []digestItemJSON `json:"waiting"`
		Merged     []digestItemJSON `json:"merged"`
		RollingOut []digestItemJSON `json:"rolling_out"`
		Blocked    []digestItemJSON `json:"blocked"`
		InProgress []digestItemJSON `json:"in_progress"`
		Ready      []digestItemJSON `json:"ready"`
		Backlog    []digestItemJSON `json:"backlog"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("invalid digest JSON: %v\n%s", err, raw)
	}
	if !got.At.Equal(t0) {
		t.Errorf("at = %v, want %v", got.At, t0)
	}
	if got.Summary.Total != 18 || got.Summary.Stale != 3 || got.Summary.NeedsYou != 3 || got.Summary.Backlog != 9 || got.Summary.RollingOut != 1 {
		t.Errorf("summary = %+v", got.Summary)
	}
	if got.Summary.Headline != d.headline() || got.Summary.Quiet != d.quiet() {
		t.Errorf("headline/quiet mismatch: %+v", got.Summary)
	}
	if !slices.Equal(got.Warnings, []string{"GitHub: rate limited"}) {
		t.Errorf("warnings = %v", got.Warnings)
	}
	if got.Changes == nil || len(got.Changes) != 0 {
		t.Errorf("changes = %#v, want empty non-nil slice", got.Changes)
	}
	if got.Waiting == nil || len(got.Waiting) != 0 || got.Merged == nil || len(got.Merged) != 0 {
		t.Errorf("empty sections should decode as [] not null: waiting=%#v merged=%#v", got.Waiting, got.Merged)
	}
	if len(got.NeedsYou) != 3 || got.NeedsYou[0].Bead != "app-ask" || !got.NeedsYou[0].Stale || got.NeedsYou[0].PR == nil || got.NeedsYou[0].PR.Number != 10 {
		t.Errorf("needs_you = %+v", got.NeedsYou)
	}
	if len(got.Backlog) != 9 {
		t.Errorf("backlog len = %d, want all 9 items", len(got.Backlog))
	}
	if len(got.Ready) != 1 || got.Ready[0].Bead != "app-next" || got.Ready[0].PR != nil {
		t.Errorf("ready = %+v", got.Ready)
	}
}

func TestRunDigestJSONPreservesMarkdownAndDelivery(t *testing.T) {
	runOnce := func(t *testing.T, asJSON bool) (stdout, md string, desktopCalls [][2]string, slackCalls []string) {
		t.Helper()
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		in := fixtureInputs(t)
		env := digestEnv{
			open: func() ([]Bead, error) { return in.open, nil },
			agents: func() ([]Agent, error) {
				var out []Agent
				for _, a := range in.agents {
					out = append(out, a)
				}
				return out, nil
			},
			ticker: func() TickerState {
				return TickerState{PRs: map[string]PR{"app-run": {URL: pr + "6"}}}
			},
			lookup: func(urls []string) (map[string]DigestPR, int, error) {
				out := map[string]DigestPR{}
				for _, u := range urls {
					if p, ok := in.prs[u]; ok {
						out[u] = p
					}
				}
				return out, 1, nil
			},
		}
		deliv := digestDelivery{
			desktop: func(title, body string) error {
				// Strip temp path suffix so the two runs can be compared directly.
				body, _, _ = strings.Cut(body, "\n\nFull digest: ")
				desktopCalls = append(desktopCalls, [2]string{title, body})
				return nil
			},
			slack: func(msg string) error {
				msg, _, _ = strings.Cut(msg, "\n\nFull digest: ")
				slackCalls = append(slackCalls, msg)
				return nil
			},
		}
		var buf bytes.Buffer
		if err := runDigestWith(Config{Repo: "/r"}, env, deliv, "desktop,slack", false, asJSON, t0, &buf); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(digestPath())
		if err != nil {
			t.Fatalf("digest.md not written: %v", err)
		}
		if _, err := os.Stat(digestStatePath()); err != nil {
			t.Fatalf("digest-state.json not written: %v", err)
		}
		return buf.String(), string(b), desktopCalls, slackCalls
	}

	textOut, textMD, textDesktop, textSlack := runOnce(t, false)
	jsonOut, jsonMD, jsonDesktop, jsonSlack := runOnce(t, true)

	if textMD != jsonMD {
		t.Errorf("digest.md changed under --json:\n--- text ---\n%s\n--- json ---\n%s", textMD, jsonMD)
	}
	if !slices.Equal(textDesktop, jsonDesktop) {
		t.Errorf("desktop delivery changed: text=%v json=%v", textDesktop, jsonDesktop)
	}
	if !slices.Equal(textSlack, jsonSlack) {
		t.Errorf("slack delivery changed: text=%v json=%v", textSlack, jsonSlack)
	}
	if !strings.Contains(textOut, "wrote ") {
		t.Errorf("non-json stdout should keep status line, got %q", textOut)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &parsed); err != nil {
		t.Fatalf("digest --json stdout is not valid JSON: %v\n%s", err, jsonOut)
	}
	for _, key := range []string{"at", "summary", "changes", "needs_you", "waiting", "merged", "rolling_out", "blocked", "in_progress", "ready", "backlog"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("digest --json missing key %q", key)
		}
	}
}
