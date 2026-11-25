package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestWithNoPrivateProjectANewProjectReSnapshotsNoDeliveryOrLegionPage: in a deployment with no
// private project every open viewer sees every object (seesEveryObject) and reads the same rows, so
// a new project moves no view such a viewer holds: its page keeps the view and is sent only what
// changed, as every page was before a project could be private (R9). alice holds a timeline and
// PUB's Legion board; carol makes project NEW and a P0 issue in it that nobody owns. alice's pages
// are sent no snapshot, and the timeline's delta lists NEW's issue among the unowned P0 issues: it
// is read for the reader alice is now, who sees NEW.
//
// Controls: the view key holds the projects a reader that sees every object sees; the new project
// moves both subs to views of their own, each answered a snapshot, and the test fails at the first.
// The view keeps the reader it was made for; NEW's issue never reaches the timeline, and the test
// fails at its deadline.
func TestWithNoPrivateProjectANewProjectReSnapshotsNoDeliveryOrLegionPage(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPublic+"/legion/daemon",
		map[string]any{"state_url": "http://legion-pub.internal:8080"}, http.StatusOK)
	seedHubSettings(t, seed.database)
	clock := newNoonHubClock()
	seedLegionRead(t, seed.database, seedPublic, clock.Now().Add(-time.Minute), "")
	resolve := func(ctx context.Context) (model.Actor, error) {
		return seed.server.readerFor(ctx, model.BrowserActor(seedAlice))
	}
	if reader, err := resolve(t.Context()); err != nil || !seesEveryObject(reader) {
		t.Fatalf("with PRIV public alice reads as %+v (%v), want a reader that sees every object", reader.Viewer, err)
	}
	rig := hubOn(t, seed.handler, seed.database, seed.deps, clock, nil)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, deliverySubscription{Sub: "t", View: presetView(deliveryLiveTimeline, 7, ""), Resolve: resolve})
	rig.hub.Subscribe(conn, deliverySubscription{Sub: "l", View: legionView(seedPublic), Resolve: resolve})
	held := conn.firstSnapshots(t, "t", "l")["t"].Body
	rig.waitListening(t)
	rig.waitFor(t, 0, "the re-check after the listener connected", func(note deliveryHubNote) bool {
		return note.what == "readers rechecked"
	})
	rig.settle(t)

	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/projects", map[string]string{"key": "NEW", "name": "Project NEW"}, http.StatusCreated)
	created := decodeBody[struct {
		Key string `json:"key"`
	}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "NEW", "title": "NEW is down", "priority": 0}, http.StatusCreated)).Key

	deadline := testwait.Context(t).Done()
	for {
		select {
		case sent := <-conn.frames:
			switch frame := sent.frame.(type) {
			case DeliveryLiveSnapshot:
				raw, _ := json.Marshal(frame)
				t.Fatalf("a new project sent alice's page %s a snapshot, which a deployment with no private project never sends it: %.300s", frame.Sub, raw)
			case DeliveryLiveDelta:
				if frame.Sub != "t" {
					continue
				}
				held = applyLiveDelta(t, deliveryLiveTimeline, held, frame)
				var timeline struct {
					Measures struct {
						UnownedP0 []struct {
							Key string `json:"key"`
						} `json:"unowned_p0"`
					} `json:"measures"`
				}
				if err := json.Unmarshal(held, &timeline); err != nil {
					t.Fatalf("decode the timeline alice's page holds: %v", err)
				}
				for _, issue := range timeline.Measures.UnownedP0 {
					if issue.Key == created {
						return
					}
				}
			}
		case <-deadline:
			t.Fatalf("no delta before the test binary's deadline listed %s, NEW's unowned P0 issue, on alice's timeline", created)
		}
	}
}
