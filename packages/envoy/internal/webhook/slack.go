package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/verify"
)

func typeName(value any) string {
	text, _ := value.(string)
	return text
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

// slackAnswerBudget bounds a Slack event's publishes, counted from the handler's start. Slack takes
// an event it has no answer to within 3 s as failed and retries it, so a publish the bus does not
// answer is answered 503 inside that window, with half a second left for the answer's way back,
// rather than at the bus's own 5 s bound.
const slackAnswerBudget = 2500 * time.Millisecond

// slackMaxBody is the largest Slack event body the handler reads: 1 MiB. Slack documents no cap on
// the size of an event. It cuts a message's text at 40,000 characters
// (docs.slack.dev/reference/methods/chat.postMessage, "Truncating content"). A message event
// carries that text and its blocks, which repeat it. At six bytes a character, the length of a
// \uXXXX escape, the two come to 480,000 bytes, under half the limit.
const slackMaxBody = 1 << 20

// slackRoomWait is the most a Slack event waits, in all, for room in the listener's body budget
// (BodyBudget) before it is answered 503, which Slack retries: inside slackAnswerBudget, with
// 1.5 s of it left for the publish. It is a variable so a test can shorten it.
var slackRoomWait = time.Second

// SlackHandler returns the HTTP handler for Slack webhook events, which reads its bodies against
// bodies, the listener's one body budget. It checks every event's signature with secret before it
// parses a byte of the body, the url_verification handshake included: Slack signs that request as
// it signs every other, and its documentation says to verify it before answering the challenge
// (https://docs.slack.dev/reference/events/url_verification). So an event that does not check is
// a 401, whatever its body holds, and a handler built with an empty secret refuses every event
// (verify.Slack).
func SlackHandler(secret string, publisher Publisher, bodies *BodyBudget) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), slackAnswerBudget)
		defer cancel()
		body, release, err := bodies.readBody(ctx, w, r, r.Body, slackMaxBody, slackRoomWait)
		defer release()
		if err != nil {
			return
		}
		if !verify.Slack(secret, body, r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature")) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if typeName(payload["type"]) == "url_verification" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"challenge": payload["challenge"]})
			return
		}
		if typeName(payload["type"]) == "event_callback" && stringValue(payload["event_id"]) != "" {
			items := contracts.SlackEnvelopes(contracts.SlackEnvelopeInput{Body: payload, EventID: id.New(), TraceID: id.New()})
			for _, item := range items {
				if err := item.Validate(); err != nil {
					continue
				}
				if err := publisher.PublishEachWithin(ctx, item, nil); err != nil {
					deliveryFailed(w, "slack publish", err)
					return
				}
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
