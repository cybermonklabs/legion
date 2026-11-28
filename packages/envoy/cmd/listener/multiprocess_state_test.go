//go:build multiprocess

// The rows of the two-pod suite (multiprocess_harness_test.go) about the state the machine's pods
// share: the writes a pod whose cache trails must not lose, a pod-local address another pod
// accepted, and the role and last-seen answers every pod must give alike. Its first helpers read a
// pod's log, its cache and the buckets, which these rows do more than the take-over ones.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
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
// it, and which the refresh reads from the stream's leader and writes; the role's claim, which the
// claim reads, reads for its delete marker and writes; and the claimant's interest record, which
// the claim reads and writes, and reads once more after its tries to confirm the role's topic. The
// pod's own traffic names none of them: its lease record's reads every half second are on
// lease.<durable> in the listener state bucket, and its watchers' consumer creates, flow control
// replies and stream info reads name no key at all.
func roleSetKeys(sessionID, role string) []string {
	return []string{
		"$KV." + bus.SessionBucket + "." + sessionID,
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

// A send and a role claim through the pod whose cache trails: the standby sits behind a 150 ms slow
// link, a session registers through the active pod, and at once a POST /v1/messages/send to it, and
// for another such session a role set, go to the standby. Both answer 200, read from the bucket.
// Before INFRA-557 the standby answers from its cache alone, which has not seen the registration:
// 404 "no live session" and 404 "session is not registered".
//
// The 150 ms is this rig's means to make the standby's cache trail: it delays every byte the server
// sends the standby, so the standby's cache sees each write long after the test has made its next
// request. It is no latency the claim is built for. The role set is a hard claim of a role
// no claim has held, by a claimant the standby's cache does not hold yet: eight NATS round trips up
// to its claim write, the read of the role's delete marker among them, which must land before the
// claim's tries end (roleSetHandler counts them and says how slow a link they fit). The row counts
// those requests through the standby's relay (countedRoleSet) and requires at most eight, a bound
// no server's speed moves; the link alone would not tell a ninth, which at 150 ms lands at 1.35 s,
// well before that claim's own tries' end at (7 s + 4 x 0.15 s)/3 = 2.53 s.
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
	claim.requireTrips(t, claimant, 8)
	mpEventually(t, 5*time.Second, a.instance+" to answer "+claimant+" as "+role+"'s holder", func() bool {
		answer := a.call(http.MethodGet, "/v1/roles/"+role, "")
		return answer.status == http.StatusOK && strings.Contains(answer.body, `"holder":"`+claimant+`"`)
	})
	t.Logf("the send answered in %s and the role set in %s", send.took.Round(time.Millisecond), claim.took.Round(time.Millisecond))
}

// Writes through the pod whose cache trails keep the other pod's newer write: the standby sits
// behind a 250 ms slow link, so a write through the active pod reaches its cache 250 ms late. The
// role set here is a hard claim of a role no claim has held, by a claimant the standby's cache
// holds: seven NATS round trips up to its claim write, two of them before the store
// (roleSetHandler counts them and says how slow a link they fit). At 250 ms the seventh lands at
// 1.75 s and the server's time, 0.75 s before the claim's tries end at (7 s + 2 x 0.25 s)/3 =
// 2.5 s. The row counts those requests through the standby's relay (countedRoleSet) and requires
// at most seven, because the link alone cannot tell an eighth: with the claimant's read put back
// before the refresh, eight land at 2 s, 0.58 s before that claim's own tries' end at (7 s + 3 x
// 0.25 s)/3 = 2.58 s, so it would pass on timing, and the count fails it by one request whatever
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
		claim.requireTrips(t, sessionID, 7)
		if stored := publisher.stored(t, bus.SessionBucket, sessionID); stored["host"] != "example-host-b" || stored["self_subscribed"] != true {
			t.Fatalf("%s's registration after a role set on %s: %v, want host example-host-b, self-subscribed", sessionID, b.instance, stored)
		}
	})
}

// A role claim whose holder is gone is answered alike by every pod, whenever each started: the
// active pod has served for a while when the claim is made and its holder's session then ends,
// and a standby starts after. Each answers the same holder, the same process holding the claim
// and the same `claim_released: false`, and the claim itself stands: a lookup releases nothing
// whatever its holder's state, and only the role claim reaper, on the pod holding the lease, takes
// one away.
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
	requireSameLapsedAnswer(t, a, b, onA, onB, "holder", "holder_state", "claim_released", "client_instance_id")
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
// that — the state is the record's age (session.StateOf) and the time is the record's own
// registration — so a pod that never watched the session answers as the pod that took its
// registration does.
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
