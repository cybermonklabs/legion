package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// queueStartWithLastError queues a start row of role on the root issue with id and last error
// lastError, as a try of the outbox leaves it on its backoff: the last error is what tells a start
// that waits for its pod (pods.StartWaitsForPodPrefix) from a start that waits on anything else.
func queueStartWithLastError(t *testing.T, st *Store, id int64, role claim.Role, lastError string) {
	t.Helper()
	if _, err := st.Pool().Exec(context.Background(), `insert into outbox (id, kind, issue, payload, attempts, next_at, last_error)
		values ($1, 'supervise', $2, jsonb_build_object('op', 'start', 'tree', $2::text, 'role', $3::text, 'generation', 1), 1, now(), $4)`,
		id, rootIssue, string(role), lastError); err != nil {
		t.Fatal(err)
	}
}

// suspendedImplementer stores the root issue's implementer claim suspended by suspendedBy.
func suspendedImplementer(t *testing.T, st *Store, suspendedBy supervise.SuspendSource) {
	t.Helper()
	if err := st.PutClaim(context.Background(), supervise.Claim{Token: claim.Token("legion-legion-legion-208-implementer"),
		Project: "legion", Tree: rootIssue, Issue: rootIssue, Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1,
		State: supervise.StateSuspended, SuspendedBy: suspendedBy}); err != nil {
		t.Fatal(err)
	}
}

// A start that waits because the issue's pod lacks the slot of its claim or the role of its step
// is no delivery the pod
// waits for: the pod cannot run it, and only its round or its release lets it start, so main's
// rule for a pod with no live claim must not wait on it (ruling 18 point 3). The view names such
// a start's role, and the reason of the pod's running hold streak.
func TestTheIdleViewCountsAStartThatWaitsForItsPodAsNoDelivery(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	suspendedImplementer(t, st, supervise.SuspendedByOperator)
	queueStartWithLastError(t, st, 31, "checker", pods.StartWaitsForPodPrefix+"checker of LEGION-208: the pod pod-1 was built before the deployment defined checker")

	view := idleViewOf(t, st, rootIssue)
	if view.Waiting {
		t.Errorf("the view reads a delivery waiting, want none: the one start waits for its pod")
	}
	if !slices.Equal(view.StartsWaitingForPod, []claim.Role{"checker"}) {
		t.Errorf("the view's starts waiting for the pod = %v, want [checker]", view.StartsWaitingForPod)
	}
	if view.HeldFor != "" {
		t.Errorf("the view's hold = %q, want none", view.HeldFor)
	}

	queueStartWithLastError(t, st, 32, claim.RoleTester, "Dispatch is down")
	if view := idleViewOf(t, st, rootIssue); !view.Waiting {
		t.Errorf("the view reads no delivery waiting, want the tester's start that waits on Dispatch")
	}

	if err := st.RecordPodHold(ctx, PodHold{Project: "legion", Issue: rootIssue, Reason: "unowned_operator_suspended", At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if view := idleViewOf(t, st, rootIssue); view.HeldFor != "unowned_operator_suspended" {
		t.Errorf("the view's hold = %q, want unowned_operator_suspended", view.HeldFor)
	}
}

// The release reads the same set at its boundary and at its teardown as the rule that holds a
// pod: a start that waits for the pod, queued before the mark, does not overtake the release,
// where any other start queued before it does. The teardown reads the starts below the release's
// rows too (main's #5665), and the release of a pod no claim owns makes no boundary read, so the
// teardown alone is what lets such a pod go for the start that waits for it.
func TestAReleaseIsNotOvertakenByAStartThatWaitsForItsPod(t *testing.T) {
	const first, boundary, last = 18, 20, 22
	for _, tc := range []struct {
		name      string
		other     bool
		want      ReleaseAct
		wantCause ReleaseCause
	}{
		{name: "the start waits for its pod", want: ReleaseActs},
		{name: "and another start waits on Dispatch", other: true, want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			suspendedImplementer(t, st, supervise.SuspendedByReleased)
			queueStartWithLastError(t, st, first-2, "checker", pods.StartWaitsForPodPrefix+"checker of LEGION-208: the pod pod-1 was built before the deployment defined checker")
			if tc.other {
				queueStartWithLastError(t, st, first-1, claim.RoleTester, "Dispatch is down")
			}
			markRelease(t, st, first, boundary, last, 0, claim.RoleImplementer)
			decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Act != tc.want || decision.Cause != tc.wantCause {
				t.Errorf("the boundary = %s for %s, want %s for %s", decision.Act, decision.Cause, tc.want, tc.wantCause)
			}
			teardown, err := st.IssueRelease(ctx, "legion", IssueReleased{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: last},
				record.OutOfWorkflow)
			if err != nil {
				t.Fatal(err)
			}
			if teardown.Act != tc.want || teardown.Cause != tc.wantCause {
				t.Errorf("the teardown = %s for %s, want %s for %s", teardown.Act, teardown.Cause, tc.want, tc.wantCause)
			}
		})
	}
}
