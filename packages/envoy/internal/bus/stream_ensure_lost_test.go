package bus

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// ensureAfterALostRequest bounds how long after a reconnect the stream's ensure that the reconnect
// lost a request of may take to finish, and the reconnect hook after it to run: the connection
// event, the request sent again on the connection that came back and the update behind it, with
// room for a loaded test machine, and far inside the JetStream context's 10 s wait the lost request
// held before.
const ensureAfterALostRequest = 2 * time.Second

// The stream's ensure on a connection event (ensureStreamOn, an owning client's first reconnect
// hook) whose read of the stream is lost with a connection that drops again mid-ensure finishes
// at the reconnect, and the hooks after it run then too. The relay drops the ensure's
// STREAM.INFO, so nothing answers it, and then cuts the link: nats.go fails no request its
// connection drops, so without the connection watch the read waited out its JetStream context's
// whole 10 s, and every hook queued behind it - the KV rewatch, the listener's receive - with it.
// The stream's age, changed behind the client's back, is what the ensure puts back, so the row
// reads the ensure's end off the stream itself.
func TestAStreamEnsureWhoseReadIsLostWithItsConnectionFinishesAtTheReconnect(t *testing.T) {
	uri := testnats.URL(t)
	relay := testnats.StartUnansweredRelayOn(t, uri, streamInfoSubject)
	// The keeper also puts the stream's age back; a period past the row keeps that to the ensure.
	client, err := ConnectOwningStream([]string{relay.URL()}, WithKeepInterval(time.Hour))
	if err != nil {
		t.Fatalf("connect the stream's owner through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	var hookMu sync.Mutex
	var hookRuns []time.Time
	client.AddReconnectHook(func(context.Context, *nats.Conn) error {
		hookMu.Lock()
		defer hookMu.Unlock()
		hookRuns = append(hookRuns, time.Now())
		return nil
	})
	firstHookRunAfter := func(at time.Time) (time.Time, bool) {
		hookMu.Lock()
		defer hookMu.Unlock()
		for _, run := range hookRuns {
			if !run.Before(at) {
				return run, true
			}
		}
		return time.Time{}, false
	}

	admin := testnats.Connect(t, uri)
	t.Cleanup(admin.Close)
	adminJS, err := admin.JetStream()
	if err != nil {
		t.Fatalf("open the admin's JetStream context: %v", err)
	}
	changed := deployedStream(t, adminJS).Config
	changed.MaxAge = streamCfg.MaxAge + time.Hour
	if _, err := adminJS.UpdateStream(&changed); err != nil {
		t.Fatalf("change the stream's age behind the client: %v", err)
	}

	// The first cut's reconnect runs the hooks; the ensure's read of the stream goes out and the
	// relay drops it. The second cut drops the connection that read went out on.
	conn := client.Connection()
	relay.Arm(1)
	cutAndAwaitReconnect(t, relay, conn)
	awaitRelaySent(t, relay, 1)
	reconnected := cutAndAwaitReconnect(t, relay, conn)

	var ensured, hooked time.Time
	deadline := reconnected.Add(15 * time.Second)
	for ensured.IsZero() || hooked.IsZero() {
		if time.Now().After(deadline) {
			t.Fatalf("15s after the reconnect the stream's age is %s (ensured %t) and the hook after the ensure ran %t; want the ensure finished and the hook run within %s of the reconnect",
				deployedStream(t, adminJS).Config.MaxAge, !ensured.IsZero(), !hooked.IsZero(), ensureAfterALostRequest)
		}
		if ensured.IsZero() && deployedStream(t, adminJS).Config.MaxAge == streamCfg.MaxAge {
			ensured = time.Now()
		}
		if hooked.IsZero() {
			if run, ok := firstHookRunAfter(reconnected); ok {
				hooked = run
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	ensuredAfter, hookedAfter := ensured.Sub(reconnected), hooked.Sub(reconnected)
	if ensuredAfter > ensureAfterALostRequest || hookedAfter > ensureAfterALostRequest {
		t.Fatalf("the stream's ensure whose read was lost with its connection finished %s after the reconnect and the hook after it ran %s after it; want both within ensureAfterALostRequest (%s), not the JetStream context's 10 s wait",
			ensuredAfter.Round(time.Millisecond), hookedAfter.Round(time.Millisecond), ensureAfterALostRequest)
	}
	t.Logf("the ensure finished %s and the hook after it ran %s after the reconnect", ensuredAfter.Round(time.Millisecond), hookedAfter.Round(time.Millisecond))
}
