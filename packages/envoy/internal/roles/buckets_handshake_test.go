package roles

import (
	"context"
	"fmt"
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
// Buckets waited behind it. A second caller tries for b.mu from the moment a read made during the
// hold begins until that read has answered, again and again, and must take it within
// bus.ConnLockWait each time it tries; the read answers its caller at its own bound; and b.mu is
// free within testnats.HeldHandshakeAnswerWait of that answer.
//
// The role bucket does not exist, so no read keeps a handle and the read in the hold opens one, as
// a first read after a reconnect does. The controls: nats.go's lock is still held when the two
// have answered (RequireLockHeld), so they answered during the handshake; and a read before the
// hold and one after its release answer the role's empty claim, so the row's answers are the
// hold's. The negative control is bucket as it was (OpenKeyValue with context.Background() under
// b.mu): the second caller does not take b.mu, and b.mu is still held when the wait ends. An open
// under b.mu bounded by anything longer than bus.ConnLockWait fails the second caller too, since it
// keeps trying for as long as the read takes.
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

	conn := client.Connection()
	hold.HoldReconnect(t, conn)
	// The read's own bound, inside the answer wait.
	const readWait = time.Second
	read := make(chan struct{})
	answers := testnats.AnswersWhileHeld(t, "a read of the role's claim, and a second caller of b.mu,",
		func() error {
			defer close(read)
			ctx, cancel := context.WithTimeout(t.Context(), readWait)
			defer cancel()
			_, err := buckets.RoleClaim(ctx, "reviewer")
			return err
		},
		func() error {
			for tries := 1; ; tries++ {
				select {
				case <-read:
					return nil
				default:
				}
				if !lockedWithin(&buckets.mu, bus.ConnLockWait) {
					return fmt.Errorf("try %d did not take b.mu within %s while the read waited on its open", tries, bus.ConnLockWait)
				}
			}
		})
	testnats.RequireLockHeld(t, conn)
	if answers[0] == nil {
		t.Fatal("a read of the role's claim answered nil while the handshake was held, want its bound's error")
	}
	if answers[1] != nil {
		t.Fatalf("a second caller of b.mu, trying while the read waited: %v", answers[1])
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
