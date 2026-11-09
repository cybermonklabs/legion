package config

import (
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The defaults a worker's own stall watch runs on, as its plugin holds them
// (`packages/pi-shared/src/stall-limits.ts`, DEFAULT_STALL_LIMITS and defaultCallBoundMs). The
// daemon holds this copy because its own backstop bound must stay above the whole ladder a
// working plugin runs — the role's bound, plus the grace, plus two step gaps — and it can only
// refuse a `legion.yaml` that sets a bound below that if it knows those numbers.
//
// The two copies cannot drift in silence: TestStallDefaultsGolden writes them to
// `packages/contracts/fixtures/daemon-api/stall-defaults.json`, and
// `packages/pi-shared/src/stall-limits.test.ts` reads that fixture back and holds it equal to the
// plugin's own defaults, so a number moved on one side alone fails a test on the other.
//
// A deployment moves any of them with `LEGION_STALL_CALL_GRACE_MS`,
// `LEGION_STALL_CALL_BOUND_MS` and `LEGION_STALL_STEP_MS`, which the daemon passes on to every
// worker from its own environment: one value for every role, where the defaults below are the
// role's.
const (
	// StallCallGrace is LEGION_STALL_CALL_GRACE_MS's default: the grace over the deadline a call
	// itself declared, longer than the host's own wait for a native cancellation.
	StallCallGrace = 5 * time.Minute
	// StallCallBound is LEGION_STALL_CALL_BOUND_MS's default for every role but the implementer
	// and the reviewer, and for the controller: the bound for a call no deadline judges.
	StallCallBound = time.Hour
	// StallCallBoundLong is that same default for the implementer and the reviewer, the two roles
	// that wait inside one call on a subagent of their own.
	StallCallBoundLong = 4 * time.Hour
	// StallStep is LEGION_STALL_STEP_MS's default: the gap between the recovery ladder's steps.
	StallStep = 2 * time.Minute
)

// StallDeclaredDeadlineCap is the longest deadline a call's own arguments can declare, as the
// host clamps them (`packages/pi-shared/src/stall-watchdog.ts`, DECLARED_DEADLINE_MOST_MS: 0
// disables the deadline and every other number is clamped to 1–3600 seconds, which is
// INFRA-1274's R6). A call that declares one is judged by it and the grace over it, whatever the
// role's bound is, so an hour is how long a working call can legitimately run before the
// worker's own ladder takes its first step, and so the daemon's backstop floor is reckoned from
// it where it is the longer of the two (StallLadderFor).
//
// It is no LEGION_STALL_* variable: the host clamps what it clamps, and a deployment moves
// nothing here. It is in this file because it is the same kind of number as the three above — a
// copy of the plugin's, held to the plugin's by the same golden. TestStallDefaultsGolden writes
// it to `packages/contracts/fixtures/daemon-api/stall-defaults.json` as `declaredDeadlineMostMs`
// and `packages/pi-shared/src/stall-limits.test.ts` holds that field equal to the plugin's own
// DECLARED_DEADLINE_MOST_MS, so neither copy can move alone.
const StallDeclaredDeadlineCap = time.Hour

// StallCallBoundFor is the default bound of a call no deadline judges for role: four hours for an
// implementer and a reviewer, one hour for every other role and for the controller.
func StallCallBoundFor(role claim.Role) time.Duration {
	if role == claim.RoleImplementer || role == claim.RoleReviewer {
		return StallCallBoundLong
	}
	return StallCallBound
}
