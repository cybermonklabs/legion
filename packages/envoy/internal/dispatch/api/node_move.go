package api

// node_move.go serves the move of a node (R7 of the issue-tree spec): PATCH
// /api/v1/issues/{key}/nodes/{id}, the one route that moves a node of an issue's tree under another
// parent or among its siblings, on the node's own resource, as HTTP defines the method (RFC 5789).
//
// `{id}` is the node's store address, one percent-encoded path segment that FindNodeUnderIssue
// decodes (node_address.go), of a node that stands under `{key}`; a node the caller may not see
// there is 404 NOT_FOUND. Every move states the placement revision it read in HTTP's own
// precondition: an `If-Match` naming, as a quoted entity tag, the node's placement_seq, the seq of
// its current part_of, or, for a child issue that holds none, of its current child_of
// (childOfPlacementSQL), which the tree read answers on every node. A move with none is 428
// PRECONDITION_REQUIRED, and one whose tag is no longer the node's placement_seq 412
// PRECONDITION_FAILED. The body takes parent and after; one with neither is 400 MOVE_INPUT.
//
// The move appends one part_of tuple from the moved node to its new parent and one retraction of
// the old, and touches no descendant and no serves; it gives a ranked node a rank in its own
// document's band between after and the next ranked sibling of that band (rank.Between), and a
// block of a document the placement refs.PlacementStored, which the index pass keeps. Where
// Markdown can show the block's new place, its text moves there in the same transaction, written
// into the document's tree directly under the room locks (docs.Service.MoveBlockText), and a typed
// block's parent attribute is made to name the new parent, or taken out where it can name none.
// The tuples are written at graphstore.NoEvent, as every node route writes its own; where the move
// moves the record of a query of a converted issue, its batch marks that issue's queries owed and
// the drain writes their versions after the COMMIT (docs.Service.DrainOwedQueries).
//
// The refusals, in the order the route reads them: an ask, a comment or a message, which the route
// moves none of, and a decision that holds a from_ask, which stands under the ask it settles, for
// every caller (400 NODE_NOT_MOVABLE); a parent nobody holds (404 NOT_FOUND),
// and a constraint moved under any node but an issue's (400 INVALID_POINTER), which R7 reads before
// every other rule; a body that names no move (400 MOVE_INPUT) and a move that states no placement
// (428 PRECONDITION_REQUIRED); R28's writer check on a requirement, a criterion or a check (403
// DOCUMENT_WRITER_NOT_ARCHITECT); R32's refusal of an agent's move of a person's decision, an
// answered ask's `:::ask` block or a decision a person decided (409 ANSWERED_ASK_KEPT); then,
// under the owner locks (lockNodeRouteOwners), a placement revision that moved (412); a parent
// inside the moving subtree, a parent outside the issue's own tree, and a node that reads its issue
// from its own row moved under a node of an issue below (each 400 INVALID_POINTER); a parent that
// is, or stands under, the node of a document a removal could take (409
// PARENT_DOCUMENT_REMOVABLE); a parent the walk of its issue does not reach (409
// PARENT_NOT_REACHED); an after that is no ranked sibling of the node's band (400
// INVALID_POINTER); and last, under the room locks, a text move that would leave a callout or a
// list item with no block of its own (409 CONTAINER_WOULD_EMPTY), or a text the document's schema
// cannot hold where the move puts it (400 INVALID_POINTER). A refused move stores nothing.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/rank"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Codes of the move's own refusals.
const (
	// codeMoveInput is a body that names neither parent nor after, or names one as no address.
	codeMoveInput = "MOVE_INPUT"
	// codeNodeNotMovable is a move of an ask, a comment or a message, whose part_of follows its
	// own row and no move writes, or of a decision that holds a from_ask, whose part_of follows
	// that from_ask.
	codeNodeNotMovable = "NODE_NOT_MOVABLE"
	// codeParentNotReached is a parent the walk of the issue it stands under does not reach.
	codeParentNotReached = "PARENT_NOT_REACHED"
	// codeParentDocumentRemovable is a parent that is, or stands under, the node of a document a
	// removal could take, under which the node moved may not stand.
	codeParentDocumentRemovable = "PARENT_DOCUMENT_REMOVABLE"
	// codeContainerWouldEmpty is a move whose text would leave a callout or a list item with no
	// block of its own.
	codeContainerWouldEmpty = "CONTAINER_WOULD_EMPTY"
	// codePreconditionRequired is a move with no If-Match, and codePreconditionFailed one whose
	// tag is not the node's placement_seq.
	codePreconditionRequired = "PRECONDITION_REQUIRED"
	codePreconditionFailed   = "PRECONDITION_FAILED"
)

// moveChainDepth bounds the walk up a parent's part_of chain: a chain longer than that reads as
// ending there, as the block index's own climb does (refs' authoredChainDepth).
const moveChainDepth = 64

// nodeMoveInput is the body of PATCH /api/v1/issues/{key}/nodes/{id}.
type nodeMoveInput struct {
	Actor *model.Actor `json:"actor"`
	// Parent is the store address of the node the moved node is to stand under, as `{id}` names a
	// node, or its dispatch:// address. Without it the node keeps its parent.
	Parent *string `json:"parent,omitempty"`
	// After is the store address of the ranked sibling the node goes right after, under the parent
	// it has once the move is taken; null places it first among the ranked siblings of its own
	// document's band (after the issue's memo, for a block of the spec's band under the issue), and
	// a body without it puts it after every ranked sibling of its band.
	After *string `json:"after"`
}

// nodeMoved is the answer of a move that is taken: the node's parent, the ranked sibling it now
// follows, null where it follows none, each by its store address, and its new placement_seq, the
// tag the next If-Match of the node takes.
type nodeMoved struct {
	Parent       string  `json:"parent"`
	After        *string `json:"after"`
	PlacementSeq int64   `json:"placement_seq"`
}

// moveNode is one end of a move as the route reads it: the kind the graph addresses it by, the
// kind and the id the store keys it by, the address the answer names it by, and the words a
// refusal names it by, its dispatch:// address where it has one.
type moveNode struct {
	kind, stored, id string
	named            string
	// typ is a block's type as its document_blocks row holds it, "" for any other node, which a
	// refusal names the node's kind by (word).
	typ string
}

// word is the node's kind as a refusal names it: a block of a document by its type, a finding
// as a finding, any other node by its kind.
func (n moveNode) word() string { return cmp.Or(n.typ, kindWord(n.kind)) }

// readBlockType sets node's typ to the type its document_blocks row holds, through q, where node is
// a block; any other node, and a block no row holds (a lens's finding), keeps no type.
func readBlockType(ctx context.Context, q queryer, node *moveNode) error {
	if node.kind != refs.BlockNodeKind {
		return nil
	}
	if err := q.QueryRow(ctx, `select coalesce((select type from document_blocks where node_kind = $1 and node_address = $2 limit 1), '')`,
		node.stored, node.id).Scan(&node.typ); err != nil {
		return fmt.Errorf("read the type of %s: %w", node.named, err)
	}
	return nil
}

// address is the node's store address as `{id}` spells it (decodeNodeAddress): a block's own
// node id, every other node `<kind>/<id>`.
func (n moveNode) address() string { return storeAddress(n.kind, n.id) }

// storeAddress is the store address of the node the graph addresses as kind and id, as `{id}`
// spells it.
func storeAddress(kind, id string) string {
	if strings.Contains(id, "#") {
		return id
	}
	return kind + "/" + id
}

// moveNodeOf is node, which address named, as a move reads it.
func moveNodeOf(node model.GraphNode, end storedEnd, address string) moveNode {
	return moveNode{
		kind: end.kind, stored: refs.StoredKindOf(end.kind), id: end.id,
		named: cmp.Or(node.Ref, address),
	}
}

// moveNodeKinds are the kinds the route moves no node of: an ask, a comment and a message, and a
// block of one, each of whose part_of follows its own row (R7).
var unmovableKinds = []string{graphstore.KindAsk, graphstore.KindComment, graphstore.KindMessage,
	graphstore.KindAsk + "_block", graphstore.KindComment + "_block", graphstore.KindMessage + "_block"}

// moveNodeRoute is PATCH /api/v1/issues/{key}/nodes/{id}: the move of the node {id} names under the
// node parent names, among its siblings where after names one, by the caller (node_move.go's head).
func (s *server) moveNodeRoute(w http.ResponseWriter, r *http.Request) {
	proving, resolved, ok := s.resolveCaller(w, r)
	if !ok {
		return
	}
	var input nodeMoveInput
	body, err := decodeJSONBody(resolved, &input)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(body, &members); err != nil {
		s.writeHandlerError(w, fmt.Errorf("read the move's members: %w", err))
		return
	}
	_, parentNamed := members["parent"]
	_, afterNamed := members["after"]
	var shape error
	switch {
	case !parentNamed && !afterNamed:
		shape = errorf(http.StatusBadRequest, codeMoveInput,
			"a move names parent, the node to stand under, after, the ranked sibling to follow, or both; this body names neither")
	case parentNamed && (input.Parent == nil || *input.Parent == ""):
		shape = errorf(http.StatusBadRequest, codeMoveInput, "parent names no node: give the store address of the node to stand under")
	case afterNamed && input.After != nil && *input.After == "":
		shape = errorf(http.StatusBadRequest, codeMoveInput, "after names no node: give the store address of a ranked sibling, or null to stand first")
	}
	person := docs.CallerIsPerson(proving)
	var writer model.Actor
	if person {
		writer, ok = s.actingPerson(w, resolved)
	} else {
		writer, ok = s.requireActor(w, resolved, input.Actor)
	}
	if !ok {
		return
	}
	move := nodeMoveRequest{
		key: r.PathValue("key"), id: r.PathValue("id"), who: proving.Recorded(), writer: writer, person: person,
		parent: input.Parent, after: input.After, afterNamed: afterNamed, shape: shape, tags: r.Header.Values("If-Match"),
	}
	answer, published, marked, err := s.moveNode(r.Context(), move)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var drained []model.Event
	for _, issue := range marked {
		drained = append(drained, s.deps.Docs.DrainOwedQueries(r.Context(), issue)...)
	}
	s.publish(r.Context(), append(appendedEvents(published...), drained...)...)
	WriteJSON(w, http.StatusOK, answer)
}

// nodeMoveRequest is one move as the route read its request: the issue and the node's address,
// who reads (who) and who writes (writer), whether the caller is a person, the parent and after the
// body named, whether it named after at all, the body's own refusal (400 MOVE_INPUT, nil for a
// body that names a move), and the If-Match tags.
type nodeMoveRequest struct {
	key, id    string
	who        model.Actor
	writer     model.Actor
	person     bool
	parent     *string
	after      *string
	afterNamed bool
	shape      error
	tags       []string
}

// moveNode makes the move in one transaction, joined to the document service, and answers the
// answer, the events its version appended, and each issue whose queries its batch marked owed, which
// the caller drains after the COMMIT.
func (s *server) moveNode(ctx context.Context, move nodeMoveRequest) (nodeMoved, []*model.Event, []string, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return nodeMoved{}, nil, nil, err
	}
	defer tx.Rollback(ctx)
	documentCtx, ledger := s.deps.Docs.Join(ctx, tx)
	defer ledger.Discard()
	at, err := transactionNow(ctx, tx)
	if err != nil {
		return nodeMoved{}, nil, nil, err
	}
	found, end, err := findWrittenNode(ctx, tx, move.who, move.key, move.id)
	if err != nil {
		return nodeMoved{}, nil, nil, err
	}
	moved := moveNodeOf(found, end, move.id)
	if err := readBlockType(ctx, tx, &moved); err != nil {
		return nodeMoved{}, nil, nil, err
	}
	// An ask, a comment and a message move with nothing: R7 answers this once {id} is found under
	// {key}, before every other rule, whoever the caller is.
	if slices.Contains(unmovableKinds, moved.kind) {
		return nodeMoved{}, nil, nil, errorf(http.StatusBadRequest, codeNodeNotMovable,
			"the %s %s does not move: its part_of follows its own row - an ask's the document that holds its block, a comment's and a message's what each answers or the node its anchor is on - so a move would leave a difference no pass clears, and the next restore would undo it; an ask moves with the block that holds it",
			moved.word(), moved.named)
	}
	// A decision that holds a from_ask stands under the ask it settles, whatever a move would
	// store (R2): R7 answers it 400 NODE_NOT_MOVABLE at the same point, whoever the caller is, so
	// an agent's move of a person's such decision is this and not R32's 409. The refusal names the
	// ask where the caller may read it (refs.DecisionFromAsk), and in the same words names none
	// where the caller may not, or where no ask resolves.
	if moved.kind == refs.BlockNodeKind && moved.typ == refs.DecisionSupertag {
		ask, holds, err := refs.DecisionFromAsk(ctx, tx, move.who, moved.id)
		if err != nil {
			return nodeMoved{}, nil, nil, err
		}
		if holds {
			held := "a from_ask"
			if ask != "" {
				held += ", " + ask
			}
			return nodeMoved{}, nil, nil, errorf(http.StatusBadRequest, codeNodeNotMovable,
				"the %s %s does not move: it holds %s, so it stands under the ask it settles and its place follows that from_ask - a move would leave it where the next index pass undoes it; to move it, change or remove its from_ask through a write of its document's text",
				moved.word(), moved.named, held)
		}
	}
	// R7 reads the constraint rule next, before every other rule: a constraint stands only under an
	// issue node, so one moved under any other parent is refused before the body, the If-Match and
	// the writer are read, and a constraint moved under a document's node is 400, never 409.
	var named *moveNode
	if move.parent != nil && *move.parent != "" {
		parent, err := findMoveParent(ctx, tx, move.who, *move.parent)
		if err != nil {
			return nodeMoved{}, nil, nil, err
		}
		if moved.kind == graphstore.KindConstraint && parent.kind != graphstore.KindIssue {
			if err := readBlockType(ctx, tx, &parent); err != nil {
				return nodeMoved{}, nil, nil, err
			}
			return nodeMoved{}, nil, nil, errorf(http.StatusBadRequest, codeInvalidPointer,
				"the constraint %s may not stand under %s, a %s: a constraint stands only under an issue node, the issue whose constraints list it, and under any other node it would stand under no issue",
				moved.named, parent.named, parent.word())
		}
		named = &parent
	}
	if move.shape != nil {
		return nodeMoved{}, nil, nil, move.shape
	}
	if len(move.tags) == 0 {
		return nodeMoved{}, nil, nil, errorf(http.StatusPreconditionRequired, codePreconditionRequired,
			"a move states the placement it read: send If-Match with the placement_seq of %s as a quoted entity tag, as the tree read answers it",
			moved.named)
	}
	// R28: only a person or an architect of the issue moves a requirement, a criterion or a check.
	if err := refuseSpecWriterOfNode(ctx, tx, specWriter{actor: move.writer, person: move.person}, move.key, storedEnd{kind: moved.stored, id: moved.id}, moved.named); err != nil {
		return nodeMoved{}, nil, nil, err
	}
	// R32: an agent moves no person's decision: the :::ask block node of an answered ask, or a
	// decision a person decided.
	if !move.person {
		if err := refusePersonDecisionMove(ctx, tx, moved); err != nil {
			return nodeMoved{}, nil, nil, err
		}
	}
	current, held, err := currentPlacement(ctx, tx, moved)
	if err != nil {
		return nodeMoved{}, nil, nil, err
	}
	// A body that names a parent moves the node under the node read above (the shape refuses an
	// empty one); a body that names none keeps the node's parent.
	parent := current
	switch {
	case named != nil:
		parent = *named
	case !held:
		return nodeMoved{}, nil, nil, errorf(http.StatusBadRequest, codeMoveInput,
			"the %s %s stands under no node, so a move that keeps its parent has none to keep: name parent", moved.word(), moved.named)
	}
	read, err := lockNodeRouteOwners(ctx, tx, move.key, func(q queryer) (moveRead, []string, error) {
		read, err := readMove(ctx, q, moved, parent)
		return read, read.issues(), err
	}, s.betweenNodeRouteReads, func() error {
		return errorf(http.StatusConflict, parentContended,
			"the move of %s under %s met a node under an issue it had not locked at each of %d attempts while this request waited for its locks, so nothing was moved: read the tree and move it again",
			moved.named, parent.named, parentLockAttempts)
	})
	if err != nil {
		return nodeMoved{}, nil, nil, err
	}
	if !matchesPlacement(move.tags, read.placementSeq) {
		return nodeMoved{}, nil, nil, errorf(http.StatusPreconditionFailed, codePreconditionFailed,
			"the %s %s stands at placement_seq %s, which no If-Match tag names: its parent has changed since it was read, so nothing was moved; read the tree and move it again",
			moved.word(), moved.named, placementWords(read.placementSeq))
	}
	// A reparent can take a child out of the route's tree while the request waits for its locks.
	// Recheck that scope under the locks before changing its placement.
	if moved.kind == graphstore.KindIssue && moved.id != move.key {
		below, err := issueNodeBelow(ctx, tx, move.who, moved.id, move.key)
		if err != nil {
			return nodeMoved{}, nil, nil, err
		}
		if !below {
			return nodeMoved{}, nil, nil, nodeNotFound(move.key, move.id)
		}
	}
	reparents := read.parentKind != parent.stored || read.parentID != parent.id
	if reparents {
		if err := s.refuseMoveParent(ctx, tx, move.key, moved, parent, read); err != nil {
			return nodeMoved{}, nil, nil, err
		}
	}
	placed, err := placeAmongSiblings(ctx, tx, move, moved, parent)
	if err != nil {
		return nodeMoved{}, nil, nil, err
	}
	block, err := readMovedBlock(ctx, tx, moved)
	if err != nil {
		return nodeMoved{}, nil, nil, err
	}
	var published []*model.Event
	if block != nil {
		event, err := s.moveBlockText(documentCtx, tx, ledger, move, moved, parent, block, placed)
		if err != nil {
			return nodeMoved{}, nil, nil, err
		}
		if event != nil {
			published = append(published, event)
		}
	}
	// The queries of the issue the node stood under move with it, and those of the issue a lens, a
	// finding, a risk or a constraint comes to stand under, whose issue follows its part_of.
	marks := []string{move.key}
	if issue := read.parentIssue(); issue != "" && issue != move.key && standingKind(moved.kind) {
		marks = append(marks, issue)
	}
	write := moveWrite{
		moved: moved, from: storedEnd{kind: read.parentKind, id: read.parentID}, to: parent,
		reparents: reparents, rank: placed.rank, ranked: placed.ranked, stored: block != nil && block.placement != refs.PlacementStored,
		block: block, at: at, writer: move.writer, marks: marks,
	}
	seq := read.placementSeq
	marked := map[string]*bool{}
	if err := ledger.Queue(func(b *store.WriteBatch) error {
		queueNodeMove(b, write, marked, &seq)
		return nil
	}); err != nil {
		return nodeMoved{}, nil, nil, err
	}
	if err := ledger.Commit(ctx); err != nil {
		return nodeMoved{}, nil, nil, err
	}
	if block != nil && block.textChanged {
		s.deps.Docs.ScheduleSettlement(block.document)
	}
	var drain []string
	for _, issue := range marks {
		if took := marked[issue]; took != nil && *took {
			drain = append(drain, issue)
		}
	}
	answer := nodeMoved{Parent: parent.address(), After: placed.follows, PlacementSeq: 0}
	if seq != nil {
		answer.PlacementSeq = *seq
	}
	return answer, published, drain, nil
}

// kindWord is a node's kind as a refusal names it: a finding for a lens's block, the kind itself
// for every other.
func kindWord(kind string) string {
	if kind == graphstore.KindLensBlock {
		return "finding"
	}
	return kind
}

// placementWords is a placement_seq as a refusal names it, "none" for a node that holds no part_of.
func placementWords(seq *int64) string {
	if seq == nil {
		return "none"
	}
	return strconv.FormatInt(*seq, 10)
}

// matchesPlacement reports whether one of the If-Match header values tags names the placement_seq
// seq as a strong entity tag (RFC 9110 section 13.1.1, compared strongly): `"<seq>"`. A weak tag,
// `*`, and any tag of a node that holds no placement_seq match nothing.
func matchesPlacement(tags []string, seq *int64) bool {
	if seq == nil {
		return false
	}
	want := `"` + strconv.FormatInt(*seq, 10) + `"`
	for _, header := range tags {
		for _, tag := range strings.Split(header, ",") {
			if strings.TrimSpace(tag) == want {
				return true
			}
		}
	}
	return false
}

// refusePersonDecisionMove is R32's refusal of an agent's move whose {id} names a person's
// decision (409 ANSWERED_ASK_KEPT): the `:::ask` block node of an answered ask, or a decision block
// whose decided_by names a person (docs.PersonDecisionOfBlock). An ask's own node never reaches it:
// R7 answers it 400 NODE_NOT_MOVABLE first. The refusal quotes the ask's question or the decision's
// words and names the node. Any other node passes.
func refusePersonDecisionMove(ctx context.Context, q queryer, moved moveNode) error {
	if moved.kind != refs.BlockNodeKind {
		return nil
	}
	decision, decided, err := docs.PersonDecisionOfBlock(ctx, q, moved.stored, moved.id, moved.typ)
	if err != nil || !decided {
		return err
	}
	return errorf(http.StatusConflict, codeAnsweredAskKept,
		"%s %s, %q, is a person's decision, and an agent cannot move it: a person moves it, or an agent acting under a token that holds the person scope",
		decision.What, moved.named, decision.Words)
}

// currentPlacementStatement reads the far end and the seq of the current part_of of the node the
// store keys as $1 and $2: the newest tuple to each far end, kept while it asserts
// (graphstore.CurrentTupleRead), and of those the newest.
var currentPlacementStatement = `
	select newest.to_kind, newest.to_id, newest.seq from (
		` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.seq, t.asserted`,
	`t.from_kind = $1 and t.from_id = $2 and t.kind = '`+graphstore.TuplePartOf+`' and t.to_kind <> '`+graphstore.KindValue+`'`) + `
	) newest
	where newest.asserted
	order by newest.seq desc limit 1`

// currentPlacement is the node moved stands under now, as a far end, and whether it stands under
// any.
func currentPlacement(ctx context.Context, q queryer, moved moveNode) (moveNode, bool, error) {
	var parent moveNode
	var seq int64
	err := q.QueryRow(ctx, currentPlacementStatement, moved.stored, moved.id).Scan(&parent.kind, &parent.id, &seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return moveNode{}, false, nil
	}
	if err != nil {
		return moveNode{}, false, fmt.Errorf("read where %s stands: %w", moved.named, err)
	}
	parent.stored = refs.StoredKindOf(parent.kind)
	parent.named = parent.address()
	return parent, true, nil
}

// goneParentStatement reads, of the node the store keys as $1 and $2, which every read answers as
// gone - gone, or a stale node of a standing kind (refs.GoneOrStaleRow) - for a reader ($3) the
// node's own kind's predicate admits but for that (refs.VisibleGoneRow), whether it is so.
var goneParentStatement = `
	select true from graph_nodes held
	where held.kind = $1 and held.id = $2 and ` + refs.GoneOrStaleRow("held") + `
	  and ` + refs.VisibleGoneRow("held", "$3")

// orphanedDocumentStatement reads whether the store holds a node of the document whose ref_key is
// $1, not gone, at an address no artifacts row holds, as graphcensus's orphanedDocumentNode reads
// one: a document a removal took the row of.
var orphanedDocumentStatement = `
	select true from graph_nodes n
	where n.kind = '` + graphstore.KindArtifact + `' and n.id = $1 and n.state <> '` + graphstore.StateGone + `'
	  and not exists (select 1 from artifacts a where a.ref_key = $1)`

// findMoveParent is the node address names as a move's parent, read for who: the node the read
// registry answers (refs.Node), of any issue, as a tuple's far end names it (farEndOf); else the
// node the store holds gone or stale for a reader who may see it, which the move refuses as a
// parent the walk does not reach; else the node of a document at an address no artifacts row
// holds, which the move refuses as a document a removal could take. Anything else is 404
// NOT_FOUND, naming the address.
func findMoveParent(ctx context.Context, q queryer, who model.Actor, address string) (moveNode, error) {
	kind, id, ok, err := resolvedNodeAddress(ctx, q, address)
	if err != nil {
		return moveNode{}, err
	}
	notFound := errorf(http.StatusNotFound, "NOT_FOUND", "no node %q to stand under", address)
	if !ok {
		return moveNode{}, notFound
	}
	node, found, err := refs.Node(ctx, q, who, kind, id, true)
	if err != nil {
		return moveNode{}, err
	}
	if found {
		end := farEndOf(node, address)
		return moveNode{kind: end.kind, stored: refs.StoredKindOf(end.kind), id: end.id, named: cmp.Or(node.Ref, address)}, nil
	}
	parent := moveNode{kind: kind, stored: refs.StoredKindOf(kind), id: id, named: address}
	var held bool
	err = q.QueryRow(ctx, goneParentStatement, parent.stored, parent.id, refs.VisibilityArg(who)).Scan(&held)
	if err == nil {
		return parent, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return moveNode{}, fmt.Errorf("read whether %s is gone: %w", address, err)
	}
	if kind == graphstore.KindArtifact {
		err = q.QueryRow(ctx, orphanedDocumentStatement, id).Scan(&held)
		if err == nil {
			return parent, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return moveNode{}, fmt.Errorf("read whether %s is a document with no row: %w", address, err)
		}
	}
	return moveNode{}, notFound
}

// moveSide is one node a move's read reaches: the kind and the id its tuples name it by, how far
// from its start (the moved node or the parent) it stands, the issue it stands under
// (refs.GoneStandingIssueSQL, "" for none), and whether every read answers it as gone.
type moveSide struct {
	kind, id string
	depth    int
	issue    string
	gone     bool
}

// moveRead is what a move reads under its owner locks, and what it reads to learn which locks to
// take: the moved node and its part_of subtree (beneath, the moved node first), the parent and its
// part_of chain (above, the parent first), the far end of the moved node's current part_of, and
// its placement_seq (readMove), nil where it holds none.
type moveRead struct {
	beneath, above       []moveSide
	parentKind, parentID string
	placementSeq         *int64
}

// issues are the issues the read's nodes stand under, each once, in the order the read met them.
func (read moveRead) issues() []string {
	var issues []string
	for _, side := range slices.Concat(read.beneath, read.above) {
		if side.issue != "" && !slices.Contains(issues, side.issue) {
			issues = append(issues, side.issue)
		}
	}
	return issues
}

// parentIssue is the issue the parent stands under, "" for none.
func (read moveRead) parentIssue() string {
	if len(read.above) == 0 {
		return ""
	}
	return read.above[0].issue
}

// standingKind reports whether a node the graph addresses as kind reads its issue off its part_of
// chain (refs.StandingKinds): a lens, a finding, a risk or a constraint.
func standingKind(kind string) bool { return slices.Contains(refs.StandingKinds(), kind) }

// moveReadStatement is the read of a move of the node the store keys as $1 and $2 under the node
// its tuples name $3 and $4: the moved node and every node its current part_of tuples hold beneath
// it at any depth, as nodesBeneath walks them, each once; the parent and every node up its current
// part_of chain, at most $5 deep, and none where $3 is null, as for a delete, which names no parent;
// each with the issue it stands under, read as a write's test of a gone node reads it
// (refs.GoneStandingIssueSQL: a document no row stands at stands under the issue its address
// spells), and whether every read answers it as gone (refs.GoneOrStaleRow).
var moveReadStatement = `
	with recursive beneath (kind, id) as (
		select $1::text, $2::text
		union
		select placed.from_kind, placed.from_id
		from beneath
		cross join lateral (
			` + graphstore.CurrentTupleRead(`t.to_kind, t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.asserted`,
	`t.to_kind in (beneath.kind, `+graphstore.AddressKindSQL("beneath.kind", "beneath.id")+`)
				and t.to_id = beneath.id and t.kind = '`+graphstore.TuplePartOf+`' and t.to_kind <> '`+graphstore.KindValue+`'`) + `
		) placed
		where placed.asserted
	), above (kind, id, depth) as (
		select $3::text, $4::text, 0 where $3::text is not null
		union all
		select placed.to_kind, placed.to_id, above.depth + 1
		from above
		cross join lateral (` + refs.CurrentFarEndSQL(refs.StoredKindSQL("above.kind"), "above.id", graphstore.TuplePartOf) + `) placed
		where above.depth < $5
	)
	select reached.side, reached.kind, reached.id, reached.depth,
	       coalesce(` + refs.GoneStandingIssueSQL("reached.kind", "reached.id") + `, ''),
	       coalesce((select ` + refs.GoneOrStaleRow("held") + ` from graph_nodes held
	                 where held.kind = ` + refs.StoredKindSQL("reached.kind") + ` and held.id = reached.id), false)
	from (
		select 0 as side, kind, id, 0 as depth from beneath
		union all
		select 1, kind, id, depth from above
	) reached
	order by reached.side, reached.depth, reached.kind, reached.id`

// readMove reads moveRead for the move of moved under parent, through q; for a parent of no id, as
// a delete reads it, the moved node's subtree alone.
func readMove(ctx context.Context, q queryer, moved, parent moveNode) (moveRead, error) {
	var parentKind, parentID any
	if parent.id != "" {
		parentKind, parentID = parent.stored, parent.id
	}
	rows, err := q.Query(ctx, moveReadStatement, moved.stored, moved.id, parentKind, parentID, moveChainDepth)
	if err != nil {
		return moveRead{}, fmt.Errorf("read the subtree of %s and the chain of %s: %w", moved.named, parent.named, err)
	}
	var read moveRead
	for rows.Next() {
		var side int
		var reached moveSide
		if err := rows.Scan(&side, &reached.kind, &reached.id, &reached.depth, &reached.issue, &reached.gone); err != nil {
			rows.Close()
			return moveRead{}, fmt.Errorf("read the subtree of %s and the chain of %s: %w", moved.named, parent.named, err)
		}
		reached.kind = graphstore.AddressKind(reached.kind, reached.id)
		if side == 0 {
			read.beneath = append(read.beneath, reached)
		} else {
			read.above = append(read.above, reached)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return moveRead{}, fmt.Errorf("read the subtree of %s and the chain of %s: %w", moved.named, parent.named, err)
	}
	var seq int64
	err = q.QueryRow(ctx, currentPlacementStatement, moved.stored, moved.id).Scan(&read.parentKind, &read.parentID, &seq)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return moveRead{}, fmt.Errorf("read where %s stands: %w", moved.named, err)
	default:
		read.placementSeq = &seq
	}
	if read.placementSeq == nil && moved.stored == graphstore.KindIssue {
		if err := q.QueryRow(ctx, childOfPlacementStatement, moved.id).Scan(&read.placementSeq); err != nil {
			return moveRead{}, fmt.Errorf("read the child_of %s hangs from: %w", moved.named, err)
		}
	}
	return read, nil
}

// childOfPlacementSQL is the placement_seq of a child issue that holds no current part_of (R7,
// R8): the seq of the current child_of out of the node the SQL expressions kind and id name, where
// kind names an issue's node, read as a current relation is read (graphstore.CurrentTupleRead)
// over the tuples narrow keeps, "true" for the head; null for a node of any other kind and for an
// issue that holds no child_of. A tag of it is stale once a PATCH writes the child's first part_of,
// whose seq the node's placement_seq then is, or a reparent appends a new child_of.
func childOfPlacementSQL(kind, id, narrow string) string {
	return `(select held.seq from (
		` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.seq, t.asserted`,
		kind+` = '`+graphstore.KindIssue+`' and t.from_kind = '`+graphstore.KindIssue+`' and t.from_id = `+id+`
			and t.kind = '`+graphstore.TupleChildOf+`' and t.to_kind <> '`+graphstore.KindValue+`' and `+narrow) + `
		) held
		where held.asserted
		order by held.seq desc limit 1)`
}

// childOfPlacementStatement reads childOfPlacementSQL of the issue node $1 at the head.
var childOfPlacementStatement = `select ` + childOfPlacementSQL(`'`+graphstore.KindIssue+`'`, `$1::text`, `true`)

// lockNodeRouteOwners takes the owner locks of a node route's write (R7: the PATCH and the DELETE)
// and answers what it reads under them: it reads once with no lock held (read), to learn the issue
// each node it reaches stands under, then, under a savepoint, takes the owner locks of key and of
// every such issue in one call of lockIssueOwners, which takes them in the store's lock order, and
// reads again. Where the second read reaches a node under an issue it did not lock - a write that
// committed in between moved one there - it gives every lock of the attempt back through the
// savepoint's rollback and starts over from the first read, within parentLockAttempts, as
// lockPatchOwnersAndParent does; a write that meets such an issue at each attempt is contended's
// 409 PARENT_CONTENDED, and nothing is stored. It takes no owner lock before that call. pause, where
// the server holds one (server.betweenNodeRouteReads), runs after each first read, with the
// attempt's number from 1, before the attempt takes a lock.
func lockNodeRouteOwners[T any](ctx context.Context, tx pgx.Tx, key string, read func(q queryer) (T, []string, error),
	pause func(ctx context.Context, attempt int), contended func() error) (T, error) {
	var none T
	for attempt := range parentLockAttempts {
		_, locked, err := read(tx)
		if err != nil {
			return none, err
		}
		if pause != nil {
			pause(ctx, attempt+1)
		}
		savepoint, err := tx.Begin(ctx)
		if err != nil {
			return none, err
		}
		if _, err := lockIssueOwners(ctx, savepoint, key, slices.DeleteFunc(slices.Clone(locked), func(issue string) bool { return issue == key })...); err != nil {
			_ = savepoint.Rollback(ctx)
			return none, err
		}
		second, reached, err := read(savepoint)
		if err != nil {
			_ = savepoint.Rollback(ctx)
			return none, err
		}
		if !slices.ContainsFunc(reached, func(issue string) bool { return issue != key && !slices.Contains(locked, issue) }) {
			return second, savepoint.Commit(ctx)
		}
		if err := savepoint.Rollback(ctx); err != nil {
			return none, err
		}
	}
	return none, contended()
}

// refuseMoveParent holds a move that changes the moved node's parent to the rules R7 reads of the
// new parent, under the owner locks, on read: a parent inside the moving subtree, which would make
// a cycle; a parent outside the issue's own tree, which neither stands under key nor under an
// issue above or below it (refs.IssueAncestors); a node that reads its issue from its own row
// moved under a node of an issue below key, where no walk would reach it; each 400
// INVALID_POINTER. Then a parent that is, or stands under, the node of a document a removal could
// take, under which only a lens, a finding or a risk stand (refuseRemovableParent, 409
// PARENT_DOCUMENT_REMOVABLE); and last a parent the walk of the issue it stands under does not
// reach (refuseUnreachedParent, 409 PARENT_NOT_REACHED).
func (s *server) refuseMoveParent(ctx context.Context, tx pgx.Tx, key string, moved, parent moveNode, read moveRead) error {
	for _, node := range read.beneath {
		if node.kind == parent.stored && node.id == parent.id {
			return errorf(http.StatusBadRequest, codeInvalidPointer,
				"the move of %s under %s would make a cycle: %s stands in the subtree of %s, so it cannot hold it",
				moved.named, parent.named, parent.named, moved.named)
		}
	}
	issue := read.parentIssue()
	if issue == "" {
		return errorf(http.StatusBadRequest, codeInvalidPointer,
			"the move of %s under %s takes it outside the tree of %s: %s stands under no issue", moved.named, parent.named, key, parent.named)
	}
	if issue != key {
		above, err := refs.IssueAncestors(ctx, tx, key)
		if err != nil {
			return err
		}
		if !slices.Contains(above, issue) {
			below, err := refs.IssueAncestors(ctx, tx, issue)
			if err != nil {
				return err
			}
			if !slices.Contains(below, key) {
				return errorf(http.StatusBadRequest, codeInvalidPointer,
					"the move of %s under %s takes it outside the tree of %s: %s stands under %s, which is neither %s nor an issue above or below it",
					moved.named, parent.named, key, parent.named, issue, key)
			}
			if !standingKind(moved.kind) {
				return errorf(http.StatusBadRequest, codeInvalidPointer,
					"the %s %s may not stand under %s, which stands under %s, an issue below %s: it reads its issue from its own row, so it would still stand under %s, and neither the walk of %s nor that of %s would reach it there",
					moved.word(), moved.named, parent.named, issue, key, key, key, issue)
			}
		}
	}
	if err := refuseRemovableParent(ctx, tx, moved, parent, read); err != nil {
		return err
	}
	return refuseUnreachedParent(ctx, tx, moved, parent, read)
}

// removableDocumentStatement reads, of the document whose ref_key is $1, whether a removal could
// take it (R7): a document of kind doc of an issue, other than the issue's spec, whose versions
// hold nothing - exactly one version, whose markdown is empty - the version tests of
// artifactIsUntouched; or a document node no artifacts row stands at the address of, read off
// graph_nodes where it is not gone, which a removal already took the row of. It answers no row for
// any other document.
var removableDocumentStatement = `
	select true
	from (select $1::text as ref_key) addressed
	left join artifacts a on a.ref_key = addressed.ref_key
	where case when a.id is null
		then exists (select 1 from graph_nodes n where n.kind = '` + graphstore.KindArtifact + `' and n.id = addressed.ref_key
		             and n.state <> '` + graphstore.StateGone + `')
		else a.kind = 'doc' and a.issue_key is not null and not a.is_primary
		     and (select count(*) from artifact_versions v where v.artifact_id = a.id) = 1
		     and (select v.markdown from artifact_versions v where v.artifact_id = a.id limit 1) = ''
		end`

// reasoningKinds are the kinds that may stand on a document a removal could take: a lens on it,
// and a lens, a finding or a risk at any depth under such a lens.
var reasoningKinds = []string{graphstore.KindLens, graphstore.KindLensBlock, graphstore.KindRisk}

// refuseRemovableParent is the 409 PARENT_DOCUMENT_REMOVABLE of a move to a parent that is, or
// stands under through its part_of chain, the node of a document a removal could take
// (removableDocumentStatement), where the moved node may not stand (R7). On such a document's node
// only a lens stands, and only a lens whose every node of its subtree is a lens, a finding or a
// risk; on a lens that stands on such a document, or at any depth under one, only a lens, a finding
// or a risk whose subtree holds those kinds alone. The refusal names the moved node and its kind,
// the parent and the document, and where the moved node may stand there and a node of its subtree
// may not, says so and names no node of that subtree, which the caller may not see whole.
func refuseRemovableParent(ctx context.Context, q queryer, moved, parent moveNode, read moveRead) error {
	at := slices.IndexFunc(read.above, func(node moveSide) bool { return node.kind == graphstore.KindArtifact })
	if at < 0 {
		return nil
	}
	document := read.above[at]
	var removable bool
	err := q.QueryRow(ctx, removableDocumentStatement, document.id).Scan(&removable)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read whether a removal could take %s: %w", document.id, err)
	}
	admitted := []string{graphstore.KindLens}
	if at > 0 {
		for _, between := range read.above[:at] {
			if !slices.Contains(reasoningKinds, between.kind) {
				admitted = nil
				break
			}
		}
		if admitted != nil && read.above[at-1].kind != graphstore.KindLens {
			admitted = nil
		}
		if admitted != nil {
			admitted = reasoningKinds
		}
	}
	where := fmt.Sprintf("%s, which is or stands under the node of the document %s, a document a removal could take", parent.named, document.id)
	if !slices.Contains(admitted, moved.kind) {
		return errorf(http.StatusConflict, codeParentDocumentRemovable,
			"the %s %s may not stand under %s: a removal of the document would leave it live behind no row, and only %s stands there, so nothing was moved",
			moved.word(), moved.named, where, admittedWords(admitted))
	}
	for _, node := range read.beneath[1:] {
		if !slices.Contains(reasoningKinds, node.kind) {
			return errorf(http.StatusConflict, codeParentDocumentRemovable,
				"the %s %s may stand under %s, but its subtree holds a node of a kind that may not stand there: only a lens, a finding or a risk stands under such a document, so nothing was moved",
				moved.word(), moved.named, where)
		}
	}
	return nil
}

// admittedWords names the kinds that may stand on a document a removal could take, for a refusal.
func admittedWords(admitted []string) string {
	switch len(admitted) {
	case 0:
		return "no node of any kind, but a lens on the document itself and a lens, a finding or a risk under such a lens,"
	case 1:
		return "a lens whose subtree holds lenses, findings and risks alone"
	}
	return "a lens, a finding or a risk"
}

// refuseUnreachedParent is the 409 PARENT_NOT_REACHED of a move to a parent that the walk of the
// issue it stands under (docs.WalkIssueTree, at the head of the move's transaction) does not reach,
// naming the parent and why: it is gone; it is a comment or a message the conversation test (R2)
// does not pass, such as an agent's status note or its comment on text no person answered; or its
// own parent is not reached, such as a block of such a comment. So every node the route moves stays
// in the walk of its issue, and no move changes which nodes the walk reaches.
func refuseUnreachedParent(ctx context.Context, tx pgx.Tx, moved, parent moveNode, read moveRead) error {
	tree, err := docs.WalkIssueTree(ctx, tx, read.parentIssue(), refs.TreeCut{})
	if err != nil {
		return err
	}
	for _, node := range tree.Nodes {
		if node.Kind == parent.stored && node.ID == parent.id {
			return nil
		}
	}
	reason := "its own parent is not reached by the walk"
	switch {
	case len(read.above) > 0 && read.above[0].gone:
		reason = "it is gone"
	case parent.kind == graphstore.KindComment || parent.kind == graphstore.KindMessage:
		reason = "it is a " + parent.kind + " the conversation test does not pass (R2), such as an agent's status note or a comment on text no person answered"
	}
	return errorf(http.StatusConflict, codeParentNotReached,
		"the move of %s under %s is refused: the walk of %s does not reach %s, because %s, so the moved node would stand where no read answers it; nothing was moved",
		moved.named, parent.named, read.parentIssue(), parent.named, reason)
}

// moveSibling is one ranked child of the parent a move places a node under: its kind and id as its
// tuples name it, its rank, its block's type and id and the document whose text holds it, each ""
// for a node that is no block of a document.
type moveSibling struct {
	kind, id, rank       string
	typ, block, document string
}

// siblingsStatement reads every child the node its tuples name $1 and $2 holds through its current
// part_of (graphstore.CurrentTupleRead, into a node) that holds a rank, with that rank, and, for a
// block of a document, its type, its block id and the document whose text holds it
// (refs.BlockDocumentSQL), in the order of their ranks, compared byte for byte.
var siblingsStatement = `
	select child.from_kind, child.from_id, ranked.held, coalesce(d.type, ''), coalesce(d.block_id, ''),
	       coalesce(` + refs.BlockDocumentSQL("d") + `::text, '')
	from (
		` + graphstore.CurrentTupleRead(`t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.asserted`,
	`t.to_kind in ($1, `+graphstore.AddressKindSQL("$1", "$2")+`) and t.to_id = $2 and t.kind = '`+graphstore.TuplePartOf+`'`) + `
	) child
	join lateral (
		select case when t.asserted then t.to_id end as held from graph_tuples t
		where t.from_kind = child.from_kind and t.from_id = child.from_id and t.kind = '` + refs.AttributeRank + `'
		  and t.to_kind = '` + graphstore.KindValue + `'
		order by t.seq desc limit 1
	) ranked on ranked.held is not null
	left join document_blocks d on d.node_kind = child.from_kind and d.node_address = child.from_id
	where child.asserted
	order by ranked.held collate "C"`

// moveSiblings reads siblingsStatement for parent.
func moveSiblings(ctx context.Context, q queryer, parent moveNode) ([]moveSibling, error) {
	rows, err := q.Query(ctx, siblingsStatement, parent.stored, parent.id)
	if err != nil {
		return nil, fmt.Errorf("read the ranked children of %s: %w", parent.named, err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (moveSibling, error) {
		var sibling moveSibling
		err := row.Scan(&sibling.kind, &sibling.id, &sibling.rank, &sibling.typ, &sibling.block, &sibling.document)
		return sibling, err
	})
}

// movePlace is where a move puts its node among the ranked children of its parent: the rank it
// gives it and whether it holds one, the ranked sibling it now follows (nil for none), and the
// ranked children of the parent in their new order, the moved node among them, each with its block
// and its document.
type movePlace struct {
	rank     string
	ranked   bool
	follows  *string
	siblings []moveSibling
}

// placeAmongSiblings is where the move puts moved among the ranked children of parent (R7): a node
// that holds no rank keeps none, and takes no after; a ranked node takes a rank in its own
// document's band (its rank's first byte, refs.RankBandSpec and the rest) between after's rank and
// the next ranked sibling's of its band, or the band's own bounds where it has none, by
// rank.Between, compared byte for byte. after null puts it first in its band, but right after the
// issue's memo for a node of the spec's band under the issue; no after puts it after every ranked
// sibling of its band. An after that is no child of the parent, the moved node itself, a node that
// holds no rank or one of another band, and an after on a node that holds none, are each 400
// INVALID_POINTER naming after.
func placeAmongSiblings(ctx context.Context, q queryer, move nodeMoveRequest, moved, parent moveNode) (movePlace, error) {
	var held *string
	if err := q.QueryRow(ctx, `
		select case when t.asserted then t.to_id end from graph_tuples t
		where t.from_kind = $1 and t.from_id = $2 and t.kind = '`+refs.AttributeRank+`' and t.to_kind = '`+graphstore.KindValue+`'
		order by t.seq desc limit 1`, moved.stored, moved.id).Scan(&held); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return movePlace{}, fmt.Errorf("read the rank of %s: %w", moved.named, err)
	}
	refuseAfter := func(why string) error {
		return errorf(http.StatusBadRequest, codeInvalidPointer, "after %s: %s, so nothing was moved", afterWords(move.after), why)
	}
	if held == nil || *held == "" {
		if move.afterNamed {
			return movePlace{}, refuseAfter(fmt.Sprintf("the %s %s holds no rank, so it stands after the ranked children of its parent by when it was made, and no after places it", moved.word(), moved.named))
		}
		return movePlace{}, nil
	}
	band := (*held)[0]
	siblings, err := moveSiblings(ctx, q, parent)
	if err != nil {
		return movePlace{}, err
	}
	siblings = slices.DeleteFunc(siblings, func(sibling moveSibling) bool { return sibling.kind == moved.stored && sibling.id == moved.id })
	inBand := func(sibling moveSibling) bool { return sibling.rank[0] == band }
	// lower is the index among siblings of the one the node goes right after, -1 for none.
	lower := -1
	switch {
	case move.after != nil:
		kind, id, ok, err := resolvedNodeAddress(ctx, q, *move.after)
		if err != nil {
			return movePlace{}, err
		}
		if ok && refs.StoredKindOf(kind) == moved.stored && id == moved.id {
			return movePlace{}, refuseAfter("it is the moved node itself")
		}
		lower = slices.IndexFunc(siblings, func(sibling moveSibling) bool {
			return ok && sibling.kind == refs.StoredKindOf(kind) && sibling.id == id
		})
		if lower < 0 {
			return movePlace{}, refuseAfter(fmt.Sprintf("it is no ranked child of %s: after names a node that holds a rank and stands under the parent the moved node has once the move is taken", parent.named))
		}
		if !inBand(siblings[lower]) {
			return movePlace{}, refuseAfter(fmt.Sprintf("it holds a rank of band %c, and %s stands in band %c: a rank stays in its document's band, so a node moves among the siblings of its own document",
				siblings[lower].rank[0], moved.named, band))
		}
	case move.afterNamed:
		// First in the band: after the last sibling of an earlier band, or after the memo where the
		// band is the spec's and the parent the issue.
		for index, sibling := range siblings {
			if sibling.rank[0] < band {
				lower = index
			}
		}
		if band == refs.RankBandSpec && parent.kind == graphstore.KindIssue {
			if first := slices.IndexFunc(siblings, inBand); first >= 0 && siblings[first].typ == "callout" {
				lower = first
			}
		}
	default:
		for index, sibling := range siblings {
			if sibling.rank[0] <= band {
				lower = index
			}
		}
	}
	below := string(band)
	if lower >= 0 && inBand(siblings[lower]) {
		below = siblings[lower].rank
	}
	above := string(band + 1)
	if next := lower + 1; next < len(siblings) && inBand(siblings[next]) {
		above = siblings[next].rank
	}
	place := movePlace{rank: rank.Between(below, above), ranked: true}
	if lower >= 0 {
		follows := storeAddress(graphstore.AddressKind(siblings[lower].kind, siblings[lower].id), siblings[lower].id)
		place.follows = &follows
	}
	place.siblings = slices.Insert(siblings, lower+1, moveSibling{kind: moved.stored, id: moved.id, rank: place.rank})
	return place, nil
}

// afterWords is after as a refusal names it.
func afterWords(after *string) string {
	if after == nil {
		return "null"
	}
	return strconv.Quote(*after)
}

// movedBlock is what a move reads of a block of a document it moves: the artifact whose row holds
// it (the document, or an issue room's tree), the document whose text holds it, that document's
// ref_key and band, the block's id and type, the placement its node holds, and whether the move
// changed its text.
type movedBlock struct {
	artifact, document, refKey string
	band                       byte
	issue                      string
	id, typ, placement         string
	textChanged                bool
}

// movedBlockStatement reads movedBlock of the block whose node the store keys as $1 and $2.
var movedBlockStatement = `
	select d.artifact_id::text, ` + refs.BlockDocumentSQL("d") + `::text, document.ref_key, ` + refs.DocumentBandSQL("document") + `,
	       coalesce(document.issue_key, ''), d.block_id, d.type,
	       coalesce((select case when t.asserted then t.to_id end from graph_tuples t
	                 where t.from_kind = $1 and t.from_id = $2 and t.kind = '` + refs.AttributePlacement + `'
	                   and t.to_kind = '` + graphstore.KindValue + `'
	                 order by t.seq desc limit 1), '')
	from document_blocks d
	join artifacts document on document.id = ` + refs.BlockDocumentSQL("d") + `
	where d.node_kind = $1 and d.node_address = $2
	limit 1`

// readMovedBlock is movedBlock of moved, nil for a node that is no block of a document: a lens's
// finding, or a node of any other kind.
func readMovedBlock(ctx context.Context, q queryer, moved moveNode) (*movedBlock, error) {
	if moved.kind != refs.BlockNodeKind {
		return nil, nil
	}
	var block movedBlock
	var band string
	err := q.QueryRow(ctx, movedBlockStatement, moved.stored, moved.id).Scan(&block.artifact, &block.document, &block.refKey, &band,
		&block.issue, &block.id, &block.typ, &block.placement)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the block of %s: %w", moved.named, err)
	}
	block.band = band[0]
	return &block, nil
}

// parentAttributeTypes are the typed blocks whose type declares the parent attribute
// (pmdoc.ParentAttribute), whose text a move makes name the block's new parent.
var parentAttributeTypes = []string{"requirement", "criterion", "check", "plan_task"}

// moveBlockText writes the text of the move of block where Markdown can show its new place, and
// a typed block's parent attribute, into the document that holds it (docs.Service.MoveBlockText),
// under the room locks, and, where that changed the document, records the move's stored block in
// the transaction (docs.Service.HoldStoredBlock) and writes the document's version, whose
// artifact.version it answers, as the edit route writes one (SnapshotVersion,
// commitArtifactVersionEvent), which moves the document's open approval request. The text nests
// under the document's own top level - the issue's node for an issue's spec, Verification, plan
// or tree fragment, the document's node for any other - and under a block of the same document's
// text; under any other parent it keeps its place (pmdoc.NodeTextMove.Nest).
func (s *server) moveBlockText(documentCtx context.Context, tx pgx.Tx, ledger *docs.Ledger, move nodeMoveRequest, moved, parent moveNode, block *movedBlock, placed movePlace) (*model.Event, error) {
	text := pmdoc.NodeTextMove{Block: block.id}
	top := graphstore.KindArtifact
	topID := block.refKey
	if block.band != refs.RankBandOther && block.issue != "" {
		top, topID = graphstore.KindIssue, block.issue
	}
	switch {
	case parent.kind == top && parent.id == topID:
		text.Nest = true
	case parent.kind == refs.BlockNodeKind:
		var document, blockID string
		err := tx.QueryRow(documentCtx, `
			select `+refs.BlockDocumentSQL("d")+`::text, d.block_id from document_blocks d
			where d.node_kind = $1 and d.node_address = $2 limit 1`, parent.stored, parent.id).Scan(&document, &blockID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return nil, fmt.Errorf("read the document of %s: %w", parent.named, err)
		case document == block.document:
			text.Nest, text.Container = true, blockID
		}
	}
	for _, sibling := range placed.siblings {
		if sibling.kind == moved.stored && sibling.id == moved.id {
			text.Siblings = append(text.Siblings, block.id)
			continue
		}
		if sibling.document == block.document && sibling.block != "" {
			text.Siblings = append(text.Siblings, sibling.block)
		}
	}
	if slices.Contains(parentAttributeTypes, block.typ) {
		text.RewriteParent = true
		switch parent.kind {
		case refs.BlockNodeKind, graphstore.KindAsk, graphstore.KindComment, graphstore.KindMessage:
			if strings.HasPrefix(parent.named, "dispatch://") {
				text.Parent = []string{parent.named}
			}
		}
	}
	changed, err := s.deps.Docs.MoveBlockText(documentCtx, block.document, text, move.writer)
	var emptied *pmdoc.ContainerEmptiedError
	if errors.As(err, &emptied) {
		owner, _, _ := strings.Cut(moved.id, "#")
		kind := strings.ReplaceAll(emptied.Type, "_", " ")
		return nil, errorf(http.StatusConflict, codeContainerWouldEmpty,
			"the move of %s would leave the %s %s#%s with no block of its own: %s; nothing was moved, and the %s itself moves or goes whole",
			moved.named, kind, owner, emptied.Container, emptied.Error(), kind)
	}
	var unplaced *pmdoc.TextPlaceError
	if errors.As(err, &unplaced) {
		return nil, errorf(http.StatusBadRequest, codeInvalidPointer,
			"the move of %s under %s is refused: %s, so nothing was moved", moved.named, parent.named, unplaced.Error())
	}
	if err != nil || !changed {
		return nil, err
	}
	block.textChanged = true
	if err := s.deps.Docs.HoldStoredBlock(documentCtx, moved.id, refs.StoredBlock{ParentKind: parent.stored, ParentID: parent.id, Rank: placed.rank}); err != nil {
		return nil, err
	}
	written, err := s.deps.Docs.SnapshotVersion(docs.WithDeferredBlockIndex(documentCtx), block.document, move.writer)
	if err != nil || !written.Wrote {
		return nil, err
	}
	artifact, err := loadArtifactRowByID(documentCtx, tx, block.document)
	if err != nil {
		return nil, err
	}
	return s.commitArtifactVersionEvent(documentCtx, tx, ledger, ownerForArtifact(artifact), move.writer, artifact.ID, artifact.Name, written, nil)
}

// moveWrite is the write a move makes once every rule passed, at graphstore.NoEvent, dated at and
// signed by its writer: the retraction of the moved node's part_of to from and the assertion of one
// to to where the move changes its parent; its rank, where it holds one; the placement
// refs.PlacementStored of a block of a document that does not hold it yet; for such a block, its
// document_blocks.parent_block_id, the projection of its part_of: the new parent's block id where
// it is a block of the same artifact, null otherwise; and the mark of the queries of each of marks
// owed, which a converted issue takes (docs.QueueQueryMark).
type moveWrite struct {
	moved     moveNode
	from      storedEnd
	to        moveNode
	reparents bool
	rank      string
	ranked    bool
	stored    bool
	block     *movedBlock
	at        time.Time
	writer    model.Actor
	marks     []string
}

// queueNodeMove puts write on b: its tuples (graphstore.QueueWrite), the projection row of a
// block, the marks, after the tuples so their sequence is the last one the write drew, the read of
// the placement_seq the new part_of took, and the head rows of what it states
// (graphstore.QueueHeads). marked holds, for each issue of marks, whether its mark took; seq the
// new placement_seq.
func queueNodeMove(b *store.WriteBatch, write moveWrite, marked map[string]*bool, seq **int64) {
	tuple := func(kind, toKind, toID string, asserted bool) graphstore.Tuple {
		return graphstore.Tuple{
			FromKind: write.moved.stored, FromID: write.moved.id, Kind: kind, ToKind: toKind, ToID: toID,
			At: write.at, Who: write.writer, SourceEvent: graphstore.NoEvent, Asserted: asserted, Changes: true,
		}
	}
	var tuples []graphstore.Tuple
	if write.reparents {
		if write.from.kind != "" {
			tuples = append(tuples, tuple(graphstore.TuplePartOf, write.from.kind, write.from.id, false))
		}
		tuples = append(tuples, tuple(graphstore.TuplePartOf, write.to.stored, write.to.id, true))
	}
	if write.ranked {
		tuples = append(tuples, tuple(refs.AttributeRank, graphstore.KindValue, write.rank, true))
	}
	if write.stored {
		tuples = append(tuples, tuple(refs.AttributePlacement, graphstore.KindValue, refs.PlacementStored, true))
	}
	if len(tuples) == 0 {
		return
	}
	graphstore.QueueWrite(b, nil, tuples)
	if write.block != nil && write.reparents {
		var parent *string
		if write.to.kind == refs.BlockNodeKind {
			if owner, id, cut := strings.Cut(write.to.id, "#"); cut && owner+"#"+id == write.to.id && strings.HasPrefix(write.moved.id, owner+"#") {
				parent = &id
			}
		}
		b.Queue("project the part_of of the moved block "+write.moved.id, `
			update document_blocks set parent_block_id = $3
			where node_kind = $1 and node_address = $2 and parent_block_id is distinct from $3`,
			write.moved.stored, write.moved.id, parent)
	}
	for _, issue := range write.marks {
		took := false
		marked[issue] = &took
		docs.QueueQueryMark(b, issue, graphstore.NoEvent, &took)
	}
	if write.reparents {
		b.Queue("read the placement_seq of "+write.moved.id, `
			select max(t.seq) from graph_tuples t
			where t.from_kind = $1 and t.from_id = $2 and t.kind = $3 and t.to_kind = $4 and t.to_id = $5 and t.asserted`,
			write.moved.stored, write.moved.id, graphstore.TuplePartOf, write.to.stored, write.to.id).QueryRow(func(row pgx.Row) error {
			var placement int64
			if err := row.Scan(&placement); err != nil {
				return err
			}
			*seq = &placement
			return nil
		})
	}
	graphstore.QueueHeads(b, graphstore.HeadsOf(nil, tuples))
}
