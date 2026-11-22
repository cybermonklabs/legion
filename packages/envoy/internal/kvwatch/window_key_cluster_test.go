package kvwatch_test

import (
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestAReadHoldsAWindowKeyOnlyAnotherWriterWrote's order on a real three-member cluster, with the
// bucket's stream on all three: the watcher reads through one member and the other listener
// writes through another. The read's list of the bucket's keys is the stream leader's
// (bus.KeyValue.StoredKeys), which answers whichever member the request reaches, so the read
// completes holding K, written only by the other listener.
func TestAReadOnAClusterHoldsAWindowKeyOnlyAnotherWriterWrote(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	cluster.AwaitReady(t)
	_, kv := clusterBucket(t, cluster.Servers[0].URL())
	_, other := clusterBucket(t, cluster.Servers[1].URL())
	readHoldsAWindowKeyOnlyAnotherWriterWrote(t, kv, other)
}

// clusterBucket opens the test bucket, with its stream on three members, on a connection of its
// own to uri.
func clusterBucket(t *testing.T, uri string) (*natsgo.Conn, bus.KeyValue) {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(natsgo.MaxWait(10 * time.Second))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := bus.EnsureKeyValue(t.Context(), conn, js, &natsgo.KeyValueConfig{Bucket: testBucketName, Replicas: 3, Storage: natsgo.MemoryStorage})
	if err != nil {
		t.Fatalf("bucket: %v", err)
	}
	return conn, kv
}
