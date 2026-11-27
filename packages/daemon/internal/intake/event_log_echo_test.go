package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// echoedToken is the Dispatch token the rows below read the event log with. It is made up, and it
// holds the gate's -RDX- marker, so a capture that prints it names the marker.
const echoedToken = "dsp_event-log-RDX-7f3a9c2e1b5d"

// echoedEvent is one event of LEGION-9's Dispatch event log of type typ at sequence 1, created at
// createdAt, carrying payload.
func echoedEvent(typ, createdAt string, payload map[string]any) map[string]any {
	return map[string]any{"id": 1, "issue_key": "LEGION-9", "seq": 1, "notify": true, "type": typ,
		"actor": map[string]string{"kind": "user", "id": "someone"}, "created_at": createdAt, "payload": payload}
}

// TestAnEventLogThatEchoesTheBearerPrintsItInNoDecodersOutput: a stand-in for Dispatch answers the
// read of an issue's event log (dispatch.HTTPClient.EventsAfter) with the Authorization the read
// carried written into one member of its one event: the event's created_at, which the event's
// readers once decoded as a time whose error quoted it, or a payload member the live decoder
// names in its unread list when it cannot read it (an ask's kind, a reply's turn and its ask's
// state, an issue's claim). Nothing the read and the three decoders print holds a run of the
// bearer: EventsAfter's error, the errors of LabelSetter, StatusChangeOf and DecodeDispatchEvent,
// and DecodeDispatchEvent's unread list, which the boot catch-up logs. Each row also shows that
// its member reached the reader that names it: EventsAfter refuses the created_at, and the live
// decoder names the payload member unread. The stream's decoder of an event whose created_at
// holds a bearer (decodeDispatchFact) names the member in fixed text as well. Captures
// intake.event-log-echo/<row>.
func TestAnEventLogThatEchoesTheBearerPrintsItInNoDecodersOutput(t *testing.T) {
	const at = "2026-11-23T03:00:00Z"
	for _, row := range []struct {
		name  string
		event func(echo string) map[string]any
		// refused says EventsAfter refuses the answer, naming created_at; every other row's event
		// is read, and the live decoder names its member unread.
		refused bool
	}{
		{name: "created-at", refused: true, event: func(echo string) map[string]any {
			return echoedEvent("ask.opened", echo, map[string]any{"id": "ask-1", "kind": "question"})
		}},
		{name: "ask-kind", event: func(echo string) map[string]any {
			return echoedEvent("ask.opened", at, map[string]any{"id": "ask-1", "kind": echo})
		}},
		{name: "reply-turn", event: func(echo string) map[string]any {
			return echoedEvent("comment.created", at, map[string]any{"ask_id": "ask-1", "ask_waiting_on": echo})
		}},
		{name: "reply-ask-state", event: func(echo string) map[string]any {
			return echoedEvent("comment.created", at, map[string]any{"ask_id": "ask-1", "ask_state": echo, "ask_waiting_on": "human"})
		}},
		{name: "issue-claim", event: func(echo string) map[string]any {
			return echoedEvent("issue.updated", at, map[string]any{"key": "LEGION-9", "status": "todo", "claim": echo})
		}},
		{name: "issue-claim-no-actor", event: func(echo string) map[string]any {
			return echoedEvent("issue.updated", at, map[string]any{"key": "LEGION-9", "status": "todo",
				"claim": map[string]any{"actor": map[string]string{"kind": "user"}, "note": echo}})
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode([]map[string]any{row.event(r.Header.Get("Authorization"))})
			}))
			defer server.Close()
			events, err := dispatch.New(server.URL, dispatch.BearerTransport(echoedToken, nil)).EventsAfter(context.Background(), "LEGION-9", 0)
			printed := []string{fmt.Sprint(err)}
			unread := 0
			if len(events) > 0 {
				_, labelErr := LabelSetter(events)
				printed = append(printed, fmt.Sprint(labelErr))
				for _, event := range events {
					_, changeErr := StatusChangeOf(event)
					_, named, decodeErr := DecodeDispatchEvent("LEGION", "LEGION-9", event)
					printed = append(printed, fmt.Sprint(changeErr), fmt.Sprint(decodeErr), fmt.Sprint(named))
					unread += len(named)
				}
			}
			oauth2calltest.RequireNoPiece(t, "intake.event-log-echo/"+row.name, strings.Join(printed, "\n"), echoedToken)
			switch {
			case row.refused && (err == nil || !strings.Contains(err.Error(), "created_at")):
				t.Errorf("EventsAfter = %d events, %v; want the answer refused, naming created_at", len(events), err)
			case !row.refused && (err != nil || len(events) != 1 || unread == 0):
				t.Errorf("EventsAfter = %d events, %v, and %d members unread; want the one event read and its echoed member named unread", len(events), err, unread)
			}
		})
	}
	t.Run("stream-created-at", func(t *testing.T) {
		payload, err := json.Marshal(echoedEvent("ask.opened", "Bearer "+echoedToken, map[string]any{"id": "ask-1", "kind": "question"}))
		if err != nil {
			t.Fatalf("encode the event: %v", err)
		}
		_, _, _, err = decodeDispatchFact("notifications.dispatch.issue.LEGION-9.ask.opened", "LEGION", string(payload))
		oauth2calltest.RequireNoPiece(t, "intake.event-log-echo/stream-created-at", fmt.Sprint(err), echoedToken)
		if err == nil || !strings.Contains(err.Error(), "created_at") {
			t.Errorf("the stream's decoder = %v, want the event refused, naming created_at", err)
		}
	})
}
