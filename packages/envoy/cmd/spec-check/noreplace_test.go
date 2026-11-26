//go:build unix

package main

import (
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// A file system can exchange two directories and refuse a rename that keeps an existing name: a
// pod's gVisor volume (v9fs) exchanges two directories with renameat2 and RENAME_EXCHANGE, and
// answers EINVAL for RENAME_NOREPLACE. On such a system print still moves each entry of the
// directory that is no spec file back into it after the exchange (a file that is no spec file, a
// subdirectory and a symbolic link), and leaves nothing beside it: it moves a file or a link by a
// second name and a removal, which refuse an existing name, and a directory by a rename after it
// finds no entry of that name. The stand-in answers EINVAL for each rename with no-replace. A
// system that refuses those moves too refuses print before any change, and the line names the
// system's refusal: the directory is as it was, and nothing is beside it. The control is the same
// run on the file system of the process.
func TestPrintOnAFileSystemThatRefusesARenameWithNoReplace(t *testing.T) {
	t.Parallel()
	first := "---\ntype: term\nid: first-term\nname: first term\ndefinition:\n  - A term is a unit of meaning.\n"
	second := strings.Replace(first, "unit", "part", 1)
	approved := writeApproved(t, map[string]string{"first-term": second})
	a := readApprovedFile(t, approved)
	// setUp is a spec directory of one spec file, a file that is no spec file, a subdirectory with a
	// file in it and a symbolic link.
	setUp := func(t *testing.T) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "spec")
		if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "terms.yaml"), []byte(first))
		writeFile(t, filepath.Join(dir, "README"), []byte("The spec files.\n"))
		writeFile(t, filepath.Join(dir, "notes", "a.txt"), []byte("A note.\n"))
		if err := os.Symlink("terms.yaml", filepath.Join(dir, "link")); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	run := func(t *testing.T, dir string, fsys fileSystem) (int, string) {
		t.Helper()
		var stderr strings.Builder
		code := printApproved(io.Discard, &stderr, fsys, approved, a, []string{"first-term"}, dir, printBounds{limit: 1 << 20, wait: 0})
		return code, stderr.String()
	}
	// others is each entry of dir that is no spec file, with its text or its target.
	others := func(t *testing.T, dir string) map[string]string {
		t.Helper()
		got := map[string]string{}
		for _, path := range []string{"README", "notes/a.txt"} {
			content, err := os.ReadFile(filepath.Join(dir, path))
			if err != nil {
				t.Errorf("%s: %v", path, err)
				continue
			}
			got[path] = string(content)
		}
		target, err := os.Readlink(filepath.Join(dir, "link"))
		if err != nil {
			t.Errorf("link: %v", err)
		}
		got["link"] = target
		return got
	}
	wantOthers := map[string]string{"README": "The spec files.\n", "notes/a.txt": "A note.\n", "link": "terms.yaml"}
	refuse := func(ops ...string) callFS {
		return callFS{func(op, path string, do func() error) error {
			if slices.Contains(ops, op) {
				return &os.PathError{Op: op, Path: path, Err: syscall.EINVAL}
			}
			return do()
		}}
	}
	for _, row := range []struct {
		name string
		fsys fileSystem
	}{
		{"the control: the file system of the process", osFileSystem{}},
		{"a file system that refuses a rename with no-replace", refuse("move")},
		{"a file system that refuses a rename with no-replace and a second name", refuse("move", "link")},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir := setUp(t)
			code, stderr := run(t, dir, row.fsys)
			if code != 0 || strings.Contains(stderr, "print kept") || strings.Contains(stderr, "could not") {
				t.Fatalf("print: exit %d, stderr %q, want exit 0 and no entry kept", code, stderr)
			}
			if got := specFiles(t, dir); got["terms.yaml"] != second || len(got) != 1 {
				t.Errorf("%s holds the spec files %q, want the run's text alone", dir, got)
			}
			if got := others(t, dir); !maps.Equal(got, wantOthers) {
				t.Errorf("%s holds %q, want %q", dir, got, wantOthers)
			}
			if names := dirNames(t, filepath.Dir(dir)); !slices.Equal(names, []string{"spec"}) {
				t.Errorf("beside %s: %q, want nothing", dir, names)
			}
		})
	}
	t.Run("a file system that refuses each move refuses print before any change", func(t *testing.T) {
		t.Parallel()
		dir := setUp(t)
		code, stderr := run(t, dir, refuse("move", "link", "rename"))
		if code != 2 || !strings.Contains(stderr, "invalid argument") || !strings.Contains(stderr, "run print on a checkout on a local file system") {
			t.Fatalf("print: exit %d, stderr %q, want exit 2, the system's refusal and the way out", code, stderr)
		}
		if got := specFiles(t, dir); got["terms.yaml"] != first || len(got) != 1 {
			t.Errorf("%s holds the spec files %q, want them as they were", dir, got)
		}
		if got := others(t, dir); !maps.Equal(got, wantOthers) {
			t.Errorf("%s holds %q, want %q", dir, got, wantOthers)
		}
		if names := dirNames(t, filepath.Dir(dir)); !slices.Equal(names, []string{"spec"}) {
			t.Errorf("beside %s: %q, want nothing", dir, names)
		}
	})
}

// The fallback move, on a system with no rename that refuses an existing name, replaces no entry
// that another program made at the name: print takes the name itself first, an empty directory for
// a directory and a new file for a file, so another program that makes the name meanwhile gets the
// refusal, and print replaces nothing but what it made. The rows: a directory and a file whose
// hard link the system refuses, which falls through to the rename; in each, another program makes
// the name at the one moment the move could replace it, which the stand-in plays at each call by
// which print takes a name or renames. The control is each move with no other program, which moves
// the entry.
func TestTheFallbackMoveReplacesNoEntryOfAnotherProgram(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name      string
		directory bool
		other     bool
	}{
		{"the control: a directory, no other program", true, false},
		{"the control: a file whose link the system refuses, no other program", false, false},
		{"a directory, and another program makes the name", true, true},
		{"a file whose link the system refuses, and another program makes the name", false, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			from, to := filepath.Join(root, "old", "notes"), filepath.Join(root, "spec", "notes")
			if err := os.Mkdir(filepath.Join(root, "old"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Dir(to), 0o755); err != nil {
				t.Fatal(err)
			}
			const text = "A note of the old directory.\n"
			if row.directory {
				if err := os.Mkdir(from, 0o755); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(from, "note.md"), []byte(text))
			} else {
				writeFile(t, from, []byte(text))
			}
			// made is the inode of the entry that the other program holds at the name, 0 for none:
			// it takes the name at the first call by which print takes it or renames over it, which
			// is the one moment a move could replace it.
			var made uint64
			fsys := callFS{func(op, path string, do func() error) error {
				if op == "link" && !row.directory {
					return &os.LinkError{Op: "link", Old: from, New: path, Err: syscall.EPERM}
				}
				if row.other && made == 0 && (op == "rename" || op == "mkdir" || op == "create") {
					if err := os.WriteFile(to, []byte("Another program's file.\n"), 0o644); err == nil {
						made = inodeOf(t, to)
					}
				}
				return do()
			}}
			err := moveByHand(fsys, from, to)
			switch {
			case made != 0:
				if err == nil || !fileExists(from) || inodeOf(t, to) != made {
					t.Errorf("moveByHand: %v; want a refusal, the entry of the other program at %s as it made it, and the entry still at %s", err, to, from)
				}
			case row.other:
				t.Logf("the other program could not take the name, so print held it first: %v", err)
				fallthrough
			default:
				moved := fileExists(to) && !fileExists(from)
				if row.directory {
					moved = moved && fileExists(filepath.Join(to, "note.md"))
				}
				if err != nil || !moved {
					t.Errorf("moveByHand: %v; want the entry moved to %s", err, to)
				}
			}
		})
	}
}

// fileExists reports whether path names an entry.
func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
