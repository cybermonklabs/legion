package refs

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// BacklinkCounts is how many edges point at each of ids, in one grouped query: the number a
// list carries beside a row, counted the same way the rows behind it are listed. It unions only
// the arms of graph_edges that can point at a node of toKind - the six mention arms every kind
// has and the structural arms declared for that kind (backlinkArms) - and counts them in one
// statement, each arm read through its own index, rather than leaving the planner the whole
// 26-arm view on every call. The counts are the view's, arm for arm. A kind with no entry in the
// arm table is refused rather than counted from the mention arms alone, so a count is never
// silently low.
//
// It counts only edges whose source is a node kind this build reads, so the count is the rows its
// list shows, and counts a document's, comment's, message's or ask's mention once for each of its
// blocks' links to the node, in place of the item's own edge (heldByABlock): a comment citing a
// node from two paragraphs counts 2, and a document's block pointing at the node counts once
// whatever its link's kind. An edge at a current block of a counted item is counted as that
// item's, since the item's own read lists it (readEdgeRows): a comment citing one part of an ask
// counts once for that ask. A node with no inbound edge is absent from the result; the caller
// decides what zero means.
//
// It is the sequential half of the one count: QueueBacklinkCounts queues the same statement and
// reads it with the same scan, for a caller that counts beside its other reads (the issue read's
// own count, and api.queueAskDetails for every list of asks), so the two paths cannot drift.
func BacklinkCounts(
	ctx context.Context,
	q Queryer,
	who model.Actor,
	toKind string,
	ids []string,
	excludeKinds []string,
) (map[string]int, error) {
	counts := make(map[string]int, len(ids))
	if len(ids) == 0 {
		return counts, nil
	}
	statement, args, err := backlinkCountStatement(who, toKind, ids, excludeKinds)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("count %s backlinks: %w", toKind, err)
	}
	defer rows.Close()
	if err := scanBacklinkCounts(rows, toKind, counts); err != nil {
		return nil, err
	}
	return counts, nil
}

// QueueBacklinkCounts is the batched half: it queues BacklinkCounts's statement in b and reads its
// rows into counts, for a caller that counts beside its other reads instead of spending a round
// trip on it (the issue read's own count, api.getIssue, and api.queueAskDetails). No id queues
// nothing and leaves counts as it is, so a caller need not ask whether it has any. A kind the arm
// table does not declare is refused here as BacklinkCounts refuses it, before anything is queued.
func QueueBacklinkCounts(b *pgx.Batch, who model.Actor, toKind string, ids, excludeKinds []string, counts map[string]int) error {
	if len(ids) == 0 {
		return nil
	}
	statement, args, err := backlinkCountStatement(who, toKind, ids, excludeKinds)
	if err != nil {
		return err
	}
	b.Queue(statement, args...).Query(func(rows pgx.Rows) error {
		return scanBacklinkCounts(rows, toKind, counts)
	})
	return nil
}

// backlinkCountStatement is the statement and arguments both halves run. A kind that is no key of
// the arm table is refused before any SQL is built: the count has no arms to read for it, and
// would otherwise be silently low.
//
// The acting credential is bound as $3, which every arm's visibility predicate names: an edge
// whose source who may not see raises no count, by the same test the list of those edges binds
// (readEdgeRows), so a count and its list cannot disagree.
//
// A count of one address binds that address itself and not an array of one, which is the
// statement every page read of a single node runs (the issue read's own count). An array tells
// the planner nothing about how many addresses it holds, so it costs the generic plan at ten
// of them and keeps choosing a custom plan instead: measured on this pod over the read-cost
// corpus, this child's issue count planned itself again on every execution, 4.1 ms of planning
// against 0.7 ms of execution, and spent 10.8 ms on the sixth execution of each connection
// building the generic plan it then threw away. With the one address bound as itself the
// generic plan is the cheaper one, Postgres keeps it from that sixth execution on, and planning
// falls to 0.06 ms a read.
func backlinkCountStatement(who model.Actor, toKind string, ids, excludeKinds []string) (string, []any, error) {
	one := len(ids) == 1
	statement, ok := backlinkCountSQL(toKind, excludeKinds, one)
	if !ok {
		return "", nil, fmt.Errorf("count %s backlinks: no arms declared for that node kind", toKind)
	}
	var addresses any = ids
	if one {
		addresses = ids[0]
	}
	args := []any{addresses, nodeKinds, VisibilityArg(who)}
	if len(excludeKinds) > 0 {
		args = append(args, excludeKinds)
	}
	return statement, args, nil
}

// scanBacklinkCounts reads the rows the count statement returns into counts, which a node with no
// inbound edge stays absent from. A batch can hand it rows that already hold an error - one the
// sequential Query would have returned itself - and that error is wrapped here as BacklinkCounts
// wraps its Query's; an error that arrives while the rows are read is the last rows.Err(),
// unwrapped on both paths.
func scanBacklinkCounts(rows pgx.Rows, toKind string, counts map[string]int) error {
	if err := rows.Err(); err != nil {
		return fmt.Errorf("count %s backlinks: %w", toKind, err)
	}
	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return fmt.Errorf("scan %s backlink count: %w", toKind, err)
		}
		counts[id] = count
	}
	return rows.Err()
}

// backlinkCountSQL is the statement BacklinkCounts runs: the union of the arms of graph_edges
// that can point at a node of toKind, grouped by target, with the arms a caller's excludeKinds
// rule out left out. It binds the addresses as $1 - one address as itself and two or more as an
// array, which is what one says (backlinkCountStatement) - the node kinds this build reads as
// $2, the acting credential as $3 (backlinkSeen) and, when excludeKinds is not empty, those
// kinds as $4. ok is false for a kind backlinkArms does not declare, the one kind
// BacklinkCounts refuses; it is a builder of its own so a test can read the statement
// BacklinkCounts runs and hold its plan to an index on every arm.
//
// The kind is written into each arm as a literal rather than a parameter because the statement
// text already differs per kind, and a literal lets the planner know how selective each arm's
// to_kind predicate is; only a kind backlinkArms declares reaches that, so nothing a caller names
// is ever written into a statement.
func backlinkCountSQL(toKind string, excludeKinds []string, one bool) (string, bool) {
	arms, ok := backlinkTargetArms(backlinkTarget{kind: toKind, one: one}, excludeKinds)
	if !ok {
		return "", false
	}
	// An ask, a comment and a message are each read with the edges at their current blocks, so
	// the count adds the arms that point at a block of the counted item, each counted under the
	// item that holds it. A block kind whose arms are not declared would count those edges low,
	// so it is refused for the same reason the counted kind's own are.
	if item := ItemKind(toKind); slices.Contains(itemKinds, item) {
		tables, _ := item.tables()
		target := backlinkTarget{kind: item.BlockKind(), blocks: tables.blocks, item: tables.item, one: one}
		atBlocks, ok := backlinkTargetArms(target, excludeKinds)
		if !ok {
			return "", false
		}
		arms = append(arms, atBlocks...)
	}
	return `select to_id, count(*)::int from (` + strings.Join(arms, " union all ") + `) edge (to_id) group by to_id`, true
}

// backlinkTargetArms is every arm of graph_edges that can point at target: the mention arms
// every node kind has and the structural arms declared for that kind, with the arms a caller's
// excludeKinds rule out left out. ok is false for a kind backlinkArms does not declare.
func backlinkTargetArms(target backlinkTarget, excludeKinds []string) ([]string, bool) {
	structural, ok := backlinkArms[target.kind]
	if !ok {
		return nil, false
	}
	arms := mentionBacklinkArms(target, excludeKinds)
	for _, arm := range structural {
		// A structural arm emits one edge kind, written as a literal, so excluding that kind
		// excludes every row the arm can produce: the arm is dropped rather than filtered.
		if slices.Contains(excludeKinds, arm.kind) {
			continue
		}
		arms = append(arms, arm.sql(target))
	}
	return arms, true
}

// backlinkTarget is what the arms built for it count: kind is the node kind their edges point
// at, written into each mention arm as a literal. The address counted is the edge's own to_id,
// unless blocks names an item's blocks table and item the column of it that names the item, when
// each arm counts the item holding the current block its edge points at instead, because that
// item's own read lists the edge (readEdgeRows).
type backlinkTarget struct {
	kind   string
	blocks string
	item   string
	// one says the count is over a single address, bound as itself rather than as an array
	// of one (backlinkCountStatement): the shape every read of one node's page runs, and the
	// one shape whose generic plan Postgres keeps.
	one bool
}

// read is how an arm whose rows point at the address toID is written for the target: the address
// the arm counts, the join that reaches it, the predicate that keys the arm on the addresses
// bound as $1, and the expression the arm's own test of the address is written over. An arm over
// an item's blocks is keyed on the item's id as text and reaches the block by its node id, the
// two columns the blocks table is indexed on, so the pair is one lookup each way whichever end
// the planner starts from; its test of the address is written over the blocks table's own node
// id, the same value, which leaves the arm's other relation keyed on the join alone.
func (t backlinkTarget) read(toID string) (counted, join, keyed, address string) {
	if t.blocks == "" {
		return toID, "", toID + t.addressed(), toID
	}
	counted = `part.` + t.item + `::text`
	return counted,
		` join ` + t.blocks + ` part on part.node_id = ` + toID,
		counted + t.addressed() + ` and part.position is not null`,
		`part.node_id`
}

// addressed is the test every arm is keyed on: the one address bound as $1, or membership of
// the array of them. Both read the same index the same way; which is written decides only what
// the planner may assume about how many addresses there are, and so whether it can keep one
// plan for the statement (backlinkCountStatement).
func (t backlinkTarget) addressed() string {
	if t.one {
		return ` = $1`
	}
	return ` = any($1)`
}

// backlinkSeen is the parameter the acting credential is bound at, which every arm's
// visibility predicate names (backlinkCountStatement).
const backlinkSeen = "$3"

// A kind's cutover row says which relation holds that kind's edges now: while it reads false
// the projection table does and the tuples do not, and the moment the backfill of that kind
// completes it is the other way round (migration 20261109013400). graph_edges carries both
// tests the same way, so an arm of this count answers exactly what the view's arm answers at
// every instant. Each is a scalar subquery over a table of three rows, keyed on its primary
// key, which the planner reads once for the whole statement.
func beforeCutover(kind string) string {
	return `not (select complete from graph_store_cutover where kind = '` + kind + `')`
}

func afterCutover(kind string) string {
	return `(select complete from graph_store_cutover where kind = '` + kind + `')`
}

// currentTuple is the test that the row alias names is the fact the store holds now: the newest
// tuple of its (from, kind, to), kept only while that newest one asserts, which is how the
// store reads a current relation (graphstore.CurrentOut) and how the view's own tuple arm reads
// one. The probe for a later tuple is keyed on graph_tuples_current, whose columns are exactly
// the five it names and then the sequence.
func currentTuple(alias string) string {
	return alias + `.asserted and not exists (select 1 from graph_tuples later
		where later.from_kind = ` + alias + `.from_kind and later.from_id = ` + alias + `.from_id
			and later.kind = ` + alias + `.kind and later.to_kind = ` + alias + `.to_kind
			and later.to_id = ` + alias + `.to_id and later.seq > ` + alias + `.seq)`
}

// liveVisibleNode is the two tests a tuple arm makes of the node an edge comes from, answered
// by one read of that node's row.
//
// The first is the view's: the store answers an edge only for a node it holds whose current
// state is not gone, which is what keeps the tuples level with a projection row that is really
// deleted - a block a version no longer holds. The state is the node row's, the projection of
// its own state tuples, so a block a later version restores holds its edges again. The second
// is that node kind's predicate from the registry, which keeps an edge whose source the acting
// person may not see out of the count exactly as it is out of the list (visibility.go).
//
// Written apart - an `exists` over the node's primary key for the state, and the predicate's
// own scalar subquery over the same key - the arm read that one row twice for every candidate
// tuple: two index probes and eight buffer hits a row, which on a node with 5,000 inbound
// tuples is 10,000 probes and 40,000 hits for 5,000 edges. The row the predicate needs is the
// row the state test has already found, so both tests are made of one read of it. A node the
// store holds no row for answers false here, as it did before: the state test refuses it, and
// the registry's rule that such a node is visible is never reached for an edge this arm can
// answer, since the tuples are the store's own and a node of the store has a row.
//
// It is keyed as the view's join is, over the node's own primary key, so the count and the
// view cannot read one tuple differently.
func liveVisibleNode(kind, address string) string {
	return `exists (select 1 from graph_nodes live
		where live.kind = '` + kind + `' and live.id = ` + address + ` and live.state <> '` + graphstore.StateGone + `'
			and ` + VisibleKindRow(kind, "live", backlinkSeen) + `)`
}

// backlinkArm is one arm of graph_edges whose edge kind is a SQL literal rather than a column:
// kind is that literal, from the relations the arm reads, where the arm's own predicates and
// toID the address the view selects as its to_id. Because the kind is fixed, a caller excluding
// it excludes the whole arm. An arm is held in parts rather than as finished SQL because the
// count reads each of them two ways, keyed on the address the arm points at and keyed on the
// item whose current block that address is.
type backlinkArm struct {
	kind string
	from string
	// fromKind and fromID are the node the arm's edge comes from: the kind the view writes as
	// that arm's from_kind, and the expression that names the node under it. They are what the
	// arm binds its visibility predicate over, so an edge whose source the acting person may
	// not see raises no count, exactly as it is no row of the list (readEdgeRows).
	fromKind string
	fromID   string
	// addressIs is the test the view's arm makes of the address itself, written as the tail of
	// a predicate over it: a reply names an ask's part and a comment's block in one column, and
	// the node id's prefix is what tells the two kinds apart. It is written over whichever
	// expression carries the address in the arm the count builds, so the same test reaches the
	// relation the arm is keyed on. Written instead over the replies' own column, the prefix
	// test reads as a near-certain filter on comments, which prices that table as the selective
	// side of the arm over an ask's parts: the planner then reads comments_replies_to_block
	// whole, with no index condition, under the custom and the generic plan alike
	// (TestBacklinkCountStatementReadsEveryArmThroughAnIndex).
	addressIs string
	where     string
	toID      string
	// cutover and tupleKind are how the arm reads an edge the node and tuple store now writes:
	// cutover is that kind's row of graph_store_cutover and tupleKind the kind the store
	// states the edge under, which is the view's kind except for a block's membership of its
	// document, stated as in_document and rendered as part_of. An arm that names neither reads
	// its own table alone, as every arm did before the store existed.
	cutover   string
	tupleKind string
}

// sql is the arm as the count reads it for target, keyed on the addresses bound as $1 and
// narrowed to the sources the acting credential may see. An arm over a kind the store writes
// is two reads, the table's and the store's, each behind its own side of that kind's cutover,
// exactly as graph_edges holds the two arms it answers that kind from.
func (a backlinkArm) sql(target backlinkTarget) string {
	counted, join, keyed, address := target.read(a.toID)
	where := a.where
	if a.addressIs != "" {
		if where != "" {
			where += ` and `
		}
		where += address + a.addressIs
	}
	if a.cutover != "" {
		if where != "" {
			where += ` and `
		}
		where += beforeCutover(a.cutover)
	}
	if where != "" {
		where += ` and `
	}
	read := `select ` + counted + ` from ` + a.from + join + ` where ` + where + keyed +
		` and ` + VisibleKind(a.fromKind, a.fromID, backlinkSeen)
	if a.cutover == "" {
		return read
	}
	return read + ` union all ` + a.tupleSQL(target)
}

// tupleSQL is the arm's reading of the store, which answers once the kind's cutover row reads
// true: the current tuples of this arm's kind pointing at the addresses bound as $1, read
// through graph_tuples_inbound, whose columns are the to side, the kind and then the from side.
// Its last test is the view's own two rules over the node the tuple comes from, taken with the
// registry's predicate for that node kind out of one read of its row (liveVisibleNode): an edge
// is the newest tuple of its own while that one asserts, a node whose state is gone holds none,
// and a node the acting person may not see raises no count.
func (a backlinkArm) tupleSQL(target backlinkTarget) string {
	counted, join, keyed, address := target.read(`tuple.to_id`)
	where := `tuple.to_kind = '` + target.kind + `' and tuple.kind = '` + a.tupleKind +
		`' and tuple.from_kind = '` + a.fromKind + `'`
	if a.addressIs != "" {
		where += ` and ` + address + a.addressIs
	}
	return `select ` + counted + ` from graph_tuples tuple` + join + ` where ` + where +
		` and ` + keyed + ` and ` + currentTuple("tuple") +
		` and ` + afterCutover(a.cutover) + ` and ` + liveVisibleNode(a.fromKind, `tuple.from_id`)
}

// backlinkArms is, for each node kind nodeRegistry resolves, every arm of graph_edges whose edge
// kind is a literal and whose target is a node of that kind. BacklinkCounts refuses a kind that
// is no key of this table, so a node kind a newer build adds to the graph is counted only once
// its arms are declared here; "block" is present and empty because no literal-kind arm of the
// view targets a document's block, and an entry that is present but empty is counted from the
// mention arms alone rather than refused.
//
// Each arm reads only the relations it needs to reach the address it points at and leaves out
// the joins the view makes only to date an edge. Dropping them preserves every row:
// components.snapshot_id (0037 line 38), component_depends' (project_key, from_id) and
// (project_key, to_id) foreign keys (0037 lines 47-48) and issue_component_members.issue_key
// (0038 line 21) are each a not-null foreign key to the primary key the view joins, so each of
// those joins finds exactly one row.
var backlinkArms = map[string][]backlinkArm{
	"issue": {
		// issues_parent_key, partial on parent_key is not null.
		{kind: "child_of", from: `issues i`, where: `i.parent_key is not null`, toID: `i.parent_key`,
			fromKind: "issue", fromID: `i.key`},
		// issue_links_target (target_key, kind).
		{kind: "blocked_by", from: `issue_links l`, where: `l.kind = 'blocked_by'`, toID: `l.target_key`,
			fromKind: "issue", fromID: `l.issue_key`},
		// artifacts_issue_key, partial on issue_key is not null.
		{kind: "attached_to", from: `artifacts a`, where: `a.issue_key is not null`, toID: `a.issue_key`,
			fromKind: "artifact", fromID: `a.ref_key`},
	},
	// The view addresses an artifact by artifacts.ref_key, so each arm starts at artifacts
	// through the unique artifacts_ref_key_key and joins the table that points at it.
	"artifact": {
		// asks_anchor_artifact, on the same coalesce the join makes.
		{kind: "anchored_to", from: `artifacts a join asks k on coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid) = a.id`, toID: `a.ref_key`,
			fromKind: "ask", fromID: `k.id::text`},
		// comments_anchor_artifact, on the same cast the join makes; the store states the same
		// fact as a comment's anchored_to.
		{kind: "anchored_to", from: `artifacts a join comments c on (c.anchor->>'artifact_id')::uuid = a.id`, toID: `a.ref_key`,
			fromKind: "comment", fromID: `c.id::text`, cutover: "comment", tupleKind: "anchored_to"},
		// asks_artifact_id.
		{kind: "owned_by", from: `artifacts a join asks k on k.artifact_id = a.id`, toID: `a.ref_key`,
			fromKind: "ask", fromID: `k.id::text`},
		// comments_artifact_id, and the comment's owned_by in the store.
		{kind: "owned_by", from: `artifacts a join comments c on c.artifact_id = a.id`, toID: `a.ref_key`,
			fromKind: "comment", fromID: `c.id::text`, cutover: "comment", tupleKind: "owned_by"},
		// The document_blocks primary key (artifact_id, block_id). The store states a block's
		// membership of its document as in_document, which the view renders as this part_of.
		{kind: "part_of", from: `artifacts a join document_blocks d on d.artifact_id = a.id`, toID: `a.ref_key`,
			fromKind: "block", fromID: `d.node_id`, cutover: "block", tupleKind: TupleInDocument},
	},
	"ask": {
		// comments_ask_id_text.
		{kind: "replies_to", from: `comments c`, where: `c.ask_id is not null`, toID: `c.ask_id::text`,
			fromKind: "comment", fromID: `c.id::text`},
		// ask_blocks_ask_id_text. The view's arm counts a current part only, so this one carries
		// its position filter.
		{kind: "part_of", from: `ask_blocks b`, where: `b.position is not null`, toID: `b.ask_id::text`,
			fromKind: "ask_block", fromID: `b.node_id`},
	},
	"comment": {
		// comments_reply_to_text.
		{kind: "replies_to", from: `comments c`, where: `c.reply_to is not null`, toID: `c.reply_to::text`,
			fromKind: "comment", fromID: `c.id::text`},
		// comments_answers_text, partial on answers is not null.
		{kind: "answers", from: `comments c`, where: `c.answers is not null`, toID: `c.answers::text`,
			fromKind: "comment", fromID: `c.id::text`},
		// comment_blocks_comment_id_text. The view's arm counts a current block only, so this
		// one carries its position filter.
		{kind: "part_of", from: `comment_blocks b`, where: `b.position is not null`, toID: `b.comment_id::text`,
			fromKind: "comment_block", fromID: `b.node_id`},
	},
	"message": {
		// messages_in_reply_to_text.
		{kind: "replies_to", from: `messages m`, where: `m.in_reply_to is not null`, toID: `m.in_reply_to::text`,
			fromKind: "message", fromID: `m.id::text`},
		// message_blocks_message_id_text, with the view's position filter.
		{kind: "part_of", from: `message_blocks b`, where: `b.position is not null`, toID: `b.message_id::text`,
			fromKind: "message_block", fromID: `b.node_id`},
	},
	"component": {
		// components_parent_node, on the same expression the arm selects.
		{kind: "part_of", from: `components c`, where: `c.parent is not null`, toID: `c.project_key || '/' || c.parent`,
			fromKind: "component", fromID: `c.project_key || '/' || c.id`},
		// component_depends_to_node.
		{kind: "depends_on", from: `component_depends d`, toID: `d.project_key || '/' || d.to_id`,
			fromKind: "component", fromID: `d.project_key || '/' || d.from_id`},
		// issue_component_members_node.
		{kind: "affects", from: `issue_component_members m`, toID: `m.project_key || '/' || m.component_id`,
			fromKind: "issue", fromID: `m.issue_key`},
	},
	"session": {
		// ask_followers_session_id (migration 20261101182419), which exists for this arm.
		{kind: "followed_by", from: `ask_followers f`, toID: `f.session_id`,
			fromKind: "ask", fromID: `f.ask_id::text`},
	},
	// A person, a role and a page are pointed at by mentions alone. The tuples the store holds
	// of them - authored_by, delivered_to, held_by and part_of - are no arm of graph_edges, so
	// no count reads one; each entry is present and empty so its kind is counted from the
	// mention arms rather than refused, as a document's block is.
	"person": {},
	"role":   {},
	"page":   {},
	// No arm of the view whose edge kind is a literal targets a document's block: such a block
	// is pointed at by mentions alone, a reply naming the block of an ask, a comment or a
	// message instead. The entry is present and empty so a block is counted from its mention
	// arms rather than refused.
	"block": {},
	// A reply names the block it answers by node id, and the prefix of that id is what tells an
	// ask's part from a comment's block, so each of these two arms carries the test the view's
	// own arm carries. Both read comments_replies_to_block, partial on replies_to_block is not
	// null, and neither carries a position filter, as the view's arms do not.
	"comment_block": {
		{kind: "replies_to", from: `comments c`, where: `c.replies_to_block is not null`, addressIs: ` not like 'ask/%'`, toID: `c.replies_to_block`,
			fromKind: "comment", fromID: `c.id::text`},
	},
	"ask_block": {
		{kind: "replies_to", from: `comments c`, where: `c.replies_to_block is not null`, addressIs: ` like 'ask/%'`, toID: `c.replies_to_block`,
			fromKind: "comment", fromID: `c.id::text`},
	},
	"message_block": {
		// messages_replies_to_block, partial on replies_to_block is not null.
		{kind: "replies_to", from: `messages m`, where: `m.replies_to_block is not null`, toID: `m.replies_to_block`,
			fromKind: "message", fromID: `m.id::text`},
	},
}

// BacklinkKinds is every node kind BacklinkCounts answers for, sorted: the keys of the arm
// table, which is what the count refuses a kind for not holding. A test outside this package
// counts the same kinds the count itself declares rather than keeping a list of its own
// (docs.TestGraphEdgesLosesEveryArmOverAMovedKind).
func BacklinkKinds() []string {
	return slices.Sorted(maps.Keys(backlinkArms))
}

// mentionBacklinkArms is the six arms of graph_edges whose edge kind and target kind are
// columns rather than literals: refs, the four tables of block references, and the store's own
// tuples, which hold a document block's links once that kind's cutover row reads true. Every
// node kind gets all six, since a mention can name any node. The target kind is written as a
// literal (see BacklinkCounts), and excludeKinds the caller's excluded edge kinds, bound as
// $4 - on these arms the edge kind is a column, so an exclusion is a predicate and never drops
// an arm, and an empty list adds nothing.
//
// Each arm carries the visibility predicate of the node its mention comes from: the refs arm's
// source kind is a column, so it binds the registry over every kind at once (VisibleEnd, over
// the from side's spelling of an id), and each block arm's source is a block of the kind its
// table holds.
func mentionBacklinkArms(target backlinkTarget, excludeKinds []string) []string {
	exclude := ""
	if len(excludeKinds) > 0 {
		exclude = ` and not (r.kind = any($4))`
	}
	counted, join, keyed, _ := target.read(`r.to_id`)
	arm := func(relation, rest, visible string) string {
		return `select ` + counted + ` from ` + relation + join + `
			where r.to_kind = '` + target.kind + `' and ` + keyed + rest + exclude + ` and ` + visible
	}
	arms := []string{
		// refs_to (to_kind, to_id). heldByABlock belongs on this arm alone: it is gated on
		// kind = 'mentions' and from_kind in artifact, comment, message, ask, and refs is the
		// only arm of the view that can emit that pair, every other arm fixing from_kind as a
		// literal that is none of the four. Its exists probes carry no position filter, as the
		// view's readers do not. The source-kind filter is this arm's alone for the same reason:
		// every other arm's literal from_kind is a kind nodeRegistry resolves, so the filter would
		// drop nothing there.
		arm(`refs r`, ` and r.from_kind = any($2) and not `+heldByABlock("r"),
			VisibleEnd(`r.from_kind`, `r.from_id`, false, backlinkSeen)),
		// block_refs_to (to_kind, to_id), while the block_ref cutover row reads false. The
		// view's own arm lists a block[] pointer's row only while block_ref_listed holds, the
		// types test a typed block's pointer carries, and a row with no limit is listed without
		// the call; this arm repeats both, so the count is the rows the list shows. A pointer
		// whose target is retyped out of its allowed types leaves the count the moment it
		// leaves the list, with no rewrite of either.
		arm(`block_refs r`, ` and (r.to_types is null or block_ref_listed(r.to_types, r.to_id))
			and `+beforeCutover("block_ref"),
			VisibleKind("block", `r.from_node`, backlinkSeen)),
		// The same links in the store, read through graph_tuples_inbound once that cutover row
		// reads true: every current relation a block states but its membership of its document,
		// which is the artifact's own part_of arm, and its placement under the block holding it,
		// which is no edge of the view. The types test is the same one with the attribute node's
		// targets as its input, as the view's tuple arm makes it (migration 20261109215947), and
		// a mention is no attribute of any block type, so it is listed without the lookup. Its
		// own test of the citing block is the view's state rule and this registry's predicate
		// made out of one read of that block's node row (liveVisibleNode).
		arm(`graph_tuples r`, ` and r.from_kind = 'block' and r.kind <> '`+TupleInDocument+`'
			and r.kind <> '`+graphstore.TuplePartOf+`' and `+currentTuple("r")+`
			and (r.kind = 'mentions' or block_ref_listed(block_pointer_targets(r.from_id, r.kind), r.to_id))
			and `+afterCutover("block_ref"),
			liveVisibleNode("block", `r.from_id`)),
	}
	// One arm per item kind over the links its blocks hold: <kind>_block_refs_to
	// (to_kind, to_id, (<item>::text)), then the blocks table's primary key for the join. The
	// join keeps the view's position filter: a link a block holds is listed while that block is
	// one of the item's current ones, and the count copies the view's answer rather than
	// correcting it.
	for _, item := range itemKinds {
		tables, _ := item.tables()
		arms = append(arms, arm(tables.refs+` r join `+tables.blocks+` b on b.`+tables.item+` = r.`+tables.item+` and b.block_id = r.block_id`,
			` and b.position is not null`, VisibleKind(item.BlockKind(), `r.from_node`, backlinkSeen)))
	}
	return arms
}
