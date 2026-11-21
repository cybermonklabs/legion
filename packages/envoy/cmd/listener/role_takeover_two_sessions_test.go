package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// The rows of TestATakeoverCopyThatStopsOrRunsAgainLosesNothing in which one role message reaches
// two sessions, each at least once, as R4 allows: Envoy delivers each message at least once, and a
// copy for the role's next holder does not take the frame from a previous holder that still reads
// it. Each row starts the same way: A holds the role through a listener that is gone, so no relay
// of A's reads its consumer, and is sent a role message it does not read; B takes the role and is
// handed the copy. What A's return does with the frame is the row.

// twoSessionStart is that start: the rig, the role message's event id, and B's stand-in once B has
// received the copy.
func twoSessionStart(t *testing.T, row string) (*crashRig, string, *sessionStandIn) {
	t.Helper()
	rig := newCrashRig(t)
	a, b, role := "ses_cr"+row+"_a", "ses_cr"+row+"_b", "cr"+row
	rig.gone(a)
	rig.claim(rig.away, a, role)
	sent := rig.sendTo(a, role, 1, "for "+a+", which does not read it")
	next := rig.relayed(b, answerDone)
	rig.claim(rig.mux, b, role)
	crashAwaitEvents(t, next, sent, nil, b+", handed the copy of what "+a+" did not read")
	return rig, sent[0], next
}

// twoSessionReceived counts the frames of eventID standIn receives within quiet of the last.
func twoSessionReceived(standIn *sessionStandIn, eventID string, earlier []relayedFrame) int {
	count := 0
	for _, frame := range append(earlier, crashDrain(standIn, 2*time.Second)...) {
		if frame.item.EventID == eventID {
			count++
		}
	}
	return count
}

// (14) A takes the role back from B where its listener is gone, before any relay of A's read its
// consumer, and then comes back: its relay finds the frame A never read, and the claim names A, so
// A receives it. The role message reaches B (the copy) and A (the frame), each once (R4). The
// control is A coming back while B still holds the role, and taking it back after: its relay
// copies the frame, a duplicate of B's copy, and acknowledges it, so A does not receive it and B
// alone holds it.
func crashTakenBackBeforeItsRelayRan(t *testing.T) {
	for _, row := range []struct {
		name           string
		suffix         string
		claimBackFirst bool
		wantA          int
	}{
		{"case: the role taken back before A's relay ran: A and B each receive it once", "14", true, 1},
		{"control: A back while B holds the role, then the role taken back: B alone receives it", "14c", false, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig, event, toB := twoSessionStart(t, row.suffix)
			a, role := "ses_cr"+row.suffix+"_a", "cr"+row.suffix
			if row.claimBackFirst {
				rig.claim(rig.away, a, role)
			}
			back := startSessionStandIn(t, rig.url, a, answerDone)
			rig.register(rig.mux, a, "epoch-2")
			var earlier []relayedFrame
			if row.claimBackFirst {
				earlier = crashAwaitEvents(t, back, []string{event}, nil, a+", which took the role back before its relay ran")
			} else {
				awaitAcked(t, rig.consumers, a, rig.seqOn(contracts.LegacyAgentSubject(a), event))
				rig.claim(rig.mux, a, role)
			}
			if got := twoSessionReceived(back, event, earlier); got != row.wantA {
				t.Fatalf("%s received %s %d times, want %d", a, event, got, row.wantA)
			}
			if got := twoSessionReceived(toB, event, nil); got != 0 {
				t.Fatalf("B received %s %d more times after the copy, want none", event, got)
			}
		})
	}
}

// (15) A comes back on a listener of the release before this change, whose relay sends every frame
// whoever holds its role: the row stands in for that relay as it was, pulling A's session consumer
// and taking and acknowledging each frame (bus.SessionConsumers.Pull), with no code of this
// release's relay. A receives the frame though B holds the role, and B has the copy, so the role
// message reaches both, each once (R4; a roll that runs both releases at once). The control is the
// same return on this release's relay, which copies the frame, a duplicate, and acknowledges it, so
// A does not receive it.
func crashRelayOfTheReleaseBefore(t *testing.T) {
	t.Run("case: A's relay of the release before sends the frame: A and B each receive it once", func(t *testing.T) {
		rig, event, toB := twoSessionStart(t, "15")
		const a = "ses_cr15_a"
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		pull, err := rig.consumers.Pull(ctx, fixedConsumerName(t, a))
		if err != nil {
			t.Fatalf("pull %s's session consumer as the release before's relay does: %v", a, err)
		}
		defer pull.Stop()
		got := 0
		for got == 0 {
			message, err := pull.Next(ctx)
			if err != nil {
				t.Fatalf("%s's consumer handed no frame of %s to the release before's relay: %v", a, event, err)
			}
			var item contracts.Envelope
			if json.Unmarshal(message.Data, &item) == nil && item.EventID == event {
				got++
			}
			if err := message.Ack(); err != nil {
				t.Fatalf("acknowledge %s's frame: %v", a, err)
			}
		}
		awaitAcked(t, rig.consumers, a, rig.seqOn(contracts.LegacyAgentSubject(a), event))
		if got := twoSessionReceived(toB, event, nil); got != 0 {
			t.Fatalf("B received %s %d more times after the copy, want none", event, got)
		}
	})
	t.Run("control: A's relay of this release copies the frame and A does not receive it", func(t *testing.T) {
		rig, event, toB := twoSessionStart(t, "15c")
		const a = "ses_cr15c_a"
		back := startSessionStandIn(t, rig.url, a, answerDone)
		rig.register(rig.mux, a, "epoch-2")
		awaitAcked(t, rig.consumers, a, rig.seqOn(contracts.LegacyAgentSubject(a), event))
		if got := twoSessionReceived(back, event, nil); got != 0 {
			t.Fatalf("%s received %s %d times, want none", a, event, got)
		}
		if got := twoSessionReceived(toB, event, nil); got != 0 {
			t.Fatalf("B received %s %d more times after the copy, want none", event, got)
		}
	})
}

// takeBack is rows 16 and 17's run: A takes the role and is sent two role-lane messages; it reads
// the first (done) and only receives the second, and stops. B takes the role and reads none of the
// copy it is handed, and stops; A comes back while B holds the role, so its relay copies its frame
// and acknowledges it, and then A takes the role back. It answers the two messages, the frame A
// received of the second before it stopped, and every frame A receives after it took the role
// back. The take-back's copy is of B's frame, which the role lane's walk forwards to A, the session
// the lane's own forward of that message went to before.
func takeBack(t *testing.T, suffix string) (read, unread contracts.Envelope, before relayedFrame, after []relayedFrame) {
	t.Helper()
	rig := newCrashRig(t)
	a, b, role := "ses_cr"+suffix+"_a", "ses_cr"+suffix+"_b", "cr"+suffix
	frames := 0
	first := rig.relayed(a, func(frame relayedFrame) []string {
		frames++
		if frames == 1 {
			return answerDone(frame)
		}
		return crashAnswerReceived(frame)
	})
	rig.claim(rig.mux, a, role)
	agent := contracts.LegacyAgentSubject(a)
	read = rig.onLane(role, "on the role lane, read by A")
	crashAwaitEvents(t, first, []string{read.EventID}, nil, a+"'s first frame, read")
	unread = rig.onLane(role, "on the role lane, received by A and not read")
	got := crashAwaitEvents(t, first, []string{unread.EventID}, nil, a+"'s second frame, answered received alone")
	for _, frame := range got {
		if frame.item.EventID == unread.EventID {
			before = frame
		}
	}
	first.conn.Close()
	awaitNoRelaySubscriber(t, rig.url, a)
	rig.gone(b)
	rig.claim(rig.away, b, role)
	rig.awaitStored(contracts.LegacyAgentSubject(b), unread.EventID, "the walk's forward of the copy to "+b)
	back := startSessionStandIn(t, rig.url, a, answerDone)
	rig.register(rig.mux, a, "epoch-2")
	awaitAcked(t, rig.consumers, a, rig.seqOn(agent, unread.EventID))
	rig.claim(rig.mux, a, role)
	return read, unread, before, crashDrain(back, 3*time.Second)
}

// (16) The take-back: the copy of the frame B did not read reaches A. Its dedupe key is its own
// (takenFrameDedupeKey), so the walk - this release's, and the release before's, which reads
// nothing of a copy but its dedupe key - forwards it under an id no earlier forward took, where the
// role lane's own forward of that message to A would have the stream drop it. The control is the
// message A read before it stopped, which is not copied and does not come back to A.
func crashTakeBackReachesThePreviousHolder(t *testing.T) {
	read, unread, _, after := takeBack(t, "16")
	counts := map[string]int{}
	for _, frame := range after {
		counts[frame.item.EventID]++
	}
	if counts[unread.EventID] < 1 {
		t.Fatalf("A, which took the role back, received %s %d times, want at least once: %v", unread.EventID, counts[unread.EventID], counts)
	}
	if counts[read.EventID] != 0 {
		t.Fatalf("control: A received %s, which it read before it stopped, %d more times, want none", read.EventID, counts[read.EventID])
	}
}

// (17) A, which received the message before it stopped, receives it again in the take-back (R4: a
// host can be handed a message twice). The copy carries the message's own event id and a dedupe
// key of its own, so a host that already showed the message drops the copy by its event id
// (packages/envoy-client delivery.ts, createDeliveryDedupe) and does not act on it twice. The
// control is the frame A received first: the role lane's forward, under the message's own key.
func crashTakeBackCopyIsTheSameEvent(t *testing.T) {
	_, unread, before, after := takeBack(t, "17")
	if strings.TrimPrefix(before.item.DedupeKey, contracts.RoleForwardDedupePrefix) != unread.DedupeKey || before.item.EventID != unread.EventID {
		t.Fatalf("control: the frame A received first = %s under %q, want %s under the message's own key %q",
			before.item.EventID, before.item.DedupeKey, unread.EventID, unread.DedupeKey)
	}
	var copies []relayedFrame
	for _, frame := range after {
		if frame.item.EventID == unread.EventID {
			copies = append(copies, frame)
		}
	}
	if len(copies) == 0 {
		t.Fatalf("A received no copy of %s after it took the role back", unread.EventID)
	}
	for _, frame := range copies {
		copied := frame.item
		copied.DedupeKey = strings.TrimPrefix(copied.DedupeKey, contracts.RoleForwardDedupePrefix)
		if !strings.HasPrefix(copied.DedupeKey, takenFrameKeyPrefix) || copied.DedupeKey == unread.DedupeKey || !contracts.DedupeKeyNamesTheUpstreamEvent(copied) {
			t.Fatalf("the copy A received carries dedupe key %q, want a minted key of its own, apart from the message's %q", frame.item.DedupeKey, unread.DedupeKey)
		}
	}
}

// (18) This release's walk hands the new holder each copy once: B takes the role, is handed the
// copy of A's frame, and reads it; A then comes back while B holds the role, so its relay copies
// the same frame again, which the stream stores once (takenFrameMsgID), and the sweep runs. B
// receives the message once, not twice, and the role's kept lane holds one copy of it.
func crashCopyReachesTheNewHolderOnce(t *testing.T) {
	rig, event, toB := twoSessionStart(t, "18")
	a, role := "ses_cr18_a", "cr18"
	back := startSessionStandIn(t, rig.url, a, answerDone)
	rig.register(rig.mux, a, "epoch-2")
	awaitAcked(t, rig.consumers, a, rig.seqOn(contracts.LegacyAgentSubject(a), event))
	rig.sweep(rig.cfg(), role)
	if got := twoSessionReceived(toB, event, nil); got != 0 {
		t.Fatalf("B received %s %d more times after the copy, want none", event, got)
	}
	if got := twoSessionReceived(back, event, nil); got != 0 {
		t.Fatalf("A received %s %d times while B holds the role, want none", event, got)
	}
	crashRequireOneEach(t, "role "+role+"'s kept lane", rig.kept(role), []string{event})
}
