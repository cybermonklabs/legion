package api

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// TestALegionBoardLeavesOutABlockerItsReaderCannotSee: PUB's daemon reports PUB's issue blocked
// by PRIV's issue and by a key no issue holds. carol, who sees PRIV, reads both blockers on the
// card; alice, who does not, reads the key no issue holds alone, so the card says nothing of PRIV's
// issue, and the key the daemon reported that names no issue stays as the daemon said it, which is
// what the board shows with no private project.
//
// Control: the board read keeps every blocker (leaveOutHiddenBlockers returning at once), and
// alice's card names PRIV's issue.
func TestALegionBoardLeavesOutABlockerItsReaderCannotSee(t *testing.T) {
	seed := seedPrivateProject(t)
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPublic+"/legion/daemon",
		map[string]any{"state_url": "http://legion-pub.internal:8080"}, http.StatusOK)
	blocked, hidden, nobody := seed.issue[seedPublic], seed.issue[seedPrivate], seedPublic+"-99"
	now := time.Now().UTC()
	seedLegionObservation(t, seed.database, seedPublic, blocked, legion.FieldPhase, `"admitted"`, now.Add(-time.Hour))
	seedLegionObservation(t, seed.database, seedPublic, legion.AdmissionKey, legion.FieldAdmission, fmt.Sprintf(`{
		"cap": 4, "active": [], "waiting": [%q],
		"blocked": [{"issue": %q, "blockedBy": [%q, %q]}]
	}`, blocked, blocked, hidden, nobody), now.Add(-time.Hour))
	seedLegionRead(t, seed.database, seedPublic, now.Add(-time.Minute), "")

	for _, row := range []struct {
		caller privateCaller
		want   []string
	}{
		{callerCarol, []string{hidden, nobody}},
		{callerAlice, []string{nobody}},
	} {
		board := decodeBody[legionBoardResponse](t, seed.mustRequest(t, row.caller, http.MethodGet,
			"/api/v1/projects/"+seedPublic+"/legion/board", nil, http.StatusOK))
		if got := legionCard(t, board, blocked).BlockedBy; !slices.Equal(got, row.want) {
			t.Errorf("%s reads %s's card blocked by %v, want %v", row.caller, blocked, got, row.want)
		}
	}
}
