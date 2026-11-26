package api

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// strandedTokenDropped is the `dropped` of the band of strandedTokenContract's entry, the line it
// writes, named here as a literal so a strip applied beside the table's loop fails this test.
const strandedTokenDropped = "the stranded claims' token, the claim views' token and the issues' otherClaims"

// A caller one contract below the stranded token's entry is served each stranded claim and each
// claim view without its `token` and each issue without its `otherClaims`, keeps every stranded
// claim, and is told so in one line of the entry's band. It stands at or above the deployment
// roles' entry, so LEGION-240 is still at step with its checker. It reads the band's number from
// the registry (entryNumber), so it holds no entry to be the newest (R4 of dispatch://INFRA-1569).
func TestACallerBelowTheStrandedTokenEntryIsServedNoTokens(t *testing.T) {
	state := populatedState(t)
	if other := state.Issues["LEGION-208"].OtherClaims; len(other) == 0 || other[0].Claim.Token == "" {
		t.Fatal("the fixture must carry another claim and its token")
	}
	if len(state.StrandedClaims) == 0 || state.StrandedClaims[0].Token == "" {
		t.Fatal("the fixture must carry a stranded claim's token")
	}
	if entryNumber(strandedTokenContract) <= entryNumber(deploymentRolesContract) {
		t.Fatalf("the stranded token's entry is numbered %d, at or below the deployment roles' %d: this test reads LEGION-240 at step below it",
			entryNumber(strandedTokenContract), entryNumber(deploymentRolesContract))
	}
	below := entryNumber(strandedTokenContract) - 1
	body, narrowings := stateNarrowedStating(t, state, statePodAddr+":41000", podClaims(0), below)
	views := issueViews(t, body)
	if step := views["LEGION-240"]; step["phase"] != string(phase.Step) || step["stepRole"] != "checker" {
		t.Errorf("the body served at %d names LEGION-240 at %v with stepRole %v, want step and checker: deployment roles remain at this contract",
			below, step["phase"], step["stepRole"])
	}
	for _, view := range claimViews(t, body) {
		wantNoKeys(t, "a claim view served below the stranded token's entry", view, "token")
	}
	for _, view := range views {
		wantNoKeys(t, "an issue served below the stranded token's entry", view, "otherClaims")
	}
	var served struct {
		StrandedClaims []map[string]any `json:"strandedClaims"`
	}
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatal(err)
	}
	if len(served.StrandedClaims) == 0 {
		t.Fatal("the token narrowing removed every stranded claim, want their views without tokens")
	}
	for _, view := range served.StrandedClaims {
		wantNoKeys(t, "a stranded claim served below the stranded token's entry", view, "token")
	}
	want := narrowingsExpected(state, below)
	if !slices.Contains(want, strandedTokenDropped) {
		t.Fatalf("a caller at %d expects the lines %v, want the stranded token band's %q among them", below, want, strandedTokenDropped)
	}
	wantNarrowings(t, narrowings, below, want)
	if got := droppedAt(t, entryNumber(strandedTokenContract)); got != strandedTokenDropped {
		t.Errorf("the band at the stranded token's entry drops %q, want %q", got, strandedTokenDropped)
	}
}
