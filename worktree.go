package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// shorten fits s into n terminal cells, not n characters: a CJK character or
// an emoji takes two cells.
func shorten(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= n {
		return s
	}
	return strings.TrimRight(ansi.Truncate(s, n-1, ""), " ") + "…"
}

// localBranch is the branch a PR is checked out on. A PR from the repo
// itself uses its own branch name, so pushing just works; one from a fork
// gets the fork owner in front ("alice/fix-typo"), since two forks can use
// the same branch name and neither should clobber your own branch.
func localBranch(pr pullRequest) string {
	if pr.IsCrossRepository {
		return pr.headOwner() + "/" + pr.HeadRefName
	}
	return pr.HeadRefName
}

// prMarker is the git config key (branch.<name>.herdr-github-pr) naming the
// PR a branch was made for. Branch names alone can collide: a branch of the
// repo called "alice/fix" and alice's fork PR from "fix" would share one.
const prMarker = "herdr-github-pr"

// prMark is what the marker holds: the PR, and its head (owner:branch) in
// its repo. The head is what a branch really is: a PR closed and reopened
// from the same branch is a new number on the same head.
func prMark(pr pullRequest) string {
	return pr.key() + " " + pr.headOwner() + ":" + pr.HeadRefName
}

// markHead is a marker's repo and head ("host/owner/repo owner:branch"), or
// "" for a marker written before heads were recorded.
func markHead(mark string) string {
	key, head, ok := strings.Cut(mark, " ")
	if !ok {
		return ""
	}
	repo, _, _ := strings.Cut(key, "#")
	return strings.ToLower(repo + " " + head)
}

// branchConflict says why the PR can't use an existing local branch: it was
// made for another PR's head, or (for a fork's PR) it's a branch of yours that
// only happens to have the name. A branch this PR may use is marked as its.
func branchConflict(ctx context.Context, dir string, pr pullRequest, branch string) error {
	mark, _ := git(ctx, dir, "config", "--get", "branch."+branch+"."+prMarker)
	want := prMark(pr)
	ok := false
	switch {
	case mark == want, mark == pr.key():
		ok = true
	case mark != "" && markHead(mark) != "" && markHead(mark) == markHead(want):
		ok = true // the same head, under a new PR number
	case mark != "":
		name, _, _ := strings.Cut(mark, " ")
		return fmt.Errorf("the branch %s belongs to %s, not #%d: delete or rename it to check out #%d here", branch, name, pr.Number, pr.Number)
	case !pr.IsCrossRepository:
		ok = true // a branch of the repo itself: its name is its head
	case pr.HeadRepository != nil && pushesToFork(ctx, dir, pr, branch):
		ok = true // set up for this fork, before branches were marked
	default:
		return fmt.Errorf("a branch named %s already exists and isn't #%d's: rename it to check out the fork's", branch, pr.Number)
	}
	if ok && mark != want {
		_, _ = git(ctx, dir, "config", "branch."+branch+"."+prMarker, want)
	}
	return nil
}

// pushesToFork says whether branch is set up to pull from the PR's fork and
// head branch, as this plugin (or gh) sets up a fork's PR.
func pushesToFork(ctx context.Context, dir string, pr pullRequest, branch string) bool {
	rem, _ := git(ctx, dir, "config", "--get", "branch."+branch+".remote")
	merge, _ := git(ctx, dir, "config", "--get", "branch."+branch+".merge")
	r, ok := parseRemote(rem)
	return ok && strings.EqualFold(r.Owner+"/"+r.Name, pr.HeadRepository.NameWithOwner) &&
		merge == "refs/heads/"+pr.HeadRefName
}

// prWorktree finds the linked worktree on branch in dir. The main checkout
// never counts: it's the repo itself, whatever branch it's on.
func prWorktree(wts []worktreeInfo, branch string) *worktreeInfo {
	for i, w := range wts {
		if w.IsLinkedWorktree && strings.TrimPrefix(w.Branch, "refs/heads/") == branch {
			return &wts[i]
		}
	}
	return nil
}

func worktreeLabel(pr pullRequest) string {
	return shorten(fmt.Sprintf("#%d %s", pr.Number, pr.Title), 48)
}

// Seams for tests: the herdr side of doWorktree.
var (
	openWorktreeFn = openWorktree
	spawnKickoffFn = spawnKickoff
)

// openWorktree focuses the PR's worktree in checkout dir, creating it when
// there is none. It returns the workspace and, for a new worktree, its root
// pane.
//
// A new one starts from the PR's head as GitHub has it right now: fetched
// from the base repo's remote, by branch for a PR from the repo itself and by
// refs/pull/N/head for one from a fork. A fork's branch is then set to pull
// from and push to the fork (as `gh pr checkout` does), which works when the
// author lets maintainers edit.
func openWorktree(ctx context.Context, cfg config, pr pullRequest, dir string) (*worktreeResult, bool, error) {
	branch := localBranch(pr)
	existing, err := listWorktrees(dir)
	if err != nil {
		return nil, false, err
	}
	if w := prWorktree(existing, branch); w != nil {
		if err := branchConflict(ctx, dir, pr, branch); err != nil {
			return nil, false, err
		}
		var res worktreeResult
		err := herdrCall("worktree.open", map[string]any{"cwd": dir, "path": w.Path, "label": worktreeLabel(pr), "focus": true}, &res)
		return &res, false, err
	}

	rem, ok := remoteForCheckout(ctx, cfg, dir, pr.repo())
	if !ok {
		return nil, false, fmt.Errorf("%s has no remote for %s", dir, pr.repo())
	}
	// The fetch goes into a remote-tracking ref of our own naming for a
	// fork's PR, never into a local branch that might hold your work.
	src, dst := "refs/heads/"+pr.HeadRefName, "refs/remotes/"+rem.Name+"/"+pr.HeadRefName
	if pr.IsCrossRepository {
		src, dst = theForge.prHeadRef(pr.Number), fmt.Sprintf("refs/remotes/%s/pr/%d", rem.Name, pr.Number)
	}
	fctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	_, fetchErr := git(fctx, dir, "fetch", "--quiet", "--no-tags", rem.Name, "+"+src+":"+dst)
	cancel()

	params := map[string]any{"cwd": dir, "branch": branch, "label": worktreeLabel(pr), "focus": true}
	_, haveLocal := git(ctx, dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	switch {
	case haveLocal == nil:
		// A branch of that name already exists (its worktree was removed,
		// say): check it out as it is rather than lose commits on it, but
		// only if it's this PR's.
		if err := branchConflict(ctx, dir, pr, branch); err != nil {
			return nil, false, err
		}
	case fetchErr != nil:
		return nil, false, fmt.Errorf("couldn't fetch #%d from %s: %v", pr.Number, rem.Name, fetchErr)
	default:
		params["base"] = strings.TrimPrefix(dst, "refs/remotes/")
	}
	var res worktreeResult
	if err := herdrCall("worktree.create", params, &res); err != nil {
		return nil, false, err
	}
	if haveLocal != nil && res.Worktree.Path != "" {
		_, _ = git(ctx, res.Worktree.Path, "config", "branch."+branch+"."+prMarker, prMark(pr))
		trackHead(ctx, res.Worktree.Path, pr, rem, branch)
	}
	return &res, true, nil
}

// trackHead points a new branch at the PR's head branch, so `git pull` and
// `git push` go to the right place. Best effort: the worktree is usable
// without it.
func trackHead(ctx context.Context, path string, pr pullRequest, rem remote, branch string) {
	if !pr.IsCrossRepository {
		_, _ = git(ctx, path, "branch", "--set-upstream-to="+rem.Name+"/"+pr.HeadRefName, branch)
		return
	}
	if pr.HeadRepository == nil { // the fork is gone
		return
	}
	forkURL := forkRemoteURL(rem.URL, pr.Host, pr.HeadRepository.NameWithOwner)
	_, _ = git(ctx, path, "config", "branch."+branch+".remote", forkURL)
	_, _ = git(ctx, path, "config", "branch."+branch+".merge", "refs/heads/"+pr.HeadRefName)
}

// forkRemoteURL spells the fork's URL the way the base remote is spelled, so
// it authenticates the same way (SSH key or HTTPS credential helper).
func forkRemoteURL(baseURL, host, nameWithOwner string) string {
	if strings.HasPrefix(baseURL, "https://") || strings.HasPrefix(baseURL, "http://") {
		return "https://" + host + "/" + nameWithOwner + ".git"
	}
	if strings.HasPrefix(baseURL, "ssh://") {
		return "ssh://git@" + host + "/" + nameWithOwner + ".git"
	}
	return "git@" + host + ":" + nameWithOwner + ".git"
}

// doWorktree opens (or creates) the PR's worktree; with start, a new
// worktree's agent is also handed its first prompt. Only a new one: an
// existing worktree's agent may be in the middle of something.
func doWorktree(ctx context.Context, cfg config, pr pullRequest, dir string, start bool) actionDoneMsg {
	res, created, err := openWorktreeFn(ctx, cfg, pr, dir)
	if err != nil {
		return actionDoneMsg{err: err}
	}
	spawnTick()
	if !start {
		return actionDoneMsg{}
	}
	if !created {
		return actionDoneMsg{note: fmt.Sprintf("#%d's worktree already existed, so no prompt was sent: its agent may be busy.", pr.Number)}
	}
	if res.RootPane == nil || res.RootPane.PaneID == "" {
		return actionDoneMsg{note: "Worktree created, but herdr didn't say which pane is the new worktree's, so no prompt was sent."}
	}
	if err := spawnKickoffFn(res.Workspace.WorkspaceID, res.RootPane.PaneID, expandPrompt(cfg.StartPrompt, pr)); err != nil {
		return actionDoneMsg{note: "Worktree created, but the prompt couldn't be queued: " + err.Error()}
	}
	return actionDoneMsg{}
}

func expandPrompt(tmpl string, pr pullRequest) string {
	return strings.NewReplacer(
		"{number}", fmt.Sprint(pr.Number), "{url}", pr.URL, "{repo}", pr.Repository.NameWithOwner,
		"{branch}", pr.HeadRefName, "{title}", pr.Title,
	).Replace(tmpl)
}

// removeWorktree removes the PR's worktree after a merge: through herdr when
// a space has it open (which closes the space), else with git. Neither
// forces: uncommitted changes keep the worktree where it is.
func removeWorktree(ctx context.Context, pr pullRequest, dir string) error {
	ws, err := listWorktrees(dir)
	if err != nil {
		return err
	}
	branch := localBranch(pr)
	w := prWorktree(ws, branch)
	if w == nil {
		return errors.New("its worktree is already gone")
	}
	if err := branchConflict(ctx, dir, pr, branch); err != nil {
		return err
	}
	if w.OpenWorkspaceID != "" {
		return herdrCall("worktree.remove", map[string]any{"workspace_id": w.OpenWorkspaceID}, nil)
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", dir, "worktree", "remove", w.Path).CombinedOutput(); err != nil {
		return errors.New(strings.TrimSpace(clean(string(out), false)))
	}
	return nil
}

// ── detached helpers ──────────────────────────────────────────────────────────

// spawnDetached runs this binary with args in its own session, so it outlives
// the popup that started it. Output goes to a small log in the state dir.
func spawnDetached(stdin string, args ...string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	logPath := stateDir() + "/background.log"
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > 256<<10 {
		_ = os.Rename(logPath, logPath+".old") // keep the log small: one generation back
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := startWithInput(cmd, stdin); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// startWithInput starts cmd with stdin on a pipe that's written in full
// before it returns. With a Reader, exec copies stdin in a goroutine that dies
// with this process, and the popup quits right after starting a kickoff, so
// the child could read nothing.
func startWithInput(cmd *exec.Cmd, stdin string) error {
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdin = r
	err = cmd.Start()
	r.Close()
	if err != nil {
		w.Close()
		return err
	}
	_, werr := w.WriteString(stdin)
	if cerr := w.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// spawnTickFn refreshes the sidebar labels in the background after the
// popup changed something; a seam for tests and the demo.
var spawnTickFn = func() { _ = spawnDetached("", "tick", "--force") }

func spawnTick() { spawnTickFn() }

// spawnKickoff runs `kickoff` detached. The prompt goes over stdin, not argv:
// any local user can list another process's arguments.
func spawnKickoff(workspaceID, rootPaneID, text string) error {
	return spawnDetached(text, "kickoff", workspaceID, rootPaneID)
}

// kickoff waits for an agent to come up in the new worktree's own pane (your
// worktree template starts it) and be ready for input, then sends the prompt.
// An approval or trust dialog is waited out: that's yours to answer.
func kickoff(cfg config, workspaceID, rootPaneID, text string) error {
	deadline := time.Now().Add(time.Duration(cfg.AgentWaitSeconds) * time.Second)
	for time.Now().Before(deadline) {
		panes, err := listPanes(workspaceID)
		if err != nil {
			return err
		}
		if p := agentPane(panes, rootPaneID); p != nil && (p.AgentStatus == "idle" || p.AgentStatus == "done") {
			err := herdrCall("agent.prompt", map[string]any{
				"target": p.PaneID, "text": text,
				"wait": map[string]any{"timeout_ms": 8000, "until": []string{"working", "blocked"}},
			}, nil)
			switch {
			case err == nil:
				fmt.Println(time.Now().Format(time.RFC3339), "prompt sent to", p.PaneID)
				return nil
			case isHerdrCode(err, "agent_prompt_stalled"), isHerdrCode(err, "timeout"):
				// It may still be sitting in the input box: resending could
				// duplicate it, so stop here and say so.
				notify("Typed the prompt but the agent didn't start. Press enter in the new space.")
				return err
			case isHerdrCode(err, "agent_blocked"):
				// A dialog came up: wait for it to be answered.
			default:
				return err
			}
		}
		time.Sleep(time.Second)
	}
	notify("No agent came up in the new space, so the PR prompt wasn't sent.")
	return errors.New("no ready agent before the deadline")
}

// agentPane is the new worktree's own pane, once an agent runs in it.
func agentPane(panes []paneInfo, rootPaneID string) *paneInfo {
	for i := range panes {
		if p := &panes[i]; p.PaneID == rootPaneID && p.Agent != "" {
			return p
		}
	}
	return nil
}
