package main

import (
	"strings"
	"testing"
)

// cappedDemo is the demo with a source that lacks a forge's capabilities:
// the TUI must hide the Review tab and the merge key without the rest
// breaking.
type cappedDemo struct {
	*demoSource
	c caps
}

func (c cappedDemo) caps() caps { return c.c }

func cappedModel(t *testing.T, c caps) model {
	t.Helper()
	d := cappedDemo{newDemoSource(), c}
	home := d.homeRepo()
	m := newModel(nil, withDefaults(config{}), d, "", &home)
	m.width, m.height = 110, 34
	return m
}

func TestNoReviewRequestedHidesTheTab(t *testing.T) {
	m := cappedModel(t, caps{autoMerge: true})
	_, cells := m.tabBar()
	for _, c := range cells {
		if c.tab == tabReview {
			t.Fatalf("the Review tab shows: %s", plain(m))
		}
	}
	// Tab cycling skips it rather than landing on an empty tab.
	if next := m.cycleTab(1); next == tabReview {
		t.Fatal("cycling landed on the hidden tab")
	}
	full := cappedModel(t, caps{reviewRequested: true, autoMerge: true})
	_, cells = full.tabBar()
	if len(cells) != 3 {
		t.Fatalf("a forge with everything keeps three tabs: %v", cells)
	}
}

func TestNoAutoMergeHidesTheKey(t *testing.T) {
	m := cappedModel(t, caps{reviewRequested: true})
	pr := pullRequest{Number: 1, State: "OPEN"}
	m.cur = &pr
	if hs := m.detailFooter(); strings.Contains(hintsText(hs), "merge") {
		t.Fatalf("the footer offers a merge key: %v", hs)
	}
	full := cappedModel(t, caps{reviewRequested: true, autoMerge: true})
	full.cur = &pr
	if hs := full.detailFooter(); !strings.Contains(hintsText(hs), "merge") {
		t.Fatalf("a forge with auto-merge keeps the key: %v", hs)
	}
}

func hintsText(hs []hint) string {
	var b strings.Builder
	for _, h := range hs {
		b.WriteString(h.label + " ")
	}
	return b.String()
}
