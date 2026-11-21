package workflow

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// naming is fact, a settlement of settledAt or settledRunsAt, naming statuses: each a context, its
// state and its created_at, as the decoder reads the listener's field.
func naming(fact intake.Fact, statuses ...record.CommitStatus) intake.PullRequestChecks {
	checks := fact.(intake.PullRequestChecks)
	checks.Statuses = statuses
	return checks
}

// statusAt is the created_at of a commit status these tests name: minute minutes past a fixed
// hour, in UTC as the decoder keeps it, so a later minute is a newer status of its context.
func statusAt(minute int) time.Time { return time.Date(2026, 11, 17, 5, minute, 0, 0, time.UTC) }

// status is the commit status context in state created at minute, as a settlement names it.
func status(context string, state record.CommitStatusState, minute int) record.CommitStatus {
	return record.CommitStatus{Context: context, State: state, CreatedAt: statusAt(minute)}
}

// keptStatus is that status as the pull request keeps it, stamped with the head it was settled at.
func keptStatus(context string, state record.CommitStatusState, minute int, head string) record.CommitStatus {
	kept := status(context, state, minute)
	kept.Head = head
	return kept
}

// red is the commit status context, in state, as a text names a status that did not pass.
func red(context string, state record.CommitStatusState) texts.StatusState {
	return texts.StatusState{Context: context, State: string(state)}
}

// seedSettlingRound seeds LEGION-208 in testing, its pull request #42 open at head with no
// settlement yet, and the implementer's and the tester's phases claimed: the issue each settlement
// below is applied to, whose CI message reaches three roles (tell).
func seedSettlingRound(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim"})
}

// settledCI is what a ci-settled message of a settlement at head names of its checks and its
// commit statuses: the checks that did not pass (notPassed), the checks GitHub skipped (skipped,
// beside which the message says once that a skipped check ran nothing), the statuses that did not
// pass (statuses), and the count of the checks and statuses that passed.
type settledCI struct {
	passed    int
	notPassed []texts.CheckConclusion
	skipped   []string
	statuses  []texts.StatusState
}

// statusesTold is the ci-settled message of a settlement of LEGION-208's pull request 42 at head,
// in phase at, to each of roles at the urgency the settlement's rule gives it there, naming what
// settled says.
func statusesTold(t *testing.T, roles []claim.Role, at phase.Phase, settled settledCI) []roleTold {
	t.Helper()
	told := settledTold(t, roles, at, "head", settled.passed, settled.notPassed...)
	for i := range told {
		rec := told[i].Record.(texts.CISettledRecord)
		rec.Skipped, rec.NotPassedStatuses = settled.skipped, settled.statuses
		if len(settled.skipped) > 0 {
			rec.EachSkippedCheck = texts.RanNothing
		}
		told[i].Record = rec
	}
	return told
}

// tell is told with the CI message settled added for each role a settlement of
// seedSettlingRound's issue is told to - the architect, the tester and the implementer - in the
// order the daemon queues them.
func tell(t *testing.T, told []roleTold, settled settledCI) []roleTold {
	t.Helper()
	return append(told, statusesTold(t, []claim.Role{claim.RoleArchitect, claim.RoleTester, claim.RoleImplementer}, phase.Testing, settled)...)
}

// The CI message names each commit status the settlement standing for the head names in state
// failure or error, with its state as GitHub spells it, in context order, in a table of its own
// apart from the check runs, and counts the statuses that passed with the checks that passed: one
// message to each of the three roles, as for a settlement that names none. A later settlement in
// which only a status changed state, a newer status of its context, says something new, and is
// told again.
func TestTheCIMessageNamesEachStatus(t *testing.T) {
	pool := migratedPool(t)
	seedSettlingRound(t, pool)
	engine := mergingEngine()
	told := []roleTold{}
	for i, step := range []struct {
		fact    intake.Fact
		settled settledCI
	}{
		{naming(settledAt("head", 1, "unit"), status("sched-suite", "error", 1), status("ci/untested-draft", "failure", 1), status("deploy", "success", 1)),
			settledCI{passed: 2, notPassed: []texts.CheckConclusion{failed("unit")},
				statuses: []texts.StatusState{red("ci/untested-draft", "failure"), red("sched-suite", "error")}}},
		{naming(settledAt("head", 2, "unit"), status("ci/untested-draft", "failure", 1), status("deploy", "failure", 2), status("sched-suite", "error", 1)),
			settledCI{passed: 1, notPassed: []texts.CheckConclusion{failed("unit")},
				statuses: []texts.StatusState{red("ci/untested-draft", "failure"), red("deploy", "failure"), red("sched-suite", "error")}}},
	} {
		applyRefusingNothing(t, pool, engine, step.fact)
		told = tell(t, told, step.settled)
		assertRoleMessages(t, pool, "LEGION-208", told)
		if got := issuePhase(t, pool); got != phase.Testing {
			t.Fatalf("after settlement %d the issue is in %s, want testing", i+1, got)
		}
	}
	want := []record.CommitStatus{keptStatus("ci/untested-draft", "failure", 1, "head"), keptStatus("deploy", "failure", 2, "head"),
		keptStatus("sched-suite", "error", 1, "head")}
	if got := recordedPR(t, pool, "LEGION-208").Statuses; !reflect.DeepEqual(got, want) {
		t.Fatalf("the pull request keeps %+v, want the newest status of each context, each stamped with the head: %+v", got, want)
	}
}

// stuckApprovedRound is the approved-round-stuck message the outbox holds for LEGION-208's
// reviewer, and false where it holds none.
func stuckApprovedRound(t *testing.T, pool *pgxpool.Pool) (texts.ApprovedRoundStuckRecord, bool) {
	t.Helper()
	for _, message := range roleMessageRows(t, pool, "LEGION-208") {
		if message.Kind != texts.ApprovedRoundStuck {
			continue
		}
		rec, err := message.Record()
		if err != nil {
			t.Fatalf("the approved-round-stuck message carries no record: %v", err)
		}
		return *rec.(*texts.ApprovedRoundStuckRecord), true
	}
	return texts.ApprovedRoundStuckRecord{}, false
}

// The notice of a head GitHub will not merge, and the reviewer's message of it, name each commit
// status the record keeps for that head in state failure or error, with its state, and count the
// others with the head's own check runs that passed, when the checked head is the current head;
// they name and count none of another head's. Three records: the head settled with two statuses;
// the branch moved on to a new head whose own settlement names none, which starts the head's fence
// afresh and the statuses with it; and the record an earlier release leaves after a rollback,
// whose checked head and runs it moved to the head while the statuses stayed stamped with the head
// before, which the stamp alone keeps out of the notice.
func TestTheStuckNoticeNamesStatusesOfTheCurrentHeadAlone(t *testing.T) {
	const earlier = "0000000000000000000000000000000000000abc"
	// stuckNames holds the one review-stuck notice and the reviewer's approved-round-stuck message
	// to the statuses that did not pass, statuses, and the checks and statuses that passed.
	stuckNames := func(t *testing.T, pool *pgxpool.Pool, passed int, statuses ...texts.StatusState) {
		t.Helper()
		told := reviewStuckNotices(t, pool)
		if len(told) != 1 || told[0].Passed == nil || *told[0].Passed != passed || len(told[0].NotPassed) != 0 ||
			!reflect.DeepEqual(told[0].NotPassedStatuses, statuses) {
			t.Fatalf("review-stuck notices %+v, want one naming the statuses %+v that did not pass and counting %d that passed", told, statuses, passed)
		}
		message, found := stuckApprovedRound(t, pool)
		if !found || message.Passed != passed || len(message.NotPassed) != 0 || !reflect.DeepEqual(message.NotPassedStatuses, statuses) {
			t.Fatalf("the reviewer's approved-round-stuck message %+v (found %t), want the statuses %+v and %d passed", message, found, statuses, passed)
		}
	}
	t.Run("the current head's statuses", func(t *testing.T) {
		pool := migratedPool(t)
		seedMergeStateRound(t, pool, record.MergeStateBlocked)
		apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
		apply("settled", naming(settledRunsAt(headOne, 1, []record.AttemptRun{{Name: "unit tests", ID: 2}}),
			status("deploy", "success", 1), status("ci", "failure", 1)))
		apply("approve", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: headOne,
			HeadSHA: headOne, Author: testReviewApp, Body: reviewBody("ship it", headOne)})
		stuckNames(t, pool, 2, red("ci", "failure"))
	})
	t.Run("a new head whose settlement names none", func(t *testing.T) {
		pool := migratedPool(t)
		seedMergeStateRound(t, pool, record.MergeStateBlocked)
		apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
		apply("settled", naming(settledRunsAt(headOne, 1, []record.AttemptRun{{Name: "unit tests", ID: 2}}), status("ci", "failure", 1)))
		apply("push", intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: headOne, After: headTwo, Pusher: "legion-implementer[bot]"})
		apply("sync", intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: headTwo})
		if got := recordedPR(t, pool, "LEGION-208"); !reflect.DeepEqual(got.Statuses, []record.CommitStatus{keptStatus("ci", "failure", 1, headOne)}) {
			t.Fatalf("after the move the record keeps %+v, want ci failed at %s: this test proves nothing without it", got.Statuses, headOne)
		}
		apply("settled-new-head", settledRunsAt(headTwo, 1, []record.AttemptRun{{Name: "unit tests", ID: 3}}))
		apply("approve", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: headTwo,
			HeadSHA: headTwo, Author: testReviewApp, Body: reviewBody("ship it", headTwo)})
		apply("blocked", intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: headTwo,
			Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateBlocked})
		stuckNames(t, pool, 1)
	})
	t.Run("a list an earlier release left beside its checked head", func(t *testing.T) {
		pool := migratedPool(t)
		seedMergeStateRound(t, pool, record.MergeStateBlocked)
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
			Number: 42, Branch: "legion/LEGION-208", HeadSHA: headOne, Failing: []string{}, Cancelled: []string{},
			Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateBlocked,
			CheckedHead: headOne, CheckRuns: []record.AttemptRun{{Name: "unit tests", ID: 1, Head: headOne, Conclusion: record.RunSucceeded}},
			Statuses: []record.CommitStatus{keptStatus("ci", "failure", 1, earlier)}})
		apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
		apply("approve", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: headOne,
			HeadSHA: headOne, Author: testReviewApp, Body: reviewBody("ship it", headOne)})
		stuckNames(t, pool, 1)
	})
}

// A failed commit status moves no issue, in any phase, as a failed check run moves none
// (dispatch://INFRA-714/spec R1): the settlement that names it is told to the three roles, which
// decide what it means, and nothing else happens - no phase moves, no notice reaches the
// architect, no worker is started and no return is counted.
func TestAFailedStatusMovesNoIssue(t *testing.T) {
	for _, tc := range []struct {
		from   phase.Phase
		status string
		roles  []claim.Role
	}{
		{phase.Implementing, "in_progress", []claim.Role{claim.RoleArchitect, claim.RoleImplementer}},
		{phase.Testing, "testing", []claim.Role{claim.RoleArchitect, claim.RoleTester, claim.RoleImplementer}},
		{phase.Reviewing, "needs_review", []claim.Role{claim.RoleArchitect, claim.RoleReviewer, claim.RoleImplementer}},
		{phase.Accepting, "needs_review", []claim.Role{claim.RoleArchitect, claim.RoleImplementer}},
		{phase.Merging, "retro", []claim.Role{claim.RoleArchitect, claim.RoleMerger, claim.RoleImplementer}},
	} {
		t.Run(string(tc.from), func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			if role := RoleFor(tc.from); role != "" && role != claim.RoleImplementer {
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: role, Claim: claim.Token(string(role) + "-claim")})
			}
			applyRefusingNothing(t, pool, mergingEngine(), naming(settledAt("head", 1), status("ci", "failure", 1), status("deploy", "error", 1)))

			if got := issuePhase(t, pool); got != tc.from {
				t.Fatalf("after a settlement naming failed statuses the issue is in %s, want it left in %s", got, tc.from)
			}
			if got := noticeKinds(t, pool, "LEGION-208"); len(got) != 0 {
				t.Fatalf("the architect was told %v, want nothing but the role message", got)
			}
			assertOutboxCount(t, pool, "supervise", 0)
			assertOutboxCount(t, pool, "dispatch_status", 0)
			if pass := implementerPass(t, pool); pass != 0 {
				t.Fatalf("pass = %d, want none counted: nothing went back", pass)
			}
			assertRoleMessages(t, pool, "LEGION-208", statusesTold(t, tc.roles, tc.from,
				settledCI{passed: 2, statuses: []texts.StatusState{red("ci", "failure"), red("deploy", "error")}}))
		})
	}
}

// The pull request keeps the newest status of each context the settlements of the head named, by
// created_at, so a later settlement that names a newer status of a context as success replaces the
// failure the record kept for it: the pull request keeps no failed status, and the CI message
// names no status as not passed and counts the context with the ones that passed. A settlement
// after it whose list the daemon could not read names no status, so the green list stays as it is
// and no red name comes back, and it says nothing new, so nobody is told again.
func TestAStatusThatTurnsGreenLeavesNoRedName(t *testing.T) {
	pool := migratedPool(t)
	seedSettlingRound(t, pool)
	engine := mergingEngine()
	unread := naming(settledAt("head", 3))
	unread.StatusesUnread = true
	told := []roleTold{}
	for i, step := range []struct {
		fact intake.Fact
		// settled is what the roles are told, nil when the settlement says nothing new.
		settled *settledCI
		kept    []record.CommitStatus
	}{
		{naming(settledAt("head", 1), status("ci", "failure", 1)),
			&settledCI{passed: 2, statuses: []texts.StatusState{red("ci", "failure")}},
			[]record.CommitStatus{keptStatus("ci", "failure", 1, "head")}},
		{naming(settledAt("head", 2), status("ci", "success", 2)),
			&settledCI{passed: 3},
			[]record.CommitStatus{keptStatus("ci", "success", 2, "head")}},
		{unread, nil, []record.CommitStatus{keptStatus("ci", "success", 2, "head")}},
	} {
		applyRefusingNothing(t, pool, engine, step.fact)
		if step.settled != nil {
			told = tell(t, told, *step.settled)
		}
		assertRoleMessages(t, pool, "LEGION-208", told)
		pr := recordedPR(t, pool, "LEGION-208")
		if !reflect.DeepEqual(pr.Statuses, step.kept) {
			t.Fatalf("after settlement %d the pull request keeps %+v, want %+v", i+1, pr.Statuses, step.kept)
		}
		if i > 0 {
			for _, kept := range pr.Statuses {
				if kept.Failed() {
					t.Fatalf("after settlement %d the pull request keeps %+v failed, want no red name once ci turned green", i+1, kept)
				}
			}
			if named := CISettledMessage("LEGION-208", pr, phase.Testing).NotPassedStatuses; len(named) != 0 {
				t.Fatalf("after settlement %d the CI message names %+v as not passed, want no failed status in it", i+1, named)
			}
		}
	}
}

// The task of a role that a settlement wakes names each commit status the settlement names failed,
// in context order, beside the failed check runs, the cancelled ones, or none, and counts the
// statuses that passed with the checks that passed. A settlement whose list the daemon could not
// read names the failed statuses the pull request keeps for the head, which applying it leaves as
// they are, rather than telling the role the head passed every check. A settlement that names no
// failed status names no status, and counts the ones that passed.
func TestTheWakeTaskNamesAFailedStatus(t *testing.T) {
	checks := func(failing, cancelled []string, statuses ...record.CommitStatus) intake.PullRequestChecks {
		fact := settlement("head1", failing...)
		fact.Cancelled, fact.Statuses = cancelled, statuses
		return fact
	}
	cancelled := func(check string) texts.CheckConclusion {
		return texts.CheckConclusion{Check: check, Conclusion: string(record.RunCancelled)}
	}
	for _, shape := range []struct {
		name      string
		fact      intake.PullRequestChecks
		notPassed []texts.CheckConclusion
		statuses  []texts.StatusState
		passed    int
	}{
		{"a failed check run", checks([]string{"build"}, nil, status("deploy", "success", 1), status("ci", "failure", 1)),
			[]texts.CheckConclusion{failed("build")}, []texts.StatusState{red("ci", "failure")}, 1},
		{"a cancelled check run", checks(nil, []string{"build"}, status("sched-suite", "error", 1), status("ci/untested-draft", "failure", 1)),
			[]texts.CheckConclusion{cancelled("build")}, []texts.StatusState{red("ci/untested-draft", "failure"), red("sched-suite", "error")}, 0},
		{"no failed check run", checks(nil, nil, status("ci/untested-draft", "failure", 1)),
			nil, []texts.StatusState{red("ci/untested-draft", "failure")}, 1},
	} {
		t.Run(shape.name, func(t *testing.T) {
			got := SettlementWake("LEGION-208", phase.Implementing, "sjawhar/legion", shape.fact)
			if !reflect.DeepEqual(got.NotPassed, shape.notPassed) || !reflect.DeepEqual(got.NotPassedStatuses, shape.statuses) ||
				got.Passed == nil || *got.Passed != shape.passed {
				t.Fatalf("the settlement's wake = %+v, want the checks %+v and the statuses %+v not passed and %d passed",
					got, shape.notPassed, shape.statuses, shape.passed)
			}
			rig := newWakeRig(t)
			rig.apply("settled", shape.fact)
			// The wake brings back a session an idle release stopped (newWakeRig), which its task names.
			got.StoppedBy = StoppedByRelease.word()
			oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeChecks, taskText(got))
		})
	}
	t.Run("a list the daemon could not read", func(t *testing.T) {
		rig := newWakeRig(t)
		seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
			Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head1", Failing: []string{}, Cancelled: []string{},
			CheckedHead: "head1", CheckRuns: []record.AttemptRun{{Name: "build", ID: 1, Head: "head1", Conclusion: record.RunSucceeded}},
			Generation: 1, Snapshot: "head1-snapshot",
			Statuses: []record.CommitStatus{keptStatus("ci", "failure", 1, "head1"), keptStatus("deploy", "success", 1, "head1")}})
		unread := checks(nil, nil)
		unread.Generation, unread.Snapshot, unread.StatusesUnread = 2, "head1-2", true
		rig.apply("settled", unread)
		passed := 2
		want := texts.PodWakeRecord{TaskHead: texts.TaskHead{Kind: texts.PodWake, Issue: "LEGION-208"}, Wake: pods.WakeChecks,
			PullRequest: "sjawhar/legion#42", Head: "head1", Phase: phase.Implementing,
			NotPassedStatuses: []texts.StatusState{red("ci", "failure")}, Passed: &passed, ListPassed: "gh pr checks 42 --repo sjawhar/legion",
			StoppedBy: StoppedByRelease.word()}
		oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeChecks, taskText(want))
	})
	for _, today := range []struct {
		name      string
		fact      intake.PullRequestChecks
		notPassed []texts.CheckConclusion
		passed    int
	}{
		{"no failed status", checks(nil, nil, status("deploy", "success", 1)), nil, 2},
		{"no status", checks(nil, nil), nil, 1},
		{"a failed check run and no failed status", checks([]string{"build"}, nil, status("deploy", "success", 1)), []texts.CheckConclusion{failed("build")}, 1},
		{"a cancelled check run and no status", checks(nil, []string{"build"}), []texts.CheckConclusion{cancelled("build")}, 0},
	} {
		got := SettlementWake("LEGION-208", phase.Implementing, "sjawhar/legion", today.fact)
		if !reflect.DeepEqual(got.NotPassed, today.notPassed) || len(got.NotPassedStatuses) != 0 || got.Passed == nil || *got.Passed != today.passed {
			t.Errorf("%s: the settlement's wake = %+v, want the checks %+v not passed, no status, and %d passed", today.name, got, today.notPassed, today.passed)
		}
	}
}

// A settlement that names a commit status the record does not hold yet - one created later than
// the kept status of its context, or one of a context the newer settlement left out, as a record
// that dropped its statuses at the size bound or a listener of an earlier release writes it - and
// that arrives after a newer settlement changes that status alone: one whose check runs are older
// than the kept ones, and one whose check runs are the same at a lower generation, as a settlement
// the listener sent before a check name appeared is to the fence of the one it sent after, which
// adds lint. The kept check runs, the names failing and cancelled, the generation and the snapshot
// stay as the newer settlement left them: e2e, a check GitHub never started, stays cancelled as
// the newer settlement says, though the older one names it failing. The roles are told of the
// changed status, and the newer settlement delivered again after it changes nothing, tells nobody
// and marks no movement of the issue (record.Issue.MovedAt).
func TestAnOlderCheckRunSetChangesItsNewerStatusesAlone(t *testing.T) {
	build1, build2 := []record.AttemptRun{{Name: "build", ID: 1}}, []record.AttemptRun{{Name: "build", ID: 2}}
	e2e := []texts.CheckConclusion{{Check: "e2e", Conclusion: string(record.RunCancelled)}}
	ciFailed := []texts.StatusState{red("ci", "failure")}
	for _, tc := range []struct {
		name string
		// newerRuns and newer are the check runs and the statuses the newer settlement names, and
		// told what the roles are told of it; olderRuns are the check runs of the settlement that
		// arrives after it, and toldOlder what the roles are told of that one.
		newerRuns, olderRuns []record.AttemptRun
		newer                []record.CommitStatus
		told, toldOlder      settledCI
	}{
		{"a status created later than the kept one", build2, build1, []record.CommitStatus{status("ci", "success", 1)},
			settledCI{passed: 2, notPassed: e2e},
			settledCI{passed: 1, notPassed: e2e, statuses: ciFailed}},
		{"a context the newer settlement left out", build2, build1, nil,
			settledCI{passed: 1, notPassed: e2e},
			settledCI{passed: 1, notPassed: e2e, statuses: ciFailed}},
		{"the same runs as the newer settlement but the check name it added", []record.AttemptRun{{Name: "build", ID: 2}, {Name: "lint", ID: 3}}, build2, nil,
			settledCI{passed: 2, notPassed: e2e},
			settledCI{passed: 2, notPassed: e2e, statuses: ciFailed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedSettlingRound(t, pool)
			// Each settlement is applied at a moment of its own after the record's own stamp, so a
			// movement one of them marks is one no other could have written: the mark only ever
			// moves forwards (record.Store.MarkIssueMoved).
			base := issueRecord(t, pool, "LEGION-208").MovedAt.Add(time.Minute)
			newer := intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head", CheckRuns: tc.newerRuns,
				Generation: 6, Snapshot: "head-6", Failing: []string{}, Cancelled: []string{"e2e"}, Statuses: tc.newer}
			older := intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head", CheckRuns: tc.olderRuns,
				Generation: 5, Snapshot: "head-5", Failing: []string{"e2e"}, Cancelled: []string{}, Statuses: []record.CommitStatus{status("ci", "failure", 2)}}
			fence := make([]record.AttemptRun, 0, len(tc.newerRuns))
			for _, run := range tc.newerRuns {
				fence = append(fence, record.AttemptRun{Name: run.Name, ID: run.ID, Head: "head", Conclusion: record.RunSucceeded})
			}
			told := []roleTold{}

			applyRefusingNothing(t, pool, mergingEngineAt(base), newer)
			settled := recordedPR(t, pool, "LEGION-208")
			if !reflect.DeepEqual(settled.CheckRuns, fence) || len(settled.Failing) != 0 || !reflect.DeepEqual(settled.Cancelled, []string{"e2e"}) ||
				settled.Generation != 6 || settled.Snapshot != "head-6" {
				t.Fatalf("the newer settlement left %+v; want the runs %+v, e2e cancelled at generation 6, head-6: this test proves nothing without it", settled, fence)
			}
			told = tell(t, told, tc.told)
			assertRoleMessages(t, pool, "LEGION-208", told)

			applyRefusingNothing(t, pool, mergingEngineAt(base.Add(time.Minute)), older)
			want := settled
			want.Statuses = []record.CommitStatus{keptStatus("ci", "failure", 2, "head")}
			if got := recordedPR(t, pool, "LEGION-208"); !reflect.DeepEqual(got, want) {
				t.Fatalf("after the older settlement the pull request is\n  %+v\nwant the newer settlement's record with ci's failure its one change:\n  %+v", got, want)
			}
			told = tell(t, told, tc.toldOlder)
			assertRoleMessages(t, pool, "LEGION-208", told)
			moved := issueRecord(t, pool, "LEGION-208").MovedAt

			applyRefusingNothing(t, pool, mergingEngineAt(base.Add(2*time.Minute)), newer)
			if got := recordedPR(t, pool, "LEGION-208"); !reflect.DeepEqual(got, want) {
				t.Fatalf("after the newer settlement delivered again the pull request is\n  %+v\nwant it as it was:\n  %+v", got, want)
			}
			assertRoleMessages(t, pool, "LEGION-208", told)
			if got := issueRecord(t, pool, "LEGION-208").MovedAt; !got.Equal(moved) {
				t.Fatalf("the newer settlement delivered again marked the issue moved at %s, want it last moved at %s still", got, moved)
			}
		})
	}
}

// The listener numbers the settlements of a record the bucket's TTL removed and the reconcile
// wrote again afresh, so a settlement of the recreated record can come at generation 0 after one
// of a higher generation, with the same check runs. A failed commit status it names, created later
// than the kept status of its context, makes it newer for its statuses alone: the pull request
// keeps the failure at its own numbering, 4 and head-4, and the daemon names that failed status to
// the roles.
func TestARecreatedRecordStillNamesARedStatus(t *testing.T) {
	pool := migratedPool(t)
	seedSettlingRound(t, pool)
	engine := mergingEngine()
	recreated := naming(settledAt("head", 4), status("ci", "failure", 2))
	recreated.Generation, recreated.Snapshot = 0, "head-recreated"
	told := []roleTold{}
	for _, step := range []struct {
		fact    intake.Fact
		settled settledCI
	}{
		{naming(settledAt("head", 4), status("ci", "success", 1)), settledCI{passed: 3}},
		{recreated, settledCI{passed: 2, statuses: []texts.StatusState{red("ci", "failure")}}},
	} {
		applyRefusingNothing(t, pool, engine, step.fact)
		told = tell(t, told, step.settled)
		assertRoleMessages(t, pool, "LEGION-208", told)
	}
	pr := recordedPR(t, pool, "LEGION-208")
	if want := []record.CommitStatus{keptStatus("ci", "failure", 2, "head")}; !reflect.DeepEqual(pr.Statuses, want) || pr.Generation != 4 || pr.Snapshot != "head-4" {
		t.Fatalf("the pull request keeps %+v at generation %d, snapshot %q; want %+v at its own 4, head-4: the recreated record's settlement changes its statuses alone",
			pr.Statuses, pr.Generation, pr.Snapshot, want)
	}
}

// R3: a list names "the checks and commit statuses that did not pass" (dispatch://INFRA-3686/spec).
// The pull request keeps the failed status ci for head1, and a later settlement of head1 names no
// status at all, as a listener record that dropped its statuses at its size bound, or one of an
// earlier release, writes it. The daemon merges statuses per context, so ci stays kept as failed
// (classify.ApplySettlement) and the CI message names it. The settlement wakes the released
// implementer, and the wake task is built from the settlement's own list (SettlementWake), so it
// names no failed status; and tellChecks sends no CI message to the role the settlement woke, so
// that role never learns that ci failed.
func TestAWakeTaskNamesAKeptFailedStatusASettlementLeftOut(t *testing.T) {
	rig := newWakeRig(t)
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head1", Failing: []string{}, Cancelled: []string{},
		CheckedHead: "head1", CheckRuns: []record.AttemptRun{{Name: "build", ID: 1, Head: "head1", Conclusion: record.RunSucceeded}},
		Generation: 1, Snapshot: "head1-snapshot",
		Statuses: []record.CommitStatus{keptStatus("ci", "failure", 1, "head1"), keptStatus("deploy", "success", 1, "head1")}})
	leftOut := settlement("head1")
	leftOut.Generation, leftOut.Snapshot = 2, "head1-2"
	rig.apply("settled", leftOut)

	message := CISettledMessage("LEGION-208", recordedPR(t, rig.pool, "LEGION-208"), phase.Implementing)
	if len(message.NotPassedStatuses) != 1 {
		t.Fatalf("the CI message names %+v, want ci failed: this test proves nothing without it", message.NotPassedStatuses)
	}
	starts := wakeStarts(t, rig.pool)
	if len(starts) != 1 || !strings.Contains(starts[0].Task, "ci,failure") {
		t.Errorf("the woken implementer's task does not name the kept failed status ci, which the CI message of the same settlement names: %+v", starts)
	}
}
