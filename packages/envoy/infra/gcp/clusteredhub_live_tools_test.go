//go:build natslive

package gcp_test

import (
	"bufio"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// liveToolsFile pins the tools the live tests run, one `go install` argument (a package path and
// its version) per line, at the versions the hub runs; the file says which pin each follows.
// .github/scripts/go-fetch.sh installs them before the tests, and the tests only look them up.
const liveToolsFile = "testdata/live-tools.txt"

// liveToolsDirName is the directory, under the first entry of `go env GOPATH`, that
// `.github/scripts/go-fetch.sh tools` installs the pinned tools into and the tests look them up
// in: a directory of their own rather than GOBIN, which a developer keeps on PATH, so installing
// them replaces no helm or nats-server of the developer's.
const liveToolsDirName = "legion-live-tools"

var liveToolPins = sync.OnceValues(func() (map[string]string, error) { return readLiveTools(liveToolsFile) })

// liveTool is the path of the tool liveToolsFile pins under the name binary, in the live tools'
// directory, where `go-fetch.sh tools` puts it. It installs nothing and so downloads nothing: a
// fetch from the module proxy or the checksum database while a test runs would fail the test
// whenever that service fails, on a change that touched nothing it tests. A tool that is absent,
// or that is another build than its line, fails the test with the command that installs it.
func liveTool(t testing.TB, binary string) string {
	t.Helper()
	pins, err := liveToolPins()
	require.NoError(t, err)
	path, err := lookupLiveTool(t.TempDir(), pins, binary)
	require.NoError(t, err)
	t.Logf("%s: %s, the build %s pins", binary, path, pins[binary])
	return path
}

// lookupLiveTool is what liveTool does, with its failure returned: the path of the build pins
// holds for binary in the live tools' directory, or why not. The one go command it runs is `go
// env GOPATH`, in scratch.
func lookupLiveTool(scratch string, pins map[string]string, binary string) (string, error) {
	pin, ok := pins[binary]
	if !ok {
		return "", fmt.Errorf("%s pins no tool named %s", liveToolsFile, binary)
	}
	dir, err := liveToolsDir(scratch)
	if err != nil {
		return "", err
	}
	return findLiveTool(dir, binary, pin)
}

// findLiveTool is the path of binary in dir when its build information names pin's package and
// version, and otherwise why not, with the command that installs pin there.
func findLiveTool(dir, binary, pin string) (string, error) {
	path := filepath.Join(dir, binary)
	install := "Install it before the tests, as CI's envoy-infra-go does:\n  GOBIN=" + dir + " go install " + pin +
		"\nor install every tool the live tests run, from the repository root:\n" +
		"  .github/scripts/go-fetch.sh tools packages/envoy/infra/gcp/" + liveToolsFile
	info, err := buildinfo.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("%s is not installed in %s, the live tools' directory, and the live tests "+
			"download nothing while they run.\n%s", binary, dir, install)
	case err != nil:
		return "", fmt.Errorf("read the build information of %s: %w\n%s", path, err, install)
	}
	if got := info.Path + "@" + info.Main.Version; got != pin {
		return "", fmt.Errorf("%s is %s, not %s, the build %s pins.\n%s", path, got, pin, liveToolsFile, install)
	}
	return path, nil
}

// liveToolsDir is liveToolsDirName under the first entry of GOPATH, as go-fetch.sh's tools mode
// computes it. It asks the go command, which also reads `go env -w` settings, in dir, a
// directory outside any module.
func liveToolsDir(dir string) (string, error) {
	// go-offline: go env of one setting, in a directory outside any module, so it loads no module
	// and no go.mod's toolchain line can make it fetch a toolchain.
	cmd := exec.Command("go", "env", "GOPATH")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go env GOPATH: %w", err)
	}
	gopath := filepath.SplitList(strings.TrimSpace(string(out)))
	if len(gopath) == 0 || gopath[0] == "" {
		return "", fmt.Errorf("go env GOPATH names no directory, so the live tools' directory is unknown")
	}
	return filepath.Join(gopath[0], liveToolsDirName), nil
}

// readLiveTools reads a list of pins, liveToolsFile's form, into each pin by the name of the
// binary it installs. A line that is no pin, and two pins of one binary, are refused.
func readLiveTools(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	pins := map[string]string{}
	scanner := bufio.NewScanner(f)
	for n := 1; scanner.Scan(); n++ {
		line, _, _ := strings.Cut(scanner.Text(), "#")
		pin := strings.TrimSpace(line)
		if pin == "" {
			continue
		}
		binary, err := liveToolBinary(pin)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		if earlier, ok := pins[binary]; ok {
			return nil, fmt.Errorf("%s:%d: two lines install tool %s: %s and %s", path, n, binary, earlier, pin)
		}
		pins[binary] = pin
	}
	return pins, scanner.Err()
}

var (
	pinPattern     = regexp.MustCompile(`^([^\s@]+)@(v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.+-]+)?)$`)
	versionElement = regexp.MustCompile(`^v[1-9][0-9]*$`)
)

// liveToolBinary is the name `go install` gives pin's binary: the last element of its package
// path, or the one before it when the last is a major version (cmd/go's DefaultExecName). A pin
// is a package path and a semantic version; a query such as @latest is refused, since the tests
// check the build against the version their line names.
func liveToolBinary(pin string) (string, error) {
	m := pinPattern.FindStringSubmatch(pin)
	if m == nil {
		return "", fmt.Errorf("%q is not a package path and a semantic version, path@vX.Y.Z", pin)
	}
	elems := strings.Split(m[1], "/")
	name := elems[len(elems)-1]
	if len(elems) > 1 && versionElement.MatchString(name) {
		name = elems[len(elems)-2]
	}
	return name, nil
}

// The tool lookup the live tests run their tools through: a pinned tool is taken from the live
// tools' directory only when its binary is the build its line pins, and a tool that is absent or
// another build is refused with the command that installs it. The lookup rows run lookupLiveTool,
// and the liveTool row liveTool itself, through a testing.TB that records the failure liveTool
// would fail its test with; each under a stand-in go, first on PATH and as GOROOT's bin/go, that
// answers `go env GOPATH` with the row's own directory, records every call and runs nothing else.
// So the rows fail when the lookup accepts a binary by its name alone (the another-build row),
// when the lookup, or liveTool before or after it, runs any go command but `go env GOPATH` - an
// install of what is missing among them, by the go on PATH or the toolchain's by its path - or
// installs anything into the directory by another way (the absent rows: their tool is nats-server
// at its real pin, which a `go install` from the module cache the fetch step filled would put
// there), when liveTool stops failing its test on an absent tool, and when the message stops
// naming the install command.
func TestALiveToolIsTakenFromItsDirectoryOnlyAtItsPin(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	info, err := buildinfo.ReadFile(self)
	require.NoError(t, err)
	// The test binary stands in for a tool: its build information names its main package and
	// that module's version, as a `go install` of a pinned tool's does.
	pin := info.Path + "@" + info.Main.Version
	name := filepath.Base(info.Path)

	t.Run("the pinned build", func(t *testing.T) {
		dir := t.TempDir()
		copyFile(t, self, filepath.Join(dir, name))
		path, err := findLiveTool(dir, name, pin)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(dir, name), path)
	})
	t.Run("another build", func(t *testing.T) {
		dir := t.TempDir()
		copyFile(t, self, filepath.Join(dir, name))
		other := info.Path + "@v0.0.1"
		_, err := findLiveTool(dir, name, other)
		require.ErrorContains(t, err, pin+", not "+other)
		require.ErrorContains(t, err, "GOBIN="+dir+" go install "+other)
	})
	t.Run("the lookup takes the pinned build from the directory under GOPATH's first entry", func(t *testing.T) {
		gopath, calls := standInGo(t)
		dir := filepath.Join(gopath, liveToolsDirName)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		copyFile(t, self, filepath.Join(dir, name))
		path, err := lookupLiveTool(t.TempDir(), map[string]string{name: pin}, name)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(dir, name), path)
		require.Equal(t, []string{"env GOPATH"}, calls(), "the lookup's go commands")
	})
	t.Run("the lookup refuses an absent tool and installs nothing", func(t *testing.T) {
		pins, err := liveToolPins()
		require.NoError(t, err)
		absent := pins["nats-server"]
		require.NotEmpty(t, absent, "%s pins nats-server", liveToolsFile)
		gopath, calls := standInGo(t)
		dir := filepath.Join(gopath, liveToolsDirName)
		_, err = lookupLiveTool(t.TempDir(), pins, "nats-server")
		require.ErrorContains(t, err, "nats-server is not installed in "+dir)
		require.ErrorContains(t, err, "GOBIN="+dir+" go install "+absent)
		require.ErrorContains(t, err, ".github/scripts/go-fetch.sh tools packages/envoy/infra/gcp/"+liveToolsFile)
		require.Equal(t, []string{"env GOPATH"}, calls(), "the lookup's go commands: it installs nothing")
		entries, err := os.ReadDir(dir)
		if !errors.Is(err, fs.ErrNotExist) {
			require.NoError(t, err)
		}
		require.Empty(t, entries, "the lookup installs nothing into %s", dir)
	})
	t.Run("liveTool fails its test on an absent tool and installs nothing", func(t *testing.T) {
		pins, err := liveToolPins()
		require.NoError(t, err)
		absent := pins["nats-server"]
		require.NotEmpty(t, absent, "%s pins nats-server", liveToolsFile)
		gopath, calls := standInGo(t)
		dir := filepath.Join(gopath, liveToolsDirName)
		path, failed := runLiveTool(t, "nats-server")
		require.Empty(t, path, "liveTool returns no path for an absent tool")
		message := strings.Join(failed, "\n")
		require.Contains(t, message, "nats-server is not installed in "+dir, "liveTool fails its test, naming the tool")
		require.Contains(t, message, "GOBIN="+dir+" go install "+absent)
		require.Contains(t, message, ".github/scripts/go-fetch.sh tools packages/envoy/infra/gcp/"+liveToolsFile)
		require.Equal(t, []string{"env GOPATH"}, calls(), "liveTool's go commands: it installs nothing")
		entries, err := os.ReadDir(dir)
		if !errors.Is(err, fs.ErrNotExist) {
			require.NoError(t, err)
		}
		require.Empty(t, entries, "liveTool installs nothing into %s", dir)
	})
	t.Run("the lookup refuses a name the list does not pin, before any go command", func(t *testing.T) {
		_, calls := standInGo(t)
		_, err := lookupLiveTool(t.TempDir(), map[string]string{name: pin}, "absent-tool")
		require.ErrorContains(t, err, liveToolsFile+" pins no tool named absent-tool")
		require.Empty(t, calls(), "the lookup's go commands")
	})
}

// standInGo puts a go first on PATH, and as the bin/go of GOROOT, that answers `go env GOPATH`
// with a directory of the test's own, then a second one, records each call's arguments, and fails
// every other call without running it: so a go command run by its name or by the toolchain's path
// is recorded alike. It returns the first directory and the calls so far.
func standInGo(t *testing.T) (string, func() []string) {
	t.Helper()
	root, gopath := t.TempDir(), t.TempDir()
	bin := filepath.Join(root, "bin")
	record := filepath.Join(t.TempDir(), "calls")
	script := "#!/bin/sh\necho \"$*\" >> \"$LIVE_TOOL_TEST_CALLS\"\n" +
		"if [ \"$*\" = \"env GOPATH\" ]; then echo \"$LIVE_TOOL_TEST_GOPATH\"; exit 0; fi\n" +
		"echo \"the stand-in go runs nothing but go env GOPATH: go $*\" >&2\nexit 1\n"
	require.NoError(t, os.Mkdir(bin, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOROOT", root)
	t.Setenv("LIVE_TOOL_TEST_CALLS", record)
	t.Setenv("LIVE_TOOL_TEST_GOPATH", gopath+string(os.PathListSeparator)+t.TempDir())
	return gopath, func() []string {
		b, err := os.ReadFile(record)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		require.NoError(t, err)
		return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	}
}

// runLiveTool runs liveTool for binary as a live test does, but through failures in a goroutine of
// its own, and returns the path liveTool returned and what it would have failed its test with.
func runLiveTool(t *testing.T, binary string) (string, []string) {
	t.Helper()
	recorded := &failures{TB: t}
	var path string
	done := make(chan struct{})
	go func() {
		defer close(done)
		path = liveTool(recorded, binary)
	}()
	<-done
	return path, recorded.messages
}

// failures is a testing.TB that records each failure rather than failing the test, and ends the
// goroutine of a call that would stop the test, as testing.T's FailNow ends the test's own.
type failures struct {
	testing.TB
	messages []string
}

func (f *failures) Helper() {}

func (f *failures) Fail() { f.messages = append(f.messages, "Fail") }

func (f *failures) Error(args ...any) { f.messages = append(f.messages, fmt.Sprint(args...)) }

func (f *failures) Errorf(format string, args ...any) {
	f.messages = append(f.messages, fmt.Sprintf(format, args...))
}

func (f *failures) Fatal(args ...any) { f.Error(args...); f.FailNow() }

func (f *failures) Fatalf(format string, args ...any) { f.Errorf(format, args...); f.FailNow() }

func (f *failures) FailNow() { runtime.Goexit() }

// A tool's binary is the last element of its package path, or the one before a major-version
// suffix, as `go install` names it; the pinned list holds the four tools the live tests run.
func TestTheLiveToolsListPinsEachToolTheTestsRun(t *testing.T) {
	for pin, want := range map[string]string{
		"helm.sh/helm/v3/cmd/helm@v3.20.2":                                "helm",
		"github.com/nats-io/nats-server/v2@v2.15.1":                       "nats-server",
		"github.com/nats-io/nack/cmd/nats-server-config-reloader@v0.24.0": "nats-server-config-reloader",
		"github.com/nats-io/prometheus-nats-exporter@v0.20.2":             "prometheus-nats-exporter",
	} {
		got, err := liveToolBinary(pin)
		require.NoError(t, err)
		require.Equal(t, want, got, pin)
	}
	for _, bad := range []string{"helm.sh/helm/v3/cmd/helm", "helm.sh/helm/v3/cmd/helm@latest", "@v1.0.0", "a b@v1"} {
		_, err := liveToolBinary(bad)
		require.Error(t, err, bad)
	}

	pins, err := readLiveTools(liveToolsFile)
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"helm":                        "helm.sh/helm/v3/cmd/helm@v3.20.2",
		"nats-server":                 "github.com/nats-io/nats-server/v2@v2.15.1",
		"nats-server-config-reloader": "github.com/nats-io/nack/cmd/nats-server-config-reloader@v0.24.0",
		"prometheus-nats-exporter":    "github.com/nats-io/prometheus-nats-exporter@v0.20.2",
	}, pins)

	dir := t.TempDir()
	twice := filepath.Join(dir, "twice.txt")
	require.NoError(t, os.WriteFile(twice, []byte("example.com/a/tool@v1.0.0\nexample.com/b/tool@v2.0.0\n"), 0o600))
	_, err = readLiveTools(twice)
	require.ErrorContains(t, err, "two lines install tool")
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(to, b, 0o700))
}
