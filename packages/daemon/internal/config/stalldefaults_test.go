package config

// This row and two of the envoy rows below it - internal/dispatch/outbox/
// expects_reply_never_false_test.go and internal/dispatch/api/live_events_test.go - declare a
// read every filter gating a job that compiles them already matches. They are here so that a
// later narrowing of one of those filters is refused by rule 1 rather than dropping the read
// with nobody noticing (dispatch://INFRA-1413).
//ci:reads packages/contracts/fixtures/daemon-api/* golden (the write is under -update, which CI never passes)

import (
	"flag"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/testgolden"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden fixtures this package pins")

// stallDefaults is the fixture's shape. Milliseconds, because that is the unit of the plugin's
// own numbers and of every LEGION_STALL_* variable; one field per number, each named as the
// plugin's own source names it, so the bun test reads the two side by side.
type stallDefaults struct {
	CallGraceMs int64 `json:"callGraceMs"`
	CallBoundMs int64 `json:"callBoundMs"`
	StepMs      int64 `json:"stepMs"`
	// DeclaredDeadlineMostMs is the hour the host clamps a call's own declared deadline to
	// (StallDeclaredDeadlineCap), which no variable moves: it is here because the daemon's
	// backstop floor is reckoned from it, so it is a copy of a plugin number like the three
	// above and is held to the plugin's own DECLARED_DEADLINE_MOST_MS the same way.
	DeclaredDeadlineMostMs int64 `json:"declaredDeadlineMostMs"`
	// CallBoundMsByRole is the bound's default for every role a session can hold, the controller
	// among them, since the bound's default is the role's and the daemon's floor is per role.
	CallBoundMsByRole map[claim.Role]int64 `json:"callBoundMsByRole"`
}

// The numbers a worker's stall watch runs on live once, in `packages/pi-shared/src/stall-limits.ts`
// and `packages/pi-shared/src/stall-watchdog.ts`; this package holds the copies the daemon's own
// backstop floor is computed from (stalldefaults.go). This golden is where they meet: Go writes it
// (`-update`), and `packages/pi-shared/src/stall-limits.test.ts` reads it back and holds it equal
// to DEFAULT_STALL_LIMITS, to the per-role bounds the plugin resolves and to the watchdog's
// DECLARED_DEADLINE_MOST_MS, so a number moved on one side alone fails a test on the other.
func TestStallDefaultsGolden(t *testing.T) {
	// RoleConfigRoles is every issue role and then the controller: the same set a session's
	// LEGION_ROLE can hold, and so the set the bound has a default for.
	byRole := make(map[claim.Role]int64, len(RoleConfigRoles))
	for _, role := range RoleConfigRoles {
		byRole[role] = StallCallBoundFor(role).Milliseconds()
	}
	golden(t, "stall-defaults.json", stallDefaults{
		CallGraceMs:            StallCallGrace.Milliseconds(),
		CallBoundMs:            StallCallBound.Milliseconds(),
		StepMs:                 StallStep.Milliseconds(),
		DeclaredDeadlineMostMs: StallDeclaredDeadlineCap.Milliseconds(),
		CallBoundMsByRole:      byRole,
	})
}

// golden pins what this package writes for the plugin to read (testgolden.Check).
func golden(t *testing.T, name string, value any) {
	t.Helper()
	testgolden.Check(t, filepath.Join("..", "..", "..", "contracts", "fixtures", "daemon-api", name), value, *updateGolden,
		"go test ./internal/config/ -update")
}
