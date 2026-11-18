package daemon

// The state route's own bound (INFRA-3169 R3), over a real pool on a real database: a read that
// does not finish ends at the route's deadline instead of holding its transaction for as long as
// the client holds its socket, and the connection it was holding is back in the pool afterwards.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// heldState is a state projection that does not finish while it is held: its read hands the test
// the context it was given (reading), waits for that context to end and reports that ending,
// which is what the daemon's own projection does when a query of it is cut short, and what the
// production failure looked like from the route — a read inside an open transaction that never
// came back. Released, it answers an empty state, which is all the second read of the test needs.
// api.StateSource is an interface, so the route takes this in the daemon's own source's place
// (daemon.go's source).
type heldState struct {
	held    atomic.Bool
	reading chan context.Context
}

func (s *heldState) State(ctx context.Context, _ pgx.Tx) (api.State, []supervise.Claim, error) {
	if !s.held.Load() {
		return api.State{}, nil, nil
	}
	s.reading <- ctx
	<-ctx.Done()
	return api.State{}, nil, ctx.Err()
}

// stateRouteReadBound is the daemon's own bound on a state read (api's stateReadBound), which
// the route arms on its clock as the read begins.
const stateRouteReadBound = time.Minute

// stateAnswer is what a GET of the state route answered.
type stateAnswer struct {
	status int
	body   string
	err    error
}

// getState is a GET of the state route at base on ctx.
func getState(ctx context.Context, base string) stateAnswer {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/legion/v1/state", nil)
	if err != nil {
		return stateAnswer{err: err}
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return stateAnswer{err: err}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return stateAnswer{status: response.StatusCode, body: strings.TrimSpace(string(body)), err: err}
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
// The route's bound runs on a fake clock the test moves (api.Options.Clock), at the daemon's own
// minute: the route arms it as the read begins, the held read's context is still live just short
// of it, and the fake reaching it is what answers the client. Without the route's deadline no
// bound is armed and the test fails as the read reaches the held projection; with only the
// client's to end it, the read would hold its connection as long as the client held its socket.
// That is the measured failure — 32 connections of the production pool held by reads nobody
// ended — in one request.
func TestTheStateRouteEndsItsTransactionAtItsDeadline(t *testing.T) {
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

	state := &heldState{reading: make(chan context.Context, 1)}
	state.held.Store(true)
	clk := clocktest.New(time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC))
	handler := api.NewServer("127.0.0.1", 0, api.Options{
		State: state, StateTransactions: st, Clock: clk, Log: quietLogger(),
	}).Handler
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	// The client waits on the test's own bound only, because what this proves is the route
	// answering of its own accord: a client that gave up first would prove nothing at all.
	ctx := testwait.Context(t)
	started := clk.Now()
	answered := make(chan stateAnswer, 1)
	go func() { answered <- getState(ctx, server.URL) }()
	held := testwait.Receive(t, "the state read to reach the held projection", state.reading)
	if armed, want := clk.Armed(), started.Add(stateRouteReadBound); !slices.Equal(armed, []time.Time{want}) {
		t.Fatalf("the route armed %v for the held read, want its own bound alone, at %s", armed, want)
	}
	clk.Advance(stateRouteReadBound - time.Millisecond)
	if err := held.Err(); err != nil {
		t.Fatalf("the held read's context ended %s into the read, short of the route's %s bound: %v",
			stateRouteReadBound-time.Millisecond, stateRouteReadBound, err)
	}
	clk.Advance(time.Millisecond)
	got := testwait.Receive(t, "the held state read's answer once the route's bound passed", answered)
	if got.err != nil {
		t.Fatalf("the held state read: %v", got.err)
	}
	if got.status != http.StatusInternalServerError {
		t.Fatalf("the held state read answered %d (%s), want 500 at the route's own deadline", got.status, got.body)
	}

	// The transaction is ended, not merely abandoned by a handler that returned: no backend of
	// the daemon's pool sits inside one.
	testwait.Eventually(t, "the state route's transaction to end", func() bool {
		return daemonBackends(t, watcher, application, backendIdleInTransaction) == 0
	})

	// And the connection is back: the daemon's pool has exactly one, so a read that is served at
	// all is served on the connection the held read gave up.
	state.held.Store(false)
	next := getState(ctx, server.URL)
	if next.err != nil {
		t.Fatalf("the read after the held one: %v", next.err)
	}
	if next.status != http.StatusOK {
		t.Fatalf("the read after the held one answered %d (%s), want the connection back and the state served",
			next.status, next.body)
	}
}
