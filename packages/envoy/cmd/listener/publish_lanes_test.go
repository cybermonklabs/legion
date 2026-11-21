package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// Security[untrusted-input]: the publish route refuses Dispatch's lane and GitHub's lane by name.
// The Legion daemon trusts an event on Dispatch's lane as Dispatch's, and it applies a merge and a
// deciding review from an event on GitHub's lane. A grant refuses a direct publish on either lane
// to every user but the lane's own publisher (packages/envoy/natsgrant). No grant can refuse a
// caller of this route on the listener beside envoy-dispatch, whose user holds Dispatch's lane, or
// on the webhook listener, whose user holds GitHub's lane. So the route answers 400 naming the lane
// and publishes nothing, and a subscriber on each lane sees nothing. A team topic and a role topic
// still publish on the same route.
func TestThePublishRouteRefusesDispatchAndGitHubLanes(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const (
		holder = "ses_lane_refusal_holder"
		role   = "lane-refusal"
		team   = "notifications.team.lane-refusal"
	)
	claimRoleFor(t, harness.registry, harness.sessions, holder, role, nil)
	frames := startStandIn(t, testnats.URL(t), holder)
	subscribe := func(subject string, into chan *natsgo.Msg) {
		t.Helper()
		subscription, err := harness.client.Conn.ChanSubscribe(subject, into)
		if err != nil {
			t.Fatalf("subscribe %s: %v", subject, err)
		}
		t.Cleanup(func() { _ = subscription.Unsubscribe() })
	}
	// Both lanes share one channel, so a message on either fails the row that reads it.
	onLanes, onTeam := make(chan *natsgo.Msg, 16), make(chan *natsgo.Msg, 16)
	subscribe(bus.DispatchTopicPrefix+">", onLanes)
	subscribe(contracts.GithubTopicPrefix+">", onLanes)
	subscribe(team, onTeam)
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the subscriptions: %v", err)
	}
	d := harness.publishDeps(t)

	// Each lane row poses as the lane's own publisher, with that publisher's source on the
	// envelope. The rows are subtests on the parent's bus, so a row that fails leaves the next
	// one to run.
	for _, row := range []struct{ lane, topic, source string }{
		{bus.DispatchTopicPrefix, bus.DispatchTopicPrefix + "issue.INFRA.INFRA-1.comment.created", "dispatch"},
		{contracts.GithubTopicPrefix, contracts.GithubTopicPrefix + "example-org.example-repo.pr.1.review", "github"},
	} {
		t.Run("refuses "+row.lane, func(t *testing.T) {
			recorder, _ := publishThrough(d, `{"topic":"`+row.topic+`","source":"`+row.source+`","message":"a forged event"}`)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("publish to %s: status %d, want 400; body %s", row.topic, recorder.Code, recorder.Body.String())
			}
			var refusal listenerwire.APIError
			if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("decode the refusal of %s %s: %v", row.topic, recorder.Body.String(), err)
			}
			if !strings.Contains(refusal.Error, row.lane) || !reflect.DeepEqual(refusal.Expected, []string{"topic"}) {
				t.Fatalf("publish to %s: refusal %+v, want it to name the lane %s and the topic field", row.topic, refusal, row.lane)
			}
		})
	}
	t.Run("publishes nothing on either lane", func(t *testing.T) {
		deadline := time.After(500 * time.Millisecond)
		for {
			select {
			case msg := <-onLanes:
				t.Errorf("a refused publish reached NATS on %s", msg.Subject)
			case <-deadline:
				return
			}
		}
	})

	t.Run("still publishes a team topic", func(t *testing.T) {
		if recorder, _ := publishThrough(d, `{"topic":"`+team+`","source":"agent","message":"a team message"}`); recorder.Code != http.StatusOK {
			t.Fatalf("publish to %s: status %d, want 200; body %s", team, recorder.Code, recorder.Body.String())
		}
		select {
		case msg := <-onTeam:
			if msg.Subject != team {
				t.Fatalf("the team publish reached %s, want %s", msg.Subject, team)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the publish to %s reached nobody", team)
		}
	})

	t.Run("still publishes a role topic", func(t *testing.T) {
		roleTopic := contracts.RoleTopicPrefix + role
		recorder, _ := publishThrough(d, `{"topic":"`+roleTopic+`","source":"agent","message":"a role message"}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("publish to %s: status %d, want 200; body %s", roleTopic, recorder.Code, recorder.Body.String())
		}
		var answer listenerwire.PublishResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
		}
		if answer.Holder != holder {
			t.Fatalf("the role publish answered holder %q, want %s; body %s", answer.Holder, holder, recorder.Body.String())
		}
		if frame := expectFrames(t, frames, 1, 300*time.Millisecond)[0]; frame.EventID != answer.EventID || frame.Topic != roleTopic {
			t.Fatalf("the holder received event %s on %s, want the answered event %s on %s", frame.EventID, frame.Topic, answer.EventID, roleTopic)
		}
		select {
		case msg := <-onLanes:
			t.Fatalf("the team or role publish reached NATS on %s", msg.Subject)
		default:
		}
	})
}
