package api

// reasoning_routes.go serves the writes of the nodes that record how a piece of work was reasoned
// about - a lens, its findings, a risk, a constraint - and the list of an issue's constraints: a
// lens invoked on a node of the issue with its findings (POST /api/v1/issues/{key}/lenses), a
// finding added to a lens later (POST .../nodes/{id}/findings), a risk under a pre-mortem (POST
// .../risks), a constraint under the issue (POST .../constraints), one pointer tuple appended,
// retracted or retargeted out of a node (POST .../nodes/{id}/pointers), and the constraints under
// the issue (GET .../constraints).
//
// Each write goes through the one write path of these kinds (writeReasoningNodes, or
// writeReasoningPointer for a tuple out of a node that exists already), in one transaction of the
// shared pool, at graphstore.NoEvent: no event is appended and no delivery is sent, so a reader
// learns of one of these nodes by reading it. A repeat of a request under its Idempotency-Key is
// answered from the key and writes nothing (keyedWrites). The type rules of the pointers and of
// a risk's place, and the project of a risk's prevented_by, are the store's (graphstore's
// pointerRules): a route keeps none of its own, and answers the store's refusals, 400
// INVALID_POINTER and 409 POINTER_ALREADY_ASSERTED, as they are, naming a check of another project
// by the address the caller read it at (farEndNamed). That each end a tuple names exists, is not
// gone and may be seen by the caller is the route's: an end under {key} is found through
// FindNodeUnderIssue, a prevented_by's check, which stands under any issue of the project, and an
// end that stands under no issue - a lens_kind, a constraint of the catalogue - through refs.Node
// with the caller's own visibility (pointerEnd), so an end the caller may not see answers as one
// that does not exist.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
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

// Codes of the reasoning routes' refusals of a body, and INVALID_POINTER, a far end of the wrong
// type, which a route answers as the store's pointer rules answer it (writeHandlerError).
const (
	codeLensKindRequired = "LENS_KIND_REQUIRED"
	codeLensInput        = "LENS_INPUT"
	codeFindingInput     = "FINDING_INPUT"
	codeRiskInput        = "RISK_INPUT"
	codeConstraintInput  = "CONSTRAINT_INPUT"
	codePointerInput     = "POINTER_INPUT"
	codeInvalidPointer   = "INVALID_POINTER"
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

// farEndNamed is err naming the far end end by node's address, where err is the store's refusal
// of a tuple at end for standing in another project than the tuple's near end
// (graphstore.InvalidPointerError.NamedAt): the store keys a block by its document's ref_key and
// its block id, and the caller read it at its dispatch:// address. named reports that it is. Any
// other err it answers as it is.
func farEndNamed(err error, end storedEnd, node model.GraphNode) (refusal error, named bool) {
	var invalid *graphstore.InvalidPointerError
	if !errors.As(err, &invalid) {
		return err, false
	}
	if at, ok := invalid.NamedAt(end.kind, end.id, node.Ref); ok {
		return at, true
	}
	return err, false
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
	// Visibility is who the lens and its findings are for: project, the default, or author.
	Visibility string `json:"visibility" enum:"project,author"`
}

// lensFinding is one finding of a lens as a write answers it: a block node under the lens whose
// id names the lens, `lens/<uuid>#<block id>`, the block id a random one, and whose address is
// the lens's with that block id, `dispatch://lens/<uuid>#b-<block id>`. Its rank among the lens's
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

// lensAddress is the address of the lens whose id is id.
func lensAddress(id string) string { return refs.StoreRef(graphstore.KindLens, id) }

// newFinding is a finding of the lens whose id is lens, holding text, at the visibility asked
// for: a block node at a random block id, under the lens, whose text is its `text` value tuple.
func newFinding(lens, visibility, text string) reasoningNode {
	id := graphstore.KindLens + "/" + lens + "#" + uuid.NewString()
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
	lens, blockID, _ := strings.Cut(strings.TrimPrefix(node.ID, graphstore.KindLens+"/"), "#")
	return lensFinding{
		Address: refs.StoreRef(graphstore.KindLensBlock, node.ID), ID: node.ID, BlockID: blockID,
		Lens: lensAddress(lens), Text: text, Author: writtenAuthor(node),
		CreatedAt: node.MadeAt, State: node.State, Visibility: node.Visibility,
	}
}

// invokeLens is POST /api/v1/issues/{key}/lenses: one lens of one kind of the catalogue, invoked
// on one node under the issue, with its verdict, its findings and the lens that triggered it. A
// body with no kind is 400 LENS_KIND_REQUIRED, an id no lens_kind node holds 400 INVALID_POINTER,
// a node or a triggering lens the caller may not see under the issue 404 NOT_FOUND, and `author`
// on a credential that proves no author 403 AUTHOR_NAMES_NOBODY; each stores nothing. A second
// has_kind cannot be asked for here, since the kind is one field, and a lens cannot trigger
// itself, since its id is drawn here; the store holds both rules all the same.
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
	on, onEnd, err := findEndUnderIssue(ctx, tx, reader, key, input.On)
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
	lens := reasoningNode{
		kind: graphstore.KindLens, id: id, visibility: input.Visibility,
		parentKind: onEnd.kind, parentID: onEnd.id,
		tuples: []graphstore.Tuple{{
			FromKind: graphstore.KindLens, FromID: id, Kind: graphstore.TupleHasKind,
			ToKind: graphstore.KindLensKind, ToID: kind.ID,
		}},
	}
	var verdict *string
	if input.Verdict != nil && !blankText(*input.Verdict) {
		verdict = input.Verdict
		lens.attributes = map[string]any{graphstore.AttributeVerdict: *verdict}
	}
	var triggeredBy *string
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
	}
	nodes := []reasoningNode{lens}
	for _, text := range input.Findings {
		nodes = append(nodes, newFinding(id, input.Visibility, text))
	}
	written, err := writeReasoningNodes(ctx, tx, reasoningWrite{
		reasoningCaller: caller, issue: key, at: at, nodes: nodes,
	})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	invoked := written[0]
	answer := lensInvoked{
		Address: lensAddress(id), ID: id, Kind: kind.ID, On: cmp.Or(on.Ref, input.On),
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
	// Visibility is author to keep the finding to its author; project, or nothing, gives the
	// finding its lens's visibility, and any other word is 400 INVALID_ATTRIBUTE_VALUE.
	Visibility string `json:"visibility" enum:"project,author"`
}

// addLensFinding is POST /api/v1/issues/{key}/nodes/{id}/findings: one finding added to the lens
// {id} names, a block node at a random block id under the lens, after every finding it holds. The
// finding carries its lens's visibility unless the request asks for author. {id} is the lens's
// store address (FindNodeUnderIssue): a node the caller may not see under the issue is 404
// NOT_FOUND, and a node the caller sees that is no lens is 400 INVALID_POINTER, since a finding
// stands under a lens alone and the refusal names nothing the caller could not read.
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
	lens, _, err := findEndUnderIssue(ctx, tx, caller.reader(), key, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if lens.Kind != graphstore.KindLens {
		writeError(w, codeInvalidPointer, http.StatusBadRequest, fmt.Sprintf(
			"a finding stands under a lens, and %s is %s", cmp.Or(lens.Ref, r.PathValue("id")), graphstore.WithArticle(lens.Kind)))
		return
	}
	visibility := lens.Visibility
	if input.Visibility == graphstore.VisibilityAuthor {
		visibility = graphstore.VisibilityAuthor
	}
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
	// Visibility is who the risk is for: project, the default, or author.
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
// one to a check of another project, naming the check. A lens the caller may not see under the
// issue, and a check the caller may not see wherever it stands, are 404 NOT_FOUND. Each stores
// nothing.
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
	at, err := transactionNow(ctx, tx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	lens, lensEnd, err := findEndUnderIssue(ctx, tx, reader, key, input.Lens)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	id := uuid.NewString()
	risk := reasoningNode{
		kind: graphstore.KindRisk, id: id, visibility: input.Visibility,
		parentKind: lensEnd.kind, parentID: lensEnd.id,
		attributes: map[string]any{
			graphstore.AttributeHeading:      input.Heading,
			graphstore.AttributeEarlyWarning: input.EarlyWarning,
			graphstore.AttributePrevention:   input.Prevention,
		},
	}
	preventedBy := []string{}
	var check model.GraphNode
	var checkEnd storedEnd
	if input.PreventedBy != nil {
		check, checkEnd, err = pointerEnd(ctx, tx, reader, key, graphstore.TuplePreventedBy, *input.PreventedBy)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		risk.tuples = append(risk.tuples, graphstore.Tuple{
			FromKind: graphstore.KindRisk, FromID: id, Kind: graphstore.TuplePreventedBy,
			ToKind: checkEnd.kind, ToID: checkEnd.id,
		})
		preventedBy = append(preventedBy, check.Ref)
	}
	written, err := writeReasoningNodes(ctx, tx, reasoningWrite{
		reasoningCaller: caller, issue: key, at: at, nodes: []reasoningNode{risk},
	})
	if err != nil {
		refusal, _ := farEndNamed(err, checkEnd, check)
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

// --- POST and GET /api/v1/issues/{key}/constraints ----------------------------------------------

// constraintInput is the body of a constraint under an issue: a standard with its reference, or a
// measurement with its value and where it was taken, and nothing of the other shape.
type constraintInput struct {
	Actor *model.Actor `json:"actor"`
	// Standard is the standard a requirement is held to, with its Reference.
	Standard  string `json:"standard"`
	Reference string `json:"reference"`
	// Measurement is what was measured, with its Value and where it was taken (TakenWhere).
	Measurement string `json:"measurement"`
	Value       string `json:"value"`
	TakenWhere  string `json:"taken_where"`
	// Visibility is who the constraint is for: project, the default, or author.
	Visibility string `json:"visibility" enum:"project,author"`
}

// constraintShape is the refusal of a constraint body that is neither of a constraint's shapes,
// empty for one that is: a standard with its reference, or a measurement with its value and where
// it was taken, with nothing of the other.
func (input constraintInput) constraintShape() string {
	standard := !blankText(input.Standard) || !blankText(input.Reference)
	measurement := !blankText(input.Measurement) || !blankText(input.Value) || !blankText(input.TakenWhere)
	const shapes = "a constraint is a standard with its reference (standard, reference), or a measurement with its value and where it was taken (measurement, value, taken_where)"
	switch {
	case standard && measurement:
		return shapes + ", and this one names fields of both"
	case standard && (blankText(input.Standard) || blankText(input.Reference)):
		return shapes + ", and this standard leaves out " + missingFields(map[string]string{
			"standard": input.Standard, "reference": input.Reference,
		})
	case measurement && (blankText(input.Measurement) || blankText(input.Value) || blankText(input.TakenWhere)):
		return shapes + ", and this measurement leaves out " + missingFields(map[string]string{
			"measurement": input.Measurement, "value": input.Value, "taken_where": input.TakenWhere,
		})
	case !standard && !measurement:
		return shapes + ", and this one names neither"
	}
	return ""
}

// missingFields names the fields of one shape a body leaves blank, in the order the shape names
// them.
func missingFields(fields map[string]string) string {
	var missing []string
	for _, name := range []string{"standard", "reference", "measurement", "value", "taken_where"} {
		if value, named := fields[name]; named && blankText(value) {
			missing = append(missing, name)
		}
	}
	return strings.Join(missing, " and ")
}

// constraintRead is one constraint of an issue: its address, `dispatch://constraint/<uuid>`, the
// fields of its shape (each null where it holds none), the person or session its authored_by
// tuple names (the session its body names for one written on the shared bearer, and none where
// the write named only a workload), when it was written, and its state and visibility.
type constraintRead struct {
	Address     string           `json:"address"`
	ID          string           `json:"id"`
	Standard    *string          `json:"standard"`
	Reference   *string          `json:"reference"`
	Measurement *string          `json:"measurement"`
	Value       *string          `json:"value"`
	TakenWhere  *string          `json:"taken_where"`
	Author      *reasoningAuthor `json:"author"`
	CreatedAt   time.Time        `json:"created_at"`
	State       string           `json:"state" enum:"live,gone,proposed,settled,overtaken"`
	Visibility  string           `json:"visibility" enum:"project,author"`
}

// issueConstraints is the constraints under one issue, in the order they were written there.
type issueConstraints struct {
	Issue       string           `json:"issue"`
	Constraints []constraintRead `json:"constraints"`
}

// constraintAttributes are a constraint's fields as its row carries them: each one the body
// names, by its attribute's name.
func (input constraintInput) constraintAttributes() map[string]any {
	attributes := map[string]any{}
	for name, value := range map[string]string{
		graphstore.AttributeStandard: input.Standard, graphstore.AttributeReference: input.Reference,
		graphstore.AttributeMeasurement: input.Measurement, graphstore.AttributeValue: input.Value,
		graphstore.AttributeTakenWhere: input.TakenWhere,
	} {
		if !blankText(value) {
			attributes[name] = value
		}
	}
	return attributes
}

// namedOrNull is text where it holds something, and null where it is blank.
func namedOrNull(text string) *string {
	if blankText(text) {
		return nil
	}
	return &text
}

// writeConstraint is POST /api/v1/issues/{key}/constraints: one constraint under the issue,
// placed there by its part_of tuple to the issue's node, with the fields of its shape as
// attributes of its row. A body that is neither shape is 400 CONSTRAINT_INPUT and stores nothing.
// A requirement points at it through POST .../nodes/{id}/pointers (constrained_by).
func (s *server) writeConstraint(w http.ResponseWriter, r *http.Request) {
	var input constraintInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	caller, ok := s.reasoningCallerOf(w, r, input.Actor)
	if !ok {
		return
	}
	if refusal := input.constraintShape(); refusal != "" {
		writeError(w, codeConstraintInput, http.StatusBadRequest, refusal)
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
	id := uuid.NewString()
	written, err := writeReasoningNodes(ctx, tx, reasoningWrite{
		reasoningCaller: caller, issue: key, at: at,
		nodes: []reasoningNode{{
			kind: graphstore.KindConstraint, id: id, visibility: input.Visibility,
			parentKind: graphstore.KindIssue, parentID: key, attributes: input.constraintAttributes(),
		}},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The issue's row is the first thing the write reads, so an issue nobody holds is this.
		s.writeHandlerError(w, errorf(http.StatusNotFound, "NOT_FOUND", "issue not found"))
		return
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	node := written[0]
	WriteJSON(w, http.StatusCreated, constraintRead{
		Address: refs.StoreRef(graphstore.KindConstraint, id), ID: id,
		Standard: namedOrNull(input.Standard), Reference: namedOrNull(input.Reference),
		Measurement: namedOrNull(input.Measurement), Value: namedOrNull(input.Value),
		TakenWhere: namedOrNull(input.TakenWhere), Author: writtenAuthor(node),
		CreatedAt: node.MadeAt, State: node.State, Visibility: node.Visibility,
	})
}

// issueConstraintsQuery reads the constraints under one issue ($1) for a reader ($3): each
// constraint whose current part_of tuple points at the issue's node, in the order those tuples
// were appended, that is not gone ($2) and that the reader may see through the predicate of its
// kind (refs.VisibleKindRow). Beside each ride its fields, its stored state and visibility, read
// off the row the read joins (refs.StoredRowOf), and the far end of its current authored_by tuple
// (refs.CurrentFarEndSQL).
var issueConstraintsQuery = `
	select held.id, held.made_at,
	       held.attributes->>'` + graphstore.AttributeStandard + `', held.attributes->>'` + graphstore.AttributeReference + `',
	       held.attributes->>'` + graphstore.AttributeMeasurement + `', held.attributes->>'` + graphstore.AttributeValue + `',
	       held.attributes->>'` + graphstore.AttributeTakenWhere + `',
	       ` + refs.StoredRowOf(`held`) + `,
	       author.to_kind, author.to_id
	from (
		` + graphstore.CurrentTupleRead(`t.from_id`, `t.from_id, t.seq, t.asserted`,
	`t.to_kind = '`+graphstore.KindIssue+`' and t.to_id = $1 and t.kind = '`+graphstore.TuplePartOf+`'
			and t.from_kind = '`+graphstore.KindConstraint+`' and t.to_kind <> '`+graphstore.KindValue+`'`) + `
	) placed
	join graph_nodes held on held.kind = '` + graphstore.KindConstraint + `' and held.id = placed.from_id
	left join lateral (
		` + refs.CurrentFarEndSQL(`newest.to_kind, newest.to_id`, `held.kind`, `held.id`, graphstore.TupleAuthoredBy) + `
	) author on true
	where placed.asserted and held.state <> $2 and ` + refs.VisibleKindRow(graphstore.KindConstraint, "held", "$3") + `
	order by placed.seq`

// listIssueConstraints is GET /api/v1/issues/{key}/constraints: the constraints under the issue
// the caller may see, in the order they were written there, each with its fields, its author, its
// state and its visibility. A constraint reserved for its author is in its author's list alone,
// and an issue nobody holds is 404 NOT_FOUND. An issue that holds a constraint exists, so only an
// empty list asks whether the issue does.
func (s *server) listIssueConstraints(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.actingReader(w, r)
	if !ok {
		return
	}
	ctx, key := r.Context(), r.PathValue("key")
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rows, err := pool.Query(ctx, issueConstraintsQuery, key, graphstore.StateGone, refs.VisibilityArg(reader))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	read := issueConstraints{Issue: key, Constraints: []constraintRead{}}
	for rows.Next() {
		var constraint constraintRead
		var stored []string
		var authorKind, authorID *string
		if err := rows.Scan(&constraint.ID, &constraint.CreatedAt, &constraint.Standard, &constraint.Reference,
			&constraint.Measurement, &constraint.Value, &constraint.TakenWhere, &stored, &authorKind, &authorID); err != nil {
			rows.Close()
			s.writeHandlerError(w, fmt.Errorf("scan a constraint of %s: %w", key, err))
			return
		}
		constraint.State, constraint.Visibility, err = refs.StoredStateOf(graphstore.KindConstraint+" "+constraint.ID, stored)
		if err != nil {
			rows.Close()
			s.writeHandlerError(w, err)
			return
		}
		constraint.Address = refs.StoreRef(graphstore.KindConstraint, constraint.ID)
		constraint.CreatedAt = constraint.CreatedAt.UTC()
		if authorKind != nil && authorID != nil {
			constraint.Author = &reasoningAuthor{Kind: *authorKind, ID: *authorID}
		}
		read.Constraints = append(read.Constraints, constraint)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, fmt.Errorf("read the constraints of %s: %w", key, err))
		return
	}
	if len(read.Constraints) == 0 {
		if err := s.requireIssueExists(ctx, key); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	WriteJSON(w, http.StatusOK, read)
}

// --- POST /api/v1/issues/{key}/nodes/{id}/pointers ----------------------------------------------

// pointerKinds are the tuple kinds the pointer route writes out of a node. A part_of tuple is a
// node's place, which no pointer route moves.
var pointerKinds = []string{
	graphstore.TupleHasKind, graphstore.TupleTriggeredBy, graphstore.TupleConstrainedBy, graphstore.TuplePreventedBy,
}

// pointerInput is the body of one pointer tuple's write out of the node {id} names.
type pointerInput struct {
	Actor *model.Actor `json:"actor"`
	// Kind is the tuple kind.
	Kind string `json:"kind" required:"true" enum:"has_kind,triggered_by,constrained_by,prevented_by"`
	// Action is what the write does: append a tuple at To, retract the one at To, or retarget
	// the one at From to To.
	Action string `json:"action" required:"true" enum:"append,retract,retarget"`
	// To is the address of the tuple's far end: the one appended or retracted, or the one a
	// retarget moves it to. A has_kind's far end is a lens_kind id.
	To string `json:"to" required:"true"`
	// From is, for a retarget alone, the address of the far end the node's tuple is at now.
	From *string `json:"from,omitempty"`
}

// reasoningEnd is one end of a pointer tuple as the route answers it: the node's address and
// kind, as the graph addresses it, and its state and visibility.
type reasoningEnd struct {
	Address    string `json:"address"`
	Kind       string `json:"kind"`
	State      string `json:"state" enum:"live,gone,proposed,settled,overtaken"`
	Visibility string `json:"visibility" enum:"project,author"`
}

// endOf is the answer's reading of node.
func endOf(node model.GraphNode) reasoningEnd {
	return reasoningEnd{Address: node.Ref, Kind: node.Kind, State: node.State, Visibility: node.Visibility}
}

// pointerWritten is what one pointer write did: the node the tuple goes out of, the tuple's kind,
// the action, the far end it is at now (appended or moved to) or was at (retracted), and the far
// end a retarget moved it from.
type pointerWritten struct {
	Node   reasoningEnd  `json:"node"`
	Kind   string        `json:"kind"`
	Action string        `json:"action" enum:"append,retract,retarget"`
	To     reasoningEnd  `json:"to"`
	From   *reasoningEnd `json:"from"`
}

// pointerEnd is the far end address names for a tuple of kind out of a node of the issue key,
// read for who, with the kind and the id the tuple names it by (farEndOf). A has_kind's far end
// is a lens_kind of the catalogue (lensKindNamed). A prevented_by's far end is a check of any
// issue of the project, as the pre-mortem conversion writes one (premortemChecks): it is read
// through refs.Node wherever it stands, an address the caller may not see answering
// FindNodeUnderIssue's 404 as one that does not exist, and the store holds the check to the
// risk's project (graphstore's pointerRules), so a check of another project that the caller may
// see is its 400 INVALID_POINTER. The risk route reads its check here too. Every other far end
// stands under the issue (FindNodeUnderIssue), but for a constrained_by at a constraint, which
// stands under the issue or, one of the catalogue, under none: it is read once through refs.Node
// and placed by its IssueKey, and a constraint of another issue is no far end of this one,
// answering FindNodeUnderIssue's 404 as one that does not exist.
func pointerEnd(ctx context.Context, q refs.Queryer, who model.Actor, key, kind, address string) (model.GraphNode, storedEnd, error) {
	if kind == graphstore.TupleHasKind {
		node, err := lensKindNamed(ctx, q, who, address)
		return node, storedEnd{kind: graphstore.KindLensKind, id: node.ID}, err
	}
	decoded, id, ok, err := resolvedNodeAddress(ctx, q, address)
	if err != nil {
		return model.GraphNode{}, storedEnd{}, err
	}
	constraint := kind == graphstore.TupleConstrainedBy && decoded == graphstore.KindConstraint
	if ok && (kind == graphstore.TuplePreventedBy || constraint) {
		node, found, err := refs.Node(ctx, q, who, decoded, id, true)
		if err != nil {
			return model.GraphNode{}, storedEnd{}, err
		}
		if !found || (constraint && node.IssueKey != nil && *node.IssueKey != key) {
			return model.GraphNode{}, storedEnd{}, nodeNotFound(key, address)
		}
		return node, farEndOf(node, address), nil
	}
	return findEndUnderIssue(ctx, q, who, key, address)
}

// writePointer is POST /api/v1/issues/{key}/nodes/{id}/pointers: one has_kind, triggered_by,
// constrained_by or prevented_by tuple out of the node {id} names (FindNodeUnderIssue), appended,
// retracted or retargeted by the store's rules (writeReasoningPointer). {id} is the node's store
// address. A prevented_by's far ends, to and a retarget's from, are checks of any issue of the
// project (pointerEnd). A node or a far end the caller may not see is 404 NOT_FOUND; a far end of
// a type the tuple's rule does not allow, a check of another project than the risk's, a lens
// triggered by itself and a lens left with no kind are the store's 400 INVALID_POINTER, the check
// named by its address; a second has_kind, and a retarget of a pre-mortem's away from pre-mortem,
// are its 409 POINTER_ALREADY_ASSERTED, one body whatever hangs under the lens. A retarget touches
// no part_of tuple, and this route writes none: a node moves to another parent through its parent
// route.
func (s *server) writePointer(w http.ResponseWriter, r *http.Request) {
	var input pointerInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	caller, ok := s.reasoningCallerOf(w, r, input.Actor)
	if !ok {
		return
	}
	switch {
	case input.Kind == graphstore.TuplePartOf:
		writeError(w, codePointerInput, http.StatusBadRequest,
			"this route writes no part_of tuple: a part_of tuple is a node's place under its parent, which moves through its parent route")
		return
	case !slices.Contains(pointerKinds, input.Kind):
		writeError(w, codePointerInput, http.StatusBadRequest, fmt.Sprintf(
			"kind %q is no pointer this route writes: one of %s", input.Kind, strings.Join(pointerKinds, ", ")))
		return
	case input.Action != pointerAppend && input.Action != pointerRetract && input.Action != pointerRetarget:
		writeError(w, codePointerInput, http.StatusBadRequest, fmt.Sprintf(
			"action %q is none of %s, %s, %s", input.Action, pointerAppend, pointerRetract, pointerRetarget))
		return
	case blankText(input.To):
		writeError(w, codePointerInput, http.StatusBadRequest, "to is required: the address of the tuple's far end")
		return
	case input.Action == pointerRetarget && (input.From == nil || blankText(*input.From)):
		writeError(w, codePointerInput, http.StatusBadRequest, "a retarget names from, the far end the node's tuple is at now, beside to, the far end it moves to")
		return
	case input.Action != pointerRetarget && input.From != nil:
		writeError(w, codePointerInput, http.StatusBadRequest, "from goes with a retarget alone: an "+input.Action+" names its far end in to")
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
	node, nodeEnd, err := findEndUnderIssue(ctx, tx, reader, key, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	to, toEnd, err := pointerEnd(ctx, tx, reader, key, input.Kind, input.To)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	write := reasoningPointer{issue: key, action: input.Action, pointer: graphstore.Tuple{
		FromKind: refs.StoredKindOf(nodeEnd.kind), FromID: nodeEnd.id, Kind: input.Kind, ToKind: toEnd.kind, ToID: toEnd.id,
		At: at, Who: caller.recorded, SourceEvent: graphstore.NoEvent, Asserted: true,
	}}
	answer := pointerWritten{Node: endOf(node), Kind: input.Kind, Action: input.Action, To: endOf(to)}
	if input.Action == pointerRetarget {
		from, fromEnd, err := pointerEnd(ctx, tx, reader, key, input.Kind, *input.From)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		write.pointer.ToKind, write.pointer.ToID = fromEnd.kind, fromEnd.id
		write.toKind, write.toID = toEnd.kind, toEnd.id
		answered := endOf(from)
		answer.From = &answered
	}
	if err := writeReasoningPointer(ctx, tx, write); err != nil {
		refusal, _ := farEndNamed(err, toEnd, to)
		s.writeHandlerError(w, refusal)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}
