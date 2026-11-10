package api

import (
	"bufio"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Dispatch's answers are JSON a page reads whole - an issue's comment list, its event log, a
// document's blocks - and they compress by a factor of ten or more. Under `Dispatch-Trees: 1` the
// dashboard is sent the display tree of every stored text beside the markdown itself
// (display_trees.go), which makes the same answer several times larger, so the bytes a browser
// waits for grow with the trees it asked for. compressAnswers gzips those answers for the callers
// that say they take gzip, and changes nothing else about them: same status, same JSON, same
// headers but Content-Encoding, Vary, and the nosniff every answer that goes through this layer
// is given (pass).
//
// gzip alone: every browser and every Go client takes it, and brotli would cost a dependency and
// a second encoder for an answer nothing holds in a cache.

const (
	// compressedEncoding is the only encoding this layer writes.
	compressedEncoding = "gzip"
	// compressibleMinimum is the smallest answer worth compressing. A gzip stream costs 18 bytes
	// of header and trailer and a compressor's whole state, and an answer under a kilobyte already
	// travels in the one segment it would travel in compressed, so nothing below this is
	// compressed: the keyed layer's refusals, a 404, an ask's answer, the per-request writes a
	// page makes while a person types. The comment lists and event logs this exists for are two
	// orders of magnitude above it.
	compressibleMinimum = 1024
)

// compressAnswers serves one route so that its answer is gzipped when the request says the caller
// takes gzip, the answer is Dispatch's own JSON, and it is at least compressibleMinimum bytes
// long. It is mounted around every row of the route table (register), inside the access log, whose
// line then records the bytes that went on the wire.
//
// Every answer carries `Vary: Accept-Encoding` whether or not it was compressed, beside the
// `Vary: Dispatch-Trees` the same loop adds (varyOnDisplayTrees), so no cache between a browser
// and Dispatch hands a caller that takes neither gzip nor trees an answer written for one that
// takes both. Every answer carries `X-Content-Type-Options: nosniff` the same way, compressed or
// not: these answers are JSON holding text people wrote, and a browser that reads one of them as
// something other than the type it was given runs that text as a page of Dispatch's own origin.
// The routes that serve an upload's own bytes set the same header for the same reason
// (serveFileVersion, artifact_files.go).
func compressAnswers(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			// A caller that sends no Accept-Encoding, or none that names gzip, is written the
			// bytes it was written before this existed, through a writer that adds one thing:
			// the media type its handler may not have named. Without it the type would be
			// whatever net/http sniffs from the answer's first bytes, which for a body opening
			// with an HTML tag (`<html`, `<script`, `<!--`) is text/html, and nosniff holds a
			// browser to a type the server set rather than to none at all.
			handler(&typedWriter{ResponseWriter: w}, r)
			return
		}
		answer := &compressingWriter{ResponseWriter: w}
		handler(answer, r)
		// Only a handler that returned has an answer to end: one that panics (an artifact
		// download whose store fails mid-stream aborts with http.ErrAbortHandler) leaves its
		// connection to be dropped rather than its half-written body to be closed as if whole.
		answer.finish()
	}
}

// acceptsGzip reads an Accept-Encoding header and reports whether the caller takes gzip: `gzip`
// named with any weight above zero, or `*` with one where gzip is not named. `gzip;q=0` is a
// refusal and outranks a `*` beside it (RFC 9110 §12.5.3).
func acceptsGzip(header string) bool {
	if header == "" {
		return false
	}
	wildcard := false
	for rest := header; rest != ""; {
		var field string
		field, rest, _ = strings.Cut(rest, ",")
		name, parameters, _ := strings.Cut(field, ";")
		name = strings.TrimSpace(name)
		switch {
		case strings.EqualFold(name, compressedEncoding):
			return !refusedWeight(parameters)
		case name == "*":
			wildcard = !refusedWeight(parameters)
		}
	}
	return wildcard
}

// refusedWeight reports whether the parameters of one Accept-Encoding field carry `q=0`, which is
// how a caller names an encoding to refuse it. A weight this cannot read is no refusal.
func refusedWeight(parameters string) bool {
	for rest := parameters; rest != ""; {
		var parameter string
		parameter, rest, _ = strings.Cut(rest, ";")
		name, value, found := strings.Cut(parameter, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "q") {
			continue
		}
		weight, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return err == nil && weight <= 0
	}
	return false
}

// compressibleAnswer reports whether an answer's own headers say its body is Dispatch's JSON, the
// only thing this layer compresses. Every answer Dispatch composes is written through WriteJSON,
// which sets `application/json`; every other media type an API route writes is an upload's own
// bytes, served under the type they were stored as (serveFileVersion) - an image, a PDF, an
// archive, each already compressed or none of this layer's business - so the media type alone
// keeps the downloads out without a list of routes to keep up to date. An answer that already
// carries an encoding is left as the handler wrote it.
func compressibleAnswer(header http.Header) bool {
	if header.Get("Content-Encoding") != "" {
		return false
	}
	mediaType, _, _ := strings.Cut(header.Get("Content-Type"), ";")
	mediaType = strings.TrimSpace(mediaType)
	return strings.EqualFold(mediaType, "application/json") ||
		(len(mediaType) > len("+json") && strings.EqualFold(mediaType[len(mediaType)-len("+json"):], "+json"))
}

// gzipWriters keeps the compressors between answers: a gzip.Writer carries deflate's whole window
// and hash tables, which is hundreds of kilobytes an answer would otherwise allocate and drop.
// Its New makes every writer at one level, so the layer that compresses an answer on its way out
// and the list answers kept already compressed (list_answers.go) send a caller the same bytes
// without either of them naming a level of its own.
//
// gzip.BestSpeed, not the default 6: a warm read is dominated by encoding and compressing the
// answer, and the trees these answers now carry make the body larger. On the 103 KB comment list
// of BenchmarkCompressingAnAnswer, level 1 takes 284us and sends 7,698 bytes where level 6 takes
// 861us and sends 6,882 - a third of the time for a ninth more bytes, and the bytes are the side
// of the trade with room to give. The level is a constant, so NewWriterLevel cannot refuse it.
var gzipWriters = sync.Pool{New: func() any {
	writer, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	return writer
}}

// acquireGzip is a compressor from the pool, writing to w. Every caller gives it back with
// releaseGzip when the stream it wrote is closed.
func acquireGzip(w io.Writer) *gzip.Writer {
	writer := gzipWriters.Get().(*gzip.Writer)
	writer.Reset(w)
	return writer
}

// releaseGzip gives writer back to the pool holding nothing of the answer it wrote, so a pooled
// compressor keeps no reference to a response writer or a buffer that is done with.
func releaseGzip(writer *gzip.Writer) {
	writer.Reset(io.Discard)
	gzipWriters.Put(writer)
}

// compressingWriter is the answer's writer while the choice between gzip and the bytes as they
// are is still open. It holds the answer's first compressibleMinimum bytes rather than deciding
// from a Content-Length no JSON answer sets: once that much is held the answer is compressed, and
// an answer that ends below it goes out as it was written, header and all. Nothing but the body
// waits - the status and every header the handler set are written the moment the choice is made.
type compressingWriter struct {
	http.ResponseWriter
	// status is the status the handler gave, and 0 until it gives one.
	status int
	// held is the answer's first bytes, kept until they reach compressibleMinimum or the answer
	// ends.
	held []byte
	// gzip is the compressor, once the answer is being compressed, and nil otherwise.
	gzip *gzip.Writer
	// settled is true once the choice is made: compressing through gzip, or passing every byte
	// straight through.
	settled bool
	// err is the first error a write to the compressor gave, which the Write that caused it and
	// every Write after it answer.
	err error
}

// pass settles the answer when the choice is still open - the status and the headers under it go
// out then, and whatever was held goes out ahead of body - and passes body on to the compressor
// or to the connection, whichever the answer settled on. compress is read only while the choice
// is open. Compressing drops the Content-Length the handler may have set, since the length it
// names is the length of bytes that are no longer what goes out, and chunks the answer instead.
//
// The headers and the bytes they describe are written by the one method because the answer's
// media type is part of what makes its body safe to send: once this layer holds the body, it is
// this layer that would otherwise hand net/http a typeless body to sniff, and bytes someone
// else wrote that are sniffed into text/html run as a page of Dispatch's own origin. So an
// answer that reaches here naming no type goes out as bytes; the nosniff that holds a browser
// to the type it was given is on every answer of this layer already (compressAnswers).
func (w *compressingWriter) pass(body []byte, compress bool) (int, error) {
	held := 0
	if !w.settled {
		w.settled = true
		if compress {
			w.Header().Del("Content-Length")
			w.Header().Set("Content-Encoding", compressedEncoding)
			w.gzip = acquireGzip(w.ResponseWriter)
		}
		status := w.status
		if status == 0 {
			status = http.StatusOK
		}
		if answerNamesNoType(w.Header(), status) {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		w.ResponseWriter.WriteHeader(status)
		if len(w.held) > 0 {
			held = len(w.held)
			body = append(w.held, body...)
			w.held = nil
		}
	}
	if w.err != nil {
		return 0, w.err
	}
	if len(body) == 0 {
		return 0, nil
	}
	// The count answers for body alone: what was held went out under an earlier Write, which
	// counted it then.
	if w.gzip == nil {
		written, err := w.ResponseWriter.Write(body)
		return max(written-held, 0), err
	}
	written, err := w.gzip.Write(body)
	if err != nil {
		w.err = err
	}
	return max(written-held, 0), err
}

// answerNamesNoType reports whether an answer of this status still needs a media type named for
// it: one the handler left unnamed, on an answer that carries a body for a type to describe (a
// 204 and a 304 carry none). No route of the table answers without a type today, so it is true
// only for a handler that forgets one.
//
// The `Set` it leads to is written at each place that writes an answer rather than inside a
// helper of its own, which reads as a duplication and is not one: CodeQL's reflected-XSS query
// drops a body sink only where the content type is written on the same writer in the same
// function (`Xss.qll`'s nonHtmlContentType through SsaWithFields.similar), so a `Set` one call
// away leaves every `Write` of this file flagged - measured, by a round that wrote it that way
// and saw the two new alerts. The condition is shared; the write is not.
func answerNamesNoType(header http.Header, status int) bool {
	if status == http.StatusNoContent || status == http.StatusNotModified {
		return false
	}
	return header.Get("Content-Type") == ""
}

// typedWriter is the answer's writer for a caller that takes no gzip: net/http's own, with the
// media type named at the moment the status is written, which is the last point before a browser
// could be handed a sniffed one. It holds nothing and copies nothing, so a caller that takes no
// gzip is written the bytes it was written before this layer existed.
type typedWriter struct {
	http.ResponseWriter
	// wrote is true once a status has gone out, after which the headers are no longer this
	// writer's to change.
	wrote bool
}

func (w *typedWriter) WriteHeader(status int) {
	if !w.wrote && status >= http.StatusOK {
		w.wrote = true
		if answerNamesNoType(w.Header(), status) {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write names the type for a handler that writes a body without a status of its own, which
// net/http answers 200 for.
func (w *typedWriter) Write(body []byte) (int, error) {
	if !w.wrote {
		w.wrote = true
		if answerNamesNoType(w.Header(), http.StatusOK) {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
	}
	return w.ResponseWriter.Write(body)
}

// ReadFrom, Flush and Hijack are forwarded so wrapping costs a handler none of the interfaces
// net/http's own writer offers it: the event stream flushes, an upgrade hijacks, and a file's
// bytes are copied with ReadFrom. Each names the media type first, since each can be the moment
// the answer's head goes out.
func (w *typedWriter) ReadFrom(body io.Reader) (int64, error) {
	if !w.wrote {
		w.wrote = true
		if answerNamesNoType(w.Header(), http.StatusOK) {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
	}
	if from, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return from.ReadFrom(body)
	}
	return io.Copy(w.ResponseWriter, body)
}

func (w *typedWriter) Flush() {
	if !w.wrote {
		w.wrote = true
		if answerNamesNoType(w.Header(), http.StatusOK) {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *typedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	// Past the upgrade the connection's bytes are the handler's frames, not an answer's body.
	w.wrote = true
	return hijacker.Hijack()
}

func (w *typedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *compressingWriter) WriteHeader(status int) {
	if w.settled || status < http.StatusOK {
		// An informational response (100 Continue, 103 Early Hints) is not the answer and is
		// never this layer's to hold; a status after the answer's own is net/http's to complain
		// about, as it would without this layer.
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status == 0 {
		w.status = status
	}
	if !compressibleAnswer(w.Header()) {
		_, _ = w.pass(nil, false)
	}
}

// Write holds the answer's first bytes until they are worth compressing. A body that arrives in
// one piece, as json.Encoder writes it, is never copied into the held buffer at all.
func (w *compressingWriter) Write(body []byte) (int, error) {
	if w.settled {
		return w.pass(body, false)
	}
	if !compressibleAnswer(w.Header()) {
		return w.pass(body, false)
	}
	if len(w.held)+len(body) >= compressibleMinimum {
		return w.pass(body, true)
	}
	if w.held == nil {
		w.held = make([]byte, 0, compressibleMinimum)
	}
	w.held = append(w.held, body...)
	return len(body), nil
}

// ReadFrom keeps the server's own copy (sendfile, and its buffer reuse) for an answer that is not
// compressed, so an artifact download still streams as it did (accesslog.recorder.ReadFrom).
func (w *compressingWriter) ReadFrom(body io.Reader) (int64, error) {
	if !w.settled && compressibleAnswer(w.Header()) {
		// A compressible body streamed rather than written whole still passes the held buffer,
		// so it is judged by the same threshold: io.Copy through Write, without handing it this
		// ReadFrom again.
		return io.Copy(writeOnly{w}, body)
	}
	if !w.settled {
		_, _ = w.pass(nil, false)
	}
	if w.gzip != nil {
		return io.Copy(writeOnly{w.gzip}, body)
	}
	if from, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return from.ReadFrom(body)
	}
	return io.Copy(writeOnly{w.ResponseWriter}, body)
}

// writeOnly hides every method of its writer but Write.
type writeOnly struct{ io.Writer }

// Flush sends what the answer has so far, which makes the choice if it is still open: a handler
// that flushes has bytes it wants on the wire now, and below the threshold they go as they are.
func (w *compressingWriter) Flush() {
	if !w.settled {
		_, _ = w.pass(nil, compressibleAnswer(w.Header()) && len(w.held) >= compressibleMinimum)
	}
	if w.gzip != nil && w.err == nil {
		if err := w.gzip.Flush(); err != nil {
			w.err = err
		}
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// Hijack hands over the connection for a handler that writes its own answer on it, the websocket
// upgrade. Nothing of such an answer is compressed, and whatever the handler held before taking
// the connection goes out as it was written.
func (w *compressingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	if !w.settled && (w.status != 0 || len(w.held) > 0) {
		_, _ = w.pass(nil, false)
	}
	// Past the upgrade the connection's bytes are the handler's frames, not an answer's body.
	w.settled = true
	return hijacker.Hijack()
}

// Unwrap gives http.ResponseController the server's own writer, so a handler that moves its
// connection's deadlines reaches it through this wrapper (accesslog.recorder.Unwrap).
func (w *compressingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// finish ends the answer: a compressed one is closed, so its trailer goes out and its compressor
// returns to the pool, and one still held is written as it stands.
func (w *compressingWriter) finish() {
	if w.gzip != nil {
		if err := w.gzip.Close(); err != nil && w.err == nil {
			w.err = err
		}
		releaseGzip(w.gzip)
		w.gzip = nil
		return
	}
	if !w.settled {
		_, _ = w.pass(nil, false)
	}
}
