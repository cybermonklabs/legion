//go:build linux

package sessionproxy

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// readPeer reads the process that made conn, a connection the proxy's unix listener accepted, as
// the kernel recorded it at connect (SO_PEERCRED). The read goes through the connection's own
// descriptor and does not block.
func readPeer(conn net.Conn) (Peer, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return Peer{}, fmt.Errorf("a %T is no unix socket connection", conn)
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return Peer{}, fmt.Errorf("reach the connection's socket: %w", err)
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return Peer{}, fmt.Errorf("reach the connection's socket: %w", err)
	}
	if credErr != nil {
		return Peer{}, fmt.Errorf("read SO_PEERCRED: %w", credErr)
	}
	return Peer{PID: int(cred.Pid), UID: int(cred.Uid)}, nil
}
