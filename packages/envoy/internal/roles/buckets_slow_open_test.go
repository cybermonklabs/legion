package roles

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A bucket open slower than within's readTimeout still finishes and caches its handle: the read
// that made it answers its caller at readTimeout, while the open runs on to openBound. On main,
// handing the open the read's short context cut it at readTimeout and cached nothing, so every
// later read reopened. Here the first open is held past readTimeout; the first read answers with a
// timeout error, and the open then caches, so the next read finds the handle and answers the claim.
func TestABucketOpenSlowerThanTheReadTimeoutStillCaches(t *testing.T) {
	_, url := testnats.Start(t)
	writer := testnats.Connect(t, url)
	registry, err := store.Open(t.Context(), writer)
	if err != nil {
		t.Fatalf("open the role registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	if _, err := registry.SetRole("ses_holder", "test-machine", "reviewer", false); err != nil {
		t.Fatalf("claim reviewer: %v", err)
	}

	client, err := bus.Connect([]string{url})
	if err != nil {
		t.Fatalf("connect holder reader: %v", err)
	}
	t.Cleanup(client.Close)
	buckets := NewBuckets(client, Windows{})

	// Hold only the first open past readTimeout (2 s), and under openBound (20 s), so the open
	// answers after within has returned its caller but before openBound ends it.
	var once sync.Once
	hook := func() { once.Do(func() { time.Sleep(readTimeout + time.Second) }) }
	openBucketHook.Store(&hook)
	t.Cleanup(func() { openBucketHook.Store(nil) })

	if _, err := buckets.RoleClaim(context.Background(), "reviewer"); err == nil {
		t.Fatal("the first read, whose open is held past the read timeout, answered with no error; want the read timeout error")
	}

	// The held open runs on and caches. Wait for the handle, then read the claim through it.
	testwait.WithinPolling(t, testwait.Bounded(t, openBound, time.Second), 20*time.Millisecond,
		"the slow open to cache its handle", func() bool {
			buckets.mu.Lock()
			defer buckets.mu.Unlock()
			return buckets.roles != nil
		})
	claim, err := buckets.RoleClaim(context.Background(), "reviewer")
	if err != nil {
		t.Fatalf("read reviewer after the slow open cached its handle: %v", err)
	}
	if claim.HolderSessionID != "ses_holder" {
		t.Fatalf("read reviewer after the slow open cached its handle = holder %q, want %q", claim.HolderSessionID, "ses_holder")
	}
}
