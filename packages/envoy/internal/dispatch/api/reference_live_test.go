package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestALiveFrameNamesNoObjectItsReaderCannotSee: the live socket's relay reads each event once, as
// the server, for every subscriber (eventStreamRelay.page), and a subscriber that does not see
// every object is sent the event as it reads it (sendRun, readerEvents): the fields a read fills for
// its reader counted for it, and each reference to an object it does not see left out. carol, who
// sees both projects, makes PUB's issue wait on PRIV's, and cites PUB's ask from a comment of
// PRIV's, then edits the ask. alice, who does not see PRIV, is sent the issue.updated naming no
// PRIV issue and the ask.edited counting the backlinks she reads over HTTP (none); carol is sent
// the PRIV issue and the backlink she sees, so each half of the test reads a frame that can fail.
func TestALiveFrameNamesNoObjectItsReaderCannotSee(t *testing.T) {
	seed := seedPrivateProject(t)
	url := liveURLOf(t, seed.handler)
	public, private := seed.issue[seedPublic], seed.issue[seedPrivate]
	ask := createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+public+"/asks",
		map[string]any{"question": "Which way does PUB ship?"}))
	clients := map[privateCaller]*socketClient{}
	for _, caller := range []privateCaller{callerAlice, callerCarol} {
		client := seed.dialLive(t, url, seed.liveHeader(t, caller))
		subscribeEvents(t, client, "live", nil)
		clients[caller] = client
	}

	seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+public, map[string]any{"blocked_by": []string{private}}, http.StatusOK)
	createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+private+"/comments",
		map[string]any{"body": "PRIV leans on dispatch://" + public + "/ask/" + ask + "."}))
	seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/asks/"+ask, map[string]any{"urgency": "high"}, http.StatusOK)

	askEdited := func(event liveEvent) bool {
		var payload struct {
			ID string `json:"id"`
		}
		return event.Type == "ask.edited" && json.Unmarshal(event.Payload, &payload) == nil && payload.ID == ask
	}
	for _, want := range []struct {
		caller    privateCaller
		sees      bool
		backlinks int
	}{{callerAlice, false, 0}, {callerCarol, true, 1}} {
		events := liveEventsThrough(t, clients[want.caller], "live", askEdited)
		var updated, edited *liveEvent
		for index := range events {
			switch {
			case events[index].Type == "issue.updated" && strings.Contains(string(events[index].Payload), `"key":"`+public+`"`):
				updated = &events[index]
			case askEdited(events[index]):
				edited = &events[index]
			}
		}
		if updated == nil {
			t.Fatalf("%s's live socket sent no issue.updated of %s before the ask's edit", want.caller, public)
		}
		if names := strings.Contains(string(updated.Payload), private); names != want.sees {
			t.Errorf("%s's live issue.updated of %s names %s: %t, want %t: %s", want.caller, public, private, names, want.sees, updated.Payload)
		}
		var payload struct {
			ReferencedByCount int `json:"referenced_by_count"`
		}
		if err := json.Unmarshal(edited.Payload, &payload); err != nil {
			t.Fatalf("decode %s's ask.edited: %v", want.caller, err)
		}
		read := decodeBody[struct {
			Ask struct {
				ReferencedByCount int `json:"referenced_by_count"`
			} `json:"ask"`
		}](t, seed.mustRequest(t, want.caller, http.MethodGet, "/api/v1/asks/"+ask, nil, http.StatusOK))
		if payload.ReferencedByCount != want.backlinks || read.Ask.ReferencedByCount != want.backlinks {
			t.Errorf("%s's live ask.edited counts %d backlinks and its read of the ask %d, want %d: a count counts only what its reader sees",
				want.caller, payload.ReferencedByCount, read.Ask.ReferencedByCount, want.backlinks)
		}
	}
}
