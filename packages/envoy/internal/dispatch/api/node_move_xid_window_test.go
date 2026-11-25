//go:build !race

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The phases of a write of the node routes the xid window test tells apart, each by the
// statements of its committed transaction (nodeRoutePhaseOf): a move whose text Markdown cannot
// show, which writes its tuples alone; a move whose text moves, which writes a version of its
// document under the room's lock in the same transaction (R7); and the drain of the queries the
// move marked owed, which writes their versions in a transaction of its own once the move has
// committed (docs.Service.DrainOwedQueries).
const (
	phaseFlatMove    = "flat-move"
	phaseVersionMove = "move-version"
	phaseDrain       = "drain"
)

// nodeRoutePhaseOf is the phase of the committed transaction window: the drain's clears the
// issue's query mark, a move that writes a version inserts the version, and a flat move does
// neither.
func nodeRoutePhaseOf(window storetest.XidWindow) string {
	var sql strings.Builder
	for _, statement := range window.Statements {
		sql.WriteString(strings.ToLower(statement.SQL))
		sql.WriteByte('\n')
	}
	switch text := sql.String(); {
	case strings.Contains(text, "delete from issue_query_marks"):
		return phaseDrain
	case strings.Contains(text, "insert into artifact_versions"):
		return phaseVersionMove
	}
	return phaseFlatMove
}

// TestXidWindowsOfTheNodeRoutesTheDrainAndTheFill measures, on the large spec of the xid fixture
// with a criterion and a requirement after it, the windows of the node routes' writes that change a
// query and of the drain each runs (R7, c-windows-kept): a flat move, the requirement moved under a
// lens on the issue, which the requirement's parent attribute cannot name, so its text and its
// document stay as they are while the spec query's record moves; and a move that writes a version,
// the requirement moved under the criterion, whose parent attribute the text then gains under the
// room's lock. Each request's committed transactions are told apart by their statements
// (nodeRoutePhaseOf), each named with its operation, its phase and its window, and each held to
// storetest.XidBatchedWindow: its reads, then one batch and its COMMIT. Each step requires the
// phases it is meant to show, its move's and the drain's, each in one window and no other window:
// a move that wrote its version in a transaction of its own after its COMMIT would show two
// windows for one move.
func TestXidWindowsOfTheNodeRoutesTheDrainAndTheFill(t *testing.T) {
	// Not parallel: the xid rig measures only in a test that runs alone in the process.
	large, _ := xidSpec(1)
	f := newXidFixture(t, testServerOptions{}, large+"\n:::criterion{#k1}\nThe store answers in time.\n:::\n\n"+
		":::requirement{#m1}\nEvery read answers in a second.\n:::\n")
	lens := decodeBody[lensInvoked](t, created(t, "invoke a lens on the issue", dispatchRequest(t, f.handler, http.MethodPost,
		"/api/v1/issues/"+f.issueKey+"/lenses", map[string]any{"on": "dispatch://" + f.issueKey, "kind": "trade-off"}, "alice")))
	requirement := blockAddressOf(t, f.database, f.issueKey, "m1")
	criterion := blockAddressOf(t, f.database, f.issueKey, "k1")
	for _, step := range []struct {
		name, parent string
		phases       []string
	}{
		{"flat move", graphstore.KindLens + "/" + lens.ID, []string{phaseFlatMove, phaseDrain}},
		{"move that writes a version", criterion, []string{phaseVersionMove, phaseDrain}},
	} {
		t.Run(step.name, func(t *testing.T) {
			awaitQuietDatabase(t, f.database, false)
			_, tag := placementOf(t, f.database, "block", requirement)
			report := f.measure(t, http.StatusOK, storetest.XidBatchedWindow, func(h http.Handler) *httptest.ResponseRecorder {
				return patchNode(t, h, f.issueKey, requirement, tag, map[string]any{"parent": step.parent}, "alice")
			})
			seen := map[string]int{}
			var phases []string
			for _, window := range report.Windows {
				phase := nodeRoutePhaseOf(window)
				seen[phase]++
				phases = append(phases, phase)
				t.Logf("PATCH /api/v1/issues/{key}/nodes/{id}, %s: phase %s: %s", step.name, phase, window.Summary())
			}
			for _, phase := range step.phases {
				if seen[phase] == 0 {
					t.Errorf("PATCH /api/v1/issues/{key}/nodes/{id}, %s: phase %s not observed; the request's phases were %v", step.name, phase, phases)
				}
			}
			if moves := seen[phaseFlatMove] + seen[phaseVersionMove]; moves != 1 || len(phases) != len(step.phases) {
				t.Errorf("PATCH /api/v1/issues/{key}/nodes/{id}, %s: the phase %s with %d windows for one move; the request's phases were %v, want %v",
					step.name, step.phases[0], moves, phases, step.phases)
			}
		})
	}
}
