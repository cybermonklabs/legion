package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
)

// withheldPayloadMark is written into every withheld frame's payload, so a log line that held any
// of the payload's bytes would hold it.
const withheldPayloadMark = "WITHHELD-PAYLOAD-MARK"

// withheldFrameCase is one frame the edge withholds from a session of PRIV on its own agent
// subject, and the reason it is withheld for.
type withheldFrameCase struct {
	name   string
	data   []byte
	reason string
}

// withheldFrameCases is a Dispatch frame of another project (rule 1), a Dispatch frame the edge
// cannot place in a project (rule 1's second reason), and a role-marked frame whose role mark is
// another project's (the role-mark rule), each carrying withheldPayloadMark in its payload.
func withheldFrameCases(t *testing.T) []withheldFrameCase {
	t.Helper()
	own := contracts.LegacyAgentSubject(payloadSession)
	pubEvent := routed(t, issueEvent("PUB", "comment.created", comment("PUB-1", withheldPayloadMark+" PUB's comment")), own, "")
	unplaced := []byte(payloadJSON(t, contracts.Envelope{EventID: "dispatch-45", Source: "dispatch", Topic: own, DedupeKey: "dispatch-45",
		Payload: `{"type":"comment.created","actor":{"kind":"user","id":"alice"},"issue_key":null,"payload":{"body":"` + withheldPayloadMark + `"}}`}))
	roleMarked := []byte(payloadJSON(t, contracts.Envelope{EventID: "agent-46", Source: "agent", Topic: own, DedupeKey: "agent.ses_v.k46",
		Role: "legion-pub-pub-1-tester", Payload: withheldPayloadMark + " PUB's role message"}))
	return []withheldFrameCase{
		{"a Dispatch frame of another project", pubEvent, withheldProjectNotHeld},
		{"a Dispatch frame the edge cannot place in a project", unplaced, withheldDispatchProjectUnread},
		{"a frame carrying another project's role mark", roleMarked, withheldProjectNotHeld},
	}
}

// withheldCount is envoy_edge_github_withheld_total{reason} as met's /metrics renders it, 0 for no
// series.
func withheldCount(t *testing.T, met *metrics.Registry, reason string) int64 {
	t.Helper()
	recorder := httptest.NewRecorder()
	met.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return max(counterValue(t, recorder.Body.String(), `envoy_edge_github_withheld_total{reason="`+reason+`"}`), 0)
}

// requireWithheldLine fails the test unless logged holds exactly one line at INFO whose message
// begins with message, naming session, subject and reason, and unless no line of logged holds
// withheldPayloadMark: a withheld frame is logged with its subject and reason and none of its
// content.
func requireWithheldLine(t *testing.T, logged, message, subject, reason string) {
	t.Helper()
	lines := edgeLogLines(logged, "INFO", message)
	if len(lines) != 1 {
		t.Fatalf("logged %d lines %q, want 1:\n%s", len(lines), message, logged)
	}
	if lines[0]["session_id"] != payloadSession || lines[0]["subject"] != subject || lines[0]["reason"] != reason {
		t.Errorf("the line names session %v, subject %v, reason %v; want %s, %s, %s", lines[0]["session_id"], lines[0]["subject"], lines[0]["reason"], payloadSession, subject, reason)
	}
	if strings.Contains(logged, withheldPayloadMark) {
		t.Errorf("the log holds bytes of the withheld frame's payload (%s):\n%s", withheldPayloadMark, logged)
	}
}

// TestAWithheldFrameIsLoggedWithItsSubjectAndReasonAndCounted holds rule 4's trace at the edge: a
// frame the edge withholds from a session is acknowledged undelivered, logged with the session, its
// subject and the reason, and none of its payload, and counted in envoy_edge_github_withheld_total
// under that reason, so a send withheld there leaves a trace the operator can find. It holds both
// places the edge withholds a frame: the session relay, whose loop answers a dropped frame
// (frameDrop) with acknowledgeWithheld, and the relay socket (relaySocket.forward). The frames are a
// Dispatch frame of another project, a Dispatch frame the edge cannot place in a project, and a frame
// carrying another project's role mark, each with a mark in its payload; the control is a Dispatch
// frame of the token's project, which passes, unlogged and uncounted.
func TestAWithheldFrameIsLoggedWithItsSubjectAndReasonAndCounted(t *testing.T) {
	t.Parallel()
	own := contracts.LegacyAgentSubject(payloadSession)
	control := routed(t, issueEvent("PRIV", "comment.created", comment("PRIV-1", withheldPayloadMark+" PRIV's comment")), own, "")

	t.Run("the session relay", func(t *testing.T) {
		for _, tc := range withheldFrameCases(t) {
			t.Run(tc.name, func(t *testing.T) {
				logs, met := &cmdtest.LockedBuffer{}, metrics.New()
				m := &sessionRelays{delivery: &sessionDelivery{}, logger: logging.NewWithWriter("test", logs),
					repositories: newEdgeRepositories("http://broker.example", listenerBrokerAccount(edgeAccountToken), []string{"PRIV"},
						newEdgeWithheldCounter(met), logging.NewWithWriter("test", logs))}
				m.delivery.admissions.admit(payloadSession, []string{"PRIV"})

				if verdict := m.edgeWithholds(payloadSession, bus.SessionMessage{Subject: own, Data: control, SessionAcker: nakAcker{}}); verdict != framePasses() {
					t.Fatalf("UNKNOWN: the control frame of the token's project = {action %d, reason %q}, want a pass", verdict.action, verdict.reason)
				}
				msg := bus.SessionMessage{Subject: own, Data: tc.data, SessionAcker: nakAcker{}}
				verdict := m.edgeWithholds(payloadSession, msg)
				if verdict.action != frameDrop || verdict.reason != tc.reason {
					t.Fatalf("UNKNOWN: edgeWithholds = {action %d, reason %q}, want a drop under %s", verdict.action, verdict.reason, tc.reason)
				}
				m.acknowledgeWithheld(payloadSession, msg, verdict.reason)

				requireWithheldLine(t, logs.String(), "edge listener withheld a frame from a session", own, tc.reason)
				if got := withheldCount(t, met, tc.reason); got != 1 {
					t.Errorf(`envoy_edge_github_withheld_total{reason=%q} = %d, want 1`, tc.reason, got)
				}
			})
		}
	})

	t.Run("the relay socket", func(t *testing.T) {
		_, uri := testnats.Start(t)
		client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
		if err != nil {
			t.Fatalf("connect bus: %v", err)
		}
		t.Cleanup(client.Close)
		frame := func(data []byte) *natsgo.Msg {
			return &natsgo.Msg{Subject: "relay." + payloadSession, Reply: bus.RelayReplyPrefix + "m1.n1." + payloadSession + ".t1",
				Header: natsgo.Header{relayHeaderSubject: []string{own}}, Data: data}
		}
		for _, tc := range withheldFrameCases(t) {
			t.Run(tc.name, func(t *testing.T) {
				logs, met := &cmdtest.LockedBuffer{}, metrics.New()
				socket := &relaySocket{sockets: &relaySockets{client: client, logger: logging.NewWithWriter("test", logs)}, session: payloadSession,
					admitted: ciphertoken.Caller{Projects: []string{"PRIV"}},
					repositories: newEdgeRepositories("http://broker.example", listenerBrokerAccount(edgeAccountToken), []string{"PRIV"},
						newEdgeWithheldCounter(met), logging.NewWithWriter("test", logs))}

				socket.forward(1, frame(tc.data))

				requireWithheldLine(t, logs.String(), "listener: the relay socket withheld a frame from its session", own, tc.reason)
				if line := edgeLogLines(logs.String(), "INFO", "listener: the relay socket withheld a frame from its session"); len(line) == 1 && line[0]["answer"] != relayReplyDone {
					t.Errorf("the socket answered %v, want %s: a withheld frame is acknowledged undelivered", line[0]["answer"], relayReplyDone)
				}
				if got := withheldCount(t, met, tc.reason); got != 1 {
					t.Errorf(`envoy_edge_github_withheld_total{reason=%q} = %d, want 1`, tc.reason, got)
				}
			})
		}
	})
}
