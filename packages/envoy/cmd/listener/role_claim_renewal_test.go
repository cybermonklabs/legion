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
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/alonetest"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/shapetest"
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

// largestShapeHolder is the session that registers shapetest.Topics in the cases below.
const largestShapeHolder = "ses_largest_holder"

// The first registration of a holder of the largest shape production has seen (shapetest.Topics),
// on a claim that names no process, adopts and renews the claim, is answered 200 and stores every
// topic it names. The renewal runs after the registration's topic work, on what that work left of
// the registration's budget, and a registration of that many topics still reaches it and renews.
// The registration is given the time left before the test binary's deadline
// (budgetsUntilTheDeadline) in place of its 3.5 s (sendRequestBudget), so the case asserts what the
// registration writes and not how soon, and its verdict is the same on any machine, serial or
// parallel.
//
// What spends that budget is topic work, and three counts hold it, none of them the machine's.
// Reducing the topics to the consumer's filters spent it in production (INFRA-1555): every
// registration was answered 200, its renewal's claim read ran out of time, and the claim was never
// renewed while its holder kept registering. TestReducingTheLargestShapeInProductionAllocatesPerSubjectNotPerPair
// holds the allocations of a registration's reductions, which see a split of both subjects of every
// pair, and internal/bus's TestReducingTheLargestRegistrationInProductionWeighsEachPairOnce holds the
// pairs the reduction weighs, which see a scan started again after each drop. The third is here:
// the configurations of this session's consumer that the registration hands the server, and the
// filter subjects in them, held to largestShapeServerFilterCeiling. Those are the only inputs the
// listener gives the server's check of a consumer's filters (checkConsumerCfg), which under the
// race detector is the largest cost that grows with the topics. So the ceiling bounds what the
// listener asks the server to check, and says nothing about how the server checks it. The
// allocation count sees the allocations of the listener's own filter rules (sessionConsumerFilter),
// and not their work that allocates nothing. No count sees the listener's topic work outside its
// consumer filter (the plan's merge, the answer's warnings, the record's encoding), the server's
// own algorithm, or the size of the KV records the registration writes.
func TestARegistrationOfTheLargestShapeInProductionStillRenewsItsClaim(t *testing.T) {
	t.Parallel()
	const role = "legion-controller"
	topics := shapetest.Topics(role)
	body, err := json.Marshal(map[string]any{
		"session_id":         largestShapeHolder,
		"topics":             topics,
		"delivery":           map[string]any{"consumer": true, "epoch": "e"},
		"client_instance_id": holdingProcess,
	})
	if err != nil {
		t.Fatalf("encode the registration: %v", err)
	}
	consumer, ok := bus.SessionConsumerName(largestShapeHolder)
	if !ok {
		t.Fatalf("%s can have no session consumer", largestShapeHolder)
	}
	rig := newRoleClaimRenewalRig(t)
	budgetsUntilTheDeadline(t, rig.deps)
	seeded := rig.seed(role, lapsedClaim(largestShapeHolder, ""))
	creates := watchConsumerConfigs(t, rig.deps.client.Conn, consumer)

	registered := time.Now()
	recorder := httptest.NewRecorder()
	rig.subscribe.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", bytes.NewReader(body)))
	configs, filters := creates.read(t)
	// The time is for the reader only: the registration was given until the test's deadline, and
	// nothing below reads it.
	t.Logf("registered %d topics in %s; the product gives a registration %s (sendRequestBudget), and this case gave it until the test binary's deadline; the server was handed %d configurations of consumer %s, %d filter subjects in all",
		len(topics), time.Since(registered).Round(time.Millisecond), sendRequestBudget, configs, consumer, filters)

	if recorder.Code != http.StatusOK {
		t.Fatalf("the registration of %d topics was answered %d %s, want 200", len(topics), recorder.Code, strings.TrimSpace(recorder.Body.String()))
	}
	if configs == 0 {
		t.Fatalf("UNKNOWN: the watch of the JetStream API saw no configuration of consumer %s, the one this registration creates, so the count read nothing", consumer)
	}
	if ceiling := largestShapeServerFilterCeiling(len(topics)); filters > ceiling {
		t.Fatalf("the registration of %d topics handed the server %d configurations of consumer %s, %d filter subjects in all, for it to check; want at most %d (largestShapeServerFilterCeiling)", len(topics), configs, consumer, filters, ceiling)
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
// server, over all the configurations of its session consumer it sends: half as many again as its
// topics. The registration of shapetest.Topics sends one configuration of 1,602 filters for its
// 1,609 topics (five runs under the race detector), so the ceiling, 2,413, leaves it half again as
// many. It refuses a doubling: a configuration sent twice, or every topic given a second filter,
// hands the server about 3,200.
func largestShapeServerFilterCeiling(topics int) int { return topics * 3 / 2 }

// consumerConfigs counts the configurations of one consumer a JetStream server is handed to check,
// with the filter subjects each carries.
type consumerConfigs struct {
	conn *natsgo.Conn
	subs []*natsgo.Subscription
	// subjects are the create subjects of the consumer counted: the create request names the
	// consumer, and one that carries a single filter adds it after the name.
	subjects []string
}

// watchConsumerConfigs subscribes, on conn's account, to the JetStream API's consumer create
// requests, which carry every configuration a consumer is created or updated with: the session
// consumers send both as a create with an action (bus.SessionConsumers.upsert), and a durable may
// be created on the older durable subject. Every such request made on that account from here on
// reaches the subscriptions as well as the server. Each test has an account of its own
// (testnats.URL), so no other test's consumers are read.
func watchConsumerConfigs(t *testing.T, conn *natsgo.Conn, consumer string) *consumerConfigs {
	t.Helper()
	w := &consumerConfigs{conn: conn, subjects: []string{
		"$JS.API.CONSUMER.CREATE." + bus.Stream + "." + consumer,
		"$JS.API.CONSUMER.DURABLE.CREATE." + bus.Stream + "." + consumer,
	}}
	for _, subject := range []string{"$JS.API.CONSUMER.CREATE.>", "$JS.API.CONSUMER.DURABLE.CREATE.>"} {
		sub, err := conn.SubscribeSync(subject)
		if err != nil {
			t.Fatalf("subscribe to %s: %v", subject, err)
		}
		t.Cleanup(func() { _ = sub.Unsubscribe() })
		w.subs = append(w.subs, sub)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the consumer create subscriptions: %v", err)
	}
	return w
}

// read counts the configurations of the watched consumer handed to the server since the watch
// began, and the filter subjects they carried in all; creates of other consumers, a key-value
// watcher's ordered consumer among them, are not counted. A create of the watched consumer that
// does not decode, or carries no configuration, fails the case, since the count could not read it.
// The flush comes first: the server queued each request's copy for this connection when it
// received the request, before the reply its sender waited for, so after the flush's PONG every
// request answered so far is in the subscriptions' queues.
func (w *consumerConfigs) read(t *testing.T) (configs, filters int) {
	t.Helper()
	if err := w.conn.Flush(); err != nil {
		t.Fatalf("flush the consumer create subscriptions: %v", err)
	}
	for _, sub := range w.subs {
		queued, _, err := sub.Pending()
		if err != nil {
			t.Fatalf("count the consumer create requests: %v", err)
		}
		for range queued {
			msg, err := sub.NextMsg(time.Second)
			if err != nil {
				t.Fatalf("read a queued consumer create request: %v", err)
			}
			if !slices.ContainsFunc(w.subjects, func(subject string) bool {
				return msg.Subject == subject || strings.HasPrefix(msg.Subject, subject+".")
			}) {
				continue
			}
			var request struct {
				Config *struct {
					FilterSubject  string   `json:"filter_subject"`
					FilterSubjects []string `json:"filter_subjects"`
				} `json:"config"`
			}
			if err := json.Unmarshal(msg.Data, &request); err != nil || request.Config == nil {
				t.Fatalf("a create request on %s did not decode to a configuration (%v): %q", msg.Subject, err, msg.Data)
			}
			configs++
			filters += len(request.Config.FilterSubjects)
			if request.Config.FilterSubject != "" {
				filters++
			}
		}
	}
	return configs, filters
}

// reduceAllocsChildEnv marks the run of the test binary that counts a registration's reductions
// alone (TestReducingTheLargestShapeInProductionAllocatesPerSubjectNotPerPair).
const reduceAllocsChildEnv = "ENVOY_TEST_LARGEST_SHAPE_REDUCE_ALLOCS_CHILD"

// consumerFilterAllocsPerTopic is how many allocations sessionConsumerFilter may make for each
// topic of shapetest.Planned: half again the 23.95 a topic it makes now (38,559 for 1,610 topics,
// three runs under the race detector, alone). That is the filter rules read for each topic, the
// forms and provenance of each, and one reduction with provenance (bus.SessionConsumerReduction).
// It refuses that work done twice over. The reduction INFRA-1555 replaced made about 30,000 a topic
// over this shape.
const consumerFilterAllocsPerTopic = 36

// reduceAllocsPerTopic is how many allocations the consumer's own reduction of that filter
// (bus.SessionConsumerFilters) may make for each topic of shapetest.Planned: half again the 2.01 a
// topic it makes now, the forms of each subject and one split of each form into its tokens, a
// ceiling of 4,830 against 3,229. It is set to refuse a split of the subjects of every pair
// weighed, which the reduction INFRA-1555 replaced made, about 30,300 a topic. A second split of
// each form (1,612 allocations more, 4,841) is refused only by 11 allocations, so a saving of 12
// elsewhere would let it through: the ceiling is no guard against a change that small.
const reduceAllocsPerTopic = 3

// A registration's consumer call computes its consumer's filter (sessionConsumerFilter: the
// filter rules read for each planned topic, then one reduction with provenance through
// bus.SessionConsumerReduction, to leave out wildcard topics whose merge reaches another session),
// and then the consumer's own reduction of that filter (bus.SessionConsumerFilters, in
// sessionConsumerTarget), inside the registration's budget and before the renewal that runs on
// what is left of it. This case counts the allocations of each of those two calls, as the
// registration makes them, over shapetest.Planned, the topics a registration of the largest shape
// plans. So the list the second call reduces is the one the registration computes, and not a list
// built to stand for it. sessionConsumerFilter leaves the role lane out by the same code that
// leaves it out of a registration, and puts the session's agent subject first, so the agent subject
// is in the list twice: 1,610 subjects in 1,612 forms, the agent subject's two forms counted twice,
// which are 1,609 topics in 1,610 forms held once each. They reduce to the 1,602 filters
// TestARegistrationOfTheLargestShapeInProductionStillRenewsItsClaim counts on the server. A
// registration makes this computation more than once (planWithoutLeftOut does it on the plan as
// well), each time at this cost.
//
// INFRA-1555 was the reduction spending the budget in production. It split both subjects of every
// pair it weighed (bus.SubjectContains), and it weighed them all again after each subject it
// dropped. A count of allocations sees the first half. The second allocates nothing, and
// internal/bus's TestReducingTheLargestRegistrationInProductionWeighsEachPairOnce sees it by the
// pairs the reduction weighs. What the registration hands the server to check is
// TestARegistrationOfTheLargestShapeInProductionStillRenewsItsClaim's count.
//
// The count runs in a run of the test binary of its own (alonetest), since the package's other
// tests leave servers and watchers running that allocate meanwhile. The run that starts it is
// parallel: it measures nothing and only waits. The run alone is not, because
// testing.AllocsPerRun refuses to measure in a parallel test.
func TestReducingTheLargestShapeInProductionAllocatesPerSubjectNotPerPair(t *testing.T) {
	if !alonetest.Child(reduceAllocsChildEnv) {
		t.Parallel()
		alonetest.Run(t, reduceAllocsChildEnv, 2)
		return
	}
	topics := shapetest.Planned("legion-controller", largestShapeHolder)
	var filter []string
	filterAllocs := testing.AllocsPerRun(1, func() { filter = sessionConsumerFilter(largestShapeHolder, topics) })
	forms := 0
	for _, subject := range filter {
		forms += len(contracts.TopicForms(subject))
	}
	alonetest.Measured(t, "%v allocations for the consumer filter of %d planned topics, %d subjects; %.2f a topic",
		filterAllocs, len(topics), len(filter), filterAllocs/float64(len(topics)))
	var filters []string
	reduceAllocs := testing.AllocsPerRun(1, func() { filters = bus.SessionConsumerFilters(filter) })
	alonetest.Measured(t, "%v allocations to reduce those %d subjects in %d forms to %d filters; %.2f a topic",
		reduceAllocs, len(filter), forms, len(filters), reduceAllocs/float64(len(topics)))
	if bound := float64(consumerFilterAllocsPerTopic * len(topics)); filterAllocs > bound {
		t.Errorf("the consumer filter of %d planned topics made %v allocations, more than %v (%d a topic): it does more than the filter rules, forms, provenance and one reduction of each topic", len(topics), filterAllocs, bound, consumerFilterAllocsPerTopic)
	}
	if bound := float64(reduceAllocsPerTopic * len(topics)); reduceAllocs > bound {
		t.Errorf("reducing the consumer filter of %d planned topics made %v allocations, more than %v (%d a topic): it allocates for more than the forms of each subject and one split of each, as INFRA-1555's split of every pair it weighed did", len(topics), reduceAllocs, bound, reduceAllocsPerTopic)
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

	if stored := rig.stored(role); !reflect.DeepEqual(stored, seeded) {
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
	// Revision and Created belong to the entry the read answered, not to the stored record.
	stored.Revision, stored.Created = 0, time.Time{}
	if !reflect.DeepEqual(stored, winner) {
		t.Fatalf("the claim is now %+v, want the claim that won the race, %+v", stored, winner)
	}
}
