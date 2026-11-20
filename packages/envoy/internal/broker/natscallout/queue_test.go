package natscallout

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// withDecisionSlots runs the rest of the test with concurrentDecisions set to n, so a test can make
// requests queue for a slot without sending sixty-four of them.
func withDecisionSlots(t *testing.T, n int) {
	t.Helper()
	was := concurrentDecisions
	concurrentDecisions = n
	t.Cleanup(func() { concurrentDecisions = was })
}

// withWaitingBound runs the rest of the test with maxWaiting set to n, so a test can fill the
// bound with a handful of requests.
func withWaitingBound(t *testing.T, n int) {
	t.Helper()
	was := maxWaiting
	maxWaiting = n
	t.Cleanup(func() { maxWaiting = was })
}

// withRefusingBound runs the rest of the test with maxRefusing set to n, so a test can make a
// replica drop what it is sent.
func withRefusingBound(t *testing.T, n int) {
	t.Helper()
	was := maxRefusing
	maxRefusing = n
	t.Cleanup(func() { maxRefusing = was })
}

// slowIntrospect answers each session's token after wait, giving up when ctx ends, as a broker
// whose database is slow does.
func slowIntrospect(wait time.Duration, sessions ...*session) Deps {
	answer := introspector(sessions...)
	return Deps{Introspect: func(ctx context.Context, token string) (oauthwire.Introspection, error) {
		select {
		case <-ctx.Done():
			return oauthwire.Introspection{}, ctx.Err()
		case <-time.After(wait):
		}
		return answer(ctx, token)
	}}
}

// connectAll connects every session at once and answers each one's error and how long its connect
// took, in the order of sessions.
func connectAll(t *testing.T, url string, sessions []*session) ([]error, []time.Duration) {
	t.Helper()
	errs := make([]error, len(sessions))
	took := make([]time.Duration, len(sessions))
	var wait sync.WaitGroup
	for index, s := range sessions {
		wait.Add(1)
		go func() {
			defer wait.Done()
			began := time.Now()
			nc, err := connectSession(t, url, s, nil)
			took[index], errs[index] = time.Since(began), err
			if err == nil {
				nc.Close()
			}
		}()
	}
	wait.Wait()
	return errs, took
}

func sessionsNamed(t *testing.T, prefix string, n int) []*session {
	t.Helper()
	sessions := make([]*session, n)
	for index := range sessions {
		sessions[index] = newSession(t, fmt.Sprintf("%s_%d", prefix, index), 15*time.Minute)
	}
	return sessions
}

// The budget counts from when the request arrives, not from when a decision slot frees up: a
// request that waited for a slot is decided under what is left of its budget, so every client
// hears inside the budget, admitted or refused, and none waits out the server's own timeout.
// Counted from its slot instead, the budget of a request behind slow decisions outlasts the
// server: with one slot and a 700 ms broker, the third request of three waits 1.4 s for its slot
// and would be answered at 2.1 s, after the server had given up on it.
//
// The deadline is read from the decision's own context. One slot is held by a first decision until
// a second request is seen waiting behind it, and that request is then decided under the deadline
// it was given as it arrived: one budget after a moment between its client starting to connect and
// the test seeing it wait. A budget counted from its slot ends one budget after the slot freed,
// which is after the test saw it wait, since the slot frees only then.
func TestTheBudgetCountsTheTimeARequestWaitsForASlot(t *testing.T) {
	withDecisionSlots(t, 1)
	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys)
	holder, queued := newSession(t, "ses_holder", 15*time.Minute), newSession(t, "ses_queued", 15*time.Minute)
	holding, release := make(chan struct{}), make(chan struct{})
	decidedUnder := make(chan time.Time, 1)
	service := startService(t, url, keys, Deps{Introspect: introspector(holder, queued)}, func(ctx context.Context, deps Deps, services []ServiceEntry, req Request) (Decision, error) {
		switch req.Token {
		case holder.token:
			// The slot stays held, whatever the holder's own budget, until the test releases it.
			close(holding)
			<-release
		case queued.token:
			deadline, _ := ctx.Deadline()
			decidedUnder <- deadline
		}
		return Decide(ctx, deps, services, req)
	})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	// Registered after the service, so it runs before the service's own Close, which waits for the
	// holder's decision.
	t.Cleanup(free)

	held := make(chan error, 1)
	go func() {
		nc, err := connectSession(t, url, holder, nil)
		if err == nil {
			nc.Close()
		}
		held <- err
	}()
	<-holding
	connecting := time.Now()
	heard := make(chan error, 1)
	go func() {
		nc, err := connectSession(t, url, queued, nil)
		if err == nil {
			nc.Close()
		}
		heard <- err
	}()
	testwait.Eventually(t, "the second request waiting for the slot", func() bool {
		return service.waiting.Load() == 1
	})
	seen := time.Now()
	free()
	err := <-heard
	// The holder's answer, whatever it is, went out before the slot freed; its client is waited
	// for so that its connection is not left to a test that has ended.
	<-held
	var deadline time.Time
	select {
	case deadline = <-decidedUnder:
	default:
		t.Fatalf("the request that waited for the slot was never decided (its client heard %v)", err)
	}
	require.False(t, deadline.Before(connecting.Add(decideBudget)) || deadline.After(seen.Add(decideBudget)),
		"the request that waited for the slot was decided under a deadline %s after its client began to connect, where its %s budget runs from its arrival, which came before it was seen waiting %s after that: its budget was counted from its slot, not from its arrival",
		deadline.Sub(connecting), decideBudget, seen.Sub(connecting))
	require.NoError(t, err, "the request that waited for the slot was not admitted inside what was left of its budget")
}

// A request waiting for a decision slot is refused when its own budget is spent, whatever the
// slot's holder is doing: one slot is held by a decision that ignores its context, as a broker call
// that does not honour cancellation would, and keeps it until the test ends, and a request arriving
// behind it is refused for its budget instead of waiting for the slot. Without the wait's deadline
// it would wait for the slot as long as the holder keeps it, past the two seconds its client's
// connect allows, and the server would give up on it at its authorization timeout as it gives up on
// the holder's own request. So the service counts the waiting request's refusal under the budget's
// label, and the server gives up on one request, the holder's: it writes each give-up to its log
// (hubGaveUp) before it refuses that client, so once both clients have heard, its log names every
// request it gave up on.
func TestAWaitingRequestIsRefusedWhenItsBudgetIsSpent(t *testing.T) {
	withDecisionSlots(t, 1)
	keys := testnats.NewCalloutKeys(t)
	server, url := startDebugCalloutServer(t, keys)
	stuck, waiting := newSession(t, "ses_stuck", 15*time.Minute), newSession(t, "ses_waiting", 15*time.Minute)
	registry := metrics.New()
	deps := Deps{Introspect: introspector(stuck, waiting), Metrics: NewMetrics(registry)}
	holding, release := make(chan struct{}), make(chan struct{})
	startService(t, url, keys, deps, func(ctx context.Context, deps Deps, services []ServiceEntry, req Request) (Decision, error) {
		if req.Token == stuck.token {
			close(holding)
			<-release
		}
		return Decide(ctx, deps, services, req)
	})
	// Registered after the service, so it runs before the service's own Close, which waits for the
	// stuck decision.
	t.Cleanup(func() { close(release) })

	stuckHeard := make(chan error, 1)
	go func() {
		_, err := connectSession(t, url, stuck, nil)
		stuckHeard <- err
	}()
	<-holding
	_, err := connectSession(t, url, waiting, nil)
	require.ErrorIs(t, err, nats.ErrAuthorization, "the request behind a stuck slot was not refused")
	require.ErrorIs(t, <-stuckHeard, nats.ErrAuthorization, "the stuck request's client was not refused at the server's timeout")
	require.Equal(t, 1, strings.Count(server.Log(), hubGaveUp),
		"the server gave up on the request behind the stuck slot too: it waited for the slot past its own budget")
	testwait.Eventually(t, "the refusal of the request behind the stuck slot counted under the budget's label", func() bool {
		return refusedFor(t, registry, labelBudget) == 1
	})
}

// A request whose wait for a slot spent its budget is refused naming the wait, not the step its
// decision would have reached. The one slot is held by a decision that waits until the waiter's
// refusal is counted, so the request behind it spends its whole budget waiting for the slot.
// Started, its 200 ms introspection would be cancelled and named; it is refused naming the wait
// instead, and the broker is never asked about its token.
func TestABudgetSpentWaitingForASlotNamesTheWait(t *testing.T) {
	withDecisionSlots(t, 1)
	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys)
	holder, waiter := newSession(t, "ses_holder", 15*time.Minute), newSession(t, "ses_waiter", 15*time.Minute)
	var asked atomic.Int32
	deps := slowIntrospect(200*time.Millisecond, holder, waiter)
	introspect := deps.Introspect
	deps.Introspect = func(ctx context.Context, token string) (oauthwire.Introspection, error) {
		if token == waiter.token {
			asked.Add(1)
		}
		return introspect(ctx, token)
	}
	log := &logtest.Buffer{}
	deps.Log = slog.New(slog.NewTextHandler(log, nil))
	registry := metrics.New()
	deps.Metrics = NewMetrics(registry)
	// The holder keeps the one slot until the waiter's refusal is counted, so the waiter's is the
	// first refusal, whose detail the reporter's line carries (report).
	release := make(chan struct{})
	startService(t, url, keys, deps, func(ctx context.Context, deps Deps, services []ServiceEntry, req Request) (Decision, error) {
		if req.Token == holder.token {
			<-release
		}
		return Decide(ctx, deps, services, req)
	})

	go func() { _, _ = connectSession(t, url, holder, nil) }()
	time.Sleep(30 * time.Millisecond)
	_, err := connectSession(t, url, waiter, nil)
	require.ErrorIs(t, err, nats.ErrAuthorization)
	testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, "the waiter's refusal counted", func() bool {
		return refusedFor(t, registry, labelBudget) == 1
	})
	close(release)
	testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, "the refusal in the broker's log", func() bool {
		return strings.Contains(log.String(), "budget=")
	})
	require.Contains(t, log.String(), "budget at the wait for a decision slot", "no refusal named the wait:\n%s", log.String())
	require.Zero(t, asked.Load(), "the broker was asked about a token whose wait had spent its budget:\n%s", log.String())
}

// A replica holds at most maxWaiting requests waiting for a decision slot, and refuses the one past
// the bound at once, naming the bound, while the ones it holds wait on and are decided. One slot is
// held by a decision until that refusal is heard; two requests, the bound, wait behind it; a third
// is refused under the bound's label rather than waiting, and the holder and the two held are then
// admitted. With no bound the third waits behind the slot too, and is refused for its budget or
// decided, never under the bound's label: the bound's refusal is made as the request arrives
// (deliver), so the label tells a refusal at once from one that waited, where how long the client
// took would also count whatever slowed the client or the hub.
//
// The counter is read after Close, which returns once every request has been answered and counted,
// and so is the refusal's reason: Close's last report writes whatever is still counted (report), so
// it is in the log whether a tick's line or that last one carried it, and the test waits on no tick
// of the reporter.
func TestARequestPastTheWaitingBoundIsRefusedAtOnce(t *testing.T) {
	withDecisionSlots(t, 1)
	withWaitingBound(t, 2)
	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys)
	holder, extra := newSession(t, "ses_holder", 15*time.Minute), newSession(t, "ses_extra", 15*time.Minute)
	waiters := sessionsNamed(t, "ses_waiting", 2)
	log := &logtest.Buffer{}
	registry := metrics.New()
	deps := Deps{Introspect: introspector(append([]*session{holder, extra}, waiters...)...), Log: slog.New(slog.NewTextHandler(log, nil)), Metrics: NewMetrics(registry)}
	holding, release := make(chan struct{}), make(chan struct{})
	service := startService(t, url, keys, deps, func(ctx context.Context, deps Deps, services []ServiceEntry, req Request) (Decision, error) {
		if req.Token == holder.token {
			close(holding)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return Decide(ctx, deps, services, req)
	})

	held := make(chan error, 1)
	go func() {
		nc, err := connectSession(t, url, holder, nil)
		if err == nil {
			nc.Close()
		}
		held <- err
	}()
	<-holding
	results := make(chan []error, 1)
	go func() {
		errs, _ := connectAll(t, url, waiters)
		results <- errs
	}()
	testwait.WithinPolling(t, 5*time.Second, 5*time.Millisecond, "both waiters waiting for the slot", func() bool {
		return service.waiting.Load() == int64(len(waiters))
	})

	_, err := connectSession(t, url, extra, nil)
	close(release)
	require.ErrorIs(t, err, nats.ErrAuthorization, "the request past the waiting bound was admitted")
	service.Close()
	require.Equal(t, 1, refusedFor(t, registry, labelWaitingBound),
		"the request past the waiting bound was not refused under the bound's label: it waited behind the slot rather than being refused at once")
	require.NoError(t, <-held, "the slot's holder was refused")
	for index, err := range <-results {
		require.NoError(t, err, "waiter %d of the bound was refused", index)
	}
	require.Contains(t, log.String(), labelWaitingBound+"=1", "the refusal was not in the broker's log:\n%s", log.String())
	require.Contains(t, log.String(), "2 requests already wait for a decision slot", "the refusal did not name the bound:\n%s", log.String())
}

// A request past both bounds, the requests waiting for a decision slot and the requests being
// refused, is dropped unanswered, the hub refuses its client itself at its authorization timeout,
// and the replica counts each drop under its reason and logs the first in a minute: not from the
// delivery goroutine, which writes no log line (deliver), but at the reporter's next tick, so a
// flood of drops is a counter and a line a minute rather than a line a request. Both bounds are
// zero here, so the replica drops every request it is sent; of two, both are counted and the
// first alone is logged.
func TestARequestPastBothBoundsIsDroppedCountedAndLoggedOnceAMinute(t *testing.T) {
	withWaitingBound(t, 0)
	withRefusingBound(t, 0)
	withLiveness(t, 20*time.Millisecond, 10*time.Second)
	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys)
	dropped := newSession(t, "ses_dropped", 15*time.Minute)
	var asked atomic.Int32
	introspect := introspector(dropped)
	log := &logtest.Buffer{}
	registry := metrics.New()
	deps := Deps{
		Introspect: func(ctx context.Context, token string) (oauthwire.Introspection, error) {
			asked.Add(1)
			return introspect(ctx, token)
		},
		Log:     slog.New(slog.NewTextHandler(log, nil)),
		Metrics: NewMetrics(registry),
	}
	service := startService(t, url, keys, deps, Decide)

	for attempt := range 2 {
		began := time.Now()
		_, err := connectSession(t, url, dropped, nil)
		took := time.Since(began)
		require.ErrorIs(t, err, nats.ErrAuthorization, "the hub admitted a client whose request was dropped")
		require.Greater(t, took, time.Second, "the client heard after %s: the replica answered a request past both bounds", took)
		testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, "the drop counted under its reason", func() bool {
			return strings.Contains(scrape(t, registry), fmt.Sprintf(`broker_nats_callout_dropped_total{reason="past_bounds"} %d`, attempt+1))
		})
	}
	require.Equal(t, 1, strings.Count(log.String(), "dropped authorization requests"), "the drops were not logged once in the minute:\n%s", log.String())
	require.Contains(t, log.String(), "past_bounds=1")
	require.Zero(t, asked.Load(), "the broker was asked about a dropped request")
	require.Zero(t, service.waiting.Load(), "a dropped request is still counted waiting")
	require.Zero(t, service.refusing.Load(), "a dropped request is still counted being refused")
}

// holdDeliveries blocks every delivery of a request to the replica target names, once it names
// one, until the returned release runs, and reports on entered each delivery that reached the
// hold. Other replicas' deliveries pass.
func holdDeliveries(t *testing.T) (target *atomic.Pointer[Service], entered <-chan struct{}, release func()) {
	t.Helper()
	target = &atomic.Pointer[Service]{}
	reached := make(chan struct{}, 64)
	gate := make(chan struct{})
	holdDelivery = func(s *Service) {
		if target.Load() != s {
			return
		}
		reached <- struct{}{}
		<-gate
	}
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	// Registered before the service, so it runs after the service's own Close: no delivery is
	// left to read the hook when it is cleared.
	t.Cleanup(func() { release(); holdDelivery = nil })
	return target, reached, release
}

// A replica that stops answers every request the hub had already routed to it: it drains its
// subscription rather than dropping what it holds. Delivery is held, so of three requests one is
// at the hold and two wait in the subscription's own buffer when the replica is closed; the hold
// is released only after Close has begun. A replica that unsubscribed instead would drop the two
// buffered ones, whose clients would then wait out the hub's authorization timeout.
func TestAClosingReplicaAnswersTheRequestsItHolds(t *testing.T) {
	target, entered, release := holdDeliveries(t)
	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys)
	sessions := sessionsNamed(t, "ses_held", 3)
	service := startService(t, url, keys, Deps{Introspect: introspector(sessions...)}, Decide)
	target.Store(service)

	results := make(chan []error, 1)
	go func() {
		errs, _ := connectAll(t, url, sessions)
		results <- errs
	}()
	<-entered
	// Pending counts the request at the hold too: nats.go takes a message off the subscription's
	// pending count only once its callback returns.
	testwait.WithinPolling(t, 5*time.Second, 10*time.Millisecond, "all three requests held by the subscription", func() bool {
		pending, _, err := service.sub.Pending()
		return err == nil && pending == len(sessions)
	})
	closed := make(chan struct{})
	go func() {
		service.Close()
		close(closed)
	}()
	time.Sleep(200 * time.Millisecond)
	release()
	<-closed
	errs := <-results
	for index, err := range errs {
		require.NoError(t, err, "session %d was refused: the closing replica dropped the request it held", index)
	}
}

// A replica closed while its connection is reconnecting does not come back into the queue group
// when the connection does. The fault this closes needs the drain to outlast the reconnect, so the
// closing replica holds a request when the hub stops, and that request's decision takes a second
// once the hold is released after the hub returns. A replica that drained there would leave its
// subscription among the connection's own, which nats.go sends again on the reconnect, and drop
// it on the client's side only once the held request is delivered: for the second its decision
// then takes, the hub routes requests to a replica with no subscriber, and refuses each of those
// clients at its authorization timeout. One that unsubscribes is out of the group before the hub
// returns, so every connection after the release is admitted, and by the other replica: the
// closing replica, whose decisions take a second, decides none of them.
func TestAReplicaClosedWhileReconnectingStaysOutOfTheGroup(t *testing.T) {
	target, entered, release := holdDeliveries(t)
	keys := testnats.NewCalloutKeys(t)
	server, url := testnats.StartWithCalloutServer(t, keys)
	probes := sessionsNamed(t, "ses_probe", 20)
	sessions := sessionsNamed(t, "ses_after", 20)
	deps := Deps{Introspect: introspector(append(probes, sessions...)...)}
	after := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		after[s.token] = true
	}
	var decidedAfter atomic.Int32
	slow := func(ctx context.Context, deps Deps, services []ServiceEntry, req Request) (Decision, error) {
		if after[req.Token] {
			decidedAfter.Add(1)
		}
		time.Sleep(time.Second)
		return Decide(ctx, deps, services, req)
	}
	closing := startService(t, url, keys, deps, slow)
	staying := startService(t, url, keys, deps, Decide)
	target.Store(closing)

	// Connect until the hub routes one request to the closing replica, which holds it.
	for _, s := range probes {
		go func() { _, _ = connectSession(t, url, s, nil) }()
		select {
		case <-entered:
		case <-time.After(300 * time.Millisecond):
			continue
		}
		break
	}
	pending, _, err := closing.sub.Pending()
	require.NoError(t, err)
	require.Positive(t, pending, "the closing replica was routed no request to hold")

	server.Stop(t)
	testwait.WithinPolling(t, 10*time.Second, 10*time.Millisecond, "both replicas reconnecting", func() bool {
		return !closing.conn.IsConnected() && !staying.conn.IsConnected()
	})
	closed := make(chan struct{})
	go func() {
		closing.Close()
		close(closed)
	}()
	time.Sleep(100 * time.Millisecond)
	server.Start(t)
	ready, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.NoError(t, staying.Ready(ready))
	time.Sleep(200 * time.Millisecond) // the closing replica's own reconnect
	release()

	for _, s := range sessions {
		_, err := connectSession(t, url, s, nil)
		require.NoError(t, err, "%s was refused after the hub returned: the closing replica took its request", s.id)
	}
	require.Zero(t, decidedAfter.Load(), "the closing replica decided %d of the connections made after the hub returned: it came back into the group", decidedAfter.Load())
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the closing replica's Close never returned")
	}
}

// A replica whose connection drops and comes back while a connected Close drains it does not stay
// in the queue group. The drain unsubscribes from the hub at once, but nats.go sends a draining
// subscription again on a reconnect (nats.go v1.55.0:6179-6230, resendSubscriptions, which does
// not check draining), so the hub puts the replica back in the group, and the drain's end then
// removes the subscription on the client's side only (nats.go:5504-5554, checkDrained). A Close that then
// waited out its decisions on that connection left the hub routing requests to a replica with no
// subscriber for as long as the longest of them ran, and each of those clients waited out the
// hub's whole authorization timeout and was refused. Here the closing replica holds a decision of
// five seconds, started before anything else, and a request at the hold keeps its drain from
// ending until the hub has restarted and the replica has reconnected; once the drain ends, every
// connection is admitted by the other replica, since a request the hub routed to the closing one
// would reach no subscriber.
func TestAReplicaThatReconnectsWhileItDrainsLeavesTheGroup(t *testing.T) {
	target, entered, release := holdDeliveries(t)
	keys := testnats.NewCalloutKeys(t)
	server, url := testnats.StartWithCalloutServer(t, keys)
	long := newSession(t, "ses_long", 15*time.Minute)
	probes := sessionsNamed(t, "ses_probe", 20)
	sessions := sessionsNamed(t, "ses_after", 20)
	deps := Deps{Introspect: introspector(append(append(probes, sessions...), long)...)}
	deciding := make(chan struct{})
	closing := startService(t, url, keys, deps, func(ctx context.Context, deps Deps, services []ServiceEntry, req Request) (Decision, error) {
		if req.Token == long.token {
			close(deciding)
			time.Sleep(5 * time.Second)
		}
		return Decide(ctx, deps, services, req)
	})
	// The closing replica is the only one in the group yet, so it takes this request.
	go func() { _, _ = connectSession(t, url, long, nil) }()
	<-deciding
	staying := startService(t, url, keys, deps, Decide)
	target.Store(closing)

	// Connect until the hub routes one request to the closing replica, which holds it.
	for _, s := range probes {
		go func() { _, _ = connectSession(t, url, s, nil) }()
		select {
		case <-entered:
		case <-time.After(300 * time.Millisecond):
			continue
		}
		break
	}
	pending, _, err := closing.sub.Pending()
	require.NoError(t, err)
	require.Positive(t, pending, "the closing replica was routed no request to hold")

	closed := make(chan struct{})
	go func() {
		closing.Close()
		close(closed)
	}()
	testwait.WithinPolling(t, 5*time.Second, 5*time.Millisecond, "the closing replica draining", closing.sub.IsDraining)
	server.Stop(t)
	testwait.WithinPolling(t, 10*time.Second, 10*time.Millisecond, "both replicas reconnecting", func() bool {
		return !closing.conn.IsConnected() && !staying.conn.IsConnected()
	})
	server.Start(t)
	ready, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.NoError(t, staying.Ready(ready))
	testwait.WithinPolling(t, 10*time.Second, 10*time.Millisecond, "the closing replica reconnected", closing.conn.IsConnected)
	// A round trip on the closing replica's connection, so the subscription nats.go sent again on
	// the reconnect has reached the hub.
	require.NoError(t, closing.conn.FlushTimeout(5*time.Second))
	release()
	testwait.WithinPolling(t, 5*time.Second, 5*time.Millisecond, "the closing replica's drain to end", func() bool {
		return !closing.sub.IsValid()
	})

	for _, s := range sessions {
		_, err := connectSession(t, url, s, nil)
		require.NoError(t, err, "%s was refused after the drain ended: the hub routed its request to the closing replica", s.id)
	}
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the closing replica's Close never returned")
	}
}

// Close returns only once the request its subscription was delivering when it unsubscribed has
// been decided. An unsubscribe, which Close makes on a connection that is reconnecting, ends the
// subscription at once (nats.go v1.55.0 Unsubscribe, :5477-5495) while its callback may still be delivering a
// request; a Close that went straight on to wait for the decisions started so far waited without
// the one that callback was about to start, and returned with it still to run. Here delivery is
// held with one request at the hold when the hub stops, the replica is closed while reconnecting,
// and the hold is released a moment later: the request's decision, a second long, has ended by
// the time Close returns. The hold spans the hub's stop, which can take seconds; the request is the
// first this replica has from that hub, so it shows the replica none of that time (hubClock), and
// it is decided rather than dropped as one the hub had given up on.
func TestCloseWaitsForTheRequestItWasDeliveringWhenItUnsubscribed(t *testing.T) {
	target, entered, release := holdDeliveries(t)
	keys := testnats.NewCalloutKeys(t)
	server, url := testnats.StartWithCalloutServer(t, keys)
	held := newSession(t, "ses_held", 15*time.Minute)
	var started, finished atomic.Int32
	deps := Deps{Introspect: introspector(held)}
	service := startService(t, url, keys, deps, func(ctx context.Context, deps Deps, services []ServiceEntry, req Request) (Decision, error) {
		started.Add(1)
		defer finished.Add(1)
		time.Sleep(time.Second)
		return Decide(ctx, deps, services, req)
	})
	target.Store(service)
	go func() { _, _ = connectSession(t, url, held, nil) }()
	<-entered

	server.Stop(t)
	testwait.WithinPolling(t, 10*time.Second, 10*time.Millisecond, "the replica reconnecting", func() bool {
		return !service.conn.IsConnected()
	})
	closed := make(chan struct{})
	go func() {
		service.Close()
		close(closed)
	}()
	time.Sleep(200 * time.Millisecond)
	release()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close never returned")
	}
	require.EqualValues(t, 1, finished.Load(),
		"Close returned before the decision of the request it was delivering ended (%d started)", started.Load())
}

// A request the hub has already given up on is dropped without a decision, and the broker is never
// asked about its token, whatever this broker's wall clock reads against the hub's: the drop reads
// how long the hub has waited from the hub's own clock (hubClock), never from the broker's. In each
// row a live request is decided and admitted, and a request held past its lifetime is dropped. The
// held one is held before it reaches the subscription, as a replica whose delivery goroutine fell
// behind holds what it has not reached; an earlier request from the same hub shows the replica the
// hub's clock, which is how it sees the hold. The drop is counted under its reason, and logged.
//
// Each row fails a drop that reads the broker's wall clock against the hub's expiry, each on one of
// its two requests. Three seconds ahead, that drop reads the live request as given up on, refuses
// it and fails the first step. Thirty seconds behind, it reads the request held seven seconds as
// live, asks the broker about it and admits it. Three seconds behind would fail nothing: seven
// seconds on the hub is past the expiry even read three seconds late, and a hold short enough to
// read as live then is one the hub's clock reads inside the two seconds the drop allows
// (hubWholeSeconds), within a second of either side by where the hub's seconds fall, so no hold
// separates the two every run. Thirty seconds is inside the minute a connection proof's issue time
// may be off this broker's clock (brokertoken.ProofClockSkew), so the live request's proof is
// accepted.
func TestARequestTheHubHasGivenUpOnIsDroppedWhateverTheBrokersClockReads(t *testing.T) {
	for name, skew := range map[string]time.Duration{
		"the broker 3 s ahead of the hub": 3 * time.Second,
		"the broker 30 s behind the hub":  -30 * time.Second,
	} {
		t.Run(name, func(t *testing.T) {
			target, entered, release := holdDeliveries(t)
			keys := testnats.NewCalloutKeys(t)
			url := testnats.StartWithCallout(t, keys)
			live, late := newSession(t, "ses_live", 15*time.Minute), newSession(t, "ses_late", 15*time.Minute)
			var asked atomic.Int32
			introspect := introspector(live, late)
			log := &logtest.Buffer{}
			registry := metrics.New()
			deps := Deps{
				Introspect: func(ctx context.Context, token string) (oauthwire.Introspection, error) {
					asked.Add(1)
					return introspect(ctx, token)
				},
				Log:     slog.New(slog.NewTextHandler(log, nil)),
				Metrics: NewMetrics(registry),
				Now:     func() time.Time { return time.Now().Add(skew) },
			}
			service := startService(t, url, keys, deps, Decide)

			nc, err := connectSession(t, url, live, nil)
			require.NoError(t, err, "a live request was not admitted with the broker's clock %s off the hub's:\n%s", skew, log.String())
			nc.Close()
			require.EqualValues(t, 1, asked.Load(), "the live request was not decided")

			target.Store(service)
			refused := make(chan error, 1)
			go func() {
				_, err := connectSession(t, url, late, nil)
				refused <- err
			}()
			<-entered
			// Past the request's two-second lifetime and the two seconds the drop allows for the
			// hub writing whole seconds (hubClock.gaveUp), with a second to spare.
			time.Sleep(7 * time.Second)
			release()
			// Close waits until the released request is answered or dropped, then moves what the
			// replica counted into the metric and has the reporter write its last line, so the read
			// below needs no time bound however late the replica's goroutines run. Close waits for
			// that line for drainBound at most, so the read waits for the reporter to end, and fails
			// rather than hangs if it never does.
			service.Close()
			select {
			case <-service.reported:
			case <-time.After(2 * drainBound):
				t.Fatalf("the reporter had not ended %s after Close, which waits for it %s at most:\n%s", 2*drainBound, drainBound, log.String())
			}
			require.Contains(t, scrape(t, registry), `broker_nats_callout_dropped_total{reason="hub_gave_up"} 1`,
				"the drop was not counted under its reason:\n%s", log.String())
			require.Contains(t, log.String(), "hub_gave_up=1", "the drop was not logged")
			require.EqualValues(t, 1, asked.Load(), "the broker was asked about a request the hub had given up on:\n%s", log.String())
			require.ErrorIs(t, <-refused, nats.ErrAuthorization, "the hub admitted a client whose request it had given up on")
		})
	}
}

// scrape is registry's whole Prometheus text, as the broker's metrics port serves it.
func scrape(t *testing.T, registry *metrics.Registry) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	registry.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return recorder.Body.String()
}

// A request nats.go drops on arrival, past the subscription's pending limits (start), never reaches
// deliver, and is counted all the same: the counter reads the subscription's own count of them
// (Subscription.Dropped) into broker_nats_callout_dropped_total under pending_limit. Both bounds
// are zero here, so the limit is one request; delivery of the first is held, and the two sent
// behind it find the limit full.
func TestARequestDroppedPastThePendingLimitIsCounted(t *testing.T) {
	withWaitingBound(t, 0)
	withRefusingBound(t, 0)
	withLiveness(t, 20*time.Millisecond, 10*time.Second)
	target, entered, release := holdDeliveries(t)
	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys)
	registry := metrics.New()
	service := startService(t, url, keys, Deps{Metrics: NewMetrics(registry)}, Decide)
	target.Store(service)

	connect := func() { _, _ = nats.Connect(url, nats.NoReconnect(), nats.Timeout(5*time.Second)) }
	go connect()
	<-entered
	go connect()
	go connect()
	testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, "the requests past the pending limit counted", func() bool {
		return strings.Contains(scrape(t, registry), `broker_nats_callout_dropped_total{reason="pending_limit"} 2`)
	})
	release()
}
