package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
)

// The read test of INFRA-3231's t08 (Decision 3 of its plan): while an agent's ask, comment,
// message and new issue are held, alice, who reads each, finds none of them in any read a person
// makes of asks, comments, messages and issues, nor on her live socket; a session's read of each
// by its id answers 409 HELD_TEXT with the held id. Once each posts, the reads show today's rows
// and the event log and the live socket carry today's events.

// heldReadsMarker is a word every held text of the read test holds and nothing else does.
const heldReadsMarker = "Zanzibar"

// personRead is one person-facing read: its path, and whether it lists the posted texts once they
// post (a read that names no held text, like a summary, is held to the first half alone).
type personRead struct {
	name, path string
	shows      bool
}

func TestAHeldTextIsInNoPersonsReadUntilItPosts(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{
		Asks: ReviewHold, Issues: ReviewHold, Comments: ReviewHold, Messages: ReviewHold, WriterWindow: time.Hour,
	}, nil)
	served := httptest.NewServer(rig.handler)
	t.Cleanup(served.Close)
	socket := openLiveSocketAt(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(served.URL, "http")+"/ws/live",
		http.Header{"X-Dispatch-User": {"alice"}}).servedBy(rig.database).read()
	subscribeEvents(t, socket, "events", nil)

	comment := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{
		"body": "Status\nThe " + heldReadsMarker + " comment. " + vacuousPhrase,
	}), "the comment")
	ask := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{
		"question": "Ship " + heldReadsMarker + "?\n" + vacuousPhrase,
		"options":  []map[string]string{{"label": "Ship it", "description": "Release it today."}, {"label": "Do not file", "description": "Wait."}},
	}), "the ask")
	message := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/messages", map[string]any{
		"body": "Status\nThe " + heldReadsMarker + " message. " + vacuousPhrase,
	}), "the message")
	issue := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Retry the " + heldReadsMarker + " sweep", "spec": "The sweep. " + vacuousPhrase,
	}), "the issue")
	held := map[string]string{"comment": comment.ID, "ask": ask.ID, "message": message.ID, "issue": issue.ID}
	for _, id := range held {
		rig.waitHeld(id, heldAwaitingWriter)
	}

	reads := []personRead{
		{"the Inbox", "/api/v1/inbox", true},
		{"the Inbox's replies", "/api/v1/me/replies", false},
		{"the Inbox's agent rows", "/api/v1/me/replies?kind=agent", false},
		{"the issue's comments", "/api/v1/issues/" + rig.issue + "/comments", true},
		{"the issue's asks", "/api/v1/issues/" + rig.issue + "/asks", true},
		{"the issue's messages", "/api/v1/issues/" + rig.issue + "/messages", true},
		{"the issue", "/api/v1/issues/" + rig.issue, true},
		{"the issue's summary", "/api/v1/issues/" + rig.issue + "/summary", false},
		{"the issue list", "/api/v1/issues?project=RVW", true},
		{"the writer's conversation", "/api/v1/agents/" + heldWriter + "/messages", false},
		{"search", "/api/v1/search?q=" + heldReadsMarker, false},
		{"the issue's references", "/api/v1/issues/" + rig.issue + "/references", false},
		{"the issue's collaborators", "/api/v1/issues/" + rig.issue + "/collaborators", false},
		{"the issue's event feed", "/api/v1/issues/" + rig.issue + "/events", true},
		{"the event log", "/api/v1/events/log", true},
	}
	for kind, id := range held {
		path := map[string]string{
			"comment": "/api/v1/comments/", "ask": "/api/v1/asks/", "message": "/api/v1/messages/", "issue": "/api/v1/issues/",
		}[kind] + id
		reads = append(reads, personRead{"the " + kind + " by its id", path, true})

		// A session reads the held text by its id as held.
		answer := agentRequest(t, rig.handler, http.MethodGet, path, nil, "agent-token")
		if answer.Code != http.StatusConflict {
			t.Fatalf("a session's read of the held %s: %d %s, want 409", kind, answer.Code, answer.Body.String())
		}
		if refusal := decodeBody[heldTextError](t, answer); refusal.Code != "HELD_TEXT" || refusal.HeldID != id {
			t.Fatalf("a session's read of the held %s answered %+v", kind, refusal)
		}
	}
	for _, read := range reads {
		response := dispatchRequest(t, rig.handler, http.MethodGet, read.path, nil, "alice")
		if response.Code != http.StatusOK && response.Code != http.StatusNotFound {
			t.Fatalf("alice's read of %s: %d %s", read.name, response.Code, response.Body.String())
		}
		body := response.Body.String()
		if strings.Contains(body, heldReadsMarker) {
			t.Fatalf("alice's read of %s shows a held text: %.300s", read.name, body)
		}
		for kind, id := range held {
			if strings.Contains(body, id) {
				t.Fatalf("alice's read of %s names the held %s %s", read.name, kind, id)
			}
		}
	}
	socket.quiet(t, time.Second, "alice's live socket while the texts are held")

	for _, id := range []string{issue.ID, comment.ID, ask.ID, message.ID} {
		posted := rig.asWriter(http.MethodPut, "/api/v1/held/"+id+"/state", map[string]any{"state": "posted", "reason": "The reader asked for it as written."})
		if posted.Code != http.StatusOK {
			t.Fatalf("the writer's post of %s: %d %s", id, posted.Code, posted.Body.String())
		}
		rig.waitHeld(id, heldPosted)
	}
	for _, read := range reads {
		if !read.shows {
			continue
		}
		pollUntil(t, 10*time.Second, read.name+" shows the posted texts", func() bool {
			response := dispatchRequest(t, rig.handler, http.MethodGet, read.path, nil, "alice")
			return response.Code == http.StatusOK && strings.Contains(response.Body.String(), heldReadsMarker)
		})
	}
	// Today's events, on the live socket too.
	for _, eventType := range []string{"comment.created", "ask.opened", "message.created", "issue.created"} {
		if count := countRows(t, rig.database, `select count(*) from events where type = $1 and payload::text like '%'||$2||'%'`, eventType, heldReadsMarker); count != 1 {
			t.Fatalf("%d %s events hold the posted text, want 1", count, eventType)
		}
	}
	seen := map[string]bool{}
	deadline := time.Now().Add(20 * time.Second)
	for len(seen) < len(held) && time.Now().Before(deadline) {
		frame := socket.event(t, 20*time.Second, "events")
		var event struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(frame.Event, &event) != nil {
			continue
		}
		for kind, id := range held {
			if strings.Contains(string(event.Payload), id) && strings.Contains(string(event.Payload), heldReadsMarker) {
				seen[kind] = true
			}
		}
	}
	if len(seen) != len(held) {
		t.Fatalf("alice's live socket carried the posts of %v, want all of %v", seen, held)
	}
}
