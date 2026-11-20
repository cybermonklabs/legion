package testdb

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The line a drop's time is written with is a statement the server runs, for a name that holds a
// quote and a percent sign as well, which the RAISE's format would otherwise read as its own: an
// error here would leave every CI job's log with no drop time and its sample unknown.
func TestLogDropWritesALineTheServerAccepts(t *testing.T) {
	url := os.Getenv("DISPATCH_TEST_DATABASE_URL")
	if url == "" {
		SkipOutsideCI(t, "DISPATCH_TEST_DATABASE_URL is unset, so there is no Postgres to write the line to")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	for _, name := range []string{"dispatch_test_0123456789abcdef", "a'quote and 100% of a name"} {
		if err := LogDrop(ctx, conn, name, 1234567*time.Microsecond); err != nil {
			t.Errorf("log the drop of %q: %v", name, err)
		}
	}
}
