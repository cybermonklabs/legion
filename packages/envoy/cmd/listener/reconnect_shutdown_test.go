package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// listenerRelay lets this test hold one NATS request from the listener, then drop the live link so
// nats.go reconnects through the same relay. New connections forward normally, unless cut has
// severed the relay: from then on every connection it accepts is refused, which is how a bus that
// has gone away under the listener looks.
type listenerRelay struct {
	listener net.Listener
	target   string
	relayed  string
	ctx      context.Context
	// severed is read by relay before it dials the server, so a connection accepted after cut is
	// closed rather than relayed; it is atomic because relay runs on its own goroutine per
	// connection.
	severed atomic.Bool

	mu        sync.Mutex
	trigger   []byte
	holdAfter int
	// hook is what pause armed: it runs once, on the relay's own goroutine, before the bytes
	// completing hookAfter matches of hookTrigger reach the server, and is cleared as it runs.
	hook        func()
	hookTrigger []byte
	hookAfter   int
	links       []*listenerRelayLink

	swallowed chan struct{}
}

type listenerRelayLink struct {
	client, server net.Conn
	swallow        atomic.Bool
	seen           []byte
}

func startListenerRelay(t *testing.T, uri string) *listenerRelay {
	t.Helper()
	server, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse NATS URL %q: %v", uri, err)
	}
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen for relay: %v", err)
	}
	relayed := *server
	relayed.Host = listener.Addr().String()
	relay := &listenerRelay{
		listener:  listener,
		target:    server.Host,
		relayed:   relayed.String(),
		ctx:       testwait.Context(t),
		swallowed: make(chan struct{}, 1),
	}
	t.Cleanup(relay.close)
	go relay.serve()
	return relay
}

func (r *listenerRelay) serve() {
	for {
		client, err := r.listener.Accept()
		if err != nil {
			return
		}
		go r.relay(client)
	}
}

func (r *listenerRelay) relay(client net.Conn) {
	r.mu.Lock()
	if r.severed.Load() {
		r.mu.Unlock()
		_ = client.Close()
		return
	}
	link := &listenerRelayLink{client: client}
	r.links = append(r.links, link)
	r.mu.Unlock()
	server, err := (&net.Dialer{}).DialContext(r.ctx, "tcp", r.target)
	if err != nil {
		_ = client.Close()
		return
	}
	r.mu.Lock()
	link.server = server
	r.mu.Unlock()
	go func() {
		_, _ = io.Copy(client, server)
		_ = client.Close()
	}()
	defer func() { _ = server.Close() }()

	buffer := make([]byte, 32*1024)
	for {
		n, err := client.Read(buffer)
		if n > 0 && !link.swallow.Load() {
			if r.shouldSwallow(link, buffer[:n]) {
				link.swallow.Store(true)
				select {
				case r.swallowed <- struct{}{}:
				default:
				}
			} else if _, writeErr := server.Write(buffer[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *listenerRelay) shouldSwallow(link *listenerRelayLink, data []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	link.seen = append(link.seen, data...)
	if r.hook != nil && bytes.Count(link.seen, r.hookTrigger) >= r.hookAfter {
		hook := r.hook
		r.hook = nil
		hook()
	}
	return r.holdAfter > 0 && bytes.Count(link.seen, r.trigger) >= r.holdAfter
}

func (r *listenerRelay) arm(trigger string, occurrence int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trigger = []byte(trigger)
	r.holdAfter = occurrence
	for _, link := range r.links {
		link.seen = nil
	}
}

// pause holds the protocol line completing the occurrence'th match of trigger, runs hook, and
// then forwards that line. It is how a test lands a write of its own in the middle of a request
// the listener is making: the write a hook makes is already in the bucket when the held call
// reaches the server, with nothing timed and nothing slept. The hook runs on the relay's
// goroutine with this relay's lock held, so it must reach the server over a connection of its
// own and call nothing on the relay. Like arm, it starts the count from here, and it runs once.
func (r *listenerRelay) pause(trigger string, occurrence int, hook func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hookTrigger = []byte(trigger)
	r.hookAfter = occurrence
	r.hook = hook
	for _, link := range r.links {
		link.seen = nil
	}
}

func (r *listenerRelay) drop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, link := range r.links {
		_ = link.client.Close()
		if link.server != nil {
			_ = link.server.Close()
		}
	}
	r.links = nil
}

// cut drops every live link as drop does and refuses every connection accepted after it, so the
// client's reconnect (MaxReconnect -1, internal/bus's options) never completes and the bus stays
// unreachable for the rest of the test. drop alone leaves the relay forwarding, which is what a
// reconnect through the same relay needs.
func (r *listenerRelay) cut() {
	r.severed.Store(true)
	r.drop()
}

func (r *listenerRelay) close() {
	_ = r.listener.Close()
	r.cut()
}

// During a deploy, a reconnect can interrupt the replacement's durable bind. The reconnect must
// leave that bind to finish, keep the role lane forwarding, then make SIGTERM an ordered shutdown.
func TestListenerReconnectWithBindInFlightShutsDownAfterForwardingRoles(t *testing.T) {
	t.Parallel()
	const (
		machineID = "reconnect-bind-sigterm"
		holderID  = "ses_reconnect_bind_sigterm_holder"
		role      = "reconnect-bind-sigterm-role"
		messages  = 2
	)
	uri := testnats.URL(t)
	relay := startListenerRelay(t, uri)
	old, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the old task: %v", err)
	}
	t.Cleanup(old.Close)
	consumer := "listener-" + machineID
	durable, err := startListenerSubscription(old, consumer, func(*natsgo.Msg) {})
	if err != nil {
		t.Fatalf("bind the durable as the old task: %v", err)
	}
	ctx := testwait.Context(t)
	testwait.Until(t, "the old task's durable to become push-bound", func() (string, bool) {
		info, err := old.JS().ConsumerInfo(bus.Stream, consumer, natsgo.Context(ctx))
		if err != nil {
			return err.Error(), false
		}
		return "consumer is not push-bound", info.PushBound
	})
	listener := startListenerProcess(t, jetStreamWaitListenerBinary(t), relay.relayed, machineID)
	listener.WaitForOutput(t, "subscribe failed, retrying")

	holder, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect holder: %v", err)
	}
	t.Cleanup(holder.Close)
	for _, route := range []struct{ path, body string }{
		{"/v1/interests/subscribe", consumerRegistration(holderID, "epoch-1")},
		{"/v1/roles/set", `{"session_id":"` + holderID + `","role":"` + role + `"}`},
	} {
		if status, answer := callListenerWithinTest(t, ctx, listener.port, http.MethodPost, route.path, route.body); status != http.StatusOK {
			t.Fatalf("%s: status %d: %s", route.path, status, answer)
		}
	}
	frames := make(chan *natsgo.Msg, 256)
	subscription, err := holder.Conn.ChanSubscribe(contracts.LegacyAgentSubject(holderID), frames)
	if err != nil {
		t.Fatalf("subscribe as %s: %v", holderID, err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	flushCtx, release := clock.WithShownDeadline(ctx)
	defer release()
	if err := holder.Conn.FlushWithContext(flushCtx); err != nil {
		t.Fatalf("flush the subscription as %s: %v", holderID, err)
	}

	relay.arm("$JS.API.CONSUMER.INFO.", 2)
	testwait.Receive(t, "the listener's durable bind to reach the controlled relay", relay.swallowed)
	relay.drop()
	// The role lane holds a connection of its own (bus.ConnectRoleLane), which reconnects, and sends
	// its subscription again, beside the main client's.
	listener.WaitForOutput(t, "envoy nats reconnected name=envoy ", "envoy nats reconnected name=envoy-role-lane ")
	const leftToBind = "envoy nats resubscribe left to the bind in flight"
	output := testwait.Until(t, "the restore to report the bind in flight", func() (string, bool) {
		output := listener.Output.String()
		return output, strings.Contains(output, leftToBind) ||
			strings.Contains(output, "envoy nats resubscribe failed") ||
			strings.Contains(output, "envoy nats resubscribing name=envoy ")
	})
	if !strings.Contains(output, " INFO "+leftToBind+" name=envoy ") {
		t.Fatalf("the restore did not report the bind in flight at INFO:\n%s", output)
	}
	if line := firstErrorLine(output); line != "" {
		t.Fatalf("the reconnect with a bind in flight logged ERROR: %s", line)
	}
	if strings.Contains(output, "durable bound") {
		t.Fatalf("the durable bound before the role messages proved the reconnect window:\n%s", output)
	}

	publisher := testnats.Connect(t, uri)
	defer publisher.Close()
	for index := range messages {
		id := strconv.Itoa(index)
		item := contracts.Envelope{
			EventID:        "evt-reconnect-bind-sigterm-" + id,
			Source:         "agent",
			SourceEventID:  "source-reconnect-bind-sigterm",
			Topic:          contracts.RoleTopicPrefix + role,
			DedupeKey:      "reconnect-bind-sigterm-" + id,
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: "role message during reconnect",
			TraceID:        "trace-reconnect-bind-sigterm",
		}
		data, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("marshal role message %d: %v", index, err)
		}
		if err := publisher.Publish(item.Topic, data); err != nil {
			t.Fatalf("publish role message %d: %v", index, err)
		}
	}
	if err := publisher.FlushWithContext(flushCtx); err != nil {
		t.Fatalf("flush role messages: %v", err)
	}
	for received := range messages {
		msg := testwait.Receive(t, fmt.Sprintf("the listener to forward role message %d of %d during the reconnect", received+1, messages), frames)
		var frame contracts.Envelope
		if err := json.Unmarshal(msg.Data, &frame); err != nil {
			t.Fatalf("decode role message %d: %v", received, err)
		}
		if !strings.HasPrefix(frame.DedupeKey, roleForwardDedupePrefix) {
			t.Fatalf("role message %d forwarded with dedupe key %q, want %s prefix", received, frame.DedupeKey, roleForwardDedupePrefix)
		}
	}

	listener.Terminate(t)
	listener.WaitExit(t, "SIGTERM during the bind in flight")
	output = listener.Output.String()
	for _, line := range []string{"received signal, shutting down", "envoy-listener shutdown complete"} {
		if !strings.Contains(output, line) {
			t.Fatalf("the listener did not log %q:\n%s", line, output)
		}
	}
	if line := firstErrorLine(output); line != "" {
		t.Fatalf("the ordered SIGTERM shutdown logged ERROR: %s", line)
	}
	if !durable.IsValid() {
		t.Fatal("the old task's durable subscription was lost when the replacement stopped")
	}
}

// callListenerWithinTest sends a request with the shared wait's context so a listener that
// stops answering cannot hold the test past the point where its other waits give up.
func callListenerWithinTest(t *testing.T, ctx context.Context, port int, method, path, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:"+strconv.Itoa(port)+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	request.Header.Set("Authorization", "Bearer "+listenerTestToken)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	return do(t, request)
}

func firstErrorLine(output string) string {
	for line := range strings.SplitSeq(output, "\n") {
		if errorLine.MatchString(line) {
			return line
		}
	}
	return ""
}
