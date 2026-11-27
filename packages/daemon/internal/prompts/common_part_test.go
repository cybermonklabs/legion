package prompts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// Each of the seven roles an issue's agents start with composes one of the two parts this daemon
// writes for every role of a kind, and not the other: an owner composes architect-common.md, and
// the agent of a step composes worker-common.md. The test plants a marker of its own in each part
// of the snapshot a launch reads (New's state directory), then reads which marker each composed
// prompt holds. It reads no word of Legion's own texts, and no composition holds either marker
// before the planting. It shows which common part each role composes, and nothing of what the
// part says.
func TestEveryRoleComposesItsKindsCommonPart(t *testing.T) {
	const (
		ownersMarker  = "planted-owners-common-part-5a1c"
		workersMarker = "planted-workers-common-part-5a1c"
	)
	stateDir := t.TempDir()
	composer, err := New(stateDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	roles := []struct {
		name   string
		role   claim.Role
		isRoot bool
		owner  bool
	}{
		{"root architect", claim.RoleArchitect, true, true},
		{"sub-architect", claim.RoleArchitect, false, true},
		{"planner", claim.RolePlanner, false, false},
		{"implementer", claim.RoleImplementer, false, false},
		{"tester", claim.RoleTester, false, false},
		{"reviewer", claim.RoleReviewer, false, false},
		{"merger", claim.RoleMerger, false, false},
	}
	for _, tc := range roles {
		text := composedText(t, composer, tc.role, tc.isRoot)
		for _, marker := range []string{ownersMarker, workersMarker} {
			if strings.Contains(text, marker) {
				t.Fatalf("the composed %s prompt holds %q before it is planted", tc.name, marker)
			}
		}
	}
	for part, marker := range map[string]string{"architect-common.md": ownersMarker, "worker-common.md": workersMarker} {
		path := filepath.Join(stateDir, "prompts", "go", part)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read the snapshot's go/%s: %v", part, err)
		}
		if err := os.WriteFile(path, append(body, []byte("\n"+marker+"\n")...), 0o600); err != nil {
			t.Fatalf("plant the marker in the snapshot's go/%s: %v", part, err)
		}
	}
	for _, tc := range roles {
		t.Run(tc.name, func(t *testing.T) {
			text := composedText(t, composer, tc.role, tc.isRoot)
			want, wantPart, other, otherPart := workersMarker, "go/worker-common.md", ownersMarker, "go/architect-common.md"
			if tc.owner {
				want, wantPart, other, otherPart = ownersMarker, "go/architect-common.md", workersMarker, "go/worker-common.md"
			}
			if !strings.Contains(text, want) {
				t.Errorf("the composed %s prompt holds no marker planted in %s", tc.name, wantPart)
			}
			if strings.Contains(text, other) {
				t.Errorf("the composed %s prompt holds the marker planted in %s, the other kind's part", tc.name, otherPart)
			}
		})
	}
}
