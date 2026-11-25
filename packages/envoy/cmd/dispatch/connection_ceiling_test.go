package main

import (
	"context"
	"fmt"
	"strconv"
	"sync"
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
// So every start does two things, against constants rather than numbers written here. It takes one
// of the binary's testlimits.MaxProcessesAtOnce process slots, which every CI lane's server is
// sized from, waiting while parallel tests hold them all, so the binary never runs more processes
// at once than that. And it refuses a start the server it meets cannot serve. The fix is the
// server's size, never the pools: pools shrunk for tests would test a shape production does not
// run.
//
// The check reads max_connections, not the slots in use: a server whose slots other clients hold
// (this binary's own pools, another test binary, any other program) passes it and refuses the
// process later. A process that exits before it is healthy on such a refusal fails its test
// naming the server's max_connections and the client backends open on it
// (connectionRefusalCause).
//
// dispatchProcessSlots holds one token for each Dispatch process running in this test binary, so
// it has testlimits.MaxProcessesAtOnce slots, and the slots held are the processes running. It is
// one count for the whole binary because one `go test` binary is what one lane's server serves,
// and parallel root tests in it share that server. A start takes a slot before it runs its process
// and gives it back once the process has exited, which is what it holds connections for, so a row
// that restarts a process one after another asks for one process's worth. Parallel tests that each
// start a process, such as TestBootRefusesASignInDomainNoFullMemberRecords's rows, wait here for a
// slot rather than run one process more than every lane's Postgres is sized for.
var dispatchProcessSlots = make(chan struct{}, testlimits.MaxProcessesAtOnce)

// processSlotsHeld is how many of the slots each test holds, so that a test that would wait for a
// slot only it can give back is refused at once, by name, rather than waiting for itself until the
// binary's -timeout.
var processSlotsHeld = struct {
	sync.Mutex
	by map[*testing.T]int
}{by: map[*testing.T]int{}}

// requireConnectionCeiling takes one of the binary's process slots for one more Dispatch process,
// waiting while other tests hold them all, and refuses its start when this test already holds
// every slot or the server cannot serve the processes running then, which is the slots held once
// this start holds its own. It answers the release that gives the slot back, which the caller runs
// once the process has exited; the test's cleanup runs it too, for a start that never ran a
// process, and only the first call counts.
func requireConnectionCeiling(t *testing.T, databaseURL string) (release func()) {
	t.Helper()
	processSlotsHeld.Lock()
	held := processSlotsHeld.by[t]
	if held >= testlimits.MaxProcessesAtOnce {
		processSlotsHeld.Unlock()
		t.Fatalf("this start would run %d Dispatch processes at once in this one test, and "+
			"testlimits.MaxProcessesAtOnce is %d, so it would wait for a process slot only this test can "+
			"give back. Every CI lane's Postgres is sized from that constant: raise it, so the lanes grow "+
			"with it, rather than leaving this row to meet a server that cannot serve it.",
			held+1, testlimits.MaxProcessesAtOnce)
	}
	processSlotsHeld.by[t] = held + 1
	processSlotsHeld.Unlock()
	dispatchProcessSlots <- struct{}{}
	processes := len(dispatchProcessSlots)
	var once sync.Once
	release = func() {
		once.Do(func() {
			<-dispatchProcessSlots
			processSlotsHeld.Lock()
			if processSlotsHeld.by[t]--; processSlotsHeld.by[t] == 0 {
				delete(processSlotsHeld.by, t)
			}
			processSlotsHeld.Unlock()
		})
	}
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
		allowed, open, len(dispatchProcessSlots), store.ConnectionsPerProcess)
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
