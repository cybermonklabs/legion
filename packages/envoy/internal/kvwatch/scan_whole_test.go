package kvwatch_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestAScanVisitsAListedKeyItsWatchLeftOut checks that both scans recover an omitted live key at
// its stored revision and read no other key from the leader. A scan that ended at the watch's
// nil entry would never visit the omitted key.
func TestAScanVisitsAListedKeyItsWatchLeftOut(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name    string
		filters []string
	}{
		{name: "existing"},
		{name: "filtered", filters: []string{"left.*", "right.*"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			kv := wholeScanBucket(t, "scan_visits_omitted")
			const omitted = "right.b"
			want := putScanKeys(t, kv, "left.a", omitted, "left.c")
			putKeys(t, kv, "right.deleted")
			if err := kv.Delete("right.deleted"); err != nil {
				t.Fatalf("delete right.deleted: %v", err)
			}
			handle, leader := scanLeavingOut(kv, omitted, nil)

			got := make(scanVisits)
			scan, err := runWholeScan(testwait.Context(t), handle, row.filters, "visit listed keys", got)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if !maps.Equal(got, want) {
				t.Errorf("scan visited %v, want %v, including the omitted key %q at its put revision", got, want, omitted)
			}
			if scan.Puts != 3 || scan.Markers != 1 {
				t.Errorf("scan counted %+v, want 3 puts (including %q) and 1 marker", scan, omitted)
			}
			if !slices.Equal(leader.keys, []string{omitted}) {
				t.Errorf("leader reads = %q, want one read of %q", leader.keys, omitted)
			}
		})
	}
}

// TestAScanLeavesOutAListedKeyTheLeaderNoLongerHolds checks that a listed key deleted before its
// leader read is not visited or counted as a put.
func TestAScanLeavesOutAListedKeyTheLeaderNoLongerHolds(t *testing.T) {
	t.Parallel()
	kv := wholeScanBucket(t, "scan_deleted_before_read")
	want := putScanKeys(t, kv, "a", "c")
	const omitted = "b"
	putKeys(t, kv, omitted)
	handle, leader := scanLeavingOut(kv, omitted, func() error { return kv.Delete(omitted) })

	got := make(scanVisits)
	scan, err := runWholeScan(testwait.Context(t), handle, nil, "visit surviving keys", got)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !maps.Equal(got, want) || scan.Puts != 2 {
		t.Errorf("scan visited %v and counted %d puts, want %v and 2 puts", got, scan.Puts, want)
	}
	if !slices.Contains(leader.keys, omitted) {
		t.Errorf("leader reads = %q, want a read of the omitted key %q", leader.keys, omitted)
	}
}

// TestAScanWhoseLeaderReadFailsNamesTheKey checks that a failed or refused read names the scan and
// the omitted key and wraps the read error.
func TestAScanWhoseLeaderReadFailsNamesTheKey(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		legacy bool
	}{
		{name: "leader read fails"},
		{name: "legacy key is refused", legacy: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			const bucket = "scan_failed_leader_read"
			const what = "visit keys with a failed leader read"
			kv := wholeScanBucket(t, bucket)
			ctx := testwait.Context(t)
			key := "b"
			wantErr := errors.New("the leader did not answer")
			beforeRead := func() error { return wantErr }
			if row.legacy {
				_, key = testnats.LegacyKeys(bucket, "u")
				if _, err := kv.Streams().Publish(ctx, testnats.KVSubject(bucket, key), []byte("legacy")); err != nil {
					t.Fatalf("publish the %d-byte legacy key: %v", len(key), err)
				}
				wantErr = bus.ErrRefused
				beforeRead = nil
			} else {
				putKeys(t, kv, key)
			}
			handle, _ := scanLeavingOut(kv, key, beforeRead)

			got := make(scanVisits)
			_, err := runWholeScan(ctx, handle, nil, what, got)
			if _, visited := got[key]; visited {
				t.Errorf("scan visited the omitted %d-byte key even though its leader read should fail", len(key))
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("scan error for the omitted %d-byte key = %v, want it to wrap %v", len(key), err, wantErr)
			}
			if text := err.Error(); !strings.HasPrefix(text, what+": ") || !strings.Contains(text, key) {
				t.Errorf("scan error %q must start with %q and name the omitted %d-byte key", text, what+": ", len(key))
			}
		})
	}
}

// TestAScanKeepsAKeyWrittenAgainDuringIt checks that every scan visits both keys while one is
// continuously rewritten. A rewrite can move that key past the watch's first delivery.
func TestAScanKeepsAKeyWrittenAgainDuringIt(t *testing.T) {
	t.Parallel()
	const scans = 100
	const rewritten = "written-again"
	keys := []string{"steady", rewritten}
	kv := wholeScanBucket(t, "scan_written_again")
	putKeys(t, kv, keys...)
	ctx := testwait.Context(t)

	stop, stopped := make(chan struct{}), make(chan struct{})
	var writes int
	var writeErr error
	go func() {
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, writeErr = kv.Put(rewritten, []byte("2")); writeErr != nil {
				return
			}
			writes++
		}
	}()
	defer func() {
		close(stop)
		testwait.Receive(t, "the writer of written-again to stop", stopped)
		if writeErr != nil {
			t.Errorf("rewrite %q: %v", rewritten, writeErr)
		}
		if writes == 0 {
			t.Errorf("no writes to %q while the scans ran", rewritten)
		}
	}()

	for i := 1; i <= scans; i++ {
		got := make(scanVisits)
		if _, err := runWholeScan(ctx, kv, nil, "visit a key being rewritten", got); err != nil {
			t.Fatalf("scan %d of %d: %v", i, scans, err)
		}
		if len(got) != len(keys) || got["steady"] == 0 || got[rewritten] == 0 {
			t.Errorf("scan %d of %d visited %v, want both %q", i, scans, got, keys)
		}
	}
}

// TestAScanVisitsAKeyDeletedAndWrittenAgainDuringIt checks that both scans hand over a listed key
// that another client deletes after the list and writes again before the watch's first delivery
// ends, so the bucket holds the key when the scan starts and when it returns. The first delivery
// carries the key's delete marker, and its new put comes after the nil entry: a scan that takes a
// delivered marker as the key's newest message leaves the key out.
func TestAScanVisitsAKeyDeletedAndWrittenAgainDuringIt(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name    string
		filters []string
	}{
		{name: "existing"},
		{name: "filtered", filters: []string{"left.*", "right.*"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			kv := wholeScanBucket(t, "scan_deleted_and_written_again")
			const flapping = "right.flapping"
			putScanKeys(t, kv, "left.steady", flapping)
			handle := bus.NewKeyValue(kv.Legacy(), deleteThenRewrite{KeyValue: kv.WatchHandle(), kv: kv, key: flapping, t: t}, kv.Streams())

			got := make(scanVisits)
			scan, err := runWholeScan(testwait.Context(t), handle, row.filters, "visit a key deleted and written again", got)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			held, err := kv.Get(flapping)
			if err != nil {
				t.Fatalf("read %s after the scan: %v", flapping, err)
			}
			if got[flapping] == 0 {
				t.Errorf("scan counted %+v and visited %v, without %q, which the bucket held when the scan listed it and holds at revision %d after the scan returned",
					scan, got, flapping, held.Revision())
			}
		})
	}
}

// deleteThenRewrite deletes key when the scan makes its watch, after the scan listed the bucket,
// and writes key again once the watch's first delivery has ended, before it forwards the nil entry
// that ends that delivery.
type deleteThenRewrite struct {
	jetstream.KeyValue
	kv  bus.KeyValue
	key string
	t   *testing.T
}

func (k deleteThenRewrite) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	k.deleteKey()
	return k.forward(k.KeyValue.Watch(ctx, keys, opts...))
}

func (k deleteThenRewrite) WatchFiltered(ctx context.Context, keys []string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	k.deleteKey()
	return k.forward(k.KeyValue.WatchFiltered(ctx, keys, opts...))
}

func (k deleteThenRewrite) deleteKey() {
	if err := k.kv.Delete(k.key); err != nil {
		k.t.Errorf("delete %s: %v", k.key, err)
	}
}

func (k deleteThenRewrite) forward(watcher jetstream.KeyWatcher, err error) (jetstream.KeyWatcher, error) {
	if err != nil {
		return nil, err
	}
	updates := make(chan jetstream.KeyValueEntry)
	go func() {
		defer close(updates)
		initial := true
		for entry := range watcher.Updates() {
			if entry == nil && initial {
				initial = false
				if _, err := k.kv.Put(k.key, []byte("again")); err != nil {
					k.t.Errorf("put %s again: %v", k.key, err)
				}
			}
			updates <- entry
		}
	}()
	return &forwardedWatcher{KeyWatcher: watcher, updates: updates}, nil
}

// TestAScanReadsNoDeletedKeyFromTheLeader checks that both scans count 10,000 delete markers and
// visit the live keys without reading any key individually.
func TestAScanReadsNoDeletedKeyFromTheLeader(t *testing.T) {
	t.Parallel()
	const bucket = "scan_many_markers"
	const deleted, live = 10_000, 20
	kv := wholeScanBucket(t, bucket)
	uri := testnats.URL(t)
	seedScanMarkers(t, kv, deleted)
	keys := make([]string, live)
	for i := range keys {
		keys[i] = fmt.Sprintf("live.%02d", i)
	}
	want := putScanKeys(t, kv, keys...)

	for _, row := range []struct {
		name    string
		filters []string
	}{
		{name: "existing"},
		{name: "filtered", filters: []string{"deleted.*", "live.*"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			inbox := scanGetInboxPrefix + row.name
			conn, err := testnats.AwaitConnectSeed(t, uri, "", natsgo.CustomInboxPrefix(inbox))
			if err != nil {
				t.Fatalf("connect to the scan bucket's account: %v", err)
			}
			t.Cleanup(conn.Close)
			ctx := testwait.Context(t)
			kv, err := bus.OpenKeyValue(ctx, conn, jetStream(t, conn), bucket)
			if err != nil {
				t.Fatalf("open bucket %s: %v", bucket, err)
			}

			stream := "KV_" + kv.Bucket()
			gets := []string{"$JS.API.STREAM.MSG.GET." + stream, "$JS.API.DIRECT.GET." + stream, "$JS.API.DIRECT.GET." + stream + ".>"}
			subs := make([]*natsgo.Subscription, 0, len(gets))
			for _, subject := range gets {
				sub, err := conn.SubscribeSync(subject)
				if err != nil {
					t.Fatalf("subscribe to %s: %v", subject, err)
				}
				t.Cleanup(func() { _ = sub.Unsubscribe() })
				subs = append(subs, sub)
			}

			got := make(scanVisits)
			scan, err := runWholeScan(ctx, kv, row.filters, "visit live keys among markers", got)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if !maps.Equal(got, want) || scan.Puts != live || scan.Markers != deleted {
				t.Errorf("scan visited %v and counted %+v, want %v, %d puts and %d markers", got, scan, want, live, deleted)
			}
			assertNoScanGets(t, conn, subs, inbox)
		})
	}
}

func wholeScanBucket(t *testing.T, name string) bus.KeyValue {
	t.Helper()
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	kv, err := bus.EnsureKeyValue(testwait.Context(t), conn, js, &natsgo.KeyValueConfig{Bucket: name, Storage: natsgo.MemoryStorage})
	if err != nil {
		t.Fatalf("create bucket %s: %v", name, err)
	}
	return kv
}

func putScanKeys(t *testing.T, kv bus.KeyValue, keys ...string) map[string]uint64 {
	t.Helper()
	revisions := make(map[string]uint64, len(keys))
	for _, key := range keys {
		revision, err := kv.Put(key, []byte(key))
		if err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		revisions[key] = revision
	}
	return revisions
}

type scanVisits map[string]uint64

func (v scanVisits) visit(entry kvwatch.Entry) error {
	v[entry.Key] = entry.Revision
	return nil
}

func runWholeScan(ctx context.Context, kv bus.KeyValue, filters []string, what string, visits scanVisits) (kvwatch.KeyScan, error) {
	if len(filters) == 0 {
		return kvwatch.ScanExistingKeys(ctx, kv, jetstream.AllKeys, what, kvwatch.ScanIdleTimeout, slog.Default(), visits.visit)
	}
	return kvwatch.ScanFilteredKeys(kv, filters, what, kvwatch.ScanIdleTimeout, slog.Default(), visits.visit)
}

// leaveOut drops key only from the watch's initial delivery. The scan can still stop the real
// watcher and drain all updates that follow the nil entry.
type leaveOut struct {
	jetstream.KeyValue
	key string
}

func (k leaveOut) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return k.forward(k.KeyValue.Watch(ctx, keys, opts...))
}

func (k leaveOut) WatchFiltered(ctx context.Context, keys []string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return k.forward(k.KeyValue.WatchFiltered(ctx, keys, opts...))
}

func (k leaveOut) forward(watcher jetstream.KeyWatcher, err error) (jetstream.KeyWatcher, error) {
	if err != nil {
		return nil, err
	}
	updates := make(chan jetstream.KeyValueEntry)
	go func() {
		defer close(updates)
		initial := true
		for entry := range watcher.Updates() {
			if entry == nil {
				initial = false
			} else if initial && entry.Key() == k.key {
				continue
			}
			updates <- entry
		}
	}()
	return &forwardedWatcher{KeyWatcher: watcher, updates: updates}, nil
}

// scanLeader records the keys the scan reads and can change the omitted key or fail its read.
// It forwards reads through the plain handle so they do not reenter the wrapper.
type scanLeader struct {
	natsgo.KeyValue
	plain      bus.KeyValue
	omitted    string
	beforeRead func() error
	keys       []string
}

var _ bus.LeaderReads = (*scanLeader)(nil)

func (r *scanLeader) NewestFromLeader(ctx context.Context, key string) (natsgo.KeyValueEntry, error) {
	r.keys = append(r.keys, key)
	if key == r.omitted && r.beforeRead != nil {
		if err := r.beforeRead(); err != nil {
			return nil, err
		}
	}
	return r.plain.NewestFromLeader(ctx, key)
}

func scanLeavingOut(kv bus.KeyValue, key string, beforeRead func() error) (bus.KeyValue, *scanLeader) {
	leader := &scanLeader{KeyValue: kv.Legacy(), plain: kv, omitted: key, beforeRead: beforeRead}
	return bus.NewKeyValue(leader, leaveOut{KeyValue: kv.WatchHandle(), key: key}, kv.Streams()), leader
}

// seedScanMarkers sends paired puts and deletes on one connection so the server stores them in
// order. Each batch is smaller than the async pending limit and is acknowledged before the next.
func seedScanMarkers(t *testing.T, kv bus.KeyValue, count int) {
	t.Helper()
	const batchKeys = 128
	js := kv.Streams()
	futures := make([]jetstream.PubAckFuture, 0, 2*batchKeys)
	for first := 0; first < count; first += batchKeys {
		futures = futures[:0]
		for i := first; i < min(first+batchKeys, count); i++ {
			subject := testnats.KVSubject(kv.Bucket(), fmt.Sprintf("deleted.%05d", i))
			put, err := js.PublishAsync(subject, []byte("value"))
			if err != nil {
				t.Fatalf("publish put on %s: %v", subject, err)
			}
			marker := natsgo.NewMsg(subject)
			marker.Header.Set("KV-Operation", "DEL")
			del, err := js.PublishMsgAsync(marker)
			if err != nil {
				t.Fatalf("publish delete on %s: %v", subject, err)
			}
			futures = append(futures, put, del)
		}
		testwait.Receive(t, "the batch of puts and deletes to finish publishing", js.PublishAsyncComplete())
		for _, future := range futures {
			select {
			case err := <-future.Err():
				t.Fatalf("store message on %s: %v", future.Msg().Subject, err)
			case <-future.Ok():
			default:
				t.Fatalf("no publish result for %s after the batch completed", future.Msg().Subject)
			}
		}
	}
}

const scanGetInboxPrefix = "_INBOX.scan_many_markers."

// assertNoScanGets counts requests after a sentinel sent on the handle's connection. The server
// echoes that connection's publishes in order, so when the sentinel arrives nats.go has already
// queued every matching get the scan sent before it returned. The request's reply inbox identifies
// the other row's gets on the shared bucket. A request with no recognized inbox still counts.
func assertNoScanGets(t *testing.T, conn *natsgo.Conn, subs []*natsgo.Subscription, inbox string) {
	t.Helper()
	sentinels := make(chan *natsgo.Msg, 1)
	sentinel, err := conn.ChanSubscribe(natsgo.NewInbox(), sentinels)
	if err != nil {
		t.Fatalf("subscribe to the sentinel: %v", err)
	}
	defer func() { _ = sentinel.Unsubscribe() }()
	if err := conn.Publish(sentinel.Subject, nil); err != nil {
		t.Fatalf("publish the sentinel: %v", err)
	}
	testwait.Receive(t, "the sentinel published after the scan returned", sentinels)
	ctx := testwait.Context(t)
	for _, sub := range subs {
		queued, _, err := sub.Pending()
		if err != nil {
			t.Fatalf("count requests on %s: %v", sub.Subject, err)
		}
		pending := 0
		for range queued {
			msg, err := sub.NextMsgWithContext(ctx)
			if err != nil {
				t.Fatalf("read a queued request on %s: %v", sub.Subject, err)
			}
			if !strings.HasPrefix(msg.Reply, scanGetInboxPrefix) || strings.HasPrefix(msg.Reply, inbox+".") {
				pending++
			}
		}
		if pending != 0 {
			t.Errorf("scan sent %d requests on %s, want zero", pending, sub.Subject)
		}
	}
}
