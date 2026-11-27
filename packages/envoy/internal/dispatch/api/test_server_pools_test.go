package api

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A test helper that opens a request pool of its own (newTestServer's requestPoolSize and
// acquireBound, writerGateHandler's maxConns) replaces the store's Pool with one of its own. Every
// pool that Pool opens on demand - the background loops', the live readers', the rooms' and the
// rest - must close when the test ends, as storetest's own Pool does: a pool left open keeps its
// connections to the test's database, whose drop then fails on any login without
// pg_signal_backend, and the whole package reports FAIL after every test passed.
func TestATestHelperWithItsOwnRequestPoolClosesEveryPoolItOpened(t *testing.T) {
	t.Parallel()
	helpers := map[string]func(t *testing.T) *store.Store{
		"newTestServer": func(t *testing.T) *store.Store {
			_, database, _ := newTestServer(t, testServerOptions{requestPoolSize: 4})
			return database
		},
		"writerGateHandler": func(t *testing.T) *store.Store {
			_, _, database := writerGateHandler(t, 8, nil, nil)
			return database
		},
	}
	for name, open := range helpers {
		t.Run(name, func(t *testing.T) {
			var databaseName string
			// Registered first, so it runs after every cleanup the helper registers.
			t.Cleanup(func() { requireNoPoolConnectionLeft(t, databaseName, "dispatch-background") })
			database := open(t)
			databaseName = database.Pool.Config().ConnConfig.Database
			background, err := database.Pool.Background()
			if err != nil {
				t.Fatalf("open the background pool: %v", err)
			}
			if _, err := background.Exec(context.Background(), "select 1"); err != nil {
				t.Fatalf("use the background pool: %v", err)
			}
		})
	}
}

// requireNoPoolConnectionLeft fails t when application still holds a connection to databaseName
// after the test's own cleanups ran: a pool's closed connections leave pg_stat_activity once their
// backends end, and its one bound is the test binary's deadline (testwait.UntilPolling). It reads
// pg_stat_activity through a connection of its own to the server's maintenance database, since the
// test's pools are closed by then.
func requireNoPoolConnectionLeft(t *testing.T, databaseName, application string) {
	t.Helper()
	if databaseName == "" {
		return
	}
	admin, err := pgx.Connect(context.Background(), os.Getenv(storetest.DatabaseURLEnv))
	if err != nil {
		t.Fatalf("connect to read pg_stat_activity: %v", err)
	}
	defer admin.Close(context.Background())
	testwait.UntilPolling(t, 100*time.Millisecond, application+"'s connections to "+databaseName+" to end after the test's cleanups ran (the connections left)", func() (int, bool) {
		var open int
		if err := admin.QueryRow(context.Background(), `
			select count(*) from pg_stat_activity where datname = $1 and application_name = $2
		`, databaseName, application).Scan(&open); err != nil {
			t.Fatalf("count %s's connections: %v", application, err)
		}
		return open, open == 0
	})
}
