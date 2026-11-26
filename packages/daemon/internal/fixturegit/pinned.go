package fixturegit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/lookpath"
)

// pinnedJJRows are the rows of .github/ci-tools.yaml that pin jj, by GOARCH: each row's digest is
// the sha256 of the jj binary itself, the `./jj` member of the release tarball.
var pinnedJJRows = map[string]string{"amd64": "jj", "arm64": "jj-aarch64"}

// PinnedJJ is the jj that .github/ci-tools.yaml pins for this machine's architecture, for a test of
// what the pinned jj does: the first jj on the test process's PATH whose bytes have the pin's
// sha256, wherever it stands on PATH, so another jj in front of it (a dev box's own build, a
// fork) changes nothing about which binary the test runs. On CI the install-jj action puts the
// pinned binary at ~/.local/bin/jj. The test logs the binary's path and its --version, and fails,
// naming the pin and each jj on PATH with its digest, when PATH holds none: a machine without the
// pinned jj fails the test by name rather than testing another binary.
func PinnedJJ(tb testing.TB) string {
	tb.Helper()
	row, ok := pinnedJJRows[runtime.GOARCH]
	if !ok {
		tb.Fatalf(".github/ci-tools.yaml pins no jj for %s", runtime.GOARCH)
	}
	version, digest, err := readJJPin(row)
	if err != nil {
		tb.Fatal(err)
	}
	found, _ := lookpath.Others(os.Getenv("PATH"), "jj", "")
	passed := make([]string, 0, len(found))
	for _, each := range found {
		sum, err := fileSHA256(each.Path)
		if err != nil {
			passed = append(passed, fmt.Sprintf("%s (%v)", each.Path, err))
			continue
		}
		if sum != digest {
			passed = append(passed, fmt.Sprintf("%s (sha256 %s)", each.Path, sum))
			continue
		}
		command := exec.Command(each.Path, "--version")
		command.Dir, command.Env = tb.TempDir(), fixtureenv.Isolated(tb.TempDir())
		out, err := command.Output()
		if err != nil {
			tb.Fatalf("the pinned jj %s at %s: --version: %v", version, each.Path, err)
		}
		tb.Logf("the pinned jj %s (.github/ci-tools.yaml row %s, sha256 %s) is %s: %s", version, row, digest, each.Path, strings.TrimSpace(string(out)))
		return each.Path
	}
	if len(passed) == 0 {
		passed = append(passed, "none")
	}
	tb.Fatalf("PATH holds no jj whose sha256 is %s, the jj %s that .github/ci-tools.yaml (row %s) pins: this test tests that binary, and .github/actions/install-jj installs it. The jj on PATH: %s", digest, version, row, strings.Join(passed, ", "))
	return ""
}

// readJJPin reads the version and digest of row from the .github/ci-tools.yaml of the repository
// the test runs in, found by walking up from the test's working directory.
func readJJPin(row string) (version, digest string, err error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	var path string
	for {
		candidate := filepath.Join(dir, ".github", "ci-tools.yaml")
		if _, err := os.Stat(candidate); err == nil {
			path = candidate
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", fmt.Errorf("no .github/ci-tools.yaml in the test's working directory or any above it")
		}
		dir = parent
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	var tools struct {
		Tools []struct {
			Name    string `yaml:"name"`
			Version string `yaml:"version"`
			Digest  string `yaml:"digest"`
		} `yaml:"tools"`
	}
	if err := yaml.Unmarshal(data, &tools); err != nil {
		return "", "", fmt.Errorf("read %s: %w", path, err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == row {
			if tool.Version == "" || len(tool.Digest) != sha256.Size*2 {
				return "", "", fmt.Errorf("%s's row %s has version %q and digest %q, want a version and a sha256", path, row, tool.Version, tool.Digest)
			}
			return tool.Version, tool.Digest, nil
		}
	}
	return "", "", fmt.Errorf("%s has no row %s", path, row)
}

// fileSHA256 is the hex sha256 of the file at path.
func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
