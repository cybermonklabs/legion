package docs

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"sync"

	"github.com/sjawhar/envoy/internal/dispatch/access"
)

// recheckedConn is a document socket's connection once ygo has taken it for the upgrade. Before
// each batch of the room's frames goes out it asks whether the socket's person still reads its room
// (stillReads, Service.socketStillReads), as the live agent view asks its rule before each batch
// of a session's frames: a session's documents follow the session content rule
// (access.DocumentClause through access.PartyClause), and a write that ties the session to another
// person, or records a project it wrote in or reached, announces nothing, so the access re-check
// (RunAccessRecheck) would find it only on its next heartbeat. Once the answer is no, the socket's
// admission is revoked (errAccessRevoked, on which ygo sends the revocation's close frame and
// closes the connection), and from then on every frame but a control frame (the close frame
// among them) is dropped, so no frame of the room reaches the peer.
//
// The upgrader writes its 101 answer in one write, which passes as it is. After it, every write is
// websocket framing as gorilla/websocket writes it: a frame's header in one write, and its payload
// in the same write or the ones after it (net.Buffers on a connection that cannot write vectors),
// so the connection reads each header it is handed at a frame boundary, asks before a data frame,
// and passes or drops the rest of that frame with its header. A control frame carries no document
// and is never asked about.
type recheckedConn struct {
	net.Conn
	stillReads func() bool

	mu        sync.Mutex
	upgraded  bool
	remaining uint64 // bytes of the current frame not yet written, header included
	dropping  bool   // the current frame is dropped
	revoked   bool
}

func (c *recheckedConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.upgraded {
		c.upgraded = true
		return c.Conn.Write(p)
	}
	if c.remaining == 0 {
		size, control := frameSize(p)
		c.remaining = size
		switch {
		case control:
			c.dropping = false
		case c.revoked:
			c.dropping = true
		case !c.stillReads():
			c.revoked, c.dropping = true, true
		default:
			c.dropping = false
		}
	}
	if uint64(len(p)) >= c.remaining {
		c.remaining = 0
	} else {
		c.remaining -= uint64(len(p))
	}
	if c.dropping {
		return len(p), nil
	}
	return c.Conn.Write(p)
}

// frameSize reads the websocket frame header that begins p, a frame as a server writes it (no
// mask): the frame's whole size, header included, and whether it is a control frame (close, ping
// or pong, opcode 8 or above). A header too short to read is taken as a frame of p alone.
func frameSize(p []byte) (size uint64, control bool) {
	if len(p) < 2 {
		return uint64(len(p)), false
	}
	control = p[0]&0x08 != 0
	header, length := uint64(2), uint64(p[1]&0x7f)
	switch length {
	case 126:
		if len(p) < 4 {
			return uint64(len(p)), control
		}
		header, length = 4, uint64(binary.BigEndian.Uint16(p[2:4]))
	case 127:
		if len(p) < 10 {
			return uint64(len(p)), control
		}
		header, length = 10, binary.BigEndian.Uint64(p[2:10])
	}
	if p[1]&0x80 != 0 {
		header += 4
	}
	return header + length, control
}

// SetRosterRefresh sets what refreshes the stored listener answer before a socket's ask
// (socketStillReads): the session content rule, which a session's documents follow, reads a
// listener list only from a stored answer that has not passed its expires_at. The API sets it
// once, as it mounts the sockets.
func (s *Service) SetRosterRefresh(refresh func(context.Context)) {
	s.rosterRefresh.Store(&refresh)
}

// socketStillReads reports whether admission's person may still read its room, asked before a
// batch of the room's frames goes out on the socket (recheckedConn), and revokes the socket when
// they may not or the read fails (errAccessRevoked), as recheckAdmission does. It reads nothing for
// a socket not yet admitted (its admission's own read decides), nor, for a person whose browser was
// open at admission, while no project is private: then every room they hold passes the rule
// before it reads a row (R9), and it reads only whether a project is private now
// (access.AnyProjectPrivate, one exists), as the live agent view's re-ask does. Before the rule's
// read it refreshes the stored listener answer (SetRosterRefresh), which the rule reads only while
// it is fresh.
func (s *Service) socketStillReads(ctx context.Context, admission *schemaAdmission, open bool) bool {
	if admission.key.room == "" || ctx.Err() != nil {
		return true
	}
	if open {
		private, err := access.AnyProjectPrivate(ctx, s.store.Pool)
		if err == nil && !private {
			return true
		}
	}
	if refresh := s.rosterRefresh.Load(); refresh != nil {
		(*refresh)(ctx)
	}
	readable, err := s.readableRooms(ctx, admission.person, []*schemaAdmission{admission})
	if err == nil && readable[admission.key.room] {
		return true
	}
	if err != nil && ctx.Err() == nil {
		slog.Warn("dispatch: read whether a document socket's person still reads its document before a batch of its frames; the socket closes and the provider reconnects", "room", admission.key.room, "error", err)
	}
	s.schemaAdmissions.revokeUnless(func(other *schemaAdmission) bool { return other != admission }, errAccessRevoked)
	return false
}
