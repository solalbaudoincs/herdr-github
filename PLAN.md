# Forge-agnostic refactor (no GitLab yet)

Make the GitHub-specific knowledge that lives outside `github.go` sit behind a
forge descriptor, and add capability flags the TUI consults. Zero behavior
change for GitHub and the demo; the GitLab implementation stays out of scope.

## Descriptor (`forge.go`, new)

A small struct, one instance per forge. GitHub gets `githubForge`; the demo
runs on it too (it impersonates GitHub today).

```go
type forge struct {
    name        string                                  // "GitHub": errors, toasts
    defaultHost string                                  // "github.com": shown unprefixed
    // PR URL parsing (labels.go/agents.go read transcript links):
    prFromURL   func(u *url.URL, number int) (repoRef, int, bool)
    // ref namespace for a fork PR's head (worktree.go, demo.go):
    prHeadRef   func(number int) string                 // "refs/pull/N/head"
    // fork remote URL spelling (worktree.go): generic, stays a function
    // auth token per host (github.go, main.go):
    token       func(ctx context.Context, host string) (string, error)
    // clone command for a repo with no checkout (repos.go):
    cloneCmd    func(ctx context.Context, host string, r repoRef, dest string) *exec.Cmd
    // merge-method vocabulary (detail.go):
    methodNames map[string]string                       // MERGE/SQUASH/REBASE → label
    methodVerbs map[string]string
}
```

Host matching (`knownHost`) stays on `config` — it already comes from config
and is forge-neutral. `isDefaultHost` on the descriptor replaces the
`!= "github.com"` display checks. The GraphQL endpoint (`endpointFn`) is a
GitHub API detail and stays in `github.go`.

## Capabilities (`source` interface)

```go
type caps struct { reviewRequested, autoMerge bool }
```

`source` gains `caps() caps`. Both current sources report full capability
(behavior unchanged), but the TUI consults the flags:

- `tabBar`/`switchTab`: the Review tab only appears when `reviewRequested`.
- `detail.go`: the merge menu and its keys only when `autoMerge`.

## Files

- `forge.go` (new), `forge_test.go` (new): descriptor + parsing/matching tests.
- `source.go`: `caps` type, method on `githubSource`/`demoSource`.
- `github.go`: `ghToken`, `clone`'s command, merge-method maps move behind
  `githubForge`; behavior identical.
- `agents.go`: `prFromURL` delegates to the descriptor (host check unchanged).
- `worktree.go`: `refs/pull/N/head` via descriptor; `detail.go`: method maps.
- `repos.go`, `tui.go`, `demo.go`, `main.go`: route the hard-coded bits above.
- Tests: existing suite untouched except where a test constructs a hard-coded
  path that now goes through the descriptor — assertions stay equivalent.

## Steps

1. `forge.go` + `forge_test.go`; move `ghToken`, endpoint-agnostic helpers.
2. Thread the descriptor through agents/worktree/repos/detail/demo/main.
3. Capability flags + TUI consultation.
4. `go build ./... && go vet ./... && go test ./...`, deslop + no-comments,
   push, draft PR.
