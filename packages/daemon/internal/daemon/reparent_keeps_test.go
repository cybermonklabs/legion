package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A change of parent moves nothing of the issue's own (R17 of dispatch://INFRA-3320): LEGION-500,
// implementing under LEGION-208 with its spec step open on LEGION-208's approval, a working
// implementer in its pod, a slot and a pull request, moves under LEGION-300, and its run and phase
// entry, its implementer's claim, session and pod, its slot, its gate steps with their basis and
// its pull request are as they were. The daemon's whole path of the move runs: the event through
// the mirror and the engine, and the outbox until nothing is due.
func TestAReparentKeepsTheRunSessionPodSlotGateAndPullRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := &reparentDispatch{}
	client.issue = dispatch.Issue{Key: reparentNewParent, Status: "in_progress"}
	rig := newReparentRig(t, client)
	token, err := claim.NewToken("legion", reparentIssue, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rig.runner.supervisor.Create(ctx, supervise.Claim{
		Token: token, Project: "legion", Tree: reparentOldParent, LaunchTree: reparentOldParent, Issue: reparentIssue,
		Role: claim.RoleImplementer, State: supervise.StateWorking, Generation: 1, Session: "ses-implementer",
		Locator: &runtime.Locator{Runtime: "sandbox", Claim: token, Incarnation: "inc-1", Sandbox: &runtime.SandboxLocator{
			Namespace: "legion", Name: "legion-legion-legion-500", PodUID: "uid-pod-500", Container: "implementer", Generation: 1,
		}},
	}, ""); err != nil {
		t.Fatalf("create the implementer's claim: %v", err)
	}
	opened := 3
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := rig.records.PutSlot(ctx, tx, record.Slot{Issue: reparentIssue, Index: 0, AdmittedAt: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}); err != nil {
			return err
		}
		if err := rig.records.PutGate(ctx, tx, record.DesignGate{Issue: reparentIssue, Step: record.DesignStepSpec, ArtifactID: "artifact-500",
			LatestVersion: 2, ApprovedVersion: &opened, EverOpened: true, Basis: &record.GateBasis{Issue: reparentOldParent, ArtifactID: "artifact-208",
				Version: 3, Approver: "user:alice", Reason: "the spec rests on its parent's", RegisteredAt: time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)}}); err != nil {
			return err
		}
		return rig.records.PutPullRequest(ctx, tx, record.PullRequest{Issue: reparentIssue, Repo: "acme/widgets", Number: 7,
			Branch: "legion/" + reparentIssue, Base: "main", HeadSHA: "head", State: record.PullRequestOpen})
	}); err != nil {
		t.Fatalf("seed the slot, the gate and the pull request: %v", err)
	}

	before := reparentKept(t, rig, token)
	rig.move(reparentNewParent)
	if told := rig.told(); len(told[reparentIssue]) != 1 {
		t.Fatalf("the move was told %v, want it told to the issue's own architect once: the move is the test's premise", told)
	}
	if after := reparentKept(t, rig, token); after != before {
		t.Fatalf("after the change of parent the issue holds\n  %s\nwant what it held before\n  %s", after, before)
	}
}

// reparentKept is what a change of parent must leave of the moved issue, spelled out: its run and
// phase entry and phase, its implementer's claim as the supervisor holds it, its slot, its gates
// and its pull request.
func reparentKept(t *testing.T, rig *reparentRig, token claim.Token) string {
	t.Helper()
	ctx := context.Background()
	machine, found := rig.runner.supervisor.Machine(token)
	if !found {
		t.Fatalf("the supervisor holds no claim %s", token)
	}
	c := machine.Claim()
	kept := fmt.Sprintf("claim %s gen %d session %s pod %s launch tree %s", c.State, c.Generation, c.Session, c.Locator.Sandbox.PodUID, c.LaunchTree)
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := rig.records.Issue(ctx, tx, reparentIssue)
		if err != nil {
			return err
		}
		fence, err := record.ReadFence(ctx, tx, rig.records, reparentIssue, false)
		if err != nil {
			return err
		}
		slots, err := rig.records.Slots(ctx, tx)
		if err != nil {
			return err
		}
		var slot []record.Slot
		for _, held := range slots {
			if held.Issue == reparentIssue {
				slot = append(slot, held)
			}
		}
		gates, err := rig.records.GatesOf(ctx, tx, reparentIssue)
		if err != nil {
			return err
		}
		spec := gates[record.DesignStepSpec]
		pr, err := rig.records.PullRequest(ctx, tx, reparentIssue)
		if err != nil {
			return fmt.Errorf("read the pull request: %w", err)
		}
		if pr == nil {
			return fmt.Errorf("%s holds no pull request", reparentIssue)
		}
		approved := "none"
		if spec.ApprovedVersion != nil {
			approved = fmt.Sprintf("v%d", *spec.ApprovedVersion)
		}
		kept += fmt.Sprintf("; phase %s fence %+v; slot %+v; spec gate v%d approved %s ever opened %t basis %+v; pull request #%d on %s",
			issue.Phase, fence, slot, spec.LatestVersion, approved, spec.EverOpened, spec.Basis, pr.Number, pr.Branch)
		return nil
	}); err != nil {
		t.Fatalf("read what the issue holds: %v", err)
	}
	return kept
}
