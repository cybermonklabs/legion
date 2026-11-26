package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
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
// it waiting. The record reads its moments off a fake clock, which the test moves past the
// threshold itself. When the event is published the row's copy goes and the gauges read none: the
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
	fake := clocktest.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	deps := Deps{Store: database, Publisher: publisher, Broker: broker, Metrics: registry,
		held: newHeldCopies(fake), heldCopyWarnAfter: time.Second}
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

	fake.Advance(1100 * time.Millisecond)
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

// The read load of fallback copies held behind an earlier event is bounded by the cost
// requirement: M waiting rows read the order rule at most M times each 2 s (readEvery, written
// here rather than read from mentionPendingDelay, so that a change to the outbox's wait fails this
// test rather than moving its bound with it). Three things make it so, and the test reads each off
// the passes themselves, in the database's own clock, never off a wall clock it waits on: a pass
// reads the rule at most once for each row it claims; it claims a row only once the row is due;
// and it writes a row that waits due again 2 s or more after that write. A trigger records, beside
// each value of next_attempt_at written, the now() of the statement that wrote it: a row's claim
// and its record. The test reads every statement the server ran or refused on a pass's
// connections, off the bytes they write and the server's answers (SentStatements), so a statement
// shows however it was sent - on its own, in a batch, prepared and run by name, bound in one round
// trip and run in a later one - and each must be, byte for byte, one of the statements a pass
// sends (passStatements), and run: any other, and any the server refused, fails the test, whatever
// it reads. Each read of the rule is bound to one row's event and destination, and no row
// may be read for more than once in a pass, nor read for at all unless the pass claims it. Fifty
// rows wait behind an earlier event no record has written. The first pass claims all fifty,
// reading the rule at least once to hold them. Half the rows are then written due an hour on, on
// the database's clock, and a second pass runs: it must claim none of those, however long after
// the first it starts, and claims whichever of the others are due by then. The control is a third
// pass once the rows are made due, which claims all fifty again. In each, no row is claimed before
// it is due, no row due at the pass's start is left, and each row claimed is written due again at
// least 2 s on.
//
// The test sees what the test store's own connections send, and a connection the outbox opened
// from a copy of their configuration; a read on a connection the code opens itself from a
// connection string is outside it. The pass reads the rule through copyrule.Holder on the store's
// pool (copyHolder passes deps.Store.Pool), which is the recorded pool here;
// TestOnlyHolderReadsTheCopyRule keeps the rule's SQL and names in Holder and each road calling it,
// and does not check which pool a call is given. A pass whose only reads of the rule moved to
// another connection fails the floor of one read when it holds rows.
func TestHeldFallbackCopiesReadTheRuleAtMostOncePerPendingDelay(t *testing.T) {
	const rows = 50
	const readEvery = 2 * time.Second
	ctx := store.WithTransactionTracking(context.Background())
	database := storetest.Open(t)
	broker := events.NewBroker()
	earlier, _, _ := earlierEventAndComment(t, database, broker)
	alice := model.Actor{Kind: "user", ID: "alice"}
	s1 := contracts.LegacyAgentSubject("s1")
	// rowOf is each held row, "<comment id> <destination>" as the trigger below writes it, by the
	// id of its comment's event, which a read of the rule for the row is bound to.
	rowOf := map[int64]string{}
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
		rowOf[comment.ID] = fmt.Sprintf("%s %s", commentID, s1)
	}
	for _, statement := range []string{
		`create table fallback_due_writes (
			id bigserial primary key, row_key text not null, written_at timestamptz not null, next_attempt_at timestamptz
		)`,
		`create function record_fallback_due_write() returns trigger language plpgsql as $$
		begin
			insert into fallback_due_writes (row_key, written_at, next_attempt_at)
			values (new.comment_id::text || ' ' || new.destination, now(), new.next_attempt_at);
			return null;
		end
		$$`,
		`create trigger record_fallback_due_write after update of next_attempt_at on comment_fallback_copies
			for each row execute function record_fallback_due_write()`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("record each write of a fallback row's next_attempt_at: %v", err)
		}
	}
	sent := &storetest.SentStatements{}
	publisher := &recordingPublisher{}
	deps := withHeldRoles(Deps{Store: storetest.SentStore(t, database, sent), Publisher: publisher, Broker: broker})
	holder := holding(t, deps).instance
	// passStatements is every statement a fallback pass sends, byte for byte: its claim, its read
	// of the rows' own events, its two reads of each row's mentions, the rule's own statement, its
	// read of whether a comment whose copy it would publish is deleted, and its record, and the
	// pool's check of a connection that sat idle (PoolPing).
	passStatements := map[string]bool{
		fallbackClaimQuery: true, fallbackEventsQuery: true, mentionDeliveryQuery: true,
		mentionFallbackHoldsQuery: true, model.MentionCopyRuleSQL: true, fallbackWithdrawnQuery: true,
		fallbackRecordQuery: true, storetest.PoolPing: true,
	}
	// due is each row's next_attempt_at as last written, nil for due at once, as a fresh row is.
	due := map[string]*time.Time{}
	keys, err := database.Pool.Query(ctx, `select comment_id::text || ' ' || destination from comment_fallback_copies`)
	if err != nil {
		t.Fatalf("read the rows: %v", err)
	}
	for keys.Next() {
		var row string
		if err := keys.Scan(&row); err != nil {
			t.Fatalf("read a row: %v", err)
		}
		due[row] = nil
	}
	keys.Close()
	if len(due) != rows {
		t.Fatalf("%d rows held, want %d", len(due), rows)
	}
	var written int64
	var problems []string
	// pass runs one fallback pass and reads what it did: how many rows it claimed, and how many
	// times it read the rule. Each claimed row is written twice, by the claim and by the record,
	// and each read of the rule is bound to one row's event and destination.
	pass := func(name string) (claimed, reads int) {
		t.Helper()
		var started time.Time
		if err := database.Pool.QueryRow(ctx, `select clock_timestamp()`).Scan(&started); err != nil {
			t.Fatalf("read the database's clock: %v", err)
		}
		before := len(sent.Statements())
		publishFallbackCopies(ctx, deps, unfenced{}, holder)
		readsOf := map[string]int{}
		for _, statement := range sent.Statements()[before:] {
			switch {
			case statement.Failed:
				problems = append(problems, fmt.Sprintf("%s: a statement the server answered with error %s, which may have read the rule in part: %q",
					name, statement.Code, statementText(statement.SQL, 160)))
			case statement.SQL == model.MentionCopyRuleSQL:
				reads++
				event, readEvent := statement.Int8(0)
				destination, readDestination := statement.Text(1)
				row, held := rowOf[event]
				if !readEvent || !readDestination || !held || destination != fmt.Sprint(s1) {
					problems = append(problems, fmt.Sprintf("%s: a read of the rule for no held row (event %d, destination %q)", name, event, destination))
					continue
				}
				readsOf[row]++
			case !passStatements[statement.SQL]:
				problems = append(problems, fmt.Sprintf("%s: a statement that is none of a pass's own: %q", name, statementText(statement.SQL, 160)))
			}
		}
		var writes []fallbackDueWrite
		writes, written = fallbackDueWritesSince(t, database, written)
		byRow := map[string][]fallbackDueWrite{}
		for _, write := range writes {
			byRow[write.row] = append(byRow[write.row], write)
		}
		waits := map[time.Duration]int{}
		for row, rowWrites := range byRow {
			if len(rowWrites) != 2 {
				problems = append(problems, fmt.Sprintf("%s: %s written %d times, want its claim and its record", name, row, len(rowWrites)))
				continue
			}
			claimed++
			claim, record := rowWrites[0], rowWrites[1]
			if was := due[row]; was != nil && was.After(claim.at) {
				problems = append(problems, fmt.Sprintf("%s: %s claimed at %v, due only at %v", name, row, claim.at, *was))
			}
			if record.due == nil {
				problems = append(problems, fmt.Sprintf("%s: %s written due at once", name, row))
			} else if wait := record.due.Sub(record.at); wait < readEvery {
				problems = append(problems, fmt.Sprintf("%s: %s due %v after its record, want at least %v", name, row, wait, readEvery))
			} else {
				waits[wait]++
			}
			due[row] = record.due
		}
		dueAtStart := 0
		for row, at := range due {
			if _, read := byRow[row]; read {
				continue
			}
			if at == nil || !at.After(started) {
				dueAtStart++
				problems = append(problems, fmt.Sprintf("%s: %s was due by the database's clock when the pass started and was not claimed", name, row))
			}
		}
		for row, rowReads := range readsOf {
			if _, claimedRow := byRow[row]; !claimedRow {
				problems = append(problems, fmt.Sprintf("%s: %s read the rule %d times, and the pass did not claim it", name, row, rowReads))
			} else if rowReads > 1 {
				problems = append(problems, fmt.Sprintf("%s: %s read the rule %d times, want at most once a row", name, row, rowReads))
			}
		}
		if claimed > 0 && reads == 0 {
			problems = append(problems, fmt.Sprintf("%s: %d rows claimed and held with no read of the rule", name, claimed))
		}
		most := 0
		for _, rowReads := range readsOf {
			most = max(most, rowReads)
		}
		t.Logf("%s: %d rows claimed, %d reads of the rule over %d rows, at most %d for one row, the claimed rows due again after %v; %d rows due at its start left unclaimed",
			name, claimed, reads, len(readsOf), most, waits, dueAtStart)
		return claimed, reads
	}

	if claimed, _ := pass("the first pass"); claimed != rows {
		t.Fatalf("the first pass claimed %d of the %d fresh rows, want all: a row it did not decide shows no wait", claimed, rows)
	}
	// Half the rows due an hour on, on the database's clock, written before the second pass: it
	// must leave them, however long after the first pass it starts.
	if _, err := database.Pool.Exec(ctx, `
		update comment_fallback_copies set next_attempt_at = now() + interval '1 hour'
		where (comment_id, destination) in (
			select comment_id, destination from comment_fallback_copies order by comment_id limit $1
		)
	`, rows/2); err != nil {
		t.Fatalf("write half the rows due an hour on: %v", err)
	}
	var later []fallbackDueWrite
	later, written = fallbackDueWritesSince(t, database, written)
	if len(later) != rows/2 {
		t.Fatalf("%d rows written due an hour on, want %d", len(later), rows/2)
	}
	for _, write := range later {
		due[write.row] = write.due
	}
	pass("a second pass, half the rows due an hour on")
	if _, err := database.Pool.Exec(ctx, `
		update comment_fallback_copies set next_attempt_at = now() - interval '1 second' where settled_at is null
	`); err != nil {
		t.Fatalf("make the rows due: %v", err)
	}
	var madeDue []fallbackDueWrite
	madeDue, written = fallbackDueWritesSince(t, database, written)
	for _, write := range madeDue {
		due[write.row] = write.due
	}
	if claimed, _ := pass("control: a third pass once the rows are due"); claimed != rows {
		t.Errorf("control: once the rows were made due a pass claimed %d of them, want all %d", claimed, rows)
	}
	for _, what := range sent.Blind() {
		problems = append(problems, "the recorder could not read a statement the pass's connections sent: "+what)
	}
	slices.Sort(problems)
	for _, problem := range problems[:min(10, len(problems))] {
		t.Error(problem)
	}
	if len(problems) > 10 {
		t.Errorf("and %d more", len(problems)-10)
	}
	if at := publishedAt(t, database, earlier.ID); at != nil {
		t.Fatalf("the earlier event was published at %v, want it unpublished throughout", at)
	}
	if published := publisher.all(); len(published) != 0 {
		t.Fatalf("the passes published %d messages while every row waited behind the earlier event, want none", len(published))
	}
	var settled int
	if err := database.Pool.QueryRow(ctx, `select count(*) from comment_fallback_copies where settled_at is not null`).Scan(&settled); err != nil {
		t.Fatalf("count the settled rows: %v", err)
	}
	if settled != 0 {
		t.Fatalf("%d rows settled while the earlier event was unpublished, want none", settled)
	}
}

// statementText is statement's first n characters, its runs of white space each one space.
func statementText(statement string, n int) string {
	text := strings.Join(strings.Fields(statement), " ")
	if len(text) > n {
		return text[:n] + "..."
	}
	return text
}

// fallbackDueWrite is one write of a fallback row's next_attempt_at, as the trigger
// TestHeldFallbackCopiesReadTheRuleAtMostOncePerPendingDelay installs records it: the row, the
// now() of the statement that wrote it, and the value written.
type fallbackDueWrite struct {
	row string
	at  time.Time
	due *time.Time
}

// fallbackDueWritesSince is the writes the trigger recorded after id since, in order, and the id
// of the last of them, since itself for none.
func fallbackDueWritesSince(t *testing.T, database *store.Store, since int64) ([]fallbackDueWrite, int64) {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select id, row_key, written_at, next_attempt_at from fallback_due_writes where id > $1 order by id
	`, since)
	if err != nil {
		t.Fatalf("read the writes of next_attempt_at: %v", err)
	}
	defer rows.Close()
	last := since
	var writes []fallbackDueWrite
	for rows.Next() {
		var write fallbackDueWrite
		if err := rows.Scan(&last, &write.row, &write.at, &write.due); err != nil {
			t.Fatalf("read a write of next_attempt_at: %v", err)
		}
		writes = append(writes, write)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the writes of next_attempt_at: %v", err)
	}
	return writes, last
}
