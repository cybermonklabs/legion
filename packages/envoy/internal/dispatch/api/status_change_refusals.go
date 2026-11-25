package api

// status_change_refusals.go gives every refusal that reaches POST
// /api/v1/issues/{key}/status-change's layers the changed that the route's declared refusals
// require (StatusChangeRefusal and StatusChangeConflict in the OpenAPI document). The handler and
// the keyed layer write it themselves, since they know which steps committed
// (writeStatusChangeRefusal, refuseStatusChangeKey). Every layer register puts outside the keyed
// layer - the read-only and scope gates, the sign-in and Dispatch-Login checks, the project and
// session checks, a refused path parameter, the drain's 413 - refuses before any step runs, with
// the {code, error} it answers on every route. Under such a refusal nothing ran, so its changed is
// [] whichever layer refused, and one layer of this route alone adds it, rather than a branch for
// this route in each of those layers. The router's cookie-origin check (enforceCookieOrigin)
// answers its 403 before any of those layers, on every route, and carries no changed.
//
// That layer holds every final answer of the route, the passing ones too, until the handler
// returns, and writes it then in one call (changedRefusalWriter.finish). Holding delays no byte
// the route would have sent sooner: the route streams nothing, since every answer is one
// WriteJSON, and the keyed layer holds every answer until it is kept anyway.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
)

// addChangedToRefusals serves the status-change route so that an error answer (a status from 400
// to 599) whose body is one JSON object with no changed member goes out with "changed":[] added
// as its last member; every other member keeps the bytes it was written with, and every header
// the layers inside set is kept but Content-Length, which is set for the new body. Any other
// answer goes out byte for byte as written, under the status and the headers the layers inside
// set: a 2xx and a 3xx, a refusal that already carries changed (the handler's and the keyed
// layer's), and one whose body is not one JSON object. A 1xx passes through as it is written.
// Every final answer is held, from its status or its first Write until the handler returns, and
// written then in one call: the route streams nothing, since every answer is one WriteJSON and
// the keyed layer holds every answer until it is kept anyway. register mounts it on this route
// alone (routeHandler), outside every layer of the route's chain and inside compression, so it
// reads the JSON each layer wrote.
func addChangedToRefusals(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		answer := &changedRefusalWriter{ResponseWriter: w}
		handler(answer, r)
		// Only a handler that returned has an answer to end, as compressAnswers has it: one that
		// panics leaves its connection to be dropped rather than a held answer to be sent.
		answer.finish()
	}
}

// changedRefusalWriter is the route's writer under addChangedToRefusals: it holds the answer's
// final status and its body until the handler returns, when finish writes them, so no body goes
// to the writer inside before then. A 1xx passes straight through. It implements neither Flush
// nor Hijack: the route streams nothing and upgrades nothing.
type changedRefusalWriter struct {
	http.ResponseWriter
	// status is the held answer's final status, and 0 while nothing is held: a status of 200 or
	// more is held, and a Write before any holds 200, which net/http answers a first Write with.
	status int
	// body is the held answer's body.
	body bytes.Buffer
}

// WriteHeader holds the answer's first final status and drops every status after it, as net/http
// drops one after the answer's own.
func (w *changedRefusalWriter) WriteHeader(status int) {
	switch {
	case w.status != 0:
		return
	case status < http.StatusOK:
		// An informational status (100 Continue, 103 Early Hints) passes straight through: it is
		// not the answer, which follows it.
		w.ResponseWriter.WriteHeader(status)
	default:
		w.status = status
	}
}

// Write holds body as the answer's; a first Write without WriteHeader holds a 200, as net/http
// answers it with 200.
func (w *changedRefusalWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(body)
}

// Unwrap gives http.ResponseController the writer this one holds answers for, so the drain sets
// its read deadline on the connection through this wrapper (readFor, drainingWriter.Unwrap).
func (w *changedRefusalWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// finish writes the held answer, if there is one, under its status and the headers the layers
// inside set: an error answer's body with changed added when withEmptyChanged adds it, and every
// other body as it was written. With nothing held it writes nothing, so net/http answers as it
// does a handler that wrote nothing.
func (w *changedRefusalWriter) finish() {
	if w.status == 0 {
		return
	}
	header := w.ResponseWriter.Header()
	body := w.body.Bytes()
	if w.status >= http.StatusBadRequest && w.status < 600 {
		if refusal, added := withEmptyChanged(body); added {
			body = refusal
			header.Set("Content-Length", strconv.Itoa(len(body)))
		}
	}
	// CodeQL's go/reflected-xss takes a body written to a ResponseWriter as HTML unless a
	// Content-Type is set on that writer in the function that writes the body. This sets again,
	// where the body goes out, the type the route's layers set (WriteJSON's application/json); a
	// header they did not set stays unset, so net/http sniffs the body as it would without this.
	if contentType := header.Get("Content-Type"); contentType != "" {
		header.Set("Content-Type", contentType)
	}
	w.ResponseWriter.WriteHeader(w.status)
	if len(body) > 0 {
		_, _ = w.ResponseWriter.Write(body)
	}
}

// withEmptyChanged answers body with "changed":[] written just inside its closing brace, and true,
// when body is one JSON object with no member named changed; every byte of body before that brace
// and after it is kept. Any other body answers false.
func withEmptyChanged(body []byte) ([]byte, bool) {
	var members map[string]json.RawMessage
	if json.Unmarshal(body, &members) != nil || members == nil {
		// Not one JSON object: another value, JSON null among them, or no JSON at all.
		return nil, false
	}
	if _, has := members["changed"]; has {
		return nil, false
	}
	member := `,"changed":[]`
	if len(members) == 0 {
		member = `"changed":[]`
	}
	// The object's closing brace is body's last '}': body was accepted as one JSON object, so only
	// JSON's whitespace follows it, such as the newline json.Encoder ends each value with.
	closing := bytes.LastIndexByte(body, '}')
	return slices.Concat(body[:closing], []byte(member), body[closing:]), true
}
