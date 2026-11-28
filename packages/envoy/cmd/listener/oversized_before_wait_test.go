package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/listenerwire"
)

// A message past the bus's max payload is answered 413 before anything waits, on both message
// routes, whatever the request's deadline (bus.Client.EnvelopeRefusal). Before, each route found
// the size only at its publish, after reads that share the request's deadline: the target's
// session read, the sender stamp's reads, a role's resolution. Under load those can spend the
// deadline - a large body read slowly spends it before any of them - and the route then answered
// the 503 of a bus that did not answer, with Retry-After: 1, so the sender sent the same bytes
// again. TestSendAnswersServiceUnavailableWhenTheBusRefusesThePublish met it in CI. Each row
// serves its request with a context that has already ended, the extreme of a deadline spent
// before the handler got to the bus, and wants the 413 with no Retry-After and the record's
// too_large outcome:
//   - a send to a session no cache holds, whose route would read the session bucket first;
//   - a send to a session the cache holds, whose route would stamp and publish;
//   - a publish to a team topic, which would stamp and publish;
//   - a publish to a role lane, which would resolve the role's holder first.
//
// The control is each request with a short message under the same ended context: it is the 503
// of a bus that did not answer, with Retry-After: 1, so the size check answers only the size.
func TestAMessagePastTheMaxPayloadIsAnswered413BeforeAnythingWaits(t *testing.T) {
	t.Parallel()
	const live, unknown = "ses_oversized_live_target", "ses_oversized_uncached_target"
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, []string{live})
	state := withPublishRoute(t, &listenerDeps{client: client, registry: registry, sessions: sessions})
	maxPayload := int(client.Conn.MaxPayload())
	if maxPayload <= 0 {
		t.Fatalf("the server announced a max payload of %d bytes", maxPayload)
	}
	oversized := strings.Repeat("a", maxPayload+1)
	ended, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	send := func(logger *logging.Logger) http.HandlerFunc { return sendHandler(state, logger) }
	publish := func(logger *logging.Logger) http.HandlerFunc { return publishHandler(state, logger) }
	for _, row := range []struct {
		name  string
		path  string
		route func(*logging.Logger) http.HandlerFunc
		body  func(message string) map[string]any
	}{
		{"a send to a session no cache holds", "/v1/messages/send", send, func(message string) map[string]any {
			return map[string]any{"target_session": unknown, "source": "agent", "source_session": "ses_oversized_sender", "message": message}
		}},
		{"a send to a session the cache holds", "/v1/messages/send", send, func(message string) map[string]any {
			return map[string]any{"target_session": live, "source": "agent", "source_session": "ses_oversized_sender", "message": message}
		}},
		{"a publish to a team topic", "/v1/messages/publish", publish, func(message string) map[string]any {
			return map[string]any{"topic": "notifications.team.oversized-squad", "source": "agent", "source_session": "ses_oversized_sender", "message": message}
		}},
		{"a publish to a role lane", "/v1/messages/publish", publish, func(message string) map[string]any {
			return map[string]any{"topic": contracts.RoleTopicPrefix + "oversized-reviewer", "source": "agent", "source_session": "ses_oversized_sender", "message": message}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			answer, record := serveRecorded(t, row.route, postJSON(row.path, sendBody(row.body(oversized))).WithContext(ended))
			if answer.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("an oversized message under an ended deadline: status %d, body %s; want 413", answer.Code, answer.Body.String())
			}
			var refusal listenerwire.APIError
			if err := json.Unmarshal(answer.Body.Bytes(), &refusal); err != nil || !strings.Contains(refusal.Error, bus.ErrTooLarge.Error()) {
				t.Fatalf("the 413's body %s does not name %q (%v)", answer.Body.String(), bus.ErrTooLarge, err)
			}
			if got := answer.Header().Get("Retry-After"); got != "" {
				t.Fatalf("the 413 carries Retry-After %q: the same bytes are refused however often they are sent", got)
			}
			assertRecordFields(t, record, map[string]any{"level": "WARN", "outcome": "too_large", "status": float64(http.StatusRequestEntityTooLarge)})

			control, controlRecord := serveRecorded(t, row.route, postJSON(row.path, sendBody(row.body("a short message"))).WithContext(ended))
			if control.Code != http.StatusServiceUnavailable || control.Header().Get("Retry-After") != "1" {
				t.Fatalf("a short message under the same ended deadline: status %d, Retry-After %q, body %s; want the bus-unavailable 503 with Retry-After: 1",
					control.Code, control.Header().Get("Retry-After"), control.Body.String())
			}
			if controlRecord["outcome"] == "too_large" {
				t.Fatalf("a short message was recorded too_large: %v", controlRecord)
			}
		})
	}
}
