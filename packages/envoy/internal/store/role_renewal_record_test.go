// The renewal reads its set from the interest record the registration's own write returned and
// from the role claim copy, and renews each role only as its claim record says; and the renewals
// that fail are recorded, by role and holder session, until one of them writes
// (Registry.RenewRoleClaims, Registry.RecordRenewalFailures, Registry.RenewalFailedWithin). The
// last two tests hold every act of the store that turns on who holds a role to that role's claim
// record: the renewal, the release, and the controller notice lane a registration keeps.

package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// errClaimReadFailed and errClaimWriteFailed are the answers a countedKeyValue that fails gives a
// read and a write, as a bus that does not answer one does.
var (
	errClaimReadFailed  = errors.New("the role bucket did not answer the read")
	errClaimWriteFailed = errors.New("the role bucket did not answer the write")
)

// countedKeyValue is a bucket's legacy handle with every Get counted, and every Get or Update
// failing while the test says so. The count is what says which reads an act made of the bucket
// it wraps, so it wraps the handle the registry reads through and nothing else. bus.Bounded runs
// each call on a goroutine of its own, so the count and the switches are atomic.
type countedKeyValue struct {
	natsgo.KeyValue

	gets        atomic.Int64
	failGets    atomic.Bool
	failUpdates atomic.Bool
}

func (kv *countedKeyValue) Get(key string) (natsgo.KeyValueEntry, error) {
	kv.gets.Add(1)
	if kv.failGets.Load() {
		return nil, errClaimReadFailed
	}
	return kv.KeyValue.Get(key)
}

func (kv *countedKeyValue) Update(key string, value []byte, revision uint64) (uint64, error) {
	if kv.failUpdates.Load() {
		return 0, errClaimWriteFailed
	}
	return kv.KeyValue.Update(key, value, revision)
}

// reads is how many Gets the bucket has taken through this handle.
func (kv *countedKeyValue) reads() int64 { return kv.gets.Load() }

// countRoleReads puts a countedKeyValue in front of the registry's role bucket handle, the one
// every claim read and write of the registry goes through (Registry.roles); the role claim
// watcher reads through a handle of its own, so the copy goes on following the bucket.
func countRoleReads(reg *Registry) *countedKeyValue {
	reg.kvMu.Lock()
	defer reg.kvMu.Unlock()
	counted := &countedKeyValue{KeyValue: reg.roleKV.Legacy()}
	reg.roleKV = bus.NewKeyValue(counted, reg.roleKV.WatchHandle(), reg.roleKV.Streams())
	return counted
}

// countInterestReads puts a countedKeyValue in front of the registry's interest bucket handle,
// rebuilding its interest watcher over it (useKV), so every read of an interest record the
// registry makes is counted.
func countInterestReads(t *testing.T, reg *Registry) *countedKeyValue {
	t.Helper()
	counted := &countedKeyValue{KeyValue: reg.watcher.KV().Legacy()}
	useKV(t, reg, counted)
	return counted
}

// stopInterestCache stops the registry's interest watcher, so its cache keeps the rows it holds
// while the bucket moves on: what a listener's cache is between another listener's write and its
// delivery. The registry's own reads and writes go on through the same handle, at the same epoch,
// and each still caches what it read or wrote (Registry.getWithRevision, recordInterest).
func stopInterestCache(reg *Registry) {
	reg.watcher.Stop()
}

// seedStaleInterest makes the registry's interest cache hold sessionID's bucket row without
// topic, at the very revision the bucket holds the row at (kvwatch.Copy.Put takes an equal
// revision), so a write that merged over the cache would pass its compare-and-swap and store the
// row without topic. It returns the row it cached. The interest watcher must be stopped
// (stopInterestCache): a delivery of that revision would otherwise put the bucket's row back.
func seedStaleInterest(t *testing.T, rig *repairRig, sessionID, topic string) Interest {
	t.Helper()
	entry, err := rig.interests.Get(sessionID)
	if err != nil {
		t.Fatalf("read %s's interest record: %v", sessionID, err)
	}
	var row Interest
	if err := json.Unmarshal(entry.Value(), &row); err != nil {
		t.Fatalf("decode %s's interest record: %v", sessionID, err)
	}
	row.Topics = slices.DeleteFunc(slices.Clone(row.Topics), func(stored string) bool { return stored == topic })
	rig.reg.mu.Lock()
	_, epoch := rig.reg.watcher.Handle()
	outcome := rig.reg.cache.Put(rig.reg.watcher, epoch, sessionID, row, entry.Revision())
	rig.reg.mu.Unlock()
	if outcome != kvwatch.Applied {
		t.Fatalf("the cache refused %s's row at revision %d (outcome %d)", sessionID, entry.Revision(), outcome)
	}
	return row
}

// settle runs the armed scheduled read of the role claims and returns once it has completed, so
// the copy holds what the bucket holds and no delivery of a claim written before it can still
// arrive: a test that then takes a claim out of the copy, or puts one in, is not undone by a
// record the watcher had not yet handed over. It moves the registry's clock to the read's moment.
func settle(rig *repairRig) { rig.repair() }

// beat moves the registry's clock one heartbeat on and returns the moment it moved to, which a
// renewal then stamps on the claim it writes.
func beat(rig *repairRig) time.Time {
	at := rig.clock.Now().Add(contracts.SessionHeartbeat)
	rig.clock.Set(at)
	return at
}

// registered is one registration's interest write of sessionID for topics (UpsertWithin), as the
// route's own write returns it, which is the row it hands RenewRoleClaims.
func registered(t *testing.T, reg *Registry, sessionID string, topics ...string) Interest {
	t.Helper()
	row, err := reg.UpsertWithin(context.Background(), Interest{SessionID: sessionID, MachineID: "m1"}, topics)
	if err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	return row
}

// controllerRegistration is one registration of sessionID naming topics as the listener's route
// makes its interest half: the plan (PlanInterest), whose topics are the filter of the session's
// consumer, and the write of that plan (UpsertPlanned). It returns the three topic sets that
// registration left, by name: the planned ones, the ones the write returned, and the ones of the
// row the interest bucket now holds.
func controllerRegistration(t *testing.T, rig *repairRig, sessionID string, topics ...string) map[string][]string {
	t.Helper()
	ctx := context.Background()
	plan, err := rig.reg.PlanInterest(ctx, sessionID, topics)
	if err != nil {
		t.Fatalf("plan %s's registration of %v: %v", sessionID, topics, err)
	}
	row, err := rig.reg.UpsertPlanned(ctx, Interest{SessionID: sessionID, MachineID: "m1"}, plan)
	if err != nil {
		t.Fatalf("write %s's registration of %v: %v", sessionID, topics, err)
	}
	return map[string][]string{"planned": plan.Topics, "written": row.Topics, "stored": storedInterest(t, rig.conn, sessionID).Topics}
}

// requireLane fails the test, saying why, unless every topic set of a registration
// (controllerRegistration) holds lane when kept is set, and none of them does when it is not.
func requireLane(t *testing.T, sets map[string][]string, lane string, kept bool, why string) {
	t.Helper()
	want := "without"
	if kept {
		want = "with"
	}
	for name, topics := range sets {
		if slices.Contains(topics, lane) != kept {
			t.Fatalf("the %s topics are %v, want them %s %s: %s", name, topics, want, lane, why)
		}
	}
}

// assertNoRenewalFailure fails the test if a renewal failure is recorded of role's claim for
// sessionID inside the live window.
func assertNoRenewalFailure(t *testing.T, reg *Registry, role, sessionID string) {
	t.Helper()
	if failure, failed := reg.RenewalFailedWithin(role, sessionID, contracts.SessionLiveWindow); failed {
		t.Fatalf("a renewal failure of %s's claim for %s is recorded at %s (%v), want none", role, sessionID, failure.At, failure.Err)
	}
}

// requireRenewalFailure fails the test unless a renewal failure of role's claim for sessionID is
// recorded at at, with an error that is cause and names role, and returns it.
func requireRenewalFailure(t *testing.T, reg *Registry, role, sessionID string, at time.Time, cause error) RenewalFailure {
	t.Helper()
	failure, failed := reg.RenewalFailedWithin(role, sessionID, contracts.SessionLiveWindow)
	if !failed {
		t.Fatalf("no renewal failure of %s's claim for %s is recorded, want one at %s", role, sessionID, at)
	}
	if !failure.At.Equal(at) || !errors.Is(failure.Err, cause) || !strings.Contains(failure.Err.Error(), role) {
		t.Fatalf("the renewal failure of %s's claim for %s is recorded at %s as %v, want it at %s as %v, naming the role", role, sessionID, failure.At, failure.Err, at, cause)
	}
	return failure
}

// A claim the copy lost is renewed by the next registration, from the interest record that
// registration's own write returned, with no read of the role claims completing in between: the
// record names the role's lane topic (the grant wrote it there), and the claim record names the
// registering process. The control is the set from the copy alone - a record that names no role -
// which renews nothing, because the copy no longer names the claim.
func TestARenewRenewsAClaimTheCopyLostFromTheRecordItIsGiven(t *testing.T) {
	const (
		role   = "planner"
		holder = "ses_renew_from_record"
	)
	own := contracts.AgentTopicPrefix + holder
	rig := newRepairRig(t, nil)
	claimRoleAs(t, rig.reg, holder, "m1", role, roleClaimInstance)
	settle(rig)
	loseRoleClaim(rig.reg, role)
	if got := heldRoles(t, rig.reg, holder); len(got) != 0 {
		t.Fatalf("with its claim taken out of the copy, RolesHeldBy(%s) = %v, want nothing", holder, got)
	}
	claimed := storedRoleClaim(t, rig.reg, role)
	reads := rig.reg.RoleClaimRepair().Completed
	at := beat(rig)

	control, err := rig.reg.RenewRoleClaims(context.Background(), holder, roleClaimInstance, Interest{SessionID: holder})
	if err != nil {
		t.Fatalf("the renewal from the copy alone: %v", err)
	}
	if len(control.Set) != 0 || len(control.Renewed) != 0 {
		t.Fatalf("the renewal from the copy alone weighed %v and renewed %v, want nothing: the copy lost the claim", control.Set, control.Renewed)
	}
	if stored := storedRoleClaim(t, rig.reg, role); stored.RenewedAt != claimed.RenewedAt {
		t.Fatalf("the renewal from the copy alone renewed the claim at %d, want it left at %d", stored.RenewedAt, claimed.RenewedAt)
	}

	row := registered(t, rig.reg, holder, own)
	renewal, err := rig.reg.RenewRoleClaims(context.Background(), holder, roleClaimInstance, row)
	if err != nil {
		t.Fatalf("renew %s's role claims: %v", holder, err)
	}
	renewed := storedRoleClaim(t, rig.reg, role)
	t.Logf("the claim was renewed at %d before the registration and at %d after it (the registration's moment is %d)", claimed.RenewedAt, renewed.RenewedAt, at.UnixMilli())
	if !slices.Equal(renewal.Set, []string{role}) || !slices.Equal(renewal.Renewed, []string{role}) {
		t.Fatalf("the renewal weighed %v and renewed %v, want [%s] for both: the record it was given names the role", renewal.Set, renewal.Renewed, role)
	}
	if renewed.RenewedAt != at.UnixMilli() {
		t.Fatalf("the claim is renewed at %d, want the registration's moment %d", renewed.RenewedAt, at.UnixMilli())
	}
	if got := rig.reg.RoleClaimRepair().Completed; got != reads {
		t.Fatalf("%d reads of the role claims completed before the renewal and %d after it, want none between: the renewal must not wait on the repair", reads, got)
	}
}

// The store half of c-set-read-from-the-store. The interest cache holds the session's row from
// before another listener's grant, without the role's lane topic, while the bucket's row has it,
// and the copy lost the claim. The registration's store calls run as registerSession makes them on
// a conflict - the plan from the cache, its write refused, the plan again from the bucket and its
// write - and the renewal is handed the row that write returned. The registration reads the
// interest bucket three times and the renewal not at all, and the claim is renewed.
func TestTheSetReadFromTheStoreRenewsARoleAStaleInterestCacheHides(t *testing.T) {
	const (
		role   = "reviewer"
		holder = "ses_stale_interest_cache"
	)
	own := contracts.AgentTopicPrefix + holder
	roleTopic := contracts.RoleTopicPrefix + role
	ctx := context.Background()
	rig := newRepairRig(t, nil)
	interests := countInterestReads(t, rig.reg)
	registered(t, rig.reg, holder, own)
	stopInterestCache(rig.reg)

	// Another listener grants the role: the claim and the lane topic in the bucket's row.
	other, err := Open(rig.conn, WithReplicas(1), WithClock(rig.clock.Now))
	if err != nil {
		t.Fatalf("open the other listener's registry: %v", err)
	}
	t.Cleanup(other.StopWatch)
	ready, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := other.WaitForCacheReady(ready); err != nil {
		t.Fatalf("WaitForCacheReady on the other listener's registry: %v", err)
	}
	claimRoleAs(t, other, holder, "m2", role, roleClaimInstance)
	waitForHeld(t, rig.reg, holder, role)
	settle(rig)
	loseRoleClaim(rig.reg, role)
	if got := heldRoles(t, rig.reg, holder); len(got) != 0 {
		t.Fatalf("with its claim taken out of the copy, RolesHeldBy(%s) = %v, want nothing", holder, got)
	}
	cached, _ := rig.reg.Cached(holder)
	stored := storedInterest(t, rig.conn, holder)
	t.Logf("the cache holds %s's topics %v and the bucket %v", holder, cached.Topics, stored.Topics)
	if slices.Contains(cached.Topics, roleTopic) || !slices.Contains(stored.Topics, roleTopic) {
		t.Fatalf("the cache holds %v and the bucket %v, want the lane topic %s in the bucket's row alone", cached.Topics, stored.Topics, roleTopic)
	}
	claimed := storedRoleClaim(t, rig.reg, role)
	at := beat(rig)

	start := interests.reads()
	item := Interest{SessionID: holder, MachineID: "m1"}
	plan, err := rig.reg.PlanInterest(ctx, holder, []string{own})
	if err != nil {
		t.Fatalf("plan the registration: %v", err)
	}
	if slices.Contains(plan.Topics, roleTopic) {
		t.Fatalf("the first plan holds %v, want it made from the cache, which lacks %s", plan.Topics, roleTopic)
	}
	if _, err := rig.reg.UpsertPlanned(ctx, item, plan); !errors.Is(err, ErrInterestPlanConflict) {
		t.Fatalf("the write of the plan made from the cache answered %v, want ErrInterestPlanConflict", err)
	}
	if plan, err = rig.reg.PlanInterestAfterConflict(ctx, holder, []string{own}); err != nil {
		t.Fatalf("plan the registration again: %v", err)
	}
	row, err := rig.reg.UpsertPlanned(ctx, item, plan)
	if err != nil {
		t.Fatalf("write the registration's second plan: %v", err)
	}
	registrationReads := interests.reads() - start

	renewal, err := rig.reg.RenewRoleClaims(ctx, holder, roleClaimInstance, row)
	if err != nil {
		t.Fatalf("renew %s's role claims: %v", holder, err)
	}
	renewalReads := interests.reads() - start - registrationReads
	t.Logf("interest bucket reads: the registration's %d, the renewal's %d", registrationReads, renewalReads)
	if registrationReads != 3 {
		t.Fatalf("the registration read the interest bucket %d times, want 3: the refused write's, the second plan's and its write's", registrationReads)
	}
	if renewalReads != 0 {
		t.Fatalf("the renewal read the interest bucket %d times, want none: its set is the row it was handed", renewalReads)
	}
	renewed := storedRoleClaim(t, rig.reg, role)
	t.Logf("the claim was renewed at %d before the registration and at %d after it", claimed.RenewedAt, renewed.RenewedAt)
	if !slices.Equal(renewal.Renewed, []string{role}) || renewed.RenewedAt != at.UnixMilli() {
		t.Fatalf("the renewal renewed %v and left the claim renewed at %d, want [%s] renewed at %d", renewal.Renewed, renewed.RenewedAt, role, at.UnixMilli())
	}
}

// c-merge-reads-the-record. Each write that merges a registration into the session's row merges
// it into the row the bucket holds, read for that write, and not into the cached row: the cache
// is seeded with the row less the role's lane topic at the bucket's very revision, so a merge over
// the cache would pass the compare-and-swap and store the row without the topic. UpsertPlanned
// and UpsertWithin each write a topic of their own, and the bucket's row still carries the lane
// topic after each.
func TestTheMergeReadsTheRecordAndKeepsARoleTopicAStaleCacheLacks(t *testing.T) {
	const (
		role   = "merge-role"
		holder = "ses_merge_reads_record"
	)
	own := contracts.AgentTopicPrefix + holder
	roleTopic := contracts.RoleTopicPrefix + role
	ctx := context.Background()
	item := Interest{SessionID: holder, MachineID: "m1"}
	rig := newRepairRig(t, nil)
	claimRoleAs(t, rig.reg, holder, "m1", role, roleClaimInstance)
	stopInterestCache(rig.reg)

	writes := []struct {
		name  string
		topic string
		write func(topic string) error
	}{
		{name: "UpsertPlanned", topic: "notifications.team.merge-planned", write: func(topic string) error {
			plan, err := rig.reg.PlanInterest(ctx, holder, []string{own, topic})
			if err != nil {
				return err
			}
			_, err = rig.reg.UpsertPlanned(ctx, item, plan)
			return err
		}},
		{name: "UpsertWithin", topic: "notifications.team.merge-within", write: func(topic string) error {
			_, err := rig.reg.UpsertWithin(ctx, item, []string{topic})
			return err
		}},
	}
	for _, write := range writes {
		cached := seedStaleInterest(t, rig, holder, roleTopic)
		before := storedInterest(t, rig.conn, holder)
		t.Logf("before %s: the cache holds %v and the bucket %v", write.name, cached.Topics, before.Topics)
		if err := write.write(write.topic); err != nil {
			t.Fatalf("%s: %v", write.name, err)
		}
		after := storedInterest(t, rig.conn, holder)
		t.Logf("after %s: the bucket holds %v", write.name, after.Topics)
		if !slices.Contains(after.Topics, write.topic) {
			t.Fatalf("after %s the bucket's row holds %v, want the write's own topic %s: the write did not land", write.name, after.Topics, write.topic)
		}
		if !slices.Contains(after.Topics, roleTopic) {
			t.Fatalf("after %s the bucket's row holds %v, want %s kept: the write merged over the cached row", write.name, after.Topics, roleTopic)
		}
	}
}

// A renewal whose claim read failed and one whose write failed each record a failure against the
// role and the session; a renewal that writes nothing because the claim is another process's
// leaves the record standing; and the next renewal that writes clears it. A role the record names
// with no claim record is weighed and recorded as no failure.
func TestARenewalThatLandsClearsItsFailureRecord(t *testing.T) {
	const (
		role      = "landing-role"
		unclaimed = "unclaimed-role"
		holder    = "ses_renewal_lands"
	)
	ctx := context.Background()
	rig := newRepairRig(t, nil)
	claimRoleAs(t, rig.reg, holder, "m1", role, roleClaimInstance)
	row := registered(t, rig.reg, holder)
	roles := countRoleReads(rig.reg)

	roles.failGets.Store(true)
	readFailedAt := beat(rig)
	if _, err := rig.reg.RenewRoleClaims(ctx, holder, roleClaimInstance, row); !errors.Is(err, errClaimReadFailed) {
		t.Fatalf("a renewal whose claim read failed answered %v, want %v", err, errClaimReadFailed)
	}
	requireRenewalFailure(t, rig.reg, role, holder, readFailedAt, errClaimReadFailed)
	roles.failGets.Store(false)

	roles.failUpdates.Store(true)
	writeFailedAt := beat(rig)
	if _, err := rig.reg.RenewRoleClaims(ctx, holder, roleClaimInstance, row); !errors.Is(err, errClaimWriteFailed) {
		t.Fatalf("a renewal whose write failed answered %v, want %v", err, errClaimWriteFailed)
	}
	requireRenewalFailure(t, rig.reg, role, holder, writeFailedAt, errClaimWriteFailed)
	roles.failUpdates.Store(false)

	beat(rig)
	stood, err := rig.reg.RenewRoleClaims(ctx, holder, roleClaimSecondInstance, row)
	if err != nil || len(stood.Renewed) != 0 {
		t.Fatalf("a renewal by another process renewed %v with %v, want nothing and no error", stood.Renewed, err)
	}
	requireRenewalFailure(t, rig.reg, role, holder, writeFailedAt, errClaimWriteFailed)

	landedAt := beat(rig)
	withUnclaimed := row
	withUnclaimed.Topics = append(slices.Clone(row.Topics), contracts.RoleTopicPrefix+unclaimed)
	landed, err := rig.reg.RenewRoleClaims(ctx, holder, roleClaimInstance, withUnclaimed)
	if err != nil {
		t.Fatalf("a renewal with reads and writes answering: %v", err)
	}
	if !slices.Equal(landed.Set, []string{role, unclaimed}) || !slices.Equal(landed.Renewed, []string{role}) {
		t.Fatalf("the renewal weighed %v and renewed %v, want [%s %s] weighed and [%s] renewed", landed.Set, landed.Renewed, role, unclaimed, role)
	}
	if renewed := storedRoleClaim(t, rig.reg, role); renewed.RenewedAt != landedAt.UnixMilli() {
		t.Fatalf("the claim is renewed at %d, want %d", renewed.RenewedAt, landedAt.UnixMilli())
	}
	assertNoRenewalFailure(t, rig.reg, role, holder)
	assertNoRenewalFailure(t, rig.reg, unclaimed, holder)
}

// The failure record is keyed by role and holder session: a registration whose store work failed
// records a failure for the roles the copy names for its session and the role topics of its
// cached interest row, reading neither bucket, and for no other session or role. An entry as old
// as the live window is pruned at the role claim reaper's pass, which is handed that window, and
// at each write of the record once a pass has handed it one; before the first pass a write prunes
// nothing, since the record does not know the deployment's window yet.
func TestARenewalFailureRecordIsKeyedByHolderAndPrunedPastTheLiveWindow(t *testing.T) {
	const (
		holder    = "ses_failed_registration"
		other     = "ses_other_registration"
		both      = "both-role"
		lostRole  = "row-only-role"
		copyRole  = "copy-only-role"
		otherRole = "other-role"
		window    = contracts.SessionLiveWindow
	)
	ctx := context.Background()
	alive := func(string) bool { return true }
	cause := errors.New("the interest bucket did not answer the registration's read")
	rig := newRepairRig(t, nil)
	claimRoleAs(t, rig.reg, holder, "m1", both, roleClaimInstance)
	claimRoleAs(t, rig.reg, holder, "m1", lostRole, roleClaimInstance)
	putRoleClaim(t, rig.roles, copyRole, holder)
	registered(t, rig.reg, other, contracts.RoleTopicPrefix+otherRole)
	waitForHeld(t, rig.reg, holder, both, copyRole, lostRole)
	settle(rig)
	loseRoleClaim(rig.reg, lostRole)
	cached, _ := rig.reg.Cached(holder)
	t.Logf("the copy gives %s %v and its cached row holds %v", holder, heldRoles(t, rig.reg, holder), cached.Topics)
	interests := countInterestReads(t, rig.reg)
	roles := countRoleReads(rig.reg)

	recorded := rig.clock.Now()
	rig.reg.RecordRenewalFailures(holder, cause)
	if got := interests.reads() + roles.reads(); got != 0 {
		t.Fatalf("recording the registration's failures read the buckets %d times, want none", got)
	}
	for _, role := range []string{both, lostRole, copyRole} {
		requireRenewalFailure(t, rig.reg, role, holder, recorded, cause)
	}
	assertNoRenewalFailure(t, rig.reg, both, other)
	assertNoRenewalFailure(t, rig.reg, otherRole, holder)
	assertNoRenewalFailure(t, rig.reg, otherRole, other)

	// Before any reaper pass a write of the record prunes nothing.
	rig.clock.Set(recorded.Add(2 * window))
	otherRecorded := rig.clock.Now()
	rig.reg.RecordRenewalFailures(other, cause)
	requireRenewalFailure(t, rig.reg, otherRole, other, otherRecorded, cause)
	if _, kept := rig.reg.RenewalFailedWithin(both, holder, 3*window); !kept {
		t.Fatalf("a write of the record before any reaper pass pruned %s's failure %s old", holder, 2*window)
	}

	// The reaper's pass prunes what is as old as its window, and keeps what is younger.
	if _, err := rig.reg.ReapRoleClaims(ctx, alive, 0, window); err != nil {
		t.Fatalf("the role claim reaper's pass: %v", err)
	}
	for _, role := range []string{both, lostRole, copyRole} {
		if failure, kept := rig.reg.RenewalFailedWithin(role, holder, 3*window); kept {
			t.Fatalf("the reaper's pass kept %s's failure of %s from %s, %s old: want it pruned at the live window %s", holder, role, failure.At, 2*window, window)
		}
	}
	rig.clock.Set(otherRecorded.Add(window - time.Millisecond))
	if _, err := rig.reg.ReapRoleClaims(ctx, alive, 0, window); err != nil {
		t.Fatalf("the role claim reaper's second pass: %v", err)
	}
	if _, kept := rig.reg.RenewalFailedWithin(otherRole, other, 2*window); !kept {
		t.Fatalf("the reaper's pass pruned %s's failure younger than the live window", other)
	}

	// Once a pass has handed the record its window, each write prunes at it.
	rig.clock.Set(otherRecorded.Add(window))
	rewritten := rig.clock.Now()
	rig.reg.RecordRenewalFailures(holder, cause)
	requireRenewalFailure(t, rig.reg, both, holder, rewritten, cause)
	if failure, kept := rig.reg.RenewalFailedWithin(otherRole, other, 2*window); kept {
		t.Fatalf("a write of the record kept %s's failure from %s, a live window old: want it pruned", other, failure.At)
	}
}

// Every act that turns on who holds a role reads that role's claim record, in both directions the
// copy can be wrong in: a surplus copy names the session where the store names another session or
// none, and a lost copy lacks a claim the store gives the session. Each act is required to do what
// the store's claim says: the renewal, the release, and the registration's controller notice lane.
func TestEachActReadsTheRecordInBothDirections(t *testing.T) {
	t.Run("renewal", func(t *testing.T) {
		const (
			role   = "direction-role"
			holder = "ses_direction_holder"
			other  = "ses_direction_other"
		)
		own := contracts.AgentTopicPrefix + holder

		// The copy names the session for a role whose claim names another session, or which no
		// claim record holds, and the session's record names no such role: the copy alone puts
		// the role in the set, and the claim record leaves it unrenewed.
		t.Run("surplus", func(t *testing.T) {
			for name, storeHolder := range map[string]string{"another session": other, "no claim": ""} {
				t.Run(name, func(t *testing.T) {
					rig := newRepairRig(t, nil)
					revision := uint64(1)
					if storeHolder != "" {
						claimRoleAs(t, rig.reg, storeHolder, "m1", role, roleClaimInstance)
					}
					settle(rig)
					claimed := storedRoleClaim(t, rig.reg, role)
					if claimed.Revision > 0 {
						revision = claimed.Revision + 1
					}
					rig.reg.applyRoleClaim(roleClaimEntry(role, holder, revision))
					if got := heldRoles(t, rig.reg, holder); !slices.Equal(got, []string{role}) {
						t.Fatalf("with the surplus claim in the copy, RolesHeldBy(%s) = %v, want [%s]", holder, got, role)
					}
					row := registered(t, rig.reg, holder, own)
					beat(rig)

					renewal, err := rig.reg.RenewRoleClaims(context.Background(), holder, roleClaimInstance, row)
					if err != nil {
						t.Fatalf("renew %s's role claims: %v", holder, err)
					}
					if !slices.Equal(renewal.Set, []string{role}) || len(renewal.Renewed) != 0 {
						t.Fatalf("the renewal weighed %v and renewed %v, want [%s] weighed and nothing renewed: the store gives the role to %q", renewal.Set, renewal.Renewed, role, storeHolder)
					}
					if stored := storedRoleClaim(t, rig.reg, role); stored != claimed {
						t.Fatalf("the claim is now %+v, want it as the store held it, %+v", stored, claimed)
					}
					assertNoRenewalFailure(t, rig.reg, role, holder)
				})
			}
		})

		// The store gives the session the role, the copy lost it, and the record the
		// registration's write returned names the role's lane: the claim is renewed.
		t.Run("lost", func(t *testing.T) {
			rig := newRepairRig(t, nil)
			claimRoleAs(t, rig.reg, holder, "m1", role, roleClaimInstance)
			settle(rig)
			loseRoleClaim(rig.reg, role)
			if got := heldRoles(t, rig.reg, holder); len(got) != 0 {
				t.Fatalf("with its claim taken out of the copy, RolesHeldBy(%s) = %v, want nothing", holder, got)
			}
			row := registered(t, rig.reg, holder, own)
			at := beat(rig)

			renewal, err := rig.reg.RenewRoleClaims(context.Background(), holder, roleClaimInstance, row)
			if err != nil {
				t.Fatalf("renew %s's role claims: %v", holder, err)
			}
			if !slices.Equal(renewal.Renewed, []string{role}) {
				t.Fatalf("the renewal weighed %v and renewed %v, want [%s] renewed: the store gives the session the role", renewal.Set, renewal.Renewed, role)
			}
			if renewed := storedRoleClaim(t, rig.reg, role); renewed.RenewedAt != at.UnixMilli() {
				t.Fatalf("the claim is renewed at %d, want the registration's moment %d", renewed.RenewedAt, at.UnixMilli())
			}
		})
	})

	t.Run("release", func(t *testing.T) {
		const (
			role   = "direction-release-role"
			holder = "ses_direction_releaser"
			other  = "ses_direction_release_other"
		)
		own := contracts.AgentTopicPrefix + holder
		lane := contracts.RoleTopicPrefix + role

		// The copy names the session for a role whose claim names another session, or which no
		// claim record holds, and the session's record names no such role: the copy alone puts
		// the role in the deregistration's set, its claim is read once, and the claim record
		// leaves it as the store holds it.
		t.Run("surplus", func(t *testing.T) {
			for name, storeHolder := range map[string]string{"another session": other, "no claim": ""} {
				t.Run(name, func(t *testing.T) {
					rig := newRepairRig(t, nil)
					revision := uint64(1)
					if storeHolder != "" {
						claimRoleAs(t, rig.reg, storeHolder, "m1", role, roleClaimInstance)
					}
					settle(rig)
					claimed := storedRoleClaim(t, rig.reg, role)
					if claimed.Revision > 0 {
						revision = claimed.Revision + 1
					}
					rig.reg.applyRoleClaim(roleClaimEntry(role, holder, revision))
					if got := heldRoles(t, rig.reg, holder); !slices.Equal(got, []string{role}) {
						t.Fatalf("with the surplus claim in the copy, RolesHeldBy(%s) = %v, want [%s]", holder, got, role)
					}
					registered(t, rig.reg, holder, own)
					claims := countRoleReads(rig.reg)

					if _, err := rig.reg.Remove(context.Background(), holder, nil, releasedByItsProcess); err != nil {
						t.Fatalf("deregister %s: %v", holder, err)
					}
					if reads := claims.reads(); reads != 1 {
						t.Fatalf("the deregistration read %s's claim %d times, want once: the copy names it, and the claim record decides it", role, reads)
					}
					if stored := storedRoleClaim(t, rig.reg, role); stored != claimed {
						t.Fatalf("the claim is now %+v, want it as the store held it, %+v: the store gives the role to %q", stored, claimed, storeHolder)
					}
					assertNoInterestRecord(t, rig, holder)
				})
			}
		})

		// The store gives the session the role and the copy lost it, and the record in the bucket
		// names the role's lane: the claim is released. The interest cache is the second copy the
		// deregistration does not read its record from, so the case runs again with that cache
		// holding the session's row without the lane topic.
		t.Run("lost", func(t *testing.T) {
			for name, staleCache := range map[string]bool{"the role claim copy": false, "the role claim copy and the interest cache": true} {
				t.Run(name, func(t *testing.T) {
					rig := newRepairRig(t, nil)
					claimRoleAs(t, rig.reg, holder, "m1", role, roleClaimInstance)
					settle(rig)
					loseRoleClaim(rig.reg, role)
					if got := heldRoles(t, rig.reg, holder); len(got) != 0 {
						t.Fatalf("with its claim taken out of the copy, RolesHeldBy(%s) = %v, want nothing", holder, got)
					}
					if staleCache {
						stopInterestCache(rig.reg)
						if cached := seedStaleInterest(t, rig, holder, lane); slices.Contains(cached.Topics, lane) {
							t.Fatalf("the interest cache holds %s's row with %s, want it without", holder, lane)
						}
					}

					if _, err := rig.reg.Remove(context.Background(), holder, nil, releasedByItsProcess); err != nil {
						t.Fatalf("deregister %s: %v", holder, err)
					}
					if stored := storedRoleClaim(t, rig.reg, role); stored.Revision != 0 {
						t.Fatalf("the claim is now %+v, want it released: the store gave the session the role", stored)
					}
					assertNoInterestRecord(t, rig, holder)
				})
			}
		})
	})

	// A registration keeps a Legion controller notice lane in its plan, which is the filter of the
	// session's consumer, and in the row its write stores exactly while the project's controller
	// claim record names the session (controllerLaneRoles), and it reads that record once.
	t.Run("controller lane", func(t *testing.T) {
		const (
			role   = "legion-DIRECTION-controller"
			lane   = "notifications.legion.DIRECTION.controller"
			holder = "ses_direction_controller"
			other  = "ses_direction_controller_other"
		)
		own := contracts.AgentTopicPrefix + holder

		// The copy names the session for a claim the store gives another session, or no session:
		// no record, or a record this build cannot decode, which names no holder. The session's
		// stored row holds the lane, as a registration made while it held the role left it, and
		// the registration names the lane again: the lane leaves both.
		t.Run("surplus", func(t *testing.T) {
			for name, seed := range map[string]func(t *testing.T, rig *repairRig) uint64{
				"another session": func(t *testing.T, rig *repairRig) uint64 {
					claimRoleAs(t, rig.reg, other, "m1", role, roleClaimInstance)
					return storedRoleClaim(t, rig.reg, role).Revision
				},
				"no claim": func(*testing.T, *repairRig) uint64 { return 0 },
				"a claim this build cannot decode": func(t *testing.T, rig *repairRig) uint64 {
					revision, err := rig.roles.Put(role, []byte(`{"holder_session_id":""}`))
					if err != nil {
						t.Fatalf("store a claim of %s naming no holder: %v", role, err)
					}
					return revision
				},
			} {
				t.Run(name, func(t *testing.T) {
					rig := newRepairRig(t, nil)
					revision := seed(t, rig) + 1
					putInterest(t, rig.interests, Interest{SessionID: holder, MachineID: "m1", Topics: []string{own, lane}})
					settle(rig)
					rig.reg.applyRoleClaim(roleClaimEntry(role, holder, revision))
					if got := heldRoles(t, rig.reg, holder); !slices.Equal(got, []string{role}) {
						t.Fatalf("with the surplus claim in the copy, RolesHeldBy(%s) = %v, want [%s]", holder, got, role)
					}
					claims := countRoleReads(rig.reg)

					requireLane(t, controllerRegistration(t, rig, holder, lane, own), lane, false, "the store's claim of "+role+" ("+name+") does not name "+holder)
					if reads := claims.reads(); reads != 1 {
						t.Fatalf("the registration read %s's claim %d times, want once for the lane its body and its row both name", role, reads)
					}
				})
			}
		})

		// The store gives the session the role and the copy lost the claim: the registration that
		// names the lane keeps it, and so does the heartbeat after it that leaves the lane out of
		// its body, which reads the claim for the lane its row holds.
		t.Run("lost", func(t *testing.T) {
			rig := newRepairRig(t, nil)
			claimRoleAs(t, rig.reg, holder, "m1", role, roleClaimInstance)
			settle(rig)
			loseRoleClaim(rig.reg, role)
			if got := heldRoles(t, rig.reg, holder); len(got) != 0 {
				t.Fatalf("with its claim taken out of the copy, RolesHeldBy(%s) = %v, want nothing", holder, got)
			}
			claims := countRoleReads(rig.reg)

			requireLane(t, controllerRegistration(t, rig, holder, lane, own), lane, true, "the store gives "+role+" to "+holder)
			requireLane(t, controllerRegistration(t, rig, holder, own), lane, true, "the heartbeat's row holds the lane of a role the store gives "+holder)
			if reads := claims.reads(); reads != 2 {
				t.Fatalf("the registration and the heartbeat read %s's claim %d times, want once each", role, reads)
			}
		})
	})
}

// An act whose claim read fails does not act as if the read had answered: it holds, says so, and
// leaves the claim as the store holds it. The control of each is the same drive with the read
// working, which acts. The acts are the renewal, the release, and the registration's controller
// notice lane.
func TestTheActHoldsOnAFailedRead(t *testing.T) {
	t.Run("renewal", func(t *testing.T) {
		const (
			role   = "held-role"
			holder = "ses_held_on_failed_read"
		)
		for name, fails := range map[string]bool{"failed read": true, "reads working": false} {
			t.Run(name, func(t *testing.T) {
				rig := newRepairRig(t, nil)
				claimRoleAs(t, rig.reg, holder, "m1", role, roleClaimInstance)
				row := registered(t, rig.reg, holder)
				claimed := storedRoleClaim(t, rig.reg, role)
				roles := countRoleReads(rig.reg)
				roles.failGets.Store(fails)
				at := beat(rig)

				renewal, err := rig.reg.RenewRoleClaims(context.Background(), holder, roleClaimInstance, row)
				roles.failGets.Store(false)
				stored := storedRoleClaim(t, rig.reg, role)
				if !fails {
					if err != nil || !slices.Equal(renewal.Renewed, []string{role}) || stored.RenewedAt != at.UnixMilli() {
						t.Fatalf("with the claim read working the renewal renewed %v (claim renewed at %d, want %d) with %v, want [%s] and no error",
							renewal.Renewed, stored.RenewedAt, at.UnixMilli(), err, role)
					}
					assertNoRenewalFailure(t, rig.reg, role, holder)
					return
				}
				if err == nil || !errors.Is(err, errClaimReadFailed) || !strings.Contains(err.Error(), role) {
					t.Fatalf("the renewal whose claim read failed answered %v, want an error naming %s and carrying %v", err, role, errClaimReadFailed)
				}
				if len(renewal.Renewed) != 0 || stored != claimed {
					t.Fatalf("the renewal whose claim read failed renewed %v and left the claim %+v, want nothing renewed and the claim %+v", renewal.Renewed, stored, claimed)
				}
				requireRenewalFailure(t, rig.reg, role, holder, at, errClaimReadFailed)
			})
		}
	})

	// A deregistration whose claim read fails frees nothing and fails, leaving the claim and the
	// interest record for the caller's retry; with the read working it releases the claim and
	// deletes the record.
	t.Run("release", func(t *testing.T) {
		const (
			role   = "held-release-role"
			holder = "ses_release_held_on_failed_read"
		)
		for name, fails := range map[string]bool{"failed read": true, "reads working": false} {
			t.Run(name, func(t *testing.T) {
				rig := newRepairRig(t, nil)
				claimRoleAs(t, rig.reg, holder, "m1", role, roleClaimInstance)
				claimed := storedRoleClaim(t, rig.reg, role)
				claims := countRoleReads(rig.reg)
				claims.failGets.Store(fails)

				_, err := rig.reg.Remove(context.Background(), holder, nil, releasedByItsProcess)
				claims.failGets.Store(false)
				stored := storedRoleClaim(t, rig.reg, role)
				if !fails {
					if err != nil || stored.Revision != 0 {
						t.Fatalf("with the claim read working the deregistration answered %v and left the claim %+v, want no error and the claim released", err, stored)
					}
					assertNoInterestRecord(t, rig, holder)
					return
				}
				if !errors.Is(err, errClaimReadFailed) {
					t.Fatalf("the deregistration whose claim read failed answered %v, want an error carrying %v", err, errClaimReadFailed)
				}
				if stored != claimed {
					t.Fatalf("the deregistration whose claim read failed left the claim %+v, want it as the store held it, %+v", stored, claimed)
				}
				if _, err := rig.interests.Get(holder); err != nil {
					t.Fatalf("read %s's interest record after the failed deregistration: %v, want the record left for the retry", holder, err)
				}
			})
		}
	})

	// A registration naming a controller notice lane whose claim read fails is refused before
	// anything is stored: the plan the listener filters the session's consumer on, the plan made
	// again after a conflict and the merging write each return the read's error, naming the
	// role, and the bucket holds the session's row at the revision it held before. The copy names
	// the session, so only the claim record refuses. With the read working the same registration
	// plans the lane and stores it.
	t.Run("controller lane", func(t *testing.T) {
		const (
			role   = "legion-HELD-controller"
			lane   = "notifications.legion.HELD.controller"
			holder = "ses_controller_held_on_failed_read"
		)
		own := contracts.AgentTopicPrefix + holder
		for name, fails := range map[string]bool{"failed read": true, "reads working": false} {
			t.Run(name, func(t *testing.T) {
				rig := newRepairRig(t, nil)
				claimRoleAs(t, rig.reg, holder, "m1", role, roleClaimInstance)
				settle(rig)
				if got := heldRoles(t, rig.reg, holder); !slices.Equal(got, []string{role}) {
					t.Fatalf("RolesHeldBy(%s) = %v, want [%s]: the copy must name the session", holder, got, role)
				}
				before, err := rig.interests.Get(holder)
				if err != nil {
					t.Fatalf("read %s's interest record: %v", holder, err)
				}
				claims := countRoleReads(rig.reg)
				claims.failGets.Store(fails)
				if !fails {
					requireLane(t, controllerRegistration(t, rig, holder, lane, own), lane, true, "the claim read answered and names "+holder)
					return
				}

				ctx := context.Background()
				requested := []string{lane, own}
				for call, write := range map[string]func() error{
					"PlanInterest": func() error {
						_, err := rig.reg.PlanInterest(ctx, holder, requested)
						return err
					},
					"PlanInterestAfterConflict": func() error {
						_, err := rig.reg.PlanInterestAfterConflict(ctx, holder, requested)
						return err
					},
					"UpsertWithin": func() error {
						_, err := rig.reg.UpsertWithin(ctx, Interest{SessionID: holder, MachineID: "m1"}, requested)
						return err
					},
				} {
					if err := write(); !errors.Is(err, errClaimReadFailed) || !strings.Contains(err.Error(), role) {
						t.Fatalf("%s of a registration whose controller claim read failed answered %v, want an error naming %s and carrying %v", call, err, role, errClaimReadFailed)
					}
				}
				claims.failGets.Store(false)
				after, err := rig.interests.Get(holder)
				if err != nil {
					t.Fatalf("read %s's interest record after the refused registration: %v", holder, err)
				}
				if after.Revision() != before.Revision() {
					t.Fatalf("%s's interest record moved from revision %d to %d, want nothing stored by a registration whose controller claim read failed", holder, before.Revision(), after.Revision())
				}
			})
		}

		// A lane whose role is a key NATS refuses has a claim no read can tell the holder of, which
		// is no failed read: the registration plans and stores the rest of its row without that
		// lane, rather than being refused at every heartbeat.
		t.Run("a key NATS refuses", func(t *testing.T) {
			const refused = "notifications.legion.HELD:bad.controller"
			if role, lane := bus.ControllerNoticeRole(refused); !lane || role != "legion-HELD:bad-controller" {
				t.Fatalf("ControllerNoticeRole(%s) = %q, %v; want legion-HELD:bad-controller, a controller notice lane", refused, role, lane)
			}
			rig := newRepairRig(t, nil)
			sets := controllerRegistration(t, rig, holder, refused, own)
			requireLane(t, sets, refused, false, "no read can tell the holder of a claim under a key NATS refuses")
			requireLane(t, sets, own, true, "the rest of the registration is stored")
		})
	})
}
