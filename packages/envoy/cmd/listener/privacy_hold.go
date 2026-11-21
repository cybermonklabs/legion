package main

// privacy_hold.go is the relay's privacy rule in the required mode (project_privacy.go): each
// frame whose subject names a project (contracts.SubjectProject) is relayed to a session only
// when the session's stored scope (session.SessionEntry.RequiredScope, K6) sees the project
// (roles.Sees), decided at the frame's delivery:
//
//  1. a session whose token lists the project is relayed its frames in every state, with nothing
//     read, and a restricted member's session is withheld every other project's;
//  2. a full member's session is relayed a public project's frame and withheld a private one's, by
//     a fresh privacy read; while the read cannot answer, the project is unknown and counts as
//     private: the frame is held for that session, in the listener's memory, for at most
//     holdLimits.undecided, and relayed or withheld once a read decides its project;
//  3. a frame of no project, a GitHub event included (contracts.AnyToken), goes at once to every
//     session its subject reaches and is never held.
//
// A withheld frame is acknowledged and not relayed, counted (envoy_listener_project_withheld_total)
// and logged with its subject. A held frame is acknowledged upstream at once, so the session
// consumer's MaxAckPending never fills with held frames and a held project never stalls another
// project's frames to the same session, and is kept per session and per held project (a lane) in
// stream order; a later frame of a lane that still holds frames joins it, so a lane keeps its
// order. A lane holds at most holdLimits.laneFrames frames or laneBytes: a frame that would
// overfill it drops every frame held there, and the lane starts again with that frame. A frame
// larger than laneBytes is dropped at once. A session's lanes together hold at most sessionBytes,
// and the listener's at most totalBytes: a frame that would overfill either is itself dropped, so
// no session's frames evict another's. A frame undecided at holdLimits.undecided is dropped, and
// every held lane is read again at least every holdLimits.redecide. Each drop is counted
// (envoy_listener_held_dropped_total) and marks the session's next relayed frame Envoy-Gap: true
// with Envoy-Gap-Reason: privacy_hold.
//
// A listener that restarts loses its buffers, so the session's record carries the held marker
// (session.SessionEntry.Held) while any frame is held for it, set and cleared at the revision it
// read (session.SessionRegistry.SetHeld): a listener that starts relaying a session whose record
// holds it marks the first frame it relays with the gap and clears the mark. A marker write that
// does not land marks the session's next frame with the gap too.

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// privacyHoldGap is relayHeaderGapReason's value for frames the privacy hold dropped.
const privacyHoldGap = "privacy_hold"

// heldMarkerTimeout bounds one write of a session's held marker.
const heldMarkerTimeout = 2 * time.Second

// privacyHoldLimits are the hold's bounds.
type privacyHoldLimits struct {
	// undecided is how long a frame is held while its project's privacy is unknown before it is
	// dropped.
	undecided time.Duration
	// redecide is how often every held lane's project is read again while frames are held.
	redecide time.Duration
	// laneFrames and laneBytes bound one session's frames of one held project; sessionBytes one
	// session's held frames together; totalBytes the listener's.
	laneFrames   int
	laneBytes    int64
	sessionBytes int64
	totalBytes   int64
}

// holdLimits are the hold's bounds; a test lowers them and restores them (t.Cleanup).
var holdLimits = privacyHoldLimits{
	undecided:    120 * time.Second,
	redecide:     30 * time.Second,
	laneFrames:   256,
	laneBytes:    4 << 20,
	sessionBytes: 16 << 20,
	totalBytes:   256 << 20,
}

// heldFrame is one frame held for a session: the message as the consumer delivered it, already
// acknowledged upstream, and when it was held.
type heldFrame struct {
	msg    bus.SessionMessage
	heldAt time.Time
}

// heldFrameSize is what a held message counts against the hold's byte bounds.
func heldFrameSize(msg bus.SessionMessage) int64 { return int64(len(msg.Data) + len(msg.Subject)) }

// heldLane is one session's held frames of one project, in stream order.
type heldLane struct {
	frames []heldFrame
	bytes  int64
}

// sessionHold is one session's held lanes, whether its next relayed frame carries the hold's gap,
// whether this listener set its record's held marker, and when its lanes are next read again.
//
// A hold runs from the first frame of the session decided while its project's privacy was unknown
// until a read answers (decideHeld): open while it runs. It marks one relayed frame with its gap
// however many frames it drops (told), so a long hold tells the session once; a hold that ends and
// begins anew marks one frame again.
type sessionHold struct {
	lanes    map[string]*heldLane
	bytes    int64
	gap      bool
	marked   bool
	nextRead time.Time
	open     bool
	told     bool
}

// begin opens the hold when none is open, its gap not yet told.
func (hold *sessionHold) begin() {
	if !hold.open {
		hold.open, hold.told = true, false
	}
}

// dropped marks the session's next relayed frame with the hold's gap, unless a frame of the open
// hold already carried it.
func (hold *sessionHold) dropped() {
	if !hold.open || !hold.told {
		hold.gap = true
	}
}

// takeGap reports whether the next relayed frame carries the hold's gap, and clears it; a gap told
// while the hold is open is the hold's one.
func (hold *sessionHold) takeGap() bool {
	if !hold.gap {
		return false
	}
	hold.gap = false
	if hold.open {
		hold.told = true
	}
	return true
}

// oldest is when hold's oldest held frame was held, and whether it holds one.
func (hold *sessionHold) oldest() (time.Time, bool) {
	var oldest time.Time
	for _, lane := range hold.lanes {
		if len(lane.frames) > 0 && (oldest.IsZero() || lane.frames[0].heldAt.Before(oldest)) {
			oldest = lane.frames[0].heldAt
		}
	}
	return oldest, !oldest.IsZero()
}

// privacyHolds is the listener's held frames, by session.
type privacyHolds struct {
	mu       sync.Mutex
	sessions map[string]*sessionHold
	total    int64
}

// heldAck answers a frame relayed from the hold: the message was acknowledged upstream when it
// was held, so a reply to its frame has nothing more to answer.
type heldAck struct{}

func (heldAck) Ack() error                       { return nil }
func (heldAck) InProgress() error                { return nil }
func (heldAck) NakWithDelay(time.Duration) error { return nil }

// setPrivacy sets the relay's privacy mode, before the first relay starts.
func (m *sessionRelays) setPrivacy(privacy *listenerPrivacy) {
	m.privacy = privacy
	if privacy.isRequired() {
		m.holds = &privacyHolds{sessions: map[string]*sessionHold{}}
	}
}

// relayGap is the gap mark a frame is relayed with: none, the stream's own (Envoy-Gap alone), or
// the hold's (Envoy-Gap with Envoy-Gap-Reason: privacy_hold).
type relayGap struct {
	set    bool
	reason string
}

// laneHolds reports whether sessionID's lane of project holds frames.
func (m *sessionRelays) laneHolds(sessionID, project string) bool {
	m.holds.mu.Lock()
	defer m.holds.mu.Unlock()
	hold := m.holds.sessions[sessionID]
	if hold == nil {
		return false
	}
	lane := hold.lanes[project]
	return lane != nil && len(lane.frames) > 0
}

// decideFrame is the privacy rule for msg, which sessionID's consumer delivered: the messages to
// relay now, in stream order. A message the hold takes, and one a read withholds, is in none.
func (m *sessionRelays) decideFrame(ctx context.Context, sessionID string, msg bus.SessionMessage) []bus.SessionMessage {
	project, ok := contracts.SubjectProject(msg.Subject)
	if !ok || project == contracts.NoProject || project == contracts.AnyToken {
		return []bus.SessionMessage{msg}
	}
	entry, _ := m.sessions.Lookup(sessionID)
	listed, restricted := entry.RequiredScope(sessionID)
	if slices.Contains(listed, project) {
		return []bus.SessionMessage{msg}
	}
	if restricted {
		m.withhold(sessionID, msg, true)
		return nil
	}
	// A frame of a project whose lane already holds frames for the session joins that lane with
	// no read of its own: the lane is decided as a whole by the next read (redecideHeld, at most
	// holdLimits.redecide away), which is sent after this frame arrived, and keeps its order. So an
	// outage costs one read per session and lane per redecide interval, not one per frame.
	if m.laneHolds(sessionID, project) {
		m.hold(ctx, sessionID, project, msg)
		return nil
	}
	counters := m.privacy.counted()
	set, err := m.readBeforeDrop(ctx, sessionID)
	if err != nil {
		counters.decisions.Inc([2]string{"answer", decisionUnknown})
		counters.unknown.Inc([2]string{"reason", unknownReasonOf(err)})
		m.hold(ctx, sessionID, project, msg)
		return nil
	}
	// The read was sent after every frame held for the session arrived, so it decides every one
	// of its lanes, and a lane relays what it held before this frame of it.
	relayed := m.decideHeld(ctx, sessionID, set)
	if set.private(project) {
		counters.decisions.Inc([2]string{"answer", decisionPrivate})
		m.withhold(sessionID, msg, true)
		return relayed
	}
	counters.decisions.Inc([2]string{"answer", decisionPublic})
	return append(relayed, msg)
}

// withhold counts msg withheld and logs its subject, after acknowledging it when ack (a message
// the hold did not already acknowledge).
func (m *sessionRelays) withhold(sessionID string, msg bus.SessionMessage, ack bool) {
	if ack {
		if err := msg.Ack(); err != nil {
			m.logger.Warn("session relay could not acknowledge a frame it withholds; the consumer delivers it again",
				slog.String("session_id", sessionID), slog.String("subject", msg.Subject), slog.String("error", err.Error()))
			return
		}
	}
	m.privacy.counted().withheld.Inc([2]string{"family", subjectFamily(msg.Subject)})
	m.logger.Info("session relay withheld a frame from a session whose token does not see its project",
		slog.String("session_id", sessionID), slog.String("subject", msg.Subject))
}

// hold acknowledges msg upstream and keeps it for sessionID in project's lane, within the hold's
// bounds (the file comment says which frame each bound drops). A message whose acknowledgement
// fails is not kept: the consumer delivers it again, and it is decided then.
func (m *sessionRelays) hold(ctx context.Context, sessionID, project string, msg bus.SessionMessage) {
	if err := msg.Ack(); err != nil {
		m.logger.Warn("session relay could not acknowledge a frame it would hold; the consumer delivers it again",
			slog.String("session_id", sessionID), slog.String("subject", msg.Subject), slog.String("error", err.Error()))
		return
	}
	frame := heldFrame{msg: msg, heldAt: time.Now()}
	frame.msg.SessionAcker = heldAck{}
	size := heldFrameSize(msg)
	limits := holdLimits
	h := m.holds
	h.mu.Lock()
	hold := h.sessionLocked(sessionID)
	hold.begin()
	if len(hold.lanes) == 0 {
		hold.nextRead = frame.heldAt.Add(limits.redecide)
	}
	lane := hold.lanes[project]
	switch {
	case size > limits.laneBytes:
		m.dropLocked(hold, heldDroppedTooLarge, 1)
	default:
		if lane != nil && (len(lane.frames)+1 > limits.laneFrames || lane.bytes+size > limits.laneBytes) {
			m.dropLaneLocked(hold, project, heldDroppedLane)
			lane = nil
		}
		switch {
		case h.total+size > limits.totalBytes:
			m.dropLocked(hold, heldDroppedTotal, 1)
		case hold.bytes+size > limits.sessionBytes:
			m.dropLocked(hold, heldDroppedSession, 1)
		default:
			if lane == nil {
				lane = &heldLane{}
				hold.lanes[project] = lane
			}
			lane.frames = append(lane.frames, frame)
			lane.bytes += size
			hold.bytes += size
			h.total += size
		}
	}
	mark := len(hold.lanes) > 0 && !hold.marked
	if mark {
		hold.marked = true
	}
	h.mu.Unlock()
	if mark {
		m.writeHeldMarker(ctx, sessionID, true)
	}
}

// dropLocked counts n of hold's frames dropped for reason and marks its next relayed frame with
// the gap. The caller holds m.holds.mu.
func (m *sessionRelays) dropLocked(hold *sessionHold, reason string, n int) {
	hold.dropped()
	m.privacy.counted().heldDropped.Add(int64(n), [2]string{"reason", reason})
}

// dropLaneLocked drops every frame of hold's lane of project for reason. The caller holds
// m.holds.mu.
func (m *sessionRelays) dropLaneLocked(hold *sessionHold, project, reason string) {
	lane := hold.lanes[project]
	if lane == nil {
		return
	}
	delete(hold.lanes, project)
	hold.bytes -= lane.bytes
	m.holds.total -= lane.bytes
	m.dropLocked(hold, reason, len(lane.frames))
}

// decideHeld decides every lane held for sessionID by set, a whole answer: the frames of a public
// project's lane are returned to relay, in stream order, and a private one's withheld. A session
// left holding nothing has its held marker cleared.
func (m *sessionRelays) decideHeld(ctx context.Context, sessionID string, set privateSet) []bus.SessionMessage {
	if m.holds == nil {
		return nil
	}
	h := m.holds
	h.mu.Lock()
	hold := h.sessions[sessionID]
	if hold != nil {
		// A read answered: the hold, if one was open, has ended.
		hold.open = false
	}
	if hold == nil || len(hold.lanes) == 0 {
		h.mu.Unlock()
		return nil
	}
	lanes := hold.lanes
	hold.lanes = map[string]*heldLane{}
	h.total -= hold.bytes
	hold.bytes = 0
	clear := hold.marked
	hold.marked = false
	h.mu.Unlock()
	var relayed []heldFrame
	counters := m.privacy.counted()
	for project, lane := range lanes {
		if set.private(project) {
			for _, frame := range lane.frames {
				counters.decisions.Inc([2]string{"answer", decisionPrivate})
				m.withhold(sessionID, frame.msg, false)
			}
			continue
		}
		counters.decisions.Add(int64(len(lane.frames)), [2]string{"answer", decisionPublic})
		relayed = append(relayed, lane.frames...)
	}
	// Each lane is in stream order already; frames of two lanes are relayed in the order the
	// stream stored them too.
	slices.SortFunc(relayed, func(a, b heldFrame) int { return cmp.Compare(a.msg.StreamSeq, b.msg.StreamSeq) })
	if clear {
		m.writeHeldMarker(ctx, sessionID, false)
	}
	messages := make([]bus.SessionMessage, len(relayed))
	for i, frame := range relayed {
		messages[i] = frame.msg
	}
	return messages
}

// readBeforeDrop is a fresh privacy read for sessionID's relay, which runs inline in its loop: while
// frames are held for the session it waits no later than when the oldest of them reaches
// holdLimits.undecided, so a read the source answers late never holds that frame past its limit.
// A read cut short there is unknown, and the loop drops the frame on time (redecideHeld).
func (m *sessionRelays) readBeforeDrop(ctx context.Context, sessionID string) (privateSet, error) {
	m.holds.mu.Lock()
	hold := m.holds.sessions[sessionID]
	var oldest time.Time
	held := false
	if hold != nil {
		oldest, held = hold.oldest()
	}
	m.holds.mu.Unlock()
	if !held {
		return m.privacy.read(ctx)
	}
	readCtx, cancel := context.WithDeadline(ctx, oldest.Add(holdLimits.undecided))
	defer cancel()
	return m.privacy.read(readCtx)
}

// heldDue is when sessionID's held lanes are next due a decision - a read again, or the drop of a
// frame undecided at the limit - and whether any frame is held for it.
func (m *sessionRelays) heldDue(sessionID string) (time.Time, bool) {
	if m.holds == nil {
		return time.Time{}, false
	}
	m.holds.mu.Lock()
	defer m.holds.mu.Unlock()
	hold := m.holds.sessions[sessionID]
	if hold == nil {
		return time.Time{}, false
	}
	oldest, held := hold.oldest()
	if !held {
		return time.Time{}, false
	}
	due := oldest.Add(holdLimits.undecided)
	if hold.nextRead.Before(due) {
		due = hold.nextRead
	}
	return due, true
}

// redecideHeld is the hold's pass for sessionID once heldDue has come: every frame undecided at
// the limit is dropped, and every lane left is read again; the frames a whole answer decides
// public are returned to relay.
func (m *sessionRelays) redecideHeld(ctx context.Context, sessionID string) []bus.SessionMessage {
	now := time.Now()
	limits := holdLimits
	h := m.holds
	h.mu.Lock()
	hold := h.sessions[sessionID]
	if hold == nil {
		h.mu.Unlock()
		return nil
	}
	for project, lane := range hold.lanes {
		kept := lane.frames[:0]
		for _, frame := range lane.frames {
			if now.Sub(frame.heldAt) >= limits.undecided {
				size := heldFrameSize(frame.msg)
				lane.bytes -= size
				hold.bytes -= size
				h.total -= size
				m.dropLocked(hold, heldDroppedLimit, 1)
				continue
			}
			kept = append(kept, frame)
		}
		lane.frames = kept
		if len(lane.frames) == 0 {
			delete(hold.lanes, project)
		}
	}
	empty := len(hold.lanes) == 0
	clear := empty && hold.marked
	if clear {
		hold.marked = false
	}
	hold.nextRead = now.Add(limits.redecide)
	h.mu.Unlock()
	if clear {
		m.writeHeldMarker(ctx, sessionID, false)
	}
	if empty {
		return nil
	}
	set, err := m.readBeforeDrop(ctx, sessionID)
	if err != nil {
		m.privacy.counted().unknown.Inc([2]string{"reason", unknownReasonOf(err)})
		return nil
	}
	return m.decideHeld(ctx, sessionID, set)
}

// takeGap reports whether sessionID's next relayed frame carries the hold's gap, and clears it.
func (m *sessionRelays) takeGap(sessionID string) bool {
	if m.holds == nil {
		return false
	}
	m.holds.mu.Lock()
	defer m.holds.mu.Unlock()
	hold := m.holds.sessions[sessionID]
	if hold == nil || !hold.takeGap() {
		return false
	}
	if len(hold.lanes) == 0 && !hold.marked && !hold.open {
		delete(m.holds.sessions, sessionID)
	}
	return true
}

// startHeld is the hold's part of a relay's start for sessionID: a record that carries the held
// marker while this listener holds nothing for the session is a predecessor's, whose held frames
// are lost, so the session's first relayed frame carries the gap and the mark is cleared.
func (m *sessionRelays) startHeld(ctx context.Context, sessionID string) {
	if m.holds == nil {
		return
	}
	entry, _ := m.sessions.Lookup(sessionID)
	if !entry.Held {
		return
	}
	m.holds.mu.Lock()
	if hold := m.holds.sessions[sessionID]; hold != nil && (len(hold.lanes) > 0 || hold.marked) {
		m.holds.mu.Unlock()
		return
	}
	m.holds.sessionLocked(sessionID).gap = true
	m.holds.mu.Unlock()
	m.writeHeldMarker(ctx, sessionID, false)
}

// writeHeldMarker writes sessionID's held marker (session.SessionRegistry.SetHeld). A write that
// does not land marks the session's next frame with the gap, since a later start may not read it.
func (m *sessionRelays) writeHeldMarker(ctx context.Context, sessionID string, held bool) {
	writeCtx, cancel := context.WithTimeout(ctx, heldMarkerTimeout)
	defer cancel()
	err := m.sessions.SetHeld(writeCtx, sessionID, held)
	if err == nil {
		return
	}
	m.logger.Warn("session relay could not write the session's held marker; its next frame carries the gap mark",
		slog.String("session_id", sessionID), slog.Bool("held", held), slog.String("error", err.Error()))
	m.holds.mu.Lock()
	defer m.holds.mu.Unlock()
	m.holds.sessionLocked(sessionID).dropped()
}

// sessionLocked is sessionID's hold, created empty where it has none. The caller holds h.mu.
func (h *privacyHolds) sessionLocked(sessionID string) *sessionHold {
	hold := h.sessions[sessionID]
	if hold == nil {
		hold = &sessionHold{lanes: map[string]*heldLane{}}
		h.sessions[sessionID] = hold
	}
	return hold
}

// forgetHeld drops every frame held for sessionID, whose relay this listener no longer runs: the
// listener the session moved to reads the held marker its record still carries.
func (m *sessionRelays) forgetHeld(sessionID string) {
	if m.holds == nil {
		return
	}
	m.holds.mu.Lock()
	defer m.holds.mu.Unlock()
	hold := m.holds.sessions[sessionID]
	if hold == nil {
		return
	}
	m.holds.total -= hold.bytes
	delete(m.holds.sessions, sessionID)
}
