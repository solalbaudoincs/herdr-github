package main

import (
	"regexp"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// The repo tab shows the repo of the space the popup was opened from; ctrl+t
// (or a click on that tab when you're on it) picks another for this popup.
// Next time it opens on the space's repo again.

// repoChoice is one repo the picker offers, and why it's there.
type repoChoice struct {
	repo repoRef
	note string
}

type repoChoicesMsg []repoChoice

// loadRepoChoices gathers the repos worth offering: this space's, the ones
// picked lately, every herdr space's, and those your PRs are in. herdr and
// git are asked off the UI's goroutine; the lists are read here.
func (m model) loadRepoChoices() tea.Cmd {
	var fromLists []repoChoice
	for _, t := range []tab{tabMine, tabReview} {
		note := map[tab]string{tabMine: "your PRs", tabReview: "review requested"}[t]
		for _, pr := range m.prs[t] {
			fromLists = append(fromLists, repoChoice{pr.repo(), note})
		}
	}
	here, cfg, ctx := m.home, m.cfg, m.ctx
	if d, ok := m.client.(*demoSource); ok {
		return func() tea.Msg {
			var out []repoChoice
			if here != nil {
				out = append(out, repoChoice{*here, "this space"})
			}
			for _, r := range d.spaceRepos() {
				out = append(out, repoChoice{r, "a herdr space"})
			}
			return repoChoicesMsg(dedupeChoices(append(out, fromLists...)))
		}
	}
	return func() tea.Msg {
		var out []repoChoice
		if here != nil {
			out = append(out, repoChoice{*here, "this space"})
		}
		for _, k := range readPrefs().RecentRepos {
			if r, ok := parseRepoKey(k); ok {
				out = append(out, repoChoice{r, "recent"})
			}
		}
		if wss, err := listWorkspacesFn(); err == nil {
			for _, w := range wss {
				if w.Worktree == nil || w.Worktree.RepoRoot == "" {
					continue
				}
				if r, ok := repoOf(ctx, w.Worktree.RepoRoot); ok && cfg.knownHost(r.Host) {
					out = append(out, repoChoice{r, "a herdr space"})
				}
			}
		}
		return repoChoicesMsg(dedupeChoices(append(out, fromLists...)))
	}
}

// dedupeChoices keeps each repo once, where it first appears.
func dedupeChoices(cs []repoChoice) []repoChoice {
	seen := map[string]bool{}
	var out []repoChoice
	for _, c := range cs {
		if c.repo.valid() && !seen[c.repo.key()] {
			seen[c.repo.key()] = true
			out = append(out, c)
		}
	}
	return out
}

// remoteReposMsg brings a page of the repos you can reach on GitHub.
type remoteReposMsg struct {
	cursor string // the page asked for: "" is the first
	repos  []repoInfo
	next   string
	err    error
}

func (m model) loadRemoteRepos(cursor string) tea.Cmd {
	client, ctx := m.client, m.ctx
	return func() tea.Msg {
		rs, next, err := client.repos(ctx, cursor)
		return remoteReposMsg{cursor, rs, next, err}
	}
}

// mergeRepos is fresh, then whatever of old fresh doesn't have yet: while
// the pages come in, last time's list fills the gaps.
func mergeRepos(fresh, old []repoInfo) []repoInfo {
	seen := map[string]bool{}
	out := append([]repoInfo(nil), fresh...)
	for _, r := range fresh {
		seen[r.Repo.key()] = true
	}
	for _, r := range old {
		if !seen[r.Repo.key()] {
			out = append(out, r)
		}
	}
	return out
}

// takeRemoteRepos adds a page to the repo list. It's saved after every
// page, so even a popup closed a moment after opening leaves a list for the
// next one.
func (m model) takeRemoteRepos(msg remoteReposMsg) (model, tea.Cmd) {
	if msg.cursor == "" {
		m.repoFresh = nil
	}
	if msg.err != nil && len(msg.repos) == 0 {
		m.remoteLoading, m.remoteLoaded = false, true
		// Keep last time's list; say so only if there's none.
		if len(m.repoRemote) == 0 && m.menu != nil && m.menu.id == "repos" {
			m.err = "Couldn't list your " + theForge.name + " repos: " + msg.err.Error()
		}
		return m.refreshRepoMenu(), nil
	}
	m.repoFresh = append(m.repoFresh, msg.repos...)
	var cmds []tea.Cmd
	if msg.next != "" && len(m.repoFresh) < 300 {
		m.repoRemote = mergeRepos(m.repoFresh, m.repoRemote)
		cmds = append(cmds, m.loadRemoteRepos(msg.next))
	} else {
		m.repoRemote = m.repoFresh
		m.remoteLoading, m.remoteLoaded = false, true
	}
	if !m.demo {
		repos := append([]repoInfo(nil), m.repoRemote...)
		cmds = append(cmds, func() tea.Msg { saveRepoCache(repos); return nil })
	}
	return m.refreshRepoMenu(), tea.Batch(cmds...)
}

// repoEntry is a repo in the picker: from here (a space, a recent pick, a
// PR list; rank orders those) and/or from GitHub (pushed).
type repoEntry struct {
	repo   repoRef
	note   string
	rank   int // position among the local choices; -1: GitHub's only
	pushed time.Time
}

// repoMenu is the picker, grouped by owner (org or user): this space's org
// first, then the orgs of your spaces, picks and PRs, then the rest of what
// you can reach on GitHub, most recently pushed first. Within an org, the
// same order. Typing filters; an org's name keeps its whole group; an
// owner/repo that isn't listed is offered as typed.
func (m model) repoMenu() *menu {
	title := "Pull requests of which repo?"
	if !m.remoteLoaded && len(m.repoRemote) == 0 {
		title += " (loading yours from " + theForge.name + "…)"
	}
	mn := &menu{id: "repos", title: title, filterable: true}

	byKey := map[string]*repoEntry{}
	var entries []*repoEntry
	for i, c := range m.repoLocal {
		e := &repoEntry{repo: c.repo, note: c.note, rank: i}
		byKey[c.repo.key()] = e
		entries = append(entries, e)
	}
	for _, r := range m.repoRemote {
		if e, ok := byKey[r.Repo.key()]; ok {
			e.pushed = r.Pushed
			continue
		}
		e := &repoEntry{repo: r.Repo, rank: -1, pushed: r.Pushed}
		byKey[r.Repo.key()] = e
		entries = append(entries, e)
	}

	type group struct {
		name    string
		entries []*repoEntry
		rank    int // best local rank, -1 if none
		pushed  time.Time
	}
	groups := map[string]*group{}
	var order []*group
	for _, e := range entries {
		k := strings.ToLower(e.repo.Host + "/" + e.repo.Owner)
		g, ok := groups[k]
		if !ok {
			name := e.repo.Owner
			if !theForge.isDefaultHost(e.repo.Host) {
				name = e.repo.Host + "/" + name
			}
			g = &group{name: name, rank: -1}
			groups[k] = g
			order = append(order, g)
		}
		g.entries = append(g.entries, e)
		if e.rank >= 0 && (g.rank < 0 || e.rank < g.rank) {
			g.rank = e.rank
		}
		if e.pushed.After(g.pushed) {
			g.pushed = e.pushed
		}
	}
	before := func(ra, rb int, pa, pb time.Time, na, nb string) bool {
		switch {
		case ra >= 0 && rb >= 0 && ra != rb:
			return ra < rb
		case (ra >= 0) != (rb >= 0):
			return ra >= 0
		case !pa.Equal(pb):
			return pa.After(pb)
		}
		return strings.ToLower(na) < strings.ToLower(nb)
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		return before(a.rank, b.rank, a.pushed, b.pushed, a.name, b.name)
	})

	for _, g := range order {
		sort.SliceStable(g.entries, func(i, j int) bool {
			a, b := g.entries[i], g.entries[j]
			return before(a.rank, b.rank, a.pushed, b.pushed, a.repo.Name, b.repo.Name)
		})
		mn.items = append(mn.items, menuItem{label: g.name, header: true})
		for _, e := range g.entries {
			r := e.repo
			note := e.note
			if m.repo != nil && r.same(*m.repo) {
				note = strings.TrimPrefix(note+" · showing", " · ")
			}
			if note == "" && !e.pushed.IsZero() {
				note = "pushed " + ago(e.pushed)
			}
			mn.items = append(mn.items, menuItem{
				label: r.Name, detail: note, search: r.Owner + "/" + r.Name + " " + note,
				run: func(m model) (tea.Model, tea.Cmd) { return m.switchRepo(r) },
			})
		}
	}

	host := m.cfg.Hosts[0]
	if m.repo != nil {
		host = m.repo.Host
	}
	mn.typed = func(q string) *menuItem {
		r, ok := typedRepo(strings.TrimSpace(q), host)
		if !ok || byKey[r.key()] != nil || !m.cfg.knownHost(r.Host) {
			return nil
		}
		return &menuItem{label: r.String(), detail: "as typed", run: func(m model) (tea.Model, tea.Cmd) {
			return m.switchRepo(r)
		}}
	}
	return mn
}

// refreshRepoMenu redraws an open picker with what's now known, keeping the
// filter and the repo the cursor was on.
func (m model) refreshRepoMenu() model {
	if m.menu == nil || m.menu.id != "repos" {
		return m
	}
	items := m.menuItems()
	on := ""
	if m.menuCursor < len(items) {
		on = items[m.menuCursor].search
	}
	mn := m.repoMenu()
	mn.query = m.menu.query
	m.menu = mn
	m.menuCursor = m.selectable(0, 1)
	for i, it := range m.menuItems() {
		if on != "" && it.search == on {
			m.menuCursor = i
		}
	}
	m.menuOffset = m.menuStart(m.menuRoom())
	return m
}

var repoNamePart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// typedRepo reads "owner/repo" (on host) or "host/owner/repo".
func typedRepo(q, host string) (repoRef, bool) {
	parts := strings.Split(q, "/")
	if len(parts) >= 3 && strings.Contains(parts[0], ".") {
		host, parts = strings.ToLower(parts[0]), parts[1:]
	}
	if len(parts) < 2 || (len(parts) > 2 && !theForge.nestedPaths) {
		return repoRef{}, false
	}
	for _, p := range parts {
		if !repoNamePart.MatchString(p) {
			return repoRef{}, false
		}
	}
	return repoRef{Host: host, Owner: strings.Join(parts[:len(parts)-1], "/"), Name: parts[len(parts)-1]}, true
}

func parseRepoKey(k string) (repoRef, bool) {
	host, path, _ := strings.Cut(k, "/")
	owner, name, ok := splitRepoPath(path, theForge.nestedPaths)
	r := repoRef{Host: host, Owner: owner, Name: name}
	return r, ok && r.valid()
}

// openRepoPicker opens the picker once the local choices are gathered; your
// GitHub repos join it when they arrive (once per popup).
func (m model) openRepoPicker() (tea.Model, tea.Cmd) {
	m.flash, m.err = "", ""
	cmds := []tea.Cmd{m.loadRepoChoices()}
	if !m.remoteLoaded && !m.remoteLoading {
		m.remoteLoading = true
		cmds = append(cmds, m.loadRemoteRepos(""))
	}
	return m, tea.Batch(cmds...)
}

// switchRepo points the repo tab at r for this popup and loads its PRs.
func (m model) switchRepo(r repoRef) (tea.Model, tea.Cmd) {
	m.repo = &r
	switch c := m.client.(type) {
	case *githubSource, *gitlabSource:
		m.client = theForge.newSource(m.cfg, &r)
	case *demoSource:
		c.setHome(r)
	}
	m.prs[tabRepo], m.loaded[tabRepo], m.stale[tabRepo], m.paging[tabRepo], m.tabErr[tabRepo] = nil, false, false, false, ""
	m.tab, m.screen, m.cur, m.curDetail = tabRepo, screenList, nil, nil
	m.cursor, m.offset, m.chose, m.mode = 0, 0, false, modeLoading
	m.picks++
	if !m.demo {
		rememberRecentRepo(r)
	}
	return m, tea.Batch(m.spin.Tick, m.loadTab(tabRepo, ""))
}
