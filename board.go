package main

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

type boardRow struct {
	state  *ThreadState
	header string
	// count is a header's number of rows.
	count int
	group Group
	bead  Bead
	agent *Agent
	line  string
	color string
	run   string
	// search is the lowercased text '/' matches: id, title, status, agent,
	// state, PR, Linear key, labels and the section the row sits in.
	search string
	// prs, linear, moves and runs are for the detail pane: the bead's PRs as
	// the ticker last saw them, its Linear key, where that issue should be,
	// and its workflow runs.
	prs    []PR
	linear string
	moves  []string
	runs   []Run
	native *rpcVisibility
}

func searchText(parts ...string) string {
	return strings.ToLower(strings.Join(parts, "\x00"))
}

const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[2m"
	ansiBold   = "\x1b[1m"
	ansiRed    = "\x1b[31m"
	ansiYellow = "\x1b[33m"
	ansiGreen  = "\x1b[32m"
	ansiCyan   = "\x1b[36m"
	ansiInvert = "\x1b[7m"
)

func groupColor(g Group) string {
	switch g {
	case GroupNeedsYou, GroupFailing:
		return ansiRed
	case GroupReview, GroupMerged, GroupRollingOut:
		return ansiYellow
	case GroupWorking, GroupReady:
		return ansiGreen
	case sectionNext:
		return ansiCyan
	default:
		return ansiDim
	}
}

// How often the board refreshes on its own. Agents are one cheap herdr call;
// beads are a bd call that costs a second or more and queues behind every
// worker's, so they refresh rarely and after each action.
const (
	agentsEvery = 2 * time.Second
	localEvery  = 5 * time.Second
	beadsEvery  = 30 * time.Second
)

// boardEnv is everything the board reads or changes, so the model is testable
// without bd, herdr, git or a terminal.
type boardEnv struct {
	beads  func() ([]Bead, error)
	agents func() ([]Agent, error)
	// local is the ticker's last state and the unhandled inbox, both files.
	local  func() (TickerState, []InboxItem)
	events func() ([]NotifyFeedEvent, []journalRecord, error)
	show   func(id string) (Bead, error)
	read   func(pane string) (string, error)
	gc     func(data boardData) ([]gcItem, error)
	// Native viewing is independent of focus/dispatch and of live agents.
	native     func() map[string]rpcVisibility
	isNative   func(id string) bool
	viewNative func(id, title string, links []string) error

	readPrompt    func(id string) (ApprovalPrompt, error)
	answerPrompt  func(id, choice, expect string) (string, error)
	answerNeedsMe func(id, choice, text, expect string) (string, error)
	completeRun   func(id, command, result string) (string, error)
	snoozes       func() SnoozeState
	snooze        func(id, dur string) (string, error)
	clearSnooze   func(id string) (string, error)

	note           func(id, text string) error
	setStatus      func(id, status string) error
	setLabel       func(id, label string, on bool) error
	setPriority    func(id string, p int) error
	close          func(id, reason string) error
	child          func(parent Bead, title string) (string, error)
	create         func(title, repo, role string) (string, error)
	prompt         func(agent, text string) error
	askCoordinator func(text string) (string, error)
	dispatch       func(id, kind, role string, focus bool) (string, error)
	resume         func(id, kind string) (string, error)
	resolve        func(id string) (string, error)
	release        func(b Bead, days int) error
	autonomy       func(id string, n int, d time.Duration) (string, error)
	focus          func(agent string) error
	inboxDone      func(files []string) error
	copy           func(text string) error
	open           func(url string) string
}

// askBoardCoordinator forwards a free-form natural-language prompt to the
// running interactive coordinator session/pane without leaving the board.
// Refuses when no coordinator is running or when it is busy/blocked.
func askBoardCoordinator(cfg Config, listAgents func() ([]Agent, error), send func(target, text string) error, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("nothing to send to coordinator")
	}
	agents, err := listAgents()
	if err != nil {
		return "", fmt.Errorf("list agents: %w", err)
	}
	byName := make(map[string]Agent, len(agents))
	for _, a := range agents {
		if a.Name != "" {
			byName[a.Name] = a
		}
	}
	coord, ok := findCoordinatorAgent(cfg, byName)
	if !ok {
		return "", fmt.Errorf("coordinator is not running; open it with `quartermaster coordinator`")
	}
	if !agentReady(&coord) {
		return "", fmt.Errorf("coordinator (%s) is %s; wait until it is idle", coord.Name, coord.Status)
	}
	if err := send(coord.Name, text); err != nil {
		return "", err
	}
	return "sent to coordinator (" + coord.Name + ")", nil
}

// completeBoardRun records a completed RUN: command as RAN: on the bead,
// returns the bead to in_progress, and prompts an idle worker to continue.
func completeBoardRun(env promptEnv, id, command, result string) (string, error) {
	b, err := env.show(id)
	if err != nil {
		return "", err
	}
	res := strings.TrimSpace(result)
	if res == "" {
		res = "ok"
	}
	note := fmt.Sprintf("RAN: %s → %s", command, res)
	if err := env.note(id, note); err != nil {
		return "", fmt.Errorf("noting RAN on %s failed: %w", id, err)
	}
	if env.setStatus == nil {
		return "", fmt.Errorf("RAN noted on %s but setting status is unavailable", id)
	}
	if err := env.setStatus(id, StatusInProgress); err != nil {
		return "", fmt.Errorf("RAN noted on %s but returning to in_progress failed: %w", id, err)
	}
	msg := fmt.Sprintf("recorded RAN on %s; bead is in_progress", id)
	host, a := questionWorker(env, b)
	switch {
	case a == nil:
		return msg + " (no worker running)", nil
	case !agentReady(a):
		return msg + fmt.Sprintf(" (worker is %s)", a.Status), nil
	case env.tell == nil:
		return msg, nil
	}
	prompt := fmt.Sprintf("User ran the requested RUN command:\n%s\nResult: %s\nRecorded as `RAN:` on the bead and returned to in_progress. Carry on.", command, res)
	if err := env.tell(host, a, prompt); err != nil {
		return msg + " (prompting worker failed: " + err.Error() + ")", nil
	}
	return msg + "; prompted worker", nil
}

func liveBoardEnv(cfg Config, h Herdr) boardEnv {
	bd := Beads{}
	cache := boardCachePath()
	pEnv := livePromptEnv(cfg, h)
	return boardEnv{
		beads: func() ([]Bead, error) {
			beads, err := bd.list("--status", strings.Join(boardStatuses, ","))
			if err == nil {
				_ = saveBoardCache(cache, beads, time.Now())
			}
			return beads, err
		},
		agents: h.Agents,
		local: func() (TickerState, []InboxItem) {
			inbox, _ := readInbox()
			return loadState(), inbox
		},
		events: func() ([]NotifyFeedEvent, []journalRecord, error) {
			return loadBoardEvents(time.Now())
		},
		show:       bd.Show,
		read:       func(pane string) (string, error) { return h.Read(pane, 40, detailTimeout) },
		gc:         func(data boardData) ([]gcItem, error) { return loadGC(cfg, h, data) },
		native:     rpcVisibilityAll,
		isNative:   rpcExists,
		viewNative: runRPCView,

		readPrompt: func(id string) (ApprovalPrompt, error) {
			p, _, _, err := workerPrompt(pEnv, id)
			return p, err
		},
		answerPrompt: func(id, choice, expect string) (string, error) {
			return answerPromptVia(pEnv, id, choice, expect, "TJ", -1, "board")
		},
		answerNeedsMe: func(id, choice, text, expect string) (string, error) {
			return answerNeedsMeTextVia(pEnv, id, choice, text, expect, "board")
		},
		completeRun: func(id, command, result string) (string, error) {
			return completeBoardRun(pEnv, id, command, result)
		},
		snoozes: loadSnoozeState,
		snooze: func(id, dur string) (string, error) {
			var buf bytes.Buffer
			err := snoozeCmd(cfg, liveSnoozeEnv(cfg), []string{id, dur}, &buf)
			return strings.TrimSpace(buf.String()), err
		},
		clearSnooze: func(id string) (string, error) {
			var buf bytes.Buffer
			err := snoozeCmd(cfg, liveSnoozeEnv(cfg), []string{id, "--clear"}, &buf)
			return strings.TrimSpace(buf.String()), err
		},

		note:      bd.Note,
		setStatus: bd.SetStatus,
		setLabel:  bd.SetLabel,
		setPriority: func(id string, p int) error {
			_, err := bd.run("update", id, "--priority", fmt.Sprint(p))
			return err
		},
		close: func(id, reason string) error {
			return closeBead(cfg, liveCloseEnv(cfg), id, reason, "TJ", io.Discard)
		},
		child: func(parent Bead, title string) (string, error) {
			out, err := bd.run("create", title, "-t", "task", "-p", fmt.Sprint(parent.Priority), "--parent", parent.ID, "--silent")
			return strings.TrimSpace(string(out)), err
		},
		create: func(title, repo, role string) (string, error) {
			args := []string{"create", title, "-t", "task", "-p", "2"}
			var labels []string
			if repo != "" {
				labels = append(labels, repoLabel+repo)
			}
			if role != "" && role != roleImplementer {
				labels = append(labels, roleLabel+role)
			}
			if len(labels) > 0 {
				args = append(args, "-l", strings.Join(labels, ","))
			}
			args = append(args, "--silent")
			out, err := bd.run(args...)
			return strings.TrimSpace(string(out)), err
		},
		prompt: h.Prompt,
		askCoordinator: func(text string) (string, error) {
			return askBoardCoordinator(cfg, h.Agents, h.Prompt, text)
		},
		autonomy: func(id string, n int, d time.Duration) (string, error) {
			return setLevel(cfg, liveAutonomyEnv(cfg, h), id, n, d, "TJ", time.Now())
		},
		dispatch: func(id, kind, role string, focus bool) (string, error) {
			return dispatch(cfg, h, id, DispatchOpts{Kind: kind, Role: role, Focus: focus, By: "TJ"})
		},
		resume: func(id, kind string) (string, error) {
			lines, err := resume(cfg, h, []string{id}, kind, "TJ")
			return strings.Join(lines, "; "), err
		},
		resolve: func(id string) (string, error) { return resolve(cfg, h, id, false) },
		release: func(b Bead, days int) error {
			if err := bd.Note(b.ID, fmt.Sprintf("released by quartermaster board: no agent or worktree for %dd", days)); err != nil {
				return err
			}
			return bd.SetStatus(b.ID, StatusOpen)
		},
		focus: h.Focus,
		inboxDone: func(files []string) error {
			_, err := inboxDoneFiles(files)
			return err
		},
		copy: func(text string) error { return copyViaTerminal(os.Stdout, text) },
		open: func(u string) string { return openBoardURL(os.Stdout, u) },
	}
}

// Messages are what finishes off the UI: loads and actions run as goroutines
// and send one of these back.
type (
	msg      any
	cmd      func() msg
	beadsMsg struct {
		beads []Bead
		at    time.Time
		err   error
	}
	agentsMsg struct {
		agents []Agent
		err    error
	}
	localMsg struct {
		st     TickerState
		inbox  []InboxItem
		snooze SnoozeState
	}
	gcMsg struct {
		items []gcItem
		err   error
		took  time.Duration
	}
	// doneMsg is an action finishing: its message, and what to reload.
	doneMsg struct {
		text         string
		err          error
		reload       bool
		reloadLocal  bool
		reloadAgents bool
		gc           bool
		closeAnswer  bool
		advanceQueue bool
		answeredID   string
		quit         bool
	}
)

type boardView int

const (
	viewList boardView = iota
	viewDetail
	viewGC
	viewEvents
	viewAnswer
	viewQueue
	viewHelp
)

// ask is the bottom line asking for something: free text (a note, an
// answer, a reason), one key of a few (a status, a priority), or y to
// confirm. Esc cancels any of them.
type ask struct {
	prompt     string
	text       bool
	allowEmpty bool
	keys       string
	buf        string
	then       func(answer string) []cmd
}

// confirm asks y/n before do runs; any key but y cancels.
func confirm(prompt string, do func() []cmd) *ask {
	return &ask{prompt: prompt + "  y/n", keys: "y", then: func(string) []cmd { return do() }}
}

type board struct {
	cfg Config
	env boardEnv
	now func() time.Time

	data boardData
	// all is every row; rows is what's shown and what selection indexes: all,
	// narrowed by the '/' filter.
	all    []boardRow
	rows   []boardRow
	cur    int
	sort   sortMode
	filter boardFilter
	view   boardView
	// back is the view help returns to.
	back   boardView
	status string
	ask    *ask

	beadsLoading, agentsLoading, localLoading bool
	nativeLoading                             bool
	nativeAt                                  time.Time
	// reloadAgain asks for another bead load once the running one returns,
	// because an action wrote after it started.
	reloadAgain       bool
	beadsErr          string
	agentsAt, localAt time.Time
	beadsTried        time.Time

	// details caches each bead's slow lookups until a bead reload; scroll is
	// how far the pane is scrolled, starting over on each bead.
	details    map[string]*beadDetail
	scroll     int
	scrolledOn string

	// marked tracks main-board multi-selected bead IDs for bulk actions.
	marked map[string]bool

	// ans holds the live approval prompt, structured needs_me question, or RUN
	// command shown in viewAnswer.
	ans *answerState

	// Responsive layout mode and hit targets for SGR mouse/touch interaction.
	layout layoutMode
	hits   boardHits

	// Decision Queue (Q / --queue) state: prioritized items needing user input.
	queueHome        bool
	queue            []queueItem
	queueCur         int
	queueSnoozed     int
	queueShowSnoozed bool
	queueDone        map[string]bool

	// Live Events / Inbox view (E) state.
	feed      []NotifyFeedEvent
	autoRecs  []journalRecord
	evRows    []eventRow
	evCur     int
	evLoading bool
	evLoaded  bool
	evErr     string
	evAt      time.Time

	gc        []gcItem
	gcLoading bool
	gcLoaded  bool
	gcErr     string
	gcTook    time.Duration
	gcCur     int
	gcMarked  map[string]bool

	quit bool
	// handoff, when set, is run by the loop with the terminal handed back
	// (the pager).
	handoff func() error
}

func newBoard(cfg Config, env boardEnv, data boardData, now func() time.Time) *board {
	m := &board{
		cfg:       cfg,
		env:       env,
		now:       now,
		data:      data,
		details:   map[string]*beadDetail{},
		marked:    map[string]bool{},
		gcMarked:  map[string]bool{},
		queueDone: map[string]bool{},
	}
	m.rebuild()
	m.rebuildEvents()
	return m
}

// rebuild lays the rows out again from data, keeping the selection on the
// same bead when it is still shown.
func (m *board) rebuild() {
	var id string
	if r := m.sel(); r != nil {
		id = r.bead.ID
	}
	m.all = buildRows(m.cfg, m.data, m.sort)
	present := make(map[string]bool, len(m.all))
	for _, r := range m.all {
		if r.header == "" {
			present[r.bead.ID] = true
		}
	}
	for mid := range m.marked {
		if !present[mid] {
			delete(m.marked, mid)
		}
	}
	m.rows = filterRows(m.all, m.filter.query)
	m.cur = -1
	for i, r := range m.rows {
		if r.header == "" && id != "" && r.bead.ID == id {
			m.cur = i
		}
	}
	if m.cur < 0 {
		m.cur = nextSelectable(m.rows, 0, 1)
	}
	m.rebuildQueue()
}

func (m *board) sel() *boardRow {
	if m.cur < 0 || m.cur >= len(m.rows) || m.rows[m.cur].header != "" {
		return nil
	}
	return &m.rows[m.cur]
}

// tick starts whatever refresh is due.
func (m *board) tick() []cmd {
	now := m.now()
	var cmds []cmd
	if m.env.native != nil && !m.nativeLoading && now.Sub(m.nativeAt) >= agentsEvery {
		m.nativeLoading = true
		cmds = append(cmds, func() msg { return nativeViewMsg{m.env.native()} })
	}
	if !m.agentsLoading && now.Sub(m.agentsAt) >= agentsEvery {
		m.agentsLoading = true
		cmds = append(cmds, func() msg {
			a, err := m.env.agents()
			return agentsMsg{a, err}
		})
	}
	if !m.localLoading && now.Sub(m.localAt) >= localEvery {
		m.localLoading = true
		cmds = append(cmds, func() msg {
			st, inbox := m.env.local()
			var sn SnoozeState
			if m.env.snoozes != nil {
				sn = m.env.snoozes()
			}
			return localMsg{st: st, inbox: inbox, snooze: sn}
		})
	}
	if !m.beadsLoading && now.Sub(m.beadsTried) >= beadsEvery {
		cmds = append(cmds, m.loadBeads())
	}
	if m.view == viewDetail {
		cmds = append(cmds, m.loadDetail()...)
	} else if m.view == viewList || m.view == viewQueue {
		cmds = append(cmds, m.loadDetailIfWide()...)
	}
	if m.view == viewGC && !m.gcLoaded && !m.gcLoading {
		cmds = append(cmds, m.loadGC())
	}
	if m.view == viewEvents && !m.evLoading && (!m.evLoaded || now.Sub(m.evAt) >= localEvery) {
		if c := m.loadEvents(); c != nil {
			cmds = append(cmds, c)
		}
	}
	return cmds
}

// loadBeads starts a bead load, or asks for one after the running one.
func (m *board) loadBeads() cmd {
	if m.beadsLoading {
		m.reloadAgain = true
		return nil
	}
	m.beadsLoading = true
	m.beadsTried = m.now()
	return func() msg {
		b, err := m.env.beads()
		return beadsMsg{b, time.Now(), err}
	}
}

func (m *board) loadGC() cmd {
	m.gcLoading = true
	data := m.data
	return func() msg {
		start := time.Now()
		items, err := m.env.gc(data)
		return gcMsg{items, err, time.Since(start)}
	}
}

// loadDetail starts the selected bead's slow lookups once: bd show for its
// description, and the end of its agent's screen.
func (m *board) loadDetail() []cmd {
	r := m.sel()
	if r == nil {
		return nil
	}
	return m.loadDetailFor(*r)
}

func (m *board) loadDetailFor(r boardRow) []cmd {
	id := r.bead.ID
	if _, ok := m.details[id]; ok {
		return nil
	}
	var inbox []InboxItem
	for _, it := range m.data.Inbox {
		if it.Bead == id {
			inbox = append(inbox, it)
		}
	}
	m.details[id] = &beadDetail{inbox: inbox}
	cmds := []cmd{func() msg {
		b, err := m.env.show(id)
		return detailResult{id, func(d *beadDetail) {
			if err != nil {
				d.fullErr = err.Error()
				return
			}
			d.full = &b
		}}
	}}
	if r.agent != nil {
		pane, working := r.agent.PaneID, r.agent.Status == "working"
		cmds = append(cmds, func() msg {
			text, err := m.env.read(pane)
			return detailResult{id, func(d *beadDetail) {
				d.tailDone = true
				switch {
				case err == nil:
					d.tail = tailLines(text, detailTailLines)
				case !working:
					// A working agent's screen can be mid-redraw; its read failing
					// isn't worth showing.
					d.tailErr = err.Error()
				}
			}}
		})
	}
	return cmds
}

// apply takes in one finished load or action.
func (m *board) apply(x msg) []cmd {
	switch x := x.(type) {
	case nativeViewMsg:
		m.nativeLoading = false
		m.nativeAt = m.now()
		m.data.Native = x.workers
		m.rebuild()
	case beadsMsg:
		m.beadsLoading = false
		var cmds []cmd
		if m.reloadAgain {
			m.reloadAgain = false
			cmds = append(cmds, m.loadBeads())
		}
		if x.err != nil {
			m.beadsErr = x.err.Error()
			return cmds
		}
		m.beadsErr = ""
		m.data.Beads, m.data.BeadsAt = x.beads, x.at
		m.queueDone = map[string]bool{}
		// Notes and status may have moved: look the details up again.
		m.details = map[string]*beadDetail{}
		m.rebuild()
		return cmds
	case agentsMsg:
		m.agentsLoading = false
		m.agentsAt = m.now()
		if x.err == nil {
			m.data.Agents = x.agents
			m.rebuild()
		}
	case localMsg:
		m.localLoading = false
		m.localAt = m.now()
		m.data.State, m.data.Inbox = x.st, x.inbox
		if x.snooze.Beads != nil || m.env.snoozes != nil {
			m.data.Snooze = x.snooze
		}
		m.rebuild()
		m.rebuildEvents()
	case eventsMsg:
		m.evLoading, m.evLoaded = false, true
		m.evAt = m.now()
		if x.err != nil {
			m.evErr = x.err.Error()
			return nil
		}
		m.evErr = ""
		m.feed, m.autoRecs = x.feed, x.journal
		m.rebuildEvents()
	case answerLoadMsg:
		m.applyAnswerLoad(x)
	case gcMsg:
		m.gcLoading, m.gcLoaded = false, true
		m.gcErr, m.gcTook = "", x.took
		if x.err != nil {
			m.gcErr = x.err.Error()
			return nil
		}
		m.gc = x.items
		m.gcCur = min(m.gcCur, max(0, len(m.gc)-1))
		for id := range m.gcMarked {
			if !m.gcHas(id) {
				delete(m.gcMarked, id)
			}
		}
	case detailResult:
		if d, ok := m.details[x.id]; ok {
			x.apply(d)
		}
	case doneMsg:
		switch {
		case x.err != nil && x.text != "":
			m.status = x.text + "; " + x.err.Error()
		case x.err != nil:
			m.status = x.err.Error()
		default:
			m.status = x.text
		}
		if x.closeAnswer && x.err == nil && m.view == viewAnswer && m.ans != nil {
			m.view = m.ans.back
			m.ans = nil
		}
		var cmds []cmd
		if x.advanceQueue && x.err == nil {
			if x.answeredID != "" {
				m.queueDone[x.answeredID] = true
			}
			m.rebuildQueue()
			if m.view == viewAnswer && m.ans != nil && m.ans.fromQueue {
				cmds = append(cmds, m.openQueueStep(min(m.queueCur, max(0, len(m.queue)-1)))...)
			}
		}
		if x.quit && x.err == nil {
			m.quit = true
		}
		if x.reload {
			cmds = append(cmds, m.loadBeads())
		}
		if x.reloadLocal {
			m.localAt = time.Time{}
			m.evAt = time.Time{}
			cmds = append(cmds, m.tick()...)
		} else if x.reloadAgents {
			m.agentsAt = time.Time{}
			cmds = append(cmds, m.tick()...)
		}
		if x.gc {
			m.gcLoaded = false
		}
		return cmds
	}
	return nil
}

func (m *board) gcHas(id string) bool {
	for _, it := range m.gc {
		if it.bead.ID == id {
			return true
		}
	}
	return false
}

// edit applies one key to a line being typed: it reports whether Enter
// submitted it and whether Esc dropped it.
func edit(buf *string, k string) (submit, cancel bool) {
	switch k {
	case "\r", "\n":
		return true, false
	case "\x1b":
		return false, true
	case "\x7f", "\b":
		if r := []rune(*buf); len(r) > 0 {
			*buf = string(r[:len(r)-1])
		}
	case "\x15": // ctrl-u
		*buf = ""
	case "\x17": // ctrl-w
		s := strings.TrimRight(*buf, " ")
		*buf = s[:strings.LastIndex(s, " ")+1]
	default:
		if r, _ := utf8.DecodeRuneInString(k); len(k) > 0 && unicode.IsPrint(r) && utf8.RuneCountInString(k) == 1 {
			*buf += k
		}
	}
	return false, false
}

// key handles one keypress (or SGR mouse/touch sequence) and returns the work it starts.
func (m *board) key(k string) []cmd {
	if k == "\x03" {
		m.quit = true
		return nil
	}
	if ev, ok := parseSGRMouse(k); ok {
		return m.handleMouse(ev)
	}
	if a := m.ask; a != nil {
		if a.text {
			submit, cancel := edit(&a.buf, k)
			switch {
			case cancel:
				m.ask, m.status = nil, "cancelled"
			case submit && strings.TrimSpace(a.buf) == "" && !a.allowEmpty:
				m.ask, m.status = nil, "cancelled: nothing typed"
			case submit:
				m.ask = nil
				return a.then(strings.TrimSpace(a.buf))
			}
			return nil
		}
		m.ask = nil
		if k == "\n" && strings.Contains(a.keys, "\r") {
			k = "\r"
		}
		if len(k) == 1 && strings.Contains(a.keys, k) {
			return a.then(k)
		}
		m.status = "cancelled"
		return nil
	}
	if m.filter.typing {
		if m.filter.key(k) {
			m.rows = filterRows(m.all, m.filter.query)
			m.cur = nextSelectable(m.rows, 0, 1)
		}
		return nil
	}
	m.status = ""
	switch m.view {
	case viewHelp:
		m.view = m.back
		return nil
	case viewGC:
		return m.gcKey(k)
	case viewEvents:
		return m.eventsKey(k)
	case viewAnswer:
		return m.answerKey(k)
	case viewQueue:
		return m.queueKey(k)
	case viewDetail:
		if m.detailKey(k) {
			return nil
		}
	}
	return m.listKey(k)
}

func (m *board) detailKey(k string) bool {
	switch k {
	case "\x1b", "q", " ", "v":
		m.view = viewList
	case "J", "\x1b[6~":
		m.scroll += 5
	case "K", "\x1b[5~":
		m.scroll = max(0, m.scroll-5)
	default:
		return false
	}
	return true
}

// move steps the selection by n selectable rows, stopping at either end.
func (m *board) move(n int) {
	dir := 1
	if n < 0 {
		dir, n = -1, -n
	}
	for ; n > 0; n-- {
		i := nextSelectable(m.rows, m.cur+dir, dir)
		if i < 0 {
			return
		}
		m.cur = i
	}
}

// jumpSection moves to the first row of the next (dir 1) or previous section.
func (m *board) jumpSection(dir int) {
	// The current section's header.
	h := m.cur
	for h > 0 && m.rows[h].header == "" {
		h--
	}
	target := -1
	if dir > 0 {
		for i := h + 1; i < len(m.rows); i++ {
			if m.rows[i].header != "" {
				target = i
				break
			}
		}
	} else {
		for i := h - 1; i >= 0; i-- {
			if m.rows[i].header != "" {
				target = i
				break
			}
		}
	}
	if target < 0 {
		return
	}
	if i := nextSelectable(m.rows, target, 1); i >= 0 {
		m.cur = i
	}
}

// page is how far ctrl-d and ctrl-u move.
const page = 10

func (m *board) listKey(k string) []cmd {
	switch k {
	case "q", "\x1b":
		// Esc clears a kept filter, then clears marks, before it closes the board.
		if k == "\x1b" && m.filter.query != "" {
			m.filter.query = ""
			m.rebuild()
			return nil
		}
		if k == "\x1b" && len(m.marked) > 0 {
			m.marked = map[string]bool{}
			m.status = "cleared marks"
			return nil
		}
		m.quit = true
		return nil
	case "?":
		m.back, m.view = m.view, viewHelp
		return nil
	case "j", "\x1b[B":
		m.move(1)
		return m.loadDetailIfWide()
	case "k", "\x1b[A":
		m.move(-1)
		return m.loadDetailIfWide()
	case "\x04", "\x1b[6~":
		m.move(page)
		return m.loadDetailIfWide()
	case "\x15", "\x1b[5~":
		m.move(-page)
		return m.loadDetailIfWide()
	case "g", "\x1b[H":
		m.cur = nextSelectable(m.rows, 0, 1)
		return m.loadDetailIfWide()
	case "G", "\x1b[F":
		m.cur = nextSelectable(m.rows, len(m.rows)-1, -1)
		return m.loadDetailIfWide()
	case "]", "\t":
		m.jumpSection(1)
		return m.loadDetailIfWide()
	case "[", "\x1b[Z":
		m.jumpSection(-1)
		return m.loadDetailIfWide()
	case "J":
		if effectiveLayout(m.layout, m.hits.width) == layoutWide {
			m.scroll += 5
		}
		return nil
	case "K":
		if effectiveLayout(m.layout, m.hits.width) == layoutWide {
			m.scroll = max(0, m.scroll-5)
		}
		return nil
	case "/":
		m.filter.typing = true
		return nil
	case ":":
		m.ask = &ask{prompt: "go to bead: ", text: true, then: func(id string) []cmd { m.jump(id); return nil }}
		return nil
	case "s":
		m.sort = (m.sort + 1) % 3
		m.rebuild()
		m.status = "sorted by " + m.sort.String()
		return nil
	case "L":
		m.cycleLayout()
		return m.loadDetailIfWide()
	case "Q":
		return m.openQueue()
	case "r":
		m.status = "refreshing…"
		m.agentsAt, m.localAt = time.Time{}, time.Time{}
		return append(m.tick(), m.loadBeads())
	case "T":
		m.view = viewGC
		return m.tick()
	case "E":
		m.view = viewEvents
		m.rebuildEvents()
		m.evAt = time.Time{}
		m.localAt = time.Time{}
		return m.tick()
	case "N":
		m.createAsk()
		return nil
	case "@":
		m.askCoordinatorBar()
		return nil
	case ";", "M":
		if r := m.sel(); r != nil {
			id := r.bead.ID
			if m.marked[id] {
				delete(m.marked, id)
			} else {
				m.marked[id] = true
			}
			m.move(1)
		}
		return nil
	case "*":
		m.toggleSectionMarks()
		return nil
	case " ", "v":
		if m.sel() != nil {
			m.view = viewDetail
			return m.loadDetail()
		}
		return nil
	}
	if r := m.sel(); r != nil {
		return m.beadKey(k, *r)
	}
	return nil
}

// jump selects the bead whose id is id, else ends with it, else contains it,
// clearing a filter that hides it.
func (m *board) jump(id string) {
	id = strings.ToLower(id)
	find := func(rows []boardRow) int {
		for _, match := range []func(string) bool{
			func(s string) bool { return s == id },
			func(s string) bool { return strings.HasSuffix(s, id) },
			func(s string) bool { return strings.Contains(s, id) },
		} {
			for i, r := range rows {
				if r.header == "" && match(strings.ToLower(r.bead.ID)) {
					return i
				}
			}
		}
		return -1
	}
	if i := find(m.rows); i >= 0 {
		m.cur = i
		return
	}
	if i := find(m.all); i >= 0 {
		m.filter.query = ""
		m.rows = m.all
		m.cur = i
		return
	}
	m.status = "no open bead matches " + id
}

// beadKey is the keys that act on the selected bead (or marked beads for bulk actions).
func (m *board) beadKey(k string, r boardRow) []cmd {
	id := r.bead.ID
	switch k {
	case "V":
		m.openNative(r)
	case "\r":
		if m.openNative(r) {
			return nil
		}
		if r.agent != nil {
			name := r.agent.Name
			return []cmd{func() msg { return doneMsg{err: m.env.focus(name), quit: true} }}
		}
		m.dispatchAsk([]boardRow{r}, "", "", false)
	case "c", "x", "m", "i", "t":
		kind := map[string]string{"c": "claude", "x": "codex", "m": "muse", "i": "pi", "t": "agy"}[k]
		if len(m.marked) > 0 {
			targets, ok := m.boardTargets()
			if !ok {
				return nil
			}
			eligible := filterRowsWithoutAgent(targets)
			if len(eligible) == 0 {
				m.status = "nothing to dispatch in the selection (all already have agents)"
				return nil
			}
			m.dispatchAsk(eligible, kind, "", false)
			return nil
		}
		m.dispatchAsk([]boardRow{r}, kind, "", false)
	case "D":
		if len(m.marked) > 0 {
			targets, ok := m.boardTargets()
			if !ok {
				return nil
			}
			eligible := filterRowsWithoutAgent(targets)
			if len(eligible) == 0 {
				m.status = "nothing to dispatch in the selection (all already have agents)"
				return nil
			}
			m.dispatchRolePick(eligible, "")
			return nil
		}
		m.dispatchRolePick([]boardRow{r}, "")
	case "R":
		if len(m.marked) > 0 {
			targets, ok := m.boardTargets()
			if !ok {
				return nil
			}
			eligible := filterRowsWithoutAgent(targets)
			if len(eligible) == 0 {
				m.status = "nothing to resume in the selection (all already have agents)"
				return nil
			}
			m.resumeAsk(eligible, "")
			return nil
		}
		if r.agent != nil {
			m.status = id + " already has an agent (" + r.agent.Name + ")"
			return nil
		}
		m.resumeAsk([]boardRow{r}, "")
	case "a":
		m.ask = &ask{prompt: "note on " + id + ": ", text: true, then: func(text string) []cmd {
			m.local(id, func(b *Bead) { b.Notes = strings.TrimLeft(b.Notes+"\n"+text, "\n") })
			return []cmd{func() msg {
				return doneMsg{text: "noted on " + id, err: m.env.note(id, text), reload: true}
			}}
		}}
	case "A":
		return m.openAnswer(r)
	case "f":
		return m.followRecommendation(r, false)
	case "z":
		m.snoozeAsk(id, false)
		return nil
	case "n":
		if len(m.marked) > 0 {
			targets, ok := m.boardTargets()
			if !ok {
				return nil
			}
			on := false
			for _, t := range targets {
				if !t.bead.HasLabel(LabelNext) {
					on = true
					break
				}
			}
			for _, t := range targets {
				for i := range m.data.Beads {
					if m.data.Beads[i].ID == t.bead.ID {
						m.data.Beads[i].Labels = without(m.data.Beads[i].Labels, LabelNext)
						if on {
							m.data.Beads[i].Labels = append(m.data.Beads[i].Labels, LabelNext)
						}
					}
				}
			}
			m.rebuild()
			word := map[bool]string{true: "labelled next", false: "unlabelled next"}[on]
			return m.bulkRun(word, targets, func(t boardRow) (string, error) {
				return "", m.env.setLabel(t.bead.ID, LabelNext, on)
			})
		}
		on := !r.bead.HasLabel(LabelNext)
		m.local(id, func(b *Bead) {
			if on {
				b.Labels = append(b.Labels, LabelNext)
			} else {
				b.Labels = without(b.Labels, LabelNext)
			}
		})
		word := map[bool]string{true: "labelled next", false: "unlabelled next"}[on]
		return []cmd{func() msg {
			return doneMsg{text: id + " " + word, err: m.env.setLabel(id, LabelNext, on), reload: true}
		}}
	case "U", "0", "1", "2", "3", "4", "5", "6", "7", "8", "9":
		lvl, _ := m.cfg.beadLevel(r.bead, autonomyStopped())
		n, _ := strconv.Atoi(k)
		if k == "U" {
			// The usual stops: supervised, the default, autonomous, then round.
			n = levelSupervised
			for _, s := range []int{levelSupervised, 4, levelAutonomous} {
				if s > lvl {
					n = s
					break
				}
			}
		}
		set := func(d time.Duration) []cmd {
			m.local(id, func(b *Bead) { b.Labels = withLevel(b.Labels, n) })
			return []cmd{func() msg {
				text, err := m.env.autonomy(id, n, d)
				return doneMsg{text: text, err: err, reload: true}
			}}
		}
		if n >= 8 {
			// 8 and 9 are leases; entering how long is the confirmation.
			hint := "empty for 4h"
			if n == 9 {
				hint = "required"
			}
			m.ask = &ask{prompt: fmt.Sprintf("%s autonomy level %d (now %d) for how long, e.g. 2h (%s): ", id, n, lvl, hint), text: true, then: func(a string) []cmd {
				var d time.Duration
				if a = strings.TrimSpace(a); a != "" {
					var err error
					if d, err = time.ParseDuration(a); err != nil || d <= 0 {
						return []cmd{func() msg { return doneMsg{err: fmt.Errorf("not a duration: %q", a)} }}
					}
				}
				return set(d)
			}}
			return nil
		}
		m.ask = confirm(fmt.Sprintf("set %s to autonomy level %d (now %d)?", id, n, lvl), func() []cmd { return set(0) })
	case "S":
		statuses := map[string]string{"o": StatusOpen, "i": StatusInProgress, "b": StatusBlocked, "m": StatusNeedsMe}
		m.ask = &ask{prompt: id + " status: o open · i in_progress · b blocked · m needs_me", keys: "oibm", then: func(a string) []cmd {
			status := statuses[a]
			if status == r.bead.Status {
				m.status = id + " is already " + status
				return nil
			}
			m.local(id, func(b *Bead) { b.Status = status })
			return []cmd{func() msg {
				return doneMsg{text: id + " → " + status, err: m.env.setStatus(id, status), reload: true}
			}}
		}}
	case "P":
		m.ask = &ask{prompt: fmt.Sprintf("%s priority (now P%d): 0-4", id, r.bead.Priority), keys: "01234", then: func(a string) []cmd {
			p := int(a[0] - '0')
			m.local(id, func(b *Bead) { b.Priority = p })
			return []cmd{func() msg {
				return doneMsg{text: fmt.Sprintf("%s → P%d", id, p), err: m.env.setPriority(id, p), reload: true}
			}}
		}}
	case "+":
		m.ask = &ask{prompt: "child task of " + id + ", title: ", text: true, then: func(title string) []cmd {
			m.ask = confirm(fmt.Sprintf("create task %q under %s (P%d)?", title, id, r.bead.Priority), func() []cmd {
				return []cmd{func() msg {
					child, err := m.env.child(r.bead, title)
					return doneMsg{text: "created " + child + " under " + id, err: err, reload: true}
				}}
			})
			return nil
		}}
	case "C":
		if len(m.marked) > 0 {
			targets, ok := m.boardTargets()
			if !ok {
				return nil
			}
			m.ask = &ask{prompt: "close " + rowIDs(targets) + ", reason: ", text: true, then: func(reason string) []cmd {
				m.ask = confirm(fmt.Sprintf("close %s: %q?", rowIDs(targets), reason), func() []cmd {
					return m.bulkRun("closed", targets, func(t boardRow) (string, error) {
						return "", m.env.close(t.bead.ID, reason)
					})
				})
				return nil
			}}
			return nil
		}
		m.ask = &ask{prompt: "close " + id + ", reason: ", text: true, then: func(reason string) []cmd {
			m.ask = confirm(fmt.Sprintf("close %s: %q?", id, reason), func() []cmd {
				return []cmd{func() msg {
					return doneMsg{text: "closed " + id, err: m.env.close(id, reason), reload: true}
				}}
			})
			return nil
		}}
	case "d":
		targets, ok := m.boardTargets()
		if !ok {
			return nil
		}
		m.ask = confirm("quartermaster resolve "+rowIDs(targets)+" (remove worktree/role dir, never --force)?", func() []cmd {
			if len(targets) == 1 && len(m.marked) == 0 {
				tid := targets[0].bead.ID
				m.status = "resolving " + tid + "…"
				return []cmd{func() msg {
					text, err := m.env.resolve(tid)
					return doneMsg{text: text, err: err, reload: true, reloadAgents: true}
				}}
			}
			return m.bulkRun("resolved", targets, func(t boardRow) (string, error) {
				return m.env.resolve(t.bead.ID)
			})
		})
	case "o":
		if len(r.prs) == 0 {
			m.status = "no PR known"
			return nil
		}
		m.status = m.env.open(r.prs[0].URL)
	case "l":
		m.status = openLinearWith(m.cfg, r, m.env.open)
	case "y":
		m.status = m.copied(id, "copied "+id)
	case "p":
		if r.run != "" {
			m.status = m.copied(r.run, "copied the command")
		}
	case "b":
		m.handoff = func() error { return pageBead(id) }
	}
	return nil
}

func (m *board) copied(text, done string) string {
	if err := m.env.copy(text); err != nil {
		return "wl-copy: " + err.Error()
	}
	return done
}

// boardTargets returns the marked visible rows when any rows are marked on the
// main board, or the currently selected row otherwise. If marks exist but none
// are visible under the current filter, ok is false.
func (m *board) boardTargets() ([]boardRow, bool) {
	if len(m.marked) == 0 {
		if r := m.sel(); r != nil {
			return []boardRow{*r}, true
		}
		return nil, false
	}
	var out []boardRow
	for _, r := range m.rows {
		if r.header == "" && m.marked[r.bead.ID] {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		m.status = "no marked beads match current filter (esc clears filter)"
		return nil, false
	}
	return out, true
}

func filterRowsWithoutAgent(rows []boardRow) []boardRow {
	var out []boardRow
	for _, r := range rows {
		if r.agent == nil {
			out = append(out, r)
		}
	}
	return out
}

func rowIDs(rows []boardRow) string {
	s := make([]string, 0, len(rows))
	for _, r := range rows {
		s = append(s, r.bead.ID)
	}
	if len(s) > 3 {
		return fmt.Sprintf("%s and %d more", strings.Join(s[:3], ", "), len(s)-3)
	}
	return strings.Join(s, ", ")
}

// toggleSectionMarks marks all visible rows in the current section, or clears
// all marks in the current section when any row in it is already marked.
func (m *board) toggleSectionMarks() {
	sec := m.sectionRows()
	if len(sec) == 0 {
		return
	}
	anyMarked := false
	for _, r := range sec {
		if m.marked[r.bead.ID] {
			anyMarked = true
			break
		}
	}
	if anyMarked {
		for _, r := range sec {
			delete(m.marked, r.bead.ID)
		}
		m.status = fmt.Sprintf("unmarked %d in section", len(sec))
		return
	}
	for _, r := range sec {
		m.marked[r.bead.ID] = true
	}
	m.status = fmt.Sprintf("marked %d in section", len(sec))
}

func (m *board) sectionRows() []boardRow {
	if m.cur < 0 || m.cur >= len(m.rows) {
		return nil
	}
	h := m.cur
	for h > 0 && m.rows[h].header == "" {
		h--
	}
	var out []boardRow
	for i := h + 1; i < len(m.rows) && m.rows[i].header == ""; i++ {
		out = append(out, m.rows[i])
	}
	return out
}

// bulkRun applies do to each row in turn on the main board, unmarking each
// acted row and summarizing successes and errors.
func (m *board) bulkRun(done string, rows []boardRow, do func(boardRow) (string, error)) []cmd {
	for _, r := range rows {
		delete(m.marked, r.bead.ID)
	}
	m.status = fmt.Sprintf("%s: working on %d…", done, len(rows))
	return []cmd{func() msg {
		var ok, errs []string
		for _, r := range rows {
			if _, err := do(r); err != nil {
				errs = append(errs, r.bead.ID+": "+err.Error())
				continue
			}
			ok = append(ok, r.bead.ID)
		}
		text := fmt.Sprintf("%s %d", done, len(ok))
		if len(ok) > 0 && len(ok) <= 3 {
			text = done + " " + strings.Join(ok, ", ")
		}
		var err error
		if len(errs) > 0 {
			err = fmt.Errorf("%d failed: %s", len(errs), strings.Join(errs, "; "))
		}
		return doneMsg{text: text, err: err, reload: true, reloadAgents: true}
	}}
}

// dispatchAsk confirms, then starts a worker (or bulk workers) on rows. When
// role is empty and not explicitly chosen, a single bead defaults to any
// role:<name> label on the bead; pressing r on the confirmation lets the user
// pick any built-in or configured [roles.<name>] role before starting.
func (m *board) dispatchAsk(rows []boardRow, kind, role string, roleChosen bool) {
	if len(rows) == 0 {
		return
	}
	single := len(rows) == 1 && len(m.marked) == 0
	effectiveName := role
	if !roleChosen && effectiveName == "" && len(rows) == 1 {
		labelRole, err := beadRole(rows[0].bead)
		if err != nil {
			m.status = err.Error()
			return
		}
		effectiveName = labelRole
	}
	eff, err := m.cfg.resolveRole(effectiveName)
	if err != nil {
		m.status = err.Error()
		return
	}
	who := kind
	if who == "" {
		if eff.Agent != "" {
			who = eff.Agent
		} else {
			who = "an agent"
		}
	}
	dispatchRole := ""
	if roleChosen || (effectiveName != "" && effectiveName != roleImplementer) {
		dispatchRole = eff.Name
	}
	roleNote := ""
	if dispatchRole != "" {
		roleNote = " (role " + dispatchRole + ")"
	}
	var prompt string
	if single {
		where := "in its own worktree"
		if !eff.Worktree {
			where = "in an isolated role dir (no worktree)"
		}
		prompt = fmt.Sprintf("start %s on %s%s %s?  y/n · r role", who, rows[0].bead.ID, roleNote, where)
	} else {
		prompt = fmt.Sprintf("start %s on %s%s?  y/n · r role", who, rowIDs(rows), roleNote)
	}
	m.ask = &ask{prompt: prompt, keys: "yr", then: func(k string) []cmd {
		if k == "r" {
			m.pickRoleAsk("role for "+rowIDs(rows), eff.Name, func(picked string) []cmd {
				m.dispatchAsk(rows, kind, picked, true)
				return nil
			})
			return nil
		}
		if single {
			id := rows[0].bead.ID
			m.status = "starting " + id + "…"
			return []cmd{func() msg {
				text, err := m.env.dispatch(id, kind, dispatchRole, true)
				return doneMsg{text: text, err: err, reload: true, quit: true}
			}}
		}
		return m.bulkRun("started", rows, func(r boardRow) (string, error) {
			return m.env.dispatch(r.bead.ID, kind, dispatchRole, false)
		})
	}}
}

// dispatchRolePick opens the [roles.<name>] role picker directly (D key),
// defaulting to any role:<name> label on the bead, then confirms dispatch.
func (m *board) dispatchRolePick(rows []boardRow, kind string) {
	if len(rows) == 0 {
		return
	}
	defRole := roleImplementer
	if len(rows) == 1 {
		labelRole, err := beadRole(rows[0].bead)
		if err != nil {
			m.status = err.Error()
			return
		}
		if labelRole != "" {
			defRole = labelRole
		}
	}
	m.pickRoleAsk("dispatch "+rowIDs(rows)+" role", defRole, func(picked string) []cmd {
		m.dispatchAsk(rows, kind, picked, true)
		return nil
	})
}

func (m *board) pickRoleAsk(prefix, defRole string, onPick func(role string) []cmd) {
	names := m.cfg.roleNames()
	if defRole == "" {
		defRole = roleImplementer
	}
	var parts []string
	var keys strings.Builder
	keys.WriteString("\r")
	limit := min(len(names), 9)
	for i := 0; i < limit; i++ {
		label := fmt.Sprintf("%d %s", i+1, names[i])
		if names[i] == defRole {
			label += "*"
		}
		parts = append(parts, label)
		keys.WriteByte(byte('1' + i))
	}
	if len(names) > 9 {
		parts = append(parts, "t type")
		keys.WriteByte('t')
	}
	prompt := fmt.Sprintf("%s (↵ %s): %s", prefix, defRole, strings.Join(parts, " · "))
	m.ask = &ask{prompt: prompt, keys: keys.String(), then: func(k string) []cmd {
		if k == "\r" {
			return onPick(defRole)
		}
		if k == "t" {
			m.ask = &ask{prompt: prefix + " name: ", text: true, then: func(raw string) []cmd {
				if _, err := m.cfg.resolveRole(raw); err != nil {
					m.status = err.Error()
					return nil
				}
				return onPick(raw)
			}}
			return nil
		}
		idx := int(k[0] - '1')
		if idx < 0 || idx >= limit {
			return nil
		}
		return onPick(names[idx])
	}}
}

var boardAgentKeys = map[string]string{
	"c": "claude",
	"x": "codex",
	"m": "muse",
	"i": "pi",
	"t": "agy",
}

// resumeAsk confirms resuming one or more exited workers, allowing an optional
// agent kind override via a (picker) or c/x/m/i/t directly on the confirm line.
func (m *board) resumeAsk(rows []boardRow, kind string) {
	if len(rows) == 0 {
		return
	}
	single := len(rows) == 1 && len(m.marked) == 0
	var prompt string
	switch {
	case single && kind == "":
		prompt = fmt.Sprintf("resume %s's last agent conversation in its worktree?  y/n · a agent (c/x/m/i/t)", rows[0].bead.ID)
	case single:
		prompt = fmt.Sprintf("resume %s with %s?  y/n · a agent (d/c/x/m/i/t)", rows[0].bead.ID, kind)
	case kind == "":
		prompt = fmt.Sprintf("resume %s?  y/n · a agent (c/x/m/i/t)", rowIDs(rows))
	default:
		prompt = fmt.Sprintf("resume %s with %s?  y/n · a agent (d/c/x/m/i/t)", rowIDs(rows), kind)
	}
	m.ask = &ask{prompt: prompt, keys: "yadcxmit", then: func(k string) []cmd {
		switch k {
		case "a":
			m.pickResumeAgentAsk(rows)
			return nil
		case "d":
			m.resumeAsk(rows, "")
			return nil
		case "c", "x", "m", "i", "t":
			m.resumeAsk(rows, boardAgentKeys[k])
			return nil
		}
		if single {
			id := rows[0].bead.ID
			m.status = "resuming " + id + "…"
			return []cmd{func() msg {
				text, err := m.env.resume(id, kind)
				return doneMsg{text: text, err: err, reload: true, reloadAgents: true}
			}}
		}
		return m.bulkRun("resumed", rows, func(r boardRow) (string, error) {
			return m.env.resume(r.bead.ID, kind)
		})
	}}
}

func (m *board) pickResumeAgentAsk(rows []boardRow) {
	prompt := fmt.Sprintf("resume %s agent: d default · c claude · x codex · m muse · i pi · t agy", rowIDs(rows))
	m.ask = &ask{prompt: prompt, keys: "dcxmit", then: func(k string) []cmd {
		if k == "d" {
			m.resumeAsk(rows, "")
			return nil
		}
		m.resumeAsk(rows, boardAgentKeys[k])
		return nil
	}}
}

// createAsk drives top-level bead creation (N): title, optional repo label
// (when [repos] is configured), optional role, and create vs create+dispatch.
func (m *board) createAsk() {
	m.ask = &ask{prompt: "new bead title: ", text: true, then: func(title string) []cmd {
		if len(m.cfg.Repos) > 0 {
			m.pickCreateRepo(title)
			return nil
		}
		m.pickCreateRole(title, "")
		return nil
	}}
}

func (m *board) pickCreateRepo(title string) {
	repos := slices.Sorted(maps.Keys(m.cfg.Repos))
	parts := []string{"0 default"}
	var keys strings.Builder
	keys.WriteString("\r0")
	limit := min(len(repos), 9)
	for i := 0; i < limit; i++ {
		parts = append(parts, fmt.Sprintf("%d %s", i+1, repos[i]))
		keys.WriteByte(byte('1' + i))
	}
	if len(repos) > 9 {
		parts = append(parts, "t type")
		keys.WriteByte('t')
	}
	prompt := fmt.Sprintf("repo for %q (↵ default): %s", shortTitle(title, 28), strings.Join(parts, " · "))
	m.ask = &ask{prompt: prompt, keys: keys.String(), then: func(k string) []cmd {
		if k == "\r" || k == "0" {
			m.pickCreateRole(title, "")
			return nil
		}
		if k == "t" {
			m.ask = &ask{prompt: "repo name: ", text: true, then: func(raw string) []cmd {
				if _, ok := m.cfg.Repos[raw]; !ok {
					m.status = fmt.Sprintf("unknown repo %q", raw)
					return nil
				}
				m.pickCreateRole(title, raw)
				return nil
			}}
			return nil
		}
		idx := int(k[0] - '1')
		if idx < 0 || idx >= limit {
			return nil
		}
		m.pickCreateRole(title, repos[idx])
		return nil
	}}
}

func (m *board) pickCreateRole(title, repo string) {
	m.pickRoleAsk(fmt.Sprintf("role for %q", shortTitle(title, 28)), roleImplementer, func(role string) []cmd {
		if role == roleImplementer {
			role = ""
		}
		m.confirmCreate(title, repo, role)
		return nil
	})
}

func (m *board) confirmCreate(title, repo, role string) {
	var tags []string
	if repo != "" {
		tags = append(tags, "repo:"+repo)
	}
	if role != "" {
		tags = append(tags, "role:"+role)
	}
	suffix := ""
	if len(tags) > 0 {
		suffix = " [" + strings.Join(tags, ", ") + "]"
	}
	prompt := fmt.Sprintf("create %q%s?  y create · d create+dispatch · n cancel", shortTitle(title, 36), suffix)
	m.ask = &ask{prompt: prompt, keys: "yd", then: func(k string) []cmd {
		if m.env.create == nil {
			m.status = "bead creation unavailable"
			return nil
		}
		dispatchNow := k == "d"
		m.status = "creating bead…"
		return []cmd{func() msg {
			id, err := m.env.create(title, repo, role)
			if err != nil {
				return doneMsg{err: err}
			}
			if !dispatchNow {
				return doneMsg{text: "created " + id, reload: true}
			}
			text, derr := m.env.dispatch(id, "", role, true)
			if derr != nil {
				return doneMsg{text: "created " + id, err: derr, reload: true}
			}
			return doneMsg{text: "created " + id + "; " + text, reload: true, quit: true}
		}}
	}}
}

// askCoordinatorBar opens an input bar (@) that sends a free-form natural-language
// prompt to the running coordinator session/pane without leaving the board.
func (m *board) askCoordinatorBar() {
	m.ask = &ask{prompt: "@coordinator: ", text: true, then: func(text string) []cmd {
		if m.env.askCoordinator == nil {
			m.status = "coordinator prompt unavailable"
			return nil
		}
		m.status = "sending to coordinator…"
		return []cmd{func() msg {
			res, err := m.env.askCoordinator(text)
			return doneMsg{text: res, err: err}
		}}
	}}
}

// answer prompts the bead's worker with the user's reply. Only an idle (or
// done) agent is prompted with free text: a working one is mid-turn, and a
// blocked one at a tool-approval prompt goes through openAnswer's hash-verified
// option path instead of raw pane text.
func (m *board) answer(r boardRow) {
	id := r.bead.ID
	if why := unanswerable(r.agent); why != "" {
		m.status = id + ": " + why
		return
	}
	name := r.agent.Name
	m.ask = &ask{prompt: "answer " + name + ": ", text: true, then: func(text string) []cmd {
		m.ask = confirm(fmt.Sprintf("send to %s: %q?", name, shortTitle(text, 50)), func() []cmd {
			return []cmd{func() msg {
				// The agent may have moved on while the answer was typed.
				agents, err := m.env.agents()
				if err != nil {
					return doneMsg{err: err}
				}
				var now *Agent
				for i := range agents {
					if agents[i].Name == name {
						now = &agents[i]
					}
				}
				if why := unanswerable(now); why != "" {
					return doneMsg{err: fmt.Errorf("not sent: %s", why)}
				}
				return doneMsg{text: "sent to " + name, err: m.env.prompt(name, text)}
			}}
		})
		return nil
	}}
}

// unanswerable is why an agent can't be prompted now, "" when it can.
func unanswerable(a *Agent) string {
	switch {
	case a == nil:
		return "no agent: start one (enter) or resume it (R)"
	case a.Status == "blocked":
		return a.Name + " is at an approval prompt; answer it in its pane (enter focuses)"
	case !agentReady(a):
		return a.Name + " is " + a.Status + "; answer once it stops"
	}
	return ""
}

// local changes the bead in place so the board shows an action at once; the
// reload that follows replaces it with what bd has.
func (m *board) local(id string, f func(*Bead)) {
	for i := range m.data.Beads {
		if m.data.Beads[i].ID == id {
			f(&m.data.Beads[i])
		}
	}
	m.rebuild()
}

func without(list []string, x string) []string {
	var out []string
	for _, s := range list {
		if s != x {
			out = append(out, s)
		}
	}
	return out
}

// gcTargets is the marked items, or the selected one when none are marked.
func (m *board) gcTargets() []gcItem {
	var out []gcItem
	for _, it := range m.gc {
		if m.gcMarked[it.bead.ID] {
			out = append(out, it)
		}
	}
	if len(out) == 0 && m.gcCur < len(m.gc) {
		out = append(out, m.gc[m.gcCur])
	}
	return out
}

func (m *board) gcKey(k string) []cmd {
	switch k {
	case "q", "\x1b", "T":
		m.view = viewList
	case "?":
		m.back, m.view = viewGC, viewHelp
	case "j", "\x1b[B":
		m.gcCur = min(m.gcCur+1, max(0, len(m.gc)-1))
	case "k", "\x1b[A":
		m.gcCur = max(m.gcCur-1, 0)
	case "g":
		m.gcCur = 0
	case "G":
		m.gcCur = max(0, len(m.gc)-1)
	case " ":
		if m.gcCur < len(m.gc) {
			id := m.gc[m.gcCur].bead.ID
			if m.gcMarked[id] {
				delete(m.gcMarked, id)
			} else {
				m.gcMarked[id] = true
			}
			m.gcCur = min(m.gcCur+1, max(0, len(m.gc)-1))
		}
	case "*":
		if len(m.gcMarked) > 0 {
			m.gcMarked = map[string]bool{}
		} else {
			for _, it := range m.gc {
				m.gcMarked[it.bead.ID] = true
			}
		}
	case "r":
		m.gcLoaded = false
		return m.tick()
	case "\r":
		if m.gcCur < len(m.gc) && m.gc[m.gcCur].agent != "" {
			name := m.gc[m.gcCur].agent
			return []cmd{func() msg { return doneMsg{err: m.env.focus(name), quit: true} }}
		}
	case "C":
		items := pick(m.gcTargets(), func(it gcItem) bool { return it.canClose })
		if m.none(items, "close") {
			return nil
		}
		m.ask = &ask{prompt: "close " + itemIDs(items) + ", reason: ", text: true, then: func(reason string) []cmd {
			m.ask = confirm(fmt.Sprintf("close %s: %q?", itemIDs(items), reason), func() []cmd {
				return m.gcRun("closed", items, func(it gcItem) (string, error) { return "", m.env.close(it.bead.ID, reason) })
			})
			return nil
		}}
	case "u":
		items := pick(m.gcTargets(), func(it gcItem) bool { return it.canRelease })
		if m.none(items, "release") {
			return nil
		}
		now := m.now()
		m.ask = confirm("release "+itemIDs(items)+" back to open (a note, then status open)?", func() []cmd {
			return m.gcRun("released", items, func(it gcItem) (string, error) {
				return "", m.env.release(it.bead, daysSince(it.bead.UpdatedAt, now))
			})
		})
	case "d":
		items := pick(m.gcTargets(), func(it gcItem) bool { return it.canResolve })
		if m.none(items, "resolve") {
			return nil
		}
		m.ask = confirm("quartermaster resolve "+itemIDs(items)+" (remove the worktree, and the branch once merged)?", func() []cmd {
			return m.gcRun("resolved", items, func(it gcItem) (string, error) { return m.env.resolve(it.bead.ID) })
		})
	case "i":
		items := pick(m.gcTargets(), func(it gcItem) bool { return it.canDismiss })
		if m.none(items, "dismiss") {
			return nil
		}
		return m.gcRun("dismissed", items, func(it gcItem) (string, error) { return "", m.env.inboxDone(it.inboxFiles) })
	}
	return nil
}

func (m *board) none(items []gcItem, what string) bool {
	if len(items) > 0 {
		return false
	}
	m.status = "nothing to " + what + " in the selection"
	return true
}

// gcRun applies do to each item in turn (bd serializes them anyway) and
// reports what it did and what failed.
func (m *board) gcRun(done string, items []gcItem, do func(gcItem) (string, error)) []cmd {
	for _, it := range items {
		delete(m.gcMarked, it.bead.ID)
	}
	m.status = fmt.Sprintf("%s: working on %d…", done, len(items))
	return []cmd{func() msg {
		var ok, errs []string
		for _, it := range items {
			if _, err := do(it); err != nil {
				errs = append(errs, it.bead.ID+": "+err.Error())
				continue
			}
			ok = append(ok, it.bead.ID)
		}
		text := fmt.Sprintf("%s %d", done, len(ok))
		if len(ok) > 0 && len(ok) <= 3 {
			text = done + " " + strings.Join(ok, ", ")
		}
		var err error
		if len(errs) > 0 {
			err = fmt.Errorf("%d failed: %s", len(errs), strings.Join(errs, "; "))
		}
		return doneMsg{text: text, err: err, reload: true, gc: true}
	}}
}

func pick(items []gcItem, ok func(gcItem) bool) []gcItem {
	var out []gcItem
	for _, it := range items {
		if ok(it) {
			out = append(out, it)
		}
	}
	return out
}

func itemIDs(items []gcItem) string {
	var s []string
	for _, it := range items {
		s = append(s, it.bead.ID)
	}
	if len(s) > 3 {
		return fmt.Sprintf("%s and %d more", strings.Join(s[:3], ", "), len(s)-3)
	}
	return strings.Join(s, ", ")
}

// filterRows keeps the rows whose search text contains every word of query,
// case-insensitively, and the section headers that still have a row under them.
func filterRows(rows []boardRow, query string) []boardRow {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return rows
	}
	var out []boardRow
	var header *boardRow
	for _, r := range rows {
		if r.header != "" {
			header = &r
			continue
		}
		if !matchesAll(r.search, words) {
			continue
		}
		if header != nil {
			out = append(out, *header)
			header = nil
		}
		out = append(out, r)
	}
	return out
}

func matchesAll(s string, words []string) bool {
	for _, w := range words {
		if !strings.Contains(s, w) {
			return false
		}
	}
	return true
}

// boardFilter is the '/' prompt: typing is true while keys edit the query
// rather than drive the board.
type boardFilter struct {
	query  string
	typing bool
}

// key applies one keypress while typing and reports whether the query changed.
// Enter keeps the query and returns to navigation; Esc clears it.
func (f *boardFilter) key(k string) bool {
	before := f.query
	submit, cancel := edit(&f.query, k)
	switch {
	case submit:
		f.typing = false
	case cancel:
		f.typing = false
		f.query = ""
		return true
	}
	return f.query != before
}

// splitKeys cuts one read's bytes into keypresses: a read can hold several
// (a paste, fast typing) or end inside a UTF-8 rune, which comes back as rest
// to prefix the next read. Escape sequences (arrows) stay whole; a lone ESC
// is the Esc key.
func splitKeys(b []byte) (keys []string, rest []byte) {
	for len(b) > 0 {
		n := 1
		switch {
		case b[0] == 0x1b && len(b) >= 2 && (b[1] == '[' || b[1] == 'O'):
			n = 2
			if b[1] == '[' {
				for n < len(b) && (b[n] < 0x40 || b[n] > 0x7e) {
					n++
				}
			}
			n = min(n+1, len(b))
		case b[0] >= utf8.RuneSelf:
			if !utf8.FullRune(b) {
				return keys, b
			}
			_, n = utf8.DecodeRune(b)
		}
		keys = append(keys, string(b[:n]))
		b = b[n:]
	}
	return keys, nil
}

func nextSelectable(rows []boardRow, from, dir int) int {
	for i := from; i >= 0 && i < len(rows); i += dir {
		if rows[i].header == "" {
			return i
		}
	}
	return -1
}

// waitInput waits up to timeout ms (-1: forever) for a key on fd. Kept for
// callers like rpcview that only poll stdin.
func waitInput(fd, timeout int) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, timeout)
		if err == unix.EINTR {
			continue
		}
		return n > 0, err
	}
}

// waitInputOrWake waits up to timeout ms (-1: forever) for input on stdinFD or
// an async/SIGWINCH wake byte on wakeFD. Polling rather than a reader goroutine
// leaves stdin alone while a pager has it.
func waitInputOrWake(stdinFD, wakeFD, timeout int) (stdinReady, woke bool, err error) {
	fds := []unix.PollFd{{Fd: int32(stdinFD), Events: unix.POLLIN}}
	if wakeFD >= 0 {
		fds = append(fds, unix.PollFd{Fd: int32(wakeFD), Events: unix.POLLIN})
	}
	for {
		n, err := unix.Poll(fds, timeout)
		if err == unix.EINTR {
			return false, true, nil
		}
		if err != nil {
			return false, false, err
		}
		if n == 0 {
			return false, false, nil
		}
		return fds[0].Revents&unix.POLLIN != 0, len(fds) > 1 && fds[1].Revents&unix.POLLIN != 0, nil
	}
}

// boardRunOpts configures the initial mode of `quartermaster board`.
type boardRunOpts struct {
	Queue bool
}

// startBoard is the board's first state: beads from the last board's cache,
// so the first paint waits on nothing slow, and agents and ticker state read
// now, since both are cheap. The first tick loads beads from bd.
func startBoard(cfg Config, env boardEnv, cache string, now func() time.Time) *board {
	beads, at := loadBoardCache(cache)
	st, inbox := env.local()
	var sn SnoozeState
	if env.snoozes != nil {
		sn = env.snoozes()
	}
	m := newBoard(cfg, env, boardData{Beads: beads, BeadsAt: at, State: st, Inbox: inbox, Snooze: sn}, now)
	m.localAt = now()
	if agents, err := env.agents(); err == nil {
		m.data.Agents = agents
		m.agentsAt = now()
		m.rebuild()
	}
	return m
}

const (
	boardEnterScreen = "\x1b[?1049h\x1b[?25l\x1b[?1000h\x1b[?1006h"
	boardLeaveScreen = "\x1b[?1006l\x1b[?1000l\x1b[?25h\x1b[?1049l"
)

func runBoard(cfg Config) error {
	return runBoardWithOpts(cfg, boardRunOpts{})
}

func runBoardWithOpts(cfg Config, opts boardRunOpts) error {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, old)
	fmt.Print(boardEnterScreen)
	defer fmt.Print(boardLeaveScreen)

	// Self-pipe wakes unix.Poll instantaneously on SIGWINCH resize or async
	// command completion, eliminating 1s redraw stalls on Moshi/Ghostty.
	wakeR, wakeW, pipeErr := os.Pipe()
	wakeFD := -1
	if pipeErr == nil {
		_ = unix.SetNonblock(int(wakeR.Fd()), true)
		_ = unix.SetNonblock(int(wakeW.Fd()), true)
		wakeFD = int(wakeR.Fd())
		defer wakeR.Close()
		defer wakeW.Close()
	}
	wake := func() {
		if wakeW != nil {
			_, _ = wakeW.Write([]byte{1})
		}
	}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	stopWinch := make(chan struct{})
	go func() {
		for {
			select {
			case <-winch:
				wake()
			case <-stopWinch:
				return
			}
		}
	}()
	defer func() {
		signal.Stop(winch)
		close(stopWinch)
	}()

	// The same server the ticker reads, so focus and dispatch act on the agents shown.
	h := newHerdr().on(cfg.coordSocket())
	m := startBoard(cfg, liveBoardEnv(cfg, h), boardCachePath(), time.Now)
	if opts.Queue {
		m.queueHome = true
		m.view = viewQueue
		m.rebuildQueue()
	}

	results := make(chan msg, 64)
	start := func(cmds []cmd) {
		for _, c := range cmds {
			if c != nil {
				go func() {
					results <- c()
					wake()
				}()
			}
		}
	}
	buf := make([]byte, 256)
	var drainBuf [64]byte
	var partial []byte
	for !m.quit {
		start(m.tick())
		w, hgt, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil {
			w, hgt = 100, 30
		}
		fmt.Print(renderBoard(m, w, hgt))
		// Wake immediately on input, resize (SIGWINCH), or async completion,
		// and at least each second so ages tick and refreshes come due.
		ready, woke, err := waitInputOrWake(fd, wakeFD, 1000)
		if err != nil {
			return err
		}
		if woke && wakeR != nil {
			for {
				if n, _ := wakeR.Read(drainBuf[:]); n <= 0 {
					break
				}
			}
		}
		for drained := false; !drained; {
			select {
			case x := <-results:
				start(m.apply(x))
			default:
				drained = true
			}
		}
		if !ready {
			continue
		}
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return err
		}
		var keys []string
		keys, partial = splitKeys(append(partial, buf[:n]...))
		for _, k := range keys {
			start(m.key(k))
			if f := m.handoff; f != nil {
				m.handoff = nil
				fmt.Print(boardLeaveScreen)
				term.Restore(fd, old)
				if err := f(); err != nil {
					m.status = err.Error()
				}
				if _, err := term.MakeRaw(fd); err != nil {
					return err
				}
				fmt.Print(boardEnterScreen)
				// Keys buffered beside the viewer/pager entry key belong to
				// that transition, not to the board after it returns. Never
				// let a pasted Vn become a task-label write on viewer close.
				partial = nil
				break
			}
			if m.quit {
				break
			}
		}
	}
	return nil
}

// openLinear opens the row's Linear issue in the browser.
func openLinear(cfg Config, r boardRow) string {
	if r.linear == "" {
		return "no Linear issue known"
	}
	u := linearURL(r.linear, r.bead.Notes, cfg.LinearWorkspace)
	if u == "" {
		return "set linear_workspace in config.toml to open " + r.linear
	}
	return openURL(u)
}

// statusFor is the status line: the last action's message, else the command
// the selected thread is waiting on the user to run.
func statusFor(rows []boardRow, cur int, status string) string {
	if status != "" || cur < 0 || cur >= len(rows) || rows[cur].run == "" {
		return status
	}
	return "run: " + rows[cur].run + "  (p copies)"
}

// fit pads or cuts s to exactly n characters (runes, not bytes: the state
// lines carry "·" and "…").
func fit(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) > n {
		if n == 1 {
			return "…"
		}
		return string(r[:n-1]) + "…"
	}
	return s + strings.Repeat(" ", n-len(r))
}
