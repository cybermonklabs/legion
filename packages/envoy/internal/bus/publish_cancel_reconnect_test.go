package bus_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// reconnectWaitFixture is a client that reaches NATS through a proxy the test can cut, a subscriber
// on topic connected to NATS directly, and the envelope to publish there. onPass is what the
// client's reconnect wait runs at each of its passes (bus.WithReconnectWaitPass); nil runs nothing.
//
// The client's clock is a clocktest.Fake the rows never move (WithClock), so a publish's bound
// (bus.PublishBound) never ends a publish, however long the reconnect it waits for takes: each
// publish ends by its caller's cancel, which the bound's context reads at once, or by the
// reconnect. The reconnect wait's recheck (bus.ReconnectRecheck) is armed on the same fake and
// never fires either, so the wait reads the connection again only at a connection event
// (ConnectionChanged).
type reconnectWaitFixture struct {
	proxy    *relayProxy
	client   *bus.Client
	received *nats.Subscription
	envelope contracts.Envelope
	onPass   atomic.Pointer[func()]
	// cut says startPublishWhileReconnecting has cut the link before, and hookedAtCut is how many
	// connection events the client's reconnect hooks had run for when it last did.
	cut         bool
	hookedAtCut uint64
}

func newReconnectWaitFixture(t *testing.T, name string) *reconnectWaitFixture {
	t.Helper()
	uri := testnats.URL(t)
	f := &reconnectWaitFixture{proxy: startRelayProxy(t, uri)}
	client, err := bus.ConnectOwningStream([]string{f.proxy.url()}, bus.WithClock(clocktest.New(fakeStart)), bus.WithReconnectWaitPass(func() {
		if pass := f.onPass.Load(); pass != nil {
			(*pass)()
		}
	}))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	f.client = client
	topic := contracts.RoleTopicPrefix + name
	subscriber := testnats.Connect(t, uri)
	t.Cleanup(subscriber.Close)
	f.received, err = subscriber.SubscribeSync(topic)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := subscriber.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	f.envelope = contracts.Envelope{
		EventID:        "evt-" + name,
		Source:         "agent",
		SourceEventID:  "source-" + name,
		Topic:          topic,
		DedupeKey:      "publish." + name,
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "published for a caller that left during the reconnect",
		TraceID:        "trace-" + name,
	}
	return f
}

// startPublishWhileReconnecting cuts the link so the client is reconnecting, sets onPass as what
// the reconnect wait runs at each pass, and starts the publish under ctx, returning where its
// answer arrives. onPass runs on the publish's goroutine, so it must not fail the test itself.
//
// A cut after an earlier one first waits for the client's reconnect hooks to have run for the
// reconnect that followed it. Those hooks run on nats.go's one callback goroutine and ask the
// server for the stream (ConnectOwningStream), and a cut while that request is in flight loses it:
// the goroutine then waits out the JetStream context's ten-second wait before it reports this cut
// or the reconnect after it, so the reconnect wait, which reads the connection again only at such
// a report, and the pass that waits for it (ConnectionChanged) would wait those ten seconds out.
func (f *reconnectWaitFixture) startPublishWhileReconnecting(t *testing.T, ctx context.Context, onPass func()) <-chan error {
	t.Helper()
	if f.cut {
		testwait.Eventually(t, "the reconnect hooks to have run for the reconnect after the last cut", func() bool {
			return f.client.HookedConnectionEvents() > f.hookedAtCut
		})
	}
	f.cut, f.hookedAtCut = true, f.client.HookedConnectionEvents()
	f.proxy.refuse.Store(true)
	f.proxy.drop()
	testwait.Eventually(t, "the client to start reconnecting", f.client.Conn.IsReconnecting)
	f.onPass.Store(&onPass)
	answer := make(chan error, 1)
	go func() { answer <- f.client.PublishWithin(ctx, f.envelope) }()
	return answer
}

// awaitAnswer returns the publish's answer and clears onPass.
func (f *reconnectWaitFixture) awaitAnswer(t *testing.T, answer <-chan error) error {
	t.Helper()
	defer f.onPass.Store(nil)
	return testwait.Receive(t, "the publish's answer", answer)
}

// requireNothingReceived fails the test if the subscriber received anything from the publisher. A
// marker the publisher's own connection sends after the publish reaches the subscriber behind
// anything that connection sent before it, so the subscriber's first message being the marker
// shows the publish sent nothing.
func (f *reconnectWaitFixture) requireNothingReceived(t *testing.T, what string, publishErr error) {
	t.Helper()
	if err := f.client.Conn.Publish(f.envelope.Topic, []byte("marker")); err != nil {
		t.Fatalf("publish the marker: %v", err)
	}
	message, err := f.received.NextMsgWithContext(testwait.Context(t))
	if err != nil {
		t.Fatalf("the subscriber's first message after %s: %v", what, err)
	}
	if string(message.Data) != "marker" {
		t.Fatalf("the subscriber received %s from %s (publish answered %v)", string(message.Data), what, publishErr)
	}
}

// A core publish whose context is cancelled while it waits for NATS to reconnect, with its
// deadline still ahead, sends nothing even when the reconnect lands right after the cancel. What
// answers it here is ensureConnWithContext's wait, whose select consults the context while it
// waits and so returns the cancellation while the connection is still down; publishConfirmed's
// read of the context immediately before the publish is the backstop, pinned on its own by
// TestAPublishCancelledAsTheReconnectCompletesSendsNothing.
//
// The cancel lands only once the publish is inside the wait: the wait's first pass closes waiting,
// and the test cancels once waiting is closed, then lets the link forward again so the reconnect
// follows.
//
// The control is the same publish under a live context, blocked in the same wait and released by
// the same reconnect: it must be received, so the cancelled case's silence is the cancel's doing
// and not a publish that never reached the wait.
func TestAPublishCancelledWhileWaitingForAReconnectSendsNothing(t *testing.T) {
	f := newReconnectWaitFixture(t, "cancel-while-reconnecting")

	// publishDuringReconnect starts the publish under ctx while the client is reconnecting, calls
	// release once the publish is inside the reconnect wait, then lets the link forward again and
	// returns the publish's answer once the client has reconnected.
	publishDuringReconnect := func(ctx context.Context, release func()) error {
		t.Helper()
		waiting := make(chan struct{})
		var once sync.Once
		answer := f.startPublishWhileReconnecting(t, ctx, func() { once.Do(func() { close(waiting) }) })
		testwait.Eventually(t, "the publish to enter the reconnect wait", func() bool {
			select {
			case <-waiting:
				return true
			case err := <-answer:
				t.Fatalf("the publish answered %v without entering the reconnect wait", err)
			default:
			}
			return false
		})
		release()
		f.proxy.refuse.Store(false)
		err := f.awaitAnswer(t, answer)
		testwait.Eventually(t, "the client to reconnect", f.client.Conn.IsConnected)
		return err
	}

	// The shared wait's context is far later than PublishBound, so PublishBound is the bound
	// PublishCoreContext keeps, on the client's fake clock, which never moves: still ahead when the
	// cancel lands one pass into the wait and when the reconnect follows.
	ctx, cancel := context.WithCancel(testwait.Context(t))
	defer cancel()
	publishErr := publishDuringReconnect(ctx, cancel)
	f.requireNothingReceived(t, "a publish cancelled during the reconnect wait", publishErr)
	if !errors.Is(publishErr, context.Canceled) {
		t.Fatalf("publish cancelled during the reconnect wait = %v, want an error naming %v", publishErr, context.Canceled)
	}

	if err := publishDuringReconnect(testwait.Context(t), func() {}); err != nil {
		t.Fatalf("publish under a live context released by the reconnect: %v", err)
	}
	if _, err := f.received.NextMsgWithContext(testwait.Context(t)); err != nil {
		t.Fatalf("the subscriber received nothing from the live publish released by the reconnect: %v", err)
	}
}

// A core publish whose context is cancelled at the moment its wait for a reconnect ends, after the
// wait's last look at the context and with the connection already back, sends nothing.
// ensureConnWithContext then returns nil, since a usable connection is all it checks once its wait
// has ended; publishConfirmed's read of the context immediately before the publish is the only
// thing that can refuse it. Without that read the message goes out to a caller that had been told
// nothing and had gone.
//
// The wait's first pass is where it happens: the pass lets the link forward again, waits for the
// client to reconnect, and then cancels, so the wait reads a connected connection next and returns.
// The control is the same publish with the same pass and no cancel: it must be received, so the
// cancelled case's silence is the cancel's doing and not a publish that never got past the wait.
func TestAPublishCancelledAsTheReconnectCompletesSendsNothing(t *testing.T) {
	f := newReconnectWaitFixture(t, "cancel-as-reconnected")

	// publishAsTheReconnectCompletes publishes under ctx while the client is reconnecting; the
	// wait's first pass reconnects the client and then calls release. The pass runs on the
	// publish's goroutine and waits for the reconnect through the client's connection events, and
	// closes reconnected once it saw the client reconnected, before release. The publish answers
	// only after its pass has returned, so the test takes the answer first and then reads
	// reconnected: a pass that saw the reconnect has closed it by then. Reading the two at once
	// would let a test that looks late, when both are ready, take the answer for one that came
	// without the pass.
	publishAsTheReconnectCompletes := func(ctx context.Context, release func()) error {
		t.Helper()
		var once sync.Once
		reconnected := make(chan struct{})
		answer := f.startPublishWhileReconnecting(t, ctx, func() {
			once.Do(func() {
				changed := f.client.ConnectionChanged()
				f.proxy.refuse.Store(false)
				for !f.client.Conn.IsConnected() {
					<-changed
					changed = f.client.ConnectionChanged()
				}
				close(reconnected)
				release()
			})
		})
		err := f.awaitAnswer(t, answer)
		select {
		case <-reconnected:
		default:
			t.Fatalf("the publish answered %v without its wait's first pass seeing the client reconnect", err)
		}
		return err
	}

	// The shared wait's context is far later than PublishBound, so PublishBound is the bound
	// PublishCoreContext keeps, on the client's fake clock, which never moves: still ahead when the
	// reconnect completes inside the wait.
	ctx, cancel := context.WithCancel(testwait.Context(t))
	defer cancel()
	publishErr := publishAsTheReconnectCompletes(ctx, cancel)
	f.requireNothingReceived(t, "a publish cancelled as the reconnect completed", publishErr)
	if !errors.Is(publishErr, context.Canceled) {
		t.Fatalf("publish cancelled as the reconnect completed = %v, want an error naming %v", publishErr, context.Canceled)
	}

	if err := publishAsTheReconnectCompletes(testwait.Context(t), func() {}); err != nil {
		t.Fatalf("publish under a live context as the reconnect completed: %v", err)
	}
	if _, err := f.received.NextMsgWithContext(testwait.Context(t)); err != nil {
		t.Fatalf("the subscriber received nothing from the live publish as the reconnect completed: %v", err)
	}
}
