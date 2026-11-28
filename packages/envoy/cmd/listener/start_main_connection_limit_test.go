package main

import (
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// An account at its connection limit refuses the listener's first connection, the main client's,
// as it refuses the session consumers' connection in
// TestAListenerWhoseAccountRefusesItsSessionConsumersConnectionWaitsForASlotThenServes. The limit is
// the account's state, which other connections of the machine's user change, and not the
// listener's own configuration, so the start rule keeps the pod up and not ready, /livez 204 and
// /healthz 503, and the pod serves once connections are freed. Two pods of one machine share one
// NATS user, so an exit here would send each pod into a crash loop whose backoff delays recovery.
func TestAListenerWhoseAccountRefusesItsMainConnectionWaitsForASlotThenServes(t *testing.T) {
	t.Parallel()
	const limit = 8
	config := `jetstream {}
accounts {
  LISTENER: {
    jetstream: enabled
    users: [ { user: listener, password: listener } ]
    limits: { max_connections: 8 }
  }
}
`
	_, uri := testnats.StartConfig(t, config, []string{"-js"}, false)
	uri = asListenerUser(t, uri)
	var held []*natsgo.Conn
	for range limit {
		conn, err := natsgo.Connect(uri)
		if err != nil {
			t.Fatalf("hold a connection of the account: %v", err)
		}
		t.Cleanup(conn.Close)
		held = append(held, conn)
	}

	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "main-connection-refused")
	// The main client's connect tries ten dials a second apart before it reports a failure, so
	// 30 s covers that and two more of the start's tries.
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		requireLiveAndStarting(t, listener, "its account refused the main client's connection")
	}
	for _, conn := range held {
		conn.Close()
	}
	listener.waitHealthy(t)
}
