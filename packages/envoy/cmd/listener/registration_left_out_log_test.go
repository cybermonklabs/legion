package main

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A registration that leaves a topic out of the session's consumer filter names it in its answer
// every time, and logs filterLeftOutLine for it once per session and topic within
// registrationLeftOutLogWindow: a plugin sends the topics it follows again on every heartbeat, so
// a line per registration would repeat every 90 s for as long as the session runs. The window keys
// on both: a second topic of the same session and a second session on the same topic each get a
// line of their own inside it, and the first pair gets one again once it has passed, read on the
// limiter's clock moved past it. The test isolates the limiter (leftOutLogLimiter.isolate), so no
// entry an earlier run stored, at a time that clock read ahead of the wall clock, suppresses this
// run's first lines.
func TestARegistrationLogsALeftOutTopicOncePerSessionAndTopicAnHour(t *testing.T) {
	// Not parallel: it swaps the clock and memory of registrationLeftOutLogs, the one limiter
	// of every registration in the process (leftOutLogLimiter.isolate), and asserts on the
	// lines that limiter lets through.
	const (
		leftLine = "listener left a topic out of the session's consumer filter"
		first    = "ses_registration_log_first"
		second   = "ses_registration_log_second"
		topic    = "*.team.registration-log-a"
		other    = "*.team.registration-log-b"
		kept     = "notifications.team.registration-log-kept"
	)
	var offset atomic.Int64
	t.Cleanup(registrationLeftOutLogs.isolate(func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }))
	rig := newSessionConsumerRig(t)
	// register registers sessionID on topics, fails unless the answer names each of named as not
	// registered, and answers the registration lines logged meanwhile for sessionID and each topic.
	register := func(t *testing.T, sessionID string, named []string, topics ...string) map[string]int {
		t.Helper()
		before := rig.harness.logs.Len()
		var answer interestSubscribeAnswer
		if err := json.Unmarshal([]byte(rig.post(t, "/v1/interests/subscribe", consumerRegistration(sessionID, "e", topics...))), &answer); err != nil {
			t.Fatalf("decode the registration's answer: %v", err)
		}
		for _, topic := range named {
			if !strings.Contains(strings.Join(answer.Warnings, "\n"), topic+" was not registered: ") {
				t.Errorf("the answer to %s's registration %q does not name %s", sessionID, answer.Warnings, topic)
			}
		}
		logs := rig.harness.logs.String()[before:]
		lines := map[string]int{}
		for _, topic := range []string{topic, other} {
			lines[topic] = len(linesIn(logs, leftLine, map[string]any{"session_id": sessionID, "topic": topic, "from": "registration"}))
		}
		return lines
	}

	logged := 0
	for range 3 {
		logged += register(t, first, []string{topic}, topic, kept)[topic]
	}
	if logged != 1 {
		t.Errorf("three registrations of %s naming %s logged %d lines, want one", first, topic, logged)
	}
	if got := register(t, first, []string{topic, other}, topic, other, kept); got[other] != 1 || got[topic] != 0 {
		t.Errorf("a second topic of %s inside the window logged %v, want one line for %s and none more for %s", first, got, other, topic)
	}
	if got := register(t, second, []string{topic}, topic, kept); got[topic] != 1 {
		t.Errorf("a second session on %s inside the window logged %d lines, want its own one", topic, got[topic])
	}
	offset.Store(int64(registrationLeftOutLogWindow + time.Second))
	if got := register(t, first, []string{topic}, topic, kept); got[topic] != 1 {
		t.Errorf("past the window, %s's %s logged %d lines, want one again", first, topic, got[topic])
	}
}
