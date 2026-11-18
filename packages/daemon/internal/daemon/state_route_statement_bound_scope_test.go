package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// settingState is a state projection that reads, through the route's own transaction, the
// statement bound that transaction runs under and the backend it runs on, and answers an empty
// state.
type settingState struct {
	bound   string
	backend int
}

func (s *settingState) State(ctx context.Context, tx pgx.Tx) (api.State, []supervise.Claim, error) {
	err := tx.QueryRow(ctx, "select current_setting('statement_timeout'), pg_backend_pid()").Scan(&s.bound, &s.backend)
	return api.State{}, nil, err
}

// The state route's statement bound holds for the route's own transaction and for nothing else
// (R3, spec version 5: "statement_timeout on that transaction alone"). Inside the route's read a
// statement runs under the route's bound; the next caller of the pool, on the very connection the
// route gave back, runs under the server's default, which is no statement bound at all. A bound
// that outlived the route's transaction would stay on the pooled connection and cut whatever ran
// on it next, a migration's long DDL among them (internal/store's apply), which is why the route
// sets it for its transaction and the pool carries the idle bound instead.
func TestTheStateRoutesStatementBoundEndsWithItsTransaction(t *testing.T) {
	watcher := isolatedOutboxPool(t)
	application := "legion-daemon-" + strings.ToLower(randomSuffix(t))
	st, err := store.Open(context.Background(), daemonDSN(t, watcher, application))
	if err != nil {
		t.Fatalf("open the daemon's store: %v", err)
	}
	t.Cleanup(st.Close)

	state := &settingState{}
	server := httptest.NewServer(api.NewServer("127.0.0.1", 0, api.Options{
		State: state, StateTransactions: st, Log: quietLogger(),
	}).Handler)
	t.Cleanup(server.Close)
	got := getState(testwait.Context(t), server.URL)
	if got.err != nil {
		t.Fatalf("GET the state: %v", got.err)
	}
	if got.status != http.StatusOK {
		t.Fatalf("GET the state = %d %s, want 200", got.status, got.body)
	}
	// The route's bound is the daemon's own minute (stateRouteReadBound), which PostgreSQL shows
	// as 1min.
	if state.bound != "1min" {
		t.Fatalf("the route's own transaction runs under statement_timeout %q, want the route's bound, 1min", state.bound)
	}

	// The pool has one connection, so this read runs on the connection the route gave back.
	var after string
	var backend int
	if err := st.Pool().QueryRow(context.Background(), "select current_setting('statement_timeout'), pg_backend_pid()").Scan(&after, &backend); err != nil {
		t.Fatalf("read the statement bound after the route: %v", err)
	}
	if backend != state.backend {
		t.Fatalf("the read after the route ran on backend %d, the route on %d: the test needs the same connection", backend, state.backend)
	}
	if after != "0" {
		t.Fatalf("the connection the route gave back runs under statement_timeout %q, want the server's default 0: the route's bound outlived its transaction", after)
	}
}
