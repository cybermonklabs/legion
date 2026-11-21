package bus

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// The case beside TestAFlapDuringTheEnsuresRequestRunsTheSecondDisconnectHookAtOnce: the run of
// the reconnect hooks that met the second outage is off nats.go's callback goroutine now, so the
// second disconnect's hooks run at once; but its stream request is still unanswered on the
// connection that dropped. When the connection is back a moment later, the reconnect that brings it
// back is due a run of the hooks of its own, which re-watch the KV caches the second disconnect
// told to stop following and ensure the stream on the server reached. Runs take turns (rewatch),
// so a first run that waits out its request's whole bound (the JetStream context's MaxWait, 10 s)
// holds the second reconnect's run behind it, and every cache answers from a bucket it does not
// follow for that long while the connection is up.
//
// flapProxy (stream_flap_request_test.go) drops the reconnect at its first $JS.API.STREAM request
// and refuses connections for 3 s; nats.go reconnects within a second or so of that. A hook added
// after the client's own records each run; this asks that a run begins within 3 s of the
// connection being back.
func TestASecondReconnectRunsItsHooksWithoutWaitingOutTheFirstRunsRequest(t *testing.T) {
	// Serial: its assertion is a time (the hooks run within 3 s of the connection being back).
	_, uri := testnats.Start(t)
	proxy := newFlapProxy(t, strings.TrimPrefix(uri, "nats://"))
	client, err := ConnectOwningStream([]string{"nats://" + proxy.addr()}, WithName(t.Name()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	var mu sync.Mutex
	var runs []time.Time
	client.AddReconnectHook(func(context.Context, *nats.Conn) error {
		mu.Lock()
		runs = append(runs, time.Now())
		mu.Unlock()
		return nil
	})

	proxy.cutAndArm(time.Second)
	var dropped time.Time
	select {
	case dropped = <-proxy.dropped:
	case <-time.After(30 * time.Second):
		t.Fatal("no stream request reached the proxy within 30s of the first outage")
	}
	// The connection is back once the proxy's 3 s refusal ends and nats.go reconnects.
	var back time.Time
	deadline := time.Now().Add(20 * time.Second)
	for {
		if conn := client.Connection(); conn != nil && conn.IsConnected() && time.Since(dropped) > 3*time.Second {
			back = time.Now()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the connection was not back within 20s of the second outage")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for time.Since(back) < 15*time.Second {
		mu.Lock()
		for _, at := range runs {
			if at.After(back) || at.After(dropped.Add(3*time.Second)) {
				late := at.Sub(back)
				mu.Unlock()
				if late > 3*time.Second {
					t.Fatalf("the hooks first ran %s after the connection was back (%s after the second outage began), want within 3s: the reconnect's run waited for the first run's unanswered request", late, at.Sub(dropped))
				}
				t.Logf("the hooks ran %s after the connection was back", late)
				return
			}
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the hooks did not run within 15s of the connection being back")
}
