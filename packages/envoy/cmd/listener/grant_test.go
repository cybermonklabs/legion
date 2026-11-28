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
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

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

// One machine's grant writes its own delivery lease as the lease does (Create, a read back by key,
// Update), and it is refused a write to another machine's lease key, hand-back key or delivery
// ledger key, so no machine's user writes another machine's keys by a key write (what every
// listener user can still do to the whole bucket is natsgrant.ListenerState's to say). The grant
// names the hand-back and ledger keys for the clean-hand-back marks and the delivery ledger of
// dispatch://INFRA-6344, and the listener of this build writes and reads the lease key alone, so
// the row itself writes a key under the machine's own hand-back prefix (bus.ListenerHandBackPrefix)
// through the raw KV handle and reads it back, to show the grant lets that through. A refused
// Create draws two refusals, since nats.go reads the key after any failed create
// (nats.go@v1.55.0 kv.go:729-737): its write, and the direct get of the same key, which the grant
// refuses too. Each row waits for every refusal its write draws, and fails on any other answer.
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
		t.Fatalf("write box-a's hand-back key: %v", err)
	}
	if entry, err := kv.Get(mark); err != nil || len(entry.Value()) == 0 {
		t.Fatalf("read back box-a's hand-back key = %v, %v; want the value written", entry, err)
	}
	select {
	case err := <-errs:
		t.Fatalf("the server refused box-a's own lease or hand-back key: %v", err)
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

// Every call a listener makes on its state bucket passes under its machine's grant
// (testnats.ListenerGrant, whose entries for the bucket are natsgrant.ListenerState's nine
// subjects), and the server logs no permission violation, each made by the listener's own code:
//   - the open of the absent bucket (openListenerState): its lookup, its create, the wait for its
//     leader and the handle's open;
//   - the age raise of the bucket found with a shorter MaxAge (bus.RaiseMaxAge, from the open),
//     the stream info and update of KV_envoy_listener that the keeper's copy raise sends too
//     (raiseReplicas and raiseStream reach the same updateStream), which a single server of one
//     copy never needs;
//   - the lease: its create of an absent record and the read back, its renewal and its release,
//     and the next pod's poll and take-over.
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
	// A graceful stop that handed messages back: unsettled and released.
	a.Unsettle(term.Term)
	if err := a.Release(t.Context()); err != nil {
		t.Fatalf("release pod-a's lease: %v", err)
	}
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	b.step()
	if next := b.Status(); !next.Holding || next.How != leaseTakenReleased || !next.Settle {
		t.Fatalf("pod-b status = %+v, want held from pod-a's released record, settling", next)
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
