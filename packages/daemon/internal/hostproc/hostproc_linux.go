package hostproc

import "golang.org/x/sys/unix"

// isProc says whether root is on a mount of proc, by the filesystem's own magic number: whatever
// the path's spelling, a link or a bind mount, a listing there is of a PID namespace's processes.
// A root that cannot be read is none, and the scanner's own read of it fails as it would anyway.
func isProc(root string) bool {
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil {
		return false
	}
	return fs.Type == unix.PROC_SUPER_MAGIC
}
