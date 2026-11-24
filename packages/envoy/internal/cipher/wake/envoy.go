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
	"github.com/sjawhar/envoy/internal/urljoin"
	"github.com/sjawhar/envoy/listenerwire"
	"github.com/sjawhar/envoy/redirects"
)

// sendPath is the listener's route a notice is posted to, under BROKER_ENVOY_URL.
const sendPath = "/v1/messages/send"

// Envoy sends a "secret request <state>" notification directly to the target session, presenting
// the broker's own platform identity (its account, read afresh for each notice) as its bearer: the
// listener admits the account that token names, as it admits every caller's, so no bearer is
// shared between the broker and the listener's other callers.
type Envoy struct {
	url     string
	account ciphertoken.Account
	http    *http.Client
}

// NewEnvoy is the notice to the listener at url (BROKER_ENVOY_URL), presenting the token account
// reads, sent with client. It keeps a copy of client that follows no redirect (redirects.Refused),
// made once here: the notice carries the broker's account token, and the listener answers
// /v1/messages/send itself with no redirect, so a Location is the answering party's words and a
// 3xx a status Notify logs. client must not be nil.
func NewEnvoy(url string, account ciphertoken.Account, client *http.Client) Envoy {
	return Envoy{url: url, account: account, http: redirects.Refused(client)}
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
//
// sendPath is joined onto BROKER_ENVOY_URL's path (urljoin.Join), its query kept and its
// fragment, which no request sends, dropped: the path appended to that URL's text would sit
// inside a query or a fragment and the listener would be asked another route. A URL that does not
// parse or names no host, and a joined URL that does not parse again, are logged in this
// package's own words naming the setting, never url.Parse's error, which http.NewRequest's
// carries and which quotes the URL whole, user information and query included.
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
	target, err := urljoin.Join(e.url, sendPath)
	if err != nil {
		slog.Warn("wake envoy: build request", "session", sessionID, "request", requestID,
			"error", "BROKER_ENVOY_URL is not an absolute URL with a host")
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		slog.Warn("wake envoy: build request", "session", sessionID, "request", requestID,
			"error", "the URL joined from BROKER_ENVOY_URL does not parse")
		return
	}
	bearer, err := e.account.Read()
	if err != nil {
		slog.Warn("wake envoy: read the broker's account token", "session", sessionID, "request", requestID, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := e.http.Do(req)
	if err != nil {
		slog.Warn("wake envoy: send", "session", sessionID, "request", requestID, "error", redact.Cause(err, e.url))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("wake envoy: send", "session", sessionID, "request", requestID, "status", resp.StatusCode, "error", fmt.Sprintf("unexpected status %d", resp.StatusCode))
	}
}
