package events

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestStreamDropsASubscriberThatCannotKeepUp is the overflow contract every reader of a Stream
// rests on: a subscriber whose channel is full is dropped and its channel closed, rather than
// waited for. An events subscription ends with an ended frame when its channel closes, and the
// page subscribes again from the last id it applied, which replays what it missed. Blocking
// instead would hold the event stream relay -- one goroutine for the whole process -- behind the
// slowest reader of any open page, and silently skipping the event would leave that page wrong
// with no resubscribe to repair it.
func TestStreamDropsASubscriberThatCannotKeepUp(t *testing.T) {
	const buffer = 8
	stream := NewStream[model.Event]()
	subscription, cancel := stream.Subscribe(buffer)
	defer cancel()

	for sequence := 1; sequence <= buffer+1; sequence++ {
		stream.Publish(model.Event{Seq: sequence})
	}
	if count := stream.SubscriberCount(); count != 0 {
		t.Fatalf("subscriber count after overflow = %d, want 0", count)
	}
	// A closed subscription gives the zero value, which is none of the published ones.
	for sequence := 1; sequence <= buffer; sequence++ {
		event := testwait.Receive(t, "the overflowed subscription's buffered events, in order", subscription)
		if event.Seq != sequence {
			t.Fatalf("event %d = %#v, want the buffered events in order (a zero event is a closed subscription)", sequence, event)
		}
	}
	if event := testwait.Receive(t, "the overflowed subscription's close", subscription); event.Seq != 0 {
		t.Fatalf("overflowed subscriber remained open: it received %#v after its buffered events", event)
	}
}

// TestStreamBuffersAsManyValuesAsItsSubscriberAsked: each subscriber chooses how far behind it may
// fall, so a slow consumer with a large buffer survives a burst that drops one with a small buffer.
func TestStreamBuffersAsManyValuesAsItsSubscriberAsked(t *testing.T) {
	stream := NewStream[int]()
	small, cancelSmall := stream.Subscribe(1)
	defer cancelSmall()
	large, cancelLarge := stream.Subscribe(4)
	defer cancelLarge()

	for value := 1; value <= 4; value++ {
		stream.Publish(value)
	}
	if count := stream.SubscriberCount(); count != 1 {
		t.Fatalf("subscriber count after a burst of 4 = %d, want the 4-slot subscriber alone", count)
	}
	// A closed subscription gives 0, which is none of the published values.
	if value := testwait.Receive(t, "the 1-slot subscription's one buffered value", small); value != 1 {
		t.Fatalf("1-slot subscriber received %d, want 1 before it was dropped (0 is a closed subscription)", value)
	}
	if value := testwait.Receive(t, "the 1-slot subscription's close", small); value != 0 {
		t.Fatalf("1-slot subscriber remained open after the burst overflowed it: it received %d", value)
	}
	for want := 1; want <= 4; want++ {
		if value := testwait.Receive(t, "the 4-slot subscription's buffered values, in order", large); value != want {
			t.Fatalf("4-slot subscriber received %d, want %d", value, want)
		}
	}
}

// TestStreamCloseAllEndsEverySubscription: the event stream relay ends a generation's
// subscriptions this way when the event log is reset under its cursor, so each page subscribes
// again against the log as it now stands.
func TestStreamCloseAllEndsEverySubscription(t *testing.T) {
	stream := NewStream[model.Event]()
	first, cancelFirst := stream.Subscribe(1)
	defer cancelFirst()
	second, cancelSecond := stream.Subscribe(1)
	defer cancelSecond()
	if count := stream.SubscriberCount(); count != 2 {
		t.Fatalf("subscriber count = %d, want 2", count)
	}

	stream.CloseAll()

	for name, subscription := range map[string]<-chan model.Event{"first": first, "second": second} {
		if event := testwait.Receive(t, "the "+name+" subscription's close by CloseAll", subscription); event.Seq != 0 {
			t.Fatalf("%s subscription stayed open after CloseAll: it received %#v", name, event)
		}
	}
	if count := stream.SubscriberCount(); count != 0 {
		t.Fatalf("subscriber count after CloseAll = %d, want 0", count)
	}
}
