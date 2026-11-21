package config

import "testing"

// claimFirstTurnBound is the fixture's shape: milliseconds, the unit of the plugin's own number
// and of LEGION_CLAIM_FIRST_TURN_BOUND_MS, in a field named as the plugin's own source names it.
type claimFirstTurnBound struct {
	ClaimFirstTurnBoundMs int64 `json:"claimFirstTurnBoundMs"`
}

// The bound a session's claim boot waits for its first turn lives once, in
// `packages/pi-shared/src/claim-first-turn-bound.ts`; ClaimFirstTurnBound is the daemon's copy.
// This golden is where they meet: Go writes it (`-update`), and
// `packages/pi-shared/src/legion-claim-core.test.ts` reads it back and holds it equal to
// DEFAULT_CLAIM_FIRST_TURN_BOUND_MS, so a bound moved on one side alone fails a test on the other.
func TestClaimFirstTurnBoundGolden(t *testing.T) {
	golden(t, "claim-first-turn-bound.json", claimFirstTurnBound{ClaimFirstTurnBoundMs: ClaimFirstTurnBound.Milliseconds()})
}
