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

// serverSlots holds one token for each Dispatch server running in this test binary, at most
// testlimits.MaxProcessesAtOnce: the most the lane's Postgres is sized to serve at once, which
// requireConnectionCeiling refuses a start past. A start past it waits here for a server to exit
// instead, so tests that run at the same time share the servers the lane can serve.
var serverSlots = make(chan struct{}, testlimits.MaxProcessesAtOnce)

// takeServerSlot waits for a slot of serverSlots and answers the call that gives it back, which
// the test's cleanup also runs, for a start that never ran a process; only the first call counts.
// A test holds a slot for each of its servers that is running, so a test that waits for one while
// it holds another (TestTheHeaviestStoredDocumentsStayWithinTheMemoryBound's reader beside its
// writer) waits for a server of another test, which never waits for a slot while it holds one:
// every other test stops its server before it starts the next.
func takeServerSlot(t *testing.T) (release func()) {
	t.Helper()
	serverSlots <- struct{}{}
	var once sync.Once
	release = func() { once.Do(func() { <-serverSlots }) }
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

// reserving lets one reserveProcessSlots at a time take its slots.
var reserving sync.Mutex

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
// returns, and holds them until that test ends. A test that runs n Dispatch processes at once calls
// it before its first start, and each start then takes a reserved slot without waiting
// (takeProcessSlot). One reservation at a time takes slots, and every other holder of a slot is a
// test running one process, which gives it back when that process exits, so a reservation that
// waits for its last slot waits on no test that waits on it. No count of tests or slots enters
// that: any mix of parallel and serial tests, each needing up to MaxProcessesAtOnce processes,
// runs without a deadlock.
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
	reserving.Lock()
	for range n {
		serverSlots <- struct{}{}
	}
	reserving.Unlock()
	slotsByRoot.Lock()
	slots.reserved += n
	slotsByRoot.Unlock()
	t.Cleanup(func() {
		slotsByRoot.Lock()
		slots.reserved -= n
		slotsByRoot.Unlock()
		for range n {
			<-serverSlots
		}
	})
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
	serverSlots <- struct{}{}
	slotsByRoot.Lock()
	slots.held++
	slotsByRoot.Unlock()
	var once sync.Once
	release = func() {
		once.Do(func() {
			slotsByRoot.Lock()
			slots.held--
			slotsByRoot.Unlock()
			<-serverSlots
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
