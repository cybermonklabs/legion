package storetest

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// HoldTransactionIDInThePostgresDatabase opens a transaction in the test server's postgres
// database and takes a transaction id, as another service's job on the same instance, or another
// package's create or drop database, holds one. It holds no event id of any Dispatch log, so
// neither the event horizon nor a mark waits on it, while it holds every snapshot's xmin back. It
// rolls back when the test ends.
func HoldTransactionIDInThePostgresDatabase(t testing.TB) {
	t.Helper()
	config, err := pgx.ParseConfig(os.Getenv(DatabaseURLEnv))
	if err != nil {
		t.Fatalf("parse %s: %v", DatabaseURLEnv, err)
	}
	config.Database = "postgres"
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect to the test server's postgres database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	elsewhere, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the postgres database's transaction: %v", err)
	}
	t.Cleanup(func() { _ = elsewhere.Rollback(context.Background()) })
	if _, err := elsewhere.Exec(ctx, `select pg_current_xact_id()`); err != nil {
		t.Fatalf("take the postgres database's transaction id: %v", err)
	}
}
