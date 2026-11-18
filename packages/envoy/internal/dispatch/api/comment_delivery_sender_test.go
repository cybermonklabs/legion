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
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// senderTestSessions are the live sessions the mention sender's stand-in listener knows, each
// able to receive a steer, and s1 a BTW too, so a Retry of its mention in another mode resolves
// (TestARetryInAnotherModeThatTimedOutLeavesTheRecipientOneCopy).
const senderTestSessions = `[{"session_id":"s1","title":"planner","capabilities":["steer","btw"]},` +
	`{"session_id":"s2","title":"reviewer","capabilities":["steer"]},` +
	`{"session_id":"s3","title":"tester","capabilities":["steer"]}]`

// mentionListener is a stand-in Envoy listener for the mention sender's tests. It answers for
// senderTestSessions, resolves role:reviewer to s1, records every send in the order the sends
// arrive, holds a send to a held session until the test releases it, and holds a lookup of a held
// path the same way.
type mentionListener struct {
	mu          sync.Mutex
	sends       []mentionListenerSend
	held        map[string]chan struct{}
	heldLookups map[string]chan struct{}
	// refused holds the sessions whose sends the listener answers 503: a send the recipient's
	// host refuses, which fails its attempt.
	refused map[string]struct{}
	// stalls holds the next send to a session before the listener records it at all, so a send
	// the test has not released has not reached the recipient yet.
	stalls map[string]*mentionStall
	// answers holds the ANSWER to the next send to a session, which the listener records first:
	// that send reached the recipient and only its receipt is outstanding (holdAnswer).
	answers map[string]*mentionStall
	// dedupe makes the listener answer duplicate for a send repeating the key of one it already
	// took for that session, as the stream does inside contracts.DeliveryDuplicateWindow
	// (dropRepeats); delivered holds the keys it has taken.
	dedupe    bool
	delivered map[string]struct{}
	arrived   chan string
	// calls counts the calls this stand-in is serving and the ones it is holding, for a test
	// whose server runs on a fake clock: it moves that clock through each held call's window
	// (stepListenerWaits). A call held by hold or holdLookups counts as held, since a test that
	// drives the clock is one whose stand-in never answers those.
	calls listenerCalls
	// answeredLookups counts, by path, the lookups this stand-in has written an answer to, so a
	// test moves the server's clock past a lookup bound only once the lookups it wants answered
	// have been (commentWithAnUnansweredLookup).
	answeredLookups map[string]int
}

// mentionStall is one held-back send: reached is closed when that send arrives at the listener,
// and release when the test lets it be recorded and answered.
type mentionStall struct {
	reached chan struct{}
	release chan struct{}
}

// mentionListenerSend is one send the listener took: the session it was for, its body, the
// idempotency key it went under, and whether the listener answered it as a repeat of a key it
// already held for that session, which means the recipient was not handed it again (dropRepeats).
type mentionListenerSend struct {
	session, body, key string
	duplicate          bool
}

// is reports whether the send carried body to session.
func (s mentionListenerSend) is(session, body string) bool {
	return s.session == session && s.body == body
}

func newMentionListener(t *testing.T) (*httptest.Server, *mentionListener) {
	t.Helper()
	state := &mentionListener{
		held: map[string]chan struct{}{}, heldLookups: map[string]chan struct{}{},
		refused:         map[string]struct{}{},
		stalls:          map[string]*mentionStall{},
		answers:         map[string]*mentionStall{},
		delivered:       map[string]struct{}{},
		arrived:         make(chan string, 64),
		answeredLookups: map[string]int{},
	}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer state.calls.took()()
		if r.Method == http.MethodGet {
			state.mu.Lock()
			gate := state.heldLookups[r.URL.Path]
			state.mu.Unlock()
			if gate != nil {
				answered := state.calls.holding()
				select {
				case <-gate:
				case <-r.Context().Done():
					answered()
					return
				}
				answered()
			}
		}
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			writeSessionRecord(t, w, senderTestSessions, strings.TrimPrefix(r.URL.Path, "/v1/registry/"))
			state.answeredLookup(r.URL.Path)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/reviewer":
			_, _ = w.Write([]byte(`{"role":"reviewer","holder":"s1","title":"planner","capabilities":["steer"]}`))
			state.answeredLookup(r.URL.Path)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(senderTestSessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				TargetSession  string `json:"target_session"`
				Message        string `json:"message"`
				IdempotencyKey string `json:"idempotency_key"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode mention send: %v", err)
			}
			state.mu.Lock()
			stall := state.stalls[request.TargetSession]
			delete(state.stalls, request.TargetSession)
			state.mu.Unlock()
			if stall != nil {
				close(stall.reached)
				select {
				case <-stall.release:
				case <-r.Context().Done():
					return
				}
			}
			state.mu.Lock()
			_, refused := state.refused[request.TargetSession]
			// A refused send is handed to nobody, so it leaves the recipient's host no key.
			repeat := false
			if state.dedupe && !refused {
				frame := request.TargetSession + " " + request.IdempotencyKey
				_, repeat = state.delivered[frame]
				state.delivered[frame] = struct{}{}
			}
			state.sends = append(state.sends, mentionListenerSend{
				session: request.TargetSession, body: request.Message,
				key: request.IdempotencyKey, duplicate: repeat,
			})
			gate := state.held[request.TargetSession]
			answer := state.answers[request.TargetSession]
			delete(state.answers, request.TargetSession)
			state.mu.Unlock()
			state.arrived <- request.TargetSession
			if answer != nil {
				close(answer.reached)
				select {
				case <-answer.release:
				case <-r.Context().Done():
					return
				}
			}
			if gate != nil {
				answered := state.calls.holding()
				select {
				case <-gate:
				case <-r.Context().Done():
					answered()
					return
				}
				answered()
			}
			if refused {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"the recipient's host refused the frame"}`))
				return
			}
			if repeat {
				_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"` +
					request.TargetSession + `","duplicate":true}`))
				return
			}
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"` + request.TargetSession + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(listener.Close)
	return listener, state
}

// hold makes every send to sessions wait until the returned release is called; release may be
// called more than once.
func (l *mentionListener) hold(sessions ...string) func() {
	gate := make(chan struct{})
	l.mu.Lock()
	for _, session := range sessions {
		l.held[session] = gate
	}
	l.mu.Unlock()
	return sync.OnceFunc(func() { close(gate) })
}

// holdAnswer holds the ANSWER to the next send to session: the listener records that send, so the
// frame reached the recipient, and only its receipt waits. landed is closed when it is recorded;
// release lets the listener answer it, and may be called more than once. Every other send is
// answered as usual, so a lane behind it is not held with it.
func (l *mentionListener) holdAnswer(session string) (landed <-chan struct{}, release func()) {
	answer := &mentionStall{reached: make(chan struct{}), release: make(chan struct{})}
	l.mu.Lock()
	l.answers[session] = answer
	l.mu.Unlock()
	return answer.reached, sync.OnceFunc(func() { close(answer.release) })
}

// dropRepeats makes the listener answer duplicate for a send that repeats the key of one it has
// already taken for that session, as the stream and the hosts do inside
// contracts.DeliveryDuplicateWindow: that frame is not handed to the recipient a second time, and
// the attempt behind it records a duplicate.
func (l *mentionListener) dropRepeats() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dedupe = true
}

// refuseSends makes every send to session from now on answer the refusal its host gave, so the
// attempt behind it fails on that one try: the refusal is the listener's own answer, which
// retryableDeliveryFailure settles at once, rather than a 503, which a sender reads as a listener
// being replaced and tries again inside the take-over window, so a test counting the sends of one
// comment would count a try the take-over timing decides.
func (l *mentionListener) refuseSends(session string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refused[session] = struct{}{}
}

// allowSends answers sends to session normally again: the recipient's host is back, and a send
// that reaches the listener from now on lands.
func (l *mentionListener) allowSends(session string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.refused, session)
}

// stall holds the next send to session before the listener records it - a send that has not
// reached the recipient at all - and lets every later send through. reached is closed when that
// send arrives; release lets it be recorded and answered, and may be called more than once.
func (l *mentionListener) stall(session string) (reached <-chan struct{}, release func()) {
	stall := &mentionStall{reached: make(chan struct{}), release: make(chan struct{})}
	l.mu.Lock()
	l.stalls[session] = stall
	l.mu.Unlock()
	return stall.reached, sync.OnceFunc(func() { close(stall.release) })
}

// holdLookups makes every GET of path wait until the returned release is called; release may be
// called more than once.
func (l *mentionListener) holdLookups(path string) func() {
	gate := make(chan struct{})
	l.mu.Lock()
	l.heldLookups[path] = gate
	l.mu.Unlock()
	return sync.OnceFunc(func() { close(gate) })
}

// answeredLookup records that the stand-in has written its answer to a lookup of path.
func (l *mentionListener) answeredLookup(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.answeredLookups[path]++
}

// hasAnswered reports whether the stand-in has written an answer to a lookup of path.
func (l *mentionListener) hasAnswered(path string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.answeredLookups[path] > 0
}

// awaitSend waits for the next send to arrive and returns the session it is for.
func (l *mentionListener) awaitSend(t *testing.T) string {
	t.Helper()
	return testwait.Receive(t, "the next mention send to reach the listener", l.arrived)
}

func (l *mentionListener) sent() []mentionListenerSend {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.sends)
}

// unclaimedPending reports whether the comment's attempt for target is pending with no sender's
// claim on it: the shape of an attempt whose send the mention sender refused, and of one whose
// claim lapsed.
func unclaimedPending(t *testing.T, database *store.Store, commentID, target string) bool {
	t.Helper()
	var unclaimed bool
	if err := database.Pool.QueryRow(context.Background(), `
		select state = 'pending' and claimed_at is null from comment_deliveries
		where comment_id = $1 and target = $2 and attempt = 1
	`, commentID, target).Scan(&unclaimed); err != nil {
		t.Fatalf("read the attempt of %s for %s: %v", commentID, target, err)
	}
	return unclaimed
}

// claimedPending reports whether the comment's attempt for target is pending under a live claim:
// committed and claimed, which is how the mention sender holds a send waiting on its lane, and how
// a sender holds the attempt its send is in flight for.
func claimedPending(t *testing.T, database *store.Store, commentID, target string) bool {
	t.Helper()
	var claimed bool
	if err := database.Pool.QueryRow(context.Background(), `
		select state = 'pending' and not `+model.DeliveryClaimFreeSQL+` from comment_deliveries
		where comment_id = $1 and target = $2 and attempt = 1
	`, commentID, target).Scan(&claimed); err != nil {
		t.Fatalf("read the attempt of %s for %s: %v", commentID, target, err)
	}
	return claimed
}

// attemptClaimedAt is when the comment's attempt for target was last claimed.
func attemptClaimedAt(t *testing.T, database *store.Store, commentID, target string) time.Time {
	t.Helper()
	var claimed time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		select claimed_at from comment_deliveries where comment_id = $1 and target = $2 and attempt = 1
	`, commentID, target).Scan(&claimed); err != nil {
		t.Fatalf("read the claim on the attempt of %s for %s: %v", commentID, target, err)
	}
	return claimed
}

// allSent reports whether every attempt settled sent.
func allSent(attempts []model.CommentDelivery) bool {
	for _, attempt := range attempts {
		if attempt.State != "sent" {
			return false
		}
	}
	return len(attempts) > 0
}

// The listener calls a mention's send makes - the recheck and the send itself, each up to the
// listener client's window - run after the comment's response. A comment mentioning three
// sessions whose sends are all held answers with its attempts pending, and they settle once the
// sends are released: a create that waited for its sends would never answer, since nothing
// releases them until this test does.
func TestMentionSendsRunAfterTheCommentHasAnswered(t *testing.T) {
	listener, state := newMentionListener(t)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	release := state.hold("s1", "s2", "s3")
	defer release()
	issue := createInteractionIssue(t, handler, "TEST", "Mentions after the answer", "before")

	posted := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		posted <- postMentionedComment(t, handler, issue.Key, map[string]any{
			"body":     "All three of you.",
			"mentions": []map[string]any{{"target": "session:s1"}, {"target": "session:s2"}, {"target": "session:s3"}},
		})
	}()
	response := testwait.Receive(t, "the comment to answer while its three mention sends are held", posted)
	created := decodeMentionedComment(t, response)
	if len(created.Deliveries) != 3 {
		t.Fatalf("the create answered deliveries %#v, want the three attempts", created.Deliveries)
	}
	for _, attempt := range created.Deliveries {
		if attempt.State != "pending" || attempt.Attempt != 1 {
			t.Fatalf("the create answered attempt %#v, want attempt 1 pending", attempt)
		}
	}
	for range 3 {
		state.awaitSend(t)
	}

	release()
	settled := settledCommentDeliveries(t, database, created.ID)
	if len(settled) != 3 || !allSent(settled) {
		t.Fatalf("attempts once the sends were released = %#v, want all three sent", settled)
	}
}

// One recipient gets the comments it is mentioned in in the order they were accepted, also when
// one names it by role and the next by session: both resolve to the same session, and so to the
// same lane. While the first comment's send is held, the second's attempt waits on that lane
// under this process's claim, with its send not started.
func TestMentionSendsReachOneRecipientInTheOrderItsCommentsWereAccepted(t *testing.T) {
	listener, state := newMentionListener(t)
	// A clock that stands still: the first comment's lookup of the role is answered as the
	// listener answers it and is never left unanswered by its bound (mentionLookupTimeout), which
	// would key its send by the role's string and put the two comments on two lanes. Every send
	// it holds is released by the test, so no window of the listener client's ends one either.
	handler, database := newTargetedMessageHandlerOn(t, listener.URL, clocktest.Held())
	release := state.hold("s1")
	defer release()
	issue := createInteractionIssue(t, handler, "TEST", "Mentions in order", "before")

	first := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "first", "mentions": []map[string]any{{"target": "role:reviewer"}},
	}))
	if session := state.awaitSend(t); session != "s1" {
		t.Fatalf("the first send went to %s, want s1", session)
	}
	second := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "second", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if !claimedPending(t, database, second.ID, "session:s1") {
		t.Fatal("the second comment's attempt is not held while the first's send to the same session is still held")
	}
	// The second comment's send waits behind the first on their one lane. A send that jumped the
	// lane would reach the listener now, and the stand-in reports each arrival as it is taken, so
	// its arrivals stay quiet while the first is held; the count after it is the first alone.
	testwait.Quiet(t, "a second send to s1 while the first is held", state.arrived, 500*time.Millisecond,
		"the mention sender keeps a lane's queued sends in its own memory, which no route or row reads, so no "+
			"state says the second send is waiting rather than on its way, and every later action of the test "+
			"releases the first send, after which the second may go")
	if sent := state.sent(); len(sent) != 1 {
		t.Fatalf("sends while the first was still held = %#v, want the first alone", sent)
	}

	release()
	settledCommentDeliveries(t, database, first.ID)
	settledCommentDeliveries(t, database, second.ID)
	if sent := state.sent(); len(sent) != 2 || !sent[0].is("s1", "first") ||
		!sent[1].is("s1", "second") {
		t.Fatalf("sends = %#v, want first then second, both to s1", sent)
	}
}

// A recipient whose send is held - a slow or unanswering session - does not hold back the
// sends to anyone else.
func TestABlockedMentionRecipientDoesNotHoldBackAnother(t *testing.T) {
	listener, state := newMentionListener(t)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	release := state.hold("s1")
	defer release()
	issue := createInteractionIssue(t, handler, "TEST", "A slow recipient", "before")

	blocked := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "For the slow one.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	state.awaitSend(t)
	other := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "For the other one.", "mentions": []map[string]any{{"target": "session:s2"}},
	}))
	if settled := waitForCommentDeliveries(t, database, other.ID, "the other recipient's attempt sent", allSent); settled[0].SessionID == nil ||
		*settled[0].SessionID != "s2" {
		t.Fatalf("the other recipient's attempt = %#v, want it sent to s2", settled)
	}
	if attempts := waitForCommentDeliveries(t, database, blocked.ID, "the held attempt", func([]model.CommentDelivery) bool {
		return true
	}); len(attempts) != 1 || attempts[0].State != "pending" {
		t.Fatalf("the held recipient's attempt = %#v, want it still pending behind its held send", attempts)
	}

	release()
	if settled := settledCommentDeliveries(t, database, blocked.ID); !allSent(settled) {
		t.Fatalf("the held recipient's attempt once released = %#v, want it sent", settled)
	}
}

// A comment's mention lookups all run at once, so its mentions cost the comment one listener
// round trip rather than one each: the listener holds each session's first lookup until all
// three have arrived, which a comment that looked its mentions up one after another never gets
// past.
func TestMentionLookupsOfOneCommentRunAtOnce(t *testing.T) {
	var mu sync.Mutex
	looked := map[string]bool{}
	together := make(chan struct{})
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			session := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
			mu.Lock()
			first := !looked[session]
			looked[session] = true
			arrived := len(looked)
			mu.Unlock()
			// Only the comment's own lookup of each session waits for the others; the recheck
			// its send makes later is not this test's.
			if first {
				if arrived == 3 {
					close(together)
				}
				select {
				case <-together:
				case <-r.Context().Done():
					return
				}
			}
			writeSessionRecord(t, w, senderTestSessions, session)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	// A clock that stands still: a lookup the listener holds waits until the listener answers it
	// and is never ended by its bound (mentionLookupTimeout), so a comment that looked its
	// mentions up one after another never answers, where a real bound would end each held lookup
	// in turn and let it through.
	handler, database := newTargetedMessageHandlerOn(t, listener.URL, clocktest.Held())
	issue := createInteractionIssue(t, handler, "TEST", "Concurrent lookups", "before")

	posted := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		posted <- postMentionedComment(t, handler, issue.Key, map[string]any{
			"body":     "Three lookups.",
			"mentions": []map[string]any{{"target": "session:s1"}, {"target": "session:s2"}, {"target": "session:s3"}},
		})
	}()
	created := decodeMentionedComment(t, testwait.Receive(t,
		"the comment to answer once its three mention lookups had all reached the listener together", posted))
	settledCommentDeliveries(t, database, created.ID)
	mu.Lock()
	defer mu.Unlock()
	if len(looked) != 3 {
		t.Fatalf("the listener was asked for %d sessions, want the comment's three mentions", len(looked))
	}
}

// unansweredLookupComment is what one comment whose listener lookup went unanswered in time came
// to: its comment.created payload, every send the listener took, and the topics the outbox
// published comment.created to.
type unansweredLookupComment struct {
	payload      model.CommentEventPayload
	sends        []mentionListenerSend
	destinations []string
}

// unansweredLookup is the comment commentWithAnUnansweredLookup posts: one mentioning mention, on
// an issue whose route is route ("" for none), while the listener holds every lookup of hungPath
// unanswered. replyToS1 makes the comment a reply to a root comment session s1 wrote. pinned says
// the mention's own lookup is not the held one, so it answers in time and pins s1.
type unansweredLookup struct {
	route, mention, hungPath string
	replyToS1                bool
	pinned                   bool
}

// commentWithAnUnansweredLookup posts the comment lookup describes. It checks that every lookup
// the comment makes is bounded by mentionLookupTimeout, that the comment answers once that bound
// has run out for the held lookups with its attempt pending - with the mention unresolved, or
// pinned to s1 when lookup.pinned - then lets the listener answer, waits for the mention sender
// to send the attempt to s1, and runs the outbox until it has published comment.created. The
// listener resolves every session of senderTestSessions, and role:reviewer to s1.
//
// The server runs on a fake clock, and the test moves it to the lookups' bound only once every
// lookup it does not hold has been answered and the server has taken that answer, so no real
// time decides which of the comment's lookups counts as answered.
func commentWithAnUnansweredLookup(t *testing.T, lookup unansweredLookup) unansweredLookupComment {
	t.Helper()
	listener, state := newMentionListener(t)
	letAnswer := state.holdLookups(lookup.hungPath)
	defer letAnswer()
	clock := clocktest.New(time.Now())
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL, clock: clock})
	issue := createInteractionIssue(t, handler, "TEST", "An unanswered lookup", "before")
	if lookup.route != "" {
		if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
			"route": lookup.route,
		}, "alice"); patched.Code != http.StatusOK {
			t.Fatalf("set the route: status=%d body=%s", patched.Code, patched.Body.String())
		}
	}
	input := map[string]any{"body": "Are you there?", "mentions": []map[string]any{{"target": lookup.mention}}}
	if lookup.replyToS1 {
		root := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"body": "Where should this go?", "actor": map[string]any{"kind": "session", "id": "s1"},
		})
		input["reply_to"] = decodeMentionedComment(t, root).ID
	}

	// The comment looks up its mention and, when the route is not one of its mentions, the
	// route, all at once (resolveCommentDeliveries), each bounded by mentionLookupTimeout on the
	// server's clock from this moment. The lookups of hungPath are never answered; every other
	// one is.
	targets := []string{lookup.mention}
	if lookup.route != "" && lookup.route != lookup.mention {
		targets = append(targets, lookup.route)
	}
	var answering []string
	held := 0
	for _, target := range targets {
		if path := mentionLookupPath(t, target); path == lookup.hungPath {
			held++
		} else {
			answering = append(answering, path)
		}
	}
	bound := clock.Now().Add(mentionLookupTimeout)
	posted := make(chan *httptest.ResponseRecorder, 1)
	go func() { posted <- postMentionedComment(t, handler, issue.Key, input) }()
	// The bound is pending for exactly mentionLookupTimeout from the start for every lookup in
	// flight, and an answered lookup's bound ends once the server has taken the answer. So the
	// clock moves to it once the stand-in has answered every lookup it does not hold, is holding
	// every lookup it holds (which armed its bound before it was sent), and only the held lookups'
	// bounds are still pending; a lookup with no bound, or another one, never gets there, and the
	// comment then answers on that bound alone.
	testwait.Until(t, fmt.Sprintf("the lookups of %v answered and taken, and the %d held lookups' bounds pending at %v",
		answering, held, bound.Format(time.RFC3339Nano)), func() ([]time.Time, bool) {
		armed := clock.Armed()
		if _, holding := state.calls.counts(); holding != held {
			return armed, false
		}
		for _, path := range answering {
			if !state.hasAnswered(path) {
				return armed, false
			}
		}
		pending := 0
		for _, at := range armed {
			if at.Equal(bound) {
				pending++
			}
		}
		return armed, pending == held
	})
	clock.Set(bound)
	created := decodeMentionedComment(t, testwait.Receive(t, "the comment to answer once its held lookups' bound ran out", posted))
	pinnedTo := func(sessionID *string) bool {
		if lookup.pinned {
			return sessionID != nil && *sessionID == "s1"
		}
		return sessionID == nil
	}
	if len(created.Mentions) != 1 || !pinnedTo(created.Mentions[0].SessionID) {
		t.Fatalf("mentions = %#v, want the one mention pinned to s1 only when its own lookup answered (%v)",
			created.Mentions, lookup.pinned)
	}
	if len(created.Deliveries) != 1 || created.Deliveries[0].State != "pending" ||
		!pinnedTo(created.Deliveries[0].SessionID) || created.Deliveries[0].Error != nil {
		t.Fatalf("deliveries = %#v, want one pending attempt with no failure, on s1 only when pinned (%v)",
			created.Deliveries, lookup.pinned)
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("read events: status=%d body=%s", events.Code, events.Body.String())
	}
	var payload *model.CommentEventPayload
	for _, event := range decodeBody[[]struct {
		Type    string                    `json:"type"`
		Payload model.CommentEventPayload `json:"payload"`
	}](t, events) {
		if event.Type == "comment.created" && event.Payload.ID == created.ID {
			payload = &event.Payload
		}
	}
	if payload == nil {
		t.Fatalf("no comment.created event for %s: %s", created.ID, events.Body.String())
	}
	if len(payload.Mentions) != 1 || !pinnedTo(payload.Mentions[0].SessionID) {
		t.Fatalf("comment.created mentions = %#v, want the one mention pinned to s1 only when its own lookup answered (%v)",
			payload.Mentions, lookup.pinned)
	}

	letAnswer()
	settled := settledCommentDeliveries(t, database, created.ID)
	if len(settled) != 1 || settled[0].State != "sent" || settled[0].SessionID == nil || *settled[0].SessionID != "s1" {
		t.Fatalf("the mention once the listener answered = %#v, want it sent to s1", settled)
	}
	published := eventDeliveries(t, database, "comment.created", created.ID)
	if len(published) != 1 {
		t.Fatalf("comment.created deliveries = %#v, want the one event's", published)
	}
	return unansweredLookupComment{payload: *payload, sends: state.sent(), destinations: published[0].Destinations}
}

// mentionLookupPath is the path the mention stand-in answers a lookup of target on: a session's
// registry record, or a role's holder.
func mentionLookupPath(t *testing.T, target string) string {
	t.Helper()
	kind, id, _ := strings.Cut(target, ":")
	switch kind {
	case "session":
		return "/v1/registry/" + id
	case "role":
		return "/v1/roles/" + id
	}
	t.Fatalf("mention target %q is neither a session nor a role", target)
	return ""
}

// A listener whose mention lookup never answers delays the comment by at most
// mentionLookupTimeout, and the mention sender delivers the mention after the response once the
// listener answers. When the issue's route is the very string the comment mentions, both name
// one session, so the event suppresses the route by that string and the session hears the
// comment once, from the mention.
func TestAnUnansweredMentionOfTheRouteItselfSuppressesTheRouteCopy(t *testing.T) {
	got := commentWithAnUnansweredLookup(t, unansweredLookup{
		route: "session:s1", mention: "session:s1", hungPath: "/v1/registry/s1",
	})
	if !got.payload.SuppressRoute || got.payload.SuppressedRoute != "session:s1" ||
		got.payload.SuppressedRouteSessionID != nil {
		t.Fatalf("comment.created suppresses route %v (%q, session %v), want session:s1 suppressed by its string",
			got.payload.SuppressRoute, got.payload.SuppressedRoute, got.payload.SuppressedRouteSessionID)
	}
	if len(got.sends) != 1 || got.sends[0].session != "s1" {
		t.Fatalf("the listener took sends %#v, want the one mention's, to s1", got.sends)
	}
	if slices.Contains(got.destinations, contracts.LegacyAgentSubject("s1")) {
		t.Fatalf("the outbox published comment.created to %v, want s1's topic left out: the mention delivered it",
			got.destinations)
	}
}

// A route that names the mentioned session by a different string - here session:s1, while the
// comment mentions role:reviewer, which s1 holds - is not suppressed when the mention's lookup
// goes unanswered: no session is known to compare, so the payload names none. The mention's own
// sender pins s1 on the attempt row when it sends, and the outbox reads that row rather than the
// payload, so the route copy to s1 is left to the mention and s1 hears the comment once.
func TestAnUnansweredMentionOfAnotherStringForTheRouteIsLeftToThatMention(t *testing.T) {
	got := commentWithAnUnansweredLookup(t, unansweredLookup{
		route: "session:s1", mention: "role:reviewer", hungPath: "/v1/roles/reviewer",
	})
	if got.payload.SuppressRoute || got.payload.SuppressedRoute != "" || got.payload.SuppressedRouteSessionID != nil {
		t.Fatalf("comment.created suppresses route %v (%q, session %v), want no route suppressed",
			got.payload.SuppressRoute, got.payload.SuppressedRoute, got.payload.SuppressedRouteSessionID)
	}
	if len(got.sends) != 1 || got.sends[0].session != "s1" {
		t.Fatalf("the listener took sends %#v, want the one mention's, to s1", got.sends)
	}
	if slices.Contains(got.destinations, contracts.LegacyAgentSubject("s1")) {
		t.Fatalf("the outbox published comment.created to %v, want s1's topic left out: the attempt row names s1 though the payload's mention names nobody, and the mention delivered the comment",
			got.destinations)
	}
}

// A reply that mentions session:s1, the session that wrote its thread's root, names s1 even when
// s1's lookup goes unanswered: the comment's mentions name s1, so the author route leaves s1 to
// the mention, and s1 hears the reply once, from it.
func TestAnUnansweredSessionMentionOfTheRootAuthorSuppressesTheAuthorCopy(t *testing.T) {
	got := commentWithAnUnansweredLookup(t, unansweredLookup{
		mention: "session:s1", hungPath: "/v1/registry/s1", replyToS1: true,
	})
	if len(got.sends) != 1 || got.sends[0].session != "s1" {
		t.Fatalf("the listener took sends %#v, want the one mention's, to s1", got.sends)
	}
	if slices.Contains(got.destinations, contracts.LegacyAgentSubject("s1")) {
		t.Fatalf("the outbox published comment.created to %v, want s1's topic left out: the mention delivered it",
			got.destinations)
	}
}

// A reply that mentions role:reviewer, which the root author s1 holds, names no session in its
// payload when the role's lookup goes unanswered. The mention's sender pins s1 on the attempt
// row when it sends, which is what the author route reads, so s1 is left to the mention there
// too and hears the reply once.
func TestAnUnansweredRoleMentionHeldByTheRootAuthorIsLeftToThatMention(t *testing.T) {
	got := commentWithAnUnansweredLookup(t, unansweredLookup{
		mention: "role:reviewer", hungPath: "/v1/roles/reviewer", replyToS1: true,
	})
	if len(got.sends) != 1 || got.sends[0].session != "s1" {
		t.Fatalf("the listener took sends %#v, want the one mention's, to s1", got.sends)
	}
	if slices.Contains(got.destinations, contracts.LegacyAgentSubject("s1")) {
		t.Fatalf("the outbox published comment.created to %v, want s1's topic left out: the attempt row names s1 though the payload's mention names nobody, and the mention delivered the reply",
			got.destinations)
	}
}

// A session:s1 route whose own lookup goes unanswered still names s1, so a mention that resolves
// to s1 under another string - role:reviewer, which s1 holds - suppresses the route copy for s1:
// s1 hears the comment once, from the mention.
func TestARouteWhoseOwnLookupGoesUnansweredIsSuppressedForTheSessionItNames(t *testing.T) {
	got := commentWithAnUnansweredLookup(t, unansweredLookup{
		route: "session:s1", mention: "role:reviewer", hungPath: "/v1/registry/s1", pinned: true,
	})
	if !got.payload.SuppressRoute || got.payload.SuppressedRoute != "session:s1" ||
		got.payload.SuppressedRouteSessionID == nil || *got.payload.SuppressedRouteSessionID != "s1" {
		t.Fatalf("comment.created suppresses route %v (%q, session %v), want session:s1 suppressed for s1",
			got.payload.SuppressRoute, got.payload.SuppressedRoute, got.payload.SuppressedRouteSessionID)
	}
	if len(got.sends) != 1 || got.sends[0].session != "s1" {
		t.Fatalf("the listener took sends %#v, want the one mention's, to s1", got.sends)
	}
	if slices.Contains(got.destinations, contracts.LegacyAgentSubject("s1")) {
		t.Fatalf("the outbox published comment.created to %v, want s1's topic left out: the mention delivered it",
			got.destinations)
	}
}

// A listener that answers no lookup at all delays a mentioned comment by at most
// mentionLookupTimeout, also on an issue whose route is not one of the comment's mentions: the
// route's own lookup runs beside the mentions' lookups, not after them, so the two timeouts do not
// add up. Both bounds are therefore armed on the server's clock at once, for the same moment; a
// route looked up after the mention would arm its bound only once the mention's had run out. The
// clock moves to that moment once the stand-in holds both lookups, and no further: the comment
// answers there.
func TestAListenerThatAnswersNoLookupDelaysARoutedCommentByOneTimeoutAtMost(t *testing.T) {
	listener, state := newMentionListener(t)
	letMentionAnswer := state.holdLookups("/v1/registry/s1")
	defer letMentionAnswer()
	letRouteAnswer := state.holdLookups("/v1/registry/s2")
	defer letRouteAnswer()
	clock := clocktest.New(time.Now())
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL, clock: clock})
	issue := createInteractionIssue(t, handler, "TEST", "A silent listener", "before")
	if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
		"route": "session:s2",
	}, "alice"); patched.Code != http.StatusOK {
		t.Fatalf("set the route: status=%d body=%s", patched.Code, patched.Body.String())
	}

	bound := clock.Now().Add(mentionLookupTimeout)
	posted := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		posted <- postMentionedComment(t, handler, issue.Key, map[string]any{
			"body": "Is anyone there?", "mentions": []map[string]any{{"target": "session:s1"}},
		})
	}()
	// Each lookup arms its bound and then the listener client's own window (envoy.CallWindow)
	// before its request leaves, so the deadlines pending say the two lookups are in flight only
	// once two of them stand at the bound. The stand-in holding both says their requests reached
	// it: moved sooner, the clock would end a lookup the listener never saw.
	testwait.Until(t, fmt.Sprintf("the stand-in to hold the mention's and the route's lookups, both bounded at %v",
		bound.Format(time.RFC3339Nano)), func() ([]time.Time, bool) {
		armed := clock.Armed()
		_, holding := state.calls.counts()
		return armed, holding == 2 && len(slices.DeleteFunc(slices.Clone(armed), func(at time.Time) bool { return !at.Equal(bound) })) == 2
	})
	clock.Set(bound)
	created := decodeMentionedComment(t, testwait.Receive(t, "the comment to answer once its lookups' bound ran out", posted))

	letMentionAnswer()
	letRouteAnswer()
	settledCommentDeliveries(t, database, created.ID)
}

// ageAttempts moves the creation of the comments' attempts a claim lease and a second into the
// past: the delivery sweep takes an attempt only once it has waited that long.
func ageAttempts(t *testing.T, database *store.Store, commentIDs ...string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set created_at = created_at - `+model.DeliveryClaimLapseInterval+` - interval '1 second'
		where comment_id = any($1)
	`, commentIDs); err != nil {
		t.Fatalf("age the attempts: %v", err)
	}
}

// A process that stops with mention sends still queued leaves their attempts pending with no
// claim. The next process's delivery sweep sends such an attempt once it has waited a claim
// lease, and leaves alone one whose claim is live, which another process's sender holds.
func TestTheSweepSendsAMentionAStoppedProcessLeftAndLeavesALiveClaimAlone(t *testing.T) {
	listener, state := newMentionListener(t)
	stopping, stop := context.WithCancel(t.Context())
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL, lifetime: stopping})
	issue := createInteractionIssue(t, handler, "TEST", "Recovered mentions", "before")

	// The process is stopping: the comments commit, and no send of theirs starts.
	stop()
	stranded := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Accepted while stopping.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	held := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Held by another sender.", "mentions": []map[string]any{{"target": "session:s2"}},
	}))
	if !unclaimedPending(t, database, stranded.ID, "session:s1") || !unclaimedPending(t, database, held.ID, "session:s2") {
		t.Fatal("a comment accepted while the process stopped had its attempt taken")
	}
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set claimed_at = now() where comment_id = $1
	`, held.ID); err != nil {
		t.Fatalf("claim the held attempt as another live sender: %v", err)
	}
	ageAttempts(t, database, stranded.ID, held.ID)
	next, stopNext := context.WithCancel(t.Context())
	restarted := register(http.NewServeMux(), next, deps)
	if err := restarted.resumeOwedDeliveries(context.Background()); err != nil {
		t.Fatalf("resume the owed deliveries: %v", err)
	}
	settled := waitForCommentDeliveries(t, database, stranded.ID, "the owed attempt sent", allSent)
	stopNext()
	restarted.waitBackground()

	if settled[0].SessionID == nil || *settled[0].SessionID != "s1" {
		t.Fatalf("the recovered attempt = %#v, want it sent to s1", settled)
	}
	if attempts := waitForCommentDeliveries(t, database, held.ID, "the held attempt", func([]model.CommentDelivery) bool {
		return true
	}); len(attempts) != 1 || attempts[0].State != "pending" {
		t.Fatalf("the attempt another sender holds = %#v, want it left pending", attempts)
	}
	if sent := state.sent(); len(sent) != 1 || sent[0].session != "s1" {
		t.Fatalf("sends = %#v, want the owed attempt's one send to s1", sent)
	}
}

// A rolling update starts the successor before the old process is stopped. SIGTERM ends the old
// process's lifetime before its HTTP drain, so a comment that commits during that drain is
// refused by the old process's sender and its attempt stays pending with no claim. The
// successor's delivery sweep sends it, once.
func TestARollingUpdatesSuccessorSendsTheMentionTheOldProcessAcceptedWhileDraining(t *testing.T) {
	listener, state := newMentionListener(t)
	stopping, stop := context.WithCancel(t.Context())
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL, lifetime: stopping})
	issue := createInteractionIssue(t, handler, "TEST", "A rolling update", "before")

	// The successor is up first, on a lifetime of its own.
	successorLifetime, stopSuccessor := context.WithCancel(t.Context())
	successor := register(http.NewServeMux(), successorLifetime, deps)
	defer func() {
		stopSuccessor()
		successor.waitBackground()
	}()

	// SIGTERM: the old process's lifetime ends, and then its HTTP drain answers a comment.
	stop()
	drained := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Accepted during the HTTP drain.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if !unclaimedPending(t, database, drained.ID, "session:s1") {
		t.Fatal("a comment the old process accepted after its lifetime ended had its attempt taken before the successor's sweep")
	}
	// The sweep leaves an attempt younger than a claim lease to the process that accepted it; this
	// one has waited that long once the test moves its creation back a lease.
	ageAttempts(t, database, drained.ID)

	if err := successor.resumeOwedDeliveries(context.Background()); err != nil {
		t.Fatalf("resume the owed deliveries: %v", err)
	}
	settled := waitForCommentDeliveries(t, database, drained.ID, "the attempt sent by the successor", allSent)
	if settled[0].SessionID == nil || *settled[0].SessionID != "s1" {
		t.Fatalf("the successor's send = %#v, want it sent to s1", settled)
	}
	if sent := state.sent(); len(sent) != 1 || !sent[0].is("s1", "Accepted during the HTTP drain.") {
		t.Fatalf("sends = %#v, want the drained comment's one send to s1", sent)
	}
}

// A busy sender holds the claim of every send waiting on its lanes, so a sweep pass running
// meanwhile leaves each of them to it and each reaches its recipient once, in the order its
// comment was accepted.
func TestTheSweepLeavesABusySendersUnstartedSendsToIt(t *testing.T) {
	listener, state := newMentionListener(t)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
	issue := createInteractionIssue(t, handler, "TEST", "A busy sender", "before")
	release := state.hold("s1")
	defer release()
	post := func(body string) string {
		return decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
			"body": body, "mentions": []map[string]any{{"target": "session:s1"}},
		})).ID
	}

	sending := post("in flight")
	state.awaitSend(t)
	queued := []string{post("queued first"), post("queued second")}
	for _, comment := range queued {
		if !claimedPending(t, database, comment, "session:s1") {
			t.Fatalf("the attempt of %s behind the held send is not held by the sender", comment)
		}
	}
	if err := served.resumeOwedDeliveries(context.Background()); err != nil {
		t.Fatalf("resume the owed deliveries: %v", err)
	}

	release()
	for _, comment := range append([]string{sending}, queued...) {
		if settled := settledCommentDeliveries(t, database, comment); !allSent(settled) {
			t.Fatalf("comment %s attempts = %#v, want it sent", comment, settled)
		}
	}
	if sent := state.sent(); len(sent) != 3 || sent[0].body != "in flight" || sent[1].body != "queued first" ||
		sent[2].body != "queued second" {
		t.Fatalf("listener sends = %#v, want each comment sent once, in order", sent)
	}
}

// A mention past the sender's queue bound is not queued: the claim its attempt committed with is
// given back, so the attempt is pending and unclaimed, and the delivery sweep, once the queue has
// room and the attempt has waited a claim lease, sends it, once.
func TestAMentionPastTheQueueBoundIsSentOnceTheQueueDrains(t *testing.T) {
	listener, state := newMentionListener(t)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
	served.mentions.mu.Lock()
	served.mentions.capacity = 1
	served.mentions.mu.Unlock()
	release := state.hold("s1")
	defer release()
	issue := createInteractionIssue(t, handler, "TEST", "A full queue", "before")
	post := func(body string) string {
		return decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
			"body": body, "mentions": []map[string]any{{"target": "session:s1"}},
		})).ID
	}

	sending := post("in flight")
	state.awaitSend(t)
	queued := post("queued")
	refused := post("past the bound")
	if !claimedPending(t, database, queued, "session:s1") {
		t.Fatal("the attempt behind the held send is not held by the sender")
	}
	if !unclaimedPending(t, database, refused, "session:s1") {
		t.Fatal("the attempt past the queue's bound kept the claim it committed with")
	}

	release()
	for _, comment := range []string{sending, queued} {
		if settled := settledCommentDeliveries(t, database, comment); !allSent(settled) {
			t.Fatalf("comment %s attempts = %#v, want it sent", comment, settled)
		}
	}
	if !unclaimedPending(t, database, refused, "session:s1") {
		t.Fatal("the attempt past the bound was taken before it waited a claim lease")
	}
	ageAttempts(t, database, refused)
	if err := served.resumeOwedDeliveries(context.Background()); err != nil {
		t.Fatalf("resume the owed deliveries: %v", err)
	}
	if settled := settledCommentDeliveries(t, database, refused); !allSent(settled) {
		t.Fatalf("comment %s attempts = %#v, want it sent by the delivery sweep", refused, settled)
	}
	if sent := state.sent(); len(sent) != 3 || sent[0].body != "in flight" || sent[1].body != "queued" ||
		sent[2].body != "past the bound" {
		t.Fatalf("sends = %#v, want each comment sent once, in order", sent)
	}
}

// runOutboxBeside runs the outbox beside the test, as a Dispatch process runs it beside its API,
// publishing through publisher every event it decides on, until the test ends.
func runOutboxBeside(t *testing.T, deps Deps, publisher outbox.Publisher) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		outbox.New(outbox.Deps{
			Store: deps.Store, Publisher: publisher, Broker: events.NewBroker(),
			Listen: deps.Store.Pool.Listener,
		}).Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
}

// withMentionClaimRefresh gives served a mention sender that writes the claims of its queued sends
// again every period, in place of the one register built, which uses mentionClaimRefresh (a third
// of a claim lease). A test calls it before its first comment, while the sender it replaces holds
// nothing, and the server's background wait waits the replacement out.
func withMentionClaimRefresh(t *testing.T, served *server, period time.Duration) {
	t.Helper()
	served.mentions.wait()
	served.mentions = newMentionSender(served.lifetime, period,
		served.sendCommentMention, served.refreshMentionClaims, served.releaseMentionClaims)
}

// ageQueuedClaims moves every claim the sender holds on a send that has not started back by by,
// on the attempt row and in the sender's own record together: what a lane held that long leaves
// behind when nothing writes those claims again. It holds the sender's lock throughout, so no
// refresh records anything until every row and its record agree again.
func ageQueuedClaims(t *testing.T, database *store.Store, sender *mentionSender, by time.Duration) {
	t.Helper()
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.claims) == 0 {
		t.Fatal("the mention sender holds no queued send whose claim could age")
	}
	for key := range sender.claims {
		// A refresh whose statement ran but whose result is not recorded yet leaves the row newer
		// than the map (refreshHeldClaims), so the claim aged is the one the row carries, and the
		// map is set to it: that refresh then finds the map changed and records nothing over it.
		var aged time.Time
		if err := database.Pool.QueryRow(context.Background(), `
			update comment_deliveries set claimed_at = claimed_at - make_interval(secs => $4)
			where comment_id = $1 and target = $2 and attempt = $3 and state = 'pending' and claimed_at is not null
			returning claimed_at
		`, key.comment, key.target, key.attempt, by.Seconds()).Scan(&aged); err != nil {
			t.Fatalf("age the claim on the queued attempt of %s for %s: %v", key.comment, key.target, err)
		}
		sender.claims[key] = aged
	}
}

// awaitRefreshedClaims waits until every comment's attempt for target is under a live claim
// again: the sender's refresh loop has written the claims it holds.
func awaitRefreshedClaims(t *testing.T, database *store.Store, commentIDs []string, target string) {
	t.Helper()
	testwait.Eventually(t, fmt.Sprintf("the sender to write the claims of %d queued attempts again", len(commentIDs)), func() bool {
		for _, comment := range commentIDs {
			if !claimedPending(t, database, comment, target) {
				return false
			}
		}
		return true
	})
}

// postMentionOfS1 posts a comment on the issue mentioning s1 and answers its id.
func postMentionOfS1(t *testing.T, handler http.Handler, issueKey, body string) string {
	t.Helper()
	return decodeMentionedComment(t, postMentionedComment(t, handler, issueKey, map[string]any{
		"body": body, "mentions": []map[string]any{{"target": "session:s1"}},
	})).ID
}

// A recipient's comments reach it in the order they were accepted however long its first send is
// held. The comments behind that send wait on the recipient's lane under the claim their attempts
// committed with, and the sender's refresh loop (keepClaims) writes each of those claims again
// before it lapses, so a lane held longer than a claim lease keeps them: a delivery sweep pass
// that runs while every attempt is old enough to be owed takes none of them and sends nothing
// past the send in flight.
func TestAHeldLaneKeepsItsQueuedCommentsInOrderThroughASweep(t *testing.T) {
	listener, state := newMentionListener(t)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
		// A clock that stands still: every send this test stalls is released by the test, and
		// no window of the listener client's may settle one behind its back.
		envoyURL: listener.URL, clock: clocktest.Held(),
	})
	withMentionClaimRefresh(t, served, 20*time.Millisecond)
	issue := createInteractionIssue(t, handler, "TEST", "A held lane", "before")
	reached, release := state.stall("s1")
	defer release()

	// The first comment's send is at the listener, which has not taken it: s1 holds no copy yet.
	sending := postMentionOfS1(t, handler, issue.Key, "first")
	<-reached
	queued := []string{
		postMentionOfS1(t, handler, issue.Key, "second"),
		postMentionOfS1(t, handler, issue.Key, "third"),
	}
	for _, comment := range queued {
		if !claimedPending(t, database, comment, "session:s1") {
			t.Fatalf("the attempt of %s waiting on the held lane is not held by the sender", comment)
		}
	}
	// The lane has now been held longer than a claim lease: every attempt was opened that long
	// ago, and the claims the sender took on the sends waiting on it are that old too.
	comments := append([]string{sending}, queued...)
	ageAttempts(t, database, comments...)
	ageQueuedClaims(t, database, served.mentions, model.DeliveryClaimLapse+time.Second)
	awaitRefreshedClaims(t, database, queued, "session:s1")
	if err := served.resumeOwedDeliveries(context.Background()); err != nil {
		t.Fatalf("resume the owed deliveries: %v", err)
	}

	release()
	for _, comment := range comments {
		if settled := settledCommentDeliveries(t, database, comment); !allSent(settled) {
			t.Fatalf("comment %s attempts = %#v, want it sent", comment, settled)
		}
	}
	if sent := state.sent(); len(sent) != 3 || !sent[0].is("s1", "first") ||
		!sent[1].is("s1", "second") || !sent[2].is("s1", "third") {
		t.Fatalf("sends = %#v, want each comment sent to s1 once, in the order they were accepted", sent)
	}
}

// The claims written again are what keeps the sweep off a held lane. With nothing writing them -
// the process that queued the sends gone, or its refresh loop stopped - the attempts waiting on
// the lane fall to the delivery sweep as soon as their claims lapse, and its pass sends them past
// the send still in flight, out of the order the comments were accepted.
func TestASweepTakesTheQueuedMentionsOfALaneWhoseClaimsLapsed(t *testing.T) {
	listener, state := newMentionListener(t)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
		// A clock that stands still: every send this test stalls is released by the test, and
		// no window of the listener client's may settle one behind its back.
		envoyURL: listener.URL, clock: clocktest.Held(),
	})
	// A sender whose refresh cannot tick while the test runs: nothing writes its claims again.
	withMentionClaimRefresh(t, served, time.Hour)
	issue := createInteractionIssue(t, handler, "TEST", "A lapsed lane", "before")
	reached, release := state.stall("s1")
	defer release()

	sending := postMentionOfS1(t, handler, issue.Key, "first")
	<-reached
	queued := []string{
		postMentionOfS1(t, handler, issue.Key, "second"),
		postMentionOfS1(t, handler, issue.Key, "third"),
	}
	ageAttempts(t, database, append([]string{sending}, queued...)...)
	ageQueuedClaims(t, database, served.mentions, model.DeliveryClaimLapse+time.Second)
	if err := served.resumeOwedDeliveries(context.Background()); err != nil {
		t.Fatalf("resume the owed deliveries: %v", err)
	}

	// The pass sent them itself, in whichever order its workers took them, while the lane's own
	// first send is still at the listener.
	for _, comment := range queued {
		if settled := settledCommentDeliveries(t, database, comment); !allSent(settled) {
			t.Fatalf("comment %s attempts = %#v, want the sweep to have sent it", comment, settled)
		}
	}
	swept := state.sent()
	bodies := make([]string, 0, len(swept))
	for _, send := range swept {
		bodies = append(bodies, send.body)
	}
	slices.Sort(bodies)
	if !slices.Equal(bodies, []string{"second", "third"}) {
		t.Fatalf("sends while the lane is still held = %#v, want the sweep's two, ahead of the send in flight", swept)
	}

	release()
	if settled := settledCommentDeliveries(t, database, sending); !allSent(settled) {
		t.Fatalf("the send in flight = %#v, want it sent", settled)
	}
	if sent := state.sent(); len(sent) != 3 || sent[2].body != "first" {
		t.Fatalf("sends = %#v, want the comment accepted first delivered last: the sweep overtook it", sent)
	}
}

// A keyed repeat of a comment whose mention is still waiting on a lane claims that attempt anew
// before it queues it, and the sender takes the claim the repeat wrote over for the send it
// already holds: its next refresh writes that claim again, so the attempt stays this process's
// and the delivery sweep leaves it alone. The repeat queues no second send.
func TestAKeyedRepeatOfAQueuedMentionLeavesTheSenderHoldingTheNewClaim(t *testing.T) {
	listener, state := newMentionListener(t)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
		// A clock that stands still: every send this test stalls is released by the test, and
		// no window of the listener client's may settle one behind its back.
		envoyURL: listener.URL, clock: clocktest.Held(),
	})
	// Only the refreshes this test makes itself run.
	withMentionClaimRefresh(t, served, time.Hour)
	issue := createInteractionIssue(t, handler, "TEST", "A repeated comment", "before")
	reached, release := state.stall("s1")
	defer release()

	postMentionOfS1(t, handler, issue.Key, "first")
	<-reached
	keyed := map[string]any{"body": "second", "mentions": []map[string]any{{"target": "session:s1"}}}
	comments := "/api/v1/issues/" + issue.Key + "/comments"
	queued := decodeMentionedComment(t,
		keyedRequest(t, handler, http.MethodPost, comments, keyed, "alice", "mention-queued")).ID
	if !claimedPending(t, database, queued, "session:s1") {
		t.Fatal("the attempt waiting on the held lane is not held by the sender")
	}
	// The lane has held it longer than a claim lease and nothing wrote its claim again, so the
	// attempt reads as one no sender holds.
	ageQueuedClaims(t, database, served.mentions, model.DeliveryClaimLapse+time.Second)

	repeat := keyedRequest(t, handler, http.MethodPost, comments, keyed, "alice", "mention-queued")
	if repeat.Code != http.StatusCreated || !replayed(repeat) {
		t.Fatalf("the repeat: status=%d body=%s, want the comment replayed", repeat.Code, repeat.Body.String())
	}
	if !claimedPending(t, database, queued, "session:s1") {
		t.Fatal("the repeat left the queued attempt unclaimed")
	}
	claimed := attemptClaimedAt(t, database, queued, "session:s1")

	served.mentions.refreshHeldClaims(context.Background())
	if again := attemptClaimedAt(t, database, queued, "session:s1"); !again.After(claimed) {
		t.Fatalf("the claim on the queued attempt is %v, want it written again past the repeat's %v: "+
			"the sender is holding a claim the repeat replaced", again, claimed)
	}

	release()
	settled := settledCommentDeliveries(t, database, queued)
	if len(settled) != 1 || !allSent(settled) {
		t.Fatalf("the repeated comment's attempts = %#v, want its one attempt sent", settled)
	}
	if sent := state.sent(); len(sent) != 2 || sent[0].body != "first" || sent[1].body != "second" {
		t.Fatalf("sends = %#v, want each comment sent to s1 once, in the order they were accepted", sent)
	}
}

// A person's Retry of a mention whose first attempt is still waiting on a held lane opens the
// attempt that is sent. When the lane reaches the first attempt it is no longer its mention's
// latest, so the lane sends nothing, settles it failed as superseded and gives its claim back:
// the recipient is handed one copy, the Retry's.
func TestARetryWhileTheFirstAttemptIsQueuedLeavesTheRecipientOneCopy(t *testing.T) {
	listener, state := newMentionListener(t)
	handler, database, _, _ := newRegisteredTestServer(t, testServerOptions{
		// A clock that stands still: every send this test stalls is released by the test, and
		// no window of the listener client's may settle one behind its back.
		envoyURL: listener.URL, clock: clocktest.Held(),
	})
	issue := createInteractionIssue(t, handler, "TEST", "A retried mention", "before")
	reached, release := state.stall("s1")
	defer release()

	postMentionOfS1(t, handler, issue.Key, "first")
	<-reached
	retried := postMentionOfS1(t, handler, issue.Key, "retried")
	if !claimedPending(t, database, retried, "session:s1") {
		t.Fatal("the attempt waiting on the held lane is not held by the sender")
	}

	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+retried+"/deliveries",
		map[string]any{"delivery": "steer"}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry the mention: status=%d body=%s", retry.Code, retry.Body.String())
	}

	release()
	settled := settledCommentDeliveries(t, database, retried)
	if len(settled) != 2 || settled[0].Attempt != 1 || settled[1].Attempt != 2 {
		t.Fatalf("the retried mention's attempts = %#v, want its first and the Retry's", settled)
	}
	if settled[0].State != "failed" || settled[0].Error == nil ||
		!strings.Contains(*settled[0].Error, "superseded") {
		t.Fatalf("the attempt the Retry overtook = %#v, want it failed as superseded", settled[0])
	}
	if settled[1].State != "sent" {
		t.Fatalf("the Retry's attempt = %#v, want it sent", settled[1])
	}
	if !unclaimedSettled(t, database, retried, "session:s1", 1) {
		t.Fatal("the superseded attempt kept the claim its queued send held")
	}
	// The Retry's send reached the listener while the first comment's was still stalled there.
	if sent := state.sent(); len(sent) != 2 || sent[0].body != "retried" || sent[1].body != "first" {
		t.Fatalf("sends = %#v, want the Retry's one copy of the retried comment beside the first comment's", sent)
	}
}

// unclaimedSettled reports whether the comment's attempt for target is settled with its claim
// given back: the shape of an attempt a sender settled without sending it.
func unclaimedSettled(t *testing.T, database *store.Store, commentID, target string, attempt int) bool {
	t.Helper()
	var unclaimed bool
	if err := database.Pool.QueryRow(context.Background(), `
		select state <> 'pending' and claimed_at is null from comment_deliveries
		where comment_id = $1 and target = $2 and attempt = $3
	`, commentID, target, attempt).Scan(&unclaimed); err != nil {
		t.Fatalf("read attempt %d of %s for %s: %v", attempt, commentID, target, err)
	}
	return unclaimed
}

// A stop that outlasts the drain still gives back the claims of the sends the sender never
// started: that release runs on a context of its own, not the one the drain's timeout cancels,
// so the attempts are left pending and unclaimed for the next delivery sweep pass rather than
// held by a claim nothing writes again.
func TestTheMentionDrainGivesBackItsClaimsAfterItsSendContextIsCancelled(t *testing.T) {
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	started, finish := make(chan struct{}), make(chan struct{})
	var released []mentionClaim
	var releaseErr error
	sender := newMentionSender(lifetime, time.Hour,
		func(context.Context, mentionSend) {
			close(started)
			<-finish
		},
		func(context.Context, []mentionClaim) ([]mentionClaim, error) { return nil, nil },
		func(ctx context.Context, held []mentionClaim) error {
			releaseErr = ctx.Err()
			released = append(released, held...)
			return ctx.Err()
		},
	)
	queued := mentionSend{
		comment: model.Comment{ID: "queued"}, mention: ResolvedMention{Target: "session:s1"},
		attempt: 1, claimed: time.Now(),
	}
	sender.enqueue(context.Background(), mentionSend{
		comment: model.Comment{ID: "in flight"}, mention: ResolvedMention{Target: "session:s1"},
		attempt: 1, claimed: time.Now(),
	})
	<-started
	sender.enqueue(context.Background(), queued)

	// SIGTERM, then a stop that takes longer than the drain: the sends' context is cancelled
	// before the drain gets to the claims it still holds.
	stop()
	sender.cancel()
	close(finish)
	sender.wait()

	if releaseErr != nil {
		t.Fatalf("the drain released the claims on a context that was already %v, want a live one", releaseErr)
	}
	if len(released) != 1 || released[0].key != queued.key() {
		t.Fatalf("the drain gave back %#v, want the claim of the send it never started", released)
	}
}
