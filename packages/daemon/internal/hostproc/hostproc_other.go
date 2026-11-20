//go:build !linux

package hostproc

// isProc is false where there is no proc filesystem: such a machine has no /proc to list, and a
// scanner's read of one fails on its own.
func isProc(string) bool {
	return false
}
