package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// Every lane that keeps core NATS delivers through the listener's client whatever the deployed
// stream captures: this build's own subjects, a newer build's that drops notifications.agent.>,
// and a newer build's that adds notifications.x.>. A role lane, its exception lane, a relay
// subject, an ad-hoc topic and its exception lane go through bus.Client.Publish, which reaches a
// core subscriber at once, answers nil and stores nothing; the connection probe and its reply inbox
// go through RequestCore, and a probe of a subject nothing holds meets no responders; the live
// view's frames go through its own core source. A lane that took JetStream instead would store
// nothing for that subscriber to read and wait out the stream's acknowledgement, which no stream
// gives a role lane, so a change that sends one of these lanes through the stream fails here.
//
// The last rows are the agent subject, which this build stores: over the stream that drops it, its
// publish answers an error and stores nothing, never nil with nothing stored, with nothing
// subscribed to it (as in production, where a session reads only its own consumer) and with a core
// subscriber holding it, which a JetStream publish to a subject no stream captures is handed to.
func TestEveryCoreLaneDeliversWhateverTheDeployedStreamCaptures(t *testing.T) {
	agent := contracts.LegacyAgentSubject("ses_lane_over_deployed_streams")
	var dropsAgent []string
	for _, subject := range bus.StreamCarriedSubjects() {
		if !bus.SubjectContains(subject, agent) {
			dropsAgent = append(dropsAgent, subject)
		}
	}
	coreLanes := []struct{ name, topic string }{
		{"a role lane", contracts.RoleTopicPrefix + "reviewer"},
		{"a role lane's exception lane", bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + "reviewer"},
		{"a relay subject", bus.DeliverSubject("ses_lane_over_deployed_streams")},
		{"an ad-hoc topic", "notifications.lxspec.probe.all"},
		{"an ad-hoc topic's exception lane", bus.ExceptionTopicPrefix + "notifications.lxspec.probe.all"},
	}
	for _, stream := range []struct {
		name     string
		subjects []string
	}{
		{"this build's subjects", bus.StreamCarriedSubjects()},
		{"a newer build's that drops notifications.agent.>", dropsAgent},
		{"a newer build's that adds notifications.x.>", append(bus.StreamCarriedSubjects(), "notifications.x.>")},
	} {
		t.Run(stream.name, func(t *testing.T) {
			url := testnats.URL(t)
			deployNewerBuildsStreamOf(t, url, stream.subjects)
			client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
			if err != nil {
				t.Fatalf("connect the listener: %v", err)
			}
			t.Cleanup(client.Close)
			raw := testnats.Connect(t, url)
			t.Cleanup(raw.Close)

			for _, lane := range coreLanes {
				sub, err := raw.SubscribeSync(lane.topic)
				if err != nil {
					t.Fatalf("subscribe %s: %v", lane.topic, err)
				}
				if err := raw.Flush(); err != nil {
					t.Fatalf("flush the subscription to %s: %v", lane.topic, err)
				}
				publishErr := client.Publish(listenerTestEnvelope(lane.topic, "lane-"+id.New()))
				msg, nextErr := sub.NextMsg(5 * time.Second)
				_ = sub.Unsubscribe()
				stored := storedOnStream(t, client, bus.Stream, lane.topic)
				t.Logf("%s (%s): core=%t err=%v delivered=%t stored=%d", lane.name, lane.topic, client.UsesCoreTransport(lane.topic), publishErr, nextErr == nil, stored)
				if !client.UsesCoreTransport(lane.topic) {
					t.Errorf("%s (%s) takes JetStream, want core NATS", lane.name, lane.topic)
				}
				if publishErr != nil {
					t.Errorf("%s (%s): Publish = %v, want nil", lane.name, lane.topic, publishErr)
				}
				if nextErr != nil {
					t.Errorf("%s (%s): the core subscriber got nothing: %v", lane.name, lane.topic, nextErr)
				} else if len(msg.Data) == 0 {
					t.Errorf("%s (%s): the core subscriber got an empty message", lane.name, lane.topic)
				}
				if stored != 0 {
					t.Errorf("%s (%s): the stream stored %d copies, want none", lane.name, lane.topic, stored)
				}
			}

			// The connection probe on a relay subject a responder holds answers that responder's reply.
			probed := bus.DeliverSubject("ses_lane_probe")
			responder, err := raw.Subscribe(probed, func(msg *natsgo.Msg) { _ = msg.Respond([]byte("done")) })
			if err != nil {
				t.Fatalf("subscribe the probe's responder: %v", err)
			}
			if err := raw.Flush(); err != nil {
				t.Fatalf("flush the probe's responder: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			reply, probeErr := client.RequestCore(ctx, probed, natsgo.Header{bus.ConnectionProbeHeader: []string{"1"}})
			cancel()
			_ = responder.Unsubscribe()
			if probeErr != nil || reply == nil || string(reply.Data) != "done" {
				t.Errorf("the probe of a held relay subject = %v, %v; want done and nil", reply, probeErr)
			}
			// Nothing holds this one: the probe meets no responders, as a stopped session's does.
			ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
			_, silentErr := client.RequestCore(ctx, bus.DeliverSubject("ses_lane_nobody"), natsgo.Header{bus.ConnectionProbeHeader: []string{"1"}})
			cancel()
			if silentErr == nil {
				t.Errorf("the probe of a relay subject nothing holds answered nil, want no responders")
			}

			// The live view's frames over core NATS.
			view := agentstream.NewNATS(raw)
			frames := make(chan agentstream.Frame, 1)
			stop, err := view.Subscribe("ses_lane_view", func(frame agentstream.Frame) { frames <- frame })
			if err != nil {
				t.Fatalf("subscribe the live view: %v", err)
			}
			if err := raw.Flush(); err != nil {
				t.Fatalf("flush the live view's subscription: %v", err)
			}
			if err := client.Conn.Publish(contracts.AgentStreamFramesSubject("ses_lane_view"), []byte(`{"frame":1}`)); err != nil {
				t.Fatalf("publish a frame: %v", err)
			}
			var got string
			select {
			case frame := <-frames:
				got = string(frame)
			case <-time.After(5 * time.Second):
			}
			stop()
			if got != `{"frame":1}` {
				t.Errorf("the live view got %q, want the frame", got)
			}

			for _, held := range []bool{false, true} {
				topic := agent + map[bool]string{false: "_bare", true: "_held"}[held]
				var sub *natsgo.Subscription
				if held {
					if sub, err = raw.SubscribeSync(topic); err != nil {
						t.Fatalf("subscribe %s: %v", topic, err)
					}
					if err := raw.Flush(); err != nil {
						t.Fatalf("flush the subscription to %s: %v", topic, err)
					}
				}
				publishErr := client.Publish(listenerTestEnvelope(topic, "agent-"+id.New()))
				if sub != nil {
					_ = sub.Unsubscribe()
				}
				stored := storedOnStream(t, client, bus.Stream, topic)
				t.Logf("the agent subject %s (a core subscriber holds it: %t): err=%v stored=%d", topic, held, publishErr, stored)
				if publishErr == nil && stored == 0 {
					t.Errorf("Publish of %s answered nil and the stream holds no copy", topic)
				}
				if publishErr != nil && stored != 0 {
					t.Errorf("Publish of %s answered %v and the stream holds %d copies", topic, publishErr, stored)
				}
			}
		})
	}
}

// Every subject family this build stores - the agent subject, the outside events' (Dispatch,
// GitHub, Slack, Legion, ghostwispr, WhatsApp), a team's topic and the agent and team exception
// lanes - is stored or refused through the listener's client, never answered nil with no copy in
// the notifications stream. The negative control is a deployed stream of this build's subjects:
// nil and one copy each. A newer build's stream that keeps only the listener's own lanes stores
// none of them, so each publish answers an error and stores nothing; and beside it another stream
// that captures every one of those families acknowledges copies no session consumer reads, so each
// publish answers an error all the same, and that stream keeps nothing either. Over both, the
// error is the refusal of a stream that does not capture the subject (bus.ErrStreamLacksSubject),
// which the listener answers in words of its own rather than as a bus that did not answer.
func TestEveryStoredTopicIsStoredOrRefusedWhateverTheDeployedStreamCaptures(t *testing.T) {
	topics := []string{
		contracts.LegacyAgentSubject("ses_every_stored_family"),
		"notifications.dispatch.issue.ask.opened",
		"notifications.github.acme.widgets.pr.1",
		"notifications.slack.T1.C1.message",
		"notifications.legion.ACME.controller",
		"notifications.ghostwispr.s1.summary.ready",
		"notifications.whatsapp.15551234567.message",
		"notifications.team.deploy-squad",
		bus.ExceptionTopicPrefix + contracts.LegacyAgentSubject("ses_every_stored_family"),
		bus.ExceptionTopicPrefix + "notifications.team.deploy-squad",
	}
	var ownLanesOnly []string
	for _, subject := range bus.StreamCarriedSubjects() {
		if !slices.Contains(bus.StreamSubjects(), subject) {
			ownLanesOnly = append(ownLanesOnly, subject)
		}
	}
	if len(ownLanesOnly) == 0 || len(ownLanesOnly) == len(bus.StreamCarriedSubjects()) {
		t.Fatalf("the listener's own lanes are %v of %v, so no stream keeps them alone", ownLanesOnly, bus.StreamCarriedSubjects())
	}
	for _, stream := range []struct {
		name     string
		subjects []string
		another  []string
		captures bool
	}{
		{"this build's subjects", bus.StreamCarriedSubjects(), nil, true},
		{"a newer build's that keeps only the listener's own lanes", ownLanesOnly, nil, false},
		{"the same beside another stream that captures every family", ownLanesOnly, bus.StreamSubjects(), false},
	} {
		t.Run(stream.name, func(t *testing.T) {
			url := testnats.URL(t)
			deployNewerBuildsStreamOf(t, url, stream.subjects)
			if stream.another != nil {
				deployAnotherStream(t, url, stream.another)
			}
			client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
			if err != nil {
				t.Fatalf("connect the listener: %v", err)
			}
			t.Cleanup(client.Close)
			for _, topic := range topics {
				if !bus.StreamCaptures(topic) {
					t.Fatalf("this build stores no subject capturing %s", topic)
				}
				if got := client.DeployedStreamCaptures(topic); got != stream.captures {
					t.Fatalf("DeployedStreamCaptures(%s) = %t, want %t", topic, got, stream.captures)
				}
				publishErr := client.Publish(listenerTestEnvelope(topic, "family-"+id.New()))
				stored := storedOnStream(t, client, bus.Stream, topic)
				elsewhere := uint64(0)
				if stream.another != nil {
					elsewhere = storedOnStream(t, client, anotherStream, topic)
				}
				t.Logf("%s: err=%v, %d in %s, %d in %s", topic, publishErr, stored, bus.Stream, elsewhere, anotherStream)
				if publishErr == nil && stored == 0 {
					t.Errorf("Publish of %s answered nil and %s holds no copy (%d in %s)", topic, bus.Stream, elsewhere, anotherStream)
					continue
				}
				if stream.captures && (publishErr != nil || stored != 1) {
					t.Errorf("control: Publish of %s = %v with %d copies, want nil and 1", topic, publishErr, stored)
				}
				if !stream.captures && (publishErr == nil || stored != 0 || elsewhere != 0) {
					t.Errorf("Publish of %s = %v with %d copies in %s and %d in %s, want an error and none", topic, publishErr, stored, bus.Stream, elsewhere, anotherStream)
				}
				if !stream.captures && !errors.Is(publishErr, bus.ErrStreamLacksSubject) {
					t.Errorf("Publish of %s = %v, want the refusal of a stream that does not capture the subject (bus.ErrStreamLacksSubject)", topic, publishErr)
				}
			}
		})
	}
}
