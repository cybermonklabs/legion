package roles

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// lockedWithin reports whether mu could be taken within wait, and lets it go again when it could.
func lockedWithin(mu *sync.Mutex, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if mu.TryLock() {
			mu.Unlock()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// A bucket's open holds no lock of Buckets across a reconnect's handshake. While the hub holds its
// answer to the client's CONNECT, nats.go holds its connection lock, and the open's STREAM.INFO
// waits for it. The open was made under b.mu with no context, so it held b.mu until the handshake
// attempt ended, long after the read that made it had answered its caller: every other read of
// Buckets waited behind it. While a read made during the hold waits on its open, a second caller
// takes b.mu; the read answers its caller at its own bound; and b.mu is free within
// testnats.HeldHandshakeAnswerWait of that answer.
//
// The role bucket does not exist, so no read keeps a handle and the read in the hold opens one, as
// a first read after a reconnect does. The controls: a read before the hold and one after its
// release answer the role's empty claim, so the row's answers are the hold's. The negative control
// is bucket as it was (OpenKeyValue with context.Background() under b.mu): the second caller does
// not take b.mu, and b.mu is still held when the wait ends. An open under b.mu that ended at the
// read's bound would fail the second caller's take too.
func TestABucketsOpenHoldsNoLockAcrossAHeldHandshake(t *testing.T) {
	_, url := testnats.Start(t)
	hold := testnats.StartHandshakeHold(t, url)
	client, err := bus.Connect([]string{hold.URL()})
	if err != nil {
		t.Fatalf("connect through the handshake hold: %v", err)
	}
	t.Cleanup(client.Close)
	t.Cleanup(hold.Release)
	buckets := NewBuckets(client, Windows{})
	if claim, err := buckets.RoleClaim(t.Context(), "reviewer"); err != nil || claim.HolderSessionID != "" {
		t.Fatalf("control: before the hold the claim with no role bucket is %+v, %v; want none", claim, err)
	}

	hold.HoldReconnect(t)
	// The read's own bound, inside the answer wait. The second caller tries for b.mu from 200 ms
	// into it, by when the read is waiting on its open, until 300 ms before it ends.
	const (
		readWait    = time.Second
		secondAfter = 200 * time.Millisecond
		secondFor   = 500 * time.Millisecond
	)
	answers := testnats.AnswersWhileHeld(t, "a read of the role's claim, and a second caller of b.mu,",
		func() error {
			ctx, cancel := context.WithTimeout(t.Context(), readWait)
			defer cancel()
			_, err := buckets.RoleClaim(ctx, "reviewer")
			return err
		},
		func() error {
			time.Sleep(secondAfter)
			if !lockedWithin(&buckets.mu, secondFor) {
				return errors.New("b.mu was held while the read waited on its open")
			}
			return nil
		})
	if answers[0] == nil {
		t.Fatal("a read of the role's claim answered nil while the handshake was held, want its bound's error")
	}
	if answers[1] != nil {
		t.Fatalf("a second caller of b.mu, from %s to %s after the read began: %v", secondAfter, secondAfter+secondFor, answers[1])
	}
	if !lockedWithin(&buckets.mu, testnats.HeldHandshakeAnswerWait) {
		t.Fatalf("b.mu was still held %s after the read answered %v: the open that read made holds it through the handshake", testnats.HeldHandshakeAnswerWait, answers[0])
	}

	hold.Release()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for {
		claim, err := buckets.RoleClaim(ctx, "reviewer")
		if err == nil && claim.HolderSessionID == "" {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("control: once the hold was released the claim read %+v, %v; want none", claim, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
