package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// refusedMaxPayload is the maximum payload refusingPublisher names when it refuses an envelope.
const refusedMaxPayload = 1 << 20

// refusingPublisher refuses one topic as NATS refuses an envelope past its maximum payload,
// whatever the envelope carries, and answers fail on another topic with failure until allow is
// called. It publishes every other envelope, and counts every attempt by topic.
type refusingPublisher struct {
	publishOnly
	mu       sync.Mutex
	refuse   string
	fail     string
	failure  error
	attempts map[string]int
	items    []contracts.Envelope
}

func (p *refusingPublisher) Publish(item contracts.Envelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts[item.Topic]++
	switch item.Topic {
	case p.refuse:
		// The words the bus uses for a message past the server's maximum payload (bus.Client).
		return fmt.Errorf("bus: %w: an envelope of %d bytes against the server's max payload of %d bytes",
			bus.ErrTooLarge, len(item.Payload), refusedMaxPayload)
	case p.fail:
		return fmt.Errorf("bus: publish %q: %w", item.Topic, p.failure)
	}
	p.items = append(p.items, item)
	return nil
}

func (p *refusingPublisher) WindowUntil(deadline func() time.Time) bus.Sender {
	return deadlineWindow{Sender: p, deadline: deadline}
}

func (p *refusingPublisher) PublishAsync(item contracts.Envelope) <-chan error {
	return answered(p.Publish(item))
}

// Request takes a role forward by its envelope's topic, the role's, as Publish takes a publish.
func (p *refusingPublisher) Request(_ string, item contracts.Envelope, _ bus.RequestOptions) <-chan error {
	return answered(p.Publish(item))
}

// allow lifts the failure of fail: every later publish to it is taken.
func (p *refusingPublisher) allow() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fail = ""
}

func (p *refusingPublisher) snapshot() (map[string]int, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	attempts := make(map[string]int, len(p.attempts))
	for topic, n := range p.attempts {
		attempts[topic] = n
	}
	return attempts, topicsOf(p.items)
}

type lockedBuffer = logtest.Buffer

// refusedRow is what a batch left on an event's row. It reads refused_destinations out of the
// row's JSON (to_jsonb), so on a schema without that column the read answers none rather than
// failing.
type refusedRow struct {
	destinations []string
	refused      map[string]string
	attempts     int
	nextAttempt  *time.Time
	publishedAt  *time.Time
	failure      *string
}

func readRefusedRow(t *testing.T, database *store.Store, eventID int64) refusedRow {
	t.Helper()
	var row refusedRow
	var refused []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select e.published_destinations, to_jsonb(e) -> 'refused_destinations', e.attempt_count,
		       e.next_attempt_at, e.published_at, e.publish_error
		from events e where e.id = $1
	`, eventID).Scan(&row.destinations, &refused, &row.attempts, &row.nextAttempt, &row.publishedAt, &row.failure); err != nil {
		t.Fatalf("read event %d: %v", eventID, err)
	}
	if refused != nil {
		if err := json.Unmarshal(refused, &row.refused); err != nil {
			t.Fatalf("decode the refused destinations of event %d, %s: %v", eventID, refused, err)
		}
	}
	return row
}

// loggedLines counts the lines of logged at level that carry msg and every one of attrs.
func loggedLines(logged *lockedBuffer, level, msg string, attrs ...string) int {
	count := 0
	for _, line := range strings.Split(logged.String(), "\n") {
		if !strings.Contains(line, "level="+level) || !strings.Contains(line, fmt.Sprintf("msg=%q", msg)) {
			continue
		}
		matches := true
		for _, attr := range attrs {
			matches = matches && strings.Contains(line, attr)
		}
		if matches {
			count++
		}
	}
	return count
}

// countingRefusals is deps with a publish-failure counter on a registry of its own, which
// failureReasons reads.
func countingRefusals(deps Deps) (Deps, *metrics.Registry) {
	registry := metrics.New()
	deps.PublishFailures = registry.NewCounter("envoy_dispatch_outbox_publish_failures_total",
		"Destinations of an outbox publish attempt that did not reach NATS")
	return deps, registry
}

// A destination NATS refuses is refused the same way however often the outbox sends it, so the
// event goes out to it once more with its payload left out (standInEnvelope). When NATS refuses
// that stand-in too, the destination is refused for good: the event keeps it out of
// published_destinations, which says what reached NATS, stores the subject and its error in
// refused_destinations, and is finished, with no attempt counted and no retry. The two ERROR
// lines keep their words and attributes, which an alert matches. Only a refusal
// (bus.ErrRefused) takes this path: a full stream answers that it stored nothing now, and the
// event is retried. A stand-in the bus takes is in event_share_nats_test.go, against a real NATS
// server.
func TestARefusedDestinationIsRecordedAsFailed(t *testing.T) {
	const (
		issueTopic = "notifications.dispatch.issue.T-1.message.created"
		roleTopic  = "notifications.role.reviewer"
	)
	message := func(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
		t.Helper()
		return appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
			Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: new("T-1"), Body: "Too large for NATS"},
		})
	}

	t.Run("a destination refused twice is recorded refused and the event is finished", func(t *testing.T) {
		var logged lockedBuffer
		logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
		database := storetest.Open(t)
		broker := events.NewBroker()
		route := "role:reviewer"
		seedIssue(t, database, "T-1", &route)
		event := message(t, database, broker)
		publisher := &refusingPublisher{refuse: issueTopic, attempts: map[string]int{}}
		deps, registry := countingRefusals(withHeldRoles(Deps{Store: database, Publisher: publisher, Broker: broker}))

		holding(t, deps).scan(context.Background(), unfenced{})

		attempts, published := publisher.snapshot()
		if attempts[issueTopic] != 2 {
			t.Errorf("the refused destination was attempted %d times, want twice: the event, then its stand-in", attempts[issueTopic])
		}
		if !slices.Equal(published, []string{roleTopic}) {
			t.Errorf("NATS took %v, want the role route past the refused destination", published)
		}
		row := readRefusedRow(t, database, event.ID)
		if slices.Contains(row.destinations, issueTopic) {
			t.Errorf("published_destinations %v holds %s, which NATS refused: a reader takes it for delivered", row.destinations, issueTopic)
		}
		if !slices.Equal(row.destinations, []string{roleTopic}) {
			t.Errorf("published_destinations = %v, want the role route alone", row.destinations)
		}
		reason, stored := row.refused[issueTopic]
		if !stored || !strings.Contains(reason, bus.ErrTooLarge.Error()) || !strings.Contains(reason, "bytes") {
			t.Errorf("refused_destinations = %v, want %s with the refusal naming the size", row.refused, issueTopic)
		}
		if len(row.refused) != 1 {
			t.Errorf("refused_destinations = %v, want the one refused subject", row.refused)
		}
		if row.publishedAt == nil || row.attempts != 0 || row.nextAttempt != nil || row.failure != nil {
			t.Errorf("the event has published_at %v, attempt_count %d, next_attempt_at %v and publish_error %s, want it finished: set, 0, none and none",
				row.publishedAt, row.attempts, row.nextAttempt, publishFailure(row.failure))
		}
		eventAttr, topicAttr := fmt.Sprintf("event_id=%d", event.ID), "topic="+issueTopic
		refusals := loggedLines(&logged, "ERROR", "dispatch outbox: destination refused", eventAttr, topicAttr, "error=")
		standInRefusals := loggedLines(&logged, "ERROR", "dispatch outbox: the stand-in was refused too", eventAttr, topicAttr, "error=")
		standInsOut := loggedLines(&logged, "WARN", "dispatch outbox: a stand-in went out with the payload left out", eventAttr)
		if refusals != 1 || standInRefusals != 1 || standInsOut != 0 {
			t.Errorf("logged %d refusal lines, %d stand-in refusal lines and %d stand-in-went-out lines naming the event and its topic, want one, one and none:\n%s",
				refusals, standInRefusals, standInsOut, logged.String())
		}
		// One event, one refusal: the stand-in's own refusal is the same event's, and counting it
		// again would read as two destinations NATS would not take.
		if body := failureReasons(t, registry); !strings.Contains(body, `envoy_dispatch_outbox_publish_failures_total{reason="refused"} 1`) {
			t.Errorf("/metrics body = %q, want the refused destination counted once", body)
		}
	})

	t.Run("a retry of an event with a refused and a failed destination sends the refused one no more", func(t *testing.T) {
		database := storetest.Open(t)
		broker := events.NewBroker()
		route := "role:reviewer"
		seedIssue(t, database, "T-1", &route)
		event := message(t, database, broker)
		publisher := &refusingPublisher{
			refuse: issueTopic, fail: roleTopic, failure: errors.New("the server answered with an error"),
			attempts: map[string]int{},
		}
		deps, registry := countingRefusals(withHeldRoles(Deps{Store: database, Publisher: publisher, Broker: broker}))

		holding(t, deps).scan(context.Background(), unfenced{})

		row := readRefusedRow(t, database, event.ID)
		if row.publishedAt != nil || row.attempts != 1 || row.nextAttempt == nil || row.failure == nil || !strings.Contains(*row.failure, "role:reviewer") {
			t.Fatalf("the event has published_at %v, attempt_count %d, next_attempt_at %v and publish_error %s, want it backed off for its failed role route",
				row.publishedAt, row.attempts, row.nextAttempt, publishFailure(row.failure))
		}
		if len(row.destinations) != 0 {
			t.Errorf("published_destinations = %v, want none: one destination was refused and the other failed", row.destinations)
		}
		if _, stored := row.refused[issueTopic]; !stored {
			t.Errorf("refused_destinations = %v, want %s stored while the event backs off", row.refused, issueTopic)
		}

		publisher.allow()
		dueNow(t, database, event.ID)
		holding(t, deps).scan(context.Background(), unfenced{})

		attempts, published := publisher.snapshot()
		if attempts[issueTopic] != 2 {
			t.Errorf("the refused destination was attempted %d times, want twice, both in the first attempt: the retry sends it no more", attempts[issueTopic])
		}
		if !slices.Equal(published, []string{roleTopic}) {
			t.Errorf("NATS took %v, want the role route once, on the retry", published)
		}
		row = readRefusedRow(t, database, event.ID)
		if row.publishedAt == nil || !slices.Equal(row.destinations, []string{roleTopic}) {
			t.Errorf("after the retry the event has published_at %v and published_destinations %v, want it published to the role route alone",
				row.publishedAt, row.destinations)
		}
		if _, stored := row.refused[issueTopic]; !stored || len(row.refused) != 1 {
			t.Errorf("after the retry refused_destinations = %v, want %s alone", row.refused, issueTopic)
		}
		if body := failureReasons(t, registry); !strings.Contains(body, `envoy_dispatch_outbox_publish_failures_total{reason="refused"} 1`) {
			t.Errorf("/metrics body = %q, want the refusal counted once across both attempts", body)
		}
	})

	t.Run("a full stream is retried", func(t *testing.T) {
		var logged lockedBuffer
		logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
		database := storetest.Open(t)
		broker := events.NewBroker()
		seedIssue(t, database, "T-1", nil)
		event := message(t, database, broker)
		publisher := &refusingPublisher{fail: issueTopic, failure: bus.ErrStreamFull, attempts: map[string]int{}}
		deps, registry := countingRefusals(withHeldRoles(Deps{Store: database, Publisher: publisher, Broker: broker}))

		holding(t, deps).scan(context.Background(), unfenced{})

		row := readRefusedRow(t, database, event.ID)
		if row.publishedAt != nil || row.attempts != 1 || row.nextAttempt == nil || row.failure == nil ||
			!strings.Contains(*row.failure, bus.ErrStreamFull.Error()) {
			t.Fatalf("the event has published_at %v, attempt_count %d, next_attempt_at %v and publish_error %s, want it backed off naming %v",
				row.publishedAt, row.attempts, row.nextAttempt, publishFailure(row.failure), bus.ErrStreamFull)
		}
		if len(row.destinations) != 0 || len(row.refused) != 0 {
			t.Errorf("published_destinations = %v and refused_destinations = %v, want neither to hold the full stream's topic",
				row.destinations, row.refused)
		}
		if n := loggedLines(&logged, "ERROR", "dispatch outbox: destination refused"); n != 0 {
			t.Errorf("logged %d refusal lines for a full stream, want none:\n%s", n, logged.String())
		}
		body := failureReasons(t, registry)
		if !strings.Contains(body, `envoy_dispatch_outbox_publish_failures_total{reason="failed"} 1`) || strings.Contains(body, `reason="refused"`) {
			t.Errorf("/metrics body = %q, want the full stream counted a failure and no refusal", body)
		}

		publisher.allow()
		dueNow(t, database, event.ID)
		holding(t, deps).scan(context.Background(), unfenced{})

		row = readRefusedRow(t, database, event.ID)
		if row.publishedAt == nil || !slices.Equal(row.destinations, []string{issueTopic}) || len(row.refused) != 0 {
			t.Errorf("after the retry the event has published_at %v, published_destinations %v and refused_destinations %v, want it published to %s",
				row.publishedAt, row.destinations, row.refused, issueTopic)
		}
	})

	t.Run("a refused fallback copy is settled unpublished and not claimed again", func(t *testing.T) {
		const sessionTopic = "notifications.agent.s1"
		var logged lockedBuffer
		logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
		ctx := store.WithTransactionTracking(context.Background())
		database := storetest.Open(t)
		publisher := &refusingPublisher{refuse: sessionTopic, attempts: map[string]int{}}
		deps, registry := countingRefusals(Deps{Store: database, Publisher: publisher, Broker: events.NewBroker()})
		commentID := seedHeldFallbackCopy(ctx, t, deps)
		if attempts, _ := publisher.snapshot(); attempts[sessionTopic] != 0 {
			t.Fatalf("the comment's scan sent %d copies to %s, want none: the fallback row holds that copy", attempts[sessionTopic], sessionTopic)
		}

		publishFallbackCopies(ctx, deps, unfenced{}, holdingProcess(t, database))

		row := readHeldCopy(t, database, commentID, sessionTopic)
		if !row.settled || row.published || row.attempts != 0 {
			t.Errorf("the refused copy's row has settled=%v published=%v attempts=%d, want it settled, unpublished, with no attempt counted",
				row.settled, row.published, row.attempts)
		}
		if n := loggedLines(&logged, "ERROR", "dispatch outbox: fallback copy refused", "comment_id="+commentID, "destination="+sessionTopic, "error="); n != 1 {
			t.Errorf("logged %d fallback refusal lines naming the comment and the destination, want one:\n%s", n, logged.String())
		}
		if body := failureReasons(t, registry); !strings.Contains(body, `envoy_dispatch_outbox_publish_failures_total{reason="refused"} 1`) {
			t.Errorf("/metrics body = %q, want the refused copy counted once", body)
		}

		// A later pass, with every unsettled row due, claims nothing: the settled row is done.
		if _, err := database.Pool.Exec(ctx, `
			update comment_fallback_copies set next_attempt_at = now() - interval '1 second'
			where comment_id = $1 and settled_at is null
		`, commentID); err != nil {
			t.Fatalf("make every unsettled row due: %v", err)
		}
		publishFallbackCopies(ctx, deps, unfenced{}, holdingProcess(t, database))
		if attempts, _ := publisher.snapshot(); attempts[sessionTopic] != 1 {
			t.Errorf("the refused copy was attempted %d times over two passes, want once: a settled row is not claimed again", attempts[sessionTopic])
		}
	})
}

// standInRecorder refuses the first envelope of each event as NATS refuses one past its maximum
// payload, and takes every later one, the event's stand-in, recording both by event id.
type standInRecorder struct {
	publishOnly
	mu       sync.Mutex
	refused  map[string]contracts.Envelope
	standIns map[string][]contracts.Envelope
}

func (p *standInRecorder) WindowUntil(deadline func() time.Time) bus.Sender {
	return deadlineWindow{Sender: p, deadline: deadline}
}

func (p *standInRecorder) PublishAsync(item contracts.Envelope) <-chan error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, seen := p.refused[item.EventID]; !seen {
		p.refused[item.EventID] = item
		return answered(fmt.Errorf("bus: %w: an envelope of %d bytes against the server's max payload of %d bytes",
			bus.ErrTooLarge, len(item.Payload), refusedMaxPayload))
	}
	p.standIns[item.EventID] = append(p.standIns[item.EventID], item)
	return answered(nil)
}

// sent is the envelope NATS refused for eventID and the envelopes it took after it.
func (p *standInRecorder) sent(eventID string) (contracts.Envelope, []contracts.Envelope) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refused[eventID], slices.Clone(p.standIns[eventID])
}

// Every event NATS refuses gets a stand-in, whatever its payload is. A payload that is not a JSON
// object has no field to keep, so its stand-in's payload is {"payload_omitted":true}; before, such
// an event got no stand-in at all and its readers heard nothing. The stand-in keeps the refused
// envelope's topic, event id and payload_summary, and an object payload keeps its short fields.
func TestAStandInIsAlwaysMade(t *testing.T) {
	const issueTopic = "notifications.dispatch.issue.T-1.issue.updated"
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	cases := []struct {
		name    string
		payload any
		want    map[string]any
	}{
		{name: "an array payload", payload: []any{"first", "second"}, want: map[string]any{"payload_omitted": true}},
		{name: "a string payload", payload: "a renamed issue", want: map[string]any{"payload_omitted": true}},
		{name: "a number payload", payload: 42, want: map[string]any{"payload_omitted": true}},
		{name: "a boolean payload", payload: true, want: map[string]any{"payload_omitted": true}},
		{name: "a null payload", payload: nil, want: map[string]any{"payload_omitted": true}},
		{
			name:    "an object payload keeps its short fields",
			payload: map[string]any{"id": "issue-update-1", "title": "Renamed", "body": strings.Repeat("a", 300)},
			want:    map[string]any{"id": "issue-update-1", "title": "Renamed", "payload_omitted": true},
		},
	}
	ids := make([]string, len(cases))
	for index, tc := range cases {
		event := appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "issue.updated", Actor: model.Actor{Kind: "user", ID: "alice"}, Payload: tc.payload,
		})
		ids[index] = "dispatch-" + strconv.FormatInt(event.ID, 10)
	}
	publisher := &standInRecorder{refused: map[string]contracts.Envelope{}, standIns: map[string][]contracts.Envelope{}}

	holding(t, Deps{Store: database, Publisher: publisher, Broker: broker}).scan(context.Background(), unfenced{})

	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refused, standIns := publisher.sent(ids[index])
			if refused.EventID != ids[index] {
				t.Fatalf("NATS was never sent event %s", ids[index])
			}
			if len(standIns) != 1 {
				t.Fatalf("event %s was sent %d stand-ins after NATS refused it, want one", ids[index], len(standIns))
			}
			standIn := standIns[0]
			if standIn.Topic != issueTopic || standIn.EventID != ids[index] || standIn.PayloadSummary != refused.PayloadSummary {
				t.Errorf("the stand-in has topic %q, event id %q and payload_summary %q, want the refused envelope's %q, %q and %q",
					standIn.Topic, standIn.EventID, standIn.PayloadSummary, issueTopic, ids[index], refused.PayloadSummary)
			}
			var carried struct {
				Payload map[string]any `json:"payload"`
			}
			if err := json.Unmarshal([]byte(standIn.Payload), &carried); err != nil {
				t.Fatalf("decode the stand-in's event %s: %v", standIn.Payload, err)
			}
			if !reflect.DeepEqual(carried.Payload, tc.want) {
				t.Errorf("the stand-in's payload is %v, want %v", carried.Payload, tc.want)
			}
		})
	}
}
