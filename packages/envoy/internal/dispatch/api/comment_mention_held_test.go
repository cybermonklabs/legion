package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// scrape answers registry's /metrics body.
func scrape(t *testing.T, registry *metrics.Registry) string {
	t.Helper()
	response := httptest.NewRecorder()
	registry.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return response.Body.String()
}

// A mention's send that waits behind an earlier event shows, since nothing bounds the wait: the
// server's gauges count it and read how long the oldest has waited, naming no owner, since
// /metrics answers whoever reaches the port; and past the warn threshold - shortened here to a
// second - the sender logs one WARN naming the owner, the event it waits behind and that event's
// last publish error. The cause is logged too: the outbox's own ERROR for the earlier event's
// failed topic send names that event and its topic. While it waits, each try of the parked lane
// reads the rule alone: no claim, resolution or settle of the attempt is written (a traced pool
// counts the server's statements). Once the topic family is captured again the send lands, the
// gauges read none and no second WARN comes. The controls are the scrape before the comment and
// the one after the send landed.
func TestAHeldMentionSendIsCountedAndReportedOnce(t *testing.T) {
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
	database := storetest.Open(t)
	recorder := &storetest.StatementRecorder{}
	registry := metrics.New()
	listener, state := newMentionListener(t)
	handler, _, deps, served := newRegisteredTestServer(t, testServerOptions{
		envoyURL: listener.URL, envoyTimeout: 30 * time.Second, outboxPublishes: true, metrics: registry,
		store: storetest.TracedStore(t, database, recorder),
	})
	served.mentions.mu.Lock()
	served.mentions.heldWarnAfter = time.Second
	served.mentions.mu.Unlock()
	issue := createInteractionIssue(t, handler, "TEST", "A held mention", "before")
	view := &sessionView{issue: issue.Key}
	state.mu.Lock()
	state.landed = view.mentionLanded
	state.mu.Unlock()
	// The outbox runs on a pool of its own, as cmd/dispatch's does, so the traced pool counts the
	// server's statements alone.
	outboxDeps := deps
	outboxDeps.Store = database
	runOutboxBeside(t, outboxDeps, view)
	waitUntil(t, 10*time.Second, "the issue's first events published", func() bool {
		return unpublishedEvents(t, database, issue.Key) == 0
	})
	if body := scrape(t, registry); !strings.Contains(body, "envoy_dispatch_mention_sends_held 0\n") {
		t.Fatalf("the gauges read %q before any mention, want no held send", heldLines(body))
	}

	view.refuseTopics(true)
	firstComment := decodeMentionedComment(t, sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "event 1, by a session", "actor": map[string]any{"kind": "session", "id": "s2"},
	}))
	first := commentEventID(t, database, firstComment.ID)
	waitUntil(t, 10*time.Second, "event 1's topic send backed off", func() bool {
		attempts, _ := eventAttempts(t, database, first)
		return attempts > 0
	})
	mention := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "the comment that mentions s1", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	const warning = `level=WARN msg="dispatch: a comment mention's send waits behind an earlier event of its owner"`
	waitUntil(t, 10*time.Second, "the held send counted and reported", func() bool {
		return strings.Contains(scrape(t, registry), "envoy_dispatch_mention_sends_held 1\n") &&
			strings.Contains(logs.String(), warning)
	})

	// Three seconds parked: count what the server sends the database meanwhile.
	before := len(recorder.Statements())
	time.Sleep(3 * time.Second)
	window := recorder.Statements()[before:]
	var reads, attemptWrites int
	for _, statement := range window {
		switch {
		case strings.Contains(statement, "k.owed_keys &&"):
			reads++
		case strings.Contains(statement, "comment_deliveries"):
			attemptWrites++
		}
	}
	body := scrape(t, registry)
	t.Logf("over 3 s parked: %d reads of the rule, %d statements on comment_deliveries, %d statements in all; gauges:\n%s",
		reads, attemptWrites, len(window), heldLines(body))
	if reads == 0 || reads > 4 {
		t.Errorf("the parked lane read the rule %d times over 3 s, want one each try at mentionOrderRetryMax (%v) once it ramps up", reads, mentionOrderRetryMax)
	}
	if attemptWrites != 0 {
		t.Errorf("the parked lane made %d statements on comment_deliveries over 3 s, want none: a try of a held send reads the rule alone", attemptWrites)
	}
	if !strings.Contains(body, "envoy_dispatch_mention_send_held_oldest_seconds ") ||
		strings.Contains(body, "envoy_dispatch_mention_send_held_oldest_seconds 0\n") {
		t.Errorf("the gauges read %q, want the held send's age past zero seconds", heldLines(body))
	}
	if lines := heldLines(body); strings.Contains(lines, "{") || strings.Contains(lines, issue.Key) {
		t.Errorf("the gauges read %q, want no label and no owner's key: /metrics answers whoever reaches the port", lines)
	}
	logged := logs.String()
	if got := strings.Count(logged, warning); got != 1 {
		t.Errorf("%d WARN lines for the held send, want one", got)
	}
	for _, want := range []string{"owner=" + issue.Key, fmt.Sprintf("event_id=%d", first), "session=s1",
		"publish_error=", "notifications.dispatch.issue." + issue.Key + ".comment.created"} {
		if !strings.Contains(warningLine(logged, warning), want) {
			t.Errorf("the WARN line does not name %q: %s", want, warningLine(logged, warning))
		}
	}
	// The cause: the outbox's ERROR for event 1's topic send, naming the event and its topic.
	cause := warningLine(logged, `level=ERROR msg="dispatch outbox: publish event"`)
	if !strings.Contains(cause, fmt.Sprintf("event_id=%d", first)) || !strings.Contains(cause, "publish owner topic") {
		t.Errorf("the outbox's ERROR for event 1's failed topic send is %q, want one naming event %d and its owner topic", cause, first)
	}

	view.refuseTopics(false)
	waitUntil(t, 30*time.Second, "the held send landed", func() bool {
		attempts := waitForCommentDeliveries(t, database, mention.ID, "the mention's attempts", func([]model.CommentDelivery) bool { return true })
		return len(attempts) == 1 && attempts[0].State == "sent"
	})
	if body := scrape(t, registry); !strings.Contains(body, "envoy_dispatch_mention_sends_held 0\n") {
		t.Fatalf("the gauges read %q once the send landed, want no held send", heldLines(body))
	}
	if got := strings.Count(logs.String(), warning); got != 1 {
		t.Fatalf("%d WARN lines in all, want the one", got)
	}
}

// heldLines is the held gauges' lines of a /metrics body.
func heldLines(body string) string {
	var lines []string
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, "envoy_dispatch_mention_send") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// warningLine is the first line of logged holding marker, "" for none.
func warningLine(logged, marker string) string {
	for line := range strings.SplitSeq(logged, "\n") {
		if strings.Contains(line, marker) {
			return line
		}
	}
	return ""
}
