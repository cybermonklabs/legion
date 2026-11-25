package main

import (
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// A takeover record whose claim never landed waits while its previous holder A keeps the role, and
// A reads three role messages. Two hours later on the listener's clock, the kept lane's sweep having
// passed every fifteen minutes, A's consumer is gone with no resume record and C takes the role.
// With A's floor unknown, a record copies from the hour before its base time, and a record that
// found A holding the role again counts that hour from when it last found it
// (bus.TakeoverRecord.HolderSeenAt), not from its own claim two hours back: none of the three A read
// is copied. The control is the same steps with no record of a claim that never landed, which leaves
// the kept lane empty too, so a copy in the case comes from the waiting record alone.
func TestATakeoverRecordThatWaitedWhileItsHolderKeptTheRoleCopiesNoOlderHour(t *testing.T) {
	t.Parallel()
	for _, waiting := range []bool{false, true} {
		name := "control: no record of a claim that never landed"
		if waiting {
			name = "a record of a claim that never landed copies none of what the holder read two hours before it lost the role"
		}
		t.Run(name, func(t *testing.T) {
			rig := newCrashRig(t)
			const a, b, c, role = "ses_dormant_a", "ses_dormant_b", "ses_dormant_c", "dormant"
			keeper := rig.relayed(a, answerDone)
			rig.claim(rig.mux, a, role)
			agent := contracts.LegacyAgentSubject(a)
			if waiting {
				rig.gone(b)
				setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{beforeClaimTry: func(r, previous string) error {
					if r == role && previous == a {
						return errors.New("the listener stopped after the takeover record and before the claim write")
					}
					return nil
				}})
				if recorder := rig.claimThrough(rig.mux, b, role, ""); recorder.Code == http.StatusOK {
					t.Fatalf("%s's claim answered 200; want it stopped before its write", b)
				}
				setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{})
				if holder := rig.holder(role); holder != a {
					t.Fatalf("role %s is held by %q after %s's stopped claim; want %s", role, holder, b, a)
				}
				if len(rig.records(role, a)) == 0 {
					t.Fatalf("control: no takeover record of %s from %s after the claim that never landed", role, a)
				}
			}
			read := rig.sendTo(a, role, 3, "read by A")
			crashAwaitEvents(t, keeper, read, nil, "A reads its three role messages")
			awaitAcked(t, rig.consumers, a, rig.seqOn(agent, read[len(read)-1]))
			for range 8 {
				rig.clock.advance(15 * time.Minute)
				rig.sweep(rig.cfg(), role)
				if kept := rig.kept(role); len(kept) != 0 {
					t.Fatalf("role %s's kept lane holds %v while %s holds the role; want nothing", role, kept, a)
				}
			}

			keeper.close()
			awaitNoRelaySubscriber(t, rig.url, a)
			consumer, _ := bus.SessionConsumerName(a)
			if err := rig.harness.client.JS().DeleteConsumer(bus.Stream, consumer); err != nil {
				t.Fatalf("delete %s's consumer: %v", a, err)
			}
			waitFor(t, 10*time.Second, a+"'s consumer gone", func() bool {
				_, err := rig.harness.client.JS().ConsumerInfo(bus.Stream, consumer)
				return err != nil
			})
			rig.relayed(c, answerDone)
			rig.claim(rig.mux, c, role)
			rig.idle(rig.cfg(), role)
			for range 3 {
				rig.sweep(rig.cfg(), role)
			}

			var copied []string
			for _, event := range rig.kept(role) {
				if slices.Contains(read, event) {
					copied = append(copied, event)
				}
			}
			if len(copied) != 0 {
				for _, record := range rig.records(role, a) {
					t.Logf("record %s claimed_at=%s next=%d finished=%v", record.subject,
						time.UnixMilli(record.ClaimedAt).UTC().Format(time.RFC3339), record.Next, record.Finished)
				}
				t.Fatalf("the takeover copied %d role messages %s read two hours before %s took the role: %v", len(copied), a, c, copied)
			}
		})
	}
}
