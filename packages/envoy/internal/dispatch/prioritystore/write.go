package prioritystore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Write writes every node and appends every tuple inside the caller's transaction. It appends
// only: no statement here updates or deletes a tuple row, so the log a read walks is whatever
// every writer before it appended, in the order they appended it. The node row is written again
// where it is already there, because it is the projection of its own tuples; MadeAt and MadeBy
// stay as the row's first write left them.
//
// It is the primitive the named writes below are built from, and what a caller holding a whole
// batch at once reaches for: the seeded corpus states its `advances` relations through it, one
// statement a batch rather than one a relation (cmd/dispatch-seed/write_priorities.go). Either
// list may be empty, so a caller with tuples and no node passes none.
func Write(ctx context.Context, tx pgx.Tx, nodes []Node, tuples []Tuple) error {
	if err := writeNodes(ctx, tx, nodes); err != nil {
		return err
	}
	return appendTuples(ctx, tx, tuples)
}

// Create writes one priority and states what it was written with: its visibility, its state, and
// each attribute values names, every one of them in the caller's transaction. The id is the
// caller's, as an issue key is: nothing here makes one.
//
// It refuses an id the store already holds with ErrPriorityExists rather than writing the node
// again, because a second create is a caller's mistake and the first write's own history would
// otherwise read as this one's.
func Create(ctx context.Context, tx pgx.Tx, id string, values map[string]string, actor model.Actor, at time.Time) error {
	attributes, err := json.Marshal(values)
	if err != nil {
		return fmt.Errorf("encode the attributes of priority %s: %w", id, err)
	}
	writer, err := json.Marshal(actor)
	if err != nil {
		return fmt.Errorf("encode the writer of priority %s: %w", id, err)
	}
	var written string
	err = tx.QueryRow(ctx, `
		insert into priority_nodes (`+nodeColumns+`)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		on conflict (kind, id) do nothing
		returning id
	`, KindPriority, id, KindPriority, VisibilityProject, StateLive, attributes, at, writer, NoEvent).Scan(&written)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("write priority %s: %w", id, ErrPriorityExists)
	}
	if err != nil {
		return fmt.Errorf("write priority %s: %w", id, err)
	}
	tuples := make([]Tuple, 0, 2+len(values))
	tuples = append(tuples,
		valueTuple(id, AttributeVisibility, VisibilityProject, actor, at),
		valueTuple(id, AttributeState, StateLive, actor, at))
	return appendTuples(ctx, tx, append(tuples, attributeTuples(id, values, actor, at)...))
}

// SetAttributes moves the node row's attributes and appends one value tuple per attribute, in the
// caller's one transaction: the row answers the next read and the tuples answer the history. The
// attributes values does not name are left as they were, so a write of one attribute states one
// fact. It refuses an id the store does not hold with ErrNoPriority, and states nothing for an
// empty values.
func SetAttributes(ctx context.Context, tx pgx.Tx, id string, values map[string]string, actor model.Actor, at time.Time) error {
	if len(values) == 0 {
		return nil
	}
	attributes, err := json.Marshal(values)
	if err != nil {
		return fmt.Errorf("encode the attributes of priority %s: %w", id, err)
	}
	if err := moveNode(ctx, tx, `attributes = priority_nodes.attributes || $2::jsonb`, id, attributes); err != nil {
		return err
	}
	return appendTuples(ctx, tx, attributeTuples(id, values, actor, at))
}

// SetState moves the node row's state and appends the tuple that states it. Deleting a priority
// is StateGone and restoring it is StateLive: no row is removed, and the history holds who ended
// it and when.
func SetState(ctx context.Context, tx pgx.Tx, id, state string, actor model.Actor, at time.Time) error {
	if err := moveNode(ctx, tx, `state = $2`, id, state); err != nil {
		return err
	}
	return appendTuples(ctx, tx, []Tuple{valueTuple(id, AttributeState, state, actor, at)})
}

// Advance states that one issue advances one priority. The relation is stated on the issue, so
// its far end is the priority and a read of either end answers it (Timeline reads the ends into
// a priority; api/issue_queries.go reads the ends out of a page of issues). It appends a tuple
// and nothing else: that the priority is live and that the issue exists are the caller's
// refusals, since this store holds no issue row.
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
	return appendTuples(ctx, tx, []Tuple{{
		FromKind: KindIssue, FromID: issueKey, Kind: RelationAdvances,
		ToKind: KindPriority, ToID: id,
		At: at, Who: actor, SourceEvent: NoEvent, Asserted: asserted,
	}})
}

// moveNode writes one column of one node row and answers ErrNoPriority where the store holds no
// such priority. It is the one place a row is updated: a tuple never is.
func moveNode(ctx context.Context, tx pgx.Tx, assignment, id string, value any) error {
	var written string
	err := tx.QueryRow(ctx, `
		update priority_nodes set `+assignment+`
		where kind = '`+KindPriority+`' and id = $1
		returning id
	`, id, value).Scan(&written)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("write priority %s: %w", id, ErrNoPriority)
	}
	if err != nil {
		return fmt.Errorf("write priority %s: %w", id, err)
	}
	return nil
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

// writeNodes is one statement, whose rows arrive in ascending (kind, id): a write touching
// several nodes takes its row locks in that order, so two such writes cannot each hold the row
// the other waits for. The upsert moves supertag, visibility, state, attributes and source_event,
// and leaves made_at and made_by as the row's own first write left them.
func writeNodes(ctx context.Context, tx pgx.Tx, nodes []Node) error {
	if len(nodes) == 0 {
		return nil
	}
	args, err := nodeParams(nodes)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		insert into priority_nodes (`+nodeColumns+`)
		select * from unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[],
			$6::jsonb[], $7::timestamptz[], $8::jsonb[], $9::bigint[])
		order by 1, 2
		on conflict (kind, id) do update set
			supertag = excluded.supertag,
			visibility = excluded.visibility,
			state = excluded.state,
			attributes = excluded.attributes,
			source_event = excluded.source_event
	`, args...); err != nil {
		return fmt.Errorf("write priority nodes: %w", err)
	}
	return nil
}

// appendTuples appends every tuple of a write in one statement, in the order the caller handed
// them over, which is the order their seq is drawn in and so the order a history reads them back.
func appendTuples(ctx context.Context, tx pgx.Tx, tuples []Tuple) error {
	if len(tuples) == 0 {
		return nil
	}
	args, err := tupleParams(tuples)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		insert into priority_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		select * from unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[],
			$6::timestamptz[], $7::jsonb[], $8::bigint[], $9::boolean[])
	`, args...); err != nil {
		return fmt.Errorf("append priority tuples: %w", err)
	}
	return nil
}

// nodeParams is the node write's nine arrays, one per column, in nodeColumns' order.
func nodeParams(nodes []Node) ([]any, error) {
	kinds := make([]string, len(nodes))
	ids := make([]string, len(nodes))
	supertags := make([]string, len(nodes))
	visibilities := make([]string, len(nodes))
	states := make([]string, len(nodes))
	attributes := make([][]byte, len(nodes))
	madeAt := make([]time.Time, len(nodes))
	madeBy := make([][]byte, len(nodes))
	events := make([]int64, len(nodes))
	for index, node := range nodes {
		kinds[index] = node.Kind
		ids[index] = node.ID
		supertags[index] = node.Supertag
		visibilities[index] = node.Visibility
		states[index] = node.State
		encoded, err := encodeAttributes(node.Attributes)
		if err != nil {
			return nil, fmt.Errorf("encode the attributes of priority %s: %w", node.ID, err)
		}
		attributes[index] = encoded
		madeAt[index] = node.MadeAt
		writer, err := json.Marshal(node.MadeBy)
		if err != nil {
			return nil, fmt.Errorf("encode the writer of priority %s: %w", node.ID, err)
		}
		madeBy[index] = writer
		events[index] = node.SourceEvent
	}
	return []any{kinds, ids, supertags, visibilities, states, attributes, madeAt, madeBy, events}, nil
}

// tupleParams is the tuple append's nine arrays, one per column.
func tupleParams(tuples []Tuple) ([]any, error) {
	fromKinds := make([]string, len(tuples))
	fromIDs := make([]string, len(tuples))
	kinds := make([]string, len(tuples))
	toKinds := make([]string, len(tuples))
	toIDs := make([]string, len(tuples))
	at := make([]time.Time, len(tuples))
	who := make([][]byte, len(tuples))
	events := make([]int64, len(tuples))
	asserted := make([]bool, len(tuples))
	for index, tuple := range tuples {
		fromKinds[index] = tuple.FromKind
		fromIDs[index] = tuple.FromID
		kinds[index] = tuple.Kind
		toKinds[index] = tuple.ToKind
		toIDs[index] = tuple.ToID
		at[index] = tuple.At
		writer, err := json.Marshal(tuple.Who)
		if err != nil {
			return nil, fmt.Errorf("encode the writer of a %s tuple on %s: %w", tuple.Kind, tuple.FromID, err)
		}
		who[index] = writer
		events[index] = tuple.SourceEvent
		asserted[index] = tuple.Asserted
	}
	return []any{fromKinds, fromIDs, kinds, toKinds, toIDs, at, who, events, asserted}, nil
}

// encodeAttributes writes an empty object for a node that carries no attribute, since the column
// is `not null` and a nil map encodes as the JSON null, which is a value rather than no value.
func encodeAttributes(attributes map[string]any) ([]byte, error) {
	if attributes == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(attributes)
}
