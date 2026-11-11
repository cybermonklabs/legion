package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// coldVerificationDocument is a Verification document with one check, uploaded by the process
// that creates the issue and read by the process that records a result against it.
const coldVerificationDocument = ":::check{#k1 surface=\"cli\" run=\"go test ./...\" negative_control=\"Break it and watch it fail.\"}\n" +
	"The server records a result against a check.\n" +
	":::\n"

// A keyed write that reads a document no room holds is served, driven on the real binary.
//
// Recording a check result resolves the check against the live Verification document
// (api/results.go's resolveCheck -> checkBlockWords -> Docs.Text), which on a process that holds
// no room for it and has no rendering of it cached is a cold read: the fold opens a read-only
// transaction on the request pool, under the request's own context. Every write an envoy client
// makes carries an Idempotency-Key, and the step that key puts on every transaction of the
// request is a SELECT with an update CTE, which Postgres refuses inside a READ ONLY transaction
// whether or not the update matches a row. So the fold's commit failed, the fold failed the
// room, every reader sharing the singleflight got the error, and the write was answered 503
// DOC_SERVICE_UNAVAILABLE with no Retry-After.
//
// The second process is how the room is made cold: it never loaded the document, so nothing of
// it is resident and nothing of its rendering is cached, which is the state the room heap budget
// leaves an idle document in as well.
func TestAKeyedResultOnADocumentNoProcessHoldsIsRecorded(t *testing.T) {
	database := storetest.Open(t)
	databaseURL := database.Pool.Config().ConnString()
	writer := startDispatchProcess(t, databaseURL)
	writer.waitHealthy(t)
	issueKey, _ := writer.createIssue(t)
	upload, err := json.Marshal(map[string]string{"name": "verification.md", "content": coldVerificationDocument})
	if err != nil {
		t.Fatalf("encode the verification document: %v", err)
	}
	writer.call(t, http.MethodPost, "/api/v1/issues/"+issueKey+"/artifacts", string(upload), http.StatusCreated)
	signalled := writer.Terminate(t)
	writer.waitPromptExit(t, signalled)

	reader := startDispatchProcess(t, databaseURL)
	reader.waitHealthy(t)
	result, err := json.Marshal(map[string]any{
		"check": "k1", "role": "implementer", "verdict": "pass",
		"commit": "1111111111111111111111111111111111111111",
	})
	if err != nil {
		t.Fatalf("encode the result: %v", err)
	}
	answer := reader.send(t, http.MethodPost, "/api/v1/issues/"+issueKey+"/results", string(result),
		asKeyed(dispatchTestLogin, "cold-result-1"))
	if answer.status != http.StatusCreated {
		t.Fatalf("a keyed result on a document no process holds: status %d, want 201: %s", answer.status, answer.body)
	}
	// The room the fold would have failed still serves its text.
	reader.call(t, http.MethodGet, "/api/v1/issues/"+issueKey+"/results", "", http.StatusOK)
}
