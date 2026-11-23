package main

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
)

// A lane whose forward slots are all held says so once, at ERROR, when its handler first has to
// wait for a slot, and once more, at ERROR, when at most half its slots are held again. NATS
// refuses every forward, so four slots fill with four held messages and the handler waits with a
// fifth; then forwards are stored again, the held messages end, the fifth takes a slot and ends,
// and the lane has room. Each line is written once for the spell, whatever the turnover of slots
// in it.
func TestAFullRoleLaneLogsOneErrorWhenItFillsAndOneWhenItHasRoom(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const (
		role  = "full-lane"
		limit = 4
	)
	if err := harness.sessions.Put("ses_full_lane", session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register holder: %v", err)
	}
	if _, err := harness.registry.SetRole("ses_full_lane", "test-machine", role, false); err != nil {
		t.Fatalf("claim role: %v", err)
	}
	var up atomic.Bool
	cfg := harness.config
	cfg.forwardRole = inlineForward(func(string, contracts.Envelope, string, time.Duration) error {
		if up.Load() {
			return nil
		}
		return errors.New("nats: connection closed")
	})
	forwarder := newRoleForwarderLimited(cfg, limit)
	cfg.roleForwards = forwarder
	handler := coreNATSDeliveryHandler(cfg)
	items := make([]contracts.Envelope, limit+1)
	for i := range items {
		items[i] = listenerTestEnvelope(contracts.RoleTopicPrefix+role, fmt.Sprintf("full-lane-%d", i))
		items[i].Payload = `{"type":"worker-queued"}`
	}
	for _, item := range items[:limit] {
		handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
	}
	waitFor(t, 20*time.Second, fmt.Sprintf("the %d messages held", limit), func() bool {
		return len(linesIn(harness.logs.String(), roleHeldLine, nil)) >= limit
	})
	if full := linesIn(harness.logs.String(), roleLaneFullLine, nil); len(full) != 0 {
		t.Fatalf("the lane said it was full with a slot still free: %v", full)
	}
	fifth := make(chan struct{})
	go func() {
		defer close(fifth)
		handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, items[limit])})
	}()
	waitFor(t, 20*time.Second, "the lane to say its slots are full", func() bool {
		return len(linesIn(harness.logs.String(), roleLaneFullLine, nil)) == 1
	})

	up.Store(true)
	<-fifth
	forwarder.settled(t)

	logs := harness.logs.String()
	full := linesIn(logs, roleLaneFullLine, nil)
	room := linesIn(logs, roleLaneRoomLine, nil)
	if len(full) != 1 || full[0]["level"] != "ERROR" || full[0]["slots"] != float64(limit) {
		t.Fatalf("full lines %v, want one at ERROR naming %d slots:\n%s", full, limit, logs)
	}
	if len(room) != 1 || room[0]["level"] != "ERROR" {
		t.Fatalf("room lines %v, want one at ERROR:\n%s", room, logs)
	}
	for _, item := range items {
		if got := countLogged(logs, "listener role forwarded", item.EventID); got != 1 {
			t.Fatalf("message %s was forwarded %d times, want once:\n%s", item.EventID, got, logs)
		}
	}
}
