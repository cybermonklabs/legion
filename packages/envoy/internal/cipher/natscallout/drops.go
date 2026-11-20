package natscallout

import (
	"sync/atomic"
	"time"
)

// The reasons a request is dropped unanswered, the reason label of
// broker_nats_callout_dropped_total. The hub refuses the client of each at its own authorization
// timeout.
const (
	// dropPastBounds: the request reached a replica already holding maxWaiting requests waiting
	// for a decision slot and maxRefusing being refused (deliver).
	dropPastBounds = iota
	// dropHubGaveUp: the hub had already given up on the request when the replica read it, by the
	// hub's own clock (hubClock).
	dropHubGaveUp
	// dropUnopened: the request did not open with the service's curve key.
	dropUnopened
	// dropUnreadable: the opened request is no authorization request JWT.
	dropUnreadable
	// dropPendingLimit: nats.go dropped the request on arrival, past the subscription's pending
	// limits (start), before deliver saw it; the counter reads these from Subscription.Dropped.
	dropPendingLimit
	dropReasons
)

// dropLabels are the reason labels, by reason.
var dropLabels = [dropReasons]string{"past_bounds", "hub_gave_up", "unopened", "not_a_request", "pending_limit"}

// pastBoundsDetail is the first-drop detail of a drop past both bounds.
const pastBoundsDetail = "past both bounds of this replica"

// drops is the lock-free hand-off of the requests the service drops unanswered, from deliver and
// answer to the counter: each drop is an atomic add, and each reason's first detail a
// compare-and-swap, so a drop takes neither a lock nor a write. The counter (countDrops) moves them
// into broker_nats_callout_dropped_total and into the service's dropped tally, which the reporter
// logs as it logs refusals and failures (report). Its counts move at the counter's next tick; a
// detail noted after the counter took its reason's count waits for that reason's next count, and
// is not logged if none comes before Close.
type drops struct {
	// pending counts each reason's drops since the counter last ran.
	pending [dropReasons]atomic.Int64
	// first is the detail of each reason's first drop since the counter last took one.
	first [dropReasons]atomic.Pointer[string]
	// subDropped is Subscription.Dropped as countDrops last read it; only countDrops reads or
	// writes it.
	subDropped int
}

// add counts one drop for reason, and reports whether the drop's detail is wanted: whether none
// waits for the counter yet for that reason. A caller builds the detail, and gives it to note, only
// then, so a flood of drops builds one string a reason a tick rather than one a drop, save for
// drops at once: each of them can be told yes, and only the first note is kept.
func (d *drops) add(reason int) bool {
	d.pending[reason].Add(1)
	return d.first[reason].Load() == nil
}

// note keeps detail as reason's first drop's, unless one is kept already.
func (d *drops) note(reason int, detail string) {
	d.first[reason].CompareAndSwap(nil, &detail)
}

// countDrops reads nats.go's own count of the requests it dropped past the subscription's pending
// limits, and moves every drop handed off since it last ran into broker_nats_callout_dropped_total
// and the dropped tally. It writes no log line, so the metric moves while the log is stalled. The
// counter runs it every tick (countWhileRunning), and Close once before it unsubscribes and once
// after the last request is answered; Close waits for the counter to end first, so it never runs
// twice at once. A drop counted in one tick whose detail lands after it carries that detail with
// the reason's next count.
func (s *Service) countDrops() {
	d := s.answer.drops
	if dropped, err := s.sub.Dropped(); err == nil && dropped > d.subDropped {
		d.pending[dropPendingLimit].Add(int64(dropped - d.subDropped))
		d.note(dropPendingLimit, "past the subscription's pending limits, dropped by nats.go on arrival")
		d.subDropped = dropped
	}
	for reason := range dropReasons {
		n := d.pending[reason].Swap(0)
		s.answer.deps.Metrics.dropped(dropLabels[reason], n)
		if n == 0 {
			continue
		}
		detail := ""
		if first := d.first[reason].Swap(nil); first != nil {
			detail = *first
		}
		s.answer.dropped.addCount(dropLabels[reason], n, detail)
	}
}

// countWhileRunning is the counter: every tick it runs countDrops, until Close. It is a goroutine
// of its own because neither the reporter, whose log write can block, nor the heartbeat, which
// waits for a decision slot, can keep the metric moving through what it would report.
func (s *Service) countWhileRunning() {
	defer close(s.counted)
	ticker := time.NewTicker(s.settings.tick)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.countDrops()
		}
	}
}
