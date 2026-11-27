package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

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

// An attempt to open the bucket while nats.go reconnects the connection fails as
// bus.ErrReconnecting, and the start's one WARN names the reconnect. nats.go refuses the bucket's
// lookup then before it sends anything, with words of its own
// (`nats: key-value requires at least server version 2.6.2`), since a reconnecting connection
// reports no server version, and those words name no reconnect.
func TestAKnownBucketOpenWhileNATSReconnectsFailsByName(t *testing.T) {
	srv, uri := testnats.Start(t)
	client, err := bus.Connect([]string{uri}, bus.WithName(t.Name()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	srv.Stop(t)
	if !testwait.Holds(10*time.Second, 10*time.Millisecond, func() bool { return client.Conn.Status() == nats.RECONNECTING }) {
		t.Fatalf("the connection reads %v 10s after its server stopped, want RECONNECTING", client.Conn.Status())
	}

	if _, err := openKnownBucket(context.Background(), client.Conn, 1); !errors.Is(err, bus.ErrReconnecting) {
		t.Fatalf("the open while NATS reconnects failed with %v, want bus.ErrReconnecting", err)
	}

	logs := &lockedWriter{}
	known, err := OpenKnownSessions(t.Context(), client.Conn,
		WithKnownSessionReplicas(1),
		WithKnownSessionLogger(slog.New(slog.NewTextHandler(logs, nil))),
		// The retry waits an hour, so the start's own attempt is the one the WARN reports.
		withKnownSessionRetry(time.Hour, time.Hour))
	if err != nil {
		t.Fatalf("open the store while NATS reconnects: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if warned := logs.Lines("the known-session bucket is not open"); !strings.Contains(warned, "bus: NATS is reconnecting") {
		t.Fatalf("the start's WARN reads %q, want it to name the reconnect: bus: NATS is reconnecting", warned)
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
	bustest.Fixture(conn)
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
	bustest.Fixture(conn)
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
