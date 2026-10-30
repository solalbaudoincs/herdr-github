package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestGithubPRFromPath(t *testing.T) {
	for _, c := range []struct {
		path   string
		number int
		want   string // "" for no match
	}{
		{"/o/r/pull/12", 12, "github.com/o/r#12"},
		{"/o/r/pull/12/", 12, "github.com/o/r#12"},
		{"/o/r/issues/12", 12, ""},
		{"/o/r/pull/13", 12, ""}, // the number the URL doesn't say
		{"/o/r/pull/abc", 12, ""},
		{"/o/r", 12, ""},
		{"/o/r/pull/12/files", 12, ""},
		{"/pull/12", 12, ""},
	} {
		r, n, ok := theForge.prFromPath("github.com", c.path, c.number)
		got := ""
		if ok {
			got = fmt.Sprintf("%s/%s/%s#%d", r.Host, r.Owner, r.Name, n)
		}
		if got != c.want {
			t.Errorf("prFromPath(%q, %d) = %q, want %q", c.path, c.number, got, c.want)
		}
	}
}

func TestIsDefaultHost(t *testing.T) {
	if !theForge.isDefaultHost("GitHub.com") || theForge.isDefaultHost("ghe.example.com") || theForge.isDefaultHost("") {
		t.Fatal("isDefaultHost")
	}
}

// A clone runs with the environment this process has, plus the forge's own
// additions: gh needs PATH, HOME and any GH_* token to work at all.
func TestCloneCmdInheritsTheEnvironment(t *testing.T) {
	cmd := theForge.cloneCmd(context.Background(), "github.com", repoRef{"github.com", "o", "r"}, "/tmp/dest")
	for _, want := range []string{"PATH=", "HOME="} {
		found := false
		for _, kv := range cmd.Env {
			if strings.HasPrefix(kv, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("cloneCmd's Env lacks %s: %v", want, cmd.Env)
		}
	}
	for _, kv := range cmd.Env {
		if kv == "GH_HOST=github.com" {
			return
		}
	}
	t.Errorf("cloneCmd's Env lacks GH_HOST: %v", cmd.Env)
}
