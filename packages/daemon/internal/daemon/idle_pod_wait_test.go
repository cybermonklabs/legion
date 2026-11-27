package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
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
// the operator acts: main's rule keeps the pod for the suspension (ruling 18 point 4). The
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

// suspendedByController leaves the rig's one live claim as the project's controller's own
// suspension leaves it (its `legion` tool's suspend, api's suspendAgent): suspended by the
// controller, holding no process, and backdated with the rest of the pod. It holds the pod as the
// operator's does (workflow.IdleHoldUnownedSuspensionHolds), and only a resume or a stop ends it.
func (r *idleRig) suspendedByController(t *testing.T) {
	t.Helper()
	if _, err := r.pool.Exec(context.Background(), `update claims set state = $2, suspended_by = $3,
		locator = null, updated_at = now() - interval '30 minutes' where token = $1`,
		string(r.token), string(supervise.StateSuspended), string(supervise.SuspendedByController)); err != nil {
		t.Fatalf("suspend the implementer as the controller does: %v", err)
	}
}

// architectSuspendedByController gives the rig's pod a second claim, the architect's, which the
// project's controller suspended: no process, and as quiet as the rest of the pod.
func (r *idleRig) architectSuspendedByController(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	token := mustClaimToken(t, r.issue.Key, claim.RoleArchitect)
	if err := r.st.PutClaim(ctx, supervise.Claim{Token: token, Project: "legion", Tree: r.issue.Tree, Issue: r.issue.Key,
		Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, ServingGeneration: 1,
		State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByController,
		Session: "ses-architect", SessionFile: "/tmp/architect.jsonl", PluginContract: api.DaemonAPIVersion()}); err != nil {
		t.Fatalf("seed the architect the controller suspended: %v", err)
	}
	if _, err := r.pool.Exec(ctx, `update claims set updated_at = now() - interval '30 minutes' where token = $1`,
		string(token)); err != nil {
		t.Fatalf("backdate the architect: %v", err)
	}
}

// Main's rule keeps a pod for a suspension the operator holds and for one the project's
// controller holds (workflow.IdleHoldUnownedSuspensionHolds), so a start that waits for that pod
// waits on either for as long as its holder leaves it. The controller is told of either, once for
// each streak of the hold, in words that name each holder with the claims it suspended: the
// operator's, which only the operator lifts, and its own, which it resumes or stops itself. A
// holder with no claim is never named, so no notice tells of an empty list. The hold's streak is
// recorded under either holder, and it is what keeps the next pass from telling again.
//
// The operator's case is the control that the words did not change for it; the controller's and
// the two together fail where the notice is told only of the operator's suspension.
func TestTheControllerIsToldOfASuspensionThatHoldsAWaitingPodByWhoeverHoldsIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		suspend func(t *testing.T, rig *idleRig)
		names   []string
		refuses []string
	}{
		{
			name:    "the operator's suspension",
			suspend: func(t *testing.T, rig *idleRig) { rig.suspendedByOperator(t) },
			names:   []string{"The operator suspended implementer (suspended)"},
			refuses: []string{"You suspended"},
		},
		{
			name:    "the controller's own suspension",
			suspend: func(t *testing.T, rig *idleRig) { rig.suspendedByController(t) },
			names:   []string{"You suspended implementer (suspended)"},
			refuses: []string{"The operator suspended"},
		},
		{
			name: "a suspension of each holder",
			suspend: func(t *testing.T, rig *idleRig) {
				rig.suspendedByOperator(t)
				rig.architectSuspendedByController(t)
			},
			names: []string{"The operator suspended implementer (suspended)", "You suspended architect (suspended)"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIdleRig(t)
			rig.w.records = rig.issues
			tc.suspend(t, rig)
			rig.startWaitsForPod(t, "checker")

			rig.w.releaseUnownedPods(ctx)
			notices := podWaitHeldNotices(t, rig)
			if len(notices) != 1 {
				t.Fatalf("the first pass queued %d pod-wait-held notices, want one", len(notices))
			}
			for _, want := range append([]string{"checker", rig.issue.Key}, tc.names...) {
				if !strings.Contains(notices[0], want) {
					t.Errorf("the notice's reason %q does not name %q", notices[0], want)
				}
			}
			for _, refused := range append([]string{"suspended .", "suspended ,"}, tc.refuses...) {
				if strings.Contains(notices[0], refused) {
					t.Errorf("the notice's reason %q says %q", notices[0], refused)
				}
			}
			wantStreak := func(passes int64) {
				t.Helper()
				holds := rig.records(t).Holds
				if len(holds) != 1 || holds[0].Reason != workflow.IdleHoldUnownedSuspensionHolds.String() || holds[0].Passes != passes {
					t.Errorf("holds recorded = %+v, want one streak of %s over %d passes", holds, workflow.IdleHoldUnownedSuspensionHolds, passes)
				}
			}
			wantStreak(1)

			rig.w.releaseUnownedPods(ctx)
			if notices := podWaitHeldNotices(t, rig); len(notices) != 1 {
				t.Errorf("after a second pass the outbox holds %d pod-wait-held notices, want the first alone", len(notices))
			}
			wantStreak(2)

			if err := rig.st.EndPodHoldStreaks(ctx, "legion", rig.issue.Key); err != nil {
				t.Fatal(err)
			}
			rig.w.releaseUnownedPods(ctx)
			if notices := podWaitHeldNotices(t, rig); len(notices) != 2 {
				t.Errorf("after the streak ended and the hold read again the outbox holds %d pod-wait-held notices, want two", len(notices))
			}
		})
	}
}
