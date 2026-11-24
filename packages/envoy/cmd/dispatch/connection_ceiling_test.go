package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/testlimits"
)

// Every Dispatch process a test starts against one Postgres may open store.ConnectionsPerProcess
// connections, so n processes running at once need n * store.ConnectionsPerProcess beside the
// slots Postgres reserves for superusers. A server that allows fewer does not fail where the
// arithmetic is wrong. It fails later and elsewhere: a room that cannot take a connection (503
// DOC_SERVICE_UNAVAILABLE), a socket dropped mid-read, a lease that cannot be taken, or a process
// that exits before it is healthy. Each has `FATAL: remaining connection slots are reserved for
// roles with the SUPERUSER attribute` (SQLSTATE 53300) in its own log, and nothing in the failure
// names the cause.
//
// So every start checks two things, against constants rather than numbers written here. It refuses
// a start that would run more processes at once than testlimits.MaxProcessesAtOnce, which every CI
// lane's server is sized from. And it refuses a start the server it meets cannot serve. The fix is
// the server's size, never the pools: pools shrunk for tests would test a shape production does
// not run.
//
// The check reads max_connections, not the slots in use: a server whose slots other clients hold
// (this binary's own pools, another test binary, any other program) passes it and refuses the
// process later. A process that exits before it is healthy on such a refusal fails its test
// naming the server's max_connections and the client backends open on it
// (connectionRefusalCause).
//
// dispatchProcessesRunning is the count of Dispatch processes running in this test binary. It is
// one count for the whole binary because one `go test` binary is what one lane's server serves,
// and parallel root tests in it share that server. A process counts from its start until it exits,
// which is what it holds connections for, so a row that restarts a process one after another asks
// for one process's worth.
var dispatchProcessesRunning atomic.Int64

// serverSlots holds one slot for each Dispatch server running in this test binary, at most
// testlimits.MaxProcessesAtOnce: the most the lane's Postgres is sized to serve at once, which
// requireConnectionCeiling refuses a start past. A start past it waits here for a server to exit
// instead, so tests that run at the same time share the servers the lane can serve.
var serverSlots = &slotPool{size: testlimits.MaxProcessesAtOnce, free: testlimits.MaxProcessesAtOnce}

// slotPool hands out slots first come, first served: a taker that must wait queues, and each freed
// slot goes to the oldest taker, whole, once enough are free for it. A taker of two therefore waits
// only for the takers ahead of it, where with a channel each freed slot went to whichever taker of
// one won it, and a taker of two holding one of its two could wait out the whole run.
//
// paused counts the slots held by tests paused at t.Parallel with a process running
// (parallelHoldingItsProcesses). Those come back only once the serial tests have all run and the
// paused test has its turn among the parallel ones, so a take that could need one of them could
// wait on a test that waits on it. The pool refuses such a take, and a pause that would leave a
// queued take such a one, by name and at once (take, pause): every take it lets wait needs no
// more than the slots no paused test holds, and each of those is free or held by a running test
// that gives it back without waiting for another, so the oldest taker always gets its slots.
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

// takeServerSlot waits for a slot of serverSlots and answers the call that gives it back, which
// the test's cleanup also runs, for a start that never ran a process; only the first call counts.
// A test holds a slot for each of its servers that is running, so a test that waits for one while
// it holds another (TestTheHeaviestStoredDocumentsStayWithinTheMemoryBound's reader beside its
// writer) waits for a server of another test, which never waits for a slot while it holds one:
// every other test stops its server before it starts the next.
func takeServerSlot(t *testing.T) (release func()) {
	t.Helper()
	if err := serverSlots.take(1); err != nil {
		t.Fatalf("a Dispatch server cannot start: %v", err)
	}
	var once sync.Once
	release = func() { once.Do(func() { serverSlots.give(1) }) }
	t.Cleanup(release)
	return release
}

// rootSlots are the slots of serverSlots one root test's Dispatch processes hold, its subtests'
// with its own: reserved, taken all at once by reserveProcessSlots and held until the test ends, of
// which inUse serve a running process; and held, each taken by one start (takeProcessSlot) and given
// back when its process exits.
type rootSlots struct{ reserved, inUse, held int }

// slotsByRoot is every root test's rootSlots, by root test name.
var slotsByRoot = struct {
	sync.Mutex
	byRoot map[string]*rootSlots
}{byRoot: map[string]*rootSlots{}}

// slotsOf is root's rootSlots, made at its first use; the caller holds slotsByRoot.
func slotsOf(root string) *rootSlots {
	slots := slotsByRoot.byRoot[root]
	if slots == nil {
		slots = &rootSlots{}
		slotsByRoot.byRoot[root] = slots
	}
	return slots
}

// reserveProcessSlots takes n slots of serverSlots for the root test of t, all of them before it
// returns, and holds them until that test ends (less any parallelHoldingItsProcesses gives back). A
// test that runs n Dispatch processes at once calls it before its first start, and each start then
// takes a reserved slot without waiting (takeProcessSlot). It takes the n at once, in its turn,
// holding none while it waits, and the pool lets it wait only for slots that a running test will
// give back without waiting for another (slotPool), so no mix of parallel and serial tests, each
// needing up to MaxProcessesAtOnce processes, deadlocks: a take or a pause that could is refused
// by name.
func reserveProcessSlots(t *testing.T, n int) {
	t.Helper()
	if n > testlimits.MaxProcessesAtOnce {
		t.Fatalf("this test runs %d Dispatch processes at once, and testlimits.MaxProcessesAtOnce is %d. Every CI "+
			"lane's Postgres is sized from that constant: raise it, so the lanes grow with it.", n, testlimits.MaxProcessesAtOnce)
	}
	root, _, _ := strings.Cut(t.Name(), "/")
	slotsByRoot.Lock()
	slots := slotsOf(root)
	holding := slots.reserved + slots.held
	slotsByRoot.Unlock()
	if holding > 0 {
		t.Fatalf("%s reserves process slots while it holds %d: reserve them before its first Dispatch start", root, holding)
	}
	if err := serverSlots.take(n); err != nil {
		t.Fatalf("%s cannot reserve %d process slots: %v", root, n, err)
	}
	slotsByRoot.Lock()
	slots.reserved += n
	slotsByRoot.Unlock()
	t.Cleanup(func() {
		// What the reservation still holds: parallelHoldingItsProcesses gives back a slot the test
		// no longer uses before it pauses.
		slotsByRoot.Lock()
		held := slots.reserved
		slots.reserved = 0
		slotsByRoot.Unlock()
		serverSlots.give(held)
	})
}

// parallelHoldingItsProcesses calls t.Parallel for a test whose Dispatch processes keep running
// across the pause, so a product timer they wait out (a claim lapse, a sweep interval) passes
// while the serial tests run rather than after. Before it pauses it gives back each reserved slot
// no process uses, and counts the rest as held across the pause (slotPool.pause), which refuses
// by name a pause that a queued start could then wait on. It fails by name when the pool would.
func parallelHoldingItsProcesses(t *testing.T) {
	t.Helper()
	root, _, _ := strings.Cut(t.Name(), "/")
	slotsByRoot.Lock()
	slots := slotsOf(root)
	idle := slots.reserved - slots.inUse
	slots.reserved -= idle
	holding := slots.held + slots.inUse
	slotsByRoot.Unlock()
	serverSlots.give(idle)
	if err := serverSlots.pause(holding); err != nil {
		t.Fatalf("%s cannot pause at t.Parallel holding %d process slots: %v", root, holding, err)
	}
	t.Parallel()
	serverSlots.resume(holding)
}

// takeProcessSlot is the slot of serverSlots a Dispatch process of startDispatchProcessWith holds
// from its start until it exits, and answers the call that gives it back (cleanup runs it too). A
// start of a test that reserved its slots (reserveProcessSlots) takes a free one of those without
// waiting. A start of any other test waits for a slot of the pool while it holds none, so the
// package's parallel tests share the servers the lane can serve, as the memory tests do
// (takeServerSlot); a second start while its first process runs fails by name, since waiting for a
// slot while holding one is how every slot ends up held by tests waiting for another: a test that
// runs two processes at once reserves them.
func takeProcessSlot(t *testing.T) (release func()) {
	t.Helper()
	root, _, _ := strings.Cut(t.Name(), "/")
	slotsByRoot.Lock()
	slots := slotsOf(root)
	switch {
	case slots.inUse < slots.reserved:
		slots.inUse++
		slotsByRoot.Unlock()
		var once sync.Once
		release = func() {
			once.Do(func() {
				slotsByRoot.Lock()
				slots.inUse--
				slotsByRoot.Unlock()
			})
		}
		t.Cleanup(release)
		return release
	case slots.reserved > 0 || slots.held > 0:
		held := slots.reserved + slots.held
		slotsByRoot.Unlock()
		t.Fatalf("this start would run one more Dispatch process in %s beside the %d it holds slots for: a test "+
			"that runs more than one process at once reserves them before its first start "+
			"(reserveProcessSlots(t, n)), up to testlimits.MaxProcessesAtOnce (%d)", root, held, testlimits.MaxProcessesAtOnce)
		return nil
	}
	slotsByRoot.Unlock()
	if err := serverSlots.take(1); err != nil {
		t.Fatalf("%s cannot start a Dispatch process: %v", root, err)
	}
	slotsByRoot.Lock()
	slots.held++
	slotsByRoot.Unlock()
	var once sync.Once
	release = func() {
		once.Do(func() {
			slotsByRoot.Lock()
			slots.held--
			slotsByRoot.Unlock()
			serverSlots.give(1)
		})
	}
	t.Cleanup(release)
	return release
}

// requireConnectionCeiling counts one more Dispatch process running in this test binary and
// refuses its start when the server cannot serve that many. Every start takes a slot of
// serverSlots first (takeProcessSlot, takeServerSlot), so the count never passes
// testlimits.MaxProcessesAtOnce. It answers the release that takes the process off the count,
// which the caller runs once the process has exited; the test's cleanup runs it too, for a start
// that never ran a process, and only the first call counts.
func requireConnectionCeiling(t *testing.T, databaseURL string) (release func()) {
	t.Helper()
	processes := int(dispatchProcessesRunning.Add(1))
	var once sync.Once
	release = func() { once.Do(func() { dispatchProcessesRunning.Add(-1) }) }
	t.Cleanup(release)

	ctx := context.Background()
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("read the test database's URL: %v", err)
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect to the test database to read its connection ceiling: %v", err)
	}
	defer conn.Close(ctx)
	allowed := serverInt(t, conn, "max_connections")
	reserved := serverInt(t, conn, "superuser_reserved_connections")
	opened := processes * store.ConnectionsPerProcess
	need := opened + reserved
	if allowed >= need {
		return release
	}
	release()
	// The fix names the size the whole suite needs, not this start's minimum: a server raised to
	// the minimum for one process refuses the next row that runs more.
	suite := testlimits.ServerConnections + reserved
	t.Fatalf("this start would run %d Dispatch process(es) at once in this test binary, which may open %d "+
		"connections (store.ConnectionsPerProcess=%d each), against a Postgres that allows %d with %d of "+
		"them reserved for superusers. The server must allow at least %d for this start, and %d for the "+
		"whole suite (testlimits.ServerConnections + the reserve). A smaller one does not fail here but "+
		"later, as 53300 refusals inside a room, a lease or a socket.\n"+
		"To fix it:\n"+
		"- In CI, the lane runs .github/scripts/pg-connection-ceiling.sh before its tests.\n"+
		"- On a server you own, restart it with -c max_connections=%d.\n"+
		"- In a Legion pod whose legion-testpg still starts the server at 100 connections (an image "+
		"built before legion-testpg was sized from testlimits), start a Postgres of your own on the "+
		"issue's volume with -c max_connections=%d and point DISPATCH_TEST_DATABASE_URL at it.",
		processes, opened, store.ConnectionsPerProcess, allowed, reserved, need, suite, suite, suite)
	return release
}

// tooManyConnections is how a Dispatch process's output carries a connection Postgres refused
// because the server had no slot left for it: pgx ends the error with its SQLSTATE,
// too_many_connections.
const tooManyConnections = "(SQLSTATE 53300)"

// connectionRefusalCause is the reason a Dispatch process that exited before it was healthy gives
// when refused, a line of its output, says Postgres turned it away for too many clients. It quotes
// the line and reads, through databaseURL, the server's max_connections and the client backends
// open on it now, which say whether the server is too small or other clients hold its slots. The
// process has exited, so the count holds none of its connections once Postgres has ended their
// backends.
func connectionRefusalCause(refused, databaseURL string) string {
	reason := "Postgres refused it a connection for too many clients (SQLSTATE 53300):\n" + refused + "\n"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var allowed, open int
	conn, err := pgx.Connect(ctx, databaseURL)
	if err == nil {
		defer conn.Close(ctx)
		err = conn.QueryRow(ctx, `
			select current_setting('max_connections')::int, count(*)
			from pg_stat_activity
			where backend_type = 'client backend' and pid <> pg_backend_pid()`).Scan(&allowed, &open)
	}
	if err != nil {
		return reason + fmt.Sprintf("Reading the server's max_connections and its open client backends failed too: %v", err)
	}
	return reason + fmt.Sprintf("The server allows max_connections=%d and has %d client backends open now "+
		"(pg_stat_activity), while %d other Dispatch process(es) of this test binary run, each of which may "+
		"open %d (store.ConnectionsPerProcess). The connection ceiling check counts only those processes; "+
		"the rest are this binary's own pools and the server's other clients. Free the server's "+
		"connections, or raise max_connections.",
		allowed, open, dispatchProcessesRunning.Load(), store.ConnectionsPerProcess)
}

// serverInt is one integer setting of the server this test's database is on.
func serverInt(t *testing.T, conn *pgx.Conn, name string) int {
	t.Helper()
	var text string
	if err := conn.QueryRow(context.Background(), "select current_setting($1)", name).Scan(&text); err != nil {
		t.Fatalf("read the server's %s: %v", name, err)
	}
	value, err := strconv.Atoi(text)
	if err != nil {
		t.Fatalf("read the server's %s as a number: %q: %v", name, text, err)
	}
	return value
}
