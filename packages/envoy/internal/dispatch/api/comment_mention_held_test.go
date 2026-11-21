package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
// reads the rule at most once and sends nothing else: the test steps the lane one try at a time,
// holding each park's timer and running it itself, and a traced pool sees every statement the
// server sends between one park and the next. Each must be, byte for byte, the rule's own
// statement (model.MentionCopyRuleSQL), at most one a try; any other statement fails the test,
// whatever it reads, so a second read of the rule spelled another way fails as a second read
// spelled the same way does. The send was decided held on a read of the rule: at least one is sent
// between the comment and the first WARN. Once the topic family is captured again the send lands,
// the gauges read none and no second WARN comes. The controls are the scrape before the comment and
// the one after the send landed. The test's waits for the lane are its liveness alone
// (heldLiveness), never the bound under test.
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
	// The sender's park timer: until the test steps the lane, each park's timer starts as the
	// sender starts it; from then each park hands its timer's run to the test instead.
	parked := make(chan func(), 4)
	var stepping atomic.Bool
	served.mentions.mu.Lock()
	served.mentions.heldWarnAfter = time.Second
	own := served.mentions.after
	served.mentions.after = func(delay time.Duration, f func()) {
		if stepping.Load() {
			parked <- f
			return
		}
		own(delay, f)
	}
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
	posted := len(recorder.Statements())
	mention := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "the comment that mentions s1", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	const warning = `level=WARN msg="dispatch: a comment mention's send waits behind an earlier event of its owner"`
	waitUntil(t, heldLiveness(), "the held send counted and reported", func() bool {
		return strings.Contains(scrape(t, registry), "envoy_dispatch_mention_sends_held 1\n") &&
			strings.Contains(logs.String(), warning)
	})
	decidedOn := 0
	for _, statement := range recorder.Statements()[posted:] {
		if statement == model.MentionCopyRuleSQL {
			decidedOn++
		}
	}
	if decidedOn == 0 {
		t.Errorf("the send was counted held with no read of the rule since the comment, want at least one")
	}

	// The parked lane stepped one try at a time: each park's timer is held and the test runs it,
	// so the statements between one park and the next are one try's, with nothing else of the
	// lane in flight.
	stepping.Store(true)
	unpark := nextPark(t, parked)
	var readsPerTry []int
	var unrecognised []string
	for range 3 {
		before := len(recorder.Statements())
		unpark()
		unpark = nextPark(t, parked)
		reads := 0
		for _, statement := range recorder.Statements()[before:] {
			if statement == model.MentionCopyRuleSQL {
				reads++
				continue
			}
			unrecognised = append(unrecognised, statement)
		}
		readsPerTry = append(readsPerTry, reads)
	}
	// Scraped while the lane is parked: the gauges leave out a held lane's head while a try of it
	// is in flight, which the run of the last timer would start.
	body := scrape(t, registry)
	stepping.Store(false)
	unpark()
	t.Logf("%d reads of the rule from the comment to its first WARN; three tries of the parked send: reads of the rule %v, %d other statements; gauges:\n%s",
		decidedOn, readsPerTry, len(unrecognised), heldLines(body))
	for try, reads := range readsPerTry {
		if reads > 1 {
			t.Errorf("try %d of the parked send read the rule %d times, want at most once", try+1, reads)
		}
	}
	for _, statement := range unrecognised {
		t.Errorf("a try of the parked send sent a statement that is not the rule's own: %q", firstChars(statement, 160))
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
	waitUntil(t, heldLiveness(), "the held send landed", func() bool {
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

// heldLiveness is how long the held-send test waits for the next thing its parked lane does: a
// park, the first held WARN, the send landing. It is liveness alone, never the bound under test,
// so it grows with the sender's own waits (mentionOrderRetryFirst, mentionOrderRetryMax): a
// schedule of longer waits reads the rule less, which the cost requirement allows, and must not
// time the test out.
func heldLiveness() time.Duration {
	return 30*time.Second + 2*(mentionOrderRetryFirst+mentionOrderRetryMax)
}

// nextPark waits for the sender's next park and answers its timer's run, which the test makes.
// Its bound is the test's liveness alone (heldLiveness): a try takes milliseconds, and a park the
// sender started before the test held the timers waits at most its own delay.
func nextPark(t *testing.T, parked <-chan func()) func() {
	t.Helper()
	select {
	case unpark := <-parked:
		return unpark
	case <-time.After(heldLiveness()):
		t.Fatalf("the held send did not park again within %v", heldLiveness())
		return nil
	}
}

// firstChars is statement's first n characters, its runs of white space each one space.
func firstChars(statement string, n int) string {
	text := strings.Join(strings.Fields(statement), " ")
	if len(text) > n {
		return text[:n] + "..."
	}
	return text
}
