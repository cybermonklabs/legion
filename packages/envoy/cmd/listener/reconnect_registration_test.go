package main

import (
	"net/http"
	"strings"
	"testing"
)

// A registration the listener answers while its NATS connection is reconnecting fails by name, and
// inside the client's own deadline (R14 of dispatch://INFRA-3589): the plugin's envoy-client gives
// one attempt contracts.ListenerCallTimeout (LISTENER_CALL_TIMEOUT_MS, 5 s), and an answer that
// comes later reaches nobody. The child here runs a short route budget (shortBudgetListenerBinary)
// so the row waits for no 3.5 s; it decides on the child's named 503 and its own line, and
// TestReconnectRouteBudgetsFitTheClientAttempt holds the production budget inside the client's
// attempt. The leader read of principal admission is what meets the reconnect, before any consumer
// call.
func TestARegistrationThroughANATSReconnectFailsByName(t *testing.T) {
	t.Parallel()
	const (
		machineID = "reconnect-registration"
		sessionID = "ses_reconnect_registration"
	)
	server, listener := startShortBudgetReconnectFixture(t, machineID, consumerRegistration(sessionID, "epoch-1"))
	stopReconnectServer(t, server, listener)

	from := len(listener.Output.String())
	answer := callReconnectRoute(t, listener, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-2"))
	t.Logf("the registration during the outage answered %s", answer)
	if answer.err != nil || answer.status != http.StatusServiceUnavailable ||
		!strings.Contains(answer.body, "read the known-session record of "+sessionID) ||
		!strings.Contains(answer.body, reconnectingReason) {
		t.Fatalf("the registration during the outage answered %s, want 503 naming the principal admission read and %q", answer, reconnectingReason)
	}
	line := waitChildRecord(t, listener, from, "listener refused a registration whose principal it could not record first", "session_id", sessionID)
	if !strings.Contains(line, reconnectingReason) {
		t.Fatalf("the child's registration log = %s, want it to name %q", line, reconnectingReason)
	}
	t.Log(line)
	if strings.Contains(listener.Output.String(), reconnectBufferRefusal) {
		t.Errorf("the listener logged %q during the outage, want the reconnect named instead", reconnectBufferRefusal)
	}
}
