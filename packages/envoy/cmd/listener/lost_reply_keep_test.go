package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A keep whose acknowledgement the connection's drop lost, the copy itself stored, answers that
// copy as kept (storeKeptRoleCopy): the bus sends the keep's publish once and answers
// bus.ErrConnectionLost (PublishStoredAck), the keep publishes again under the same id once the
// connection is back, and the stream's duplicate answer names the copy the first send stored,
// which the keep reads back as its own (heldKeptRoleCopy). So the stream holds one copy, and the
// keep reports it kept, as the publish route's 202 and the role lane's count then say. The hold
// drops the connection and lets it reconnect (testnats.ReplyHold.Drop), so a publish sent again
// would be. The keep runs under the test's own deadline, not the 2 s roleKeepPublishWindow
// keepRoleMessage gives it, so the order of events decides the row, not how soon the box
// reconnects.
//
// The control is this row at 36d203dcf5, whose keep answered the loss as a keep that failed while
// the stream held the copy, so the publish route answered 404 and the sender's fallback ran for a
// message the role's next holder is handed.
func TestAKeepWhoseAckTheDropLostAnswersTheStoredCopyAsKept(t *testing.T) {
	hold := testnats.StartReplyHold(t, testnats.URL(t))
	client, err := bus.ConnectOwningStream([]string{hold.URL()}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener through the hold: %v", err)
	}
	t.Cleanup(client.Close)
	const role = "lost-keep-role"
	subject := bus.KeptRoleSubject(role)
	cfg := listenerDeliveryHandlerConfig{client: client, logger: logging.NewWithWriter("test", &cmdtest.LockedBuffer{}), now: time.Now}
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "lost-keep-00")

	held := hold.Hold(subject)
	type answer struct {
		kept keptRoleCopy
		err  error
	}
	answered := make(chan answer, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t)-5*time.Second)
		defer cancel()
		kept, err := storeKeptRoleCopy(ctx, cfg, role, item, client.PublishStoredAck)
		answered <- answer{kept, err}
	}()
	select {
	case <-held.Withheld():
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the hold withheld no acknowledgement of the keep's publish by the test's deadline")
	}
	dropped := time.Now()
	hold.Drop()
	var got answer
	select {
	case got = <-answered:
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the keep whose acknowledgement the drop lost was still waiting at the test's deadline")
	}
	if got.err != nil {
		t.Fatalf("the keep answered %v %s after the drop (ErrConnectionLost: %t), want the stored copy answered as kept",
			got.err, time.Since(dropped), errors.Is(got.err, bus.ErrConnectionLost))
	}
	ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
	defer cancel()
	first, err := client.NextStoredMessage(ctx, subject, 1)
	if err != nil {
		t.Fatalf("read the kept lane's first copy: %v", err)
	}
	if got.kept.sequence != first.Sequence || got.kept.item.EventID != item.EventID {
		t.Fatalf("the keep answered the copy at %d of event %s, want the stored copy at %d of event %s",
			got.kept.sequence, got.kept.item.EventID, first.Sequence, item.EventID)
	}
	if next, err := client.NextStoredMessage(ctx, subject, first.Sequence+1); !errors.Is(err, nats.ErrMsgNotFound) {
		t.Fatalf("the kept lane holds a copy past %d (at %d, %v), want one copy", first.Sequence, next.Sequence, err)
	}
	t.Logf("the keep answered its own stored copy at sequence %d as kept %s after the drop", first.Sequence, time.Since(dropped))
}
