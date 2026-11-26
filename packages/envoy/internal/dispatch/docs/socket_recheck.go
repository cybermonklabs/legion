package docs

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pglisten"
)

// recheckedConn is a document socket's connection once ygo has taken it for the upgrade. Before
// each data frame of the room goes out - a document update or an awareness update - it asks
// whether the socket's person still reads its room (stillReads, Service.socketAsk), which reads
// the database only once the process has heard an access change since the socket's last read
// (accessEpoch): a session's documents follow the session content rule (access.DocumentClause
// through access.PartyClause), and a write that ties the session to another person, or records a
// project it wrote in or reached, can take them from a person without any change of a project's
// access, so the access re-check (RunAccessRecheck) alone would find it only on its next
// heartbeat. Once the answer is no, the socket's admission is revoked (errAccessRevoked, on which
// ygo sends the revocation's close frame and closes the connection), and from then on every frame
// but a control frame (the close frame among them) is dropped, so no frame of the room reaches the
// peer.
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

// accessEpoch counts the changes this process has heard that can move what a document socket's
// person reads, so that a socket asks its rule again only once one has (socketAsk) rather than
// before every frame. It moves:
//
//   - on each notification on store.AccessChangedChannel (a project's owner, visibility or access
//     list, a group) and on events.SessionAccessChannel (a session's tie or project, a project
//     it reached, a stored listener answer), which each such write sends inside its own
//     transaction;
//   - on each LISTEN that stands and each connection that is lost, since a notification sent
//     while none stood reached nobody;
//   - on each beat of the sockets' heartbeat (RunAccessRecheck), which bounds how long a change no
//     notification carries - one made by SQL alone, or one sent while the listener's connection
//     was half-open and not yet found dead - goes unread.
//
// It keeps no answer while no LISTEN on those channels stands (stamp): from the process's start to
// its hub's first LISTEN, from a lost connection to the next LISTEN, and in a process that runs no
// room sync, every data frame asks, as it would with no epoch at all.
//
// What it leaves open is a notification's delivery: Postgres sends it as the write commits, and
// the hub reads it some time later, a network trip on a healthy connection. A frame the room writes
// to a socket between the commit and the notification's arrival goes out under the answer the
// socket read before the change; the first data frame after the arrival asks again.
type accessEpoch struct {
	moves     atomic.Uint64
	listening atomic.Bool
}

// move records that something may have changed what a socket's person reads.
func (e *accessEpoch) move() { e.moves.Add(1) }

// listen registers the epoch on hub, the process's one shared listener, before hub runs: it moves
// on each notification of the two access channels and on each LISTEN that stands or is lost, and
// keeps answers only while a LISTEN stands. Each callback runs on the hub's goroutine and only
// stores an atomic, so it never blocks the hub.
func (e *accessEpoch) listen(hub *pglisten.Hub) {
	moved := func(string) { e.move() }
	hub.Handle(store.AccessChangedChannel, moved)
	hub.Handle(events.SessionAccessChannel, moved)
	hub.OnConnected(func() {
		e.move()
		e.listening.Store(true)
	})
	hub.OnDisconnected(func() {
		e.listening.Store(false)
		e.move()
	})
}

// stamp answers the epoch an answer read now holds for, and whether it holds past this read at all:
// not while no LISTEN on the access channels stands, when a change would reach no listener here.
// A caller reads the stamp before its read, so a change heard during the read moves the epoch past
// the answer.
func (e *accessEpoch) stamp() (moves uint64, kept bool) {
	if !e.listening.Load() {
		return 0, false
	}
	return e.moves.Load(), true
}

// SetRosterRefresh sets what refreshes the stored listener answer before a read of which rooms a
// person still reads (readableRooms, through refreshRosterForReader): the session content rule,
// which a session's documents follow, reads a listener list only from a stored answer that has
// not passed its expires_at. The API sets it once, as it mounts the sockets, from S3c-issues on,
// the slice that brings the refresh it sets; until then nothing sets it, and the rule reads the
// stored answer as it stands.
func (s *Service) SetRosterRefresh(refresh func(context.Context)) {
	s.rosterRefresh.Store(&refresh)
}

// socketAsk is what a document socket's connection asks before each data frame it writes
// (recheckedConn): whether admission's person, resolved at admission as viewer, still reads its
// room. It reads the database (socketStillReads) only when it has no answer that holds - before
// the socket's first data frame after its admission, once the process's access epoch has moved
// since its last read, and before every frame while the epoch keeps no answer (accessEpoch.stamp)
// - so a frame stream with no access change reads nothing, and a change any process announces is
// read before the first data frame after its notification arrives here. A socket not yet admitted
// is passed without a read, and without an answer kept, since its admission's own read decides
// it. Once the socket's context has ended - its admission revoked, or its request gone - every
// data frame is dropped: a revoked socket sends nothing more of the room while ygo closes it. The
// connection calls it under its own lock, one frame at a time, so what it keeps needs no lock of
// its own.
func (s *Service) socketAsk(ctx context.Context, admission *schemaAdmission, viewer model.Viewer) func() bool {
	ask := socketAnswer{open: viewer.Open, public: !viewer.AnyPrivate}
	return func() bool {
		if ctx.Err() != nil {
			return false
		}
		if admission.key.room == "" {
			return true
		}
		stamp, holds := s.accessEpoch.stamp()
		if ask.kept && holds && stamp == ask.asked {
			return true
		}
		ask.kept = false
		if !s.socketStillReads(ctx, admission, &ask) {
			return false
		}
		ask.kept, ask.asked = holds, stamp
		return true
	}
}

// socketAnswer is what one socket's ask keeps (socketAsk): whether its person's browser was open
// at admission, whether the last read it saw found no project private (public, from the admission's
// own read until the socket reads again), and the access epoch its last yes holds for.
type socketAnswer struct {
	open, public bool
	kept         bool
	asked        uint64
}

// socketStillReads reports whether admission's person may still read its room, read when the
// socket's ask holds no answer (socketAsk), and revokes the socket when they may not or the rule's
// read fails (errAccessRevoked), as recheckAdmission does. For a person whose browser was open at
// admission it reads first whether a project is private (anyProjectPrivate, once per move of the
// access epoch for every socket of the process), and while none is passes without the rule: then
// every room they hold passes the rule before it reads a row (R9). That read failing passes such
// a person too, as long as the last read the socket saw found no project private: a deployment
// that holds none closes no socket because the database did not answer once, and the next move of
// the epoch reads again (the sockets' heartbeat moves it at least every beat). A switch to private
// is an access change, on which the access lane's pass (recheckAccess) reads every socket's person
// again and closes the sockets of a person it cannot read. Once a project is private, the rule
// decides: its read (readableRooms) refreshes the stored listener answer first, once
// (refreshRosterForReader), since the rule reads a listener list only while that answer is fresh,
// and a read of the rule that fails closes the socket with the revocation's code, on which the
// provider reconnects and the reconnect's admission reads again.
func (s *Service) socketStillReads(ctx context.Context, admission *schemaAdmission, ask *socketAnswer) bool {
	if ask.open {
		if private, err := s.anyProjectPrivate(ctx); err == nil {
			ask.public = !private
		}
		if ask.public {
			return true
		}
	}
	readable, err := s.readableRooms(ctx, admission.person, []*schemaAdmission{admission})
	if err == nil && readable[admission.key.room] {
		return true
	}
	if err != nil && ctx.Err() == nil {
		slog.Warn("dispatch: read whether a document socket's person still reads its document before its next frame; the socket closes and the provider reconnects", "room", admission.key.room, "error", err)
	}
	s.schemaAdmissions.revokeUnless(func(other *schemaAdmission) bool { return other != admission }, errAccessRevoked)
	return false
}

// privateRead is the process's last read of whether a project is private, the access epoch it
// holds for and the error it ended in (anyProjectPrivate), and whether the reads are failing now.
type privateRead struct {
	mu      sync.Mutex
	known   bool
	stamp   uint64
	private bool
	err     error
	failing atomic.Bool
}

// anyProjectPrivate answers whether a project is private (access.AnyProjectPrivate), read once per
// move of the access epoch: a switch of a project's visibility announces itself on
// store.AccessChangedChannel, which moves it. The sockets that ask at one epoch wait for its one
// read, and share its answer or its failure, so a database that does not answer is asked once per
// move of the epoch, not once per frame. While the epoch keeps no answer, every call reads. The
// first failure after an answer is logged, and none after it until a read answers again.
func (s *Service) anyProjectPrivate(ctx context.Context) (bool, error) {
	stamp, holds := s.accessEpoch.stamp()
	read := &s.projectsPrivate
	if !holds {
		return read.read(ctx, s.store.Pool)
	}
	read.mu.Lock()
	defer read.mu.Unlock()
	if read.known && read.stamp == stamp {
		return read.private, read.err
	}
	private, err := read.read(ctx, s.store.Pool)
	if ctx.Err() == nil {
		read.known, read.stamp, read.private, read.err = true, stamp, private, err
	}
	return private, err
}

// read reads whether a project is private through q, and logs a read that failed after one that
// answered.
func (r *privateRead) read(ctx context.Context, q access.Querier) (bool, error) {
	private, err := access.AnyProjectPrivate(ctx, q)
	if err == nil {
		r.failing.Store(false)
		return private, nil
	}
	if ctx.Err() == nil && !r.failing.Swap(true) {
		slog.Warn("dispatch: read whether a project is private before a document socket's next frame; an open person's socket keeps the last answer it read until a read answers", "error", err)
	}
	return false, err
}
