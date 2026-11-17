package outbox

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// An earlier event of T-1 is routed to s2 and its send to s2 fails, so the record says it owes
// s2's subject alone. The issue's route then moves to s1, and a later comment mentions s1, whose
// copy the fallback pass holds as a row. s1 takes the earlier event before the comment.
//
// The route moves the way the API moves it - the one write of issues.route, PATCH
// /api/v1/issues/{key} (issue_patch.go), which appends issue.updated in the same transaction - so
// that event is unpublished, and owes its topic family, when the row is first decided, and the
// batch that publishes it reads the earlier event not due and writes what it owes now (owedAgain):
// s1's subject. Either holds the row.
//
// The control row moves the route with no event, a write only direct SQL makes: nothing then
// tells the record the earlier event's route changed before its next batch, and the row's copy
// reaches s1 first - which is what shows this test can see a copy pass the earlier event.
func TestARouteChangeKeepsAMentionCopyBehindAnEarlierEvent(t *testing.T) {
	for _, tc := range []struct {
		name      string
		withEvent bool
		passes    bool
	}{
		{name: "the route moved as the API moves it, with issue.updated", withEvent: true},
		{name: "control: the route moved by direct SQL, with no event", passes: true},
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

			if _, err := database.Pool.Exec(ctx, `update issues set route = 'session:s1' where key = 'T-1'`); err != nil {
				t.Fatalf("move the route: %v", err)
			}
			if tc.withEvent {
				appendEvent(t, database, broker, model.Event{
					IssueKey: new("T-1"), Type: "issue.updated", Actor: alice,
					Payload: map[string]any{"changed_fields": []string{"route"}},
				})
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
			if tc.withEvent {
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
