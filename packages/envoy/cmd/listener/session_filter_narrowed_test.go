package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
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
// A registration adds every followed topic to the row (uncapturedTopics only warns), and warns of
// a topic that neither this build nor the deployed stream captures with
// listenerwire.UncapturedTopicWarning, never of a topic this build stores: the stream stores it
// again once the narrowing ends.
//
// notifications.x.* stands for a subject only a newer build compiles: the stream is stamped with a
// generation above this build's before the listener starts, so the listener leaves it as it is,
// and a topic under it is one the deployed stream captures and this build does not store. Each
// step reports its own result, so a step that fails does not hide the steps after it.
func TestASessionKeepsAStoredTopicInItsFilterWhileTheStreamIsNarrowed(t *testing.T) {
	// Not parallel: it requires the listener's client to read a narrowing of the stream within 2 s,
	// inside the 5 s a fenced filter write has (sessionConsumerCallTimeout), and holds a 500 ms
	// quiet window after each consumer's wanted messages in which no other may arrive (delivered).
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
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1), bus.WithStreamSubjectsRefresh(100*time.Millisecond))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newSessionConsumerRigOn(t, client, url)
	relays, err := startSessionRelays(rig.deps.consumers, rig.harness.config)
	if err != nil {
		t.Fatalf("start the session relays: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		relays.stop(ctx)
	})
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

	// narrow narrows the named subjects as the quiesce does, leaving the others as they are, and
	// waits until the listener's client has read it, judged on probe, within bound.
	narrow := func(t *testing.T, probe string, bound time.Duration, subjects ...string) bool {
		t.Helper()
		bustest.NarrowStream(t, js, func(subject string) bool { return !slices.Contains(subjects, subject) })
		deadline := time.Now().Add(bound)
		for client.DeployedStreamCaptures(probe) {
			if time.Now().After(deadline) {
				t.Errorf("the listener's client did not read the narrowing of %v within %s", subjects, bound)
				return false
			}
			time.Sleep(20 * time.Millisecond)
		}
		return true
	}
	restore := func(t *testing.T, probe string) {
		t.Helper()
		bustest.RestoreStream(t, js, original.Config)
		bustest.AwaitDeployedCapture(t, client, probe, true, 15*time.Second)
	}
	register := func(t *testing.T, sessionID string, topics ...string) interestSubscribeAnswer {
		t.Helper()
		var answer interestSubscribeAnswer
		if err := json.Unmarshal([]byte(rig.post(t, "/v1/interests/subscribe", consumerRegistration(sessionID, "e", topics...))), &answer); err != nil {
			t.Fatalf("decode the registration's answer: %v", err)
		}
		return answer
	}
	uncapturedWarned := func(answer interestSubscribeAnswer, sessionID, topic string) bool {
		return slices.Contains(answer.Warnings, listenerwire.UncapturedTopicWarning(topic, sessionID))
	}
	rowHolds := func(t *testing.T, sessionID string, topics ...string) {
		t.Helper()
		row, _, err := registry.RefreshAt(context.Background(), sessionID)
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
	// delivered pulls sessionID's consumer for the subjects of want messages, acking each, and
	// then reads for half a second more.
	delivered := func(t *testing.T, sessionID string, want int) []string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pull, err := rig.consumers.Pull(ctx, sessionID, nil)
		if err != nil {
			t.Fatalf("pull the consumer of %s: %v", sessionID, err)
		}
		defer pull.Stop()
		var subjects []string
		for len(subjects) < want+1 {
			wait := 5 * time.Second
			if len(subjects) >= want {
				wait = 500 * time.Millisecond
			}
			next, cancelNext := context.WithTimeout(ctx, wait)
			msg, err := pull.Next(next)
			cancelNext()
			if err != nil {
				break
			}
			subjects = append(subjects, msg.Subject)
			if err := msg.Ack(); err != nil {
				t.Fatalf("ack %s: %v", msg.Subject, err)
			}
		}
		return subjects
	}

	register(t, sesA, teamT, newerX)
	filterHolds(t, "the first registration", sesA, teamT, newerX)
	if !narrow(t, teamT, 15*time.Second, "notifications.team.>", "notifications.x.*") {
		t.FailNow()
	}

	t.Run("step 1, a registration while narrowed", func(t *testing.T) {
		answer := register(t, sesA, teamT, newerX)
		if !uncapturedWarned(answer, sesA, newerX) {
			t.Errorf("warnings %q, want one naming %s, which neither this build nor the narrowed stream captures", answer.Warnings, newerX)
		}
		if uncapturedWarned(answer, sesA, teamT) {
			t.Errorf("warnings %q name %s, a topic this build stores", answer.Warnings, teamT)
		}
		rowHolds(t, sesA, teamT, newerX)
		filterHolds(t, "a registration while narrowed", sesA, teamT, newerX)
	})

	t.Run("step 2, the rebuilds while narrowed", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		reads := 0
		rig.deps.consumers.afterStoredRowRead = func() { reads++ }
		defer func() { rig.deps.consumers.afterStoredRowRead = nil }()

		if _, outcome := rig.deps.consumers.storedFilterStep(ctx, registry, sesA, "narrowed test"); outcome != filterWriteLanded || reads != 1 {
			t.Errorf("storedFilterStep read the row %d times and answered %v, want one read and a write that landed", reads, outcome)
		}
		filterHolds(t, "storedFilterStep", sesA, teamT, newerX)

		reads = 0
		// A relay pulls a session's consumer only while this task serves its machine's receive term
		// (sessionRelays.pullBound; cmd/listener/README.md, "Take-over"), so the restart is given
		// one, set on the relay set alone, which starts no run of its own.
		relays.eventsMu.Lock()
		relays.term = endlessTerm()
		relays.eventsMu.Unlock()
		_, pull, outcome := relays.restart(ctx, &relayRun{sessionID: sesA})
		relays.eventsMu.Lock()
		relays.term = nil
		relays.eventsMu.Unlock()
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
		if answer := register(t, sesProbe, adhoc); !uncapturedWarned(answer, sesProbe, adhoc) {
			t.Errorf("warnings %q, want one naming %s, which no stream captures", answer.Warnings, adhoc)
		}
	})

	t.Run("step 4, a new session while narrowed", func(t *testing.T) {
		if answer := register(t, sesB, teamT); uncapturedWarned(answer, sesB, teamT) {
			t.Errorf("warnings %q name %s, a topic this build stores", answer.Warnings, teamT)
		}
		rowHolds(t, sesB, teamT)
		filterHolds(t, "a new session while narrowed", sesB, teamT)
	})

	t.Run("step 5, a narrowing between a fenced write's two passes", func(t *testing.T) {
		restore(t, teamT)
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
				// The correction pass's build, made with the narrowing read: at most 2 s of the
				// fence's 5 s, so its own write keeps the rest.
				narrow(t, teamT, 2*time.Second, "notifications.team.>", "notifications.x.*")
			}
		}
		rig.deps.consumers.storedFilterWrite(context.Background(), registry, sesA, "narrowed test")
		rig.deps.consumers.afterStoredRowRead = nil
		if passes != 2 {
			t.Errorf("the fenced write made %d passes, want the write and its correction", passes)
		}
		filterHolds(t, "a correction pass built while narrowed", sesA, teamT, teamU, newerX)
	})

	t.Run("step 6, the agent subject narrowed too", func(t *testing.T) {
		own := "notifications.agent.ACME." + sesC
		if !narrow(t, contracts.LegacyAgentSubject(sesC), 15*time.Second, "notifications.agent.>") {
			return
		}
		if answer := register(t, sesC, own); uncapturedWarned(answer, sesC, own) {
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
		if got := delivered(t, sesA, len(published)); !slices.Equal(got, published) {
			t.Errorf("the consumer of %s delivered %v, want %v", sesA, got, published)
		}
		if got := delivered(t, sesB, 1); !slices.Equal(got, []string{teamT}) {
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
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, outcome := rig.deps.consumers.storedFilterStep(ctx, registry, sesD, "narrowed test"); outcome != filterWriteLanded {
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
		if got, want := delivered(t, sesD, 2), []string{lane, teamV}; !slices.Equal(got, want) {
			t.Errorf("the consumer of %s delivered %v, want %v", sesD, got, want)
		}
	})
}

// natsgoUnused keeps the natsgo import for the JetStream context type the rig reads through.
var _ natsgo.JetStreamContext
