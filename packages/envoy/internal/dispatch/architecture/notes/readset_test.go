// These tests judge ReadSet over this package's testdata directories, for what a checkout can
// hold, and over in-memory directories, for what a checkout cannot hold or this module must not
// commit: two names that differ only in case, which a case-insensitive filesystem cannot hold,
// and a note that is not valid UTF-8, which the store's test of every *.md file of this module
// sends to Postgres. They test the bound with a note they write. cmd/architecture-check's
// built-binary test reads the same testdata directories.
package notes

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/envoy/internal/runnerlog"
)

// ReadSet over what a checkout holds: a valid set, a set holding a broken note, and a set beside
// entries that are not notes.
func TestReadSetOverTestdata(t *testing.T) {
	t.Parallel()
	t.Run("a valid set", func(t *testing.T) {
		t.Parallel()
		dir := testdataDir(t, "valid")
		set, err := ReadSet(os.DirFS(dir), dir)
		if err != nil || len(set.Rejected) != 0 {
			t.Fatalf("ReadSet(%s): rejected %v, err %v, want neither", dir, set.Rejected, err)
		}
		if got, want := names(set.Files), []string{"checker.md", "rules.md", "text.md"}; !slices.Equal(got, want) {
			t.Fatalf("notes %q, want %q", got, want)
		}
		model, err := Parse(set.Files)
		if err != nil {
			t.Fatalf("Parse: %v, want a valid set", err)
		}
		at := slices.IndexFunc(model.Components, func(component Component) bool { return component.ID == "rules" })
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
		set, err := ReadSet(os.DirFS(dir), dir)
		if err != nil || len(set.Rejected) != 0 {
			t.Fatalf("ReadSet(%s): rejected %v, err %v, want neither", dir, set.Rejected, err)
		}
		if got, want := names(set.Files), []string{"alpha.md", "beta.md"}; !slices.Equal(got, want) {
			t.Fatalf("notes %q, want %q", got, want)
		}
		_, err = Parse(set.Files)
		if err == nil {
			t.Fatal("Parse accepted a note with an unknown front-matter key")
		}
		found := parseProblems(err)
		if len(found) != 1 {
			t.Fatalf("problems %q, want one", found)
		}
		// yaml.v3 answers an unknown key over two lines, which runnerlog.Printable writes as one,
		// its line break as \n.
		line := runnerlog.Printable(found[0].Error())
		if !strings.HasPrefix(line, "beta.md: front matter: ") || !strings.Contains(line, `:\n  line 2: field bogus not found`) || strings.Contains(line, "\n") {
			t.Errorf("problem %q, want one line naming beta.md and its unknown key bogus", line)
		}
	})
	t.Run("entries that are not notes", func(t *testing.T) {
		t.Parallel()
		dir := testdataDir(t, "mixed")
		set, err := ReadSet(os.DirFS(dir), dir)
		if err != nil {
			t.Fatalf("ReadSet(%s): %v", dir, err)
		}
		want := []Rejected{
			{Path: filepath.Join(dir, "link.md"), Kind: KindSymlink},
			{Path: filepath.Join(dir, "notes.txt"), Kind: KindFile},
			{Path: filepath.Join(dir, "sub"), Kind: KindDirectory},
		}
		if !slices.Equal(set.Rejected, want) {
			t.Errorf("rejected %v, want %v", set.Rejected, want)
		}
		if got, want := names(set.Files), []string{"alpha.md", "beta.md"}; !slices.Equal(got, want) {
			t.Fatalf("notes %q, want %q", got, want)
		}
		if _, err := Parse(set.Files); err != nil {
			t.Errorf("Parse: %v, want a valid set", err)
		}
	})
}

// ReadSet over in-memory directories: the two sets a checkout cannot hold or this module must not
// commit, which Parse refuses, and the kinds of entry the predicate decides by mode alone.
func TestReadSetOverMemory(t *testing.T) {
	t.Parallel()
	const dir = "/checkout/.dispatch/architecture"
	note := []byte("---\ntitle: A note\n---\nIts prose.\n")
	for _, row := range []struct {
		name     string
		fsys     fstest.MapFS
		notes    []string
		rejected []Rejected
		problem  string // what a problem Parse answers holds; empty for a valid set
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
			name:     "a symbolic link",
			fsys:     fstest.MapFS{"api.md": {Data: note}, "link.md": {Data: []byte("api.md"), Mode: fs.ModeSymlink | 0o777}},
			notes:    []string{"api.md"},
			rejected: []Rejected{{Path: dir + "/link.md", Kind: KindSymlink}},
		},
		{
			name:  "a file of mode 0755",
			fsys:  fstest.MapFS{"api.md": {Data: note, Mode: 0o755}},
			notes: []string{"api.md"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			set, err := ReadSet(row.fsys, dir)
			if err != nil {
				t.Fatalf("ReadSet: %v", err)
			}
			if got := names(set.Files); !slices.Equal(got, row.notes) {
				t.Errorf("notes %q, want %q", got, row.notes)
			}
			if !slices.Equal(set.Rejected, row.rejected) {
				t.Errorf("rejected %v, want %v", set.Rejected, row.rejected)
			}
			_, err = Parse(set.Files)
			switch {
			case row.problem == "" && err != nil:
				t.Errorf("Parse: %v, want a valid set", err)
			case row.problem != "" && (err == nil || !slices.ContainsFunc(parseProblems(err), func(problem error) bool {
				return strings.Contains(problem.Error(), row.problem)
			})):
				t.Errorf("Parse: %v, want a problem holding %q", err, row.problem)
			}
		})
	}
}

// The bound refuses a set holding a note of MaxFileSize + 1 bytes before ReadSet reads any note,
// so the note beside it, whose front matter holds an unknown key, is never read.
func TestTheBoundRefusesTheSetBeforeAnyNoteIsRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	big := filepath.Join(dir, "big.md")
	writeFile(t, big, bytes.Repeat([]byte("a"), MaxFileSize+1))
	writeFile(t, filepath.Join(dir, "broken.md"), []byte("---\nbogus: 1\n---\nA note with an unknown key.\n"))

	opened := &openRecorder{fsys: os.DirFS(dir)}
	set, err := ReadSet(opened, dir)
	var refused *SizeRefusal
	if !errors.As(err, &refused) || !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("ReadSet: %v, want a *SizeRefusal of ErrFileTooLarge", err)
	}
	if set.Files != nil || refused.Held != 2 || len(refused.Problems) != 1 || !strings.Contains(refused.Problems[0].Error(), big+" is 1048577 bytes") {
		t.Errorf("ReadSet: files %q, %d notes held, problems %q; want no files, 2 notes and one refusal naming %s", names(set.Files), refused.Held, refused.Problems, big)
	}
	if !slices.Equal(opened.names, []string{"."}) {
		t.Errorf("opened %q, want the directory alone: a note was read", opened.names)
	}
}

// The bound checks each note again once it is read, so a note that grew between the listing and
// the read is refused as one the listing showed too large is.
func TestTheBoundRefusesANoteThatGrewAfterItsSizeWasRead(t *testing.T) {
	t.Parallel()
	const dir = "/checkout/.dispatch/architecture"
	set, err := ReadSet(grownFS{fstest.MapFS{"api.md": {Data: []byte("a")}}}, dir)
	var refused *SizeRefusal
	if !errors.As(err, &refused) || !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("ReadSet: %v, want a *SizeRefusal of ErrFileTooLarge", err)
	}
	if set.Files != nil || refused.Held != 1 || len(refused.Problems) != 1 || !strings.Contains(refused.Problems[0].Error(), dir+"/api.md is 1048577 bytes") {
		t.Errorf("ReadSet: files %q, %d notes held, problems %q; want no files, 1 note and one refusal naming %s/api.md", names(set.Files), refused.Held, refused.Problems, dir)
	}
}

// The two bounds of the set refuse a set from its listing, with one finding, before ReadSet reads
// any note: one note more than MaxFiles, and notes whose sizes sum to one byte more than
// MaxSetSize, though each note is within MaxFileSize. A set at each bound is read whole.
func TestTheSetBoundsRefuseTheSetBeforeAnyNoteIsRead(t *testing.T) {
	t.Parallel()
	const dir = "/checkout/.dispatch/spec"
	for _, row := range []struct {
		name  string
		fsys  fstest.MapFS
		want  string
		notes int
	}{
		{name: "one note over MaxFiles", fsys: notesOfSize(MaxFiles+1, 1), notes: MaxFiles + 1, want: "set too large: " + dir + " holds 1001 files (limit 1000)"},
		{name: "one byte over MaxSetSize", fsys: notesSumming(MaxSetSize + 1), notes: MaxSetSize/MaxFileSize + 1, want: "set too large: " + dir + " holds 16777217 bytes in its files (limit 16777216)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			opened := &openRecorder{fsys: row.fsys}
			set, err := ReadSet(opened, dir)
			var refused *SizeRefusal
			if !errors.As(err, &refused) || !errors.Is(err, ErrSetTooLarge) {
				t.Fatalf("ReadSet: %v, want a *SizeRefusal of ErrSetTooLarge", err)
			}
			if set.Files != nil || refused.Held != row.notes || len(refused.Problems) != 1 || refused.Problems[0].Error() != row.want {
				t.Errorf("ReadSet: %d files, %d notes held, problems %q; want no files, %d notes and the one refusal %q", len(set.Files), refused.Held, refused.Problems, row.notes, row.want)
			}
			if !slices.Equal(opened.names, []string{"."}) {
				t.Errorf("opened %d names, want the directory alone: a note was read", len(opened.names))
			}
		})
	}
	for _, row := range []struct {
		name string
		fsys fstest.MapFS
	}{
		{name: "MaxFiles notes", fsys: notesOfSize(MaxFiles, 1)},
		{name: "notes summing MaxSetSize", fsys: notesSumming(MaxSetSize)},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			set, err := ReadSet(row.fsys, dir)
			if err != nil || len(set.Files) != len(row.fsys) {
				t.Fatalf("ReadSet: %d files, err %v; want all %d read", len(set.Files), err, len(row.fsys))
			}
		})
	}
}

// The set's bounds are checked again once the notes are read, so a set that grew between the
// listing and the read past MaxSetSize is refused as one whose listing showed it is: its listing
// sums MaxSetSize, and one note listed empty answers one byte.
func TestTheSetBoundsRefuseASetThatGrewAfterItsSizesWereRead(t *testing.T) {
	t.Parallel()
	const dir = "/checkout/.dispatch/spec"
	fsys := notesSumming(MaxSetSize)
	fsys["zz-empty.md"] = &fstest.MapFile{}
	set, err := ReadSet(oneByteMoreFS{MapFS: fsys, name: "zz-empty.md"}, dir)
	var refused *SizeRefusal
	if !errors.As(err, &refused) || !errors.Is(err, ErrSetTooLarge) || set.Files != nil {
		t.Fatalf("ReadSet: %d files, %v; want no files and a *SizeRefusal of ErrSetTooLarge", len(set.Files), err)
	}
	if want := "set too large: " + dir + " holds 16777217 bytes in its files (limit 16777216)"; len(refused.Problems) != 1 || refused.Problems[0].Error() != want {
		t.Errorf("ReadSet: problems %q, want the one refusal %q", refused.Problems, want)
	}
}

// A note ReadSet cannot read is its "cannot be read" error, naming the note by its absolute path,
// though os.DirFS's error names it by its name inside the directory.
func TestReadSetNamesANoteItCannotReadByItsAbsolutePath(t *testing.T) {
	t.Parallel()
	const dir = "/checkout/.dispatch/architecture"
	set, err := ReadSet(deniedFS{fstest.MapFS{"api.md": {Data: []byte("A note.\n")}}}, dir)
	if !errors.Is(err, fs.ErrPermission) || set.Files != nil {
		t.Fatalf("ReadSet: files %q, err %v; want no files and an error of fs.ErrPermission", names(set.Files), err)
	}
	if want := "cannot be read: open " + dir + "/api.md: permission denied"; err.Error() != want {
		t.Errorf("ReadSet: %q, want %q", err, want)
	}
}

// parseProblems splits Parse's refusal into its problems: the errors.Join it answers holds one
// error for each.
func parseProblems(err error) []error {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return joined.Unwrap()
	}
	return []error{err}
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
// ReadSet read. It offers fs.FS's Open alone, so fs.ReadDir and fs.ReadFile both reach the
// directory through it.
type openRecorder struct {
	fsys  fs.FS
	names []string
}

func (r *openRecorder) Open(name string) (fs.File, error) {
	r.names = append(r.names, name)
	return r.fsys.Open(name)
}

// grownFS is a directory whose every note grew after it was listed: its listing gives the size
// the MapFS holds, and a read answers MaxFileSize + 1 bytes.
type grownFS struct{ fstest.MapFS }

func (grownFS) ReadFile(string) ([]byte, error) {
	return bytes.Repeat([]byte("a"), MaxFileSize+1), nil
}

// deniedFS is a directory that lists its notes and refuses to read any, as os.DirFS answers a note
// its process may not read: a *fs.PathError naming the note inside the directory.
type deniedFS struct{ fstest.MapFS }

func (deniedFS) ReadFile(name string) ([]byte, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
}

// notesOfSize is a directory of count notes of size bytes each, named n0000.md and on.
func notesOfSize(count, size int) fstest.MapFS {
	content := bytes.Repeat([]byte("a"), size)
	fsys := fstest.MapFS{}
	for i := range count {
		fsys[fmt.Sprintf("n%04d.md", i)] = &fstest.MapFile{Data: content}
	}
	return fsys
}

// notesSumming is a directory of notes of MaxFileSize bytes each and one of the rest, whose sizes
// sum to total.
func notesSumming(total int) fstest.MapFS {
	fsys := notesOfSize(total/MaxFileSize, MaxFileSize)
	if rest := total % MaxFileSize; rest > 0 {
		fsys["rest.md"] = &fstest.MapFile{Data: bytes.Repeat([]byte("a"), rest)}
	}
	return fsys
}

// oneByteMoreFS is a directory whose note name grew by one byte after it was listed: its listing
// gives the size the MapFS holds, and a read of it answers one byte more.
type oneByteMoreFS struct {
	fstest.MapFS
	name string
}

func (f oneByteMoreFS) ReadFile(name string) ([]byte, error) {
	content, err := f.MapFS.ReadFile(name)
	if name == f.name {
		content = append(content, 'a')
	}
	return content, err
}
