package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// followedWakeKind is the kind of the wake report a message on a topic a released session follows
// raises, as the Legion daemon's decoder reads it.
const followedWakeKind = "followed_topic"

// heardWake is what a test reads of one wake report on bus.LegionWakeTopic.
type heardWake struct {
	Kind    string `json:"kind"`
	Session string `json:"session"`
	Topic   string `json:"topic"`
}

// wakingDurable is the rig's durable handler with the wake reporter main.go gives it, and a
// subscription to the wake reports that handler raises.
func (rig *sessionConsumerRig) wakingDurable(t *testing.T) (natsgo.MsgHandler, *natsgo.Subscription) {
	t.Helper()
	reports, err := rig.harness.client.Conn.SubscribeSync(bus.LegionWakeTopic)
	if err != nil {
		t.Fatalf("subscribe to the wake reports: %v", err)
	}
	t.Cleanup(func() { _ = reports.Unsubscribe() })
	if err := rig.harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the wake subscription: %v", err)
	}
	lifetime, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	cfg := rig.harness.config
	cfg.wakes = newWakeReporter(lifetime, rig.harness.client)
	return durableTestHandler(t, wireTestRoleForwarder(t, cfg)), reports
}

// handStored hands the message the stream stored at seq to the durable handler, as the listener's
// durable consumer delivers it.
func (rig *sessionConsumerRig) handStored(t *testing.T, handler natsgo.MsgHandler, seq uint64) {
	t.Helper()
	stored, err := rig.harness.client.JS().GetMsg(bus.Stream, seq)
	if err != nil {
		t.Fatalf("read the stored message %d: %v", seq, err)
	}
	handler(&natsgo.Msg{Subject: stored.Subject, Data: stored.Data})
}

// heardWakes reads every wake report that arrives on reports within window.
func heardWakes(t *testing.T, reports *natsgo.Subscription, window time.Duration) []heardWake {
	t.Helper()
	var heard []heardWake
	deadline := time.Now().Add(window)
	for {
		msg, err := reports.NextMsg(time.Until(deadline))
		if err != nil {
			return heard
		}
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err != nil {
			t.Fatalf("decode a wake report: %v", err)
		}
		var wake heardWake
		if err := json.Unmarshal([]byte(item.Payload), &wake); err != nil {
			t.Fatalf("decode a wake report's payload %s: %v", item.Payload, err)
		}
		heard = append(heard, wake)
	}
}

// followedWakesOf is the followed-topic wake reports among heard that name sessionID.
func followedWakesOf(heard []heardWake, sessionID string) []heardWake {
	var mine []heardWake
	for _, wake := range heard {
		if wake.Kind == followedWakeKind && wake.Session == sessionID {
			mine = append(mine, wake)
		}
	}
	return mine
}

// release is the daemon's clean stop of sessionID at an idle release, after which the session is
// gone and its row stands as it was.
func (rig *sessionConsumerRig) release(t *testing.T, sessionID string) {
	t.Helper()
	rig.deregister(t, sessionID, fmt.Sprintf("?if_not_registered_after=%d", time.Now().Add(time.Second).UnixMilli()))
	rig.awaitNoConsumer(t, sessionID)
	rig.deps.consumers.deletes.Wait()
}

// reapPastTheAllowance runs another listener's reaper pass one minute past the interest reaper's
// allowance after now, and waits until this listener follows what it did to sessionID's row.
func (rig *sessionConsumerRig) reapPastTheAllowance(t *testing.T, sessionID string) {
	t.Helper()
	reapAt := time.Now().Add(store.InterestReapAllowance(store.InterestReapGrace) + time.Minute)
	reaper, err := store.Open(t.Context(), rig.harness.client.Conn, store.WithReplicas(1), store.WithClock(func() time.Time { return reapAt }))
	if err != nil {
		t.Fatalf("open the reaping listener's registry: %v", err)
	}
	waitForRegistry(t, reaper)
	testwait.Eventually(t, "the reaping listener to hold "+sessionID+"'s row", func() bool {
		_, cached := reaper.Cached(sessionID)
		return cached
	})
	if _, err := reaper.Reap(func(id string) (bool, error) { return sessionMayBeLive(rig.harness.sessions, id) }, store.InterestReapGrace); err != nil {
		t.Fatalf("the reaper's pass: %v", err)
	}
	testwait.Eventually(t, "this listener to follow the reaper's pass over "+sessionID+"'s row", func() bool {
		row, cached := rig.harness.registry.Cached(sessionID)
		return !cached || row.MachineID == ""
	})
}

// A released session whose row the reaper has stopped is woken by a message on each topic it
// kept: the listener reads the message off its durable, finds the stopped row that keeps the
// topic, and reports a followed-topic wake naming the session and the message's topic, which the
// Legion daemon resumes the released claim on. The process the wake starts registers its agent
// subject alone and is handed both messages by its own consumer.
func TestAReleasedSessionIsWokenByAMessageOnEachTopicItKeptAndReceivesIt(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const (
		sessionID = "ses_woken_follower"
		widgets   = "notifications.github.acme.widgets.pr.3370.>"
		tools     = "notifications.github.acme.tools.pr.205.>"
	)
	agent := contracts.LegacyAgentSubject(sessionID)
	rig.subscribe(t, processRegistration(t, sessionID, "process-1", "epoch-1", agent, widgets, tools))
	awaitSessionConsumerStored(t, rig.harness.client.Conn, sessionID)
	rig.release(t, sessionID)
	rig.reapPastTheAllowance(t, sessionID)
	handler, reports := rig.wakingDurable(t)

	published := map[string]uint64{}
	for _, subject := range []string{"notifications.github.acme.widgets.pr.3370.review", "notifications.github.acme.tools.pr.205.review"} {
		seq := rig.store(t, subject)
		published[subject] = seq
		rig.handStored(t, handler, seq)
	}
	woken := followedWakesOf(heardWakes(t, reports, 3*time.Second), sessionID)
	var topics []string
	for _, wake := range woken {
		topics = append(topics, wake.Topic)
	}
	slices.Sort(topics)
	if want := []string{"notifications.github.acme.tools.pr.205.review", "notifications.github.acme.widgets.pr.3370.review"}; !slices.Equal(topics, want) {
		t.Fatalf("the followed-topic wakes of %s name %v, want one for the message on each kept topic, %v", sessionID, topics, want)
	}

	rig.subscribe(t, processRegistration(t, sessionID, "process-2", "epoch-2", agent))
	pullCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pull, err := rig.consumers.Pull(pullCtx, sessionID)
	if err != nil {
		t.Fatalf("pull the woken session's consumer: %v", err)
	}
	defer pull.Stop()
	want := []uint64{published["notifications.github.acme.widgets.pr.3370.review"], published["notifications.github.acme.tools.pr.205.review"]}
	for _, seq := range want {
		msg, err := pull.Next(pullCtx)
		if err != nil || msg.StreamSeq != seq {
			t.Fatalf("the woken session's consumer handed over seq %d, %v; want the message that woke it, stored at %d", msg.StreamSeq, err, seq)
		}
		if err := msg.Ack(); err != nil {
			t.Fatalf("ack %d: %v", seq, err)
		}
	}
}

// A released session is woken by a message on a topic it followed in the minutes before the reaper
// stops its row too: its row still names this listener, which finds the session gone and reports
// the same wake.
func TestAReleasedSessionIsWokenByAFollowedTopicBeforeItsRowIsStopped(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const (
		sessionID = "ses_just_released"
		widgets   = "notifications.github.acme.widgets.pr.3370.>"
	)
	rig.subscribe(t, processRegistration(t, sessionID, "process-1", "epoch-1", contracts.LegacyAgentSubject(sessionID), widgets))
	awaitSessionConsumerStored(t, rig.harness.client.Conn, sessionID)
	rig.release(t, sessionID)
	handler, reports := rig.wakingDurable(t)

	rig.handStored(t, handler, rig.store(t, "notifications.github.acme.widgets.pr.3370.review"))
	woken := followedWakesOf(heardWakes(t, reports, 3*time.Second), sessionID)
	if len(woken) != 1 || woken[0].Topic != "notifications.github.acme.widgets.pr.3370.review" {
		t.Fatalf("the followed-topic wakes of %s = %+v, want one for the message on the topic it followed", sessionID, woken)
	}
}

// A message on a topic a released session never subscribed to wakes nothing, for a session whose
// row is stopped and for one whose row still names this listener. The message on a topic each
// one follows, handed to the durable after it, is the control: it wakes each session once,
// naming its own topic.
func TestAMessageOnATopicTheSessionNeverFollowedWakesNothing(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const (
		stopped  = "ses_stopped_follower"
		owned    = "ses_owned_follower"
		widgets  = "notifications.github.acme.widgets.pr.3370.>"
		followed = "notifications.github.acme.widgets.pr.3370.review"
		other    = "notifications.github.acme.other.pr.9.review"
	)
	follow := func(sessionID string) {
		t.Helper()
		rig.subscribe(t, processRegistration(t, sessionID, "process-1", "epoch-1", contracts.LegacyAgentSubject(sessionID), widgets))
		awaitSessionConsumerStored(t, rig.harness.client.Conn, sessionID)
		rig.release(t, sessionID)
	}
	follow(stopped)
	rig.reapPastTheAllowance(t, stopped)
	// The other session follows and is released after the reaper's pass, so its row still names
	// this listener.
	follow(owned)
	if row, _ := rig.harness.registry.Cached(owned); row.MachineID != "test-machine" {
		t.Fatalf("precondition: %s's row = %+v, want it still naming this listener", owned, row)
	}
	if row, _ := rig.harness.registry.Cached(stopped); row.MachineID != "" {
		t.Fatalf("precondition: %s's row = %+v, want it naming no listener", stopped, row)
	}
	handler, reports := rig.wakingDurable(t)

	rig.handStored(t, handler, rig.store(t, other))
	rig.handStored(t, handler, rig.store(t, followed))
	heard := heardWakes(t, reports, 3*time.Second)
	for _, sessionID := range []string{stopped, owned} {
		woken := followedWakesOf(heard, sessionID)
		if len(woken) != 1 || woken[0].Topic != followed {
			t.Errorf("the followed-topic wakes of %s = %+v, want the one for %s alone: %s is no topic it followed", sessionID, woken, followed, other)
		}
	}
}

// A private stream subject wakes no session but its own through a topic it follows: not another
// session's inbox, not another session's failed-delivery lane, and not a controller's notice
// lane. Each row here holds notifications.> as an earlier build could have stored it, which
// matches every one of those subjects, once stopped and once still naming this listener for a
// session that is gone. The message on a team topic after them is the control: it wakes each
// session once.
func TestAPrivateSubjectNeverWakesAnotherSession(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const (
		stopped = "ses_stopped_wildcard"
		owned   = "ses_owned_wildcard"
		team    = "notifications.team.wake-control"
	)
	interests, err := rig.harness.client.JS().KeyValue(bus.InterestBucket)
	if err != nil {
		t.Fatalf("open the interest bucket: %v", err)
	}
	for sessionID, row := range map[string]string{
		stopped: `{"session_id":%q,"machine_id":"","dir":"","topics":["notifications.>"],"updated_at":%d,"stopped":true}`,
		owned:   `{"session_id":%q,"machine_id":"test-machine","dir":"","topics":["notifications.>"],"updated_at":%d}`,
	} {
		if _, err := interests.Put(sessionID, fmt.Appendf(nil, row, sessionID, time.Now().UnixMilli())); err != nil {
			t.Fatalf("store %s's row: %v", sessionID, err)
		}
		testwait.Eventually(t, "this listener to hold "+sessionID+"'s row", func() bool {
			_, cached := rig.harness.registry.Cached(sessionID)
			return cached
		})
	}
	handler, reports := rig.wakingDurable(t)

	for _, subject := range []string{
		contracts.LegacyAgentSubject("ses_somebody_else"),
		bus.ExceptionTopicPrefix + contracts.LegacyAgentSubject("ses_somebody_else"),
		"notifications.legion.ACME.controller",
		team,
	} {
		rig.handStored(t, handler, rig.store(t, subject))
	}
	heard := heardWakes(t, reports, 3*time.Second)
	for _, sessionID := range []string{stopped, owned} {
		woken := followedWakesOf(heard, sessionID)
		if len(woken) != 1 || woken[0].Topic != team {
			t.Errorf("the followed-topic wakes of %s = %+v, want the one for %s alone: a private subject is not %s's to be woken by", sessionID, woken, team, sessionID)
		}
	}
}
