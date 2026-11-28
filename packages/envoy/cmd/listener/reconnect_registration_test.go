package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// Serial (listenerTestParallelism): it holds a wall-clock bound within a second or two of what it
// waits for: an answer inside the client's 5 s attempt, from a route whose budget is 3.5 s.
//
// A registration the listener answers while its NATS connection is reconnecting fails by name, and
// inside the client's own deadline (R14 of dispatch://INFRA-3589). A registration of a session the
// listener already serves reads and updates the session's consumer, and those are JetStream
// requests like any other write: R14 has each of them wait for the connection within its caller's
// deadline or fail with an error that says the bus is reconnecting. The caller is the plugin,
// whose envoy-client gives one attempt contracts.ListenerCallTimeout (LISTENER_CALL_TIMEOUT_MS,
// 5 s): an answer that comes later reaches nobody, and the plugin reads its own timeout instead.
//
// A consumer read bounded only by sessionConsumerCallTimeout (5 s) would wait it out, log "session
// consumer call failed; the registration is refused" with the error "context deadline exceeded",
// and answer 503 "the session consumer of <id> could not be read or created", which names no
// reconnect, 5 s after the request.
func TestARegistrationThroughANATSReconnectFailsByName(t *testing.T) {
	const (
		machineID = "reconnect-registration"
		sessionID = "ses_reconnect_registration"
	)
	server, uri := testnats.Start(t)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, machineID)
	listener.waitHealthy(t)
	postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-1"))

	// disconnected counts the main client's disconnect lines. That client is named
	// `envoy-listener <instance>` (connectListenerBus), which slog quotes for its space; the
	// lease's own connection, `envoy-listener-lease <instance>`, has a hyphen there, not the space.
	disconnected := func() int {
		return strings.Count(listener.Output.String(), `envoy nats disconnected name="envoy-listener `)
	}
	before := disconnected()
	server.Stop(t)
	waitFor(t, "the listener to see the NATS server go", func() bool { return disconnected() > before })

	started := time.Now()
	answer := reconnectWritesCall(listener.port, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-2"), 15*time.Second)
	took := time.Since(started)
	t.Logf("the registration during the outage answered %s after %s", answer, took.Round(time.Millisecond))
	if answer.err != nil || answer.status != http.StatusServiceUnavailable || !strings.Contains(answer.body, reconnectingReason) {
		t.Errorf("the registration during the outage answered %s, want 503 naming %q", answer, reconnectingReason)
	}
	if took >= contracts.ListenerCallTimeout {
		t.Errorf("the registration during the outage was answered after %s, want inside the client's %s attempt (contracts.ListenerCallTimeout), or the plugin never reads the answer",
			took.Round(time.Millisecond), contracts.ListenerCallTimeout)
	}
	if strings.Contains(listener.Output.String(), reconnectBufferRefusal) {
		t.Errorf("the listener logged %q during the outage, want the reconnect named instead", reconnectBufferRefusal)
	}
}
