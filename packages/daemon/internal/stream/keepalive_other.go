//go:build !linux

package stream

import (
	"net"
	"time"
)

// setUserTimeout is Linux's TCP_USER_TIMEOUT, which no other kernel has. The daemon runs on
// Linux; a developer's macOS build gets the keepalive alone, and a connection whose peer vanished
// there ends at the retransmission timeout instead.
func setUserTimeout(*net.TCPConn, time.Duration) error { return nil }
