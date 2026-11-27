package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestADeliveryViewWhoseReaderNoLongerResolvesEnds: the hub resolves every subscriber's reader
// again on an access change, and a sub whose reader does not resolve cannot be shown to read its
// view's rows still, so it ends (dropped), and its page subscribes again, where the old reader
// would have kept it on the rows of a project its caller may have lost. alice holds a timeline;
// her reader then fails to resolve, and carol changes PUB's access list, a change the relay
// carries: alice's page is sent the ended frame.
//
// Control: the old reader kept when the re-resolve fails; no ended frame reaches alice's page.
func TestADeliveryViewWhoseReaderNoLongerResolvesEnds(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	seedHubSettings(t, seed.database)
	seedPeople(t, seed.database, contentDora)
	clock := newNoonHubClock()
	rig := hubOn(t, seed.handler, seed.database, seed.deps, clock, nil)
	conn := newFakeLiveConn()
	var failing atomic.Bool
	rig.hub.Subscribe(conn, deliverySubscription{Sub: "t", View: presetView(deliveryLiveTimeline, 7, ""), Resolve: func(ctx context.Context) (model.Actor, error) {
		if failing.Load() {
			return model.Actor{}, errors.New("the reader does not resolve")
		}
		return seed.server.readerFor(ctx, model.BrowserActor(seedAlice))
	}})
	conn.snapshot(t, "t")
	rig.waitListening(t)
	rig.settle(t)

	failing.Store(true)
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPublic+"/access/people/"+contentDora, nil, http.StatusOK)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case sent := <-conn.frames:
			if ended, ok := sent.frame.(LiveEnded); ok {
				if ended.Sub != "t" || ended.Reason != liveEndedDropped {
					sent, _ := json.Marshal(ended)
					t.Fatalf("alice's page is sent %s, want the timeline's sub ended (dropped)", sent)
				}
				if rig.hub.Holds(conn, "t") {
					t.Error("the hub still holds alice's timeline after ending it")
				}
				return
			}
		case <-deadline:
			t.Fatal("no ended frame within 10s reached alice's page, whose reader no longer resolves: her view stays on the reader it last resolved")
		}
	}
}
