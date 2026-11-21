package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// inProcessRig is the listener's send and publish routes over a three-copy stream of a real
// cluster, with the listener connected to the stream's leader alone, so a test that freezes the
// members' routes (testnats.Cluster.FreezeRoutes) has the leader take every publish and store none
// until the thaw: a second publish under the same message id meanwhile is refused as still in
// process (nats-server's 10158, bus.ErrDuplicateInProcess), as a hub failover leaves a sender's
// resend. The sessions, the role claims and the role send's delivery configuration are on the
// package's shared server, so a frozen route holds the stream's publishes and nothing the routes
// read; the role send forwards through startRoleForward over the cluster's listener. Every
// message the rig sends is counted under its sender (senderLimit), and the routes' record lines
// are kept in logs.
type inProcessRig struct {
	cluster *testnats.Cluster
	js      natsgo.JetStreamContext
	leader  string
	// watch is a connection to the leader alone, on which a test hears every publish the leader
	// is handed.
	watch   *natsgo.Conn
	harness listenerDeliveryHarness
	d       *listenerDeps
	logs    *cmdtest.LockedBuffer
	logger  *logging.Logger
}

func newInProcessRig(t *testing.T) *inProcessRig {
	t.Helper()
	cluster := testnats.StartCluster(t, 3)
	owner, err := bus.ConnectOwningStream(cluster.URLs(), bus.WithReplicas(3))
	if err != nil {
		t.Fatalf("connect the listener that creates the three-copy stream: %v", err)
	}
	t.Cleanup(owner.Close)
	js := cluster.JetStream(t)
	leaderName := testnats.AwaitStreamCopies(t, js, bus.Stream, 3).Cluster.Leader
	leader := cluster.Member(leaderName)
	if leader < 0 {
		t.Fatalf("the stream's leader %q is no member of the cluster", leaderName)
	}
	client, err := bus.ConnectOwningStream([]string{cluster.Servers[leader].URL()}, bus.WithReplicas(3))
	if err != nil {
		t.Fatalf("connect the listener to the stream's leader %s: %v", leaderName, err)
	}
	t.Cleanup(client.Close)
	harness := newListenerDeliveryHarness(t)
	delivery := harness.config
	delivery.forwardRole = startRoleForward(client)
	logs := &cmdtest.LockedBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("route log:\n%s", logs.String())
		}
	})
	watch := testnats.Connect(t, cluster.Servers[leader].URL())
	t.Cleanup(watch.Close)
	return &inProcessRig{
		cluster: cluster,
		js:      js,
		leader:  leaderName,
		watch:   watch,
		harness: harness,
		d: &listenerDeps{
			client:      client,
			registry:    harness.registry,
			sessions:    harness.sessions,
			delivery:    &delivery,
			confirmed:   newConfirmedRecord(t),
			senderLimit: newSenderLimit(60, 8, func() time.Time { return senderLimitStart }),
		},
		logs:   logs,
		logger: logging.NewWithWriter("test", logs),
	}
}

// inProcessPath is one listener path that publishes under a message id its request's
// idempotency_key names: the route it is sent through, the subject its one copy is stored on, and
// the request's body for the key.
type inProcessPath struct {
	name    string
	route   string
	subject string
	body    func(sender, key string) map[string]any
}

// line is the name of the route's record line.
func (p inProcessPath) line() string {
	if p.route == "/v1/messages/send" {
		return "listener send"
	}
	return "listener publish"
}

// inProcessPaths are the publish route to a team topic and to a topic the stream captures for a
// source, and the send route to target, a session the rig registers live.
func inProcessPaths(target string) []inProcessPath {
	const team, captured = "notifications.team.in-process", "notifications.github.legion-test.in_process.pr.1"
	return []inProcessPath{
		{"publish to a team topic", "/v1/messages/publish", team, func(sender, key string) map[string]any {
			return map[string]any{"topic": team, "source": "agent", "source_session": sender, "message": "team, resent while in process", "idempotency_key": key}
		}},
		{"publish to a captured topic", "/v1/messages/publish", captured, func(sender, key string) map[string]any {
			return map[string]any{"topic": captured, "source": "agent", "source_session": sender, "message": "captured, resent while in process", "idempotency_key": key}
		}},
		{"send to a session", "/v1/messages/send", contracts.LegacyAgentSubject(target), func(sender, key string) map[string]any {
			return map[string]any{"target_session": target, "source": "agent", "source_session": sender, "message": "send, resent while in process", "idempotency_key": key}
		}},
	}
}

// requireLeader fails unless the stream's leader is still the member the rig's listener is
// connected to.
func (rig *inProcessRig) requireLeader(t *testing.T) {
	t.Helper()
	if now := testnats.AwaitStreamCopies(t, rig.js, bus.Stream, 3).Cluster.Leader; now != rig.leader {
		t.Fatalf("the stream's leader moved from %s to %s, and the rig's listener is connected to %s alone", rig.leader, now, rig.leader)
	}
}

// hear subscribes to subject on the leader, so the test hears each publish the leader is handed.
func (rig *inProcessRig) hear(t *testing.T, subject string) *natsgo.Subscription {
	t.Helper()
	heard, err := rig.watch.SubscribeSync(subject)
	if err != nil {
		t.Fatalf("subscribe to %s on the leader: %v", subject, err)
	}
	t.Cleanup(func() { _ = heard.Unsubscribe() })
	if err := rig.watch.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	return heard
}

// awaitHanded waits until the leader has been handed n publishes on heard's subject.
func awaitHanded(t *testing.T, heard *natsgo.Subscription, n int) {
	t.Helper()
	for i := range n {
		if _, err := heard.NextMsg(10 * time.Second); err != nil {
			t.Fatalf("the leader was handed %d of the %d publishes: %v", i, n, err)
		}
	}
}

// serve sends body to route, the publish route or the send route, under ctx, and returns its
// answer.
func (rig *inProcessRig) serve(ctx context.Context, route, body string) *httptest.ResponseRecorder {
	handler := publishHandler(rig.d, rig.logger)
	if route == "/v1/messages/send" {
		handler = sendHandler(rig.d, rig.logger)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, route, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	return recorder
}

// requireOneCopy waits until the stream holds subject's one copy, and fails on any other count.
func (rig *inProcessRig) requireOneCopy(t *testing.T, subject string) {
	t.Helper()
	copies := func() uint64 {
		info, err := rig.js.StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
		if err != nil {
			return 0
		}
		return info.State.Subjects[subject]
	}
	waitFor(t, 10*time.Second, "the stream to store the message on "+subject, func() bool { return copies() >= 1 })
	if got := copies(); got != 1 {
		t.Fatalf("the stream holds %d copies on %s, want 1: both attempts carry one message id", got, subject)
	}
}

// lines is every record line route wrote for a request from sender.
func (rig *inProcessRig) lines(path inProcessPath, sender string) []map[string]any {
	return linesIn(rig.logs.String(), path.line(), map[string]any{"source_session": sender})
}

// A resend that reaches a three-copy stream while its earlier attempt is still being stored is
// refused by the stream as a publish whose message id is still in process: the leader holds the
// id's dedupe entry with no sequence yet. The listener publishes again until the earlier attempt
// is stored and its answer names that copy (publishPastInProcess), so both attempts are answered
// 200 and the stream holds one copy. It is never answered 503 bus_unavailable, "the message bus
// did not answer": the bus answered, and the sender would be told its message was not taken
// when its first attempt was. The copy costs its sender one, and the attempt the stream answered
// with the other's copy costs nothing (senderCharge.published).
//
// The rows are each path of the listener that publishes under a message id the sender's
// idempotency key names: the publish route to a team topic and to a topic the stream captures,
// the send route to a session, and the publish route's send to a role's live holder, whose
// forward carries one Nats-Msg-Id on every attempt (startRoleForward). The two attempts go out
// while the members' routes are frozen, and the thaw comes once the leader has been handed both.
func TestAResendWhoseEarlierAttemptIsStillInProcessIsAnsweredOnceThatAttemptIsStored(t *testing.T) {
	rig := newInProcessRig(t)
	const target, holder = "ses_in_process_target", "ses_in_process_holder"
	if err := rig.harness.sessions.Put(target, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s: %v", target, err)
	}
	role := "in-process-" + id.New()[:8]
	claimRoleFor(t, rig.harness.registry, rig.harness.sessions, holder, role, nil)
	paths := append(inProcessPaths(target), inProcessPath{"send to a role's live holder", "/v1/messages/publish", contracts.LegacyAgentSubject(holder),
		func(sender, key string) map[string]any {
			return map[string]any{"topic": contracts.RoleTopicPrefix + role, "source": "agent", "source_session": sender, "message": "role, resent while in process", "idempotency_key": key}
		}})
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			rig.requireLeader(t)
			heard := rig.hear(t, path.subject)
			sender := "ses_in_process_" + id.New()[:8]
			body := messageJSON(t, path.body(sender, id.New()), nil)
			thaw := rig.cluster.FreezeRoutes(t)
			t.Cleanup(thaw)
			answers := make(chan *httptest.ResponseRecorder, 2)
			for range 2 {
				go func() { answers <- rig.serve(context.Background(), path.route, body) }()
			}
			awaitHanded(t, heard, 2)
			thaw()
			for range 2 {
				if answer := <-answers; answer.Code != http.StatusOK {
					t.Errorf("an attempt was answered %d %s; want 200: its earlier attempt was still in process, and is stored", answer.Code, strings.TrimSpace(answer.Body.String()))
				}
			}
			rig.requireOneCopy(t, path.subject)
			lines := rig.lines(path, sender)
			named := 0
			for _, line := range lines {
				if line["sender"] == sender {
					named++
				}
			}
			if len(lines) != 2 || named != 1 {
				t.Errorf("the attempts' record lines are %v; want two, one naming sender %s: the stored copy costs one, and the attempt answered with it nothing", lines, sender)
			}
		})
	}
}

// inProcessBudget is the request deadline the rows past the route's budget send under. The
// route's budget is the earlier of its own (sendRequestBudget) and the request's, so this ends it
// early, inside the members' election timeout: a freeze that outlasted it would let a follower
// take the stream over and drop the leader's uncommitted publish.
const inProcessBudget = time.Second

// A request whose budget ends while the stream still refuses its publish as in process under an
// earlier attempt of the same message is answered 503 with Retry-After and code maybe_stored,
// under earlierAttemptInProcessPrefix and the outcome earlier_attempt_in_process: the stream may
// yet store that earlier attempt, and it does once the members hear each other again. The earlier
// attempt, whose own acknowledgement never came, is answered maybe_stored too, and costs its
// sender one; the later one costs nothing, since the copy is the earlier one's.
//
// The rows are the paths of the route test above whose publish answers the route directly. The
// earlier attempt goes out first, the later one once the leader has it, and the thaw comes once
// both are answered.
func TestAResendWhoseEarlierAttemptIsStillInProcessWhenItsBudgetEndsIsAnsweredMaybeStored(t *testing.T) {
	rig := newInProcessRig(t)
	const target = "ses_in_process_budget_target"
	if err := rig.harness.sessions.Put(target, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s: %v", target, err)
	}
	for _, path := range inProcessPaths(target) {
		t.Run(path.name, func(t *testing.T) {
			rig.requireLeader(t)
			heard := rig.hear(t, path.subject)
			sender := "ses_in_process_budget_" + id.New()[:8]
			body := messageJSON(t, path.body(sender, id.New()), nil)
			thaw := rig.cluster.FreezeRoutes(t)
			t.Cleanup(thaw)
			attempt := func() <-chan *httptest.ResponseRecorder {
				answer := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), inProcessBudget)
					defer cancel()
					answer <- rig.serve(ctx, path.route, body)
				}()
				return answer
			}
			first := attempt()
			awaitHanded(t, heard, 1)
			later := attempt()
			earlier, resend := <-first, <-later
			thaw()
			for _, answer := range []struct {
				name     string
				recorder *httptest.ResponseRecorder
			}{{"the earlier attempt", earlier}, {"the resend", resend}} {
				var body listenerwire.APIError
				if err := json.Unmarshal(answer.recorder.Body.Bytes(), &body); err != nil {
					t.Fatalf("%s: decode %q: %v", answer.name, answer.recorder.Body.String(), err)
				}
				if answer.recorder.Code != http.StatusServiceUnavailable || body.Code != listenerwire.CodeMaybeStored || answer.recorder.Header().Get("Retry-After") != "1" {
					t.Errorf("%s was answered %d, Retry-After %q, %s; want 503, Retry-After 1 and code %s: the stream may hold the message",
						answer.name, answer.recorder.Code, answer.recorder.Header().Get("Retry-After"), strings.TrimSpace(answer.recorder.Body.String()), listenerwire.CodeMaybeStored)
				}
				if answer.name == "the resend" && !strings.HasPrefix(body.Error, earlierAttemptInProcessPrefix+": ") {
					t.Errorf("the resend's error is %q, want it to open with %q", body.Error, earlierAttemptInProcessPrefix)
				}
			}
			rig.requireOneCopy(t, path.subject)
			lines := rig.lines(path, sender)
			want := []string{
				fmt.Sprintf("%s %s %v", outcomeBusUnavailable, sender, true),
				fmt.Sprintf("%s %s %v", outcomeEarlierAttemptInProcess, "", false),
			}
			var got []string
			for _, line := range lines {
				named, _ := line["sender"].(string)
				got = append(got, fmt.Sprintf("%v %s %v", line["outcome"], named, named != ""))
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("the attempts' record lines read %q (outcome, sender named, named), want %q: the earlier attempt may have stored the copy and costs one, and the resend, whose copy is the earlier one's, costs nothing; lines %v", got, want, lines)
			}
		})
	}
}

// A takeover's copy of a frame onto the role's kept lane publishes again while its message id is
// still in process under another publish (bus.ErrDuplicateInProcess), under the id it refused,
// until an answer names the copy, as the keep's own does (publishPastInProcess): a copy refused
// twice and then stored is a copy made, under takenFrameMsgID each time.
func TestATakeoverCopyPublishesAgainWhileItsMessageIDIsStillInProcess(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	role := "taken-in-process-" + id.New()[:8]
	inProcess := fmt.Errorf("%w: %w", bus.ErrDuplicateInProcess,
		&natsgo.APIError{Code: 409, ErrorCode: 10158, Description: "duplicate message id is in process"})
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, contracts.RoleForwardDedupePrefix+"taken-in-process-"+id.New())
	const seq = 7
	cfg := harness.config
	var ids []string
	cfg.keepPublish = func(ctx context.Context, subject string, envelope contracts.Envelope, msgID string) (bus.StoredAck, error) {
		ids = append(ids, msgID)
		if len(ids) <= 2 {
			return bus.StoredAck{}, inProcess
		}
		return harness.client.PublishStoredAck(ctx, subject, envelope, msgID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), roleKeepPublishWindow)
	defer cancel()
	if err := keepTakenRoleFrame(ctx, cfg, role, item, seq); err != nil {
		t.Fatalf("a takeover copy whose first two publishes were refused as in process: %v; want it copied by the third", err)
	}
	msgID := takenFrameMsgID(strings.TrimPrefix(item.DedupeKey, contracts.RoleForwardDedupePrefix), role, seq)
	if !slices.Equal(ids, []string{msgID, msgID, msgID}) {
		t.Fatalf("the copy was published under %v, want %s three times: the in-process refusal is retried under the id it refused", ids, msgID)
	}
	if stored, _, ok := readKeptRoleCopy(t, harness.client, role); !ok || stored.EventID != item.EventID {
		t.Fatalf("the lane holds %+v (held: %t), want event %s", stored, ok, item.EventID)
	}
}
