//go:build !linux

package sessionproxy

import (
	"errors"
	"net"
)

// readPeer reads no peer on a system other than Linux, which this package reads SO_PEERCRED on
// alone. So a proxy with an Admit refuses every connection here: the check fails closed.
func readPeer(net.Conn) (Peer, error) {
	return Peer{}, errors.New("this system has no SO_PEERCRED read in the session proxy")
}
