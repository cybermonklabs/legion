package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

// SlackPrivacy is what the Slack intake reads at each event in the listener's required privacy
// mode (cmd/listener's listenerPrivacy): which private projects list a channel, from one fresh
// read of the privacy source, and the count and the line of an event published on no lane.
type SlackPrivacy interface {
	// SlackChannelProjects is the private projects whose slack_channels list channel, read
	// afresh within ctx; an error is an answer that is not known, on which nothing is published.
	SlackChannelProjects(ctx context.Context, channel string) ([]string, error)
	// SlackWithheld counts and logs an event of channel that projects all list, which is
	// published on no lane.
	SlackWithheld(channel string, projects []string)
}

// SlackOption configures SlackHandler.
type SlackOption func(*slackHandlerOptions)

type slackHandlerOptions struct {
	privacy SlackPrivacy
}

// WithSlackPrivacy has the intake decide each event from privacy: an event whose channel, trimmed
// and upper-cased, exactly one private project lists is published on that project's own Slack
// lane (contracts.PrivateSlackTopic), its thread copy too, and nothing of it on
// notifications.slack.>; a channel two projects list is acknowledged and published on no lane; any
// read that does not answer is the 503 Slack retries, with nothing published; and every other
// event is published as without the option.
func WithSlackPrivacy(privacy SlackPrivacy) SlackOption {
	return func(o *slackHandlerOptions) { o.privacy = privacy }
}

// SlackHandler returns the HTTP handler for Slack webhook events.
func SlackHandler(secret string, publisher Publisher, options ...SlackOption) http.HandlerFunc {
	var opts slackHandlerOptions
	for _, option := range options {
		option(&opts)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), slackAnswerBudget)
		defer cancel()
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		// URL verification must happen BEFORE signature check.
		if typeName(payload["type"]) == "url_verification" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"challenge": payload["challenge"]})
			return
		}
		if !verify.Slack(secret, body, r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature")) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		if typeName(payload["type"]) == "event_callback" && stringValue(payload["event_id"]) != "" {
			items := contracts.SlackEnvelopes(contracts.SlackEnvelopeInput{Body: payload, EventID: id.New(), TraceID: id.New()})
			if opts.privacy != nil {
				var withheld bool
				if items, withheld = privateSlackLane(ctx, w, opts.privacy, payload, items); withheld {
					return
				}
			}
			for _, item := range items {
				if err := item.Validate(); err != nil {
					continue
				}
				if err := publisher.PublishWithin(ctx, item); err != nil {
					deliveryFailed(w, "slack publish", err)
					return
				}
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}

// privateSlackLane decides an event's envelopes from one fresh privacy read (WithSlackPrivacy):
// the envelopes moved to the one private project that lists the event's channel, or as they are
// for a channel none lists. answered is true when it has answered the request itself: the 503 of
// a read that did not answer, or the 200 of a channel two projects list, published on no lane.
func privateSlackLane(ctx context.Context, w http.ResponseWriter, privacy SlackPrivacy, payload map[string]any, items []contracts.Envelope) (_ []contracts.Envelope, answered bool) {
	event, _ := payload["event"].(map[string]any)
	channel := strings.ToUpper(strings.TrimSpace(stringValue(event["channel"])))
	projects, err := privacy.SlackChannelProjects(ctx, channel)
	switch {
	case err != nil:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "slack event not published: the project privacy answer could not be read", http.StatusServiceUnavailable)
		return nil, true
	case len(projects) > 1:
		privacy.SlackWithheld(channel, projects)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return nil, true
	case len(projects) == 0:
		return items, false
	}
	moved := make([]contracts.Envelope, 0, len(items))
	for _, item := range items {
		topic, ok := contracts.PrivateSlackTopic(projects[0], item.Topic)
		if !ok {
			continue
		}
		item.Topic = topic
		moved = append(moved, item)
	}
	return moved, false
}
