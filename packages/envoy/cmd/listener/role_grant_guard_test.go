// The grant guard: POST /v1/roles/set does not hand a role held under the claimant's own session id
// to a second process while this listener cannot vouch that the holder stopped renewing it - its
// own renewal of that claim failed inside the live window, its role claim copy's last whole read
// is past the repair bound or none has completed, or the copy cannot be served
// (roleOwnSessionVerdict). Every case drives the real routes over a registry whose clock the test
// moves and whose role claim watcher keeps its scheduled read for the test to fire, so no whole
// read of the role claims completes that a case did not ask for.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// heldScheduledReads stands in for time.AfterFunc in a registry's role claim watcher
// (store.WithRoleClaimWatch, kvwatch.WithAfterFunc). It keeps every wait longer than a scan's idle
// timeout - the scheduled read of the role claims, and the minute a stop nobody asked for waits
// before its ERROR - for the test to fire or leave unfired, and hands every shorter one (the
// stream probe, a re-read, a scan's idle wait) to time.AfterFunc, so the watcher follows its
// bucket as it does in production and completes no whole read the test did not ask for.
type heldScheduledReads struct {
	mu   sync.Mutex
	kept []*heldScheduledRead
}

// heldScheduledRead is one wait a heldScheduledReads kept.
type heldScheduledRead struct {
	delay   time.Duration
	fire    func()
	stopped atomic.Bool
}

func (r *heldScheduledRead) Stop() bool { return !r.stopped.Swap(true) }

// after is the AfterFunc the watcher arms its waits through.
func (h *heldScheduledReads) after(d time.Duration, f func()) kvwatch.Timer {
	if d <= kvwatch.ScanIdleTimeout {
		return time.AfterFunc(d, f)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	wait := &heldScheduledRead{delay: d, fire: f}
	h.kept = append(h.kept, wait)
	return wait
}

// armed is the kept wait the watcher armed last and has not stopped, and nil when there is none.
func (h *heldScheduledReads) armed() *heldScheduledRead {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.kept) - 1; i >= 0; i-- {
		if !h.kept[i].stopped.Load() {
			return h.kept[i]
		}
	}
	return nil
}

// grantGuardRig is one listener's registration and role routes over one account: a registry on a
// clock the test moves (store.WithClock), whose role claim watcher measures on that clock too
// (kvwatch.WithNow) and keeps its scheduled read for the test (heldScheduledReads), the session
// registry the route reads its live window from, and the routes' shared log.
type grantGuardRig struct {
	client    *bus.Client
	clock     *listenerClock
	reads     *heldScheduledReads
	deps      *listenerDeps
	logs      *cmdtest.LockedBuffer
	set       http.Handler
	subscribe http.Handler
}

// newGrantGuardRig opens the rig and returns once the start's read of the role claims has
// completed and armed the scheduled read, which the rig then holds: from here on a whole read
// completes only when a case fires it.
func newGrantGuardRig(t *testing.T) *grantGuardRig {
	t.Helper()
	client := setupPublishTestClient(t)
	clock := &listenerClock{now: time.Now()}
	reads := &heldScheduledReads{}
	registry, err := store.Open(client.Conn, store.WithReplicas(1), store.WithClock(clock.Now),
		store.WithRoleClaimWatch(kvwatch.WithAfterFunc(reads.after), kvwatch.WithNow(clock.Now)))
	if err != nil {
		t.Fatalf("open the listener's registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	waitForRegistry(t, registry)
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	waitForSessions(t, sessions)
	deps := withRoleClaimDrain(t, &listenerDeps{client: client, registry: registry, sessions: sessions, consumers: setupSessionConsumers(t)})
	logs := &cmdtest.LockedBuffer{}
	logger := logging.NewWithWriter("test", logs)
	rig := &grantGuardRig{
		client:    client,
		clock:     clock,
		reads:     reads,
		deps:      deps,
		logs:      logs,
		set:       roleSetHandler(deps, "test-machine", logger),
		subscribe: subscribeHandler(deps, "test-machine", logger),
	}
	rig.scheduled(t)
	return rig
}

// scheduled is the scheduled read the role claim watcher armed last and has not stopped. It is
// waited for: a read records its completion a moment before the watcher arms the next.
func (r *grantGuardRig) scheduled(t *testing.T) *heldScheduledRead {
	t.Helper()
	var wait *heldScheduledRead
	waitFor(t, 10*time.Second, "the role claim watcher to arm its scheduled read", func() bool {
		wait = r.reads.armed()
		return wait != nil
	})
	return wait
}

// repair fires the scheduled read the watcher holds and returns once the whole read it starts has
// completed and armed the next, with the report of that read.
func (r *grantGuardRig) repair(t *testing.T) store.RoleClaimRepairReport {
	t.Helper()
	fired := r.scheduled(t)
	before := r.deps.registry.RoleClaimRepair()
	fired.fire()
	waitFor(t, 10*time.Second, "the fired scheduled read of the role claims to complete", func() bool {
		next := r.reads.armed()
		return next != nil && next != fired && r.deps.registry.RoleClaimRepair().Completed > before.Completed
	})
	return r.deps.registry.RoleClaimRepair()
}

// register is the heartbeat POST /v1/interests/subscribe a running client repeats, stating the
// process making it and asking for the session's own consumer. It fails the test unless the
// registration was answered 200: no renewal of any outcome may cost a session its registration.
func (r *grantGuardRig) register(t *testing.T, sessionID, instance string) {
	t.Helper()
	body := fmt.Sprintf(`{"session_id":%q,"topics":["notifications.team.heartbeat"],"delivery":{"consumer":true,"epoch":"e"},"client_instance_id":%q}`, sessionID, instance)
	recorder := httptest.NewRecorder()
	r.subscribe.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("registration of %s by process %q: status %d %s, want 200", sessionID, instance, recorder.Code, recorder.Body.String())
	}
}

// claimThrough is a soft claim of role under sessionID by the process instance names, made through
// the role route set and logged with what it answered.
func claimThrough(t *testing.T, set http.Handler, sessionID, role, instance string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true,"client_instance_id":%q}`, sessionID, role, instance)
	recorder := httptest.NewRecorder()
	set.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set", strings.NewReader(body)))
	t.Logf("POST /v1/roles/set of %s under %s by process %s answered %d (Retry-After %q): %s",
		role, sessionID, instance, recorder.Code, recorder.Header().Get("Retry-After"), strings.TrimSpace(recorder.Body.String()))
	return recorder
}

// claim is claimThrough the rig's own role route.
func (r *grantGuardRig) claim(t *testing.T, sessionID, role, instance string) *httptest.ResponseRecorder {
	t.Helper()
	return claimThrough(t, r.set, sessionID, role, instance)
}

// seed registers holder as holdingProcess and has that process claim role, which writes the role's
// lane topic onto holder's interest record beside the claim, and waits for the role claim copy to
// name holder. It returns the claim as the bucket holds it.
func (r *grantGuardRig) seed(t *testing.T, holder, role string) store.RoleClaim {
	t.Helper()
	r.register(t, holder, holdingProcess)
	if recorder := r.claim(t, holder, role, holdingProcess); recorder.Code != http.StatusOK {
		t.Fatalf("the seed claim of %s by %s: status %d %s, want 200", role, holdingProcess, recorder.Code, recorder.Body.String())
	}
	waitFor(t, 10*time.Second, fmt.Sprintf("%s's claim to reach the role claim copy", role), func() bool {
		held, err := r.deps.registry.RolesHeldBy(holder)
		return err == nil && slices.Contains(held, role)
	})
	return r.storedClaim(t, role)
}

// storedClaim is role's claim as the bucket holds it now.
func (r *grantGuardRig) storedClaim(t *testing.T, role string) store.RoleClaim {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	claim, err := r.deps.registry.RoleClaim(ctx, role)
	if err != nil {
		t.Fatalf("read %s's claim: %v", role, err)
	}
	return claim
}

// liveWindow is the live window the role route weighs a claim's renewal against
// (roleClaimLiveWindow).
func (r *grantGuardRig) liveWindow() time.Duration { return roleClaimLiveWindow(r.deps) }

// failRenewal has holder's process register while every read of the role bucket's records fails,
// so the registration's renewal of role's claim fails and is recorded, then lets the reads answer
// again, so the route's own read of the claim answers. It returns the failure recorded.
func (r *grantGuardRig) failRenewal(t *testing.T, holder, role string) store.RenewalFailure {
	t.Helper()
	answering := registryRoleKV(t, r.deps.registry)
	failed := failRoleClaimReads(t, r.deps.registry)
	r.register(t, holder, holdingProcess)
	setRegistryRoleKV(t, r.deps.registry, answering)
	if failed.Load() == 0 {
		t.Fatalf("the registration read no claim record, so no renewal of %s failed", role)
	}
	failure, recorded := r.deps.registry.RenewalFailedWithin(role, holder, r.liveWindow())
	if !recorded || !errors.Is(failure.Err, errRoleBucketReadFailed) || !strings.Contains(failure.Err.Error(), role) {
		t.Fatalf("after the renewal's claim read failed the record of %s's renewal for %s is %+v (recorded %v), want the read's failure naming the role",
			role, holder, failure, recorded)
	}
	t.Logf("the renewal of %s's claim for %s failed and was recorded at %s: %v", role, holder, failure.At.Format(time.RFC3339Nano), failure.Err)
	return failure
}

// requireGuardState fails the test unless registry is in the state a case is about: the role claim
// view answering or not (viewServes), the repair inside its bound or not (repairInBound), and a
// renewal failure of role's claim for holder recorded inside the live window or not (failed).
func requireGuardState(t *testing.T, registry *store.Registry, role, holder string, window time.Duration, viewServes, repairInBound, failed bool) {
	t.Helper()
	viewErr, repairErr := registry.RoleViewErr(), registry.RoleClaimRepairErr()
	failure, recorded := registry.RenewalFailedWithin(role, holder, window)
	t.Logf("before the claim: RoleViewErr %v, RoleClaimRepairErr %v, renewal failure recorded %v (%v), repair report %+v",
		viewErr, repairErr, recorded, failure.Err, registry.RoleClaimRepair())
	if (viewErr == nil) != viewServes || (repairErr == nil) != repairInBound || recorded != failed {
		t.Fatalf("the case's state is RoleViewErr %v, RoleClaimRepairErr %v and a recorded renewal failure %v; want the view serving %v, the repair inside its bound %v and a failure recorded %v",
			viewErr, repairErr, recorded, viewServes, repairInBound, failed)
	}
}

// requireGuardRefusal fails the test unless recorder is the bus-unavailable 503 with Retry-After: 1
// the guard answers, with an error that names arm.
func requireGuardRefusal(t *testing.T, recorder *httptest.ResponseRecorder, arm string) {
	t.Helper()
	var answer apiError
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" ||
		!strings.HasPrefix(answer.Error, busUnansweredPrefix) || !strings.Contains(answer.Error, arm) {
		t.Fatalf("the claim answered %d, Retry-After %q, %q; want the 503 with Retry-After: 1 opening %q and naming %q",
			recorder.Code, recorder.Header().Get("Retry-After"), answer.Error, busUnansweredPrefix, arm)
	}
}

// requireHeldBy fails the test unless role's claim names the process instance.
func (r *grantGuardRig) requireHeldBy(t *testing.T, role, instance, why string) {
	t.Helper()
	if got := r.storedClaim(t, role).ClientInstanceID; got != instance {
		t.Fatalf("the stored claim of %s names the process %q, want %q: %s", role, got, instance, why)
	}
}

// unreadRegistry opens another listener's registry over client's account and stops its role claim
// watcher before that watcher's first read of the bucket completes, as a listener restart leaves
// it for as long as that read takes: the view refuses (refuseRoleClaimView's refilling watcher
// stands in its place) and no whole read of the role claims has completed. Open returns as soon as
// the watcher has started, and its first read needs several round trips to the server before it
// can complete, so the stop lands first; an open whose read completed first all the same is
// stopped and opened again.
func unreadRegistry(t *testing.T, client *bus.Client, clock *listenerClock) *store.Registry {
	t.Helper()
	const opens = 10
	for range opens {
		registry, err := store.Open(client.Conn, store.WithReplicas(1), store.WithClock(clock.Now))
		if err != nil {
			t.Fatalf("open the restarted listener's registry: %v", err)
		}
		t.Cleanup(registry.StopWatch)
		refuseRoleClaimView(t, registry)
		if registry.RoleClaimRepair().Completed == 0 {
			return registry
		}
	}
	t.Fatalf("each of %d registries completed its first read of the role claims before its watcher was stopped", opens)
	return nil
}

// c-grant-guard. A role held under the claimant's own session id, whose holder's renewal is older
// than the live window, is not given to a second process of that session while this listener
// cannot vouch that the holder stopped renewing it. Each case asks once, from secondProcess, and
// the claim record reads cleanly in every case, so the route's own read of it never refuses:
//
//  1. the holder's renewal's claim read failed and was recorded: refused, naming the failure;
//  2. the copy's last whole read is older than the repair bound: refused, naming the repair;
//  3. a listener that has completed no whole read at all: refused, with the view's refusal, which
//     a just-started listener's copy gives before anything else;
//  4. case one's failure, then a registration whose renewal lands before the grant is asked: the
//     record clears, and with the holder stopped again the role is given away;
//  5. the view cannot be served while every read works: refused, with the view's refusal;
//  6. no failure, a whole read inside the bound and a servable view: the role is given away.
//
// The test logs every answer and the state each case asked in.
func TestTheGrantGuardRefusesWhileThisListenersRenewalsFailOrItsCopyIsStale(t *testing.T) {
	const holder = "ses_grant_guard"

	t.Run("1 a renewal of the holder's claim failed on this listener", func(t *testing.T) {
		const role = "guard-renewal-failed"
		rig := newGrantGuardRig(t)
		rig.seed(t, holder, role)
		rig.failRenewal(t, holder, role)
		ageRoleClaim(t, rig.deps, role, 2*rig.liveWindow())
		requireGuardState(t, rig.deps.registry, role, holder, rig.liveWindow(), true, true, true)

		recorder := rig.claim(t, holder, role, secondProcess)

		requireGuardRefusal(t, recorder, fmt.Sprintf("role %s's renewal for %s failed on this listener", role, holder))
		if !strings.Contains(recorder.Body.String(), errRoleBucketReadFailed.Error()) {
			t.Fatalf("the refusal %s does not quote the renewal's failure %q", recorder.Body.String(), errRoleBucketReadFailed)
		}
		rig.requireHeldBy(t, role, holdingProcess, "a claim refused writes nothing")
	})

	t.Run("2 the last whole read of the role claims is older than the bound", func(t *testing.T) {
		const role = "guard-repair-late"
		rig := newGrantGuardRig(t)
		rig.seed(t, holder, role)
		ageRoleClaim(t, rig.deps, role, 2*rig.liveWindow())
		completed := rig.deps.registry.RoleClaimRepair().CompletedAt
		rig.clock.Advance(completed.Add(store.RoleClaimRepairBound + time.Second).Sub(rig.clock.Now()))
		requireGuardState(t, rig.deps.registry, role, holder, rig.liveWindow(), true, false, false)

		recorder := rig.claim(t, holder, role, secondProcess)

		requireGuardRefusal(t, recorder, "role claim repair: the last whole read of the role claims completed")
		rig.requireHeldBy(t, role, holdingProcess, "a claim refused writes nothing")
	})

	t.Run("3 no whole read of the role claims has completed", func(t *testing.T) {
		const role = "guard-never-read"
		rig := newGrantGuardRig(t)
		rig.seed(t, holder, role)
		ageRoleClaim(t, rig.deps, role, 2*rig.liveWindow())
		restarted := unreadRegistry(t, rig.client, rig.clock)
		set := roleSetHandler(withRoleClaimDrain(t, &listenerDeps{client: rig.client, registry: restarted, sessions: rig.deps.sessions}),
			"test-machine", logging.NewWithWriter("test", rig.logs))
		if err := restarted.RoleClaimRepairErr(); err == nil || err.Error() != "no whole read of the role claims has completed" {
			t.Fatalf("the restarted listener's RoleClaimRepairErr is %v, want that no whole read has completed", err)
		}
		requireGuardState(t, restarted, role, holder, rig.liveWindow(), false, false, false)

		recorder := claimThrough(t, set, holder, role, secondProcess)

		if !roleViewUnanswered(recorder) {
			t.Fatalf("a second process's claim on a listener that has read no role claims: status %d, Retry-After %q, body %s; want the bus-unavailable 503 naming the role claim view's refusal",
				recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
		}
		if report := restarted.RoleClaimRepair(); report.Completed != 0 {
			t.Fatalf("the restarted listener's repair report is %+v after the claim, want no read completed: the case did not hold", report)
		}
		rig.requireHeldBy(t, role, holdingProcess, "a claim refused writes nothing")
	})

	t.Run("4 a renewal that lands clears the failure", func(t *testing.T) {
		const role = "guard-failure-cleared"
		rig := newGrantGuardRig(t)
		rig.seed(t, holder, role)
		failure := rig.failRenewal(t, holder, role)

		rig.register(t, holder, holdingProcess)

		renewed := rig.storedClaim(t, role)
		t.Logf("the registration after the failure renewed %s's claim at %d (the registry's clock reads %d)", role, renewed.RenewedAt, rig.clock.Now().UnixMilli())
		if renewed.RenewedAt != rig.clock.Now().UnixMilli() {
			t.Fatalf("the registration left %s's claim renewed at %d, want it renewed at the registry's moment %d", role, renewed.RenewedAt, rig.clock.Now().UnixMilli())
		}
		if !rig.clock.Now().Before(failure.At.Add(rig.liveWindow())) {
			t.Fatalf("the failure recorded at %s is past the live window at %s: the case would not show the record cleared", failure.At, rig.clock.Now())
		}
		// The holder stops again after the renewal that landed, which is all that stands between
		// the second process and the role once the record no longer refuses it.
		ageRoleClaim(t, rig.deps, role, 2*rig.liveWindow())
		requireGuardState(t, rig.deps.registry, role, holder, rig.liveWindow(), true, true, false)

		recorder := rig.claim(t, holder, role, secondProcess)

		if recorder.Code != http.StatusOK {
			t.Fatalf("the second process's claim once the failed renewal was followed by one that landed: status %d %s, want 200", recorder.Code, recorder.Body.String())
		}
		rig.requireHeldBy(t, role, secondProcess, "the role is given to the claimant")
	})

	t.Run("5 the role claim view cannot be served", func(t *testing.T) {
		const role = "guard-view-refusing"
		rig := newGrantGuardRig(t)
		rig.seed(t, holder, role)
		ageRoleClaim(t, rig.deps, role, 2*rig.liveWindow())
		refuseRoleClaimView(t, rig.deps.registry)
		requireGuardState(t, rig.deps.registry, role, holder, rig.liveWindow(), false, true, false)

		recorder := rig.claim(t, holder, role, secondProcess)

		if !roleViewUnanswered(recorder) {
			t.Fatalf("a second process's claim while the role claim view cannot answer: status %d, Retry-After %q, body %s; want the bus-unavailable 503 naming the role claim view's refusal",
				recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
		}
		rig.requireHeldBy(t, role, holdingProcess, "a claim refused writes nothing")
	})

	t.Run("6 control: nothing the guard reads refuses", func(t *testing.T) {
		const role = "guard-control"
		rig := newGrantGuardRig(t)
		rig.seed(t, holder, role)
		ageRoleClaim(t, rig.deps, role, 2*rig.liveWindow())
		// The clock stands where case two's does, past the bound from the start's read, and a
		// scheduled read completes there: the read's age is all that told the two apart.
		completed := rig.deps.registry.RoleClaimRepair().CompletedAt
		rig.clock.Advance(completed.Add(store.RoleClaimRepairBound + time.Second).Sub(rig.clock.Now()))
		report := rig.repair(t)
		t.Logf("the scheduled read completed at %s: %+v", report.CompletedAt.Format(time.RFC3339Nano), report)
		requireGuardState(t, rig.deps.registry, role, holder, rig.liveWindow(), true, true, false)

		recorder := rig.claim(t, holder, role, secondProcess)

		if recorder.Code != http.StatusOK {
			t.Fatalf("the second process's claim of a role nobody renews, with nothing the guard reads refusing: status %d %s, want 200", recorder.Code, recorder.Body.String())
		}
		rig.requireHeldBy(t, role, secondProcess, "the role is given to the claimant")
	})
}
