package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The session reaper a started listener runs deletes the consumer of every record it reaps. A
// pass deletes no consumer of its own: it deletes the one the SessionReaper it was given names
// (session.SessionReaper.DeleteConsumer), and main is the only place the listener's own session
// consumers are handed to it, so a pass built without that deleter reaps records and leaves their
// consumers on the stream until the inactive threshold takes them hours later. Nothing a test
// builds its own reaper in can see that: this one starts the real binary, registers a session
// through its /v1 as a plugin's heartbeat does, and leaves that session to reach the gone
// horizon.
//
// The registration reports consumer mode and its start only after it creates the consumer.
// That answer proves the consumer existed even if the child reaps it before the test next runs.
// The short windows only shorten the wait for the reaper. The bucket keeps its default maximum
// age, so it cannot delete the record before the reaper does.
func TestTheSessionReaperAStartedListenerRunsDeletesTheReapedSessionsConsumer(t *testing.T) {
	t.Parallel()
	_, uri := testnats.Start(t)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "session-consumer-reap-wiring",
		"ENVOY_SESSION_LIVE_WINDOW=100ms",
		"ENVOY_SESSION_GONE_HORIZON=700ms",
		"ENVOY_SESSION_REAP_INTERVAL=50ms",
	)
	listener.waitHealthy(t)
	// The test's own view of the session consumers, so what it reads of them is what the server
	// holds and not what the listener believes.
	consumers, err := bus.ConnectSessionConsumers([]string{uri}, 0, nil)
	if err != nil {
		t.Fatalf("connect the test's view of the session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)

	const sessionID = "ses_reap_wiring"
	status, body := callListener(t, listener.port, http.MethodPost, "/v1/interests/subscribe", consumerRegistration(sessionID, "e"))
	if status != http.StatusOK {
		t.Fatalf("register %s: status %d: %s", sessionID, status, body)
	}
	var answer subscribeAnswer
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("decode the registration of %s: %v: %s", sessionID, err, body)
	}
	if answer.Delivery == nil || answer.Delivery.Mode != deliveryModeConsumer || answer.Delivery.ConsumerStart == 0 {
		t.Fatalf("the registration of %s did not report a consumer and its start: %s", sessionID, body)
	}

	// The record delete is the pass's own work and is logged by the pass itself, so a consumer
	// still on the stream after that line is a deleter the reaper was never given, not a pass
	// that never ran.
	listener.WaitForOutput(t, `"msg":"session reaper deleted the record of a session that reached the gone horizon with no clean stop"`)
	if output := listener.Output.String(); !strings.Contains(output, `"session_id":"`+sessionID+`"`) {
		t.Fatalf("the pass the listener ran names no record of %s:\n%s", sessionID, output)
	}
	awaitConsumerGone(t, consumers, sessionID, "after the reaper deleted its record")
}
