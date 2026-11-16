//go:build !linux

package memoryguard

import (
	"fmt"
	"runtime"
)

// watchModified answers no watch: the file-modified event the guest watch wakes on is Linux's
// inotify, and a cgroup's memory.events is Linux's alone, so the watch has its scan alone (Guest).
func watchModified(string) (*fileWatch, error) {
	return nil, fmt.Errorf("no file-modified event is read on %s", runtime.GOOS)
}
