package workflow

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// naming is fact, a settlement of settledAt or settledRunsAt, naming statuses: each a context and
// its state, as the decoder reads the listener's field.
func naming(fact intake.Fact, statuses ...record.CommitStatus) intake.PullRequestChecks {
	checks := fact.(intake.PullRequestChecks)
	checks.Statuses = statuses
	return checks
}

// status is the commit status context in state, as a settlement names it.
func status(context, state string) record.CommitStatus {
	return record.CommitStatus{Context: context, State: state}
}

// The CI message names every commit status the settlement standing for the head names, each
// with its state as GitHub spells it, in context order, after the check runs and apart from them:
// one message to each of the three roles, as for a settlement that names none. A later settlement
// that changes only a status's state says something new, and is told again.
func TestTheCIMessageNamesEachStatus(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim"})
	engine := mergingEngine()
	messages := []record.RoleMessage{}
	for i, step := range []struct {
		fact    intake.Fact
		settled string
	}{
		{naming(settledAt("head", 1, "unit"), status("sched-suite", "error"), status("ci/untested-draft", "failure"), status("deploy", "success")),
			`CI settled at head: "build" succeeded, "unit" failed; commit statuses: "ci/untested-draft" failure, "deploy" success, "sched-suite" error`},
		{naming(settledAt("head", 2, "unit"), status("ci/untested-draft", "failure"), status("deploy", "failure"), status("sched-suite", "error")),
			`CI settled at head: "build" succeeded, "unit" failed; commit statuses: "ci/untested-draft" failure, "deploy" failure, "sched-suite" error`},
	} {
		applyRefusingNothing(t, pool, engine, step.fact)
		for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleTester, claim.RoleImplementer} {
			messages = append(messages, record.RoleMessage{Role: roleToken(t, role), Body: toldBody(step.settled)})
		}
		assertRoleMessages(t, pool, "LEGION-208", messages)
		if got := issuePhase(t, pool); got != phase.Testing {
			t.Fatalf("after settlement %d the issue is in %s, want testing", i+1, got)
		}
	}
	want := []record.CommitStatus{{Context: "ci/untested-draft", State: "failure", Head: "head"},
		{Context: "deploy", State: "failure", Head: "head"}, {Context: "sched-suite", State: "error", Head: "head"}}
	if got := recordedPR(t, pool, "LEGION-208").Statuses; !reflect.DeepEqual(got, want) {
		t.Fatalf("the pull request keeps %+v, want the last settlement's %+v, each stamped with the head", got, want)
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
			status("deploy", "success"), status("ci", "failure")))
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
		apply("settled", naming(settledRunsAt(headOne, 1, []record.AttemptRun{{Name: "unit tests", ID: 2}}), status("ci", "failure")))
		apply("push", intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: headOne, After: headTwo, Pusher: "legion-implementer[bot]"})
		apply("sync", intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: headTwo})
		if got := recordedPR(t, pool, "LEGION-208"); !reflect.DeepEqual(got.Statuses, []record.CommitStatus{{Context: "ci", State: "failure", Head: headOne}}) {
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
			Statuses: []record.CommitStatus{{Context: "ci", State: "failure", Head: earlier}}})
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
			applyRefusingNothing(t, pool, mergingEngine(), naming(settledAt("head", 1), status("ci", "failure"), status("deploy", "error")))

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

// A settlement's list is the whole list GitHub holds for the head, so a later settlement that
// names a context as success replaces the failure the record kept for it: the pull request keeps
// no failed status, and the CI message names the context as success and nothing as failed. A
// settlement after it whose list the daemon could not read keeps the green list as it is, so no
// red name comes back, and says nothing new, so nobody is told again.
func TestAStatusThatTurnsGreenLeavesNoRedName(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim"})
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
		{naming(settledAt("head", 1), status("ci", "failure")),
			`CI settled at head: "build" succeeded, "unit" succeeded; commit statuses: "ci" failure`,
			[]record.CommitStatus{{Context: "ci", State: "failure", Head: "head"}}},
		{naming(settledAt("head", 2), status("ci", "success")),
			`CI settled at head: "build" succeeded, "unit" succeeded; commit statuses: "ci" success`,
			[]record.CommitStatus{{Context: "ci", State: "success", Head: "head"}}},
		{unread, "", []record.CommitStatus{{Context: "ci", State: "success", Head: "head"}}},
	} {
		applyRefusingNothing(t, pool, engine, step.fact)
		if step.settled != "" {
			for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleTester, claim.RoleImplementer} {
				messages = append(messages, record.RoleMessage{Role: roleToken(t, role), Body: toldBody(step.settled)})
			}
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
// cancelled ones, and, where neither, that the head passed every check run. A settlement that names
// no failed status gives today's text, "passed every check" included.
func TestTheWakeTaskNamesAFailedStatus(t *testing.T) {
	checks := func(failing, cancelled []string, statuses ...record.CommitStatus) intake.PullRequestChecks {
		fact := settlement("head1", failing...)
		fact.Cancelled, fact.Statuses = cancelled, statuses
		return fact
	}
	shapes := []struct {
		name   string
		fact   intake.PullRequestChecks
		detail string
	}{
		{"a failed check run", checks([]string{"build"}, nil, status("deploy", "success"), status("ci", "failure")),
			"head1 failed build; failed commit status ci"},
		{"a cancelled check run", checks(nil, []string{"build"}, status("sched-suite", "error"), status("ci/untested-draft", "failure")),
			"head1 had build cancelled; failed commit statuses ci/untested-draft, sched-suite"},
		{"no failed check run", checks(nil, nil, status("ci/untested-draft", "failure")),
			"head1 passed every check run; failed commit status ci/untested-draft"},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			if got := settlementDetail(shape.fact); got != shape.detail {
				t.Fatalf("settlementDetail = %q, want %q", got, shape.detail)
			}
			rig := newWakeRig(t)
			rig.apply("settled", shape.fact)
			oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeChecks, "The wake is a CI settlement on your pull request's head: "+shape.detail)
		})
	}
	for _, today := range []struct {
		name   string
		fact   intake.PullRequestChecks
		detail string
	}{
		{"no failed status", checks(nil, nil, status("deploy", "success")), "head1 passed every check"},
		{"no status", checks(nil, nil), "head1 passed every check"},
		{"a failed check run and no failed status", checks([]string{"build"}, nil, status("deploy", "success")), "head1 failed build"},
		{"a cancelled check run and no status", checks(nil, []string{"build"}), "head1 had build cancelled"},
	} {
		if got := settlementDetail(today.fact); got != today.detail {
			t.Errorf("%s: settlementDetail = %q, want today's %q", today.name, got, today.detail)
		}
	}
}
