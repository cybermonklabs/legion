package stream

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// newerToken is the boot token of the claim's next launch: the generation the daemon started
// once its runtime reported the older process gone, while that process is still connected (R3).
const newerToken = "boot-token-newer"

// generations is a resolver that gives testClaim one launch generation per boot token, which is
// what a claim whose older process is still connected and whose newer one is dialling looks
// like: both tokens resolve, each for the launch it was minted for.
func generations(perToken map[string]uint64) *resolver {
	return &resolver{fn: func(bootToken string) (claim.Token, uint64, bool, bool, error) {
		generation, minted := perToken[bootToken]
		if !minted {
			return "", 0, false, false, nil
		}
		return testClaim, generation, false, true, nil
	}}
}

// A newer generation's first hello sends the older generation's bound connection its stop frame,
// and the newer process binds once that connection closes. Until then it stays refused, and each
// refusal names the claim and both connections' generations, which the 314 lines of 2026-11-12
// named neither of. The stop frame goes once, not once per hello.
func TestANewerGenerationStopsTheOlderAndBindsOnItsClose(t *testing.T) {
	h := startListener(t, harnessOptions{resolver: generations(map[string]uint64{
		testToken:  testGeneration,
		newerToken: testGeneration + 1,
	})})
	older := h.connect(testToken)
	bound := h.conn()

	newer := dial(t, h.listener.Addr())
	newer.hello(newerToken)
	older.expect(shimwire.TypeShutdown)
	newer.awaitClosed()

	if h.conn() != bound {
		t.Fatal("the newer generation's hello took the claim while the older connection was still bound")
	}
	want := []string{"worker-stream: rejected hello (already bound to a live stream of an older generation, which has been told to stop) " +
		"claim=" + string(testClaim) + " generation=4 boundGeneration=3"}
	if got := h.logs.Lines(); !slices.Equal(got, want) {
		t.Fatalf("logs = %q, want %q", got, want)
	}

	// The lock-out holds while the older connection does, and the older connection is told once.
	again := dial(t, h.listener.Addr())
	again.hello(newerToken)
	again.awaitClosed()
	if got := h.logs.Lines(); len(got) != 2 {
		t.Fatalf("logs = %q, want the second refusal's line beside the first", got)
	}

	// The older connection's close frees the claim, and the newer process's next hello binds.
	older.conn.Close()
	if event := h.next(); event != (Closed{Claim: testClaim, Generation: testGeneration}) {
		t.Fatalf("event = %#v, want the older connection's close", event)
	}
	// The older connection is asked for its state once it is told to stop, so the frames after
	// its stop frame are that question's; none of them is a second stop frame. Its reader has
	// ended with the close above, so every frame it read is in the channel.
	<-older.closed
	for drained := false; !drained; {
		select {
		case frame := <-older.frames:
			if frame.FrameType() == shimwire.TypeShutdown {
				t.Fatal("the older connection was sent a second shutdown; the stop frame goes once per lock-out")
			}
		default:
			drained = true
		}
	}

	next := dial(t, h.listener.Addr())
	next.hello(newerToken)
	next.expect(shimwire.TypeHelloAck)
	if event := h.next(); event != (Hello{Claim: testClaim, Generation: testGeneration + 1}) {
		t.Fatalf("event = %#v, want the newer generation bound", event)
	}
}

// A hello refused because its generation is older than the claim's current one is told so before
// its connection closes, so an older process cut off by the network, or by a daemon restart
// during a lock-out, stops when it next reaches the daemon.
func TestAHelloOfAGenerationTheClaimHasLeftIsToldToStop(t *testing.T) {
	left := &resolver{fn: func(string) (claim.Token, uint64, bool, bool, error) {
		return testClaim, testGeneration - 1, true, true, nil
	}}
	h := startListener(t, harnessOptions{resolver: left})
	p := dial(t, h.listener.Addr())
	p.hello(testToken)

	p.expect(shimwire.TypeShutdown)
	p.awaitHangUp()

	want := []string{"worker-stream: rejected hello (stale worker generation) claim=" + string(testClaim) + " generation=2"}
	if got := h.logs.Lines(); !slices.Equal(got, want) {
		t.Fatalf("logs = %q, want %q", got, want)
	}
	if _, ok := h.listener.Conn(testClaim); ok {
		t.Fatal("a hello of a generation the claim has left registered a connection")
	}
	if events := h.stop(); len(events) != 0 {
		t.Fatalf("a hello of a generation the claim has left emitted %#v", events)
	}
}
