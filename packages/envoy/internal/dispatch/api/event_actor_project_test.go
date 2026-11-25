package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestAStoredActorsProjectIsNotAnswered: servers before private projects wrote `project` on the
// actor of a write a named broker token made, and their stored events keep it. This server's
// model.Actor has no such field, so every route that reads a stored event decodes its actor into
// model.Actor and answers it without `project`: the event log, and an owner's event feed (an
// issue's here; a document's feed is the same read, readEvents). After this server is deployed,
// only a reader talking to an older server (version skew) can be handed an actor `project`, and
// the contract's ActorSchema keeps the field optional for that case alone.
func TestAStoredActorsProjectIsNotAnswered(t *testing.T) {
	t.Parallel()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "CORE", "An older server's write", "A spec")
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `
		with next as (update issues set last_seq = last_seq + 1 where key = $1 returning last_seq)
		insert into events (issue_key, seq, type, actor, payload, notify)
		select $1, last_seq, 'message.created',
		       '{"kind":"session","id":"session-older-server","project":"CORE"}'::jsonb, '{}'::jsonb, false
		from next
		returning id
	`, issue.Key).Scan(&id); err != nil {
		t.Fatalf("store an event whose actor carries project: %v", err)
	}
	// The event log answers only the events at or below the safe horizon, which a transaction
	// elsewhere on the test cluster can hold below the stored event (awaitHorizonRead).
	safeHorizon(t, served)

	actorOf := func(t *testing.T, route string, events []json.RawMessage) map[string]any {
		t.Helper()
		for _, raw := range events {
			var event struct {
				ID    int64          `json:"id"`
				Actor map[string]any `json:"actor"`
			}
			if err := json.Unmarshal(raw, &event); err != nil {
				t.Fatalf("%s: decode %s: %v", route, raw, err)
			}
			if event.ID == id {
				return event.Actor
			}
		}
		t.Fatalf("%s answered no event %d", route, id)
		return nil
	}
	for _, route := range []string{"/api/v1/events/log", "/api/v1/issues/" + issue.Key + "/events"} {
		response := dispatchRequest(t, handler, http.MethodGet, route, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%.400s", route, response.Code, response.Body.String())
		}
		actor := actorOf(t, route, decodeBody[[]json.RawMessage](t, response))
		if actor["id"] != "session-older-server" {
			t.Fatalf("GET %s: the stored event's actor is %v, want the session that wrote it", route, actor)
		}
		if project, ok := actor["project"]; ok {
			t.Errorf("GET %s answers the stored actor's project %v; this server's Actor has no such field", route, project)
		}
	}
}
