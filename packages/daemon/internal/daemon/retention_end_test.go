package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// At the end of an issue's retention its suspended and failed claims retire and its workspace on
// the daemon's own host goes, in the row's one run (record.RetentionEnd, decision 17 of
// dispatch://INFRA-4730): the linger expiry the row replaces retired every claim of the issue it
// could stop, a failed one among them, and the workspace's removal waits for each claim of the
// issue to retire, so a failed claim left standing would keep the row waiting for good.
//
// Negative control, run when this was written: with the executor retiring suspended claims alone,
// the failed claim stays failed and the row waits on it.
func TestARetentionEndRetiresTheIssuesFailedClaimsWithItsSuspendedOnes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rig := newOneStartRig(t)
	retained := time.Now().Add(-time.Minute)
	issue := readIssue(t, rig.pool, rig.issue.Key)
	issue.Status, issue.DispatchStatus, issue.RetainedUntil = "backlog", "backlog", &retained
	putOutboxIssue(t, rig.pool, record.NewStore(), *issue)
	states := map[claim.Role]supervise.ClaimState{claim.RoleImplementer: supervise.StateSuspended, claim.RolePlanner: supervise.StateFailed}
	machines := map[claim.Role]*supervise.Machine{}
	for role, state := range states {
		token, err := claim.NewToken("legion", issue.Key, role)
		if err != nil {
			t.Fatalf("the %s claim token: %v", role, err)
		}
		locator, err := rig.rt.Spawn(ctx, runtime.SpawnSpec{Claim: token, Role: role})
		if err != nil {
			t.Fatalf("mint the %s claim's locator: %v", role, err)
		}
		machine, _, err := rig.sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
			Role: role, State: state, Generation: issue.Generation, Locator: &locator}, "")
		if err != nil {
			t.Fatalf("create the %s claim: %v", role, err)
		}
		machine.Wait()
		machines[role] = machine
	}
	var removed []string
	rig.runner.now = time.Now
	rig.runner.remove = func(_ context.Context, working workspace.Workspace, _ sync.Locker) error {
		removed = append(removed, working.Dir)
		return nil
	}
	enqueueOutbox(t, rig.pool, record.NewStore(), mustOutboxRow(t, issue.Key, record.RetentionEnd{}, time.Now()))

	if err := rig.runner.execute(ctx, claimDueOutboxRowOf(t, rig.pool, record.OutboxKindRetentionEnd)); err != nil {
		t.Fatalf("run the retention end: %v", err)
	}
	for role, machine := range machines {
		machine.Wait()
		if got := machine.Claim().State; got != supervise.StateRetired {
			t.Fatalf("the %s claim, %s before the retention ended, is %s after it, want retired", role, states[role], got)
		}
	}
	if len(removed) != 1 {
		t.Fatalf("workspaces removed = %v, want the issue's one", removed)
	}
}
