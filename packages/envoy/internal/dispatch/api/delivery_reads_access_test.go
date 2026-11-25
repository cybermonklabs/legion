package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAnUnmergedPullRequestsChangeMovesThePipelineView: a PR-checks run is shown when its pull
// request, the deploy repository's row of its number, is, merged or not (prChecksShown), so a change
// to a pull request that never merged can move a Pipeline view, and the live hub marks the view
// for it. alice, an open viewer, holds a Pipeline view whose PR checks count C, a run of #31,
// which no row holds yet and so names no issue. #31 is then stored unmerged, naming PRIV's issue,
// which alice does not see: the view's next frame counts no PR-checks run.
//
// Control: the hub does not mark a Pipeline view for a change to a pull request with no merge
// time (touchedBy); no frame comes, and the test fails.
func TestAnUnmergedPullRequestsChangeMovesThePipelineView(t *testing.T) {
	seed := seedPrivateProject(t)
	seedHubSettings(t, seed.database)
	clock := newNoonHubClock()
	started := clock.Now().Add(-2 * time.Hour)
	seedPipelineRun(t, seed.database.Pool, delivery.DeliveryRun{
		RunID: 731, Kind: delivery.DeliveryRunKindPRChecks, PRNumber: new(31), StartedAt: started,
		CompletedAt: new(started.Add(20 * time.Minute)), Conclusion: new(delivery.DeliveryRunConclusionSuccess),
		Event: new("pull_request"),
	}, []pipelineFixtureJob{{name: "build / unit", conclusion: "success", start: started, end: started.Add(15 * time.Minute)}})
	alice, err := seed.server.readerFor(t.Context(), model.BrowserActor(seedAlice))
	if err != nil {
		t.Fatalf("resolve what alice sees: %v", err)
	}
	rig := hubOn(t, seed.handler, seed.database, seed.deps, clock, nil)
	checks := func(what string, raw json.RawMessage) int {
		t.Helper()
		var prChecks deliveryPipelinePRChecks
		if err := json.Unmarshal(raw, &prChecks); err != nil {
			t.Fatalf("decode %s's pr_checks: %v (%s)", what, err, raw)
		}
		return prChecks.Run.Total.N
	}
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, deliverySubscription{Sub: "p", View: livePipelineView(nil), Reader: alice})
	var body struct {
		PRChecks json.RawMessage `json:"pr_checks"`
	}
	if err := json.Unmarshal(conn.snapshot(t, 5*time.Second, "p").Body, &body); err != nil {
		t.Fatalf("decode the snapshot: %v", err)
	}
	if got := checks("the snapshot", body.PRChecks); got != 1 {
		t.Fatalf("alice's Pipeline view counts %d PR-checks runs before #31 is stored, want C", got)
	}
	rig.waitListening(t)
	// The connection's own re-check (a notification may have gone by unheard while it was down)
	// reads the view again; it runs before the write below, so only the write's change can.
	rig.waitFor(t, 0, 5*time.Second, "the re-check after the listener connected", func(note deliveryHubNote) bool {
		return note.what == "readers rechecked"
	})
	rig.settle(t)

	upsertPR(t, seed.database, delivery.DeliveryPullRequest{
		Repo: "acme/widgets", Number: 31, Title: "change 31", URL: "https://github.com/acme/widgets/pull/31",
		Author: "octocat", Partial: true, IssueKey: new(seed.issue[seedPrivate]),
	})
	delta := conn.delta(t, 5*time.Second, "p")
	raw, set := delta.Set["pr_checks"]
	if !set {
		t.Fatalf("the delta after #31 was stored unmerged sets no pr_checks: %+v", delta)
	}
	if got := checks("the delta", raw); got != 0 {
		t.Errorf("after #31 was stored naming PRIV's issue, alice's Pipeline view counts %d PR-checks runs, want none", got)
	}
}
