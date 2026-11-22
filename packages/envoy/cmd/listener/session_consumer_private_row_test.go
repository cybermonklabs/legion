package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// A relay restart filters a session's consumer on its stored interest row less every topic that
// reaches a private stream subject that is not the session's own (sessionDelivery.storedRowFilter),
// the rule a registration applies (store.MergedTopics). The row here is the session's own, under
// its key and naming its key, machine and time, as any writer of the interest bucket can store it,
// with another session's inbox in both forms and another project's controller notice lane beside
// the lane of the controller role the session holds. After the restart the consumer filters on the
// session's own agent subject and its own lane only, and no message to the other session or on the
// other lane reaches it: the session's own message, stored after those, is its first frame. The
// lane of the role it holds is the control in the same row, which the restart keeps, so the rule
// reads the role's claim and does not drop every lane. Without the rule the restart filters on the
// other session's inbox and hands its messages to this session.
func TestARelayRestartFiltersOnNoOtherSessionsInboxItsStoredRowNames(t *testing.T) {
	rig := newRelayRig(t)
	const (
		sessionID = "ses_row_forged_pub"
		victim    = "ses_row_forged_priv"
		role      = "legion-PUB-controller"
		lane      = "notifications.legion.PUB.controller"
		otherLane = "notifications.legion.PRIV.controller"
	)
	victimNewForm := "notifications.agent.PRIV." + victim
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	claimRoles(t, rig.harness.registry, sessionID, role)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1", lane))
	requireFilters(t, rig.sessionConsumerRig, sessionID, lane)

	row, _, err := rig.harness.registry.RefreshAt(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read %s's interest row: %v", sessionID, err)
	}
	row.Topics = append(row.Topics, contracts.LegacyAgentSubject(victim), victimNewForm, otherLane)
	value, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("encode %s's row: %v", sessionID, err)
	}
	interests, err := rig.harness.client.JS().KeyValue(bus.InterestBucket)
	if err != nil {
		t.Fatalf("open the interest bucket: %v", err)
	}
	if _, err := interests.Put(sessionID, value); err != nil {
		t.Fatalf("store %s's row naming %s's inbox: %v", sessionID, victim, err)
	}

	// A new epoch registered after the run's restart replaces the run; its restart reads the row
	// under the session's lock and writes the consumer's filter before it lets the lock go.
	unlock := rig.deps.consumers.locks.lock(sessionID)
	rig.relays.registered(sessionID, true, rig.deps.consumers.mark())
	awaitLockUsers(t, &rig.deps.consumers.locks, sessionID, 2)
	unlock()
	awaitLockUsers(t, &rig.deps.consumers.locks, sessionID, 0)

	requireFilters(t, rig.sessionConsumerRig, sessionID, lane)
	rig.storeEvent(t, contracts.LegacyAgentSubject(victim))
	rig.storeEvent(t, victimNewForm)
	rig.storeEvent(t, otherLane)
	mine, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), mine)
}
