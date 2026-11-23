//go:build linux

package volumestat

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Read is one statfs of path, as the size, used and available bytes of the volume path is on
// (readingOfStatfs). Its error names the path and statfs's own error, with no package prefix: each
// caller says what it was reading for.
func Read(path string) (Reading, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return Reading{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	return readingOfStatfs(&fs), nil
}

// readingOfStatfs is one statfs reading as df reads it: the size is the total blocks, used is the
// total blocks less the free ones, and available is f_bavail, what an unprivileged writer may still
// use. Every block count is in units of f_frsize, the fragment size, and of f_bsize only where
// f_frsize is 0, which is the rule df's own reading follows (gnulib's get_fs_usage, lib/fsusage.c:
// "f_frsize isn't guaranteed to be supported"). f_bsize is the size a filesystem prefers its writes
// in, which need not be the unit it counts its blocks in; on an issue pod's volume the two are both
// 4096. `stat -f` prints the unit counted in as %S, the "fundamental block size", and f_bsize as
// %s.
func readingOfStatfs(fs *unix.Statfs_t) Reading {
	block := uint64(fs.Frsize)
	if block == 0 {
		block = uint64(fs.Bsize)
	}
	return Reading{
		SizeBytes:      fs.Blocks * block,
		UsedBytes:      (fs.Blocks - fs.Bfree) * block,
		AvailableBytes: fs.Bavail * block,
	}
}
