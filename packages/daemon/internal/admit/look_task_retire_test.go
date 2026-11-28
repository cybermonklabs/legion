package admit

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// TestAReadmissionQueuesTheRetirementOfItsLooksTask is the root re-admission row of the R4/R5
// retirement inventory (see workflow/look_task_retire_test.go's
// TestEveryEndOfALooksTaskQueuesItsRetirement): re-admitting a lingering root that holds an open
// production check queues one supervise `retire_look` row (Reported false) in the re-admission
// transaction, naming the check's role, current look and opening time.
func TestAReadmissionQueuesTheRetirementOfItsLooksTask(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{
		Project:              testProject,
		Repositories:         testRepositories,
		Steps:                config.DesignGateRootIssues.Steps(),
		Linger:               time.Hour,
		ProductionCheckRole:  claim.RoleTester,
		ProductionCheckBound: 48 * time.Hour,
		Clock:                func() time.Time { return fixedNow },
	}, nil)

	opened := fixedNow.Add(-time.Hour).UTC().Truncate(time.Microsecond)
	until := fixedNow.Add(time.Hour)
	root := record.Issue{
		Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: "root", Tree: "LEGION-208", Phase: phase.Done, Generation: 1,
		Status: "done", Rank: "A", LingerUntil: &until, HandedOver: handed, LastDispatchSeq: 1,
	}
	putIssue(t, pool, root)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutProductionCheck(context.Background(), tx, record.ProductionCheck{
			Issue: root.Key, Project: testProject, Tree: root.Tree, Repo: "acme/widgets",
			Number: 42, MergeSHA: "merge-42", Role: claim.RoleTester,
			OpenedAt: opened, BoundAt: opened.Add(48 * time.Hour),
			Looks: 1, LookOpen: true, LookWakeAt: &opened,
			LastWakeKind: "merge", LastWakeDetail: "merge-42", LastWakeAt: &opened,
			LookID: "LOOKIDROOTREADMIT000000001", LookIDLook: 1, LookIDOpenedAt: opened,
			OpenedGeneration: 1,
		}); err != nil {
			t.Fatalf("seed production check: %v", err)
		}
	})

	apply(t, pool, admission, "root-readmit", intake.DispatchIssue{
		Key: root.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "todo", Title: root.Title, Rank: root.Rank, HandedOver: handed,
	}, engine)

	readmitted := issue(t, pool, root.Key)
	if readmitted.Generation != 2 || readmitted.Phase != phase.Admitted {
		t.Fatalf("root after re-admission = gen %d phase %s, want gen 2 phase admitted", readmitted.Generation, readmitted.Phase)
	}

	var got []record.SuperviseRequest
	for _, req := range superviseRequests(t, pool, root.Key) {
		if req.Op == "retire_look" {
			got = append(got, req)
		}
	}
	if len(got) != 1 {
		t.Fatalf("root re-admission queued %d retire_look row(s) (%+v), want 1 (Reported false)", len(got), got)
	}
	req := got[0]
	if req.Role != claim.RoleTester || req.CheckLook != 1 || !req.CheckOpenedAt.Equal(opened) || req.Reported {
		t.Fatalf("root re-admission retire_look = %+v, want role=%s checkLook=1 checkOpenedAt=%s reported=false",
			req, claim.RoleTester, opened.UTC().Format(time.RFC3339Nano))
	}
}
