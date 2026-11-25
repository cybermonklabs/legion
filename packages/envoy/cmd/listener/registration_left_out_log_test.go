package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/store"
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

// A registration whose session entry could not be written has stored its row without the topics
// its plan left out, and its 503 names none of them. It logs filterLeftOutLine for each all the
// same (registerSession), so a topic only an earlier build's row held does not leave the row
// without a word: the retry plans from the row this pass stored, which no longer holds it. The
// control is the row itself, read before the registration: it holds the topic, so the line is
// about a topic that pass took out.
func TestARegistrationWhoseEntryWriteFailsStillLogsTheTopicsItLeftOut(t *testing.T) {
	// Serial (listenerTestParallelism): it gives the process's one registration limiter a memory of
	// its own (registrationLeftOutLogs.isolate), which every registration of the process reads.
	const (
		leftLine  = "listener left a topic out of the session's consumer filter"
		sessionID = "ses_registration_log_entry_fails"
		leftOut   = "*.team.registration-log-entry-fails"
		kept      = "notifications.team.registration-log-entry-kept"
	)
	t.Cleanup(registrationLeftOutLogs.isolate(time.Now))
	rig := newSessionConsumerRig(t)
	// An earlier build's row: it holds a topic the session's consumer filter leaves out, which
	// this registration's body does not name.
	if _, err := rig.harness.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "test-machine"}, []string{leftOut}); err != nil {
		t.Fatalf("store the earlier build's row: %v", err)
	}
	stored, _, err := rig.harness.registry.RefreshAt(context.Background(), sessionID)
	if err != nil || !slices.Contains(stored.Topics, leftOut) {
		t.Fatalf("control: the row before the registration holds %v (%v), want %s among its topics", stored.Topics, err, leftOut)
	}
	// A stopped watcher's handle is the one swapSessionBucket swaps (its own comment).
	rig.harness.sessions.StopWatch()
	swapSessionBucket(t, rig.harness.sessions, func(kv nats.KeyValue) nats.KeyValue {
		return refusingEntryKV{KeyValue: kv, before: func() {}}
	})

	before := rig.harness.logs.Len()
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe",
		strings.NewReader(consumerRegistration(sessionID, "e", kept))))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "session registry unavailable") {
		t.Fatalf("status = %d, body = %s; want the 503 of a session entry that could not be written", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), leftOut) {
		t.Errorf("the 503 names %s: %s; this row holds that the answer names nothing and the line says it", leftOut, recorder.Body.String())
	}
	logs := rig.harness.logs.String()[before:]
	if lines := linesIn(logs, leftLine, map[string]any{"session_id": sessionID, "topic": leftOut, "from": "registration"}); len(lines) != 1 {
		t.Errorf("the registration whose entry write failed logged %d lines for %s, want one; log:\n%s", len(lines), leftOut, logs)
	}
	stored, _, err = rig.harness.registry.RefreshAt(context.Background(), sessionID)
	if err != nil || slices.Contains(stored.Topics, leftOut) || !slices.Contains(stored.Topics, kept) {
		t.Errorf("the row after the registration holds %v (%v), want %s and no %s: the pass stored the row without the topic it left out", stored.Topics, err, kept, leftOut)
	}
}
