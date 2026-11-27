package main

import (
	"testing"
	"time"
)

// TestUnplaceableRetryWindowFitsTheBoundsItDependsOn pins the arithmetic the agent wait rests on.
// The cap on messages held back for the wait (unplaceableDeferralCap), the slots' expiry
// (unplaceableDeferralExpiry), the durable's MaxAckPending and AckWait (consumerMaxAckPending,
// consumerAckWait, durable.go), one message's handling limit (deliveryHandlingLimit) and the
// settle holds' caps (productionReceiveConfig) are declared in three files, and changing one of
// them alone breaks a property no delivery test would report: the durable's pending budget stops
// being bounded, or a slot is reclaimed while its message still waits, with no failure and no
// record anywhere.
func TestUnplaceableRetryWindowFitsTheBoundsItDependsOn(t *testing.T) {
	t.Parallel()
	// A message that waits is NAKed rather than ACKed, so it holds one of the durable's pending
	// slots for the whole wait instead of giving it back at once. The cap on how many this
	// listener holds back at a time is what keeps those holds from becoming every slot there is.
	// Twice the cap is also what one restart can leave pending at once: the new process starts with
	// an empty set while the old one's messages wait out their NAK delay (unplaceableDeferrals), so
	// this bound is what keeps that moment under MaxAckPending too.
	if unplaceableDeferralCap*2 >= consumerMaxAckPending {
		t.Fatalf("a cap of %d messages held back at once against the durable's MaxAckPending of %d leaves most of the pending budget to the wait: enough messages to sessions with no record would hold every slot at once and stall this machine's durable for every topic",
			unplaceableDeferralCap, consumerMaxAckPending)
	}
	// A message that waits stays pending for its NAK delay, the window, and for the durable's
	// AckWait when the NAK is lost on the wire, before the durable redelivers it. The redelivery
	// then waits in this pod's local queue, kept alive by InProgress, behind the handling of the
	// message in hand (deliveryHandlingLimit) and behind a settle hold after a take-over (either
	// cap) before the handler runs it and gives its slot back. An expiry no longer than all that
	// would reclaim the slot of a message the durable is still holding, and the set would count
	// fewer messages than this listener holds. The window is ENVOY_UNPLACEABLE_RETRY_WINDOW, so
	// the bound is held at every window the rows try, the default and a long one among them.
	receive := productionReceiveConfig()
	for _, window := range []time.Duration{0, 2 * time.Second, defaultUnplaceableRetryWindow, 10 * time.Minute} {
		pending := max(window, 0) + consumerAckWait + deliveryHandlingLimit + receive.settleCap + receive.handBackSettleCap
		if expiry := unplaceableDeferralExpiry(window); expiry <= pending {
			t.Fatalf("at a %s window a slot expires after %s, no later than the %s a waiting message can stay pending (its %s NAK delay plus the durable's %s AckWait, then its wait in the local queue behind a message whose handling may take %s and the %s and %s settle caps): a slot would be reclaimed while its message is still held, and the cap would stop bounding the durable's pending messages",
				window, expiry, pending, window, consumerAckWait, deliveryHandlingLimit, receive.settleCap, receive.handBackSettleCap)
		}
	}
}

// TestAnOrphanedDeferralSlotIsReclaimedAtItsExpiry pins the expiry that keeps the set of held-back
// agent messages from leaking. A slot leaves the set when its own message is delivered here again
// (agentSubjectDelivery's release, and jetStreamDeliveryHandler's before an early
// acknowledgement), and a message the stream drops or purges, or whose redelivery reaches another
// subscriber, never is: without an expiry each such slot is held for the life of the process, and
// unplaceableDeferralCap of them have every later message report its wake with no wait until a
// restart. The set is filled with such orphans on a stepped clock, read one nanosecond before
// their expiry - still held, so a slot whose message may yet come back is never reclaimed early -
// and read again at the expiry, where a new message takes a slot and the orphans are gone. A
// sequence whose own entry expired is taken afresh, with an expiry counted from the new take.
func TestAnOrphanedDeferralSlotIsReclaimedAtItsExpiry(t *testing.T) {
	t.Parallel()
	start := time.Unix(1_800_000_000, 0)
	clock := start
	set := newUnplaceableDeferrals(defaultUnplaceableRetryWindow)
	set.now = func() time.Time { return clock }
	expiry := unplaceableDeferralExpiry(defaultUnplaceableRetryWindow)
	for seq := uint64(1); seq <= unplaceableDeferralCap; seq++ {
		if !set.take(seq) {
			t.Fatalf("taking orphan slot %d of %d was refused", seq, unplaceableDeferralCap)
		}
	}
	fresh := uint64(unplaceableDeferralCap + 1)
	if set.take(fresh) {
		t.Fatalf("a new message took a slot from a set holding the cap of %d - the cap no longer bounds the held-back messages", unplaceableDeferralCap)
	}

	clock = start.Add(expiry - time.Nanosecond)
	if set.take(fresh) {
		t.Fatalf("a new message took a slot %s after the orphans were taken, one nanosecond before their %s expiry - a slot was reclaimed while its message could still come back",
			clock.Sub(start), expiry)
	}
	if got := set.size(); got != unplaceableDeferralCap {
		t.Fatalf("the set holds %d slots one nanosecond before the orphans' expiry, want all %d of them", got, unplaceableDeferralCap)
	}

	clock = start.Add(expiry)
	if !set.take(fresh) {
		t.Fatalf("a new message was refused a slot at the orphans' %s expiry - %d slots whose messages never came back still hold the set full, and no message waits until a restart",
			expiry, unplaceableDeferralCap)
	}
	if got := set.size(); got != 1 {
		t.Fatalf("the set holds %d slots after the orphans expired and one new message was taken, want only that message's", got)
	}

	// An orphan's own sequence, delivered again at last after its entry expired, is not held:
	// it takes a new slot whose expiry runs from this take, not from the one that expired.
	retaken := start.Add(expiry)
	if !set.take(1) {
		t.Fatal("an expired orphan's own sequence was refused a slot in a set holding one entry")
	}
	clock = retaken.Add(expiry - time.Nanosecond)
	if got := set.size(); got != 2 {
		t.Fatalf("the set holds %d slots one nanosecond before the re-taken sequence's fresh expiry, want 2 - the new message's and the re-taken one's", got)
	}
	clock = retaken.Add(expiry)
	if got := set.size(); got != 0 {
		t.Fatalf("the set holds %d slots at the fresh expiry of both, want none", got)
	}

	// The mark that an unanswered read was logged for a message lasts as a slot does: it is set
	// once and kept while the message keeps coming back, its expiry counted again from each
	// delivery, so the line is logged once per message, and it goes at its expiry like an orphaned
	// slot.
	first := clock
	if !set.firstUnread(7) {
		t.Fatal("the first unanswered read of a message was not the first")
	}
	clock = first.Add(expiry - time.Nanosecond)
	if set.firstUnread(7) {
		t.Fatal("a second unanswered read of the same message, inside the expiry, was taken as the first: the line would be logged again")
	}
	last := first.Add(expiry)
	clock = last
	if set.firstUnread(7) {
		t.Fatal("an unanswered read past the first read's expiry was taken as the first: the second read did not count the expiry again from its own time")
	}
	clock = last.Add(expiry)
	if !set.firstUnread(7) {
		t.Fatal("an unanswered read of a message whose mark expired was not taken as a first one")
	}
}
