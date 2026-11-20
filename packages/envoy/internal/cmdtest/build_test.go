package cmdtest

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// recordingTB is a test whose failures are recorded rather than reported, so a test can read
// what BuildCommand reports for a build that must fail.
type recordingTB struct {
	testing.TB
	errors []string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

// A build fetches no module while the test runs: on a module cache that lacks what the build
// needs, it fails with the command that fetches those modules before the tests, and the cache
// is left as empty as it was. The row fails when the build reaches the module proxy - it then
// succeeds where the network answers, and fails with a dial error naming no fetch command where
// it does not - and when the failure stops naming the command.
func TestABuildFetchesNoModuleWhileTheTestRuns(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main.go")
	// A program of this module's own whose one import is a module this module requires, built
	// on an empty module cache: every module it needs is one the cache lacks.
	program := "package main\n\nimport \"github.com/nats-io/nkeys\"\n\nfunc main() { _, _ = nkeys.CreateUser() }\n"
	if err := os.WriteFile(main, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "modcache")
	// The go command of the toolchain running this test, first on PATH and with no toolchain
	// switch, so the empty cache lacks modules and no toolchain.
	path := filepath.Join(runtime.GOROOT(), "bin") + string(os.PathListSeparator) + os.Getenv("PATH")
	env := append(os.Environ(), "GOMODCACHE="+cache, "GOFLAGS=-modcacherw", "GOTOOLCHAIN=local", "PATH="+path)

	recorder := &recordingTB{TB: t}
	binary := runBuild(recorder, Command{Name: "nkeys-user", Target: main, Env: env}, "")
	if binary != "" {
		t.Fatalf("the build on an empty module cache wrote %s; want it refused", binary)
	}
	report := strings.Join(recorder.errors, "\n")
	for _, want := range []string{"module lookup disabled by GOPROXY=off", "a test fetches no module while it runs", ".github/scripts/go-fetch.sh deps "} {
		if !strings.Contains(report, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, report)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(cache, "cache", "download")); err == nil && len(entries) > 0 {
		t.Errorf("the build fetched into the empty module cache: %d entries under cache/download", len(entries))
	}
}
