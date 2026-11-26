package workspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A .jj that is a symbolic link is refused before the creation record in it is read or written
// through it. .jj is on the issue's volume, which every agent of the issue can write, and a link
// there carries the record into a directory off the workspace's disk: a record read there says
// nothing of this workspace, and one written there can be lost at a container restart while the
// workspace keeps its work. Created fails, and RecordCreated writes nothing through the link.
// Nothing here needs root, so nothing skips as root.
func TestALinkedJJIsRefusedBeforeTheCreationRecordIsReadOrWritten(t *testing.T) {
	dir, target := t.TempDir(), t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, ".jj")); err != nil {
		t.Fatal(err)
	}
	if err := RecordCreated(dir, time.Now()); !errors.Is(err, errLinkedJJ) {
		t.Errorf("RecordCreated over a linked .jj = %v, want errLinkedJJ", err)
	}
	if _, err := os.Lstat(filepath.Join(target, createdFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s was written through the link into %s (%v)", createdFile, target, err)
	}
	if err := os.WriteFile(filepath.Join(target, createdFile), []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if at, ok, err := Created(dir); !errors.Is(err, errLinkedJJ) {
		t.Errorf("Created through a linked .jj = %s, %t, %v; want errLinkedJJ", at, ok, err)
	}
}

// A .jj swapped for a link after it was opened changes nothing either: the record is read and
// written through the directory that open reached, never by its path again. Each row opens a
// workspace's real .jj, then, in the window between that open and the record's read (the held
// seam), moves the real directory aside and puts a link to another directory at .jj, as an agent
// of the issue's volume could. Read by its path after that check, the record would be the other
// directory's: a creation moment forged there. Read through the open directory it is the
// workspace's own.
func TestAJJSwappedForALinkAfterItsOpenReachesNoOtherDirectory(t *testing.T) {
	saved := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	record := func(t *testing.T, jj string, at time.Time) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(jj, createdFile), []byte(at.Format(time.RFC3339Nano)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// workspace is a workspace with a real .jj and another directory, elsewhere; swap moves the
	// real .jj aside and puts a link to elsewhere at .jj.
	workspace := func(t *testing.T) (dir, elsewhere string, swap func()) {
		t.Helper()
		dir, elsewhere = t.TempDir(), t.TempDir()
		jj := filepath.Join(dir, ".jj")
		own := filepath.Join(dir, ".jj-own")
		if err := os.Mkdir(jj, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir, elsewhere, func() {
			if err := os.Rename(jj, own); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, jj); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Run("Created reads the workspace's own moment", func(t *testing.T) {
		dir, elsewhere, swap := workspace(t)
		record(t, filepath.Join(dir, ".jj"), saved.Add(-time.Hour))
		record(t, elsewhere, saved.Add(time.Hour))
		at, ok, err := created(dir, swap)
		if err != nil || !ok || !at.Equal(saved.Add(-time.Hour)) {
			t.Errorf("Created of a workspace whose .jj was swapped for a link = %s, %t, %v; want its own %s", at, ok, err, saved.Add(-time.Hour))
		}
	})
	t.Run("Created names the directory it opened", func(t *testing.T) {
		dir, elsewhere, swap := workspace(t)
		path := filepath.Join(dir, ".jj", "legion-created")
		if err := os.Symlink(filepath.Join(elsewhere, "record"), path); err != nil {
			t.Fatal(err)
		}
		_, ok, err := created(dir, swap)
		var pathErr *fs.PathError
		if ok || !errors.As(err, &pathErr) || pathErr.Path != path {
			t.Errorf("Created after .jj was moved = %t, %v; want a refusal naming %s", ok, err, path)
		}
	})
}
