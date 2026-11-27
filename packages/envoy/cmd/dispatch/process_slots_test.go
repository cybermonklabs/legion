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
// every lane's server is sized to serve at once. startDispatchProcessWith and the memory harness
// take a process's slot with take; reserve and parallelHoldingItsProcesses are the two other ways
// a test holds slots.
//
// No test waits for a slot while it holds one, and that is what ends every wait. A test counts the
// slots of the tests above it as its own (processSlots):
//   - A test's first process waits for a slot, and the test holds none while it waits.
//   - A test that runs n processes at once and calls t.Parallel reserves them before its first
//     start (reserve). It takes the n at once, in its turn, and holds none while it waits.
//   - A process that a test starts while another of its own runs takes one of the test's reserved
//     slots. A test that runs alone (no t.Parallel in it or above it) can instead take a free slot
//     at once, because the only other holders it can meet are the paused tests of the next
//     bullet. Neither waits. In a parallel test with no reservation the start is refused by name,
//     whatever the slots' state, so such a test fails on every run, not only when other tests
//     hold the slots.
//   - A test paused at t.Parallel with a process running (parallelHoldingItsProcesses) holds its
//     slots until the serial tests have run and its turn comes again. The pool counts those slots
//     as paused and refuses at once, by name, a wait that could need one (slotPool).
//
// So each wait is for slots that a running test gives back without waiting for any, and the oldest
// waiter gets its slots as soon as enough are back: the queue drains whatever the number of tests
// and whatever -parallel is.
var dispatchSlots = newProcessSlots(testlimits.MaxProcessesAtOnce)

// processSlots is a slotPool and the slots each test holds of it, by test name. A test counts the
// slots of the tests above it as its own: a subtest that waited for a slot while its parent's
// process held another would be a test waiting for a slot while it holds one, and a subtest uses
// the slots its parent reserved. Sibling subtests are tests of their own, so parallel subtests
// each wait for their own first slot.
type processSlots struct {
	pool   *slotPool
	mu     sync.Mutex
	byTest map[string]*testSlots
}

// testSlots are the slots one test holds: reserved, taken all at once by reserve and held until
// the test ends, of which inUse serve a running process; and held, each taken by one start and
// given back when its process exits.
type testSlots struct{ reserved, inUse, held int }

func newProcessSlots(size int) *processSlots {
	return &processSlots{pool: &slotPool{size: size, free: size}, byTest: map[string]*testSlots{}}
}

// slotsOf is the test name's testSlots, made at its first use. The caller holds s.mu.
func (s *processSlots) slotsOf(name string) *testSlots {
	slots := s.byTest[name]
	if slots == nil {
		slots = &testSlots{}
		s.byTest[name] = slots
	}
	return slots
}

// lineOf is the testSlots of the test name and of each test above it, nearest first, of those
// with an entry, and name's own, made if it has none. The caller holds s.mu.
func (s *processSlots) lineOf(name string) []*testSlots {
	line := []*testSlots{s.slotsOf(name)}
	for parent, cut := cutLast(name); cut; parent, cut = cutLast(parent) {
		if slots := s.byTest[parent]; slots != nil {
			line = append(line, slots)
		}
	}
	return line
}

// cutLast cuts name at its last slash, as a subtest's name joins its parent's, and answers the
// parent's name.
func cutLast(name string) (parent string, cut bool) {
	at := strings.LastIndex(name, "/")
	if at < 0 {
		return name, false
	}
	return name[:at], true
}

// take is the slot one Dispatch process of t holds, and answers the call that gives it back. The
// test's cleanup also runs that call, for a start that never ran a process, and only the first
// call counts. A start that uses a reserved slot of t or a test above it, and a start while a
// process of t or a test above it runs, never wait; only a test that holds no slot waits for one
// (dispatchSlots says why).
func (s *processSlots) take(t testing.TB) (release func()) {
	t.Helper()
	name := t.Name()
	s.mu.Lock()
	line := s.lineOf(name)
	running := 0
	for _, slots := range line {
		if slots.inUse < slots.reserved {
			slots.inUse++
			s.mu.Unlock()
			return s.releaseLater(t, func() { slots.inUse-- }, 0)
		}
		running += slots.inUse + slots.held
	}
	own := line[0]
	// Counted before the pool's slot is taken, so that a start of this test on another goroutine
	// meanwhile sees a process of this test running.
	own.held++
	s.mu.Unlock()
	if err := s.poolSlot(t, running); err != nil {
		s.mu.Lock()
		own.held--
		s.mu.Unlock()
		t.Fatalf("%s cannot start a Dispatch process: %v", name, err)
	}
	return s.releaseLater(t, func() { own.held-- }, 1)
}

// poolSlot takes a slot of the pool for a process of t, which already runs running processes of
// its own or of tests above it, or answers why it cannot. A first process waits for its slot. A
// further process of a test that runs alone takes a free slot at once, and of a parallel test is
// refused: it would wait for a slot while its test holds one.
func (s *processSlots) poolSlot(t testing.TB, running int) error {
	if running == 0 {
		return s.pool.take(1)
	}
	if refused := requireRunsAlone(t); refused != nil {
		return fmt.Errorf("it starts a process while %d of its own, or of a test above it, run, and other tests can "+
			"run beside it (%v). A parallel test that runs several processes at once reserves them before its "+
			"first start (dispatchSlots.reserve(t, n)), so that it never waits for a slot while it holds one",
			running, refused)
	}
	if !s.pool.takeFree(1) {
		return fmt.Errorf("it starts its process number %d at once, and no process slot is free: tests paused at "+
			"t.Parallel hold %d of the %d (testlimits.MaxProcessesAtOnce), and they come back only after the "+
			"serial tests", running+1, s.pool.pausedNow(), s.pool.size)
	}
	return nil
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

// reserve takes n slots for t, all of them before it returns, and holds them until t ends (less
// any parallelHoldingItsProcesses gives back). A parallel test that runs n Dispatch processes at
// once calls it before its first start, and each start of it or of its subtests then takes a
// reserved slot without waiting (take). It takes the n at once, in its turn, and neither t nor a
// test above it holds a slot while it waits.
func (s *processSlots) reserve(t testing.TB, n int) {
	t.Helper()
	name := t.Name()
	if n > s.pool.size {
		t.Fatalf("%s runs %d Dispatch processes at once, and testlimits.MaxProcessesAtOnce is %d. Every CI "+
			"lane's Postgres is sized from that constant: raise it, so the lanes grow with it", name, n, s.pool.size)
	}
	s.mu.Lock()
	line := s.lineOf(name)
	holding := 0
	for _, slots := range line {
		holding += slots.reserved + slots.held
	}
	s.mu.Unlock()
	if holding > 0 {
		t.Fatalf("%s reserves process slots while it, or a test above it, holds %d: reserve them before the "+
			"first Dispatch start", name, holding)
	}
	if err := s.pool.take(n); err != nil {
		t.Fatalf("%s cannot reserve %d process slots: %v", name, n, err)
	}
	own := line[0]
	s.mu.Lock()
	own.reserved += n
	s.mu.Unlock()
	t.Cleanup(func() {
		// What the reservation still holds: parallelHoldingItsProcesses gives back a slot the test
		// no longer uses before it pauses.
		s.mu.Lock()
		held := own.reserved
		own.reserved = 0
		s.mu.Unlock()
		s.pool.give(held)
	})
}

// parallelHoldingItsProcesses calls t.Parallel for a top-level test whose Dispatch processes keep
// running across the pause, so a product timer they wait out (a claim lapse, a sweep interval)
// passes while the serial tests run rather than after. Before it pauses it gives back each reserved
// slot no process uses, and counts the rest as held across the pause (slotPool.pause), which
// refuses by name a pause that a queued start could then wait on. A subtest is refused by name: a
// top-level test's pause lasts until the serial tests have run, which is what the pool counts on,
// and a subtest's pause lasts only until its parent returns.
func (s *processSlots) parallelHoldingItsProcesses(t *testing.T) {
	t.Helper()
	name := t.Name()
	if _, sub := cutLast(name); sub {
		t.Fatalf("%s pauses at t.Parallel holding its processes, and it is a subtest: only a top-level test's "+
			"pause lasts until the serial tests have run, which is what the pool counts paused slots by", name)
	}
	s.mu.Lock()
	slots := s.slotsOf(name)
	idle := slots.reserved - slots.inUse
	slots.reserved -= idle
	holding := slots.held + slots.inUse
	s.mu.Unlock()
	s.pool.give(idle)
	if err := s.pool.pause(holding); err != nil {
		t.Fatalf("%s cannot pause at t.Parallel holding %d process slots: %v", name, holding, err)
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
