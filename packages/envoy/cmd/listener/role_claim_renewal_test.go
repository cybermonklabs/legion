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
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/counttest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/store"
)

// roleClaimRenewalRig is the registration route over one role bucket, with the bucket itself
// beside it: a case seeds the claim it is about as a record, as a build that wrote it before
// this one left it, and reads back what the registration made of it.
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

// largestShapeHolder is the session that registers counttest.LargestRegistration's body below.
const largestShapeHolder = "ses_largest_holder"

// The first registration of a holder of the largest shape production has seen
// (counttest.LargestRegistration), on a claim that names no process, adopts and renews the claim,
// is answered 200 and stores every topic it names. The renewal runs after the registration's topic
// work, on what that work left of the registration's budget, and a registration of that many
// topics still reaches it and renews.
//
// The registration is given the time left before the test binary's deadline
// (budgetsUntilTheDeadline) in place of its 3.5 s (sendRequestBudget), and the case asserts what
// the registration writes, not how soon, so its verdict is not the machine's. It runs in parallel:
// the registration's elapsed time meets no fixed bound, so a busy runner makes it slower and leaves
// its verdict as it was.
//
// Topic work that spends the budget is what would leave the renewal too little of it. Reducing the
// topics to the consumer's filters did that in production (INFRA-1555): every registration was
// answered 200, its renewal's claim read ran out of time, and the claim was never renewed while its
// holder kept registering. Three counts hold that work, and none is the machine's. Two are
// internal/bus's, on the listener's own reduction, one for each half of INFRA-1555:
// TestReducingTheLargestShapeInProductionAllocatesPerSubjectNotPerPair holds its allocations, which
// see a split of both subjects of every pair, and
// TestReducingTheLargestRegistrationInProductionWeighsEachPairOnceBetweenMerges holds the pairs it
// weighs, which see a scan started again after a drop. Both reduce the topics this registration
// plans, its role lane among them, which this registration's consumer filter leaves out
// (counttest.LargestRegistration). The third is here: the consumer configurations this
// registration hands the server, and the filter subjects in them, held to
// largestShapeServerFilterCeiling, where at least one of those configurations must be on this
// session consumer's create subject. Those are the only inputs the listener gives the server's
// check of a consumer's filters (checkConsumerCfg), which under the race detector is the largest
// cost that grows with the topics (1.32 s of 4.32 s of a CPU profile over five of these
// registrations, against 0.22 s for the reduction). So the ceiling bounds what the listener asks
// the server to check, and says nothing about how the server checks it. No count sees the server's
// own algorithm, the size of the KV records the registration writes, or the listener's topic work
// outside the reduction.
func TestARegistrationOfTheLargestShapeInProductionStillRenewsItsClaim(t *testing.T) {
	t.Parallel()
	const role = "legion-controller"
	topics, _ := counttest.LargestRegistration(role, largestShapeHolder)
	consumer, ok := bus.SessionConsumerName(largestShapeHolder)
	if !ok {
		t.Fatalf("%s can have no session consumer", largestShapeHolder)
	}
	body, err := json.Marshal(map[string]any{
		"session_id":         largestShapeHolder,
		"topics":             topics,
		"delivery":           map[string]any{"consumer": true, "epoch": "e"},
		"client_instance_id": holdingProcess,
	})
	if err != nil {
		t.Fatalf("encode the registration: %v", err)
	}
	rig := newRoleClaimRenewalRig(t)
	budgetsUntilTheDeadline(t, rig.deps)
	seeded := rig.seed(role, lapsedClaim(largestShapeHolder, ""))
	// The session consumer's create and update go to its create subject (bus's consumerAPI).
	create := "$JS.API.CONSUMER.CREATE." + bus.Stream + "." + consumer
	writes := watchConsumerConfigWrites(t, rig.deps.client.Conn, create)

	registered := time.Now()
	recorder := httptest.NewRecorder()
	rig.subscribe.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", bytes.NewReader(body)))
	configs, filters, creates := writes.read(t)
	// The time is for the reader only: the registration was given until the test's deadline, and
	// nothing below reads it.
	t.Logf("registered %d topics in %s; the product gives a registration %s (sendRequestBudget), and this case gave it until the test binary's deadline; the server was handed %d consumer configurations, %d of them on %s, %d filter subjects in all",
		len(topics), time.Since(registered).Round(time.Millisecond), sendRequestBudget, configs, creates, create, filters)

	if recorder.Code != http.StatusOK {
		t.Fatalf("the registration of %d topics was answered %d %s, want 200", len(topics), recorder.Code, strings.TrimSpace(recorder.Body.String()))
	}
	if creates == 0 {
		t.Fatalf("UNKNOWN: the watch of the JetStream API saw %d consumer configurations and none on %s, this session consumer's create subject, so it cannot have seen the one this registration's consumer was created with", configs, create)
	}
	if ceiling := largestShapeServerFilterCeiling(len(topics)); filters > ceiling {
		t.Fatalf("the registration of %d topics handed the server %d consumer configurations, %d filter subjects in all, for it to check; want at most %d (largestShapeServerFilterCeiling)", len(topics), configs, filters, ceiling)
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
	// A registration answered 200 may leave topics out of its row (subscribeHandler's warnings),
	// and one that left most of these out would renew the claim as a far smaller shape.
	row, _, err := rig.registry.RefreshAt(t.Context(), largestShapeHolder)
	if err != nil {
		t.Fatalf("read %s's record from the bucket: %v", largestShapeHolder, err)
	}
	stored := make(map[string]bool, len(row.Topics))
	for _, topic := range row.Topics {
		stored[topic] = true
	}
	for _, topic := range topics {
		if !stored[topic] {
			t.Fatalf("the bucket's record holds %d topics and lacks %s, one of the %d registered", len(row.Topics), topic, len(topics))
		}
	}
}

// largestShapeServerFilterCeiling is how many filter subjects a registration of topics may hand the
// server, over all the consumer configurations it sends: half as many again as its topics. The
// registration of counttest.LargestRegistration's body sends one configuration of 1,602 filters for
// its 1,609 topics (five runs under the race detector), so the ceiling, 2,413, leaves it half again
// as many. It refuses a doubling: a configuration sent twice, or every topic given a second filter,
// hands the server about 3,200.
func largestShapeServerFilterCeiling(topics int) int { return topics * 3 / 2 }

// consumerConfigWrites counts the consumer configurations a JetStream server is handed to check:
// each create or update request of the JetStream API on one account, with the filter subjects it
// carries, and how many of those requests were on one consumer's create subject.
type consumerConfigWrites struct {
	conn   *natsgo.Conn
	subs   []*natsgo.Subscription
	create string
}

// watchConsumerConfigWrites subscribes to the JetStream API's consumer create subjects on conn's
// account: the create subject, which carries updates too, and the durable create subject of older
// clients. Every request on them made on that account from here on reaches the subscriptions as
// well as the server. create is the one consumer's create subject whose requests read counts apart.
func watchConsumerConfigWrites(t *testing.T, conn *natsgo.Conn, create string) *consumerConfigWrites {
	t.Helper()
	writes := &consumerConfigWrites{conn: conn, create: create}
	for _, subject := range []string{"$JS.API.CONSUMER.CREATE.>", "$JS.API.CONSUMER.DURABLE.CREATE.>"} {
		sub, err := conn.SubscribeSync(subject)
		if err != nil {
			t.Fatalf("subscribe to %s: %v", subject, err)
		}
		t.Cleanup(func() { _ = sub.Unsubscribe() })
		writes.subs = append(writes.subs, sub)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the consumer create subscriptions: %v", err)
	}
	return writes
}

// read counts the consumer configurations handed to the server since the watch began, the filter
// subjects they carried in all, and how many of them came on the watch's create subject. Every
// request on a create subject carries a configuration, so one that does not decode, or carries
// none, fails t: the filter subjects it handed the server would otherwise drop out of the count
// unseen. The flush comes first: the server queued each request's copy for this connection when it
// received the request, before the reply its sender waited for, so after the flush's PONG every
// request answered so far is in a subscription's queue.
func (w *consumerConfigWrites) read(t *testing.T) (configs, filters, creates int) {
	t.Helper()
	if err := w.conn.Flush(); err != nil {
		t.Fatalf("flush the consumer create subscriptions: %v", err)
	}
	for _, sub := range w.subs {
		queued, _, err := sub.Pending()
		if err != nil {
			t.Fatalf("count the consumer create requests on %s: %v", sub.Subject, err)
		}
		for range queued {
			msg, err := sub.NextMsg(time.Second)
			if err != nil {
				t.Fatalf("read a queued consumer create request on %s: %v", sub.Subject, err)
			}
			var request struct {
				Config *struct {
					FilterSubject  string   `json:"filter_subject"`
					FilterSubjects []string `json:"filter_subjects"`
				} `json:"config"`
			}
			if err := json.Unmarshal(msg.Data, &request); err != nil {
				t.Fatalf("the consumer create request on %s does not decode, so the filter subjects it handed the server cannot be counted: %v; it reads %.200q", msg.Subject, err, msg.Data)
			}
			if request.Config == nil {
				t.Fatalf("the consumer create request on %s carries no configuration, so the filter subjects it handed the server cannot be counted; it reads %.200q", msg.Subject, msg.Data)
			}
			configs++
			if msg.Subject == w.create {
				creates++
			}
			filters += len(request.Config.FilterSubjects)
			if request.Config.FilterSubject != "" {
				filters++
			}
		}
	}
	return configs, filters, creates
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
