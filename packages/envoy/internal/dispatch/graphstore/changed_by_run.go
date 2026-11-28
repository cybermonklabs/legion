package graphstore

import (
	"context"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A write made for a run of the command bar names each node it created or changed as changed by
// that run (spec requirement r-interaction): one TupleChangedByRun out of the node to the run's
// interaction, appended by the append QueueWrite and QueueCaused queue, in the write's own
// transaction and under the write's own cause, beside the write's own tuples. A request names its
// run in a header, which api reads once, holds to the caller's whole credential and to the run's
// window, and hands every writer below it through store.WithRun on its context; the write phase
// built for that context carries the run (store.NewWriteBatch), and every writer here reads it off
// the batch it queues on, so no writer takes the run as an argument. A write phase built for a
// context holding no run - a background pass, the block-index reconciliation tick, the backfill, a
// seed - appends none, so no write a request did not make is attributed to a run. Which nodes a
// write changed is its writer's to say (Node.Changed, Tuple.Changes), never the store's to infer:
// a block holds no text, so an edit of its words moves nothing a node write compares.

// PlanChangedForRun is the write of a writer whose own write reaches no node of the store, and
// that writes a node for no other reason than to name it changed by the run ctx carries (spec
// requirement r-interaction): an issue's own field write (api's writeIssue, the write core the
// PATCH and each step of a status change run), the edit of an ask's or a comment's words (api's
// editAsk and editComment), and the edit and the delete of an issue message and the delete of a
// comment (api's editMessage, deleteMessage and deleteComment). On a context that carries no run
// it calls nothing and plans nothing, and answers the zero Uncaused, for which QueueNodes and
// QueueCaused queue nothing, so a write made for no run sends no statement more than it did.
//
// On a context that carries a run it plans the write of the node that node answers, as
// PlanUncaused plans one: the node as the object's create and the backfill write it - in its
// project, or between its parties, made by its creator at its created_at - restating the object,
// so a node already there keeps the state, the visibility, the made_at and the made_by the store
// holds and moves nothing else, and an object a process of the release before created gets the
// node the backfill would give it, which the backfill, writing only a node the store lacks, then
// leaves as it is. node is called only then, in the writer's read phase, so a read it makes to
// build the node - the scope of an ask or a comment (ScopeOf) - is sent for a run alone. The
// changed_by_run the node owes is the change's own, dated at at and signed by by, where the node
// is dated and signed as its object was made, and the Uncaused holds it for QueueCaused, which
// appends it with what the node states under the writer's own event, as a create's facts are,
// unless the node already stands changed by that run. The writer queues the node write
// (QueueNodes) last of the node rows its transaction takes, after its own rows and their
// documents, and QueueCaused after its event, on a write phase made for the same run.
func PlanChangedForRun(ctx context.Context, at time.Time, by model.Actor, node func() (Node, error)) (Uncaused, error) {
	if store.RunOf(ctx) == "" {
		return Uncaused{}, nil
	}
	changed, err := node()
	if err != nil {
		return Uncaused{}, err
	}
	write := PlanUncaused([]Node{changed}, nil)
	write.changes = []runChange{{kind: changed.Kind, id: changed.ID, at: at, by: by}}
	return write, nil
}

// runChange is a node PlanChangedForRun names changed by the run, dated and signed as the change
// was made rather than as the node was.
type runChange struct {
	kind, id string
	at       time.Time
	by       model.Actor
}

// changedByRunMarks is the TupleChangedByRun each node a write marks changed owes run, one for
// each node: out of each node handed with Node.Changed, caused as that node's write is and dated
// and signed as it is, out of each node of changes, dated and signed as the change says
// (PlanChangedForRun), and out of the node each fact marked Tuple.Changes goes out of, as that
// fact is. It answers nil, and allocates nothing, for no run. The append states each of them only
// where the node does not stand changed by run already (queueAppend).
//
// A write holds each node once (QueueWrite), so the nodes need no comparison among themselves; a
// marked fact is compared with what is already marked, since a retarget states two facts out of
// one node and the node may be written beside them.
func changedByRunMarks(run string, nodes []Node, changes []runChange, tuples []Tuple) []Tuple {
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
		if node.Changed {
			mark(node.Kind, node.ID, node.MadeAt, node.MadeBy, node.SourceEvent)
		}
	}
	for _, change := range changes {
		mark(change.kind, change.id, change.at, change.by, NoEvent)
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
