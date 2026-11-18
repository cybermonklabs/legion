package bus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// withLifeEndGate has the client run gate right before each end of a connection's life
// (endLifeNow), so a row can hold that end.
func withLifeEndGate(gate func()) ConnectOption {
	return clientOption(func(o *connectOpts) { o.lifeEndGate = gate })
}

// lifeEndHold holds every end of a gated client's connection life until release, and reports when
// the first end was held.
type lifeEndHold struct {
	held, released chan struct{}
	once           sync.Once
	releaseOnce    sync.Once
}

func newLifeEndHold() *lifeEndHold {
	return &lifeEndHold{held: make(chan struct{}), released: make(chan struct{})}
}

func (h *lifeEndHold) gate() {
	h.once.Do(func() { close(h.held) })
	<-h.released
}

func (h *lifeEndHold) release() { h.releaseOnce.Do(func() { close(h.released) }) }

// gatedClient is heldClient with every end of its connection's life held by hold until release.
func gatedClient(t *testing.T, hold *lifeEndHold) (*Client, *testnats.ReplyHold) {
	t.Helper()
	relay := testnats.StartReplyHold(t, testnats.URL(t))
	client, err := ConnectOwningStream([]string{relay.URL()}, WithReplicas(1), withLifeEndGate(hold.gate))
	if err != nil {
		t.Fatalf("connect through the hold: %v", err)
	}
	t.Cleanup(client.Close)
	t.Cleanup(hold.release)
	return client, relay
}

// connectionCloses is nats.go's answer for a request whose connection closed or is closing under
// it, one row each, and one of them wrapped as a caller wraps it: the classes bindRequest names a
// lost request on a running client (closedUnder).
var connectionCloses = []error{
	nats.ErrConnectionClosed,
	nats.ErrConnectionDraining,
	nats.ErrDrainTimeout,
	nats.ErrNoServers,
	fmt.Errorf("nats: the reply's channel was closed: %w", nats.ErrConnectionClosed),
}

// A KV read whose connection closed under it, the client still running, answers ErrConnectionLost
// when nats.go answers it before the client has ended the connection's life, which is the order
// nats.go gives: close sets the status CLOSED (nats.go:6070) and clears the pending requests
// (:6086) before the status report that ends the life (changeConnStatus, :6142). The row makes that
// order certain rather than likely: the client's life cannot end while the gate holds it
// (withLifeEndGate), and the gate is released only once the read has its answer. Reconnected
// matches the answer and the client recovers its connection (onClosed). The read's own handle was
// opened on the closed connection, which the client has replaced, so a read made on it again answers
// nats.ErrConnectionClosed, which no retry answers, and not ErrConnectionLost, which its caller
// would retry for ever; a handle opened on the client's connection reads the bucket.
//
// The control is this row with closedUnder left out of bindRequest's finish: the read answers
// nats.ErrConnectionClosed, which Reconnected does not match.
func TestAReadAnsweredForItsConnectionsCloseBeforeTheLifeEndsIsLost(t *testing.T) {
	gate := newLifeEndHold()
	client, relay := gatedClient(t, gate)
	kv := lostReplyBucket(t, client)
	held := relay.Hold("$JS.API.DIRECT.GET.KV_envoy_lost_reply")
	answered := make(chan error, 1)
	go func() {
		_, err := kv.Get("key")
		answered <- err
	}()
	select {
	case <-held.Withheld():
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the hold withheld no reply by the test's deadline")
	}
	client.Connection().Close()
	var err error
	select {
	case err = <-answered:
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the read the connection's close cut off was still waiting at the test's deadline")
	}
	gate.release()
	if !errors.Is(err, ErrConnectionLost) {
		t.Fatalf("the read a running client's connection close cut off answered %v, before its life ended, want ErrConnectionLost", err)
	}
	relay.Drop()
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	if !Reconnected(ctx, err) {
		t.Fatalf("the client did not recover its connection within the row's wait: %v", err)
	}
	if _, err := kv.Get("key"); !errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, ErrConnectionLost) {
		t.Fatalf("a read on the handle of the replaced connection answered %v, want nats.ErrConnectionClosed and not ErrConnectionLost", err)
	}
	if _, err := lostReplyBucket(t, client).Get("key"); !errors.Is(err, nats.ErrKeyNotFound) {
		t.Fatalf("a read on a handle of the client's connection answered %v, want the bucket's own answer", err)
	}
}

// Each class of nats.go's answer for a connection's close, given to a request bound to a running
// client's connection once that connection is closed but before the client has ended its life (the
// gate holds that end), is a lost request (ErrConnectionLost), which Reconnected matches. The same
// answer to a request bound while its connection is still connected is a handle's of a connection
// the client has replaced, and stays nats.go's: the row before the close shows it.
//
// The control is this row with closedUnder left out of bindRequest's finish: every class answers
// nats.go's own error.
func TestEveryCloseOfARunningClientsConnectionIsALostRequest(t *testing.T) {
	gate := newLifeEndHold()
	client, _ := gatedClient(t, gate)
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	for _, closed := range connectionCloses {
		_, finish := client.bindRequest(ctx)
		if err := finish(closed); !errors.Is(err, closed) || errors.Is(err, ErrConnectionLost) {
			t.Fatalf("%v answered to a request on a connected connection came back %v, want it as it is", closed, err)
		}
	}
	finishes := make([]func(error) error, len(connectionCloses))
	for i := range connectionCloses {
		_, finishes[i] = client.bindRequest(ctx)
	}
	client.Connection().Close()
	select {
	case <-gate.held:
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("no end of the closed connection's life reached the gate by the test's deadline")
	}
	for i, closed := range connectionCloses {
		err := finishes[i](closed)
		if !errors.Is(err, ErrConnectionLost) {
			t.Errorf("%v answered for the closed connection before its life ended came back %v, want ErrConnectionLost", closed, err)
			continue
		}
		var lost *connectionLost
		if !errors.As(err, &lost) || lost.client != client || lost.unsent {
			t.Errorf("%v came back %#v, want the client's connectionLost, sent", closed, err)
		}
	}
	gate.release()
	for i, closed := range connectionCloses {
		if err := finishes[i](closed); !Reconnected(ctx, err) {
			t.Errorf("Reconnected did not match %v once the client recovered its connection", err)
		}
	}
}

// The same KV read without the gate, made fifty times by its -count, answers ErrConnectionLost
// whichever of nats.go's answer and the end of the connection's life comes first, so the close of a
// running client's connection never reaches a caller as nats.ErrConnectionClosed.
func TestAReadARunningClientsConnectionCloseCutOffIsLost(t *testing.T) {
	client, relay := heldClient(t)
	kv := lostReplyBucket(t, client)
	held := relay.Hold("$JS.API.DIRECT.GET.KV_envoy_lost_reply")
	answered := make(chan error, 1)
	go func() {
		_, err := kv.Get("key")
		answered <- err
	}()
	select {
	case <-held.Withheld():
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the hold withheld no reply by the test's deadline")
	}
	client.Connection().Close()
	var err error
	select {
	case err = <-answered:
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the read the connection's close cut off was still waiting at the test's deadline")
	}
	if !errors.Is(err, ErrConnectionLost) {
		t.Fatalf("the read a running client's connection close cut off answered %v, want ErrConnectionLost", err)
	}
	relay.Drop()
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	if !Reconnected(ctx, err) {
		t.Fatalf("the client did not recover its connection within the row's wait: %v", err)
	}
}
