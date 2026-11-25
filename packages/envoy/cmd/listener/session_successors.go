package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/listenerwire"
)

// sessionSuccessorsPattern is the route of a session's successors (sessionSuccessorsHandler), one
// segment the session.
const sessionSuccessorsPattern = "/v1/sessions/{session}/successors"

// sessionSuccessorsHandler answers GET /v1/sessions/<session_id>/successors: the sessions that
// replaced that session (handoverSuccessors) as listenerwire.SessionSuccessors, the empty list for
// a session no one replaced. A handover record copies the replaced session's unread direct
// messages to its successor (role_takeover.go, handOverDirectFrame), and each copy keeps the
// message's own delivery, so Dispatch reads this to let the successor answer that delivery
// (internal/dispatch/api, lookUpRoleHolderReply).
//
// The route holds the records to the rule the claim route wrote them under, as each pass does
// (handoverStands): a session is named only while its last registration and the replaced
// session's name one principal, so a session that registered as another principal since the
// claim may no longer answer the replaced session's deliveries. A record stays on the stream, and
// names its successor here, for as long as the stream keeps it, a finished one included, since
// the copies it made stay on the successor's subject.
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
		successors, err := handoverSuccessors(ctx, d.client, d.known, sessionID)
		if err != nil {
			writeBusUnavailable(w, fmt.Errorf("read the sessions that replaced session %s: %w", sessionID, err))
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

// handoverChainMax bounds the hops handoverSuccessors follows from the session it is asked about:
// a session replaced, its successor replaced in turn, and so on. Each hop is one session lost
// before it read what it was handed, so a chain this long is far past any that happens, and the
// bound keeps one read of the route to a few stream reads.
const handoverChainMax = 8

// handoverSuccessors is every session that replaced sessionID, sorted and each once: each session
// a handover record names as the successor of sessionID (bus.Client.HandoverSuccessors), and each
// session that replaced one of those in turn, up to handoverChainMax hops, since a copy of a
// message handed to a session that is replaced before it reads it is handed on to the session that
// replaced it, its delivery unchanged. A session is followed only while its last registration
// names the principal sessionID last registered as (recordedPrincipal), the rule each record was
// written under (handoverRefusal); one that does not is not named, nor is any session only it
// leads to. A sessionID whose principal nobody recorded, or the bucket holds no record of, has
// none, and so has every session on a listener with no known-session store. A read that did not
// answer is the error.
func handoverSuccessors(ctx context.Context, client *bus.Client, known *session.KnownSessions, sessionID string) ([]string, error) {
	principal, recorded, err := recordedPrincipal(ctx, known, sessionID)
	if err != nil || !recorded {
		return []string{}, err
	}
	named := map[string]bool{sessionID: true}
	successors := []string{}
	for hop, from := 0, []string{sessionID}; hop < handoverChainMax && len(from) > 0; hop++ {
		var next []string
		for _, replaced := range from {
			listed, err := client.HandoverSuccessors(ctx, replaced)
			if err != nil {
				return nil, err
			}
			for _, successor := range listed {
				if named[successor] {
					continue
				}
				named[successor] = true
				other, recorded, err := recordedPrincipal(ctx, known, successor)
				if err != nil {
					return nil, err
				}
				if recorded && other == principal {
					successors = append(successors, successor)
					next = append(next, successor)
				}
			}
		}
		from = next
	}
	slices.Sort(successors)
	return successors, nil
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
