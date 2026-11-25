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
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
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

// largestShapeHolder is the session that registers largestProductionShape in the cases below.
const largestShapeHolder = "ses_largest_holder"

// largestProductionShape is about twice the largest registration production has seen (802
// topics, 2026-11-16): a holder following a few hundred Dispatch issues, each as `<KEY>` and
// `<KEY>.>`, with the per-kind topics of one issue that its `<KEY>.>` already holds, and the lane
// topic of role, which it holds. The per-kind topics are those of an issue that sorts among the
// others, as production's did, and not first: each is then found contained only after the pairs
// before it are weighed.
func largestProductionShape(role string) []string {
	const issues = 800
	topics := make([]string, 0, 2*issues+9)
	for issue := range issues {
		topic := fmt.Sprintf("notifications.dispatch.issue.K-%d", issue)
		topics = append(topics, topic, topic+".>")
	}
	for _, kind := range []string{"message", "issue", "comment", "ask"} {
		topic := "notifications.dispatch.issue.K-400." + kind
		topics = append(topics, topic, topic+".>")
	}
	return append(topics, contracts.RoleTopicPrefix+role)
}

// The first registration of a holder of the largest shape production has seen
// (largestProductionShape), on a claim that names no process, adopts and renews the claim, is
// answered 200 and stores every topic it names. The renewal runs after the registration's topic
// work, on what that work left of the registration's budget, and a registration of that many
// topics still reaches it and renews.
//
// The registration is given the time left before the test binary's deadline
// (budgetsUntilTheDeadline) in place of its 3.5 s (sendRequestBudget), and the case asserts what
// the registration writes, not how soon. On the product's own budget the verdict was the
// machine's. Under the race detector, on two CPUs that eight busy loops share, this registration
// took 3.6 to 4.7 s at the commit that added this case (0316ab0dcdb9) and 3.8 to 5.2 s at main
// (8ce4195034e2), and failed every run at both. On four idle CPUs it took 0.9 to 1.2 s at both.
// Since cybermonklabs/legion#5188 (2026-11-25T00:56Z) this case runs beside seven others on CI's
// four-vCPU runner, and there it overran 3.5 s by 67 to 976 ms. The largest cost that grows with
// the topics is the in-process NATS server's check of the consumer's 1,600 filters. That server is
// race-built here. Production's server is not race-built, and it runs on its own machine.
// cybermonklabs/legion#5512 then made the case serial again while it read a clock. It runs in
// parallel again now: the registration's elapsed time no longer meets a fixed bound, so a busy
// runner makes it slower and leaves its verdict as it was.
//
// What the clock guarded here is topic work that spends the budget. Reducing the topics to the
// consumer's filters did that in production (INFRA-1555): every registration was answered 200,
// its renewal's claim read ran out of time, and the claim was never renewed while its holder kept
// registering. Three counts hold that work now, and none is the machine's. Two are on the
// listener's own reduction, one for each half of INFRA-1555:
// TestReducingTheLargestShapeInProductionAllocatesPerSubjectNotPerPair holds its allocations, which
// see a split of both subjects of every pair, and internal/bus's
// TestReducingTheLargestRegistrationInProductionWeighsEachPairOnce holds the pairs it weighs, which
// see a scan started again after each drop. The third is here: the consumer configurations this
// registration hands the server, and the filter subjects in them, held to
// largestShapeServerFilterCeiling. Those are the only inputs the listener gives the server's
// check of a consumer's filters (checkConsumerCfg), which under the race detector is the largest
// cost that grows with the topics (1.32 s of 4.32 s of a CPU profile over five of these
// registrations, against 0.22 s for the reduction). So the ceiling bounds what the listener asks
// the server to check, and says nothing about how the server checks it. No count sees the
// server's own algorithm, the size of the KV records the registration writes, or the listener's
// topic work outside the reduction.
func TestARegistrationOfTheLargestShapeInProductionStillRenewsItsClaim(t *testing.T) {
	t.Parallel()
	const role = "legion-controller"
	topics := largestProductionShape(role)
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
	writes := watchConsumerConfigWrites(t, rig.deps.client.Conn)

	registered := time.Now()
	recorder := httptest.NewRecorder()
	rig.subscribe.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", bytes.NewReader(body)))
	configs, filters := writes.read(t)
	// The time is for the reader only: the registration was given until the test's deadline, and
	// nothing below reads it.
	t.Logf("registered %d topics in %s; the product gives a registration %s (sendRequestBudget), and this case gave it until the test binary's deadline; the server was handed %d consumer configurations, %d filter subjects in all",
		len(topics), time.Since(registered).Round(time.Millisecond), sendRequestBudget, configs, filters)

	if recorder.Code != http.StatusOK {
		t.Fatalf("the registration of %d topics was answered %d %s, want 200", len(topics), recorder.Code, strings.TrimSpace(recorder.Body.String()))
	}
	if configs == 0 {
		t.Fatalf("UNKNOWN: the watch of the JetStream API saw no consumer configuration, so it cannot have seen the one this registration's consumer was created with")
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
// registration of largestProductionShape sends one configuration of 1,602 filters for its 1,609
// topics (five runs under the race detector), so the ceiling, 2,413, leaves it half again as many.
// It refuses a doubling: a configuration sent twice, or every topic given a second filter, hands
// the server about 3,200.
func largestShapeServerFilterCeiling(topics int) int { return topics * 3 / 2 }

// consumerConfigWrites counts the consumer configurations a JetStream server is handed to check:
// each create or update request of the JetStream API on one account, with the filter subjects
// it carries.
type consumerConfigWrites struct {
	conn *natsgo.Conn
	sub  *natsgo.Subscription
}

// watchConsumerConfigWrites subscribes to the JetStream API's consumer requests on conn's
// account. Every request made on that account from here on reaches the subscription as well as
// the server.
func watchConsumerConfigWrites(t *testing.T, conn *natsgo.Conn) *consumerConfigWrites {
	t.Helper()
	sub, err := conn.SubscribeSync("$JS.API.CONSUMER.>")
	if err != nil {
		t.Fatalf("subscribe to the consumer requests: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the consumer request subscription: %v", err)
	}
	return &consumerConfigWrites{conn: conn, sub: sub}
}

// read counts the consumer configurations handed to the server since the watch began, and the
// filter subjects they carried in all. A request that carries no configuration (an info, a
// delete, a pull) is none. The flush comes first: the server queued each request's copy for this
// connection when it received the request, before the reply its sender waited for, so after the
// flush's PONG every request answered so far is in the subscription's queue.
func (w *consumerConfigWrites) read(t *testing.T) (configs, filters int) {
	t.Helper()
	if err := w.conn.Flush(); err != nil {
		t.Fatalf("flush the consumer request subscription: %v", err)
	}
	queued, _, err := w.sub.Pending()
	if err != nil {
		t.Fatalf("count the consumer requests: %v", err)
	}
	for range queued {
		msg, err := w.sub.NextMsg(time.Second)
		if err != nil {
			t.Fatalf("read a queued consumer request: %v", err)
		}
		var request struct {
			Config *struct {
				FilterSubject  string   `json:"filter_subject"`
				FilterSubjects []string `json:"filter_subjects"`
			} `json:"config"`
		}
		if len(msg.Data) == 0 || json.Unmarshal(msg.Data, &request) != nil || request.Config == nil {
			continue
		}
		configs++
		filters += len(request.Config.FilterSubjects)
		if request.Config.FilterSubject != "" {
			filters++
		}
	}
	return configs, filters
}

// reduceAllocsChildEnv marks the run of the test binary that counts the reduction's allocations
// alone (TestReducingTheLargestShapeInProductionAllocatesPerSubjectNotPerPair).
const reduceAllocsChildEnv = "ENVOY_TEST_LARGEST_SHAPE_REDUCE_ALLOCS_CHILD"

// reduceAllocsPerSubject is how many allocations the reduction of largestProductionShape to its
// consumer's filters may make for each of its topics, a ceiling of 4,830 for the 1,610 topics of
// that shape with the agent subject. The reduction makes 3,229 to 3,238 now, in three runs under
// the race detector, 2.01 a topic: the forms of each topic (contracts.TopicForms) and one split of
// each form into its tokens. So the ceiling leaves it half again as many, and refuses a doubling,
// such as a second split of each form. The reduction INFRA-1555 replaced split both subjects of
// every pair it weighed. Put back through an overlay, it made 48,879,575 to 48,879,593 for the
// same topics in four runs, 30,360 a topic, and this ceiling refused it each time.
const reduceAllocsPerSubject = 3

// Each consumer call of a registration reduces the session's topics to its consumer's filters
// (bus.SessionConsumerFilters), inside the registration's budget and before the renewal that runs
// on what is left of it. INFRA-1555 was that reduction spending the budget in production. It split
// both subjects of every pair it weighed (bus.SubjectContains), and it weighed them all again after
// each subject it dropped. The fix splits each subject once. This case counts the reduction's
// allocations over largestProductionShape and holds them to reduceAllocsPerSubject a topic: one
// shape and one ceiling, which the INFRA-1555 reduction exceeds 10,120 times over.
//
// The verdict is a count and not a clock, so it is not the machine's. A count of allocations does
// not see work that allocates nothing: a scan of the pre-split tokens started again after each
// drop, which was the other half of INFRA-1555, is internal/bus's
// TestReducingTheLargestRegistrationInProductionWeighsEachPairOnce's to see, by the pairs it
// weighs. What the registration hands the server to check is
// TestARegistrationOfTheLargestShapeInProductionStillRenewsItsClaim's count. No count sees the
// listener's topic work outside this reduction.
//
// testing.AllocsPerRun counts every allocation of the process while it measures, and this
// package's other tests leave servers and watchers running that allocate meanwhile. So the case
// runs the test binary again for this case alone, as
// TestTheRelaysFirstReadOfAFrameMakesNoAllocationForEachKey does. The measurement is that run's,
// and the case fails unless that run reports it. The run that starts the child is parallel
// (t.Parallel): it measures nothing and only waits for the child. The child is not parallel,
// because testing.AllocsPerRun refuses to measure in a parallel test.
func TestReducingTheLargestShapeInProductionAllocatesPerSubjectNotPerPair(t *testing.T) {
	if os.Getenv(reduceAllocsChildEnv) == "" {
		t.Parallel()
		child := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.v")
		child.Env = append(os.Environ(), reduceAllocsChildEnv+"=1")
		output, err := child.CombinedOutput()
		t.Logf("the run of this test alone:\n%s", output)
		if err != nil {
			t.Fatalf("the run of this test alone failed: %v", err)
		}
		if measured := strings.Count(string(output), "allocations to reduce"); measured != 1 {
			t.Fatalf("the run of this test alone reported %d measurements; want 1", measured)
		}
		return
	}
	// The registration reduces the topics it plans, which are the body's and the session's own
	// agent subject (subscribeHandler).
	subjects := append(largestProductionShape("legion-controller"), contracts.LegacyAgentSubject(largestShapeHolder))
	var filters []string
	allocs := testing.AllocsPerRun(1, func() { filters = bus.SessionConsumerFilters(subjects) })
	t.Logf("%v allocations to reduce %d topics to %d filters, %.2f a topic", allocs, len(subjects), len(filters), allocs/float64(len(subjects)))
	if bound := float64(reduceAllocsPerSubject * len(subjects)); allocs > bound {
		t.Fatalf("reducing %d topics to their consumer's filters made %v allocations, more than %v (%d a topic): the reduction allocates for more than each topic's forms and one split of each, as INFRA-1555's split of every pair it weighed did", len(subjects), allocs, bound, reduceAllocsPerSubject)
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
