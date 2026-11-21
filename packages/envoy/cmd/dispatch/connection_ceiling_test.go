package main

import (
	"context"
	"fmt"
	"net"
	"os/exec"
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
// connections at production's pool sizes, and one more for the test hooks' listener, beside the
// slots Postgres reserves for superusers. A server that allows fewer than the processes open does
// not fail where the arithmetic is wrong. It fails later and elsewhere: a room that cannot take a
// connection (503 DOC_SERVICE_UNAVAILABLE), a socket dropped mid-read, a lease that cannot be
// taken, or a process that exits before it is healthy. Each has `FATAL: remaining connection slots
// are reserved for roles with the SUPERUSER attribute` (SQLSTATE 53300) in its own log, and nothing
// in the failure names the cause.
//
// So every start checks two things, against constants rather than numbers written here. It refuses
// a start that would run more processes at once than testlimits.MaxProcessesAtOnce. And it fits
// the process to the server it meets (testlimits.PoolBudget). A server that allows
// testlimits.FullSizeServerConnections beyond its superuser reserve runs every process at
// production's pool sizes; every CI lane's server is sized to it
// (.github/scripts/pg-connection-ceiling.sh), so CI tests the pools production runs. A smaller
// server, such as a pod's test server (legion-testpg's, testlimits.ServerConnections, one
// connection per process short) or a Postgres at its default of 100, gives each process its share
// of the server, which the start hands the process as DISPATCH_TEST_POOL_BUDGET and the process
// scales its pools to (store.OpenWithin). A server too small for even the smallest pools refuses
// the start, naming the max_connections it needs. A row that needs production's sizes says so
// (requireProductionPools) and is refused, by name, on a server that would scale them.
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

// requireConnectionCeiling counts one more Dispatch process running in this test binary, refuses
// its start when the count is over testlimits.MaxProcessesAtOnce or the server cannot serve the
// suite's smallest pools, and answers the connection budget the process's pools scale to: zero
// where the server allows production's sizes, and otherwise the value the start sets as
// DISPATCH_TEST_POOL_BUDGET. It answers too the release that takes the process off the count,
// which the caller runs once the process has exited; the test's cleanup runs it too, for a start
// that never ran a process, and only the first call counts.
func requireConnectionCeiling(t *testing.T, databaseURL string) (release func(), budget int) {
	t.Helper()
	processes := int(dispatchProcessesRunning.Add(1))
	var once sync.Once
	release = func() { once.Do(func() { dispatchProcessesRunning.Add(-1) }) }
	t.Cleanup(release)

	if processes > testlimits.MaxProcessesAtOnce {
		release()
		t.Fatalf("this start would run %d Dispatch processes at once in this test binary, and "+
			"testlimits.MaxProcessesAtOnce is %d. Every CI lane's Postgres is sized from that constant, and "+
			"every smaller server's share for each process too: raise it, so both grow with it, rather than "+
			"leaving this row to meet a server that cannot serve it.", processes, testlimits.MaxProcessesAtOnce)
	}
	return release, serverPoolBudget(t, databaseURL)
}

// serverPoolBudget is the connection budget each Dispatch process gets on the Postgres databaseURL
// is on (testlimits.PoolBudget): zero where the server allows production's pool sizes. It refuses
// the test on a server too small for the suite's smallest pools, naming the max_connections the
// server needs.
func serverPoolBudget(t *testing.T, databaseURL string) int {
	t.Helper()
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("read the test database's URL: %v", err)
	}
	server := net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port)))
	read, ok := serverCeilings.Load(server)
	if !ok {
		read = readServerCeiling(t, config)
		serverCeilings.Store(server, read)
	}
	ceiling := read.(serverCeiling)
	budget, err := testlimits.PoolBudget(ceiling.maxConnections, ceiling.reserved)
	if err != nil {
		t.Fatalf("this test would run Dispatch processes against a Postgres too small for the suite's smallest "+
			"pools: %v. A server smaller still does not fail here but later, as 53300 refusals inside a room, a "+
			"lease or a socket.", err)
	}
	return budget
}

// serverCeiling is a Postgres server's max_connections and superuser_reserved_connections.
type serverCeiling struct {
	maxConnections, reserved int
}

// serverCeilings is each server's serverCeiling, by host and port, as serverPoolBudget first read
// it: Postgres reads both settings only as it starts, so every later start against that server is
// fitted to it without a connection of its own.
var serverCeilings sync.Map

// readServerCeiling reads the serverCeiling of the server config connects to, in one query.
func readServerCeiling(t *testing.T, config *pgx.ConnConfig) serverCeiling {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect to the test database to read its connection ceiling: %v", err)
	}
	defer conn.Close(ctx)
	var ceiling serverCeiling
	if err := conn.QueryRow(ctx, "select current_setting('max_connections')::int, current_setting('superuser_reserved_connections')::int").
		Scan(&ceiling.maxConnections, &ceiling.reserved); err != nil {
		t.Fatalf("read the server's max_connections and superuser_reserved_connections: %v", err)
	}
	return ceiling
}

// requireProductionPools refuses a row whose processes would run their pools scaled to budget, the
// budget requireConnectionCeiling answered a start or, for a row that checks before it starts any
// process, serverPoolBudget's; a budget of zero is production's sizes, and passes. why is what the
// row does that needs production's sizes. Such a row runs on a server that allows
// testlimits.FullSizeServerConnections beyond its superuser reserve, as every CI lane's does.
func requireProductionPools(t *testing.T, budget int, why string) {
	t.Helper()
	if budget == 0 {
		return
	}
	t.Fatalf("%s, so it needs production's pool sizes. The Postgres this test binary meets allows fewer than "+
		"testlimits.FullSizeServerConnections=%d beyond its superuser reserve, which scales each Dispatch process's "+
		"pools to a budget of %d connections (testlimits.PoolBudget). Run it against a server that allows "+
		"%d beyond its reserve, as every CI lane's does (.github/scripts/pg-connection-ceiling.sh).",
		why, testlimits.FullSizeServerConnections, budget, testlimits.FullSizeServerConnections)
}

// readsPoolBudget reports whether binary, an envoy-dispatch build, reads DISPATCH_TEST_POOL_BUDGET:
// whether `envoy-dispatch settings` lists it. A build from before the setting existed ignores it and
// opens its pools at its own production sizes, whatever budget its start was given. The answer of
// a run of the binary that succeeded is kept for every later start of it (budgetReaders).
func readsPoolBudget(t *testing.T, binary string) bool {
	t.Helper()
	if reads, ok := budgetReaders.Load(binary); ok {
		return reads.(bool)
	}
	command := exec.Command(binary, "settings")
	command.Dir = t.TempDir()
	command.Env = []string{"HOME=" + command.Dir}
	listed, err := command.Output()
	if err != nil {
		return false
	}
	reads := strings.Contains(string(listed), `"DISPATCH_TEST_POOL_BUDGET"`)
	budgetReaders.Store(binary, reads)
	return reads
}

// budgetReaders is readsPoolBudget's answer for each binary whose settings it listed, by path: the
// settings a build lists do not change while its file stands.
var budgetReaders sync.Map

// tooManyConnections is how a Dispatch process's output carries a connection Postgres refused
// because the server had no slot left for it: pgx ends the error with its SQLSTATE,
// too_many_connections.
const tooManyConnections = "(SQLSTATE 53300)"

// connectionRefusalCause is the reason a Dispatch process that exited before it was healthy gives
// when refused, a line of its output, says Postgres turned it away for too many clients. It quotes
// the line and reads, through databaseURL, the server's max_connections and the client backends
// open on it now, which say whether the server is too small or other clients hold its slots. The
// process has exited, so the count holds none of its connections once Postgres has ended their
// backends. budget is the connection budget the process's start gave it (requireConnectionCeiling),
// zero at production's pool sizes; every Dispatch process this binary starts on that server is
// given the same one.
func connectionRefusalCause(refused, databaseURL string, budget int) string {
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
	opens := fmt.Sprintf("%d (store.ConnectionsPerTestProcess, production's pool sizes and the test hooks' listener)",
		store.ConnectionsPerTestProcess)
	if budget > 0 {
		opens = fmt.Sprintf("%d (the DISPATCH_TEST_POOL_BUDGET its start was given on this server)", budget)
	}
	return reason + fmt.Sprintf("The server allows max_connections=%d and has %d client backends open now "+
		"(pg_stat_activity), while %d other Dispatch process(es) of this test binary run, each of which may "+
		"open %s. The connection ceiling check counts only those processes; "+
		"the rest are this binary's own pools and the server's other clients. Free the server's "+
		"connections, or raise max_connections.",
		allowed, open, dispatchProcessesRunning.Load(), opens)
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
