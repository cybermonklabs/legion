package bus

import (
	"context"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The case beside TestAFlapDuringTheEnsuresRequestRunsTheSecondDisconnectHookAtOnce: the run of
// the reconnect hooks that met the second outage is off nats.go's callback goroutine now, so the
// second disconnect's hooks run at once; but its request is still unanswered on the connection
// that dropped. When the connection is back, the reconnect that brings it back is due a run of the
// hooks of its own, which re-watch the KV caches the second disconnect told to stop following and
// ensure the stream on the server reached. Runs take turns (rewatch), so a first run that waited
// out its request's whole bound (the JetStream context's MaxWait, 10 s) held the second
// reconnect's run behind it, and every cache answered from a bucket it did not follow for that
// long while the connection was up. A run's hooks now ask under the run's context, which the loss
// of its connection ends (whileConnected).
//
// No clock decides the order here. A reconnect hook of the test's own, added after the client's
// ensure, asks a responder that never answers (silentResponder) under the run's context, as every
// hook asks, and so waits for as long as that context lets it. The first reconnect's run asks it;
// once that request has reached the responder, the test cuts the connection at the proxy
// (flapProxy), and the run of the reconnect that brings the connection back must ask again while
// the first request is still unanswered, as it always is. A run whose context the connection's
// loss did not end would wait for that answer for ever, with every later run behind it, and the
// wait for the second request gives up naming it.
func TestASecondReconnectRunsItsHooksWithoutWaitingOutTheFirstRunsRequest(t *testing.T) {
	_, uri := testnats.Start(t)
	asked := silentResponder(t, uri)
	proxy := newFlapProxy(t, strings.TrimPrefix(uri, "nats://"))
	client, err := ConnectOwningStream([]string{"nats://" + proxy.addr()}, WithName(t.Name()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	client.AddReconnectHook(func(ctx context.Context, conn *nats.Conn) error {
		_, err := conn.RequestWithContext(ctx, unansweredSubject, nil)
		return err
	})

	proxy.cut()
	testwait.Receive(t, "the first reconnect's run of the hooks to ask the responder that never answers", asked)
	proxy.cut()
	testwait.Receive(t, "the run of the hooks of the reconnect that brought the connection back to ask again, the first run's request still unanswered", asked)
}
