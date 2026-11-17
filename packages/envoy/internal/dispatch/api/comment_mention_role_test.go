package api

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The API's road to a mention asks the rule for a role route an earlier event still owes by the
// role's holder now: the outbox records the route as its role topic (owedKeys), and the stand-in
// listener resolves role:reviewer to s1, so a mention's send to s1 waits behind that event,
// whichever session held the role when the event was recorded. A route the event owes to a role
// nobody holds holds nothing (the listener knows no role:other).
//
// The control row is the record as it would read keyed on the holder at the time,
// notifications.agent.ses_stale_holder: the send to s1 then waits for nothing, which is the copy
// passing the event that the role-topic record exists to stop - so the rule reads which key the
// record holds, and the case row is not passing by accident.
func TestTheAPIsMentionWaitsForARoleRouteItsSessionHoldsNow(t *testing.T) {
	t.Logf("start %s", time.Now().UTC().Format(time.RFC3339Nano))
	ctx := context.Background()
	listener, _ := newMentionListener(t)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
		envoyURL: listener.URL, envoyTimeout: 30 * time.Second, outboxPublishes: true,
	})
	issue := createInteractionIssue(t, handler, "TEST", "A role route owed before a mention", "before")
	earlier := decodeMentionedComment(t, sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "the earlier event, routed to the role", "actor": map[string]any{"kind": "session", "id": "s2"},
	}))
	later := decodeMentionedComment(t, sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "the comment whose mention is sent", "actor": map[string]any{"kind": "session", "id": "s2"},
	}))
	earlierID, laterID := commentEventID(t, database, earlier.ID), commentEventID(t, database, later.ID)
	for _, row := range []struct {
		name  string
		owed  []string
		holds bool
	}{
		{name: "the role topic of a role s1 holds now", owed: []string{contracts.RoleTopicPrefix + "reviewer"}, holds: true},
		{name: "the role topic of a role nobody holds", owed: []string{contracts.RoleTopicPrefix + "other"}},
		{name: "control: keyed on the holder at the time of the record", owed: []string{contracts.LegacyAgentSubject("ses_stale_holder")}},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, err := database.Pool.Exec(ctx, `
				update events set published_at = case when id = $2 then null else coalesce(published_at, now()) end,
					owed_keys = case when id = $2 then $3::text[] else owed_keys end
				where issue_key = $1
			`, issue.Key, earlierID, row.owed); err != nil {
				t.Fatalf("set the earlier event owing %v: %v", row.owed, err)
			}
			by, err := served.mentionCopyHolderFor(ctx, model.Event{ID: laterID}, "s1", "steer")
			if err != nil {
				t.Fatalf("read the rule: %v", err)
			}
			waits := by != nil && by.EventID == earlierID
			t.Logf("the earlier event owes %v: the send to s1 waits behind it: %v", row.owed, waits)
			if waits != row.holds || (!row.holds && by != nil) {
				t.Fatalf("the earlier event owes %v: the send to s1 waits behind %+v, want waiting %v", row.owed, by, row.holds)
			}
		})
	}
	t.Logf("end %s", time.Now().UTC().Format(time.RFC3339Nano))
}

// A send that waits for an earlier event of issue A holds back A's later sends to the same
// recipient, in order, and no send of another issue: issue B's mention of the same session goes
// while A's two wait, since an earlier event of A holds no event of B (heldLane). Once A's earlier
// event is sent, A's two go in the order they were accepted.
func TestAMentionHeldOnOneIssueHoldsNoMentionOfAnotherIssueToTheSameSession(t *testing.T) {
	lifetime, stop := context.WithCancel(context.Background())
	var mu sync.Mutex
	var sent []string
	open := false
	sender := newMentionSender(lifetime, 100*time.Millisecond,
		func(_ context.Context, item mentionSend) (mentionSend, bool) {
			mu.Lock()
			defer mu.Unlock()
			if *item.comment.IssueKey == "A-1" && !open {
				return item, true
			}
			sent = append(sent, item.comment.ID)
			return mentionSend{}, false
		},
		func(_ context.Context, queued []mentionClaim) ([]mentionClaim, error) { return queued, nil },
		func(context.Context, []mentionClaim) error { return nil },
	)
	t.Cleanup(func() {
		stop()
		sender.wait()
	})
	enqueue := func(id, issue string) {
		sender.enqueue(context.Background(), mentionSend{
			comment: model.Comment{ID: id, IssueKey: new(issue)}, mention: ResolvedMention{Target: "session:s1"},
			attempt: 1, claimed: time.Now(),
		})
	}
	sentNow := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(sent)
	}

	enqueue("a-first", "A-1")
	waitUntil(t, 5*time.Second, "A's first send held on its own lane", func() bool {
		sender.mu.Lock()
		defer sender.mu.Unlock()
		_, parked := sender.parked[heldLane("session:s1", "A-1")]
		return parked
	})
	enqueue("b-only", "B-1")
	enqueue("a-second", "A-1")
	waitUntil(t, 5*time.Second, "B's mention of s1 sent while A's wait", func() bool {
		return slices.Contains(sentNow(), "b-only")
	})
	if got := sentNow(); !slices.Equal(got, []string{"b-only"}) {
		t.Fatalf("sent %v while A's earlier event was owed, want B's alone", got)
	}

	mu.Lock()
	open = true
	mu.Unlock()
	waitUntil(t, 5*time.Second, "A's two sends once its earlier event was sent", func() bool {
		return len(sentNow()) == 3
	})
	t.Logf("sent in the order %v", sentNow())
	if got := sentNow(); !slices.Equal(got, []string{"b-only", "a-first", "a-second"}) {
		t.Fatalf("sent %v, want B's, then A's first and A's second", got)
	}
}
