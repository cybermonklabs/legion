//go:build linux

package launcher

// boundHomeMounts is mountsUnder over dir, the reading privateHome binds back by.
func boundHomeMounts(dir string) (string, []string, error) { return mountsUnder(dir) }
