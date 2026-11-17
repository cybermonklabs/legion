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
// the listener says nobody holds (role:unheld) holds nothing. A lookup that fails says nothing
// about the holder, so it holds the send: a 503 (role:broken), an answer that does not decode
// (role:garbled), and a status the listener does not explain (role:other's bare 404).
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
		{name: "the role topic of a role the listener says nobody holds", owed: []string{contracts.RoleTopicPrefix + "unheld"}},
		{name: "a lookup the listener answers 503", owed: []string{contracts.RoleTopicPrefix + "broken"}, holds: true},
		{name: "a lookup whose answer does not decode", owed: []string{contracts.RoleTopicPrefix + "garbled"}, holds: true},
		{name: "a lookup answered by a bare 404", owed: []string{contracts.RoleTopicPrefix + "other"}, holds: true},
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
			by, err := served.mentionCopyHolderFor(ctx, model.Event{ID: laterID}, "s1")
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

// The route moves through the API between an earlier event's record and a later mention, and the
// mention still comes after the earlier event. The issue is routed to s2, a person's comment is
// sent to the topic family but its route send to s2 is refused, so the outbox records it owing
// s2's subject and backs it off. A person then moves the route to s1 (PATCH, the one write of
// issues.route, which appends issue.updated in the same transaction) and writes a comment that
// mentions s1. s1 follows no topic family, so it takes the earlier event only when the outbox
// sends it to s1 as the route on its retry; the mention's send waits until then.
func TestAMentionWaitsBehindAnEarlierEventWhoseRouteMovedToItsSession(t *testing.T) {
	t.Logf("start %s", time.Now().UTC().Format(time.RFC3339Nano))
	listener, state := newMentionListener(t)
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{
		envoyURL: listener.URL, envoyTimeout: 30 * time.Second, outboxPublishes: true,
	})
	issue := createInteractionIssue(t, handler, "TEST", "A route that moves", "before")
	route := func(to string) {
		t.Helper()
		if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
			"route": to,
		}, "alice"); patched.Code != http.StatusOK {
			t.Fatalf("route %s: status=%d body=%s", to, patched.Code, patched.Body.String())
		}
	}
	route("session:s2")
	view := &sessionView{issue: issue.Key, ownOnly: true}
	state.mu.Lock()
	state.landed = view.mentionLanded
	state.mu.Unlock()
	runOutboxBeside(t, deps, view)
	waitUntil(t, 10*time.Second, "the issue's first events published", func() bool {
		return unpublishedEvents(t, database, issue.Key) == 0
	})

	view.mu.Lock()
	view.refused = contracts.LegacyAgentSubject("s2")
	view.mu.Unlock()
	earlierComment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "the earlier event, by a person",
	}))
	earlier := commentEventID(t, database, earlierComment.ID)
	waitUntil(t, 10*time.Second, "the earlier event's route send backed off", func() bool {
		attempts, _ := eventAttempts(t, database, earlier)
		return attempts > 0
	})

	route("session:s1")
	const body = "the comment that mentions s1"
	mention := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": body, "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	comment := commentEventID(t, database, mention.ID)
	waitUntil(t, 60*time.Second, "every event of the issue published and the mention settled", func() bool {
		settled := true
		for _, attempt := range waitForCommentDeliveries(t, database, mention.ID, "the mention's attempts", func([]model.CommentDelivery) bool { return true }) {
			settled = settled && attempt.State != "pending"
		}
		return settled && unpublishedEvents(t, database, issue.Key) == 0
	})
	taken, own := view.order([]int64{earlier, comment}, map[string]int64{body: comment}, comment)
	t.Logf("s1 takes %v (want %v), the comment's copies on s1's own channels: %v", taken, []int64{earlier, comment}, own)
	t.Logf("end %s", time.Now().UTC().Format(time.RFC3339Nano))
	if !slices.Equal(taken, []int64{earlier, comment}) {
		t.Fatalf("s1 took %v, want the earlier event, whose route moved to s1, before the mention %v", taken, []int64{earlier, comment})
	}
	if !slices.Equal(own, []string{mentionChannel}) {
		t.Fatalf("the comment reached s1 on its own channels as %v, want once, by the mention", own)
	}
}

// A send in flight left the queue when it started, so a send that waits for an earlier event and
// comes back to a queue others have filled meanwhile would push it past its capacity. It is
// refused instead, as a send past the bound is: its claim is given back, the delivery sweep sends
// it later, and the queue never holds more than its capacity - here one, the send queued behind
// it, which then goes.
func TestASendThatWaitsAndFindsTheQueueFullGivesItsClaimBack(t *testing.T) {
	lifetime, stop := context.WithCancel(context.Background())
	started, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var sent []string
	var gaveUp []mentionKey
	sender := newMentionSender(lifetime, time.Hour,
		func(_ context.Context, item mentionSend) (mentionSend, bool) {
			if item.comment.ID == "first" {
				close(started)
				<-release
				return item, true
			}
			mu.Lock()
			defer mu.Unlock()
			sent = append(sent, item.comment.ID)
			return mentionSend{}, false
		},
		func(_ context.Context, queued []mentionClaim) ([]mentionClaim, error) { return queued, nil },
		func(_ context.Context, held []mentionClaim) error {
			mu.Lock()
			defer mu.Unlock()
			for _, claim := range held {
				gaveUp = append(gaveUp, claim.key)
			}
			return nil
		},
	)
	sender.mu.Lock()
	sender.capacity = 1
	sender.mu.Unlock()
	t.Cleanup(func() {
		stop()
		sender.wait()
	})
	enqueue := func(id string) mentionSend {
		item := mentionSend{
			comment: model.Comment{ID: id, IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
			attempt: 1, claimed: time.Now(),
		}
		sender.enqueue(context.Background(), item)
		return item
	}
	first := enqueue("first")
	<-started
	enqueue("second")
	close(release)
	waitUntil(t, 5*time.Second, "the send behind the refused one sent", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(sent, "second")
	})
	sender.mu.Lock()
	queued := len(sender.claims)
	sender.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	t.Logf("queued %d of a capacity of 1; claims given back %v; sent %v", queued, gaveUp, sent)
	if !slices.Equal(gaveUp, []mentionKey{first.key()}) {
		t.Fatalf("the sender gave back %v, want the claim of the send that waited and found the queue full", gaveUp)
	}
	if queued > 1 {
		t.Fatalf("the queue holds %d sends, past its capacity of 1", queued)
	}
}

// The held gauge counts every send that waits on a held lane: the one that waited for an earlier
// event of its issue and each send of that issue to the same recipient queued behind it.
func TestEverySendOnAHeldLaneIsCounted(t *testing.T) {
	lifetime, stop := context.WithCancel(context.Background())
	sender := newMentionSender(lifetime, time.Hour,
		func(_ context.Context, item mentionSend) (mentionSend, bool) { return item, true },
		func(_ context.Context, queued []mentionClaim) ([]mentionClaim, error) { return queued, nil },
		func(context.Context, []mentionClaim) error { return nil },
	)
	t.Cleanup(func() {
		stop()
		sender.wait()
	})
	for _, id := range []string{"a-first", "a-second", "a-third"} {
		sender.enqueue(context.Background(), mentionSend{
			comment: model.Comment{ID: id, IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
			attempt: 1, claimed: time.Now(),
		})
	}
	held := heldLane("session:s1", "A-1")
	waitUntil(t, 5*time.Second, "the three sends on A's held lane, parked", func() bool {
		sender.mu.Lock()
		defer sender.mu.Unlock()
		_, parked := sender.parked[held]
		return parked && len(sender.lanes[held]) == 3
	})
	count, _ := sender.heldSamples(time.Now())
	t.Logf("held gauge samples %+v", count)
	if len(count) != 1 || count[0].Value != 3 {
		t.Fatalf("the held gauge reads %+v, want 3 sends of A-1", count)
	}
}
