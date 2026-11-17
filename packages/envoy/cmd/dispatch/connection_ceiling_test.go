package main

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

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
// dispatchProcessesRunning is the count of Dispatch processes running in this test binary. It is
// one count for the whole binary because one `go test` binary is what one lane's server serves,
// and parallel root tests in it share that server. A process counts from its start until it exits,
// which is what it holds connections for, so a row that restarts a process one after another asks
// for one process's worth.
var dispatchProcessesRunning atomic.Int64

// requireConnectionCeiling counts one more Dispatch process running in this test binary and
// refuses its start when the count is over testlimits.MaxProcessesAtOnce or the server cannot serve
// that many. It answers the release that takes the process off the count, which the caller runs
// once the process has exited; the test's cleanup runs it too, for a start that never ran a
// process, and only the first call counts.
func requireConnectionCeiling(t *testing.T, databaseURL string) (release func()) {
	t.Helper()
	processes := int(dispatchProcessesRunning.Add(1))
	var once sync.Once
	release = func() { once.Do(func() { dispatchProcessesRunning.Add(-1) }) }
	t.Cleanup(release)

	if processes > testlimits.MaxProcessesAtOnce {
		release()
		t.Fatalf("this start would run %d Dispatch processes at once in this test binary, and "+
			"testlimits.MaxProcessesAtOnce is %d. Every CI lane's Postgres is sized from that constant: "+
			"raise it, so the lanes grow with it, rather than leaving this row to meet a server that "+
			"cannot serve it.", processes, testlimits.MaxProcessesAtOnce)
	}

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
