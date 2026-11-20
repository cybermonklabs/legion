package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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
// /metrics answers whoever reaches the port with no credential and an owner label would have no
// bound, one series for each issue, document or project with a held send; and past the warn
// threshold - shortened here to a second - the sender logs one WARN naming the owner, the event it
// waits behind and that event's last publish error. The cause is logged too: the outbox's own
// ERROR for the earlier event's failed topic send names that event and its topic. While it waits,
// each try of the parked lane reads the rule alone: no claim, resolution or settle of the attempt
// is written (a traced pool counts the server's statements). Once the topic family is captured
// again the send lands, the gauges read none and no second WARN comes. The controls are the
// scrape before the comment and the one after the send landed.
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
	const warnAfter = time.Second
	served.mentions.mu.Lock()
	served.mentions.heldWarnAfter = warnAfter
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
	posted := time.Now()
	mention := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "the comment that mentions s1", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	const warning = `level=WARN msg="dispatch: a comment mention's send waits behind an earlier event of its owner"`
	waitUntil(t, 10*time.Second, "the held send counted and reported", func() bool {
		return strings.Contains(scrape(t, registry), "envoy_dispatch_mention_sends_held 1\n") &&
			strings.Contains(logs.String(), warning)
	})
	warned := time.Now()

	// Parked for three seconds or more, until the lane has read the rule once: count what the
	// server sends the database meanwhile. A try reads the rule when its timer fires, which a loaded
	// runner can delay past any fixed window, so the window waits for that read rather than ending
	// on the clock. Load only slows the tries, so the most reads a window can hold - one each try,
	// the tries at least mentionOrderRetryMax apart once they ramp up - holds however long it runs.
	opened := time.Now()
	before := len(recorder.Statements())
	var reads, attemptWrites int
	var window []string
	waitUntil(t, 10*time.Second, "three seconds parked and a read of the rule", func() bool {
		window = recorder.Statements()[before:]
		reads, attemptWrites = 0, 0
		for _, statement := range window {
			switch {
			case strings.Contains(statement, "k.owed_keys &&"):
				reads++
			case strings.Contains(statement, "comment_deliveries"):
				attemptWrites++
			}
		}
		return reads > 0 && time.Since(opened) >= 3*time.Second
	})
	parked := time.Since(opened)
	// The age is read from a scrape that counts the held send: a held lane's head is out of the
	// gauges for as long as a try of it is in flight (mentionSender.heldSamples), so a scrape
	// taken during one reads no send and an age of 0 with nothing wrong. One scrape reads the count
	// and the age from one sample (registerHeldGauges), so the scrape that counts the send reads
	// its age. That age is at least the hold the test saw - the warn threshold, which the send had
	// passed when its WARN was logged, and the time since - and at most the time since the test
	// posted the mention, in whole seconds each: load can only raise the first, and the age with
	// it, and never takes the age past the second.
	least := int64((warnAfter + time.Since(warned)) / time.Second)
	var body string
	waitUntil(t, 10*time.Second, "a scrape that counts the held send", func() bool {
		body = scrape(t, registry)
		return strings.Contains(body, "envoy_dispatch_mention_sends_held 1\n")
	})
	most := int64(time.Since(posted) / time.Second)
	t.Logf("over %v parked: %d reads of the rule, %d statements on comment_deliveries, %d statements in all; gauges:\n%s",
		parked, reads, attemptWrites, len(window), heldLines(body))
	if limit := 3 + int(parked/mentionOrderRetryMax); reads > limit {
		t.Errorf("the parked lane read the rule %d times over %v, want at most %d: one each try, at mentionOrderRetryMax (%v) once it ramps up",
			reads, parked, limit, mentionOrderRetryMax)
	}
	if attemptWrites != 0 {
		t.Errorf("the parked lane made %d statements on comment_deliveries over %v, want none: a try of a held send reads the rule alone", attemptWrites, parked)
	}
	if oldest, ok := heldOldest(body); !ok || oldest < least || oldest > most {
		t.Errorf("the gauges read %q, want the held send's age from %d to %d seconds: the hold the test saw, and the time since it posted the mention",
			heldLines(body), least, most)
	}
	if lines := heldLines(body); strings.Contains(lines, "{") || strings.Contains(lines, issue.Key) {
		t.Errorf("the gauges read %q, want no label and no owner's key: /metrics answers whoever reaches the port, and an owner label would have no bound", lines)
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

// One scrape reads the held sends' count and the oldest one's age from one sample of the sender.
// The clock the gauges read takes the held send off its lane at its second read, as a try of the
// send that starts during a scrape does (next), so a scrape that read the sender twice - two
// gauge functions, one after the other - would count the send and read the age of none. The age
// is exact: the clock stands 4 s past the moment the send began to wait.
func TestAScrapeReadsTheHeldSendPairFromOneSample(t *testing.T) {
	since := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	lane := heldLane(laneKey{recipient: "session:s1"}, "A-1")
	sender := &mentionSender{
		mu:    new(sync.Mutex),
		lanes: map[laneKey][]mentionSend{lane: {{mention: ResolvedMention{Target: "session:s1"}, heldSince: since}}},
		waits: map[laneKey]heldWait{lane: {session: "s1", by: model.MentionCopyHolder{EventID: 1}}},
	}
	var reads atomic.Int64
	now := func() time.Time {
		if reads.Add(1) == 2 {
			sender.mu.Lock()
			sender.lanes[lane] = sender.lanes[lane][1:]
			sender.mu.Unlock()
		}
		return since.Add(4 * time.Second)
	}
	registry := metrics.New()
	(&server{mentions: sender}).registerHeldGauges(registry, now)
	body := scrape(t, registry)
	if !strings.Contains(body, "envoy_dispatch_mention_sends_held 1\n") ||
		!strings.Contains(body, "envoy_dispatch_mention_send_held_oldest_seconds 4\n") || reads.Load() != 1 {
		t.Fatalf("one scrape read the gauges' clock %d times and the gauges %q; want one read, and one send held for 4 s",
			reads.Load(), heldLines(body))
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

// heldOldest is the oldest held send's age, in whole seconds, that a /metrics body reads, and
// whether the body has that gauge's sample.
func heldOldest(body string) (int64, bool) {
	for line := range strings.SplitSeq(body, "\n") {
		if value, ok := strings.CutPrefix(line, "envoy_dispatch_mention_send_held_oldest_seconds "); ok {
			seconds, err := strconv.ParseInt(value, 10, 64)
			return seconds, err == nil
		}
	}
	return 0, false
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
