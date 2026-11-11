//go:build !linux

package launcher

import (
	"fmt"
	"runtime"
)

// The pod's volume is read with statfs, which is Linux's call: the trimmer runs in the roles
// container of an issue pod and nowhere else, and this binary is built for other platforms only
// so that a developer's machine can build and test the rest of it (roleinit_other.go, where a
// role's namespaces refuse the same way). The twin exists because a Linux-only file would stop
// the package compiling on darwin, and it refuses by name rather than reporting a volume that
// was never read: a trimmer handed a zero reading would see a volume 0 per cent full and sweep
// nothing for ever, which is the same silence as a trimmer that never ran.
var readVolumeUsage = func(path string) (volumeUsage, error) {
	return volumeUsage{}, fmt.Errorf("launcher: reading the volume at %s needs Linux, and this binary is built for %s", path, runtime.GOOS)
}
