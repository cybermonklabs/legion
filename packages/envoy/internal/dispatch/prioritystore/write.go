package prioritystore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Write writes every node and appends every tuple inside the caller's transaction, through the
// store's one writer (graphstore.Write). It appends only: no statement updates or deletes a tuple
// row, so the log a read walks is whatever every writer before it appended, in the order they
// appended it, a write's relations before its attributes and each in the order the caller stated
// it. The store writes the node rows before it appends a tuple, so a write takes each node's row
// before it states a fact of that node. The node row is written again where it is already there,
// because it is the projection of its own tuples; MadeAt and MadeBy stay as the row's first write
// left them. The store's node write replaces the row's attributes whole, so a node handed here
// carries every attribute it holds and not only the ones a write moves (SetAttributes).
//
// Every named write below states the base attributes it moves itself, with its own writer and
// moment, and the store then states none of them a second time (graphstore.QueueWrite); a node a
// caller hands here without them has them stated by the store, at the node's MadeAt and by its
// MadeBy.
//
// It is the primitive the named writes below are built from, and what a caller holding a whole
// batch at once reaches for: the seeded corpus states its `advances` relations through it, one
// statement a batch rather than one a relation (cmd/dispatch-seed/write_priorities.go). Either
// list may be empty, so a caller with tuples and no node passes none. It is this package's one
// call of the store's writer, so every write of a priority is the one site the register of the
// store's writers lists for it (store/storetest/lockstep/sites.go).
func Write(ctx context.Context, tx pgx.Tx, nodes []Node, tuples []Tuple) error {
	written := make([]graphstore.Node, len(nodes))
	for index, node := range nodes {
		written[index] = graphstore.Node{
			Kind: node.Kind, ID: node.ID, Supertag: node.Supertag,
			Visibility: node.Visibility, State: node.State, Attributes: node.Attributes,
			MadeAt: node.MadeAt, MadeBy: node.MadeBy, SourceEvent: node.SourceEvent,
		}
	}
	// The head rows of what it writes go in the same batch, after its statements: an `advances`
	// tuple is a relation out of an issue's node, whose head row holds it.
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueWrite(b, written, tuples)
		graphstore.QueueHeads(b, graphstore.HeadsOf(written, tuples))
		return nil
	}); err != nil {
		return fmt.Errorf("write priorities: %w", err)
	}
	return nil
}

// Create writes one priority and states what it was written with: its visibility, its state, and
// each attribute values names, every one of them in the caller's transaction. The id is the
// caller's, as an issue key is: nothing here makes one.
//
// It refuses an id the store already holds with ErrPriorityExists rather than writing the node
// again, because a second create is a caller's mistake and the first write's own history would
// otherwise read as this one's. The store's node write makes a node the store does not hold and
// writes one it holds again, so it cannot refuse a second create itself: Create reads whether the
// id is held first, under the order's allocation lock (lockRankAllocation), which every create
// takes, as POST /api/v1/priorities does for the key it draws (RankAt). So a second create of one
// id waits for the first to end, and its read then answers the row the first committed.
func Create(ctx context.Context, tx pgx.Tx, id string, values map[string]string, actor model.Actor, at time.Time) error {
	if err := lockRankAllocation(ctx, tx); err != nil {
		return err
	}
	var held bool
	if err := tx.QueryRow(ctx, `
		select exists (select 1 from graph_nodes where kind = '`+KindPriority+`' and id = $1)
	`, id).Scan(&held); err != nil {
		return fmt.Errorf("read whether priority %s is written: %w", id, err)
	}
	if held {
		return fmt.Errorf("write priority %s: %w", id, ErrPriorityExists)
	}
	attributes := make(map[string]any, len(values))
	for name, value := range values {
		attributes[name] = value
	}
	tuples := make([]Tuple, 0, 2+len(values))
	tuples = append(tuples,
		valueTuple(id, AttributeVisibility, VisibilityProject, actor, at),
		valueTuple(id, AttributeState, StateLive, actor, at))
	return Write(ctx, tx, []Node{{
		Kind: KindPriority, ID: id, Supertag: KindPriority,
		Visibility: VisibilityProject, State: StateLive, Attributes: attributes,
		MadeAt: at, MadeBy: actor, SourceEvent: NoEvent,
	}}, append(tuples, attributeTuples(id, values, actor, at)...))
}

// SetAttributes moves the node row's attributes and appends one value tuple per attribute, in the
// caller's one transaction: the row answers the next read and the tuples answer the history. The
// attributes values does not name are left as they were, so a write of one attribute states one
// fact. It refuses an id the store does not hold, and an id whose priority is gone, with
// ErrNoPriority (liveRow), and states nothing for an empty values.
//
// The store's node write replaces the row's attributes whole. So the row is read under its lock
// first (lockedNode), each attribute values names is set over the one the row holds, every other
// is kept, and the merged whole is what the store writes: the row `attributes || values` leaves,
// read off a version no other writer can move until this transaction ends.
func SetAttributes(ctx context.Context, tx pgx.Tx, id string, values map[string]string, actor model.Actor, at time.Time) error {
	if len(values) == 0 {
		return nil
	}
	node, err := lockedNode(ctx, tx, id, liveRow)
	if err != nil {
		return err
	}
	if node.Attributes == nil {
		node.Attributes = make(map[string]any, len(values))
	}
	for name, value := range values {
		node.Attributes[name] = value
	}
	return Write(ctx, tx, []Node{node}, attributeTuples(id, values, actor, at))
}

// SetState moves the node row's state and appends the tuple that states it. Deleting a priority
// is StateGone and restoring it is StateLive: no row is removed, and the history holds who ended
// it and when. The tuple carries this write's writer and moment, which is why it is stated here
// and not left to the store, whose node write would date a state it states at the node's MadeAt.
//
// Writing StateLive is the one write that lands on a gone row (anyRow), because a restore is
// what takes a priority out of that state; writing any other state, StateGone included, asks for
// a live priority, so a second delete is refused ErrNoPriority and appends no second tuple.
func SetState(ctx context.Context, tx pgx.Tx, id, state string, actor model.Actor, at time.Time) error {
	rows := liveRow
	if state == StateLive {
		rows = anyRow
	}
	node, err := lockedNode(ctx, tx, id, rows)
	if err != nil {
		return err
	}
	node.State = state
	return Write(ctx, tx, []Node{node}, []Tuple{valueTuple(id, AttributeState, state, actor, at)})
}

// Advance states that one issue advances one priority. The relation is stated on the issue, so
// its far end is the priority and a read of either end answers it (Timeline reads the ends into
// a priority; api/issue_queries.go reads the ends out of a page of issues). The issue end is the
// issue's key, which is the id of the issue's node. It appends a tuple and nothing else: that the
// priority is live and that the issue exists are the caller's refusals, since this store holds
// no issue row, and the caller holds the issue's own row lock, which orders two writes of one
// issue's set (api/issue_advances.go).
func Advance(ctx context.Context, tx pgx.Tx, issueKey, id string, actor model.Actor, at time.Time) error {
	return relate(ctx, tx, issueKey, id, true, actor, at)
}

// Retract ends the relation Advance stated, by appending a second tuple that does not assert it.
// Nothing is updated and nothing is deleted, so the history still holds when the issue advanced
// the priority and who ended that.
func Retract(ctx context.Context, tx pgx.Tx, issueKey, id string, actor model.Actor, at time.Time) error {
	return relate(ctx, tx, issueKey, id, false, actor, at)
}

func relate(ctx context.Context, tx pgx.Tx, issueKey, id string, asserted bool, actor model.Actor, at time.Time) error {
	return Write(ctx, tx, nil, []Tuple{{
		FromKind: KindIssue, FromID: issueKey, Kind: RelationAdvances,
		ToKind: KindPriority, ToID: id,
		At: at, Who: actor, SourceEvent: NoEvent, Asserted: asserted,
	}})
}

// The rows a node write lands on, beside the id it names. liveRow is every write but one: a
// priority whose state is gone is no priority, so a write of one matches no row and is refused
// ErrNoPriority, the refusal an id no row carries takes — which is the 404 its own page, the
// listing and an issue's `advances` relation already answer for it. The guard stands here, in
// the one place a row is read for a write, so a rename, a rank move and a second delete are each
// refused by it; and it stands ahead of every append, so a refused write states nothing.
//
// anyRow is the restore's: SetState writing StateLive again reaches a gone row, since a guard
// that caught that one too would make a delete final to this store and not just to a caller.
const (
	liveRow = ` and state = '` + StateLive + `'`
	anyRow  = ``
)

// lockedNode reads one priority's node row and holds it `for no key update` until the
// transaction ends, the lock the store's own node write takes on a row it writes again
// (graphstore's queueNodes). Two writes of one priority therefore queue on that row, and the
// second reads it as the first committed it, its guard (rows) included. It answers ErrNoPriority
// where the store holds no such priority, or none that rows names.
func lockedNode(ctx context.Context, tx pgx.Tx, id, rows string) (Node, error) {
	node, err := scanNode(tx.QueryRow(ctx, `
		select `+nodeColumns+`
		from graph_nodes
		where kind = '`+KindPriority+`' and id = $1`+rows+`
		for no key update
	`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Node{}, fmt.Errorf("write priority %s: %w", id, ErrNoPriority)
	}
	if err != nil {
		return Node{}, fmt.Errorf("write priority %s: %w", id, err)
	}
	return node, nil
}

// attributeTuples states each attribute of one write, in the name order of the attributes, so a
// write of several states them in one order however the caller's map was built.
func attributeTuples(id string, values map[string]string, actor model.Actor, at time.Time) []Tuple {
	tuples := make([]Tuple, 0, len(values))
	for _, name := range slices.Sorted(maps.Keys(values)) {
		tuples = append(tuples, valueTuple(id, name, values[name], actor, at))
	}
	return tuples
}

// valueTuple is one attribute of one priority stated at one moment: the attribute is the tuple's
// kind, and the value itself is its far end.
func valueTuple(id, attribute, value string, actor model.Actor, at time.Time) Tuple {
	return Tuple{
		FromKind: KindPriority, FromID: id, Kind: attribute,
		ToKind: KindValue, ToID: value,
		At: at, Who: actor, SourceEvent: NoEvent, Asserted: true,
	}
}
