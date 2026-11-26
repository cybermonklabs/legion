package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// checkStep is what one resolution of a heartbeat's check answers (scriptedCheckIdentity).
type checkStep func(*http.Request) error

var (
	// checkFindsPoolBusy is a check whose read of the caller's session found no connection free.
	checkFindsPoolBusy checkStep = func(*http.Request) error {
		return fmt.Errorf("read the caller's session: %w", store.ErrPoolBusy)
	}
	// checkHangs is a check whose database never answers, so only the check's own bound ends it.
	checkHangs checkStep = func(r *http.Request) error {
		<-r.Context().Done()
		return fmt.Errorf("read the caller's session: %w", r.Context().Err())
	}
	// checkLosesItsConnection is a check whose read of the caller's session lost its connection
	// under it, as the store names such a read: a statement the pool ran outside a transaction,
	// whose session Postgres ended after it was sent (store.ErrStatementOutcomeUnknown).
	checkLosesItsConnection checkStep = func(*http.Request) error {
		ended := &pgconn.PgError{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "57P01", Message: "terminating connection due to administrator command"}
		return fmt.Errorf("read the caller's session: %s pool: %w: the connection ended under it: %w", store.RequestsPool, store.ErrStatementOutcomeUnknown, ended)
	}
	// checkResolves is a check that finds the caller still signed in.
	checkResolves checkStep = func(*http.Request) error { return nil }
	// checkRefuses is a check that finds the caller's sign-in ended: a logout.
	checkRefuses checkStep = func(*http.Request) error { return identity.ErrNoIdentity }
)

// scriptedCheckIdentity answers its resolutions with steps, one each in order, and resolves alice
// once they are spent; asked counts the resolutions it was asked for.
type scriptedCheckIdentity struct {
	steps []checkStep
	asked atomic.Int32
}

func (i *scriptedCheckIdentity) Login(r *http.Request) (string, error) {
	asked := int(i.asked.Add(1))
	if asked <= len(i.steps) {
		if err := i.steps[asked-1](r); err != nil {
			return "", err
		}
	}
	return "alice", nil
}

// A socket's heartbeat outlives a check that fails for a transient reason - the pool busy, the
// connection lost under the check's read (store.UnservedRead), the check's own bound passing, or
// the request pool's share outside admission having no place for it, so that it is not made - and
// checks again at the next heartbeat, and the third such failure in a row closes the socket as one
// whose caller no longer resolves, with that failure as its cause. A skipped check counts toward
// those three as the others do, so a socket whose checks find no place three heartbeats running
// closes though no check was made, and a check that resolves the caller, alone, starts the count
// again. A refusal closes the socket at once, whatever came before it. The checks are the document
// socket's own (signInStillResolves), each answered as the script says, on a connection whose
// handler waits for its context to end; no check is made after the one that closes it.
func TestAHeartbeatOutlivesTwoTransientFailuresAndClosesOnTheThird(t *testing.T) {
	t.Parallel()
	const heartbeat = 100 * time.Millisecond
	busy, lost, hangs, resolves, refuses := checkFindsPoolBusy, checkLosesItsConnection, checkHangs, checkResolves, checkRefuses
	for _, run := range []struct {
		name  string
		steps []checkStep
		// heldFull are the heartbeats, counted from one, whose check finds every place of the
		// share outside admission held.
		heldFull []int
		// closesAt is how many resolutions were made when the socket closed, cause what the close
		// carries, and transient how many of the heartbeats up to it failed for a transient reason.
		closesAt  int
		cause     error
		transient uint64
	}{
		{"a busy pool at three heartbeats in a row", []checkStep{busy, busy, busy}, nil, 3, store.ErrPoolBusy, 3},
		{"a check running out its bound at three heartbeats in a row", []checkStep{hangs, hangs, hangs}, nil, 3, errHeartbeatCheckBound, 3},
		{"a connection lost under the read at three heartbeats in a row", []checkStep{lost, lost, lost}, nil, 3, store.ErrStatementOutcomeUnknown, 3},
		{"a resolution after a lost connection and a busy pool", []checkStep{lost, busy, resolves, lost, busy, lost}, nil, 6, store.ErrStatementOutcomeUnknown, 5},
		{"a resolution between transient failures", []checkStep{busy, hangs, resolves, busy, busy, busy}, nil, 6, store.ErrPoolBusy, 5},
		{"a skipped check between two busy pools", []checkStep{busy, busy}, []int{2}, 2, store.ErrPoolBusy, 3},
		{"every check skipped at three heartbeats in a row", nil, []int{1, 2, 3}, 0, errHeartbeatCheckSkipped, 3},
		{"a resolution after two skipped checks", []checkStep{resolves, busy, busy, busy}, []int{1, 2}, 4, store.ErrPoolBusy, 5},
		{"a refusal after a transient failure", []checkStep{busy, refuses}, nil, 2, identity.ErrNoIdentity, 1},
	} {
		t.Run(run.name, func(t *testing.T) {
			scripted := &scriptedCheckIdentity{steps: run.steps}
			counts := &HeartbeatCounts{}
			served := directServer(Deps{Identity: scripted, StreamHeartbeat: heartbeat, Heartbeats: counts})
			// A wait for a place well inside the check's bound of a heartbeat, as production's
			// second is inside its fifteen, so a check no place comes free for is skipped.
			served.outside = admission.New(admission.Limits{OutsideWait: heartbeat / 5}).Outside()
			var beats atomic.Int32
			check := func(r *http.Request) error {
				if slices.Contains(run.heldFull, int(beats.Add(1))) {
					for range admission.OutsideLimit {
						if err := served.outside.Take(context.Background(), admission.DPoPDecision); err != nil {
							t.Errorf("hold a place outside admission: %v", err)
						}
					}
					defer func() {
						for range admission.OutsideLimit {
							served.outside.Release()
						}
					}()
				}
				return served.signInStillResolves(r)
			}
			ended := make(chan error, 1)
			connection := served.whileCallerResolves(check, func(_ http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
				ended <- context.Cause(r.Context())
			})
			go connection(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ws/doc/room", nil))

			cause := testwait.Receive(t, fmt.Sprintf("the socket to close, which it should at resolution %d (the resolutions so far are counted below)", run.closesAt), ended)
			if asked := int(scripted.asked.Load()); asked != run.closesAt {
				t.Fatalf("the socket closed at resolution %d (%v), want at resolution %d", asked, cause, run.closesAt)
			}
			if !errors.Is(cause, errCallerNoLongerResolves) || !errors.Is(cause, run.cause) {
				t.Fatalf("the socket closed with %v, want %v wrapping %v", cause, errCallerNoLongerResolves, run.cause)
			}
			stats := served.outside.Stats()
			if skipped := stats.Refused[admission.HeartbeatCheck]; skipped != (admission.OutsideRefusals{WaitExceeded: uint64(len(run.heldFull))}) || stats.InUse != 0 {
				t.Errorf("%+v checks skipped and %d places held, want %d whose wait ran out and none held", skipped, stats.InUse, len(run.heldFull))
			}
			// The counts are this server's own (Deps.Heartbeats), which no other test's sockets add
			// to, so they are exact.
			if got := counts.TransientFailures(); got != run.transient {
				t.Errorf("%d transient failures counted, want %d", got, run.transient)
			}
			var closedAtCeiling uint64
			if run.cause != identity.ErrNoIdentity {
				closedAtCeiling = 1
			}
			if got := counts.ClosesAtCeiling(); got != closedAtCeiling {
				t.Errorf("%d sockets counted closed at the ceiling, want %d", got, closedAtCeiling)
			}
			time.Sleep(3 * heartbeat)
			if asked := int(scripted.asked.Load()); asked != run.closesAt {
				t.Errorf("the closed socket's heartbeat checked again, %d resolutions in all, want none after the close at %d", asked, run.closesAt)
			}
		})
	}
}

// A live socket opened by a broker-issued token is checked at every heartbeat by the broker's
// answer alone, which reads nothing of the request pool and so takes no place of the pool's share
// outside admission (callerStillResolves), and waits for none. With every connection of the
// request pool held and every place of that share held, a token the broker stops holding live
// still closes the socket with 1008 at the first heartbeat that asks the broker again, within a
// heartbeat and a half of the revocation, where a check waiting its second for a place and failing
// for it would close the socket at the third such heartbeat. The verifier's clock is moved past the
// live answer it remembers, so that heartbeat is the next one.
func TestARevokedBrokerTokenClosesTheLiveSocketWhateverTheRequestPoolHolds(t *testing.T) {
	t.Parallel()
	const heartbeat = time.Second
	limiter := admission.New(admission.DefaultLimits())
	var ahead atomic.Int64
	rig := newBrokerTokenRigOn(t, testServerOptions{streamHeartbeat: heartbeat, requestPoolSize: 2, admission: limiter},
		func(options *ciphertoken.Options) {
			options.Now = func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) }
		})
	caller := newBrokerCaller(t)
	const token = "a-token-holding-the-live-socket-open"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	server := httptest.NewServer(rig.handler)
	t.Cleanup(server.Close)
	presented := caller.presenting(t, http.MethodGet, "/ws/live", token, nil)
	client := openLiveSocketAt(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/live", presentedHeaders(presented)).read()
	client.quiet(t, 2*heartbeat+heartbeat/2, "a live socket whose token the broker holds live")

	holdEveryConnection(t, rig.database.Pool)
	release := holdTheShareOutsideAdmission(t, limiter.Outside())
	defer release()

	rig.broker.Says(token, ciphertoken.Introspection{Active: false})
	ahead.Store(int64(ciphertoken.AffirmativeCacheTTL + time.Second))
	revoked := time.Now()
	if closed := client.closeError(t); closed.Code != gws.ClosePolicyViolation {
		t.Fatalf("the live socket closed with %d %q once the broker stopped holding its token live, want 1008", closed.Code, closed.Text)
	}
	t.Logf("the live socket closed %s after its token was revoked (heartbeat %s)", time.Since(revoked).Round(time.Millisecond), heartbeat)
	if stats := limiter.Outside().Stats(); stats.MaxQueued != 0 || stats.Refused[admission.HeartbeatCheck] != (admission.OutsideRefusals{}) {
		t.Errorf("the broker token's checks waited for a place (the longest queue %d) and were refused %+v, want none of either", stats.MaxQueued, stats.Refused[admission.HeartbeatCheck])
	}
}

// A burst of DPoP presentations, more at once than the request pool's share outside admission and
// its queue hold, takes no more of the request pool before admission than that share, 8 of its 48:
// each decision takes a place before it writes its proof's row (decideBrokerToken), the next 176
// wait for one - the queue at its cap, admission.ReadBurstFactor times the share, which bounds its
// memory - and every presentation past those is answered at once with admission's own refusal for
// a full queue - its status, every header and every byte of its body are those admission answers a
// read it has no room for - and its proof is not spent, so the same request sent again is served.
// Every connection of the request pool is held while the burst arrives, so a decision that reaches
// the pool waits there holding its place, and the pool's own count of the acquires waiting on it
// is the burst's hold on the pool before admission. Once the pool is free, inside the wait the
// waiting presentations wait, every presentation the queue held is served. The share and its queue
// are read from the numbers they are made of rather than from the limit, so a limit taken out is a
// burst this test still holds to them.
func TestABurstOfDPoPPresentationsTakesNoMoreOfTheRequestPoolThanAdmissionLeaves(t *testing.T) {
	t.Parallel()
	share := store.SharedPoolSize - admission.MaxAdmittedUnits
	queue := admission.ReadBurstFactor * share
	burst := share + queue + share
	// Room for every decision the share lets through once the pool is free - the read class's
	// share and its queue's cap of ReadBurstFactor times it hold all of them - a wait for a place
	// long enough that the 176 the queue holds are each served through the pool's two connections,
	// and a read queue that waits long enough for the refusal it is compared with to be had.
	limiter := admission.New(admission.Limits{Read: share, Write: share, ReadWait: time.Minute, WriteWait: time.Minute, OutsideWait: time.Minute})
	rig := newBrokerTokenRigOn(t, testServerOptions{requestPoolSize: 2, acquireBound: 30 * time.Second, admission: limiter})
	caller := newBrokerCaller(t)
	const token = "a-token-presented-in-a-burst"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	requests := make([]*http.Request, burst)
	for i := range requests {
		requests[i] = caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil)
	}

	release := holdEveryConnection(t, rig.database.Pool)
	answers := make([]*httptest.ResponseRecorder, burst)
	done := make(chan struct{}, burst)
	for i, request := range requests {
		go func() {
			answers[i] = rig.serve(request)
			done <- struct{}{}
		}()
	}
	answered, peak := 0, 0
	testwait.UntilPolling(t, time.Millisecond, fmt.Sprintf("each of the %d presentations answered, waiting on the request pool or waiting for a place (how many of each)", burst), func() (string, bool) {
		for more := true; more; {
			select {
			case <-done:
				answered++
			default:
				more = false
			}
		}
		waiting := int(requestPoolStats(t, rig.database.Pool).Acquire.Waiting)
		queued := limiter.Outside().Stats().Queued
		peak = max(peak, waiting)
		return fmt.Sprintf("%d answered, %d waiting on the pool and %d for a place", answered, waiting, queued), answered+waiting+queued == burst
	})
	if peak > share {
		t.Fatalf("the burst of %d presentations had %d acquires of the request pool waiting at once before admission, want at most the %d admission leaves", burst, peak, share)
	}
	if refusedAtOnce := answered; refusedAtOnce != burst-share-queue {
		t.Fatalf("%d of the %d presentations answered while the pool was held, want the %d past the share and its queue of %d", refusedAtOnce, burst, burst-share-queue, queue)
	}
	release()
	for ; answered < burst; answered++ {
		testwait.Receive(t, "a presentation to be answered once the request pool was free", done)
	}

	reference := admissionsQueueFullRefusal(t, rig, limiter, share)
	var refused []int
	for i, answer := range answers {
		switch answer.Code {
		case http.StatusOK:
		case http.StatusTooManyRequests:
			refused = append(refused, i)
			if !reflect.DeepEqual(answer.Header(), reference.Header()) || !bytes.Equal(answer.Body.Bytes(), reference.Body.Bytes()) {
				t.Errorf("presentation %d was refused %v %q, want admission's own refusal %v %q",
					i, answer.Header(), answer.Body.Bytes(), reference.Header(), reference.Body.Bytes())
			}
		default:
			t.Errorf("presentation %d was answered %d %s, want 200 or admission's 429", i, answer.Code, answer.Body.String())
		}
	}
	if len(refused) != burst-share-queue {
		t.Fatalf("%d of the %d presentations refused, want the %d past the share and its queue: every one the queue held is served once the pool frees within its wait", len(refused), burst, burst-share-queue)
	}
	if got := limiter.Outside().Stats(); got.Refused[admission.DPoPDecision] != (admission.OutsideRefusals{QueueFull: uint64(burst - share - queue)}) || got.MaxQueued != queue {
		t.Errorf("DPoP decisions refused %+v with the longest queue %d, want %d for a full queue and a queue of %d", got.Refused[admission.DPoPDecision], got.MaxQueued, burst-share-queue, queue)
	}

	// A refused presentation decided nothing, so its proof is unspent: sent again, it is served.
	first := requests[refused[0]]
	again := brokerRequest(t, http.MethodGet, "/api/v1/whoami", nil, first.Header.Get("Authorization"), first.Header.Get("DPoP"))
	if response := rig.serve(again); response.Code != http.StatusOK {
		t.Fatalf("a refused presentation sent again was answered %d %s, want 200: its proof was spent", response.Code, response.Body.String())
	}
}

// A broker slow to answer holds none of the request pool's share outside admission: a DPoP
// presentation asks the broker whether its token is live before it takes a place
// (decidedBrokerToken), so while twice the share's presentations, each its own token, wait on
// introspections the broker has not answered, every place is free, a document socket's heartbeat
// check takes one and resolves its caller, and a further presentation, of a token whose answer the
// verifier remembers, takes one and is served. The request pool is free throughout: a place held
// here would be held for the broker's round trip alone. Once the broker answers, the presentations
// that waited on it reach the share at once, twice as many as it holds, so each is served or
// refused admission's 429, and the share counts every refusal. The verifier's clock stands still,
// so the remembered answer outlasts however long a loaded runner takes.
func TestABrokerSlowToAnswerHoldsNoPlaceOfTheShareOutsideAdmission(t *testing.T) {
	t.Parallel()
	burst := 2 * admission.OutsideLimit
	limiter := admission.New(admission.DefaultLimits())
	broker := &heldIntrospections{release: make(chan struct{})}
	answer := sync.OnceFunc(func() { close(broker.release) })
	now := time.Now()
	rig := newBrokerTokenRigOn(t, testServerOptions{admission: limiter}, func(options *ciphertoken.Options) {
		options.HTTP = &http.Client{Transport: broker}
		options.Now = func() time.Time { return now }
	})
	// Before the rig's own cleanups, so no presentation is still waiting on the broker when the
	// server and its database close.
	var serving sync.WaitGroup
	t.Cleanup(func() {
		answer()
		serving.Wait()
	})
	caller := newBrokerCaller(t)
	const remembered = "a-token-whose-answer-the-verifier-remembers"
	rig.broker.Says(remembered, liveNamedToken(caller.jkt))
	if response := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", remembered, nil)); response.Code != http.StatusOK {
		t.Fatalf("the presentation that warms the verifier's answer: status=%d body=%s, want 200", response.Code, response.Body.String())
	}

	broker.holding.Store(true)
	answers := make([]*httptest.ResponseRecorder, burst)
	done := make(chan struct{}, burst)
	for i := range burst {
		token := fmt.Sprintf("a-token-the-broker-is-slow-to-answer-%d", i)
		rig.broker.Says(token, liveNamedToken(caller.jkt))
		request := caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil)
		serving.Go(func() {
			answers[i] = rig.serve(request)
			done <- struct{}{}
		})
	}
	waitUntil(t, "every presentation waiting on the broker or answered", func() bool {
		return int(broker.entered.Load())+len(done) == burst
	})
	if stats := limiter.Outside().Stats(); stats.InUse != 0 || stats.MaxQueued != 0 || stats.Refused != [len(stats.Refused)]admission.OutsideRefusals{} {
		t.Fatalf("with %d presentations waiting on the broker, %d places of the share outside admission are held and %v refused, want none held and none refused",
			broker.entered.Load(), stats.InUse, stats.Refused)
	}
	check := httptest.NewRequest(http.MethodGet, "/ws/doc/room", nil)
	check.Header.Set("X-Dispatch-User", "alice")
	if err := rig.served.signInStillResolves(check); err != nil {
		t.Fatalf("a heartbeat check while presentations wait on the broker: %v, want its caller resolved", err)
	}
	if response := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", remembered, nil)); response.Code != http.StatusOK {
		t.Fatalf("a presentation of a remembered token while others wait on the broker: status=%d body=%s, want 200", response.Code, response.Body.String())
	}

	answer()
	for range burst {
		testwait.Receive(t, "a presentation that waited on the broker to be answered once it answered", done)
	}
	refused := 0
	for i, response := range answers {
		switch {
		case response.Code == http.StatusOK:
		case response.Code == http.StatusTooManyRequests && refusalCode(response.Body.Bytes()) == admission.CodeOverloaded:
			refused++
		default:
			t.Errorf("presentation %d, which waited on the broker: status=%d body=%s, want 200 or admission's 429", i, response.Code, response.Body.String())
		}
	}
	if counted := limiter.Outside().Stats().Refused[admission.DPoPDecision]; counted.QueueFull+counted.WaitExceeded != uint64(refused) {
		t.Errorf("%d presentations refused 429 once the broker answered, and the share counted %+v DPoP decisions refused", refused, counted)
	}
}

// A DPoP presentation's decision asks the broker about its token once, and never under the place
// it takes of the request pool's share outside admission: its proof is checked against the answer
// the broker gave before the decision waited for that place (decidedBrokerToken,
// ciphertoken.Verifier.Prove). Every place is held as the presentation arrives, the verifier's
// clock moves past the answer's cache entry (ciphertoken.AffirmativeCacheTTL) while the decision
// waits, and a place is given back: the presentation is served, and the broker was asked once,
// where the proof's check asked it again, under the place, for the answer the cache no longer held.
func TestADPoPDecisionAsksTheBrokerOnceHoweverLongItWaitsForAPlace(t *testing.T) {
	t.Parallel()
	limiter := admission.New(admission.DefaultLimits())
	var ahead atomic.Int64
	rig := newBrokerTokenRigOn(t, testServerOptions{admission: limiter}, func(options *ciphertoken.Options) {
		options.Now = func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) }
	})
	caller := newBrokerCaller(t)
	const token = "a-token-whose-answer-expires-while-its-decision-waits"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	release := holdTheShareOutsideAdmission(t, limiter.Outside())
	t.Cleanup(release)

	request := caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil)
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- rig.serve(request) }()
	waitUntil(t, "the presentation's decision waiting for a place", func() bool {
		return limiter.Outside().Stats().Queued == 1
	})
	ahead.Store(int64(ciphertoken.AffirmativeCacheTTL + time.Second))
	release()

	response := testwait.Receive(t, "the presentation to be answered once a place came free", answered)
	if response.Code != http.StatusOK {
		t.Fatalf("the presentation a place came free for: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	if asked := rig.broker.Count(); asked != 1 {
		t.Errorf("the broker was asked about the token %d times, want once: the proof is checked against the answer the decision had before it waited", asked)
	}
}

// heldIntrospections is the transport a verifier sends its introspections through. While holding
// is set, each waits until release closes, or its request's context ends, before it reaches the
// broker, and entered counts those that have begun waiting.
type heldIntrospections struct {
	holding atomic.Bool
	entered atomic.Int32
	release chan struct{}
}

func (h *heldIntrospections) RoundTrip(r *http.Request) (*http.Response, error) {
	if h.holding.Load() {
		h.entered.Add(1)
		select {
		case <-h.release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	return http.DefaultTransport.RoundTrip(r)
}

// admissionsQueueFullRefusal is admission's own answer to a read on rig's server it has no room
// for and no place in its queue for: every read unit of limiter, whose read limit is limit, held,
// and as many reads queued as the queue holds, its burst cap, admission.ReadBurstFactor times the
// limit, the queue's one bound: a request below it would queue for its minute here and never be
// refused queue_full.
func admissionsQueueFullRefusal(t *testing.T, rig *brokerTokenRig, limiter *admission.Limiter, limit int) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var queued sync.WaitGroup
	defer func() {
		cancel()
		queued.Wait()
	}()
	for range limit {
		ticket, err := limiter.Admit(ctx, admission.Read, 1)
		if err != nil {
			t.Fatalf("hold a read unit: %v", err)
		}
		defer ticket.Release()
	}
	burstCap := admission.ReadBurstFactor * limit
	for range burstCap {
		queued.Go(func() {
			if ticket, err := limiter.Admit(ctx, admission.Read, 1); err == nil {
				ticket.Release()
			}
		})
	}
	waitUntil(t, "a full read queue", func() bool { return limiter.Stats()[admission.Read].Queued == burstCap })
	request := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	request.Header.Set("X-Dispatch-User", "alice")
	refusal := rig.serve(request)
	if refusal.Code != http.StatusTooManyRequests || limiter.Stats()[admission.Read].QueueFull != 1 {
		t.Fatalf("a read with the read queue full was answered %d %s, want admission's queue_full refusal", refusal.Code, refusal.Body.String())
	}
	return refusal
}

// holdTheShareOutsideAdmission takes every place of outside, the request pool's share admission
// leaves, waiting its turn behind a check in flight that holds one, and answers the release that
// gives them back.
func holdTheShareOutsideAdmission(t *testing.T, outside *admission.Outside) func() {
	t.Helper()
	for place := range admission.OutsideLimit {
		if err := outside.Take(context.Background(), admission.DPoPDecision); err != nil {
			t.Fatalf("place %d of the share outside admission: %v", place+1, err)
		}
	}
	return sync.OnceFunc(func() {
		for range admission.OutsideLimit {
			outside.Release()
		}
	})
}

// outsideWorkArrival is the header a test's heartbeat check names its arrival in
// (outsideWorkRig), so the identity it resolves through knows which work it is.
const outsideWorkArrival = "X-Outside-Work-Arrival"

// outsideWorkRig is a server whose request pool is two connections a test can hold, and the
// request-pool work outside admission it starts against that server, in arrivals numbered from 0:
// an even arrival is a document socket's heartbeat check (signInStillResolves, its read of the
// caller resolving through the request pool), an odd one a document socket's admission reads as
// docs.Service.ServeHTTP makes them, a place held (admission.Outside.Hold) over a read of the
// request pool. Each work reports its arrival on took once it holds its place, and where gates
// is set waits there at its own gate before it reads.
type outsideWorkRig struct {
	served   *server
	database *store.Store
	limiter  *admission.Limiter
	took     chan int
	gates    []chan struct{}
}

// outsideWorkAnswer is how one work of an outsideWorkRig ended, and when.
type outsideWorkAnswer struct {
	err error
	at  time.Time
}

// newOutsideWorkRig builds the rig on a server of its own, with gates for gated arrivals and
// outsideWait the work's wait for a place (zero is the second every server waits). Its request
// pool's acquire bound is past any run, so the work holding a place waits on a held pool for as
// long as the row holds it. Its took holds a report from every work the places and their queue
// can hold at once, so work none of the test reads reports to it without waiting.
func newOutsideWorkRig(t *testing.T, gated int, outsideWait time.Duration) *outsideWorkRig {
	t.Helper()
	limits := admission.DefaultLimits()
	limits.OutsideWait = outsideWait
	rig := &outsideWorkRig{limiter: admission.New(limits), took: make(chan int, admission.OutsideLimit+admission.OutsideQueueLimit)}
	for range gated {
		rig.gates = append(rig.gates, make(chan struct{}))
	}
	_, rig.database, _, rig.served = newRegisteredTestServer(t, testServerOptions{
		requestPoolSize: 2, acquireBound: pastTheRun, admission: rig.limiter, identity: outsideWorkIdentity{rig},
	})
	t.Cleanup(func() {
		for _, gate := range rig.gates {
			select {
			case <-gate:
			default:
				close(gate)
			}
		}
	})
	return rig
}

// tookPlace reports arrival on took and waits at its gate, where it has one.
func (rig *outsideWorkRig) tookPlace(arrival int) {
	rig.took <- arrival
	if arrival < len(rig.gates) {
		<-rig.gates[arrival]
	}
}

// start starts arrival's work and answers how it ended.
func (rig *outsideWorkRig) start(arrival int) <-chan outsideWorkAnswer {
	answered := make(chan outsideWorkAnswer, 1)
	if arrival%2 == 0 {
		check := httptest.NewRequest(http.MethodGet, "/ws/doc/room", nil)
		check.Header.Set("X-Dispatch-User", "alice")
		check.Header.Set(outsideWorkArrival, strconv.Itoa(arrival))
		go func() {
			err := rig.served.signInStillResolves(check)
			answered <- outsideWorkAnswer{err: err, at: time.Now()}
		}()
		return answered
	}
	go func() {
		ctx := context.Background()
		place, err := rig.limiter.Outside().Hold(ctx, admission.SocketAdmission)
		if err == nil {
			rig.tookPlace(arrival)
			var one int
			err = rig.database.Pool.QueryRow(ctx, "select 1").Scan(&one)
			place.Release()
		}
		answered <- outsideWorkAnswer{err: err, at: time.Now()}
	}()
	return answered
}

// arrive starts arrivals from up to to, one after another, each once the one before it holds its
// place, waits in the queue for one or has been refused one, so they arrive in that order, and
// answers each's answer and the moment just before it was started: its wait for a place, counted
// from when it asks (admission.Outside), cannot have begun before that moment.
func (rig *outsideWorkRig) arrive(t *testing.T, from, to int) ([]<-chan outsideWorkAnswer, []time.Time) {
	t.Helper()
	answers := make([]<-chan outsideWorkAnswer, 0, to-from)
	started := make([]time.Time, 0, to-from)
	for arrival := from; arrival < to; arrival++ {
		started = append(started, time.Now())
		answers = append(answers, rig.start(arrival))
		waitUntil(t, fmt.Sprintf("arrival %d holding, waiting for or refused a place", arrival), func() bool {
			stats := rig.limiter.Outside().Stats()
			accounted := stats.InUse + stats.Queued
			for _, refused := range stats.Refused {
				accounted += int(refused.QueueFull + refused.WaitExceeded)
			}
			return accounted == arrival+1
		})
	}
	return answers, started
}

// outsideWorkIdentity is the identity an outsideWorkRig's heartbeat checks resolve through: the
// package's header identity on the rig's database, once the check, which holds its place by then,
// has reported its arrival and passed its gate.
type outsideWorkIdentity struct{ rig *outsideWorkRig }

func (i outsideWorkIdentity) Login(r *http.Request) (string, error) {
	if arrival, err := strconv.Atoi(r.Header.Get(outsideWorkArrival)); err == nil {
		i.rig.tookPlace(arrival)
	}
	return headerIdentity(i.rig.database).Login(r)
}

// Work outside admission that finds every place held waits for one, and is given it in the order
// it asked, for at most its wait, in a queue of at most admission.OutsideQueueLimit, 176, the cap
// that bounds its memory: heartbeat checks and document sockets' admission reads, alternately,
// arrive one after another while eight of them hold every place.
//
//   - In the order it asked: the eight holding are held at gates of the test's, and twelve more
//     wait; each place given back, one at a time, goes to the work that has waited longest, however
//     long the queue has stood (admission's fifoGate). Their wait is past any run here: the order is
//     the proof.
//   - On a held pool freed within the wait: every connection of the request pool is held, so the
//     eight holding wait on it, and 176 more wait for a place, the most the queue holds; four more
//     are refused at once for the full queue, the checks skipped and the sockets refused. The
//     pool's acquires waiting stay at the eight, and work past the full queue is answered while
//     the pool is still held. Once the pool is freed, inside the wait, all 184 are served. Their
//     wait is past any run here rather than a server's second, since 184 arrivals one after
//     another take longer than a second to queue.
//   - On a held pool past the wait: the eight holding wait on the pool and twelve more for a
//     place, and the pool is held past the second: each of the twelve is refused for its wait, the
//     checks skipped and the sockets refused, and the eight are served once the pool frees. The
//     second is the server's own and stays: each refusal comes at least a second after the
//     moment before its work was started, and the pool, whose acquire bound is past any run, is
//     held until every one of them has come, so nothing but its wait could have ended it.
func TestWorkOutsideAdmissionWaitsForAPlaceInOrderForAtMostASecond(t *testing.T) {
	t.Parallel()
	const queued = 12
	t.Run("in the order it asked", func(t *testing.T) {
		rig := newOutsideWorkRig(t, admission.OutsideLimit+queued, pastTheRun)
		answers, _ := rig.arrive(t, 0, admission.OutsideLimit+queued)
		for range admission.OutsideLimit {
			if arrival := testwait.Receive(t, "the work holding the places to report its place", rig.took); arrival >= admission.OutsideLimit {
				t.Fatalf("arrival %d took a place while the first %d held them all", arrival, admission.OutsideLimit)
			}
		}
		for next := range queued {
			close(rig.gates[next])
			if arrival, want := testwait.Receive(t, fmt.Sprintf("the place arrival %d gave back to go to the one that has waited longest", next), rig.took), admission.OutsideLimit+next; arrival != want {
				t.Fatalf("the place arrival %d gave back went to arrival %d, want arrival %d: the one that has waited longest", next, arrival, want)
			}
		}
		for next := queued; next < len(rig.gates); next++ {
			close(rig.gates[next])
		}
		for arrival, answer := range answers {
			if got := testwait.Receive(t, fmt.Sprintf("arrival %d to be answered once every gate was let go", arrival), answer); got.err != nil {
				t.Errorf("arrival %d: %v, want it served", arrival, got.err)
			}
		}
		if stats := rig.limiter.Outside().Stats(); stats.MaxQueued != queued || stats.Refused != [len(stats.Refused)]admission.OutsideRefusals{} {
			t.Errorf("stats %+v, want a queue of %d at its longest and nothing refused", stats, queued)
		}
	})

	t.Run("on a held pool freed within the wait", func(t *testing.T) {
		rig := newOutsideWorkRig(t, 0, pastTheRun)
		release := holdEveryConnection(t, rig.database.Pool)
		served := admission.OutsideLimit + admission.OutsideQueueLimit
		answers, _ := rig.arrive(t, 0, served)
		waitUntil(t, "the work holding the places waiting on the request pool", func() bool {
			return requestPoolStats(t, rig.database.Pool).Acquire.Waiting == admission.OutsideLimit
		})
		for arrival := served; arrival < served+4; arrival++ {
			// The pool is held and the wait for a place is past any run, so work past the full
			// queue that waited rather than being refused would not be answered before the test
			// binary's deadline.
			got := testwait.Receive(t, fmt.Sprintf("arrival %d past a full queue of %d to be refused", arrival, admission.OutsideQueueLimit), rig.start(arrival))
			var refusal *admission.OutsideRefusal
			if !errors.As(got.err, &refusal) || refusal.Reason != admission.QueueFull || (arrival%2 == 0) != errors.Is(got.err, errHeartbeatCheckSkipped) {
				t.Fatalf("arrival %d past a full queue: %v, want refused for the full queue (a check skipped)", arrival, got.err)
			}
		}
		if waiting := requestPoolStats(t, rig.database.Pool).Acquire.Waiting; waiting != admission.OutsideLimit {
			t.Fatalf("%d acquires waiting on the request pool, want the %d holding a place", waiting, admission.OutsideLimit)
		}
		release()
		for arrival, answer := range answers {
			if got := testwait.Receive(t, fmt.Sprintf("arrival %d to be answered once the pool freed", arrival), answer); got.err != nil {
				t.Errorf("arrival %d: %v, want it served once the pool freed within its wait", arrival, got.err)
			}
		}
		stats := rig.limiter.Outside().Stats()
		want := [len(stats.Refused)]admission.OutsideRefusals{}
		want[admission.HeartbeatCheck], want[admission.SocketAdmission] = admission.OutsideRefusals{QueueFull: 2}, admission.OutsideRefusals{QueueFull: 2}
		if stats.MaxQueued != admission.OutsideQueueLimit || stats.Refused != want || stats.InUse != 0 {
			t.Errorf("stats %+v, want a queue of %d at its longest, the four past it refused for a full queue and no place held", stats, admission.OutsideQueueLimit)
		}
	})

	t.Run("on a held pool past the wait", func(t *testing.T) {
		rig := newOutsideWorkRig(t, 0, 0)
		release := holdEveryConnection(t, rig.database.Pool)
		answers, started := rig.arrive(t, 0, admission.OutsideLimit+queued)
		for i := admission.OutsideLimit; i < len(answers); i++ {
			got := testwait.Receive(t, fmt.Sprintf("arrival %d, no place free for it, to be refused at its %v wait", i, admission.DefaultReadWait), answers[i])
			var refusal *admission.OutsideRefusal
			if !errors.As(got.err, &refusal) || refusal.Reason != admission.WaitExceeded || (i%2 == 0) != errors.Is(got.err, errHeartbeatCheckSkipped) {
				t.Fatalf("arrival %d, no place free for it: %v, want refused for its wait (a check skipped)", i, got.err)
			}
			if waited := got.at.Sub(started[i]); waited < admission.DefaultReadWait {
				t.Errorf("arrival %d refused %v after it was started, want at least its %v wait", i, waited, admission.DefaultReadWait)
			}
		}
		release()
		for i := range admission.OutsideLimit {
			if got := testwait.Receive(t, fmt.Sprintf("arrival %d, which held its place, to be answered once the pool freed", i), answers[i]); got.err != nil {
				t.Errorf("arrival %d, which held its place: %v, want it served once the pool freed", i, got.err)
			}
		}
		stats := rig.limiter.Outside().Stats()
		want := [len(stats.Refused)]admission.OutsideRefusals{}
		want[admission.HeartbeatCheck], want[admission.SocketAdmission] = admission.OutsideRefusals{WaitExceeded: queued / 2}, admission.OutsideRefusals{WaitExceeded: queued / 2}
		if stats.Refused != want || stats.InUse != 0 || stats.Queued != 0 {
			t.Errorf("stats %+v, want the %d waiting refused for their wait and no place held", stats, queued)
		}
	})
}
