package bus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// lossCountingReads is js with each read of a stream that answered the connection's loss
// (ErrConnectionLost) counted: a read the drop cut off, or one made while the connection was down,
// which is not sent. The count is atomic, because a read the wait abandons at its limit runs on
// beside the next one.
type lossCountingReads struct {
	nats.JetStreamContext
	lost atomic.Int32
}

func (r *lossCountingReads) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	info, err := r.JetStreamContext.StreamInfo(stream, opts...)
	if errors.Is(err, ErrConnectionLost) {
		r.lost.Add(1)
	}
	return info, err
}

// outage is how long the row below keeps the connection down: forty pauses of a wait on a stream
// (bucketAppliedInterval), so a wait that read again after each pause would read about forty times
// in it, and still ten times on a box that ran each pause four times late.
const outage = 40 * bucketAppliedInterval

// A start's wait on a stream whose connection drops waits for the client to be connected again
// before it reads again (awaitReconnect), as sentAgain does before it sends again, and goes on once
// the bus is back. A read made while the connection is down fails at once, unsent
// (ErrConnectionLost, which unanswered counts), so a wait that only paused between reads would read
// every bucketAppliedInterval for the whole outage. The row cuts the connection under a wait still
// short of its fact, keeps it down for outage, and counts the reads that answered the loss: one,
// the read the cut cut off or the first made after it, and two if the reconnect took longer than
// awaitReconnect's RequestTimeout, where a wait with no reconnect wait reads about forty times. It
// requires at least one such read, so a cut that reached no read fails the row rather than passing
// it on nothing.
//
// The control is this row with awaitStream's awaitReconnect call taken out: the wait reads again
// every bucketAppliedInterval while the connection is down, and the row fails on the count.
func TestAStreamWaitWhoseConnectionDropsReadsAgainOnlyOnceTheConnectionIsBack(t *testing.T) {
	client, hold := heldClient(t)
	reads := &lossCountingReads{JetStreamContext: client.JS()}
	var back atomic.Bool
	firstAnswer := make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	waited := make(chan error, 1)
	go func() {
		waited <- awaitStream(ctx, reads, afterOpen, Stream, "the connection was not back", "the row's wait on the stream is short, waiting", func(*nats.StreamInfo) string {
			once.Do(func() { close(firstAnswer) })
			if back.Load() {
				return ""
			}
			return "the connection has not dropped and come back yet"
		})
	}()
	select {
	case <-firstAnswer:
	case err := <-waited:
		t.Fatalf("the wait answered %v before any read of it was answered", err)
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("no read of the wait had been answered by the test's deadline")
	}

	hold.Cut()
	deadline := time.Now().Add(testnats.Remaining(t))
	for client.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("the client was still connected through the cut hold at the test's deadline")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(outage)
	back.Store(true)
	hold.Restore()

	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("the wait answered %v once the connection was back, want nil", err)
		}
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the wait had not ended by the test's deadline, though the connection was back")
	}
	lost := reads.lost.Load()
	t.Logf("%d reads of the stream answered the connection's loss in an outage of %s", lost, outage)
	switch {
	case lost == 0:
		t.Fatal("no read answered the connection's loss: the cut reached no read, so the row shows nothing")
	case lost > 2:
		t.Fatalf("%d reads answered the connection's loss in an outage of %s, want 1, or 2 after a reconnect slower than %s: the wait read again every %s while the connection was down",
			lost, outage, RequestTimeout, bucketAppliedInterval)
	}
}

// A read the connection's loss cut off, or one made while the connection was down and so not sent,
// says nothing of how long the server takes to answer, so it leaves its run's read limit as it was
// (limitedRead), where a read the server left unanswered doubles it. Doubled at each such read, the
// limit would reach RequestTimeout after four of them, and a read the server lost on the new
// connection would then cost ten seconds rather than firstReadLimit.
//
// The control row is a read the server left unanswered (nats.ErrTimeout): it doubles the limit, so
// the row reads the change it rules out for the loss.
func TestAReadTheConnectionsLossCutOffLeavesTheReadLimitAsItWas(t *testing.T) {
	for _, row := range []struct {
		name string
		err  error
		want time.Duration
	}{
		{"a read the loss cut off", &connectionLost{}, firstReadLimit},
		{"a read made while the connection was down", &connectionLost{unsent: true}, firstReadLimit},
		{"a read the server left unanswered, the control", nats.ErrTimeout, 2 * firstReadLimit},
	} {
		t.Run(row.name, func(t *testing.T) {
			limit := readLimitOf(t.Context())
			_, _, answered, err := limitedRead(t.Context(), limit, func() (*nats.StreamInfo, error) { return nil, row.err })
			if !answered || !errors.Is(err, row.err) {
				t.Fatalf("the read answered %v (answered %t), want %v", err, answered, row.err)
			}
			if limit.limit != row.want {
				t.Fatalf("the read limit after a read that answered %v is %s, want %s", row.err, limit.limit, row.want)
			}
		})
	}
}
