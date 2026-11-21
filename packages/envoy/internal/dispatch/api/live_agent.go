package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The agent view is one live session's own conversation on a page's live socket: its turns, tool
// calls and streamed output, relayed from the session itself (agentstream), one agent frame each.
// It keeps every rule of the agent stream (agent_stream.go): a person's alone, nothing written, no
// frame's bytes logged.
//
// A subscription subscribes to the session's frames before anything else, which is what makes the
// replay safe to stitch on: a frame the session publishes while the replay request is in flight
// lands in the subscription's buffer rather than falling between the two. It then tells the session
// a viewer is attached, since a session publishes only while one is, and asks for the replay. All
// of it runs on the subscription's own goroutine, the replay's wait of up to
// agentStreamReplayTimeout and every re-arm included, so the socket's read loop never waits on a
// session.
//
// A viewer that cannot keep up ends its subscription (behind) rather than losing a frame. The relay
// never parses a frame, so it cannot tell a superseded snapshot from a tool result or the settled
// frame of an earlier message, and dropping the wrong one leaves a tool call reading "running…"
// until the page reloads. Ending costs the page a subscribe, and the subscribe rebuilds from the
// session's own replay, which is exactly the state that was lost.

// The events an agent frame names: whether anyone answers on the session's control subject, the
// history the session can still replay, and one frame it published.
const (
	agentEventResponder = "responder"
	agentEventReplay    = "replay"
	agentEventFrame     = "frame"
)

// startAgentView checks an agent view, then starts the view's subscription. The view keeps the
// rule of GET /api/v1/agents/{session_id}/messages (liveViewReads): only a credential acting as
// its person may watch a session's conversation — the person scope, which /ws/live cannot take
// from the route table's gate because it is one socket serving every view, so it is refused here
// with the gate's code, in the words the view has always refused it in — and the conversation of a
// session the caller may not see answers as one nobody answers for (relayAgentConversation). The
// view names its kind and a session uuid alone; a deployment with no relay (no NATS) has nothing to
// watch. A frame carries no HTTP header, so a refusal carries no WWW-Authenticate challenge beside
// it.
func (s *server) startAgentView(ctx context.Context, stream liveStreamer, sub string, view LiveView, _ *EventsLiveResume) (liveSubscription, *LiveError) {
	if !stream.Holds(model.ScopePerson) {
		refusal := liveRefusal(sub, codeScopeRequired, "this socket's credential does not hold the "+
			model.ScopePerson+" scope, which watching an agent's conversation requires")
		return nil, &refusal
	}
	if refusal := liveReadRefusal(ctx, sub, liveAgentView, stream.Holds, s.liveReader(stream)); refusal != nil {
		return nil, refusal
	}
	if view.Window != nil || view.Query != nil || view.At != nil {
		refusal := liveRefusal(sub, "INVALID_FRAME", "an agent view names its kind and its session_id and nothing else")
		return nil, &refusal
	}
	raw := ""
	if view.SessionID != nil {
		raw = *view.SessionID
	}
	sessionID, problem := agentStreamSessionID(raw)
	if problem != "" {
		refusal := liveRefusal(sub, "SESSION_ID_INPUT", problem)
		return nil, &refusal
	}
	if s.deps.AgentStream == nil {
		refusal := liveRefusal(sub, "STREAM_UNAVAILABLE", "this deployment has no agent conversation relay (it needs NATS)")
		return nil, &refusal
	}
	return s.startAgentSubscription(ctx, stream, sub, sessionID), nil
}

// startAgentSubscription starts sub's view of sessionID's conversation on a goroutine of its own and
// returns at once. The goroutine writes the subscription's frames through stream until ctx ends or
// the subscription is stopped: the responder verdict first, then the session's replay when it has
// one, then each frame the session publishes, and an ended frame should the view fall behind
// (behind), or the relay drop the viewer or fail it (dropped).
func (s *server) startAgentSubscription(ctx context.Context, stream liveStreamer, sub string, sessionID string) liveSubscription {
	return runSubscription(ctx, func(ctx context.Context) { s.relayAgentConversation(ctx, stream, sub, sessionID) })
}

// relayAgentConversation is one agent subscription's whole life: it returns once the subscription
// has ended, on its own or because ctx did.
//
// A session whose conversation the caller may not read whole (sessionContentHiddenFrom) is
// answered as one nobody answers for, as GET /api/v1/agents/{session_id}/messages answers its list
// as the empty list of a session nobody knows: the no-responder verdict and nothing after it, and
// the relay never subscribes to the session, tells it a viewer is attached or asks for its replay.
// The view relays the session's raw transcript, whose tool results hold whatever the session read,
// and a read records nothing, where the HTTP read filters each message by its own project. So a
// viewer that is not open watches only a session its own person's credential proved, and any
// other viewer, while a project is private, only one whose reach it sees: the projects its listener
// record lists, or with no list the private projects of the people who proved it, and never one
// that acted in or reached a project it does not see (access.SessionContentClause). The rule is
// asked again before each batch of the session's frames goes out (with the reader the view last
// resolved), and with the reader resolved again on each change of a project's access or of a
// group the event relay carries, on every heartbeat (accessRecheckEvery) and on each re-arm; a
// view whose session the caller no longer may read ends (dropped), so its page subscribes again
// and is answered as above.
func (s *server) relayAgentConversation(ctx context.Context, stream liveStreamer, sub, sessionID string) {
	reader, err := s.readerFor(ctx, stream.Caller())
	if err != nil {
		endAgentAfterFailure(ctx, stream, sub, "resolve what the caller sees", err)
		return
	}
	hidden, err := s.sessionContentHiddenFrom(ctx, reader, sessionID)
	if err != nil {
		endAgentAfterFailure(ctx, stream, sub, "read whether the caller sees the session", err)
		return
	}
	if hidden {
		if stream.SendWait(ctx, liveAgentFrame(sub, agentEventResponder, responderEvent(false))) == nil {
			<-ctx.Done()
		}
		return
	}
	// The access changes, from the event relay: a change of a project's access or of a group may
	// take the session's projects from the caller, so each one asks the rule again. A relay the
	// view cannot join, or one whose feed this view fell behind, leaves the heartbeat to ask.
	var accessChanges <-chan relayedEvent
	if s.eventRelay != nil {
		if feed, err := s.eventRelay.subscribe(ctx, liveEventsBuffer); err == nil {
			defer feed.cancel()
			accessChanges = feed.events
		}
	}
	source := s.deps.AgentStream
	queue := newAgentStreamQueue()
	// queue.overflow closes once a frame would take the view past its budget; dropped once the
	// relay ended this viewer, which only the in-memory source's test hook does
	// (agentstream.Memory.Drop): a session's own frame is never nil. The e2e suite has no other way
	// to make a page rebuild its transcript from a second replay.
	dropped := make(chan struct{})
	var droppedOnce sync.Once
	unsubscribe, err := source.Subscribe(sessionID, func(frame agentstream.Frame) {
		if frame == nil {
			droppedOnce.Do(func() { close(dropped) })
			return
		}
		queue.push(frame)
	})
	if err != nil {
		endAgentAfterFailure(ctx, stream, sub, "subscribe to the session's frames", err)
		return
	}
	var stopRelayOnce sync.Once
	stopRelay := func() { stopRelayOnce.Do(unsubscribe) }
	defer stopRelay()
	// The session publishes only while a viewer is attached, so it has to hear from this one
	// before the first turn, not after it.
	if err := source.Watch(sessionID); err != nil {
		stopRelay()
		endAgentAfterFailure(ctx, stream, sub, "tell the session a viewer is attached", err)
		return
	}
	skipped := 0
	defer func() {
		if skipped > 0 {
			slog.Warn("dispatch: skipped agent frames that are not JSON", "skipped", skipped)
		}
	}()
	// send writes data, which the socket carries as JSON it never parses, as sub's agent frame named
	// event. A session's bytes that are not JSON could not be carried at all (the writer would close
	// the socket), so they are skipped and counted. It is false once the subscription has ended.
	send := func(event string, data []byte) bool {
		if !json.Valid(data) {
			skipped++
			return true
		}
		return stream.SendWait(ctx, liveAgentFrame(sub, event, data)) == nil
	}
	// answer says whether anyone is listening on the session's control subject, which says whether
	// it can stream at all - every plugin release that can answers, and one that predates the live
	// view never does - and then sends the history it answered with. A session that answers with
	// nothing has simply not spoken yet, and a replay that failed for any other reason leaves the
	// viewer the live conversation with no history behind it.
	answer := func(replay agentstream.Frame, responding bool) bool {
		if !send(agentEventResponder, responderEvent(responding)) {
			return false
		}
		return len(replay) == 0 || send(agentEventReplay, replay)
	}
	replay, responding := s.replayAgentStream(ctx, source, sessionID)
	if !answer(replay, responding) {
		return
	}

	// endedBy is why the view has ended, or "": it fell past its budget and lost a frame, or the
	// relay dropped it as a lost connection does.
	endedBy := func() string {
		select {
		case <-queue.overflow:
			return liveEndedBehind
		default:
		}
		select {
		case <-dropped:
			return liveEndedDropped
		default:
			return ""
		}
	}
	// stillReads resolves what the caller sees again and asks the rule again
	// (sessionContentHiddenFrom), and reports whether the view goes on: a session the caller may no
	// longer read is told nothing more, and the view ends.
	stillReads := func() bool {
		resolved, err := s.readerFor(ctx, stream.Caller())
		if err == nil {
			reader = resolved
			hidden, err = s.sessionContentHiddenFrom(ctx, reader, sessionID)
		}
		return s.agentViewGoesOn(ctx, stream, sub, hidden, err, stopRelay)
	}
	// batchStillReads asks the rule again, with the reader the view last resolved, before each
	// batch of the session's frames goes out. A write that ties the session to another person
	// (session_owners), records a project it newly wrote in (session_projects) or one its person
	// lost (session_reached_projects) commits before the session publishes the frame after it, and
	// may take the session from this caller. Nothing announced in that write could order the two:
	// the event relay hears a commit through its LISTEN and its coalescing window
	// (eventRelayCoalesce), and a frame from NATS wins that race, so only a read in this process
	// after the commit orders the rule before the frame. It reads nothing where no such write can
	// change the answer (agentViewReadsWhatever).
	batchStillReads := func() bool {
		if agentViewReadsWhatever(reader, sessionID) {
			return true
		}
		hidden, err := s.sessionContentHiddenFrom(ctx, reader, sessionID)
		return s.agentViewGoesOn(ctx, stream, sub, hidden, err, stopRelay)
	}
	rearm := time.NewTicker(s.agentWatchInterval)
	defer rearm.Stop()
	heartbeat := time.NewTicker(s.accessRecheckEvery())
	defer heartbeat.Stop()
	for {
		var frames []agentstream.Frame
		var size int
		select {
		case <-ctx.Done():
			return
		case <-queue.overflow:
		case <-dropped:
		case <-queue.wake:
			frames, size = queue.take()
		case event, open := <-accessChanges:
			if !open {
				// The view fell behind the relay's feed: the heartbeat asks from here on.
				accessChanges = nil
				continue
			}
			if (event.Event.Type == projectAccessChangedEvent || event.Event.Type == accessGroupChangedEvent) && !stillReads() {
				return
			}
			continue
		case <-heartbeat.C:
			if !stillReads() {
				return
			}
			continue
		case <-rearm.C:
			// What the caller sees is read again before the session hears from this viewer.
			if !stillReads() {
				return
			}
			if err := source.Watch(sessionID); err != nil {
				stopRelay()
				endAgentAfterFailure(ctx, stream, sub, "re-arm the session", err)
				return
			}
			if responding {
				continue
			}
			// The session was not on its control subject when this viewer arrived. It may be now -
			// a session restarted onto a plugin that streams - so every re-arm asks again, and the
			// first answer delivers its history without the page subscribing again.
			retry, answered := s.replayAgentStream(ctx, source, sessionID)
			if !answered {
				continue
			}
			responding = true
			if !answer(retry, true) {
				return
			}
			continue
		}
		// A frame goes only while nothing has ended the view: sending what was queued after the
		// frame the budget lost would stitch a gap into the middle of the conversation, and the
		// page's subscribe replays it whole, which is the honest answer.
		if reason := endedBy(); reason != "" {
			_ = stream.SendWait(ctx, liveEnded(sub, reason))
			return
		}
		if len(frames) > 0 && !batchStillReads() {
			return
		}
		for _, frame := range frames {
			if !send(agentEventFrame, frame) {
				return
			}
		}
		queue.release(size)
	}
}

// agentViewGoesOn reports whether an agent view goes on after a re-ask of its rule answered hidden
// and err: a view the caller still reads goes on; one it no longer reads stops its relay (stop)
// and ends (dropped), and one whose re-ask failed ends through endAgentAfterFailure.
func (s *server) agentViewGoesOn(ctx context.Context, stream liveStreamer, sub string, hidden bool, err error, stop func()) bool {
	if err == nil && !hidden {
		return true
	}
	stop()
	if err != nil {
		endAgentAfterFailure(ctx, stream, sub, "read whether the caller still sees the session", err)
		return false
	}
	_ = stream.SendWait(ctx, liveEnded(sub, liveEndedDropped))
	return false
}

// agentViewReadsWhatever reports whether the content rule (access.SessionContentClause) answers
// reader's view of session the same whatever the session's ties, pairs and reached projects hold,
// so the re-ask before a batch of its frames reads nothing:
//
//   - the server's viewer (All), which the rule passes first;
//   - the viewer's own session, which the rule passes next;
//   - an open viewer while no project is private (R9), which the rule passes before it reads a
//     row. A project made private is an access change, on which the view resolves its reader
//     again (stillReads), so a later batch reads.
func agentViewReadsWhatever(reader model.Actor, session string) bool {
	viewer := reader.Viewer
	return viewer != nil && (viewer.All || (viewer.Session != "" && viewer.Session == session) || (viewer.Open && !viewer.AnyPrivate))
}

// endAgentAfterFailure ends a view whose relay call failed with an ended frame (dropped), so its
// page subscribes again rather than watching a session that never hears of it. It waits
// agentStreamRetry first: a page answers an ended frame by subscribing again at once, and a relay
// that keeps failing would otherwise have the page and the subscription take turns as fast as the
// failure comes back. A view whose own context ended (stopped, or its socket closed) sends nothing.
func endAgentAfterFailure(ctx context.Context, stream liveStreamer, sub, what string, err error) {
	if ctx.Err() != nil {
		return
	}
	slog.Warn("dispatch: an agent view could not "+what+"; it ends, and its page subscribes again", "error", err)
	if !waitFor(ctx, agentStreamRetry) {
		return
	}
	_ = stream.SendWait(ctx, liveEnded(sub, liveEndedDropped))
}

// liveAgentFrame is sub's agent frame named event, carrying data: the session's own JSON, or the
// responder verdict Dispatch writes (responderEvent).
func liveAgentFrame(sub, event string, data []byte) LiveAgentFrame {
	return LiveAgentFrame{Type: "agent", Sub: sub, Event: event, Data: data}
}
