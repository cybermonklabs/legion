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
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/kvwatch/kvwatchtest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// grantGuardRig is one listener's registration and role routes over one account: a registry on a
// clock the test moves (store.WithClock), whose role claim watcher measures on that clock too
// (kvwatch.WithNow) and keeps its scheduled read for the test (kvwatchtest.Waits), the session
// registry the route reads its live window from, and the routes' shared log.
type grantGuardRig struct {
	client    *bus.Client
	clock     *listenerClock
	reads     *kvwatchtest.Waits
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
	reads := &kvwatchtest.Waits{}
	registry, err := store.Open(client.Conn, store.WithReplicas(1), store.WithClock(clock.Now),
		store.WithRoleClaimWatch(kvwatch.WithAfterFunc(reads.After), kvwatch.WithNow(clock.Now)))
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
func (r *grantGuardRig) scheduled(t *testing.T) *kvwatchtest.Wait {
	t.Helper()
	var wait *kvwatchtest.Wait
	waitFor(t, 10*time.Second, "the role claim watcher to arm its scheduled read", func() bool {
		wait = r.reads.Armed()
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
	fired.Fire()
	waitFor(t, 10*time.Second, "the fired scheduled read of the role claims to complete", func() bool {
		next := r.reads.Armed()
		return next != nil && next != fired && r.deps.registry.RoleClaimRepair().Completed > before.Completed
	})
	return r.deps.registry.RoleClaimRepair()
}

// registerThrough is the heartbeat POST /v1/interests/subscribe a running client repeats, made
// through the registration route subscribe, stating the process making it and asking for the
// session's own consumer, as every registration this build stores does. It fails the test unless
// the registration was answered 200: no renewal of any outcome may cost a session its
// registration.
func registerThrough(t *testing.T, subscribe http.Handler, sessionID, instance string) {
	t.Helper()
	body := fmt.Sprintf(`{"session_id":%q,"topics":["notifications.team.heartbeat"],"delivery":{"consumer":true,"epoch":"e"},"client_instance_id":%q}`, sessionID, instance)
	recorder := httptest.NewRecorder()
	subscribe.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("registration of %s by process %q: status %d %s, want 200", sessionID, instance, recorder.Code, recorder.Body.String())
	}
}

// register is registerThrough the rig's own registration route.
func (r *grantGuardRig) register(t *testing.T, sessionID, instance string) {
	t.Helper()
	registerThrough(t, r.subscribe, sessionID, instance)
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

// storedRoleClaim is role's claim as registry's role bucket holds it now.
func storedRoleClaim(t *testing.T, registry *store.Registry, role string) store.RoleClaim {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	claim, err := registry.RoleClaim(ctx, role)
	if err != nil {
		t.Fatalf("read %s's claim: %v", role, err)
	}
	return claim
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

// storedClaim is storedRoleClaim of the rig's own registry.
func (r *grantGuardRig) storedClaim(t *testing.T, role string) store.RoleClaim {
	t.Helper()
	return storedRoleClaim(t, r.deps.registry, role)
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
	viewErr, repairErr := registry.RoleViewErr(), registry.RoleClaimRepair().Err()
	failure, recorded := registry.RenewalFailedWithin(role, holder, window)
	t.Logf("before the claim: RoleViewErr %v, the repair error %v, renewal failure recorded %v (%v), repair report %+v",
		viewErr, repairErr, recorded, failure.Err, registry.RoleClaimRepair())
	if (viewErr == nil) != viewServes || (repairErr == nil) != repairInBound || recorded != failed {
		t.Fatalf("the case's state is RoleViewErr %v, the repair error %v and a recorded renewal failure %v; want the view serving %v, the repair inside its bound %v and a failure recorded %v",
			viewErr, repairErr, recorded, viewServes, repairInBound, failed)
	}
}

// roleHeldAnswer is the body of the 409 POST /v1/roles/set gives a soft claim it refused a held
// role, decoded as packages/envoy-client's setRole reads it (RoleHeldWireSchema): the error, the
// role and the session holding it. setRole answers it as a claim not granted naming that holder,
// which both harnesses wait out while the holder is their own session.
type roleHeldAnswer struct {
	Error  string `json:"error"`
	Role   string `json:"role"`
	Holder string `json:"holder"`
}

// requireGuardRefusal fails the test unless recorder is the grant guard's refusal of a claim of
// role under sessionID: the 409 a second process of a session gets while the holder still answers
// for its claim, with no Retry-After, whose body names role and, as its holder, sessionID itself -
// the claimant's own session - and whose error opens with grantGuardPrefix and goes straight on to
// name arm. It is not the bus-unavailable answer, since every read the guard rests on answered. It
// returns the body as the client decodes it.
func requireGuardRefusal(t *testing.T, recorder *httptest.ResponseRecorder, role, sessionID, arm string) roleHeldAnswer {
	t.Helper()
	var answer roleHeldAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	if strings.Contains(answer.Error, busUnansweredPrefix) {
		t.Fatalf("the claim's refusal reads %q: it says the bus did not answer, but every read the guard rests on answered", answer.Error)
	}
	want := grantGuardPrefix + ": " + arm
	if recorder.Code != http.StatusConflict || recorder.Header().Get("Retry-After") != "" ||
		answer.Role != role || answer.Holder != sessionID || !strings.HasPrefix(answer.Error, want) {
		t.Fatalf("the claim answered %d, Retry-After %q, %+v; want the 409 with no Retry-After naming %s and, as its holder, the claimant's own session %s, its error opening %q",
			recorder.Code, recorder.Header().Get("Retry-After"), answer, role, sessionID, want)
	}
	return answer
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
//  1. the holder's renewal's claim read failed and was recorded: refused with the 409 naming the
//     claimant's own session, under the guard's own error, naming the failure;
//  2. the copy's last whole read is older than the repair bound: refused with the same 409, under
//     the guard's own error, naming the repair;
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

		requireGuardRefusal(t, recorder, role, holder, fmt.Sprintf("role %s's renewal for %s failed on this listener", role, holder))
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

		requireGuardRefusal(t, recorder, role, holder, "role claim repair: the last whole read of the role claims completed")
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
		if err := restarted.RoleClaimRepair().Err(); err == nil || err.Error() != "no whole read of the role claims has completed" {
			t.Fatalf("the restarted listener's repair error is %v, want that no whole read has completed", err)
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

// guardRefusalLogged is the line the role route logged when the grant guard refused a claim,
// decoded, or the test fails when it logged none.
func (r *grantGuardRig) guardRefusalLogged(t *testing.T) map[string]any {
	t.Helper()
	for line := range strings.Lines(r.logs.String()) {
		if !strings.Contains(line, "the grant guard cannot vouch") {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("decode the logged refusal %q: %v", line, err)
		}
		return fields
	}
	t.Fatalf("the route logged no grant guard refusal:\n%s", r.logs.String())
	return nil
}

// answerFields is the names of the fields recorder's JSON body carries, sorted.
func answerFields(t *testing.T, recorder *httptest.ResponseRecorder) []string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &fields); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body.String(), err)
	}
	return slices.Sorted(maps.Keys(fields))
}

// A relaunched Legion pane is a process that resumes the session id of the one it replaced, with a
// client instance of its own, while that process's claim still stands. The grant guard's refusal
// of its claim is the answer it gets while the process before it still answers for that claim: a
// 409 whose body, decoded as the client decodes it, names the role and the claimant's own session
// as the holder, in the same three fields. So envoy-client's setRole reads it as a claim not
// granted to a holder that is this very session, which both harnesses wait out, and not as a
// failure that stops the pane. Only the error tells the two apart: the guard's opens with
// grantGuardPrefix and names the arm. The route logs the guard's refusal once, with the role, the
// session, both processes and the arm.
func TestTheGrantGuardAnswersARelaunchedProcessAsItsOwnSessionsStandingClaim(t *testing.T) {
	const (
		holder = "ses_relaunched_pane"
		role   = "guard-relaunched"
	)
	rig := newGrantGuardRig(t)
	rig.seed(t, holder, role)
	rig.failRenewal(t, holder, role)
	// The pane is relaunched: the new process registers under the session id, stating its own
	// instance, and claims the role the process before it holds.
	rig.register(t, holder, secondProcess)

	renewed := rig.claim(t, holder, role, secondProcess)
	var held roleHeldAnswer
	if err := json.Unmarshal(renewed.Body.Bytes(), &held); err != nil {
		t.Fatalf("decode the answer %s: %v", renewed.Body.String(), err)
	}
	if renewed.Code != http.StatusConflict || held.Role != role || held.Holder != holder {
		t.Fatalf("the relaunched process's claim while the claim is renewed: status %d %+v, want the 409 naming %s held by %s", renewed.Code, held, role, holder)
	}

	// The process before it stopped: nobody renews the claim, and this listener's own renewal of
	// it failed inside the live window, so the guard cannot vouch that it stopped.
	ageRoleClaim(t, rig.deps, role, 2*rig.liveWindow())
	requireGuardState(t, rig.deps.registry, role, holder, rig.liveWindow(), true, true, true)

	guarded := rig.claim(t, holder, role, secondProcess)

	arm := fmt.Sprintf("role %s's renewal for %s failed on this listener", role, holder)
	answer := requireGuardRefusal(t, guarded, role, holder, arm)
	t.Logf("the guard's 409 decodes to %+v; the refusal of the renewed claim to %+v", answer, held)
	if answer.Role != held.Role || answer.Holder != held.Holder {
		t.Fatalf("the guard's 409 names %s held by %s, the refusal of the renewed claim %s held by %s: the client must read the two alike",
			answer.Role, answer.Holder, held.Role, held.Holder)
	}
	if got, want := answerFields(t, guarded), answerFields(t, renewed); !slices.Equal(got, want) {
		t.Fatalf("the guard's 409 carries the fields %v, the refusal of the renewed claim %v: the client must read the two alike", got, want)
	}
	rig.requireHeldBy(t, role, holdingProcess, "a claim refused writes nothing")
	logged := rig.guardRefusalLogged(t)
	t.Logf("the route logged the refusal: %v", logged)
	for key, want := range map[string]string{
		"level":                     "INFO",
		"role":                      role,
		"session_id":                holder,
		"holder_client_instance_id": holdingProcess,
		"client_instance_id":        secondProcess,
	} {
		if logged[key] != want {
			t.Fatalf("the logged refusal's %s is %v, want %q: %v", key, logged[key], want, logged)
		}
	}
	if got, _ := logged["arm"].(string); !strings.HasPrefix(got, arm) {
		t.Fatalf("the logged refusal's arm is %q, want it to open %q", got, arm)
	}
}

// A relaunched process's own registrations neither record a renewal failure of the claim the
// process before it holds nor clear the one that process recorded: no renewal of a claim naming
// another process is this process's to make. So the renewal-failure arm refuses the relaunched
// process's claim, with the 409 naming its own session, until one live window after the stopped
// process's last failure, and grants it from then on. The relaunched process registers a heartbeat
// apart while the registry's clock moves, every read answering, and after each registration the
// record is still the stopped process's failure at the moment it was recorded; the scheduled read
// is fired at each step, so the repair's bound is never the arm that refuses.
func TestTheGrantGuardRefusesARelaunchedProcessOnlyUntilItsPredecessorsLastFailureAges(t *testing.T) {
	const (
		holder = "ses_relaunched_after_a_failure"
		role   = "guard-predecessor-failed"
	)
	rig := newGrantGuardRig(t)
	rig.seed(t, holder, role)
	failed := rig.failRenewal(t, holder, role).At
	// The process that holds the claim stops: nothing renews the claim from here on.
	ageRoleClaim(t, rig.deps, role, 2*rig.liveWindow())
	window := rig.liveWindow()
	ends := failed.Add(window)
	arm := fmt.Sprintf("role %s's renewal for %s failed on this listener at %s", role, holder, failed.UTC().Format(time.RFC3339))

	// registerAt moves the registry's clock to at, has the relaunched process register there and
	// fires the scheduled read, then fails the test unless the record of the failure is still the
	// stopped process's, recorded at failed, exactly when at is inside the window it opened.
	registerAt := func(at time.Time) {
		t.Helper()
		rig.clock.Advance(at.Sub(rig.clock.Now()))
		rig.register(t, holder, secondProcess)
		report := rig.repair(t)
		failure, recorded := rig.deps.registry.RenewalFailedWithin(role, holder, window)
		t.Logf("at %s (%s after the failure) the relaunched process registered; the scheduled read completed at %s; the renewal failure recorded: %v at %s",
			at.Format(time.RFC3339), at.Sub(failed), report.CompletedAt.Format(time.RFC3339), recorded, failure.At.Format(time.RFC3339Nano))
		if inside := at.Before(ends); recorded != inside || (recorded && !failure.At.Equal(failed)) {
			t.Fatalf("after the relaunched process registered at %s the record of %s's renewal for %s is %v at %s; want the stopped process's failure at %s recorded %v",
				at.Format(time.RFC3339), role, holder, recorded, failure.At.Format(time.RFC3339Nano), failed.Format(time.RFC3339Nano), inside)
		}
	}

	beats := 0
	for at := failed.Add(contracts.SessionHeartbeat); at.Before(ends); at = at.Add(contracts.SessionHeartbeat) {
		registerAt(at)
		beats++
		requireGuardState(t, rig.deps.registry, role, holder, window, true, true, true)

		recorder := rig.claim(t, holder, role, secondProcess)

		requireGuardRefusal(t, recorder, role, holder, arm)
		rig.requireHeldBy(t, role, holdingProcess, "a claim refused writes nothing")
	}
	if beats < 2 {
		t.Fatalf("the relaunched process registered %d times inside the %s window, want more than once", beats, window)
	}

	registerAt(ends.Add(time.Second))
	requireGuardState(t, rig.deps.registry, role, holder, window, true, true, false)

	recorder := rig.claim(t, holder, role, secondProcess)

	if recorder.Code != http.StatusOK {
		t.Fatalf("the relaunched process's claim one live window after the stopped process's last failure: status %d %s, want 200", recorder.Code, recorder.Body.String())
	}
	rig.requireHeldBy(t, role, secondProcess, "the role is given to the relaunched process")
}
