// pending_scope_read_test.go counts what the document a create's `#id`s resolve against costs.
// A route writing a comment, a reply, a message, an ask or an approval request has only queued
// that row's insert (INFRA-636), so that document is read over the fields the row will hold
// rather than over the row (refs.PendingDocument) - and only for a body that holds a `#`
// candidate, which is the only body whose text can name a question block. A body that holds
// none costs the read nothing.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// pendingScopeQueries are the reads refs makes of the document a queued row belongs to, one per
// kind of source a route queues (refs.pendingScope): each a `with` over the columns that insert
// will bind. They are matched by the column list, which is what tells them from every other
// read the request sends.
var pendingScopeQueries = []string{
	"with k (block_artifact_id, anchor, artifact_id, issue_key)",
	"with c (anchor, artifact_id, ask_id, reply_to, issue_key)",
	"with m (issue_key)",
}

// pendingScopeReads is how many of the statements one request sent read that document.
func pendingScopeReads(counted []string) int {
	reads := 0
	for _, statement := range counted {
		for _, query := range pendingScopeQueries {
			if strings.Contains(statement, query) {
				reads++
			}
		}
	}
	return reads
}

// countedCreate posts body to path through the handler on a request a roundTrips counts
// (countedRead, issue_read_batch_test.go).
func countedCreate(t *testing.T, handler http.Handler, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the request to %s: %v", path, err)
	}
	ctx := context.WithValue(context.Background(), countedRead{}, true)
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer agent-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// A create whose body holds no `#` candidate reads no scope: the document its `#id`s would
// resolve against is handed to the reader unread (refs.PendingDocument, refs.NewReaderInScope)
// and the reader asks for it only when text.Mentions asks it for the question map, which a body
// with no `#` in it never does. A body that writes one pays the read, once for the write
// however many of its texts are indexed: a new ask's question and its parts share the document
// between them (queueNewAskReferences).
//
// The handover a two-phase write needs costs no read of its own, then: the reader asked the
// source's own row for its document before (refs.ScopeOf), and asked it only here, so a body
// naming no question block sends what it always sent.
func TestACreateWithNoQuestionCandidateReadsNoScope(t *testing.T) {
	database := storetest.Open(t)
	counter := &roundTrips{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	handler, _, _ := newTestServer(t, testServerOptions{
		store: traced,
		// The caller's identity is resolved on the untraced pool, so the count is the request's
		// own statements and not the sign-in lookup every request makes.
		identity: headerIdentity(database),
		// A settlement of the issue's spec is database work of its own on the same pool; a delay
		// no test run reaches keeps it out of the count.
		settle: time.Hour,
	})
	issue := createInteractionIssue(t, handler, "SCOPE", "Count the scope read", "A spec")

	for _, create := range []struct {
		what  string
		path  string
		body  map[string]any
		reads int
	}{
		{"a comment naming no question block", "/api/v1/issues/" + issue.Key + "/comments",
			map[string]any{"body": "Nothing in this body names a block.", "actor": sessionActor()}, 0},
		{"a comment writing `#q1`", "/api/v1/issues/" + issue.Key + "/comments",
			map[string]any{"body": "Answered by #q1.", "actor": sessionActor()}, 1},
		{"an ask naming no question block", "/api/v1/issues/" + issue.Key + "/asks",
			map[string]any{"question": "Which way?", "actor": sessionActor()}, 0},
		{"an ask writing `#q1`", "/api/v1/issues/" + issue.Key + "/asks",
			map[string]any{"question": "Does #q1 settle this?", "actor": sessionActor()}, 1},
	} {
		counter.take()
		response := countedCreate(t, handler, create.path, create.body)
		counted := counter.take()
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s: status=%d body=%s", create.what, response.Code, response.Body.String())
		}
		if reads := pendingScopeReads(counted); reads != create.reads {
			t.Errorf("%s read the scope %d times in %d round trips, want %d:\n%s",
				create.what, reads, len(counted), create.reads, strings.Join(roundTripLines(counted), "\n"))
		}
	}
}
