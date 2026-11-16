package config

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden fixture this package pins")

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

// golden pins what this package writes for the plugin to read: internal/api's helper with this
// package's command in its message, as internal/projection's copy carries its own. A fixture that
// differs from what Go now writes is stale.
func golden(t *testing.T, name string, value any) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	encoded = append(encoded, '\n')
	path := filepath.Join("..", "..", "..", "contracts", "fixtures", "daemon-api", name)
	if *updateGolden {
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run `go test ./internal/config/ -update`): %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("%s is stale\n got: %s\nwant: %s", name, encoded, want)
	}
}
