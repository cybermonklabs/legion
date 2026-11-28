package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
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
// The child uses short windows to reach the gone horizon promptly. The test reads the consumer
// first, then waits for the reaper's log and the consumer's removal, not for a duration. The
// bucket keeps its default maximum age, so it cannot take the record before the reaper does.
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
	postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(sessionID, "e"))
	if _, err := consumers.Info(testwait.Context(t), sessionID); err != nil {
		t.Fatalf("the consumer of %s after its registration: %v; want the stream holding it", sessionID, err)
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
