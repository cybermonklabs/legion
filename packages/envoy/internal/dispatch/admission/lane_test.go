package admission

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// A lane whose queue is full refuses at once, 429 OVERLOADED with Retry-After, and the refusal
// names the lane: nothing of the request ran, and the client is told which route is busy.
func TestAFullLaneRefusesWithTheLaneNamed(t *testing.T) {
	limiter := New(Limits{Read: 8, Write: 8, HeavyReadLane: 1, ReadWait: 10 * time.Second})
	ctx := context.Background()
	held, err := enterLane(ctx, limiter, Search)
	if err != nil {
		t.Fatalf("take the search lane: %v", err)
	}
	defer held()
	// The queue holds queueFactor times the limit; fill it, then arrive one past it.
	for range queueFactor {
		go func() {
			if leave, err := enterLane(ctx, limiter, Search); err == nil {
				leave()
			}
		}()
	}
	waitForLane(t, limiter, Search, queueFactor)

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
// keeps three and a sixteen-processor server is not held to that box's three, and it stops at the
// read class's cap over the four lanes, so the heavy reads together never exceed the reads a
// process admits.
func TestALaneIsOneRequestWideAProcessorUpToTheReadClassesShare(t *testing.T) {
	for _, c := range []struct{ procs, want int }{{1, 1}, {3, 3}, {7, 7}, {16, MaxHeavyReadLane}} {
		if got := heavyReadLaneLimit(c.procs); got != c.want {
			t.Errorf("heavyReadLaneLimit(%d) = %d, want %d", c.procs, got, c.want)
		}
	}
	if lanes := len(HeavyReadLanes()); lanes*MaxHeavyReadLane > MaxReadLimit {
		t.Errorf("%d lanes of %d hold more than the read class's %d", lanes, MaxHeavyReadLane, MaxReadLimit)
	}
}
