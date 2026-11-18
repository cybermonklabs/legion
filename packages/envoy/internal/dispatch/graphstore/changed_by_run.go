package graphstore

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A write made for a run of the command bar names each node it created or changed as changed by
// that run (dispatch://INFRA-1950/spec#b-r-interaction): one TupleChangedByRun out of the node to
// the run's interaction, appended by Write and Caused in the write's own transaction and under the
// write's own cause, beside the write's own tuples. A request names its run in a header, which api
// reads once, holds to the caller's whole credential and to the run's window, and hands every
// writer below it through WithRun, so no writer takes the run as an argument. A write on a context
// holding no run - a background pass, the block-index reconciliation tick, the backfill, a seed -
// appends none, so no write a request did not make is attributed to a run. Which nodes a write
// changed is its writer's to say (Node.Changed, Tuple.Changes), never the store's to infer: a block
// holds no text, so an edit of its words moves nothing a node write compares.

// runKey is the context key WithRun carries a run under.
type runKey struct{}

// WithRun is ctx carrying run, the uuid of a run of the command bar - its interaction node's id,
// in the lowercase form Postgres writes a uuid as text - for which every write of this store on
// that context is made. It is the one way a run reaches this store. Its caller has proved the run
// live and the caller's own (api's withinGoingRun); nothing here asks either again.
func WithRun(ctx context.Context, run string) context.Context {
	return context.WithValue(ctx, runKey{}, run)
}

// runOf is the run ctx carries (WithRun), and the empty string where it carries none.
func runOf(ctx context.Context) string {
	run, _ := ctx.Value(runKey{}).(string)
	return run
}

// MadeForRun is whether ctx carries a run (WithRun). A writer asks it only where it writes a node
// for no other reason than to name what a run changed - the issue PATCH, whose own write reaches
// no node of the store (api's patchIssue), and the edit of an ask's or a comment's words, whose
// own write reaches no node of the store either (api's editAsk and editComment) - so that a write
// made for no run sends nothing more than it did.
func MadeForRun(ctx context.Context) bool { return runOf(ctx) != "" }

// changedByRunMarks is the TupleChangedByRun each node a write marks changed owes the run ctx
// carries, one for each node: out of each node handed with Node.Changed, caused as that node's
// write is and dated and signed as it is, or as its ChangedAt and ChangedBy say where it carries
// them, and out of the node each fact marked Tuple.Changes goes out of, as that fact is. It
// answers nil, and allocates nothing, on a context that carries no run.
//
// A write holds each node once (Write), so the nodes need no comparison among themselves; a
// marked fact is compared with what is already marked, since a retarget states two facts out of
// one node and the node may be written beside them.
func changedByRunMarks(ctx context.Context, nodes []Node, tuples []Tuple) []Tuple {
	run := runOf(ctx)
	if run == "" {
		return nil
	}
	var marks []Tuple
	mark := func(kind, id string, at time.Time, who model.Actor, event int64) {
		marks = append(marks, Tuple{
			FromKind: kind, FromID: id, Kind: TupleChangedByRun, ToKind: KindInteraction, ToID: run,
			At: at, Who: who, SourceEvent: event, Asserted: true,
		})
	}
	for _, node := range nodes {
		if !node.Changed {
			continue
		}
		at, who := node.MadeAt, node.MadeBy
		if !node.ChangedAt.IsZero() {
			at, who = node.ChangedAt, node.ChangedBy
		}
		mark(node.Kind, node.ID, at, who, node.SourceEvent)
	}
	for _, tuple := range tuples {
		if !tuple.Changes || slices.ContainsFunc(marks, func(marked Tuple) bool {
			return marked.FromKind == tuple.FromKind && marked.FromID == tuple.FromID
		}) {
			continue
		}
		mark(tuple.FromKind, tuple.FromID, tuple.At, tuple.Who, tuple.SourceEvent)
	}
	return marks
}

// heldChangedByRun is, of the nodes $2 and $3 name by kind and id, each that already holds an
// asserted changed_by_run tuple to the interaction of the run $1 names: the newest tuple of each
// such relation, read as every relation as it stands now is read (CurrentTupleRead), through
// graph_tuples_inbound, which keys the run's interaction, the kind and then the node.
var heldChangedByRun = `
	select from_kind, from_id from (
		` + CurrentTupleRead(`t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.asserted`,
	`t.to_kind = '`+KindInteraction+`' and t.to_id = $1 and t.kind = '`+TupleChangedByRun+`'
			and (t.from_kind, t.from_id) in (select kind, id from unnest($2::text[], $3::text[]) as marked (kind, id))`) + `
	) newest
	where asserted`

// changedByRun is marks less each the store already holds: a node that holds an asserted
// changed_by_run to the run stands changed by it, so a second write of it for the same run states
// nothing, and the tuple is appended once for each node and run. It reads them in one statement of
// the write's own transaction, and none for no marks, so a write on a context with no run sends
// nothing more. The read takes no lock: what orders two writes of one node for one run is the row
// each takes before it reads here - the node's own row, which Write's node step and WriteUncaused
// before Caused hold until the transaction ends, or the owner row a pointer's writer takes first -
// so the second reads, in a snapshot of its own, the tuple the first committed.
func changedByRun(ctx context.Context, tx pgx.Tx, marks []Tuple) ([]Tuple, error) {
	if len(marks) == 0 {
		return nil, nil
	}
	kinds, ids := make([]string, len(marks)), make([]string, len(marks))
	for index, marked := range marks {
		kinds[index], ids[index] = marked.FromKind, marked.FromID
	}
	rows, err := tx.Query(ctx, heldChangedByRun, marks[0].ToID, kinds, ids)
	if err != nil {
		return nil, fmt.Errorf("read the changed_by_run tuples run %s holds: %w", marks[0].ToID, err)
	}
	defer rows.Close()
	held := map[[2]string]struct{}{}
	for rows.Next() {
		var kind, id string
		if err := rows.Scan(&kind, &id); err != nil {
			return nil, fmt.Errorf("scan a changed_by_run tuple of run %s: %w", marks[0].ToID, err)
		}
		held[[2]string{kind, id}] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the changed_by_run tuples run %s holds: %w", marks[0].ToID, err)
	}
	if len(held) == 0 {
		return marks, nil
	}
	return slices.DeleteFunc(marks, func(marked Tuple) bool {
		_, stands := held[[2]string{marked.FromKind, marked.FromID}]
		return stands
	}), nil
}
