package bus

// Rows for the three answers a read of a connection's state gives through the probe (requestRead,
// requestLife, the life's own goroutine): up, down, and "unknown", a read the probe's budget ended
// (ConnLockWait). A caller that holds a lock lets its lock go on "unknown"; one that holds none
// waits for nats.go's own answer within its context; a real "down" answers the loss at once. Each
// row decides by counted events or by order, and a row that fails ends at the test's deadline.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// slowFirstRead returns a seam that answers true, "unknown", for its first read alone, and the count
// of the reads it was asked about.
func slowFirstRead() (func() bool, *atomic.Int64) {
	var reads atomic.Int64
	return func() bool { return reads.Add(1) == 1 }, &reads
}

// everyReadSlow returns a seam that answers true, "unknown", for every read, and the count of the
// reads it was asked about: a row whose client makes requests of its own besides the row's cannot
// hand the row's request a read the client's other request took.
func everyReadSlow() (func() bool, *atomic.Int64) {
	var reads atomic.Int64
	return func() bool { reads.Add(1); return true }, &reads
}

// A publish its caller sends once (a message id whose duplicate verdict it reads), by a caller that
// holds no lock, whose request's read of the connection the probe's budget ended - a loaded box's
// slow read on a connection that is up, made here by the read's seam (requestReadSlow) - is stored:
// the read waits on for nats.go's own answer within the publish's context, which reads the
// connection connected, and the publish is sent. Before this, a slow read answered the loss, the
// publish was not sent, and a route answered it 503.
//
// The control is "unknown" read as the loss for every caller: the publish answers ErrConnectionLost
// and stores nothing.
func TestASlowReadWithNoLockHeldWaitsForTheRealAnswerAndThePublishIsStored(t *testing.T) {
	client, _ := heldClient(t)
	slow, reads := everyReadSlow()
	client.requestReadSlow.Store(&slow)
	defer client.requestReadSlow.Store(nil)
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	subject := contracts.AgentSubject(contracts.NoProject, "ses_slow_read_no_lock")
	ack, err := client.sendStored(ctx, subject, []byte(`{"slow":true}`), "slow-read-no-lock", true, nats.Context(ctx))
	if reads.Load() == 0 {
		t.Fatal("the publish's request never read the connection through its seam, so the row shows nothing")
	}
	if err != nil {
		t.Fatalf("a publish sent once, whose read the probe's budget ended on a connection that is up, answered %v, want it stored", err)
	}
	if ack.Sequence == 0 {
		t.Fatal("the publish answered no error and no stream sequence, want the stream's acknowledgement")
	}
}

// A request made under a lock (WithLockHeld), whose read of the connection the probe's budget ended,
// answers the loss at once, unsent, so its caller lets the lock go: it is not sent, though the
// connection is up and nats.go's own answer would come at once.
//
// The control is the lock's mark ignored: the request waits for nats.go's own answer, is sent, and
// answers no error.
func TestASlowReadUnderALockAnswersTheLossAtOnce(t *testing.T) {
	client, _ := heldClient(t)
	slow, reads := everyReadSlow()
	client.requestReadSlow.Store(&slow)
	defer client.requestReadSlow.Store(nil)
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	sent := false
	_, err := Request(WithLockHeld(ctx), client, func(ctx context.Context) (struct{}, error) {
		sent = true
		return struct{}{}, nil
	})
	if reads.Load() == 0 {
		t.Fatal("the request never read the connection through its seam, so the row shows nothing")
	}
	if sent || !errors.Is(err, ErrConnectionLost) || !requestUnsent(err) {
		t.Fatalf("a request under a lock whose read the probe's budget ended was sent %v and answered %v, want ErrConnectionLost, unsent", sent, err)
	}
}

// A request whose caller holds no lock, made while the hub holds a reconnect's handshake, waits for
// nats.go's own answer for as long as its own context lasts, and no longer: the row cancels that
// context once the request's read is under way, and the request answers its context's end while the
// handshake is still held. The read's seam skips the budgeted read (requestReadSlow), so the row
// sees the wait begin as the probe's next read (ProbesForTest).
//
// The controls are the wait not bounded by the request's context, which gives no answer until the
// handshake ends, and "unknown" read as the loss, which never begins the wait.
func TestANoLockRequestDuringAHeldHandshakeEndsAtItsOwnContext(t *testing.T) {
	client, hold := proxiedClient(t)
	awaitHeld(t, hold.Hold())
	always := func() bool { return true }
	client.requestReadSlow.Store(&always)
	defer client.requestReadSlow.Store(nil)
	before := client.ProbesForTest()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	answered := make(chan error, 1)
	go func() {
		_, err := Request(ctx, client, func(ctx context.Context) (struct{}, error) { return struct{}{}, ctx.Err() })
		answered <- err
	}()
	for deadline := time.Now().Add(testnats.Remaining(t)); client.ProbesForTest() == before; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			hold.Release()
			t.Fatal("the request never began its wait for nats.go's own answer by the test's deadline")
		}
	}
	cancel()
	if err := awaitBeforeHoldEnds(t, hold, answered, "the request's answer at its context's end"); !errors.Is(err, context.Canceled) || errors.Is(err, ErrConnectionLost) {
		t.Fatalf("a request whose context ended while it waited for nats.go's own answer answered %v, want its context's end", err)
	}
}

// A read the probe's budget ended - a false "down" on a loaded box - ends no life that hears
// nats.go's own reports: the request that made it is not sent, and the life running on the
// connection, whose goroutine has its status listener, goes on, so the requests bound to it are not
// answered with the loss. requestLife ends a life before it returns, so the row reads the life's
// state at once after it.
//
// The control is the down branch ending the running life on every read that is not up: the life
// ends, and the row fails.
func TestASlowReadEndsNoLifeThatHearsNatsGosReports(t *testing.T) {
	client, _ := proxiedClient(t, withLossAtOnce())
	endRunningLife(client)
	life, _, _ := client.requestLife(t.Context())
	if life.Err() != nil {
		t.Fatalf("the row's life ended as it began: %v", context.Cause(life))
	}
	for deadline := time.Now().Add(testnats.Remaining(t)); ; time.Sleep(time.Millisecond) {
		client.mu.Lock()
		listening := client.life == life && client.lifeListening
		client.mu.Unlock()
		if listening {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the life's goroutine never received its status listener by the test's deadline")
		}
	}
	down, reads := slowFirstRead()
	restore := client.ProbeReadsDownForTest(down)
	defer restore()
	if slow, _, _ := client.requestLife(t.Context()); slow.Err() == nil {
		t.Fatal("a request whose read the probe's budget ended was bound to a running life, want it not sent")
	}
	restore()
	if reads.Load() == 0 {
		t.Fatal("the request never read the probe's seam, so the row shows nothing")
	}
	if life.Err() != nil {
		t.Fatalf("a read the probe's budget ended, on a connection that is up, ended the life running on it: %v", context.Cause(life))
	}
}

// A read that began before a life did ends only the life that was running when it began: a
// reconnect in place keeps the connection, so a life a request began on it while the read was kept
// waiting is another life, whose requests the read says nothing of. The row's seam begins that life
// while the first read is kept waiting, with no life running when that read began.
//
// The control is the down branch ending whatever life runs on the connection when the read ends:
// the life begun meanwhile, whose goroutine has no listener yet, ends.
func TestASlowReadEndsNoLifeBegunAfterItBegan(t *testing.T) {
	client, _ := proxiedClient(t, withLossAtOnce())
	endRunningLife(client)
	var later context.Context
	var reads atomic.Int64
	down := func() bool {
		if reads.Add(1) != 1 {
			return false
		}
		later, _, _ = client.requestLife(t.Context())
		return true
	}
	restore := client.ProbeReadsDownForTest(down)
	defer restore()
	if slow, _, _ := client.requestLife(t.Context()); slow.Err() == nil {
		t.Fatal("a request whose read the probe's budget ended was bound to a running life, want it not sent")
	}
	restore()
	if later == nil {
		t.Fatal("the seam never began the later life, so the row shows nothing")
	}
	if later.Err() != nil {
		t.Fatalf("a read that began before a life did ended that life: %v", context.Cause(later))
	}
}

// A new life's goroutine whose first read of the connection the probe's budget ended, with no
// connection event since the life began, does not end the life: a loaded box's slow read on a
// connection that was up a moment ago, when the request that began the life read it. The goroutine
// goes on to its listener, which the row waits for.
//
// The control is "unknown" ending the life at any read: the life ends at its first read, before its
// goroutine has its listener.
func TestALifesFirstSlowReadDoesNotEndIt(t *testing.T) {
	client, _ := proxiedClient(t)
	endRunningLife(client)
	slow, reads := slowFirstRead()
	client.lifeReadSlow.Store(&slow)
	defer client.lifeReadSlow.Store(nil)
	life, _, _ := client.requestLife(t.Context())
	if life.Err() != nil {
		t.Fatalf("the row's life ended as it began: %v", context.Cause(life))
	}
	for deadline := time.Now().Add(testnats.Remaining(t)); ; time.Sleep(time.Millisecond) {
		if life.Err() != nil {
			t.Fatalf("a new life's goroutine ended it at a slow first read, with no connection event since it began: %v", context.Cause(life))
		}
		client.mu.Lock()
		listening := client.life == life && client.lifeListening
		client.mu.Unlock()
		if listening {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the life's goroutine never received its status listener by the test's deadline")
		}
	}
	if reads.Load() == 0 {
		t.Fatal("the life's goroutine never read the connection through its seam, so the row shows nothing")
	}
}
