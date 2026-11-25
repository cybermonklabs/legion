// check_states_needed_test.go holds the settler to read GitHub about a candidate former head only
// where a `before_merge` check needs its state: a candidate whose results are all of a check of
// another `when` is kept, unread, and holds no obligation and no test handoff, since no state the
// settler publishes is computed there. The rig is check_states_test.go's.
package api

import (
	"net/http"
	"testing"
	"time"
)

// TestACandidateNoBeforeMergeCheckNeedsHoldsNoHandoff: ACME-1's Verification document holds k9,
// an `after_merge` check, alone, or beside k1, a `before_merge` check; the tester records k9
// passing at H1 while #70 stands at H2, so H1 is #70's candidate, and GitHub fails every read of
// #70's commits. No `before_merge` check has a result at H1, so the settles ask GitHub nothing
// about #70, keep no mark a test handoff waits for, and the handoff is recorded: with k9 alone the
// set of checks at H2 is empty, which is the pass a testing step with no check takes. Once k9 is a
// `before_merge` check, which owes the issue a settle as any change of a check's words does, H1 is
// read: GitHub's answer makes it a former head of #70 and a state of k9 is published there.
func TestACandidateNoBeforeMergeCheckNeedsHoldsNoHandoff(t *testing.T) {
	afterMerge := checkStatesBlock("k9", `when="after_merge" `+ciCheckOrdinary, "Production answers at the merged commit.")
	beforeMerge := checkStatesBlock("k9", ciCheckOrdinary, "Production answers at the merged commit.")
	ordinary := checkStatesBlock("k1", ciCheckOrdinary, "The server records a result against a check.") + "\n"
	for _, story := range []struct {
		name, before string
		set          []string
	}{
		{name: "an after_merge check alone", before: "", set: []string{}},
		{name: "an after_merge check beside a before_merge check", before: ordinary, set: []string{"k1"}},
	} {
		t.Run(story.name, func(t *testing.T) {
			rig := newCheckStatesRig(t, story.before+afterMerge)
			eachHeadResult(t, rig.handler, rig.issue, "k9", "tester", "pass", eachHeadH1)
			upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
			if got := candidatesOf(t, rig); len(got) != 1 || got[eachHeadH1] {
				t.Fatalf("#70 holds the former heads %v, want H1 unverified: this test holds nothing", got)
			}
			rig.github.failPullCommits(http.StatusBadGateway)
			settleOwed(t, rig)
			if commits, pulls := rig.github.pullCommitReadsOf(70), rig.github.pullReadsOf(70); commits != 0 || pulls != 0 {
				t.Fatalf("the settles read #70's commits %d times and #70 itself %d times, want neither: no before_merge check needs a state at H1", commits, pulls)
			}
			if marks := fenceMarksOf(t, rig, rig.issue); len(marks) != 0 {
				t.Fatalf("%s keeps the marks %v a test handoff waits for, want none: no before_merge check needs a state at H1", rig.issue, marks)
			}
			row, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, eachHeadH2, "")
			if !held || !sameStringSet(row.checks, story.set) {
				t.Fatalf("the set of checks published at H2 is %v (held %v), want %v", row.checks, held, story.set)
			}
			rig.served.checkStatesSchedule.fenceWait = time.Second
			wantHandoff(t, rig, "while the only candidate is one no before_merge check needs", rig.issue, http.StatusCreated)

			rig.github.failPullCommits(0)
			rig.github.setPullCommits(70, eachHeadH1, eachHeadH2)
			uploadDocument(t, rig.handler, "/api/v1/issues/"+rig.issue, "verification.md", story.before+beforeMerge)
			settleEveryMark(t, rig)
			if got := candidatesOf(t, rig); !got[eachHeadH1] {
				t.Fatalf("#70 holds the former heads %v once k9 is a before_merge check, want H1 verified: its read comes once a check needs it", got)
			}
			if got := stateAt(t, rig, rig.issue, eachHeadH1, "k9"); got == "" {
				t.Fatal("no state of k9 is published at H1 once k9 is a before_merge check and GitHub names H1 among #70's commits")
			}
		})
	}
}
