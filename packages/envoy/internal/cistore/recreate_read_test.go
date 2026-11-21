package cistore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The order TestARecreatedBucketForgetsTheOldBucketsRecords met on a loaded runner, held here so
// it runs the same way every time: the cache is read while the recreated bucket's read has not
// completed. The read cannot complete while the stream's leader refuses its reads of the old
// bucket's records: the connection's grant leaves out the stream's message get (STREAM.MSG.GET),
// which those reads send, and the read fails and is read again later. So the cache keeps the old
// bucket's three records beside the new bucket's one. The new bucket's record is stored before
// the rewatch, so the read's scan delivers it and the leader is asked only about the old three.
// The grant then allows the message get, the read after the refused one completes, and the
// cache then holds only the new bucket's record.
func TestTheCacheOfARecreatedBucketHoldsOnlyItsRecordsOnceItsReadCompletes(t *testing.T) {
	stream := "KV_" + bus.CIStateBucket
	granted := []string{
		"_INBOX.>",
		"$JS.API.INFO",
		"$JS.API.STREAM.INFO." + stream,
		"$JS.API.STREAM.CREATE." + stream,
		"$JS.API.STREAM.UPDATE." + stream,
		"$JS.API.STREAM.DELETE." + stream,
		"$JS.API.DIRECT.GET." + stream,
		"$JS.API.DIRECT.GET." + stream + ".>",
		"$JS.API.CONSUMER.CREATE." + stream,
		"$JS.API.CONSUMER.CREATE." + stream + ".>",
		"$JS.API.CONSUMER.DELETE." + stream + ".>",
		"$JS.API.CONSUMER.INFO." + stream + ".>",
		"$JS.FC." + stream + ".>",
		"$KV." + bus.CIStateBucket + ".>",
	}
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, granted...)
	conn, err := testnats.ConnectSeed(t, grant.URL, seed)
	if err != nil {
		t.Fatalf("connect as the user whose grant leaves out the message get: %v", err)
	}
	t.Cleanup(conn.Close)
	store := openStore(t, conn)
	past := time.Now().Add(-time.Hour).UnixMilli()
	for _, old := range []head{
		{"example-org", "example-repo", "70", "01d070000000000000000000000000000000beef"},
		{"example-org", "example-repo", "71", "01d071000000000000000000000000000000beef"},
		{"example-org", "example-repo", "72", "9e0e72000000000000000000000000000000beef"},
	} {
		putHeadGatedRecord(t, store, old, past, nil)
	}

	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.CIStateBucket); err != nil {
		t.Fatalf("delete the bucket: %v", err)
	}
	testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.CIStateBucket, Replicas: 1, Storage: natsgo.FileStorage, TTL: time.Hour})
	fresh := head{"example-org", "example-repo", "73", "f2e573000000000000000000000000000000beef"}
	raw, err := json.Marshal(State{Owner: fresh.owner, Repo: fresh.repo, Number: fresh.number, SHA: fresh.sha, LastEventAt: past})
	if err != nil {
		t.Fatalf("encode the new bucket's record: %v", err)
	}
	if _, err := store.watcher.KV().Put(fresh.key(), raw); err != nil {
		t.Fatalf("store the new bucket's record: %v", err)
	}
	if err := store.Rewatch(context.Background(), conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	waitCached(t, store, fresh.key(), func(State) bool { return true })
	if store.watcher.Following() {
		t.Fatal("the recreated bucket's read completed while the leader's reads were refused, so this row did not hold the order it names")
	}
	if cached := store.List(); len(cached) != 4 {
		t.Fatalf("the cache during the recreated bucket's read holds %d records, want the old bucket's 3 kept beside the new one", len(cached))
	}

	grant.Grant(t, append(granted, "$JS.API.STREAM.MSG.GET."+stream)...)
	testwait.Eventually(t, "the recreated bucket's read to complete", func() bool { return store.watcher.Following() })
	if cached := store.List(); len(cached) != 1 {
		t.Fatalf("the cache after the recreate holds %d records, want only the new bucket's %s", len(cached), fresh.key())
	}
}
