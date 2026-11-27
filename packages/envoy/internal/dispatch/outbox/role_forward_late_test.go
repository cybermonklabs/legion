package outbox

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
)

// heldForward is a publisher whose window holds the first forward to subject after the forward
// has passed its fence and before it is published, until release is closed, as a goroutine the
// machine did not run holds one: entered is closed once that forward has reached the window.
// Every other send goes through at once.
type heldForward struct {
	*bus.Client
	subject string
	entered chan struct{}
	release chan struct{}
	taken   atomic.Bool
}

func (p *heldForward) WindowUntil(deadline func() time.Time) bus.Sender {
	return heldForwardWindow{Sender: p.Client.WindowUntil(deadline), publisher: p}
}

type heldForwardWindow struct {
	bus.Sender
	publisher *heldForward
}

func (w heldForwardWindow) Request(subject string, item contracts.Envelope, opts bus.RequestOptions) <-chan error {
	if subject != w.publisher.subject || !w.publisher.taken.CompareAndSwap(false, true) {
		return w.Sender.Request(subject, item, opts)
	}
	answer := make(chan error, 1)
	close(w.publisher.entered)
	go func() {
		<-w.publisher.release
		answer <- <-w.Sender.Request(subject, item, opts)
	}()
	return answer
}

// A forward that passed its fence and was then held past the fence and its acknowledgement window
// before the stream stored it is no delivery: the holder it was resolved for may have lost the role
// meanwhile, and the copy stored on that holder's subject may be one no takeover copies on. The
// row stays pending, naming roles.ErrForwardStoredTooLate, and the next read resolves the role's
// holder again and forwards the event to it, which takes it exactly once. At legion main the
// stream's acknowledgement counts the forward delivered and the event is published, so the
// session that holds the role then is never sent it.
func TestAForwardStoredAfterItsFenceAndAckWindowIsNotADelivery(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role, first, second = "forward-stored-late", "ses_stored_late_first", "ses_stored_late_second"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	rig.claim(t, role, first, session.SessionEntry{SelfSubscribed: true})
	rig.receiver(t, first)
	next := rig.receiver(t, second)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})

	publisher := &heldForward{Client: rig.client, subject: contracts.LegacyAgentSubject(first),
		entered: make(chan struct{}), release: make(chan struct{})}
	var offset atomic.Int64
	deps := rig.publisherOn(publisher)
	deps.clock = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	outbox := holding(t, deps)
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		outbox.scan(ctx, unfenced{})
	}()
	select {
	case <-publisher.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the forward to the first holder did not reach the window within 30s")
	}
	// The role moves to the next holder while the forward waits, and the forward is stored past
	// its resolution's fence and acknowledgement window.
	rig.claim(t, role, second, session.SessionEntry{SelfSubscribed: true})
	offset.Store(int64(roles.ForwardFenceAge + deps.forwardAckTimeout() + 10*time.Minute))
	close(publisher.release)
	select {
	case <-scanned:
	case <-time.After(30 * time.Second):
		t.Fatal("the scan did not return within 30s of the held forward's release")
	}

	if at := publishedAt(t, rig.database, event.ID); at != nil {
		t.Fatalf("the event was published at %v, though its forward was stored too late to count as delivered", at)
	}
	if _, failure, destinations := eventState(t, rig.database, event.ID); failure == nil ||
		!strings.Contains(*failure, roles.ErrForwardStoredTooLate.Error()) || strings.Contains(strings.Join(destinations, ","), contracts.RoleTopicPrefix) {
		t.Fatalf("publish_error %v, published_destinations %v: want the role route left pending, naming the late store", failure, destinations)
	}

	offset.Store(0)
	dueNow(t, rig.database, event.ID)
	outbox.scan(ctx, unfenced{})
	forward := handedNow(t, next, "the next read resolves the role's new holder")
	if want := fmt.Sprintf("dispatch-%d", event.ID); forward.EventID != want {
		t.Fatalf("the new holder took event %q, want %q", forward.EventID, want)
	}
	if publishedAt(t, rig.database, event.ID) == nil {
		t.Fatal("the event is still unpublished once its forward reached the new holder")
	}
	outbox.scan(ctx, unfenced{})
	if copies := storedCopies(t, rig.client, contracts.LegacyAgentSubject(second)); copies != 1 {
		t.Fatalf("the new holder's subject holds %d copies, want the event exactly once", copies)
	}
}
