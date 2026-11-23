// Package wake is the broker's one best-effort side channel: a POST to Envoy's
// notifications.agent.<sessionID> subject via /v1/messages/send so a session already watching
// for wake events knows a secret decision landed. /v1/messages/publish rejects any
// notifications.agent.* topic outright (cmd/listener/api.go's publishHandler:
// "cannot publish to agent topics; use /v1/messages/send for direct agent messages"), so this
// targets the session directly with target_session instead of publishing to that topic. The
// body is the listener's own (listenerwire.SendRequest). The envelope's source is "envoy" — the
// platform's own enum (contracts.Envelope.Validate) has no "agent-secrets" entry, and the broker
// is a backend service speaking on the platform's behalf here, not an agent session, so "agent"
// would misrepresent it. Notify never reports failure to its caller and never blocks the
// decision it announces on Envoy being reachable — the client's own status poll is always the
// authority, this is only a nudge to check sooner.
package wake

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/listenerwire"
)

// Envoy sends a "secret request <state>" notification directly to the target session, presenting
// the broker's own platform identity (Account, read afresh for each notice) as its bearer: the
// listener admits the account that token names, as it admits every caller's, so no bearer is
// shared between the broker and the listener's other callers. Log is where a notice that failed
// is logged; nil is slog.Default(), read once at the start of each Notify, since an Envoy is a
// value built where it is used and has no constructor.
type Envoy struct {
	URL     string
	Account ciphertoken.Account
	HTTP    *http.Client
	Log     *slog.Logger
}

type secretRequestPayload struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	State     string `json:"state"`
}

// Notify tells sessionID that requestID reached state. Any failure — building the request,
// reaching Envoy, or a non-2xx response (including a 404 for a session that is no longer live) —
// is logged at WARN to e.Log and otherwise ignored: the caller has already applied the decision
// and this is only a best-effort nudge.
func (e Envoy) Notify(ctx context.Context, sessionID, requestID, state string) {
	log := cmp.Or(e.Log, slog.Default())
	payload, err := json.Marshal(secretRequestPayload{
		Type:      "secret-request",
		RequestID: requestID,
		State:     state,
	})
	if err != nil {
		log.Warn("wake envoy: marshal payload", "session", sessionID, "request", requestID, "error", err)
		return
	}
	payloadText := string(payload)
	body, err := json.Marshal(listenerwire.SendRequest{
		MessageBody: listenerwire.MessageBody{
			Source:  "envoy",
			Message: "secret request " + state,
			Payload: &payloadText,
		},
		TargetSession: sessionID,
	})
	if err != nil {
		log.Warn("wake envoy: marshal body", "session", sessionID, "request", requestID, "error", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL+"/v1/messages/send", bytes.NewReader(body))
	if err != nil {
		log.Warn("wake envoy: build request", "session", sessionID, "request", requestID, "error", err)
		return
	}
	bearer, err := e.Account.Read()
	if err != nil {
		log.Warn("wake envoy: read the broker's account token", "session", sessionID, "request", requestID, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := e.HTTP.Do(req)
	if err != nil {
		log.Warn("wake envoy: send", "session", sessionID, "request", requestID, "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Warn("wake envoy: send", "session", sessionID, "request", requestID, "status", resp.StatusCode, "error", fmt.Sprintf("unexpected status %d", resp.StatusCode))
	}
}
