package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// A session's consumer filter keeps every topic of its interest row while a hub move's quiesce has
// narrowed the stream, so that a message stored on such a topic once the narrowing ends is
// delivered: every build of the filter (a registration, each rebuild from the stored row, both
// passes of a fenced write) holds the row's topics whatever the deployed stream captures, and none
// is refused for a topic the stream does not capture now. The filter leaves out only five fixed
// kinds: a topic the server refuses as a filter subject, a role lane or its exception lane, a
// topic with a wildcard token reaching another session's private subjects (a literal topic is
// never of this kind, so a controller notice lane the session holds stays), a topic that does not
// begin with the literal notifications., and a wildcard topic that fed a merge of the filter's
// reduction reaching another session's private subjects. The last two are named, logged and kept
// out of the row (TestATopicTheFilterLeavesOutIsNamedLoggedAndKeptOutOfTheRow), and the
// reduction's cases are TestAFilterWhoseTopicsReduceToAnotherSessionsSubjectsLeavesThemOut.
//
// Before, the filter carried only the topics the deployed stream captured as the listener last
// read it: a rebuild while narrowed dropped the session's stored topics, and once the stream
// captured them again a message stored on one was stored and answered 200 and never delivered,
// until a later registration put the topic back, the consumer having passed the message by.
//
// What a registration adds to the row is as before, but for a topic this build stores: it adds
// one also while the stream is narrowed, since the stream stores it again once the narrowing ends,
// where before the narrowed listener dropped it with a "was not registered" warning.
//
// notifications.x.* stands for a subject only a newer build compiles: the stream is stamped with a
// generation above this build's before the listener starts, so the listener leaves it as it is,
// and a topic under it is one the deployed stream captures and this build does not store. Each
// step reports its own result, so a step that fails does not hide the steps after it.
func TestASessionKeepsAStoredTopicInItsFilterWhileTheStreamIsNarrowed(t *testing.T) {
	// Serial: step 5 reads the new subjects during the second pass of the write.
	// The production fence still gives both passes a single wall-clock budget.
	const (
		teamT      = "notifications.team.t"
		teamU      = "notifications.team.u"
		teamV      = "notifications.team.v"
		newerX     = "notifications.x.y"
		adhoc      = "notifications.lxspec.probe"
		lane       = "notifications.legion.infra.controller"
		laneRole   = "legion-infra-controller"
		sesA, sesB = "ses_narrow_a", "ses_narrow_b"
		sesC, sesD = "ses_narrow_c", "ses_narrow_d"
		sesProbe   = "ses_narrow_probe"
	)
	if role, ok := bus.ControllerNoticeRole(lane); !ok || role != laneRole {
		t.Fatalf("%s is the lane of role %q (%t), want %s", lane, role, ok, laneRole)
	}
	url := testnats.URL(t)
	deployNewerBuildsStream(t, url, "notifications.x.*")
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1), bus.WithPromptReconnect())
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newSessionConsumerRigOn(t, client, url)
	relays, err := startSessionRelays(rig.deps.consumers, rig.harness.config)
	if err != nil {
		t.Fatalf("start the session relays: %v", err)
	}
	t.Cleanup(func() { testwait.Returns(t, "the session relays to stop", func() { relays.stop(testwait.Context(t)) }) })
	registry := rig.harness.registry
	raw := testnats.Connect(t, url)
	t.Cleanup(raw.Close)
	js, err := raw.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	original, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the newer build's stream: %v", err)
	}
	if !slices.Contains(original.Config.Subjects, "notifications.x.*") {
		t.Fatalf("the stream's subjects are %v, want the newer build's notifications.x.* among them", original.Config.Subjects)
	}

	// An owning client reads the deployed subjects in its reconnect hook. Drive that
	// read on this same client instead of waiting for its periodic refresh.
	readCapture := func(t *testing.T, probe string, captures bool) {
		t.Helper()
		before := client.HookedConnectionEvents()
		if err := client.Connection().ForceReconnect(); err != nil {
			t.Fatalf("reconnect the listener to read the deployed subjects: %v", err)
		}
		testwait.Eventually(t, "the listener's reconnect hooks to read the deployed subjects", func() bool {
			return client.HookedConnectionEvents() > before
		})
		if got := client.DeployedStreamCaptures(probe); got != captures {
			t.Fatalf("the listener read deployed capture of %s as %t, want %t", probe, got, captures)
		}
	}
	narrow := func(t *testing.T, probe string, subjects ...string) {
		t.Helper()
		bustest.NarrowStream(t, js, func(subject string) bool { return !slices.Contains(subjects, subject) })
		readCapture(t, probe, false)
	}
	register := func(t *testing.T, sessionID string, topics ...string) interestSubscribeAnswer {
		t.Helper()
		var answer interestSubscribeAnswer
		if err := json.Unmarshal([]byte(rig.post(t, "/v1/interests/subscribe", consumerRegistration(sessionID, "e", topics...))), &answer); err != nil {
			t.Fatalf("decode the registration's answer: %v", err)
		}
		return answer
	}
	notRegistered := func(answer interestSubscribeAnswer, sessionID, topic string) bool {
		return slices.Contains(answer.Warnings, contracts.UncapturedTopicRefusal(topic, sessionID))
	}
	rowHolds := func(t *testing.T, sessionID string, topics ...string) {
		t.Helper()
		row, _, err := registry.RefreshAt(testwait.Context(t), sessionID)
		if err != nil {
			t.Fatalf("read the interest row of %s: %v", sessionID, err)
		}
		for _, topic := range topics {
			if !slices.Contains(row.Topics, topic) {
				t.Errorf("the interest row of %s holds %v, without %s", sessionID, row.Topics, topic)
			}
		}
	}
	filterHolds := func(t *testing.T, what, sessionID string, topics ...string) {
		t.Helper()
		filters := rig.filters(t, sessionID)
		for _, topic := range topics {
			if !slices.Contains(filters, topic) {
				t.Errorf("%s: the consumer of %s filters on %v, without %s", what, sessionID, filters, topic)
			}
		}
	}
	// Pull through a marker stored after the messages under test. The consumer
	// returns messages in stream order, so an extra match precedes the marker.
	delivered := func(t *testing.T, sessionID, markerSubject string) []string {
		t.Helper()
		ctx := testwait.Context(t)
		marker := listenerTestEnvelope(markerSubject, "narrowed-marker-"+sessionID)
		ack, err := js.Publish(markerSubject, marshalListenerEnvelope(t, marker), natsgo.Context(ctx))
		if err != nil {
			t.Fatalf("store the marker for %s on %s: %v", sessionID, markerSubject, err)
		}
		pull, err := rig.consumers.Pull(ctx, sessionID)
		if err != nil {
			t.Fatalf("pull the consumer of %s: %v", sessionID, err)
		}
		defer pull.Stop()
		var subjects []string
		for {
			msg, err := pull.Next(ctx)
			if err != nil {
				t.Fatalf("read the consumer of %s through marker sequence %d: %v", sessionID, ack.Sequence, err)
			}
			if err := msg.Ack(); err != nil {
				t.Fatalf("ack %s: %v", msg.Subject, err)
			}
			if msg.StreamSeq == ack.Sequence {
				return subjects
			}
			subjects = append(subjects, msg.Subject)
		}
	}

	register(t, sesA, teamT, newerX)
	filterHolds(t, "the first registration", sesA, teamT, newerX)
	narrow(t, teamT, "notifications.team.>", "notifications.x.*")

	t.Run("step 1, a registration while narrowed", func(t *testing.T) {
		answer := register(t, sesA, teamT, newerX)
		if !notRegistered(answer, sesA, newerX) {
			t.Errorf("warnings %q, want one naming %s, which neither this build nor the narrowed stream captures", answer.Warnings, newerX)
		}
		if notRegistered(answer, sesA, teamT) {
			t.Errorf("warnings %q name %s, a topic this build stores", answer.Warnings, teamT)
		}
		rowHolds(t, sesA, teamT, newerX)
		filterHolds(t, "a registration while narrowed", sesA, teamT, newerX)
	})

	t.Run("step 2, the rebuilds while narrowed", func(t *testing.T) {
		ctx := testwait.Context(t)
		reads := 0
		rig.deps.consumers.afterStoredRowRead = func() { reads++ }
		defer func() { rig.deps.consumers.afterStoredRowRead = nil }()

		if _, outcome, _ := rig.deps.consumers.storedFilterStep(ctx, registry, sesA, "narrowed test"); outcome != filterWriteLanded || reads != 1 {
			t.Errorf("storedFilterStep read the row %d times and answered %v, want one read and a write that landed", reads, outcome)
		}
		filterHolds(t, "storedFilterStep", sesA, teamT, newerX)

		reads = 0
		_, pull, outcome := relays.restart(ctx, &relayRun{sessionID: sesA})
		if pull == nil || reads == 0 {
			t.Errorf("the relay restart read the row %d times and pulled %v (outcome %v), want a read through storedFilter and a pull", reads, pull, outcome)
		} else {
			pull.Stop()
		}
		filterHolds(t, "a relay restart", sesA, teamT, newerX)

		built := 0
		rig.deps.consumers.afterRepairPlan = func() { built++ }
		defer func() { rig.deps.consumers.afterRepairPlan = nil }()
		rig.deps.consumers.repairRegistered(ctx, rig.deps, sesA, 0, "narrowed test")
		if built == 0 {
			t.Errorf("the repair built no target through registeredTarget")
		}
		filterHolds(t, "a repair", sesA, teamT, newerX)
	})

	t.Run("step 3, the control that the narrowing was seen", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		rig.mux.ServeHTTP(recorder, postJSON("/v1/messages/publish", `{"topic":"`+teamT+`","message":"narrowed","source":"agent","source_session":"`+sesA+`"}`))
		var answer listenerwire.APIError
		_ = json.Unmarshal(recorder.Body.Bytes(), &answer)
		if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(answer.Error, bus.ErrSubjectNotDeployed.Error()) {
			t.Errorf("a publish to %s while narrowed: %d %s, want the 503 of %q", teamT, recorder.Code, recorder.Body.String(), bus.ErrSubjectNotDeployed)
		}
		if answer := register(t, sesProbe, adhoc); !notRegistered(answer, sesProbe, adhoc) {
			t.Errorf("warnings %q, want one naming %s, which no stream captures", answer.Warnings, adhoc)
		}
	})

	t.Run("step 4, a new session while narrowed", func(t *testing.T) {
		if answer := register(t, sesB, teamT); notRegistered(answer, sesB, teamT) {
			t.Errorf("warnings %q name %s, a topic this build stores", answer.Warnings, teamT)
		}
		rowHolds(t, sesB, teamT)
		filterHolds(t, "a new session while narrowed", sesB, teamT)
	})

	t.Run("step 5, a narrowing between a fenced write's two passes", func(t *testing.T) {
		bustest.RestoreStream(t, js, original.Config)
		readCapture(t, teamT, true)
		passes := 0
		rig.deps.consumers.afterStoredRowRead = func() {
			passes++
			switch passes {
			case 1:
				// Another listener's registration adds a topic under the first pass's write.
				if _, err := registry.Upsert(store.Interest{SessionID: sesA, MachineID: "test-machine"}, []string{teamU}); err != nil {
					t.Errorf("store %s on %s's row: %v", teamU, sesA, err)
				}
			case 2:
				// The correction must build its filter from the newly narrowed subjects.
				narrow(t, teamT, "notifications.team.>", "notifications.x.*")
			}
		}
		defer func() { rig.deps.consumers.afterStoredRowRead = nil }()
		if err := rig.deps.consumers.storedFilterWrite(testwait.Context(t), registry, sesA, "narrowed test"); err != nil {
			t.Errorf("the fenced write did not settle: %v", err)
		}
		if passes != 2 {
			t.Errorf("the fenced write made %d passes, want the write and its correction", passes)
		}
		filterHolds(t, "a correction pass built while narrowed", sesA, teamT, teamU, newerX)
	})

	t.Run("step 6, the agent subject narrowed too", func(t *testing.T) {
		own := "notifications.agent.ACME." + sesC
		narrow(t, contracts.LegacyAgentSubject(sesC), "notifications.agent.>")
		if answer := register(t, sesC, own); notRegistered(answer, sesC, own) {
			t.Errorf("warnings %q name %s, the session's own agent subject", answer.Warnings, own)
		}
		rowHolds(t, sesC, own)
	})

	t.Run("step 7, the restore", func(t *testing.T) {
		bustest.RestoreStream(t, js, original.Config)
		published := []string{teamT, teamU, newerX, contracts.LegacyAgentSubject(sesA)}
		for _, subject := range published {
			if _, err := js.Publish(subject, marshalListenerEnvelope(t, listenerTestEnvelope(subject, "narrowed-restore-"+subject))); err != nil {
				t.Fatalf("store a message on %s after the restore: %v", subject, err)
			}
		}
		if got := delivered(t, sesA, contracts.LegacyAgentSubject(sesA)); !slices.Equal(got, published) {
			t.Errorf("the consumer of %s delivered %v, want %v", sesA, got, published)
		}
		if got := delivered(t, sesB, contracts.LegacyAgentSubject(sesB)); !slices.Equal(got, []string{teamT}) {
			t.Errorf("the consumer of %s delivered %v, want %v", sesB, got, []string{teamT})
		}
	})

	t.Run("step 8, a wildcard left in an earlier build's row, and a held controller lane", func(t *testing.T) {
		claimRoles(t, registry, sesD, laneRole)
		register(t, sesD, "notifications.team.>")
		kv, err := client.JS().KeyValue(bus.InterestBucket)
		if err != nil {
			t.Fatalf("open the interest bucket: %v", err)
		}
		earlier, err := json.Marshal(store.Interest{SessionID: sesD, MachineID: "test-machine", UpdatedAt: contracts.NowMillis(), Topics: []string{
			"notifications.>", "notifications.*.t", "notifications.agent.*." + sesA, "notifications.agent.*." + sesD, lane, "notifications.team.>",
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := kv.Put(sesD, earlier); err != nil {
			t.Fatalf("store %s's row as an earlier build stored it: %v", sesD, err)
		}
		ctx := testwait.Context(t)
		if _, outcome, _ := rig.deps.consumers.storedFilterStep(ctx, registry, sesD, "narrowed test"); outcome != filterWriteLanded {
			t.Errorf("storedFilterStep for %s answered %v, want a write that landed", sesD, outcome)
		}
		filters := rig.filters(t, sesD)
		filterHolds(t, "an earlier build's row", sesD, contracts.LegacyAgentSubject(sesD), lane, "notifications.team.>")
		for _, private := range []string{"notifications.>", "notifications.*.t", "notifications.agent.*." + sesA} {
			if slices.Contains(filters, private) {
				t.Errorf("the consumer of %s filters on %v, %s among them", sesD, filters, private)
			}
		}
		for _, subject := range []string{contracts.LegacyAgentSubject(sesA), lane, teamV} {
			if _, err := js.Publish(subject, marshalListenerEnvelope(t, listenerTestEnvelope(subject, "narrowed-earlier-"+subject))); err != nil {
				t.Fatalf("store a message on %s: %v", subject, err)
			}
		}
		if got, want := delivered(t, sesD, contracts.LegacyAgentSubject(sesD)), []string{lane, teamV}; !slices.Equal(got, want) {
			t.Errorf("the consumer of %s delivered %v, want %v", sesD, got, want)
		}
	})
}
