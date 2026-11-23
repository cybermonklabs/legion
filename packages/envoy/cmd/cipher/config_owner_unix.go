//go:build unix

// packages/envoy/cmd/cipher/config_owner_unix.go
package main

import (
	"os"
	"syscall"
)

// fileOwner is the uid that owns the file info describes, and false where the file system says
// none. It is a variable so a test names an owner a test cannot make.
var fileOwner = func(info os.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}
