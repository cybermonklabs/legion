package outbox

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/duty/dutytest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// process is one envoy-dispatch process's outbox under test: its own pool, its own events broker
// and its own NATS client, which counts every envelope it sends.
type process struct {
	name    string
	outbox  *Outbox
	store   *store.Store
	broker  *events.Broker
	sending *sendCounter
	stop    func()
}

// sendCounter counts the envelopes every process of a test sends, by event and subject.
type sendCounter struct {
	mu    sync.Mutex
	sends map[string]int
}

func (c *sendCounter) add(item contracts.Envelope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sends == nil {
		c.sends = map[string]int{}
	}
	c.sends[item.SourceEventID+" "+item.Topic]++
}

// of is how many envelopes of event were sent, and the most sent to any one subject.
func (c *sendCounter) of(event int64) (total, most int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, count := range c.sends {
		if strings.HasPrefix(key, fmt.Sprint(event)+" ") {
			total += count
			most = max(most, count)
		}
	}
	return total, most
}

// countingClient is a bus client whose windows count every envelope they send.
type countingClient struct {
	*bus.Client
	counter *sendCounter
}

func (c countingClient) WindowUntil(ctx context.Context, deadline func() time.Time) bus.Sender {
	return countingSender{Sender: c.Client.WindowUntil(ctx, deadline), counter: c.counter}
}

type countingSender struct {
	bus.Sender
	counter *sendCounter
}

func (s countingSender) PublishAsync(item contracts.Envelope) <-chan error {
	s.counter.add(item)
	return s.Sender.PublishAsync(item)
}

// startProcess runs an outbox on database's own outbox pool, publishing through a client of its own
// connected to url with options, until the test ends or stop is called.
func startProcess(t *testing.T, name string, database *store.Store, url string, counter *sendCounter, options ...bus.ConnectOption) *process {
	t.Helper()
	client, err := bus.ConnectOwningStream([]string{url}, options...)
	if err != nil {
		t.Fatalf("connect %s's NATS client: %v", name, err)
	}
	t.Cleanup(client.Close)
	pool, err := database.Pool.Outbox()
	if err != nil {
		t.Fatalf("open %s's outbox pool: %v", name, err)
	}
	p := &process{name: name, store: database, broker: events.NewBroker(), sending: counter}
	p.outbox = New(Deps{
		Store:     &store.Store{Pool: pool},
		Publisher: countingClient{Client: client, counter: counter},
		Broker:    p.broker,
		Listen:    database.Pool.Listener,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.outbox.Run(ctx); close(done) }()
	p.stop = sync.OnceFunc(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("%s's outbox did not stop", name)
		}
	})
	t.Cleanup(p.stop)
	return p
}

// publishing is the processes that publish now.
func publishing(processes ...*process) []string {
	var names []string
	for _, p := range processes {
		if p.outbox.Publishing() {
			names = append(names, p.name)
		}
	}
	return names
}

// waitForOnePublisher waits until exactly one of processes publishes, and returns it.
func waitForOnePublisher(t *testing.T, within time.Duration, processes ...*process) *process {
	t.Helper()
	var holder *process
	waitFor(t, within, "exactly one outbox publishing", func() bool {
		holder = nil
		count := 0
		for _, p := range processes {
			if p.outbox.Publishing() {
				holder = p
				count++
			}
		}
		return count == 1
	})
	return holder
}

// writeMessage commits one message event on issue T-1 through p's store and broker, as a request to
// p would, and returns it.
func writeMessage(t *testing.T, p *process, body string) model.Event {
	t.Helper()
	event := appendEvent(t, p.store, p.broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: new("T-1"), Body: body},
	})
	p.broker.Publish(event)
	return event
}

// waitForPublished waits until event is marked published and was sent to every subject once.
func waitForPublished(t *testing.T, within time.Duration, database *store.Store, counter *sendCounter, event model.Event) {
	t.Helper()
	waitFor(t, within, fmt.Sprintf("event %d published", event.ID), func() bool {
		return publishedAt(t, database, event.ID) != nil
	})
	if total, most := counter.of(event.ID); total == 0 || most != 1 {
		t.Fatalf("event %d: %d envelopes sent, at most %d to one subject, want each of its subjects sent once", event.ID, total, most)
	}
}

// outboxHangGuard bounds every wait the row below makes. It is a hang guard, not a budget: the row
// asserts what the outbox did and in what order, never how fast it did it, so load only makes the
// same reading take longer to reach, and a wait that reaches this bound means the thing it waited
// for never happened at all.
const outboxHangGuard = 60 * time.Second

// backOffDue is how long the back-off of the row below is due in, and the one bound the row puts
// on the outbox's own work: the insert's notification must reach the holder, and the scan it
// wakes must read next_attempt_at, inside it. A scan later than that finds the back-off already
// due and publishes it there and then, arming no wait at all, which the row catches where it
// requires exactly one wait armed between the insert and the publish - so it still fails on what
// happened rather than on a clock, but it would be failing for the machine's slowness and not
// the outbox's, which is why this is ten seconds against the single-digit milliseconds that work
// takes on an idle pod.
const backOffDue = 10 * time.Second

// An idle outbox scans nothing: no claim runs while no event is written and no retry falls due.
// An event another process commits - which reaches this one only through the database, not its
// broker - is claimed at once by the LISTEN on dispatch_events, and a back-off is claimed when it
// falls due, by the one timer armed to it, with nothing else waking the outbox in between. Every
// step is read from what the outbox and the database recorded - the waits armed, the claims made,
// the moment the database marked the event published - and never from how long a step took.
func TestTheOutboxDoesNotScanWhileIdle(t *testing.T) {
	database := storetest.Open(t)
	other := storetest.SecondProcess(t, database)
	seedIssue(t, database, "T-1", nil)
	publisher := &recordingPublisher{}
	outbox := New(Deps{Store: database, Publisher: publisher, Broker: events.NewBroker(), Listen: database.Pool.Listener})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// Every wait the outbox arms its one retry timer for, with the moment it was armed and the
	// claims the outbox had made by then: the sequence this row reads, in place of a wall clock.
	type armedWait struct {
		wait   time.Duration
		at     time.Time
		claims int64
	}
	var armedMu sync.Mutex
	var armed []armedWait
	outbox.afterRetryArmed = func(wait time.Duration) {
		armedMu.Lock()
		defer armedMu.Unlock()
		armed = append(armed, armedWait{wait: wait, at: time.Now(), claims: outbox.claims.Load()})
	}
	// armedWaits copies what has been armed so far, so nothing reads the slice unlocked.
	armedWaits := func() []armedWait {
		armedMu.Lock()
		defer armedMu.Unlock()
		return slices.Clone(armed)
	}
	// restingAt is the claims the outbox had made when the holder last entered its wait with
	// nothing pending and the LISTEN connection already up, and -1 until it enters one. A wait
	// entered with a wake pending records nothing, because the scan that wake causes is still to
	// come; a wait entered before that connection is up records nothing either, because the
	// connection's own catch-up scan is still to come and would land inside the idle window
	// below. The holder stores this before it waits and scans only after, so while restingAt
	// equals the claims made the holder is waiting and nothing has scanned since - a state that
	// holds for as long as the outbox has nothing to do, rather than an edge a reader can arrive
	// too late to see.
	//
	// Both hooks take restMu, and the wait's own queue is read under it rather than handed in,
	// because the two facts have to be read together: the holder could otherwise record a rest
	// in the gap between the flag going up and the wake that follows it, and the scan that wake
	// causes would then land inside the window. Under one lock a wait that runs before the
	// connection is reported is refused by the flag, and one that runs while that wake is still
	// queued is refused by the queue. A wait after the wake's own scan finds the queue empty and
	// records a rest, which is the rest this row is for: by then that scan is one of the claims
	// restingAt is compared with.
	var restMu sync.Mutex
	listening := false
	var restingAt atomic.Int64
	restingAt.Store(-1)
	outbox.afterListening = func() {
		restMu.Lock()
		defer restMu.Unlock()
		listening = true
		// One wake more, so the holder is certain to enter a wait after the flag is set.
		// OnConnected signals before it reports, so the holder may have taken that connection's
		// own wake, scanned and parked again while the flag was still false.
		outbox.signal()
	}
	outbox.afterServeWait = func() {
		restMu.Lock()
		defer restMu.Unlock()
		if listening && len(outbox.wake) == 0 {
			restingAt.Store(outbox.claims.Load())
		}
	}
	go func() { outbox.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	// atRest waits for the holder to be waiting with nothing pending and nothing scanned since,
	// a wait it entered with the LISTEN connection already up. That is this row's "the outbox has
	// nothing left to do": every catch-up scan - the hold's take, and that connection's - has
	// already run, and only a new event can wake the outbox again. This row's publisher never
	// disconnects, so such a wait is always one the holder reached with nothing to scan, never
	// one it reached with NATS down.
	atRest := func(what string) {
		t.Helper()
		// restingAt is read first, so the claims it is compared with are never older than it: an
		// equal pair is a wait no scan has followed, never a scan this read straddled.
		waitFor(t, outboxHangGuard, what, func() bool { return restingAt.Load() == outbox.claims.Load() })
	}

	waitFor(t, outboxHangGuard, "the outbox taking the publisher hold", outbox.Publishing)
	// The first scan and the LISTEN connection's catch-up run as the hold is taken; the row reads
	// that they are over rather than allowing them a span of time.
	atRest("the outbox resting after the hold's first scan and the LISTEN connection's catch-up")
	idleFrom := outbox.claims.Load()
	// A lower bound on quiet, not an upper bound on work: the row asserts nothing scanned in this
	// window, and load can only make an idle outbox quieter, never busier.
	time.Sleep(12 * time.Second)
	if claims := outbox.claims.Load() - idleFrom; claims != 0 {
		t.Fatalf("an idle outbox ran %d claims in 12 s, want 0", claims)
	}

	// Committed on another process's pool, and published to no broker this outbox hears.
	fresh := appendEvent(t, other, events.NewBroker(), model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "fresh", IssueKey: new("T-1"), Body: "fresh"},
	})
	// A local clock for the hang guards and the readings below alone: no assertion reads it.
	committed := time.Now()
	// The 12 s window above showed this outbox scans on nothing periodic, so a claim at all after
	// this commit is the notification's; the bound only stops the row hanging when no claim ever
	// comes.
	waitFor(t, outboxHangGuard, "the event another process committed claimed", func() bool {
		return outbox.claims.Load() != idleFrom
	})
	t.Logf("the event another process committed was claimed %s after its commit", time.Since(committed).Round(time.Millisecond))
	// The same hang guard: the claim above is this event's own scan, so the only way to this bound
	// is a publication that never happens at all.
	waitFor(t, outboxHangGuard, "the other process's event published", func() bool { return publishedAt(t, database, fresh.ID) != nil })
	// The sequence below starts from an outbox at rest: the wait above returns as soon as the
	// other process's event is marked published, while the batch that published it has still to
	// start the batch that follows it and the scan has still to arm whatever comes of that.
	atRest("the outbox resting after the other process's event")

	// A back-off due in backOffDue, committed by the other process: its insert wakes the outbox,
	// whose scan finds nothing due and arms its one retry timer to the back-off's own moment, and
	// the timer's scan is the one that publishes it. The waits armed earlier in this row are not
	// this one's.
	armedBefore := len(armedWaits())
	// The waits this back-off's own scan armed, read afresh each time: the slice only grows.
	waitsSince := func() []armedWait { return armedWaits()[armedBefore:] }
	// due is the moment the database computed for this back-off; every assertion about it below is
	// made against that value, on the database's own clock.
	backedOff, due := appendBackedOffEvent(t, other, backOffDue)
	// A local clock for this loop's hang guard and the readings below alone: no assertion reads it.
	inserted := time.Now()
	var published time.Time
	for {
		// Read oldest first, so nothing is judged against a reading younger than itself: the waits,
		// then the claims (never fewer than the waits' own count), then the database's answer,
		// whose now() is therefore no earlier than the moment those claims were read.
		since := waitsSince()
		claims := outbox.claims.Load()
		at, beforeDue := backOffState(t, database, backedOff.ID)
		if at != nil {
			published = *at
			break
		}
		// The one clock left in this loop is a hang guard, 60 s against the backOffDue the work
		// waits out, so no load reaches it: it fires only when nothing was published at all. A
		// hang guard needs no agreement between clocks, so the local one serves.
		if time.Since(inserted) > outboxHangGuard {
			t.Fatalf("the back-off due in %s was never published: %s after its insert the outbox had armed %v",
				backOffDue, time.Since(inserted).Round(time.Millisecond), waitsSince())
		}
		// Nothing else wakes the outbox until the timer fires: no claim since the wait was armed,
		// while the database says the back-off is still ahead on its own clock.
		if len(since) > 0 && beforeDue {
			if made := claims - since[0].claims; made != 0 {
				t.Fatalf("the outbox made %d claims in the %s since it armed a %s wait for the back-off, %s after the insert and before the back-off fell due, want none",
					made, time.Since(since[0].at).Round(time.Millisecond), since[0].wait.Round(time.Millisecond),
					since[0].at.Sub(inserted).Round(time.Millisecond))
			}
		}
		// How often this loop reads, bounding nothing: a coarser poll would only read the same
		// sequence later.
		time.Sleep(5 * time.Millisecond)
	}
	publishedAfter := time.Since(inserted)
	since := waitsSince()
	if len(since) != 1 {
		t.Fatalf("the outbox armed %d waits between the back-off's insert and its publish, want 1: %v", len(since), since)
	}
	// The wait is a value the database computed from its own next_attempt_at, not an elapsed time
	// this test measured, so a loaded runner can only make it smaller: the only way past
	// backOffDue is a wait armed for something that is not this back-off. The lower end is the
	// cheaper half of the same statement: nextRetry clamps a back-off already due to zero, so a
	// wait of zero is a timer armed to fire at once, which is not the timer this row is about.
	// A scan so late that the back-off is already due does not arm even that - it publishes the
	// back-off in that scan and arms nothing - and the assertion above, exactly one wait armed,
	// is what catches that.
	if since[0].wait <= 0 || since[0].wait > backOffDue {
		t.Fatalf("the outbox armed a %s wait for a back-off due in %s, want one above zero and no more than %s",
			since[0].wait, backOffDue, backOffDue)
	}
	// Exact where a margin was a guess: the publication time and the moment the back-off fell due
	// are both the database's own values, so no clock is compared with another and no load can
	// move either. A publication before that moment would be the outbox publishing a back-off that
	// was not yet due.
	if published.Before(due) {
		t.Fatalf("the back-off was published at %s, %s before the %s the database made it due, want no sooner than due",
			published, due.Sub(published).Round(time.Millisecond), due)
	}
	// A reading for the record, not an assertion: the publishing batch starts another batch, so
	// the scan the timer woke claims more than once.
	t.Logf("the outbox armed a %s wait %s after the insert, the back-off was published %s after it and %s after the database made it due, and the outbox made %d claims from the arming to the publish",
		since[0].wait.Round(time.Millisecond), since[0].at.Sub(inserted).Round(time.Millisecond),
		publishedAfter.Round(time.Millisecond), published.Sub(due).Round(time.Millisecond),
		outbox.claims.Load()-since[0].claims)
}

// appendBackedOffEvent commits an event on database whose next attempt is due after wait on the
// database's clock, as a back-off leaves it. It returns the event and the next_attempt_at the
// database computed: the moment it falls due, which only the database's own clock can be read
// against.
func appendBackedOffEvent(t *testing.T, database *store.Store, wait time.Duration) (model.Event, time.Time) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin event transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	event, err := events.NewBroker().Append(ctx, tx, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "backed-off", IssueKey: new("T-1"), Body: "backed off"},
	})
	if err != nil {
		t.Fatalf("append event: %v", err)
	}
	var due time.Time
	if err := tx.QueryRow(ctx, `update events set next_attempt_at = now() + make_interval(secs => $2), attempt_count = 1 where id = $1 returning next_attempt_at`,
		event.ID, wait.Seconds()).Scan(&due); err != nil {
		t.Fatalf("back the event off: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit event: %v", err)
	}
	return event, due
}

// backOffState reads whether the event has been published and whether its back-off is still
// ahead, both on the database's own clock and in one statement: next_attempt_at is a value the
// database computed, so only the database can say whether it has passed. A published event has no
// next_attempt_at left (the publisher clears it), and nothing ahead of the clock either.
func backOffState(t *testing.T, database *store.Store, eventID int64) (published *time.Time, beforeDue bool) {
	t.Helper()
	var ahead *bool
	if err := database.Pool.QueryRow(context.Background(),
		`select published_at, now() < next_attempt_at from events where id = $1`, eventID).Scan(&published, &ahead); err != nil {
		t.Fatalf("read the event's back-off state: %v", err)
	}
	return published, ahead != nil && *ahead
}

// Two processes' outboxes on one database: exactly one publishes at every moment, and every event
// either process writes is sent once, by whichever holds the publisher hold.
func TestOnlyOneOutboxPublishesAtATime(t *testing.T) {
	_, url := testnats.Start(t)
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	counter := &sendCounter{}
	first := startProcess(t, "first", database, url, counter)
	second := startProcess(t, "second", storetest.SecondProcess(t, database), url, counter)
	waitForOnePublisher(t, 10*time.Second, first, second)

	var written []model.Event
	deadline := time.Now().Add(10 * time.Second)
	for sample := 0; time.Now().Before(deadline); sample++ {
		if names := publishing(first, second); len(names) != 1 {
			t.Fatalf("sample %d: publishing %v, want exactly one outbox", sample, names)
		}
		if sample%10 == 0 {
			writer := []*process{first, second}[(sample/10)%2]
			written = append(written, writeMessage(t, writer, fmt.Sprintf("message %d", sample)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, event := range written {
		waitForPublished(t, 5*time.Second, database, counter, event)
	}
}

// A process that stops gracefully releases the publisher hold, and the other takes it over at once
// rather than when the hold would have lapsed.
func TestAStoppedOutboxHandsThePublisherToTheOtherAtOnce(t *testing.T) {
	_, url := testnats.Start(t)
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	counter := &sendCounter{}
	first := startProcess(t, "first", database, url, counter)
	second := startProcess(t, "second", storetest.SecondProcess(t, database), url, counter)
	holder := waitForOnePublisher(t, 10*time.Second, first, second)
	standby := first
	if holder == first {
		standby = second
	}

	holder.stop()
	stopped := time.Now()
	waitFor(t, time.Second, "the standby publishing once the holder stopped", standby.outbox.Publishing)
	t.Logf("the standby took the publisher hold %s after the holder stopped", time.Since(stopped).Round(time.Millisecond))
	event := writeMessage(t, standby, "after the hand-over")
	waitForPublished(t, time.Second, database, counter, event)
}

// standbyTakesWithin is the take-over a killed publisher gets: a standby takes the publisher hold
// within six seconds of the kill.
const standbyTakesWithin = 6 * time.Second

// killedHolderEnv names, to a child of this test binary, the database on which the child holds the
// publisher hold until it is killed (runKilledHolder): the holder the killed-holder row kills.
const killedHolderEnv = "DISPATCH_OUTBOX_TEST_KILLED_HOLDER_DATABASE_URL"

// runKilledHolder is the whole of such a child: an outbox on the database at databaseURL, as a
// server process runs one (an outbox pool and an events broker of its own, and a publisher that is
// always connected), which takes the publisher hold and renews it every Renew until the process
// is killed. It first prints its instance, the holder its row names, on a line of its own.
func runKilledHolder(databaseURL string) {
	ctx := context.Background()
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open the holder's database: %v\n", err)
		os.Exit(2)
	}
	pool, err := database.Pool.Outbox()
	if err != nil {
		fmt.Fprintf(os.Stderr, "open the holder's outbox pool: %v\n", err)
		os.Exit(2)
	}
	holder := New(Deps{Store: &store.Store{Pool: pool}, Publisher: &recordingPublisher{}, Broker: events.NewBroker(), Listen: database.Pool.Listener})
	fmt.Printf("holder %s\n", holder.instance)
	holder.Run(ctx)
	// Run returns only once its context ends, and nothing ends this one: the row kills the process.
	os.Exit(3)
}

// killedHolder is a child of this test binary that holds the publisher hold (runKilledHolder).
type killedHolder struct {
	cmd      *exec.Cmd
	stdout   io.ReadCloser
	stderr   bytes.Buffer
	instance uuid.UUID
}

// startKilledHolder starts a holder on database's database and reads its instance. The child is
// killed and waited for when the test ends, and its own output is logged when the test failed.
func startKilledHolder(t *testing.T, database *store.Store) *killedHolder {
	t.Helper()
	holder := &killedHolder{cmd: exec.Command(os.Args[0])}
	holder.cmd.Env = append(os.Environ(), killedHolderEnv+"="+database.Pool.Config().ConnString())
	holder.cmd.Stderr = &holder.stderr
	stdout, err := holder.cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe the holder's output: %v", err)
	}
	holder.stdout = stdout
	if err := holder.cmd.Start(); err != nil {
		t.Fatalf("start the holder: %v", err)
	}
	t.Cleanup(func() {
		if holder.cmd.ProcessState == nil {
			_ = holder.cmd.Process.Kill()
			holder.wait()
		}
		if t.Failed() {
			t.Logf("the holder's output:\n%s", holder.stderr.String())
		}
	})
	line := make(chan string, 1)
	go func() {
		read, _ := bufio.NewReader(stdout).ReadString('\n')
		line <- read
	}()
	select {
	case read := <-line:
		text, found := strings.CutPrefix(strings.TrimSpace(read), "holder ")
		instance, err := uuid.Parse(text)
		if !found || err != nil {
			t.Fatalf("the holder printed %q, want its instance", read)
		}
		holder.instance = instance
	case <-time.After(outboxHangGuard):
		t.Fatalf("the holder printed no instance within %v", outboxHangGuard)
	}
	return holder
}

// kill sends the holder SIGKILL and waits for it to end.
func (h *killedHolder) kill(t *testing.T) {
	t.Helper()
	if err := h.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the holder: %v", err)
	}
	h.wait()
}

// wait reads what is left of the holder's output, which ends with the process, and waits for it.
func (h *killedHolder) wait() {
	_, _ = io.Copy(io.Discard, h.stdout)
	_ = h.cmd.Wait()
}

// A publisher killed at the moment of a renewal is replaced by a standby within six seconds of the
// kill, on the database's clock, and the standby becomes the publisher within six seconds of it on
// this process's clock. The holder is a process of its own, a child of this test binary
// (runKilledHolder), which takes the publisher hold and renews it; the standby runs here. The
// hold's writes are recorded (dutytest.Record): each statement's own now(), when the statement
// started, and the moment the write could first be seen committed. The row kills the holder with
// SIGKILL the moment a renewal committed since the standby started is read: the worst moment a
// kill has, since the row that renewal left lapses a whole TTL after it. The take is read at the
// moment it could be seen committed, so the reading holds all of the take's statement and its
// commit; the renewal at the moment its statement started, which is before the kill, so the
// reading is at or above take - kill, and the row fails on it. Of the writes since that renewal,
// only those that change the row's holder are takes, and there is one: the standby's own
// renewals after its take are not takes.
func TestAStandbyTakesAKilledPublishersHoldWithinSixSecondsOfTheKill(t *testing.T) {
	database := storetest.Open(t)
	writes := dutytest.Record(t, database.Pool, database.Pool.Listener, publisherHold)

	holder := startKilledHolder(t, database)
	writes.WaitForRenewal(t, holder.instance, outboxHangGuard)
	standby := standbyOf(t, database, &recordingPublisher{})
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { standby.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	writes.WaitForRenewal(t, holder.instance, outboxHangGuard)
	killed := time.Now()
	holder.kill(t)
	killedBy := writes.Now(t)
	waitFor(t, outboxHangGuard, "the standby publishing", standby.Publishing)
	publishing := time.Since(killed)

	take := writes.RequireTakeWithin(t, holder.instance, killedBy, "the kill", standbyTakesWithin, outboxHangGuard)
	t.Logf("the standby was read publishing %v after the kill was sent, on this process's clock", publishing.Round(time.Millisecond))
	if take.Took.Holder != standby.instance || take.Changes != 1 {
		t.Fatalf("the hold was taken by %s and changed hands %d times since the killed holder's last renewal (%+v), want once, by the standby %s",
			take.Took.Holder, take.Changes, writes.All(), standby.instance)
	}
	if publishing > standbyTakesWithin {
		t.Fatalf("the standby was read publishing %v after the kill, past the %v bound: the hold lasts %v after a renewal",
			publishing.Round(time.Millisecond), standbyTakesWithin, publisherHoldOptions.TTL)
	}
}

// A holder whose NATS connection is cut releases the publisher hold once it has been without NATS
// for natsLossGrace, and the other process, whose NATS works, takes the hold and publishes what is
// written after the cut.
func TestAnOutboxThatLosesNATSReleasesThePublisher(t *testing.T) {
	_, url := testnats.Start(t)
	proxy := testnats.StartProxy(t, url)
	proxied := proxy.URL()
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	counter := &sendCounter{}
	cut := startProcess(t, "cut", database, proxied, counter)
	waitFor(t, 10*time.Second, "the first process taking the publisher hold", cut.outbox.Publishing)
	working := startProcess(t, "working", storetest.SecondProcess(t, database), url, counter)

	proxy.Cut()
	downAt := time.Now()
	waitFor(t, natsLossGrace+500*time.Millisecond, "the cut process releasing the publisher hold", func() bool {
		return cut.outbox.hold.Load() == nil
	})
	t.Logf("the cut process released the publisher hold %s after the cut", time.Since(downAt).Round(time.Millisecond))
	event := writeMessage(t, working, "after the cut")
	waitForPublished(t, 10*time.Second-time.Since(downAt), database, counter, event)
	if !working.outbox.Publishing() || cut.outbox.Publishing() {
		t.Fatalf("publishing %v after the cut, want only the process whose NATS works", publishing(cut, working))
	}
}

// A holder whose NATS connection goes silent - still connected, nothing answering - finds out when
// a batch's sends go unanswered and a probe of their connection fails, and releases the publisher
// hold. It does not take the hold back while its connection stays silent, although the connection
// still reports itself connected: the other process, whose NATS works, takes it and publishes the
// event, within 10 s of the silence.
func TestAnOutboxWhoseNATSGoesSilentGivesThePublisherToTheOther(t *testing.T) {
	_, url := testnats.Start(t)
	proxy := testnats.StartProxy(t, url)
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	counter := &sendCounter{}
	silent := startProcess(t, "silent", database, proxy.URL(), counter)
	waitFor(t, 10*time.Second, "the first process taking the publisher hold", silent.outbox.Publishing)
	working := startProcess(t, "working", storetest.SecondProcess(t, database), url, counter)

	proxy.Freeze()
	t.Cleanup(proxy.Restore)
	frozenAt := time.Now()
	event := writeMessage(t, working, "after the silence")
	waitFor(t, 10*time.Second, "the working process publishing the event", func() bool {
		return working.outbox.Publishing() && publishedAt(t, database, event.ID) != nil
	})
	t.Logf("the working process published the event %s after the silence", time.Since(frozenAt).Round(time.Millisecond))
	for range 20 {
		if silent.outbox.Publishing() {
			t.Fatalf("publishing %v while the silent process's connection stays silent, want only the process whose NATS works",
				publishing(silent, working))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// An idle holder whose NATS connection goes silent at the worst moment - right after the server
// answered one of the client's own pings, so the client's pings find the silence
// bus.SilentCutFoundWithin later, the latest they can - gives the hold up for NATS lost once its
// grace has run out, and the other process, whose NATS works, becomes the publisher within six
// seconds of the silence: the finding, natsLossGrace, the two bounded reads of the client's status
// and the hand-over (releaseToServe) sum to takeOverWithin, and nothing the holder sends finds the
// silence earlier, since it sends nothing. The holder's client finds silent cuts as cmd/dispatch's
// does (bus.WithSilentCutDetection).
func TestAnIdleHolderSilencedAtItsLatestFindingIsReplacedWithinSixSeconds(t *testing.T) {
	_, url := testnats.Start(t)
	proxy := testnats.StartProxy(t, url)
	database := storetest.Open(t)
	counter := &sendCounter{}
	silent := startProcess(t, "silent", database, proxy.URL(), counter, bus.WithSilentCutDetection())
	waitFor(t, outboxHangGuard, "the first process taking the publisher hold", silent.outbox.Publishing)
	working := startProcess(t, "working", storetest.SecondProcess(t, database), url, counter)
	t.Cleanup(proxy.Restore)

	var frozen time.Time
	select {
	case frozen = <-proxy.FreezeAfterServer("PONG\r\n"):
	case <-time.After(outboxHangGuard):
		t.Fatalf("the server answered no ping of the silent process's client within %v", outboxHangGuard)
	}
	waitFor(t, outboxHangGuard, "the working process publishing", working.outbox.Publishing)
	took := time.Since(frozen)
	t.Logf("the working process was read publishing %v after the silent process's client last heard its server", took.Round(time.Millisecond))
	if silent.outbox.Publishing() {
		t.Errorf("the silent process still publishes beside the working one")
	}
	if took > standbyTakesWithin {
		t.Errorf("the working process became the publisher %v after the silence, past the %v bound: the client finds the silence up to %v in, and the holder's grace is %v",
			took.Round(time.Millisecond), standbyTakesWithin, bus.SilentCutFoundWithin, natsLossGrace)
	}
}

// A standby waits for the publisher hold to be released or to lapse, or for its NATS connection to
// change, and for nothing else: the events the holder writes and publishes do not make it try the
// hold. 100 events written one after another while the holder holds send at most the few takes and
// lapse reads the standby's timer to the holder's lapse makes over that span, not one take and one
// read per event, which would put a write on duty_holds for every event on every standby. Each
// renewal moves the lapse TTL on and comes Renew after the last, so the timer fires at most once
// each TTL - Renew, 4 s: the row allows that many over the span it counts, and never fewer than 3.
func TestAStandbyDoesNotTryTheHoldForEveryEventTheHolderWrites(t *testing.T) {
	_, url := testnats.Start(t)
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	counter := &sendCounter{}
	holder := startProcess(t, "holder", database, url, counter)
	waitFor(t, 10*time.Second, "the holder taking the publisher hold", holder.outbox.Publishing)

	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect the standby's NATS client: %v", err)
	}
	t.Cleanup(client.Close)
	tracer := &statementTracer{}
	standby := New(Deps{
		Store:     tracedStore(t, storetest.SecondProcess(t, database), tracer),
		Publisher: client,
		Broker:    events.NewBroker(),
		Listen:    database.Pool.Listener,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { standby.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, 5*time.Second, "the standby trying the publisher hold once", func() bool {
		return len(tracer.matching("insert into duty_holds")) > 0
	})
	// The standby's LISTEN connection's catch-up wake comes as it starts.
	time.Sleep(time.Second)

	// A take reads the row it replaces too (duty's takeHold), so a read of the lapse is told by
	// its own expression, expires_at - now(), which only duty.Expiry's statement holds.
	takes, reads := len(tracer.matching("insert into duty_holds")), len(tracer.matching("expires_at - now()"))
	counting := time.Now()
	var last model.Event
	for index := range 100 {
		last = writeMessage(t, holder, fmt.Sprintf("message %d", index))
	}
	waitForPublished(t, 10*time.Second, database, counter, last)
	if standby.Publishing() {
		t.Fatal("the standby publishes while the holder holds the publisher hold")
	}
	takes = len(tracer.matching("insert into duty_holds")) - takes
	reads = len(tracer.matching("expires_at - now()")) - reads
	span := time.Since(counting)
	timerFires := max(3, int(span/(publisherHoldOptions.TTL-publisherHoldOptions.Renew))+1)
	t.Logf("while 100 events were written and published, over %v, the standby sent %d takes of the hold and %d reads of its lapse",
		span.Round(time.Millisecond), takes, reads)
	if takes > timerFires || reads > timerFires {
		t.Fatalf("the standby sent %d takes of the publisher hold and %d reads of its lapse while the holder wrote 100 events over %v, want at most %d of each: its timer to the lapse alone",
			takes, reads, span.Round(time.Millisecond), timerFires)
	}
}

// fenceAfterFirstSend is a publisher hold whose deadline passes as the batch's first envelope is
// sent, as a process that froze mid-batch past its deadline finds when it resumes.
type fenceAfterFirstSend struct {
	recordingPublisher
	passed atomic.Bool
}

func (p *fenceAfterFirstSend) WindowUntil(_ context.Context, deadline func() time.Time) bus.Sender {
	return deadlineWindow{Sender: p, deadline: deadline}
}

func (p *fenceAfterFirstSend) PublishAsync(item contracts.Envelope) <-chan error {
	p.passed.Store(true)
	return p.recordingPublisher.PublishAsync(item)
}

func (p *fenceAfterFirstSend) Deadline() time.Time {
	if p.passed.Load() {
		return time.Now().Add(-time.Second)
	}
	return time.Now().Add(time.Hour)
}

// A batch checks the publisher hold's deadline before every send: once a send has met it the
// batch sends nothing more, counts no attempt, writes nothing, and reports the hold lost. The
// record is not checked against the in-process clock a second time - a batch whose sends all
// went out under the hold records what they came to - and whether its back-offs may still be
// scheduled is the record's own check, on the database's clock (recordBatch).
func TestABatchPastThePublisherHoldsDeadlineSendsAndRecordsNothingMore(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	for index := range 3 {
		appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
			Payload: model.Message{ID: fmt.Sprintf("message-%d", index), IssueKey: new("T-1"), Body: "fenced"},
		})
	}
	publisher := &fenceAfterFirstSend{}
	batch, err := scanBatch(context.Background(), Deps{Store: database, Publisher: publisher}, publisher, uuid.New())
	if err != nil {
		t.Fatalf("scan the batch: %v", err)
	}
	if !batch.fenced || batch.sent != 1 {
		t.Fatalf("batch = %+v, want it fenced after its first event", batch)
	}
	if sent := publisher.all(); len(sent) != 1 {
		t.Fatalf("the batch sent %d envelopes, want only the one before the deadline passed", len(sent))
	}
	var published, attempted int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) filter (where published_at is not null), count(*) filter (where attempt_count <> 0) from events
	`).Scan(&published, &attempted); err != nil {
		t.Fatalf("read the events: %v", err)
	}
	if published != 0 || attempted != 0 {
		t.Fatalf("%d events marked published and %d backed off past the deadline, want none written", published, attempted)
	}
	end := New(Deps{Store: database, Publisher: &recordingPublisher{}}).scan(context.Background(), publisher)
	if !end.fenced {
		t.Fatalf("a scan under a hold past its deadline ended %+v, want fenced before any claim", end)
	}
}

// reconnectedQuietly is a publisher whose client reads connected again without its connection
// change ever being reported, as a reconnect whose event waits in nats.go's callback queue behind
// a slow callback finds it: ConnectionChanged never closes, and nothing wakes the holder.
type reconnectedQuietly struct {
	recordingPublisher
	connected atomic.Bool
}

func (p *reconnectedQuietly) Connected() bool { return p.connected.Load() }

func (p *reconnectedQuietly) WindowUntil(_ context.Context, deadline func() time.Time) bus.Sender {
	return deadlineWindow{Sender: p, deadline: deadline}
}

// A holder whose client reads connected again when natsLossGrace runs out serves on, though no
// connection event came: it reads the client's status as the grace ends, keeps the hold and scans,
// rather than giving the hold up for a loss that is over.
func TestAHolderConnectedAgainWhenTheGraceEndsKeepsTheHold(t *testing.T) {
	database := storetest.Open(t)
	publisher := &reconnectedQuietly{}
	outbox := New(Deps{Store: database, Publisher: publisher})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hold, _ := outbox.tryHold(ctx)
	if hold == nil {
		t.Fatal("the outbox did not take the publisher hold")
	}
	t.Cleanup(func() { _ = hold.Release(context.Background()) })

	ended := make(chan holdEnd, 1)
	go func() { ended <- outbox.serve(ctx, hold) }()
	time.Sleep(200 * time.Millisecond)
	if outbox.claims.Load() != 0 {
		t.Fatal("the holder scanned while its client read disconnected")
	}
	publisher.connected.Store(true)
	select {
	case end := <-ended:
		t.Fatalf("the holder stopped serving (%s) though its client read connected when the grace ended", end)
	case <-time.After(natsLossGrace + time.Second):
	}
	if outbox.claims.Load() == 0 {
		t.Fatal("the holder did not scan once it found its client connected at the grace's end")
	}
	cancel()
	if end := <-ended; end != stopping {
		t.Fatalf("serve ended %s once ctx ended, want %s", end, stopping)
	}
}

// reconnectAttemptHolds is how long a reconnect attempt holds nats.go's lock of the connection
// through a silent cut: the dial and handshake wait out the client's connect timeout.
const reconnectAttemptHolds = 5 * time.Second

// reconnectingUnderLock is a publisher whose client is disconnected and in the middle of a
// reconnect attempt at every read of its status, as nats.go is through a silent cut: each read
// waits out the attempt holding the connection's lock, and then answers not connected.
type reconnectingUnderLock struct {
	recordingPublisher
}

func (p *reconnectingUnderLock) Connected() bool {
	time.Sleep(reconnectAttemptHolds)
	return false
}

// A holder whose client reads disconnected gives the publisher hold up for NATS lost once
// natsLossGrace has passed, though every read of its client's status waits out a reconnect
// attempt: its reads are bounded (statusReadBound), so neither the read that finds the loss nor
// the one at the grace's end holds the hand-over back. Unbounded, the two reads alone took twice
// reconnectAttemptHolds.
func TestAHolderWhoseClientIsMidReconnectGivesThePublisherUpOnTime(t *testing.T) {
	database := storetest.Open(t)
	outbox := New(Deps{Store: database, Publisher: &reconnectingUnderLock{}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hold, _ := outbox.tryHold(ctx)
	if hold == nil {
		t.Fatal("the outbox did not take the publisher hold")
	}
	t.Cleanup(func() { _ = hold.Release(context.Background()) })

	started := time.Now()
	end := outbox.serve(ctx, hold)
	took := time.Since(started)
	t.Logf("the holder whose client was mid-reconnect stopped serving (%s) %v after it began", end, took.Round(time.Millisecond))
	if end != natsLost {
		t.Fatalf("serve ended %s, want %s", end, natsLost)
	}
	if bound := natsLossGrace + 2*statusReadBound + time.Second; took > bound {
		t.Fatalf("the holder gave the publisher hold up %v after it began, past %v: natsLossGrace, the two bounded reads and a second", took.Round(time.Millisecond), bound)
	}
}

// A batch that begins while its client reconnects - the client found its connection lost a
// moment before, as a batch that a scan starts at the loss's own wake does - ends for NATS lost
// within probeTimeout and one bounded status read of its start: its send does not wait for a reconnect that does not come, and
// the probe of the connection that follows is the batch's one wait for it. So a holder whose loss
// a batch finds gives the publisher hold up in the time its grace allows (natsLossGrace), inside
// the six seconds a standby takes over in, rather than once a send has waited out a reconnect.
//
// It holds for both ways a connection is lost. A cut connection's reconnect is refused at once. A
// silent one, found by the client's own pings (bus.WithSilentCutDetection, the client
// cmd/dispatch runs), reconnects through a path that answers nothing, and each attempt holds
// nats.go's lock of the connection across its dial and handshake, up to the client's connect
// timeout, with the client's reconnect wait between attempts: a read of the connection's status
// that waited on that lock held the batch up to 6 s. So the silent row begins its batch while an
// attempt holds the lock (inReconnectAttempt), where the batch's first read of the
// connection's status meets it. The bound is probeTimeout, one bounded status read past it
// (bus.ConnectionReadBound, 250 ms), and the batch's own two statements, its read of the events
// and its release of what it did not send, which the rest of the second covers.
func TestABatchBegunWhileItsClientReconnectsEndsForNATSLostWithinTheProbe(t *testing.T) {
	for _, row := range []struct {
		name    string
		silent  bool
		options []bus.ConnectOption
	}{
		{"a cut connection", false, nil},
		{"a silent connection", true, []bus.ConnectOption{bus.WithSilentCutDetection()}},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, url := testnats.Start(t)
			proxy := testnats.StartProxy(t, url)
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", nil)
			appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
				Payload: model.Message{ID: "message-reconnecting", IssueKey: new("T-1"), Body: "sent while the client reconnects"},
			})
			client, err := bus.ConnectOwningStream([]string{proxy.URL()}, row.options...)
			if err != nil {
				t.Fatalf("connect the holder's NATS client: %v", err)
			}
			t.Cleanup(client.Close)
			holder := New(Deps{Store: database, Publisher: client})
			hold, _ := holder.tryHold(context.Background())
			if hold == nil {
				t.Fatal("the outbox did not take the publisher hold")
			}
			t.Cleanup(func() { _ = hold.Release(context.Background()) })
			cut := time.Now()
			if row.silent {
				proxy.Freeze()
			} else {
				proxy.Cut()
			}
			t.Cleanup(proxy.Restore)
			waitFor(t, outboxHangGuard, "the client reading its connection lost", func() bool { return !bus.ConnectedWithin(client, statusReadBound) })
			if row.silent {
				waitFor(t, outboxHangGuard, "a reconnect attempt holding the client's connection", func() bool { return inReconnectAttempt(client) })
			}
			found := time.Now()

			batch, err := scanBatch(context.Background(), holder.deps, hold, holder.instance)
			took := time.Since(batch.started)
			if err != nil {
				t.Fatalf("scan the batch: %v", err)
			}
			t.Logf("the loss was read %v after the cut; the batch begun then ended %v after its start: natsLost %v, fenced %v, sent %d, published %d",
				found.Sub(cut).Round(time.Millisecond), took.Round(time.Millisecond), batch.natsLost, batch.fenced, batch.sent, batch.published)
			if !batch.natsLost || batch.fenced || batch.published != 0 {
				t.Fatalf("the batch begun while its client reconnected ended %+v, want it ended for NATS lost, unfenced, with nothing published", batch)
			}
			if bound := probeTimeout + time.Second; took > bound {
				t.Fatalf("the batch begun while its client reconnected ended for NATS lost %v after its start, past %v: probeTimeout and a second",
					took.Round(time.Millisecond), bound)
			}
		})
	}
}

// inReconnectAttempt reports whether a read of client's connection status waits longer than
// statusReadBound: nats.go holds its lock of the connection that long only across a reconnect
// attempt's dial and handshake. The read it starts is left to end on its own.
func inReconnectAttempt(client *bus.Client) bool {
	answered := make(chan struct{})
	go func() {
		client.Connected()
		close(answered)
	}()
	select {
	case <-answered:
		return false
	case <-time.After(statusReadBound):
		return true
	}
}

// A record that finds the publisher hold another's ends the holder as one that lost the hold,
// with the hold's loss deadline still ahead. The holder stops that hold's renewal there and then:
// its Lost is closed, it sends no renewal across the first tick that follows, and the other's row
// is left alone. This is also the one path that touches the hold not at all, so its deadline says
// nothing about when the process stopped publishing: the line that ends the turn has to date the
// stop at serve's return rather than at a deadline still minutes ahead. That makes it the one
// path in this package's rows where the line's two bounds, stopped and stopped_by, are not both
// that deadline but bracket the store that turned Publishing() false, so this is the row that
// holds their order.
func TestAHolderWhoseRecordFindsTheHoldAnothersStopsItsRenewal(t *testing.T) {
	recorder := &logtest.Recorder{}
	logtest.SetDefault(t, recorder)

	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	tracer := &statementTracer{}
	outbox := New(Deps{Store: tracedStore(t, database, tracer), Publisher: &recordingPublisher{}})

	// The row takes the hold with duty.TryHold under options of its own rather than through
	// outbox.tryHold, which uses publisherHoldOptions. What it tests is what publish does and
	// logs when a recordBatch finds the hold another's before any renewal has noticed, and
	// that property is not the publisher hold's production numbers; binding the row's slack to
	// those numbers made every change of them a change of this row's flakiness. Halving Renew
	// from 2 s to 1 s left the take, the seed, the append and a whole scan one second to run in
	// before the guard below reports a correct product as a defect - on a runner that, by this
	// change's own evidence, stalled for 3.1 s.
	//
	// These numbers leave seconds instead, inside what duty.TryHold accepts (0 <= Margin < TTL
	// and 0 < Renew < TTL - Margin): the first renewal tick falls five seconds after the take, so
	// the take, the seed, the append and a whole scan have those five seconds to run in before
	// any renewal of this hold can reach the record path, and the deadline - the take plus
	// TTL - Margin - stays five minutes ahead for the whole row. Five seconds is also short
	// enough for the count below to wait that tick out and see whether the lost hold's renewal
	// stopped. The hold is the outbox's own, under its token and instance, so the batch that
	// records under it finds the row another's exactly as it would in production.
	recordPath := duty.HoldOptions{TTL: 6 * time.Minute, Renew: 5 * time.Second, Margin: time.Minute, Token: outbox.token}
	taken := time.Now()
	hold, err := duty.TryHold(ctx, outbox.deps.Store.Pool, publisherHold, outbox.instance, recordPath)
	if err != nil {
		t.Fatalf("take the publisher hold under the row's own options: %v", err)
	}
	if hold == nil {
		t.Fatal("the outbox did not take the publisher hold")
	}
	deadline := hold.Deadline()
	other := uuid.New()
	holdRow(t, database, other)
	appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: uuid.NewString(), IssueKey: new("T-1"), Body: "recorded under another's hold"},
	})

	began := time.Now()
	if end := outbox.publish(ctx, hold); end != holdLost {
		t.Fatalf("publish ended %s, want %s", end, holdLost)
	}
	returned := time.Now()
	// A publish slow enough to outrun the first renewal would reach holdLost by that renewal
	// finding the row another's instead, where loseLocked pulls the deadline back to the moment
	// it closed Lost and stopped is that deadline, so say which path ran rather than let the
	// assertions below fail as if the product were wrong.
	if elapsed := returned.Sub(taken); elapsed >= recordPath.Renew {
		t.Fatalf("publish took %s, past the first renewal of the row's own hold: the row is about a record finding the hold another's before any renewal does", elapsed)
	}
	select {
	case <-hold.Lost():
	default:
		t.Fatal("the lost hold's Lost is still open: its renewal runs on")
	}
	var holder uuid.UUID
	if err := database.Pool.QueryRow(ctx, `select holder from duty_holds where name = $1`, publisherHold).Scan(&holder); err != nil || holder != other {
		t.Fatalf("the hold's row names %s (%v), want the other process's %s left alone", holder, err, other)
	}

	stopped, ok := timeIn(t, recorder, "lost the publisher hold", "stopped")
	if !ok {
		t.Fatalf("no record of the log says %q; it says %q", "lost the publisher hold", loggedMessages(recorder))
	}
	// timeIn has already found that record, and it fails the row itself when a key it is asked
	// for is missing, so there is nothing left for a second !ok to catch.
	stoppedBy, _ := timeIn(t, recorder, "lost the publisher hold", "stopped_by")
	// The four comparisons below hold the whole order a reader of the log depends on:
	// began <= stopped <= stopped_by <= returned, with stopped_by still short of the deadline.
	// began is read just ahead of publish and returned just after it, so the pair is held inside
	// the turn itself, which is what separates serve's return from a deadline minutes further on;
	// a stopped that is too early - a zero value, or one dated before this turn began - would
	// pass every bound from the future and is caught by the first. The middle comparison is the
	// reason this row reads stopped_by at all: this is the one path here where the two are not
	// the same capped value, so the two clocks read in the wrong order, or the two attributes
	// swapped, leave an upper bound earlier than the lower one and fail here; two reads that land
	// on the same nanosecond pass, and leave bounds that are still honest to the clock.
	//
	// What no comparison here can catch is which side of o.hold.Store(nil) each clock was read
	// on. Moving the second read above the store, and leaving it after the first, keeps the two
	// in order and every bound below holding; the line is written after both reads and after the
	// store, so nothing it carries says where the store fell between them. No row holds that
	// placement: publish's doc comment records it, and the one reader that compares /healthz with
	// stopped_by, the two-process row, sees only turns that end at the hold's deadline, where
	// stopped_by is the deadline wherever the read sits.
	if stopped.Before(began) {
		t.Errorf("the line dates the stop at %s, before publish was called at %s",
			stopped.Format(time.RFC3339Nano), began.Format(time.RFC3339Nano))
	}
	if stoppedBy.Before(stopped) {
		t.Errorf("the line bounds the stop above by %s, before the %s it bounds it below by",
			stoppedBy.Format(time.RFC3339Nano), stopped.Format(time.RFC3339Nano))
	}
	if returned.Before(stoppedBy) {
		t.Errorf("the line bounds the stop above by %s, after the turn ended at %s",
			stoppedBy.Format(time.RFC3339Nano), returned.Format(time.RFC3339Nano))
	}
	if !stoppedBy.Before(deadline) {
		t.Errorf("the line bounds the stop above by %s, at or past the loss deadline of %s this path leaves minutes ahead",
			stoppedBy.Format(time.RFC3339Nano), deadline.Format(time.RFC3339Nano))
	}

	// This count is the whole of the proof that the renewal of the hold just lost stopped when
	// publish released it: nothing else in the row watches that renewal. Its window starts here,
	// with publish returned and the stop asserted, and runs past that hold's first renewal tick
	// at taken + Renew, which no earlier part of the row reaches: a renewal still running sends
	// its one statement there - that is what a duty.Hold whose Release does not stop the renewal,
	// and whose renew does not return when Lost closes, does - and nothing else in this process
	// renews a hold, so a statement in the window could only be this one's. Half a Renew of slack
	// past the tick, so a statement a loaded runner was slow to start still lands inside the
	// window rather than after it.
	renewalsBefore := len(tracer.matching("update duty_holds set expires_at"))
	window := recordPath.Renew + recordPath.Renew/2
	time.Sleep(time.Until(taken.Add(window)))
	if got := len(tracer.matching("update duty_holds set expires_at")) - renewalsBefore; got != 0 {
		t.Fatalf("%s after the hold was taken - past the first tick of its renewal at %s - %d renewals of the lost hold were sent, want none: publish left its renewal running", window, recordPath.Renew, got)
	}
}

// heldRelease is a pgx tracer that holds the hold's release statement until the test lets it go,
// so a row can read the log while the release is still running. Every other statement passes.
// A row defers let: without it a t.Fatalf in the middle would leave the statement blocked, and
// the pool's own close - a cleanup, which runs after every defer - would wait on it until the
// package's timeout instead of the row failing.
type heldRelease struct {
	started   chan struct{}
	goAhead   chan struct{}
	onceStart sync.Once
	onceGo    sync.Once
}

func newHeldRelease() *heldRelease {
	return &heldRelease{started: make(chan struct{}), goAhead: make(chan struct{})}
}

// let lets the release run, whether or not it has reached the tracer yet.
func (h *heldRelease) let() { h.onceGo.Do(func() { close(h.goAhead) }) }

func (h *heldRelease) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "delete from duty_holds") {
		h.onceStart.Do(func() { close(h.started) })
		<-h.goAhead
	}
	return ctx
}

func (h *heldRelease) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// timeIn is the key attribute (momentOf) of the first record the recorder kept whose message holds
// message, and false when no record says it yet, so a row can both wait for the line and read a
// moment it carries. It reads the records the code wrote rather than a rendering of them: slog's
// text handler truncates a record's own time to the millisecond and quotes what it writes, so a
// row that cut its output on "stopped=" and trimmed the quotes off asserted on a layer that is not
// the code under test.
//
// It takes the attribute's name because the lost and released lines carry two bounds on the
// moment the process stopped publishing, stopped and stopped_by, and a row that reads only the
// first leaves their order, their place inside the turn and their distance from the deadline
// unheld.
func timeIn(t *testing.T, recorder *logtest.Recorder, message, key string) (time.Time, bool) {
	t.Helper()
	for _, record := range recorder.Records() {
		if strings.Contains(record.Message, message) {
			return momentOf(t, record, key), true
		}
	}
	return time.Time{}, false
}

// momentOf is the moment record's key attribute carries, failing t when it carries none: a
// time.Time the code logged as one (the sent batch line's started) as it is, and a string as the
// RFC3339Nano text the code writes the took, lost and released lines' moments in.
func momentOf(t *testing.T, record slog.Record, key string) time.Time {
	t.Helper()
	value, found := attributeOf(record, key)
	if !found {
		t.Fatalf("the record %q carries no %s attribute", record.Message, key)
	}
	if value.Kind() == slog.KindTime {
		return value.Time()
	}
	moment, err := time.Parse(time.RFC3339Nano, value.String())
	if err != nil {
		t.Fatalf("read the %s attribute %q of the record %q: %v", key, value.String(), record.Message, err)
	}
	return moment
}

// attributeOf is the value of record's key attribute, and false when the record carries none.
func attributeOf(record slog.Record, key string) (slog.Value, bool) {
	var value slog.Value
	found := false
	record.Attrs(func(attribute slog.Attr) bool {
		if attribute.Key != key {
			return true
		}
		value, found = attribute.Value.Resolve(), true
		return false
	})
	return value, found
}

// loggedMessages are the messages of every record the recorder kept so far, for a row's failure
// to say what was logged instead of what it waited for.
func loggedMessages(recorder *logtest.Recorder) []string {
	records := recorder.Records()
	messages := make([]string, 0, len(records))
	for _, record := range records {
		messages = append(messages, record.Message)
	}
	return messages
}

// A holder whose row is taken from under it loses the hold at once: the deadline is pulled back
// to that moment, and the line that ends the turn says so. The line is written before the
// release, which is a database round trip the job this came from took a second over, so it is
// already in the log while the release's own statement is still running, and its stopped
// attribute names the moment the process stopped publishing rather than the moment it was
// written.
func TestTheLostLineIsWrittenBeforeTheReleaseAndDatesTheStop(t *testing.T) {
	recorder := &logtest.Recorder{}
	logtest.SetDefault(t, recorder)

	ctx := context.Background()
	database := storetest.Open(t)
	held := newHeldRelease()
	defer held.let()
	outbox := New(Deps{Store: tracedStore(t, database, held), Publisher: &recordingPublisher{}})
	hold, _ := outbox.tryHold(ctx)
	if hold == nil {
		t.Fatal("the outbox did not take the publisher hold")
	}
	ended := make(chan holdEnd, 1)
	go func() { ended <- outbox.publish(ctx, hold) }()

	// The row goes, so the next renewal affects no row and the hold is lost there and then.
	if _, err := database.Pool.Exec(ctx, `delete from duty_holds where name = $1`, publisherHold); err != nil {
		t.Fatalf("take the hold's row away: %v", err)
	}
	select {
	case <-hold.Lost():
	case <-time.After(2 * publisherHoldOptions.Renew):
		t.Fatalf("the hold was not lost within %v of its row going", 2*publisherHoldOptions.Renew)
	}
	lostAt := time.Now()

	select {
	case <-held.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the holder never ran the release")
	}
	if _, ok := timeIn(t, recorder, "lost the publisher hold", "stopped"); !ok {
		t.Fatalf("the holder's release is running and it has not logged that it lost the hold yet; it logged %q", loggedMessages(recorder))
	}
	held.let()
	if end := <-ended; end != holdLost {
		t.Fatalf("publish ended %s, want %s", end, holdLost)
	}

	stopped, ok := timeIn(t, recorder, "lost the publisher hold", "stopped")
	if !ok {
		t.Fatalf("no record of the log says %q; it says %q", "lost the publisher hold", loggedMessages(recorder))
	}
	if stopped.After(lostAt) || lostAt.Sub(stopped) > time.Second {
		t.Errorf("the line dates the stop at %s, and the hold reported itself lost at %s",
			stopped.Format(time.RFC3339Nano), lostAt.Format(time.RFC3339Nano))
	}
}

// seedFallbackBacklog seeds one comment on issue T-1, whose comment.created event the outbox has
// published already, and count copies of it held as comment_fallback_copies rows, each on an
// agent subject of its own and none of them claimed yet (next_attempt_at null, the state a row
// the batch just held back is in). It returns the comment's id.
func seedFallbackBacklog(t *testing.T, database *store.Store, broker *events.Broker, count int) string {
	t.Helper()
	const body = "a comment the outbox held copies of"
	ctx := context.Background()
	seedIssue(t, database, "T-1", nil)
	alice := model.Actor{Kind: "user", ID: "alice"}
	commentID := seedComment(t, database, "T-1", alice, body, nil)
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body}},
	})
	if _, err := database.Pool.Exec(ctx, `update events set published_at = now() where id = $1`, event.ID); err != nil {
		t.Fatalf("publish the comment's event: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into comment_fallback_copies (comment_id, destination, event_id, session_id)
		select $1, 'notifications.agent.s' || i, $2, 's' || i from generate_series(1, $3::int) i
	`, commentID, event.ID, count); err != nil {
		t.Fatalf("hold %d fallback copies: %v", count, err)
	}
	return commentID
}

// publishedCopies is how many of commentID's held copies a pass has published.
func publishedCopies(t *testing.T, database *store.Store, commentID string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from comment_fallback_copies
		where comment_id = $1 and settled_at is not null and published
	`, commentID).Scan(&count); err != nil {
		t.Fatalf("count the published copies: %v", err)
	}
	return count
}

// A pass whose claim filled its batch leaves the holder due again. A row held back by a batch has
// no next_attempt_at, so no retry timer is armed to it (Outbox.nextRetry): were the pass to report
// nothing, the rows past the first batchSize its claim took would wait for an unrelated event's
// insert to wake the holder. The holder scans and passes again instead, as it does after a full
// batch, until nothing is left.
func TestAFallbackPassThatFilledItsClaimLeavesTheHolderDueAgain(t *testing.T) {
	ctx, cancel := context.WithCancel(store.WithTransactionTracking(context.Background()))
	defer cancel()
	database := storetest.Open(t)
	deps := Deps{Store: database, Publisher: &recordingPublisher{}, Broker: events.NewBroker()}
	const copies = batchSize + 1
	commentID := seedFallbackBacklog(t, database, deps.Broker, copies)

	outbox := New(deps)
	hold, _ := outbox.tryHold(ctx)
	if hold == nil {
		t.Fatal("the outbox did not take the free publisher hold")
	}
	t.Cleanup(func() { _ = hold.Release(context.Background()) })
	served := make(chan holdEnd, 1)
	go func() { served <- outbox.serve(ctx, hold) }()

	waitFor(t, 30*time.Second, fmt.Sprintf("all %d held copies published", copies), func() bool {
		return publishedCopies(t, database, commentID) == copies
	})
	cancel()
	select {
	case end := <-served:
		if end != stopping {
			t.Fatalf("serve ended %q, want it to stop with its context", end)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return once its context ended")
	}
}

// A pass whose claim failed backs the holder off rather than leaving it to pass again at once:
// the claim read no row, so no row's next_attempt_at holds the wait, and a claim that keeps
// failing would re-pass on every wake. The holder waits retry.BaseDelay, as it does after a scan
// whose statement failed.
func TestAFallbackPassWhoseClaimFailedBacksTheHolderOff(t *testing.T) {
	database := storetest.Open(t)
	deps := Deps{Store: database, Publisher: &recordingPublisher{}, Broker: events.NewBroker()}
	outbox := New(deps)
	// A claim the pool refuses, as it refuses every read a caller makes while it holds a
	// connection of its own (store.ErrNestedAcquire).
	held, release := store.HoldsConnection(store.WithTransactionTracking(context.Background()))
	defer release()

	full, failed := publishFallbackCopies(held, deps, unfenced{}, outbox.instance)

	if full || !failed {
		t.Fatalf("a pass whose claim failed reports full=%v failed=%v, want a failure and no full claim", full, failed)
	}
	if wait, ok := outbox.nextRetry(context.Background(), failed); !ok || wait != retry.BaseDelay {
		t.Fatalf("the holder's next pass is armed to %s (armed=%v), want retry.BaseDelay = %s", wait, ok, retry.BaseDelay)
	}
}

// failEveryFallbackClaim makes every update of comment_fallback_copies in database raise, which
// fails the fallback pass's claim and nothing else: the claim is that one update ... returning
// (claimFallbackCopies), while every read of the table still answers, Outbox.nextRetry's among
// them.
func failEveryFallbackClaim(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		create function fail_the_fallback_claim() returns trigger language plpgsql as $$
		begin
			raise exception 'this test fails every claim of a fallback copy';
		end
		$$
	`); err != nil {
		t.Fatalf("write the function behind the failing claim: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		create trigger fail_the_fallback_claim before update on comment_fallback_copies
		for each row execute function fail_the_fallback_claim()
	`); err != nil {
		t.Fatalf("make every fallback claim fail: %v", err)
	}
}

// failedClaims is how many fallback claims the log says failed: the pass logs one Error line for
// each of them (publishFallbackCopies).
func failedClaims(logged *lockedBuffer) int {
	var count int
	for line := range strings.SplitSeq(logged.String(), "\n") {
		if strings.Contains(line, "dispatch outbox: claim fallback copies") {
			count++
		}
	}
	return count
}

// The holder passes again retry.BaseDelay after a pass whose claim failed, rather than waiting
// for a wake that never comes. A row the claim never reached keeps the next_attempt_at it had -
// none at all, for a row a batch has just held back - so no retry timer is armed to it
// (Outbox.nextRetry), and nothing in this test inserts an event or changes a connection: a serve
// that read the failed claim as an idle pass would leave the held copy unpublished until some
// unrelated event woke it. The pass writes one Error line for every claim that fails, so a
// second such line is the second pass and one line alone is a holder that armed nothing.
//
// The claim fails on a trigger of this test's own database rather than on a table nothing can
// read at all, because nextRetry reads comment_fallback_copies too: a read of it that fails arms
// the timer by itself, its error path answering retry.BaseDelay, whatever serve made of the
// pass's failure.
func TestAFallbackPassWhoseClaimFailedHasTheHolderPassAgain(t *testing.T) {
	var logged lockedBuffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
	ctx, cancel := context.WithCancel(store.WithTransactionTracking(context.Background()))
	defer cancel()
	database := storetest.Open(t)
	deps := Deps{Store: database, Publisher: &recordingPublisher{}, Broker: events.NewBroker()}
	seedFallbackBacklog(t, database, deps.Broker, 1)
	failEveryFallbackClaim(t, database)

	outbox := New(deps)
	hold, _ := outbox.tryHold(ctx)
	if hold == nil {
		t.Fatal("the outbox did not take the free publisher hold")
	}
	t.Cleanup(func() { _ = hold.Release(context.Background()) })
	served := make(chan holdEnd, 1)
	started := time.Now()
	go func() { served <- outbox.serve(ctx, hold) }()

	waitFor(t, 30*time.Second, "a second pass of the claim that keeps failing", func() bool {
		return failedClaims(&logged) >= 2
	})
	if waited := time.Since(started); waited < retry.BaseDelay {
		t.Fatalf("the holder passed again %s after it started, want it to wait retry.BaseDelay = %s rather than spin",
			waited, retry.BaseDelay)
	}
	cancel()
	select {
	case end := <-served:
		if end != stopping {
			t.Fatalf("serve ended %q, want it to stop with its context", end)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return once its context ended")
	}
}
