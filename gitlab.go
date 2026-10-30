package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitLab, over its REST API (v4). Its merge requests are mapped onto the
// plugin's pull requests (pullRequest, prDetail, prStatus).

var gitlabForge = forge{
	name:        "GitLab",
	defaultHost: "gitlab.com",
	token:       glToken,
	prFromPath: func(host, path string, number int) (repoRef, int, bool) {
		project, rest, ok := strings.Cut(strings.Trim(path, "/"), "/-/")
		if !ok || rest != "merge_requests/"+strconv.Itoa(number) {
			return repoRef{}, 0, false
		}
		owner, name, ok := splitRepoPath(project, true)
		r := repoRef{Host: strings.ToLower(host), Owner: owner, Name: name}
		if !ok || !r.valid() {
			return repoRef{}, 0, false
		}
		return r, number, true
	},
	prHeadRef: func(number int) string { return fmt.Sprintf("refs/merge-requests/%d/head", number) },
	cloneCmd: func(ctx context.Context, host string, r repoRef, dest string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "glab", "repo", "clone", r.String(), dest)
		cmd.Env = append(os.Environ(), "GITLAB_HOST="+host)
		return cmd
	},
	loginCmd: func(host string) *exec.Cmd {
		return exec.Command("glab", "auth", "login", "--hostname", host, "--web")
	},
	loginText:   "glab auth login",
	cliName:     "glab",
	methodNames: map[string]string{"MERGE": "Merge", "SQUASH": "Squash and merge"},
	methodVerbs: map[string]string{"MERGE": "Merge", "SQUASH": "Squash-merge"},
	startPrompt: "Merge request {url} is checked out here. Read it with `glab mr view {number} --comments`, look at any failing checks, and tell me what's left to do.",
	nestedPaths: true,
	newSource:   func(cfg config, repo *repoRef) source { return newGitLabSource(cfg, repo) },
	api:         func(host string) forgeAPI { return newGLClient(host) },
}

// ── auth and transport ────────────────────────────────────────────────────────

// glToken finds a host's token as glab does: its environment variables
// first (for the host GITLAB_HOST names, or any host when it names none),
// then glab's own store. glab has no `auth token`; `config get token` reads
// the keyring or its config file and prints only the token.
func glToken(ctx context.Context, host string) (string, error) {
	if h := os.Getenv("GITLAB_HOST"); h == "" || strings.EqualFold(hostOf(h), host) {
		for _, v := range []string{"GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "OAUTH_TOKEN"} {
			if t := strings.TrimSpace(os.Getenv(v)); t != "" {
				return t, nil
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "glab", "config", "get", "token", "--host", host).Output()
	if err != nil {
		var ee *exec.Error
		if errors.As(err, &ee) {
			return "", errors.New("glab isn't installed: https://gitlab.com/gitlab-org/cli")
		}
		return "", &signedOutError{host}
	}
	t := strings.TrimSpace(string(out))
	if t == "" {
		return "", &signedOutError{host}
	}
	return t, nil
}

// hostOf is the host in GITLAB_HOST, which can be a URL.
func hostOf(s string) string {
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return u.Hostname()
	}
	return strings.TrimSuffix(s, "/")
}

// glEndpointFn is the REST API's base URL for a host; a seam for tests.
var glEndpointFn = func(host string) string { return "https://" + host + "/api/v4" }

// glPageSize is every list page's size: GitLab pages by number, so the size
// can't change between one page and the next.
const glPageSize = 50

type glClient struct {
	host     string
	mu       sync.Mutex
	token    string
	http     *http.Client
	projects map[int]string // project id → its path, for forks
}

func newGLClient(host string) *glClient {
	return &glClient{host: host, http: &http.Client{Timeout: 30 * time.Second}, projects: map[int]string{}}
}

func (c *glClient) auth(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" {
		return c.token, nil
	}
	t, err := tokenFn(ctx, c.host)
	if err != nil {
		return "", err
	}
	c.token = t
	return t, nil
}

// glHTTPError is an answer GitLab refused with.
type glHTTPError struct {
	status int
	msg    string
}

func (e *glHTTPError) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("GitLab: HTTP %d", e.status)
	}
	return fmt.Sprintf("GitLab: HTTP %d: %s", e.status, e.msg)
}

func isStatus(err error, status int) bool {
	var h *glHTTPError
	return errors.As(err, &h) && h.status == status
}

// call makes one request; body, when there is one, is sent as JSON. Every
// string in the answer was written by someone else, so it's sanitized
// before anything else sees it.
func (c *glClient) call(ctx context.Context, method, path string, q url.Values, body any, out any) (http.Header, error) {
	token, err := c.auth(ctx)
	if err != nil {
		return nil, err
	}
	u := glEndpointFn(c.host) + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "herdr-github")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitLab: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("GitLab: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		c.mu.Lock()
		c.token = ""
		c.mu.Unlock()
		return nil, &signedOutError{c.host}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &glHTTPError{resp.StatusCode, glMessage(data)}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, fmt.Errorf("GitLab: bad reply: %w", err)
		}
		sanitize(out)
	}
	return resp.Header, nil
}

// glMessage is what an error reply says: its "message" or "error", which
// can be a string or, for a validation error, an object.
func glMessage(data []byte) string {
	var reply struct {
		Message json.RawMessage `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &reply) != nil {
		return ""
	}
	raw := reply.Message
	if len(raw) == 0 {
		raw = reply.Error
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		s = string(raw)
	}
	return clean(s, false)
}

func projectPath(r repoRef) string {
	return "/projects/" + url.PathEscape(r.Owner+"/"+r.Name)
}

func mrPath(r repoRef, iid int) string {
	return projectPath(r) + "/merge_requests/" + strconv.Itoa(iid)
}

// each runs fn for 0..n-1, a few at a time.
func each(n int, fn func(i int)) {
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			fn(i)
		}()
	}
	wg.Wait()
}

// ── data ──────────────────────────────────────────────────────────────────────

type glUser struct {
	Username string `json:"username"`
}

type glPipeline struct {
	ID        int    `json:"id"`
	ProjectID int    `json:"project_id"`
	Status    string `json:"status"`
}

// glLabels are an MR's labels: names, or with with_labels_details objects
// with a colour too.
type glLabels []label

func (l *glLabels) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, r := range raw {
		var x label
		if json.Unmarshal(r, &x.Name) != nil {
			if err := json.Unmarshal(r, &x); err != nil {
				return err
			}
		}
		x.Color = strings.TrimPrefix(x.Color, "#")
		*l = append(*l, x)
	}
	return nil
}

type glMR struct {
	ID                        int         `json:"id"`
	IID                       int         `json:"iid"`
	Title                     string      `json:"title"`
	Body                      string      `json:"description"`
	State                     string      `json:"state"`
	WebURL                    string      `json:"web_url"`
	Draft                     bool        `json:"draft"`
	UpdatedAt                 time.Time   `json:"updated_at"`
	ClosedAt                  *time.Time  `json:"closed_at"`
	MergedAt                  *time.Time  `json:"merged_at"`
	Author                    *glUser     `json:"author"`
	SourceBranch              string      `json:"source_branch"`
	TargetBranch              string      `json:"target_branch"`
	SHA                       string      `json:"sha"`
	SourceProjectID           int         `json:"source_project_id"`
	TargetProjectID           int         `json:"target_project_id"`
	DetailedMergeStatus       string      `json:"detailed_merge_status"`
	HasConflicts              bool        `json:"has_conflicts"`
	MergeWhenPipelineSucceeds bool        `json:"merge_when_pipeline_succeeds"`
	Squash                    bool        `json:"squash"`
	ShouldRemoveSourceBranch  *bool       `json:"should_remove_source_branch"`
	ForceRemoveSourceBranch   *bool       `json:"force_remove_source_branch"`
	AllowCollaboration        bool        `json:"allow_collaboration"`
	HeadPipeline              *glPipeline `json:"head_pipeline"`
	ChangesCount              string      `json:"changes_count"`
	Labels                    glLabels    `json:"labels"`
	User                      *struct {
		CanMerge bool `json:"can_merge"`
	} `json:"user"`
}

func (mr glMR) fork() bool {
	return mr.SourceProjectID != 0 && mr.SourceProjectID != mr.TargetProjectID
}

// state is GitLab's state as GitHub spells it. A locked MR is one being
// merged right now: still open.
func (mr glMR) state() string {
	switch mr.State {
	case "merged":
		return "MERGED"
	case "closed":
		return "CLOSED"
	}
	return "OPEN"
}

func (mr glMR) mergeable() string {
	switch {
	case mr.HasConflicts || mr.DetailedMergeStatus == "conflict":
		return "CONFLICTING"
	case mr.DetailedMergeStatus == "mergeable":
		return "MERGEABLE"
	}
	return "UNKNOWN"
}

// mergeState is detailed_merge_status as GitHub's mergeStateStatus: what
// the PR screen says about merging.
func (mr glMR) mergeState() string {
	switch mr.DetailedMergeStatus {
	case "mergeable":
		return "CLEAN"
	case "need_rebase":
		return "BEHIND"
	case "conflict":
		return "DIRTY"
	case "draft_status":
		return "DRAFT"
	case "", "checking", "unchecked", "preparing", "approvals_syncing":
		return "UNKNOWN"
	}
	return "BLOCKED"
}

// reviewDecision is what detailed_merge_status says of reviews; the PR
// screen adds what the approvals say.
func (mr glMR) reviewDecision() string {
	switch mr.DetailedMergeStatus {
	case "requested_changes":
		return "CHANGES_REQUESTED"
	case "not_approved":
		return "REVIEW_REQUIRED"
	}
	return ""
}

// checks is the head pipeline's state as a check rollup. A list's MRs come
// without their pipeline: then what detailed_merge_status says of it.
func (mr glMR) checks() string {
	if p := mr.HeadPipeline; p != nil {
		switch p.Status {
		case "success":
			return "SUCCESS"
		case "failed":
			return "FAILURE"
		case "canceled":
			return "ERROR"
		case "skipped", "manual":
			return ""
		}
		return "PENDING"
	}
	switch mr.DetailedMergeStatus {
	case "ci_still_running":
		return "PENDING"
	case "ci_must_pass":
		return "FAILURE"
	}
	return ""
}

func checkRollup(state string) rollup {
	var r rollup
	if state == "" {
		return r
	}
	r.Nodes = make([]struct {
		Commit struct {
			StatusCheckRollup *struct {
				State string `json:"state"`
			} `json:"statusCheckRollup"`
		} `json:"commit"`
	}, 1)
	r.Nodes[0].Commit.StatusCheckRollup = &struct {
		State string `json:"state"`
	}{state}
	return r
}

// projectOf is the MR's project path, from its URL ("…/group/proj/-/merge_requests/1").
func projectOf(webURL string) string {
	u, err := url.Parse(webURL)
	if err != nil {
		return ""
	}
	p, _, ok := strings.Cut(u.Path, "/-/merge_requests/")
	if !ok {
		return ""
	}
	return strings.Trim(p, "/")
}

// sourcePath is the path of a fork MR's source project; "" when it isn't
// a fork's, or the fork can't be seen.
func (c *glClient) sourcePath(ctx context.Context, mr glMR) string {
	if !mr.fork() {
		return ""
	}
	c.mu.Lock()
	p, ok := c.projects[mr.SourceProjectID]
	c.mu.Unlock()
	if ok {
		return p
	}
	var proj struct {
		Path string `json:"path_with_namespace"`
	}
	if _, err := c.call(ctx, "GET", "/projects/"+strconv.Itoa(mr.SourceProjectID), nil, nil, &proj); err != nil {
		return ""
	}
	c.mu.Lock()
	c.projects[mr.SourceProjectID] = proj.Path
	c.mu.Unlock()
	return proj.Path
}

// pr is the MR as a row in the lists.
func (c *glClient) pr(ctx context.Context, mr glMR) pullRequest {
	pr := pullRequest{
		Host: c.host, ID: strconv.Itoa(mr.ID), Number: mr.IID, Title: mr.Title, URL: mr.WebURL,
		IsDraft: mr.Draft, State: mr.state(), UpdatedAt: mr.UpdatedAt,
		HeadRefName: mr.SourceBranch, BaseRefName: mr.TargetBranch, IsCrossRepository: mr.fork(),
		ReviewDecision: mr.reviewDecision(), Mergeable: mr.mergeable(), Commits: checkRollup(mr.checks()),
	}
	if mr.Author != nil {
		pr.Author = &actor{mr.Author.Username}
	}
	pr.Repository.NameWithOwner = projectOf(mr.WebURL)
	pr.Labels.Nodes = mr.Labels
	if src := c.sourcePath(ctx, mr); src != "" {
		owner, _, _ := splitRepoPath(src, true)
		pr.HeadRepositoryOwner = &actor{owner}
		pr.HeadRepository = &struct {
			NameWithOwner string `json:"nameWithOwner"`
		}{src}
	}
	if mr.MergeWhenPipelineSucceeds {
		method := "MERGE"
		if mr.Squash {
			method = "SQUASH"
		}
		pr.AutoMergeRequest = &struct {
			EnabledAt   string `json:"enabledAt"`
			MergeMethod string `json:"mergeMethod"`
		}{MergeMethod: method}
	}
	return pr
}

// status is the MR as a sidebar label has it.
func (c *glClient) status(ctx context.Context, mr glMR) prStatus {
	pr := c.pr(ctx, mr)
	s := prStatus{
		Number: pr.Number, State: pr.State, IsDraft: pr.IsDraft, ReviewDecision: pr.ReviewDecision,
		Mergeable: pr.Mergeable, URL: pr.URL, HeadRepositoryOwner: pr.HeadRepositoryOwner, Commits: pr.Commits,
	}
	if mr.MergeWhenPipelineSucceeds {
		s.AutoMergeRequest = &struct {
			EnabledAt string `json:"enabledAt"`
		}{}
	}
	if mr.MergedAt != nil {
		s.ClosedAt = mr.MergedAt
	} else if mr.ClosedAt != nil {
		s.ClosedAt = mr.ClosedAt
	}
	return s
}

// ── lists ─────────────────────────────────────────────────────────────────────

// mrPage is one page of an MR list. The cursor is GitLab's page number.
func (c *glClient) mrPage(ctx context.Context, path string, q url.Values, cursor string) ([]pullRequest, string, error) {
	q.Set("state", "opened")
	q.Set("order_by", "updated_at")
	q.Set("with_labels_details", "true")
	q.Set("per_page", strconv.Itoa(glPageSize))
	if cursor != "" {
		q.Set("page", cursor)
	}
	var mrs []glMR
	h, err := c.call(ctx, "GET", path, q, nil, &mrs)
	if err != nil {
		return nil, "", err
	}
	prs := make([]pullRequest, len(mrs))
	each(len(mrs), func(i int) { prs[i] = c.pr(ctx, mrs[i]) })
	return prs, h.Get("X-Next-Page"), nil
}

func (c *glClient) mr(ctx context.Context, r repoRef, iid int) (glMR, error) {
	var mr glMR
	_, err := c.call(ctx, "GET", mrPath(r, iid), nil, nil, &mr)
	return mr, err
}

// ── the PR screen ─────────────────────────────────────────────────────────────

func (c *glClient) detail(ctx context.Context, pr pullRequest) (*prDetail, error) {
	r := pr.repo()
	mr, err := c.mr(ctx, r, pr.Number)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return nil, fmt.Errorf("!%d isn't there any more", pr.Number)
		}
		return nil, err
	}
	d := &prDetail{pullRequest: c.pr(ctx, mr), Body: mr.Body, MergeStateStatus: mr.mergeState(),
		HeadRefOid: mr.SHA, MaintainerCanModify: mr.AllowCollaboration}
	if !mr.fork() {
		d.HeadRef = &struct {
			ID string `json:"id"`
		}{mr.SourceBranch}
	}
	d.ViewerCanMerge = mr.User != nil && mr.User.CanMerge
	d.RepoSettings.AutoMergeAllowed = true
	d.RepoSettings.DeleteBranchOnMerge = (mr.ForceRemoveSourceBranch != nil && *mr.ForceRemoveSourceBranch) ||
		(mr.ShouldRemoveSourceBranch != nil && *mr.ShouldRemoveSourceBranch)
	d.ChangedFiles, _ = strconv.Atoi(strings.TrimSuffix(mr.ChangesCount, "+"))

	// The rest is extra: what can't be had is left out, not an error.
	var wg sync.WaitGroup
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	var approved bool
	var approvers, reviewers []reviewer
	run(func() { c.settings(ctx, r, d) })
	run(func() { approved, approvers = c.approvals(ctx, r, mr.IID) })
	run(func() { reviewers = c.reviewers(ctx, r, mr.IID) })
	run(func() { d.Comments, d.Threads = c.discussions(ctx, r, mr.IID) })
	run(func() { d.Checks = c.jobs(ctx, mr.HeadPipeline) })
	run(func() { d.Additions, d.Deletions = c.lineCounts(ctx, r, mr.IID) })
	wg.Wait()

	sortChecks(d.Checks)
	d.Reviewers = mergeReviewers(reviewers, approvers)
	if d.ReviewDecision == "" && approved && len(approvers) > 0 {
		d.ReviewDecision = "APPROVED"
	}
	return d, nil
}

// settings are the project's merge settings. squash_option decides which
// methods there are: never, always, or either, with one ticked by default.
func (c *glClient) settings(ctx context.Context, r repoRef, d *prDetail) {
	var p struct {
		SquashOption string `json:"squash_option"`
	}
	if _, err := c.call(ctx, "GET", projectPath(r), nil, nil, &p); err != nil {
		p.SquashOption = "default_off"
	}
	s := &d.RepoSettings
	s.MergeCommitAllowed = p.SquashOption != "always"
	s.SquashMergeAllowed = p.SquashOption != "never"
	s.ViewerDefaultMergeMethod = "MERGE"
	if p.SquashOption == "always" || p.SquashOption == "default_on" {
		s.ViewerDefaultMergeMethod = "SQUASH"
	}
}

func (c *glClient) approvals(ctx context.Context, r repoRef, iid int) (bool, []reviewer) {
	var a struct {
		Approved   bool `json:"approved"`
		ApprovedBy []struct {
			User glUser `json:"user"`
		} `json:"approved_by"`
	}
	if _, err := c.call(ctx, "GET", mrPath(r, iid)+"/approvals", nil, nil, &a); err != nil {
		return false, nil
	}
	var out []reviewer
	for _, u := range a.ApprovedBy {
		out = append(out, reviewer{u.User.Username, "APPROVED"})
	}
	return a.Approved, out
}

func (c *glClient) reviewers(ctx context.Context, r repoRef, iid int) []reviewer {
	var rs []struct {
		User  glUser `json:"user"`
		State string `json:"state"`
	}
	if _, err := c.call(ctx, "GET", mrPath(r, iid)+"/reviewers", nil, nil, &rs); err != nil {
		return nil
	}
	var out []reviewer
	for _, x := range rs {
		state := map[string]string{"approved": "APPROVED", "requested_changes": "CHANGES_REQUESTED", "reviewed": "COMMENTED"}[x.State]
		if state == "" {
			state = "PENDING"
		}
		out = append(out, reviewer{x.User.Username, state})
	}
	return out
}

// mergeReviewers is the reviewers, with an approval counting for whoever
// gave it, reviewer or not.
func mergeReviewers(reviewers, approvers []reviewer) []reviewer {
	out := append([]reviewer(nil), reviewers...)
	for _, a := range approvers {
		found := false
		for i := range out {
			if out[i].Name == a.Name {
				out[i].State, found = "APPROVED", true
			}
		}
		if !found {
			out = append(out, a)
		}
	}
	return out
}

// discussions are the MR's comments and its threads on the code. A comment
// thread that isn't on a line of code reads as a comment.
func (c *glClient) discussions(ctx context.Context, r repoRef, iid int) ([]comment, []thread) {
	var ds []struct {
		Notes []struct {
			Author     *glUser   `json:"author"`
			Body       string    `json:"body"`
			CreatedAt  time.Time `json:"created_at"`
			System     bool      `json:"system"`
			Resolvable bool      `json:"resolvable"`
			Resolved   bool      `json:"resolved"`
			Position   *struct {
				NewPath string `json:"new_path"`
				NewLine int    `json:"new_line"`
			} `json:"position"`
		} `json:"notes"`
	}
	if _, err := c.call(ctx, "GET", mrPath(r, iid)+"/discussions", url.Values{"per_page": {"100"}}, nil, &ds); err != nil {
		return nil, nil
	}
	var comments []comment
	var threads []thread
	for _, d := range ds {
		if len(d.Notes) == 0 || d.Notes[0].System {
			continue
		}
		first := d.Notes[0]
		if first.Position == nil {
			for _, n := range d.Notes {
				if !n.System {
					comments = append(comments, comment{Author: glActor(n.Author), Body: n.Body, CreatedAt: n.CreatedAt})
				}
			}
			continue
		}
		t := thread{IsResolved: first.Resolved, Path: first.Position.NewPath, Line: first.Position.NewLine}
		for _, n := range d.Notes {
			if !n.System && len(t.Comments.Nodes) < 4 {
				t.Comments.Nodes = append(t.Comments.Nodes, comment{Author: glActor(n.Author), Body: n.Body, CreatedAt: n.CreatedAt})
			}
		}
		threads = append(threads, t)
	}
	return comments, threads
}

func glActor(u *glUser) *actor {
	if u == nil {
		return nil
	}
	return &actor{u.Username}
}

// jobs are the head pipeline's jobs as checks: one allowed to fail isn't
// required.
func (c *glClient) jobs(ctx context.Context, p *glPipeline) []check {
	if p == nil {
		return nil
	}
	var js []struct {
		Name         string `json:"name"`
		Status       string `json:"status"`
		AllowFailure bool   `json:"allow_failure"`
		WebURL       string `json:"web_url"`
	}
	path := fmt.Sprintf("/projects/%d/pipelines/%d/jobs", p.ProjectID, p.ID)
	if _, err := c.call(ctx, "GET", path, url.Values{"per_page": {"100"}}, nil, &js); err != nil {
		return nil
	}
	out := make([]check, 0, len(js))
	for _, j := range js {
		ch := check{Typename: "CheckRun", Name: j.Name, Status: "COMPLETED", IsRequired: !j.AllowFailure, DetailsURL: j.WebURL}
		switch j.Status {
		case "success":
			ch.Conclusion = "SUCCESS"
		case "failed", "canceled":
			ch.Conclusion = "FAILURE"
			if j.AllowFailure {
				ch.Conclusion = "NEUTRAL"
			}
		case "skipped", "manual":
			ch.Conclusion = "SKIPPED"
		default:
			ch.Status = "IN_PROGRESS"
		}
		out = append(out, ch)
	}
	return out
}

// lineCounts adds up the lines the MR's diffs add and remove, over its
// first 100 files.
func (c *glClient) lineCounts(ctx context.Context, r repoRef, iid int) (add, del int) {
	var diffs []struct {
		Body string `json:"diff"` // Body: sanitize keeps its line breaks
	}
	if _, err := c.call(ctx, "GET", mrPath(r, iid)+"/diffs", url.Values{"per_page": {"100"}}, nil, &diffs); err != nil {
		return 0, 0
	}
	for _, d := range diffs {
		for _, line := range strings.Split(d.Body, "\n") {
			switch {
			case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			case strings.HasPrefix(line, "+"):
				add++
			case strings.HasPrefix(line, "-"):
				del++
			}
		}
	}
	return add, del
}

// ── changes ───────────────────────────────────────────────────────────────────

// draftPrefix is how GitLab marks a draft in a title, and how it used to.
var draftPrefix = regexp.MustCompile(`(?i)^\s*(\[draft\]|\(draft\)|draft:|\[wip\]|wip:)\s*`)

// setDraft retitles the MR: GitLab's drafts are a title prefix. The title
// is read again first, so a rename since the list loaded isn't undone.
func (c *glClient) setDraft(ctx context.Context, r repoRef, iid int, draft bool) error {
	var mr glMR
	if _, err := c.call(ctx, "GET", mrPath(r, iid), nil, nil, &mr); err != nil {
		return err
	}
	title := mr.Title
	for draftPrefix.MatchString(title) {
		title = draftPrefix.ReplaceAllString(title, "")
	}
	if draft {
		title = "Draft: " + title
	}
	_, err := c.call(ctx, "PUT", mrPath(r, iid), nil, map[string]any{"title": title}, nil)
	return err
}

// merge merges now, or with auto once its pipeline passes. The head commit
// is pinned: if someone pushed since the PR screen loaded, GitLab refuses
// rather than merge code you haven't seen.
func (c *glClient) merge(ctx context.Context, d *prDetail, method string, auto bool) error {
	body := map[string]any{"sha": d.HeadRefOid, "squash": method == "SQUASH"}
	if auto {
		// auto_merge since GitLab 17.11; merge_when_pipeline_succeeds before.
		body["auto_merge"], body["merge_when_pipeline_succeeds"] = true, true
	}
	_, err := c.call(ctx, "PUT", mrPath(d.repo(), d.Number)+"/merge", nil, body, nil)
	if isStatus(err, http.StatusConflict) {
		return fmt.Errorf("!%d changed since it was loaded: look again (ctrl+r) before merging", d.Number)
	}
	return err
}

// disableAutoMerge cancels auto-merge, by the endpoint GitLab 19.5 added or,
// on an older GitLab, the one before it.
func (c *glClient) disableAutoMerge(ctx context.Context, d *prDetail) error {
	path := mrPath(d.repo(), d.Number)
	_, err := c.call(ctx, "POST", path+"/cancel_auto_merge", nil, nil, nil)
	if isStatus(err, http.StatusNotFound) {
		_, err = c.call(ctx, "POST", path+"/cancel_merge_when_pipeline_succeeds", nil, nil, nil)
	}
	return err
}

func (c *glClient) deleteBranch(ctx context.Context, r repoRef, branch string) error {
	_, err := c.call(ctx, "DELETE", projectPath(r)+"/repository/branches/"+url.PathEscape(branch), nil, nil, nil)
	return err
}

// ── the label hooks and `status` ──────────────────────────────────────────────

// prsForBranches finds each head branch's MR in one project: an open one
// first, else the latest, from the expected owner's project. Any lookup
// failing fails them all: a branch GitLab didn't answer for would read as
// having no MR.
func (c *glClient) prsForBranches(ctx context.Context, r repoRef, heads []branchHead) (map[branchHead]prStatus, error) {
	found := make([]*prStatus, len(heads))
	errs := make([]error, len(heads))
	each(len(heads), func(i int) {
		q := url.Values{"source_branch": {heads[i].Branch}, "state": {"all"}, "order_by": {"updated_at"}, "per_page": {"5"}}
		var mrs []glMR
		if _, errs[i] = c.call(ctx, "GET", projectPath(r)+"/merge_requests", q, nil, &mrs); errs[i] != nil {
			return
		}
		nodes := make([]prStatus, len(mrs))
		for j, mr := range mrs {
			nodes[j] = c.status(ctx, mr)
		}
		s, ok := pickPR(nodes, heads[i].Owner)
		if !ok {
			return
		}
		// A list's MRs come without their pipeline: the one picked, in full.
		mr, err := c.mr(ctx, r, s.Number)
		if errs[i] = err; err == nil {
			s = c.status(ctx, mr)
			found[i] = &s
		}
	})
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	out := map[branchHead]prStatus{}
	for i, s := range found {
		if s != nil {
			out[heads[i]] = *s
		}
	}
	return out, nil
}

// prsByNumber is the status of MRs in one project. One that isn't there is
// left out.
func (c *glClient) prsByNumber(ctx context.Context, r repoRef, numbers []int) (map[int]prStatus, error) {
	found := make([]*prStatus, len(numbers))
	errs := make([]error, len(numbers))
	each(len(numbers), func(i int) {
		mr, err := c.mr(ctx, r, numbers[i])
		switch {
		case isStatus(err, http.StatusNotFound):
		case err != nil:
			errs[i] = err
		default:
			s := c.status(ctx, mr)
			found[i] = &s
		}
	})
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	out := map[int]prStatus{}
	for i, s := range found {
		if s != nil {
			out[numbers[i]] = *s
		}
	}
	return out, nil
}

func (c *glClient) viewer(ctx context.Context) (string, error) {
	var u glUser
	_, err := c.call(ctx, "GET", "/user", nil, nil, &u)
	return u.Username, err
}

// reposPage is one page of the projects you're a member of, most recently
// active first.
func (c *glClient) reposPage(ctx context.Context, cursor string) ([]repoInfo, string, error) {
	q := url.Values{"membership": {"true"}, "simple": {"true"}, "archived": {"false"},
		"order_by": {"last_activity_at"}, "per_page": {"100"}}
	if cursor != "" {
		q.Set("page", cursor)
	}
	var ps []struct {
		Path         string    `json:"path_with_namespace"`
		LastActivity time.Time `json:"last_activity_at"`
	}
	h, err := c.call(ctx, "GET", "/projects", q, nil, &ps)
	if err != nil {
		return nil, "", err
	}
	var out []repoInfo
	for _, p := range ps {
		if owner, name, ok := splitRepoPath(p.Path, true); ok {
			out = append(out, repoInfo{repoRef{Host: c.host, Owner: owner, Name: name}, p.LastActivity})
		}
	}
	return out, h.Get("X-Next-Page"), nil
}

// ── the source ────────────────────────────────────────────────────────────────

// gitlabSource talks to every configured host, one client per host.
type gitlabSource struct {
	cfg     config
	repo    *repoRef
	mu      sync.Mutex
	clients map[string]*glClient
}

func newGitLabSource(cfg config, repo *repoRef) *gitlabSource {
	return &gitlabSource{cfg: cfg, repo: repo, clients: map[string]*glClient{}}
}

func (s *gitlabSource) client(host string) *glClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[host]
	if !ok {
		c = newGLClient(host)
		s.clients[host] = c
	}
	return c
}

// list: GitLab has no list of the MRs waiting on your review, so there's no
// Review tab (caps); Mine is every host's MRs of yours, the repo tab one
// project's.
func (s *gitlabSource) list(ctx context.Context, t tab, cursor string) (page, error) {
	switch t {
	case tabRepo:
		if s.repo == nil {
			return page{}, nil
		}
		prs, next, err := s.client(s.repo.Host).mrPage(ctx, projectPath(*s.repo)+"/merge_requests", url.Values{}, cursor)
		return page{prs, next}, err
	case tabMine:
		return fanOut(s.cfg.Hosts, t, cursor, func(host, after string) ([]pullRequest, string, error) {
			q := url.Values{"scope": {"created_by_me"}, "non_archived": {"true"}}
			return s.client(host).mrPage(ctx, "/merge_requests", q, after)
		})
	}
	return page{}, nil
}

func (s *gitlabSource) detail(ctx context.Context, pr pullRequest) (*prDetail, error) {
	return s.client(pr.Host).detail(ctx, pr)
}

func (s *gitlabSource) byNumber(ctx context.Context, r repoRef, number int) (pullRequest, error) {
	c := s.client(r.Host)
	mr, err := c.mr(ctx, r, number)
	if isStatus(err, http.StatusNotFound) {
		err = fmt.Errorf("%s!%d isn't there, or isn't yours to see", r, number)
	}
	if err != nil {
		return pullRequest{}, err
	}
	return c.pr(ctx, mr), nil
}

func (s *gitlabSource) setDraft(ctx context.Context, pr pullRequest, draft bool) error {
	return s.client(pr.Host).setDraft(ctx, pr.repo(), pr.Number, draft)
}

func (s *gitlabSource) merge(ctx context.Context, d *prDetail, method string, auto bool) error {
	return s.client(d.Host).merge(ctx, d, method, auto)
}

func (s *gitlabSource) disableAutoMerge(ctx context.Context, d *prDetail) error {
	return s.client(d.Host).disableAutoMerge(ctx, d)
}

func (s *gitlabSource) deleteBranch(ctx context.Context, d *prDetail) error {
	if d.HeadRef == nil {
		return errors.New("the branch is already gone")
	}
	return s.client(d.Host).deleteBranch(ctx, d.repo(), d.HeadRef.ID)
}

func (s *gitlabSource) repos(ctx context.Context, cursor string) ([]repoInfo, string, error) {
	return pageHosts(s.cfg.Hosts, cursor, func(host, after string) ([]repoInfo, string, error) {
		return s.client(host).reposPage(ctx, after)
	})
}

func (s *gitlabSource) caps() caps { return caps{reviewRequested: false, autoMerge: true} }
