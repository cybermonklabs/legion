package delivery

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
)

// Every reconcile pass searches and lists GitHub against one installation's rate limit, so one
// process reconciles however many tick: with the delivery-reconcile lease held by another caller
// - the other envoy-dispatch process, inside a pass of its own - this process's pass asks GitHub
// for nothing and leaves the settings' freshness untouched.
func TestAReconcilePassWhoseLeaseIsHeldElsewhereAsksGitHubNothing(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	leases := deliveryTestLeases(t, pool)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	reconcile := NewReconcile(pool, fake.newTestClient(), events.NewBroker(), unrenamed())

	// 0: this caller stands for the other process's pass while it is inside it, which is all
	// this row is about, and records no pass of its own -- so what the passes below do is the
	// lease's doing alone and not the reconcile interval's.
	taken, release := make(chan struct{}), make(chan struct{})
	held := make(chan bool, 1)
	go func() {
		held <- duty.Exclusive(ctx, nil, leases, deliveryReconcileLease, time.Time{}, 0, func(context.Context) {
			close(taken)
			<-release
		})
	}()
	select {
	case <-taken:
	case <-time.After(20 * time.Second):
		t.Fatal("the other caller never took the delivery-reconcile lease")
	}

	reconcile.leasedPass(ctx, leases, time.Now())
	if mints := fake.tokenMints.Load(); mints != 0 {
		t.Fatalf("the pass minted %d installation tokens under a lease held elsewhere; want none", mints)
	}
	settings, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("read the settings after the skipped pass: %v", err)
	}
	if settings.LastReconcileAt != nil || settings.LastError != nil {
		t.Fatalf("the skipped pass recorded last_reconcile_at=%v last_error=%v; want neither",
			settings.LastReconcileAt, settings.LastError)
	}
	close(release)
	if !<-held {
		t.Fatal("the other caller never held the lease, so the pass was free to reconcile")
	}

	// The lease is the pass's, not the process's: the next pass takes it and reconciles.
	reconcile.leasedPass(ctx, leases, time.Now())
	settings, err = GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("read the settings after the pass that ran: %v", err)
	}
	if settings.LastReconcileAt == nil && settings.LastError == nil {
		t.Fatal("the pass with the lease free recorded neither a reconcile nor a failure: it did not run")
	}
}

// The reconcile's shortest wait between two ticks is ReconcileInterval less a tenth of it, and a
// nanosecond more (duty.RunJittered draws each wait in (ReconcileInterval - ReconcileInterval/10,
// ReconcileInterval]). Two ticks of a lone process that far apart must both run: a tick refused as
// a period the process's own last pass served is a pass lost, and the next comes almost two
// intervals after the last. The first tick here is due at the start of a ReconcileInterval-aligned
// bucket, so the second is due inside that same five-minute bucket, which a lease whose period
// were ReconcileInterval would find served. The gap is worked out from ReconcileInterval alone,
// not from the period the loop hands the lease, so this test reads the same against a build that
// handed it ReconcileInterval and shows the refusal there.
//
// The delivery settings are saved first, since a pass with none and no head rows asks GitHub
// nothing. With them, the pass's first request is its permission check, the lookup of the deploy
// repository's installation, which the fake counts here.
func TestTwoReconcileTicksTheShortestWaitApartBothRun(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	leases := deliveryTestLeases(t, pool)
	settings := seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	var checks atomic.Int64
	fake.handle("GET /repos/"+settings.DeployRepo+"/installation", func(w http.ResponseWriter, _ *http.Request) {
		checks.Add(1)
		mustEncode(t, w, map[string]any{"id": fake.installID, "app_slug": "delivery-test", "permissions": fake.permissions})
	})
	reconcile := NewReconcile(pool, fake.newTestClient(), events.NewBroker(), unrenamed())

	interval := ReconcileInterval.Nanoseconds()
	first := time.Unix(0, time.Now().UnixNano()/interval*interval)
	second := first.Add(ReconcileInterval - ReconcileInterval/10 + 1)

	reconcile.leasedPass(ctx, leases, first)
	afterFirst := checks.Load()
	if afterFirst == 0 {
		t.Fatal("the first tick asked GitHub for no permission check: it did not run")
	}
	reconcile.leasedPass(ctx, leases, second)
	if afterSecond := checks.Load(); afterSecond == afterFirst {
		t.Fatalf("the second tick, %v after the first, asked GitHub for no permission check: it did not run, when it is due more than a lease period after the first, in a period no pass has served", second.Sub(first))
	}
}
