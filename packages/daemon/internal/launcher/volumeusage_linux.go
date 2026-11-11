//go:build linux

package launcher

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// readVolumeUsage is one statfs of path, the pod's volume (VolumeRoot), as the used and available
// bytes df takes its percentage from: used is the total blocks less the free ones, and available
// is f_bavail, what an unprivileged writer may still use. Every block count is in units of
// f_bsize, which statfs reports and which this pod's volume gives as 4096.
//
// It is a package-level variable rather than a function because it is the trimmer's one reading
// of the machine: every test of a sweep injects a reading through it and counts the calls, which
// is this package's seam for a thing a test must fake (roleInitLaunch, testPGInterval). No other
// code under packages/ calls Statfs, so there is no helper to reuse here.
var readVolumeUsage = func(path string) (volumeUsage, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return volumeUsage{}, fmt.Errorf("launcher: statfs %s: %w", path, err)
	}
	block := uint64(fs.Bsize)
	return volumeUsage{
		UsedBytes:      (fs.Blocks - fs.Bfree) * block,
		AvailableBytes: fs.Bavail * block,
	}, nil
}
