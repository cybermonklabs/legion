package bus_test

import (
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Every request a client built WithInboxPrefix makes names its reply inbox under the prefix: a
// stored forward's (StartRequestStoredTo) and a window's (Sender.Request) alike. A client whose
// grant subscribes only prefix.> is otherwise refused the inbox subscription of each.
//
// Control: put either site back on the package's nats.NewInbox(), which is always _INBOX.; its
// request's reply reads _INBOX. and its row fails.
func TestEveryRequestOfAPrefixedClientRepliesUnderThePrefix(t *testing.T) {
	const prefix = "_INBOX_prefix-test"
	url := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithInboxPrefix(prefix))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	watcher, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect the watcher: %v", err)
	}
	t.Cleanup(watcher.Close)
	replies := make(chan *natsgo.Msg, 8)
	if _, err := watcher.ChanSubscribe("test.uncaptured.inbox-prefix.>", replies); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := watcher.Flush(); err != nil {
		t.Fatalf("flush the watcher: %v", err)
	}
	item := contracts.Envelope{EventID: "evt-inbox-prefix", Source: "dispatch", SourceEventID: "src-inbox-prefix", Topic: "notifications.role.inbox-prefix", DedupeKey: "inbox-prefix", IssuedAt: contracts.NowMillis()}

	const stored = "test.uncaptured.inbox-prefix.stored"
	forward, err := client.StartRequestStoredTo(stored, item, bus.ForwardMsgID(item.DedupeKey, stored), time.Second)
	if err != nil {
		t.Fatalf("StartRequestStoredTo: %v", err)
	}
	t.Cleanup(func() { _ = forward.Wait() })
	if reply := testwait.Receive(t, "the stored forward's request", replies).Reply; !strings.HasPrefix(reply, prefix+".") {
		t.Fatalf("the stored forward's reply inbox is %q, not under %s", reply, prefix)
	}

	const windowed = "test.uncaptured.inbox-prefix.window"
	answer := client.WindowUntil(nil).Request(windowed, item, bus.RequestOptions{MsgID: bus.ForwardMsgID(item.DedupeKey, windowed), Timeout: time.Second})
	t.Cleanup(func() { <-answer })
	if reply := testwait.Receive(t, "the window's request", replies).Reply; !strings.HasPrefix(reply, prefix+".") {
		t.Fatalf("the window's request's reply inbox is %q, not under %s", reply, prefix)
	}
}
