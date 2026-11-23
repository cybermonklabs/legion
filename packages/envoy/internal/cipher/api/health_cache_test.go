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
// read its result, a failure included, and a check after it pings again.
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

// TestTheEdgesHealthPingOutlivesACallerWhoLeaves: a check whose request ends while its ping runs
// does not cancel the ping, which runs on a context of its own (context.WithoutCancel, its own
// timeout): in pgx a cancelled query closes the pool's connection. The ping's result is kept, so
// the next check reads it and costs no ping of its own.
//
// The named revert: ping on the caller's context and keep the result only while that context has
// not ended, as before, and the ping sees the cancel, and the next check pings again.
func TestTheEdgesHealthPingOutlivesACallerWhoLeaves(t *testing.T) {
	release := make(chan struct{})
	started := make(chan context.Context, 1)
	var pings int
	c := &cachedPing{
		ping: func(ctx context.Context) error {
			pings++
			started <- ctx
			select {
			case <-release:
			case <-ctx.Done():
			}
			return ctx.Err()
		},
		now: time.Now,
	}
	leaving, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { first <- c.result(leaving) }()
	pingCtx := <-started
	cancel()
	if err := pingCtx.Err(); err != nil {
		close(release)
		t.Fatalf("the ping's context once its caller's request ended = %v, want it running still", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("the check whose request ended = %v, want its ping's nil", err)
	}
	if err := c.result(context.Background()); err != nil || pings != 1 {
		t.Fatalf("the check after one whose caller left = %v with %d pings in all, want nil from the one ping", err, pings)
	}
}

// TestTheEdgesHealthPingsAgainAfterAPingThatPanicked: a ping that panics leaves the cache ready
// for the next check, which pings again, rather than every later check waiting on a ping that
// will never end: run clears the running ping in a defer.
//
// The named revert: clear the running ping after the ping returns, not in a defer, and the next
// check waits until its own deadline.
func TestTheEdgesHealthPingsAgainAfterAPingThatPanicked(t *testing.T) {
	var pings int
	c := &cachedPing{
		ping: func(context.Context) error {
			pings++
			if pings == 1 {
				panic("the driver panicked")
			}
			return nil
		},
		now: time.Now,
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the first ping's panic did not reach its caller")
			}
		}()
		c.result(context.Background())
	}()
	next, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.result(next); err != nil || pings != 2 {
		t.Fatalf("the check after a ping that panicked = %v with %d pings in all, want nil from a ping of its own", err, pings)
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
