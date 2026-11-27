// check_states_unresolved_test.go holds the settler and the test handoff's fence to the work a
// settle could not finish: while a read Dispatch needs to know which heads are a pull request's is
// unresolved, the obligation that asked for it stays and blocks the fence, whatever backoff the
// retry waits; a state published before stays whatever it was, so only a fence that waits keeps it
// from deciding. The rig is check_states_test.go's.
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// wantHandoff posts a test handoff of issue and fails the test unless it answers want: 201 with
// the handoff, or 503 CHECK_STATES_UNSETTLED.
func wantHandoff(t *testing.T, rig *checkStatesRig, what, issue string, want int) {
	t.Helper()
	response := postHandoff(t, rig.handler, issue, "test", `{"tested":"H1"}`)
	switch want {
	case http.StatusCreated:
		if response.Code != http.StatusCreated {
			t.Fatalf("%s: the test handoff of %s answered %d %s, want 201", what, issue, response.Code, response.Body.String())
		}
	default:
		if code, _ := refusalOf(t, response); response.Code != http.StatusServiceUnavailable || code != "CHECK_STATES_UNSETTLED" {
			t.Fatalf("%s: the test handoff of %s answered %d %s, want 503 CHECK_STATES_UNSETTLED", what, issue, response.Code, response.Body.String())
		}
	}
}

// TestAnUnresolvedMembershipHoldsTheTestHandoffOverAnOldPass: #70 stood at H1, where the tester
// passed k1 and k2, and the settler published both yes there, which a reader holds. In the
// release before the install #70 moved to H2, which nothing recorded, and the tester then recorded
// k2 failing at H1, so the install left H1 as #70's candidate. GitHub fails the read of #70's
// commits: H1 is no former head yet, so nothing published replaces the old yes there. The test
// handoff therefore waits, and answers 503, rather than hand a reader a serial its old pass would
// decide at. Once GitHub answers, H1 is accepted, k2's state there is published, and the handoff
// is recorded.
func TestAnUnresolvedMembershipHoldsTheTestHandoffOverAnOldPass(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "pass", eachHeadH1)
	settleOwed(t, rig)
	if got := stateAt(t, rig, rig.issue, eachHeadH1, "k2"); got != model.PassStandsYes {
		t.Fatalf("k2 at H1 is published %q, want yes: this test holds no old pass", got)
	}
	upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
	eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "fail", eachHeadH1)
	forgetCandidates(t, rig)
	settleOwed(t, rig)
	rig.github.failPullCommits(http.StatusBadGateway)
	seedInstallCandidates(t, rig, eachHeadH1)
	settleOwed(t, rig)
	if got := candidatesOf(t, rig); len(got) != 1 || got[eachHeadH1] {
		t.Fatalf("#70 holds the former heads %v after the failed read, want H1 unverified: this test holds nothing unresolved", got)
	}
	rig.served.checkStatesSchedule.fenceWait = time.Second
	wantHandoff(t, rig, "while the read of #70's commits that would replace k2's old yes at H1 fails", rig.issue, http.StatusServiceUnavailable)

	rig.github.failPullCommits(0)
	rig.github.setPullCommits(70, eachHeadH1, eachHeadH2)
	settleEveryMark(t, rig)
	if got := stateAt(t, rig, rig.issue, eachHeadH1, "k2"); got != model.PassStandsUnknown {
		t.Fatalf("k2 at H1, with a pass and a fail, is published %q once GitHub answered, want unknown", got)
	}
	wantHandoff(t, rig, "once H1 is accepted and its states published", rig.issue, http.StatusCreated)
}

// TestACauseNoRetryFixesHoldsNoTestHandoff: as in the test above, H1 is #70's candidate and no
// answer has decided it, but GitHub cannot answer for #70 and no retry changes that: the
// deployment has no App key, or GitHub refuses the installation's token the repository (403). The
// settle then keeps no mark for the read, so the test handoff is recorded within the fence's
// wait, and H1 stays a candidate, no former head, until a later settle of the issue reads it once
// access returns. The control is the 404 of the test above, which a retry can change, and which
// holds the handoff.
func TestACauseNoRetryFixesHoldsNoTestHandoff(t *testing.T) {
	for _, tc := range []struct {
		name string
		cut  func(rig *checkStatesRig)
	}{
		{"a deployment with no App key", func(rig *checkStatesRig) { rig.served.deps.GitHub = nil }},
		{"a repository GitHub refuses the installation", func(rig *checkStatesRig) { rig.github.refusePulls(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newCheckStatesRig(t, checkStatesTwoChecks)
			upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
			eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
			eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "pass", eachHeadH1)
			settleOwed(t, rig)
			upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
			eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "fail", eachHeadH1)
			forgetCandidates(t, rig)
			settleOwed(t, rig)
			tc.cut(rig)
			seedInstallCandidates(t, rig, eachHeadH1)
			settleOwed(t, rig)
			if got := candidatesOf(t, rig); len(got) != 1 || got[eachHeadH1] {
				t.Fatalf("#70 holds the former heads %v after the read GitHub cannot answer, want H1 unverified", got)
			}
			if marks := fenceMarksOf(t, rig, rig.issue); len(marks) != 0 {
				t.Fatalf("the settle kept the marks %v in the fence's wait, want none: no retry reads #70's commits", marks)
			}
			rig.served.checkStatesSchedule.fenceWait = time.Second
			wantHandoff(t, rig, "while GitHub cannot answer for #70 and no retry changes that", rig.issue, http.StatusCreated)
		})
	}
}

// TestTheBootstrapObligationLastsUntilItsCandidatesAreVerified: before the install, the tester
// recorded k1 passing at H1 while #70 stood there, and #70 moved on to H2; the install leaves H1
// as #70's candidate and the issue owed. After the install, the other issue's tester records k2
// passing at H4 before Dispatch writes any row of #71, which then starts at that issue at H5: H4 is
// #71's candidate. Neither the install nor #71's first row is a cutoff of the results each takes.
// GitHub fails the read of both pull requests' commits, so each issue's obligation stays, and its
// test handoff answers 503, until GitHub answers and the candidates are accepted: then k1 is
// published yes at H1 and k2 yes at H4, and both handoffs are recorded.
func TestTheBootstrapObligationLastsUntilItsCandidatesAreVerified(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	other := rig.fileIssue(t, "Another issue")
	uploadDocument(t, rig.handler, "/api/v1/issues/"+other, "verification.md", checkStatesTwoChecks)
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
	forgetCandidates(t, rig)
	settleOwed(t, rig)
	eachHeadResult(t, rig.handler, other, "k2", "tester", "pass", eachHeadH4)
	settleOwed(t, rig)
	rig.github.failPullCommits(http.StatusBadGateway)
	seedInstallCandidates(t, rig, eachHeadH1)
	upsertHead(t, rig, 71, eachHeadH5, other, "Another change")
	settleOwed(t, rig)
	rig.served.checkStatesSchedule.fenceWait = time.Second
	wantHandoff(t, rig, "while the read of the install's candidate fails", rig.issue, http.StatusServiceUnavailable)
	wantHandoff(t, rig, "while the read of the candidate #71's start left fails", other, http.StatusServiceUnavailable)

	rig.github.failPullCommits(0)
	rig.github.setPullCommits(70, eachHeadH1, eachHeadH2)
	rig.github.setPullCommits(71, eachHeadH4, eachHeadH5)
	settleEveryMark(t, rig)
	if got := stateAt(t, rig, rig.issue, eachHeadH1, "k1"); got != model.PassStandsYes {
		t.Fatalf("k1 at H1, a result from before the install, is published %q once #70's commits list it, want yes", got)
	}
	if got := stateAt(t, rig, other, eachHeadH4, "k2"); got != model.PassStandsYes {
		t.Fatalf("k2 at H4, a result from before #71's first row, is published %q once #71's commits list it, want yes", got)
	}
	wantHandoff(t, rig, "once the install's candidate is accepted", rig.issue, http.StatusCreated)
	wantHandoff(t, rig, "once #71's candidate is accepted", other, http.StatusCreated)
}

// fenceMarksOf is the ids of the marks of issue a test handoff that starts now waits for, read
// by the fence's own predicate (awaitCheckStatesSettled), lowest first.
func fenceMarksOf(t *testing.T, rig *checkStatesRig, issue string) []int64 {
	t.Helper()
	var marks []int64
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select coalesce(array_agg(id order by id), '{}') from check_states_owed
		where issue_key = $1 and (due_at <= now() or reads_from is not null)`, issue).Scan(&marks); err != nil {
		t.Fatalf("read the marks of %s a handoff waits for: %v", issue, err)
	}
	return marks
}

// TestNoMarkCommittedLateReleasesATestHandoffWhileAReadIsUnresolved: #70 stood at H1, where the
// tester passed k1 and k2 and the settler published both yes, which a reader holds, and moved on
// to H2. A writer's transaction draws a mark and holds it uncommitted; the tester then records k2
// failing at H1, whose marks are drawn after it, the install leaves H1 as #70's candidate, and
// GitHub fails the read of #70's commits. The test handoff reads the marks it waits for, which
// the uncommitted mark is not among, and only then does that mark commit: the settle that follows
// reads it with the others, a lower id than every mark the handoff waits for, while the read that
// would replace k2's old yes at H1 is unresolved. However the settle combines the marks it read,
// none the handoff waits for may go while that read is unresolved, so the handoff answers 503.
// The neighbour commits the mark before the handoff reads its marks, so the handoff waits for it
// too, and answers 503 the same.
func TestNoMarkCommittedLateReleasesATestHandoffWhileAReadIsUnresolved(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "every mark commits before the handoff reads the marks it waits for"
		if late {
			name = "a lower mark commits after the handoff read the marks it waits for"
		}
		t.Run(name, func(t *testing.T) {
			rig := newCheckStatesRig(t, checkStatesTwoChecks)
			upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
			eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
			eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "pass", eachHeadH1)
			settleOwed(t, rig)
			if got := stateAt(t, rig, rig.issue, eachHeadH1, "k2"); got != model.PassStandsYes {
				t.Fatalf("k2 at H1 is published %q, want yes: this test holds no old pass", got)
			}
			upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
			forgetCandidates(t, rig)
			settleOwed(t, rig)

			ctx := context.Background()
			writer, err := rig.database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the writer's transaction: %v", err)
			}
			defer writer.Rollback(ctx)
			var lower int64
			if err := writer.QueryRow(ctx, `insert into check_states_owed (issue_key) values ($1) returning id`, rig.issue).Scan(&lower); err != nil {
				t.Fatalf("draw the writer's mark: %v", err)
			}
			if !late {
				if err := writer.Commit(ctx); err != nil {
					t.Fatalf("commit the writer's mark: %v", err)
				}
			}
			eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "fail", eachHeadH1)
			seedInstallCandidates(t, rig, eachHeadH1)
			rig.github.failPullCommits(http.StatusBadGateway)
			rig.served.checkStatesSchedule.fenceWait = 10 * time.Second
			rig.served.checkStatesSchedule.fencePoll = 5 * time.Millisecond
			entered, release := make(chan struct{}), make(chan struct{})
			releaseOnce := sync.OnceFunc(func() { close(release) })
			defer releaseOnce()
			rig.served.checkStatesFenceWaits = func(string) {
				close(entered)
				<-release
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- postHandoff(t, rig.handler, rig.issue, "test", `{"tested":"H1"}`) }()
			ciAwait(t, "the handoff read the marks it waits for", entered)
			waited := fenceMarksOf(t, rig, rig.issue)
			if late {
				if len(waited) == 0 || slices.Contains(waited, lower) || lower > waited[0] {
					t.Fatalf("the handoff waits for %v and the uncommitted mark is %d, want a lower mark it does not wait for: this test holds no late mark", waited, lower)
				}
				if err := writer.Commit(ctx); err != nil {
					t.Fatalf("commit the writer's mark: %v", err)
				}
			} else if !slices.Contains(waited, lower) {
				t.Fatalf("the handoff waits for %v, want the committed mark %d among them", waited, lower)
			}
			settleOwed(t, rig)
			if got := candidatesOf(t, rig); len(got) != 1 || got[eachHeadH1] {
				t.Fatalf("#70 holds the former heads %v after the failed read, want H1 unverified: this test holds nothing unresolved", got)
			}
			releaseOnce()
			var response *httptest.ResponseRecorder
			select {
			case response = <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("the handoff did not answer in 15 s")
			}
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("the handoff waiting for %v answered %d %s while #70's read is unresolved and the marks %v are held, want 503",
					waited, response.Code, response.Body.String(), fenceMarksOf(t, rig, rig.issue))
			}
		})
	}
}
