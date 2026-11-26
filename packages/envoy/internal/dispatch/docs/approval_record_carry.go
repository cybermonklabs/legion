package docs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The record carry (R14). A version whose record digest equals the digest of the approved version,
// as a paragraph moved in the text before a requirement leaves it, carries the approval the same
// way every other carry does: a carried review at the new version, its artifact.approved before
// the version's artifact.version. Without it Dispatch's gate reads the query approved by its digest
// while the Legion daemon, which reads an approval at latest_version alone, reads the request
// handed back.
//
// A write of a document's text does not know its new version's record digest before its batch:
// the record is built from the store once the batch's tuples stand, so the digest is owed and the
// fill records it after the COMMIT (fillOwedDigests). The write decides the carry in its read phase
// from the facts it states (recordUnmoved), and must never be wrong: a carry over a moved record
// publishes an approval nobody gave. A carry it passes over is safe, since the gate still reads the
// digest, and a later reader of the stored digest carries it (followLatestVersion, the approval
// request's CarryApprovalToLatest, the drain's carryAppendedResults). The fill holds the write's
// decision to its claim (fillOwedDigests): a record carry whose version digests otherwise is left
// unpinned, so every gate reads it stale.

// recordCarryEnd closes the reason of a record carry, after the opening every carried reason has
// and the version the person approved (RecordCarryReason). It is the one spelling the reason's
// writer and its reader (IsRecordCarry) share.
const recordCarryEnd = ": this version changes nothing that approval covers."

// RecordCarryReason is the reason of a record carry of the approval of version origin
// (approvalOrigin): CarriedReviewReasonPrefix, the opening the Legion daemon matches, the version
// and recordCarryEnd.
func RecordCarryReason(origin int) string {
	return CarriedReviewReasonPrefix + strconv.Itoa(origin) + recordCarryEnd
}

// IsRecordCarry reports whether reason is a record carry's, byte for byte as RecordCarryReason
// writes it: the fill holds such a carry to the digest its approval stood on (fillOwedDigests).
func IsRecordCarry(reason string) bool {
	number, opened := strings.CutPrefix(reason, CarriedReviewReasonPrefix)
	number, closed := strings.CutSuffix(number, recordCarryEnd)
	origin, err := strconv.Atoi(number)
	return opened && closed && err == nil && RecordCarryReason(origin) == reason
}

// RecordCarry is what the record carry of a version weighs it by (recordUnmoved), handed to
// carryNewestApprovalNaming by the call site that knows it: every fact the version's write states
// before its version's event, which the call site vouches for whole (StatedWhole); or the version's
// record digest, known to its writer (the drain's, knownRecord); or that the version stands stored
// with its record's digest (storedRecord, followLatestVersion and the approval request's carry).
// The zero RecordCarry runs no record case: a call site that cannot hand the rule every fact its
// write states hands it.
type RecordCarry struct {
	stated []graphstore.Uncaused
	whole  bool
	digest *string
	stored bool
}

// StatedWhole is the RecordCarry of a version whose write states, before its version's event,
// stated and nothing else of the store's tuples a query's record reads (recordUnmoved's comment
// names what else such a write states, and why none of it moves a record).
func StatedWhole(stated ...graphstore.Uncaused) RecordCarry {
	return RecordCarry{stated: stated, whole: true}
}

// knownRecord is the RecordCarry of a version whose record digest is digest, known to its writer
// in its read phase (the drain's, writeRevisions).
func knownRecord(digest string) RecordCarry {
	return RecordCarry{digest: &digest}
}

// storedRecord is the RecordCarry of a version already stored, whose version_renders row holds its
// record's digest where a pass stated it.
func storedRecord() RecordCarry {
	return RecordCarry{stored: true}
}

// runs reports whether the record case is weighed at all.
func (record RecordCarry) runs() bool {
	return record.whole || record.stored || record.digest != nil
}

// versionWriteRecord is the RecordCarry of a version writeVersionTx writes outside an issue room's
// settlement, blocks being what its pass and its node stated (versionWriteResult.blocks): every
// fact the write states before its version's event, but for two writes, which hand none. The
// decisions step's own write (steps) states decisions and restores asks through writes of its own.
// A move of the node route (Ledger.storedBlocks, HoldStoredBlock) states its block's part_of, rank
// and placement outside the pass (api's queueNodeMove), so a container it moves can change the
// walk's order with no fact of the pass about it.
func versionWriteRecord(ctx context.Context, steps decisionStepCases, blocks graphstore.Uncaused) RecordCarry {
	if steps.any() {
		return RecordCarry{}
	}
	if ledger := ledgerFrom(ctx); ledger != nil && len(ledger.storedBlocks) > 0 {
		return RecordCarry{}
	}
	return StatedWhole(blocks)
}

// roomPassRecord is the RecordCarry of each version an issue room's settlement writes: the facts
// of the room's one pass (indexRoom), of every document of the room and of the room's own
// references, since a block written into one document can move the record of another document's
// query (a requirement written into the plan moves the spec's).
func roomPassRecord(index roomIndex) RecordCarry {
	stated := make([]graphstore.Uncaused, 0, len(index.facts)+1)
	for _, document := range slices.Sorted(maps.Keys(index.facts)) {
		stated = append(stated, index.facts[document])
	}
	return StatedWhole(append(stated, index.references)...)
}

// recordCarryForcedKey marks the context of a write whose record carry answers that its version's
// record is the approved one once the approval stands on the version before it, without weighing
// the write's facts: the one way to make the fill's backstop fire (fillOwedDigests), which only
// this package's tests put on a context. No route can.
type recordCarryForcedKey struct{}

// recordCarryForced reports whether ctx holds recordCarryForcedKey.
func recordCarryForced(ctx context.Context) bool {
	forced, _ := ctx.Value(recordCarryForcedKey{}).(bool)
	return forced
}

// recordStandStatement reads, for the record carry of a version of the document $1, in one
// statement: the document's issue; the query of its node where that node is a query node; whether
// the issue has converted (issue_trees); the record digest the version_renders row of version $2
// holds where it owes none and holds no tree_mismatch; the digest the review $3 stands on, which
// every gate compares (ReviewStandColumns): its row of artifact_review_carries' record_sha256 where
// that holds one, else its content_sha256, $4; and whether the issue's queries hold a mark
// (issue_query_marks).
var recordStandStatement = `
	select coalesce(a.issue_key, ''), coalesce(q.attributes->>'` + graphstore.AttributeQuery + `', ''),
	       exists (select 1 from issue_trees t where t.issue_key = a.issue_key),
	       (select r.record_sha256 from version_renders r
	        where r.artifact_id = a.id and r.number = $2 and not r.digest_owed and not r.tree_mismatch),
	       coalesce((select c.record_sha256 from artifact_review_carries c where c.review_id = $3::uuid), $4::text),
	       exists (select 1 from issue_query_marks m where m.issue_key = a.issue_key)
	from artifacts a
	left join graph_nodes q on q.kind = '` + graphstore.KindArtifact + `' and q.id = a.ref_key and q.supertag = '` + graphstore.SupertagQuery + `'
	where a.id = $1`

// recordStand is what recordStandStatement reads.
type recordStand struct {
	issue, query string
	converted    bool
	digest, pin  *string
	marked       bool
}

// readRecordStand reads recordStandStatement for version number of artifactID and newest, the
// approval carried; a document with no row reads as none.
func readRecordStand(ctx context.Context, tx pgx.Tx, artifactID string, number int, newest model.ArtifactReview) (recordStand, error) {
	var review *string
	if newest.ID != "" {
		review = &newest.ID
	}
	var stand recordStand
	err := tx.QueryRow(ctx, recordStandStatement, artifactID, number, review, newest.ContentSHA256).Scan(
		&stand.issue, &stand.query, &stand.converted, &stand.digest, &stand.pin, &stand.marked)
	if errors.Is(err, pgx.ErrNoRows) {
		return recordStand{}, nil
	}
	if err != nil {
		return recordStand{}, fmt.Errorf("read the record digest the approval of version %d of %s stands on: %w", newest.Version, artifactID, err)
	}
	return stand, nil
}

// approvalPin is the digest newest, an approval, stands on, as every gate reads it
// (ReviewStandColumns): the record digest its row of artifact_review_carries holds where newest is a
// carry whose row holds one, read in one statement, and its content_sha256 elsewhere, read off the
// review itself.
func approvalPin(ctx context.Context, tx pgx.Tx, newest model.ArtifactReview) (*string, error) {
	if newest.CarriedFromVersion == nil || newest.ID == "" {
		return newest.ContentSHA256, nil
	}
	var pin *string
	if err := tx.QueryRow(ctx, `
		select coalesce((select c.record_sha256 from artifact_review_carries c where c.review_id = $1::uuid), $2::text)`,
		newest.ID, newest.ContentSHA256).Scan(&pin); err != nil {
		return nil, fmt.Errorf("read the digest the approval of version %d of %s stands on: %w", newest.Version, newest.ArtifactID, err)
	}
	return pin, nil
}

// carryUnmovedRecord is carryNewestApprovalNaming's record case: where version number of artifactID
// has the record newest stands on (recordUnmoved, weighed by record), it carries newest to it as
// every batch carry does (writeCarriedReview), its reason naming the version the person approved
// (approvalOrigin, RecordCarryReason). A version whose digest is owed holds the carried review
// pinned to its stored markdown's digest until the fill repins it on its row of
// artifact_review_carries, where the version's record is the one the approval stood on
// (fillOwedDigests).
func carryUnmovedRecord(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, number int, newest model.ArtifactReview, serverURL string, record RecordCarry, sink func(model.Event)) error {
	unmoved, err := recordUnmoved(ctx, tx, artifactID, number, newest, record)
	if err != nil || !unmoved {
		return err
	}
	origin, err := approvalOrigin(ctx, tx, artifactID, newest)
	if err != nil {
		return err
	}
	return writeCarriedReview(ctx, tx, broker, artifactID, number, origin, newest, RecordCarryReason(origin), serverURL, sink)
}

// recordUnmoved reports whether version number of artifactID, which the write of tx makes, or
// which stands written, has the record of the version newest, the document's newest review and an
// approval, stands on: the record case of carryNewestApprovalNaming, which runs once the restore
// and carryVerdict's cases carried nothing (R14). record says what the call site knows of the
// version (RecordCarry), and the zero RecordCarry answers false.
//
// A version whose digest is known answers by its digest alone: the drain's (knownRecord) carries
// where that digest is the one newest stands on, the digest every gate compares (its carry row's
// record_sha256, else its content_sha256); a stored version (storedRecord) carries where its
// version_renders row holds that digest and the issue holds no query mark.
//
// A version its write makes, whose digest is owed until the fill after the COMMIT, carries where
// all of this holds, each refusal made as cheaply as it can be, in this order.
//
//	A. The call site vouches for every fact its write states before the version's event
//	   (StatedWhole), and a pass of tx stated this very version (Ledger.stated,
//	   recordStatedVersions): a deferred pass (indexDeferredVersion), a room left owed
//	   (indexRoomLeftOwed) and a pass that lost its fence record none, and carry nothing here.
//	   No statement.
//	P1, P2 (recordStandStatement, one statement). The version before it, L, holds a record digest
//	   that is newest's pin; the issue holds no query mark, so no write moved a record of it since
//	   L without a version. Every writer of a mark holds the issue's owner lock, as tx does, so none
//	   appears under it. The document is a query node of a converted issue, of the spec, the
//	   Verification or the plan: the decisions query takes asks by their conversation rows, which
//	   writers outside any version write make.
//	C. No node the write's facts write or state out of can be taken by the query's types once those
//	   facts stand (statedFacts.mayTake): an edit of a requirement stops here. No statement.
//	D. The walk of the issue's tree at the head (WalkIssueTree), T the nodes the query takes in it,
//	   A every ancestor of a node of T below the issue.
//	E. No fact of the write is out of a node of T (R1); none is a node write, a part_of, a rank or a
//	   state out of a node of A (R2); no node of T is of the walk's second part; no node of T or A
//	   is an ask, a conversation node or a block of one; no node the write's facts reach can stand
//	   in the walk after the write as a type the query takes (R3, a callout counted for the spec
//	   whether or not it is the memo); for the spec, the issue's first child stays the node it is
//	   (R4, statedFacts.keepsFirstChild); and no node outside the head's walk that the write brings
//	   into it holds a child at the head (refs.TreeChildren, one statement where there is one), so
//	   no subtree the write does not state joins the walk.
//	F. The record of the query at the head digests to newest's pin (IssueTree.QueryRecord, one
//	   statement), so no write that left no mark moved it since L.
//
// Soundness. F gives the head's record the approved one; what is left is that the record at the
// version's cut - the cut the batch's version_renders row takes after every step
// (versionRenderCutStatement) - is the head's. The store at that cut is the head and the write's
// tuples, so of each node's attribute or relation it holds either the head's value or one the
// write states, whichever is newest by its time and its seq; the rule takes every such choice.
// The record writes seven fields of each node of T, in the walk's order (IssueTree.QueryRecord):
//
//   - its address: fixed;
//   - its type, refs.TreeNodeType of its supertag and the opening of its text value: only a node
//     write or a text value out of the node moves either (R1);
//   - its parent's address, its newest standing part_of: a tuple out of the node (R1); a comment's
//     loader names its parent, and no node of T or A is a conversation node;
//   - its rank, its values and its text: values out of the node (R1);
//   - its relations: tuples out of the node (R1); a tuple into a node of T is no field of its
//     record.
//
// Which nodes the query takes: by type, R1 keeps each node of T's type and R3 gives no other node
// the walk holds after the write a type the query takes; a node the write states nothing of keeps
// its type and its place, and comes into the walk only under a node the write brings into it, which
// holds no child at the head. The memo, the spec's first child where it is a callout: R4 keeps
// that child, and R1 or R3 its type. A result, by its parent's type: a parent of T is a check of
// T, which R1 keeps, and no other node becomes a check (R3). A view: ViewOf reads the issue's row,
// which no version write moves (markOwnSpecIndexed writes only where ViewOf answers no view).
//
// The order of T: the walk takes each node before its children, so two nodes of T stand in the
// order their lowest common ancestor's children on their two paths stand in, each of them a node
// of T or A. Those children sort by rank, then the lower address, and the unranked by made_at,
// then the lower address (treeWalk.ordered): made_at never moves (graphstore.Node), a rank and a
// standing move only by the facts R1 and R2 refuse, and the order is total, so a sibling outside T
// and A, added, removed or moved, never reorders them. The walk's second part, ordered by its
// roots' parents' addresses, is refused outright.
//
// What a write states beside the facts its call site hands the rule changes none of this: the move
// of the open approval request and the carry's own answer state facts of the request, an ask of
// the conversation; refreshAnchors and a comment's pin state facts of comments, conversation
// nodes; a settlement's settles, copies and readability state facts of asks and their blocks; the
// version nodes a settlement writes after its carry are of a version's supertag, part_of their
// document, with nothing under them; the gone nodes and retractions a delete of the node route
// states after its versions are lenses, findings, risks and constraints, none of a type a query
// takes, and tuples out of the blocks its versions state gone; and the task counts, the room's
// block rows and the marks are rows and no tuples. The node route's move, which states its tuples
// outside the pass, and the decisions step's own write, hand nothing (versionWriteRecord).
//
// Each of its reads runs where it is called, the write's read phase, before the batch: the batch
// takes no statement for it beyond the carry's own.
func recordUnmoved(ctx context.Context, tx pgx.Tx, artifactID string, number int, newest model.ArtifactReview, record RecordCarry) (bool, error) {
	if !record.runs() {
		return false, nil
	}
	at := number
	if record.whole {
		ledger := queueFor(ctx, tx)
		if ledger == nil || !slices.Contains(ledger.stated, statedVersion{artifactID: artifactID, number: number}) {
			return false, nil
		}
		at = number - 1
	}
	stand, err := readRecordStand(ctx, tx, artifactID, at, newest)
	if err != nil || stand.pin == nil {
		return false, err
	}
	if record.digest != nil {
		return *record.digest == *stand.pin, nil
	}
	if stand.marked || stand.digest == nil || *stand.digest != *stand.pin {
		return false, nil
	}
	if record.stored {
		return true, nil
	}
	takes := graphstore.QueryTakes(stand.query)
	if !stand.converted || stand.issue == "" || stand.query == graphstore.QueryDecisions || len(takes) == 0 {
		return false, nil
	}
	if recordCarryForced(ctx) {
		return true, nil
	}
	stated := readStatedFacts(record.stated)
	if stated.mayTake(takes) {
		return false, nil
	}
	if err := refs.TurnJITOff(ctx, tx); err != nil {
		return false, err
	}
	tree, err := WalkIssueTree(ctx, tx, stand.issue, refs.TreeCut{})
	if err != nil {
		return false, err
	}
	joining, leaves := stated.leavesQuery(tree, stand.query, takes)
	if !leaves {
		return false, nil
	}
	if len(joining) > 0 {
		children, err := refs.TreeChildren(ctx, tx, joining, tree.Cut)
		if err != nil || len(children) > 0 {
			return false, err
		}
	}
	head, err := tree.QueryRecord(ctx, tx, stand.query)
	if err != nil {
		return false, err
	}
	return head.Digest == *stand.pin, nil
}

// statedNode is what a write's facts state of one node (readStatedFacts): the supertag of each of
// its node writes, the opening line of each text value ("" for a retraction), the far end of each
// part_of asserted and each rank asserted; whether a node write, a part_of or a rank of it is among
// them; whether a node write or a state value states its standing, and whether one states it there
// (anything but gone).
type statedNode struct {
	supertags []string
	openings  []string
	parents   []refs.Target
	ranks     []string
	written   bool
	placed    bool
	ranked    bool
	standing  bool
	live      bool
}

// moves reports whether the facts move the node's place in the walk: a node write, a part_of, a
// rank or a state.
func (node *statedNode) moves() bool {
	return node.written || node.placed || node.ranked || node.standing
}

// types is every type the node, of kind kind, can be taken by once the facts stand, head being its
// node in the head's walk, nil where the walk does not take it: its type there, and the type
// refs.TreeNodeType gives of each supertag it can hold with each opening its text can open with.
// known is false for a node outside the head's walk of which no node write states the supertag,
// whose type the facts alone do not say.
func (node *statedNode) types(kind string, head *TreeNode) (types []string, known bool) {
	supertags := slices.Clone(node.supertags)
	if head != nil {
		types = append(types, head.Type)
		supertags = append(supertags, head.Supertag)
	}
	for _, supertag := range node.supertags {
		types = append(types, refs.TreeNodeType(kind, supertag, ""))
	}
	for _, opening := range node.openings {
		if len(supertags) == 0 {
			types = append(types, refs.TreeNodeType(kind, "", opening))
		}
		for _, supertag := range supertags {
			types = append(types, refs.TreeNodeType(kind, supertag, opening))
		}
	}
	return types, head != nil || len(node.supertags) > 0
}

// statedFacts is what a write's facts state of each node they reach, by its address.
type statedFacts map[refs.Target]*statedNode

// readStatedFacts reads writes, every fact a write states before its version's event: each node
// write, and each tuple by the node it goes out of.
func readStatedFacts(writes []graphstore.Uncaused) statedFacts {
	stated := statedFacts{}
	of := func(kind, id string) *statedNode {
		target := refs.Target{Kind: kind, ID: id}
		node := stated[target]
		if node == nil {
			node = &statedNode{}
			stated[target] = node
		}
		return node
	}
	for _, write := range writes {
		for _, written := range write.Nodes {
			node := of(written.Kind, written.ID)
			node.written, node.standing = true, true
			node.live = node.live || written.State != graphstore.StateGone
			node.supertags = append(node.supertags, written.Supertag)
		}
		for index := range write.Tuples {
			tuple := &write.Tuples[index].Tuple
			node := of(tuple.FromKind, tuple.FromID)
			switch {
			case tuple.ToKind != graphstore.KindValue:
				if tuple.Kind == graphstore.TuplePartOf {
					node.placed = true
					if tuple.Asserted {
						node.parents = append(node.parents, refs.Target{Kind: tuple.ToKind, ID: tuple.ToID})
					}
				}
			case tuple.Kind == graphstore.AttributeText:
				opening := ""
				if tuple.Asserted {
					opening, _, _ = strings.Cut(tuple.ToID, "\n")
				}
				node.openings = append(node.openings, opening)
			case tuple.Kind == refs.AttributeRank:
				node.ranked = true
				if tuple.Asserted {
					node.ranks = append(node.ranks, tuple.ToID)
				}
			case tuple.Kind == graphstore.AttributeState:
				node.standing = true
				node.live = node.live || tuple.Asserted && tuple.ToID != graphstore.StateGone
			}
		}
	}
	return stated
}

// mayTake reports whether a node the facts reach can be taken by one of takes, a query's types, by
// what the facts alone say of its type (recordUnmoved's C).
func (stated statedFacts) mayTake(takes []string) bool {
	for target, node := range stated {
		types, _ := node.types(target.Kind, nil)
		for _, typ := range types {
			if slices.Contains(takes, typ) {
				return true
			}
		}
	}
	return false
}

// leavesQuery reports whether the facts leave the record of query, whose types are takes, as the
// head's walk, tree, has it (recordUnmoved's E), and answers each node outside that walk the facts
// bring into it, whose children at the head the caller reads.
func (stated statedFacts) leavesQuery(tree IssueTree, query string, takes []string) ([]refs.Target, bool) {
	issue := refs.Target{Kind: graphstore.KindIssue, ID: tree.Issue}
	taken, _ := tree.Takes(query)
	ancestors := map[refs.Target]bool{}
	for _, index := range taken {
		node := tree.Nodes[index]
		if node.Above || conversational(tree, node) {
			return nil, false
		}
		if _, touched := stated[node.target()]; touched {
			return nil, false
		}
		for parent := node.Parent; parent != issue && !ancestors[parent]; {
			at, held := tree.index[parent]
			if !held {
				return nil, false
			}
			above := tree.Nodes[at]
			if conversational(tree, above) {
				return nil, false
			}
			if touched := stated[parent]; touched != nil && touched.moves() {
				return nil, false
			}
			ancestors[parent] = true
			parent = above.Parent
		}
	}
	var joining []refs.Target
	for target, node := range stated {
		var head *TreeNode
		if at, held := tree.index[target]; held {
			head = &tree.Nodes[at]
		}
		if head == nil {
			// A node the walk does not take stands outside it still unless the facts state it there
			// or place it.
			if !node.live && len(node.parents) == 0 {
				continue
			}
			joining = append(joining, target)
		}
		types, known := node.types(target.Kind, head)
		if !known {
			return nil, false
		}
		for _, typ := range types {
			if slices.Contains(takes, typ) {
				return nil, false
			}
		}
	}
	if query == graphstore.QuerySpec && !stated.keepsFirstChild(tree) {
		return nil, false
	}
	return joining, true
}

// conversational reports whether node is a node of the issue's conversation (an ask, a comment, a
// message, or a block of one) or an ask's block in a document, whose facts writers outside the
// version write state.
func conversational(tree IssueTree, node TreeNode) bool {
	_, conversation := tree.Conversation[node.target()]
	return conversation || node.Kind == graphstore.KindAsk || node.Type == graphstore.SupertagAsk
}

// keepsFirstChild reports whether the issue's first child in the head's walk, tree, stays its first
// child once the facts stand, which the spec query takes as its memo where it is a callout (R14):
// that child holds a rank and the facts move nothing of its place, and no node they move can stand
// under the issue before it, by any rank it can hold then, compared as treeWalk.ordered compares
// two ranked children - the rank byte for byte, then the lower address. A node outside the head's
// walk that the facts place under the issue with no rank of theirs holds a rank they do not say,
// and refuses.
func (stated statedFacts) keepsFirstChild(tree IssueTree) bool {
	issue := refs.Target{Kind: graphstore.KindIssue, ID: tree.Issue}
	if len(tree.Nodes) < 2 || tree.Nodes[1].Parent != issue || !tree.Nodes[1].Ranked {
		return false
	}
	first := tree.Nodes[1]
	if touched := stated[first.target()]; touched != nil && touched.moves() {
		return false
	}
	for target, node := range stated {
		if target == first.target() || !node.moves() {
			continue
		}
		parents, ranks := slices.Clone(node.parents), slices.Clone(node.ranks)
		at, held := tree.index[target]
		if held {
			head := tree.Nodes[at]
			parents = append(parents, head.Parent)
			if head.Ranked {
				ranks = append(ranks, head.Rank)
			}
		}
		if !slices.Contains(parents, issue) {
			continue
		}
		if !held && len(node.ranks) == 0 {
			return false
		}
		for _, rank := range ranks {
			if cmp.Or(strings.Compare(rank, first.Rank), refs.CompareAddresses(target, first.target())) < 0 {
				return false
			}
		}
	}
	return true
}

// CarryApprovalToLatest is the record carry an approval request makes (api's
// requestArtifactApproval) where the document's newest approval stands by its digest at a version
// before the document's latest: it carries that approval to the latest version, in the approver's
// name, where the latest version's version_renders row holds the record digest the approval stands
// on (recordUnmoved's stored case), after R14's other rules (carryNewestApprovalNaming). The daemon
// reads an approval at latest_version alone, so without it the request reads approved and the
// daemon never reads it so. Its reads run where it is called, the route's read phase, and its
// writes are queued on the ledger of the transaction ctx joined, whose Commit publishes its events.
func CarryApprovalToLatest(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID, serverURL string) error {
	latest, err := latestVersion(ctx, tx, artifactID)
	if err != nil {
		return err
	}
	return carryToLatest(ctx, tx, broker, artifactID, latest.Number, latest.markdown, serverURL)
}

// carryToLatest carries the document's newest review, an approval of a version before number, its
// latest, stored as markdown, to that version, on carryNewestApprovalNaming's conditions with the
// record case of a stored version (storedRecord): the reopen's follow (followLatestVersion) and the
// approval request (CarryApprovalToLatest). A transaction carries one document's approval at most
// once (refuseSecondCarry), and an approval of the latest version carries nothing.
func carryToLatest(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, number int, markdown, serverURL string) error {
	if err := refuseSecondCarry(ctx, tx, artifactID); err != nil {
		return err
	}
	newest, err := NewestReview(ctx, tx, artifactID)
	if err != nil || newest == nil || newest.State != "approved" || newest.Version >= number {
		return err
	}
	// The answer carry reads the version's blocks from its tree, as the version's own write reads
	// the live tree it rendered: the stored text's read-back is that tree.
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		return skipUnreadableVersion(artifactID, number, err)
	}
	return carryNewestApprovalNaming(ctx, tx, broker, artifactID, number, markdown, tree, *newest, serverURL, decisionStepCases{}, "", storedRecord(), eventSink(ctx))
}
