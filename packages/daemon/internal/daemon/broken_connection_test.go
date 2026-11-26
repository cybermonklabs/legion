package daemon

// A whole daemon over a database whose connection breaks under one claim write (INFRA-1132): a
// real pool, a real worker stream, a real claim, and a turn the agent really starts.

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A turn start whose claim write the database did not take is made at the claim's next decision,
// in a running daemon, with nothing restarted: the agent said it was working, the connection
// under that write was ended, and the row reads working all the same.
//
// This is the measured incident: the daemon's own backend ended under the write, SQLSTATE 57P01.
// The backend is ended while the write's transaction runs, waiting on a row lock this test holds
// on the claim: a backend ended before the write is no longer a lost write, since the store begins
// a bounded write on another connection when the one it is handed was closed (store.Pool.BeginTx),
// so the break this measures is one inside a transaction, which nothing can begin again for it.
// What the daemon then does is read from the row itself.
//
// On the merge base the same test leaves the claim at its older state: a write the store did not
// take was lost with the decision that failed, the claim stood working in memory over a row that
// read ready, and nothing wrote it again until something else happened to the claim. With the
// termination taken out the same test passes on the merge base too, since then no write is lost
// at all. The break is what this measures, which is why the kill is in it.
//
// The daemon's pool is given three parameters of this test's own. One connection makes the kill
// deterministic: there is exactly one backend to end, the one the write waits on. The hour-long
// health check keeps the pool from replacing that connection behind the test's back. And the
// application name is how the kill finds the daemon's backend and nothing else: the package's
// tests run under the race detector against one shared PostgreSQL, where a kill by database or by
// login would end another test's connection.
func TestATurnStartSurvivesABrokenDatabaseConnection(t *testing.T) {
	t.Parallel()
	// A database of this test's own, and a pool of its own on it: the reads below and the kill go
	// over a connection the daemon does not own and the kill does not end.
	pool := isolatedOutboxPool(t)
	cfg := testConfig(t)
	application := "legion-daemon-" + strings.ToLower(randomSuffix(t))
	cfg.PostgresDSN = daemonDSN(t, pool, application)

	rt := fake.NewRuntime()
	daemonBuilt := &built{}
	logs := &syncBuffer{}
	d := startDaemonLogging(t, cfg, fakeRuntime(rt, daemonBuilt), slog.New(slog.NewJSONHandler(logs, nil)))

	// One claim, driven to a turn start as a worker drives it: the operator spawns it, its shim
	// connects over the worker stream, its agent registers and says it is ready.
	spawn := architect()
	token := d.spawn(spawn)
	launch := lastLaunch(t, rt, token)
	admitIssue(t, cfg, spawn.Issue, claim.RoleArchitect, token)
	sh := dialShim(t, daemonBuilt.address, launch.BootToken)
	testwait.Eventually(t, "the hello to reach the machine", func() bool {
		return storedClaimState(t, pool, token) == supervise.StateShimConnected
	})
	registerReady(t, d, launch)
	testwait.Eventually(t, "the ready claim to reach its row", func() bool {
		return storedClaimState(t, pool, token) == supervise.StateReady
	})

	// The test holds the claim's row, so the turn start's write waits on it inside its
	// transaction, and the daemon's one backend is ended, by its application name alone, while it
	// waits there.
	ctx := context.Background()
	holder, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin the transaction that holds the claim's row: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "select 1 from claims where token = $1 for update", string(token)); err != nil {
		t.Fatalf("hold the claim's row: %v", err)
	}

	// The agent starts a turn. The claim stands working in the daemon's memory; its write waits on
	// the row, its connection is ended under it, and the row is behind what memory holds.
	sh.send(shimwire.AgentStart{})
	testwait.Eventually(t, "the turn start's claim write to wait on the claim's row", func() bool {
		return daemonBackends(t, pool, application, backendWaitingOnALock) == 1
	})
	if ended := terminateDaemonBackends(t, pool, application); ended != 1 {
		t.Fatalf("ended %d backends of %s, want the daemon's one connection", ended, application)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("let the claim's row go: %v", err)
	}
	testwait.Eventually(t, "the turn start's claim write to fail on the broken connection", func() bool {
		return strings.Contains(logs.String(), loggedMessage(owedWriteFailedLine))
	})
	if got := storedClaimState(t, pool, token); got != supervise.StateReady {
		t.Fatalf("the row reads %s straight after the broken write, want the ready row it left", got)
	}

	// The claim's next decision makes the write the database did not take. A second agent_start is
	// a turn already in flight, which the table ignores — the repair at the head of the decision is
	// the whole of what this drives.
	sh.send(shimwire.AgentStart{})
	testwait.Eventually(t, "the claim to read working in the database", func() bool {
		return storedClaimState(t, pool, token) == supervise.StateWorking
	})
	// The line is waited for rather than read once: the row and the line are written by the one
	// decision but not in one step, so a loaded machine can serve the working row to the wait
	// above before the line reaches the log (measured on 2026-11-14, under another Go suite of
	// this pod, where this read failed at 0.59s and the line stood in the log it printed).
	testwait.Eventually(t, "the daemon to log the claim write it made for the broken connection", func() bool {
		return strings.Contains(logs.String(), loggedMessage(owedWriteMadeLine))
	})

	// No daemon was restarted: the one boot this test started is the one that wrote the row.
	if count, _ := boots(t, cfg); count != 1 {
		t.Fatalf("the project records %d boots, want the one: the claim caught up with no restart", count)
	}
}

// daemonDSN is the database pool holds, as the daemon under test is given it: the test's own
// database, with the application name the kill selects on, one connection, and a health check far
// enough away that the pool never replaces that connection on its own.
func daemonDSN(t *testing.T, pool *store.Pool, application string) string {
	t.Helper()
	dsn, err := url.Parse(pool.Config().ConnString())
	if err != nil {
		// url.Parse's error, and the error it wraps, can quote the URL's password, so none of it is printed.
		t.Fatal("the test database's DSN does not parse")
	}
	query := dsn.Query()
	query.Set("application_name", application)
	query.Set("pool_max_conns", "1")
	query.Set("pool_health_check_period", "1h")
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

// terminateDaemonBackends ends every backend whose application name is application, on the test's
// own connection, and answers how many it ended. It selects on that name and on nothing else: the
// name is this test's alone, and a kill by database or by login would end the connections of
// whatever else runs against the same PostgreSQL.
func terminateDaemonBackends(t *testing.T, pool *store.Pool, application string) int {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"select pg_terminate_backend(pid) from pg_stat_activity where application_name = $1", application)
	if err != nil {
		t.Fatalf("end the backends of %s: %v", application, err)
	}
	ended, err := pgx.CollectRows(rows, pgx.RowTo[bool])
	if err != nil {
		t.Fatalf("read what ending the backends of %s answered: %v", application, err)
	}
	count := 0
	for _, terminated := range ended {
		if terminated {
			count++
		}
	}
	return count
}

// storedClaimState is token's state as the database holds it, read on the test's own connection:
// what the daemon's memory holds is no part of this test's question.
func storedClaimState(t *testing.T, pool *store.Pool, token claim.Token) supervise.ClaimState {
	t.Helper()
	var state supervise.ClaimState
	if err := pool.QueryRow(context.Background(), "select state from claims where token = $1", string(token)).Scan(&state); err != nil {
		t.Fatalf("read the stored claim of %s: %v", token, err)
	}
	return state
}
