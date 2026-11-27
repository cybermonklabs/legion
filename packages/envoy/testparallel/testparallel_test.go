package testparallel

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// plantedTests is the planted package's main test file: a test of each shape the rule tells
// apart, and three functions that are not tests: Testimony and TestMain, which go test does not
// run as tests, and TestWithAResult, whose signature go test refuses to build (a test function
// returns nothing), and which this file holds only for the check to pass over. Each test whose name
// has Keeps in it keeps the rule, and each whose name has Breaks in it breaks it.
const plantedTests = `package planted

import "testing"

var other *testing.T

func TestPlantedKeepsParallel(t *testing.T) {
	t.Parallel()
}

func TestPlantedKeepsParenthesizedParallel(t *testing.T) {
	(t).Parallel()
}

func TestPlantedKeepsSayingWhyNot(t *testing.T) {
	// Not parallel: it sets an environment variable of the whole process.
	t.Setenv("LEGION_TEST_FIXTURE_VARIABLE", "1")
}

func TestPlantedKeepsSayingWhyNotWithAParallelSubtest(t *testing.T) {
	// Not parallel: it sets an environment variable of the whole process.
	t.Run("sub", func(t *testing.T) { t.Parallel() })
}

func TestPlantedBreaksForgettingBoth(t *testing.T) {
	_ = t.TempDir()
}

func TestPlantedBreaksCallingParallelLate(t *testing.T) {
	dir := t.TempDir()
	t.Parallel()
	_ = dir
}

func TestPlantedBreaksCallingAnotherTestsParallel(t *testing.T) {
	other.Parallel()
}

func TestPlantedBreaksDeferringParallel(t *testing.T) {
	defer t.Parallel()
}

func TestPlantedBreaksCallingParallelInASubtestOnly(t *testing.T) {
	t.Run("sub", func(t *testing.T) { t.Parallel() })
}

func TestPlantedBreaksGivingNoReason(t *testing.T) {
	// Not parallel:
	_ = t.TempDir()
}

func TestPlantedBreaksGivingItsReasonOnTheNextLine(t *testing.T) {
	// Not parallel:
	// it sets an environment variable of the whole process.
	_ = t.TempDir()
}

func TestPlantedBreaksGivingItsReasonAfterAnEmptyCommentLine(t *testing.T) {
	//
	// Not parallel: it sets an environment variable of the whole process.
	_ = t.TempDir()
}

func TestPlantedBreaksSayingWhyNotAfterAStatement(t *testing.T) {
	_ = t.TempDir()
	// Not parallel: it sets an environment variable of the whole process.
}

func TestPlantedBreaksSayingWhyNotAfterAnotherComment(t *testing.T) {
	// A comment about the test.

	// Not parallel: it sets an environment variable of the whole process.
	_ = t.TempDir()
}

// Not parallel: above the test, which is no line of its body.
func TestPlantedBreaksSayingWhyNotAboveItself(t *testing.T) {
	_ = t.TempDir()
}

func TestPlantedBreaksSayingWhyNotAndCallingParallel(t *testing.T) {
	// Not parallel: it says so, and calls t.Parallel all the same.
	t.Parallel()
}

func TestPlantedBreaksSayingWhyNotAndCallingParallelSecond(t *testing.T) {
	// Not parallel: it says so, and calls t.Parallel after another statement.
	_ = t.TempDir()
	t.Parallel()
}

func TestPlantedBreaksSayingWhyNotAndCallingParallelInAClosure(t *testing.T) {
	// Not parallel: it says so, and a closure over t calls t.Parallel.
	func() { t.Parallel() }()
}

func TestPlantedBreaksSayingWhyNotAndCallingParallelThroughAMethodValue(t *testing.T) {
	// Not parallel: it says so, and calls t.Parallel through a method value.
	p := t.Parallel
	p()
}

func TestPlantedBreaksWithAnUnnamedParameter(*testing.T) {}

func TestPlantedBreaksEmpty(t *testing.T) {}

func Testimony(t *testing.T) {}

func TestWithAResult(t *testing.T) int { return 0 }

func TestMain(m *testing.M) {}
`

// citedTests is a test file of the planted package whose serial tests give a reason that names
// a line, each a way a line can be named, and one whose marker is followed by a comment holding an
// address's port and a time of day, which name no line. Each test whose name has Keeps in it keeps
// the rule, and each whose name has Breaks in it breaks it.
const citedTests = `package planted

import "testing"

func TestPlantedBreaksCitingAFilesLine(t *testing.T) {
	// Not parallel: it reads the default logger back (slog.SetDefault at logtest.go:36).
	_ = t.TempDir()
}

func TestPlantedBreaksCitingALineOnItsSecondLine(t *testing.T) {
	// Not parallel: it reads the default logger back (logtest.SetDefault,
	// slog.SetDefault at logtest.go:36).
	_ = t.TempDir()
}

func TestPlantedBreaksCitingABareLine(t *testing.T) {
	// Not parallel: it holds an upper bound on a measured time (the check at :85).
	_ = t.TempDir()
}

func TestPlantedBreaksCitingALineInWords(t *testing.T) {
	// Not parallel: it holds an upper bound on a measured time (the check on line 85).
	_ = t.TempDir()
}

func TestPlantedBreaksCitingLinesInWords(t *testing.T) {
	// Not parallel: it holds an upper bound on a measured time (the checks on lines 85-90).
	_ = t.TempDir()
}

func TestPlantedBreaksCitingALineAcrossTwoLines(t *testing.T) {
	// Not parallel: it holds an upper bound on a measured time (the check on line
	// 85).
	_ = t.TempDir()
}

func TestPlantedBreaksCitingALineAfterATab(t *testing.T) {
	// Not parallel: it holds an upper bound on a measured time (the check on line	85).
	_ = t.TempDir()
}

func TestPlantedKeepsAnAddressAndATimeUnderItsMarker(t *testing.T) {
	// Not parallel: it sets an environment variable of the whole process.
	// The stand-in listens on 127.0.0.1:1, which nothing serves, from 10:30 on.
	t.Setenv("LEGION_TEST_FIXTURE_VARIABLE", "1")
}
`

// plantedPackage is a package holding plantedTests and citedTests; two test files that import
// testing under another name and with a dot, each with a test that breaks the rule; a test of its
// external test package that breaks it; and a test behind a build tag this build does not set,
// which breaks it too and is read all the same, since a tagged test runs in some lane.
func plantedPackage(t *testing.T) string {
	t.Helper()
	return plant(t, map[string]string{
		"planted.go":      "package planted\n",
		"planted_test.go": plantedTests,
		"cited_test.go":   citedTests,
		"aliased_test.go": "package planted\n\nimport tst \"testing\"\n\nfunc TestPlantedKeepsAliasedParallel(t *tst.T) {\n\tt.Parallel()\n}\n\nfunc TestPlantedBreaksAliased(t *tst.T) {}\n",
		"dotted_test.go":  "package planted\n\nimport . \"testing\"\n\nfunc TestPlantedBreaksDotted(t *T) {}\n",
		"outside_test.go": "package planted_test\n\nimport \"testing\"\n\nfunc TestPlantedBreaksOutside(t *testing.T) {\n\t_ = t.Name()\n}\n",
		"e2e_test.go":     "//go:build e2e\n\npackage planted\n\nimport \"testing\"\n\nfunc TestPlantedBreaksBehindATag(t *testing.T) {\n\t_ = t.Name()\n}\n",
	})
}

// plant writes files, by name, into a directory of its own and answers it.
func plant(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("plant %s: %v", name, err)
		}
	}
	return dir
}

// failures is a testing.TB that keeps each failure Require reports rather than failing the test.
type failures struct {
	testing.TB
	reported []string
}

func (f *failures) Errorf(format string, args ...any) {
	f.reported = append(f.reported, fmt.Sprintf(format, args...))
}

// serialTests reads every test of every build, as go test finds one: in the package's own test
// files, whichever way a file imported testing, and in its external test package's. It names
// exactly those that break the rule, each where it is declared: one that forgets both, one that
// calls t.Parallel() after another statement, on another test's *testing.T, in a defer or in a
// subtest alone, one whose comment gives no reason on its first line (on the next line, or under
// an empty `//`), is not the body's first comment or is above the test, one that says it is not
// parallel and calls t.Parallel() all the same (first, after another statement, in a closure over
// its t, or through the method value t.Parallel), one whose parameter has no name, an empty one,
// one behind a build tag the build does not set, and one whose reason names a line (a file's, on
// its first line or its second, a bare one, or one in words, on one line or across two, singular
// or plural, after a tab). A test that says it is not parallel and runs a subtest in parallel
// keeps the rule, and so does one whose marker is followed by a comment holding an address's port
// and a time of day. It lists the tests that keep the rule by running alone, each with its reason.
// A function that is not a test is not counted.
func TestSerialTestsNamesEachTestThatBreaksTheRule(t *testing.T) {
	t.Parallel()
	read, broken, marked, err := serialTests(plantedPackage(t))
	if err != nil {
		t.Fatalf("serialTests: %v", err)
	}
	var named []string
	for _, b := range broken {
		named = append(named, b.file+":"+strconv.Itoa(b.line)+" "+b.name)
	}
	want := []string{
		"aliased_test.go:9 TestPlantedBreaksAliased",
		"cited_test.go:5 TestPlantedBreaksCitingAFilesLine",
		"cited_test.go:10 TestPlantedBreaksCitingALineOnItsSecondLine",
		"cited_test.go:16 TestPlantedBreaksCitingABareLine",
		"cited_test.go:21 TestPlantedBreaksCitingALineInWords",
		"cited_test.go:26 TestPlantedBreaksCitingLinesInWords",
		"cited_test.go:31 TestPlantedBreaksCitingALineAcrossTwoLines",
		"cited_test.go:37 TestPlantedBreaksCitingALineAfterATab",
		"dotted_test.go:5 TestPlantedBreaksDotted",
		"e2e_test.go:7 TestPlantedBreaksBehindATag",
		"outside_test.go:5 TestPlantedBreaksOutside",
		"planted_test.go:25 TestPlantedBreaksForgettingBoth",
		"planted_test.go:29 TestPlantedBreaksCallingParallelLate",
		"planted_test.go:35 TestPlantedBreaksCallingAnotherTestsParallel",
		"planted_test.go:39 TestPlantedBreaksDeferringParallel",
		"planted_test.go:43 TestPlantedBreaksCallingParallelInASubtestOnly",
		"planted_test.go:47 TestPlantedBreaksGivingNoReason",
		"planted_test.go:52 TestPlantedBreaksGivingItsReasonOnTheNextLine",
		"planted_test.go:58 TestPlantedBreaksGivingItsReasonAfterAnEmptyCommentLine",
		"planted_test.go:64 TestPlantedBreaksSayingWhyNotAfterAStatement",
		"planted_test.go:69 TestPlantedBreaksSayingWhyNotAfterAnotherComment",
		"planted_test.go:77 TestPlantedBreaksSayingWhyNotAboveItself",
		"planted_test.go:81 TestPlantedBreaksSayingWhyNotAndCallingParallel",
		"planted_test.go:86 TestPlantedBreaksSayingWhyNotAndCallingParallelSecond",
		"planted_test.go:92 TestPlantedBreaksSayingWhyNotAndCallingParallelInAClosure",
		"planted_test.go:97 TestPlantedBreaksSayingWhyNotAndCallingParallelThroughAMethodValue",
		"planted_test.go:103 TestPlantedBreaksWithAnUnnamedParameter",
		"planted_test.go:105 TestPlantedBreaksEmpty",
	}
	if read != 34 || !slices.Equal(named, want) {
		t.Fatalf("serialTests read %d tests and named\n\t%s\nwant 34 tests read and\n\t%s", read, strings.Join(named, "\n\t"), strings.Join(want, "\n\t"))
	}
	for _, b := range broken {
		want := forgetsBoth
		switch {
		case strings.HasPrefix(b.name, "TestPlantedBreaksSayingWhyNotAndCallingParallel"):
			want = contradicted
		case strings.HasPrefix(b.name, "TestPlantedBreaksCiting"):
			if !strings.HasPrefix(b.note, citesALine) {
				t.Errorf("%s breaks the rule because %q, want a note that opens %q", b.name, b.note, citesALine)
			}
			continue
		}
		if b.note != want {
			t.Errorf("%s breaks the rule because %q, want %q", b.name, b.note, want)
		}
	}
	var alone []string
	for _, b := range marked {
		alone = append(alone, b.at()+" "+b.note)
	}
	wantAlone := []string{
		"cited_test.go:42 TestPlantedKeepsAnAddressAndATimeUnderItsMarker it sets an environment variable of the whole process.",
		"planted_test.go:15 TestPlantedKeepsSayingWhyNot it sets an environment variable of the whole process.",
		"planted_test.go:20 TestPlantedKeepsSayingWhyNotWithAParallelSubtest it sets an environment variable of the whole process.",
	}
	if !slices.Equal(alone, wantAlone) {
		t.Errorf("serialTests listed the tests that run alone as\n\t%s\nwant\n\t%s", strings.Join(alone, "\n\t"), strings.Join(wantAlone, "\n\t"))
	}
}

// Require fails the test that runs it, naming each test serialTests names; it fails a package
// with no test at all, which a check that read nothing would otherwise pass, a directory that is
// not there and a test file it cannot parse; and it passes a package whose every test keeps the
// rule.
func TestRequireFailsOnATestThatBreaksTheRuleAndOnWhatItCannotRead(t *testing.T) {
	t.Parallel()
	broken := &failures{TB: t}
	Require(broken, plantedPackage(t))
	if len(broken.reported) != 1 {
		t.Fatalf("Require on the planted package reported %q, want one failure", broken.reported)
	}
	for _, name := range []string{"TestPlantedBreaksForgettingBoth", "TestPlantedBreaksAliased", "TestPlantedBreaksOutside", "TestPlantedBreaksSayingWhyNotAndCallingParallel", "TestPlantedBreaksCitingAFilesLine", "TestPlantedBreaksBehindATag"} {
		if !strings.Contains(broken.reported[0], name) {
			t.Errorf("Require's failure on the planted package does not name %s:\n%s", name, broken.reported[0])
		}
	}
	if strings.Contains(broken.reported[0], "Keeps") {
		t.Errorf("Require's failure names a test that keeps the rule:\n%s", broken.reported[0])
	}

	for _, tc := range []struct {
		name, dir, want string
	}{
		{name: "a package with no test", dir: plant(t, map[string]string{"none.go": "package none\n"}), want: "read no test"},
		{name: "a directory that is not there", dir: filepath.Join(t.TempDir(), "gone"), want: "read the tests of"},
		{name: "a test file that does not parse", dir: plant(t, map[string]string{
			"bad.go":      "package bad\n",
			"bad_test.go": "package bad\n\nimport \"testing\"\n\nfunc TestBad(t *testing.T) { ??? }\n",
		}), want: "read the tests of"},
	} {
		unread := &failures{TB: t}
		Require(unread, tc.dir)
		if len(unread.reported) != 1 || !strings.Contains(unread.reported[0], tc.want) {
			t.Errorf("Require on %s reported %q, want one failure saying %q", tc.name, unread.reported, tc.want)
		}
	}

	kept := &failures{TB: t}
	Require(kept, plant(t, map[string]string{
		"kept.go":      "package kept\n",
		"kept_test.go": "package kept\n\nimport \"testing\"\n\nfunc TestKept(t *testing.T) {\n\tt.Parallel()\n}\n",
	}))
	if len(kept.reported) != 0 {
		t.Errorf("Require on a package whose one test calls t.Parallel() first reported %q, want nothing", kept.reported)
	}
}
