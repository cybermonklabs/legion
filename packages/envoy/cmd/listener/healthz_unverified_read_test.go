package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A cache whose every read could not read its bucket past a barrier - the barrier is never written
// - for longer than the copy's stale bound is unhealthy on /healthz: 503, naming that cache's KV
// read and the error. Within the bound the listener is still healthy, since its copy is still
// served (kvwatch.Watcher.Servable) and a blip of the bucket repairs itself.
func TestHealthzIsUnhealthyOnceACachesReadsCannotReadPastABarrierForTheBound(t *testing.T) {
	const consumer = "listener-healthz-unverified-read"
	client := setupTestNATS(t)
	bindBoundDurable(t, client, consumer, func(msg *natsgo.Msg) { _ = msg.Ack() })
	registry, err := store.Open(t.Context(), client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open interest registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	sessions, err := session.OpenSessionRegistry(t.Context(), client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer readyCancel()
	if err := registry.WaitForCacheReady(readyCtx); err != nil {
		t.Fatalf("wait for the registry's caches: %v", err)
	}
	if err := sessions.WaitForCacheReady(readyCtx); err != nil {
		t.Fatalf("wait for the session cache: %v", err)
	}

	// The session cache's reads cannot read past a barrier: a watcher on a bucket whose barriers
	// are never written, on a clock the test moves.
	clock := &listenerClock{now: time.Now()}
	waits := &heldWaits{}
	unverified := unverifiedReadWatcher(t, client.Conn, clock.Now, waits)
	caches := listenerCaches(registry, sessions, nil)
	for i := range caches {
		if caches[i].name == "session" {
			caches[i].cache = unverifiedReads{kvCache: caches[i].cache, watcher: unverified}
		}
	}
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions, caches: caches, consumer: consumer})
	get := func(when string) (int, map[string]any) {
		t.Helper()
		recorder := httptest.NewRecorder()
		healthzHandler(&state).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		t.Logf("%s: /healthz answered %d %s", when, recorder.Code, strings.TrimSpace(recorder.Body.String()))
		var body map[string]any
		_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		return recorder.Code, body
	}

	unverified.Start()
	waitFor(t, 10*time.Second, "the first read that could not read past a barrier", func() bool { return waits.armed() > 0 })
	if code, body := get("after the first failed read"); code != http.StatusOK || body["status"] != "healthy" {
		t.Fatalf("/healthz within the stale bound = %d %v, want 200 healthy", code, body)
	}
	clock.Advance(2*time.Minute - time.Second)
	if code, body := get("a second before the 2m stale bound"); code != http.StatusOK || body["status"] != "healthy" {
		t.Fatalf("/healthz a second before the stale bound = %d %v, want 200 healthy", code, body)
	}
	clock.Advance(2 * time.Second)
	code, body := get("past the stale bound")
	reported, _ := body["error"].(string)
	if code != http.StatusServiceUnavailable || body["status"] != "unhealthy" ||
		!strings.HasPrefix(reported, "session KV read unavailable: session registry (KV bucket healthz-unverified-read): "+kvwatch.ErrUnverified.Error()) ||
		!strings.Contains(reported, natsgo.ErrConnectionClosed.Error()) {
		t.Fatalf("/healthz past the stale bound = %d %v, want 503 unhealthy naming the session KV read and its watcher's bucket, %q and the reads' own %q",
			code, body, kvwatch.ErrUnverified, natsgo.ErrConnectionClosed)
	}
}

// The known-session store is none of the listener's caches, so its reads that fail to settle past
// their barrier for longer than the store's stale bound are reported apart: 200 "degraded",
// naming the store's KV read, its watcher and its bucket, with the healthy answer's fields kept.
// The store is on a server of its own, whose one user may do everything the store does except
// publish to the bucket's subjects, so every read's barrier is refused, as a denied publish is in
// production. The bucket is made first with a maximum age of 2 s, which the store keeps, so the
// stale bound is 1 s and the answer comes on the first read that fails.
func TestHealthzIsDegradedOnceTheKnownSessionStoresReadsCannotReadPastABarrier(t *testing.T) {
	const consumer = "listener-healthz-known-read"
	client := setupTestNATS(t)
	bindBoundDurable(t, client, consumer, func(msg *natsgo.Msg) { _ = msg.Ack() })
	registry, err := store.Open(t.Context(), client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open interest registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	sessions, err := session.OpenSessionRegistry(t.Context(), client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer readyCancel()
	if err := registry.WaitForCacheReady(readyCtx); err != nil {
		t.Fatalf("wait for the registry's caches: %v", err)
	}
	if err := sessions.WaitForCacheReady(readyCtx); err != nil {
		t.Fatalf("wait for the session cache: %v", err)
	}

	stream := "KV_" + bus.KnownSessionBucket
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public,
		"$JS.API.INFO",
		"$JS.API.STREAM.INFO."+stream,
		"$JS.API.STREAM.CREATE."+stream,
		"$JS.API.DIRECT.GET."+stream+".>",
		"$JS.API.STREAM.MSG.GET."+stream,
		"$JS.API.CONSUMER.CREATE."+stream+".>",
		"$JS.API.CONSUMER.DELETE."+stream+".>",
		"$JS.FC."+stream+".>")
	conn, err := testnats.ConnectSeed(t, grant.URL, seed)
	if err != nil {
		t.Fatalf("connect as the user who may not publish to the bucket: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := bus.EnsureKeyValue(t.Context(), conn, js, &natsgo.KeyValueConfig{Bucket: bus.KnownSessionBucket, TTL: 2 * time.Second, Storage: natsgo.MemoryStorage}); err != nil {
		t.Fatalf("make the known-session bucket with a 2 s maximum age: %v", err)
	}
	known, err := session.OpenKnownSessions(t.Context(), conn, session.WithKnownSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)

	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions, known: known,
		caches: listenerCaches(registry, sessions, nil), consumer: consumer})
	var code int
	var body map[string]any
	waitFor(t, 60*time.Second, "/healthz to report the known-session store's reads", func() bool {
		recorder := httptest.NewRecorder()
		healthzHandler(&state).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		code, body = recorder.Code, nil
		_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		return body["status"] != "healthy"
	})
	reported, _ := body["error"].(string)
	want := "known session KV read unavailable: known session store (KV bucket " + bus.KnownSessionBucket + "): " + kvwatch.ErrUnverified.Error()
	if code != http.StatusOK || body["status"] != "degraded" || !strings.HasPrefix(reported, want) {
		t.Fatalf("/healthz once the known-session store's reads failed past its bound = %d %v, want 200 degraded starting %q", code, body, want)
	}
	if _, kept := body["num_pending"]; !kept {
		t.Fatalf("the degraded answer %v dropped the healthy answer's fields", body)
	}
}

// unverifiedReads is the listener's session cache with ReadErr taken from watcher, whose reads
// cannot read past a barrier; every other method is the session cache's own.
type unverifiedReads struct {
	kvCache
	watcher *kvwatch.Watcher
}

func (c unverifiedReads) ReadErr() error { return c.watcher.ReadErr() }

// unverifiedReadWatcher is a watcher, not yet started, over a bucket of its own on conn whose read
// barriers (bus.KeyValue.WriteBarrier) fail as a closed connection's publishes do, so every read
// fails, over a copy that keeps nothing. It measures on now and arms its waits through waits.
func unverifiedReadWatcher(t *testing.T, conn *natsgo.Conn, now func() time.Time, waits *heldWaits) *kvwatch.Watcher {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := bus.EnsureKeyValue(t.Context(), conn, js, &natsgo.KeyValueConfig{Bucket: "healthz-unverified-read"})
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	handle := bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(), failedBarriers{JetStream: kv.Streams()})
	w := kvwatch.New("session registry", handle, discardingCache{}, kvwatch.WithNow(now), kvwatch.WithAfterFunc(waits.after),
		kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	return w
}

// failedBarriers is a bucket's jetstream context whose publishes fail as a closed connection's do:
// a read's barrier is the one publish through it, so every read fails, while every stream read and
// watch goes on as usual.
type failedBarriers struct {
	jetstream.JetStream
}

func (failedBarriers) PublishMsg(context.Context, *natsgo.Msg, ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	return nil, natsgo.ErrConnectionClosed
}

// heldWaits is a kvwatch.AfterFunc that keeps every wait and fires none: the watcher's re-reads
// wait for the test, which reads only how many re-reads of a failed read were armed (the 5 s wait
// the first of a run of failed reads arms).
type heldWaits struct {
	mu      sync.Mutex
	rereads int
}

func (h *heldWaits) after(d time.Duration, _ func()) kvwatch.Timer {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d == 5*time.Second {
		h.rereads++
	}
	return heldWait{}
}

func (h *heldWaits) armed() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rereads
}

// heldWait is a wait heldWaits keeps, which never fires.
type heldWait struct{}

func (heldWait) Stop() bool { return true }
