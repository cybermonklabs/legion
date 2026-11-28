package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// reasoningNode is one node of the reasoning kinds a request writes under an issue: a lens
// (graphstore.KindLens), a finding of a lens - a node of kind and supertag block whose id names
// its lens, `lens/<uuid>#<block id>` (graphstore.KindLensBlock) - a risk (graphstore.KindRisk) or
// a constraint (graphstore.KindConstraint). A run's interaction is no node of this write: the
// going POST states it (api/going.go).
//
// visibility is what the request asked for, the empty string being project. parentKind and
// parentID are the node it stands under, which its part_of tuple names: a lens's node, a
// finding's or a risk's lens, a constraint's issue. attributes are what its row carries, each of
// which the write also states as a value tuple, and tuples every other fact the request states
// with it: a relation out of it or into it - a lens's has_kind and triggered_by, a risk's
// prevented_by, a requirement's constrained_by to a constraint - or a value of it that its row
// does not carry (ToKind graphstore.KindValue), a finding's text. A tuple is named by its kind and
// its two ends alone: the write dates it, signs it as the node's writer and asserts it.
type reasoningNode struct {
	kind, id             string
	visibility           string
	parentKind, parentID string
	attributes           map[string]any
	tuples               []graphstore.Tuple
}

// reasoningWrite is one request's write of reasoning nodes, all under the issue issue names, by
// the caller the route resolved (reasoningCaller): a caller that resolves no request - a command
// - passes the actor it acts as for both of its actors. farEnds are the far ends of its tuples the
// route found under the issue, each named by the address the request gave, which the write reads
// again under the issue's owner lock (refuseGoneFarEnds): the lens a lens is triggered by.
type reasoningWrite struct {
	reasoningCaller
	issue   string
	at      time.Time
	nodes   []reasoningNode
	farEnds []namedEnd
}

// lockIssueOwner takes the issue's owner lock (store.LockOwner), the lock the order on
// api/inbox_read.go's markInboxReadThreads puts before every other, and then reads the issue's
// project in a statement of its own, so that read sees every commit of the lock's previous holder
// (store.LockOwnerAndRead): the first lock of every reasoning write that names no check. A write
// that names one takes the lock before, with the owner locks of its checks' documents
// (lockReasoningOwners), and then takes here a lock its transaction holds, which returns at once.
// An issue that does not exist is pgx.ErrNoRows.
func lockIssueOwner(ctx context.Context, tx pgx.Tx, key string) (string, error) {
	var project string
	err := store.LockOwnerAndRead(ctx, tx, store.IssueOwner(key), `select project_key from issues where key = $1`, []any{key}, &project)
	return project, err
}

// lockReasoningOwners takes every owner lock a reasoning write stands under, in one
// events.LockOwnerRows call, so once each and in ascending lock key, before the write reads a
// node: the issue's own, and the owner of the document of each check a prevented_by of the write
// asserts, which checks names by the addresses the request gave (a dispatch:// address or a store
// id), none for a write that asserts no prevented_by. A retirement of a document
// (docs.RetireDocument) and an index pass that marks a block gone (refs.IndexBlocks, under the
// revision fence) each take that document's owner lock first, so an assertion that races either
// runs wholly before it, and is retracted with the rest, or wholly after it, and reads the
// document retired or the check gone, and is refused (spec requirement r-constraint-risk). Two
// writes that name each other's issues take the locks they share in one order, so neither holds a
// lock the other waits for.
//
// The document a check stands in is read off its address with no lock, before the locks: a
// document block's id is its document's ref_key, '#' and its block id, a ref_key names one
// document, and no write moves a document to another issue (project_scope.go). An address that
// names no block of a document names no owner, and the read under the locks answers it as one
// that does not exist.
//
// A check of an issue room named by its room address, `<tree ref_key>#<block>` - the id its
// prevented_by stores, which a block of a converted document resolves to - names the room's tree
// artifact before its '#', not the document whose fragment holds the block. The lock is the same:
// the tree and every document joined to its room are artifacts of one issue, and an issue's
// artifacts all stand under the issue's owner (events.OwnerRows, docs' lockArtifactOwner). So the
// write queues with the retirement of that document and with the room's index pass, which moves a
// block from one fragment to another under the same lock, whichever fragment holds the block; the
// store's rule then reads the block's document under the locks (graphstore's readBlocks).
func lockReasoningOwners(ctx context.Context, tx pgx.Tx, issue string, checks []string) error {
	owners := events.OwnerRows{}
	owners.Add(issue, "")
	documents := make(map[string]struct{}, len(checks))
	for _, check := range checks {
		kind, id, ok := decodeNodeAddress(check)
		if !ok || kind != refs.BlockNodeKind {
			continue
		}
		// A ref_key holds no '#', so the block's id names its document before the first one.
		if document, _, held := strings.Cut(id, "#"); held {
			documents[document] = struct{}{}
		}
	}
	if len(documents) > 0 {
		rows, err := tx.Query(ctx, `
			select id::text, coalesce(issue_key, '') from artifacts where ref_key = any($1)
		`, slices.Collect(maps.Keys(documents)))
		if err != nil {
			return fmt.Errorf("read the documents of the checks the write names: %w", err)
		}
		var id, issueKey string
		if _, err := pgx.ForEachRow(rows, []any{&id, &issueKey}, func() error {
			owners.Add(issueKey, id)
			return nil
		}); err != nil {
			return fmt.Errorf("read the documents of the checks the write names: %w", err)
		}
	}
	return events.LockOwnerRows(ctx, tx, owners)
}

// writeReasoningNodes is the one write path of the lenses, the findings, the risks and the
// constraints a request writes, and of their tuples, in the caller's transaction: every route that
// writes one, and the pre-mortem conversion, write through here. For each node it writes the
// node, its part_of tuple to its parent, one value tuple of each attribute its row carries
// (graphstore.AttributeTuples, in the attributes' name order), so the row is the projection of its
// tuples and the history and the as-of reads answer each attribute, the tuples the request states
// with it, and its authored_by tuple to the writer graphstore.AuthorOf names of its made_by, with
// that writer's own person or session node and its alias and recorded_as tuples
// (graphstore.AuthorFacts), as the comment path states who wrote a comment. A lens it places on a
// document or on a block of one records, in the same write, the artifacts.id of the row the
// artifacts table holds at that document's address under the issue's owner lock
// (graphstore.AttributeArtifactID), which the store's write of its part_of states, so the lens is
// stale once that row no longer stands there (refs.StaleSQL). The authored_by tuple is
// attribution, not proof (spec v46 R12): on the deployment's shared bearer it names the session
// the body names, and on a pod's write the session its body names beside the pod's workload,
// though neither credential proves that session (graphstore.ProvesAuthor). The author a read and
// a settle compare is the one the node's made_by proves (graphstore.SameAuthor), which this tuple
// does not decide. A node whose made_by names only a workload - a pod's node reserved for its
// author, made by the pod's token - or nobody AuthorOf names states no authored_by and no author's
// node.
//
// Each node's visibility is graphstore.VisibilityFor of what it asked for and the proving
// credential: `author` on a credential that proves no author is graphstore.ErrAuthorNamesNobody,
// 403 AUTHOR_NAMES_NOBODY, refused before the first statement, so nothing of the write is stored.
// An author-only node is made by the proving credential, so the credential its author reads it
// with matches its made_by; a project node is made by the recorded actor. A node written on the
// shared bearer is always project-visible and its writer settles nothing, since that bearer proves
// no author, while its authored_by names the session its body names. Every node is live, in the
// project of the issue (the project graphstore.IssueNode states of the issue's own node), and
// between no parties.
//
// It takes the issue's owner lock first and reads the project in the statement after it
// (lockIssueOwner), a lock its caller holds already where the write asserts a prevented_by
// (lockReasoningOwners); an issue that does not exist is pgx.ErrNoRows. Under that lock it reads
// each node's parent again and refuses one the store holds gone (refuseGoneParents), 409
// NODE_GONE, and each far end the route found under the issue (refuseGoneFarEnds), 404
// NOT_FOUND, before anything is written. Then it writes every node, the authors' among them, and
// every tuple in one batch (graphstore.QueueWrite), with their head rows after them
// (graphstore.QueueHeads), no Go read between the statements: the nodes
// in ascending (kind, id), the type rules of the pointer tuples read and decided by statements
// queued between the node write and the append (graphstore's pointerRules, whose refusals are 400
// INVALID_POINTER and 409 POINTER_ALREADY_ASSERTED, which end the transaction), and every node and
// tuple at graphstore.NoEvent. No event is appended, because no write of these kinds sends a
// delivery: a reader learns of one by reading it.
//
// That each far end a tuple names exists, is not gone and may be seen by the caller is the
// route's to check before it calls this, through the read registry's loader for that end's kind,
// so that a far end the caller may not see answers as one that does not exist. An end that
// stands under `{key}` - the node a lens is invoked on, a lens a risk or a finding stands under,
// a requirement of the issue's documents - is found through FindNodeUnderIssue. A risk's check,
// which stands under any issue of the project (the store holds it to the risk's project), and an
// end that stands under no issue - a has_kind tuple's lens_kind, a constraint of the catalogue a
// lens_kind reaches - are no nodes FindNodeUnderIssue answers but 404 for, and are found through
// refs.Node with the caller's visibility instead (pointerEnd). A far end that stands under `{key}`
// is read again under the issue's owner lock, since a write may take it gone while the request
// waits for that lock (refuseGoneFarEnds). A caller whose write asserts a prevented_by first takes
// the owner lock of each check's document with the issue's (its race with a retirement or an
// index pass: lockReasoningOwners).
//
// It answers the nodes the request wrote, in the order it named them, without their authors':
// the nodes this write made, which are the ones a write made for a run names as changed by it
// (graphstore.TupleChangedByRun). Each is marked changed (graphstore.Node's Changed) and no
// author's node is, so graphstore.QueueWrite, on a batch made for a run (store.WriteBatch's Run),
// appends that run's changed_by_run out of each of them alone.
func writeReasoningNodes(ctx context.Context, tx pgx.Tx, write reasoningWrite) ([]graphstore.Node, error) {
	written := make([]graphstore.Node, len(write.nodes))
	for index, node := range write.nodes {
		if node.parentKind == "" || node.parentID == "" {
			return nil, fmt.Errorf("the %s %s names no node to stand under", node.kind, node.id)
		}
		visibility, err := graphstore.VisibilityFor(node.visibility, write.proving)
		if err != nil {
			return nil, err
		}
		madeBy := write.recorded
		if visibility == graphstore.VisibilityAuthor {
			madeBy = write.proving.Recorded()
		}
		written[index] = graphstore.Node{
			Kind: node.kind, ID: node.id, Supertag: node.kind,
			Visibility: visibility, State: graphstore.StateLive, Attributes: node.attributes,
			MadeAt: write.at, MadeBy: madeBy, SourceEvent: graphstore.NoEvent, Changed: true,
		}
	}
	project, err := lockIssueOwner(ctx, tx, write.issue)
	if err != nil {
		return nil, err
	}
	if err := refuseGoneParents(ctx, tx, write.nodes); err != nil {
		return nil, err
	}
	if err := refuseGoneFarEnds(ctx, tx, write.issue, write.farEnds); err != nil {
		return nil, err
	}

	nodes := make([]graphstore.Node, 0, len(written)+2)
	var tuples []graphstore.Tuple
	// One write writes a node once and states a fact once, so an author several nodes of the
	// request name is made by the first of them, as are the alias facts of that author; each node
	// still states its own authored_by.
	authors := map[[2]string]struct{}{}
	aliases := map[[5]string]struct{}{}
	for index := range written {
		node := &written[index]
		node.Project = project
		nodes = append(nodes, *node)
		tuples = append(tuples, graphstore.Tuple{
			FromKind: node.Kind, FromID: node.ID, Kind: graphstore.TuplePartOf,
			ToKind: write.nodes[index].parentKind, ToID: write.nodes[index].parentID,
			At: write.at, Who: node.MadeBy, SourceEvent: graphstore.NoEvent, Asserted: true,
		})
		tuples = append(tuples, graphstore.AttributeTuples(*node, slices.Sorted(maps.Keys(node.Attributes))...)...)
		for _, stated := range write.nodes[index].tuples {
			stated.Seq, stated.LegacySourceSeq = 0, nil
			stated.At, stated.Who, stated.SourceEvent, stated.Asserted = write.at, node.MadeBy, graphstore.NoEvent, true
			tuples = append(tuples, stated)
		}
		authorNodes, facts, named := graphstore.AuthorFacts(node.Kind, node.ID, node.MadeBy, write.at, graphstore.NoEvent)
		if !named {
			continue
		}
		for _, author := range authorNodes {
			if _, made := authors[[2]string{author.Kind, author.ID}]; !made {
				authors[[2]string{author.Kind, author.ID}] = struct{}{}
				// The author's node is the person's or the session's, whose state and visibility
				// no node of theirs owns.
				author.Restates = true
				nodes = append(nodes, author)
			}
		}
		// The first fact is this node's authored_by; the rest are the author's aliases.
		tuples = append(tuples, facts[0])
		for _, alias := range facts[1:] {
			key := [5]string{alias.FromKind, alias.FromID, alias.Kind, alias.ToKind, alias.ToID}
			if _, stated := aliases[key]; !stated {
				aliases[key] = struct{}{}
				tuples = append(tuples, alias)
			}
		}
	}
	// A lens placed on a document no row stands at, as the store's write reads it under the issue's
	// owner lock (graphstore.NoDocumentRowError) - one a removal took while this write waited for
	// that lock - is a write under a gone node: 409 NODE_GONE, naming the node it is placed on as
	// refuseGoneNode names a gone parent (goneEndError): a document by its ref_key, a block by its
	// own address. The head rows of what the write names go in its batch, after it.
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueWrite(b, nodes, tuples)
		graphstore.QueueHeads(b, graphstore.HeadsOf(nodes, tuples))
		return nil
	}); err != nil {
		if noRow := (*graphstore.NoDocumentRowError)(nil); errors.As(err, &noRow) {
			return nil, goneEndError(storedEnd{kind: noRow.ToKind, id: noRow.ToID})
		}
		return nil, err
	}
	return written, nil
}

// refuseGoneParents is the 409 NODE_GONE of a write of nodes one of whose parents the store holds
// gone, or stale (storedGone), read once the issue's owner lock is held: the node a lens is invoked
// on, the lens a finding or a risk is written under, the issue a constraint stands under
// (refuseGoneNode). The route found each parent present before it took that lock, and a write that
// takes a parent gone in the meantime holds the same lock - the state route (writeNodeState), and a
// version's index pass, whose owner is its issue - so this read sees what that write committed, and
// nothing of the request is stored under a gone node or a stale one. A parent the request writes
// itself, a finding's lens written in the same request, is none of these.
func refuseGoneParents(ctx context.Context, tx pgx.Tx, nodes []reasoningNode) error {
	read := make(map[storedEnd]bool, len(nodes)*2)
	for _, node := range nodes {
		read[storedEnd{kind: node.kind, id: node.id}] = true
	}
	for _, node := range nodes {
		parent := storedEnd{kind: node.parentKind, id: node.parentID}.stored()
		if read[parent] {
			continue
		}
		read[parent] = true
		if err := refuseGoneNode(ctx, tx, parent); err != nil {
			return err
		}
	}
	return nil
}

// refuseGoneNode is the 409 NODE_GONE of a write on the node the store keys as end, where its row
// is at gone or the node is stale (storedGone), naming it as goneEndError does; nil where the row
// is at any other state and the node is not stale, or the store holds no row of it, which every
// read answers live.
func refuseGoneNode(ctx context.Context, tx pgx.Tx, end storedEnd) error {
	gone, err := storedGone(ctx, tx, end)
	if err != nil || !gone {
		return err
	}
	return goneEndError(end)
}

// goneEndError is the 409 NODE_GONE (nodeGone) of a write on the node the store keys as end, which
// it holds gone, naming it by its dispatch:// address where it is a lens, a finding, a risk or a
// constraint, and by its store id otherwise.
func goneEndError(end storedEnd) error {
	kind, address := graphstore.AddressKind(end.kind, end.id), end.id
	if standingKind(kind) {
		address = refs.StoreRef(kind, end.id)
	}
	return nodeGone(kind, address)
}

// refuseGoneFarEnds is the 404 NOT_FOUND of a write one of whose tuples points at a far end the
// store holds gone, or stale (storedGone), read once the issue's owner lock is held: ends are the
// far ends the route found under the issue, each named by the address the request gave - the lens
// a lens is triggered by, and the lens a triggered_by or the constraint a constrained_by of the
// pointer route points at. The route found each not gone before it took that lock, and a write
// that takes one gone in the meantime holds the same lock (writeNodeState), so this read sees what
// that write committed. A gone far end answers what the route answers for one it found gone, the
// 404 of an address nobody holds under the issue (nodeNotFound), and nothing of the request is
// stored.
func refuseGoneFarEnds(ctx context.Context, tx pgx.Tx, issue string, ends []namedEnd) error {
	for _, end := range ends {
		gone, err := storedGone(ctx, tx, end.stored())
		if err != nil {
			return err
		}
		if gone {
			return nodeNotFound(issue, end.address)
		}
	}
	return nil
}

// storedGoneQuery is whether the row of the node whose stored kind and id are $1 and $2 is at gone,
// or the node is stale, which every read answers as gone (refs.GoneOrStaleRow), and no row where
// the store holds none.
var storedGoneQuery = `select ` + refs.GoneOrStaleRow(`held`) + `
	from graph_nodes held where held.kind = $1 and held.id = $2`

// storedGone is whether the store holds the node it keys as end at gone, or stale
// (storedGoneQuery), false where it holds no row of it.
func storedGone(ctx context.Context, tx pgx.Tx, end storedEnd) (bool, error) {
	var gone bool
	err := tx.QueryRow(ctx, storedGoneQuery, end.kind, end.id).Scan(&gone)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read whether %s %s is gone: %w", end.kind, end.id, err)
	}
	return gone, nil
}

// The three things a request may do to one pointer tuple out of a node (writeReasoningPointer).
const (
	pointerAppend   = "append"
	pointerRetract  = "retract"
	pointerRetarget = "retarget"
)

// reasoningPointer is one request's write of one pointer tuple out of a node that stands under
// the issue issue names: a lens's has_kind or triggered_by, a requirement's constrained_by, a
// risk's prevented_by, or the moved_to from a document the pre-mortem conversion retires to its
// lens. action is pointerAppend, pointerRetract or pointerRetarget. pointer is the tuple the write
// names, dated and signed by the write and at graphstore.NoEvent: the one it appends, the one it
// retracts, or, for a retarget, the one the node holds now, which moves to toKind and toID.
// farEnds are the far ends the route found under the issue, each named by the address the request
// gave, which the write reads again under the issue's owner lock (refuseGoneFarEnds): the lens of a
// triggered_by and the constraint of a constrained_by.
type reasoningPointer struct {
	issue        string
	action       string
	pointer      graphstore.Tuple
	toKind, toID string
	farEnds      []namedEnd
}

// writeReasoningPointer is the one write path of a pointer tuple a request appends, retracts or
// retargets out of a lens, a risk or a requirement block, and of the moved_to the pre-mortem
// conversion appends out of the document it retires (retireConvertedDocument), in the caller's
// transaction. It takes the issue's owner lock first (lockIssueOwner), which a caller that asserts
// a prevented_by holds already (lockReasoningOwners), and reads the node the tuple goes out of
// again under it: one a write took gone since the route found it is 409 NODE_GONE
// (refuseGoneNode), as a gone parent is for writeReasoningNodes, and a far end of farEnds a write
// took gone is the route's 404 (refuseGoneFarEnds). Then it writes through the store, which holds
// the tuple to its type rule (graphstore's pointerRules) and takes the node rows that rule names
// after the owner lock: an append and a retraction through graphstore.QueueWrite, one batch of
// their own with the head row of the node the tuple goes out of (graphstore.QueueHeads), a retarget
// through graphstore.Retarget, whose read phase takes the node's row and reads its pointers before
// its Write, which writes that head row too. No event is appended, as none is for the nodes these
// tuples tie (writeReasoningNodes). The pointer is marked as changing the node it goes out of
// (graphstore.Tuple's Changes), so a write made for a run names that node, and not its far end,
// as changed by it.
//
// A retraction of a tuple the node does not hold now is a 404, as a retarget of one is
// (graphstore.ErrPointerNotHeld): there is nothing to retract. An append of a tuple the node holds
// now changes nothing, so it writes nothing, and a write made for a run names no node as changed
// by it, as a retarget to the far end the node holds writes nothing; a has_kind is the store's to
// refuse, since a lens holds one kind and a second assertion of it is ErrLensKindAsserted whatever
// its far end. That both ends exist, are not gone and may be seen by the caller is the route's to
// check before it calls this; a far end that stands under the issue is read again here.
func writeReasoningPointer(ctx context.Context, tx pgx.Tx, write reasoningPointer) error {
	if _, err := lockIssueOwner(ctx, tx, write.issue); err != nil {
		return err
	}
	if err := refuseGoneNode(ctx, tx, storedEnd{kind: write.pointer.FromKind, id: write.pointer.FromID}.stored()); err != nil {
		return err
	}
	if err := refuseGoneFarEnds(ctx, tx, write.issue, write.farEnds); err != nil {
		return err
	}
	write.pointer.Changes = true
	switch write.action {
	case pointerRetarget:
		return graphstore.Retarget(ctx, tx, write.pointer, write.toKind, write.toID)
	case pointerRetract:
		held, err := holdsPointer(ctx, tx, write.pointer)
		if err != nil {
			return err
		}
		if !held {
			return errorf(http.StatusNotFound, "NOT_FOUND", "the node holds no %s tuple at that node to retract", write.pointer.Kind)
		}
		retraction := write.pointer
		retraction.Asserted = false
		return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueWrite(b, nil, []graphstore.Tuple{retraction})
			graphstore.QueueHeads(b, graphstore.HeadsOf(nil, []graphstore.Tuple{retraction}))
			return nil
		})
	case pointerAppend:
		if write.pointer.Kind != graphstore.TupleHasKind {
			held, err := holdsPointer(ctx, tx, write.pointer)
			if err != nil || held {
				return err
			}
		}
		return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueWrite(b, nil, []graphstore.Tuple{write.pointer})
			graphstore.QueueHeads(b, graphstore.HeadsOf(nil, []graphstore.Tuple{write.pointer}))
			return nil
		})
	}
	return fmt.Errorf("no pointer write %q", write.action)
}

// holdsPointer is whether the node pointer goes out of holds it now: a current tuple of its kind
// at its far end.
func holdsPointer(ctx context.Context, tx pgx.Tx, pointer graphstore.Tuple) (bool, error) {
	current, err := graphstore.CurrentOut(ctx, tx, pointer.FromKind, pointer.FromID, []string{pointer.Kind})
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(current, func(tuple graphstore.Tuple) bool {
		return tuple.ToKind == pointer.ToKind && tuple.ToID == pointer.ToID
	}), nil
}

// nodeStateWrite is one request's write of one node's state: the node target names, under the
// issue issue names, moved to state, signed by who and dated at. allowed is the route's rule over
// the node as the store holds it, read once the issue's owner lock is held. A rule that answers a
// refusal stores nothing.
type nodeStateWrite struct {
	issue   string
	target  stateTarget
	state   string
	who     model.Actor
	at      time.Time
	allowed func(stored graphstore.Node) error
}

// errNodeNotHeld is writeNodeState's refusal of a node the store holds no row of, which every read
// answers live and project: a block of an ask, a comment or a message, or a node written before
// the store existed that the backfill has not reached yet. Nothing writes a node that is not
// there, and each route of a node's state answers this in its own words (nodeStateRule).
var errNodeNotHeld = errors.New("the store holds no row of the node")

// writeNodeState is the one write path of a node's state a request asks for - the state route and
// the settle (node_state.go) - in the caller's transaction. It takes the issue's owner lock first
// (lockIssueOwner), as writeReasoningPointer does, so two writes of one node's state queue there
// and the second reads what the first wrote. It reads the node's row: a node the store holds no
// row of is errNodeNotHeld, and a node at gone, or a stale one (storedGone), 409 NODE_GONE
// (nodeGone), since no write writes on a gone node, so a write the route found the node live for
// and that queued behind its gone is refused too. It holds the node to the route's rule and writes
// it again as the store holds it but for its state (graphstore.PlanState), with its head rows after
// it in the same batch (graphstore.QueueHeads): a plain node write, not one that restates the node
// (graphstore.Node.Restates), dated and signed by the request and at graphstore.NoEvent, as every
// write of these routes is. So graphstore.QueueWrite moves the row's state and appends the one
// state tuple that says so, whose writer is who: the row and the log
// agree, and every read of the node answers the new state at once. The tuple is at the sentinel,
// which no repeat key drops, so a node moved proposed, live and proposed again holds three state
// tuples. The row keeps the cause the store holds of it (graphstore.Node.KeepsCause): a write of
// the state is no cause of the node's other facts, and a block row put at the sentinel would take
// the event of its document's next version, whose index pass moves its cause onto every block row
// still at the sentinel, whether or not that version changed the block. made_at and made_by stay
// the row's own, the store's rule for a node written again, so the node's author is still the
// writer of its first write. A write of the state the node holds moves nothing and appends
// nothing. A write that moves it marks the node changed (graphstore.Node's Changed), so one made
// for a run names the node as changed by that run.
//
// The write that takes a node to gone - a lens, a finding, a risk or a constraint, the route's
// only nodes that take gone (stateRefusal) - takes its whole current part_of subtree that is not
// gone yet to gone in the same batch, each node the same way, at every depth and
// whatever its kind, those the caller may not see among them: a finding or a risk under a lens, a
// lens invoked on a lens, a finding, a risk or a constraint, and all that stands under each
// (graphstore.PlanState, whose read beneath a node is one statement however deep the tree).
// So no read by its own address answers a node under a gone one live with nothing to stand on
// (spec requirement r-proposed). The route lets a person alone make that move (goneNotPermitted),
// so what stands under the node is read for the write and for no refusal, and tells the caller
// nothing.
//
// The state is one of the values a query's record holds of the node (R14), so a write that moves
// it marks the queries of the issue owed in its batch, after the tuples, so its sequence is the
// state tuple's, and before the head rows (docs.QueueQueryMark), as the node delete marks them:
// marked says, once the batch has run, whether the issue has converted and took the mark, and the
// route drains its queries after its COMMIT (docs.Service.DrainOwedQueries).
//
// It answers the node as the write leaves it, the state it held before, and whether it marked.
func writeNodeState(ctx context.Context, tx pgx.Tx, write nodeStateWrite) (graphstore.Node, string, bool, error) {
	if _, err := lockIssueOwner(ctx, tx, write.issue); err != nil {
		return graphstore.Node{}, "", false, err
	}
	node, held, err := graphstore.ReadNode(ctx, tx, write.target.end.kind, write.target.end.id)
	switch {
	case err != nil:
		return graphstore.Node{}, "", false, err
	case !held:
		return graphstore.Node{}, "", false, errNodeNotHeld
	}
	if gone, err := storedGone(ctx, tx, write.target.end); err != nil {
		return graphstore.Node{}, "", false, err
	} else if gone {
		return graphstore.Node{}, "", false, nodeGone(write.target.kind, write.target.address)
	}
	if err := write.allowed(node); err != nil {
		return graphstore.Node{}, "", false, err
	}
	previous := node.State
	if previous == write.state {
		return node, previous, false, nil
	}
	moved, err := graphstore.PlanState(ctx, tx, []graphstore.Node{node}, write.state, write.at, write.who)
	if err != nil {
		return graphstore.Node{}, "", false, err
	}
	marked := false
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueWrite(b, moved, nil)
		docs.QueueQueryMark(b, write.issue, graphstore.NoEvent, &marked)
		graphstore.QueueHeads(b, graphstore.HeadsOf(moved, nil))
		return nil
	}); err != nil {
		return graphstore.Node{}, "", false, err
	}
	return moved[0], previous, marked, nil
}
