package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Which agents have to do with which pull request. A space's branch has one
// PR, but an agent can open several, from branches the space was never on,
// and more than one agent can work on a PR. Claude Code notes in a session's
// transcript the PRs it works on, and which of them it created; herdr says
// which session each pane runs. So on each tick the new lines of the recent
// transcripts are read, and each PR remembered with its sessions: the sidebar
// lists an agent's PRs, its branch's and the ones it opened ($prs), and the
// popup lists a PR's agents and takes you to them. One that's merged or
// closed leaves $prs for $prs_past, which isn't for showing: it's there so
// the agent can still be found by a PR it's done with.
//
// Only Claude Code keeps such notes; other agents have their branch's PR.

const (
	// prsShown is how many PRs $prs spells out before it counts the rest.
	prsShown = 3
	// pastKept is how long a PR is remembered after it's merged or closed,
	// pastShown how many of them $prs_past names, and pastFresh how often
	// GitHub is asked whether one is still shut.
	pastKept  = 14 * 24 * time.Hour
	pastShown = 12
	pastFresh = 6 * time.Hour
	// transcriptLineLimit skips a line too long to be one of the notes (a
	// pasted image, say) without holding it in memory.
	transcriptLineLimit = 4 << 20
	// transcriptWindow is how far back the first sweep looks for sessions.
	transcriptWindow = 30 * 24 * time.Hour
	// transcriptQuiet: a transcript untouched this long has its place
	// forgotten. If it's written to again it's read from the start.
	transcriptQuiet = 24 * time.Hour
)

// trackedPR is a pull request that Claude sessions have worked on: open, or
// not long merged or closed.
type trackedPR struct {
	Host    string            `json:"host"`
	Owner   string            `json:"owner"`
	Name    string            `json:"name"`
	Number  int               `json:"number"`
	Fetched time.Time         `json:"fetched"`
	Status  *prStatus         `json:"status,omitempty"` // nil: GitHub hasn't said yet
	By      map[string]prPart `json:"by"`               // session → what it had to do with it
}

// prPart is one session's part in a PR.
type prPart struct {
	Opened time.Time `json:"opened"` // when it created the PR; zero if it didn't
	Last   time.Time `json:"last"`   // when it last worked on it
}

// sessionSeen is what's known of a Claude session that worked on a PR.
type sessionSeen struct {
	Title    string `json:"title,omitempty"`    // its own name for itself
	Cwd      string `json:"cwd,omitempty"`      // where it runs, to resume it
	Pane     string `json:"pane,omitempty"`     // the pane it's in; "" when it isn't in one
	Terminal string `json:"terminal,omitempty"` // that pane's terminal: a pane ID can come round again
}

func (t trackedPR) repo() repoRef { return repoRef{Host: t.Host, Owner: t.Owner, Name: t.Name} }

// openedKey is how a tracked PR is looked up: pullRequest.key(), lowercased.
func openedKey(r repoRef, number int) string { return r.key() + "#" + strconv.Itoa(number) }

func (t trackedPR) key() string { return openedKey(t.repo(), t.Number) }

func (t trackedPR) open() bool { return t.Status != nil && t.Status.State == "OPEN" }

// past: it's been merged or closed.
func (t trackedPR) past() bool { return t.Status != nil && t.Status.State != "OPEN" }

// expired: it was merged or closed too long ago to remember (or GitHub
// didn't say when).
func (t trackedPR) expired(now time.Time) bool {
	return t.past() && (t.Status.ClosedAt == nil || now.Sub(*t.Status.ClosedAt) > pastKept)
}

// opener is the session that opened the PR, "" if none here did. Two can
// claim to (one did, another ran a command that looked like it): the earlier.
func (t trackedPR) opener() string {
	best := ""
	for s, part := range t.By {
		if part.Opened.IsZero() {
			continue
		}
		if o := t.By[best].Opened; best == "" || part.Opened.Before(o) || (part.Opened.Equal(o) && s < best) {
			best = s
		}
	}
	return best
}

// sessionID is what a Claude session ID looks like. It goes into a command.
var sessionID = regexp.MustCompile(`^[0-9a-zA-Z][0-9a-zA-Z-]{7,63}$`)

// claudeDir is where Claude Code keeps its sessions.
func claudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	return expandHome("~/.claude")
}

// transcript is one file of a session: its own, or that of a subagent it
// started, which can open a PR on its behalf.
type transcript struct {
	path    string
	session string
	sub     bool
}

func transcripts() []transcript {
	var out []transcript
	own, _ := filepath.Glob(filepath.Join(claudeDir(), "projects", "*", "*.jsonl"))
	for _, p := range own {
		out = append(out, transcript{path: p, session: strings.TrimSuffix(filepath.Base(p), ".jsonl")})
	}
	subs, _ := filepath.Glob(filepath.Join(claudeDir(), "projects", "*", "*", "subagents", "*.jsonl"))
	for _, p := range subs {
		out = append(out, transcript{path: p, session: filepath.Base(filepath.Dir(filepath.Dir(p))), sub: true})
	}
	return out
}

// note is what one transcript line says that's of use here.
type note struct {
	repo   repoRef // a PR the session worked on, if number isn't 0
	number int
	opened bool // … and created
	at     time.Time
	title  string // the session's name for itself
}

// The marks of the lines that are decoded; the rest (nearly all of a
// transcript) aren't.
var (
	gitOpMark, createdMark = []byte(`"gitOperation"`), []byte(`"created"`)
	linkMark, titleMark    = []byte(`"type":"pr-link"`), []byte(`"type":"ai-title"`)
	cwdMark                = []byte(`"cwd":"`)
)

// parseLine reads one transcript line: Claude Code linking the session to a
// PR, a command's result on which it noted a PR it created, or the session's
// title. PRs on a GitHub host we don't know are passed over.
func parseLine(cfg config, line []byte) (note, bool) {
	created := bytes.Contains(line, gitOpMark) && bytes.Contains(line, createdMark)
	if !created && !bytes.Contains(line, linkMark) && !bytes.Contains(line, titleMark) {
		return note{}, false
	}
	var l struct {
		Type      string    `json:"type"`
		Timestamp time.Time `json:"timestamp"`
		Title     string    `json:"aiTitle"`
		Number    int       `json:"prNumber"`
		URL       string    `json:"prUrl"`
		Result    struct {
			GitOperation struct {
				PR *struct {
					Number int    `json:"number"`
					URL    string `json:"url"`
					Action string `json:"action"`
				} `json:"pr"`
			} `json:"gitOperation"`
		} `json:"toolUseResult"`
	}
	// A result that isn't an object (a string, for some tools) fails here.
	if err := json.Unmarshal(line, &l); err != nil {
		return note{}, false
	}
	n := note{at: l.Timestamp}
	switch pr := l.Result.GitOperation.PR; {
	case l.Type == "ai-title":
		n.title = strings.TrimSpace(clean(l.Title, false))
		return n, n.title != ""
	case l.Type == "pr-link":
		n.repo, n.number = prFromURL(cfg, l.URL, l.Number)
	case pr != nil && pr.Action == "created":
		n.repo, n.number = prFromURL(cfg, pr.URL, pr.Number)
		n.opened = true
	}
	return n, n.number > 0
}

// prFromURL is the PR the forge knows at that URL, when the host is one of
// ours and the number is the one given. Number 0: it isn't.
func prFromURL(cfg config, raw string, number int) (repoRef, int) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !cfg.knownHost(u.Hostname()) || number <= 0 {
		return repoRef{}, 0
	}
	r, n, ok := theForge.prFromPath(u.Hostname(), u.Path, number)
	if !ok {
		return repoRef{}, 0
	}
	return r, n
}

// lineCwd is the working directory a transcript line was written in.
func lineCwd(line []byte) string {
	i := bytes.Index(line, cwdMark)
	if i < 0 {
		return ""
	}
	var cwd string
	if json.NewDecoder(bytes.NewReader(line[i+len(cwdMark)-1:])).Decode(&cwd) != nil {
		return ""
	}
	return cwd
}

// scanLines reads path from byte offset from, calls each for every whole
// line, and returns where to start next time: after the last whole line (the
// one being written is left for later).
func scanLines(path string, from int64, each func(line []byte)) int64 {
	f, err := os.Open(path)
	if err != nil {
		return from
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil {
		return from
	} else if info.Size() < from {
		from = 0 // rewritten: read it again
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return from
	}
	r := bufio.NewReaderSize(f, 1<<16)
	var line []byte
	skip := false
	for {
		chunk, err := r.ReadSlice('\n')
		from += int64(len(chunk))
		if !skip {
			line = append(line, chunk...)
			skip = len(line) > transcriptLineLimit
		}
		switch err {
		case nil:
			if !skip {
				each(line)
			}
			line, skip = line[:0], false
		case bufio.ErrBufferFull:
			// A long line: more of it follows.
		default:
			// The end, mid-line or not: what's unfinished is read next time.
			return from - int64(len(line))
		}
		if skip {
			line = line[:0]
		}
	}
}

// name is what you'd call a pane: the name you gave it, else its title.
func (p paneInfo) name() string {
	for _, s := range []string{p.Label, p.Title, p.TerminalTitle} {
		if s = strings.TrimSpace(clean(s, false)); s != "" {
			return s
		}
	}
	return p.PaneID
}

func (p paneInfo) session() string {
	if p.AgentSession == nil || p.Agent != "claude" || p.AgentSession.Kind != "id" {
		return ""
	}
	return p.AgentSession.Value
}

// trackSessions reads what Claude's sessions have noted since last time, and
// works out which pane each session that worked on a PR is in now.
func (st *labelState) trackSessions(cfg config, panes []paneInfo, now time.Time) {
	// What's new: a transcript written to since the last sweep, or one whose
	// place we kept. The first sweep goes back transcriptWindow.
	since := st.Swept
	if since.IsZero() {
		// Places kept by a version that noted less are no use: start over.
		since, st.Scanned = now.Add(-transcriptWindow), nil
	}
	scanned := map[string]int64{}
	for _, t := range transcripts() {
		info, err := os.Stat(t.path)
		if err != nil || !sessionID.MatchString(t.session) {
			continue
		}
		from, known := st.Scanned[t.path]
		if !known && info.ModTime().Before(since) {
			continue
		}
		if info.Size() != from {
			from = scanLines(t.path, from, func(line []byte) { st.note(cfg, t, line) })
		}
		if now.Sub(info.ModTime()) < transcriptQuiet {
			scanned[t.path] = from
		}
	}
	st.Scanned, st.Swept = scanned, now

	bySession, byPane := map[string]paneInfo{}, map[string]paneInfo{}
	for _, p := range panes {
		byPane[p.PaneID] = p
		if s := p.session(); s != "" {
			bySession[s] = p
		}
	}
	// Only the sessions of a PR are kept.
	sessions := map[string]sessionSeen{}
	for _, pr := range st.PRs {
		for s := range pr.By {
			ss := st.Sessions[s]
			if p, ok := bySession[s]; ok {
				ss.Pane, ss.Terminal = p.PaneID, p.TerminalID
				if ss.Title == "" {
					ss.Title = p.name()
				}
			} else if p, ok := byPane[ss.Pane]; !ok || p.TerminalID != ss.Terminal {
				ss.Pane, ss.Terminal = "", ""
			} // else the same pane, on a new session (after /clear): still its
			sessions[s] = ss
		}
	}
	st.Sessions = sessions
}

// note takes in one line of a session's transcript.
func (st *labelState) note(cfg config, t transcript, line []byte) {
	ss := st.Sessions[t.session]
	// A subagent can run somewhere else, and under another name.
	if ss.Cwd == "" && !t.sub {
		if ss.Cwd = lineCwd(line); ss.Cwd != "" {
			st.Sessions[t.session] = ss
		}
	}
	n, ok := parseLine(cfg, line)
	if !ok {
		return
	}
	if n.title != "" {
		if !t.sub {
			ss.Title = n.title
			st.Sessions[t.session] = ss
		}
		return
	}
	key := openedKey(n.repo, n.number)
	pr, ok := st.PRs[key]
	if !ok {
		pr = trackedPR{Host: n.repo.Host, Owner: n.repo.Owner, Name: n.repo.Name, Number: n.number}
	}
	if pr.By == nil {
		pr.By = map[string]prPart{}
	}
	part := pr.By[t.session]
	if n.opened && (part.Opened.IsZero() || n.at.Before(part.Opened)) {
		part.Opened = n.at
	}
	if n.at.After(part.Last) {
		part.Last = n.at
	}
	pr.By[t.session] = part
	st.PRs[key] = pr
}

// numberLookupFn is GitHub, a seam for tests.
var numberLookupFn = func(ctx context.Context, r repoRef, numbers []int) (map[int]prStatus, error) {
	return newClient(r.Host).prsByNumber(ctx, r, numbers)
}

// refreshTracked asks GitHub how the tracked PRs are doing, a repo at a
// time, for those whose answer is due. One that's merged or closed is kept
// for pastKept (its agent can still be found by it), asked about only now
// and then in case it was reopened, and after that forgotten.
func (st *labelState) refreshTracked(ctx context.Context, cfg config, now time.Time, force bool) error {
	fresh := time.Duration(cfg.LabelRefreshSeconds) * time.Second
	due := map[string][]trackedPR{} // repo key → PRs
	for key, pr := range st.PRs {
		if pr.expired(now) {
			delete(st.PRs, key)
			continue
		}
		rk := pr.repo().key()
		if failed, ok := st.Failed[rk]; ok && !force && now.Sub(failed) < fresh {
			continue
		}
		switch {
		case pr.past():
			if now.Sub(pr.Fetched) >= pastFresh {
				due[rk] = append(due[rk], pr)
			}
		case force || pr.Status == nil || now.Sub(pr.Fetched) >= fresh:
			due[rk] = append(due[rk], pr)
		}
	}
	var lookupErr error
	for rk, list := range due {
		for i := 0; i < len(list); i += branchesPerQ {
			batch := list[i:min(len(list), i+branchesPerQ)]
			numbers := make([]int, len(batch))
			for j, pr := range batch {
				numbers[j] = pr.Number
			}
			found, err := numberLookupFn(ctx, batch[0].repo(), numbers)
			if err != nil {
				// Signed out or offline: keep what we knew.
				lookupErr = err
				st.Failed[rk] = now
				continue
			}
			delete(st.Failed, rk)
			for _, pr := range batch {
				s, ok := found[pr.Number]
				pr.Fetched, pr.Status = now, &s
				if !ok || pr.expired(now) {
					delete(st.PRs, pr.key()) // gone, or merged or closed long ago
					continue
				}
				st.PRs[pr.key()] = pr
			}
		}
	}
	return lookupErr
}

// openByPane is each pane's open PRs: the ones its agent opened.
func (st *labelState) openByPane() map[string][]trackedPR {
	out := map[string][]trackedPR{}
	for _, pr := range st.PRs {
		if pane := st.Sessions[pr.opener()].Pane; pane != "" && pr.open() {
			out[pane] = append(out[pane], pr)
		}
	}
	return out
}

// pastByPane is each pane's PRs that are merged or closed: the ones its
// agent opened.
func (st *labelState) pastByPane() map[string][]trackedPR {
	out := map[string][]trackedPR{}
	for _, pr := range st.PRs {
		if pane := st.Sessions[pr.opener()].Pane; pane != "" && pr.past() {
			out[pane] = append(out[pane], pr)
		}
	}
	return out
}

// pastToken is $prs_past: the merged and closed PRs opened there, newest
// first, by number alone ("#1155 #1149"). It's to find an agent by, not to
// show, so it says nothing of their state: a filter for "merged" shouldn't
// find an agent that has since moved on. The branch's PR, which $prs has
// whatever its state, is left out.
func pastToken(branchKey string, past []trackedPR) string {
	sort.Slice(past, func(i, j int) bool { return past[i].Number > past[j].Number })
	var parts []string
	seen := map[string]bool{branchKey: true}
	for _, pr := range past {
		if !seen[pr.key()] && len(parts) < pastShown {
			seen[pr.key()] = true
			parts = append(parts, fmt.Sprintf("#%d", pr.Number))
		}
	}
	return strings.Join(parts, " ")
}

// prsToken is $prs, every PR that's an agent's (or a space's) in one list:
// its branch's first, as the badge has it, then the open ones opened there,
// newest first, each with its checks: "#1112 draft ✓ #1103 ● #1101 ✓". A PR
// that's both is there once, and those past the first few are only counted.
// branchKey is the branch's PR's openedKey.
func prsToken(branch *prStatus, branchKey string, opened []trackedPR) string {
	var parts []string
	if branch != nil {
		parts = append(parts, badgeTokens(branch)["pr_badge"])
	}
	sort.Slice(opened, func(i, j int) bool { return opened[i].Number > opened[j].Number })
	for _, pr := range opened {
		if branch != nil && pr.key() == branchKey {
			continue
		}
		part := fmt.Sprintf("#%d", pr.Number)
		if g := checkGlyph(pr.Status.checks()); g != "" {
			part += " " + g
		}
		parts = append(parts, part)
	}
	if more := len(parts) - prsShown; more > 0 {
		parts = append(parts[:prsShown], fmt.Sprintf("+%d", more))
	}
	return strings.Join(parts, " ")
}

// panePRs are the PRs of the agent in a pane, by openedKey, the likeliest
// first: its branch's (its space's, for a pane that isn't an agent's), then
// the open ones it opened, newest first, then the ones it worked on, the
// latest first.
func panePRs(st labelState, paneID, workspaceID string) []string {
	var keys []string
	seen := map[string]bool{}
	add := func(key string) {
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	branch := st.Reported[paneID].Branch
	if branch == "" {
		branch = st.Reported[workspaceID].Branch
	}
	if e := st.Branches[branch]; branch != "" && e.PR != nil {
		repo, _, _ := strings.Cut(branch, "\x00")
		add(repo + "#" + strconv.Itoa(e.PR.Number))
	}
	if paneID == "" {
		return keys
	}
	opened := st.openByPane()[paneID]
	sort.Slice(opened, func(i, j int) bool { return opened[i].Number > opened[j].Number })
	for _, pr := range opened {
		add(pr.key())
	}
	type worked struct {
		key  string
		last time.Time
	}
	var rest []worked
	for key, pr := range st.PRs {
		for s, part := range pr.By {
			if st.Sessions[s].Pane == paneID && pr.open() {
				rest = append(rest, worked{key, part.Last})
			}
		}
	}
	sort.Slice(rest, func(i, j int) bool {
		if !rest[i].last.Equal(rest[j].last) {
			return rest[i].last.After(rest[j].last)
		}
		return rest[i].key < rest[j].key
	})
	for _, w := range rest {
		add(w.key)
	}
	return keys
}

// What an agent had to do with a PR, the most telling first.
const (
	partOpened = "opened it"
	partBranch = "on its branch"
	partWorked = "worked on it"
)

// prAgent is one of a PR's agents, as the popup shows it.
type prAgent struct {
	Pane    string // "": it isn't in a pane (any more)
	Name    string
	Status  string // the agent's state, when it's in a pane
	Session string // its Claude session, to resume it; "" for another kind of agent
	Cwd     string
	Part    string
	last    time.Time
}

// resume is the command that brings a session with no pane back.
func (a prAgent) resume() string {
	cmd := "claude --resume " + a.Session
	if a.Cwd != "" {
		cmd = "cd " + shellQuote(a.Cwd) + " && " + cmd
	}
	return cmd
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// readAgents is each PR's agents, as the last tick left them and as herdr's
// panes are now, by openedKey: the sessions that opened or worked on it, and
// the agents (of any kind) in a space on its branch.
func readAgents() map[string][]prAgent {
	st := readLabelState()
	panes, err := listPanes("")
	byPane := map[string]paneInfo{}
	for _, p := range panes {
		byPane[p.PaneID] = p
	}
	out := map[string][]prAgent{}
	for key, pr := range st.PRs {
		opener := pr.opener()
		for s, part := range pr.By {
			if !sessionID.MatchString(s) {
				continue
			}
			ss := st.Sessions[s]
			a := prAgent{Name: clean(ss.Title, false), Session: s, Cwd: clean(ss.Cwd, false), Part: partWorked, last: part.Last}
			if s == opener {
				a.Part = partOpened
			}
			if p, ok := byPane[ss.Pane]; ok && p.TerminalID == ss.Terminal {
				a.Pane, a.Name, a.Status = p.PaneID, p.name(), p.AgentStatus
			} else if err != nil {
				a.Pane = ss.Pane // herdr didn't say: go by the last tick
			}
			if a.Name == "" {
				a.Name = "Claude session " + shorten(s, 9)
			}
			out[key] = append(out[key], a)
		}
	}
	// A pane's tokens were reported for its space's branch, and that branch's
	// PR is known.
	for _, p := range panes {
		if p.Agent == "" {
			continue
		}
		branch := st.Reported[p.PaneID].Branch
		repo, _, _ := strings.Cut(branch, "\x00")
		if e := st.Branches[branch]; branch != "" && e.PR != nil {
			key := repo + "#" + strconv.Itoa(e.PR.Number)
			out[key] = append(out[key], prAgent{Pane: p.PaneID, Name: p.name(), Status: p.AgentStatus, Session: p.session(), Part: partBranch})
		}
	}
	for key, list := range out {
		out[key] = orderAgents(list)
	}
	return out
}

// orderAgents puts a PR's agents in the order they're shown, one line to a
// pane (a pane can have opened it in one session and gone on in another):
// the one that opened it, then those in panes, then by what they had to do
// with it and how lately.
func orderAgents(list []prAgent) []prAgent {
	rank := func(a prAgent) int {
		r := map[string]int{partOpened: 0, partBranch: 1, partWorked: 2}[a.Part]
		if a.Part != partOpened && a.Pane == "" {
			r += 3
		}
		return r
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if rank(a) != rank(b) {
			return rank(a) < rank(b)
		}
		if !a.last.Equal(b.last) {
			return a.last.After(b.last)
		}
		return a.Name+a.Session < b.Name+b.Session
	})
	var out []prAgent
	seen := map[string]bool{}
	for _, a := range list {
		if a.Pane != "" {
			if seen[a.Pane] {
				continue
			}
			seen[a.Pane] = true
		}
		out = append(out, a)
	}
	return out
}

// ── in the popup ──────────────────────────────────────────────────────────────

// agentsShown is how many of a PR's agents its screen lists; with more, the
// last line counts the rest, which are in the menu.
const agentsShown = 4

func (m model) loadAgents() tea.Cmd {
	if d, ok := m.client.(*demoSource); ok {
		return func() tea.Msg { return agentsMsg(d.agents()) }
	}
	return func() tea.Msg { return agentsMsg(readAgents()) }
}

// agentsOf is a PR's agents, the one to go to first; none when no agent here
// had to do with it.
func (m model) agentsOf(pr pullRequest) []prAgent { return m.agents[strings.ToLower(pr.key())] }

// agentNames is what the filter matches of a PR's agents.
func agentNames(as []prAgent) string {
	names := make([]string, len(as))
	for i, a := range as {
		names[i] = a.Name
	}
	return strings.Join(names, " ")
}

// mark is the agent's state, as herdr's sidebar marks it; one with no pane
// (nowhere to go: it can only be resumed) has the empty mark.
func (a prAgent) mark() string {
	if a.Pane == "" {
		return agentMark("", "")
	}
	return agentMark(a.Status, "")
}

// where is the agent's state, or that it has no pane.
func (a prAgent) where() string {
	switch {
	case a.Pane == "":
		return "no pane"
	case a.Status == "" || a.Status == "unknown":
		return ""
	}
	return a.Status
}

// rowAgent is what a list row says of a PR's agents: the first, and how many
// more. hot: the pointer is on it, and a click goes there.
func (m model) rowAgent(pr pullRequest, hot bool) string {
	as := m.agentsOf(pr)
	if len(as) == 0 {
		return ""
	}
	name, style := shorten(as[0].Name, 22), styleDim
	if len(as) > 1 {
		name += fmt.Sprintf(" +%d", len(as)-1)
	}
	if hot {
		style = styleHintHot
	}
	return as[0].mark() + " " + style.Render(name)
}

// agentLines is the PR screen's "Agents" field: a line each, which a click
// goes to. top is the screen line of the first.
func (m model) agentLines(pr pullRequest, top int) string {
	as := m.agentsOf(pr)
	var b strings.Builder
	for i, a := range as {
		name := ""
		if i == 0 {
			name = plural(len(as), "Agent", "Agents")
		}
		hot := m.menu == nil && m.mouseY == top+i
		if i == agentsShown-1 && len(as) > agentsShown {
			style := styleDim
			if hot {
				style = styleHintHot
			}
			b.WriteString(m.field(name, style.Render(fmt.Sprintf("+%d more  ·  a lists them", len(as)-i))))
			break
		}
		line := a.Name
		if hot {
			line = styleHintHot.Render(line)
		}
		for _, s := range []string{a.where(), a.Part} {
			if s != "" {
				line += styleDim.Render("  ·  " + s)
			}
		}
		b.WriteString(m.field(name, a.mark()+" "+line))
	}
	return b.String()
}

// agentAt is which of the PR screen's agent lines is on screen line y.
func (m model) agentAt(y int) (int, bool) {
	if m.screen != screenPR || m.menu != nil || m.cur == nil {
		return 0, false
	}
	i := y - 2 - strings.Count(m.prHeaderTop(), "\n") // tabs, blank, header
	if i < 0 || i >= min(agentsShown, len(m.agentsOf(*m.cur))) {
		return 0, false
	}
	return i, true
}

// clickAgent goes to the agent on the PR screen's i-th agent line, or lists
// them all when that line is the count of the rest.
func (m model) clickAgent(i int) (tea.Model, tea.Cmd) {
	as := m.agentsOf(*m.cur)
	if i == agentsShown-1 && len(as) > agentsShown {
		return m.openMenu(agentsMenu(*m.cur, as)), nil
	}
	return m.goTo(as[i])
}

// goToAgent goes to a PR's agent, asking which when there's more than one.
func (m model) goToAgent(pr pullRequest) (tea.Model, tea.Cmd) {
	as := m.agentsOf(pr)
	switch len(as) {
	case 0:
		m.err, m.flash = fmt.Sprintf("No agent here has worked on #%d", pr.Number), ""
		return m, nil
	case 1:
		return m.goTo(as[0])
	}
	return m.openMenu(agentsMenu(pr, as)), nil
}

func agentsMenu(pr pullRequest, as []prAgent) *menu {
	mn := &menu{id: "agents", title: fmt.Sprintf("#%d's agents", pr.Number)}
	for _, a := range as {
		detail := a.Part
		if w := a.where(); w != "" {
			detail = w + "  ·  " + detail
		}
		if a.Pane == "" {
			detail += "  ·  copies the command that resumes it"
		}
		mn.items = append(mn.items, menuItem{label: a.mark() + " " + a.Name, detail: detail, search: a.Name,
			run: func(m model) (tea.Model, tea.Cmd) { return m.goTo(a) }})
	}
	return mn
}

// goTo takes you to an agent's pane, closing the popup. One with no pane
// can't be gone to, so the command that resumes its session goes on your
// clipboard instead.
func (m model) goTo(a prAgent) (tea.Model, tea.Cmd) {
	m.err, m.flash = "", ""
	switch {
	case m.demo && a.Pane == "":
		m.flash = "Demo: would copy `" + a.resume() + "`"
	case m.demo:
		m.flash = "Demo: would go to " + a.Name
	case a.Pane == "" && a.Session == "":
		m.err = a.Name + " isn't in a pane any more"
	case a.Pane == "":
		if term, err := copyText(a.resume()); err != nil {
			m.err = "Couldn't copy the command: " + err.Error()
		} else {
			m.flash = copied("the command that resumes "+a.Name, term)
		}
	default:
		return m, func() tea.Msg {
			// agent.focus brings its space and tab forward too; a pane the
			// agent has since left is still where it was.
			err := herdrCall("agent.focus", map[string]any{"target": a.Pane}, nil)
			if err != nil {
				err = herdrCall("pane.focus", map[string]any{"pane_id": a.Pane}, nil)
			}
			if err != nil {
				return actionDoneMsg{err: fmt.Errorf("couldn't go to %s: %w", a.Name, err)}
			}
			return actionDoneMsg{}
		}
	}
	return m, nil
}

// ── straight to an agent's PR ─────────────────────────────────────────────────

// prRef names a PR the picker is to open on.
type prRef struct {
	repo   repoRef
	number int
}

func (r prRef) key() string { return openedKey(r.repo, r.number) }

// prRefsOf turns openedKeys (as panePRs gives them) into refs.
func prRefsOf(keys []string) []prRef {
	var out []prRef
	for _, k := range keys {
		repo, num, ok := strings.Cut(k, "#")
		r, valid := parseRepoKey(repo)
		n, err := strconv.Atoi(num)
		if ok && valid && err == nil && n > 0 {
			out = append(out, prRef{r, n})
		}
	}
	return out
}

// paneRefsMsg is an agent's PRs, looked for again after a refresh.
type paneRefsMsg []prRef

// refreshLabelsFn brings labels.json up to date; a seam for tests.
var refreshLabelsFn = func(ctx context.Context, cfg config) { _ = tick(ctx, cfg, true) }

// openForPane starts the picker on the PR of the agent in a pane ("pr"
// action). If none is known, the labels are refreshed and it looks again: a
// PR opened a moment ago isn't known until a tick has seen it.
func (m model) openForPane(pane, space string) (model, tea.Cmd) {
	if refs := prRefsOf(panePRs(readLabelState(), pane, space)); len(refs) > 0 {
		return m.openOn(refs)
	}
	m.opening = "Looking for this agent's pull request…"
	ctx, cfg := m.ctx, m.cfg
	return m, tea.Batch(m.spin.Tick, func() tea.Msg {
		refreshLabelsFn(ctx, cfg)
		return paneRefsMsg(prRefsOf(panePRs(readLabelState(), pane, space)))
	})
}

// noPRForAgent is where the "pr" action ends up when the agent has no PR:
// on this repo's list, where one the plugin couldn't tie to it would be.
func (m model) noPRForAgent() model {
	m.opening = ""
	m.err = "No pull request found for this agent"
	if m.repo != nil {
		m.err += ": these are " + m.repo.String() + "'s"
		m.tab, m.cursor, m.offset, m.wantRepoRow, m.chose = tabRepo, 0, 0, false, false
		m.mode = modeLoading
		if m.loaded[tabRepo] {
			m.mode = modeList
		}
		m.clampCursor()
	}
	return m
}

// prsResolvedMsg is the PRs the picker was opened on, found.
type prsResolvedMsg struct {
	prs []pullRequest
	err error // why one (or all) couldn't be
}

// openOn starts the picker on an agent's PRs, not on the lists: those in a
// list from last time open at once; any other is asked for first, behind a
// line saying so.
func (m model) openOn(refs []prRef) (model, tea.Cmd) {
	known := map[string]pullRequest{}
	for _, list := range m.prs {
		for _, pr := range list {
			known[strings.ToLower(pr.key())] = pr
		}
	}
	prs := make([]pullRequest, len(refs))
	missing := false
	for i, r := range refs {
		pr, ok := known[r.key()]
		prs[i], missing = pr, missing || !ok
	}
	if !missing {
		return m.showPRs(prs)
	}
	m.opening = fmt.Sprintf("Opening #%d…", refs[0].number)
	client, ctx := m.client, m.ctx
	return m, tea.Batch(m.spin.Tick, func() tea.Msg {
		var msg prsResolvedMsg
		for i, r := range refs {
			pr := prs[i]
			if _, ok := known[r.key()]; !ok {
				var err error
				if pr, err = client.byNumber(ctx, r.repo, r.number); err != nil {
					msg.err = err
					continue
				}
			}
			msg.prs = append(msg.prs, pr)
		}
		return msg
	})
}

// showPRs opens the one PR, or asks which of several. You came for the PR,
// so from its screen esc closes the popup; the lists are a ← away.
func (m model) showPRs(prs []pullRequest) (model, tea.Cmd) {
	switch len(prs) {
	case 0:
		return m, nil
	case 1:
		m.direct = true
		next, cmd := m.openPR(prs[0])
		return next.(model), cmd
	}
	mn := &menu{id: "prs", title: "This agent's pull requests"}
	for _, pr := range prs {
		state := "open"
		switch {
		case pr.State == "MERGED":
			state = "merged"
		case pr.State == "CLOSED":
			state = "closed"
		case pr.IsDraft:
			state = "draft"
		}
		mn.items = append(mn.items, menuItem{
			label:  prNumber(pr.Number, pr.State, pr.IsDraft) + " " + pr.Title,
			search: fmt.Sprintf("#%d %s", pr.Number, pr.Title),
			detail: pr.Repository.NameWithOwner + "  ·  " + state,
			run: func(m model) (tea.Model, tea.Cmd) {
				m.direct = true
				return m.openPR(pr)
			},
		})
	}
	return m.openMenu(mn), nil
}
