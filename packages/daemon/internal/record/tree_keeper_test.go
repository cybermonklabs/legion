package record

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// keptRow is one tree_lifecycles row as the keeper's test reads it.
type keptRow struct {
	epoch     int64
	authority string
	started   bool
	confirmed bool
}

// The shadow keeper keeps, for the release before (R22 of dispatch://INFRA-3320), one open
// tree_lifecycles row with a bound epoch for each value issues.tree stores while an issue that
// stores it is in Legion's work, and reserves none. A value with no row gets epoch one under the
// workflow, and one whose cleanup confirmed its next epoch; a row already open stays as it is,
// and so do the two it cannot open, a cleanup the release before reserved and a lifetime the
// operator holds, which it answers with their reasons. A tree no issue in work stores — every
// issue storing it at phase done, or out of work on the backlog — gets no row, and nor does a
// tree only another project's issue stores. Its second run changes nothing. The keeper's runner
// logs each tree it leaves once, and again only after the tree was kept in between.
func TestTheKeeperOpensARowForEachStoredTreeOfAnIssueInWorkAndReservesNone(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	seeds := []struct {
		key, tree, project, status string
		at                         phase.Phase
	}{
		{"LEGION-700", "LEGION-700", "LEGION", "in_progress", phase.Implementing}, // a root in work, no row
		{"LEGION-701", "LEGION-700", "LEGION", "todo", phase.Admitted},            // a member storing the same tree
		{"LEGION-710", "LEGION-710", "LEGION", "done", phase.Done},                // a tree only a closed issue stores
		{"LEGION-720", "LEGION-720", "LEGION", "backlog", phase.Implementing},     // a tree only an issue out of work stores
		{"LEGION-730", "LEGION-730", "LEGION", "testing", phase.Testing},          // its tree's cleanup confirmed
		{"LEGION-740", "LEGION-740", "LEGION", "needs_review", phase.Reviewing},   // its tree's cleanup reserved
		{"LEGION-750", "LEGION-750", "LEGION", "retro", phase.Retro},              // the operator holds its tree
		{"LEGION-760", "LEGION-760", "LEGION", "in_progress", phase.Implementing}, // its tree's row is open already
		{"OTHER-7", "OTHER-7", "OTHER", "in_progress", phase.Implementing},        // another project's
	}
	inTx(t, st, func(tx pgx.Tx) {
		for _, seed := range seeds {
			issue := issueFixture(seed.key)
			issue.Tree, issue.Project, issue.Status, issue.Phase = seed.tree, seed.project, seed.status, seed.at
			must(t, records.PutIssue(ctx, tx, issue))
		}
		for _, row := range []struct {
			tree, authority    string
			epoch              int64
			started, confirmed bool
		}{
			{"LEGION-730", "workflow", 2, true, true},
			{"LEGION-740", "workflow", 1, true, false},
			{"LEGION-750", "operator", 1, false, false},
			{"LEGION-760", "workflow", 3, false, false},
		} {
			if _, err := tx.Exec(ctx, `insert into tree_lifecycles (project, tree, epoch, authority, cleanup_started, cleanup_confirmed_at)
				values ('legion', $1, $2, $3, $4, case when $5 then now() end)`, row.tree, row.epoch, row.authority, row.started, row.confirmed); err != nil {
				t.Fatalf("seed the lifecycle of %s: %v", row.tree, err)
			}
		}
	})
	rows := func() map[string]keptRow {
		t.Helper()
		read := map[string]keptRow{}
		inTx(t, st, func(tx pgx.Tx) {
			result, err := tx.Query(ctx, `select project || '/' || tree, epoch, authority, cleanup_started, cleanup_confirmed_at is not null from tree_lifecycles`)
			must(t, err)
			defer result.Close()
			for result.Next() {
				var key string
				var row keptRow
				must(t, result.Scan(&key, &row.epoch, &row.authority, &row.started, &row.confirmed))
				read[key] = row
			}
			must(t, result.Err())
		})
		return read
	}
	want := map[string]keptRow{
		"legion/LEGION-700": {epoch: 1, authority: "workflow"},
		"legion/LEGION-730": {epoch: 3, authority: "workflow"},
		"legion/LEGION-740": {epoch: 1, authority: "workflow", started: true},
		"legion/LEGION-750": {epoch: 1, authority: "operator"},
		"legion/LEGION-760": {epoch: 3, authority: "workflow"},
	}
	wantUnkept := []UnkeptTree{{Tree: "LEGION-740", Reason: UnkeptCleanupReserved}, {Tree: "LEGION-750", Reason: UnkeptOperatorHold}}

	var log testlog.Buffer
	keeper := NewTreeKeeper(slog.New(slog.NewTextHandler(&log, nil)))
	for run := 1; run <= 2; run++ {
		var unkept []UnkeptTree
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			unkept, err = records.KeepTreeLifecyclesOpen(ctx, tx, "LEGION")
			must(t, err)
		})
		if !reflect.DeepEqual(unkept, wantUnkept) {
			t.Fatalf("run %d of the keeper left %#v, want %#v: the reserved cleanup and the operator's hold, each with its reason", run, unkept, wantUnkept)
		}
		if got := rows(); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d of the keeper left the tree lifecycles %v, want %v: an open row for each tree an issue in work stores, the two it cannot open as they stood, and none reserved", run, got, want)
		}
		inTx(t, st, func(tx pgx.Tx) { must(t, keeper.Keep(ctx, tx, records, "LEGION")) })
	}
	for _, tree := range []string{"LEGION-740", "LEGION-750"} {
		if count := strings.Count(log.String(), "tree="+tree); count != 1 {
			t.Fatalf("two passes logged %s %d times, want once:\n%s", tree, count, log.String())
		}
	}

	// The operator's hold ends with a confirmed cleanup: the next pass keeps the tree open, and a
	// hold the operator takes of it again afterwards is logged anew.
	inTx(t, st, func(tx pgx.Tx) {
		_, err := tx.Exec(ctx, `update tree_lifecycles set cleanup_started = true, cleanup_confirmed_at = now() where tree = 'LEGION-750'`)
		must(t, err)
		must(t, keeper.Keep(ctx, tx, records, "LEGION"))
		_, err = tx.Exec(ctx, `update tree_lifecycles set authority = 'operator' where tree = 'LEGION-750'`)
		must(t, err)
		must(t, keeper.Keep(ctx, tx, records, "LEGION"))
	})
	if count := strings.Count(log.String(), "tree=LEGION-750"); count != 2 {
		t.Fatalf("a hold logged, kept and taken again was logged %d times, want twice:\n%s", count, log.String())
	}
}
