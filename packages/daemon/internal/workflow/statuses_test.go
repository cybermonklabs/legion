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

// tell is messages with the CI message settled added for each role a settlement of
// seedSettlingRound's issue is told to - the architect, the tester and the implementer - in the
// order the daemon queues them.
func tell(t *testing.T, messages []record.RoleMessage, settled string) []record.RoleMessage {
	t.Helper()
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleTester, claim.RoleImplementer} {
		messages = append(messages, record.RoleMessage{Role: roleToken(t, role), Body: toldBody(settled)})
	}
	return messages
}

// The CI message names every commit status the settlement standing for the head names, each
// with its state as GitHub spells it, in context order, after the check runs and apart from them:
// one message to each of the three roles, as for a settlement that names none. A later settlement
// in which only a status changed state, a newer status of its context, says something new, and is
// told again.
func TestTheCIMessageNamesEachStatus(t *testing.T) {
	pool := migratedPool(t)
	seedSettlingRound(t, pool)
	engine := mergingEngine()
	messages := []record.RoleMessage{}
	for i, step := range []struct {
		fact    intake.Fact
		settled string
	}{
		{naming(settledAt("head", 1, "unit"), status("sched-suite", "error", 1), status("ci/untested-draft", "failure", 1), status("deploy", "success", 1)),
			`CI settled at head: "build" succeeded, "unit" failed; commit statuses: "ci/untested-draft" failure, "deploy" success, "sched-suite" error`},
		{naming(settledAt("head", 2, "unit"), status("ci/untested-draft", "failure", 1), status("deploy", "failure", 2), status("sched-suite", "error", 1)),
			`CI settled at head: "build" succeeded, "unit" failed; commit statuses: "ci/untested-draft" failure, "deploy" failure, "sched-suite" error`},
	} {
		applyRefusingNothing(t, pool, engine, step.fact)
		messages = tell(t, messages, step.settled)
		assertRoleMessages(t, pool, "LEGION-208", messages)
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

// The notice of a head GitHub will not merge names every commit status the record keeps for that
// head, with its state, after the head's own check runs, when the checked head is the current
// head; it names none of another head's. Three records: the head settled with two statuses; the
// branch moved on to a new head whose own settlement names none, which starts the head's fence
// afresh and the statuses with it; and the record an earlier release leaves after a rollback,
// whose checked head and runs it moved to the head while the statuses stayed stamped with the head
// before, which the stamp alone keeps out of the notice.
func TestTheStuckNoticeNamesStatusesOfTheCurrentHeadAlone(t *testing.T) {
	const earlier = "0000000000000000000000000000000000000abc"
	t.Run("the current head's statuses", func(t *testing.T) {
		pool := migratedPool(t)
		seedMergeStateRound(t, pool, record.MergeStateBlocked)
		apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
		apply("settled", naming(settledRunsAt(headOne, 1, []record.AttemptRun{{Name: "unit tests", ID: 2}}),
			status("deploy", "success", 1), status("ci", "failure", 1)))
		apply("approve", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: headOne,
			HeadSHA: headOne, Author: testReviewApp, Body: reviewBody("ship it", headOne)})
		told := reviewStuckNotices(t, pool)
		want := "GitHub reported on head " + headOne + `: "unit tests" succeeded; commit statuses: "ci" failure, "deploy" success. The daemon reads no ruleset`
		if len(told) != 1 || !strings.Contains(told[0].Reason, want) {
			t.Fatalf("review-stuck notices %+v, want one reading %q", told, want)
		}
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
		told := reviewStuckNotices(t, pool)
		want := "GitHub reported on head " + headTwo + `: "unit tests" succeeded. The daemon reads no ruleset`
		if len(told) != 1 || !strings.Contains(told[0].Reason, want) {
			t.Fatalf("review-stuck notices %+v, want one reading %q and naming no status of %s", told, want, headOne)
		}
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
		told := reviewStuckNotices(t, pool)
		want := "GitHub reported on head " + headOne + `: "unit tests" succeeded. The daemon reads no ruleset`
		if len(told) != 1 || !strings.Contains(told[0].Reason, want) {
			t.Fatalf("review-stuck notices %+v, want one reading %q and naming no status of %s", told, want, earlier)
		}
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
			want := make([]record.RoleMessage, 0, len(tc.roles))
			for _, role := range tc.roles {
				want = append(want, record.RoleMessage{Role: roleToken(t, role),
					Body: toldBody(`CI settled at head: "build" succeeded, "unit" succeeded; commit statuses: "ci" failure, "deploy" error`)})
			}
			assertRoleMessages(t, pool, "LEGION-208", want)
		})
	}
}

// The pull request keeps the newest status of each context the settlements of the head named, by
// created_at, so a later settlement that names a newer status of a context as success replaces the
// failure the record kept for it: the pull request keeps no failed status, and the CI message
// names the context as success and nothing as failed. A settlement after it whose list the daemon
// could not read names no status, so the green list stays as it is and no red name comes back,
// and it says nothing new, so nobody is told again.
func TestAStatusThatTurnsGreenLeavesNoRedName(t *testing.T) {
	pool := migratedPool(t)
	seedSettlingRound(t, pool)
	engine := mergingEngine()
	unread := naming(settledAt("head", 3))
	unread.StatusesUnread = true
	messages := []record.RoleMessage{}
	for i, step := range []struct {
		fact intake.Fact
		// settled is the line the roles are told, "" when the settlement says nothing new.
		settled string
		kept    []record.CommitStatus
	}{
		{naming(settledAt("head", 1), status("ci", "failure", 1)),
			`CI settled at head: "build" succeeded, "unit" succeeded; commit statuses: "ci" failure`,
			[]record.CommitStatus{keptStatus("ci", "failure", 1, "head")}},
		{naming(settledAt("head", 2), status("ci", "success", 2)),
			`CI settled at head: "build" succeeded, "unit" succeeded; commit statuses: "ci" success`,
			[]record.CommitStatus{keptStatus("ci", "success", 2, "head")}},
		{unread, "", []record.CommitStatus{keptStatus("ci", "success", 2, "head")}},
	} {
		applyRefusingNothing(t, pool, engine, step.fact)
		if step.settled != "" {
			messages = tell(t, messages, step.settled)
		}
		assertRoleMessages(t, pool, "LEGION-208", messages)
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
			if line := settledChecks(pr); strings.Contains(line, "failure") {
				t.Fatalf("after settlement %d the CI line reads %q, want no failed status in it", i+1, line)
			}
		}
	}
}

// The task of a role that a settlement wakes names each commit status the settlement names failed,
// in context order, after each of the three texts it gives today: the failed check runs, the
// cancelled ones, and, where neither, that the head passed every check run. A settlement whose list
// the daemon could not read names the failed statuses the pull request keeps for the head, which
// applying it leaves as they are, rather than telling the role the head passed every check. A
// settlement that names no failed status gives today's text, "passed every check" included.
func TestTheWakeTaskNamesAFailedStatus(t *testing.T) {
	checks := func(failing, cancelled []string, statuses ...record.CommitStatus) intake.PullRequestChecks {
		fact := settlement("head1", failing...)
		fact.Cancelled, fact.Statuses = cancelled, statuses
		return fact
	}
	for _, shape := range []struct {
		name   string
		fact   intake.PullRequestChecks
		detail string
	}{
		{"a failed check run", checks([]string{"build"}, nil, status("deploy", "success", 1), status("ci", "failure", 1)),
			"head1 failed build; failed commit status ci"},
		{"a cancelled check run", checks(nil, []string{"build"}, status("sched-suite", "error", 1), status("ci/untested-draft", "failure", 1)),
			"head1 had build cancelled; failed commit statuses ci/untested-draft, sched-suite"},
		{"no failed check run", checks(nil, nil, status("ci/untested-draft", "failure", 1)),
			"head1 passed every check run; failed commit status ci/untested-draft"},
	} {
		t.Run(shape.name, func(t *testing.T) {
			if got := settlementDetail(shape.fact); got != shape.detail {
				t.Fatalf("settlementDetail = %q, want %q", got, shape.detail)
			}
			rig := newWakeRig(t)
			rig.apply("settled", shape.fact)
			oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeChecks, "The wake is a CI settlement on your pull request's head: "+shape.detail)
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
		oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeChecks,
			"The wake is a CI settlement on your pull request's head: head1 passed every check run; failed commit status ci")
	})
	for _, today := range []struct {
		name   string
		fact   intake.PullRequestChecks
		detail string
	}{
		{"no failed status", checks(nil, nil, status("deploy", "success", 1)), "head1 passed every check"},
		{"no status", checks(nil, nil), "head1 passed every check"},
		{"a failed check run and no failed status", checks([]string{"build"}, nil, status("deploy", "success", 1)), "head1 failed build"},
		{"a cancelled check run and no status", checks(nil, []string{"build"}), "head1 had build cancelled"},
	} {
		if got := settlementDetail(today.fact); got != today.detail {
			t.Errorf("%s: settlementDetail = %q, want today's %q", today.name, got, today.detail)
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
	for _, tc := range []struct {
		name string
		// newerRuns and newer are the check runs and the statuses the newer settlement names, and
		// told what the roles are told of it; olderRuns are the check runs of the settlement that
		// arrives after it, and toldOlder what the roles are told of that one.
		newerRuns, olderRuns []record.AttemptRun
		newer                []record.CommitStatus
		told, toldOlder      string
	}{
		{"a status created later than the kept one", build2, build1, []record.CommitStatus{status("ci", "success", 1)},
			`CI settled at head: "build" succeeded, "e2e" cancelled; commit statuses: "ci" success`,
			`CI settled at head: "build" succeeded, "e2e" cancelled; commit statuses: "ci" failure`},
		{"a context the newer settlement left out", build2, build1, nil,
			`CI settled at head: "build" succeeded, "e2e" cancelled`,
			`CI settled at head: "build" succeeded, "e2e" cancelled; commit statuses: "ci" failure`},
		{"the same runs as the newer settlement but the check name it added", []record.AttemptRun{{Name: "build", ID: 2}, {Name: "lint", ID: 3}}, build2, nil,
			`CI settled at head: "build" succeeded, "e2e" cancelled, "lint" succeeded`,
			`CI settled at head: "build" succeeded, "e2e" cancelled, "lint" succeeded; commit statuses: "ci" failure`},
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
			messages := []record.RoleMessage{}

			applyRefusingNothing(t, pool, mergingEngineAt(base), newer)
			settled := recordedPR(t, pool, "LEGION-208")
			if !reflect.DeepEqual(settled.CheckRuns, fence) || len(settled.Failing) != 0 || !reflect.DeepEqual(settled.Cancelled, []string{"e2e"}) ||
				settled.Generation != 6 || settled.Snapshot != "head-6" {
				t.Fatalf("the newer settlement left %+v; want the runs %+v, e2e cancelled at generation 6, head-6: this test proves nothing without it", settled, fence)
			}
			messages = tell(t, messages, tc.told)
			assertRoleMessages(t, pool, "LEGION-208", messages)

			applyRefusingNothing(t, pool, mergingEngineAt(base.Add(time.Minute)), older)
			want := settled
			want.Statuses = []record.CommitStatus{keptStatus("ci", "failure", 2, "head")}
			if got := recordedPR(t, pool, "LEGION-208"); !reflect.DeepEqual(got, want) {
				t.Fatalf("after the older settlement the pull request is\n  %+v\nwant the newer settlement's record with ci's failure its one change:\n  %+v", got, want)
			}
			messages = tell(t, messages, tc.toldOlder)
			assertRoleMessages(t, pool, "LEGION-208", messages)
			moved := issueRecord(t, pool, "LEGION-208").MovedAt

			applyRefusingNothing(t, pool, mergingEngineAt(base.Add(2*time.Minute)), newer)
			if got := recordedPR(t, pool, "LEGION-208"); !reflect.DeepEqual(got, want) {
				t.Fatalf("after the newer settlement delivered again the pull request is\n  %+v\nwant it as it was:\n  %+v", got, want)
			}
			assertRoleMessages(t, pool, "LEGION-208", messages)
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
	messages := []record.RoleMessage{}
	for _, step := range []struct {
		fact    intake.Fact
		settled string
	}{
		{naming(settledAt("head", 4), status("ci", "success", 1)),
			`CI settled at head: "build" succeeded, "unit" succeeded; commit statuses: "ci" success`},
		{recreated, `CI settled at head: "build" succeeded, "unit" succeeded; commit statuses: "ci" failure`},
	} {
		applyRefusingNothing(t, pool, engine, step.fact)
		messages = tell(t, messages, step.settled)
		assertRoleMessages(t, pool, "LEGION-208", messages)
	}
	pr := recordedPR(t, pool, "LEGION-208")
	if want := []record.CommitStatus{keptStatus("ci", "failure", 2, "head")}; !reflect.DeepEqual(pr.Statuses, want) || pr.Generation != 4 || pr.Snapshot != "head-4" {
		t.Fatalf("the pull request keeps %+v at generation %d, snapshot %q; want %+v at its own 4, head-4: the recreated record's settlement changes its statuses alone",
			pr.Statuses, pr.Generation, pr.Snapshot, want)
	}
}
