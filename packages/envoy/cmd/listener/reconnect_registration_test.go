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

// A registration the listener answers while its NATS connection is reconnecting fails by name, and
// inside the client's own deadline (R14 of dispatch://INFRA-3589). A registration of a session the
// listener already serves reads and updates the session's consumer, and those are JetStream
// requests like any other write: R14 has each of them wait for the connection within its caller's
// deadline or fail with an error that says the bus is reconnecting. The caller is the plugin,
// whose envoy-client gives one attempt contracts.ListenerCallTimeout (LISTENER_CALL_TIMEOUT_MS,
// 5 s): an answer that comes later reaches nobody, and the plugin reads its own timeout instead.
//
// At 6eda53b5 the listener waits out sessionConsumerCallTimeout (5 s) on the consumer read, logs
// "session consumer call failed; the registration is refused" with the error "context deadline
// exceeded", and answers 503 "the session consumer of <id> could not be read or created", which
// names no reconnect, 5.0 s after the request.
func TestARegistrationThroughANATSReconnectFailsByName(t *testing.T) {
	const (
		machineID = "reconnect-registration"
		sessionID = "ses_reconnect_registration"
	)
	server, uri := testnats.Start(t)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, machineID)
	listener.waitHealthy(t)
	postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-1"))

	disconnected := func() int {
		return strings.Count(listener.Output.String(), "envoy nats disconnected name=envoy ")
	}
	before := disconnected()
	server.Stop(t)
	waitFor(t, 15*time.Second, "the listener to see the NATS server go", func() bool { return disconnected() > before })

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
