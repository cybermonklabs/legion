//go:build !linux

package launcher

import (
	"fmt"
	"os"
	"runtime"
)

// The trimmer's opens are Linux's, as its reading of the volume is (volumeusage_other.go): the
// flags that make an open refuse a link and never wait on a FIFO are openat's, and the
// descriptor a handle is made over is read back through /proc. The binary is built for other
// platforms only so that a developer's machine can build and test the rest of it, and every
// open refuses by name rather than answering a handle that holds none of those properties.
func openVolumeDir(path string) (*dirHandle, error) {
	return nil, volumeOpensNeedLinux(path)
}

func (d *dirHandle) open(name string) (*dirHandle, error) {
	return nil, volumeOpensNeedLinux(name)
}

func rootOverDescriptor(*os.File) (*os.Root, error) {
	return nil, volumeOpensNeedLinux("a descriptor")
}

func volumeOpensNeedLinux(what string) error {
	return fmt.Errorf("launcher: opening %s as the cache trimmer does needs Linux, and this binary is built for %s", what, runtime.GOOS)
}
