package githubapptest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// PassLimits moves the end of every recorded rate limit (github_rate_limits.until) into the past,
// as waiting out GitHub's wait would; the rows stay, strikes and all.
func PassLimits(t testing.TB, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update github_rate_limits set until = now() - interval '1 second'`); err != nil {
		t.Fatalf("move the recorded rate limits back: %v", err)
	}
}

// HoldLimits moves the end of each named scope's recorded rate limit (github_rate_limits.until),
// or of every recorded limit when it names none, ahead past the database's clock, for a row that
// reads a request made inside a limit a slow row may outlast before it gets there: GitHub's
// minute where its answer names no wait, or a sweep's own doubled backoff. ahead is the row's
// staged state for the database's clock, which judges the limit, and not a wait: the caller's
// package names it and says why it outlasts any run of the package. The rows stay, strikes and
// all. A named scope with no recorded limit, or no recorded limit at all when none is named,
// fails the test. The row reads the wait a limit was recorded with (RecordedWait) before this
// moves its end.
func HoldLimits(t testing.TB, database *store.Store, ahead time.Duration, scopes ...string) {
	t.Helper()
	statement, arguments := `update github_rate_limits set until = now() + make_interval(secs => $1)`, []any{ahead.Seconds()}
	if len(scopes) > 0 {
		statement, arguments = statement+` where scope = any($2)`, append(arguments, scopes)
	}
	tag, err := database.Pool.Exec(context.Background(), statement, arguments...)
	if err != nil {
		t.Fatalf("hold the recorded rate limits %v on: %v", ahead, err)
	}
	switch held := tag.RowsAffected(); {
	case len(scopes) == 0 && held == 0:
		t.Fatalf("github_rate_limits holds no recorded rate limit to hold %v on", ahead)
	case len(scopes) > 0 && held != int64(len(scopes)):
		t.Fatalf("held %d recorded rate limits %v on, want one under each of %v: a scope holds none", held, ahead, scopes)
	}
}

// RecordedWait is the wait scope's recorded rate limit was last written with (ratelimit.Store's
// Record and Strike): its end less the database's now() at that write (updated_at), which neither
// the moment of this read nor the time since moves. A scope with no recorded limit fails the
// test.
func RecordedWait(t testing.TB, database *store.Store, scope string) time.Duration {
	t.Helper()
	var micros int64
	err := database.Pool.QueryRow(context.Background(), `
		select (extract(epoch from until - updated_at) * 1000000)::bigint from github_rate_limits where scope = $1
	`, scope).Scan(&micros)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("github_rate_limits holds no recorded rate limit under %s", scope)
	}
	if err != nil {
		t.Fatalf("read the wait the rate limit under %s was recorded with: %v", scope, err)
	}
	return time.Duration(micros) * time.Microsecond
}
