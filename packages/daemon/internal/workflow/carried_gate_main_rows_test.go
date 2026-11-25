package workflow

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// mainGateRow inserts a design_gates row with the columns main's PutGate names (c03d3afe and
// 93541fc1: issue, step, artifact_id, latest_version, approved_version, ..., ever_opened), so
// document_issue stays null, as every row a daemon on main writes on this schema.
func mainGateRow(t *testing.T, pool *pgxpool.Pool, issue string, step record.DesignStep, artifact string, latest int, approved *int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `insert into design_gates (issue, step, artifact_id, latest_version, approved_version, ever_opened)
		values ($1, $2, $3, $4, $5, $6)`, issue, string(step), artifact, latest, approved, approved != nil && *approved == latest); err != nil {
		t.Fatalf("insert main's gate row of %s %s: %v", issue, step, err)
	}
}

// backfill runs the update of migration 20261122040952 as its file writes it, over rows main wrote
// before the upgrade.
func backfill(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	sql, err := os.ReadFile("../store/migrations/20261122040952_design_gate_document_issue.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, update, found := strings.Cut(string(sql), "alter table design_gates add column document_issue text;")
	if !found {
		t.Fatal("the migration's alter statement moved")
	}
	if _, err := pool.Exec(context.Background(), update); err != nil {
		t.Fatalf("the migration's backfill: %v", err)
	}
}

func documentIssues(t *testing.T, pool *pgxpool.Pool, issue string) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select step, coalesce(document_issue, '<null>') from design_gates where issue = $1`, issue)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var step, doc string
		if err := rows.Scan(&step, &doc); err != nil {
			t.Fatal(err)
		}
		got[step] = doc
	}
	return got
}

// seedOldRootAndCarried records, with main's SQL for the gate rows, the old root gatedRoot (done)
// and LEGION-209, a root of its own a hand-over created with the old root's gate at generation 2
// (handed_over_roots gate_carried), holding a copy of each old row on the old root's documents.
// from is LEGION-209's handed_over_from: the old root before its promotion, "" after it (admit's
// promotion clears it when it opens the root's own tree).
func seedOldRootAndCarried(t *testing.T, pool *pgxpool.Pool, at phase.Phase, from string) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: gatedRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: gatedRoot, Project: "LEGION", Title: "the old root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U"})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-209", Project: "LEGION", Title: "the carried root", Parent: new(gatedRoot), Phase: at, Generation: 2,
		Status: "in_progress", Rank: "V", HandedOver: true, HandedOverFrom: from, LastDispatchSeq: 1})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-209", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	seedTreeLifecycle(t, pool, "LEGION-209")
	for _, issue := range []string{gatedRoot, "LEGION-209"} {
		mainGateRow(t, pool, issue, record.DesignStepCriteria, criteriaDoc, 2, new(1))
		mainGateRow(t, pool, issue, record.DesignStepSpec, gateDoc, 4, new(4))
	}
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutHandedOverRoot(t.Context(), tx, record.HandedOverRoot{Issue: "LEGION-209", Generation: 2, GateCarried: true})
	})
}

func readCatchUp(t *testing.T, pool *pgxpool.Pool, key string) texts.CatchUpFacts {
	t.Helper()
	var facts texts.CatchUpFacts
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		issue, err := record.NewStore().Issue(t.Context(), tx, key)
		if err != nil {
			return err
		}
		facts, err = treeCatchUp(t.Context(), tx, record.NewStore(), twoSteps(), *issue)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return facts
}

// A carried root its promotion opened before the upgrade: main wrote its carried rows (no
// document_issue), the promotion cleared its handed_over_from, and the store still records that a
// hand-over created it with its gate at this generation (handed_over_roots), on the old root's
// artifacts, which the old root's own rows hold. The migration's backfill fills none of its rows
// (it reads handed_over_from alone). After the upgrade the root architect's catch-up names, for
// each carried step, the carried root's own document beside the old root's artifact, version and
// approval. INFRA-3686 R8 (the carried-gate and catch-up facts) and R2 ("a dispatch:// link for each
// Dispatch item"): the document a person approves is the old root's.
func TestAPromotedCarriedRootsCatchUpNamesTheOldRootsDocumentAfterTheUpgrade(t *testing.T) {
	pool := migratedPool(t)
	seedOldRootAndCarried(t, pool, phase.Implementing, "")
	backfill(t, pool)
	t.Logf("after the backfill, LEGION-209's rows record document issues %v", documentIssues(t, pool, "LEGION-209"))

	caught := readCatchUp(t, pool, "LEGION-209")
	if !caught.Carried || len(caught.Gates) != 2 {
		t.Fatalf("the catch-up says carried %t with gates %+v; want a carried root's catch-up naming both carried steps", caught.Carried, caught.Gates)
	}
	for _, gate := range caught.Gates {
		if want := stepRef(gatedRoot, record.DesignStep(gate.Step)); gate.Document != want {
			t.Errorf("the promoted carried root's catch-up names the %s step's document %s with artifact %s at version %d (approved %d), but that artifact is the old root's document %s, which the copy is bound to and a person approves",
				gate.Step, gate.Document, gate.Artifact, gate.Version, gate.Approved, want)
		}
	}
}

// A carry a daemon on main made on this schema (a rollback, then a roll forward): the carried
// root is still at admitted, in the line, handed over from the old root, with main's rows (no
// document_issue). Its catch-up names the old root's documents (gateDocumentIssue reads
// handed_over_from); its architect-start task, a gate-standing shape, names the carried root's own
// documents for the same rows (gateSteps reads the row's document_issue alone).
func TestARootMainCarriedNamesTheOldRootsDocumentInItsStartTask(t *testing.T) {
	pool := migratedPool(t)
	seedOldRootAndCarried(t, pool, phase.Admitted, gatedRoot)

	caught := readCatchUp(t, pool, "LEGION-209")
	catchUpDocs := map[string]string{}
	for _, gate := range caught.Gates {
		catchUpDocs[gate.Step] = gate.Document
	}
	var facts texts.ArchitectStartRecord
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		root, err := record.NewStore().Issue(t.Context(), tx, "LEGION-209")
		if err != nil {
			return err
		}
		start, err := ArchitectStartOf(t.Context(), tx, record.NewStore(), twoSteps(), *root)
		if err != nil {
			return err
		}
		var served bool
		facts, served = ArchitectStartFacts(start)
		if !served {
			t.Fatalf("no start task for %+v", start)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("start task shape %s, gates %+v; catch-up documents %v", facts.Shape, facts.Gates, catchUpDocs)
	if len(facts.Gates) != 2 {
		t.Fatalf("the start task names gates %+v, want both carried steps", facts.Gates)
	}
	for _, gate := range facts.Gates {
		want := stepRef(gatedRoot, record.DesignStep(gate.Step))
		if gate.Document != want {
			t.Errorf("the carried root's architect-start task names the %s step's document %s (version %d), where its catch-up names %s: the copy is bound to the old root's document %s",
				gate.Step, gate.Document, gate.Version, catchUpDocs[gate.Step], want)
		}
	}
}
