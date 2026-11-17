package outbox

import (
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/metrics"
)

// heldCopies is what this process's fallback passes found of the comment_fallback_copies rows that
// wait behind an earlier event of their owner (fallbackWaitsForAnEarlierEvent): for each, its
// owner, when a pass first found it waiting, when one last did, and whether it was reported. Two
// gauges read it, by owner: how many copies wait (envoy_dispatch_outbox_fallback_copies_held) and
// how long the oldest has (envoy_dispatch_outbox_fallback_copy_held_oldest_seconds); and a copy
// that waits past the warn threshold is logged once at WARN, naming its owner, the event it waits
// behind and that event's last publish error, beside the ERROR the outbox writes for that event's
// own failed publish (backOff) - the cause, which is what an operator acts on.
//
// The wait itself has no bound - a bound would let the copy pass the event - so this is how it
// shows. A row leaves the record when a pass decides it otherwise (forget), and an entry no pass
// has found waiting for fallbackClaim - the hold moved to another process, whose passes read the
// row now - is dropped as the gauges read it, so a process that stopped publishing reports no
// copy it no longer reads. A nil record, a pass's that New did not make, records nothing.
type heldCopies struct {
	mu         sync.Mutex
	rows       map[heldCopyKey]*heldCopy
	registered sync.Once
}

// heldCopyKey names one comment_fallback_copies row: its comment and destination.
type heldCopyKey struct {
	comment, destination string
}

// heldCopy is one row's entry: its owner, when a pass first and last found it waiting behind an
// earlier event, and whether its WARN was logged.
type heldCopy struct {
	owner       string
	since, seen time.Time
	warned      bool
}

func newHeldCopies() *heldCopies {
	return &heldCopies{rows: map[heldCopyKey]*heldCopy{}}
}

// register adds the record's two gauges to registry, once; a nil registry adds none.
func (h *heldCopies) register(registry *metrics.Registry) {
	if h == nil || registry == nil {
		return
	}
	h.registered.Do(func() {
		registry.NewGaugeSetFunc("envoy_dispatch_outbox_fallback_copies_held",
			"Fallback copies of comment mentions that wait behind an earlier event of their owner the outbox still owes a send their session may take, by owner",
			func() []metrics.GaugeSample { count, _ := h.samples(time.Now()); return count })
		registry.NewGaugeSetFunc("envoy_dispatch_outbox_fallback_copy_held_oldest_seconds",
			"How long the oldest fallback copy of a comment mention has waited behind an earlier event of its owner, in whole seconds, by owner",
			func() []metrics.GaugeSample { _, oldest := h.samples(time.Now()); return oldest })
	})
}

// hold records that row, a copy of event's comment, waits behind the earlier event by, and logs
// its WARN once it has waited warnAfter (model.MentionCopyHeldWarnAfter when zero) since a pass
// first found it waiting.
func (h *heldCopies) hold(row fallbackCopy, event model.Event, by model.MentionCopyHolder, warnAfter time.Duration) {
	if h == nil {
		return
	}
	if warnAfter <= 0 {
		warnAfter = model.MentionCopyHeldWarnAfter
	}
	now := time.Now()
	key := heldCopyKey{comment: row.commentID, destination: row.destination}
	h.mu.Lock()
	entry := h.rows[key]
	if entry == nil {
		entry = &heldCopy{owner: eventOwner(event), since: now}
		h.rows[key] = entry
	}
	entry.seen = now
	warn := !entry.warned && now.Sub(entry.since) >= warnAfter
	entry.warned = entry.warned || warn
	owner, since := entry.owner, entry.since
	h.mu.Unlock()
	if warn {
		publishError := ""
		if by.PublishError != nil {
			publishError = *by.PublishError
		}
		slog.Warn("dispatch outbox: a fallback copy of a mention waits behind an earlier event of its owner",
			"owner", owner, "comment_id", row.commentID, "destination", row.destination,
			"event_id", by.EventID, "publish_error", publishError, "held_for", now.Sub(since).Round(time.Second))
	}
}

// forget drops row from the record: a pass decided it otherwise.
func (h *heldCopies) forget(row fallbackCopy) {
	if h == nil {
		return
	}
	h.mu.Lock()
	delete(h.rows, heldCopyKey{comment: row.commentID, destination: row.destination})
	h.mu.Unlock()
}

// samples is, by owner, how many copies wait and how long the oldest has, in whole seconds, as of
// now. An entry no pass has found waiting for fallbackClaim is dropped first.
func (h *heldCopies) samples(now time.Time) (count, oldest []metrics.GaugeSample) {
	if h == nil {
		return nil, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	counts := map[string]int64{}
	ages := map[string]time.Duration{}
	for key, entry := range h.rows {
		if now.Sub(entry.seen) > fallbackClaim {
			delete(h.rows, key)
			continue
		}
		counts[entry.owner]++
		ages[entry.owner] = max(ages[entry.owner], now.Sub(entry.since))
	}
	for owner, held := range counts {
		labels := [][2]string{{"owner", owner}}
		count = append(count, metrics.GaugeSample{Labels: labels, Value: held})
		oldest = append(oldest, metrics.GaugeSample{Labels: labels, Value: int64(ages[owner] / time.Second)})
	}
	return count, oldest
}

// eventOwner is the owner the outbox orders event among: its issue, its document or its project.
func eventOwner(event model.Event) string {
	switch {
	case event.IssueKey != nil:
		return *event.IssueKey
	case event.ArtifactID != nil:
		return *event.ArtifactID
	}
	return event.Project
}
