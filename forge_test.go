package main

import (
	"fmt"
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
