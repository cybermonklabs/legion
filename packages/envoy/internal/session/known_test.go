package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// openKnown opens the known-session store on the package's shared NATS server, with a clock the
// test moves.
func openKnown(t *testing.T, now func() time.Time) *KnownSessions {
	t.Helper()
	client := setupNATS(t)
	known, err := OpenKnownSessions(t.Context(), client.Conn, WithKnownSessionReplicas(1), WithKnownSessionClock(now))
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if err := known.WaitForCacheReady(context.Background()); err != nil {
		t.Fatalf("the known-session cache did not become ready: %v", err)
	}
	return known
}

// matchedIDs is the session ids MatchEach visits for topic, sorted.
func matchedIDs(known *KnownSessions, topic string) []string {
	ids := []string{}
	known.MatchEach(topic, func(record KnownSession) { ids = append(ids, record.SessionID) })
	slices.Sort(ids)
	return ids
}

// A record written to the bucket is answered from memory by id and by topic, with the topics it
// was written with - a team topic among them.
func TestKnownSessionsAnswerByIDAndByTopic(t *testing.T) {
	known := openKnown(t, time.Now)
	topics := []string{"notifications.agent.ses_known", "notifications.team.deploy-squad"}
	if err := known.RecordWithin(context.Background(), KnownSession{
		SessionID:    "ses_known",
		Title:        "a known session",
		Capabilities: []string{"aside"},
		Topics:       topics,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	record, ok := known.Get("ses_known")
	if !ok {
		t.Fatal("Get answered no record for the session just written")
	}
	if record.Title != "a known session" || !slices.Equal(record.Topics, topics) {
		t.Fatalf("record = %#v, want the written title and topics", record)
	}
	if record.LastSeen <= 0 {
		t.Fatalf("record.LastSeen = %d, want the write's own stamp", record.LastSeen)
	}
	if got := matchedIDs(known, "notifications.team.deploy-squad"); !slices.Equal(got, []string{"ses_known"}) {
		t.Errorf("MatchEach of the team topic = %v, want the session", got)
	}
	if got := matchedIDs(known, "notifications.team.other"); len(got) != 0 {
		t.Errorf("MatchEach of an unfollowed topic = %v, want none", got)
	}
}

// TouchWithin moves a record's last_seen and keeps its content; a session the store knows nothing
// of writes nothing.
func TestKnownSessionsTouchKeepsTheRecordAndMovesLastSeen(t *testing.T) {
	clock := time.Now()
	known := openKnown(t, func() time.Time { return clock })
	topics := []string{"notifications.team.deploy-squad"}
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: "ses_touch", Topics: topics}); err != nil {
		t.Fatalf("record: %v", err)
	}
	first, _ := known.Get("ses_touch")
	clock = clock.Add(KnownSessionRefreshAge + time.Minute)
	if err := known.TouchWithin(context.Background(), "ses_touch"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	moved, ok := known.Get("ses_touch")
	if !ok {
		t.Fatal("the touched record is gone")
	}
	if moved.LastSeen != clock.UnixMilli() || moved.LastSeen == first.LastSeen {
		t.Errorf("last_seen = %d, want the clock's %d (was %d)", moved.LastSeen, clock.UnixMilli(), first.LastSeen)
	}
	if !slices.Equal(moved.Topics, topics) {
		t.Errorf("topics = %v, want the record's own %v", moved.Topics, topics)
	}
	if err := known.TouchWithin(context.Background(), "ses_unknown"); err != nil {
		t.Fatalf("touch of a session with no record: %v", err)
	}
	if _, ok := known.Get("ses_unknown"); ok {
		t.Error("a touch of a session with no record wrote one")
	}
}

// A record a newer build wrote carries a key this build does not name, and each rewrite of it
// through this build keeps that key with its value (KnownSessions.unknown): the registration's and
// the interest change's (RecordWithin) and the clean stop's (TouchWithin) alike. Every listener
// pod writes the records of the sessions it serves, so during a roll pods of two builds take
// turns writing one session's record, and a rewrite that dropped the newer build's field would
// take it from that build's readers until its own next write.
func TestARewriteOfAKnownSessionRecordKeepsANewerBuildsKeys(t *testing.T) {
	clock := time.Now()
	known := openKnown(t, func() time.Time { return clock })
	const sessionID = "ses_newer_build"
	const field, value = "newer_build_field", `{"kept":["by","both"]}`
	kv := watchHandle(known)
	// storeNewer puts the record as a newer build writes it, its own field among the others, and
	// waits for the store's copy to hold that write.
	storeNewer := func(title string) {
		t.Helper()
		raw := fmt.Sprintf(`{"session_id":%q,"title":%q,"capabilities":["aside"],"topics":["notifications.team.newer"],"last_seen":%d,%q:%s}`,
			sessionID, title, clock.UnixMilli(), field, value)
		if _, err := kv.Put(sessionID, []byte(raw)); err != nil {
			t.Fatalf("put the newer build's record: %v", err)
		}
		awaitKnown(t, 5*time.Second, "the store's copy held the newer build's record", func() bool {
			record, ok := known.Get(sessionID)
			return ok && record.Title == title
		})
	}
	// requireKept reads the record the rewrite stored, from the bucket, and fails unless the
	// rewrite stored it (its last_seen is the rewrite's) and kept the newer build's key with its
	// value.
	requireKept := func(rewrite string) {
		t.Helper()
		entry, err := kv.Get(sessionID)
		if err != nil {
			t.Fatalf("read the record %s stored: %v", rewrite, err)
		}
		var stored map[string]json.RawMessage
		if err := json.Unmarshal(entry.Value(), &stored); err != nil {
			t.Fatalf("the record %s stored is no JSON object: %v: %s", rewrite, err, entry.Value())
		}
		if got := string(stored["last_seen"]); got != fmt.Sprint(clock.UnixMilli()) {
			t.Fatalf("the record's last_seen is %s, want %s's %d: the rewrite stored nothing: %s", got, rewrite, clock.UnixMilli(), entry.Value())
		}
		got, ok := stored[field]
		if !ok {
			t.Fatalf("%s dropped the newer build's key %q from the record: %s", rewrite, field, entry.Value())
		}
		if string(got) != value {
			t.Fatalf("%s rewrote the newer build's key %q as %s, want %s", rewrite, field, got, value)
		}
	}

	storeNewer("written by a newer build")
	clock = clock.Add(time.Second)
	if err := known.RecordWithin(context.Background(), KnownSession{
		SessionID:    sessionID,
		Title:        "registered through this build",
		Capabilities: []string{"aside"},
		Topics:       []string{"notifications.team.newer"},
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	requireKept("RecordWithin")

	storeNewer("written by a newer build again")
	clock = clock.Add(time.Second)
	if err := known.TouchWithin(context.Background(), sessionID); err != nil {
		t.Fatalf("touch: %v", err)
	}
	requireKept("TouchWithin")
}

// observedKnown is openKnown with the store's watcher feeding its cache through a holdingCache, so
// a test can wait for the watcher to deliver back a record the store wrote through (holdApply on
// the record's key, opened at once). Once the first read has completed it ends the store's watcher
// and puts in its place one built as startWatch builds it, over the same bucket, with the store's
// logger and watch options, the way holdingRegistry replaces the session registry's watcher. Unlike
// holdingRegistry it writes the field under openMu, which every read of the known store's watcher
// takes, and it waits until the replacement's own first read has completed, so a delivery the gate
// meets after that is of a write the test makes.
func observedKnown(t *testing.T, now func() time.Time) (*KnownSessions, *holdingCache) {
	t.Helper()
	known := openKnown(t, now)
	previous := known.watch()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	previous.End(ctx)
	held := &holdingCache{Cache: knownCache{known}}
	replacement := kvwatch.New("known session store", previous.KV(), held,
		append([]kvwatch.Option{kvwatch.WithLogger(known.log)}, known.watchOptions...)...)
	known.openMu.Lock()
	known.watcher = replacement
	known.openMu.Unlock()
	replacement.Start()
	if err := known.WaitForCacheReady(context.Background()); err != nil {
		t.Fatalf("the replaced watcher's first read did not complete: %v", err)
	}
	return known, held
}

// A record whose last_seen is past the 30-day retention answers as no record, by id and by topic,
// before the sweep has run; the sweep then gives its memory back.
//
// The write puts the record in the copy at once (write-through), and the watcher delivers the same
// revision back a moment later. The sweep's removal keeps that revision in the copy's fence, which
// drops only older ones (kvwatch.Copy.Remove), so a delivery applied after the sweep puts the
// record back: a sweep run before the watcher has delivered the write proves nothing about the
// sweep. So the row sweeps once that delivery has been applied. In production a delivery can put
// back a record the sweep took too: a later read of the bucket (a Rewatch) delivers every record
// the bucket still holds, and the bucket can still hold a record past this store's retention. A
// bucket that already existed keeps the TTL it was created with (openKnownBucket changes none),
// and a listener whose clock is behind stamps a record's last_seen earlier than its write, so the
// record is past the retention here before the bucket's TTL removes it. Get and MatchEach answer no
// such record, and it costs its memory until the next sweep.
func TestKnownSessionsAnswerNoRecordPastTheRetention(t *testing.T) {
	clock := time.Now()
	known, cache := observedKnown(t, func() time.Time { return clock })
	delivered := cache.holdApply(t, "ses_old")
	delivered.open()
	if err := known.RecordWithin(context.Background(), KnownSession{
		SessionID: "ses_old",
		Topics:    []string{"notifications.team.deploy-squad"},
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	testwait.Eventually(t, "the watcher to deliver ses_old's write back to the copy", func() bool {
		select {
		case <-delivered.done:
			return true
		default:
			return false
		}
	})
	clock = clock.Add(KnownSessionRetention + time.Hour)
	if _, ok := known.Get("ses_old"); ok {
		t.Error("Get answered a record older than the retention")
	}
	if got := matchedIDs(known, "notifications.team.deploy-squad"); len(got) != 0 {
		t.Errorf("MatchEach answered %v for a record older than the retention", got)
	}
	known.mu.RLock()
	held := known.records.Len()
	known.mu.RUnlock()
	if held != 1 {
		t.Fatalf("the store holds %d records before the sweep, want the one it has not swept yet", held)
	}
	known.sweep()
	known.mu.RLock()
	held = known.records.Len()
	known.mu.RUnlock()
	if held != 0 {
		t.Errorf("the store holds %d records after the sweep, want none", held)
	}
	if got := matchedIDs(known, "notifications.team.deploy-squad"); len(got) != 0 {
		t.Errorf("MatchEach answered %v after the sweep, want none", got)
	}
}

// Every method is safe on a nil store, the listener that could not build a JetStream context: no
// record, no match, not open, and writes that do nothing.
func TestNilKnownSessionsAnswersNothingAndPanicsNowhere(t *testing.T) {
	var known *KnownSessions
	if known.Opened() {
		t.Error("a nil store reports itself open")
	}
	if record, ok := known.Get("ses_any"); ok || record.SessionID != "" {
		t.Errorf("Get on a nil store = %#v, %v, want no record", record, ok)
	}
	known.MatchEach("notifications.team.deploy-squad", func(record KnownSession) {
		t.Errorf("MatchEach on a nil store visited %#v", record)
	})
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: "ses_any"}); err != nil {
		t.Errorf("RecordWithin on a nil store = %v, want nil", err)
	}
	if err := known.TouchWithin(context.Background(), "ses_any"); err != nil {
		t.Errorf("TouchWithin on a nil store = %v, want nil", err)
	}
	if err := known.Rewatch(context.Background(), nil); err != nil {
		t.Errorf("Rewatch on a nil store = %v, want nil", err)
	}
	if err := known.WaitForCacheReady(context.Background()); err != nil {
		t.Errorf("WaitForCacheReady on a nil store = %v, want nil", err)
	}
	known.Disconnected()
	known.StopWatch()
	known.EndWatch(context.Background())
}

// knownBucketGrant is the publish grant a NATS user needs to open the known-session bucket,
// watch it and write it. A listener's user carries these where its hub's grant carries the
// bucket, and none of them where it does not, which is the listener below.
func knownBucketGrant() []string {
	stream := "KV_" + bus.KnownSessionBucket
	return []string{
		"$JS.API.INFO",
		"$JS.API.STREAM.INFO." + stream,
		"$JS.API.STREAM.CREATE." + stream,
		"$JS.API.DIRECT.GET." + stream + ".>",
		"$JS.API.STREAM.MSG.GET." + stream,
		"$KV." + bus.KnownSessionBucket + ".>",
		"$JS.API.CONSUMER.CREATE." + stream + ".>",
		"$JS.API.CONSUMER.DELETE." + stream + ".>",
		"$JS.FC." + stream + ".>",
	}
}

// A store whose NATS user may not open the bucket opens all the same, within one attempt's wait:
// it stops no start, waits for no cache, answers no record and writes nothing, and says so once.
// When the grant arrives an attempt of its own opens the bucket, with no restart, and the store
// writes and reads from then on. The denial is the server's own: the user's publish grant names
// none of the bucket's subjects, so the request is dropped as a denied JetStream request is in
// production, and the attempt costs the whole wait rather than ending on a refusal.
func TestAKnownSessionStoreOpensItsDeniedBucketOnceTheGrantArrives(t *testing.T) {
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, "nothing.this.store.uses")
	conn, err := testnats.ConnectSeed(t, grant.URL, seed)
	if err != nil {
		t.Fatalf("connect as the user whose grant does not carry the bucket: %v", err)
	}
	t.Cleanup(conn.Close)
	logs := &lockedWriter{}
	logged := logs.String

	started := time.Now()
	known, err := OpenKnownSessions(t.Context(), conn,
		WithKnownSessionReplicas(1),
		WithKnownSessionLogger(slog.New(slog.NewTextHandler(logs, nil))),
		// The production schedule's first wait is a second and its cap five minutes, which a
		// test would spend waiting; every attempt it makes is the same.
		withKnownSessionRetry(100*time.Millisecond, 500*time.Millisecond))
	opening := time.Since(started)
	if err != nil {
		t.Fatalf("open the store over a bucket its user may not open: %v", err)
	}
	if known == nil {
		t.Fatal("no store over a bucket its user may not open; the listener has nothing to answer reads with")
	}
	t.Cleanup(known.StopWatch)
	if opening > 2*knownBucketOpenWait {
		t.Fatalf("the open took %s, want it bounded by the %s one attempt waits: the listener's start waits here before it opens /v1",
			opening, knownBucketOpenWait)
	}
	t.Logf("the open over a denied bucket took %s", opening)

	waited := time.Now()
	readyCtx, cancelReady := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelReady()
	if err := known.WaitForCacheReady(readyCtx); err != nil {
		t.Fatalf("WaitForCacheReady over a bucket that is not open: %v, want nil", err)
	}
	if elapsed := time.Since(waited); elapsed > time.Second {
		t.Fatalf("the cache gate waited %s on a bucket that is not open, want it to wait for nothing", elapsed)
	}

	const denied = "ses_denied_bucket"
	topic := "notifications.team.deploy-squad"
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: denied, Topics: []string{topic}}); err != nil {
		t.Fatalf("RecordWithin over a bucket that is not open: %v, want nil", err)
	}
	if record, ok := known.Get(denied); ok {
		t.Errorf("Get over a bucket that is not open answered %#v, want no record", record)
	}
	if got := matchedIDs(known, topic); len(got) != 0 {
		t.Errorf("Match over a bucket that is not open answered %v, want none", got)
	}
	if warnings := strings.Count(logged(), "level=WARN"); warnings != 1 {
		t.Fatalf("the store logged %d WARN lines over a bucket it may not open, want the one that names it:\n%s", warnings, logged())
	}

	grant.Grant(t, knownBucketGrant()...)

	deadline := time.Now().Add(30 * time.Second)
	for !known.Opened() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !known.Opened() {
		t.Fatalf("the store did not open the bucket within 30s of the grant:\n%s", logged())
	}
	// Opened is the watcher's first pass delivered, and the line is written by the goroutine that
	// waited for it, so the two are not ordered: wait for the line before counting it.
	for !strings.Contains(logged(), "the known-session bucket is open") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: denied, Topics: []string{topic}}); err != nil {
		t.Fatalf("record once the grant arrived: %v", err)
	}
	record, ok := known.Get(denied)
	if !ok {
		t.Fatal("the store holds no record of the session written once the grant arrived")
	}
	if !slices.Equal(record.Topics, []string{topic}) {
		t.Errorf("the record's topics are %v, want the written %q", record.Topics, topic)
	}
	if warnings := strings.Count(logged(), "level=WARN"); warnings != 1 {
		t.Errorf("the store logged %d WARN lines, want the one the first failed open wrote and none per attempt:\n%s", warnings, logged())
	}
	if opens := strings.Count(logged(), "the known-session bucket is open"); opens != 1 {
		t.Errorf("the store logged the open %d times, want one line:\n%s", opens, logged())
	}
}

// A bucket opened after the start answers the records it already holds from the moment the store
// reports it open. The store is what the send route reads to tell a session Envoy knows from one
// it does not (cmd/listener/api.go, sendHandler) and what a publish counts its recipients
// against, so a store that reports an open bucket over a cache no watcher has filled answers
// `404 no live session` for a session the bucket knows and counts a publish's recipients low -
// the two failures the store exists to prevent, arriving at the moment the grant lands. The
// listener's start keeps that from happening by waiting on WaitForCacheReady before it opens /v1
// (cmd/listener/main.go); nothing waits on the attempt that opens the bucket later, so the store
// itself must not answer before its cache is filled.
func TestABucketOpenedAfterTheStartAnswersTheRecordsItHolds(t *testing.T) {
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, knownBucketGrant()...)
	conn, err := testnats.ConnectSeed(t, grant.URL, seed)
	if err != nil {
		t.Fatalf("connect as the user whose grant carries the bucket: %v", err)
	}
	t.Cleanup(conn.Close)

	// The records a listener started before the grant finds waiting for it. Enough of them that
	// one watcher pass over the bucket is work: a cache filled before the store answers is the
	// whole question, and a handful of records would be filled whether it waits or not.
	const records = 2000
	const topic = "notifications.team.late-open"
	filling, err := OpenKnownSessions(t.Context(), conn, WithKnownSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the store that fills the bucket: %v", err)
	}
	for index := range records {
		id := fmt.Sprintf("ses_late_open_%04d", index)
		if err := filling.RecordWithin(context.Background(), KnownSession{SessionID: id, Topics: []string{topic}}); err != nil {
			t.Fatalf("write the record of %s: %v", id, err)
		}
	}
	if err := filling.WaitForCacheReady(context.Background()); err != nil {
		t.Fatalf("the filling store's cache did not become ready: %v", err)
	}
	if held := len(matchedIDs(filling, topic)); held != records {
		t.Fatalf("the bucket holds %d records, want the %d written: this test proves nothing about a cache filled before the store answers", held, records)
	}
	filling.StopWatch()

	// The grant goes away, as it is away on every listener started before it is applied.
	grant.Grant(t, "nothing.this.store.uses")
	known, err := OpenKnownSessions(t.Context(), conn,
		WithKnownSessionReplicas(1),
		withKnownSessionRetry(100*time.Millisecond, 500*time.Millisecond))
	if err != nil {
		t.Fatalf("open the store over a bucket its user may not open: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if known.Opened() {
		t.Fatal("the store reports its bucket open while its user may not open it")
	}

	grant.Grant(t, knownBucketGrant()...)
	deadline := time.Now().Add(60 * time.Second)
	for !known.Opened() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !known.Opened() {
		t.Fatal("the store did not open the bucket within 60s of the grant")
	}
	if answered := len(matchedIDs(known, topic)); answered != records {
		t.Fatalf("the store answered %d of the %d records the bucket holds at the moment it reported its bucket open; until its cache is filled a send to each of the other sessions is refused 404 no live session and a publish counts them for nothing",
			answered, records)
	}
	last := fmt.Sprintf("ses_late_open_%04d", records-1)
	if _, ok := known.Get(last); !ok {
		t.Fatalf("the store answers no record for %s, the last the bucket was given, at the moment it reported its bucket open", last)
	}
}

// stallingFirstWatch is a bucket's jetstream handle whose first watch delivers `after` records of
// its scan and then nothing more of it, neither the rest nor its end marker, as a scan that stalls;
// what the bucket delivers after the scan is forwarded. Every later watch goes through the handle
// itself.
type stallingFirstWatch struct {
	jetstream.KeyValue
	after   int
	mu      sync.Mutex
	watched int
}

func (k *stallingFirstWatch) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	k.mu.Lock()
	k.watched++
	first := k.watched == 1
	k.mu.Unlock()
	watcher, err := k.KeyValue.Watch(ctx, keys, opts...)
	if err != nil || !first {
		return watcher, err
	}
	updates := make(chan jetstream.KeyValueEntry)
	go func() {
		defer close(updates)
		scanning, forwarded := true, 0
		for entry := range watcher.Updates() {
			switch {
			case entry == nil && scanning:
				scanning = false
			case scanning && forwarded == k.after:
			default:
				forwarded++
				updates <- entry
			}
		}
	}()
	return &forwardedKeyWatcher{KeyWatcher: watcher, updates: updates}, nil
}

// forwardedKeyWatcher is a watcher whose updates are another channel's.
type forwardedKeyWatcher struct {
	jetstream.KeyWatcher
	updates chan jetstream.KeyValueEntry
}

func (w *forwardedKeyWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.updates }

// A store whose first scan stalled holds only the records that scan delivered, so it is no
// answer: Opened reads whether the copy can be served (kvwatch.Watcher.Servable), which a read
// only completes once its scan has delivered every record, not the watcher's readiness, which a
// scan its idle wait ended releases too. Until a read completes, a publish's count is left out
// rather than counted against part of the bucket. The watcher reads the bucket again itself after
// a timed-out scan, and the store reports its bucket open once that read completes.
func TestAStoreWhoseFirstScanTimedOutOpensOnlyOnceItsRescanCompletes(t *testing.T) {
	client := setupNATS(t)
	const topic = "notifications.team.timed-out-scan"
	ids := []string{"ses_timeout_1", "ses_timeout_2", "ses_timeout_3"}
	filling, err := OpenKnownSessions(t.Context(), client.Conn, WithKnownSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the store that fills the bucket: %v", err)
	}
	for _, id := range ids {
		if err := filling.RecordWithin(context.Background(), KnownSession{SessionID: id, Topics: []string{topic}}); err != nil {
			t.Fatalf("write the record of %s: %v", id, err)
		}
	}
	filling.StopWatch()
	raw, err := bus.OpenKeyValue(context.Background(), client.Conn, client.JS(), bus.KnownSessionBucket)
	if err != nil {
		t.Fatalf("open the known-session bucket: %v", err)
	}

	// The store's own watcher, over a handle whose first watch stalls after one entry, with a
	// scan idle timeout short enough that the test meets the timed-out scan rather than waiting
	// out the production one.
	known := newKnownSessions(client.Conn, knownSessionsOpts{
		now:   time.Now,
		watch: []kvwatch.Option{kvwatch.WithScanIdleTimeout(200 * time.Millisecond)},
	})
	t.Cleanup(known.StopWatch)
	known.startWatch(bus.NewKeyValue(raw.Legacy(), &stallingFirstWatch{KeyValue: raw.WatchHandle(), after: 1}, raw.Streams()))
	watcher := known.watch()

	deadline := time.Now().Add(30 * time.Second)
	for !watcher.Ready() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !watcher.Ready() {
		t.Fatal("the cache never became ready, so this test says nothing about a scan the idle timer ended")
	}
	if known.Opened() {
		t.Fatal("the store reports its bucket open over a first scan the idle timer ended: a publish would count its recipients against the one record that scan delivered")
	}
	if got := matchedIDs(known, topic); len(got) != 0 {
		t.Fatalf("the store answered %v over a first scan the idle timer ended, want nothing until a read completes", got)
	}

	for !known.Opened() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !known.Opened() {
		t.Fatal("the store never reported its bucket open: nothing completed a read after the idle timer ended the first")
	}
	if got := matchedIDs(known, topic); !slices.Equal(got, ids) {
		t.Fatalf("the store answered %v at the moment it reported its bucket open, want every record the bucket holds, %v", got, ids)
	}
}

// knownOverGrant opens a store whose bucket is open, on a server whose one user's grant the test
// takes away and gives back (testnats.NkeyGrant): while it is away every request the store makes
// is dropped by the server, so a move to a recovered connection fails as it fails against a
// cluster that refuses this listener - a grant rolled back, or a bucket deleted under its
// watcher - and succeeds again once it is back. The retry's waits are the test's, since the
// production schedule's first wait is a second and its cap five minutes; extra adds options of
// the test's own.
func knownOverGrant(t *testing.T, first, longest time.Duration, extra ...KnownSessionsOption) (*KnownSessions, *nats.Conn, *testnats.NkeyGrant, func() string) {
	t.Helper()
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, knownBucketGrant()...)
	conn, err := testnats.ConnectSeed(t, grant.URL, seed)
	if err != nil {
		t.Fatalf("connect as the user whose grant carries the bucket: %v", err)
	}
	t.Cleanup(conn.Close)
	logs := &lockedWriter{}
	options := append([]KnownSessionsOption{
		WithKnownSessionReplicas(1),
		WithKnownSessionLogger(slog.New(slog.NewTextHandler(logs, nil))),
		withKnownSessionRetry(first, longest),
	}, extra...)
	known, err := OpenKnownSessions(t.Context(), conn, options...)
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if err := known.WaitForCacheReady(context.Background()); err != nil {
		t.Fatalf("the known-session cache did not become ready: %v", err)
	}
	if !known.Opened() {
		t.Fatal("the store does not report its bucket open before the grant is taken away")
	}
	return known, conn, grant, logs.String
}

// repairRunning reports whether the store's one background repair is running.
func repairRunning(known *KnownSessions) bool {
	known.repairMu.Lock()
	defer known.repairMu.Unlock()
	return known.repairing
}

// watchHandle is the bucket handle the store's watcher reads and the store writes through
// (kvwatch.Watcher.KV): a move that succeeded opens a new one on the connection it moved to, and a
// move that failed leaves the one there was. It is how a test tells a move the store made from one
// it did not.
func watchHandle(known *KnownSessions) nats.KeyValue {
	return known.watch().KV().Legacy()
}

// knownBackLine is the words of the line the store writes when it is back after a move that
// failed (attemptRepair).
const knownBackLine = "the known-session cache is back"

// awaitKnown waits for what to hold, failing the test when it has not within the wait.
func awaitKnown(t *testing.T, within time.Duration, what string, holds func() bool) {
	t.Helper()
	if !testwait.Holds(within, 10*time.Millisecond, holds) {
		t.Fatalf("%s within %s", what, within)
	}
}

// A move to a recovered connection that failed is repaired by the store itself, on its own
// backoff. The bus runs a reconnect hook that fails again (internal/bus/recovery.go,
// AddReconnectHook), but the listener's hook for this store answers nil over the error: a failed
// run is run again whole, so every cache of the listener would read its bucket again on each of
// the bus's attempts, and its stall would turn /healthz degraded and log the ERROR an operator is
// paged by, for a store no delivery reads. The store is kept out of listenerCaches, so the
// self-health rebuild passes it by too, and the attempts that open a bucket that never opened are
// not this. Until the move is made the watcher and the write handle are on a connection the
// listener has left, so every record written is lost and, past the stale bound, a publish's
// recipient count is left out.
func TestAFailedMoveToARecoveredConnectionIsRepairedByTheStore(t *testing.T) {
	known, conn, grant, logged := knownOverGrant(t, 200*time.Millisecond, time.Second)
	before := watchHandle(known)

	grant.Grant(t, "nothing.this.store.uses")
	if err := known.Rewatch(context.Background(), conn); err == nil {
		t.Fatal("the move succeeded while the user's grant named none of the bucket's subjects; this test says nothing about a move that failed")
	}
	if watchHandle(known) != before {
		t.Fatal("the watcher moved to a new handle over a move that failed, want it left on the one it had")
	}
	if !repairRunning(known) {
		t.Fatal("no repair runs after a move that failed: nothing else moves the watcher off the connection the listener has left")
	}

	// The cluster takes this listener back, and nothing asks the store to move again: no
	// connection event, no reconnect hook, no rebuild. The store's own retry is the whole repair.
	grant.Grant(t, knownBucketGrant()...)
	awaitKnown(t, 60*time.Second, "the store's own repair did not move the watcher", func() bool {
		return watchHandle(known) != before
	})
	awaitKnown(t, 60*time.Second, "the repair did not retire once it had nothing left to repair", func() bool {
		return !repairRunning(known)
	})
	if lines := strings.Count(logged(), knownBackLine); lines != 1 {
		t.Errorf("the repair logged %d lines, want the one that reports the move it made:\n%s", lines, logged())
	}

	// The cache the repair left behind is the bucket's again: it reports the bucket open once the
	// moved watcher's scan has delivered every record, and the records written through it answer.
	awaitKnown(t, 60*time.Second, "the repaired store did not report its bucket open", known.Opened)
	const repaired = "ses_after_the_repair"
	const topic = "notifications.team.repaired-move"
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: repaired, Topics: []string{topic}}); err != nil {
		t.Fatalf("record through the repaired store: %v", err)
	}
	if got := matchedIDs(known, topic); !slices.Equal(got, []string{repaired}) {
		t.Errorf("the repaired store answered %v for the topic of the record just written, want %q", got, repaired)
	}
}

// The move the next connection event makes ends the retry, and the retry moves nothing after it:
// the connection event's move is the newer one, and a retry that moved the watcher again would
// put it on the handle of a connection the store has already left. The waits here are long
// enough that the connection event lands between the retry being armed and its first attempt,
// which is the window this guards. The connection event's move is the repair, so it is the one
// that says the store is back, and the retry that ends after it says nothing more.
func TestAMoveThatSucceedsEndsTheStoresRetry(t *testing.T) {
	known, conn, grant, logged := knownOverGrant(t, 5*time.Second, 5*time.Second)
	before := watchHandle(known)

	grant.Grant(t, "nothing.this.store.uses")
	if err := known.Rewatch(context.Background(), conn); err == nil {
		t.Fatal("the move succeeded while the user's grant named none of the bucket's subjects; this test says nothing about a move that failed")
	}
	if !repairRunning(known) {
		t.Fatal("no repair runs after a move that failed")
	}

	grant.Grant(t, knownBucketGrant()...)
	if err := known.Rewatch(context.Background(), conn); err != nil {
		t.Fatalf("the move of the next connection event, once the cluster took this listener back: %v", err)
	}
	moved := watchHandle(known)
	if moved == before {
		t.Fatal("the move that succeeded left the watcher on the handle it had")
	}
	awaitKnown(t, 60*time.Second, "the retry did not end after a move that succeeded", func() bool {
		return !repairRunning(known)
	})
	if watchHandle(known) != moved {
		t.Error("the retry moved the watcher again: the move the connection event made is the newer one")
	}
	if lines := strings.Count(logged(), knownBackLine); lines != 1 {
		t.Errorf("the store logged %d lines saying it is back, want the one the connection event's move wrote and none from the retry after it:\n%s", lines, logged())
	}
}

// The store says it is back on every repair, the one the next connection event's own move makes
// as well as one its retry makes (TestAFailedMoveToARecoveredConnectionIsRepairedByTheStore): one
// INFO line naming the bucket, written by the move that succeeded after one that failed. A failed
// move says nothing of the kind, and neither does the move of a connection event with no failure
// before it, which is the ordinary path.
func TestTheStoreSaysItIsBackAfterARepairThroughAConnectionEvent(t *testing.T) {
	known, conn, grant, logged := knownOverGrant(t, 5*time.Second, 5*time.Second)

	if err := known.Rewatch(context.Background(), conn); err != nil {
		t.Fatalf("the move of an ordinary connection event: %v", err)
	}
	if strings.Contains(logged(), knownBackLine) {
		t.Fatalf("the move of an ordinary connection event said the store is back, with nothing failed before it:\n%s", logged())
	}

	grant.Grant(t, "nothing.this.store.uses")
	if err := known.Rewatch(context.Background(), conn); err == nil {
		t.Fatal("the move succeeded while the user's grant named none of the bucket's subjects; this test says nothing about a repair")
	}
	if strings.Contains(logged(), knownBackLine) {
		t.Fatalf("a move that failed said the store is back:\n%s", logged())
	}

	grant.Grant(t, knownBucketGrant()...)
	if err := known.Rewatch(context.Background(), conn); err != nil {
		t.Fatalf("the move of the next connection event, once the cluster took this listener back: %v", err)
	}
	var back []string
	for line := range strings.Lines(logged()) {
		if strings.Contains(line, knownBackLine) {
			back = append(back, line)
		}
	}
	if len(back) != 1 {
		t.Fatalf("the repair the connection event made logged %d lines saying the store is back, want one:\n%s", len(back), logged())
	}
	if !strings.Contains(back[0], "level=INFO") || !strings.Contains(back[0], "bucket="+bus.KnownSessionBucket) {
		t.Errorf("the line saying the store is back is %q, want an INFO line naming the bucket", back[0])
	}
	awaitKnown(t, 60*time.Second, "the retry did not end after the connection event's repair", func() bool {
		return !repairRunning(known)
	})
	if lines := strings.Count(logged(), knownBackLine); lines != 1 {
		t.Errorf("the store logged %d lines saying it is back, want the connection event's one:\n%s", lines, logged())
	}
}

// While a move to a recovered connection has failed, the store answers from its copy for the
// watcher's stale bound, two minutes, as every cache of the listener does
// (kvwatch.Watcher.Servable), and past it answers nothing: Opened is false, so a publish leaves its
// counts out, and Get answers no record, so a send to a session with no entry answers 404, exactly
// as on a listener with no bucket. Once the retry has moved the watcher and the moved watcher's
// read has completed, the store answers again. The watcher's clock is the test's, and it moves only
// when the test moves it, so the bound is passed without waiting it out, and a failed move that
// takes minutes on a busy machine does not pass it before the test asks the store inside it.
func TestAStaleStoreLeavesTheCountsOutPastItsBound(t *testing.T) {
	watcherClock := &heldClock{now: time.Now()}
	known, conn, grant, logged := knownOverGrant(t, 200*time.Millisecond, time.Second, withKnownSessionWatch(kvwatch.WithNow(watcherClock.Now)))
	const stale = "ses_stale_store"
	const topic = "notifications.team.stale-store"
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: stale, Topics: []string{topic}}); err != nil {
		t.Fatalf("record: %v", err)
	}

	// The connection drops and the move to the recovered one fails: the copy stops following its
	// bucket, and its stale episode begins at the disconnect.
	grant.Grant(t, "nothing.this.store.uses")
	known.Disconnected()
	if err := known.Rewatch(context.Background(), conn); err == nil {
		t.Fatal("the move succeeded while the user's grant named none of the bucket's subjects; this test says nothing about a stale store")
	}
	if !known.Opened() {
		t.Fatal("the store stopped answering at a failed move, inside the stale bound")
	}
	if _, ok := known.Get(stale); !ok {
		t.Error("Get answered no record inside the stale bound, want the record the copy holds")
	}
	if got := matchedIDs(known, topic); !slices.Equal(got, []string{stale}) {
		t.Errorf("MatchEach answered %v inside the stale bound, want %q", got, stale)
	}

	watcherClock.advance(2*time.Minute + time.Second)
	if known.Opened() {
		t.Fatal("the store reports itself open past the stale bound: a publish would count its recipients against a copy that stopped following its bucket")
	}
	if record, ok := known.Get(stale); ok {
		t.Errorf("Get answered %#v past the stale bound, want no record, as a listener with no bucket answers", record)
	}
	if got := matchedIDs(known, topic); len(got) != 0 {
		t.Errorf("MatchEach answered %v past the stale bound, want none", got)
	}

	grant.Grant(t, knownBucketGrant()...)
	awaitKnown(t, 60*time.Second, "the store did not answer again once its retry had moved the watcher", known.Opened)
	if _, ok := known.Get(stale); !ok {
		t.Error("the repaired store answers no record of the session written before the failed move")
	}
	// Opened is the moved watcher's read completed, on the watcher's own goroutine, and the line
	// saying the store is back is written by the retry once its move has returned (attemptRepair),
	// so the two are not ordered: a count read when Opened turns true can find no line yet. The
	// retry retires at an attempt after the one that wrote the line, both under repairMu, so its
	// retirement is what the count waits for, and no attempt after it writes a second line.
	awaitKnown(t, 60*time.Second, "the repair did not retire once it had nothing left to repair", func() bool {
		return !repairRunning(known)
	})
	if lines := strings.Count(logged(), knownBackLine); lines != 1 {
		t.Errorf("the repair logged %d lines saying the store is back, want one:\n%s", lines, logged())
	}
}

// heldClock is a clock that moves only when the test advances it, as kvwatch's own tests hold
// theirs: a stale bound measured on it is passed by the test's advance, never by how long the
// steps before that advance took. The package's other clock (clock) follows the real time, which
// a stale episode would then measure too.
type heldClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *heldClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *heldClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// A stop ends the retry wherever it is, and a store that stopped repairs nothing afterwards: its
// watcher is retired for the drain, and a move it made would arm a watcher nothing ends.
func TestAStopEndsTheStoresRetry(t *testing.T) {
	known, conn, grant, _ := knownOverGrant(t, 200*time.Millisecond, time.Second)
	before := watchHandle(known)

	grant.Grant(t, "nothing.this.store.uses")
	if err := known.Rewatch(context.Background(), conn); err == nil {
		t.Fatal("the move succeeded while the user's grant named none of the bucket's subjects; this test says nothing about a move that failed")
	}
	if watchHandle(known) != before {
		t.Fatal("the watcher moved to a new handle over a move that failed, want it left on the one it had")
	}
	if !repairRunning(known) {
		t.Fatal("no repair runs after a move that failed")
	}

	known.StopWatch()
	awaitKnown(t, 60*time.Second, "the retry did not end at the stop", func() bool {
		return !repairRunning(known)
	})
	// A move made after the stop, however it came, would still move the handle
	// (kvwatch.Watcher.Rewatch), so the handle is what says none was made.
	retired := watchHandle(known)

	// The cluster takes the listener back, past several of the retry's waits: a store that
	// stopped arms nothing, and moves nothing.
	grant.Grant(t, knownBucketGrant()...)
	time.Sleep(2 * time.Second)
	if repairRunning(known) {
		t.Error("a repair was armed after the stop")
	}
	if watchHandle(known) != retired {
		t.Error("a stopped store moved its watcher to a new handle, want it left on the one it had")
	}
}

// A move that fails while a bucket opened after the start is still in its first read is repaired
// by the store itself, as any move that failed is
// (TestAFailedMoveToARecoveredConnectionIsRepairedByTheStore). A disconnect during that read
// completes nothing and wakes nothing that waits for it (kvwatch.Watcher.Disconnected), and
// nats.go keeps the watcher's subscription open through the outage, so nothing ends a wait for
// that read either. A retry that waited for that read itself stayed armed while it repaired
// nothing: the failed move of the reconnect after the disconnect armed no retry of its own
// (armRepair), and the store answered no record until the next connection event, which can be
// days away. Here no connection event follows the failed move, so the store's own retry is the
// whole repair.
func TestAFailedMoveDuringTheFirstReadOfALateOpenIsRepairedByTheStore(t *testing.T) {
	const openLine = "the known-session bucket is open"
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, knownBucketGrant()...)
	conn, err := testnats.ConnectSeed(t, grant.URL, seed)
	if err != nil {
		t.Fatalf("connect as the user whose grant carries the bucket: %v", err)
	}
	t.Cleanup(conn.Close)

	// The records the late open is to answer once the store is repaired.
	const topic = "notifications.team.late-open-repair"
	held := []string{"ses_late_repair_1", "ses_late_repair_2", "ses_late_repair_3"}
	filling, err := OpenKnownSessions(t.Context(), conn, WithKnownSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the store that fills the bucket: %v", err)
	}
	for _, id := range held {
		if err := filling.RecordWithin(context.Background(), KnownSession{SessionID: id, Topics: []string{topic}}); err != nil {
			t.Fatalf("write the record of %s: %v", id, err)
		}
	}
	filling.StopWatch()

	// reading is closed once the late open's watcher has installed its first read: the watcher arms
	// its first wait (kvwatch.WithAfterFunc) as that read installs, after the read has taken its
	// count of disconnects, so a disconnect said from then on lands inside the read. Every wait is
	// armed on the real clock all the same.
	reading := make(chan struct{})
	var installed sync.Once
	after := func(d time.Duration, f func()) kvwatch.Timer {
		installed.Do(func() { close(reading) })
		return time.AfterFunc(d, f)
	}

	// The grant goes away, as it is away on every listener started before it is applied.
	grant.Grant(t, "nothing.this.store.uses")
	logs := &lockedWriter{}
	logged := logs.String
	known, err := OpenKnownSessions(t.Context(), conn,
		WithKnownSessionReplicas(1),
		WithKnownSessionLogger(slog.New(slog.NewTextHandler(logs, nil))),
		withKnownSessionRetry(100*time.Millisecond, 500*time.Millisecond),
		withKnownSessionWatch(kvwatch.WithAfterFunc(after)))
	if err != nil {
		t.Fatalf("open the store over a bucket its user may not open: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if known.Opened() {
		t.Fatal("the store reports its bucket open while its user may not open it")
	}

	// The first read is held at its start while the disconnect is said: its first hook takes the
	// store's lock (knownCache.BeginRead), so it delivers nothing, let alone completes, while the
	// test holds that lock.
	known.mu.Lock()
	var unlocked sync.Once
	release := func() { unlocked.Do(known.mu.Unlock) }
	defer release()
	grant.Grant(t, knownBucketGrant()...)
	select {
	case <-reading:
	case <-time.After(60 * time.Second):
		t.Fatal("the store did not open the bucket within 60s of the grant")
	}
	known.Disconnected()
	release()
	awaitKnown(t, 60*time.Second, "the first read of the bucket did not end", known.watch().Ready)
	if known.Opened() {
		t.Fatal("the first read completed over the disconnect said inside it; this test says nothing about a read a disconnect overlapped")
	}
	if strings.Contains(logged(), openLine) {
		t.Fatalf("the store said its bucket is open before a read of it completed:\n%s", logged())
	}

	// The reconnect after the disconnect moves the watcher, and the move fails: the cluster
	// refuses this listener again.
	grant.Grant(t, "nothing.this.store.uses")
	if err := known.Rewatch(context.Background(), conn); err == nil {
		t.Fatal("the move succeeded while the user's grant named none of the bucket's subjects; this test says nothing about a move that failed")
	}

	// The cluster takes this listener back, and nothing asks the store to move again: no
	// connection event, no reconnect hook, no rebuild.
	grant.Grant(t, knownBucketGrant()...)
	awaitKnown(t, 60*time.Second, "the store did not repair itself after the move that failed during its first read", known.Opened)
	if got := matchedIDs(known, topic); !slices.Equal(got, held) {
		t.Errorf("the repaired store answered %v, want every record the bucket holds, %v", got, held)
	}
	if _, ok := known.Get(held[0]); !ok {
		t.Errorf("the repaired store answers no record of %s, which the bucket holds", held[0])
	}
	const written = "ses_after_the_late_repair"
	const writtenTopic = "notifications.team.late-open-written"
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: written, Topics: []string{writtenTopic}}); err != nil {
		t.Fatalf("record through the repaired store: %v", err)
	}
	if got := matchedIDs(known, writtenTopic); !slices.Equal(got, []string{written}) {
		t.Errorf("the repaired store answered %v for the topic of the record just written, want %q", got, written)
	}
	awaitKnown(t, 60*time.Second, "the repair did not retire once it had nothing left to repair", func() bool {
		return !repairRunning(known)
	})

	// Opened is the moved watcher's read completed, and the open line is written by the goroutine
	// that waited for that read, so the two are not ordered: wait for the line before counting it.
	awaitKnown(t, 60*time.Second, "the store did not say its bucket is open", func() bool {
		return strings.Contains(logged(), openLine)
	})
	if lines := strings.Count(logged(), openLine); lines != 1 {
		t.Errorf("the store said its bucket is open %d times, want one line:\n%s", lines, logged())
	}
	if lines := strings.Count(logged(), knownBackLine); lines != 1 {
		t.Errorf("the repair logged %d lines saying the store is back, want the one that reports the move it made:\n%s", lines, logged())
	}
}

// A watch that ends before the first read of a bucket opened after the start has completed is
// repaired by the store too, whenever it ends. The retry does not wait for that read
// (TestAFailedMoveDuringTheFirstReadOfALateOpenIsRepairedByTheStore), so it can have found nothing
// left to repair and retired before the watch ended: the goroutine that waits for the read meets
// the end and arms the retry again, whose next attempt moves the watcher that ended. Here the
// watch ends because the grant carries the bucket but not the consumer a watch of it creates, so
// the bucket opens and the create request of its watcher's consumer is dropped.
func TestAWatchThatEndsBeforeTheFirstReadOfALateOpenIsRepairedByTheStore(t *testing.T) {
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, "nothing.this.store.uses")
	conn, err := testnats.ConnectSeed(t, grant.URL, seed)
	if err != nil {
		t.Fatalf("connect as the user whose grant does not carry the bucket: %v", err)
	}
	t.Cleanup(conn.Close)
	known, err := OpenKnownSessions(t.Context(), conn,
		WithKnownSessionReplicas(1),
		withKnownSessionRetry(100*time.Millisecond, 500*time.Millisecond))
	if err != nil {
		t.Fatalf("open the store over a bucket its user may not open: %v", err)
	}
	t.Cleanup(known.StopWatch)

	grant.Grant(t, slices.DeleteFunc(knownBucketGrant(), func(subject string) bool {
		return strings.HasPrefix(subject, "$JS.API.CONSUMER.CREATE.")
	})...)
	awaitKnown(t, 60*time.Second, "the store did not open the bucket", func() bool {
		return known.watch() != nil
	})
	awaitKnown(t, 60*time.Second, "the first watch of the bucket did not end", func() bool {
		return known.watch().Err() != nil
	})
	if known.Opened() {
		t.Fatal("the store reports its bucket open over a watch that ended before any read of it")
	}

	// The cluster grants the consumer, and nothing asks the store to move: no connection event.
	grant.Grant(t, knownBucketGrant()...)
	awaitKnown(t, 60*time.Second, "the store did not repair the watch that ended before its first read", known.Opened)
	const written = "ses_after_the_ended_watch"
	const topic = "notifications.team.ended-watch"
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: written, Topics: []string{topic}}); err != nil {
		t.Fatalf("record through the repaired store: %v", err)
	}
	if got := matchedIDs(known, topic); !slices.Equal(got, []string{written}) {
		t.Errorf("the repaired store answered %v for the topic of the record just written, want %q", got, written)
	}
	awaitKnown(t, 60*time.Second, "the repair did not retire once it had nothing left to repair", func() bool {
		return !repairRunning(known)
	})
}

// knownSessionMemoryBound is what the records, their revision fence and the subject index of
// 100,000 sessions may cost in memory. Each session here carries its agent subject, which no other
// session shares, and one of ten team topics, which is the shape the store holds in production:
// one unique pattern per session plus a handful of shared ones. The measurement on this machine
// (linux/amd64, go1.27), over the watcher's copy (kvwatch.Copy), was 89,710,800 bytes, 85.6 MiB,
// 897 bytes a record. The bound is 128 MiB: room for the allocator's own slack and for a Go
// release that grows a map's header, while still failing a change that makes a record or an index
// node cost half as much again.
const knownSessionMemoryBound = 128 << 20

// knownSessionMemoryRecords is the number of records the bound above is measured over: the order
// of magnitude a publish count must answer against (~100,000 sessions in 30 days).
const knownSessionMemoryRecords = 100_000

func TestKnownSessionsMemoryForAHundredThousandRecords(t *testing.T) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	known := newKnownSessions(nil, knownSessionsOpts{now: time.Now})
	now := time.Now().UnixMilli()
	for i := range knownSessionMemoryRecords {
		sessionID := fmt.Sprintf("ses_%024d", i)
		value, err := json.Marshal(KnownSession{
			SessionID:    sessionID,
			Title:        fmt.Sprintf("session %d of the memory measurement", i),
			Capabilities: []string{"aside"},
			Topics:       []string{"notifications.agent." + sessionID, fmt.Sprintf("notifications.team.squad-%d", i%10)},
			LastSeen:     now,
		})
		if err != nil {
			t.Fatalf("encode %s: %v", sessionID, err)
		}
		// Each record reaches the store as a read delivers it (kvwatch.Cache), so what is measured
		// is what the watcher's copy and the index keep of it, its revision fence included.
		knownCache{known}.Apply(kvwatch.Entry{Key: sessionID, Value: value, Revision: uint64(i + 1), Op: kvwatch.OpPut}, kvwatch.Read{})
	}

	runtime.GC()
	runtime.ReadMemStats(&after)
	held := after.HeapAlloc - before.HeapAlloc
	runtime.KeepAlive(known)

	known.mu.RLock()
	stored := known.records.Len()
	known.mu.RUnlock()
	if stored != knownSessionMemoryRecords {
		t.Fatalf("the store holds %d records, want %d", stored, knownSessionMemoryRecords)
	}
	matched := 0
	known.eachMatch("notifications.team.squad-3", func(KnownSession) { matched++ })
	if matched != knownSessionMemoryRecords/10 {
		t.Fatalf("the team topic matched %d records, want %d", matched, knownSessionMemoryRecords/10)
	}
	t.Logf("%d records and their subject index hold %d bytes (%.1f MiB), %d bytes each",
		knownSessionMemoryRecords, held, float64(held)/(1<<20), held/knownSessionMemoryRecords)
	if held > knownSessionMemoryBound {
		t.Errorf("%d records hold %d bytes, above the %d-byte bound", knownSessionMemoryRecords, held, knownSessionMemoryBound)
	}
}
