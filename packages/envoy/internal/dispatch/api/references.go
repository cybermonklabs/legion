package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// queueAskParts is docs.Ledger.QueueAskParts bound to this server's URL: the parts of the ask
// this write just wrote - its question, its options and its answer - planned in the read phase
// and queued in the transaction's write phase, written at `at`, with what they moved merged
// into beside, the counted changes the same write already holds, so the event it appends names
// them all. The parts are read with the whole grammar, through a reader of their own
// (refs.NewReaderInScope), which the ledger builds. document is the one the parts' `#id`s
// resolve against for an ask whose insert this transaction has queued
// (refs.PendingAskDocument), nil for an ask already stored, whose row ScopeOf reads.
// questionReconciled says the write queued the reconciliation of the ask's question before the
// parts (queueNewAskReferences, queueEditedReferences); a write that holds the parts alone - an
// answer, a resolution, a restoration - passes false, and the parts' step reconciles the question
// too and settles the ask's row of the forms queue after both (docs.Ledger.QueueAskParts).
func (s *server) queueAskParts(
	ctx context.Context,
	ledger *docs.Ledger,
	ask model.Ask,
	document *refs.PendingDocument,
	at time.Time,
	questionReconciled bool,
	beside model.ReferenceChanges,
) (model.ReferenceChanges, error) {
	return ledger.QueueAskParts(ctx, ask, s.deps.ServerURL, document, at, questionReconciled, beside)
}

// referencesSinceWait bounds how long a since read waits for the safe horizon to pass every event
// committed before the read (events.Horizon.Await). Only an event write of Dispatch's own database
// held open that long keeps the horizon back that long: the horizon waits on no other
// transaction. A poller would rather have the mentions at or below the horizon now, and the rest
// on its next read, than an answer that waits on another event write for as long as it stays open.
const referencesSinceWait = time.Second

// getReferences reads one node's edges in the reference graph: ?to= lists every edge pointing
// at the node (backlinks), ?from= every edge it writes; exactly one is required. ?kind= narrows
// edge types and ?since= keeps mentions introduced after an events.id, which excludes
// structural edges. Cross-project by design: no project filter. A block reference names a
// block node carrying its whole current text, the node its address resolves to (its moved_to
// tuples first, then its document's room, then itself), and lists the edges at the address and at
// that node (referenceEdges); one pinned to a version of its document, or a revision of its ask's
// parts or of its comment, also reads the block as that version or revision recorded it
// (blockReferenceNode, itemBlockReferenceNode). A finding of a lens
// (`dispatch://lens/<uuid>#b-<id>`) is the registry's lens_block node, read as a lens, a risk or a
// constraint is (refs.Node). A document's address that moved to a lens - the pre-mortem
// conversion retires a document and moves its address to the lens it writes in its place - answers
// that lens (movedDocumentNode), as a moved block's old address answers the block it moved to. A
// ?kind= this build does not emit - a newer build's, during a release - has no edges here, and the
// answer lists it in unknown_kinds rather than refusing the read. The edges, with the excerpts of
// their document mentions, are read with JIT off (inReferencesListRead); the node is read before
// them on the pool.
//
// A since read keeps only mentions whose event is at or below the safe horizon
// (events.Horizon), at or below which every event is final. A poller passes the highest
// source_seq it was shown as its next since, and event ids are not commit order: a mention whose
// event drew a lower id and committed after a higher one was read would otherwise fall below
// that since and never be shown. The read caps at the horizon sinceHorizon answers, which is at
// or above every event committed before the read unless an open transaction holds the horizon
// back past referencesSinceWait: so a since read answers every mention committed before it, the
// caller's own included, and one held back answers the mentions at or below the horizon and
// leaves the rest to a later read.
//
// A ?to= reference whose item id or block id is written short is read as the one item of the
// owner the reference names that carries it (wholeReference), so the node answered carries the
// whole id and the page behind it reads its record under that. The owner decides a prefix and
// nothing else: a whole id is read whatever owner the reference names, as this route has always
// read one (refs.loadAskNodes filters on the id alone). That asymmetry is deliberate. A whole id
// already names one row, so an owner that disagrees with it says nothing this read needs; a
// prefix names a row only within some set, and the set the reference itself names is the one
// its writer meant and the only one that can be answered without guessing.
func (s *server) getReferences(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	who := actor.Recorded()
	query, ref, unknownKinds, err := parseReferencesQuery(r, s.deps.ServerURL)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if query.Direction == "in" {
		resolved, ok := s.wholeReference(w, r, ref, who, refs.Viewer{Human: human, ServerURL: s.deps.ServerURL})
		if !ok {
			return
		}
		ref = resolved
		// The whole id of a prefix, under the same kind: parseReferencesQuery has already
		// refused every kind TargetOf stores nothing for, so this one names a node.
		target, _ := refs.TargetOf(ref)
		query.Kind, query.ID = target.Kind, target.ID
	}
	var node model.GraphNode
	// A finding of a lens is a block of no document and of no item: no version or revision of one
	// is on record and no reference pins one (text.ParseRef reads none), so it is read at its
	// address as any node of the registry is, through its own kind's predicate.
	if ref.Kind == "block" && query.Kind != graphstore.KindLensBlock {
		if ref.Item != "" {
			node, err = s.itemBlockReferenceNode(r.Context(), who, query, ref)
		} else {
			node, err = s.blockReferenceNode(r.Context(), who, query, ref)
		}
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if node.Pin != nil && node.Pin.State == "removed" {
			// The graph lists no link to a block no current version holds.
			reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
			answer := graphReferencesRead{Node: graphNodeRead{GraphNode: node}, Edges: []graphEdgeRead{}, UnknownKinds: unknownKinds}
			answer.Node.shape(reader)
			reader.resolve()
			WriteJSON(w, http.StatusOK, answer)
			return
		}
	} else {
		if query.Direction == "out" && ref.Kind == "artifact" {
			if err := s.deps.Store.Pool.QueryRow(r.Context(), `select id::text from artifacts where ref_key = $1`, query.ID).Scan(&query.ID); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					s.writeHandlerError(w, errorf(http.StatusNotFound, "NOT_FOUND", "no document at %s", query.ID))
					return
				}
				s.writeHandlerError(w, err)
				return
			}
		}
		var found bool
		if query.Direction == "in" && ref.Kind == "artifact" {
			if node, found, err = s.movedDocumentNode(r.Context(), who, query.ID); err != nil {
				s.writeHandlerError(w, err)
				return
			}
		}
		if !found {
			node, found, err = refs.Node(r.Context(), s.deps.Store.Pool, who, query.Kind, query.ID, query.Direction == "in")
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
		}
		if !found {
			s.writeHandlerError(w, errorf(http.StatusNotFound, "NOT_FOUND", "no %s at %s", query.Kind, query.ID))
			return
		}
	}
	// When this build knows none of the kinds the caller named, no edge is of them; refs.Edges
	// with no Kinds would read every kind.
	readEdges := len(query.Kinds) > 0 || len(unknownKinds) == 0
	if readEdges && query.Since != nil {
		safe, err := s.sinceHorizon(r.Context())
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		query.Through = &safe
	}
	edges := []model.GraphEdge{}
	if readEdges {
		read, err := s.inReferencesListRead(r.Context(), who, query, node)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		edges = read
	}
	answerIssueWaitsAsNeeds(edges)
	// The node's own text and the text its pin recorded go through the request's reader; each
	// edge's excerpt is a cut, read where it stands (graphEdgeReads).
	trees := wantsDisplayTrees(r)
	reader := s.readDisplayTrees(r.Context(), trees)
	answer := graphReferencesRead{Node: graphNodeRead{GraphNode: node}, Edges: graphEdgeReads(edges, trees), UnknownKinds: unknownKinds}
	answer.Node.shape(reader)
	reader.resolve()
	WriteJSON(w, http.StatusOK, answer)
}

// referenceEdges is the edges the read of query lists beside node, the node it answers, read
// through q, the list read's transaction (inReferencesListRead): the edges at the address the
// query names, each document mention with its stored excerpt (documentMentionExcerpts), and,
// where that address is a block's that resolves to another node (resolvedBlockNode), the edges at
// that node too. So the read of a block that moved lists the citations of its old address and the
// links into the block where it stands now, and the read of a block of a document converted into
// its issue's room, at the document's own address, lists the mentions written at that address and
// the links the block index wrote at the room's node, which is where it writes a pointer's (T17 of
// this branch's plan). Each list is in the read's order, newest first, and the two are merged by
// time, a tie taking the address's edge first.
func referenceEdges(ctx context.Context, q refs.Queryer, who model.Actor, query refs.Query, node model.GraphNode) ([]model.GraphEdge, error) {
	read := func(query refs.Query) ([]model.GraphEdge, error) {
		edges, err := refs.Edges(ctx, q, who, query)
		if err != nil || query.Direction != "in" {
			return edges, err
		}
		return edges, documentMentionExcerpts(ctx, q, who, query, edges)
	}
	edges, err := read(query)
	if err != nil || query.Kind != refs.BlockNodeKind || node.Kind != refs.BlockNodeKind || node.ID == query.ID {
		return edges, err
	}
	at := query
	at.ID = node.ID
	resolved, err := read(at)
	if err != nil || len(resolved) == 0 {
		return edges, err
	}
	merged := make([]model.GraphEdge, 0, len(edges)+len(resolved))
	for len(edges) > 0 || len(resolved) > 0 {
		if len(edges) == 0 || len(resolved) > 0 && resolved[0].CreatedAt.After(edges[0].CreatedAt) {
			merged, resolved = append(merged, resolved[0]), resolved[1:]
			continue
		}
		merged, edges = append(merged, edges[0]), edges[1:]
	}
	return merged, nil
}

// wholeReference is ref with its item id, or its block id, written whole where the request
// wrote it short: the one item of the owner the reference names that carries the prefix, among
// the items who may see. ok is false once it has written the answer itself, the 409 of a prefix
// two items or more carry.
//
// Which owner, and so which set the prefix names one of, is read from the reference by the one
// rule the index-time resolver reads it by (refs.PrefixScope): an ask's, a comment's or a
// message's within its issue, an ask's or a comment's written on a project document within that
// document, and a document block's among the blocks of the document the reference names, at the
// version it pins. A reference whose owner no scope holds is left as written: a block of an ask,
// a comment or a message, whose blocks are numbered (`b2`) and are no id a prefix shortens. A
// prefix nothing carries is left as written too, and the read answers the 404 it always did for
// a node it has no edge of.
func (s *server) wholeReference(w http.ResponseWriter, r *http.Request, ref text.Ref, who model.Actor, viewer refs.Viewer) (text.Ref, bool) {
	block := ref.Kind == "block" && ref.Item == ""
	kind, scope, written, ok := refs.PrefixScope(text.Prefix{Ref: ref, Block: block})
	if !ok || !text.IsIDPrefix(written) {
		return ref, true
	}
	// A prefix nothing carries is "" and no error, which this route leaves as written.
	whole, err := s.wholeID(r.Context(), kind, scope, written, who, viewer)
	if err != nil {
		s.writeIDPrefixError(w, r, err)
		return ref, false
	}
	if whole == "" {
		return ref, true
	}
	if block {
		ref.Block = whole
	} else {
		ref.ID = whole
	}
	return ref, true
}

// referenceQuestions is the question map GET /api/v1/references/questions answers: the document
// one reference source's text belongs to, and the ask each question block of that document
// declares, which is what a `#id` written in that text names.
type referenceQuestions struct {
	Document  string              `json:"document"`
	Questions []referenceQuestion `json:"questions"`
}

// referenceQuestion is one question block of the document: the block id a `#id` writes, the
// whole id of the ask settlement made for it, and that ask's `dispatch://` address, the one the
// `#id`'s own edge is stored under (refs.AskRef). The address is empty for an ask no address
// names - one on an agent conversation's document - which a reader can name by its id alone.
type referenceQuestion struct {
	BlockID   string `json:"block_id"`
	Ask       string `json:"ask"`
	Reference string `json:"reference"`
}

// questionSourceKinds are the reference sources whose text belongs to a document, the kinds
// refs.ScopeOf reads: a document version, an ask, a comment and an issue message.
var questionSourceKinds = []string{"artifact", "ask", "comment", "message"}

// getReferenceQuestions answers the question map a `#id` in one source's text resolves against:
// refs.ScopeOf for the document that text belongs to, then refs.QuestionMap for that document's
// question blocks. The page that renders a text reads it once for the whole text, as the
// indexing resolver does, rather than once per `#id`.
//
// ?id= takes a whole uuid or an id written short, resolved among every source of its kind, as
// the routes that take an id in their path do (resolveIDPrefixes): one match is read, a prefix
// two sources or more carry is 409 ID_AMBIGUOUS, and one nothing carries is the 404 of a source
// whose text belongs to no document, which is also the answer for a direct message and for any
// source that is on no document. The id arrives in the query string, which the prefix layer
// does not touch: that layer rewrites path values alone, so a prefix is resolved here. A whole
// uuid written in one of uuid.Parse's other spellings is canonicalized by the read itself
// (refs.ScopeOf, canonicalUUID), which is the one place it is canonicalized, so this route
// hands on what it was given and `urn:uuid:<uuid>` answers as the canonical id answers.
func (s *server) getReferenceQuestions(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	who := actor.Recorded()
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if !slices.Contains(questionSourceKinds, kind) {
		s.writeHandlerError(w, errorf(http.StatusBadRequest, "INVALID_KIND",
			"kind %q is not one of %s", kind, strings.Join(questionSourceKinds, ", ")))
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		s.writeHandlerError(w, errorf(http.StatusBadRequest, "INVALID_QUERY", "id is required"))
		return
	}
	if text.IsIDPrefix(id) {
		// A prefix nothing carries is left as written, and the read below answers this route's
		// own 404 for a source whose text belongs to no document.
		whole, err := s.wholeID(r.Context(), kind, refs.Scope{}, id, who, refs.Viewer{Human: human, ServerURL: s.deps.ServerURL})
		if err != nil {
			s.writeIDPrefixError(w, r, err)
			return
		}
		if whole != "" {
			id = whole
		}
	}
	document, err := refs.ScopeOf(r.Context(), s.deps.Store.Pool, who, kind, id)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if document == "" {
		s.writeHandlerError(w, errorf(http.StatusNotFound, "NOT_FOUND",
			"the text of %s %s belongs to no document", kind, id))
		return
	}
	questions, err := refs.QuestionMap(r.Context(), s.deps.Store.Pool, who, document)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	answer := referenceQuestions{Document: document, Questions: make([]referenceQuestion, 0, len(questions))}
	for blockID, ask := range questions {
		answer.Questions = append(answer.Questions, referenceQuestion{
			BlockID: blockID, Ask: ask.ID, Reference: refs.AskRef(ask),
		})
	}
	slices.SortFunc(answer.Questions, func(left, right referenceQuestion) int {
		return strings.Compare(left.BlockID, right.BlockID)
	})
	WriteJSON(w, http.StatusOK, answer)
}

// sinceHorizon is the horizon a since read caps at: H once an observation taken after the call is
// proven safe (Horizon.Await), so H is at or above every event committed before the read. Safe
// alone would not do: an observation is proven only by a later one, so on a process where
// nothing else observes (no event stream has started the relay, no Delivery page the hub) the
// first read after boot or a Reset would answer H = 0, and each later read stop at the previous
// read's observation, however old. Await takes longer than an observation or two only while an
// event write of this database holds the horizon back; past referencesSinceWait, with the request
// still live, the read takes the horizon as an observation started after that finds it
// (Horizon.Safe), and a mention above it waits for a later read. A
// request that ends first answers Await's error.
func (s *server) sinceHorizon(ctx context.Context) (int64, error) {
	wait, cancel := context.WithTimeout(ctx, referencesSinceWait)
	safe, err := s.horizon().Await(wait)
	cancel()
	if err == nil || ctx.Err() != nil {
		return safe, err
	}
	return s.horizon().Safe(ctx)
}

// blockReferenceNode is the block node ref names (query addresses it), with its whole current
// text. Unpinned, it is the block the address resolves to (resolvedBlockNode: its moved_to tuples
// first, then its document's room, then itself), and answers 404 where it resolves to none.
// Pinned to a version N that held the block, the node is the one the address names
// (refs.NamedBlock: the block it resolves to, or where none stands now the node the same two steps
// name, the room's node for a block of a document converted into its issue's room), and it carries
// what N recorded (model.BlockPin), "unchanged" or "changed" by comparing the current text with
// N's, or "removed" with the version that deleted it, as a node with no text or changed version
// whose facts are a removed block's (refs.RemovedBlockNode); a pin to a current block carries the
// current block's facts. A pin to a version the document lacks, to one whose blocks are not on
// record (an older build wrote it and none indexed it) or to one that did not hold the block
// answers 404, each saying which. A block who may not see answers, pinned or not, what an address
// nobody holds answers (refs/visibility.go), its predicate read at the node the address names.
func (s *server) blockReferenceNode(ctx context.Context, who model.Actor, query refs.Query, ref text.Ref) (model.GraphNode, error) {
	pool := s.deps.Store.Pool
	documentKey, _, _ := strings.Cut(query.ID, "#")
	document, found, err := documentAtAddress(ctx, pool, who, documentKey)
	if err != nil {
		return model.GraphNode{}, err
	}
	if !found {
		return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "no document at %s", documentKey)
	}
	if ref.Pin == 0 {
		return s.resolvedBlockNode(ctx, who, query.ID, ref, document)
	}
	named, err := refs.NamedBlock(ctx, pool, query.ID)
	if err != nil {
		return model.GraphNode{}, err
	}
	node, current, err := refs.Node(ctx, pool, who, "block", named, true)
	if err != nil {
		return model.GraphNode{}, err
	}
	return pinnedBlockNode(ctx, pool, who, document, node, current, named, ref)
}

// pinnedBlockNode is block nodeID of document read at the version ref pins (ref.Pin is not 0), as
// a reference pinned to it reads it (blockReferenceNode, which passes the node its address names,
// refs.NamedBlock), where node and current are what refs.Node answered for nodeID: the block with
// what that version recorded of it (model.BlockPin). A block no current version holds is the node
// RemovedBlockNode builds, and a pin to a version the document lacks, to one whose blocks are not
// on record or to one that did not hold the block answers 404, each saying which. The lens read
// reads a pinned address the same way (lensReadNode).
func pinnedBlockNode(ctx context.Context, q refs.Queryer, who model.Actor, document, node model.GraphNode, current bool, nodeID string, ref text.Ref) (model.GraphNode, error) {
	at, err := refs.BlockTextAt(ctx, q, document.ID, ref.Block, ref.Pin)
	if err != nil {
		return model.GraphNode{}, err
	}
	// refs.Node answers a block reserved for another author exactly as it answers one no current
	// version holds, so such a block reaches the branch below as removed, and BlockTextAt reads a
	// version's record of it for nobody in particular. Whether who may see it is the block's own
	// predicate's to say (refs.VisibleKind, the one every read of a block binds): a block who may
	// not see answers what an address nobody holds answers, the 404 of a version that did not
	// hold it, and its text, its removal and its existence stay unsaid.
	hidden := false
	if !current && at.Text != nil {
		if err := q.QueryRow(ctx, `select not `+refs.VisibleKind("block", "$1", "$2"),
			nodeID, refs.VisibilityArg(who)).Scan(&hidden); err != nil {
			return model.GraphNode{}, fmt.Errorf("read whether the reader may see block %s: %w", nodeID, err)
		}
	}
	switch {
	case !at.Exists:
		return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "no version %d of %s", ref.Pin, document.Ref)
	case !at.Indexed && at.Earliest == 0:
		return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "version %d of %s has no record of its blocks; no version of it has one yet", ref.Pin, document.Ref)
	case !at.Indexed:
		return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "version %d of %s has no record of its blocks; the earliest is version %d", ref.Pin, document.Ref, at.Earliest)
	case at.Text == nil, hidden:
		return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "no block %q in %s at version %d", ref.Block, document.Ref, ref.Pin)
	}
	blockRef := refs.BlockRef(document.Ref, ref.Block)
	pin := &model.BlockPin{Version: ref.Pin, Ref: blockRef + "@v" + strconv.Itoa(ref.Pin), Text: *at.Text}
	if !current {
		pin.State, pin.RemovedVersion = "removed", at.RemovedVersion
		removed, err := refs.RemovedBlockNode(ctx, q, document, nodeID, ref.Block, at.RemovedVersion)
		if err != nil {
			return model.GraphNode{}, err
		}
		removed.Pin = pin
		return removed, nil
	}
	if node.Text, err = refs.CurrentBlockText(ctx, q, document.ID, ref.Block); err != nil {
		return model.GraphNode{}, err
	}
	pin.State = "unchanged"
	if node.Text != pin.Text {
		pin.State = "changed"
	}
	node.Pin = pin
	return node, nil
}

// resolvedBlockNode is the block an address resolves to (graphstore.MovedTo: the far end of its
// moved_to tuples first, then the same block id in its document's room, then the address itself),
// named by its own address and carrying its whole current text: so a citation written before the
// block moved reads the block where it stands now, and a citation of a block of a document
// converted into its issue's room, at the document's own address, reads the room's node. A chain
// that ends at an ask - the heading of a decisions document's section that cites an answered ask a
// person removed from its spec - answers that ask's own node, as a reference of the ask's address
// reads it, which holds its question and the person's answer. Where the address resolves to no
// block and no ask, and where who may not see the one it resolves to, it answers the 404 of a
// block no current version holds, naming the address asked, so a move says nothing of a node the
// reader may not see. The edges the read lists beside it are those at the address asked, the
// citations of that address, and those at the node it reaches (referenceEdges).
func (s *server) resolvedBlockNode(ctx context.Context, who model.Actor, address string, ref text.Ref, document model.GraphNode) (model.GraphNode, error) {
	pool := s.deps.Store.Pool
	noBlock := errorf(http.StatusNotFound, "NOT_FOUND", "no block %q in %s", ref.Block, document.Ref)
	reached, moved, err := graphstore.MovedTo(ctx, pool, address)
	if err != nil {
		return model.GraphNode{}, err
	}
	if !moved {
		return model.GraphNode{}, noBlock
	}
	if reached.Ask != "" {
		node, found, err := refs.Node(ctx, pool, who, graphstore.KindAsk, reached.Ask, true)
		if err != nil {
			return model.GraphNode{}, err
		}
		if !found {
			return model.GraphNode{}, noBlock
		}
		return node, nil
	}
	node, found, err := refs.Node(ctx, pool, who, refs.BlockNodeKind, reached.Address, true)
	if err != nil {
		return model.GraphNode{}, err
	}
	if !found || node.Parent == nil {
		return model.GraphNode{}, noBlock
	}
	node.Text, err = refs.CurrentBlockText(ctx, pool, node.Parent.ID, node.BlockID)
	return node, err
}

// movedDocumentNode is the lens the address of the document stored at refKey moved to: the lens
// the pre-mortem conversion wrote in the place of the document it retired (api.ConvertPremortem),
// which the moved_to from the document's address reaches (graphstore.MovedTo). It is answered as a
// moved block's old address answers the block it reached (resolvedBlockNode): the node the move
// reaches, named by its own address, and the edges the read lists beside it are those at the
// address asked, the citations of the document. found is false where the address moved to no lens
// that stands, and where who may not see the lens, and the read then answers the address as it
// would with no move - the document, or a retired document's issue (refs.loadArtifactNodes) - so
// a move says nothing of a lens the reader may not see.
func (s *server) movedDocumentNode(ctx context.Context, who model.Actor, refKey string) (model.GraphNode, bool, error) {
	pool := s.deps.Store.Pool
	reached, moved, err := graphstore.MovedTo(ctx, pool, refKey)
	if err != nil || !moved {
		return model.GraphNode{}, false, err
	}
	return refs.Node(ctx, pool, who, graphstore.KindLens, reached.Lens, true)
}

// documentAtAddress is the document stored at refKey, as a read of its blocks needs it: the node
// refs.Node answers for the address, or, where the address is a retired document's, which refs
// answers as the document's issue (refs.loadArtifactNodes), the document itself, read by its uuid,
// since a retired document's blocks and versions stay readable.
func documentAtAddress(ctx context.Context, q refs.Queryer, who model.Actor, refKey string) (model.GraphNode, bool, error) {
	document, found, err := refs.Node(ctx, q, who, graphstore.KindArtifact, refKey, true)
	if err != nil || !found || document.Kind == graphstore.KindArtifact {
		return document, found, err
	}
	var id string
	if err := q.QueryRow(ctx, `select id::text from artifacts where ref_key = $1`, refKey).Scan(&id); err != nil {
		return model.GraphNode{}, false, fmt.Errorf("read the retired document at %s: %w", refKey, err)
	}
	return refs.Node(ctx, q, who, graphstore.KindArtifact, id, false)
}

// itemBlockReferenceNode is the block node ref, a part of an ask or a block of a comment or a
// message, names (query addresses it), with its whole current text (refs.CurrentItemBlockText). A
// block no current revision of its item holds answers 404, as a document's does, unless ref is
// pinned to a revision N of its ask or comment that held it: the node then carries what N
// recorded (model.BlockPin, its Version the revision), "unchanged" or "changed" by comparing the
// current text with N's, or "removed" with the revision that removed it (RemovedVersion), as a
// node with no text or changed revision whose facts are a removed block's
// (refs.RemovedItemBlockNode). A pin past the item's current revision, or to one that did not
// hold the block, answers 404, each saying which. A message records no revisions, not even for an
// edit of its body, so there is no revision to pin a message's block to and no reference pins one
// (parseReferencesQuery refuses one).
//
// A block of a post its author deleted is gone with the body: the delete indexes the empty body,
// which takes every block out of the post (planEditedItem, in clearDeletedPostBody), and clears
// the text each revision recorded (refs.TombstoneItem). So each 404 a read of it can reach - no
// current block, no recorded text at the pin, and a pin past the current revision - names the
// deletion as well (itemBlockNotFound): every read of a deleted post says that it is deleted and
// gives no part of what it said.
func (s *server) itemBlockReferenceNode(ctx context.Context, who model.Actor, query refs.Query, ref text.Ref) (model.GraphNode, error) {
	pool := s.deps.Store.Pool
	kind := refs.ItemKind(ref.Item)
	noun := itemBlockNoun(ref.Item)
	item, found, err := refs.Node(ctx, pool, who, ref.Item, ref.ID, true)
	if err != nil {
		return model.GraphNode{}, err
	}
	if !found {
		return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "no %s %s", ref.Item, ref.ID)
	}
	node, current, err := refs.Node(ctx, pool, who, query.Kind, query.ID, true)
	if err != nil {
		return model.GraphNode{}, err
	}
	if current {
		if node.Text, err = refs.CurrentItemBlockText(ctx, pool, kind, query.ID); err != nil {
			return model.GraphNode{}, err
		}
	}
	if ref.Pin == 0 {
		if !current {
			return model.GraphNode{}, itemBlockNotFound(ctx, pool, kind, item.ID,
				fmt.Sprintf("no %s %q in %s %s", noun, ref.Block, ref.Item, item.ID))
		}
		return node, nil
	}
	return pinnedItemBlockNode(ctx, pool, who, item, node, current, query.ID, ref)
}

// pinnedItemBlockNode is block nodeID of item, an ask or a comment, read at the revision ref pins
// (ref.Pin is not 0), as a reference pinned to it reads it (itemBlockReferenceNode), where node and
// current are what refs.Node answered for nodeID, its text the current one: the block with what
// that revision recorded of it (model.BlockPin). A block no current revision holds is the node
// refs.RemovedItemBlockNode builds, and a pin past the item's current revision or to one that did
// not hold the block answers 404, each saying which. The lens read reads a pinned address of an
// item's block the same way (lensReadNode).
func pinnedItemBlockNode(ctx context.Context, q refs.Queryer, who model.Actor, item, node model.GraphNode, current bool, nodeID string, ref text.Ref) (model.GraphNode, error) {
	kind := refs.ItemKind(ref.Item)
	noun := itemBlockNoun(ref.Item)
	at, err := refs.ItemBlockAt(ctx, q, kind, item.ID, ref.Block, ref.Pin)
	if err != nil {
		return model.GraphNode{}, err
	}
	switch {
	case ref.Pin > at.Current:
		return model.GraphNode{}, itemBlockNotFound(ctx, q, kind, item.ID,
			fmt.Sprintf("no revision %d of %s %s; its current revision is %d", ref.Pin, ref.Item, item.ID, at.Current))
	case at.Text == nil:
		return model.GraphNode{}, itemBlockNotFound(ctx, q, kind, item.ID,
			fmt.Sprintf("no %s %q in %s %s at revision %d", noun, ref.Block, ref.Item, item.ID, ref.Pin))
	}
	pin := &model.BlockPin{Version: ref.Pin, Ref: refs.BlockRef(item.Ref, ref.Block) + "@v" + strconv.Itoa(ref.Pin), Text: *at.Text}
	if !current {
		removed, held, err := refs.RemovedItemBlockNode(ctx, q, who, kind, nodeID, at.RemovedRevision)
		if err != nil {
			return model.GraphNode{}, err
		}
		if !held {
			return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "no %s %q in %s %s", noun, ref.Block, ref.Item, item.ID)
		}
		pin.State, pin.RemovedVersion = "removed", at.RemovedRevision
		removed.Pin = pin
		return removed, nil
	}
	pin.State = "unchanged"
	if node.Text != pin.Text {
		pin.State = "changed"
	}
	node.Pin = pin
	return node, nil
}

// itemBlockNoun is what one block of an item of kind is called where a refusal names it: a part
// of an ask, a block of a comment or a message.
func itemBlockNoun(kind string) string {
	if kind == string(refs.ItemAsk) {
		return "part"
	}
	return "block"
}

// itemBlockNotFound is the 404 a block reference answers when the item holds no such block:
// `missing`, the words the read's own case wrote, and after them the deletion when the item is
// a post its author withdrew. The delete clears the body, takes every block out of the post by
// indexing the empty body (planEditedItem, in clearDeletedPostBody) and clears the text each
// revision recorded (refs.TombstoneItem), so a read of a deleted post's block would otherwise
// say that the block is missing and leave the deletion unsaid, where every read of the post
// says that it is deleted. The status stays 404: the reference names a node no read can give,
// which is what NOT_FOUND says here, and the words match deletedPostError's
// (handler_errors.go), written again here under that 404. The mark is read on this path alone, so
// a reference to a block that is there costs no query for it, and through q, the queryer the read
// that missed the block reads with, so the lens read's reads of a pinned block
// (pinnedItemBlockNode, from lensReadNode) take it on their own transaction.
func itemBlockNotFound(ctx context.Context, q refs.Queryer, kind refs.ItemKind, itemID, missing string) error {
	deleted, err := postIsDeleted(ctx, q, kind, itemID)
	if err != nil {
		return err
	}
	if deleted {
		return errorf(http.StatusNotFound, "NOT_FOUND", "%s: this %s is deleted", missing, kind)
	}
	return errorf(http.StatusNotFound, "NOT_FOUND", "%s", missing)
}

// postIsDeleted reports whether comment or message itemID carries a delete mark (deleted_at).
// A comment and a message are the only posts a delete marks - the columns are theirs alone
// (20261110224508_post_corrections.up.sql) - so an ask, the third item kind, answers false and
// reads nothing. The table of a kind is refs', which owns the map of an item kind to its
// storage (refs.ItemKind.ItemTable), as postHistoryOwner reads it. A row deleted outright
// between the caller's read and this one answers false, and the caller's own refusal then
// stands as it wrote it.
func postIsDeleted(ctx context.Context, q refs.Queryer, kind refs.ItemKind, itemID string) (bool, error) {
	if kind != refs.ItemComment && kind != refs.ItemMessage {
		return false, nil
	}
	items, _, err := kind.ItemTable()
	if err != nil {
		return false, err
	}
	var deleted bool
	if err := q.QueryRow(ctx, `
		select deleted_at is not null from `+items+` where id = $1
	`, itemID).Scan(&deleted); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("read the delete mark of %s %s: %w", kind, itemID, err)
	}
	return deleted, nil
}

// blockPin is a trailing `@v<N>` pin, as a block reference writes one.
var blockPin = regexp.MustCompile(`@v[0-9]+$`)

// pinsAMessageBlock reports whether raw, a reference text.ParseRef refused, is a block of a
// message pinned to a revision: the grammar reads none, since a message records no revisions.
func pinsAMessageBlock(raw, serverURL string) bool {
	unpinned := blockPin.ReplaceAllString(raw, "")
	if unpinned == raw {
		return false
	}
	ref, ok := text.ParseRef(unpinned, serverURL)
	return ok && ref.Kind == "block" && ref.Item == "message"
}

// parseRequestRef reads one reference a request names - a ?to= or ?from=, or one ?ref= of the
// batched read - by text.ParseRef, so a node is read whatever the length of its name: the target
// bound decides only what text indexes, and a node no index holds an edge to simply has fewer
// edges. Anything else is 400 INVALID_REFERENCE naming the value, with its own sentence for a
// pinned block of a message, which the grammar reads as no reference at all. So is a party - an
// agent's session, a role or a person - which the grammar reads but the graph holds no node for
// (refs.TargetOf): each caller reads the graph, which has no edge of one in either direction and
// no name for one, so every reference this answers has a target.
func parseRequestRef(raw, serverURL string) (text.Ref, error) {
	ref, ok := text.ParseRef(raw, serverURL)
	if !ok {
		if pinsAMessageBlock(raw, serverURL) {
			return text.Ref{}, errorf(http.StatusBadRequest, "INVALID_REFERENCE", "%q pins a block of a message, which records no revisions; name the block without @v<N>", raw)
		}
		return text.Ref{}, errorf(http.StatusBadRequest, "INVALID_REFERENCE", "%q is not a dispatch:// reference to an issue, document, block, ask, comment, message, component, interaction, lens, lens finding, risk, constraint, or lens kind", raw)
	}
	if _, stored := refs.TargetOf(ref); !stored {
		return text.Ref{}, errorf(http.StatusBadRequest, "INVALID_REFERENCE", "%q names a %s, which the reference graph holds no node for", raw, ref.Kind)
	}
	return ref, nil
}

// parseReferencesQuery turns ?to=|?from=, ?kind=, and ?since= into an edge query addressed the
// way graph_edges stores the node (refs.TargetOf): text.TargetID for the to side (ref_key for
// artifacts; an ask's part and a comment's or message's block under its storage kind), the
// item id for the from side (the caller swaps an artifact's ref_key for its uuid). The reference
// itself is read by parseRequestRef, as the batched read reads each of its own. It returns apart
// the ?kind= values that are no edge kind of this build.
func parseReferencesQuery(r *http.Request, serverURL string) (refs.Query, text.Ref, []string, error) {
	values := r.URL.Query()
	to, from := strings.TrimSpace(values.Get("to")), strings.TrimSpace(values.Get("from"))
	if (to == "") == (from == "") {
		return refs.Query{}, text.Ref{}, nil, errorf(http.StatusBadRequest, "INVALID_REFERENCE", "exactly one of to or from is required")
	}
	raw, direction := to, "in"
	if from != "" {
		raw, direction = from, "out"
	}
	ref, err := parseRequestRef(raw, serverURL)
	if err != nil {
		return refs.Query{}, text.Ref{}, nil, err
	}
	// The node as graph_edges stores it: an ask's part and a comment's or message's block under
	// its storage kind. Every reference parseRequestRef answers has one: it refuses a party.
	target, _ := refs.TargetOf(ref)
	query := refs.Query{ServerURL: serverURL, Direction: direction, Kind: target.Kind, ID: target.ID}
	var unknownKinds []string
	if kinds := strings.TrimSpace(values.Get("kind")); kinds != "" {
		linkKinds := pmdoc.LinkKinds()
		for _, kind := range strings.Split(kinds, ",") {
			kind = strings.TrimSpace(kind)
			if !slices.Contains(model.GraphEdgeKinds, kind) && !slices.Contains(linkKinds, kind) {
				unknownKinds = append(unknownKinds, kind)
				continue
			}
			query.Kinds = append(query.Kinds, kind)
			// An issue's wait on an issue is blocked_by in graph_edges through this release, the
			// kind a process of the release before reads it by, and needs in this read's answer
			// (answerIssueWaitsAsNeeds), so needs reads that edge as well as a plan node's needs,
			// and blocked_by reads it too (R10 of the issue-tree spec).
			if kind == model.EdgeKindNeeds {
				query.Kinds = append(query.Kinds, blockedByMember)
			}
		}
		slices.Sort(query.Kinds)
		query.Kinds = slices.Compact(query.Kinds)
	}
	// An ask's inbound `replies_to` edges are its own clarification thread, which every ask
	// surface renders inline, and its parts are each part_of it: "what references this ask" means
	// neither (refs.AskBacklinkExclusions, the exclusion the count on its card reads too). A
	// document's, a comment's or a message's blocks are each part_of it the same way; its links
	// are what its read means, so those edges are listed only when asked for by kind. A caller
	// that asks for one of those kinds explicitly still gets it — this is the default, not a
	// hidden rule.
	if len(query.Kinds) == 0 {
		switch {
		case direction == "in" && ref.Kind == "ask":
			query.ExcludeKinds = refs.AskBacklinkExclusions
		case ref.Kind == "artifact" || ref.Kind == "ask" || ref.Kind == "comment" || ref.Kind == "message":
			query.ExcludeKinds = []string{"part_of"}
		}
	}
	if since := strings.TrimSpace(values.Get("since")); since != "" {
		value, err := strconv.ParseInt(since, 10, 64)
		if err != nil || value < 0 {
			return refs.Query{}, text.Ref{}, nil, errorf(http.StatusBadRequest, "INVALID_SINCE", "since must be an event id")
		}
		query.Since = &value
	}
	return query, ref, unknownKinds, nil
}

// answerIssueWaitsAsNeeds names each edge of an issue's wait on an issue needs, the one relation an
// issue and a plan node wait by (R10 of the issue-tree spec): graph_edges names it blocked_by in
// both its arms through this release, which a process of the release before reads, and this
// build's references read answers it as the store states it.
func answerIssueWaitsAsNeeds(edges []model.GraphEdge) {
	for index := range edges {
		if edges[index].Kind == blockedByMember {
			edges[index].Kind = model.EdgeKindNeeds
		}
	}
}

// inReferencesListRead is getReferences's list: query's edges beside node, the node the read
// answers, with, on an inbound read, the excerpts stored on the document mentions among them
// (referenceEdges), all read in a read transaction of its own whose first statement sets jit off
// (inJITOffRead).
//
// JIT is off because compiling the read's statement over graph_edges (readEdgeRows) costs more
// than running it: with JIT on, which compiled 451 functions for each call, it took 136.5 ms a
// call, and with JIT off 21.6 to 27.8 ms (the root's c-read-cost). Production's
// database is Cloud SQL, which runs no JIT, so the setting makes the read on a deployment with its
// own Postgres what it is in production. No statement of this read carries a tag, so the setting
// carries none either. The read has no budget: referencesClosureBound is the closure's alone.
// TestTheReferencesListReadRunsWithJITOff holds the route's read to it on a session that starts at
// jit = on.
func (s *server) inReferencesListRead(ctx context.Context, who model.Actor, query refs.Query, node model.GraphNode) ([]model.GraphEdge, error) {
	edges := []model.GraphEdge{}
	err := s.inJITOffRead(ctx, "", func(tx pgx.Tx) error {
		var err error
		edges, err = referenceEdges(ctx, tx, who, query, node)
		return err
	})
	return edges, err
}

// documentMentionExcerpts replaces the excerpt of every mention written by a document with the
// excerpt stored when the mention was indexed (refs.RecordExcerpts), read through q: the block
// holding the mention, its id a #b-<id> jump target, and its markdown. The route reads no
// document: a mention whose excerpt the reconciliation has not stored yet, is owed a fresh one, or
// lies in no block keeps the document's name. The reconciliation serves the next stored block
// only after it records a non-null excerpt_version. During the deploy that ships migration 0092, a
// stale excerpt whose version is still set is served until the reconciliation marks it owed
// (refs.StoredExcerpts). The excerpts are read for who, so one whose document or block who may
// not see is none.
func documentMentionExcerpts(ctx context.Context, q refs.Queryer, who model.Actor, query refs.Query, edges []model.GraphEdge) error {
	stored, err := refs.StoredExcerpts(ctx, q, who, query.Kind, query.ID)
	if err != nil {
		return err
	}
	for index := range edges {
		edge := &edges[index]
		if edge.Kind != "mentions" || edge.Node.Kind != "artifact" {
			continue
		}
		if excerpt, found := stored[edge.Node.ID]; found {
			edge.Excerpt = &excerpt
		}
	}
	return nil
}

// getIssueReferences answers an issue's references read, one hop in each direction (refs.Closure):
// links, every node the issue's own content links to, and linked_from, every node that links to
// the issue or to its own content, each written [] when it has no row and never null. members are
// the documents of links, each read whole by its ref_key, at depth 1 and with its row's via, and
// truncated is false, since a read of one hop is never cut: both stay for the clients of the build
// before, which read them alone. Each member carries its kind and address: artifact and the
// document's own, or lens and the lens's where a link names the address of a document the
// pre-mortem conversion moved to a lens, its artifact then the retired document. The read runs
// under referencesClosureBound (inClosureRead); loadIssue before it and loadArtifactsByRefKey
// after it read by key on the pool, outside the bound. ?versions=latest leaves each member's
// artifact.versions holding only its latest version, under its own ETag; absent, empty or all is
// the full answer. A repeated versions is refused rather than read by its first value.
func (s *server) getIssueReferences(w http.ResponseWriter, r *http.Request) {
	who, ok := s.actingReader(w, r)
	if !ok {
		return
	}
	versionsValues := r.URL.Query()["versions"]
	if len(versionsValues) > 1 {
		s.writeHandlerError(w, errorf(http.StatusBadRequest, "INVALID_QUERY", "versions must be given once, as all or latest"))
		return
	}
	var latestOnly bool
	if len(versionsValues) == 1 {
		switch versionsValues[0] {
		case "", "all":
		case "latest":
			latestOnly = true
		default:
			s.writeHandlerError(w, errorf(http.StatusBadRequest, "INVALID_QUERY", "versions must be all or latest"))
			return
		}
	}
	issue, err := s.loadIssue(r.Context(), s.deps.Store.Pool, r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	closure, err := s.inClosureRead(r.Context(), who, issue.Key)
	if err != nil {
		if closureBoundHit(err) {
			s.writeHandlerError(w, errorf(http.StatusServiceUnavailable, "REFERENCES_TIMEOUT",
				"the issue's reference closure ran past its %s bound, so no reference was read; try again later", referencesClosureBound))
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	refKeys := make([]string, len(closure.Members))
	for index, member := range closure.Members {
		refKeys[index] = member.RefKey
	}
	artifacts, err := s.loadArtifactsByRefKey(r.Context(), s.deps.Store.Pool, refKeys, latestOnly)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	members := make([]model.ReferenceMember, 0, len(closure.Members))
	for _, closureMember := range closure.Members {
		artifact, ok := artifacts[closureMember.RefKey]
		if !ok {
			s.writeHandlerError(w, fmt.Errorf("reference target %q disappeared while loading closure", closureMember.RefKey))
			return
		}
		members = append(members, model.ReferenceMember{
			Artifact: *artifact,
			Kind:     closureMember.Kind,
			Address:  closureMember.Address,
			Depth:    closureMember.Depth,
			Via:      closureMember.Via,
		})
	}
	// Each row's name reaches a caller that sent Dispatch-Trees: 1 with its tree, read where it
	// stands (newIssueReferencesRead). The answer is encoded once, exactly as WriteJSON writes it,
	// trailing newline and all, so the token hashes the bytes it validates and those are the bytes
	// written; the token names the reading and the parser that read the trees too
	// (referencesETag), so a validator given for one reading never answers the other.
	trees := wantsDisplayTrees(r)
	var answer bytes.Buffer
	if err := json.NewEncoder(&answer).Encode(newIssueReferencesRead(model.IssueReferences{
		Members:    members,
		Truncated:  false,
		Links:      closure.Links,
		LinkedFrom: closure.LinkedFrom,
	}, trees)); err != nil {
		s.writeHandlerError(w, fmt.Errorf("encode the issue's references: %w", err))
		return
	}
	if answerNotModified(w, r, s.referencesETag(answer.Bytes(), latestOnly, trees)) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(answer.Bytes())
}

func (s *server) getArtifactReferences(w http.ResponseWriter, r *http.Request) {
	who, ok := s.actingReader(w, r)
	if !ok {
		return
	}
	artifact, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	outgoing, err := refs.Outgoing(r.Context(), s.deps.Store.Pool, who, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	referencedBy, err := refs.ReferencedBy(r.Context(), s.deps.Store.Pool, who, artifact.RefKey, s.deps.ServerURL)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, model.ArtifactReferences{Outgoing: outgoing, ReferencedBy: referencedBy})
}

// referencesETag is an issue references answer's validator: a hash of the answer's own bytes.
// The trimmed answer (latestOnly) hashes one more line naming its variant ahead of them, so the
// full and trimmed answers of one issue never share an ETag, even where every member has one
// version and the two answers are the same bytes; the full answer hashes no such line, and no
// answer's bytes begin with it, as each begins with `{`. The hash is closed as the lists' are
// (listETag), with whether the caller asked for the rows' name trees, the display parser version
// and the build commit, for the reasons listETag gives: an issue with no row answers the same
// bytes to either reading.
//
// It hashes the answer rather than what the answer is made of because a member moves without
// anything this issue holds moving with it. A member is a document of another owner (no member
// is one of this issue's own documents, refs.Closure), and that owner's writes append their
// events there: a rename of the document (artifact.renamed, renameArtifact) changes the
// member's artifact.name, the delete of a comment whose accepted suggestion named a version
// after the comment's body renames that version (renameAcceptedVersion), and a document's
// conversion into a lens moves the member's address with no new version of it, while this
// issue's event sequence, the closure and each member's latest version stand still. A row of
// links or linked_from carries its node's name, last change and parent, and another node's new
// link to the issue adds a row, none of which moves the issue's own sequence either. A token
// over those would answer a reader holding it 304 over the old words; a token over the answer
// moves with every byte a reader is shown, and the build before, which writes neither list,
// writes other bytes for the same issue, so the two builds never share an ETag.
func (s *server) referencesETag(answer []byte, latestOnly, trees bool) string {
	hash := sha256.New()
	if latestOnly {
		_, _ = io.WriteString(hash, "versions=latest\n")
	}
	_, _ = hash.Write(answer)
	return s.listETag(hash, trees)
}

// loadArtifactsByRefKey reads the documents refKeys name, each with every version in number
// order, or with latestOnly only its latest version. A document with no version is absent.
func (s *server) loadArtifactsByRefKey(ctx context.Context, q queryer, refKeys []string, latestOnly bool) (map[string]*model.Artifact, error) {
	artifacts := make(map[string]*model.Artifact, len(refKeys))
	if len(refKeys) == 0 {
		return artifacts, nil
	}
	versions := `join artifact_versions v on v.artifact_id = artifacts.id`
	if latestOnly {
		versions = `join lateral (
			select * from artifact_versions v where v.artifact_id = artifacts.id order by v.number desc limit 1
		) v on true`
	}
	// The row's role columns, retired mark and room are artifactColumns', read as every reader of
	// an artifact row reads them, so a member answers its room and fragment (placeArtifact).
	rows, err := q.Query(ctx, `
		select artifacts.id::text, artifacts.issue_key, coalesce(artifacts.project_key, ''), artifacts.session_id,
		       artifacts.ref_key, artifacts.slug, artifacts.name, artifacts.kind, artifacts.is_primary,
		       artifacts.role, `+graphstore.ArtifactQueryRoleSQL+`, artifacts.created_by, artifacts.created_at,
		       artifacts.retired_at is not null, (`+refs.DocumentRoomSQL("artifacts.id")+`)::text,
		       `+versionColumns+`
		from artifacts
		`+versions+`
		where artifacts.ref_key = any($1)
		order by artifacts.ref_key, v.number
	`, refKeys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// authorsByJSON decodes each distinct author list once, and every version storing those bytes
	// shares the slice. That is safe because what this loader returns is read-only, encoded once
	// by getIssueReferences, and the memo lives for this one call only.
	authorsByJSON := map[string][]model.Actor{}
	for rows.Next() {
		var artifact model.Artifact
		var role, queryRole *string
		var createdBy, authors []byte
		var version model.Version
		if err := rows.Scan(
			&artifact.ID,
			&artifact.IssueKey,
			&artifact.Project,
			&artifact.SessionID,
			&artifact.RefKey,
			&artifact.Slug,
			&artifact.Name,
			&artifact.Kind,
			&artifact.Primary,
			&role,
			&queryRole,
			&createdBy,
			&artifact.CreatedAt,
			&artifact.Retired,
			&artifact.Room,
			&version.Number,
			&version.Named,
			&version.Summary,
			&authors,
			&version.CreatedAt,
			&version.Size,
			&version.MIME,
			&version.SHA256,
		); err != nil {
			return nil, err
		}
		current, found := artifacts[artifact.RefKey]
		if !found {
			if err := json.Unmarshal(createdBy, &artifact.CreatedBy); err != nil {
				return nil, fmt.Errorf("decode artifact author: %w", err)
			}
			placeArtifact(&artifact, role, queryRole)
			current = &artifact
			artifacts[artifact.RefKey] = current
		}
		decoded, ok := authorsByJSON[string(authors)]
		if !ok {
			if err := json.Unmarshal(authors, &decoded); err != nil {
				return nil, fmt.Errorf("decode artifact version authors: %w", err)
			}
			decoded = slices.Clip(decoded)
			authorsByJSON[string(authors)] = decoded
		}
		version.Authors = decoded
		current.Versions = append(current.Versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, artifact := range artifacts {
		model.StateFileTokens(artifact.Kind, artifact.Versions)
	}
	return artifacts, nil
}
