package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A role claim read made under a role's keep guard - the handover's read of the role's holder
// (RoleClaim, under ResolveHolder) - on a context the guard's scope marks (bus.WithLockHeld),
// while the hub holds the handshake of the main client's reconnect, answers ErrConnectionLost
// while the handshake is still held: the read is bound under its caller's context, so its request
// answers "unknown" as the loss and never begins a wait for nats.go's own answer. The registry is
// opened on a main client through a handshake hold (testnats.HandshakeHold), so the claim read's
// handle is that client's. It decides by events: the answer must come before the read begins a
// wait for nats.go's own answer (bus.Client.AnswerWaitForTest), and while the handshake is held; a
// row that fails ends at that wait or at the test's deadline.
//
// The control is the claim read bound with context.Background() rather than its caller's context:
// the read holds no mark, begins the wait, and fails.
func TestARoleClaimReadUnderAGuardDuringAHeldHandshakeAnswersAtOnce(t *testing.T) {
	hold := testnats.StartHandshakeHold(t, testnats.URL(t))
	client, err := bus.ConnectOwningStream([]string{hold.URL()}, bus.WithReplicas(1), bus.WithConnectTimeout(testnats.Remaining(t)))
	if err != nil {
		t.Fatalf("connect the main client through the handshake hold: %v", err)
	}
	t.Cleanup(client.Close)
	t.Cleanup(hold.Release)
	registry := openRegistry(t, client.Connection())
	t.Cleanup(registry.StopWatch)
	if lost := client.LostNow(); lost != nil {
		t.Fatalf("the main client's question before the drop answered %v, want up", lost)
	}
	select {
	case <-hold.Hold():
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("no reconnect's CONNECT reached the server through the hold by the test's deadline")
	}
	waited := make(chan struct{}, 1)
	restore := client.AnswerWaitForTest(func() {
		select {
		case waited <- struct{}{}:
		default:
		}
	})
	defer restore()
	ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
	defer cancel()
	answered := make(chan error, 1)
	go func() {
		_, err := registry.RoleClaim(bus.WithLockHeld(ctx), "marked-role")
		answered <- err
	}()
	select {
	case err := <-answered:
		if !errors.Is(err, bus.ErrConnectionLost) {
			t.Fatalf("the role claim read under a guard during the held handshake answered %v, want ErrConnectionLost", err)
		}
		select {
		case <-hold.Ended():
			t.Fatal("the role claim read answered only once the held handshake had ended")
		default:
		}
	case <-waited:
		hold.Release()
		t.Fatal("the role claim read under a guard began a wait for nats.go's own answer, with the guard held")
	case <-hold.Ended():
		t.Fatal("the role claim read gave no answer while the handshake was held")
	case <-time.After(testnats.Remaining(t)):
		hold.Release()
		t.Fatal("the role claim read gave no answer by the test's deadline while the handshake was held")
	}
}
