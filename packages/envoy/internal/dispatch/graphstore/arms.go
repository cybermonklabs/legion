package graphstore

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Claim is one single-valued relation as its writer reads it now: the far end FromKind/FromID
// holds under Kind, or none at all when ToID is empty. The store keeps appending only, so a
// claim is written as the facts it changes - a retraction of every far end the node holds under
// that kind and is no longer at, and an assertion of the new one where the node is not at it
// already - and a claim that restates what the store already holds writes nothing. That is what
// a role's holder needs: the publisher resolves it on every batch, and a held_by tuple appended
// per batch would be the same fact stated thousands of times, while a role read unclaimed has to
// end the relation rather than leave it standing.
//
// One claim per (FromKind, FromID, Kind) in a call: two are the caller's mistake, and both read
// the store as it was before the statement, so both would be asserted.
type Claim struct {
	FromKind, FromID string
	Kind             string
	ToKind, ToID     string
	At               time.Time
	Who              model.Actor
	SourceEvent      int64
}

// WriteArms is this store's write as arms of a statement the caller owns, for the writer that
// cannot hand its work to Write: outbox.recordBatch writes what a batch of events came to on a
// pooled connection with no transaction of its own, and is retried, so what that record states
// about the store has to commit with the record and be safe to state again. The arms are text
// rather than a run statement so the caller keeps its one statement, and they are built here
// rather than written out there so this package stays the only place in the tree that writes
// graph_nodes and graph_tuples.
//
// The caller splices sql into its own `with` list: it is a comma-separated list of
// data-modifying common-table expressions, each of which Postgres runs once and to completion
// whether or not the statement reads it, and it ends with no comma. Its parameters are bound
// from $first on, in the order params answers them, after the caller's own. Nothing to write is
// the empty string and no parameter, which leaves the caller's statement exactly as it was.
//
// Each arm is repeat-safe on its own: the nodes are upserted `do nothing`, since a node another
// write already made is that write's to describe, and the tuples take the two partial repeat
// keys (20261109013400), under which a second run of one event's record appends nothing.
//
// The arms keep the store's lock order: the node rows are written first, in ascending (kind, id),
// and every tuple after them, because each tuple arm of a write that has nodes reads the node
// arm before its first row (nodesWrittenFirst).
//
// A node's or a tuple's SourceEvent may be LatestEvent, for a record whose event its write phase
// appends right before this statement: every cause column is read through causeOf.
func WriteArms(first int, nodes []Node, tuples []Tuple, claims []Claim) (string, []any, error) {
	if err := refusePersonTuples(tuples, claims); err != nil {
		return "", nil, err
	}
	sql, params, _, err := writeArms(first, nodes, tuples, claims, true)
	return sql, params, err
}

// WriteArmsUncaused is WriteArms for a writer whose cause is appended after these arms run: the
// nodes it makes state no base attribute here and are made at the sentinel, marked as made
// (nodeMark), and the write it answers holds them for that event, which QueueCaused moves them
// onto and appends their base attributes under, read off that mark. No tuple may be updated to
// learn its cause, and the node row moves onto the event, so a node made at the sentinel whose
// base attributes were stated here would be a row naming one event and base tuples naming
// another. A node the arm found there is another write's and is neither marked nor caused.
func WriteArmsUncaused(first int, nodes []Node, tuples []Tuple, claims []Claim) (string, []any, Uncaused, error) {
	if err := refusePersonTuples(tuples, claims); err != nil {
		return "", nil, Uncaused{}, err
	}
	return writeArms(first, nodes, tuples, claims, false)
}

// writeArms is both: states says whether the node arm states the base attributes of each node
// it makes, or marks it for the cause that comes later.
func writeArms(first int, nodes []Node, tuples []Tuple, claims []Claim, states bool) (string, []any, Uncaused, error) {
	relations, values := splitTuples(tuples)
	if len(nodes) == 0 && len(relations) == 0 && len(values) == 0 && len(claims) == 0 {
		return "", nil, Uncaused{}, nil
	}
	var arms []string
	var params []any
	var held Uncaused
	// Each arm's parameters follow the ones before it, so an arm is numbered from the end of
	// what the arms before it bound.
	add := func(arm func(first int) string, args []any) {
		arms = append(arms, arm(first+len(params)))
		params = append(params, args...)
	}
	// The condition every tuple arm reads its rows under: none for a write with no node, and
	// nodesWrittenFirst for one with nodes.
	after := ""
	if len(nodes) > 0 {
		args, err := nodeParams(nodes)
		if err != nil {
			return "", nil, Uncaused{}, err
		}
		mark := int64(0)
		if !states {
			mark = nextNodeMark()
			args = append(args, mark)
			uncaused := make([]Node, len(nodes))
			for index, node := range nodes {
				node.SourceEvent = NoEvent
				uncaused[index] = node
			}
			held = Uncaused{Nodes: uncaused, mark: mark}
		}
		add(func(at int) string { return nodeArm(at, states) }, args)
		after = nodesWrittenFirst
	}
	if len(relations) > 0 {
		args, err := tupleParams(relations)
		if err != nil {
			return "", nil, Uncaused{}, err
		}
		add(func(at int) string {
			return `graph_relations_appended as (
	` + tupleInsert(at, after) + relationRepeatKey + `
)`
		}, args)
	}
	if len(values) > 0 {
		args, err := tupleParams(values)
		if err != nil {
			return "", nil, Uncaused{}, err
		}
		add(func(at int) string {
			return `graph_values_appended as (
	` + tupleInsert(at, after) + valueRepeatKey + `
)`
		}, args)
	}
	if len(claims) > 0 {
		args, err := claimParams(claims)
		if err != nil {
			return "", nil, Uncaused{}, err
		}
		add(func(at int) string { return claimArms(at, after) }, args)
	}
	return strings.Join(arms, ",\n"), params, held, nil
}

// nodesWrittenFirst is the condition every tuple arm of a write that has nodes reads its rows
// under. Postgres runs a data-modifying arm the statement does not read after the statement's
// own query, the later arms of the with list first, so a tuple arm that read nothing of the node
// arm would append its tuples before the nodes they are about: outside the lock order on
// api/inbox_read.go's markInboxReadThreads, and, on a repeat key another transaction holds, a
// wait that holds none of the node rows yet. The count is true whatever the node arm wrote, and
// reading it runs that arm to its end first, so the nodes are written, and their rows locked,
// before the first tuple is. It is a filter rather than a join because a node arm that wrote no
// row - every node was already there - must still let every tuple through.
const nodesWrittenFirst = `(select count(*) from graph_nodes_written) >= 0`

// nodeArm writes the nodes and, where states says so, the base attributes of each node it made.
// The node rows arrive in ascending (kind, id), as every write of this store takes them
// (queueNodes), and the upsert moves nothing on a node that is already there: a record of what
// a delivery reached learns nothing about a node another write made, and its own made_at,
// made_by and source_event would otherwise overwrite that write's. The base attributes are read
// off the rows the insert returned, so they are stated once, by the write that made the node,
// rather than per event: its visibility and state always, and its project and parties where it
// carries them.
//
// That is the rule QueueWrite follows too, and the arm needs no second half of it: this upsert
// moves no stored state, so there is never a state of a node already there for it to state.
//
// states is false for a writer whose cause is appended after these arms (WriteArmsUncaused): the
// nodes it makes carry the mark bound after the node arrays instead of a cause (nodeMark), as
// made, so the append that cause comes with states those same attributes for that event.
func nodeArm(first int, states bool) string {
	source := causeOf("source_event")
	if !states {
		source = markSQL("$"+strconv.Itoa(first+len(nodeArrayTypes)), markMade)
	}
	written := `graph_nodes_written as (
	insert into graph_nodes (` + nodeColumns + `)
	select kind, id, supertag, visibility, state, attributes, made_at, made_by, ` + source + `,
	       project_key, string_to_array(parties, ' ')
	from unnest(` + nodeArrays(first) + `) as node(` + nodeColumns + `)
	order by kind, id
	on conflict (kind, id) do nothing
	returning kind, id, visibility, state, project_key, parties, made_at, made_by, source_event
)`
	if !states {
		return written
	}
	return written + `,
graph_node_attributes as (
	insert into graph_tuples (` + tupleWriteColumns + `)
	select written.kind, written.id, attribute.kind, '` + KindValue + `', attribute.value,
	       written.made_at, written.made_by, written.source_event, null, true
	from graph_nodes_written written
	cross join lateral (values
		('` + AttributeVisibility + `', written.visibility), ('` + AttributeState + `', written.state),
		('` + AttributeProject + `', written.project_key), ('` + AttributeParties + `', array_to_string(written.parties, ' ')))
		as attribute(kind, value)
	where attribute.value is not null
	order by written.kind, written.id, attribute.kind` + valueRepeatKey + `
)`
}

// claimArms append what each claim changed. The far ends a node holds now are read the way
// CurrentOut reads them (CurrentTupleRead), through graph_tuples_as_of_out: the kind each read
// binds is the claim row's, which proves nothing of graph_tuples_current's predicate, so the read
// takes the node's tuples of that kind and sorts them. The insert states a retraction of every
// far end the claim is no longer at before the assertion of the one it is at, in the order the
// caller gave its claims.
// A claim with no far end (ToID empty) retracts what the node holds and asserts nothing in its
// place, which is a role read unclaimed. where is the condition its insert reads its rows under,
// as tupleInsert takes it.
//
// A claim is stated whole or not at all. Its rows carry its event, and the repeat key admits one
// row per (far end, event, asserted) (D5), so a claim whose event the log already holds one of
// those rows under is that event stated again: the publisher dates a role's holder by the event
// that resolved it, and an event retried after a later one resolved the role again reads the
// store as the later event left it. Appended row by row, such a claim would lose its assertion to
// the repeat key and keep its retraction - event 10 resolves s1, event 11 resolves s2, the retry
// of event 10 resolves s1 again, and nobody holds the role. So a claim any row of which its event
// has already stated appends no row at all, and the node keeps the far end the later write left
// it at until a newer event resolves it again. A claim at the sentinel is outside the repeat key
// and is always stated.
func claimArms(first int, where string) string {
	if where != "" {
		where = "\n\t  and " + where
	}
	return `graph_claims as (
	select claim.from_kind, claim.from_id, claim.kind, claim.to_kind, claim.to_id, claim.at, claim.who,
	       ` + causeOf("claim.source_event") + ` as source_event, claim.position
	from unnest(` + claimArrays(first) + `)
		with ordinality as claim(from_kind, from_id, kind, to_kind, to_id, at, who, source_event, position)
),
graph_claims_held as (
	select claim.position, held.to_kind, held.to_id
	from graph_claims claim
	join lateral (
		` + CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = claim.from_kind and t.from_id = claim.from_id
			and t.kind = claim.kind and t.to_kind <> '`+KindValue+`'`) + `
	) held on held.asserted
),
graph_claims_stated (` + tupleWriteColumns + `, position, step) as (
	select claim.from_kind, claim.from_id, claim.kind, held.to_kind, held.to_id, claim.at, claim.who,
	       claim.source_event, null::bigint, false, claim.position, 0
	from graph_claims claim
	join graph_claims_held held on held.position = claim.position
	where (held.to_kind, held.to_id) <> (claim.to_kind, claim.to_id)
	union all
	select claim.from_kind, claim.from_id, claim.kind, claim.to_kind, claim.to_id, claim.at, claim.who,
	       claim.source_event, null::bigint, true, claim.position, 1
	from graph_claims claim
	where claim.to_id <> '' and not exists (
		select 1 from graph_claims_held held
		where held.position = claim.position
			and held.to_kind = claim.to_kind and held.to_id = claim.to_id
	)
),
graph_claims_written as (
	insert into graph_tuples (` + tupleWriteColumns + `)
	select ` + tupleWriteColumns + ` from graph_claims_stated stated
	where not exists (
		select 1 from graph_claims_stated restated
		join graph_tuples once
			on once.from_kind = restated.from_kind and once.from_id = restated.from_id
			and once.kind = restated.kind and once.to_kind = restated.to_kind and once.to_id = restated.to_id
			and once.source_event = restated.source_event and once.asserted = restated.asserted
			and once.to_kind <> '` + KindValue + `' and once.source_event <> 0
		where restated.position = stated.position
	)` + where + `
	order by position, step` + relationRepeatKey + `
)`
}

// tupleWriteColumns is a tuple row as a writer states it: tupleColumns (read.go) without seq,
// the column the log gives itself. The node writers spell read.go's nodeColumns, the same list
// a node read answers.
const tupleWriteColumns = `from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted`

// The two repeat keys, named by their exact column list and predicate: a partial index is no
// constraint, so `on conflict on constraint` cannot name either of them. Both are partial on
// source_event <> 0, so a tuple at the sentinel is appended unconditionally. Both carry the far
// end, so one event may state two far ends under one kind: the value key carries md5(to_id)
// rather than the value itself, which has no length bound a btree key would take
// (20261109013400_graph_store.up.sql), and an inference clause names that expression as the
// index declares it.
const (
	relationRepeatKey = `
	on conflict (from_kind, from_id, kind, to_kind, to_id, source_event, asserted)
		where to_kind <> '` + KindValue + `' and source_event <> 0
	do nothing`
	valueRepeatKey = `
	on conflict (from_kind, from_id, kind, md5(to_id), source_event, asserted)
		where to_kind = '` + KindValue + `' and source_event <> 0
	do nothing`
)

// nodeArrayTypes are the types of the eleven arrays a node write binds, in nodeColumns' order,
// and nodeArrays their parameter list: one spelling of the types, read by the arm the publisher's
// record splices in and by queueNodes alike. The project and the parties are bound as text, null
// for a node that carries none, and the parties as the one value their attribute carries
// (PartiesValue), which a node write splits into the column's array (string_to_array).
var nodeArrayTypes = []string{"text", "text", "text", "text", "text", "jsonb", "timestamptz", "jsonb", "bigint", "text", "text"}

func nodeArrays(first int) string {
	return arrayList(first, nodeArrayTypes...)
}

// tupleInsert appends tuples in the order the caller stated them (with ordinality, order by
// position), so the sequence the log is read back in is the order a caller stated its facts.
// where is a condition the rows are read under, or empty for none: a tuple arm of WriteArms
// passes nodesWrittenFirst.
func tupleInsert(first int, where string) string {
	if where != "" {
		where = "\n\twhere " + where
	}
	return `insert into graph_tuples (` + tupleWriteColumns + `)
	select ` + tupleSelect + `
	from unnest(` + tupleArrays(first) + `)
		with ordinality as tuple(` + tupleWriteColumns + `, position)` + where + `
	order by position`
}

// claimArrays are the eight arrays a claim arm takes, in claimParams' order.
func claimArrays(first int) string {
	return arrayList(first, "text", "text", "text", "text", "text", "timestamptz", "jsonb", "bigint")
}

// arrayList writes the parameter list of an unnest: one placeholder per type, numbered from
// first, each cast to its own array type.
func arrayList(first int, types ...string) string {
	cast := make([]string, len(types))
	for index, kind := range types {
		cast[index] = "$" + strconv.Itoa(first+index) + "::" + kind + "[]"
	}
	return strings.Join(cast, ", ")
}

// splitTuples parts the relations from the attributes: the two carry different repeat keys, so
// each is appended by a statement, or an arm, of its own. It counts the attributes before it
// copies, so each of the two slices is made once at the size it ends at: a first index of a
// document of thousands of blocks hands this thousands of tuples, and a slice grown from empty
// over them is allocated again and again on the way.
func splitTuples(tuples []Tuple) (relations, values []Tuple) {
	attributes := 0
	for _, tuple := range tuples {
		if tuple.ToKind == KindValue {
			attributes++
		}
	}
	if attributes < len(tuples) {
		relations = make([]Tuple, 0, len(tuples)-attributes)
	}
	if attributes > 0 {
		values = make([]Tuple, 0, attributes)
	}
	for _, tuple := range tuples {
		if tuple.ToKind == KindValue {
			values = append(values, tuple)
		} else {
			relations = append(relations, tuple)
		}
	}
	return relations, values
}

// nodeParams are the eleven arrays a node write binds, in nodeColumns' order.
func nodeParams(nodes []Node) ([]any, error) {
	kinds := make([]string, len(nodes))
	ids := make([]string, len(nodes))
	supertags := make([]string, len(nodes))
	visibilities := make([]string, len(nodes))
	states := make([]string, len(nodes))
	attributes := make([][]byte, len(nodes))
	madeAts := make([]time.Time, len(nodes))
	madeBys := make([][]byte, len(nodes))
	sourceEvents := make([]int64, len(nodes))
	projects := make([]*string, len(nodes))
	parties := make([]*string, len(nodes))
	for index, node := range nodes {
		encoded, err := encodeAttributes(node.Attributes)
		if err != nil {
			return nil, fmt.Errorf("encode attributes of node %s/%s: %w", node.Kind, node.ID, err)
		}
		// Recorded drops the request's credential, which no row ever holds.
		madeBy, err := json.Marshal(node.MadeBy.Recorded())
		if err != nil {
			return nil, fmt.Errorf("encode writer of node %s/%s: %w", node.Kind, node.ID, err)
		}
		kinds[index], ids[index] = node.Kind, node.ID
		supertags[index], visibilities[index], states[index] = node.Supertag, node.Visibility, node.State
		attributes[index], madeAts[index], madeBys[index] = encoded, node.MadeAt, madeBy
		sourceEvents[index] = node.SourceEvent
		if node.Project != "" {
			projects[index] = &nodes[index].Project
		}
		value, err := PartiesValue(node.Parties)
		if err != nil {
			return nil, fmt.Errorf("node %s/%s: %w", node.Kind, node.ID, err)
		}
		if value != "" {
			parties[index] = &value
		}
	}
	return []any{kinds, ids, supertags, visibilities, states, attributes, madeAts, madeBys, sourceEvents, projects, parties}, nil
}

// tupleParams are the ten arrays a tuple append binds, in tupleWriteColumns' order.
func tupleParams(tuples []Tuple) ([]any, error) {
	fromKinds := make([]string, len(tuples))
	fromIDs := make([]string, len(tuples))
	kinds := make([]string, len(tuples))
	toKinds := make([]string, len(tuples))
	toIDs := make([]string, len(tuples))
	ats := make([]time.Time, len(tuples))
	whos := make([][]byte, len(tuples))
	sourceEvents := make([]int64, len(tuples))
	legacySeqs := make([]*int64, len(tuples))
	asserted := make([]bool, len(tuples))
	for index, tuple := range tuples {
		who, err := json.Marshal(tuple.Who.Recorded())
		if err != nil {
			return nil, fmt.Errorf("encode writer of %s %s: %w", tuple.Kind, tuple.FromID, err)
		}
		fromKinds[index], fromIDs[index], kinds[index] = tuple.FromKind, tuple.FromID, tuple.Kind
		toKinds[index], toIDs[index], ats[index] = tuple.ToKind, tuple.ToID, tuple.At
		whos[index], sourceEvents[index] = who, tuple.SourceEvent
		legacySeqs[index], asserted[index] = tuple.LegacySourceSeq, tuple.Asserted
	}
	return []any{fromKinds, fromIDs, kinds, toKinds, toIDs, ats, whos, sourceEvents, legacySeqs, asserted}, nil
}

// claimParams are the eight arrays a claim arm binds, in the order claimArrays casts them.
func claimParams(claims []Claim) ([]any, error) {
	fromKinds := make([]string, len(claims))
	fromIDs := make([]string, len(claims))
	kinds := make([]string, len(claims))
	toKinds := make([]string, len(claims))
	toIDs := make([]string, len(claims))
	ats := make([]time.Time, len(claims))
	whos := make([][]byte, len(claims))
	sourceEvents := make([]int64, len(claims))
	for index, claim := range claims {
		who, err := json.Marshal(claim.Who.Recorded())
		if err != nil {
			return nil, fmt.Errorf("encode writer of %s %s: %w", claim.Kind, claim.FromID, err)
		}
		fromKinds[index], fromIDs[index], kinds[index] = claim.FromKind, claim.FromID, claim.Kind
		toKinds[index], toIDs[index], ats[index] = claim.ToKind, claim.ToID, claim.At
		whos[index], sourceEvents[index] = who, claim.SourceEvent
	}
	return []any{fromKinds, fromIDs, kinds, toKinds, toIDs, ats, whos, sourceEvents}, nil
}
