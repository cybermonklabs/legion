package bus

import (
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/nats-io/nats.go"
)

// A connection a test fixture marked as its own (CreateCopiedBucketsOn) is held weakly: once
// nothing else references it, it is collected and its mark removed, so a test process keeps no
// fixture connection, and nothing it references, after its test is done with it. A connection still
// referenced stays marked.
func TestAFixtureConnectionIsForgottenOnceCollected(t *testing.T) {
	kept := &nats.Conn{}
	CreateCopiedBucketsOn(kept)
	dropped := &nats.Conn{}
	CreateCopiedBucketsOn(dropped)
	keptKey, droppedKey := weak.Make(kept), weak.Make(dropped)
	if _, ok := fixtureConns.Load(droppedKey); !ok {
		t.Fatal("control: the mark of the connection about to be dropped was not recorded")
	}
	dropped = nil
	deadline := time.Now().Add(10 * time.Second)
	for {
		runtime.GC()
		if _, ok := fixtureConns.Load(droppedKey); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a fixture connection nothing references was still marked 10 s after it could be collected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := fixtureConns.Load(keptKey); !ok {
		t.Fatal("a fixture connection the test still references lost its mark")
	}
	runtime.KeepAlive(kept)
}
