package hostproc

import (
	"bytes"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode"
	"unicode/utf8"
)

// RecordEnv names the file a test binary records its scans in, which
// .github/scripts/private_proc.py sets for each test binary it runs: Check writes each scan it
// judges there, once per test, scanner, root and verdict, and the wrapper prints from it the tests
// that read a process table, each with the root it read. A binary started with no such file, a test
// run by hand, records nothing.
//
// The file holds one line per entry, its fields separated by tabs:
//
//	binary <pid>                                  written once, when a test binary that links
//	                                              this package starts, so the wrapper knows a
//	                                              binary with no scan line recorded and scanned
//	                                              nothing
//	scan <kind> <test> <scanner> <root> <reason>  one scan: kind is ScanOwn, ScanNamespace,
//	                                              ScanReason or ScanRefused; test is "" where the
//	                                              record cannot name it (testOf), which the
//	                                              wrapper says as UNKNOWN
const RecordEnv = "HOSTPROC_RECORD"

// BinaryEnv is set, by the wrapper, for the test binary go ran, and for no child a test starts of
// it: the binary takes it out of its own environment as it starts, so a child a test starts with
// that environment is known for a child, whose scans its chain of goroutines cannot name (testOf).
const BinaryEnv = "HOSTPROC_BINARY"

// TestEnv names, in a child a test starts of its own test binary, the top-level test that started
// it: the test every scan of the child is recorded with. ChildEnv is its entry.
const TestEnv = "HOSTPROC_TEST"

// ChildEnv is the environment entry that tells a child t starts of its own test binary which test
// started it, TestEnv set to t's top-level test, for the child's command to carry beside the
// variables it is started with. A child given none records its scans with no test, and the wrapper
// says them as UNKNOWN and fails the step: the helper test a child runs is not the test the
// command ran.
func ChildEnv(t testing.TB) string {
	top, _, _ := strings.Cut(t.Name(), "/")
	return TestEnv + "=" + top
}

// The kinds of root a scan line names.
const (
	// ScanOwn is a root that is no mount of proc: a directory the test wrote, or one of links to
	// the processes it started.
	ScanOwn = "own"
	// ScanNamespace is the /proc of a PID namespace whose first process is this one.
	ScanNamespace = "namespace"
	// ScanReason is a mount of proc the scanner read by its Reason: the machine's /proc in a test
	// run by hand, and the binary's own private /proc under the wrapper.
	ScanReason = "reason"
	// ScanRefused is a mount of proc the scanner had neither a root of the test's own nor a Reason
	// for: Check ended the binary on it.
	ScanRefused = "refused"
)

// recorder is this process's record, when RecordEnv names one it can open: seen keeps each line
// to one write, tests the test of each goroutine an earlier scan's chain passed through (testOf),
// and child and starter say whether this process is a child a test started of its test binary and,
// for one, the test TestEnv names.
var recorder struct {
	mu      sync.Mutex
	file    *os.File
	seen    map[string]bool
	tests   map[uint64]string
	child   bool
	starter string
}

// CIEnv is the variable GitHub Actions sets to true in every step it runs.
const CIEnv = "GITHUB_ACTIONS"

// A test binary is refused before any test runs when CI started it outside the wrapper
// (refuseUnwrapped), and otherwise opens its record as it starts and writes its binary line,
// whether or not a test of it ever scans.
func init() {
	refuseUnwrapped()
	openRecord()
}

// refuseUnwrapped ends a test binary that CI started with no record (RecordEnv unset), before any
// of its tests runs, naming itself, with RefusalStatus. CI runs every test binary of this module
// through .github/scripts/private_proc.py, which gives each one a record and a PID namespace of its
// own, so a binary with no record was started some other way, on the runner's /proc, whatever
// shell form started it: a script string handed to su -c or flock -c, a script file, a command a
// variable holds, none of which rule 22 of check-ci-filters.sh can read in a workflow's text. A
// child a test starts of its own binary inherits the record, or, given an environment of its own
// (env -i), no CIEnv either; a test that sets RecordEnv to nothing has set it. A binary that only
// lists its tests (-test.list) runs none and is let be. Outside CI nothing changes: a test run by
// hand has no record and needs none.
//
// RefusalStatus is no status a test binary ends with otherwise: a failed test ends it with 1, and
// a panic, a test's timeout or a fatal error of the runtime with 2; no code of this module but this
// package's refusals ends one with 86. The refusal also comes before any test runs, so its output
// holds no test's `--- FAIL` line.
func refuseUnwrapped() {
	if !inTest() || os.Getenv(CIEnv) != "true" || listsOnly(os.Args[1:]) {
		return
	}
	if _, recorded := os.LookupEnv(RecordEnv); recorded {
		return
	}
	fmt.Fprintf(os.Stderr, "%s %s is a test binary that links internal/hostproc, started in CI (%s=true) with no %s: it "+
		"runs outside .github/scripts/private_proc.py, on the runner's /proc, where its tests read every process of the "+
		"machine; run it through the wrapper (docs/testing.md, \"A test that lists the machine's /proc\")\n",
		Refusal, os.Args[0], CIEnv, RecordEnv)
	exit(RefusalStatus)
}

// listsOnly says whether a test binary's arguments only list its tests (-test.list), which runs
// none of them.
func listsOnly(args []string) bool {
	for _, arg := range args {
		if name, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "="); name == "test.list" {
			return true
		}
	}
	return false
}

// openRecord opens the file RecordEnv names, in a test binary. The file is the wrapper's, made
// before the binary ran, and opened without O_CREAT, for the reason the wrapper's own record is
// (private_proc.py). A process that cannot open it — a child a test started in a mount namespace
// of its own, where the path is another directory — records nothing; the wrapper holds each binary
// to its own binary line. The binary go ran takes BinaryEnv out of its environment; a process
// started without it is a child, recorded with the test TestEnv names.
func openRecord() {
	path := os.Getenv(RecordEnv)
	if path == "" || !inTest() {
		return
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return
	}
	_, binary := os.LookupEnv(BinaryEnv)
	if binary {
		_ = os.Unsetenv(BinaryEnv)
	}
	recorder.file, recorder.seen, recorder.tests = file, map[string]bool{}, map[uint64]string{}
	recorder.child, recorder.starter = !binary, os.Getenv(TestEnv)
	writeRecord("binary", strconv.Itoa(os.Getpid()))
}

// record writes what Check judged of one scan, the kind classify gave its root, once per test,
// scanner, root, kind and reason.
func record(scanner, root string, reason Reason, kind string) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.file == nil {
		return
	}
	fields := []string{"scan", kind, testOf(), scanner, root, reason.why}
	key := strings.Join(fields, "\x00")
	if recorder.seen[key] {
		return
	}
	recorder.seen[key] = true
	writeRecord(fields...)
}

// writeRecord writes one line in one write, so lines of the binary and of its children never
// interleave. A record that takes no line ends the binary: a list with a scan missing from it would
// say a test read nothing that read a process table.
func writeRecord(fields ...string) {
	for i, field := range fields {
		fields[i] = strings.Join(strings.Fields(field), " ")
	}
	if _, err := recorder.file.WriteString(strings.Join(fields, "\t") + "\n"); err != nil {
		fmt.Fprintf(os.Stderr, "hostproc: cannot record a scan in %s (%s): %v\n", recorder.file.Name(), RecordEnv, err)
		exit(1)
	}
}

// testOf is the test the calling goroutine scans for. In a child a test started of its test
// binary it is the test TestEnv names, "" for none: the helper the child runs is no test the
// command ran, and the test that started it runs in another process, which no chain reaches.
// Elsewhere it is the top-level test go test ran, which is the outermost Test function on the
// stack of the calling goroutine or of the goroutines that started it, one up from the next, as
// Go's dump of every goroutine names each one's starter ("created by … in goroutine N"). That chain
// holds a test's own goroutine and every subtest's, so a subtest made of another test's function
// (t.Run(name, TestX)), or a launcher's loop a test started, is named by the test that ran it.
//
// The answer is kept for every goroutine of the chain up to the one that named it (testOfChain):
// goroutine ids are never reused, and a goroutine a test started stays that test's. So a loop
// that scans every second dumps every goroutine once, and a loop whose first scan comes after its
// test function returned, as its cleanup stops it, is named by a goroutine of its chain an earlier
// scan named, the one that built its scanner among them. TestMain is no test: a scan whose chain
// reaches only TestMain, or a test function that has returned with no earlier scan passing
// through it, is "", which the wrapper says as UNKNOWN rather than name a test that may not have
// made it. The caller holds recorder.mu.
func testOf() string {
	if recorder.child {
		return recorder.starter
	}
	id := goroutineID()
	if test, ok := recorder.tests[id]; ok {
		return test
	}
	test, chain := testOfChain(id, allStacks(), recorder.tests)
	for _, g := range chain {
		recorder.tests[g] = test
	}
	return test
}

// testName is the test a function belongs to: TestX for "<package path>.TestX" and for its
// closures, "<package path>.TestX.func1.2", or "" for a function of no test. A test's name is Test
// and then nothing or a character that is no lower-case letter, as go test reads it.
func testName(function string) string {
	name := function[strings.LastIndexByte(function, '/')+1:]
	_, rest, ok := strings.Cut(name, ".")
	if !ok {
		return ""
	}
	test, _, _ := strings.Cut(rest, ".")
	if !strings.HasPrefix(test, "Test") {
		return ""
	}
	if next, _ := utf8.DecodeRuneInString(test[len("Test"):]); next != utf8.RuneError && unicode.IsLower(next) {
		return ""
	}
	return test
}

// goroutineID is the calling goroutine's id, from the first line of its stack: "goroutine N [".
func goroutineID() uint64 {
	var buf [64]byte
	fields := strings.Fields(string(buf[:runtime.Stack(buf[:], false)]))
	if len(fields) < 2 {
		return 0
	}
	id, _ := strconv.ParseUint(fields[1], 10, 64)
	return id
}

// allStacks is Go's dump of every goroutine of the process.
func allStacks() []byte {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return buf[:n]
		}
		buf = make([]byte, 2*len(buf))
	}
}

// goroutineStack is what testOfChain needs of one goroutine of a dump: the outermost Test function
// on its stack (TestMain is none), and the goroutine that started it, 0 for none.
type goroutineStack struct {
	test    string
	starter uint64
}

// testOfChain is testOf's answer for goroutine id, read from dump and from known, the answers kept
// for goroutines an earlier chain passed through: the test of the highest goroutine of id's chain
// of starters whose stack names one, or a known answer of a goroutine on it, which was that
// goroutine's own highest; "" when neither is. chain is the goroutines from id up to the one that
// gave the answer, each of which takes it: a goroutine above that one, goroutine 1 or a t.Run's
// caller, is shared with other tests and takes none, and with no answer every goroutine walked
// takes "", since a chain only loses stacks as its goroutines return.
func testOfChain(id uint64, dump []byte, known map[uint64]string) (string, []uint64) {
	stacks := parseStacks(dump)
	test, named := "", 0
	var walked []uint64
	seen := map[uint64]bool{}
	for g := id; g != 0 && !seen[g]; g = stacks[g].starter {
		seen[g] = true
		walked = append(walked, g)
		if answer, ok := known[g]; ok && answer != "" {
			return answer, walked
		}
		stack, ok := stacks[g]
		if !ok {
			break
		}
		if stack.test != "" {
			test, named = stack.test, len(walked)
		}
	}
	if test == "" {
		return "", walked
	}
	return test, walked[:named]
}

// parseStacks reads a dump of every goroutine: blocks apart by a blank line, each a "goroutine N
// [state]:" line, then a line naming each function, innermost first, with its file and line on the
// line after it, then "created by F in goroutine M" for a goroutine another started.
func parseStacks(dump []byte) map[uint64]goroutineStack {
	stacks := map[uint64]goroutineStack{}
	for _, block := range bytes.Split(dump, []byte("\n\n")) {
		lines := strings.Split(strings.TrimSpace(string(block)), "\n")
		header := strings.Fields(lines[0])
		if len(header) < 2 || header[0] != "goroutine" {
			continue
		}
		id, err := strconv.ParseUint(header[1], 10, 64)
		if err != nil {
			continue
		}
		var stack goroutineStack
		for _, line := range lines[1:] {
			if strings.HasPrefix(line, "\t") {
				continue
			}
			if created, ok := strings.CutPrefix(line, "created by "); ok {
				if _, starter, ok := strings.Cut(created, " in goroutine "); ok {
					stack.starter, _ = strconv.ParseUint(strings.TrimSpace(starter), 10, 64)
				}
				continue
			}
			function := line
			if open := strings.LastIndexByte(line, '('); open > 0 {
				function = line[:open]
			}
			if test := testName(function); test != "" && test != "TestMain" {
				stack.test = test
			}
		}
		stacks[id] = stack
	}
	return stacks
}
