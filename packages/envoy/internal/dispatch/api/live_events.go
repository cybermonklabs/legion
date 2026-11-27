package api

import (
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The events view is the Dispatch event feed on a page's live socket: every committed event its
// caller may read, in id order, one event frame each.
//
// Its live source is the process's event stream relay (event_stream_relay.go), which publishes
// every committed event it reads back out of Postgres - this process's and the other
// envoy-dispatch process's alike - up to the safe horizon (events.Horizon), in id order, each
// encoded once for every subscriber. A subscription takes its relay subscription together with the
// cursor the relay had published through when that feed began, which is what leaves no gap: every
// event at or below that cursor is final and is replayed here from the log, and every event above
// it reaches the relay subscription (feedEvents).
//
// That handover is also why a subscription that names no since starts where the relay stands
// rather than at a head a page asked for first: a page that read the newest id through a separate
// request and subscribed from it would lose an event that drew a lower id and committed after
// that read. The one head a page reads beforehand is GET /auth/whoami's event_head, which it reads
// before it starts any read the feed keeps current: an id at or below which every event is already
// final (eventFeedHead), so what commits after it stands above it and is the feed's to replay.
//
// What a resume is replayed is bounded (resolveEventReplay): past maxLiveReplay events or
// maxLiveReplayBytes of their encodings it is replayed nothing, and its events_start says
// replay_complete false, which is the page's cue to refresh everything it shows once. A
// subscription holds no record of what it replayed: the one id it keeps is the highest it has
// sent, so its state is bounded whatever the backlog was, and it is freed with the subscription
// when the page unsubscribes or the socket closes.
//
// The event log can be reset under a reader (the browser suite's e2e/seed.ts deletes its rows and
// restarts its ids; `truncate events restart identity` does the same): the ids restart at 1 and
// the relay, which notices, ends the generation and with it every open subscription, and each page
// subscribes again. A page only ever raises its
// cursor and never lowers it, so it resumes with a since above the new log's newest id. Unclamped,
// the live loop would then drop every event of the new log for ever, on a subscription that stays
// open: a reader silent for good, with nothing logged and nothing failed. So a since above the
// newest id starts where the relay stands, as no since does.
//
// A subscription sends only the events its caller may read, by the event log's one rule
// (eventReadableSQL), for what the caller sees as resolved from the credential its socket's upgrade
// proved (readerFor): resolved when the subscription starts, and again at each event that can move
// it (movesSight). One that moved it ends the subscription (behind), and the page's resubscribe is
// answered replay_complete false, because its gap holds that access change: the page refreshes
// everything it shows, which drops what it read of a project its caller no longer sees and reads
// one it now sees (resolveEventReplay).

const (
	// maxLiveReplay and maxLiveReplayBytes bound what a resuming events subscription is replayed:
	// at most 256 events, whose encodings total at most 1 MiB. A page further behind is replayed
	// nothing and refreshes what it shows, which costs less than sending it that backlog
	// (resolveEventReplay).
	maxLiveReplay      = 256
	maxLiveReplayBytes = 1 << 20
	// liveEventsBuffer is how many relayed events one events subscription may fall behind before
	// the relay's stream drops it: 64, more than any one request commits. A dropped subscription
	// subscribes to the relay again and catches up what it missed from the event log, within the
	// replay's bounds (resumeAfterOverflow). A subscription also holds what it took off its
	// channel to ask the log's rule of in one statement (takeQueued), at most as many again.
	liveEventsBuffer = 64
)

// The events whose commit can move what a viewer sees (access.Resolve): a change of a project's
// owner, visibility or access list, a change of a group, and a new project, which every full member
// sees while it is public.
const (
	projectAccessChangedEvent = "project.access_changed"
	accessGroupChangedEvent   = "access_group.changed"
	projectCreatedEvent       = "project.created"
)

// startEventsView checks an events view, then starts the view's subscription. The view keeps the
// rule of GET /api/v1/events/log (liveViewReads, liveReadRefusal). It names its kind alone, and the
// resume the decimal id of the last event the page applied.
func (s *server) startEventsView(ctx context.Context, stream liveStreamer, sub string, view LiveView, resume *EventsLiveResume) (liveSubscription, *LiveError) {
	if refusal := liveReadRefusal(ctx, sub, liveEventsView, stream.Holds, s.liveReader(stream)); refusal != nil {
		return nil, refusal
	}
	if view.Window != nil || view.Query != nil || view.At != nil || view.SessionID != nil {
		refusal := liveRefusal(sub, "INVALID_FRAME", "an events view names its kind and nothing else")
		return nil, &refusal
	}
	var since *int64
	if resume != nil {
		value, err := strconv.ParseInt(resume.Since, 10, 64)
		if err != nil || value < 0 {
			refusal := liveRefusal(sub, "INVALID_FRAME", "resume.since must be a decimal event id")
			return nil, &refusal
		}
		since = &value
	}
	return s.startEventsSubscription(ctx, stream, sub, since), nil
}

// startEventsSubscription starts sub's event feed on a goroutine of its own, after since when it
// is not nil, and returns at once. The goroutine writes the subscription's frames through stream
// until ctx ends or the subscription is stopped: events_start first, then the replay after the
// id it names, then the events the relay publishes, and an ended frame (behind) should the relay
// end its generation, a catch-up pass the replay's bounds, or a read fail.
func (s *server) startEventsSubscription(ctx context.Context, stream liveStreamer, sub string, since *int64) liveSubscription {
	return runSubscription(ctx, func(ctx context.Context) { s.feedEvents(ctx, stream, sub, since) })
}

// feedEvents is one events subscription's whole life: it returns once the subscription has ended,
// on its own or because ctx did.
//
// Event ids do not commit in id order across owners (events.Broker.Append), so a page's resume
// point and every point this subscription hands over at must be ones no later commit can fall at
// or below. The relay publishes only up to the safe horizon (events.Horizon), each event once and
// in id order, and the cursor it had published through, read together with the relay subscription
// (eventStreamRelay.subscribe), is the handover: every event at or below it is final and is
// replayed here from the log, within the replay's bounds, and every event above it reaches the
// relay subscription.
//
// The relay keeps a reset log's cursor until its reset check has run (resetCursor), which ends the
// generation and waits, on the next generation, for a horizon proven after the reset. A relay
// subscription taken meanwhile carries a published cursor that names a position in the old log:
// above the new log's newest id, or at an id whose row is gone or carries another created_at than
// the relay read (events.EventLogWasReset). Started there, the subscription would drop the new
// log's events up to it, or start and then be ended by the relay's reset, sending the page through
// a resubscribe and, for a page with no complete replay, the whole refresh it owes a gap. So the
// subscription waits for that reset, which ends the relay subscription, and subscribes again on
// the generation the relay starts next. A subscription with no since then replays from
// replacedFrom, read when it found the relay on the replaced log, rather than starting at the new
// generation's cursor: that cursor is taken under a horizon proven after the reset, so an event of
// the new log that commits while that proof is awaited can stand at or below it. replacedFrom is
// the id before the first one missing from the new log, whose ids restart at 1, which is every
// event committed then and none still open.
func (s *server) feedEvents(ctx context.Context, stream liveStreamer, sub string, since *int64) {
	replacedFrom := int64(-1)
	var subscription relaySubscription
	var subscribed bool
	defer func() {
		if subscribed {
			subscription.cancel()
		}
	}()
	var head int64
	var reader model.Actor
	behind := -1
	accessChanged := false
	for {
		next, err := s.eventRelay.subscribe(ctx, liveEventsBuffer)
		if err != nil {
			endEventsAfterFailure(ctx, stream, sub, "subscribe to the event stream relay", err)
			return
		}
		if subscribed {
			subscription.cancel()
		}
		subscription, subscribed = next, true
		// What the caller sees, resolved after the relay subscription began, so it holds every
		// access change at or below the subscription's head, and before the head is read below,
		// so every access change it holds stands at or below that head (eventsReader.through).
		if reader, err = s.readerFor(ctx, stream.Caller()); err != nil {
			endEventsAfterFailure(ctx, stream, sub, "resolve what the caller sees", err)
			return
		}
		// The log's newest id, and the row at the relay's cursor, as they stand now, after the
		// relay subscription began. A cursor above the newest id, or one whose row is gone or
		// carries another created_at, is a relay whose reset check has not yet run. The same
		// statement counts what a resume is behind and asks whether an access change stands in
		// that gap (eventSubscriptionCheck), so a resume reads the pool once before its replay
		// rather than twice.
		var cursorAt *time.Time
		if head, cursorAt, behind, accessChanged, err = eventSubscriptionCheck(ctx, s.deps.Store.Pool, reader, subscription.head, since); err != nil {
			endEventsAfterFailure(ctx, stream, sub, "read the event log's head", err)
			return
		}
		if !events.EventLogWasReset(subscription.head, subscription.headAt, head, cursorAt) {
			break
		}
		if since == nil && replacedFrom < 0 {
			if replacedFrom, err = s.eventLogUnbrokenThrough(ctx); err != nil {
				endEventsAfterFailure(ctx, stream, sub, "read the event log's unbroken ids", err)
				return
			}
		}
		subscription.wake()
		// Whatever the relay publishes before its reset check is a position in the old log, so it
		// is dropped; the close is its reset ending this generation.
		for open := true; open; {
			select {
			case <-ctx.Done():
				return
			case _, open = <-subscription.events:
			}
		}
	}

	replay, err := s.resolveEventReplay(ctx, reader, since, subscription.head, head, replacedFrom, behind, accessChanged)
	if err != nil {
		endEventsAfterFailure(ctx, stream, sub, "read the event log", err)
		return
	}
	// The since events_start names is a head the caller reads: a since the page sent comes back as
	// it was, and every other, the relay's cursor or a replaced log's unbroken id, is read as the
	// highest id of an event in the caller's gap (viewerEventHead), so no event of a project the
	// caller does not see moves it.
	start := replay.start
	if since == nil || start != *since {
		if start, err = viewerEventHead(ctx, s.deps.Store.Pool, reader, start); err != nil {
			endEventsAfterFailure(ctx, stream, sub, "read the caller's head of the event log", err)
			return
		}
	}
	if stream.SendWait(ctx, liveEventsStart(sub, start, replay.complete)) != nil {
		return
	}
	if !sendReplayedEvents(ctx, stream, sub, replay.events) {
		return
	}

	// The live loop skips an event at or below after, the highest id this subscription holds: it
	// starts at replay.after (the replay's last id, or what the page held through its since when
	// the relay stands behind it) and moves to each id the loop takes, whether or not its caller
	// may read it. The relay publishes into this subscription only events above its head, each
	// once and in strictly increasing id order, so the replay's (replay.start, head] and the ids
	// above after are together every event after replay.start, each taken exactly once and in id
	// order. One id, not a set: a relay standing behind a subscription made after it sends ids at
	// or below after - events this page has already seen, or backlog a page with no since asked
	// not to get - and the comparison is all it takes to leave them out.
	//
	// A subscription that falls liveEventsBuffer events behind is dropped by the relay's stream,
	// which closes its channel while the generation runs on (Stream.Publish). The loop reads what
	// the channel still holds, subscribes again and catches up from the log what it missed after
	// after (resumeAfterOverflow), then goes on live on the new subscription. A backlog past the
	// replay's bounds ends the subscription instead: the page subscribes again from the last id
	// it applied, which resolves its position as any resume does.
	//
	// What the relay publishes it read for every subscriber at once (everyReader), so each event
	// goes out only once the log's rule passes it for this caller (sendReadable). The loop takes
	// every event already queued with the one it received (takeQueued), so one statement decides
	// them all, and an event that can move what the caller sees resolves the caller again first.
	// An access change no event carries moves what the caller sees too, so the loop also resolves
	// the caller again on the socket's heartbeat (recheckEventsReader).
	//
	// A reset reuses ids, and the comparison cannot tell this page apart from the events of the
	// log it was reading; the relay ends the generation at a reset instead, which ends this
	// subscription, and the page comes back on a new one, which clamps its since against the log
	// as it then stands.
	view := eventsReader{reader: reader, through: head}
	queued := make([]relayedEvent, 0, liveEventsBuffer+1)
	after := replay.after
	recheck := time.NewTicker(s.accessRecheckEvery())
	defer recheck.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-recheck.C:
			if !s.recheckEventsReader(ctx, stream, sub, &view) {
				return
			}
		case event, open := <-subscription.events:
			if open {
				var sending bool
				if after, sending = s.sendReadable(ctx, stream, sub, &view, takeQueued(queued, subscription.events, event), after); !sending {
					return
				}
				continue
			}
			select {
			case <-subscription.ended:
				// The relay ended this generation: the log was reset under its cursor. The page
				// subscribes again from the last id it applied. A shutdown ends the generation
				// too, but it closes the socket with 1012 and stops its writer (live_socket.go),
				// so no ended frame reaches the page then: this one is a reset's.
				_ = stream.SendWait(ctx, liveEnded(sub, liveEndedBehind))
				return
			default:
			}
			next, caughtUp, caughtUpView, ok := s.resumeAfterOverflow(ctx, stream.Caller(), after)
			if !ok {
				_ = stream.SendWait(ctx, liveEnded(sub, liveEndedBehind))
				return
			}
			subscription.cancel()
			subscription, view = next, caughtUpView
			if !sendReplayedEvents(ctx, stream, sub, caughtUp.events) {
				return
			}
			after = caughtUp.after
		}
	}
}

// sendReplayedEvents writes each event's frame on sub, in order, every one of events to be sent: a
// replay, or a run read as its reader reads it (sendRun); false once the subscription has ended.
func sendReplayedEvents(ctx context.Context, stream liveStreamer, sub string, events []relayedEvent) bool {
	for _, event := range events {
		if stream.SendWait(ctx, liveEventFrame(sub, event)) != nil {
			return false
		}
	}
	return true
}

// accessRecheckEvery is how often a live reader is resolved again whatever its events say: the
// socket's heartbeat (Deps.StreamHeartbeat), on which the socket asks again of its credential too
// (whileCallerResolves).
func (s *server) accessRecheckEvery() time.Duration {
	return cmp.Or(s.deps.StreamHeartbeat, defaultStreamHeartbeat)
}

// recheckEventsReader resolves the caller of an events subscription again on the socket's
// heartbeat (accessRecheckEvery) and reports whether the subscription goes on. An access change
// that no event carries, one made by SQL alone, which appends none and announces nothing on
// store.AccessChangedChannel, moves what the caller sees all the same, and no event can tell the live
// loop so: this is what finds it, a heartbeat at most after it committed. One that moved what the
// caller sees (sightMoved) ends the subscription (behind), as an access change an event carries
// does (sendReadable), so nothing more of a project the caller lost reaches its page; the
// resubscribe's gap holds no event of the change, so it is replayed as complete, and the page keeps
// what it read of that project before the change until it next reads everything again. Otherwise
// the loop goes on with the reader it read; a read that fails ends the subscription, as every
// failed read of the loop does.
func (s *server) recheckEventsReader(ctx context.Context, stream liveStreamer, sub string, view *eventsReader) bool {
	next, err := s.readerFor(ctx, stream.Caller())
	if err != nil {
		endEventsAfterFailure(ctx, stream, sub, "resolve what the caller sees", err)
		return false
	}
	if sightMoved(view.reader, next, model.Event{}) {
		_ = stream.SendWait(ctx, liveEnded(sub, liveEndedBehind))
		return false
	}
	view.reader = next
	return true
}

// eventsReader is who an events subscription sends the log to: the caller as last resolved
// (reader), and through, the log's newest id as it stood just after the subscription last resolved
// the caller where no event asked it to (feedEvents, resumeAfterOverflow). An access change that
// resolution can hold committed before it and so stands at or below through; one above through
// committed after it and is not in it.
type eventsReader struct {
	reader  model.Actor
	through int64
}

// movesSight reports whether an event of eventType can move what a viewer sees.
func movesSight(eventType string) bool {
	return eventType == projectAccessChangedEvent || eventType == accessGroupChangedEvent || eventType == projectCreatedEvent
}

// takeQueued is first and every relayed event already queued behind it on events, in buffer's
// storage, so one statement asks the log's rule of them all (sendReadable). It takes only what the
// channel held as it looked, so it never waits, and leaves a close behind them to the live loop's
// next receive.
func takeQueued(buffer []relayedEvent, events <-chan relayedEvent, first relayedEvent) []relayedEvent {
	batch := append(buffer[:0], first)
	for range len(events) {
		batch = append(batch, <-events)
	}
	return batch
}

// sendReadable sends sub each event of batch above after that its caller may read, in id order,
// and answers the highest id it took; false once the subscription has ended. The relay read batch
// for every subscriber at once (everyReader), so the log's one rule (eventReadableSQL) is asked
// here, in one statement for each run of events between two that can move what the caller sees
// (movesSight). Each of those resolves the caller again before the run it opens is asked, so it is
// itself read under what the caller sees once it committed; one that moved what the caller sees
// ends the subscription (behind) instead, and the page's resubscribe refreshes everything it shows
// (resolveEventReplay).
func (s *server) sendReadable(ctx context.Context, stream liveStreamer, sub string, view *eventsReader, batch []relayedEvent, after int64) (int64, bool) {
	fresh := batch[:0]
	for _, event := range batch {
		if event.Event.ID > after {
			fresh = append(fresh, event)
			after = event.Event.ID
		}
	}
	start := 0
	for i, event := range fresh {
		if !movesSight(event.Event.Type) {
			continue
		}
		if !s.sendRun(ctx, stream, sub, view.reader, fresh[start:i]) {
			return after, false
		}
		moved, err := s.resolveAgain(ctx, view, stream.Caller(), event.Event)
		if err != nil {
			endEventsAfterFailure(ctx, stream, sub, "resolve what the caller sees", err)
			return after, false
		}
		if moved {
			_ = stream.SendWait(ctx, liveEnded(sub, liveEndedBehind))
			return after, false
		}
		start = i
	}
	return after, s.sendRun(ctx, stream, sub, view.reader, fresh[start:])
}

// sendRun sends sub each event of run that reader may read; false once the subscription has ended.
// The relay encoded each event once as the server reads it (everyReader), which is every reader's
// frame while the reader sees every object (seesEveryObject). Any other reader is sent the events
// as it reads them (readerEvents): the fields a read fills for its reader (an ask's backlink count,
// its documents) counted for it, and each reference to an object it does not see left out.
func (s *server) sendRun(ctx context.Context, stream liveStreamer, sub string, reader model.Actor, run []relayedEvent) bool {
	if len(run) == 0 {
		return true
	}
	if !seesEveryObject(reader) {
		readable, err := s.readerEvents(ctx, reader, run)
		if err != nil {
			endEventsAfterFailure(ctx, stream, sub, "read the events as the caller reads them", err)
			return false
		}
		return sendReplayedEvents(ctx, stream, sub, readable)
	}
	readable, err := s.readableEventIDs(ctx, reader, run)
	if err != nil {
		endEventsAfterFailure(ctx, stream, sub, "read which events the caller may read", err)
		return false
	}
	for _, event := range run {
		if len(readable) == 0 {
			break
		}
		if event.Event.ID != readable[0] {
			continue
		}
		readable = readable[1:]
		if stream.SendWait(ctx, liveEventFrame(sub, event)) != nil {
			return false
		}
	}
	return true
}

// readerEvents is every event of run that reader may read (eventReadableSQL), in id order, read
// and encoded as reader reads it (readEventRows), for a reader that does not see every object.
func (s *server) readerEvents(ctx context.Context, reader model.Actor, run []relayedEvent) ([]relayedEvent, error) {
	events, err := s.readEventRows(ctx, reader, eventSelect+`
		where e.id = any($1) and `+eventReadableSQL("e", "$2")+` order by e.id
	`, relayedEventIDs(run), refs.VisibilityArg(reader))
	if err != nil {
		return nil, err
	}
	encoded := make([]relayedEvent, len(events))
	for i, event := range events {
		frame, err := encodeLiveEvent(event)
		if err != nil {
			return nil, err
		}
		encoded[i] = relayedEvent{Event: event, Encoded: frame}
	}
	return encoded, nil
}

// readableEventIDs is the ids of the events of run that reader may read (eventReadableSQL), in id
// order, run's own.
func (s *server) readableEventIDs(ctx context.Context, reader model.Actor, run []relayedEvent) ([]int64, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select e.id from events e where e.id = any($1) and `+eventReadableSQL("e", "$2")+` order by e.id
	`, relayedEventIDs(run), refs.VisibilityArg(reader))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// relayedEventIDs is the id of each event of run, in run's order.
func relayedEventIDs(run []relayedEvent) []int64 {
	ids := make([]int64, len(run))
	for i, event := range run {
		ids[i] = event.Event.ID
	}
	return ids
}

// resolveAgain resolves caller again at event, one that can move what it sees (movesSight), keeps
// what it reads in view, and reports whether what the caller sees moved. An access change at or
// below view.through may already be in the reader view held, so it moves it whatever the caller
// sees now, where it can have moved what the page shows (mayPredatePage); any other event moves it
// when the caller now sees another set of projects, or is open where it was not or the reverse,
// beyond the project a project.created adds (sightMoved).
func (s *server) resolveAgain(ctx context.Context, view *eventsReader, caller model.Actor, event model.Event) (bool, error) {
	if event.ID <= view.through && mayPredatePage(view.reader, event) {
		return true, nil
	}
	next, err := s.readerFor(ctx, caller)
	if err != nil {
		return false, err
	}
	held := view.reader
	view.reader = next
	return sightMoved(held, next, event), nil
}

// mayPredatePage reports whether event, an access change at or below eventsReader.through that the
// reader held may already hold, can have moved what the page shows though the reader did not move:
// the page read before the change, and the reader was resolved after it. A project.created never
// has, since that event tells the page of the project. For a reader that sees every object
// (seesEveryObject) only a change of a project's visibility can have, a project made public, which
// a page read while it was private lacks (the resume asks the same, madePublicSQL); no write of a
// group, a list, an owner or a project's Slack channels moves what such a reader sees, as none did
// before a project could be private (R9). A change whose payload names none is taken as one that
// can.
func mayPredatePage(held model.Actor, event model.Event) bool {
	switch {
	case event.Type == projectCreatedEvent:
		return false
	case !seesEveryObject(held):
		return true
	case event.Type != projectAccessChangedEvent:
		return false
	}
	change, named := accessChangeOf(event)
	return !named || change == model.AccessVisibilityChanged
}

// accessChangeOf is the change a project.access_changed event names (model.ProjectAccessEventPayload),
// and false when its payload names none: the relay's events carry the JSON the log holds
// (readEventRows).
func accessChangeOf(event model.Event) (string, bool) {
	switch payload := event.Payload.(type) {
	case model.ProjectAccessEventPayload:
		return payload.Change, payload.Change != ""
	case json.RawMessage:
		var decoded model.ProjectAccessEventPayload
		if json.Unmarshal(payload, &decoded) == nil && decoded.Change != "" {
			return decoded.Change, true
		}
	}
	return "", false
}

// sightMoved reports whether next sees otherwise than held, past what event explains. A
// project.created event's project, which a full member comes to see as it is made, moves nothing:
// the page owes no refresh for it, since that event tells the page the project is there. A reader
// that saw every object (seesEveryObject) and still does moved nothing either: each project it sees
// now and did not then was made since, and the feed sends it that project's events from its
// project.created on, as every feed did before a project could be private (R9), however many
// projects were made before the reader was resolved again.
func sightMoved(held, next model.Actor, event model.Event) bool {
	if seesEveryObject(held) && seesEveryObject(next) {
		return false
	}
	was, now := held.Viewer, next.Viewer
	if was.Open != now.Open {
		return true
	}
	if sameSet(was.Sees, now.Sees) {
		return false
	}
	if event.Type == projectCreatedEvent && !slices.Contains(was.Sees, event.Project) {
		return !sameSet(append(slices.Clone(was.Sees), event.Project), now.Sees)
	}
	return true
}

// sameSet reports whether a and b hold the same strings, each holding a string once: the projects
// two viewers see, or the projects and the scopes two resolutions of one credential grant
// (sameCredentialGrant).
func sameSet(a, b []string) bool {
	if slices.Equal(a, b) {
		return true
	}
	return len(a) == len(b) && slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// eventReplay is what one events subscription is replayed before its live events, or after an
// overflow (resumeAfterOverflow): the events to send, whether they are everything the page lacks
// (complete), the id its events_start names (start), and the id at or below which the live loop
// skips an event (after).
type eventReplay struct {
	events   []relayedEvent
	complete bool
	start    int64
	after    int64
}

// resolveEventReplay answers a subscription whose relay subscription began at the cursor the relay
// had published through (published), with the log's newest id as it stood just after (head).
// Every event at or below published is final, so the rows a read takes now are the rows any later
// read would take, and the replay hands over to the live feed at published with no gap.
//
// A subscription that names no since asked for no backlog: it is replayed nothing, starts at
// published and is not complete. One that waited out a reset of the log (replacedFrom at or above
// zero) is replayed (replacedFrom, published] all the same: the new generation's cursor was taken
// under a horizon proven after the reset, so an event of the new log can stand at or below it.
//
// A resume at or below published is replayed (since, published] when that is at most maxLiveReplay
// events of at most maxLiveReplayBytes of encodings, and is then complete; past either bound it is
// replayed nothing and its page refreshes what it shows (replay_complete false is its signal).
// Rows are counted, not ids subtracted: ids have gaps. behind is that count when the caller's own
// read already took it (eventSubscriptionCheck), and below zero when it did not. A resume whose gap
// holds an access change (accessChanged) is replayed nothing and is not complete either: what its
// page read before the change can be of a project its caller no longer sees, or lack one it now
// sees, which only the refresh replay_complete false asks for reads again.
//
// A resume above published is either ahead of this process's relay or from a replaced log. When
// since is at or below head, the relay has not yet published what the page already holds (another
// process served it, or the newest commit is not yet read): nothing is replayed, the replay is
// complete, and the live loop skips what the relay publishes through since. When since is above
// head, the log was replaced under the page and the page, which only ever raises its cursor and
// never lowers it, resumes from an id of the old log: it is replayed nothing, is not complete and
// starts at published, so the new log's events reach it rather than sitting below a since that
// will never be passed.
//
// What is replayed is what reader may read (replayThrough).
func (s *server) resolveEventReplay(ctx context.Context, reader model.Actor, since *int64, published, head, replacedFrom int64, behind int, accessChanged bool) (eventReplay, error) {
	if since == nil {
		if replacedFrom < 0 {
			return eventReplay{start: published, after: published}, nil
		}
		return s.replayThrough(ctx, reader, min(replacedFrom, published), published, false, -1)
	}
	resume := *since
	if resume > published {
		if resume <= head {
			return eventReplay{complete: true, start: resume, after: resume}, nil
		}
		return eventReplay{start: published, after: published}, nil
	}
	if accessChanged {
		return eventReplay{start: published, after: published}, nil
	}
	return s.replayThrough(ctx, reader, resume, published, true, behind)
}

// replayThrough is the bounded replay of (from, published]: every event there that reader may read
// (eventReadableSQL), each encoded once, when the events there are at most maxLiveReplay and the
// encodings replayed total at most maxLiveReplayBytes, and complete says so in the events_start.
// Past either bound nothing is replayed and the subscription starts at published, not complete, so
// its page refreshes what it shows. behind is how many events stand there, counted up to
// maxLiveReplay+1, or below zero when the caller has not counted them, and it counts them itself.
func (s *server) replayThrough(ctx context.Context, reader model.Actor, from, published int64, complete bool, behind int) (eventReplay, error) {
	if behind < 0 {
		if err := s.deps.Store.Pool.QueryRow(ctx, `
			select count(*) from (select 1 from events e where e.id > $1 and e.id <= $2 and `+eventGapSQL("e", "$4")+` order by e.id limit $3) as behind
		`, from, published, maxLiveReplay+1, refs.VisibilityArg(reader)).Scan(&behind); err != nil {
			return eventReplay{}, err
		}
	}
	if behind > maxLiveReplay {
		return eventReplay{start: published, after: published}, nil
	}
	missed, err := s.readEventsAfterID(ctx, reader, from, published, maxLiveReplay)
	if err != nil {
		return eventReplay{}, err
	}
	replayed := make([]relayedEvent, 0, len(missed))
	size := 0
	for _, event := range missed {
		encoded, err := encodeLiveEvent(event)
		if err != nil {
			return eventReplay{}, err
		}
		size += len(encoded)
		if size > maxLiveReplayBytes {
			return eventReplay{start: published, after: published}, nil
		}
		replayed = append(replayed, relayedEvent{Event: event, Encoded: encoded})
	}
	return eventReplay{events: replayed, complete: complete, start: from, after: published}, nil
}

// resumeAfterOverflow answers a subscription the relay dropped, holding every event through after:
// a new relay subscription and what it missed, (after, published] of that new subscription,
// resolved as a resume from after (resolveEventReplay) for caller resolved again, as feedEvents
// resolves it before its own check, and the reader the live loop goes on with. The new
// subscription is made before the caller cancels the dropped one, so the generation runs on
// between them. false is a catch-up that is not complete (past the replay's bounds, across an
// access change, or from a log replaced meanwhile) or that failed; the subscription then ends,
// and its page subscribes again from the last id it applied.
func (s *server) resumeAfterOverflow(ctx context.Context, caller model.Actor, after int64) (relaySubscription, eventReplay, eventsReader, bool) {
	next, err := s.eventRelay.subscribe(ctx, liveEventsBuffer)
	if err != nil {
		return relaySubscription{}, eventReplay{}, eventsReader{}, false
	}
	reader, err := s.readerFor(ctx, caller)
	if err != nil {
		next.cancel()
		return relaySubscription{}, eventReplay{}, eventsReader{}, false
	}
	head, cursorAt, behind, accessChanged, err := eventSubscriptionCheck(ctx, s.deps.Store.Pool, reader, next.head, &after)
	if err != nil || events.EventLogWasReset(next.head, next.headAt, head, cursorAt) {
		next.cancel()
		return relaySubscription{}, eventReplay{}, eventsReader{}, false
	}
	caughtUp, err := s.resolveEventReplay(ctx, reader, &after, next.head, head, -1, behind, accessChanged)
	if err != nil || !caughtUp.complete {
		next.cancel()
		return relaySubscription{}, eventReplay{}, eventsReader{}, false
	}
	return next, caughtUp, eventsReader{reader: reader, through: head}, true
}

// eventSubscriptionCheck is events.EventCursorCheck for an events subscription whose relay
// subscription began at cursor, and in the same statement how many events a resume from since
// stands behind it: the rows of (since, cursor] in the gap of reader's page (eventGapSQL), counted
// up to maxLiveReplay+1, which is all the bound needs (replayThrough), and whether an access
// change that moved what reader sees stands among them (accessChanged, sightMovedSQL). For a
// reader that sees every object (seesEveryObject) that is a project made public alone
// (madePublicSQL): no other access change moves what such a reader sees, so its resume across a
// write of a group or of a list is replayed and complete, as every resume was before a project
// could be private (R9). behind is -1 when there is nothing to count: no since, or a since above
// the cursor, which resolveEventReplay answers without a replay read. One statement, so a resume
// waits for the shared pool once before its replay read rather than twice: on a restart every
// page resumes at once, behind every request queued for a connection.
func eventSubscriptionCheck(ctx context.Context, q queryer, reader model.Actor, cursor int64, since *int64) (maxID int64, cursorAt *time.Time, behind int, accessChanged bool, err error) {
	moved := sightMovedSQL("e", "$4")
	if seesEveryObject(reader) {
		moved = madePublicSQL("e")
	}
	if err := q.QueryRow(ctx, `
		select coalesce((select max(id) from events), 0), (select created_at from events where id = $1),
			case when $2::bigint is null or $2::bigint > $1 then -1 else count(*)::int end,
			coalesce(bool_or(missed.moved), false)
		from (select `+moved+` as moved from events e
			where e.id > $2::bigint and e.id <= $1 and `+eventGapSQL("e", "$4")+` order by e.id limit $3) as missed
	`, cursor, since, maxLiveReplay+1, refs.VisibilityArg(reader)).Scan(&maxID, &cursorAt, &behind, &accessChanged); err != nil {
		return 0, nil, 0, false, err
	}
	return maxID, cursorAt, behind, accessChanged, nil
}

// eventGapSQL passes the events row eventAlias names when it belongs to the gap of a resume read
// for the viewer bound at viewerParam: an event that viewer may read (eventReadableSQL), which a
// complete replay sends, and a change of a project's access that took the project from the viewer
// (lostSightSQL), which the viewer no longer reads once it is made. Every other event is one the
// page never held and would not be sent, so it is no gap: a resume across a project the caller
// never saw is complete. A write of a group is read by every open viewer, a full member's
// credential whatever it lists, and by no restricted member, whom no group admits to a project
// (store.PersonSeesProjectSQL), so a group write a viewer may not read moves nothing it sees.
func eventGapSQL(eventAlias, viewerParam string) string {
	return `(` + eventReadableSQL(eventAlias, viewerParam) + ` or ` + lostSightSQL(eventAlias, viewerParam) + `)`
}

// sightMovedSQL is whether the event eventAlias names, one of the gap's (eventGapSQL), moved what
// the viewer bound at viewerParam sees, so the page owes the refresh replay_complete false asks for:
// a change of a project's access that took the project from the viewer (lostSightSQL) or gave it
// the project (gainedSightSQL), and a change of a group the viewer reads, whose payload names no
// member and so may have moved anyone's. Every other access change in the gap is one the viewer
// reads and that moved nothing it sees: another person added to a list it is on, its project turned
// private while it stays on the list, a project's Slack channels.
func sightMovedSQL(eventAlias, viewerParam string) string {
	e := eventAlias
	return `(` + e + `.type = '` + accessGroupChangedEvent + `' or ` + lostSightSQL(e, viewerParam) + ` or ` + gainedSightSQL(e, viewerParam) + `)`
}

// madePublicSQL is sightMovedSQL for a viewer that sees every object (seesEveryObject): whether the
// event eventAlias names made a project public, which that viewer's page, read while the project
// was private, may lack. It reads the server's record of the event, as lostSightSQL does.
func madePublicSQL(eventAlias string) string {
	e := eventAlias
	return `(` + e + `.type = '` + projectAccessChangedEvent + `' and ` + e + `.payload->>'change' = '` + model.AccessVisibilityChanged + `' and exists (
		select 1 from project_access_changes recorded where recorded.event_id = ` + e + `.id and recorded.visibility = '` + model.ProjectPublic + `'
		offset 0))`
}

// lostSightSQL is whether the event eventAlias names is a change of the access of a project the
// viewer bound at viewerParam does not see now, that took the project from it:
//
//   - the removal of the viewer's own person from the project's list;
//   - the removal from the list of a group that holds the viewer's person as a full member, which
//     the person saw the project through: a group admits only its full members
//     (store.PersonSeesProjectSQL), so its removal takes nothing from a restricted member in it;
//   - an owner change that took the project from the viewer's person (its principal, SetOwner);
//   - the project turned private, to a viewer that read it while it was public: an open viewer,
//     which reads every public project, and a credential whose own list names it (Viewer.Lists).
//
// Who a change named, and the visibility it left, are the server's own record of the event
// (project_access_changes, access.recordProjectChanges), never the payload, which names the project
// and the change alone.
func lostSightSQL(eventAlias, viewerParam string) string {
	e, person := eventAlias, access.ViewerPerson(viewerParam)
	return `(` + e + `.type = '` + projectAccessChangedEvent + `' and not ` + access.ProjectClause(e+`.project_key`, viewerParam) + ` and exists (
		select 1 from project_access_changes recorded where recorded.event_id = ` + e + `.id and (
			(` + e + `.payload->>'change' in ('` + model.AccessPersonRemoved + `', '` + model.AccessOwnerChanged + `')
				and recorded.principal = ` + person + `)
			or (` + e + `.payload->>'change' = '` + model.AccessGroupRemoved + `' and exists (
				select 1 from access_group_members lost
				join people full_member on full_member.email = lost.person and full_member.member_kind = 'full'
				where lost.group_name = recorded.principal and lost.person = ` + person + `
				offset 0))
			or (` + e + `.payload->>'change' = '` + model.AccessVisibilityChanged + `' and recorded.visibility = '` + model.ProjectPrivate + `'
				and (` + access.OpenClause(viewerParam) + ` or ` + access.ListsClause(e+`.project_key`, viewerParam) + `)))
		offset 0))`
}

// gainedSightSQL is whether the event eventAlias names is a change of the access of a project the
// viewer bound at viewerParam sees now, that may have given the viewer the project: its own person
// added to the project's list, or made its owner; a group added to the list that holds the viewer's
// person; or the project turned public, to a viewer that reads every public project it may (an
// open viewer, or a credential whose list names it). It reads the server's record of the event, as
// lostSightSQL does.
func gainedSightSQL(eventAlias, viewerParam string) string {
	e, person := eventAlias, access.ViewerPerson(viewerParam)
	return `(` + e + `.type = '` + projectAccessChangedEvent + `' and ` + access.ProjectClause(e+`.project_key`, viewerParam) + ` and exists (
		select 1 from project_access_changes recorded where recorded.event_id = ` + e + `.id and (
			(` + e + `.payload->>'change' = '` + model.AccessPersonAdded + `' and recorded.principal = ` + person + `)
			or (` + e + `.payload->>'change' = '` + model.AccessOwnerChanged + `' and recorded.owner = ` + person + `)
			or (` + e + `.payload->>'change' = '` + model.AccessGroupAdded + `' and exists (
				select 1 from access_group_members gained where gained.group_name = recorded.principal and gained.person = ` + person + `
				offset 0))
			or (` + e + `.payload->>'change' = '` + model.AccessVisibilityChanged + `' and recorded.visibility = '` + model.ProjectPublic + `'
				and (` + access.OpenClause(viewerParam) + ` or ` + access.ListsClause(e+`.project_key`, viewerParam) + `)))
		offset 0))`
}

// endEventsAfterFailure ends a subscription whose read failed, with an ended frame (behind), so its
// page subscribes again from the last id it applied. It waits the relay's own retry first: a page
// answers the end of a feed that had started by subscribing again at once, and a read that keeps
// failing (a database that does not answer) would otherwise have the page and the subscription take
// turns as fast as the failure comes back. An end that comes before the feed's events_start, as
// every end does while the head cannot be read, the page answers after its own doubling backoff. A
// subscription whose own context ended (stopped, or its socket closed) sends nothing: that is why
// its read failed.
func endEventsAfterFailure(ctx context.Context, stream liveStreamer, sub, what string, err error) {
	if ctx.Err() != nil {
		return
	}
	slog.Warn("dispatch: an events subscription could not "+what+"; it ends, and its page subscribes again", "error", err)
	if !waitFor(ctx, eventRelayRetry) {
		return
	}
	_ = stream.SendWait(ctx, liveEnded(sub, liveEndedBehind))
}

// liveEventsStart is the frame opening sub, a subscription that starts after the event since;
// replayComplete says its replay covers every event the page missed.
func liveEventsStart(sub string, since int64, replayComplete bool) LiveEventsStart {
	return LiveEventsStart{Type: "events_start", Sub: sub, Since: strconv.FormatInt(since, 10), ReplayComplete: replayComplete}
}

// liveEventFrame is event's frame on sub: its decimal id and its own JSON encoding, byte for byte,
// which is what a page reads every event as. The relay encodes each live event once for every
// subscriber and a replay encodes each missed event once, so a frame sent live and the same frame
// replayed carry the same bytes.
func liveEventFrame(sub string, event relayedEvent) LiveEventFrame {
	return LiveEventFrame{
		Type: "event", Sub: sub, ID: strconv.FormatInt(event.Event.ID, 10), Event: event.Encoded,
	}
}

// encodeLiveEvent is one event's own JSON encoding, which a frame carries whole. Its trees ride
// in the envelope's own `trees` map, always: a browser cannot set Dispatch-Trees on a socket, and
// a page reads every event it is handed the same way whether it arrived here or through a REST
// read.
//
// An encoding builds its trees and copies none: it is one event on its way to the sockets
// watching now, with no request behind it to take a connection from, and the text it carries was
// written a moment ago, so the table is the slower of the two ways to the same bytes. The build
// leaves them in this process's cache for the reads that follow. It runs here, where the event is
// encoded, rather than in the frame: the relay encodes a live event once for every subscriber, so
// the trees of one event are built once however many sockets are watching.
func encodeLiveEvent(event model.Event) ([]byte, error) {
	event.Trees = eventDisplayTrees(event)
	return json.Marshal(event)
}
