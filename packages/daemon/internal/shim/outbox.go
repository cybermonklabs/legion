package shim

import (
	"log"
	"net"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// outbox is every frame bound for the daemon: written to the connection when one is attached,
// held in the backlog of the newest backlogLimit lines when none is, and replayed in order —
// ahead of anything newer — when the next connection is acked (createShimBridge's forward and
// onConnect, worker-shim.ts). A frame is the lines that carry it: one line, or a whole rpc_chunk
// sequence, which is written, kept and dropped whole. The pump sends a harness sequence only once
// it is whole, so every frame goes in the order the shim sends it, and none lands inside another.
type outbox struct {
	mu      sync.Mutex
	conn    net.Conn
	w       *shimwire.Writer
	backlog [][][]byte
	dropped bool
	log     *log.Logger
	// kept is how many lines the backlog holds.
	kept int
}

// send writes frames in order, each the lines that carry it. A frame of no line is skipped.
func (o *outbox) send(frames ...[][]byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, frame := range frames {
		if len(frame) > 0 {
			o.write(frame)
		}
	}
}

// write writes one frame to the connection, or keeps it for the next. The caller holds o.mu.
func (o *outbox) write(frame [][]byte) {
	if o.w != nil {
		if writeLines(o.w, frame) == nil {
			return
		}
		// The stream broke under this frame, so the daemon never got it whole: it waits with the
		// rest for the next connection. Closing the connection is what tells its reader.
		_ = o.conn.Close()
		o.conn, o.w = nil, nil
	}
	o.backlog = append(o.backlog, frame)
	o.kept += len(frame)
	o.trim()
}

// trim drops the oldest whole frames while the backlog holds more than backlogLimit lines. The
// newest frame stays. The caller holds o.mu.
func (o *outbox) trim() {
	for o.kept > backlogLimit && len(o.backlog) > 1 {
		o.kept -= len(o.backlog[0])
		o.backlog[0] = nil
		o.backlog = o.backlog[1:]
		if !o.dropped {
			o.dropped = true
			o.log.Printf("[worker-shim] the oldest frames beyond the newest %d lines that wait for the daemon are dropped", backlogLimit)
		}
	}
}

func writeLines(w *shimwire.Writer, lines [][]byte) error {
	for _, line := range lines {
		if err := w.WriteLine(line); err != nil {
			return err
		}
	}
	return nil
}

// attach replays the backlog on a newly acked connection and makes it the one frames go to. A
// connection that breaks mid-replay is closed, and what it did not take waits for the next.
func (o *outbox) attach(conn net.Conn, w *shimwire.Writer) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for len(o.backlog) > 0 {
		if writeLines(w, o.backlog[0]) != nil {
			_ = conn.Close()
			return
		}
		o.kept -= len(o.backlog[0])
		o.backlog[0] = nil
		o.backlog = o.backlog[1:]
	}
	o.backlog = nil
	o.conn, o.w = conn, w
}

func (o *outbox) detach(conn net.Conn) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.conn == conn {
		o.conn, o.w = nil, nil
	}
}
