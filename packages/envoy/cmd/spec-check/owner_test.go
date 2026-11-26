//go:build unix

package main

import (
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
// that it replaces, and a new file gets the owner and the group of the spec directory: a run as
// root in a container over a bind-mounted checkout leaves the host user's files as they were. A
// file that the run does not change, which print links or, where the system refuses the link,
// copies, keeps them too. The group tells the writer from the owner of the file: the test gives
// the file and the directory a group of the process that is not its own. The control is the file
// mode 0644 and the process's own group, which print writes either way.
func TestPrintKeepsTheModeAndTheOwnerOfEachFile(t *testing.T) {
	t.Parallel()
	other := -1
	for _, g := range mustGroups(t) {
		if g != os.Getgid() {
			other = g
			break
		}
	}
	if other < 0 {
		skipOutsideCI(t, "the process is in no group but its own, so no file of another group can be made")
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

// mustGroups is each group of the process.
func mustGroups(t *testing.T) []int {
	t.Helper()
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	return slices.Concat(groups, []int{os.Getgid()})
}
