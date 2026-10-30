# GitLab source (mrolafsson/herdr-github#6)

Builds on the forge-agnostic refactor (#5): a `gitlabForge` descriptor and a
`gitlabSource` behind the `source` interface, over GitLab REST v4, stdlib
only. GitHub stays the default and its behavior is unchanged.

## Switch

Runtime, from config: `"forge": "gitlab"` in config.json (default `github`).
`run` calls `useForge` before anything talks to a forge, which sets
`theForge` and the `tokenFn` seam. The demo always runs on `githubForge` (it
impersonates GitHub). The descriptor gains what the TUI and the label hooks
build per forge: `newSource`, `prsForBranches`, `prsByNumber`, `viewer` (for
`status`), `startPrompt` (the default prompt names the CLI), and
`nestedPaths` (GitLab namespaces nest: `group/sub/project`).

Call sites that hard-coded GitHub now go through the descriptor:
`newGitHubSource` in tui.go/repopicker.go, `newClient(...).prsForBranches` in
labels.go, `.prsByNumber` in agents.go, the `status` command. A few
user-facing strings that said "GitHub" use `theForge.name` (same text on
GitHub).

## Shared, extracted (no behavior change for GitHub)

- `fanOut`: the Mine/Review multi-host cursor (JSON host → cursor) and the
  signed-out/partial handling, out of `githubSource.list`.
- `pageHosts`: the repo picker's "<host index>:<cursor>" paging, out of
  `githubSource.repos`.
- `pr.repo()` cuts at the last `/` (identical for `owner/name`), and
  `parseRepoKey`/`parseRemote` accept nested owners when the forge says so.

## Auth and transport (gitlab.go)

- Token: `GITLAB_TOKEN`, `GITLAB_ACCESS_TOKEN`, `OAUTH_TOKEN` (when
  `GITLAB_HOST` is unset or names this host), then
  `glab config get token --host <host>`. `glab auth token` does not exist
  (gitlab-org/cli#8231, closed won't-do); `config get token` reads the
  keyring or config file and prints just the value.
- Header `Authorization: Bearer <token>`: GitLab accepts PATs and OAuth
  tokens (what `glab auth login --web` stores) both that way.
- Base `https://<host>/api/v4` (seam `glEndpointFn`). Project ids are the
  URL-encoded path (`group%2Fsub%2Fproj`).
- 401 → `signedOutError`; other non-2xx → `GitLab: HTTP n: <message>`.
  Every decoded value goes through `sanitize`; `description` and note
  `body` decode into fields named `Body`, so they keep their line breaks.
- Pages: `page`/`per_page`, next from `X-Next-Page`; the cursor is that page
  number.

## Mapping (GitLab JSON → shared structs)

| pullRequest / prDetail | GitLab |
|---|---|
| ID | global `id` (unused by the GitLab source) |
| Number | `iid` |
| Title, URL, UpdatedAt | `title`, `web_url`, `updated_at` |
| IsDraft | `draft` |
| State | `opened`→OPEN, `merged`→MERGED, `closed`/`locked`→CLOSED |
| Author.Login | `author.username` |
| Repository.NameWithOwner | path from `web_url` before `/-/` |
| HeadRefName, BaseRefName, HeadRefOid | `source_branch`, `target_branch`, `sha` |
| IsCrossRepository | `source_project_id != target_project_id` |
| HeadRepository, HeadRepositoryOwner | source project's `path_with_namespace` (GET /projects/:id, cached); owner = its namespace |
| Mergeable | `conflict` or `has_conflicts`→CONFLICTING, `mergeable`→MERGEABLE, else UNKNOWN |
| MergeStateStatus | `mergeable`→CLEAN, `need_rebase`→BEHIND, `conflict`→DIRTY, `draft_status`→DRAFT, checking/unchecked/preparing/approvals_syncing→UNKNOWN, any other check→BLOCKED |
| ReviewDecision | `requested_changes`→CHANGES_REQUESTED, `not_approved`→REVIEW_REQUIRED; detail: approvals `approved` with approvers→APPROVED |
| Commits rollup | `head_pipeline.status` (single MR only): success→SUCCESS, failed/canceled→FAILURE, running/pending/created/…→PENDING |
| Labels | `labels` with `with_labels_details=true`, `#` stripped from color |
| AutoMergeRequest | set when `merge_when_pipeline_succeeds`; method SQUASH when `squash` |
| Body | `description` |
| ChangedFiles, Additions, Deletions | `changes_count`; +/− counted from `/diffs` (first 300 files) |
| MaintainerCanModify | `allow_collaboration` |
| HeadRef | the source branch (deleted via DELETE /repository/branches/:branch) |
| Checks | jobs of `head_pipeline` (GET /projects/:pid/pipelines/:id/jobs): required = `!allow_failure` |
| Reviewers | /reviewers (`approved`, `requested_changes`, `unreviewed`→PENDING, else COMMENTED) + /approvals `approved_by` |
| Comments, Threads | /discussions: individual non-system notes; resolvable diff discussions with `position.new_path/new_line` |
| RepoSettings | project `squash_option`: never→MERGE, always→SQUASH, else both (default_on prefers SQUASH); `DeleteBranchOnMerge` = `force_remove_source_branch` or `should_remove_source_branch`; auto-merge allowed |
| ViewerCanMerge | `user.can_merge` |

## Endpoints

- Mine: `GET /merge_requests?scope=created_by_me&state=opened&order_by=updated_at&non_archived=true`
- Repo: `GET /projects/:id/merge_requests?state=opened&order_by=updated_at`
- Review: none (`caps.reviewRequested=false`; the tab is hidden).
- Detail: single MR + project + approvals + reviewers + discussions + jobs + diffs.
- Draft: re-read the title, then `PUT /projects/:id/merge_requests/:iid` with `title` prefixed `Draft: ` or stripped of GitLab's draft prefixes.
- Merge: `PUT …/merge` with `sha` (pinned head), `squash`, and for auto `auto_merge` and `merge_when_pipeline_succeeds` (pre-17.11).
- Auto-merge off: `POST …/cancel_auto_merge`, falling back to `cancel_merge_when_pipeline_succeeds` on 404 (pre-19.5).
- Labels: per branch `GET /projects/:id/merge_requests?source_branch=…&state=all&order_by=updated_at`, then the picked MR in full (for its pipeline).
- Agents: per number `GET /projects/:id/merge_requests/:iid` (404 = gone).
- Repos: `GET /projects?membership=true&simple=true&archived=false&order_by=last_activity_at`.
- status: `GET /user`.

## Descriptor

`name` GitLab, `defaultHost` gitlab.com, `prFromPath` parses
`/<namespace…>/<project>/-/merge_requests/N`, `prHeadRef`
`refs/merge-requests/N/head`, clone `glab repo clone` with `GITLAB_HOST`,
login `glab auth login --hostname H --web`, methods MERGE "Merge" / SQUASH
"Squash and merge".

## Tests (gitlab_test.go)

Fake GitLab server (httptest) behind `glEndpointFn`/`tokenFn`: list paging by
`X-Next-Page`, MR→pullRequest mapping (state, draft, fork, labels, nested
path), detail mapping (checks, reviewers, threads, settings), merge pinning
(`sha`, `squash`, `auto_merge`), draft toggle title rewrite, cancel fallback,
401 → signed out, prFromPath, prHeadRef, caps (Review tab hidden through the
real TUI), config switch.

## Steps

1. Plan.
2. Descriptor seams + shared extraction (GitHub unchanged; suite green).
3. gitlab.go client + source + descriptor.
4. Config switch.
5. Tests.
6. Full checks, deslop, no-comments, push.
