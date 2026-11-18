// handlers_ui_pending.go: GET /v1/pending — lists the credential-request records naming an
// approver that nobody has decided, each with its state, pending or expired, and its deadline.
// With no ?state=, the list is every record still pending by what is stored, newest first, as it
// was before a record had a state; each ?state=pending or ?state=expired asks for the records of
// that state, the expired ones nobody has dismissed, the pending ones first. Part of the routes a
// listed service calls with its own ServiceAccount token (accountAuth), relaying a person's own
// page: each secret request names the broker's own page where that person decides it.
package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/wire"
)

// pendingResponse is GET /v1/pending's answer: wire.PendingResponse itself, named here because
// the broker's generated HTTP reference documents a route's answer from a type this package
// declares, as introspectionResponse names oauthwire.Introspection (handlers_introspect.go).
type pendingResponse = wire.PendingResponse

func (s *server) listPending(w http.ResponseWriter, r *http.Request, _ string) {
	approver := r.URL.Query().Get("approver")
	if !requireApprover(w, approver) {
		return
	}
	states, ok := pendingStates(w, r.URL.Query()["state"])
	if !ok {
		return
	}
	rows, err := s.deps.Machine.PendingForApprover(r.Context(), approver, states)
	if err != nil {
		writeInternal(w, "list pending requests", err)
		return
	}
	entries := make([]wire.PendingEntry, len(rows))
	for i, row := range rows {
		entries[i] = wire.PendingEntry{
			RecordID: row.RecordID, Kind: row.Kind, Identifiers: row.Identifiers, RequestedAt: row.RequestedAt,
			Session: sessionResp(row.Session), State: row.State, ExpiresAt: row.ExpiresAt,
		}
		if row.Kind == record.KindAgentSecret {
			entries[i].ApproveURL = s.approveURL(row.RecordID)
		}
	}
	writeJSON(w, http.StatusOK, pendingResponse{Pending: entries})
}

// pendingStates reads GET /v1/pending's repeatable ?state= values into the states the list holds,
// refusing any value but "pending" and "expired" with 400 STATE_INPUT. No value at all is
// requests.PendingStates' zero value, the list from before a record had a state, so a Dispatch
// build that sends none reads what it always read.
func pendingStates(w http.ResponseWriter, values []string) (requests.PendingStates, bool) {
	var states requests.PendingStates
	for _, value := range values {
		switch value {
		case "pending":
			states.Pending = true
		case "expired":
			states.Expired = true
		default:
			writeError(w, http.StatusBadRequest, "STATE_INPUT", `state is "pending" or "expired", and is repeated to list both`)
			return requests.PendingStates{}, false
		}
	}
	return states, true
}
