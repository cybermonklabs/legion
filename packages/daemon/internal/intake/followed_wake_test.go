package intake

import (
	"strconv"
	"testing"
	"time"
)

// The listener's report of a message on a topic a released session follows decodes into a wake
// naming that session, as its report of a message sent to the session's agent subject does. The
// kind is written as the listener writes it on the wire.
func TestTheListenersFollowedTopicReportDecodesIntoAWakeNamingTheSession(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	payload := `{"kind":"followed_topic","session":"session-1","topic":"notifications.team.x","at":` + strconv.FormatInt(at.UnixMilli(), 10) + `}`
	decoded, err := decodeMessage(legionWakeSubject, "CAPTURE", capturedRepositories, envelopeJSON(t, "wake-1", "envoy", payload))
	if err != nil {
		t.Fatalf("decode the followed-topic report: %v", err)
	}
	wake, ok := decoded.Fact.(ListenerWake)
	if !ok {
		t.Fatalf("decoded fact = %#v, want a listener wake", decoded.Fact)
	}
	if wake.Kind != "followed_topic" || wake.Session != "session-1" || !wake.At.Equal(at) || wake.Report != "wake-1" {
		t.Fatalf("decoded wake = %#v, want the followed-topic wake of session-1 at %s under report wake-1", wake, at)
	}
}
