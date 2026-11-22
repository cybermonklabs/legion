package storetest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// OneConnection opens a pool of one connection onto pool's database, with pool's settings, and
// answers it with the backend process id that connection holds. A call a test makes on it runs on
// that one backend, so a wait for the call's queue watches that backend alone (QueuedBehind,
// WaitForQueuedBehind, WaitForEachQueuedBehind, HoldsTransactionID), and no other backend queued
// behind the same lock - a document settlement or a sweep of the server's own - is taken for it.
// The pool closes when t ends, after every cleanup registered after this call. A call that needs
// two connections at once cannot run on it.
func OneConnection(t testing.TB, pool *store.Pool) (*store.Pool, uint32) {
	t.Helper()
	config := pool.Config()
	config.MaxConns = 1
	config.MinConns = 0
	config.MinIdleConns = 0
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a pool of one connection: %v", err)
	}
	one := store.NewPool(opened)
	t.Cleanup(one.Close)
	conn, err := one.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire the one connection: %v", err)
	}
	pid := conn.Conn().PgConn().PID()
	conn.Release()
	return one, pid
}
