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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
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
	return ledger.QueueAskParts(ctx, ask, s.deps.Origins, document, at, questionReconciled, beside)
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
// block node carrying its whole current text; one pinned to a version of its document, or a
// revision of its ask's parts or of its comment, also reads the block as that version or revision
// recorded it (blockReferenceNode, itemBlockReferenceNode). A ?kind= this build does not emit - a
// newer build's, during a release - has no edges here, and the answer lists it in unknown_kinds
// rather than refusing the read.
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
	_, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	query, ref, unknownKinds, err := parseReferencesQuery(r, s.deps.Origins)
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
	if ref.Kind == "block" {
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
		node, found, err = refs.Node(r.Context(), s.deps.Store.Pool, who, query.Kind, query.ID, query.Direction == "in")
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if !found {
			s.writeHandlerError(w, errorf(http.StatusNotFound, "NOT_FOUND", "no %s at %s", query.Kind, query.ID))
			return
		}
	}
	// When this build knows none of the kinds the caller named, no edge is of them; refs.Edges
	// with no Kinds would read every kind.
	edges := []model.GraphEdge{}
	if len(query.Kinds) > 0 || len(unknownKinds) == 0 {
		if query.Since != nil {
			safe, err := s.sinceHorizon(r.Context())
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			query.Through = &safe
		}
		edges, err = refs.Edges(r.Context(), s.deps.Store.Pool, who, query)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if query.Direction == "in" {
		if err := s.documentMentionExcerpts(r.Context(), who, query, edges); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// The node's own text and the text its pin recorded go through the request's reader; each
	// edge's excerpt is a cut, read where it stands (graphEdgeReads).
	trees := wantsDisplayTrees(r)
	reader := s.readDisplayTrees(r.Context(), trees)
	answer := graphReferencesRead{Node: graphNodeRead{GraphNode: node}, Edges: graphEdgeReads(edges, trees), UnknownKinds: unknownKinds}
	answer.Node.shape(reader)
	reader.resolve()
	WriteJSON(w, http.StatusOK, answer)
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
	_, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
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
// text. A block no current version of its document holds answers 404, unless ref is pinned to a
// version N that held it: the node then carries what N recorded (model.BlockPin), "unchanged"
// or "changed" by comparing the current text with N's, or "removed" with the version that deleted
// it, as a node with no text or changed version whose facts are a removed block's
// (refs.RemovedBlockNode); a pin to a current block carries the current block's facts. A pin to
// a version the document lacks, to one whose blocks are not on record (an older build wrote it and
// none indexed it) or to one that did not hold the block answers 404, each saying which. A block
// who may not see answers, pinned or not, what an address nobody holds answers (refs/visibility.go).
func (s *server) blockReferenceNode(ctx context.Context, who model.Actor, query refs.Query, ref text.Ref) (model.GraphNode, error) {
	pool := s.deps.Store.Pool
	documentKey, _, _ := strings.Cut(query.ID, "#")
	document, found, err := refs.Node(ctx, pool, who, "artifact", documentKey, true)
	if err != nil {
		return model.GraphNode{}, err
	}
	if !found {
		return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "no document at %s", documentKey)
	}
	node, current, err := refs.Node(ctx, pool, who, "block", query.ID, true)
	if err != nil {
		return model.GraphNode{}, err
	}
	if ref.Pin == 0 {
		if !current {
			return model.GraphNode{}, errorf(http.StatusNotFound, "NOT_FOUND", "no block %q in %s", ref.Block, document.Ref)
		}
		node.Text, err = refs.CurrentBlockText(ctx, pool, document.ID, ref.Block)
		return node, err
	}
	at, err := refs.BlockTextAt(ctx, pool, document.ID, ref.Block, ref.Pin)
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
		if err := pool.QueryRow(ctx, `select not `+refs.VisibleKind("block", "$1", "$2"),
			query.ID, refs.VisibilityArg(who)).Scan(&hidden); err != nil {
			return model.GraphNode{}, fmt.Errorf("read whether the reader may see block %s: %w", query.ID, err)
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
		removed, err := refs.RemovedBlockNode(ctx, pool, document, query.ID, ref.Block, at.RemovedVersion)
		if err != nil {
			return model.GraphNode{}, err
		}
		removed.Pin = pin
		return removed, nil
	}
	if node.Text, err = refs.CurrentBlockText(ctx, pool, document.ID, ref.Block); err != nil {
		return model.GraphNode{}, err
	}
	pin.State = "unchanged"
	if node.Text != pin.Text {
		pin.State = "changed"
	}
	node.Pin = pin
	return node, nil
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
			return model.GraphNode{}, s.itemBlockNotFound(ctx, kind, item.ID,
				fmt.Sprintf("no %s %q in %s %s", noun, ref.Block, ref.Item, item.ID))
		}
		return node, nil
	}
	at, err := refs.ItemBlockAt(ctx, pool, kind, item.ID, ref.Block, ref.Pin)
	if err != nil {
		return model.GraphNode{}, err
	}
	switch {
	case ref.Pin > at.Current:
		return model.GraphNode{}, s.itemBlockNotFound(ctx, kind, item.ID,
			fmt.Sprintf("no revision %d of %s %s; its current revision is %d", ref.Pin, ref.Item, item.ID, at.Current))
	case at.Text == nil:
		return model.GraphNode{}, s.itemBlockNotFound(ctx, kind, item.ID,
			fmt.Sprintf("no %s %q in %s %s at revision %d", noun, ref.Block, ref.Item, item.ID, ref.Pin))
	}
	pin := &model.BlockPin{Version: ref.Pin, Ref: refs.BlockRef(item.Ref, ref.Block) + "@v" + strconv.Itoa(ref.Pin), Text: *at.Text}
	if !current {
		removed, held, err := refs.RemovedItemBlockNode(ctx, pool, who, kind, query.ID, at.RemovedRevision)
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
// (handler_errors.go), written again here under that 404. The mark is read on this path alone, so a reference to a
// block that is there costs no query for it.
func (s *server) itemBlockNotFound(ctx context.Context, kind refs.ItemKind, itemID, missing string) error {
	deleted, err := s.postIsDeleted(ctx, kind, itemID)
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
func (s *server) postIsDeleted(ctx context.Context, kind refs.ItemKind, itemID string) (bool, error) {
	if kind != refs.ItemComment && kind != refs.ItemMessage {
		return false, nil
	}
	items, _, err := kind.ItemTable()
	if err != nil {
		return false, err
	}
	var deleted bool
	if err := s.deps.Store.Pool.QueryRow(ctx, `
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
func pinsAMessageBlock(raw string, origins text.Origins) bool {
	unpinned := blockPin.ReplaceAllString(raw, "")
	if unpinned == raw {
		return false
	}
	ref, ok := text.ParseRef(unpinned, origins)
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
func parseRequestRef(raw, origins string) (text.Ref, error) {
	ref, ok := text.ParseRef(raw, origins)
	if !ok {
		if pinsAMessageBlock(raw, origins) {
			return text.Ref{}, errorf(http.StatusBadRequest, "INVALID_REFERENCE", "%q pins a block of a message, which records no revisions; name the block without @v<N>", raw)
		}
		return text.Ref{}, errorf(http.StatusBadRequest, "INVALID_REFERENCE", "%q is not a dispatch:// reference to an issue, document, block, ask, comment, message, or component", raw)
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
func parseReferencesQuery(r *http.Request, origins text.Origins) (refs.Query, text.Ref, []string, error) {
	values := r.URL.Query()
	to, from := strings.TrimSpace(values.Get("to")), strings.TrimSpace(values.Get("from"))
	if (to == "") == (from == "") {
		return refs.Query{}, text.Ref{}, nil, errorf(http.StatusBadRequest, "INVALID_REFERENCE", "exactly one of to or from is required")
	}
	raw, direction := to, "in"
	if from != "" {
		raw, direction = from, "out"
	}
	ref, err := parseRequestRef(raw, origins)
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
		}
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

// documentMentionExcerpts replaces the excerpt of every mention written by a document with the
// excerpt stored when the mention was indexed (refs.RecordExcerpts): the block holding the mention,
// its id a #b-<id> jump target, and its markdown. The route reads no document: a mention whose
// excerpt the reconciliation has not stored yet, is owed a fresh one, or lies in no block keeps
// the document's name. The reconciliation serves the next stored block only after it records a
// non-null excerpt_version. During the deploy that ships migration 0092, a stale excerpt whose
// version is still set is served until the reconciliation marks it owed (refs.StoredExcerpts).
// The excerpts are read for who, so one whose document or block who may not see is none.
func (s *server) documentMentionExcerpts(ctx context.Context, who model.Actor, query refs.Query, edges []model.GraphEdge) error {
	stored, err := refs.StoredExcerpts(ctx, s.deps.Store.Pool, who, query.Kind, query.ID)
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
// before, which read them alone. The read runs under referencesClosureBound (boundedClosure);
// loadIssue before it and loadArtifactsByRefKey after it read by key on the pool, outside the
// bound. ?versions=latest leaves each member's artifact.versions holding only its latest version,
// under its own ETag; absent, empty or all is the full answer. A repeated versions is refused
// rather than read by its first value.
func (s *server) getIssueReferences(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
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
	issue, err := s.loadIssue(r.Context(), s.deps.Store.Pool, who, r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	closure, err := s.boundedClosure(r.Context(), who, issue.Key)
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

// referencesClosureBound is the budget every statement of one GET of an issue's references shares
// (boundedClosure). A read that reaches it answers 503 REFERENCES_TIMEOUT, which the CLI prints as
// references unavailable, rather than holding a connection and a CPU for as long as the read
// takes. On 2026-11-20 from 00:27Z the eight-hop walk this read replaced held Dispatch's Cloud SQL
// instance at full CPU: every `dispatch read --issue` and the Artifacts tab ask for it, and Query
// Insights gave that one statement 31,299 s of execution in 20 minutes, 12 times every other
// statement together (cybermonklabs/legion#4554).
const referencesClosureBound = 2 * time.Second

// boundedClosure runs refs.Closure, as who reads it, in a read transaction of its own, through
// closureBudget, so that every statement of the read - the out and in statements and each loader
// that names their nodes - runs under what is left of referencesClosureBound since the read
// started. One statement_timeout of the whole bound for the transaction would not hold the bound:
// the read sends several statements, whose sum can pass the context's deadline with none past the
// bound, and at that deadline pgx closes the connection while the server's statement runs on (its
// default BuildContextWatcherHandler, as outbox/lease.go records). The context, a second past the
// bound, stays as the last guard, for a wait no statement_timeout covers, such as the begin's. The
// rollback runs on a context of its own, as the near-copy gate's does (duplicates.go): the read's
// context has expired exactly when the rollback matters, and pgx answers a rollback on an expired
// context by destroying the connection instead of returning it to the pool.
func (s *server) boundedClosure(ctx context.Context, who model.Actor, issueKey string) (refs.References, error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, referencesClosureBound+time.Second)
	defer cancel()
	tx, err := s.begin(ctx)
	if err != nil {
		return refs.References{}, err
	}
	defer func() {
		rollback, cancelRollback := context.WithTimeout(context.WithoutCancel(ctx), duplicateGateRollback)
		defer cancelRollback()
		_ = tx.Rollback(rollback)
	}()
	return refs.Closure(ctx, closureBudget{tx: tx, started: started}, who, issueKey, s.deps.ServerURL)
}

// closureBudget is boundedClosure's transaction tx as refs.Closure reads it (refs.Queryer). Before
// each statement it passes on, it gives that statement what is left of referencesClosureBound
// since started, the start of the read (setStatementBudget), so a statement that waits, on a lock
// or for the CPU, takes its wait out of the time of every statement after it. It answers the
// budget's refusal in place of a statement it does not send.
type closureBudget struct {
	tx      pgx.Tx
	started time.Time
}

func (b closureBudget) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := b.spend(ctx); err != nil {
		return nil, err
	}
	return b.tx.Query(ctx, sql, args...)
}

func (b closureBudget) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if err := b.spend(ctx); err != nil {
		return refusedRow{err: err}
	}
	return b.tx.QueryRow(ctx, sql, args...)
}

// spend gives the next statement of the read what is left of referencesClosureBound since
// started (setStatementBudget).
func (b closureBudget) spend(ctx context.Context) error {
	return setStatementBudget(ctx, b.tx, referencesClosureBound-time.Since(b.started))
}

// refusedRow is the row closureBudget answers for a statement it did not send: its Scan answers
// the refusal.
type refusedRow struct{ err error }

func (r refusedRow) Scan(...any) error { return r.err }

// errClosureBudgetSpent is setStatementBudget's answer when less than a millisecond of
// referencesClosureBound is left before a statement of boundedClosure's read: the read is at its
// bound, and the route answers it as it answers a statement Postgres cancelled there.
var errClosureBudgetSpent = errors.New("the reference closure's budget has less than 1ms left")

// setStatementBudget gives the next statement of boundedClosure's transaction tx what is left of
// referencesClosureBound, left, so that the statements of the read share the one bound rather
// than each having it whole. It sets statement_timeout for the rest of tx to the whole
// milliseconds of left, rounded down, 1ms at 1 ms left, with
// `select set_config('statement_timeout', $1, true)` after refs.ClosureTag, which starts every
// statement of the read, so that pg_stat_activity names this statement as the read's too. Under
// 1 ms left it sends no statement and answers errClosureBudgetSpent, which closureBoundHit reads
// as the bound, because the timeout rounded down would be 0, and a statement_timeout of 0 turns
// the timeout off.
func setStatementBudget(ctx context.Context, tx pgx.Tx, left time.Duration) error {
	milliseconds := left.Milliseconds()
	if milliseconds < 1 {
		return errClosureBudgetSpent
	}
	_, err := tx.Exec(ctx, refs.ClosureTag+"select set_config('statement_timeout', $1, true)", strconv.FormatInt(milliseconds, 10)+"ms")
	return err
}

// closureBoundHit says whether boundedClosure's read ended at its bound: Postgres cancelled a
// statement at the statement_timeout its budget set (SQLSTATE 57014), the budget had less than a
// millisecond left for the next statement (errClosureBudgetSpent), or the context expired first.
func closureBoundHit(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errClosureBudgetSpent) {
		return true
	}
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "57014" // query_canceled
}

func (s *server) getArtifactReferences(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	artifact, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, who, r.PathValue("id"))
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
// member's artifact.name, and the delete of a comment whose accepted suggestion named a version
// after the comment's body renames that version (renameAcceptedVersion), while this issue's
// event sequence, the closure and each member's latest version stand still. A row of links or
// linked_from carries its node's name, last change and parent, and another node's new link to
// the issue adds a row, none of which moves the issue's own sequence either. A token over those
// would answer a reader holding it 304 over the old words; a token over the answer moves with
// every byte a reader is shown, and the build before, which writes neither list, writes other
// bytes for the same issue, so the two builds never share an ETag.
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
	versions := `join artifact_versions v on v.artifact_id = a.id`
	if latestOnly {
		versions = `join lateral (
			select * from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1
		) v on true`
	}
	rows, err := q.Query(ctx, `
		select a.id::text, a.issue_key, coalesce(a.project_key, ''), a.session_id, a.ref_key, a.slug, a.name, a.kind, a.is_primary,
		       a.created_by, a.created_at,
		       `+versionColumns+`
		from artifacts a
		`+versions+`
		where a.ref_key = any($1)
		order by a.ref_key, v.number
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
			&createdBy,
			&artifact.CreatedAt,
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
