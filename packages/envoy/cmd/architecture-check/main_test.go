// These tests judge the set builder over this package's testdata directories, for what a checkout
// can hold, and over in-memory directories, for what a checkout cannot hold or this module must
// not commit: two names that differ only in case, which a case-insensitive filesystem cannot
// hold, and a note that is not valid UTF-8, which the store's test of every *.md file of this
// module sends to Postgres. They test the bound with a note they write, and each exit on the
// built binary, since go run does not carry a program's exit status.
package main

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
)

// The set builder over what a checkout holds: a valid set, a set holding a broken note, and a set
// beside entries that are not notes.
func TestBuildSetOverTestdata(t *testing.T) {
	t.Parallel()
	t.Run("a valid set", func(t *testing.T) {
		t.Parallel()
		dir := testdataDir(t, "valid")
		files, rejects, err := buildSet(os.DirFS(dir), dir)
		if err != nil || len(rejects) != 0 {
			t.Fatalf("buildSet(%s): rejected %v, err %v, want neither", dir, rejects, err)
		}
		if got, want := names(files), []string{"checker.md", "rules.md", "text.md"}; !slices.Equal(got, want) {
			t.Fatalf("notes %q, want %q", got, want)
		}
		model, err := notes.Parse(files)
		if err != nil {
			t.Fatalf("notes.Parse: %v, want a valid set", err)
		}
		at := slices.IndexFunc(model.Components, func(component notes.Component) bool { return component.ID == "rules" })
		if at < 0 {
			t.Fatalf("model %+v holds no component rules", model)
		}
		if rules := model.Components[at]; rules.Parent != "checker" || !slices.Equal(rules.DependsOn, []string{"text"}) {
			t.Errorf("component rules %+v, want parent checker and depends_on [text]", rules)
		}
	})
	t.Run("a broken note", func(t *testing.T) {
		t.Parallel()
		dir := testdataDir(t, "broken")
		files, rejects, err := buildSet(os.DirFS(dir), dir)
		if err != nil || len(rejects) != 0 {
			t.Fatalf("buildSet(%s): rejected %v, err %v, want neither", dir, rejects, err)
		}
		if got, want := names(files), []string{"alpha.md", "beta.md"}; !slices.Equal(got, want) {
			t.Fatalf("notes %q, want %q", got, want)
		}
		_, err = notes.Parse(files)
		if err == nil {
			t.Fatal("notes.Parse accepted a note with an unknown front-matter key")
		}
		found := problems(err)
		if len(found) != 1 {
			t.Fatalf("problems %q, want one", found)
		}
		// yaml.v3 answers an unknown key over two lines, which the command prints as one.
		line := oneLine(found[0].Error())
		if !strings.HasPrefix(line, "beta.md: front matter: ") || !strings.Contains(line, "field bogus not found") || strings.Contains(line, "\n") {
			t.Errorf("problem %q, want one line naming beta.md and its unknown key bogus", line)
		}
	})
	t.Run("entries that are not notes", func(t *testing.T) {
		t.Parallel()
		dir := testdataDir(t, "mixed")
		files, rejects, err := buildSet(os.DirFS(dir), dir)
		if err != nil {
			t.Fatalf("buildSet(%s): %v", dir, err)
		}
		want := []rejected{
			{path: filepath.Join(dir, "link.md"), kind: notes.KindSymlink},
			{path: filepath.Join(dir, "notes.txt"), kind: notes.KindFile},
			{path: filepath.Join(dir, "sub"), kind: notes.KindDirectory},
		}
		if !slices.Equal(rejects, want) {
			t.Errorf("rejected %v, want %v", rejects, want)
		}
		if got, want := names(files), []string{"alpha.md", "beta.md"}; !slices.Equal(got, want) {
			t.Fatalf("notes %q, want %q", got, want)
		}
		if _, err := notes.Parse(files); err != nil {
			t.Errorf("notes.Parse: %v, want a valid set", err)
		}
	})
}

// The set builder over in-memory directories: the two sets a checkout cannot hold or this module
// must not commit, which notes.Parse refuses, and the kinds of entry the predicate decides by
// mode alone.
func TestBuildSetOverMemory(t *testing.T) {
	t.Parallel()
	const dir = "/checkout/.dispatch/architecture"
	note := []byte("---\ntitle: A note\n---\nIts prose.\n")
	for _, row := range []struct {
		name    string
		fsys    fstest.MapFS
		notes   []string
		rejects []rejected
		problem string // what a problem notes.Parse answers holds; empty for a valid set
	}{
		{
			name:    "names that differ only in case",
			fsys:    fstest.MapFS{"Api.md": {Data: note}, "api.md": {Data: note}},
			notes:   []string{"Api.md", "api.md"},
			problem: `api.md: duplicate component id "api"`,
		},
		{
			name:    "a note that is not valid UTF-8",
			fsys:    fstest.MapFS{"binary.md": {Data: []byte("prose \xff\xfe")}},
			notes:   []string{"binary.md"},
			problem: "binary.md: file must be valid UTF-8",
		},
		{
			name:    "a symbolic link",
			fsys:    fstest.MapFS{"api.md": {Data: note}, "link.md": {Data: []byte("api.md"), Mode: fs.ModeSymlink | 0o777}},
			notes:   []string{"api.md"},
			rejects: []rejected{{path: dir + "/link.md", kind: notes.KindSymlink}},
		},
		{
			name:  "a file of mode 0755",
			fsys:  fstest.MapFS{"api.md": {Data: note, Mode: 0o755}},
			notes: []string{"api.md"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			files, rejects, err := buildSet(row.fsys, dir)
			if err != nil {
				t.Fatalf("buildSet: %v", err)
			}
			if got := names(files); !slices.Equal(got, row.notes) {
				t.Errorf("notes %q, want %q", got, row.notes)
			}
			if !slices.Equal(rejects, row.rejects) {
				t.Errorf("rejected %v, want %v", rejects, row.rejects)
			}
			_, err = notes.Parse(files)
			switch {
			case row.problem == "" && err != nil:
				t.Errorf("notes.Parse: %v, want a valid set", err)
			case row.problem != "" && (err == nil || !slices.ContainsFunc(problems(err), func(problem error) bool {
				return strings.Contains(oneLine(problem.Error()), row.problem)
			})):
				t.Errorf("notes.Parse: %v, want a problem holding %q", err, row.problem)
			}
		})
	}
}

// The bound refuses a set holding a note of notes.MaxFileSize + 1 bytes before the set builder
// reads any note, so the note beside it, whose front matter holds an unknown key, is never parsed
// and its problem never printed.
func TestTheBoundRefusesTheSetBeforeAnyNoteIsRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	big := filepath.Join(dir, "big.md")
	writeFile(t, big, bytes.Repeat([]byte("a"), notes.MaxFileSize+1))
	writeFile(t, filepath.Join(dir, "broken.md"), []byte("---\nbogus: 1\n---\nA note with an unknown key.\n"))

	opened := &openRecorder{fsys: os.DirFS(dir)}
	files, _, err := buildSet(opened, dir)
	var refused *sizeRefusal
	if !errors.As(err, &refused) || !errors.Is(err, notes.ErrFileTooLarge) {
		t.Fatalf("buildSet: %v, want a *sizeRefusal of notes.ErrFileTooLarge", err)
	}
	if files != nil || refused.held != 2 || len(refused.problems) != 1 || !strings.Contains(refused.problems[0].Error(), big+" is 1048577 bytes") {
		t.Errorf("buildSet: files %q, %d notes held, problems %q; want no files, 2 notes and one refusal naming %s", names(files), refused.held, refused.problems, big)
	}
	if !slices.Equal(opened.names, []string{"."}) {
		t.Errorf("opened %q, want the directory alone: a note was read", opened.names)
	}

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

// The bound checks each note again once it is read, so a note that grew between the listing and
// the read is refused as one the listing showed too large is.
func TestTheBoundRefusesANoteThatGrewAfterItsSizeWasRead(t *testing.T) {
	t.Parallel()
	const dir = "/checkout/.dispatch/architecture"
	files, _, err := buildSet(grownFS{fstest.MapFS{"api.md": {Data: []byte("a")}}}, dir)
	var refused *sizeRefusal
	if !errors.As(err, &refused) || !errors.Is(err, notes.ErrFileTooLarge) {
		t.Fatalf("buildSet: %v, want a *sizeRefusal of notes.ErrFileTooLarge", err)
	}
	if files != nil || refused.held != 1 || len(refused.problems) != 1 || !strings.Contains(refused.problems[0].Error(), dir+"/api.md is 1048577 bytes") {
		t.Errorf("buildSet: files %q, %d notes held, problems %q; want no files, 1 note and one refusal naming %s/api.md", names(files), refused.held, refused.problems, dir)
	}
}

// A note the set builder cannot read is exit 2's "cannot be read", naming the note by its
// absolute path, though os.DirFS's error names it by its name inside the directory.
func TestBuildSetNamesANoteItCannotReadByItsAbsolutePath(t *testing.T) {
	t.Parallel()
	const dir = "/checkout/.dispatch/architecture"
	files, _, err := buildSet(deniedFS{fstest.MapFS{"api.md": {Data: []byte("A note.\n")}}}, dir)
	if !errors.Is(err, fs.ErrPermission) || files != nil {
		t.Fatalf("buildSet: files %q, err %v; want no files and an error of fs.ErrPermission", names(files), err)
	}
	if want := "cannot be read: open " + dir + "/api.md: permission denied"; err.Error() != want {
		t.Errorf("buildSet: %q, want %q", err, want)
	}
}

// The built binary answers in three ways: exit 0 for a valid set, 1 for a refused one, and 2 for a
// directory it cannot judge or arguments that do not name one directory. Every path it prints is
// absolute.
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
			!strings.Contains(got.stderr, "field bogus not found") || strings.Count(got.stderr, "\n") != 1 {
			t.Errorf("stderr %q, want one line starting %q and naming the unknown key bogus", got.stderr, prefix)
		}
	})
	t.Run("a missing path exits 2", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "missing")
		expect(t, runBinary(t, binary, "", dir), result{code: 2, stderr: "architecture-check: " + dir + ": does not exist\n"})
	})
	t.Run("an empty directory exits 2", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		expect(t, runBinary(t, binary, "", dir), result{code: 2, stderr: "architecture-check: " + dir + ": holds no note\n"})
	})
	t.Run("a file given as the directory exits 2", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "api.md")
		writeFile(t, path, []byte("A note.\n"))
		got := runBinary(t, binary, "", path)
		if prefix := "architecture-check: " + path + ": cannot be read: "; got.code != 2 || got.stdout != "" ||
			!strings.HasPrefix(got.stderr, prefix) || strings.Count(got.stderr, "\n") != 1 {
			t.Errorf("exit %d, stdout %q, stderr %q; want exit 2, no stdout and one line starting %q", got.code, got.stdout, got.stderr, prefix)
		}
	})
	for _, row := range []struct {
		name string
		args []string
	}{
		{name: "no argument exits 2"},
		{name: "two arguments exit 2", args: []string{filepath.Join(testdata, "valid"), filepath.Join(testdata, "mixed")}},
		{name: "an unknown flag exits 2", args: []string{"--bogus", filepath.Join(testdata, "valid")}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got := runBinary(t, binary, "", row.args...)
			if got.code != 2 || got.stdout != "" || !strings.Contains(got.stderr, "usage: architecture-check [--allow-empty] <directory>\n") {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit 2, no stdout and the usage line", got.code, got.stdout, got.stderr)
			}
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
}

// result is one run of the command: its exit status and what it wrote to each stream.
type result struct {
	code   int
	stdout string
	stderr string
}

// runBinary runs the built binary with args in the working directory dir, the test's own when
// empty, and answers its exit status and output.
func runBinary(t *testing.T, binary, dir string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
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

// testdataDir is the absolute path of name under this package's testdata directory.
func testdataDir(t *testing.T, name string) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// names is the names of files, sorted.
func names(files map[string][]byte) []string {
	return slices.Sorted(maps.Keys(files))
}

// writeFile writes content to path.
func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// openRecorder is a directory that records every name opened in it, so a test sees which notes
// the set builder read. It offers fs.FS's Open alone, so fs.ReadDir and fs.ReadFile both reach
// the directory through it.
type openRecorder struct {
	fsys  fs.FS
	names []string
}

func (r *openRecorder) Open(name string) (fs.File, error) {
	r.names = append(r.names, name)
	return r.fsys.Open(name)
}

// grownFS is a directory whose every note grew after it was listed: its listing gives the size
// the MapFS holds, and a read answers notes.MaxFileSize + 1 bytes.
type grownFS struct{ fstest.MapFS }

func (grownFS) ReadFile(string) ([]byte, error) {
	return bytes.Repeat([]byte("a"), notes.MaxFileSize+1), nil
}

// deniedFS is a directory that lists its notes and refuses to read any, as os.DirFS answers a note
// its process may not read: a *fs.PathError naming the note inside the directory.
type deniedFS struct{ fstest.MapFS }

func (deniedFS) ReadFile(name string) ([]byte, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
}
