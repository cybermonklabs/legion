// The role claim copy's repair as the listener reports it and as its session routes show it:
// /healthz and /metrics name when the role claim watcher's last whole read of the role bucket
// completed and what it put back and took out, /healthz answers 200 "degraded" once that read is
// past its bound, and a claim the copy lost is back in GET /v1/sessions and GET
// /v1/registry/<session id> once the scheduled read has run. Every test drives the real handlers
// over the grant guard's rig (newGrantGuardRig): a registry on a clock the test moves whose role
// claim watcher keeps its scheduled read for the test to fire, so no whole read completes that a
// test did not ask for.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/store"
)

// roleClaimRepairGauges are the three gauges of the role claim copy's repair (registerListGauges).
var roleClaimRepairGauges = []string{
	"envoy_role_claim_repair_completed_timestamp_seconds",
	"envoy_role_claim_repair_put_back",
	"envoy_role_claim_repair_took_out",
}

// newHealthzRepairRig is newGrantGuardRig with what /healthz reads beyond the stores: the listener's
// durable consumer bound to the rig's client, whose lag the answer reports, and the caches it pings.
func newHealthzRepairRig(t *testing.T, consumer string) *grantGuardRig {
	t.Helper()
	rig := newGrantGuardRig(t)
	bindBoundDurable(t, rig.client, consumer, func(msg *natsgo.Msg) { _ = msg.Ack() })
	// The durable is the listener's own and outlives its subscription, on a server the package's
	// tests share, so the test deletes it rather than leave it holding the stream's messages.
	t.Cleanup(func() { _ = rig.client.JS().DeleteConsumer(bus.Stream, consumer) })
	rig.deps.caches = listenerCaches(rig.deps.registry, rig.deps.sessions, nil)
	rig.deps.consumer = consumer
	return rig
}

// listenerReports is /healthz and /metrics over one listener's deps as main mounts them: the
// handler and the list gauges (registerListGauges) each read the deps through a published pointer.
type listenerReports struct {
	healthz http.Handler
	metrics http.Handler
}

// reportsOver is listenerReports over d.
func reportsOver(d *listenerDeps) listenerReports {
	var served atomic.Pointer[listenerDeps]
	served.Store(d)
	met := metrics.New()
	registerListGauges(met, &served)
	return listenerReports{healthz: healthzHandler(&served), metrics: met.Handler()}
}

// healthzAnswer is one /healthz answer: its status code and its body's fields.
type healthzAnswer struct {
	code int
	body map[string]any
}

// health is one /healthz answer, logged with when it was asked.
func (r listenerReports) health(t *testing.T, when string) healthzAnswer {
	t.Helper()
	recorder := httptest.NewRecorder()
	r.healthz.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	t.Logf("%s: /healthz answered %d %s", when, recorder.Code, strings.TrimSpace(recorder.Body.String()))
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the /healthz answer %s: %v", recorder.Body.String(), err)
	}
	return healthzAnswer{code: recorder.Code, body: body}
}

// requireHealthy fails the test unless answer is 200 "healthy" with no error.
func requireHealthy(t *testing.T, answer healthzAnswer, why string) {
	t.Helper()
	if _, failed := answer.body["error"]; answer.code != http.StatusOK || answer.body["status"] != "healthy" || failed {
		t.Fatalf("/healthz = %d %v, want 200 healthy with no error: %s", answer.code, answer.body, why)
	}
}

// repairField is the role_claim_repair field a /healthz answer must carry, as a JSON decoder reads
// it. A zero completedAt is a listener that has completed no whole read, whose time and age are
// null.
func repairField(completedAt time.Time, age time.Duration, putBack, tookOut int) map[string]any {
	field := map[string]any{
		"completed_at":  nil,
		"age_seconds":   nil,
		"bound_seconds": float64(120),
		"put_back":      float64(putBack),
		"took_out":      float64(tookOut),
	}
	if !completedAt.IsZero() {
		field["completed_at"] = completedAt.UTC().Format(time.RFC3339Nano)
		field["age_seconds"] = age.Seconds()
	}
	return field
}

// requireRepairField fails the test unless answer's role_claim_repair field is exactly want, and
// its completion time, where it has one, is RFC 3339 in UTC.
func requireRepairField(t *testing.T, answer healthzAnswer, want map[string]any) {
	t.Helper()
	got, ok := answer.body["role_claim_repair"].(map[string]any)
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("/healthz's role_claim_repair = %#v, want %#v", answer.body["role_claim_repair"], want)
	}
	if at, ok := got["completed_at"].(string); ok {
		if _, err := time.Parse(time.RFC3339, at); err != nil || !strings.HasSuffix(at, "Z") {
			t.Fatalf("/healthz's role_claim_repair.completed_at %q is not RFC 3339 in UTC (%v)", at, err)
		}
	}
}

// requireRepairSamples fails the test unless /metrics has each repair gauge's sample: completedAt
// as a Unix time, and the two counts.
func (r listenerReports) requireRepairSamples(t *testing.T, completedAt time.Time, putBack, tookOut int) {
	t.Helper()
	want := []int64{completedAt.Unix(), int64(putBack), int64(tookOut)}
	for i, gauge := range roleClaimRepairGauges {
		got, ok := gaugeSample(t, r.metrics, gauge)
		if !ok || got != want[i] {
			t.Fatalf("/metrics %s = %d (sample present: %v), want %d", gauge, got, ok, want[i])
		}
		t.Logf("/metrics %s %d", gauge, got)
	}
}

// requireNoRepairSamples fails the test when /metrics has a sample of any repair gauge.
func (r listenerReports) requireNoRepairSamples(t *testing.T) {
	t.Helper()
	for _, gauge := range roleClaimRepairGauges {
		if got, ok := gaugeSample(t, r.metrics, gauge); ok {
			t.Fatalf("/metrics has %s %d before any whole read of the role claims completed, want no sample", gauge, got)
		}
	}
}

// loseRoleFromCopy takes role out of registry's copy of the role bucket while the bucket goes on
// holding its claim, as a delivery the copy missed leaves it, and leaves the copy's fence where it
// was (kvwatch.Copy.Remove), so the next whole read of the bucket puts the claim back.
// dropRoleFromCopy's drift is not that one: it fences the role at a revision past any the bucket
// reaches, so no read repairs it, which the acts' tests need and these must not have.
func loseRoleFromCopy(t *testing.T, registry *store.Registry, role string) {
	t.Helper()
	guard, roleCopy, _ := roleClaimCopy(t, registry)
	guard.Lock()
	defer guard.Unlock()
	roleCopy.Remove(role)
}

// c-repair-bound, the listener's half. /healthz carries the role claim copy's last whole read -
// when it completed, how long ago on the registry's clock, the bound, and what it put back and took
// out - and /metrics carries the same as three gauges with no sample before a read has completed.
// With the clock moved past the bound and no read since, /healthz answers 200 "degraded" naming the
// repair, every other field kept and never 503; the scheduled read fired, it is healthy again, and
// the report counts the claim the copy lost as put back and the two it held for no claim as taken
// out.
func TestHealthzReportsTheRoleClaimRescanAndIsDegradedPastItsBound(t *testing.T) {
	const (
		consumer = "listener-healthz-role-claim-repair"
		holder   = "ses_healthz_role_claim_repair"
		lost     = "healthz-repair-lost"
	)
	surplus := []string{"healthz-repair-surplus-a", "healthz-repair-surplus-b"}
	rig := newHealthzRepairRig(t, consumer)

	// A listener whose copy has completed no whole read: the view refuses, which /healthz names,
	// and the repair's field names no read.
	restarted := unreadRegistry(t, rig.client, rig.clock)
	unread := reportsOver(&listenerDeps{client: rig.client, registry: restarted, sessions: rig.deps.sessions,
		caches: listenerCaches(restarted, rig.deps.sessions, nil), consumer: consumer})
	answer := unread.health(t, "no whole read of the role claims completed")
	if reported, _ := answer.body["error"].(string); answer.code != http.StatusOK || answer.body["status"] != "degraded" ||
		!strings.HasPrefix(reported, "role claim view unavailable: ") {
		t.Fatalf("/healthz of a listener that completed no whole read = %d %v, want 200 degraded naming the role claim view", answer.code, answer.body)
	}
	requireRepairField(t, answer, repairField(time.Time{}, 0, 0, 0))
	unread.requireNoRepairSamples(t)

	reports := reportsOver(rig.deps)
	rig.seed(t, holder, lost)
	started := rig.deps.registry.RoleClaimRepair()
	answer = reports.health(t, "after the start's whole read")
	requireHealthy(t, answer, "the start's read completed and nothing is late")
	requireRepairField(t, answer, repairField(started.CompletedAt, rig.clock.Now().Sub(started.CompletedAt), 0, 0))
	reports.requireRepairSamples(t, started.CompletedAt, 0, 0)

	// The copy drifts both ways, and the clock moves past the bound with no read since.
	loseRoleFromCopy(t, rig.deps.registry, lost)
	requireDrift(t, rig.deps.registry, lost, "", holder)
	for _, role := range surplus {
		holdRoleInCopy(t, rig.deps.registry, role, holder)
		requireDrift(t, rig.deps.registry, role, holder, "")
	}
	late := store.RoleClaimRepairBound + time.Second
	rig.clock.Advance(started.CompletedAt.Add(late).Sub(rig.clock.Now()))
	repairErr := rig.deps.registry.RoleClaimRepairErr()
	if repairErr == nil {
		t.Fatalf("RoleClaimRepairErr is nil %s after the last whole read, past the %s bound", late, store.RoleClaimRepairBound)
	}
	answer = reports.health(t, "past the bound with no whole read since")
	if answer.code != http.StatusOK || answer.body["status"] != "degraded" {
		t.Fatalf("/healthz past the repair bound = %d %v, want 200 degraded: a late repair is never unhealthy", answer.code, answer.body)
	}
	if reported, want := answer.body["error"], dependencyUnavailable("role claim repair", repairErr); reported != want ||
		!strings.HasPrefix(want, "role claim repair unavailable: ") || !strings.Contains(want, "past the 2m0s bound") {
		t.Fatalf("/healthz past the repair bound reports the error %q, want %q, naming the repair and its bound", reported, want)
	}
	for _, field := range []string{"session_cache_ready", "session_cache_size", "session_watch_error", "session_max_age", "ci_cache", "num_pending", "num_ack_pending"} {
		if _, ok := answer.body[field]; !ok {
			t.Errorf("the degraded answer %v has no %s; a reader of that field loses it while the repair is late", answer.body, field)
		}
	}
	requireRepairField(t, answer, repairField(started.CompletedAt, late, 0, 0))
	reports.requireRepairSamples(t, started.CompletedAt, 0, 0)

	repaired := rig.repair(t)
	if !repaired.CompletedAt.Equal(rig.clock.Now()) || repaired.PutBack != 1 || repaired.TookOut != len(surplus) {
		t.Fatalf("the scheduled read's report = %+v, want it completed at %s with 1 claim put back and %d taken out",
			repaired, rig.clock.Now(), len(surplus))
	}
	answer = reports.health(t, "after the scheduled read")
	requireHealthy(t, answer, "the scheduled read completed on the registry's clock just now")
	requireRepairField(t, answer, repairField(repaired.CompletedAt, 0, 1, len(surplus)))
	reports.requireRepairSamples(t, repaired.CompletedAt, 1, len(surplus))
}

// c-lost-claim, the listener's half. A claim the bucket holds and the copy lost is in neither the
// holder's row of GET /v1/sessions nor its GET /v1/registry/<session id>, which both read the copy;
// no registration runs, since its renewal would write the claim and so put it back in the copy;
// and once the scheduled read - armed inside the bound - has run, both name the role again. The
// claim record is never written in between, so the read is what put it back.
func TestARoleCopyLostClaimIsBackInTheSessionsRowsAndTheRegistryWithinTheBound(t *testing.T) {
	const (
		holder = "ses_role_copy_lost_rows"
		role   = "role-copy-lost-rows"
	)
	rig := newGrantGuardRig(t)
	rig.seed(t, holder, role)
	rolesNamed := func(when string) (listed, entry []string) {
		t.Helper()
		row, ok := sessionRows(t, rig.deps.registry, rig.deps)[holder]
		if !ok {
			t.Fatalf("%s: GET /v1/sessions lists no row for %s", when, holder)
		}
		answer := sessionEntryRoute(t, rig.deps, holder)
		if answer.Code != http.StatusOK {
			t.Fatalf("%s: GET /v1/registry/%s: status %d %s, want 200", when, holder, answer.Code, answer.Body.String())
		}
		entry = decodeSessionInfo(t, answer).Roles
		t.Logf("%s: GET /v1/sessions gives %s the roles %v, GET /v1/registry/%s %v", when, holder, row.Roles, holder, entry)
		return row.Roles, entry
	}
	if listed, entry := rolesNamed("after the claim"); !slices.Equal(listed, []string{role}) || !slices.Equal(entry, []string{role}) {
		t.Fatalf("control: after the claim the routes give %s the roles %v and %v, want [%s] in both", holder, listed, entry, role)
	}
	claim := rig.storedClaim(t, role)

	lostAt := rig.clock.Now()
	loseRoleFromCopy(t, rig.deps.registry, role)
	requireDrift(t, rig.deps.registry, role, "", holder)
	if listed, entry := rolesNamed("with the claim lost from the copy"); len(listed) != 0 || len(entry) != 0 {
		t.Fatalf("with the claim lost from the copy the routes give %s the roles %v and %v, want none: the copy still holds the claim", holder, listed, entry)
	}

	wait := rig.scheduled(t)
	if wait.delay > store.RoleClaimRepairBound {
		t.Fatalf("the role claim watcher armed its scheduled read %s after its last read, past the repair bound of %s", wait.delay, store.RoleClaimRepairBound)
	}
	rig.clock.Advance(wait.delay)
	repaired := rig.repair(t)

	if listed, entry := rolesNamed("after the scheduled read"); !slices.Equal(listed, []string{role}) || !slices.Equal(entry, []string{role}) {
		t.Fatalf("after the scheduled read the routes give %s the roles %v and %v, want [%s] in both: the read did not put back the claim the copy lost",
			holder, listed, entry, role)
	}
	if repaired.PutBack != 1 || repaired.TookOut != 0 {
		t.Fatalf("the scheduled read's report = %+v, want one claim put back and none taken out", repaired)
	}
	if back := repaired.CompletedAt.Sub(lostAt); back > store.RoleClaimRepairBound {
		t.Fatalf("the claim the copy lost was back %s after the loss, past the repair bound of %s", back, store.RoleClaimRepairBound)
	}
	if after := rig.storedClaim(t, role); after.Revision != claim.Revision || after.RenewedAt != claim.RenewedAt {
		t.Fatalf("%s's claim moved from revision %d renewed at %d to revision %d renewed at %d: a write put it back, not the read",
			role, claim.Revision, claim.RenewedAt, after.Revision, after.RenewedAt)
	}
	t.Logf("the claim the copy lost was back %s after the loss, at claim revision %d", repaired.CompletedAt.Sub(lostAt), claim.Revision)
}
