package admit

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// Admission's unslotted repair is narrow: the running-tree statuses are the only statuses where
// a slotless root has definitely left its queue. An unknown Dispatch status must remain recorded
// for its own handler rather than being silently taken out of Legion's workflow.
func TestAnUnknownStatusDoesNotReleaseAnUnslottedRoot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Clock: func() time.Time { return fixedNow }}, nil)
	stranded := record.Issue{Key: "LEGION-UNKNOWN", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject, Title: "unknown status", Tree: "LEGION-UNKNOWN",
		Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 1}
	putIssue(t, pool, stranded)

	apply(t, pool, admission, "unknown-status", intake.DispatchIssue{Key: stranded.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "blocked", Title: stranded.Title, Rank: stranded.Rank, HandedOver: true}, engine)

	got := issue(t, pool, stranded.Key)
	if got.Phase != phase.Admitted || got.LingerUntil != nil || got.Status != "blocked" {
		t.Fatalf("unknown-status root = %#v, want it left admitted without a linger", got)
	}
	if notices := controllerNotices(t, pool); len(notices) != 0 {
		t.Fatalf("controller notices = %#v, want none for an unknown status", notices)
	}
}

func controllerNotices(t *testing.T, pool *pgxpool.Pool) []effect {
	t.Helper()
	var got []effect
	for _, row := range effects(t, pool) {
		if row.kind == record.OutboxKindControllerNotice {
			got = append(got, row)
		}
	}
	return got
}
