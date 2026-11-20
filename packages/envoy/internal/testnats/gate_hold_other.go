//go:build !linux && !darwin

package testnats

import (
	"fmt"
	"net"
	"runtime"
)

// errNoGateHold is why a gate cannot start here: it holds its port with a socket that shares the
// port with the gate's listener through SO_REUSEPORT (Gate), which this package does on Linux and
// Darwin only. A gate that gave its port up while closed would let another program take it.
var errNoGateHold = fmt.Errorf("a gate holds its port with SO_REUSEPORT beside its listener, which this package does on Linux and Darwin, not on %s", runtime.GOOS)

func holdPort() (string, func(), error) { return "", nil, errNoGateHold }

func listenBesideHold(string) (net.Listener, error) { return nil, errNoGateHold }
