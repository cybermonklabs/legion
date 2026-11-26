package api

import (
	"reflect"
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
