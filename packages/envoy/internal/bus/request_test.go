package bus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A request made while its client's connection is down is not sent: Request answers
// ErrConnectionLost at once, without calling send, where nats.go would refuse the publish with
// the reconnect buffer off. Its retry rule holds: Reconnected waits for the connection, and the
// request made again once it is back is sent and answered.
//
// The negative control is the same request before the cut, which is sent and answered.
func TestARequestWhileTheConnectionIsDownIsNotSentAndIsSentOnceItIsBack(t *testing.T) {
	proxy := testnats.StartProxy(t, testnats.URL(t))
	client, err := ConnectOwningStream([]string{proxy.URL()}, WithReplicas(1))
	if err != nil {
		t.Fatalf("connect through the proxy: %v", err)
	}
	t.Cleanup(client.Close)
	sends := 0
	read := func(ctx context.Context) (*nats.StreamInfo, error) {
		sends++
		return client.js.StreamInfo(Stream, nats.Context(ctx))
	}
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	if _, err := Request(ctx, client, read); err != nil || sends != 1 {
		t.Fatalf("the request on a live connection answered %v after %d sends, want an answer after one", err, sends)
	}

	proxy.Cut()
	for client.Connected() {
		time.Sleep(time.Millisecond)
	}
	_, err = Request(ctx, client, read)
	if !errors.Is(err, ErrConnectionLost) || sends != 1 {
		t.Fatalf("the request while the connection was down answered %v after %d sends in all, want ErrConnectionLost and no send", err, sends)
	}
	if Reconnected(ctx, errors.New("some other failure")) {
		t.Fatal("Reconnected reported a retry for an error that is no lost connection")
	}
	ended, end := context.WithCancel(ctx)
	end()
	if Reconnected(ended, err) {
		t.Fatal("Reconnected reported a retry for a caller whose context had ended")
	}

	time.AfterFunc(200*time.Millisecond, proxy.Restore)
	if !Reconnected(ctx, err) {
		t.Fatal("Reconnected did not report the connection back once the proxy was restored")
	}
	if _, err := Request(ctx, client, read); err != nil || sends != 2 {
		t.Fatalf("the request made again once the connection was back answered %v after %d sends in all, want an answer after two", err, sends)
	}
}

// A request that answers before its connection is lost keeps its answer: the loss of the
// connection afterwards changes nothing it returned, and a request made on the reconnected
// connection is bound to the new connection's life, not the one that ended.
func TestALostConnectionEndsOnlyTheRequestsItCutOff(t *testing.T) {
	client, hold := heldClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	answered, err := Request(ctx, client, func(ctx context.Context) (string, error) { return "answer", nil })
	if err != nil || answered != "answer" {
		t.Fatalf("a request that answered returned %q, %v", answered, err)
	}
	hold.Drop()
	for !client.Connected() {
		time.Sleep(time.Millisecond)
	}
	for {
		_, err := Request(ctx, client, func(ctx context.Context) (*nats.StreamInfo, error) {
			return client.js.StreamInfo(Stream, nats.Context(ctx))
		})
		if err == nil {
			break
		}
		// A request made between nats.go's reconnect and the end of the life it cut off can be
		// that life's; its retry is the one on the new connection.
		if !Reconnected(ctx, err) {
			t.Fatalf("a request on the reconnected connection answered %v", err)
		}
	}
}
