package bus

// Rows for how a request's life ends at a drop (requestLife) and for a close answer read during a
// held handshake (closedUnder).

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// withLifeListenGate makes a client whose lives' goroutines run gate before they register their
// status listener ("register") and before their first wait on it once registered ("select")
// (Client.lifeListenGate).
func withLifeListenGate(gate func(stage string, life context.Context)) ConnectOption {
	return clientOption(func(o *connectOpts) { o.lifeListenGate = gate })
}

// stageHold holds the first life listener goroutine that reaches stage, of a life not yet ended,
// until release. One made with newUnarmedStageHold holds nothing until arm: a row whose client began
// a life at its connect (proxiedClient) arms it after it ends that life (endRunningLife), so the
// goroutine it holds is the one of the life the row's own request begins, whether or not the ended
// life's goroutine has reached the stage yet.
type stageHold struct {
	stage       string
	armed       atomic.Bool
	once        sync.Once
	held        chan struct{}
	released    chan struct{}
	releaseOnce sync.Once
}

func newStageHold(stage string) *stageHold {
	h := newUnarmedStageHold(stage)
	h.arm()
	return h
}

func newUnarmedStageHold(stage string) *stageHold {
	return &stageHold{stage: stage, held: make(chan struct{}), released: make(chan struct{})}
}

func (h *stageHold) arm() { h.armed.Store(true) }

func (h *stageHold) gate(stage string, life context.Context) {
	if stage != h.stage || !h.armed.Load() || life.Err() != nil {
		return
	}
	first := false
	h.once.Do(func() { first = true; close(h.held) })
	if first {
		<-h.released
	}
}

func (h *stageHold) release() { h.releaseOnce.Do(func() { close(h.released) }) }

// inFlight starts a request on client whose send waits for its bound context to end, and returns
// the channel its answer arrives on once send has been called.
func inFlight(t *testing.T, ctx context.Context, client *Client) <-chan error {
	t.Helper()
	started := make(chan struct{})
	answered := make(chan error, 1)
	go func() {
		_, err := Request(ctx, client, func(bound context.Context) (struct{}, error) {
			close(started)
			<-bound.Done()
			return struct{}{}, context.Cause(bound)
		})
		answered <- err
	}()
	select {
	case <-started:
	case err := <-answered:
		t.Fatalf("the request answered %v before it was sent", err)
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the request was never sent")
	}
	return answered
}

func requireLostAtDrop(t *testing.T, answered <-chan error, what string) {
	t.Helper()
	select {
	case err := <-answered:
		if !errors.Is(err, ErrConnectionLost) {
			t.Fatalf("%s: the request in flight answered %v, want ErrConnectionLost", what, err)
		}
	case <-time.After(testnats.Remaining(t)):
		t.Fatalf("%s: the request in flight gave no answer by the test's deadline: its life outlived the drop", what)
	}
}

// heldClientKinds are the two kinds of client a caller can hold a lock across: the session
// consumers' (withLossAtOnce), every request of which is made under a lock, and the listener's main
// client, which a caller holding a session's lock asks first (LostNow) and then calls under a
// context it marks (WithLockHeld). held is the context such a caller makes its requests under.
var heldClientKinds = []struct {
	name    string
	options []ConnectOption
	held    func(context.Context) context.Context
}{
	{"session consumers' client", []ConnectOption{withLossAtOnce()}, func(ctx context.Context) context.Context { return ctx }},
	{"main client", nil, WithLockHeld},
}

// awaitBeforeHoldEnds waits for answered while hold still holds the handshake: a row that passes
// reads the answer before the row releases the hold, and one that fails ends at the test's
// deadline. It decides by that order alone.
func awaitBeforeHoldEnds[T any](t *testing.T, hold *testnats.HandshakeHold, answered <-chan T, what string) T {
	t.Helper()
	select {
	case got := <-answered:
		select {
		case <-hold.Ended():
			t.Fatalf("%s came only once the held handshake had ended", what)
		default:
		}
		return got
	case <-hold.Ended():
		t.Fatalf("%s did not come while the handshake was held", what)
	case <-time.After(testnats.Remaining(t)):
		hold.Release()
		t.Fatalf("%s did not come by the test's deadline while the handshake was held: it waited for nats.go's connection lock, which the handshake holds", what)
	}
	var zero T
	return zero
}

// A close answer read while nats.go is in a reconnect's handshake answers within the probe's
// budget, on either kind of client a caller can hold a lock across: closedUnder reads the
// connection's status by the probe (requestRead), and a caller that holds a lock reads within its
// budget. A forced reconnect (the token keeper's) answers the pending request ErrConnectionClosed
// and starts the reconnect at once, and the hub holding the CONNECT holds nats.go's lock. The row
// holds the life's end (withLifeEndGate), so finish reaches closedUnder, as it does whenever the
// request's answer is read before the life has ended. It decides by order: the answer must come
// while the handshake is still held.
//
// The control is closedUnder reading nats.go's own Status and Stats: no answer comes while the
// handshake is held, and each case fails at the test's deadline.
func TestACloseAnswerDuringAHeldHandshakeAnswersAtOnce(t *testing.T) {
	for _, kind := range heldClientKinds {
		t.Run(kind.name, func(t *testing.T) {
			gate := newLifeEndHold()
			client, hold := proxiedClient(t, append([]ConnectOption{withLifeEndGate(gate.gate)}, kind.options...)...)
			t.Cleanup(gate.release)
			ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
			defer cancel()
			_, finish := client.bindRequest(kind.held(ctx))
			conn := client.Connection()
			held := hold.HoldNext()
			if err := conn.ForceReconnect(); err != nil {
				t.Fatalf("force a reconnect: %v", err)
			}
			awaitHeld(t, held)
			select {
			case <-gate.held:
			case <-time.After(testnats.Remaining(t)):
				t.Fatal("the forced reconnect never reached the end of the request's life")
			}
			answered := make(chan error, 1)
			go func() { answered <- finish(natsgoClosedAnswer) }()
			if err := awaitBeforeHoldEnds(t, hold, answered, "the close answer"); !errors.Is(err, ErrConnectionLost) {
				t.Fatalf("the close answer read during the held handshake answered %v, want ErrConnectionLost", err)
			}
		})
	}
}

// A request's read of its connection made while nats.go is in a reconnect's handshake, by a caller
// that holds a lock across the request, answers within the probe's budget, on either kind of client:
// requestLife reads the connection by the probe (requestRead), which answers a lock held past
// ConnLockWait "unknown", and the request is not sent. A caller holding a session's lock across
// the main client's calls asks LostNow first; a reconnect that starts between that answer and the
// request's own read holds the read here, not the lock. It decides by order: requestLife must
// return while the handshake is still held.
//
// The control is requestLife reading nats.go's own Status and Stats: it gives no answer while the
// handshake is held, and each case fails at the test's deadline.
func TestARequestsReadDuringAHeldHandshakeAnswersAtOnce(t *testing.T) {
	for _, kind := range heldClientKinds {
		t.Run(kind.name, func(t *testing.T) {
			client, hold := proxiedClient(t, kind.options...)
			awaitHeld(t, hold.Hold())
			answered := make(chan context.Context, 1)
			go func() {
				life, _, _ := client.requestLife(kind.held(t.Context()))
				answered <- life
			}()
			if life := awaitBeforeHoldEnds(t, hold, answered, "the request's read of its connection"); life.Err() == nil {
				t.Fatal("a request made during the held handshake was bound to a running life, want the ended one")
			}
		})
	}
}

// A life whose goroutine has not yet run - not scheduled, on a loaded box - when the connection
// drops and a reconnect's attempt takes nats.go's lock ends at the next request's read of the
// connection, by a caller that holds a lock, while the attempt still holds nats.go's: requestLife,
// reading "unknown" for a life whose goroutine has no listener yet, ends the life running on it. A
// request bound to that life would otherwise wait meanwhile, with its caller's lock held. The row
// gates the goroutine at "register" through the whole held handshake, so only the next request's
// read can end the life. It decides by order: the request in flight must answer while the handshake
// is still held.
//
// The control is requestLife's down branch leaving the running life as it is: the request in
// flight gives no answer while the handshake is held, and each case fails at the test's deadline.
func TestALifeWhoseGoroutineIsHeldEndsAtTheNextRequestDuringAHeldReconnect(t *testing.T) {
	for _, kind := range heldClientKinds {
		t.Run(kind.name, func(t *testing.T) {
			stage := newUnarmedStageHold("register")
			client, hold := proxiedClient(t, append([]ConnectOption{withLifeListenGate(stage.gate)}, kind.options...)...)
			t.Cleanup(stage.release)
			endRunningLife(client)
			stage.arm()
			ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
			defer cancel()
			inflight := inFlight(t, ctx, client)
			select {
			case <-stage.held:
			case <-time.After(testnats.Remaining(t)):
				t.Fatal("the life's listener goroutine never started")
			}
			awaitHeld(t, hold.Hold())
			go client.requestLife(kind.held(ctx))
			if err := awaitBeforeHoldEnds(t, hold, inflight, "the request in flight's answer"); !errors.Is(err, ErrConnectionLost) {
				t.Fatalf("the request in flight answered %v, want ErrConnectionLost", err)
			}
		})
	}
}

// A life whose goroutine registers its status listener only once the connection has dropped and a
// reconnect's attempt holds nats.go's lock ends at the drop, while the attempt still holds that
// lock, with no other request made: the listener is registered on a goroutine of its own
// (requestLife), since StatusChanged waits for nats.go's lock, and the life's goroutine reads the
// connection meanwhile, "unknown" after the drop's connection event, which ends the life. The row
// gates the goroutine at "register" until the attempt holds the lock and the client has reported
// the drop (ConnectionChanged), then releases it. It decides by order: the request in flight must
// answer while the handshake is still held.
//
// The control is the listener registered on the life's own goroutine: that goroutine waits for
// nats.go's lock before it reads, the request in flight gives no answer while the handshake is
// held, and each case fails at the test's deadline.
func TestALifeWhoseListenerRegistersDuringAHeldReconnectEndsAtTheDrop(t *testing.T) {
	for _, kind := range heldClientKinds {
		t.Run(kind.name, func(t *testing.T) {
			stage := newUnarmedStageHold("register")
			client, hold := proxiedClient(t, append([]ConnectOption{withLifeListenGate(stage.gate)}, kind.options...)...)
			t.Cleanup(stage.release)
			endRunningLife(client)
			stage.arm()
			ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
			defer cancel()
			inflight := inFlight(t, ctx, client)
			select {
			case <-stage.held:
			case <-time.After(testnats.Remaining(t)):
				t.Fatal("the life's listener goroutine never started")
			}
			dropped := client.ConnectionChanged()
			awaitHeld(t, hold.Hold())
			select {
			case <-dropped:
			case <-time.After(testnats.Remaining(t)):
				t.Fatal("the client never reported the drop by the test's deadline")
			}
			stage.release()
			if err := awaitBeforeHoldEnds(t, hold, inflight, "the request in flight's answer"); !errors.Is(err, ErrConnectionLost) {
				t.Fatalf("the request in flight answered %v, want ErrConnectionLost", err)
			}
		})
	}
}

// A drop and reconnect that land between the life's begin and its goroutine's registration of its
// status listener end the life: the goroutine wakes at the client's connection events and reads a
// reconnect count other than the one the life began at. A request sent on the dropped socket, whose
// reply the drop took (nats.go fails no pending request at a drop), would otherwise wait out its
// whole deadline. The gate holds the goroutine before it registers, as a goroutine not yet
// scheduled is held on a loaded box.
//
// The control is the goroutine without its reconnect-count check: the request gives no answer
// after the listener registers.
func TestALifeEndsAtADropAndReconnectBeforeItsListenerRegisters(t *testing.T) {
	hold := newStageHold("register")
	client, proxy := proxiedClient(t, withLifeListenGate(hold.gate))
	t.Cleanup(hold.release)
	conn := client.Connection()
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	before := conn.Stats().Reconnects
	answered := inFlight(t, ctx, client)
	select {
	case <-hold.held:
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the life's listener goroutine never started")
	}
	proxy.Cut()
	waitForNatsStatus(t, conn, "reconnecting after the cut", func(s nats.Status) bool { return s != nats.CONNECTED })
	proxy.Restore()
	waitForNatsStatus(t, conn, "connected again", func(s nats.Status) bool { return s == nats.CONNECTED })
	if after := conn.Stats().Reconnects; after <= before {
		t.Fatalf("nats.go's reconnect count is %d, want above %d", after, before)
	}
	hold.release()
	requireLostAtDrop(t, answered, "a drop and reconnect before the listener registered")
}

// A life ends at a drop whose report nats.go took back: sendStatusEvent, sending a second report of
// a status the listener is registered for, first receives from the channel when it holds a value
// and deletes the listener (nats.go:6684-6690). A drop (RECONNECTING) followed by a reconnect
// attempt whose dial succeeds and whose handshake fails (RECONNECTING again, nats.go:3491) - a
// proxy that accepts and closes, a hub refusing the credential - while the life's goroutine has not
// yet received the first report leaves its channel empty and the listener deleted. The goroutine
// also wakes at the client's connection events (ConnectionChanged) and reads the connection down.
// The gate holds the goroutine between its registration and its first wait, as a goroutine not yet
// scheduled is held.
//
// The control is the goroutine waiting on its listener alone: the life outlives the drop.
func TestALifeEndsAtADropWhoseReportNatsGoTookBack(t *testing.T) {
	hold := newStageHold("select")
	client, proxy := proxiedClient(t, withLifeListenGate(hold.gate))
	t.Cleanup(hold.release)
	conn := client.Connection()
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	before := conn.Stats().Reconnects
	answered := inFlight(t, ctx, client)
	select {
	case <-hold.held:
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the life's listener goroutine never reached its wait")
	}
	proxy.Cut()
	// nats.go counts a reconnect once the attempt's dial succeeds (nats.go:3482), and the proxy's
	// cut closes the connection it accepted, so the handshake fails and nats.go reports
	// RECONNECTING again under the same lock hold; Stats waits for that hold.
	deadline := time.Now().Add(testnats.Remaining(t))
	for conn.Stats().Reconnects <= before {
		if time.Now().After(deadline) {
			t.Fatal("no reconnect attempt reached the cut proxy")
		}
		time.Sleep(5 * time.Millisecond)
	}
	proxy.Restore()
	waitForNatsStatus(t, conn, "connected again", func(s nats.Status) bool { return s == nats.CONNECTED })
	hold.release()
	requireLostAtDrop(t, answered, "a drop whose report nats.go took back")
}
