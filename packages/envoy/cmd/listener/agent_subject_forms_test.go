package main

import (
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// A session's agent subject has two forms while every reader takes both (dispatch://INFRA-3862):
// the session alone, and the session under a project (`_` for none). The listener's durable path
// reads the session a stored agent message is for (durableNeedsDelivery, agentSubjectDelivery),
// and must read it from either form: a reader that cut the prefix off the new form would look up a
// session named INFRA.s1, which none is. It would find no record, wait once and then report the
// wake of a session that does not exist, although s1's own consumer delivers the message.
func TestAnAgentSubjectNamesItsSessionInEitherForm(t *testing.T) {
	for subject, want := range map[string]string{
		"notifications.agent.s1":       "s1",
		"notifications.agent._.s1":     "s1",
		"notifications.agent.INFRA.s1": "s1",
		"notifications.agent.ses-a-1":  "ses-a-1",
	} {
		if got := agentSubjectSession(subject); got != want {
			t.Errorf("agentSubjectSession(%q) = %q, want %q", subject, got, want)
		}
	}
}

// The agent path reads the session from either form of an agent subject at each place it reads
// one: the durable's check of its cached record and interest row (durableNeedsDelivery), and the
// delivery's reads of the row and the record, the outcome it takes from them
// (session.HandleAgentMessage) and the wake report it stores (agentSubjectDelivery). Under a
// project and under `_`, a session with a record is acknowledged as a skip and wakes nothing. A
// session that another listener's row names is that listener's, and wakes nothing here. A session
// with no record is woken by its own name, not as <PROJECT>.<session>. The core handler delivers
// with no metadata, so the session with no record is woken on its first delivery.
func TestTheAgentPathReadsTheSessionOfEitherForm(t *testing.T) {
	for _, project := range []string{"INFRA", contracts.NoProject} {
		t.Run(project, func(t *testing.T) {
			harness := newListenerDeliveryHarness(t)
			wakes := agentWakeProbe(t, harness)
			suffix := id.New()
			recorded, elsewhere, absent := "ses_forms_recorded_"+suffix, "ses_forms_elsewhere_"+suffix, "ses_forms_absent_"+suffix
			if err := harness.sessions.Put(recorded, session.SessionEntry{
				SelfSubscribed: true, MachineID: "test-machine", Delivery: session.DeliveryConsumer, Epoch: "e",
			}); err != nil {
				t.Fatalf("register %s: %v", recorded, err)
			}
			if _, err := harness.registry.Upsert(store.Interest{SessionID: elsewhere, MachineID: "other-machine"},
				[]string{contracts.LegacyAgentSubject(elsewhere)}); err != nil {
				t.Fatalf("register %s through another listener: %v", elsewhere, err)
			}

			for _, tc := range []struct {
				sessionID string
				needed    bool
				skipped   int
			}{
				{recorded, false, 1},
				{elsewhere, false, 0},
				{absent, true, 0},
			} {
				subject := contracts.AgentSubject(project, tc.sessionID)
				if needed, skipped := durableNeedsDelivery(harness.config, subject, 1); needed != tc.needed || skipped != tc.skipped {
					t.Errorf("durableNeedsDelivery(%s) = %v, %d; want %v, %d", subject, needed, skipped, tc.needed, tc.skipped)
				}
				harness.coreHandler(&natsgo.Msg{Data: marshalListenerEnvelope(t, listenerTestEnvelope(subject, "forms-"+tc.sessionID))})
			}

			body := metricsBody(t, harness)
			if got := counterValue(t, body, deliveredSeries("skipped")); got != 1 {
				t.Errorf("%s = %d, want 1: only the message to the session with a record is a skip\n%s", deliveredSeries("skipped"), got, body)
			}
			if got := counterValue(t, body, deliveredSeries("wake_reported")); got != 1 {
				t.Errorf("%s = %d, want 1: only the session with no record is woken\n%s", deliveredSeries("wake_reported"), got, body)
			}
			if reports := deliveryRecords(t, harness.logs.String(), "listener agent wake reported"); len(reports) != 1 || reports[0].SessionID != absent {
				t.Errorf("wake lines %+v, want one naming %s", reports, absent)
			}
			awaitWake(t, wakes, wakeKindAgent, absent)
		})
	}
}
