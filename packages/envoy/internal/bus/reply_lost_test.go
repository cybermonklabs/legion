package bus

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/testnats"
)

// TestASessionStreamStateReadWhoseFirstReplyIsLostIsSentAgain proves that one shared STREAM.INFO
// read does not fail every SessionConsumers.streamState waiter. The proxy drops the first answer
// after the server sent it, as nats-server's service-import response-prefix race does.
func TestASessionStreamStateReadWhoseFirstReplyIsLostIsSentAgain(t *testing.T) {
	t.Parallel()
	_, uri := testnats.Start(t)
	owner, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the stream owner: %v", err)
	}
	t.Cleanup(owner.Close)

	armed := make(chan struct{})
	proxy := testnats.StartProxy(t, uri, testnats.DropFirstReplyForRequests("$JS.API.STREAM.INFO."+Stream, armed))
	consumers, err := ConnectSessionConsumers([]string{proxy.URL()}, 0, nil)
	if err != nil {
		t.Fatalf("connect session consumers through the reply-dropping proxy: %v", err)
	}
	t.Cleanup(consumers.Close)
	close(armed)

	// Hold the reader off until both calls have joined its one streamRead, so a success proves the
	// reply retry served the whole shared cohort and not merely one later call.
	consumers.streamMu.Lock()
	consumers.streamReading = true
	consumers.streamMu.Unlock()
	first := consumers.joinStreamRead()
	second := consumers.joinStreamRead()
	if first != second {
		t.Fatal("two stream-state waiters joined different reads")
	}
	go consumers.readStreamStates()
	for waiter, read := range map[string]*streamRead{"first": first, "second": second} {
		state, err := read.wait(context.Background())
		if err != nil {
			t.Fatalf("%s shared STREAM.INFO read after its first reply was lost: %v", waiter, err)
		}
		if state.id == "" {
			t.Fatalf("%s shared STREAM.INFO read after its first reply was lost returned no stream id", waiter)
		}
	}
	if dropped := proxy.DroppedReplies(); dropped != 1 {
		t.Fatalf("the STREAM.INFO reply drop fired %d times, want once", dropped)
	}
}
