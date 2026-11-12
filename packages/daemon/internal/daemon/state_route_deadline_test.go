package daemon

// The state route's own bound (INFRA-3169 R3), over a real pool on a real database: a read that
// does not finish ends at the route's deadline instead of holding its transaction for as long as
// the client holds its socket, and the connection it was holding is back in the pool afterwards.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// heldState is a state projection that does not finish while it is held: its read waits for the
// context it was given to end and reports that ending, which is what the daemon's own projection
// does when a query of it is cut short, and what the production failure looked like from the
// route — a read inside an open transaction that never came back. Released, it answers an empty
// state, which is all the second read of the test needs. api.StateSource is an interface, so the
// route takes this in the daemon's own source's place (daemon.go's source).
type heldState struct {
	held atomic.Bool
}

func (s *heldState) State(ctx context.Context, _ pgx.Tx) (api.State, []supervise.Claim, error) {
	if !s.held.Load() {
		return api.State{}, nil, nil
	}
	<-ctx.Done()
	return api.State{}, nil, ctx.Err()
}

// backendIdleInTransaction and backendWaitingOnALock are the two conditions this package's tests
// count the daemon's backends under: a backend inside an open transaction with no statement
// running, which is the state the production failure left all 32 production connections in
// (store's idleBound), and a backend waiting on a lock, which for a pool of one connection is a
// pool with no free connection — a read the daemon makes from then on waits for the test's
// transaction to end (readyz_test.go).
const (
	backendIdleInTransaction = "state = 'idle in transaction'"
	backendWaitingOnALock    = "wait_event_type = 'Lock'"
)

// daemonBackends counts the backends of application that condition holds of now, read on the
// test's own connection. It selects on the application name alone, the way this package's other
// census of the daemon's backends does (terminateDaemonBackends): the name is the test's, where
// the database and the login are shared with every other test running against the same
// PostgreSQL.
func daemonBackends(t *testing.T, pool *store.Pool, application, condition string) int {
	t.Helper()
	var count int
	err := pool.QueryRow(context.Background(),
		"select count(*) from pg_stat_activity where application_name = $1 and "+condition,
		application,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count the backends of %s that are %s: %v", application, condition, err)
	}
	return count
}

// A state read that does not finish is ended by the route's own deadline: the client is answered
// a failure while it is still connected, the transaction is ended rather than left open, and the
// connection goes back to the pool for the next caller.
//
// Without the route's deadline this same test hangs: the only thing that would end the read is
// the client giving up, so nothing answers and the test fails when its client runs out of
// patience. That is the measured failure — 32 connections of the production pool held by reads
// nobody ended — in one request.
func TestTheStateRouteEndsItsTransactionAtItsDeadline(t *testing.T) {
	// The route's bound as this test sets it (api.Options.StateReadBound), seconds rather than
	// the daemon's own 60, which is what api's default pins. Nothing in this test is slow but
	// the hold, so two seconds cuts nothing legitimate here.
	const bound = 2 * time.Second
	// The client waits far longer than that, because what this measures is the route answering
	// of its own accord: a client that gave up first would prove nothing at all.
	const clientPatience = 20 * time.Second

	// A database of this test's own and a pool of its own on it, to watch the daemon's
	// connections from outside; the daemon's store is opened on that database with one
	// connection, the application name the census selects on, and a health check that never
	// replaces that connection behind the test's back (daemonDSN).
	watcher := isolatedOutboxPool(t)
	application := "legion-daemon-" + strings.ToLower(randomSuffix(t))
	st, err := store.Open(context.Background(), daemonDSN(t, watcher, application))
	if err != nil {
		t.Fatalf("open the daemon's store: %v", err)
	}
	t.Cleanup(st.Close)

	state := &heldState{}
	state.held.Store(true)
	handler := api.NewServer("127.0.0.1", 0, api.Options{
		State: state, StateTransactions: st, StateReadBound: bound, Log: quietLogger(),
	}).Handler
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := &http.Client{Timeout: clientPatience}

	started := time.Now()
	response, err := client.Get(server.URL + "/legion/v1/state")
	if err != nil {
		t.Fatalf("the held state read did not answer within the client's %s: %v", clientPatience, err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read what the held state read answered: %v", err)
	}
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("the held state read answered %d (%s), want 500 at the route's own deadline",
			response.StatusCode, strings.TrimSpace(string(body)))
	}
	if took := time.Since(started); took < bound {
		t.Fatalf("the held state read answered after %s, want it held to the route's %s bound", took, bound)
	}

	// The transaction is ended, not merely abandoned by a handler that returned: no backend of
	// the daemon's pool sits inside one.
	testwait.Eventually(t, "the state route's transaction to end", func() bool {
		return daemonBackends(t, watcher, application, backendIdleInTransaction) == 0
	})

	// And the connection is back: the daemon's pool has exactly one, so a read that is served at
	// all is served on the connection the held read gave up.
	state.held.Store(false)
	next, err := client.Get(server.URL + "/legion/v1/state")
	if err != nil {
		t.Fatalf("the read after the held one: %v", err)
	}
	served, err := io.ReadAll(next.Body)
	next.Body.Close()
	if err != nil {
		t.Fatalf("read what the next state read answered: %v", err)
	}
	if next.StatusCode != http.StatusOK {
		t.Fatalf("the read after the held one answered %d (%s), want the connection back and the state served",
			next.StatusCode, strings.TrimSpace(string(served)))
	}
}
