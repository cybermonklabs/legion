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

// A new version of an approved spec closes the tree's design gate, and no value of the hold holds
// the merge (decision 28 of dispatch://INFRA-4528's plan): the merger's start runs while the gate
// is closed, as when no version waits. The merger took version 3 and what changed up to version 4
// is recorded, so the news it is owed holds nothing back (mergerWaitsForTheDesignGate, which reads
// no gate).
func TestTheMergersStartGoesOnWhileItsTreesDesignGateIsClosed(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "a reviewed root", Phase: phase.Merging, Generation: 1, Status: "retro"}
	putOutboxIssue(t, pool, records, issue)
	// The gate as a new version leaves it: version 4 with the person's approval still at 3.
	approved := 3
	putOutboxGate(t, pool, records, record.DesignGate{Issue: issue.Key, ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: &approved})
	putOutboxTold(t, pool, records, issue.Key, map[record.DesignStep]int{record.DesignStepSpec: 3})
	putOutboxChanges(t, pool, records, record.VersionPair{ArtifactID: "artifact-208", From: 3, To: 4})
	sup, runtime := newOutboxSupervisor(t, "legion", t.TempDir())
	runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup,
		trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		// The deployment gates the spec step alone, the list `gates.design: root-issues` names
		// (config.DesignGate.Steps, INFRA-1272): a runner told no step gates nothing, so the
		// start it holds here is held by the step this gate's row belongs to.
		gatedSteps: config.DesignGateRootIssues.Steps(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleMerger,
		Generation: issue.Generation, Phase: phase.Merging, Task: "Merge it."}, time.Now())

	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("the merger's start under a closed gate = %v, want it run: no value of the hold holds the merge", err)
	}
	if spawns := len(runtime.CallsOf("Spawn")); spawns != 1 {
		t.Fatalf("the runtime spawned %d sessions while the gate was closed, want the merger's one", spawns)
	}
}

// A step an ancestor's approval can open (dispatch://INFRA-3320, R1 and R2) holds no merge either:
// while the step stands closed at the root's new version, which no person approved, the merger's
// start runs, as when no version waits (decision 28). The merger took version 3 and what changed
// up to version 4 is recorded, so the news it is owed holds nothing back
// (mergerWaitsForTheDesignGate).
func TestTheMergersStartGoesOnWhileItsStepStandsClosedBeforeABasis(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "a root that is a view", Phase: phase.Merging, Generation: 1, Status: "retro"}
	putOutboxIssue(t, pool, records, issue)
	// Version 4 with the person's approval still at 3: closed.
	personal := 3
	closed := record.DesignGate{Issue: issue.Key, Step: record.DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: &personal,
		ApprovedMark: record.EventMark{Seq: 7, At: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}}
	putOutboxGate(t, pool, records, closed)
	putOutboxTold(t, pool, records, issue.Key, map[record.DesignStep]int{record.DesignStepSpec: 3})
	putOutboxChanges(t, pool, records, record.VersionPair{ArtifactID: "artifact-208", From: 3, To: 4})
	sup, runtime := newOutboxSupervisor(t, "legion", t.TempDir())
	runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup,
		trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		gatedSteps: config.DesignGateRootIssues.Steps(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleMerger,
		Generation: issue.Generation, Phase: phase.Merging, Task: "Merge it."}, time.Now())

	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("the merger's start under the closed step = %v, want it run: no value of the hold holds the merge", err)
	}
	if spawns := len(runtime.CallsOf("Spawn")); spawns != 1 {
		t.Fatalf("the runtime spawned %d sessions while the step was closed, want the merger's one", spawns)
	}
}

// Under `root-issues` a root that registered its Verification document as criteria holds its
// tree on that document too (record.TreeSteps), and a new version of it holds no merge, as a new
// spec version holds none (decision 28): the merger's start runs. The merger took the spec at 4
// and the checks at 1, and what changed in the checks from 1 to 2 is recorded.
func TestTheMergersStartGoesOnWhileItsTreesRegisteredVerificationDocumentIsClosed(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "a reviewed root", Phase: phase.Merging, Generation: 1, Status: "retro"}
	putOutboxIssue(t, pool, records, issue)
	putOutboxGate(t, pool, records, record.DesignGate{Issue: issue.Key, Step: record.DesignStepSpec, ArtifactID: "artifact-208",
		LatestVersion: 4, ApprovedVersion: new(4)})
	// The Verification document as a new version leaves it: version 2 with the approval at 1.
	putOutboxGate(t, pool, records, record.DesignGate{Issue: issue.Key, Step: record.DesignStepCriteria, ArtifactID: "artifact-208-verification",
		LatestVersion: 2, ApprovedVersion: new(1)})
	putOutboxTold(t, pool, records, issue.Key, map[record.DesignStep]int{record.DesignStepSpec: 4, record.DesignStepCriteria: 1})
	putOutboxChanges(t, pool, records, record.VersionPair{ArtifactID: "artifact-208-verification", From: 1, To: 2})
	sup, runtime := newOutboxSupervisor(t, "legion", t.TempDir())
	runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup,
		trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		gatedSteps: config.DesignGateRootIssues.Steps(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleMerger,
		Generation: issue.Generation, Phase: phase.Merging, Task: "Merge it."}, time.Now())

	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("the merger's start under a closed Verification document = %v, want it run: no value of the hold holds the merge", err)
	}
	if spawns := len(runtime.CallsOf("Spawn")); spawns != 1 {
		t.Fatalf("the runtime spawned %d sessions while the Verification document was closed, want the merger's one", spawns)
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

// putOutboxChanges records what changed in pair's document between its two versions, as Dispatch
// read it.
func putOutboxChanges(t *testing.T, pool *store.Pool, records record.Store, pair record.VersionPair) {
	t.Helper()
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.PutDesignChanges(context.Background(), tx, record.DesignChanges{VersionPair: pair, State: record.DesignChangesRead,
			Sections: []record.SectionChange{{Heading: "Design", Change: "changed"}}, ReadAt: time.Now().UTC()})
	}); err != nil {
		t.Fatalf("record the change set of %+v: %v", pair, err)
	}
}
