package daemon

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
)

// The tmux boot reads each overlay of a role the deployment defines, as it reads Legion's: an
// overlay of role_config_files.checker that no pane could read refuses the boot, naming the key,
// the file and the role. The roles it walks are Legion's (config.RoleConfigRoles), then each role
// of `roles` in the file's order (overlayRoles).
func TestTheBootChecksADeploymentRolesOverlays(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "checker.yml")
	cfg := config.Config{
		Roles:           []config.Role{{Name: "checker", Type: "testing"}},
		RoleConfigFiles: map[claim.Role][]string{"checker": {missing}},
	}

	err := checkRoleConfigFiles(cfg)
	want := "role_config_files.checker names " + missing + ", which the checker's Oh My Pi could not read: "
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("checkRoleConfigFiles = %v, want an error that begins %q", err, want)
	}

	wantRoles := append(slices.Clone(config.RoleConfigRoles), "checker")
	if got := overlayRoles(cfg); !slices.Equal(got, wantRoles) {
		t.Errorf("overlayRoles = %v, want %v", got, wantRoles)
	}
}
