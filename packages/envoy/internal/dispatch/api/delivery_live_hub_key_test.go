package api

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestADeliveryViewIsSharedOnlyByReadersOfTheSameRows: two readers share a delivery view only when
// they read the same rows. A view's session titles are read under the event rule
// (delivery.ResolveSessionTitles under readableEvents), whose party and own-session arms read the
// viewer's person and session. So once a project is private, two readers alike in all, open and
// sees, but not in person or session, each get a view of their own: one may read a session's title
// from an event the other may not. Where no project is private every open viewer reads every event,
// and all of them share the one view, as before (R9).
//
// Control: a key of all, open and sees alone; bob and dave share a view, and the test fails.
func TestADeliveryViewIsSharedOnlyByReadersOfTheSameRows(t *testing.T) {
	t.Parallel()
	reader := func(person, session string, anyPrivate bool) model.Actor {
		return model.Actor{Viewer: &model.Viewer{Person: person, Session: session, Sees: []string{"PRIV", "PUB"}, AnyPrivate: anyPrivate}}
	}
	if deliveryReaderKey(reader("bob@example.com", "", true)) == deliveryReaderKey(reader("dave@example.com", "", true)) {
		t.Error("bob and dave, restricted members of the same projects, share a delivery view: a session title one may read reaches the other")
	}
	if deliveryReaderKey(reader("bob@example.com", "s1", true)) == deliveryReaderKey(reader("bob@example.com", "s2", true)) {
		t.Error("two sessions of bob share a delivery view: a title its own session's event names reaches the other")
	}
	open := func(person string) model.Actor {
		return model.Actor{Viewer: &model.Viewer{Person: person, Open: true, Sees: []string{"PUB"}}}
	}
	if deliveryReaderKey(open("alice@example.com")) != deliveryReaderKey(open("carol@example.com")) {
		t.Error("two open viewers of a deployment with no private project read the same rows, and get views of their own")
	}
}
