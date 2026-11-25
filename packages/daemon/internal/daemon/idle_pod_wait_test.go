package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// startWaitsForPod queues a start of role on the rig's issue, the role's own claim's, as a try of
// the outbox leaves it when the issue's pod lacks the slot of the start's claim or the role of its
// step, here one name (the outbox's roleWaitsForItsPod): its last error begins with
// pods.StartWaitsForPodPrefix, and it is due again in an hour.
func (r *idleRig) startWaitsForPod(t *testing.T, role claim.Role) {
	t.Helper()
	if _, err := r.pool.Exec(context.Background(), `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ('supervise', $1, jsonb_build_object('op', 'start', 'tree', $2::text, 'role', $3::text, 'generation', 1), 1,
			now() + interval '1 hour', $4)`,
		r.issue.Key, r.issue.Tree, string(role),
		pods.StartWaitsForPodPrefix+string(role)+" of "+r.issue.Key+": the pod pod-1 was built before the deployment defined "+string(role)); err != nil {
		t.Fatalf("queue the waiting start of %s: %v", role, err)
	}
}

// podWaitHeldNotices are the reasons of the pod-wait-held controller notices queued for issue.
func podWaitHeldNotices(t *testing.T, r *idleRig) []string {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), `select coalesce(payload->>'reason', '') from outbox
		where issue = $1 and kind = $2 and payload->>'kind' = $3 order by id`,
		r.issue.Key, string(record.OutboxKindControllerNotice), string(record.PodWaitHeldNotice))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var reasons []string
	for rows.Next() {
		var reason string
		if err := rows.Scan(&reason); err != nil {
			t.Fatal(err)
		}
		reasons = append(reasons, reason)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return reasons
}

// An operator's suspension that holds a pod a role's start waits for is a wait nothing ends until
// the operator acts: main's rule keeps the pod for the suspension. The
// controller is told once for each streak of that hold, with what holds the pod, and told again
// only when a later streak begins.
func TestAnOperatorsSuspensionThatHoldsAWaitingPodIsToldOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("a start waits for the pod", func(t *testing.T) {
		rig := newIdleRig(t)
		rig.w.records = rig.issues
		rig.suspendedByOperator(t)
		rig.startWaitsForPod(t, "checker")

		rig.w.releaseUnownedPods(ctx)
		notices := podWaitHeldNotices(t, rig)
		if len(notices) != 1 {
			t.Fatalf("the first pass queued %d pod-wait-held notices, want one", len(notices))
		}
		for _, want := range []string{"checker", rig.issue.Key, "implementer (suspended)"} {
			if !strings.Contains(notices[0], want) {
				t.Errorf("the notice's reason %q does not name %q", notices[0], want)
			}
		}
		if holds := rig.records(t).Holds; len(holds) != 1 {
			t.Errorf("holds recorded = %+v, want the pass's one hold", holds)
		}

		rig.w.releaseUnownedPods(ctx)
		if notices := podWaitHeldNotices(t, rig); len(notices) != 1 {
			t.Errorf("after a second pass the outbox holds %d pod-wait-held notices, want the first alone", len(notices))
		}

		if err := rig.st.EndPodHoldStreaks(ctx, "legion", rig.issue.Key); err != nil {
			t.Fatal(err)
		}
		rig.w.releaseUnownedPods(ctx)
		if notices := podWaitHeldNotices(t, rig); len(notices) != 2 {
			t.Errorf("after the streak ended and the hold read again the outbox holds %d pod-wait-held notices, want two", len(notices))
		}
	})
	t.Run("no start waits", func(t *testing.T) {
		rig := newIdleRig(t)
		rig.w.records = rig.issues
		rig.suspendedByOperator(t)
		rig.w.releaseUnownedPods(ctx)
		if notices := podWaitHeldNotices(t, rig); len(notices) != 0 {
			t.Errorf("a pass over a pod no start waits for queued %d pod-wait-held notices, want none", len(notices))
		}
	})
}
