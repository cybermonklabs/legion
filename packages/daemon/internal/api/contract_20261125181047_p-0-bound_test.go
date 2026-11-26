package api

import (
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// The P0 start's rows of this package's test tables (entryTestRows): the bodies of the
// controller's two P0 bound routes, which the `legion` tool sends, and the fixture of the answer
// both routes give, AnswerP0Bound. The operator's two routes are the `legion` command's
// (routesWithNoPluginBody). The entry adds no state body (states) and carries nothing into
// narrowingState (carry): the keys it adds to the state are in the state golden itself
// (populatedState), because state.json, which the TypeScript mirror's tests and pi-legion's
// read_state rows parse, is written from that state alone, and narrowingState is built on it, so
// both of the entry's bands find their keys there and write their lines.
func init() {
	registerEntryTestRows(entryTestRows{
		entry: p0BoundContract,
		requests: map[string]reflect.Type{
			"POST /legion/v1/project/p0-bound":       reflect.TypeFor[ProjectP0BoundRequest](),
			"POST /legion/v1/project/p0-bound/clear": reflect.TypeFor[ProjectP0BoundClearRequest](),
		},
		answers: p0BoundAnswerFixtures,
	})
}

// p0BoundAnswerFixtures is the fixture of AnswerP0Bound: a set of the P0 bound by the controller,
// with the bound in force, legion.yaml's and the live value with who set it, when and why, every
// key the answer can carry. A clear answers no live value, and a set of none a null bound.
func p0BoundAnswerFixtures(t *testing.T) map[AnswerID]answerFixture {
	t.Helper()
	return map[AnswerID]answerFixture{
		AnswerP0Bound: {roots: typesOf(P0BoundView{}), bodies: []any{P0BoundView{Bound: new(2), Configured: new(4),
			Live: &P0BoundLiveView{Bound: new(2), SetBy: PauseActorView{Kind: "controller", Session: "ses_controller"},
				SetAt: time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC), Reason: "the P0 issues outran the gateway"}}}},
	}
}

// TestACallerInNoPodStatingNoContractIsServedTheP0Holders: Dispatch's sampler reads the state
// stating no plugin contract, from an address no claim's pod names, so contractsFor places it at
// this daemon's own body, which carries `admission.p0Holders`, the number the Legion board's
// header copies (dispatch://INFRA-5636; Dispatch's own row,
// TestTheStateReadStatesNoContractAndKeepsP0Holders, pins that its read states none). The caller
// of the pod whose claims record a contract below this entry, asking the same daemon, is the
// control: it is served no number, so the claims could strip the key, and the caller in no pod is
// served it by its placement alone. A change that strips the key for such a caller fails here.
func TestACallerInNoPodStatingNoContractIsServedTheP0Holders(t *testing.T) {
	state := populatedState(t)
	if state.Admission.P0Holders == nil {
		t.Fatal("the populated state holds no number of P0 holders, so this test proves nothing")
	}
	handler := stateServer(t, state, podClaims(ServingFloor))
	admissionServed := func(remote string) map[string]json.RawMessage {
		t.Helper()
		var body struct {
			Admission map[string]json.RawMessage `json:"admission"`
		}
		if err := json.Unmarshal(stateAsked(t, handler, remote), &body); err != nil {
			t.Fatalf("decode the state served %s: %v", remote, err)
		}
		return body.Admission
	}

	if _, carried := admissionServed(statePodAddr + ":41000")["p0Holders"]; carried {
		t.Fatalf("the caller of a pod at contract %d is served admission.p0Holders, want none below the P0 start's entry: this test proves nothing", ServingFloor)
	}
	raw, carried := admissionServed("192.0.2.1:41000")["p0Holders"]
	if !carried {
		t.Fatal("a caller in no pod stating no contract is served no admission.p0Holders, want this daemon's own body, which carries it")
	}
	if want := strconv.Itoa(*state.Admission.P0Holders); string(raw) != want {
		t.Errorf("a caller in no pod stating no contract is served admission.p0Holders %s, want the state's %s", raw, want)
	}
}
