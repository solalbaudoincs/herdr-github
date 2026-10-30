package main

import (
	"strings"
	"testing"
)

// The signed-out screen says exactly what main said for GitHub, through the
// forge's own words: the command you'd run yourself, and gh's token.
func TestSignedOutScreenWording(t *testing.T) {
	m := demoModel(t, tabMine)
	next, _ := m.Update(listMsg{tab: tabMine, err: &signedOutError{"github.com"}, gen: m.gen})
	m = next.(model)
	got := plain(m)
	for _, want := range []string{
		"GitHub isn't connected: gh has no working sign-in for github.com.",
		"Press enter to run gh auth login here; it opens your browser.",
		"The plugin uses gh's token and keeps none of its own.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("screen lacks %q:\n%s", want, got)
		}
	}
}
