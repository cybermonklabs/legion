package api

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// TestOneIssuesLegionStateNamesNoBlockerOrWaitingRootItsReaderCannotSee: GET
// /api/v1/issues/{key}/legion of PUB's issue, which PUB's daemon reports waiting in the line behind
// PRIV's issue and blocked by PRIV's issue and by a key no issue holds. A blocker and a waiting root
// may be of another project (main's #4896 lets a blocked_by link cross one), so the one issue's read
// names and counts them as the board's card does (TestALegionBoardLeavesOutABlockerItsReaderCannotSee):
// carol, who sees PRIV, reads PRIV's issue as its blocker and her place second of two; alice, who
// does not, reads no blocker and her place first of one. The key no issue holds is left out for both.
// With PRIV public, alice reads what carol reads (R9).
//
// Control: readLegionIssue without leaveOutHiddenAdmission (the admission block as the daemon wrote
// it), and alice reads PRIV's issue and the key nothing holds as blockers, second of two in the line.
func TestOneIssuesLegionStateNamesNoBlockerOrWaitingRootItsReaderCannotSee(t *testing.T) {
	seed := seedPrivateProject(t)
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPublic+"/legion/daemon",
		map[string]any{"state_url": "http://legion-pub.internal:8080"}, http.StatusOK)
	blocked, hidden, nobody := seed.issue[seedPublic], seed.issue[seedPrivate], seedPublic+"-99"
	now := time.Now().UTC()
	seedLegionObservation(t, seed.database, seedPublic, blocked, legion.FieldPhase, `"admitted"`, now.Add(-time.Hour))
	seedLegionObservation(t, seed.database, seedPublic, legion.AdmissionKey, legion.FieldAdmission, fmt.Sprintf(`{
		"cap": 4, "active": [], "waiting": [%q, %q],
		"blocked": [{"issue": %q, "blockedBy": [%q, %q]}]
	}`, hidden, blocked, blocked, hidden, nobody), now.Add(-time.Hour))
	seedLegionRead(t, seed.database, seedPublic, now.Add(-time.Minute), "")

	type want struct {
		blockedBy     []string
		place, length int
	}
	read := func(caller privateCaller) want {
		t.Helper()
		answer := decodeBody[wireLegionIssue](t, seed.mustRequest(t, caller, http.MethodGet,
			"/api/v1/issues/"+blocked+"/legion", nil, http.StatusOK))
		if answer.State == nil || answer.State.WaitingLine == nil {
			t.Fatalf("%s: GET /api/v1/issues/%s/legion answered no state or no place in the line: %+v", caller, blocked, answer)
		}
		return want{answer.State.BlockedBy, answer.State.WaitingLine.Place, answer.State.WaitingLine.Length}
	}
	check := func(caller privateCaller, expected want) {
		t.Helper()
		got := read(caller)
		if !slices.Equal(got.blockedBy, expected.blockedBy) || got.place != expected.place || got.length != expected.length {
			t.Errorf("%s reads %s blocked by %v, place %d of %d; want blocked by %v, place %d of %d",
				caller, blocked, got.blockedBy, got.place, got.length, expected.blockedBy, expected.place, expected.length)
		}
	}
	check(callerCarol, want{[]string{hidden}, 2, 2})
	check(callerAlice, want{[]string{}, 1, 1})

	seed.setVisibility(t, seedPrivate, "public")
	check(callerAlice, want{[]string{hidden, nobody}, 2, 2})
}
