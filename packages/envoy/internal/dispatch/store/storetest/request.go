package storetest

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// Request is the request store of database, the test's own database (Open): a store whose Pool is
// a request pool (store.OpenRequestPool) beside database's pool, closed after t ends. Its
// statements run as dispatch_app under the viewer the context carries (store.WithViewer), as
// api.Deps.Store's do in production, and database stays the system store (plan D1).
func Request(t testing.TB, database *store.Store) *store.Store {
	t.Helper()
	pool, err := store.OpenRequestPool(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("storetest.Request: open the request pool beside the test's database: %v", err)
	}
	testcleanup.Add(t, pool.Close)
	return &store.Store{Pool: pool}
}

// RequestAsOwner is Request with no role: a request store that runs the same wrapper and sends
// the same viewer settings, but as the owner, so no policy is met, which is main's behaviour. The
// baseline server of TestAllPublicDeploymentUnchanged runs on it (plan D1).
func RequestAsOwner(t testing.TB, database *store.Store) *store.Store {
	t.Helper()
	pool, err := store.OpenRequestPoolAsOwnerForTest(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("storetest.RequestAsOwner: open the owner's request pool beside the test's database: %v", err)
	}
	testcleanup.Add(t, pool.Close)
	return &store.Store{Pool: pool}
}
