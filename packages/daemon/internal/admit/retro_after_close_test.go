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
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A person's todo on a closed root while its tree lingers re-admits the root at its next
// generation (readmit), and the retro Legion runs after the root's close, still open, ends there
// (P10, the readmit half of s7): its step row ends stopped with no rating, as a child's todo ends
// it (the workflow's TestATodoOnAClosedIssueEndsItsOpenRetro), and the run's record ends stopped
// by the todo. The root's run started at its first phase, which the admission's own start of it
// (workflow.Engine.StartFirstPhase) began, and its close owed that run its retro. The re-admitted
// root stands at admitted at generation 2 with its linger cleared, as every re-admission leaves it.
func TestAReadmissionEndsAnOpenRetroAfterAClose(t *testing.T) {
	pool := migratedPool(t)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateOff.Steps(),
		ReviewRoundCap: 3, Linger: time.Hour, ProductionCheckRole: claim.RoleTester, Clock: func() time.Time { return fixedNow }}, nil)
	admission := New(record.NewStore(), engine, 3, testProject, slog.New(slog.NewTextHandler(io.Discard, nil)))
	admission.now = func() time.Time { return fixedNow }
	const root = "LEGION-1"
	seedSlotted(t, pool, root, "A")

	// The admission's promotion starts a root's first phase in a fact of its own, inside the fact's
	// transaction and settle (Admission.OpenFact and SettleFact are the engine's).
	stored := issue(t, pool, root)
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		ctx = admission.OpenFact(ctx)
		if err := engine.StartFirstPhase(ctx, tx, stored, nil); err != nil {
			return err
		}
		return admission.SettleFact(ctx, tx)
	}); err != nil {
		t.Fatalf("start the first phase of %s: %v", root, err)
	}
	if got := issue(t, pool, root); got.Phase != phase.Planning {
		t.Fatalf("%s after its first phase started stands in %s, want planning", root, got.Phase)
	}
	apply(t, pool, admission, "root-closed", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.closed", Status: "done",
		Title: root, Rank: "A", HandedOver: true}, engine)
	if got := issue(t, pool, root); got.Phase != phase.Done || !got.Lingers() {
		t.Fatalf("%s after a person's done = %s, lingering %t; want it done with its tree lingering", root, got.Phase, got.Lingers())
	}

	before := len(stepWrites(t, pool, root))
	apply(t, pool, admission, "root-reopened", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "todo",
		Title: root, Rank: "A", HandedOver: true}, engine)
	if got := issue(t, pool, root); got.Generation != 2 || got.Phase != phase.Admitted || got.Lingers() {
		t.Fatalf("%s after a person's todo = generation %d in %s, lingering %t; want it re-admitted at generation 2, at admitted", root, got.Generation, got.Phase, got.Lingers())
	}

	var ends, rated []record.StepRecord
	for _, write := range stepWrites(t, pool, root)[before:] {
		switch {
		case write.PlanRating != nil:
			rated = append(rated, write)
		case write.Kind == "retro" && write.EndedAt != nil:
			ends = append(ends, write)
		}
	}
	if len(ends) != 1 || ends[0].Result == nil || *ends[0].Result != string(workflow.ResultStopped) || ends[0].Generation != 1 || len(rated) != 0 {
		t.Fatalf("the retro's step row at the re-admission: ends %+v, ratings %+v; want one end of run 1, stopped, and no rating", ends, rated)
	}
	var ended bool
	var outcome, endedBy string
	if err := pool.QueryRow(context.Background(), `select ended_at is not null, outcome, ended_by from run_retros where issue = $1 and generation = 1`,
		root).Scan(&ended, &outcome, &endedBy); err != nil {
		t.Fatalf("read the run_retros row of %s run 1: %v", root, err)
	}
	if !ended || outcome != "stopped" || endedBy != "todo" {
		t.Fatalf("the run_retros row of %s run 1 = ended %t, outcome %q, by %q; want it ended stopped by the todo", root, ended, outcome, endedBy)
	}
}
