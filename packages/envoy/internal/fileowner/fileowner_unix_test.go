//go:build unix

package fileowner

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestOfIsTheUIDThatOwnsTheFile: a file this process made is owned by this process's uid, an info
// whose stat names another uid answers that uid, not this process's, and an info that carries no
// stat answers no owner.
func TestOfIsTheUIDThatOwnsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if uid, ok := Of(info); !ok || uid != os.Getuid() {
		t.Fatalf("Of(a file this process made) = %d, %t; want %d, true", uid, ok, os.Getuid())
	}
	other := os.Getuid() + 1
	if uid, ok := Of(withStat{info, &syscall.Stat_t{Uid: uint32(other)}}); !ok || uid != other {
		t.Fatalf("Of(a file uid %d owns) = %d, %t; want %d, true", other, uid, ok, other)
	}
	if uid, ok := Of(noStat{info}); ok {
		t.Fatalf("Of(an info with no stat) = %d, %t; want no owner", uid, ok)
	}
}

// noStat is a FileInfo whose Sys carries nothing.
type noStat struct{ os.FileInfo }

func (noStat) Sys() any { return nil }

// withStat is a FileInfo whose Sys is stat, as a file another user owns carries it.
type withStat struct {
	os.FileInfo
	stat *syscall.Stat_t
}

func (w withStat) Sys() any { return w.stat }
