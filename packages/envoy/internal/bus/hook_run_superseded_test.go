package bus

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// supersede is what a reconnect hook does to lose the connection its run is on: it stops srv, and
// returns once conn, the run's connection, has left CONNECTED and ctx, the run's context, has ended
// with it, with the context's error. Either that is not done within 10 s returns an error of its
// own, which no run reads as superseded.
func supersede(ctx context.Context, t *testing.T, srv *testnats.Server, conn *nats.Conn) error {
	srv.Stop(t)
	return awaitRunLost(ctx, conn)
}

// awaitRunLost waits, within 10 s each, for conn to leave CONNECTED and then for ctx, the context
// of a run on conn, to end, and returns ctx's error; a wait that runs out returns an error of its
// own.
func awaitRunLost(ctx context.Context, conn *nats.Conn) error {
	if !testwait.Holds(10*time.Second, 10*time.Millisecond, func() bool { return !conn.IsConnected() }) {
		return errors.New("the run's connection still read CONNECTED 10s after it was cut")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
		return errors.New("the run's context had not ended 10s after its connection left CONNECTED")
	}
}

// markHooksDue counts a connection event, as onReconnect does, so the next rewatch runs the hooks.
func markHooksDue(c *Client) {
	c.mu.Lock()
	c.markHooksDueLocked()
	c.mu.Unlock()
}

// A run of the reconnect hooks whose connection is lost while it runs is superseded, not failed:
// rewatch returns errRunSuperseded and keeps no error for HookStall, which /healthz reports, and the
// run afterReconnect makes after a reconnect logs it at INFO, with neither the WARN of a failed run
// nor the ERROR of a stall. The first run is the test's own (rewatch), the second the reconnect's
// (afterReconnect), and both lose their connection to a stopped server; the third covers the
// reconnect that follows.
func TestASupersededRunKeepsNoErrorAndLogsNoWarning(t *testing.T) {
	srv, uri := testnats.Start(t)
	client, err := Connect([]string{uri}, WithName(t.Name()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	logs := captureLogs(t)
	var calls atomic.Int32
	client.AddReconnectHook(func(ctx context.Context, conn *nats.Conn) error {
		if calls.Add(1) <= 2 {
			return supersede(ctx, t, srv, conn)
		}
		return nil
	})

	markHooksDue(client)
	err = client.rewatch()
	if !errors.Is(err, errRunSuperseded) {
		t.Fatalf("a run whose connection was lost under it returned %v, want errRunSuperseded", err)
	}
	if _, kept := client.HookStall(); kept != nil {
		t.Fatalf("a superseded run kept its error for HookStall: %v", kept)
	}
	if !client.hooksDue() {
		t.Fatal("a superseded run covered its connection event")
	}

	srv.Start(t)
	// The reconnect's run ends in one of two lines, and the wait takes whichever comes: the INFO of
	// a superseded run, or the report of a failed one, which this test refuses.
	const superseded = "envoy nats reconnect hooks superseded by a newer connection event"
	failures := func() string {
		return logs.Lines("reconnect hook failed") + logs.Lines("reconnect hooks have not succeeded")
	}
	testwait.WithinPolling(t, 30*time.Second, 20*time.Millisecond, "the end of the reconnect's own run", func() bool {
		return calls.Load() >= 2 && (logs.Lines(superseded) != "" || failures() != "")
	})
	if lines := failures(); lines != "" {
		t.Fatalf("the reconnect's superseded run was reported as a failure:\n%s", lines)
	}
	if _, kept := client.HookStall(); kept != nil {
		t.Fatalf("the reconnect's superseded run kept its error for HookStall: %v", kept)
	}
	srv.Start(t)
	testwait.WithinPolling(t, 30*time.Second, 20*time.Millisecond, "a run that covers the last reconnect", func() bool {
		return calls.Load() >= 3 && !client.hooksDue()
	})
	if lines := failures(); lines != "" {
		t.Fatalf("a superseded run was reported as a failure:\n%s", lines)
	}
	if _, kept := client.HookStall(); kept != nil {
		t.Fatalf("the run that covered the reconnect left an error for HookStall: %v", kept)
	}
}

// hookCall is one call of a reconnect hook: whether the run's context was live when it began, and
// how many times its connection had reconnected.
type hookCall struct {
	live       bool
	reconnects uint64
}

// A superseded run covers nothing, whatever its hooks returned, so every hook runs again on the
// connection that comes back: a hook that ran before the loss may have moved its state under a
// context that then ended, and one that ran after it ran on a dead connection. That holds for a
// hook that returns the cancel, which ends the run there, and for one that swallows it and returns
// nil, after which the hooks behind it run on the ended context; the run is superseded either way.
func TestASupersededRunLeavesEveryHookDue(t *testing.T) {
	for _, tc := range []struct {
		name    string
		swallow bool
	}{
		{name: "the hook returns the cancel"},
		{name: "the hook swallows the cancel", swallow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, uri := testnats.Start(t)
			client, err := Connect([]string{uri}, WithName(t.Name()))
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)
			var firsts atomic.Int32
			client.AddReconnectHook(func(ctx context.Context, conn *nats.Conn) error {
				if firsts.Add(1) > 1 {
					return nil
				}
				err := supersede(ctx, t, srv, conn)
				if tc.swallow && errors.Is(err, context.Canceled) {
					return nil
				}
				return err
			})
			var mu sync.Mutex
			var seconds []hookCall
			client.AddReconnectHook(func(ctx context.Context, conn *nats.Conn) error {
				mu.Lock()
				seconds = append(seconds, hookCall{live: ctx.Err() == nil, reconnects: conn.Stats().Reconnects})
				mu.Unlock()
				return nil
			})

			markHooksDue(client)
			err = client.rewatch()
			if !client.hooksDue() {
				t.Fatalf("a run its connection's loss superseded covered its connection event (rewatch returned %v); want every hook due", err)
			}
			if !errors.Is(err, errRunSuperseded) {
				t.Fatalf("a run whose connection was lost under it returned %v, want errRunSuperseded", err)
			}

			srv.Start(t)
			testwait.WithinPolling(t, 30*time.Second, 20*time.Millisecond, "the second hook to run on the reconnected connection and the run to cover it", func() bool {
				mu.Lock()
				defer mu.Unlock()
				for _, call := range seconds {
					if call.live && call.reconnects >= 1 {
						return !client.hooksDue()
					}
				}
				return false
			})
		})
	}
}

// halfOpenProxy forwards each connection it accepts to upstream once admit has run, and closes each
// one before then, as a refused dial would be. cutClients ends the client's side of every connection
// open at the time and keeps its server's side open, reading and dropping what the server sends:
// the client sees its connection lost, while the server still holds that connection and every
// subscription on it, as it does for a client whose network went away until its own pings find it
// gone (minutes, at nats-server's defaults).
type halfOpenProxy struct {
	ln       net.Listener
	upstream string

	mu       sync.Mutex
	admitted bool
	open     map[*proxiedConn]struct{}
}

// proxiedConn is one connection halfOpenProxy forwards: the client's side and the server's.
type proxiedConn struct {
	client, server net.Conn
	cut            atomic.Bool
}

func newHalfOpenProxy(t *testing.T, upstream string) *halfOpenProxy {
	t.Helper()
	ln, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &halfOpenProxy{ln: ln, upstream: upstream, open: map[*proxiedConn]struct{}{}}
	t.Cleanup(func() {
		_ = ln.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for conn := range p.open {
			_ = conn.client.Close()
			_ = conn.server.Close()
		}
	})
	go p.accept()
	return p
}

func (p *halfOpenProxy) addr() string { return p.ln.Addr().String() }

// admit makes the proxy forward the connections it accepts from now on.
func (p *halfOpenProxy) admit() {
	p.mu.Lock()
	p.admitted = true
	p.mu.Unlock()
}

// cutClients closes the client's side of every connection open now, and keeps the server's side.
func (p *halfOpenProxy) cutClients() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for conn := range p.open {
		if conn.cut.CompareAndSwap(false, true) {
			_ = conn.client.Close()
		}
	}
}

func (p *halfOpenProxy) accept() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		admitted := p.admitted
		p.mu.Unlock()
		if !admitted {
			_ = client.Close()
			continue
		}
		server, err := net.Dial("tcp", p.upstream)
		if err != nil {
			_ = client.Close()
			continue
		}
		conn := &proxiedConn{client: client, server: server}
		p.mu.Lock()
		p.open[conn] = struct{}{}
		p.mu.Unlock()
		go p.forward(conn)
	}
}

// forward copies conn's two sides into each other. A side that ends closes both, unless the end is
// a cut's: then the server's side stays open, and what the server sends on it is read and dropped,
// so the server never finds the connection stalled; the test's cleanup closes it.
func (p *halfOpenProxy) forward(conn *proxiedConn) {
	end := func() {
		_ = conn.client.Close()
		_ = conn.server.Close()
		p.mu.Lock()
		delete(p.open, conn)
		p.mu.Unlock()
	}
	go func() {
		_, _ = io.Copy(conn.client, conn.server)
		if conn.cut.Load() {
			_, _ = io.Copy(io.Discard, conn.server)
		}
		end()
	}()
	_, _ = io.Copy(conn.server, conn.client)
	if !conn.cut.Load() {
		end()
	}
}

// The restore of a reconnect's subscriptions is never under the context of the reconnect's run of
// the hooks (whileConnected). A bind the restore made stays the registration's when the connection
// it was made on is lost while the hooks run: nats.go keeps the subscription and re-sends it on
// the reconnect, the server holds the durable bound to it meanwhile, and nothing binds it again. A
// bind abandoned at that loss would leave the registration for the next restore, whose bind the
// server then refuses with "consumer is already bound to a subscription" while it still holds the
// connection that was lost.
//
// The client connects in the background, so its first connection's restore binds the durable
// Subscribe registered before it. That run's hook waits for the server to apply the bind, then
// cuts the connection on the client's side alone (halfOpenProxy): the server keeps it, with the
// durable's push binding, for minutes. nats.go reconnects in place, and the durable ends bound and
// handed what the stream takes.
func TestTheRestoresBindOutlivesTheRunItsConnectionLossSuperseded(t *testing.T) {
	_, uri := testnats.Start(t)
	owner, err := ConnectOwningStream([]string{uri}, WithName(t.Name()+"-owner"))
	if err != nil {
		t.Fatalf("create the stream: %v", err)
	}
	t.Cleanup(owner.Close)
	const consumer = "restore-across-a-flap"
	if _, err := owner.JS().AddConsumer(Stream, &nats.ConsumerConfig{
		Durable:        consumer,
		DeliverSubject: nats.NewInbox(),
		AckPolicy:      nats.AckExplicitPolicy,
		FilterSubjects: StreamSubjects(),
	}); err != nil {
		t.Fatalf("add the durable: %v", err)
	}
	pushBound := func() bool {
		info, err := owner.JS().ConsumerInfo(Stream, consumer)
		return err == nil && info.PushBound
	}

	proxy := newHalfOpenProxy(t, strings.TrimPrefix(uri, "nats://"))
	client, err := Connect([]string{"nats://" + proxy.addr()}, WithConnectInBackground(), WithName(t.Name()))
	if err != nil {
		t.Fatalf("connect in the background: %v", err)
	}
	t.Cleanup(client.Close)
	var delivered atomic.Int32
	if _, err := client.Subscribe("", func(msg *nats.Msg) {
		delivered.Add(1)
		_ = msg.Ack()
	}, nats.Bind(Stream, consumer), nats.ManualAck()); err == nil {
		t.Fatal("the durable was bound before the client's first connection; the test needs that connection's restore to bind it")
	}
	logs := captureLogs(t)
	var calls atomic.Int32
	cut := make(chan error, 1)
	client.AddReconnectHook(func(ctx context.Context, conn *nats.Conn) error {
		if calls.Add(1) > 1 {
			return nil
		}
		// nats-server applies a push subscriber's interest on the consumer's own goroutine, so only
		// the consumer's report says the restore's bind has reached it.
		if !testwait.Holds(10*time.Second, 20*time.Millisecond, pushBound) {
			err := errors.New("the server did not report the restore's bind within 10s")
			cut <- err
			return err
		}
		proxy.cutClients()
		err := awaitRunLost(ctx, conn)
		cut <- err
		return err
	})

	proxy.admit()
	select {
	case err := <-cut:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the first connection's run was not superseded by the cut: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the first connection's run of the hooks did not reach the cut within 30s")
	}
	if !testwait.Holds(30*time.Second, 20*time.Millisecond, func() bool {
		return calls.Load() >= 2 && !client.hooksDue() && client.SubOK()
	}) {
		t.Fatalf("30s after the cut: hook calls %d, hooks due %t, durable bound %t, want the reconnect's run done with the durable bound; already-bound lines:\n%s",
			calls.Load(), client.hooksDue(), client.SubOK(), logs.Lines("already bound"))
	}
	if !pushBound() {
		t.Fatal("the server does not report the durable bound")
	}
	if _, err := owner.JS().Publish("notifications.github.test.restore-across-a-flap", []byte(`{}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !testwait.Holds(10*time.Second, 20*time.Millisecond, func() bool { return delivered.Load() >= 1 }) {
		t.Fatalf("the durable bound across the cut was handed nothing within 10s; already-bound lines:\n%s", logs.Lines("already bound"))
	}
	if lines := logs.Lines("already bound"); lines != "" {
		t.Fatalf("a bind of the durable was refused as already bound:\n%s", lines)
	}
}
