package main

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// A slotPool serves its takers first come, first served (processSlots.reserve rests on it): a taker
// of one that comes after a queued taker of two waits behind it even while a slot is free, and the
// taker of two gets both slots at once, as soon as both are free. With a channel of tokens the
// taker of one took the free slot, and a taker of two could wait as long as takers of one kept
// coming.
func TestASlotPoolServesItsTakersInTurn(t *testing.T) {
	t.Parallel()
	pool := &slotPool{size: 2, free: 2}
	if err := pool.take(1); err != nil {
		t.Fatal(err)
	}
	tookTwo, tookOne := make(chan struct{}), make(chan struct{})
	go func() { _ = pool.take(2); close(tookTwo) }()
	waitQueued(t, pool, 1)
	go func() { _ = pool.take(1); close(tookOne) }()
	waitQueued(t, pool, 2)
	select {
	case <-tookOne:
		t.Fatal("a taker of one took the free slot ahead of the taker of two queued before it")
	default:
	}
	pool.give(1)
	select {
	case <-tookTwo:
	case <-time.After(10 * time.Second):
		t.Fatal("the taker of two did not get its slots once both were free")
	}
	select {
	case <-tookOne:
		t.Fatal("the taker of one got a slot while the taker of two held both")
	default:
	}
	pool.give(2)
	select {
	case <-tookOne:
	case <-time.After(10 * time.Second):
		t.Fatal("the taker of one did not get its slot once the taker of two gave both back")
	}
}

// A slot held across a pause at t.Parallel comes back only after the serial tests, so the pool
// refuses at once, by name, a take that would need it, and a pause that would make a queued take
// need it (processSlots.parallelHoldingItsProcesses): a wait that could never end fails instead of hanging.
func TestASlotPoolRefusesAWaitOnASlotHeldAcrossAPause(t *testing.T) {
	t.Parallel()
	pool := &slotPool{size: 2, free: 2}
	if err := pool.take(1); err != nil {
		t.Fatal(err)
	}
	if err := pool.pause(1); err != nil {
		t.Fatalf("a pause holding one of two slots with nothing queued: %v", err)
	}
	// On a goroutine of its own and waited on for a bounded time, so a take let wait fails this
	// test by name instead of hanging it until -timeout.
	refused := make(chan error, 1)
	go func() { refused <- pool.take(2) }()
	select {
	case err := <-refused:
		if err == nil {
			t.Error("a take of both slots while one is held across a pause got them, want it refused")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a take of both slots while one is held across a pause was let wait, want it refused at once")
	}
	if err := pool.take(1); err != nil {
		t.Errorf("a take of the slot no paused test holds: %v", err)
	}
	pool.resume(1)
	pool.give(2)

	queued := &slotPool{size: 2, free: 2}
	if err := queued.take(1); err != nil {
		t.Fatal(err)
	}
	go func() { _ = queued.take(2) }()
	waitQueued(t, queued, 1)
	if err := queued.pause(1); err == nil {
		t.Error("a pause holding a slot a queued take of two needs was let through, want it refused")
	}
	queued.give(1)
}

// A process that a test starts while another of its own runs never waits for a slot
// (processSlots.take). Each row builds the state in which waiting would hang: two of four slots
// held across a pause, as the two mention tests hold theirs; one held by the test's own first
// process; and a take of two queued for the last free slot and one more. A second start that waited
// would queue behind that take, which waits for a slot only the waiting test could give back. In a
// test that runs alone, the second start takes the free slot at once. In a parallel test with no
// reservation, it is refused by name at once. In a parallel test that reserved two, it takes its
// reserved slot at once. When each row ends and its slots are back, the queued take is served.
func TestASecondStartNeverWaitsForASlot(t *testing.T) {
	// Not parallel: its first row is a test that runs alone, which requireRunsAlone confirms
	// through t.Setenv, and testing refuses that in a test under a parallel one.
	const state = "one free and a take of two queued ahead of it"
	t.Run("a test that runs alone", func(t *testing.T) {
		stopped := startOnItsOwnGoroutine(t, slotsWithATakeOfTwoQueued(t, false), state)
		if stopped != "" {
			t.Fatalf("the second start of a test that runs alone stopped (%s), want it to take the free slot", stopped)
		}
	})
	t.Run("a parallel test with no reservation", func(t *testing.T) {
		t.Parallel()
		stopped := startOnItsOwnGoroutine(t, slotsWithATakeOfTwoQueued(t, false), state)
		if !strings.Contains(stopped, "dispatchSlots.reserve") {
			t.Fatalf("the second start of a parallel test with no reservation stopped with %q, want it refused, "+
				"naming dispatchSlots.reserve", stopped)
		}
	})
	t.Run("a parallel test that reserved two", func(t *testing.T) {
		t.Parallel()
		stopped := startOnItsOwnGoroutine(t, slotsWithATakeOfTwoQueued(t, true), state)
		if stopped != "" {
			t.Fatalf("the second start of a parallel test that reserved two stopped (%s), want its reserved slot", stopped)
		}
	})
}

// slotsWithATakeOfTwoQueued is a pool of four slots of its own in TestASecondStartNeverWaitsForASlot's
// state. Two slots are held across a pause, t's first process holds a third (reserving two first
// when reserve is set), and a take of two is queued. When t ends, its own slots go back through
// its cleanups, and the queued take must then be served.
func slotsWithATakeOfTwoQueued(t *testing.T, reserve bool) *processSlots {
	t.Helper()
	slots := newProcessSlots(4)
	queued := make(chan struct{})
	// Registered first, so it runs last, once the cleanups registered below have given t's slots
	// back: the queued take waits for slots a running test gives back, and gets them.
	t.Cleanup(func() {
		select {
		case <-queued:
		case <-time.After(10 * time.Second):
			t.Error("the queued take of two never got its slots once the test's own were back")
		}
	})
	if err := slots.pool.take(2); err != nil {
		t.Fatal(err)
	}
	if err := slots.pool.pause(2); err != nil {
		t.Fatal(err)
	}
	if reserve {
		slots.reserve(t, 2)
	}
	slots.take(t)
	go func() {
		if err := slots.pool.take(2); err == nil {
			close(queued)
		}
	}()
	waitQueued(t, slots.pool, 1)
	return slots
}

// startOnItsOwnGoroutine starts t's next process on slots from a goroutine of its own, under a
// testing.TB whose Fatalf stops that goroutine as a test's does, and answers why the start was
// stopped, empty when it took a slot (whose release t's cleanup runs). It waits a bounded time, so
// a start that waits for a slot fails the test by name, saying it waited with state, instead of
// hanging it.
func startOnItsOwnGoroutine(t testing.TB, slots *processSlots, state string) (stopped string) {
	t.Helper()
	answered := make(chan string, 1)
	go func() {
		stopping := &stoppingTB{TB: t}
		defer func() { answered <- stopping.stop() }()
		slots.take(stopping)
	}()
	select {
	case stopped = <-answered:
		return stopped
	case <-time.After(10 * time.Second):
		t.Fatalf("the start waited for a slot, with %s", state)
		return ""
	}
}

// namedTB is a test as processSlots sees it under another name: everything but Name is the
// wrapped test's, its cleanups, its Setenv and its failures among them.
type namedTB struct {
	testing.TB
	name string
}

func (n namedTB) Name() string { return n.name }

// Sibling subtests are tests of their own (processSlots): parallel subtests each take their own
// first slot. With one count for a whole root test, the second sibling's first start counted the
// first sibling's process as its own and was refused (dispatch-memory job 138518824605, every row
// of TestTheHeaviestDocumentsWithAFullMarginStayWithinTheMemoryBound). Rows run one after another
// each take a slot for their own process once the row before has given its back. And a subtest
// uses the slots its parent reserved, as TestTheHeaviestStoredDocumentsStayWithinTheMemoryBound's
// rows do.
func TestASubtestHoldsTheSlotsOfTheTestsAboveIt(t *testing.T) {
	t.Parallel()
	// Sibling b starts while sibling a holds its slot, on every run. Two real parallel subtests
	// overlap only when the scheduler runs both at once, and in the package's whole run the
	// -parallel slots can all be held by tests of a minute or more, so no bound on a wait for the
	// other sibling would be both reliable and short. The siblings here are named as subtests of
	// this parallel row are, and take their slots one after the other, each held until the row ends.
	t.Run("parallel siblings", func(t *testing.T) {
		slots := newProcessSlots(2)
		for _, sibling := range []string{"a", "b"} {
			named := namedTB{TB: t, name: t.Name() + "/" + sibling}
			if stopped := startOnItsOwnGoroutine(named, slots, "two slots, the other sibling holding one"); stopped != "" {
				t.Fatalf("sibling %s's first start, its sibling holding a slot, stopped (%s), want a slot of its own", sibling, stopped)
			}
		}
	})
	t.Run("rows one after another", func(t *testing.T) {
		slots := newProcessSlots(1)
		for _, name := range []string{"first row", "second row", "third row"} {
			t.Run(name, func(t *testing.T) {
				if stopped := startOnItsOwnGoroutine(t, slots, "one slot, given back by the row before"); stopped != "" {
					t.Fatalf("a row's start stopped (%s), want the slot the row before gave back", stopped)
				}
			})
		}
	})
	t.Run("a parent's reservation", func(t *testing.T) {
		slots := newProcessSlots(2)
		slots.reserve(t, 2)
		for _, name := range []string{"first row", "second row"} {
			t.Run(name, func(t *testing.T) {
				for range 2 {
					if stopped := startOnItsOwnGoroutine(t, slots, "the parent's two reserved"); stopped != "" {
						t.Fatalf("a subtest's start stopped (%s), want a slot its parent reserved", stopped)
					}
				}
			})
		}
	})
}

// waitQueued waits a bounded time until pool has n takers queued.
func waitQueued(t *testing.T, pool *slotPool, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		pool.mu.Lock()
		got := len(pool.queue)
		pool.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pool queued %d takers, want %d", got, n)
		}
	}
}

// stoppingTB is a testing.TB whose Fatal, Fatalf and FailNow record why and stop the goroutine that
// calls them (runtime.Goexit), as a test's own do, without failing the test it wraps.
type stoppingTB struct {
	testing.TB
	mu      sync.Mutex
	stopped string
}

func (s *stoppingTB) Fatalf(format string, args ...any) {
	s.mu.Lock()
	s.stopped = fmt.Sprintf(format, args...)
	s.mu.Unlock()
	runtime.Goexit()
}

func (s *stoppingTB) Fatal(args ...any) { s.Fatalf("%s", fmt.Sprint(args...)) }

func (s *stoppingTB) FailNow() { s.Fatalf("FailNow") }

// stop is why the TB was stopped, empty when it was not.
func (s *stoppingTB) stop() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}
