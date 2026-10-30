package main

import (
	"context"
	"fmt"
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
	// methodNames and methodVerbs spell this forge's merge methods: what
	// its API calls them (the keys) and what each is called and said to do.
	methodNames map[string]string
	methodVerbs map[string]string
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
		cmd.Env = append(cmd.Env, "GH_HOST="+host)
		return cmd
	},
	loginCmd: func(host string) *exec.Cmd {
		return exec.Command("gh", "auth", "login", "--hostname", host, "--web")
	},
	methodNames: map[string]string{"MERGE": "Create a merge commit", "SQUASH": "Squash and merge", "REBASE": "Rebase and merge"},
	methodVerbs: map[string]string{"MERGE": "Merge", "SQUASH": "Squash-merge", "REBASE": "Rebase-merge"},
}
