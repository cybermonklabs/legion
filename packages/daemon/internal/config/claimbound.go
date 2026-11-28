package config

import "time"

// ClaimFirstTurnBound is how long a Legion session's claim boot waits for the session's first turn
// before it takes its role's Envoy name itself: DEFAULT_CLAIM_FIRST_TURN_BOUND_MS of
// `packages/pi-shared/src/claim-first-turn-bound.ts`. The daemon has no other copy of it, and it
// copies no LEGION_CLAIM_FIRST_TURN_BOUND_MS into a role's environment (the daemon's
// stallLimitVariables does not hold the name), so a role process runs on that default. The daemon
// counts from it when a claim a project release stopped holds its role again (the daemon's
// releaseStoppedClaims).
//
// The two copies cannot drift in silence: TestClaimFirstTurnBoundGolden writes this one to
// `packages/contracts/fixtures/daemon-api/claim-first-turn-bound.json`, and
// `packages/pi-shared/src/legion-claim-core.test.ts` reads that fixture back and holds it equal to
// the plugin's own default, so a bound moved on one side alone fails a test on the other.
const ClaimFirstTurnBound = 30 * time.Second
