package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A graceful stop gives the message in hand its grace and then cancels its handling, NAKs every
// message it has not handled with the hand-back delay, in stream order, marks the lease released
// and unsettled, and drains its subscription: the standby takes the lease at its next poll,
// settles, and delivers the stopped pod's messages in the order they were published once the
// hand-back delay has run, with no ack wait.
// It must not run in parallel: its test lease lapses if one renewal is 300 ms late.
func TestAGracefulStopHandsWhatItHeldBackAndReleasesTheLeaseToTheStandby(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-handover"
	inHand := make(chan context.Context, 1)
	var activeHandled atomic.Int32
	active := startReceiverRig(t, uri, machineID, "pod-a", func(ctx context.Context, msg *natsgo.Msg) {
		activeHandled.Add(1)
		inHand <- ctx
		// A delivery that never answers: it ends only when the stop cancels it, and the handler
		// then NAKs it, as the delivery handler does a cancelled delivery.
		<-ctx.Done()
		_ = msg.Nak()
	}, testReceiveTiming(), testLeaseTiming())
	active.awaitState(t, receiveActive, 10*time.Second)
	handle, received := recordingHandler()
	standby := startReceiverRig(t, uri, machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	waitFor(t, "the standby to read pod-a's lease", func() bool {
		return standby.lease.Status().ActiveInstance == "pod-a"
	})

	for _, data := range []string{"first", "second", "third"} {
		publishDurable(t, active.client, data)
	}
	var handling context.Context
	select {
	case handling = <-inHand:
	case <-time.After(5 * time.Second):
		t.Fatal("the active pod never took the first message")
	}
	waitFor(t, "the active pod to hold all three messages", func() bool {
		info, err := active.client.JS().ConsumerInfo(bus.Stream, active.consumer)
		return err == nil && info.NumAckPending == 3
	})

	stopped := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	active.receiver.Stop(ctx)
	if took := time.Since(stopped); took > 3*time.Second {
		t.Fatalf("the stop took %s, want the 300 ms grace, the flush, the release and the drain", took)
	}
	if handling.Err() == nil {
		t.Fatal("the stop left the message in hand's handling running past its grace")
	}
	if status := active.lease.Status(); status.Holding {
		t.Fatal("the stopped pod still holds the lease")
	}
	end := handBackEnd(t, active)
	if end["handed_back"] != float64(3) {
		t.Fatalf("the stopped pod's hand-back ended %v, want all three handed back", end)
	}

	for _, want := range []string{"first", "second", "third"} {
		message := awaitReceived(t, received, 5*time.Second, "the standby's "+want)
		if message.data != want || message.delivered < 2 {
			t.Fatalf("the standby received %q as delivery %d, want %q redelivered", message.data, message.delivered, want)
		}
		if after := message.at.Sub(stopped); after >= consumerAckWait {
			t.Fatalf("the standby received %q %s after the stop, want it after the hand-back delay rather than the ack wait", want, after)
		}
	}
	noneReceived(t, received, time.Second, "after the three handed back")
	if status := standby.lease.Status(); !status.Holding || status.How != leaseTakenReleased || !status.Settle {
		t.Fatalf("the standby's lease after the stop: %+v, want held, taken from a released record, settling", status)
	}
	if got := activeHandled.Load(); got != 1 {
		t.Fatalf("the stopped pod handled %d messages, want only the first", got)
	}
}

// handBackEnd waits for the rig's log record of its hand-back's end and returns it.
func handBackEnd(t *testing.T, rig *receiverRig) map[string]any {
	t.Helper()
	return rig.logRecord(t, "handed the durable's messages back to the next holder", 5*time.Second)
}

// logRecord waits for the rig's log record whose msg is msg and returns it.
func (rig *receiverRig) logRecord(t *testing.T, msg string, within time.Duration) map[string]any {
	t.Helper()
	var found map[string]any
	testwait.WithinPolling(t, within, 50*time.Millisecond, "the log record "+msg, func() bool {
		for line := range strings.SplitSeq(rig.logs.String(), "\n") {
			var record map[string]any
			if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == msg {
				found = record
				return true
			}
		}
		return false
	})
	return found
}
