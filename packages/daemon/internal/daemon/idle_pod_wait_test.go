package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
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
		pods.StartWaitsForPodPrefix+string(role)+" of "+r.issue.Key+": the pod pod-1 was built without "+string(role)); err != nil {
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

// The notice of a pod an operator's suspension holds names each start that waits for the pod and
// each claim the operator suspended as `legion state` names a claim (api.ClaimNameInIssue). Two
// claims of one issue can hold one role: here the start of `implementer-2`, a second agent of the
// implementer role, waits beside a start of the role's own claim, and the operator suspended the
// claims of both. The second agent's start and its suspension are each named by the role with the
// claim's token beside it. The role's own claim, the control, is named by its role alone: the
// notice sets its token beside no name.
func TestThePodWaitHeldNoticeNamesEachStartAndSuspensionByItsClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rig := newIdleRig(t)
	rig.w.records = rig.issues
	second, err := claim.AgentToken("legion", rig.issue.Key, "implementer-2")
	if err != nil {
		t.Fatal(err)
	}
	other := rig.machine.Claim()
	other.Token, other.Session, other.SessionFile, other.Pending = second, "ses-implementer-2",
		"/tmp/implementer-2.jsonl", nil
	if err := rig.st.PutClaim(ctx, other); err != nil {
		t.Fatalf("seed the second agent's claim: %v", err)
	}
	for _, token := range []claim.Token{rig.token, second} {
		if _, err := rig.pool.Exec(ctx, `update claims set state = $2, suspended_by = $3,
			locator = null, updated_at = now() - interval '30 minutes' where token = $1`,
			string(token), string(supervise.StateSuspended), string(supervise.SuspendedByOperator),
		); err != nil {
			t.Fatalf("suspend %s as an operator does: %v", token, err)
		}
		payload, err := json.Marshal(map[string]any{"op": "start", "tree": rig.issue.Tree,
			"role": claim.RoleImplementer, "claim": token, "generation": 1})
		if err != nil {
			t.Fatal(err)
		}
		_, agent, _ := token.Cut()
		text := pods.StartWaitsForPodPrefix + "implementer of " + rig.issue.Key +
			": the pod pod-1 was built without " + string(agent)
		if _, err := rig.pool.Exec(ctx, `insert into outbox
			(kind, issue, payload, attempts, next_at, last_error)
			values ('supervise', $1, $2::jsonb, 1, now() + interval '1 hour', $3)`,
			rig.issue.Key, string(payload), text); err != nil {
			t.Fatalf("queue the waiting start of %s: %v", token, err)
		}
	}

	rig.w.releaseUnownedPods(ctx)
	notices := podWaitHeldNotices(t, rig)
	if len(notices) != 1 {
		t.Fatalf("the pass queued %d pod-wait-held notices, want one", len(notices))
	}
	reason := notices[0]
	named := api.ClaimNameInIssue(claim.RoleImplementer, second)
	// The second agent is named as a start that waits and as a suspension, so in two places at
	// least.
	if got := strings.Count(reason, named); got < 2 {
		t.Errorf("the notice's reason %q names %q %d times, want its start and its suspension",
			reason, named, got)
	}
	if !strings.Contains(reason, string(supervise.StateSuspended)) ||
		!strings.Contains(reason, rig.issue.Key) {
		t.Errorf("the notice's reason %q does not name the state %s and the issue %s", reason,
			supervise.StateSuspended, rig.issue.Key)
	}
	if own := "(claim " + string(rig.token) + ")"; strings.Contains(reason, own) {
		t.Errorf("the notice's reason %q names the role's own claim %s by its token, "+
			"want it named by its role alone", reason, rig.token)
	}
}

// The notice of a pod an operator's suspension holds names each waiting start by the claim it
// starts, by the one rule the outbox's start and the served state name it by: the claim its row
// names, else the claim the record gives its role's step. A start whose claim that rule cannot give,
// which only a role no token spells gives, is a fault of the record, which the served state is
// refused for: the pass queues no notice that names such a start, and its warning names the start's
// role and why. The control is TestAnOperatorsSuspensionThatHoldsAWaitingPodIsToldOnce, whose start
// of a role the grammar spells is told. The fault: the rule's error was dropped, and the notice named
// the start by its role alone, as though it started the role's own claim.
func TestThePodWaitHeldNoticeNamesNoStartWhoseClaimNoTokenSpells(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	rig.w.records = rig.issues
	logs := &syncBuffer{}
	rig.w.log = slog.New(slog.NewTextHandler(logs, nil))
	rig.suspendedByOperator(t)
	rig.startWaitsForPod(t, "Checker")

	rig.w.releaseUnownedPods(context.Background())
	if notices := podWaitHeldNotices(t, rig); len(notices) != 0 {
		t.Errorf("the pass queued the pod-wait-held notices %q, want none: no token spells the claim of the start of Checker", notices)
	}
	if logged := logs.String(); !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, `\"Checker\" is not a role`) {
		t.Errorf("the pass logged %q, want a warning that names the start of Checker and why its claim has no token", logged)
	}
}
