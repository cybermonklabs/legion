package roles

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A bucket's open holds no lock of Buckets across a reconnect's handshake. While the hub holds its
// answer to the client's CONNECT, nats.go holds its connection lock, and the open's STREAM.INFO
// waits for it. The open was made under b.mu with no context, so it held b.mu until the handshake
// attempt ended, long after the read that made it had answered its caller: every other read of
// Buckets waited behind it. The row decides by events, never by how long a call took: a read made
// during the hold begins its open with b.mu free, which the row reads as the open begins
// (openBucketHook); a second caller takes b.mu while that open has begun and not ended, since the
// open waits for it; and the read answers its caller, at its own bound, within the attempt held when
// it began (HandshakeHold.AnswersWithinAttempt).
//
// The role bucket does not exist, so no read keeps a handle and the read in the hold opens one, as
// a first read after a reconnect does. The controls: a read before the hold and one after its
// release answer the role's empty claim, so the row's answers are the hold's. The negative controls:
// bucket as it was (OpenKeyValue with context.Background() under b.mu), and an open under b.mu with
// any bound, 150 ms or the read's own, begin the open with b.mu held.
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
	// At each open's start: whether b.mu was free, so the row reads that the open runs with it
	// released. The open then waits until the second caller has tried for b.mu, so that caller
	// tries while the open has begun and not ended.
	opens := make(chan bool, 16)
	secondDone := make(chan struct{})
	seeOpen := func() {
		free := buckets.mu.TryLock()
		if free {
			buckets.mu.Unlock()
		}
		opens <- free
		<-secondDone
	}
	openBucketHook.Store(&seeOpen)
	t.Cleanup(func() { openBucketHook.Store(nil) })
	read := make(chan error, 1)
	answers := hold.AnswersWithinAttempt(t, conn, "a read of the role's claim, and a second caller of b.mu,",
		func() error {
			_, err := buckets.RoleClaim(t.Context(), "reviewer")
			read <- err
			return err
		},
		func() error {
			defer close(secondDone)
			select {
			case free := <-opens:
				if !free {
					return errOpenUnderLock
				}
			case err := <-read:
				read <- err
				return errNoOpen
			}
			// The open has begun with b.mu free and waits for this caller: it takes b.mu now.
			buckets.mu.Lock()
			buckets.mu.Unlock()
			return nil
		})
	if answers[0] == nil {
		t.Fatal("a read of the role's claim answered nil while the handshake was held, want its bound's error")
	}
	if answers[1] != nil {
		t.Fatalf("a second caller of b.mu: %v", answers[1])
	}

	openBucketHook.Store(nil)
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

var (
	errOpenUnderLock = rowError("the read began its bucket's open with b.mu held, so every other read of Buckets waits behind the open")
	errNoOpen        = rowError("control: the read answered without opening its bucket")
)

type rowError string

func (e rowError) Error() string { return string(e) }
