package storetest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// SecondProcess opens a second store on the database database is already on, closed after t ends.
// Production runs two envoy-dispatch processes against one Postgres, and each holds a pool of its
// own: a test of what the two arbitrate between them through the database - the outbox's
// publisher hold (duty.Hold), a lease one of them runs a duty under (duty.Exclusive), a row both
// write - has to give each of them a pool of its own too. Two callers sharing a store share one
// process's pool and its connection limit, which is not the deployment such a test is about.
// It is store.Open of the connection string database was opened from, the second process being
// the same deployment twice: a pool of the same shape, with an identity of its own
// (store.Pool.Identity), so none of its sessions carries the first process's.
func SecondProcess(t testing.TB, database *store.Store) *store.Store {
	t.Helper()
	second, err := store.Open(context.Background(), database.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open a second process's store on the same database: %v", err)
	}
	t.Cleanup(second.Pool.Close)
	return second
}

// TracedStore opens a store of its own on the database database is already on, closed after t
// ends, whose every connection, and every connection of every pool it opens later, reports to
// tracer: the store opens each later pool from a copy of the configuration tracer is installed on
// (Traced). pgx hands tracer each pipeline too when it is also a pgx.BatchTracer.
func TracedStore(t testing.TB, database *store.Store, tracer pgx.QueryTracer) *store.Store {
	t.Helper()
	return &store.Store{Pool: Traced(t, database.Pool, tracer, nil)}
}
