package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// knownRig is a listener's routes over one NATS server with the known-session store on a clock the
// test moves (knownSessionRig), and the bucket handle a test reads revisions from.
type knownRig struct {
	*knownSessionRig
	kv nats.KeyValue
}

func setupKnownRig(t *testing.T) *knownRig {
	t.Helper()
	rig := &knownRig{knownSessionRig: newKnownSessionRig(t)}
	kv, err := rig.harness.client.JS().KeyValue(bus.KnownSessionBucket)
	if err != nil {
		t.Fatalf("open the known-session bucket: %v", err)
	}
	rig.kv = kv
	return rig
}

// subscribe runs one registration of sessionID on topics through the real route, as a plugin in
// consumer mode sends it on every heartbeat.
func (r *knownRig) subscribe(t *testing.T, sessionID string, topics []string) {
	t.Helper()
	r.register(t, sessionID, map[string]any{
		"topics":             topics,
		"title":              "a known session",
		"capabilities":       []string{"aside"},
		"client_instance_id": "inst_one",
	})
}

// revision is the bucket revision of sessionID's record, which counts the writes it has taken, or
// 0 when the bucket holds none.
func (r *knownRig) revision(t *testing.T, sessionID string) uint64 {
	t.Helper()
	entry, err := r.kv.Get(sessionID)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return 0
	}
	if err != nil {
		t.Fatalf("read %s from the known-session bucket: %v", sessionID, err)
	}
	return entry.Revision()
}

// knownMatchedIDs is the session ids the known-session store's MatchEach visits for topic,
// sorted, which is what a publish's recipient count reads it for (recipients.go).
func knownMatchedIDs(known *session.KnownSessions, topic string) []string {
	ids := []string{}
	known.MatchEach(topic, func(record session.KnownSession) { ids = append(ids, record.SessionID) })
	slices.Sort(ids)
	return ids
}

// A registration writes the session's record with the topics the registration stored, which are
// the filter of the consumer it gave the session: every topic a registration stores is one a
// stream subject captures, a team topic among them, so the record counts exactly the sessions a
// stored publish reaches.
func TestARegistrationWritesTheKnownSessionRecordWithTheRegisteredTopics(t *testing.T) {
	t.Parallel()
	rig := setupKnownRig(t)
	team := "notifications.team.deploy-squad"
	rig.subscribe(t, "ses_registered", []string{team, "notifications.github.sjawhar.legion.pr.1"})

	record, ok := rig.known.Get("ses_registered")
	if !ok {
		t.Fatal("the registration wrote no known-session record")
	}
	stored, cached := rig.harness.registry.CachedInterest("ses_registered")
	if !cached {
		t.Fatal("the registration stored no interest row")
	}
	if !slices.Equal(record.Topics, stored.Topics) {
		t.Errorf("the record's topics are %v, want the registration's stored topics %v", record.Topics, stored.Topics)
	}
	if !slices.Contains(record.Topics, team) {
		t.Errorf("the record's topics are %v, want the team topic %q the registration stored", record.Topics, team)
	}
	if !slices.Contains(record.Topics, contracts.LegacyAgentSubject("ses_registered")) {
		t.Errorf("the record's topics are %v, want the session's own agent subject", record.Topics)
	}
	if filter := rig.filters(t, "ses_registered"); !slices.Contains(filter, team) {
		t.Errorf("the consumer filter %v does not hold the team topic the record counts the session for", filter)
	}
	if record.Title != "a known session" || !slices.Equal(record.Capabilities, []string{"aside"}) {
		t.Errorf("record = %#v, want the registration's title and capabilities", record)
	}
	if record.LastSeen != rig.clock.Now().UnixMilli() {
		t.Errorf("record.LastSeen = %d, want the write's clock %d", record.LastSeen, rig.clock.Now().UnixMilli())
	}
	if got := knownMatchedIDs(rig.known, team); !slices.Equal(got, []string{"ses_registered"}) {
		t.Errorf("MatchEach of the team topic = %v, want the session just registered", got)
	}
}

// A clean stop keeps the record and its topics, and moves only its last_seen: a session that
// stopped is still a recipient of the topics it followed, since Envoy keeps its messages for 72
// hours and it can resume under the same id. The stop here lands inside
// KnownSessionRefreshAge of the registration, which is where a real one lands - a live session
// rewrites its record at least that often - so the write a stop makes is the one the throttle
// would skip, and a `last_seen` minutes before the stop is what a send to the stopped session
// would then answer.
func TestACleanStopKeepsTheKnownSessionRecordAndMovesLastSeen(t *testing.T) {
	t.Parallel()
	rig := setupKnownRig(t)
	team := "notifications.team.deploy-squad"
	rig.subscribe(t, "ses_stopping", []string{team})
	registered, ok := rig.known.Get("ses_stopping")
	if !ok {
		t.Fatal("the registration wrote no known-session record")
	}

	rig.clock.Advance(time.Minute)
	rig.stop(t, "ses_stopping")

	stopped, ok := rig.known.Get("ses_stopping")
	if !ok {
		t.Fatal("the clean stop removed the known-session record")
	}
	if !slices.Equal(stopped.Topics, registered.Topics) {
		t.Errorf("the stopped record's topics are %v, want the registered %v", stopped.Topics, registered.Topics)
	}
	if stopped.LastSeen != rig.clock.Now().UnixMilli() {
		t.Errorf("the stopped record's last_seen = %d, want the stop's clock %d", stopped.LastSeen, rig.clock.Now().UnixMilli())
	}
	if stopped.LastSeen <= registered.LastSeen {
		t.Errorf("the stop left last_seen at %d, want it past the registration's %d", stopped.LastSeen, registered.LastSeen)
	}
	if got := knownMatchedIDs(rig.known, team); !slices.Equal(got, []string{"ses_stopping"}) {
		t.Errorf("MatchEach of the team topic after the stop = %v, want the stopped session", got)
	}
}

// 100 heartbeats of a session whose registration never changes, inside the 10-minute refresh
// age, write the record at most twice. The count is the bucket's own revision, which counts the
// writes the key took, and the heartbeats run through the registration route itself on a clock
// this test moves.
func TestAHundredUnchangedHeartbeatsWriteTheRecordAtMostTwice(t *testing.T) {
	t.Parallel()
	rig := setupKnownRig(t)
	topics := []string{"notifications.team.deploy-squad"}
	const heartbeats = 100
	// 100 heartbeats five seconds apart span 8m15s, inside the refresh age, as a session's own
	// 90-second heartbeat would not: the point is the number of writes, not the interval.
	step := 5 * time.Second
	if total := step * (heartbeats - 1); total >= session.KnownSessionRefreshAge {
		t.Fatalf("the heartbeats span %s, which is not inside the %s refresh age", total, session.KnownSessionRefreshAge)
	}
	for i := range heartbeats {
		if i > 0 {
			rig.clock.Advance(step)
		}
		rig.subscribe(t, "ses_heartbeat", topics)
	}
	if revision := rig.revision(t, "ses_heartbeat"); revision > 2 {
		t.Errorf("%d unchanged heartbeats wrote the record %d times, want at most 2", heartbeats, revision)
	} else {
		t.Logf("%d unchanged heartbeats inside %s wrote the record %d time(s)", heartbeats, session.KnownSessionRefreshAge, revision)
	}

	// A change to the registration is written at once, whatever the throttle's window.
	rig.subscribe(t, "ses_heartbeat", append(slices.Clone(topics), "notifications.team.another-squad"))
	record, ok := rig.known.Get("ses_heartbeat")
	if !ok {
		t.Fatal("the record is gone after a changed registration")
	}
	if !slices.Contains(record.Topics, "notifications.team.another-squad") {
		t.Errorf("the record's topics are %v, want the topic the changed registration added", record.Topics)
	}
}

// A subscription change writes the record too: the unsubscribe route writes it from the topics
// the interest bucket still holds for the session, beside the refollow that sets its consumer's
// filter to what is left. Neither waits on the other, so the record is right whatever the consumer
// call did.
func TestARefollowWritesTheKnownSessionRecord(t *testing.T) {
	t.Parallel()
	rig := setupKnownRig(t)
	kept, dropped := "notifications.team.deploy-squad", "notifications.team.other-squad"
	rig.subscribe(t, "ses_refollow", []string{kept, dropped})
	registered, ok := rig.known.Get("ses_refollow")
	if !ok {
		t.Fatal("the registration wrote no known-session record")
	}
	if !slices.Contains(registered.Topics, dropped) {
		t.Fatalf("the registered record's topics are %v, want the topic the unsubscribe will remove", registered.Topics)
	}
	if entry := rig.entry(t, "ses_refollow"); entry.Delivery != session.DeliveryConsumer {
		t.Fatalf("ses_refollow's entry is %#v, want one that records consumer delivery", entry)
	}

	rig.clock.Advance(session.KnownSessionRefreshAge + time.Minute)
	body, err := json.Marshal(map[string]any{"session_id": "ses_refollow", "topics": []string{dropped}})
	if err != nil {
		t.Fatal(err)
	}
	rig.post(t, "/v1/interests/unsubscribe", string(body))

	after, ok := rig.known.Get("ses_refollow")
	if !ok {
		t.Fatal("the unsubscribe removed the known-session record")
	}
	if slices.Contains(after.Topics, dropped) {
		t.Errorf("the record's topics are %v, want the unsubscribed %q gone", after.Topics, dropped)
	}
	if !slices.Contains(after.Topics, kept) {
		t.Errorf("the record's topics are %v, want the topic the session still follows", after.Topics)
	}
	if after.LastSeen != rig.clock.Now().UnixMilli() {
		t.Errorf("the record's last_seen = %d, want the unsubscribe's clock %d", after.LastSeen, rig.clock.Now().UnixMilli())
	}
	if filter := rig.filters(t, "ses_refollow"); slices.Contains(filter, dropped) {
		t.Errorf("the consumer filter %v still holds the unsubscribed %q", filter, dropped)
	}
}

// A session whose interests an operator removes through the admin route stops being counted too:
// that route is the unsubscribe's other call site, and a record left naming the topics the
// session no longer follows would keep telling a publish that it gets the message.
func TestRemovingASessionsInterestsThroughTheAdminRouteEmptiesItsRecord(t *testing.T) {
	t.Parallel()
	rig := setupKnownRig(t)
	team := "notifications.team.deploy-squad"
	rig.subscribe(t, "ses_admin_removed", []string{team})
	registered, ok := rig.known.Get("ses_admin_removed")
	if !ok {
		t.Fatal("the registration wrote no known-session record")
	}
	if !slices.Contains(registered.Topics, team) {
		t.Fatalf("the registered record's topics are %v, want the team topic the removal will take", registered.Topics)
	}

	rig.clock.Advance(session.KnownSessionRefreshAge + time.Minute)
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/v1/interests/ses_admin_removed", nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("DELETE /v1/interests/ses_admin_removed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	after, ok := rig.known.Get("ses_admin_removed")
	if !ok {
		t.Fatal("the removal took the known-session record; it must stay, naming no topic")
	}
	if len(after.Topics) != 0 {
		t.Errorf("the record's topics are %v, want none: the route removed every interest the session had", after.Topics)
	}
	if after.LastSeen != rig.clock.Now().UnixMilli() {
		t.Errorf("the record's last_seen = %d, want the removal's clock %d", after.LastSeen, rig.clock.Now().UnixMilli())
	}
	if got := knownMatchedIDs(rig.known, team); len(got) != 0 {
		t.Errorf("the team topic still matches %v, so a publish on it would count a session that no longer follows it", got)
	}
}

// A listener with no known-session store - one whose connection gave no JetStream context, which
// runs on a nil store - answers a registration and a clean stop 200: the routes that write a
// record carry no check of their own for the store.
func TestTheRoutesServeAListenerWithNoKnownSessionStore(t *testing.T) {
	t.Parallel()
	rig := newSessionConsumerRig(t)
	if rig.deps.known != nil {
		t.Fatal("the deps hold a known-session store, so this test is not the listener without one")
	}
	rig.subscribe(t, consumerRegistration("ses_degraded", "e", "notifications.team.deploy-squad"))
	stopSession(t, rig.mux, "ses_degraded", "")
}

// A listener whose NATS user may not open the bucket yet carries neither recipient count, as one
// with no store at all does (TestAListenerWithNoKnownSessionStoreCarriesNeitherCount): the count
// it could make is the live sessions alone, and the records that make it whole are the ones its
// grant does not let it read. This is a listener whose hub's grant does not carry the bucket, and
// the store it holds there is open for business in every way but the bucket.
func TestAListenerWhoseKnownSessionBucketIsNotOpenCarriesNeitherCount(t *testing.T) {
	// Not parallel: it sets an environment variable of the whole process (t.Setenv of
	// NATS_NKEY_SEED).
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, listenerGrantBeforeSessionConsumers()...)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.ConnectOwningStream([]string{grant.URL}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener's user: %v", err)
	}
	t.Cleanup(client.Close)
	known, err := session.OpenKnownSessions(t.Context(), client.Conn, session.WithKnownSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the known-session store over a bucket the grant does not carry: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if known.Opened() {
		t.Fatal("the bucket opened under a grant that does not carry it, so this test says nothing about a listener that cannot open it")
	}

	harness := newListenerDeliveryHarnessOn(t, client)
	deps := harness.publishDeps(t)
	deps.known = known
	recipients, connected := recipientCounts(t.Context(), deps, "notifications.team.deploy-squad", "ses_sender")
	if recipients != nil || connected != nil {
		t.Errorf("the counts are %v and %v, want both left out while the bucket is not open", recipients, connected)
	}
	if _, recorded := known.Get("ses_sender"); recorded {
		t.Error("a store whose bucket is not open answered a record")
	}
	if err := known.RecordWithin(context.Background(), session.KnownSession{SessionID: "ses_sender"}); err != nil {
		t.Errorf("a write to a store whose bucket is not open answered %v, want nil: it writes nothing and fails nothing", err)
	}
}
