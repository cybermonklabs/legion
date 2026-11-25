package api

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// TestALegionBoardLeavesOutABlockerItsReaderCannotSee: PUB's daemon reports PUB's issue blocked
// by PRIV's issue and by a key no issue holds. carol, who sees PRIV, reads PRIV's issue on the
// card; alice, who does not, reads no blocker at all. The key no issue holds is left out for both,
// as it is from an issue's own blocked_by (leaveOutHiddenIssueReferences): a reader that does not
// see every object reads a key of a project it does not see and a key nothing holds alike, so the
// board tells it nothing of which keys a hidden project holds.
//
// Control: the board read keeps every key (leaveOutHiddenIssues returning at once), and alice's
// card names PRIV's issue.
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
		{callerCarol, []string{hidden}},
		{callerAlice, []string{}},
	} {
		board := decodeBody[legionBoardResponse](t, seed.mustRequest(t, row.caller, http.MethodGet,
			"/api/v1/projects/"+seedPublic+"/legion/board", nil, http.StatusOK))
		if got := legionCard(t, board, blocked).BlockedBy; !slices.Equal(got, row.want) {
			t.Errorf("%s reads %s's card blocked by %v, want %v", row.caller, blocked, got, row.want)
		}
	}
}

// TestALegionBoardShowsNoIssueItsReaderCannotSee is the trace of a full member who is not on
// PRIV's list: alice names, as PUB's daemon, a daemon that reports PRIV's issues, and the sampler
// recorded what it reported under PUB, as every read did before a project's record was cut to its
// own issues (legion.State.keepProject). PUB's issue is in design holding slot 0, its tree root
// the PRIV issue the daemon names; PRIV's first issue is implementing in slot 1 with its
// implementer in a turn, and its second waits in the line, blocked by the first; a key no issue
// holds is testing in slot 2. The daemon lists all three slots, both PRIV issues among the trees.
//
// alice, who does not see PRIV, is answered a board that names none of PRIV's issues, nor the key
// nothing holds, anywhere in its body: PUB's card alone, its tree its own, one slot in use, one
// tree and an empty queue. carol, who sees PRIV, keeps the board the daemon reported of PRIV's
// issues: PRIV's cards with their slot and roles, the waiting issue's blocker, PUB's tree root, two
// slots and two trees in use; the key nothing holds is left out for her too, since she does not
// see every object of a deployment that holds a private project.
//
// Control: the board read keeps every key (leaveOutHiddenIssues returning at once), and alice's
// board names PRIV's issues, their slots and roles.
func TestALegionBoardShowsNoIssueItsReaderCannotSee(t *testing.T) {
	seed := seedPrivateProject(t)
	seed.mustRequest(t, callerAlice, http.MethodPut, "/api/v1/projects/"+seedPublic+"/legion/daemon",
		map[string]any{"state_url": "http://legion-priv.internal:8080"}, http.StatusOK)
	public, working := seed.issue[seedPublic], seed.issue[seedPrivate]
	waiting := decodeBody[struct {
		Key string `json:"key"`
	}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues",
		map[string]string{"project": seedPrivate, "title": "Waits for a slot"}, http.StatusCreated)).Key
	nobody := seedPublic + "-99"
	at := time.Now().UTC().Add(-time.Hour)
	observe := func(issue, field, value string) {
		t.Helper()
		seedLegionObservation(t, seed.database, seedPublic, issue, field, value, at)
	}
	observe(public, legion.FieldPhase, `"admitted"`)
	observe(public, legion.FieldSlot, `0`)
	observe(public, legion.FieldTree, fmt.Sprintf("%q", working))
	observe(working, legion.FieldPhase, `"implementing"`)
	observe(working, legion.FieldSlot, `1`)
	observe(working, legion.ClaimFieldPrefix+"implementer", `"working"`)
	observe(working, legion.FieldTree, fmt.Sprintf("%q", working))
	observe(waiting, legion.FieldPhase, `"admitted"`)
	observe(nobody, legion.FieldPhase, `"testing"`)
	observe(nobody, legion.FieldSlot, `2`)
	observe(legion.AdmissionKey, legion.FieldAdmission, fmt.Sprintf(`{
		"cap": 4, "active": [%q, %q, %q], "waiting": [%q], "trees": [%q, %q],
		"blocked": [{"issue": %q, "blockedBy": [%q]}]
	}`, public, working, nobody, waiting, public, working, waiting, working))
	seedLegionRead(t, seed.database, seedPublic, time.Now().UTC().Add(-time.Minute), "")

	read := func(caller privateCaller) (legionBoardResponse, string) {
		t.Helper()
		response := seed.mustRequest(t, caller, http.MethodGet, "/api/v1/projects/"+seedPublic+"/legion/board", nil, http.StatusOK)
		return decodeBody[legionBoardResponse](t, response), response.Body.String()
	}

	board, body := read(callerAlice)
	for _, key := range []string{working, waiting, nobody} {
		if strings.Contains(body, key) {
			t.Errorf("alice's board of PUB names %s, which she may not see: %s", key, body)
		}
	}
	for _, column := range board.Columns {
		want := []string{}
		if column.ID == legionColumnDesign {
			want = []string{public}
		}
		if got := legionColumnKeys(t, board, column.ID); !slices.Equal(got, want) {
			t.Errorf("alice's %s column holds %v, want %v", column.ID, got, want)
		}
	}
	if tree := legionCard(t, board, public).Tree; tree != public {
		t.Errorf("alice reads %s in tree %q, want its own: the root the daemon names is PRIV's", public, tree)
	}
	if header := board.Header; header.SlotsInUse != 1 || header.Trees != 1 || header.QueueLength != 0 {
		t.Errorf("alice's header = %+v, want PUB's one slot and one tree in use and an empty queue", header)
	}

	board, body = read(callerCarol)
	if strings.Contains(body, nobody) {
		t.Errorf("carol's board of PUB names %s, which no issue holds: %s", nobody, body)
	}
	card := legionCard(t, board, working)
	if card.Phase != "implementing" || card.Slot == nil || *card.Slot != 1 || !slices.Equal(card.Roles, []string{"implementer"}) {
		t.Errorf("carol reads %s's card as %+v, want it implementing in slot 1 with its implementer in a turn", working, card)
	}
	if got := legionColumnKeys(t, board, legionColumnQueue); !slices.Equal(got, []string{waiting}) {
		t.Errorf("carol's queue holds %v, want %s", got, waiting)
	}
	if blockers := legionCard(t, board, waiting).BlockedBy; !slices.Equal(blockers, []string{working}) {
		t.Errorf("carol reads %s blocked by %v, want %s", waiting, blockers, working)
	}
	if tree := legionCard(t, board, public).Tree; tree != working {
		t.Errorf("carol reads %s in tree %q, want %s, the root the daemon names", public, tree, working)
	}
	if header := board.Header; header.SlotsInUse != 2 || header.Trees != 2 || header.QueueLength != 1 {
		t.Errorf("carol's header = %+v, want two slots and two trees in use and a queue of one", header)
	}
}
