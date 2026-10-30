package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// A forge is a code-hosting service: GitHub here, and the shape is ready for
// a sibling (a herdr-gitlab, say) to bring its own vocabulary. The client
// (github.go) speaks the service's API; the forge holds what the rest of the
// plugin needs to know about it: how to authenticate, how a PR's URL and
// fork refs are spelled, how clones and sign-ins happen, and what its merge
// methods are called. This binary runs as one forge; `theForge` is it, a
// variable so tests can stand another in.

type forge struct {
	// name is the service's name, as errors and notices spell it ("GitHub").
	name string
	// defaultHost is the host shown without spelling: a repo on another
	// host of the same forge says "ghe.example.com/o/r" instead of "o/r".
	defaultHost string
	// token finds the auth token for a host: the service's CLI, say.
	token func(ctx context.Context, host string) (string, error)
	// prFromPath is the repo and PR number a URL's host and path point at,
	// when they're one of this forge's PRs ("/owner/repo/pull/N").
	prFromPath func(host, path string, number int) (repoRef, int, bool)
	// prHeadRef is the ref a fork's PR head lives at in the base repo
	// ("refs/pull/N/head").
	prHeadRef func(number int) string
	// cloneCmd is the command that clones r into dest, authenticating the
	// way this forge does.
	cloneCmd func(ctx context.Context, host string, r repoRef, dest string) *exec.Cmd
	// loginCmd is the command that signs you in to host, in the popup.
	loginCmd func(host string) *exec.Cmd
	// loginText is that command as it's said on the signed-out screen and
	// prefixed to a sign-in error ("gh auth login"): what you'd run yourself.
	loginText string
	// cliName is the CLI itself, as the signed-out screen names it ("gh").
	cliName string
	// methodNames and methodVerbs spell this forge's merge methods: what
	// its API calls them (the keys) and what each is called and said to do.
	methodNames map[string]string
	methodVerbs map[string]string
	// startPrompt is the default first prompt for a new worktree's agent.
	startPrompt string
	// nestedPaths: a repo's owner can itself have slashes in it (GitLab's
	// groups and subgroups: "group/sub/project").
	nestedPaths bool
	// newSource is the picker's source of pull requests for this forge.
	newSource func(cfg config, repo *repoRef) source
	// api is what the label hooks and `status` ask a host, outside the picker.
	api func(host string) forgeAPI
}

// forgeAPI is a host's answers to the label hooks and `status`.
type forgeAPI interface {
	prsForBranches(ctx context.Context, r repoRef, heads []branchHead) (map[branchHead]prStatus, error)
	prsByNumber(ctx context.Context, r repoRef, numbers []int) (map[int]prStatus, error)
	// viewer is the login the host's token belongs to.
	viewer(ctx context.Context) (string, error)
}

// forges are the forges a config can choose, by its "forge" setting.
var forges = map[string]forge{"github": githubForge, "gitlab": gitlabForge}

// forgeNamed is the forge a config names; "" is GitHub.
func forgeNamed(name string) (forge, bool) {
	if name == "" {
		return githubForge, true
	}
	f, ok := forges[strings.ToLower(name)]
	return f, ok
}

// useForge makes f the forge this process serves.
func useForge(f forge) {
	theForge, tokenFn = f, f.token
}

// splitRepoPath reads "owner/name", or with nested paths "group/sub/name".
func splitRepoPath(path string, nested bool) (owner, name string, ok bool) {
	i := strings.LastIndex(path, "/")
	if i < 0 {
		return "", "", false
	}
	owner, name = path[:i], path[i+1:]
	if owner == "" || name == "" || (!nested && strings.Contains(owner, "/")) {
		return "", "", false
	}
	return owner, name, true
}

// theForge is the forge this process serves. Both real sources and the demo
// run on it (the demo impersonates GitHub).
var theForge = githubForge

// isDefaultHost says whether host is the forge's default host, and so needn't
// be spelled out next to a repo's name.
func (f forge) isDefaultHost(host string) bool {
	return strings.EqualFold(host, f.defaultHost)
}

var githubForge = forge{
	name:        "GitHub",
	defaultHost: "github.com",
	token:       ghToken,
	prFromPath: func(host, path string, number int) (repoRef, int, bool) {
		parts := strings.Split(strings.Trim(path, "/"), "/")
		if len(parts) != 4 || parts[2] != "pull" || parts[3] != fmt.Sprint(number) {
			return repoRef{}, 0, false
		}
		r := repoRef{Host: strings.ToLower(host), Owner: parts[0], Name: parts[1]}
		if !r.valid() {
			return repoRef{}, 0, false
		}
		return r, number, true
	},
	prHeadRef: func(number int) string { return fmt.Sprintf("refs/pull/%d/head", number) },
	cloneCmd: func(ctx context.Context, host string, r repoRef, dest string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "gh", "repo", "clone", r.String(), dest)
		cmd.Env = append(os.Environ(), "GH_HOST="+host)
		return cmd
	},
	loginCmd: func(host string) *exec.Cmd {
		return exec.Command("gh", "auth", "login", "--hostname", host, "--web")
	},
	loginText:   "gh auth login",
	cliName:     "gh",
	methodNames: map[string]string{"MERGE": "Create a merge commit", "SQUASH": "Squash and merge", "REBASE": "Rebase and merge"},
	methodVerbs: map[string]string{"MERGE": "Merge", "SQUASH": "Squash-merge", "REBASE": "Rebase-merge"},
	startPrompt: "Pull request {url} is checked out here. Read it with `gh pr view {number} --comments`, look at any failing checks, and tell me what's left to do.",
	newSource:   func(cfg config, repo *repoRef) source { return newGitHubSource(cfg, repo) },
	api:         func(host string) forgeAPI { return newClient(host) },
}
