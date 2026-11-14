package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A new version of an approved spec closes the tree's design gate, and what may merge is what the
// person's review of that version decides: the merger's start waits while the gate is closed and
// runs when it opens again (dispatch://INFRA-714/spec R5). The start is held, not dropped: the row
// stays on its backoff with nobody spawned, and the same row starts the merger once the approval
// lands. The merger took version 3 and what changed up to version 4 is recorded, so the news it is
// owed holds nothing back once the gate opens (mergerWaitsForTheDesignGate).
func TestTheMergersStartWaitsWhileItsTreesDesignGateIsClosed(t *testing.T) {
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

	err := runner.execute(context.Background(), row)
	if !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("the merger's start under a closed gate = %v, want it held (wait.ErrWaiting)", err)
	}
	if spawns := len(runtime.CallsOf("Spawn")); spawns != 0 {
		t.Fatalf("the runtime spawned %d sessions while the gate was closed, want none", spawns)
	}

	// The person approves version 4, which opens the gate.
	approvedNow := 4
	putOutboxGate(t, pool, records, record.DesignGate{Issue: issue.Key, ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: &approvedNow})

	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("the merger's start once the gate opened = %v, want it run", err)
	}
	if spawns := len(runtime.CallsOf("Spawn")); spawns != 1 {
		t.Fatalf("the runtime spawned %d sessions once the gate opened, want the merger's one", spawns)
	}
}

// Under `root-issues` a root that registered its Verification document as criteria holds its
// tree on that document too (record.TreeSteps), so a new version of it holds the merger's start
// as a new spec version does, with the spec itself approved, and the approval of that version
// runs the same row. The merger took the spec at 4 and the checks at 1, and what changed in the
// checks from 1 to 2 is recorded.
func TestTheMergersStartWaitsWhileItsTreesRegisteredVerificationDocumentIsClosed(t *testing.T) {
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

	if err := runner.execute(context.Background(), row); !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("the merger's start under a closed Verification document = %v, want it held (wait.ErrWaiting)", err)
	}
	if spawns := len(runtime.CallsOf("Spawn")); spawns != 0 {
		t.Fatalf("the runtime spawned %d sessions while the Verification document was closed, want none", spawns)
	}

	putOutboxGate(t, pool, records, record.DesignGate{Issue: issue.Key, Step: record.DesignStepCriteria, ArtifactID: "artifact-208-verification",
		LatestVersion: 2, ApprovedVersion: new(2)})
	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("the merger's start once the Verification document is approved = %v, want it run", err)
	}
	if spawns := len(runtime.CallsOf("Spawn")); spawns != 1 {
		t.Fatalf("the runtime spawned %d sessions once the Verification document was approved, want the merger's one", spawns)
	}
}

// putOutboxGate records a tree's design gate.
func putOutboxGate(t *testing.T, pool *store.Pool, records record.Store, gate record.DesignGate) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutGate(context.Background(), tx, gate)
	}); err != nil {
		t.Fatalf("put the design gate of %s: %v", gate.Issue, err)
	}
}

// putOutboxTold records what issue's holder took of its tree's approved design.
func putOutboxTold(t *testing.T, pool *store.Pool, records record.Store, issue string, versions map[record.DesignStep]int) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.RaiseTold(context.Background(), tx, issue, record.AudienceHolder, versions, time.Now().UTC())
	}); err != nil {
		t.Fatalf("record what %s's holder took: %v", issue, err)
	}
}

// putOutboxChanges records what changed in pair's document between its two versions, as Dispatch
// read it.
func putOutboxChanges(t *testing.T, pool *store.Pool, records record.Store, pair record.VersionPair) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutDesignChanges(context.Background(), tx, record.DesignChanges{VersionPair: pair, State: record.DesignChangesRead,
			Sections: []record.SectionChange{{Heading: "Design", Change: "changed"}}, ReadAt: time.Now().UTC()})
	}); err != nil {
		t.Fatalf("record the change set of %+v: %v", pair, err)
	}
}
