package workerbin

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/testexec"
)

// The scripts Install writes run at once while the process forks beside the install: an agent runs
// them by name off its PATH. Install's callers start no command beside it today: a tmux daemon
// installs the scripts at its boot before it starts supervision, the NATS connection or its API,
// and `legion controller start` while nothing else of it starts one. RunsBeside's forks stand in
// for a process that does. Each install's four scripts are four runs: the first of each four
// installs again, over the install before, and the four run newest first, so jj, which Install
// writes last, runs as soon as it returns. Run oldest first, legion's run came after the writes of
// gh and git, and with programfile.Write's lock taken out the test met 13 refusals in one run of
// its 450 and none in another. Once Install came to write jj after git, the first run, git's, no
// longer followed its own write: with the lock taken out, one run in ten met no refusal in 450,
// and the other nine met 4 to 22.
func TestTheScriptsInstallWritesRunWhileTheProcessForks(t *testing.T) {
	legion, err := exec.LookPath("true")
	if err != nil {
		t.Fatalf("no true command for the scripts to exec: %v", err)
	}
	root := t.TempDir()
	names := []string{"jj", "git", "gh", "legion"}
	const installs = 150
	testexec.RunsBeside(t, len(names)*installs, func(i int) error {
		if i%len(names) == 0 {
			if err := Install(root, legion, ""); err != nil {
				return fmt.Errorf("Install: %w", err)
			}
		}
		name := names[i%len(names)]
		if err := exec.Command(filepath.Join(LauncherDir(root), name)).Run(); err != nil {
			return fmt.Errorf("run the installed %s: %w", name, err)
		}
		return nil
	})
}
