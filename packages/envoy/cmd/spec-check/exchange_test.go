//go:build unix

package main

import (
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// writeDir changes all of the files of a run or none, in one step: with a step that fails at each
// place, the directory is as it was, each entry the same inode, and nothing is left beside it. The
// directory holds a.yaml, b.yaml and kept.yaml, a file that is no spec file (README), a symbolic
// link and a subdirectory; the run changes a.yaml and b.yaml and adds c.yaml. The faults: the link
// of the spec file that the run keeps (kept.yaml) and its copy; the new file of b.yaml, after
// a.yaml was written; the exchange, which a system that cannot exchange two directories refuses,
// and the two refusals of it that a writer meets, each with its cause and its way out; and a
// directory that the process may not write. A link that the system refuses is no fault: the run
// copies kept.yaml with its mode. The control changes the three files in one step and keeps each
// other entry: the same inode for README, kept.yaml, the subdirectory and the file in it, and the
// same target for the link. A row that needs a user who is not root, a second group or /dev/shm
// skips where the process cannot make its state, and fails in the CI job, which makes each state
// (skipOutsideCI).
func TestWriteDirChangesAllOrNone(t *testing.T) {
	t.Parallel()
	files := map[string][]byte{"a.yaml": []byte("a before\n"), "b.yaml": []byte("b before\n"), "kept.yaml": []byte("kept\n")}
	after := map[string][]byte{"a.yaml": []byte("a after\n"), "b.yaml": []byte("b after\n"), "c.yaml": []byte("c after\n"), "kept.yaml": []byte("kept\n")}
	setUp := func(t *testing.T) (string, *specDirLock) {
		dir := filepath.Join(t.TempDir(), "spec")
		if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range files {
			writeFile(t, filepath.Join(dir, name), content)
		}
		writeFile(t, filepath.Join(dir, "README"), []byte("The spec files.\n"))
		writeFile(t, filepath.Join(dir, "notes", "note.md"), []byte("A note.\n"))
		if err := os.Symlink("README", filepath.Join(dir, "readme-link")); err != nil {
			t.Fatal(err)
		}
		lock, err := lockSpecDir(osFileSystem{}, dir, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { lock.release(true) })
		return dir, lock
	}
	inodes := func(t *testing.T, dir string, names ...string) map[string]uint64 {
		all := map[string]uint64{}
		for _, name := range names {
			all[name] = inodeOf(t, filepath.Join(dir, name))
		}
		return all
	}
	kept := []string{"README", "kept.yaml", "notes", "notes/note.md"}
	// unchanged fails the test unless dir holds the entries before and the inodes ino, and its
	// parent holds dir alone.
	unchanged := func(t *testing.T, dir string, before map[string]string, ino map[string]uint64) {
		t.Helper()
		if got := entries(t, dir); !maps.Equal(got, before) {
			t.Errorf("the directory is %q, want it as it was, %q", got, before)
		}
		if got := inodes(t, dir, slices.Sorted(maps.Keys(ino))...); !maps.Equal(got, ino) {
			t.Errorf("the inodes are %v, want %v", got, ino)
		}
		if names := dirNames(t, filepath.Dir(dir)); !slices.Equal(names, []string{"spec"}) {
			t.Errorf("the parent holds %q, want spec alone", names)
		}
	}
	// failing is the file system of the process with each fault: a call op whose path ends in the
	// fault's name answers the fault's error and changes nothing.
	failing := func(faults ...callFault) fileSystem {
		return callFS{func(op, path string, do func() error) error {
			for _, f := range faults {
				if op == f.op && strings.HasSuffix(path, f.name) {
					return f.err
				}
			}
			return do()
		}}
	}
	const refused = "print changes its files in one step, by an exchange of the directory with a new directory that it builds beside it, and the system refused the exchange: "
	for _, row := range []struct {
		name string
		fsys fileSystem
		err  string
	}{
		{"the link of the spec file that the run keeps, and its copy", failing(callFault{"link", "/kept.yaml", errors.New("link fails")}, callFault{"create", "/kept.yaml", errors.New("create fails")}),
			"link fails, and print could not copy the file either: create fails"},
		{"the new file of b.yaml", failing(callFault{"create", "/b.yaml", errors.New("create fails")}), "create fails"},
		{"the exchange", failing(callFault{"exchange", "/spec", errors.New("exchange fails")}), refused + "exchange fails"},
		{"the exchange of a directory that comes from the layers of a container image", failing(callFault{"exchange", "/spec", syscall.EXDEV}),
			refused + syscall.EXDEV.Error() + ". The file system refuses so to move a directory that comes from the layers of a container image (overlayfs): put a copy of the directory in its place in the container (cp -a <dir> <dir>.copy && rm -rf <dir> && mv <dir>.copy <dir>), or mount the checkout into the container, then run print again"},
		{"the exchange of a directory that is a mount point", failing(callFault{"exchange", "/spec", syscall.EBUSY}),
			refused + syscall.EBUSY.Error() + ". The system refuses so to move a directory that is a mount point: mount a directory above it instead, such as the checkout (docker run -v <checkout>:/work, not -v <checkout>/.dispatch/spec:/work/spec), then run print again"},
		// A file system that does not exchange two directories answers EINVAL (Linux) or ENOTSUP
		// (macOS), and the way out is a checkout on a local file system.
		{"the exchange on a file system that does not exchange two directories, on Linux", failing(callFault{"exchange", "/spec", syscall.EINVAL}),
			refused + syscall.EINVAL.Error() + ". This file system does not exchange two directories, as some network and FUSE file systems do not: run print on a checkout on a local file system of Linux or macOS"},
		{"the exchange on a file system that does not exchange two directories, on macOS", failing(callFault{"exchange", "/spec", syscall.ENOTSUP}),
			refused + syscall.ENOTSUP.Error() + ". This file system does not exchange two directories, as some network and FUSE file systems do not: run print on a checkout on a local file system of Linux or macOS"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir, lock := setUp(t)
			before, ino := entries(t, dir), inodes(t, dir, kept...)
			if warnings, keptEntry, err := writeDir(row.fsys, dir, lock, files, after); err == nil || err.Error() != row.err || len(warnings) != 0 || keptEntry {
				t.Fatalf("writeDir: %v, warnings %q, want %q", err, warnings, row.err)
			}
			unchanged(t, dir, before, ino)
		})
	}
	t.Run("a directory that the process may not write", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			skipOutsideCI(t, "root writes into a directory of mode 555")
		}
		dir, lock := setUp(t)
		before, ino := entries(t, dir), inodes(t, dir, kept...)
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		if _, _, err := writeDir(osFileSystem{}, dir, lock, files, after); !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("writeDir into a directory of mode 555: %v, want a refusal of permission", err)
		}
		unchanged(t, dir, before, ino)
	})
	// print builds its new directory in the parent of the directory, so a parent that the process
	// may not write is refused before print makes any change, with what to change.
	t.Run("a parent that the process may not write", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			skipOutsideCI(t, "root writes into a directory of mode 555")
		}
		dir, lock := setUp(t)
		before, ino := entries(t, dir), inodes(t, dir, kept...)
		parent := filepath.Dir(dir)
		if err := os.Chmod(parent, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
		want := "print builds the directory that takes the place of " + dir + " beside it, in " + parent + ", and this process may not write there (write " + parent + ": permission denied): " +
			"give this process write permission on " + parent + ", or run print as a user who may write there, then run print again"
		if _, _, err := writeDir(osFileSystem{}, dir, lock, files, after); err == nil || err.Error() != want {
			t.Fatalf("writeDir with a parent of mode 555: %v, want %q", err, want)
		}
		if err := os.Chmod(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		unchanged(t, dir, before, ino)
	})
	// A mount point whose parent the process may not write, as a spec directory mounted into a
	// container: the line names the way out of the mount point, since a parent that the
	// process may write would meet the refusal of the exchange next. /dev/shm is such a directory
	// on Linux, for each user but root; writable reads it and changes nothing.
	t.Run("a mount point whose parent the process may not write", func(t *testing.T) {
		t.Parallel()
		const dir = "/dev/shm"
		mount, parent := new(syscall.Stat_t), new(syscall.Stat_t)
		if os.Geteuid() == 0 || syscall.Stat(dir, mount) != nil || syscall.Stat(filepath.Dir(dir), parent) != nil || mount.Dev == parent.Dev || syscall.Access(filepath.Dir(dir), 2) == nil {
			skipOutsideCI(t, dir+" is no mount point that this process may write in a parent that it may not")
		}
		want := "print builds the directory that takes the place of " + dir + " beside it, in /dev, and this process may not write there (write /dev: permission denied): " +
			dir + " is a mount point, which print cannot put another directory in the place of: " + mountPointWayOut
		if err := writable(dir); err == nil || err.Error() != want {
			t.Fatalf("writable(%s): %v, want %q", dir, err, want)
		}
	})
	// A directory whose group this process may give the new directory, and whose giving the system
	// refuses, is refused with the way out: an owner that the process may not give never reaches
	// lchown (mayGive). The process gives the parent one of its other groups and the set-group-ID
	// bit, so the new directory takes that group and differs from the directory's, and print calls
	// lchown, which the row fails as a system that refuses it fails it.
	t.Run("a directory whose group the process may give the new entries, and the system refuses", func(t *testing.T) {
		t.Parallel()
		other := otherGroup(t)
		if other < 0 {
			return
		}
		dir, lock := setUp(t)
		before, ino := entries(t, dir), inodes(t, dir, kept...)
		parent := filepath.Dir(dir)
		if err := os.Chown(parent, -1, other); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(parent, 0o755|fs.ModeSetgid); err != nil {
			t.Fatal(err)
		}
		// The new directory takes the group of its parent, and each file in it the group of the new
		// directory, so the first refusal is of the first file that print writes, a.yaml, which
		// takes the owner and the group of the file a.yaml of dir.
		info, err := os.Lstat(filepath.Join(dir, "a.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		st := info.Sys().(*syscall.Stat_t)
		of := filepath.Join(dir, "a.yaml")
		// The way out depends on whether the process is root (refusedOwner): a run as root is told
		// to run as the owner of the entry, and a run as a user, which asked for a group that it
		// belongs to, to run as root.
		wayOut := func(of string) string {
			if os.Geteuid() == 0 {
				return "run print as the owner of " + of
			}
			return "run print as root"
		}
		want := "print gives the new file a.yaml the group of " + of + ", " + strconv.Itoa(int(st.Gid)) + ", and the system refused it (operation not permitted): " +
			wayOut(of) + ", then run print again"
		if _, _, err := writeDir(failing(callFault{"lchown", "", syscall.EPERM}), dir, lock, files, after); err == nil || err.Error() != want {
			t.Fatalf("writeDir with lchown refused: %v, want %q", err, want)
		}
		unchanged(t, dir, before, ino)
		// A system that gives each file its owner and refuses the new directory's gives the
		// directory's line, and the directory is as it was.
		held, err := os.Lstat(dir)
		if err != nil {
			t.Fatal(err)
		}
		dirStat := held.Sys().(*syscall.Stat_t)
		wantDir := "print gives the new directory the group of " + dir + ", " + strconv.Itoa(int(dirStat.Gid)) + ", and the system refused it (operation not permitted): " +
			wayOut(dir) + ", then run print again"
		if _, _, err := writeDir(failing(callFault{"lchown", filepath.Base(stagingPath(dir, inode(held))), syscall.EPERM}), dir, lock, files, after); err == nil || err.Error() != wantDir {
			t.Fatalf("writeDir with the new directory's lchown refused: %v, want %q", err, wantDir)
		}
		unchanged(t, dir, before, ino)
	})
	// The way out of a system that gives no exchange or no flock names the systems where print runs.
	t.Run("a system that gives no exchange, and a system that gives no flock", func(t *testing.T) {
		t.Parallel()
		for _, refusal := range []string{noExchange, noFlock} {
			if !strings.HasSuffix(refusal, "; run print on Linux or macOS") {
				t.Errorf("%q names no way out, want it to end %q", refusal, "; run print on Linux or macOS")
			}
		}
	})
	// A link that the system refuses: another user's file under fs.protected_hardlinks (EPERM), a
	// file on another file system (EXDEV), a file at its bound of links (EMLINK), a file system with
	// no hard links (ENOTSUP). The run copies kept.yaml, byte for byte and with its mode, 0640.
	for _, errno := range []syscall.Errno{syscall.EPERM, syscall.EXDEV, syscall.EMLINK, syscall.ENOTSUP} {
		t.Run("a link that the system refuses with "+errno.Error(), func(t *testing.T) {
			t.Parallel()
			dir, lock := setUp(t)
			if err := os.Chmod(filepath.Join(dir, "kept.yaml"), 0o640); err != nil {
				t.Fatal(err)
			}
			before, ino := entries(t, dir), inodes(t, dir, kept...)
			fsys := callFS{func(op, path string, do func() error) error {
				if op == "link" {
					return &os.LinkError{Op: "link", Old: filepath.Join(dir, filepath.Base(path)), New: path, Err: errno}
				}
				return do()
			}}
			if warnings, keptEntry, err := writeDir(fsys, dir, lock, files, after); err != nil || len(warnings) != 0 || keptEntry {
				t.Fatalf("writeDir: %v, warnings %q, want the run to copy the file that it may not link", err, warnings)
			}
			want := maps.Clone(before)
			for name, content := range after {
				want[name] = "file " + string(content)
			}
			linked := ino["kept.yaml"]
			delete(ino, "kept.yaml")
			unchanged(t, dir, want, ino)
			if inodeOf(t, filepath.Join(dir, "kept.yaml")) == linked {
				t.Error("kept.yaml is the inode that it was: the run linked the file that the system did not link")
			}
			info, err := os.Lstat(filepath.Join(dir, "kept.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode() != 0o640 {
				t.Errorf("the copy of kept.yaml has the mode %v, want -rw-r----- as the file had", info.Mode())
			}
		})
	}
	t.Run("the control: no step fails", func(t *testing.T) {
		t.Parallel()
		dir, lock := setUp(t)
		before, ino := entries(t, dir), inodes(t, dir, kept...)
		old := inodeOf(t, dir)
		if warnings, keptEntry, err := writeDir(osFileSystem{}, dir, lock, files, after); err != nil || len(warnings) != 0 || keptEntry {
			t.Fatalf("writeDir: %v, warnings %q", err, warnings)
		}
		want := maps.Clone(before)
		for name, content := range after {
			want[name] = "file " + string(content)
		}
		unchanged(t, dir, want, ino)
		if inodeOf(t, dir) == old {
			t.Error("the directory is the same inode: writeDir did not put a new directory in its place")
		}
		// The run holds the lock of the directory that now stands at dir, so that a run that opens
		// dir after the exchange waits for it and finds no live run's directory beside dir.
		f, err := os.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := tryLock(f); !errors.Is(err, errLockHeld) {
			t.Errorf("the lock of the new directory: %v, want it held by the run", err)
		}
	})
}

// writeKilledRecord writes beside the spec directory the record that a run of print writes before
// its exchange (writeRecord), with the text of each spec file that it read, files, for staging, the
// directory that the test leaves there as a killed run leaves it.
func writeKilledRecord(t *testing.T, staging string, files map[string]string) {
	t.Helper()
	texts := make(map[string][]byte, len(files))
	for name, text := range files {
		texts[name] = []byte(text)
	}
	if err := writeRecord(osFileSystem{}, staging, texts); err != nil {
		t.Fatal(err)
	}
}

// callFault is one fault of a call of the seam in a test: the call op of the path that ends in
// name answers err and changes nothing.
type callFault struct {
	op, name string
	err      error
}

// callFS is the file system of the process with each call of the seam made through call, which
// runs it (do) or answers an error in its place, changing nothing: a test fails a call, stops a
// run or records the calls there. op is the name of the method, and path the entry that it
// changes: the new name for link, the directory that print exchanges, and the old name for
// moveNoReplace.
type callFS struct {
	call func(op, path string, do func() error) error
}

func (c callFS) mkdir(path string, perm fs.FileMode) error {
	return c.call("mkdir", path, func() error { return osFileSystem{}.mkdir(path, perm) })
}

func (c callFS) create(path string) (f *os.File, err error) {
	err = c.call("create", path, func() (err error) {
		f, err = osFileSystem{}.create(path)
		return err
	})
	return f, err
}

func (c callFS) write(f *os.File, content []byte) error {
	return c.call("write", f.Name(), func() error { return osFileSystem{}.write(f, content) })
}

func (c callFS) sync(f *os.File) error {
	return c.call("sync", f.Name(), func() error { return osFileSystem{}.sync(f) })
}

func (c callFS) chmod(path string, mode fs.FileMode) error {
	return c.call("chmod", path, func() error { return osFileSystem{}.chmod(path, mode) })
}

func (c callFS) lchown(path string, uid, gid int) error {
	return c.call("lchown", path, func() error { return osFileSystem{}.lchown(path, uid, gid) })
}

func (c callFS) link(oldname, newname string) error {
	return c.call("link", newname, func() error { return osFileSystem{}.link(oldname, newname) })
}

func (c callFS) lock(f *os.File) error {
	return c.call("lock", f.Name(), func() error { return osFileSystem{}.lock(f) })
}

func (c callFS) exchange(a, b string) error {
	return c.call("exchange", b, func() error { return osFileSystem{}.exchange(a, b) })
}

func (c callFS) moveNoReplace(from, to string) error {
	return c.call("move", from, func() error { return osFileSystem{}.moveNoReplace(from, to) })
}

func (c callFS) rename(from, to string) error {
	return c.call("rename", from, func() error { return osFileSystem{}.rename(from, to) })
}

func (c callFS) remove(path string) error {
	return c.call("remove", path, func() error { return osFileSystem{}.remove(path) })
}

// A run of print finishes what a killed run left beside its directory, and keeps each entry of
// the directory: a new directory that the killed run built and did not exchange, a second name of
// a spec file and a new file, which it removes; the old directory that a killed run exchanged out
// of the place of the directory, with the old text of a spec file, which it removes, and a file
// and a subdirectory that are no spec files, which it moves into the directory, the same inode.
// An entry with the name that print builds, made for no run on this directory, and a directory
// that print kept and that holds an entry, it passes over; an empty directory of a kept name,
// which a stop inside keep left, it removes. The run prints a term that the directory already
// holds, and changes no file of it.
func TestPrintRemovesWhatAKilledRunLeft(t *testing.T) {
	t.Parallel()
	term := "---\ntype: term\nid: first-term\nname: first term\ndefinition:\n  - A term is a unit of meaning.\n"
	approved := writeApproved(t, map[string]string{"first-term": term})
	a := readApprovedFile(t, approved)
	run := func(t *testing.T, dir string) (int, string) {
		t.Helper()
		var stderr strings.Builder
		code := printApproved(io.Discard, &stderr, osFileSystem{}, approved, a, []string{"first-term"}, dir, printBounds{limit: 1 << 20, wait: 0})
		return code, stderr.String()
	}
	t.Run("a new directory that the run did not exchange", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "spec")
		if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "terms.yaml"), []byte(term))
		staging := stagingPath(dir, inodeOf(t, dir))
		if err := os.Mkdir(staging, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(dir, "terms.yaml"), filepath.Join(staging, "terms.yaml")); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(staging, "terms-2.yaml"), []byte(term))
		code, stderr := run(t, dir)
		want := "spec-check: print removed " + staging + ", a new directory that a killed run of print built and did not put in the place of " + dir + "\n" +
			"spec-check: warning: " + filepath.Join(dir, "notes") + ": not a spec file (directory); the importer passes over it\n"
		if code != 0 || stderr != want {
			t.Fatalf("print: exit %d, stderr %q, want exit 0 and %q", code, stderr, want)
		}
		if got := entries(t, dir); !maps.Equal(got, map[string]string{"notes": "directory", "terms.yaml": "file " + term}) {
			t.Errorf("the directory holds %q", got)
		}
		if names := dirNames(t, filepath.Dir(dir)); !slices.Equal(names, []string{"spec"}) {
			t.Errorf("the parent holds %q, want spec alone", names)
		}
	})
	t.Run("the old directory that the run exchanged out", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		dir := filepath.Join(root, "spec")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "terms.yaml"), []byte(term))
		old := filepath.Join(root, "old")
		if err := os.MkdirAll(filepath.Join(old, "notes"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(old, "terms.yaml"), []byte(strings.Replace(term, "unit", "part", 1)))
		writeFile(t, filepath.Join(old, "README"), []byte("The spec files.\n"))
		writeFile(t, filepath.Join(old, "notes", "note.md"), []byte("A note.\n"))
		notes := inodeOf(t, filepath.Join(old, "notes"))
		staging := stagingPath(dir, inodeOf(t, old))
		if err := os.Rename(old, staging); err != nil {
			t.Fatal(err)
		}
		writeKilledRecord(t, staging, map[string]string{"terms.yaml": strings.Replace(term, "unit", "part", 1)})
		code, stderr := run(t, dir)
		want := "spec-check: print finished " + staging + ", the directory that a killed run of print put out of the place of " + dir + ": it moved each entry that is no spec file into " + dir + "\n" +
			"spec-check: warning: " + filepath.Join(dir, "README") + ": not a spec file (file): a spec file is a *.yaml file, which spec-check print writes; the importer passes over it\n" +
			"spec-check: warning: " + filepath.Join(dir, "notes") + ": not a spec file (directory); the importer passes over it\n"
		if code != 0 || stderr != want {
			t.Fatalf("print: exit %d, stderr %q, want exit 0 and %q", code, stderr, want)
		}
		if got := entries(t, dir); !maps.Equal(got, map[string]string{"README": "file The spec files.\n", "notes": "directory", "notes/note.md": "file A note.\n", "terms.yaml": "file " + term}) {
			t.Errorf("the directory holds %q", got)
		}
		if got := inodeOf(t, filepath.Join(dir, "notes")); got != notes {
			t.Errorf("notes is inode %d, was %d", got, notes)
		}
		if names := dirNames(t, root); !slices.Equal(names, []string{"spec"}) {
			t.Errorf("the parent holds %q, want spec alone", names)
		}
	})
	// The old directory of a killed run whose record is gone: print cannot tell a text that the run
	// read from one that another program wrote there, so it removes only the second names of the
	// files of the directory, keeps the old directory and exits 1.
	t.Run("the old directory of a killed run with no record", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		dir := filepath.Join(root, "spec")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "terms.yaml"), []byte(term))
		old := filepath.Join(root, "old")
		if err := os.Mkdir(old, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(old, "terms.yaml"), []byte(strings.Replace(term, "unit", "part", 1)))
		staging := stagingPath(dir, inodeOf(t, old))
		if err := os.Rename(old, staging); err != nil {
			t.Fatal(err)
		}
		code, stderr := run(t, dir)
		names := dirNames(t, root)
		if len(names) != 2 || names[1] != "spec" || !strings.HasPrefix(names[0], filepath.Base(staging)+"-kept-") {
			t.Fatalf("the parent holds %q, want spec and the old directory that print kept", names)
		}
		kept := filepath.Join(root, names[0])
		want := "spec-check: print kept " + kept + ", which held " + dir + " before the run: it holds terms.yaml, and print has no record of the texts that the run that left it read, so compare each with the entry of that name in " + dir + "\n"
		if code != 1 || stderr != want {
			t.Fatalf("print: exit %d, stderr %q, want exit 1 and %q", code, stderr, want)
		}
		if got := specFiles(t, kept); !maps.Equal(got, map[string]string{"terms.yaml": strings.Replace(term, "unit", "part", 1)}) {
			t.Errorf("%s holds %q, want the text that no record holds", kept, got)
		}
	})
	// A record whose directory is gone, which a run killed after it finished its old directory
	// leaves, is removed.
	t.Run("a record whose directory is gone", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		dir := filepath.Join(root, "spec")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "terms.yaml"), []byte(term))
		writeKilledRecord(t, stagingPath(dir, inodeOf(t, dir)+7), map[string]string{"terms.yaml": term})
		if code, stderr := run(t, dir); code != 0 || stderr != "" {
			t.Fatalf("print: exit %d, stderr %q, want exit 0 and nothing", code, stderr)
		}
		if names := dirNames(t, root); !slices.Equal(names, []string{"spec"}) {
			t.Errorf("the parent holds %q, want spec alone", names)
		}
	})
	t.Run("an entry that no run on the directory made, and a directory that print kept", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		dir := filepath.Join(root, "spec")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "terms.yaml"), []byte(term))
		other := filepath.Join(root, ".spec"+stagingInfix+strconv.FormatUint(inodeOf(t, dir)+1, 10))
		kept := stagingPath(dir, inodeOf(t, dir)) + "-kept-1"
		for _, path := range []string{other, kept} {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(path, "terms.yaml"), []byte(term))
		}
		code, stderr := run(t, dir)
		want := "spec-check: print passes over " + other + ": it has the name of a directory that print builds beside " + dir + ", and no run of print on " + dir + " made it\n"
		if code != 0 || stderr != want {
			t.Fatalf("print: exit %d, stderr %q, want exit 0 and %q", code, stderr, want)
		}
		for _, path := range []string{other, kept} {
			if got := entries(t, path); !maps.Equal(got, map[string]string{"terms.yaml": "file " + term}) {
				t.Errorf("%s holds %q, want it as it was", path, got)
			}
		}
	})
	// On a system with no rename that refuses an existing name, keep takes the name of the kept
	// directory first, an empty directory, and renames over it, so a kill between the two steps
	// leaves that empty directory. The next run removes it: it holds nothing, so no person has to
	// compare it with anything. A kept directory that holds an entry stays, as the row above holds.
	t.Run("an empty directory of a name that print keeps, which a killed run left", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		dir := filepath.Join(root, "spec")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "terms.yaml"), []byte(term))
		empty := stagingPath(dir, inodeOf(t, dir)) + "-kept-2"
		if err := os.Mkdir(empty, 0o700); err != nil {
			t.Fatal(err)
		}
		if code, stderr := run(t, dir); code != 0 || stderr != "" {
			t.Fatalf("print: exit %d, stderr %q, want exit 0 and no line", code, stderr)
		}
		if names := dirNames(t, root); !slices.Equal(names, []string{"spec"}) {
			t.Errorf("the parent holds %q, want spec alone", names)
		}
	})
	// A removal that the system refuses, of an entry of the spec directory that holds nothing and
	// that a stopped move of print left: the line names the system's refusal, not another program,
	// and the entry stays. The stand-in refuses the removal of the empty file alone.
	t.Run("a removal that the system refuses, of an entry that holds nothing", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		dir := filepath.Join(root, "spec")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "terms.yaml"), []byte(term))
		writeFile(t, filepath.Join(dir, "README"), nil) // the empty name a stopped move took
		old := stagingPath(dir, inodeOf(t, dir))
		if err := os.Mkdir(old, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(old, "README"), []byte("The spec files.\n"))
		refusal := &os.PathError{Op: "unlink", Path: filepath.Join(dir, "README"), Err: syscall.EPERM}
		fsys := callFS{func(op, path string, do func() error) error {
			if op == "remove" && path == filepath.Join(dir, "README") {
				return refusal
			}
			return do()
		}}
		lines, kept := finishOld(fsys, old, dir, nil)
		want := "the system refused print's move or removal of README (" + refusal.Error() + ")"
		if !kept || len(lines) != 1 || !strings.Contains(lines[0], want) {
			t.Fatalf("finishOld: kept %t, lines %q; want kept, and one line that holds %q", kept, lines, want)
		}
		if strings.Contains(lines[0], "another program") {
			t.Errorf("the line blames another program: %q", lines[0])
		}
	})
}

// A run of print that keeps beside its directory an entry that another program made or changed
// exits 1, and its line names what it kept and where: a person must compare that entry with
// the spec directory, and an exit 0 would hide it. The rows: a spec file that another program
// changes in place while the run builds its new directory, so the old directory holds that change;
// a new directory that a killed run left, which holds an entry that print did not make; and the old
// directory that a killed run left, which holds an entry of a name that the spec directory holds
// too. The control is the run of the first row with no other program: exit 0, nothing beside.
func TestPrintExitsOneWhenItKeepsAnEntryOfAnotherProgram(t *testing.T) {
	t.Parallel()
	first := "---\ntype: term\nid: first-term\nname: first term\ndefinition:\n  - A term is a unit of meaning.\n"
	second := strings.Replace(first, "unit", "part", 1)
	other := strings.Replace(first, "unit", "piece", 1)
	approved := writeApproved(t, map[string]string{"first-term": second})
	a := readApprovedFile(t, approved)
	run := func(t *testing.T, dir string, fsys fileSystem) (int, string) {
		t.Helper()
		var stderr strings.Builder
		code := printApproved(io.Discard, &stderr, fsys, approved, a, []string{"first-term"}, dir, printBounds{limit: 1 << 20, wait: 0})
		return code, stderr.String()
	}
	// keptBeside is the one directory beside dir whose name says that print kept it; a failure names
	// the exit and the lines of the run that should have kept it.
	keptBeside := func(t *testing.T, dir string, code int, stderr string) string {
		t.Helper()
		var kept []string
		for _, name := range dirNames(t, filepath.Dir(dir)) {
			if strings.Contains(name, "-kept-") {
				kept = append(kept, filepath.Join(filepath.Dir(dir), name))
			}
		}
		if len(kept) != 1 {
			t.Fatalf("beside %s: %q, want one directory that print kept; the run exited %d with %q", dir, dirNames(t, filepath.Dir(dir)), code, stderr)
		}
		return kept[0]
	}
	// changeAtExchange writes other into terms.yaml of the spec directory in place, as another
	// program does, just before the exchange.
	changeAtExchange := callFS{func(op, path string, do func() error) error {
		if op == "exchange" {
			if err := os.WriteFile(filepath.Join(path, "terms.yaml"), []byte(other), 0o644); err != nil {
				return err
			}
		}
		return do()
	}}
	t.Run("a spec file that another program changes while the run builds its new directory", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "spec")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "terms.yaml"), []byte(first))
		code, stderr := run(t, dir, changeAtExchange)
		kept := keptBeside(t, dir, code, stderr)
		want := "spec-check: print kept " + kept + ", which held " + dir + " before the run: another program made or changed terms.yaml there while print ran, so compare each with the entry of that name in " + dir + "\n"
		if code != 1 || stderr != want {
			t.Fatalf("print: exit %d, stderr %q, want exit 1 and %q", code, stderr, want)
		}
		if got := specFiles(t, kept); !maps.Equal(got, map[string]string{"terms.yaml": other}) {
			t.Errorf("%s holds %q, want the change of the other program", kept, got)
		}
		if got := specFiles(t, dir); !maps.Equal(got, map[string]string{"terms.yaml": second}) {
			t.Errorf("%s holds %q, want the run's text", dir, got)
		}
	})
	// A move back that the system refuses, after the exchange, beside a spec file that another
	// program changed: the line gives each cause once, each with its own entries, and the change of
	// the other program is not blamed on the system. The stand-in answers EIO for the move of the
	// README and nothing else.
	t.Run("a move back that the system refuses, beside a spec file that another program changed", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "spec")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "terms.yaml"), []byte(first))
		writeFile(t, filepath.Join(dir, "README"), []byte("The spec files.\n"))
		refusal := &os.LinkError{Op: "renameat2", Old: "README", New: "README", Err: syscall.EIO}
		fsys := callFS{func(op, path string, do func() error) error {
			if op == "move" && filepath.Base(path) == "README" {
				return refusal
			}
			return changeAtExchange.call(op, path, do)
		}}
		code, stderr := run(t, dir, fsys)
		kept := keptBeside(t, dir, code, stderr)
		want := "spec-check: warning: " + filepath.Join(dir, "README") + ": not a spec file (file): a spec file is a *.yaml file, which spec-check print writes; the importer passes over it\n" +
			"spec-check: print kept " + kept + ", which held " + dir + " before the run: another program made or changed terms.yaml there while print ran; " +
			"the system refused print's move or removal of README (" + refusal.Error() + "), so compare each with the entry of that name in " + dir + "\n"
		if code != 1 || stderr != want {
			t.Fatalf("print: exit %d, stderr %q, want exit 1 and %q", code, stderr, want)
		}
	})
	t.Run("a new directory that a killed run left, with an entry that print did not make", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "spec")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "terms.yaml"), []byte(second))
		staging := stagingPath(dir, inodeOf(t, dir))
		if err := os.MkdirAll(filepath.Join(staging, "notes"), 0o755); err != nil {
			t.Fatal(err)
		}
		code, stderr := run(t, dir, osFileSystem{})
		kept := keptBeside(t, dir, code, stderr)
		want := "spec-check: print kept " + kept + ", which a killed run of print left beside " + dir + ": it holds notes, which print did not make\n"
		if code != 1 || stderr != want {
			t.Fatalf("print: exit %d, stderr %q, want exit 1 and %q", code, stderr, want)
		}
	})
	t.Run("the old directory that a killed run left, with an entry of a name that the directory holds", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		dir := filepath.Join(root, "spec")
		if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
			t.Fatal(err)
		}
		// The directory holds a note, so it is no entry that holds nothing, which a stopped move
		// of print leaves and the next run finishes (holdsNothing).
		writeFile(t, filepath.Join(dir, "notes", "note.md"), []byte("A note.\n"))
		writeFile(t, filepath.Join(dir, "terms.yaml"), []byte(second))
		old := filepath.Join(root, "old")
		if err := os.MkdirAll(filepath.Join(old, "notes"), 0o755); err != nil {
			t.Fatal(err)
		}
		staging := stagingPath(dir, inodeOf(t, old))
		if err := os.Rename(old, staging); err != nil {
			t.Fatal(err)
		}
		writeKilledRecord(t, staging, map[string]string{})
		code, stderr := run(t, dir, osFileSystem{})
		kept := keptBeside(t, dir, code, stderr)
		want := "spec-check: print kept " + kept + ", which held " + dir + " before the run: another program made or changed notes there while print ran, so compare each with the entry of that name in " + dir + "\n" +
			"spec-check: warning: " + filepath.Join(dir, "notes") + ": not a spec file (directory); the importer passes over it\n"
		if code != 1 || stderr != want {
			t.Fatalf("print: exit %d, stderr %q, want exit 1 and %q", code, stderr, want)
		}
	})
	// The loss path: the run cannot give the old directory, which holds the change of another program,
	// another name, because the system refuses the rename (here its parent became a directory that
	// the process may not write, after the exchange), so the old directory stays at the name of print's
	// own directory beside dir. The next run finishes it as print's own, and removes no change that its
	// run did not record: it keeps the directory with the change, and exits 1 naming it.
	t.Run("a change of another program in an old directory that print cannot rename, which the next run keeps", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			skipOutsideCI(t, "root may write a directory of mode 555, so the rename that the row needs to fail does not fail")
		}
		root := t.TempDir()
		dir := filepath.Join(root, "spec")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "terms.yaml"), []byte(first))
		staging := stagingPath(dir, inodeOf(t, dir))
		t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
		parentLocked := callFS{func(op, path string, do func() error) error {
			if op == "move" && path == staging {
				if err := os.Chmod(root, 0o555); err != nil {
					return err
				}
			}
			return changeAtExchange.call(op, path, do)
		}}
		code, stderr := run(t, dir, parentLocked)
		if code != 1 || !strings.HasPrefix(stderr, "spec-check: print could not remove "+staging+", which held "+dir+" before the run: another program made or changed terms.yaml there while print ran: ") {
			t.Fatalf("print: exit %d, stderr %q, want exit 1 and the line of %s, which it could not rename", code, stderr, staging)
		}
		if err := os.Chmod(root, 0o755); err != nil {
			t.Fatal(err)
		}
		code, stderr = run(t, dir, osFileSystem{})
		kept := keptBeside(t, dir, code, stderr)
		want := "spec-check: print kept " + kept + ", which held " + dir + " before the run: another program made or changed terms.yaml there while print ran, so compare each with the entry of that name in " + dir + "\n"
		if code != 1 || stderr != want {
			t.Fatalf("the next print: exit %d, stderr %q, want exit 1 and %q", code, stderr, want)
		}
		if got := specFiles(t, kept); !maps.Equal(got, map[string]string{"terms.yaml": other}) {
			t.Errorf("%s holds %q, want the change of the other program", kept, got)
		}
		if got := specFiles(t, dir); !maps.Equal(got, map[string]string{"terms.yaml": second}) {
			t.Errorf("%s holds %q, want the run's text", dir, got)
		}
	})
	t.Run("the control: no other program", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "spec")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "terms.yaml"), []byte(first))
		if code, stderr := run(t, dir, osFileSystem{}); code != 0 || stderr != "" {
			t.Fatalf("print: exit %d, stderr %q, want exit 0 and nothing", code, stderr)
		}
		if names := dirNames(t, filepath.Dir(dir)); !slices.Equal(names, []string{"spec"}) {
			t.Errorf("the parent holds %q, want spec alone", names)
		}
	})
}

// specFiles is the text of each spec file of dir: each regular file directly in it whose name
// ends in .yaml.
func specFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for name, entry := range entries(t, dir) {
		if text, ok := strings.CutPrefix(entry, "file "); ok && !strings.Contains(name, "/") && strings.HasSuffix(name, specgraph.FileSuffix) {
			files[name] = text
		}
	}
	return files
}

// entries is each entry under dir, by its path from dir: "file <text>" for a regular file, "link
// <target>" for a symbolic link, and "directory" for a directory.
func entries(t *testing.T, dir string) map[string]string {
	t.Helper()
	all := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		name, _ := filepath.Rel(dir, path)
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			all[name] = "link " + target
			return err
		case entry.IsDir():
			all[name] = "directory"
		default:
			content, err := os.ReadFile(path)
			all[name] = "file " + string(content)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return all
}

// copyTree is a copy of dir, its files with their modes, symbolic links and subdirectories, in a
// new temporary directory.
func copyTree(t *testing.T, dir string) string {
	t.Helper()
	copied := filepath.Join(t.TempDir(), filepath.Base(dir))
	if err := os.Mkdir(copied, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range slices.Sorted(maps.Keys(entries(t, dir))) {
		path, to := filepath.Join(dir, name), filepath.Join(copied, name)
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err == nil {
				err = os.Symlink(target, to)
			}
			if err != nil {
				t.Fatal(err)
			}
		case info.IsDir():
			if err := os.Mkdir(to, 0o755); err != nil {
				t.Fatal(err)
			}
		default:
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, to, content)
			if err := os.Chmod(to, info.Mode().Perm()); err != nil {
				t.Fatal(err)
			}
		}
	}
	return copied
}

// dirNames is the name of each entry directly in dir, sorted.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range list {
		names = append(names, entry.Name())
	}
	return names
}

// inodeOf is the inode of the entry at path, not followed when it is a symbolic link.
func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(info.Sys().(*syscall.Stat_t).Ino)
}
