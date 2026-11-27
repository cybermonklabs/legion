package leakguard

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
)

// git sets GIT_EXEC_PATH in every hook's environment and puts that directory first on the hook's
// PATH, and a push runs that directory's git-receive-pack, which can write anywhere. So StaysLocal
// answers false for a push under an exec path that is not git's own, even to a local path from a
// repository with no remote, and the git it asks for git's own exec path is looked up on PATH with
// that directory taken out: the directory's own git, first on the guard's PATH here as on a hook's,
// never runs for it. That git logs each run and exits 0 printing nothing, so a read it answered
// would find no remote, no setting and no exec path to compare, and the log says it ran.
func TestStaysLocalRunsNoGitOfAForeignExecPath(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	repo := filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	execPath, log := t.TempDir(), filepath.Join(t.TempDir(), "runs")
	logging(t, filepath.Join(execPath, "git"), log)
	guard := New(Options{Env: append(fixtureenv.Isolated(t.TempDir()), "GIT_EXEC_PATH="+execPath, pathWith(execPath))})
	if guard.StaysLocal(bound(t), repo, filepath.Join(root, "remote.git")) {
		t.Error("StaysLocal cleared a push under a GIT_EXEC_PATH that is not git's own")
	}
	if ran, err := os.ReadFile(log); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the git in GIT_EXEC_PATH ran for StaysLocal: %q (%v)", ran, err)
	}
}
