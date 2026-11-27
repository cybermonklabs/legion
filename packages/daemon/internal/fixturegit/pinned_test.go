package fixturegit

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// A pinned jj that PATH reaches through a relative entry is found, and PinnedJJ answers it as an
// absolute path, which runs after the test moves to another directory: lookpath.Others keeps a
// relative entry as it is, and exec.Cmd would take a relative path against its own Dir. The
// relative entry holds a link to the pinned jj the test process's own PATH reaches, the one CI's
// install-jj puts at ~/.local/bin/jj, and is the only jj on the PATH the lookup reads.
func TestPinnedJJAnswersARelativeEntryAsAnAbsolutePath(t *testing.T) {
	// Not parallel: it sets the test process's PATH and changes its working directory.
	pinned := PinnedJJ(t)
	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "relative-bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(pinned, filepath.Join(root, "relative-bin", "jj")); err != nil {
		t.Fatal(err)
	}
	// readJJPin walks up from the working directory to .github/ci-tools.yaml, so the lookup's
	// working directory is a copy of the path to this package's: a directory under root whose
	// .github holds a link to the repository's ci-tools.yaml.
	if err := os.Mkdir(filepath.Join(root, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	found := ""
	for dir := here; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".github", "ci-tools.yaml")); err == nil {
			found = filepath.Join(dir, ".github", "ci-tools.yaml")
			break
		}
		if filepath.Dir(dir) == dir {
			t.Fatal("no .github/ci-tools.yaml above the package's directory")
		}
	}
	if err := os.Symlink(found, filepath.Join(root, ".github", "ci-tools.yaml")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("PATH", "relative-bin")

	got := PinnedJJ(t)
	if want := filepath.Join(root, "relative-bin", "jj"); got != want {
		t.Fatalf("PinnedJJ answered %q for the relative PATH entry relative-bin, want the absolute %q", got, want)
	}
	t.Chdir(t.TempDir())
	command := exec.Command(got, "--version")
	command.Dir, command.Env = t.TempDir(), fixtureenv.Isolated(t.TempDir())
	if out, err := command.Output(); err != nil {
		t.Fatalf("the jj PinnedJJ answered, %s, run from another directory: %v (%s)", got, err, out)
	}
}
