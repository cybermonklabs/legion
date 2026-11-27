package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
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

// sendHoldKey is the request context key of a sendHold.
type sendHoldKey struct{}

// sendHold is what a test does at each send of one request's stored publish: the request's context
// carries it, and the rig listener's acknowledgement clock (heldSendClock) calls started as each
// send begins, before anything is sent, and ended once that send has returned its answer, each
// with the send's number from 1. A route's publish makes one send per call of
// bus.Client.PublishStoredAck, which takes its acknowledgement bound from that clock.
type sendHold struct {
	sends   atomic.Int32
	started func(n int32)
	ended   func(n int32)
}

// heldSendClock is the rig listener's acknowledgement clock (bus.WithPublishAcknowledgementClock):
// the bound every send takes, as the wall clock gives it, and the hold of a request whose context
// carries one (sendHold).
type heldSendClock struct{}

func (heldSendClock) WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	hold, _ := parent.Value(sendHoldKey{}).(*sendHold)
	if hold == nil {
		return ctx, cancel
	}
	n := hold.sends.Add(1)
	if hold.started != nil {
		hold.started(n)
	}
	return ctx, func() {
		cancel()
		if hold.ended != nil {
			hold.ended(n)
		}
	}
}

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
	leader  int
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
	client, err := bus.ConnectOwningStream([]string{cluster.Servers[leader].URL()}, bus.WithReplicas(3),
		bus.WithPublishAcknowledgementClock(heldSendClock{}))
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
		leader:  leader,
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
// idempotency_key names: the route it is sent through, the subject its one copy is stored on, the
// request's body for the key, and whether it is a role send, whose forward carries the id.
type inProcessPath struct {
	name    string
	route   string
	subject string
	body    func(sender, key string) map[string]any
	role    bool
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
		{name: "publish to a team topic", route: "/v1/messages/publish", subject: team, body: func(sender, key string) map[string]any {
			return map[string]any{"topic": team, "source": "agent", "source_session": sender, "message": "team, resent while in process", "idempotency_key": key}
		}},
		{name: "publish to a captured topic", route: "/v1/messages/publish", subject: captured, body: func(sender, key string) map[string]any {
			return map[string]any{"topic": captured, "source": "agent", "source_session": sender, "message": "captured, resent while in process", "idempotency_key": key}
		}},
		{name: "send to a session", route: "/v1/messages/send", subject: contracts.LegacyAgentSubject(target), body: func(sender, key string) map[string]any {
			return map[string]any{"target_session": target, "source": "agent", "source_session": sender, "message": "send, resent while in process", "idempotency_key": key}
		}},
	}
}

// roleInProcessPath is the publish route's send to role, whose live holder is holder: its forward
// is stored on the holder's agent subject.
func roleInProcessPath(role, holder string) inProcessPath {
	return inProcessPath{name: "send to a role's live holder", route: "/v1/messages/publish", subject: contracts.LegacyAgentSubject(holder), role: true,
		body: func(sender, key string) map[string]any {
			return map[string]any{"topic": contracts.RoleTopicPrefix + role, "source": "agent", "source_session": sender, "message": "role, resent while in process", "idempotency_key": key}
		}}
}

// requireLeader fails unless the stream's leader is still the member the rig's listener is
// connected to.
func (rig *inProcessRig) requireLeader(t *testing.T) {
	t.Helper()
	if now := testnats.AwaitStreamCopies(t, rig.js, bus.Stream, 3).Cluster.Leader; now != rig.cluster.Name(rig.leader) {
		t.Fatalf("the stream's leader moved from %s to %s, and the rig's listener is connected to %s alone", rig.cluster.Name(rig.leader), now, rig.cluster.Name(rig.leader))
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

// attempt serves body to route under ctx on a goroutine of its own, and returns where its answer
// arrives.
func (rig *inProcessRig) attempt(ctx context.Context, route, body string) <-chan *httptest.ResponseRecorder {
	answer := make(chan *httptest.ResponseRecorder, 1)
	go func() { answer <- rig.serve(ctx, route, body) }()
	return answer
}

// copies is how many copies the stream holds on subject, failing the test on a read that did not
// answer.
func (rig *inProcessRig) copies(t *testing.T, subject string) uint64 {
	t.Helper()
	info, err := rig.js.StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		t.Fatalf("read the stream's copies on %s: %v", subject, err)
	}
	return info.State.Subjects[subject]
}

// requireOneCopy waits until the stream holds one copy on subject more than before, the count
// the row read before it sent anything, and fails on any other count.
func (rig *inProcessRig) requireOneCopy(t *testing.T, subject string, before uint64) {
	t.Helper()
	waitFor(t, "the stream to store the message on "+subject, func() bool { return rig.copies(t, subject) > before })
	if got := rig.copies(t, subject) - before; got != 1 {
		t.Fatalf("the stream stored %d copies on %s, want 1: both attempts carry one message id", got, subject)
	}
}

// outcomes is each record line route wrote for a request from sender, as "<outcome> charged" or
// "<outcome> free": charged where the line names the sender, which is a message that cost it one.
func (rig *inProcessRig) outcomes(path inProcessPath, sender string) []string {
	var got []string
	for _, line := range linesIn(rig.logs.String(), path.line(), map[string]any{"source_session": sender}) {
		cost := "free"
		if line["sender"] == sender {
			cost = "charged"
		}
		got = append(got, fmt.Sprintf("%v %s", line["outcome"], cost))
	}
	slices.Sort(got)
	return got
}

// requireOutcomes fails unless the record lines of sender's requests are want, in any order.
func (rig *inProcessRig) requireOutcomes(t *testing.T, path inProcessPath, sender string, want ...string) {
	t.Helper()
	slices.Sort(want)
	if got := rig.outcomes(path, sender); !slices.Equal(got, want) {
		t.Errorf("the attempts' record lines read %q, want %q", got, want)
	}
}

// forwardAttempts is the attempts of each role send delivery line for the dedupe key key.
func (rig *inProcessRig) forwardAttempts(key string) []float64 {
	var attempts []float64
	for _, line := range linesIn(rig.harness.logs.String(), "listener publish role send", map[string]any{"dedupe_key": publishDedupePrefix + key}) {
		n, _ := line["attempts"].(float64)
		attempts = append(attempts, n)
	}
	return attempts
}

// A resend that reaches a three-copy stream while its earlier attempt is still being stored is
// refused by the stream as a publish whose message id is still in process: the leader holds the
// id's dedupe entry with no sequence yet. The listener publishes again until the earlier attempt
// is stored and its answer names that copy (publishPastInProcess), so both attempts are answered
// 200 and the stream holds one copy. It is never answered 503 bus_unavailable, "the message bus
// did not answer": the bus answered, and the sender would be told its message was not taken
// when its first attempt was.
//
// The rows are each path of the listener that publishes under a message id the sender's
// idempotency key names: the publish route to a team topic and to a topic the stream captures,
// the send route to a session, and the publish route's send to a role's live holder, whose
// forward carries one Nats-Msg-Id on every attempt (startRoleForward). The two attempts go out
// while the members' routes are frozen, and the thaw comes once the leader has been handed both.
//
// The charge is each route's own rule. The send and the topic publish charge the stored copy and
// forgive the attempt the stream answered with it (senderCharge.published). A role send charges
// every confirmed forward, a duplicate's too (sendToRole), and the refused forward is sent again
// inside that one forward, so each role send makes one attempt, where a refused forward counted
// as one that failed makes a second.
func TestAResendWhoseEarlierAttemptIsStillInProcessIsAnsweredOnceThatAttemptIsStored(t *testing.T) {
	t.Parallel()
	rig := newInProcessRig(t)
	const target, holder = "ses_in_process_target", "ses_in_process_holder"
	if err := rig.harness.sessions.Put(target, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s: %v", target, err)
	}
	role := "in-process-" + id.New()[:8]
	claimRoleFor(t, rig.harness.registry, rig.harness.sessions, holder, role, nil)
	for _, path := range append(inProcessPaths(target), roleInProcessPath(role, holder)) {
		t.Run(path.name, func(t *testing.T) {
			rig.requireLeader(t)
			heard, before := rig.hear(t, path.subject), rig.copies(t, path.subject)
			sender, key := "ses_in_process_"+id.New()[:8], id.New()
			body := messageJSON(t, path.body(sender, key), nil)
			thaw := rig.cluster.FreezeRoutes(t)
			t.Cleanup(thaw)
			answers := []<-chan *httptest.ResponseRecorder{
				rig.attempt(context.Background(), path.route, body),
				rig.attempt(context.Background(), path.route, body),
			}
			awaitHanded(t, heard, 2)
			thaw()
			for _, answer := range answers {
				if answer := <-answer; answer.Code != http.StatusOK {
					t.Errorf("an attempt was answered %d %s; want 200: its earlier attempt was still in process, and is stored", answer.Code, strings.TrimSpace(answer.Body.String()))
				}
			}
			rig.requireOneCopy(t, path.subject, before)
			if !path.role {
				rig.requireOutcomes(t, path, sender, "accepted charged", "accepted free")
				return
			}
			rig.requireOutcomes(t, path, sender, "accepted charged", "accepted charged")
			if got := rig.forwardAttempts(key); !slices.Equal(got, []float64{1, 1}) {
				t.Errorf("the role sends' delivery lines name attempts %v, want [1 1]: a forward refused as in process is sent again inside that forward, not counted as one that failed", got)
			}
		})
	}
}

// inProcessForwardWindow is the window of the one forward TestARoleResendWhoseFirstForwardEndsInProcessIsAnsweredByItsNextForward
// shortens: inside the members' election timeout, as the freeze that holds it is.
const inProcessForwardWindow = 300 * time.Millisecond

// A role resend whose first forward's window ends while the earlier send's forward is still in
// process gets the stored copy from its next forward. The first forward is sent again while the
// stream refuses it as in process, until its window ends (publishPastInProcess), so it ends no
// sooner than that window, as a forward that ended while an earlier forward was in process
// (earlierAttemptInProcess). That error wraps the refusal when the window's end stops the pause,
// and the failed send when it stops a send the pause let go past it; the answer and the charge are
// the same either way. It failed with no refusal, so it costs the sender one, as every such
// forward does, and the next forward, answered Duplicate once the earlier forward is stored, is the
// resend's 200. Both sends are charged by main's rule for a role send (sendToRole), and the stream
// holds one forward.
//
// The rig's forward seam shortens the resend's first forward to inProcessForwardWindow, so its
// window ends while the routes are frozen, and the thaw comes once it has ended.
func TestARoleResendWhoseFirstForwardEndsInProcessIsAnsweredByItsNextForward(t *testing.T) {
	t.Parallel()
	rig := newInProcessRig(t)
	const holder = "ses_in_process_next_holder"
	role := "in-process-next-" + id.New()[:8]
	claimRoleFor(t, rig.harness.registry, rig.harness.sessions, holder, role, nil)
	path := roleInProcessPath(role, holder)
	type ended struct {
		err  error
		took time.Duration
	}
	first := make(chan ended, 1)
	var forwards atomic.Int32
	forward := rig.d.delivery.forwardRole
	rig.d.delivery.forwardRole = func(subject string, item contracts.Envelope, msgID string, timeout time.Duration) (func() (bus.StoredAck, error), error) {
		n := forwards.Add(1)
		if n != 2 {
			return forward(subject, item, msgID, timeout)
		}
		started := time.Now()
		wait, err := forward(subject, item, msgID, inProcessForwardWindow)
		if err != nil {
			first <- ended{err, time.Since(started)}
			return nil, err
		}
		return func() (bus.StoredAck, error) {
			ack, err := wait()
			first <- ended{err, time.Since(started)}
			return ack, err
		}, nil
	}
	rig.requireLeader(t)
	heard, before := rig.hear(t, path.subject), rig.copies(t, path.subject)
	sender, key := "ses_in_process_next_"+id.New()[:8], id.New()
	body := messageJSON(t, path.body(sender, key), nil)
	thaw := rig.cluster.FreezeRoutes(t)
	t.Cleanup(thaw)
	earlier := rig.attempt(context.Background(), path.route, body)
	awaitHanded(t, heard, 1)
	resend := rig.attempt(context.Background(), path.route, body)
	var firstForward ended
	select {
	case firstForward = <-first:
	case <-time.After(listenerWaitBound(t)):
		t.Fatal("the resend's first forward did not end")
	}
	thaw()
	for name, answer := range map[string]<-chan *httptest.ResponseRecorder{"the earlier send": earlier, "the resend": resend} {
		if answer := <-answer; answer.Code != http.StatusOK {
			t.Errorf("%s was answered %d %s; want 200", name, answer.Code, strings.TrimSpace(answer.Body.String()))
		}
	}
	if firstForward.took < inProcessForwardWindow || !earlierAttemptInProcess(firstForward.err) {
		t.Errorf("the resend's first forward ended after %s with %v; want its whole window of %s, ending as a forward whose earlier forward was still in process: a refused forward is sent again until its window ends",
			firstForward.took, firstForward.err, inProcessForwardWindow)
	}
	if n := forwards.Load(); n != 3 {
		t.Errorf("the two sends made %d forwards, want 3: the earlier send's, and the resend's first and next", n)
	}
	rig.requireOneCopy(t, path.subject, before)
	rig.requireOutcomes(t, path, sender, "accepted charged", "accepted charged")
	if got := rig.forwardAttempts(key); !slices.Contains(got, 2) {
		t.Errorf("the role sends' delivery lines name attempts %v, want the resend's 2", got)
	}
}

// inProcessBudget is the request deadline a request whose budget ends in process is sent under.
// The route's budget is the earlier of its own (sendRequestBudget, publishDeliveryWindow for a role
// send) and the request's, so this ends it early, inside the members' election timeout: a freeze
// that outlasted it would let a follower take the stream over and drop the leader's uncommitted
// publish.
const inProcessBudget = time.Second

// A request whose budget ends while the stream still refuses its publish as in process under an
// earlier attempt of the same message is answered 503 with Retry-After and code maybe_stored,
// under earlierAttemptInProcessPrefix and the outcome earlier_attempt_in_process: the stream may
// yet store that earlier attempt, and it does once the members hear each other again. It is never
// the 503 "the message bus did not answer", nor a role send's 504 "was not stored". The earlier
// attempt, whose own acknowledgement never came, is answered as main answers it: 503 maybe_stored
// for the send and the topic publish, 504 for a role send, charged.
//
// What the resend costs turns on its own sends. A resend whose every send was answered, the last
// one refused, before its budget ended stored nothing of its own and costs nothing: the rig holds
// its sends (sendHold) and ends its context once its second send has returned its refusal. A
// resend whose last send went out unanswered may have stored the copy itself, since a stream
// leader change drops the ids in process and the next send under one is stored, so it costs one:
// the rig freezes the leader's client relay as its second send begins, and the request's deadline
// ends that send. A role send whose window ends while its forward is still in process costs one,
// as main charges its 504 (sendToRole).
func TestAResendWhoseEarlierAttemptIsStillInProcessWhenItsBudgetEndsIsAnsweredMaybeStored(t *testing.T) {
	t.Parallel()
	rig := newInProcessRig(t)
	const target, holder = "ses_in_process_budget_target", "ses_in_process_budget_holder"
	if err := rig.harness.sessions.Put(target, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s: %v", target, err)
	}
	role := "in-process-budget-" + id.New()[:8]
	claimRoleFor(t, rig.harness.registry, rig.harness.sessions, holder, role, nil)
	// A resend's case is the context it is sent under, which ends its budget, the answer and
	// outcome of the earlier attempt, and what the resend costs.
	type resendCase struct {
		name    string
		context func(t *testing.T, thaws *[]func()) context.Context
		cost    string
	}
	betweenSends := resendCase{"its every send answered", func(t *testing.T, _ *[]func()) context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		return context.WithValue(ctx, sendHoldKey{}, &sendHold{ended: func(n int32) {
			if n == 2 {
				cancel()
			}
		}})
	}, "free"}
	lastUnanswered := resendCase{"its last send unanswered", func(t *testing.T, thaws *[]func()) context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), inProcessBudget)
		t.Cleanup(cancel)
		leader := rig.cluster.Servers[rig.leader]
		return context.WithValue(ctx, sendHoldKey{}, &sendHold{started: func(n int32) {
			if n == 2 {
				*thaws = append(*thaws, leader.Freeze(t))
			}
		}})
	}, "charged"}
	windowEnds := resendCase{"its window ends with its forward in process", func(t *testing.T, _ *[]func()) context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), inProcessBudget)
		t.Cleanup(cancel)
		return ctx
	}, "charged"}
	type row struct {
		path   inProcessPath
		resend resendCase
	}
	var rows []row
	for _, path := range inProcessPaths(target) {
		rows = append(rows, row{path, betweenSends}, row{path, lastUnanswered})
	}
	rows = append(rows, row{roleInProcessPath(role, holder), windowEnds})
	for _, row := range rows {
		t.Run(row.path.name+"/"+row.resend.name, func(t *testing.T) {
			rig.requireLeader(t)
			heard, before := rig.hear(t, row.path.subject), rig.copies(t, row.path.subject)
			sender := "ses_in_process_budget_" + id.New()[:8]
			body := messageJSON(t, row.path.body(sender, id.New()), nil)
			thaws := []func(){rig.cluster.FreezeRoutes(t)}
			t.Cleanup(func() {
				for _, thaw := range thaws {
					thaw()
				}
			})
			earlierCtx, cancel := context.WithTimeout(context.Background(), inProcessBudget)
			defer cancel()
			first := rig.attempt(earlierCtx, row.path.route, body)
			awaitHanded(t, heard, 1)
			later := rig.attempt(row.resend.context(t, &thaws), row.path.route, body)
			resend, earlier := <-later, <-first
			for _, thaw := range slices.Backward(thaws) {
				thaw()
			}
			earlierStatus, earlierOutcome := http.StatusServiceUnavailable, outcomeBusUnavailable
			if row.path.role {
				earlierStatus, earlierOutcome = http.StatusGatewayTimeout, outcomeDeliveryFailed
			}
			if earlier.Code != earlierStatus {
				t.Errorf("the earlier attempt was answered %d %s, want %d", earlier.Code, strings.TrimSpace(earlier.Body.String()), earlierStatus)
			}
			var body503 listenerwire.APIError
			if err := json.Unmarshal(resend.Body.Bytes(), &body503); err != nil {
				t.Fatalf("the resend: decode %q: %v", resend.Body.String(), err)
			}
			if resend.Code != http.StatusServiceUnavailable || body503.Code != listenerwire.CodeMaybeStored || resend.Header().Get("Retry-After") != "1" {
				t.Errorf("the resend was answered %d, Retry-After %q, %s; want 503, Retry-After 1 and code %s: the stream may hold the message",
					resend.Code, resend.Header().Get("Retry-After"), strings.TrimSpace(resend.Body.String()), listenerwire.CodeMaybeStored)
			}
			if !strings.HasPrefix(body503.Error, earlierAttemptInProcessPrefix+": ") {
				t.Errorf("the resend's error is %q, want it to open with %q", body503.Error, earlierAttemptInProcessPrefix)
			}
			rig.requireOneCopy(t, row.path.subject, before)
			rig.requireOutcomes(t, row.path, sender, string(earlierOutcome)+" charged", string(outcomeEarlierAttemptInProcess)+" "+row.resend.cost)
		})
	}
}

// A takeover's copy of a frame onto the role's kept lane publishes again while its message id is
// still in process under another publish (bus.ErrDuplicateInProcess), under the id it refused,
// until an answer names the copy, as the keep's own does (publishPastInProcess): a copy refused
// twice and then stored is a copy made, under takenFrameMsgID each time.
func TestATakeoverCopyPublishesAgainWhileItsMessageIDIsStillInProcess(t *testing.T) {
	t.Parallel()
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

// A delivery exception a clustered stream refuses as still in process under another listener's
// publish of it (nats-server's 10158, bus.ErrDuplicateInProcess) is no failed publish: that
// listener raised the same exception from its own durable under the same Nats-Msg-Id,
// exception.<reason>.<origin> (publishDeliveryException), and its publish stores the one copy. So
// it is logged at WARN, never the line the deployed publish-failure filter counts,
// `{ $.level = "ERROR" && $.msg = "listener exception publish failed" }`
// (packages/envoy/infra/aws/rendering/metricfilters.go), while a failure of the same publish, an
// exception past the server's max payload, is still logged that line. The in-process row freezes
// the members' routes, waits until the leader holds the other listener's publish, and raises the
// exception through applyDeliveryOutcome before the thaw, so the refusal comes by events.
func TestADeliveryExceptionStillInProcessIsNotLoggedAsAFailedPublish(t *testing.T) {
	t.Parallel()
	rig := newInProcessRig(t)
	const topic, recipient, reason = "notifications.team.exception-in-process", "ses_exception_in_process", "delivery_failed"
	lane := bus.ExceptionTopicPrefix + topic
	for _, row := range []struct {
		name      string
		inProcess bool
	}{
		{"refused as still in process under another listener's publish", true},
		{"past the server's max payload", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			logs := &cmdtest.LockedBuffer{}
			cfg := *rig.d.delivery
			cfg.client = rig.d.client
			cfg.logger = logging.NewWithWriter("test", logs)
			item := listenerTestEnvelope(topic, "exception-in-process-"+id.New())
			origin := "exception-in-process-origin-" + id.New()
			outcome := deliveryOutcome{sessionID: recipient, exceptionReason: reason, fanoutRefusal: true, origin: origin}
			want := bus.ErrTooLarge
			if row.inProcess {
				want = bus.ErrDuplicateInProcess
				heard := rig.hear(t, lane)
				before := rig.copies(t, lane)
				thaw := rig.cluster.FreezeRoutes(t)
				t.Cleanup(thaw)
				other := make(chan error, 1)
				go func() { other <- publishDeliveryException(rig.d.client, item, reason, recipient, origin) }()
				awaitHanded(t, heard, 1)
				applyDeliveryOutcome(cfg, item, outcome)
				thaw()
				if err := <-other; err != nil {
					t.Fatalf("the other listener's publish of the exception: %v; want it stored", err)
				}
				rig.requireOneCopy(t, lane, before)
			} else {
				item.Payload = strings.Repeat("p", int(rig.d.client.Conn.MaxPayload()))
				applyDeliveryOutcome(cfg, item, outcome)
			}
			var counted, refusals []string
			for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
				var record map[string]any
				if json.Unmarshal([]byte(line), &record) != nil || record["topic"] != topic {
					continue
				}
				if record["level"] == "ERROR" && record["msg"] == "listener exception publish failed" {
					counted = append(counted, line)
				}
				if text, _ := record["error"].(string); strings.Contains(text, want.Error()) {
					refusals = append(refusals, line)
				}
			}
			if len(refusals) != 1 {
				t.Fatalf("logged %d lines naming %q, want 1:\n%s", len(refusals), want, logs.String())
			}
			if row.inProcess && len(counted) != 0 {
				t.Fatalf("an exception still in process under another listener's publish logged the line the publish-failure filter counts:\n%s", strings.Join(counted, "\n"))
			}
			if !row.inProcess && (len(counted) != 1 || counted[0] != refusals[0]) {
				t.Fatalf("an exception the server refused as too large logged %d lines the publish-failure filter counts, want its one failure line:\n%s", len(counted), logs.String())
			}
		})
	}
}
