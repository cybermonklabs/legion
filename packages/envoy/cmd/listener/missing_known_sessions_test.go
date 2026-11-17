package main

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The known-session bucket is a copied bucket like the others: a listener that starts
// while the server says it holds none waits at its start, every route answering 503 and /healthz
// naming the bucket, and serves once the bucket is there. Deleted under a running listener, it
// refuses each registration whose 30-day record it cannot write with the bus-unavailable 503,
// rather than answering 200 with the record dropped. At 7b262c2e0a the start did not wait for that
// bucket, and a registration was answered 200 with no record written.
func TestAMissingKnownSessionBucketHoldsTheStartAndRefusesARegistration(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "envoy-listener")
	uri := testnats.URL(t)
	conn := bustest.Connect(t, uri)
	if err := bustest.Provision(conn, 1); err != nil {
		t.Fatalf("provision the server: %v", err)
	}
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.KnownSessionBucket); err != nil {
		t.Fatalf("delete the known-session bucket: %v", err)
	}
	run := id.New()
	registration := consumerRegistrationIn(t, "ses_known_missing_"+run, "epoch-1", "/work/known-missing", "notifications.team.known-missing-"+run)

	// The server is provisioned already, so the harness provisions nothing and the bucket stays
	// missing for this start.
	listener := startListenerProcess(t, binary, uri, "known-sessions-missing")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the listener's output:\n%s", listener.Output.String())
		}
	})
	listener.WaitForOutput(t, missingBucketLine)
	waited := time.Now()
	for time.Since(waited) < 5*time.Second {
		code, status, missing := healthzMissing(t, listener.port)
		if code != http.StatusServiceUnavailable || status != "waiting" || !slices.Equal(missing, []string{bus.KnownSessionBucket}) {
			t.Fatalf("/healthz of a start with the known-session bucket missing = %d %q %v, want 503 waiting naming %s", code, status, missing, bus.KnownSessionBucket)
		}
		if code, answer := callListener(t, listener.port, http.MethodPost, "/v1/interests/subscribe", registration); code != http.StatusServiceUnavailable {
			t.Fatalf("a registration while the start waits on the known-session bucket answered %d %s, want 503", code, answer)
		}
		time.Sleep(time.Second)
	}

	config, _ := bus.CopiedBucketConfig(bus.KnownSessionBucket, 1)
	if _, err := js.CreateKeyValue(&config); err != nil {
		t.Fatalf("create the known-session bucket, as `envoy-infra buckets` would: %v", err)
	}
	listener.waitHealthy(t)
	postListener(t, listener.port, "/v1/interests/subscribe", registration)

	if err := js.DeleteKeyValue(bus.KnownSessionBucket); err != nil {
		t.Fatalf("delete the known-session bucket under the running listener: %v", err)
	}
	another := consumerRegistrationIn(t, "ses_known_deleted_"+run, "epoch-1", "/work/known-deleted", "notifications.team.known-deleted-"+run)
	if code, answer := callListener(t, listener.port, http.MethodPost, "/v1/interests/subscribe", another); code != http.StatusServiceUnavailable {
		t.Fatalf("a registration whose 30-day record cannot be written answered %d %s, want 503", code, answer)
	}
}
