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
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
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
	statement, rest, err := backlinkCountRead(who, toKind, excludeKinds, one)
	if err != nil {
		return "", nil, err
	}
	var addresses any = ids
	if one {
		addresses = ids[0]
	}
	return statement, append([]any{addresses}, rest...), nil
}

// BacklinkCountRead is the statement BacklinkCounts runs over an array of addresses, and the
// arguments it binds after them: $1 is the addresses (text[]), and rest are $2 onwards - the node
// kinds this build reads, the acting credential and, when excludeKinds is not empty, those kinds.
// Each row it answers is one address that has an inbound edge and its count, as scanBacklinkCounts
// reads them. It is exported for a reader that counts inside a statement of its own, binding $1
// to its own addresses and the rest to placeholders of its own (api's GET /api/v1/me/inbox), so
// the count it splices in is this one. A kind the arm table does not declare is refused, as
// BacklinkCounts refuses it.
func BacklinkCountRead(who model.Actor, toKind string, excludeKinds []string) (statement string, rest []any, err error) {
	return backlinkCountRead(who, toKind, excludeKinds, false)
}

// backlinkCountRead is backlinkCountStatement less its first argument, the addresses, for one
// address bound as itself or for an array of them (one).
func backlinkCountRead(who model.Actor, toKind string, excludeKinds []string, one bool) (string, []any, error) {
	statement, ok := backlinkCountSQL(toKind, excludeKinds, one)
	if !ok {
		return "", nil, fmt.Errorf("count %s backlinks: no arms declared for that node kind", toKind)
	}
	rest := []any{nodeKinds, VisibilityArg(who)}
	if len(excludeKinds) > 0 {
		rest = append(rest, excludeKinds)
	}
	return statement, rest, nil
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

// A projection row says for itself which relation holds its edges: while it names no node the
// table does, and from the moment it names one the tuples do (migration 20261109215947). The
// two sets never overlap, because the writer that states a row's tuples names its node on the
// row in the same transaction, and the store's reading answers a tuple only while the row that
// names it carries its node (projectionCarries). graph_edges carries both tests the same way, so
// an arm of this count answers exactly what the view's arm answers at every instant, including
// for the rows a previous release wrote and for a block past the address bound, which name no
// node and are answered by the table for as long as that is so, and for a row a previous release
// deleted, which neither answers.
func rowNamesNoNode(column string) string { return column + ` is null` }

// A comment's pointers - what it replies to, what it answers, the block it names - are told
// apart the same way by a marker of the comment's own rather than by its node, because a comment
// names its node whether its pointers are tuples or not (migration 20261116124447):
// pointersNotStored is the arm's own test that the comments row at alias is one whose pointers the
// store does not hold, which the table's arm answers, and pointersStored what the store's reading
// adds to its probe of that row (projectionCarries), so a tuple counts only while the row holds
// tuples_stored. graph_edges carries both tests the same way (migration 20261116124448).
func pointersNotStored(alias string) string { return `not ` + alias + `.tuples_stored` }

const pointersStored = `projected.tuples_stored`

// projectionCarries is the join the store's reading of a moved kind makes to its projection row:
// the tuple at alias counts only while a row of table names the node it comes from, fromKind as
// a literal so the probe reads that table's (node_kind, node_address) index, and while that row
// meets carried as well, where carried is not empty: a test over the row the probe finds, written
// over the alias `projected` (pointersStored). An ask holds a follower row per session, so the
// follower row that carries a followed_by is the one of the session the tuple reaches. A build of
// the release before this one deletes a row and appends no retraction, so the tuple outlives the
// row, and only the row can say the edge still stands; that build may also write the row again
// with no node, which its table's arm then answers alone. graph_edges' tuple arm makes the same
// join (migrations 20261109215947 and 20261116094101).
//
// The probe ends in `offset 0`, as each of graph_edges' does, which keeps it one probe of that
// index per tuple the arm counts: without it PostgreSQL may pull it up into a semi-join it hashes
// from a read of the whole projection table, and offset 0 is the fence that stops the pull-up
// (simplify_EXISTS_query) while skipping no row.
func projectionCarries(table, fromKind, alias, carried string) string {
	if carried != "" {
		carried = ` and ` + carried
	}
	if table == "ask_followers" {
		carried += ` and projected.session_id = ` + alias + `.to_id`
	}
	return `exists (select 1 from ` + table + ` projected
		where projected.node_kind = '` + fromKind + `' and projected.node_address = ` + alias + `.from_id` + carried + `
		offset 0)`
}

// blockRefCarries is projectionCarries for a link, whose projection row is the block_refs row of
// the pair: the same node, kind and far end as the tuple at alias. The node is fromKind's: a
// block's own link, or an answered ask's shaped, whose block_refs row names the ask's node
// (migration 20261116094101).
func blockRefCarries(fromKind, alias string) string {
	return `exists (select 1 from block_refs projected
		where projected.node_kind = '` + fromKind + `' and projected.node_address = ` + alias + `.from_id
			and projected.kind = ` + alias + `.kind and projected.to_kind = ` + alias + `.to_kind
			and projected.to_id = ` + alias + `.to_id
		offset 0)`
}

// TupleKinds is every relation kind this build writes as tuples and graph_edges answers from
// them, sorted: a block's in_document and mentions, the pointer kinds the installed block schema
// declares (pmdoc.LinkKinds), a comment's and an ask's anchored_to and owned_by, an ask's
// followed_by, a decision block's from_ask and decided_by, which the arm renders through the
// block_refs row of the pair as it renders a block's every other link, a message's and a
// comment's replies_to, a comment's answers, and an item block's part_of. It is the list the
// view's tuple arms render (migrations 20261109215947, 20261116094101, 20261116114517,
// 20261116124448 and 20261116140008) and the list the announcer writes into
// server_build_announcements.tuple_kinds (startup.Deps), which is how the release that deletes an
// old arm of the view reads whether the release a rollback would reach writes that arm's kinds as
// tuples too. A child that moves another relation into the store adds its kinds here, so the arm
// and the announcement read one list.
func TupleKinds() []string {
	kinds := append([]string{TupleInDocument, "mentions", graphstore.TupleAnchoredTo, graphstore.TupleOwnedBy,
		graphstore.TupleRepliesTo, graphstore.TupleAnswers, graphstore.TuplePartOf, graphstore.TupleFollowedBy, TupleFromAsk,
		graphstore.TupleDecidedBy}, pmdoc.LinkKinds()...)
	slices.Sort(kinds)
	return slices.Compact(kinds)
}

// currentTuple is the test that the row alias names is the fact the store holds now: the newest
// tuple of its (from, kind, to), kept only while that newest one asserts, which is how the store
// reads a current relation (graphstore.CurrentTupleRead) and how the view's own tuple arm reads
// one. The read of the newest binds all five columns of the relation from the row, and reads
// them through graph_tuples_as_of_in, which leads with the far end and the kind: the row's kind
// proves nothing of graph_tuples_current's predicate, which leaves delivered_to out. The row is a
// relation, never a value, and the read says so, which is the predicate as_of_in is partial on.
func currentTuple(alias string) string {
	return alias + `.asserted and ` + alias + `.seq = (select newest.seq from (` +
		graphstore.CurrentTupleRead(`t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id`, `t.seq`,
			`t.from_kind = `+alias+`.from_kind and t.from_id = `+alias+`.from_id
			and t.kind = `+alias+`.kind and t.to_kind = `+alias+`.to_kind
			and t.to_id = `+alias+`.to_id and t.to_kind <> '`+graphstore.KindValue+`'`) + `) newest)`
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
	// projected and tupleKind are how the arm reads an edge the node and tuple store now
	// writes: projected is the arm's own test that its row is one the store does not answer,
	// which the arm answers alone - its node_kind column null (rowNamesNoNode), or for a
	// comment's pointers its tuples_stored false (pointersNotStored) - and tupleKind the kind the
	// store states the edge under, which is the view's kind except for a block's membership of
	// its document, stated as in_document and rendered as part_of. tupleToKind is the kind the
	// store names the far end by where it is not the target's own: a reply to a block of a
	// message, a comment or an ask points at that block's node, of kind block, which the view
	// renders as the item's block address form (migrations 20261116114517 and 20261116124448).
	// tupleFromKind is the kind the store names the near end by where it is not the view's: an
	// item's block is a `block` node, which the view renders as the item's block address form
	// (migration 20261116140008), so the store's reading keys and probes the tuple under `block`
	// and tests the node's state there, while the registry's predicate it binds is the address
	// form's, the one the list of those edges binds (blockNodeVisibleAs).
	// projectedIn is the table that row is in, which the store's reading joins
	// (projectionCarries), and carried what that join requires of the row beside its naming the
	// tuple's node (pointersStored, currentBlockRow), empty where naming it is the whole test. An
	// arm that names none of them reads its own table alone, as every arm did before the store
	// existed.
	projected     string
	tupleKind     string
	tupleToKind   string
	tupleFromKind string
	projectedIn   string
	carried       string
}

// sql is the arm as the count reads it for target, keyed on the addresses bound as $1 and
// narrowed to the sources the acting credential may see. An arm over a kind the store writes
// is two reads, the table's and the store's, the first over the rows the store does not answer
// and the second over what the store holds, exactly as graph_edges holds the two arms it answers
// that kind from.
func (a backlinkArm) sql(target backlinkTarget) string {
	counted, join, keyed, address := target.read(a.toID)
	where := a.where
	if a.addressIs != "" {
		if where != "" {
			where += ` and `
		}
		where += address + a.addressIs
	}
	if a.projected != "" {
		if where != "" {
			where += ` and `
		}
		where += a.projected
	}
	if where != "" {
		where += ` and `
	}
	read := `select ` + counted + ` from ` + a.from + join + ` where ` + where + keyed +
		` and ` + VisibleKind(a.fromKind, a.fromID, backlinkSeen)
	if a.tupleKind == "" {
		return read
	}
	return read + ` union all ` + a.tupleSQL(target)
}

// tupleSQL is the arm's reading of the store: the current tuples of this arm's kind pointing at
// the addresses bound as $1, read through graph_tuples_as_of_in, which leads with the to side
// and the kind. Its last tests are the view's own three rules over the node the tuple comes
// from: an edge is the newest tuple of its own while that one asserts, it counts only while the
// projection row that names the node carries it (projectionCarries), and a node whose state is
// gone holds none, the last taken with the registry's predicate for that node kind out of one
// read of its row (liveVisibleNode), so a node the acting person may not see raises no count.
func (a backlinkArm) tupleSQL(target backlinkTarget) string {
	counted, join, keyed, address := target.read(`tuple.to_id`)
	toKind := target.kind
	if a.tupleToKind != "" {
		toKind = a.tupleToKind
	}
	fromKind := a.fromKind
	visible := liveVisibleNode(a.fromKind, `tuple.from_id`)
	if a.tupleFromKind != "" {
		fromKind = a.tupleFromKind
		visible = blockNodeVisibleAs(a.fromKind, `tuple.from_id`)
	}
	where := `tuple.to_kind = '` + toKind + `' and tuple.kind = '` + a.tupleKind +
		`' and tuple.from_kind = '` + fromKind + `'`
	if a.addressIs != "" {
		where += ` and ` + address + a.addressIs
	}
	return `select ` + counted + ` from graph_tuples tuple` + join + ` where ` + where +
		` and ` + keyed + ` and ` + currentTuple("tuple") +
		` and ` + projectionCarries(a.projectedIn, fromKind, "tuple", a.carried) +
		` and ` + visible
}

// blockNodeVisibleAs is the two tests a tuple arm makes of an item's block its edge comes from:
// the view's, that the store holds the block's `block` node and its state is not gone, and the
// registry's predicate of the address form the view renders the block under (viewKind:
// comment_block, message_block or ask_block), which is the predicate the list of those edges
// binds over the view's from_kind (readEdgeRows), so a count and its list cannot disagree.
func blockNodeVisibleAs(viewKind, address string) string {
	return `exists (select 1 from graph_nodes live
		where live.kind = '` + BlockNodeKind + `' and live.id = ` + address + ` and live.state <> '` + graphstore.StateGone + `')
		and ` + VisibleKind(viewKind, address, backlinkSeen)
}

// currentBlockRow is what the store's reading of an item block's part_of requires of the block's
// row beside its naming the block's node: that it stands at a position, the old arm's own filter,
// so a removed block's part_of, which the store keeps, is counted by neither.
const currentBlockRow = `projected.position is not null`

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
		// asks_anchor_artifact, on the same coalesce the join makes; the store states the same
		// fact as an ask's anchored_to, answered while the ask's row carries its node.
		{kind: "anchored_to", from: `artifacts a join asks k on coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid) = a.id`, toID: `a.ref_key`,
			fromKind: "ask", fromID: `k.id::text`, projected: rowNamesNoNode(`k.node_kind`), tupleKind: graphstore.TupleAnchoredTo, projectedIn: "asks"},
		// comments_anchor_artifact, on the same cast the join makes; the store states the same
		// fact as a comment's anchored_to, answered while the comment's row carries its node.
		{kind: "anchored_to", from: `artifacts a join comments c on (c.anchor->>'artifact_id')::uuid = a.id`, toID: `a.ref_key`,
			fromKind: graphstore.KindComment, fromID: `c.id::text`, projected: rowNamesNoNode(`c.node_kind`), tupleKind: graphstore.TupleAnchoredTo, projectedIn: "comments"},
		// asks_artifact_id, and the ask's owned_by in the store.
		{kind: "owned_by", from: `artifacts a join asks k on k.artifact_id = a.id`, toID: `a.ref_key`,
			fromKind: "ask", fromID: `k.id::text`, projected: rowNamesNoNode(`k.node_kind`), tupleKind: graphstore.TupleOwnedBy, projectedIn: "asks"},
		// comments_artifact_id, and the comment's owned_by in the store.
		{kind: "owned_by", from: `artifacts a join comments c on c.artifact_id = a.id`, toID: `a.ref_key`,
			fromKind: graphstore.KindComment, fromID: `c.id::text`, projected: rowNamesNoNode(`c.node_kind`), tupleKind: graphstore.TupleOwnedBy, projectedIn: "comments"},
		// The document_blocks primary key (artifact_id, block_id). The store states a block's
		// membership of its document as in_document, which the view renders as this part_of,
		// answered while the block's own row carries its node.
		{kind: "part_of", from: `artifacts a join document_blocks d on d.artifact_id = a.id`, toID: `a.ref_key`,
			fromKind: "block", fromID: `d.node_id`, projected: rowNamesNoNode(`d.node_kind`), tupleKind: TupleInDocument, projectedIn: "document_blocks"},
	},
	"ask": {
		// comments_ask_id_text; the store states the same fact as the reply's replies_to the ask,
		// answered while the reply's row holds tuples_stored (migration 20261116124448).
		{kind: "replies_to", from: `comments c`, where: `c.ask_id is not null`, toID: `c.ask_id::text`,
			fromKind: "comment", fromID: `c.id::text`,
			projected: pointersNotStored("c"), tupleKind: graphstore.TupleRepliesTo, projectedIn: "comments", carried: pointersStored},
		// ask_blocks_ask_id_text. The view's arm counts a current part only, so this one carries
		// its position filter; the store states a part's place as its block node's part_of the
		// ask, answered while the part's row names that node and stands (migration
		// 20261116140008).
		{kind: "part_of", from: `ask_blocks b`, where: `b.position is not null`, toID: `b.ask_id::text`,
			fromKind: "ask_block", fromID: `b.node_id`,
			projected: rowNamesNoNode(`b.node_kind`), tupleKind: graphstore.TuplePartOf, tupleFromKind: BlockNodeKind,
			projectedIn: "ask_blocks", carried: currentBlockRow},
	},
	"comment": {
		// comments_reply_to_text, and the reply's replies_to its thread's root in the store.
		{kind: "replies_to", from: `comments c`, where: `c.reply_to is not null`, toID: `c.reply_to::text`,
			fromKind: "comment", fromID: `c.id::text`,
			projected: pointersNotStored("c"), tupleKind: graphstore.TupleRepliesTo, projectedIn: "comments", carried: pointersStored},
		// comments_answers_text, partial on answers is not null, and the reply's answers in the
		// store.
		{kind: "answers", from: `comments c`, where: `c.answers is not null`, toID: `c.answers::text`,
			fromKind: "comment", fromID: `c.id::text`,
			projected: pointersNotStored("c"), tupleKind: graphstore.TupleAnswers, projectedIn: "comments", carried: pointersStored},
		// comment_blocks_comment_id_text. The view's arm counts a current block only, so this
		// one carries its position filter; the store's part_of is read as the ask's is.
		{kind: "part_of", from: `comment_blocks b`, where: `b.position is not null`, toID: `b.comment_id::text`,
			fromKind: "comment_block", fromID: `b.node_id`,
			projected: rowNamesNoNode(`b.node_kind`), tupleKind: graphstore.TuplePartOf, tupleFromKind: BlockNodeKind,
			projectedIn: "comment_blocks", carried: currentBlockRow},
	},
	"message": {
		// messages_in_reply_to_text; the store states the same fact as the reply's replies_to,
		// answered while the reply's row names its node (migration 20261116114517).
		{kind: "replies_to", from: `messages m`, where: `m.in_reply_to is not null`, toID: `m.in_reply_to::text`,
			fromKind: "message", fromID: `m.id::text`,
			projected: rowNamesNoNode(`m.node_kind`), tupleKind: graphstore.TupleRepliesTo, projectedIn: "messages"},
		// message_blocks_message_id_text, with the view's position filter, and the store's
		// part_of read as the ask's is.
		{kind: "part_of", from: `message_blocks b`, where: `b.position is not null`, toID: `b.message_id::text`,
			fromKind: "message_block", fromID: `b.node_id`,
			projected: rowNamesNoNode(`b.node_kind`), tupleKind: graphstore.TuplePartOf, tupleFromKind: BlockNodeKind,
			projectedIn: "message_blocks", carried: currentBlockRow},
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
		// ask_followers_session_id (migration 20261101182419), which exists for this arm; the
		// store states the same fact as an ask's followed_by, answered while the follower row of
		// that ask and that session carries the ask's node (projectionCarries' session test).
		{kind: "followed_by", from: `ask_followers f`, toID: `f.session_id`,
			fromKind: "ask", fromID: `f.ask_id::text`, projected: rowNamesNoNode(`f.node_kind`), tupleKind: graphstore.TupleFollowedBy, projectedIn: "ask_followers"},
	},
	// A person, a role and a page are pointed at by mentions alone. The tuples the store holds
	// of them - authored_by, delivered_to, held_by and part_of - are no arm of graph_edges, so
	// no count reads one; each entry is present and empty so its kind is counted from the
	// mention arms rather than refused, as a document's block is.
	"person": {},
	"role":   {},
	"page":   {},
	// No arm of the view whose edge kind is a literal targets a delivery attempt: its tuples -
	// owed_to, delivered_to and answered_by - run out of it and are no arm of graph_edges, and no
	// address names one for a writer to mention. The entry is present and empty so an attempt is
	// counted from the mention arms, which answer zero for it today, rather than refused.
	graphstore.KindDeliveryAttempt: {},
	// No arm of the view whose edge kind is a literal targets a document's block: such a block
	// is pointed at by mentions alone, a reply naming the block of an ask, a comment or a
	// message instead. The entry is present and empty so a block is counted from its mention
	// arms rather than refused.
	"block": {},
	// A reply names the block it answers by node id, and the prefix of that id is what tells an
	// ask's part from a comment's block, so each of these two arms carries the test the view's
	// own arm carries. Both read comments_replies_to_block, partial on replies_to_block is not
	// null, and neither carries a position filter, as the view's arms do not. The store states
	// the reply's replies_to to the block's own node, of kind block, which the view renders as
	// these two address forms by the same prefix while the reply's row holds tuples_stored
	// (migration 20261116124448).
	"comment_block": {
		{kind: "replies_to", from: `comments c`, where: `c.replies_to_block is not null`, addressIs: ` not like 'ask/%'`, toID: `c.replies_to_block`,
			fromKind: "comment", fromID: `c.id::text`,
			projected: pointersNotStored("c"), tupleKind: graphstore.TupleRepliesTo, tupleToKind: graphstore.KindBlock, projectedIn: "comments", carried: pointersStored},
	},
	"ask_block": {
		{kind: "replies_to", from: `comments c`, where: `c.replies_to_block is not null`, addressIs: ` like 'ask/%'`, toID: `c.replies_to_block`,
			fromKind: "comment", fromID: `c.id::text`,
			projected: pointersNotStored("c"), tupleKind: graphstore.TupleRepliesTo, tupleToKind: graphstore.KindBlock, projectedIn: "comments", carried: pointersStored},
	},
	"message_block": {
		// messages_replies_to_block, partial on replies_to_block is not null. The store states
		// the reply's replies_to to the block's own node, of kind block, which the view renders
		// as this address form while the reply's row names its node.
		{kind: "replies_to", from: `messages m`, where: `m.replies_to_block is not null`, toID: `m.replies_to_block`,
			fromKind: "message", fromID: `m.id::text`,
			projected: rowNamesNoNode(`m.node_kind`), tupleKind: graphstore.TupleRepliesTo, tupleToKind: graphstore.KindBlock, projectedIn: "messages"},
	},
}

// BacklinkKinds is every node kind BacklinkCounts answers for, sorted: the keys of the arm
// table, which is what the count refuses a kind for not holding. A test outside this package
// counts the same kinds the count itself declares rather than keeping a list of its own
// (docs.TestGraphEdgesLosesEveryArmOverAMovedKind).
func BacklinkKinds() []string {
	return slices.Sorted(maps.Keys(backlinkArms))
}

// mentionBacklinkArms is the arms of graph_edges whose edge kind and target kind are columns
// rather than literals: refs; the block_refs rows naming no node and the store's links of a
// document's blocks; the store's links of every item's blocks, in one arm; and for each item kind
// its citations in the store and its blocks' link rows naming no node. Every node kind gets all
// of them, since a mention can name any node; a document's block gets one more, an answered ask's
// shaped, which points at a block alone. The target kind is written as a literal (see
// BacklinkCounts), and excludeKinds the caller's excluded edge kinds, bound as $4 - on these arms
// the edge kind is a column, so an exclusion is a predicate and never drops an arm, and an empty
// list adds nothing.
//
// Each arm carries the visibility predicate of the node its mention comes from: the refs arm's
// source kind is a column, so it binds the registry over every kind at once (VisibleEnd, over
// the from side's spelling of an id), and each block arm's source is a block of the kind its
// table holds, or for the store's item block arm the kind its address's prefix names.
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
	itemLinkCarried := byItemBlockAddress(`r.from_id`, func(_ ItemKind, tables itemTables) string {
		return itemLinkCarries(tables, "r")
	})
	itemBlockVisible := byItemBlockAddress(`r.from_id`, func(item ItemKind, _ itemTables) string {
		return blockNodeVisibleAs(item.BlockKind(), `r.from_id`)
	})
	arms := []string{
		// refs_to (to_kind, to_id), over the rows whose citation the store does not hold
		// (tuples_stored false: a document's row, and an item's row an older build wrote or the
		// store holds no tuple of, migration 20261116135959). heldByABlock belongs on this arm
		// and its store twin below alone: it is gated on kind = 'mentions' and from_kind in
		// artifact, comment, message, ask, and refs is the only table of the view that can emit
		// that pair, every other arm fixing from_kind as a literal that is none of the four. Its
		// exists probes carry no position filter, as the view's readers do not. The source-kind
		// filter is this arm's alone for the same reason: every other arm's literal from_kind is
		// a kind nodeRegistry resolves, so the filter would drop nothing there.
		arm(`refs r`, ` and r.from_kind = any($2) and not r.tuples_stored and not `+heldByABlock("r"),
			VisibleEnd(`r.from_kind`, `r.from_id`, false, backlinkSeen)),
		// block_refs_to (to_kind, to_id), over the rows that name no node. The view's own arm
		// lists a block[] pointer's row only while block_ref_listed holds, the types test a
		// typed block's pointer carries, and a row with no limit is listed without the call;
		// this arm repeats both, so the count is the rows the list shows. A pointer whose
		// target is retyped out of its allowed types leaves the count the moment it leaves the
		// list, with no rewrite of either.
		arm(`block_refs r`, ` and (r.to_types is null or block_ref_listed(r.to_types, r.to_id))
			and `+rowNamesNoNode(`r.node_kind`),
			VisibleKind("block", `r.from_node`, backlinkSeen)),
		// The same links in the store, read through graph_tuples_as_of_in: every current
		// relation a document's block states but its membership of its document, which is the
		// artifact's own part_of arm, and its placement under the block holding it, which is no
		// edge of the view, each counted only while the block_refs row of its pair carries the
		// block's node (blockRefCarries), as the view's tuple arm joins it. An item's block is
		// the item block arm's below, as the view's tuple arm leaves it to the item block arm
		// (ItemBlockAddress), so the two split the block tuples between them. The types test is
		// the same one with the attribute node's targets as its input, as the view's tuple arm
		// makes it (migration 20261109215947), and a mention is no attribute of any block type,
		// so it is listed without the lookup. Its own test of the citing block is the view's state
		// rule and this registry's predicate made out of one read of that block's node row
		// (liveVisibleNode).
		arm(`graph_tuples r`, ` and r.from_kind = 'block' and not `+ItemBlockAddress(`r.from_id`)+`
			and r.kind <> '`+TupleInDocument+`' and r.kind <> '`+graphstore.TuplePartOf+`' and `+currentTuple("r")+`
			and (r.kind = 'mentions' or block_ref_listed(block_pointer_targets(r.from_id, r.kind), r.to_id))
			and `+blockRefCarries(BlockNodeKind, "r"),
			liveVisibleNode("block", `r.from_id`)),
		// The links of every item's blocks in the store, in one arm as the view's item block arm
		// reads them (migration 20261116140008): the current tuples out of a `block` node whose
		// address an item kind's prefix names, read through graph_tuples_as_of_in, which leads
		// with the far end and the kind, once whichever item the block is of, each counted only
		// while its link row in that item kind's table names the block's node and that block
		// stands (itemLinkCarries), and its citing block tested by the view's state rule over its
		// node and the predicate of the address form the view renders it under
		// (blockNodeVisibleAs), each chosen by the same prefix.
		arm(`graph_tuples r`, ` and r.from_kind = '`+BlockNodeKind+`' and `+ItemBlockAddress(`r.from_id`)+`
			and r.kind <> '`+graphstore.TuplePartOf+`' and `+currentTuple("r")+` and `+itemLinkCarried,
			itemBlockVisible),
	}
	// An answered ask's shaped, its tie to a block of its issue, which the store states from the
	// ask's node and the view's tuple arm answers while the block_refs row of the pair, marked with
	// that node, stands (migration 20261116094101). An ask's shaped declares no targets, so it
	// carries no types test.
	if target.kind == BlockNodeKind {
		arms = append(arms, arm(`graph_tuples r`, ` and r.from_kind = '`+graphstore.KindAsk+`' and r.kind = '`+TupleShaped+`'
			and `+currentTuple("r")+` and `+blockRefCarries(graphstore.KindAsk, "r"),
			liveVisibleNode(graphstore.KindAsk, `r.from_id`)))
	}
	for _, item := range itemKinds {
		tables, _ := item.tables()
		arms = append(arms,
			// An item's citations in the store: the current `mentions` tuples out of the item's
			// node, read through graph_tuples_as_of_in, which leads with the far end and the kind,
			// each counted only while the refs row of its pair stands holding tuples_stored
			// (citationCarries), as the view's tuple arm joins it, and held by no block of the item
			// as its refs row would be (heldByABlock over the tuple's own columns). Its test of the
			// citing item is the view's state rule and the item kind's predicate out of one read of
			// its node row (liveVisibleNode).
			arm(`graph_tuples r`, ` and r.from_kind = '`+string(item)+`' and r.kind = 'mentions' and `+currentTuple("r")+`
				and `+citationCarries("r")+` and not `+heldByABlock("r"),
				liveVisibleNode(string(item), `r.from_id`)),
			// One arm per item kind over the links its blocks hold, over the rows that name no
			// node: <kind>_block_refs_to (to_kind, to_id, (<item>::text)), then the blocks table's
			// primary key for the join. The join keeps the view's position filter: a link a block
			// holds is listed while that block is one of the item's current ones, and the count
			// copies the view's answer rather than correcting it.
			arm(tables.refs+` r join `+tables.blocks+` b on b.`+tables.item+` = r.`+tables.item+` and b.block_id = r.block_id`,
				` and b.position is not null and `+rowNamesNoNode(`r.node_kind`), VisibleKind(item.BlockKind(), `r.from_node`, backlinkSeen)),
		)
	}
	return arms
}

// byItemBlockAddress is a CASE over the item kind prefix of the block address held by column,
// answering for each item kind the test of(item, its tables) makes: the one way a single arm over
// every item's blocks reads each block by the tables of its own item kind. A column no item
// prefix names answers null, which no arm counts.
func byItemBlockAddress(column string, of func(ItemKind, itemTables) string) string {
	var cases strings.Builder
	cases.WriteString(`case`)
	for _, item := range itemKinds {
		tables, _ := item.tables()
		cases.WriteString(` when ` + column + ` like '` + string(item) + `/%' then ` + of(item, tables))
	}
	cases.WriteString(` end`)
	return cases.String()
}

// citationCarries is projectionCarries for an item's citation, whose projection row is the refs
// row of the pair, holding tuples_stored (migration 20261116135959).
func citationCarries(alias string) string {
	return `exists (select 1 from refs projected
		where projected.from_kind = ` + alias + `.from_kind and projected.from_id = ` + alias + `.from_id
			and projected.to_kind = ` + alias + `.to_kind and projected.to_id = ` + alias + `.to_id
			and projected.kind = ` + alias + `.kind and projected.tuples_stored
		offset 0)`
}

// itemLinkCarries is projectionCarries for a link of an item's block, whose projection row is the
// link row of the pair in the kind's table, naming the block's node, with the block's own row
// standing at a position.
func itemLinkCarries(tables itemTables, alias string) string {
	return `exists (select 1 from ` + tables.refs + ` projected
		join ` + tables.blocks + ` held on held.` + tables.item + ` = projected.` + tables.item + ` and held.block_id = projected.block_id
		where projected.node_kind = '` + BlockNodeKind + `' and projected.node_address = ` + alias + `.from_id
			and projected.kind = ` + alias + `.kind and projected.to_kind = ` + alias + `.to_kind
			and projected.to_id = ` + alias + `.to_id and held.position is not null
		offset 0)`
}
