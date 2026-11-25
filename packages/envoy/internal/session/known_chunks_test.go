package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvchunk"
	"github.com/sjawhar/envoy/internal/kvchunk/kvchunktest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// chunkSubjects is how many subjects under the chunk keys the known-session bucket's stream holds a
// message on: live chunks and the markers replaced chunks left, read from the stream itself.
func chunkSubjects(t *testing.T, conn *nats.Conn) int {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	info, err := js.StreamInfo("KV_"+bus.KnownSessionBucket, &nats.StreamInfoRequest{SubjectsFilter: "$KV." + bus.KnownSessionBucket + "." + kvchunk.Prefix + ">"})
	if err != nil {
		t.Fatalf("read the known-session stream's chunk subjects: %v", err)
	}
	return len(info.State.Subjects)
}

// heartbeatDay records a session of the 1.2 MB shape on a server of the default 1 MiB max
// payload, then rewrites it as a day of heartbeats does: 144 times, each KnownSessionRefreshAge
// after the last on the store's clock. With purge, the marker pass runs kvchunk.MarkerGrace after
// each rewrite, once the watcher has delivered the deletes of the chunks it replaced. It reports
// how many chunks the record is stored in and how many chunk subjects the bucket holds after the
// day, and requires after each pass that the bucket hold the live chunks alone.
func heartbeatDay(t *testing.T, purge bool) (int, int) {
	t.Helper()
	_, uri := testnats.StartConfig(t, "jetstream {}\n", []string{"-js"}, true)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	clock := &knownClock{at: time.Now()}
	known := openKnownOn(t, conn, clock.Now)
	topics := []string{"notifications.dispatch.issue.K-0.>"}
	for i := range 300 {
		topics = append(topics, fmt.Sprintf("notifications.dispatch.issue.K-0.t%04d-%s", i, strings.Repeat("x", 3900)))
	}
	record := KnownSession{SessionID: "ses_known_day", Topics: topics}
	if err := known.RecordWithin(t.Context(), record); err != nil {
		t.Fatalf("record the session: %v", err)
	}
	row, ok := known.cached("ses_known_day")
	if !ok || !row.stored().Chunked() {
		t.Fatalf("the record of %d bytes of topics is not stored in chunks: %+v", len(strings.Join(topics, "")), row.stored())
	}
	chunks := row.stored().Chunks
	for beat := range 144 {
		clock.set(clock.Now().Add(KnownSessionRefreshAge))
		if err := known.RecordWithin(t.Context(), record); err != nil {
			t.Fatalf("heartbeat %d: %v", beat, err)
		}
		testwait.WithinPolling(t, 10*time.Second, testwait.Poll, "the watcher to deliver the deletes of the replaced chunks", func() bool {
			known.mu.RLock()
			defer known.mu.RUnlock()
			return len(known.markers.Due(clock.Now())) >= chunks
		})
		if !purge {
			continue
		}
		clock.set(clock.Now().Add(kvchunk.MarkerGrace))
		purged, err := known.purgeChunkMarkers(t.Context())
		if err != nil || purged < chunks {
			t.Fatalf("heartbeat %d: the marker pass purged %d markers (%v), want the %d chunks the rewrite replaced", beat, purged, err, chunks)
		}
		if subjects := chunkSubjects(t, conn); subjects != chunks {
			t.Fatalf("heartbeat %d: after the marker pass the bucket holds %d chunk subjects, want the record's %d live chunks", beat, subjects, chunks)
		}
	}
	requireKnownTopics(t, "the store after a heartbeat day", known, "ses_known_day", topics)
	// The watcher delivers the last rewrite's deletes a moment after its write: wait for them, and
	// judge the fence once they are in. With no marker pass the loop above never waits for them.
	fenced := func() int {
		known.mu.RLock()
		defer known.mu.RUnlock()
		return known.chunks.Fenced()
	}
	deadline := time.Now().Add(10 * time.Second)
	for fenced() > chunks && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := fenced(); got != chunks {
		t.Fatalf("after 144 heartbeats the chunk copy's fence holds %d keys, want the record's %d chunks: a replaced chunk's key must leave the fence with its delete", got, chunks)
	}
	return chunks, chunkSubjects(t, conn)
}

// The case: with the marker pass on, a day of heartbeats of a record of about 1.2 MB leaves the
// bucket holding the record's two live chunks and no marker, where the pass runs.
func TestAHeartbeatDayOfAChunkedKnownSessionRecordLeavesOnlyItsLiveChunks(t *testing.T) {
	chunks, subjects := heartbeatDay(t, true)
	t.Logf("after 144 heartbeats with the marker pass: %d chunk subjects, the record in %d chunks", subjects, chunks)
	if chunks != 2 || subjects != chunks {
		t.Fatalf("after a heartbeat day the bucket holds %d chunk subjects for a record in %d chunks, want 2 and 2", subjects, chunks)
	}
}

// The control: the same day with no marker pass leaves every replaced chunk's marker, one subject
// each, which the bucket would keep for its 30-day MaxAge.
func TestAHeartbeatDayOfAChunkedKnownSessionRecordWithNoMarkerPassLeavesEveryMarker(t *testing.T) {
	chunks, subjects := heartbeatDay(t, false)
	t.Logf("after 144 heartbeats with no marker pass: %d chunk subjects, the record in %d chunks", subjects, chunks)
	if want := chunks * 145; subjects != want {
		t.Fatalf("after a heartbeat day with no marker pass the bucket holds %d chunk subjects, want %d: %d live and %d markers", subjects, want, chunks, chunks*144)
	}
}

// lockedLog is a log a store writes from its own goroutines while a test reads it.
type lockedLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// markerRig is a tracing NATS server of a 64 KiB max payload whose one user may publish to
// everything but deny, the known-session store on it with a clock the test moves, and the session
// ses_known_marked whose chunked record one rewrite has replaced, so the watcher has delivered the
// markers of the chunks that rewrite deleted.
type markerRig struct {
	srv    *testnats.Server
	conn   *nats.Conn
	known  *KnownSessions
	clock  *knownClock
	logs   *lockedLog
	before int
}

func newMarkerRig(t *testing.T, deny ...string) markerRig {
	t.Helper()
	seed, public := testnats.User(t)
	quoted := make([]string, len(deny))
	for i, subject := range deny {
		quoted[i] = fmt.Sprintf("%q", subject)
	}
	config := fmt.Sprintf("trace: true\nmax_payload: %d\njetstream {}\nauthorization {\n  users = [ { nkey: %q, permissions: "+
		"{ publish: { allow: [\">\"], deny: [%s] }, subscribe: { allow: [\">\"] } } } ]\n}\n",
		kvchunktest.MaxPayload, public, strings.Join(quoted, ", "))
	srv, uri := testnats.StartConfig(t, config, []string{"-DV", "-js"}, false)
	conn, err := testnats.ConnectSeed(t, uri, seed)
	if err != nil {
		t.Fatalf("connect to the granted NATS: %v", err)
	}
	t.Cleanup(conn.Close)
	clock := &knownClock{at: time.Now()}
	logs := &lockedLog{}
	known, err := OpenKnownSessions(t.Context(), conn, WithKnownSessionReplicas(1), WithKnownSessionClock(clock.Now),
		WithKnownSessionLogger(slog.New(slog.NewTextHandler(logs, nil))))
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if err := known.WaitForCacheReady(context.Background()); err != nil {
		t.Fatalf("the known-session cache did not become ready: %v", err)
	}
	replaced := 0
	for _, label := range []string{"first", "second"} {
		if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_marked", Topics: kvchunktest.Topics(label, 400)}); err != nil {
			t.Fatalf("record the %s record: %v", label, err)
		}
		if label == "first" {
			row, _ := known.cached("ses_known_marked")
			replaced = row.stored().Chunks
		}
	}
	if replaced == 0 {
		t.Fatal("the first record was stored with no chunk, so its rewrite replaced none")
	}
	rig := markerRig{srv: srv, conn: conn, known: known, clock: clock, logs: logs, before: replaced}
	testwait.WithinPolling(t, 10*time.Second, testwait.Poll, "the watcher to deliver the markers of every replaced chunk", func() bool {
		known.mu.RLock()
		defer known.mu.RUnlock()
		return len(known.markers.Due(clock.Now())) == replaced
	})
	// Inside the grace, a pass purges nothing: another listener's watcher may not have dropped the
	// chunk yet.
	clock.set(clock.Now().Add(kvchunk.MarkerGrace - time.Second))
	if purged, err := known.purgeChunkMarkers(t.Context()); err != nil || purged != 0 {
		t.Fatalf("a pass inside the grace purged %d markers (%v), want none", purged, err)
	}
	clock.set(clock.Now().Add(2 * time.Second))
	return rig
}

// traceThrough is every line of the server's log once the trace of a sentinel published after
// everything the store sent is in it.
func (r markerRig) traceThrough(t *testing.T) []string {
	t.Helper()
	sentinel := fmt.Sprintf("trace.sentinel.%d", time.Now().UnixNano())
	if err := r.conn.Publish(sentinel, nil); err != nil {
		t.Fatalf("publish the trace sentinel: %v", err)
	}
	if err := r.conn.Flush(); err != nil {
		t.Fatalf("flush the trace sentinel: %v", err)
	}
	var log string
	testwait.WithinPolling(t, 10*time.Second, testwait.Poll, "the sentinel's trace line", func() bool {
		log = r.srv.Log()
		return strings.Contains(log, "[PUB "+sentinel+" ")
	})
	return strings.Split(log, "\n")
}

// tracedPurgeSubjects is the subject of every stream purge the server traced a client publishing.
func tracedPurgeSubjects(trace []string) []string {
	var subjects []string
	for _, line := range trace {
		_, rest, ok := strings.Cut(line, "<<- [PUB ")
		if !ok {
			_, rest, ok = strings.Cut(line, "<<- [HPUB ")
		}
		if subject, _, _ := strings.Cut(rest, " "); ok && strings.Contains(subject, ".API.STREAM.PURGE.") {
			subjects = append(subjects, subject)
		}
	}
	return subjects
}

// The capability the marker pass needs, off a real server's own trace: one purge of
// $JS.API.STREAM.PURGE.KV_envoy_known_sessions per marker, and no purge of any other stream. Each
// purge is filtered to a chunk's key, so the record keys keep every message, and the pass leaves
// the bucket holding the live chunks and the records alone.
func TestTheChunkMarkerPassSendsTheKnownSessionPurgeItsGrantMustAllow(t *testing.T) {
	rig := newMarkerRig(t)
	purgeSubject := "$JS.API.STREAM.PURGE.KV_" + bus.KnownSessionBucket
	purged, err := rig.known.purgeChunkMarkers(t.Context())
	if err != nil || purged != rig.before {
		t.Fatalf("a pass under a grant that allows the purge purged %d markers (%v), want the %d due", purged, err, rig.before)
	}
	purges := tracedPurgeSubjects(rig.traceThrough(t))
	t.Logf("purges the server traced: %v", purges)
	if len(purges) != purged {
		t.Fatalf("the server traced %d purges for %d markers purged", len(purges), purged)
	}
	for _, subject := range purges {
		if subject != purgeSubject {
			t.Fatalf("the pass published to another purge subject, %s, besides %s", subject, purgeSubject)
		}
	}
	row, _ := rig.known.cached("ses_known_marked")
	if subjects := chunkSubjects(t, rig.conn); subjects != row.stored().Chunks {
		t.Fatalf("after the pass the bucket holds %d chunk subjects, want the record's live chunks alone", subjects)
	}
}

// The negative control is the refusal path a listener whose grant predates this pass must survive:
// a pass the server refuses purges nothing, keeps every marker for the next pass, logs one WARN
// however many passes run, and the store keeps answering its records.
func TestAChunkMarkerPassTheGrantRefusesPurgesNothingWarnsOnceAndTheStoreServes(t *testing.T) {
	purgeSubject := "$JS.API.STREAM.PURGE.KV_" + bus.KnownSessionBucket
	rig := newMarkerRig(t, purgeSubject)
	subjects := chunkSubjects(t, rig.conn)
	for pass := range 2 {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		purged, err := rig.known.purgeChunkMarkers(ctx)
		cancel()
		if err == nil || purged != 0 {
			t.Fatalf("pass %d under a grant that denies the purge purged %d markers (%v), want none and the error", pass, purged, err)
		}
		t.Logf("pass %d: %v", pass, err)
	}
	rig.known.mu.RLock()
	kept := len(rig.known.markers.Due(rig.clock.Now()))
	rig.known.mu.RUnlock()
	if kept != rig.before {
		t.Fatalf("after two refused passes the store keeps %d markers, want the %d due for the next pass", kept, rig.before)
	}
	if after := chunkSubjects(t, rig.conn); after != subjects {
		t.Fatalf("a refused pass changed the bucket's chunk subjects from %d to %d", subjects, after)
	}
	if warnings := strings.Count(rig.logs.String(), "could not purge the markers of its replaced chunks"); warnings != 1 {
		t.Fatalf("two refused passes logged %d warnings, want one:\n%s", warnings, rig.logs.String())
	}
	var violations int
	for _, line := range rig.traceThrough(t) {
		if strings.Contains(line, "Permissions Violation for Publish to") {
			violations++
			if !strings.Contains(line, purgeSubject) {
				t.Fatalf("the grant refused another subject too: %s", strings.TrimSpace(line))
			}
		}
	}
	if violations == 0 {
		t.Fatal("the server logged no permissions violation; the grant did not refuse the purge")
	}
	requireKnownTopics(t, "the store after a refused pass", rig.known, "ses_known_marked", kvchunktest.Topics("second", 400))
}

// A record's own key is never purged: its marker says the session's record was deleted, which a
// listener that starts later must still be told. The pass purges the replaced chunks' markers and
// leaves the deleted record's marker in the bucket, so a store opened afterwards answers no record
// for that session.
func TestTheChunkMarkerPassLeavesARecordsOwnMarker(t *testing.T) {
	rig := newMarkerRig(t)
	js, err := rig.conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.KeyValue(bus.KnownSessionBucket)
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	if err := rig.known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_deleted", Topics: []string{"notifications.team.deleted"}}); err != nil {
		t.Fatalf("record the session to delete: %v", err)
	}
	if err := kv.Delete("ses_known_deleted"); err != nil {
		t.Fatalf("delete the record: %v", err)
	}
	testwait.WithinPolling(t, 10*time.Second, testwait.Poll, "the store to drop the deleted record", func() bool {
		_, ok := rig.known.Get("ses_known_deleted")
		return !ok
	})
	rig.clock.set(rig.clock.Now().Add(kvchunk.MarkerGrace + time.Second))
	if _, err := rig.known.purgeChunkMarkers(t.Context()); err != nil {
		t.Fatalf("the pass: %v", err)
	}
	info, err := js.StreamInfo("KV_"+bus.KnownSessionBucket, &nats.StreamInfoRequest{SubjectsFilter: "$KV." + bus.KnownSessionBucket + ".ses_known_deleted"})
	if err != nil {
		t.Fatalf("read the deleted record's subject: %v", err)
	}
	if len(info.State.Subjects) != 1 {
		t.Fatalf("after the pass the deleted record's key holds %d subjects, want its one marker", len(info.State.Subjects))
	}
	if fresh := openKnownOn(t, rig.conn, time.Now); fresh != nil {
		if _, ok := fresh.Get("ses_known_deleted"); ok {
			t.Fatal("a store opened after the pass answers a record for the deleted session")
		}
	}
}

// openKnownOn opens the known-session store on conn with the clock now and waits for its cache.
func openKnownOn(t *testing.T, conn *nats.Conn, now func() time.Time) *KnownSessions {
	t.Helper()
	known, err := OpenKnownSessions(t.Context(), conn, WithKnownSessionReplicas(1), WithKnownSessionClock(now))
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if err := known.WaitForCacheReady(context.Background()); err != nil {
		t.Fatalf("the known-session cache did not become ready: %v", err)
	}
	return known
}

// rawKnown is the known-session bucket on conn as a raw handle, which reads what is stored with no
// reassembly of its own.
func rawKnown(t *testing.T, conn *nats.Conn) nats.KeyValue {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.KeyValue(bus.KnownSessionBucket)
	if err != nil {
		t.Fatalf("open the known-session bucket: %v", err)
	}
	return kv
}

// useKnownKV rebuilds the store's watcher over kv, a wrapper of its bucket handle that watches
// through the handle's own jetstream one, so the store writes its records through kv from here on.
// The replaced watcher is stopped first.
func useKnownKV(t *testing.T, known *KnownSessions, kv nats.KeyValue) {
	t.Helper()
	base := known.watch()
	base.Stop()
	known.startWatch(bus.NewKeyValue(kv, base.KV().WatchHandle(), base.KV().Streams()))
	if err := known.WaitForCacheReady(context.Background()); err != nil {
		t.Fatalf("wait for the rebuilt watcher: %v", err)
	}
}

// requireKnownTopics fails the test unless known answers sessionID's record with exactly want.
func requireKnownTopics(t *testing.T, what string, known *KnownSessions, sessionID string, want []string) {
	t.Helper()
	record, ok := known.Get(sessionID)
	if !ok {
		t.Fatalf("%s answers no record for %s", what, sessionID)
	}
	if !slices.Equal(record.Topics, want) {
		t.Fatalf("%s answers %s's record with %d topics, want the %d written", what, sessionID, len(record.Topics), len(want))
	}
}

// A known-session record whose encoding is past the server's max payload is stored in chunks a
// manifest under the session's id names, each under that max payload, and the record is answered
// whole by id and by topic, by the store that wrote it and by another listener's store, which its
// watcher fills from the bucket alone. Before this layout the write was refused for its size and
// the session had no record at all.
func TestAKnownSessionRecordPastTheMaxPayloadIsStoredInChunksAndAnsweredWhole(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	known := openKnownOn(t, conn, time.Now)
	topics := kvchunktest.Topics("wide", 600)

	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_wide", Title: "wide", Topics: topics}); err != nil {
		t.Fatalf("record a known session of %d topics past the max payload: %v", len(topics), err)
	}
	requireKnownTopics(t, "the writer's store", known, "ses_known_wide", topics)
	other := openKnownOn(t, conn, time.Now)
	requireKnownTopics(t, "another listener's store", other, "ses_known_wide", topics)
	if got := matchedIDs(other, topics[len(topics)-1]); !slices.Equal(got, []string{"ses_known_wide"}) {
		t.Fatalf("another listener's store matches %v for the record's last topic, want ses_known_wide alone", got)
	}

	kv := rawKnown(t, conn)
	head, err := kv.Get("ses_known_wide")
	if err != nil {
		t.Fatalf("read the session's key: %v", err)
	}
	if !kvchunk.IsManifest(head.Value()) {
		t.Fatalf("the session's key holds %.60q, want a manifest", head.Value())
	}
	chunks := kvchunktest.LiveChunkKeys(t, kv)
	if len(chunks) < 3 {
		t.Fatalf("the bucket holds %d chunks, want the record in 3 or more", len(chunks))
	}
	for _, key := range chunks {
		chunk, err := kv.Get(key)
		if err != nil {
			t.Fatalf("read chunk %s: %v", key, err)
		}
		if len(chunk.Value()) > kvchunktest.MaxPayload {
			t.Fatalf("chunk %s holds %d bytes, past the server's max payload of %d", key, len(chunk.Value()), kvchunktest.MaxPayload)
		}
	}
}

// The control: a known-session record under the max payload is stored whole under the session's
// id, as every earlier build stores it, with no chunk.
func TestAKnownSessionRecordUnderTheMaxPayloadIsStoredWholeAsBefore(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	known := openKnownOn(t, conn, time.Now)
	topics := kvchunktest.Topics("narrow", 10)
	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_narrow", Topics: topics}); err != nil {
		t.Fatalf("record a known session of %d topics: %v", len(topics), err)
	}
	kv := rawKnown(t, conn)
	head, err := kv.Get("ses_known_narrow")
	if err != nil {
		t.Fatalf("read the session's key: %v", err)
	}
	var stored KnownSession
	if err := json.Unmarshal(head.Value(), &stored); err != nil || !slices.Equal(stored.Topics, topics) {
		t.Fatalf("the session's key holds %.60q (%v), want the record whole as JSON", head.Value(), err)
	}
	if chunks := kvchunktest.LiveChunkKeys(t, kv); len(chunks) != 0 {
		t.Fatalf("the bucket holds chunks %v for a record that fits one message", chunks)
	}
}

// A rewrite that makes a chunked known-session record fit one message stores it whole and deletes
// every chunk it had, and a rewrite that grows it again stores a new set of chunks and deletes the
// old set, so no write leaves a chunk no record names.
func TestAKnownSessionRewriteThatShrinksOrGrowsLeavesNoStaleChunk(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	known := openKnownOn(t, conn, time.Now)
	kv := rawKnown(t, conn)
	wide, narrow := kvchunktest.Topics("wide", 400), kvchunktest.Topics("narrow", 10)

	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_shrink", Topics: wide}); err != nil {
		t.Fatalf("record the chunked record: %v", err)
	}
	if len(kvchunktest.LiveChunkKeys(t, kv)) == 0 {
		t.Fatal("the record past the max payload was stored with no chunk")
	}
	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_shrink", Topics: narrow}); err != nil {
		t.Fatalf("shrink the record: %v", err)
	}
	if chunks := kvchunktest.LiveChunkKeys(t, kv); len(chunks) != 0 {
		t.Fatalf("after the record shrank to one message the bucket still holds chunks %v", chunks)
	}
	requireKnownTopics(t, "the store after the shrink", known, "ses_known_shrink", narrow)

	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_shrink", Topics: wide}); err != nil {
		t.Fatalf("grow the record again: %v", err)
	}
	grown := kvchunktest.LiveChunkKeys(t, kv)
	more := kvchunktest.Topics("more", 500)
	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_shrink", Topics: more}); err != nil {
		t.Fatalf("grow the record further: %v", err)
	}
	further := kvchunktest.LiveChunkKeys(t, kv)
	for _, key := range grown {
		if slices.Contains(further, key) {
			t.Fatalf("after the record grew again the bucket still holds %s, a chunk of the record it replaced", key)
		}
	}
	if generations := kvchunktest.Generations(further); len(generations) != 1 {
		t.Fatalf("the bucket holds chunks of %d records %v, want the one stored", len(generations), generations)
	}
	requireKnownTopics(t, "the store after the growth", known, "ses_known_shrink", more)
}

// knownClock is a store clock (WithKnownSessionClock) the test sets by hand, read by the store's
// own goroutines too.
type knownClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *knownClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *knownClock) set(at time.Time) {
	c.mu.Lock()
	c.at = at
	c.mu.Unlock()
}

// A known-session write that stops at any step - between two chunks, between its chunks and its
// manifest, or after chunk writes that took past the write's bound - leaves the record it would
// have replaced answered whole, by the store and by another listener's store. What it left behind
// is chunks no record names, which the store's sweep collects once they are older than the
// collection grace and not before, and which no collection judges while the cache does not follow
// its bucket.
func TestAKnownSessionWriteStoppedAtAnyStepLeavesTheOldRecordAndItsChunksCollected(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	clock := &knownClock{at: time.Now()}
	known := openKnownOn(t, conn, clock.Now)
	other := openKnownOn(t, conn, time.Now)
	kv := rawKnown(t, conn)
	legacy := known.watch().KV().Legacy()
	old := kvchunktest.Topics("old", 400)
	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_crashed", Topics: old}); err != nil {
		t.Fatalf("record the old record: %v", err)
	}
	served := kvchunktest.LiveChunkKeys(t, kv)

	for _, stopped := range []struct {
		step, label string
		kv          nats.KeyValue
	}{
		{"between two chunks", "chunks", &kvchunktest.ChunkFailing{KeyValue: legacy, FailAt: 2}},
		{"between its chunks and its manifest", "manifest", &kvchunktest.HeadFailing{KeyValue: legacy, Key: "ses_known_crashed"}},
		{"after its chunks took past the write's bound", "bound", &kvchunktest.ChunkFailing{KeyValue: legacy, Before: func() {
			clock.set(clock.Now().Add(kvchunk.WriteBound))
		}}},
	} {
		useKnownKV(t, known, stopped.kv)
		err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_crashed", Topics: kvchunktest.Topics(stopped.label, 300)})
		if err == nil {
			t.Fatalf("a write stopped %s answered success", stopped.step)
		}
		t.Logf("a write stopped %s answered: %v", stopped.step, err)
		// The clock the cutoff reads moved with the bound case; the retention reads it too.
		clock.set(time.Now())
		requireKnownTopics(t, "the store after a write stopped "+stopped.step, known, "ses_known_crashed", old)
		testwait.WithinPolling(t, 10*time.Second, testwait.Poll, "another listener's store to answer the old record", func() bool {
			record, ok := other.Get("ses_known_crashed")
			return ok && slices.Equal(record.Topics, old)
		})
	}
	useKnownKV(t, known, legacy)
	orphans := slices.DeleteFunc(kvchunktest.LiveChunkKeys(t, kv), func(key string) bool { return slices.Contains(served, key) })
	if len(orphans) == 0 {
		t.Fatal("the stopped writes left no chunk behind, want the chunks of the write whose manifest failed")
	}

	if collected, err := known.collectChunks(t.Context()); err != nil || collected != 0 {
		t.Fatalf("a collection inside the grace collected %d chunks (%v), want none", collected, err)
	}
	clock.set(time.Now().Add(kvchunk.Grace + 10*time.Minute))
	known.Disconnected()
	if collected, err := known.collectChunks(t.Context()); err != nil || collected != 0 {
		t.Fatalf("a collection while the cache does not follow its bucket collected %d chunks (%v), want none", collected, err)
	}
	if err := known.Rewatch(t.Context(), conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	testwait.WithinPolling(t, 10*time.Second, testwait.Poll, "the store's cache to follow its bucket again", known.watch().Following)
	collected, err := known.collectChunks(t.Context())
	if err != nil {
		t.Fatalf("collect the chunks past the grace: %v", err)
	}
	if collected != len(orphans) {
		t.Fatalf("the collection past the grace collected %d chunks, want the %d no record names", collected, len(orphans))
	}
	if after := kvchunktest.LiveChunkKeys(t, kv); !slices.Equal(after, served) {
		t.Fatalf("after the collection the bucket holds chunks %v, want the served record's %v", after, served)
	}
	clock.set(time.Now())
	requireKnownTopics(t, "the store after the collection", known, "ses_known_crashed", old)
}

// A chunked known-session record with a chunk missing is a record the store cannot decode: a store
// that reads the bucket afresh answers no record for it, never a partial one.
func TestAChunkedKnownSessionRecordWithAMissingChunkIsNoRecordNotAPartialOne(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	known := openKnownOn(t, conn, time.Now)
	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_damaged", Topics: kvchunktest.Topics("damaged", 400)}); err != nil {
		t.Fatalf("record the record: %v", err)
	}
	kv := rawKnown(t, conn)
	chunks := kvchunktest.LiveChunkKeys(t, kv)
	if err := kv.Delete(chunks[len(chunks)/2]); err != nil {
		t.Fatalf("delete a chunk: %v", err)
	}
	fresh := openKnownOn(t, conn, time.Now)
	if record, ok := fresh.Get("ses_known_damaged"); ok {
		t.Fatalf("a store that read the bucket afresh answered a record of %d topics for a record missing a chunk", len(record.Topics))
	}
}

// A manifest no write of this build makes - here one claiming more chunks than the record has
// bytes, which a store that allocated by the claim would crash on - is a record the store cannot
// decode: a store that reads the bucket afresh keeps running and answers no record, and the
// session's next write replaces it.
func TestAForgedKnownSessionManifestIsNoRecordAndTheNextWriteReplacesIt(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	forged := `{"chunked":{"generation":"AAAAAAAAAAAAAAAAAAAAAAAAAA","chunks":4611686018427387904,"bytes":10,"sha256":"` + strings.Repeat("0", 64) + `"}}`
	// The forgery is a well-formed manifest whose one fault is its chunk count, so the store meets
	// that count rather than a decode error.
	if !json.Valid([]byte(forged)) {
		t.Fatalf("the forged manifest %s is not valid JSON, so the case would not reach its chunk count", forged)
	}
	known := openKnownOn(t, conn, time.Now)
	if _, err := rawKnown(t, conn).Put("ses_known_forged", []byte(forged)); err != nil {
		t.Fatalf("store the forged manifest: %v", err)
	}
	fresh := openKnownOn(t, conn, time.Now)
	if record, ok := fresh.Get("ses_known_forged"); ok {
		t.Fatalf("a store that read the bucket afresh answered a record of %d topics for a forged manifest", len(record.Topics))
	}
	topics := kvchunktest.Topics("replacing", 10)
	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_forged", Topics: topics}); err != nil {
		t.Fatalf("the write over the forged manifest: %v", err)
	}
	testwait.WithinPolling(t, 10*time.Second, testwait.Poll, "a store that read the bucket afresh to answer the record that replaced it", func() bool {
		record, ok := fresh.Get("ses_known_forged")
		return ok && slices.Equal(record.Topics, topics)
	})
}

// A manifest copied under another session's key names the owner's chunks, and a known-session
// write is a put, not a compare-and-swap, so the copy's session's next write lands over it: that
// write leaves the owner's chunks alone, and the owner's record still reads whole in a store that
// reads the bucket afresh. The control is the owner's own rewrite, which deletes its old chunks.
func TestAKnownSessionWriteOverACopiedManifestLeavesTheOwnersChunks(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	known := openKnownOn(t, conn, time.Now)
	kv := rawKnown(t, conn)
	topics := kvchunktest.Topics("owner", 400)
	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_owner", Topics: topics}); err != nil {
		t.Fatalf("record the owner: %v", err)
	}
	head, err := kv.Get("ses_known_owner")
	if err != nil {
		t.Fatalf("read the owner's manifest: %v", err)
	}
	chunks := kvchunktest.LiveChunkKeys(t, kv)
	if _, err := kv.Put("ses_known_copy", head.Value()); err != nil {
		t.Fatalf("store the copy: %v", err)
	}
	testwait.WithinPolling(t, 10*time.Second, testwait.Poll, "the store to hold the copy as a row", func() bool {
		_, ok := known.cached("ses_known_copy")
		return ok
	})
	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_copy", Topics: []string{"notifications.team.copy"}}); err != nil {
		t.Fatalf("the write over the copy: %v", err)
	}
	if live := kvchunktest.LiveChunkKeys(t, kv); !slices.Equal(live, chunks) {
		t.Fatalf("after the write over the copy the bucket holds chunks %v, want the owner's %v", live, chunks)
	}
	requireKnownTopics(t, "a store that read the bucket afresh", openKnownOn(t, conn, time.Now), "ses_known_owner", topics)

	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: "ses_known_owner", Topics: kvchunktest.Topics("again", 400)}); err != nil {
		t.Fatalf("the owner's rewrite: %v", err)
	}
	for _, key := range kvchunktest.LiveChunkKeys(t, kv) {
		if slices.Contains(chunks, key) {
			t.Fatalf("after the owner's own rewrite the bucket still holds %s, a chunk of the record it replaced", key)
		}
	}
}

// A known-session write deletes the chunks of the record its listener's copy holds, not those of
// the bucket's head, and that copy can be behind the bucket (internal/bus/kv_leader_read.go). Each
// write names its chunks by a generation of its own, so a copy behind the bucket names a generation
// no head names once the write's own head lands, and the write deletes only chunks no head names.
// Listener A's copy holds the session's chunked record G0 and its watch is stopped, so it takes
// nothing more. Listener B, whose watch was stopped before G0 so its copy holds no record of the
// session and its write deletes nothing, writes G1. A then writes G2 over its copy's G0: the bucket
// holds every chunk G2's manifest names and the record reads back whole, G1's chunks are still
// there for the collection, which this row does not run, and G0's are gone. Control: with the
// write deleting the layout of the bucket's head (G1's, read from the bucket) in place of its
// copy's, G1's chunks are gone and the row fails.
func TestAKnownSessionWriteFromACopyBehindTheBucketDeletesOnlyChunksNoHeadNames(t *testing.T) {
	const sessionID = "ses_known_behind"
	conn := kvchunktest.SmallPayloadNATS(t)
	known := openKnownOn(t, conn, time.Now)
	other := openKnownOn(t, conn, time.Now)
	other.StopWatch()
	kv := rawKnown(t, conn)

	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: sessionID, Topics: kvchunktest.Topics("g0", 400)}); err != nil {
		t.Fatalf("record G0 from listener A: %v", err)
	}
	g0 := kvchunktest.HeadLayout(t, kv, sessionID)
	known.StopWatch()
	if err := other.RecordWithin(t.Context(), KnownSession{SessionID: sessionID, Topics: kvchunktest.Topics("g1", 400)}); err != nil {
		t.Fatalf("record G1 from listener B: %v", err)
	}
	g1 := kvchunktest.HeadLayout(t, kv, sessionID)
	if g1.Generation == g0.Generation {
		t.Fatalf("G1's head names G0's generation %s, want one of its own", g0.Generation)
	}
	if held, ok := known.cached(sessionID); !ok || held.stored().Generation != g0.Generation {
		t.Fatalf("once G1 landed listener A's copy holds generation %q (%v), want G0's %s: the row says nothing about a copy behind the bucket",
			held.stored().Generation, ok, g0.Generation)
	}

	topics := kvchunktest.Topics("g2", 400)
	if err := known.RecordWithin(t.Context(), KnownSession{SessionID: sessionID, Topics: topics}); err != nil {
		t.Fatalf("record G2 from listener A's copy behind the bucket: %v", err)
	}
	g2 := kvchunktest.HeadLayout(t, kv, sessionID)
	if g2.Generation == g0.Generation || g2.Generation == g1.Generation {
		t.Fatalf("after G2's write the head names generation %s, want one neither G0 (%s) nor G1 (%s) names", g2.Generation, g0.Generation, g1.Generation)
	}
	live := kvchunktest.LiveChunkKeys(t, kv)
	for _, key := range g2.Keys() {
		if !slices.Contains(live, key) {
			t.Fatalf("after G2's write the bucket holds no %s, a chunk its head names", key)
		}
	}
	requireKnownTopics(t, "a store that read the bucket afresh", openKnownOn(t, conn, time.Now), sessionID, topics)
	for _, key := range g1.Keys() {
		if !slices.Contains(live, key) {
			t.Fatalf("after G2's write the bucket holds no %s, a chunk of G1, which listener A's copy never held: the write deleted the head's chunks, not its copy's", key)
		}
	}
	for _, key := range g0.Keys() {
		if slices.Contains(live, key) {
			t.Fatalf("after G2's write the bucket still holds %s, a chunk of G0, the record listener A's copy held", key)
		}
	}
}

// While builds share the known-session bucket, a listener of an earlier build reads a chunked
// record's manifest as a KnownSession: every earlier build decodes a value with encoding/json into
// that type and ignores a member it does not know (origin/main's decodeKnownSession). So the
// manifest carries the session's id, title, capabilities and last-seen time, and the covering
// topics, which match every subject the full list does: that listener keeps sending to and counting
// the stopped session. This build reads the same manifest as the full record.
func TestAnEarlierBuildReadsAChunkedKnownSessionRecordAsTheSameSession(t *testing.T) {
	conn := kvchunktest.SmallPayloadNATS(t)
	known := openKnownOn(t, conn, time.Now)
	registered := []string{"notifications.dispatch.issue.K-0.>"}
	for i := range 400 {
		registered = append(registered, fmt.Sprintf("notifications.dispatch.issue.K-0.t%04d-%s", i, strings.Repeat("x", 270)))
	}
	record := KnownSession{SessionID: "ses_known_overlap", Title: "overlap", Capabilities: []string{"steer"}, Topics: registered}
	if err := known.RecordWithin(t.Context(), record); err != nil {
		t.Fatalf("record the session: %v", err)
	}
	head, err := rawKnown(t, conn).Get("ses_known_overlap")
	if err != nil {
		t.Fatalf("read the session's key: %v", err)
	}
	if !kvchunk.IsManifest(head.Value()) {
		t.Fatalf("the session's key holds %.60q, want a manifest", head.Value())
	}
	var earlier KnownSession
	if err := json.Unmarshal(head.Value(), &earlier); err != nil {
		t.Fatalf("an earlier build's decode of the manifest: %v, want the session", err)
	}
	if earlier.SessionID != record.SessionID || earlier.Title != "overlap" || !slices.Equal(earlier.Capabilities, []string{"steer"}) || earlier.LastSeen == 0 {
		t.Fatalf("an earlier build reads the manifest as %+v, want ses_known_overlap titled overlap with its capability and last-seen time", earlier)
	}
	if want := []string{"notifications.dispatch.issue.K-0.>"}; !slices.Equal(earlier.Topics, want) {
		t.Fatalf("an earlier build reads the topics %v, want the covering %v", earlier.Topics, want)
	}
	requireKnownTopics(t, "this build's store", openKnownOn(t, conn, time.Now), "ses_known_overlap", registered)
}
