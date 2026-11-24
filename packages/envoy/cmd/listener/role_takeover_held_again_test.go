package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// A loses a role to B, so a takeover record of A is written and its pass moves the record's
// sequence on. A takes the role back and reads three role messages while it holds it, and leaves a
// fourth unread. C then takes the role from A. A pass of the first record, which waited while A held
// the role, starts again from A's floor when A loses the role, as C's own record does, so C receives
// the fourth message and none of the three A read. The fourth is the control: it shows the watch
// sees the copies the passes make.
func TestATakeoverRecordWhoseHolderTookTheRoleBackCopiesOnlyWhatItDidNotRead(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, c, role = "ses_held_again_a", "ses_held_again_b", "ses_held_again_c", "held-again"
	answerA := func(frame relayedFrame) []string {
		if strings.Contains(frame.item.PayloadSummary, "unread") {
			return []string{relayReplyReceived}
		}
		return answerDone(frame)
	}
	first := rig.relayed(a, answerA)
	rig.claim(rig.mux, a, role)
	agent := contracts.LegacyAgentSubject(a)
	before := rig.onLane(role, "before B")
	rig.awaitStored(agent, before.EventID, "the forward to A before B took the role")
	crashAwaitEvents(t, first, []string{before.EventID}, nil, "A reads the message sent before B took the role")
	awaitAcked(t, rig.consumers, a, rig.seqOn(agent, before.EventID))

	rig.gone(b)
	rig.claim(rig.away, b, role)
	// The claim's handover runs in a goroutine of its own, so the control waits for its pass's
	// write rather than for the role's guard, which that goroutine may not hold yet.
	waitFor(t, crashWatch, "a takeover record of "+a+" that moved its sequence after "+b+" took the role", func() bool {
		for _, record := range rig.records(role, a) {
			if record.Next != 0 {
				return true
			}
		}
		return false
	})
	rig.idle(rig.cfg(), role)

	rig.claim(rig.mux, a, role)
	// The handover of A's claim runs a pass of the record while A holds the role, which sets the
	// record back to reading A's floor anew.
	waitFor(t, crashWatch, "the takeover record of "+a+" set back while "+a+" holds the role", func() bool {
		for _, record := range rig.records(role, a) {
			if record.Next != 0 {
				return false
			}
		}
		return true
	})
	rig.idle(rig.cfg(), role)

	read := make([]string, 0, 3)
	for i := range 3 {
		item := rig.onLane(role, fmt.Sprintf("read by A %d", i))
		rig.awaitStored(agent, item.EventID, "a forward to A after it took the role back")
		read = append(read, item.EventID)
	}
	crashAwaitEvents(t, first, read, nil, "A reads the three it receives after it took the role back")
	awaitAcked(t, rig.consumers, a, rig.seqOn(agent, read[len(read)-1]))
	unread := rig.onLane(role, "unread by A")
	rig.awaitStored(agent, unread.EventID, "the forward A leaves unread")
	crashAwaitEvents(t, first, []string{unread.EventID}, nil, "A receives the fourth and does not read it")

	next := rig.relayed(c, answerDone)
	rig.claim(rig.mux, c, role)
	got := crashAwaitEvents(t, next, []string{unread.EventID}, nil, "C, handed the message A did not read")
	got = append(got, crashDrain(next, 5*time.Second)...)
	var replayed []string
	for _, frame := range got {
		if slices.Contains(read, frame.item.EventID) {
			replayed = append(replayed, frame.item.EventID)
		}
	}
	if len(replayed) > 0 {
		t.Fatalf("C received %d role messages A had read while it held the role: %v (A read %v)", len(replayed), replayed, read)
	}
}
