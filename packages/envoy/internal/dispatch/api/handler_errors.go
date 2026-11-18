package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// How a handler's error becomes a response: one function, writeHandlerError, that every route's
// error goes through, so one failure reads the same whichever route met it.

// The codes writeHandlerError answers a document it cannot read or serve and an unclassified
// failure with, which a comment's or ask's single read also carries as anchor_block_error
// (anchorBlock).
const (
	codeDocSchema             = "DOC_SCHEMA"
	codeDocNewerContent       = "DOC_NEWER_CONTENT"
	codeDocServiceUnavailable = "DOC_SERVICE_UNAVAILABLE"
	codeDocumentUnloadable    = "DOCUMENT_UNLOADABLE"
	codeInternal              = "INTERNAL"
)

// codeNegativeControlRequired is the 400 recordCheckResult answers a pass of a before_merge check
// whose input carries an empty negative_control, or one of whitespace alone: a passing
// before_merge result states the negative control that would have caught its failure.
const codeNegativeControlRequired = "NEGATIVE_CONTROL_REQUIRED"

// codeAnsweredAskKept refuses a document write by a credential with no person scope that would
// drop or retype the block of an answered ask (docs.ErrAnsweredAskKept).
const codeAnsweredAskKept = "ANSWERED_ASK_KEPT"

// codeDecisionsAreNodes refuses an upload or a create of a decisions document on an issue
// (decisionsAreNodes): a decision is a `decision` block of the spec. It stays for good.
const codeDecisionsAreNodes = "DECISIONS_ARE_NODES"

// documentErrorCode names an error a document operation returns when it could not read or serve
// the document, and is empty for any other error. writeHandlerError and anchorBlock both name a
// document error by it, and both in one order, so the two cannot name one error differently.
//
// A room or store that could not serve the document is DOC_SERVICE_UNAVAILABLE whatever failed it,
// so both take that code before any branch that reads the error's cause. A failed room carries the
// error another operation failed it with - settlement's schema refusal, a settlement that failed
// three times (its warm-up refused because the issue had closed, among others), a writer's commit
// that failed or its client cancelled, a store write or load that failed, a history that did not
// decode - and that error says nothing of this request: the caller retries once the room is
// evicted, and its retry meets the document itself.
//
// A stored history this request itself could not decode is DOCUMENT_UNLOADABLE, the state a
// rebuild repairs. A tree holding an element this build does not know is DOC_NEWER_CONTENT: a
// newer build wrote it while two builds serve one database, so the caller tries again later,
// perhaps on that build, and no repair applies. A tree outside the schema is DOC_SCHEMA, which both
// take only after the branches that name the caller's own input or a block the document does not
// hold, so a refusal whose reason the renderer gave (an ask block that cannot render) stays that
// refusal.
func documentErrorCode(err error) string {
	switch {
	case errors.Is(err, docs.ErrServiceUnavailable):
		return codeDocServiceUnavailable
	case errors.Is(err, docs.ErrDocumentUnloadable):
		return codeDocumentUnloadable
	case errors.Is(err, docs.ErrDocNewerContent):
		return codeDocNewerContent
	case errors.Is(err, docs.ErrDocSchema):
		return codeDocSchema
	}
	return ""
}

// editPreconditionCurrent is the `current` object of editPreconditionError: the document token
// and the block tokens the write found, which a caller re-reads its precondition from.
type editPreconditionCurrent struct {
	Blocks   []model.EditBlockPrecondition `json:"blocks"`
	Document string                        `json:"document"`
}

// editPreconditionError is the 409 PRECONDITION_FAILED body every document write answers a
// precondition the document no longer meets.
type editPreconditionError struct {
	Code       string                           `json:"code"`
	Current    editPreconditionCurrent          `json:"current"`
	Error      string                           `json:"error"`
	Mismatches []model.EditPreconditionMismatch `json:"mismatches"`
}

// editLostError is the 409 EDIT_LOST_TO_CONCURRENT_CHANGE body a document write answers when a
// concurrent browser change removed its text before it could be versioned. A write refused for
// an accepted comment names that comment in Suggestion and no operation index; any other names
// the operations in LostOps. Both are pointers, so the body carries exactly the one that
// applies, as the two map literals behind it did.
type editLostError struct {
	Code         string        `json:"code"`
	Error        string        `json:"error"`
	LostOps      *[]int        `json:"lost_ops,omitempty"`
	Participants []model.Actor `json:"participants"`
	Suggestion   *string       `json:"suggestion,omitempty"`
}

func (s *server) writeHandlerError(w http.ResponseWriter, err error) {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		if apiErr.retryAfter > 0 {
			// Before the body: writeError writes the status and the headers go with it.
			w.Header().Set("Retry-After", strconv.Itoa(apiErr.retryAfter))
		}
		if apiErr.scope != "" {
			w.Header().Set("WWW-Authenticate", insufficientScopeChallenge(apiErr.scope))
		}
		writeError(w, apiErr.code, apiErr.status, apiErr.message)
		// A 500 the handler named is a server fault like any other, so the operator's log shows it.
		if apiErr.status == http.StatusInternalServerError {
			slog.Error("dispatch: API handler failed", "code", apiErr.code, "error", err)
		}
		return
	}
	documentCode := documentErrorCode(err)
	switch documentCode {
	case codeDocServiceUnavailable:
		writeError(w, documentCode, http.StatusServiceUnavailable, docs.ErrServiceUnavailable.Error())
		return
	case codeDocumentUnloadable:
		writeError(w, documentCode, http.StatusConflict, docs.ErrDocumentUnloadable.Error())
		slog.Warn("dispatch: API document history cannot load", "error", err)
		return
	case codeDocNewerContent:
		// The message is the document's own (docs.NewerContentError), the same on every route.
		var newer *docs.NewerContentError
		message := docs.ErrDocNewerContent.Error()
		var elements []string
		if errors.As(err, &newer) {
			message = newer.Error()
			elements = newer.Elements
		}
		writeError(w, documentCode, http.StatusConflict, message)
		// The operator's line names the elements the document holds and nothing taken from the
		// error: its chain carries whatever the request wrapped it in, the caller's own identity
		// among it, which no log of ours writes (CodeQL go/clear-text-logging).
		slog.Warn("dispatch: API document holds an element this build cannot read", "elements", elements)
		return
	}
	// The edit route's own ambiguity error names the operation and the quote; the bare pmdoc one
	// still serves every other caller of FindQuote.
	var ambiguousQuote *docs.ErrAnchorAmbiguous
	if errors.As(err, &ambiguousQuote) {
		writeAmbiguousTarget(w, ambiguousQuote.Error(), ambiguousQuote.Candidates)
		return
	}
	var ambiguous *pmdoc.ErrTargetAmbiguous
	if errors.As(err, &ambiguous) {
		writeAmbiguousTarget(w, ambiguous.Error(), ambiguous.Candidates)
		return
	}

	var preconditionFailed *docs.ErrPreconditionFailed
	if errors.As(err, &preconditionFailed) {
		currentBlocks := make([]model.EditBlockPrecondition, 0)
		for _, mismatch := range preconditionFailed.Mismatches {
			if mismatch.Scope == "block" && mismatch.Current != nil {
				currentBlocks = append(currentBlocks, model.EditBlockPrecondition{
					ID: mismatch.BlockID, Token: *mismatch.Current,
				})
			}
		}
		WriteJSON(w, http.StatusConflict, editPreconditionError{
			Code: "PRECONDITION_FAILED",
			Current: editPreconditionCurrent{
				Blocks:   currentBlocks,
				Document: preconditionFailed.CurrentDocument,
			},
			Error:      preconditionFailed.Error(),
			Mismatches: preconditionFailed.Mismatches,
		})
		return
	}
	// A write whose text a concurrent browser change removed before the write could be versioned
	// (LEGION-269). Nothing was written, so this reads like PRECONDITION_FAILED: re-read the
	// document and decide again.
	var lostEdit *docs.ErrEditLost
	if errors.As(err, &lostEdit) {
		body := editLostError{
			Code:         "EDIT_LOST_TO_CONCURRENT_CHANGE",
			Error:        lostEdit.Error(),
			Participants: participantsOrEmpty(lostEdit.Participants),
		}
		if lostEdit.Suggestion != "" {
			body.Suggestion = &lostEdit.Suggestion
		} else {
			body.LostOps = &lostEdit.Ops
		}
		WriteJSON(w, http.StatusConflict, body)
		return
	}
	var invalidPrecondition *docs.ErrInvalidPrecondition
	if errors.As(err, &invalidPrecondition) {
		writeError(w, "INVALID_PRECONDITION", http.StatusBadRequest, invalidPrecondition.Error())
		return
	}
	if errors.Is(err, docs.ErrPreconditionBusy) {
		writeError(w, "EDIT_QUEUE_FULL", http.StatusTooManyRequests, docs.ErrPreconditionBusy.Error())
		return
	}
	var answeredAskKept *docs.ErrAnsweredAskKept
	if errors.As(err, &answeredAskKept) {
		writeError(w, codeAnsweredAskKept, http.StatusConflict, answeredAskKept.Error())
		return
	}
	var invalidAskBlock *docs.ErrInvalidAskBlock
	if errors.As(err, &invalidAskBlock) {
		writeError(w, "INVALID_ASK_BLOCK", http.StatusBadRequest, invalidAskBlock.Error())
		return
	}
	var invalidCICheck *docs.ErrInvalidCICheck
	if errors.As(err, &invalidCICheck) {
		writeError(w, "INVALID_CI_CHECK", http.StatusBadRequest, invalidCICheck.Error())
		return
	}
	// A write that states a node's state or visibility at a value that attribute does not take.
	// The message is the refusal's own, which names the attribute, the value and the values it
	// takes, whatever the route wrapped it in, as DOC_NEWER_CONTENT's is.
	if message, refused := refusalWords[*graphstore.InvalidAttributeValueError](err, graphstore.ErrInvalidAttributeValue); refused {
		writeError(w, "INVALID_ATTRIBUTE_VALUE", http.StatusBadRequest, message)
		return
	}
	// A node reserved for its author asked for on a credential that proves none
	// (graphstore.VisibilityFor). The going POST refuses such an asker before it reaches here,
	// with its own GOING_ASKER_NAMES_NOBODY; both decide by graphstore.ProvesAuthor.
	if errors.Is(err, graphstore.ErrAuthorNamesNobody) {
		writeError(w, "AUTHOR_NAMES_NOBODY", http.StatusForbidden, graphstore.ErrAuthorNamesNobody.Error())
		return
	}
	// A node reserved for its author whose kind takes no such node (graphstore's AdmitAuthorOnly):
	// the refusal's own words name the kind, so a comment asked for `author` reads as one.
	if message, refused := refusalWords[*graphstore.VisibilityNotAdmittedError](err, graphstore.ErrVisibilityNotAdmitted); refused {
		writeError(w, "VISIBILITY_NOT_ADMITTED", http.StatusBadRequest, message)
		return
	}
	// A node of a type the store holds no supertag row of (graphstore's checkSupertags): the
	// refusal's own words name each type, and nothing of the write was stored.
	if message, refused := refusalWords[*graphstore.SupertagNotHeldError](err, graphstore.ErrSupertagNotHeld); refused {
		writeError(w, "SUPERTAG_NOT_HELD", http.StatusConflict, message)
		return
	}
	// A pointer tuple at a node its type rule does not allow (graphstore's pointerRules): the
	// refusal's own words name the tuple kind, the far end's kind and the kinds the rule allows.
	if message, refused := refusalWords[*graphstore.InvalidPointerError](err, graphstore.ErrInvalidPointer); refused {
		writeError(w, codeInvalidPointer, http.StatusBadRequest, message)
		return
	}
	// A second has_kind of a lens, or a pre-mortem's retargeted away from pre-mortem: one body
	// for each, whatever hangs under the lens.
	if message, refused := refusalWords[*graphstore.PointerAlreadyAssertedError](err, graphstore.ErrPointerAlreadyAsserted); refused {
		writeError(w, "POINTER_ALREADY_ASSERTED", http.StatusConflict, message)
		return
	}
	// A retarget of a pointer the node does not hold answers as a pointer that does not exist.
	if errors.Is(err, graphstore.ErrPointerNotHeld) {
		writeError(w, "NOT_FOUND", http.StatusNotFound, graphstore.ErrPointerNotHeld.Error())
		return
	}
	var unrepresentableAsk *docs.ErrAskBlockUnrepresentable
	if errors.As(err, &unrepresentableAsk) {
		writeError(w, "ASK_BLOCK_TEXT", http.StatusBadRequest, unrepresentableAsk.Error())
		return
	}
	var invalidOp *docs.ErrInvalidOp
	if errors.As(err, &invalidOp) {
		writeError(w, "INVALID_OP", http.StatusBadRequest, invalidOp.Error())
		return
	}
	if errors.Is(err, pmdoc.ErrTargetSpansBlocks) {
		writeError(w, "TARGET_SPANS_BLOCKS", http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, pmdoc.ErrTableWidth) {
		writeError(w, "TABLE_WIDTH", http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, docs.ErrAnchorMissing) {
		writeError(w, "ANCHOR_MISSING", http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, docs.ErrAnchorOrphaned) {
		writeError(w, "ANCHOR_ORPHANED", http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, pmdoc.ErrTargetNotFound) {
		writeError(w, "TARGET_NOT_FOUND", http.StatusNotFound, err.Error())
		return
	}
	// A block pointer refused on any route but the edit route (which answers INVALID_OP) is the
	// caller's markdown to fix, as a schema refusal of it is.
	if errors.Is(err, docs.ErrInvalidMarkdown) || errors.Is(err, pmdoc.ErrBlockPointer) {
		writeError(w, "INVALID_MARKDOWN", http.StatusBadRequest, err.Error())
		return
	}
	if docs.IsTooLarge(err) {
		writeError(w, "CAP_EXCEEDED", http.StatusRequestEntityTooLarge, err.Error())
		slog.Warn("dispatch: API refused a document too large to store", "error", err)
		return
	}
	// A tree outside the schema that the document holds answers 409 with its one message, the same
	// on every route whatever the route wrapped it in (docs.OutsideSchemaError). Any other schema
	// refusal is a tree an operation produced, which is the server's fault.
	if documentCode == codeDocSchema {
		var outside *docs.OutsideSchemaError
		if errors.As(err, &outside) {
			writeError(w, documentCode, http.StatusConflict, outside.Error())
			slog.Warn("dispatch: API document outside Proof schema", "error", err)
			return
		}
		writeError(w, documentCode, http.StatusInternalServerError, err.Error())
		slog.Error("dispatch: API document outside Proof schema", "error", err)
		return
	}
	if errors.Is(err, docs.ErrIssueClosed) {
		writeError(w, "ISSUE_CLOSED", http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, "NOT_FOUND", http.StatusNotFound, "not found")
		return
	}
	writeError(w, codeInternal, http.StatusInternalServerError, "internal server error")
	slog.Error("dispatch: API handler failed", "error", err)
}

// refusalWords is whether err is the store's refusal sentinel and, where it is, the words a
// caller reads: those of the typed refusal T err carries, which name what was refused, or the
// sentinel's own where it carries none, whatever the route wrapped the refusal in.
func refusalWords[T error](err, sentinel error) (string, bool) {
	if !errors.Is(err, sentinel) {
		return "", false
	}
	var refusal T
	if errors.As(err, &refusal) {
		return refusal.Error(), true
	}
	return sentinel.Error(), true
}

// ambiguousTargetError is the 409 TARGET_AMBIGUOUS body a write answers a quote or heading
// anchor that resolved in several places.
type ambiguousTargetError struct {
	Candidates []pmdoc.Candidate `json:"candidates"`
	Code       string            `json:"code"`
	Error      string            `json:"error"`
}

// writeAmbiguousTarget serves a quote that resolved in several places: the caller picks one by
// the candidates' positions, or narrows the quote.
func writeAmbiguousTarget(w http.ResponseWriter, message string, candidates []pmdoc.Candidate) {
	WriteJSON(w, http.StatusConflict, ambiguousTargetError{
		Candidates: candidates,
		Code:       "TARGET_AMBIGUOUS",
		Error:      message,
	})
}
