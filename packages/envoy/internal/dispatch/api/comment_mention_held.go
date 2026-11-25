package api

import (
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/metrics"
)

// heldWait is what the sends of a held lane wait behind: the session the lane's head send last
// waited to reach and the earlier event it found that session must take first.
type heldWait struct {
	session string
	by      model.MentionCopyHolder
}

// warnHeld logs the one WARN of send, held behind wait since send.heldSince, as of now: its owner,
// the earlier event it waits behind and that event's last publish error, beside the ERROR the
// outbox writes for that event's own failed publish, which is the cause.
func warnHeld(send mentionSend, wait heldWait, now time.Time) {
	slog.Warn("dispatch: a comment mention's send waits behind an earlier event of its owner",
		"owner", send.owner(), "comment", send.comment.ID, "target", send.mention.Target, "session", wait.session,
		"event_id", wait.by.EventID, "publish_error", deref(wait.by.PublishError),
		"held_for", now.Sub(send.heldSince).Round(time.Second))
}

// reportLate logs the WARN of item, a send that has just left lane, a held lane, with no wait
// left, when it had waited there heldWarnAfter and no WARN was logged for it: its wait passed the
// threshold while it was in flight, or while it waited behind a send whose try was in flight,
// which no park saw. Every send held past the threshold is so reported once, however its wait
// ends. It names what the lane last waited behind, or, when the lane has no wait recorded, what
// item itself last waited behind.
func (m *mentionSender) reportLate(lane laneKey, item mentionSend) {
	now := m.sendClock().Now()
	if item.heldSince.IsZero() || item.warned || now.Sub(item.heldSince) < m.heldWarnAfter {
		return
	}
	m.mu.Lock()
	wait := m.reportedWaitLocked(lane, item)
	m.mu.Unlock()
	warnHeld(item, wait, now)
}

// reportedWaitLocked is what a late WARN of item, leaving lane, names (reportLate): what the lane
// last waited behind, or, when the lane has no wait recorded, what item itself last waited behind.
func (m *mentionSender) reportedWaitLocked(lane laneKey, item mentionSend) heldWait {
	if wait, ok := m.waits[lane]; ok {
		return wait
	}
	return heldWait{session: item.heldFor, by: item.heldBy}
}

// heldSamples is how many sends wait on a held lane behind an earlier event of their owner - the
// one that waited and every send of its owner and recipient queued behind it - and how long the
// oldest has, as of now: what the held gauges read (registerHeldGauges). It names no owner: the
// gauges are on /metrics, which answers whoever reaches the server's port, and an owner's key is
// neither bounded nor public; each send's WARN names its owner (warnHeld). A held lane's head is
// out of the count for the moment a try of it is in flight.
func (m *mentionSender) heldSamples(now time.Time) (count int64, oldest time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for lane := range m.waits {
		for _, send := range m.lanes[lane] {
			count++
			oldest = max(oldest, now.Sub(send.heldSince))
		}
	}
	return count, oldest
}

// registerHeldGauges adds to registry the two gauges of the sends this server's mention sender
// holds behind an earlier event, with no label: envoy_dispatch_mention_sends_held, how many, and
// envoy_dispatch_mention_send_held_oldest_seconds, how long the oldest has waited, in whole
// seconds. They read the sender register built, so a test that replaces it registers none
// (withMentionClaimRefresh) - nor does a nil registry.
func (s *server) registerHeldGauges(registry *metrics.Registry) {
	if registry == nil {
		return
	}
	sender := s.mentions
	registry.NewGaugeFunc("envoy_dispatch_mention_sends_held",
		"Comment mention sends that wait behind an earlier event of their owner the outbox still owes a send their session may take",
		func() int64 { count, _ := sender.heldSamples(sender.sendClock().Now()); return count })
	registry.NewGaugeFunc("envoy_dispatch_mention_send_held_oldest_seconds",
		"How long the oldest comment mention send has waited behind an earlier event of its owner, in whole seconds",
		func() int64 {
			_, oldest := sender.heldSamples(sender.sendClock().Now())
			return int64(oldest / time.Second)
		})
}
