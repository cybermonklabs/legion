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
	"log/slog"
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
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
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
// link to the server does, and SlowTo changes it while the test runs. Hold stops everything the
// client sends at the relay, in order, until Release, as a link that carries nothing from the
// process to the server for as long as the test needs does.
type natsCut struct {
	listener net.Listener
	target   string
	latency  atomic.Int64

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
	relay := &natsCut{listener: listener, target: parsed.Host}
	relay.SlowTo(latency)
	t.Cleanup(func() {
		_ = listener.Close()
		relay.Cut()
	})
	go relay.serve()
	parsed.Host = listener.Addr().String()
	return relay, parsed.String()
}

// SlowTo holds back by latency everything the client sends through the relay from here on. A
// connection's handshake has the server's authorization timeout to reach it, so a test that
// wants a link slower than that slows it once its client is connected.
func (r *natsCut) SlowTo(latency time.Duration) { r.latency.Store(int64(latency)) }

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

// toServer copies what the client sends to the server, each read held back by the latency the
// relay holds as it is read (SlowTo), and by the gate while the relay holds (Hold).
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
			if latency := time.Duration(r.latency.Load()); latency > 0 {
				time.Sleep(latency)
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

// Cut closes every connection through the relay and every new one until Restore, and lets go
// what Hold held, which has nowhere to go.
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
	// The run is runIntake's. stop ends it and waits for the run to return, on the test's goroutine
	// (awaitIntakeStopped); stopping cancels the run and returns the channel the run's return
	// closes, for a row that acts while the stop is under way. A row that waits for that return
	// does so on its own test goroutine (testwait.Receive), so a give-up fails the row there, by
	// name, rather than on a goroutine of the row's; one that goes on while the stop drains leaves
	// the return to the test's cleanup (runIntake).
	stop     func()
	stopping func() <-chan struct{}
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

// bringTries moves tries, the hold's clock of an intake whose run loop started on it at start,
// through the loop's tries for the hold from the first after the one at the run's start to the
// nth, one durableBindInterval at a time, and returns once the nth has ended. The loop arms each
// try on a grid durableBindInterval apart from its start only once the try before it has ended
// (heldDurable.run), so a try has ended once the next is armed at its moment on that grid. The
// loop's checks, and a hold's renewals and loss, are on the same clock, intakeHoldRenew apart from
// the run's start or the take: a try is read only while its grid moment is short of
// 2*intakeHoldRenew, where no moment of theirs but the first check's, which no try moment meets,
// is armed beside it. A take-over's bound and its calls and waits between tries are on it too
// (finishTakeOver, retryHeld): its bound falls takeOverFinishWithin past its start, past every
// moment read here, and its calls' deadlines and its retries fall off the grid. So do the bounds of
// a try's own reads of the hold and of the durable (takeHold, lookupDurable), intakeHoldRenew past
// the try's moment, which are disarmed once the try has ended.
func bringTries(t *testing.T, tries *clocktest.Fake, start time.Time, n int) {
	t.Helper()
	if next := time.Duration(n+1) * durableBindInterval; next >= 2*intakeHoldRenew {
		t.Fatalf("bringTries reads the end of try %d from the arming of the try at %s, a moment the loop's checks or a hold's renewals may share: bring fewer tries", n, next)
	}
	// The try on the grid's kth moment is armed then, which the first read shows the loop started
	// on the fake at start, and each later read shows the try before it ended.
	armedAt := func(k int, what string) {
		t.Helper()
		due := start.Add(time.Duration(k) * durableBindInterval)
		tries.AwaitArmedAt(t, fmt.Sprintf("%s, %s past the intake's start,", what, due.Sub(start)), due)
	}
	armedAt(1, "the intake's first try for the hold after its start")
	for try := 1; try <= n; try++ {
		tries.Advance(durableBindInterval)
		armedAt(try+1, fmt.Sprintf("the intake's try for the hold after try %d, once that try has ended", try))
	}
}

// awaitTakeOver waits for taken to report that the standby whose tries for the hold run on tries
// has taken the durable over, and when the wait gives up prints failure's text. tries is a fake
// only the row moves, which bringTries has left at a moment of the try grid, so the standby's next
// try is armed one durableBindInterval past where tries stands, once the try before it has ended
// (heldDurable.run). On each reading that finds the durable not yet taken, the row moves tries to
// that try once the standby has armed it, and never further (clocktest.Fake.SetWhen). A try that
// finds the durable still bound to the process that let go -- the server reports an unsubscribe a
// moment after it has it -- stands by and arms the next, so the row waits for the take-over itself,
// however many tries it takes, and not for one try to see it.
//
// The loop's checks and the hold's renewals and loss run on tries too (bringTries), and some of
// their moments fall on a try's. A move to such a moment can come before the try is armed; the try
// then fires as it is armed, a wait of zero, at the moment tries stands at, and the next move
// waits for the moment after it.
func awaitTakeOver(t *testing.T, tries *clocktest.Fake, taken func() bool, failure func() string) {
	t.Helper()
	waitFor(t, func() bool {
		if taken() {
			return true
		}
		tries.SetWhen(func(now time.Time, armed []time.Time) (time.Time, bool) {
			next := now.Add(durableBindInterval)
			return next, slices.ContainsFunc(armed, next.Equal)
		})
		return false
	}, failure)
}

// startIntakeProcess runs a delivery intake as a process of its own on pool's database (a pool of
// its own, storetest.SecondProcess) and natsURL's server, until the test ends or its stop is
// called. Its GitHub answers every pull request 404 after counting it, which the intake logs and
// acknowledges, and holds the fetch of each number block reports true until the handler's context
// ends or the test's end. The client's own bound on that fetch is the test's give-up
// (githubapptest.RequestDeadlineAtTheGiveUp), so a row stages its next step while the fetch is held.
// options are added to its NATS client's own.
func startIntakeProcess(t *testing.T, pool *store.Pool, natsURL string, block func(number int) bool, options ...bus.ConnectOption) *intakeProcess {
	t.Helper()
	return startIntakeProcessWith(t, pool, natsURL, block, nil, options...)
}

// startIntakeProcessWith is startIntakeProcess with before run on the process once it is built
// and before its intake runs, for a row that gives the intake a clock of its own, a message
// timeout past the run (processAckWaitPastTheRun) or in-flight slots of its own, or cuts its lease
// pool from the start. A nil before does nothing. options are added to its NATS client's own.
func startIntakeProcessWith(t *testing.T, pool *store.Pool, natsURL string, block func(number int) bool, before func(*intakeProcess), options ...bus.ConnectOption) *intakeProcess {
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
	github := fake.newTestClient(githubapp.WithRequestDeadline(githubapptest.RequestDeadlineAtTheGiveUp(t)))
	// Registered after the fake's server, so it runs first: the server's Close waits for the
	// handler it holds.
	process.release = sync.OnceFunc(func() { close(held) })
	t.Cleanup(process.release)

	intake := newIntake(tuningFor(t), client, second.Pool, process.leases, github, events.NewBroker(), loadedSettingsCache(t, t.Context(), second.Pool), unrenamed())
	process.intake = intake
	if before != nil {
		before(process)
	}
	process.stopping = runIntake(t, intake)
	process.stop = func() { awaitIntakeStopped(t, process.stopping) }
	return process
}

// processAckWaitPastTheRun is ackWaitPastTheRun for the intake of a process
// startIntakeProcessWith runs, as its before.
func processAckWaitPastTheRun(p *intakeProcess) { ackWaitPastTheRun(p.intake) }

// failOnce is t for a wait that may run once t has already failed, as the stop a cleanup makes
// after a row gave up does: a give-up fails t as Fatalf does while t has not failed, and is only
// logged once it has, so a row that failed prints its own failure and not a second one.
type failOnce struct{ *testing.T }

func (t failOnce) Fatalf(format string, args ...any) {
	t.Helper()
	if t.Failed() {
		t.Logf(format, args...)
		return
	}
	t.T.Fatalf(format, args...)
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
// standby handles every one of them within seconds, not after the ack wait. The messages it holds
// are the ones waiting behind their key (intakeKey): one pull request's messages run one after
// another, so four more of the one its handler is holding wait behind it.
func TestAGracefulStopHandsTheBufferedMessagesToTheStandby(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	const inFlight = 100
	const queued = 4
	// One message in the holder's handler and four behind it on its key: the row's tuning, which
	// every intake it builds shares (tuningFor), has a slot for each of the five, set before the
	// first intake starts, and each process's intake a message timeout past the run.
	const slots = 1 + queued
	tuningFor(t).maxAckPending = slots
	holder := startIntakeProcessWith(t, pool, natsURL, func(number int) bool { return number == inFlight }, processAckWaitPastTheRun)
	awaitDurableBoundAt(t, publisher, slots)
	standby := startIntakeProcessWith(t, pool, natsURL, nil, processAckWaitPastTheRun)

	publishMergedPullRequest(t, publisher, inFlight)
	waitFor(t, func() bool { return holder.fetches(inFlight) == 1 }, func() string {
		return "the holder never started on the first message: " + consumerState(publisher)
	})
	for range queued {
		publishMergedPullRequest(t, publisher, inFlight)
	}
	awaitAckPending(t, publisher, 1+queued)

	stopped := time.Now()
	holder.stopping()
	// The stop hands the four queued messages back as it stops the keyed runner, and only then
	// waits for the handler holding the fifth. Released before that, the handler would return
	// while the runner still ran, and the runner would hand it the next queued message of the
	// same key rather than leaving it to the standby. The wait for the handler cannot return
	// while that fetch is held, so waiting for the runner here cannot deadlock.
	waitFor(t, holder.keyedRunnerStopped, func() string {
		return "the stopping holder has not stopped its keyed runner: " + consumerState(publisher)
	})
	// The message in the holder's handler finishes as the stop waits for it.
	holder.release()

	waitFor(t, func() bool { return standby.fetches(inFlight) == queued }, func() string {
		return fmt.Sprintf("the standby has not handled the stopped holder's %d queued messages of pull request %d: standby %d, holder %d; %s",
			queued, inFlight, standby.fetches(inFlight), holder.fetches(inFlight), consumerState(publisher))
	})
	t.Logf("the standby handled the queued messages %s after the holder's stop", time.Since(stopped))
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
// What it measures is the answers published to the durable, read off the ack subject itself
// rather than from the consumer's counts, as the head-write row does
// (TestAHolderWhoseHoldLapsedPublishesNoHandBackAndTheKeeperIsHandedTheEnvelopeOnce): a
// hand-back is a publish, and whether the server still has a consumer to act on it depends on
// how far the take-over has got. None of them may be a hand-back, and the standby's own
// acknowledgements are there to be read beside them, so a recorder that saw nothing at all
// cannot pass the row by silence.
func TestAStopPastTheHoldsLapsePublishesNoHandBackAndTheStandbyHandlesEachMessageOnce(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	answers := durableAnswers(t, publisher)

	// One message in the holder's handler and the rest waiting behind it on its key, which is
	// what fills the durable's in-flight slots (intakeMaxAckPending). The handler is held for
	// the whole row: both intakes take a message timeout past the run (processAckWaitPastTheRun),
	// so the handler's own deadline is six hours out on the real clock, and what ends that fetch
	// is the stop giving the handler up and not a deadline of the handler's own.
	const inFlight = 300
	queued := intakeMaxAckPending - 1
	holder := startIntakeProcessWith(t, pool, natsURL, func(number int) bool { return number == inFlight }, processAckWaitPastTheRun)
	awaitBoundDurable(t, publisher)
	standby := startIntakeProcessWith(t, pool, natsURL, nil, processAckWaitPastTheRun)

	publishMergedPullRequest(t, publisher, inFlight)
	waitFor(t, func() bool { return holder.fetches(inFlight) == 1 }, func() string {
		return "the holder never started on the first message: " + consumerState(publisher)
	})
	for range queued {
		publishMergedPullRequest(t, publisher, inFlight)
	}
	awaitAckPending(t, publisher, 1+queued)

	// The cut takes the holder off the lease pool its hold is renewed on, as a network that took
	// it off Postgres would, or as a freeze leaves a process that renews nothing: the hold
	// lapses, and the holder stops delivering.
	cut := time.Now()
	holder.leases.cut.Store(true)
	waitFor(t, holder.keyedRunnerStopped, func() string {
		return "the holder never began stopping, though its hold is no longer renewed: " + consumerState(publisher)
	})
	// The stop hands the messages it held back once the handler it gave up has returned, which
	// is a margin past the loss deadline, so every one of those hand-backs is withheld.
	waitFor(t, holder.heldListReleased, func() string {
		return "the holder's stop never released the messages it held: " + consumerState(publisher)
	})

	// The handler given up when the stop's budget ends, the hold's lapse (intakeHoldTTL, 15 s),
	// the standby's poll (durableBindInterval, 2 s) and its take-over come first.
	waitFor(t, func() bool { return standby.fetches(inFlight) == 1+queued }, func() string {
		return fmt.Sprintf("the standby handled %d of the lapsed holder's %d messages: holder %d; %s",
			standby.fetches(inFlight), 1+queued, holder.fetches(inFlight), consumerState(publisher))
	})
	t.Logf("the standby handled the lapsed holder's messages %s after its lease pool was cut", time.Since(cut))
	awaitEverythingAcknowledged(t, publisher)

	published := answers()
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
	if len(published) == 0 {
		t.Fatal("no answer to the delivery durable was recorded at all, so the row measured nothing: the standby's acknowledgements are among them")
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
// subscription, which nats.go hands to the callback while the drain runs. The client side of
// that drain lasts as long: it ends the subscription once the flush it opens with is answered
// (nats.go's checkDrained), and that flush is held at the relay too.
//
// The hold's loss deadline is on a fake the row moves (heldDurable.holdClock), and the stop's
// budget and drain wait on one the row never moves (heldDurable.clock), so no real time decides
// the order: the row cuts the holder's lease pool, so no renewal of its hold lands, stops the
// process while the deadline is still ahead on its clock -- so its run loop never reaches the
// lost hold, which would stop delivering under intakeHoldMargin instead -- and moves that clock
// to the deadline once the stop has released its held list. It publishes then: giveBack hands
// the message back at once, because the held list is released, and handBack withholds that
// hand-back, because the hold no longer covers the delivery. The standby is cut off from
// Postgres until the holder has withheld that hand-back, so it takes the durable over only after.
//
// A hand-back published instead would land on the durable the standby's take-over recreates
// under the same name (heldDurable.takeOver), move that message's pending entry and have the
// standby delivered it twice. What the row measures is the answers published to the durable,
// read off the ack subject itself (durableAnswers) as the row above reads them, and the line
// handBack logs for a message it withholds, so a run whose message never reached that callback
// fails rather than passing on a branch it never ran.
func TestAMessagePushedToADrainingHolderPastItsHoldsLapseIsLeftToTheStandby(t *testing.T) {
	records := captureIntakeLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	answers := durableAnswers(t, publisher)

	// lateNumber is the row's one message, published after the holder's stop has released the
	// messages it held -- it held none, no handler of its having been running -- and after its
	// hold's loss deadline has passed.
	const lateNumber = 310
	relay, holderURL := startNATSCut(t, natsURL, 0)
	// Both clocks stand still until the row moves them: the hold measures its lapse, and the stop
	// its drain wait, as elapsed time on them, which the machine's clock must not supply.
	holdClock := clocktest.New(time.Now())
	holder := startIntakeProcessWith(t, pool, holderURL, nil, func(p *intakeProcess) {
		ackWaitPastTheRun(p.intake)
		p.intake.holdClock = holdClock
		p.intake.clock = clocktest.New(time.Now())
	})
	awaitBoundDurable(t, publisher)
	standby := startIntakeProcessWith(t, pool, natsURL, nil, func(p *intakeProcess) {
		ackWaitPastTheRun(p.intake)
		p.leases.cut.Store(true)
	})

	// The lease pool is cut, so no renewal of the holder's hold lands, and what its client sends
	// the server is held at the relay, so its stop drains for as long as the row holds it.
	holder.leases.cut.Store(true)
	relay.Hold()
	holder.stopping()
	waitFor(t, holder.heldListReleased, func() string {
		return "the holder's stop never released its held list: " + consumerState(publisher)
	})
	holdClock.Set(holder.holdDeadline())
	if !holder.holdLapsed() {
		t.Fatalf("the holder's hold still covers its delivery with its clock at the loss deadline %v", holder.holdDeadline())
	}
	publishMergedPullRequest(t, publisher, lateNumber)

	// The message reached the callback of the subscription the stop is draining: the holder
	// logs the hand-back it withholds for it.
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
	waitFor(t, func() bool { return withheldFor() > 0 }, func() string {
		return fmt.Sprintf("the lapsed holder's draining subscription withheld no hand-back for pull request %d: %s", lateNumber, consumerState(publisher))
	})
	relay.Release()
	standby.leases.cut.Store(false)

	// The hold's lapse on the database's clock (intakeHoldTTL, 15 s), the standby's poll
	// (durableBindInterval, 2 s) and its take-over come first.
	waitFor(t, func() bool { return standby.fetches(lateNumber) == 1 }, func() string {
		return fmt.Sprintf("the standby handled the message published to the stopped holder %d times, want it once: holder %d; %s",
			standby.fetches(lateNumber), holder.fetches(lateNumber), consumerState(publisher))
	})

	// Everything the holder's client wrote is with the server once a flush of its own is
	// answered: nats.go writes one connection's publishes in the order they were made and the
	// relay hands on what it reads in that same order, so a hand-back published for that
	// message is there to be read below rather than still crossing the link. The flush crosses
	// it too, which is what it costs.
	if err := holder.client.Conn.FlushWithContext(testnats.Context(t)); err != nil {
		t.Fatalf("wait for what the stopped holder's client sent to reach the server: %v", err)
	}
	awaitEverythingAcknowledged(t, publisher)

	// The holder logged the hand-back it withheld for that message (withheldFor, above), and no
	// other answer of its own.
	t.Logf("the lapsed holder withheld %d hand-back(s) for pull request %d", withheldFor(), lateNumber)

	published := answers()
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
	if len(published) == 0 {
		t.Fatal("no answer to the delivery durable was recorded at all, so the row measured nothing: the standby's acknowledgement is among them")
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
// no longer renewed -- strands the messages it had been delivered. The standby's first try for the
// hold once that hold has lapsed takes the durable over, and the take-over redelivers every one of
// them at once, not after the ack wait, which ackWaitPastTheRun keeps from ever coming.
//
// No real time decides it. The hold's liveness is its row on the database's clock (duty.Hold),
// which no clock of the test reaches, and the row writes that clock's account itself. The holder's
// hold runs on a fake the row never moves (heldDurable.holdClock), so the holder renews nothing,
// and the row keeps its hold's row live, as those renewals would, past anything the run waits for;
// then it ends it as the database's clock passing intakeHoldTTL after the last renewal does: its
// expiry set to the database's now(), its holder and token kept, and no release notified. The
// standby tries for the hold on a fake of its own, which the row moves one durableBindInterval at
// a time: a try while the hold is live stands by, and the first try after the lapse takes the
// durable over.
func TestAnAbandonedHoldersMessagesReachTheStandbyWithinTheHoldsLapse(t *testing.T) {
	logs := captureLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	holderStart := time.Now()
	holder := startIntakeProcessWith(t, pool, natsURL, func(int) bool { return true }, func(p *intakeProcess) {
		ackWaitPastTheRun(p.intake)
		p.intake.holdClock = clocktest.New(holderStart)
	})
	// The holder's fetches hold its handlers until the test lets them go, which it does as it
	// ends, before the holder's stop (registered after it, so run first): that stop then finds its
	// handlers answered rather than waiting its budget out for them (intakeStopBudget).
	t.Cleanup(holder.release)
	awaitBoundDurable(t, publisher)
	holdName := intakeHoldName(bus.DispatchDeliveryDurable)
	// The lapse the row brings about below is the one the take stored, read before the row moves
	// it: on the holder's fake, which never moved, the hold's loss deadline is the take's moment
	// plus intakeHoldTTL less intakeHoldMargin, and the row the take wrote lapses no later than
	// intakeHoldTTL past the database's now().
	if got, want := holder.holdDeadline(), holderStart.Add(intakeHoldTTL-intakeHoldMargin); !got.Equal(want) {
		t.Fatalf("the holder's hold has its loss deadline at %s, want %s: the take's moment plus intakeHoldTTL (%s) less intakeHoldMargin (%s)",
			got, want, intakeHoldTTL, intakeHoldMargin)
	}
	var withinTTL bool
	if err := pool.QueryRow(ctx, `select expires_at <= now() + make_interval(secs => $2) from duty_holds where name = $1 and holder = $3`,
		holdName, intakeHoldTTL.Seconds(), holder.intake.holder).Scan(&withinTTL); err != nil {
		t.Fatalf("read the lapse the holder's take stored: %v", err)
	}
	if !withinTTL {
		t.Fatalf("the holder's take stored a lapse more than intakeHoldTTL (%s) past the database's now()", intakeHoldTTL)
	}
	if tag, err := pool.Exec(ctx, `update duty_holds set expires_at = now() + interval '1 day' where name = $1 and holder = $2`,
		holdName, holder.intake.holder); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("keep the holder's hold live as its renewals would: %d rows, %v", tag.RowsAffected(), err)
	}
	triesStart := time.Now()
	tries := clocktest.New(triesStart)
	standby := startIntakeProcessWith(t, pool, natsURL, nil, func(p *intakeProcess) {
		ackWaitPastTheRun(p.intake)
		p.intake.holdClock = tries
	})
	// The standby's first try, as it starts, finds the hold live and stands by.
	waitFor(t, func() bool { return loggedMessage(logs(), heldElsewhere) }, func() string {
		return fmt.Sprintf("the standby never stood by for the durable the holder holds (%q); the messages logged were %q",
			heldElsewhere, loggedMessages(logs()))
	})

	stranded := []int{1, 2, 3, 4}
	for _, number := range stranded {
		publishMergedPullRequest(t, publisher, number)
	}
	awaitAckPending(t, publisher, len(stranded))
	holder.leases.cut.Store(true)
	bustest.Close(t, holder.client)

	// A try while the abandoned hold is still live stands by: the try has ended once the next one
	// is armed, and then the hold still names the holder and the standby has handled nothing.
	bringTries(t, tries, triesStart, 1)
	var named string
	var live bool
	if err := pool.QueryRow(ctx, "select holder::text, expires_at > now() from duty_holds where name = $1", holdName).Scan(&named, &live); err != nil {
		t.Fatalf("read the durable's hold: %v", err)
	}
	if named != holder.intake.holder.String() || !live {
		t.Fatalf("after the standby's try while the abandoned hold was live, the hold names %s, live = %t; want the holder's %s, live",
			named, live, holder.intake.holder)
	}
	for _, number := range stranded {
		if got := standby.fetches(number); got != 0 {
			t.Fatalf("the standby handled pull request %d %d times while the abandoned hold was still live, want none: %s",
				number, got, consumerState(publisher))
		}
	}

	// The hold lapses, and the standby's next try takes the durable over.
	if _, err := pool.Exec(ctx, `update duty_holds set expires_at = now() where name = $1`, holdName); err != nil {
		t.Fatalf("lapse the abandoned hold on the database's clock: %v", err)
	}
	tries.Advance(durableBindInterval)
	waitFor(t, func() bool {
		for _, number := range stranded {
			if standby.fetches(number) == 0 {
				return false
			}
		}
		return true
	}, func() string {
		return fmt.Sprintf("the standby's try after the hold lapsed has not handled the abandoned holder's messages %v: standby %v; %s",
			stranded, fetchesOf(standby, stranded), consumerState(publisher))
	})
}

// A holder cut off from NATS and Postgres loses its hold, and the standby takes the durable over.
// When the holder's connection comes back, its client sends its subscription again on the deliver
// subject it had; the standby's take-over moved the durable to another, so the holder is handed
// nothing, and each later message is handled by exactly one process.
func TestAHolderCutOffAndRestoredHandlesNothingTheStandbyTookOver(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	relay, relayedURL := startNATSCut(t, natsURL, 0)
	holder := startIntakeProcess(t, pool, relayedURL, nil)
	awaitBoundDurable(t, publisher)
	standby := startIntakeProcess(t, pool, natsURL, nil)
	// The bus runs a client's reconnect hooks once it has restored the subscriptions it still
	// holds registrations for (internal/bus/recovery.go), so this one says when the holder's
	// client has sent its subscriptions again.
	reconnected := make(chan struct{}, 8)
	holder.client.AddReconnectHook(func(context.Context, *natsgo.Conn) error {
		reconnected <- struct{}{}
		return nil
	})

	relay.Cut()
	holder.leases.cut.Store(true)
	cut := time.Now()
	waitFor(t, func() bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && info.PushBound
	}, func() string {
		return "the standby never took the durable over from the cut-off holder: " + consumerState(publisher)
	})
	t.Logf("the standby took the durable over %s after the holder was cut off", time.Since(cut))

	relay.Restore()
	holder.leases.cut.Store(false)
	waitFor(t, holder.client.Connected, func() string {
		return "the holder's NATS connection did not come back"
	})
	// The holder's client has sent its subscriptions again once its reconnect hooks have run, and
	// the server has read them once a flush on that connection is answered.
	testwait.Receive(t, "the holder's client to restore its subscriptions on its connection back", reconnected)
	if err := holder.client.Conn.FlushWithContext(testnats.Context(t)); err != nil {
		t.Fatalf("flush the holder's connection back: %v", err)
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
	// Once nothing is left with the durable, no second delivery of any of them is on its way, so
	// the counts are final.
	awaitEverythingAcknowledged(t, publisher)
	for _, number := range later {
		if got := standby.fetches(number) + holder.fetches(number); got != 1 {
			t.Errorf("message %d was handled %d times (standby %d, holder %d), want exactly once",
				number, got, standby.fetches(number), holder.fetches(number))
		}
	}
}

// A process of the build before holds binds the durable by looking it up and binding it when the
// lookup reports nobody bound to it. A take-over that created the durable before it subscribed to
// the durable's deliver subject would leave a moment in which such a lookup finds the durable
// unbound, and both processes would then be handed every message on one deliver subject. The
// take-over subscribes first, so the durable is push-bound from the moment it exists: a lookup the
// moment the server announces its creation finds it bound, even with the taking process's link to
// the server slow.
func TestATakeOverCreatesTheDurableAlreadyBound(t *testing.T) {
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
// of this build changes nothing about it across two settings polls while the newer build holds it,
// and once that build lets go, the standby's take-over recreates the durable with the newer build's
// settings, not its own.
func TestAStandbyOfAnOlderGenerationLeavesANewerConsumerAlone(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	// The newer build's durable: generation 2, with flow control this build does not set.
	newer := natsgo.ConsumerConfig{
		Durable:        bus.DispatchDeliveryDurable,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  githubIntakeSubject,
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        7 * time.Minute,
		MaxAckPending:  9,
		Metadata:       map[string]string{generationKey: "2"},
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
	awaitPushBound(t, publisher, "the newer build's process is not bound")

	// The standby's tries for the hold run on a fake the row moves: its try at its start and three
	// after it, each of which would have changed the durable had it been going to, have all ended
	// once bringTries returns.
	triesStart := time.Now()
	tries := clocktest.New(triesStart)
	standby := startIntakeProcessWith(t, pool, natsURL, nil, func(p *intakeProcess) { p.intake.holdClock = tries })
	bringTries(t, tries, triesStart, 3)
	after, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the durable: %v", err)
	}
	if after.Config.AckWait != newer.AckWait || after.Config.MaxAckPending != newer.MaxAckPending ||
		after.Config.Metadata[generationKey] != "2" || after.Config.DeliverSubject != newer.DeliverSubject ||
		!after.Created.Equal(before.Created) {
		t.Fatalf("the standby changed the newer build's durable: ack_wait %s max_ack_pending %d generation %q deliver_subject %s created %s, want %s %d %q %s %s",
			after.Config.AckWait, after.Config.MaxAckPending, after.Config.Metadata[generationKey], after.Config.DeliverSubject, after.Created,
			newer.AckWait, newer.MaxAckPending, "2", newer.DeliverSubject, before.Created)
	}

	// The newer build stops: it unsubscribes and releases its hold.
	if err := held.Unsubscribe(); err != nil {
		t.Fatalf("unsubscribe the newer build's process: %v", err)
	}
	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release the newer build's hold: %v", err)
	}
	// A try of the standby's that finds the hold released and the durable unbound takes it over.
	var taken *natsgo.ConsumerInfo
	awaitTakeOver(t, tries, func() bool {
		taken, err = publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && taken.PushBound && taken.Config.DeliverSubject != newer.DeliverSubject
	}, func() string {
		return "the standby never took the released durable over: " + consumerState(publisher)
	})
	if taken.Config.AckWait != newer.AckWait || taken.Config.MaxAckPending != newer.MaxAckPending || taken.Config.Metadata[generationKey] != "2" {
		t.Fatalf("the take-over recreated the durable with ack_wait %s max_ack_pending %d generation %q, want the newer build's %s %d %q",
			taken.Config.AckWait, taken.Config.MaxAckPending, taken.Config.Metadata[generationKey], newer.AckWait, newer.MaxAckPending, "2")
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
	awaitPushBound(t, publisher, "the previous build's process is not bound")

	// The standby's tries for the hold run on a fake the row moves: its try at its start, which
	// takes the hold, and three after it have all ended once bringTries returns, before the message,
	// so it arrives while the standby stands by.
	triesStart := time.Now()
	tries := clocktest.New(triesStart)
	standby := startIntakeProcessWith(t, pool, natsURL, nil, func(p *intakeProcess) { p.intake.holdClock = tries })
	bringTries(t, tries, triesStart, 3)
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
	// A try of the standby's that finds the durable unbound takes it over.
	awaitTakeOver(t, tries, func() bool {
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
// once, then for the durable to have nothing left to hand anybody, by which time an acknowledged
// message the take-over wrongly replayed has been handled too, and fails the test for any of them.
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

// bringCreateRetries moves tries, the hold's clock of an intake whose run loop started on it at
// start, through the take-over that loop began there, every try of whose create fails (failed
// counts the failed tries, and gaveUp reports the give-up's ERROR line). The take-over waits
// takeOverFirstRetry on that clock after its first failed try and twice as long after each one
// after it, up to takeOverLastRetry (heldDurable.retryHeld); the driver moves the clock to each
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
func bringCreateRetries(t *testing.T, tries *clocktest.Fake, start time.Time, failed *atomic.Int32, gaveUp func() bool) int32 {
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
					got, tries.Now().Sub(start), takeOverFinishWithin)
			case got > try:
				t.Fatalf("the take-over tried the create %d times with its hold's clock %s past the intake's start, where try %d is not due before %s",
					got, tries.Now().Sub(start), try+1, retry.Sub(start))
			}
			armed := tries.Armed()
			pending := make([]time.Duration, len(armed))
			for i, deadline := range armed {
				pending[i] = deadline.Sub(start)
			}
			return fmt.Sprintf("%d failed tries, armed %v past the intake's start", got, pending), got == try && slices.ContainsFunc(armed, retry.Equal)
		})
		if !retry.Before(finish) {
			return try
		}
		tries.Set(retry)
		at, wait = retry, min(2*wait, takeOverLastRetry)
	}
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
	logs := captureLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	const acknowledged, unacknowledged = 5, 4
	durablePartWayThrough(t, ctx, publisher, acknowledged, unacknowledged)

	// Every try of the first take-over's create fails: the creates fail until that take-over has
	// given its create up, which it logs at ERROR before it returns (heldDurable.retryHeld,
	// failedStep), so the create that succeeds is a later take-over's.
	gaveUp := func() bool {
		for _, record := range logs() {
			if record["level"] == "ERROR" && record["msg"] == "dispatch delivery: create NATS consumer" {
				return true
			}
		}
		return false
	}
	var failed atomic.Int32
	attempts := failCreatesWhile(t, func() bool {
		if gaveUp() {
			return false
		}
		failed.Add(1)
		return true
	})
	start := time.Now()
	holdClock := clocktest.New(start)
	process := startIntakeProcessWith(t, pool, natsURL, nil, func(p *intakeProcess) { p.intake.holdClock = holdClock })
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
	first := startIntakeProcess(t, pool, natsURL, nil)
	waitFor(t, func() bool {
		_, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return attempts.Load() > 0 && errors.Is(err, natsgo.ErrConsumerNotFound)
	}, func() string {
		return fmt.Sprintf("the first intake did not delete the durable and fail its create (%d creates): %s", attempts.Load(), consumerState(publisher))
	})
	first.stop()
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

	created, present, passes atomic.Int32
	// passed is closed by the first pass, once it has run (passedOnce).
	passed     chan struct{}
	passedOnce sync.Once

	mu sync.Mutex
	// end stops the passes reconcile started and waits for the one in flight; nil while none run.
	end func()
}

func previousBuildReconciler(t *testing.T, pool *store.Pool, natsURL string) *previousBuild {
	t.Helper()
	leases := storetest.Leases(t, storetest.SecondProcess(t, &store.Store{Pool: pool}).Pool)
	client, err := bus.Connect([]string{natsURL})
	if err != nil {
		t.Fatalf("connect the previous build's process: %v", err)
	}
	t.Cleanup(func() { bustest.Close(t, client) })
	p := &previousBuild{t: t, leases: leases, client: client, passed: make(chan struct{})}
	t.Cleanup(p.stop)
	return p
}

// pass makes one pass, as that build's process does at each tick of its reconciliation.
func (p *previousBuild) pass() {
	p.passes.Add(1)
	defer p.passedOnce.Do(func() { close(p.passed) })
	duty.Exclusive(context.Background(), p.leases, intakeConsumerLease, time.Time{}, 0, func(context.Context) {
		_, err := p.client.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		switch {
		case errors.Is(err, natsgo.ErrConsumerNotFound):
			previous := natsgo.ConsumerConfig{
				Durable:        bus.DispatchDeliveryDurable,
				DeliverSubject: natsgo.NewInbox(),
				FilterSubject:  githubIntakeSubject,
				AckPolicy:      natsgo.AckExplicitPolicy,
				AckWait:        newIntakeTuning().ackWait(),
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
			// falls inside the window whatever the take-over's retry wait is on the machine's
			// clock. The hook runs on the intake's goroutine, so it waits in the form for a goroutine
			// that is not the test's: a pass that never comes fails the row through t.Error at the
			// shared wait's give-up, naming it, and the create fails and goes on.
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
	process := startIntakeProcess(t, pool, natsURL, nil)
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

// An intake writes DurableChecked at Debug for each check of its durable, numbered from 1 in its
// run, with the durable's name and what the check found, so a test that runs the real binary
// counts a process's checks from its log. The holder's checks run on its hold's clock, a fake the
// row moves one intakeHoldRenew at a time: each move brings one check, and the holder still bound
// finds the durable delivering to it each time. The row moves the clock only once the take-over
// has logged its bind, its last step: the take-over's calls and its retries run on that clock too
// (finishTakeOver, retryHeld), so a move inside one would end that call and leave its retry to a
// move the row never makes.
func TestAnIntakeLogsEachCheckOfItsDurableAtDebug(t *testing.T) {
	logs := captureLogsFrom(t, slog.LevelDebug)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	checks := clocktest.New(time.Now())
	startIntakeProcessWith(t, pool, natsURL, nil, func(p *intakeProcess) { p.intake.holdClock = checks })
	awaitBoundDurable(t, publisher)
	testwait.Eventually(t, "the intake's take-over to log its bind", func() bool {
		return loggedMessage(logs(), "dispatch delivery: NATS consumer bound")
	})
	for pass := 1; pass <= 2; pass++ {
		checks.Advance(intakeHoldRenew)
		var record map[string]any
		testwait.Until(t, fmt.Sprintf("the intake's check %d of its durable logged", pass), func() (string, bool) {
			records := logs()
			for _, logged := range records {
				if logged["msg"] == DurableChecked && logged["pass"] == float64(pass) {
					record = logged
					return "logged", true
				}
			}
			return fmt.Sprintf("the messages logged: %q", loggedMessages(records)), false
		})
		if record["level"] != "DEBUG" || record["name"] != bus.DispatchDeliveryDurable || record["found"] != checkDeliversHere {
			t.Fatalf("the intake logged its check %d as %v, want it at DEBUG naming %s and finding %q", pass, record, bus.DispatchDeliveryDurable, checkDeliversHere)
		}
	}
}
