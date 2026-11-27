package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The bucket is created with the configured replicas, file storage, listenerStateMaxAge and one
// value per key.
func TestListenerStateBucketIsCreatedAtTheConfiguredReplicas(t *testing.T) {
	t.Parallel()
	t.Run("one replica", func(t *testing.T) {
		kv := leaseBucket(t)
		status, err := kv.Status()
		if err != nil {
			t.Fatalf("read the bucket's status: %v", err)
		}
		bucket, ok := status.(*natsgo.KeyValueBucketStatus)
		if !ok {
			t.Fatalf("status is %T", status)
		}
		config := bucket.StreamInfo().Config
		if bucket.Bucket() != "envoy_listener" || bus.ListenerStateBucket != "envoy_listener" {
			t.Fatalf("bucket %q, constant %q, want envoy_listener", bucket.Bucket(), bus.ListenerStateBucket)
		}
		if config.Replicas != 1 || config.Storage != natsgo.FileStorage || config.MaxAge != listenerStateMaxAge || bucket.History() != 1 {
			t.Fatalf("replicas %d, storage %v, max age %s, history %d; want 1, file, %s, 1",
				config.Replicas, config.Storage, config.MaxAge, bucket.History(), listenerStateMaxAge)
		}
	})
	// A single test server cannot hold three replicas, and refuses the create for exactly that,
	// which shows the configured count is what the create asks for.
	t.Run("three replicas reach the server", func(t *testing.T) {
		conn := testnats.Connect(t, testnats.URL(t))
		t.Cleanup(conn.Close)
		_, err := openListenerState(t.Context(), conn, 3, listenerStateMaxAge, listenerStateRequestWait)
		if err == nil || !strings.Contains(err.Error(), "replicas > 1 not supported") {
			t.Fatalf("open at 3 replicas on one server: %v, want the server's refusal of replicas > 1", err)
		}
		if errors.Is(err, errListenerStateRefused) {
			t.Fatalf("a refused replica count reads as a missing grant: %v", err)
		}
	})
}

// The listener state bucket's age (listenerStateMaxAge) is at least the whole window a default
// session registry reports a lapsed session's last_seen for past its last registration write, the
// span seenUntil adds (session.SessionRegistry.LastSeenWindow: the session TTL and its last-seen
// retention), plus the interest reaper's allowance (store.InterestReapAllowance of
// store.InterestReapGrace), and so at least the session TTL plus that allowance; a bucket the
// open creates under that registry keeps its keys that long. No pod's answer then outlives the
// `seen.` record it is read from.
func TestTheListenerStateBucketOutlivesTheLastSeenWindow(t *testing.T) {
	t.Parallel()
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	sessions, err := session.OpenSessionRegistry(t.Context(), conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	allowance := store.InterestReapAllowance(store.InterestReapGrace)
	if ttlWindow := sessions.TTL() + allowance; listenerStateMaxAge < ttlWindow {
		t.Fatalf("the listener state bucket's age is %s, want at least the session TTL plus the interest reaper's allowance, %s", listenerStateMaxAge, ttlWindow)
	}
	window := sessions.LastSeenWindow() + allowance
	if listenerStateMaxAge < window {
		t.Fatalf("the listener state bucket's age is %s, want at least the session's last-seen window (the TTL and its last-seen retention) plus the interest reaper's allowance, %s", listenerStateMaxAge, window)
	}
	if _, err := openListenerState(t.Context(), conn, 1, listenerStateAgeOf(sessions)(), listenerStateRequestWait); err != nil {
		t.Fatalf("open the listener state bucket: %v", err)
	}
	if got := listenerStateStreamAge(t, conn); got < window {
		t.Fatalf("the listener state bucket keeps a record for %s, want at least %s", got, window)
	}
}

// listenerStateStreamAge is the MaxAge the listener state bucket's stream carries, read over conn.
func listenerStateStreamAge(t *testing.T, conn *natsgo.Conn) time.Duration {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	info, err := js.StreamInfo("KV_" + bus.ListenerStateBucket)
	if err != nil {
		t.Fatalf("read the bucket's stream: %v", err)
	}
	return info.Config.MaxAge
}

// A longer session TTL reaches the listener state bucket. The session registry opened under a
// longer ENVOY_SESSION_MAX_AGE (session.WithSessionMaxAge) runs with that TTL, and each open of the
// bucket under it, the start's and a late join's (joinDeliveryLease) alike, asks for the
// registry's last-seen window and the interest reaper's allowance (listenerStateAgeOf): the
// start's open creates an absent bucket at that age, a join raises a bucket the default age
// created to it, and an open at the default age afterwards lowers it to nothing shorter.
func TestALongerSessionTTLRaisesTheListenerStateBucketsAge(t *testing.T) {
	t.Parallel()
	const configured = 6 * time.Hour
	// longer opens the session registry over conn as the listener does under an
	// ENVOY_SESSION_MAX_AGE of configured, and returns what each open of the bucket asks for.
	longer := func(t *testing.T, conn *natsgo.Conn) func() time.Duration {
		t.Helper()
		sessions, err := session.OpenSessionRegistry(t.Context(), conn, session.WithSessionReplicas(1), session.WithSessionMaxAge(configured))
		if err != nil {
			t.Fatalf("open the session registry at a %s maximum age: %v", configured, err)
		}
		t.Cleanup(sessions.StopWatch)
		if ttl := sessions.TTL(); ttl != configured {
			t.Fatalf("the session registry runs with a %s TTL, want the configured %s", ttl, configured)
		}
		age := listenerStateAgeOf(sessions)
		if want := sessions.LastSeenWindow() + store.InterestReapAllowance(store.InterestReapGrace); age() != want || want <= listenerStateMaxAge {
			t.Fatalf("the listener state bucket's age under a %s session TTL is %s, want %s, the registry's last-seen window and the interest reaper's allowance, longer than the default %s",
				configured, age(), want, listenerStateMaxAge)
		}
		return age
	}

	t.Run("the start's open creates the bucket at it", func(t *testing.T) {
		t.Parallel()
		conn := testnats.Connect(t, testnats.URL(t))
		t.Cleanup(conn.Close)
		age := longer(t, conn)
		if _, err := openListenerState(t.Context(), conn, 1, age(), listenerStateRequestWait); err != nil {
			t.Fatalf("open the absent bucket: %v", err)
		}
		if got := listenerStateStreamAge(t, conn); got != age() {
			t.Fatalf("the bucket keeps a record for %s after the open, want %s", got, age())
		}
		if _, err := openListenerState(t.Context(), conn, 1, listenerStateMaxAge, listenerStateRequestWait); err != nil {
			t.Fatalf("open the bucket at the default age: %v", err)
		}
		if got := listenerStateStreamAge(t, conn); got != age() {
			t.Fatalf("the bucket keeps a record for %s after an open at the default %s, want %s: an open lowers no age", got, listenerStateMaxAge, age())
		}
	})
	t.Run("a late join's open raises the bucket to it", func(t *testing.T) {
		t.Parallel()
		conn := testnats.Connect(t, testnats.URL(t))
		t.Cleanup(conn.Close)
		if _, err := openListenerState(t.Context(), conn, 1, listenerStateMaxAge, listenerStateRequestWait); err != nil {
			t.Fatalf("open the absent bucket at the default age: %v", err)
		}
		if got := listenerStateStreamAge(t, conn); got != listenerStateMaxAge {
			t.Fatalf("the bucket keeps a record for %s, want the default %s", got, listenerStateMaxAge)
		}
		age := longer(t, conn)
		if joined := joinDeliveryLease(t.Context(), conn, 1, age, leaseTestMachine, "pod-a", logging.NewWithWriter(leaseTestMachine, io.Discard)); joined == nil {
			t.Fatal("the join made no lease")
		}
		if got := listenerStateStreamAge(t, conn); got != age() {
			t.Fatalf("the bucket keeps a record for %s after the join, want it raised to %s", got, age())
		}
	})
}

// A bucket an earlier build created with a shorter MaxAge is raised to listenerStateMaxAge when
// a listener opens it: a create that finds the bucket there changes nothing of it, so without the
// raise a deployment that ran an earlier build would drop every `seen.` record at that build's
// shorter age, and its pods' last_seen answers would end with it.
func TestOpeningTheListenerStateBucketRaisesAnEarlierBuildsMaxAge(t *testing.T) {
	t.Parallel()
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := bus.EnsureKeyValue(t.Context(), conn, js, &natsgo.KeyValueConfig{
		Bucket:   bus.ListenerStateBucket,
		History:  1,
		TTL:      10 * time.Minute,
		Storage:  natsgo.FileStorage,
		Replicas: 1,
	}); err != nil {
		t.Fatalf("create the bucket as an earlier build did: %v", err)
	}
	if _, err := openListenerState(t.Context(), conn, 1, listenerStateMaxAge, listenerStateRequestWait); err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	info, err := js.StreamInfo("KV_" + bus.ListenerStateBucket)
	if err != nil {
		t.Fatalf("read the bucket's stream: %v", err)
	}
	if info.Config.MaxAge != listenerStateMaxAge {
		t.Fatalf("the bucket keeps a record for %s after the open, want %s", info.Config.MaxAge, listenerStateMaxAge)
	}
}

// A user whose grant lacks the bucket is refused with errListenerStateRefused, naming the bucket
// and the grant; once the grant has it the same connection opens the bucket and takes the lease;
// a connection error is not the refusal.
// It must not run in parallel: the refusal must arrive inside the open's 1.5 s (3 waits of 500 ms).
func TestListenerStateBucketRefusalIsTheMissingGrant(t *testing.T) {
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, "_INBOX.>", "$JS.API.INFO")
	// The server's violation is expected; the handler keeps nats.go from printing it.
	conn, err := testnats.ConnectSeed(t, grant.URL, seed, natsgo.ErrorHandler(func(*natsgo.Conn, *natsgo.Subscription, error) {}))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	_, err = openListenerState(t.Context(), conn, 1, listenerStateMaxAge, 500*time.Millisecond)
	if !errors.Is(err, errListenerStateRefused) || !errors.Is(err, natsgo.ErrPermissionViolation) {
		t.Fatalf("open without the grant: %v, want errListenerStateRefused wrapping the server's violation", err)
	}
	for _, want := range []string{"envoy_listener", "grant", `Publish to "$JS.API.STREAM.INFO.KV_envoy_listener"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}

	grant.Grant(t, "_INBOX.>", "$JS.API.>", "$KV.envoy_listener.>")
	kv, err := openListenerState(t.Context(), conn, 1, listenerStateMaxAge, listenerStateRequestWait)
	if err != nil {
		t.Fatalf("open with the grant: %v", err)
	}
	l := testLease(kv, "pod-a", productionLeaseConfig(), time.Now)
	l.step()
	if !l.Holding() {
		t.Fatal("the lease was not taken under the grant")
	}

	conn.Close()
	if _, err := openListenerState(t.Context(), conn, 1, listenerStateMaxAge, 500*time.Millisecond); err == nil || errors.Is(err, errListenerStateRefused) {
		t.Fatalf("open on a closed connection: %v, want a connection error that is not the refusal", err)
	}
}

// Only a violation of a subject of the listener state bucket, as the open's own error or as a
// last error that changed across the open, is the refusal.
func TestListenerStateBucketRefusalClassification(t *testing.T) {
	t.Parallel()
	violation := func(subject string) error {
		return fmt.Errorf("%w: Permissions Violation for Publish to %q", natsgo.ErrPermissionViolation, subject)
	}
	stale := violation("$JS.API.STREAM.INFO.KV_envoy_listener")
	for _, tc := range []struct {
		name               string
		err, before, after error
		refused            bool
	}{
		{name: "stream info refused", err: natsgo.ErrTimeout, after: violation("$JS.API.STREAM.INFO.KV_envoy_listener"), refused: true},
		{name: "stream create refused", err: natsgo.ErrTimeout, after: violation("$JS.API.STREAM.CREATE.KV_envoy_listener"), refused: true},
		{name: "direct get refused", err: natsgo.ErrTimeout, after: violation("$JS.API.DIRECT.GET.KV_envoy_listener.$KV.envoy_listener.lease.listener-box-a"), refused: true},
		{name: "key write refused", err: natsgo.ErrTimeout, after: violation("$KV.envoy_listener.lease.listener-box-a"), refused: true},
		{name: "the open's own error", err: violation("$JS.API.STREAM.INFO.KV_envoy_listener"), refused: true},
		{name: "another bucket", err: natsgo.ErrTimeout, after: violation("$JS.API.STREAM.INFO.KV_envoy_listener_other")},
		{name: "another bucket's keys", err: natsgo.ErrTimeout, after: violation("$KV.envoy_listeners.x")},
		{name: "a subscription refusal", err: natsgo.ErrTimeout, after: fmt.Errorf("%w: Permissions Violation for Subscription to %q", natsgo.ErrPermissionViolation, "$KV.envoy_listener.>")},
		{name: "unchanged last error", err: natsgo.ErrTimeout, before: stale, after: stale},
		{name: "a timeout alone", err: natsgo.ErrTimeout},
		{name: "not a violation", err: natsgo.ErrTimeout, after: errors.New(`Permissions Violation for Publish to "$KV.envoy_listener.x"`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := listenerStateRefusal(tc.err, tc.before, tc.after) != nil; got != tc.refused {
				t.Fatalf("refused = %v, want %v", got, tc.refused)
			}
		})
	}
}
