package testbuild

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(Main(m.Run)) }

// A build of an envoy command fetches no module while the test runs: on a module cache that
// lacks what the build needs, it fails with the command that fetches those modules before the
// tests, and the cache is left as empty as it was. The row fails when the build reaches the
// module proxy - it then succeeds where the network answers, and fails with a dial error naming
// no fetch command where it does not - and when the failure stops naming the command.
func TestAnEnvoyBuildFetchesNoModuleWhileTheTestRuns(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "modcache")
	t.Setenv("GOMODCACHE", cache)
	t.Setenv("GOFLAGS", "-modcacherw")
	// The go command of the toolchain running this test, first on PATH and with no toolchain
	// switch, so the empty cache lacks modules and no toolchain.
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("PATH", filepath.Join(runtime.GOROOT(), "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))

	binary, err := buildEnvoy("./cmd/listener")
	if err == nil {
		t.Fatalf("the build on an empty module cache wrote %s; want it refused", binary)
	}
	for _, want := range []string{"module lookup disabled by GOPROXY=off", "a test fetches no module while it runs", ".github/scripts/go-fetch.sh deps "} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(cache, "cache", "download")); err == nil && len(entries) > 0 {
		t.Errorf("the build fetched into the empty module cache: %d entries under cache/download", len(entries))
	}
}
