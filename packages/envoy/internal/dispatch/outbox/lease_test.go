package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
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

func (c countingClient) WindowUntil(deadline func() time.Time) bus.Sender {
	return countingSender{Sender: c.Client.WindowUntil(deadline), counter: c.counter}
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
// connected to url, until the test ends or stop is called.
func startProcess(t *testing.T, name string, database *store.Store, url string, counter *sendCounter) *process {
	t.Helper()
	return startProcessOn(t, name, database, url, counter, nil)
}

// startProcessOn is startProcess with the outbox's timers - its standby's timer to the holder's
// lapse among them - armed on timers, or on the real clock when timers is nil.
func startProcessOn(t *testing.T, name string, database *store.Store, url string, counter *sendCounter, timers timerClock) *process {
	t.Helper()
	return startProcessWith(t, name, database, url, counter, func(o *Outbox) {
		if timers != nil {
			o.clock = timers
		}
	})
}

// startProcessWith is startProcess with options given to the process's NATS client and set run on
// its outbox before Run starts, for a row that puts one of the outbox's clocks on a fake.
func startProcessWith(t *testing.T, name string, database *store.Store, url string, counter *sendCounter, set func(*Outbox), options ...bus.ConnectOption) *process {
	t.Helper()
	client, err := bus.ConnectOwningStream([]string{url}, options...)
	if err != nil {
		t.Fatalf("connect %s's NATS client: %v", name, err)
	}
	t.Cleanup(func() { bustest.Close(t, client) })
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
	set(p.outbox)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.outbox.Run(ctx); close(done) }()
	// The stop runs once, through sync.Once.Do and not sync.OnceFunc: a give-up inside the stop
	// ends its goroutine (runtime.Goexit, from testwait's t.Fatalf), which sync.OnceFunc turns into
	// a panic with a nil argument that ends the whole test binary. Once.Do marks the stop done as
	// the goroutine exits, so the cleanup's second call returns at once and the row fails once,
	// by name; a give-up once the row has already failed is logged rather than failed again
	// (failOnce).
	var once sync.Once
	p.stop = func() {
		once.Do(func() {
			cancel()
			testwait.Receive(failOnce{t}, name+"'s outbox Run to return once its context ended", done)
		})
	}
	t.Cleanup(p.stop)
	return p
}

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

// holdPastTheRun sets publisherHoldOptions' TTL and Margin to stagedAhead and an hour for the rest
// of t, and puts them back once t ends, for a row whose verdict reads a hold held throughout: an
// outbox that takes the hold then reports it lost only 23 hours after its last confirmed renewal,
// and its row lapses on the database's clock a day after it, so no stall of the machine running
// the row loses it. On publisherHoldOptions' own 6 s and 2 s a renewal that goes unanswered for 4 s
// loses the hold, and the outbox gives it up and takes it again while the row reads it. Renew
// stays the package's, so the hold is still renewed every second.
func holdPastTheRun(t *testing.T) {
	t.Helper()
	saved := publisherHoldOptions
	publisherHoldOptions.TTL, publisherHoldOptions.Margin = stagedAhead, time.Hour
	t.Cleanup(func() { publisherHoldOptions = saved })
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
func waitForOnePublisher(t *testing.T, processes ...*process) *process {
	t.Helper()
	var holder *process
	testwait.Eventually(t, "exactly one outbox publishing", func() bool {
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
func waitForPublished(t *testing.T, database *store.Store, counter *sendCounter, event model.Event) {
	t.Helper()
	testwait.Eventually(t, fmt.Sprintf("event %d published", event.ID), func() bool {
		return publishedAt(t, database, event.ID) != nil
	})
	if total, most := counter.of(event.ID); total == 0 || most != 1 {
		t.Fatalf("event %d: %d envelopes sent, at most %d to one subject, want each of its subjects sent once", event.ID, total, most)
	}
}

// holderRest reads an outbox's holder at rest: waiting with nothing pending, a wait it entered
// with its LISTEN connection already up, and nothing scanned since. That is "the outbox has
// nothing left to do": every catch-up scan - the hold's take, and that connection's - has already
// run, and only a new wake (an event, a retry timer, a connection change) can make it scan again.
// A holder whose publisher reads disconnected also waits without scanning, so a row reads rest
// this way only with a publisher that stays connected.
//
// restingAt is the claims the outbox had made when the holder last entered its wait with nothing
// pending and the LISTEN connection already up, and -1 until it enters one. A wait entered with a
// wake pending records nothing, because the scan that wake causes is still to come; a wait
// entered before that connection is up records nothing either, because the connection's own
// catch-up scan is still to come. The holder stores this before it waits and scans only after, so
// while restingAt equals the claims made the holder is waiting and nothing has scanned since - a
// state that holds for as long as the outbox has nothing to do, rather than an edge a reader can
// arrive too late to see.
//
// Both hooks take mu, and the wait's own queue is read under it rather than handed in, because
// the two facts have to be read together: the holder could otherwise record a rest in the gap
// between the flag going up and the wake that follows it, and the scan that wake causes would
// then land after the rest. Under one lock a wait that runs before the connection is reported is
// refused by the flag, and one that runs while that wake is still queued is refused by the queue.
// A wait after the wake's own scan finds the queue empty and records a rest: by then that scan is
// one of the claims restingAt is compared with.
type holderRest struct {
	outbox    *Outbox
	mu        sync.Mutex
	listening bool
	restingAt atomic.Int64
}

// watchRest sets outbox's afterListening and afterServeWait to report its holder's rest. Call it
// before Run starts.
func watchRest(outbox *Outbox) *holderRest {
	rest := &holderRest{outbox: outbox}
	rest.restingAt.Store(-1)
	outbox.afterListening = func() {
		rest.mu.Lock()
		defer rest.mu.Unlock()
		rest.listening = true
		// One wake more, so the holder is certain to enter a wait after the flag is set.
		// OnConnected signals before it reports, so the holder may have taken that connection's
		// own wake, scanned and parked again while the flag was still false.
		outbox.signal()
	}
	outbox.afterServeWait = func() {
		rest.mu.Lock()
		defer rest.mu.Unlock()
		if rest.listening && len(outbox.wake) == 0 {
			rest.restingAt.Store(outbox.claims.Load())
		}
	}
	return rest
}

// await waits for the holder to be at rest.
func (r *holderRest) await(t *testing.T, what string) {
	t.Helper()
	testwait.Eventually(t, what, func() bool {
		// restingAt is read first, so the claims it is compared with are never older than it: an
		// equal pair is a wait no scan has followed, never a scan this read straddled.
		rested := r.restingAt.Load()
		return rested == r.outbox.claims.Load()
	})
}

// An idle outbox scans nothing: no claim runs while no event is written and no retry falls due.
// An event another process commits - which reaches this one only through the database, not its
// broker - is claimed at once by the LISTEN on dispatch_events, and a back-off is claimed when it
// falls due, by the one timer armed to it, with nothing else waking the outbox in between. Every
// step is read from what the outbox and the database recorded - the waits armed, the claims made,
// the moment the database marked the event published - and never from how long a step took. The
// hold lasts past the row (holdPastTheRun), since a hold lost and taken again scans once more.
func TestTheOutboxDoesNotScanWhileIdle(t *testing.T) {
	holdPastTheRun(t)
	database := storetest.Open(t)
	other := storetest.SecondProcess(t, database)
	seedIssue(t, database, "T-1", nil)
	publisher := &recordingPublisher{}
	outbox := New(Deps{Store: database, Publisher: publisher, Broker: events.NewBroker(), Listen: database.Pool.Listener})
	// The outbox arms its timers on a fake the row moves: the back-off's wait fires only when the
	// row moves the fake to it, and a timer of the outbox's own that a period would arm is read
	// from the fake's Armed rather than waited for.
	timers := clocktest.New(time.Now())
	outbox.clock = timers
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
	// The holder's rest, read before and after the idle window and the other process's event.
	rest := watchRest(outbox)
	go func() { outbox.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		testwait.Receive(failOnce{t}, "the outbox's Run to return once its context ended", done)
	})

	testwait.Eventually(t, "the outbox taking the publisher hold", outbox.Publishing)
	// The first scan and the LISTEN connection's catch-up run as the hold is taken; the row reads
	// that they are over rather than allowing them a span of time.
	rest.await(t, "the outbox resting after the hold's first scan and the LISTEN connection's catch-up")
	idleFrom := outbox.claims.Load()
	// A lower bound on quiet, not an upper bound on work: the row asserts nothing scanned in this
	// window, and load can only make an idle outbox quieter, never busier.
	testwait.Span(t, 12*time.Second, "a scan on a period would come from a timer of the wall clock's that "+
		"no state of the outbox names before it is armed (afterRetryArmed reports the retry timer alone), "+
		"and no later action's effect bounds a period, so only a span longer than any period proves none")
	if claims := outbox.claims.Load() - idleFrom; claims != 0 {
		t.Fatalf("an idle outbox ran %d claims in 12 s, want 0", claims)
	}
	if armed := timers.Armed(); len(armed) != 0 {
		t.Fatalf("an idle outbox holds timers for %v, want none: nothing it owes falls due", armed)
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
	testwait.Eventually(t, "the event another process committed claimed", func() bool {
		return outbox.claims.Load() != idleFrom
	})
	t.Logf("the event another process committed was claimed %s after its commit", time.Since(committed).Round(time.Millisecond))
	// The same hang guard: the claim above is this event's own scan, so the only way to this bound
	// is a publication that never happens at all.
	testwait.Eventually(t, "the other process's event published", func() bool { return publishedAt(t, database, fresh.ID) != nil })
	// The sequence below starts from an outbox at rest: the wait above returns as soon as the
	// other process's event is marked published, while the batch that published it has still to
	// start the batch that follows it and the scan has still to arm whatever comes of that.
	rest.await(t, "the outbox resting after the other process's event")

	// A back-off due stagedAhead from now, committed by the other process: its insert wakes the
	// outbox, whose scan finds nothing due and arms its one retry timer to the back-off's own
	// moment, and the timer's scan is the one that publishes it. The waits armed earlier in this
	// row are not this one's.
	armedBefore := len(armedWaits())
	// The waits armed since the insert, read afresh each time: the slice only grows.
	waitsSince := func() []armedWait { return armedWaits()[armedBefore:] }
	backedOff, _ := appendBackedOffEvent(t, other, stagedAhead)
	inserted := time.Now()
	testwait.Until(t, "the outbox arming a wait for the back-off its insert woke it to", func() (string, bool) {
		if since := waitsSince(); len(since) > 0 {
			return fmt.Sprintf("armed %v", since), true
		}
		return fmt.Sprintf("no wait armed since the insert (%d before it); the outbox has made %d claims", armedBefore, outbox.claims.Load()), false
	})
	rest.await(t, "the outbox resting with the back-off's wait armed")
	since := waitsSince()
	// The wait is a value the database computed from its own next_attempt_at, not an elapsed time
	// this test measured, so a loaded runner can only make it smaller: the only way past
	// stagedAhead is a wait armed for something that is not this back-off. nextRetry clamps a
	// back-off already due to zero, so a wait of zero is a timer armed to fire at once, which is
	// not the timer this row is about.
	if len(since) != 1 || since[0].wait <= 0 || since[0].wait > stagedAhead {
		t.Fatalf("the outbox armed %v once the back-off's insert woke it, want one wait above zero and no more than %s",
			since, stagedAhead)
	}
	// The back-off is still ahead on the database's clock, so a publication now is the
	// outbox publishing a back-off that was not yet due.
	if at, beforeDue := backOffState(t, database, backedOff.ID); at != nil || !beforeDue {
		t.Fatalf("the back-off reads published at %v, still ahead %v, before its wait fired; want it unpublished and ahead", at, beforeDue)
	}
	// The wait covers the whole back-off: it is next_attempt_at less the database's now() as the
	// scan read it (nextRetry), and what is left of the back-off now, read the same way at a later
	// now(), can only be less. A retry timer armed short of the back-off - a wait the outbox worked
	// out on another clock, or cut - reads below what is left, and would fire a scan that finds
	// nothing due.
	var left int64
	if err := database.Pool.QueryRow(context.Background(),
		`select (extract(epoch from next_attempt_at - now()) * 1000000)::bigint from events where id = $1`, backedOff.ID).Scan(&left); err != nil {
		t.Fatalf("read what is left of the back-off: %v", err)
	}
	if remaining := time.Duration(left) * time.Microsecond; since[0].wait < remaining {
		t.Fatalf("the outbox armed a %s wait for a back-off with %s still left on the database's clock, want at least that",
			since[0].wait, remaining)
	}
	// The back-off falls due on the database's clock, which no fake moves: the row brings its
	// next_attempt_at to now(), an update that notifies no one (events_notify fires on an insert,
	// 0107), so nothing wakes the outbox until the row fires its one timer.
	makeDue(t, other, backedOff)
	var due time.Time
	if err := database.Pool.QueryRow(context.Background(),
		`select next_attempt_at from events where id = $1`, backedOff.ID).Scan(&due); err != nil {
		t.Fatalf("read the back-off's moment: %v", err)
	}
	if made := outbox.claims.Load() - since[0].claims; made != 0 {
		t.Fatalf("the outbox made %d claims since it armed the %s wait for the back-off, before that wait fired, want none",
			made, since[0].wait.Round(time.Millisecond))
	}
	timers.Advance(since[0].wait)
	var published *time.Time
	testwait.Until(t, "the back-off published by the scan its wait woke", func() (string, bool) {
		at, ahead := backOffState(t, database, backedOff.ID)
		if at != nil {
			published = at
			return fmt.Sprintf("published at %s", at), true
		}
		return fmt.Sprintf("unpublished, still ahead on the database's clock: %t; %d claims since the wait was armed; timers armed: %v",
			ahead, outbox.claims.Load()-since[0].claims, timers.Armed()), false
	})
	if since := waitsSince(); len(since) != 1 {
		t.Fatalf("the outbox armed %d waits between the back-off's insert and its publish, want 1: %v", len(since), since)
	}
	// The publication time and the moment the back-off fell due are both the database's own
	// values, so no clock is compared with another.
	if published.Before(due) {
		t.Fatalf("the back-off was published at %s, %s before the %s it fell due, want no sooner than due",
			published, due.Sub(*published).Round(time.Millisecond), due)
	}
	t.Logf("the outbox armed a %s wait %s after the back-off's insert, and made %d claims from the wait's firing to the publish",
		since[0].wait.Round(time.Millisecond), since[0].at.Sub(inserted).Round(time.Millisecond),
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
// either process writes is sent once, by whichever holds the publisher hold. The row samples the
// two for as long as it takes the hold to be renewed a whole TTL past the deadline it had when the
// sampling began, so the samples span renewals and a lapse the hold would have come to unrenewed.
//
// The hold has publisherHoldOptions' shape scaled from seconds to hours: it lasts six hours past
// each renewal, is renewed every hour and reports itself lost two hours before it lapses. Both
// processes' holds and their bus clients' windows run on one fake (Outbox.holdClock,
// bus.WithWindowClock), which the row moves one renewal at a time, writing a message through
// either process at each move, and moves again only once the renewal the last move fired is
// confirmed: so no renewal comes late however slow the machine, which on the real clock a
// renewal unanswered for 4 s would, leaving neither process publishing until the other took the
// hold. The hold's row lasts six hours on the database's clock too, which no fake moves and no
// run reaches, so it never lapses there and the standby never takes it.
func TestOnlyOneOutboxPublishesAtATime(t *testing.T) {
	saved := publisherHoldOptions
	publisherHoldOptions.TTL, publisherHoldOptions.Renew, publisherHoldOptions.Margin = 6*time.Hour, time.Hour, 2*time.Hour
	t.Cleanup(func() { publisherHoldOptions = saved })
	_, url := testnats.Start(t)
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	counter := &sendCounter{}
	holds := clocktest.New(time.Now())
	onHolds := func(o *Outbox) { o.holdClock = holds }
	first := startProcessWith(t, "first", database, url, counter, onHolds, bus.WithWindowClock(holds))
	second := startProcessWith(t, "second", storetest.SecondProcess(t, database), url, counter, onHolds, bus.WithWindowClock(holds))
	holder := waitForOnePublisher(t, first, second)
	hold := holder.outbox.hold.Load()
	if hold == nil {
		t.Fatalf("%s gave the publisher hold up as soon as it was the one publishing", holder.name)
	}
	renewedPast := hold.Deadline().Add(publisherHoldOptions.TTL)

	var written []model.Event
	sample, moves := 0, 0
	// confirmed is the holder's deadline when the fake last moved: a later one is the renewal
	// that move fired, confirmed.
	confirmed := hold.Deadline()
	testwait.Eventually(t, "the publisher hold renewed a whole TTL past its deadline when the sampling began", func() bool {
		if names := publishing(first, second); len(names) != 1 {
			t.Fatalf("sample %d, %d renewals in: publishing %v, want exactly one outbox", sample, moves, names)
		}
		sample++
		for _, p := range []*process{first, second} {
			hold := p.outbox.hold.Load()
			if hold == nil {
				continue
			}
			if hold.Deadline().After(renewedPast) {
				return true
			}
			if moves == 0 || hold.Deadline().After(confirmed) {
				confirmed = hold.Deadline()
				writer := []*process{first, second}[moves%2]
				written = append(written, writeMessage(t, writer, fmt.Sprintf("message %d", moves)))
				moves++
				holds.Advance(publisherHoldOptions.Renew)
			}
		}
		return false
	})
	for _, event := range written {
		waitForPublished(t, database, counter, event)
	}
}

// A process that stops gracefully releases the publisher hold, and the other takes it over at once
// rather than when the hold would have lapsed. Each process arms its timers on a fake the row
// never moves, so the standby's timer to the holder's lapse never fires: only the release's wake
// (HoldReleasedChannel) can hand the hold over.
func TestAStoppedOutboxHandsThePublisherToTheOtherAtOnce(t *testing.T) {
	_, url := testnats.Start(t)
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	counter := &sendCounter{}
	first := startProcessOn(t, "first", database, url, counter, clocktest.New(time.Now()))
	second := startProcessOn(t, "second", storetest.SecondProcess(t, database), url, counter, clocktest.New(time.Now()))
	holder := waitForOnePublisher(t, first, second)
	standby := first
	if holder == first {
		standby = second
	}

	holder.stop()
	stopped := time.Now()
	testwait.Eventually(t, "the standby publishing once the holder stopped", standby.outbox.Publishing)
	t.Logf("the standby took the publisher hold %s after the holder stopped", time.Since(stopped).Round(time.Millisecond))
	event := writeMessage(t, standby, "after the hand-over")
	waitForPublished(t, database, counter, event)
}

// A holder whose NATS connection is cut releases the publisher hold once it has been without NATS
// for natsLossGrace, and the other process, whose NATS works, takes the hold and publishes what is
// written after the cut. The hold lasts past the row (holdPastTheRun): the row reads at one moment
// that the working process publishes, which a renewal of its own that came late would end.
func TestAnOutboxThatLosesNATSReleasesThePublisher(t *testing.T) {
	holdPastTheRun(t)
	_, url := testnats.Start(t)
	proxy := testnats.StartProxy(t, url)
	proxied := proxy.URL()
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	counter := &sendCounter{}
	cut := startProcess(t, "cut", database, proxied, counter)
	testwait.Eventually(t, "the first process taking the publisher hold", cut.outbox.Publishing)
	working := startProcess(t, "working", storetest.SecondProcess(t, database), url, counter)

	proxy.Cut()
	downAt := time.Now()
	testwait.Eventually(t, "the cut process releasing the publisher hold", func() bool {
		return cut.outbox.hold.Load() == nil
	})
	t.Logf("the cut process released the publisher hold %s after the cut", time.Since(downAt).Round(time.Millisecond))
	event := writeMessage(t, working, "after the cut")
	waitForPublished(t, database, counter, event)
	if !working.outbox.Publishing() || cut.outbox.Publishing() {
		t.Fatalf("publishing %v after the cut, want only the process whose NATS works", publishing(cut, working))
	}
}

// A holder whose NATS connection goes silent - still connected, nothing answering - finds out when
// a batch's sends go unanswered and a probe of their connection fails, and releases the publisher
// hold. It does not take the hold back while its connection stays silent, although the connection
// still reports itself connected: the other process, whose NATS works, takes it and publishes the
// event.
//
// The silent holder's hold and its bus client's windows run on a fake (Outbox.holdClock,
// bus.WithWindowClock), so the probe's bound, probeTimeout, is a deadline the row reads armed and
// moves the fake to: the hand-over comes at that bound and not one tick of the fake before it. The
// wait before the probe, for an acknowledgement NATS never sends, is nats.go's own
// (publishAcknowledgementTimeout), which the row waits for through the shared wait. The hold lasts
// on the database's clock, which no fake moves, and the silent holder renews it only as far as the
// row moves its fake, so the row gives the hold a TTL and Margin no run reaches: its row cannot
// lapse on the database, and the hand-over is the silent holder's release alone.
func TestAnOutboxWhoseNATSGoesSilentGivesThePublisherToTheOther(t *testing.T) {
	holdPastTheRun(t)
	_, url := testnats.Start(t)
	proxy := testnats.StartProxy(t, url)
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	counter := &sendCounter{}
	silenceStart := time.Now()
	silentClock := clocktest.New(silenceStart)
	silent := startProcessWith(t, "silent", database, proxy.URL(), counter,
		func(o *Outbox) { o.holdClock = silentClock }, bus.WithWindowClock(silentClock))
	testwait.Eventually(t, "the first process taking the publisher hold", silent.outbox.Publishing)
	// The hold runs on the silent holder's fake, which has not moved: its loss deadline is the
	// take's moment on it plus TTL less Margin.
	if got, want := silent.outbox.hold.Load().Deadline(), silenceStart.Add(publisherHoldOptions.TTL-publisherHoldOptions.Margin); !got.Equal(want) {
		t.Fatalf("the silent holder's hold has its loss deadline at %s, want %s on its fake: the take's moment plus TTL less Margin", got, want)
	}
	working := startProcess(t, "working", storetest.SecondProcess(t, database), url, counter)

	proxy.Freeze()
	t.Cleanup(proxy.Restore)
	event := writeMessage(t, working, "after the silence")
	// The silent holder sends the event, NATS acknowledges nothing, and the holder probes its
	// connection: a flush timed on its fake, armed probeTimeout past the fake's present.
	probeEnds := silenceStart.Add(probeTimeout)
	silentClock.AwaitArmedAt(t, fmt.Sprintf("the silent holder's probe of its connection, on its fake %s past the take,", probeTimeout), probeEnds)
	// One tick of the fake short of the probe's bound the probe still waits, and the hold is still
	// the silent holder's.
	silentClock.Set(probeEnds.Add(-time.Nanosecond))
	if armed := silentClock.Armed(); !slices.ContainsFunc(armed, probeEnds.Equal) {
		t.Fatalf("the silent holder's probe is no longer armed for %s one tick short of it: %v", probeEnds, armed)
	}
	var holding uuid.UUID
	if err := database.Pool.QueryRow(context.Background(), `select holder from duty_holds where name = $1`, publisherHold).Scan(&holding); err != nil {
		t.Fatalf("read the publisher hold's holder: %v", err)
	}
	if holding != silent.outbox.instance || publishedAt(t, database, event.ID) != nil {
		t.Fatalf("one tick short of the probe's bound the hold names %s (the silent holder is %s) and the event is published %v; want the silent holder still holding and the event unpublished",
			holding, silent.outbox.instance, publishedAt(t, database, event.ID) != nil)
	}
	silentClock.Set(probeEnds)
	testwait.Eventually(t, "the working process publishing the event once the silent holder's probe ran out", func() bool {
		return working.outbox.Publishing() && publishedAt(t, database, event.ID) != nil
	})
	// The silent process cannot publish again while the working one holds the hold: the hold is one
	// row, which names the working process and which it renews, so the silent process could take it
	// back only once that row is released or lapses, and neither happens in this row. Its own hold
	// is gone, so it does not publish on one it still believes it has.
	var holder uuid.UUID
	if err := database.Pool.QueryRow(context.Background(), `select holder from duty_holds where name = $1`, publisherHold).Scan(&holder); err != nil {
		t.Fatalf("read the publisher hold's holder: %v", err)
	}
	if holder != working.outbox.instance || silent.outbox.hold.Load() != nil || silent.outbox.Publishing() {
		t.Fatalf("the hold names %s (the working process is %s), and the silent process still has a hold of its own: %v, publishing %v; want the working process alone to hold it",
			holder, working.outbox.instance, silent.outbox.hold.Load() != nil, publishing(silent, working))
	}
}

// A holder that stops renewing without a release - a killed process - leaves its row live until
// the row lapses on the database's clock, TTL after its last renewal (publisherHoldOptions), and
// a standby takes the hold when it lapses: a process that is killed is replaced within TTL of
// its last renewal. The standby reads what is left of the hold's row on the database's clock
// (duty.Expiry) and arms its timer that far past its present on a fake (Outbox.clock), and takes
// the hold only once the fake reaches that timer: the row lapses the hold on the database's clock
// itself, so the timer is what brings the take.
//
// The row ties the wait the standby armed, its timer less the fake's present, to the hold's row
// on both sides. Once the timer is armed, the row reads what is left of the hold's row on the
// database's clock, in microseconds as duty.Expiry reads it. The standby read it earlier on that
// clock, so its wait is no shorter than what is left, and no longer than stagedAhead, all the
// hold's row had when it was written. A standby that arms a fixed wait, the hold's TTL or half an
// hour, arms short of what is left; one that arms past the lapse arms beyond stagedAhead. The
// wait the standby arms and both bounds are whole microseconds, so they are compared exactly.
//
// The killed holder's row is written to lapse stagedAhead past the database's now(), so it stays
// live past anything the standby's start waits for, however slow, until the row lapses it itself:
// a row written to lapse TTL on would lapse on its own while a slow start was still connecting,
// and the standby would then take the hold at its first try, arming no timer.
func TestAStandbyTakesTheHoldOfAHolderThatStoppedRenewingOnceItsRowLapses(t *testing.T) {
	_, url := testnats.Start(t)
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	// The row the killed holder's last renewal left: its holder and the lapse that renewal wrote.
	killed := uuid.New()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into duty_holds (name, holder, expires_at) values ($1, $2, now() + make_interval(secs => $3))
	`, publisherHold, killed, stagedAhead.Seconds()); err != nil {
		t.Fatalf("write the killed holder's last renewal: %v", err)
	}
	start := time.Now()
	timers := clocktest.New(start)
	standby := startProcessOn(t, "standby", database, url, &sendCounter{}, timers)
	armed := timers.AwaitArmed(t, "the standby's timer to the killed holder's lapse", 1)
	// What is left of the killed holder's row on the database's clock now, in microseconds as
	// duty.Expiry reads it: the standby read it before it armed, so the wait it armed is no shorter.
	var leftMicros int64
	if err := database.Pool.QueryRow(context.Background(),
		`select (extract(epoch from expires_at - now())*1000000)::bigint from duty_holds where name = $1`, publisherHold).Scan(&leftMicros); err != nil {
		t.Fatalf("read what is left of the killed holder's row: %v", err)
	}
	left := time.Duration(leftMicros) * time.Microsecond
	if wait := armed[0].Sub(start); len(armed) != 1 || wait < left || wait > stagedAhead {
		t.Fatalf("the standby armed %v, %s past the fake's present %s, for a hold whose row had %s left on the database's clock once that timer was armed and %s when it was written: want one timer no less than what was left and no more than %s past the fake's present, the lapse the standby read before it armed",
			armed, wait, start, left, stagedAhead, stagedAhead)
	}
	// The killed holder's row lapses on the database's clock, which notifies no one.
	if _, err := database.Pool.Exec(context.Background(), `update duty_holds set expires_at = now() where name = $1`, publisherHold); err != nil {
		t.Fatalf("lapse the killed holder's row: %v", err)
	}
	// One tick short of its timer the standby has not tried again: the timer is still armed, and it
	// does not publish.
	timers.Set(armed[0].Add(-time.Nanosecond))
	if still := timers.Armed(); len(still) != 1 || !still[0].Equal(armed[0]) || standby.outbox.Publishing() {
		t.Fatalf("one tick short of the standby's timer it holds timers %v and publishing is %v, want its timer for %s still armed and no take",
			still, standby.outbox.Publishing(), armed[0])
	}
	timers.Set(armed[0])
	testwait.Eventually(t, "the standby taking the lapsed hold once its timer fired", standby.outbox.Publishing)
	var holder uuid.UUID
	if err := database.Pool.QueryRow(context.Background(), `select holder from duty_holds where name = $1`, publisherHold).Scan(&holder); err != nil {
		t.Fatalf("read the publisher hold's holder: %v", err)
	}
	if holder != standby.outbox.instance {
		t.Fatalf("the hold names %s once the standby's timer fired, want the standby %s", holder, standby.outbox.instance)
	}
}

// A standby waits for the publisher hold to be released or to lapse, or for its NATS connection to
// change, and for nothing else: the events the holder writes and publishes do not make it try the
// hold. The standby arms its timer to the holder's lapse on a fake the row never moves, so that
// timer never fires, and 100 events written one after another while the holder holds send not one
// take and not one lapse read, where a standby woken by events would put a write on duty_holds
// for every event.
//
// The holder must hold throughout: a holder whose hold is lost gives it up, and the standby then
// rightly tries it, which this row would read as a try an event caused. So the holder's hold runs
// on a fake of its own (Outbox.holdClock, and its bus client's windows, bus.WithWindowClock), which
// the row does not move while it counts: no renewal falls due, and the loss deadline, the take's
// moment plus TTL less Margin, comes only when the row moves the fake to it, however slow the
// machine. The hold lasts on the database's clock too, which no fake moves and which the holder
// renews only as far as its fake moves, so the row gives the hold a TTL and Margin no run reaches:
// its row cannot lapse there, and the holder's records, which check it there, are never refused.
//
// Once it has counted, the row brings the loss deadline itself. From then on the holder's renewals
// go unanswered (unansweredRenewals), as a database too loaded to answer leaves them, so the move
// that fires the next renewal moves the deadline no further. One tick of the fake short of the
// deadline the holder still publishes and the standby has sent nothing; at it the holder's hold is
// lost, the holder releases it, and the standby, woken by that release, tries the hold once. The
// holder, whose NATS works, tries it again at once too, so either of them may take it.
func TestAStandbyDoesNotTryTheHoldForEveryEventTheHolderWrites(t *testing.T) {
	holdPastTheRun(t)
	_, url := testnats.Start(t)
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	counter := &sendCounter{}
	holderStart := time.Now()
	holderClock := clocktest.New(holderStart)
	renewals := &unansweredRenewals{}
	holder := startProcessWith(t, "holder", database, url, counter, func(o *Outbox) {
		o.holdClock = holderClock
		o.deps.Store = &store.Store{Pool: storetest.Traced(t, o.deps.Store.Pool, renewals, nil)}
	}, bus.WithWindowClock(holderClock))
	testwait.Eventually(t, "the holder taking the publisher hold", holder.outbox.Publishing)
	// The hold runs on the holder's fake, which has not moved: its loss deadline is the take's
	// moment on it plus TTL less Margin.
	lapse := holderStart.Add(publisherHoldOptions.TTL - publisherHoldOptions.Margin)
	if got := holder.outbox.hold.Load().Deadline(); !got.Equal(lapse) {
		t.Fatalf("the holder's hold has its loss deadline at %s, want %s on its fake: the take's moment plus TTL less Margin", got, lapse)
	}
	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect the standby's NATS client: %v", err)
	}
	t.Cleanup(func() { bustest.Close(t, client) })
	tracer := &statementTracer{}
	standby := New(Deps{
		Store:     tracedStore(t, storetest.SecondProcess(t, database), tracer),
		Publisher: client,
		Broker:    events.NewBroker(),
		Listen:    database.Pool.Listener,
	})
	standby.clock = clocktest.New(time.Now())
	// restingAt is the takes the standby had sent when it last entered its wait with no wake pending
	// and its LISTEN connection already up, and -1 until it enters one: while it equals the takes
	// sent, the standby is waiting and has tried nothing since, so the catch-up try that
	// connection's wake causes is over before the count below starts. Both hooks take restMu, so a
	// rest is never recorded between the flag going up and the wake that follows it (as holderRest
	// reads the holder's rest).
	var restMu sync.Mutex
	listening := false
	var restingAt atomic.Int64
	restingAt.Store(-1)
	standby.afterListening = func() {
		restMu.Lock()
		defer restMu.Unlock()
		listening = true
		// One wake more, so the standby is certain to enter a wait after the flag is set.
		standby.signalStandby()
	}
	standby.afterStandbyWait = func() {
		restMu.Lock()
		defer restMu.Unlock()
		if listening && len(standby.standbyWake) == 0 {
			restingAt.Store(int64(len(tracer.matching("insert into duty_holds"))))
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { standby.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		testwait.Receive(failOnce{t}, "the standby's Run to return once its context ended", done)
	})
	testwait.Eventually(t, "the standby resting after its first try and its LISTEN connection's catch-up", func() bool {
		// restingAt is read first, so the takes it is compared with are never older than it.
		rested := restingAt.Load()
		return rested > 0 && rested == int64(len(tracer.matching("insert into duty_holds")))
	})

	takes, reads := len(tracer.matching("insert into duty_holds")), len(tracer.matching("from duty_holds where name"))
	var last model.Event
	for index := range 100 {
		last = writeMessage(t, holder, fmt.Sprintf("message %d", index))
	}
	waitForPublished(t, database, counter, last)
	if standby.Publishing() {
		t.Fatal("the standby publishes while the holder holds the publisher hold")
	}
	takes = len(tracer.matching("insert into duty_holds")) - takes
	reads = len(tracer.matching("from duty_holds where name")) - reads
	t.Logf("while 100 events were written and published, the standby sent %d takes of the hold and %d reads of its lapse", takes, reads)
	if takes != 0 || reads != 0 {
		t.Fatalf("the standby sent %d takes of the publisher hold and %d reads of its lapse while the holder wrote 100 events, want none: nothing but a release, a lapse or its connection changing wakes it",
			takes, reads)
	}

	// The holder's renewals go unanswered from here. One tick of its fake short of the loss
	// deadline, a move that fires its next renewal, the hold is still the holder's, so nothing has
	// released it and the standby has sent nothing.
	renewals.unanswer()
	takes, reads = len(tracer.matching("insert into duty_holds")), len(tracer.matching("from duty_holds where name"))
	holderClock.Set(lapse.Add(-time.Nanosecond))
	if !holder.outbox.Publishing() {
		t.Fatalf("one tick short of its loss deadline %s on its fake the holder no longer publishes: its hold lapsed before its deadline", lapse)
	}
	if sentTakes, sentReads := len(tracer.matching("insert into duty_holds"))-takes, len(tracer.matching("from duty_holds where name"))-reads; sentTakes != 0 || sentReads != 0 {
		t.Fatalf("one tick short of the holder's loss deadline the standby sent %d takes of the publisher hold and %d reads of its lapse, want none: the holder still held it",
			sentTakes, sentReads)
	}
	// At the deadline the holder's hold is lost, the holder releases it, and the release wakes the
	// standby, which tries the hold: it takes it, or, when the holder took it again first, reads its
	// lapse and waits again.
	holderClock.Set(lapse)
	testwait.Eventually(t, "the standby trying the hold once the holder released the hold that lapsed at its deadline", func() bool {
		// restingAt is read first, so the takes it is compared with are never older than it.
		rested := restingAt.Load()
		sent := int64(len(tracer.matching("insert into duty_holds")))
		return sent > int64(takes) && (standby.Publishing() || rested == sent)
	})
	if tried := len(tracer.matching("insert into duty_holds")) - takes; tried != 1 {
		t.Fatalf("the standby sent %d takes of the publisher hold once the holder released the hold that lapsed at its deadline, want one: the release wakes it once",
			tried)
	}
	t.Logf("once the holder released the hold that lapsed at its deadline, the standby sent one take of it and %d reads of its lapse, and publishes: %t",
		len(tracer.matching("from duty_holds where name"))-reads, standby.Publishing())
}

// unansweredRenewals is a pgx tracer that, once unanswer is called, holds every renewal of a hold
// until that renewal's own context ends, as a database too loaded to answer leaves it: no renewal
// is confirmed, so the hold's loss deadline stays where the last confirmed one left it. A renewal's
// context ends at its bound on the hold's clock or when the hold is released (duty.Hold.renew).
// Every other statement passes, the release among them.
type unansweredRenewals struct{ on atomic.Bool }

// unanswer holds every renewal sent from now on.
func (u *unansweredRenewals) unanswer() { u.on.Store(true) }

func (u *unansweredRenewals) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if u.on.Load() && strings.Contains(data.SQL, "update duty_holds set expires_at = now() + make_interval") {
		<-ctx.Done()
	}
	return ctx
}

func (u *unansweredRenewals) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// fenceAfterFirstSend is a publisher hold whose deadline passes as the batch's first envelope is
// sent, as a process that froze mid-batch past its deadline finds when it resumes.
type fenceAfterFirstSend struct {
	recordingPublisher
	passed atomic.Bool
}

func (p *fenceAfterFirstSend) WindowUntil(deadline func() time.Time) bus.Sender {
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

func (p *reconnectedQuietly) WindowUntil(deadline func() time.Time) bus.Sender {
	return deadlineWindow{Sender: p, deadline: deadline}
}

// A holder whose client reads connected again when natsLossGrace runs out serves on, though no
// connection event came: it reads the client's status as the grace ends, keeps the hold and scans,
// rather than giving the hold up for a loss that is over. The grace is armed on a fake clock, so
// the row reads the grace serve armed and ends it itself.
func TestAHolderConnectedAgainWhenTheGraceEndsKeepsTheHold(t *testing.T) {
	database := storetest.Open(t)
	publisher := &reconnectedQuietly{}
	outbox := New(Deps{Store: database, Publisher: publisher})
	epoch := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	grace := clocktest.New(epoch)
	outbox.clock = grace
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hold, _ := outbox.tryHold(ctx)
	if hold == nil {
		t.Fatal("the outbox did not take the publisher hold")
	}
	t.Cleanup(func() { _ = hold.Release(context.Background()) })

	// The holder calls afterServeWait each time it is about to wait; a holder whose client reads
	// disconnected arms its grace there and waits without having scanned (serve).
	waiting := make(chan struct{}, 1)
	outbox.afterServeWait = func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}
	ended := make(chan holdEnd, 1)
	go func() { ended <- outbox.serve(ctx, hold) }()
	testwait.Receive(t, "the holder waiting with its client disconnected", waiting)
	if armed := grace.Armed(); len(armed) != 1 || !armed[0].Equal(epoch.Add(natsLossGrace)) {
		t.Fatalf("the holder waiting with its client disconnected armed %v, want its grace alone, natsLossGrace = %s on",
			armed, natsLossGrace)
	}
	if outbox.claims.Load() != 0 {
		t.Fatal("the holder scanned while its client read disconnected")
	}
	publisher.connected.Store(true)
	grace.Advance(natsLossGrace)
	// No wake reaches this holder - serve runs alone, with no LISTEN connection, broker or
	// connection change - so the one scan that can come is the one serve makes when the grace
	// ends and it finds the client connected. A holder that gave the hold up there returns
	// natsLost instead, and never scans.
	testwait.Eventually(t, "the holder scanning once it found its client connected at the grace's end", func() bool {
		return outbox.claims.Load() != 0
	})
	select {
	case end := <-ended:
		t.Fatalf("the holder stopped serving (%s) though its client read connected when the grace ended", end)
	default:
	}
	cancel()
	if end := testwait.Receive(t, "serve to return once ctx ended", ended); end != stopping {
		t.Fatalf("serve ended %s once ctx ended, want %s", end, stopping)
	}
}

// steppingClock is a fake whose every read of Now moves it on a nanosecond first, so the moments
// one goroutine reads on it come out in the order it read them, each its own: a row that brackets
// the moments the code read between two reads of its own judges their order on the fake, and no
// two of them can land on one moment. It moves by nothing else.
type steppingClock struct{ *clocktest.Fake }

func (c steppingClock) Now() time.Time {
	c.Advance(time.Nanosecond)
	return c.Fake.Now()
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
	// that property is not the publisher hold's production numbers. The hold's renewals, its
	// loss timer and the outbox's reads of the moment (holdClock) run on one fake, as they do on
	// one clock in production, which moves only a nanosecond at each read (steppingClock): no
	// renewal tick comes however slow the machine, so the record path is the one that finds the
	// row another's, and what the fake has armed afterwards is the hold's own record of whether
	// its renewal stopped. The hold is the outbox's own, under its token and instance, so the batch
	// that records under it finds the row another's exactly as it would in production.
	renewals := steppingClock{clocktest.New(time.Now())}
	outbox.holdClock = renewals
	recordPath := duty.HoldOptions{TTL: 6 * time.Minute, Renew: 5 * time.Second, Margin: time.Minute, Token: outbox.token, Clock: renewals}
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

	began := renewals.Now()
	if end := outbox.publish(ctx, hold); end != holdLost {
		t.Fatalf("publish ended %s, want %s", end, holdLost)
	}
	returned := renewals.Now()
	// No renewal tick can come on the hold's clock, which moves a nanosecond a read and so never
	// reaches the first, so a renewal sent before the record path ended the turn would be the hold
	// renewing without one.
	if renewed := len(tracer.matching("update duty_holds set expires_at")); renewed != 0 {
		t.Fatalf("publish returned after %d renewals of the row's own hold were sent, on a clock that brought no renewal tick", renewed)
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
	// began < stopped < stopped_by < returned, with stopped_by still short of the deadline.
	// began is read on the fake just ahead of publish and returned just after it, so the pair is
	// held inside the turn itself, which is what separates serve's return from a deadline minutes
	// further on; a stopped that is too early - a zero value, or one dated before this turn began -
	// would pass every bound from the future and is caught by the first. The middle comparison is
	// the reason this row reads stopped_by at all: this is the one path here where the two are not
	// the same capped value, so the two clocks read in the wrong order, or the two attributes
	// swapped, leave an upper bound earlier than the lower one and fail here. On the stepping fake
	// no two reads land on one moment, so each comparison is strict.
	//
	// What no comparison here can catch is which side of o.hold.Store(nil) each clock was read
	// on. Moving the second read above the store, and leaving it after the first, keeps the two
	// in order and every bound below holding; the line is written after both reads and after the
	// store, so nothing it carries says where the store fell between them. No row holds that
	// placement: publish's doc comment records it, and the one reader that compares /healthz with
	// stopped_by, the two-process row, sees only turns that end at the hold's deadline, where
	// stopped_by is the deadline wherever the read sits.
	if !began.Before(stopped) {
		t.Errorf("the line dates the stop at %s, not after publish was called at %s",
			stopped.Format(time.RFC3339Nano), began.Format(time.RFC3339Nano))
	}
	if !stopped.Before(stoppedBy) {
		t.Errorf("the line bounds the stop above by %s, not after the %s it bounds it below by",
			stoppedBy.Format(time.RFC3339Nano), stopped.Format(time.RFC3339Nano))
	}
	if !stoppedBy.Before(returned) {
		t.Errorf("the line bounds the stop above by %s, not before the turn ended at %s",
			stoppedBy.Format(time.RFC3339Nano), returned.Format(time.RFC3339Nano))
	}
	if !stoppedBy.Before(deadline) {
		t.Errorf("the line bounds the stop above by %s, at or past the loss deadline of %s this path leaves minutes ahead",
			stoppedBy.Format(time.RFC3339Nano), deadline.Format(time.RFC3339Nano))
	}

	// The renewal of the hold just lost must stop when publish released it. Until it does, its
	// next tick stays armed on the hold's clock beside the loss timer; a duty.Hold whose Release
	// does not stop the renewal, and whose renew does not return when Lost closes, keeps that tick
	// armed for good. Once nothing is armed there, no renewal of this hold can ever be sent.
	testwait.Until(t, "the lost hold's renewal tick and loss timer to be stopped", func() ([]time.Time, bool) {
		armed := renewals.Armed()
		return armed, len(armed) == 0
	})
	if got := len(tracer.matching("update duty_holds set expires_at")); got != 0 {
		t.Fatalf("%d renewals of the lost hold were sent, want none", got)
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

// timeIn is the key attribute, read as the RFC3339Nano string the code logs, of the first record
// the recorder kept whose message holds message, and false when no record says it yet, so a row
// can both wait for the line and read a moment it carries. It reads the records the code wrote
// rather than a rendering of them: slog's text handler truncates a record's own time to the
// millisecond and quotes what it writes, so a row that cut its output on "stopped=" and trimmed
// the quotes off asserted on a layer that is not the code under test.
//
// It takes the attribute's name because the lost and released lines carry two bounds on the
// moment the process stopped publishing, stopped and stopped_by, and a row that reads only the
// first leaves their order, their place inside the turn and their distance from the deadline
// unheld.
func timeIn(t *testing.T, recorder *logtest.Recorder, message, key string) (time.Time, bool) {
	t.Helper()
	for _, record := range recorder.Records() {
		if !strings.Contains(record.Message, message) {
			continue
		}
		value, found := "", false
		record.Attrs(func(attribute slog.Attr) bool {
			if attribute.Key != key {
				return true
			}
			value, found = attribute.Value.String(), true
			return false
		})
		if !found {
			t.Fatalf("the record %q carries no %s attribute", record.Message, key)
		}
		moment, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			t.Fatalf("read the %s attribute %q of the record %q: %v", key, value, record.Message, err)
		}
		return moment, true
	}
	return time.Time{}, false
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
// already in the log while the release's own statement is still running, and its stopped and
// stopped_by attributes name the moment the process stopped publishing rather than the moment it
// was written: on this path both are the hold's deadline, which the loss pulled back to the
// moment the hold was lost and which the hold keeps from then on (Deadline).
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
	testwait.Receive(t, "the hold lost once its row went", hold.Lost())

	testwait.Receive(t, "the holder running the release", held.started)
	if _, ok := timeIn(t, recorder, "lost the publisher hold", "stopped"); !ok {
		t.Fatalf("the holder's release is running and it has not logged that it lost the hold yet; it logged %q", loggedMessages(recorder))
	}
	held.let()
	if end := testwait.Receive(t, "publish to return once the release ran", ended); end != holdLost {
		t.Fatalf("publish ended %s, want %s", end, holdLost)
	}

	stopped, ok := timeIn(t, recorder, "lost the publisher hold", "stopped")
	if !ok {
		t.Fatalf("no record of the log says %q; it says %q", "lost the publisher hold", loggedMessages(recorder))
	}
	stoppedBy, _ := timeIn(t, recorder, "lost the publisher hold", "stopped_by")
	if deadline := hold.Deadline(); !stopped.Equal(deadline) || !stoppedBy.Equal(deadline) {
		t.Errorf("the line dates the stop at %s and bounds it by %s, want both at %s: the deadline the loss pulled back to the moment the hold was lost",
			stopped.Format(time.RFC3339Nano), stoppedBy.Format(time.RFC3339Nano), deadline.Format(time.RFC3339Nano))
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

	testwait.Eventually(t, fmt.Sprintf("all %d held copies published", copies), func() bool {
		return publishedCopies(t, database, commentID) == copies
	})
	cancel()
	if end := testwait.Receive(t, "serve to return once its context ended", served); end != stopping {
		t.Fatalf("serve ended %q, want it to stop with its context", end)
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
	// The retry timer is armed on a fake clock, which moves only when the row moves it: a second
	// pass before the row moves it is a holder that did not wait for its timer.
	epoch := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	retries := clocktest.New(epoch)
	outbox.clock = retries
	waiting := make(chan struct{}, 1)
	outbox.afterServeWait = func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}
	hold, _ := outbox.tryHold(ctx)
	if hold == nil {
		t.Fatal("the outbox did not take the free publisher hold")
	}
	t.Cleanup(func() { _ = hold.Release(context.Background()) })
	served := make(chan holdEnd, 1)
	go func() { served <- outbox.serve(ctx, hold) }()

	// serve arms the retry after the pass and then waits, so once it waits the first pass is over;
	// a holder that passes again without waiting never gets there, and its second pass ends the wait.
	testwait.Eventually(t, "the holder waiting after its first pass", func() bool {
		if passes := failedClaims(&logged); passes > 1 {
			t.Fatalf("the holder passed %d times before it first waited, want once rather than spin", passes)
		}
		select {
		case <-waiting:
			return true
		default:
			return false
		}
	})
	if armed := retries.Armed(); len(armed) != 1 || !armed[0].Equal(epoch.Add(retry.BaseDelay)) {
		t.Fatalf("the holder armed %v after a pass whose claim failed, want one retry, retry.BaseDelay = %s on", armed, retry.BaseDelay)
	}
	if passes := failedClaims(&logged); passes != 1 {
		t.Fatalf("the holder passed %d times before its retry fell due, want once rather than spin", passes)
	}
	retries.Advance(retry.BaseDelay)
	testwait.Eventually(t, "a second pass of the claim that keeps failing", func() bool {
		return failedClaims(&logged) >= 2
	})
	cancel()
	if end := testwait.Receive(t, "serve to return once its context ended", served); end != stopping {
		t.Fatalf("serve ended %q, want it to stop with its context", end)
	}
}
