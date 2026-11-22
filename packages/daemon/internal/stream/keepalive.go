package stream

import (
	"net"
	"time"
)

// What every accepted TCP connection carries, so a connection whose other end vanished, with
// nothing left there to acknowledge a byte, ends within 3 minutes rather than holding its claim's
// stream (R3).
//
// The worker stream has no heartbeat frame of its own — an idle worker says nothing for hours —
// so a bound connection whose peer is gone is indistinguishable from a quiet one until the
// kernel gives up on it. These are Go's own keepalive defaults, 15 s idle and 9 probes 15 s
// apart, set here rather than inherited: a count left to the machine is whatever its
// `net.ipv4.tcp_keepalive_probes` says, and the daemon's pods and a developer's box are not the
// same machine.
//
// Keepalive alone is not enough. Linux sends no probe while data waits unacknowledged
// (`tcp_keepalive_timer`), and the stop frame R3 sends an older connection is such data, after
// which only the retransmission timeout, about 15 minutes, would end the connection.
// TCP_USER_TIMEOUT bounds exactly that wait, which is why grpc-go sets it beside its own
// keepalive (internal/transport/http2_client.go). With both, a vanished connection ends within 3
// minutes whether or not anything was written to it.
//
// Both are the kernel's, and end only a connection whose far end acknowledges nothing. A far end
// whose kernel still answers while nothing above it reads or writes, as when a pod's sandbox
// outlives its force delete, acknowledges every probe and every byte, the stop frame's included,
// and these never end it. For an older generation's connection the lock-out ends it instead,
// once it has sent nothing for LockOutSilenceAfter after its stop frame (lockout.go); these stay
// the backstop for every other connection, and for that one until then.
const (
	keepaliveIdle     = 15 * time.Second
	keepaliveInterval = 15 * time.Second
	keepaliveCount    = 9
	userTimeout       = 2 * time.Minute
)

// tune sets them on a connection the listener has just accepted. A unix socket under tmux gets
// none: it has no TCP timers, and both its ends are processes of one machine, whose death closes
// the socket.
func (l *Listener) tune(nc net.Conn) {
	tcp, ok := nc.(*net.TCPConn)
	if !ok {
		return
	}
	if err := tcp.SetKeepAliveConfig(net.KeepAliveConfig{
		Enable:   true,
		Idle:     keepaliveIdle,
		Interval: keepaliveInterval,
		Count:    keepaliveCount,
	}); err != nil {
		l.log.Warn("worker-stream: could not set keepalive on an accepted connection", "address", l.addr, "error", err)
	}
	if err := setUserTimeout(tcp, userTimeout); err != nil {
		l.log.Warn("worker-stream: could not set TCP_USER_TIMEOUT on an accepted connection", "address", l.addr, "error", err)
	}
}
