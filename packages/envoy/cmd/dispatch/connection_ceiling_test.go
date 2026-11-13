package main

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/testlimits"
)

// A test that starts n Dispatch processes against one Postgres needs that server to allow what
// those processes may open: n * store.ConnectionsPerProcess, beside the slots Postgres keeps for
// superusers. A server that allows fewer does not fail where the arithmetic is wrong - it fails
// later and elsewhere, as a room that cannot take a connection (503 DOC_SERVICE_UNAVAILABLE), a
// socket dropped mid-read, a lease that cannot be taken, or a process that exits before it is
// healthy, each with `FATAL: remaining connection slots are reserved for roles with the SUPERUSER
// attribute` (SQLSTATE 53300) in its own log and nothing in the failure naming the cause.
//
// So every start checks it, against the constant rather than against a number written here: the
// ceiling has climbed 105, 108 and 112 as pools were added, and a lane configured for one of
// those stays silent until a row fails for a reason that reads like something else. The refusal
// names the arithmetic and what to change, which is the lane's own server rather than the pools:
// shrinking the pools for tests would test a shape production does not run.
//
// processesRunning counts the processes one test has running, keyed by the root test's name, so a
// subtest's processes count against its parent's total: they share the server. A process is
// counted from its start until it ends, which is what it holds connections for - a row that
// restarts a process one after another needs one server's worth, not one per start.
var processesRunning sync.Map

// requireConnectionCeiling refuses the start of a process the lane's Postgres cannot serve beside
// the ones this test is already running, and answers the release the caller runs when this
// process ends.
func requireConnectionCeiling(t *testing.T, databaseURL string) (release func()) {
	t.Helper()
	root, _, _ := strings.Cut(t.Name(), "/")
	counter, _ := processesRunning.LoadOrStore(root, &atomic.Int64{})
	processes := counter.(*atomic.Int64).Add(1)
	var once sync.Once
	release = func() { once.Do(func() { counter.(*atomic.Int64).Add(-1) }) }
	t.Cleanup(release)

	if int(processes) > testlimits.MaxProcessesPerTest {
		t.Fatalf("this test starts %d Dispatch processes against one Postgres and testlimits."+
			"MaxProcessesPerTest is %d, which is what every CI lane's server is sized from: raise that "+
			"constant, so the lanes grow with it, rather than leaving this row to meet a server that "+
			"cannot serve it.", processes, testlimits.MaxProcessesPerTest)
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
	need := int(processes)*store.ConnectionsPerProcess + reserved
	if allowed >= need {
		return release
	}
	t.Fatalf("this test starts %d Dispatch process(es) against a Postgres that allows %d connections "+
		"with %d reserved for superusers, and %d process(es) may open %d (store.ConnectionsPerProcess=%d): "+
		"raise the server's max_connections to at least %d. A smaller server does not fail here but later, "+
		"as 53300 refusals inside a room, a lease or a socket.",
		processes, allowed, reserved, processes, int(processes)*store.ConnectionsPerProcess,
		store.ConnectionsPerProcess, need)
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
