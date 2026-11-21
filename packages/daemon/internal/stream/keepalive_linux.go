package stream

import (
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// setUserTimeout bounds how long the kernel retransmits unacknowledged data on c before it ends
// the connection, whatever the keepalive says: Linux's TCP_USER_TIMEOUT, in milliseconds.
func setUserTimeout(c *net.TCPConn, d time.Duration) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var set error
	if err := raw.Control(func(fd uintptr) {
		set = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(d.Milliseconds()))
	}); err != nil {
		return err
	}
	return set
}
