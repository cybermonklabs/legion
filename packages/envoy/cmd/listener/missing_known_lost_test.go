package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The known-session bucket deleted under a running listener, the one copied bucket a listener
// serves without (optionalCopiedBucket): the self-health monitor's ticks hold it missing, so
// /healthz names it as 200 "degraded", but they log it once at WARN and never missingBucketLine at
// ERROR, which pages, and fail no tick (missingBuckets.err); the store answers a registration's
// write nil and writes nothing, and nothing creates the bucket. The control is the same two ticks
// over the session bucket deleted instead: missingBucketLine at ERROR at each tick, and a failed
// tick. At b20f45e30a the known-session bucket's ticks logged the ERROR and failed too, and the
// store's write answered bus.ErrBucketMissing, which refused the registration with a 503.
func TestAKnownSessionBucketLostUnderARunningListenerIsNamedOnceAndFailsNoTick(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		bucket   string
		optional bool
	}{
		{bus.KnownSessionBucket, true},
		{bus.SessionBucket, false},
	} {
		t.Run(row.bucket, func(t *testing.T) {
			t.Parallel()
			_, uri := testnats.Start(t)
			bustest.ProvisionURL(t, uri, 1)
			known, err := session.OpenKnownSessions(t.Context(), testnats.Connect(t, uri), session.WithKnownSessionReplicas(1))
			if err != nil {
				t.Fatalf("open the known-session store: %v", err)
			}
			t.Cleanup(known.StopWatch)
			if !testwait.Holds(10*time.Second, 50*time.Millisecond, known.Opened) {
				t.Fatal("control: the store never opened its bucket, so this test says nothing about a bucket lost under an open store")
			}
			operator := testnats.Connect(t, uri)
			js, err := operator.JetStream()
			if err != nil {
				t.Fatalf("jetstream: %v", err)
			}
			if err := js.DeleteKeyValue(row.bucket); err != nil {
				t.Fatalf("delete the %s bucket under the running store: %v", row.bucket, err)
			}

			logs := &cmdtest.LockedBuffer{}
			logger := logging.NewWithWriter("test-machine", logs)
			missing := &missingBuckets{}
			for range 2 {
				checkMonitoredBuckets(context.Background(), logger, js, listenerCopiedBuckets(false), known, missing)
			}
			if names := missing.list(); !slices.Equal(names, []string{row.bucket}) {
				t.Fatalf("missing after two ticks = %v, want [%s]: /healthz names what this holds", names, row.bucket)
			}
			errorLines := countLogLines(logs.String(), "ERROR", missingBucketLine)
			warnLines := countLogLines(logs.String(), "WARN", optionalBucketMissingLine)
			if !row.optional {
				if missing.err() == nil || errorLines != 2 || warnLines != 0 {
					t.Fatalf("control: two ticks over %s missing = tick error %v, %d ERROR and %d WARN lines, want a tick error, 2 and 0", row.bucket, missing.err(), errorLines, warnLines)
				}
				return
			}
			if err := missing.err(); err != nil || errorLines != 0 || warnLines != 1 {
				t.Fatalf("two ticks over the known-session bucket missing = tick error %v, %d ERROR and %d WARN lines, want none, 0 and 1", err, errorLines, warnLines)
			}
			if err := known.RecordWithin(context.Background(), session.KnownSession{SessionID: "ses_lost_bucket", Topics: []string{"notifications.team.lost"}}); err != nil {
				t.Fatalf("a registration's write over the lost bucket = %v, want nil: the store writes nothing and the registration is answered", err)
			}
			if _, err := js.StreamInfo("KV_" + bus.KnownSessionBucket); !errors.Is(err, natsgo.ErrStreamNotFound) {
				t.Fatalf("the known-session bucket after the write = %v, want it still missing: nothing creates it", err)
			}
		})
	}
}
