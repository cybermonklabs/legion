package bus

import (
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A Request answers ErrUnavailable the moment the life its window pinned ends (connLife), whether
// or not the client has heard of the loss yet: lifeEnded says why the two come in either order.
//
// The row ends the window's life as the drain does on nats.go's report, while the connection stays
// up, so the client hears of nothing and no connection event comes at all. The request's subject
// is a role lane, which no stream subject captures, so nothing acknowledges it, and its timeout is
// a day: the answer can only be the life's end. A Request that waits for the client's event
// instead waits until the test binary's deadline less testwait's margin, the row's one bound, or
// under -timeout 0 until its day runs out and it answers ErrNotAcknowledged; either fails the row.
func TestARequestAnswersUnavailableOnceItsWindowsLifeEndsBeforeTheClientHearsOfTheLoss(t *testing.T) {
	t.Parallel()
	_, url := testnats.Start(t)
	client, err := ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	w := client.WindowUntil(nil).(*window)

	const subject = "notifications.role.request-life"
	item := contracts.Envelope{EventID: "evt-life", Source: "dispatch", SourceEventID: "src-life", Topic: subject, DedupeKey: "envoy.role.forward.dispatch-life", IssuedAt: contracts.NowMillis()}
	pending := w.Request(subject, item, RequestOptions{MsgID: ForwardMsgID("life", subject), Timeout: 24 * time.Hour})
	lifeOf(w.conn).end(nats.RECONNECTING)

	if err := testwait.Receive(t, "the answer of the Request whose window's life ended", pending); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNotAcknowledged) {
		t.Fatalf("a Request whose window's life ended returned %v, want ErrUnavailable", err)
	}
	if !client.Connected() {
		t.Fatal("control: the client lost its connection, so it heard of a loss; the row is a life that ended while the client heard nothing")
	}
}
