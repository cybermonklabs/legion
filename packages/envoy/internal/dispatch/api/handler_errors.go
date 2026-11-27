package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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
// A stored history this request itself could not decode, or one whose decode parks more than a
// room's document may, which no room loads, is DOCUMENT_UNLOADABLE, the state a rebuild repairs. A
// tree holding an element this build does not know is DOC_NEWER_CONTENT: a
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
	return preconditionBody(failed.CurrentDocument, failed.Mismatches, failed.Error())
}

// preconditionBody is that body from its parts: the token that stands, the mismatches that named
// what the caller expected, and the sentence saying so. A block mismatch's current token is
// listed in current.blocks. A file's upload, whose refusal no document write raises, builds its
// body here (fileUploadPreconditionBody).
func preconditionBody(currentDocument string, mismatches []model.EditPreconditionMismatch, message string) editPreconditionError {
	currentBlocks := make([]model.EditBlockPrecondition, 0)
	for _, mismatch := range mismatches {
		if mismatch.Scope == "block" && mismatch.Current != nil {
			currentBlocks = append(currentBlocks, model.EditBlockPrecondition{
				ID: mismatch.BlockID, Token: *mismatch.Current,
			})
		}
	}
	return editPreconditionError{
		Code:       "PRECONDITION_FAILED",
		Current:    editPreconditionCurrent{Blocks: currentBlocks, Document: currentDocument},
		Error:      message,
		Mismatches: mismatches,
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

// deletedPostError is the 409 every write that would put text on a post its author withdrew
// answers, and the one place that names that refusal's code and its words: a reply under the
// post, an edit or a delete of it, an accept or a reject of its suggestion, a send or a reply
// of a delivery of it. kind names the post in the message, as the route's caller met it ("this
// comment is deleted"). A deleted post keeps its row and its place, so a write that moves no
// text - a resolve, a reopen - takes it and meets this refusal nowhere.
func deletedPostError(kind refs.ItemKind) *apiError {
	return errorf(http.StatusConflict, "POST_DELETED", "this %s is deleted", kind)
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
	// A write that R24 refuses names each issue that holds the rule (parent_status_rule.go).
	var conflict *parentStatusConflict
	if errors.As(err, &conflict) {
		WriteJSON(w, http.StatusConflict, conflict.body())
		return
	}
	// An acceptance write refused for an open child of another project names each such child
	// (refuseOpenForeignChildren in issue_parent.go).
	var open openForeignChildrenError
	if errors.As(err, &open) {
		WriteJSON(w, http.StatusConflict, open.body())
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
		WriteJSON(w, http.StatusConflict, editPreconditionBody(preconditionFailed))
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
	// A call whose error is its own context's end is the service unable to serve it in time, not
	// a fault: the same 503 DOC_SERVICE_UNAVAILABLE, with the same reason and the same retry
	// contract, as the document service's own busy refusal (docs.ErrServiceUnavailable, and docs'
	// errWriteBusy for a write whose wait for the copy-heap pool ended with its request). A burst
	// of concurrent API writes meets this wherever a request's context ends outside that one
	// wait: opening a transaction while every pooled connection is held, taking the owner's row,
	// reading the stored history. Each of those was answered 500, which names a fault of the
	// server's and tells the caller not to repeat a write the server never made
	// (dispatch://INFRA-179). The check comes after the document codes, so a failed room or an
	// unloadable history keeps the answer its own state earns, whatever cancelled this request.
	if s.endedWithACancelledContext(w, err) {
		writeError(w, codeDocServiceUnavailable, http.StatusServiceUnavailable, docs.ErrServiceUnavailable.Error())
		return
	}
	writeError(w, codeInternal, http.StatusInternalServerError, "internal server error")
	slog.Error("dispatch: API handler failed", "error", err)
}

// endedWithACancelledContext reports whether err is the end of the call w answers rather than a
// fault of the server's: err carries a context's end (context.Canceled or
// context.DeadlineExceeded) and one of the call's own contexts has ended. Those are the context
// the call runs on (callOf: the request's, which its client or net/http ends, or inside a keyed
// run the run's own, which keyedRunLimit bounds), the bound Join puts on the call's whole document
// write from its request's start (docs.WriteRequestBoundPassed), and the server's lifetime, which
// ends at a shutdown. Each says the same thing to the caller - the server could not serve this
// call in time, and the call may be made again - so each is refused alike. A cold read's bound
// (docs' coldReadTimeout) is not read here: the document service answers it
// docs.ErrServiceUnavailable, which documentErrorCode has answered before this runs.
//
// The end of any other context is a failure like any other and stays a 500: a statement whose own
// context a pool or a tracer ended while the call's was live
// (TestAReplyWhoseRouteReadFailsIsNotRefused), or a bound a handler put on one of its waits. A
// writer that names no call - a recorder a test hands writeHandlerError, a socket's upgrade -
// leaves only the server's lifetime to read.
func (s *server) endedWithACancelledContext(w http.ResponseWriter, err error) bool {
	deadline := errors.Is(err, context.DeadlineExceeded)
	if !deadline && !errors.Is(err, context.Canceled) {
		return false
	}
	if s.lifetime != nil && s.lifetime.Err() != nil {
		return true
	}
	call := callOf(w)
	// A call past WriteRequestBound has every DeadlineExceeded answered 503, a bound a handler put
	// on one of its own waits included, since the error names no deadline. That edge stays: by
	// then the call's own bound has passed too, and the answer turns what would be a 500 into the
	// retryable 503 the call would be owed for that bound.
	return call != nil && (call.Err() != nil || (deadline && docs.WriteRequestBoundPassed(call)))
}

// answersCall is embedded in a writer that knows the call it answers, naming the context that call
// runs on: compressAnswers' two writers, which every row of the table answers through, name the
// request's context as markCallerRequest marked it, and the keyed run's writer (keyedAnswer),
// which hides every writer beneath it, names the run's.
type answersCall struct{ call context.Context }

func (a answersCall) callContext() context.Context { return a.call }

// callOf is the context the first writer of w's chain that names a call (answersCall) names,
// unwrapping each writer before it, and nil when none does.
func callOf(w http.ResponseWriter) context.Context {
	for {
		if named, ok := w.(interface{ callContext() context.Context }); ok {
			return named.callContext()
		}
		inner, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		w = inner.Unwrap()
	}
}

// errorAnswer records what an error writer (writeHandlerError, writeAuthenticationError) writes:
// its status, its headers and its body, for a caller that answers it in a form of its own. A test
// hook sends it as a process's acknowledgement (runTestHook), and the status-change route answers
// it with the entries of the steps that committed (writeStatusChangeRefusal).
type errorAnswer struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (a *errorAnswer) Header() http.Header         { return a.header }
func (a *errorAnswer) Write(p []byte) (int, error) { return a.body.Write(p) }
func (a *errorAnswer) WriteHeader(status int)      { a.status = status }

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
