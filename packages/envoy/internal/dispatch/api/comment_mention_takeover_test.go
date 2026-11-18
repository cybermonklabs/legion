package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// takeOverTries drives the fake clock a test server runs on through a comment's wait for a
// listener take-over before its transaction (resolveThroughTakeover): each lookup the comment
// makes is bounded by mentionLookupTimeout, and each one the take-over answered is followed by a
// backoff. The test moves the clock through every backoff and never through a lookup's bound,
// which would leave that lookup unanswered instead of answered by the stand-in - the outcome a
// real clock gave a loaded machine, and one these rows' comments decide their routes on.
//
// The first backoff and a lookup's bound are one span (takeoverFirstBackoff and
// mentionLookupTimeout are both 250 ms), so for a moment after a try is answered its bound is
// still pending at the very deadline its backoff is about to be armed for. The stand-in tells the
// two apart: as it takes a lookup it notes the clock's reading - the moment that try armed its
// bound and its call's window (envoy.CallWindow) - and moves the clock on by takeOverTryStep,
// within which nothing is due, so the backoff that try's answer leads to is armed later than the
// try's own bound.
//
// Each try must reach the stand-in as one request, which an answered status gives and a dropped
// connection does not: an HTTP client sends a GET again, on a new connection, when a reused one
// is dropped.
type takeOverTries struct {
	clock *clocktest.Fake

	mu sync.Mutex
	// taken is the clock's reading as the stand-in took each registry lookup.
	taken []time.Time
	// moved is how many lookups the stand-in had taken when the clock last moved.
	moved int
}

// takeOverTryStep is how far the stand-in moves the clock as it takes a lookup: less than any
// deadline the try has armed, so the move fires nothing.
const takeOverTryStep = time.Millisecond

// takeOverServer is a test server whose listener is a take-over stand-in and whose clock is a
// fake, which runs the take-over window every deployment runs, and the record of the lookups the
// stand-in takes (takeOverTries), through which a comment's wait for the take-over is driven.
func takeOverServer(t *testing.T) (*takeOverListener, *takeOverTries, http.Handler, *store.Store, Deps) {
	t.Helper()
	listener := startTakeOverListener(t)
	clock := clocktest.New(time.Now())
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.server.URL, clock: clock})
	tries := &takeOverTries{clock: clock}
	listener.mu.Lock()
	listener.observe = tries.took
	listener.mu.Unlock()
	return listener, tries, handler, database, deps
}

func (w *takeOverTries) took(route takeOverRoute) {
	if route != takeOverRegistry {
		return
	}
	w.mu.Lock()
	w.taken = append(w.taken, w.clock.Now())
	w.mu.Unlock()
	w.clock.Advance(takeOverTryStep)
}

// answer runs request, a comment whose mention lookups wait a take-over out, on a goroutine of
// its own and waits for its answer, moving the clock to each backoff once the try before it has
// returned: the stand-in has taken a lookup since the last move, and neither that lookup's bound
// nor its call's window is pending any longer. A backoff is never further off than
// takeoverMaxBackoff.
func (w *takeOverTries) answer(t *testing.T, request func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	w.mu.Lock()
	w.moved = len(w.taken)
	w.mu.Unlock()
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- request() }()
	return testwait.Until(t, "the comment to answer once its mention's lookups had waited the take-over out",
		func() (*httptest.ResponseRecorder, bool) {
			select {
			case response := <-answered:
				return response, true
			default:
			}
			w.mu.Lock()
			taken, moved := len(w.taken), w.moved
			var last time.Time
			if taken > 0 {
				last = w.taken[taken-1]
			}
			w.mu.Unlock()
			if taken <= moved {
				return nil, false
			}
			armed := w.clock.Armed()
			inFlight := func(at time.Time) bool {
				return at.Equal(last.Add(mentionLookupTimeout)) || at.Equal(last.Add(envoy.CallWindow))
			}
			if len(armed) == 0 || slices.ContainsFunc(armed, inFlight) ||
				armed[0].After(w.clock.Now().Add(takeoverMaxBackoff)) {
				return nil, false
			}
			w.clock.Set(armed[0])
			w.mu.Lock()
			w.moved = taken
			w.mu.Unlock()
			return nil, false
		})
}

// A person's comment on an issue routed to session s1 that also mentions s1 reaches s1 once:
// either the mention steers it there and the route fan-out stands aside (suppress_route), or the
// mention does not reach s1 and the route fan-out does. The listener being replaced when the
// comment is written - its registry read answered 503 once - must not make it both: the
// creating transaction decides suppress_route from the mention's pre-commit resolution, so a
// mention that only resolves on the post-commit take-over retry steers s1 a comment the route
// fan-out also delivers, under a different key the listener cannot recognise as a repeat. The
// server runs on a fake clock, moved through the backoff between the two lookups and never
// through a lookup's bound (takeOverTries).
func TestACommentDuringAListenerTakeOverReachesTheRouteHolderOnce(t *testing.T) {
	listener, tries, handler, database, _ := takeOverServer(t)
	issue := createInteractionIssue(t, handler, "TEST", "Routed take-over", "before")
	if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key,
		map[string]any{"route": "session:s1"}, "alice"); patched.Code != http.StatusOK {
		t.Fatalf("route the issue to s1: status=%d body=%s", patched.Code, patched.Body.String())
	}

	// The listener is starting when the comment's mention is resolved, and answers from then on.
	listener.play(takeOverAnswer{takeOverRegistry, answerJSON(http.StatusServiceUnavailable, `{"error":"service starting"}`)})
	created := decodeMentionedComment(t, tries.answer(t, func() *httptest.ResponseRecorder {
		return postMentionedComment(t, handler, issue.Key, map[string]any{
			"body": "@s1, please look at this.", "mentions": []map[string]any{{"target": "session:s1"}},
		})
	}))

	// The mention's send runs on the recipient's lane after the response, which answers its
	// attempts pending, so what s1 received is only settled once they are. A lane that waits a
	// take-over out does so on the server's clock, which the wait moves through its backoffs; a
	// lane's lookups carry no bound of the comment's.
	settled := settledCommentDeliveriesOn(t, database, created.ID, tries.clock, &listener.calls)

	var suppressRoute bool
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce((payload->>'suppress_route')::boolean, false)
		from events where type = 'comment.created' and payload->>'id' = $1
	`, created.ID).Scan(&suppressRoute); err != nil {
		t.Fatalf("read the comment's suppress_route: %v", err)
	}
	mentionSends := 0
	for _, send := range listener.published() {
		if send.target == "s1" {
			mentionSends++
		}
	}
	deliveries := eventDeliveries(t, database, "comment.created", created.ID)
	if len(deliveries) != 1 {
		t.Fatalf("comment.created events for %s = %#v, want one", created.ID, deliveries)
	}
	routeSends := 0
	for _, topic := range deliveries[0].Destinations {
		if topic == contracts.LegacyAgentSubject("s1") {
			routeSends++
		}
	}

	attempts := make([]string, 0, len(settled))
	for _, attempt := range settled {
		session := "<none>"
		if attempt.SessionID != nil {
			session = *attempt.SessionID
		}
		attempts = append(attempts, fmt.Sprintf("%s attempt %d %s to %s", attempt.Target, attempt.Attempt, attempt.State, session))
	}
	if heard := mentionSends + routeSends; heard != 1 {
		t.Fatalf("session s1 received the comment %d times, want once: %d mention steer(s) and %d route fan-out(s); "+
			"comment.created suppress_route=%t while the mention's delivery is [%s] - the mention retried through the "+
			"listener take-over after commit and reached s1, but the route suppression was decided from its failed "+
			"pre-commit resolution, so the route holder also gets it",
			heard, mentionSends, routeSends, suppressRoute, strings.Join(attempts, "; "))
	}
}

// Each mention of a session the listener holds no record of waits out the listener take-over
// window before it settles "no live session", and the creating request sends its mentions before
// it answers. Those waits must not stack per mention: a comment mentioning several ended
// sessions answers within one take-over window - read on the clock the server runs on, which
// this test moves through each wait - well inside the dispatch CLI's 60 s request abort, which
// would otherwise fire after the comment committed and make its retry post the comment a second
// time.
func TestACommentMentioningSeveralEndedSessionsAnswersWithinOneTakeOverWindow(t *testing.T) {
	var calls listenerCalls
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer calls.took()()
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/") {
			writeSessionRecord(t, w, `[]`, strings.TrimPrefix(r.URL.Path, "/v1/registry/"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer listener.Close()
	clock := clocktest.New(time.Now())
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL, clock: clock})
	issue := createInteractionIssue(t, handler, "TEST", "Ended sessions", "before")

	ended := []string{"ended-1", "ended-2"}
	mentions := make([]map[string]any, 0, len(ended))
	for _, session := range ended {
		mentions = append(mentions, map[string]any{"target": "session:" + session})
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	started := clock.Now()
	go func() {
		responses <- postMentionedComment(t, handler, issue.Key, map[string]any{
			"body": "Did either of you finish this?", "mentions": mentions,
		})
	}()
	// The comment looks each mention up before its transaction, bounded by mentionLookupTimeout on
	// the server's clock - a deadline as near as a backoff, which stepListenerWaits fires whenever
	// the stand-in is serving nothing, as it is while a lookup's request is still on its way to
	// it. A lookup cut so goes unanswered, not answered with the "no live session" the stand-in
	// gives, so its comment commits on another resolution than the one this row writes it with. A
	// comment with no route, as this one is, commits once its lookups have answered, waiting out
	// no take-over first (resolveCommentDeliveries), so the clock stays where it is until the
	// comment's attempts are committed; a wait of the request's after that is one the move below
	// takes it through.
	testwait.Eventually(t, "the comment's attempts to be committed", func() bool {
		var committed int
		if err := database.Pool.QueryRow(context.Background(), `select count(*) from comment_deliveries`).Scan(&committed); err != nil {
			t.Fatalf("count the comment's attempts: %v", err)
		}
		return committed >= len(ended)
	})
	response := answerWhileCallsAreHeld(t, clock, &calls, responses)
	took := clock.Now().Sub(started)
	created := decodeMentionedComment(t, response)
	if len(created.Deliveries) != len(ended) {
		t.Fatalf("deliveries = %#v, want one per mention", created.Deliveries)
	}
	// The response answers every attempt pending: each mention's send runs on its recipient's
	// lane after it, and settles there with the resolution the ended session gave.
	for _, attempt := range created.Deliveries {
		if attempt.State != "pending" {
			t.Fatalf("delivery = %#v, want it answered pending: the mention is sent after the response", attempt)
		}
	}
	settled := settledCommentDeliveriesOn(t, database, created.ID, clock, &calls)
	if len(settled) != len(ended) {
		t.Fatalf("settled deliveries = %#v, want one per mention", settled)
	}
	for index, attempt := range settled {
		want := "no live session " + ended[index]
		if attempt.State != "failed" || attempt.Error == nil || *attempt.Error != want {
			t.Fatalf("delivery = %#v, want failed with %q: the mentioned session is ended", attempt, want)
		}
	}
	if took > listenerTakeoverWindow {
		t.Fatalf("the comment mentioning %d ended sessions answered %v after its first try, want within one take-over "+
			"window (%v): each mention waited out a window of its own, one after another, so the wait grows with "+
			"every mention and the dispatch CLI's 60 s abort fires after the comment committed",
			len(ended), took, listenerTakeoverWindow)
	}
}

// A comment on an issue routed to s1 that mentions s1, written while the listener is replaced
// for longer than the take-over window, decides its route on the mention's failed resolution: the
// mention reached nobody, so suppress_route stays off and the route fan-out carries the comment to
// s1. A sender that dies before it settles that mention leaves attempt 1 pending, holding the
// resolve error the decision was made on. Whoever finishes the attempt later - the delivery
// sweep, or a person's Retry of the pending attempt - must settle it with that resolution: s1
// answers by then, and a mention resolved afresh steers s1 the comment under the mention's own
// key, beside the route's frame the listener cannot recognise it as a repeat of.
//
// The server runs the window every deployment runs, on a fake clock the test moves through each
// backoff of a comment's resolution and never through a lookup's bound (takeOverTries): a bound
// that ran out before the listener's answer leaves the mention unanswered rather than failed,
// which suppresses the route and leaves the mention to a sender that resolves it afresh - what a
// real clock let a loaded machine do.
func TestAResumedMentionOfARoutedCommentReachesTheRouteHolderOnce(t *testing.T) {
	listener, tries, handler, database, deps := takeOverServer(t)
	direct := directServer(deps)
	issue := createInteractionIssue(t, handler, "TEST", "Routed resume", "before")
	if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key,
		map[string]any{"route": "session:s1"}, "alice"); patched.Code != http.StatusOK {
		t.Fatalf("route the issue to s1: status=%d body=%s", patched.Code, patched.Body.String())
	}

	// Both comments are written during one take-over that outlasts the window, so each mention's
	// resolution runs the window out before its comment commits. The listener answers 503 as it
	// does while it starts, so each try is one lookup the stand-in takes (takeOverTries); the
	// comments are written one after the other, so the backoffs the clock is moved to are one
	// comment's at a time.
	listener.answerEvery(answerJSON(http.StatusServiceUnavailable, `{"error":"service starting"}`))
	comments := make([]mentionedCommentRead, 2)
	for index := range comments {
		comments[index] = decodeMentionedComment(t, tries.answer(t, func() *httptest.ResponseRecorder {
			return postMentionedComment(t, handler, issue.Key, map[string]any{
				"body":     fmt.Sprintf("@s1, look at this (%d).", index),
				"mentions": []map[string]any{{"target": "session:s1"}},
			})
		}))
	}
	listener.answerEvery(nil)

	// heard is how many times s1 has received comment: the mention sends the listener published
	// under the comment's key since the last play, and the route fan-out of its comment.created.
	heard := func(comment mentionedCommentRead) (mentionSends, routeSends int) {
		t.Helper()
		for _, send := range listener.published() {
			if send.target == "s1" && strings.HasPrefix(send.key, comment.ID+":") {
				mentionSends++
			}
		}
		deliveries := eventDeliveries(t, database, "comment.created", comment.ID)
		if len(deliveries) != 1 {
			t.Fatalf("comment.created events for %s = %#v, want one", comment.ID, deliveries)
		}
		for _, topic := range deliveries[0].Destinations {
			if topic == contracts.LegacyAgentSubject("s1") {
				routeSends++
			}
		}
		return mentionSends, routeSends
	}
	for _, comment := range comments {
		if mentionSends, routeSends := heard(comment); mentionSends != 0 || routeSends != 1 {
			t.Fatalf("comment %s reached s1 by %d mention send(s) and %d route fan-out(s), want the route alone: "+
				"its mention's resolution ran the take-over window out before the commit", comment.ID, mentionSends, routeSends)
		}
	}

	finish := []struct {
		name    string
		comment mentionedCommentRead
		run     func(t *testing.T)
	}{
		{"the delivery sweep", comments[0], func(t *testing.T) {
			// strandStoredAttempt leaves the attempt as main's sweep requires an owed one: created
			// over a minute ago, under a claim long lapsed.
			if err := direct.resumeOwedDeliveries(context.Background()); err != nil {
				t.Fatalf("delivery sweep pass: %v", err)
			}
		}},
		{"a person's Retry", comments[1], func(t *testing.T) {
			retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comments[1].ID+"/deliveries",
				map[string]any{"delivery": "steer"}, "alice")
			if retry.Code != http.StatusCreated {
				t.Fatalf("retry the pending attempt: status=%d body=%s", retry.Code, retry.Body.String())
			}
		}},
	}
	for _, tc := range finish {
		t.Run(tc.name, func(t *testing.T) {
			// The comment's sender died before it settled attempt 1, its claim long lapsed.
			strandStoredAttempt(t, database, "comment_deliveries", "comment_id", tc.comment.ID, "'5 minutes'", "'5 minutes'")
			listener.play()
			tc.run(t)

			mentionSends, routeSends := heard(tc.comment)
			states := attemptStates(t, database, "comment_deliveries", "comment_id", tc.comment.ID)
			if heardTimes := mentionSends + routeSends; heardTimes != 1 {
				t.Fatalf("session s1 received the comment %d times, want once: %d mention steer(s) and %d route "+
					"fan-out(s), attempts %v - the finished attempt resolved the mention afresh after the route was "+
					"decided on its failed resolution", heardTimes, mentionSends, routeSends, states)
			}
			if !slices.Equal(states, []string{"1:failed"}) {
				t.Fatalf("attempts = %v, want attempt 1 alone, settled failed with the resolution the route was decided on", states)
			}
		})
	}
}

// receivedDuringTakeOver posts alice's comment on issueKey mentioning s1, with input's fields beside
// the body and the mention, while the listener is starting - its registry read answers 503 once,
// and it answers from then on - and returns how many times s1 received the comment: the mention
// steers the listener published to s1, and the comment.created event's fan-outs to s1's own topic.
// attempts describes the mention's delivery attempts, for a failure. The comment waits the
// take-over out on the server's fake clock, which tries moves through the backoff between its two
// lookups.
func receivedDuringTakeOver(
	t *testing.T, listener *takeOverListener, tries *takeOverTries, handler http.Handler, database *store.Store,
	issueKey string, input map[string]any,
) (mentionSends, fanOuts int, attempts string) {
	t.Helper()
	body := map[string]any{"body": "@s1, please look at this.", "mentions": []map[string]any{{"target": "session:s1"}}}
	for field, value := range input {
		body[field] = value
	}
	listener.play(takeOverAnswer{takeOverRegistry, answerJSON(http.StatusServiceUnavailable, `{"error":"service starting"}`)})
	created := decodeMentionedComment(t, tries.answer(t, func() *httptest.ResponseRecorder {
		return postMentionedComment(t, handler, issueKey, body)
	}))

	// The mention's send runs on s1's lane after the response, and the outbox holds the
	// comment.created event while its attempt is live: both are read once it has settled. A lane
	// that waits a take-over out does so on the server's clock, which the wait moves through its
	// backoffs; a lane's lookups carry no bound of the comment's.
	settled := settledCommentDeliveriesOn(t, database, created.ID, tries.clock, &listener.calls)

	for _, send := range listener.published() {
		if send.target == "s1" {
			mentionSends++
		}
	}
	deliveries := eventDeliveries(t, database, "comment.created", created.ID)
	if len(deliveries) != 1 {
		t.Fatalf("comment.created events for %s = %#v, want one", created.ID, deliveries)
	}
	for _, topic := range deliveries[0].Destinations {
		if topic == contracts.LegacyAgentSubject("s1") {
			fanOuts++
		}
	}
	described := make([]string, 0, len(settled))
	for _, attempt := range settled {
		session := "<none>"
		if attempt.SessionID != nil {
			session = *attempt.SessionID
		}
		described = append(described, fmt.Sprintf("%s attempt %d %s to %s", attempt.Target, attempt.Attempt, attempt.State, session))
	}
	return mentionSends, fanOuts, strings.Join(described, "; ")
}

// A person's comment on an issue that session s1 claims and that has no route reaches s1 from the
// claimant route (the outbox's addClaimantRoute) unless the comment's mention of s1 delivered
// it. A mention that only resolves on its post-commit take-over retry names no session in the
// comment.created payload, so the claimant route cannot leave s1's copy to it: the comment must
// resolve its mentions through the take-over before it commits, as a routed issue's comment does.
func TestACommentDuringAListenerTakeOverReachesTheClaimantOnce(t *testing.T) {
	listener, tries, handler, database, _ := takeOverServer(t)
	issue := createInteractionIssue(t, handler, "TEST", "Claimed take-over", "before")
	if _, err := database.Pool.Exec(context.Background(), `
		update issues set claimed_by = '{"kind":"session","id":"s1"}'::jsonb, claimed_at = now() where key = $1
	`, issue.Key); err != nil {
		t.Fatalf("claim the issue for s1: %v", err)
	}

	mentionSends, fanOuts, attempts := receivedDuringTakeOver(t, listener, tries, handler, database, issue.Key, nil)
	if heard := mentionSends + fanOuts; heard != 1 {
		t.Fatalf("session s1 received the comment %d times, want once: %d mention steer(s) and %d claimant fan-out(s) "+
			"while the mention's delivery is [%s] - the mention committed unresolved and retried through the listener "+
			"take-over after commit, so the claimant route found no mentioned session to leave s1's copy to",
			heard, mentionSends, fanOuts, attempts)
	}
}

// A person's reply on an ask's thread reaches every session that follows the ask - its asker s1
// here - from the follower route (the outbox's addFollowerRoutes) unless the reply's mention
// of s1 delivered it. The reply names the ask, not a comment to reply to, so it is in a thread with
// an agent even with no route, no reply_to, no anchor and no document: it must resolve its mentions
// through the take-over before it commits too.
func TestAnAskReplyDuringAListenerTakeOverReachesTheFollowerOnce(t *testing.T) {
	listener, tries, handler, database, _ := takeOverServer(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ask thread take-over", "before")
	askResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which approach?", "actor": map[string]any{"kind": "session", "id": "s1"},
	})
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("s1 asks: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	ask := decodeBody[model.Ask](t, askResponse)

	mentionSends, fanOuts, attempts := receivedDuringTakeOver(t, listener, tries, handler, database, issue.Key,
		map[string]any{"ask_id": ask.ID})
	if heard := mentionSends + fanOuts; heard != 1 {
		t.Fatalf("session s1 received the reply %d times, want once: %d mention steer(s) and %d follower fan-out(s) "+
			"while the mention's delivery is [%s] - the mention committed unresolved and retried through the listener "+
			"take-over after commit, so the follower route found no mentioned session to leave s1's copy to",
			heard, mentionSends, fanOuts, attempts)
	}
}

// A person's comment on an issue with no route and no session claimant reaches every session the
// issue's primary document credits - s1, which wrote it, here - from the primary document route
// (the outbox's addPrimaryDocumentAuthors) unless the comment's mention of s1 delivered it. A
// comment on the issue's conversation is on no document itself, so it must resolve its mentions
// through the take-over before it commits once the issue's primary document credits a session.
func TestACommentDuringAListenerTakeOverReachesThePrimaryDocumentAuthorOnce(t *testing.T) {
	listener, tries, handler, database, _ := takeOverServer(t)
	issue := createInteractionIssue(t, handler, "TEST", "Spec author take-over", "before")
	if _, err := database.Pool.Exec(context.Background(), `
		update artifacts set created_by = '{"kind":"session","id":"s1"}'::jsonb where id = $1
	`, issue.PrimaryArtifactID); err != nil {
		t.Fatalf("credit s1 with the issue's primary document: %v", err)
	}

	mentionSends, fanOuts, attempts := receivedDuringTakeOver(t, listener, tries, handler, database, issue.Key, nil)
	if heard := mentionSends + fanOuts; heard != 1 {
		t.Fatalf("session s1 received the comment %d times, want once: %d mention steer(s) and %d primary document "+
			"author fan-out(s) while the mention's delivery is [%s] - the mention committed unresolved and retried "+
			"through the listener take-over after commit, so the author route found no mentioned session to leave "+
			"s1's copy to", heard, mentionSends, fanOuts, attempts)
	}
}

// A listener that holds the session in its registry and answers its sends "no live session" is a
// listener being replaced mid-send (retryableDeliveryFailure), so the mention's sender tries
// again inside the take-over window and settles with the last try's answer. That sender is this
// branch's lane, which runs after the comment's 201 rather than inside the request, so the
// attempt settles about one window after the response: the proof the repository runs over the
// real binary (scripts/e2e/dispatch-claimant-delivery.sh, mention-failed-claimant and
// mention-failed-follower) waits that long for the row, and the outbox publishes the session's
// own copy once it is failed.
func TestAMentionWhoseSendsTheListenerRefusesSettlesFailedAfterTheTakeOverWindow(t *testing.T) {
	var calls listenerCalls
	var mu sync.Mutex
	var sends []string
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer calls.took()()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			// The registry still holds s1: only its sends are refused.
			writeSessionRecord(t, w, `[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`, "s1")
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				IdempotencyKey string `json:"idempotency_key"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			mu.Lock()
			sends = append(sends, request.IdempotencyKey)
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no live session s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	clock := clocktest.New(time.Now())
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL, clock: clock})
	issue := createInteractionIssue(t, handler, "TEST", "Refused sends", "before")

	started := clock.Now()
	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "@s1, the listener will refuse every send of this.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	for _, attempt := range created.Deliveries {
		if attempt.State != "pending" {
			t.Fatalf("the comment answered %#v, want its attempt pending: the mention is sent after the response", attempt)
		}
	}
	settled := settledCommentDeliveriesOn(t, database, created.ID, clock, &calls)
	if len(settled) != 1 || settled[0].State != "failed" ||
		settled[0].Error == nil || *settled[0].Error != "no live session s1" {
		t.Fatalf("the refused mention settled %#v, want attempt 1 failed with the listener's own answer", settled)
	}

	mu.Lock()
	tries := slices.Clone(sends)
	mu.Unlock()
	key := created.ID + ":session:s1:steer"
	if len(tries) < 2 || slices.ContainsFunc(tries, func(k string) bool { return k != key }) {
		t.Fatalf("the listener took sends %#v, want more than one, every one under the mention's key %q: a send "+
			"the listener answers \"no live session\" is tried again inside the take-over window", tries, key)
	}
	// The sender stops once the next try would start past the window (takeoverRetry.again), so
	// the settle lands inside it; what it may not do is settle before its first backoff. Both
	// are read on the clock its waits run on, the one this test moved through them.
	if waited := clock.Now().Sub(started); waited < takeoverFirstBackoff || waited > listenerTakeoverWindow {
		t.Fatalf("the attempt settled %v after the comment, want between the first backoff (%v) and the take-over "+
			"window (%v): its sender settled on the first refusal instead of trying again, or tried on past the window",
			waited, takeoverFirstBackoff, listenerTakeoverWindow)
	}
}
