package outbox

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// An earlier event of T-1 is routed to s2 and its send to s2 fails, so the record says it owes
// s2's subject alone. A write then makes s1 a destination of it - the route moves to s1, or s1
// takes the claim - and a comment mentions s1, whose copy the fallback pass holds as a row. s1
// takes the earlier event before the comment, whether the write came before the comment or after
// it, and whatever batch has run by the time the row is decided.
//
// Each write is made the way Dispatch makes it, its event appended in the same transaction: the
// route by PATCH /api/v1/issues/{key} (issue_patch.go, issue.updated), the claim by a claim
// (issue.claimed). The new event moves T-1's last_seq past the owed_seq of the earlier event's
// record, which then holds the row until a batch that read the write records the earlier event
// again (owedAgain), owing s1's subject (model.MentionCopyHolderSQL). The write after the comment
// is the case a rule reading only events before the comment cannot see: its event comes after
// the comment, and the earlier event's record still names s2 alone when the row is decided.
//
// The control row moves the route with no event, a write only direct SQL makes, after a batch has
// recorded the earlier event as of the comment: nothing then moves T-1's last_seq or tells the
// record the earlier event's route changed before its next batch, and the row's copy reaches s1
// first - which is what shows this test can see a copy pass the earlier event.
func TestARouteChangeKeepsAMentionCopyBehindAnEarlierEvent(t *testing.T) {
	const (
		route = `update issues set route = 'session:s1' where key = 'T-1'`
		claim = `update issues set claimed_by = '{"kind": "session", "id": "s1"}', claimed_at = now() where key = 'T-1'`
	)
	routed := map[string]any{"changed_fields": []string{"route"}}
	claimed := map[string]any{"claimed_by": map[string]any{"kind": "session", "id": "s1"}}
	for _, tc := range []struct {
		name  string
		write string
		// event is the type of the event the write appends, "" for none, and payload its payload.
		event        string
		payload      map[string]any
		afterComment bool
		passes       bool
	}{
		{name: "the route moved as the API moves it, with issue.updated, before the comment", write: route, event: "issue.updated", payload: routed},
		{name: "the route moved as the API moves it, with issue.updated, after the comment", write: route, event: "issue.updated", payload: routed, afterComment: true},
		{name: "s1 took the claim, with issue.claimed, after the comment", write: claim, event: "issue.claimed", payload: claimed, afterComment: true},
		{name: "control: the route moved by direct SQL, with no event, after a batch read the comment", write: route, afterComment: true, passes: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("start %s", time.Now().UTC().Format(time.RFC3339Nano))
			ctx := context.Background()
			database := storetest.Open(t)
			broker := events.NewBroker()
			route := "session:s2"
			seedIssue(t, database, "T-1", &route)
			alice := model.Actor{Kind: "user", ID: "alice"}
			earlier := appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "message.created", Actor: alice,
				Payload: model.Message{ID: "6b1b1c5e-0d3a-4c38-9b7e-0a5f8a0c0303", IssueKey: new("T-1"), Body: "earlier"},
			})
			s1, s2 := contracts.LegacyAgentSubject("s1"), contracts.LegacyAgentSubject("s2")
			publisher := &recordingPublisher{failTopic: s2}
			deps := Deps{Store: database, Publisher: publisher, Broker: broker}
			holder := holding(t, deps)
			holder.scan(ctx, unfenced{})
			if owed, next, _, _ := eventRecord(t, database, earlier.ID); !slices.Equal(owed, []string{s2}) || next == nil {
				t.Fatalf("the earlier event owes %v, next attempt %v, want s2's subject alone, backed off", owed, next)
			}

			write := func() {
				tx, err := database.Pool.Begin(ctx)
				if err != nil {
					t.Fatalf("begin the write: %v", err)
				}
				defer func() { _ = tx.Rollback(ctx) }()
				if _, err := tx.Exec(ctx, tc.write); err != nil {
					t.Fatalf("write %q: %v", tc.write, err)
				}
				if tc.event != "" {
					if _, err := broker.Append(ctx, tx, model.Event{
						IssueKey: new("T-1"), Type: tc.event, Actor: alice, Payload: tc.payload,
					}); err != nil {
						t.Fatalf("append %s: %v", tc.event, err)
					}
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatalf("commit the write: %v", err)
				}
			}
			if !tc.afterComment {
				write()
			}
			const body = "@session:s1 look"
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
				t.Fatalf("hold s1's copy as a fallback row: %v", err)
			}
			switch {
			case tc.afterComment:
				if tc.event == "" {
					// The control: a batch records the earlier event as of the comment, owing s2,
					// and only then does the route move, with no event to tell the next batch.
					holder.scan(ctx, unfenced{})
				}
				// The write commits after the comment and before any batch reads it: the row is
				// decided while the earlier event's record still names s2 alone.
				write()
			case tc.event != "":
				// The batch the route's event wakes, before the row is decided.
				holder.scan(ctx, unfenced{})
			}
			publishFallbackCopies(ctx, deps, unfenced{}, holder.instance)
			dueNow(t, database, earlier.ID)
			// Every unpublished event of T-1 falls due again, a batch runs and the row is decided
			// again, until the comment's copy and every event have gone or the rounds run out.
			done := func() bool {
				var waiting int
				if err := database.Pool.QueryRow(ctx, `
					select count(*) from events where issue_key = 'T-1' and published_at is null
				`).Scan(&waiting); err != nil {
					t.Fatalf("count T-1's unpublished events: %v", err)
				}
				return waiting == 0 && slices.ContainsFunc(publisher.all(), func(item contracts.Envelope) bool {
					return item.Topic == s1 && item.SourceEventID == fmt.Sprint(comment.ID)
				})
			}
			for round := 0; round < 5 && !done(); round++ {
				if _, err := database.Pool.Exec(ctx, `
					update events set next_attempt_at = now() - interval '1 second' where issue_key = 'T-1' and published_at is null
				`); err != nil {
					t.Fatalf("make T-1's events due: %v", err)
				}
				holder.scan(ctx, unfenced{})
				if _, err := database.Pool.Exec(ctx, `update comment_fallback_copies set next_attempt_at = now() - interval '1 second' where comment_id = $1`, commentID); err != nil {
					t.Fatalf("make the row due: %v", err)
				}
				publishFallbackCopies(ctx, deps, unfenced{}, holder.instance)
			}
			var unpublished []string
			if err := database.Pool.QueryRow(ctx, `
				select coalesce(array_agg(type || ' ' || coalesce(publish_error, '') order by id), '{}') from events where issue_key = 'T-1' and published_at is null
			`).Scan(&unpublished); err != nil {
				t.Fatalf("read T-1's unpublished events: %v", err)
			}
			t.Logf("unpublished events of T-1 after the rounds: %v", unpublished)

			var got []string
			for _, item := range publisher.all() {
				if item.Topic == s1 && !slices.Contains(got, item.SourceEventID) &&
					(item.SourceEventID == fmt.Sprint(earlier.ID) || item.SourceEventID == fmt.Sprint(comment.ID)) {
					got = append(got, item.SourceEventID)
				}
			}
			inOrder := []string{fmt.Sprint(earlier.ID), fmt.Sprint(comment.ID)}
			t.Logf("s1 took events %v (earlier %d, comment %d)", got, earlier.ID, comment.ID)
			t.Logf("end %s", time.Now().UTC().Format(time.RFC3339Nano))
			switch {
			case !tc.passes && !slices.Equal(got, inOrder):
				t.Fatalf("s1 took %v, want the earlier event and then the comment %v", got, inOrder)
			case tc.passes && !slices.Equal(got, []string{inOrder[1], inOrder[0]}):
				t.Fatalf("control: s1 took %v, want the comment first, as a route moved with no event lets it", got)
			}
		})
	}
}

// A record that writes no owed keys - one made after its process lost the publisher hold, or one
// of the release before this one, which knows no owed_keys - can publish the event a write
// appended while it leaves an earlier event's owed_keys as an older record wrote them. The order
// model found it (TestTheMentionCopyOrderModel, the hand-over's plan under two publishers): an
// earlier event of T-1 recorded owing s2, the route moved to s1 after a comment that mentions s1,
// and the issue.updated of that move published by such a record, the earlier event still owing
// s2 alone. s1's copy waits all the same: the write moved T-1's last_seq past the earlier event's
// owed_seq, which no record that read the write has written since (model.MentionCopyHolderSQL).
//
// The control is the record with the hold, which writes the earlier event owing s1: the copy then
// waits on those keys, and once the earlier event is published it waits on nothing in any row -
// which shows the rule can answer no event here.
func TestARecordThatWritesNoOwedKeysLeavesAMentionCopyWaiting(t *testing.T) {
	for _, tc := range recordsAfterARouteMove {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("start %s", time.Now().UTC().Format(time.RFC3339Nano))
			ctx := context.Background()
			moved := recordAfterARouteMove(t, tc.record)
			owed, _, _, _ := eventRecord(t, moved.database, moved.earlier)
			by, err := copyHolder(ctx, moved.deps, moved.comment, moved.s1, "s1")
			if err != nil {
				t.Fatalf("read the rule: %v", err)
			}
			t.Logf("the earlier event owes %v after the record; s1's copy waits behind %s", owed, describeHolder(by))
			if by == nil || by.EventID != moved.earlier {
				t.Fatalf("s1's copy waits behind %s, want the earlier event %d: it owes s1 a send since the route moved", describeHolder(by), moved.earlier)
			}
			if _, err := moved.database.Pool.Exec(ctx, `update events set published_at = now() where id = $1`, moved.earlier); err != nil {
				t.Fatalf("publish the earlier event: %v", err)
			}
			if by, err := copyHolder(ctx, moved.deps, moved.comment, moved.s1, "s1"); err != nil || by != nil {
				t.Fatalf("control: with the earlier event published, s1's copy waits behind %s (%v), want nothing", describeHolder(by), err)
			}
			t.Logf("end %s", time.Now().UTC().Format(time.RFC3339Nano))
		})
	}
}

// The record writes an event's owed_keys and owed_seq only while the publisher hold is its own,
// the event it publishes included: the record with the hold writes issue.updated's two (owing
// nothing, as of T-1's last_seq), and a record without it - after its process lost the hold, or
// of the release before this one - writes neither. Nothing reads a published event's columns
// (model.MentionCopyHolderSQL and model.MentionCopyRolesSQL read unpublished events alone); this
// holds the record to the rule it states.
func TestARecordWithoutTheHoldWritesNoOwedColumnsOfAnEventItPublishes(t *testing.T) {
	for _, tc := range recordsAfterARouteMove {
		t.Run(tc.name, func(t *testing.T) {
			moved := recordAfterARouteMove(t, tc.record)
			var neither, both bool
			if err := moved.database.Pool.QueryRow(context.Background(), `
				select owed_keys is null and owed_seq is null, coalesce(owed_keys = '{}' and owed_seq = $2, false)
				from events where id = $1
			`, moved.updated, moved.lastSeq).Scan(&neither, &both); err != nil {
				t.Fatalf("read issue.updated's owed columns: %v", err)
			}
			t.Logf("issue.updated after the record: neither column written %v, both written as of last_seq %d %v", neither, moved.lastSeq, both)
			if tc.record == "held" && !both {
				t.Fatalf("the record with the hold left issue.updated's owed_keys and owed_seq unwritten, want '{}' and %d", moved.lastSeq)
			}
			if tc.record != "held" && !neither {
				t.Fatalf("a record without the hold wrote issue.updated's owed_keys or owed_seq, want neither: the record writes them only while the hold is its own")
			}
		})
	}
}

// recordsAfterARouteMove are the three records recordAfterARouteMove makes.
var recordsAfterARouteMove = []struct {
	name   string
	record string
}{
	{name: "a record made after its process lost the publisher hold", record: "lost"},
	{name: "a record of the release before this one", record: "previous"},
	{name: "control: the record with the hold", record: "held"},
}

// routeMoveRecord is what recordAfterARouteMove leaves: T-1's earlier event, recorded owing s2,
// the comment that mentions s1, and the issue.updated of the route's move to s1, published by
// the record named, with T-1's last_seq after the move.
type routeMoveRecord struct {
	database                  *store.Store
	deps                      Deps
	earlier, comment, updated int64
	lastSeq                   int
	s1                        string
}

// recordAfterARouteMove seeds T-1 routed to s2, records its earlier event owing s2 (s2's subject
// failing), writes a comment that mentions s1, moves the route to s1 with issue.updated, and has
// the record named - "lost" (recordBatch under a holder that is not the hold's), "previous" (the
// previous release's columns alone) or "held" - write what a batch that read the move comes to:
// the earlier event, not due, owing s1, and issue.updated published.
func recordAfterARouteMove(t *testing.T, record string) routeMoveRecord {
	t.Helper()
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	route := "session:s2"
	seedIssue(t, database, "T-1", &route)
	alice := model.Actor{Kind: "user", ID: "alice"}
	earlier := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: alice,
		Payload: model.Message{ID: "6b1b1c5e-0d3a-4c38-9b7e-0a5f8a0c0404", IssueKey: new("T-1"), Body: "earlier"},
	})
	s1, s2 := contracts.LegacyAgentSubject("s1"), contracts.LegacyAgentSubject("s2")
	deps := Deps{Store: database, Publisher: &recordingPublisher{failTopic: s2}, Broker: broker}
	holder := holding(t, deps)
	holder.scan(ctx, unfenced{})
	if owed, _, _, _ := eventRecord(t, database, earlier.ID); !slices.Equal(owed, []string{s2}) {
		t.Fatalf("the earlier event owes %v, want s2's subject alone", owed)
	}
	commentID := seedComment(t, database, "T-1", alice, "@session:s1 look", nil)
	comment := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: commentID, IssueKey: new("T-1"), Body: "@session:s1 look",
			Mentions: []model.Mention{{Target: "session:s1", Delivery: "steer"}},
		}},
	})
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the route's move: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `update issues set route = 'session:s1' where key = 'T-1'`); err != nil {
		t.Fatalf("move the route: %v", err)
	}
	updated, err := broker.Append(ctx, tx, model.Event{
		IssueKey: new("T-1"), Type: "issue.updated", Actor: alice,
		Payload: map[string]any{"changed_fields": []string{"route"}},
	})
	if err != nil {
		t.Fatalf("append issue.updated: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the route's move: %v", err)
	}
	item, err := envelope(updated, "")
	if err != nil {
		t.Fatalf("build issue.updated's envelope: %v", err)
	}
	var lastSeq int
	if err := database.Pool.QueryRow(ctx, `select last_seq from issues where key = 'T-1'`).Scan(&lastSeq); err != nil {
		t.Fatalf("read T-1's last_seq: %v", err)
	}
	// What a batch that read the move comes to: the earlier event, not due, owes s1 now,
	// and issue.updated, which goes to the topic family alone, is published.
	outcomes := []outcome{
		{id: earlier.ID, state: owedAgain, owed: []string{s1}, owedSeq: lastSeq},
		{id: updated.ID, state: published, reached: []string{item.Topic}, owed: []string{}, owedSeq: lastSeq},
	}
	switch record {
	case "lost":
		if holds, err := recordBatch(ctx, deps, outcomes, uuid.New()); err != nil || holds {
			t.Fatalf("record without the hold: holds %v, %v; want the hold another's", holds, err)
		}
	case "previous":
		// The record of the release before this one, which writes no owed_keys and no
		// owed_seq: a published event's destinations and its publish.
		if _, err := database.Pool.Exec(ctx, `
			update events set published_destinations = published_destinations || $2::text[], published_at = now(),
				publish_error = null, next_attempt_at = null
			where id = $1 and published_at is null
		`, updated.ID, []string{item.Topic}); err != nil {
			t.Fatalf("record issue.updated as the previous release does: %v", err)
		}
	case "held":
		if holds, err := recordBatch(ctx, deps, outcomes, holder.instance); err != nil || !holds {
			t.Fatalf("record with the hold: holds %v, %v; want the hold this process's", holds, err)
		}
	}
	var published bool
	if err := database.Pool.QueryRow(ctx, `select published_at is not null from events where id = $1`, updated.ID).Scan(&published); err != nil || !published {
		t.Fatalf("issue.updated published %v (%v), want published by the record", published, err)
	}
	return routeMoveRecord{database: database, deps: deps, earlier: earlier.ID, comment: comment.ID, updated: updated.ID, lastSeq: lastSeq, s1: s1}
}
