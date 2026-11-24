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
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/listenerwire"
	"github.com/sjawhar/envoy/redirects"
)

// Envoy sends a "secret request <state>" notification directly to the target session, presenting
// the broker's own platform identity (Account, read afresh for each notice) as its bearer: the
// listener admits the account that token names, as it admits every caller's, so no bearer is
// shared between the broker and the listener's other callers.
type Envoy struct {
	URL     string
	Account ciphertoken.Account
	// HTTP sends the notice, as a copy that follows no redirect (redirects.Refused): the notice
	// carries the broker's account token, and the listener answers /v1/messages/send itself with
	// no redirect, so a Location is the answering party's words and a 3xx a status Notify logs.
	HTTP *http.Client
}

type secretRequestPayload struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	State     string `json:"state"`
}

// Notify tells sessionID that requestID reached state. Any failure — building the request,
// reaching Envoy, or a non-2xx response (including a 404 for a session that is no longer live) —
// is logged with slog.Warn and otherwise ignored: the caller has already applied the decision
// and this is only a best-effort nudge.
func (e Envoy) Notify(ctx context.Context, sessionID, requestID, state string) {
	payload, err := json.Marshal(secretRequestPayload{
		Type:      "secret-request",
		RequestID: requestID,
		State:     state,
	})
	if err != nil {
		slog.Warn("wake envoy: marshal payload", "session", sessionID, "request", requestID, "error", err)
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
		slog.Warn("wake envoy: marshal body", "session", sessionID, "request", requestID, "error", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL+"/v1/messages/send", bytes.NewReader(body))
	if err != nil {
		slog.Warn("wake envoy: build request", "session", sessionID, "request", requestID, "error", err)
		return
	}
	bearer, err := e.Account.Read()
	if err != nil {
		slog.Warn("wake envoy: read the broker's account token", "session", sessionID, "request", requestID, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := redirects.Refused(e.HTTP).Do(req)
	if err != nil {
		// net/http's error can quote the answering party's words (a Location that does not parse,
		// read before any redirect policy), so it is logged as Envoy's address allows
		// (redact.Cause): a transport failure's own text, or its class.
		slog.Warn("wake envoy: send", "session", sessionID, "request", requestID, "error", redact.Cause(err, e.URL))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("wake envoy: send", "session", sessionID, "request", requestID, "status", resp.StatusCode, "error", fmt.Sprintf("unexpected status %d", resp.StatusCode))
	}
}
