package main

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/testlimits"
)

// dispatchSlots are the slots of the lane's Postgres that this test binary's Dispatch processes
// hold, one each from its start until it exits: at most testlimits.MaxProcessesAtOnce, the most
// every lane's server is sized to serve at once (requireConnectionCeiling refuses a start past it).
// startDispatchProcessWith and the memory harness take a process's slot through takeProcessSlot;
// reserveProcessSlots and parallelHoldingItsProcesses are the two other ways a test holds slots.
//
// No test waits for a slot while it holds one, and that is what ends every wait:
//   - A test's first process waits for a slot, and the test holds none while it waits.
//   - A test that runs n processes at once and calls t.Parallel reserves them before its first
//     start (reserveProcessSlots). It takes the n at once, in its turn, and holds none while it
//     waits.
//   - A process that a test starts while another of its own runs takes one of the test's reserved
//     slots. A test that runs alone (no t.Parallel in it or above it) can instead take a free slot
//     at once, because the only other holders it can meet are the paused tests below. Neither
//     waits. In a parallel test with no reservation the start is refused by name, whatever the
//     slots' state, so such a test fails on every run, not only when other tests hold the slots.
//   - A test paused at t.Parallel with a process running (parallelHoldingItsProcesses) holds its
//     slots until the serial tests have run and its turn comes again. The pool counts those slots
//     as paused and refuses at once, by name, a wait that could need one (slotPool).
//
// So each wait is for slots that a running test gives back without waiting for any, and the oldest
// waiter gets its slots as soon as enough are back: the queue drains whatever the number of tests
// and whatever -parallel is.
var dispatchSlots = newProcessSlots(testlimits.MaxProcessesAtOnce, rootTestName)

// takeProcessSlot is the slot of dispatchSlots that a Dispatch process of t holds from its start
// until it exits (processSlots.take).
func takeProcessSlot(t testing.TB) (release func()) {
	t.Helper()
	return dispatchSlots.take(t)
}

// reserveProcessSlots takes n slots of dispatchSlots for t's root test (processSlots.reserve).
func reserveProcessSlots(t testing.TB, n int) {
	t.Helper()
	dispatchSlots.reserve(t, n)
}

// parallelHoldingItsProcesses calls t.Parallel with t's processes still running
// (processSlots.pauseParallel).
func parallelHoldingItsProcesses(t *testing.T) {
	t.Helper()
	dispatchSlots.pauseParallel(t)
}

// rootTestName is the name of t's root test. A root test's subtests share its slots: a subtest
// that waited for a slot while its parent's process held another would be a test waiting for a
// slot while it holds one.
func rootTestName(t testing.TB) string {
	root, _, _ := strings.Cut(t.Name(), "/")
	return root
}

// processSlots is a slotPool and the slots each root test holds of it. Each root test is one key
// of byRoot, named by rootOf.
type processSlots struct {
	pool   *slotPool
	rootOf func(testing.TB) string
	mu     sync.Mutex
	byRoot map[string]*rootSlots
}

// rootSlots are the slots one root test holds: reserved, taken all at once by reserve and held
// until the test ends, of which inUse serve a running process; and held, each taken by one start
// and given back when its process exits.
type rootSlots struct{ reserved, inUse, held int }

func newProcessSlots(size int, rootOf func(testing.TB) string) *processSlots {
	return &processSlots{pool: &slotPool{size: size, free: size}, rootOf: rootOf, byRoot: map[string]*rootSlots{}}
}

// slotsOf is root's rootSlots, made at its first use. The caller holds s.mu.
func (s *processSlots) slotsOf(root string) *rootSlots {
	slots := s.byRoot[root]
	if slots == nil {
		slots = &rootSlots{}
		s.byRoot[root] = slots
	}
	return slots
}

// take is the slot one Dispatch process of t holds, and answers the call that gives it back. The
// test's cleanup also runs that call, for a start that never ran a process, and only the first
// call counts. A start that uses a reserved slot, and a start while another of the test's
// processes runs, never wait; only a test that holds no slot waits for one (dispatchSlots says why).
func (s *processSlots) take(t testing.TB) (release func()) {
	t.Helper()
	root := s.rootOf(t)
	s.mu.Lock()
	slots := s.slotsOf(root)
	if slots.inUse < slots.reserved {
		slots.inUse++
		s.mu.Unlock()
		return s.releaseLater(t, func() { slots.inUse-- }, 0)
	}
	running := slots.inUse + slots.held
	// Counted before the take, so that a start of this test on another goroutine meanwhile sees a
	// process of this test running.
	slots.held++
	s.mu.Unlock()
	forget := func() {
		s.mu.Lock()
		slots.held--
		s.mu.Unlock()
	}
	if running == 0 {
		if err := s.pool.take(1); err != nil {
			forget()
			t.Fatalf("%s cannot start a Dispatch process: %v", root, err)
		}
		return s.releaseLater(t, func() { slots.held-- }, 1)
	}
	if refused := requireRunsAlone(t); refused != nil {
		forget()
		t.Fatalf("%s starts a Dispatch process while %d of its own run, and other tests can run beside it (%v). "+
			"A parallel test that runs several processes at once reserves them before its first start "+
			"(reserveProcessSlots(t, n)), so that it never waits for a slot while it holds one", root, running, refused)
	}
	if !s.pool.takeFree(1) {
		forget()
		t.Fatalf("%s starts its process number %d at once, and no process slot is free: tests paused at "+
			"t.Parallel hold %d of the %d (testlimits.MaxProcessesAtOnce), and they come back only after the "+
			"serial tests", root, running+1, s.pool.pausedNow(), s.pool.size)
	}
	return s.releaseLater(t, func() { slots.held-- }, 1)
}

// releaseLater answers the call that runs forget under s.mu and gives n slots back to the pool,
// once, and has t's cleanup run it too.
func (s *processSlots) releaseLater(t testing.TB, forget func(), n int) func() {
	var once sync.Once
	release := func() {
		once.Do(func() {
			s.mu.Lock()
			forget()
			s.mu.Unlock()
			s.pool.give(n)
		})
	}
	t.Cleanup(release)
	return release
}

// reserve takes n slots for t's root test, all of them before it returns, and holds them until
// that test ends (less any pauseParallel gives back). A test that runs n Dispatch processes at once
// and calls t.Parallel calls it before its first start, and each start then takes a reserved slot
// without waiting (take). It takes the n at once, in its turn, holding none while it waits.
func (s *processSlots) reserve(t testing.TB, n int) {
	t.Helper()
	root := s.rootOf(t)
	if n > s.pool.size {
		t.Fatalf("%s runs %d Dispatch processes at once, and testlimits.MaxProcessesAtOnce is %d. Every CI "+
			"lane's Postgres is sized from that constant: raise it, so the lanes grow with it", root, n, s.pool.size)
	}
	s.mu.Lock()
	slots := s.slotsOf(root)
	holding := slots.reserved + slots.held
	s.mu.Unlock()
	if holding > 0 {
		t.Fatalf("%s reserves process slots while it holds %d: reserve them before its first Dispatch start", root, holding)
	}
	if err := s.pool.take(n); err != nil {
		t.Fatalf("%s cannot reserve %d process slots: %v", root, n, err)
	}
	s.mu.Lock()
	slots.reserved += n
	s.mu.Unlock()
	t.Cleanup(func() {
		// What the reservation still holds: pauseParallel gives back a slot the test no longer uses
		// before it pauses.
		s.mu.Lock()
		held := slots.reserved
		slots.reserved = 0
		s.mu.Unlock()
		s.pool.give(held)
	})
}

// pauseParallel calls t.Parallel for a test whose Dispatch processes keep running across the
// pause, so a product timer they wait out (a claim lapse, a sweep interval) passes while the
// serial tests run rather than after. Before it pauses it gives back each reserved slot no process
// uses, and counts the rest as held across the pause (slotPool.pause), which refuses by name a
// pause that a queued start could then wait on.
func (s *processSlots) pauseParallel(t *testing.T) {
	t.Helper()
	root := s.rootOf(t)
	s.mu.Lock()
	slots := s.slotsOf(root)
	idle := slots.reserved - slots.inUse
	slots.reserved -= idle
	holding := slots.held + slots.inUse
	s.mu.Unlock()
	s.pool.give(idle)
	if err := s.pool.pause(holding); err != nil {
		t.Fatalf("%s cannot pause at t.Parallel holding %d process slots: %v", root, holding, err)
	}
	t.Parallel()
	s.pool.resume(holding)
}

// runsAloneEnv is the variable requireRunsAlone sets for the rest of a test. Nothing reads it:
// setting it through testing's Setenv is how a test shows that no other test runs beside it. No
// Dispatch process inherits it, since startDispatchProcessWith and the memory harness write out a
// process's whole environment.
const runsAloneEnv = "CMD_DISPATCH_TEST_RUNS_PROCESSES_AT_ONCE"

// requireRunsAlone answers why t can have other tests running beside it, or nil when it cannot:
// when neither t nor a test above it called t.Parallel. The check is testing's own for a test that
// changes the process: Setenv panics in a test that is parallel or under a parallel one, and makes
// a later t.Parallel in it panic, so the answer holds for the rest of the test.
func requireRunsAlone(t testing.TB) (refused any) {
	defer func() { refused = recover() }()
	t.Setenv(runsAloneEnv, "1")
	return nil
}

// slotPool hands out slots first come, first served: a taker that must wait queues, and each freed
// slot goes to the oldest taker, whole, once enough are free for it. A taker of two therefore
// waits only for the takers ahead of it. With a channel, each freed slot went to whichever taker of
// one won it, so a taker of two holding one of its two could wait out the whole run.
//
// paused counts the slots held by tests paused at t.Parallel with a process running
// (parallelHoldingItsProcesses). Those come back only once the serial tests have all run and the
// paused test's turn comes among the parallel ones, so a take that could need one of them could
// wait on a test that waits on it. The pool refuses such a take at once, by name (take), and also a
// pause that would make a queued take such a one (pause). Every take it lets wait therefore needs
// no more than the slots no paused test holds. Each of those slots is free, or is held by a running
// test that gives it back without waiting for another (dispatchSlots), so the oldest taker always
// gets its slots.
type slotPool struct {
	mu                 sync.Mutex
	size, free, paused int
	queue              []*slotTaker
}

// slotTaker is one queued take: how many slots it needs, and the channel closed once it has them.
type slotTaker struct {
	n     int
	ready chan struct{}
}

// take waits until n slots are this caller's, behind every taker that queued before it, or answers
// at once why it cannot: n is more than the slots no paused test holds.
func (p *slotPool) take(n int) error {
	p.mu.Lock()
	if n > p.size-p.paused {
		defer p.mu.Unlock()
		return fmt.Errorf("it needs %d of the %d process slots while tests paused at t.Parallel hold %d until "+
			"the serial tests have run, so it could wait on a test that waits on it", n, p.size, p.paused)
	}
	if len(p.queue) == 0 && p.free >= n {
		p.free -= n
		p.mu.Unlock()
		return nil
	}
	taker := &slotTaker{n: n, ready: make(chan struct{})}
	p.queue = append(p.queue, taker)
	p.mu.Unlock()
	<-taker.ready
	return nil
}

// takeFree takes n slots now if n are free, and answers whether it did. It neither queues nor waits
// behind the queue. Only a test that already holds a slot calls it (processSlots.take), and such a
// test gives its slots back without waiting for any, so going ahead of the queued takers, who hold
// none, delays them but never stops them.
func (p *slotPool) takeFree(n int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.free < n {
		return false
	}
	p.free -= n
	return true
}

// give hands n slots back, to the queued takers in turn.
func (p *slotPool) give(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.free += n
	for len(p.queue) > 0 && p.free >= p.queue[0].n {
		p.free -= p.queue[0].n
		close(p.queue[0].ready)
		p.queue = p.queue[1:]
	}
}

// pause counts n held slots as held across a pause at t.Parallel, or answers why not: a queued
// take would then need more than the slots no paused test holds.
func (p *slotPool) pause(n int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, taker := range p.queue {
		if taker.n > p.size-p.paused-n {
			return fmt.Errorf("a queued start needs %d of the %d process slots, and tests paused at t.Parallel "+
				"would hold %d", taker.n, p.size, p.paused+n)
		}
	}
	p.paused += n
	return nil
}

// resume takes back pause's count of n slots, once the paused test runs again.
func (p *slotPool) resume(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused -= n
}

// pausedNow is how many slots tests paused at t.Parallel hold.
func (p *slotPool) pausedNow() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}
