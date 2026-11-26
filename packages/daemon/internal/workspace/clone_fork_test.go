package workspace

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/testexec"
)

// The one-shot credential's helper runs at once while the process forks beside its write: git runs
// the helper by its path, with `get`, as soon as a clone or fetch asks for the credential, and the
// daemon starts git, gh and tmux from many goroutines. Each run makes the credential, runs its
// helper as git does, checks that it answered the token, and removes the credential.
func TestTheProvisioningHelperRunsWhileTheProcessForks(t *testing.T) {
	parent := t.TempDir()
	testexec.RunsBeside(t, 400, func(int) error {
		credential, err := newProvisioningCredential(parent, testInstallationToken)
		if err != nil {
			return fmt.Errorf("make the provisioning credential: %w", err)
		}
		helper := exec.Command(filepath.Join(credential.dir, "helper"), "get")
		helper.Env = append([]string{"PATH=" + os.Getenv("PATH")}, credential.env...)
		answer, run := helper.Output()
		if err := credential.remove(); err != nil {
			return fmt.Errorf("remove the provisioning credential: %w", err)
		}
		switch {
		case run != nil:
			return fmt.Errorf("run the provisioning helper: %w", run)
		case !strings.Contains(string(answer), testInstallationToken):
			return fmt.Errorf("the provisioning helper answered %q, want the token it was made with", answer)
		}
		return nil
	})
}
