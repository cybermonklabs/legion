package startup

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/storesmove"
	"github.com/sjawhar/envoy/internal/dispatch/storesmove/storesmovetest"
)

// storesMovedBuild is the commit the boot step runs for in this test, so an announcement the
// test writes under another commit names the build a rollback reaches.
const storesMovedBuild = "5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a"

// storesMovedObjectsSQL is every object of the nine the move keeps for the release before that
// the catalog holds, as "<catalog> <name>": the two triggers on graph_nodes in pg_trigger, the
// functions of their names in pg_proc, each of the four names that pg_class holds, with its
// relkind, so a name that is still a table reads as one, and the index graph_tuples_workflow_inbound
// (relkind i).
const storesMovedObjectsSQL = `
select 'pg_trigger ' || t.tgname from pg_trigger t
 where t.tgrelid = to_regclass('graph_nodes') and t.tgname in ('graph_nodes_workflow_state', 'graph_nodes_workflow_project')
union all
select 'pg_proc ' || p.proname from pg_proc p
 where p.pronamespace = current_schema()::regnamespace and p.proname in ('graph_nodes_workflow_state', 'graph_nodes_workflow_project')
union all
select 'pg_class ' || c.relname || ' ' || c.relkind::text from pg_class c
 where c.relnamespace = current_schema()::regnamespace
   and c.relname in ('priority_nodes', 'priority_tuples', 'workflow_nodes', 'workflow_tuples', 'graph_tuples_workflow_inbound')
order by 1`

// storesMovedIndex is how storesMovedObjectsSQL names the index the move keeps for the release
// before's read of the workflow tuples through their view.
const storesMovedIndex = "pg_class graph_tuples_workflow_inbound i"

// storesMovedObjects is what storesMovedObjectsSQL answers over a database the move swapped.
var storesMovedObjects = []string{
	storesMovedIndex,
	"pg_class priority_nodes v", "pg_class priority_tuples v", "pg_class workflow_nodes v", "pg_class workflow_tuples v",
	"pg_proc graph_nodes_workflow_project", "pg_proc graph_nodes_workflow_state",
	"pg_trigger graph_nodes_workflow_project", "pg_trigger graph_nodes_workflow_state",
}

// storesTablesStand is what storesMovedObjectsSQL answers over a database whose four names are
// still the tables main's migrations create, before the move: the migrations built the index too.
var storesTablesStand = []string{
	storesMovedIndex,
	"pg_class priority_nodes r", "pg_class priority_tuples r", "pg_class workflow_nodes r", "pg_class workflow_tuples r",
}

// storesTablesLeft is what it answers over that database once the step ran: the step drops the
// index, which serves no read through a view that was never made, and leaves the four tables.
var storesTablesLeft = storesTablesStand[1:]

// TestTheBootStepDropsTheMovedStoresObjectsOnlyAfterStoresMoved holds the boot step's entry
// StoresMovedRetirement (dispatch://INFRA-1098's R32) to its guard, the capability stores_moved of
// the newest announcement of another build or a boot that created the database from empty, through
// startup.Retire as the boot runs it. In each database but the last, the test writes the old
// tables in the release before's shapes (storesmovetest.Write) and runs the boot's move
// (storesmove.Move), which swaps them for the four views and creates the two triggers with their
// functions; the migrations built the index graph_tuples_workflow_inbound beside them. It prints
// what the catalog holds before and after each run of the step:
//   - the newest announcement of another build lists stores_moved: all nine go, and the graph
//     store keeps every row of the three kinds;
//   - it does not: all nine stand, and the log names the announcement and the missing
//     capability;
//   - the boot created the database from empty: all nine go;
//   - another transaction holds graph_nodes past the half second: all nine stand, the log names
//     the statement that waited and the timeout, and the next run of the step, once the holder
//     ends, drops them;
//   - another transaction holds graph_tuples past the half second: the triggers, the functions and
//     the views go with the transaction, so all nine stand, the log names the drop of the index as
//     the statement that waited, and the next run drops them all;
//   - a second run after a drop finds nothing, logs nothing, and leaves the database as it was,
//     with no graph_tuples_workflow_inbound;
//   - a database this boot created from empty whose four names are still the tables main's
//     migrations create, as before the move: the step drops no table, logs no failure, drops the
//     index and no more, and the tables keep their rows.
func TestTheBootStepDropsTheMovedStoresObjectsOnlyAfterStoresMoved(t *testing.T) {
	t.Run("released", func(t *testing.T) {
		database := movedStoresDatabase(t)
		before := movedKindsRows(t, database.Pool)
		announceBuild(t, database.Pool, "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1", []string{"anchored_to", processes.CapabilityStoresMoved})
		logged := retireStoresMoved(t, database.Pool, false)
		requireStoresMovedObjects(t, database.Pool, nil)
		requireNoWorkflowInboundIndex(t, database.Pool)
		requireLoggedLine(t, logged, `msg="dispatch: the boot's retirement deleted its objects"`, "announcement_commit=c1c1c1c1")
		if after := movedKindsRows(t, database.Pool); after != before {
			t.Errorf("the graph store held %s of the three kinds before the drop and %s after it, want them unchanged", before, after)
		}

		// A second run after the drop finds nothing and changes nothing.
		again := retireStoresMoved(t, database.Pool, false)
		requireStoresMovedObjects(t, database.Pool, nil)
		requireNoWorkflowInboundIndex(t, database.Pool)
		if strings.Contains(again, StoresMovedRetirement.Name) {
			t.Errorf("the second run after the drop logged a line of the entry, want none:\n%s", again)
		}
	})
	t.Run("release before", func(t *testing.T) {
		database := movedStoresDatabase(t)
		announceBuild(t, database.Pool, "c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2", []string{"anchored_to", processes.CapabilityIssueRoom})
		logged := retireStoresMoved(t, database.Pool, false)
		requireStoresMovedObjects(t, database.Pool, storesMovedObjects)
		requireLoggedLine(t, logged, "kept its objects", "announcement_commit=c2c2c2c2", "missing=["+processes.CapabilityStoresMoved+"]")
	})
	t.Run("from empty", func(t *testing.T) {
		database := movedStoresDatabase(t)
		logged := retireStoresMoved(t, database.Pool, true)
		requireStoresMovedObjects(t, database.Pool, nil)
		requireLoggedLine(t, logged, `msg="dispatch: the boot's retirement deleted its objects"`, "created_from_empty=true")
	})
	t.Run("lock timeout", func(t *testing.T) {
		database := movedStoresDatabase(t)
		announceBuild(t, database.Pool, "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3", []string{processes.CapabilityStoresMoved})
		ctx := context.Background()
		holder, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the holder: %v", err)
		}
		if _, err := holder.Exec(ctx, `lock table graph_nodes in access share mode`); err != nil {
			t.Fatalf("hold graph_nodes: %v", err)
		}
		started := time.Now()
		logged := retireStoresMoved(t, database.Pool, false)
		took := time.Since(started)
		if err := holder.Rollback(ctx); err != nil {
			t.Fatalf("end the holder: %v", err)
		}
		requireStoresMovedObjects(t, database.Pool, storesMovedObjects)
		requireLoggedLine(t, logged, "failed and changed nothing",
			"drop trigger graph_nodes_workflow_state on graph_nodes waited longer than 500ms for its lock", "55P03")
		t.Logf("the step gave up after %s with graph_nodes held", took)
		if took > 4*time.Second {
			t.Errorf("the step waited %s on graph_nodes, want about the half second its lock_timeout bounds", took)
		}

		next := retireStoresMoved(t, database.Pool, false)
		requireStoresMovedObjects(t, database.Pool, nil)
		requireLoggedLine(t, next, `msg="dispatch: the boot's retirement deleted its objects"`)
	})
	t.Run("lock timeout on graph_tuples", func(t *testing.T) {
		database := movedStoresDatabase(t)
		announceBuild(t, database.Pool, "c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4", []string{processes.CapabilityStoresMoved})
		ctx := context.Background()
		holder, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the holder: %v", err)
		}
		if _, err := holder.Exec(ctx, `lock table graph_tuples in access share mode`); err != nil {
			t.Fatalf("hold graph_tuples: %v", err)
		}
		started := time.Now()
		logged := retireStoresMoved(t, database.Pool, false)
		took := time.Since(started)
		if err := holder.Rollback(ctx); err != nil {
			t.Fatalf("end the holder: %v", err)
		}
		// The drop of the index is the statement that waits, last of the nine, and the transaction
		// rolls back the triggers, the functions and the views the statements before it dropped.
		requireStoresMovedObjects(t, database.Pool, storesMovedObjects)
		requireLoggedLine(t, logged, "failed and changed nothing",
			"drop index graph_tuples_workflow_inbound waited longer than 500ms for its lock", "55P03")
		t.Logf("the step gave up after %s with graph_tuples held", took)
		if took > 4*time.Second {
			t.Errorf("the step waited %s on graph_tuples, want about the half second its lock_timeout bounds", took)
		}

		next := retireStoresMoved(t, database.Pool, false)
		requireStoresMovedObjects(t, database.Pool, nil)
		requireNoWorkflowInboundIndex(t, database.Pool)
		requireLoggedLine(t, next, `msg="dispatch: the boot's retirement deleted its objects"`)
	})
	t.Run("from empty before the move", func(t *testing.T) {
		database := storetest.Open(t)
		storesmovetest.Write(t, database.Pool)
		before := oldTablesRows(t, database.Pool)
		requireStoresMovedObjects(t, database.Pool, storesTablesStand)
		logged := retireStoresMoved(t, database.Pool, true)
		requireStoresMovedObjects(t, database.Pool, storesTablesLeft)
		requireNoWorkflowInboundIndex(t, database.Pool)
		requireLoggedLine(t, logged, `msg="dispatch: the boot's retirement deleted its objects"`, "created_from_empty=true")
		if strings.Contains(logged, "failed") {
			t.Errorf("the step logged a failure over the four tables, want none:\n%s", logged)
		}
		if after := oldTablesRows(t, database.Pool); after != before {
			t.Errorf("the four tables held %s before the step and %s after it, want them unchanged", before, after)
		}
	})
}

// requireNoWorkflowInboundIndex fails t where pool's database holds an index named
// graph_tuples_workflow_inbound, whatever table it stands on.
func requireNoWorkflowInboundIndex(t *testing.T, pool *store.Pool) {
	t.Helper()
	var standing int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from pg_class where relname = 'graph_tuples_workflow_inbound'`).Scan(&standing); err != nil {
		t.Fatalf("read whether graph_tuples_workflow_inbound stands: %v", err)
	}
	if standing != 0 {
		t.Errorf("graph_tuples_workflow_inbound stands after the step, want it dropped")
	}
}

// movedStoresDatabase is a migrated database whose old tables hold the release before's rows and
// which the boot's move then swapped for the views and the triggers.
func movedStoresDatabase(t *testing.T) *store.Store {
	t.Helper()
	database := storetest.Open(t)
	storesmovetest.Write(t, database.Pool)
	result, err := storesmove.Move(context.Background(), database.Pool.Config().ConnString(), storesmove.Options{})
	if err != nil {
		t.Fatalf("move the stores: %v", err)
	}
	if !result.Moved || result.Census == nil || result.Census.Differs() {
		t.Fatalf("the move moved=%t with a census of %v, want it moved with no difference", result.Moved, result.Census)
	}
	requireStoresMovedObjects(t, database.Pool, storesMovedObjects)
	return database
}

// announceBuild writes an announcement of another build, under commit, listing kinds.
func announceBuild(t *testing.T, pool *store.Pool, commit string, kinds []string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`insert into server_build_announcements (commit, tuple_kinds) values ($1, $2)`, commit, kinds); err != nil {
		t.Fatalf("announce %s: %v", commit, err)
	}
}

// retireStoresMoved runs the boot step over pool with StoresMovedRetirement alone, as a process of
// storesMovedBuild whose boot created the database from empty where fromEmpty, and answers what it
// logged.
func retireStoresMoved(t *testing.T, pool *store.Pool, fromEmpty bool) string {
	t.Helper()
	var logged bytes.Buffer
	Retire(context.Background(), pool, storesMovedBuild, fromEmpty, slog.New(slog.NewTextHandler(&logged, nil)),
		[]Retirement{StoresMovedRetirement})
	t.Logf("the boot step logged:\n%s", logged.String())
	return logged.String()
}

// requireStoresMovedObjects fails t unless storesMovedObjectsSQL answers want over pool.
func requireStoresMovedObjects(t *testing.T, pool *store.Pool, want []string) {
	t.Helper()
	rows, err := pool.Query(context.Background(), storesMovedObjectsSQL)
	if err != nil {
		t.Fatalf("read the catalog: %v", err)
	}
	defer rows.Close()
	var standing []string
	for rows.Next() {
		var object string
		if err := rows.Scan(&object); err != nil {
			t.Fatalf("read the catalog: %v", err)
		}
		standing = append(standing, object)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the catalog: %v", err)
	}
	t.Logf("the catalog holds %d of the objects: %v", len(standing), standing)
	if !slices.Equal(standing, want) {
		t.Errorf("the catalog holds %v, want %v", standing, want)
	}
}

// requireLoggedLine fails t unless one line of logged holds every one of parts.
func requireLoggedLine(t *testing.T, logged string, parts ...string) {
	t.Helper()
	for _, line := range strings.Split(logged, "\n") {
		if !strings.Contains(line, `retirement="`+StoresMovedRetirement.Name+`"`) {
			continue
		}
		held := true
		for _, part := range parts {
			held = held && strings.Contains(line, part)
		}
		if held {
			return
		}
	}
	t.Errorf("no line the step logged for %q holds all of %q", StoresMovedRetirement.Name, parts)
}

// movedKindsRows counts and digests the graph store's node rows and tuples of the three kinds.
func movedKindsRows(t *testing.T, pool *store.Pool) string {
	t.Helper()
	var counted string
	if err := pool.QueryRow(context.Background(), `
		select (select count(*) || ' nodes ' || coalesce(md5(string_agg(n::text, ',' order by n.kind, n.id)), '') from graph_nodes n
		         where n.kind in ('priority', 'step', 'plan'))
		    || ', ' ||
		       (select count(*) || ' tuples ' || coalesce(md5(string_agg(g::text, ',' order by g.seq)), '') from graph_tuples g
		         where g.from_kind in ('priority', 'step', 'plan') or g.to_kind = 'priority')`).Scan(&counted); err != nil {
		t.Fatalf("read the graph store's rows of the three kinds: %v", err)
	}
	return counted
}

// oldTablesRows counts and digests the rows of the four old tables.
func oldTablesRows(t *testing.T, pool *store.Pool) string {
	t.Helper()
	var counted string
	if err := pool.QueryRow(context.Background(), `
		select (select count(*) || ' ' || coalesce(md5(string_agg(n::text, ',' order by n.kind, n.id)), '') from priority_nodes n)
		    || ', ' || (select count(*) || ' ' || coalesce(md5(string_agg(p::text, ',' order by p.seq)), '') from priority_tuples p)
		    || ', ' || (select count(*) || ' ' || coalesce(md5(string_agg(w::text, ',' order by w.kind, w.id)), '') from workflow_nodes w)
		    || ', ' || (select count(*) || ' ' || coalesce(md5(string_agg(u::text, ',' order by u.seq)), '') from workflow_tuples u)`).Scan(&counted); err != nil {
		t.Fatalf("read the four tables' rows: %v", err)
	}
	return counted
}
