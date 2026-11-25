package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/listenerwire"
)

// sessionSuccessorsPattern is the route of a session's successors (sessionSuccessorsHandler), one
// segment the session.
const sessionSuccessorsPattern = "/v1/sessions/{session}/successors"

// sessionSuccessorsHandler answers GET /v1/sessions/<session_id>/successors: the sessions the
// stream's handover records name as having replaced that session (bus.Client.HandoverSuccessors,
// which reads the records' subjects and no body), as listenerwire.SessionSuccessors, the empty
// list for a session no record names. A handover record copies the replaced session's unread
// direct messages to its successor (role_takeover.go, handOverDirectFrame), and each copy keeps
// the message's own delivery, so Dispatch reads this to let the successor answer that delivery
// (internal/dispatch/api, lookUpRoleHolderReply).
//
// The route reads the records as they stand and judges none of them: only the claim route writes
// a handover record (publishHandover), and it writes one only for a claim whose replacement it
// has attested, so every record names a session that replaced the one it names. A record stays
// on the stream, and names its successor here, for as long as the stream keeps it, a finished one
// included, since the copies it made stay on the successor's subject.
//
// The read runs within sendRequestBudget of the request's own context, as the registry route's
// does, and one that did not answer is the bus-unavailable 503 with Retry-After: 1
// (writeBusUnavailable). The route reads another session's records by design, so a broker token
// is not narrowed to its own session here, as the registry route's read is not.
//
// An edge listener reads the records too: its grant (natsgrant.Edge) keeps the stream's
// STREAM.INFO, though it publishes no takeover lane and so writes no record of its own; the
// records are the shared listeners'. A successor is another session, so the edge names one only
// where it shows that session, confirmed by its stored record as a row of the session list is
// (shownSuccessors); a confirmation that did not answer is the bus-unavailable 503.
func sessionSuccessorsHandler(d *listenerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		sessionID := r.PathValue("session")
		if sessionID == "" {
			writeJSONError(w, http.StatusBadRequest, "session_id is required", "session_id")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
		defer cancel()
		successors, err := d.client.HandoverSuccessors(ctx, sessionID)
		if err != nil {
			writeBusUnavailable(w, fmt.Errorf("read the handover records of session %s: %w", sessionID, err))
			return
		}
		shown, err := shownSuccessors(ctx, edgeScopeOf(r, nil), d.sessions, successors)
		if err != nil {
			writeBusUnavailable(w, err)
			return
		}
		writeJSON(w, http.StatusOK, listenerwire.SessionSuccessors{SessionID: sessionID, Successors: shown})
	}
}

// shownSuccessors is successors, in their order, less each session scope does not show confirmed
// by its stored record (edgeScope.showsConfirmed, judged on the entry this listener's cache holds
// for it, and on none for a session it holds no entry of), and the empty list, never nil, for
// none. A nil scope, every listener but the edge, shows them all and reads nothing. A
// confirmation that did not answer is its error, which names no session (confirmUnanswered).
func shownSuccessors(ctx context.Context, scope *edgeScope, sessions *session.SessionRegistry, successors []string) ([]string, error) {
	shown := make([]string, 0, len(successors))
	if scope == nil {
		return append(shown, successors...), nil
	}
	for _, successor := range successors {
		entry, _ := sessions.Get(successor)
		ok, err := scope.showsConfirmed(ctx, sessions, successor, entry)
		if err != nil {
			return nil, err
		}
		if ok {
			shown = append(shown, successor)
		}
	}
	return shown, nil
}
