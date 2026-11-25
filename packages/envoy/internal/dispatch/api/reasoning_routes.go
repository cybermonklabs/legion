package api

// reasoning_routes.go, constraint_routes.go and pointer_routes.go serve the writes of the nodes
// that record how a piece of work was reasoned about - a lens, its findings, a risk, a
// constraint - and the list of an issue's constraints: a lens invoked on a node of the issue
// with its findings (POST /api/v1/issues/{key}/lenses), a finding added to a lens later (POST
// .../nodes/{id}/findings), a risk under a pre-mortem (POST .../risks), a constraint under the
// issue (POST .../constraints), one pointer tuple appended, retracted or retargeted out of a node
// (POST .../nodes/{id}/pointers), and the constraints under the issue (GET .../constraints).
//
// Each write goes through the one write path of these kinds (writeReasoningNodes, or
// writeReasoningPointer for a tuple out of a node that exists already), in one transaction of the
// shared pool, at graphstore.NoEvent: no event is appended and no delivery is sent, so a reader
// learns of one of these nodes by reading it. A repeat of a request under its Idempotency-Key is
// answered from the key and writes nothing (keyedWrites). The type rules of the pointers and of
// a risk's place, the project and the document of a risk's prevented_by and the order of a
// triggered_by are the store's (graphstore's pointerRules), and so is the list of the kinds the
// pointer route writes (graphstore.PointerKinds): a route keeps no type rule of its own, and
// answers the store's refusals, 400 INVALID_POINTER and 409 POINTER_ALREADY_ASSERTED, as they are,
// naming a check of another project or of a retired document, and both lenses of a triggered_by
// to a lens not invoked before its own, by the addresses the caller read them at (refusalNamed).
// That each end a tuple names exists, is not gone and may be seen by the caller is the route's: an
// end under {key} is found through FindNodeUnderIssue, a prevented_by's check, which stands under
// any issue of the project, and an end that stands under no issue - a lens_kind, a constraint of
// the catalogue - through refs.Node with the caller's own visibility (pointerEnd), so an end the
// caller may not see answers as one that does not exist.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// lensCatalogueRead is where a caller reads the ids a lens's kind may take, which a refusal of an
// id the catalogue does not hold names.
const lensCatalogueRead = "GET /api/v1/schema/blocks lists the catalogue's kinds under lens_kinds"

// Codes of the reasoning routes' refusals of a body; INVALID_POINTER, a far end of the wrong
// type, which a route answers as the store's pointer rules answer it (writeHandlerError); and
// VISIBILITY_WIDER_THAN_PARENT, a node every reader may see placed under or on one reserved for
// its author, which every route answers as the store refuses it (widerThanParentWords).
const (
	codeLensKindRequired          = "LENS_KIND_REQUIRED"
	codeLensInput                 = "LENS_INPUT"
	codeFindingInput              = "FINDING_INPUT"
	codeRiskInput                 = "RISK_INPUT"
	codeConstraintInput           = "CONSTRAINT_INPUT"
	codePointerInput              = "POINTER_INPUT"
	codeInvalidPointer            = "INVALID_POINTER"
	codeVisibilityWiderThanParent = "VISIBILITY_WIDER_THAN_PARENT"
)

// reasoningAuthor is the writer a reasoning node's authored_by tuple names, the one
// graphstore.AuthorOf names of its made_by: a person, by canonical login, or a session - on the
// deployment's shared bearer, and on a pod's write, the session the request's body names, which
// that credential does not prove (spec v46 R12). A node whose write names only a workload, or
// nobody AuthorOf names, has none.
type reasoningAuthor struct {
	Kind string `json:"kind" enum:"person,session"`
	ID   string `json:"id"`
}

// writtenAuthor is the writer writeReasoningNodes stated of node through its authored_by tuple:
// the one graphstore.AuthorOf names of its made_by, where the store can key that writer's id
// (graphstore.AuthorFacts), and nil otherwise. It is attribution, whichever credential proved the
// caller, as every read of the node answers it from that tuple; the author the one rule compares
// is the one made_by proves (graphstore.SameAuthor).
func writtenAuthor(node graphstore.Node) *reasoningAuthor {
	kind, id, ok := graphstore.AuthorOf(node.MadeBy)
	if !ok || !graphstore.AddressFits(id) {
		return nil
	}
	return &reasoningAuthor{Kind: kind, ID: id}
}

// reasoningCaller is who one reasoning write is made by: proving is the credential that proved
// the caller (optionalActor's actor), which decides whether the write may reserve a node for its
// author and is who an author-only node is made by, and recorded is the actor the write records
// (actorFrom's), the session the body names on a bearer that proves none, as a comment's is.
type reasoningCaller struct {
	proving, recorded model.Actor
}

// reader is the actor the caller reads the store as, which every read of an end a write names
// binds the visibility predicate for, as GET /api/v1/references reads as the caller.
func (c reasoningCaller) reader() model.Actor { return c.proving.Recorded() }

// reasoningCallerOf resolves the caller of a reasoning write, answering the refusal itself and
// false where it does not resolve: a caller with no credential is 401, and a bearer that names
// no session in actor is 400 ACTOR_KIND, as every write that records its actor answers. The
// credential is verified once (resolveCaller), and the recorded actor read off that resolution.
func (s *server) reasoningCallerOf(w http.ResponseWriter, r *http.Request, supplied *model.Actor) (reasoningCaller, bool) {
	proving, resolved, ok := s.resolveCaller(w, r)
	if !ok {
		return reasoningCaller{}, false
	}
	recorded, ok := s.requireActor(w, resolved, supplied)
	if !ok {
		return reasoningCaller{}, false
	}
	return reasoningCaller{proving: proving, recorded: recorded}, true
}

// resolveCaller resolves the request's caller once and answers it with the request that carries
// it (resolvedCallerKey), so requireActor and actingPerson read that caller off the request rather
// than verify the credential again: a personal token costs a read and an update each time it is
// verified. A caller that does not resolve is answered here, and false.
func (s *server) resolveCaller(w http.ResponseWriter, r *http.Request) (model.Actor, *http.Request, bool) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return model.Actor{}, nil, false
	}
	return actor, r.WithContext(context.WithValue(r.Context(), resolvedCallerKey{}, resolvedCaller{actor: actor, human: human})), true
}

// storedEnd is a node as one end of a tuple names it: a kind and an id.
type storedEnd struct{ kind, id string }

// stored is the end as the store keys its node, the near end the node's own tuples carry: the end
// itself, but for a finding of a lens, which a tuple names lens_block and the store holds as a node
// of kind block at its own id (refs.StoredKindOf).
func (e storedEnd) stored() storedEnd {
	e.kind = refs.StoredKindOf(e.kind)
	return e
}

// findEndUnderIssue is FindNodeUnderIssue's node for address, with the kind and the id a tuple
// whose far end it is names it by (farEndOf).
func findEndUnderIssue(ctx context.Context, q refs.Queryer, who model.Actor, key, address string) (model.GraphNode, storedEnd, error) {
	node, err := FindNodeUnderIssue(ctx, q, who, key, address)
	if err != nil {
		return model.GraphNode{}, storedEnd{}, err
	}
	return node, farEndOf(node, address), nil
}

// namedEnd is one end of a tuple as the route read it for its caller: the kind and the id the tuple
// names it by, and the dispatch:// address the caller read it at.
type namedEnd struct {
	storedEnd
	address string
}

// refusalNamed is err naming each of ends by its address, where err is the store's refusal of a
// tuple that names that end by the id the store keys it by (graphstore.InvalidPointerError.NamedAt):
// a check of another project than the tuple's near end or of a retired document, which the store
// keys by its document's ref_key and its block id, and both lenses of a triggered_by to a lens not
// invoked before its own, which it keys by their uuids, where the caller read each at its
// dispatch:// address. named reports that it named one. Any other err it answers as it is.
func refusalNamed(err error, ends ...namedEnd) (refusal error, named bool) {
	var invalid *graphstore.InvalidPointerError
	if !errors.As(err, &invalid) {
		return err, false
	}
	for _, end := range ends {
		if at, ok := invalid.NamedAt(end.kind, end.id, end.address); ok {
			invalid, named = at, true
		}
	}
	if !named {
		return err, false
	}
	return invalid, true
}

// widerThanParentWords is the words of the store's refusal of a part_of out of a node every reader
// may see at one reserved for its author (graphstore.VisibilityWiderThanParentError), naming each
// of the two by its dispatch:// address (refs.StoreRef): a node of these kinds, and the node a
// lens stands on where that is one of them too, since only such a node is ever reserved for its
// author. ok is false for any other err.
func widerThanParentWords(err error) (words string, ok bool) {
	var wider *graphstore.VisibilityWiderThanParentError
	if !errors.As(err, &wider) {
		return "", false
	}
	return wider.Named(refs.StoreRef(wider.ChildKind, wider.ChildID), refs.StoreRef(wider.ParentKind, wider.ParentID)), true
}

// farEndOf is the kind and the id a tuple whose far end is node names it by, where address is what
// named it (decodeNodeAddress): the node's own kind and id, but for a document, whose node answers
// its uuid and which a tuple names by its ref_key, the id its address decodes to, and for a block
// of an ask, a comment or a message, whose node answers the kind block as a document's block does
// (refs' loadItemBlockNodes) and which a tuple names by its item's block kind, the kind its address
// decodes to (comment_block; refs.TargetOf). graph_nodes keys such a block under that kind, and
// every reader of an edge into one keys it so: graph_edges, the references read of the block, the
// backlink arm of a lens standing on it and the lens read. A finding of a lens is named lens_block,
// the kind the graph addresses it by, at its own id, as a tuple into a block of an item names that
// item's block kind: graph_edges answers the tuple as it is stated, so every read of an edge into a
// finding - refs.Edges, the backlink arm of a lens standing on it, the lens read - finds it under
// that kind, and a predicate over that end reads the store's block row behind it
// (refs.VisibleEnd).
func farEndOf(node model.GraphNode, address string) storedEnd {
	end := storedEnd{kind: node.Kind, id: node.ID}
	switch node.Kind {
	case graphstore.KindArtifact:
		_, end.id, _ = decodeNodeAddress(address)
	case refs.BlockNodeKind:
		end.kind, _, _ = decodeNodeAddress(address)
	}
	return end
}

// lensKindNamed is the lens_kind node of the catalogue id names - the bare id (`pre-mortem`), its
// store address (`lens_kind/pre-mortem`) or its whole address (`dispatch://lens_kind/pre-mortem`)
// - read for who through refs.Node. A kind of the catalogue stands under no issue and every
// viewer reads it, so an id no lens_kind node holds is the caller's word to fix, 400
// INVALID_POINTER, and tells nothing of a node the caller may not see.
func lensKindNamed(ctx context.Context, q refs.Queryer, who model.Actor, id string) (model.GraphNode, error) {
	slug := strings.TrimPrefix(strings.TrimPrefix(id, "dispatch://"), graphstore.KindLensKind+"/")
	node, found, err := refs.Node(ctx, q, who, graphstore.KindLensKind, slug, false)
	if err != nil {
		return model.GraphNode{}, err
	}
	if !found {
		return model.GraphNode{}, errorf(http.StatusBadRequest, codeInvalidPointer,
			"no lens_kind node holds the id %q: a lens's kind is one of the catalogue's, and %s", id, lensCatalogueRead)
	}
	return node, nil
}

// transactionNow is the time the write records, the transaction's own now(), which every host
// writing the database agrees on.
func transactionNow(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var at time.Time
	if err := tx.QueryRow(ctx, `select now()`).Scan(&at); err != nil {
		return time.Time{}, err
	}
	return at.UTC(), nil
}

// blankText is whether a text a reasoning write stores holds nothing a reader could read.
func blankText(text string) bool { return strings.TrimSpace(text) == "" }

// --- POST /api/v1/issues/{key}/lenses -----------------------------------------------------------

// lensInvocationInput is the body of a lens's invocation.
type lensInvocationInput struct {
	Actor *model.Actor `json:"actor"`
	// On is the address of the node the lens is invoked on, which stands under the issue the
	// path names (FindNodeUnderIssue): the issue itself, a document or a block of one, an ask, a
	// comment or a message or a block of one, or another lens, a finding, a risk or a
	// constraint of the issue.
	On string `json:"on" required:"true"`
	// Kind is the id of the lens_kind node of the catalogue the lens is of.
	Kind string `json:"kind" required:"true"`
	// Verdict is what the analysis concluded, stored as the lens's verdict attribute.
	Verdict *string `json:"verdict,omitempty"`
	// Findings are the lens's findings in order, each the text of one block node under the lens.
	Findings []string `json:"findings,omitempty"`
	// TriggeredBy is the address of the lens of this issue whose finding asked for this one.
	TriggeredBy *string `json:"triggered_by,omitempty"`
	// Visibility is who the lens and its findings are for: author, or project. With none the lens
	// takes the visibility of the node it is invoked on, so a lens on a node reserved for its
	// author is reserved for that author too; project on such a node is 400
	// VISIBILITY_WIDER_THAN_PARENT.
	Visibility string `json:"visibility" enum:"project,author"`
}

// lensFinding is one finding of a lens as a write answers it: a block node under the lens whose
// id names the lens (graphstore.FindingID), the block id a random one, and whose address is the
// lens's with that block id, `dispatch://lens/<uuid>#b-<block id>`. Its rank among the lens's
// children is the order of its part_of tuple and no value of its own.
type lensFinding struct {
	Address    string           `json:"address"`
	ID         string           `json:"id"`
	BlockID    string           `json:"block_id"`
	Lens       string           `json:"lens"`
	Text       string           `json:"text"`
	Author     *reasoningAuthor `json:"author"`
	CreatedAt  time.Time        `json:"created_at"`
	State      string           `json:"state" enum:"live,gone,proposed,settled,overtaken"`
	Visibility string           `json:"visibility" enum:"project,author"`
}

// lensInvoked is the lens a write invoked: its address, `dispatch://lens/<uuid>`, its kind, the
// address of the node it stands on, and each finding it was written with, in order.
type lensInvoked struct {
	Address     string           `json:"address"`
	ID          string           `json:"id"`
	Kind        string           `json:"kind"`
	On          string           `json:"on"`
	Verdict     *string          `json:"verdict"`
	TriggeredBy *string          `json:"triggered_by"`
	Author      *reasoningAuthor `json:"author"`
	CreatedAt   time.Time        `json:"created_at"`
	State       string           `json:"state" enum:"live,gone,proposed,settled,overtaken"`
	Visibility  string           `json:"visibility" enum:"project,author"`
	Findings    []lensFinding    `json:"findings"`
}

// newLens is the lens id of the catalogue kind whose id is kind, invoked on the node on, at the
// visibility its writer gives it (the empty string being project; the rule of a visibility the
// request leaves out is the route's), with its verdict where verdict is not nil: a node under on
// whose row carries the verdict, and its one has_kind tuple to the kind's lens_kind node. The
// lens route appends its triggered_by where the request names one. The lens route and the
// pre-mortem conversion build each lens here.
func newLens(id, kind string, on storedEnd, visibility string, verdict *string) reasoningNode {
	lens := reasoningNode{
		kind: graphstore.KindLens, id: id, visibility: visibility,
		parentKind: on.kind, parentID: on.id,
		tuples: []graphstore.Tuple{{
			FromKind: graphstore.KindLens, FromID: id, Kind: graphstore.TupleHasKind,
			ToKind: graphstore.KindLensKind, ToID: kind,
		}},
	}
	if verdict != nil {
		lens.attributes = map[string]any{graphstore.AttributeVerdict: *verdict}
	}
	return lens
}

// newRisk is the risk id under the lens lens, at the visibility its writer gives it (the empty
// string being project; the rule of a visibility the request leaves out is the route's), naming
// the failure heading, the sign it is seen coming by earlyWarning and what keeps it from happening
// prevention, the three attributes of its row, with one prevented_by tuple to each of checks, in
// order. The risk route and the pre-mortem conversion build each risk here.
func newRisk(id string, lens storedEnd, visibility, heading, earlyWarning, prevention string, checks ...storedEnd) reasoningNode {
	risk := reasoningNode{
		kind: graphstore.KindRisk, id: id, visibility: visibility,
		parentKind: lens.kind, parentID: lens.id,
		attributes: map[string]any{
			graphstore.AttributeHeading:      heading,
			graphstore.AttributeEarlyWarning: earlyWarning,
			graphstore.AttributePrevention:   prevention,
		},
	}
	if len(checks) > 0 {
		risk.tuples = make([]graphstore.Tuple, 0, len(checks))
	}
	for _, check := range checks {
		risk.tuples = append(risk.tuples, graphstore.Tuple{
			FromKind: graphstore.KindRisk, FromID: id, Kind: graphstore.TuplePreventedBy,
			ToKind: check.kind, ToID: check.id,
		})
	}
	return risk
}

// lensAddress is the address of the lens whose id is id.
func lensAddress(id string) string { return refs.StoreRef(graphstore.KindLens, id) }

// newFinding is a finding of the lens whose id is lens, holding text, at the visibility asked
// for: a block node at a random block id, under the lens, whose text is its `text` value tuple.
func newFinding(lens, visibility, text string) reasoningNode {
	id := graphstore.FindingID(lens, uuid.NewString())
	return reasoningNode{
		kind: refs.BlockNodeKind, id: id, visibility: visibility,
		parentKind: graphstore.KindLens, parentID: lens,
		tuples: []graphstore.Tuple{{
			FromKind: refs.BlockNodeKind, FromID: id, Kind: graphstore.AttributeText,
			ToKind: graphstore.KindValue, ToID: text,
		}},
	}
}

// findingWritten is the answer for a finding the write wrote as node, holding text.
func findingWritten(node graphstore.Node, text string) lensFinding {
	lens, blockID, _ := graphstore.ParseFindingID(node.ID)
	return lensFinding{
		Address: refs.StoreRef(graphstore.KindLensBlock, node.ID), ID: node.ID, BlockID: blockID,
		Lens: lensAddress(lens), Text: text, Author: writtenAuthor(node),
		CreatedAt: node.MadeAt, State: node.State, Visibility: node.Visibility,
	}
}

// invokeLens is POST /api/v1/issues/{key}/lenses: one lens of one kind of the catalogue, invoked
// on one node under the issue, with its verdict, its findings and the lens that triggered it. A
// body with no kind is 400 LENS_KIND_REQUIRED, an id no lens_kind node holds 400 INVALID_POINTER,
// a node or a triggering lens the caller may not see under the issue 404 NOT_FOUND, `author` on a
// credential that proves no author 403 AUTHOR_NAMES_NOBODY, and `project` on a node reserved for
// its author the store's 400 VISIBILITY_WIDER_THAN_PARENT, naming the lens and that node; each
// stores nothing. A body that names no visibility gives the lens and its findings the visibility
// of the node it is invoked on. A second has_kind cannot be asked for here, since the kind is one
// field, and the lens this writes cannot trigger itself or a lens invoked after it, since its id
// is drawn here and its part_of, in the same write, stands after every lens the store holds; the
// store holds every rule all the same, and a refusal of its trigger names both lenses by address
// (refusalNamed).
func (s *server) invokeLens(w http.ResponseWriter, r *http.Request) {
	var input lensInvocationInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	caller, ok := s.reasoningCallerOf(w, r, input.Actor)
	if !ok {
		return
	}
	if blankText(input.Kind) {
		writeError(w, codeLensKindRequired, http.StatusBadRequest,
			"a lens carries exactly one kind: name the id of a lens_kind node in kind; "+lensCatalogueRead)
		return
	}
	if blankText(input.On) {
		writeError(w, codeLensInput, http.StatusBadRequest,
			"on is required: the dispatch:// address of the node under the issue the lens is invoked on")
		return
	}
	for index, text := range input.Findings {
		if blankText(text) {
			writeError(w, codeLensInput, http.StatusBadRequest, fmt.Sprintf("finding %d holds no text", index+1))
			return
		}
	}
	if _, err := graphstore.VisibilityFor(input.Visibility, caller.proving); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ctx, key, reader := r.Context(), r.PathValue("key"), caller.reader()
	tx, err := s.begin(ctx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	at, err := transactionNow(ctx, tx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	on, onEnd, err := findWrittenNode(ctx, tx, reader, key, input.On)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	kind, err := lensKindNamed(ctx, tx, reader, input.Kind)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	id := uuid.NewString()
	address := lensAddress(id)
	// A lens with no visibility of its own is no wider than the node it stands on.
	visibility := cmp.Or(input.Visibility, on.Visibility)
	var verdict *string
	if input.Verdict != nil && !blankText(*input.Verdict) {
		verdict = input.Verdict
	}
	lens := newLens(id, kind.ID, onEnd, visibility, verdict)
	var triggeredBy *string
	// The lens a refusal of the lens's triggered_by names after the lens; a lens invoked with no
	// trigger leaves it the zero end, whose empty address NamedAt reads as not named.
	var triggerNamed namedEnd
	// The trigger, which the write reads again under the issue's owner lock (refuseGoneFarEnds).
	var farEnds []namedEnd
	if input.TriggeredBy != nil {
		trigger, triggerEnd, err := findEndUnderIssue(ctx, tx, reader, key, *input.TriggeredBy)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		lens.tuples = append(lens.tuples, graphstore.Tuple{
			FromKind: graphstore.KindLens, FromID: id, Kind: graphstore.TupleTriggeredBy,
			ToKind: triggerEnd.kind, ToID: triggerEnd.id,
		})
		triggeredBy = &trigger.Ref
		triggerNamed = namedEnd{storedEnd: triggerEnd, address: trigger.Ref}
		farEnds = []namedEnd{{storedEnd: triggerEnd, address: *input.TriggeredBy}}
	}
	nodes := []reasoningNode{lens}
	for _, text := range input.Findings {
		nodes = append(nodes, newFinding(id, visibility, text))
	}
	written, err := writeReasoningNodes(ctx, tx, reasoningWrite{
		reasoningCaller: caller, issue: key, at: at, nodes: nodes, farEnds: farEnds,
	})
	if err != nil {
		// The ends a refusal of the lens's triggered_by names: the lens, then the lens it names.
		refusal, _ := refusalNamed(err, namedEnd{storedEnd: storedEnd{kind: graphstore.KindLens, id: id}, address: address}, triggerNamed)
		s.writeHandlerError(w, refusal)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	invoked := written[0]
	answer := lensInvoked{
		Address: address, ID: id, Kind: kind.ID, On: cmp.Or(on.Ref, input.On),
		Verdict: verdict, TriggeredBy: triggeredBy, Author: writtenAuthor(invoked),
		CreatedAt: invoked.MadeAt, State: invoked.State, Visibility: invoked.Visibility,
		Findings: make([]lensFinding, 0, len(input.Findings)),
	}
	for index, text := range input.Findings {
		answer.Findings = append(answer.Findings, findingWritten(written[index+1], text))
	}
	WriteJSON(w, http.StatusCreated, answer)
}

// --- POST /api/v1/issues/{key}/nodes/{id}/findings ----------------------------------------------

// lensFindingInput is the body of a finding added to a lens.
type lensFindingInput struct {
	Actor *model.Actor `json:"actor"`
	// Text is the finding's text.
	Text string `json:"text" required:"true"`
	// Visibility is who the finding is for: author, or project. With none the finding takes its
	// lens's, so a finding under a lens reserved for its author is reserved for that author too;
	// project under such a lens is 400 VISIBILITY_WIDER_THAN_PARENT, and any other word is 400
	// INVALID_ATTRIBUTE_VALUE.
	Visibility string `json:"visibility" enum:"project,author"`
}

// addLensFinding is POST /api/v1/issues/{key}/nodes/{id}/findings: one finding added to the lens
// {id} names, a block node at a random block id under the lens, after every finding it holds. A
// body that names no visibility gives the finding its lens's, and the store writes the visibility
// a body names as named: `project` under a lens reserved for its author is the store's 400
// VISIBILITY_WIDER_THAN_PARENT, naming the finding and the lens, and stores nothing, since no
// write is narrowed in silence. {id} is the lens's store address (FindNodeUnderIssue): a node the
// caller may not see under the issue is 404 NOT_FOUND, and a node the caller sees that is no lens
// is 400 INVALID_POINTER, since a finding stands under a lens alone and the refusal names nothing
// the caller could not read.
func (s *server) addLensFinding(w http.ResponseWriter, r *http.Request) {
	var input lensFindingInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	caller, ok := s.reasoningCallerOf(w, r, input.Actor)
	if !ok {
		return
	}
	if blankText(input.Text) {
		writeError(w, codeFindingInput, http.StatusBadRequest, "text is required: the finding's text")
		return
	}
	if _, err := graphstore.VisibilityFor(input.Visibility, caller.proving); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ctx, key := r.Context(), r.PathValue("key")
	tx, err := s.begin(ctx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	at, err := transactionNow(ctx, tx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	lens, _, err := findWrittenNode(ctx, tx, caller.reader(), key, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if lens.Kind != graphstore.KindLens {
		writeError(w, codeInvalidPointer, http.StatusBadRequest, fmt.Sprintf(
			"a finding stands under a lens, and %s is %s", cmp.Or(lens.Ref, r.PathValue("id")), graphstore.WithArticle(lens.Kind)))
		return
	}
	// A finding with no visibility of its own is no wider than the lens it stands under; one that
	// names a visibility is written at it, and the store refuses project under an author-only lens.
	visibility := cmp.Or(input.Visibility, lens.Visibility)
	written, err := writeReasoningNodes(ctx, tx, reasoningWrite{
		reasoningCaller: caller, issue: key, at: at,
		nodes: []reasoningNode{newFinding(lens.ID, visibility, input.Text)},
	})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, findingWritten(written[0], input.Text))
}

// --- POST /api/v1/issues/{key}/risks ------------------------------------------------------------

// riskInput is the body of a risk under a pre-mortem.
type riskInput struct {
	Actor *model.Actor `json:"actor"`
	// Lens is the address of the pre-mortem lens of this issue the risk stands under.
	Lens string `json:"lens" required:"true"`
	// Heading is the failure the risk names.
	Heading string `json:"heading" required:"true"`
	// EarlyWarning is the sign the failure is seen coming by.
	EarlyWarning string `json:"early_warning" required:"true"`
	// Prevention is what keeps the failure from happening.
	Prevention string `json:"prevention" required:"true"`
	// PreventedBy is the address of a check block that covers the risk, of any issue of the
	// issue's project.
	PreventedBy *string `json:"prevented_by,omitempty"`
	// Visibility is who the risk is for: author, or project. With none the risk takes its lens's,
	// so a risk under a lens reserved for its author is reserved for that author too; project
	// under such a lens is 400 VISIBILITY_WIDER_THAN_PARENT.
	Visibility string `json:"visibility" enum:"project,author"`
}

// riskWritten is the risk a write wrote: its address, `dispatch://risk/<uuid>`, the lens it
// stands under, its attributes, and the checks that prevent it, by address.
type riskWritten struct {
	Address      string           `json:"address"`
	ID           string           `json:"id"`
	Lens         string           `json:"lens"`
	Heading      string           `json:"heading"`
	EarlyWarning string           `json:"early_warning"`
	Prevention   string           `json:"prevention"`
	PreventedBy  []string         `json:"prevented_by"`
	Author       *reasoningAuthor `json:"author"`
	CreatedAt    time.Time        `json:"created_at"`
	State        string           `json:"state" enum:"live,gone,proposed,settled,overtaken"`
	Visibility   string           `json:"visibility" enum:"project,author"`
}

// writeRisk is POST /api/v1/issues/{key}/risks: one risk under a lens of the issue, with its
// heading, its early warning and its prevention as attributes of its row, and a prevented_by
// tuple to the check its body names, a check of any issue of the project, read as the pointer
// route reads one (pointerEnd). The store holds a risk to a lens of kind pre-mortem: a risk under
// any other lens, or under a node that is no lens, is its 400 INVALID_POINTER, which says a risk
// belongs under a pre-mortem, and a prevented_by to a block that is no check is the same, as is
// one to a check of another project or of a retired document, naming the check. A body that names
// no visibility gives the risk its lens's, and `project` under a lens reserved for its author is
// the store's 400 VISIBILITY_WIDER_THAN_PARENT, naming the risk and the lens. A lens the caller
// may not see under the issue, and a check the caller may not see wherever it stands, are 404
// NOT_FOUND. Each stores nothing. A risk that names a prevented_by takes the issue's owner lock
// and the owner lock of the check's document before it reads either (its race with a retirement
// or an index pass: lockReasoningOwners); any other takes the issue's owner lock at the write
// (writeReasoningNodes).
func (s *server) writeRisk(w http.ResponseWriter, r *http.Request) {
	var input riskInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	caller, ok := s.reasoningCallerOf(w, r, input.Actor)
	if !ok {
		return
	}
	for _, required := range []struct{ name, value string }{
		{"lens", input.Lens}, {"heading", input.Heading},
		{"early_warning", input.EarlyWarning}, {"prevention", input.Prevention},
	} {
		if blankText(required.value) {
			writeError(w, codeRiskInput, http.StatusBadRequest, required.name+" is required: a risk names the pre-mortem lens it stands under (lens), the failure (heading), the sign it is seen coming by (early_warning) and what keeps it from happening (prevention)")
			return
		}
	}
	if _, err := graphstore.VisibilityFor(input.Visibility, caller.proving); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ctx, key, reader := r.Context(), r.PathValue("key"), caller.reader()
	tx, err := s.begin(ctx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	if input.PreventedBy != nil {
		if err := lockReasoningOwners(ctx, tx, key, []string{*input.PreventedBy}); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	at, err := transactionNow(ctx, tx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	lens, lensEnd, err := findWrittenNode(ctx, tx, reader, key, input.Lens)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	id := uuid.NewString()
	preventedBy := []string{}
	var check model.GraphNode
	var checkEnd storedEnd
	var checks []storedEnd
	if input.PreventedBy != nil {
		check, checkEnd, err = pointerEnd(ctx, tx, reader, key, graphstore.TuplePreventedBy, *input.PreventedBy)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		checks = []storedEnd{checkEnd}
		preventedBy = append(preventedBy, check.Ref)
	}
	risk := newRisk(id, lensEnd, cmp.Or(input.Visibility, lens.Visibility), input.Heading, input.EarlyWarning, input.Prevention, checks...)
	written, err := writeReasoningNodes(ctx, tx, reasoningWrite{
		reasoningCaller: caller, issue: key, at: at, nodes: []reasoningNode{risk},
	})
	if err != nil {
		refusal, _ := refusalNamed(err, namedEnd{storedEnd: checkEnd, address: check.Ref})
		s.writeHandlerError(w, refusal)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	node := written[0]
	WriteJSON(w, http.StatusCreated, riskWritten{
		Address: refs.StoreRef(graphstore.KindRisk, id), ID: id, Lens: cmp.Or(lens.Ref, input.Lens),
		Heading: input.Heading, EarlyWarning: input.EarlyWarning, Prevention: input.Prevention,
		PreventedBy: preventedBy, Author: writtenAuthor(node),
		CreatedAt: node.MadeAt, State: node.State, Visibility: node.Visibility,
	})
}
