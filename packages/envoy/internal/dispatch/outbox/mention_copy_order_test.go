package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// describeHolder is what a test log says of the event a copy waits behind.
func describeHolder(by *model.MentionCopyHolder) string {
	switch {
	case by == nil:
		return "nothing"
	case by.PublishError == nil:
		return fmt.Sprintf("event %d (no publish error)", by.EventID)
	}
	return fmt.Sprintf("event %d (publish error %q)", by.EventID, *by.PublishError)
}

// earlierEventAndComment writes, on issue T-1 routed to s1, an earlier event the outbox has not
// published - a person's message - and then a comment of the same issue that mentions s1, whose
// mention's send failed, and answers both events and the comment's id.
func earlierEventAndComment(t *testing.T, database *store.Store, broker *events.Broker) (earlier, comment model.Event, commentID string) {
	t.Helper()
	route := "session:s1"
	seedIssue(t, database, "T-1", &route)
	alice := model.Actor{Kind: "user", ID: "alice"}
	earlier = appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: alice,
		Payload: model.Message{ID: "6b1b1c5e-0d3a-4c38-9b7e-0a5f8a0c0101", IssueKey: new("T-1"), Body: "earlier"},
	})
	commentID = seedComment(t, database, "T-1", alice, "@s1 look", nil)
	comment = appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: commentID, IssueKey: new("T-1"), Body: "@s1 look",
			Mentions: []model.Mention{{Target: "session:s1", Delivery: "steer"}},
		}},
	})
	seedDelivery(t, database, commentID, "session:s1", "s1", "failed")
	return earlier, comment, commentID
}

// The rule a mention's copy waits on reads what the outbox recorded an earlier event still owes
// (events.owed_keys), as of the owner's last_seq the recording batch read (events.owed_seq). An
// event no record has written that for - null, as every new event is until the outbox's first
// batch of it - holds the copy: it owes its topic family at least, and a copy let past it would
// reach the session before it, which is the case the order exists for. So does an event whose
// record read the owner before its last event: a write since may have made the session one of
// its destinations. The controls in the same run are the same event recorded, as of the owner's
// last event, as owing nothing, or owing another session's subject only, which hold no copy of
// s1's; and the words that do hold it.
func TestAnEarlierEventTheOutboxHasNotRecordedHoldsAMentionCopy(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	earlier, comment, _ := earlierEventAndComment(t, database, events.NewBroker())
	deps := Deps{Store: database}
	s1 := contracts.LegacyAgentSubject("s1")
	const failure = `publish owner topic "notifications.dispatch.issue.T-1.message.created": no responders`
	const current = `owed_seq = (select last_seq from issues where key = 'T-1')`
	for _, row := range []struct {
		name  string
		state string
		holds bool
	}{
		{name: "never recorded (null)", state: `owed_keys = null`, holds: true},
		{name: "never recorded (null) beside an owed_seq as of the owner's last event", state: `owed_keys = null, ` + current, holds: true},
		{name: "control: recorded owing nothing", state: `owed_keys = '{}', ` + current},
		{name: "control: recorded owing another session's subject alone", state: `owed_keys = array['notifications.agent.s2'], ` + current},
		{name: "recorded owing nothing before the owner's last event", state: `owed_keys = '{}', owed_seq = (select last_seq - 1 from issues where key = 'T-1')`, holds: true},
		{name: "recorded owing nothing with no owed_seq", state: `owed_keys = '{}'`, holds: true},
		{name: "owes its topic family", state: `owed_keys = array['` + model.OwedTopicFamily + `'], ` + current, holds: true},
		{name: "owes s1's own subject", state: `owed_keys = array['` + s1 + `'], ` + current, holds: true},
		{name: "a recipient read failed", state: `owed_keys = array['` + model.OwedAnySession + `'], ` + current, holds: true},
		{name: "control: published, never recorded", state: `owed_keys = null, published_at = now()`},
		{name: "control: a pending subscription command", state: `owed_keys = null, payload = payload || '{"pending": true}'`},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, err := database.Pool.Exec(ctx, `
				update events set owed_keys = null, owed_seq = null, published_at = null, payload = payload - 'pending', publish_error = $2
				where id = $1
			`, earlier.ID, failure); err != nil {
				t.Fatalf("reset the earlier event: %v", err)
			}
			if _, err := database.Pool.Exec(ctx, `update events set `+row.state+` where id = $1`, earlier.ID); err != nil {
				t.Fatalf("set the earlier event %s: %v", row.state, err)
			}
			by, err := copyHolder(ctx, deps, comment.ID, s1, "s1")
			if err != nil {
				t.Fatalf("read the rule: %v", err)
			}
			t.Logf("%s: the copy for s1 waits behind %s", row.state, describeHolder(by))
			switch {
			case row.holds && (by == nil || by.EventID != earlier.ID || by.PublishError == nil || *by.PublishError != failure):
				t.Fatalf("%s: the copy for s1 waits behind %s, want event %d with its publish error", row.state, describeHolder(by), earlier.ID)
			case !row.holds && by != nil:
				t.Fatalf("%s: the copy for s1 waits behind event %d, want nothing", row.state, by.EventID)
			}
		})
	}
	// Control: the rule reads earlier events only - the comment holds no copy of the event before it.
	if by, err := copyHolder(ctx, deps, earlier.ID, s1, "s1"); err != nil || by != nil {
		t.Fatalf("a copy of the earlier event itself waits behind %+v (%v), want nothing: no event of T-1 comes before it", by, err)
	}
}

// The rule reads the owner's newest event through the owner's own column: a document's for a
// comment on a document no issue owns (events.artifact_id), and a project's for an event of a
// project (events.project_key), as it reads an issue's for an issue's comment. An earlier event
// recorded owing nothing as of a seq before the owner's newest event holds s1's copy - a write
// since may have made s1 one of its destinations - and the control, the same record as of the
// owner's newest event, holds nothing. A rule that read no newest event for either owner would
// hold nothing in either row.
func TestTheCopyRuleReadsTheNewestEventOfADocumentOrAProject(t *testing.T) {
	alice := model.Actor{Kind: "user", ID: "alice"}
	for _, owner := range []struct {
		name string
		// seed writes the owner and answers an event of it, its type and payload to be set, and
		// the statement that reads the owner's last_seq.
		seed func(t *testing.T, database *store.Store) (model.Event, string)
	}{
		{name: "a document no issue owns", seed: func(t *testing.T, database *store.Store) (model.Event, string) {
			seedProject(t, database)
			doc := seedDocument(t, database, nil, "plan", alice)
			return model.Event{ArtifactID: new(doc)}, `select last_seq from artifacts where id = '` + doc + `'`
		}},
		{name: "a project", seed: func(t *testing.T, database *store.Store) (model.Event, string) {
			seedProject(t, database)
			return model.Event{ProjectKey: new("TT")}, `select last_seq from projects where key = 'TT'`
		}},
	} {
		for _, row := range []struct {
			name   string
			behind int
		}{
			{name: "recorded owing nothing before the owner's newest event", behind: 1},
			{name: "control: recorded owing nothing as of the owner's newest event"},
		} {
			t.Run(owner.name+": "+row.name, func(t *testing.T) {
				ctx := context.Background()
				database := storetest.Open(t)
				broker := events.NewBroker()
				of, lastSeq := owner.seed(t, database)
				earlier, comment := of, of
				earlier.Type, earlier.Actor, earlier.Payload = "message.created", alice, map[string]any{"body": "earlier"}
				earlier = appendEvent(t, database, broker, earlier)
				comment.Type, comment.Actor, comment.Payload = "comment.created", alice, map[string]any{"body": "@s1 look"}
				comment = appendEvent(t, database, broker, comment)
				if _, err := database.Pool.Exec(ctx, `
					update events set owed_keys = '{}', owed_seq = (`+lastSeq+`) - $2 where id = $1
				`, earlier.ID, row.behind); err != nil {
					t.Fatalf("record the earlier event: %v", err)
				}
				by, err := copyHolder(ctx, Deps{Store: database}, comment.ID, contracts.LegacyAgentSubject("s1"), "s1")
				if err != nil {
					t.Fatalf("read the rule: %v", err)
				}
				t.Logf("the earlier event recorded %d behind the owner's newest event: s1's copy waits behind %s", row.behind, describeHolder(by))
				if holds := row.behind > 0; (by != nil) != holds || (by != nil && by.EventID != earlier.ID) {
					t.Fatalf("s1's copy waits behind %s, want holding %t (event %d)", describeHolder(by), holds, earlier.ID)
				}
			})
		}
	}
}

// seedProject inserts the project TT, which seedIssue inserts beside its issue.
func seedProject(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `insert into projects (key, name, default_assignee) values ('TT', 'Test', 'alice')`); err != nil {
		t.Fatalf("create project: %v", err)
	}
}

// A pending subscription command (subscription.remove_requested, which api/subscribers.go writes
// pending) moves its owner's last_seq, but no batch reads it and it makes no session a
// destination, so it leaves an earlier event's record current: s1's copy waits behind nothing for
// it. The control is the same event appended not pending, a write the record did not see, which
// holds the copy behind the earlier event.
func TestAPendingSubscriptionCommandLeavesAnEarlierEventsRecordCurrent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pending bool
		holds   bool
	}{
		{name: "a pending subscription command", pending: true},
		{name: "control: the same event, not pending", holds: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			database := storetest.Open(t)
			broker := events.NewBroker()
			earlier, comment, _ := earlierEventAndComment(t, database, broker)
			if _, err := database.Pool.Exec(ctx, `
				update events set owed_keys = '{}', owed_seq = (select last_seq from issues where key = 'T-1') where id = $1
			`, earlier.ID); err != nil {
				t.Fatalf("record the earlier event as of the comment: %v", err)
			}
			payload := map[string]any{"session_id": "s9", "topics": []string{"notifications.dispatch.issue.T-1.>"}}
			if tc.pending {
				payload["pending"] = true
			}
			appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "subscription.remove_requested", Actor: model.Actor{Kind: "session", ID: "s9"}, Payload: payload,
			})
			by, err := copyHolder(ctx, Deps{Store: database}, comment.ID, contracts.LegacyAgentSubject("s1"), "s1")
			if err != nil {
				t.Fatalf("read the rule: %v", err)
			}
			t.Logf("%s: s1's copy waits behind %s", tc.name, describeHolder(by))
			if (by != nil) != tc.holds || (by != nil && by.EventID != earlier.ID) {
				t.Fatalf("%s: s1's copy waits behind %s, want holding %v (event %d)", tc.name, describeHolder(by), tc.holds, earlier.ID)
			}
		})
	}
}

// heldGauges scrapes registry and answers its body.
func heldGauges(t *testing.T, registry *metrics.Registry) string {
	t.Helper()
	response := httptest.NewRecorder()
	registry.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return response.Body.String()
}

// A fallback copy that waits behind an earlier event of its owner shows: the gauges count it and
// read how long the oldest has waited, naming no owner, since /metrics answers whoever reaches the
// port; and once it has waited past the warn threshold the pass logs one WARN naming the owner,
// the event it waits behind and that event's last publish error - once, however many passes find
// it waiting. When the event is published the row's copy goes and the gauges read none: the
// control, as is the scrape before any pass.
func TestAHeldFallbackCopyIsCountedAndReportedOnce(t *testing.T) {
	ctx := store.WithTransactionTracking(context.Background())
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
	database := storetest.Open(t)
	broker := events.NewBroker()
	earlier, comment, commentID := earlierEventAndComment(t, database, broker)
	const failure = `publish owner topic "notifications.dispatch.issue.T-1.message.created": no responders`
	if _, err := database.Pool.Exec(ctx, `update events set publish_error = $2, attempt_count = 3 where id = $1`, earlier.ID, failure); err != nil {
		t.Fatalf("record the earlier event's failed publish: %v", err)
	}
	s1 := contracts.LegacyAgentSubject("s1")
	if _, err := database.Pool.Exec(ctx, `
		insert into comment_fallback_copies (comment_id, destination, event_id, session_id) values ($1, $2, $3, 's1')
	`, commentID, s1, comment.ID); err != nil {
		t.Fatalf("hold s1's copy as a fallback row: %v", err)
	}
	registry := metrics.New()
	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher, Broker: broker, Metrics: registry,
		held: newHeldCopies(), heldCopyWarnAfter: time.Second}
	holder := holding(t, deps).instance
	due := func() {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `update comment_fallback_copies set next_attempt_at = now() - interval '1 second' where comment_id = $1`, commentID); err != nil {
			t.Fatalf("make the row due: %v", err)
		}
	}
	warnings := func() int {
		return strings.Count(logs.String(), `level=WARN msg="dispatch outbox: a fallback copy of a mention waits behind an earlier event of its owner"`)
	}
	if body := heldGauges(t, registry); !strings.Contains(body, "envoy_dispatch_outbox_fallback_copies_held 0\n") {
		t.Fatalf("the gauges read %q before any pass, want no held copy", body)
	}

	publishFallbackCopies(ctx, deps, unfenced{}, holder)
	if row := readHeldCopy(t, database, commentID, s1); row.settled || row.published {
		t.Fatalf("the row is settled %v and published %v while event %d owes its topic family, want it waiting", row.settled, row.published, earlier.ID)
	}
	if body := heldGauges(t, registry); !strings.Contains(body, "envoy_dispatch_outbox_fallback_copies_held 1\n") ||
		!strings.Contains(body, "envoy_dispatch_outbox_fallback_copy_held_oldest_seconds 0\n") {
		t.Fatalf("the gauges read %q, want one copy held, the oldest held under a second", body)
	}
	if got := warnings(); got != 0 {
		t.Fatalf("%d WARN lines before the threshold, want none", got)
	}

	time.Sleep(1100 * time.Millisecond)
	due()
	publishFallbackCopies(ctx, deps, unfenced{}, holder)
	due()
	publishFallbackCopies(ctx, deps, unfenced{}, holder)
	body := heldGauges(t, registry)
	if !strings.Contains(body, "envoy_dispatch_outbox_fallback_copies_held 1\n") ||
		!strings.Contains(body, "envoy_dispatch_outbox_fallback_copy_held_oldest_seconds 1\n") {
		t.Fatalf("the gauges read %q, want one copy held for a second", body)
	}
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, "envoy_dispatch_outbox_fallback_cop") && (strings.Contains(line, "{") || strings.Contains(line, "T-1")) {
			t.Fatalf("the gauges read %q, want no label and no owner's key: /metrics answers whoever reaches the port", line)
		}
	}
	logged := logs.String()
	t.Logf("logged:\n%s", logged)
	if got := warnings(); got != 1 {
		t.Fatalf("%d WARN lines past the threshold over two passes, want one", got)
	}
	for _, want := range []string{"owner=T-1", fmt.Sprintf("event_id=%d", earlier.ID), "publish_error=", "notifications.dispatch.issue.T-1.message.created", "destination=" + s1} {
		if !strings.Contains(logged, want) {
			t.Fatalf("the WARN line does not name %q: %s", want, logged)
		}
	}

	// Control: the earlier event is published, so the copy goes and nothing is held.
	if _, err := database.Pool.Exec(ctx, `update events set published_at = now() where id = $1`, earlier.ID); err != nil {
		t.Fatalf("publish the earlier event: %v", err)
	}
	due()
	publishFallbackCopies(ctx, deps, unfenced{}, holder)
	if row := readHeldCopy(t, database, commentID, s1); !row.settled || !row.published {
		t.Fatalf("the row is settled %v and published %v once the earlier event was published, want both", row.settled, row.published)
	}
	if body := heldGauges(t, registry); !strings.Contains(body, "envoy_dispatch_outbox_fallback_copies_held 0\n") {
		t.Fatalf("the gauges read %q once the copy went, want no held copy", body)
	}
	if got := warnings(); got != 1 {
		t.Fatalf("%d WARN lines in all, want the one", got)
	}
}

// The read load of fallback copies held behind an earlier event is bounded by mentionPendingDelay:
// a waiting row is released for that long after each pass that decides it, and a pass claims only
// the rows that are due, so the rule is read at most once per row per delay, whatever wakes the
// holder meanwhile. Measured with the outbox running for eight seconds over 50 rows held behind
// one event whose topic send the stream keeps refusing, the holder woken every 100 ms beside its
// own timers: the rule's reads are counted on a traced pool, and so is every statement.
func TestHeldFallbackCopiesReadTheRuleAtMostOncePerPendingDelay(t *testing.T) {
	const rows = 50
	const window = 8 * time.Second
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	earlier, _, _ := earlierEventAndComment(t, database, broker)
	alice := model.Actor{Kind: "user", ID: "alice"}
	s1 := contracts.LegacyAgentSubject("s1")
	for index := range rows {
		body := fmt.Sprintf("@s1 comment %d", index)
		commentID := seedComment(t, database, "T-1", alice, body, nil)
		comment := appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
			Payload: model.CommentEventPayload{Comment: model.Comment{
				ID: commentID, IssueKey: new("T-1"), Body: body,
				Mentions: []model.Mention{{Target: "session:s1", Delivery: "steer"}},
			}},
		})
		seedDelivery(t, database, commentID, "session:s1", "s1", "failed")
		if _, err := database.Pool.Exec(ctx, `
			insert into comment_fallback_copies (comment_id, destination, event_id, session_id) values ($1, $2, $3, 's1')
		`, commentID, s1, comment.ID); err != nil {
			t.Fatalf("hold copy %d: %v", index, err)
		}
	}
	recorder := &storetest.StatementRecorder{}
	traced := storetest.TracedStore(t, database, recorder)
	publisher := &failingPublisher{topic: "notifications.dispatch.issue.T-1.message.created", answer: fmt.Errorf("no responders")}
	started := time.Now()
	stop := runDeps(t, withHeldRoles(Deps{Store: traced, Publisher: publisher, Broker: broker, Listen: database.Pool.Listener}))
	// A wake every 100 ms - what an event committed beside the outbox gives it - so each wake
	// runs a pass, and only the rows' own delay keeps the reads down.
	waking, stopWaking := context.WithCancel(ctx)
	woken := make(chan struct{})
	go func() {
		defer close(woken)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-waking.Done():
				return
			case <-ticker.C:
				broker.Publish(model.Event{ID: earlier.ID, IssueKey: new("T-1"), Type: "message.created"})
			}
		}
	}()
	time.Sleep(window)
	stopWaking()
	<-woken
	stop()
	elapsed := time.Since(started)
	if at := publishedAt(t, database, earlier.ID); at != nil {
		t.Fatalf("the earlier event was published at %v, want its topic send refused throughout", at)
	}
	statements := recorder.Statements()
	reads := 0
	for _, statement := range statements {
		if strings.Contains(statement, "k.owed_keys &&") {
			reads++
		}
	}
	bound := rows * (int(elapsed/mentionPendingDelay) + 1)
	t.Logf("%d rows held for %v: %d reads of the rule (%.1f a second, %.2f a row a second), %d statements in all (%.1f a second); bound %d reads",
		rows, elapsed.Round(time.Millisecond), reads, float64(reads)/elapsed.Seconds(), float64(reads)/elapsed.Seconds()/rows,
		len(statements), float64(len(statements))/elapsed.Seconds(), bound)
	if reads == 0 {
		t.Fatal("the rule was never read: the rows were not decided, so this measures nothing")
	}
	if reads > bound {
		t.Fatalf("%d reads of the rule for %d rows over %v, want at most %d: one a row each %v", reads, rows, elapsed, bound, mentionPendingDelay)
	}
	var settled int
	if err := database.Pool.QueryRow(ctx, `select count(*) from comment_fallback_copies where settled_at is not null`).Scan(&settled); err != nil {
		t.Fatalf("count the settled rows: %v", err)
	}
	if settled != 0 {
		t.Fatalf("%d rows settled while the earlier event was unpublished, want none", settled)
	}
}
