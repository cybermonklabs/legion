package api

import (
	"context"
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
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A listener that answers none of a session's lookups, its comment-time lookup or its send's
// recheck, delivers no mention copy: the mention's attempt fails. A session the comment's route or
// its author route reaches must then still hear the comment from that route's copy on its own
// topic, as it does when no mention names it, rather than from no copy at all.
func TestASilentListenerStillLeavesTheSessionOneCopyOfTheComment(t *testing.T) {
	for _, tc := range []struct {
		name      string
		route     string
		replyToS1 bool
	}{
		{name: "the issue's route is the mentioned session", route: "session:s1"},
		{name: "the comment replies in the mentioned session's thread", replyToS1: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, state := newMentionListener(t)
			// Never released: every lookup of s1 waits until its caller gives up. What ends
			// each of them is the window the listener client gives the call, on this clock,
			// which the wait for the attempt moves to the end of each window.
			_ = state.holdLookups("/v1/registry/s1")
			clock := clocktest.New(time.Now())
			handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL, clock: clock})
			issue := createInteractionIssue(t, handler, "TEST", "A silent listener", "before")
			if tc.route != "" {
				if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
					"route": tc.route,
				}, "alice"); patched.Code != http.StatusOK {
					t.Fatalf("set the route: status=%d body=%s", patched.Code, patched.Body.String())
				}
			}
			input := map[string]any{"body": "Are you there?", "mentions": []map[string]any{{"target": "session:s1"}}}
			if tc.replyToS1 {
				root := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
					"body": "Where should this go?", "actor": map[string]any{"kind": "session", "id": "s1"},
				})
				input["reply_to"] = decodeMentionedComment(t, root).ID
			}
			bound := clock.Now().Add(mentionLookupTimeout)
			posted := make(chan *httptest.ResponseRecorder, 1)
			go func() { posted <- postMentionedComment(t, handler, issue.Key, input) }()
			// The comment looks s1 up before its transaction, bounded by mentionLookupTimeout on
			// the server's clock and armed before the lookup's request leaves. The clock moves to
			// that bound once the stand-in holds the lookup: moved sooner, it would end a lookup
			// the listener never saw, which is no silent listener's doing.
			testwait.Until(t, "the stand-in to hold s1's lookup, bounded at "+bound.Format(time.RFC3339Nano),
				func() ([]time.Time, bool) {
					armed := clock.Armed()
					_, holding := state.calls.counts()
					return armed, holding == 1 && slices.ContainsFunc(armed, bound.Equal)
				})
			clock.Set(bound)
			created := decodeMentionedComment(t, testwait.Receive(t, "the comment to answer once its lookup's bound ran out", posted))

			settled := settledCommentDeliveriesOn(t, database, created.ID, clock, &state.calls)
			if len(settled) != 1 || settled[0].State != "failed" {
				t.Fatalf("the mention to a listener that never answers = %#v, want its one attempt failed", settled)
			}
			if sends := state.sent(); len(sends) != 0 {
				t.Fatalf("the listener took sends %#v, want none: it answered nothing", sends)
			}
			if !publishedToSession(t, deps, issue.Key, created.ID, "s1") {
				t.Fatalf("no event of comment %s was published to %s, and its mention failed: s1 got no copy of the comment",
					created.ID, contracts.LegacyAgentSubject("s1"))
			}
		})
	}
}

// publishedToSession runs the outbox until every event of issueKey is published, then reports
// whether any event about commentID (its comment.created, a comment.delivery, any other) was
// published to the session's own topic.
func publishedToSession(t *testing.T, deps Deps, issueKey, commentID, sessionID string) bool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		outbox.New(outbox.Deps{
			Store: deps.Store, Publisher: acceptingPublisher{}, Broker: events.NewBroker(),
			Listen: deps.Store.Pool.Listener,
		}).Run(ctx)
	}()
	defer func() {
		cancel()
		<-stopped
	}()
	testwait.Eventually(t, "the outbox to publish every event of "+issueKey, func() bool {
		var unpublished int
		if err := deps.Store.Pool.QueryRow(context.Background(),
			`select count(*) from events where issue_key = $1 and published_at is null`, issueKey).Scan(&unpublished); err != nil {
			t.Fatalf("count unpublished events of %s: %v", issueKey, err)
		}
		return unpublished == 0
	})
	rows, err := deps.Store.Pool.Query(context.Background(), `
		select published_destinations from events
		where issue_key = $1 and (payload->>'id' = $2 or payload->>'comment_id' = $2)
	`, issueKey, commentID)
	if err != nil {
		t.Fatalf("read the destinations of comment %s's events: %v", commentID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var destinations []string
		if err := rows.Scan(&destinations); err != nil {
			t.Fatalf("scan destinations: %v", err)
		}
		if slices.Contains(destinations, contracts.LegacyAgentSubject(sessionID)) {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read destinations: %v", err)
	}
	return false
}

// A process stopped (or killed) with a comment's mention send queued leaves its attempt pending
// with no claim. Once it has waited a lease, the outbox counts it abandoned and publishes the route
// copy it had held back, and the next process's delivery sweep may also send it. The two must be
// exclusive for one (comment, session): here the outbox acts first, and the session still ends up
// with exactly one copy, whichever of the two delivers it.
func TestAnAbandonedMentionIsDeliveredOnceByTheOutboxOrTheSweepNotBoth(t *testing.T) {
	listener, state := newMentionListener(t)
	stopping, stop := context.WithCancel(t.Context())
	handler, database, deps, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL, lifetime: stopping})
	issue := createInteractionIssue(t, handler, "TEST", "A stopped process's mention", "before")
	if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
		"route": "session:s1",
	}, "alice"); patched.Code != http.StatusOK {
		t.Fatalf("set the route: status=%d body=%s", patched.Code, patched.Body.String())
	}

	// The process is stopping: the comment commits and its send never starts.
	stop()
	stranded := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Accepted while stopping.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if !unclaimedPending(t, database, stranded.ID, "session:s1") {
		t.Fatal("the attempt of a comment accepted while the process stopped was taken")
	}
	ageAttempts(t, database, stranded.ID)

	// The outbox reaches the comment first.
	routeCopy := publishedToSession(t, deps, issue.Key, stranded.ID, "s1")

	// Then the next process's delivery sweep runs.
	if err := served.resumeOwedDeliveries(context.Background()); err != nil {
		t.Fatalf("resume the owed deliveries: %v", err)
	}
	waitForCommentDeliveries(t, database, stranded.ID, "no attempt pending", func(attempts []model.CommentDelivery) bool {
		for _, attempt := range attempts {
			if attempt.State == "pending" {
				return false
			}
		}
		return true
	})

	copies := 0
	if routeCopy {
		copies++
	}
	for _, send := range state.sent() {
		if send.session == "s1" {
			copies++
		}
	}
	if copies != 1 {
		t.Fatalf("s1 received %d copies of the comment (route copy published: %v, mention sends: %#v), want exactly 1",
			copies, routeCopy, state.sent())
	}
}

// A process killed while its mention send was in flight leaves the attempt pending with a claim
// that lapses a lease later, though the send may already have reached the listener: here it did,
// so s1 already holds the comment. Whatever then settles the attempt - the outbox's copy or the
// delivery sweep's resend, which the listener drops as a repeat of the same message - s1 must end
// up with exactly one copy of the comment, not the mention and the route copy both.
func TestAMentionInFlightAtAKillReachesItsSessionOnceAfterTheClaimLapses(t *testing.T) {
	listener, state := newMentionListener(t)
	stopping, stop := context.WithCancel(t.Context())
	handler, database, deps, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL, lifetime: stopping})
	issue := createInteractionIssue(t, handler, "TEST", "A send in flight at a kill", "before")
	if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
		"route": "session:s1",
	}, "alice"); patched.Code != http.StatusOK {
		t.Fatalf("set the route: status=%d body=%s", patched.Code, patched.Body.String())
	}
	stop()
	const body = "In flight when the process was killed."
	inFlight := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": body, "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	// The killed process claimed the attempt and its send reached the listener; it died before it
	// recorded the outcome, and its claim has lapsed.
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set claimed_at = now() - `+model.DeliveryClaimLapseInterval+` - interval '1 second'
		where comment_id = $1
	`, inFlight.ID); err != nil {
		t.Fatalf("lapse the killed process's claim: %v", err)
	}
	ageAttempts(t, database, inFlight.ID)
	landed, err := http.Post(listener.URL+"/v1/messages/send", "application/json",
		strings.NewReader(`{"target_session":"s1","message":"`+body+`"}`))
	if err != nil {
		t.Fatalf("land the killed process's send: %v", err)
	}
	landed.Body.Close()

	routeCopy := publishedToSession(t, deps, issue.Key, inFlight.ID, "s1")
	if err := served.resumeOwedDeliveries(context.Background()); err != nil {
		t.Fatalf("resume the owed deliveries: %v", err)
	}
	waitForCommentDeliveries(t, database, inFlight.ID, "no attempt pending", func(attempts []model.CommentDelivery) bool {
		for _, attempt := range attempts {
			if attempt.State == "pending" {
				return false
			}
		}
		return true
	})

	// The listener stores one message per (session, text): a resend of what already landed is a
	// repeat it drops, so distinct texts count copies.
	mentionCopies := map[string]struct{}{}
	for _, send := range state.sent() {
		if send.session == "s1" {
			mentionCopies[send.body] = struct{}{}
		}
	}
	copies := len(mentionCopies)
	if routeCopy {
		copies++
	}
	if copies != 1 {
		t.Fatalf("s1 received %d copies of the comment (route copy published: %v, mention sends: %#v), want exactly 1",
			copies, routeCopy, state.sent())
	}
}

// A process killed after it claimed a mention attempt, before its send reached the listener,
// leaves the attempt pending with a claim that lapses a lease later; the session holds no copy.
// If the listener is silent from then on, the sweep's resend fails too, and the route copy
// is the only way the session can still hear the comment: it must get exactly one copy, as it does
// when the listener is silent and no process was killed.
func TestAMentionClaimedByAKilledProcessStillReachesItsSessionWhenTheResendFails(t *testing.T) {
	listener, state := newMentionListener(t)
	stopping, stop := context.WithCancel(t.Context())
	// The lookups of s1 the resend makes are never answered: each one ends at its window on
	// this clock, which the wait for the attempt moves to the end of each window.
	clock := clocktest.New(time.Now())
	handler, database, deps, served := newRegisteredTestServer(t, testServerOptions{
		envoyURL: listener.URL, lifetime: stopping, clock: clock,
	})
	issue := createInteractionIssue(t, handler, "TEST", "A claimed send that never landed", "before")
	if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
		"route": "session:s1",
	}, "alice"); patched.Code != http.StatusOK {
		t.Fatalf("set the route: status=%d body=%s", patched.Code, patched.Body.String())
	}
	stop()
	claimed := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Claimed when the process was killed.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set claimed_at = now() - `+model.DeliveryClaimLapseInterval+` - interval '1 second'
		where comment_id = $1
	`, claimed.ID); err != nil {
		t.Fatalf("lapse the killed process's claim: %v", err)
	}
	ageAttempts(t, database, claimed.ID)
	// From now on the listener answers nothing.
	_ = state.holdLookups("/v1/registry/s1")

	// The outbox runs from here on; it gets the comment before the next process's first pass.
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		outbox.New(outbox.Deps{
			Store: deps.Store, Publisher: acceptingPublisher{}, Broker: events.NewBroker(),
			Listen: deps.Store.Pool.Listener,
		}).Run(ctx)
	}()
	defer func() {
		cancel()
		<-stopped
	}()
	testwait.Eventually(t, "the outbox to publish the comment.created of "+claimed.ID, func() bool {
		var published bool
		if err := database.Pool.QueryRow(context.Background(), `
			select published_at is not null from events where type = 'comment.created' and payload->>'id' = $1
		`, claimed.ID).Scan(&published); err != nil {
			t.Fatalf("read comment.created: %v", err)
		}
		return published
	})

	// The resend's lookups of s1 are held unanswered, so the sweep runs on its own goroutine and
	// the wait below moves the clock to the end of each call's window until the attempt settles.
	swept := make(chan error, 1)
	go func() { swept <- served.resumeOwedDeliveries(context.Background()) }()
	waitForCommentDeliveries(t, database, claimed.ID, "no attempt pending", func(attempts []model.CommentDelivery) bool {
		stepListenerWaits(clock, &state.calls)
		for _, attempt := range attempts {
			if attempt.State == "pending" {
				return false
			}
		}
		return true
	})
	if err := testwait.Receive(t, "the delivery sweep to finish", swept); err != nil {
		t.Fatalf("resume the owed deliveries: %v", err)
	}

	copies := 0
	testwait.Eventually(t, "the outbox to publish every event of "+issue.Key+" and settle every held copy", func() bool {
		var unpublished, unsettled int
		if err := database.Pool.QueryRow(context.Background(), `
			select (select count(*) from events where issue_key = $1 and published_at is null),
			       (select count(*) from comment_fallback_copies where comment_id = $2 and settled_at is null)
		`, issue.Key, claimed.ID).Scan(&unpublished, &unsettled); err != nil {
			t.Fatalf("count unpublished events: %v", err)
		}
		return unpublished == 0 && unsettled == 0
	})
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from events
		        where issue_key = $1 and (payload->>'id' = $2 or payload->>'comment_id' = $2) and $3 = any(published_destinations))
		     + (select count(*) from comment_fallback_copies where comment_id::text = $2 and destination = $3 and published)
	`, issue.Key, claimed.ID, contracts.LegacyAgentSubject("s1")).Scan(&copies); err != nil {
		t.Fatalf("count s1's copies: %v", err)
	}
	for _, send := range state.sent() {
		if send.session == "s1" {
			copies++
		}
	}
	if copies != 1 {
		t.Fatalf("s1 received %d copies of the comment (mention sends: %#v), want exactly 1: its resend failed, so only the route copy can reach it",
			copies, state.sent())
	}
}
