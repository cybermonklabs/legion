package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// Through a silent cut each reconnect attempt of the role lane's client holds nats.go's lock of
// its connection across the attempt's dial and handshake, up to the client's connect timeout, and
// a read of the client's status waits that out. /healthz reads the role lane within
// roleLaneStatusRead (bus.ConnectedWithin), so while the role lane is in the middle of such an
// attempt it answers at once that the role lane is down, rather than when the attempt has failed.
func TestHealthzAnswersAtOnceWhileTheRoleLaneIsMidReconnect(t *testing.T) {
	const consumer = "listener-healthz-role-lane-reconnect"
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
	proxy := testnats.StartProxy(t, client.Conn.ConnectedUrl())
	roleLane, err := bus.Connect([]string{proxy.URL()}, bus.WithSilentCutDetection())
	if err != nil {
		t.Fatalf("connect the role lane's client: %v", err)
	}
	t.Cleanup(roleLane.Close)
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, roleLane: roleLane, registry: registry, sessions: sessions, caches: listenerCaches(registry, sessions, nil), consumer: consumer})
	handler := healthzHandler(&state)
	proxy.Freeze()
	t.Cleanup(proxy.Restore)

	// The pings find the cut two to three seconds in, and the first attempt starts a ReconnectWait
	// later: wait for a read of the role lane's status that does not answer, which is that attempt.
	deadline := time.Now().Add(30 * time.Second)
	for {
		read := make(chan struct{})
		go func() {
			roleLane.Connected()
			close(read)
		}()
		select {
		case <-read:
			if time.Now().After(deadline) {
				t.Fatal("no read of the role lane's status waited within 30 s of the freeze")
			}
			time.Sleep(50 * time.Millisecond)
			continue
		case <-time.After(500 * time.Millisecond):
		}
		recorder := httptest.NewRecorder()
		started := time.Now()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		took := time.Since(started)
		var body map[string]any
		_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		t.Logf("with the role lane in the middle of a reconnect attempt, /healthz answered %d %v in %v", recorder.Code, body, took.Round(time.Millisecond))
		if took > time.Second {
			t.Errorf("/healthz answered in %v with the role lane in the middle of a reconnect attempt, want within a second", took.Round(time.Millisecond))
		}
		if recorder.Code != http.StatusServiceUnavailable || body["error"] != "role lane subscription inactive" {
			t.Errorf("/healthz = %d %v with the role lane in the middle of a reconnect attempt, want 503 naming the role lane", recorder.Code, body)
		}
		<-read
		return
	}
}
