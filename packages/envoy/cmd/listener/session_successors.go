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
// messages to its successor (role_handover.go, handOverDirectFrame), and each copy keeps the
// message's own delivery, so Dispatch reads this to let the successor answer that delivery
// (internal/dispatch/api, lookUpRoleHolderReply).
//
// The route holds the records to the rule the claim route wrote them under (handoverRefusal): it
// follows a chain of records only through sessions whose last registration names the principal
// the replaced session last registered as, each read from the known-session bucket's leader
// (recordedPrincipal), so a session that registered as another principal since the claim may no
// longer answer the replaced session's deliveries. A record stays on the stream, and names its
// successor here, for as long as the stream keeps it, a finished one included, since the copies
// it made stay on the successor's subject.
//
// The answer names that principal (Principal) only to Dispatch's own verified identity
// (ENVOY_DISPATCH_PRINCIPAL, the one caller whose send may carry a role mark, roleMarkRefusal):
// Dispatch holds a successor's reply on a broker token to it (actsForSuccessorPrincipal), and no
// other caller needs it. Every other caller - a session's broker token, the shared token, another
// identity, an open /v1, and every caller of a listener that names no Dispatch principal - is
// answered the same successors with no principal, so no session reads which person or service
// another session registered as. A principal-less answer names a principal no broker token acts
// for, so Dispatch refuses every broker-token successor reply it reads it for, as it refuses one
// against a listener that predates the route.
//
// The read runs within sendRequestBudget of the request's own context, as the registry route's
// does, and one that did not answer is the bus-unavailable 503 with Retry-After: 1
// (writeBusUnavailable). A listener whose known-session bucket is not open can read no principal
// any session registered as, so it names no successor, as the claim route refuses every handover
// there (knownBucketNotOpenRefusal), and answers 200 rather than 503: the bucket opens only once
// an attempt of the store's own succeeds (session.KnownSessions says how often it attempts),
// which on a listener whose NATS grant does not carry the bucket is not before the grant does,
// and Dispatch would answer every successor's reply 503 for as long as that lasts. So a
// successor's reply Dispatch reads this for in the window before an attempt opens the bucket is
// refused 403, which its sender does not retry. The route reads another session's records by
// design, so a broker token is not narrowed to its own session here, as the registry route's read
// is not.
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
		principal, successors, err := handoverSuccessors(ctx, d.client, d.known, sessionID)
		if err != nil {
			writeBusUnavailable(w, fmt.Errorf("read the sessions that replaced session %s: %w", sessionID, err))
			return
		}
		shown, err := shownSuccessors(ctx, edgeScopeOf(r, nil), d.sessions, successors)
		if err != nil {
			writeBusUnavailable(w, err)
			return
		}
		// Dispatch holds a successor's reply to this principal (lookUpRoleHolderReply), so it is
		// named only beside a session it is the principal of, and only to Dispatch itself.
		if len(shown) == 0 || d.dispatchPrincipal == "" || verifiedPrincipal(r) != d.dispatchPrincipal {
			principal = ""
		}
		writeJSON(w, http.StatusOK, listenerwire.SessionSuccessors{SessionID: sessionID, Principal: principal, Successors: shown})
	}
}

// handoverSuccessors is every session that replaced sessionID, sorted and each once, and the
// principal sessionID last registered as (recordedPrincipal), which each of them last registered
// as too: each session a handover record names as the successor of sessionID
// (bus.Client.HandoverSuccessors), and each session that replaced one of those in turn, however
// long the chain, since a copy of a message handed to a session that is replaced before it reads
// it is handed on to the session that replaced it, its delivery unchanged, and the copies have no
// bound on their hops. A session is followed only while its last registration names that
// principal, the rule each record was written under (handoverRefusal); one that does not is not
// named, nor is any session only it leads to. Each session is read once, so a chain that comes
// back round ends there. A sessionID whose principal nobody recorded, or the bucket holds no
// record of, has none, and so has every session on a listener whose known-session store cannot
// answer (principalUnavailable: its bucket is not open, or it has no store). A read that did not
// answer - one the request's budget ended before the whole chain was read among them - is the
// error, never a list that names part of the chain.
func handoverSuccessors(ctx context.Context, client *bus.Client, known *session.KnownSessions, sessionID string) (string, []string, error) {
	principal, state, err := recordedPrincipal(ctx, known, sessionID)
	if err != nil || state != principalRecorded {
		return "", []string{}, err
	}
	named := map[string]bool{sessionID: true}
	successors := []string{}
	for from := []string{sessionID}; len(from) > 0; {
		var next []string
		for _, replaced := range from {
			listed, err := client.HandoverSuccessors(ctx, replaced)
			if err != nil {
				return "", nil, err
			}
			for _, successor := range listed {
				if named[successor] {
					continue
				}
				named[successor] = true
				other, state, err := recordedPrincipal(ctx, known, successor)
				if err != nil {
					return "", nil, err
				}
				if state == principalRecorded && other == principal {
					successors = append(successors, successor)
					next = append(next, successor)
				}
			}
		}
		from = next
	}
	slices.Sort(successors)
	return principal, successors, nil
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
