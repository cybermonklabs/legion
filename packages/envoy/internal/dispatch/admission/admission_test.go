package admission

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// patient is a wait no test reaches: a test that wants a waiter to stay queued gives it this, so
// only the test's own release or cancellation moves it.
const patient = time.Minute

// newLimiter gives both classes the same wait, so a test that names a wait gets it whichever
// class it then admits; a zero wait leaves each class its own default (DefaultReadWait,
// DefaultWriteWait).
func newLimiter(read, write int, wait time.Duration) *Limiter {
	return New(Limits{Read: read, Write: write, ReadWait: wait, WriteWait: wait})
}

func admit(t *testing.T, l *Limiter, class Class, weight int) *Ticket {
	t.Helper()
	ticket, err := l.Admit(context.Background(), class, weight)
	if err != nil {
		t.Fatalf("admit a %s of weight %d: %v", class, weight, err)
	}
	return ticket
}

func classStats(l *Limiter, class Class) ClassStats { return l.Stats()[class] }

// waitForQueued returns once class has queued requests waiting.
func waitForQueued(t *testing.T, l *Limiter, class Class, queued int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for classStats(l, class).Queued != queued {
		if time.Now().After(deadline) {
			t.Fatalf("%s queue holds %d, want %d", class, classStats(l, class).Queued, queued)
		}
		time.Sleep(time.Millisecond)
	}
}

// admitInBackground starts an Admit and answers the channel its outcome arrives on, once the
// request is queued.
func admitInBackground(t *testing.T, l *Limiter, ctx context.Context, class Class, weight int) <-chan admitted {
	t.Helper()
	queuedBefore := classStats(l, class).Queued
	outcome := make(chan admitted, 1)
	go func() {
		ticket, err := l.Admit(ctx, class, weight)
		outcome <- admitted{ticket, err}
	}()
	waitForQueued(t, l, class, queuedBefore+1)
	return outcome
}

// queueAtOnce starts n requests of class at once on ctx, each sending its outcome on outcomes,
// which has room for it, and returns once all n are queued.
func queueAtOnce(t *testing.T, l *Limiter, ctx context.Context, class Class, n int, outcomes chan<- admitted) {
	t.Helper()
	queuedBefore := classStats(l, class).Queued
	for range n {
		go func() {
			ticket, err := l.Admit(ctx, class, 1)
			outcomes <- admitted{ticket, err}
		}()
	}
	waitForQueued(t, l, class, queuedBefore+n)
}

type admitted struct {
	ticket *Ticket
	err    error
}

func receive(t *testing.T, outcome <-chan admitted) admitted {
	t.Helper()
	select {
	case result := <-outcome:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("a queued request was never answered")
		return admitted{}
	}
}

func assertPending(t *testing.T, outcome <-chan admitted) {
	t.Helper()
	select {
	case result := <-outcome:
		t.Fatalf("a request that should still wait was answered: %+v", result)
	default:
	}
}

func refusalReason(err error) Reason {
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return refusal.Reason
	}
	return ""
}

// Each queued request takes the whole limit, so the next is admitted only once the one before it
// is released: the order they are admitted in is the order the queue serves them.
func TestQueuedRequestsAreAdmittedInArrivalOrder(t *testing.T) {
	l := newLimiter(3, 1, patient) // the queue holds up to 66 requests, ReadBurstFactor times the limit
	held := admit(t, l, Read, 3)
	order := make(chan int, 3)
	for index := range 3 {
		outcome := admitInBackground(t, l, context.Background(), Read, 3)
		go func() {
			result := <-outcome
			if result.err != nil {
				t.Errorf("queued request %d: %v", index, result.err)
				return
			}
			order <- index
			result.ticket.Release()
		}()
	}
	held.Release()
	for want := range 3 {
		select {
		case got := <-order:
			if got != want {
				t.Fatalf("request %d admitted in place %d", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("request %d was never admitted", want)
		}
	}
}

// A heavy request at the head of the queue is not overtaken by a light one behind it that would
// fit: first come, first served.
func TestAHeavyRequestAtTheHeadIsNotOvertaken(t *testing.T) {
	l := newLimiter(2, 1, patient)
	first := admit(t, l, Read, 1)
	heavy := admitInBackground(t, l, context.Background(), Read, 2)
	light := admitInBackground(t, l, context.Background(), Read, 1)
	assertPending(t, light)
	first.Release()
	heavyTicket := receive(t, heavy)
	if heavyTicket.err != nil {
		t.Fatalf("heavy request: %v", heavyTicket.err)
	}
	if stats := classStats(l, Read); stats.InFlight != 2 {
		t.Fatalf("in flight %d with the heavy request admitted, want its 2 units", stats.InFlight)
	}
	assertPending(t, light)
	heavyTicket.ticket.Release()
	if result := receive(t, light); result.err != nil {
		t.Fatalf("light request: %v", result.err)
	}
}

// testClock is the clock the class queues' standing decision reads in a row on their order
// (newWithClock), which a row moves by hand: it stands a queue by moving the clock a whole wait on,
// never by waiting one out, and the timer a queue arms for that moment (Limiter.standIn) fires as
// the clock passes it. A waiter's own wait, which refuses it, still runs on the real clock.
type testClock struct {
	mu     sync.Mutex
	at     time.Time
	timers []*testTimer
}

func newTestClock() *testClock { return &testClock{at: time.Unix(1_800_000_000, 0)} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

// AfterFunc arms a timer that calls f once the clock has moved d on from now (advance).
func (c *testClock) AfterFunc(d time.Duration, f func()) timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	armed := &testTimer{clock: c, at: c.at.Add(d), armed: true, fire: f}
	c.timers = append(c.timers, armed)
	return armed
}

// advance moves the clock on by, and calls every timer due by then in the caller's goroutine
// before it returns, so a row reads what a timer did once advance has.
func (c *testClock) advance(by time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(by)
	var due []func()
	for _, armed := range c.timers {
		if armed.armed && !armed.at.After(c.at) {
			armed.armed = false
			due = append(due, armed.fire)
		}
	}
	c.mu.Unlock()
	for _, fire := range due {
		fire()
	}
}

// testTimer is a testClock's timer, which fires at at while it is armed.
type testTimer struct {
	clock *testClock
	at    time.Time
	armed bool
	fire  func()
}

func (t *testTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	armed := t.armed
	t.armed = false
	return armed
}

func (t *testTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	armed := t.armed
	t.at, t.armed = t.clock.at.Add(d), true
	return armed
}

// orderedQueue is one of admission's queues as the rows on its order drive it: places is how many
// requests it holds at once, take asks for one for a request arriving now and answers once the
// request holds it, with the give-back, or why not, reason is the Reason a refusal of its refuses
// for, queued reads how many wait in it now, and newestFirst how many it has granted newest first,
// nil for the places outside admission, which serve first in, first out at all times and count no
// such grants (fifoGate).
type orderedQueue struct {
	places      int
	take        func(ctx context.Context) (give func(), err error)
	reason      func(err error) Reason
	queued      func() int
	newestFirst func() uint64
}

// queueKind is a queue the rows on their order drive, each built on a Limiter of its own whose
// standing decision reads clock and whose waiters wait wait.
type queueKind struct {
	name  string
	build func(t *testing.T, clock *testClock, wait time.Duration) orderedQueue
}

// classQueues are each class's queue with one unit, which serves adaptive LIFO (gate.next).
var classQueues = []queueKind{{"the read class", classQueue(Read)}, {"the write class", classQueue(Write)}}

// placeQueues are the queues of places (fifoGate): a heavy read's lane with one place, its read
// class idle, so it serves first in, first out however long its own queue stands (laneGate), and
// the places outside admission (OutsideLimit), which serve first in, first out at all times.
var placeQueues = []queueKind{
	{"a lane", func(t *testing.T, clock *testClock, wait time.Duration) orderedQueue {
		l := newWithClock(Limits{Read: 8, Write: 1, HeavyReadLane: 1, ReadWait: wait}, clock)
		return orderedQueue{
			places:      1,
			take:        func(ctx context.Context) (func(), error) { return enterLane(ctx, l, Search) },
			reason:      refusalReason,
			queued:      func() int { return laneStatsOf(t, l, Search).Queued },
			newestFirst: func() uint64 { return laneStatsOf(t, l, Search).NewestFirst },
		}
	}},
	{"the outside places", func(t *testing.T, clock *testClock, wait time.Duration) orderedQueue {
		outside := newWithClock(Limits{OutsideWait: wait}, clock).Outside()
		return orderedQueue{
			places: OutsideLimit,
			take: func(ctx context.Context) (func(), error) {
				return outside.Release, outside.Take(ctx, HeartbeatCheck)
			},
			reason: outsideRefusalReason,
			queued: func() int { return outside.Stats().Queued },
		}
	}},
}

// classQueue builds class's queue for classQueues.
func classQueue(class Class) func(t *testing.T, clock *testClock, wait time.Duration) orderedQueue {
	return func(t *testing.T, clock *testClock, wait time.Duration) orderedQueue {
		l := newWithClock(Limits{Read: 1, Write: 1, ReadWait: wait, WriteWait: wait}, clock)
		return orderedQueue{
			places: 1,
			take: func(ctx context.Context) (func(), error) {
				ticket, err := l.Admit(ctx, class, 1)
				return func() { ticket.Release() }, err
			},
			reason:      refusalReason,
			queued:      func() int { return classStats(l, class).Queued },
			newestFirst: func() uint64 { return classStats(l, class).NewestFirst },
		}
	}
}

// served is what one request a row lined up was answered: its place in the line, nil once it
// holds a place or why not, how long after it asked, and the give-back of the place it holds.
type served struct {
	place  int
	err    error
	waited time.Duration
	give   func()
}

// holdEveryPlace takes every place of q, failing the row where one is not free at once, and
// answers their give-backs.
func holdEveryPlace(t *testing.T, ctx context.Context, q orderedQueue) []func() {
	t.Helper()
	held := make([]func(), q.places)
	for i := range held {
		give, err := q.take(ctx)
		if err != nil {
			t.Fatalf("place %d of %d: %v, want it taken at once", i+1, q.places, err)
		}
		held[i] = give
	}
	return held
}

// lineUp sends n requests into q's queue one after another, each once the one before it has joined
// it (join), and answers the channel their answers arrive on, in the order they are answered.
func lineUp(t *testing.T, ctx context.Context, q orderedQueue, n int) <-chan served {
	t.Helper()
	answers := make(chan served, n)
	for place := range n {
		join(t, ctx, q, answers, place)
	}
	return answers
}

// join sends request place of a line into q's queue and returns once it has joined it, its answer
// to arrive on answers, which holds no answer yet. Every place of q is held, so a request answered
// before it joins fails the row.
func join(t *testing.T, ctx context.Context, q orderedQueue, answers chan served, place int) {
	t.Helper()
	before := q.queued()
	go func() {
		asked := time.Now()
		give, err := q.take(ctx)
		answers <- served{place, err, time.Since(asked), give}
	}()
	testwait.Until(t, "the request lined up joined the queue", func() (int, bool) {
		queued := q.queued()
		return queued, queued == before+1 || len(answers) != 0
	})
	if len(answers) != 0 {
		got := <-answers
		t.Fatalf("request %d of the line was answered %v before it joined the queue, want it queued behind every place held", got.place, got.err)
	}
}

// grantedNext receives the next answer of a line and fails the row unless it is the grant of the
// request at place want.
func grantedNext(t *testing.T, answers <-chan served, want int, why string) served {
	t.Helper()
	got := testwait.Receive(t, "the next request granted a place", answers)
	if got.err != nil || got.place != want {
		t.Fatalf("the place that came free was answered to request %d of the line (%v), want request %d granted: %s", got.place, got.err, want, why)
	}
	return got
}

// A burst that queues and drains within less than a wait keeps its order: four requests - the
// write queue's whole cap at a limit of one (WriteBurstFactor) - line up behind every place held,
// the queue has been non-empty for less than its wait when the places start coming back, and each
// place goes to the request that asked first of those left - on each class's queue, a heavy read's
// lane and the places outside admission alike - and no class's queue or lane grants one newest
// first. A queue that emptied within its last wait is absorbing a burst (gate.next).
func TestABurstThatDrainsWithinAWaitIsServedInArrivalOrder(t *testing.T) {
	t.Parallel()
	const burst = WriteBurstFactor
	for _, kind := range slices.Concat(classQueues, placeQueues) {
		t.Run(kind.name, func(t *testing.T) {
			t.Parallel()
			clock := newTestClock()
			q := kind.build(t, clock, patient)
			ctx := testwait.Context(t)
			held := holdEveryPlace(t, ctx, q)
			answers := lineUp(t, ctx, q, burst)
			clock.advance(patient - time.Nanosecond)
			held[0]()
			for want := range burst {
				got := grantedNext(t, answers, want, "a queue non-empty for less than its wait serves first come, first served")
				got.give()
			}
			if queued := q.queued(); queued != 0 {
				t.Fatalf("%d queued once the burst drained, want none", queued)
			}
			if q.newestFirst != nil {
				if newestFirst := q.newestFirst(); newestFirst != 0 {
					t.Fatalf("%d granted newest first once the burst drained, want none", newestFirst)
				}
			}
			for _, give := range held[1:] {
				give()
			}
		})
	}
}

// A class's queue that has stood non-empty for a whole wait serves its newest waiter first and
// refuses its oldest at the oldest's wait: three requests line up behind every unit held and the
// queue stands a wait (by the clock its decision reads), so the first unit back goes to the third,
// which gives it back, and the next to the second, which keeps it; the first is refused
// wait_exceeded once its own wait has run, not before, and the queue counts its two grants newest
// first - on the read class's queue and the write class's alike. First come, first served there
// gave every request it served the whole wait first (ReadBurstFactor).
func TestAStandingQueueServesTheNewestFirstAndRefusesTheOldestAtItsWait(t *testing.T) {
	t.Parallel()
	const wait = time.Second
	for _, kind := range classQueues {
		t.Run(kind.name, func(t *testing.T) {
			t.Parallel()
			clock := newTestClock()
			q := kind.build(t, clock, wait)
			ctx := testwait.Context(t)
			held := holdEveryPlace(t, ctx, q)
			answers := lineUp(t, ctx, q, 3)
			clock.advance(wait)
			held[0]()
			const why = "a queue that has stood non-empty for a whole wait serves the newest first"
			grantedNext(t, answers, 2, why).give()
			second := grantedNext(t, answers, 1, why)
			oldest := testwait.Receive(t, "the oldest request's answer", answers)
			if oldest.place != 0 || q.reason(oldest.err) != WaitExceeded {
				t.Fatalf("request %d of the line was answered %v, want the oldest, request 0, refused wait_exceeded at its wait", oldest.place, oldest.err)
			}
			if oldest.waited < wait {
				t.Fatalf("the oldest request was refused %v after it asked, before its %v wait had run", oldest.waited, wait)
			}
			second.give()
			if queued, newestFirst := q.queued(), q.newestFirst(); queued != 0 || newestFirst != 2 {
				t.Fatalf("%d queued and %d granted newest first, want none queued and the two grants counted", queued, newestFirst)
			}
			for _, give := range held[1:] {
				give()
			}
		})
	}
}

// A class's queue that empties serves first come, first served again: a line of three is served
// newest first once the queue has stood a wait, and the queue empties with the last of them
// granted; a second line of three then joins it, the clock where the first line had stood, and is
// served in arrival order, since the queue went from empty to non-empty less than a wait ago - on
// the read class's queue and the write class's alike.
func TestAQueueThatEmptiesServesFirstComeFirstServedAgain(t *testing.T) {
	t.Parallel()
	for _, kind := range classQueues {
		t.Run(kind.name, func(t *testing.T) {
			t.Parallel()
			clock := newTestClock()
			q := kind.build(t, clock, patient)
			ctx := testwait.Context(t)
			held := holdEveryPlace(t, ctx, q)
			first := lineUp(t, ctx, q, 3)
			clock.advance(patient)
			held[0]()
			grantedNext(t, first, 2, "the queue stood a whole wait").give()
			grantedNext(t, first, 1, "the queue stood a whole wait").give()
			last := grantedNext(t, first, 0, "the queue stood a whole wait")
			if queued := q.queued(); queued != 0 {
				t.Fatalf("%d queued once the first line was granted, want the queue empty", queued)
			}
			second := lineUp(t, ctx, q, 3)
			last.give()
			for want := range 3 {
				grantedNext(t, second, want, "a queue that emptied serves first come, first served until it stands again").give()
			}
			if newestFirst := q.newestFirst(); newestFirst != 3 {
				t.Fatalf("%d granted newest first, want the first line's 3 alone", newestFirst)
			}
			for _, give := range held[1:] {
				give()
			}
		})
	}
}

// A queue of places serves first in, first out however long it has stood, a heavy read's lane
// while its read class has room and the places outside admission at all times (fifoGate,
// laneGate): three requests line up behind every place held and the queue stands a wait by the
// clock a class's queue decides by; then three times a newer request joins and a place comes back,
// and each place goes to the request that has waited longest, the three first and the newcomers
// after them, every one of them granted and none refused or granted newest first. Newest first
// there, decided by the lane's own queue standing, handed each place to the newcomer and left the
// oldest to be refused at their wait while the places turned over: on the cluster copy at ten
// times the load every refusal of the run was the search lane's at its wait, with 4 or 5 queued
// and the read class's room unused (INFRA-592, c-scale at 72e98685).
func TestAQueueOfPlacesServesTheOldestFirstHoweverLongItStands(t *testing.T) {
	t.Parallel()
	const lined, newcomers = 3, 3
	const why = "a queue of places serves first in, first out however long it has stood"
	for _, kind := range placeQueues {
		t.Run(kind.name, func(t *testing.T) {
			t.Parallel()
			clock := newTestClock()
			q := kind.build(t, clock, patient)
			ctx := testwait.Context(t)
			held := holdEveryPlace(t, ctx, q)
			answers := make(chan served, lined+newcomers)
			for place := range lined {
				join(t, ctx, q, answers, place)
			}
			clock.advance(patient)
			give := held[0]
			for want := range lined {
				join(t, ctx, q, answers, lined+want)
				give()
				give = grantedNext(t, answers, want, why).give
			}
			for want := lined; want < lined+newcomers; want++ {
				give()
				give = grantedNext(t, answers, want, why).give
			}
			give()
			if queued := q.queued(); queued != 0 {
				t.Fatalf("%d queued once every request was granted, want none", queued)
			}
			if q.newestFirst != nil {
				if newestFirst := q.newestFirst(); newestFirst != 0 {
					t.Fatalf("%d granted newest first, want none: %s", newestFirst, why)
				}
			}
			for _, give := range held[1:] {
				give()
			}
		})
	}
}

// In a queue that stands, a take-back that must not be refused (Away.TakeBackNeverRefused) is
// served ahead of every waiter that may be refused, the oldest of such take-backs first: it has no
// wait to run out, so newest first would pass it over for as long as the overload lasted, after its
// request had changed another server's state. Two requests give their units back for a wait on the
// network and take them back never refused, the later arrival asking first; two requests that may
// be refused queue after them; and once the queue has stood a wait, the units that come back go to
// the earlier take-back, then the later, and only then newest first to the other two.
func TestATakeBackThatMustNotBeRefusedIsServedFirstInAStandingQueue(t *testing.T) {
	t.Parallel()
	for _, class := range []Class{Read, Write} {
		t.Run(class.String(), func(t *testing.T) {
			t.Parallel()
			clock := newTestClock()
			l := newWithClock(Limits{Read: 1, Write: 1, ReadWait: patient, WriteWait: patient}, clock)
			ctx := testwait.Context(t)
			names := [...]string{"the take-back that arrived first", "the take-back that arrived second", "the older request", "the newer request"}
			type granted struct {
				who    string
				ticket *Ticket
				err    error
			}
			grants := make(chan granted, len(names))
			var kept [2]*Ticket
			var aways [2]Away
			for i := range kept {
				kept[i] = admit(t, l, class, 1)
				aways[i] = GiveBack(WithTicket(ctx, kept[i]))
			}
			holder := admit(t, l, class, 1)
			for _, i := range []int{1, 0} {
				queuedBefore := classStats(l, class).Queued
				go func() { grants <- granted{names[i], kept[i], aways[i].TakeBackNeverRefused(ctx)} }()
				waitForQueued(t, l, class, queuedBefore+1)
			}
			for _, who := range names[2:] {
				queuedBefore := classStats(l, class).Queued
				go func() {
					ticket, err := l.Admit(ctx, class, 1)
					grants <- granted{who, ticket, err}
				}()
				waitForQueued(t, l, class, queuedBefore+1)
			}
			clock.advance(patient)
			holder.Release()
			for _, want := range []string{names[0], names[1], names[3], names[2]} {
				got := testwait.Receive(t, "the next unit's holder", grants)
				if got.err != nil || got.who != want {
					t.Fatalf("the unit that came back went to %s (%v), want %s: a queue that stands serves a take-back that must not be refused ahead of every other waiter, the oldest first", got.who, got.err, want)
				}
				got.ticket.Release()
			}
		})
	}
}

// A queue that stands keeps the two classes' guarantees with its candidate in place of its head
// (grant, heldByTotal): a weight-2 write the standing write queue serves next, which the combined
// cap alone holds up, is not overtaken by a read the cap has room for. With the cap of 4 full of
// reads, a weight-1 write queues and the write queue stands a wait; a weight-2 write then joins it,
// its newest waiter and so its candidate, and a read queues. The first read unit back is held for
// the weight-2 write - neither the read nor the older weight-1 write, which would fit, takes it -
// and the second unit back grants it, newest first.
func TestAStandingQueuesWeightTwoCandidateIsNotStarvedByTheOtherClass(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	l := newWithClock(Limits{Read: 4, Write: 4, Total: 4, ReadWait: patient, WriteWait: patient}, clock)
	ctx := testwait.Context(t)
	reads := make([]*Ticket, 4)
	for i := range reads {
		reads[i] = admit(t, l, Read, 1)
	}
	older := admitInBackground(t, l, ctx, Write, 1)
	clock.advance(patient)
	heavy := admitInBackground(t, l, ctx, Write, 2)
	read := admitInBackground(t, l, ctx, Read, 1)

	reads[0].Release()
	if got := (Totals{Limit: 4, InFlight: 3}); l.Totals() != got {
		t.Fatalf("totals %+v once one read unit came back, want %+v: the unit is the weight-2 write's, which the standing write queue serves next", l.Totals(), got)
	}
	if reads, writes := classStats(l, Read), classStats(l, Write); reads.Queued != 1 || writes.Queued != 2 || writes.NewestFirst != 0 {
		t.Fatalf("read stats %+v and write stats %+v, want the read and both writes still queued", reads, writes)
	}
	reads[1].Release()
	granted := receive(t, heavy)
	if granted.err != nil {
		t.Fatalf("the weight-2 write once the cap had room for its 2 units: %v, want it granted", granted.err)
	}
	if writes := classStats(l, Write); writes.InFlight != 2 || writes.Queued != 1 || writes.NewestFirst != 1 {
		t.Fatalf("write stats %+v, want the weight-2 write's 2 units in flight, granted newest first, and the older write queued", writes)
	}

	granted.ticket.Release()
	reads[2].Release()
	reads[3].Release()
	for _, outcome := range []<-chan admitted{older, read} {
		result := receive(t, outcome)
		if result.err != nil {
			t.Fatalf("a request still queued once every unit came back: %v", result.err)
		}
		result.ticket.Release()
	}
	if got := (Totals{Limit: 4}); l.Totals() != got {
		t.Fatalf("totals %+v after every release, want %+v", l.Totals(), got)
	}
}

// A weight-2 head refused at its wait leaves the light waiter behind it the room beside it: with the
// write limit of 2 and one unit held, a weight-2 write that spent most of its wait at a document's
// writer gate (Ticket.Waited) takes its units back first, a weight-1 write queues behind it, and
// the queue stands a wait by the clock its decision reads, so it serves its newest waiter, the
// light write, first. The heavy write is refused at what was left of its wait, and the light write
// is admitted beside the unit held, long before its own wait ends: the moment the queue stands
// grants it (Limiter.standIn), and so does the heavy write's leaving, whichever waiter the queue
// serves next by then (Limiter.acquire). Before either, a waiter's leaving granted only where it was
// the waiter served next, which the head of a queue that stands is not, and the light write sat
// beside a free unit until a release, an arrival or its own refusal.
func TestAWaiterThatLeavesAQueueHandsTheRoomBesideItOn(t *testing.T) {
	t.Parallel()
	const short = 100 * time.Millisecond
	clock := newTestClock()
	l := newWithClock(Limits{Read: 1, Write: 2, ReadWait: patient, WriteWait: patient}, clock)
	ctx := testwait.Context(t)
	heavy := admit(t, l, Write, 2)
	heavy.Park()
	heavy.Waited(patient - short)
	holder := admit(t, l, Write, 1)
	defer holder.Release()
	resumed := make(chan error, 1)
	go func() { resumed <- heavy.Resume(ctx) }()
	waitForQueued(t, l, Write, 1)
	light := admitInBackground(t, l, ctx, Write, 1)
	clock.advance(patient)

	if err := testwait.Receive(t, "the heavy write's take-back", resumed); refusalReason(err) != WaitExceeded {
		t.Fatalf("the heavy write's take-back with the cap a unit short of it: %v, want it refused wait_exceeded at what was left of its wait", err)
	}
	got := testwait.Receive(t, "the light write's answer", light)
	if got.err != nil {
		t.Fatalf("the light write once the heavy write ahead of it left: %v, want it admitted to the unit beside the one held", got.err)
	}
	if writes := classStats(l, Write); writes.InFlight != 2 || writes.Queued != 0 {
		t.Fatalf("write stats %+v, want the holder's unit and the light write's in flight and nothing queued", writes)
	}
	got.ticket.Release()
}

// The moment a queue stands is itself a grant: with the write limit of 2 and one unit held, a
// weight-2 write queues first and waits on, and a weight-1 write that spent most of its wait at a
// document's writer gate (Ticket.Waited) takes its unit back behind it, which the weight-2 write
// keeps it from while the queue serves first come, first served. Once the queue has stood a wait
// by the clock its decision reads it serves its newest waiter first, and the light write, which
// fits beside the unit held, is admitted then and there, with the weight-2 write still queued and
// no release, arrival or leaving waiter to prompt it. Nothing granted at that moment before: the
// light write sat beside a free unit until the weight-2 write left or its own short wait ran out,
// and was refused at it while the room was there.
func TestAQueueThatStandsGrantsItsNewestWaiterThere(t *testing.T) {
	t.Parallel()
	const short = time.Second
	clock := newTestClock()
	l := newWithClock(Limits{Read: 1, Write: 2, ReadWait: patient, WriteWait: patient}, clock)
	ctx := testwait.Context(t)
	heavy := l.Arrive(Write, 2)
	light := admit(t, l, Write, 1)
	light.Park()
	light.Waited(patient - short)
	holder := admit(t, l, Write, 1)
	heavyAnswer := make(chan admitted, 1)
	go func() {
		ticket, err := heavy.Admit(ctx)
		heavyAnswer <- admitted{ticket, err}
	}()
	waitForQueued(t, l, Write, 1)
	resumed := make(chan error, 1)
	go func() { resumed <- light.Resume(ctx) }()
	waitForQueued(t, l, Write, 2)
	clock.advance(patient)

	if err := testwait.Receive(t, "the light write's take-back", resumed); err != nil {
		t.Fatalf("the light write's take-back once the queue stood: %v, want it granted the unit beside the one held, newest first", err)
	}
	assertPending(t, heavyAnswer)
	if writes := classStats(l, Write); writes.InFlight != 2 || writes.Queued != 1 || writes.NewestFirst != 1 {
		t.Fatalf("write stats %+v, want the holder's unit and the light write's in flight, granted newest first, and the weight-2 write queued", writes)
	}
	light.Release()
	holder.Release()
	granted := testwait.Receive(t, "the weight-2 write's answer", heavyAnswer)
	if granted.err != nil {
		t.Fatalf("the weight-2 write once both units came back: %v, want it admitted", granted.err)
	}
	granted.ticket.Release()
}

func TestAWeightAboveTheLimitRunsAlone(t *testing.T) {
	l := newLimiter(1, 1, patient)
	ticket := admit(t, l, Write, 2)
	if stats := classStats(l, Write); stats.InFlight != 1 {
		t.Fatalf("in flight %d, want the whole limit of 1", stats.InFlight)
	}
	ticket.Release()
	if stats := classStats(l, Write); stats.InFlight != 0 {
		t.Fatalf("in flight %d after release, want 0", stats.InFlight)
	}
}

// Reads and writes are limited apart: a full write class leaves reads their room.
func TestEachClassHasItsOwnLimit(t *testing.T) {
	l := newLimiter(1, 1, 10*time.Millisecond)
	write := admit(t, l, Write, 1)
	defer write.Release()
	admit(t, l, Read, 1).Release()
	if _, err := l.Admit(context.Background(), Write, 1); refusalReason(err) != WaitExceeded {
		t.Fatalf("a second write: %v, want a wait_exceeded refusal", err)
	}
}

// A burst the server can serve within its wait is served, however far past the limit it queues:
// with the class full at the server's width and its queue empty a moment before, ten times the
// read limit arrive at once - 280 reads - and the burst cap's worth of writes, 96; the holders
// give their units back within the wait, and every request is admitted, none refused queue_full.
// Held to twice the limit, the read queue refused such a burst on the cluster copy at 10x
// (INFRA-592, test handoff v39) that the server then served.
func TestABurstIntoAnEmptyQueueIsServed(t *testing.T) {
	const wait = 5 * time.Second
	for _, test := range []struct {
		class Class
		burst int
	}{
		{Read, 10 * MaxReadLimit},
		{Write, WriteBurstFactor * MaxWriteLimit},
	} {
		t.Run(test.class.String(), func(t *testing.T) {
			l := newLimiter(MaxReadLimit, MaxWriteLimit, wait)
			held := make([]*Ticket, classStats(l, test.class).Limit)
			for i := range held {
				held[i] = admit(t, l, test.class, 1)
			}
			outcomes := make(chan error, test.burst)
			for range test.burst {
				go func() {
					ticket, err := l.Admit(context.Background(), test.class, 1)
					if err == nil {
						ticket.Release()
					}
					outcomes <- err
				}()
			}
			// Each request of the burst has arrived once it is queued or refused.
			deadline := time.Now().Add(5 * time.Second)
			for stats := classStats(l, test.class); stats.Queued+int(stats.QueueFull) != test.burst; stats = classStats(l, test.class) {
				if time.Now().After(deadline) {
					t.Fatalf("%s stats %+v, want the whole burst of %d queued or refused", test.class, stats, test.burst)
				}
				time.Sleep(time.Millisecond)
			}
			if stats := classStats(l, test.class); stats.QueueFull != 0 {
				t.Fatalf("%d of a burst of %d %ss were refused queue_full into a queue empty a moment before (%+v), want none: the queue takes a burst up to its cap of %d",
					stats.QueueFull, test.burst, test.class, stats, l.classes[test.class].burstCap)
			}
			for _, ticket := range held {
				ticket.Release()
			}
			for range test.burst {
				select {
				case err := <-outcomes:
					if err != nil {
						t.Fatalf("a %s of the burst: %v, want it admitted within its %v wait", test.class, err, wait)
					}
				case <-time.After(wait):
					t.Fatalf("a %s of the burst was not answered within its %v wait", test.class, wait)
				}
			}
			if stats := classStats(l, test.class); stats.InFlight != 0 || stats.Queued != 0 || stats.QueueFull+stats.WaitExceeded != 0 {
				t.Fatalf("%s stats %+v once the burst was served, want nothing held, queued or refused", test.class, stats)
			}
		})
	}
}

// The burst cap is a bound on memory, and holds: with each class full at the server's width, its
// queue takes the class's burst factor times its limit - 616 reads, 96 writes - and the next
// request is refused queue_full at once, counted against its own class alone.
func TestABurstPastItsCapIsRefusedAtOnce(t *testing.T) {
	for _, test := range []struct {
		class    Class
		burstCap int
	}{
		{Read, ReadBurstFactor * MaxReadLimit},
		{Write, WriteBurstFactor * MaxWriteLimit},
	} {
		t.Run(test.class.String(), func(t *testing.T) {
			l := newLimiter(MaxReadLimit, MaxWriteLimit, patient)
			held := make([]*Ticket, classStats(l, test.class).Limit)
			for i := range held {
				held[i] = admit(t, l, test.class, 1)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			queueAtOnce(t, l, ctx, test.class, test.burstCap, make(chan admitted, test.burstCap))
			started := time.Now()
			_, err := l.Admit(context.Background(), test.class, 1)
			if refusalReason(err) != QueueFull {
				t.Fatalf("a %s past a burst of %d: %v, want a queue_full refusal", test.class, test.burstCap, err)
			}
			if waited := time.Since(started); waited > 100*time.Millisecond {
				t.Fatalf("a queue at its burst cap refused after %v, want at once", waited)
			}
			if stats := classStats(l, test.class); stats.QueueFull != 1 || stats.Queued != test.burstCap {
				t.Fatalf("%s stats %+v, want one queue_full refusal and the %d of the burst still queued", test.class, stats, test.burstCap)
			}
			other := Read
			if test.class == Read {
				other = Write
			}
			if stats := classStats(l, other); stats.QueueFull != 0 {
				t.Fatalf("%s stats %+v, want the refusal counted against %ss alone", other, stats, test.class)
			}
			cancel()
			waitForQueued(t, l, test.class, 0)
			for _, ticket := range held {
				ticket.Release()
			}
		})
	}
}

// standAWait gives class in l a queue that has stood non-empty for one whole wait of the class,
// with every unit of the class held, and answers the tickets that hold them and the answer of the
// keeper that stands in the queue. The keeper is a ticket given back for a wait on the network
// that takes its unit back never refused (Away.TakeBackNeverRefused), so it waits at the head of
// the queue for as long as ctx lives and the queue never empties once it has joined. A request
// that joins behind it is refused at its wait, and that refusal is the event that says the queue
// has stood a whole wait: the row reads no clock of its own for it.
func standAWait(t *testing.T, l *Limiter, ctx context.Context, class Class) (held []*Ticket, keeper <-chan error) {
	t.Helper()
	parked := admit(t, l, class, 1)
	away := GiveBack(WithTicket(ctx, parked))
	held = make([]*Ticket, classStats(l, class).Limit)
	for i := range held {
		held[i] = admit(t, l, class, 1)
	}
	kept := make(chan error, 1)
	go func() {
		err := away.TakeBackNeverRefused(ctx)
		parked.Release()
		kept <- err
	}()
	testwait.Until(t, "the keeper queued at the head of the queue", func() (ClassStats, bool) {
		stats := classStats(l, class)
		return stats, stats.Queued == 1
	})
	first := make(chan error, 1)
	go func() {
		_, err := l.Admit(ctx, class, 1)
		first <- err
	}()
	if err := testwait.Receive(t, "the refusal of the request queued behind the keeper", first); refusalReason(err) != WaitExceeded {
		t.Fatalf("a %s queued behind the keeper with every unit held: %v, want it refused wait_exceeded at its wait", class, err)
	}
	if stats := classStats(l, class); stats.Queued != 1 || stats.WaitExceeded != 1 || stats.QueueFull != 0 {
		t.Fatalf("%s stats %+v once the queue stood a whole wait, want the keeper alone queued and the one refusal at the wait", class, stats)
	}
	return held, kept
}

// A class full for less than its wait answers every waiter, however long its queue has stood and
// however many wait in it below its cap: with every unit of the class held at the server's width,
// 28 reads or 24 writes, and its queue non-empty for one whole wait of the class's own
// (DefaultReadWait, DefaultWriteWait; standAWait), twice the limit and eight more arrive at once -
// 64 reads or 56 writes, past the 56 and 48 at which a queue that had stood a wait was refused at
// once - and the units come back within their wait: every one of them is admitted, and none is
// refused. That bound refused such reads while the queue drained within its wait: 121 of the 150
// refusals of c-scale's step head-j-10x-5 at eb23d38e (ReadBurstFactor).
func TestAClassFullForLessThanItsWaitAnswersEveryWaiter(t *testing.T) {
	t.Parallel()
	for _, class := range []Class{Read, Write} {
		t.Run(class.String(), func(t *testing.T) {
			t.Parallel()
			l := newLimiter(MaxReadLimit, MaxWriteLimit, 0) // each class its own default wait
			ctx := testwait.Context(t)
			held, keeper := standAWait(t, l, ctx, class)
			arrivals := 2*classStats(l, class).Limit + 8
			answers := make(chan error, arrivals)
			for range arrivals {
				go func() {
					ticket, err := l.Admit(ctx, class, 1)
					ticket.Release()
					answers <- err
				}()
			}
			// The keeper, and the one refusal standAWait waited for, are not the arrivals'.
			stats := testwait.Until(t, "every arrival queued or refused", func() (ClassStats, bool) {
				stats := classStats(l, class)
				return stats, stats.Queued-1+int(stats.QueueFull)+int(stats.WaitExceeded-1) == arrivals
			})
			if stats.QueueFull != 0 {
				t.Fatalf("%d of %d %ss arriving at a queue that had stood a whole wait were refused queue_full, below its cap of %d (%+v), want none: their wait had room",
					stats.QueueFull, arrivals, class, l.classes[class].burstCap, stats)
			}
			for _, ticket := range held {
				ticket.Release()
			}
			if err := testwait.Receive(t, "the keeper's take-back", keeper); err != nil {
				t.Fatalf("the keeper's take-back once the units came back: %v, want it granted", err)
			}
			for range arrivals {
				if err := testwait.Receive(t, "an arrival's answer", answers); err != nil {
					t.Fatalf("a %s queued behind a queue that had stood a whole wait, whose unit came back within its wait: %v, want it admitted", class, err)
				}
			}
			if stats := classStats(l, class); stats.InFlight != 0 || stats.Queued != 0 || stats.QueueFull != 0 || stats.WaitExceeded != 1 {
				t.Fatalf("%s stats %+v once every arrival was answered, want nothing held or queued and only standAWait's refusal", class, stats)
			}
		})
	}
}

// A class full past its wait refuses a waiter at the wait and not before: with the class's one
// unit held and never given back, a request that finds no room waits in the queue and is refused
// wait_exceeded once its whole wait (DefaultReadWait, DefaultWriteWait) has run, not at once, and
// the class records it as having waited that wait.
func TestAClassFullPastItsWaitRefusesAtTheWait(t *testing.T) {
	t.Parallel()
	for _, class := range []Class{Read, Write} {
		t.Run(class.String(), func(t *testing.T) {
			t.Parallel()
			l := newLimiter(1, 1, 0) // each class its own default wait
			wait := l.classes[class].wait
			held := admit(t, l, class, 1)
			defer held.Release()
			answered := make(chan error, 1)
			asked := time.Now()
			go func() {
				_, err := l.Admit(context.Background(), class, 1)
				answered <- err
			}()
			err := testwait.Receive(t, "the answer of a request its class never has room for", answered)
			waited := time.Since(asked)
			if refusalReason(err) != WaitExceeded {
				t.Fatalf("a %s its class never had room for: %v, want a wait_exceeded refusal", class, err)
			}
			if waited < wait {
				t.Fatalf("the %s was refused %v after it asked, before its %v wait had run", class, waited, wait)
			}
			if stats := classStats(l, class); stats.WaitExceeded != 1 || stats.QueueFull != 0 || stats.Queued != 0 || stats.LongestRefusedWait < wait {
				t.Fatalf("%s stats %+v, want one refusal at the wait, recorded as having waited its %v, and nobody queued", class, stats, wait)
			}
		})
	}
}

// overloadCounts is what a gate queues now and has refused, for sustainOverload.
type overloadCounts struct {
	queued                  int
	queueFull, waitExceeded uint64
}

// sustainOverload sends waves of arrivals at a gate that never has room, through ask, which sends
// one request and answers how it was refused, and reads the gate's counts through counts. Each wave
// is the arrivals that fill the queue to its cap and one more, sent at once, and each is sent once
// the wave before has been answered whole. standing is the places of the queue a waiter that is
// no wave's holds throughout. The queue never holds past its cap, at most the arrivals it has no
// place for are refused queue_full at once, and every arrival it queued is refused wait_exceeded
// once its whole wait has run, never before.
func sustainOverload(t *testing.T, wait time.Duration, capacity, standing int, ask func() error, counts func() overloadCounts) {
	t.Helper()
	type answer struct {
		err    error
		waited time.Duration
	}
	arrivals := capacity - standing + 1
	for wave := range 3 {
		before := counts()
		answers := make(chan answer, arrivals)
		for range arrivals {
			go func() {
				asked := time.Now()
				err := ask()
				answers <- answer{err, time.Since(asked)}
			}()
		}
		arrived := testwait.Until(t, "every arrival of the wave queued or refused", func() (overloadCounts, bool) {
			now := counts()
			return now, now.queued-standing+int(now.queueFull-before.queueFull)+int(now.waitExceeded-before.waitExceeded) == arrivals
		})
		if arrived.queued > capacity {
			t.Fatalf("wave %d: the queue holds %d, past its cap of %d", wave, arrived.queued, capacity)
		}
		refusedAtOnce := 0
		for range arrivals {
			got := testwait.Receive(t, "an arrival's answer", answers)
			switch refusalReason(got.err) {
			case QueueFull:
				refusedAtOnce++
			case WaitExceeded:
				if got.waited < wait {
					t.Fatalf("wave %d: an arrival the queue took was refused %v after it asked, before its %v wait had run", wave, got.waited, wait)
				}
			default:
				t.Fatalf("wave %d: an arrival no room ever came for was answered %v, want it refused at its wait or at the queue's cap", wave, got.err)
			}
		}
		if past := arrivals - (capacity - standing); refusedAtOnce > past {
			t.Fatalf("wave %d: %d of %d arrivals were refused queue_full at once, want at most the %d the queue's cap of %d had no place for: their wait had room",
				wave, refusedAtOnce, arrivals, past, capacity)
		}
	}
}

// Under sustained overload a queue never holds past its cap, and every refusal below the cap comes
// at the wait, never before it. The read class's one unit is held for the whole row and never comes
// back, and a keeper holds its queue non-empty throughout (standAWait), so the queue stands for
// more than three of its waits; a heavy read's lane of one place is held the same way. Three waves
// of arrivals, each the cap's worth - ReadBurstFactor times the limit, the bound on the queue's
// memory - and one more, come one after another (sustainOverload): each wave is queued up to the
// cap and the arrival past it is refused queue_full at once, and every arrival the queue took is
// refused wait_exceeded at its wait.
func TestUnderSustainedOverloadAQueueHoldsItsCapAndRefusesOnlyAtTheWait(t *testing.T) {
	t.Parallel()
	const wait = 300 * time.Millisecond
	t.Run("the read class", func(t *testing.T) {
		t.Parallel()
		l := newLimiter(1, 1, wait)
		ctx := testwait.Context(t)
		held, keeper := standAWait(t, l, ctx, Read)
		sustainOverload(t, wait, l.classes[Read].burstCap, 1, func() error {
			ticket, err := l.Admit(ctx, Read, 1)
			ticket.Release()
			return err
		}, func() overloadCounts {
			stats := classStats(l, Read)
			return overloadCounts{stats.Queued, stats.QueueFull, stats.WaitExceeded}
		})
		for _, ticket := range held {
			ticket.Release()
		}
		if err := testwait.Receive(t, "the keeper's take-back", keeper); err != nil {
			t.Fatalf("the keeper's take-back once the unit came back: %v, want it granted", err)
		}
	})
	t.Run("a lane", func(t *testing.T) {
		t.Parallel()
		l := New(Limits{Read: 1, Write: 1, HeavyReadLane: 1, ReadWait: wait})
		ctx := testwait.Context(t)
		holder, err := enterLane(ctx, l, Search)
		if err != nil {
			t.Fatalf("take the search lane's only place: %v", err)
		}
		defer holder()
		sustainOverload(t, wait, ReadBurstFactor, 0, func() error {
			leave, err := enterLane(ctx, l, Search)
			if err == nil {
				leave()
			}
			return err
		}, func() overloadCounts {
			stats := laneStatsOf(t, l, Search)
			return overloadCounts{stats.Queued, stats.QueueFull, stats.WaitExceeded}
		})
	})
}

// A request queued behind others still waits no longer than its class's wait: where its room
// never comes it is refused wait_exceeded at that wait, as the requests queued ahead of it are,
// and answered 429 OVERLOADED with Retry-After: 1, as every refusal is.
func TestARequestQueuedBehindOthersWaitsNoLongerThanItsClasssWait(t *testing.T) {
	const wait, ahead = 300 * time.Millisecond, 2
	l := newLimiter(1, 1, wait)
	held := admit(t, l, Read, 1)
	defer held.Release()
	outcomes := make(chan admitted, ahead)
	queueAtOnce(t, l, context.Background(), Read, ahead, outcomes)
	started := time.Now()
	_, err := l.Admit(context.Background(), Read, 1)
	waited := time.Since(started)
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Reason != WaitExceeded {
		t.Fatalf("a read queued behind %d others with no room coming: %v, want a wait_exceeded refusal", ahead, err)
	}
	if waited < wait || waited >= wait+250*time.Millisecond {
		t.Fatalf("refused after %v, want after its %v wait and within 250ms of it", waited, wait)
	}
	w := httptest.NewRecorder()
	WriteRefused(w, refusal)
	assertRetryable(t, w, http.StatusTooManyRequests, CodeOverloaded)
	for range ahead {
		if result := receive(t, outcomes); refusalReason(result.err) != WaitExceeded {
			t.Fatalf("a read queued ahead of it: %+v, want a wait_exceeded refusal", result)
		}
	}
	if stats := classStats(l, Read); stats.QueueFull != 0 || stats.WaitExceeded != ahead+1 || stats.Queued != 0 {
		t.Fatalf("read stats %+v, want all %d refused at their wait and none for a full queue", stats, ahead+1)
	}
}

// A read waits DefaultReadWait, 1 s, for room and is then refused, so its refusal is answered once
// its wait has run out and not much later.
func TestAWaitPastItsBoundIsRefused(t *testing.T) {
	l := newLimiter(1, 1, 0) // a zero wait leaves each class its default
	held := admit(t, l, Read, 1)
	started := time.Now()
	_, err := l.Admit(context.Background(), Read, 1)
	waited := time.Since(started)
	if refusalReason(err) != WaitExceeded {
		t.Fatalf("admit behind a held request: %v, want a wait_exceeded refusal", err)
	}
	if waited < DefaultReadWait || waited >= DefaultReadWait+250*time.Millisecond {
		t.Fatalf("refused after %v, want after its wait of %v and within 250ms of it", waited, DefaultReadWait)
	}
	stats := classStats(l, Read)
	if stats.WaitExceeded != 1 || stats.Queued != 0 || stats.InFlight != 1 {
		t.Fatalf("read stats %+v, want one wait_exceeded refusal, nothing queued, the held request in flight", stats)
	}
	held.Release()
	admit(t, l, Read, 1).Release()
}

// A write waits for room past the wait a read is refused at: room that comes after a read's wait
// but inside a write's is the write's, where one wait for both classes refused that write. The
// waits here are short ones of the same shape as the shipped DefaultReadWait and
// DefaultWriteWait, and the room comes between the two, as the burst at the window start of
// INFRA-589's 1x reproduction took 2-6 s to free a write unit against a 1 s wait
// (DefaultWriteWait).
func TestAWriteWaitsForRoomPastAReadsWait(t *testing.T) {
	const readWait, writeWait = 100 * time.Millisecond, time.Second
	const roomComes = 300 * time.Millisecond
	limits := Limits{Read: 1, Write: 1, ReadWait: readWait, WriteWait: writeWait}

	t.Run("a write takes room that comes past a read's wait", func(t *testing.T) {
		l := New(limits)
		held := admit(t, l, Write, 1)
		started := time.Now()
		outcome := admitInBackground(t, l, context.Background(), Write, 1)

		time.Sleep(roomComes) // past a read's wait, inside a write's
		if stats := classStats(l, Write); stats.Queued != 1 || stats.WaitExceeded != 0 {
			t.Fatalf("write stats %+v after %v, want the write still queued: its wait is %v, not a read's %v",
				stats, roomComes, writeWait, readWait)
		}
		held.Release()
		result := receive(t, outcome)
		if result.err != nil {
			t.Fatalf("a write whose room came %v in, inside its %v wait: %v, want it admitted", roomComes, writeWait, result.err)
		}
		result.ticket.Release()
		if waited := time.Since(started); waited < roomComes {
			t.Fatalf("the write was admitted after %v, want it to have waited for the room that came %v in", waited, roomComes)
		}
	})

	t.Run("a read with the same room held is refused at its own wait", func(t *testing.T) {
		l := New(limits)
		held := admit(t, l, Read, 1)
		defer held.Release()
		started := time.Now()
		_, err := l.Admit(context.Background(), Read, 1)
		waited := time.Since(started)
		if refusalReason(err) != WaitExceeded {
			t.Fatalf("a read behind a held one: %v, want a wait_exceeded refusal", err)
		}
		if waited < readWait || waited >= roomComes {
			t.Fatalf("the read was refused after %v, want it refused at its own %v wait, before the %v at which the write above found room",
				waited, readWait, roomComes)
		}
		if stats := classStats(l, Read); stats.WaitExceeded != 1 {
			t.Fatalf("read stats %+v, want the one wait_exceeded refusal", stats)
		}
	})

	t.Run("a write is refused at its own wait when no room comes", func(t *testing.T) {
		l := New(limits)
		held := admit(t, l, Write, 1)
		defer held.Release()
		started := time.Now()
		_, err := l.Admit(context.Background(), Write, 1)
		waited := time.Since(started)
		if refusalReason(err) != WaitExceeded {
			t.Fatalf("a write behind a held one: %v, want a wait_exceeded refusal", err)
		}
		if waited < writeWait || waited >= writeWait+250*time.Millisecond {
			t.Fatalf("refused after %v, want after its %v wait and within 250ms of it", waited, writeWait)
		}
		if stats := classStats(l, Write); stats.WaitExceeded != 1 || stats.LongestRefusedWait < writeWait {
			t.Fatalf("write stats %+v, want one wait_exceeded refusal recorded as having waited its %v", stats, writeWait)
		}
	})
}

// A request whose client goes away while it waits leaves the queue and takes no unit with it:
// the units it would have held go to the request behind it.
func TestACancelledWaitLeaksNoUnits(t *testing.T) {
	l := newLimiter(1, 2, patient) // the queue holds up to eight requests, WriteBurstFactor times the limit
	held := admit(t, l, Write, 2)
	ctx, cancel := context.WithCancel(context.Background())
	gone := admitInBackground(t, l, ctx, Write, 1)
	behind := admitInBackground(t, l, context.Background(), Write, 1)
	cancel()
	if result := receive(t, gone); !errors.Is(result.err, context.Canceled) {
		t.Fatalf("cancelled wait: %+v, want context.Canceled", result)
	}
	waitForQueued(t, l, Write, 1)
	held.Release()
	result := receive(t, behind)
	if result.err != nil {
		t.Fatalf("request behind the cancelled one: %v", result.err)
	}
	result.ticket.Release()
	if stats := classStats(l, Write); stats.InFlight != 0 || stats.Queued != 0 || stats.QueueFull+stats.WaitExceeded != 0 {
		t.Fatalf("write stats %+v, want nothing held, queued or refused", stats)
	}
}

func TestReleaseRunsOnce(t *testing.T) {
	l := newLimiter(2, 1, patient)
	ticket := admit(t, l, Read, 1)
	other := admit(t, l, Read, 1)
	ticket.Release()
	ticket.Release()
	if stats := classStats(l, Read); stats.InFlight != 1 {
		t.Fatalf("in flight %d after releasing one ticket twice, want the other's 1", stats.InFlight)
	}
	other.Release()
	ticket.Park()
	if err := ticket.Resume(context.Background()); err != nil {
		t.Fatalf("resume a released ticket: %v", err)
	}
	if stats := classStats(l, Read); stats.InFlight != 0 {
		t.Fatalf("in flight %d after parking and resuming a released ticket, want 0", stats.InFlight)
	}
	var none *Ticket
	none.Park()
	none.Release()
	if err := none.Resume(context.Background()); err != nil {
		t.Fatalf("resume a nil ticket: %v", err)
	}
}

// A parked ticket gives its units to the next request while its holder waits elsewhere, and
// takes them back through the queue, under the same bounds as Admit.
func TestAParkedTicketGivesItsUnitsBackUntilItResumes(t *testing.T) {
	const wait = 50 * time.Millisecond
	l := newLimiter(1, 1, wait)
	parked := admit(t, l, Write, 1)
	parked.Park()
	parked.Park()
	other := admit(t, l, Write, 1)

	if err := parked.Resume(context.Background()); refusalReason(err) != WaitExceeded {
		t.Fatalf("resume while the other request holds the unit: %v, want a wait_exceeded refusal", err)
	}
	parked.Release() // a refused resume leaves the ticket parked: its release gives nothing back
	if stats := classStats(l, Write); stats.InFlight != 1 {
		t.Fatalf("in flight %d, want the other request's 1", stats.InFlight)
	}
	other.Release()

	again := newLimiter(1, 1, patient)
	ticket := admit(t, again, Write, 1)
	ticket.Park()
	holder := admit(t, again, Write, 1)
	done := make(chan error, 1)
	go func() { done <- ticket.Resume(context.Background()) }()
	waitForQueued(t, again, Write, 1)
	holder.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("resume once the unit came free: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the parked ticket never resumed")
	}
	if stats := classStats(again, Write); stats.InFlight != 1 {
		t.Fatalf("in flight %d after resume, want 1", stats.InFlight)
	}
	ticket.Release()
	if stats := classStats(again, Write); stats.InFlight != 0 {
		t.Fatalf("in flight %d after the resumed ticket's release, want 0", stats.InFlight)
	}
}

// A request that took its place in the arrival order and only then asked for room - a write
// reading its body off the connection first - is queued where it arrived: the requests that
// arrived while it was busy, and are queued already, do not overtake it.
func TestAnEarlierArrivalIsQueuedAheadOfLaterOnesAlreadyWaiting(t *testing.T) {
	l := newLimiter(1, 1, patient)
	held := admit(t, l, Write, 1)
	early := l.Arrive(Write, 1)
	if ticket := early.AdmitNow(); ticket != nil {
		t.Fatal("a write was admitted at once while the only unit was held")
	}
	later := admitInBackground(t, l, context.Background(), Write, 1)

	earliest := make(chan admitted, 1)
	go func() {
		ticket, err := early.Admit(context.Background())
		earliest <- admitted{ticket, err}
	}()
	waitForQueued(t, l, Write, 2)
	held.Release()

	first := receive(t, earliest)
	if first.err != nil {
		t.Fatalf("the write that arrived first: %v, want it admitted first", first.err)
	}
	assertPending(t, later)
	first.ticket.Release()
	if result := receive(t, later); result.err != nil {
		t.Fatalf("the write that arrived second: %v, want it admitted after the first", result.err)
	}
}

// A parked ticket takes its units back at the place its request arrived, ahead of the writes
// that arrived after it and are queued already: a write that waits for a document's writer gate
// gave its units back, and the writes that took them meanwhile are not ahead of it in the queue.
func TestAResumedTicketIsGrantedAheadOfLaterArrivals(t *testing.T) {
	l := newLimiter(1, 1, patient)
	parked := admit(t, l, Write, 1)
	parked.Park()
	holder := admit(t, l, Write, 1)
	later := admitInBackground(t, l, context.Background(), Write, 1)

	resumed := make(chan error, 1)
	go func() { resumed <- parked.Resume(context.Background()) }()
	waitForQueued(t, l, Write, 2)
	holder.Release()

	select {
	case err := <-resumed:
		if err != nil {
			t.Fatalf("resume once the unit came free: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the parked ticket never resumed, though it arrived before the queued write")
	}
	assertPending(t, later)
	parked.Release()
	if result := receive(t, later); result.err != nil {
		t.Fatalf("the write that arrived after the parked one: %v, want it admitted after it", result.err)
	}
}

// A unit given back for a wait on the network (GiveBack) is free for as long as the wait lasts and
// taken back after it (Away.TakeBack): a ticket's units through its class's queue, waiting at most
// its class's wait however long the network took, since that wait is no wait for room and is not
// counted into the one wait the queue costs the request; a place as Retake takes one. A take-back
// refused releases the ticket for good - no unit is counted for it, and nothing resumes it again -
// and puts the refusal on the request's store mark, so its next statement and its commit are
// refused with it, while its context goes on; a place refused is answered as a busy pool. Units
// parked already, by a body still arriving, are left parked.
func TestAUnitGivenBackForAWaitOnTheNetworkIsTakenBackWithinItsClassesWait(t *testing.T) {
	const (
		wait  = 300 * time.Millisecond
		slack = 250 * time.Millisecond
	)
	l := newLimiter(1, 1, wait)
	ticket := admit(t, l, Read, 1)
	defer ticket.Release()
	ctx := store.WithTransactionTracking(WithTicket(context.Background(), ticket))

	away := GiveBack(ctx)
	if stats := classStats(l, Read); stats.InFlight != 0 {
		t.Fatalf("in flight %d while the request waits on the network, want 0: its unit given back", stats.InFlight)
	}
	// The wait on the network, longer than the class's own wait.
	time.Sleep(2 * wait)
	if err := away.TakeBack(ctx); err != nil {
		t.Fatalf("take back with the unit free: %v", err)
	}
	if stats := classStats(l, Read); stats.InFlight != 1 {
		t.Fatalf("in flight %d once the unit was taken back, want 1", stats.InFlight)
	}

	away = GiveBack(ctx)
	holder := admit(t, l, Read, 1)
	started := time.Now()
	err := away.TakeBack(ctx)
	waited := time.Since(started)
	holder.Release()
	if refusalReason(err) != WaitExceeded {
		t.Fatalf("take back while another request holds the unit: %v, want a wait_exceeded refusal", err)
	}
	if waited < wait-slack/2 || waited > wait+slack {
		t.Errorf("the take-back was refused after %v, want after its class's %v wait: the network's wait is not counted into it, and it waits no longer", waited, wait)
	}
	var refusal *Refusal
	if committed := store.CommitRefusal(ctx); !errors.As(committed, &refusal) || refusal.Reason != WaitExceeded {
		t.Errorf("the refused request's commit is refused with %v, want the take-back's refusal", committed)
	}
	if ctx.Err() != nil {
		t.Errorf("the refused request's context ended (%v), want it live: the refusal rides on its store mark", ctx.Err())
	}
	if err := away.TakeBack(context.Background()); err != nil {
		t.Fatalf("a second take-back of the refused ticket: %v, want nothing taken and no error", err)
	}
	if err := ticket.Resume(context.Background()); err != nil {
		t.Fatalf("a resume of the refused ticket: %v, want nothing taken and no error", err)
	}
	if stats := classStats(l, Read); stats.InFlight != 0 {
		t.Fatalf("in flight %d with the holder gone and the refused ticket asked again, want 0: a refused ticket is released for good", stats.InFlight)
	}

	writer := admit(t, l, Write, 1)
	defer writer.Release()
	writer.Park()
	if err := GiveBack(WithTicket(context.Background(), writer)).TakeBack(context.Background()); err != nil {
		t.Fatalf("give back and take back a ticket parked already: %v", err)
	}
	if stats := classStats(l, Write); stats.InFlight != 0 {
		t.Fatalf("write in flight %d, want 0: the take-back left the units another wait parked to it", stats.InFlight)
	}
	if err := GiveBack(context.Background()).TakeBack(context.Background()); err != nil {
		t.Fatalf("give back and take back with nothing held: %v", err)
	}

	outside := New(Limits{OutsideWait: wait}).Outside()
	place, err := outside.Hold(context.Background(), HeartbeatCheck)
	if err != nil {
		t.Fatalf("a place: %v", err)
	}
	placed := WithPlace(context.Background(), place)
	away = GiveBack(placed)
	if held := outside.Stats().InUse; held != 0 {
		t.Fatalf("%d places held while the work waits on the network, want none", held)
	}
	if err := away.TakeBack(placed); err != nil {
		t.Fatalf("take the place back with every place free: %v", err)
	}
	if held := outside.Stats().InUse; held != 1 {
		t.Fatalf("%d places held once the place was taken back, want 1", held)
	}
	away = GiveBack(placed)
	takeEveryPlace(t, outside)
	if err := away.TakeBack(placed); !store.IsPoolBusy(err) || outsideRefusalReason(err) != WaitExceeded {
		t.Fatalf("take the place back with every place held: %v, want a busy pool wrapping the wait_exceeded refusal", err)
	}
}

// The queue costs a request one wait however often it asks for room: a write queued for most of
// its wait, admitted, parked at a document's writer gate and refused on resuming is refused
// within what is left of that one wait, not given a whole second one. Twice the wait is what had
// a write refused long after it was admitted.
func TestAQueuedThenParkedRequestWaitsOnceOverAll(t *testing.T) {
	const wait = time.Second
	const queued = 600 * time.Millisecond
	l := newLimiter(1, 1, wait)
	held := admit(t, l, Write, 1)
	outcome := admitInBackground(t, l, context.Background(), Write, 1)
	time.Sleep(queued)
	held.Release()
	admittedAfterWaiting := receive(t, outcome)
	if admittedAfterWaiting.err != nil {
		t.Fatalf("the write queued for %v: %v, want it admitted", queued, admittedAfterWaiting.err)
	}

	admittedAfterWaiting.ticket.Park()
	other := admit(t, l, Write, 1)
	defer other.Release()
	resuming := time.Now()
	err := admittedAfterWaiting.ticket.Resume(context.Background())
	waited := time.Since(resuming)
	if refusalReason(err) != WaitExceeded {
		t.Fatalf("resume with the unit held: %v, want a wait_exceeded refusal", err)
	}
	if rest := wait - queued; waited > rest+250*time.Millisecond {
		t.Fatalf("the parked write waited %v more after %v in the queue, want no more than the %v left of its %v wait",
			waited, queued, rest, wait)
	}
	if longest := classStats(l, Write).LongestRefusedWait; longest < wait-250*time.Millisecond || longest > wait+250*time.Millisecond {
		t.Fatalf("the refused write is recorded as having waited %v, want its whole %v wait over both asks", longest, wait)
	}
}

func TestTheTicketRidesOnTheContext(t *testing.T) {
	l := newLimiter(1, 1, patient)
	ticket := admit(t, l, Read, 1)
	defer ticket.Release()
	if got := TicketFrom(WithTicket(context.Background(), ticket)); got != ticket {
		t.Fatalf("TicketFrom = %p, want %p", got, ticket)
	}
	if got := TicketFrom(context.Background()); got != nil {
		t.Fatalf("TicketFrom a context without one = %p, want nil", got)
	}
}

func TestClassOfMethods(t *testing.T) {
	for method, want := range map[string]Class{"GET": Read, "HEAD": Read, "POST": Write, "PUT": Write, "PATCH": Write, "DELETE": Write} {
		if got := ClassOf(method); got != want {
			t.Errorf("ClassOf(%s) = %s, want %s", method, got, want)
		}
	}
}

// The server reads 14 at once and writes 12 at once per CPU it may use, at least one CPU's worth
// and at most MaxReadLimit (28) and MaxWriteLimit (24), so two CPUs already reach both caps and 16
// get no more; the two together hold at most MaxAdmittedUnits (40), a read waits
// DefaultReadWait, 1 s, for room and a write DefaultWriteWait, 2 s.
func TestTheDefaultLimitsFollowTheCPUs(t *testing.T) {
	for procs, want := range map[int][2]int{0: {14, 12}, 1: {14, 12}, 2: {28, 24}, 3: {28, 24}, 16: {28, 24}, 64: {28, 24}} {
		if got := [2]int{readLimit(procs), writeLimit(procs)}; got != want {
			t.Errorf("readLimit, writeLimit(%d) = %v, want %v", procs, got, want)
		}
	}
	limits := DefaultLimits()
	procs := runtime.GOMAXPROCS(0)
	if limits.Read != readLimit(procs) || limits.Write != writeLimit(procs) {
		t.Errorf("DefaultLimits() = %+v, want reads %d and writes %d for GOMAXPROCS %d", limits, readLimit(procs), writeLimit(procs), procs)
	}
	if limits.Total != MaxAdmittedUnits {
		t.Errorf("DefaultLimits() = %+v, want the two classes capped at %d units together", limits, MaxAdmittedUnits)
	}
	if limits.ReadWait != time.Second {
		t.Errorf("DefaultLimits() = %+v, want a read to wait 1 s for room", limits)
	}
	if limits.WriteWait != 2*time.Second {
		t.Errorf("DefaultLimits() = %+v, want a write to wait 2 s for room", limits)
	}
}

// The class caps add up to more than the combined cap, so a class reaches its own cap only while
// the other is quiet: with every read unit held a write class of 24 has 12 units left, and with
// no read held it has all 24.
func TestTheCombinedCapBoundsTheTwoClassesTogether(t *testing.T) {
	l := New(Limits{
		Read: MaxReadLimit, Write: MaxWriteLimit, Total: MaxAdmittedUnits,
		ReadWait: 20 * time.Millisecond, WriteWait: 20 * time.Millisecond,
	})
	reads := admit(t, l, Read, MaxReadLimit)
	writes := make([]*Ticket, 0, MaxWriteLimit)
	for range MaxAdmittedUnits - MaxReadLimit {
		writes = append(writes, admit(t, l, Write, 1))
	}
	if _, err := l.Admit(context.Background(), Write, 1); refusalReason(err) != WaitExceeded {
		t.Fatalf("write %d of a class of %d, with the reads holding %d of %d units: %v, want a wait_exceeded refusal",
			MaxAdmittedUnits-MaxReadLimit+1, MaxWriteLimit, MaxReadLimit, MaxAdmittedUnits, err)
	}
	if stats := classStats(l, Write); stats.InFlight != MaxAdmittedUnits-MaxReadLimit || stats.Limit != MaxWriteLimit {
		t.Fatalf("write stats %+v, want %d of its %d units in flight", stats, MaxAdmittedUnits-MaxReadLimit, MaxWriteLimit)
	}
	if got := (Totals{Limit: MaxAdmittedUnits, InFlight: MaxAdmittedUnits}); l.Totals() != got {
		t.Fatalf("totals %+v, want %+v", l.Totals(), got)
	}

	reads.Release()
	for _, ticket := range writes {
		ticket.Release()
	}
	writes = writes[:0]
	for range MaxWriteLimit {
		writes = append(writes, admit(t, l, Write, 1))
	}
	if _, err := l.Admit(context.Background(), Write, 1); refusalReason(err) != WaitExceeded {
		t.Fatalf("write %d with no read held: %v, want a wait_exceeded refusal past the class's %d", MaxWriteLimit+1, err, MaxWriteLimit)
	}
	if got := (Totals{Limit: MaxAdmittedUnits, InFlight: MaxWriteLimit}); l.Totals() != got {
		t.Fatalf("totals %+v, want %+v", l.Totals(), got)
	}
	for _, ticket := range writes {
		ticket.Release()
	}
}

// A request its own class has room for, which only the combined cap holds up, is granted by the
// other class's release.
func TestAWaitOnTheCombinedCapEndsWhenTheOtherClassReleases(t *testing.T) {
	l := New(Limits{Read: 4, Write: 4, Total: 5, ReadWait: patient, WriteWait: patient})
	read := admit(t, l, Read, 3)
	write := admit(t, l, Write, 2)
	waiting := admitInBackground(t, l, context.Background(), Write, 2)
	assertPending(t, waiting)
	read.Release()
	result := receive(t, waiting)
	if result.err != nil {
		t.Fatalf("the write waiting on the combined cap: %v", result.err)
	}
	if got := (Totals{Limit: 5, InFlight: 4}); l.Totals() != got {
		t.Fatalf("totals %+v, want %+v", l.Totals(), got)
	}
	write.Release()
	result.ticket.Release()
}

// A head the combined cap holds up is not overtaken by a later, lighter request of the other
// class, which would fit both its own class and the cap: the room belongs to the head.
func TestAHeadHeldByTheCombinedCapIsNotOvertakenByTheOtherClass(t *testing.T) {
	l := New(Limits{Read: 4, Write: 4, Total: 4, ReadWait: patient, WriteWait: patient})
	read := admit(t, l, Read, 3)
	heavy := admitInBackground(t, l, context.Background(), Write, 2)
	light := admitInBackground(t, l, context.Background(), Read, 1)
	assertPending(t, heavy)
	assertPending(t, light)
	if got := (Totals{Limit: 4, InFlight: 3}); l.Totals() != got {
		t.Fatalf("totals %+v, want %+v: the last unit is the queued write's", l.Totals(), got)
	}
	read.Release()
	granted := receive(t, heavy)
	if granted.err != nil {
		t.Fatalf("the write at the head: %v", granted.err)
	}
	behind := receive(t, light)
	if behind.err != nil {
		t.Fatalf("the read behind it: %v", behind.err)
	}
	behind.ticket.Release()
	granted.ticket.Release()
	if got := (Totals{Limit: 4}); l.Totals() != got {
		t.Fatalf("totals %+v, want %+v", l.Totals(), got)
	}
}

// A parked ticket gives its units back to the combined cap as well as to its class, so a request
// the cap alone held up runs while its holder waits elsewhere.
func TestAParkedTicketFreesCombinedRoom(t *testing.T) {
	l := New(Limits{Read: 4, Write: 4, Total: 4, ReadWait: 20 * time.Millisecond, WriteWait: 20 * time.Millisecond})
	read := admit(t, l, Read, 2)
	parked := admit(t, l, Write, 2)
	if _, err := l.Admit(context.Background(), Write, 2); refusalReason(err) != WaitExceeded {
		t.Fatalf("a write its class has room for, past the combined cap: %v, want a wait_exceeded refusal", err)
	}
	parked.Park()
	if got := (Totals{Limit: 4, InFlight: 2}); l.Totals() != got {
		t.Fatalf("totals %+v after parking, want %+v", l.Totals(), got)
	}
	other := admit(t, l, Write, 2)
	other.Release()
	if err := parked.Resume(context.Background()); err != nil {
		t.Fatalf("resume once the room came back: %v", err)
	}
	if got := (Totals{Limit: 4, InFlight: 4}); l.Totals() != got {
		t.Fatalf("totals %+v after resuming, want %+v", l.Totals(), got)
	}
	read.Release()
	parked.Release()
	if got := (Totals{Limit: 4}); l.Totals() != got {
		t.Fatalf("totals %+v after every release, want %+v", l.Totals(), got)
	}
}

// With the reads idle the writes take their whole class cap; with the reads asking for all 28 of
// their units the reservation hands the room back to them, one released write at a time, until
// the reads hold ReservedReadUnits and the writes the rest of the combined cap - and only then
// is the write that has been waiting all along granted.
func TestTheReservationKeepsReadRoomFromWritesWhileReadsWait(t *testing.T) {
	l := New(Limits{
		Read: MaxReadLimit, Write: MaxWriteLimit, Total: MaxAdmittedUnits,
		ReservedReads: ReservedReadUnits, ReadWait: patient, WriteWait: patient,
	})
	writes := make([]*Ticket, 0, MaxWriteLimit)
	for range MaxWriteLimit {
		writes = append(writes, admit(t, l, Write, 1))
	}
	if stats := classStats(l, Write); stats.InFlight != MaxWriteLimit {
		t.Fatalf("write stats %+v with no read held, want its whole cap of %d units in flight", stats, MaxWriteLimit)
	}
	reads := make([]*Ticket, 0, MaxReadLimit)
	for range MaxAdmittedUnits - MaxWriteLimit {
		reads = append(reads, admit(t, l, Read, 1))
	}
	// A write asks for the next unit ahead of every read that queues after it, and the reads
	// ask for the 28 their class holds.
	waitingWrite := admitInBackground(t, l, context.Background(), Write, 1)
	readWaiters := make([]<-chan admitted, 0, MaxReadLimit-(MaxAdmittedUnits-MaxWriteLimit))
	for range MaxReadLimit - (MaxAdmittedUnits - MaxWriteLimit) {
		readWaiters = append(readWaiters, admitInBackground(t, l, context.Background(), Read, 1))
	}
	const toTheReads = MaxWriteLimit - (MaxAdmittedUnits - ReservedReadUnits)
	for index := range toTheReads {
		writes[index].Release()
		granted := receive(t, readWaiters[index])
		if granted.err != nil {
			t.Fatalf("the read waiting for the reserved room: %v", granted.err)
		}
		reads = append(reads, granted.ticket)
		assertPending(t, waitingWrite)
	}
	if stats := classStats(l, Write); stats.InFlight != MaxAdmittedUnits-ReservedReadUnits {
		t.Fatalf("write stats %+v with the reads waiting, want the %d units the reservation leaves them",
			stats, MaxAdmittedUnits-ReservedReadUnits)
	}
	if stats := classStats(l, Read); stats.InFlight != ReservedReadUnits {
		t.Fatalf("read stats %+v, want the reserved %d units in flight", stats, ReservedReadUnits)
	}
	if got := (Totals{Limit: MaxAdmittedUnits, InFlight: MaxAdmittedUnits, ReservedReads: ReservedReadUnits}); l.Totals() != got {
		t.Fatalf("totals %+v, want %+v", l.Totals(), got)
	}

	// The reads hold their reservation now, so the next unit a write gives up is the waiting
	// write's and not theirs.
	writes[toTheReads].Release()
	granted := receive(t, waitingWrite)
	if granted.err != nil {
		t.Fatalf("the write waiting under the reservation: %v", granted.err)
	}
	if stats := classStats(l, Write); stats.InFlight != MaxAdmittedUnits-ReservedReadUnits {
		t.Fatalf("write stats %+v once the reads hold the reservation, want %d units in flight",
			stats, MaxAdmittedUnits-ReservedReadUnits)
	}

	granted.ticket.Release()
	for _, ticket := range writes[toTheReads+1:] {
		ticket.Release()
	}
	for _, outcome := range readWaiters[toTheReads:] {
		result := receive(t, outcome)
		if result.err != nil {
			t.Fatalf("a read still queued once the writes were done: %v", result.err)
		}
		reads = append(reads, result.ticket)
	}
	for _, ticket := range reads {
		ticket.Release()
	}
	if got := (Totals{Limit: MaxAdmittedUnits, ReservedReads: ReservedReadUnits}); l.Totals() != got {
		t.Fatalf("totals %+v after every release, want %+v", l.Totals(), got)
	}
}

// A write the reservation alone holds up is not in the way of the reads the room is kept for:
// the read queued behind it takes the unit the write may not, though the write arrived first.
func TestAWriteHeldByTheReservationDoesNotHoldBackTheReads(t *testing.T) {
	l := New(Limits{Read: 4, Write: 4, Total: 4, ReservedReads: 2, ReadWait: patient, WriteWait: patient})
	writes := []*Ticket{admit(t, l, Write, 1), admit(t, l, Write, 1), admit(t, l, Write, 1)}
	read := admit(t, l, Read, 1)
	waitingWrite := admitInBackground(t, l, context.Background(), Write, 1)
	waitingRead := admitInBackground(t, l, context.Background(), Read, 1)

	writes[0].Release()
	granted := receive(t, waitingRead)
	if granted.err != nil {
		t.Fatalf("the read queued behind the held write: %v", granted.err)
	}
	assertPending(t, waitingWrite)
	if got := (Totals{Limit: 4, InFlight: 4, ReservedReads: 2}); l.Totals() != got {
		t.Fatalf("totals %+v, want %+v: the freed unit is the reads' reserved one", l.Totals(), got)
	}

	// Nothing of the reads waits now, so the write takes the next unit itself.
	writes[1].Release()
	late := receive(t, waitingWrite)
	if late.err != nil {
		t.Fatalf("the write once no read waited for the reserved room: %v", late.err)
	}
	read.Release()
	granted.ticket.Release()
	late.ticket.Release()
	writes[2].Release()
	if got := (Totals{Limit: 4, ReservedReads: 2}); l.Totals() != got {
		t.Fatalf("totals %+v after every release, want %+v", l.Totals(), got)
	}
}

// A write the reservation holds up is granted once the reads drop below the reservation and ask
// for nothing more: the room is reserved against the reads that want it, not idled.
func TestAWriteHeldByTheReservationRunsWhenTheReadsDropBelowIt(t *testing.T) {
	l := New(Limits{Read: 8, Write: 8, Total: 8, ReservedReads: 5, ReadWait: patient, WriteWait: patient})
	writes := make([]*Ticket, 0, 4)
	for range 4 {
		writes = append(writes, admit(t, l, Write, 1))
	}
	reads := make([]*Ticket, 0, 4)
	for range 4 {
		reads = append(reads, admit(t, l, Read, 1))
	}
	waitingWrite := admitInBackground(t, l, context.Background(), Write, 1)
	waitingRead := admitInBackground(t, l, context.Background(), Read, 1)

	// The first read's unit goes back to the reads, which hold less than the reserved 5.
	reads[0].Release()
	granted := receive(t, waitingRead)
	if granted.err != nil {
		t.Fatalf("the read waiting for the reserved room: %v", granted.err)
	}
	assertPending(t, waitingWrite)

	// With no read waiting the reservation keeps nothing back, so the write takes the room the
	// reads left, past the 3 units the reservation would leave it.
	reads[1].Release()
	late := receive(t, waitingWrite)
	if late.err != nil {
		t.Fatalf("the write once the reads dropped below the reservation: %v", late.err)
	}
	if stats := classStats(l, Write); stats.InFlight != 5 {
		t.Fatalf("write stats %+v with the reads light, want 5 units in flight", stats)
	}
	if stats := classStats(l, Read); stats.InFlight != 3 {
		t.Fatalf("read stats %+v, want the 3 units the reads kept", stats)
	}
	if got := (Totals{Limit: 8, InFlight: 8, ReservedReads: 5}); l.Totals() != got {
		t.Fatalf("totals %+v, want %+v", l.Totals(), got)
	}

	for _, ticket := range append(writes, reads[2], reads[3], granted.ticket, late.ticket) {
		ticket.Release()
	}
	if got := (Totals{Limit: 8, ReservedReads: 5}); l.Totals() != got {
		t.Fatalf("totals %+v after every release, want %+v", l.Totals(), got)
	}
}

// A weight-2 write at the head of the write queue that the combined cap and the read reservation
// both refuse holds back only its own queue: the unit a release frees is one the write could not
// have taken, since the reads it waits for have not taken their reservation yet, so the read
// queued behind it is granted. Counted as held by the cap alone, that write kept every read
// behind it waiting for room the server had for them.
func TestAWriteBothTheCapAndTheReservationRefuseDoesNotHoldBackTheReads(t *testing.T) {
	l := New(Limits{Read: 8, Write: 8, Total: 8, ReservedReads: 4, ReadWait: patient, WriteWait: patient})
	writes := make([]*Ticket, 0, 4)
	for range 4 {
		writes = append(writes, admit(t, l, Write, 1))
	}
	reads := make([]*Ticket, 0, 4)
	for range 4 {
		reads = append(reads, admit(t, l, Read, 1))
	}
	heavyWrite := admitInBackground(t, l, context.Background(), Write, 2)
	waitingRead := admitInBackground(t, l, context.Background(), Read, 1)

	// 7 units held: the write's 2 are past the cap of 8, and the reservation keeps one of those
	// 8 for the read waiting besides, so the freed unit is the read's.
	reads[0].Release()
	granted := receive(t, waitingRead)
	if granted.err != nil {
		t.Fatalf("the read queued behind the held write: %v", granted.err)
	}
	assertPending(t, heavyWrite)
	if got := (Totals{Limit: 8, InFlight: 8, ReservedReads: 4}); l.Totals() != got {
		t.Fatalf("totals %+v, want %+v: the freed unit went to the read", l.Totals(), got)
	}

	// With no read waiting the reservation keeps nothing back, so the write waits for the cap
	// alone: one released write is not enough for its 2 units and the second is.
	writes[0].Release()
	assertPending(t, heavyWrite)
	writes[1].Release()
	late := receive(t, heavyWrite)
	if late.err != nil {
		t.Fatalf("the write once the cap had room for its 2 units: %v", late.err)
	}

	for _, ticket := range append(writes[2:], reads[1], reads[2], reads[3], granted.ticket, late.ticket) {
		ticket.Release()
	}
	if got := (Totals{Limit: 8, ReservedReads: 4}); l.Totals() != got {
		t.Fatalf("totals %+v after every release, want %+v", l.Totals(), got)
	}
}

// The server keeps ReservedReadUnits of the combined cap for the reads, and New takes a
// reservation of zero as none and holds one past the read limit or the cap down to what the
// reads could hold at all.
func TestTheDefaultLimitsReserveReadRoom(t *testing.T) {
	if limits := DefaultLimits(); limits.ReservedReads != ReservedReadUnits {
		t.Errorf("DefaultLimits() = %+v, want %d of its units reserved for reads", limits, ReservedReadUnits)
	}
	if got := New(DefaultLimits()).Totals(); got.ReservedReads != ReservedReadUnits {
		t.Errorf("totals %+v of a default limiter, want %d of its units reserved for reads", got, ReservedReadUnits)
	}
	for _, limits := range []struct {
		name  string
		given Limits
		want  int
	}{
		{"none unless it is asked for", Limits{Read: 8, Write: 4, Total: 10}, 0},
		{"never below none", Limits{Read: 8, Write: 4, Total: 10, ReservedReads: -3}, 0},
		{"never past the read limit", Limits{Read: 6, Write: 4, Total: 10, ReservedReads: 8}, 6},
		{"never past the combined cap", Limits{Read: 8, Write: 4, Total: 5, ReservedReads: 7}, 5},
	} {
		t.Run(limits.name, func(t *testing.T) {
			if got := New(limits.given).Totals().ReservedReads; got != limits.want {
				t.Errorf("New(%+v) reserved %d read units, want %d", limits.given, got, limits.want)
			}
		})
	}
}
