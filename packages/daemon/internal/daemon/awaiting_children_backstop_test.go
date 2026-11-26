package daemon

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The release of a waiting parent's quiet pod arms no backstop for any role of it
// (dispatch://INFRA-3320 R11): the phase no role works has no worker whose wake is due
// (workflow.OpenStepsOf answers none for awaiting_children, where the parent runs no step:
// record.RunningOf), so the release's arming (outbox.armBackstops) cancels the backstops an
// earlier release armed, as every arming does, and arms none. The control is the same release of
// the same issue in implementing, which arms the released implementer's.
func TestTheReleaseOfAWaitingParentsPodArmsNoBackstop(t *testing.T) {
	for _, tc := range []struct {
		name  string
		at    phase.Phase
		armed int
	}{
		{name: "in implementing, the control", at: phase.Implementing, armed: 1},
		{name: "in awaiting_children", at: phase.AwaitingChildren},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: tc.at, Generation: 1, Status: "in_progress", Rank: "U",
				LastDispatchSeq: 1}
			if tc.at == phase.AwaitingChildren {
				waited := time.Now().UTC().Add(-time.Hour)
				issue.ChildrenWait = &record.ChildrenWait{Start: phase.Implementing, Task: "Continue root. Issue: LEGION-208. Phase: implementing.",
					Reason: "the admission pass stopped the implementer's implementing phase, while these Dispatch children of LEGION-208 were open: LEGION-209 (todo)",
					Since:  waited}
				issue.ChildrenWaitedAt = &waited
			}
			putOutboxIssue(t, pool, records, issue)
			st := outboxTreeStore(t, pool, issue.Key)
			// The implementer as the idle release leaves it: suspended with the source
			// `released`, serving the issue's generation.
			implementer := mustClaimToken(t, issue.Key, claim.RoleImplementer)
			if err := st.PutClaim(ctx, supervise.Claim{Token: implementer, Project: "legion",
				Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, ServingGeneration: 1,
				State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased, Session: "ses-implementer",
				SessionFile: "/tmp/implementer.jsonl", PluginContract: api.DaemonAPIVersion()}); err != nil {
				t.Fatalf("seed the released implementer: %v", err)
			}
			// The backstop an earlier release of this pod armed for the implementer's claim,
			// still open.
			if err := st.ArmPodBackstop(ctx, store.PodWakeBackstop{Project: "legion", Issue: issue.Key, Claim: implementer,
				Role: claim.RoleImplementer, DueAt: time.Now().UTC().Add(30 * time.Minute)}); err != nil {
				t.Fatalf("arm the earlier release's backstop: %v", err)
			}
			runner := &outbox{pool: pool, records: records, project: "legion", dispatchProject: "LEGION", trees: st,
				log: quietLogger(), backstop: time.Hour, now: time.Now}

			// The release arms from the issue as the record reads it back (outbox.releaseIssue),
			// whose running steps (record.RunningOf) name the step a backstop wakes.
			if err := runner.armBackstops(ctx, recordOf(t, pool, records, issue.Key)); err != nil {
				t.Fatalf("arm the release's backstops: %v", err)
			}

			var open int
			if err := pool.QueryRow(ctx, `select count(*) from pod_wake_backstops where issue = $1 and not fired and role = 'implementer'
				and due_at > now() + interval '45 minutes'`, issue.Key).Scan(&open); err != nil {
				t.Fatalf("count the open backstops: %v", err)
			}
			var earlier int
			if err := pool.QueryRow(ctx, `select count(*) from pod_wake_backstops where issue = $1 and not fired
				and due_at <= now() + interval '45 minutes'`, issue.Key).Scan(&earlier); err != nil {
				t.Fatalf("count the earlier backstops: %v", err)
			}
			if open != tc.armed || earlier != 0 {
				t.Fatalf("after the release in %s: %d backstops of this release open and %d of the earlier one; want %d and none",
					tc.at, open, earlier, tc.armed)
			}
		})
	}
}

// The daemon releases the quiet pod of a parent waiting on its open Dispatch children as it
// releases any quiet pod (dispatch://INFRA-3320 R11): the idle rule reads the issue's phase for
// nothing but a phase reminder (workflow.IdleRelease), so the idle pass
// (workflowRuntime.releaseIdlePods) writes the same three rows for the same quiet pod — the save,
// the role stop and the teardown — and the drain leaves its implementer suspended by `released`
// and its Sandbox torn down by the release's own teardown. That teardown arms no backstop for
// the waiting parent, since no role works awaiting_children (outbox.armBackstops,
// workflow.OpenStepsOf), where the same release in implementing, the control, arms the released
// implementer's.
func TestAWaitingParentsQuietPodIsReleasedAsAnyQuietPodAndArmsNoBackstop(t *testing.T) {
	for _, tc := range []struct {
		name  string
		at    phase.Phase
		armed int
	}{
		{name: "in implementing, the control", at: phase.Implementing, armed: 1},
		{name: "in awaiting_children", at: phase.AwaitingChildren},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rig := newIdleRigIn(t, tc.at)
			rig.runner.backstop = time.Hour

			queued := rig.queuedBy(t, func() { rig.w.releaseIdlePods(ctx) })
			if want := []string{"issue_bundle", "supervise:release", "issue_release"}; !slices.Equal(queued, want) {
				t.Fatalf("queued rows in %s = %v, want the save, then the role stop, then the teardown", tc.at, queued)
			}

			rig.drain(t)

			if got := rig.machine.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByReleased {
				t.Fatalf("the released implementer in %s = %s by %s, want suspended by released", tc.at, got.State, got.SuspendedBy)
			}
			if _, suspended := rig.rt.reads(); suspended != 1 {
				t.Fatalf("Sandbox teardowns in %s = %d, want one", tc.at, suspended)
			}
			if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"ReleaseIssue"}) {
				t.Fatalf("teardowns asked in %s = %v, want the release's alone: the issue stays in the workflow", tc.at, asked)
			}
			if rig.marked(t) {
				t.Fatalf("the finished release in %s left its mark behind", tc.at)
			}
			var open int
			if err := rig.pool.QueryRow(ctx, `select count(*) from pod_wake_backstops where issue = $1 and not fired`,
				rig.issue.Key).Scan(&open); err != nil {
				t.Fatalf("count the open backstops: %v", err)
			}
			if open != tc.armed {
				t.Fatalf("open backstops after the release in %s = %d, want %d", tc.at, open, tc.armed)
			}
		})
	}
}
