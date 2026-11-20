package hostproc

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// useRecord points this process's record at a file of the test's own until the test's end, as the
// record of the binary go ran, and answers a reader of the lines written to it since.
func useRecord(t *testing.T) func() []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scans")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	recorder.mu.Lock()
	previousFile, previousSeen, previousTests := recorder.file, recorder.seen, recorder.tests
	previousChild, previousStarter := recorder.child, recorder.starter
	recorder.file, recorder.seen, recorder.tests = file, map[string]bool{}, map[uint64]string{}
	recorder.child, recorder.starter = false, ""
	recorder.mu.Unlock()
	t.Cleanup(func() {
		recorder.mu.Lock()
		recorder.file, recorder.seen, recorder.tests = previousFile, previousSeen, previousTests
		recorder.child, recorder.starter = previousChild, previousStarter
		recorder.mu.Unlock()
		_ = file.Close()
	})
	return func() []string {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	}
}

// Each scan is recorded as Check judged it, with the kind the same classify gave that decided the
// refusal: a root that is no mount of proc as the test's own, a mount of proc read by a reason as
// that, and a refusal as one, which Check also ends the binary on; the /proc of a namespace this
// process is PID 1 of (the one way past a refusal with no reason, which this binary is not) as
// that; each once, however often a loop makes it, and its text kept to one line.
func TestEachScanIsRecordedAsCheckJudgedIt(t *testing.T) {
	lines := useRecord(t)
	var exited []int
	previousExit := exit
	t.Cleanup(func() { exit = previousExit })
	exit = func(code int) { exited = append(exited, code) }
	own := t.TempDir()
	Check("the own scanner", own, Reason{})
	Check("the own scanner", own, Reason{})
	Check("the reasoned scanner", "/proc", Because("it reads\tits own pid\nalone"))
	record("the namespace's scanner", "/proc", Reason{}, ScanNamespace)
	Check("the refused scanner", "/proc", Reason{})
	const test = "TestEachScanIsRecordedAsCheckJudgedIt"
	want := []string{
		"scan\town\t" + test + "\tthe own scanner\t" + own + "\t",
		"scan\treason\t" + test + "\tthe reasoned scanner\t/proc\tit reads its own pid alone",
		"scan\tnamespace\t" + test + "\tthe namespace's scanner\t/proc\t",
		"scan\trefused\t" + test + "\tthe refused scanner\t/proc\t",
	}
	if got := lines(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the record holds\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(exited) != 1 || exited[0] != RefusalStatus {
		t.Fatalf("Check exited %v, want once with %d, on the refused scan alone", exited, RefusalStatus)
	}
}

// classify is the one verdict on a root: each kind for the root and reason that make it, a
// namespace's /proc aside, which needs this process to be its PID 1.
func TestClassifyGivesEachRootItsKind(t *testing.T) {
	own := t.TempDir()
	for _, c := range []struct {
		root   string
		reason Reason
		want   string
	}{
		{own, Reason{}, ScanOwn},
		{own, Because("a reason changes nothing of a root of the test's own"), ScanOwn},
		{"/proc", Because("it reads its own pid alone"), ScanReason},
		{"/proc", Because("   "), ScanRefused},
		{"/proc", Reason{}, ScanRefused},
		{"/proc/self/..", Reason{}, ScanRefused},
	} {
		if got := classify(c.root, c.reason); got != c.want {
			t.Errorf("classify(%q, %q) = %q, want %q", c.root, c.reason, got, c.want)
		}
	}
}

// A test's name is the outermost Test function on a stack: the test itself or a closure of it, and
// no function whose name only starts with Test.
func TestATestIsNamedByItsTestFunction(t *testing.T) {
	for function, want := range map[string]string{
		"github.com/sjawhar/legion/daemon/internal/launcher.TestFoo":             "TestFoo",
		"github.com/sjawhar/legion/daemon/internal/launcher.TestFoo.func1.2":     "TestFoo",
		"github.com/sjawhar/legion/daemon/internal/launcher_test.Test_underline": "Test_underline",
		"github.com/sjawhar/legion/daemon/internal/launcher.Test":                "Test",
		"github.com/sjawhar/legion/daemon/internal/launcher.Testing":             "",
		"github.com/sjawhar/legion/daemon/internal/launcher.(*T).TestFoo":        "",
		"github.com/sjawhar/legion/daemon/internal/launcher.launchFleet.func1":   "",
		"testing.tRunner": "",
	} {
		if got := testName(function); got != want {
			t.Errorf("testName(%q) = %q, want %q", function, got, want)
		}
	}
}

// A goroutine's test is the top-level test up its chain of starters, which a dump of every
// goroutine names: a launcher's loop takes its starter's starter's; a subtest made of another
// test's function takes the test that ran it, and not TestMain, which the main goroutine's stack
// holds under every test; a scan TestMain makes itself takes none, for TestMain is no test; a
// goroutine whose chain reaches one that has returned takes none; and a loop whose test function
// has returned, as its cleanup stops the loop, takes the answer an earlier scan kept for a
// goroutine of its chain, and none where no earlier scan kept one. Each answer is kept for the
// goroutines up to the one that named it, and for none above it, which other tests share.
func TestAGoroutinesTestIsTheTopLevelTestUpItsStarters(t *testing.T) {
	const dump = `goroutine 40 [running]:
example.com/pkg.(*sweep).holders(0xc000123456)
	/src/pkg/sweep.go:10 +0x1
example.com/pkg.Run.func2()
	/src/pkg/run.go:20 +0x1
created by example.com/pkg.Run in goroutine 30
	/src/pkg/run.go:19 +0x1

goroutine 30 [select]:
example.com/pkg.Run({0x1, 0x2})
	/src/pkg/run.go:18 +0x1
example.com/pkg.launchFleet.func1()
	/src/pkg/fleet_test.go:9 +0x1
created by example.com/pkg.launchFleet in goroutine 23
	/src/pkg/fleet_test.go:8 +0x1

goroutine 23 [chan receive]:
example.com/pkg.newFleet(...)
	/src/pkg/fleet_test.go:5
example.com/pkg.TestTheLauncherSweeps(0xc000102380)
	/src/pkg/sweep_test.go:3 +0x1
testing.tRunner(0xc000102380, 0x1)
	/go/src/testing/testing.go:1792 +0xf4
created by testing.(*T).Run in goroutine 22
	/go/src/testing/testing.go:1851 +0x413

goroutine 22 [chan receive]:
testing.(*T).Run(0xc000102000, {0x1, 0x2}, 0x3)
	/go/src/testing/testing.go:1859 +0x431
example.com/pkg.TestTheOriginalConditions.func3(0xc000102000)
	/src/pkg/hostile_test.go:40 +0x1
testing.tRunner(0xc000102000, 0x1)
	/go/src/testing/testing.go:1792 +0xf4
created by testing.(*T).Run in goroutine 21
	/go/src/testing/testing.go:1851 +0x413

goroutine 21 [chan receive]:
testing.(*T).Run(0xc000101000, {0x1, 0x2}, 0x3)
	/go/src/testing/testing.go:1859 +0x431
example.com/pkg.TestTheOriginalConditions(0xc000101000)
	/src/pkg/hostile_test.go:30 +0x1
testing.tRunner(0xc000101000, 0x1)
	/go/src/testing/testing.go:1792 +0xf4
created by testing.(*T).Run in goroutine 1
	/go/src/testing/testing.go:1851 +0x413

goroutine 1 [chan receive]:
testing.(*T).Run(0xc000100000, {0x1, 0x2}, 0x3)
	/go/src/testing/testing.go:1859 +0x431
testing.runTests.func1(0xc000100000)
	/go/src/testing/testing.go:2279 +0x37
testing.tRunner(0xc000100000, 0x1)
	/go/src/testing/testing.go:1792 +0xf4
testing.runTests(...)
	/go/src/testing/testing.go:2277 +0x4a5
testing.(*M).Run(0xc000180000)
	/go/src/testing/testing.go:2145 +0x7a5
example.com/pkg.TestMain(0xc000180000)
	/src/pkg/main_test.go:12 +0x1
main.main()
	_testmain.go:100 +0xa8

goroutine 50 [running]:
example.com/pkg.orphan()
	/src/pkg/orphan.go:3 +0x1
created by example.com/pkg.starter in goroutine 49
	/src/pkg/orphan.go:2 +0x1

goroutine 70 [running]:
example.com/pkg.(*guard).scan(0xc000123456)
	/src/pkg/guard.go:10 +0x1
example.com/pkg.(*guard).loop()
	/src/pkg/guard.go:20 +0x1
created by example.com/pkg.startGuard in goroutine 69
	/src/pkg/guard.go:19 +0x1

goroutine 69 [chan receive]:
example.com/pkg.startGuard.func2()
	/src/pkg/guard_test.go:9 +0x1
testing.(*common).Cleanup.func1()
	/go/src/testing/testing.go:1211 +0x1
testing.(*common).runCleanup(0xc000103000, 0x0)
	/go/src/testing/testing.go:1445 +0x1
testing.tRunner.func2()
	/go/src/testing/testing.go:1786 +0x1
testing.tRunner(0xc000103000, 0x1)
	/go/src/testing/testing.go:1798 +0xf4
created by testing.(*T).Run in goroutine 1
	/go/src/testing/testing.go:1851 +0x413
`
	for _, c := range []struct {
		id    uint64
		known map[uint64]string
		want  string
		chain []uint64
		why   string
	}{
		{40, nil, "TestTheOriginalConditions", []uint64{40, 30, 23, 22, 21}, "a loop of a launcher a subtest made of TestTheLauncherSweeps started"},
		{23, nil, "TestTheOriginalConditions", []uint64{23, 22, 21}, "the subtest made of TestTheLauncherSweeps"},
		{21, nil, "TestTheOriginalConditions", []uint64{21}, "the top-level test itself"},
		{50, nil, "", []uint64{50, 49}, "its starter, goroutine 49, has returned"},
		{70, nil, "", []uint64{70, 69, 1}, "its test function has returned, and no earlier scan kept its test's name"},
		{70, map[uint64]string{69: "TestTheGuardStopsInItsCleanup", 1: "TestNotThisOne"}, "TestTheGuardStopsInItsCleanup", []uint64{70, 69},
			"an earlier scan kept its test's goroutine's answer"},
		{40, map[uint64]string{30: "TestTheLauncherSweeps", 23: ""}, "TestTheLauncherSweeps", []uint64{40, 30},
			"the answer kept for its starter, the nearest of the chain that has one"},
	} {
		got, chain := testOfChain(c.id, []byte(dump), c.known)
		if got != c.want || !slices.Equal(chain, c.chain) {
			t.Errorf("the test of goroutine %d is %q, kept for %v, want %q kept for %v: %s", c.id, got, chain, c.want, c.chain, c.why)
		}
	}
	const helper = `goroutine 1 [running]:
example.com/pkg.reapNamespace()
	/src/pkg/reap.go:3 +0x1
example.com/pkg.TestMain(0xc000180000)
	/src/pkg/main_test.go:12 +0x1
main.main()
	_testmain.go:100 +0xa8
`
	if got, _ := testOfChain(1, []byte(helper), nil); got != "" {
		t.Errorf("the test of a scan TestMain makes is %q, want none: TestMain is no test", got)
	}
}
