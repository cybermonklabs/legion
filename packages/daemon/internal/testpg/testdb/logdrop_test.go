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
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip(SkipMessage)
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	for _, name := range []string{"legion_test_0123456789abcdef", "a'quote and 100% of a name"} {
		if err := logDrop(ctx, conn, name, 1234567*time.Microsecond); err != nil {
			t.Errorf("log the drop of %q: %v", name, err)
		}
	}
}
