package delivery

// Two envoy-dispatch processes over one Postgres and one NATS, as a rolling release runs them: each
// intake here is a process of its own, with its own NATS connection, its own database pool and its
// own GitHub, and the tests hold which of them handles each message when the one delivering stops,
// dies, or is cut off and comes back.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// errCutOff is every statement of a process the test has cut off from Postgres.
var errCutOff = errors.New("the process is cut off from the database")

// cuttableLeases is one process's lease pool, whose every statement fails once the test cuts it, as
// a process the network cut off from Postgres or the kernel killed sends nothing: the hold it
// renews on this pool then lapses.
type cuttableLeases struct {
	pool *pgxpool.Pool
	cut  atomic.Bool
}

func (l *cuttableLeases) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	if l.cut.Load() {
		return nil, errCutOff
	}
	return l.pool.Acquire(ctx)
}

func (l *cuttableLeases) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	if l.cut.Load() {
		return pgconn.CommandTag{}, errCutOff
	}
	return l.pool.Exec(ctx, sql, arguments...)
}

func (l *cuttableLeases) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if l.cut.Load() {
		return cutOffRow{}
	}
	return l.pool.QueryRow(ctx, sql, args...)
}

// cutOffRow is the row a cut-off process reads.
type cutOffRow struct{}

func (cutOffRow) Scan(...any) error { return errCutOff }

// natsCut is a TCP relay to the test's NATS server that one process's NATS connection goes
// through. Cut closes every connection through it and closes each new one at once, as a network
// partition leaves the process: its client reconnects and fails until Restore. The relay keeps its
// port for the whole test, so the client's reconnects reach it again once restored. latency holds
// back each read from the client by that long before the server is handed it, in order, as a slow
// link to the server does. Hold stops everything the client sends at the relay, in order, until
// Release, as a link that carries nothing from the process to the server for as long as the row
// needs does, while what the server sends goes on reaching the client.
type natsCut struct {
	listener net.Listener
	target   string
	latency  time.Duration

	mu    sync.Mutex
	cut   bool
	conns []net.Conn
	// gate, while not nil, is what each read from a client waits on before the server is handed
	// it (Hold, Release).
	gate chan struct{}
}

// startNATSCut relays to natsURL's server, holding back what the client sends by latency, until
// the test ends, and returns the relay and the URL a client connects to it by, with natsURL's
// credentials.
func startNATSCut(t *testing.T, natsURL string, latency time.Duration) (*natsCut, string) {
	t.Helper()
	parsed, err := url.Parse(natsURL)
	if err != nil {
		t.Fatalf("parse the NATS URL: %v", err)
	}
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen for the NATS relay: %v", err)
	}
	relay := &natsCut{listener: listener, target: parsed.Host, latency: latency}
	t.Cleanup(func() {
		_ = listener.Close()
		relay.Cut()
	})
	go relay.serve()
	parsed.Host = listener.Addr().String()
	return relay, parsed.String()
}

func (r *natsCut) serve() {
	for {
		client, err := r.listener.Accept()
		if err != nil {
			return
		}
		r.mu.Lock()
		if r.cut {
			r.mu.Unlock()
			_ = client.Close()
			continue
		}
		server, err := net.Dial("tcp", r.target)
		if err != nil {
			r.mu.Unlock()
			_ = client.Close()
			continue
		}
		r.conns = append(r.conns, client, server)
		r.mu.Unlock()
		go func() { r.toServer(server, client); _ = server.Close() }()
		go func() { _, _ = io.Copy(client, server); _ = client.Close() }()
	}
}

// toServer copies what the client sends to the server, each read held back by the gate while the
// relay holds (Hold), and by the relay's latency, on the clock the shared waits read
// (testwait.Pause): the latency is the stand-in's own argument, the slow link a row asks for.
func (r *natsCut) toServer(server, client net.Conn) {
	buffer := make([]byte, 32*1024)
	for {
		n, err := client.Read(buffer)
		if n > 0 {
			r.mu.Lock()
			gate := r.gate
			r.mu.Unlock()
			if gate != nil {
				<-gate
			}
			if r.latency > 0 {
				testwait.Pause(r.latency)
			}
			if _, err := server.Write(buffer[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Hold stops what every client sends at the relay until Release.
func (r *natsCut) Hold() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gate == nil {
		r.gate = make(chan struct{})
	}
}

// Release hands the server what the relay held, in order, and relays freely again.
func (r *natsCut) Release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gate != nil {
		close(r.gate)
		r.gate = nil
	}
}

// Cut closes every connection through the relay and every new one until Restore, and lets go what
// Hold held, which has nowhere to go.
func (r *natsCut) Cut() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cut = true
	for _, conn := range r.conns {
		_ = conn.Close()
	}
	r.conns = nil
	if r.gate != nil {
		close(r.gate)
		r.gate = nil
	}
}

// Restore relays new connections again.
func (r *natsCut) Restore() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cut = false
}

// intakeProcess is one envoy-dispatch process's delivery intake: its NATS client, its lease pool,
// and the GitHub it fetches each merged pull request from, which counts the fetches by number.
type intakeProcess struct {
	client *bus.Client
	intake *Intake
	leases *cuttableLeases
	stop   func()
	// release lets every fetch its GitHub holds answer.
	release func()

	mu      sync.Mutex
	fetched map[int]int
}

// fetches is how many times this process fetched pull request number.
func (p *intakeProcess) fetches(number int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fetched[number]
}

// keyedRunnerStopped reports whether this process's intake has stopped the keyed runner of the
// delivery it is subscribed with.
func (p *intakeProcess) keyedRunnerStopped() bool { return keyedRunnerStopped(p.intake) }

// subscribedDelivery is the subscription intake delivers with, and nil before its first one. A
// stop leaves it there: the callback of a stopped subscription is still the one NATS hands the
// messages the client had buffered while it drains.
func subscribedDelivery(intake *Intake) *intakeDelivery {
	intake.mu.Lock()
	defer intake.mu.Unlock()
	return intake.delivering
}

// keyedRunnerStopped reports whether intake has stopped the keyed runner of the delivery it is
// subscribed with. That is the point of its stop at which the messages queued behind a handler
// still running have been handed back and the runner takes no further one: a handler that
// returns after it starts nothing else.
func keyedRunnerStopped(intake *Intake) bool {
	delivering := subscribedDelivery(intake)
	if delivering == nil {
		return false
	}
	delivering.keys.mu.Lock()
	defer delivering.keys.mu.Unlock()
	return delivering.keys.stopped
}

// heldListReleased reports whether this process's intake has released the held list of the
// delivery it is subscribed with.
func (p *intakeProcess) heldListReleased() bool { return heldListReleased(p.intake) }

// heldListReleased reports whether the stop of intake's delivery has handed back the messages it
// held -- those that were waiting behind their keys -- and so hands back at once, rather than
// holding, every message its callback is handed from then on (intakeDelivery.giveBack).
func heldListReleased(intake *Intake) bool {
	delivering := subscribedDelivery(intake)
	if delivering == nil {
		return false
	}
	delivering.mu.Lock()
	defer delivering.mu.Unlock()
	return delivering.released
}

// holdLapsed reports whether the hold the delivery this process's intake is subscribed with
// delivers under has passed its loss deadline, the point from which every hand-back of that
// delivery is withheld rather than published (intakeDelivery.handBack).
func (p *intakeProcess) holdLapsed() bool {
	delivering := subscribedDelivery(p.intake)
	return delivering != nil && !delivering.holdCovers()
}

// holdDeadline is that loss deadline, and the zero time while this process's intake is
// subscribed with no delivery or its delivery under no hold.
func (p *intakeProcess) holdDeadline() time.Time {
	delivering := subscribedDelivery(p.intake)
	if delivering == nil || delivering.hold == nil {
		return time.Time{}
	}
	return delivering.hold.Deadline()
}

// startIntakeProcess runs a delivery intake as a process of its own on pool's database (a pool of
// its own, storetest.SecondProcess) and natsURL's server, until the test ends or its stop is
// called. Its GitHub answers every pull request 404 after counting it, which the intake logs and
// acknowledges, and holds the fetch of each number block reports true until the handler's context
// ends or the test's end. Its client holds its calls to the test's give-up (newHoldingClient), so a
// row stages its next step while the fetch is held. options are added to its NATS client's own.
func startIntakeProcess(t *testing.T, pool *store.Pool, natsURL string, block func(number int) bool, options ...bus.ConnectOption) *intakeProcess {
	t.Helper()
	return startIntakeProcessWith(t, pool, natsURL, block, nil, options...)
}

// startIntakeProcessWith is startIntakeProcess with before run on the process's intake once it is
// built and before it runs, for a row that gives the intake a clock of its own (heldDurable.clock,
// heldDurable.holdClock) or tries for the hold it reads (holdTries.on). A nil before does nothing.
// The process's stop is runIntake's, which waits for Run through the shared wait.
func startIntakeProcessWith(t *testing.T, pool *store.Pool, natsURL string, block func(number int) bool, before func(*Intake), options ...bus.ConnectOption) *intakeProcess {
	t.Helper()
	second := storetest.SecondProcess(t, &store.Store{Pool: pool})
	leasePool := storetest.Leases(t, second.Pool)
	client, err := bus.Connect([]string{natsURL}, append([]bus.ConnectOption{bus.WithName(intakeConnectionName)}, options...)...)
	if err != nil {
		t.Fatalf("connect the process's intake: %v", err)
	}
	t.Cleanup(func() { bustest.Close(t, client) })
	process := &intakeProcess{client: client, leases: &cuttableLeases{pool: leasePool}, fetched: map[int]int{}}

	held := make(chan struct{})
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		number, err := strconv.Atoi(r.PathValue("number"))
		if err != nil {
			t.Errorf("parse pull request number %q: %v", r.PathValue("number"), err)
		}
		process.mu.Lock()
		process.fetched[number]++
		process.mu.Unlock()
		if block != nil && block(number) {
			select {
			case <-held:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(http.StatusNotFound)
	})
	github := fake.newHoldingClient()
	// Registered after the fake's server, so it runs first: the server's Close waits for the
	// handler it holds.
	process.release = sync.OnceFunc(func() { close(held) })
	t.Cleanup(process.release)

	intake := newIntake(tuningFor(t), client, second.Pool, process.leases, github, events.NewBroker(), loadedSettingsCache(t, t.Context(), second.Pool), unrenamed())
	process.intake = intake
	if before != nil {
		before(intake)
	}
	process.stop = runIntake(t, intake)
	return process
}

// holdTries is the hold clock of one process's intake (heldDurable.holdClock), a clocktest fake
// only the row moves, and the outcome of each of that intake's tries for the hold and the durable
// under it, in order. The intake's run loop tries once as it starts and then at each
// durableBindInterval tick of that clock (heldDurable.run), and its tuning's attempted hook, which
// on sets, adds each try as it ends, with whether the intake delivers from the durable then
// (intakeDelivers). The hook is the tuning's, which every intake a test builds shares (tuningFor),
// so a row starts the intake whose tries it reads once no other intake of the row's tries any more:
// a run loop tries only while it delivers nothing.
type holdTries struct {
	clock *clocktest.Fake
	ended testwait.Stream[bool]
}

// newHoldTries is a holdTries whose clock stands at fakeIntakeClock's moment until the row moves it.
func newHoldTries() *holdTries { return &holdTries{clock: fakeIntakeClock()} }

// on gives intake, before its Run, the fake as its hold clock, and has its tuning's attempted hook
// add each of its tries: a before of startIntakeProcessWith or startHeadWriteProcess, or a call on
// an intake a row builds itself (newIntake).
func (h *holdTries) on(intake *Intake) {
	intake.holdClock = h.clock
	tried := func() { h.ended.Add(intakeDelivers(intake)) }
	intake.tuning.attempted.Store(&tried)
}

// firstTryEnded has the attempted hook of intake's tuning close the channel it returns once intake's
// first try for the hold has ended (heldDurable.run), for a holder a row starts before the standby
// whose tries it reads (holdTries). The hook is the tuning every intake of the test shares, and a
// holder's first try that takes the durable over runs on after the server reports it bound -- its
// token's write and the consumer lease's release -- so a standby's hook set before that try has
// ended would count it as the standby's. A row waits for the channel before it starts the standby,
// and from then on the holder, which delivers, tries for nothing until it loses its hold.
func firstTryEnded(intake *Intake) <-chan struct{} {
	ended := make(chan struct{})
	var once sync.Once
	tried := func() { once.Do(func() { close(ended) }) }
	intake.tuning.attempted.Store(&tried)
	return ended
}

// first waits for the try the intake makes as its run starts, before the row has moved its clock,
// and reports whether that try left it delivering.
func (h *holdTries) first(t *testing.T, what string) bool {
	t.Helper()
	return testwait.Next(t, what, &h.ended, func(bool) bool { return true })
}

// next moves the clock one durableBindInterval to the intake's next try, waits for that try to end,
// and reports whether it left the intake delivering. The loop arms each tick only once its try
// before it has ended (clock.Ticker), and a tick the move passed before it was armed comes at once,
// so each move is one try.
func (h *holdTries) next(t *testing.T, what string) bool {
	t.Helper()
	h.clock.Advance(durableBindInterval)
	return testwait.Next(t, what, &h.ended, func(bool) bool { return true })
}

// untilDelivering moves the intake to each next try in turn, as next does, until one leaves it
// delivering, through the shared wait, whose give-up names what and how many tries stood by.
func (h *holdTries) untilDelivering(t *testing.T, what string) {
	t.Helper()
	tries := 0
	testwait.Until(t, what, func() (string, bool) {
		tries++
		delivers := h.next(t, fmt.Sprintf("%s: try %d", what, tries))
		return fmt.Sprintf("%d tries left the intake standing by", tries), delivers
	})
}

// standBy waits for the intake's try as its run starts and moves it through the n tries after it,
// as next does, and fails the test, naming what, at the first of them that left the intake
// delivering: each try a standby makes reads the hold and the durable afresh, so the n+1 tries are
// the polls a row counts in place of a span of them.
func (h *holdTries) standBy(t *testing.T, what string, n int) {
	t.Helper()
	if h.first(t, what+": the try at its start") {
		t.Fatalf("%s: the intake took the durable over at its try as it started", what)
	}
	for try := 1; try <= n; try++ {
		if h.next(t, fmt.Sprintf("%s: try %d after its start", what, try)) {
			t.Fatalf("%s: the intake took the durable over at its try %d after its start", what, try)
		}
	}
}

// intakeDelivers reports whether intake delivers from its durable: its latest subscription, read
// under its lock (heldDurable.mu), is there and not stopped. A take-over that bound the durable
// leaves it so; one whose create or token write failed stops it (stopDeliveringWithin).
func intakeDelivers(intake *Intake) bool {
	delivering := subscribedDelivery(intake)
	return delivering != nil && !delivering.stopping.Load()
}

// moveTicks waits through the shared wait until done reports true, and on each reading that finds
// it false moves c, a clocktest fake only the row moves, every forward when a timer is armed there,
// and not before (clocktest.Fake.SetWhen). A loop on a clock.Ticker arms its next tick only once its
// work for the one before is done, and a call's bound is armed as the call begins, so each move fires
// what the code has armed one step on and passes nothing it has not reached. failure is the wait's
// text should it give up.
func moveTicks(t *testing.T, c *clocktest.Fake, every time.Duration, done func() bool, failure func() string) {
	t.Helper()
	waitFor(t, func() bool {
		if done() {
			return true
		}
		c.SetWhen(func(now time.Time, armed []time.Time) (time.Time, bool) {
			next := now.Add(every)
			return next, slices.ContainsFunc(armed, next.Equal)
		})
		return false
	}, failure)
}

// loseHold moves holdClock, intake's hold clock (heldDurable.holdClock), a fake only the row moves,
// to the loss deadline of the hold intake delivers under, once the hold has armed it there, and
// fails the test unless the hold is lost at that moment. The row has cut the lease pool the hold is
// renewed on first (cuttableLeases), so no renewal the move brings lands and moves the deadline on
// (duty.Hold), as for a holder the database no longer answers; the run loop's tries and checks the
// move passes run as they do on the way to a loss. It returns the deadline.
func loseHold(t *testing.T, holdClock *clocktest.Fake, intake *Intake) time.Time {
	t.Helper()
	delivering := subscribedDelivery(intake)
	if delivering == nil || delivering.hold == nil {
		t.Fatal("control: the holder delivers under no hold")
	}
	deadline := delivering.hold.Deadline()
	holdClock.AwaitArmedAt(t, "the holder's loss deadline, armed as its hold was taken or last renewed", deadline)
	holdClock.Set(deadline)
	if !closedChannel(delivering.hold.Lost()) {
		t.Fatalf("control: the holder's hold was not lost with its clock at the loss deadline %s", deadline)
	}
	return deadline
}

// passLossMargin moves holdClock, the hold clock of intake, whose hold loseHold lost at deadline,
// past the budget the lost hold gives the handlers still running before it gives them up
// (heldDurable.run, stopDeliveringWithin: intakeHoldMargin on that clock), once the stop has begun
// (keyedRunnerStopped), which comes after the budget is armed. A row whose handler holds its message
// through the loss moves it there, so the stop gives that handler up as it does a margin past the
// loss.
func passLossMargin(t *testing.T, holdClock *clocktest.Fake, intake *Intake, deadline time.Time) {
	t.Helper()
	waitFor(t, func() bool { return keyedRunnerStopped(intake) }, func() string {
		return "the holder never began to stop delivering at its lost hold"
	})
	end := deadline.Add(intakeHoldMargin)
	holdClock.AwaitArmedAt(t, "the budget the lost hold's stop gives the handlers still running (stopDeliveringWithin)", end)
	holdClock.Set(end)
}

// lapseHoldRow lapses the delivery durable's hold row, which names holder, on the database's clock,
// as that clock passing intakeHoldTTL after the holder's last renewal does: the test's own statement,
// on the test's own pool, sets the row's expiry to the database's now(), keeps its holder and token,
// and notifies no release, so a standby's next try takes the hold (duty.TryHold) and reads the token
// the row records (duty.HoldToken). It stands in for waiting the TTL out, which no clock of the test
// moves (dispatch://INFRA-3149/comment/cc01d314-cbbc-4990-b232-cb40ac523f86, item 6). A row lapses
// the row only once it has cut the holder's renewals and moved its hold clock to its loss deadline
// (loseHold), the order the hold's margin keeps in production (intakeHoldMargin).
func lapseHoldRow(t *testing.T, pool *store.Pool, holder uuid.UUID) {
	t.Helper()
	tag, err := pool.Exec(t.Context(), `update duty_holds set expires_at = now() where name = $1 and holder = $2`,
		intakeHoldName(bus.DispatchDeliveryDurable), holder)
	if err != nil {
		t.Fatalf("lapse the holder's hold row on the database's clock: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("control: the delivery durable's hold row does not name the holder %s, so the row has no hold of its to lapse", holder)
	}
}

// keepHoldRowLive keeps the delivery durable's hold row, which names holder, live on the database's
// clock past anything the run waits for, as the holder's renewals would: the test's own statement,
// on the test's own pool, sets the row's expiry a day past the database's now(). A holder whose hold
// runs on a fake hold clock the row does not move through a renewal renews nothing (duty.Hold), so
// its row would lapse intakeHoldTTL after the take while the row still stages its steps, and a
// standby's try, or a reading of the hold (holdLive), would then find it lapsed however the code
// behaved: a span of the wall clock the row would have to beat. The holder's own loss deadline stays
// on its fake (loseHold); the row lapses the row itself (lapseHoldRow), or the holder releases it.
func keepHoldRowLive(t *testing.T, pool *store.Pool, holder uuid.UUID) {
	t.Helper()
	tag, err := pool.Exec(t.Context(), `update duty_holds set expires_at = now() + interval '1 day' where name = $1 and holder = $2`,
		intakeHoldName(bus.DispatchDeliveryDurable), holder)
	if err != nil {
		t.Fatalf("keep the holder's hold row live on the database's clock as its renewals would: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("control: the delivery durable's hold row does not name the holder %s, so the row has no hold of its to keep live", holder)
	}
}

// publishMergedPullRequest publishes the merge of acme/widgets pull request number by a population
// author, which the intake fetches from GitHub, and answers the stream sequence the stream stored
// it at.
func publishMergedPullRequest(t *testing.T, publisher *bus.Client, number int) uint64 {
	t.Helper()
	return publishPullRequestEnvelope(t, publisher, "acme", "widgets", number, map[string]string{
		"kind": "pr", "action": "closed", "merged": "true", "repo": "acme/widgets",
		"number": strconv.Itoa(number), "author": "octocat",
	})
}

// consumerState is the delivery durable's state, one line, for a failure message.
func consumerState(publisher *bus.Client) string {
	info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("push_bound=%t deliver_subject=%s delivered=%d ack_floor=%d ack_pending=%d pending=%d redelivered=%d",
		info.PushBound, info.Config.DeliverSubject, info.Delivered.Stream, info.AckFloor.Stream, info.NumAckPending, info.NumPending, info.NumRedelivered)
}

// awaitAckPending waits for the delivery durable to have want messages outstanding with its
// subscriber: delivered to it, and not yet acknowledged.
func awaitAckPending(t *testing.T, publisher *bus.Client, want int) {
	t.Helper()
	waitFor(t, func() bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && info.NumAckPending == want
	}, func() string {
		return fmt.Sprintf("the durable does not have %d messages outstanding: %s", want, consumerState(publisher))
	})
}

// awaitEverythingAcknowledged waits for the delivery durable to have nothing left to hand anybody:
// no message outstanding with its subscriber, none waiting to be delivered, and its ack floor
// level with what it has delivered (consumer sequences, which a take-over's recreate starts afresh
// from, rather than stream sequences). Past that point no redelivery is still on its way, so what
// each process has handled no longer changes.
func awaitEverythingAcknowledged(t *testing.T, publisher *bus.Client) {
	t.Helper()
	waitFor(t, func() bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && info.NumAckPending == 0 && info.NumPending == 0 && info.AckFloor.Consumer == info.Delivered.Consumer
	}, func() string {
		return "the durable still has messages to hand out or to be acknowledged: " + consumerState(publisher)
	})
}

// A holder stopped gracefully, as a rolling release stops the old process, hands the messages it
// had been delivered and not yet handled back at once, and lets the standby take over at once: the
// standby takes the durable over at its first try after the holder's stop has released its hold,
// and handles every one of the messages then, not after the ack wait. The messages it holds are the
// ones waiting behind their key (intakeKey): one pull request's messages run one after another, so
// four more of the one its handler is holding wait behind it.
//
// The holder's own deadlines are on a fake it never moves (heldDurable.clock), so neither its
// handler's deadline nor its stop's ends the handler the row holds: the handler ends at the row's
// release. The standby tries for the hold on a fake hold clock the row moves one durableBindInterval
// at a time (holdTries), so "at once" is its first try after the release, counted rather than timed.
// Both intakes' ack wait is past the run (ackWaitPastTheRun), so the server's own redelivery never
// brings the messages: only the hand-over can.
func TestAGracefulStopHandsTheBufferedMessagesToTheStandby(t *testing.T) {
	t.Parallel()
	ackWaitPastTheRun(t)
	// One message in the holder's handler and four behind it on its key.
	tuningFor(t).maxAckPending = 5
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	const inFlight = 100
	const queued = 4
	var holderTried <-chan struct{}
	holder := startIntakeProcessWith(t, pool, natsURL, func(number int) bool { return number == inFlight }, func(intake *Intake) {
		intake.clock = fakeIntakeClock()
		holderTried = firstTryEnded(intake)
	})
	awaitBoundDurable(t, publisher)
	testwait.Receive(t, "the holder's first try for the hold to end", holderTried)
	tries := newHoldTries()
	standby := startIntakeProcessWith(t, pool, natsURL, nil, tries.on)
	tries.standBy(t, "the standby, while the holder held the durable", 0)

	publishMergedPullRequest(t, publisher, inFlight)
	waitFor(t, func() bool { return holder.fetches(inFlight) == 1 }, func() string {
		return "the holder never started on the first message: " + consumerState(publisher)
	})
	for range queued {
		publishMergedPullRequest(t, publisher, inFlight)
	}
	awaitAckPending(t, publisher, 1+queued)

	stopped := testwait.Now()
	holderStopped := make(chan struct{})
	go func() {
		defer close(holderStopped)
		holder.stop()
	}()
	// The stop hands the four queued messages back as it stops the keyed runner, and only then
	// waits for the handler holding the fifth. Released before that, the handler would return
	// while the runner still ran, and the runner would hand it the next queued message of the
	// same key rather than leaving it to the standby. The wait for the handler cannot return
	// while that fetch is held, so waiting for the runner here cannot deadlock.
	waitFor(t, holder.keyedRunnerStopped, func() string {
		return "the stopping holder has not stopped its keyed runner: " + consumerState(publisher)
	})
	// The message in the holder's handler finishes as the stop waits for it, and the stop then
	// releases the hold, once its hand-backs have reached the server, before Run returns.
	holder.release()
	testwait.Receive(t, "the holder's stop to return once its handler finished", holderStopped)
	// The stop's drain took the holder's subscription off the durable; the server reports it once it
	// has handled the unsubscribe.
	waitFor(t, func() bool { return !deliveryDurableBound(publisher) }, func() string {
		return "the stopped holder's subscription is still on the durable: " + consumerState(publisher)
	})
	if !tries.next(t, "the standby's first try after the holder's stop released its hold") {
		t.Fatalf("the standby's first try after the holder's stop released its hold did not take the durable over: %s", consumerState(publisher))
	}
	waitFor(t, func() bool { return standby.fetches(inFlight) == queued }, func() string {
		return fmt.Sprintf("the standby has not handled the stopped holder's %d queued messages of pull request %d: standby %d, holder %d; %s",
			queued, inFlight, standby.fetches(inFlight), holder.fetches(inFlight), consumerState(publisher))
	})
	t.Logf("the standby handled the queued messages %s after the holder's stop", testwait.Now().Sub(stopped))
	if got := holder.fetches(inFlight); got != 1 {
		t.Errorf("the stopping holder fetched pull request %d %d times; want the one its handler held, the rest handed back", inFlight, got)
	}
	// Nothing is left with the durable: no message outstanding with a subscriber, none waiting to
	// be delivered, and its ack floor level with what it has delivered. Only then are the counts
	// final. A message a process handed back rather than acknowledged -- the one the holder's
	// handler held among them -- has by then been redelivered and handled, so it is in the count
	// below rather than still on its way while the counts are read.
	awaitEverythingAcknowledged(t, publisher)
	if got := standby.fetches(inFlight); got != queued {
		t.Errorf("with nothing left with the durable, the standby fetched pull request %d %d times; want the %d queued messages and no more: standby %d, holder %d; %s",
			inFlight, got, queued, got, holder.fetches(inFlight), consumerState(publisher))
	}
	if got := holder.fetches(inFlight); got != 1 {
		t.Errorf("with nothing left with the durable, the stopping holder fetched pull request %d %d times; want the one its handler held and no more: standby %d, holder %d; %s",
			inFlight, got, standby.fetches(inFlight), got, consumerState(publisher))
	}
}

// withheldHandBack is the line intakeDelivery.handBack logs for a message it leaves with the
// durable rather than handing back, because the hold no longer covers the delivery.
const withheldHandBack = "dispatch delivery: the hold no longer covers this delivery, so the message is left with the durable for the next holder's take-over"

// A holder whose stop runs on past its hold's loss deadline publishes no hand-back at all, and
// the standby that takes the durable over handles each of its messages once.
//
// Every hand-back is a publish at the message's reply subject, which names the durable and not
// the consumer the message was fetched on. A take-over deletes the durable and creates it again
// under the same name at its ack floor (heldDurable.takeOver), so a hand-back published after
// the lapse lands on the new holder's consumer, moves that message's pending entry and has the
// new holder delivered it a second time. Three hand-backs of a stop can take that path: the
// messages it held, which are the ones that were waiting behind their key, the message of the
// handler it gave up (Intake.deliver), and every message its callback is handed once it has
// released that held list (intakeDelivery.giveBack). All three are withheld once the hold has
// lapsed (intakeDelivery.handBack), and nothing is lost: no message was acknowledged, so the
// take-over's recreate at the ack floor hands every one of them to the new holder.
//
// This row drives the first two, the ones a stop that gives a handler up makes. The third
// belongs to a stop that still has budget left, and so to a row of its own
// (TestAMessagePushedToADrainingHolderPastItsHoldsLapseIsLeftToTheStandby): the moment a stop
// gives its handlers up it unsubscribes the subscription it was draining, in the same breath as
// it releases the held list (intakeDelivery.stop), so nothing the server sends afterwards is
// handed to the callback here, whatever the link between them does.
//
// The row drives the sequence a frozen holder makes of that, which the multiprocess suite drives
// with a real SIGSTOP (TestAFrozenHolderIsTakenOverAndHandlesNothingOnceResumed): the holder's
// handler is held inside its GitHub fetch, its lease pool is cut so no renewal of its hold
// lands, and its stop -- which cannot publish anything until that handler returns -- therefore
// runs its hand-backs after the loss deadline, since the handler is given up only when the
// stop's own budget (intakeHoldMargin) ends, a margin past that deadline.
//
// The holder's hold runs on a fake hold clock only the row moves (heldDurable.holdClock): with the
// pool cut, the row moves it to the loss deadline (loseHold) and, once the stop has begun, past the
// budget the stop gives the held handler (passLossMargin), and only then lapses the hold's row on
// the database's clock (lapseHoldRow), as the margin orders the two in production; until then the
// row keeps the hold's row live as the holder's renewals would (keepHoldRowLive). The standby
// tries for the hold on a fake of its own, which the row moves to each of its tries until it takes
// the durable over (awaitDurableTakenOver). No step waits on a time.
//
// What it measures is the answers published to the durable, read off the ack subject itself
// rather than from the consumer's counts, as the head-write row does
// (TestAHolderWhoseHoldLapsedPublishesNoHandBackAndTheKeeperIsHandedTheEnvelopeOnce): a
// hand-back is a publish, and whether the server still has a consumer to act on it depends on
// how far the take-over has got. None of them may be a hand-back, and the standby's own
// acknowledgements are there to be read beside them, so a recorder that saw nothing at all
// cannot pass the row by silence.
func TestAStopPastTheHoldsLapsePublishesNoHandBackAndTheStandbyHandlesEachMessageOnce(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	answers := durableAnswers(t, publisher)
	// The standby must be handed the lapsed holder's messages by its take-over's recreate at the
	// ack floor, so the server's own redelivery at the ack wait never comes inside the run
	// (ackWaitPastTheRun).
	ackWaitPastTheRun(t)

	// One message in the holder's handler and the rest waiting behind it on its key, which is
	// what fills the durable's in-flight slots (the intakes' maxAckPending). The handler is held
	// for the whole row: the intakes' messageTimeout is past the run, so what ends that fetch is
	// the stop giving the handler up and not a deadline of the handler's own.
	const inFlight = 300
	queued := tuningFor(t).maxAckPending - 1
	holdClock := fakeIntakeClock()
	var holderTried <-chan struct{}
	holder := startIntakeProcessWith(t, pool, natsURL, func(number int) bool { return number == inFlight }, func(intake *Intake) {
		intake.holdClock = holdClock
		holderTried = firstTryEnded(intake)
	})
	awaitBoundDurable(t, publisher)
	testwait.Receive(t, "the holder's first try for the hold to end", holderTried)
	keepHoldRowLive(t, pool, holder.intake.holder)
	tries := newHoldTries()
	standby := startIntakeProcessWith(t, pool, natsURL, nil, tries.on)
	tries.standBy(t, "the standby, while the holder held the durable", 0)

	publishMergedPullRequest(t, publisher, inFlight)
	waitFor(t, func() bool { return holder.fetches(inFlight) == 1 }, func() string {
		return "the holder never started on the first message: " + consumerState(publisher)
	})
	for range queued {
		publishMergedPullRequest(t, publisher, inFlight)
	}
	awaitAckPending(t, publisher, 1+queued)

	// The cut takes the holder off the lease pool its hold is renewed on, as a network that took
	// it off Postgres would, or as a freeze leaves a process that renews nothing: the hold is lost
	// at its deadline, and the holder stops delivering, giving its handler the margin.
	holder.leases.cut.Store(true)
	deadline := loseHold(t, holdClock, holder.intake)
	passLossMargin(t, holdClock, holder.intake, deadline)
	// The stop hands the messages it held back once the handler it gave up has returned, which
	// is a margin past the loss deadline, so every one of those hand-backs is withheld.
	waitFor(t, holder.heldListReleased, func() string {
		return "the holder's stop never released the messages it held: " + consumerState(publisher)
	})

	lapseHoldRow(t, pool, holder.intake.holder)
	awaitDurableTakenOver(t, holder.intake, standby.intake, publisher, tries)
	waitFor(t, func() bool { return standby.fetches(inFlight) == 1+queued }, func() string {
		return fmt.Sprintf("the standby handled %d of the lapsed holder's %d messages: holder %d; %s",
			standby.fetches(inFlight), 1+queued, holder.fetches(inFlight), consumerState(publisher))
	})
	awaitEverythingAcknowledged(t, publisher)

	// The standby's acknowledgements, one for each message, which the durable's ack floor above
	// shows the server has had, are the last answers of the row.
	published := answersOnceAcknowledged(t, answers, 1+queued)
	var handedBack []string
	for _, answer := range published {
		if strings.HasPrefix(answer, "-NAK") {
			handedBack = append(handedBack, answer)
		}
	}
	if len(handedBack) > 0 {
		t.Errorf("hand-backs published to the delivery durable = %q, want none: the holder's hold had lapsed before its stop answered, so each of them lands on the durable the standby's take-over recreated",
			handedBack)
	}
	if got := standby.fetches(inFlight); got != 1+queued {
		t.Errorf("with nothing left with the durable, the standby fetched pull request %d %d times; want the lapsed holder's %d messages, each once: holder %d; %s",
			inFlight, got, 1+queued, holder.fetches(inFlight), consumerState(publisher))
	}
	if got := holder.fetches(inFlight); got != 1 {
		t.Errorf("the holder fetched pull request %d %d times; want the one its given-up handler held: %s", inFlight, got, consumerState(publisher))
	}
}

// A message the server pushes to a holder that has stopped delivering, after that stop has
// released the messages it held and past the hold's loss deadline, earns no hand-back either,
// and the standby that takes the durable over handles it once.
//
// It is the third of the hand-backs the row above lists, the one a stopped subscription's own
// callback makes (intakeDelivery.giveBack, past the release), and it takes a stop of its own.
// A stop that gives a handler up unsubscribes the subscription it was draining in the same
// breath as it releases that held list -- the handlers' context being done is what ends its
// drain wait (intakeDelivery.stop) -- so from that moment its client is handed nothing more.
// Here no handler of the holder's is running, so its stop releases at once and goes on
// draining.
//
// What keeps the messages coming is the link the relay holds (natsCut.Hold): the stop drains
// before it does anything else, so its unsubscribe is on its way to the server from the first
// moment, and until that lands the server goes on pushing the durable's messages to that
// subscription, which nats.go hands to the callback while the drain runs. The client side of that
// drain lasts as long: it ends the subscription once the flush it opens with is answered (nats.go's
// checkDrained), and that flush is held at the relay too. The hold is the link's order itself, the
// push reaching the draining holder before its unsubscribe reaches the server, with no latency
// standing in for it; testnats offers no hold of one direction of a link (its proxy freezes both,
// or the server's replies alone), so the row's own relay holds it.
//
// The hold's loss deadline is on a fake the row moves (heldDurable.holdClock), and the stop's
// budget and drain wait on one the row never moves (heldDurable.clock), so no real time decides
// the order: the row cuts the holder's lease pool, so no renewal of its hold lands, stops the
// process while the deadline is still ahead on its clock -- so its run loop never reaches the
// lost hold, which would stop delivering under intakeHoldMargin instead -- and moves that clock to
// the deadline once the stop has released its held list (loseHold). It publishes then: giveBack
// hands the message back at once, because the held list is released, and handBack withholds that
// hand-back, because the hold no longer covers the delivery. Only once the holder's callback has
// answered the message does the row let the link move and lapse the hold's row on the database's
// clock (lapseHoldRow), which it keeps live until then as the holder's renewals would
// (keepHoldRowLive), and the standby, which tries for the hold only where the row moves its own
// fake (holdTries), takes the durable over.
//
// A hand-back published instead would land on the durable the standby's take-over recreates
// under the same name (heldDurable.takeOver), move that message's pending entry and have the
// standby delivered it twice. What the row measures is the answers published to the durable,
// read off the ack subject itself (durableAnswers) as the row above reads them, and the line
// handBack logs for a message it withholds, so a run whose message never reached that callback
// fails rather than passing on a branch it never ran.
func TestAMessagePushedToADrainingHolderPastItsHoldsLapseIsLeftToTheStandby(t *testing.T) {
	t.Parallel()
	records := captureIntakeLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	answers := durableAnswers(t, publisher)
	// The standby must be handed the row's message by its take-over's recreate at the ack floor, so
	// the server's own redelivery at the ack wait never comes inside the run (ackWaitPastTheRun).
	ackWaitPastTheRun(t)

	// lateNumber is the row's one message, published after the holder's stop has released the
	// messages it held -- it held none, no handler of its having been running -- and after its
	// hold's loss deadline has passed.
	const lateNumber = 310
	relay, holderURL := startNATSCut(t, natsURL, 0)
	holdClock := fakeIntakeClock()
	var holderTried <-chan struct{}
	holder := startIntakeProcessWith(t, pool, holderURL, nil, func(intake *Intake) {
		intake.clock, intake.holdClock = fakeIntakeClock(), holdClock
		holderTried = firstTryEnded(intake)
	})
	awaitBoundDurable(t, publisher)
	testwait.Receive(t, "the holder's first try for the hold to end", holderTried)
	keepHoldRowLive(t, pool, holder.intake.holder)
	tries := newHoldTries()
	standby := startIntakeProcessWith(t, pool, natsURL, nil, tries.on)
	tries.standBy(t, "the standby, while the holder held the durable", 0)

	// The lease pool is cut, so no renewal of the holder's hold lands, and what its client sends
	// the server is held at the relay, so its stop drains for as long as the row holds the link. The
	// stop's drain is a call to NATS it waits for at most the intakes' callWait, and one it went on
	// without would end its drain at once and release the hold: the stop's calls wait for NATS with
	// no bound of the row's.
	holder.leases.cut.Store(true)
	tuningFor(t).callWait.Store(math.MaxInt64)
	relay.Hold()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		holder.stop()
	}()
	waitFor(t, holder.heldListReleased, func() string {
		return "the holder's stop never released its held list: " + consumerState(publisher)
	})
	loseHold(t, holdClock, holder.intake)
	if !holder.holdLapsed() {
		t.Fatalf("control: the holder's hold still covers its delivery with its clock at the loss deadline %s", holder.holdDeadline())
	}
	publishMergedPullRequest(t, publisher, lateNumber)

	// The message reaches the callback of the subscription the stop is draining, which answers it
	// with one line naming its subject (handBack) or with a hand-back it publishes (handedBack).
	lateSubject := contracts.GithubResourceSubject("acme", "widgets", "pr", strconv.Itoa(lateNumber))
	withheldFor := func() int {
		withheld := 0
		for _, record := range records() {
			if record["msg"] == withheldHandBack && record["subject"] == lateSubject {
				withheld++
			}
		}
		return withheld
	}
	waitFor(t, func() bool {
		if subscribedDelivery(holder.intake).handedBack() {
			return true
		}
		for _, record := range records() {
			if record["subject"] == lateSubject {
				return true
			}
		}
		return false
	}, func() string {
		return fmt.Sprintf("the lapsed holder's draining subscription has not answered pull request %d: %s", lateNumber, consumerState(publisher))
	})
	relay.Release()
	testwait.Receive(t, "the holder's stop to return once its link moved", stopped)
	lapseHoldRow(t, pool, holder.intake.holder)
	tries.untilDelivering(t, "the standby to take the durable over once the stopped holder's hold row lapsed")
	waitFor(t, func() bool { return standby.fetches(lateNumber) == 1 }, func() string {
		return fmt.Sprintf("the standby handled the message published to the stopped holder %d times, want it once: holder %d; %s",
			standby.fetches(lateNumber), holder.fetches(lateNumber), consumerState(publisher))
	})

	// Everything the holder's client wrote is with the server once a flush of its own is
	// answered: nats.go writes one connection's publishes in the order they were made and the
	// relay hands on what it reads in that same order, so a hand-back published for that
	// message is there to be read below rather than still crossing the link.
	if err := holder.client.Conn.FlushWithContext(testnats.Context(t)); err != nil {
		t.Fatalf("wait for what the stopped holder's client sent to reach the server: %v", err)
	}
	awaitEverythingAcknowledged(t, publisher)

	// The message reached the callback of the subscription the stop was draining: the holder
	// logged the hand-back it withheld for it. Without that line the row says nothing about the
	// branch it is here for, whatever the counts below read.
	withheld := withheldFor()
	if withheld == 0 {
		t.Errorf("the lapsed holder withheld no hand-back for pull request %d, so that message never reached the callback of the subscription its stop was draining and the row measured nothing of that branch: %s",
			lateNumber, consumerState(publisher))
	}
	t.Logf("the lapsed holder withheld %d hand-back(s) for pull request %d", withheld, lateNumber)

	// The standby's one acknowledgement, which the durable's ack floor above shows the server has
	// had, is the last answer of the row.
	published := answersOnceAcknowledged(t, answers, 1)
	var handedBack []string
	for _, answer := range published {
		if strings.HasPrefix(answer, "-NAK") {
			handedBack = append(handedBack, answer)
		}
	}
	if len(handedBack) > 0 {
		t.Errorf("hand-backs published to the delivery durable = %q, want none: the holder's hold had lapsed before its callback was handed that message, so the hand-back lands on the durable the standby's take-over recreated",
			handedBack)
	}
	if got := holder.fetches(lateNumber); got != 0 {
		t.Errorf("the lapsed holder fetched pull request %d %d times; want none: its callback was handed that message after its stop had released the messages it held, and it may no longer act on the durable's messages: %s",
			lateNumber, got, consumerState(publisher))
	}
	if got := standby.fetches(lateNumber); got != 1 {
		t.Errorf("the standby fetched pull request %d %d times; want it once, from the durable its take-over recreated: holder %d; %s",
			lateNumber, got, holder.fetches(lateNumber), consumerState(publisher))
	}
}

// A holder abandoned without a stop, as a SIGKILL leaves it -- its NATS connection gone and its hold
// no longer renewed -- strands the messages it had been delivered. The standby takes the durable
// over once the hold lapses and is redelivered every one of them at once: within the hold's TTL
// and one bind interval, not after the ack wait.
//
// No real time decides it. The hold's liveness is its row on the database's clock (duty.Hold),
// which no clock of the test reaches. The row reads that the take stored a lapse no more than
// intakeHoldTTL past the database's now(), and a loss deadline intakeHoldTTL less intakeHoldMargin
// past the take on the holder's hold clock (heldDurable.holdClock), a fake only the row moves, which
// renews nothing; it then keeps the row live as the renewals would (keepHoldRowLive), so no span of
// the wall clock ends it early. Once the holder is abandoned the row moves that clock to the loss
// deadline (loseHold), and then lapses the row as the database's clock passing the TTL does
// (lapseHoldRow). The standby tries for the hold
// on a fake of its own, one durableBindInterval at a time where the row moves it (holdTries): its
// try while the abandoned hold's row is live stands by, and its first try after the row lapses
// takes the durable over, which is the bound the row holds -- the TTL from the last renewal, then
// one try. Both intakes' ack wait is past the run (ackWaitPastTheRun), so the server's own
// redelivery never brings the stranded messages: only the take-over's recreate at the ack floor
// does.
func TestAnAbandonedHoldersMessagesReachTheStandbyWithinTheHoldsLapse(t *testing.T) {
	t.Parallel()
	ackWaitPastTheRun(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	holdClock := fakeIntakeClock()
	take := holdClock.Now()
	var holderTried <-chan struct{}
	holder := startIntakeProcessWith(t, pool, natsURL, func(int) bool { return true }, func(intake *Intake) {
		intake.holdClock = holdClock
		holderTried = firstTryEnded(intake)
	})
	// The holder's fetches hold its handlers until the test lets them go, which it does as it ends,
	// before the holder's stop (registered after it, so run first): the stop of its lost hold waits
	// for them, under a budget on the hold clock the row does not move past.
	t.Cleanup(holder.release)
	awaitBoundDurable(t, publisher)
	testwait.Receive(t, "the holder's first try for the hold to end", holderTried)
	// The lapse the row brings about below is the one the take stored: on the holder's fake, which
	// has not moved, the hold's loss deadline is the take's moment plus intakeHoldTTL less
	// intakeHoldMargin, and the row the take wrote lapses no later than intakeHoldTTL past the
	// database's now().
	if got, want := holder.holdDeadline(), take.Add(intakeHoldTTL-intakeHoldMargin); !got.Equal(want) {
		t.Fatalf("the holder's hold has its loss deadline at %s, want %s: the take's moment plus intakeHoldTTL (%s) less intakeHoldMargin (%s)",
			got, want, intakeHoldTTL, intakeHoldMargin)
	}
	holdName := intakeHoldName(bus.DispatchDeliveryDurable)
	var withinTTL bool
	if err := pool.QueryRow(ctx, `select expires_at <= now() + make_interval(secs => $2) from duty_holds where name = $1 and holder = $3`,
		holdName, intakeHoldTTL.Seconds(), holder.intake.holder).Scan(&withinTTL); err != nil {
		t.Fatalf("read the lapse the holder's take stored: %v", err)
	}
	if !withinTTL {
		t.Fatalf("the holder's take stored a lapse more than intakeHoldTTL (%s) past the database's now()", intakeHoldTTL)
	}
	keepHoldRowLive(t, pool, holder.intake.holder)
	tries := newHoldTries()
	standby := startIntakeProcessWith(t, pool, natsURL, nil, tries.on)
	tries.standBy(t, "the standby, while the holder held the durable", 0)

	stranded := []int{1, 2, 3, 4}
	for _, number := range stranded {
		publishMergedPullRequest(t, publisher, number)
	}
	awaitAckPending(t, publisher, len(stranded))

	holder.leases.cut.Store(true)
	bustest.Close(t, holder.client)
	loseHold(t, holdClock, holder.intake)

	// A try while the abandoned hold's row is still live stands by: the hold still names the holder
	// and the standby has handled nothing.
	if tries.next(t, "the standby's try while the abandoned hold's row is live") {
		t.Fatalf("the standby took the durable over while the abandoned hold's row was still live: %s", consumerState(publisher))
	}
	if !holdLive(t, pool, holder.intake.holder) {
		t.Fatalf("after the standby's try while the abandoned hold was live, the hold does not name the holder %s live",
			holder.intake.holder)
	}
	for _, number := range stranded {
		if got := standby.fetches(number); got != 0 {
			t.Fatalf("the standby handled pull request %d %d times while the abandoned hold was still live, want none: %s",
				number, got, consumerState(publisher))
		}
	}

	// The hold's row lapses, and the standby's next try takes the durable over.
	lapseHoldRow(t, pool, holder.intake.holder)
	if !tries.next(t, "the standby's first try after the abandoned hold's row lapsed") {
		t.Fatalf("the standby's first try after the abandoned hold's row lapsed, one durableBindInterval (%s) on its clock, did not take the durable over: %s",
			durableBindInterval, consumerState(publisher))
	}
	waitFor(t, func() bool {
		for _, number := range stranded {
			if standby.fetches(number) == 0 {
				return false
			}
		}
		return true
	}, func() string {
		return fmt.Sprintf("the standby took the durable over and has not handled the abandoned holder's messages %v: standby %v; %s",
			stranded, fetchesOf(standby, stranded), consumerState(publisher))
	})
}

// A holder cut off from NATS and Postgres loses its hold, and the standby takes the durable over.
// When the holder's connection comes back, nothing it subscribed to before the cut delivers to it:
// the lost hold stopped its delivery, which unsubscribes a subscription whose link is down, and the
// standby's take-over deleted the durable and created it again on another deliver subject. So the
// holder is handed nothing, and each later message is handled by the standby alone, exactly once.
//
// The holder's hold runs on a fake hold clock only the row moves (heldDurable.holdClock): with its
// link and its lease pool cut, the row moves it to the loss deadline (loseHold) and lapses the
// hold's row on the database's clock (lapseHoldRow); the standby, on a fake of its own, takes the
// durable over at the try the row moves it to (awaitDurableTakenOver); until the lapse the row keeps
// the hold's row live as the holder's renewals would (keepHoldRowLive), and from the standby's take
// on it keeps the standby's live the same way, since the row moves the standby's fake through no
// renewal either. Once the holder is back, the row moves its clock to its next try for the hold,
// which finds the standby's hold live and stands by, before it reads the holder's fence. The
// holder's client redials at once after the cut (bus.WithPromptReconnect), rather than a
// ReconnectWait later.
func TestAHolderCutOffAndRestoredHandlesNothingTheStandbyTookOver(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	relay, relayedURL := startNATSCut(t, natsURL, 0)
	holdClock := fakeIntakeClock()
	start := holdClock.Now()
	var holderTried <-chan struct{}
	holder := startIntakeProcessWith(t, pool, relayedURL, nil, func(intake *Intake) {
		intake.holdClock = holdClock
		holderTried = firstTryEnded(intake)
	}, bus.WithPromptReconnect())
	awaitBoundDurable(t, publisher)
	testwait.Receive(t, "the holder's first try for the hold to end", holderTried)
	keepHoldRowLive(t, pool, holder.intake.holder)
	tries := newHoldTries()
	standby := startIntakeProcessWith(t, pool, natsURL, nil, tries.on)
	tries.standBy(t, "the standby, while the holder held the durable", 0)

	// Each wait below reads what only the cut can bring about. The cut closes both of the relay's
	// sockets at once, but each end acts on that only once its own read loop runs: until the
	// server's has, the durable still reads push-bound to the holder's subscription, and until the
	// holder client's has, the client still reads connected. Under load both took milliseconds, and
	// waits on those two states passed before either end had seen the cut, so the row flushed a
	// connection nats.go then dropped (INFRA-3904). The holder's client reads disconnected before its
	// hold is lost, so the checks of the durable its clock's move brings fail at once.
	holderConn := holder.client.Connection()
	reconnectsBeforeCut := holderConn.Stats().Reconnects
	relay.Cut()
	holder.leases.cut.Store(true)
	waitFor(t, func() bool { return !holder.client.Connected() }, func() string {
		return "the holder's client still reads connected after its link was cut"
	})
	deadline := loseHold(t, holdClock, holder.intake)
	lapseHoldRow(t, pool, holder.intake.holder)
	// The standby's take-over under way: it has deleted the durable the holder was bound to and
	// subscribed to a fresh deliver subject, which it can do only once the holder's hold has lapsed.
	awaitDurableTakenOver(t, holder.intake, standby.intake, publisher, tries)
	// The standby's hold renews only where the row moves its fake, which it does not do again: its
	// row would lapse intakeHoldTTL after the take on the database's clock, and the holder's try
	// below would then take it however the code behaved.
	keepHoldRowLive(t, pool, standby.intake.holder)

	relay.Restore()
	holder.leases.cut.Store(false)
	// The holder's connection on a session nats.go began after the cut: a reconnect count past the
	// one read before the cut, and then the same connection connected. The count is read first:
	// nats.go counts a reconnect as soon as it has a socket, before the handshake that can still
	// fail, so only a status read after the count says the session it counted is up. A flush on the
	// session the cut ended is answered "nats: connection closed" once nats.go acts on the cut,
	// since it drops every flush still waiting when it starts to reconnect.
	waitFor(t, func() bool {
		reconnected := holderConn.Stats().Reconnects > reconnectsBeforeCut
		return reconnected && holder.client.Connection() == holderConn && holderConn.Status() == natsgo.CONNECTED
	}, func() string {
		return fmt.Sprintf("the holder's NATS connection did not come back after the cut: nats.go counts %d reconnect(s) since the cut, the client still holds the connection it had = %t, and its status is %v",
			holderConn.Stats().Reconnects-reconnectsBeforeCut, holder.client.Connection() == holderConn, holderConn.Status())
	})
	// The holder's client sends again, as it reconnects, every subscription it still holds, before
	// anything else it writes; a round trip on that connection (a flush) returns only once the
	// server has read them, so whatever the holder could still be handed is in place before the
	// later messages are published.
	if err := holder.client.Connection().FlushWithContext(testnats.Context(t)); err != nil {
		t.Fatalf("flush the holder's restored connection: %v", err)
	}
	// The holder, back, tries for the hold at its next tick, one durableBindInterval on the grid its
	// run began, and that try finds the standby's hold live and stands by: once the tick after it is
	// armed (heldDurable.run), the try has ended, and the holder delivers nothing.
	nextTry := start.Add((deadline.Sub(start)/durableBindInterval + 1) * durableBindInterval)
	holdClock.AwaitArmedAt(t, "the holder's next try for the hold, past its lost hold", nextTry)
	holdClock.Set(nextTry)
	holdClock.AwaitArmedAt(t, "the holder's try after that one, armed once the try the move brought has ended", nextTry.Add(durableBindInterval))
	if intakeDelivers(holder.intake) {
		t.Fatalf("the holder, back, took the durable over at its try while the standby held it: %s", consumerState(publisher))
	}
	// The holder's fence, before anything is published for it to be handed: its delivery
	// subscription closed. Its lost hold stopped its delivery, which unsubscribes a subscription
	// whose link is down (heldDurable.run, bus.Client.DrainSubscription). From that close on, the
	// holder's intake is handed no message while it makes no new subscription, which the row reads
	// again before it reads the counts, so what the holder handled can no longer change by then and
	// no window is waited out for a late handling. A holder that keeps its subscription fails here.
	waitFor(t, func() bool { return !intakeHoldsSubscription(holder.intake) }, func() string {
		return "the holder still holds a live delivery subscription after the standby took the durable over: " + consumerState(publisher)
	})
	fenced := subscribedDelivery(holder.intake)
	if fenced == nil {
		t.Fatal("the holder has no delivery at all, so the row cannot tell whether a new one was made: " + consumerState(publisher))
	}

	later := make([]int, 20)
	for i := range later {
		later[i] = 200 + i
		publishMergedPullRequest(t, publisher, later[i])
	}
	waitFor(t, func() bool {
		for _, number := range later {
			if standby.fetches(number)+holder.fetches(number) == 0 {
				return false
			}
		}
		return true
	}, func() string {
		return fmt.Sprintf("not every later message was handled: standby %v, holder %v; %s",
			fetchesOf(standby, later), fetchesOf(holder, later), consumerState(publisher))
	})
	// Every message is acknowledged and none is pending, so the server has nothing left to deliver
	// of them, and the standby's count is final.
	waitFor(t, func() bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && info.NumAckPending == 0 && info.NumPending == 0
	}, func() string {
		return "the later messages were not all acknowledged: " + consumerState(publisher)
	})
	// The holder's count is final too, if its intake still has the delivery whose subscription
	// closed before any of these messages existed, and that subscription is still closed.
	if delivering := subscribedDelivery(holder.intake); delivering != fenced || intakeHoldsSubscription(holder.intake) {
		t.Fatalf("the holder subscribed for delivery again after the standby took the durable over: it has the delivery whose subscription closed = %t, and holds a live delivery subscription = %t: %s",
			delivering == fenced, intakeHoldsSubscription(holder.intake), consumerState(publisher))
	}
	for _, number := range later {
		if got := standby.fetches(number) + holder.fetches(number); got != 1 {
			t.Errorf("message %d was handled %d times (standby %d, holder %d), want exactly once",
				number, got, standby.fetches(number), holder.fetches(number))
		}
		if got := holder.fetches(number); got != 0 {
			t.Errorf("the holder the standby took the durable over from handled message %d %d times, want never: standby %d",
				number, got, standby.fetches(number))
		}
	}
}

// A process of the build before holds binds the durable by looking it up and binding it when the
// lookup reports nobody bound to it. A take-over that created the durable before it subscribed to
// the durable's deliver subject would leave a moment in which such a lookup finds the durable
// unbound, and both processes would then be handed every message on one deliver subject. The
// take-over subscribes first, so the durable is push-bound from the moment it exists: a lookup the
// moment the server announces its creation finds it bound, even with the taking process's link to
// the server slow. The link's latency is the row's own argument (natsCut), the width of the window a
// create sent before its subscribe would leave, so the row takes that latency for each of the
// take-over's calls through it.
func TestATakeOverCreatesTheDurableAlreadyBound(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	intakeTestClient(t)

	// The previous build's standby, as fast as it can be: it looks the durable up the moment the
	// server announces that the durable was created.
	type lookup struct {
		bound bool
		err   error
	}
	lookups := make(chan lookup, 8)
	observer := testnats.Connect(t, natsURL)
	t.Cleanup(observer.Close)
	observerJS, err := observer.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("open the observer's JetStream context: %v", err)
	}
	created := "$JS.EVENT.ADVISORY.CONSUMER.CREATED." + bus.Stream + "." + bus.DispatchDeliveryDurable
	if _, err := observer.Subscribe(created, func(*natsgo.Msg) {
		info, err := observerJS.ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		select {
		case lookups <- lookup{bound: err == nil && info.PushBound, err: err}:
		default:
		}
	}); err != nil {
		t.Fatalf("subscribe to the durable's creation: %v", err)
	}
	if err := observer.FlushWithContext(testnats.Context(t)); err != nil {
		t.Fatalf("flush the observer's subscription: %v", err)
	}

	_, slowURL := startNATSCut(t, natsURL, 500*time.Millisecond)
	startIntakeProcess(t, pool, slowURL, nil)
	first := testwait.Receive(t, "the take-over to create the durable", lookups)
	if !first.bound {
		t.Fatalf("a lookup the moment the take-over created the durable found it unbound (error %v): a process that binds by lookup would have bound it too", first.err)
	}
}

// generationKey is the consumer metadata key that holds the generation of the durable's settings,
// written out here rather than taken from the code under test, so a build that renamed it would not
// see the generation another build stamped.
const generationKey = "dispatch.consumer-generation"

// intakeHold is the name of the hold a process delivers from the delivery durable under, written
// out for the same reason.
const intakeHold = "delivery-intake-" + bus.DispatchDeliveryDurable

// intakeConsumerLease is the name of the lease every change to the delivery durable runs under, of
// this build and of the build before holds, written out for the same reason.
const intakeConsumerLease = "delivery-consumer-" + bus.DispatchDeliveryDurable

// A consumer a newer build stamped with a higher generation keeps that build's settings: a standby
// of this build changes nothing about it across its tries for the hold while the newer build holds
// it, and once that build lets go, the standby's take-over recreates the durable with the newer
// build's settings, not its own. The standby tries on a fake hold clock the row moves one
// durableBindInterval at a time (holdTries): the durable is read once its try at its start and four
// after it have each ended standing by.
func TestAStandbyOfAnOlderGenerationLeavesANewerConsumerAlone(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	// The newer build's durable: the generation after this build's, with flow control this build
	// does not set.
	newerGeneration := strconv.Itoa(consumerConfigGeneration + 1)
	newer := natsgo.ConsumerConfig{
		Durable:        bus.DispatchDeliveryDurable,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  githubIntakeSubject,
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        7 * time.Minute,
		MaxAckPending:  9,
		Metadata:       map[string]string{generationKey: newerGeneration},
	}
	before, err := publisher.JS().AddConsumer(bus.Stream, &newer)
	if err != nil {
		t.Fatalf("create the newer build's durable: %v", err)
	}
	// The newer build's holder: its hold, recording its deliver subject, and its subscription.
	newerBuild, err := bus.Connect([]string{natsURL})
	if err != nil {
		t.Fatalf("connect the newer build's process: %v", err)
	}
	t.Cleanup(func() { bustest.Close(t, newerBuild) })
	newerLeases := storetest.Leases(t, storetest.SecondProcess(t, &store.Store{Pool: pool}).Pool)
	hold, err := duty.TryHold(ctx, newerLeases, intakeHold, uuid.New(),
		duty.HoldOptions{TTL: 15 * time.Second, Renew: 5 * time.Second, Margin: 2 * time.Second, Token: newer.DeliverSubject})
	if err != nil || hold == nil {
		t.Fatalf("take the newer build's hold: hold %v, error %v", hold, err)
	}
	t.Cleanup(func() { _ = hold.Release(context.Background()) })
	held, err := newerBuild.Subscribe(githubIntakeSubject, func(msg *natsgo.Msg) { _ = msg.Ack() },
		natsgo.Bind(bus.Stream, bus.DispatchDeliveryDurable), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("bind the newer build's process: %v", err)
	}
	awaitPushBound(t, publisher, "", "the newer build's process is not bound")

	tries := newHoldTries()
	standby := startIntakeProcessWith(t, pool, natsURL, nil, tries.on)
	tries.standBy(t, "the standby while the newer build holds the durable", 4)
	after, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the durable: %v", err)
	}
	if after.Config.AckWait != newer.AckWait || after.Config.MaxAckPending != newer.MaxAckPending ||
		after.Config.Metadata[generationKey] != newerGeneration || after.Config.DeliverSubject != newer.DeliverSubject ||
		!after.Created.Equal(before.Created) {
		t.Fatalf("the standby changed the newer build's durable: ack_wait %s max_ack_pending %d generation %q deliver_subject %s created %s, want %s %d %q %s %s",
			after.Config.AckWait, after.Config.MaxAckPending, after.Config.Metadata[generationKey], after.Config.DeliverSubject, after.Created,
			newer.AckWait, newer.MaxAckPending, newerGeneration, newer.DeliverSubject, before.Created)
	}

	// The newer build stops: it unsubscribes and releases its hold.
	if err := held.Unsubscribe(); err != nil {
		t.Fatalf("unsubscribe the newer build's process: %v", err)
	}
	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release the newer build's hold: %v", err)
	}
	tries.untilDelivering(t, "the standby to take the released durable over")
	var taken *natsgo.ConsumerInfo
	waitFor(t, func() bool {
		taken, err = publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && taken.PushBound && taken.Config.DeliverSubject != newer.DeliverSubject
	}, func() string {
		return "the standby never took the released durable over: " + consumerState(publisher)
	})
	if taken.Config.AckWait != newer.AckWait || taken.Config.MaxAckPending != newer.MaxAckPending || taken.Config.Metadata[generationKey] != newerGeneration {
		t.Fatalf("the take-over recreated the durable with ack_wait %s max_ack_pending %d generation %q, want the newer build's %s %d %q",
			taken.Config.AckWait, taken.Config.MaxAckPending, taken.Config.Metadata[generationKey], newer.AckWait, newer.MaxAckPending, newerGeneration)
	}
	publishMergedPullRequest(t, publisher, 5)
	waitFor(t, func() bool { return standby.fetches(5) == 1 }, func() string {
		return "the standby that took the durable over did not handle a message: " + consumerState(publisher)
	})
}

// A consumer bound by a process of the build before holds (it binds without taking a hold, so no
// hold recorded its deliver subject) is never taken from it: this build's standby waits, handles
// nothing, and changes nothing, until that process lets go. Then it takes the durable over on a
// deliver subject of its own.
func TestANewBuildStandbyWaitsForABindingNoHoldRecorded(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	// The previous build's durable and its process bound to it.
	previous := natsgo.ConsumerConfig{
		Durable:        bus.DispatchDeliveryDurable,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  githubIntakeSubject,
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        tuningFor(t).ackWait(),
		MaxAckPending:  intakeMaxAckPending,
	}
	before, err := publisher.JS().AddConsumer(bus.Stream, &previous)
	if err != nil {
		t.Fatalf("create the previous build's durable: %v", err)
	}
	previousBuild, err := bus.Connect([]string{natsURL})
	if err != nil {
		t.Fatalf("connect the previous build's process: %v", err)
	}
	t.Cleanup(func() { bustest.Close(t, previousBuild) })
	var received atomic.Int32
	held, err := previousBuild.Subscribe(githubIntakeSubject, func(msg *natsgo.Msg) {
		received.Add(1)
		_ = msg.Ack()
	}, natsgo.Bind(bus.Stream, bus.DispatchDeliveryDurable), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("bind the previous build's process: %v", err)
	}
	awaitPushBound(t, publisher, "", "the previous build's process is not bound")

	// The standby tries on a fake hold clock the row moves one durableBindInterval at a time
	// (holdTries): its try at its start, which takes the hold, and two after it have each ended
	// standing by before the message is published.
	tries := newHoldTries()
	standby := startIntakeProcessWith(t, pool, natsURL, nil, tries.on)
	tries.standBy(t, "the standby while the previous build is bound", 2)
	publishMergedPullRequest(t, publisher, 1)
	waitFor(t, func() bool { return received.Load() == 1 }, func() string {
		return "the previous build's process stopped receiving while the standby stood by: " + consumerState(publisher)
	})
	after, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the durable: %v", err)
	}
	if !after.Created.Equal(before.Created) || after.Config.DeliverSubject != previous.DeliverSubject || !held.IsValid() {
		t.Fatalf("the standby took the previous build's binding: created %s deliver_subject %s (want %s %s), subscription valid %t",
			after.Created, after.Config.DeliverSubject, before.Created, previous.DeliverSubject, held.IsValid())
	}
	if got := standby.fetches(1); got != 0 {
		t.Fatalf("the standby handled a message the previous build's process holds (%d fetches)", got)
	}

	if err := held.Unsubscribe(); err != nil {
		t.Fatalf("unsubscribe the previous build's process: %v", err)
	}
	tries.untilDelivering(t, "the standby to take the durable over once the previous build let go")
	waitFor(t, func() bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && info.PushBound && info.Config.DeliverSubject != previous.DeliverSubject
	}, func() string {
		return "the standby never took the durable over on a deliver subject of its own once the previous build let go: " + consumerState(publisher)
	})
	publishMergedPullRequest(t, publisher, 2)
	waitFor(t, func() bool { return standby.fetches(2) == 1 }, func() string {
		return "the standby that took the durable over did not handle a message: " + consumerState(publisher)
	})
	if got := received.Load(); got != 1 {
		t.Fatalf("the previous build's process received %d messages, want only the one before it let go", got)
	}
}

// fetchesOf is each of numbers' fetch count at process, for a failure message.
func fetchesOf(process *intakeProcess, numbers []int) map[int]int {
	counts := make(map[int]int, len(numbers))
	for _, number := range numbers {
		counts[number] = process.fetches(number)
	}
	return counts
}

// failCreatesWhile makes the take-over's create of the durable fail while failing reports true,
// as a JetStream API that times out, or a crash between the delete and the create, leaves the
// durable deleted and not created: in the intakes the test starts after it, which take the test's
// tuning (tuningFor). It counts every create attempt.
func failCreatesWhile(t *testing.T, failing func() bool) *atomic.Int32 {
	t.Helper()
	attempts := &atomic.Int32{}
	tuning := tuningFor(t)
	create := tuning.createDurable
	tuning.createDurable = func(js natsgo.JetStreamContext, config *natsgo.ConsumerConfig, opts ...natsgo.JSOpt) (*natsgo.ConsumerInfo, error) {
		attempts.Add(1)
		if failing() {
			return nil, errors.New("the test fails the create: JetStream API timed out")
		}
		return create(js, config, opts...)
	}
	return attempts
}

// durablePartWayThrough publishes merged pull requests 1 to acknowledged+unacknowledged and leaves
// the delivery durable as a holder that stopped part way through them leaves it: unbound, with the
// first acknowledged of them acknowledged and the rest not. They are acknowledged through a pull
// fetch, which reaches them in stream order, so the ack floor sits at the last of them.
func durablePartWayThrough(t *testing.T, ctx context.Context, publisher *bus.Client, acknowledged, unacknowledged int) {
	t.Helper()
	for number := 1; number <= acknowledged+unacknowledged; number++ {
		publishMergedPullRequest(t, publisher, number)
	}
	if _, err := publisher.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable:       bus.DispatchDeliveryDurable,
		FilterSubject: githubIntakeSubject,
		AckPolicy:     natsgo.AckExplicitPolicy,
	}); err != nil {
		t.Fatalf("create the durable: %v", err)
	}
	js, err := jetstream.New(publisher.Conn, testnats.RequestTimeout(t))
	if err != nil {
		t.Fatalf("open the jetstream API: %v", err)
	}
	consumer, err := js.Consumer(ctx, bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("look up the durable: %v", err)
	}
	batch, err := consumer.Fetch(acknowledged, jetstream.FetchContext(testwait.Context(t)))
	if err != nil {
		t.Fatalf("fetch from the durable: %v", err)
	}
	got := 0
	for msg := range batch.Messages() {
		if err := msg.DoubleAck(ctx); err != nil {
			t.Fatalf("acknowledge a message: %v", err)
		}
		got++
	}
	if err := batch.Error(); err != nil || got != acknowledged {
		t.Fatalf("fetch the first %d messages: got %d, err %v", acknowledged, got, err)
	}
	if info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable); err != nil || info.AckFloor.Consumer != uint64(acknowledged) {
		t.Fatalf("the durable's ack floor = %s, want %d", consumerState(publisher), acknowledged)
	}
}

// requireDeliveredFromTheFloor waits for process to handle each of the unacknowledged messages
// once, then for the durable to have nothing left to hand anybody (awaitEverythingAcknowledged), by
// which time an acknowledged message the take-over wrongly replayed has been handled too: deliver
// handles a message before it acknowledges it. It fails the test for any of them.
func requireDeliveredFromTheFloor(t *testing.T, publisher *bus.Client, process *intakeProcess, acknowledged, unacknowledged int) {
	t.Helper()
	var acked, unacked []int
	for number := 1; number <= acknowledged+unacknowledged; number++ {
		if number <= acknowledged {
			acked = append(acked, number)
		} else {
			unacked = append(unacked, number)
		}
	}
	waitFor(t, func() bool {
		for _, number := range unacked {
			if process.fetches(number) == 0 {
				return false
			}
		}
		return true
	}, func() string {
		return fmt.Sprintf("the intake did not handle the unacknowledged messages %v: %v; %s", unacked, fetchesOf(process, unacked), consumerState(publisher))
	})
	awaitEverythingAcknowledged(t, publisher)
	for _, number := range acked {
		if got := process.fetches(number); got != 0 {
			t.Errorf("message %d was acknowledged before the take-over and was handled %d times after it", number, got)
		}
	}
	for _, number := range unacked {
		if got := process.fetches(number); got != 1 {
			t.Errorf("message %d was not acknowledged before the take-over and was handled %d times after it, want 1", number, got)
		}
	}
}

// bringCreateRetries moves holdClock, the hold's clock of an intake whose run loop started on it at
// start, through the take-over that loop began there, every try of whose create fails (failed
// counts the failed tries, and gaveUp reports the give-up's ERROR line). The take-over waits
// takeOverFirstRetry on that clock after its first failed try and twice as long after each one
// after it, up to takeOverLastRetry (heldDurable.retryHeld); the row moves the clock to each
// retry's moment once the try before it has failed and that retry is armed there, and returns the
// tries it saw fail by the time the next retry falls at or past the take-over's finish deadline,
// takeOverFinishWithin past start. A try before its retry's moment, or a give-up before that
// deadline, fails the row at once.
//
// The moves pass the loop's tries for the hold and its checks, and the hold's renewals, on the
// same clock: their timers fire as the clock passes them, and the loop reads its own only once
// the take-over returns. No moment of theirs is a retry's: those fall a quarter second off the
// loop's 2 s and 5 s grids, and a renewal's bound and the hold's loss deadline are no sooner than
// 10 s past start. Nor is a try's own call deadline, takeOverCallTimeout past it, ever pending
// beside its retry: the try's call has ended before its retry is armed.
func bringCreateRetries(t *testing.T, holdClock *clocktest.Fake, start time.Time, failed *atomic.Int32, gaveUp func() bool) int32 {
	t.Helper()
	finish := start.Add(takeOverFinishWithin)
	at, wait := start, takeOverFirstRetry
	for try := int32(1); ; try++ {
		retry := at.Add(wait)
		testwait.Until(t, fmt.Sprintf("try %d of the take-over's create to fail, %s past the intake's start, and its retry armed %s past it",
			try, at.Sub(start), retry.Sub(start)), func() (string, bool) {
			got := failed.Load()
			switch {
			case gaveUp():
				t.Fatalf("the take-over gave its create up after %d tries with its hold's clock %s past the intake's start, short of its finish deadline %s",
					got, holdClock.Now().Sub(start), takeOverFinishWithin)
			case got > try:
				t.Fatalf("the take-over tried the create %d times with its hold's clock %s past the intake's start, where try %d is not due before %s",
					got, holdClock.Now().Sub(start), try+1, retry.Sub(start))
			}
			armed := holdClock.Armed()
			pending := make([]time.Duration, len(armed))
			for i, deadline := range armed {
				pending[i] = deadline.Sub(start)
			}
			return fmt.Sprintf("%d failed tries, armed %v past the intake's start", got, pending), got == try && slices.ContainsFunc(armed, retry.Equal)
		})
		if !retry.Before(finish) {
			return try
		}
		holdClock.Set(retry)
		at, wait = retry, min(2*wait, takeOverLastRetry)
	}
}

// createGivenUp reports whether logs holds the ERROR line of a take-over that gave its create of
// the delivery durable up (heldDurable.retryHeld, failedStep).
func createGivenUp(logs func() []map[string]any) bool {
	for _, record := range logs() {
		if record["level"] == "ERROR" && record["msg"] == "dispatch delivery: create NATS consumer" && record["name"] == bus.DispatchDeliveryDurable {
			return true
		}
	}
	return false
}

// A take-over deletes the durable and creates it again at its ack floor. When every try of the
// create fails after the delete, the take-over gives the consumer lease up with the durable absent,
// and the next take-over of the same intake finds no durable and creates it at that same floor:
// none of the acknowledged messages come back, and none of the others are skipped.
//
// The intake's hold runs on a fake (heldDurable.holdClock), and so do the take-over's bound and its
// waits between tries (finishTakeOver, retryHeld): the row brings each try of the create at its
// retry's moment, and the give-up at the bound and not before it, so the count of tries is the
// product's schedule and no machine's speed. The next take-over follows at once: the loop's try
// for the hold fell due on the clock during the first one, and waits for it to return.
func TestATakeOverWhoseCreateFailsCreatesTheDurableAtItsFloorOnTheNextAttempt(t *testing.T) {
	t.Parallel()
	logs := captureIntakeLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	const acknowledged, unacknowledged = 5, 4
	durablePartWayThrough(t, ctx, publisher, acknowledged, unacknowledged)

	// Every try of the first take-over's create fails: the creates fail until that take-over has
	// given its create up, which it logs at ERROR before it returns (createGivenUp), so the create
	// that succeeds is a later take-over's.
	gaveUp := func() bool { return createGivenUp(logs) }
	var failed atomic.Int32
	attempts := failCreatesWhile(t, func() bool {
		if gaveUp() {
			return false
		}
		failed.Add(1)
		return true
	})
	holdClock := fakeIntakeClock()
	start := holdClock.Now()
	process := startIntakeProcessWith(t, pool, natsURL, nil, func(intake *Intake) { intake.holdClock = holdClock })
	tries := bringCreateRetries(t, holdClock, start, &failed, gaveUp)
	if tries < 2 {
		t.Fatalf("the take-over tried the create %d times before its finish deadline %s, want it tried again", tries, takeOverFinishWithin)
	}
	if gaveUp() {
		t.Fatalf("the take-over gave its create up after %d tries with its hold's clock %s past the intake's start, short of its finish deadline %s",
			failed.Load(), holdClock.Now().Sub(start), takeOverFinishWithin)
	}
	holdClock.Set(start.Add(takeOverFinishWithin))
	testwait.Until(t, "the take-over to give its create up at its finish deadline", func() (int32, bool) { return failed.Load(), gaveUp() })
	if got := failed.Load(); got != tries {
		t.Fatalf("the take-over tried the create %d times before it gave up, want the %d tries due within its finish deadline %s", got, tries, takeOverFinishWithin)
	}
	requireDeliveredFromTheFloor(t, publisher, process, acknowledged, unacknowledged)
	if got, failedTries := attempts.Load(), failed.Load(); got <= failedTries {
		t.Fatalf("the intake tried the create %d times, want the %d failed ones and one after them", got, failedTries)
	}
}

// A take-over whose create fails after the delete leaves its start to the next holder: a process
// stopped gracefully before any create succeeded hands the hold on, and a fresh intake, as a
// restarted process runs one, creates the durable at the old floor: none of the acknowledged
// messages come back, and none of the others are skipped.
//
// The first process's take-over runs on its hold clock (heldDurable.holdClock), a fake the row
// moves, and its stop's deadline on its own clock (heldDurable.clock), a fake the row never moves.
// The row stops it while its take-over is still trying the create, and once Run's context has ended
// (the stop's deadline armed on its clock) moves its hold clock to the take-over's finish deadline,
// takeOverFinishWithin past its start, where the take-over gives the create up; the stop, which
// waits for the take-over, then returns. A loop that tried again past that point would do it under
// Run's ended context, which takes no lease.
func TestATakeOverWhoseCreateFailsLeavesTheFloorToTheNextHolder(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	const acknowledged, unacknowledged = 5, 4
	durablePartWayThrough(t, ctx, publisher, acknowledged, unacknowledged)

	var failing atomic.Bool
	failing.Store(true)
	attempts := failCreatesWhile(t, failing.Load)
	holdClock, stopClock := fakeIntakeClock(), fakeIntakeClock()
	start := holdClock.Now()
	first := startIntakeProcessWith(t, pool, natsURL, nil, func(intake *Intake) {
		intake.clock, intake.holdClock = stopClock, holdClock
	})
	waitFor(t, func() bool {
		_, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return attempts.Load() > 0 && errors.Is(err, natsgo.ErrConsumerNotFound)
	}, func() string {
		return fmt.Sprintf("the first intake did not delete the durable and fail its create (%d creates): %s", attempts.Load(), consumerState(publisher))
	})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		first.stop()
	}()
	stopClock.AwaitArmedAt(t, "the first intake's stop deadline, armed as its Run's context ended", stopClock.Now().Add(tuningFor(t).stopBudget))
	holdClock.Set(start.Add(takeOverFinishWithin))
	testwait.Receive(t, "the first intake's stop to return once its take-over gave its create up at its finish deadline", stopped)
	failing.Store(false)

	next := startIntakeProcess(t, pool, natsURL, nil)
	requireDeliveredFromTheFloor(t, publisher, next, acknowledged, unacknowledged)
}

// previousBuild is a process of the build before holds, as far as the delivery durable goes: the
// configuration reconciliation of that build (legion-envoy-v19.0.0's reconcileDurableConsumer,
// cut to its first case) on a pool of its own. A pass, under the durable's consumer lease, looks
// the durable up, and when there is none it creates the durable with that build's config, which
// names no start and so delivers from the stream's first message. It reads no hold. created counts
// the durables its passes created, present the passes that found the durable there, and passes
// every pass it made, one skipped because another process held the lease included.
//
// That build paces its passes by a ticker of its own (durableBindInterval). Here they run only
// when the test asks: one at a time (pass), or back to back from reconcile to stop, over a window
// the test opens and closes itself, so a pass reaches every moment of that window it can and none
// is left to a pace.
type previousBuild struct {
	// t is the test a give-up of the join of the passes (stop) is reported to, through t.Error.
	t      *testing.T
	leases duty.Leases
	client *bus.Client
	// ackWait is the ack wait of the durable a pass creates: the test's tuning's (ackWait), read
	// once on the test's goroutine.
	ackWait time.Duration

	created, present, passes atomic.Int32
	// passed is closed by the first pass, once it has run (passedOnce).
	passed     chan struct{}
	passedOnce sync.Once

	mu sync.Mutex
	// end stops the passes reconcile started and waits for the one in flight; nil while none run.
	end func()
}

// previousBuildReconciler is a process of the build before holds (previousBuild) on pool's database
// (a pool of its own, storetest.SecondProcess) and natsURL's server, whose passes end with the test.
func previousBuildReconciler(t *testing.T, pool *store.Pool, natsURL string) *previousBuild {
	t.Helper()
	leases := storetest.Leases(t, storetest.SecondProcess(t, &store.Store{Pool: pool}).Pool)
	client, err := bus.Connect([]string{natsURL})
	if err != nil {
		t.Fatalf("connect the previous build's process: %v", err)
	}
	t.Cleanup(func() { bustest.Close(t, client) })
	p := &previousBuild{t: t, leases: leases, client: client, ackWait: tuningFor(t).ackWait(), passed: make(chan struct{})}
	t.Cleanup(p.stop)
	return p
}

// pass makes one pass, as that build's process does at each tick of its reconciliation.
func (p *previousBuild) pass() {
	p.passes.Add(1)
	defer p.passedOnce.Do(func() { close(p.passed) })
	duty.Exclusive(context.Background(), nil, p.leases, intakeConsumerLease, time.Time{}, 0, func(context.Context) {
		_, err := p.client.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		switch {
		case errors.Is(err, natsgo.ErrConsumerNotFound):
			previous := natsgo.ConsumerConfig{
				Durable:        bus.DispatchDeliveryDurable,
				DeliverSubject: natsgo.NewInbox(),
				FilterSubject:  githubIntakeSubject,
				AckPolicy:      natsgo.AckExplicitPolicy,
				AckWait:        p.ackWait,
				MaxAckPending:  intakeMaxAckPending,
			}
			if _, err := p.client.JS().AddConsumer(bus.Stream, &previous); err == nil {
				p.created.Add(1)
			}
		case err == nil:
			p.present.Add(1)
		}
	})
}

// reconcile makes passes back to back, on a goroutine of their own, until stop.
func (p *previousBuild) reconcile() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.end != nil {
		return
	}
	quit, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		for {
			select {
			case <-quit:
				return
			default:
				p.pass()
			}
		}
	}()
	p.end = func() {
		close(quit)
		// stop runs on the intake's goroutine (the second create, failCreatesWhile) as well as the
		// test's, so the wait is the form for a goroutine that is not the test's: a give-up fails the
		// test through t.Error, which ends no goroutine, and the wait returns.
		if _, err := testwait.ReceiveInHelper("previousBuild.stop", "the previous build's passes to end once quit closed", ended); err != nil {
			p.t.Error(err)
		}
	}
}

// stop ends the passes reconcile started, once the pass in flight has returned. Its wait returns
// whether the passes ended or it gave up (reconcile's end), so whichever of the intake's stop and
// the cleanup's comes second finds nothing to end.
func (p *previousBuild) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.end != nil {
		p.end()
		p.end = nil
	}
}

// A take-over whose create fails once after the delete creates the durable itself, at its ack
// floor, while a process of the build before holds reconciles the same durable under the same
// consumer lease: that process never finds the durable missing, so it never creates its own,
// which would replay every message the durable had acknowledged. The window is the take-over's,
// from the create that fails, made after the delete, to the create tried after it, and that
// process reconciles through the whole of it. None of the acknowledged messages come back, none of
// the others are skipped, and that process finds the durable there afterwards.
//
// The take-over's wait before it tries the create again is on the intake's hold clock
// (heldDurable.holdClock, retryHeld), a fake the row moves to that retry once the failed create has
// armed it: the window closes when the row moves the clock, and not on a span of the machine's.
func TestATakeOverWhoseCreateFailsCreatesTheDurableBeforeAPreviousBuildReconciles(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	const acknowledged, unacknowledged = 5, 4
	durablePartWayThrough(t, ctx, publisher, acknowledged, unacknowledged)
	floor, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the durable: %v", err)
	}
	start := floor.AckFloor.Stream + 1

	previous := previousBuildReconciler(t, pool, natsURL)
	var creates atomic.Int32
	attempts := failCreatesWhile(t, func() bool {
		switch creates.Add(1) {
		case 1:
			// The failing create returns only once the previous build has made a pass, so a pass
			// falls inside the window whatever the take-over's retry wait is. The hook runs on the
			// intake's goroutine, so it waits in the form for a goroutine that is not the test's: a
			// pass that never comes fails the row through t.Error at the shared wait's give-up,
			// naming it, and the create fails and goes on.
			previous.reconcile()
			if _, err := testwait.ReceiveInHelper("failCreatesWhile", "the previous build's first pass", previous.passed); err != nil {
				t.Error(err)
			}
			return true
		case 2:
			previous.stop()
		}
		return false
	})
	holdClock := fakeIntakeClock()
	process := startIntakeProcessWith(t, pool, natsURL, nil, func(intake *Intake) { intake.holdClock = holdClock })
	retry := holdClock.Now().Add(takeOverFirstRetry)
	holdClock.AwaitArmedAt(t, "the take-over's wait to try the create again after the one that failed", retry)
	holdClock.Set(retry)
	requireDeliveredFromTheFloor(t, publisher, process, acknowledged, unacknowledged)
	if got := attempts.Load(); got < 2 {
		t.Fatalf("the intake tried the create %d times, want the failed one and one after it", got)
	}
	if previous.passes.Load() == 0 {
		t.Fatal("the previous build's process made no pass between the take-over's failed create and the create after it, so this row proves nothing")
	}
	if got := previous.created.Load(); got != 0 {
		t.Fatalf("the previous build's process found the durable missing and created it %d times", got)
	}
	taken, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the durable: %v", err)
	}
	if taken.Config.DeliverPolicy != natsgo.DeliverByStartSequencePolicy || taken.Config.OptStartSeq != start ||
		taken.Config.Metadata[generationKey] != strconv.Itoa(consumerConfigGeneration) {
		t.Fatalf("the durable has deliver policy %d, start %d, generation %q; want this build's durable by start sequence (%d) at %d, generation %d",
			taken.Config.DeliverPolicy, taken.Config.OptStartSeq, taken.Config.Metadata[generationKey],
			natsgo.DeliverByStartSequencePolicy, start, consumerConfigGeneration)
	}
	// A pass the lease skips finds nothing, so the process passes until one takes the lease.
	before := previous.present.Load()
	waitFor(t, func() bool {
		previous.pass()
		return previous.present.Load() > before
	}, func() string {
		return "the previous build's process did not find the durable present after the take-over: " + consumerState(publisher)
	})
	if got := previous.created.Load(); got != 0 {
		t.Fatalf("the previous build's process found the durable missing after the take-over and created it %d times", got)
	}
}
