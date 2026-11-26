package bus

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// The create of a bucket's stream that EnsureKeyValue makes one request at a time
// (createBucketStream) sends what nats.go's CreateKeyValue sends, request for request, since its
// stream configuration (bucketStreamConfig) is a copy of nats.go's. A second connection subscribes
// to the stream's create and update subjects, which every request to them reaches as well as the
// server, and the row compares what each create sent: nats.go's create first, which makes the
// stream, then createBucketStream's, which the server answers "stream name already in use" and
// which then reads the stream and updates it, then nats.go's again, which does the same. A config
// the server refuses ends at the create, and both answer the same error. A config nats.go refuses
// before its create (a history past nats.KeyValueMaxHistory) sends no create from either.
func TestABucketStreamIsCreatedAsNatsGoCreatesIt(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	conn, err := nats.Connect(uri)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(nats.MaxWait(RequestTimeout))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	watcher, err := nats.Connect(uri)
	if err != nil {
		t.Fatalf("connect the watcher: %v", err)
	}
	t.Cleanup(watcher.Close)
	start := time.Unix(1_700_000_000, 0)
	for index, row := range []struct {
		name   string
		config func(bucket string) *nats.KeyValueConfig
	}{
		{"the fields a start sets", func(bucket string) *nats.KeyValueConfig {
			return &nats.KeyValueConfig{Bucket: bucket, TTL: time.Hour, Storage: nats.MemoryStorage, Replicas: 1}
		}},
		{"every scalar field, a short age among them", func(bucket string) *nats.KeyValueConfig {
			return &nats.KeyValueConfig{Bucket: bucket, Description: "described", History: 5, TTL: 30 * time.Second, MaxBytes: 1 << 20, MaxValueSize: 1024, Storage: nats.FileStorage, Compression: true}
		}},
		{"a republish", func(bucket string) *nats.KeyValueConfig {
			return &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage, RePublish: &nats.RePublish{Source: ">", Destination: "republished." + bucket + ".>"}}
		}},
		{"a mirror", func(bucket string) *nats.KeyValueConfig {
			return &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage, Mirror: &nats.StreamSource{Name: bucket + "_origin", OptStartTime: &start}}
		}},
		{"sources", func(bucket string) *nats.KeyValueConfig {
			return &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage, Sources: []*nats.StreamSource{{Name: bucket + "_one"}, {Name: "KV_" + bucket + "_two"}}}
		}},
		{"a placement the server cannot meet", func(bucket string) *nats.KeyValueConfig {
			return &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage, Placement: &nats.Placement{Cluster: "nowhere"}}
		}},
		{"a history past the most nats.go takes", func(bucket string) *nats.KeyValueConfig {
			return &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage, History: nats.KeyValueMaxHistory + 1}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			bucket := fmt.Sprintf("envoy_create_as_natsgo_%d", index)
			stream := "KV_" + bucket
			creates, err := watcher.SubscribeSync(jsAPI("STREAM.CREATE", stream))
			if err != nil {
				t.Fatalf("watch the stream's creates: %v", err)
			}
			updates, err := watcher.SubscribeSync(jsAPI("STREAM.UPDATE", stream))
			if err != nil {
				t.Fatalf("watch the stream's updates: %v", err)
			}
			if err := watcher.Flush(); err != nil {
				t.Fatalf("flush the watcher: %v", err)
			}
			t.Cleanup(func() { _ = js.DeleteStream(stream) })

			_, natsFirst := js.CreateKeyValue(row.config(bucket))
			ours := createBucketStream(conn, js, row.config(bucket))
			_, natsSecond := js.CreateKeyValue(row.config(bucket))

			if natsFirst == nil {
				// The stream is there, so both later creates are answered "stream name already in
				// use", and each reads the stream and, when it holds the configuration asked for,
				// updates it to that configuration: both update it or neither does.
				if !sameErrorText(ours, natsSecond) {
					t.Fatalf("createBucketStream over the stream nats.go created = %v, want nats.go's own second create's answer, %v", ours, natsSecond)
				}
				requireSameRequests(t, "create", creates, 3)
				requireSameRequests(t, "update", updates, 0, 2)
				return
			}
			if !sameErrorText(ours, natsFirst) || !sameErrorText(natsSecond, natsFirst) {
				t.Fatalf("the creates nats.go refused answered nats.go %v and %v, createBucketStream %v, want one answer", natsFirst, natsSecond, ours)
			}
			if errors.Is(natsFirst, nats.ErrHistoryToLarge) {
				requireSameRequests(t, "create", creates, 0)
				return
			}
			requireSameRequests(t, "create", creates, 3)
		})
	}

	t.Run("a connection that has not connected", func(t *testing.T) {
		client, err := Connect([]string{"nats://127.0.0.1:1"}, WithEnvironment(seedLookup("")), WithConnectInBackground())
		if err != nil {
			t.Fatalf("connect in the background: %v", err)
		}
		t.Cleanup(client.Close)
		unconnected, err := client.Conn.JetStream()
		if err != nil {
			t.Fatalf("open JetStream: %v", err)
		}
		config := &nats.KeyValueConfig{Bucket: "envoy_create_unconnected"}
		_, natsErr := unconnected.CreateKeyValue(config)
		if ours := createBucketStream(client.Conn, unconnected, config); natsErr == nil || !sameErrorText(ours, natsErr) {
			t.Fatalf("a create on a connection that has not connected answered nats.go %v, createBucketStream %v, want the same refusal", natsErr, ours)
		}
	})
}

// The stream configuration of a bucket takes DiscardNew only from a server that reports 2.7.2 or
// later, as nats.go's CreateKeyValue does (nats.go@v1.55.0 kv.go:520-523): the server the row
// above sends both creates to reports 2.15, so these are the versions it cannot.
func TestABucketStreamDiscardsNewOnlyFromServersThatTakeIt(t *testing.T) {
	t.Parallel()
	for version, want := range map[string]nats.DiscardPolicy{
		"2.15.1": nats.DiscardNew,
		"2.7.2":  nats.DiscardNew,
		"2.7.1":  nats.DiscardOld,
		"":       nats.DiscardOld,
	} {
		stream, err := bucketStreamConfig(&nats.KeyValueConfig{Bucket: "envoy_discard"}, version)
		if err != nil {
			t.Fatalf("the stream configuration on a server reporting %q: %v", version, err)
		}
		if stream.Discard != want {
			t.Errorf("the stream configuration on a server reporting %q discards %v, want %v", version, stream.Discard, want)
		}
	}
}

// sameErrorText reports whether a and b are both nil or carry the same text.
func sameErrorText(a, b error) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Error() == b.Error()
}

// requireSameRequests reads every request requests receives until none arrives for a second, and
// fails the row unless their number is one of counts and their payloads are equal.
func requireSameRequests(t *testing.T, kind string, requests *nats.Subscription, counts ...int) {
	t.Helper()
	var sent [][]byte
	for {
		message, err := requests.NextMsg(time.Second)
		if errors.Is(err, nats.ErrTimeout) {
			break
		}
		if err != nil {
			t.Fatalf("read the %s requests: %v", kind, err)
		}
		sent = append(sent, message.Data)
	}
	if !slices.Contains(counts, len(sent)) {
		t.Fatalf("%d %s requests were sent, want one of %v", len(sent), kind, counts)
	}
	for index := 1; index < len(sent); index++ {
		if !bytes.Equal(sent[index], sent[0]) {
			t.Fatalf("%s request %d sent\n%s\nwhere request 1 sent\n%s", kind, index+1, sent[index], sent[0])
		}
	}
}
