package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/sjawhar/envoy/internal/webhook"
)

// apiBodyLimit bounds the body of every /v1 route that reads one: 4 MiB, twice the largest request
// a caller sends. That request is a send or a publish (POST /v1/messages/send and
// /v1/messages/publish). Its envelope must fit the NATS max payload, the server's default of
// 1 MiB, which no deployment changes. The envelope carries the request's payload and only a
// one-line summary of its message (contracts.MessageEnvelope), so a body whose message and
// payload each come near 1 MiB, about 2 MiB in all, is served. The bodies of the other routes are
// smaller. The largest registration (POST /v1/interests/subscribe) that the live listener held on
// 2026-11-16 named 802 topics in about 35 KB, and a registration's topics and its entry's fields
// each go into one KV value under the same 1 MiB. A role claim is a few hundred bytes. A Legion
// agent batch is at most legionagents.MaxBatch entries of bounded fields, under 2 MiB.
const apiBodyLimit = 4 << 20

// apiBodyTooLarge is the error a /v1 route answers, with 413, to a body past apiBodyLimit: the
// webhook routes' text (webhook.BodyTooLarge), so every route names its limit in the same words.
var apiBodyTooLarge = webhook.BodyTooLarge(apiBodyLimit)

// readAPIBody decodes the JSON body of a /v1 request into into. A body past apiBodyLimit is
// answered 413 naming the limit, whatever it holds. A request whose Content-Length is past the
// limit is refused before any of its body is read. Any other body is read through
// http.MaxBytesReader, so no more than apiBodyLimit bytes of it are read and parsed. The decode
// stops at the end of the first JSON value or at the first syntax error, so the rest of the body
// is then read through the same limit and dropped, whatever the decode answered: a small value
// followed by more than the limit, and a body that does not decode and runs past the limit, are
// both bodies past it. http.MaxBytesReader keeps its first error and answers it to every later
// read, so a decode the limit stopped meets the limit again in that read. A body that did not
// decode, or whose rest could not be read, and is not past the limit is answered 400
// "invalid json"; an empty body is such a body. The status is 0 when the body decoded.
func readAPIBody(w http.ResponseWriter, r *http.Request, into any) (status int, message string) {
	if r.ContentLength > apiBodyLimit {
		return http.StatusRequestEntityTooLarge, apiBodyTooLarge
	}
	body := http.MaxBytesReader(w, r.Body, apiBodyLimit)
	decodeErr := json.NewDecoder(body).Decode(into)
	_, drainErr := io.Copy(io.Discard, body)
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(decodeErr, &tooLarge), errors.As(drainErr, &tooLarge):
		return http.StatusRequestEntityTooLarge, apiBodyTooLarge
	case decodeErr != nil, drainErr != nil:
		return http.StatusBadRequest, "invalid json"
	}
	return 0, ""
}

// decodeAPIBody is readAPIBody for a route that answers its refusal with writeJSONError. It
// reports whether the handler goes on.
func decodeAPIBody(w http.ResponseWriter, r *http.Request, into any) bool {
	status, message := readAPIBody(w, r, into)
	if status != 0 {
		writeJSONError(w, status, message)
		return false
	}
	return true
}
