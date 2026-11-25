package api

import (
	"strings"
	"testing"
	"time"
)

// TestAnIssuesLegionLiveViewAloneRefusesAnIssueThatIsNotAString holds, where the socket decodes a
// frame (decodeSocketClientFrame), the rule that no frame an earlier build subscribed becomes
// INVALID_FRAME for the `issue` member the legion_issue kind reads
// (dispatch://INFRA-4869/comment/af35ce01-6eff-48d6-8723-c066c44550b0). An events view and a
// pipeline view whose issue is a string, a number or an object subscribe as the same view with
// none, each answered its first frame (the feed's events_start, the snapshot), and a legion_issue
// view, the one kind that reads the member, refuses an issue that is not a string, INVALID_FRAME
// in its own words rather than the decode's. Every frame is the JSON text a page sends, so what
// the test holds is the decode of that text, whatever Go type the struct gives the member; and
// each is sent on a socket of its own, so no frame of one view's answer stands in front of the
// next view's first.
func TestAnIssuesLegionLiveViewAloneRefusesAnIssueThatIsNotAString(t *testing.T) {
	t.Parallel()
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	// A pipeline view is answered a snapshot only where delivery is configured.
	seedHubSettings(t, rig.database)
	subscribe := func(t *testing.T, sub, view string) *socketClient {
		t.Helper()
		client := rig.dial(t, asUser("alice"))
		client.sendText(t, []byte(`{"type":"subscribe","sub":"`+sub+`","view":`+view+`}`))
		return client
	}
	for _, issue := range []struct{ what, member string }{
		{"no issue", ``},
		{"a string issue", `,"issue":"INFRA-7"`},
		{"the issue 7", `,"issue":7`},
		{"an issue that is an object", `,"issue":{"key":"INFRA-7"}`},
	} {
		t.Run("an events view with "+issue.what+" starts the feed", func(t *testing.T) {
			eventsStart(t, subscribe(t, "e", `{"kind":"events"`+issue.member+`}`), "e")
		})
		t.Run("a pipeline view with "+issue.what+" is answered its snapshot", func(t *testing.T) {
			subscribe(t, "p", `{"kind":"pipeline"`+issue.member+`}`).snapshot(t, 10*time.Second, "p")
		})
	}
	for _, issue := range []struct{ what, member string }{
		{"the issue 7", `7`},
		{"an issue that is an object", `{"key":"INFRA-7"}`},
	} {
		t.Run("a legion_issue view of "+issue.what+" is refused in its own words", func(t *testing.T) {
			refusal := subscribe(t, "i", `{"kind":"legion_issue","issue":`+issue.member+`}`).refusal(t, 5*time.Second, "i", "INVALID_FRAME")
			if !strings.HasPrefix(refusal.Message, "a legion_issue view") {
				t.Errorf("the refusal says %q, want the legion_issue kind's own words, not the decode's", refusal.Message)
			}
		})
	}
}
