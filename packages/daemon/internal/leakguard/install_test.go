package leakguard

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// theTag is the pin these tests install, spelled as the worker image spells one.
const theTag = "leak-check-v15"

// The members the test's own tarball carries under its leak-check directory. They are the shape
// the real one has at leak-check-v15, which this package measured: the launcher, the python file
// it runs from beside itself, a fixture its self-test reads, and a nested directory. A file
// outside that directory is in the tarball too, to show that the rest of the repository is left
// where it is.
const (
	theNeighbour = "public_leak_check.py"
	theFixture   = "must-pass-lines.txt"
	theNested    = "revert-gate/run.py"
)

// answersItsHelp is a stand-in launcher that answers as the real one does: it finds its python
// file beside itself, prints the usage header naming the two shapes this package calls, and exits
// 1, which is what `public-leak-check.sh --help` exits with at leak-check-v15. A caller that read
// the status rather than the answer would call this a failure.
const answersItsHelp = `#!/bin/sh
beside="$(dirname "$0")/` + theNeighbour + `"
if [ ! -f "$beside" ]; then
  echo "LEAK CHECK ERROR: $beside is missing, so the check could not start" >&2
  exit 2
fi
echo '# Usage: check-public-text FILE..., --commits REV..., --tree DIR, gh-guard ARGS, or REMOTE URL.'
exit 1
`

// answersNothing is a stand-in launcher that cannot start: the shape the real launcher takes when
// /usr/bin/python3 is missing or the python file did not land beside it.
const answersNothing = `#!/bin/sh
echo "LEAK CHECK ERROR: /usr/bin/python3 is missing or not executable, so the check could not start" >&2
exit 2
`

// member is one file of the test's tarball.
type member struct {
	body string
	mode int64
}

// tarball writes a gzipped tarball shaped like GitHub's tarball of the check's repository: every
// member under one `<owner>-<repo>-<sha>` directory, the check's own files under that directory's
// `leak-check`, and one file of the repository outside it. It answers its path.
func tarball(t *testing.T, files map[string]member) string {
	t.Helper()
	var raw bytes.Buffer
	zipped := gzip.NewWriter(&raw)
	archive := tar.NewWriter(zipped)
	const prefix = "theorem-labs-agent-infra-84d835ba5d00f97212253ffa221898f1f234a772/"
	write := func(name string, m member) {
		t.Helper()
		if err := archive.WriteHeader(&tar.Header{Name: prefix + name, Mode: m.mode, Size: int64(len(m.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(m.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.WriteHeader(&tar.Header{Name: prefix + checkDirectory, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	write("README.md", member{body: "the rest of the repository\n", mode: 0o644})
	for name, m := range files {
		write(checkDirectory+"/"+name, m)
	}
	for _, closer := range []interface{ Close() error }{archive, zipped} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "tarball.tgz")
	if err := os.WriteFile(path, raw.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// wholeCheck is a tarball of a check that runs: the launcher, its python file, its fixture and a
// nested directory.
func wholeCheck(t *testing.T, launcher string) string {
	t.Helper()
	return tarball(t, map[string]member{
		checkName:    {body: launcher, mode: 0o755},
		theNeighbour: {body: "# the check itself\n", mode: 0o644},
		theFixture:   {body: "a line the self-test must pass\n", mode: 0o644},
		theNested:    {body: "# the revert gate\n", mode: 0o644},
	})
}

// standInGh writes a `gh` on the install's PATH and answers the directory it is on and the file
// each call appends its arguments to. script is the body after the recording line.
func standInGh(t *testing.T, script string) (bin, record string) {
	t.Helper()
	bin = t.TempDir()
	record = filepath.Join(bin, "calls")
	body := "#!/bin/sh\necho \"$*\" >> " + record + "\n" + script
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, record
}

// servesTheTarball is a stand-in gh that answers the fetch with path, after a pause: the pause is
// what makes a second caller of one pod actually wait on the lease rather than race past it.
func servesTheTarball(path string) string {
	return "sleep 0.3\nexec cat " + path + "\n"
}

// calls is the argument lines the stand-in gh recorded, in order.
func calls(t *testing.T, record string) []string {
	t.Helper()
	body, err := os.ReadFile(record)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
}

// installEnv is the environment an install runs under: the test's own, with the stand-in gh
// first on PATH — ahead of the image's own directories, which the stand-in's `sh` needs for the
// commands it runs — and the pin the worker image would carry.
func installEnv(t *testing.T, bin, tag string) []string {
	t.Helper()
	env := append(fixtureenv.Isolated(t.TempDir()), "PATH="+bin+":/usr/bin:/bin")
	if tag != "" {
		env = append(env, TagVariable+"="+tag)
	}
	return env
}

// leftovers is every entry beside the install directory, the lease file aside: a failed install
// leaves none, since the directory is unpacked under a name of its own and renamed into place.
func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, entry := range entries {
		if entry.Name() != filepath.Base(dir)+".lock" {
			left = append(left, entry.Name())
		}
	}
	return left
}

// The install fetches the pinned tag with the gh on its PATH, lands the check's whole directory,
// and runs the installed check before anything may use it. A fetch that fails and an installed
// check that does not answer each fail the install, and each leaves the install path absent and
// nothing half-unpacked beside it: a role that finds the directory finds a whole install.
func TestInstallLandsTheWholeCheckOrFailsWithNothingInPlace(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gh      func(t *testing.T) string
		want    string // the substring the failure names, empty for an install that must succeed
		fetched bool   // whether gh was called at all
	}{
		{
			name:    "a fetch that serves the tarball installs the check",
			gh:      func(t *testing.T) string { return servesTheTarball(wholeCheck(t, answersItsHelp)) },
			fetched: true,
		},
		{
			name:    "a 404 of the tag fails the install",
			gh:      func(*testing.T) string { return "echo 'gh: Not Found (HTTP 404)' >&2\nexit 1\n" },
			want:    "exited 1",
			fetched: true,
		},
		{
			name:    "a fetched check that does not run fails the install",
			gh:      func(t *testing.T) string { return servesTheTarball(wholeCheck(t, answersNothing)) },
			want:    "names no check-public-text shape",
			fetched: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, record := standInGh(t, tc.gh(t))
			dir := filepath.Join(t.TempDir(), "leak-check")
			err := Install(bound(t), InstallOptions{Env: installEnv(t, bin, theTag), Dir: dir})
			check := filepath.Join(dir, checkName)

			if tc.want != "" {
				if got := setupFailure(t, err); !strings.Contains(got.Error(), tc.want) {
					t.Errorf("install = %q, want it to name %q", got, tc.want)
				}
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Errorf("stat %s = %v, want the install path absent after a failed install", dir, err)
				}
				if left := leftovers(t, dir); left != nil {
					t.Errorf("a failed install left %v beside the install path", left)
				}
			} else {
				if err != nil {
					t.Fatalf("install: %v", err)
				}
				// The whole directory, because the launcher resolves its python file beside
				// itself and its self-test reads the fixture from there.
				for _, name := range []string{checkName, theNeighbour, theFixture, theNested} {
					if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
						t.Errorf("the install is missing %s: %v", name, err)
					}
				}
				if _, err := os.Stat(filepath.Join(dir, "README.md")); !os.IsNotExist(err) {
					t.Errorf("stat the repository's own README in the install = %v, want nothing of the repository outside %s", err, checkDirectory)
				}
				// Installed and runnable: the same shape the install verified with.
				printed, err := exec.Command(check, "--help").Output()
				if err == nil || !strings.Contains(string(printed), "gh-guard") {
					t.Errorf("the installed check answered %q (%v), want the usage header it was verified by", printed, err)
				}
			}

			made := calls(t, record)
			if !tc.fetched {
				if made != nil {
					t.Errorf("gh was called %v, want no call at all", made)
				}
				return
			}
			if want := []string{"api repos/" + checkSlug + "/tarball/" + theTag}; !slices.Equal(made, want) {
				t.Errorf("gh was called %v, want %v", made, want)
			}
		})
	}
}

// With no pin in the shim's environment the install does nothing and succeeds: that is a tmux
// pane, whose machine carries the check through the check's own installer, and where it carries
// none every entry point of this package refuses the write with a setup failure naming it.
func TestWithNoPinTheInstallDoesNothing(t *testing.T) {
	bin, record := standInGh(t, servesTheTarball(wholeCheck(t, answersItsHelp)))
	dir := filepath.Join(t.TempDir(), "leak-check")
	if err := Install(bound(t), InstallOptions{Env: installEnv(t, bin, ""), Dir: dir}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("stat %s = %v, want nothing installed", dir, err)
	}
	if made := calls(t, record); made != nil {
		t.Errorf("gh was called %v, want no call at all", made)
	}
}

// The six role shims of one pod start together and install once between them: the lease is taken
// before the install is decided on, so the role that waits finds the check already there, fetches
// nothing, and still returns to a whole install.
func TestTwoShimsOfOnePodInstallOnce(t *testing.T) {
	bin, record := standInGh(t, servesTheTarball(wholeCheck(t, answersItsHelp)))
	dir := filepath.Join(t.TempDir(), "leak-check")
	env := installEnv(t, bin, theTag)
	var wait sync.WaitGroup
	failures := make([]error, 2)
	for shim := range failures {
		wait.Add(1)
		go func() {
			defer wait.Done()
			failures[shim] = Install(bound(t), InstallOptions{Env: env, Dir: dir})
		}()
	}
	wait.Wait()
	for shim, err := range failures {
		if err != nil {
			t.Errorf("shim %d's install: %v", shim, err)
		}
	}
	if made := calls(t, record); len(made) != 1 {
		t.Errorf("gh was called %d times (%v), want the pod's one fetch", len(made), made)
	}
	// Both callers saw the whole install, not a directory one of them was still filling.
	for _, name := range []string{checkName, theNeighbour, theFixture, theNested} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("the install is missing %s: %v", name, err)
		}
	}
}
