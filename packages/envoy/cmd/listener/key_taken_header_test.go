// How the repeat-answer tests send a request whose client names the key-taken answer as one it
// reads. Kept apart from held_answer_test.go, which a check's negative control adds to a tree at
// main, where listenerwire names no such header.

package main

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/listenerwire"
)

// readingKeyTaken is request naming the key-taken answer as one its client reads
// (listenerwire.ReadsHeader), as @legion/envoy-client names it on a send or a publish that names
// its session.
func readingKeyTaken(request *http.Request) *http.Request {
	request.Header.Set(listenerwire.ReadsHeader, listenerwire.ReadsKeyTaken)
	return request
}

// publishFromReading is rig.publishFrom by a client that reads the key-taken answer
// (readingKeyTaken).
func (rig *senderLimitRig) publishFromReading(t *testing.T, sender, topic string, fields map[string]any) limitedAnswer {
	t.Helper()
	body := map[string]any{"topic": topic, "source_session": sender, "message": countedMessage}
	return serveLimited(t, rig.mux, rig.harness.logs.String, readingKeyTaken(postJSON("/v1/messages/publish", messageJSON(t, body, fields))))
}
