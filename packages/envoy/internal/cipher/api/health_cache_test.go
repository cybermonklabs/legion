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

// TestTheEdgesHealthWaitersLeaveWhenTheirRequestEnds: while one check's ping hangs, as it does
// when the database does, another check waits for it and leaves as soon as its own request ends,
// answering its own context's error, rather than queue behind the ping. Once the ping ends, a
// third check reads its result without a ping of its own.
//
// The named revert: hold the cache's lock while it pings, and the second check stays until the
// first ping ends, past its own deadline.
func TestTheEdgesHealthWaitersLeaveWhenTheirRequestEnds(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var pings int
	c := &cachedPing{
		ping: func(context.Context) error { pings++; close(started); <-release; return nil },
		now:  time.Now,
	}
	first := make(chan error, 1)
	go func() { first <- c.result(context.Background()) }()
	<-started

	waiting, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	left := make(chan error, 1)
	go func() { left <- c.result(waiting) }()
	select {
	case err := <-left:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the waiting check whose request ended = %v, want its own deadline", err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the waiting check stayed past its own deadline behind a ping that hangs")
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("the first check = %v, want its ping's nil", err)
	}
	if err := c.result(context.Background()); err != nil || pings != 1 {
		t.Fatalf("a check after the ping ended = %v with %d pings in all, want nil from the one ping", err, pings)
	}
}

// TestTheEdgesPersonKeyCannotCollide: the key of a person's bucket at an address names the
// login's length before it, so two different pairs whose plain joining with ":" gives one string
// (a login holding ":" from one /64, and another login from a /64 that holds the rest) get two
// buckets, and the same pair gets one.
//
// The named revert: join the login and the address with ":" alone, and the two pairs share a key.
func TestTheEdgesPersonKeyCannotCollide(t *testing.T) {
	a := edgePersonKey("bob@x.example:2001", "2400:1:2::/64")
	b := edgePersonKey("bob@x.example", "2001:2400:1:2::/64")
	if a == b {
		t.Fatalf("edgePersonKey gives one key, %q, for two different person and address pairs", a)
	}
	if edgePersonKey("bob@x.example", "2001:2400:1:2::/64") != b {
		t.Fatal("edgePersonKey gives one pair two keys")
	}
}
