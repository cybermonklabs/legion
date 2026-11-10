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
func WriteArms(first int, nodes []Node, tuples []Tuple, claims []Claim) (string, []any, error) {
	relations, values := splitTuples(tuples)
	if len(nodes) == 0 && len(relations) == 0 && len(values) == 0 && len(claims) == 0 {
		return "", nil, nil
	}
	var arms []string
	var params []any
	// Each arm's parameters follow the ones before it, so an arm is numbered from the end of
	// what the arms before it bound.
	add := func(arm func(first int) string, args []any) {
		arms = append(arms, arm(first+len(params)))
		params = append(params, args...)
	}
	if len(nodes) > 0 {
		args, err := nodeParams(nodes)
		if err != nil {
			return "", nil, err
		}
		add(nodeArm, args)
	}
	if len(relations) > 0 {
		args, err := tupleParams(relations)
		if err != nil {
			return "", nil, err
		}
		add(func(at int) string {
			return `graph_relations_appended as (
	` + tupleInsert(at) + relationRepeatKey + `
)`
		}, args)
	}
	if len(values) > 0 {
		args, err := tupleParams(values)
		if err != nil {
			return "", nil, err
		}
		add(func(at int) string {
			return `graph_values_appended as (
	` + tupleInsert(at) + valueRepeatKey + `
)`
		}, args)
	}
	if len(claims) > 0 {
		args, err := claimParams(claims)
		if err != nil {
			return "", nil, err
		}
		add(claimArms, args)
	}
	return strings.Join(arms, ",\n"), params, nil
}

// nodeArm writes the nodes and states the two base attributes of each node it made. The node
// rows arrive in ascending (kind, id), as every write of this store takes them (writeNodes), and
// the upsert moves nothing on a node that is already there: a record of what a delivery reached
// learns nothing about a node another write made, and its own made_at, made_by and source_event
// would otherwise overwrite that write's. The base attributes are read off the rows the insert
// returned, so they are stated once, by the write that made the node, rather than per event.
//
// That is the rule Write follows too, and the arm needs no second half of it: this upsert moves
// no stored state, so there is never a state of a node already there for it to state.
func nodeArm(first int) string {
	return `graph_nodes_written as (
	insert into graph_nodes (` + nodeColumns + `)
	` + nodeRows(first) + `
	on conflict (kind, id) do nothing
	returning kind, id, visibility, state, made_at, made_by, source_event
),
graph_node_attributes as (
	insert into graph_tuples (` + tupleWriteColumns + `)
	select written.kind, written.id, attribute.kind, '` + KindValue + `', attribute.value,
	       written.made_at, written.made_by, written.source_event, null, true
	from graph_nodes_written written
	cross join lateral (values ('visibility', written.visibility), ('state', written.state))
		as attribute(kind, value)
	order by written.kind, written.id, attribute.kind` + valueRepeatKey + `
)`
}

// claimArms append what each claim changed. The far ends a node holds now are read the way
// CurrentOut reads them - the newest tuple per far end, kept while it asserts - so the read goes
// through graph_tuples_current, and the insert states a retraction of every far end the claim is
// no longer at before the assertion of the one it is at, in the order the caller gave its claims.
// A claim with no far end (ToID empty) retracts what the node holds and asserts nothing in its
// place, which is a role read unclaimed.
func claimArms(first int) string {
	return `graph_claims as (
	select * from unnest(` + claimArrays(first) + `)
		with ordinality as claim(from_kind, from_id, kind, to_kind, to_id, at, who, source_event, position)
),
graph_claims_held as (
	select claim.position, held.to_kind, held.to_id
	from graph_claims claim
	join lateral (
		select distinct on (held.to_kind, held.to_id) held.to_kind, held.to_id, held.asserted
		from graph_tuples held
		where held.from_kind = claim.from_kind and held.from_id = claim.from_id
			and held.kind = claim.kind and held.to_kind <> '` + KindValue + `'
		order by held.to_kind, held.to_id, held.seq desc
	) held on held.asserted
),
graph_claims_written as (
	insert into graph_tuples (` + tupleWriteColumns + `)
	select ` + tupleWriteColumns + ` from (
		select claim.from_kind, claim.from_id, claim.kind, held.to_kind, held.to_id, claim.at, claim.who,
		       claim.source_event, null::bigint as legacy_source_seq, false as asserted,
		       claim.position, 0 as step
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
	) stated (` + tupleWriteColumns + `, position, step)
	order by position, step` + relationRepeatKey + `
)`
}

// tupleWriteColumns is a tuple row as a writer states it: tupleColumns (read.go) without seq,
// the column the log gives itself. The node writers spell read.go's nodeColumns, the same list
// a node read answers.
const tupleWriteColumns = `from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted`

// The two repeat keys, named by their exact column list and predicate: a partial index is no
// constraint, so `on conflict on constraint` cannot name either of them. Both are partial on
// source_event <> 0, so a tuple at the sentinel is appended unconditionally.
const (
	relationRepeatKey = `
	on conflict (from_kind, from_id, kind, to_kind, to_id, source_event, asserted)
		where to_kind <> '` + KindValue + `' and source_event <> 0
	do nothing`
	valueRepeatKey = `
	on conflict (from_kind, from_id, kind, source_event, asserted)
		where to_kind = '` + KindValue + `' and source_event <> 0
	do nothing`
)

// nodeArrays are the nine arrays a node write binds, in nodeColumns' order: one spelling of the
// types, read by the arm the publisher's record splices in and by writeNodes alike.
func nodeArrays(first int) string {
	return arrayList(first, "text", "text", "text", "text", "text", "jsonb", "timestamptz", "jsonb", "bigint")
}

// nodeRows reads the nodes of a write in ascending (kind, id): a write touching several nodes
// takes its row locks in that order, so two such writes cannot each hold the row the other waits
// for (the lock order on api/inbox_read.go's markInboxReadThreads).
func nodeRows(first int) string {
	return `select ` + nodeColumns + `
	from unnest(` + nodeArrays(first) + `) as node(` + nodeColumns + `)
	order by kind, id`
}

// tupleInsert appends tuples in the order the caller stated them (with ordinality, order by
// position), so the sequence the log is read back in is the order a caller stated its facts.
func tupleInsert(first int) string {
	arrays := arrayList(first, "text", "text", "text", "text", "text", "timestamptz", "jsonb", "bigint", "bigint", "boolean")
	return `insert into graph_tuples (` + tupleWriteColumns + `)
	select ` + tupleWriteColumns + `
	from unnest(` + arrays + `)
		with ordinality as tuple(` + tupleWriteColumns + `, position)
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
// each is appended by a statement, or an arm, of its own.
func splitTuples(tuples []Tuple) (relations, values []Tuple) {
	for _, tuple := range tuples {
		if tuple.ToKind == KindValue {
			values = append(values, tuple)
		} else {
			relations = append(relations, tuple)
		}
	}
	return relations, values
}

// nodeParams are the nine arrays a node write binds, in nodeColumns' order.
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
	}
	return []any{kinds, ids, supertags, visibilities, states, attributes, madeAts, madeBys, sourceEvents}, nil
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
