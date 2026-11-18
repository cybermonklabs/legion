package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// subjectStallRelay forwards NATS client connections to a server. Once stall names subject
// prefixes, it drops each PUB or HPUB from the client whose subject starts with one, payload and
// all, and forwards every other line: the server never sees those requests, so they get no
// answer, while a core PING still gets its PONG. It is how JetStream alone stops answering.
type subjectStallRelay struct {
	listener net.Listener
	target   string
	relayed  string

	mu       sync.Mutex
	prefixes []string
	conns    []net.Conn
}

func startSubjectStallRelay(t *testing.T, uri string) *subjectStallRelay {
	t.Helper()
	server, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse NATS URL %q: %v", uri, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for relay: %v", err)
	}
	relayed := *server
	relayed.Host = listener.Addr().String()
	relay := &subjectStallRelay{listener: listener, target: server.Host, relayed: relayed.String()}
	t.Cleanup(relay.close)
	go relay.serve()
	return relay
}

func (r *subjectStallRelay) serve() {
	for {
		client, err := r.listener.Accept()
		if err != nil {
			return
		}
		go r.relay(client)
	}
}

func (r *subjectStallRelay) relay(client net.Conn) {
	server, err := net.Dial("tcp", r.target)
	if err != nil {
		_ = client.Close()
		return
	}
	r.mu.Lock()
	r.conns = append(r.conns, client, server)
	r.mu.Unlock()
	defer func() { _ = server.Close() }()
	go func() {
		_, _ = io.Copy(client, server)
		_ = client.Close()
	}()
	lines := bufio.NewReader(client)
	for {
		line, err := lines.ReadBytes('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(string(line))
		var payload []byte
		if len(fields) >= 3 && (strings.EqualFold(fields[0], "PUB") || strings.EqualFold(fields[0], "HPUB")) {
			size, err := strconv.Atoi(fields[len(fields)-1])
			if err != nil {
				return
			}
			payload = make([]byte, size+len("\r\n"))
			if _, err := io.ReadFull(lines, payload); err != nil {
				return
			}
			if r.stalls(fields[1]) {
				continue
			}
		}
		if _, err := server.Write(append(line, payload...)); err != nil {
			return
		}
	}
}

func (r *subjectStallRelay) stalls(subject string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.ContainsFunc(r.prefixes, func(prefix string) bool { return strings.HasPrefix(subject, prefix) })
}

// stall drops every request from here on whose subject starts with one of prefixes.
func (r *subjectStallRelay) stall(prefixes ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prefixes = prefixes
}

func (r *subjectStallRelay) close() {
	_ = r.listener.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, conn := range r.conns {
		_ = conn.Close()
	}
}

// /healthz flushes the connection first, a core PING, and then pings each cache's bucket and reads
// the durable consumer's info, which are JetStream requests. nats.go's KV Status takes no context
// and the info read waited out the JetStream MaxWait (10 s), so a JetStream that stops answering
// while core NATS still does held /healthz past a probe's own timeout, and the container health
// check (`curl -sf`) failed. Each must answer inside /healthz's 3 s budget instead, and still fail
// that check: 503 unhealthy, naming the dependency and the context's deadline. The negative control
// is /healthz on the same rig before the stall: 200 healthy.
func TestHealthzAnswersAJetStreamThatStoppedAnsweringInsideItsBudget(t *testing.T) {
	for _, stall := range []struct {
		name     string
		prefixes []string
		want     string
	}{
		{"every JetStream request", []string{"$JS.", "$KV."}, "interest KV unavailable: context deadline exceeded"},
		{"the durable consumer's info read", []string{"$JS.API.CONSUMER.INFO."}, "durable consumer unavailable: context deadline exceeded"},
	} {
		t.Run(stall.name, func(t *testing.T) {
			t.Parallel()
			relay := startSubjectStallRelay(t, testnats.URL(t))
			client, err := bus.ConnectOwningStream([]string{relay.relayed}, bus.WithReplicas(1))
			if err != nil {
				t.Fatalf("connect the listener's bus through the relay: %v", err)
			}
			t.Cleanup(client.Close)
			const consumer = "listener-healthz-jetstream-stall"
			bindBoundDurable(t, client, consumer, func(msg *natsgo.Msg) { _ = msg.Ack() })
			registry, err := store.Open(client.Conn, store.WithReplicas(1))
			if err != nil {
				t.Fatalf("open the interest registry: %v", err)
			}
			t.Cleanup(registry.StopWatch)
			sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
			if err != nil {
				t.Fatalf("open the session registry: %v", err)
			}
			t.Cleanup(sessions.StopWatch)
			ready := testwait.Context(t)
			if err := registry.WaitForCacheReady(ready); err != nil {
				t.Fatalf("wait for the interest cache: %v", err)
			}
			if err := sessions.WaitForCacheReady(ready); err != nil {
				t.Fatalf("wait for the session cache: %v", err)
			}
			// The route reports a JetStream that stopped answering as the deadline its own budget
			// imposes, "context deadline exceeded", rather than nats.go's 10 s MaxWait, whose
			// failure reads differently ("nats: timeout"/no responders). That the stalled read
			// answers with the budget's own deadline is the whole of what this test holds, so it
			// is read from the error the 503 carries rather than from how long the answer took:
			// /healthz cannot run on a fake clock, since its flush (FlushWithContext) refuses a
			// context with no real deadline.
			var state atomic.Pointer[listenerDeps]
			state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions, caches: listenerCaches(registry, sessions, nil), consumer: consumer})
			get := func() (int, map[string]any) {
				recorder := httptest.NewRecorder()
				healthzHandler(&state).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
				var body map[string]any
				_ = json.Unmarshal(recorder.Body.Bytes(), &body)
				return recorder.Code, body
			}
			if code, body := get(); code != http.StatusOK || body["status"] != "healthy" {
				t.Fatalf("/healthz with JetStream answering = %d %v, want 200 healthy", code, body)
			}

			relay.stall(stall.prefixes...)
			code, body := get()
			if code != http.StatusServiceUnavailable || body["status"] != "unhealthy" || body["error"] != stall.want {
				t.Fatalf("/healthz with %s stalled = %d %v, want 503 unhealthy with the error %q - the budget's own deadline, not the JetStream MaxWait", stall.name, code, body, stall.want)
			}
			t.Logf("/healthz with %s stalled: %d with %q", stall.name, code, body["error"])
		})
	}
}
