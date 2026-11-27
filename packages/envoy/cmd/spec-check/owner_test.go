//go:build unix

package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// Each file that print writes keeps the mode, the owner and the group of the file of that name
// that it replaces, and a new file gets the owner and the group of the spec directory, where the
// process may give them (TestSameOwnerGivesOnlyAnOwnerThatTheProcessMayGive): a run as root in a
// container over a bind-mounted checkout leaves the host user's files as they were. A file that
// the run does not change, which print links or, where the system refuses the link, copies, keeps
// them too. The group tells the writer from the owner of the file: the test gives the file and the
// directory a group of the process that is not its own. The control is the file mode 0644 and the
// process's own group, which print writes either way.
func TestPrintKeepsTheModeAndTheOwnerOfEachFile(t *testing.T) {
	t.Parallel()
	other := otherGroup(t)
	if other < 0 {
		return
	}
	const changed = "---\ntype: term\nid: first-term\nname: first term\ndefinition:\n  - A term is a unit of meaning.\n"
	const kept = "---\ntype: term\nid: kept-term\nname: kept term\ndefinition:\n  - A term stays as it is.\n"
	approvedChanged := strings.Replace(changed, "unit", "part", 1)
	const added = "---\ntype: component\nid: a-part\nname: a part\nkind: component\npurpose:\n  - It holds a part.\n"
	approved := writeApproved(t, map[string]string{"first-term": approvedChanged, "kept-term": kept, "a-part": added})
	a := readApprovedFile(t, approved)
	for _, row := range []struct {
		name  string
		mode  fs.FileMode
		group int
		fsys  fileSystem
	}{
		{"the control: mode 0644 and the group of the process", 0o644, os.Getgid(), osFileSystem{}},
		{"mode 0600 and another group", 0o600, other, osFileSystem{}},
		{"mode 0640 and another group, where the system refuses a hard link", 0o640, other, callFS{func(op, path string, do func() error) error {
			if op == "link" && strings.HasSuffix(path, "kept.yaml") {
				return &os.LinkError{Op: "link", Old: path, New: path, Err: syscall.EXDEV}
			}
			return do()
		}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "spec")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(dir, "terms.yaml"), []byte(changed))
			writeFile(t, filepath.Join(dir, "kept.yaml"), []byte(kept))
			for _, path := range []string{dir, filepath.Join(dir, "terms.yaml"), filepath.Join(dir, "kept.yaml")} {
				if err := os.Lchown(path, -1, row.group); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"terms.yaml", "kept.yaml"} {
				if err := os.Chmod(filepath.Join(dir, name), row.mode); err != nil {
					t.Fatal(err)
				}
			}
			var stderr strings.Builder
			if code := printApproved(io.Discard, &stderr, row.fsys, approved, a, []string{"first-term", "a-part"}, dir, printBounds{limit: 1 << 20}); code != 0 {
				t.Fatalf("print: exit %d, stderr %q", code, stderr.String())
			}
			if got := specFiles(t, dir); got["terms.yaml"] != approvedChanged || got["kept.yaml"] != kept || got["components.yaml"] != added {
				t.Fatalf("%s holds %q, want the run's texts", dir, got)
			}
			for name, want := range map[string]fs.FileMode{"terms.yaml": row.mode, "kept.yaml": row.mode, "components.yaml": 0o644} {
				info, err := os.Lstat(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != want {
					t.Errorf("%s has the mode %v, want %v", name, info.Mode().Perm(), want)
				}
				if gid := int(info.Sys().(*syscall.Stat_t).Gid); gid != row.group {
					t.Errorf("%s has the group %d, want %d", name, gid, row.group)
				}
			}
		})
	}
}

// sameOwner asks the system only for what the process may give the entry that print made and that
// entry does not already have: a process as root may give any owner and group; another may give
// its own user, which each entry that it makes already has, and any group that it belongs to, as
// chown(2) lets the owner of an entry give it a group of its own. So a member of the spec
// directory's group who runs print leaves the group its access, and a run over an entry of
// another user writes it as itself, as git does. The rows read what sameOwner asks of the system
// through a stand-in that records each call and answers nothing. The three rows that ask for
// nothing are the controls, and the row of another user with a group of the process is the one
// that asked for nothing before mayGive answered each part.
func TestSameOwnerAsksForWhatTheProcessMayGive(t *testing.T) {
	t.Parallel()
	group := otherGroup(t)
	if group < 0 {
		return
	}
	if os.Geteuid() == 0 {
		skipOutsideCI(t, "the process is root, which may give any owner, so it never gives a group alone")
		return
	}
	notOurs := 0
	for slices.Contains(mustGroups(t), notOurs) {
		notOurs++
	}
	for _, row := range []struct {
		name     string
		uid, gid int
		want     string
	}{
		{"another of the process's groups, which the entry does not have", os.Geteuid(), group, fmt.Sprintf("-1:%d", group)},
		{"another user and a group of the process", os.Geteuid() + 1, group, fmt.Sprintf("-1:%d", group)},
		{"the control: another user and a group that the process is not in", os.Geteuid() + 1, notOurs, ""},
		{"the control: another user and the group that the entry already has", os.Geteuid() + 1, os.Getegid(), ""},
		{"the control: the process's own user and group, which the entry already has", os.Geteuid(), os.Getegid(), ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "spec.yaml")
			writeFile(t, path, []byte("---\n"))
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			var asked []string
			fsys := ownerFS{callFS{func(op, p string, do func() error) error { return do() }}, &asked}
			if err := sameOwner(fsys, path, "the new file spec.yaml", path, ownedBy{info, uint32(row.uid), uint32(row.gid)}); err != nil {
				t.Fatalf("sameOwner: %v", err)
			}
			if got := strings.Join(asked, " "); got != row.want {
				t.Errorf("sameOwner asked the system for %q, want %q", got, row.want)
			}
		})
	}
}

// ownerFS records the owner and the group of each lchown that it is asked for, as "<uid>:<gid>",
// and gives it to no system.
type ownerFS struct {
	callFS
	asked *[]string
}

func (o ownerFS) lchown(_ string, uid, gid int) error {
	*o.asked = append(*o.asked, fmt.Sprintf("%d:%d", uid, gid))
	return nil
}

// A spec file that another program turns into a symbolic link after the run read it, and before
// print writes the new file in its place, is no file whose mode print may keep: a link's mode is
// 0777. print refuses the run with that line and changes nothing, and the link stays. The stand-in
// makes the link when print makes its new directory, after the read. The control is the same run
// with no link made, which writes the file with its old mode.
func TestPrintRefusesASpecFileThatBecameALinkAfterTheRead(t *testing.T) {
	t.Parallel()
	const term = "---\ntype: term\nid: first-term\nname: first term\ndefinition:\n  - A term is a unit of meaning.\n"
	approvedTerm := strings.Replace(term, "unit", "part", 1)
	approved := writeApproved(t, map[string]string{"first-term": approvedTerm})
	a := readApprovedFile(t, approved)
	for _, swap := range []bool{false, true} {
		t.Run(map[bool]string{false: "the control: no link", true: "a link in the place of the file"}[swap], func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "spec")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, "terms.yaml")
			writeFile(t, file, []byte(term))
			if err := os.Chmod(file, 0o600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "elsewhere.yaml")
			writeFile(t, target, []byte(term))
			made := false
			fsys := callFS{func(op, path string, do func() error) error {
				if op == "mkdir" && swap && !made {
					made = true
					if err := os.Remove(file); err != nil {
						return err
					}
					if err := os.Symlink(target, file); err != nil {
						return err
					}
				}
				return do()
			}}
			var stderr strings.Builder
			code := printApproved(io.Discard, &stderr, fsys, approved, a, []string{"first-term"}, dir, printBounds{limit: 1 << 20})
			info, err := os.Lstat(file)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case !swap && (code != 0 || info.Mode() != 0o600):
				t.Errorf("print: exit %d, stderr %q, the file of mode %v; want exit 0 and mode 0600", code, stderr.String(), info.Mode())
			case swap && (code != 2 || !strings.Contains(stderr.String(), "is no regular file") || info.Mode()&fs.ModeSymlink == 0):
				t.Errorf("print: exit %d, stderr %q, the entry of mode %v; want exit 2, the refusal of the link, and the link as it was", code, stderr.String(), info.Mode())
			}
			if names := dirNames(t, filepath.Dir(dir)); !slices.Equal(names, []string{"spec"}) {
				t.Errorf("beside %s: %q, want nothing", dir, names)
			}
		})
	}
}

// otherGroup is a group of the process that is not its effective group. A process in no other
// group fails the test in a CI job and skips it outside one (skipOutsideCI), and answers -1.
func otherGroup(t *testing.T) int {
	t.Helper()
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g != os.Getegid() {
			return g
		}
	}
	skipOutsideCI(t, fmt.Sprintf("the process is in no group but its own (%d), so no entry of another group can be made", os.Getegid()))
	return -1
}

// ownedBy describes an entry of the owner uid and the group gid, as os.Lstat of such an entry does
// to sameOwner, which reads only Sys.
type ownedBy struct {
	fs.FileInfo
	uid, gid uint32
}

func (o ownedBy) Sys() any { return &syscall.Stat_t{Uid: o.uid, Gid: o.gid} }

// mustGroups is each group of the process.
func mustGroups(t *testing.T) []int {
	t.Helper()
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	return slices.Concat(groups, []int{os.Getgid()})
}
