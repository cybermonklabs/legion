package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A new version of an approved document closes the tree's design gate, and no value of the hold
// holds the merge (decision 28 of dispatch://INFRA-4528's plan): the merger's start runs while the
// gate is closed, as when no version waits. In each case the merger took the approved version of
// each gated step, so it is owed no news (mergerWaitsForItsNews). The cases close the gate in
// three ways:
//   - a new version of the spec, with the person's approval still at the version before;
//   - a step an ancestor's approval can open (dispatch://INFRA-3320, R1 and R2), which stands
//     closed at the root's new version, which no person approved;
//   - under `root-issues`, a new version of the Verification document the root registered as
//     criteria, which holds its tree as the spec does (record.TreeSteps), the spec standing
//     approved.
func TestTheMergersStartGoesOnWhileItsTreesDesignGateIsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// title is the root's title, gates its gate rows, and took the versions its merger took.
		title string
		gates []record.DesignGate
		took  map[record.DesignStep]int
		// closed names what closed the gate in the case's failures.
		closed string
	}{
		{
			name:  "a new spec version",
			title: "a reviewed root",
			// The gate as a new version leaves it: version 4 with the person's approval still at 3.
			gates:  []record.DesignGate{{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: new(3)}},
			took:   map[record.DesignStep]int{record.DesignStepSpec: 3},
			closed: "the gate",
		},
		{
			name:  "a step closed before a basis",
			title: "a root that is a view",
			// Version 4 with the person's approval still at 3: closed.
			gates: []record.DesignGate{{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 4,
				ApprovedVersion: new(3), ApprovedMark: record.EventMark{Seq: 7, At: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}}},
			took:   map[record.DesignStep]int{record.DesignStepSpec: 3},
			closed: "the step",
		},
		{
			name:  "a registered Verification document closed",
			title: "a reviewed root",
			// The spec approved at 4, and the Verification document as a new version leaves it:
			// version 2 with the approval at 1.
			gates: []record.DesignGate{
				{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: new(4)},
				{Issue: "LEGION-208", Step: record.DesignStepCriteria, ArtifactID: "artifact-208-verification", LatestVersion: 2, ApprovedVersion: new(1)},
			},
			took:   map[record.DesignStep]int{record.DesignStepSpec: 4, record.DesignStepCriteria: 1},
			closed: "the Verification document",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: "LEGION-208", Title: tc.title, Phase: phase.Merging, Generation: 1, Status: "retro"}
			putOutboxIssue(t, pool, records, issue)
			for _, gate := range tc.gates {
				putOutboxGate(t, pool, records, gate)
			}
			putOutboxTold(t, pool, records, issue.Key, tc.took)
			sup, runtime := newOutboxSupervisor(t, "legion", t.TempDir())
			runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup,
				trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
				// The deployment gates the spec step alone, the list `gates.design: root-issues`
				// names (config.DesignGate.Steps, INFRA-1272): a runner told no step gates nothing,
				// so the start it holds here is held by the step the gate's rows belong to.
				gatedSteps: config.DesignGateRootIssues.Steps(),
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
				},
			}
			row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleMerger,
				Generation: issue.Generation, Phase: phase.Merging, Task: "Merge it."}, time.Now())

			if err := runner.execute(context.Background(), row); err != nil {
				t.Fatalf("the merger's start while %s is closed = %v, want it run: no value of the hold holds the merge", tc.closed, err)
			}
			if spawns := len(runtime.CallsOf("Spawn")); spawns != 1 {
				t.Fatalf("the runtime spawned %d sessions while %s was closed, want the merger's one", spawns, tc.closed)
			}
		})
	}
}

// putOutboxGate records a tree's design gate.
func putOutboxGate(t *testing.T, pool *store.Pool, records record.Store, gate record.DesignGate) {
	t.Helper()
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.PutGate(context.Background(), tx, gate)
	}); err != nil {
		t.Fatalf("put the design gate of %s: %v", gate.Issue, err)
	}
}

// putOutboxTold records what issue's holder took of its tree's approved design.
func putOutboxTold(t *testing.T, pool *store.Pool, records record.Store, issue string, versions map[record.DesignStep]int) {
	t.Helper()
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.RaiseTold(context.Background(), tx, issue, record.AudienceHolder, raisedTo(versions), time.Now().UTC())
	}); err != nil {
		t.Fatalf("record what %s's holder took: %v", issue, err)
	}
}
