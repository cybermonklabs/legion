// packages/envoy/internal/fileowner/fileowner_unix.go
//go:build unix

// Package fileowner reads which user owns a file, for the checks that refuse a private file or
// directory another user holds: the helper's identity key and machine file, and agent-secrets'
// airlock directory. A target that keeps no uid on a file, such as Windows, answers none, so
// those packages build there with the check doing nothing.
package fileowner

import (
	"os"
	"syscall"
)

// Of answers the uid that owns the file info describes, and false where info carries none.
func Of(info os.FileInfo) (uid int, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
