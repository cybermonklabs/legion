package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// failedImplementer is issue's implementer failed under cause with budgets, its session kept:
// the claim a production check's look finds when its checking role failed in an earlier look.
func failedImplementer(t *testing.T, sup *supervisor, issue record.Issue, cause supervise.FailureCause, budgets supervise.Budgets) *supervise.Machine {
	t.Helper()
	token, err := claim.NewToken("legion", issue.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := sup.Create(context.Background(), supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleImplementer, Generation: 1,
		State: supervise.StateFailed, Failure: "the claim failed for the test", FailureCause: cause, FailedAt: time.Now().Add(-time.Hour), Budgets: budgets,
		Session: "ses_implementer", SessionFile: "/sessions/implementer.jsonl"}, "")
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

// checkLookRetry runs the start of look 1 of root's open production check against its
// implementer failed under cause with budgets, and answers the claim after it and the runtime's
// resumes of it. The outbox supervisor's limits are 2 launch failures, 2 prompt failures and 2
// prompt retirements (newOutboxSupervisor).
func checkLookRetry(t *testing.T, cause supervise.FailureCause, budgets supervise.Budgets) (supervise.Claim, int) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := checkRoot()
	putOutboxIssue(t, pool, records, root)
	putProductionCheck(t, pool, records, openCheck(root, "a1b2c3d4"))
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	machine := failedImplementer(t, sup, root, cause, budgets)
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), log: quietLogger(), now: time.Now}
	if err := runner.execute(context.Background(), checkStart(t, root, 1)); err != nil {
		t.Fatalf("the look's start against the failed implementer = %v, want it retried", err)
	}
	return machine.Claim(), len(rt.CallsOf("Resume"))
}

// A production check look's start against its checking role's claim failed at a budget's limit is
// the half-open trial of a role that keeps failing: it retries the claim with one attempt left of
// each budget the failure spent, not a fresh budget, so one more failure of the same kind fails the
// claim again and the look ends at once, and every count the failure did not spend stands as the
// failure left it. The launch budget for a failure whose launch failures reached the launch
// failure limit, the death budget for one whose deaths did (measured against the same limit), and
// both prompt counts for one whose prompt retirements ran out. The retry relaunches the recorded
// session, as every retry that asks for no new one does. At main the retry zeroed every budget
// (supervise's retry), so the role was launched a whole budget's worth of times at every look.
func TestCheckLookStartRetriesAFailedClaimWithOneAttemptLeft(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		cause  supervise.FailureCause
		failed supervise.Budgets
		want   supervise.Budgets
	}{
		{name: "launch failures at the limit", cause: supervise.CauseDied,
			failed: supervise.Budgets{LaunchFailures: 2}, want: supervise.Budgets{LaunchFailures: 1}},
		{name: "launch failures at the limit, a prompt failure beside them", cause: supervise.CauseNeverRegistered,
			failed: supervise.Budgets{LaunchFailures: 2, PromptFailures: 1}, want: supervise.Budgets{LaunchFailures: 1, PromptFailures: 1}},
		{name: "deaths at the launch failure limit", cause: supervise.CauseDied,
			failed: supervise.Budgets{LaunchFailures: 1, Deaths: 2}, want: supervise.Budgets{LaunchFailures: 1, Deaths: 1}},
		{name: "prompt retirements at their limit", cause: supervise.CausePromptRetiresExhausted,
			failed: supervise.Budgets{PromptFailures: 2, PromptRetires: 2}, want: supervise.Budgets{PromptFailures: 1, PromptRetires: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, resumes := checkLookRetry(t, tc.cause, tc.failed)
			if got.State == supervise.StateFailed || resumes != 1 {
				t.Fatalf("the implementer after the look's start = %s with %d resumes, want it retried on its session once", got.State, resumes)
			}
			if got.Budgets != tc.want {
				t.Fatalf("the implementer's budgets after the look's retry = %+v, want %+v: one attempt left of each budget the failure spent, the rest as it left them", got.Budgets, tc.want)
			}
			if got.Failure != "" || got.FailureCause != "" {
				t.Fatalf("the implementer after the look's retry still records its failure %q (%s)", got.Failure, got.FailureCause)
			}
		})
	}
}

// A failure that spent no budget the claim records is retried by a look's start as every retry
// is, with a fresh run: a provider error that cannot clear, whose counts are the run of refused
// turns the claim keeps apart from its budgets, and a launch the runtime refused in a way no try
// changes (a system prompt past the start frame), which fails the claim with no budget spent. The
// one-attempt rule has nothing to keep of either, and the look after the next failure backs off
// all the same (workflow's TestACheckRoleFailureDoublesTheNextLookInterval).
func TestACheckRoleProviderErrorIsRetriedWithAFreshRunAtTheNextLook(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		cause  supervise.FailureCause
		failed supervise.Budgets
	}{
		{name: "a provider error", cause: supervise.CauseProviderError, failed: supervise.Budgets{LaunchFailures: 1, PromptFailures: 1}},
		{name: "a prompt refused at launch", cause: supervise.CauseLaunchRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, resumes := checkLookRetry(t, tc.cause, tc.failed)
			if got.State == supervise.StateFailed || resumes != 1 {
				t.Fatalf("the implementer after the look's start = %s with %d resumes, want it retried on its session once", got.State, resumes)
			}
			if got.Budgets != (supervise.Budgets{}) {
				t.Fatalf("the implementer's budgets after the look's retry of %s = %+v, want a fresh run", tc.cause, got.Budgets)
			}
		})
	}
}

// checkLookStart is the start row the workflow queued for look of issue's production check.
func checkLookStart(t *testing.T, r *adoptionRig, issue string, look int) record.OutboxRow {
	t.Helper()
	rows, err := r.pool.Query(r.ctx, `select id, kind, issue, payload from outbox
		where kind = 'supervise' and issue = $1 and payload->>'op' = 'start' and (payload->>'checkLook')::int = $2 order by id`, issue, look)
	if err != nil {
		t.Fatalf("read the start rows of %s: %v", issue, err)
	}
	starts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (record.OutboxRow, error) {
		var out record.OutboxRow
		return out, row.Scan(&out.ID, &out.Kind, &out.Issue, &out.Payload)
	})
	if err != nil || len(starts) != 1 {
		t.Fatalf("the start rows of %s's look %d = %+v (%v), want one", issue, look, starts, err)
	}
	return starts[0]
}

// The half-open trial of a checking role that keeps failing is that one attempt and no more. A
// look's start retries the claim, failed at its launch budget, with one attempt left; the runtime
// refuses that launch as it refused the ones before, so the claim fails again at once, and its
// failure ends the look it was started for as the second failed look in a row, which names the
// next look twice the look bound out. The same start row run again, as the outbox runs a row again
// after an error, finds its look ended and launches nothing: the runtime is asked for no launch past
// the one attempt, and the claim stays failed for the next look's start. A retry at a fresh budget
// would have launched the role twice in the look, and at a start that is not fenced on its look,
// again at the rerun.
func TestACheckRolesOneAttemptThatFailsEndsTheLookAndItsStartRunAgainLaunchesNothing(t *testing.T) {
	t.Parallel()
	r := newAdoptionRig(t)
	root := checkRoot()
	check := openCheck(root, "a1b2c3d4")
	check.LookOpen, check.FailedLooks = false, 1
	r.put(root)
	putProductionCheck(t, r.pool, r.records, check)
	r.openTree(root.Key)
	failed := r.suspended(root.Key, root.Key, claim.RoleImplementer)
	failed.State, failed.SuspendedBy = supervise.StateFailed, ""
	failed.Failure, failed.FailureCause, failed.FailedAt = "the claim failed for the test", supervise.CauseNeverRegistered, time.Now().Add(-time.Hour)
	failed.Budgets = supervise.Budgets{LaunchFailures: 2}
	r.admitClaims(failed)
	r.restoreClaims()
	refused := fake.SpawnResult{Err: errors.New("the pod died in workspace-init")}
	r.rt.ScriptResume(refused, refused, refused)
	launches := func() int { return len(r.rt.CallsOf("Resume")) + len(r.rt.CallsOf("Spawn")) }

	r.apply("check-wake", intake.CheckWakeDue{Issue: root.Key, Look: 1, OpenedAt: check.OpenedAt, Reason: "the retry of a look nobody reported: claim failed"})
	start := checkLookStart(t, r, root.Key, 2)
	started := time.Now()
	if err := r.runner.execute(r.ctx, start); err != nil {
		t.Logf("look 2's start against the failed implementer = %v", err)
	}
	r.settle()
	if got := launches(); got != 1 {
		t.Fatalf("the runtime was asked for %d launches by look 2's start, want the one attempt", got)
	}
	if c := r.claimOf(root.Key, claim.RoleImplementer); c.State != supervise.StateFailed || c.Budgets.LaunchFailures != 2 {
		t.Fatalf("the implementer after its one attempt failed = %s with budgets %+v, want it failed again at its launch limit of 2", c.State, c.Budgets)
	}
	ended := readProductionCheck(t, r.pool, r.records, root.Key)
	if !ended.Open() || ended.LookOpen || ended.Looks != 2 || ended.FailedLooks != 2 || ended.WakeAt == nil ||
		ended.WakeAt.Before(started.Add(2*workflow.CheckLookBound)) || ended.WakeAt.After(time.Now().Add(2*workflow.CheckLookBound)) {
		t.Fatalf("the check after its implementer failed in look 2 = %+v, want look 2 ended, 2 failed looks in a row and the next look %s out", ended, 2*workflow.CheckLookBound)
	}

	if err := r.runner.execute(r.ctx, start); err != nil {
		t.Fatalf("look 2's start run again = %v, want it to finish without acting", err)
	}
	r.settle()
	if got := launches(); got != 1 {
		t.Fatalf("the runtime was asked for %d launches after look 2's start ran again, want still the one attempt", got)
	}
	if c := r.claimOf(root.Key, claim.RoleImplementer); c.State != supervise.StateFailed {
		t.Fatalf("the implementer after look 2's start ran again = %s, want it still failed", c.State)
	}
}
