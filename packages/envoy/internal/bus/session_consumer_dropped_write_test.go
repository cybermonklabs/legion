package bus

import (
	"context"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A write the server dropped is never answered (nats-server v2.15.1 drops the requests queued on
// its JetStream API past its queue limit, server/jetstream_api.go, apiDispatch, queue.drain()),
// so its wait holds one goroutine and one subscription until the connection's next reconnect or
// the stop (UnansweredWrite.Wait). The server here is restarted with a request queue of one entry,
// so every request that is not an info request is dropped, while info requests keep their own
// queue; the update's read of the consumer is answered and the update itself is dropped, which
// the server's own line says (the case's control: without that line it fails as one that did not
// make its condition). The rows end the wait as the doc says it ends: "reconnect", by restarting
// the server, so the connection reconnects; "stop" (the control), by the stop. In each the wait
// has not ended while the connection stayed up, and its subscription is gone once it has.
func TestADroppedWriteHoldsItsWaitUntilTheNextReconnectOrTheStop(t *testing.T) {
	for _, row := range []string{"reconnect", "stop"} {
		t.Run(row, func(t *testing.T) {
			srv, url := testnats.Start(t)
			owner, err := ConnectOwningStream([]string{url}, WithReplicas(1))
			if err != nil {
				t.Fatalf("create the notification stream: %v", err)
			}
			owner.Close()
			// Every bucket and record a create reads is made here, on the server's default queue,
			// by one whole create: after the restart below those reads are direct gets or info
			// requests, which the request queue does not carry.
			warm, err := ConnectSessionConsumers([]string{url}, 0, nil)
			if err != nil {
				t.Fatalf("connect the session consumers before the restart: %v", err)
			}
			const sessionID = "ses_dropped_write_warm"
			warmCtx, warmCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if _, err := warm.Ensure(warmCtx, sessionID, []string{"notifications.agent." + sessionID}, 0); err != nil {
				t.Fatalf("create a consumer before the restart: %v", err)
			}
			warmCancel()
			warm.Close()
			const dropping = "host: 127.0.0.1\njetstream {\n  request_queue_limit: 1\n  info_queue_limit: 100000\n}\n"
			srv.Restart(t, dropping)

			consumers, err := ConnectSessionConsumers([]string{url}, 0, nil)
			if err != nil {
				t.Fatalf("connect the session consumers: %v", err)
			}
			t.Cleanup(consumers.Close)
			handed := make(chan *UnansweredWrite, 4)
			consumers.OnUnansweredWrite(func(w *UnansweredWrite) { handed <- w })

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err = consumers.UpdateFilter(ctx, sessionID, []string{"notifications.agent." + sessionID, "notifications.team.droppedwrite"})
			cancel()
			t.Logf("the update's caller ended with: %v", err)
			var w *UnansweredWrite
			select {
			case w = <-handed:
			case <-time.After(5 * time.Second):
				t.Fatalf("no write was handed on as unanswered; the caller ended with %v", err)
			}
			if !strings.Contains(srv.Log(), "limit reached, dropping") {
				t.Fatalf("control: the server logged no dropped request, so this case did not make the condition it is about")
			}

			stop := make(chan struct{})
			done := make(chan UnansweredWriteEnd, 1)
			go func() { done <- w.Wait(stop) }()
			// The cost the doc states: while the connection stays up, nothing ends the wait.
			select {
			case end := <-done:
				t.Fatalf("the wait for a write the server dropped ended %v on a connection that stayed up", end)
			case <-time.After(3 * time.Second):
			}
			if !w.sub.IsValid() {
				t.Fatal("the wait dropped the write's subscription while the connection stayed up")
			}
			want := WriteNotAwaited
			if row == "reconnect" {
				srv.Restart(t, dropping)
				want = WriteAnswerLost
			} else {
				close(stop)
			}
			select {
			case end := <-done:
				if end != want {
					t.Fatalf("the wait ended %v, want %v", end, want)
				}
			case <-time.After(30 * time.Second):
				if row == "reconnect" {
					close(stop)
				}
				t.Fatalf("the wait for a dropped write did not end at the %s", row)
			}
			if w.sub.IsValid() {
				t.Errorf("the wait ended at the %s and kept the write's subscription", row)
			}
		})
	}
}

// ServerPingInterval and ServerPingMax are nats-server's own defaults, which a hub whose
// configuration sets no ping_interval or ping_max runs on.
func TestTheServerPingDefaultsAreTheServersOwn(t *testing.T) {
	if ServerPingInterval != natsserver.DEFAULT_PING_INTERVAL || ServerPingMax != natsserver.DEFAULT_PING_MAX_OUT {
		t.Fatalf("ServerPingInterval %v and ServerPingMax %d, want the pinned server's %v and %d", ServerPingInterval, ServerPingMax, natsserver.DEFAULT_PING_INTERVAL, natsserver.DEFAULT_PING_MAX_OUT)
	}
	if got := StaleConnectionLimit(20*time.Second, 2); got != 80*time.Second {
		t.Fatalf("the stale limit of ping_interval 20s and ping_max 2 is %v, want 80s, the latest close the server's ping timer makes", got)
	}
}
