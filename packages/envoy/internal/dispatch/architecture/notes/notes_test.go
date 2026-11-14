package notes

import (
	"errors"
	"io/fs"
	"testing"
)

// A GitHub tree entry is a note only when it is a blob of mode 100644 or
// 100755 named *.md directly inside the directory.
func TestGitKindAndIsNote(t *testing.T) {
	for name, tc := range map[string]struct {
		objectType, mode, path string
		kind                   Kind
		note                   bool
	}{
		"blob 100644":          {objectType: "blob", mode: "100644", path: "a.md", kind: KindFile, note: true},
		"blob 100755":          {objectType: "blob", mode: "100755", path: "a.md", kind: KindFile, note: true},
		"symlink 120000":       {objectType: "blob", mode: "120000", path: "a.md", kind: KindSymlink},
		"tree 040000":          {objectType: "tree", mode: "040000", path: "a.md", kind: KindDirectory},
		"submodule 160000":     {objectType: "commit", mode: "160000", path: "a.md", kind: KindSubmodule},
		"blob of another mode": {objectType: "blob", mode: "100600", path: "a.md", kind: KindOther},
		"unknown object type":  {objectType: "tag", mode: "100644", path: "a.md", kind: KindOther},
		"nested path":          {objectType: "blob", mode: "100644", path: "a/b.md", kind: KindFile},
		"not markdown":         {objectType: "blob", mode: "100644", path: "notes.txt", kind: KindFile},
	} {
		t.Run(name, func(t *testing.T) {
			kind := GitKind(tc.objectType, tc.mode)
			if kind != tc.kind {
				t.Fatalf("GitKind(%q, %q) = %v, want %v", tc.objectType, tc.mode, kind, tc.kind)
			}
			if got := IsNote(tc.path, kind); got != tc.note {
				t.Errorf("IsNote(%q, %v) = %t, want %t", tc.path, kind, got, tc.note)
			}
		})
	}
}

// A checkout entry is a note only when it is a regular file, whatever its
// permission bits, named *.md.
func TestFileKindAndIsNote(t *testing.T) {
	for name, tc := range map[string]struct {
		mode fs.FileMode
		kind Kind
		note bool
	}{
		"regular file 0644": {mode: 0o644, kind: KindFile, note: true},
		"regular file 0755": {mode: 0o755, kind: KindFile, note: true},
		"symbolic link":     {mode: fs.ModeSymlink | 0o777, kind: KindSymlink},
		"directory":         {mode: fs.ModeDir | 0o755, kind: KindDirectory},
		"named pipe":        {mode: fs.ModeNamedPipe | 0o644, kind: KindOther},
	} {
		t.Run(name, func(t *testing.T) {
			kind := FileKind(tc.mode)
			if kind != tc.kind {
				t.Fatalf("FileKind(%v) = %v, want %v", tc.mode, kind, tc.kind)
			}
			if got := IsNote("a.md", kind); got != tc.note {
				t.Errorf("IsNote(%q, %v) = %t, want %t", "a.md", kind, got, tc.note)
			}
		})
	}
}

func TestKindString(t *testing.T) {
	for kind, want := range map[Kind]string{
		KindFile:      "file",
		KindSymlink:   "symbolic link",
		KindDirectory: "directory",
		KindSubmodule: "submodule",
		KindOther:     "other entry",
	} {
		if got := kind.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", int(kind), got, want)
		}
	}
}

// A note of exactly MaxFileSize bytes passes; one byte more is
// ErrFileTooLarge naming the path, the size and the limit.
func TestCheckSize(t *testing.T) {
	const path = ".dispatch/architecture/big.md"
	if err := CheckSize(path, MaxFileSize); err != nil {
		t.Fatalf("CheckSize at MaxFileSize: %v, want nil", err)
	}
	err := CheckSize(path, MaxFileSize+1)
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("CheckSize over MaxFileSize: err=%v, want ErrFileTooLarge", err)
	}
	const want = "file too large: .dispatch/architecture/big.md is 1048577 bytes (limit 1048576)"
	if err.Error() != want {
		t.Errorf("CheckSize over MaxFileSize: message %q, want %q", err, want)
	}
}
