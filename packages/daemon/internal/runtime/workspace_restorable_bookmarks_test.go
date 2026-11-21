package runtime

import (
	"maps"
	"testing"
)

// A restore sets every bookmark of a copy but the issue's branch and its rescue branch, and the
// answer names each of those two the copy held, with its commit, for the line an operator reads.
// Every other bookmark crosses: another issue's rescue branch, and an older shim's per-pod rescue
// branch `legion/<KEY>-rescue-<pod>-<n>`, which names the pod the copy came off and so is a branch
// no rescue of the pod restoring it pushes. The copy's own map is never written: the caller's
// copy keeps every bookmark it held.
func TestRestorableBookmarksWithholdsTheIssuesBranchAndItsRescueBranch(t *testing.T) {
	const issue = "LEGION-208"
	const (
		branch = "legion/LEGION-208"
		rescue = "legion/LEGION-208-rescue"
		side   = "side"
		perPod = "legion/LEGION-208-rescue-9d3f0c7a-1"
		other  = "legion/LEGION-209-rescue"
	)
	commits := map[string]string{
		branch: "dffe88ae2f793e13436cef3921adc988433fbd4e",
		rescue: "0c4e5f6a7b8c9d0e1f2a3b4c5d6e7f8091a2b3c4",
		side:   "da4f9f492e9f8349389c243743e0a000cd1c676f",
		perPod: "5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f7081",
		other:  "7b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f708192",
	}
	of := func(names ...string) map[string]string {
		m := map[string]string{}
		for _, name := range names {
			m[name] = commits[name]
		}
		return m
	}
	for _, c := range []struct {
		name                         string
		bookmarks                    map[string]string
		wantRestorable, wantWithheld map[string]string
	}{
		{"both", of(branch, rescue, side), of(side), of(branch, rescue)},
		{"the issue's branch alone", of(branch, side), of(side), of(branch)},
		{"the rescue branch alone", of(rescue, side), of(side), of(rescue)},
		{"neither, beside a per-pod rescue branch and another issue's", of(side, perPod, other), of(side, perPod, other), nil},
		{"only the two", of(branch, rescue), of(), of(branch, rescue)},
		{"no bookmark at all", nil, nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := maps.Clone(c.bookmarks)
			restorable, withheld := RestorableBookmarks(issue, c.bookmarks)
			if !maps.Equal(restorable, c.wantRestorable) || !maps.Equal(withheld, c.wantWithheld) {
				t.Fatalf("RestorableBookmarks(%v) = %v, withheld %v; want %v, withheld %v",
					c.bookmarks, restorable, withheld, c.wantRestorable, c.wantWithheld)
			}
			if !maps.Equal(c.bookmarks, before) {
				t.Fatalf("RestorableBookmarks wrote the copy's own bookmarks: %v, were %v", c.bookmarks, before)
			}
		})
	}
}
