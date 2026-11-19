package workspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The in-use record is the launcher's fence against a bundle restored over a workspace a role
// has worked: MarkInUse writes it once, InUse reads it, and a record a role could plant in its
// place — a link — counts as in use without being followed. A .jj that cannot take the record
// is MarkInUse's error, which is what keeps a role from starting there.
func TestMarkInUseIsReadByInUseAndWritesOnce(t *testing.T) {
	fresh := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ".jj"), 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	dir := fresh(t)
	if inUse, err := InUse(dir); inUse || err != nil {
		t.Fatalf("InUse of a fresh .jj = %t, %v; want false and no error", inUse, err)
	}
	if err := MarkInUse(dir); err != nil {
		t.Fatalf("MarkInUse = %v", err)
	}
	if inUse, err := InUse(dir); !inUse || err != nil {
		t.Fatalf("InUse after MarkInUse = %t, %v; want true", inUse, err)
	}
	if err := MarkInUse(dir); err != nil {
		t.Fatalf("a second MarkInUse = %v, want nil: the record is already there", err)
	}

	linked := fresh(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(target, filepath.Join(linked, ".jj", inUseFile)); err != nil {
		t.Fatal(err)
	}
	if inUse, err := InUse(linked); !inUse || err != nil {
		t.Fatalf("InUse of a link at the record's path = %t, %v; want true", inUse, err)
	}
	if err := MarkInUse(linked); err != nil {
		t.Fatalf("MarkInUse over a link at the record's path = %v, want nil", err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the link's target %s was made (%v): the record's path was followed", target, err)
	}

	t.Run("a .jj that is no directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".jj"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := MarkInUse(dir); !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf("MarkInUse = %v, want ENOTDIR", err)
		}
		if inUse, err := InUse(dir); err == nil {
			t.Fatalf("InUse = %t and no error, want an error", inUse)
		}
	})

	t.Run("a .jj that refuses a create", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("as root a 0555 directory does not refuse the create")
		}
		dir := fresh(t)
		jj := filepath.Join(dir, ".jj")
		if err := os.Chmod(jj, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(jj, 0o755) })
		if err := MarkInUse(dir); !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("MarkInUse = %v, want a permission error", err)
		}
		if inUse, err := InUse(dir); inUse || err != nil {
			t.Fatalf("InUse = %t, %v; want false and no error: only MarkInUse's error keeps the role from starting", inUse, err)
		}
	})
}
