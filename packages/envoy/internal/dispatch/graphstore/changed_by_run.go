package graphstore

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A write made for a run of the command bar names each node it created or changed as changed by
// that run (dispatch://INFRA-1950/spec#b-r-interaction): one TupleChangedByRun out of the node to
// the run's interaction, appended by QueueWrite and QueueCaused (and so by Write and Caused) in the
// write's own transaction and under the write's own cause, after the write's own tuples. A request
// names its run in a header, which api reads once, holds to the caller's whole credential and to
// the run's window, and hands every writer below it through WithRun, so no writer takes the run
// as an argument: each takes the context it writes for. A write on a context
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

// heldChangedByRun is, of the nodes the text arrays at kinds and ids name by kind and id, each
// that already holds an asserted changed_by_run tuple to the interaction of the run the parameter
// at run names: the newest tuple of each such relation, read as every relation as it stands now
// is read (CurrentTupleRead), through graph_tuples_inbound, which keys the run's interaction, the
// kind and then the node.
func heldChangedByRun(run, kinds, ids string) string {
	return `
	select from_kind, from_id from (
		` + CurrentTupleRead(`t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.asserted`,
		`t.to_kind = '`+KindInteraction+`' and t.to_id = `+run+`::text and t.kind = '`+TupleChangedByRun+`'
			and (t.from_kind, t.from_id) in (select kind, id from unnest(`+kinds+`::text[], `+ids+`::text[]) as marked (kind, id))`) + `
	) newest
	where asserted`
}

// queueChangedByRun queues the append of marks less each the store already holds: a node that
// holds an asserted changed_by_run to the run stands changed by it, so a second write of it for
// the same run states nothing, and the tuple is appended once for each node and run. The one
// statement reads what the store holds and appends the rest, in the order marks names them, in
// the write's own transaction; it queues none for no marks, so a write on a context with no run
// sends nothing more. The read takes no lock: what orders two writes of one node for one run is
// the row each takes before it - the node's own row, which QueueWrite's node write and QueueNodes
// before QueueCaused hold until the transaction ends, or the owner row a pointer's writer takes
// first - so the second's statement reads, in a snapshot of its own, the tuple the first
// committed.
func queueChangedByRun(b *store.WriteBatch, marks []Tuple) error {
	if len(marks) == 0 {
		return nil
	}
	params, err := tupleParams(marks)
	if err != nil {
		return err
	}
	run := "$" + strconv.Itoa(len(params)+1)
	b.Queue(fmt.Sprintf("append the changed_by_run tuples of run %s", marks[0].ToID), `
		insert into graph_tuples (`+tupleWriteColumns+`)
		select `+tupleSelect+`
		from unnest(`+tupleArrays(1)+`) with ordinality as tuple(`+tupleWriteColumns+`, position)
		where (tuple.from_kind, tuple.from_id) not in (`+heldChangedByRun(run, "$1", "$2")+`)
		order by position
		on conflict do nothing
	`, append(params, marks[0].ToID)...)
	return nil
}
