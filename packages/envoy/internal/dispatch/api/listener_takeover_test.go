package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// takeOverRoute is a listener route a take-over listener's script names: the registry read, the
// send, or any request at all.
type takeOverRoute string

const (
	takeOverRegistry takeOverRoute = "registry"
	takeOverSend     takeOverRoute = "send"
	takeOverAny      takeOverRoute = "any"
)

// takeOverAnswer is one misbehaviour a take-over listener answers the next request on its route
// with, in place of the answer it would give once the take-over is over.
type takeOverAnswer struct {
	route  takeOverRoute
	answer func(l *takeOverListener, w http.ResponseWriter, r *http.Request, send resumeSend)
}

// takeOverListener is a listener whose replacement is under way: session s1, which takes every
// mode, once it answers, and until then the script it is given - each entry answers the next
// request on its route and is spent - or, while it answers every request alike (answerEvery),
// that answer. It records every send it published, a late answer's included, and how many
// requests reached each route.
type takeOverListener struct {
	server *httptest.Server

	// calls counts the requests this listener is serving and the ones it is holding unanswered,
	// so a test whose server runs on a fake clock moves that clock through each held call's
	// window (stepListenerWaits).
	calls listenerCalls

	mu     sync.Mutex
	script []takeOverAnswer
	// every answers every request while it is set, in place of the script and of the listener's
	// own answer (answerEvery).
	every    func(l *takeOverListener, w http.ResponseWriter, r *http.Request, send resumeSend)
	requests map[takeOverRoute]int
	sends    []resumeSend
	// observe runs on every request before it is answered, with the route the request is on, so
	// a test can read the attempt row as each try finds it, or note each lookup it took.
	observe func(route takeOverRoute)
}

func startTakeOverListener(t *testing.T) *takeOverListener {
	t.Helper()
	listener := &takeOverListener{requests: map[takeOverRoute]int{}}
	listener.server = httptest.NewServer(http.HandlerFunc(listener.serve))
	t.Cleanup(listener.server.Close)
	return listener
}

const takeOverSession = `{"session_id":"s1","title":"planner","roles":[],"capabilities":["btw","aside","steer"]}`

func (l *takeOverListener) serve(w http.ResponseWriter, r *http.Request) {
	defer l.calls.took()()
	body, _ := io.ReadAll(r.Body)
	route := takeOverRoute("")
	var send resumeSend
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
		route = takeOverRegistry
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
		route = takeOverSend
		var request struct {
			TargetSession  string `json:"target_session"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		_ = json.Unmarshal(body, &request)
		send = resumeSend{key: request.IdempotencyKey, target: request.TargetSession}
	}
	l.mu.Lock()
	observe := l.observe
	l.requests[route]++
	var scripted *takeOverAnswer
	switch {
	case l.every != nil:
		scripted = &takeOverAnswer{answer: l.every}
	case len(l.script) > 0 && (l.script[0].route == takeOverAny || l.script[0].route == route):
		scripted = &l.script[0]
		l.script = l.script[1:]
	}
	l.mu.Unlock()
	if observe != nil {
		observe(route)
	}
	if scripted != nil {
		scripted.answer(l, w, r, send)
		return
	}
	switch route {
	case takeOverRegistry:
		_, _ = w.Write([]byte(takeOverSession))
	case takeOverSend:
		l.publish(w, send)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// publish answers a send the way the listener does once it holds the session: a key and
// recipient it has published before is answered as a duplicate.
func (l *takeOverListener) publish(w http.ResponseWriter, send resumeSend) {
	l.mu.Lock()
	duplicate := slices.Contains(l.sends, send)
	l.sends = append(l.sends, send)
	l.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"event_id": "takeover-envelope", "recipient": send.target, "duplicate": duplicate})
}

// refuseConnection drops the connection without an answer, which the client meets as a
// connection the listener did not serve - what a listener pod that is gone or not yet listening
// gives.
func refuseConnection(_ *takeOverListener, w http.ResponseWriter, _ *http.Request, _ resumeSend) {
	connection, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	_ = connection.Close()
}

func answerJSON(status int, body string) func(*takeOverListener, http.ResponseWriter, *http.Request, resumeSend) {
	return func(_ *takeOverListener, w http.ResponseWriter, _ *http.Request, _ resumeSend) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func (l *takeOverListener) play(script ...takeOverAnswer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.script = script
	l.requests = map[takeOverRoute]int{}
	l.sends = nil
}

// answerEvery answers every request with answer until it is called again with nil: a take-over
// that lasts until the test ends it, refusing connections (refuseConnection) or answering a
// status (answerJSON).
func (l *takeOverListener) answerEvery(answer func(l *takeOverListener, w http.ResponseWriter, r *http.Request, send resumeSend)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.every = answer
}

func (l *takeOverListener) requestsTo(route takeOverRoute) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.requests[route]
}

func (l *takeOverListener) published() []resumeSend {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.sends)
}

// deliveryReceiptsOf counts the delivery receipts the events hold for attempt 1 of a message or
// comment: receiptType is message.delivery or comment.delivery, field the payload key naming it.
func deliveryReceiptsOf(t *testing.T, database *store.Store, receiptType, field, id string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events
		where type = $1 and payload ->> $2 = $3 and (payload ->> 'attempt')::int = 1
	`, receiptType, field, id).Scan(&count); err != nil {
		t.Fatalf("count the receipts of %s: %v", id, err)
	}
	return count
}

// listenerCalls is how many calls a stand-in listener is serving and how many of those it is
// holding - a call it will never answer, which nothing but the window on the server's clock ends.
// A stand-in records both so that a test whose server runs on a fake clock can move that clock
// through the sender's waits without cutting a call that would have been answered
// (stepListenerWaits).
type listenerCalls struct {
	mu   sync.Mutex
	open int
	held int
}

// took records a call the stand-in has taken and answers the function that records its end.
func (c *listenerCalls) took() func() {
	c.mu.Lock()
	c.open++
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		c.open--
		c.mu.Unlock()
	}
}

// holding records that the call the stand-in is serving is one it will never answer, and answers
// the function that records its end.
func (c *listenerCalls) holding() func() {
	c.mu.Lock()
	c.held++
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		c.held--
		c.mu.Unlock()
	}
}

// counts is how many calls the stand-in is serving and how many of those it is holding.
func (c *listenerCalls) counts() (open, held int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.open, c.held
}

// stepListenerWaits moves fake, a test server's clock, through the waits its sender is in while a
// stand-in listener answers nothing: the window the listener client gives each call the stand-in
// holds, and the take-over's backoff between two tries. A test calls it from the probe of the
// wait for what the sender writes, so every move is made on the test's goroutine, and it names no
// span of its own: it only brings forward a deadline the product armed.
//
// It moves the clock only where the move can cut no call that would have been answered:
//   - while every call in flight is one the stand-in holds, it steps to the earliest deadline
//     pending on the fake. That is a held call's window, or something due sooner, which the move
//     fires instead; the wait's next pass steps again, so the windows run out without the test
//     naming any of them.
//   - with no call in flight, it fires only a deadline no further off than takeoverMaxBackoff,
//     which is the take-over's backoff before the next try. A listener call's window (five
//     seconds) is further off, so a call already on its way to the stand-in, whose window is
//     armed before the request is written, is never cut short by this.
//
// Either rule can also fire the bound of a comment's own mention lookup while that lookup's
// request is still on its way to the stand-in: the request makes the lookup before its
// transaction (resolveCommentDeliveries), bounded by mentionLookupTimeout - as near as the first
// backoff - and arms that bound before the stand-in has taken the call. A caller whose stand-in
// would answer that lookup moves the clock only once the request's lookups are done, which is
// once the comment's attempts are committed.
func stepListenerWaits(fake *clocktest.Fake, calls *listenerCalls) {
	armed := fake.Armed()
	if len(armed) == 0 {
		return
	}
	open, held := calls.counts()
	switch {
	case held > 0 && open == held:
		fake.Set(armed[0])
	case open == 0 && !armed[0].After(fake.Now().Add(takeoverMaxBackoff)):
		fake.Set(armed[0])
	}
}

// answerWhileCallsAreHeld waits for a request running on a goroutine of its own, whose listener
// calls a stand-in holds, moving fake - the server's clock - through the waits those calls leave
// the request in (stepListenerWaits) until it answers.
func answerWhileCallsAreHeld(
	t *testing.T, fake *clocktest.Fake, calls *listenerCalls, answered <-chan *httptest.ResponseRecorder,
) *httptest.ResponseRecorder {
	t.Helper()
	return testwait.Until(t, "the request to answer once the calls its stand-in holds had run out of window",
		func() (*httptest.ResponseRecorder, bool) {
			select {
			case response := <-answered:
				return response, true
			default:
			}
			stepListenerWaits(fake, calls)
			return nil, false
		})
}

// A listener being replaced refuses connections, answers 503, or does not hold a session yet for
// a few seconds. A send that meets one of those waits out the take-over under the attempt it
// holds and its own key, and reaches the session once the listener answers. Anything else the
// listener says - the session does not take this mode, the send itself is refused - is its
// answer, and the attempt settles failed on that first try. The server runs the window every
// deployment runs, on a fake clock: the test moves it through each try's backoff and through the
// window of the call the late answer holds, so every try a case's take-over takes happens with
// no wall clock spent on it.
func TestAListenerTakeOverIsWaitedOutAndARefusalSettlesAtOnce(t *testing.T) {
	listener := startTakeOverListener(t)
	clock := clocktest.New(time.Now())
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.server.URL, clock: clock})

	lateAnswer := func(l *takeOverListener, w http.ResponseWriter, r *http.Request, send resumeSend) {
		// The listener publishes before it answers, and this answer misses the client's window:
		// the call is held until that window, on the server's clock, runs out.
		l.mu.Lock()
		l.sends = append(l.sends, send)
		l.mu.Unlock()
		answered := l.calls.holding()
		<-r.Context().Done()
		answered()
	}
	for _, tc := range []struct {
		name   string
		script []takeOverAnswer
		// failure is the error a refusal settles with; empty for a take-over the send waits out.
		failure       string
		wantDuplicate bool
		// triesRoute is the route a refusal answers on, which it asks exactly once.
		triesRoute takeOverRoute
	}{
		{name: "the listener refuses the connection", script: []takeOverAnswer{{takeOverAny, refuseConnection}}},
		{name: "the listener is starting", script: []takeOverAnswer{
			{takeOverRegistry, answerJSON(http.StatusServiceUnavailable, `{"error":"service starting"}`)},
		}},
		{name: "the listener does not hold the session yet", script: []takeOverAnswer{
			{takeOverRegistry, answerJSON(http.StatusNotFound, `{"error":"no live session s1"}`)},
		}},
		{name: "the send finds no live session yet", script: []takeOverAnswer{
			{takeOverSend, answerJSON(http.StatusNotFound, `{"error":"no live session s1"}`)},
		}},
		{name: "the send meets the listener's starting gate", script: []takeOverAnswer{
			{takeOverSend, answerJSON(http.StatusServiceUnavailable, `{"error":"service starting"}`)},
			{takeOverSend, answerJSON(http.StatusServiceUnavailable, `{"error":"service starting"}`)},
		}},
		{name: "a gateway answers for the listener", script: []takeOverAnswer{
			{takeOverSend, func(_ *takeOverListener, w http.ResponseWriter, _ *http.Request, _ resumeSend) {
				w.WriteHeader(http.StatusBadGateway)
			}},
		}},
		{name: "the send's receipt timed out", script: []takeOverAnswer{{takeOverSend, lateAnswer}}, wantDuplicate: true},
		{name: "the session does not take the mode", script: []takeOverAnswer{
			{takeOverRegistry, answerJSON(http.StatusOK, `{"session_id":"s1","title":"planner","capabilities":["btw"]}`)},
		}, failure: "session s1 (planner) does not advertise steer", triesRoute: takeOverRegistry},
		{name: "the listener refuses the send", script: []takeOverAnswer{
			{takeOverSend, answerJSON(http.StatusBadRequest, `{"error":"message is required"}`)},
		}, failure: "message is required", triesRoute: takeOverSend},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener.play(tc.script...)
			sent := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				sent <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages",
					map[string]any{"body": tc.name, "delivery": "steer"}, "alice")
			}()
			response := answerWhileCallsAreHeld(t, clock, &listener.calls, sent)
			if response.Code != http.StatusCreated {
				t.Fatalf("send: status=%d body=%s", response.Code, response.Body.String())
			}
			message := decodeBody[model.Message](t, response)
			if len(message.Deliveries) != 1 {
				t.Fatalf("deliveries = %#v, want attempt 1 alone", message.Deliveries)
			}
			attempt := message.Deliveries[0]
			states := attemptStates(t, database, "message_deliveries", "message_id", message.ID)
			if tc.failure != "" {
				if attempt.State != "failed" || attempt.Error == nil || *attempt.Error != tc.failure {
					t.Fatalf("attempt = %#v, want failed with %q", attempt, tc.failure)
				}
				if tries := listener.requestsTo(tc.triesRoute); tries != 1 {
					t.Fatalf("the listener's %s was asked %d times, want once: a refusal is the listener's answer", tc.triesRoute, tries)
				}
				if !slices.Equal(states, []string{"1:failed"}) {
					t.Fatalf("attempts = %v, want attempt 1 alone, failed", states)
				}
				return
			}
			if attempt.Attempt != 1 || attempt.State != "sent" || attempt.Duplicate != tc.wantDuplicate || attempt.SessionID != "s1" {
				t.Fatalf("attempt = %#v, want attempt 1 sent to s1 (duplicate %v) once the take-over ended", attempt, tc.wantDuplicate)
			}
			if !slices.Equal(states, []string{"1:sent"}) {
				t.Fatalf("attempts = %v, want attempt 1 alone, sent", states)
			}
			key := message.ID + ":steer"
			for _, send := range listener.published() {
				if send != (resumeSend{key: key, target: "s1"}) {
					t.Fatalf("published %#v, want every send to s1 under %q", listener.published(), key)
				}
			}
			if receipts := deliveryReceiptsOf(t, database, "message.delivery", "message_id", message.ID); receipts != 1 {
				t.Fatalf("attempt 1 has %d receipts, want one", receipts)
			}
		})
	}
}

// With no listener configured there is no take-over to wait out: the send settles failed on its
// one try. The server runs the production window on a fake clock the test never moves, so a send
// that waited out any part of a take-over would never answer at all.
func TestASendWithNoListenerConfiguredSettlesAtOnce(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{clock: clocktest.New(time.Now())})
	sent := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		sent <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages",
			map[string]any{"body": "Nobody listens", "delivery": "steer"}, "alice")
	}()
	response := testwait.Receive(t, "the send to settle with no listener configured", sent)
	if response.Code != http.StatusCreated {
		t.Fatalf("send: status=%d body=%s", response.Code, response.Body.String())
	}
	message := decodeBody[model.Message](t, response)
	if len(message.Deliveries) != 1 || message.Deliveries[0].State != "failed" {
		t.Fatalf("deliveries = %#v, want attempt 1 failed", message.Deliveries)
	}
}

// A sender waiting out a take-over renews its claim before each try and settles on the claim it
// renewed last, so the attempt never reads as abandoned while its sender is still trying: the
// delivery sweep, run during the wait, sends nothing and opens nothing beside it, and the attempt
// reaches its session once, under attempt 1, with one receipt. The listener refuses until two
// tries have found the attempt held and a sweep pass has run, so the try it answers is the fourth
// and the claim is read at four points across three renewals. The server runs the window every
// deployment runs, on a fake clock: each try's backoff passes when the test moves that clock, so
// the tries cost no wall clock and no window is shortened to fit them.
func TestASenderWaitingOutAListenerTakeOverRenewsItsClaimBeforeEachTry(t *testing.T) {
	for _, tc := range []struct {
		name, table, column, receipt, path string
		body                               map[string]any
		key                                func(id string) string
		// answersPending is the mention, whose send runs on its recipient's lane after the
		// response: the request answers its attempt pending, and the claims its tries renewed
		// and the state they settled are read once that send has finished. A direct message is
		// sent inside the request, which answers the attempt it settled.
		answersPending bool
	}{
		{
			name: "a direct message", table: "message_deliveries", column: "message_id", receipt: "message.delivery",
			path: "/api/v1/agents/s1/messages",
			body: map[string]any{"body": "Sent during a take-over", "delivery": "steer"},
			key:  func(id string) string { return id + ":steer" },
		},
		{
			// The comment route is the issue's, created below.
			name: "a mention", table: "comment_deliveries", column: "comment_id", receipt: "comment.delivery",
			body:           map[string]any{"body": "Mentioned during a take-over", "mentions": []map[string]any{{"target": "session:s1"}}},
			key:            func(id string) string { return id + ":session:s1:steer" },
			answersPending: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := startTakeOverListener(t)
			clock := clocktest.New(time.Now())
			handler, database, deps := newTestServer(t, testServerOptions{
				envoyURL: listener.server.URL, clock: clock,
			})
			path := tc.path
			if path == "" {
				issue := createInteractionIssue(t, handler, "TEST", "A take-over", "before")
				path = "/api/v1/issues/" + issue.Key + "/comments"
			}
			var claimsMu sync.Mutex
			var claims []time.Time
			listener.mu.Lock()
			listener.observe = func(takeOverRoute) {
				var claimed *time.Time
				err := database.Pool.QueryRow(context.Background(),
					`select claimed_at from `+tc.table+` where state = 'pending' and attempt = 1`).Scan(&claimed)
				if err == nil && claimed != nil {
					claimsMu.Lock()
					claims = append(claims, *claimed)
					claimsMu.Unlock()
				}
			}
			listener.mu.Unlock()
			listener.answerEvery(refuseConnection)

			data, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatalf("encode the request: %v", err)
			}
			answered := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-Dispatch-User", "alice")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				answered <- response
			}()
			// The clock stays where it is until the attempt the tries hold is committed. A
			// comment's request looks its mention up before its transaction
			// (resolveCommentDeliveries), bounded by mentionLookupTimeout on the server's clock -
			// a deadline as near as a backoff, which stepListenerWaits fires whenever the stand-in
			// is serving nothing, as it is while that lookup's request is still on its way to it.
			// A lookup cut so reaches the stand-in later all the same, and its read of the claim is
			// one no try of the sender's made. The attempt is written after every lookup the
			// request makes, so a move made once it stands cuts none of them.
			testwait.Eventually(t, "the attempt the sender's tries hold to be committed", func() bool {
				var committed bool
				if err := database.Pool.QueryRow(context.Background(),
					`select exists (select from `+tc.table+` where state = 'pending' and attempt = 1)`,
				).Scan(&committed); err != nil {
					t.Fatalf("read the attempt the sender's tries hold: %v", err)
				}
				return committed
			})
			// Two tries the sender made while holding the attempt, each after the backoff this
			// wait moves the clock through.
			testwait.Eventually(t, "two tries of the sender that found its attempt held", func() bool {
				stepListenerWaits(clock, &listener.calls)
				claimsMu.Lock()
				defer claimsMu.Unlock()
				return len(claims) >= 2
			})

			// The attempt is old enough for the delivery sweep, and only its live claim keeps it
			// the waiting sender's.
			if _, err := database.Pool.Exec(context.Background(),
				`update `+tc.table+` set created_at = created_at - interval '5 minutes' where state = 'pending'`,
			); err != nil {
				t.Fatalf("age the attempt: %v", err)
			}
			if err := directServer(deps).resumeOwedDeliveries(context.Background()); err != nil {
				t.Fatalf("delivery sweep pass during the wait: %v", err)
			}
			var id string
			if err := database.Pool.QueryRow(context.Background(),
				`select `+tc.column+`::text from `+tc.table+` where state = 'pending'`).Scan(&id); err != nil {
				t.Fatalf("read the waiting attempt: %v", err)
			}
			if states := attemptStates(t, database, tc.table, tc.column, id); !slices.Equal(states, []string{"1:pending"}) {
				t.Fatalf("attempts after a sweep pass during the wait = %v, want attempt 1 alone, still the waiting sender's", states)
			}

			listener.answerEvery(nil)
			response := answerWhileCallsAreHeld(t, clock, &listener.calls, answered)
			if response.Code != http.StatusCreated {
				t.Fatalf("send: status=%d body=%s", response.Code, response.Body.String())
			}
			if tc.answersPending {
				settledCommentDeliveriesOn(t, database, id, clock, &listener.calls)
			}
			claimsMu.Lock()
			observed := slices.Clone(claims)
			claimsMu.Unlock()
			if len(observed) < 4 {
				t.Fatalf("claimed_at was read %d times, want at least four: at the two refused tries, then at the answered try's registry read and its send", len(observed))
			}
			for index := 1; index < len(observed); index++ {
				if !observed[index].After(observed[index-1]) {
					t.Fatalf("claimed_at as each try found it = %v, want it later at every try: the claim is renewed before each", observed)
				}
			}
			if states := attemptStates(t, database, tc.table, tc.column, id); !slices.Equal(states, []string{"1:sent"}) {
				t.Fatalf("attempts = %v, want attempt 1 alone, sent on the claim renewed last", states)
			}
			if sends := listener.published(); len(sends) != 1 || sends[0] != (resumeSend{key: tc.key(id), target: "s1"}) {
				t.Fatalf("published %#v, want one send to s1 under %q", sends, tc.key(id))
			}
			if receipts := deliveryReceiptsOf(t, database, tc.receipt, tc.column, id); receipts != 1 {
				t.Fatalf("attempt 1 has %d receipts, want one", receipts)
			}
			answeredState := `"state":"sent"`
			if tc.answersPending {
				answeredState = `"state":"pending"`
			}
			if !strings.Contains(response.Body.String(), answeredState) {
				t.Fatalf("the request answered %s, want its attempt %s", response.Body.String(), answeredState)
			}
		})
	}
}
