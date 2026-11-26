package bus

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/testnats"
)

// TestASessionStreamStateReadWhoseFirstReplyIsLostIsSentAgain proves that one shared STREAM.INFO
// read does not fail every SessionConsumers.streamState waiter. The proxy drops the first answer
// after the server sent it, as nats-server's service-import response-prefix race does.
func TestASessionStreamStateReadWhoseFirstReplyIsLostIsSentAgain(t *testing.T) {
	t.Parallel()
	_, uri := testnats.Start(t)
	owner, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the stream owner: %v", err)
	}
	t.Cleanup(owner.Close)

	armed := make(chan struct{})
	proxy := testnats.StartProxy(t, uri, testnats.DropFirstReplyForRequests("$JS.API.STREAM.INFO."+Stream, armed))
	consumers, err := ConnectSessionConsumers([]string{proxy.URL()}, 0, nil)
	if err != nil {
		t.Fatalf("connect session consumers through the reply-dropping proxy: %v", err)
	}
	t.Cleanup(consumers.Close)
	if dropped := proxy.DroppedReplies(); dropped != 0 {
		t.Fatalf("the STREAM.INFO reply drop fired %d times during setup, want none", dropped)
	}
	close(armed)

	// Hold the reader off until both calls have joined its one streamRead, so a success proves the
	// reply retry served the whole shared cohort and not merely one later call.
	consumers.streamMu.Lock()
	consumers.streamReading = true
	consumers.streamMu.Unlock()
	first := consumers.joinStreamRead()
	second := consumers.joinStreamRead()
	if first != second {
		t.Fatal("two stream-state waiters joined different reads")
	}
	go consumers.readStreamStates()
	for waiter, read := range map[string]*streamRead{"first": first, "second": second} {
		state, err := read.wait(context.Background())
		if err != nil {
			t.Fatalf("%s shared STREAM.INFO read after its first reply was lost: %v", waiter, err)
		}
		if state.id == "" {
			t.Fatalf("%s shared STREAM.INFO read after its first reply was lost returned no stream id", waiter)
		}
	}
	if dropped := proxy.DroppedReplies(); dropped != 1 {
		t.Fatalf("the STREAM.INFO reply drop fired %d times, want once", dropped)
	}
}

// replyDroppingBucket opens bucket through a proxy that drops the first STREAM.MSG.GET response.
// The bucket and its key are prepared over another connection before the proxy starts, so the drop
// can only take the read this test is about.
func replyDroppingBucket(t *testing.T, bucket string, options ...ConnectOption) (*testnats.Proxy, KeyValue) {
	t.Helper()
	uri, prepared := leaderReadBucket(t, bucket)
	if _, err := prepared.Put("ses_live", []byte("live")); err != nil {
		t.Fatalf("put the key the leader read needs: %v", err)
	}
	armed := make(chan struct{})
	proxy := testnats.StartProxy(t, uri, testnats.DropFirstReplyForRequests("$JS.API.STREAM.MSG.GET.KV_"+bucket, armed))
	client, err := Connect([]string{proxy.URL()}, options...)
	if err != nil {
		t.Fatalf("connect the KV reader through the reply-dropping proxy: %v", err)
	}
	t.Cleanup(client.Close)
	legacy, err := client.JS().KeyValue(bucket)
	if err != nil {
		t.Fatalf("open KV bucket %s: %v", bucket, err)
	}
	kv, err := WrapKeyValue(client.Connection(), legacy)
	if err != nil {
		t.Fatalf("wrap KV bucket %s: %v", bucket, err)
	}
	close(armed)
	return proxy, kv
}

// TestAKVLeaderReadWhoseFirstReplyIsLostIsSentAgain proves that the leader-only STREAM.MSG.GET
// retries a lost reply. The read changes no stream state, so the second request is idempotent.
func TestAKVLeaderReadWhoseFirstReplyIsLostIsSentAgain(t *testing.T) {
	t.Parallel()
	const bucket = "kv_leader_lost_reply"
	proxy, kv := replyDroppingBucket(t, bucket)

	entry, err := kv.GetFromLeader(context.Background(), "ses_live")
	if dropped := proxy.DroppedReplies(); dropped != 1 {
		t.Fatalf("the leader-read reply drop fired %d times, want once", dropped)
	}
	if err != nil {
		t.Fatalf("GetFromLeader after its first reply was lost: %v", err)
	}
	if got := string(entry.Value()); got != "live" {
		t.Fatalf("GetFromLeader after its first reply was lost = %q, want %q", got, "live")
	}
}

// TestAnEdgeKVReadWhoseFirstReplyIsLostIsSentAgain proves the stream-message reader used by edge
// listeners retries the same idempotent read. The edge has no direct-get grant, so this request is
// the exposed STREAM.MSG.GET form.
func TestAnEdgeKVReadWhoseFirstReplyIsLostIsSentAgain(t *testing.T) {
	t.Parallel()
	const bucket = "kv_edge_lost_reply"
	proxy, kv := replyDroppingBucket(t, bucket, WithStreamMessageReads())

	entry, err := kv.Get("ses_live")
	if dropped := proxy.DroppedReplies(); dropped != 1 {
		t.Fatalf("the edge-read reply drop fired %d times, want once", dropped)
	}
	if err != nil {
		t.Fatalf("edge Get after its first reply was lost: %v", err)
	}
	if got := string(entry.Value()); got != "live" {
		t.Fatalf("edge Get after its first reply was lost = %q, want %q", got, "live")
	}
}
