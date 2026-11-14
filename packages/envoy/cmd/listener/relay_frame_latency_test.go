package main

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// relayFrameRoundTrips is how many frames the measurement below sends, one at a time.
const relayFrameRoundTrips = 200

// relayFrameP99Bound is what the gate below holds a run's p99 round trip to: half of
// relaySettleGrace, 375 ms.
const relayFrameP99Bound = relaySettleGrace / 2

// The settle grace a stopping relay gives the frames it has unanswered (relaySettleGrace) is
// sized on one frame's round trip: the session's reply to it and the relay's acknowledgement
// after that reply, which is all the grace has to cover. This row measures that round trip on
// the test host, from the moment the frame reaches the session to the moment the consumer's ack
// floor has passed it, over relayFrameRoundTrips frames sent one at a time. It is the frame
// world's replacement for the push-latency measurement the serving-port relay's grace was sized
// on.
//
// It gates on one thing: the run's p99 must stay under relayFrameP99Bound, 375 ms, half of the
// 750 ms grace (frameRoundTripFailure, which TestTheFrameRoundTripGateCatchesASlowRelay
// exercises). The p50, the p90 and the slowest round trip are printed and gated on nothing. One
// slow sample is the box pausing rather than the round trip changing, and it costs no message:
// a frame the grace ends without a done is sent again by the successor under its own event id,
// which the session reads as the frame it already has, so such a frame is seen twice at worst
// and never lost (settleStoppedFrames, and cmd/listener/README.md, "When a session can see a
// message twice"). A p99 at the bound is the relay itself slowing, which is what would leave
// the grace too small.
//
// The measurements the 750 ms grace is sized on, all of them, are here. On the test host over
// 200 frames: p50 2.44 ms, p99 11.8 ms, slowest 37.2 ms on a quiet box, and p50 2.58 ms,
// p99 8.26 ms, slowest 111 ms with the rest of this package running beside it. On the pod over
// 40 runs of this row (the tester's round 36): the worst run had p50 5.0 ms, p90 9.6 ms and
// p99 116.9 ms, with its slowest single round trip 520.1 ms, and the other 39 runs' slowest
// stayed under 169 ms. So the grace is many times the round trip it covers, and the bound this
// row gates on is many times the p99 that was measured.
func TestAFramesRoundTripIsFarInsideTheRelaysSettleGrace(t *testing.T) {
	rig := newRelayRig(t)
	const sessionID = "ses_frame_latency"
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-latency"))
	subject := contracts.AgentSubject(sessionID)
	consumer, ok := bus.SessionConsumerName(sessionID)
	if !ok {
		t.Fatalf("no session consumer name for %s", sessionID)
	}

	ackedPast := func(seq uint64) time.Time {
		deadline := time.Now().Add(10 * time.Second)
		for {
			info, err := rig.harness.client.JS().ConsumerInfo(bus.Stream, consumer)
			if err == nil && info.AckFloor.Stream >= seq {
				return time.Now()
			}
			if time.Now().After(deadline) {
				t.Fatalf("the relay never acked the message at stream sequence %d: %+v, %v", seq, info, err)
			}
			time.Sleep(200 * time.Microsecond)
		}
	}

	roundTrips := make([]time.Duration, 0, relayFrameRoundTrips)
	for range relayFrameRoundTrips {
		_, seq := rig.storeEvent(t, subject)
		var frame relayedFrame
		select {
		case frame = <-standIn.frames:
		case <-time.After(15 * time.Second):
			t.Fatalf("the relay never sent the message at stream sequence %d to the session", seq)
		}
		roundTrips = append(roundTrips, ackedPast(seq).Sub(frame.arrived))
	}

	run := readFrameRoundTrips(roundTrips)
	t.Logf("%d frame round trips (the session's reply and the relay's ack after it): p50 %s, p90 %s, p99 %s, slowest %s, against relaySettleGrace %s",
		len(roundTrips), run.p50.Round(time.Microsecond), run.p90.Round(time.Microsecond), run.p99.Round(time.Microsecond), run.slowest.Round(time.Microsecond), relaySettleGrace)
	if failure := frameRoundTripFailure(run); failure != "" {
		t.Fatal(failure)
	}
}

// frameRoundTripRun is one run of the measurement above as the gate reads it: the percentiles of
// its round trips and the slowest of them.
type frameRoundTripRun struct {
	p50, p90, p99, slowest time.Duration
}

// readFrameRoundTrips sorts samples and reads one run off them.
func readFrameRoundTrips(samples []time.Duration) frameRoundTripRun {
	slices.Sort(samples)
	return frameRoundTripRun{
		p50:     samples[len(samples)/2],
		p90:     samples[(len(samples)*90)/100],
		p99:     samples[(len(samples)*99)/100],
		slowest: samples[len(samples)-1],
	}
}

// frameRoundTripFailure is the gate itself: the line a run fails with when its p99 is
// relayFrameP99Bound or more, and empty when that p99 is under the bound. The slowest sample
// fails nothing, whatever it is.
func frameRoundTripFailure(run frameRoundTripRun) string {
	if run.p99 < relayFrameP99Bound {
		return ""
	}
	return fmt.Sprintf("the p99 frame round trip is %s, half of relaySettleGrace (%s) or more: the grace is sized on this round trip and must be re-read (slowest %s)",
		run.p99, relaySettleGrace, run.slowest)
}

// frameRoundTripSamples is a run's samples: count round trips of each, with slow after them. A
// run of relayFrameRoundTrips samples reads its p99 off that tail, the second-slowest of 200.
func frameRoundTripSamples(count int, each time.Duration, slow ...time.Duration) []time.Duration {
	samples := make([]time.Duration, 0, count+len(slow))
	for range count {
		samples = append(samples, each)
	}
	return append(samples, slow...)
}

// The gate still catches a relay that slowed, and lets a box that paused past: a sample set
// whose p99 reaches relayFrameP99Bound fails it, one under the bound passes, and a single sample
// past the whole grace - the excursion the measurement above saw once in 40 runs - is a pass,
// since that frame is repeated under its own event id rather than lost.
func TestTheFrameRoundTripGateCatchesASlowRelay(t *testing.T) {
	for _, row := range []struct {
		name    string
		samples []time.Duration
		fails   bool
	}{
		{
			name:    "a quiet run",
			samples: frameRoundTripSamples(relayFrameRoundTrips, 5*time.Millisecond),
			fails:   false,
		},
		{
			name:    "one sample past the whole grace",
			samples: frameRoundTripSamples(relayFrameRoundTrips-1, 5*time.Millisecond, relaySettleGrace+150*time.Millisecond),
			fails:   false,
		},
		{
			name:    "a p99 just under the bound",
			samples: frameRoundTripSamples(relayFrameRoundTrips-2, 5*time.Millisecond, relayFrameP99Bound-time.Millisecond, 600*time.Millisecond),
			fails:   false,
		},
		{
			name:    "a p99 at the bound",
			samples: frameRoundTripSamples(relayFrameRoundTrips-2, 5*time.Millisecond, relayFrameP99Bound, 600*time.Millisecond),
			fails:   true,
		},
		{
			name:    "a p99 over the bound",
			samples: frameRoundTripSamples(relayFrameRoundTrips-2, 5*time.Millisecond, relayFrameP99Bound+10*time.Millisecond, 600*time.Millisecond),
			fails:   true,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			run := readFrameRoundTrips(slices.Clone(row.samples))
			failure := frameRoundTripFailure(run)
			if (failure != "") != row.fails {
				t.Fatalf("a run with p99 %s and slowest %s: the gate answered %q, want a failure: %v", run.p99, run.slowest, failure, row.fails)
			}
		})
	}
}
