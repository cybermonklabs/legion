package api

import (
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestWithNoPrivateProjectTwoProjectsMadeTogetherEndNoEventsFeed: in a deployment with no private
// project an open viewer sees every object (seesEveryObject), and a new project moves nothing its
// page holds, so its events feed goes on past each project.created, as every feed did before a
// project could be private (R9). Two projects made close together are both in the database by the
// time a feed reads the first one's event: the event horizon is held while both commit, so the
// relay publishes them together. alice's feed and the shared token's are each sent both
// project.created events, then the issue made in the second project, and no end frame.
//
// Control: sightMoved passes nothing of a reader that sees every object; at the first
// project.created it sees the second project too, which that event does not explain, and each feed
// ends (behind) where the test reads an event.
func TestWithNoPrivateProjectTwoProjectsMadeTogetherEndNoEventsFeed(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	if reader, err := seed.server.readerFor(t.Context(), model.BrowserActor(seedAlice)); err != nil || !seesEveryObject(reader) {
		t.Fatalf("with PRIV public alice reads as %+v (%v), want a reader that sees every object", reader.Viewer, err)
	}
	url := liveURLOf(t, seed.handler)
	callers := []privateCaller{callerAlice, callerSharedToken}
	clients := make([]*socketClient, len(callers))
	for i, caller := range callers {
		clients[i] = seed.dialLive(t, url, seed.liveHeader(t, caller))
		subscribeEvents(t, clients[i], "feed", nil)
	}

	release := holdClusterTransactionID(t)
	made := []string{"NEWA", "NEWB"}
	for _, key := range made {
		seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/projects", map[string]string{"key": key, "name": "Project " + key}, http.StatusCreated)
	}
	release()
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]string{"project": "NEWB", "title": "The issue of NEWB"}, http.StatusCreated)

	for i, client := range clients {
		read := liveEventsThrough(t, client, "feed", func(event liveEvent) bool { return event.Type == "issue.created" && event.Project == "NEWB" })
		var created []string
		for _, event := range read {
			if event.Type == projectCreatedEvent {
				created = append(created, event.Project)
			}
		}
		if !slices.Equal(created, made) {
			t.Errorf("%s's feed was sent project.created of %v before NEWB's issue, want %v", callers[i], created, made)
		}
	}
}
