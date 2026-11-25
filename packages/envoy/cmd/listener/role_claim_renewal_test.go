// A registration is its process's heartbeat, and POST /v1/interests/subscribe renews every role
// claim that names that process (store.Registry.RenewRoleClaims). Every case here drives the real
// route against a real role bucket, so what it proves is what a heartbeat does to the claims a
// session holds, and what it never does to another process's.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/store"
)

// roleClaimRenewalRig is the registration route over one role bucket, with the bucket itself
// beside it: a case seeds the claim it is about as a record, as a build that wrote it before
// this one left it, and reads back what the registration made of it. deps is the route's own, for
// a case that changes its bounds before it registers (budgetsUntilTheDeadline).
type roleClaimRenewalRig struct {
	t         *testing.T
	deps      *listenerDeps
	registry  *store.Registry
	roleKV    natsgo.KeyValue
	logs      *bytes.Buffer
	subscribe http.Handler
}

func newRoleClaimRenewalRig(t *testing.T) *roleClaimRenewalRig {
	t.Helper()
	registry, sessions := setupSessionsTest(t, nil, nil)
	logs := &bytes.Buffer{}
	// withRoleClaimDrain, because a registration that makes its session live again hands that
	// session the kept lane of each role whose claim record it read for it (runKeptRoleDrain),
	// and the first registration of every holder here is one: the rig's session cache has never
	// seen it.
	deps := withRoleClaimDrain(t, &listenerDeps{client: setupPublishTestClient(t), registry: registry, sessions: sessions, consumers: setupSessionConsumers(t, nil)})
	return &roleClaimRenewalRig{
		t:         t,
		deps:      deps,
		registry:  registry,
		roleKV:    registryRoleKV(t, registry),
		logs:      logs,
		subscribe: subscribeHandler(deps, "test-machine", logging.NewWithWriter("test", logs)),
	}
}

// seed stores claim as role's record, leaves role's lane topic on its holder's interest record,
// and waits for the role claim view to give its holder that role.
//
// The renewal weighs the roles of the row the registration stored and the roles the view names,
// so a case that did not wait would be asking whether a renewal happened before the registration
// could know of the claim. The lane topic is on the record because a claim puts it there (POST
// /v1/roles/set writes the holder's topic), and the registration's write keeps it in the row it
// hands the renewal; a claim seeded without the record its claim would have left is a holder that
// has handed the role back, which is the case of its own below.
func (r *roleClaimRenewalRig) seed(role string, claim store.RoleClaim) store.RoleClaim {
	r.t.Helper()
	record, err := json.Marshal(claim)
	if err != nil {
		r.t.Fatalf("encode %s's claim: %v", role, err)
	}
	if _, err := r.roleKV.Put(role, record); err != nil {
		r.t.Fatalf("store %s's claim: %v", role, err)
	}
	if _, err := r.registry.Upsert(store.Interest{SessionID: claim.HolderSessionID, MachineID: "test-machine"}, []string{contracts.RoleTopicPrefix + role}); err != nil {
		r.t.Fatalf("register %s's interest in %s's lane: %v", claim.HolderSessionID, role, err)
	}
	waitFor(r.t, 5*time.Second, fmt.Sprintf("%s's claim to reach the role claim view", role), func() bool {
		held, err := r.registry.RolesHeldBy(claim.HolderSessionID)
		return err == nil && slices.Contains(held, role)
	})
	return r.stored(role)
}

// stored is role's claim as the bucket holds it now (storedRoleClaim).
func (r *roleClaimRenewalRig) stored(role string) store.RoleClaim {
	r.t.Helper()
	return storedRoleClaim(r.t, r.registry, role)
}

// register is registerThrough the rig's own registration route.
func (r *roleClaimRenewalRig) register(sessionID, instance string) {
	r.t.Helper()
	registerThrough(r.t, r.subscribe, sessionID, instance)
}

// lapsedClaim is the claim a session took an hour ago and has not answered for since, the record
// each case starts from: a renewal moving it forward is visible against any clock.
func lapsedClaim(holder, instance string) store.RoleClaim {
	claimed := time.Now().Add(-time.Hour).UnixMilli()
	return store.RoleClaim{
		HolderSessionID:   holder,
		ClaimedAt:         claimed,
		PreviousSessionID: "ses_renewal_predecessor",
		ClientInstanceID:  instance,
		RenewedAt:         claimed,
	}
}

// A registration by the process a claim names moves that claim's renewal to the registration,
// which is what keeps a role with the process that holds it: the claim stays renewed for as long
// as its holder keeps registering, and nothing else on the record moves.
func TestARegistrationRenewsTheClaimsItsOwnProcessHolds(t *testing.T) {
	t.Parallel()
	const (
		role      = "legion-controller"
		sessionID = "ses_renewing_holder"
	)
	rig := newRoleClaimRenewalRig(t)
	seeded := rig.seed(role, lapsedClaim(sessionID, holdingProcess))
	registered := time.Now().UnixMilli()

	rig.register(sessionID, holdingProcess)

	renewed := rig.stored(role)
	if renewed.RenewedAt < registered {
		t.Fatalf("the claim was renewed at %d, want the registration's own moment, %d or later (it was claimed at %d)",
			renewed.RenewedAt, registered, seeded.ClaimedAt)
	}
	if renewed.HolderSessionID != seeded.HolderSessionID || renewed.ClaimedAt != seeded.ClaimedAt ||
		renewed.PreviousSessionID != seeded.PreviousSessionID || renewed.ClientInstanceID != seeded.ClientInstanceID {
		t.Fatalf("the renewed claim is %+v, want %+v with its renewal alone moved", renewed, seeded)
	}
}

// The first registration of the session holding a claim that names no process - one an earlier
// build wrote - adopts it for the registering process, and from then on it is that process's
// claim to renew. The role was taken when it was taken, by whom it was taken from: adoption
// writes the process and the renewal, nothing else.
func TestAnAdoptingRegistrationNamesItsProcessOnAClaimThatNamesNone(t *testing.T) {
	t.Parallel()
	const (
		role      = "legion-controller"
		sessionID = "ses_adopting_holder"
	)
	rig := newRoleClaimRenewalRig(t)
	seeded := rig.seed(role, lapsedClaim(sessionID, ""))
	registered := time.Now().UnixMilli()

	rig.register(sessionID, holdingProcess)

	adopted := rig.stored(role)
	if adopted.ClientInstanceID != holdingProcess {
		t.Fatalf("the adopted claim names the process %q, want the registering process %q", adopted.ClientInstanceID, holdingProcess)
	}
	if adopted.RenewedAt < registered {
		t.Fatalf("the adopted claim was renewed at %d, want the registration's own moment, %d or later", adopted.RenewedAt, registered)
	}
	if adopted.HolderSessionID != seeded.HolderSessionID || adopted.ClaimedAt != seeded.ClaimedAt || adopted.PreviousSessionID != seeded.PreviousSessionID {
		t.Fatalf("the adopted claim is %+v, want %+v with its process and renewal alone written", adopted, seeded)
	}
}

// The largest registration production has seen (802 topics, 2026-11-16) is a holder following a
// few hundred Dispatch issues, each as `<KEY>` and `<KEY>.>`, with the per-kind topics of one
// issue that its `<KEY>.>` already holds, and the lane topic of the role it holds. This case
// registers about twice that, on a claim that names no process, and the first registration must
// adopt and renew the claim. The per-kind topics are those of an issue that sorts among the
// others, as production's did, and not first: each is then found contained only after the pairs
// before it are weighed.
//
// What it guards (INFRA-1555): a registration of that size renews its claim. The renewal runs
// after the registration's topic work, on what is left of the request's sendRequestBudget, and in
// production reducing the topics to the consumer's filters (bus.ReduceFilterSubjects) spent that
// budget: every registration was answered 200 and its renewal's claim read ran out of time, so
// the claim was never renewed while its holder kept registering.
//
// So this case holds what the registration writes, on a budget that is what is left of the test
// binary's deadline (budgetsUntilTheDeadline), and the reduction's cost is held where it can be
// counted: internal/bus's TestReducingTheLargestRegistrationInProductionWeighsEachPairOnce counts
// the pairs the reduction weighs, and fails on the reduction INFRA-1555 found. This case once ran
// on the route's own 3.5 s and 5 s instead, and under the race detector its time went mostly to
// the in-process, race-built NATS server's own check of the consumer's 1,600 filters
// (server.checkConsumerCfg, subjectIsSubsetMatch), not to the listener: on 4 CPUs beside 16 busy
// loops it answered 503 at 5.06 s in 3 runs of 3 with the reduction as it is now, and CI's merge
// queue removed pull requests that changed nothing here. Those bounds measured the runner, not the
// renewal; a row that puts them back is the same measurement again.
func TestARegistrationOfTheLargestShapeInProductionStillRenewsItsClaim(t *testing.T) {
	t.Parallel()
	const (
		role      = "legion-controller"
		sessionID = "ses_largest_holder"
		issues    = 800
	)
	topics := make([]string, 0, 2*issues+9)
	for issue := range issues {
		topic := fmt.Sprintf("notifications.dispatch.issue.K-%d", issue)
		topics = append(topics, topic, topic+".>")
	}
	for _, kind := range []string{"message", "issue", "comment", "ask"} {
		topic := "notifications.dispatch.issue.K-400." + kind
		topics = append(topics, topic, topic+".>")
	}
	topics = append(topics, contracts.RoleTopicPrefix+role)
	body, err := json.Marshal(map[string]any{
		"session_id":         sessionID,
		"topics":             topics,
		"delivery":           map[string]any{"consumer": true, "epoch": "e"},
		"client_instance_id": holdingProcess,
	})
	if err != nil {
		t.Fatalf("encode the registration: %v", err)
	}
	rig := newRoleClaimRenewalRig(t)
	budgetsUntilTheDeadline(t, rig.deps)
	seeded := rig.seed(role, lapsedClaim(sessionID, ""))

	registered := time.Now()
	recorder := httptest.NewRecorder()
	rig.subscribe.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", bytes.NewReader(body)))
	t.Logf("registered %d topics in %s", len(topics), time.Since(registered).Round(time.Millisecond))

	if recorder.Code != http.StatusOK {
		t.Fatalf("the registration of %d topics was answered %d %s, want 200", len(topics), recorder.Code, strings.TrimSpace(recorder.Body.String()))
	}
	adopted := rig.stored(role)
	if adopted.ClientInstanceID != holdingProcess || adopted.RenewedAt < registered.UnixMilli() {
		var unrenewed []string
		for line := range strings.Lines(rig.logs.String()) {
			if strings.Contains(line, "left a role claim unrenewed") {
				unrenewed = append(unrenewed, strings.TrimSpace(line))
			}
		}
		t.Fatalf("the claim is now %+v, want %+v adopted by the registering process %q and renewed at the registration, %d or later; the route logged %q",
			adopted, seeded, holdingProcess, registered.UnixMilli(), unrenewed)
	}
}

// The incident this guards: a second process resumed a live session's id and registers under it.
// Its heartbeat says nothing about the claim the first process holds, so the claim does not move
// - a renewal it could move would keep a role alive for a process that does not hold it, and
// would hold the role against its real holder once that holder stopped.
func TestARegistrationFromAnotherProcessRenewsNothing(t *testing.T) {
	t.Parallel()
	const (
		role      = "legion-controller"
		sessionID = "ses_shared_id"
	)
	rig := newRoleClaimRenewalRig(t)
	seeded := rig.seed(role, lapsedClaim(sessionID, holdingProcess))

	rig.register(sessionID, secondProcess)

	if stored := rig.stored(role); stored != seeded {
		t.Fatalf("the claim is now %+v, want it exactly as the holding process left it, %+v", stored, seeded)
	}
}

// A listener leaves its role claim view unable to answer for a while after a restart, and every
// session's heartbeat arrives in that window. The registration is answered 200 and renews the
// claim all the same: the role's lane topic is on the row the registration's own write stored,
// the claim record names the registering process, and a view that cannot answer is no failure
// of the renewal, so nothing says the renewal was passed over.
func TestARegistrationWhoseRoleClaimViewCannotAnswerRenewsTheClaimItsRowNames(t *testing.T) {
	t.Parallel()
	const (
		role      = "legion-controller"
		sessionID = "ses_refilling_view"
	)
	rig := newRoleClaimRenewalRig(t)
	seeded := rig.seed(role, lapsedClaim(sessionID, holdingProcess))
	refuseRoleClaimView(t, rig.registry)
	if _, err := rig.registry.RolesHeldBy(sessionID); !errors.Is(err, store.ErrRoleViewNotReady) {
		t.Fatalf("RolesHeldBy(%s) answered %v, want ErrRoleViewNotReady: the case is a view that cannot answer", sessionID, err)
	}

	rig.register(sessionID, holdingProcess)

	stored := rig.stored(role)
	if stored.RenewedAt <= seeded.RenewedAt || stored.HolderSessionID != sessionID || stored.ClientInstanceID != holdingProcess {
		t.Fatalf("the claim is now %+v, want %+v renewed by the registration: its row names the role", stored, seeded)
	}
	if strings.Contains(rig.logs.String(), "left a role claim unrenewed") {
		t.Fatalf("the route logged a renewal failure for a view that cannot answer:\n%s", rig.logs.String())
	}
}

// racingRoleClaimUpdate lands another claim under a renewal's read: the hook runs before the
// first conditional update reaches the bucket, and the update itself is the real one, so the
// loss the renewal takes is the server's rather than one this wrapper invented.
type racingRoleClaimUpdate struct {
	natsgo.KeyValue

	updates           int
	beforeFirstUpdate func(natsgo.KeyValue)
}

func (kv *racingRoleClaimUpdate) Update(key string, value []byte, revision uint64) (uint64, error) {
	kv.updates++
	if kv.updates == 1 {
		kv.beforeFirstUpdate(kv.KeyValue)
	}
	return kv.KeyValue.Update(key, value, revision)
}

// A renewal that loses its conditional write lost it to a claim that landed after it read the
// record: that claim is the one standing, and the renewal is dropped rather than written into
// it. The registration is answered 200, since a heartbeat is not the place to report a race it
// has already lost.
func TestARegistrationWhoseRenewalLosesItsCompareAndSwapStillRegisters(t *testing.T) {
	t.Parallel()
	const (
		role      = "legion-controller"
		sessionID = "ses_raced_holder"
	)
	rig := newRoleClaimRenewalRig(t)
	rig.seed(role, lapsedClaim(sessionID, holdingProcess))
	winner := lapsedClaim(sessionID, secondProcess)
	winner.ClaimedAt, winner.RenewedAt = time.Now().UnixMilli(), time.Now().UnixMilli()
	record, err := json.Marshal(winner)
	if err != nil {
		t.Fatalf("encode the claim that wins the race: %v", err)
	}
	racing := &racingRoleClaimUpdate{KeyValue: rig.roleKV}
	racing.beforeFirstUpdate = func(kv natsgo.KeyValue) {
		if _, err := kv.Put(role, record); err != nil {
			t.Errorf("land the claim that wins the race: %v", err)
		}
	}
	setRegistryRoleKV(t, rig.registry, racing)

	rig.register(sessionID, holdingProcess)

	if racing.updates != 1 {
		t.Fatalf("the renewal made %d conditional updates, want exactly one: a lost renewal is never retried", racing.updates)
	}
	stored := rig.stored(role)
	stored.Revision = 0
	if stored != winner {
		t.Fatalf("the claim is now %+v, want the claim that won the race, %+v", stored, winner)
	}
}
