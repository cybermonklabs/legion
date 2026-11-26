package testdb

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TeardownGrace and teardownUnbounded set the bound of internal/dispatch/store/storetest's Run
// (TeardownDeadline), the one test harness that waits for its databases' drops after the binary's
// tests. Nothing else ends a teardown that waits: the binary's own alarm stops when its tests
// return, and cmd/go kills the binary ("ran too long") only a minute or a tenth of its -timeout past
// that -timeout, whichever is longer (cmd/go/internal/test/test.go:836-854 at Go 1.27.3): three
// minutes under CI's -timeout 30m. So the teardown ends TeardownGrace past the -timeout, counted
// from the start of the tests, inside that kill, with the time left to print what it waited on.
// cmd/go counts its kill from just before it starts the binary, so the margin under the kill is
// less by the time the binary takes to reach its tests (its package init, and its TestMain before
// Run). A binary run with -timeout 0 has no kill to stay inside, and gets teardownUnbounded from
// the end of its tests.
const (
	TeardownGrace     = 30 * time.Second
	teardownUnbounded = 5 * time.Minute
)

// TeardownDeadline is when a test binary's teardown gives up, for a binary whose tests began at
// started: TeardownGrace past the -timeout, counted from started, and teardownUnbounded from now
// under -timeout 0.
func TeardownDeadline(started time.Time) time.Time {
	if timeoutFlag := flag.Lookup("test.timeout"); timeoutFlag != nil {
		if timeout, ok := timeoutFlag.Value.(flag.Getter).Get().(time.Duration); ok && timeout > 0 {
			return started.Add(timeout + TeardownGrace)
		}
	}
	return time.Now().Add(teardownUnbounded)
}

// WaitingOn says what the server's backends running any of statements wait on, and lists the
// server's other client backends, by pid, database, state and wait, from a pg_stat_activity read on
// a connection of its own to adminURL, bounded by waitingOnBound: the read
// internal/dispatch/store/storetest's Run makes at its bound. A backend that holds a drop is among
// them, though nothing the server shows says which. Where the read fails it says at which step, so
// an empty list says why: a login the server never answered, or a query it took the login for and
// never answered. The read takes a new connection rather than one held from before the drops: a
// connection held from before them would be held from the first test on, beside the admin pool,
// on a server whose connections the tests are sized to.
func WaitingOn(adminURL string, statements ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), waitingOnBound)
	defer cancel()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		if pgconn.Timeout(err) {
			return fmt.Sprintf("pg_stat_activity could not be read: a new connection to the server got no answer to its login within %s, so the server answers no new login and no backend can be listed (%v)", waitingOnBound, err)
		}
		return fmt.Sprintf("pg_stat_activity could not be read: a new connection to the server failed (%v)", err)
	}
	defer conn.Close(context.Background())
	unread := func(err error) string {
		if pgconn.Timeout(err) {
			return fmt.Sprintf("pg_stat_activity could not be read: the server answered this read's login but not its query within %s, so no backend can be listed (%v)", waitingOnBound, err)
		}
		return fmt.Sprintf("pg_stat_activity could not be read: %v", err)
	}
	rows, err := conn.Query(ctx, `select pid, coalesce(datname, ''), coalesce(state, ''), coalesce(wait_event_type, ''),
		coalesce(wait_event, ''), left(coalesce(query, ''), 60), coalesce(query, '') = any($1)
		from pg_stat_activity where backend_type = 'client backend' and pid <> pg_backend_pid() order by pid`, statements)
	if err != nil {
		return unread(err)
	}
	defer rows.Close()
	var drops, others []string
	for rows.Next() {
		var pid int
		var database, state, waitType, wait, query string
		var isDrop bool
		if err := rows.Scan(&pid, &database, &state, &waitType, &wait, &query, &isDrop); err != nil {
			return unread(err)
		}
		line := fmt.Sprintf("pid %d in %q, %s, waiting on %s/%s, %q", pid, database, state, waitType, wait, query)
		if isDrop {
			drops = append(drops, line)
		} else {
			others = append(others, line)
		}
	}
	if err := rows.Err(); err != nil {
		return unread(err)
	}
	return fmt.Sprintf("the drops: %s; the server's other client backends: %s", listOrNone(drops), listOrNone(others))
}

// waitingOnBound bounds WaitingOn's read of pg_stat_activity.
const waitingOnBound = 10 * time.Second

func listOrNone(lines []string) string {
	if len(lines) == 0 {
		return "none"
	}
	return strings.Join(lines, "; ")
}
