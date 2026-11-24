package launcher

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// A launcher answers a check of its slot's gh files at once, from one read of the hosts.yml the
// check names: a check of the digest of the token the file holds is OK, a check of another digest
// is not, and neither waits for the file to change. A check that names a file outside the slot's gh
// directory is refused with its cause, and so is a check of the controller's launcher, which reads
// its gh directory only from a start.
func TestALauncherAnswersACredentialCheck(t *testing.T) {
	rolesDir := t.TempDir()
	hosts := filepath.Join(rolesDir, "architect", RoleGHDir, "sjawhar", ghconfig.HostsFile)
	if err := os.MkdirAll(filepath.Dir(hosts), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hosts, []byte(ghconfig.Render("ghs_implement", "implement", time.Date(2026, 10, 8, 13, 0, 0, 0, time.UTC)).Hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := func(token string) string {
		sum := sha256.Sum256([]byte(token))
		return hex.EncodeToString(sum[:])
	}
	m := &manager{role: "architect", namespaced: true, cfg: Config{RolesDir: rolesDir}, requests: map[string]request{}}

	for _, tc := range []struct {
		name  string
		token string
		ok    bool
	}{
		{"the token the slot's hosts.yml holds", "ghs_implement", true},
		{"another App's token", "ghs_review", false},
	} {
		asked := time.Now()
		got := m.checkGitHubCredential(shimwire.LauncherCredentialCheck{ID: "check-1", HostsFile: "sjawhar/" + ghconfig.HostsFile, TokenDigest: digest(tc.token)})
		if want := (shimwire.LauncherCredentialCheckResult{ID: "check-1", OK: tc.ok}); got != want {
			t.Errorf("a check of %s = %+v, want %+v", tc.name, got, want)
		}
		if took := time.Since(asked); took > time.Second {
			t.Errorf("a check of %s took %s, want an answer at once", tc.name, took)
		}
	}

	outside := m.checkGitHubCredential(shimwire.LauncherCredentialCheck{ID: "check-2", HostsFile: "../../tester/gh/sjawhar/" + ghconfig.HostsFile, TokenDigest: digest("ghs_implement")})
	if outside.OK || outside.Error == "" {
		t.Errorf("a check of a file outside the slot's gh directory = %+v, want a refusal with its cause", outside)
	}
	controller := (&manager{role: "controller", requests: map[string]request{}}).checkGitHubCredential(
		shimwire.LauncherCredentialCheck{ID: "check-3", HostsFile: "sjawhar/" + ghconfig.HostsFile, TokenDigest: digest("ghs_implement")})
	if controller.OK || controller.Error == "" {
		t.Errorf("a check of the controller's launcher = %+v, want a refusal with its cause", controller)
	}
}
