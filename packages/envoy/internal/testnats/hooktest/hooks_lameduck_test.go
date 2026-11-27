//go:build odatrace

package hooktest

import (
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A lame duck is a shutdown too: nats-server v2.15.1's lameDuckMode shuts JetStream down, which
// waits for each clustered stream's monitor goroutine (stream.go stop, stopMonitoring), before it
// calls Shutdown (server.go:4516, 4608). A committed entry held at stream.apply parks that
// goroutine, so a hold the hook released only at Shutdown's server.shutdown would never be
// released, and testnats.Server.LameDuck would give up after its 30 s connectTimeout, force a
// Shutdown and return an error. The lame duck of a server with a hold parked must return, as its
// Stop does.
//
// The lame duck runs on a goroutine of its own and the test waits stopWithin for it. Without any
// release (forced-order.sh -drop server.shutdown) the forced Shutdown waits for the parked
// goroutine for ever, so a direct call would hold the test until go test's -timeout. On that
// bound the test releases the hold itself, waits for the lame duck to end so the cleanups do not
// remove its store under it, and fails.
func TestForcedOrderALameDuckReleasesTheHoldOnItsServer(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	hooks := testnats.Hook(t)
	conn := testnats.Connect(t, cluster.URL())
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(natsgo.MaxWait(reachWithin))
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	if _, err := js.AddStream(&natsgo.StreamConfig{Name: "LAME", Subjects: []string{"lame.>"}, Replicas: 3}); err != nil {
		t.Fatalf("create the stream: %v", err)
	}
	if _, err := js.Publish("lame.first", nil); err != nil {
		t.Fatalf("the unheld publish: %v", err)
	}
	info, err := js.StreamInfo("LAME")
	if err != nil || info.Cluster == nil || cluster.Member(info.Cluster.Leader) < 0 {
		t.Fatalf("the stream's leader: %+v, %v", info, err)
	}
	leader := cluster.Servers[cluster.Member(info.Cluster.Leader)]

	hold := hooks.Hold(testnats.StreamApply, testnats.OnServer(leader), testnats.Stream("LAME"), testnats.Subject("lame.second"))
	if _, err := js.PublishAsync("lame.second", nil); err != nil {
		t.Fatalf("publish the held message: %v", err)
	}
	t.Logf("held %s", hold.Reached(t))
	start := time.Now()
	returned := make(chan error, 1)
	go func() { returned <- leader.LameDuck() }()
	select {
	case err := <-returned:
		took := time.Since(start)
		if err != nil {
			t.Fatalf("the lame duck of a server with a hold parked failed after %s: %v", took.Round(time.Millisecond), err)
		}
		if took > stopWithin {
			t.Fatalf("the lame duck of a server with a hold parked took %s, more than %s", took.Round(time.Millisecond), stopWithin)
		}
	case <-time.After(stopWithin):
		released := hold.Released()
		hold.Release()
		select {
		case <-returned:
		case <-time.After(stopWithin):
			t.Errorf("the lame duck did not end within %s of the test's own release either", stopWithin)
		}
		t.Fatalf("the lame duck of a server with a hold parked did not return within %s (hold released by then: %t)", stopWithin, released)
	}
	if !hold.Released() {
		t.Fatal("the lame duck returned and the hold is not released")
	}
}
