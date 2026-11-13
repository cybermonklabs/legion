package main

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// sweepBackoffStatement is the first statement of every sweep, as pg_stat_activity shows it: the
// read of the backoff row the sweep's own rate-limit scope holds (redeliver.Sweeper.readBackoff,
// ratelimit.Store.Get, whose `where scope = $1` no other reader of github_rate_limits sends).
const sweepBackoffStatement = "from github_rate_limits where scope = $1"

// sweepPoolWindow is how long the wait below gives that statement. The sweep at boot reaches it
// within a second of the process starting its loops; a sweep that found NATS not yet connected
// opens no state bucket and is skipped, and the next comes one interval later, so the window
// holds that one too.
const sweepPoolWindow = redeliver.Interval + time.Minute

// The webhook redelivery sweep is background work: a timer under a lease, sweeping at boot and
// every redeliver.Interval after it. Its statements belong on the background pool, where every
// other loop's are, so that a loop and the requests never take connections from one another - a
// sweep listing GitHub holds its connection across a page's round trip, and a request behind it
// waits that long for one. They ran on the request pool, which nothing said and nothing caught.
//
// This reads the running process rather than main's source. github_rate_limits is held under an
// access exclusive lock from before the process starts, so the sweep's first statement queues
// behind that lock, and pg_stat_activity names the connection it waits on. Every pool a Dispatch
// process opens stamps its connections with application_name dispatch-<pool> (store's
// applicationNamePrefix), so that name is the pool: pointed back at the request pool, the same
// wait answers dispatch-requests.
func TestTheWebhookRedeliverySweepReadsItsRateLimitsOnTheBackgroundPool(t *testing.T) {
	database := storetest.Open(t)
	_, natsURL := testnats.Start(t)
	github := startRedeliveryGitHub(t)
	gate := holdTableExclusively(t, database, "github_rate_limits")
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		NATSURL: natsURL,
		Env:     githubAppEnv(t, github),
	})
	application := applicationBehind(t, database, gate, process, sweepBackoffStatement, sweepPoolWindow)
	// Released as soon as the name is read, so the sweep runs on and the process stops in order.
	if err := gate.Rollback(context.Background()); err != nil {
		t.Fatalf("release github_rate_limits: %v", err)
	}
	if want := "dispatch-" + store.BackgroundPool; application != want {
		t.Fatalf("the sweep read its GitHub rate limits on a %s connection, want %s: a loop's statements belong on the background pool, not on the connections requests are served from", application, want)
	}
}

// startRedeliveryGitHub is the GitHub this row's process is given: the webhook delivery listing
// the sweep makes, empty, and GitHub's 404 for every other path. The sweep here never reaches it
// - the gate holds the sweep at a statement of its own first - but DISPATCH_GITHUB_API_BASE must
// name an origin the App JWT may be sent to, and a sweep released by a failing row must find an
// answer rather than a dead port.
func startRedeliveryGitHub(t *testing.T) string {
	t.Helper()
	return githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/hook/deliveries" {
			_, _ = io.WriteString(w, `[]`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}})
}

// holdTableExclusively holds table in access exclusive mode - the one mode a plain select waits
// on - until the test rolls the transaction back or ends.
func holdTableExclusively(t *testing.T, database *store.Store, table string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	gate, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the transaction holding %s: %v", table, err)
	}
	t.Cleanup(func() { _ = gate.Rollback(context.Background()) })
	if _, err := gate.Exec(ctx, "lock table "+table+" in access exclusive mode"); err != nil {
		t.Fatalf("hold %s: %v", table, err)
	}
	return gate
}

// behindTheGate is one backend queued behind the lock a test holds: the pid QueuedBehind is asked
// about, and the pool its connection belongs to, which is its application_name.
type behindTheGate struct {
	PID         uint32
	Application string
}

// applicationBehind waits up to within for a backend of this test's database whose current
// statement carries fragment to queue behind gate's transaction, and answers the application_name
// that backend set. The statement is what names the caller: the connections under test are the
// started process's own, which no pool of this test can give, and that process's other loops
// queue on the same lock.
func applicationBehind(
	t *testing.T, database *store.Store, gate pgx.Tx, process *dispatchProcess, fragment string, within time.Duration,
) string {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(within)
	for {
		rows, err := database.Pool.Query(ctx, `
			select pid, application_name from pg_stat_activity
			where datname = current_database() and pid <> pg_backend_pid() and position($1 in query) > 0
		`, fragment)
		if err != nil {
			t.Fatalf("read the sessions running a statement carrying %q: %v", fragment, err)
		}
		running, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (behindTheGate, error) {
			var session behindTheGate
			err := row.Scan(&session.PID, &session.Application)
			return session, err
		})
		if err != nil {
			t.Fatalf("read the sessions running a statement carrying %q: %v", fragment, err)
		}
		for _, session := range running {
			queued, err := storetest.QueuedBehind(ctx, gate, session.PID)
			if err != nil {
				t.Fatal(err)
			}
			if queued {
				return session.Application
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no statement carrying %q queued behind the held github_rate_limits within %s, so no sweep read its backoff:\n%s",
				fragment, within, process.Output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
