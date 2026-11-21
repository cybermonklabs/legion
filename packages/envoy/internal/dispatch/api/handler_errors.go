package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"runtime"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
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

// writeHandlerError answers err, the error a route's handler or one of its layers met, and writes
// the operator's line for a fault of the server's and for a document the server cannot serve. No
// line carries text taken from an error's message. A document's text, which can hold a person's
// address, travels in the messages of the refusals that quote it, and whatever wraps such a
// refusal carries that text on: a wrapper that keeps the chain (%w) lets errors.Is tell the
// refusal apart, but one that wraps it with %v, or an apiError whose message a handler formatted
// from it, reads as any other error, and nothing here can tell that a message holds no such text.
// So every line names the error by docs.LoggedFault instead - the refusal's class, the messages of
// the sentinels it is, a Postgres error's SQLSTATE and constraint, and the Go types of its chain -
// and says where it came from: the handler that met it (handlerSite) and the route it answered
// (answeredRoute), by which the request's own access-log line, naming its caller, path and
// status, is found (logHandlerError).
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
			logHandlerError(w, slog.LevelError, "dispatch: API handler failed", err, "code", apiErr.code)
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
		logHandlerError(w, slog.LevelWarn, "dispatch: API document history cannot load", err)
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
		logHandlerError(w, slog.LevelWarn, "dispatch: API refused a document too large to store", err)
		return
	}
	// A tree outside the schema that the document holds answers 409 with its one message, the same
	// on every route whatever the route wrapped it in (docs.OutsideSchemaError). Any other schema
	// refusal is a tree an operation produced, which is the server's fault. Either line names the
	// refusal as every line here names an error, by its class and never by its message: its chain
	// carries the document's text and whatever the request wrapped it in, the caller's own identity
	// among it.
	if documentCode == codeDocSchema {
		var outside *docs.OutsideSchemaError
		if errors.As(err, &outside) {
			writeError(w, documentCode, http.StatusConflict, outside.Error())
			logHandlerError(w, slog.LevelWarn, "dispatch: API document outside Proof schema", err)
			return
		}
		writeError(w, documentCode, http.StatusInternalServerError, err.Error())
		logHandlerError(w, slog.LevelError, "dispatch: API document outside Proof schema", err)
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
	logHandlerError(w, slog.LevelError, "dispatch: API handler failed", err)
}

// logHandlerError writes one of writeHandlerError's lines: message at level, then args, the
// attributes the line's own branch names, then the error as docs.LoggedFault describes it, the
// handler that met it (handlerSite) and the route it answered (answeredRoute). Each is words of
// this program's own, so the line holds nothing a caller or a document wrote.
func logHandlerError(w http.ResponseWriter, level slog.Level, message string, err error, args ...any) {
	args = append(args, "error", docs.LoggedFault(err))
	if site := handlerSite(); site != "" {
		args = append(args, "site", site)
	}
	if route := answeredRoute(w); route != "" {
		args = append(args, "route", route)
	}
	slog.Log(context.Background(), level, message, args...)
}

// handlerErrorWriters are the functions handlerSite reads past to the one that met the error:
// writeHandlerError, and this package's helpers that answer some errors themselves and hand it
// every other error they are given. Each is named by its method expression, so a rename cannot
// leave the set naming a function that no longer exists. init fills the set because a variable
// initialized from them would be an initialization cycle: each of them reaches handlerSite, which
// reads the set.
var handlerErrorWriters = map[string]bool{}

func init() {
	for _, writer := range []any{
		(*server).writeHandlerError,
		(*server).writeAuthenticationError,
		(*server).writeClaimError,
		(*server).writeEnvoyError,
		(*server).writeIDPrefixError,
		(*server).writeSessionGroupError,
		(*server).writeUploadDocumentError,
		(*server).refusePriorityWrite,
	} {
		handlerErrorWriters[runtime.FuncForPC(reflect.ValueOf(writer).Pointer()).Name()] = true
	}
}

// handlerSite is where the error writeHandlerError answers came from: the function and line that
// called it, past the helpers that only hand an error on (handlerErrorWriters), such as
// "api.(*server).createIssue:412", and "" when no such caller is on the stack. It names this
// build's code and never anything a request carried, and it tells two failures of one route apart.
func handlerSite() string {
	var callers [32]uintptr
	frames := runtime.CallersFrames(callers[:runtime.Callers(1, callers[:])])
	writing := false
	for {
		frame, more := frames.Next()
		switch {
		case handlerErrorWriters[frame.Function]:
			writing = true
		case writing:
			function := frame.Function[strings.LastIndexByte(frame.Function, '/')+1:]
			return function + ":" + strconv.Itoa(frame.Line)
		}
		if !more {
			return ""
		}
	}
}

// answeredRoute is the pattern of the route w answers, which the access log's writer holds
// (accesslog's Route), read through each writer a layer of the route's chain wraps it in, and ""
// for a writer no route's chain made, such as a test's recorder. It is the route table's pattern,
// never the request's path.
func answeredRoute(w http.ResponseWriter) string {
	for {
		switch writer := w.(type) {
		case interface{ Route() string }:
			return writer.Route()
		case *keyedAnswer:
			// The keyed layer's writer has no Unwrap on purpose (keyedAnswer), so the writer it
			// answers on is read from its field.
			w = writer.w
		case interface{ Unwrap() http.ResponseWriter }:
			w = writer.Unwrap()
		default:
			return ""
		}
	}
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
