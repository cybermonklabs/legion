package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// How a handler's error becomes a response: one decision, handlerRefusalOf, that every route's
// error goes through as writeHandlerError writes it, so one failure reads the same whichever route
// met it, and a held text's post reads the status its route would answer from the same decision.

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

// editPreconditionBody reads that body off the refusal: the live document token, the block
// tokens the write found, and the mismatches that named them. The version is the caller's to
// fill in, since the write reads no version rows: an edit's refusal names none, and an upload's
// names the number it read beside its write and adds the sentence saying where a current token
// is printed (writeUploadDocumentError).
func editPreconditionBody(failed *docs.ErrPreconditionFailed) editPreconditionError {
	currentBlocks := make([]model.EditBlockPrecondition, 0)
	for _, mismatch := range failed.Mismatches {
		if mismatch.Scope == "block" && mismatch.Current != nil {
			currentBlocks = append(currentBlocks, model.EditBlockPrecondition{
				ID: mismatch.BlockID, Token: *mismatch.Current,
			})
		}
	}
	return editPreconditionError{
		Code:       "PRECONDITION_FAILED",
		Current:    editPreconditionCurrent{Blocks: currentBlocks, Document: failed.CurrentDocument},
		Error:      failed.Error(),
		Mismatches: failed.Mismatches,
	}
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

// handlerRefusal is how a handler's error is answered: its status, the code and the message it is
// refused with, the body that stands in for errorBody where the refusal has a richer one (nil for
// errorBody), the headers it carries, and the operator's log line it is written with (nil for
// none). handlerRefusalOf decides it and writeHandlerError writes it, so a caller that needs the
// status a route would answer an error with, without answering it (a held text's post, postHeld),
// reads the decision every route's answer is written from.
type handlerRefusal struct {
	status        int
	code, message string
	body          any
	header        http.Header
	log           func()
}

func (s *server) writeHandlerError(w http.ResponseWriter, err error) {
	handlerRefusalOf(err).write(w)
}

// write answers the refusal on w: its headers first, since writeError and WriteJSON write the
// status with the body, then the operator's line.
func (refusal handlerRefusal) write(w http.ResponseWriter) {
	for key, values := range refusal.header {
		w.Header()[key] = values
	}
	if refusal.body != nil {
		WriteJSON(w, refusal.status, refusal.body)
	} else {
		writeError(w, refusal.code, refusal.status, refusal.message)
	}
	if refusal.log != nil {
		refusal.log()
	}
}

// apiError is the refusal as a handler returns one: its status, its code and its message.
func (refusal handlerRefusal) apiError() *apiError {
	return &apiError{status: refusal.status, code: refusal.code, message: refusal.message}
}

// handlerRefusalOf decides how err, a handler's error, is answered: one decision for every route,
// so one failure reads the same whichever route met it.
func handlerRefusalOf(err error) handlerRefusal {
	refuse := func(code string, status int, message string) handlerRefusal {
		return handlerRefusal{status: status, code: code, message: message}
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		refusal := refuse(apiErr.code, apiErr.status, apiErr.message)
		if apiErr.retryAfter > 0 || apiErr.scope != "" {
			refusal.header = http.Header{}
		}
		if apiErr.retryAfter > 0 {
			refusal.header.Set("Retry-After", strconv.Itoa(apiErr.retryAfter))
		}
		if apiErr.scope != "" {
			refusal.header.Set("WWW-Authenticate", insufficientScopeChallenge(apiErr.scope))
		}
		// A 500 the handler named is a server fault like any other, so the operator's log shows it.
		if apiErr.status == http.StatusInternalServerError {
			refusal.log = func() { slog.Error("dispatch: API handler failed", "code", apiErr.code, "error", err) }
		}
		return refusal
	}
	documentCode := documentErrorCode(err)
	switch documentCode {
	case codeDocServiceUnavailable:
		return refuse(documentCode, http.StatusServiceUnavailable, docs.ErrServiceUnavailable.Error())
	case codeDocumentUnloadable:
		refusal := refuse(documentCode, http.StatusConflict, docs.ErrDocumentUnloadable.Error())
		refusal.log = func() { slog.Warn("dispatch: API document history cannot load", "error", err) }
		return refusal
	case codeDocNewerContent:
		// The message is the document's own (docs.NewerContentError), the same on every route.
		var newer *docs.NewerContentError
		message := docs.ErrDocNewerContent.Error()
		var elements []string
		if errors.As(err, &newer) {
			message = newer.Error()
			elements = newer.Elements
		}
		refusal := refuse(documentCode, http.StatusConflict, message)
		// The operator's line names the elements the document holds and nothing taken from the
		// error: its chain carries whatever the request wrapped it in, the caller's own identity
		// among it, which no log of ours writes (CodeQL go/clear-text-logging).
		refusal.log = func() {
			slog.Warn("dispatch: API document holds an element this build cannot read", "elements", elements)
		}
		return refusal
	}
	// The edit route's own ambiguity error names the operation and the quote; the bare pmdoc one
	// still serves every other caller of FindQuote.
	var ambiguousQuote *docs.ErrAnchorAmbiguous
	if errors.As(err, &ambiguousQuote) {
		return ambiguousTargetRefusal(ambiguousQuote.Error(), ambiguousQuote.Candidates)
	}
	var ambiguous *pmdoc.ErrTargetAmbiguous
	if errors.As(err, &ambiguous) {
		return ambiguousTargetRefusal(ambiguous.Error(), ambiguous.Candidates)
	}

	var preconditionFailed *docs.ErrPreconditionFailed
	if errors.As(err, &preconditionFailed) {
		body := editPreconditionBody(preconditionFailed)
		return handlerRefusal{status: http.StatusConflict, code: body.Code, message: body.Error, body: body}
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
		return handlerRefusal{status: http.StatusConflict, code: body.Code, message: body.Error, body: body}
	}
	var invalidPrecondition *docs.ErrInvalidPrecondition
	if errors.As(err, &invalidPrecondition) {
		return refuse("INVALID_PRECONDITION", http.StatusBadRequest, invalidPrecondition.Error())
	}
	if errors.Is(err, docs.ErrPreconditionBusy) {
		return refuse("EDIT_QUEUE_FULL", http.StatusTooManyRequests, docs.ErrPreconditionBusy.Error())
	}
	var invalidAskBlock *docs.ErrInvalidAskBlock
	if errors.As(err, &invalidAskBlock) {
		return refuse("INVALID_ASK_BLOCK", http.StatusBadRequest, invalidAskBlock.Error())
	}
	var invalidCICheck *docs.ErrInvalidCICheck
	if errors.As(err, &invalidCICheck) {
		return refuse("INVALID_CI_CHECK", http.StatusBadRequest, invalidCICheck.Error())
	}
	var unrepresentableAsk *docs.ErrAskBlockUnrepresentable
	if errors.As(err, &unrepresentableAsk) {
		return refuse("ASK_BLOCK_TEXT", http.StatusBadRequest, unrepresentableAsk.Error())
	}
	var invalidOp *docs.ErrInvalidOp
	if errors.As(err, &invalidOp) {
		return refuse("INVALID_OP", http.StatusBadRequest, invalidOp.Error())
	}
	if errors.Is(err, pmdoc.ErrTargetSpansBlocks) {
		return refuse("TARGET_SPANS_BLOCKS", http.StatusBadRequest, err.Error())
	}
	if errors.Is(err, pmdoc.ErrTableWidth) {
		return refuse("TABLE_WIDTH", http.StatusBadRequest, err.Error())
	}
	if errors.Is(err, docs.ErrAnchorMissing) {
		return refuse("ANCHOR_MISSING", http.StatusConflict, err.Error())
	}
	if errors.Is(err, docs.ErrAnchorOrphaned) {
		return refuse("ANCHOR_ORPHANED", http.StatusConflict, err.Error())
	}
	if errors.Is(err, pmdoc.ErrTargetNotFound) {
		return refuse("TARGET_NOT_FOUND", http.StatusNotFound, err.Error())
	}
	// A block pointer refused on any route but the edit route (which answers INVALID_OP) is the
	// caller's markdown to fix, as a schema refusal of it is.
	if errors.Is(err, docs.ErrInvalidMarkdown) || errors.Is(err, pmdoc.ErrBlockPointer) {
		return refuse("INVALID_MARKDOWN", http.StatusBadRequest, err.Error())
	}
	if docs.IsTooLarge(err) {
		refusal := refuse("CAP_EXCEEDED", http.StatusRequestEntityTooLarge, err.Error())
		refusal.log = func() { slog.Warn("dispatch: API refused a document too large to store", "error", err) }
		return refusal
	}
	// A tree outside the schema that the document holds answers 409 with its one message, the same
	// on every route whatever the route wrapped it in (docs.OutsideSchemaError). Any other schema
	// refusal is a tree an operation produced, which is the server's fault.
	if documentCode == codeDocSchema {
		var outside *docs.OutsideSchemaError
		if errors.As(err, &outside) {
			refusal := refuse(documentCode, http.StatusConflict, outside.Error())
			refusal.log = func() { slog.Warn("dispatch: API document outside Proof schema", "error", err) }
			return refusal
		}
		refusal := refuse(documentCode, http.StatusInternalServerError, err.Error())
		refusal.log = func() { slog.Error("dispatch: API document outside Proof schema", "error", err) }
		return refusal
	}
	if errors.Is(err, docs.ErrIssueClosed) {
		return refuse("ISSUE_CLOSED", http.StatusConflict, err.Error())
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse("NOT_FOUND", http.StatusNotFound, "not found")
	}
	refusal := refuse(codeInternal, http.StatusInternalServerError, "internal server error")
	refusal.log = func() { slog.Error("dispatch: API handler failed", "error", err) }
	return refusal
}

// ambiguousTargetError is the 409 TARGET_AMBIGUOUS body a write answers a quote or heading
// anchor that resolved in several places.
type ambiguousTargetError struct {
	Candidates []pmdoc.Candidate `json:"candidates"`
	Code       string            `json:"code"`
	Error      string            `json:"error"`
}

// ambiguousTargetRefusal refuses a quote that resolved in several places: the caller picks one by
// the candidates' positions, or narrows the quote.
func ambiguousTargetRefusal(message string, candidates []pmdoc.Candidate) handlerRefusal {
	body := ambiguousTargetError{Candidates: candidates, Code: "TARGET_AMBIGUOUS", Error: message}
	return handlerRefusal{status: http.StatusConflict, code: body.Code, message: message, body: body}
}
