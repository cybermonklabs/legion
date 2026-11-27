package record_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// An admission judgement reads only the tables whose writes record.Store.JudgedWrites counts
// (record's judgedTables). The admission pass uses its last judgement again while that count does
// not change (admit's finalJudgement). So if a judgement read a table that is not on the list, a
// write to that table would leave the old judgement in use, and the pass would publish a stale
// admission block and a stale free-slot answer.
//
// The seeded project makes every read a pass can make return rows:
//   - a live tree with its slot, its lifecycle and its open design gate;
//   - a start the project owes;
//   - an admitted child whose Dispatch row has a claim and waits on an unfinished issue, and that
//     has an open child of its own in Dispatch;
//   - a member whose parent is done;
//   - a member of a tree whose root is done.
//
// The test reads the slots and judges the admission as a pass does (blockers.JudgeAdmission), in a
// new transaction. Then it reads in that same transaction each user table that Postgres counts a
// scan of (pg_stat_xact_user_tables), and fails with the name of each scanned table that is not on
// the list.
func TestAnAdmissionJudgementReadsOnlyTheJudgedTables(t *testing.T) {
	ctx := context.Background()
	st := testpg.CloneStore(t, "legion_record_test")
	records := record.NewStore()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	const (
		root, child, grandchild, blocker = "LEGION-1", "LEGION-2", "LEGION-3", "LEGION-9"
		member, doneParent               = "LEGION-4", "LEGION-5"
		doneRoot, doneRootMember         = "LEGION-10", "LEGION-11"
	)
	issue := func(key, tree, parent string, at phase.Phase, status string) record.Issue {
		seeded := record.Issue{Key: key, Tree: tree, Project: "LEGION", Title: key, Phase: at, Generation: 1, Status: status,
			Rank: "A" + key, HandedOver: key == tree, LastDispatchSeq: 1, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
		if parent != "" {
			seeded.Parent = &parent
		}
		return seeded
	}
	approved := 1
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		for _, seeded := range []record.Issue{
			issue(root, root, "", phase.Implementing, "in_progress"),
			issue(child, root, root, phase.Admitted, "todo"),
			issue(doneParent, root, root, phase.Done, "done"),
			issue(member, root, doneParent, phase.Implementing, "in_progress"),
			issue(doneRoot, doneRoot, "", phase.Done, "done"),
			issue(doneRootMember, doneRoot, doneRoot, phase.Admitted, "todo"),
		} {
			if err := records.PutIssue(ctx, tx, seeded); err != nil {
				return err
			}
		}
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: root, Index: 0, AdmittedAt: now}); err != nil {
			return err
		}
		if err := records.PutGate(ctx, tx, record.DesignGate{Issue: root, Step: record.DesignStepSpec, ArtifactID: "artifact-1",
			LatestVersion: 1, ApprovedVersion: &approved}); err != nil {
			return err
		}
		for _, mirrored := range []record.DispatchReading{
			{Key: child, Project: "LEGION", Seq: 2, Status: record.Carry("todo"), Parent: record.Carry(root),
				Blockers: record.Carry([]string{blocker}), ClaimedBy: "user:alice", ClaimSeq: 2},
			{Key: grandchild, Project: "LEGION", Seq: 3, Status: record.Carry("todo"), Parent: record.Carry(child),
				Blockers: record.Carry[[]string](nil)},
			{Key: blocker, Project: "LEGION", Seq: 4, Status: record.Carry("in_progress"), Parent: record.Carry(""),
				Blockers: record.Carry[[]string](nil)},
		} {
			if err := records.PutDispatchIssue(ctx, tx, mirrored); err != nil {
				return err
			}
		}
		owed, err := record.NewOutboxRow(member, record.SuperviseRequest{Op: "start", Tree: root, Role: claim.RoleImplementer,
			Generation: 1, Phase: phase.Implementing, Task: "Implement the change.", NeedsSlot: true}, now)
		if err != nil {
			return err
		}
		return records.Enqueue(ctx, tx, owed)
	}); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	if _, err := st.OpenTreeLifecycle(ctx, "legion", root, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the tree of %s: %v", root, err)
	}

	tx, err := st.Pool().BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	slots, err := records.Slots(ctx, tx)
	if err != nil {
		t.Fatalf("read the slots: %v", err)
	}
	judged, err := blockers.JudgeAdmission(ctx, tx, records, "LEGION", []record.DesignStep{record.DesignStepSpec}, slots, 3, now)
	if err != nil {
		t.Fatalf("judge the admission: %v", err)
	}
	// Each conditional read of the judgement found the rows it reads, so each of them ran.
	facts := judged.Facts
	var unread []string
	for read, found := range map[string]bool{
		"the tree's lifecycle":            len(facts.Lifecycles) > 0,
		"the open gate":                   facts.Gates[root].Open(),
		"the owed start":                  len(facts.OwedRows) > 0,
		"the done parent":                 len(facts.Parents) > 0,
		"the done root":                   len(facts.Roots) > 0,
		"the child's open Dispatch child": len(facts.ChildWaits) > 0,
		"the child's Dispatch row":        len(judged.Rows[child].Blockers) > 0,
	} {
		if !found {
			unread = append(unread, read)
		}
	}
	if len(unread) > 0 {
		slices.Sort(unread)
		t.Fatalf("the judgement did not read %s: the seed no longer makes every read of a judgement", strings.Join(unread, ", "))
	}

	rows, err := tx.Query(ctx, `select relname from pg_stat_xact_user_tables
		where seq_scan + coalesce(idx_scan, 0) > 0 order by relname`)
	if err != nil {
		t.Fatalf("read the scans of the transaction: %v", err)
	}
	scanned, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the scans of the transaction: %v", err)
	}
	if !slices.Contains(scanned, "issues") {
		t.Fatalf("the transaction counts scans of %v and none of issues: Postgres counted none of the judgement's reads", scanned)
	}
	var outside []string
	for _, table := range scanned {
		if !slices.Contains(record.JudgedTables, table) {
			outside = append(outside, table)
		}
	}
	if len(outside) > 0 {
		t.Fatalf("an admission judgement reads %v, which judgedTables does not hold: a write to one of them leaves the pass's last judgement in use (admit's finalJudgement); it read %v",
			outside, scanned)
	}
	t.Logf("an admission judgement reads %v", scanned)
}
