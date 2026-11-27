package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestTheReconcilesFailureIsToldOnlyToAReaderOfTheDeployment: the delivery freshness's last_error
// is a column of the deployment's settings row, the reconcile's failure, which names the repository
// it read. A caller is told it in the timeline, the timeline's measures and the measures exactly
// when GET /api/v1/settings/delivery answers that caller the row: carol and carol's token, alice,
// open viewers, read the row and are told the failure; bob, a restricted member, and bob's token
// are refused the row (403 DEPLOYMENT_READ_REFUSED) and are answered last_error null. On the live
// socket, alice's timeline snapshot names the failure and a later failure is sent to her in a
// freshness frame; bob's snapshot names none, and he is sent no frame for it.
//
// Control: answer the row's last_error to every reader, the rule before, in derive() (bob's
// timeline, measures and snapshot name the failure) or in verdictLocked (bob is sent a freshness
// frame naming the later one); either fails the test.
func TestTheReconcilesFailureIsToldOnlyToAReaderOfTheDeployment(t *testing.T) {
	t.Parallel()
	seed := seedDeliveryReads(t, true, true)
	pool := seed.database.Pool
	failure := "the installation lacks Actions: read on acme/widgets"
	if err := delivery.RecordReconcileError(t.Context(), pool, failure); err != nil {
		t.Fatalf("record the reconcile's failure: %v", err)
	}

	shown := func(text *string) string {
		if text == nil {
			return "null"
		}
		return "\"" + *text + "\""
	}
	refusedTheRow := map[privateCaller]bool{}
	for _, caller := range []privateCaller{callerCarol, callerCarolPrivToken, callerAlice, callerBob, callerBobPrivToken} {
		settings := seed.request(t, caller, http.MethodGet, "/api/v1/settings/delivery", nil)
		switch {
		case settings.Code == http.StatusOK:
		case settings.Code == http.StatusForbidden && decodeBody[struct {
			Code string `json:"code"`
		}](t, settings).Code == codeDeploymentReadRefused:
			refusedTheRow[caller] = true
		default:
			t.Fatalf("%s: GET /api/v1/settings/delivery = %d %s, want 200 or 403 DEPLOYMENT_READ_REFUSED", caller, settings.Code, settings.Body.String())
		}
		var want *string
		if !refusedTheRow[caller] {
			want = &failure
		}
		timeline := decodeBody[deliveryTimelineWithMeasures](t, seed.mustRequest(t, caller, http.MethodGet, deliveryReadsTargets[0].target, nil, http.StatusOK))
		measures := decodeBody[deliveryMeasuresResponse](t, seed.mustRequest(t, caller, http.MethodGet, deliveryReadsTargets[1].target, nil, http.StatusOK))
		for read, got := range map[string]*string{
			"timeline":            timeline.Freshness.LastError,
			"timeline's measures": timeline.Measures.Freshness.LastError,
			"measures":            measures.Freshness.LastError,
		} {
			if !equalStringPointers(got, want) {
				t.Errorf("%s reads last_error in the %s as %s, want %s", caller, read, shown(got), shown(want))
			}
		}
	}
	// The rule must hold both ways here, or the rows above prove half of it.
	for caller, refused := range map[privateCaller]bool{callerBob: true, callerBobPrivToken: true, callerCarol: false, callerAlice: false} {
		if refusedTheRow[caller] != refused {
			t.Fatalf("%s is refused the settings row: %t, want %t", caller, refusedTheRow[caller], refused)
		}
	}

	rig := hubOn(t, seed.handler, seed.database, seed.deps, newNoonHubClock(), nil)
	reader := func(login string) model.Actor {
		t.Helper()
		resolved, err := seed.server.readerFor(t.Context(), model.BrowserActor(login))
		if err != nil {
			t.Fatalf("resolve what %s sees: %v", login, err)
		}
		return resolved
	}
	alice, bob := reader(seedAlice), reader(seedBob)
	if !readsTheDeployment(alice.Viewer) || readsTheDeployment(bob.Viewer) {
		t.Fatalf("alice reads the deployment: %t, bob: %t; want alice alone", readsTheDeployment(alice.Viewer), readsTheDeployment(bob.Viewer))
	}
	conns := map[string]*fakeLiveConn{"alice": newFakeLiveConn(), "bob": newFakeLiveConn()}
	for name, who := range map[string]struct {
		reader model.Actor
		want   *string
	}{"alice": {alice, &failure}, "bob": {bob, nil}} {
		rig.hub.Subscribe(conns[name], deliverySubscription{Sub: "t", View: presetView(deliveryLiveTimeline, 7, ""), Reader: who.reader})
		var body deliveryTimelineWithMeasures
		if err := json.Unmarshal(conns[name].snapshot(t, "t").Body, &body); err != nil {
			t.Fatalf("decode %s's snapshot: %v", name, err)
		}
		if got := body.Freshness.LastError; !equalStringPointers(got, who.want) {
			t.Errorf("%s's live snapshot carries last_error %s, want %s", name, shown(got), shown(who.want))
		}
	}
	rig.waitListening(t)

	later := "GitHub answered 502"
	if err := delivery.RecordReconcileError(t.Context(), pool, later); err != nil {
		t.Fatalf("record the next failure: %v", err)
	}
	if got := conns["alice"].freshness(t, "alice's freshness frame naming the next failure").Freshness.LastError; !equalStringPointers(got, &later) {
		t.Errorf("alice's freshness frame carries last_error %s, want %s", shown(got), shown(&later))
	}
	conns["bob"].none(t, time.Second, "the next failure, which bob is not told")
}
