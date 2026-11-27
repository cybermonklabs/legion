package docs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// passPlanRuns is how many passes of the reconciliation the test runs over one room on one
// connection: past the five runs of a statement after which Postgres may take its generic plan.
// passPlanBlocks is how many paragraphs the room's spec holds: far more blocks than Postgres
// assumes of an array whose length it cannot see, so a generic plan of a read over the room's
// blocks is costed below the average of the plans made for the runs, and Postgres takes it at
// the sixth run unless the pass tells it otherwise.
const (
	passPlanRuns   = 7
	passPlanBlocks = 300
)

// The reconciliation's pass plans its reads for each run's own arguments and at the sizes its
// tables have then, however often its connection has run them (graphstore.PlanAtCurrentSizes).
//
// A room of passPlanBlocks paragraphs is written and settled, and a second document service whose
// pool holds one connection then runs the real tick passPlanRuns times, the room marked owed
// before each, so every pass runs on that connection. The connection's own record of the
// statements it prepared (pg_prepared_statements) must then show the two reads a pass makes once
// for each block - the room's blocks' documents (refs' planRoomBlocks) and who wrote each block
// first (blockAuthors) - run at least passPlanRuns times, each by a plan made for that run and
// none by a generic plan. A generic plan is costed at the sizes the tables had when it was made
// and kept: made while a room is small, it reads a table whole for each block of whichever room
// a later pass indexes.
func TestThePassPlansItsReadsForEachRunPastTheFifthOnAConnection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	writer, _ := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	var spec strings.Builder
	for paragraph := 1; paragraph <= passPlanBlocks; paragraph++ {
		fmt.Fprintf(&spec, "Paragraph %d of the room's spec.\n\n", paragraph)
	}
	if _, err := joinedReplaceText(writer, room.spec, spec.String(), model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write the %d paragraphs of %s's spec: %v", passPlanBlocks, room.issue, err)
	}
	if err := writer.waitForDurableAppends(ctx, room.tree); err != nil {
		t.Fatalf("wait for the appends of %s's room: %v", room.issue, err)
	}
	if err := writer.settleOwedNow(ctx, room.tree, time.Now().Add(roomWait(t))); err != nil {
		t.Fatalf("settle %s's room: %v", room.issue, err)
	}

	config := database.Pool.Config()
	config.MaxConns, config.MinConns = 1, 0
	opened, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open a pool of one connection: %v", err)
	}
	one := &store.Store{Pool: store.NewPool(opened)}
	testcleanup.Add(t, one.Pool.Close)
	service, _ := newTestServiceInstance(t, one)
	passes := 0
	service.afterBlockIndexWrite = func(string) error {
		passes++
		return nil
	}
	for run := range passPlanRuns {
		// The row as the backfill leaves a document no pass of this rule has stated.
		markUnstated(t, database, room.spec)
		tick, err := service.ReconcileBlockIndex(ctx, BlockIndexCursors{}, nil)
		if err != nil {
			t.Fatalf("reconcile the block index: %v", err)
		}
		if tick.LiveAhead != 0 || tick.Unreadable != 0 || passes != run+1 {
			t.Fatalf("tick %d left %d passes made, %d rooms live ahead and %d unreadable, want one pass over the room a tick",
				run+1, passes, tick.LiveAhead, tick.Unreadable)
		}
	}

	type prepared struct {
		Statement      string
		Generic, Plans int64
	}
	rows, err := one.Pool.Query(ctx, `select statement, generic_plans, custom_plans from pg_prepared_statements`)
	if err != nil {
		t.Fatalf("read the statements the connection prepared: %v", err)
	}
	statements, err := pgx.CollectRows(rows, pgx.RowToStructByPos[prepared])
	if err != nil {
		t.Fatalf("read the statements the connection prepared: %v", err)
	}
	for _, read := range []struct{ name, holds string }{
		{"planRoomBlocks' read of the documents of the room's blocks", "held.document_id <> wanted.document_id::uuid"},
		{"blockAuthors' read of who wrote each block first", "first.version is not null, first.authors"},
	} {
		var found []prepared
		for _, statement := range statements {
			if strings.Contains(statement.Statement, read.holds) {
				found = append(found, statement)
			}
		}
		if len(found) != 1 {
			t.Errorf("the connection prepared %d statements holding %q, want %s once", len(found), read.holds, read.name)
			continue
		}
		if found[0].Plans < passPlanRuns {
			t.Errorf("%s ran %d times by a plan made for the run, want each of the %d passes' on this connection", read.name, found[0].Plans, passPlanRuns)
		}
		if found[0].Generic != 0 {
			t.Errorf("%s ran %d times by a generic plan and %d by a plan made for the run, want every run of a pass planned for its own arguments at the tables' sizes",
				read.name, found[0].Generic, found[0].Plans)
		}
	}
}
