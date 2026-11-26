package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// A listener whose NATS user holds exactly the grant envoy-infra renders for its machine
// (testnats.ListenerGrant) takes its delivery lease, renews it, delivers a notification through
// the delivery ledger, records a registered session's last-seen time, and releases the lease on
// SIGTERM, and the server refuses it nothing. Each write to the listener state bucket is read
// back by key, a direct get on the key's own subject, which the grant allows: a read by revision
// is a direct get on the stream's bare subject, which it refuses, so the lease was never held and
// no last-seen record was written.
func TestAListenerUnderItsMachinesGrantHoldsTheLeaseAndRecordsLastSeen(t *testing.T) {
	t.Parallel()
	const (
		machineID = "box-granted"
		instance  = "pod-granted"
		sessionID = "ses_granted"
		topic     = "notifications.github.acme.granted.pr.1"
	)
	seed, public := testnats.User(t)
	granted := testnats.StartGranted(t, testnats.GrantedUser{Public: public, Grant: testnats.ListenerGrant(t, machineID)})
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), granted.URL(), machineID,
		"NATS_NKEY_SEED="+seed, "ENVOY_INSTANCE_ID="+instance)
	listener.waitHealthy(t)

	admin, err := bus.Connect([]string{granted.AdminURL}, bus.WithName("grant-test-admin"))
	if err != nil {
		t.Fatalf("connect as the admin: %v", err)
	}
	t.Cleanup(admin.Close)
	state, err := bus.OpenKeyValue(t.Context(), admin.Conn, admin.JS(), bus.ListenerStateBucket)
	if err != nil {
		t.Fatalf("open the listener state bucket: %v", err)
	}
	leaseKey := "lease." + bus.ListenerDurable(machineID)
	readLeaseKey := func() (leaseRecord, natsgo.KeyValueEntry) {
		t.Helper()
		entry, err := state.Get(leaseKey)
		if err != nil {
			t.Fatalf("read %s: %v", leaseKey, err)
		}
		record, err := decodeLeaseRecord(entry.Value())
		if err != nil {
			t.Fatalf("decode %s %s: %v", leaseKey, entry.Value(), err)
		}
		return record, entry
	}
	held, taken := readLeaseKey()
	if held.Holder != instance || held.Released {
		t.Fatalf("%s = %+v, want held by %s", leaseKey, held, instance)
	}
	waitFor(t, "the holder to renew the lease", func() bool {
		record, entry := readLeaseKey()
		return record.Holder == instance && entry.Revision() > taken.Revision()
	})

	standIn := startSessionStandIn(t, granted.AdminURL, sessionID, answerDone)
	registerThroughListener(t, listener, listenerwire.SubscribeRequest{
		SessionID: sessionID, Topics: []string{topic}, Title: "granted", Dir: "/test/" + sessionID,
		Capabilities: []string{"btw"}, SelfSubscribed: true, Host: machineID,
		Delivery: &listenerwire.SubscribeDelivery{Consumer: true, Epoch: "grant-" + id.New()},
	})
	publisher := connectPublisher(t, granted.AdminURL)
	item := newEnvelope("github", topic, "pull request 1 was opened")
	publish(t, publisher, item)
	if frame := standIn.collect(t, 1, 15*time.Second)[0]; frame.item.EventID != item.EventID {
		t.Fatalf("the frame relayed to %s carries event %s, want %s", sessionID, frame.item.EventID, item.EventID)
	}
	// The relay of a session's own consumer costs no ledger write: this listener hands it
	// nothing. The one delivery it makes itself is the role-lane forward, which records the pair
	// delivered once the stream stored the forward, so that is the `delivery.` key this grant must
	// allow.
	const role = "granted-keeper"
	if status, answer := callListener(t, listener.port, http.MethodPost, "/v1/roles/set",
		fmt.Sprintf(`{"session_id":%q,"role":%q}`, sessionID, role)); status != http.StatusOK {
		t.Fatalf("claim %s for %s: status %d %s", role, sessionID, status, answer)
	}
	publish(t, publisher, newEnvelope("dispatch", contracts.RoleTopicPrefix+role, "a role message for its holder"))
	ledger := "delivery." + bus.ListenerDurable(machineID) + "."
	waitFor(t, "the role forward to record its delivery in the ledger", func() bool {
		keys, err := state.Legacy().Keys()
		return err == nil && hasKeyWithPrefix(keys, ledger)
	})
	if _, err := state.Get("seen." + sessionID); err != nil {
		t.Fatalf("read %s's last-seen record: %v\n%s", sessionID, err, listener.Output.String())
	}

	listener.Terminate(t)
	listener.WaitExit(t, "SIGTERM")
	if released, _ := readLeaseKey(); !released.Released || released.Holder != instance {
		t.Fatalf("%s after SIGTERM = %+v, want released by %s", leaseKey, released, instance)
	}
	if log := granted.Log(); strings.Contains(log, "Violation") {
		t.Fatalf("the server refused the listener what its grant should allow:\n%s", log)
	}
}

// registerThroughListener registers body against a started listener process, as a session's
// plugin does: one POST of /v1/interests/subscribe, which must be answered 200.
func registerThroughListener(t *testing.T, listener *listenerProcess, body listenerwire.SubscribeRequest) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal %s's registration: %v", body.SessionID, err)
	}
	status, answer := callListener(t, listener.port, http.MethodPost, "/v1/interests/subscribe", string(encoded))
	if status != http.StatusOK {
		t.Fatalf("register %s: status %d %s", body.SessionID, status, answer)
	}
}

func hasKeyWithPrefix(keys []string, prefix string) bool {
	for _, key := range keys {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// One machine's grant writes its own delivery lease as the lease does (Create, a read back by key,
// Update) and its own clean-hand-back mark, and reads that mark back as the pod taking the record
// does; it is refused a write to another machine's lease key, hand-back mark or delivery ledger,
// so no machine's user writes another machine's keys by a key write (what every listener user can
// still do to the whole bucket is natsgrant.ListenerState's to say). A refused Create draws two
// refusals, since nats.go reads the key after any failed create (nats.go@v1.55.0 kv.go:729-737):
// its write, and the direct get of the same key, which the grant refuses too
// (TestAMachinesGrantRefusesAReadOfAnotherMachinesLeaseMarkAndLedgerAndReadsEveryLastSeen). Each
// row waits for every refusal its write draws, and fails on any other answer.
func TestAMachinesGrantRefusesAnotherMachinesLeaseMarkAndLedger(t *testing.T) {
	t.Parallel()
	seed, public := testnats.User(t)
	granted := testnats.StartGranted(t, testnats.GrantedUser{Public: public, Grant: testnats.ListenerGrant(t, "box-a")})
	errs := make(chan error, 8)
	conn, err := testnats.ConnectSeed(t, granted.URL(), seed,
		natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) { errs <- err }))
	if err != nil {
		t.Fatalf("connect as box-a's user: %v", err)
	}
	t.Cleanup(conn.Close)
	kv, err := openListenerState(t.Context(), conn, 1, 2*time.Second)
	if err != nil {
		t.Fatalf("open the listener state bucket under box-a's grant: %v", err)
	}

	own := "lease." + bus.ListenerDurable("box-a")
	taken, err := kv.Create(own, []byte(`{"holder":"pod-a","boot":"0123456789abcdef"}`))
	if err != nil {
		t.Fatalf("take box-a's lease: %v", err)
	}
	if entry, err := kv.Get(own); err != nil || entry.Revision() != taken {
		t.Fatalf("read back box-a's lease = %v, %v; want revision %d", entry, err, taken)
	}
	if _, err := kv.Update(own, []byte(`{"holder":"pod-a","boot":"0123456789abcdef","released":true}`), taken); err != nil {
		t.Fatalf("release box-a's lease: %v", err)
	}
	mark := bus.ListenerHandBackPrefix(bus.ListenerDurable("box-a")) + strconv.FormatUint(taken, 10)
	if _, err := kv.Put(mark, []byte(`{"instance":"pod-a","boot":"0123456789abcdef","revision":1}`)); err != nil {
		t.Fatalf("mark box-a's hand-back: %v", err)
	}
	if entry, err := kv.Get(mark); err != nil || len(entry.Value()) == 0 {
		t.Fatalf("read back box-a's hand-back mark = %v, %v; want the mark the stop wrote", entry, err)
	}
	select {
	case err := <-errs:
		t.Fatalf("the server refused box-a's own lease or mark: %v", err)
	default:
	}

	for _, refused := range []struct {
		key   string
		write func(key string) error
		// read is whether the write reads the key after it fails, as nats.go's Create does.
		read bool
	}{
		{"lease." + bus.ListenerDurable("box-b"), func(key string) error {
			_, err := kv.Create(key, []byte(`{"holder":"pod-a","boot":"0123456789abcdef"}`))
			return err
		}, true},
		{bus.ListenerHandBackPrefix(bus.ListenerDurable("box-b")) + "7", func(key string) error {
			_, err := kv.Put(key, []byte(`{"instance":"pod-a","boot":"0123456789abcdef","revision":7}`))
			return err
		}, false},
		{"delivery." + bus.ListenerDurable("box-b") + ".0123456789abcdef0123456789abcdef.ses_x", func(key string) error {
			_, err := kv.Put(key, []byte(`{"state":"delivered"}`))
			return err
		}, false},
	} {
		if err := refused.write(refused.key); err == nil {
			t.Fatalf("box-a's user wrote %s", refused.key)
		}
		subjects := []string{"$KV." + bus.ListenerStateBucket + "." + refused.key}
		if refused.read {
			subjects = append(subjects, listenerStateDirectGet(refused.key))
		}
		awaitRefusals(t, errs, "box-a's write of "+refused.key, subjects...)
	}
}

// listenerStateDirectGet is the subject a read of key in the listener state bucket sends: nats.go's
// direct get of the key's last message, on the key's own subject (nats.go@v1.55.0 kv.go:647-660).
func listenerStateDirectGet(key string) string {
	return "$JS.API.DIRECT.GET.KV_" + bus.ListenerStateBucket + ".$KV." + bus.ListenerStateBucket + "." + key
}

// awaitRefusals reads the connection's errors off errs until the server has refused a publish to
// each of subjects, in any order, within 5 s, and fails on any other error it reads first: what
// names the call the refusals answer.
func awaitRefusals(t *testing.T, errs <-chan error, what string, subjects ...string) {
	t.Helper()
	left := slices.Clone(subjects)
	deadline := time.After(5 * time.Second)
	for len(left) > 0 {
		select {
		case err := <-errs:
			at := slices.IndexFunc(left, func(subject string) bool {
				return errors.Is(err, natsgo.ErrPermissionViolation) && strings.Contains(err.Error(), `Publish to "`+subject+`"`)
			})
			if at < 0 {
				t.Fatalf("the server's answer to %s: %v, want its refusal of %s", what, err, strings.Join(left, ", "))
			}
			left = slices.Delete(left, at, at+1)
		case <-deadline:
			t.Fatalf("the server did not refuse %s within 5s: %s", what, strings.Join(left, ", "))
		}
	}
}

// A listener under its machine's grant cannot request a read of another machine's delivery lease,
// clean-hand-back mark or delivery ledger: the direct get of each of those keys is refused, so a
// machine's user cannot ask the bucket for another machine's lease or deliveries (its _INBOX.>
// subscription still receives every reply: natsgrant.ListenerState says what that leaves). It
// still reads every session's last-seen record, whichever machine's listener wrote it: a role
// claim names its holder session and no machine, and any listener answers a lapsed holder's
// last_seen out of that record (GET /v1/roles/<role>).
func TestAMachinesGrantRefusesAReadOfAnotherMachinesLeaseMarkAndLedgerAndReadsEveryLastSeen(t *testing.T) {
	t.Parallel()
	seedA, publicA := testnats.User(t)
	seedB, publicB := testnats.User(t)
	granted := testnats.StartGranted(t,
		testnats.GrantedUser{Public: publicA, Grant: testnats.ListenerGrant(t, "box-a")},
		testnats.GrantedUser{Public: publicB, Grant: testnats.ListenerGrant(t, "box-b")})

	// box-b writes its own keys, and the last-seen record of a session registered through it,
	// under its own grant.
	connB, err := testnats.ConnectSeed(t, granted.URL(), seedB)
	if err != nil {
		t.Fatalf("connect as box-b's user: %v", err)
	}
	t.Cleanup(connB.Close)
	kvB, err := openListenerState(t.Context(), connB, 1, 2*time.Second)
	if err != nil {
		t.Fatalf("open the listener state bucket under box-b's grant: %v", err)
	}
	durableB := bus.ListenerDurable("box-b")
	theirs := []string{
		bus.ListenerLeaseKey("box-b"),
		bus.ListenerHandBackPrefix(durableB) + "7",
		bus.ListenerLedgerPrefix(durableB) + "0123456789abcdef0123456789abcdef.ses_x",
	}
	values := []string{
		`{"holder":"pod-b","boot":"0123456789abcdef"}`,
		`{"instance":"pod-b","boot":"0123456789abcdef","revision":7}`,
		`{"state":"delivered"}`,
	}
	for index, key := range theirs {
		if _, err := kvB.Put(key, []byte(values[index])); err != nil {
			t.Fatalf("box-b writes its own %s: %v", key, err)
		}
	}
	seen := bus.LastSeenPrefix + dedupe.SessionToken("ses_on_box_b")
	const seenValue = `{"at":1763500000000}`
	if _, err := kvB.Put(seen, []byte(seenValue)); err != nil {
		t.Fatalf("box-b records %s: %v", seen, err)
	}

	errs := make(chan error, 8)
	connA, err := testnats.ConnectSeed(t, granted.URL(), seedA,
		natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) { errs <- err }))
	if err != nil {
		t.Fatalf("connect as box-a's user: %v", err)
	}
	t.Cleanup(connA.Close)
	kvA, err := openListenerState(t.Context(), connA, 1, 2*time.Second)
	if err != nil {
		t.Fatalf("open the listener state bucket under box-a's grant: %v", err)
	}
	for _, key := range theirs {
		if entry, err := kvA.Get(key); err == nil {
			t.Fatalf("box-a's user read box-b's %s: %s", key, entry.Value())
		}
		awaitRefusals(t, errs, "box-a's read of "+key, listenerStateDirectGet(key))
	}
	entry, err := kvA.Get(seen)
	if err != nil || string(entry.Value()) != seenValue {
		t.Fatalf("box-a reads %s, which box-b wrote, as %v (%v), want %s", seen, entry, err, seenValue)
	}
	select {
	case err := <-errs:
		t.Fatalf("the server refused box-a's read of %s: %v", seen, err)
	default:
	}
}

// Every call a listener makes on its state bucket passes under its machine's grant
// (testnats.ListenerGrant, whose bucket entries are natsgrant.ListenerState's eleven subjects),
// and the server logs no permission violation, each made by the listener's own code:
//   - the open of an absent bucket (openListenerState): its lookup, its create, the wait for its
//     leader and the handle's open;
//   - the age raise of a bucket an earlier build created with a shorter MaxAge (bus.RaiseMaxAge,
//     from openListenerState), the stream info and update of KV_envoy_listener that the keeper's
//     copy raise sends too (raiseReplicas and raiseStream reach the same updateStream), which a
//     single server of one copy never needs;
//   - the lease: its create of an absent record and the read back, its renewal and its release,
//     the clean-hand-back mark the stop writes and the pod that took the record reads, that pod's
//     poll and take-over, and the released pod's read of its successor;
//   - the delivery ledger (dedupe.KV): an attempt's create, a second attempt on the same pair,
//     which nats.go's Create answers by reading the key after its failed write before the ledger
//     reads it itself, a take-over's update, the record and the clear;
//   - a session's last-seen record (session.SessionRegistry): written at its registration and
//     read once the session lapsed.
func TestEveryCallTheListenerMakesOnItsStateBucketPassesUnderItsMachinesGrant(t *testing.T) {
	t.Parallel()
	seed, public := testnats.User(t)
	granted := testnats.StartGranted(t, testnats.GrantedUser{Public: public, Grant: testnats.ListenerGrant(t, leaseTestMachine)})
	errs := make(chan error, 16)
	conn, err := testnats.ConnectSeed(t, granted.URL(), seed,
		natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) { errs <- err }))
	if err != nil {
		t.Fatalf("connect as %s's user: %v", leaseTestMachine, err)
	}
	t.Cleanup(conn.Close)
	if _, err := openListenerState(t.Context(), conn, 1, listenerStateRequestWait); err != nil {
		t.Fatalf("open the absent listener state bucket under %s's grant: %v", leaseTestMachine, err)
	}

	admin := testnats.Connect(t, granted.AdminURL)
	t.Cleanup(admin.Close)
	adminJS, err := admin.JetStream()
	if err != nil {
		t.Fatalf("jetstream as the admin: %v", err)
	}
	stream := "KV_" + bus.ListenerStateBucket
	info, err := adminJS.StreamInfo(stream)
	if err != nil {
		t.Fatalf("read %s: %v", stream, err)
	}
	earlier := info.Config
	earlier.MaxAge = 10 * time.Minute
	if _, err := adminJS.UpdateStream(&earlier); err != nil {
		t.Fatalf("shorten %s's age as an earlier build had it: %v", stream, err)
	}
	kv, err := openListenerState(t.Context(), conn, 1, listenerStateRequestWait)
	if err != nil {
		t.Fatalf("open the listener state bucket of an earlier build's age under %s's grant: %v", leaseTestMachine, err)
	}
	if info, err := adminJS.StreamInfo(stream); err != nil || info.Config.MaxAge != listenerStateMaxAge {
		t.Fatalf("%s after the open: %v (%v), want its age raised to %s", stream, info, err, listenerStateMaxAge)
	}

	clock := newFakeLeaseClock()
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	a.step()
	term := a.Status()
	if !term.Holding || term.How != leaseTakenAbsent {
		t.Fatalf("pod-a status = %+v, want the lease taken from an absent record", term)
	}
	_, taken := readLease(t, kv)
	a.step()
	if _, renewed := readLease(t, kv); renewed.Revision() <= taken.Revision() {
		t.Fatalf("the lease record after pod-a's renewal is at revision %d, want past %d", renewed.Revision(), taken.Revision())
	}
	// A graceful stop that handed messages back: unsettled, released, and its hand-back marked.
	a.Unsettle(term.Term)
	if err := a.Release(t.Context()); err != nil {
		t.Fatalf("release pod-a's lease: %v", err)
	}
	if marked, err := a.MarkHandBack(t.Context()); !marked || err != nil {
		t.Fatalf("pod-a's stop marked its hand-back %v (%v), want marked", marked, err)
	}
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	b.step()
	next := b.Status()
	if !next.Holding || next.How != leaseTakenReleased || !next.HandBack {
		t.Fatalf("pod-b status = %+v, want held from pod-a's released record as a hand-back candidate", next)
	}
	if marked, err := b.HandBackMarked(t.Context(), next.Term); !marked || err != nil {
		t.Fatalf("pod-b reads its term marked %v (%v), want the mark pod-a's stop wrote", marked, err)
	}
	if successor, _, err := a.Successor(); !successor || err != nil {
		t.Fatalf("pod-a reads its successor %v (%v), want pod-b's record", successor, err)
	}

	durable := bus.ListenerDurable(leaseTestMachine)
	ledger := dedupe.NewKV(kv, dedupe.KVConfig{Durable: durable, Boot: "0123456789abcdef", TakeoverAfter: time.Hour})
	first, err := ledger.Attempt("granted-1", "ses_granted")
	if err != nil || first.Verdict != dedupe.Proceed {
		t.Fatalf("the first attempt = %+v (%v), want to proceed", first, err)
	}
	if err := ledger.Record(first); err != nil {
		t.Fatalf("record the delivery: %v", err)
	}
	if again, err := ledger.Attempt("granted-1", "ses_granted"); err != nil || again.Verdict != dedupe.Delivered {
		t.Fatalf("a second attempt of the delivered pair = %+v (%v), want delivered", again, err)
	}
	if _, err := ledger.Attempt("granted-2", "ses_granted"); err != nil {
		t.Fatalf("attempt a second pair: %v", err)
	}
	// Another boot's ledger takes over the unsettled attempt at once.
	takeover := dedupe.NewKV(kv, dedupe.KVConfig{Durable: durable, Boot: "fedcba9876543210", TakeoverAfter: time.Nanosecond})
	over, err := takeover.Attempt("granted-2", "ses_granted")
	if err != nil || over.Verdict != dedupe.Proceed {
		t.Fatalf("the take-over of the unsettled attempt = %+v (%v), want to proceed", over, err)
	}
	if err := takeover.Clear(over); err != nil {
		t.Fatalf("clear the attempt: %v", err)
	}
	if _, err := kv.Get(takeover.Key("granted-2", "ses_granted")); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("the cleared attempt's record reads %v, want no such key", err)
	}

	const (
		sessionID = "ses_granted"
		ttl       = 3 * time.Second
	)
	sessions, err := session.OpenSessionRegistry(t.Context(), conn, session.WithSessionReplicas(1), session.WithSessionTTL(ttl),
		session.WithLastSeenBucket(kv))
	if err != nil {
		t.Fatalf("open the session registry under %s's grant: %v", leaseTestMachine, err)
	}
	t.Cleanup(sessions.StopWatch)
	if err := sessions.Put(sessionID, session.SessionEntry{Port: 4242, MachineID: leaseTestMachine}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	recorded, err := kv.Get(bus.LastSeenPrefix + dedupe.SessionToken(sessionID))
	if err != nil {
		t.Fatalf("read %s's last-seen record: %v", sessionID, err)
	}
	var record struct {
		At int64 `json:"at"`
	}
	if err := json.Unmarshal(recorded.Value(), &record); err != nil || record.At == 0 {
		t.Fatalf("%s's last-seen record %s (%v), want a time", sessionID, recorded.Value(), err)
	}
	testwait.WithinPolling(t, ttl+10*time.Second, 50*time.Millisecond, sessionID+"'s registration to lapse", func() bool {
		_, err := sessions.Get(sessionID)
		return errors.Is(err, natsgo.ErrKeyNotFound)
	})
	if lastSeen := sessions.LastSeen(t.Context(), sessionID); lastSeen != record.At {
		t.Fatalf("the lapsed %s's last seen = %d, want its record's %d", sessionID, lastSeen, record.At)
	}

	select {
	case err := <-errs:
		t.Fatalf("the server refused a call the listener makes on its state bucket: %v", err)
	default:
	}
	if log := granted.Log(); strings.Contains(log, "Violation") {
		t.Fatalf("the server refused the listener what its grant should allow:\n%s", log)
	}
}

// A listener whose grant lacks the create of its own durable consumer never receives: it takes
// the machine's lease, its create is refused, and it delivers nothing. It stays up and tries the
// create again, naming the refusal at ERROR on every try (durableGrantRefusedLine), with the
// refused subject and the server's own text, and from the first refused try on /healthz answers
// 503 and /livez 204: unready while the refusal lasts, and never restarted for it, so it recovers
// at its next try once the grant is fixed. The infra live row
// TestHostPermissionsOnRealServers/*/without_its_durable's_consumer_create_the_listener_never_reaches_healthy
// watches it the same way for a minute. Before, #4343's start exited 1 at the refusal, and before
// that a holder answered 200 "degraded" while it retried, which kept it ready.
func TestAListenerWhoseGrantRefusesItsDurablesCreateStaysLiveAndUnready(t *testing.T) {
	t.Parallel()
	const (
		machineID = "box-no-create"
		instance  = "pod-a"
		// liveHealthWindow is how long packages/envoy/infra's live row watches this answer.
		liveHealthWindow = 60 * time.Second
	)
	seed, public := testnats.User(t)
	grant := testnats.ListenerGrant(t, machineID)
	// Three entries of the listener's grant admit the create of its own durable: the durable's own
	// subject, and the session consumers' two wildcards, which reach every consumer of the stream
	// (infra/core sessionConsumerCalls). All three go, or the create the test refuses still stands.
	durableCreate := "$JS.API.CONSUMER.CREATE." + bus.Stream + "." + bus.ListenerDurable(machineID)
	refused := []string{durableCreate, "$JS.API.CONSUMER.CREATE." + bus.Stream + ".*", "$JS.API.CONSUMER.CREATE." + bus.Stream + ".*.>"}
	held := len(grant.Publish.Allow)
	grant.Publish.Allow = slices.DeleteFunc(grant.Publish.Allow, func(subject string) bool { return slices.Contains(refused, subject) })
	if len(grant.Publish.Allow) != held-len(refused) {
		t.Fatalf("the listener's grant does not hold each of %q exactly once: %q", refused, grant.Publish.Allow)
	}
	granted := testnats.StartGranted(t, testnats.GrantedUser{Public: public, Grant: grant})
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), granted.URL(), machineID,
		"NATS_NKEY_SEED="+seed, "ENVOY_INSTANCE_ID="+instance)

	refusals := func() int {
		return countRecords(listener, durableGrantRefusedLine, func(record map[string]any) bool {
			return record["level"] == "ERROR" && strings.HasPrefix(fmt.Sprint(record["subject"]), "$JS.API.CONSUMER.CREATE."+bus.Stream+".") &&
				strings.Contains(fmt.Sprint(record["error"]), `Permissions Violation for Publish to "`+fmt.Sprint(record["subject"])+`"`)
		})
	}
	waitFor(t, "the listener to name its refused durable create at ERROR", func() bool { return refusals() > 0 })
	for deadline := time.Now().Add(liveHealthWindow); time.Now().Before(deadline); time.Sleep(time.Second) {
		select {
		case <-listener.Exited:
			t.Fatalf("the listener exited at its refused durable create:\n%s", listener.Output.String())
		default:
		}
		if status, body := callListener(t, listener.port, http.MethodGet, "/healthz", ""); status == http.StatusOK {
			t.Fatalf("/healthz answered %d %s while the grant refused the durable's create, want it unready:\n%s",
				status, body, listener.Output.String())
		}
		if status, body := callListener(t, listener.port, http.MethodGet, "/livez", ""); status != http.StatusNoContent {
			t.Fatalf("/livez answered %d %s while the grant refused the durable's create, want 204:\n%s",
				status, body, listener.Output.String())
		}
	}
	if got := refusals(); got < 2 {
		t.Fatalf("the listener named the refusal %d times in %s, want it named again at each try:\n%s", got, liveHealthWindow, listener.Output.String())
	}
	for _, line := range []string{durableRefusedLine, "durable consumer read failed; retrying"} {
		if strings.Contains(listener.Output.String(), line) {
			t.Fatalf("the listener logged %q for a refusal of its grant, want only %q:\n%s", line, durableGrantRefusedLine, listener.Output.String())
		}
	}
}

// A session relay's reply subscription, _INBOX.<id>.>, that the server refuses follows the rule a
// refused durable follows (TestAListenerWhoseGrantRefusesItsDurablesCreateStaysLiveAndUnready):
// the listener stays up and not ready, /livez 204 and /healthz 503 "starting", and opens the
// session consumers' connection and the subscription again (openStartConnection), naming the
// refusal at ERROR at each try (relayRepliesRefusedLine) with the refused subject and the
// server's text, after a wait of 1 s and then 2 s (nextRefusedWait); once the subscription is let
// through, a later try opens and the listener is ready. No grant of a real server refuses that
// subscription and keeps _INBOX.<id>.*, the one every request's replies come on, since the server
// matches a subscription's > as a token any * of the grant takes; so a relay (startGate) answers
// the session consumers' subscription as a server whose grant does not carry it answers, with its
// violation. Before, the start read no refusal of the subscription and served with a relay no
// reply reached.
func TestAListenerWhoseRelaysReplySubscriptionIsRefusedNamesItAtEachTryAndStaysUnready(t *testing.T) {
	t.Parallel()
	gate := startStartGate(t, testnats.URL(t))
	gate.refuseSubscriptions("envoy-session-consumers", func(subject string) bool {
		return strings.HasPrefix(subject, "_INBOX.") && strings.HasSuffix(subject, ".>")
	})
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), gate.url, "box-no-replies", "ENVOY_INSTANCE_ID=pod-a")
	const while = "its relay's reply subscription was refused"
	refusals := func() []map[string]any {
		var refused []map[string]any
		for _, record := range linesIn(listener.Output.String(), relayRepliesRefusedLine, map[string]any{"level": "ERROR"}) {
			subject, _ := record["subject"].(string)
			text, _ := record["error"].(string)
			if strings.HasPrefix(subject, "_INBOX.") && strings.HasSuffix(subject, ".>") &&
				strings.Contains(text, `Permissions Violation for Subscription to "`+subject+`"`) {
				refused = append(refused, record)
			}
		}
		return refused
	}
	for deadline := time.Now().Add(30 * time.Second); len(refusals()) < 2; time.Sleep(200 * time.Millisecond) {
		requireLiveAndStarting(t, listener, while)
		if time.Now().After(deadline) {
			t.Fatalf("the listener named the refused reply subscription %d times at ERROR within 30 s, want it named at each try:\n%s",
				len(refusals()), listener.Output.String())
		}
	}
	for index, wait := range []string{"1s", "2s"} {
		if record := refusals()[index]; record["retry_in"] != wait {
			t.Fatalf("refusal %d = %v, want retry_in %s", index+1, record, wait)
		}
	}
	requireLiveAndStarting(t, listener, while)
	gate.refuseSubscriptions("envoy-session-consumers", nil)
	listener.waitHealthy(t)
}
