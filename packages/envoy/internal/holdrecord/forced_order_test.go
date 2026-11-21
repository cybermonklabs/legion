//go:build odatrace

package holdrecord_test

import (
	"context"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/testnats"
)

// heldFor is how long the test waits to see that something does not happen while the leader's
// apply is held: the barrier's return, which an unheld three-copy put answers in milliseconds.
const heldFor = 2 * time.Second

// Barrier returns only once the bucket stream's leader has applied every write committed before
// it, so a read right after it sees them. A write W is held at the leader's apply
// (testnats.StreamApply) with its acknowledgement outstanding: Barrier, on a goroutine of its own
// since its own entry queues behind the parked apply, does not return, and a STREAM.MSG.GET, which
// the leader answers, does not see W. Released, Barrier returns and the read sees W. A Barrier
// that returned before its acknowledgement would return while W is held, and the read after it
// would miss W.
func TestForcedOrderHoldJournalBarrier(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	hooks := testnats.Hook(t)
	conn := testnats.Connect(t, cluster.URL())
	t.Cleanup(conn.Close)
	const bucket, stream = "envoy_sessions", "KV_envoy_sessions"
	const written = "$KV." + bucket + ".w"
	cluster.CreateKeyValue(t, conn, &natsgo.KeyValueConfig{Bucket: bucket, Replicas: 3, Storage: natsgo.FileStorage})
	legacy, err := conn.JetStream(natsgo.MaxWait(30 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	info, err := legacy.StreamInfo(stream)
	if err != nil || info.Cluster == nil || cluster.Member(info.Cluster.Leader) < 0 {
		t.Fatalf("the bucket stream's leader: %+v, %v", info, err)
	}
	leader := cluster.Servers[cluster.Member(info.Cluster.Leader)]
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// (1) W, its acknowledgement not awaited; (2) its apply held at the leader.
	hold := hooks.Hold(testnats.StreamApply, testnats.OnServer(leader), testnats.Stream(stream), testnats.Subject(written))
	if _, err := js.PublishAsync(written, []byte("W")); err != nil {
		t.Fatalf("publish W: %v", err)
	}
	t.Logf("held %s", hold.Reached(t))

	// (3) Barrier on a goroutine of its own; (4) it has not returned, and the leader's read misses W.
	done := make(chan error, 1)
	go func() { done <- holdrecord.Barrier(ctx, js, bucket, "h-forced") }()
	select {
	case err := <-done:
		got, readErr := lastBySubject(t, conn, stream, written)
		t.Fatalf("Barrier returned (%v) while W's apply was held at the leader, and STREAM.MSG.GET then answers %q, %v for W",
			err, got, readErr)
	case <-time.After(heldFor):
	}
	if got, err := lastBySubject(t, conn, stream, written); err != nil || got != "" {
		t.Fatalf("while W's apply is held, STREAM.MSG.GET answers %q, %v for W, want no message", got, err)
	}

	// (5) Released; (6) Barrier returns, and the read sees W and the barrier's own put.
	hold.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Barrier after the release: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Barrier did not return within 30 s of the release")
	}
	if got, err := lastBySubject(t, conn, stream, written); err != nil || got != "W" {
		t.Fatalf("right after Barrier returned, STREAM.MSG.GET answers %q, %v for W, want W", got, err)
	}
	if got, err := lastBySubject(t, conn, stream, "$KV."+bucket+".hold.barrier.h-forced"); err != nil || got != "h-forced" {
		t.Fatalf("right after Barrier returned, STREAM.MSG.GET answers %q, %v for the barrier key", got, err)
	}
}
