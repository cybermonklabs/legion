package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// How a handler's error becomes a response: one function, writeHandlerError, that every route's
// error goes through, so one failure reads the same whichever route met it.

// documentErrorCode names an error a document operation returns when it could not read or serve
// the document, and reports false for any other error. writeHandlerError and anchorBlock both
// name a document error by it, and both in one order, so the two cannot name one error
// differently.
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
// refusal; writeHandlerError answers it DocSchema or DocSchemaInternal by whose fault it is.
func documentErrorCode(err error) (apicode.Code, bool) {
	switch {
	case errors.Is(err, docs.ErrServiceUnavailable):
		return apicode.DocServiceUnavailable, true
	case errors.Is(err, docs.ErrDocumentUnloadable):
		return apicode.DocumentUnloadable, true
	case errors.Is(err, docs.ErrDocNewerContent):
		return apicode.DocNewerContent, true
	case errors.Is(err, docs.ErrDocSchema):
		return apicode.DocSchema, true
	}
	return apicode.InternalError, false
}

// editPreconditionCurrent is the `current` object of editPreconditionError: the document token
// and the block tokens the write found, which a caller re-reads its precondition from, and the
// number of the version the document stands at, which a refusal whose caller read it beside the
// write carries (an upload). An edit refuses without reading the version rows and names none,
// which is what the omitted field says.
type editPreconditionCurrent struct {
	Blocks   []model.EditBlockPrecondition `json:"blocks"`
	Document string                        `json:"document"`
	Version  *int                          `json:"version,omitempty"`
}

// editPreconditionError is the 409 PRECONDITION_FAILED body every document write answers a
// precondition the document no longer meets.
type editPreconditionError struct {
	Code       string                           `json:"code"`
	Current    editPreconditionCurrent          `json:"current"`
	Error      string                           `json:"error"`
	Mismatches []model.EditPreconditionMismatch `json:"mismatches"`
}

// editPreconditionBody reads that body off the refusal, with the name and status r's version
// answers PRECONDITION_FAILED with: the live document token, the block tokens the write found,
// and the mismatches that named them. The version is the caller's to fill in, since the write
// reads no version rows: an edit's refusal names none, and an upload's names the number it read
// beside its write and adds the sentence saying where a current token is printed
// (writeUploadDocumentError).
func editPreconditionBody(r *http.Request, failed *docs.ErrPreconditionFailed) (editPreconditionError, int) {
	currentBlocks := make([]model.EditBlockPrecondition, 0)
	for _, mismatch := range failed.Mismatches {
		if mismatch.Scope == "block" && mismatch.Current != nil {
			currentBlocks = append(currentBlocks, model.EditBlockPrecondition{
				ID: mismatch.BlockID, Token: *mismatch.Current,
			})
		}
	}
	code, status := refusalPair(r, apicode.PreconditionFailed)
	return editPreconditionError{
		Code:       code,
		Current:    editPreconditionCurrent{Blocks: currentBlocks, Document: failed.CurrentDocument},
		Error:      failed.Error(),
		Mismatches: failed.Mismatches,
	}, status
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

func (s *server) writeHandlerError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		if apiErr.retryAfter > 0 {
			// Before the body: writeError writes the status and the headers go with it.
			w.Header().Set("Retry-After", strconv.Itoa(apiErr.retryAfter))
		}
		if apiErr.scope != "" {
			w.Header().Set("WWW-Authenticate", insufficientScopeChallenge(apiErr.scope))
		}
		writeError(w, r, apiErr.code, apiErr.message)
		// A 500 the handler named is a server fault like any other, so the operator's log shows it.
		if name, status, _ := codePair(requestAPIVersion(r), apiErr.code); status == http.StatusInternalServerError {
			slog.Error("dispatch: API handler failed", "code", name, "error", err)
		}
		return
	}
	documentCode, documentError := documentErrorCode(err)
	switch {
	case documentError && documentCode == apicode.DocServiceUnavailable:
		writeError(w, r, documentCode, docs.ErrServiceUnavailable.Error())
		return
	case documentError && documentCode == apicode.DocumentUnloadable:
		writeError(w, r, documentCode, docs.ErrDocumentUnloadable.Error())
		slog.Warn("dispatch: API document history cannot load", "error", err)
		return
	case documentError && documentCode == apicode.DocNewerContent:
		// The message is the document's own (docs.NewerContentError), the same on every route.
		var newer *docs.NewerContentError
		message := docs.ErrDocNewerContent.Error()
		var elements []string
		if errors.As(err, &newer) {
			message = newer.Error()
			elements = newer.Elements
		}
		writeError(w, r, documentCode, message)
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
		writeAmbiguousTarget(w, r, ambiguousQuote.Error(), ambiguousQuote.Candidates)
		return
	}
	var ambiguous *pmdoc.ErrTargetAmbiguous
	if errors.As(err, &ambiguous) {
		writeAmbiguousTarget(w, r, ambiguous.Error(), ambiguous.Candidates)
		return
	}

	var preconditionFailed *docs.ErrPreconditionFailed
	if errors.As(err, &preconditionFailed) {
		body, status := editPreconditionBody(r, preconditionFailed)
		WriteJSON(w, status, body)
		return
	}
	// A write whose text a concurrent browser change removed before the write could be versioned
	// (LEGION-269). Nothing was written, so this reads like PRECONDITION_FAILED: re-read the
	// document and decide again.
	var lostEdit *docs.ErrEditLost
	if errors.As(err, &lostEdit) {
		code, status := refusalPair(r, apicode.EditLostToConcurrentChange)
		body := editLostError{
			Code:         code,
			Error:        lostEdit.Error(),
			Participants: participantsOrEmpty(lostEdit.Participants),
		}
		if lostEdit.Suggestion != "" {
			body.Suggestion = &lostEdit.Suggestion
		} else {
			body.LostOps = &lostEdit.Ops
		}
		WriteJSON(w, status, body)
		return
	}
	var invalidPrecondition *docs.ErrInvalidPrecondition
	if errors.As(err, &invalidPrecondition) {
		writeError(w, r, apicode.InvalidPrecondition, invalidPrecondition.Error())
		return
	}
	if errors.Is(err, docs.ErrPreconditionBusy) {
		writeError(w, r, apicode.EditQueueFull, docs.ErrPreconditionBusy.Error())
		return
	}
	var invalidAskBlock *docs.ErrInvalidAskBlock
	if errors.As(err, &invalidAskBlock) {
		writeError(w, r, apicode.InvalidAskBlock, invalidAskBlock.Error())
		return
	}
	var invalidCICheck *docs.ErrInvalidCICheck
	if errors.As(err, &invalidCICheck) {
		writeError(w, r, apicode.InvalidCICheck, invalidCICheck.Error())
		return
	}
	var unrepresentableAsk *docs.ErrAskBlockUnrepresentable
	if errors.As(err, &unrepresentableAsk) {
		writeError(w, r, apicode.AskBlockText, unrepresentableAsk.Error())
		return
	}
	var invalidOp *docs.ErrInvalidOp
	if errors.As(err, &invalidOp) {
		writeError(w, r, apicode.InvalidOp, invalidOp.Error())
		return
	}
	if errors.Is(err, pmdoc.ErrTargetSpansBlocks) {
		writeError(w, r, apicode.TargetSpansBlocks, err.Error())
		return
	}
	if errors.Is(err, pmdoc.ErrTableWidth) {
		writeError(w, r, apicode.TableWidth, err.Error())
		return
	}
	if errors.Is(err, docs.ErrAnchorMissing) {
		writeError(w, r, apicode.AnchorMissing, err.Error())
		return
	}
	if errors.Is(err, docs.ErrAnchorOrphaned) {
		writeError(w, r, apicode.AnchorOrphaned, err.Error())
		return
	}
	if errors.Is(err, pmdoc.ErrTargetNotFound) {
		writeError(w, r, apicode.TargetNotFound, err.Error())
		return
	}
	// A block pointer refused on any route but the edit route (which answers INVALID_OP) is the
	// caller's markdown to fix, as a schema refusal of it is.
	if errors.Is(err, docs.ErrInvalidMarkdown) || errors.Is(err, pmdoc.ErrBlockPointer) {
		writeError(w, r, apicode.InvalidMarkdown, err.Error())
		return
	}
	if docs.IsTooLarge(err) {
		writeError(w, r, apicode.CapExceededTooLarge, err.Error())
		slog.Warn("dispatch: API refused a document too large to store", "error", err)
		return
	}
	// A tree outside the schema that the document holds answers 409 with its one message, the same
	// on every route whatever the route wrapped it in (docs.OutsideSchemaError). Any other schema
	// refusal is a tree an operation produced, which is the server's fault.
	if documentError && documentCode == apicode.DocSchema {
		var outside *docs.OutsideSchemaError
		if errors.As(err, &outside) {
			writeError(w, r, apicode.DocSchema, outside.Error())
			slog.Warn("dispatch: API document outside Proof schema", "error", err)
			return
		}
		writeError(w, r, apicode.DocSchemaInternal, err.Error())
		slog.Error("dispatch: API document outside Proof schema", "error", err)
		return
	}
	if errors.Is(err, docs.ErrIssueClosed) {
		writeError(w, r, apicode.IssueClosed, err.Error())
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, r, apicode.NotFound, "not found")
		return
	}
	writeError(w, r, apicode.InternalError, "internal server error")
	slog.Error("dispatch: API handler failed", "error", err)
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
func writeAmbiguousTarget(w http.ResponseWriter, r *http.Request, message string, candidates []pmdoc.Candidate) {
	code, status := refusalPair(r, apicode.TargetAmbiguous)
	WriteJSON(w, status, ambiguousTargetError{
		Candidates: candidates,
		Code:       code,
		Error:      message,
	})
}
