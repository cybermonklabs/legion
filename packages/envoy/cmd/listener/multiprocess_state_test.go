//go:build multiprocess

// The rows of the two-pod suite (multiprocess_harness_test.go) about the state the machine's pods
// share: the delivery ledger, the delivery exceptions one pod's work spares the other, the writes
// a pod whose cache trails must not lose, a pod-local address another pod accepted, and the role
// and last-seen answers every pod must give alike. Its first helpers read a pod's log, its cache
// and the buckets, which these rows do more than the take-over ones.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/store"
)

// swallowNextAck has the pod's relay swallow the next acknowledgement the pod sends the durable
// (nats.go's "+ACK"), and everything the pod sends after it on that link, so the pod handles a
// message to its end and the durable never hears it. The returned func waits for that ack.
func (p *mpPod) swallowNextAck(t *testing.T) (swallowed func()) {
	t.Helper()
	p.relay.arm("+ACK", 1)
	return func() {
		t.Helper()
		select {
		case <-p.relay.swallowed:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s acknowledged nothing to the durable within 10 s", p.instance)
		}
	}
}

// setRole claims role for sessionID through the pod, naming no process, and requires a 200.
func (p *mpPod) setRole(t *testing.T, sessionID, role string) {
	t.Helper()
	p.setRoleAs(t, sessionID, role, "")
}

// setRoleAs claims role for sessionID through the pod as the client instance given, which the
// claim records as the process that holds the role (store.RoleClaim.ClientInstanceID), and
// requires a 200. An empty instance is left off the body, as a client that has none sends it.
func (p *mpPod) setRoleAs(t *testing.T, sessionID, role, clientInstanceID string) {
	t.Helper()
	body := fmt.Sprintf(`{"session_id":%q,"role":%q`, sessionID, role)
	if clientInstanceID != "" {
		body += fmt.Sprintf(`,"client_instance_id":%q`, clientInstanceID)
	}
	answer := p.call(http.MethodPost, "/v1/roles/set", body+"}")
	if answer.status != http.StatusOK {
		t.Fatalf("claim %s for %s as process %q through %s: status %d %s %v", role, sessionID, clientInstanceID, p.instance, answer.status, answer.body, answer.err)
	}
}

// mpRoleSet is the answer of a role set through a pod (countedRoleSet), with the requests the pod
// sent to the records the role set reads and writes (roleSetKeys) from the moment the request went
// out to the moment its answer was read, in the order the pod's relay forwarded them, and toClaim,
// how many of them came up to and including the claim's write, or 0 where none was that write.
type mpRoleSet struct {
	mpAnswer
	requests []listenerRelayRequest
	toClaim  int
}

// roleSetKeys is the KV key subject of every record a role set of role by sessionID reads or
// writes, each a key naming the session or the role (roleSetHandler counts the round trips to
// them): the claimant's registration, which the claim reads where the pod's cache does not hold
// it, and which the refresh reads from the stream's leader, writes and reads back; the claimant's
// last-seen record in the listener state bucket, which the refresh writes
// (session.SessionRegistry's recordSeen); the role's claim, which the claim reads, reads for its
// delete marker and writes; and the claimant's interest record, which the claim reads and writes,
// and reads once more after its tries to confirm the role's topic. The pod's own traffic names
// none of them: its lease record's reads every half second are on lease.<durable> in the listener
// state bucket, and its watchers' consumer creates, flow control replies and stream info reads
// name no key at all.
func roleSetKeys(sessionID, role string) []string {
	return []string{
		"$KV." + bus.SessionBucket + "." + sessionID,
		"$KV." + bus.ListenerStateBucket + "." + bus.LastSeenPrefix + dedupe.SessionToken(sessionID),
		"$KV." + bus.RoleClaimBucket + "." + role,
		"$KV." + bus.InterestBucket + "." + sessionID,
	}
}

// countedRoleSet claims role for sessionID through the pod, naming no process, counts through the
// pod's relay the requests the pod sends to the role set's records (roleSetKeys) while it answers,
// and logs them with the answer and the time it took. The claim's write is the request published
// on the role's own key in the role bucket; the read that confirms the role's topic after the
// claim's tries comes after it, so toClaim counts the round trips roleSetHandler counts, which
// must land before those tries end.
func (p *mpPod) countedRoleSet(t *testing.T, sessionID, role string) mpRoleSet {
	t.Helper()
	stop := p.relay.countRequests(roleSetKeys(sessionID, role)...)
	set := mpRoleSet{mpAnswer: p.call(http.MethodPost, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q}`, sessionID, role))}
	set.requests = stop()
	claimWrite := "$KV." + bus.RoleClaimBucket + "." + role
	var named []string
	for i, request := range set.requests {
		if set.toClaim == 0 && request.subject == claimWrite {
			set.toClaim = i + 1
		}
		named = append(named, fmt.Sprintf("[%d] %s", i+1, request))
	}
	t.Logf("role set requests: %s's claim of %s through %s answered %d in %s, after %d requests to its records up to its claim write and %d in all: %s",
		sessionID, role, p.instance, set.status, set.took.Round(time.Millisecond), set.toClaim, len(set.requests), strings.Join(named, " "))
	return set
}

// requireTrips fails the test unless the role set reached its claim write within trips of the
// requests counted.
func (s mpRoleSet) requireTrips(t *testing.T, sessionID string, trips int) {
	t.Helper()
	if s.toClaim == 0 {
		t.Fatalf("role set trips: no claim write among %s's %d requests counted", sessionID, len(s.requests))
	}
	if s.toClaim > trips {
		t.Fatalf("role set trips: %s's claim made %d requests to its records up to its claim write, want at most %d (roleSetHandler counts them)", sessionID, s.toClaim, trips)
	}
}

// lapsedRole reads GET /v1/roles/<role> on the pod, which must answer 404 for a holder that is
// gone, and returns the answer's fields.
func (p *mpPod) lapsedRole(t *testing.T, role string) map[string]any {
	t.Helper()
	answer := p.call(http.MethodGet, "/v1/roles/"+role, "")
	var body map[string]any
	if answer.err != nil || answer.status != http.StatusNotFound || json.Unmarshal([]byte(answer.body), &body) != nil || body["reason"] != "holder_lapsed" {
		t.Fatalf("GET /v1/roles/%s on %s: %d %s %v, want 404 for a lapsed holder", role, p.instance, answer.status, strings.TrimSpace(answer.body), answer.err)
	}
	return body
}

// requireSameLapsedAnswer fails the test unless two answers for lapsed holders (lapsedRole), about
// one holder, say the same of it in each of fields, compared in the order given.
func requireSameLapsedAnswer(t *testing.T, a, b *mpPod, onA, onB map[string]any, fields ...string) {
	t.Helper()
	for _, field := range fields {
		if fmt.Sprint(onA[field]) != fmt.Sprint(onB[field]) {
			t.Fatalf("%s answers %s %v, %s answers %v; want one answer from every pod: %v and %v", a.instance, field, onA[field], b.instance, onB[field], onA, onB)
		}
	}
}

// cachedSession is the pod's GET /v1/sessions row for sessionID, which the pod answers from its
// cache, or nil while it lists none.
func (p *mpPod) cachedSession(sessionID string) map[string]any {
	answer := p.call(http.MethodGet, "/v1/sessions", "")
	var rows []map[string]any
	if answer.err != nil || answer.status != http.StatusOK || json.Unmarshal([]byte(answer.body), &rows) != nil {
		return nil
	}
	for _, row := range rows {
		if row["session_id"] == sessionID {
			return row
		}
	}
	return nil
}

// cachedTopics is the topics of sessionID's interest as the pod's GET /v1/interests/<session_id>,
// which it answers from its cache, lists them.
func (p *mpPod) cachedTopics(sessionID string) []string {
	answer := p.call(http.MethodGet, "/v1/interests/"+sessionID, "")
	var interest struct {
		Topics []string `json:"topics"`
	}
	if answer.err != nil || answer.status != http.StatusOK || json.Unmarshal([]byte(answer.body), &interest) != nil {
		return nil
	}
	return interest.Topics
}

// stored is key's value in bucket, read from the bucket itself.
func (p *mpPublisher) stored(t *testing.T, bucket, key string) map[string]any {
	t.Helper()
	kv, err := p.client.JS().KeyValue(bucket)
	if err != nil {
		t.Fatalf("open %s: %v", bucket, err)
	}
	entry, err := kv.Get(key)
	if err != nil {
		t.Fatalf("read %s from %s: %v", key, bucket, err)
	}
	var value map[string]any
	if err := json.Unmarshal(entry.Value(), &value); err != nil {
		t.Fatalf("decode %s's %s: %v (%s)", bucket, key, err, entry.Value())
	}
	return value
}

// mpException is a delivery exception a listener published: its own id, and the event, recipient
// and reason its payload reports.
type mpException struct {
	id, event, recipient, reason string
}

// watchExceptions subscribes, over core NATS, to the exception lane of topic, where a listener
// reports a message on topic it could not deliver to one of the sessions that follow it. Every
// publish there reaches it, a repeat the stream does not store again included.
func (p *mpPublisher) watchExceptions(t *testing.T, topic string) *natsgo.Subscription {
	t.Helper()
	subject := bus.ExceptionTopicPrefix + topic
	subscription, err := p.client.Conn.SubscribeSync(subject)
	if err != nil {
		t.Fatalf("subscribe to %s: %v", subject, err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	if err := p.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the subscription to %s: %v", subject, err)
	}
	return subscription
}

// mpExceptionOf reads a delivery exception's own id, and the event, recipient and reason its
// payload reports.
func mpExceptionOf(t *testing.T, item contracts.Envelope) mpException {
	t.Helper()
	var payload struct {
		EventID          string `json:"event_id"`
		Reason           string `json:"reason"`
		RecipientSession string `json:"recipient_session"`
	}
	if err := json.Unmarshal([]byte(item.Payload), &payload); err != nil {
		t.Fatalf("decode a delivery exception's payload: %v (%s)", err, item.Payload)
	}
	return mpException{id: item.EventID, event: payload.EventID, recipient: payload.RecipientSession, reason: payload.Reason}
}

// mpDecodeException reads a delivery exception off the bus.
func mpDecodeException(t *testing.T, data []byte) mpException {
	t.Helper()
	var item contracts.Envelope
	if err := json.Unmarshal(data, &item); err != nil {
		t.Fatalf("decode a delivery exception: %v (%s)", err, data)
	}
	return mpExceptionOf(t, item)
}

// mpNextException is the next delivery exception on subscription that reports a delivery to
// recipient, passing over those reporting a delivery to any other session.
func mpNextException(t *testing.T, subscription *natsgo.Subscription, recipient string, within time.Duration) mpException {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("no delivery exception to %s on %s within %s", recipient, subscription.Subject, within)
		}
		msg, err := subscription.NextMsg(remaining)
		if err != nil {
			t.Fatalf("no delivery exception to %s on %s within %s: %v", recipient, subscription.Subject, within, err)
		}
		if exception := mpDecodeException(t, msg.Data); exception.recipient == recipient {
			return exception
		}
	}
}

// A frame a pod delivered and was killed before it could acknowledge reaches its session at most
// twice, under its own event id, and the repeat comes inside the hard take-over bound. The
// active pod relays a message to session S, S answers it, and the pod is SIGKILLed before the
// acknowledgement reaches the server (the relay swallows it). The standby takes over, restarts
// S's consumer at its ack floor and is handed that message again: nothing says S took it, since
// the reply and the acknowledgement rode the one connection that died with the pod, so it is
// sent once more under the same event id. A restart sends again every frame above the ack floor
// (cmd/listener/README.md, "When a session can see a message twice"), and here that is this one
// message; what this row refuses is a second repeat of it and a repeat of anything else.
func TestTwoPodsRepeatAtMostOnceWhatAKilledPodDelivered(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	const sessionID = "ses_mp_ledger"
	a.registerNATS(t, serving, sessionID)
	publisher := mpConnectPublisher(t, uri)

	swallowed := a.swallowNextAck(t)
	m := publisher.mustPublish(t, sessionID)
	swallowed()
	mpEventually(t, 10*time.Second, a.instance+"'s frame to "+sessionID+" to have landed", func() bool { return serving.framesTo(sessionID) == 1 })
	killed := a.kill(t)
	// The standby's restart at the ack floor after the kill recreates this consumer.
	awaitSessionConsumerStored(t, publisher.client.Conn, sessionID)

	mpEventually(t, mpHardBound, b.instance+" to take the lease over", b.active)
	serving.requireWithin(t, []mpMessage{m}, mpHardBound, killed)
	time.Sleep(mpQuiet)
	took := serving.requireAtMostTwice(t, m, "the frame whose acknowledgement died with "+a.instance)
	serving.requireKnown(t, []mpMessage{m})
	t.Logf("%s took %s %d times across the kill", sessionID, m.event, took)
}

// An attempt, then a crash: the active pod records in the shared ledger that it is delivering to
// session S and is SIGKILLed while that delivery is unanswered (the session holds the frame). The
// standby takes the killed pod's expired lease, so the attempt is a former holder's: it takes the
// attempt over at once rather than deferring it as a live pod's, and S is sent the message, once,
// within the hard take-over bound of the kill (a repeat with the same event id is allowed; the
// held frame was never answered). Before INFRA-557 the durable's 60 s ack wait keeps the message
// from the standby past that bound.
func TestTwoPodsDeliverWhatAKilledPodOnlyAttempted(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	const sessionID = "ses_mp_attempt"
	a.registerNATS(t, serving, sessionID)
	publisher := mpConnectPublisher(t, uri)

	inHand := serving.hold(sessionID)
	m := publisher.mustPublish(t, sessionID)
	select {
	case <-inHand:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never sent %s a frame", a.instance, sessionID)
	}
	killed := a.kill(t)
	// The standby's restart at the ack floor after the kill recreates this consumer.
	awaitSessionConsumerStored(t, publisher.client.Conn, sessionID)

	const bound = mpHardBound
	messages := []mpMessage{m}
	serving.awaitAll(t, messages, bound+5*time.Second)
	serving.requireWithin(t, messages, bound, killed)
	time.Sleep(2 * time.Second)
	serving.requireOnce(t, messages)
	if line := lineWith(b.Output.String(), `"msg":"listener delivery deferred to another pod's attempt"`, `"event_id":"`+m.event+`"`); line != "" {
		t.Fatalf("%s deferred %s as a live pod's attempt, though %s held the lease it took over: %s", b.instance, m.event, a.instance, line)
	}
}

// A message on a team topic that a session on consumer delivery cannot be given, since its
// delivery mode cannot be read, is reported by the fan-out of the pod that holds the machine's
// durable: one delivery_failed exception for that session, on the topic's exception lane, which
// the stream stores (TestTheFanoutReportsARefusalOfASessionOnConsumerDelivery). When that pod is
// SIGKILLed before the durable has its acknowledgement, the standby takes the message over and
// reports it again: the fan-out asks the shared delivery ledger nothing for a recipient whose own
// consumer relays the message (fanoutDelivery), so no record of the first report makes the second
// delivery a dedupe skip. Both reports carry one id, deliveryExceptionID of the event, the
// recipient and the reason, and the stream, which stores the lane under the Nats-Msg-Id of that
// id's dedupe key, holds one. Before INFRA-557 each report minted an id of its own, and the stream
// kept both. A message on an agent subject raises no exception (agentSubjectDelivery), so the row
// reads a team topic's.
//
// The acknowledgement lost is the durable's alone. The active pod stands behind a lane relay,
// which drops every `$JS.ACK.` frame on the pod's receive connection (mpReceiveConnection), the
// one the machine's durable is bound and acknowledged on, and passes those on its session-consumer
// connection (mpSessionConsumerConnection), where the relay of the session's own consumer
// acknowledges the frame the session took. The row requires the session to have taken that frame
// and its consumer to hold nothing unacknowledged before the kill, so the message the standby is
// handed again is the durable's copy.
//
// A second interest in the topic, of a session with no entry, keeps the durable path reading it
// (keepTheDurableReading): with the consumer's interest alone, the durable acknowledges the message
// before it decodes it (durableNeedsDelivery). The fan-out reports that recipient too, on the same
// lane, so the row reads only the reports to the session on consumer delivery.
func TestTwoPodsReportARedeliveredTeamExceptionOnceUnderOneID(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a := mpStartPodBehindLanes(t, binary, uri, "pod-a")
	mpWaitActive(t, a, 30*time.Second)
	b := mpStartPod(t, binary, uri, "pod-b")
	mpWaitStandby(t, b, 30*time.Second)
	serving := mpStartNATSSessions(t, uri)
	publisher := mpConnectPublisher(t, uri)
	const sessionID, reader, topic, reason = "ses_mp_refused", "ses_mp_durable_reader", "notifications.team.mp-refused", "delivery_failed"
	a.registerNATS(t, serving, sessionID, topic)
	registry, err := store.Open(t.Context(), publisher.client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the interest registry: %v", err)
	}
	if _, err := registry.Upsert(store.Interest{SessionID: reader, MachineID: mpMachine}, []string{topic}); err != nil {
		t.Fatalf("register the interest of %s: %v", reader, err)
	}
	for _, pod := range []*mpPod{a, b} {
		mpEventually(t, 10*time.Second, pod.instance+" to hold "+sessionID+"'s entry and both interests in "+topic, func() bool {
			return pod.cachedSession(sessionID) != nil && slices.Contains(pod.cachedTopics(sessionID), topic) && slices.Contains(pod.cachedTopics(reader), topic)
		})
	}
	lane := bus.ExceptionTopicPrefix + topic
	exceptions := publisher.watchExceptions(t, topic)

	t.Cleanup(a.lanes.dropRequests(mpReceiveConnection(a.instance), "$JS.ACK."))
	// The payload's delivery mode is one no session can be shown to advertise (payloadDeliveryMode).
	const event = "evt-mp-refused"
	item := contracts.Envelope{
		EventID:        event,
		Source:         "agent",
		SourceEventID:  event,
		Topic:          topic,
		DedupeKey:      "multiprocess." + event,
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "multiprocess message mp-event=" + event,
		Payload:        `{"delivery":{"mode":"bogus"}}`,
		TraceID:        "trace-" + event,
	}
	ack, err := publisher.client.JS().Publish(topic, marshalListenerEnvelope(t, item))
	if err != nil {
		t.Fatalf("store %s on %s: %v", event, topic, err)
	}
	first := mpNextException(t, exceptions, sessionID, 10*time.Second)
	mpEventually(t, 10*time.Second, sessionID+" to have taken "+event+" from its own consumer's relay, acknowledged", func() bool {
		return serving.framesTo(sessionID) == 1 && publisher.sessionAckPending(sessionID) == 0
	})
	killed := a.kill(t)
	// The standby's restart at the ack floor after the kill recreates this consumer.
	awaitSessionConsumerStored(t, publisher.client.Conn, sessionID)
	// Long enough for a build before INFRA-557 to take the message back, after its 60 s ack wait.
	second := mpNextException(t, exceptions, sessionID, 90*time.Second)
	t.Logf("%s reported %s to %s again %s after %s was killed", b.instance, event, sessionID, time.Since(killed).Round(time.Millisecond), a.instance)
	mpEventually(t, 10*time.Second, b.instance+" to log its own refusal of "+event+" to "+sessionID, func() bool {
		return lineWith(b.Output.String(), `"msg":"listener delivery mode cannot be read unambiguously"`, `"session_id":"`+sessionID+`"`, `"event_id":"`+event+`"`) != ""
	})

	for _, exception := range []mpException{first, second} {
		if exception.event != event || exception.recipient != sessionID || exception.reason != reason {
			t.Fatalf("exception %+v, want %s for %s to %s", exception, reason, event, sessionID)
		}
	}
	if first.id != second.id {
		t.Fatalf("the two reports of %s's %s to %s carry the ids %s and %s, want one", event, reason, sessionID, first.id, second.id)
	}
	// Once the successor has acknowledged the message, the stream has answered each publish of
	// its reports, so a second copy would be stored by now.
	awaitDurableState(t, publisher.client, publisher.consumer, 0, ack.Sequence)
	var stored []string
	for _, held := range storedEnvelopes(t, publisher.client, lane) {
		if exception := mpExceptionOf(t, held.item); exception.event == event && exception.recipient == sessionID && exception.reason == reason {
			stored = append(stored, exception.id)
		}
	}
	if len(stored) != 1 || stored[0] != first.id {
		t.Fatalf("the stream holds the reports %v of %s's %s to %s on %s, want the one under %s", stored, event, reason, sessionID, lane, first.id)
	}
}

// A send and a role claim through the pod whose cache trails: the standby sits behind a 150 ms slow
// link, a session registers through the active pod, and at once a POST /v1/messages/send to it, and
// for another such session a role set, go to the standby. Both answer 200, read from the bucket.
// Before INFRA-557 the standby answers from its cache alone, which has not seen the registration:
// 404 "no live session" and 404 "session is not registered".
//
// The 150 ms is this rig's means to make the standby's cache trail: it delays every byte the server
// sends the standby, so the standby's cache sees each write long after the test has made its next
// request. It is no latency the claim is built for. The role set is a hard claim of a role
// no claim has held, by a claimant the standby's cache does not hold yet: ten NATS round trips up
// to its claim write, the read of the role's delete marker among them, which must land before the
// claim's tries end (roleSetHandler counts them and says how slow a link they fit). The row counts
// those requests through the standby's relay (countedRoleSet) and requires at most ten, a bound no
// server's speed moves; the link alone would not tell an eleventh, which at 150 ms lands at 1.65 s,
// well before that claim's own tries' end at (7 s + 6 x 0.15 s)/3 = 2.63 s.
func TestTwoPodsSendAndRoleSetThroughThePodWhoseCacheTrails(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	b.relay.delay(150 * time.Millisecond)
	const target, claimant, role = "ses_mp_unseen_target", "ses_mp_unseen_claimant", "mp-unseen"

	a.subscribe(t, mpOnConsumer(t, fmt.Sprintf(`{"session_id":%q,"topics":[],"self_subscribed":true}`, target)))
	send := b.call(http.MethodPost, "/v1/messages/send", fmt.Sprintf(`{"target_session":%q,"message":"sent through the other pod","source":"agent","source_session":"ses_mp_sender"}`, target))
	a.subscribe(t, mpOnConsumer(t, fmt.Sprintf(`{"session_id":%q,"topics":[],"self_subscribed":true}`, claimant)))
	claim := b.countedRoleSet(t, claimant, role)
	if send.status != http.StatusOK {
		t.Fatalf("send to %s through %s at once after it registered through %s: %d %s %v, want 200", target, b.instance, a.instance, send.status, strings.TrimSpace(send.body), send.err)
	}
	if claim.status != http.StatusOK {
		t.Fatalf("role set for %s through %s at once after it registered through %s: %d %s %v, want 200", claimant, b.instance, a.instance, claim.status, strings.TrimSpace(claim.body), claim.err)
	}
	claim.requireTrips(t, claimant, 10)
	mpEventually(t, 5*time.Second, a.instance+" to answer "+claimant+" as "+role+"'s holder", func() bool {
		answer := a.call(http.MethodGet, "/v1/roles/"+role, "")
		return answer.status == http.StatusOK && strings.Contains(answer.body, `"holder":"`+claimant+`"`)
	})
	t.Logf("the send answered in %s and the role set in %s", send.took.Round(time.Millisecond), claim.took.Round(time.Millisecond))
}

// Writes through the pod whose cache trails keep the other pod's newer write: the standby sits
// behind a 250 ms slow link, so a write through the active pod reaches its cache 250 ms late. The
// role set here is a hard claim of a role no claim has held, by a claimant the standby's cache
// holds: nine NATS round trips up to its claim write, four of them before the store
// (roleSetHandler counts them and says how slow a link they fit). At 250 ms the ninth lands at
// 2.25 s and the server's time, 0.42 s before the claim's tries end at (7 s + 4 x 0.25 s)/3 =
// 2.67 s. The row counts those requests through the standby's relay (countedRoleSet) and requires
// at most nine, because the link alone cannot tell a tenth: with the claimant's read put back
// before the refresh, ten land at 2.5 s, 0.25 s before that claim's own tries' end at (7 s + 5 x
// 0.25 s)/3 = 2.75 s, so it would pass on timing, and the count fails it by one request whatever
// the server's speed. The row also requires the standby's cache to still hold the older host as
// the role set begins, so the newer host the role set keeps is one that cache has not seen. A
// heartbeat on the standby within that window, of a process that does not drive the session,
// keeps the driving claim written through the active pod; a heartbeat on the standby keeps a topic
// added through the active pod; and a role set on the standby keeps the newer host recorded
// through the active pod. Before INFRA-557 the standby wrote its stale copy over each.
func TestTwoPodsWritesOnThePodWhoseCacheTrailsKeepTheNewerWrite(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	b.relay.delay(250 * time.Millisecond)
	publisher := mpConnectPublisher(t, uri)

	t.Run("a topic", func(t *testing.T) {
		const sessionID, first, added = "ses_mp_cas_topic", "notifications.team.mp-first", "notifications.team.mp-added"
		heartbeat := mpOnConsumer(t, fmt.Sprintf(`{"session_id":%q,"topics":[%q],"self_subscribed":true}`, sessionID, first))
		a.subscribe(t, heartbeat)
		mpEventually(t, 10*time.Second, b.instance+" to hold "+sessionID+"'s interest", func() bool {
			return slices.Contains(b.cachedTopics(sessionID), first)
		})
		a.subscribe(t, mpOnConsumer(t, fmt.Sprintf(`{"session_id":%q,"topics":[%q],"self_subscribed":true}`, sessionID, added)))
		b.subscribe(t, heartbeat)
		stored := publisher.stored(t, bus.InterestBucket, sessionID)
		topics, _ := stored["topics"].([]any)
		if !slices.Contains(topics, any(first)) || !slices.Contains(topics, any(added)) {
			t.Fatalf("%s's interest after a heartbeat on %s: %v, want %s and %s", sessionID, b.instance, stored, first, added)
		}
	})

	t.Run("a host", func(t *testing.T) {
		const sessionID, role = "ses_mp_cas_host", "mp-cas-host"
		register := func(host string) {
			t.Helper()
			a.subscribe(t, mpOnConsumer(t, fmt.Sprintf(`{"session_id":%q,"topics":[],"self_subscribed":true,"host":%q}`, sessionID, host)))
		}
		register("example-host-a")
		mpEventually(t, 10*time.Second, b.instance+" to hold "+sessionID+" on example-host-a", func() bool {
			row := b.cachedSession(sessionID)
			return row != nil && row["host"] == "example-host-a"
		})
		register("example-host-b")
		// The standby's cache must still hold example-host-a as the role set begins, the entry the
		// claim reads its claimant from (liveSessionEntry): with the cache caught up, the role set
		// would keep the newer host with nothing trailing, and the row would show nothing. The check
		// reads the cache at the role set's start, not at its write: the write follows the refresh's
		// leader read, by when the cache can hold example-host-b, and the role set keeps the newer
		// host there because it writes from the leader's record. The check fails, naming the cause,
		// only when about 240 ms pass between example-host-b's registration answering and this read,
		// and it never passes with no trail.
		if row := b.cachedSession(sessionID); row == nil || row["host"] != "example-host-a" {
			t.Fatalf("%s's cache holds %v for %s right before the role set, want example-host-a: it no longer trails %s's write of example-host-b", b.instance, row, sessionID, a.instance)
		}
		claim := b.countedRoleSet(t, sessionID, role)
		if claim.status != http.StatusOK {
			t.Fatalf("claim %s for %s through %s: status %d %s %v, want 200", role, sessionID, b.instance, claim.status, strings.TrimSpace(claim.body), claim.err)
		}
		claim.requireTrips(t, sessionID, 9)
		if stored := publisher.stored(t, bus.SessionBucket, sessionID); stored["host"] != "example-host-b" || stored["self_subscribed"] != true {
			t.Fatalf("%s's registration after a role set on %s: %v, want host example-host-b, self-subscribed", sessionID, b.instance, stored)
		}
	})
}

// A role claim whose holder is gone is answered alike by every pod, whenever each started: the
// active pod has served for a while when the claim is made and its holder's session then ends,
// and a standby starts after. Each answers the same holder, the same process holding the claim,
// the same last_seen and the same `claim_released: false`, and the claim itself stands: a lookup
// releases nothing whatever its holder's state, and only the role claim reaper, on the pod
// holding the lease, takes one away.
//
// The holder's process (`client_instance_id`) is the claim record's own, which lives in the
// shared role bucket, so the pod that never took the claim answers it as the pod that did: no
// pod answers a process out of its own memory. Before INFRA-557 the lookup released the claim
// and each pod counted its grace from its own start, so the pod that started after the claim
// retained it while the other released it; the grace that reaper now holds a restored claim for
// is the machine's own service start (internal/store's grace_test.go).
func TestTwoPodsAnswerARoleWhoseHolderLapsedAlike(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	publisher := mpConnectPublisher(t, uri)
	a := mpStartPod(t, binary, uri, "pod-a")
	mpWaitActive(t, a, 30*time.Second)
	time.Sleep(2 * time.Second) // the machine serves for a while before the claim
	const holder = "ses_mp_grace_holder"
	const holdingProcess = "inst-mp-grace-holder"
	a.subscribe(t, mpOnConsumer(t, fmt.Sprintf(`{"session_id":%q,"topics":[],"self_subscribed":true,"client_instance_id":%q}`, holder, holdingProcess)))
	a.setRoleAs(t, holder, "mp-grace", holdingProcess)
	if answer := a.call(http.MethodDelete, "/v1/sessions/"+holder, ""); answer.status != http.StatusOK {
		t.Fatalf("end %s's session through %s: %d %s %v", holder, a.instance, answer.status, answer.body, answer.err)
	}
	b := mpStartPod(t, binary, uri, "pod-b")
	mpWaitStandby(t, b, 30*time.Second)

	onA := a.lapsedRole(t, "mp-grace")
	onB := b.lapsedRole(t, "mp-grace")
	requireSameLapsedAnswer(t, a, b, onA, onB, "holder", "holder_state", "claim_released", "last_seen", "client_instance_id")
	if onA["client_instance_id"] != holdingProcess {
		t.Fatalf("%s answers client_instance_id %v, want the process the claim names, %s: the holder's process is the claim record's and not the answering pod's: %v",
			a.instance, onA["client_instance_id"], holdingProcess, onA)
	}
	if onA["claim_released"] != false {
		t.Fatalf("%s released a claim on a lookup: %v", a.instance, onA)
	}
	if claim := publisher.stored(t, bus.RoleClaimBucket, "mp-grace"); claim["holder_session_id"] != holder || claim["client_instance_id"] != holdingProcess {
		t.Fatalf("the role bucket holds %v after both pods looked mp-grace up; want %s's claim of process %s", claim, holder, holdingProcess)
	}
}

// A role holder whose session expired while only the active pod ran is reported with one last_seen
// by a standby started after it lapsed. The session bucket is made with a 10 s lifetime, as
// startup_test.go's buckets are, so the registration expires within the row. Before INFRA-557 the
// last-seen time lived in the memory of a pod whose cache had held the session, and the standby
// reported none.
func TestTwoPodsReportALapsedHoldersLastSeenAlike(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	publisher := mpConnectPublisher(t, uri)
	const ttl = 10 * time.Second
	sessionKV, err := publisher.client.JS().CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.SessionBucket, TTL: ttl, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create the session bucket with a %s lifetime: %v", ttl, err)
	}
	a := mpStartPod(t, binary, uri, "pod-a")
	mpWaitActive(t, a, 30*time.Second)
	const holder = "ses_mp_seen_holder"
	a.subscribe(t, mpOnConsumer(t, fmt.Sprintf(`{"session_id":%q,"topics":[],"self_subscribed":true}`, holder)))
	a.setRole(t, holder, "mp-seen-a")
	a.setRole(t, holder, "mp-seen-b")
	mpEventually(t, ttl+10*time.Second, holder+"'s registration to expire", func() bool {
		_, err := sessionKV.Get(holder)
		return errors.Is(err, natsgo.ErrKeyNotFound)
	})
	b := mpStartPod(t, binary, uri, "pod-b")
	mpWaitStandby(t, b, 30*time.Second)

	onA := a.lapsedRole(t, "mp-seen-a")
	onB := b.lapsedRole(t, "mp-seen-b")
	if onA["last_seen"] == nil {
		t.Fatalf("%s reports no last_seen for %s: %v", a.instance, holder, onA)
	}
	requireSameLapsedAnswer(t, a, b, onA, onB, "holder", "last_seen", "claim_released")
}

// Both sides of this merge in one row: the relay of a session's own consumer, and the delivery
// lease released by a holder whose receive connection is down.
//
// A session's first messages are delivered by the active pod, from the relay of the session's
// own consumer, and the acknowledgements that relay sends never reach that consumer: the lane
// relay drops every `$JS.ACK.` the pod's session-consumer connection carries. So the consumer
// still holds all three, above its ack floor. The pod's receive connection is then cut: it
// releases the lease, its relays stop with the receive term, the standby takes the lease and
// starts the session's relay, which restarts the consumer at that floor and is given the three
// messages again. The messages published after the take-over reach the machine through that
// standby alone.
//
// Nothing is lost, because the lease the cut pod cannot deliver on is released to a pod that
// can: without that release the cut pod holds it and the standby never serves the machine at
// all. The three the drop left unacknowledged reach the session a second time, under their own
// event ids and no other: a frame's reply and its acknowledgement ride the one connection, so a
// drop that loses the acknowledgement leaves no pod able to say the session took it. Everything
// published after the take-over arrives exactly once.
func TestTwoPodsEveryQueuedMessageReachesTheSessionAtMostTwiceAcrossAReceiveLostTakeOver(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a := mpStartPodBehindLanes(t, binary, uri, "pod-a")
	mpWaitActive(t, a, 30*time.Second)
	b := mpStartPod(t, binary, uri, "pod-b")
	mpWaitStandby(t, b, 30*time.Second)
	serving := mpStartNATSSessions(t, uri)
	const sessionID = "ses_mp_queued_once"
	a.registerNATS(t, serving, sessionID)
	publisher := mpConnectPublisher(t, uri)

	// Every acknowledgement the active pod's relays send is dropped, so the messages it delivers
	// stay above its session consumer's ack floor and the standby's relay is given them again.
	t.Cleanup(a.lanes.dropRequests(mpSessionConsumerConnection, "$JS.ACK."))

	var delivered []mpMessage
	for range 3 {
		delivered = append(delivered, publisher.mustPublish(t, sessionID))
	}
	serving.awaitAll(t, delivered, 20*time.Second)

	// The standby's restart at the ack floor after the cut below recreates this consumer.
	awaitSessionConsumerStored(t, publisher.client.Conn, sessionID)

	heal := a.lanes.cut(mpReceiveConnection(a.instance))
	cut := time.Now()
	t.Cleanup(heal)
	mpEventually(t, mpHardBound, b.instance+" to take the lease over", b.active)
	taken := time.Since(cut)
	if released := lineWith(a.Output.String(), `"msg":"listener lease released: the receive connection is down"`); released == "" {
		t.Fatalf("%s kept the lease its lost receive connection cannot deliver on:\n%s", a.instance, mpTail(mpLinesWithout(a.Output.String()), 40))
	}

	// Published once the cut pod relays nothing: only the standby can deliver them.
	var afterTheTakeOver []mpMessage
	for range 3 {
		afterTheTakeOver = append(afterTheTakeOver, publisher.mustPublish(t, sessionID))
	}
	serving.awaitAll(t, afterTheTakeOver, mpHardBound+5*time.Second)
	serving.requireWithin(t, afterTheTakeOver, mpHardBound, cut)

	messages := append(slices.Clone(delivered), afterTheTakeOver...)
	// Past the durable's ack wait, so a message a pod took and held back would have come again.
	time.Sleep(mpQuiet)
	serving.requireOnce(t, afterTheTakeOver)
	for _, m := range delivered {
		serving.requireAtMostTwice(t, m, "a frame whose acknowledgement the drop lost")
	}
	serving.requireKnown(t, messages)
	serving.requireOrder(t, messages)
	t.Logf("%s took the lease %s after the cut of %s's receive connection; the %d messages %s had delivered were given back, the %d published after the take-over arrived, and nothing came back under an id no message carried",
		b.instance, taken.Round(time.Millisecond), a.instance, len(delivered), a.instance, len(afterTheTakeOver))
}

// sessionState is the state and the last registration a pod answers for sessionID on
// GET /v1/registry/<session_id>?include=unresponsive: the 200's own two fields while the session
// is live or unresponsive, and the 404's, which carry the same two, once it is gone.
func (p *mpPod) sessionState(t *testing.T, sessionID string) (string, int64) {
	t.Helper()
	answer := p.call(http.MethodGet, "/v1/registry/"+sessionID+"?include=unresponsive", "")
	var body struct {
		State    string `json:"state"`
		LastSeen int64  `json:"last_seen"`
	}
	if answer.err != nil || (answer.status != http.StatusOK && answer.status != http.StatusNotFound) ||
		json.Unmarshal([]byte(answer.body), &body) != nil {
		t.Fatalf("GET /v1/registry/%s on %s: %d %s %v", sessionID, p.instance, answer.status, strings.TrimSpace(answer.body), answer.err)
	}
	return body.State, body.LastSeen
}

// A session that stops answering is in one state on every pod of its machine, and is dated alike
// by each. It registers through the active pod and then heartbeats no more: both pods read it
// live inside the live window, unresponsive past that window, and gone past the horizon, each
// with one last_seen, the registration's own moment. Neither pod reads its own cache for any of
// that — the state is the record's age (session.StateOf) and the time is the `seen.` record both
// pods share (session.WithLastSeenBucket) — so a pod that never watched the session answers as
// the pod that took its registration does.
//
// The claim that session holds is released by neither pod while it is unresponsive: no lookup
// takes a claim away whatever its holder's state, and the role claim reaper, the one path that
// does, acts only on a holder that is gone. Both pods answer that 404 alike, and the claim record
// is still in the role bucket after both have looked it up.
//
// The windows are the deployment's own settings, set short so the row runs in seconds. The
// bucket's maximum age is left far longer than the horizon, as a deployment that means the
// horizon sets it, and both reapers' periods are set past the row, so nothing but the record's
// own age moves a state here.
func TestTwoPodsReportOneSessionStateAndLastSeen(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	publisher := mpConnectPublisher(t, uri)
	const (
		liveWindow  = 5 * time.Second
		goneHorizon = 20 * time.Second
	)
	windows := []string{
		"ENVOY_SESSION_LIVE_WINDOW=" + liveWindow.String(),
		"ENVOY_SESSION_GONE_HORIZON=" + goneHorizon.String(),
		"ENVOY_SESSION_MAX_AGE=10m",
		"ENVOY_SESSION_REAP_INTERVAL=1h",
		"ENVOY_ROLE_CLAIM_REAP_INTERVAL=1h",
	}
	a := mpStartPod(t, binary, uri, "pod-a", windows...)
	mpWaitActive(t, a, 30*time.Second)
	b := mpStartPod(t, binary, uri, "pod-b", windows...)
	mpWaitStandby(t, b, 30*time.Second)

	const (
		holder = "ses_mp_state"
		role   = "mp-state"
	)
	a.subscribe(t, mpOnConsumer(t, fmt.Sprintf(`{"session_id":%q,"topics":[],"self_subscribed":true}`, holder)))
	a.setRole(t, holder, role)

	// readState waits for both pods to put the session in want and answers the last_seen they
	// agree on, failing the row when they date it differently.
	readState := func(want string) int64 {
		t.Helper()
		var agreed int64
		mpEventually(t, goneHorizon+30*time.Second, "both pods to read "+holder+" "+want, func() bool {
			stateA, seenA := a.sessionState(t, holder)
			stateB, seenB := b.sessionState(t, holder)
			if stateA != want || stateB != want {
				return false
			}
			if seenA != seenB {
				t.Fatalf("%s dates %s %d and %s dates it %d; every pod answers one last_seen for one session",
					a.instance, holder, seenA, b.instance, seenB)
			}
			agreed = seenA
			return true
		})
		return agreed
	}

	live := readState("live")
	if live == 0 {
		t.Fatalf("%s is live and dated by neither pod", holder)
	}

	unresponsive := readState("unresponsive")
	if unresponsive != live {
		t.Fatalf("%s's last_seen moved from %d to %d while it only stopped answering", holder, live, unresponsive)
	}
	onA, onB := a.lapsedRole(t, role), b.lapsedRole(t, role)
	requireSameLapsedAnswer(t, a, b, onA, onB, "holder", "holder_state", "last_seen", "claim_released")
	if onA["holder_state"] != "unresponsive" || onA["claim_released"] != false {
		t.Fatalf("GET /v1/roles/%s while its holder is unresponsive = %v; want holder_state unresponsive and claim_released false", role, onA)
	}
	if claim := publisher.stored(t, bus.RoleClaimBucket, role); claim["holder_session_id"] != holder {
		t.Fatalf("the role bucket holds %v after both pods looked %s up; want %s's claim, which no lookup releases", claim, role, holder)
	}

	gone := readState("gone")
	if gone != live {
		t.Fatalf("%s's last_seen moved from %d to %d once it was gone; the record's own registration is what dates it", holder, live, gone)
	}
	t.Logf("both pods read %s live, then unresponsive, then gone, each dating it %d, and neither released %s while it was unresponsive", holder, live, role)
}
