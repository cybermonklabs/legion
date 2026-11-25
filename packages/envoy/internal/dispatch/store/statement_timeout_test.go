package store

import (
	"context"
	"testing"
	"time"
)

// StatementTimeoutSetting writes whole milliseconds, and 1 for a bound under a millisecond or of
// none, since 0 would turn the bound off. Go's own text of a minute or more ("1m0s") and of less
// than a millisecond ("500µs") is what Postgres refused, so the subtest sets both bounds in a
// transaction on the test database DISPATCH_TEST_DATABASE_URL names, and skips without one. The
// minute is set first and read back; the millisecond is set last and not read back, since every
// later statement of the transaction would run under it and could be ended by it.
func TestStatementTimeoutSettingWritesWholeMilliseconds(t *testing.T) {
	for bound, want := range map[time.Duration]string{
		15 * time.Second:               "set local statement_timeout = 15000",
		time.Minute:                    "set local statement_timeout = 60000",
		90 * time.Second:               "set local statement_timeout = 90000",
		2*time.Minute + 40*time.Second: "set local statement_timeout = 160000",
		500 * time.Microsecond:         "set local statement_timeout = 1",
		0:                              "set local statement_timeout = 1",
	} {
		if got := StatementTimeoutSetting(bound); got != want {
			t.Errorf("StatementTimeoutSetting(%v) = %q, want %q", bound, got, want)
		}
	}

	t.Run("Postgres takes it", func(t *testing.T) {
		ctx := context.Background()
		tx, err := openTestStore(t).Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		minute := StatementTimeoutSetting(time.Minute)
		if _, err := tx.Exec(ctx, minute); err != nil {
			t.Fatalf("%s: %v", minute, err)
		}
		var held string
		if err := tx.QueryRow(ctx, "show statement_timeout").Scan(&held); err != nil {
			t.Fatalf("show statement_timeout: %v", err)
		}
		if held != "1min" {
			t.Errorf("after %s the transaction holds statement_timeout %q, want 1min", minute, held)
		}
		under := StatementTimeoutSetting(500 * time.Microsecond)
		if _, err := tx.Exec(ctx, under); err != nil {
			t.Errorf("%s: %v", under, err)
		}
	})
}
