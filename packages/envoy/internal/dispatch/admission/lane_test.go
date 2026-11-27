package admission

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// A heavy read route's lane runs one request at a time where its limit is one: the second waits
// while the first holds the lane, takes it the moment the first leaves, and a request of another
// lane - or of no lane at all - is not held behind either of them. Without the lanes every one of
// these runs at once, which is what let three to five of the same heavy statement run together in
// Postgres and stall every other request (INFRA-592).
func TestALaneRunsOneRequestAtATimeAndHoldsNoOtherRouteBehindIt(t *testing.T) {
	limiter := New(Limits{Read: 8, Write: 8, HeavyReadLane: 1, ReadWait: time.Second})
	ctx := context.Background()

	first, err := enterLane(ctx, limiter, Search)
	if err != nil {
		t.Fatalf("take the search lane: %v", err)
	}
	// Another lane, and a route in no lane, are untouched by the search lane's one place.
	references, err := enterLane(ctx, limiter, References)
	if err != nil {
		t.Fatalf("take the references lane while the search lane is full: %v", err)
	}
	references()
	if none, err := enterLane(ctx, limiter, NoLane); err != nil {
		t.Fatalf("a route of no lane was held behind the search lane: %v", err)
	} else {
		none()
	}

	entered := make(chan func(), 1)
	go func() {
		leave, err := enterLane(ctx, limiter, Search)
		if err != nil {
			t.Errorf("the second search waited for the lane and was refused: %v", err)
			close(entered)
			return
		}
		entered <- leave
	}()
	select {
	case <-entered:
		t.Fatal("a second search took the search lane while the first still held it")
	case <-time.After(100 * time.Millisecond):
	}
	if stats := laneStatsOf(t, limiter, Search); stats.InFlight != 1 || stats.Queued != 1 {
		t.Fatalf("the search lane holds %d and queues %d, want one of each", stats.InFlight, stats.Queued)
	}

	first()
	select {
	case leave := <-entered:
		if leave == nil {
			t.Fatal("the second search did not take the lane the first left")
		}
		leave()
	case <-time.After(10 * time.Second):
		t.Fatal("the second search did not take the lane the first left")
	}
}

// A lane whose queue is at its cap, ReadBurstFactor times its limit, the bound on its memory
// (fifoGate), refuses at once, 429 OVERLOADED with Retry-After, and the refusal names the lane:
// nothing of the request ran, and the client is told which route is busy.
func TestAFullLaneRefusesWithTheLaneNamed(t *testing.T) {
	limiter := New(Limits{Read: 8, Write: 8, HeavyReadLane: 1, ReadWait: 10 * time.Second})
	ctx := context.Background()
	held, err := enterLane(ctx, limiter, Search)
	if err != nil {
		t.Fatalf("take the search lane: %v", err)
	}
	defer held()
	// The queue holds ReadBurstFactor times the limit, its cap; fill it, then arrive one past it.
	for range ReadBurstFactor {
		go func() {
			if leave, err := enterLane(ctx, limiter, Search); err == nil {
				leave()
			}
		}()
	}
	waitForLane(t, limiter, Search, ReadBurstFactor)

	leave, err := enterLane(ctx, limiter, Search)
	if leave != nil {
		leave()
	}
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("a search arriving at a full lane queue was answered %v, want a refusal", err)
	}
	if refusal.Lane != Search || refusal.Reason != QueueFull {
		t.Fatalf("the refusal is %+v, want the search lane and %s", refusal, QueueFull)
	}
	if message := refusal.Error(); !strings.Contains(message, "search lane") {
		t.Fatalf("the refusal reads %q, want it to name the search lane", message)
	}
	recorder := httptest.NewRecorder()
	WriteRefused(recorder, refusal)
	if recorder.Code != 429 || recorder.Header().Get("Retry-After") != RetryAfter {
		t.Fatalf("the lane's refusal answered %d with Retry-After %q, want 429 and %s",
			recorder.Code, recorder.Header().Get("Retry-After"), RetryAfter)
	}
	if body := recorder.Body.String(); !strings.Contains(body, CodeOverloaded) || !strings.Contains(body, "search lane") {
		t.Fatalf("the lane's refusal body is %s, want %s naming the search lane", body, CodeOverloaded)
	}
}

// A lane and the class behind it cost one request one wait between them: the time a busy lane
// holds a request comes off the wait it has left in its class's queue, so a lane route the lane
// delays and the class then refuses is answered within the one wait its class states. Before the
// arrival was taken ahead of the lane and handed to it, the two waits ran one after the other and
// such a request waited up to twice the wait: at ten times the load 103 to 167 refused reads a run
// were answered 1.0 to 2.4 s into a 1 s wait, every one of them on one of the four lane routes
// (INFRA-592).
func TestALaneAndItsClassCostOneWait(t *testing.T) {
	const (
		wait     = 400 * time.Millisecond
		laneHeld = 300 * time.Millisecond
		slack    = 150 * time.Millisecond
	)
	limiter := New(Limits{Read: 1, Write: 1, HeavyReadLane: 1, ReadWait: wait})
	ctx := context.Background()
	leave, err := enterLane(ctx, limiter, Search)
	if err != nil {
		t.Fatalf("take the search lane's only place: %v", err)
	}
	// The read class's only unit is held for the whole test, so the request below waits for its
	// lane first and is then refused by its class.
	ticket, err := limiter.Admit(ctx, Read, 1)
	if err != nil {
		t.Fatalf("take the read class's only unit: %v", err)
	}
	defer ticket.Release()

	answered := make(chan error, 1)
	started := time.Now()
	go func() {
		// The order api.admitted takes them in: the arrival, then the lane, then the units.
		arrival := limiter.Arrive(Read, 1)
		leave, err := limiter.EnterLane(ctx, Search, arrival)
		if err != nil {
			answered <- err
			return
		}
		defer leave()
		admitted, err := arrival.Admit(ctx)
		admitted.Release()
		answered <- err
	}()
	waitForLane(t, limiter, Search, 1)
	time.AfterFunc(laneHeld, leave)

	select {
	case err := <-answered:
		elapsed := time.Since(started)
		if elapsed > wait+slack {
			t.Errorf("the lane route was answered %v after it arrived (%v), want it within its one %v wait and %v of slack",
				elapsed, err, wait, slack)
		}
		if elapsed < laneHeld {
			t.Errorf("the lane route was answered %v after it arrived, before the %v its lane was held: it never waited for the lane",
				elapsed, laneHeld)
		}
		var refusal *Refusal
		if !errors.As(err, &refusal) {
			t.Fatalf("the lane route was answered %v, want the refusal of a class with no room", err)
		}
		if refusal.Lane != NoLane || refusal.Reason != WaitExceeded {
			t.Errorf("the refusal is %+v, want the read class's %s with no lane named", refusal, WaitExceeded)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the lane route was never answered")
	}
}

// A lane's refusal is a refusal of the read class, so it counts against the longest a refused
// read waited (ClassStats.LongestRefusedWait), the figure these tests read to see what a
// refusal cost the request that got it. A lane holds the read's one wait, so a read the lane
// refuses has spent that whole wait and nothing else records it: left out, the figure said no
// read had waited at all while every refusal of a lane route waited its second.
func TestALaneRefusalCountsAgainstTheReadClassesLongestWait(t *testing.T) {
	const wait = 300 * time.Millisecond
	limiter := New(Limits{Read: 8, Write: 8, HeavyReadLane: 1, ReadWait: wait})
	ctx := context.Background()
	held, err := enterLane(ctx, limiter, Search)
	if err != nil {
		t.Fatalf("take the search lane's only place: %v", err)
	}
	defer held()

	if _, err := enterLane(ctx, limiter, Search); refusalReason(err) != WaitExceeded {
		t.Fatalf("the second search waited out the lane: %v, want a wait_exceeded refusal", err)
	}
	stats := laneStatsOf(t, limiter, Search)
	if stats.WaitExceeded != 1 {
		t.Fatalf("the search lane counts %d requests refused for waiting, want 1", stats.WaitExceeded)
	}
	longest := classStats(limiter, Read).LongestRefusedWait
	if longest < wait-150*time.Millisecond || longest > wait+250*time.Millisecond {
		t.Fatalf("the read class records the longest refused wait as %v, want the %v the lane refused the read after", longest, wait)
	}
}

// A lane full for less than its wait answers every waiter, however many wait in its queue below
// its cap: with every place of a lane of 7 held (MaxHeavyReadLane, the width the cluster copy
// runs), twice its limit and eight more heavy reads arrive at once - 22, past the 14 its queue
// held before - and the places come back within their wait (DefaultReadWait): every one of them
// takes the lane and then its read unit, and none is refused. Held to those 14 at any time, the
// lanes refused such reads at once while they drained within the wait: 29 of the 150 refusals of
// c-scale's step head-j-10x-5 at eb23d38e (ReadBurstFactor).
func TestALaneFullForLessThanItsWaitAnswersEveryWaiter(t *testing.T) {
	t.Parallel()
	const lane = MaxHeavyReadLane
	limiter := New(Limits{Read: MaxReadLimit, Write: 1, HeavyReadLane: lane})
	ctx := testwait.Context(t)
	places := make([]func(), lane)
	for i := range places {
		leave, err := enterLane(ctx, limiter, Search)
		if err != nil {
			t.Fatalf("take place %d of the search lane's %d: %v", i+1, lane, err)
		}
		places[i] = leave
	}
	arrivals := 2*lane + 8
	answers := make(chan error, arrivals)
	for range arrivals {
		go func() {
			// The order api.admitted takes them in: the arrival, then the lane, then the units.
			arrival := limiter.Arrive(Read, 1)
			leave, err := limiter.EnterLane(ctx, Search, arrival)
			if err != nil {
				answers <- err
				return
			}
			ticket, err := arrival.Admit(ctx)
			ticket.Release()
			leave()
			answers <- err
		}()
	}
	stats := testwait.Until(t, "every heavy read queued in its lane or refused", func() (LaneStats, bool) {
		stats := laneStatsOf(t, limiter, Search)
		return stats, stats.Queued+int(stats.QueueFull)+int(stats.WaitExceeded) == arrivals
	})
	if stats.QueueFull != 0 {
		t.Fatalf("%d of %d heavy reads arriving at a full lane were refused queue_full, below its cap of %d (%+v), want none: their wait had room",
			stats.QueueFull, arrivals, ReadBurstFactor*lane, stats)
	}
	for _, leave := range places {
		leave()
	}
	for range arrivals {
		if err := testwait.Receive(t, "a heavy read's answer", answers); err != nil {
			t.Fatalf("a heavy read whose lane gave a place back within its wait: %v, want it admitted", err)
		}
	}
	if stats := laneStatsOf(t, limiter, Search); stats.InFlight != 0 || stats.Queued != 0 || stats.QueueFull+stats.WaitExceeded != 0 {
		t.Fatalf("the search lane's stats %+v once every heavy read was answered, want nothing held, queued or refused", stats)
	}
}

// A lane full past its wait refuses a waiter at the wait and not before: with the lane's one place
// held and never given back, a heavy read waits in its queue and is refused wait_exceeded, naming
// the lane, once its whole wait (DefaultReadWait) has run, not at once, and the read class records
// it as having waited that wait.
func TestALaneFullPastItsWaitRefusesAtTheWait(t *testing.T) {
	t.Parallel()
	limiter := New(Limits{Read: 8, Write: 1, HeavyReadLane: 1})
	held, err := enterLane(context.Background(), limiter, Search)
	if err != nil {
		t.Fatalf("take the search lane's only place: %v", err)
	}
	defer held()
	answered := make(chan error, 1)
	asked := time.Now()
	go func() {
		leave, err := enterLane(context.Background(), limiter, Search)
		if err == nil {
			leave()
		}
		answered <- err
	}()
	err = testwait.Receive(t, "the answer of a heavy read its lane never has room for", answered)
	waited := time.Since(asked)
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Reason != WaitExceeded || refusal.Lane != Search {
		t.Fatalf("a heavy read its lane never had room for: %v, want the search lane's wait_exceeded refusal", err)
	}
	if waited < DefaultReadWait {
		t.Fatalf("the heavy read was refused %v after it asked, before its %v wait had run", waited, DefaultReadWait)
	}
	if stats := laneStatsOf(t, limiter, Search); stats.WaitExceeded != 1 || stats.QueueFull != 0 || stats.Queued != 0 {
		t.Fatalf("the search lane's stats %+v, want one refusal at the wait and nobody queued", stats)
	}
	if longest := classStats(limiter, Read).LongestRefusedWait; longest < DefaultReadWait {
		t.Fatalf("the read class records the refused heavy read as having waited %v, want its whole %v wait", longest, DefaultReadWait)
	}
}

// A heavy read waiting for its lane holds no unit of the read class while it waits, so the reads of
// the other routes are admitted beside it: EnterLane takes the lane before the class's units. With
// the lane's one place held by a read that holds its unit too, a second heavy read waits in the
// lane's queue while the read class counts the holder's unit alone in flight and nobody queued, and
// a read of another route is admitted at once, with nobody ahead of it, to the unit the lane's
// waiter left free. Once the place comes back the waiter takes it, and then its unit.
func TestALaneWaiterHoldsNoReadUnitWhileItWaits(t *testing.T) {
	t.Parallel()
	limiter := New(Limits{Read: 2, Write: 1, HeavyReadLane: 1, ReadWait: time.Minute})
	ctx := testwait.Context(t)
	holder := limiter.Arrive(Read, 1)
	leave, err := limiter.EnterLane(ctx, Search, holder)
	if err != nil {
		t.Fatalf("take the search lane's only place: %v", err)
	}
	holding, err := holder.Admit(ctx)
	if err != nil {
		t.Fatalf("the lane holder's read unit: %v", err)
	}
	answered := make(chan error, 1)
	go func() {
		arrival := limiter.Arrive(Read, 1)
		leave, err := limiter.EnterLane(ctx, Search, arrival)
		if err != nil {
			answered <- err
			return
		}
		ticket, err := arrival.Admit(ctx)
		ticket.Release()
		leave()
		answered <- err
	}()
	testwait.Until(t, "the second heavy read queued for its lane", func() (LaneStats, bool) {
		stats := laneStatsOf(t, limiter, Search)
		return stats, stats.Queued == 1
	})
	if stats := classStats(limiter, Read); stats.InFlight != 1 || stats.Queued != 0 {
		t.Fatalf("read stats %+v with a heavy read waiting for its lane, want the lane holder's one unit in flight and nobody queued: the waiter holds no unit", stats)
	}
	other := limiter.Arrive(Read, 1).AdmitNow()
	if other == nil {
		t.Fatalf("a read of another route was not admitted at once beside the heavy read waiting for its lane (%+v), want the unit the waiter left free", classStats(limiter, Read))
	}
	other.Release()
	holding.Release()
	leave()
	if err := testwait.Receive(t, "the waiting heavy read's answer", answered); err != nil {
		t.Fatalf("the heavy read that waited for its lane, once the place came back: %v, want it admitted", err)
	}
}

// A heavy read's lane serves newest first once the read class's queue has stood non-empty for
// laneStandWaits (three) whole read waits, and first in, first out until then (laneGate): with the
// read class's one unit held and a read queued for it, and the lane's one place held with six
// heavy reads lined up behind it, the place that comes back goes to the heavy read that has waited
// longest while the read class's queue has been non-empty for less than one wait, at exactly one,
// at two and one nanosecond short of three; at three, by the clock its decision reads, each place
// that comes back goes to the heavy read that joined the lane last, and the lane counts those two
// grants newest first (LaneStats.NewestFirst).
//
// The class's own queue turns to its newest waiter at one wait, and a lane following it there gave
// its places to the requests that had just come when a stall of a second or two stood the class's
// queue, and left its oldest to run out their wait: one references read of 358,714 in a step at ten
// times the load (INFRA-592, c-scale at 2ecfa550, head step 6, a read class that stood about 1.8
// s). First in, first out there under a read class that stays standing handed each place to the
// heavy read that had waited longest, which reached the read class with its wait spent, was
// admitted all the same and crowded out the cheap reads: about 22,000 fewer of them answered 2xx a
// 3-minute window on the two-processor bed at a standing ten times the load (INFRA-592, a
// bisection of d416a857), so a standing class of three waits still turns its lanes.
func TestALaneServesTheNewestFirstWhileTheReadClassStands(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	l := newWithClock(Limits{Read: 1, Write: 1, HeavyReadLane: 1, ReadWait: patient}, clock)
	ctx := testwait.Context(t)
	reader := admit(t, l, Read, 1)
	queuedRead := admitInBackground(t, l, ctx, Read, 1)
	lane := orderedQueue{
		places: 1,
		take:   func(ctx context.Context) (func(), error) { return enterLane(ctx, l, Search) },
		queued: func() int { return laneStatsOf(t, l, Search).Queued },
	}
	held := holdEveryPlace(t, ctx, lane)
	answers := lineUp(t, ctx, lane, 6)
	notNewestFirst := func(when string) {
		t.Helper()
		if newestFirst := laneStatsOf(t, l, Search).NewestFirst; newestFirst != 0 {
			t.Fatalf("%d granted newest first %s, want none", newestFirst, when)
		}
	}

	clock.advance(patient - time.Nanosecond)
	held[0]()
	previous := grantedNext(t, answers, 0, "a lane serves first in, first out while the read class's queue has stood less than a wait")
	notNewestFirst("while the read class's queue had stood less than a wait")

	for next, when := range []struct {
		step time.Duration
		what string
	}{
		{time.Nanosecond, "at the read class's one whole wait, where its own queue turns to its newest waiter"},
		{patient, "at two whole read waits"},
		{patient - time.Nanosecond, "one nanosecond short of three whole read waits"},
	} {
		clock.advance(when.step)
		previous.give()
		previous = grantedNext(t, answers, next+1, "a lane serves first in, first out "+when.what)
		notNewestFirst(when.what)
	}

	clock.advance(time.Nanosecond)
	const why = "a lane serves the newest first once the read class's queue has stood three whole read waits"
	previous.give()
	newest := grantedNext(t, answers, 5, why)
	newest.give()
	last := grantedNext(t, answers, 4, why)
	if stats := laneStatsOf(t, l, Search); stats.NewestFirst != 2 || stats.Queued != 0 || stats.QueueFull+stats.WaitExceeded != 0 {
		t.Fatalf("the search lane's stats %+v, want the two grants since the read class stood three waits counted newest first, and nothing queued or refused", stats)
	}
	last.give()

	reader.Release()
	read := testwait.Receive(t, "the queued read's answer", queuedRead)
	if read.err != nil {
		t.Fatalf("the read queued in its class once the unit came back: %v, want it admitted", read.err)
	}
	read.ticket.Release()
}

// enterLane takes lane for a request arriving now, in the order api.admitted takes them: the
// arrival that stamps the request's place first, then the lane, which counts its wait against it.
func enterLane(ctx context.Context, limiter *Limiter, lane Lane) (func(), error) {
	return limiter.EnterLane(ctx, lane, limiter.Arrive(Read, 1))
}

func laneStatsOf(t *testing.T, limiter *Limiter, lane Lane) LaneStats {
	t.Helper()
	for _, stats := range limiter.LaneStats() {
		if stats.Lane == lane {
			return stats
		}
	}
	t.Fatalf("no stats for the %s lane", lane)
	return LaneStats{}
}

// waitForLane waits until lane has queued waiters, so a test that fills a queue knows it is full.
func waitForLane(t *testing.T, limiter *Limiter, lane Lane, queued int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if laneStatsOf(t, limiter, lane).Queued >= queued {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the %s lane never queued %d waiters", lane, queued)
}

// A lane is one request wide a processor, so the three-processor box the width was measured on
// keeps three and a sixteen-processor server is not held to that box's three; it is never
// narrower than three, the width below which that measurement refused requests the box would
// serve, so a one- or two-processor server keeps three too; and it stops at the read class's cap
// over the four lanes, so the heavy reads together never exceed the reads a process admits.
func TestALaneIsOneRequestWideAProcessorUpToTheReadClassesShare(t *testing.T) {
	for _, c := range []struct{ procs, want int }{{1, 3}, {2, 3}, {3, 3}, {4, 4}, {7, 7}, {16, MaxHeavyReadLane}} {
		if got := heavyReadLaneLimit(c.procs); got != c.want {
			t.Errorf("heavyReadLaneLimit(%d) = %d, want %d", c.procs, got, c.want)
		}
	}
	if lanes := len(HeavyReadLanes()); lanes*MinHeavyReadLane > readLimit(1) {
		t.Errorf("%d lanes of %d requests hold more than the %d read units one processor admits", lanes, MinHeavyReadLane, readLimit(1))
	}
	if lanes := len(HeavyReadLanes()); lanes*MaxHeavyReadLane > MaxReadLimit {
		t.Errorf("%d lanes of %d hold more than the read class's %d", lanes, MaxHeavyReadLane, MaxReadLimit)
	}
}
