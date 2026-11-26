package api

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// gatedStream is a liveStreamer whose SendWait waits for its gate to open, as a socket's does for
// room while its page reads nothing, and then hands each frame to frames.
type gatedStream struct {
	gate   chan struct{}
	frames chan any
}

func newGatedStream() *gatedStream {
	return &gatedStream{gate: make(chan struct{}), frames: make(chan any, 4096)}
}

func (stream *gatedStream) SendWait(ctx context.Context, frame any) error {
	select {
	case <-stream.gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case stream.frames <- frame:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (stream *gatedStream) Holds(string) bool { return false }

// next is the next frame the subscription sent.
func (stream *gatedStream) next(t *testing.T, what string) any {
	t.Helper()
	return testwait.Receive(t, what, stream.frames)
}

// feedNext is the next frame an events subscription on database's server sent: a frame of the
// event feed, which stands behind that server's event horizon, so the wait is the horizon's.
func (stream *gatedStream) feedNext(t *testing.T, database *store.Store, what string) any {
	t.Helper()
	return horizontest.WaitInProcess(t, what, stream.frames, database)
}

// stopOnCleanup registers the stop of running on t's cleanup and returns it: the subscription's
// cancel, then the shared wait for its goroutine to return, whose text names the subscription by
// kind ("the events subscription's goroutine to return once stopped"). A row that ends the
// subscription itself calls what this returns; the cleanup's stop then finds it returned.
func stopOnCleanup(t *testing.T, kind string, running liveSubscription) (stop func()) {
	t.Helper()
	stop = func() {
		testwait.Returns(t, "the "+kind+" subscription's goroutine to return once stopped", running.stop)
	}
	t.Cleanup(stop)
	return stop
}

// A subscription the relay drops, because it fell more than the relay holds for one subscriber
// behind, is caught up from the event log when what it missed is within the replay's bounds: it
// sends what its channel still held and what it missed, each event once and in id order, and goes
// on live on the subscription it made instead. The subscription here waits for room as a socket's
// does while its page reads nothing, so it drains nothing of what the relay publishes into it.
func TestARelayDropWithinTheBoundsCatchesTheSubscriptionUp(t *testing.T) {
	t.Parallel()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	issue := createPlainIssue(t, handler, "TEST")
	ctx, err := store.ForConcurrentRead(store.WithTransactionTracking(t.Context()))
	if err != nil {
		t.Fatalf("mark the subscription's context for the pool: %v", err)
	}
	stream := newGatedStream()
	running := served.startEventsSubscription(ctx, stream, "feed", new(int64(0)))
	stopOnCleanup(t, "events", running)
	relay := served.eventRelay
	horizontest.PollInProcess(t, "the subscription subscribes to the relay", database, func() bool { return relaySubscribers(relay) == 1 })

	// More events in one commit than the relay holds for one subscriber, and fewer than the
	// replay's bound, so the catch-up covers the whole of what the drop lost.
	head := seedEvents(t, database, issue, liveEventsBuffer+10, 0)
	if head > maxLiveReplay {
		t.Fatalf("the backlog of %d events is past the replay's bound of %d", head, maxLiveReplay)
	}
	horizontest.PollInProcess(t, "the relay drops the subscription that fell behind", database, func() bool {
		return relaySubscribers(relay) == 0
	})

	// Once there is room again the subscription sends what it held and what it caught up on.
	close(stream.gate)
	if start, ok := stream.feedNext(t, database, "feed's events_start").(LiveEventsStart); !ok || start.Sub != "feed" || start.Since != "0" {
		t.Fatalf("first frame = %+v, want feed's events_start from 0", start)
	}
	for last := int64(0); last < head; {
		frame, ok := stream.feedNext(t, database, "feed's events").(LiveEventFrame)
		if !ok {
			t.Fatalf("frame = %+v, want feed's next event rather than its end: what it missed is within the replay's bounds", frame)
		}
		id := mustEventFrameID(t, frame)
		if frame.Sub != "feed" || frame.ID != strconv.FormatInt(id, 10) || id <= last {
			t.Fatalf("event frame %+v after %d, want feed's next", frame, last)
		}
		last = id
	}

	// And it goes on live on the subscription it made instead of the dropped one.
	renameIssue(t, handler, issue, "Renamed after the drop")
	frame, ok := stream.feedNext(t, database, "the rename").(LiveEventFrame)
	if !ok || !strings.Contains(string(frame.Event), "Renamed after the drop") {
		t.Fatalf("the caught-up subscription was sent %+v, want the rename committed after the drop", frame)
	}
}

// A subscription the relay drops that missed more than the replay's bound is not caught up: it
// sends what its channel still held and ends with an ended frame (behind) rather than missing
// events in silence, and the page's resubscribe from the last id it applied is served what
// commits next.
func TestARelayDropPastTheBoundsEndsTheSubscriptionAsBehind(t *testing.T) {
	t.Parallel()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	issue := createPlainIssue(t, handler, "TEST")
	ctx, err := store.ForConcurrentRead(store.WithTransactionTracking(t.Context()))
	if err != nil {
		t.Fatalf("mark the subscription's context for the pool: %v", err)
	}
	stream := newGatedStream()
	running := served.startEventsSubscription(ctx, stream, "feed", new(int64(0)))
	stopOnCleanup(t, "events", running)
	relay := served.eventRelay
	horizontest.PollInProcess(t, "the subscription subscribes to the relay", database, func() bool { return relaySubscribers(relay) == 1 })

	// More events in one commit than the relay holds for one subscriber, and past the replay's
	// bound, so nothing can catch the subscription up.
	seedEvents(t, database, issue, liveEventsBuffer+maxLiveReplay+2, 0)
	horizontest.PollInProcess(t, "the relay drops the subscription that fell behind", database, func() bool {
		return relaySubscribers(relay) == 0
	})

	close(stream.gate)
	if start, ok := stream.feedNext(t, database, "feed's events_start").(LiveEventsStart); !ok || start.Sub != "feed" || start.Since != "0" {
		t.Fatalf("first frame = %+v, want feed's events_start from 0", start)
	}
	var last int64
	for ended := false; !ended; {
		switch frame := stream.feedNext(t, database, "the end of feed").(type) {
		case LiveEventFrame:
			id := mustEventFrameID(t, frame)
			if frame.Sub != "feed" || frame.ID != strconv.FormatInt(id, 10) || id <= last {
				t.Fatalf("event frame %+v after %d, want feed's next", frame, last)
			}
			last = id
		case LiveEnded:
			if frame.Sub != "feed" || frame.Reason != "behind" {
				t.Fatalf("ended = %+v, want feed's, behind", frame)
			}
			ended = true
		default:
			t.Fatalf("frame = %+v, want feed's events and then its end", frame)
		}
	}
	if head := eventHead(t, database); last >= head {
		t.Fatalf("the subscription sent through %d before it ended, want less than the head %d it could not catch up to", last, head)
	}
	testwait.Receive(t, "the dropped subscription's goroutine to return after its end", running.(*runningSubscription).done)

	// The page's resubscribe, from the last id it applied, resolves its position - the backlog it
	// missed is past the replay's bound, so it starts at the head and refreshes what it shows
	// (replay_complete false) - and is served what commits next.
	again := newGatedStream()
	close(again.gate)
	resubscribed := served.startEventsSubscription(ctx, again, "feed", new(last))
	stopOnCleanup(t, "resubscribed events", resubscribed)
	start, ok := again.feedNext(t, database, "the resubscribe's events_start").(LiveEventsStart)
	if since, err := strconv.ParseInt(start.Since, 10, 64); !ok || err != nil || since < last || start.ReplayComplete {
		t.Fatalf("the resubscribe's first frame = %+v, want its events_start at or above %d, not complete", start, last)
	}
	renameIssue(t, handler, issue, "Renamed after the drop")
	frame, ok := again.feedNext(t, database, "the rename").(LiveEventFrame)
	if !ok || !strings.Contains(string(frame.Event), "Renamed after the drop") {
		t.Fatalf("the resubscribe was sent %+v, want the rename committed after the drop", frame)
	}
}

// A subscription whose read fails (here, a database that no longer answers) ends with an ended
// frame (behind), so its page subscribes again, rather than going silent; and it waits the relay's
// retry on the server's clock first, so a read that keeps failing is asked again at that pace
// rather than as fast as the page can resubscribe. Before it waits it warns that it could not read,
// and the row requires that warning: it carries the read's error, and the page is told only that
// the feed is behind, so it is the server's one record of why the feed ended. The row is not
// parallel, because it makes its recorder the default logger's (logtest.SetDefault), and every
// test of the process logs through the default logger.
func TestAnEventsSubscriptionWhoseReadFailsEndsAsBehind(t *testing.T) {
	fake := clocktest.New(time.Now())
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{clock: fake})
	testwait.Returns(t, "every connection of the server's pool to come back, so the pool closes", database.Pool.Close)
	ctx, err := store.ForConcurrentRead(store.WithTransactionTracking(t.Context()))
	if err != nil {
		t.Fatalf("mark the subscription's context for the pool: %v", err)
	}
	records := &logtest.Recorder{}
	logtest.SetDefault(t, records)
	stream := newGatedStream()
	close(stream.gate)
	retry := fake.Now().Add(eventRelayRetry)
	running := served.startEventsSubscription(ctx, stream, "feed", nil)
	stopOnCleanup(t, "events", running)
	fake.AwaitArmedAt(t, "the failed subscription's retry", retry)
	if queued := len(stream.frames); queued != 0 {
		t.Fatalf("the subscription whose read failed sent %d frames before its retry", queued)
	}
	// The subscription logs its warning before it arms the retry (endEventsAfterFailure), so with
	// the retry armed the record is already kept, and this read waits for nothing.
	if !slices.ContainsFunc(records.Records(), func(record slog.Record) bool {
		return strings.HasPrefix(record.Message, "dispatch: an events subscription could not ")
	}) {
		t.Fatal("the subscription whose read failed logged no warning that it could not read")
	}
	fake.Set(retry)
	ended, ok := stream.next(t, "feed's end").(LiveEnded)
	if !ok || ended.Sub != "feed" || ended.Reason != "behind" {
		t.Fatalf("the first frame of a subscription whose read failed = %+v, want its end, behind", ended)
	}
}

// An event that commits between a subscription's relay subscription and its catch-up read is in
// both: the catch-up sends it (it reads every id above the subscription's since, whatever the head
// was), and the relay publishes it into the subscription's channel. The live loop sends it once,
// because it holds the ids catch-up sent. Here the subscription waits for room at its events_start
// frame, which is after it subscribed to the relay and read the head and before its catch-up, so
// the commit lands exactly in that window.
func TestAnEventCommittedDuringTheCatchUpIsSentOnce(t *testing.T) {
	t.Parallel()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	issue := createPlainIssue(t, handler, "TEST")
	ctx, err := store.ForConcurrentRead(store.WithTransactionTracking(t.Context()))
	if err != nil {
		t.Fatalf("mark the subscription's context for the pool: %v", err)
	}
	stream := newGatedStream()
	running := served.startEventsSubscription(ctx, stream, "feed", new(int64(0)))
	stopOnCleanup(t, "events", running)
	horizontest.PollInProcess(t, "the subscription subscribes to the relay", database, func() bool {
		return relaySubscribers(served.eventRelay) == 1
	})

	renameIssue(t, handler, issue, "Renamed during the catch-up")
	head := eventHead(t, database)
	close(stream.gate)
	if start, ok := stream.feedNext(t, database, "feed's events_start").(LiveEventsStart); !ok || start.Since != "0" {
		t.Fatalf("first frame = %+v, want feed's events_start from 0", start)
	}
	seen := map[int64]bool{}
	for last := int64(0); last < head; {
		frame, ok := stream.feedNext(t, database, "feed's events").(LiveEventFrame)
		if !ok {
			t.Fatalf("frame = %+v, want feed's next event", frame)
		}
		id := mustEventFrameID(t, frame)
		if seen[id] {
			t.Fatalf("event %d was sent twice: the commit during the catch-up reached the page on both paths", id)
		}
		seen[id] = true
		last = max(last, id)
	}
	// Nothing of what it already sent follows: the relay publishes into the subscription in id
	// order, so a resend of an event the catch-up sent would reach the page ahead of the next
	// commit's.
	renameIssue(t, handler, issue, "Renamed after the catch-up")
	frame, ok := stream.feedNext(t, database, "the commit after the catch-up").(LiveEventFrame)
	if !ok {
		t.Fatalf("frame = %+v, want the event of the commit after the catch-up", frame)
	}
	if id := mustEventFrameID(t, frame); id <= head {
		t.Fatalf("the subscription sent event %d after the catch-up reached the head %d", id, head)
	}
}
