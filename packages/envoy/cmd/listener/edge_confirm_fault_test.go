//go:build natslive

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/listenerwire"
)

// edgeConfirmRequest builds a request an edge listener admitted for outside, held to projects
// (withEdgeHold, withBrokerCaller), under ctx. A cancelled ctx fails only the leader read
// edgeSessionShown makes: every cache read the routes make before it (session.SessionRegistry.Lookup)
// ignores the context, so the session still reads as present and only its stored-record confirmation
// does not answer.
func edgeConfirmRequest(ctx context.Context, outside string, projects []string, method, path, body string) *http.Request {
	caller := ciphertoken.Caller{Session: outside, Person: "outside@example.com", ProjectScope: ciphertoken.ProjectScopeNamed, Projects: projects}
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	return httptest.NewRequest(method, path, reader).WithContext(withEdgeHold(withBrokerCaller(ctx, caller)))
}

// Every edge decision that shows or sends to a session confirms it by a leader read of its stored
// record (edgeSessionShown, session.SessionRegistry.StoredByWriter), so a record the server wrote
// reads as absent. When that read does not answer - a leader election on the clustered hub, while
// the cached entry and direct gets still answer - the edge must answer the bus-unavailable 503 it
// answers for every other read that did not answer, with Retry-After: 1 and the bus's own unanswered
// wording, never the 404 of a session the server wrote a record for, nor a silent drop from the
// list that reads a partial answer as complete. The fault is a cancelled context on the leader read,
// as the reviewer's probe injects it; the full view, the list and the send route read the cache
// before it, so only the confirmation does not answer. The registration view reads the bucket
// itself (bucketSessionEntry), which answers that same 503 ahead of the confirmation, so it is not
// among these three; its confirmation answers the same error the way the full view's does.
func TestAnEdgeBusFaultConfirmingASessionAnswersBusUnavailable(t *testing.T) {
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const outside, peer = "ses_confirm_fault_outside", "ses_confirm_fault_peer"
	rig.placeElsewhere(t, peer, "PRIV")

	// The instrument's control is each route's own working-context call below: it must answer 200,
	// so a 503 under the fault is the confirmation read's and not a session the edge never shows.

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	sendBody := fmt.Sprintf(`{"target_session":%q,"source_session":%q,"message":"from the edge","source":"agent"}`, peer, outside)

	for _, tc := range []struct {
		name    string
		method  string
		path    string
		body    string
		handler http.Handler
	}{
		{"the full registry view", http.MethodGet, "/v1/registry/" + peer, "", sessionEntryHandler(rig.deps)},
		{"the session list", http.MethodGet, "/v1/sessions", "", sessionsHandler(rig.deps.registry, rig.deps.sessions, rig.deps.repositories)},
		{"the send route", http.MethodPost, "/v1/messages/send", sendBody, sendHandler(rig.deps, logging.New("test"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A working leader read answers this peer as a session the edge shows: 200, never 503.
			ok := httptest.NewRecorder()
			tc.handler.ServeHTTP(ok, edgeConfirmRequest(context.Background(), outside, []string{"PRIV"}, tc.method, tc.path, tc.body))
			if ok.Code != http.StatusOK {
				t.Fatalf("with a working leader read: status %d (%s), want 200", ok.Code, ok.Body.String())
			}
			if tc.name == "the session list" && !strings.Contains(ok.Body.String(), peer) {
				t.Fatalf("with a working leader read the list leaves out %s: %s", peer, ok.Body.String())
			}

			// A leader read that does not answer is the bus-unavailable 503, with Retry-After: 1 and
			// the bus's own unanswered wording, not the 404 of a session the server wrote nor a
			// partial 200.
			fault := httptest.NewRecorder()
			tc.handler.ServeHTTP(fault, edgeConfirmRequest(cancelled, outside, []string{"PRIV"}, tc.method, tc.path, tc.body))
			if fault.Code != http.StatusServiceUnavailable {
				t.Fatalf("with the leader read failing: status %d (%s), want 503: a bus that did not answer is not a session the edge does not show", fault.Code, fault.Body.String())
			}
			if retry := fault.Header().Get("Retry-After"); retry != "1" {
				t.Errorf("the 503 carries Retry-After %q, want 1", retry)
			}
			var refusal listenerwire.APIError
			_ = json.Unmarshal(fault.Body.Bytes(), &refusal)
			if !strings.HasPrefix(refusal.Error, busUnansweredPrefix) {
				t.Errorf("the 503 body is %q, want the bus's own unanswered wording %q", refusal.Error, busUnansweredPrefix)
			}
		})
	}
}
