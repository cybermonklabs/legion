package daemon

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A launch's LEGION_TREE, the tree its spawn spec names (runtime.SpawnSpec.Tree), is the issue's
// gate root on the Dispatch chain at the launch (decision 13 of dispatch://INFRA-4730, R22 of
// dispatch://INFRA-3320), never the tree the issue's record stores: a member of LEGION-208 whose
// record still names another tree launches its implementer under LEGION-208. The claim keeps both:
// its tree is the record's, for the release before, and its launch tree the gate root, which the
// registration answers. The Sandbox half, the label the pod is made with, is runtime/sandbox's
// test of this name and the `member` golden.
func TestANewLaunchSetsLegionTreeToTheGateRoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	const root, member, stored = "LEGION-208", "LEGION-209", "LEGION-100"
	putOutboxIssue(t, pool, records, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: root, Title: "Root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", HoldsGate: true})
	issue := record.Issue{Key: member, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: stored, Parent: &[]string{root}[0], Title: "Member", Phase: phase.Planning, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		for key, parent := range map[string]string{root: "", member: root} {
			if err := records.PutDispatchIssue(ctx, tx, record.DispatchReading{Key: key, Project: "LEGION", Seq: 1,
				Status: record.Carry("in_progress"), Parent: record.Carry(parent)}); err != nil {
				return err
			}
		}
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: member, Role: claim.RolePlanner, Claim: "plan-claim"})
	}); err != nil {
		t.Fatalf("seed the chain and the planner's phases row: %v", err)
	}
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	applyFact(t, pool, "api", "plan-complete", intake.HandoffComplete{Generation: 1, Issue: member,
		Role: claim.RolePlanner, Claim: "plan-claim", HandoffRecord: "plan", HandoffVersion: 1}, engine)

	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + member}, nil
		},
	}
	start := theOneStartRow(t, pool, member)
	if err := runner.execute(ctx, start.row); err != nil {
		t.Fatalf("run the implementer's start row: %v", err)
	}
	token, err := claim.NewToken("legion", member, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	machine, found := sup.Machine(token)
	if !found {
		t.Fatal("the start row created no claim for the member's implementer")
	}
	machine.Wait()
	spawns := rt.CallsOf("Spawn")
	if len(spawns) != 1 {
		t.Fatalf("the start spawned %d processes, want one", len(spawns))
	}
	if got := spawns[0].Spec.Tree; got != root {
		t.Fatalf("the launch's LEGION_TREE = %q, want the gate root %s: the record's stored tree %s is read by no launch", got, root, stored)
	}
	got := machine.Claim()
	if got.Tree != stored || got.LaunchTree != root || got.HoldsGate {
		t.Fatalf("the claim keeps tree %q, launch tree %q, holds gate %t; want the record's %s, the gate root %s, and no gate of its own",
			got.Tree, got.LaunchTree, got.HoldsGate, stored, root)
	}
}
