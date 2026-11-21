package natscallout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/ciphertoken"
)

const (
	// authSubject is the subject the server sends each authorization request on.
	authSubject = "$SYS.REQ.USER.AUTH"
	// queueGroup is the queue group every replica subscribes authSubject in, so the server's
	// request reaches one replica and a stopped replica is sent none.
	queueGroup = "envoy-auth"
	// serverXKeyHeader carries the server's curve key on a request it sealed to the service's.
	serverXKeyHeader = "Nats-Server-Xkey"
	// globalAccount is the account every admitted user is placed in: the hub keeps one, the
	// global account, and its streams and every static user's grant live there.
	globalAccount = "$G"
	// connectionName is the name the service's own connection carries in the server's listings.
	connectionName = "envoy-broker-callout"
)

// concurrentDecisions bounds the decisions one replica makes at once: a reconnect storm after a
// server restart sends every connection's request together. It is a var so a test can make
// requests queue for a slot with a handful of them (queue_test.go).
var concurrentDecisions = 64

// maxWaiting bounds the requests one replica holds waiting for a decision slot; the one past it is
// refused without a decision (waitingRefusal), on a goroutine of its own (maxRefusing). Each
// waiting request holds a goroutine, its sealed request and a deadline until a slot frees or its
// budget ends, and a client holding no credential can send one per TCP connect, so with no bound
// what a replica holds grows with whatever such a client sends. The bound is sixteen times the
// slots: at the slowest decision this package's storm measured (146 ms), concurrentDecisions slots
// decide about 64 x 1.5 s / 146 ms = 650 requests inside one budget, so a request behind 1024
// others would be refused at its budget anyway. At faster decisions the bound refuses some that
// could have waited; their clients retry a refusal (IgnoreAuthErrorAbort) and hear it at once
// rather than after their budget. It is a var so a test can fill it with a handful of requests
// (queue_test.go, storm_test.go).
var maxWaiting = 1024

// maxRefusing bounds the requests one replica refuses at once past maxWaiting, each on a goroutine
// of its own; the one past it is dropped unanswered (deliver), and the hub refuses that client
// itself at its authorization timeout.
//
// A request past maxWaiting is refused rather than dropped because a refusal takes no broker call
// and about 0.3 ms of CPU (the request opened and its JWT verified, the answer signed and sealed;
// 3.6 ms under -race), so its client hears at once, and why, and retries on its own schedule, where
// a dropped one is held by the hub for its whole two-second authorization timeout and then refused
// with no reason. It is refused off the delivery goroutine because nats.go delivers every request
// on that one goroutine: refused there, each refusal held back the delivery of every request
// behind it, and in a storm the last were delivered after the hub had refused their clients
// (storm_test.go). And it is bounded because a client that holds no credential can send a request
// per TCP connect: 1024 refusals at once are about 0.3 s of CPU, answered well inside a budget by a
// replica of a few cores, and a replica sent more past both bounds than that is sent more than it
// can refuse, where a goroutine each would only grow. It is a var so a test can make a replica drop
// (queue_test.go).
var maxRefusing = 1024

// waitingRefusal is what a request that arrives with bound others already waiting is refused with.
// Its label in the decision counter is labelWaitingBound.
func waitingRefusal(bound int) *Refusal {
	return refuse(labelWaitingBound, fmt.Sprintf("%d requests already wait for a decision slot on this replica, the most it holds", bound))
}

// pendingBytes bounds in bytes the requests nats.go holds for the subscription that its callback
// has not taken yet, beside the count start sets (Subscription.SetPendingLimits). The count binds
// first: 64 MiB over the 2,049 requests the deployed bounds allow is 32 KiB a request, where a
// session's sealed request measures 2.3 KB and one that holds no credential 1.2 KB.
const pendingBytes = 64 << 20

// decideBudget is how long one decision may take. A Go client fails its connect when the server's
// answer arrives later than two seconds: nats.go sets one deadline of Opts.Timeout, 2 s by
// default, over the whole handshake (nats.go, processConnectInit), and the server sends its PONG
// only once the callout answers; the server gives up on its own at its authorization timeout,
// nats-server's default of 2 s (server/const.go, AUTH_TIMEOUT), which the hub takes by naming
// none. So the decision gets 1.5 s, and the dial, the request's trip here and the answer's trip
// back get the other 0.5 s. A decision that runs past it is refused naming the step it was in,
// whatever it decided, so no connection is admitted past its budget.
//
// It is a var so a test can raise it: at 2.5 s the same blocking stub makes a client fail its
// connect, which is the control this budget exists for (deadline_test.go). A replica reads it once,
// as it starts (replicaSettings).
var decideBudget = 1500 * time.Millisecond

// budgetRefusal is what a decision that ran past its budget is refused with, naming the step it
// was in. Its label in the decision counter is labelBudget. It names no duration: the budget is
// the replica's (replicaSettings), and the step is what an operator acts on.
func budgetRefusal(step string) *Refusal {
	return refuse(labelBudget, "decision exceeded its budget at "+step)
}

// drainBound bounds how long Close waits for the subscription to finish draining the requests the
// hub had already routed to this replica, and then for the subscription's delivery goroutine to
// finish (close). Each request past its budget is refused at once without a decision (answer), so
// a long queue drains in far less than this, and the delivery goroutine takes a request in
// microseconds (deliver).
const drainBound = 5 * time.Second

// minimumDecision is the least of its budget a request must have left when it gets a decision
// slot for its decision to start. Less than that, and the wait for the slot spent the budget: the
// request is refused naming the wait, rather than starting an introspection that its deadline
// cancels and naming that.
const minimumDecision = 100 * time.Millisecond

// holdDelivery, when a test sets it, runs at the start of every delivery of a request to replica
// s, before the request's budget starts; a test that blocks in it holds the hub's requests in that
// replica's subscription, which is where a replica that drops rather than drains them on Close
// loses them (queue_test.go). Nil outside tests, where reading it costs every delivery one load of
// a nil variable and a branch that always goes the same way, against the context and goroutine a
// delivery makes; a test sets it before any replica starts and clears it once every replica it
// started has closed, when no delivery is left to read it (holdDeliveries).
var holdDelivery func(s *Service)

// Settings is where the service connects and the keys it holds.
type Settings struct {
	// URLs are the hub's NATS servers.
	URLs []string
	// SeedFile holds the service's own NATS user seed, a user the hub lists as an auth user, so
	// it is never sent to itself; IssuerSeedFile the account seed that signs every answer and
	// every user it admits, the hub's auth_callout issuer; XKeySeedFile the curve seed the hub
	// seals each request to, read by nkeys.FromSeed, which derives the curve key a curve seed
	// names (nkeys.FromCurveSeed).
	SeedFile, IssuerSeedFile, XKeySeedFile string
	// Services are the services it admits by platform identity (ParseServices).
	Services []ServiceEntry
}

// Service is the running auth service: one connection to the hub, subscribed to the authorization
// subject in the replicas' queue group.
type Service struct {
	conn   *nats.Conn
	sub    *nats.Subscription
	answer *answerer
	slots  chan struct{}
	// wait counts the requests deliver started on goroutines of their own, waiting for a slot,
	// deciding or being refused, which Close waits out.
	wait sync.WaitGroup
	// stopHeld ends the asks about the projects a held entry can name (heldAccess.run), and
	// heldDone is closed once they have ended.
	stopHeld context.CancelFunc
	heldDone chan struct{}
	// waiting counts the requests waiting for a decision slot, which maxWaiting bounds, and
	// refusing the requests being refused past it, which maxRefusing bounds. Each is counted on
	// the delivery goroutine before the request's own goroutine starts, so it includes requests
	// whose goroutine the scheduler has not run yet: counted any later, a bound would not bound
	// the goroutines, which are what it holds.
	waiting, refusing atomic.Int64
	// delivered is closed once the subscription's delivery goroutine has returned from its last
	// callback and ended (Subscription.SetClosedHandler, which waitForMsgs invokes at nats.go
	// v1.55.0 :3796-3889), after which deliver adds nothing more to wait.
	delivered chan struct{}
	// beat is when the heartbeat last took a decision slot, in Unix nanoseconds; done ends the
	// heartbeat and the counter at Close, and beaten and counted are closed once each has ended,
	// which Close waits for.
	beat            atomic.Int64
	done            chan struct{}
	beaten, counted chan struct{}
	// stopReport stops the reporter, which Close does once every request has been answered, and
	// reported is closed once its last report is written (reportWhileRunning).
	stopReport, reported chan struct{}
	closeOnce            sync.Once
	// settings are livenessTick, livenessBound, decideBudget, maxWaiting and maxRefusing as start
	// read them. The heartbeat, the counter, the reporter, the delivery goroutine and Livez read
	// these, never the package's variables, which a test sets and restores while replicas run:
	// read from those goroutines, each write would race with them.
	settings replicaSettings
}

// replicaSettings are the settings a replica reads once, as it starts.
type replicaSettings struct {
	tick, bound, budget     time.Duration
	maxWaiting, maxRefusing int
}

// livenessTick is how often the heartbeat tries to take a decision slot, and the counter and the
// reporter run (countWhileRunning, reportWhileRunning); livenessBound how old the heartbeat's last
// beat may be before the service reads as stalled. Vars so a test can make a stall read stale in
// milliseconds (liveness_test.go).
var (
	livenessTick  = time.Second
	livenessBound = 10 * time.Second
)

// beatWhileTurning is the heartbeat: every livenessTick it takes a decision slot and gives it back,
// and records the time it did. When the loop turns a slot is free within a tick, idle or busy, so
// the beat stays fresh; when every slot is held by a decision that never ends, no request can be
// decided and the beat goes stale, which Livez reports. A process stopped outright never answers
// the probe at all, which a liveness probe also reads as a failure. Start stores the first beat
// itself, before anything can read it. It writes no log line: a write can block, and a blocked
// beat would read as a stalled replica (reportWhileRunning).
func (s *Service) beatWhileTurning() {
	defer close(s.beaten)
	ticker := time.NewTicker(s.settings.tick)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
		}
		select {
		case s.slots <- struct{}{}:
			<-s.slots
			s.beat.Store(time.Now().UnixNano())
		case <-s.done:
			return
		}
	}
}

// Livez is GET /livez on the broker's metrics port, its whole answer: 200 "ok" while the auth
// service's loop turned within livenessBound, 503 with a line naming how long ago it last turned
// once it has not, and 200 when service answers nil, a broker that runs no auth service or has
// not started it yet, since nothing then can stall. A liveness probe that reads the status kills a
// replica whose loop has stalled: its connection to the hub then resets and it leaves the queue
// group, where a stalled replica would keep being sent requests it never answers.
func Livez(service func() *Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if s := service(); s != nil {
			if age := time.Since(time.Unix(0, s.beat.Load())); age > s.settings.bound {
				http.Error(w, fmt.Sprintf("the NATS auth service's loop last turned %s ago", age.Round(time.Second)), http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = w.Write([]byte("ok\n"))
	})
}

// decideFunc is how the service decides a request: Decide, unless a test replaces it to hand the
// answer a grant of its own.
type decideFunc func(ctx context.Context, deps Deps, services []ServiceEntry, req Request) (Decision, error)

// Start connects to the hub as the service's own user and answers every authorization request it
// is sent, in the replicas' queue group, until Close. It reads the three seeds first, refusing a
// file that is unreadable or holds a seed of the wrong kind (a user, an account, a curve key) by
// the setting's own name and never the seed. The hub need not answer yet: the connection is made
// in the background and retried, so a hub that is down or restarting when the broker starts is
// answered once it is up.
func Start(ctx context.Context, settings Settings, deps Deps) (*Service, error) {
	return start(ctx, settings, deps, Decide)
}

func start(ctx context.Context, settings Settings, deps Deps, decide decideFunc) (*Service, error) {
	issuer, err := config.ReadSeedFile("BROKER_NATS_ISSUER_SEED_FILE", settings.IssuerSeedFile, nkeys.PrefixByteAccount)
	if err != nil {
		return nil, err
	}
	xkey, err := config.ReadSeedFile("BROKER_NATS_XKEY_SEED_FILE", settings.XKeySeedFile, nkeys.PrefixByteCurve)
	if err != nil {
		return nil, err
	}
	if _, err := config.ReadSeedFile("BROKER_NATS_SEED_FILE", settings.SeedFile, nkeys.PrefixByteUser); err != nil {
		return nil, err
	}
	// The service's own connection is the bus's: its seed by file, its reconnects, its silent-cut
	// detection, and the hub reached wherever it is, since the broker and the hub are separate
	// deployments.
	lookup := func(name string) (string, bool) {
		switch name {
		case "NATS_NKEY_SEED_FILE":
			return settings.SeedFile, true
		case bus.AllowRemoteEnvVar:
			return "1", true
		}
		return "", false
	}
	// WithImmediateReconnect: every other client of the hub waits on this one. A restart drops
	// the service's own connection with theirs, and a client that comes back before the service
	// has resubscribed is refused until it does, so the service retries at once where they wait a
	// second (internal/bus).
	conn, err := bus.Dial(connectionName, settings.URLs, lookup,
		bus.WithConnectInBackground(), bus.WithSilentCutDetection(), bus.WithImmediateReconnect())
	if err != nil {
		return nil, fmt.Errorf("natscallout: connect to %s: %w", strings.Join(settings.URLs, ","), err)
	}
	// Dispatch's answers about the projects of the entries held to their exchange projects are
	// read in the background once the service runs, so no decision waits on Dispatch (scopeGrant).
	// The answerer's copy of deps holds them; a decision before the first answer refuses
	// project_access_unknown, and its client retries.
	deps.held = newHeldAccess(deps, settings.Services)
	s := &Service{
		conn: conn,
		answer: &answerer{issuer: issuer, xkey: xkey, deps: deps, services: settings.Services, decide: decide,
			drops: &drops{}, hub: newHubClock(), dropped: &tally{}, refusals: &tally{}, failures: &tally{}},
		slots:      make(chan struct{}, concurrentDecisions),
		delivered:  make(chan struct{}),
		done:       make(chan struct{}),
		beaten:     make(chan struct{}),
		counted:    make(chan struct{}),
		stopReport: make(chan struct{}),
		reported:   make(chan struct{}),
		heldDone:   make(chan struct{}),
		settings: replicaSettings{tick: livenessTick, bound: livenessBound, budget: decideBudget,
			maxWaiting: maxWaiting, maxRefusing: maxRefusing},
	}
	s.beat.Store(time.Now().UnixNano())
	s.sub, err = conn.QueueSubscribe(authSubject, queueGroup, s.deliver)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("natscallout: subscribe %s: %w", authSubject, err)
	}
	s.sub.SetClosedHandler(func(string) { close(s.delivered) })
	// nats.go holds the requests its callback has not taken yet in a list of its own, of 500,000
	// messages or 64 MiB unless told otherwise (nats.go v1.55.0 defaults, :5944-5948; assignment,
	// :5222-5224), and past its bound drops each new one and reports a slow consumer, which the bus logs. The
	// callback takes a request in microseconds, so the list grows only while the callback cannot
	// run at all, and once it runs again it places no more of what the list holds than maxWaiting
	// to wait and maxRefusing to be refused, and drops the rest itself. So the list holds those and
	// the one being delivered, which nats.go counts until its callback returns (:3798-3815); a
	// request past them, which the callback would drop all the same a moment later, is dropped on
	// arrival without holding its memory meanwhile.
	if err := s.sub.SetPendingLimits(s.settings.maxWaiting+s.settings.maxRefusing+1, pendingBytes); err != nil {
		conn.Close()
		return nil, fmt.Errorf("natscallout: bound the requests held for %s: %w", authSubject, err)
	}
	go s.beatWhileTurning()
	go s.countWhileRunning()
	go s.reportWhileRunning()
	var asks context.Context
	asks, s.stopHeld = context.WithCancel(context.WithoutCancel(ctx))
	go func() { defer close(s.heldDone); deps.held.run(asks) }()
	return s, nil
}

// deliver is the subscription's callback, which nats.go runs for each request in turn on the one
// goroutine it delivers the subscription's messages on (nats.go v1.55.0 waitForMsgs, :3796-3889). So it
// never blocks, on any branch: it counts the request against a bound, starts a goroutine for it or
// drops it, and returns, and the request is opened, settled and answered on its own goroutine.
// Anything slower here would hold back the delivery of every request behind it, each unseen in
// nats.go's list while the hub's timeout on it runs, and each given its whole budget late once
// delivered.
//
// The request's budget counts from here, and it is one clock: the deadline set here bounds both
// the wait for a decision slot and the decision itself (decideWithin reads it). Every Add to wait
// happens on this goroutine, before the subscription's closed handler runs on it as it ends
// (delivered), and Close waits for that handler before it waits on wait, for drainBound at most,
// which a callback that takes microseconds never comes near: so every Add happens before its Wait.
func (s *Service) deliver(msg *nats.Msg) {
	if holdDelivery != nil {
		holdDelivery(s)
	}
	received := time.Now()
	if s.waiting.Add(1) <= int64(s.settings.maxWaiting) {
		ctx, cancel := context.WithDeadline(context.Background(), received.Add(s.settings.budget))
		s.wait.Add(1)
		go func() {
			defer s.wait.Done()
			defer cancel()
			select {
			case s.slots <- struct{}{}:
				s.waiting.Add(-1)
				defer func() { <-s.slots }()
			case <-ctx.Done():
				s.waiting.Add(-1)
			}
			s.answer.answer(msg, received, s.answer.decideWithin(ctx))
		}()
		return
	}
	s.waiting.Add(-1)
	if s.refusing.Add(1) <= int64(s.settings.maxRefusing) {
		s.wait.Add(1)
		go func() {
			defer s.wait.Done()
			defer s.refusing.Add(-1)
			s.answer.answer(msg, received, refuseWith(waitingRefusal(s.settings.maxWaiting)))
		}()
		return
	}
	s.refusing.Add(-1)
	// Dropped: the hub refuses its client at its authorization timeout. Counted, and logged by the
	// reporter (reportWhileRunning), not here: a log line is a write, which can block, and past both
	// bounds it would be a line a request.
	if s.answer.drops.add(dropPastBounds) {
		s.answer.drops.note(dropPastBounds, pastBoundsDetail)
	}
}

// Close stops answering.
//
// Connected, it drains the subscription: that leaves the queue group at once, so the hub sends the
// next request to another replica, and still delivers every request the hub had already routed
// here, which an unsubscribe would drop and leave each of those clients waiting out the hub's
// whole authorization timeout.
//
// Reconnecting, it unsubscribes instead. A drain then would leave the subscription among the
// connection's own, which nats.go sends again on the reconnect (nats.go v1.55.0
// resendSubscriptions, :6179-6230), putting this closing replica back in the queue group; the
// drain's end then removes it on the client's side only (checkDrained, :5504-5554), and every
// request the hub routes here after that has no subscriber. An unsubscribe removes it from the
// connection, so the reconnect never sends it, and while the connection is down the hub can route
// nothing here.
//
// A drain can meet that reconnect all the same: the connection drops while the drain still has
// requests to deliver and comes back before the drain ends. So Close counts the connection's
// reconnects across the drain, and when one came it closes the connection before it waits out its
// decisions, which takes the replica out of the group at once. The answers of the decisions it
// cuts short are lost, and their clients are refused at the hub's timeout; left open, the
// connection would have the hub route requests to no subscriber for as long as those decisions
// run. A reconnect cannot send the subscription once the drain's end or the unsubscribe has
// removed it, and both take the connection's lock, as the reconnect's count and its resend do in
// one hold of it (nats.go:3562-3600), so a reconnect that sent it is counted by then.
//
// Either way it then waits for the subscription's delivery goroutine to end, which may still be
// delivering a request when an unsubscribe ends the subscription, and then for every request that
// goroutine started.
func (s *Service) Close() {
	s.closeOnce.Do(s.close)
}

// close is Close's body, which runs once however many times Close is called: a caller that stops
// a replica early and a cleanup that stops it at the end may both call Close.
func (s *Service) close() {
	close(s.done)
	<-s.beaten
	<-s.counted
	// nats.go's count of the requests it dropped past the pending limits is read while the
	// subscription still answers it, so what it dropped since the counter's last tick reaches the
	// metric. What it drops during the drain itself is not counted: once the drain has ended the
	// subscription answers Dropped with ErrBadSubscription (nats.go v1.55.0, nats.go:6008-6018).
	// The hub logs each such client's authorization timeout.
	s.countDrops()
	if s.conn.IsConnected() {
		reconnects := s.conn.Stats().Reconnects
		if s.sub.Drain() == nil {
			s.awaitDelivered()
		}
		// After a drain this removes nothing; after a drain that outran drainBound it is what
		// keeps the subscription from being sent again.
		_ = s.sub.Unsubscribe()
		if s.conn.Stats().Reconnects != reconnects {
			s.conn.Close()
		}
	} else {
		_ = s.sub.Unsubscribe()
	}
	s.awaitDelivered()
	s.wait.Wait()
	s.stopHeld()
	<-s.heldDone
	// The drops counted while the last requests were answered, which the reporter's last report
	// then logs.
	s.countDrops()
	close(s.stopReport)
	select {
	case <-s.reported:
	case <-time.After(drainBound):
	}
	s.conn.Close()
}

// awaitDelivered waits, for drainBound at most, for the subscription's delivery goroutine to end
// (delivered), which it does once the subscription has ended and its last callback has returned.
func (s *Service) awaitDelivered() {
	select {
	case <-s.delivered:
	case <-time.After(drainBound):
	}
}

// readyPoll is how often Ready asks whether the service's subscription has reached the hub, and
// readyFlush how long one ask waits for the hub's answer.
const (
	readyPoll  = 50 * time.Millisecond
	readyFlush = time.Second
)

// Ready returns once the service's subscription has reached the hub, so the next connection the
// hub authorizes is sent to it, or ctx's error when ctx ends first; ctx needs no deadline, as the
// broker's own serving context has none. Start does not wait for it, since a hub that is down
// when the broker starts is answered once it is up; the broker logs when it is (cmd/cipherd).
func (s *Service) Ready(ctx context.Context) error {
	for {
		if s.conn.IsConnected() && s.conn.FlushTimeout(readyFlush) == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyPoll):
		}
	}
}

// answerer answers one authorization request.
type answerer struct {
	issuer, xkey nkeys.KeyPair
	deps         Deps
	services     []ServiceEntry
	decide       decideFunc
	// drops is the hand-off of what the service drops unanswered, which the counter moves into
	// dropped (countDrops); dropped, refusals by label and failures, the answers it could not send,
	// by step, are the tallies the reporter logs (reportWhileRunning). hub reads each hub server's
	// clock for the requests it has given up on.
	drops                       *drops
	dropped, refusals, failures *tally
	hub                         *hubClock
}

// refusalWork, when a test sets it, runs in every refusal before it is answered: a stand-in,
// scaled up, for the CPU a refusal costs to open, read, sign and seal (storm_test.go). Nil outside
// tests; a test sets it before any replica starts and clears it once every replica it started has
// closed, when no answer is left to read it.
var refusalWork func()

// settle is how answer reaches the verdict on a request it has opened and read: the decision it
// answers, or the refusal. A request that waited for a decision slot is decided under its budget
// (decideWithin); one past maxWaiting is refused with no decision (refuseWith).
type settle func(req Request) (Decision, *Refusal)

// decideWithin settles a request under ctx, which carries its budget from when it reached the
// subscription (deliver), so the time it waited for a decision slot is spent from it: a request
// left less than minimumDecision of it is refused naming the wait, without asking the broker
// anything, and one whose decision ran past it is refused naming the answer.
func (a *answerer) decideWithin(ctx context.Context) settle {
	return func(req Request) (Decision, *Refusal) {
		if deadline, _ := ctx.Deadline(); time.Until(deadline) < minimumDecision {
			return Decision{}, budgetRefusal("the wait for a decision slot")
		}
		decision, err := a.decide(ctx, a.deps, a.services, req)
		refused := asRefusal(err)
		// A decision that ran past the deadline is refused, whatever it decided: the budget is all
		// of the client's two seconds that the dial and the two trips do not need (decideBudget),
		// so admitting it there could hand the server a user for a connection no longer waiting.
		if refused == nil && ctx.Err() != nil {
			refused = budgetRefusal("the answer")
		}
		return decision, refused
	}
}

// refuseWith settles a request with refused, and decides nothing.
func refuseWith(refused *Refusal) settle {
	return func(Request) (Decision, *Refusal) { return Decision{}, refused }
}

// issuelessAllowance is how far past its expiry, on this broker's clock, a request that carries no
// issue time must be before it is dropped as one the hub has given up on. Such a request gives no
// reading of the hub's clock (hubClock), so its expiry is read against this broker's own, which
// can be off the hub's: the allowance is the skew the broker already allows any clock it compares
// with its own, a connection proof's (ciphertoken.ProofClockSkew, a minute). A hub writes an issue
// time on every request it sends (jwt/v2 claims.go:190), so this is a request from no hub of this
// fleet, and is answered under its budget like any other until then.
const issuelessAllowance = ciphertoken.ProofClockSkew

// hubGaveUp reports whether the hub that sent claims, which reached the subscription at received,
// has certainly given up on it. A request that names no expiry is not one it can have.
func (a *answerer) hubGaveUp(claims *jwt.AuthorizationRequestClaims, received time.Time) bool {
	switch {
	case claims.Expires <= 0:
		return false
	case claims.IssuedAt <= 0:
		return a.deps.now().After(time.Unix(claims.Expires, 0).Add(issuelessAllowance))
	default:
		return a.hub.gaveUp(claims.Server.ID, claims.IssuedAt, claims.Expires, received, time.Now())
	}
}

// answer opens msg, settles it and answers it, sealed to the server's key; received is when the
// request reached the subscription, from which the answer is timed. A request it cannot open or
// read is dropped unanswered: the server refuses that connection at its authorization timeout, and
// the drop is counted (drops) and reported (reportWhileRunning).
//
// So is a request the server has already given up on, without a decision or a broker call: nobody
// waits for its answer. Whether it has is read from the server's own clock (hubClock), so no skew
// between the server's clock and this broker's drops a request the server still waits for. A
// request in the clear is refused under its own label before that, whatever else is true of it,
// and the reporter writes its refusals at error: that line is how an operator learns the hub lost
// its curve key, which a drop would hide. The expiry does not set the budget either: written in
// whole seconds, it can name a moment up to a second before the server gives up, and would cut a
// budget of 1.5 s by as much.
func (a *answerer) answer(msg *nats.Msg, received time.Time, settle settle) {
	serverXKey := msg.Header.Get(serverXKeyHeader)
	data := msg.Data
	if serverXKey != "" {
		opened, err := a.xkey.Open(data, serverXKey)
		if err != nil {
			if a.drops.add(dropUnopened) {
				a.drops.note(dropUnopened, "did not open with the service's curve key: "+err.Error())
			}
			return
		}
		data = opened
	}
	claims, err := jwt.DecodeAuthorizationRequestClaims(string(data))
	if err != nil {
		if a.drops.add(dropUnreadable) {
			a.drops.note(dropUnreadable, "no authorization request JWT: "+err.Error())
		}
		return
	}
	req := Request{
		UserNkey: claims.UserNkey, ServerID: claims.Server.ID,
		Nonce: claims.ClientInformation.Nonce, Token: claims.ConnectOptions.Token, Sig: claims.ConnectOptions.SignedNonce,
		User: claims.ConnectOptions.Username, Password: claims.ConnectOptions.Password, Host: claims.ClientInformation.Host,
	}
	kind := credentialKind(req)
	var decision Decision
	var refused *Refusal
	switch {
	case serverXKey == "":
		// The service always holds a curve key, and a hub configured with it seals every request,
		// so a request in the clear means the hub's callout lost its xkey and every token and proof
		// is crossing the wire unencrypted. Refused, and said loudly: its refusals are logged at
		// error (report).
		refused = refuse(labelUnsealed, "the authorization request was not sealed to the service's key")
	case a.hubGaveUp(claims, received):
		if a.drops.add(dropHubGaveUp) {
			a.drops.note(dropHubGaveUp, fmt.Sprintf("the hub %s had given up on a request from %s, expiring at %s on its clock",
				claims.Server.ID, req.Host, time.Unix(claims.Expires, 0).UTC().Format(time.RFC3339)))
		}
		return
	default:
		decision, refused = settle(req)
	}
	user, refused := admit(req, decision, refused)
	if refused != nil && refusalWork != nil {
		refusalWork()
	}
	// The answer goes out first, and only then is what happened counted, so nothing a metric or
	// the reporter's tallies do - the decision counter's lock taken by a scrape while it renders,
	// a stalled log - delays the answer, and the time it is counted under is the time it took.
	// Neither holds this request's slot or its place in the refusing bound for longer than that:
	// a scrape holds no metric's lock while it writes to its client (metrics.Registry.Handler),
	// and the reporter alone writes the log.
	a.send(msg, req, user, refused, serverXKey)
	a.deps.Metrics.record(kind, refused, time.Since(received))
	if refused != nil {
		a.refusals.add(refused.Label, func() string {
			detail := refused.Reason + " (" + kind + " from " + req.Host + ")"
			if refused.Cause != nil {
				detail += ": " + refused.Cause.Error()
			}
			return detail
		})
	}
}

// send answers msg: user, or refused when it is not nil, signed by the issuer and sealed to the
// server's key when the server sent one. A step that fails is counted for the reporter
// (failures), and the server refuses that client at its own timeout.
func (a *answerer) send(msg *nats.Msg, req Request, user *jwt.UserClaims, refused *Refusal, serverXKey string) {
	encoded, err := sign(a.issuer, req, user, refused)
	if err != nil {
		a.failures.add("encode", func() string { return "could not be encoded for " + req.Host + ": " + err.Error() })
		return
	}
	out := []byte(encoded)
	if serverXKey != "" {
		if out, err = a.xkey.Seal(out, serverXKey); err != nil {
			a.failures.add("seal", func() string { return "could not be sealed to the server's key: " + err.Error() })
			return
		}
	}
	if err := msg.Respond(out); err != nil {
		a.failures.add("send", func() string { return "could not be sent to " + req.Host + ": " + err.Error() })
	}
}

// logAt writes one line to the service's log. Only the reporter calls it (reportWhileRunning).
func (a *answerer) logAt(level slog.Level, message string, args ...any) {
	if a.deps.Log != nil {
		a.deps.Log.Log(context.Background(), level, message, args...)
	}
}

// asRefusal is err as the refusal it answers: itself when it is one, and a refusal naming the
// service's own failure, with err as its cause, when it is not.
func asRefusal(err error) *Refusal {
	if err == nil {
		return nil
	}
	var refused *Refusal
	if errors.As(err, &refused) {
		return refused
	}
	return refuse(labelServiceFailed, "the service failed").because(err)
}

// admit is the verdict an answer carries, settled before it is counted: refused when the decision
// refused, and otherwise the user decision admits, once the grant guard (guard) passes it. Every
// answer the service sends passes here, whatever decided it, so this is the one place the guard
// runs.
func admit(req Request, decision Decision, refused *Refusal) (*jwt.UserClaims, *Refusal) {
	if refused != nil {
		return nil, refused
	}
	if refused := guard(decision.Grant); refused != nil {
		return nil, refused
	}
	user := jwt.NewUserClaims(req.UserNkey)
	user.Audience = globalAccount
	user.Name = decision.Name
	user.Expires = decision.Expires.Unix()
	user.Pub.Allow.Add(decision.Grant.Publish...)
	user.Sub.Allow.Add(decision.Grant.Subscribe...)
	user.Pub.Deny.Add(decision.Grant.PublishDeny...)
	user.Sub.Deny.Add(decision.Grant.SubscribeDeny...)
	return user, nil
}

// sign is the authorization response for req, signed by issuer: user, signed by issuer for req's
// user key, or, when refused is not nil, refused's one line as the response's error.
func sign(issuer nkeys.KeyPair, req Request, user *jwt.UserClaims, refused *Refusal) (string, error) {
	response := jwt.NewAuthorizationResponseClaims(req.UserNkey)
	if response == nil {
		return "", errors.New("natscallout: the request names no user key")
	}
	response.Audience = req.ServerID
	if refused != nil {
		response.Error = refused.Reason
		return response.Encode(issuer)
	}
	userJWT, err := user.Encode(issuer)
	if err != nil {
		return "", err
	}
	response.Jwt = userJWT
	return response.Encode(issuer)
}
