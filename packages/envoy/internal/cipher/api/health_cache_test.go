// This file is package api rather than api_test because it drives cachedPing, the edge's health
// check's ping, which is not exported.
package api

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestTheEdgesHealthPingsPostgresAtMostOnceASecond: the edge's GET /healthz answers from one
// Postgres ping for healthCacheFor, so a hundred checks within that second cost one ping and all
// read its result, a failure included, and a check after it pings again. A check whose own request
// ended before the ping answered reads that ping's error, which is not kept: the next check pings
// again rather than read a failure the database never had.
//
// The named revert: ping on every result (no cache), and the hundred checks cost a hundred pings.
func TestTheEdgesHealthPingsPostgresAtMostOnceASecond(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	now := start
	pings := 0
	failing := errors.New("database away")
	answer := error(nil)
	c := &cachedPing{
		ping: func(ctx context.Context) error { pings++; return answer },
		now:  func() time.Time { return now },
	}
	for i := range 100 {
		now = start.Add(time.Duration(i) * 9 * time.Millisecond)
		if err := c.result(context.Background()); err != nil {
			t.Fatalf("check %d = %v, want nil", i, err)
		}
	}
	if pings != 1 {
		t.Fatalf("100 checks within %s pinged %d times, want 1", healthCacheFor, pings)
	}

	answer = failing
	now = start.Add(healthCacheFor)
	for i := range 50 {
		if err := c.result(context.Background()); !errors.Is(err, failing) {
			t.Fatalf("check %d after the cache ran out = %v, want the new ping's failure", i, err)
		}
	}
	if pings != 2 {
		t.Fatalf("a check after %s and 49 more within it pinged %d times in all, want 2", healthCacheFor, pings)
	}

	answer = nil
	now = now.Add(healthCacheFor)
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	c.ping = func(ctx context.Context) error { pings++; return ctx.Err() }
	if err := c.result(ended); !errors.Is(err, context.Canceled) {
		t.Fatalf("a check whose request had ended = %v, want its own context's error", err)
	}
	c.ping = func(ctx context.Context) error { pings++; return answer }
	if err := c.result(context.Background()); err != nil {
		t.Fatalf("the check after one whose request ended = %v, want a new ping's nil", err)
	}
	if pings != 4 {
		t.Fatalf("pings = %d, want 4: the ended check's error must not be kept", pings)
	}
}
