package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// selfHealthTicks is how long a test waits for the self-health monitor to have acted on a missing
// bucket three times over: its 30 s interval times its threshold of 3, which is when a listener
// that took a missing bucket for a failure a restart repairs terminates itself, and the restart's
// store open created the bucket again, losing every write the deleted bucket acknowledged.
const selfHealthTicks = 3*30*time.Second + 10*time.Second

// healthzMissing is /healthz's answer on port: its status code, its "status" and the buckets its
// "missing_buckets" names.
func healthzMissing(t *testing.T, port int) (int, string, []string) {
	t.Helper()
	code, body := callListener(t, port, http.MethodGet, "/healthz", "")
	var answer struct {
		Status  string   `json:"status"`
		Missing []string `json:"missing_buckets"`
	}
	_ = json.Unmarshal([]byte(body), &answer)
	return code, answer.Status, answer.Missing
}

// requireBucketMissing fails the test unless the server js reaches holds no bucket named bucket.
func requireBucketMissing(t *testing.T, js natsgo.JetStreamContext, bucket, when string) {
	t.Helper()
	if _, err := js.KeyValue(bucket); !errors.Is(err, natsgo.ErrBucketNotFound) {
		t.Fatalf("%s: the %s bucket is there (%v), want it missing: a listener created it", when, bucket, err)
	}
}

// A copied bucket deleted under a running listener stays missing: the listener
// answers the write that needs it the bus-unavailable 503, /healthz names the bucket as 200
// "degraded", the self-health monitor neither rebuilds nor restarts over it through three of its
// intervals, and every check logs the line the AWS listener's bucket-missing alarm pages on. A
// listener that starts while the bucket is missing creates it no more: it waits at its starting
// gate, which answers 503, and /healthz names the bucket there as 503 "waiting", since that start
// serves no route.
//
// At main the self-health monitor terminates the listener after its third interval (a missing
// stream was terminal) and the restart's store open creates the bucket again, so the write the
// deleted bucket acknowledged is lost while the listener runs on (Prove4238's case b); the
// control is run on the real main binary in the PR's evidence.
func TestACopiedBucketDeletedUnderARunningListenerStaysMissing(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "envoy-listener")
	uri := testnats.URL(t)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	listener := startListenerProcess(t, binary, uri, "bucket-deleted-under")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the listener's output:\n%s", listener.Output.String())
		}
	})
	listener.waitHealthy(t)
	run := id.New()
	sessionID := "ses_bucket_deleted_" + run
	registration := consumerRegistrationIn(t, sessionID, "epoch-1", "/work/bucket-deleted", "notifications.team.bucket-deleted-"+run)
	postListener(t, listener.port, "/v1/interests/subscribe", registration)

	if err := js.DeleteKeyValue(bus.SessionBucket); err != nil {
		t.Fatalf("delete the session bucket: %v", err)
	}
	deleted := time.Now()
	deadline := time.Now().Add(35 * time.Second)
	for {
		code, status, missing := healthzMissing(t, listener.port)
		if code == http.StatusOK && status == "degraded" && slices.Contains(missing, bus.SessionBucket) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("/healthz never named the deleted session bucket: last %d %q %v", code, status, missing)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if code, answer := callListener(t, listener.port, http.MethodPost, "/v1/interests/subscribe", registration); code != http.StatusServiceUnavailable {
		t.Fatalf("a registration while the session bucket is missing answered %d %s, want 503", code, answer)
	}

	for time.Since(deleted) < selfHealthTicks {
		select {
		case <-listener.Exited:
			t.Fatalf("the listener exited %s after the session bucket was deleted, want it serving on:\n%s", time.Since(deleted).Round(time.Second), listener.Output.String())
		case <-time.After(5 * time.Second):
		}
		requireBucketMissing(t, js, bus.SessionBucket, "while the listener runs")
	}
	output := listener.Output.String()
	if !strings.Contains(output, missingBucketLine) {
		t.Fatalf("the listener never logged %q", missingBucketLine)
	}
	if strings.Contains(output, "terminating for restart") {
		t.Fatal("the self-health monitor terminated the listener over a missing bucket")
	}
	if code, status, missing := healthzMissing(t, listener.port); code != http.StatusOK || status != "degraded" || !slices.Equal(missing, []string{bus.SessionBucket}) {
		t.Fatalf("/healthz after three self-health intervals = %d %q %v, want 200 degraded naming %s", code, status, missing, bus.SessionBucket)
	}
	listener.Terminate(t)
	listener.WaitExit(t, "SIGTERM")

	// A start while the bucket is missing creates nothing: it waits at its starting gate. The
	// harness provisions a server once (bustest.Provision), so this start on the same server
	// finds the deleted bucket still missing. The start is judged by the bucket's state and its
	// answers from its first moment, not after a wait for its log line: a start that created the
	// bucket logs no such line, and a wait for it would end the test before any of the checks
	// below ran. Each pass reads the bucket first, so such a start fails here, naming the bucket
	// it created; and the start must answer 503 "waiting" for 10 s on end, inside 30 s.
	restarted := startListenerProcess(t, binary, uri, "bucket-deleted-under")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the restarted listener's output:\n%s", restarted.Output.String())
		}
	})
	started := time.Now()
	var waited time.Time
	for waited.IsZero() || time.Since(waited) < 10*time.Second {
		requireBucketMissing(t, js, bus.SessionBucket, "while a start waits on it")
		code, status, missing := healthzMissing(t, restarted.port)
		waiting := code == http.StatusServiceUnavailable && status == "waiting" && slices.Equal(missing, []string{bus.SessionBucket})
		switch {
		case waiting && waited.IsZero():
			waited = time.Now()
		case !waiting && !waited.IsZero():
			t.Fatalf("/healthz of a start waiting on the bucket = %d %q %v, want 503 waiting naming %s", code, status, missing, bus.SessionBucket)
		case !waiting && time.Since(started) > 30*time.Second:
			t.Fatalf("/healthz of the start 30 s after it began = %d %q %v, want 503 waiting naming %s", code, status, missing, bus.SessionBucket)
		}
		if waiting {
			if code, answer := callListener(t, restarted.port, http.MethodPost, "/v1/interests/subscribe", registration); code != http.StatusServiceUnavailable {
				t.Fatalf("a registration while the start waits on the bucket answered %d %s, want 503", code, answer)
			}
		}
		time.Sleep(time.Second)
	}
	if output := restarted.Output.String(); !strings.Contains(output, missingBucketLine) {
		t.Fatalf("the start waiting on the bucket never logged %q", missingBucketLine)
	}

	// Once the bucket is there again, the waiting start opens it and serves.
	config, _ := bus.CopiedBucketConfig(bus.SessionBucket, 1)
	if _, err := js.CreateKeyValue(&config); err != nil {
		t.Fatalf("create the session bucket, as `envoy-infra buckets` would: %v", err)
	}
	restarted.waitHealthy(t)
	postListener(t, restarted.port, "/v1/interests/subscribe", registration)
}
