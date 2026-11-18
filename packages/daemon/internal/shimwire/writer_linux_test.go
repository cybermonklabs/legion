package shimwire

import (
	"bytes"
	"fmt"
	"net"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/legion/daemon/internal/testtmp"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The bug socket-writer.ts exists for (socket-writer.ts:19-25): an ~350 KiB rpc_chunk line
// against a unix socket buffer of roughly 200 KiB. A writer that let the transport take only
// what fits would deliver a truncated, unparseable frame. Here the reader deliberately waits
// until the buffer is full before it starts draining: until what the writer's end has sent and
// the reader's end has not read (SIOCOUTQ) reaches the writer's send buffer (SO_SNDBUF), which is
// where a unix socket stops taking more, so the writer is waiting on the reader with most of the
// line still to send.
func TestALineLargerThanTheSocketBufferIsDeliveredWhole(t *testing.T) {
	line := chunkLine(t)
	listener, err := net.Listen("unix", testtmp.Socket(t, "w.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	conn, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	sender, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer sender.Close()
	written := make(chan error, 1)
	go func() { written <- NewWriter(sender).WriteLine(line) }()

	testwait.Until(t, "the writer to fill the socket's buffer before anything is read from it", func() (string, bool) {
		queued, limit := sendQueue(t, sender)
		return fmt.Sprintf("%d bytes sent and not read, of a %d-byte send buffer", queued, limit), queued >= limit
	})
	got, err := NewReader(conn).ReadLine()
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if writeErr := testwait.Receive(t, "WriteLine to return once the line was read", written); writeErr != nil {
		t.Fatalf("WriteLine: %v", writeErr)
	}
	if !bytes.Equal(got, line) {
		t.Fatalf("read %d bytes, want the whole %d-byte line", len(got), len(line))
	}
}

// sendQueue is what conn has sent and its peer has not read yet (SIOCOUTQ), and conn's send
// buffer (SO_SNDBUF).
func sendQueue(t *testing.T, conn net.Conn) (queued, limit int) {
	t.Helper()
	raw, err := conn.(*net.UnixConn).SyscallConn()
	if err != nil {
		t.Fatalf("reach the socket: %v", err)
	}
	var queueErr, limitErr error
	if err := raw.Control(func(fd uintptr) {
		queued, queueErr = unix.IoctlGetInt(int(fd), unix.SIOCOUTQ)
		limit, limitErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF)
	}); err != nil {
		t.Fatalf("reach the socket: %v", err)
	}
	if queueErr != nil || limitErr != nil {
		t.Fatalf("read the socket's send queue (%v) and send buffer (%v)", queueErr, limitErr)
	}
	return queued, limit
}
