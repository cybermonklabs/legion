package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asof"
)

// A route of kind answers (asOfAnswers) that answers a request at a cut says which cut it answers
// and which members it leaves out (decisions 4 and 5 of INFRA-4620's plan). serveAtCut runs the
// route's handler under the cut, behind a writer that does both for every answer, so no handler
// writes the headers itself: a handler names each member it leaves out (leaveOut), and the writer
// names the cut (Dispatch-As-Of), names the members (Dispatch-As-Of-Absent) and takes them out of
// the encoded answer.

// pastAnswerKey is the context key of the request's pastAnswer.
type pastAnswerKey struct{}

// pastAnswer is the writer of one request that a route of kind answers serves at a cut. It writes
// the headers of a past-time answer (asof.WriteHeaders) on an answer, a 2xx or a 304, and on no
// refusal: a refusal answers no question about the past. When the handler left out members
// (leaveOut), it holds the body until the handler returns (finish), takes the members out of it
// (asof.OmitAbsent) and then writes the status and the body. An answer that names a member it may
// not leave out is a programming error (misnamedAbsent), and the writer then fails closed: it
// sends none of the handler's body, and answers 500 in its place (refuse). That body can hold the
// present value of a member that the records at the cut do not give, and a reader at the cut must
// never read it.
type pastAnswer struct {
	http.ResponseWriter
	route string
	cut   asof.Cut
	// misnamed reports an answer that names a member it may not leave out. It is misnamedAbsent,
	// which panics in a test. TestAPastTimeAnswerThatNamesWhatItMayNotLeaveOutFailsClosed
	// replaces it, so that it sees the answer the writer gives in a server.
	misnamed func(route, problem string)

	// mu guards members, decided and refused, which a handler's own goroutines can reach through
	// leaveOut.
	mu sync.Mutex
	// members is each member the handler left out, by its place in the answer.
	members []string
	// decided is set once the answer's status is decided: the headers are then written, and a
	// member left out after it is a programming error.
	decided bool
	// refused is set once the answer is refused: the writer then drops every byte the handler
	// writes.
	refused bool
	// held holds the body while members are to be taken out of it, and status is the answer's
	// status, written at finish. held is nil for an answer written as it goes.
	held   *bytes.Buffer
	status int
	// answeredInternal is set once refuse has written the 500 in place of the answer.
	answeredInternal bool
}

// serveAtCut runs next under cut as the answer of the route named route ("<method> <pattern>"):
// the store binds every statement next sends to the cut (asof.WithCut), and every answer next
// writes carries the headers of a past-time answer with the members it left out taken out.
func serveAtCut(w http.ResponseWriter, r *http.Request, route string, cut asof.Cut, next http.HandlerFunc) {
	(&pastAnswer{ResponseWriter: w, route: route, cut: cut, misnamed: misnamedAbsent}).serve(r, next)
}

// serve runs next under the answer's cut, behind the answer, and ends the answer once next
// returns (finish).
func (a *pastAnswer) serve(r *http.Request, next http.HandlerFunc) {
	ctx := context.WithValue(asof.WithCut(r.Context(), a.cut), pastAnswerKey{}, a)
	next(a, r.WithContext(ctx))
	a.finish()
}

// leaveOut names members that the answer of the request ctx serves leaves out, because the
// records at the cut do not give them (decision 5). Each member is its place in the answer, an
// RFC 6901 JSON Pointer such as /open_asks/1/revision, in which * stands for every member of an
// array; asof.AbsentFields must hold a pointer for it, for the route (asof.AbsentPointer). The
// writer takes each member out of the answer and names it in Dispatch-As-Of-Absent, so a handler
// names a member only where the answer leaves it out, and before it writes the answer. A request
// served at no cut leaves out nothing, and the call does nothing.
func leaveOut(ctx context.Context, members ...string) {
	answer, _ := ctx.Value(pastAnswerKey{}).(*pastAnswer)
	if answer == nil {
		return
	}
	answer.leave(members)
}

// leave takes members into the members the answer leaves out (leaveOut). A member named after
// the answer's status was decided is a programming error (misnamed). The headers are written by
// then, and the body can be too, so the answer is refused: finish answers it 500 where its status
// is still held, and drops the connection where its status went out.
func (a *pastAnswer) leave(members []string) {
	a.mu.Lock()
	decided := a.decided
	if decided {
		a.refused = true
	} else {
		a.members = append(a.members, members...)
	}
	a.mu.Unlock()
	if decided {
		a.misnamed(a.route, "it leaves out "+strings.Join(members, ", ")+" after it wrote the answer's status")
	}
}

// misnamedAbsent reports a past-time answer that names a member asof.AbsentFields does not hold
// for its route, that cannot be taken out of the answer, or that the handler names after the
// answer's status was decided: a programming error. In a test it fails the test, by a panic in
// the handler. In a server it is logged, and the writer refuses the answer (refuse, finish): it
// sends none of the handler's body, which can hold the present value of a member that the
// records at the cut do not give.
func misnamedAbsent(route, problem string) {
	if testing.Testing() {
		panic("dispatch: the past-time answer of " + route + " is wrong: " + problem)
	}
	slog.Error("dispatch: a past-time answer names what it may not leave out; it is refused 500 with none of its body",
		"route", route, "problem", problem)
}

// refusedAnswerHeaders are the headers of the answer that refuse replaces with its 500: the cut
// and the members left out, which only an answer names, and the headers that the handler sets to
// describe its body, which the 500 does not carry.
var refusedAnswerHeaders = []string{
	contracts.DispatchAsOfHeader, contracts.DispatchAsOfAbsentHeader,
	"Cache-Control", "Content-Disposition", "Content-Encoding", "Content-Length", "ETag",
}

// refuse answers 500 INTERNAL, as writeHandlerError answers a failure it does not know, in place
// of the answer, whose status has not gone out. The writer then drops every byte the handler
// writes (refused). It calls the server's own writer, so that the 500 does not pass through this
// writer's own decision.
func (a *pastAnswer) refuse() {
	a.mu.Lock()
	a.refused = true
	a.mu.Unlock()
	a.answeredInternal = true
	header := a.Header()
	for _, name := range refusedAnswerHeaders {
		header.Del(name)
	}
	writeError(a.ResponseWriter, codeInternal, http.StatusInternalServerError, "internal server error")
}

// isRefused reports whether the answer is refused, so that the writer drops what the handler
// writes.
func (a *pastAnswer) isRefused() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refused
}

// isAnswer reports whether status is an answer, which a past-time read marks with the cut: a
// 2xx, or a 304, which says the answer the caller holds stands.
func isAnswer(status int) bool {
	return status/100 == 2 || status == http.StatusNotModified
}

// WriteHeader decides the answer: for an answer it writes the headers of a past-time answer and,
// when members are left out, holds the status until finish; a refusal and an informational status
// go out as they are. An answer that leaves out a member asof.AbsentFields does not hold for the
// route is refused 500 (refuse).
func (a *pastAnswer) WriteHeader(status int) {
	if status < http.StatusOK {
		a.ResponseWriter.WriteHeader(status)
		return
	}
	a.mu.Lock()
	if a.decided {
		refused := a.refused
		a.mu.Unlock()
		if a.held == nil && !refused {
			// A second status is net/http's to complain about, as it would without this writer.
			a.ResponseWriter.WriteHeader(status)
		}
		return
	}
	a.decided = true
	members := a.members
	a.mu.Unlock()
	if !isAnswer(status) {
		a.ResponseWriter.WriteHeader(status)
		return
	}
	if err := asof.WriteHeaders(a.Header(), a.route, a.cut, members); err != nil {
		a.misnamed(a.route, err.Error())
		a.refuse()
		return
	}
	if len(members) == 0 || status == http.StatusNotModified || status == http.StatusNoContent {
		a.ResponseWriter.WriteHeader(status)
		return
	}
	a.status, a.held = status, &bytes.Buffer{}
}

// start decides a body written with no status of its own as a 200, as net/http answers it.
func (a *pastAnswer) start() {
	a.mu.Lock()
	decided := a.decided
	a.mu.Unlock()
	if !decided {
		a.WriteHeader(http.StatusOK)
	}
}

// Write writes the body, or holds it while members are to be taken out of it, or drops it once
// the answer is refused. The media type is named here, on this writer, where the handler named
// none (answerNamesNoType says why in this function and not in a helper).
func (a *pastAnswer) Write(body []byte) (int, error) {
	a.start()
	if a.isRefused() {
		return len(body), nil
	}
	if a.held != nil {
		return a.held.Write(body)
	}
	if answerNamesNoType(a.Header(), http.StatusOK) {
		a.Header().Set("Content-Type", "application/octet-stream")
	}
	return a.ResponseWriter.Write(body)
}

// ReadFrom keeps the server's own copy for a body written as it goes (a document version's bytes,
// which leave out no member). Once the answer is refused it reads the body and drops it.
func (a *pastAnswer) ReadFrom(body io.Reader) (int64, error) {
	a.start()
	if a.isRefused() {
		return io.Copy(io.Discard, body)
	}
	if a.held != nil {
		return a.held.ReadFrom(body)
	}
	if from, ok := a.ResponseWriter.(io.ReaderFrom); ok {
		return from.ReadFrom(body)
	}
	return io.Copy(writeOnly{a.ResponseWriter}, body)
}

// Flush sends what the answer has so far, unless it is held or refused: a held body goes out
// whole, once its members are taken out, and a refused answer sends none of the handler's body.
func (a *pastAnswer) Flush() {
	a.start()
	if a.held != nil || a.isRefused() {
		return
	}
	_ = http.NewResponseController(a.ResponseWriter).Flush()
}

// Unwrap gives http.ResponseController the server's own writer.
func (a *pastAnswer) Unwrap() http.ResponseWriter { return a.ResponseWriter }

// finish ends the answer once the handler returns. A handler that wrote nothing is answered 200,
// as net/http answers it, with the headers of a past-time answer. A held body goes out with the
// members it left out taken out. A body they cannot be taken out of is a programming error
// (misnamed), and the answer is refused 500 with none of the held body (refuse).
//
// finish also ends an answer that leave refused, for a member named after the status was
// decided. Where the status is still held, the answer is refused 500 with none of the held body.
// Where the status went out, no 500 can replace it. The writer dropped every byte the handler
// wrote after the refusal, but the bytes before it went out, and a body that ends early can read
// as whole: a document's bytes with no Content-Length, for one. So finish aborts the response
// (http.ErrAbortHandler), as a download whose store fails while it streams does
// (artifact_files.go): net/http drops the connection, and the client reads a broken answer, not
// a part of one.
func (a *pastAnswer) finish() {
	a.start()
	switch {
	case a.answeredInternal:
		return
	case a.isRefused():
		if a.held == nil {
			panic(http.ErrAbortHandler)
		}
		a.refuse()
		return
	case a.held == nil:
		return
	}
	body, err := asof.OmitAbsent(a.held.Bytes(), a.members)
	if err != nil {
		a.misnamed(a.route, err.Error())
		a.refuse()
		return
	}
	a.Header().Del("Content-Length")
	if answerNamesNoType(a.Header(), a.status) {
		a.Header().Set("Content-Type", "application/octet-stream")
	}
	a.ResponseWriter.WriteHeader(a.status)
	if _, err := a.ResponseWriter.Write(body); err != nil {
		slog.Warn("dispatch: write a past-time answer failed", "route", a.route, "error", err)
	}
}
