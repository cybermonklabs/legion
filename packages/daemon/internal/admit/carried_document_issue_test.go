package admit

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A hand-over's carry binds each copy to the issue of the old row's document as the store's rows on
// that document say it (record.Store.DocumentIssues), never to the old root itself where its row is
// a copy: here the old root LEGION-208 was itself carried in from LEGION-207 with its gate, by a
// daemon that recorded no document issue, and registered its own criteria document while it stood
// carried in. So the copy of its spec row names LEGION-207, whose registration the store holds, and
// the copy of its criteria row names none, since the store cannot tell LEGION-208's registration
// from a copy; the texts name that one from Dispatch where they are sent (workflow's
// stepDocuments). The carry read the old row's own issue for both before (DocumentOf), which named
// LEGION-208 for a spec it never held, and a later carry copied that onward (INFRA-3686, test
// handoff v13's failure 1).
func TestACarryOfACarriedRootsGateNamesTheIssueOfEachDocumentTheStoreHolds(t *testing.T) {
	pool := migratedPool(t)
	steps := []record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec}
	admission := newAdmissionWithSteps(t, 1, steps, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: steps, Linger: time.Hour,
		Clock: func() time.Time { return fixedNow }}, nil)
	const first, root, child = "LEGION-207", "LEGION-208", "LEGION-209"
	until := fixedNow.Add(time.Hour)
	parent := root
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		for _, put := range []error{
			records.PutIssue(context.Background(), tx, record.Issue{Key: first, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: first, Project: testProject, Title: first, Phase: phase.Done, Generation: 1, Status: "done"}),
			records.PutIssue(context.Background(), tx, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: root, Project: testProject, Title: root, Phase: phase.Done, Generation: 2, Status: "done", LingerUntil: &until, HandedOver: true}),
			records.PutHandedOverRoot(context.Background(), tx, record.HandedOverRoot{Issue: root, Generation: 2, GateCarried: true}),
			records.PutIssue(context.Background(), tx, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: root, Project: testProject, Title: child, Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "B", LastDispatchSeq: 1}),
			records.PutHandover(context.Background(), tx, record.Handover{Issue: child, Tree: root, Generation: 2}),
		} {
			if put != nil {
				t.Fatalf("seed: %v", put)
			}
		}
		// The rows as a daemon before design_gates.document_issue wrote them: its insert names the
		// columns it knows, so none records a document issue.
		for _, row := range []struct {
			issue    string
			step     record.DesignStep
			artifact string
		}{
			{first, record.DesignStepSpec, "spec-207"},
			{root, record.DesignStepSpec, "spec-207"},
			{root, record.DesignStepCriteria, "criteria-208"},
		} {
			if _, err := tx.Exec(context.Background(), `insert into design_gates (issue, step, artifact_id, latest_version, approved_version, ever_opened)
				values ($1, $2, $3, 2, 2, true)`, row.issue, string(row.step), row.artifact); err != nil {
				t.Fatalf("write the gate row of %s %s: %v", row.issue, row.step, err)
			}
		}
	})

	apply(t, pool, admission, "handover", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo",
		Title: child, Parent: root, Rank: "B", HandedOver: true, DaemonActor: true}, engine)
	if got := issue(t, pool, child); got.Tree != child || got.Generation != 2 {
		t.Fatalf("the handed-over child = %+v, want its own tree at generation 2", got)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		gates, err := record.NewStore().GatesOf(context.Background(), tx, child)
		if err != nil {
			t.Fatal(err)
		}
		for step, want := range map[record.DesignStep]struct{ artifact, issue string }{
			record.DesignStepSpec:     {"spec-207", first},
			record.DesignStepCriteria: {"criteria-208", ""},
		} {
			if got := gates[step]; got.ArtifactID != want.artifact || got.DocumentIssue != want.issue {
				t.Errorf("the carried %s row of %s = %+v, want it on %s recording document issue %q", step, child, got, want.artifact, want.issue)
			}
		}
	})
}
