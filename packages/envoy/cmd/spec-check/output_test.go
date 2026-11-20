package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// errFull is the error of a standard output that takes no text, as a full disk gives it.
var errFull = errors.New("no space left on device")

// fullWriter is a standard output that takes no text: each write fails with errFull.
type fullWriter struct{}

func (fullWriter) Write([]byte) (int, error) { return 0, errFull }

// shortWriter takes the first byte of each write and reports no error, which io.Writer forbids.
type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return min(len(p), 1), nil }

// failsOnceWriter fails its first write with errFull, as a disk that is full for a moment does,
// and takes each write after it into took.
type failsOnceWriter struct {
	failed bool
	took   strings.Builder
}

func (w *failsOnceWriter) Write(p []byte) (int, error) {
	if !w.failed {
		w.failed = true
		return 0, errFull
	}
	return w.took.Write(p)
}

// A standard output that fails one write and takes the next ones gets no text after the failed
// write, and the subcommand exits 2 with the one line that names that write: output keeps the
// first error and takes no write after it. compare over not-in-files writes three lines. The test
// sets GITHUB_ACTIONS to the empty value, so compare runs in a CI job too, and it does not run in
// parallel, since it changes the environment.
func TestAStandardOutputThatFailsOnceTakesNoLaterText(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	dir := fixture(t, "compare", "not-in-files")
	w := &failsOnceWriter{}
	var stderr strings.Builder
	code := run([]string{"compare", "--dir", filepath.Join(dir, "spec"), "--approved", filepath.Join(dir, "approved.json")}, w, &stderr)
	if want := "spec-check: compare: cannot write standard output: " + errFull.Error() + "\n"; code != 2 || stderr.String() != want || w.took.Len() != 0 {
		t.Errorf("compare to a standard output that fails once: exit %d, stderr %q, then took %q\nwant exit 2, stderr %q, and no text after the failed write",
			code, stderr.String(), w.took.String(), want)
	}
}

// Standard output is the product of each subcommand: the lines that a caller reads, the JSON of
// checks and the blocks of print, which a change copies into the files. So a subcommand whose
// standard output takes none of its text, or a part, exits 2 with one line on standard error that
// names it, and its other lines stay as they are. A standard output that takes the text gets it
// byte for byte. The test sets GITHUB_ACTIONS to the empty value, so compare runs in a CI job too,
// and it does not run in parallel, since it changes the environment.
func TestEachSubcommandReportsAStandardOutputThatTakesNoText(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	valid := fixture(t, "trees", "valid")
	approvedText := fixture(t, "compare", "approved-text")
	clean := ciFiles(t, "clean")
	for _, row := range []struct {
		args           []string
		stdout, stderr string
	}{
		{[]string{"tree", "--dir", valid},
			"spec-check: " + valid + ": 3 statements, 4 checks, 2 components, 1 interface, 1 term; the tree is valid\n",
			"spec-check: " + valid + noList},
		{[]string{"changes", "--base", valid, "--dir", valid}, "spec-check: no statement changed\n", ""},
		{[]string{"checks", "--dir", valid, "--job", "other"}, "[]\n", ""},
		{[]string{"compare", "--dir", filepath.Join(approvedText, "spec"), "--approved", filepath.Join(approvedText, "approved.json")},
			passLine(filepath.Join(approvedText, "spec"), "3 of 3"), ""},
		{[]string{"print", "--approved", filepath.Join(approvedText, "approved.json"), "--id", "order"},
			":::term{#order name=\"order\"}\nAn order is a request of a customer for one or more products.\n:::\n", ""},
		{[]string{"guard", "--root", clean}, "spec-check: " + clean + ": no line of 4 CI files names the command that compares the files\n", ""},
	} {
		name := row.args[0]
		var stdout, stderr strings.Builder
		if code := run(row.args, &stdout, &stderr); code != 0 || stdout.String() != row.stdout || stderr.String() != row.stderr {
			t.Errorf("%s to a standard output that takes the text: exit %d, stdout %q, stderr %q\nwant exit 0, stdout %q, stderr %q",
				name, code, stdout.String(), stderr.String(), row.stdout, row.stderr)
		}
		for _, w := range []struct {
			kind   string
			writer io.Writer
			err    string
		}{{"that takes no text", fullWriter{}, errFull.Error()}, {"that takes a part", shortWriter{}, "short write"}} {
			stderr.Reset()
			want := row.stderr + "spec-check: " + name + ": cannot write standard output: " + w.err + "\n"
			if code := run(row.args, w.writer, &stderr); code != 2 || stderr.String() != want {
				t.Errorf("%s to a standard output %s: exit %d, stderr %q\nwant exit 2, stderr %q", name, w.kind, code, stderr.String(), want)
			}
		}
	}
}

// The built command reports a standard output on a full device, /dev/full, as the deep review
// found print did not: exit 2 and the line on standard error. A shell opens /dev/full as the
// standard output and puts the binary in its own place (exec), so the run of the binary has the
// limit of runBounded, as each other run of it in these tests has.
func TestTheBuiltBinaryReportsAFullStandardOutput(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/dev/full"); errors.Is(err, os.ErrNotExist) {
		t.Skip("this system has no /dev/full; TestEachSubcommandReportsAStandardOutputThatTakesNoText holds the rule")
	} else if err != nil {
		t.Fatal(err)
	}
	binary := cmdtest.Build(t, "spec-check")
	got, err := runBounded(binaryLimit, "/bin/sh", "", childEnv(), "-c", `exec "$@" >/dev/full`, "sh",
		binary, "print", "--approved", fixture(t, "compare", "approved-text", "approved.json"), "--id", "order")
	if err != nil {
		t.Fatal(err)
	}
	if want := "spec-check: print: cannot write standard output: write /dev/stdout: no space left on device\n"; got.code != 2 || got.stderr != want {
		t.Errorf("print to /dev/full: exit %d, stderr %q; want exit 2 and the line that names the write, %q", got.code, got.stderr, want)
	}
}
