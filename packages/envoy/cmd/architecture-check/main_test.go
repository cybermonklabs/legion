// These tests judge the command: each exit on the built binary, since go run does not carry a
// program's exit status, over the notes package's testdata directories, which notes.ReadSet's own
// tests read too, and over directories they write, and the bound's refusal through run. Names
// that hold a line break are written into a temporary directory alone, since a git path holding
// one breaks tools that read paths a line at a time.
package main

//ci:reads packages/envoy/internal/dispatch/architecture/notes/testdata/** testdataDir joins ../../internal/dispatch/architecture/notes/testdata, which TestTheBuiltBinary's built binary reads

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/testexec"
)

func TestMain(m *testing.M) { os.Exit(cmdtest.Main(m)) }

// The bound refuses a set holding a note of notes.MaxFileSize + 1 bytes before any note is read,
// so the note beside it, whose front matter holds an unknown key, is never parsed and its problem
// never printed: the command prints the bound's one refusal and the count line.
func TestTheBoundRefusesTheSetBeforeAnyNoteIsParsed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	big := filepath.Join(dir, "big.md")
	writeFile(t, big, bytes.Repeat([]byte("a"), notes.MaxFileSize+1))
	writeFile(t, filepath.Join(dir, "broken.md"), []byte("---\nbogus: 1\n---\nA note with an unknown key.\n"))

	var stdout, stderr strings.Builder
	code := run([]string{dir}, &stdout, &stderr)
	got := result{code: code, stdout: stdout.String(), stderr: stderr.String()}
	expect(t, got, result{
		code:   1,
		stdout: "architecture-check: " + dir + ": 2 notes, the set is refused\n",
		stderr: "architecture-check: " + dir + ": " + notes.CheckSize(big, notes.MaxFileSize+1).Error() + "\n",
	})
	if strings.Contains(got.stderr, "front matter") {
		t.Errorf("stderr %q names a front matter problem: a note was parsed", got.stderr)
	}
}

// The built binary answers in three ways: exit 0 for a valid set, 1 for a refused one, and 2 for a
// directory it cannot judge or arguments that do not name one directory. Every path it prints is
// absolute, and no text it takes from the directory or its arguments starts a line of its own or
// holds a "##[".
func TestTheBuiltBinary(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "architecture-check")
	testdata := testdataDir(t, "")

	t.Run("a valid set exits 0", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(testdata, "valid")
		expect(t, runBinary(t, binary, "", dir), result{stdout: "architecture-check: " + dir + ": 3 notes, the set is valid\n"})
	})
	t.Run("a refused set exits 1", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(testdata, "broken")
		got := runBinary(t, binary, "", dir)
		if want := "architecture-check: " + dir + ": 2 notes, the set is refused\n"; got.code != 1 || got.stdout != want {
			t.Errorf("exit %d, stdout %q; want exit 1, stdout %q", got.code, got.stdout, want)
		}
		if prefix := "architecture-check: " + dir + ": beta.md: front matter: "; !strings.HasPrefix(got.stderr, prefix) ||
			!strings.Contains(got.stderr, `: yaml: unmarshal errors:\n  line 2: field bogus not found in type notes.frontMatter`) || strings.Count(got.stderr, "\n") != 1 {
			t.Errorf("stderr %q, want one line starting %q and naming the unknown key bogus", got.stderr, prefix)
		}
	})
	t.Run("a missing path exits 2 and names --allow-empty", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "missing")
		expect(t, runBinary(t, binary, "", dir), result{
			code:   2,
			stderr: "architecture-check: " + dir + ": does not exist, and architecture-check reads it as the empty set only with --allow-empty\n",
		})
	})
	t.Run("an empty directory exits 2 and names --allow-empty", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		expect(t, runBinary(t, binary, "", dir), result{
			code:   2,
			stderr: "architecture-check: " + dir + ": holds no note, and architecture-check reads it as the empty set only with --allow-empty\n",
		})
	})
	t.Run("a file given as the directory exits 2", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()
		path := filepath.Join(parent, "api\n::error::forged.md")
		writeFile(t, path, []byte("A note.\n"))
		got := runBinary(t, binary, "", path)
		escaped := parent + `/api\n::error::forged.md`
		expect(t, got, result{code: 2, stderr: "architecture-check: " + escaped + ": cannot be read: open " + escaped + ": not a directory\n"})
		expectNoCommand(t, "stderr", got.stderr)
	})
	usageText := "usage: architecture-check [--allow-empty] <directory>\n" +
		"  -allow-empty\n" +
		"    \tread a directory that does not exist or holds no note as the empty set, which is valid\n"
	for _, row := range []struct {
		name  string
		args  []string
		first string
	}{
		{name: "no argument exits 2", first: "want one directory argument, got 0"},
		{name: "two arguments exit 2", args: []string{filepath.Join(testdata, "valid"), filepath.Join(testdata, "mixed")}, first: "want one directory argument, got 2"},
		{name: "an unknown flag exits 2", args: []string{"--bogus", filepath.Join(testdata, "valid")}, first: "flag provided but not defined: -bogus"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			expect(t, runBinary(t, binary, "", row.args...), result{code: 2, stderr: "architecture-check: " + row.first + "\n" + usageText})
		})
	}
	t.Run("--allow-empty over an empty directory exits 0", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		expect(t, runBinary(t, binary, "", "--allow-empty", dir), result{stdout: "architecture-check: " + dir + ": 0 notes, the set is valid\n"})
	})
	t.Run("--allow-empty over a missing path exits 0", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "missing")
		expect(t, runBinary(t, binary, "", "--allow-empty", dir), result{stdout: "architecture-check: " + dir + ": 0 notes, the set is valid\n"})
	})
	t.Run("a relative argument is named by its absolute path", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(testdata, "valid")
		expect(t, runBinary(t, binary, testdata, "valid"), result{stdout: "architecture-check: " + dir + ": 3 notes, the set is valid\n"})
	})
	t.Run("entries that are not notes are warnings", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(testdata, "mixed")
		expect(t, runBinary(t, binary, "", dir), result{
			stdout: "architecture-check: " + dir + ": 2 notes, the set is valid\n",
			stderr: "architecture-check: warning: " + dir + "/link.md: not a note (symbolic link); the importer passes over it\n" +
				"architecture-check: warning: " + dir + "/notes.txt: not a note (file); the importer passes over it\n" +
				"architecture-check: warning: " + dir + "/sub: not a note (directory); the importer passes over it\n",
		})
	})
	t.Run("one note is counted in the singular", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "api.md"), []byte("---\ntitle: API\n---\nThe one note.\n"))
		expect(t, runBinary(t, binary, "", dir), result{stdout: "architecture-check: " + dir + ": 1 note, the set is valid\n"})
	})
	t.Run("names in the directory start no line of the log", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "a.md"), []byte("---\ntitle: A\n---\nA valid note.\n"))
		// A file that is not a note, named to put a workflow command on a line of its own, and three
		// notes whose names notes.Parse refuses: one whose \r a runner reads as the end of a line,
		// one whose name holds a \n and one whose name starts with a tab. Each name prints exactly,
		// escaped.
		writeFile(t, filepath.Join(dir, "x\n::error title=Injected::forged annotation"), []byte("x"))
		note := []byte("---\ntitle: Y\n---\nA note.\n")
		writeFile(t, filepath.Join(dir, "y\r::warning::forged.md"), note)
		writeFile(t, filepath.Join(dir, "a\nb.md"), note)
		writeFile(t, filepath.Join(dir, "\tq.md"), note)
		got := runBinary(t, binary, "", dir)
		if want := "architecture-check: " + dir + ": 4 notes, the set is refused\n"; got.code != 1 || got.stdout != want {
			t.Errorf("exit %d, stdout %q; want exit 1, stdout %q", got.code, got.stdout, want)
		}
		// The warning, and then notes.Parse's problems in name order: the tab, the \n, the \r.
		warning := "architecture-check: warning: " + dir + `/x\n::error title=Injected::forged annotation: not a note (file); the importer passes over it`
		refusals := []string{
			"architecture-check: " + dir + `: \tq.md: file name must be <slug>.md`,
			"architecture-check: " + dir + `: a\nb.md: file name must be <slug>.md`,
			"architecture-check: " + dir + `: y\r::warning::forged.md: file name must be <slug>.md`,
		}
		lines := strings.Split(strings.TrimSuffix(got.stderr, "\n"), "\n")
		if len(lines) != 1+len(refusals) || lines[0] != warning {
			t.Errorf("stderr %q, want the line %q and then one line starting with each of %q", got.stderr, warning, refusals)
		} else {
			for i, want := range refusals {
				if !strings.HasPrefix(lines[i+1], want) {
					t.Errorf("stderr line %q, want a line starting with %q", lines[i+1], want)
				}
			}
		}
		expectNoCommand(t, "stdout", got.stdout)
		expectNoCommand(t, "stderr", got.stderr)
	})
	t.Run("the second command form in the directory is escaped", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "a.md"), []byte("---\ntitle: A\n---\nA valid note.\n"))
		// A file that is not a note and a note's parent, each holding "##[", which a runner reads
		// as a workflow command anywhere in a line.
		writeFile(t, filepath.Join(dir, "x##[error]y"), []byte("x"))
		writeFile(t, filepath.Join(dir, "b.md"), []byte("---\ntitle: B\nparent: \"##[warning]y\"\n---\nA note.\n"))
		got := runBinary(t, binary, "", dir)
		expect(t, got, result{
			code:   1,
			stdout: "architecture-check: " + dir + ": 2 notes, the set is refused\n",
			stderr: "architecture-check: warning: " + dir + `/x##\x5berror]y: not a note (file); the importer passes over it` + "\n" +
				"architecture-check: " + dir + `: b.md: unknown parent "##\x5bwarning]y"` + "\n",
		})
		expectNoCommand(t, "stdout", got.stdout)
		expectNoCommand(t, "stderr", got.stderr)
	})
	t.Run("an unknown flag holding a newline is escaped in its error line", func(t *testing.T) {
		t.Parallel()
		got := runBinary(t, binary, "", "--x\n::error::forged", filepath.Join(testdata, "valid"))
		if prefix := `architecture-check: flag provided but not defined: -x\n::error::forged` + "\nusage: architecture-check [--allow-empty] <directory>\n"; got.code != 2 ||
			got.stdout != "" || !strings.HasPrefix(got.stderr, prefix) {
			t.Errorf("exit %d, stdout %q, stderr %q; want exit 2, no stdout and stderr starting %q", got.code, got.stdout, got.stderr, prefix)
		}
		expectNoCommand(t, "stderr", got.stderr)
	})
}

// An entry that is no file, no directory and no symbolic link is not a note, though its name is a
// note's: the built binary warns of the named pipe queue.md once, as an other entry, and the valid
// note beside it keeps the exit status at 0.
func TestANamedPipeIsAWarningThatKeepsTheExitStatus(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "architecture-check")
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "api.md"), []byte("---\ntitle: API\n---\nThe one note.\n"))
	pipe := filepath.Join(dir, "queue.md")
	if err := syscall.Mkfifo(pipe, 0o644); err != nil {
		t.Fatalf("make the named pipe %s: %v", pipe, err)
	}
	expect(t, runBinary(t, binary, "", dir), result{
		stdout: "architecture-check: " + dir + ": 1 note, the set is valid\n",
		stderr: "architecture-check: warning: " + pipe + ": not a note (other entry); the importer passes over it\n",
	})
}

// result is one run of the command: its exit status and what it wrote to each stream.
type result struct {
	code   int
	stdout string
	stderr string
}

// binaryLimit bounds one run of the built binary, which answers in milliseconds: from when the
// kernel made its process (testexec.ProcStart) to its exit, not from the call, so a loaded machine
// that holds the start itself, before the kernel makes the process, kills no run. A run that
// blocks, as one that opens a named pipe nothing writes to does, or that takes that long of its
// own anywhere else, is killed at the limit and fails its test then.
const binaryLimit = 20 * time.Second

// binarySpare is what a run of the built binary leaves of the test binary's deadline (go test's
// -timeout): a run still going then is killed and fails its test, rather than holding it until go
// test's own deadline and outliving it.
const binarySpare = 30 * time.Second

// runBinary runs the built binary with args in the working directory dir, the test's own when
// empty, and answers its exit status and output. A run still going binaryLimit after the kernel
// made its process, or with binarySpare left of the test binary's deadline, is killed and fails
// the test. The binary runs on one P (GOMAXPROCS=1): the parallel rows keep every run alive at
// once, and a run left to size GOMAXPROCS to the host starts a thread for each P its collector's
// idle mark workers take, about 80 on a 288-CPU host.
func runBinary(t *testing.T, binary, dir string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testwait.Remaining(t, binarySpare))
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOMAXPROCS=1")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	overran, err := testexec.RunFromStart(t, cmd, binaryLimit)
	if overran {
		t.Fatalf("architecture-check %q did not exit within %s of when the kernel made its process and was killed: stdout %q, stderr %q", args, binaryLimit, stdout.String(), stderr.String())
	}
	if ctx.Err() != nil {
		t.Fatalf("architecture-check %q did not finish with %s left of the test binary's deadline and was killed: stdout %q, stderr %q", args, binarySpare, stdout.String(), stderr.String())
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("run architecture-check %q: %v", args, err)
	}
	return result{code: cmd.ProcessState.ExitCode(), stdout: stdout.String(), stderr: stderr.String()}
}

// expect fails the test unless got is want.
func expect(t *testing.T, got, want result) {
	t.Helper()
	if got != want {
		t.Errorf("exit %d, stdout %q, stderr %q\nwant exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr, want.code, want.stdout, want.stderr)
	}
}

// expectNoCommand fails the test unless out holds no \r, no "##[" and no line that starts with
// "::" after its leading white space: a GitHub runner reads a workflow command in both forms, a
// "##[" anywhere in a line and a line that starts with "::" once the runner trims its leading
// white space. The runner ends a line at \n and at \r alike, so out is split at both.
func expectNoCommand(t *testing.T, stream, out string) {
	t.Helper()
	if strings.Contains(out, "\r") {
		t.Errorf("%s %q holds a \\r", stream, out)
	}
	if strings.Contains(out, "##[") {
		t.Errorf("%s %q holds a ##[, which a runner reads as a workflow command", stream, out)
	}
	for _, line := range strings.FieldsFunc(out, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if strings.HasPrefix(strings.TrimLeftFunc(line, unicode.IsSpace), "::") {
			t.Errorf("%s holds the line %q, which a runner reads as a workflow command", stream, line)
		}
	}
}

// testdataDir is the absolute path of name under the notes package's testdata directory, the note
// sets notes.ReadSet's own tests read.
func testdataDir(t *testing.T, name string) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "internal", "dispatch", "architecture", "notes", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeFile writes content to path.
func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}
