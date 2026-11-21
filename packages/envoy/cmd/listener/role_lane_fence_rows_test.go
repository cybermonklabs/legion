package main

import (
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
)

// The role lane's stale-resolution fence (roles.ForwardFenceAge) and its held messages meet in
// one worker (roleForwarder.publish). These rows hold both rules on the listener's clock
// (cfg.now), which they move by hand, so no wait of theirs is a wall-clock bound on the product.

// staleFenceLine is the line of a forward whose resolution was too old when its publish started.
const staleFenceLine = "listener role forward resolves its role's holder again: its resolution is too old to forward on"

// fenceClock is a listener clock a test moves by hand.
type fenceClock struct{ at atomic.Int64 }

func newFenceClock() *fenceClock {
	c := &fenceClock{}
	c.at.Store(time.Now().UnixNano())
	return c
}

func (c *fenceClock) now() time.Time { return time.Unix(0, c.at.Load()) }

func (c *fenceClock) advance(by time.Duration) { c.at.Add(int64(by)) }

// A forward queued behind its holder's earlier forward whose resolution is ForwardFenceAge old
// when its publish starts is not made to that holder: the message is delivered again, which
// resolves the role's holder, and goes to the session that holds the role now, its slot carried
// and freed once. The first message's forward holds the first holder's worker while the role moves
// to a second session and the listener's clock passes the fence. A worker that publishes the
// queued forward on its old resolution stores it for the first holder, which no longer holds the
// role.
func TestAQueuedForwardPastTheFenceGoesToTheRolesHolderNow(t *testing.T) {
	const second = "ses_fence_second"
	r := newPinRig(t, "fence-stale", "ses_fence_first")
	clock := newFenceClock()
	release := make(chan struct{})
	var forwards atomic.Int32
	real := startRoleForward(r.harness.client)
	cfg := r.harness.config
	cfg.now = clock.now
	cfg.forwardRole = func(subject string, item contracts.Envelope, msgID string, timeout time.Duration) (func() error, error) {
		if forwards.Add(1) == 1 {
			<-release
		}
		return real(subject, item, msgID, timeout)
	}
	cfg = wireTestRoleForwarder(t, cfg)
	handler := coreNATSDeliveryHandler(cfg)
	first := listenerTestEnvelope(contracts.RoleTopicPrefix+r.role, "fence-stale-first")
	first.Payload = `{"type":"worker-queued"}`
	queued := listenerTestEnvelope(contracts.RoleTopicPrefix+r.role, "fence-stale-queued")
	queued.Payload = `{"type":"worker-queued"}`

	handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, first)})
	// The handler resolves and queues before it returns, so the second message is queued behind
	// the first's forward, on the clock's first reading.
	handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, queued)})
	moveRole(t, r.harness, r.role, second)
	clock.advance(roles.ForwardFenceAge)
	close(release)
	cfg.roleForwards.settled(t)

	if atFirst, atSecond := r.storedFor(t, r.first), r.storedFor(t, second); atFirst != 1 || atSecond != 1 {
		t.Fatalf("the stream holds %d copies for the first holder and %d for the second, want 1 and 1: the queued message resolved again:\n%s",
			atFirst, atSecond, r.harness.logs.String())
	}
	if got := countLogged(r.harness.logs.String(), staleFenceLine, queued.EventID); got != 1 {
		t.Fatalf("the queued message logged %d fence lines, want 1:\n%s", got, r.harness.logs.String())
	}
	if got := countLogged(r.harness.logs.String(), staleFenceLine, first.EventID); got != 0 {
		t.Fatalf("the first message, published on its fresh resolution, logged %d fence lines, want 0", got)
	}
	if got := cfg.roleForwards.outstanding(); got != 0 {
		t.Fatalf("%d slots are still held after both messages ended, want 0", got)
	}
}

// A held message pinned to the holder its stored forward went to (its acknowledgement lost) makes
// that same forward again past the fence: a pinned forward carries no resolution, so the fence
// does not send it to the role's holder now. The role moves to a second session, which the first
// does not leave (its session record stands), and the listener's clock passes the fence before the
// next attempt. The stream drops the repeat as the duplicate it is and holds one copy, the first
// holder's. A pinned forward judged by the fence would resolve the holder again and store a second
// copy for the second session.
func TestAPinnedForwardPastTheFenceStaysWithItsHolder(t *testing.T) {
	const second = "ses_fence_pinned_second"
	r := newPinRig(t, "fence-pinned", "ses_fence_pinned_first")
	clock := newFenceClock()
	var forwards atomic.Int32
	cfg := r.harness.config
	cfg.now = clock.now
	cfg.forwardRole = forwardLosingTheFirstAcknowledgement(r.harness.client, &forwards, func() {
		moveRole(t, r.harness, r.role, second)
		clock.advance(roles.ForwardFenceAge)
	})
	cfg = wireTestRoleForwarder(t, cfg)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+r.role, "fence-pinned")
	item.Payload = `{"type":"worker-queued"}`
	coreNATSDeliveryHandler(cfg)(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
	cfg.roleForwards.settled(t)

	if atFirst, atSecond := r.storedFor(t, r.first), r.storedFor(t, second); atFirst != 1 || atSecond != 0 {
		t.Fatalf("the stream holds %d copies for the first holder and %d for the second, want 1 and 0:\n%s",
			atFirst, atSecond, r.harness.logs.String())
	}
	if got := forwards.Load(); got != 2 {
		t.Fatalf("the lane made %d forwards, want 2: the stored one, and the same one again", got)
	}
	if got := countLogged(r.harness.logs.String(), staleFenceLine, item.EventID); got != 0 {
		t.Fatalf("the pinned message logged %d fence lines, want 0:\n%s", got, r.harness.logs.String())
	}
}
