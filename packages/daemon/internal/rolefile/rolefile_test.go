package rolefile

import (
	"bytes"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/legion/daemon/internal/testtmp"
)

// Read takes a regular file within its bound and refuses everything else a role can leave at a
// path, at once and in words naming the path: a symlink (to /dev/zero, to a regular file, or to
// nothing), a fifo, a socket, a directory, a device and a file past the bound. A missing file is
// open's own error, which fs.ErrNotExist matches. A read that followed /dev/zero would hold it
// whole in memory, and one that opened the fifo plainly would wait for a writer for ever.
func TestReadRefusesAnythingButARegularFileWithinItsBound(t *testing.T) {
	const limit = 16
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("leak-check-v15\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		path func(t *testing.T) string
		// want is the content Read answers, or "" with refusal set for a refusal.
		want    string
		refusal string
	}{
		{"a regular file", func(*testing.T) string { return regular }, "leak-check-v15\n", ""},
		{"an empty file", func(t *testing.T) string { return write(t, filepath.Join(dir, "empty"), nil) }, "", ""},
		{"a file at the bound", func(t *testing.T) string {
			return write(t, filepath.Join(dir, "at"), bytes.Repeat([]byte("x"), limit))
		}, string(bytes.Repeat([]byte("x"), limit)), ""},
		{"a file past the bound", func(t *testing.T) string {
			return write(t, filepath.Join(dir, "past"), bytes.Repeat([]byte("x"), limit+1))
		}, "", "is larger than 16 bytes"},
		{"a symlink to /dev/zero", func(t *testing.T) string { return link(t, "/dev/zero", filepath.Join(dir, "zero")) }, "", "is a symlink"},
		{"a symlink to a regular file", func(t *testing.T) string { return link(t, regular, filepath.Join(dir, "to-regular")) }, "", "is a symlink"},
		{"a dangling symlink", func(t *testing.T) string {
			return link(t, filepath.Join(dir, "nothing"), filepath.Join(dir, "dangling"))
		}, "", "is a symlink"},
		{"a fifo", func(t *testing.T) string {
			path := filepath.Join(dir, "fifo")
			if err := syscall.Mkfifo(path, 0o644); err != nil {
				t.Fatal(err)
			}
			return path
		}, "", "is a fifo"},
		{"a socket", func(t *testing.T) string {
			path := filepath.Join(testtmp.Dir(t, "socket"), "socket")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			return path
		}, "", "is a socket"},
		{"a directory", func(*testing.T) string { return dir }, "", "is a directory"},
		{"a device", func(*testing.T) string { return "/dev/null" }, "", "is a device"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path(t)
			type answer struct {
				body []byte
				err  error
			}
			done := make(chan answer, 1)
			go func() {
				body, err := Read(path, limit)
				done <- answer{body, err}
			}()
			var got answer
			select {
			case got = <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("Read(%s) was still reading after 5s", path)
			}
			switch {
			case tc.refusal == "" && got.err != nil:
				t.Fatalf("Read(%s) = %v, want %q", path, got.err, tc.want)
			case tc.refusal == "" && string(got.body) != tc.want:
				t.Fatalf("Read(%s) = %q, want %q", path, got.body, tc.want)
			case tc.refusal != "" && (got.err == nil || got.err.Error() != path+" "+tc.refusal):
				t.Fatalf("Read(%s) = %q, %v; want the refusal %q", path, got.body, got.err, path+" "+tc.refusal)
			}
		})
	}
	t.Run("a missing file", func(t *testing.T) {
		if _, err := Read(filepath.Join(dir, "missing"), limit); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Read of a missing file = %v, want an error fs.ErrNotExist matches", err)
		}
	})
}

// RemoveTree answers a path whose parent is a fifo rather than waiting on it: os.RemoveAll opens
// the parent of a path it cannot unlink outright with a plain open, which a fifo nobody writes
// holds for ever, and every caller of RemoveTree removes something under a directory a role can
// swap for a fifo.
func TestRemoveTreeAnswersAParentThatIsAFifo(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "worktrees")
	if err := syscall.Mkfifo(parent, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- RemoveTree(filepath.Join(parent, "infra-1")) }()
	select {
	case err := <-done:
		if !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf("RemoveTree under a fifo answered %v, want ENOTDIR", err)
		}
	case <-time.After(5 * time.Second):
		// Open the fifo for writing so the blocked open returns and the test's goroutine ends.
		if writer, openErr := os.OpenFile(parent, os.O_WRONLY, 0); openErr == nil {
			_ = writer.Close()
		}
		<-done
		t.Fatalf("RemoveTree was still opening %s after 5s: a parent that is a fifo blocks the removal", parent)
	}
	if info, err := os.Lstat(parent); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("the fifo %s is no longer there as it was (%v, %v)", parent, info, err)
	}
}

// RemoveTree removes a tree as os.RemoveAll does, and follows no link anywhere it walks: a link is
// removed, and what it names is left as it was. The case that tells the two apart is a link to a
// directory under a directory the removal cannot unlink it from (mode 0555): the walk opens that
// entry to remove what is under it, and os.RemoveAll opens it with O_DIRECTORY|O_NOFOLLOW, which
// a gVisor release can follow a link through (OpenDirectory), so os.RemoveAll deletes what the
// link names there. Linux, and newer gVisor, refuse that open, so only such a gVisor shows
// os.RemoveAll's answer apart from RemoveTree's; the row is the test that run reads.
func TestRemoveTreeFollowsNoLink(t *testing.T) {
	// victim is a directory holding one file, which every row's tree may name but must not touch.
	victim := func(t *testing.T) (dir, file string) {
		t.Helper()
		dir = filepath.Join(t.TempDir(), "victim")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir, write(t, filepath.Join(dir, "precious"), []byte("kept\n"))
	}
	survives := func(t *testing.T, file string) {
		t.Helper()
		if body, err := os.ReadFile(file); err != nil || string(body) != "kept\n" {
			t.Fatalf("%s was touched by the removal: %q, %v", file, body, err)
		}
	}
	t.Run("a link to a directory under a directory it cannot be unlinked from", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("as root a 0555 directory does not stop the unlink, so the walk never opens the link")
		}
		target, file := victim(t)
		tree := filepath.Join(t.TempDir(), "tree")
		locked := filepath.Join(tree, "locked")
		if err := os.MkdirAll(locked, 0o755); err != nil {
			t.Fatal(err)
		}
		link(t, target, filepath.Join(locked, "link"))
		if err := os.Chmod(locked, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		err := RemoveTree(tree)
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("RemoveTree(%s) = %v, want the link's unlink refused (permission denied)", tree, err)
		}
		survives(t, file)
	})
	t.Run("the path itself a link to a directory", func(t *testing.T) {
		target, file := victim(t)
		path := link(t, target, filepath.Join(t.TempDir(), "link"))
		if err := RemoveTree(path); err != nil {
			t.Fatalf("RemoveTree(%s) = %v, want the link removed", path, err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the link %s is still there (%v)", path, err)
		}
		survives(t, file)
	})
	t.Run("a tree with a link in it", func(t *testing.T) {
		target, file := victim(t)
		tree := filepath.Join(t.TempDir(), "tree")
		if err := os.MkdirAll(filepath.Join(tree, "a", "b"), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(tree, "a", "b", "file"), []byte("x"))
		link(t, target, filepath.Join(tree, "a", "link"))
		if err := RemoveTree(tree); err != nil {
			t.Fatalf("RemoveTree(%s) = %v, want the whole tree removed", tree, err)
		}
		if _, err := os.Lstat(tree); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is still there (%v)", tree, err)
		}
		survives(t, file)
	})
	t.Run("what is not there", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing")
		for _, path := range []string{missing, filepath.Join(missing, "under")} {
			if err := RemoveTree(path); err != nil {
				t.Errorf("RemoveTree(%s) = %v, want nil, as os.RemoveAll answers", path, err)
			}
		}
	})
}

// TestRemoveTreeFollowsNoLink's first row can fail only on a kernel that follows a link under
// O_DIRECTORY|O_NOFOLLOW, as some gVisor releases do and Linux never does, so on Linux a removal
// that asks for O_DIRECTORY passes it. Here every directory open of the walk goes through such a
// kernel (directoryOpen): an open that asks for both follows the link. RemoveTree asks for
// O_NOFOLLOW alone and leaves the link's target as it was; a walk that adds O_DIRECTORY deletes
// it here, on Linux. And the walk must open the directory under the link through that kernel at
// all: a RemoveTree that is os.RemoveAll again opens nothing through it, and fails here too.
func TestRemoveTreeFollowsNoLinkOnAKernelThatWould(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root a 0555 directory does not stop the unlink, so the walk never opens the link")
	}
	var opened []string
	directoryOpen = func(dirfd int, name string, flag int, perm uint32) (int, error) {
		opened = append(opened, name)
		if flag&unix.O_DIRECTORY != 0 && flag&unix.O_NOFOLLOW != 0 {
			flag &^= unix.O_NOFOLLOW
		}
		return Openat(dirfd, name, flag, perm)
	}
	t.Cleanup(func() { directoryOpen = Openat })

	target := filepath.Join(t.TempDir(), "victim")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	precious := write(t, filepath.Join(target, "precious"), []byte("kept\n"))
	tree := filepath.Join(t.TempDir(), "tree")
	locked := filepath.Join(tree, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	link(t, target, filepath.Join(locked, "link"))
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	err := RemoveTree(tree)
	if body, readErr := os.ReadFile(precious); readErr != nil || string(body) != "kept\n" {
		t.Fatalf("%s was touched by the removal on a kernel that follows the link (%q, %v); RemoveTree answered %v", precious, body, readErr, err)
	}
	if !slices.Contains(opened, "locked") {
		t.Fatalf("the walk opened %v through the kernel, not the directory holding the link: RemoveTree is not the walk this test measures", opened)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("RemoveTree(%s) = %v, want the link's unlink refused (permission denied)", tree, err)
	}
}

// A directory RemoveTree cannot open (mode 0000 or 0300: no read) is still removed when it is
// empty, as os.RemoveAll removes it: the walk cannot list it, but rmdir needs only the parent's
// write, so the removal of the directory itself is tried after the open fails. Otherwise one such
// directory left in a workspace's aside fails every later removal of that workspace.
func TestRemoveTreeRemovesAnEmptyDirectoryItCannotOpen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root every directory opens, so the walk never meets one it cannot")
	}
	for _, tc := range []struct {
		name string
		// paths answers, under root, the directory the subtest makes and strips of its modes, and
		// the path to remove.
		paths func(t *testing.T, root string) (stripped, path string)
		mode  os.FileMode
	}{
		{"an empty directory at 0000 inside the tree", func(t *testing.T, root string) (string, string) {
			return filepath.Join(root, "tree", "a"), filepath.Join(root, "tree")
		}, 0o000},
		{"an empty directory at 0300 inside the tree", func(t *testing.T, root string) (string, string) {
			return filepath.Join(root, "tree", "a"), filepath.Join(root, "tree")
		}, 0o300},
		{"the tree itself an empty directory at 0000", func(t *testing.T, root string) (string, string) {
			return filepath.Join(root, "tree"), filepath.Join(root, "tree")
		}, 0o000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			stripped, path := tc.paths(t, root)
			if err := os.MkdirAll(stripped, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(stripped, tc.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(stripped, 0o755) })
			if err := RemoveTree(path); err != nil {
				t.Fatalf("RemoveTree(%s) = %v, want nil, as os.RemoveAll answers", path, err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s is still there (%v)", path, err)
			}
		})
	}
}

// A file RemoveTree may neither unlink (its directory is 0555) nor read (mode 0000) is answered
// with the unlink's refusal, as os.RemoveAll answers it, and is left as it is: the walk's failed
// open of it is no directory it cannot open, so no removal of a directory is tried on it either.
func TestRemoveTreeAnswersTheUnlinkForAFileItCanNeitherUnlinkNorRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root a 0555 directory does not stop the unlink, so the walk never opens the file")
	}
	tree := filepath.Join(t.TempDir(), "tree")
	if err := os.Mkdir(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	file := write(t, filepath.Join(tree, "file"), []byte("kept\n"))
	if err := os.Chmod(file, 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tree, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tree, 0o755) })
	err := RemoveTree(tree)
	var refused *fs.PathError
	if !errors.As(err, &refused) || refused.Op != "unlinkat" || refused.Path != file || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("RemoveTree(%s) = %v, want the unlink of %s refused for permission", tree, err, file)
	}
	if _, err := os.Lstat(file); err != nil {
		t.Errorf("%s is gone (%v), want it left as it was", file, err)
	}
}

func write(t *testing.T, path string, body []byte) string {
	t.Helper()
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func link(t *testing.T, target, path string) string {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	return path
}
