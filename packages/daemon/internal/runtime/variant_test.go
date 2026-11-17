package runtime

import (
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The tails a role may start on hold an arm's overlays only for a role the arm's experiment
// reaches: the implementer of an experiment that names only the implementer starts on its own
// files with the arm's, and the reviewer on its own files alone, so the boot gate and the image
// probe read no tail that no launch produces. An arm whose experiment names no roles reaches
// every issue role, as before.
func TestRoleOverlayValuesLeaveOutAnArmForARoleItsExperimentLeavesOut(t *testing.T) {
	own := []string{"/etc/legion-operator/own.yml"}
	limited := []Variant{{Experiment: "cheaper-model", Name: "open", ConfigFiles: []string{"/etc/legion-experiments/open.yml"},
		Roles: []claim.Role{claim.RoleImplementer}}}
	every := []Variant{{Experiment: "cheaper-model", Name: "open", ConfigFiles: []string{"/etc/legion-experiments/open.yml"}}}
	files := func(values []RoleOverlayValue) [][]string {
		var out [][]string
		for _, value := range values {
			out = append(out, value.Files)
		}
		return out
	}
	withArm := [][]string{{"/etc/legion-operator/own.yml", "/etc/legion-experiments/open.yml"}}
	ownAlone := [][]string{{"/etc/legion-operator/own.yml"}}
	for _, tc := range []struct {
		name     string
		role     claim.Role
		variants []Variant
		want     [][]string
	}{
		{"the implementer, which the experiment names", claim.RoleImplementer, limited, withArm},
		{"the reviewer, which it leaves out", claim.RoleReviewer, limited, ownAlone},
		{"the reviewer, under an experiment that names no roles", claim.RoleReviewer, every, withArm},
	} {
		if got := files(RoleOverlayValues(tc.role, own, tc.variants)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: RoleOverlayValues = %q, want %q", tc.name, got, tc.want)
		}
	}
}
