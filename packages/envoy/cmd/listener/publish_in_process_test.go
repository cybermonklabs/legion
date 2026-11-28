package main

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// sendHoldKey is the request context key of a sendHold.
type sendHoldKey struct{}

// sendHold is what a test does at each send of one request's stored publish: the request's context
// carries it, and the rig listener's acknowledgement clock (heldSendClock) calls ended once
// that send has returned its answer, with the send's number from 1.
type sendHold struct {
	clock *clocktest.Fake
	sends atomic.Int32
	ended func(n int32)
}

// heldSendClock uses each timed request's fake for its acknowledgement bounds. It also
// marks when each send has returned, before the caller can retry.
type heldSendClock struct{}

func (heldSendClock) WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	hold, _ := parent.Value(sendHoldKey{}).(*sendHold)
	if hold == nil {
		return clock.Real().WithTimeout(parent, timeout)
	}
	ctx, cancel := hold.clock.WithTimeout(parent, timeout)
	n := hold.sends.Add(1)
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
	started := time.Now()
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
	t.Logf("three-member cluster and stream ready in %s", time.Since(started))
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
// source, and the send route to target, a session the rig registers live. The captured topic is
// Slack's: the route refuses Dispatch's lane and GitHub's lane (publisherLanes).
func inProcessPaths(target string) []inProcessPath {
	const team, captured = "notifications.team.in-process", "notifications.slack.legion-test.in_process.message"
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
	ctx, cancel := clock.WithShownDeadline(testwait.Context(t))
	defer cancel()
	if err := rig.watch.FlushWithContext(ctx); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	return heard
}

// awaitHanded waits until the leader has been handed n publishes on heard's subject, and returns
// them in the order the leader was handed them.
func awaitHanded(t *testing.T, heard *natsgo.Subscription, n int) []*natsgo.Msg {
	t.Helper()
	handed := make([]*natsgo.Msg, 0, n)
	for i := range n {
		msg := nextInProcessMessage(t, heard, fmt.Sprintf("the leader's publish %d of %d on %s", i+1, n, heard.Subject))
		handed = append(handed, msg)
	}
	return handed
}

func nextInProcessMessage(t *testing.T, sub *natsgo.Subscription, what string) *natsgo.Msg {
	t.Helper()
	msg, err := sub.NextMsgWithContext(testwait.Context(t))
	if err != nil {
		t.Fatalf("receive %s: %v", what, err)
	}
	return msg
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
	ctx, cancel := clock.WithShownDeadline(testwait.Context(t))
	defer cancel()
	info, err := rig.js.StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject}, natsgo.Context(ctx))
	if err != nil {
		t.Fatalf("read the stream's copies on %s: %v", subject, err)
	}
	return info.State.Subjects[subject]
}

// requireOneCopy waits until the stream holds one copy on subject more than before, the count
// the row read before it sent anything, and fails on any other count.
func (rig *inProcessRig) requireOneCopy(t *testing.T, subject string, before uint64) {
	t.Helper()
	testwait.Until(t, "the stream to store the message on "+subject, func() (uint64, bool) {
		got := rig.copies(t, subject)
		return got, got > before
	})
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
	for _, line := range linesIn(rig.harness.logs.String(), "listener publish role send", map[string]any{"dedupe_key": bus.PublishDedupePrefix + key}) {
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
//
// Both attempts the leader is handed carry one Nats-Msg-Id, which is what makes the stream refuse
// the second as in process. That is the control of the delivery exception's row below
// (TestADeliveryExceptionIsNeverRefusedAsInProcessAndItsFailureIsLoggedOnce), which requires
// the header absent on what the leader is handed.
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
				rig.attempt(t.Context(), path.route, body),
				rig.attempt(t.Context(), path.route, body),
			}
			handed := awaitHanded(t, heard, 2)
			if first, second := handed[0].Header.Get(natsgo.MsgIdHdr), handed[1].Header.Get(natsgo.MsgIdHdr); first == "" || first != second {
				t.Errorf("the leader was handed the two attempts under Nats-Msg-Id %q and %q, want one id on both", first, second)
			}
			thaw()
			for _, answer := range answers {
				if answer := testwait.Receive(t, "the publish attempt to answer after thaw", answer); answer.Code != http.StatusOK {
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
// stream refuses it as in process, until its window ends (publishPastInProcess). The result
// names both the earlier in-process forward and the expired deadline, not just a refusal.
// The forward failed without a permanent refusal, so it costs the sender one. The next
// forward, answered Duplicate once the earlier forward is stored, is the resend's 200.
// Both sends are charged (sendToRole), and the stream holds one forward.
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
	first := make(chan error, 1)
	var forwards atomic.Int32
	forward := rig.d.delivery.forwardRole
	rig.d.delivery.forwardRole = func(subject string, item contracts.Envelope, msgID string, timeout time.Duration) (func() (bus.StoredAck, error), error) {
		n := forwards.Add(1)
		if n != 2 {
			return forward(subject, item, msgID, timeout)
		}
		wait, err := forward(subject, item, msgID, inProcessForwardWindow)
		if err != nil {
			first <- err
			return nil, err
		}
		return func() (bus.StoredAck, error) {
			ack, err := wait()
			first <- err
			return ack, err
		}, nil
	}
	rig.requireLeader(t)
	heard, before := rig.hear(t, path.subject), rig.copies(t, path.subject)
	sender, key := "ses_in_process_next_"+id.New()[:8], id.New()
	body := messageJSON(t, path.body(sender, key), nil)
	thaw := rig.cluster.FreezeRoutes(t)
	t.Cleanup(thaw)
	earlier := rig.attempt(t.Context(), path.route, body)
	awaitHanded(t, heard, 1)
	resend := rig.attempt(t.Context(), path.route, body)
	firstForward := testwait.Receive(t, "the resend's first forward to end", first)
	thaw()
	for name, answer := range map[string]<-chan *httptest.ResponseRecorder{"the earlier send": earlier, "the resend": resend} {
		if answer := testwait.Receive(t, name+" to answer", answer); answer.Code != http.StatusOK {
			t.Errorf("%s was answered %d %s; want 200", name, answer.Code, strings.TrimSpace(answer.Body.String()))
		}
	}
	if !earlierAttemptInProcess(firstForward) || !errors.Is(firstForward, context.DeadlineExceeded) {
		t.Errorf("the resend's first forward ended with %v; want the forward deadline to end it while its earlier forward was still in process",
			firstForward)
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
// one refused, before its budget ended stored nothing of its own and costs nothing: the rig ends
// its request once its second send has returned its refusal. A resend whose last send went out
// unanswered may have stored the copy itself, so it costs one: a separate relay sees and withholds
// that send, then the test ends the request on its clock. A role send whose window ends while its
// forward is still in process costs one, as main charges its 504 (sendToRole).
func TestAResendWhoseEarlierAttemptIsStillInProcessWhenItsBudgetEndsIsAnsweredMaybeStored(t *testing.T) {
	t.Parallel()
	rig := newInProcessRig(t)
	const target, holder = "ses_in_process_budget_target", "ses_in_process_budget_holder"
	if err := rig.harness.sessions.Put(target, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s: %v", target, err)
	}
	role := "in-process-budget-" + id.New()[:8]
	claimRoleFor(t, rig.harness.registry, rig.harness.sessions, holder, role, nil)
	type row struct {
		path inProcessPath
		mode string
		cost string
	}
	const everyAnswered = "its every send answered"
	const lastUnanswered = "its last send unanswered"
	var rows []row
	for _, path := range inProcessPaths(target) {
		rows = append(rows, row{path, everyAnswered, "free"}, row{path, lastUnanswered, "charged"})
	}
	rows = append(rows, row{roleInProcessPath(role, holder), "its window ends with its forward in process", "charged"})
	for _, row := range rows {
		t.Run(row.path.name+"/"+row.mode, func(t *testing.T) {
			rig.requireLeader(t)
			heard, before := rig.hear(t, row.path.subject), rig.copies(t, row.path.subject)
			sender := "ses_in_process_budget_" + id.New()[:8]
			body := messageJSON(t, row.path.body(sender, id.New()), nil)
			var relay *listenerRelay
			resender := *rig
			resendDeps := *rig.d
			resender.d = &resendDeps
			if row.mode == lastUnanswered {
				relay = startListenerRelay(t, rig.cluster.Servers[rig.leader].URL())
				client, err := bus.ConnectOwningStream([]string{relay.relayed}, bus.WithReplicas(3),
					bus.WithPublishAcknowledgementClock(heldSendClock{}))
				if err != nil {
					t.Fatalf("connect the resend's publisher through the relay: %v", err)
				}
				t.Cleanup(client.Close)
				resendDeps.client = client
				relay.arm(row.path.subject+" ", 2)
			}
			thaw := rig.cluster.FreezeRoutes(t)
			t.Cleanup(thaw)

			// Role forwards still use the wall clock inside the bus, not the acknowledgement
			// clock used by PublishStoredAck. Keep that row on the real forward path.
			newAttemptClock := func(hold *sendHold) (context.Context, context.CancelFunc, func()) {
				if row.path.role {
					ctx, cancel := context.WithTimeout(t.Context(), inProcessBudget)
					return ctx, cancel, func() {}
				}
				hold.clock = clocktest.New(time.Unix(0, 0))
				ctx, cancel := hold.clock.WithTimeout(t.Context(), inProcessBudget)
				return context.WithValue(ctx, sendHoldKey{}, hold), cancel, func() { hold.clock.Advance(inProcessBudget) }
			}
			earlierCtx, cancelEarlier, expireEarlier := newAttemptClock(&sendHold{})
			defer cancelEarlier()
			first := rig.attempt(earlierCtx, row.path.route, body)
			awaitHanded(t, heard, 1)
			hold := &sendHold{}
			laterCtx, cancelLater, expireLater := newAttemptClock(hold)
			defer cancelLater()
			if row.mode == everyAnswered {
				hold.ended = func(n int32) {
					if n == 2 {
						expireLater()
					}
				}
			}
			later := resender.attempt(laterCtx, row.path.route, body)
			if relay != nil {
				testwait.Receive(t, "the resend's second publish to leave the client and meet the relay", relay.swallowed)
				expireLater()
			}
			resend := testwait.Receive(t, "the resend to answer while the earlier publish is in process", later)
			expireEarlier()
			earlier := testwait.Receive(t, "the earlier attempt to answer without an acknowledgement", first)
			thaw()
			earlierStatus, earlierOutcome := http.StatusServiceUnavailable, outcomeBusUnavailable
			if row.path.role {
				earlierStatus, earlierOutcome = http.StatusGatewayTimeout, outcomeDeliveryFailed
			}
			if earlier.Code != earlierStatus {
				t.Errorf("the earlier attempt was answered %d %s, want %d", earlier.Code, strings.TrimSpace(earlier.Body.String()), earlierStatus)
			} else if !row.path.role {
				var refusal listenerwire.APIError
				if err := json.Unmarshal(earlier.Body.Bytes(), &refusal); err != nil || refusal.Code != listenerwire.CodeMaybeStored {
					t.Errorf("the earlier attempt answered %s, want code %q: it was sent but never acknowledged", earlier.Body.String(), listenerwire.CodeMaybeStored)
				}
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
			rig.requireOutcomes(t, row.path, sender, string(earlierOutcome)+" charged", string(outcomeEarlierAttemptInProcess)+" "+row.cost)
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
	if err := keepTakenRoleFrame(testwait.Context(t), cfg, role, item, seq); err != nil {
		t.Fatalf("a takeover copy whose first two publishes were refused as in process: %v; want it copied by the third", err)
	}
	msgID := takenFrameMsgID(strings.TrimPrefix(item.DedupeKey, contracts.RoleForwardDedupePrefix), role, seq)
	if !slices.Equal(ids, []string{msgID, msgID, msgID}) {
		t.Fatalf("the copy was published under %v, want %s three times: the in-process refusal is retried under the id it refused", ids, msgID)
	}
	stored, err := harness.client.LastStoredMessage(testwait.Context(t), bus.KeptRoleSubject(role))
	if err != nil {
		t.Fatalf("read the kept copy of role %s: %v", role, err)
	}
	var kept contracts.Envelope
	if err := json.Unmarshal(stored.Data, &kept); err != nil || kept.EventID != item.EventID {
		t.Fatalf("the lane holds %s (%v), want event %s", stored.Data, err, item.EventID)
	}
}

// A hand-off resend that reaches a three-copy stream while its earlier attempt is still being
// stored is refused as still in process (bus.ErrDuplicateInProcess) under the hand-off's MsgId,
// which every attempt of one envelope derives alike. The hand-off publishes again until the
// earlier attempt is stored and the answer names that copy (publishPastInProcess), so the resend
// is handed off as a duplicate and never fails: a failed hand-off is the send route's and the
// role routes' 503 and the agent subject give-up's NAK. The earlier attempt is handed off as no
// duplicate, and the stream holds one hand-off.
func TestAHandoffResendWhoseEarlierAttemptIsStillInProcessIsAnsweredDuplicate(t *testing.T) {
	t.Parallel()
	rig := newInProcessRig(t)
	handoff := rig.stoppedAgentHandoff(t)
	subject := listenerwire.LegionHandoffSubject("infra", handoffAgent)
	item := envelopeFor(contracts.LegacyAgentSubject(handoffAgent), "agent."+handoffAgent+"."+id.New(), "agent")
	type handedOff struct {
		handed *legionHandedOff
		err    error
	}
	attempt := func() <-chan handedOff {
		answer := make(chan handedOff, 1)
		go func() {
			handed, err := handoff.handOffToLegion(t.Context(), legionHandoffTarget{envelope: item, session: handoffAgent})
			answer <- handedOff{handed, err}
		}()
		return answer
	}
	rig.requireLeader(t)
	heard, before := rig.hear(t, subject), rig.copies(t, subject)
	thaw := rig.cluster.FreezeRoutes(t)
	t.Cleanup(thaw)
	earlier := attempt()
	awaitHanded(t, heard, 1)
	resend := attempt()
	awaitHanded(t, heard, 1)
	thaw()
	if got := testwait.Receive(t, "the earlier hand-off to answer", earlier); got.err != nil || got.handed == nil || got.handed.ack.Duplicate {
		t.Errorf("the earlier hand-off = %+v, %v; want it handed off, no duplicate", got.handed, got.err)
	}
	if got := testwait.Receive(t, "the hand-off resend to answer", resend); got.err != nil || got.handed == nil || !got.handed.ack.Duplicate {
		t.Errorf("the resend's hand-off = %+v, %v; want the earlier attempt's copy, answered duplicate: its first publish was refused as in process, and that attempt is stored",
			got.handed, got.err)
	}
	rig.requireOneCopy(t, subject, before)
}

// stoppedAgentHandoff is a hand-off over the rig's listener for handoffAgent, a stopped
// implementer of the project infra, which says it answers. The registries are on the package's
// shared server, as the rig's other reads are, so a frozen route holds the hand-off's publish over
// the rig's listener and none of its reads.
func (rig *inProcessRig) stoppedAgentHandoff(t *testing.T) *legionHandoff {
	t.Helper()
	agents, err := legionagents.Open(t.Context(), rig.harness.client.Conn, legionagents.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the legion agent registry: %v", err)
	}
	ctx := testwait.Context(t)
	if _, err := agents.Put(ctx, legionagents.Entry{
		SessionID: handoffAgent, LegionProject: "infra", Issue: "INFRA-204", Role: "implementer",
		RoleToken: "legion-infra-infra-204-implementer", Title: "Legion implementer · INFRA-204", Harness: "omp",
		StartedAt: "2026-10-26T03:00:00Z",
	}); err != nil {
		t.Fatalf("record the stopped implementer: %v", err)
	}
	if _, err := agents.PutProject(ctx, "infra", legionagents.Project{
		Answering: true, UpdatedAt: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}); err != nil {
		t.Fatalf("state that the project answers: %v", err)
	}
	return &legionHandoff{client: rig.d.client, registry: rig.harness.registry, sessions: rig.harness.sessions,
		agents: newLegionAgentRegistry(agents), logger: rig.logger}
}

// The agent subject's give-up hands a message off within its window (handoffGiveUpWindow) and no
// longer. While an earlier attempt of the same hand-off is still in process, as routes frozen under
// it leave it, the stream refuses every publish of the give-up's hand-off as in process
// (bus.ErrDuplicateInProcess), and publishPastInProcess publishes again until its context ends.
// The window is that context: the give-up returns once it ends, records the failed hand-off (its
// line and its count) and NAKs the message to be tried again, rather than publishing again for as
// long as the earlier attempt is not stored, which a stream that lost its quorum may never store.
// Expire the actual give-up budget on the fake while the cluster is still frozen.
func TestTheAgentSubjectGiveUpStopsAtItsWindowWhileItsHandoffIsStillInProcess(t *testing.T) {
	t.Parallel()
	rig := newInProcessRig(t)
	cfg := rig.harness.config
	cfg.handoff = rig.stoppedAgentHandoff(t)
	fake := clocktest.New(time.Unix(0, 0))
	cfg.handoff.giveUpClock = fake
	subject := listenerwire.LegionHandoffSubject("infra", handoffAgent)
	item := envelopeFor(contracts.LegacyAgentSubject(handoffAgent), "agent."+handoffAgent+"."+id.New(), "agent")

	// The message the give-up finalizes: one bound to a subscription, with a reply subject the
	// test reads, so its NAK is heard where a durable message's would reach the server.
	conn := rig.harness.client.Conn
	nakInbox := natsgo.NewInbox()
	naks, err := conn.SubscribeSync(nakInbox)
	if err != nil {
		t.Fatalf("subscribe the message's reply subject: %v", err)
	}
	t.Cleanup(func() { _ = naks.Unsubscribe() })
	carrierSubject := "test.handoff-give-up." + id.New()
	carrier, err := conn.SubscribeSync(carrierSubject)
	if err != nil {
		t.Fatalf("subscribe %s: %v", carrierSubject, err)
	}
	t.Cleanup(func() { _ = carrier.Unsubscribe() })
	if err := conn.PublishMsg(&natsgo.Msg{Subject: carrierSubject, Reply: nakInbox, Data: marshalListenerEnvelope(t, item)}); err != nil {
		t.Fatalf("publish the message: %v", err)
	}
	carried := nextInProcessMessage(t, carrier, "the carrier for the durable message")
	message := deliveryMessage{data: carried.Data, mode: durableDelivery, msg: carried}

	rig.requireLeader(t)
	heard := rig.hear(t, subject)
	thaw := rig.cluster.FreezeRoutes(t)
	t.Cleanup(thaw)
	earlier := make(chan error, 1)
	go func() {
		_, err := cfg.handoff.handOffToLegion(t.Context(), legionHandoffTarget{envelope: item, session: handoffAgent})
		earlier <- err
	}()
	awaitHanded(t, heard, 1)

	deadline := fake.Now().Add(legionHandoffGiveUpWindow)
	answered := make(chan bool, 1)
	go func() { answered <- offerAgentDeliveryToLegion(cfg, message, item, handoffAgent) }()
	defer fake.Set(deadline)
	// The give-up's retry is a marker after its first refusal. It must retry while
	// the fake still stands at the start of the window.
	testwait.Until(t, fmt.Sprintf("the give-up window armed for %v and the leader to receive its first publish and retry", deadline), func() (string, bool) {
		select {
		case <-answered:
			t.Fatalf("the give-up returned before its window ended:\n%s", rig.harness.logs.String())
		default:
		}
		pending, _, err := heard.Pending()
		if err != nil {
			t.Fatalf("read the leader's pending hand-off publishes: %v", err)
		}
		armed := fake.Armed()
		return fmt.Sprintf("armed %v, publishes %d", armed, pending), slices.ContainsFunc(armed, deadline.Equal) && pending >= 2
	})
	fake.Set(deadline)
	if owned := testwait.Receive(t, "the give-up to stop retrying the in-process hand-off", answered); !owned {
		t.Fatalf("the give-up answered that the hand-off was not the message's outcome; want it to own the failed hand-off:\n%s", rig.harness.logs.String())
	}
	if !strings.Contains(rig.harness.logs.String(), context.DeadlineExceeded.Error()) {
		t.Errorf("the give-up did not fail because its window ended:\n%s", rig.harness.logs.String())
	}
	failed := deliveryRecords(t, rig.harness.logs.String(), "listener legion hand-off failed")
	if len(failed) != 1 || failed[0].SessionID != handoffAgent || failed[0].DeliveryStatus != "failed" {
		t.Errorf("the give-up wrote %+v, want one failed hand-off line for %s:\n%s", failed, handoffAgent, rig.harness.logs.String())
	}
	if !strings.Contains(rig.harness.logs.String(), errEarlierAttemptInProcess.Error()) {
		t.Errorf("the failed hand-off does not say it ended while the earlier attempt was in process (%q):\n%s", errEarlierAttemptInProcess, rig.harness.logs.String())
	}
	if nak := nextInProcessMessage(t, naks, "the give-up's reply"); !strings.HasPrefix(string(nak.Data), "-NAK") {
		t.Errorf("the message's reply subject read %q; want the give-up's NAK", nak.Data)
	}
	body := metricsBody(t, rig.harness)
	for _, series := range []string{`test_messages_naked 1`, `test_messages_delivered{delivery_status="failed"} 1`} {
		if !strings.Contains(body, series) {
			t.Errorf("the listener's metrics lack %s:\n%s", series, body)
		}
	}

	thaw()
	testwait.Receive(t, "the earlier hand-off to return after the thaw", earlier)
}

// A delivery exception is published under no Nats-Msg-Id (publishDeliveryException), so a
// clustered stream never refuses it as still in process (bus.ErrDuplicateInProcess). Two
// listeners that raise an exception for one message each store a copy, and neither logs a line.
// A failure of the same publish, an exception past the server's max payload, is still logged once
// at ERROR as exceptionPublishFailedLine, which the deployed publish-failure filter counts:
// `{ $.level = "ERROR" && $.msg = "listener exception publish failed" }`
// (packages/envoy/infra/aws/rendering/metricfilters.go). The first row freezes the members'
// routes and thaws them once the leader holds both publishes. Under one message id the stream
// would refuse the second publish while the first is in process.
func TestADeliveryExceptionIsNeverRefusedAsInProcessAndItsFailureIsLoggedOnce(t *testing.T) {
	t.Parallel()
	rig := newInProcessRig(t)
	const topic, recipient, reason = "notifications.team.exception-in-process", "ses_exception_in_process", "delivery_failed"
	lane := bus.ExceptionTopicPrefix + topic
	for _, row := range []struct {
		name     string
		tooLarge bool
	}{
		{"raised by two listeners while the routes are frozen", false},
		{"past the server's max payload", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			logs := &cmdtest.LockedBuffer{}
			cfg := *rig.d.delivery
			cfg.client = rig.d.client
			cfg.logger = logging.NewWithWriter("test", logs)
			item := listenerTestEnvelope(topic, "exception-in-process-"+id.New())
			outcome := deliveryOutcome{sessionID: recipient, exceptionReason: reason, fanoutRefusal: true}
			if row.tooLarge {
				item.Payload = strings.Repeat("p", int(rig.d.client.Conn.MaxPayload()))
				applyDeliveryOutcome(cfg, item, outcome)
				var counted, refusals []string
				for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
					var record map[string]any
					if json.Unmarshal([]byte(line), &record) != nil || record["topic"] != topic {
						continue
					}
					if record["level"] == "ERROR" && record["msg"] == exceptionPublishFailedLine {
						counted = append(counted, line)
					}
					if text, _ := record["error"].(string); strings.Contains(text, bus.ErrTooLarge.Error()) {
						refusals = append(refusals, line)
					}
				}
				if len(refusals) != 1 || len(counted) != 1 || counted[0] != refusals[0] {
					t.Fatalf("an exception the server refused as too large logged %d lines naming %q and %d lines the publish-failure filter counts, want one line that is both:\n%s",
						len(refusals), bus.ErrTooLarge, len(counted), logs.String())
				}
				return
			}
			rig.requireLeader(t)
			heard, before := rig.hear(t, lane), rig.copies(t, lane)
			thaw := rig.cluster.FreezeRoutes(t)
			t.Cleanup(thaw)
			other := make(chan error, 1)
			go func() { other <- publishDeliveryException(rig.d.client, item, reason, recipient) }()
			applied := make(chan struct{})
			go func() {
				defer close(applied)
				applyDeliveryOutcome(cfg, item, outcome)
			}()
			for _, msg := range awaitHanded(t, heard, 2) {
				if msgID := msg.Header.Get(natsgo.MsgIdHdr); msgID != "" {
					t.Errorf("an exception was published under Nats-Msg-Id %q, want none", msgID)
				}
			}
			thaw()
			if err := testwait.Receive(t, "the other listener's publish of the exception", other); err != nil {
				t.Fatalf("the other listener's publish of the exception: %v; want it stored", err)
			}
			testwait.Receive(t, "the delivery outcome to finish applying", applied)
			testwait.Until(t, "the stream to store both exceptions on "+lane, func() (uint64, bool) {
				got := rig.copies(t, lane)
				return got, got >= before+2
			})
			if got := rig.copies(t, lane) - before; got != 2 {
				t.Fatalf("the stream stored %d copies on %s, want 2: each exception is an event of its own", got, lane)
			}
			if text := strings.TrimSpace(logs.String()); text != "" {
				t.Fatalf("the exception the stream stored logged lines, want none:\n%s", text)
			}
		})
	}
}
