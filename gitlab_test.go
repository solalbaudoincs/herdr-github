package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeGitLab serves the REST API from h, with a token that works, and makes
// GitLab the forge for the test.
func fakeGitLab(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		h(w, r)
	}))
	oldE, oldT, oldF := glEndpointFn, tokenFn, theForge
	glEndpointFn = func(string) string { return srv.URL }
	theForge = gitlabForge
	tokenFn = func(context.Context, string) (string, error) { return "tok", nil }
	t.Cleanup(func() {
		srv.Close()
		glEndpointFn, tokenFn, theForge = oldE, oldT, oldF
	})
}

func send(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

func mrJSON(iid int, extra map[string]any) map[string]any {
	m := map[string]any{
		"id": 1000 + iid, "iid": iid, "title": "t", "state": "opened", "draft": false,
		"web_url":    fmt.Sprintf("https://gitlab.com/grp/sub/proj/-/merge_requests/%d", iid),
		"updated_at": "2026-09-20T10:00:00Z", "author": map[string]any{"username": "ana"},
		"source_branch": "feat", "target_branch": "main", "sha": "abc123",
		"source_project_id": 7, "target_project_id": 7, "detailed_merge_status": "mergeable",
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

var glRepo = repoRef{"gitlab.com", "grp/sub", "proj"}

func TestGitLabMinePagesByNextPage(t *testing.T) {
	fakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/merge_requests" || q.Get("scope") != "created_by_me" || q.Get("state") != "opened" {
			t.Errorf("asked %s", r.URL)
		}
		if q.Get("page") == "" {
			w.Header().Set("X-Next-Page", "2")
			send(w, []any{mrJSON(1, nil)})
			return
		}
		if q.Get("page") != "2" {
			t.Errorf("page = %q", q.Get("page"))
		}
		send(w, []any{mrJSON(2, nil)})
	})
	s := newGitLabSource(withDefaults(config{Forge: "gitlab"}), nil)
	p, err := s.list(context.Background(), tabMine, "")
	if err != nil || len(p.prs) != 1 || p.next == "" {
		t.Fatalf("first page: %+v %v", p, err)
	}
	p2, err := s.list(context.Background(), tabMine, p.next)
	if err != nil || len(p2.prs) != 1 || p2.prs[0].Number != 2 || p2.next != "" {
		t.Fatalf("second page: %+v %v", p2, err)
	}
	if r, _ := s.list(context.Background(), tabReview, ""); len(r.prs) != 0 {
		t.Fatal("GitLab has no Review list")
	}
}

func TestGitLabRepoTabAsksTheProject(t *testing.T) {
	fakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/projects/grp%2Fsub%2Fproj/merge_requests" {
			t.Errorf("path %s", r.URL.EscapedPath())
		}
		send(w, []any{})
	})
	r := glRepo
	if _, err := newGitLabSource(withDefaults(config{Forge: "gitlab"}), &r).list(context.Background(), tabRepo, ""); err != nil {
		t.Fatal(err)
	}
}

func TestGitLabMRMapsOntoAPullRequest(t *testing.T) {
	fakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/merge_requests":
			send(w, []any{
				mrJSON(5, map[string]any{
					"title": "Draft: fix \x1b]52;c;evil\x07it", "draft": true, "source_project_id": 9,
					"detailed_merge_status": "conflict", "merge_when_pipeline_succeeds": true, "squash": true,
					"labels": []any{map[string]any{"name": "bug", "color": "#ff0000"}},
				}),
				mrJSON(6, map[string]any{"state": "merged", "detailed_merge_status": "requested_changes", "labels": []any{"plain"}}),
			})
		case "/projects/9":
			send(w, map[string]any{"path_with_namespace": "alice/proj"})
		default:
			t.Errorf("asked %s", r.URL)
		}
	})
	p, err := newGitLabSource(withDefaults(config{Forge: "gitlab"}), nil).list(context.Background(), tabMine, "")
	if err != nil || len(p.prs) != 2 {
		t.Fatalf("%+v %v", p, err)
	}
	byNum := map[int]pullRequest{}
	for _, pr := range p.prs {
		byNum[pr.Number] = pr
	}
	a, b := byNum[5], byNum[6]
	if a.repo() != glRepo || a.key() != "gitlab.com/grp/sub/proj#5" {
		t.Errorf("repo %+v key %s", a.repo(), a.key())
	}
	if strings.ContainsRune(a.Title, 0x1b) || !a.IsDraft || a.State != "OPEN" || a.author() != "ana" {
		t.Errorf("row %+v", a)
	}
	if !a.IsCrossRepository || a.headOwner() != "alice" || a.HeadRepository.NameWithOwner != "alice/proj" {
		t.Errorf("fork %+v %+v", a.HeadRepositoryOwner, a.HeadRepository)
	}
	if a.Mergeable != "CONFLICTING" || a.AutoMergeRequest == nil || a.AutoMergeRequest.MergeMethod != "SQUASH" {
		t.Errorf("merge state %q %+v", a.Mergeable, a.AutoMergeRequest)
	}
	if len(a.Labels.Nodes) != 1 || a.Labels.Nodes[0] != (label{"bug", "ff0000"}) {
		t.Errorf("labels %+v", a.Labels.Nodes)
	}
	if b.State != "MERGED" || b.ReviewDecision != "CHANGES_REQUESTED" || b.IsCrossRepository || b.Labels.Nodes[0].Name != "plain" {
		t.Errorf("second %+v", b)
	}
}

func TestGitLabDetail(t *testing.T) {
	fakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		base := "/projects/grp/sub/proj"
		switch r.URL.Path {
		case base + "/merge_requests/5":
			send(w, mrJSON(5, map[string]any{
				"description": "line one\nline two", "changes_count": "3", "user": map[string]any{"can_merge": true},
				"detailed_merge_status": "ci_still_running",
				"head_pipeline":         map[string]any{"id": 77, "project_id": 7, "status": "running"},
			}))
		case base:
			send(w, map[string]any{"squash_option": "default_on"})
		case base + "/merge_requests/5/approvals":
			send(w, map[string]any{"approved": true, "approved_by": []any{map[string]any{"user": map[string]any{"username": "bo"}}}})
		case base + "/merge_requests/5/reviewers":
			send(w, []any{
				map[string]any{"user": map[string]any{"username": "bo"}, "state": "unreviewed"},
				map[string]any{"user": map[string]any{"username": "cy"}, "state": "unreviewed"},
			})
		case base + "/merge_requests/5/discussions":
			send(w, []any{
				map[string]any{"notes": []any{map[string]any{"author": map[string]any{"username": "cy"}, "body": "hi", "created_at": "2026-09-20T10:00:00Z"}}},
				map[string]any{"notes": []any{map[string]any{"system": true, "body": "added a commit"}}},
				map[string]any{"notes": []any{map[string]any{"author": map[string]any{"username": "bo"}, "body": "nit", "resolvable": true, "resolved": false,
					"position": map[string]any{"new_path": "a.go", "new_line": 4}}}},
			})
		case "/projects/7/pipelines/77/jobs":
			send(w, []any{
				map[string]any{"name": "test", "status": "failed"},
				map[string]any{"name": "lint", "status": "failed", "allow_failure": true},
				map[string]any{"name": "build", "status": "running"},
			})
		case base + "/merge_requests/5/diffs":
			send(w, []any{map[string]any{"diff": "--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n-x\n+y\n+z\n"}})
		default:
			t.Errorf("asked %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	s := newGitLabSource(withDefaults(config{Forge: "gitlab"}), nil)
	d, err := s.detail(context.Background(), pullRequest{Host: "gitlab.com", Number: 5, Repository: struct {
		NameWithOwner string `json:"nameWithOwner"`
	}{"grp/sub/proj"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Body != "line one\nline two" || d.HeadRefOid != "abc123" || !d.ViewerCanMerge || d.ChangedFiles != 3 {
		t.Errorf("detail %+v", d)
	}
	if d.MergeStateStatus != "BLOCKED" || d.checks() != "PENDING" || d.ReviewDecision != "APPROVED" {
		t.Errorf("state %q checks %q review %q", d.MergeStateStatus, d.checks(), d.ReviewDecision)
	}
	if got := d.RepoSettings.methods(); len(got) != 2 || d.RepoSettings.ViewerDefaultMergeMethod != "SQUASH" || !d.RepoSettings.AutoMergeAllowed {
		t.Errorf("settings %+v", d.RepoSettings)
	}
	if d.HeadRef == nil || d.HeadRef.ID != "feat" || d.Additions != 2 || d.Deletions != 1 {
		t.Errorf("head %+v +%d -%d", d.HeadRef, d.Additions, d.Deletions)
	}
	want := []reviewer{{"bo", "APPROVED"}, {"cy", "PENDING"}}
	if fmt.Sprint(d.Reviewers) != fmt.Sprint(want) {
		t.Errorf("reviewers %v", d.Reviewers)
	}
	if len(d.Comments) != 1 || d.Comments[0].Body != "hi" || len(d.Threads) != 1 || d.Threads[0].Path != "a.go" || d.Threads[0].Line != 4 {
		t.Errorf("comments %+v threads %+v", d.Comments, d.Threads)
	}
	failing, pending := requiredCounts(d.Checks)
	if len(d.Checks) != 3 || failing != 1 || pending != 1 || d.Checks[0].name() != "test" {
		t.Errorf("checks %+v", d.Checks)
	}
}

func TestGitLabMergePinsTheHead(t *testing.T) {
	var got []map[string]any
	fakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/projects/grp/sub/proj/merge_requests/5/merge" {
			t.Errorf("%s %s", r.Method, r.URL)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		if len(got) == 3 {
			w.WriteHeader(http.StatusConflict)
			send(w, map[string]any{"message": "SHA does not match HEAD of source branch"})
			return
		}
		send(w, mrJSON(5, nil))
	})
	s := newGitLabSource(withDefaults(config{Forge: "gitlab"}), nil)
	d := &prDetail{pullRequest: pullRequest{Host: "gitlab.com", Number: 5}, HeadRefOid: "abc123"}
	d.Repository.NameWithOwner = "grp/sub/proj"
	if err := s.merge(context.Background(), d, "SQUASH", false); err != nil {
		t.Fatal(err)
	}
	if err := s.merge(context.Background(), d, "MERGE", true); err != nil {
		t.Fatal(err)
	}
	if got[0]["sha"] != "abc123" || got[0]["squash"] != true || got[0]["auto_merge"] != nil {
		t.Errorf("merge now %v", got[0])
	}
	if got[1]["sha"] != "abc123" || got[1]["squash"] != false || got[1]["auto_merge"] != true || got[1]["merge_when_pipeline_succeeds"] != true {
		t.Errorf("auto-merge %v", got[1])
	}
	if err := s.merge(context.Background(), d, "MERGE", false); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Errorf("a moved head: %v", err)
	}
}

func TestGitLabDraftRewritesTheTitle(t *testing.T) {
	title := "Draft: [WIP] fix it"
	var mu sync.Mutex
	fakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == "PUT" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			title = body["title"]
		}
		send(w, mrJSON(5, map[string]any{"title": title}))
	})
	s := newGitLabSource(withDefaults(config{Forge: "gitlab"}), nil)
	pr := pullRequest{Host: "gitlab.com", Number: 5}
	pr.Repository.NameWithOwner = "grp/sub/proj"
	if err := s.setDraft(context.Background(), pr, false); err != nil || title != "fix it" {
		t.Fatalf("ready: %q %v", title, err)
	}
	if err := s.setDraft(context.Background(), pr, true); err != nil || title != "Draft: fix it" {
		t.Fatalf("draft: %q %v", title, err)
	}
}

func TestGitLabCancelAutoMergeFallsBack(t *testing.T) {
	var asked []string
	fakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/cancel_auto_merge") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		send(w, mrJSON(5, nil))
	})
	d := &prDetail{pullRequest: pullRequest{Host: "gitlab.com", Number: 5}}
	d.Repository.NameWithOwner = "grp/sub/proj"
	if err := newGitLabSource(withDefaults(config{Forge: "gitlab"}), nil).disableAutoMerge(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 2 || !strings.HasSuffix(asked[1], "/cancel_merge_when_pipeline_succeeds") {
		t.Fatalf("asked %v", asked)
	}
}

func TestGitLabUnauthorizedIsSignedOut(t *testing.T) {
	fakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := newGitLabSource(withDefaults(config{Forge: "gitlab"}), nil).list(context.Background(), tabMine, "")
	if !errors.Is(err, errSignedOut) {
		t.Fatalf("err = %v", err)
	}
}

func TestGitLabLabelLookups(t *testing.T) {
	fakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		base := "/projects/grp/sub/proj/merge_requests"
		switch r.URL.Path {
		case base:
			if r.URL.Query().Get("source_branch") != "feat" {
				send(w, []any{})
				return
			}
			send(w, []any{mrJSON(4, map[string]any{"state": "closed"}), mrJSON(5, nil)})
		case base + "/5":
			send(w, mrJSON(5, map[string]any{"head_pipeline": map[string]any{"status": "failed"}}))
		case base + "/9":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("asked %s", r.URL)
		}
	})
	c := newGLClient("gitlab.com")
	found, err := c.prsForBranches(context.Background(), glRepo, []branchHead{{"feat", "grp/sub"}, {"other", ""}})
	if err != nil || len(found) != 1 {
		t.Fatalf("%v %v", found, err)
	}
	if s := found[branchHead{"feat", "grp/sub"}]; s.Number != 5 || s.checks() != "FAILURE" {
		t.Errorf("picked %+v", s)
	}
	byNum, err := c.prsByNumber(context.Background(), glRepo, []int{5, 9})
	if err != nil || len(byNum) != 1 || byNum[5].State != "OPEN" {
		t.Fatalf("%v %v", byNum, err)
	}
}

func TestGitLabRepos(t *testing.T) {
	fakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("membership") != "true" || q.Get("order_by") != "last_activity_at" {
			t.Errorf("asked %s", r.URL)
		}
		send(w, []any{map[string]any{"path_with_namespace": "grp/sub/proj", "last_activity_at": "2026-09-20T10:00:00Z"}})
	})
	rs, next, err := newGitLabSource(withDefaults(config{Forge: "gitlab"}), nil).repos(context.Background(), "")
	if err != nil || next != "" || len(rs) != 1 || rs[0].Repo != glRepo {
		t.Fatalf("%+v %q %v", rs, next, err)
	}
}

func TestGitLabPRFromPath(t *testing.T) {
	for _, c := range []struct {
		path   string
		number int
		want   string
	}{
		{"/grp/proj/-/merge_requests/12", 12, "gitlab.com grp/proj#12"},
		{"/grp/sub/proj/-/merge_requests/12/", 12, "gitlab.com grp/sub/proj#12"},
		{"/grp/proj/-/merge_requests/13", 12, ""},
		{"/grp/proj/-/merge_requests/12/diffs", 12, ""},
		{"/grp/proj/-/issues/12", 12, ""},
		{"/grp/proj/pull/12", 12, ""},
		{"/proj/-/merge_requests/12", 12, ""},
	} {
		r, n, ok := gitlabForge.prFromPath("GitLab.com", c.path, c.number)
		got := ""
		if ok {
			got = fmt.Sprintf("%s %s/%s#%d", r.Host, r.Owner, r.Name, n)
		}
		if got != c.want {
			t.Errorf("prFromPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
	if gitlabForge.prHeadRef(7) != "refs/merge-requests/7/head" {
		t.Fatal(gitlabForge.prHeadRef(7))
	}
}

func TestGitLabNestedPaths(t *testing.T) {
	old := theForge
	t.Cleanup(func() { theForge = old })
	if _, ok := parseRemote("git@gitlab.com:grp/sub/proj.git"); ok {
		t.Fatal("GitHub has no nested paths")
	}
	theForge = gitlabForge
	r, ok := parseRemote("git@gitlab.com:grp/sub/proj.git")
	if !ok || r != glRepo {
		t.Fatalf("%+v", r)
	}
	if r, ok := typedRepo("gitlab.example.com/grp/sub/proj", "gitlab.com"); !ok || r.Host != "gitlab.example.com" || r.Owner != "grp/sub" {
		t.Fatalf("typed %+v", r)
	}
	if r, ok := parseRepoKey("gitlab.com/grp/sub/proj"); !ok || r != glRepo {
		t.Fatalf("key %+v", r)
	}
}

// GitLab has no review-requested list: its picker has no Review tab.
func TestGitLabHasNoReviewTab(t *testing.T) {
	cfg := withDefaults(config{Forge: "gitlab"})
	m := newModel(nil, cfg, newGitLabSource(cfg, nil), "", nil)
	m.width, m.height = 110, 34
	_, cells := m.tabBar()
	for _, c := range cells {
		if c.tab == tabReview {
			t.Fatalf("the Review tab shows: %s", plain(m))
		}
	}
	if len(cells) != 2 {
		t.Fatalf("cells %v", cells)
	}
}

func TestConfigChoosesTheForge(t *testing.T) {
	path := filepath.Join(configDir(), "config.json")
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(path) })
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"forge": "gitlab"}`)
	cfg, err := loadConfig()
	if err != nil || cfg.forge().name != "GitLab" || cfg.Hosts[0] != "gitlab.com" || !strings.Contains(cfg.StartPrompt, "glab mr view") {
		t.Fatalf("%+v %v", cfg, err)
	}
	write(`{}`)
	if cfg, err := loadConfig(); err != nil || cfg.forge().name != "GitHub" || cfg.Hosts[0] != "github.com" {
		t.Fatalf("%+v %v", cfg, err)
	}
	write(`{"forge": "bitbucket"}`)
	if _, err := loadConfig(); err == nil {
		t.Fatal("an unknown forge is accepted")
	}
}
