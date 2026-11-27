package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// notDeployedRig is a listener's /v1 routes over a stream a hub move's quiesce can narrow, with
// a sender limit of one message a minute and a burst of one (senderLimitRig), and a core
// subscriber of every notification topic on a connection of its own: it is handed a core publish
// of any notification topic, and the request of a JetStream publish of one too, so its silence is
// what tells a send that went nowhere from one that went out over core NATS. The listener's client
// reads the deployed subjects every 100 ms, so each wait for its read ends within a second.
type notDeployedRig struct {
	*senderLimitRig
	js       natsgo.JetStreamContext
	received *natsgo.Subscription
	conn     *natsgo.Conn
}

func newNotDeployedRig(t *testing.T) *notDeployedRig {
	t.Helper()
	url := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1), bus.WithStreamSubjectsRefresh(100*time.Millisecond))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	rig := &notDeployedRig{senderLimitRig: newSenderLimitRigOn(t, client, url)}
	rig.limit(1, 1)
	rig.conn = testnats.Connect(t, url)
	t.Cleanup(rig.conn.Close)
	if rig.js, err = rig.conn.JetStream(); err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return rig
}

// narrow narrows the stream as the quiesce does and waits until the listener's client has read
// it, judged on topic; it returns what the restore puts back.
func (rig *notDeployedRig) narrow(t *testing.T, topic string) natsgo.StreamConfig {
	t.Helper()
	original := bustest.NarrowStream(t, rig.js, nil)
	bustest.AwaitDeployedCapture(t, rig.harness.client, topic, false, 15*time.Second)
	return original
}

// restore puts original's subjects back and waits until the listener's client has read them.
func (rig *notDeployedRig) restore(t *testing.T, original natsgo.StreamConfig, topic string) {
	t.Helper()
	bustest.RestoreStream(t, rig.js, original)
	bustest.AwaitDeployedCapture(t, rig.harness.client, topic, true, 15*time.Second)
}

// subscribe starts the core subscriber of every notification topic and returns the stream's last
// sequence after it, the baseline nothing may move.
func (rig *notDeployedRig) subscribe(t *testing.T) uint64 {
	t.Helper()
	received, err := rig.conn.SubscribeSync(bus.NotificationTopicPrefix + ">")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = received.Unsubscribe() })
	if err := rig.conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	rig.received = received
	return rig.lastSeq(t)
}

func (rig *notDeployedRig) lastSeq(t *testing.T) uint64 {
	t.Helper()
	info, err := rig.js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	return info.State.LastSeq
}

// requireNothingSent fails unless, after a flush and 500 ms more, the core subscriber holds no
// message and the stream's last sequence is still baseline.
func (rig *notDeployedRig) requireNothingSent(t *testing.T, baseline uint64) {
	t.Helper()
	if err := rig.conn.Flush(); err != nil {
		t.Fatalf("flush the subscriber: %v", err)
	}
	if msg, err := rig.received.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("the subscriber was handed %s (reply %q); a refused message sends nothing", msg.Subject, msg.Reply)
	} else if !errors.Is(err, natsgo.ErrTimeout) {
		t.Fatalf("read the subscriber: %v", err)
	}
	if after := rig.lastSeq(t); after != baseline {
		t.Fatalf("the stream's last sequence moved from %d to %d; a refused message stores nothing", baseline, after)
	}
}

// requireStoredOnce fails unless the stream holds exactly one message on topic.
func (rig *notDeployedRig) requireStoredOnce(t *testing.T, topic string) {
	t.Helper()
	info, err := rig.js.StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: topic})
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if got := info.State.Subjects[topic]; got != 1 {
		t.Fatalf("the stream holds %d messages on %s, want 1", got, topic)
	}
}

// requireNotDeployedAnswer fails unless a is the 503 of a refusal the bus made before it sent
// anything, because the deployed stream does not capture topic: Retry-After: 1, a body opening
// with contracts.MessageNotSentPrefix, never busUnansweredPrefix, since the bus was never asked,
// that holds the text of bus.ErrSubjectNotDeployed and names topic, no maybe_stored code, since
// nothing was stored, and one line recording subject_not_deployed with status 503.
func requireNotDeployedAnswer(t *testing.T, what string, a limitedAnswer, topic string) {
	t.Helper()
	a.requireStatus(t, what, http.StatusServiceUnavailable)
	if a.retryAfter != "1" {
		t.Fatalf("%s: Retry-After %q, want 1", what, a.retryAfter)
	}
	if !strings.HasPrefix(a.Error, contracts.MessageNotSentPrefix+": ") || !strings.Contains(a.Error, bus.ErrSubjectNotDeployed.Error()) || !strings.Contains(a.Error, topic) {
		t.Fatalf("%s: error %q, want it to open with %q and hold %q and %s", what, a.Error, contracts.MessageNotSentPrefix+": ", bus.ErrSubjectNotDeployed, topic)
	}
	if strings.Contains(a.raw, `"code"`) {
		t.Fatalf("%s: answer %s carries a code; a message nothing stored is no maybe_stored answer", what, a.raw)
	}
	a.requireOutcome(t, what, outcomeSubjectNotDeployed)
	if a.line["status"] != float64(http.StatusServiceUnavailable) {
		t.Fatalf("%s: the line's status is %v, want 503; line %v", what, a.line["status"], a.line)
	}
}

// A send to a registered session while a hub move's quiesce has narrowed the stream, so that it
// no longer captures the session's agent subject, is the 503 with Retry-After: 1 that says the
// message was not sent and may be sent again later, and nothing goes out: no core subscriber is
// handed the envelope and the stream stores nothing. The refused send costs its sender nothing, so
// the same send is answered 200, not 429, once the stream captures the subject again and the
// listener has read that. Before the refusal the send went out over core NATS and was answered
// 200: the six sends of a hub move that their recipients never received.
func TestASendWhileTheDeployedStreamLacksTheAgentSubjectAnswers503AndStoresNothing(t *testing.T) {
	// Not parallel: it holds a 500 ms quiet window in which the refused send must reach no core
	// subscriber (requireNothingSent).
	const (
		target = "ses_not_deployed_target"
		sender = "ses_not_deployed_sender"
	)
	agent := contracts.LegacyAgentSubject(target)
	rig := newNotDeployedRig(t)
	rig.live(t, target, nil)
	original := rig.narrow(t, agent)
	baseline := rig.subscribe(t)

	// Step 1: the refusal's answer and its one line.
	requireNotDeployedAnswer(t, "a send while the stream lacks the agent subject", rig.sendFrom(t, sender, target, nil), agent)

	// Step 2: nothing went out.
	rig.requireNothingSent(t, baseline)

	// Step 3: the same send, once the stream captures the subject again, is stored once, and the
	// refused one cost the sender nothing.
	rig.restore(t, original, agent)
	rig.sendFrom(t, sender, target, nil).requireStatus(t, "the same send after the restore", http.StatusOK)
	rig.requireStoredOnce(t, agent)
}

// A send to a stopped session, which the session store holds no entry of and the known-session
// store a record of, is refused the same way while the stream does not capture the session's agent
// subject, and raises no wake: the route asks the bus before it reports the wake
// (bus.Client.UndeployedRefusal), so a sender that sends again at Retry-After: 1 for a whole
// narrowing wakes no pod for a message nothing took. The refused send costs the sender nothing, so
// the same send after the restore is stored for the stopped session and answered 200, not 429, and
// raises the wake then. Charged for a wake, the refused send would leave a sender limited to one
// message a minute refused for the minute after the stream captures the subject again.
//
// The narrowing keeps the subject that captures the wake topic, so a wake the refused send raised
// would be stored and handed to the rig's wake subscriber: the wake the send after the restore
// raises is the control that the subscriber sees one.
func TestASendToAStoppedSessionWhileTheDeployedStreamLacksItsSubjectCostsNothing(t *testing.T) {
	// Not parallel: it holds a 500 ms quiet window in which the refused send must raise no wake
	// report, which the route publishes off its path (requireNoWake).
	const (
		target = "ses_not_deployed_stopped"
		sender = "ses_not_deployed_stopped_sender"
	)
	agent := contracts.LegacyAgentSubject(target)
	rig := newNotDeployedRig(t)
	rig.recorded(t, target, nil)
	original := bustest.NarrowStream(t, rig.js, func(subject string) bool { return bus.SubjectContains(subject, bus.LegionWakeTopic) })
	bustest.AwaitDeployedCapture(t, rig.harness.client, agent, false, 15*time.Second)
	if !rig.harness.client.DeployedStreamCaptures(bus.LegionWakeTopic) {
		t.Fatalf("the narrowed stream does not capture %s, so a wake would be refused and the check below could not see one", bus.LegionWakeTopic)
	}

	requireNotDeployedAnswer(t, "a send to a stopped session while the stream lacks its agent subject", rig.sendFrom(t, sender, target, nil), agent)
	rig.requireNoWake(t, wakeKindAgent, target)

	rig.restore(t, original, agent)
	rig.sendFrom(t, sender, target, nil).requireStatus(t, "the same send after the restore", http.StatusOK)
	rig.requireStoredOnce(t, agent)
	deadline := time.Now().Add(10 * time.Second)
	for {
		msg, err := rig.wakes.NextMsg(time.Until(deadline))
		if err != nil {
			t.Fatalf("no agent wake report named %s after the send the stream stored (%v): the wake subscriber sees nothing, so its silence above proves nothing", target, err)
		}
		if wakeNamed(wakeReportIn(t, msg), wakeKindAgent, target) {
			break
		}
	}
}

// A publish to a team topic while a hub move's quiesce has narrowed the stream is the 503 with
// Retry-After: 1 that says the message was not sent and may be sent again later, not the 400 "no
// stream subject captures" that tells its sender to publish elsewhere, which no plugin sends again: a
// team topic is one this build stores. Nothing goes out, and the refused publish costs its sender
// nothing. A topic no stream captures, which this build does not store either, keeps its 400.
func TestAPublishWhileTheDeployedStreamLacksATeamTopicAnswers503AndStoresNothing(t *testing.T) {
	// Not parallel: it holds a 500 ms quiet window in which the refused publish must reach no core
	// subscriber (requireNothingSent).
	const (
		topic  = "notifications.team.t"
		adhoc  = "notifications.lxspec.probe"
		sender = "ses_not_deployed_publisher"
	)
	rig := newNotDeployedRig(t)
	original := rig.narrow(t, topic)
	baseline := rig.subscribe(t)

	// Step 1: the refusal's answer, its one line, and nothing sent.
	requireNotDeployedAnswer(t, "a publish while the stream lacks the team topic", rig.publishFrom(t, sender, topic, nil), topic)
	rig.requireNothingSent(t, baseline)

	// Step 2, the control: a topic no stream captures and this build does not store keeps its 400.
	adhocAnswer := rig.publishFrom(t, sender, adhoc, nil)
	adhocAnswer.requireStatus(t, "a publish to a topic no stream captures", http.StatusBadRequest)
	if !strings.HasPrefix(adhocAnswer.Error, "no stream subject captures "+adhoc) {
		t.Fatalf("a publish to a topic no stream captures: error %q, want it to open with %q", adhocAnswer.Error, "no stream subject captures "+adhoc)
	}
	if adhocAnswer.retryAfter != "" {
		t.Fatalf("a publish to a topic no stream captures: Retry-After %q, want none", adhocAnswer.retryAfter)
	}

	// Step 3: the same publish, once the stream captures the topic again, is stored once, and the
	// refused one cost the sender nothing.
	rig.restore(t, original, topic)
	rig.publishFrom(t, sender, topic, nil).requireStatus(t, "the same publish after the restore", http.StatusOK)
	rig.requireStoredOnce(t, topic)
}

// A send to a role whose live holder's agent subject the deployed stream does not capture, as
// during a hub move's quiesce, is not the 503 that says the message was not sent. The publish
// route forwards a role message to its holder's agent subject (bus.Client.StartRequestStoredTo),
// a publish no capture check refuses before it is sent, so the stream acknowledges no forward,
// each forward is made again until the send's window (publishDeliveryWindow) ends, and the send is
// the 504 delivery_failed. That 504 costs its sender one, as every 504 does (roleSend.timedOut):
// the listener cannot say whether a forward stored the message, where the direct send's 503 above,
// which sent nothing, costs nothing. The stream stores nothing on the holder's agent subject, while
// it is narrowed or after the restore.
//
// It pins what the listener already does, which the hub move accepts: a change that refused the
// forward before it was sent, and answered the not-sent 503 that costs nothing, fails it at the
// status.
func TestARoleSendWhileTheDeployedStreamLacksItsHoldersAgentSubjectAnswers504AndCostsOne(t *testing.T) {
	t.Parallel()
	const (
		sender = "ses_not_deployed_role_sender"
		holder = "ses_not_deployed_role_holder"
		role   = "not-deployed-role"
		what   = "a role send while the stream lacks its holder's agent subject"
	)
	agent := contracts.LegacyAgentSubject(holder)
	rig := newNotDeployedRig(t)
	claimRoleFor(t, rig.deps.registry, rig.deps.sessions, holder, role, nil)
	forwards := rig.interceptForwards(func(_ int, forward func() (func() (bus.StoredAck, error), error)) (func() (bus.StoredAck, error), error) {
		return forward()
	})
	original := rig.narrow(t, agent)
	baseline := rig.lastSeq(t)

	// Step 1: the 504 once the window has ended, after at least one forward, and its one line,
	// which names the sender because the send cost it one.
	started := time.Now()
	answer := rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role, nil)
	elapsed := time.Since(started)
	answer.requireStatus(t, what, http.StatusGatewayTimeout)
	if elapsed < publishDeliveryWindow {
		t.Fatalf("%s: answered after %s, want the %s window to have ended first", what, elapsed, publishDeliveryWindow)
	}
	if answer.Reason != string(sendDeliveryFailed) || answer.Holder != holder {
		t.Fatalf("%s: body %s, want reason %s naming holder %s", what, answer.raw, sendDeliveryFailed, holder)
	}
	if *forwards == 0 {
		t.Fatalf("%s: the send made no forward, so its 504 says nothing of a forward the stream did not store", what)
	}
	answer.requireOutcome(t, what, outcomeDeliveryFailed)
	if answer.line["status"] != float64(http.StatusGatewayTimeout) {
		t.Fatalf("%s: the line's status is %v, want 504; line %v", what, answer.line["status"], answer.line)
	}
	answer.requireNamed(t, what, sender)

	// Step 2: the stream stored nothing, on the holder's agent subject or elsewhere.
	if got := rig.storedOn(t, agent); got != 0 {
		t.Fatalf("%s: the stream holds %d messages on %s, want none", what, got, agent)
	}
	if after := rig.lastSeq(t); after != baseline {
		t.Fatalf("%s: the stream's last sequence moved from %d to %d, want no message stored", what, baseline, after)
	}

	// Step 3: once the stream captures the subject again it still holds no late copy of the
	// forward, and the sender's next message is refused for its rate: the 504 cost it its one.
	rig.restore(t, original, agent)
	if got := rig.storedOn(t, agent); got != 0 {
		t.Fatalf("after the restore the stream holds %d messages on %s, want none", got, agent)
	}
	rig.requireCount(t, sender, false)
}
