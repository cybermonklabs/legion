package hostproc_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/hostproc"
	"github.com/sjawhar/legion/daemon/internal/hostproc/hostproctest"
)

// newRecord is an empty record of scans for a child of this test binary, which opens the file
// RecordEnv names as it starts (the wrapper makes it the same way, before the binary runs). The
// child stands for a binary go ran (BinaryEnv), whose scans its chain of goroutines names, unless
// the test starts it as a child of the test (runChild).
func newRecord(t *testing.T) string {
	t.Helper()
	record := filepath.Join(t.TempDir(), "scans")
	if err := os.WriteFile(record, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(hostproc.RecordEnv, record)
	t.Setenv(hostproc.BinaryEnv, "1")
	return record
}

// runChild runs this test binary again on the tests run matches, with this process's environment
// and env, and fails t unless it passes. With binary false it takes BinaryEnv out, so the child is
// what a test's re-run of its own binary is, a child and never the binary go ran.
func runChild(t *testing.T, run string, binary bool, env ...string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(self, "-test.run="+run, "-test.count=1", "-test.v")
	for _, entry := range os.Environ() {
		if binary || !strings.HasPrefix(entry, hostproc.BinaryEnv+"=") {
			child.Env = append(child.Env, entry)
		}
	}
	child.Env = append(child.Env, env...)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("the child failed: %v\n%s", err, output)
	}
}

// recordLines is every line of a record, its fields split at the tabs.
func recordLines(t *testing.T, record string) [][]string {
	t.Helper()
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var lines [][]string
	for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		if line != "" {
			lines = append(lines, strings.Split(line, "\t"))
		}
	}
	return lines
}

// checkOnAGoroutinesGoroutine calls Check on a goroutine started by a goroutine this function
// starts, as a launcher's loops run. It is no function of a test, so neither goroutine's stack names
// one: only the chain of starters, up to the test that called this, can.
func checkOnAGoroutinesGoroutine(scanner, root string) {
	done := make(chan struct{})
	go func() {
		inner := make(chan struct{})
		go func() {
			defer close(inner)
			hostproc.Check(scanner, root, hostproc.Reason{})
		}()
		<-inner
		close(done)
	}()
	<-done
}

// A test binary with a record writes its binary line as it starts, and each scan Check lets list
// once, with the test that made it: on the test's own goroutine, and on a goroutine started by a
// goroutine the test's helper started (checkOnAGoroutinesGoroutine), whose stacks name no test.
// The root of the test's own is the package's directory, where go test runs each test binary: no
// mount of proc, and the same in the child, which starts in its parent's working directory.
func TestEachScanIsRecordedWithTheTestThatMadeIt(t *testing.T) {
	record := newRecord(t)
	own, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	reason := hostproc.Because("it takes only this test's own pid")
	hostproctest.Allowed(t, func() {
		hostproc.Check("the test goroutine's scanner", own, hostproc.Reason{})
		hostproc.Check("the test goroutine's scanner", own, hostproc.Reason{})
		hostproc.Check("the reasoned scanner", "/proc", reason)
		checkOnAGoroutinesGoroutine("the scanner of a goroutine's goroutine", own)
	})
	lines := recordLines(t, record)
	const test = "TestEachScanIsRecordedWithTheTestThatMadeIt"
	want := [][]string{
		{"scan", hostproc.ScanOwn, test, "the test goroutine's scanner", own, ""},
		{"scan", hostproc.ScanReason, test, "the reasoned scanner", "/proc", reason.String()},
		{"scan", hostproc.ScanOwn, test, "the scanner of a goroutine's goroutine", own, ""},
	}
	if len(lines) != 1+len(want) || len(lines[0]) != 2 || lines[0][0] != "binary" || lines[0][1] == "" {
		t.Fatalf("the record holds %q, want the child's binary line and then %q", lines, want)
	}
	for i, line := range lines[1:] {
		if !slices.Equal(line, want[i]) {
			t.Errorf("line %d of the record is %q, want %q", i+2, line, want[i])
		}
	}
}

// A scan Check refuses is recorded before the binary ends, with the test that made it.
func TestARefusedScanIsRecordedBeforeTheBinaryEnds(t *testing.T) {
	record := newRecord(t)
	hostproctest.Refused(t, "the refused scanner", func() {
		hostproc.Check("the refused scanner", "/proc", hostproc.Reason{})
	})
	lines := recordLines(t, record)
	want := []string{"scan", hostproc.ScanRefused, "TestARefusedScanIsRecordedBeforeTheBinaryEnds", "the refused scanner", "/proc", ""}
	if len(lines) != 2 || lines[0][0] != "binary" || !slices.Equal(lines[1], want) {
		t.Fatalf("the record holds %q, want the child's binary line and then %q", lines, want)
	}
}

// A binary given no record writes none: the control of the two above, a scan in a child whose
// record variable names nothing.
func TestABinaryGivenNoRecordWritesNone(t *testing.T) {
	record := newRecord(t)
	t.Setenv(hostproc.RecordEnv, "")
	hostproctest.Allowed(t, func() {
		hostproc.Check("the reasoned scanner", "/proc", hostproc.Because("it takes only this test's own pid"))
	})
	if lines := recordLines(t, record); len(lines) != 0 {
		t.Fatalf("a child with no record wrote %q", lines)
	}
}

// helperEnv is set in the children the tests below start, the one process in which each of the
// helper tests after it scans; elsewhere each returns at once.
const helperEnv = "HOSTPROC_RECORD_TEST_HELPER"

// ownRoot is a root of the test's own: the package's directory, where go test runs each test
// binary, and where a child started with no directory of its own runs too.
func ownRoot(t *testing.T) string {
	t.Helper()
	own, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return own
}

// wantLines fails t unless record holds a binary line and then want, in order.
func wantLines(t *testing.T, record string, want [][]string) {
	t.Helper()
	lines := recordLines(t, record)
	if len(lines) != 1+len(want) || lines[0][0] != "binary" {
		t.Fatalf("the record holds %q, want the child's binary line and then %q", lines, want)
	}
	for i, line := range lines[1:] {
		if !slices.Equal(line, want[i]) {
			t.Errorf("line %d of the record is %q, want %q", i+2, line, want[i])
		}
	}
}

// TestRecordHelperScans is the helper a child below runs under its own name, as a launcher test
// re-runs its binary as role-init: no test of the command, it scans a root of the test's own once.
func TestRecordHelperScans(t *testing.T) {
	if os.Getenv(helperEnv) == "" {
		return
	}
	hostproc.Check("the helper child's scanner", ownRoot(t), hostproc.Reason{})
}

// A scan a child makes, which a test started of its own binary under a helper's name, is recorded
// with the test that started it, which hostproc.ChildEnv tells the child, and not with the helper,
// which is no test the command ran.
func TestAScanInAHelperChildIsNamedByTheTestThatStartedIt(t *testing.T) {
	record := newRecord(t)
	runChild(t, "^TestRecordHelperScans$", false, helperEnv+"=1", hostproc.ChildEnv(t))
	wantLines(t, record, [][]string{
		{"scan", hostproc.ScanOwn, "TestAScanInAHelperChildIsNamedByTheTestThatStartedIt", "the helper child's scanner", ownRoot(t), ""},
	})
}

// A child no test named records its scans with no test, which the wrapper says as UNKNOWN, rather
// than under its helper's name, a test the command did not run.
func TestAScanInAChildNoTestNamedIsRecordedWithNoTest(t *testing.T) {
	record := newRecord(t)
	runChild(t, "^TestRecordHelperScans$", false, helperEnv+"=1")
	wantLines(t, record, [][]string{{"scan", hostproc.ScanOwn, "", "the helper child's scanner", ownRoot(t), ""}})
}

// startLoop starts a loop on a goroutine of its own that scans own once, when the test's cleanup
// stops it, as a launcher's loops make a last scan when a test's helper stops the launcher. With
// seeded, the goroutine that starts the loop first scans own itself, as a launcher's memory guard
// is built by a scan of its root before its loop starts, and returns. Neither goroutine's stack
// names a test, and the test function has returned by the time the loop scans.
func startLoop(t *testing.T, seeded bool, scanner string) {
	own, stop, done := ownRoot(t), make(chan struct{}), make(chan struct{})
	loop := func() {
		defer close(done)
		<-stop
		hostproc.Check(scanner, own, hostproc.Reason{})
	}
	if seeded {
		started := make(chan struct{})
		go func() {
			defer close(started)
			hostproc.Check("the seeding scanner", own, hostproc.Reason{})
			go loop()
		}()
		<-started
	} else {
		go loop()
	}
	t.Cleanup(func() { close(stop); <-done })
}

// TestRecordLoopSeeded and TestRecordLoopUnseeded are the two helpers the test below runs in a
// child that stands for a binary go ran.
func TestRecordLoopSeeded(t *testing.T) {
	if os.Getenv(helperEnv) != "" {
		startLoop(t, true, "the seeded loop's scanner")
	}
}

func TestRecordLoopUnseeded(t *testing.T) {
	if os.Getenv(helperEnv) != "" {
		startLoop(t, false, "the unseeded loop's scanner")
	}
}

// A loop whose one scan comes while its test's cleanup runs, after the test function returned, is
// named by the scan that built it on a goroutine of its chain, which kept its test's name; a loop no
// earlier scan of its chain named is recorded with no test, which the wrapper says as UNKNOWN,
// rather than under TestMain or a test it cannot see.
func TestALoopsScanInItsTestsCleanupIsNamedOnlyThroughAnEarlierScanOfItsChain(t *testing.T) {
	record := newRecord(t)
	runChild(t, "^(TestRecordLoopSeeded|TestRecordLoopUnseeded)$", true, helperEnv+"=1")
	own := ownRoot(t)
	wantLines(t, record, [][]string{
		{"scan", hostproc.ScanOwn, "TestRecordLoopSeeded", "the seeding scanner", own, ""},
		{"scan", hostproc.ScanOwn, "TestRecordLoopSeeded", "the seeded loop's scanner", own, ""},
		{"scan", hostproc.ScanOwn, "", "the unseeded loop's scanner", own, ""},
	})
}

// TestScanPairFirst and TestScanPairSecond are the two tests of one binary the test below runs in
// a child that stands for a binary go ran: both make one same scan, and the second one more.
func TestScanPairFirst(t *testing.T) {
	if os.Getenv(helperEnv) != "" {
		hostproc.Check("the pair's shared scanner", ownRoot(t), hostproc.Reason{})
	}
}

func TestScanPairSecond(t *testing.T) {
	if os.Getenv(helperEnv) != "" {
		hostproc.Check("the pair's shared scanner", ownRoot(t), hostproc.Reason{})
		hostproc.Check("the second's own scanner", ownRoot(t), hostproc.Reason{})
	}
}

// Two tests of one binary that make the same scan are each recorded with it, and a scan of the
// second is recorded with the second: the record holds tests apart, as a CI binary of many tests
// needs, and not only the scans of one test.
func TestEachTestOfOneBinaryIsRecordedWithItsOwnScans(t *testing.T) {
	record := newRecord(t)
	runChild(t, "^(TestScanPairFirst|TestScanPairSecond)$", true, helperEnv+"=1")
	own := ownRoot(t)
	wantLines(t, record, [][]string{
		{"scan", hostproc.ScanOwn, "TestScanPairFirst", "the pair's shared scanner", own, ""},
		{"scan", hostproc.ScanOwn, "TestScanPairSecond", "the pair's shared scanner", own, ""},
		{"scan", hostproc.ScanOwn, "TestScanPairSecond", "the second's own scanner", own, ""},
	})
}

// starterEnv is set in the child the test below starts as the binary go ran, the one process in
// which TestRecordHelperStarter starts a helper of its own.
const starterEnv = "HOSTPROC_RECORD_TEST_STARTER"

// TestRecordHelperStarter is the binary go ran, for the test below: from a subtest, as the
// launcher's tests start role-init, a stage or a PID-one helper, it starts its own binary again
// with its whole environment and ChildEnv, and that helper scans (TestRecordHelperScans).
func TestRecordHelperStarter(t *testing.T) {
	if os.Getenv(starterEnv) == "" {
		return
	}
	t.Run("a subtest", func(t *testing.T) {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		helper := exec.Command(self, "-test.run=^TestRecordHelperScans$", "-test.count=1")
		helper.Env = append(os.Environ(), helperEnv+"=1", hostproc.ChildEnv(t))
		if output, err := helper.CombinedOutput(); err != nil {
			t.Fatalf("the helper failed: %v\n%s", err, output)
		}
	})
}

// A helper that the binary go ran starts of itself, from a subtest, with that binary's whole
// environment, is recorded with the top-level test that started it: the binary took BinaryEnv out
// of its own environment as it started, so its helper is known for a child, and ChildEnv named the
// top-level test, not the subtest. The child here is started as the wrapper starts the binary go
// ran, BinaryEnv set, which runChild with binary false would take out.
func TestAHelperOfTheBinaryGoRanIsNamedByTheTopLevelTestThatStartedIt(t *testing.T) {
	record := newRecord(t)
	runChild(t, "^TestRecordHelperStarter$", true, starterEnv+"=1")
	var scans [][]string
	for _, line := range recordLines(t, record) {
		if line[0] == "scan" {
			scans = append(scans, line)
		}
	}
	want := []string{"scan", hostproc.ScanOwn, "TestRecordHelperStarter", "the helper child's scanner", ownRoot(t), ""}
	if len(scans) != 1 || !slices.Equal(scans[0], want) {
		t.Fatalf("the record's scans are %q, want only %q", scans, want)
	}
}

// ChildEnv names a test's top-level test, from the test itself, a subtest and a subtest of one:
// the test the command ran, by which the wrapper lists a child's scans.
func TestChildEnvNamesTheTopLevelTestFromAnySubtest(t *testing.T) {
	want := hostproc.TestEnv + "=TestChildEnvNamesTheTopLevelTestFromAnySubtest"
	if got := hostproc.ChildEnv(t); got != want {
		t.Errorf("ChildEnv of the test is %q, want %q", got, want)
	}
	t.Run("a subtest", func(t *testing.T) {
		if got := hostproc.ChildEnv(t); got != want {
			t.Errorf("ChildEnv of %s is %q, want %q", t.Name(), got, want)
		}
		t.Run("a subtest of it", func(t *testing.T) {
			if got := hostproc.ChildEnv(t); got != want {
				t.Errorf("ChildEnv of %s is %q, want %q", t.Name(), got, want)
			}
		})
	})
}

// A test binary CI starts with no record of the wrapper's ends before any of its tests runs, with
// RefusalStatus, naming itself: however it was started (su -c, flock -c, a script file), it runs
// outside private_proc.py, on the runner's /proc. The controls are the same binary with the
// wrapper's record, with a record a test set to nothing, outside CI, and listing its tests in CI,
// each of which runs as before.
func TestATestBinaryCIStartsWithNoRecordIsRefusedBeforeAnyTestRuns(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var outside []string
	for _, entry := range os.Environ() {
		switch name, _, _ := strings.Cut(entry, "="); name {
		case hostproc.RecordEnv, hostproc.BinaryEnv, hostproc.TestEnv, hostproc.CIEnv:
		default:
			outside = append(outside, entry)
		}
	}
	run := func(env []string, args ...string) (string, int) {
		child := exec.Command(self, args...)
		child.Env = append(append([]string{}, outside...), env...)
		output, err := child.CombinedOutput()
		var exit *exec.ExitError
		switch {
		case errors.As(err, &exit):
			return string(output), exit.ExitCode()
		case err != nil:
			t.Fatal(err)
		}
		return string(output), 0
	}
	helper := []string{"-test.run=^TestRecordHelperScans$", "-test.count=1", "-test.v"}
	ci := hostproc.CIEnv + "=true"
	output, code := run([]string{ci}, helper...)
	said := hostproc.Refusal + " " + self + " is a test binary that links internal/hostproc, started in CI"
	if code != hostproc.RefusalStatus || !strings.Contains(output, said) || strings.Contains(output, "=== RUN") {
		t.Fatalf("a test binary CI started with no record exited %d and said:\n%s\nwant exit %d, %q and no test run",
			code, output, hostproc.RefusalStatus, said)
	}
	record := filepath.Join(t.TempDir(), "scans")
	if err := os.WriteFile(record, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, control := range []struct {
		name, want string
		env, args  []string
	}{
		{"the wrapper's record", "--- PASS: TestRecordHelperScans", []string{ci, hostproc.RecordEnv + "=" + record, hostproc.BinaryEnv + "=1"}, helper},
		{"a record a test set to nothing", "--- PASS: TestRecordHelperScans", []string{ci, hostproc.RecordEnv + "="}, helper},
		{"outside CI", "--- PASS: TestRecordHelperScans", nil, helper},
		{"listing its tests in CI", "TestRecordHelperScans\n", []string{ci}, []string{"-test.list=^TestRecordHelperScans$"}},
	} {
		if output, code := run(control.env, control.args...); code != 0 || !strings.Contains(output, control.want) || strings.Contains(output, hostproc.Refusal) {
			t.Errorf("with %s the binary exited %d and said:\n%s\nwant exit 0 and %q", control.name, code, output, control.want)
		}
	}
}
