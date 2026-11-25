// check_states_base_revision_test.go holds the settler to the base revision of GitHub's answer
// about which commits are a pull request's own: the base branch can move with no change of its
// name, so the read compares the head and the base revision Dispatch stores for the pull request
// when the read is claimed, and the answer applies, positive or negative, only where the pull
// request still names them; an answer about revisions since stored anew applies nothing, keeps the
// candidate unverified and its obligation, and the next settle reads again. A row that stores no
// base revision is not read until a write stores one. The rig is check_states_test.go's.
package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// storeBaseRevision writes #number again at sha, open and attributed to the rig's issue as
// upsertHead writes it, with its base branch at baseSHA, as a read of the pull request that found
// the base branch there writes it ("" for a read that named none).
func storeBaseRevision(t *testing.T, rig *checkStatesRig, number int, sha, baseSHA string) {
	t.Helper()
	attributed := rig.issue
	if err := rig.served.deps.Attributor.UpsertPullRequestHead(context.Background(), rig.database.Pool, nil, delivery.PullRequestHead{
		Repo: ciJobRepository, Number: number, HeadSHA: sha, HeadRef: "legion/work", BaseRef: "main", BaseSHA: baseSHA,
		State: "open", IssueKey: &attributed, AttributionSource: "title", AttributionComplete: true, Title: "A change",
		URL: fmt.Sprintf("https://github.com/%s/pull/%d", ciJobRepository, number),
	}); err != nil {
		t.Fatalf("write #%d at %s with its base branch at %q: %v", number, sha, baseSHA, err)
	}
}

// The base revisions of the tests below.
const (
	baseRevisionA = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	baseRevisionB = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
)

// TestAMembershipAnswerAboutABaseRevisionSinceStoredAppliesNothing: the tester recorded k1 passing
// at H1, and #70 stands at H2 with its base branch, main, at one commit, so H1 is #70's candidate.
// A settle reads #70's commits and is held after GitHub answered and before it applies the
// answer. Meanwhile main moves on to another commit and Dispatch stores it, by a write of #70's
// row with the same head and the same branch name, and GitHub's list of #70's commits changes
// with it. An answer about the earlier base revision applies nothing, positive or negative: H1
// stays a candidate, its obligation stays, and the next settle reads #70 again and decides it by
// the list that holds now. The neighbours write #70's row again with the base revision unchanged,
// and the held answer applies.
//
// Positive: GitHub listed H1 before the move and lists it no more after (main took H1), so k1 is
// published nowhere where main moved, and yes at H1 where it did not. Negative: GitHub left H1 out
// before the move and lists it after, so k1 is published yes at H1 where main moved, and nothing
// where it did not.
func TestAMembershipAnswerAboutABaseRevisionSinceStoredAppliesNothing(t *testing.T) {
	for _, row := range []struct {
		name          string
		listed, moved bool
	}{
		{name: "a positive answer, the base revision unchanged", listed: true},
		{name: "a positive answer, the base revision moved since", listed: true, moved: true},
		{name: "a negative answer, the base revision unchanged"},
		{name: "a negative answer, the base revision moved since", moved: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newCheckStatesRig(t, checkStatesTwoChecks)
			eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
			storeBaseRevision(t, rig, 70, eachHeadH2, baseRevisionA)
			if got := candidatesOf(t, rig); !maps.Equal(got, map[string]bool{eachHeadH1: false}) {
				t.Fatalf("#70 holds the former heads %v, want H1 unverified: this test holds nothing", got)
			}
			before, after := []string{eachHeadH2}, []string{eachHeadH1, eachHeadH2}
			if row.listed {
				before, after = after, before
			}
			rig.github.setPullCommits(70, before...)
			rig.github.setPull(70, eachHeadH2, "main", baseRevisionA)
			entered, release := rig.github.holdPullCommitsAnswer(t, 70)
			first := settleInTheBackground(rig.settler, rig.issue)
			ciAwait(t, "the settle's read of #70's commits", entered)
			if row.moved {
				rig.github.setPullCommits(70, after...)
				rig.github.setPull(70, eachHeadH2, "main", baseRevisionB)
				storeBaseRevision(t, rig, 70, eachHeadH2, baseRevisionB)
			} else {
				storeBaseRevision(t, rig, 70, eachHeadH2, baseRevisionA)
			}
			release()
			ciAwait(t, "the held settle to end", first)
			if row.moved {
				if got := candidatesOf(t, rig); !maps.Equal(got, map[string]bool{eachHeadH1: false}) {
					t.Fatalf("#70 holds the former heads %v once the answer about the earlier base revision was applied, want H1 unverified still: that answer applies nothing", got)
				}
				if marks := fenceMarksOf(t, rig, rig.issue); len(marks) == 0 {
					t.Fatalf("%s holds no mark a handoff waits for once the answer about the earlier base revision applied nothing, want its obligation kept", rig.issue)
				}
			}
			if _, err := rig.database.Pool.Exec(context.Background(), `select check_states_owe($1)`, rig.issue); err != nil {
				t.Fatalf("owe %s a settle: %v", rig.issue, err)
			}
			settleEveryMark(t, rig)
			want := ""
			if row.listed != row.moved {
				want = model.PassStandsYes
			}
			if got := stateAt(t, rig, rig.issue, eachHeadH1, "k1"); got != want {
				t.Fatalf("k1 at H1 is published %q, want %q: an answer applies only to the base revision it was read about", got, want)
			}
		})
	}
}

// TestARowWithNoStoredBaseRevisionIsReadOnceAWriteStoresOne: #70's row holds no base revision, as
// a row a release before this one wrote, or one an envelope that named none wrote. The settle has
// no two revisions to compare, so it asks GitHub nothing about #70: H1 stays a candidate, and the
// obligation that asked for the read stays, a failure counted, read again after the backoff. Once
// a write stores the base revision, as the reconcile's read of the pull request does, the next
// settle compares the stored head with it once: GitHub names H1 there, so k1 is published yes at
// H1, and no obligation is left.
func TestARowWithNoStoredBaseRevisionIsReadOnceAWriteStoresOne(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	storeBaseRevision(t, rig, 70, eachHeadH2, "")
	if got := candidatesOf(t, rig); !maps.Equal(got, map[string]bool{eachHeadH1: false}) {
		t.Fatalf("#70 holds the former heads %v, want H1 unverified: this test holds nothing", got)
	}
	rig.github.setPullCommits(70, eachHeadH1, eachHeadH2)
	settleOwed(t, rig)
	if commits, pulls := rig.github.pullCommitReadsOf(70), rig.github.pullReadsOf(70); commits != 0 || pulls != 0 {
		t.Fatalf("the settle read #70's commits %d times and #70 itself %d times while its row stores no base revision, want neither", commits, pulls)
	}
	if got := candidatesOf(t, rig); !maps.Equal(got, map[string]bool{eachHeadH1: false}) {
		t.Fatalf("#70 holds the former heads %v while its row stores no base revision, want H1 unverified still", got)
	}
	var failures int
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select coalesce(max(failures), 0) from check_states_owed where issue_key = $1 and reads_from is not null`, rig.issue).Scan(&failures); err != nil {
		t.Fatalf("read the marks of %s: %v", rig.issue, err)
	}
	if marks := fenceMarksOf(t, rig, rig.issue); len(marks) == 0 || failures < 1 {
		t.Fatalf("%s keeps the marks %v a handoff waits for, with %d failures, want its obligation kept with a failure counted", rig.issue, marks, failures)
	}

	storeBaseRevision(t, rig, 70, eachHeadH2, ciJobBaseSHA)
	settleEveryMark(t, rig)
	if got := stateAt(t, rig, rig.issue, eachHeadH1, "k1"); got != model.PassStandsYes {
		t.Fatalf("k1 at H1 is published %q once a write stored #70's base revision, want yes", got)
	}
	if got := rig.github.pullCommitReadsOf(70); got != 1 {
		t.Fatalf("#70's commits were read %d times, want once", got)
	}
	if left := marksOf(t, rig.database, rig.issue); left != 0 {
		t.Fatalf("%s holds %d marks after the settles, want none: nothing is left to read", rig.issue, left)
	}
}

// TestTheReconcileStoresTheBaseRevisionARowOfTheReleaseBeforeLacks: #70's row holds no base
// revision, as a row the release before this one wrote, so its candidate H1 is not read and the
// test handoff waits (answers 503). The delivery reconcile, on its own timer
// (delivery.Reconcile.Run, ticking here every 100 ms in place of five minutes), re-reads each due
// open row, the least lately tried first, whatever the delivery settings say
// (ListStalePullRequestHeads, RefreshPullRequestHead): it stores the base revision GitHub names
// for #70 with the head and the issue the row had. The mark the settle kept then falls due, the
// settle compares #70's stored head with that base revision once, publishes k1 yes at H1, and the
// test handoff is recorded.
func TestTheReconcileStoresTheBaseRevisionARowOfTheReleaseBeforeLacks(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	storeBaseRevision(t, rig, 70, eachHeadH2, "")
	rig.github.setPullCommits(70, eachHeadH1, eachHeadH2)
	rig.github.setPullTitle(70, rig.issue+" A change")
	settleOwed(t, rig)
	if got := rig.github.pullCommitReadsOf(70); got != 0 {
		t.Fatalf("#70's commits were read %d times while its row stores no base revision, want none", got)
	}
	rig.served.checkStatesSchedule.fenceWait = time.Second
	wantHandoff(t, rig, "while #70's row stores no base revision and H1 is unread", rig.issue, http.StatusServiceUnavailable)

	ctx, stop := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		delivery.NewReconcile(rig.database.Pool, rig.served.deps.GitHub, nil, rig.served.deps.Attributor).TickEvery(100*time.Millisecond).
			Run(ctx, storetest.Leases(t, rig.database.Pool))
	}()
	var baseSHA, head, issue string
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if err := rig.database.Pool.QueryRow(context.Background(), `
			select coalesce(base_sha, ''), head_sha, coalesce(issue_key, '') from pull_request_heads where repo = $1 and number = 70`,
			ciJobRepository).Scan(&baseSHA, &head, &issue); err != nil {
			t.Fatalf("read #70's row: %v", err)
		}
		if baseSHA != "" || time.Now().After(deadline) {
			break
		}
	}
	stop()
	ciAwait(t, "the reconcile to stop", ran)
	if baseSHA != ciJobBaseSHA || head != eachHeadH2 || issue != rig.issue {
		t.Fatalf("#70's row reads base %q, head %s, issue %q once the reconcile ran, want base %s, head H2 and %s", baseSHA, head[:6], issue, ciJobBaseSHA[:6], rig.issue)
	}

	settleEveryMark(t, rig)
	if got := stateAt(t, rig, rig.issue, eachHeadH1, "k1"); got != model.PassStandsYes {
		t.Fatalf("k1 at H1 is published %q once the reconcile stored #70's base revision, want yes", got)
	}
	if got := rig.github.pullCommitReadsOf(70); got != 1 {
		t.Fatalf("#70's commits were read %d times, want once", got)
	}
	wantHandoff(t, rig, "once the reconcile stored #70's base revision and H1 is accepted", rig.issue, http.StatusCreated)
}

// TestAMembershipAnswerAboutRevisionsDispatchHasNotStoredAppliesNothing: the tester recorded k1
// passing at H1, and Dispatch stores #70 at H2 with its base branch, main, at baseRevisionA, so H1
// is #70's candidate. GitHub has moved #70 on where Dispatch has not stored it yet: its base branch
// is at baseRevisionB, or its head is H3. GitHub's read of #70 then names revisions the claim did
// not, so the settle decides nothing, whether the comparison of the stored revisions would list
// every commit or leave H1 to comparisons of its own: H1 stays a candidate and its obligation
// stays, read again after the failure backoff, not a second on, since no read can apply until a
// write stores what GitHub names. Once Dispatch stores what GitHub names, the next settle compares
// the stored revisions and applies what GitHub answers then. The neighbour's GitHub stands at the
// revisions Dispatch stores, and its answer applies at once.
func TestAMembershipAnswerAboutRevisionsDispatchHasNotStoredAppliesNothing(t *testing.T) {
	capped := make([]string, 502)
	for index := range capped {
		capped[index] = fmt.Sprintf("%040x", 0xd0000+index)
	}
	for _, row := range []struct {
		name string
		// head is the head Dispatch stores for #70; pull, base and listed are GitHub's: the head
		// and base revision its read of #70 names, and the commits between them.
		head, pull, base string
		listed           []string
		// caught is the write of #70 at the head and base revision GitHub names.
		caught func(t *testing.T, rig *checkStatesRig)
		// applied is whether the first settle's answer applies; want is k1's state at H1 once both
		// agree.
		applied bool
		want    string
	}{
		{name: "GitHub stands at the stored revisions", head: eachHeadH2, pull: eachHeadH2, base: baseRevisionA,
			listed: []string{eachHeadH1, eachHeadH2}, applied: true, want: model.PassStandsYes},
		{name: "the base branch moved on and the comparison leaves H1 out", head: eachHeadH2, pull: eachHeadH2, base: baseRevisionB,
			listed: []string{eachHeadH2}, want: "",
			caught: func(t *testing.T, rig *checkStatesRig) { storeBaseRevision(t, rig, 70, eachHeadH2, baseRevisionB) }},
		{name: "the base branch moved on and the comparison names H1", head: eachHeadH2, pull: eachHeadH2, base: baseRevisionB,
			listed: []string{eachHeadH1, eachHeadH2}, want: model.PassStandsYes,
			caught: func(t *testing.T, rig *checkStatesRig) { storeBaseRevision(t, rig, 70, eachHeadH2, baseRevisionB) }},
		{name: "the base branch moved on and the comparison past its pages leaves H1 to comparisons of its own", head: capped[501], pull: capped[501], base: baseRevisionB,
			listed: capped, want: "",
			caught: func(t *testing.T, rig *checkStatesRig) { storeBaseRevision(t, rig, 70, capped[501], baseRevisionB) }},
		{name: "the head moved on", head: eachHeadH2, pull: eachHeadH3, base: baseRevisionA,
			listed: []string{eachHeadH3}, want: "",
			caught: func(t *testing.T, rig *checkStatesRig) { storeBaseRevision(t, rig, 70, eachHeadH3, baseRevisionA) }},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newCheckStatesRig(t, checkStatesTwoChecks)
			eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
			storeBaseRevision(t, rig, 70, row.head, baseRevisionA)
			if got := candidatesOf(t, rig); !maps.Equal(got, map[string]bool{eachHeadH1: false}) {
				t.Fatalf("#70 holds the former heads %v, want H1 unverified: this test holds nothing", got)
			}
			rig.github.setPullCommits(70, row.listed...)
			rig.github.setPull(70, row.pull, "main", row.base)
			// Where the comparison's pages leave H1 out, the comparisons of H1 decide it: GitHub's
			// head holds H1 and so does the base revision, so H1 is outside.
			rig.github.setCompare(row.pull, eachHeadH1, ciJobCompare{status: "behind"})
			rig.github.setCompare(row.base, eachHeadH1, ciJobCompare{status: "behind"})
			settleOwed(t, rig)
			got := candidatesOf(t, rig)
			if row.applied {
				if !maps.Equal(got, map[string]bool{eachHeadH1: true}) {
					t.Fatalf("#70 holds the former heads %v once GitHub answered at the stored revisions, want H1 verified", got)
				}
			} else {
				if !maps.Equal(got, map[string]bool{eachHeadH1: false}) {
					t.Fatalf("#70 holds the former heads %v once GitHub named %s onto %s, which Dispatch does not store, want H1 unverified still: the settle decides nothing", got, row.pull[:4], row.base[:4])
				}
				if compares := rig.github.compareReadsOf(row.pull, eachHeadH1) + rig.github.compareReadsOf(row.base, eachHeadH1); compares != 0 {
					t.Fatalf("the settle compared H1 %d times while GitHub names revisions Dispatch does not store, want none", compares)
				}
				if marks := fenceMarksOf(t, rig, rig.issue); len(marks) == 0 {
					t.Fatalf("%s holds no mark a handoff waits for once GitHub named revisions Dispatch does not store, want its obligation kept", rig.issue)
				}
				var failures int
				var due float64
				if err := rig.database.Pool.QueryRow(context.Background(), `
					select coalesce(min(failures), 0), coalesce(min(extract(epoch from due_at - now())), 0)::float8
					from check_states_owed where issue_key = $1`, rig.issue).Scan(&failures, &due); err != nil {
					t.Fatalf("read the marks of %s: %v", rig.issue, err)
				}
				if failures < 1 || due < 20 {
					t.Fatalf("%s's marks count %d failures and fall due in %.1fs once GitHub named revisions Dispatch does not store, want a failure counted and the 30 s backoff: a read a second on is refused the same way", rig.issue, failures, due)
				}
				row.caught(t, rig)
			}
			if _, err := rig.database.Pool.Exec(context.Background(), `select check_states_owe($1)`, rig.issue); err != nil {
				t.Fatalf("owe %s a settle: %v", rig.issue, err)
			}
			settleEveryMark(t, rig)
			if got := stateAt(t, rig, rig.issue, eachHeadH1, "k1"); got != row.want {
				t.Fatalf("k1 at H1 is published %q, want %q: an answer applies only at the revisions the claim named", got, row.want)
			}
			verified, held := candidatesOf(t, rig)[eachHeadH1]
			if held != (row.want != "") || held && !verified {
				t.Fatalf("#70 holds H1 as held=%v verified=%v once Dispatch and GitHub agree, want it verified where GitHub's answer names it and gone where it does not", held, verified)
			}
		})
	}
}
