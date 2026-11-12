package launcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// leakCheckPin is the pin these tests give the launcher's environment, spelled as the worker
// image spells one.
const leakCheckPin = "leak-check-v15"

// pinLeakCheck gives the launcher a test starts after it the pin tag in its own environment,
// which fixtureenv takes out of the test process, and holds that the environment names no gh
// directory and no GitHub token: the launcher installs only what the daemon pushes it.
func pinLeakCheck(t *testing.T, tag string) {
	t.Helper()
	t.Setenv(leakguard.TagVariable, tag)
	for _, name := range []string{"LEGION_GH_OWNERS_DIR", "GH_CONFIG_DIR", "GH_TOKEN", "GITHUB_TOKEN"} {
		if value, ok := os.LookupEnv(name); ok {
			t.Fatalf("the test process carries %s=%q, through which an install could read GitHub", name, value)
		}
	}
}

// packedLeakCheck is what the daemon serves of the check's repository: a tarball shaped as
// GitHub's (every member under one `<owner>-<repo>-<sha>` directory, the check's own under its
// leak-check directory, one file of the repository outside it), whose check is launcher, with a
// python file beside it and a fixture of incompressible bytes large enough that the packed body
// takes two slices, packed as the daemon packs it (leakguard.PackTarball).
func packedLeakCheck(t *testing.T, launcher string) []byte {
	t.Helper()
	fixture := make([]byte, shimwire.LauncherBundlePayloadBytes+4096)
	_, _ = rand.NewChaCha8([32]byte{}).Read(fixture)
	raw := leakguardtest.GitHubTarball(t, map[string]leakguardtest.Member{
		leakguardtest.CheckName:  {Body: "#!/bin/sh\n" + launcher, Mode: 0o755},
		"public_leak_check.py":   {Body: "# the check itself\n", Mode: 0o644},
		"fixtures/must-pass.bin": {Body: string(fixture), Mode: 0o644},
	})
	body, files, err := leakguard.PackTarball(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("pack the test's tarball: %v", err)
	}
	if files != 3 {
		t.Fatalf("the pack holds %d files, want the check's 3", files)
	}
	return body
}

// slicedLeakCheck is the launcher_leak_check sequence the daemon pushes of body at tag.
func slicedLeakCheck(t *testing.T, tag string, body []byte) []shimwire.LauncherLeakCheck {
	t.Helper()
	frames, err := shimwire.SplitLeakCheck(tag, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) < 2 {
		t.Fatalf("the check sliced into %d frames; the test needs several", len(frames))
	}
	return frames
}

// servedLeakCheck is the sequence of a check that answers its --help, counting each run in the
// file runs, packed and sliced at tag.
func servedLeakCheck(t *testing.T, tag, runs string) []shimwire.LauncherLeakCheck {
	t.Helper()
	return slicedLeakCheck(t, tag, packedLeakCheck(t, leakguardtest.AnswersHelp(runs)))
}

// pushLeakCheck sends every slice of frames on conn and answers the launcher's result for each,
// in order, each answering the slice it was sent for.
func pushLeakCheck(t *testing.T, conn *roleConn, frames []shimwire.LauncherLeakCheck) []shimwire.LauncherLeakCheckResult {
	t.Helper()
	results := make([]shimwire.LauncherLeakCheckResult, 0, len(frames))
	for _, frame := range frames {
		got, ok := conn.answer(frame).(shimwire.LauncherLeakCheckResult)
		if !ok {
			t.Fatalf("slice %d was answered %#v, want a launcher_leak_check_result", frame.Index, got)
		}
		if got.ID != frame.ID || got.Index != frame.Index {
			t.Fatalf("slice %d of %s was answered for slice %d of %s", frame.Index, frame.ID, got.Index, got.ID)
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("slice %d was answered %#v, which the daemon refuses: %v", frame.Index, got, err)
		}
		results = append(results, got)
	}
	return results
}

// requireInstalled holds that a whole sequence was installed at its last slice: every slice
// before it taken, and the last answered Installed.
func requireInstalled(t *testing.T, results []shimwire.LauncherLeakCheckResult, log string) {
	t.Helper()
	for index, got := range results {
		if want := index == len(results)-1; !got.OK || got.Installed != want || got.Error != "" {
			t.Fatalf("slice %d of %d was answered %#v, want OK and installed=%v; the launcher's log:\n%s",
				index, len(results), got, want, log)
		}
	}
}

// requireRefused holds that result refuses its slice with the pod's cause sentence, and answers it.
func requireRefused(t *testing.T, got shimwire.LauncherLeakCheckResult) string {
	t.Helper()
	if got.OK || got.Installed || !strings.HasPrefix(got.Error, leakguard.RefusalPrefix) || strings.Contains(got.Error, "\n") {
		t.Fatalf("the launcher answered %#v, want a refusal whose cause is one line beginning %q", got, leakguard.RefusalPrefix)
	}
	return got.Error
}

// helpRuns is how many times a check of leakguardtest.AnswersHelp ran: one line in runs each.
func helpRuns(t *testing.T, runs string) int {
	t.Helper()
	body, err := os.ReadFile(runs)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(body), "\n")
}

// installedLine is the one line the launcher logs of an install, the line the production check
// of a pod reads.
func installedLine(dir string, frames []shimwire.LauncherLeakCheck) string {
	return fmt.Sprintf("launcher: installed the shared public leak check %s at %s, served by the daemon (sha256 %s)",
		frames[0].Tag, dir, frames[0].SHA256[:12])
}

// installLines is every line of log that says the launcher installed the check.
func installLines(log string) []string {
	var lines []string
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "launcher: installed the shared public leak check") {
			lines = append(lines, line)
		}
	}
	return lines
}

// requireInstalledAt holds that dir holds the check of pin, read the way every role's shim reads it.
func requireInstalledAt(t *testing.T, dir, pin string) {
	t.Helper()
	recorded, err := os.ReadFile(filepath.Join(dir, ".installed-tag"))
	if err != nil || string(recorded) != pin+"\n" {
		t.Fatalf("the install's .installed-tag holds %q (%v), want %q", recorded, err, pin+"\n")
	}
	check, err := leakguard.Installed([]string{leakguard.TagVariable + "=" + pin}, dir)
	if err != nil || check != filepath.Join(dir, leakguardtest.CheckName) {
		t.Fatalf("a role's shim reads the install as %q, %v; want the check at %s", check, err, dir)
	}
}

// tree is every entry under root with its mode and, for a file, its bytes, so a test reads
// whether a refused install left the directory and its parent as they were.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	entries := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		value := info.Mode().String()
		if info.Mode().IsRegular() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += " " + string(body)
		}
		entries[rel] = value
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return entries
}

// The launcher names its own pin in every hello, so the daemon knows to push it the check; a
// launcher whose environment names no pin says nothing of a check at all, and refuses a check
// pushed to it anyway, writing nothing.
func TestTheHelloNamesTheLaunchersLeakCheckPin(t *testing.T) {
	t.Run("a pin", func(t *testing.T) {
		pinLeakCheck(t, leakCheckPin)
		g := newSessionRig(t, Config{LeakCheckDir: filepath.Join(t.TempDir(), "leak-check")})
		if g.hello.LeakCheckTag != leakCheckPin || g.hello.LeakCheckInstalled {
			t.Fatalf("the hello names leakCheckTag %q installed=%v, want the pin %s not yet installed",
				g.hello.LeakCheckTag, g.hello.LeakCheckInstalled, leakCheckPin)
		}
	})
	t.Run("no pin", func(t *testing.T) {
		pinLeakCheck(t, "")
		parent := t.TempDir()
		g := newSessionRig(t, Config{LeakCheckDir: filepath.Join(parent, "leak-check")})
		if g.hello.LeakCheckTag != "" || g.hello.LeakCheckInstalled {
			t.Fatalf("the hello names leakCheckTag %q installed=%v, want neither", g.hello.LeakCheckTag, g.hello.LeakCheckInstalled)
		}
		frames := servedLeakCheck(t, leakCheckPin, filepath.Join(t.TempDir(), "help-runs"))
		got := pushLeakCheck(t, g.roleConn, frames[:1])[0]
		want := leakguard.Cause("the pod's launcher was served "+leakCheckPin+", and its environment names no pin ("+leakguard.TagVariable+")", nil)
		if requireRefused(t, got) != want {
			t.Fatalf("the refusal is %q, want %q", got.Error, want)
		}
		if entries := tree(t, parent); len(entries) != 1 {
			t.Fatalf("a launcher with no pin wrote %v", slices.Sorted(maps.Keys(entries)))
		}
	})
}

// The controller's pod is the single-role form: its one launcher takes the pushed check, installs
// it where every role's shim reads it, logs the one line a pod's production check reads, and
// answers Installed at the last slice; the connection goes on serving starts. A reconnection of
// the same process says it is installed, and a sequence pushed again on it is answered Installed
// at once with no second install.
func TestTheSingleRoleLauncherInstallsThePushedCheckOnce(t *testing.T) {
	pinLeakCheck(t, leakCheckPin)
	dir := filepath.Join(t.TempDir(), "leak-check")
	runs := filepath.Join(t.TempDir(), "help-runs")
	g := newSessionRig(t, Config{LeakCheckDir: dir})
	frames := servedLeakCheck(t, leakCheckPin, runs)

	requireInstalled(t, pushLeakCheck(t, g.roleConn, frames), g.stderr.String())
	requireInstalledAt(t, dir, leakCheckPin)
	if got := helpRuns(t, runs); got != 1 {
		t.Fatalf("the check's --help ran %d times, want the install's one verification", got)
	}
	if lines, want := installLines(g.stderr.String()), installedLine(dir, frames); len(lines) != 1 || lines[0] != want {
		t.Fatalf("the launcher logged %q, want exactly %q", lines, want)
	}
	marker := filepath.Join(t.TempDir(), "marker")
	if result := g.start(g.childStart(1, marker, nil)); !result.OK {
		t.Fatalf("after the install the start answered %#v", result)
	}
	g.stop(1)

	_ = g.conn.Close()
	g.accept()
	if g.hello.LeakCheckTag != leakCheckPin || !g.hello.LeakCheckInstalled {
		t.Fatalf("the reconnect hello names leakCheckTag %q installed=%v, want %s installed",
			g.hello.LeakCheckTag, g.hello.LeakCheckInstalled, leakCheckPin)
	}
	if got := pushLeakCheck(t, g.roleConn, frames[:1])[0]; !got.OK || !got.Installed {
		t.Fatalf("the first slice of the installed check pushed again answered %#v, want Installed at once", got)
	}
	if got := helpRuns(t, runs); got != 1 {
		t.Fatalf("the check's --help ran %d times, want the one install's alone", got)
	}
}

// What a launcher process installed is its own: a new Run over a directory an earlier one
// installed into says it has installed nothing, and installs over that install when pushed.
func TestANewLauncherProcessInstallsOverTheInstallItFinds(t *testing.T) {
	pinLeakCheck(t, leakCheckPin)
	dir := filepath.Join(t.TempDir(), "leak-check")
	runs := filepath.Join(t.TempDir(), "help-runs")
	frames := servedLeakCheck(t, leakCheckPin, runs)
	first := newSessionRig(t, Config{LeakCheckDir: dir})
	requireInstalled(t, pushLeakCheck(t, first.roleConn, frames), first.stderr.String())
	first.cancel()

	again := newSessionRig(t, Config{LeakCheckDir: dir})
	if again.hello.LeakCheckTag != leakCheckPin || again.hello.LeakCheckInstalled {
		t.Fatalf("the new launcher's hello names leakCheckTag %q installed=%v, want %s not installed by it",
			again.hello.LeakCheckTag, again.hello.LeakCheckInstalled, leakCheckPin)
	}
	requireInstalled(t, pushLeakCheck(t, again.roleConn, frames), again.stderr.String())
	requireInstalledAt(t, dir, leakCheckPin)
	if got := helpRuns(t, runs); got != 2 {
		t.Fatalf("the check's --help ran %d times, want one for each launcher process's install", got)
	}
	log := again.stderr.String()
	if lines, want := installLines(log), installedLine(dir, frames); len(lines) != 1 || lines[0] != want {
		t.Fatalf("the new launcher logged %q, want exactly %q", lines, want)
	}
	if replaced := "launcher: the install of " + leakCheckPin + " replaced the install of " + leakCheckPin + " at " + dir; !strings.Contains(log, replaced+"\n") {
		t.Fatalf("the new launcher's log does not say %q:\n%s", replaced, log)
	}
}

// An issue pod's one container installs the check once for all its roles: the daemon pushes it
// on the first role's connection, the second role's first slice is answered Installed, the check
// is verified once, and a role that reconnects says it is installed. Nothing of a role's
// namespaces is needed, since no generation starts.
func TestTheRolesOfOneContainerInstallTheCheckOnce(t *testing.T) {
	pinLeakCheck(t, leakCheckPin)
	dir := filepath.Join(t.TempDir(), "leak-check")
	runs := filepath.Join(t.TempDir(), "help-runs")
	f := newFleetOf(t, Config{StopGrace: 2 * time.Second, LeakCheckDir: dir}, "tester", "reviewer")
	for _, role := range []string{"tester", "reviewer"} {
		if hello := f.role(role).hello; hello.LeakCheckTag != leakCheckPin || hello.LeakCheckInstalled {
			t.Fatalf("role %s's hello names leakCheckTag %q installed=%v, want %s not yet installed",
				role, hello.LeakCheckTag, hello.LeakCheckInstalled, leakCheckPin)
		}
	}
	frames := servedLeakCheck(t, leakCheckPin, runs)
	requireInstalled(t, pushLeakCheck(t, f.role("tester"), frames), f.stderr.String())
	if got := pushLeakCheck(t, f.role("reviewer"), frames[:1])[0]; !got.OK || !got.Installed {
		t.Fatalf("the reviewer's first slice answered %#v, want Installed: the tester's connection installed it", got)
	}
	requireInstalledAt(t, dir, leakCheckPin)
	if got := helpRuns(t, runs); got != 1 {
		t.Fatalf("the check's --help ran %d times for two roles, want once for the container", got)
	}
	if lines, want := installLines(f.stderr.String()), "tester: "+installedLine(dir, frames); len(lines) != 1 || lines[0] != want {
		t.Fatalf("the container logged %q, want exactly %q", lines, want)
	}

	_ = f.role("reviewer").conn.Close()
	again := acceptRole(t, f.listener)
	if again.hello.Role != "reviewer" || again.hello.LeakCheckTag != leakCheckPin || !again.hello.LeakCheckInstalled {
		t.Fatalf("the reconnect hello is %#v, want the reviewer saying %s is installed", again.hello, leakCheckPin)
	}
	again.acknowledge()
	f.conns["reviewer"] = again
}

// Two roles' connections may carry the same sequence at once. The install runs under one lock:
// whichever slice makes the body whole installs it, every other slice of either caller finds it
// installed, both sequences end Installed, and the check is verified once.
func TestTwoCallersOfOneCheckInstallInstallItOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "leak-check")
	runs := filepath.Join(t.TempDir(), "help-runs")
	frames := servedLeakCheck(t, leakCheckPin, runs)
	c := newCheckInstall(leakCheckPin, dir)
	var mu sync.Mutex
	var logged []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	begin := make(chan struct{})
	last := make([]shimwire.LauncherLeakCheckResult, 2)
	var callers sync.WaitGroup
	for caller := range last {
		callers.Go(func() {
			<-begin
			for _, frame := range frames {
				got := c.receive(context.Background(), frame, logf)
				if !got.OK {
					t.Errorf("caller %d's slice %d answered %#v", caller, frame.Index, got)
				}
				last[caller] = got
			}
		})
	}
	close(begin)
	callers.Wait()
	for caller, got := range last {
		if !got.Installed {
			t.Errorf("caller %d's last slice answered %#v, want Installed", caller, got)
		}
	}
	if got := helpRuns(t, runs); got != 1 {
		t.Fatalf("the check's --help ran %d times for two callers, want once", got)
	}
	if want := installedLine(dir, frames); len(logged) != 1 || logged[0] != want {
		t.Fatalf("the install logged %q, want exactly %q", logged, want)
	}
	requireInstalledAt(t, dir, leakCheckPin)
	if tag, installed := c.hello(); tag != leakCheckPin || !installed {
		t.Fatalf("after the install a hello says %q installed=%v, want %s installed", tag, installed, leakCheckPin)
	}
}

// A check that cannot be installed is refused with the pod's own cause sentence, and the
// directory, which holds an earlier install, is left as it was with nothing beside it: a body
// that did not arrive as it was sent (and a retry of the true one then installs, since the
// refused slices were dropped), a tag that is not the launcher's pin, and a check whose --help
// does not answer.
func TestARefusedLeakCheckLeavesTheDirectoryAsItWas(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames func(t *testing.T, runs string) []shimwire.LauncherLeakCheck
		want   func(frames []shimwire.LauncherLeakCheck) string
		retry  bool
	}{
		{
			name: "a body whose digest does not match",
			frames: func(t *testing.T, runs string) []shimwire.LauncherLeakCheck {
				frames := slices.Clone(servedLeakCheck(t, leakCheckPin, runs))
				payload := slices.Clone(frames[1].Payload)
				payload[0] ^= 0xff
				frames[1].Payload = payload
				return frames
			},
			want: func(frames []shimwire.LauncherLeakCheck) string {
				return "not the " + frames[0].SHA256 + " its slices declared"
			},
			retry: true,
		},
		{
			name: "a tag that is not the pin",
			frames: func(t *testing.T, runs string) []shimwire.LauncherLeakCheck {
				return servedLeakCheck(t, "leak-check-v16", runs)
			},
			want: func([]shimwire.LauncherLeakCheck) string {
				return leakguard.Cause("the pod's launcher was served leak-check-v16, not its own pin "+leakCheckPin, nil)
			},
		},
		{
			name: "a check whose --help does not answer",
			frames: func(t *testing.T, runs string) []shimwire.LauncherLeakCheck {
				return slicedLeakCheck(t, leakCheckPin, packedLeakCheck(t, leakguardtest.CannotFinish))
			},
			want: func([]shimwire.LauncherLeakCheck) string {
				return "the served check did not answer: its --help names no check-public-text shape: LEAK CHECK SETUP ERROR: python3 did not start"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pinLeakCheck(t, leakCheckPin)
			parent := t.TempDir()
			dir := filepath.Join(parent, "leak-check")
			earlier := packedLeakCheck(t, leakguardtest.AnswersHelp(filepath.Join(t.TempDir(), "earlier-runs")))
			if _, err := leakguard.InstallServed(context.Background(), leakguard.ServedInstall{Dir: dir, Tag: leakCheckPin, Body: earlier}); err != nil {
				t.Fatal(err)
			}
			before := tree(t, parent)
			runs := filepath.Join(t.TempDir(), "help-runs")
			frames := tc.frames(t, runs)
			g := newSessionRig(t, Config{LeakCheckDir: dir})
			results := pushLeakCheck(t, g.roleConn, frames)
			cause := requireRefused(t, results[len(results)-1])
			if want := tc.want(frames); !strings.Contains(cause, want) {
				t.Fatalf("the refusal is %q, want it to say %q", cause, want)
			}
			if after := tree(t, parent); !maps.Equal(after, before) {
				t.Fatalf("the refused install left %v where %v was", slices.Sorted(maps.Keys(after)), slices.Sorted(maps.Keys(before)))
			}
			if lines := installLines(g.stderr.String()); len(lines) != 0 {
				t.Fatalf("the launcher logged %q of an install it refused", lines)
			}
			if !tc.retry {
				return
			}
			requireInstalled(t, pushLeakCheck(t, g.roleConn, servedLeakCheck(t, leakCheckPin, runs)), g.stderr.String())
			requireInstalledAt(t, dir, leakCheckPin)
		})
	}
}
