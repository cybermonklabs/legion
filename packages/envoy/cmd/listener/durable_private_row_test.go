package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// The durable path over a stored row naming another session's private subjects: the session's own
// row, under its key and naming its key, machine and time, as any writer of the interest bucket
// can store it, with another session's inbox and another project's controller notice lane. A
// message on each, stored on the stream and handed to the durable as its copy, reaches the session
// on no path: the durable resolves an agent subject by the session it names
// (agentSubjectDelivery), and the fan-out (fanoutDelivery), which does match the stored row's lane
// for the session, hands a session nothing itself, since a session receives only what its own
// consumer holds, and that consumer's filter is not the row's (storedRowFilter). The instrument's
// control is in the same row: the fan-out's skip of the session for the lane is counted, so the
// durable did read the stored row; and the session's own message, stored after the others, is its
// first frame, so its relay is live.
func TestADurableMessageOnAPrivateSubjectAStoredRowNamesReachesNoOtherSession(t *testing.T) {
	t.Parallel()
	rig := newRelayRig(t)
	const (
		sessionID = "ses_durable_forged_pub"
		victim    = "ses_durable_forged_priv"
		otherLane = "notifications.legion.PRIV.controller"
	)
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))

	row, _, err := rig.harness.registry.RefreshAt(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read %s's interest row: %v", sessionID, err)
	}
	row.MachineID = rig.harness.config.machineID
	row.Topics = append(row.Topics, contracts.LegacyAgentSubject(victim), otherLane)
	value, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("encode %s's row: %v", sessionID, err)
	}
	interests, err := rig.harness.client.JS().KeyValue(bus.InterestBucket)
	if err != nil {
		t.Fatalf("open the interest bucket: %v", err)
	}
	if _, err := interests.Put(sessionID, value); err != nil {
		t.Fatalf("store %s's row naming %s's inbox and %s: %v", sessionID, victim, otherLane, err)
	}
	waitFor(t, "the registry's copy to hold the stored row", func() bool {
		cached, ok := rig.harness.registry.Cached(sessionID)
		return ok && strings.Contains(strings.Join(cached.Topics, " "), otherLane)
	})

	skipped := func() int {
		t.Helper()
		const series = `test_messages_delivered{delivery_status="skipped"} `
		body := deliveredMetrics(t, rig.harness)
		at := strings.Index(body, series)
		if at < 0 {
			return 0
		}
		var n int
		if _, err := fmt.Sscan(body[at+len(series):], &n); err != nil {
			t.Fatalf("read the skipped count from %q: %v", body[at:], err)
		}
		return n
	}
	before := skipped()
	for _, subject := range []string{contracts.LegacyAgentSubject(victim), otherLane} {
		item, _ := rig.storeEvent(t, subject)
		rig.harness.handler(&natsgo.Msg{Subject: subject, Data: marshalListenerEnvelope(t, item)})
	}
	if got := skipped(); got != before+1 {
		t.Fatalf("the fan-out skipped %d deliveries for the messages, want 1, the lane's for %s: the durable did not read the stored row, so this row shows nothing", got-before, sessionID)
	}
	mine, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), mine)
	requireFilters(t, rig.sessionConsumerRig, sessionID)
}
