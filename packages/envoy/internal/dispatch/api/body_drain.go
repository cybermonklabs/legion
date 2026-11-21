package api

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// An answer given before a request's body is read reaches some clients as a failed connection
// rather than as the answer (INFRA-258). An HTTP/1.1 client that sends its whole
// body before it reads the answer - WebKit and Firefox uploading a file - is still sending when
// net/http, finding the body unread, writes the answer and closes the connection; the close resets
// it, and the client drops the answer with it. So every layer's early answer - the keyed layer's
// KEY_INVALID, the closed-issue refusal of an upload, LOGIN_CHANGED, a refused path parameter, a
// handler's refusal made before it decodes - waits until the body is off the connection, read and
// discarded, never spooled, up to the route's own limit (routeBody) and for at most
// bodyDrainLimit. The keyed layer reads a repeat's body itself when it finds the key held by a
// first request still running, and judges the repeat once that body has arrived
// (answerDrainedRepeat).

// bodyDrainLimit is the longest the server reads one request's body for: a drain waits for the
// rest of a body for at most this, counted from when the drain begins (readFor), and a handler's
// read of a body is cut this long after its first read, however steadily the body is still
// arriving (deadlinedBody's ceiling). A minute carries a whole upload (routeBody) over a link of
// 450 KB/s or more. A body still arriving then is not waited for: a drain's answer goes out as it
// stood, a handler's cut read is answered BODY_TOO_SLOW, and the connection closes after either,
// so a client that stopped sending does not hold its request open - nor whatever the layer that
// answered it holds - for as long as it cares to.
const bodyDrainLimit = time.Minute

// drainBound is how long a drain waits for the rest of a body, from when the drain begins
// (bodyDrainLimit; drainReadLimit in a test).
func (s *server) drainBound() time.Duration {
	if s.drainReadLimit > 0 {
		return s.drainReadLimit
	}
	return bodyDrainLimit
}

// readDeadline is the read deadline of the connection a request arrived over
// (http.ResponseController.SetReadDeadline), which is the only bound a read of its body has: this
// server's http.Server sets no ReadTimeout (cmd/dispatch), deliberately, because its event stream
// is long-lived. A writer with no connection of its own (a test's recorder) takes no deadline,
// which set reports, and such a request's reads are bounded by nothing. The drain holds one
// (readFor) and so does the body a handler reads (deadlinedBody), which resets it as the body
// arrives.
type readDeadline struct {
	controller *http.ResponseController
}

func newReadDeadline(w http.ResponseWriter) readDeadline {
	return readDeadline{controller: http.NewResponseController(w)}
}

// set bounds the reads that follow at at, and reports whether the connection took the bound.
func (d readDeadline) set(at time.Time) bool {
	return d.controller.SetReadDeadline(at) == nil
}

// lift leaves the connection with no read deadline, which is what it has between requests: a
// deadline left there would end the read net/http keeps on an idle connection once the body has
// ended, and cancel the request's context under its answer.
func (d readDeadline) lift() {
	_ = d.controller.SetReadDeadline(time.Time{})
}

// readFor bounds a drain of the body of the request w answers to drainBound from now, past which a
// read fails and the drain ends; it answers what lifts the bound, which the drain calls only once
// the body has reached its end. A drain that ends short of the body's end - its read failed, or
// the body ran past the route's limit - leaves the deadline in place, since net/http reads on
// after the answer itself: it discards up to 256 KiB more of a body whose Content-Length leaves
// that little unread before it writes the answer's header, and again when it closes the body once
// the handler returns. With the deadline those reads end where the drain's did and the connection
// closes; lifted, a client that stopped sending would hold the connection open after its answer,
// and withhold the answer itself where its body ran past the limit.
func (s *server) readFor(w http.ResponseWriter) (lift func()) {
	deadline := newReadDeadline(w)
	if !deadline.set(time.Now().Add(s.drainBound())) {
		return func() {}
	}
	return deadline.lift
}

// expectsContinue reports whether r's client asked to be told to continue before it sends its body
// (Expect: 100-continue). net/http tells it so at the body's first read, so a layer that answers
// such a request without reading the body asks the client for none of it.
func expectsContinue(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Expect")), "100-continue")
}

// errUploadTooLarge is an upload route's answer to a body past its limit, errHandoffTooLarge the
// handoff write's, errMarkdownRoundTripTooLarge the markdown round trip's, and errJSONTooLarge
// every other route's (routeBody, decodeJSON).
var (
	errUploadTooLarge = errorf(http.StatusRequestEntityTooLarge, "CAP_EXCEEDED", "artifact blob exceeds 25 MB")
	errJSONTooLarge   = errorf(http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "request body exceeds %d bytes", maxJSONRequestBytes)
)

// drainBeforeAnswer serves a route whose requests carry a body - every route but a GET - so that
// whatever layer answers a request, and however little of its body that layer read, the answer
// is written only once the rest of the body is read and discarded: at most the route's limit and
// one byte more, in io.Discard's buffer, for at most drainBound (readFor). A body that runs past the
// limit is answered as the limit answers it, 413 with the route's own refusal, in place of the
// answer the layer gave, since no such request is taken whoever answers it; the connection then
// closes, as net/http closes one whose body runs past what it will discard. The one answer the
// limit does not replace is a 401: a caller that resolves to nobody is refused before any limit is
// judged (TestUnauthenticatedLargeIssueBodyIsRejectedBeforeDecoding), and a page whose session
// lapsed under a write reads that 401 as the cue to sign in again. A body that stops arriving - its
// time up, its client gone - leaves the answer as the layer gave it, and the connection closes
// after it. A body a layer already read to its end, or past the limit itself (its handler's 413),
// is left as it is, and so is the body of a client that asked to be told to continue before
// sending it (Expect: 100-continue) and was never told: it sends nothing, and the first read would
// tell it to send all of it.
func (s *server) drainBeforeAnswer(method, pattern string, handler http.HandlerFunc) http.HandlerFunc {
	limit, tooLarge := routeBody(method, pattern)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			handler(w, r)
			return
		}
		body := &drainedBody{ReadCloser: r.Body}
		r.Body = body
		handler(&drainingWriter{
			ResponseWriter: w,
			body:           body,
			limit:          limit,
			tooLarge:       tooLarge,
			askedContinue:  expectsContinue(r),
			readFor:        s.readFor,
		}, r)
	}
}

// drainedBody is a request's body that counts what was read of it and whether it ended.
type drainedBody struct {
	io.ReadCloser
	read    int64
	touched bool
	ended   bool
}

func (b *drainedBody) Read(p []byte) (int, error) {
	b.touched = true
	n, err := b.ReadCloser.Read(p)
	b.read += int64(n)
	if err != nil {
		b.ended = true
	}
	return n, err
}

// keepOnlyEveryAnswersHeaders deletes from header, the header of an answer a handler began and a
// writer is replacing, every header but the ones every answer of this server carries:
// X-Content-Type-Options: nosniff and Vary: Accept-Encoding (compressAnswers), and Vary:
// Dispatch-Trees (varyOnDisplayTrees), which those layers set before any handler runs. The
// replaced answer's own headers go - its media type, its Idempotency-Key, its Retry-After -
// because they describe an answer the caller is not given. The kept ones describe the
// replacement as much as what it replaces, and Vary keeps every value it holds. The drain calls it
// for every answer it puts in place of the one a handler began (drainingWriter.replace).
func keepOnlyEveryAnswersHeaders(header http.Header) {
	for name := range header {
		if name != "Vary" && name != "X-Content-Type-Options" {
			delete(header, name)
		}
	}
}

// drainingWriter is the answer's writer: its first header, write or flush drains the body first
// (drainBeforeAnswer), and answers a body that a deadline cut (deadlinedBody) with the refusal
// naming that deadline.
//
// The cut's refusal stands in place of the 400 the request is otherwise answered: a decode that
// meets the cut answers 400 INVALID_JSON and the multipart parser 400 ARTIFACT_INPUT, each blaming
// the caller's input for bytes the server stopped reading, and neither leaving the caller anything
// to act on. The refusal is a 503, which the caller retries, as it retries every other refusal the
// pod makes of a write it could not take. A 400 is the only answer the cut makes untrue, so it is
// the only one replaced. Every other answer was reached for a reason of its own and stands,
// whatever became of the body: the 401 of a session that lapsed under the write, which a page
// reads as the cue to sign in again (the limit's replacement spares it too); the 409 the keyed
// layer answers a repeat whose first request still holds its key, which it judges whether or not
// the repeat's body ever arrives (answerDrainedRepeat); and the 413 this writer puts in place of
// any answer to a body past its route's limit.
//
// The cut is answered by this writer rather than by a second one around it. A second writer
// forwarded every byte of the handler's answer as this one does, and that second forward was
// CodeQL's alert 122 (go/reflected-xss) on sjawhar/legion#2544: the same false positive as this
// writer's own forward, alert 19, dismissed. One writer leaves one forward.
type drainingWriter struct {
	http.ResponseWriter
	body          *drainedBody
	limit         int64
	tooLarge      *apiError
	askedContinue bool
	readFor       func(http.ResponseWriter) (lift func())
	begun         bool
	replaced      bool
	// deadlined is the body as the handler reads it, under the deadline the write limit put on it
	// (holdWriteLimit), whose cut this writer answers; nil where that layer put none.
	deadlined *deadlinedBody
}

// begin answers, once, before the first byte of the answer the handler began, whose status is
// status. A body a deadline cut, answered 400, is answered with the cut's refusal instead, and
// nothing more of it is read: the cut ended it. Any other body begin drains first. A read that
// fails - the drain's time up, the client gone, a connection that broke - leaves the answer to be
// written as it was, and the connection to close after it, the rest of the body unread. Only a
// body that ended lifts the drain's deadline (readFor); one that failed or ran past the limit
// keeps it for net/http's own reads after the answer.
func (w *drainingWriter) begin(status int) {
	if w.begun {
		return
	}
	w.begun = true
	if w.deadlined != nil && status == http.StatusBadRequest {
		if cut := w.deadlined.cutBy(); cut != nil {
			w.replace(cut)
			return
		}
	}
	body := w.body
	if body.ended || body.read > w.limit || (w.askedContinue && !body.touched) {
		return
	}
	lift := w.readFor(w.ResponseWriter)
	_, err := io.CopyN(io.Discard, body, w.limit-body.read+1)
	switch {
	case errors.Is(err, io.EOF):
		// The body ended inside the limit: the answer stands.
		lift()
		return
	case err != nil:
		w.ResponseWriter.Header().Set("Connection", "close")
		return
	case status == http.StatusUnauthorized:
		return
	}
	// The body runs past the limit: the answer is the limit's.
	w.replace(w.tooLarge)
}

// replace answers refusal in place of the answer the handler began, with the headers every answer
// carries and none of the handler's own (keepOnlyEveryAnswersHeaders).
func (w *drainingWriter) replace(refusal *apiError) {
	keepOnlyEveryAnswersHeaders(w.ResponseWriter.Header())
	writeError(w.ResponseWriter, refusal.code, refusal.status, refusal.message)
	w.replaced = true
}

func (w *drainingWriter) WriteHeader(status int) {
	w.begin(status)
	if !w.replaced {
		w.ResponseWriter.WriteHeader(status)
	}
}

// Write is the answer's body; a first Write without WriteHeader answers 200, as net/http's does.
func (w *drainingWriter) Write(p []byte) (int, error) {
	w.begin(http.StatusOK)
	if w.replaced {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}

// Flush flushes the answer once the body is drained, so a handler that flushes early cannot send
// the answer ahead of it.
func (w *drainingWriter) Flush() {
	w.begin(http.StatusOK)
	if !w.replaced {
		_ = http.NewResponseController(w.ResponseWriter).Flush()
	}
}

// Unwrap is the connection's own writer, for http.ResponseController.
func (w *drainingWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
