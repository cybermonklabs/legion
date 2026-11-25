package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// --- POST /api/v1/issues/{key}/nodes/{id}/pointers ----------------------------------------------

// pointerKinds are the tuple kinds the pointer route writes out of a node, the store's
// (graphstore.PointerKinds), read once. A part_of tuple is a node's place, which no pointer route
// moves. pointerInput's Kind enum spells the same list, which
// TestThePointerRouteKindEnumIsTheStoresPointerKinds holds it to.
var pointerKinds = graphstore.PointerKinds()

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
// a type the tuple's rule does not allow, a check of another project than the risk's or of a
// retired document, a lens triggered by itself or by a lens not invoked before it, and a lens left
// with no kind are the store's 400 INVALID_POINTER, the check, and both lenses, named by address
// (refusalNamed); a second has_kind, and a retarget of a pre-mortem's away from pre-mortem, are its
// 409 POINTER_ALREADY_ASSERTED, one body whatever hangs under the lens. A retarget touches no
// part_of tuple, and this route writes none: a node moves to another parent through its parent
// route. For an append or a retarget of a prevented_by it takes the issue's owner lock and the
// owner lock of the document of the check at to before it reads a node (its race with a
// retirement or an index pass: lockReasoningOwners); any other write takes the issue's owner lock
// at the write (writeReasoningPointer). A pointer out of a requirement, criterion or check node of
// an issue Legion runs - a requirement's constrained_by - is a relation out of one: under the
// issue's owner lock, a writer who is no person and no architect of the issue or of an issue above
// it is 403 DOCUMENT_WRITER_NOT_ARCHITECT (refuseSpecWriterOfNode), and nothing is stored.
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
	// An append and a retarget assert a prevented_by at to; a retraction asserts nothing.
	if input.Kind == graphstore.TuplePreventedBy && input.Action != pointerRetract {
		if err := lockReasoningOwners(ctx, tx, key, []string{input.To}); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	at, err := transactionNow(ctx, tx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	node, nodeEnd, err := findWrittenNode(ctx, tx, reader, key, r.PathValue("id"))
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
	// A triggered_by's lens and a constrained_by's constraint stand under the issue, or a
	// constraint of the catalogue under none, and a write that takes one of the issue's gone holds
	// the issue's owner lock, so the write reads to again under that lock.
	if input.Kind == graphstore.TupleTriggeredBy || input.Kind == graphstore.TupleConstrainedBy {
		write.farEnds = []namedEnd{{storedEnd: toEnd, address: input.To}}
	}
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
	// The check of who writes a requirement, criterion or check node, under the issue's owner lock,
	// which writeReasoningPointer then finds held; it passes a person with no read, so a person's
	// write takes the lock at the write, as it always has.
	if writer := caller.specWriter(); !writer.person {
		if _, err := lockIssueOwner(ctx, tx, key); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := refuseSpecWriterOfNode(ctx, tx, writer, key, nodeEnd.stored(), node.Ref); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if err := writeReasoningPointer(ctx, tx, write); err != nil {
		refusal, _ := refusalNamed(err, namedEnd{storedEnd: nodeEnd.stored(), address: node.Ref},
			namedEnd{storedEnd: toEnd, address: to.Ref})
		s.writeHandlerError(w, refusal)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}
