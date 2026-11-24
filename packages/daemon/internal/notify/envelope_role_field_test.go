package notify

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The listener answers a publish it stored or kept with the envelope it stored (publishResponse,
// packages/envoy/cmd/listener/api.go), and that envelope may carry `role`, the role a message
// Dispatch's API sent for a route to a role was sent for. publish reads such an answer by its
// status alone and decodes no envelope from it, so an answer with the field reads as the answer
// without it: a 200 is a delivered notice, and a kept publish's 202 is a kept one.
func TestPublishReadsAnEnvelopeAnswerThatCarriesTheRoleFieldAsWithoutIt(t *testing.T) {
	const topic = "notifications.role.legion-legion-legion-1-architect"
	envelope := map[string]any{
		"event_id":        "evt-role-field",
		"source":          "envoy",
		"source_event_id": "envoy.evt-role-field",
		"topic":           topic,
		"dedupe_key":      "legion-outbox:42",
		"issued_at":       1790000000000,
		"payload_summary": "pr-blocked on LEGION-2",
		"payload":         `{"kind":"pr-blocked"}`,
		"trace_id":        "trace-role-field",
		"holder":          "ses_holder",
		"holder_state":    "live",
	}
	withRole := maps.Clone(envelope)
	withRole["role"] = "legion-legion-legion-1-architect"
	for _, tc := range []struct {
		name     string
		status   int
		keep     bool
		wantKept bool
	}{
		{"a delivered publish", http.StatusOK, false, false},
		{"a delivered kept publish", http.StatusOK, true, false},
		{"a kept publish", http.StatusAccepted, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := func(answer map[string]any) (bool, error) {
				body, err := json.Marshal(answer)
				if err != nil {
					t.Fatalf("encode the answer: %v", err)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_, _ = w.Write(body)
				}))
				defer server.Close()
				publisher := New(server.URL, "")
				payload := map[string]string{"kind": "pr-blocked"}
				if tc.keep {
					return publisher.PublishKept(context.Background(), topic, "pr-blocked on LEGION-2", payload, "legion-outbox:42")
				}
				_, err = publisher.Publish(context.Background(), topic, "pr-blocked on LEGION-2", payload, "legion-outbox:42")
				return false, err
			}

			kept, err := read(envelope)
			if err != nil || kept != tc.wantKept {
				t.Fatalf("an answer without the field = kept %t, %v; want kept %t and no error", kept, err, tc.wantKept)
			}
			keptWithRole, errWithRole := read(withRole)
			if errWithRole != nil || keptWithRole != kept {
				t.Fatalf("an answer with the field = kept %t, %v; want kept %t and no error, as without it", keptWithRole, errWithRole, kept)
			}
		})
	}
}
