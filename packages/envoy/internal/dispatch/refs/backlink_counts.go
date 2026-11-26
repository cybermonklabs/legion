package refs

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// BacklinkCounts is how many edges point at each of ids, in one grouped query: the number a
// list carries beside a row, counted the same way the rows behind it are listed. It unions only
// the arms of graph_edges that can point at a node of toKind - the seven mention arms every kind
// has and the structural arms declared for that kind (backlinkArms) - and counts them in one
// statement, each arm read through its own index, rather than leaving the planner the whole
// 26-arm view on every call. The counts are the view's, arm for arm, but for an issue's own tree
// and decisions query (issueOwnArtifact), which the view answers attached_to the issue and which
// neither the count nor the list behind it counts. A kind with no entry in the arm table is
// refused rather than counted from the mention arms alone, so a count is never silently low.
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
// (readEdgeRows), so a count and its list cannot disagree. Each arm binds its predicate through
// the gate, as the list does (authorOnlyGate): only where its source can be reserved for its
// author. The arms over a document's block keep theirs (gateKeeps), and every count holds them
// (mentionBacklinkArms), so every statement still names $3, which Postgres must find in it to type.
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

// fourArmsDeleted says graph_edges holds none of the four arms the graph-store spec moved and its
// tuple arm no join to the tables behind them: the arm over document_blocks, the arm over
// block_refs and the two arms over comments, which graphcensus.RunViewArmsStep deletes at a boot
// once it has proved that every row of those tables names its node and that every tuple of their
// kinds is named by a row. While it is false a count reads the view as every set before the step's
// states it (store.GraphEdgesBlockNodeListed): each of those arms over the rows that name no node,
// and the store's reading of each kind while the row that names it carries its node. Once it is true
// the arms marked goesWithFourArms, the arm over block_refs of mentionBacklinkArms and the store's
// reading of a block's links drop their read of the table and their join to the projection row, as
// the view's tuple arm does (store.GraphEdgesFourArmsDeleted): a block's links by their kind
// (store.BlockLinkKindsSQL) and a block's membership and a comment's relations by the tuple alone.
//
// It is the view the database holds, as this process last read it: cmd/dispatch sets it from the
// view's definition before the process serves (graphcensus.ViewArmsGone), again about every minute
// after (graphcensus.FollowViewArms), and once this boot's own step has deleted the arms. So a
// process that boots after another process's step counts as the view answers from its first
// request. A process that read the arms standing counts as the view did before the deletion until
// its next read; what it counts then, and the one class of row it counts that the view no longer
// answers, a row past the store's address bound written in that interval, FollowViewArms says.
var fourArmsDeleted atomic.Bool

// SetFourArmsDeleted records whether graph_edges holds none of the four arms the boot step deletes
// (fourArmsDeleted). Only a process that boots (cmd/dispatch, through the reads graphcensus makes
// of the view and the step's own answer) and a test that reads the counts against a view stated
// without them set it; a test that sets it holds the counts of every other test of its process to
// a view without the arms, so it runs alone and sets it back.
func SetFourArmsDeleted(deleted bool) { fourArmsDeleted.Store(deleted) }

// A projection row says for itself which relation holds its edges: while it names no node the
// table does, and from the moment it names one the tuples do (migration 20261109215947). The
// two sets never overlap, because the writer that states a row's tuples names its node on the
// row in the same transaction, and the store's reading answers a tuple only while the row that
// names it carries its node (projectionCarries). graph_edges carries both tests the same way, so
// an arm of this count answers exactly what the view's arm answers at every instant, including
// for the rows a previous release wrote and for a block past the address bound, which name no
// node and are answered by the table for as long as that is so, and for a row a previous release
// deleted, which neither answers. A row whose edge column a previous release moved in place counts
// as naming no node too, while the store holds no current tuple naming the column's value
// (backlinkArm's moved), so the table answers its new far end and the store's reading, which finds
// no row naming the far end its tuple names, answers neither.
func rowNamesNoNode(column string) string { return column + ` is null` }

// projectionCarries is the join the store's reading of a moved kind makes to its projection row:
// the tuple at alias counts only while a row of table names the node it comes from, fromKind as
// a literal so the probe reads that table's (node_kind, node_address) index. A build of the
// release before this one deletes a row and appends no retraction, so the tuple outlives the row,
// and only the row can say the edge still stands; that build may also write the row again with
// no node, which its table's arm then answers alone. graph_edges' tuple arm makes the same join
// (migrations 20261109215947, 20261116094101, 20261117140000 and 20261118023219). An ask holds a
// follower row per session, so the follower row that carries a followed_by is the one of the
// session the tuple reaches; an issue's row carries its child_of while it names that parent, and
// an issue_links row the needs of its own pair; a component's row carries its part_of while it
// names that parent, a component_depends row the depends_on of its own pair and an
// issue_component_members row the affects of its own issue and component. A component's tables
// carry no index over their node columns, so their probe also binds the address each indexes
// (migrations 0037 and 0038), which the node columns of a marked row repeat.
func projectionCarries(table, fromKind, alias string) string {
	pair := ""
	switch table {
	// kept for one release: asks-arms
	case "ask_followers":
		pair = ` and projected.session_id = ` + alias + `.to_id`
	case "issues":
		pair = ` and projected.key = ` + alias + `.from_id and projected.parent_key = ` + alias + `.to_id`
	case "issue_links":
		pair = ` and projected.issue_key = ` + alias + `.from_id and projected.kind = 'blocked_by' and projected.target_key = ` + alias + `.to_id`
	case "components":
		pair = ` and projected.project_key || '/' || projected.id = ` + alias + `.from_id and projected.project_key || '/' || projected.parent = ` + alias + `.to_id`
	case "component_depends":
		pair = ` and projected.project_key || '/' || projected.from_id = ` + alias + `.from_id and projected.project_key || '/' || projected.to_id = ` + alias + `.to_id`
	case "issue_component_members":
		pair = ` and projected.issue_key = ` + alias + `.from_id and projected.project_key || '/' || projected.component_id = ` + alias + `.to_id`
	}
	return `exists (select 1 from ` + table + ` projected
		where projected.node_kind = '` + fromKind + `' and projected.node_address = ` + alias + `.from_id` + pair + `)`
}

// blockRefCarries is projectionCarries for a link, whose projection row is the block_refs row of
// the pair: the same node, kind and far end as the tuple at alias. The node is fromKind's: a
// block's own link, or an answered ask's shaped, whose block_refs row names the ask's node
// (migration 20261116094101).
func blockRefCarries(fromKind, alias string) string {
	return `exists (select 1 from block_refs projected
		where projected.node_kind = '` + fromKind + `' and projected.node_address = ` + alias + `.from_id
			and projected.kind = ` + alias + `.kind and projected.to_kind = ` + alias + `.to_kind
			and projected.to_id = ` + alias + `.to_id)`
}

// blockLinkCarried is the test the store's reading of a block's links, or of an answered ask's
// shaped, makes of the tuple at alias in place of the block_refs row: while the row of the pair
// carries the node (blockRefCarries), and once graph_edges holds no arm over block_refs
// (fourArmsDeleted, which deleted says), while the tuple's kind is one a block_refs row projects
// (store.BlockLinkKindsSQL), as the view's tuple arm tests it.
func blockLinkCarried(fromKind, alias string, deleted bool) string {
	if deleted {
		return alias + `.kind in (` + store.BlockLinkKindsSQL + `)`
	}
	return blockRefCarries(fromKind, alias)
}

// TupleKinds is every relation kind this build writes as tuples and graph_edges answers from
// them, sorted: a block's in_document and mentions, the pointer kinds the installed block schema
// declares (pmdoc.LinkKinds), a comment's and an ask's anchored_to and owned_by, an ask's
// followed_by, and a decision block's from_ask and decided_by, which the
// arm renders through the block_refs row of the pair as it renders a block's every other link,
// and the kinds the view answers from the tuples alone, with no projection row behind them (its
// store class, migrations 20261116184707 and 20261117020856), which the graph read's store arm
// reads by the same lists (keyedStoreArm): storeArmPointers out of any node, and
// storeArmPlacements and authored_by out of a node of the kinds cybermonklabs/legion#4193 adds
// (graphstore.KindInteraction and the three beside it); an issue's relations, child_of and needs,
// which issues and issue_links project, and duplicate_of and filed_from, which no row does, beside
// a document's attached_to, the part_of to its issue the view answers by that name (migration
// 20261117140000); and a component's part_of its parent and depends_on another component and an
// issue's affects a component, which components, component_depends and issue_component_members
// project (migration 20261118023219).
// It is the list the view's tuple arms render (migrations 20261109215947, 20261116094101,
// 20261116184707, 20261117020856, 20261117140000, 20261118023219 and 20261119100000) and the list
// the announcer writes into server_build_announcements.tuple_kinds (startup.Deps), which is how
// the release that deletes an old arm of the view reads whether the release a rollback would
// reach writes that arm's kinds as tuples too. A child that moves another relation into the store
// adds its kinds here, so the arm and the announcement read one list; a relation the view answers
// from the tuples alone goes into graphstore's pointerRules instead: a pointer there alone, which
// storeArmPointers (graph.go) reads from graphstore.PointerKinds, and a placement there and then
// in storeArmPlacements (graph.go), which TestTheStoreArmReadsEveryKindThePointerRulesHold holds
// to graphstore.PlacementKinds; one that no pointer rule holds, as changed_by_run is, goes into
// storeArmPointers itself. This list reads both.
//
// The announcement names a kind alone, and part_of in it is no claim about every part_of the store
// holds: this build writes a part_of the view answers only out of a lens, a lens_block, a risk, a
// constraint or a component, and a document's to its issue as attached_to. A session's, a page's
// and a block's placement are part_of tuples no arm renders, and a block's membership of its
// document stays in_document.
//
// mentions in it is an artifact source's too, not a block's alone: a document's version index and
// rebuild-refs write a mentions tuple out of the document's node beside each refs row of the
// document (Replacement.ArtifactMentions), which the view answers while that row stands (migration
// 20261119100000).
//
// The list carries this build's capabilities too (processes.Capabilities): issue_room, which no
// relation kind spells, is how a reader of the announcements tells a build that serves the issue
// room (decision 7 of the issue-tree plan). No arm renders a capability, so the view answers
// nothing under that name.
func TupleKinds() []string {
	kinds := slices.Concat([]string{
		TupleInDocument, "mentions", graphstore.TupleAnchoredTo, graphstore.TupleOwnedBy,
		graphstore.TupleFollowedBy, TupleFromAsk, graphstore.TupleDecidedBy, graphstore.TupleAuthoredBy,
		graphstore.TupleChildOf, graphstore.TupleNeeds, graphstore.TupleDuplicateOf, graphstore.TupleFiledFrom,
		TupleAttachedTo, graphstore.TupleDependsOn, graphstore.TupleAffects,
	}, storeArmPointers, storeArmPlacements, pmdoc.LinkKinds(), processes.Capabilities)
	slices.Sort(kinds)
	return slices.Compact(kinds)
}

// TupleAttachedTo is the kind graph_edges answers a document's part_of its issue by, in its arm
// over the artifacts that carry their node as in its arm over those that carry none.
const TupleAttachedTo = "attached_to"

// attachedArtifactStands is the test the view's arm over the artifacts that carry their node makes
// of the artifacts row aliased a: its node is not gone and its part_of to the issue the row names
// is the newest tuple of that relation and asserts it (migration 20261117140000).
const attachedArtifactStands = `a.node_kind is not null
			and exists (select 1 from graph_nodes live where live.kind = a.node_kind and live.id = a.node_address and live.state <> 'gone')
			and exists (select 1 from (
				select t.asserted from graph_tuples t
				where t.from_kind = a.node_kind and t.from_id = a.node_address and t.kind = 'part_of'
				  and t.to_kind = 'issue' and t.to_id = a.issue_key
				order by t.seq desc limit 1) newest where newest.asserted)`

// issueOwnArtifact is true of the artifacts row aliased alias, an issue's, while it is a part of
// the issue rather than a document anybody attached to it: the issue's tree, whose Yjs document
// is the issue's room, or its decisions query node (DecisionsQuerySQL), a view over its decision
// nodes. The view answers both attached_to the issue, as it answers every artifact the issue
// holds, but neither is a backlink of the issue: the issue read's documents leave both out
// (api's issueArtifactsStatement), and so do the issue's count, in its two attached_to arms
// (backlinkArms), and its Referenced-by list (issueOwnAttachment), by this one test. Every other
// artifact attached to the issue is a backlink of it.
func issueOwnArtifact(alias string) string {
	return `(` + alias + `.kind = 'tree' or ` + DecisionsQuerySQL(alias) + `)`
}

// issueOwnAttachment is the test that the graph_edges row aliased alias is the attached_to edge,
// into the issue whose key the text SQL expression key names, of one of that issue's own artifacts
// (issueOwnArtifact): an edge its Referenced-by list leaves out, as its count leaves it out. Both
// of the view's attached_to arms name the artifact by its uuid, read here through the issue's own
// artifacts by artifacts_issue_key.
func issueOwnAttachment(alias, key string) string {
	return `(` + alias + `.kind = '` + TupleAttachedTo + `' and ` + alias + `.from_kind = '` + graphstore.KindArtifact + `'
		and ` + alias + `.from_id in (select own.id::text from artifacts own where own.issue_key = ` + key + ` and ` + issueOwnArtifact("own") + `))`
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
// person may not see out of the count exactly as it is out of the list (visibility.go). The first
// is made on every arm; the second only where the gate keeps the predicate of kind (gateKeeps,
// authorOnlyGate), so a source the store reserves for nobody is read for its state alone.
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
	visible := "true"
	if gateKeeps(kind) {
		visible = VisibleKindRow(kind, "live", backlinkSeen)
	}
	return liveNode(`'`+kind+`'`, address, visible)
}

// liveVisibleNodeOf is liveVisibleNode over an edge whose source kind is a column, kindExpr, of one
// of kinds: the same one read of the node's row, with the predicate of the row's own kind over it
// (VisibleRow), which compares each kind's author as that kind's entry states. The arm that binds
// it keeps only sources of kinds the registry holds (storedSQL), so no row of a kind without a
// predicate reaches it. The predicate is bound where the gate (authorOnlyGate) keeps the predicate
// of any of kinds (NodeKinds.gate), and not at all where it keeps none. It is bound whole, never
// behind the gate in SQL: it reads only columns of the row liveNode already holds, so the gate
// would spare it no probe and only add to every tuple its own test of the row's kind as the graph
// addresses it (graphstore.AddressKindSQL).
func liveVisibleNodeOf(kinds NodeKinds, kindExpr, address string) string {
	visible := "true"
	if bind, _ := kinds.gate(); bind {
		visible = VisibleRow("live", backlinkSeen)
	}
	return liveNode(kindExpr, address, visible)
}

// liveNode is the one read of the node's row that liveVisibleNode and liveVisibleNodeOf make: the
// row of the node kindExpr and address name, aliased live, whose state is not gone and which
// visible, a predicate over live, holds: true where the read binds no predicate of the node.
func liveNode(kindExpr, address, visible string) string {
	return `exists (select 1 from graph_nodes live
		where live.kind = ` + kindExpr + ` and live.id = ` + address + ` and live.state <> '` + graphstore.StateGone + `'
			and ` + visible + `)`
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
	// arm binds its visibility predicate over, where the gate keeps it (gatedKind), so an edge
	// whose source the acting person may not see raises no count, exactly as it is no row of the
	// list (readEdgeRows).
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
	// writes: projected is the node_kind column of the arm's own row, which the arm answers
	// only while it is null, and tupleKind the kind the store states the edge under, which is
	// the view's kind except for a block's membership of its document, stated as in_document
	// and rendered as part_of. projectedIn is the table that row is in, which the store's
	// reading joins (projectionCarries). An arm that names none of them reads its own table
	// alone, as every arm did before the store existed.
	projected   string
	tupleKind   string
	projectedIn string
	// moved is, for an arm over a column that may be updated in place while the row's node columns
	// are left as they were, the test that a row naming its node names a far end its node's tuple
	// does not: the arm counts such a row as naming no node (R9 of the issue-tree spec), as
	// graph_edges' old arm answers it. An issue's parent has one (store.IssueParentMoved), since
	// the release before's issue PATCH sets parent_key in place, and a component's parent the same
	// (store.ComponentParentMoved).
	moved string
	// storedFrom makes the arm the store's alone: the current tuples of kind out of a node of one
	// of these kinds, which graph_edges answers from the tuples with no projection row behind
	// them (its tuple arm's store class, migration 20261117020856). Such an arm reads no table
	// of its own, so it names no from, where, toID, fromKind or fromID (storedSQL).
	storedFrom []string
	// goesWithFourArms marks the arms of this table that count what graph_edges' arms over
	// comments' anchored_to and owned_by and over document_blocks' part_of count, three of the four
	// arms the boot step graphcensus.RunViewArmsStep deletes (the fourth, over block_refs, is
	// mentionBacklinkArms'). Once the view holds none of them (fourArmsDeleted) the arm is the
	// store's reading alone, with no read of its own table and no join to the row that names the
	// node, as the view's tuple arm answers the kind then.
	goesWithFourArms bool
}

// sql is the arm as the count reads it for target, keyed on the addresses bound as $1 and
// narrowed to the sources the acting credential may see. An arm over a kind the store writes
// is two reads, the table's and the store's, the first over the rows that name no node and the
// second over what the store holds, exactly as graph_edges holds the two arms it answers that
// kind from; an arm of the store alone is the store's read and nothing else (storedSQL).
func (a backlinkArm) sql(target backlinkTarget) string {
	if len(a.storedFrom) > 0 {
		return a.storedSQL(target)
	}
	deleted := a.goesWithFourArms && fourArmsDeleted.Load()
	if deleted {
		return a.tupleSQL(target, true)
	}
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
		if a.moved != "" {
			where += `(` + rowNamesNoNode(a.projected) + ` or ` + a.moved + `)`
		} else {
			where += rowNamesNoNode(a.projected)
		}
	}
	if where != "" {
		where += ` and `
	}
	read := `select ` + counted + ` from ` + a.from + join + ` where ` + where + keyed +
		` and ` + gatedKind(a.fromKind, a.fromID, backlinkSeen)
	if a.tupleKind == "" {
		return read
	}
	return read + ` union all ` + a.tupleSQL(target, false)
}

// tupleSQL is the arm's reading of the store: the current tuples of this arm's kind pointing at
// the addresses bound as $1, read through graph_tuples_as_of_in, which leads with the to side
// and the kind. Its last tests are the view's own three rules over the node the tuple comes
// from: an edge is the newest tuple of its own while that one asserts, it counts only while the
// projection row that names the node carries it (projectionCarries) - but for an arm that goes with
// the four, once deleted says the view holds none of them (fourArmsDeleted), which reads no row -
// and a node whose state is gone holds none, the last taken with the registry's predicate for that
// node kind out of one read of its row (liveVisibleNode), so a node the acting person may not see
// raises no count.
func (a backlinkArm) tupleSQL(target backlinkTarget, deleted bool) string {
	counted, join, keyed, address := target.read(`tuple.to_id`)
	where := `tuple.to_kind = '` + target.kind + `' and tuple.kind = '` + a.tupleKind +
		`' and tuple.from_kind = '` + a.fromKind + `'`
	if a.addressIs != "" {
		where += ` and ` + address + a.addressIs
	}
	carried := ""
	if !deleted {
		carried = ` and ` + projectionCarries(a.projectedIn, a.fromKind, "tuple")
	}
	return `select ` + counted + ` from graph_tuples tuple` + join + ` where ` + where +
		` and ` + keyed + ` and ` + currentTuple("tuple") + carried +
		` and ` + liveVisibleNode(a.fromKind, `tuple.from_id`)
}

// storedSQL is an arm of the store alone (storedFrom): the current tuples of the arm's kind
// pointing at the addresses bound as $1 out of a node of one of its kinds, read through
// graph_tuples_as_of_in as tupleSQL reads them, with the view's two rules over the node each comes
// from and no projection row to consult. A source is read under the kind the store keys it by
// and counted under the kind the graph addresses it by (graphstore.AddressKindSQL), so a finding
// of a lens, a block row whose id names its lens, counts as the lens_block the view's own arm
// writes it as, and a block arm counts no finding. Every kind of storedFrom is one this build
// resolves, so the count holds no edge whose node the list behind it cannot answer; and a source
// is counted only while it is not gone and the acting person may see it, by its own kind's
// predicate, bound through the gate over the kinds of storedFrom (liveVisibleNodeOf).
func (a backlinkArm) storedSQL(target backlinkTarget) string {
	counted, join, keyed, _ := target.read(`tuple.to_id`)
	addressed := graphstore.AddressKindSQL(`tuple.from_kind`, `tuple.from_id`)
	stored := make([]string, 0, len(a.storedFrom))
	for _, kind := range a.storedFrom {
		if kind := StoredKindOf(kind); !slices.Contains(stored, kind) {
			stored = append(stored, kind)
		}
	}
	return `select ` + counted + ` from graph_tuples tuple` + join +
		` where tuple.to_kind = '` + target.kind + `' and tuple.kind = '` + a.kind +
		`' and tuple.from_kind in (` + store.QuoteLiterals(stored) + `) and ` + addressed + ` in (` + store.QuoteLiterals(a.storedFrom) + `)` +
		` and ` + keyed + ` and ` + currentTuple("tuple") +
		` and ` + liveVisibleNodeOf(NodeKinds{kinds: a.storedFrom}, `tuple.from_kind`, `tuple.from_id`)
}

// authoredByReasoning is the arm that counts, on a person's or a session's node, the authored_by
// tuples from a node of the kinds cybermonklabs/legion#4193 adds, which the view answers from the
// tuples alone (TupleKinds), a finding's as the lens_block its own arm writes. The same tuple out
// of a comment is no edge of the view, so it is none of these, and a source kind reaches the count
// in the commit that registers it.
var authoredByReasoning = backlinkArm{kind: graphstore.TupleAuthoredBy, storedFrom: []string{
	graphstore.KindConstraint, graphstore.KindInteraction, graphstore.KindLens, graphstore.KindLensBlock, graphstore.KindRisk,
}}

// lensStandsOn is the arm that counts, on a node a lens may be invoked on, the part_of tuple of
// each lens invoked on it: what stands on the node raises its count, as R1 names the backlink
// counts among the reads that leave a hidden node out. P9 lets a lens stand on any node under an
// issue - the issue, a document and its blocks, an ask, a comment or a message and its blocks,
// another lens, a finding, a risk or a constraint - so each of those kinds holds this arm, keyed on
// the lens as the near end of the tuple. The arms that count a lens's own children, its findings
// and its risks, are the lens kind's, and a constraint's place under its issue is the issue's, so
// no tuple is counted by two arms.
var lensStandsOn = backlinkArm{kind: graphstore.TuplePartOf, storedFrom: []string{graphstore.KindLens}}

// changedByRun is the arm that counts, on a run's interaction node, the changed_by_run tuple of
// each node a write made for that run created or changed (spec requirement r-interaction),
// which the view answers from the tuples alone, out of any node. A node of each kind the store
// holds and a write made for a run can mark is a source here: an issue, a document, an ask, a
// comment or a message, a document's block, and a lens, a finding of one, a risk or a constraint.
// The author's own person or session node is never marked, so neither is a source. The tuple
// points away from the node that changed, so it raises the count of the run alone.
var changedByRun = backlinkArm{kind: graphstore.TupleChangedByRun, storedFrom: []string{
	graphstore.KindArtifact, graphstore.KindAsk, BlockNodeKind, graphstore.KindComment, graphstore.KindConstraint, graphstore.KindIssue,
	graphstore.KindLens, graphstore.KindLensBlock, graphstore.KindMessage, graphstore.KindRisk,
}}

// filedFrom is the arm that counts, on an ask, a comment or a message, the filed_from tuple of each
// issue filed out of it, which the view answers from the store alone (R9 of the issue-tree spec).
var filedFrom = backlinkArm{kind: graphstore.TupleFiledFrom, storedFrom: []string{graphstore.KindIssue}}

// backlinkArms is, for each node kind nodeRegistry resolves, every arm of graph_edges whose edge
// kind is a literal and whose target is a node of that kind, and each edge kind the view's tuple
// arm answers into that kind from the store alone, as an arm of that kind (storedFrom).
// BacklinkCounts refuses a kind that is no key of this table, so a node kind a newer build adds
// to the graph is counted only once its arms are declared here; a role and a page are present
// and empty, because no arm of the view but the mention arms targets either, and an entry that is
// present but empty is counted from the mention arms alone rather than refused. Tests outside this
// package count the same kinds from a copy of these keys (refstest.BacklinkKinds), which
// TestRefstestBacklinkKindsAreTheArmTablesKinds holds equal to them.
//
// Each arm reads only the relations it needs to reach the address it points at and leaves out
// the joins the view makes only to date an edge. Dropping them preserves every row:
// components.snapshot_id (0037 line 38), component_depends' (project_key, from_id) and
// (project_key, to_id) foreign keys (0037 lines 47-48) and issue_component_members.issue_key
// (0038 line 21) are each a not-null foreign key to the primary key the view joins, so each of
// those joins finds exactly one row.
var backlinkArms = map[string][]backlinkArm{
	"issue": {
		// issues_parent_key, partial on parent_key is not null, over the rows that name no node
		// and the rows whose parent the release before moved in place (store.IssueParentMoved);
		// the store states the same fact as the issue's child_of, answered while the issue's row
		// carries its node and names that parent (migration 20261117140000).
		{kind: graphstore.TupleChildOf, from: `issues i`, where: `i.parent_key is not null`, toID: `i.parent_key`,
			fromKind: "issue", fromID: `i.key`, projected: `i.node_kind`, moved: store.IssueParentMoved,
			tupleKind: graphstore.TupleChildOf, projectedIn: "issues"},
		// issue_links_target (target_key, kind), over the rows that name no node; the store
		// states the wait as the issue's needs, which the view names blocked_by through this
		// release, answered while the issue_links row of the pair carries its node.
		{kind: "blocked_by", from: `issue_links l`, where: `l.kind = 'blocked_by'`, toID: `l.target_key`,
			fromKind: "issue", fromID: `l.issue_key`, projected: `l.node_kind`, tupleKind: graphstore.TupleNeeds, projectedIn: "issue_links"},
		// artifacts_issue_key, partial on issue_key is not null, over the rows that name no node,
		// but for the issue's own tree and decisions query (issueOwnArtifact).
		{kind: TupleAttachedTo, from: `artifacts a`, where: `a.issue_key is not null and not ` + issueOwnArtifact("a"), toID: `a.issue_key`,
			fromKind: "artifact", fromID: `a.ref_key`, projected: `a.node_kind`},
		// The same index, over the rows that carry their node while its part_of to that issue
		// stands: the view's arm of its own for them, which names the artifact by its uuid. It
		// leaves out the issue's own tree and decisions query too.
		{kind: TupleAttachedTo, from: `artifacts a`, where: `a.issue_key is not null and not ` + issueOwnArtifact("a") + ` and ` + attachedArtifactStands, toID: `a.issue_key`,
			fromKind: "artifact", fromID: `a.ref_key`},
		// graph_tuples_as_of_in: a lens invoked on the issue, and a constraint written under it,
		// each placed there by its part_of tuple (lensStandsOn).
		{kind: graphstore.TuplePartOf, storedFrom: []string{graphstore.KindConstraint, graphstore.KindLens}},
		// graph_tuples_inbound: an issue's duplicate_of the issue it repeats, which the store
		// holds alone.
		{kind: graphstore.TupleDuplicateOf, storedFrom: []string{graphstore.KindIssue}},
	},
	// The view addresses an artifact by artifacts.ref_key, so each arm starts at artifacts
	// through the unique artifacts_ref_key_key and joins the table that points at it.
	"artifact": {
		// asks_anchor_artifact, on the same coalesce the join makes; the store states the same
		// fact as an ask's anchored_to, answered while the ask's row carries its node. It goes
		// with graph_edges' arm over asks, and the store's reading of an ask's anchored_to then
		// joins no asks row, as the view's tuple arm does then.
		// kept for one release: asks-arms
		{kind: "anchored_to", from: `artifacts a join asks k on coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid) = a.id`, toID: `a.ref_key`,
			fromKind: "ask", fromID: `k.id::text`, projected: `k.node_kind`, tupleKind: graphstore.TupleAnchoredTo, projectedIn: "asks"},
		// comments_anchor_artifact, on the same cast the join makes; the store states the same
		// fact as a comment's anchored_to, answered while the comment's row carries its node. It
		// goes with the view's arm over comments (goesWithFourArms).
		{kind: "anchored_to", from: `artifacts a join comments c on (c.anchor->>'artifact_id')::uuid = a.id`, toID: `a.ref_key`,
			fromKind: graphstore.KindComment, fromID: `c.id::text`, projected: `c.node_kind`, tupleKind: graphstore.TupleAnchoredTo, projectedIn: "comments",
			goesWithFourArms: true},
		// asks_artifact_id, and the ask's owned_by in the store, which go as an ask's anchored_to does.
		// kept for one release: asks-arms
		{kind: "owned_by", from: `artifacts a join asks k on k.artifact_id = a.id`, toID: `a.ref_key`,
			fromKind: "ask", fromID: `k.id::text`, projected: `k.node_kind`, tupleKind: graphstore.TupleOwnedBy, projectedIn: "asks"},
		// comments_artifact_id, and the comment's owned_by in the store. It goes as the arm over
		// comments' anchored_to does.
		{kind: "owned_by", from: `artifacts a join comments c on c.artifact_id = a.id`, toID: `a.ref_key`,
			fromKind: graphstore.KindComment, fromID: `c.id::text`, projected: `c.node_kind`, tupleKind: graphstore.TupleOwnedBy, projectedIn: "comments",
			goesWithFourArms: true},
		// The document_blocks primary key (artifact_id, block_id). The store states a block's
		// membership of its document as in_document, which the view renders as this part_of,
		// answered while the block's own row carries its node. It goes with the view's arm over
		// document_blocks (goesWithFourArms).
		{kind: "part_of", from: `artifacts a join document_blocks d on d.artifact_id = a.id`, toID: `a.ref_key`,
			fromKind: "block", fromID: `d.node_id`, projected: `d.node_kind`, tupleKind: TupleInDocument, projectedIn: "document_blocks",
			goesWithFourArms: true},
		// graph_tuples_as_of_in: a lens invoked on the document, its part_of at the document's
		// ref_key; a block's membership is in_document, so this arm counts none of it.
		lensStandsOn,
	},
	"ask": {
		// comments_ask_id_text.
		{kind: "replies_to", from: `comments c`, where: `c.ask_id is not null`, toID: `c.ask_id::text`,
			fromKind: "comment", fromID: `c.id::text`},
		// ask_blocks_ask_id_text. The view's arm counts a current part only, so this one carries
		// its position filter.
		{kind: "part_of", from: `ask_blocks b`, where: `b.position is not null`, toID: `b.ask_id::text`,
			fromKind: "ask_block", fromID: `b.node_id`},
		lensStandsOn,
		filedFrom,
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
		lensStandsOn,
		filedFrom,
	},
	"message": {
		// messages_in_reply_to_text.
		{kind: "replies_to", from: `messages m`, where: `m.in_reply_to is not null`, toID: `m.in_reply_to::text`,
			fromKind: "message", fromID: `m.id::text`},
		// message_blocks_message_id_text, with the view's position filter.
		{kind: "part_of", from: `message_blocks b`, where: `b.position is not null`, toID: `b.message_id::text`,
			fromKind: "message_block", fromID: `b.node_id`},
		lensStandsOn,
		filedFrom,
	},
	"component": {
		// components_parent_node, on the same expression the arm selects, over the rows that
		// name no node and the rows whose parent was moved in place (store.ComponentParentMoved);
		// the store states the same fact as the component's part_of, answered while the
		// component's row carries its node and names that parent (migration 20261118023219).
		{kind: graphstore.TuplePartOf, from: `components c`, where: `c.parent is not null`, toID: `c.project_key || '/' || c.parent`,
			fromKind: graphstore.KindComponent, fromID: `c.project_key || '/' || c.id`, projected: `c.node_kind`,
			moved: store.ComponentParentMoved, tupleKind: graphstore.TuplePartOf, projectedIn: "components"},
		// component_depends_to_node, over the rows that name no node; the store states the
		// dependency as the component's depends_on, answered while the row of the pair carries
		// its node.
		{kind: graphstore.TupleDependsOn, from: `component_depends d`, toID: `d.project_key || '/' || d.to_id`,
			fromKind: graphstore.KindComponent, fromID: `d.project_key || '/' || d.from_id`, projected: `d.node_kind`,
			tupleKind: graphstore.TupleDependsOn, projectedIn: "component_depends"},
		// issue_component_members_node, over the rows that name no node; the store states the
		// attachment as the issue's affects, answered while the row of the issue and the
		// component carries the issue's node.
		{kind: graphstore.TupleAffects, from: `issue_component_members m`, toID: `m.project_key || '/' || m.component_id`,
			fromKind: graphstore.KindIssue, fromID: `m.issue_key`, projected: `m.node_kind`,
			tupleKind: graphstore.TupleAffects, projectedIn: "issue_component_members"},
	},
	"session": {
		// ask_followers_session_id (migration 20261101182419), which exists for this arm; the
		// store states the same fact as an ask's followed_by, answered while the follower row of
		// that ask and that session carries the ask's node (projectionCarries' session test). It
		// goes as the artifact's arms over asks do.
		// kept for one release: asks-arms
		{kind: "followed_by", from: `ask_followers f`, toID: `f.session_id`,
			fromKind: "ask", fromID: `f.ask_id::text`, projected: `f.node_kind`, tupleKind: graphstore.TupleFollowedBy, projectedIn: "ask_followers"},
		// graph_tuples_as_of_in: what a session wrote of the kinds the view answers its
		// authored_by from.
		authoredByReasoning,
	},
	// A person is pointed at by mentions and by the authored_by tuples the view answers from the
	// store (authoredByReasoning); a role and a page by mentions alone. The other tuples the
	// store holds of them - a comment's authored_by, delivered_to, held_by and part_of - are no
	// arm of graph_edges, so no count reads one. The two empty entries are present so their kind
	// is counted from the mention arms rather than refused.
	"person": {authoredByReasoning},
	"role":   {},
	"page":   {},
	// A kind of the lens catalogue is pointed at by the has_kind tuple of each lens of that kind,
	// and a constraint by the constrained_by tuples of each requirement block held to it and of
	// the lens kind whose source it is, and by the part_of of a lens invoked on it: the view
	// answers each from the tuples alone, so each is an arm of the store alone, counted through the
	// predicate of the kind its tuple comes from.
	graphstore.KindLensKind: {{kind: graphstore.TupleHasKind, storedFrom: []string{graphstore.KindLens}}},
	graphstore.KindConstraint: {
		{kind: graphstore.TupleConstrainedBy, storedFrom: []string{BlockNodeKind, graphstore.KindLensKind}},
		lensStandsOn,
	},
	// A lens is pointed at by the part_of tuples of what stands under it - its findings and its
	// risks, and a lens invoked on it - and by the triggered_by tuple of each lens it triggered. A
	// triggered_by joins two lenses, so it is counted once, on the lens it points at: the lens that
	// triggered, whose count says how many lenses its findings invoked. A finding and a risk are
	// pointed at by the part_of of a lens invoked on them. Each is an arm of the store alone, the
	// view's tuple arms answering every one of them, a finding's part_of as the lens_block its own
	// arm writes (migration 20261117020856).
	graphstore.KindLens: {
		{kind: graphstore.TuplePartOf, storedFrom: []string{graphstore.KindLens, graphstore.KindLensBlock, graphstore.KindRisk}},
		{kind: graphstore.TupleTriggeredBy, storedFrom: []string{graphstore.KindLens}},
	},
	graphstore.KindLensBlock: {lensStandsOn},
	graphstore.KindRisk:      {lensStandsOn},
	// A run's interaction node is pointed at by the changed_by_run tuple of each node a write made
	// for the run changed (changedByRun), an arm of the store alone. Its own authored_by points
	// away from it, at its author, whose count holds it (authoredByReasoning).
	graphstore.KindInteraction: {changedByRun},
	// No arm of the view whose edge kind is a literal targets a document's block: such a block
	// is pointed at by mentions, a reply naming the block of an ask, a comment or a message
	// instead, and by the tuples of the store alone - a lens invoked on it, and the prevented_by of
	// each risk its check covers.
	"block": {lensStandsOn, {kind: graphstore.TuplePreventedBy, storedFrom: []string{graphstore.KindRisk}}},
	// A reply names the block it answers by node id, and the prefix of that id is what tells an
	// ask's part from a comment's block, so each of these two arms carries the test the view's
	// own arm carries. Both read comments_replies_to_block, partial on replies_to_block is not
	// null, and neither carries a position filter, as the view's arms do not. A lens invoked on an
	// item's block points at it too.
	"comment_block": {
		{kind: "replies_to", from: `comments c`, where: `c.replies_to_block is not null`, addressIs: ` not like 'ask/%'`, toID: `c.replies_to_block`,
			fromKind: "comment", fromID: `c.id::text`},
		lensStandsOn,
	},
	"ask_block": {
		{kind: "replies_to", from: `comments c`, where: `c.replies_to_block is not null`, addressIs: ` like 'ask/%'`, toID: `c.replies_to_block`,
			fromKind: "comment", fromID: `c.id::text`},
		lensStandsOn,
	},
	"message_block": {
		// messages_replies_to_block, partial on replies_to_block is not null.
		{kind: "replies_to", from: `messages m`, where: `m.replies_to_block is not null`, toID: `m.replies_to_block`,
			fromKind: "message", fromID: `m.id::text`},
		lensStandsOn,
	},
}

// mentionBacklinkArms is the arms of graph_edges whose edge kind and target kind are columns
// rather than literals: refs, the four tables of block references, and the store's own tuples,
// which hold a document's own mentions where its row names its node and the links of a document
// block whose row names its node. Every node kind gets those seven, since a mention can name any
// node; a document's block gets an eighth, an answered ask's shaped, which points at a block
// alone. The target kind is written as a literal (see BacklinkCounts), and excludeKinds the
// caller's excluded edge kinds, bound as $4 - on these arms the edge kind is a column, so an
// exclusion is a predicate and never drops an arm, and an empty list adds nothing.
//
// Each arm carries the visibility predicate of the node its mention comes from, bound through the
// gate (authorOnlyGate): only where that node can be reserved for its author. The refs arm's
// source kind is a column of the kinds a refs row's source can be (refsSources: a document, an
// ask, a comment or a message, none of them standing), which the store reserves for nobody, so it
// binds none (gatedEnd), and neither does the store's reading of a document's mentions
// (liveVisibleNode); a document block's arms bind the block predicate (gateKeeps); and the arms
// over an ask's, a comment's or a message's blocks bind none (gatedKind). The arms over the
// store's tuples test the source's state on every arm (liveVisibleNode).
func mentionBacklinkArms(target backlinkTarget, excludeKinds []string) []string {
	deleted := fourArmsDeleted.Load()
	exclude := ""
	if len(excludeKinds) > 0 {
		exclude = ` and not (r.kind = any($4))`
	}
	counted, join, keyed, _ := target.read(`r.to_id`)
	// rest is the arm's own tests of its row, and source its test of the node its mention comes
	// from; each is a predicate, source true where it makes none.
	arm := func(relation, rest, source string) string {
		return `select ` + counted + ` from ` + relation + join + `
			where r.to_kind = '` + target.kind + `' and ` + keyed + ` and ` + rest + exclude + ` and ` + source
	}
	arms := []string{
		// refs_to (to_kind, to_id), over the rows the store states no tuple of
		// (store.ReferenceNotStated): every source's but a document's whose row names its node,
		// and a far end past the store's bound. heldByABlock belongs on this arm and the store's
		// reading of the same mentions below alone: it is gated on kind = 'mentions' and
		// from_kind in artifact, comment, message, ask, and those two are the only arms of the
		// view that can emit that pair, every other arm fixing from_kind as a literal that is none
		// of the four. Its exists probes carry no position filter, as the view's readers do not.
		// The source-kind filter is this arm's alone for the same reason: every other arm's
		// from_kind is a literal, or one of a list of literals (storedSQL), of a kind nodeRegistry
		// resolves, so the filter would drop nothing there.
		arm(`refs r`, `r.from_kind = any($2) and `+store.ReferenceNotStated+` and not `+heldByABlock("r"),
			gatedEnd(refsSources, `r.from_kind`, `r.from_id`, false, backlinkSeen)),
		// The mentions of a document whose row names its node, in the store, read through
		// graph_tuples_inbound: each current mentions tuple out of the document's node, joined to
		// the artifacts row naming that node by its ref_key, counted only while the refs row of the
		// pair stands, the document named by its uuid, as the view's tuple arm reads it
		// (store.GraphEdgesArtifactRefs). A block of the document holding a link to the same target
		// is counted in the mention's place, as the refs arm's heldByABlock says, of the document the
		// row names; its own test of the document is the view's state rule and this registry's
		// predicate made out of one read of the document's node row (liveVisibleNode).
		arm(`graph_tuples r join artifacts mentioning on mentioning.ref_key = r.from_id and mentioning.node_kind is not null`,
			`r.from_kind = '`+graphstore.KindArtifact+`' and r.kind = '`+graphstore.TupleMentions+`' and `+currentTuple("r")+`
			and exists (select 1 from refs projected
				where projected.from_kind = r.from_kind and projected.from_id = mentioning.id::text
					and projected.to_kind = r.to_kind and projected.to_id = r.to_id)
			and not `+documentBlockHolds("r", "mentioning.id::text"),
			liveVisibleNode(graphstore.KindArtifact, `r.from_id`)),
	}
	// block_refs_to_source (to_kind, to_id, (artifact_id::text)), by its first two columns,
	// over the rows that name no node. The view's own arm lists a block[] pointer's row only
	// while block_ref_listed holds, the types test a typed block's pointer carries, and a row
	// with no limit is listed without the call; this arm repeats both, so the count is the
	// rows the list shows. A pointer whose target is retyped out of its allowed types leaves
	// the count the moment it leaves the list, with no rewrite of either. It goes with the view's
	// arm over block_refs, which the step of graphcensus.RunViewArmsStep deletes (fourArmsDeleted).
	if !deleted {
		arms = append(arms, arm(`block_refs r`, `(r.to_types is null or block_ref_listed(r.to_types, r.to_id))
			and `+rowNamesNoNode(`r.node_kind`),
			gatedKind("block", `r.from_node`, backlinkSeen)))
	}
	// The same links in the store, read through graph_tuples_as_of_in: every current
	// relation a block states but its membership of its document, which is the artifact's
	// own part_of arm, and its placement under the block holding it, which is no edge of
	// the view, each counted only while the block_refs row of its pair carries the block's
	// node (blockRefCarries), as the view's tuple arm joins it, and once the view holds no arm
	// over block_refs only while its kind is one a block_refs row projects (blockLinkCarried).
	// The types test is the view's
	// tuple arm's: block_node_listed with the attribute node's targets as its input
	// (migration 20261125150001), which a boot step restates to read the target's type from
	// its node (graphcensus.RunTypedBlockSupertagsStep), and a mention is no attribute of any
	// block type, so it is listed without the lookup. Its own test of the citing block is the
	// view's state rule and this registry's predicate made out of one read of that block's
	// node row (liveVisibleNode).
	arms = append(arms, arm(`graph_tuples r`, `r.from_kind = 'block' and r.kind <> '`+TupleInDocument+`'
			and r.kind <> '`+graphstore.TuplePartOf+`' and `+currentTuple("r")+`
			and (r.kind = 'mentions' or block_node_listed(block_pointer_targets(r.from_id, r.kind), r.to_id))
			and `+blockLinkCarried(BlockNodeKind, "r", deleted),
		liveVisibleNode("block", `r.from_id`)))
	// An answered ask's shaped, its tie to a block of its issue, which the store states from the
	// ask's node and the view's tuple arm answers while the block_refs row of the pair, marked with
	// that node, stands (migration 20261116094101). An ask's shaped declares no targets, so it
	// carries no types test.
	if target.kind == BlockNodeKind {
		arms = append(arms, arm(`graph_tuples r`, `r.from_kind = '`+graphstore.KindAsk+`' and r.kind = '`+TupleShaped+`'
			and `+currentTuple("r")+` and `+blockLinkCarried(graphstore.KindAsk, "r", deleted),
			liveVisibleNode(graphstore.KindAsk, `r.from_id`)))
	}
	// One arm per item kind over the links its blocks hold: <kind>_block_refs_to
	// (to_kind, to_id, (<item>::text)), then the blocks table's primary key for the join. The
	// join keeps the view's position filter (itemBlockLinks, itemBlockCurrent): a link a block
	// holds is listed while that block is one of the item's current ones, and the count copies
	// the view's answer rather than correcting it.
	for _, item := range itemKinds {
		arms = append(arms, arm(itemBlockLinks(item), itemBlockCurrent,
			gatedKind(item.BlockKind(), `r.from_node`, backlinkSeen)))
	}
	return arms
}
