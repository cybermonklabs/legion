package main

import (
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The server's own connection limit, a top-level max_connections, refuses the listener's first
// connection, the main client's, as the account's limit refuses it in
// TestAListenerWhoseAccountRefusesItsMainConnectionWaitsForASlotThenServes. nats-server answers the
// connect's handshake with -ERR 'maximum connections exceeded', which nats.go returns as its
// protocol error, "nats: maximum connections exceeded", and never as nats.ErrMaxConnectionsExceeded,
// whose text says "server maximum connections exceeded" (errServerConnectionLimit). The limit is
// the server's state, which other connections change, so the start rule keeps the pod up and not
// ready, /livez 204 and /healthz 503, logs connectionLimitLine at ERROR with the server's words at
// each try, and the pod serves once connections are freed. Under a connectionLimitRefusal that
// matched nats.ErrMaxConnectionsExceeded alone, the start exited 1 at its first refusal.
func TestAListenerWhoseServerRefusesItsMainConnectionAtItsConnectionLimitWaitsForASlotThenServes(t *testing.T) {
	t.Parallel()
	const limit = 8
	config := `max_connections: 8
jetstream {}
accounts {
  LISTENER: {
    jetstream: enabled
    users: [ { user: listener, password: listener } ]
  }
}
`
	_, uri := testnats.StartConfig(t, config, []string{"-js"}, false)
	uri = asListenerUser(t, uri)
	var held []*natsgo.Conn
	for range limit {
		conn, err := natsgo.Connect(uri)
		if err != nil {
			t.Fatalf("hold a connection of the server: %v", err)
		}
		t.Cleanup(conn.Close)
		held = append(held, conn)
	}
	if conn, err := natsgo.Connect(uri, natsgo.NoReconnect()); err == nil {
		conn.Close()
		t.Fatalf("the server took a connection past its max_connections of %d: this row needs its refusal", limit)
	}

	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "main-connection-server-limit")
	refusals := func() []map[string]any {
		var found []map[string]any
		for _, record := range linesIn(listener.Output.String(), connectionLimitLine, map[string]any{"level": "ERROR"}) {
			if text, _ := record["error"].(string); strings.Contains(text, "nats: maximum connections exceeded") {
				found = append(found, record)
			}
		}
		return found
	}
	// The main client's connect tries ten dials a second apart before it reports a failure, so the
	// listener's second refusal line, which the start's next try logs, shows it stayed up past
	// them. The wait ends on that fact, bounded by the test binary's own deadline, and the pod is
	// held live and not ready meanwhile.
	testwait.WithinPolling(t, testwait.Bounded(t, time.Hour, 30*time.Second), 200*time.Millisecond,
		"the listener to log its second "+connectionLimitLine+" naming the server's limit", func() bool {
			requireLiveAndStarting(t, listener, "the server's connection limit refused the main client's connection")
			return len(refusals()) >= 2
		})
	if first := refusals()[0]; first["retry_in"] != "1s" {
		t.Fatalf("the listener's first refusal line = %v, want retry_in 1s", first)
	}
	for _, conn := range held {
		conn.Close()
	}
	listener.waitHealthy(t)
}
