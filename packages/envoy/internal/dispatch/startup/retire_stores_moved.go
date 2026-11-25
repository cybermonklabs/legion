package startup

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/processes"
)

var _ = registerRetirement(StoresMovedRetirement)

// StoresMovedKinds is the guard of what the move of the priority store and the workflow store
// kept (dispatch://INFRA-1098's R32): the capability stores_moved, which only a build that reads
// and writes the two stores in the graph store announces. It is a list of its own, apart from
// every child's, so the guard reads that one capability and no relation kind.
var StoresMovedKinds = []string{processes.CapabilityStoresMoved}

// The objects the move keeps for the release before, by their names: the two triggers on
// graph_nodes and the functions of their names (graphstore.StoresMovedTriggers), the four names of
// the old tables, which the swap makes views over graph_nodes and graph_tuples (storesmove's
// storesViews), and the index that serves the release before's reads of workflow_tuples through
// its view (migration 20261125093000_graph_tuples_workflow_inbound_index).
var (
	storesMovedTriggers  = []string{"graph_nodes_workflow_state", "graph_nodes_workflow_project"}
	storesMovedFunctions = []string{"graph_nodes_workflow_state()", "graph_nodes_workflow_project()"}
	storesMovedViews     = []string{"priority_nodes", "priority_tuples", "workflow_nodes", "workflow_tuples"}
	storesMovedIndexes   = []string{"graph_tuples_workflow_inbound"}
)

// storesMovedLockTimeout bounds each lock the drop waits for. DROP TRIGGER takes graph_nodes in
// access exclusive mode, which every read and write of a node waits behind while the drop waits,
// and DROP INDEX takes graph_tuples in the same mode; half a second costs nothing under Recreate,
// where no other process of Dispatch holds either table, and in a rolling deployment it holds the
// serving release's requests up no longer than that.
const storesMovedLockTimeout = 500 * time.Millisecond

// StoresMovedRetirement drops what the move of the priority store and the workflow store into
// the graph store kept for one release (storesmove, dispatch://INFRA-1098's R32): the two triggers
// graph_nodes_workflow_state and graph_nodes_workflow_project with the functions of their names,
// the views priority_nodes, priority_tuples, workflow_nodes and workflow_tuples, and the index
// graph_tuples_workflow_inbound. The release before reads and writes the four names during a roll
// and after a rollback, and the two triggers complete the runs and plans it writes through them;
// while they stand, its writes through them are the one write of the store outside graphstore. The
// index keys the release before's read of an issue's runs and plans through workflow_tuples by the
// near end's kind, as the old table's index did, so that read does not fetch every other child of
// the issue from the heap; only that release reads through it, and every run and plan tuple pays
// for it until it goes.
//
// Its guard is StoresMovedKinds alone, independent of every child's list and of every proof: the
// newest announcement of another build lists stores_moved, so the build a rollback reaches reads
// and writes the graph store itself, or this boot created the database from empty. The move runs
// before the step in the same boot (cmd/dispatch's main), so the boot that moves the stores keeps
// what the move made for the release it replaced, and the first boot of a build after it drops
// them. Nothing else holds them.
//
// It drops in one transaction, under `set local lock_timeout = '500ms'`: each of the two triggers
// that exists, then each of the two functions that exists, then each of the four names that
// pg_class shows as a view, then the index if it exists. A name that is still a table, in a
// database that has not run the move, is left as it is, and that is no failure: DROP VIEW refuses
// a table. A lock the drop waits for longer than the half second rolls the transaction back, so
// every object stays, and the step logs the statement that waited and why, and tries again at the
// next boot. Once the objects are gone the step reads one probe a boot and changes nothing. It
// stays in the code for good, for a database that boots from an older state, a restore among them.
var StoresMovedRetirement = Retirement{
	Name:  "the stores' move's views and triggers",
	Kinds: StoresMovedKinds,
	Stands: func(ctx context.Context, q queryer) (bool, error) {
		triggers, functions, views, indexes, err := standingStoresMovedObjects(ctx, q)
		return len(triggers)+len(functions)+len(views)+len(indexes) > 0, err
	},
	Holds: func(context.Context, queryer) ([]Holder, error) { return nil, nil },
	Delete: func(ctx context.Context, tx pgx.Tx) error {
		timeout := strconv.FormatInt(storesMovedLockTimeout.Milliseconds(), 10) + "ms"
		if _, err := tx.Exec(ctx, `set local lock_timeout = '`+timeout+`'`); err != nil {
			return fmt.Errorf("bound the drop's lock waits at %s: %w", timeout, err)
		}
		triggers, functions, views, indexes, err := standingStoresMovedObjects(ctx, tx)
		if err != nil {
			return err
		}
		var statements []string
		for _, trigger := range triggers {
			statements = append(statements, `drop trigger `+trigger+` on graph_nodes`)
		}
		for _, function := range functions {
			statements = append(statements, `drop function `+function)
		}
		for _, view := range views {
			statements = append(statements, `drop view `+view)
		}
		for _, index := range indexes {
			statements = append(statements, `drop index `+index)
		}
		for _, statement := range statements {
			if _, err := tx.Exec(ctx, statement); err != nil {
				var refused *pgconn.PgError
				if errors.As(err, &refused) && refused.Code == "55P03" {
					return fmt.Errorf("%s waited longer than %s for its lock, so every object stays: %w", statement, timeout, err)
				}
				return fmt.Errorf("%s: %w", statement, err)
			}
		}
		return nil
	},
}

// standingStoresMovedObjects reads which of the objects StoresMovedRetirement drops stand: the
// triggers of storesMovedTriggers on graph_nodes, the functions of storesMovedFunctions, the names
// of storesMovedViews that resolve to a view and the names of storesMovedIndexes that resolve to
// an index, each in its list's order. A name that resolves to a table is none of them.
func standingStoresMovedObjects(ctx context.Context, q queryer) (triggers, functions, views, indexes []string, err error) {
	err = q.QueryRow(ctx, `
		select
			array(select name from unnest($1::text[]) with ordinality listed (name, place)
			      where exists (select 1 from pg_trigger t
			                    where t.tgrelid = to_regclass('graph_nodes') and t.tgname = listed.name)
			      order by place),
			array(select name from unnest($2::text[]) with ordinality listed (name, place)
			      where to_regprocedure(listed.name) is not null
			      order by place),
			array(select name from unnest($3::text[]) with ordinality listed (name, place)
			      where exists (select 1 from pg_class c where c.oid = to_regclass(listed.name) and c.relkind = 'v')
			      order by place),
			array(select name from unnest($4::text[]) with ordinality listed (name, place)
			      where exists (select 1 from pg_class c where c.oid = to_regclass(listed.name) and c.relkind = 'i')
			      order by place)`,
		storesMovedTriggers, storesMovedFunctions, storesMovedViews, storesMovedIndexes,
	).Scan(&triggers, &functions, &views, &indexes)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("read which of the stores' move's views and triggers stand: %w", err)
	}
	return triggers, functions, views, indexes, nil
}
