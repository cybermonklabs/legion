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
	client, err := bus.ConnectOwningStream([]string{url})
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
		testwait.Receive(t, name+"'s outbox stopping once its context ended", done)
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

// backOffDue is how long the back-off of the row below is due in, and the one bound the row puts
// on the outbox's own work: the insert's notification must reach the holder, and the scan it
// wakes must read next_attempt_at, inside it. A scan later than that finds the back-off already
// due and publishes it there and then, arming no wait at all, which the row catches where it
// requires exactly one wait armed between the insert and the publish - so it still fails on what
// happened rather than on a clock, but it would be failing for the machine's slowness and not
// the outbox's, which is why this is ten seconds against the single-digit milliseconds that work
// takes on an idle pod.
const backOffDue = 10 * time.Second

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
	// The holder's rest, read before and after the idle window and the other process's event.
	rest := watchRest(outbox)
	go func() { outbox.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

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
	// A local clock for the readings logged below alone: no assertion reads it.
	inserted := time.Now()
	// What each read of the loop below saw: the back-off's publication once there is one, and the
	// waits armed since the insert, which a wait that gives up prints.
	type backOffReading struct {
		published *time.Time
		armed     []armedWait
	}
	reading := testwait.Until(t, fmt.Sprintf("the back-off due in %s published", backOffDue), func() (backOffReading, bool) {
		// Read oldest first, so nothing is judged against a reading younger than itself: the waits,
		// then the claims (never fewer than the waits' own count), then the database's answer,
		// whose now() is therefore no earlier than the moment those claims were read.
		since := waitsSince()
		claims := outbox.claims.Load()
		at, beforeDue := backOffState(t, database, backedOff.ID)
		if at != nil {
			return backOffReading{published: at, armed: since}, true
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
		return backOffReading{armed: since}, false
	})
	published := *reading.published
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
// either process writes is sent once, by whichever holds the publisher hold. The row samples the
// two for as long as it takes the hold to be renewed a whole TTL past the deadline it had when the
// sampling began, so the samples span renewals and a lapse the hold would have come to unrenewed.
func TestOnlyOneOutboxPublishesAtATime(t *testing.T) {
	_, url := testnats.Start(t)
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	counter := &sendCounter{}
	first := startProcess(t, "first", database, url, counter)
	second := startProcess(t, "second", storetest.SecondProcess(t, database), url, counter)
	holder := waitForOnePublisher(t, first, second)
	hold := holder.outbox.hold.Load()
	if hold == nil {
		t.Fatalf("%s gave the publisher hold up as soon as it was the one publishing", holder.name)
	}
	renewedPast := hold.Deadline().Add(publisherHoldOptions.TTL)

	var written []model.Event
	sample := 0
	testwait.Eventually(t, "the publisher hold renewed a whole TTL past its deadline when the sampling began", func() bool {
		if names := publishing(first, second); len(names) != 1 {
			t.Fatalf("sample %d: publishing %v, want exactly one outbox", sample, names)
		}
		if sample%50 == 0 {
			writer := []*process{first, second}[(sample/50)%2]
			written = append(written, writeMessage(t, writer, fmt.Sprintf("message %d", sample)))
		}
		sample++
		for _, p := range []*process{first, second} {
			if hold := p.outbox.hold.Load(); hold != nil && hold.Deadline().After(renewedPast) {
				return true
			}
		}
		return false
	})
	for _, event := range written {
		waitForPublished(t, database, counter, event)
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
// written after the cut.
func TestAnOutboxThatLosesNATSReleasesThePublisher(t *testing.T) {
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
// event, within 10 s of the silence.
func TestAnOutboxWhoseNATSGoesSilentGivesThePublisherToTheOther(t *testing.T) {
	_, url := testnats.Start(t)
	proxy := testnats.StartProxy(t, url)
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	counter := &sendCounter{}
	silent := startProcess(t, "silent", database, proxy.URL(), counter)
	testwait.Eventually(t, "the first process taking the publisher hold", silent.outbox.Publishing)
	working := startProcess(t, "working", storetest.SecondProcess(t, database), url, counter)

	proxy.Freeze()
	t.Cleanup(proxy.Restore)
	frozenAt := time.Now()
	event := writeMessage(t, working, "after the silence")
	testwait.Eventually(t, "the working process publishing the event", func() bool {
		return working.outbox.Publishing() && publishedAt(t, database, event.ID) != nil
	})
	t.Logf("the working process published the event %s after the silence", time.Since(frozenAt).Round(time.Millisecond))
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

// A standby waits for the publisher hold to be released or to lapse, or for its NATS connection to
// change, and for nothing else: the events the holder writes and publishes do not make it try the
// hold. 100 events written one after another while the holder holds send at most the few takes and
// lapse reads the standby's timer to the holder's lapse makes over that span (each renewal moves the
// lapse 6 s on), not one take and one read per event, which would put a write on duty_holds for
// every event on every standby.
func TestAStandbyDoesNotTryTheHoldForEveryEventTheHolderWrites(t *testing.T) {
	_, url := testnats.Start(t)
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	counter := &sendCounter{}
	holder := startProcess(t, "holder", database, url, counter)
	testwait.Eventually(t, "the holder taking the publisher hold", holder.outbox.Publishing)

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
	t.Cleanup(func() { cancel(); <-done })
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
	if takes > 3 || reads > 3 {
		t.Fatalf("the standby sent %d takes of the publisher hold and %d reads of its lapse while the holder wrote 100 events, want at most 3 of each: its timer to the lapse alone",
			takes, reads)
	}
}

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
	// assertions below fail as if the product were wrong. The renewal's statement is the one
	// record of whether it ran: nothing else in this process renews a hold.
	if renewed := len(tracer.matching("update duty_holds set expires_at")); renewed != 0 {
		t.Fatalf("publish returned after %d renewals of the row's own hold were sent: the row is about a record finding the hold another's before any renewal does", renewed)
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
	testwait.Span(t, time.Until(taken.Add(window)), "a renewal still running would send its statement at the "+
		"first renewal tick, a wall-clock timer inside duty.Hold that no state the hold exposes names and no "+
		"later action follows, so only a span past that tick shows none was sent")
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

	// The row goes, so the next renewal affects no row and the hold is lost there and then. The
	// loss can come no earlier than this moment, read before the row goes.
	rowGone := time.Now()
	if _, err := database.Pool.Exec(ctx, `delete from duty_holds where name = $1`, publisherHold); err != nil {
		t.Fatalf("take the hold's row away: %v", err)
	}
	testwait.Receive(t, "the hold lost once its row went", hold.Lost())
	lostAt := time.Now()

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
	if stopped.Before(rowGone) || stopped.After(lostAt) {
		t.Errorf("the line dates the stop at %s, want it between the row going at %s and the hold reporting itself lost at %s",
			stopped.Format(time.RFC3339Nano), rowGone.Format(time.RFC3339Nano), lostAt.Format(time.RFC3339Nano))
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
