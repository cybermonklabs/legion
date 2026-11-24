package admit

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A fact the daemon builds from its own record (workflow.RecordedFact) says what the daemon
// intends, never what Dispatch said, so it writes nothing into the copy of Dispatch's issues. Each
// subtest runs the setting of a test whose decision builds such a fact over the handler list the
// daemon wires (openWorkflow: the mirror, the engine, admission), with the issue's row of the copy
// written by one reading at 10 and its record at LastDispatchSeq 20, with a status and a parent the
// copy does not hold: a write of the fact would pass every value's sequence and show. After the
// fact, the row holds every value, every sequence and its dispatch_seq as that reading wrote them.
func TestADaemonBuiltFactLeavesTheCopy(t *testing.T) {
	// Admission's promotion of a root re-enters the child stranded in its tree with the child's
	// own recorded values (reenterStrandedChildren, Engine.ReenterChild): the setting of
	// TestPromotingAWaitingRootReentersAChildStrandedWhileItsLabelWasOff, whose events before the
	// promoting one run on the engine and admission alone, as that test sends them, so the
	// promoting fact is the only one the mirror meets.
	t.Run("admission's start of a stranded child", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
		seedSlotted(t, pool, "LEGION-ACTIVE", "A")
		const root, child = "LEGION-WAIT", "LEGION-CHILD"
		parent := root
		putIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject, Title: root, Tree: root, Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "B", HandedOver: true, LastDispatchSeq: 1})
		putIssue(t, pool, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject, Title: "child", Tree: root, Parent: &parent, Phase: phase.Done, Generation: 1, Status: "done", Rank: "C", LastDispatchSeq: 1})
		apply(t, pool, admission, "root-unlabeled", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo", Title: root, Rank: "B"}, engine)
		apply(t, pool, admission, "child-reopened-unlabeled", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo", Title: "child", Parent: root, Rank: "C"}, engine)
		apply(t, pool, admission, "root-relabeled", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "todo", Title: root, Rank: "B", HandedOver: handed}, engine)
		want := putCopyRowAt10(t, pool, child)
		stranded := issue(t, pool, child)
		stranded.LastDispatchSeq = 20
		putIssue(t, pool, stranded)

		if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "active-done", intake.DispatchIssue{Key: "LEGION-ACTIVE", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "done", Title: "LEGION-ACTIVE", Rank: "A"},
			blockers.NewMirror(record.NewStore(), testProject), engine, admission); err != nil {
			t.Fatalf("ApplyFact active-done: %v", err)
		}
		assertSlots(t, pool, []record.Slot{{Issue: root, Index: 0, AdmittedAt: fixedNow}})
		if got := issue(t, pool, child); got.Tree != root || got.Phase != phase.Admitted || got.Generation != 2 ||
			got.LastDispatchSeq != 20 || got.Status != "todo" || got.Parent == nil || *got.Parent != root {
			t.Fatalf("child after its root is promoted = %#v, want it re-entered from its record: phase admitted, generation 2, todo under %s at 20", got, root)
		}
		if got := copiedRow(t, pool, child); !reflect.DeepEqual(got, want) {
			t.Fatalf("the copy's row of %s after the re-entry = %+v, want %+v as the reading at 10 wrote it", child, got, want)
		}
	})

	// A queued root's adoption into the live tree its parent finished in moves every member of
	// its own tree, and enters each todo member with that member's recorded values (Engine.Adopt,
	// moveIntoTree): the setting of TestAQueuedRootUnderAFinishedChildOfALiveTreeJoinsTheTree, with
	// one todo member M of the queued root's own tree, on a controller tick applied as a fact. The
	// row checked is M's: no release read carries M, while the queued root's own row a release's
	// read can write first.
	t.Run("Adopt's entry of a queued root's todo members", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		const tree, finished, child, member = "LEGION-168", "LEGION-169", "LEGION-214", "LEGION-215"
		seedOpenTree(t, pool, tree)
		putIssue(t, pool, record.Issue{Key: finished, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject, Title: finished,
			Tree: tree, Parent: record.ParentOf(tree), Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done", Rank: "A", LastDispatchSeq: 3})
		putIssue(t, pool, queuedRoot(child, finished, "B", 1))
		putIssue(t, pool, record.Issue{Key: member, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject, Title: member,
			Tree: child, Parent: record.ParentOf(child), Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "C", LastDispatchSeq: 20})
		want := putCopyRowAt10(t, pool, member)

		if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "tick", intake.ControllerTick{},
			blockers.NewMirror(record.NewStore(), testProject), admission.engine, admission); err != nil {
			t.Fatalf("ApplyFact tick: %v", err)
		}
		if got := issue(t, pool, child); got.Tree != tree || got.Generation != 2 {
			t.Fatalf("%s after the tick = %#v, want it adopted into the tree of %s at generation 2", child, got, tree)
		}
		if got := issue(t, pool, member); got.Tree != tree || got.Phase != phase.Admitted || got.Generation != 2 ||
			got.LastDispatchSeq != 20 || got.Status != "todo" || got.Parent == nil || *got.Parent != child {
			t.Fatalf("%s after the tick = %#v, want it entered into the tree of %s from its record: admitted, generation 2, todo under %s at 20", member, got, tree, child)
		}
		if got := copiedRow(t, pool, member); !reflect.DeepEqual(got, want) {
			t.Fatalf("the copy's row of %s after the adoption = %+v, want %+v as the reading at 10 wrote it", member, got, want)
		}
	})
}

// copyRow is one row of the copy of Dispatch's issues as these tests read it by SQL: each of its
// five values with the sequence of the reading that wrote it (a parent of none read as ""), and
// the row's own sequence, the highest of any reading the row took.
type copyRow struct {
	Status      string
	StatusSeq   int64
	Parent      string
	ParentSeq   int64
	Blockers    []string
	BlockersSeq int64
	Labels      []string
	LabelsSeq   int64
	Rank        string
	RankSeq     int64
	Seq         int64
}

// putCopyRowAt10 writes key's row of the copy as one reading at 10 of all five values, none of
// which a record of these tests holds, and answers the row that reading writes.
func putCopyRowAt10(t *testing.T, pool *pgxpool.Pool, key string) copyRow {
	t.Helper()
	row := copyRow{Status: "backlog", StatusSeq: 10, Parent: "LEGION-300", ParentSeq: 10, Blockers: []string{"LEGION-301"}, BlockersSeq: 10,
		Labels: []string{"design"}, LabelsSeq: 10, Rank: "a0", RankSeq: 10, Seq: 10}
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutDispatchIssue(context.Background(), tx, record.DispatchReading{Key: key, Project: testProject, Seq: 10,
			Status: record.Carry(row.Status), Parent: record.Carry(row.Parent), Blockers: record.Carry(row.Blockers),
			Labels: record.Carry(row.Labels), Rank: record.Carry(row.Rank)}); err != nil {
			t.Fatalf("write the copy's row of %s at 10: %v", key, err)
		}
	})
	return row
}

// copiedRow is key's row of the copy, read by SQL.
func copiedRow(t *testing.T, pool *pgxpool.Pool, key string) copyRow {
	t.Helper()
	var row copyRow
	if err := pool.QueryRow(context.Background(), `select status, status_seq, coalesce(parent, ''), parent_seq, blockers, blockers_seq,
		labels, labels_seq, rank, rank_seq, dispatch_seq from dispatch_issues where key = $1`, key).Scan(&row.Status, &row.StatusSeq,
		&row.Parent, &row.ParentSeq, &row.Blockers, &row.BlockersSeq, &row.Labels, &row.LabelsSeq, &row.Rank, &row.RankSeq, &row.Seq); err != nil {
		t.Fatalf("read the copy's row of %s: %v", key, err)
	}
	return row
}
