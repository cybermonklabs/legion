package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// lastSeenStreamAge is the MaxAge the last-seen bucket's stream carries, read over conn.
func lastSeenStreamAge(t *testing.T, conn *natsgo.Conn) time.Duration {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	info, err := js.StreamInfo("KV_" + bus.LastSeenBucket)
	if err != nil {
		t.Fatalf("read the last-seen bucket's stream: %v", err)
	}
	return info.Config.MaxAge
}

// The last-seen bucket is envoy_last_seen, created with the configured replicas, file storage,
// one value per key and the MaxAge its records need at the default session settings, 4 h 15 min
// (lastSeenMaxAge). A single test server cannot hold three replicas, and refuses the create for
// exactly that, an answer of the cluster's state (bus.ClusterStateAnswer), which shows the
// configured count is what the create asks for. The open's loop waits that answer out
// (startLastSeenBucket), as
// TestTheLastSeenBucketsOpenWaitsOutTheClustersStateAndOpensOnceItTakesTheBucket shows.
func TestLastSeenBucketIsCreatedAtTheConfiguredReplicas(t *testing.T) {
	t.Parallel()
	t.Run("one replica", func(t *testing.T) {
		conn := testnats.Connect(t, testnats.URL(t))
		t.Cleanup(conn.Close)
		kv, err := openLastSeen(t.Context(), conn, 1, lastSeenMaxAge)
		if err != nil {
			t.Fatalf("open the absent last-seen bucket: %v", err)
		}
		status, err := kv.Status()
		if err != nil {
			t.Fatalf("read the bucket's status: %v", err)
		}
		bucket, ok := status.(*natsgo.KeyValueBucketStatus)
		if !ok {
			t.Fatalf("status is %T", status)
		}
		config := bucket.StreamInfo().Config
		if bucket.Bucket() != "envoy_last_seen" || bus.LastSeenBucket != "envoy_last_seen" {
			t.Fatalf("bucket %q, constant %q, want envoy_last_seen", bucket.Bucket(), bus.LastSeenBucket)
		}
		if want := 4*time.Hour + 15*time.Minute; config.Replicas != 1 || config.Storage != natsgo.FileStorage || config.MaxAge != want || bucket.History() != 1 {
			t.Fatalf("replicas %d, storage %v, max age %s, history %d; want 1, file, %s, 1",
				config.Replicas, config.Storage, config.MaxAge, bucket.History(), want)
		}
	})
	t.Run("three replicas reach the server", func(t *testing.T) {
		conn := testnats.Connect(t, testnats.URL(t))
		t.Cleanup(conn.Close)
		_, err := openLastSeen(t.Context(), conn, 3, lastSeenMaxAge)
		if err == nil || !strings.Contains(err.Error(), "replicas > 1 not supported") {
			t.Fatalf("open at 3 replicas on one server: %v, want the server's refusal of replicas > 1", err)
		}
		if _, ok := bus.ClusterStateAnswer(err); !ok {
			t.Fatalf("the refused replica count %v reads as no answer of the cluster's state", err)
		}
	})
}

// The last-seen bucket's age (lastSeenMaxAge) is at least the whole window a default session
// registry reports a lapsed session's last_seen for past its last registration write, the span
// seenUntil adds (session.SessionRegistry.LastSeenWindow: the session TTL and its last-seen
// retention), plus the interest reaper's allowance (store.InterestReapAllowance of
// store.InterestReapGrace), and so at least the session TTL plus that allowance; a bucket the
// open creates under that registry keeps its keys that long. No pod's answer then outlives the
// `seen.` record it is read from.
func TestTheLastSeenBucketOutlivesTheLastSeenWindow(t *testing.T) {
	t.Parallel()
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	sessions, err := session.OpenSessionRegistry(t.Context(), conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	allowance := store.InterestReapAllowance(store.InterestReapGrace)
	window := sessions.LastSeenWindow() + allowance
	if lastSeenMaxAge < window {
		t.Fatalf("the last-seen bucket's age is %s, want at least the session's last-seen window (the TTL and its last-seen retention) plus the interest reaper's allowance, %s", lastSeenMaxAge, window)
	}
	if _, err := openLastSeen(t.Context(), conn, 1, lastSeenAge(sessions.LastSeenWindow())); err != nil {
		t.Fatalf("open the last-seen bucket: %v", err)
	}
	if got := lastSeenStreamAge(t, conn); got < window {
		t.Fatalf("the last-seen bucket keeps a record for %s, want at least %s", got, window)
	}
}

// A longer session TTL reaches the last-seen bucket. The session registry opened under a longer
// ENVOY_SESSION_MAX_AGE (session.WithSessionMaxAge) runs with that TTL, and each try of the
// bucket's open asks for the registry's last-seen window and the interest reaper's allowance at
// that try (lastSeenAge, startLastSeenBucket): an open creates an absent bucket at that age, a
// later pod's open raises a bucket the default age created to it, and an open at the default age
// afterwards lowers it to nothing shorter.
func TestALongerSessionTTLRaisesTheLastSeenBucketsAge(t *testing.T) {
	t.Parallel()
	const configured = 6 * time.Hour
	// longer opens the session registry over conn as the listener does under an
	// ENVOY_SESSION_MAX_AGE of configured, and returns it with the age an open asks for under it.
	longer := func(t *testing.T, conn *natsgo.Conn) (*session.SessionRegistry, time.Duration) {
		t.Helper()
		sessions, err := session.OpenSessionRegistry(t.Context(), conn, session.WithSessionReplicas(1), session.WithSessionMaxAge(configured))
		if err != nil {
			t.Fatalf("open the session registry at a %s maximum age: %v", configured, err)
		}
		t.Cleanup(sessions.StopWatch)
		if ttl := sessions.TTL(); ttl != configured {
			t.Fatalf("the session registry runs with a %s TTL, want the configured %s", ttl, configured)
		}
		age := lastSeenAge(sessions.LastSeenWindow())
		if want := sessions.LastSeenWindow() + store.InterestReapAllowance(store.InterestReapGrace); age != want || want <= lastSeenMaxAge {
			t.Fatalf("the last-seen bucket's age under a %s session TTL is %s, want %s, the registry's last-seen window and the interest reaper's allowance, longer than the default %s",
				configured, age, want, lastSeenMaxAge)
		}
		return sessions, age
	}

	t.Run("an open creates the bucket at it", func(t *testing.T) {
		t.Parallel()
		conn := testnats.Connect(t, testnats.URL(t))
		t.Cleanup(conn.Close)
		_, age := longer(t, conn)
		if _, err := openLastSeen(t.Context(), conn, 1, age); err != nil {
			t.Fatalf("open the absent bucket: %v", err)
		}
		if got := lastSeenStreamAge(t, conn); got != age {
			t.Fatalf("the bucket keeps a record for %s after the open, want %s", got, age)
		}
		if _, err := openLastSeen(t.Context(), conn, 1, lastSeenMaxAge); err != nil {
			t.Fatalf("open the bucket at the default age: %v", err)
		}
		if got := lastSeenStreamAge(t, conn); got != age {
			t.Fatalf("the bucket keeps a record for %s after an open at the default %s, want %s: an open lowers no age", got, lastSeenMaxAge, age)
		}
	})
	t.Run("a later pod's open raises the bucket to it", func(t *testing.T) {
		t.Parallel()
		client, err := bus.Connect([]string{testnats.URL(t)}, bus.WithName("last-seen-longer-ttl"))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)
		if _, err := openLastSeen(t.Context(), client.Conn, 1, lastSeenMaxAge); err != nil {
			t.Fatalf("open the absent bucket at the default age: %v", err)
		}
		if got := lastSeenStreamAge(t, client.Conn); got != lastSeenMaxAge {
			t.Fatalf("the bucket keeps a record for %s, want the default %s", got, lastSeenMaxAge)
		}
		sessions, age := longer(t, client.Conn)
		tried := startLastSeenBucket(t.Context(), client, 1, sessions, logging.NewWithWriter(leaseTestMachine, io.Discard))
		testwait.Receive(t, "the last-seen bucket's first try to end", tried)
		if got := lastSeenStreamAge(t, client.Conn); got != age {
			t.Fatalf("the bucket keeps a record for %s after the open, want it raised to %s", got, age)
		}
	})
}

// A last-seen bucket found with a shorter MaxAge is raised to the age the open asks for: a create
// that finds the bucket there changes nothing of it, so without the raise the bucket would drop
// every `seen.` record at that shorter age, and every pod's last_seen answer would end with it,
// inside the window the registry reports it for. One that keeps its records longer, or for good,
// is left as it is: an operator's longer age stands.
func TestOpeningTheLastSeenBucketRaisesAShorterMaxAge(t *testing.T) {
	t.Parallel()
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := bus.EnsureKeyValue(t.Context(), conn, js, &natsgo.KeyValueConfig{
		Bucket:   bus.LastSeenBucket,
		History:  1,
		TTL:      20 * time.Minute,
		Storage:  natsgo.FileStorage,
		Replicas: 1,
	}); err != nil {
		t.Fatalf("create the bucket at a shorter age: %v", err)
	}
	if _, err := openLastSeen(t.Context(), conn, 1, lastSeenMaxAge); err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	if got := lastSeenStreamAge(t, conn); got != lastSeenMaxAge {
		t.Fatalf("the bucket keeps a record for %s after the open, want it raised to %s", got, lastSeenMaxAge)
	}
	for _, kept := range []time.Duration{24 * time.Hour, 0} {
		info, err := js.StreamInfo("KV_" + bus.LastSeenBucket)
		if err != nil {
			t.Fatalf("read the bucket's stream: %v", err)
		}
		set := info.Config
		set.MaxAge = kept
		if _, err := js.UpdateStream(&set); err != nil {
			t.Fatalf("set the bucket's age to %s as an operator would: %v", kept, err)
		}
		if _, err := openLastSeen(t.Context(), conn, 1, lastSeenMaxAge); err != nil {
			t.Fatalf("open the bucket an operator set to %s: %v", kept, err)
		}
		if got := lastSeenStreamAge(t, conn); got != kept {
			t.Fatalf("the bucket an operator set to keep a record for %s keeps it for %s after the open, want it left as it was", kept, got)
		}
	}
}

// withoutBuckets is grant without a subject of any of buckets: a call on its stream
// (KV_<bucket>), a direct get on it, or a write of one of its keys ($KV.<bucket>.<key>), as a
// hub's grant rendered before those buckets joined it.
func withoutBuckets(grant testnats.Grant, buckets ...string) testnats.Grant {
	grant.Publish.Allow = slices.DeleteFunc(slices.Clone(grant.Publish.Allow), func(subject string) bool {
		return slices.ContainsFunc(buckets, func(bucket string) bool { return bucketSubject(subject, bucket) })
	})
	return grant
}

// lastSeenRow is one of the rows below that start a listener process whose last-seen bucket the
// NATS server refuses at first: listener is the process, js the server's JetStream as a user that
// reads every bucket, and machineID the listener's machine.
type lastSeenRow struct {
	listener  *listenerProcess
	js        natsgo.JetStreamContext
	machineID string
}

// requireServing fails the test unless the listener is still running and answers /healthz
// healthy: a pod that serves while the last-seen bucket is refused.
func (r lastSeenRow) requireServing(t *testing.T, while string) {
	t.Helper()
	select {
	case <-r.listener.Exited:
		output := r.listener.Output.String()
		t.Fatalf("the listener exited while %s, with %s; its last line was %q:\n%s", while, r.listener.Cmd.ProcessState, lastOutputLine(output), output)
	default:
	}
	if status, body := callListener(t, r.listener.port, http.MethodGet, "/healthz", ""); status != http.StatusOK || !strings.Contains(body, `"status":"healthy"`) {
		t.Fatalf("/healthz while %s = %d %s, want 200 healthy:\n%s", while, status, body, r.listener.Output.String())
	}
}

// awaitTries waits, the listener serving throughout, until it has logged line at least twice with
// the attributes of where, and checks that the first two name the waits of the open's backoff, 1 s
// and then 2 s (startLastSeenBucket), and the server's own text (want). It returns the lines.
func (r lastSeenRow) awaitTries(t *testing.T, line string, where map[string]any, want, while string) []map[string]any {
	t.Helper()
	tries := func() []map[string]any { return linesIn(r.listener.Output.String(), line, where) }
	testwait.WithinPolling(t, 30*time.Second, 200*time.Millisecond, fmt.Sprintf("the listener to log %q %v twice while %s", line, where, while), func() bool {
		r.requireServing(t, while)
		return len(tries()) >= 2
	})
	found := tries()
	for index, wait := range []string{"1s", "2s"} {
		if text, _ := found[index]["error"].(string); found[index]["retry_in"] != wait || !strings.Contains(text, want) {
			t.Fatalf("try %d of the last-seen bucket logged %v, want retry_in %s and the server's %q", index+1, found[index], wait, want)
		}
	}
	return found
}

// register registers sessionID through the listener on consumer delivery, as a session's plugin
// does, and returns the last_seen GET /v1/registry/<session_id> answers for it while it is live:
// its registration's time.
func (r lastSeenRow) register(t *testing.T, sessionID string) int64 {
	t.Helper()
	registerThroughListener(t, r.listener, listenerwire.SubscribeRequest{
		SessionID: sessionID, Topics: []string{}, Title: sessionID, Dir: "/test/" + sessionID,
		SelfSubscribed: true, Host: r.machineID,
		Delivery: &listenerwire.SubscribeDelivery{Consumer: true, Epoch: "last-seen-" + id.New()},
	})
	status, body := callListener(t, r.listener.port, http.MethodGet, "/v1/registry/"+sessionID, "")
	var info listenerwire.SessionInfo
	if err := json.Unmarshal([]byte(body), &info); status != http.StatusOK || err != nil || info.LastSeen == 0 {
		t.Fatalf("GET /v1/registry/%s after its registration = %d %s (%v), want 200 with a last_seen", sessionID, status, body, err)
	}
	return info.LastSeen
}

// requireLastSeenKeptInProcess registers sessionID, takes its registration out of the session
// bucket as its expiry would, and requires the listener to answer GET /v1/registry/<session_id>
// 404 with the registration's time as last_seen while the last-seen bucket holds no record of it:
// the answer of the listener's own process (session.SessionRegistry's seenLocal).
func (r lastSeenRow) requireLastSeenKeptInProcess(t *testing.T, sessionID string) {
	t.Helper()
	registered := r.register(t, sessionID)
	sessions, err := r.js.KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	if err := sessions.Delete(sessionID); err != nil {
		t.Fatalf("take %s's registration out of the session bucket: %v", sessionID, err)
	}
	var refusal listenerwire.NoLiveSessionRefusal
	testwait.WithinPolling(t, 10*time.Second, 100*time.Millisecond, "the listener to answer the lapsed "+sessionID+" 404", func() bool {
		status, body := callListener(t, r.listener.port, http.MethodGet, "/v1/registry/"+sessionID, "")
		return status == http.StatusNotFound && json.Unmarshal([]byte(body), &refusal) == nil
	})
	if refusal.LastSeen != registered {
		t.Fatalf("the listener answers the lapsed %s with last_seen %d, want its registration's %d, kept in its own process: %+v", sessionID, refusal.LastSeen, registered, refusal)
	}
	if _, err := r.js.KeyValue(bus.LastSeenBucket); !errors.Is(err, natsgo.ErrBucketNotFound) {
		t.Fatalf("the %s bucket while the listener could not open it: %v, want none", bus.LastSeenBucket, err)
	}
}

// awaitOpened waits for the listener to log lastSeenOpenedLine once the server takes the bucket,
// within the wait the open's backoff was in and one try, and requires the next registration to
// write its session's record in the bucket, with no restart: the process that logged its readiness
// once is the one that opens the bucket.
func (r lastSeenRow) awaitOpened(t *testing.T, sessionID, while string) {
	t.Helper()
	testwait.WithinPolling(t, durableRefusedRetryCap+lastSeenOpenBound+10*time.Second, 200*time.Millisecond, "the listener to open the last-seen bucket once "+while, func() bool {
		r.requireServing(t, "it waited to open the last-seen bucket once "+while)
		return len(linesIn(r.listener.Output.String(), lastSeenOpenedLine, map[string]any{"level": "INFO", "bucket": bus.LastSeenBucket})) == 1
	})
	if ready := countRecords(r.listener, "envoy-listener ready (NATS connected)", func(map[string]any) bool { return true }); ready != 1 {
		t.Fatalf("the listener logged its readiness %d times, want once: it started again:\n%s", ready, r.listener.Output.String())
	}
	r.register(t, sessionID)
	seen, err := r.js.KeyValue(bus.LastSeenBucket)
	if err != nil {
		t.Fatalf("open the last-seen bucket: %v", err)
	}
	key := bus.LastSeenPrefix + dedupe.SessionToken(sessionID)
	testwait.WithinPolling(t, 10*time.Second, 100*time.Millisecond, "the registration of "+sessionID+" to write its record in "+bus.LastSeenBucket, func() bool {
		_, err := seen.Get(key)
		return err == nil
	})
}

// A pod whose grant holds the listener state bucket and not the last-seen bucket (a hub's users an
// earlier envoy-infra rendered) starts and serves: it holds its machine's lease without waiting on
// the last-seen bucket, answers /healthz healthy and active, logs lastSeenRefusedLine at ERROR at
// each try of the bucket, naming the bucket, the grant, the refused subject and the server's text,
// 1 s and then 2 s apart, and answers a lapsed session's last_seen from its own process. Once an
// operator applies the grant that carries the bucket, a later try opens it with no restart, and the
// next registration writes its session's record there.
func TestAPodWhoseGrantLacksTheLastSeenBucketServesAndOpensItOnceAGrantCarriesIt(t *testing.T) {
	t.Parallel()
	const machineID, instance = "box-last-seen-grant", "pod-last-seen-grant"
	whole := testnats.ListenerGrant(t, machineID)
	seed, public := testnats.User(t)
	granted := testnats.StartGranted(t, testnats.GrantedUser{Public: public, Grant: withoutBuckets(whole, bus.LastSeenBucket)})
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), granted.URL(), machineID,
		"NATS_NKEY_SEED="+seed, "ENVOY_INSTANCE_ID="+instance)
	listener.waitHealthy(t)

	admin, err := bus.Connect([]string{granted.AdminURL}, bus.WithName("last-seen-grant-admin"))
	if err != nil {
		t.Fatalf("connect as the admin: %v", err)
	}
	t.Cleanup(admin.Close)
	state, err := bus.OpenKeyValue(t.Context(), admin.Conn, admin.JS(), bus.ListenerStateBucket)
	if err != nil {
		t.Fatalf("open the listener state bucket: %v", err)
	}
	entry, err := state.Get(bus.ListenerLeaseKey(machineID))
	if err != nil {
		t.Fatalf("read the lease record: %v", err)
	}
	if record, err := decodeLeaseRecord(entry.Value()); err != nil || record.Holder != instance || record.Released {
		t.Fatalf("the lease record = %+v (%v), want held by %s while the last-seen bucket is refused", record, err, instance)
	}

	row := lastSeenRow{listener: listener, js: admin.JS(), machineID: machineID}
	const while = "its grant lacked the last-seen bucket"
	for _, refusal := range row.awaitTries(t, lastSeenRefusedLine, map[string]any{"level": "ERROR", "bucket": bus.LastSeenBucket, "grant": listenerGrant}, "Permissions Violation", while) {
		if subject, _ := refusal["subject"].(string); !strings.Contains(subject, "KV_"+bus.LastSeenBucket) {
			t.Fatalf("the refusal %v names subject %q, want a call on KV_%s", refusal, subject, bus.LastSeenBucket)
		}
	}
	row.requireLastSeenKeptInProcess(t, "ses_last_seen_grant_lapsed")

	granted.Regrant(t, testnats.GrantedUser{Public: public, Grant: whole})
	row.awaitOpened(t, "ses_last_seen_grant_after", "a grant carried the bucket")
}

// A pod whose grant holds neither the listener state bucket nor the last-seen bucket starts and
// serves: it delivers without a lease, as the machine's one deliverer, /healthz names the lease
// unavailable, and it answers a lapsed session's last_seen from its own process. Once a grant holds
// both, with no restart, it joins the lease, so /healthz drops the lease field and the lease record
// names it, and it opens the last-seen bucket, where the next registration writes its session's
// record: the lease's join and the last-seen bucket's open each follow the grant on their own.
func TestAPodWhoseGrantLacksBothStateBucketsServesAndOpensBothOnceAGrantCarriesThem(t *testing.T) {
	t.Parallel()
	const (
		machineID = "box-last-seen-neither"
		instance  = "pod-last-seen-neither"
		delivered = "ses_last_seen_neither_delivered"
		topic     = "notifications.github.acme.neither.pr.1"
	)
	whole := testnats.ListenerGrant(t, machineID)
	seed, public := testnats.User(t)
	granted := testnats.StartGranted(t, testnats.GrantedUser{Public: public, Grant: withoutBuckets(whole, bus.ListenerStateBucket, bus.LastSeenBucket)})
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), granted.URL(), machineID,
		"NATS_NKEY_SEED="+seed, "ENVOY_INSTANCE_ID="+instance)
	listener.waitHealthy(t)
	if health := listener.health(t); health["lease"] != listenerLeaseUnavailable {
		t.Fatalf("/healthz without either bucket = %v, want lease %q", health, listenerLeaseUnavailable)
	}

	standIn := startSessionStandIn(t, granted.AdminURL, delivered, answerDone)
	registerThroughListener(t, listener, listenerwire.SubscribeRequest{
		SessionID: delivered, Topics: []string{topic}, Title: delivered, Dir: "/test/" + delivered,
		SelfSubscribed: true, Host: machineID,
		Delivery: &listenerwire.SubscribeDelivery{Consumer: true, Epoch: "neither-" + id.New()},
	})
	item := newEnvelope("github", topic, "pull request 1 was opened")
	publish(t, connectPublisher(t, granted.AdminURL), item)
	if frame := standIn.collect(t, 1, 15*time.Second)[0]; frame.item.EventID != item.EventID {
		t.Fatalf("the frame relayed to %s carries event %s, want %s", delivered, frame.item.EventID, item.EventID)
	}

	admin, err := bus.Connect([]string{granted.AdminURL}, bus.WithName("last-seen-neither-admin"))
	if err != nil {
		t.Fatalf("connect as the admin: %v", err)
	}
	t.Cleanup(admin.Close)
	row := lastSeenRow{listener: listener, js: admin.JS(), machineID: machineID}
	row.awaitTries(t, lastSeenRefusedLine, map[string]any{"level": "ERROR", "bucket": bus.LastSeenBucket, "grant": listenerGrant}, "Permissions Violation",
		"its grant lacked both state buckets")
	row.requireLastSeenKeptInProcess(t, "ses_last_seen_neither_lapsed")

	granted.Regrant(t, testnats.GrantedUser{Public: public, Grant: whole})
	testwait.WithinPolling(t, durableRefusedRetryCap+leaseJoinTry+10*time.Second, 200*time.Millisecond, "the listener to join its lease once a grant carried both buckets", func() bool {
		health := listener.health(t)
		_, named := health["lease"]
		return health["status"] == "healthy" && health["role"] == "active" && !named
	})
	state, err := bus.OpenKeyValue(t.Context(), admin.Conn, admin.JS(), bus.ListenerStateBucket)
	if err != nil {
		t.Fatalf("open the listener state bucket: %v", err)
	}
	entry, err := state.Get(bus.ListenerLeaseKey(machineID))
	if err != nil {
		t.Fatalf("read the lease record: %v", err)
	}
	if record, err := decodeLeaseRecord(entry.Value()); err != nil || record.Holder != instance {
		t.Fatalf("the lease record after the join = %+v (%v), want held by %s", record, err, instance)
	}
	row.awaitOpened(t, "ses_last_seen_neither_after", "a grant carried both buckets")
}

// leaseJoinTry bounds a try of the listener state bucket that was in flight when its grant came:
// the open waits listenerStateOpenSends of listenerStateRequestWait at most, and a second for the
// rest of it.
const leaseJoinTry = listenerStateOpenSends*listenerStateRequestWait + time.Second

// An account at its JetStream stream limit (max_streams) with a slot for every stream and bucket
// a listener opens but the last-seen bucket refuses that bucket's create alone (JetStream code
// 10027), an answer of the account's state, which no setting of the listener changes
// (bus.AccountStateAnswer). The pod starts and serves all the same: it holds its lease, answers
// /healthz healthy, logs lastSeenAccountStateLine at ERROR at each try with the bucket, the code
// and the server's text, 1 s and then 2 s apart, and answers a lapsed session's last_seen from its
// own process. Once a slot is freed, a later try creates the bucket with no restart, and the next
// registration writes its session's record there. A first pod of the same build, with the account
// to itself, made every other stream and bucket, so the second pod's start creates nothing but the
// last-seen bucket.
func TestAPodWhoseAccountHasNoRoomForTheLastSeenBucketServesAndOpensItOnceASlotIsFree(t *testing.T) {
	t.Parallel()
	const machineID = "box-last-seen-account"
	uri, js, _ := accountAtItsStreamLimit(t, 0)
	binary := cmdtest.Build(t, "envoy-listener")
	first := startListenerProcess(t, binary, uri, machineID, "ENVOY_INSTANCE_ID=pod-account-first")
	first.waitHealthy(t)
	first.Terminate(t)
	first.WaitExit(t, "SIGTERM")
	if err := js.DeleteStream("KV_" + bus.LastSeenBucket); err != nil {
		t.Fatalf("delete the last-seen bucket the first pod made: %v", err)
	}
	held := 0
	for range js.StreamNames() {
		held++
	}
	var fillers []string
	for index := range accountStreamLimit - held {
		name := fmt.Sprintf("FILLER_%d", index)
		if _, err := js.AddStream(&natsgo.StreamConfig{Name: name, Subjects: []string{fmt.Sprintf("filler.%d", index)}}); err != nil {
			t.Fatalf("fill the account's stream slot %d: %v", held+index+1, err)
		}
		fillers = append(fillers, name)
	}
	if len(fillers) == 0 {
		t.Fatalf("the first pod's %d streams leave no slot of the account's %d to fill", held, accountStreamLimit)
	}

	listener := startListenerProcess(t, binary, uri, machineID, "ENVOY_INSTANCE_ID=pod-account-second")
	listener.waitHealthy(t)
	row := lastSeenRow{listener: listener, js: js, machineID: machineID}
	row.awaitTries(t, lastSeenAccountStateLine, map[string]any{"level": "ERROR", "bucket": bus.LastSeenBucket, "code": maximumStreamsCode},
		"maximum number of streams reached", "its account had no slot for the last-seen bucket")
	row.requireLastSeenKeptInProcess(t, "ses_last_seen_account_lapsed")

	freeStreams(t, js, fillers[:1])
	row.awaitOpened(t, "ses_last_seen_account_after", "a slot was freed")
}

// The last-seen bucket's open waits out an answer of the cluster's state for that bucket and opens
// it once the cluster takes it, on a hub of two servers that a third joins later
// (testnats.StartClusterPart): the meta group can place no stream of three copies, so the server
// answers the bucket's create "no suitable peers for placement" (JSClusterNoPeersErrF, 10005), an
// answer of the cluster's state (bus.ClusterStateAnswer). The open's loop logs clusterStateLine
// at each try, naming the bucket, the setting and the server's text, after a wait of 1 s and then
// 2 s, ends its first try without the bucket, so a start would serve, and the session registry
// answers a lapsed session's last_seen from its own process meanwhile. Once the third server
// joins, a later try creates the bucket at three copies, and the next registration writes its
// session's record there. The registry's own bucket has two copies, which the two servers hold, so
// the bucket's create is the one request the cluster refuses.
func TestTheLastSeenBucketsOpenWaitsOutTheClustersStateAndOpensOnceItTakesTheBucket(t *testing.T) {
	t.Parallel()
	cluster := testnats.StartClusterPart(t, 3, 2)
	urls := cluster.URLs()
	client, err := bus.Connect(urls[:2], bus.WithName("last-seen-cluster"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	sessions, err := session.OpenSessionRegistry(t.Context(), client.Conn, session.WithSessionReplicas(2))
	if err != nil {
		t.Fatalf("open the session registry at two copies: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	logs := &cmdtest.LockedBuffer{}
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	tried := startLastSeenBucket(ctx, client, 3, sessions, logging.NewWithWriter("box-last-seen-cluster", logs))
	testwait.Receive(t, "the last-seen bucket's first try to end", tried)

	tries := func() []map[string]any {
		return linesIn(logs.String(), clusterStateLine, map[string]any{"bucket": bus.LastSeenBucket, "setting": bus.ReplicasEnvVar + "=3"})
	}
	testwait.WithinPolling(t, 30*time.Second, 200*time.Millisecond, "the open to log the cluster's answer twice", func() bool { return len(tries()) >= 2 })
	found := tries()
	for index, wait := range []string{"1s", "2s"} {
		record := found[index]
		if text, _ := record["error"].(string); record["retry_in"] != wait || record["code"] != float64(10005) || !strings.Contains(text, "no suitable peers for placement") {
			t.Fatalf("try %d = %v, want retry_in %s, code 10005 and the server's text", index+1, record, wait)
		}
	}

	js := cluster.JetStream(t)
	const lapsed = "ses_last_seen_cluster_lapsed"
	if err := sessions.Put(lapsed, session.SessionEntry{Port: 4242, MachineID: "box-last-seen-cluster"}); err != nil {
		t.Fatalf("register %s: %v", lapsed, err)
	}
	registered, err := sessions.Get(lapsed)
	if err != nil {
		t.Fatalf("read %s's registration: %v", lapsed, err)
	}
	registrations, err := js.KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	if err := registrations.Delete(lapsed); err != nil {
		t.Fatalf("take %s's registration out of the session bucket: %v", lapsed, err)
	}
	testwait.WithinPolling(t, 10*time.Second, 50*time.Millisecond, lapsed+" to lapse in the registry", func() bool {
		_, err := sessions.Get(lapsed)
		return errors.Is(err, natsgo.ErrKeyNotFound)
	})
	if got := sessions.LastSeen(t.Context(), lapsed); got != registered.UpdatedAt {
		t.Fatalf("the registry answers the lapsed %s with last_seen %d, want its registration's %d, kept in process", lapsed, got, registered.UpdatedAt)
	}
	if _, err := js.KeyValue(bus.LastSeenBucket); !errors.Is(err, natsgo.ErrBucketNotFound) {
		t.Fatalf("the %s bucket while the cluster could not place it: %v, want none", bus.LastSeenBucket, err)
	}

	cluster.Servers[2].Start(t)
	cluster.AwaitReady(t)
	testwait.WithinPolling(t, durableRefusedRetryCap+lastSeenOpenBound+10*time.Second, 200*time.Millisecond, "the open to create the bucket once the third server joined", func() bool {
		return len(linesIn(logs.String(), lastSeenOpenedLine, map[string]any{"bucket": bus.LastSeenBucket})) == 1
	})
	testnats.AwaitStreamCopies(t, js, "KV_"+bus.LastSeenBucket, 3)
	const after = "ses_last_seen_cluster_after"
	if err := sessions.Put(after, session.SessionEntry{Port: 4243, MachineID: "box-last-seen-cluster"}); err != nil {
		t.Fatalf("register %s: %v", after, err)
	}
	seen, err := js.KeyValue(bus.LastSeenBucket)
	if err != nil {
		t.Fatalf("open the last-seen bucket: %v", err)
	}
	if _, err := seen.Get(bus.LastSeenPrefix + dedupe.SessionToken(after)); err != nil {
		t.Fatalf("read %s's last-seen record once the bucket opened: %v\n%s", after, err, logs.String())
	}
}
