package outbox

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// What an event's envelope says about a reply. No event type says anything: an answered ask and
// an approved artifact are receipts a reader has nothing to send back to, and they would say so
// out loud were it not for the listeners built before #2903, which read expects_reply only as a
// string and drop a frame carrying the literal false (see envelope() in publisher.go for what
// removes that). Every other event type has never said anything either, an ask event the same
// branch gives an urgency included.
//
// What is read here is the marshalled envelope, because that is what the bus carries. And each
// envelope is validated as resolveDestinations validates it before publishing, because an
// envelope Validate() refuses is backed off forever and holds every later event of its owner
// behind it.
func TestNoEventEnvelopeSaysAnythingAboutAReply(t *testing.T) {
	const ask = "00000000-0000-0000-0000-000000000030"
	for _, tc := range []struct {
		name  string
		event model.Event
		// want is the envelope's expects_reply as JSON, and "" is the absent field.
		want string
	}{
		{
			name: "an answered ask, a receipt that stays silent",
			event: model.Event{
				ID: 1, IssueKey: new("T-1"), Type: "ask.answered", CreatedAt: time.Now(),
				Payload: map[string]any{"id": ask, "answer": "Approve", "urgency": "high"},
			},
		},
		{
			name: "an approved artifact, the other one",
			event: model.Event{
				ID: 2, IssueKey: new("T-1"), Type: "artifact.approved", CreatedAt: time.Now(),
				Payload: map[string]any{
					"artifact_id": "00000000-0000-0000-0000-000000000040",
					"name":        "spec.md", "version": 3, "ask_id": ask,
				},
			},
		},
		{
			name: "an opened ask, which wants an answer the envelope does not name",
			event: model.Event{
				ID: 3, IssueKey: new("T-1"), Type: "ask.opened", CreatedAt: time.Now(),
				Payload: map[string]any{"id": ask, "question": "Which approach?", "urgency": "high"},
			},
		},
		{
			name: "a comment",
			event: model.Event{
				ID: 4, IssueKey: new("T-1"), Type: "comment.created", CreatedAt: time.Now(),
				Payload: map[string]any{"id": "comment-9", "body": "Slice 3 is pushed."},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item, err := envelope(tc.event, "")
			if err != nil {
				t.Fatalf("envelope: %v", err)
			}
			if err := item.Validate(); err != nil {
				t.Fatalf("validate envelope: %v", err)
			}
			raw, err := json.Marshal(item)
			if err != nil {
				t.Fatalf("marshal envelope: %v", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatalf("decode envelope %s: %v", raw, err)
			}
			got := string(fields["expects_reply"])
			if _, present := fields["expects_reply"]; !present {
				got = ""
			}
			if got != tc.want {
				t.Fatalf("expects_reply of %s = %q, want %q", tc.event.Type, got, tc.want)
			}
		})
	}
}
