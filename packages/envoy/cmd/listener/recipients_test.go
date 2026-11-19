package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// recipientSender is the session every publish in these tests names as its source: it follows the
// topic it publishes on, so each count shows that the sender itself is never a recipient.
const recipientSender = "ses_sender"

// recipientRig is the publish route over one NATS server, with the known-session store the counts
// read on a clock of its own (knownSessionRig), as main wires the route with every store open.
type recipientRig struct {
	*knownSessionRig
}

func newRecipientRig(t *testing.T) *recipientRig {
	t.Helper()
	return &recipientRig{knownSessionRig: newKnownSessionRig(t)}
}

// register runs one registration of sessionID on topics through the real route, as a plugin in
// consumer mode sends it: that writes the interest row, the session entry, the session's consumer
// and the known-session record.
func (rig *recipientRig) register(t *testing.T, sessionID string, topics []string) {
	t.Helper()
	rig.knownSessionRig.register(t, sessionID, map[string]any{"topics": topics, "title": "a counted session"})
}

// unsubscribe drops topics from sessionID through the real route, as the envoy_unsubscribe tool
// does.
func (rig *recipientRig) unsubscribe(t *testing.T, sessionID string, topics []string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"session_id": sessionID, "topics": topics})
	if err != nil {
		t.Fatal(err)
	}
	rig.post(t, "/v1/interests/unsubscribe", string(body))
}

// reapInterest removes sessionID's interest row, as the reaper removes the row of a session that
// has been dead longer than the grace window plus store.InterestRefreshAge
// (store.Registry.Reap). It deletes the row through store.Registry.Remove rather than driving a
// reap pass, which would need the registry's own clock moved past that allowance: both go through
// deleteInterest, so the state they leave - no row in the bucket and none in the cache - is the
// same, and that state is what the count reads.
func (rig *recipientRig) reapInterest(t *testing.T, sessionID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rig.harness.registry.Remove(ctx, sessionID, nil, store.RoleReleaseAuthority{}); err != nil {
		t.Fatalf("remove %s's interest row: %v", sessionID, err)
	}
	if _, cached := rig.harness.registry.CachedInterest(sessionID); cached {
		t.Fatalf("%s still has an interest row, so it is not in the state the reaper leaves", sessionID)
	}
}

// publish sends one message on topic from recipientSender and fails on any answer but 200.
func (rig *recipientRig) publish(t *testing.T, topic, key string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"topic":           topic,
		"message":         "who is listening",
		"idempotency_key": key,
		"source_session":  recipientSender,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder, elapsed := publishThrough(rig.deps, string(body))
	if recorder.Code != http.StatusOK {
		t.Fatalf("publish on %s: status %d after %s; body %s", topic, recorder.Code, elapsed, recorder.Body.String())
	}
	return recorder
}

// storeSessionEntryOf writes sessionID's interest row on topic and its session entry, last written
// at heartbeat, straight into the stores, as a listener of a release older than the known-session
// records left them: an interest row and an entry, and no record. The entry's age is what the
// session registry judges it by (session.StateOf), so a heartbeat older than the live window is an
// unresponsive session.
func (rig *recipientRig) storeSessionEntryOf(t *testing.T, sessionID, topic string, heartbeat time.Time, want session.State) {
	t.Helper()
	if _, err := rig.harness.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "other-machine"}, []string{topic}); err != nil {
		t.Fatalf("register %s's interest: %v", sessionID, err)
	}
	storeSessionHeartbeat(t, rig.harness.client, sessionID, heartbeat)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, state := rig.harness.sessions.Lookup(sessionID); state == want {
			break
		}
		if time.Now().After(deadline) {
			_, state := rig.harness.sessions.Lookup(sessionID)
			t.Fatalf("%s reads %s in the session cache, want %s", sessionID, state, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if record, known := rig.known.Get(sessionID); known {
		t.Fatalf("%s has the known-session record %#v, so it is not the older listener's session", sessionID, record)
	}
}

// assertCounts fails unless the answer carries both recipient counts, with these values.
func assertCounts(t *testing.T, recorder *httptest.ResponseRecorder, wantTotal, wantConnected int) {
	t.Helper()
	var answer listenerwire.PublishResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if answer.Recipients == nil || answer.RecipientsConnected == nil {
		t.Fatalf("answer %s, want recipients %d and recipients_connected %d", recorder.Body.String(), wantTotal, wantConnected)
	}
	if *answer.Recipients != wantTotal || *answer.RecipientsConnected != wantConnected {
		t.Fatalf("answer %s, want recipients %d and recipients_connected %d", recorder.Body.String(), wantTotal, wantConnected)
	}
}

// assertNoCounts fails when the answer carries either count key at all. It reads the raw JSON
// object: a decoded zero and an absent key read alike, and absent is what an older client and a
// listener with no known-session store must send.
func assertNoCounts(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	for _, key := range []string{"recipients", "recipients_connected"} {
		if raw, carried := answer[key]; carried {
			t.Fatalf("the answer carries %s: %s; want the key left out entirely, body %s", key, raw, recorder.Body.String())
		}
	}
}

// assertRecipientsAlone fails unless the answer carries recipients, with this value, and leaves
// recipients_connected out entirely. It reads the raw JSON object, as assertNoCounts does: a key
// left out and one sent as null decode alike, and left out is what the route must send.
func assertRecipientsAlone(t *testing.T, recorder *httptest.ResponseRecorder, wantTotal int) {
	t.Helper()
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if total, carried := answer["recipients"]; !carried || string(total) != strconv.Itoa(wantTotal) {
		t.Fatalf("answer %s, want recipients %d", recorder.Body.String(), wantTotal)
	}
	if raw, carried := answer["recipients_connected"]; carried {
		t.Fatalf("the answer carries recipients_connected: %s; want the key left out entirely, body %s", raw, recorder.Body.String())
	}
}

// countedCopy is a cache the counts read, as the test drives it: the hooks main drives it by
// (kvCache) and the servability the counts check (recipientCounts).
type countedCopy interface {
	kvCache
	Servable() error
}

// watchCopiesOn puts the stale episodes of the interest and session copies the counts read on
// clock: each store's cache watcher is replaced by one that reads the time from clock
// (replaceWatcher), as the list rig's are, and both replacements' first reads are waited for, so
// each copy can be served until the test disconnects it. It runs right after newRecipientRig,
// before any route has read either store.
func (rig *recipientRig) watchCopiesOn(t *testing.T, clock *listenerClock) map[string]countedCopy {
	t.Helper()
	unwrapped := func(kv jetstream.KeyValue) jetstream.KeyValue { return kv }
	replaceWatcher(t, rig.harness.registry, "interest registry", unwrapped, kvwatch.WithNow(clock.Now))
	replaceWatcher(t, rig.harness.sessions, "session registry", unwrapped, kvwatch.WithNow(clock.Now))
	copies := map[string]countedCopy{"interest": rig.harness.registry, "session": rig.harness.sessions}
	awaitCopiesServable(t, copies, "the interest and session copies to complete their first reads")
	return copies
}

// awaitCopiesServable waits until every one of copies can be served.
func awaitCopiesServable(t *testing.T, copies map[string]countedCopy, what string) {
	t.Helper()
	waitFor(t, 10*time.Second, what, func() bool {
		for _, cache := range copies {
			if cache.Servable() != nil {
				return false
			}
		}
		return true
	})
}

// A publish answers with the sessions the topic is for and how many of them are running now:
// every session that follows it, whether it is registered now, stopped, or registered through a
// listener older than the known-session records, counted once however many stores name it, and
// never the sender itself. Running now is a session the registry holds live by its entry's age: an
// unresponsive session is counted and not running. Every topic a publish reaches a session on is
// stored, a team topic and a topic of another of the stream's subjects alike, and both carry the
// counts.
func TestAPublishCountsEverySessionTheTopicIsForOtherThanItsSender(t *testing.T) {
	rig := newRecipientRig(t)
	const topic = "notifications.team.recipient-count"
	if !bus.StreamCaptures(topic) {
		t.Fatalf("no stream subject captures %s, so a publish on it is refused and this test covers nothing", topic)
	}

	// The sender follows the topic too, and no count below includes it.
	rig.register(t, recipientSender, []string{topic})
	assertCounts(t, rig.publish(t, topic, "count-0"), 0, 0)

	// A session registered now: an interest row and a known-session record both name it, and it is
	// counted once.
	rig.register(t, "ses_both", []string{topic})
	if _, known := rig.known.Get("ses_both"); !known {
		t.Fatal("ses_both has no known-session record, so the count has only one store to dedupe")
	}
	if _, cached := rig.harness.registry.CachedInterest("ses_both"); !cached {
		t.Fatal("ses_both has no interest row, so the count has only one store to dedupe")
	}
	assertCounts(t, rig.publish(t, topic, "count-1"), 1, 1)

	// A session that registered through a listener of a release older than the known-session
	// records: an interest row and a live entry, and no record, as that listener left it.
	rig.storeSessionEntryOf(t, "ses_row_only", topic, time.Now(), session.Live)

	// A session that stopped answering: its entry is past the live window and inside the gone
	// horizon, so it is counted and not running.
	rig.storeSessionEntryOf(t, "ses_unresponsive", topic, time.Now().Add(-(session.LiveWindow + time.Minute)), session.Unresponsive)

	// A session that stopped cleanly more than 20 minutes ago: its entry is gone and the reaper has
	// taken its interest row, leaving its record alone to say it follows the topic.
	rig.register(t, "ses_stopped", []string{topic})
	rig.clock = rig.clock.Add(21 * time.Minute)
	rig.stop(t, "ses_stopped")
	rig.reapInterest(t, "ses_stopped")
	if _, known := rig.known.Get("ses_stopped"); !known {
		t.Fatal("the clean stop took ses_stopped's known-session record, so nothing is left to count it by")
	}

	assertCounts(t, rig.publish(t, topic, "count-4"), 4, 2)

	// A topic of another of the stream's subjects carries the counts the same way.
	const slack = "notifications.slack.channel.c-recipient-count"
	if !bus.StreamCaptures(slack) {
		t.Fatalf("no stream subject captures %s, so this part covers nothing", slack)
	}
	rig.register(t, recipientSender, []string{topic, slack})
	rig.register(t, "ses_watcher", []string{slack})
	assertCounts(t, rig.publish(t, slack, "stream-1"), 1, 1)
}

// A session that leaves a topic stops being counted as a recipient of it. A publish counts the
// sessions whose subscriptions match the topic at the publish, so an unsubscribe must take the
// topic out of the session's known-session record as well as out of its interest row: the record
// is the count's other store, and a stale one keeps telling a sender that a session gets a
// message it has stopped following. The unsubscribe route writes the record whatever the
// session's entry records (recordFollowedTopics), beside the refollow that sets its consumer's
// filter (TestARefollowWritesTheKnownSessionRecord). After a clean stop nothing registers again,
// so a record left stale here keeps the wrong count for the record's whole 30 days.
func TestASessionThatUnsubscribesStopsBeingCounted(t *testing.T) {
	rig := newRecipientRig(t)
	const topic = "notifications.slack.channel.c-unsubscribed-count"
	if !bus.StreamCaptures(topic) {
		t.Fatalf("no stream subject captures %s, so this test no longer covers the stream-captured branch", topic)
	}

	rig.register(t, recipientSender, []string{topic})
	rig.register(t, "ses_leaver", []string{topic})
	assertCounts(t, rig.publish(t, topic, "unsubscribed-1"), 1, 1)

	rig.clock = rig.clock.Add(session.KnownSessionRefreshAge + time.Minute)
	rig.unsubscribe(t, "ses_leaver", []string{topic})
	if interest, cached := rig.harness.registry.CachedInterest("ses_leaver"); cached && slices.Contains(interest.Topics, topic) {
		t.Fatalf("ses_leaver's interest row still holds %q, so the unsubscribe did not take it", topic)
	}
	if record, known := rig.known.Get("ses_leaver"); known && slices.Contains(record.Topics, topic) {
		t.Errorf("ses_leaver's known-session record still holds the unsubscribed %q: its topics are %v", topic, record.Topics)
	}
	assertCounts(t, rig.publish(t, topic, "unsubscribed-2"), 0, 0)

	// The clean stop moves the record on without changing its topics, so a record left stale above
	// is still counted once nothing registers again.
	rig.stop(t, "ses_leaver")
	assertCounts(t, rig.publish(t, topic, "unsubscribed-3"), 0, 0)
}

// A role topic carries neither count: its answer names the holder the message was forwarded to,
// which is what a role send's sender reads.
func TestAPublishToARoleTopicCarriesNeitherCount(t *testing.T) {
	rig := newRecipientRig(t)
	const role = "recipient-count-holder"
	claimRoleFor(t, rig.harness.registry, rig.harness.sessions, "ses_holder", role, nil)
	startStandIn(t, rig.url, "ses_holder")
	// The role topic's holder is the one session following it, and the known-session store
	// answers, so a count is there to make: its absence below is the role branch's, not the
	// store's.
	if !rig.known.Opened() {
		t.Fatal("the known-session store does not answer, so this test is the one with no store")
	}

	recorder := rig.publish(t, contracts.RoleTopicPrefix+role, "role-1")
	var answer listenerwire.PublishResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if answer.Holder != "ses_holder" {
		t.Fatalf("answer %s, want the holder ses_holder", recorder.Body.String())
	}
	assertNoCounts(t, recorder)
}

// A listener with no known-session store answers every publish with neither count rather than
// with the live sessions alone: its sender sees an answer with no count key at all. The same
// session, on a listener whose store answers, is counted, so the absence is the store's.
func TestAListenerWithNoKnownSessionStoreCarriesNeitherCount(t *testing.T) {
	rig := newRecipientRig(t)
	const topic = "notifications.team.degraded-count"
	rig.register(t, "ses_subscriber", []string{topic})
	assertCounts(t, rig.publish(t, topic, "degraded-0"), 1, 1)

	rig.deps.known = nil
	assertNoCounts(t, rig.publish(t, topic, "degraded-1"))
}

// A count is sent only while every cache it is read from can be served (recipientCounts), the
// rule the known-session store's own check applies. A copy that stopped following its bucket, as
// each does at a disconnect, is served for the stale bound and not past it. Past it, a stale
// session copy leaves recipients_connected out of the answer and keeps recipients, which it plays
// no part in; a stale interest copy leaves both out, since recipients is the union of its rows
// with the known-session records, and the sessions connected are among those recipients. The
// known-session store answers throughout, so each absence is the stale copy's, and a read of the
// bucket that completes serves that copy, and both counts, again.
func TestAPublishCountsOnlyFromCopiesThatCanBeServed(t *testing.T) {
	for _, stale := range []string{"session", "interest"} {
		t.Run(stale+" copy", func(t *testing.T) {
			rig := newRecipientRig(t)
			clock := &listenerClock{now: time.Now()}
			copies := rig.watchCopiesOn(t, clock)
			const topic = "notifications.team.servable-count"
			rig.register(t, recipientSender, []string{topic})
			rig.register(t, "ses_counted_a", []string{topic})
			rig.register(t, "ses_counted_b", []string{topic})
			assertCounts(t, rig.publish(t, topic, "servable"), 2, 2)

			copies[stale].Disconnected()
			clock.Advance(staleBound)
			assertCounts(t, rig.publish(t, topic, "inside-the-bound"), 2, 2)

			clock.Advance(time.Second)
			if !rig.known.Opened() {
				t.Fatal("the known-session store stopped answering, so an absent count below would be its own")
			}
			for name, cache := range copies {
				if err := cache.Servable(); (err == nil) == (name == stale) {
					t.Fatalf("past the %s copy's stale bound the %s copy answers Servable() = %v; want the %s copy alone not servable", stale, name, err, stale)
				}
			}
			if stale == "session" {
				assertRecipientsAlone(t, rig.publish(t, topic, "past-the-bound"), 2)
			} else {
				assertNoCounts(t, rig.publish(t, topic, "past-the-bound"))
			}

			if err := copies[stale].Rewatch(context.Background(), rig.harness.client.Conn); err != nil {
				t.Fatalf("read the %s bucket again: %v", stale, err)
			}
			awaitCopiesServable(t, copies, "the "+stale+" copy to be served once it read its bucket again")
			assertCounts(t, rig.publish(t, topic, "read-again"), 2, 2)
		})
	}
}

// The counts cost no JetStream API request: 100 publishes over warm caches send none, and the
// probe proves it is watching by seeing the one call the test makes itself.
func TestAHundredPublishesSendNoJetStreamAPIRequest(t *testing.T) {
	rig := newRecipientRig(t)
	const topic = "notifications.team.recipient-count-api-free"
	rig.register(t, recipientSender, []string{topic})
	// One session the known-session records alone name, stopped so that it is counted and not
	// running, and one the interest rows alone name, as an older listener left it: both matchers
	// and the session registry are read on every publish below.
	rig.register(t, "ses_stopped", []string{topic})
	rig.stop(t, "ses_stopped")
	rig.reapInterest(t, "ses_stopped")
	rig.storeSessionEntryOf(t, "ses_row_only", topic, time.Now(), session.Live)
	// One publish before the probe starts, so anything a first publish would set up is already
	// set up and the window below holds the steady state.
	assertCounts(t, rig.publish(t, topic, "api-free-warm"), 2, 1)

	probe := watchJetStreamAPI(t, rig.url, rig.harness.client.Conn, "$JS.API.CONSUMER.>", "$JS.API.STREAM.INFO.>")
	for i := range 100 {
		assertCounts(t, rig.publish(t, topic, "api-free-"+strconv.Itoa(i)), 2, 1)
	}

	control := "$JS.API.STREAM.INFO." + bus.Stream
	if _, err := rig.harness.client.JS().StreamInfo(bus.Stream); err != nil {
		t.Fatalf("the probe's control call: %v", err)
	}
	if calls := probe.awaitCall(t, control); len(calls) > 0 {
		t.Fatalf("100 publishes sent %d JetStream API requests: %v", len(calls), calls)
	}
}

// jetStreamAPIProbe watches the JetStream API requests one connection sends. A second connection
// to the same server subscribes to the API subjects given, and each request is attributed to its
// sender by the response inbox it carries, which is one connection's alone
// (jetStreamInboxPrefix): a NATS message says nothing else about who sent it. A route that must
// answer from memory asserts the probe took none of its calls while it ran.
type jetStreamAPIProbe struct {
	mu       sync.Mutex
	observed []string
	prefix   string
}

// watchJetStreamAPI starts a probe of the JetStream API calls conn sends on subjects, over a
// connection of its own to uri, and returns it ready: its subscriptions are flushed, so every call
// conn sends after this returns is the probe's to see.
func watchJetStreamAPI(t *testing.T, uri string, conn *natsgo.Conn, subjects ...string) *jetStreamAPIProbe {
	t.Helper()
	probe := &jetStreamAPIProbe{prefix: jetStreamInboxPrefix(t, conn)}
	watcher := testnats.Connect(t, uri)
	t.Cleanup(watcher.Close)
	for _, subject := range subjects {
		subscription, err := watcher.Subscribe(subject, func(msg *natsgo.Msg) {
			if !strings.HasPrefix(msg.Reply, probe.prefix) {
				return
			}
			probe.mu.Lock()
			probe.observed = append(probe.observed, msg.Subject)
			probe.mu.Unlock()
		})
		if err != nil {
			t.Fatalf("watch %s: %v", subject, err)
		}
		t.Cleanup(func() { _ = subscription.Unsubscribe() })
	}
	if err := watcher.Flush(); err != nil {
		t.Fatalf("flush the JetStream API probe: %v", err)
	}
	return probe
}

// awaitCall waits for a call on subject, the control call its caller makes to show the probe is
// watching, and returns every call the probe took before it. The server hands one connection's
// messages on in the order it took them, so once the control call has arrived every call that
// connection sent before it has arrived too.
func (p *jetStreamAPIProbe) awaitCall(t *testing.T, subject string) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		index := slices.Index(p.observed, subject)
		var before []string
		if index >= 0 {
			before = slices.Clone(p.observed[:index])
		}
		p.mu.Unlock()
		if index >= 0 {
			return before
		}
		if time.Now().After(deadline) {
			t.Fatalf("the probe did not see the control call on %s within 5s, so it is watching nothing", subject)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// jetStreamInboxPrefix is the prefix every response inbox conn puts on a request carries, which no
// other connection uses: nats.go derives one per connection (nats.Conn.NewRespInbox) and answers
// each request on it.
func jetStreamInboxPrefix(t *testing.T, conn *natsgo.Conn) string {
	t.Helper()
	inbox := conn.NewRespInbox()
	cut := strings.LastIndex(inbox, ".")
	if cut < 0 {
		t.Fatalf("the connection's response inbox %q has no prefix to watch by", inbox)
	}
	return inbox[:cut+1]
}
