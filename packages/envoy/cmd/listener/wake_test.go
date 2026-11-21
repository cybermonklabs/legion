package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// testWakeProject is the project the wakes these tests raise are for: each role they wake is a
// Legion role of it (legion-acme-…), and each session they wake has a record listing it. A wake of
// it is reported on testWakeSubject.
const testWakeProject = "ACME"

var testWakeSubject = contracts.LegionWakeSubject("acme")

// listedProjects is a reporter's project reader (sessionProjects) over a fixed map, as a
// known-session store whose records list those projects: a session the map names has a record,
// and any other has none.
func listedProjects(records map[string][]string) sessionProjects {
	return func(sessionID string) ([]string, bool) {
		projects, recorded := records[sessionID]
		return projects, recorded
	}
}

// everySessionIn is a reporter's project reader that answers every session a record listing
// projects.
func everySessionIn(projects ...string) sessionProjects {
	return func(string) ([]string, bool) { return projects, true }
}

// The wake report's wire shape is what the Legion daemon's decoder reads
// (packages/daemon/internal/intake/decode.go, decodeListenerWake): a kind, the role token or the
// session, and the listener's own clock in epoch milliseconds. A role report names no session
// and an agent report no role, so a daemon never matches one against the wrong claim.
func TestTheWakeReportsWireShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report wakeReport
		want   map[string]any
	}{
		{
			name:   "a role message",
			report: wakeReport{Kind: wakeKindRole, Role: "legion-legion-legion-208-implementer", At: 1762070400000},
			want:   map[string]any{"kind": "role_message", "role": "legion-legion-legion-208-implementer", "at": float64(1762070400000)},
		},
		{
			name:   "an agent message",
			report: wakeReport{Kind: wakeKindAgent, Session: "session-1", At: 1762070400000},
			want:   map[string]any{"kind": "agent_message", "session": "session-1", "at": float64(1762070400000)},
		},
		{
			name:   "the liveness report",
			report: wakeReport{Kind: wakeKindReady, At: 1762070400000},
			want:   map[string]any{"kind": "listener_ready", "at": float64(1762070400000)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.report)
			if err != nil {
				t.Fatalf("encode the report: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatalf("decode the report: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("report = %v, want exactly %v", got, tc.want)
			}
			for key, value := range tc.want {
				if got[key] != value {
					t.Fatalf("report[%s] = %v, want %v", key, got[key], value)
				}
			}
		})
	}
}

// A report with no bus to publish on is dropped, never a panic: a listener built without a
// client in a test answers its 404s as it always did.
func TestAWakeReportWithNoBusIsDropped(t *testing.T) {
	publishWakeReport(context.Background(), nil, logging.New("test"), wakeReport{Kind: wakeKindReady})
	reportAgentWake(nil, logging.New("test"), "")
	if !bus.StreamCaptures(bus.LegionWakeTopic) {
		t.Fatalf("the stream does not capture the wake topic")
	}
}

// Every wake report the listener publishes is an envelope the stream takes and a Legion daemon
// reads. The report's payload was proven above and the feature was still dead: an envelope
// missing one required field is dropped by publishWakeReport's own Validate, and a dropped
// listener_ready holds every daemon's release gate shut for ever (the daemon's
// workflowRuntime.listenerLive). So this reads what actually crossed the bus, for each of the
// three kinds, and checks every field contracts.Envelope.Validate requires — source_event_id,
// the one that was missing, among them — and the subject each crossed on: a role report and an
// agent report on their project's own wake subject, and listener_ready on the shared one.
func TestEveryWakeReportCrossesTheBusAsAValidEnvelope(t *testing.T) {
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the bus: %v", err)
	}
	t.Cleanup(client.Close)
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	subscription, err := conn.SubscribeSync("notifications.legion.>")
	if err != nil {
		t.Fatalf("subscribe to the wake subjects: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}

	for _, row := range []struct {
		report  wakeReport
		subject string
	}{
		{wakeReport{Kind: wakeKindRole, Role: "legion-legion-legion-208-implementer", token: "legion"}, "notifications.legion.legion.wake"},
		{wakeReport{Kind: wakeKindAgent, Session: "session-1", token: "acme"}, "notifications.legion.acme.wake"},
		{wakeReport{Kind: wakeKindReady}, bus.LegionWakeTopic},
	} {
		report := row.report
		publishWakeReport(context.Background(), client, logging.New("test"), report)
		message, err := subscription.NextMsg(10 * time.Second)
		if err != nil {
			t.Fatalf("the %s report never reached a wake subject: %v", report.Kind, err)
		}
		if message.Subject != row.subject {
			t.Errorf("the %s report crossed on %s, want %s", report.Kind, message.Subject, row.subject)
		}
		var item contracts.Envelope
		if err := json.Unmarshal(message.Data, &item); err != nil {
			t.Fatalf("decode the %s report's envelope: %v", report.Kind, err)
		}
		if err := item.Validate(); err != nil {
			t.Fatalf("the %s report's envelope is not valid: %v", report.Kind, err)
		}
		for _, required := range []struct{ field, value string }{
			{"event_id", item.EventID},
			{"source", item.Source},
			{"source_event_id", item.SourceEventID},
			{"topic", item.Topic},
			{"dedupe_key", item.DedupeKey},
			{"payload_summary", item.PayloadSummary},
			{"trace_id", item.TraceID},
			{"payload", item.Payload},
		} {
			if strings.TrimSpace(required.value) == "" {
				t.Errorf("the %s report's envelope carries no %s", report.Kind, required.field)
			}
		}
		if item.IssuedAt == 0 {
			t.Errorf("the %s report's envelope carries no issued_at", report.Kind)
		}
		if item.Source != "envoy" || item.Topic != row.subject {
			t.Errorf("the %s report crossed as source %q on %q, want envoy on %s",
				report.Kind, item.Source, item.Topic, row.subject)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(item.Payload), &payload); err != nil {
			t.Fatalf("decode the %s report's payload: %v", report.Kind, err)
		}
		if payload["kind"] != report.Kind {
			t.Errorf("the %s report's payload says kind %v", report.Kind, payload["kind"])
		}
	}
}

// A role report goes on the wake subject of the project the role's own name gives
// (legion-<token>-…), read as written; a role of any other shape is no Legion deployment's, and
// its report goes nowhere.
func TestARoleWakeIsForTheProjectItsNameGives(t *testing.T) {
	for role, want := range map[string]string{
		"legion-legion-legion-208-implementer": "legion",
		"legion-priv-priv-7-architect":         "priv",
		"legion-acme2-controller":              "acme2",
		"lead-reviewer":                        "",
		"legion-":                              "",
		"legion-priv":                          "",
		"legion-Priv-priv-7-architect":         "",
		"legion-pr.v-x":                        "",
	} {
		if got := legionRoleToken(role); got != want {
			t.Errorf("legionRoleToken(%q) = %q, want %q", role, got, want)
		}
	}
}

// An agent wake goes on the wake subject of each project the TARGET session's own record lists,
// once each, and never on a project the message that raised it names: a sender chooses every field
// of its envelope. A session whose record lists no project, or that has no record, gets no report
// on any subject, and that is logged once per session, however many messages it misses.
//
// The control is in this file: the rejected rule, the project of the agent subject the message
// was sent on (envelopeProjectTokens), named for the message sent on OTHER's subject a project its
// target is not in, and the reports read here never reach OTHER's subject.
func TestAnAgentWakeGoesOnTheProjectsOfTheTargetsRecord(t *testing.T) {
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the bus: %v", err)
	}
	t.Cleanup(client.Close)
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	subscription, err := conn.SubscribeSync("notifications.legion.>")
	if err != nil {
		t.Fatalf("subscribe to the wake subjects: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	logs := &lockedBuffer{}
	logger := logging.NewWithWriter("test", logs)
	lifetime, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	reporter := newWakeReporter(lifetime, client, listedProjects(map[string][]string{
		"ses_two_projects": {"PRIV", "ACME", "PRIV"},
		"ses_none":         nil,
		"ses_odd":          {contracts.NoProject, "priv"},
	}))

	// The message that raised the wake was sent on OTHER's agent subject.
	sentOn := contracts.AgentSubject("OTHER", "ses_two_projects")
	if control := envelopeProjectTokens(sentOn); !slices.Equal(control, []string{"other"}) {
		t.Fatalf("the control names %v for a message on %s, want [other], so it shows nothing", control, sentOn)
	}
	reportAgentWake(reporter, logger, "ses_two_projects")
	reportAgentWake(reporter, logger, "ses_none")
	reportAgentWake(reporter, logger, "ses_none")
	reportAgentWake(reporter, logger, "ses_odd")
	reportAgentWake(reporter, logger, "ses_unknown")

	var got []string
	for {
		message, err := subscription.NextMsg(2 * time.Second)
		if err != nil {
			break
		}
		report := wakeReportIn(t, message)
		got = append(got, message.Subject+" "+report.Kind+" "+report.Session)
	}
	slices.Sort(got)
	want := []string{
		"notifications.legion.acme.wake agent_message ses_two_projects",
		"notifications.legion.priv.wake agent_message ses_two_projects",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("wake reports = %q, want %q: one per project the target's record lists, none on the envelope's OTHER, none for a record listing none", got, want)
	}
	unreported := map[string]int{}
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if !strings.Contains(line, "legion agent wake not reported: the session's record lists no project") {
			continue
		}
		for _, session := range []string{"ses_none", "ses_odd", "ses_unknown", "ses_two_projects"} {
			if strings.Contains(line, `"session":"`+session+`"`) {
				unreported[session]++
			}
		}
	}
	if want := map[string]int{"ses_none": 1, "ses_odd": 1, "ses_unknown": 1}; !maps.Equal(unreported, want) {
		t.Errorf("no-project lines per session = %v, want %v, once per session that got no report:\n%s", unreported, want, logs.String())
	}
}

// envelopeProjectTokens is the rule an agent wake does not take, kept as this file's control: the
// project of the agent subject a message was sent on, which its sender chooses.
func envelopeProjectTokens(topic string) []string {
	project, _, ok := contracts.SubjectProject(topic)
	if !ok || projectWakeToken(project) == "" {
		return nil
	}
	return []string{projectWakeToken(project)}
}

// R3: a wake report is retried until the bus takes it. Losing one is not "one wake late": the
// daemon arms a backstop for a phase worker of an open phase alone, so a released architect, and
// a worker whose phase is closed, have nothing else that brings their pod back, and the daemon's
// stranded report passes over a released claim no wake ever touched. A report dropped while the
// stream is out of reach — a reconnect, a stream not yet reconciled — would leave such a tree
// stalled with nothing saying so.
//
// The bus here is out of reach exactly that way: a client of an account whose stream does not
// exist yet cannot publish to it at all, and the stream arrives while the report is retrying.
func TestAWakeReportIsRetriedUntilTheBusTakesIt(t *testing.T) {
	url := testnats.URL(t)
	client, err := bus.Connect([]string{url})
	if err != nil {
		t.Fatalf("connect the bus: %v", err)
	}
	t.Cleanup(client.Close)
	reported := make(chan struct{})
	go func() {
		defer close(reported)
		publishWakeReport(context.Background(), client, logging.New("test"),
			wakeReport{Kind: wakeKindAgent, Session: "session-released", token: "acme"})
	}()

	// The stream arrives only once one whole attempt has run out, as a reconnect or a
	// deployment's first reconcile brings it back: a report published once, however long its own
	// bound, is gone by then, and only a retry still has it to send.
	time.Sleep(wakeReportAttempt + 500*time.Millisecond)
	owner, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("create the stream: %v", err)
	}
	t.Cleanup(owner.Close)

	// What the report is read from is the stream, not the subject: a publish the stream never
	// acknowledged still reaches a core subscriber of that subject, and a wake the stream does
	// not hold reaches no daemon's durable and survives no daemon restart (R3).
	subscription, err := owner.JS().SubscribeSync(testWakeSubject, nats.DeliverAll(), nats.OrderedConsumer())
	if err != nil {
		t.Fatalf("read %s from the stream: %v", testWakeSubject, err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	message, err := subscription.NextMsg(60 * time.Second)
	if err != nil {
		t.Fatalf("the stream never stored the agent_message report: %v", err)
	}
	var item contracts.Envelope
	if err := json.Unmarshal(message.Data, &item); err != nil {
		t.Fatalf("decode the report's envelope: %v", err)
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("the retried report's envelope is not valid: %v", err)
	}
	select {
	case <-reported:
	case <-time.After(30 * time.Second):
		t.Fatal("the report's goroutine is still retrying after the stream took it")
	}
}

// A wake report the server refuses under the listener's grant is given up at its first attempt,
// naming the refusal, rather than published again until its five-minute budget runs out with one
// of the reports in flight held all that time: the grant refuses it again however often it is
// sent.
func TestAWakeReportTheGrantRefusesIsGivenUpAtOnce(t *testing.T) {
	client := deniedGrantClient(t, testWakeSubject)
	logs := &lockedBuffer{}
	lifetime, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	publishWakeReport(lifetime, client, logging.NewWithWriter("test", logs), wakeReport{Kind: wakeKindAgent, Session: "session-denied", token: "acme"})
	elapsed := time.Since(started)
	output := logs.String()
	if !strings.Contains(output, "legion wake report not published") || !strings.Contains(output, `bus: NATS denied the publish to \"`+testWakeSubject) {
		t.Fatalf("the refused report's log names no refusal:\n%s", output)
	}
	if strings.Contains(output, "retrying until the bus takes it") || elapsed > wakeReportAttempt+3*time.Second {
		t.Fatalf("the refused report was retried, or ended after %s, want one attempt within %s:\n%s",
			elapsed.Round(time.Millisecond), wakeReportAttempt+3*time.Second, output)
	}
}

// One report retries for up to wakeReportBudget, and during a bus outage one is raised per role
// send with no holder and per message nothing can place. So the reports run under the
// listener's own lifetime, which the shutdown ends, and no more than wakeReportsInFlight run at
// once: without both, an outage piles up a goroutine and a report per such request for minutes,
// and the shutdown leaves every one of them running.
func TestWakeReportsEndWithTheListenerAndAreCappedInFlight(t *testing.T) {
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the bus: %v", err)
	}
	// A closed connection is the outage: every publish from here fails and every report retries.
	client.Close()
	lifetime, stopListener := context.WithCancel(context.Background())
	t.Cleanup(stopListener)
	reporter := newWakeReporter(lifetime, client, everySessionIn(testWakeProject))
	logger := logging.NewWithWriter("test", io.Discard)

	for i := range wakeReportsInFlight {
		reportAgentWake(reporter, logger, fmt.Sprintf("session-%d", i))
	}
	if got := len(reporter.slots); got != wakeReportsInFlight {
		t.Fatalf("%d reports are in flight after %d were raised, want every slot taken", got, wakeReportsInFlight)
	}
	reportAgentWake(reporter, logger, "one-too-many")
	if got := len(reporter.slots); got != wakeReportsInFlight {
		t.Fatalf("a report past the cap left %d in flight, want it dropped at %d", got, wakeReportsInFlight)
	}

	// The listener stops: every report still retrying ends with it, rather than holding its
	// goroutine for the rest of its budget.
	stopListener()
	deadline := time.Now().Add(30 * time.Second)
	for len(reporter.slots) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d reports are still retrying after the listener stopped", len(reporter.slots))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Security[untrusted-input]: every wake subject is refused to every caller of the publish route,
// the shared one and each project's own. A Legion daemon acts on what it reads there — it wakes
// the pod a role token or a session names, and holds its whole release gate open on a
// `listener_ready` report — and a caller of this route chooses every field of the envelope and the
// payload, source "envoy" included, so a session that could publish here could wake pods at will
// and keep that gate open with a report dated in the future. The project's controller lane beside
// it is no wake subject and is not refused for being one.
func TestThePublishRouteRefusesTheLegionWakeTopic(t *testing.T) {
	for _, topic := range []string{bus.LegionWakeTopic, contracts.LegionWakeSubject("acme")} {
		recorder, _ := publishThrough(&listenerDeps{}, `{"topic":"`+topic+`","message":"listener_ready","source":"envoy"}`)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("publishing %s answered %d; want it refused with 400, body %s", topic, recorder.Code, recorder.Body)
		}
		if !strings.Contains(recorder.Body.String(), "the listener alone reports a Legion pod wake") {
			t.Fatalf("the refusal of %s = %s, want it to say the listener alone reports a wake", topic, recorder.Body)
		}
	}
	for topic, want := range map[string]bool{
		bus.LegionWakeTopic:                    true,
		"notifications.legion.acme.wake":       true,
		"notifications.legion.acme.controller": false,
		"notifications.legion.*.wake":          false,
		"notifications.legion.acme.extra.wake": false,
		"notifications.legion..wake":           false,
		"notifications.agent.ACME.ses_a":       false,
	} {
		if got := isLegionWakeTopic(topic); got != want {
			t.Errorf("isLegionWakeTopic(%q) = %v, want %v", topic, got, want)
		}
	}
}
