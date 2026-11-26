//go:build odatrace

package hooktest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Each test below holds the hooked server at one hold point and shows the hold is real: the
// point is reached, what the server was about to do there does not happen while it is held, and
// it happens once the hold is released. Each runs its control first, the same action unheld, so
// the reading that sees nothing while the hold parks the server is one that does see the action
// land. Removing the point from the hooked server (forced-order.sh -drop <point>) fails the test,
// at the wait for the hold to be reached.

// held is how long a test watches for what a hold keeps from happening, and reachWithin how long
// it waits for a call or a receive the release lets happen.
const (
	held        = 500 * time.Millisecond
	reachWithin = 30 * time.Second
)

// heldConsumer is a lone server's stream HOLD with three messages, and C, a durable pull consumer
// of it whose acks a test holds on their way to C's state file.
type heldConsumer struct {
	srv   *testnats.Server
	hooks *testnats.Hooks
	sub   *natsgo.Subscription
}

func newHeldConsumer(t *testing.T) *heldConsumer {
	t.Helper()
	srv, uri := testnats.Start(t)
	hooks := testnats.Hook(t)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(natsgo.MaxWait(reachWithin))
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	if _, err := js.AddStream(&natsgo.StreamConfig{Name: "HOLD", Subjects: []string{"hold.>"}}); err != nil {
		t.Fatalf("create the stream: %v", err)
	}
	for i := range 3 {
		if _, err := js.Publish(fmt.Sprintf("hold.%d", i+1), nil); err != nil {
			t.Fatalf("publish message %d: %v", i+1, err)
		}
	}
	if _, err := js.AddConsumer("HOLD", &natsgo.ConsumerConfig{Durable: "C", AckPolicy: natsgo.AckExplicitPolicy, AckWait: time.Minute}); err != nil {
		t.Fatalf("create the consumer: %v", err)
	}
	sub, err := js.PullSubscribe("", "C", natsgo.Bind("HOLD", "C"))
	if err != nil {
		t.Fatalf("bind the consumer: %v", err)
	}
	return &heldConsumer{srv: srv, hooks: hooks, sub: sub}
}

// ackNext fetches C's next message and acks it, waiting for the server to take the ack.
func (c *heldConsumer) ackNext(t *testing.T) {
	t.Helper()
	msgs, err := c.sub.Fetch(1, natsgo.MaxWait(5*time.Second))
	if err != nil || len(msgs) != 1 {
		t.Fatalf("fetch C's next message: %d messages, %v", len(msgs), err)
	}
	if err := msgs[0].AckSync(natsgo.AckWait(5 * time.Second)); err != nil {
		t.Fatalf("ack C's message: %v", err)
	}
}

// state is C's state as the file named file in its directory holds it, or nil while there is no
// such file or it does not decode (v2.10.29 writes o.dat in place, so a read can meet it half
// written).
func (c *heldConsumer) state(t *testing.T, file string) *natsserver.ConsumerState {
	t.Helper()
	buf, err := os.ReadFile(filepath.Join(c.srv.StoreDir(), "jetstream", "$G", "streams", "HOLD", "obs", "C", file))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read C's %s: %v", file, err)
	}
	state, err := natsserver.MeasureDecodeConsumerState(buf)
	if err != nil {
		return nil
	}
	return state
}

// awaitFloor waits for C's file to hold the ack floor floor. The wait ends on that state, bounded
// only by the test binary's deadline (testwait.UntilPolling): the server's flusher writes the file
// after the ack lands, which a loaded machine delays without anything being wrong, so a fixed span
// (reachWithin, before) failed the row when the write came late. It reads the file every
// floorPoll.
func (c *heldConsumer) awaitFloor(t *testing.T, file string, floor uint64) {
	t.Helper()
	testwait.UntilPolling(t, floorPoll, fmt.Sprintf("C's %s to reach ack floor %d", file, floor), func() (string, bool) {
		state := c.state(t, file)
		return fmt.Sprintf("%+v", state), state != nil && state.AckFloor.Stream == floor
	})
}

// floorPoll is how often awaitFloor reads C's file: the 20 ms its fixed-span loop read it at
// before.
const floorPoll = 20 * time.Millisecond

// staysBelow watches C's file for held, and fails when it reaches the ack floor floor.
func (c *heldConsumer) staysBelow(t *testing.T, file string, floor uint64) {
	t.Helper()
	for end := time.Now().Add(held); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if state := c.state(t, file); state != nil && state.AckFloor.Stream >= floor {
			t.Fatalf("C's %s reached ack floor %d while the write was held: %+v", file, state.AckFloor.Stream, state)
		}
	}
}

// ackFloor takes the events carrying a consumer state whose ack floor is floor.
func ackFloor(floor uint64) testnats.Match {
	return testnats.Where(fmt.Sprintf("ack floor %d", floor), func(e testnats.Event) bool {
		return e.State != nil && e.State.AckFloor.Stream == floor
	})
}

func TestForcedOrderHoldsAStateWriteBeforeItsIO(t *testing.T) {
	c := newHeldConsumer(t)
	// Control: an unheld ack reaches o.dat, so the reading below sees a write land.
	c.ackNext(t)
	c.awaitFloor(t, "o.dat", 1)

	hold := c.hooks.Hold(testnats.WritePreIO, testnats.OnServer(c.srv), testnats.Consumer("C"), ackFloor(2))
	c.ackNext(t)
	t.Logf("held %s", hold.Reached(t))
	c.staysBelow(t, "o.dat", 2)
	hold.Release()
	c.awaitFloor(t, "o.dat", 2)
}

func TestForcedOrderHoldsAStateWriteBeforeItsRename(t *testing.T) {
	c := newHeldConsumer(t)
	c.ackNext(t)
	c.awaitFloor(t, "o.dat", 1)

	if ok, why := testnats.WritePreRename.Exists(); !ok {
		// nats-server v2.10.29 has no rename to hold before: it writes o.dat itself. Show that,
		// so a later version that brings the rename back fails here until its point is added.
		mark := c.hooks.Mark()
		c.ackNext(t)
		c.awaitFloor(t, "o.dat", 2)
		c.hooks.Await(t, testnats.Point("write.written"), testnats.OnServer(c.srv), testnats.Consumer("C"), ackFloor(2), testnats.After(mark))
		if state := c.state(t, "o.dat.tmp"); state != nil {
			t.Fatalf("nats-server %s left o.dat.tmp, holding %+v: it writes through a temporary file after all", natsserver.VERSION, state)
		}
		t.Logf("nats-server %s has no %s: %s", natsserver.VERSION, testnats.WritePreRename, why)
		return
	}
	hold := c.hooks.Hold(testnats.WritePreRename, testnats.OnServer(c.srv), testnats.Consumer("C"), ackFloor(2))
	c.ackNext(t)
	t.Logf("held %s", hold.Reached(t))
	if state := c.state(t, "o.dat.tmp"); state == nil || state.AckFloor.Stream != 2 {
		t.Fatalf("o.dat.tmp holds %+v while the rename is held, want the new state, ack floor 2", state)
	}
	c.staysBelow(t, "o.dat", 2)
	hold.Release()
	c.awaitFloor(t, "o.dat", 2)
}

func TestForcedOrderHoldsAFlushWithItsStateEncoded(t *testing.T) {
	c := newHeldConsumer(t)
	c.ackNext(t)
	c.awaitFloor(t, "o.dat", 1)

	hold := c.hooks.Hold(testnats.FlushEncoded, testnats.OnServer(c.srv), testnats.Consumer("C"), ackFloor(2))
	c.ackNext(t)
	t.Logf("held %s", hold.Reached(t))
	c.staysBelow(t, "o.dat", 2)
	hold.Release()
	c.awaitFloor(t, "o.dat", 2)
}

// answer runs request on a goroutine of its own and hands back its error. A held request carries
// no timer of its own but the test's deadline (testwait.Context, testnats.MaxWait): a request
// bounded by a span of its own times out at the client while its hold still holds once the test
// runs late, and that timeout comes back here like an answer.
func answer(request func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- request() }()
	return done
}

// timedOut reports whether err is the client giving up on its request, not the server answering
// it: a held request's one bound is the test's deadline, so this is the test running out of time
// while the hold held, and a row says so rather than call it an answer.
func timedOut(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, natsgo.ErrTimeout)
}

// answeredWhileHeld fails t for err, which a held request's channel gave while the row required
// no answer: as an answer, or as the client giving up with no answer (timedOut), which with the
// request bounded by the test's deadline means the test ran out of time while the hold held.
func answeredWhileHeld(t *testing.T, what string, err error) {
	t.Helper()
	if timedOut(err) {
		t.Fatalf("%s: the client gave up on it while held, with no answer from the server (%v)", what, err)
	}
	t.Fatalf("%s answered while held (%v)", what, err)
}

// answered fails t unless done answers nil. It waits for the answer until the test binary's
// deadline (testwait.Receive).
func answered(t *testing.T, done <-chan error, what string) {
	t.Helper()
	if err := testwait.Receive(t, what, done); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// unanswered fails t when done answers within held.
func unanswered(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		answeredWhileHeld(t, what, err)
	case <-time.After(held):
	}
}

// heldStream is a lone server with the stream HOLD, and a JetStream client of it whose requests
// wait until the test's deadline (testnats.MaxWait), so a held one never times out short of it.
func heldStream(t *testing.T) (*testnats.Server, *testnats.Hooks, *natsgo.Conn, natsgo.JetStreamContext) {
	t.Helper()
	srv, uri := testnats.Start(t)
	hooks := testnats.Hook(t)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	if _, err := js.AddStream(&natsgo.StreamConfig{Name: "HOLD", Subjects: []string{"hold.>"}}); err != nil {
		t.Fatalf("create the stream: %v", err)
	}
	return srv, hooks, conn, js
}

func TestForcedOrderHoldsAConsumerStoreBeforeItLoads(t *testing.T) {
	srv, hooks, _, js := heldStream(t)
	create := func(name string) <-chan error {
		return answer(func() error {
			_, err := js.AddConsumer("HOLD", &natsgo.ConsumerConfig{Durable: name, AckPolicy: natsgo.AckExplicitPolicy})
			return err
		})
	}
	answered(t, create("M"), "the unheld create of M")

	hold := hooks.Hold(testnats.StoreCreate, testnats.OnServer(srv), testnats.Consumer("N"))
	done := create("N")
	t.Logf("held %s", hold.Reached(t))
	unanswered(t, done, "the create of N")
	hold.Release()
	answered(t, done, "the create of N")
}

func TestForcedOrderHoldsAnAPIRequestBeforeItIsServed(t *testing.T) {
	srv, hooks, conn, _ := heldStream(t)
	const subject = "$JS.API.STREAM.INFO.HOLD"
	info := func() <-chan error {
		ctx := testwait.Context(t)
		return answer(func() error {
			_, err := conn.RequestWithContext(ctx, subject, nil)
			return err
		})
	}
	answered(t, info(), "the unheld stream info")

	// A lone server's clients are in $G; the request reaches the API in the system account, so
	// the account matched here is the one the request's own header names.
	hold := hooks.Hold(testnats.APIWorker, testnats.OnServer(srv), testnats.InAccount("$G"), testnats.Subject(subject))
	done := info()
	t.Logf("held %s", hold.Reached(t))
	unanswered(t, done, "the stream info")
	hold.Release()
	answered(t, done, "the stream info")
}

// stopWithin bounds the wait for a Stop of a server one of whose goroutines a hold parks. A stop
// that does not release the hold waits for that goroutine for ever, so the bound is what turns
// the hang into this test's failure.
const stopWithin = 20 * time.Second

// A server's shutdown releases the hold that parks one of its goroutines before it waits for its
// goroutines, so a Stop returns with an operation held: the held request then ends, answered or
// failed. Once the server is down, a hold on it parks again. Its control is the run without the
// release (forced-order.sh -drop server.shutdown): the Stop waits for the parked goroutine and the
// test fails at stopWithin.
func TestForcedOrderAStopReleasesTheHoldOnItsServer(t *testing.T) {
	srv, hooks, conn, _ := heldStream(t)
	const subject = "$JS.API.STREAM.INFO.HOLD"
	request := func(conn *natsgo.Conn) <-chan error {
		ctx := testwait.Context(t)
		return answer(func() error {
			_, err := conn.RequestWithContext(ctx, subject, nil)
			return err
		})
	}
	answered(t, request(conn), "the unheld stream info")

	hold := hooks.Hold(testnats.APIWorker, testnats.OnServer(srv), testnats.InAccount("$G"), testnats.Subject(subject))
	done := request(conn)
	t.Logf("held %s", hold.Reached(t))
	stopped := make(chan struct{})
	go func() {
		srv.Stop(t)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(stopWithin):
		t.Fatalf("the server's Stop did not return within %s while a hold parked one of its goroutines", stopWithin)
	}
	if !hold.Released() {
		t.Fatal("the Stop returned and the hold is not released")
	}
	hooks.Await(t, testnats.ServerDown, testnats.OnServer(srv))
	// The stop ends the held request: nats.go answers it with an error when the server closes the
	// connection ("nats: connection closed" on v2.15.1). A request the stop left waiting is ended by
	// whichever comes first of two ends at the same moment, the test's deadline less testwait's
	// margin: Receive gives up there and fails the row naming this wait, or the request's own context
	// (testwait.Context) ends and the row fails below as a timeout. Either way the row fails.
	if err := testwait.Receive(t, "the held request to end with the stop", done); timedOut(err) {
		t.Fatalf("the held request did not end with the stop: it timed out at the test's deadline (%v)", err)
	} else {
		t.Logf("the held request ended with the stop: %v", err)
	}

	// The server is down, so the hook parks its goroutines again once it is started.
	srv.Start(t)
	again := testnats.Connect(t, srv.URL())
	t.Cleanup(again.Close)
	second := hooks.Hold(testnats.APIWorker, testnats.OnServer(srv), testnats.InAccount("$G"), testnats.Subject(subject),
		testnats.After(hooks.Mark()))
	done = request(again)
	t.Logf("held after the restart %s", second.Reached(t))
	unanswered(t, done, "the stream info after the restart")
	second.Release()
	answered(t, done, "the stream info after the restart")
}

// On v2.10.29 two concurrent Shutdown calls can each report server.shutdown, and the first can
// reach the hook after the second has finished the shutdown and reported server.down. That late
// report belongs to a run that is over: after a restart on the same store, holds on the new run
// must still park. The test delays the first report until the second call has returned, then
// restarts the server on its store and holds a request on it. Before the harness kept its
// shutdown state by run, the late report marked the store stopping for good and the request was
// answered through the armed hold. On v2.15.1 Shutdown reports once, and the test checks that.
func TestForcedOrderALateShutdownOfAnEndedRunHoldsNothing(t *testing.T) {
	store := t.TempDir()
	start := func() *natsserver.Server {
		srv, err := natsserver.NewServer(&natsserver.Options{
			Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: store, NoLog: true, NoSigs: true,
		})
		if err != nil {
			t.Fatalf("create the server: %v", err)
		}
		t.Cleanup(srv.Shutdown)
		go srv.Start()
		if !srv.ReadyForConnections(reachWithin) {
			t.Fatal("the server did not accept connections")
		}
		return srv
	}
	first := start()
	hooks := testnats.Hook(t)
	original := natsserver.MeasureHook.Load()
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	t.Cleanup(unblock)
	var calls atomic.Int32
	delayFirst := func(event *natsserver.MeasureEvent) {
		if event.Point == string(testnats.ServerShutdown) && event.ServerID == first.ID() && calls.Add(1) == 1 {
			close(entered)
			<-resume
		}
		(*original)(event)
	}
	natsserver.MeasureHook.Store(&delayFirst)
	t.Cleanup(func() { natsserver.MeasureHook.Store(original) })

	finished := make(chan struct{})
	go func() {
		first.Shutdown()
		close(finished)
	}()
	select {
	case <-entered:
	case <-time.After(reachWithin):
		t.Fatal("the first Shutdown did not report server.shutdown")
	}
	// The second call runs while the first one's report waits.
	first.Shutdown()
	unblock()
	select {
	case <-finished:
	case <-time.After(reachWithin):
		t.Fatal("the delayed Shutdown did not return")
	}
	natsserver.MeasureHook.Store(original)

	runs := hooks.Events(testnats.ServerShutdown, testnats.Where("the first run", func(e testnats.Event) bool { return e.ServerID == first.ID() }))
	downs := hooks.Events(testnats.ServerDown, testnats.Where("the first run", func(e testnats.Event) bool { return e.ServerID == first.ID() }))
	for _, e := range append(runs, downs...) {
		t.Logf("first run: %s", e)
	}
	if len(downs) != 1 {
		t.Fatalf("the first run reported server.down %d times, want once", len(downs))
	}
	switch natsserver.VERSION {
	case "2.10.29":
		// The control that the interleaving under test happened: a report after the run's down.
		if len(runs) != 2 || runs[1].N < downs[0].N {
			t.Fatalf("want two server.shutdown reports, the second after server.down #%d; got %v", downs[0].N, runs)
		}
	default:
		if len(runs) != 1 {
			t.Fatalf("nats-server %s reported server.shutdown %d times for one run, want once", natsserver.VERSION, len(runs))
		}
	}

	restarted := start()
	conn, err := natsgo.Connect(restarted.ClientURL(), natsgo.Timeout(time.Second))
	if err != nil {
		t.Fatalf("connect to the restarted server: %v", err)
	}
	t.Cleanup(conn.Close)
	const subject = "$JS.API.INFO"
	hold := hooks.Hold(testnats.APIWorker, testnats.Subject(subject))
	ctx := testwait.Context(t)
	done := answer(func() error {
		_, err := conn.RequestWithContext(ctx, subject, nil)
		return err
	})
	// The request reaches api.worker either way; with the store left stopping it is answered
	// through the hold rather than parked there.
	hooks.Await(t, testnats.APIWorker, testnats.Subject(subject))
	select {
	case err := <-done:
		if timedOut(err) {
			answeredWhileHeld(t, "the request on the restarted server", err)
		}
		t.Fatalf("the restarted server answered through the armed hold (%v): a late server.shutdown of the ended run left its store stopping", err)
	case <-time.After(held):
	}
	t.Logf("held on the restarted server %s", hold.Reached(t))
	hold.Release()
	answered(t, done, "the request on the restarted server")
}

// Two runs that report the same store, as two servers with no store directory do, stop at
// overlapping times: A's shutdown begins, B's begins, B's ends. A is still inside its Shutdown,
// which waits for its goroutines, so a hold must not park one of them. The test sends the hook
// those reports itself, since two real servers cannot be made to interleave there on demand,
// then a publish on that store: it must pass the armed hold while A is stopping, and park once
// A is down. With the stopping mark kept by store, B's down cleared A's mark and the publish
// parked.
func TestForcedOrderTwoRunsOnOneStoreKeepTheirOwnShutdowns(t *testing.T) {
	hooks := testnats.Hook(t)
	report := *natsserver.MeasureHook.Load()
	const store, subject = "", "shared.store"
	hold := hooks.Hold(testnats.ClientPublish, testnats.Subject(subject))
	publish := func() <-chan struct{} {
		returned := make(chan struct{})
		go func() {
			report(&natsserver.MeasureEvent{Point: string(testnats.ClientPublish), Server: "A", Store: store, Subject: subject})
			close(returned)
		}()
		return returned
	}

	report(&natsserver.MeasureEvent{Point: string(testnats.ServerShutdown), Server: "A", ServerID: "run-a", Store: store})
	report(&natsserver.MeasureEvent{Point: string(testnats.ServerShutdown), Server: "B", ServerID: "run-b", Store: store})
	report(&natsserver.MeasureEvent{Point: string(testnats.ServerDown), Server: "B", ServerID: "run-b", Store: store})
	select {
	case <-publish():
	case <-time.After(reachWithin):
		t.Fatal("a publish on a store whose run A is still stopping was parked: B's down cleared A's mark")
	}

	report(&natsserver.MeasureEvent{Point: string(testnats.ServerDown), Server: "A", ServerID: "run-a", Store: store})
	returned := publish()
	t.Logf("held once A is down %s", hold.Reached(t))
	select {
	case <-returned:
		t.Fatal("the held publish returned before its release")
	case <-time.After(held):
	}
	hold.Release()
	select {
	case <-returned:
	case <-time.After(reachWithin):
		t.Fatal("the released publish did not return")
	}
}

// A hold released before anything reached it parks nothing, and takes no event away from a live
// hold armed after it: the first matching event parks on the live hold. The test sends the hook
// the publishes itself. Its control is the same publish with one live hold and no released one.
// Before a released hold stepped aside, it took the first event, which passed through it, and
// the live hold parked only the second.
func TestForcedOrderAReleasedHoldTakesNoEventFromTheNextHold(t *testing.T) {
	const subject = "released.hold"
	for _, released := range []bool{false, true} {
		name := "one live hold parks the first event"
		if released {
			name = "a released hold, then a live one: the first event parks on the live one"
		}
		t.Run(name, func(t *testing.T) {
			hooks := testnats.Hook(t)
			report := *natsserver.MeasureHook.Load()
			if released {
				hooks.Hold(testnats.ClientPublish, testnats.Subject(subject)).Release()
			}
			live := hooks.Hold(testnats.ClientPublish, testnats.Subject(subject))
			publish := func() <-chan struct{} {
				returned := make(chan struct{})
				go func() {
					report(&natsserver.MeasureEvent{Point: string(testnats.ClientPublish), Subject: subject})
					close(returned)
				}()
				return returned
			}
			returned := publish()
			escaped := false
			select {
			case <-returned:
				// Send a second publish, so the live hold is reached and the test fails on the
				// escape rather than on a hold armed and never reached.
				escaped, returned = true, publish()
			case <-time.After(held):
			}
			t.Logf("held %s", live.Reached(t))
			live.Release()
			select {
			case <-returned:
			case <-time.After(reachWithin):
				t.Fatal("the released publish did not return")
			}
			if escaped {
				t.Fatal("the first publish passed the live hold: the released hold took it")
			}
		})
	}
}

// Two live holds A and B on one point: the first matching event parks on A, and a test that
// releases A as soon as A's Reached returns lets that event go on, not over to B, which stays for
// the next event. The release has to land after the hook closes A's reached channel and before
// it decides whether A was released, which no sleep can promise, so the test makes the hook wait
// at that instant (testnats.SetOnReached on A) until A's Reached has returned and A is released.
// While the hook read A's release after it had published A as reached, it took A for one
// released before it was reached and parked the same event on B.
func TestForcedOrderAHoldReleasedAsItIsReachedKeepsItsEvent(t *testing.T) {
	const subject = "released.on.reach"
	hooks := testnats.Hook(t)
	report := *natsserver.MeasureHook.Load()
	a := hooks.Hold(testnats.ClientPublish, testnats.Subject(subject))
	b := hooks.Hold(testnats.ClientPublish, testnats.Subject(subject))
	releasedA := make(chan struct{})
	testnats.SetOnReached(a, func() {
		select {
		case <-releasedA:
		case <-time.After(reachWithin):
		}
	})
	publish := func() <-chan struct{} {
		returned := make(chan struct{})
		go func() {
			report(&natsserver.MeasureEvent{Point: string(testnats.ClientPublish), Subject: subject})
			close(returned)
		}()
		return returned
	}

	first := publish()
	took := a.Reached(t)
	a.Release()
	close(releasedA)
	select {
	case <-first:
	case <-time.After(reachWithin):
		t.Fatalf("the first publish, %s, did not return once A was released: it parked on B", took)
	}

	second := publish()
	next := b.Reached(t)
	if next.N <= took.N {
		t.Fatalf("B took %s, the event A took (%s), want the next publish", next, took)
	}
	b.Release()
	select {
	case <-second:
	case <-time.After(reachWithin):
		t.Fatal("the second publish did not return once B was released")
	}
}

func TestForcedOrderHoldsAPublishBeforeItIsMatched(t *testing.T) {
	srv, uri := testnats.Start(t)
	hooks := testnats.Hook(t)
	publisher, err := testnats.ConnectSeed(t, uri, "", natsgo.Name("publisher"))
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(publisher.Close)
	subscriber := testnats.Connect(t, uri)
	t.Cleanup(subscriber.Close)
	sub, err := subscriber.SubscribeSync("hold.pub")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := subscriber.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	receive := func(within time.Duration) (string, error) {
		msg, err := sub.NextMsg(within)
		if err != nil {
			return "", err
		}
		return string(msg.Data), nil
	}
	if err := publisher.Publish("hold.pub", []byte("first")); err != nil {
		t.Fatalf("publish the unheld message: %v", err)
	}
	if got, err := receive(5 * time.Second); err != nil || got != "first" {
		t.Fatalf("the unheld publish delivered %q, %v; want first", got, err)
	}

	hold := hooks.Hold(testnats.ClientPublish, testnats.OnServer(srv), testnats.Subject("hold.pub"), testnats.ClientNamed("publisher"))
	// No flush: the publisher's read loop is the goroutine the hold parks, so a PING after the
	// publish would wait for the release too.
	if err := publisher.Publish("hold.pub", []byte("second")); err != nil {
		t.Fatalf("publish the held message: %v", err)
	}
	t.Logf("held %s", hold.Reached(t))
	if got, err := receive(held); err == nil {
		t.Fatalf("the held publish was delivered (%q) while held", got)
	}
	hold.Release()
	msg, err := sub.NextMsgWithContext(testwait.Context(t))
	if err != nil || string(msg.Data) != "second" {
		t.Fatalf("the released publish delivered %v, %v; want second, by the test's deadline", msg, err)
	}
}

// Every member of an R3 cluster applies a consumer's assignment from the meta layer before it
// creates the consumer. Held on all three, the create gets no answer; released, it is answered.
func TestForcedOrderHoldsAConsumerAssignmentBeforeItsServersApplyIt(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	hooks := testnats.Hook(t)
	conn := testnats.Connect(t, cluster.URL())
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	if _, err := js.AddStream(&natsgo.StreamConfig{Name: "META", Subjects: []string{"meta.>"}, Replicas: 3}); err != nil {
		t.Fatalf("create the stream: %v", err)
	}
	create := func(name string) <-chan error {
		return answer(func() error {
			_, err := js.AddConsumer("META", &natsgo.ConsumerConfig{Durable: name, AckPolicy: natsgo.AckExplicitPolicy})
			return err
		})
	}
	answered(t, create("M"), "the unheld create of M")

	holds := make([]*testnats.Hold, len(cluster.Servers))
	for i, srv := range cluster.Servers {
		holds[i] = hooks.Hold(testnats.MetaApply, testnats.OnServer(srv), testnats.Stream("META"), testnats.Consumer("N"))
	}
	done := create("N")
	for _, hold := range holds {
		t.Logf("held %s", hold.Reached(t))
	}
	// No member may answer the create while each is parked before it applies the assignment. No
	// fact orders that answer against a read here: each member applies the meta layer's entries in
	// order, so a later request on the same path (another consumer's create) waits behind the held
	// one and cannot serve as a barrier. So the row gives the answer a window it must not win,
	// assignedWindow. A hold that holds answers never, and the create's one bound is the test's
	// deadline (testnats.MaxWait above), so a correct run fails here only when that deadline comes
	// inside the window, and then as the client's timeout, not as an answer (answeredWhileHeld).
	// The window's cost is to miss a broken hold that answers later than it. A read of the channel
	// once, with no window, missed such a hold every time: a hook that reports each point reached
	// and parks nothing let the answer through after the read.
	select {
	case err := <-done:
		answeredWhileHeld(t, "the create of N", err)
	case <-time.After(assignedWindow):
	}
	for _, hold := range holds {
		hold.Release()
	}
	answered(t, done, "the create of N")
}

// assignedWindow is how long TestForcedOrderHoldsAConsumerAssignmentBeforeItsServersApplyIt watches
// for an answer to the held create. Timed on a build Job of six CPUs, five runs each, the create is
// answered 57 to 59 ms after the holds are released, and, with a hook that parks nothing, 57 to
// 62 ms after the third hold is reached, so 5 s is about 80 times the longest; a CI runner is
// slower than that Job.
const assignedWindow = 5 * time.Second

func TestForcedOrderHoldsACommittedEntryBeforeItsLeaderStoresIt(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	hooks := testnats.Hook(t)
	conn := testnats.Connect(t, cluster.URL())
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	if _, err := js.AddStream(&natsgo.StreamConfig{Name: "APPLY", Subjects: []string{"apply.>"}, Replicas: 3}); err != nil {
		t.Fatalf("create the stream: %v", err)
	}
	// Control: an unheld publish is acknowledged, once the stream's leader has stored it.
	if _, err := js.Publish("apply.first", nil); err != nil {
		t.Fatalf("the unheld publish: %v", err)
	}
	info, err := js.StreamInfo("APPLY")
	if err != nil || info.Cluster == nil || cluster.Member(info.Cluster.Leader) < 0 {
		t.Fatalf("the stream's leader: %+v, %v", info, err)
	}
	leader := cluster.Servers[cluster.Member(info.Cluster.Leader)]

	hold := hooks.Hold(testnats.StreamApply, testnats.OnServer(leader), testnats.Stream("APPLY"), testnats.Subject("apply.second"))
	ack, err := js.PublishAsync("apply.second", nil)
	if err != nil {
		t.Fatalf("publish the held message: %v", err)
	}
	reached := hold.Reached(t)
	t.Logf("held %s", reached)
	if reached.Server != leader.Name() || reached.Seq != 2 {
		t.Fatalf("held %s, want stream sequence 2 on the leader, %s", reached, leader.Name())
	}
	select {
	case got := <-ack.Ok():
		t.Fatalf("the publish was acknowledged (sequence %d) while its leader's apply was held", got.Sequence)
	case err := <-ack.Err():
		t.Fatalf("the held publish failed: %v", err)
	case <-time.After(held):
	}
	hold.Release()
	select {
	case got := <-ack.Ok():
		if got.Sequence != 2 {
			t.Fatalf("the released publish was acknowledged at sequence %d, want 2", got.Sequence)
		}
	case err := <-ack.Err():
		t.Fatalf("the released publish failed: %v", err)
	case <-testwait.Context(t).Done():
		t.Fatal("the released publish was not acknowledged by the test's deadline")
	}
}

// A point no row above holds at is held to the same contract: msgget.timeSelected, the sequence a
// message get by start_time chose (v2.15.1 alone: 2.10.29's message get takes no start_time).
// Its event names its server, its store and its stream, so OnServer and Stream take it and the
// server's Stop releases the goroutine it parks. Before it carried its server and store, the
// hold was never reached on the server, and without OnServer it parked an API worker that the
// Stop then waited for; before it named its stream as Stream rather than as a consumer, a hold
// by Stream was never reached.
func TestForcedOrderAStopReleasesAHoldAtATimeSelectedMessageGet(t *testing.T) {
	point := testnats.Point("msgget.timeSelected")
	if ok, why := point.Exists(); !ok {
		t.Skipf("nats-server %s has no %s: %s", natsserver.VERSION, point, why)
	}
	srv, hooks, conn, js := heldStream(t)
	if _, err := js.Publish("hold.1", nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	const subject = "$JS.API.STREAM.MSG.GET.HOLD"
	body := []byte(fmt.Sprintf(`{"start_time":%q}`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)))
	get := func() <-chan error {
		ctx := testwait.Context(t)
		return answer(func() error {
			_, err := conn.RequestWithContext(ctx, subject, body)
			return err
		})
	}
	answered(t, get(), "the unheld message get by start time")

	hold := hooks.Hold(point, testnats.OnServer(srv), testnats.Stream("HOLD"))
	done := get()
	reached := hold.Reached(t)
	t.Logf("held %s", reached)
	if reached.Stream != "HOLD" || reached.Consumer != "" {
		t.Fatalf("held %s, want the stream HOLD and no consumer", reached)
	}
	unanswered(t, done, "the message get by start time")
	stopped := make(chan struct{})
	go func() {
		srv.Stop(t)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(stopWithin):
		t.Fatalf("the server's Stop did not return within %s while a hold at %s parked one of its goroutines", stopWithin, point)
	}
	if !hold.Released() {
		t.Fatal("the Stop returned and the hold is not released")
	}
}
