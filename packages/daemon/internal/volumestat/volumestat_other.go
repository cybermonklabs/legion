//go:build !linux

package volumestat

import (
	"fmt"
	"runtime"
)

// Read refuses on every platform but Linux, naming the platform. A volume is read with statfs,
// which is Linux's call: the launcher's trimmer runs in the roles container of an issue pod and the
// repository cache in its own pod, and nowhere else, and this module is built for other platforms
// only so that a developer's machine can build and test the rest of it (the `legion` binary ships
// for darwin too, .github/go-release-targets.json). The twin exists because a Linux-only file would
// stop every package that reads a volume compiling on darwin, and it refuses by name rather than
// answering a reading never taken: a trimmer handed a zero reading would see a volume 0 per cent
// full and sweep nothing for ever, which is the same silence as a trimmer that never ran, and a
// cache would report a volume of no bytes.
func Read(path string) (Reading, error) {
	return Reading{}, fmt.Errorf("reading the volume at %s needs Linux, and this binary is built for %s", path, runtime.GOOS)
}
