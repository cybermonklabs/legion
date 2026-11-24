package store

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// latencyToken is the one claim the latency benchmarks write and read.
const latencyToken claim.Token = "legion-legion-legion-401-tester"

// latencyStores is one migrated database of its own, opened twice: on loopback, and through a
// proxy that adds a millisecond each way, as a database across a network would.
func latencyStores(b *testing.B) map[string]*Store {
	b.Helper()
	ctx := context.Background()
	dsn := testdb.Database(b, "legion_store_latency")
	loopback, err := Open(ctx, dsn)
	if err != nil {
		b.Fatalf("open the store: %v", err)
	}
	b.Cleanup(loopback.Close)
	if _, err := loopback.Migrate(ctx); err != nil {
		b.Fatalf("migrate the store: %v", err)
	}
	if err := loopback.PutClaim(ctx, supervise.Claim{Token: latencyToken, Project: "legion", Tree: "LEGION-401", Issue: "LEGION-401",
		Role: claim.RoleTester, Generation: 1, State: supervise.StateRegistered, Session: "ses-latency"}); err != nil {
		b.Fatalf("put the claim: %v", err)
	}
	if _, err := loopback.MintController(ctx, "legion", []byte("latency-capability")); err != nil {
		b.Fatalf("mint the controller: %v", err)
	}
	if _, err := loopback.PutClaimStep(ctx, latencyToken, supervise.StepRecord{At: time.Now().UTC().Add(-2 * time.Hour), Tool: "bash", CallID: "seed"}); err != nil {
		b.Fatalf("seed the claim's step: %v", err)
	}
	delayed, err := Open(ctx, testdb.Delayed(b, dsn, time.Millisecond))
	if err != nil {
		b.Fatalf("open the store through the delay: %v", err)
	}
	b.Cleanup(delayed.Close)
	return map[string]*Store{"loopback": loopback, "delayed_1ms": delayed}
}

// timed runs call b.N times, times each call alone, and reports the median and the 99th
// percentile of those times.
func timed(b *testing.B, call func(i int) error) {
	b.Helper()
	took := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := range b.N {
		start := time.Now()
		if err := call(i); err != nil {
			b.Fatal(err)
		}
		took = append(took, time.Since(start))
	}
	b.StopTimer()
	slices.Sort(took)
	b.ReportMetric(float64(took[len(took)/2].Nanoseconds()), "p50-ns/op")
	b.ReportMetric(float64(took[len(took)*99/100].Nanoseconds()), "p99-ns/op")
}

// The two claim writes of the hot path: a step the stream reports, and a session's activity
// report. Each is one statement, timed alone, on loopback and across a millisecond each way.
func BenchmarkClaimHotWrites(b *testing.B) {
	stores := latencyStores(b)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)
	for _, name := range []string{"loopback", "delayed_1ms"} {
		st := stores[name]
		b.Run("step/"+name, func(b *testing.B) {
			timed(b, func(i int) error {
				_, err := st.PutClaimStep(ctx, latencyToken, supervise.StepRecord{
					At: base.Add(time.Duration(i) * time.Millisecond), Tool: "bash", CallID: fmt.Sprintf("call-%s-%d", name, i)})
				return err
			})
		})
		b.Run("activity/"+name, func(b *testing.B) {
			timed(b, func(i int) error {
				_, err := st.PutClaimActivity(ctx, latencyToken, 1, claim.SessionActivity{At: base.Add(time.Duration(i) * time.Millisecond)})
				return err
			})
		})
	}
}

// Two reads of the kind that run in a read-only transaction: the controller record each
// controller grant reads, and one claim's step.
func BenchmarkHotReads(b *testing.B) {
	stores := latencyStores(b)
	ctx := context.Background()
	for _, name := range []string{"loopback", "delayed_1ms"} {
		st := stores[name]
		b.Run("controller/"+name, func(b *testing.B) {
			timed(b, func(int) error {
				if _, found, err := st.Controller(ctx, "legion"); err != nil || !found {
					return fmt.Errorf("read the controller = %t, %v; want it found", found, err)
				}
				return nil
			})
		})
		b.Run("claim_step/"+name, func(b *testing.B) {
			timed(b, func(int) error {
				step, err := st.ClaimStep(ctx, latencyToken)
				if err != nil || step.At.IsZero() {
					return fmt.Errorf("read the claim's step = %+v, %v; want the seeded step", step, err)
				}
				return nil
			})
		})
	}
}
