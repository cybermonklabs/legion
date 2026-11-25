package bus

import (
	"testing"

	"github.com/sjawhar/envoy/notificationstream"
)

// The stream's MaxAge is the value a deployment renders a Legion daemon's
// notification_stream_max_age_hours from (notificationstream.MaxAge): a daemon held to its projects
// cannot read the stream's info, so that public constant is the only place it learns the age, and
// the two must be the one number.
func TestTheStreamKeepsMessagesForTheAgeItsPublicDoorNames(t *testing.T) {
	if streamCfg.MaxAge != notificationstream.MaxAge {
		t.Fatalf("the notification stream's MaxAge = %s, notificationstream.MaxAge = %s; want them equal", streamCfg.MaxAge, notificationstream.MaxAge)
	}
	if notificationstream.MaxAge <= 0 {
		t.Fatalf("notificationstream.MaxAge = %s, want a positive age: a stream that keeps every message is no deployment's today", notificationstream.MaxAge)
	}
}
