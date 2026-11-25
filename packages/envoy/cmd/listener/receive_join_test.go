package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// startJoiningReceiverRig is startReceiverRig for a pod whose start the NATS grant refused the
// listener state bucket: its receive loop runs without a lease, as the machine's one deliverer,
// and the rig's lease is the one the pod joins once a grant lets it open the bucket, which a row
// hands over through the receiver's join (durableReceiver.startJoin), as main does. Neither the
// lease's loop nor its first rounds are started here. adjust, when not nil, changes the receiver's
// options before it is made: its holder-only loops (durableReceiverOptions.loops), its logger.
func startJoiningReceiverRig(t *testing.T, uri, machineID string, adjust func(*receiverRig, *durableReceiverOptions)) *receiverRig {
	t.Helper()
	handle, _ := recordingHandler()
	rig := newReceiverRig(t, uri, machineID, "pod-a", handle, testReceiveTiming(), testLeaseTiming())
	// The rig's own receiver was made with its lease; this one is made without, from the same
	// options (newDurableReceiver leaves the lease out of them), and it is the one the rig's
	// reconnect hook and cleanup reach, since both read rig.receiver when they run.
	options := rig.receiver.durableReceiverOptions
	if adjust != nil {
		adjust(rig, &options)
	}
	rig.receiver = newDurableReceiver(options)
	rig.receiver.Start()
	return rig
}

// stopReceiving stops the rig's receiving as the shutdown does (durableReceiver.Stop), within the
// shutdown's own 10 s budget.
func stopReceiving(rig *receiverRig) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rig.receiver.Stop(ctx)
}

// leaseRecordOf reads the rig's lease record, and reports false when the bucket holds none.
func leaseRecordOf(t *testing.T, rig *receiverRig) (leaseRecord, bool) {
	t.Helper()
	entry, err := rig.lease.kv.Get(rig.lease.key)
	if errors.Is(err, natsgo.ErrKeyNotFound) {
		return leaseRecord{}, false
	}
	if err != nil {
		t.Fatalf("read the lease record: %v", err)
	}
	record, err := decodeLeaseRecord(entry.Value())
	if err != nil {
		t.Fatalf("decode the lease record %s: %v", entry.Value(), err)
	}
	return record, true
}

// joinOutlived is how long a row waits after the stop for anything of a join the stop should have
// ended: a lease's take of an absent record and three of testLeaseTiming's renewals.
const joinOutlived = 400 * time.Millisecond

// A pod delivering without a lease that stops while it still tries to join its machine's lease
// (durableReceiver.startJoin) leaves no record of its own renewing after the stop. The stop ends the
// try and waits for it before it stops receiving (durableReceiver.Stop): a try that has not opened
// the bucket returns with no lease, so a grant that lets the open through after the stop takes
// nothing; and a try whose open went through as the stop came, whose first round takes an absent
// record whatever its context says (lease.Contend), hands that lease over, and the stop releases it.
//
// Before, the join ran on a goroutine of main's under a context the shutdown never ended, since
// the shutdown's os.Exit skips its deferred cancel, and the stop did not wait for it: a join that
// completed after the stop took the record and renewed it until the exit, and the standby waited it
// out as an expired one, a hard take-over after a graceful stop.
// It must not run in parallel: its test lease lapses if one renewal is 300 ms late.
func TestAStopEndsALeaseJoinStillTryingAndReleasesTheLeaseItTook(t *testing.T) {
	uri := testnats.URL(t)

	t.Run("a join the stop ended takes no lease", func(t *testing.T) {
		rig := startJoiningReceiverRig(t, uri, "box-join-ended", nil)
		rig.awaitState(t, receiveActive, 10*time.Second)
		granted, ended := make(chan struct{}), make(chan struct{})
		rig.receiver.startJoin(func(ctx context.Context) *lease {
			select {
			case <-granted:
			case <-ctx.Done():
				close(ended)
				return nil
			}
			rig.lease.Contend(ctx)
			rig.lease.Start(context.Background())
			return rig.lease
		})

		stopReceiving(rig)
		select {
		case <-ended:
		default:
			t.Fatal("the stop returned with the lease join still trying: nothing ended it, so a grant applied now would take the record after the stop")
		}
		// The grant lets the open through now, after the stop: a try still running would take
		// the record and renew it.
		close(granted)
		time.Sleep(joinOutlived)
		if record, found := leaseRecordOf(t, rig); found && !record.Released {
			t.Fatalf("the lease record %s after the stop is %+v, want none or a released one", joinOutlived, record.leaseFields)
		}
	})

	t.Run("a lease the join took as the stop came is released by the stop", func(t *testing.T) {
		rig := startJoiningReceiverRig(t, uri, "box-join-landed", nil)
		rig.awaitState(t, receiveActive, 10*time.Second)
		took := make(chan leaseStatus, 1)
		rig.receiver.startJoin(func(ctx context.Context) *lease {
			<-ctx.Done()
			took <- rig.lease.Contend(ctx)
			rig.lease.Start(context.Background())
			return rig.lease
		})

		stopReceiving(rig)
		select {
		case status := <-took:
			if !status.Holding {
				t.Fatalf("UNKNOWN: the join's first round left the lease %+v, want it holding the absent record; the row needs a join that took the record", status)
			}
		default:
			t.Fatal("the stop returned before the lease join it ended had returned")
		}
		record, found := leaseRecordOf(t, rig)
		if !found || record.Holder != "pod-a" || !record.Released {
			t.Fatalf("the lease record after the stop is %+v (found %t), want pod-a's, released", record.leaseFields, found)
		}
		time.Sleep(joinOutlived)
		if again, _ := leaseRecordOf(t, rig); !again.Released {
			t.Fatalf("the lease record %s after the stop is %+v, want it still released: a renewal wrote it again", joinOutlived, again.leaseFields)
		}
	})
}

// joinShutdownRounds is how many pods TestAShutdownThatComesAsTheLeaseJoinsEndsTheTermItReceivedOn
// stops as their lease joins. A wait that took the join one time in two would pass them all once
// in 65,536 runs.
const joinShutdownRounds = 16

// A shutdown that comes as the lease joins a pod receiving without one ends the term the pod
// received on, as the stop of a pod without a lease ends it: its subscription drains and what it
// holds is NAKed, and the stop then releases the lease that joined, which the pod never took up.
// The receive loop meets the join and the shutdown together: the row holds the loop before it
// waits on either, in the start of its holder-only loops (durableReceiverOptions.loops), hands the
// pod its lease, which takes the absent record, ends the loop's context as the stop's first step
// does, and only then lets the loop go, so both are ready when it waits. Each round is a pod of its
// own.
//
// The wait took either of the two at random, and took the join one time in two: the term was
// handed to the lease (joinTerm), and the loop returned at the shutdown before it served the
// lease's term, so nothing drained or NAKed and the stop released the lease with the pod's
// subscription still open. A round the wait sends to the join logs leaseJoinedHolderLine.
// It must not run in parallel: its test lease lapses if one renewal is 300 ms late.
func TestAShutdownThatComesAsTheLeaseJoinsEndsTheTermItReceivedOn(t *testing.T) {
	uri := testnats.URL(t)
	for round := range joinShutdownRounds {
		if !t.Run(fmt.Sprintf("round %d", round), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			rig := startJoiningReceiverRig(t, uri, fmt.Sprintf("box-join-shutdown-%d", round), func(_ *receiverRig, options *durableReceiverOptions) {
				options.loops = func(context.Context) []<-chan struct{} {
					once.Do(func() { close(entered) })
					<-release
					return nil
				}
			})
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				close(release)
				t.Fatal("the pod never received the durable without a lease")
			}

			rig.receiver.startJoin(func(ctx context.Context) *lease {
				rig.lease.Contend(ctx)
				rig.lease.Start(context.Background())
				return rig.lease
			})
			select {
			case <-rig.receiver.joinCtx.Done():
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("the lease never joined")
			}
			if status := rig.lease.Status(); !status.Holding {
				close(release)
				t.Fatalf("UNKNOWN: the joined lease is %+v, want it holding the record; the row needs a join that took it", status)
			}
			rig.receiver.mu.Lock()
			stop := rig.receiver.stop
			rig.receiver.mu.Unlock()
			stop()
			close(release)
			stopReceiving(rig)

			if strings.Contains(rig.logs.String(), leaseJoinedHolderLine) {
				t.Fatalf("the pod logged %q after its shutdown came: the wait took the join, and the term went to the lease rather than ending with the shutdown", leaseJoinedHolderLine)
			}
			if report := rig.receiver.report(); report.State != receiveStandby {
				t.Fatalf("the stopped pod's receive loop stands at %q, want %q: its term was never ended", report.State, receiveStandby)
			}
			rig.logRecord(t, "stopped receiving the durable", time.Second)
			awaitDurable(t, rig.client, rig.consumer, "the durable to read unbound once the pod's term ended", func(info *natsgo.ConsumerInfo) bool {
				return !info.PushBound
			})
			if record, found := leaseRecordOf(t, rig); !found || record.Holder != "pod-a" || !record.Released {
				t.Fatalf("the lease record after the stop is %+v (found %t), want pod-a's, released by the stop", record.leaseFields, found)
			}
		}) {
			break
		}
	}
}

// heldLogLine is a log writer that holds the first write of a record carrying line until release
// is closed, closing at once that write begins, and passes every write on to w: a row stops the
// goroutine that logs line just after it does, which nothing else of the code lets a row do.
type heldLogLine struct {
	w       io.Writer
	line    []byte
	at      chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *heldLogLine) Write(p []byte) (int, error) {
	if bytes.Contains(p, h.line) {
		h.once.Do(func() {
			close(h.at)
			<-h.release
		})
	}
	return h.w.Write(p)
}

// A shutdown that comes once the lease that joined has taken over the term a pod received on
// without a lease (joinTerm) ends that term as the holder's, with the hand-back a holder's stop
// makes: its subscription drains once the hand-back's linger ends, and the lease is released. The
// row holds the receive loop on the join's own line (leaseJoinedHolderLine), which it logs once
// the lease and the term are the holder's, and ends the loop's context there, as the stop's first
// step does, before it lets the loop go on.
//
// Before, the loop returned at the shutdown before it served the lease's term (run), so the term
// was never ended: its subscription stayed open, nothing was handed back, and the stop released the
// lease under it.
// It must not run in parallel: its test lease lapses if one renewal is 300 ms late.
func TestAShutdownRightAfterTheLeaseTookTheTermOverHandsTheTermBack(t *testing.T) {
	uri := testnats.URL(t)
	held := &heldLogLine{line: []byte(leaseJoinedHolderLine), at: make(chan struct{}), release: make(chan struct{})}
	rig := startJoiningReceiverRig(t, uri, "box-join-taken", func(rig *receiverRig, options *durableReceiverOptions) {
		held.w = rig.logs
		options.logger = logging.NewWithWriter("box-join-taken", held)
	})
	rig.awaitState(t, receiveActive, 10*time.Second)
	rig.receiver.startJoin(func(ctx context.Context) *lease {
		rig.lease.Contend(ctx)
		rig.lease.Start(context.Background())
		return rig.lease
	})
	select {
	case <-held.at:
	case <-time.After(5 * time.Second):
		close(held.release)
		t.Fatal("the joined lease never took the term over")
	}
	rig.receiver.mu.Lock()
	stop := rig.receiver.stop
	rig.receiver.mu.Unlock()
	stop()
	close(held.release)
	stopReceiving(rig)

	if report := rig.receiver.report(); report.State != receiveStandby {
		t.Fatalf("the stopped pod's receive loop stands at %q, want %q: the term the lease took over was never ended", report.State, receiveStandby)
	}
	if end := handBackEnd(t, rig); end["handed_back"] != float64(0) {
		t.Fatalf("the pod's hand-back ended %v, want it run with nothing to hand back", end)
	}
	awaitDurable(t, rig.client, rig.consumer, "the durable to read unbound once the pod's term ended", func(info *natsgo.ConsumerInfo) bool {
		return !info.PushBound
	})
	if record, found := leaseRecordOf(t, rig); !found || record.Holder != "pod-a" || !record.Released {
		t.Fatalf("the lease record after the stop is %+v (found %t), want pod-a's, released", record.leaseFields, found)
	}
}
