package main

import (
	"testing"
	"time"
)

// A slotPool serves its takers first come, first served (reserveProcessSlots rests on it): a taker
// of one that comes after a queued taker of two waits behind it even while a slot is free, and the
// taker of two gets both slots at once, as soon as both are free. With a channel of tokens the
// taker of one took the free slot, and a taker of two could wait as long as takers of one kept
// coming.
func TestASlotPoolServesItsTakersInTurn(t *testing.T) {
	t.Parallel()
	pool := &slotPool{size: 2, free: 2}
	if err := pool.take(1); err != nil {
		t.Fatal(err)
	}
	queued := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
			pool.mu.Lock()
			got := len(pool.queue)
			pool.mu.Unlock()
			if got == n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the pool queued %d takers, want %d", got, n)
			}
		}
	}
	tookTwo, tookOne := make(chan struct{}), make(chan struct{})
	go func() { _ = pool.take(2); close(tookTwo) }()
	queued(1)
	go func() { _ = pool.take(1); close(tookOne) }()
	queued(2)
	select {
	case <-tookOne:
		t.Fatal("a taker of one took the free slot ahead of the taker of two queued before it")
	default:
	}
	pool.give(1)
	select {
	case <-tookTwo:
	case <-time.After(10 * time.Second):
		t.Fatal("the taker of two did not get its slots once both were free")
	}
	select {
	case <-tookOne:
		t.Fatal("the taker of one got a slot while the taker of two held both")
	default:
	}
	pool.give(2)
	select {
	case <-tookOne:
	case <-time.After(10 * time.Second):
		t.Fatal("the taker of one did not get its slot once the taker of two gave both back")
	}
}

// A slot held across a pause at t.Parallel comes back only after the serial tests, so the pool
// refuses at once, by name, a take that would need it, and a pause that would make a queued take
// need it (parallelHoldingItsProcesses): a wait that could never end fails instead of hanging.
func TestASlotPoolRefusesAWaitOnASlotHeldAcrossAPause(t *testing.T) {
	t.Parallel()
	pool := &slotPool{size: 2, free: 2}
	if err := pool.take(1); err != nil {
		t.Fatal(err)
	}
	if err := pool.pause(1); err != nil {
		t.Fatalf("a pause holding one of two slots with nothing queued: %v", err)
	}
	if err := pool.take(2); err == nil {
		t.Error("a take of both slots while one is held across a pause was let wait, want it refused")
	}
	if err := pool.take(1); err != nil {
		t.Errorf("a take of the slot no paused test holds: %v", err)
	}
	pool.resume(1)
	pool.give(2)

	queued := &slotPool{size: 2, free: 2}
	if err := queued.take(1); err != nil {
		t.Fatal(err)
	}
	go func() { _ = queued.take(2) }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		queued.mu.Lock()
		n := len(queued.queue)
		queued.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the take of two never queued")
		}
	}
	if err := queued.pause(1); err == nil {
		t.Error("a pause holding a slot a queued take of two needs was let through, want it refused")
	}
	queued.give(1)
}
