package bus

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// WriteBarrier writes a delete marker to the barrier key and answers the sequence it was committed
// at: a read of the key finds no value, and a second barrier is committed past the first.
func TestWriteBarrierCommitsADeleteMarker(t *testing.T) {
	_, kv := leaderReadBucket(t, "write-barrier")
	written, err := kv.Put("live", []byte("1"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	first, err := kv.WriteBarrier(context.Background())
	if err != nil {
		t.Fatalf("WriteBarrier: %v", err)
	}
	if first <= written || barrierRevision(t, kv) != first {
		t.Fatalf("the barrier was acknowledged at %d and the bucket holds it at %d, want both past the put at %d", first, barrierRevision(t, kv), written)
	}
	if _, err := kv.legacy.Get(BarrierKey); !errors.Is(err, nats.ErrKeyNotFound) {
		t.Fatalf("a get of the barrier key = %v, want %v: the barrier is a delete marker", err, nats.ErrKeyNotFound)
	}
	second, err := kv.WriteBarrier(context.Background())
	if err != nil || second <= first {
		t.Fatalf("a second WriteBarrier = %d, %v; want a sequence past the first's %d", second, err, first)
	}
}

// On a three-copy bucket whose other two members have stopped, the member left cannot commit a
// barrier, so WriteBarrier fails rather than answer a sequence.
func TestWriteBarrierFailsOnAMemberCutOffFromTheOthers(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	cluster.AwaitReady(t)
	client, err := Connect(cluster.URLs())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := EnsureKeyValue(t.Context(), client.Conn, js, &nats.KeyValueConfig{Bucket: "write-barrier-cut-off", Replicas: 3, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	if _, err := kv.Put("held", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	connected := cluster.Member(client.Conn.ConnectedServerName())
	for i := range cluster.Servers {
		if i != connected {
			cluster.Servers[i].Stop(t)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if sequence, err := kv.WriteBarrier(ctx); err == nil || sequence != 0 {
		t.Fatalf("WriteBarrier on the member left = %d, %v; want no sequence and an error", sequence, err)
	}
}

// Every call through a KeyValue refuses BarrierKey before it sends anything, as a refusal
// (ErrReservedKey, an ErrRefused): no store writes, deletes or reads a record under it, so no
// record a store keeps can be the barrier, and none can be removed by it.
func TestEveryKeyValueCallRefusesTheBarrierKey(t *testing.T) {
	_, kv := leaderReadBucket(t, "write-barrier-reserved")
	refusals := map[string]error{
		"Get":         second(kv.Get(BarrierKey)),
		"RefusesRead": kv.RefusesRead(BarrierKey),
		"GetRevision": second(kv.GetRevision(BarrierKey, 1)),
		"Put":         second(kv.Put(BarrierKey, []byte("1"))),
		"PutString":   second(kv.PutString(BarrierKey, "1")),
		"Create":      second(kv.Create(BarrierKey, []byte("1"))),
		"Update":      second(kv.Update(BarrierKey, []byte("1"), 1)),
		"Delete":      kv.Delete(BarrierKey),
		"DeleteAtRead": func() error {
			_, err := kv.DeleteAtRead(BarrierKey)
			return err
		}(),
		"Purge":             kv.Purge(BarrierKey),
		"WatchKeys":         second(kv.WatchKeys(context.Background(), BarrierKey)),
		"WatchFilteredKeys": second(kv.WatchFilteredKeys(context.Background(), []string{"other", BarrierKey})),
		"GetFromLeader":     second(kv.GetFromLeader(context.Background(), BarrierKey)),
		"ReadHistory":       second(kv.ReadHistory(context.Background(), BarrierKey)),
	}
	for call, err := range refusals {
		if !errors.Is(err, ErrReservedKey) || !errors.Is(err, ErrRefused) {
			t.Errorf("%s of the barrier key = %v, want %v (an ErrRefused)", call, err, ErrReservedKey)
		}
	}
	if revision := barrierRevisionOrZero(t, kv); revision != 0 {
		t.Fatalf("the bucket holds a barrier record at %d after calls that refuse its key, want none", revision)
	}
	if !strings.HasPrefix(BarrierKey, "_") || !natsKeyValid(BarrierKey) {
		t.Fatalf("the barrier key %q must begin with '_', which no role matches, and be a key nats.go accepts", BarrierKey)
	}
}

// A read's barrier and its list of the bucket's keys that the connection's grant refuses end at
// once with the server's refusal, an error naming ErrPublishDenied and the refused subject, rather
// than after RequestTimeout with a bare "context deadline exceeded": the server answers a refused
// request with nothing and reports the refusal on the connection, and both calls read that report
// as it arrives (refusedOn). The bucket is opened on a client whose grant never covers its keys'
// subjects, so the barrier is refused; once it is open, the grant also drops the STREAM.INFO of the
// bucket's stream that the open needed, so the list's page is refused too.
func TestABarrierAndAKeyListTheGrantRefusesEndAtOnceNamingTheRefusal(t *testing.T) {
	const bucket = "envoy_denied_barrier"
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, "_INBOX.>", accountInfoSubject(), openSubject(bucket), createSubject(bucket))
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := Connect([]string{grant.URL})
	if err != nil {
		t.Fatalf("connect the granted client: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream(nats.MaxWait(RequestTimeout))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	kv, err := EnsureKeyValue(t.Context(), client.Conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	grant.Grant(t, "_INBOX.>", accountInfoSubject())
	for _, row := range []struct {
		name    string
		refused string
		call    func() error
	}{
		{"barrier", "$KV." + bucket + "." + BarrierKey, func() error { _, err := kv.WriteBarrier(t.Context()); return err }},
		{"key list", openSubject(bucket), func() error { _, err := kv.StoredKeys(t.Context(), 0); return err }},
	} {
		t.Run(row.name, func(t *testing.T) {
			started := time.Now()
			err := row.call()
			took := time.Since(started)
			if !errors.Is(err, ErrPublishDenied) {
				t.Fatalf("a %s the grant refuses = %v (after %s), want ErrPublishDenied", row.name, err, took)
			}
			if want := `Permissions Violation for Publish to "` + row.refused + `"`; !strings.Contains(err.Error(), want) {
				t.Fatalf("the refused %s = %v, want it to name %q", row.name, err, want)
			}
			if took > refusalAnswerBound {
				t.Fatalf("the refused %s ended after %s, want within %s, far below RequestTimeout's %s", row.name, took, refusalAnswerBound, RequestTimeout)
			}
		})
	}
}

func second[T any](_ T, err error) error { return err }

// barrierRevision is the revision of the bucket's latest barrier record.
func barrierRevision(t *testing.T, kv KeyValue) uint64 {
	t.Helper()
	revision := barrierRevisionOrZero(t, kv)
	if revision == 0 {
		t.Fatal("the bucket holds no barrier record")
	}
	return revision
}

// barrierRevisionOrZero is the revision of the bucket's latest barrier record, 0 when it holds
// none.
func barrierRevisionOrZero(t *testing.T, kv KeyValue) uint64 {
	t.Helper()
	js, err := kv.streams.Conn().JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	message, err := js.GetLastMsg("KV_"+kv.Bucket(), "$KV."+kv.Bucket()+"."+BarrierKey)
	if errors.Is(err, nats.ErrMsgNotFound) {
		return 0
	}
	if err != nil {
		t.Fatalf("read the barrier record: %v", err)
	}
	return message.Sequence
}
