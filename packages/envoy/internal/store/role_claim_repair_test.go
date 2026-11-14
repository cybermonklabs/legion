package store

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/kvwatch/kvwatchtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// repairBase is the moment the clock of every registry these tests open starts at, and so the
// moment its start's read completes on that clock.
var repairBase = time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)

// repairRig is a registry whose role claim watcher keeps its scheduled read for the test to fire
// (kvwatchtest.Waits), on a clock the test moves (manualClock), with the connection it was opened
// on and raw handles on its two buckets for the writes the registry does not make itself.
type repairRig struct {
	t         *testing.T
	conn      *natsgo.Conn
	reg       *Registry
	clock     *manualClock
	waits     *kvwatchtest.Waits
	roles     natsgo.KeyValue
	interests natsgo.KeyValue
}

// newRepairRig opens the rig's registry at repairBase, over a role bucket seed has written to when
// seed is not nil, and returns once the start's read has completed and armed the scheduled read.
func newRepairRig(t *testing.T, seed func(roles natsgo.KeyValue)) *repairRig {
	t.Helper()
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if seed != nil {
		roles, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Storage: natsgo.FileStorage})
		if err != nil {
			t.Fatalf("create the role bucket: %v", err)
		}
		seed(roles)
	}
	rig := &repairRig{t: t, conn: conn, clock: &manualClock{at: repairBase}, waits: &kvwatchtest.Waits{}}
	rig.reg, err = Open(conn, WithReplicas(1), WithClock(rig.clock.Now), WithRoleClaimWatch(kvwatch.WithAfterFunc(rig.waits.After)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(rig.reg.StopWatch)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rig.reg.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}
	if rig.roles, err = js.KeyValue(bus.RoleClaimBucket); err != nil {
		t.Fatalf("open the role bucket: %v", err)
	}
	if rig.interests, err = js.KeyValue(bus.InterestBucket); err != nil {
		t.Fatalf("open the interest bucket: %v", err)
	}
	rig.scheduled()
	return rig
}

// scheduled is the scheduled read the role claim watcher armed last and has not stopped. It is
// waited for: a read records its completion (roleCache.CompleteRead) a moment before the watcher
// arms the next.
func (rig *repairRig) scheduled() *kvwatchtest.Wait {
	rig.t.Helper()
	var wait *kvwatchtest.Wait
	waitFor(rig.t, 10*time.Second, func() bool {
		wait = rig.waits.Armed()
		return wait != nil
	})
	return wait
}

// due is the moment on the registry's clock the armed scheduled read comes due: its delay past the
// read that armed it, which completed when the report says.
func (rig *repairRig) due() time.Time {
	rig.t.Helper()
	return rig.reg.RoleClaimRepair().CompletedAt.Add(rig.scheduled().Delay())
}

// fire moves the registry's clock to at and fires the armed scheduled read, without waiting for
// the read it starts. It returns the report before that read and the wait it fired, for
// awaitRepair.
func (rig *repairRig) fire(at time.Time) (RoleClaimRepairReport, *kvwatchtest.Wait) {
	rig.t.Helper()
	wait := rig.scheduled()
	before := rig.reg.RoleClaimRepair()
	rig.clock.Set(at)
	wait.Fire()
	return before, wait
}

// awaitRepair waits until a read after before has completed and the watcher has armed a scheduled
// read in place of fired, and returns the report of that read. A read that does not complete is an
// error and not the end of the test, so the assertion the caller makes of the copy next fails too,
// naming what the repair left undone.
func (rig *repairRig) awaitRepair(before RoleClaimRepairReport, fired *kvwatchtest.Wait) RoleClaimRepairReport {
	rig.t.Helper()
	if !testwait.Holds(10*time.Second, testwait.Poll, func() bool {
		next := rig.waits.Armed()
		return next != nil && next != fired && rig.reg.RoleClaimRepair().Completed > before.Completed
	}) {
		rig.t.Errorf("the scheduled read fired after %d completed reads completed no read of the role claims within 10s", before.Completed)
	}
	return rig.reg.RoleClaimRepair()
}

// repair fires the armed scheduled read at the moment it comes due and returns the report of the
// read it started, once that read has completed.
func (rig *repairRig) repair() RoleClaimRepairReport {
	rig.t.Helper()
	return rig.awaitRepair(rig.fire(rig.due()))
}

// register is one registration of sessionID by the process instance, as POST
// /v1/interests/subscribe makes it (cmd/listener's subscribeHandler), at the registry clock's
// current moment: the interest row planned and written for topics (PlanInterest, UpsertPlanned),
// then the renewal of the session's role claims (RenewRoleClaims) over the row that write
// returned. It returns that row.
func (rig *repairRig) register(sessionID, instance string, topics ...string) Interest {
	rig.t.Helper()
	ctx := context.Background()
	plan, err := rig.reg.PlanInterest(ctx, sessionID, topics)
	if err != nil {
		rig.t.Fatalf("plan %s's registration: %v", sessionID, err)
	}
	row, err := rig.reg.UpsertPlanned(ctx, Interest{SessionID: sessionID, MachineID: "m1"}, plan)
	if err != nil {
		rig.t.Fatalf("write %s's registration: %v", sessionID, err)
	}
	if _, err := rig.reg.RenewRoleClaims(ctx, sessionID, instance, row); err != nil {
		rig.t.Fatalf("renew %s's role claims: %v", sessionID, err)
	}
	return row
}

// loseRoleClaim takes role's holder out of the copy (kvwatch.Copy.Remove) while the bucket goes on
// holding its claim, as a write or a delivery the copy missed leaves it.
func loseRoleClaim(reg *Registry, role string) {
	reg.holdersMu.Lock()
	defer reg.holdersMu.Unlock()
	reg.roleHolders.Remove(role)
}

// rolesBySession is RolesBySession that fails the test when the view cannot answer.
func rolesBySession(t *testing.T, reg *Registry) map[string][]string {
	t.Helper()
	held, err := reg.RolesBySession()
	if err != nil {
		t.Fatalf("RolesBySession: %v", err)
	}
	return held
}

// The repair runs often enough to finish inside its bound, and the registry's watcher arms its
// scheduled read at the very interval the constant names, so the constant is what runs. That the
// bound leaves a claim the copy lost two heartbeats of the live window to be renewed in is the
// bound's own definition (RoleClaimRepairBound), not something to test.
func TestTheRoleClaimRepairIntervalIsInsideItsBound(t *testing.T) {
	if roleClaimRepairInterval > RoleClaimRepairBound {
		t.Fatalf("the role claim repair runs every %s, past its bound of %s: a claim the copy lost stays lost past the bound",
			roleClaimRepairInterval, RoleClaimRepairBound)
	}
	rig := newRepairRig(t, nil)
	if wait := rig.scheduled(); wait.Delay() != roleClaimRepairInterval {
		t.Fatalf("the role claim watcher armed its scheduled read %s after the start's read, want roleClaimRepairInterval, %s",
			wait.Delay(), roleClaimRepairInterval)
	}
}

// A claim the bucket holds and the copy lost is back in the copy once the scheduled read has run,
// with no registration in between: the read is armed no later than the bound after the read before
// it, it delivers the claim again, and its report counts the claim put back and logs its role.
func TestARoleCopyLostClaimIsBackWithinTheRepairBound(t *testing.T) {
	const (
		role   = "planner"
		holder = "ses_lost_claim"
		kept   = "ses_kept_claim"
	)
	logs := captureRegistryLogs(t)
	rig := newRepairRig(t, func(roles natsgo.KeyValue) {
		putRoleClaim(t, roles, role, holder)
		putRoleClaim(t, roles, "reviewer", kept)
	})
	if wait := rig.scheduled(); wait.Delay() > RoleClaimRepairBound {
		t.Fatalf("the role claim watcher armed its scheduled read %s after the read that completed, past the repair bound of %s",
			wait.Delay(), RoleClaimRepairBound)
	}

	lostAt := rig.clock.Now()
	loseRoleClaim(rig.reg, role)
	if got := rolesBySession(t, rig.reg)[holder]; len(got) != 0 {
		t.Fatalf("with its claim taken out of the copy, RolesBySession gives %s %v, want nothing", holder, got)
	}

	repaired := rig.repair()
	if got := rolesBySession(t, rig.reg)[holder]; !slices.Equal(got, []string{role}) {
		t.Fatalf("after the scheduled read RolesBySession gives %s %v, want [%s]: the read did not put back the claim the copy lost", holder, got, role)
	}
	if got := heldRoles(t, rig.reg, holder); !slices.Equal(got, []string{role}) {
		t.Fatalf("after the scheduled read RolesHeldBy(%s) = %v, want [%s]", holder, got, role)
	}
	if got := heldRoles(t, rig.reg, kept); !slices.Equal(got, []string{"reviewer"}) {
		t.Fatalf("after the scheduled read RolesHeldBy(%s) = %v, want [reviewer]: the repair moved a claim the copy held right", kept, got)
	}
	if repaired.PutBack != 1 || repaired.TookOut != 0 {
		t.Fatalf("the scheduled read's report = %+v, want one claim put back and none taken out", repaired)
	}
	if back := repaired.CompletedAt.Sub(lostAt); back > RoleClaimRepairBound {
		t.Fatalf("the claim the copy lost was back %s after the loss, past the repair bound of %s", back, RoleClaimRepairBound)
	}
	for _, want := range []string{"level=INFO", `msg="role claim repair put back 1 claims and took out 0"`, "put_back_roles=[" + role + "]"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("registry logs %q, want them to contain %q", logs.String(), want)
		}
	}
}

// A claim the copy holds and the bucket does not is out of the copy once the scheduled read has
// run: the read finds no evidence of it, its sweep removes it, and its report counts it taken out.
func TestARoleCopySurplusClaimIsOutWithinTheRepairBound(t *testing.T) {
	const (
		role    = "surplus-role"
		surplus = "ses_surplus_claim"
		kept    = "ses_kept_claim"
	)
	logs := captureRegistryLogs(t)
	rig := newRepairRig(t, func(roles natsgo.KeyValue) {
		putRoleClaim(t, roles, "planner", kept)
	})

	gainedAt := rig.clock.Now()
	rig.reg.applyRoleClaim(roleClaimEntry(role, surplus, 1))
	if got := heldRoles(t, rig.reg, surplus); !slices.Equal(got, []string{role}) {
		t.Fatalf("with a claim the bucket does not hold put in the copy, RolesHeldBy(%s) = %v, want [%s]", surplus, got, role)
	}

	repaired := rig.repair()
	for session, roles := range rolesBySession(t, rig.reg) {
		if slices.Contains(roles, role) {
			t.Fatalf("after the scheduled read RolesBySession gives %s the role %s, which the bucket holds no claim of", session, role)
		}
	}
	if got := heldRoles(t, rig.reg, surplus); len(got) != 0 {
		t.Fatalf("after the scheduled read RolesHeldBy(%s) = %v, want nothing", surplus, got)
	}
	if got := heldRoles(t, rig.reg, kept); !slices.Equal(got, []string{"planner"}) {
		t.Fatalf("after the scheduled read RolesHeldBy(%s) = %v, want [planner]: the sweep took out a claim the bucket holds", kept, got)
	}
	if repaired.PutBack != 0 || repaired.TookOut != 1 {
		t.Fatalf("the scheduled read's report = %+v, want none put back and one claim taken out", repaired)
	}
	if out := repaired.CompletedAt.Sub(gainedAt); out > RoleClaimRepairBound {
		t.Fatalf("the surplus claim was out %s after the copy took it, past the repair bound of %s", out, RoleClaimRepairBound)
	}
	for _, want := range []string{`msg="role claim repair put back 0 claims and took out 1"`, "took_out_roles=[" + role + "]"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("registry logs %q, want them to contain %q", logs.String(), want)
		}
	}
}

// The first read of an epoch fills the copy rather than repairing it, so it counts nothing: the
// start's read of a bucket holding claims, and the first read of that bucket recreated with other
// claims, each report none put back and none taken out, and neither logs a repair. The time of each
// is still the copy's last completed read.
func TestTheFirstReadOfAnEpochCountsNoRepair(t *testing.T) {
	logs := captureRegistryLogs(t)
	rig := newRepairRig(t, func(roles natsgo.KeyValue) {
		putRoleClaim(t, roles, "planner", "ses_a")
		putRoleClaim(t, roles, "reviewer", "ses_b")
		putRoleClaim(t, roles, "architect", "ses_b")
	})
	started := rig.reg.RoleClaimRepair()
	if started.Completed != 1 || started.PutBack != 0 || started.TookOut != 0 || !started.CompletedAt.Equal(repairBase) {
		t.Fatalf("after the start's read the report = %+v, want one read completed at %s that counted nothing, though the copy it filled was empty",
			started, repairBase)
	}
	if want := map[string][]string{"ses_a": {"planner"}, "ses_b": {"architect", "reviewer"}}; !maps.EqualFunc(rolesBySession(t, rig.reg), want, slices.Equal) {
		t.Fatalf("after the start's read RolesBySession = %v, want %v", rolesBySession(t, rig.reg), want)
	}

	// Every read from here completes a second on, so the report's time tells the recreated
	// bucket's reads from the start's whichever of them - the stream probe's or the Rewatch's -
	// reads it first.
	refilledAt := repairBase.Add(time.Second)
	rig.clock.Set(refilledAt)
	js, err := rig.conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.RoleClaimBucket); err != nil {
		t.Fatalf("delete the role bucket: %v", err)
	}
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Replicas: 1, Storage: natsgo.FileStorage})
	putRoleClaim(t, recreated, "reviewer", "ses_a")
	putRoleClaim(t, recreated, "lead", "ses_c")
	if err := rig.reg.Rewatch(rig.conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	want := map[string][]string{"ses_a": {"reviewer"}, "ses_c": {"lead"}}
	waitFor(t, 30*time.Second, func() bool {
		held, err := rig.reg.RolesBySession()
		return err == nil && maps.EqualFunc(held, want, slices.Equal)
	})
	refilled := rig.reg.RoleClaimRepair()
	if refilled.Completed <= started.Completed || refilled.PutBack != 0 || refilled.TookOut != 0 || !refilled.CompletedAt.Equal(refilledAt) {
		t.Fatalf("after the recreated bucket's read the report = %+v, want a later read completed at %s that counted nothing, though the new bucket's claims differ from the old one's",
			refilled, refilledAt)
	}
	if strings.Contains(logs.String(), `msg="role claim repair`) {
		t.Fatalf("a first read of an epoch logged a repair:\n%s", logs.String())
	}
}

// The repair reads the bucket through the copy's read protocol, so a claim granted while the
// scheduled read is under way, after its scan passed the role's key, stands at the read's
// completion: the grant's write marks the key for that read and fences out the older record the
// scan delivered. The copy is read inside the read's completion, before the watcher hands over any
// record delivered after it, so the claim is the grant's own write and not a later delivery of it.
func TestARepairMidScanKeepsAClaimGrantedWhileTheReadRuns(t *testing.T) {
	const (
		role = "planner"
		// passed is stored after role, so the scan delivers it after role's claim: the scheduled
		// read is held there, past role's key.
		passed  = "reviewer"
		before  = "ses_held_before_the_grant"
		granted = "ses_granted_mid_scan"
	)
	rig := newRepairRig(t, func(roles natsgo.KeyValue) {
		putRoleClaim(t, roles, role, before)
		putRoleClaim(t, roles, passed, "ses_other")
	})

	var scheduled atomic.Bool
	reached := make(chan struct{})
	gate := make(chan struct{})
	var reachOnce, openOnce sync.Once
	open := func() { openOnce.Do(func() { close(gate) }) }
	t.Cleanup(open)
	atCompletion := make(chan string, 1)
	cache := completionWatched{
		Cache: gatedRoleClaims{r: rig.reg, hold: func(key string) {
			if scheduled.Load() && key == passed {
				reachOnce.Do(func() { close(reached) })
				<-gate
			}
		}},
		completed: func(kvwatch.Read) {
			if scheduled.Load() {
				select {
				case atCompletion <- holderOf(rig.reg, role):
				default:
				}
			}
		},
	}
	rig.reg.roleWatcher.Stop()
	rig.reg.roleWatcher = kvwatch.New("role claims", rig.reg.roles(), cache,
		kvwatch.WithScheduledRead(roleClaimRepairInterval), kvwatch.WithAfterFunc(rig.waits.After))
	rig.reg.roleWatcher.Start()
	waitFor(t, 10*time.Second, func() bool { return servableView(rig.reg) })

	scheduled.Store(true)
	report, fired := rig.fire(rig.due())
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatalf("the scheduled read never passed %s's claim", role)
	}
	claimRoleAs(t, rig.reg, granted, "m1", role, roleClaimInstance)
	if got := heldRoles(t, rig.reg, granted); !slices.Equal(got, []string{role}) {
		t.Fatalf("with the scheduled read under way, RolesHeldBy(%s) = %v as the grant answers, want [%s]", granted, got, role)
	}

	open()
	select {
	case holder := <-atCompletion:
		if holder != granted {
			t.Fatalf("the scheduled read completed with %s's claim naming %q, want %s: the repair undid a claim granted after its scan passed the key",
				role, holder, granted)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the scheduled read never completed")
	}
	repaired := rig.awaitRepair(report, fired)
	if repaired.PutBack != 0 || repaired.TookOut != 0 {
		t.Fatalf("the scheduled read's report = %+v, want nothing counted: the copy it read into held the bucket's claims, and the grant was the registry's own write", repaired)
	}
	if got := heldRoles(t, rig.reg, granted); !slices.Equal(got, []string{role}) {
		t.Fatalf("after the scheduled read RolesHeldBy(%s) = %v, want [%s]", granted, got, role)
	}
	if got := heldRoles(t, rig.reg, before); len(got) != 0 {
		t.Fatalf("after the scheduled read RolesHeldBy(%s) = %v, want nothing: the grant took %s from it", before, got, role)
	}
}

// completionWatched is a role claim cache whose completed reads call completed once the copy has
// swept, before the watcher's next hook can run (kvwatch.Cache: no two hooks of one watcher run at
// once), so a test reads the copy exactly as a read's completion left it, before any record
// delivered after that read reaches it.
type completionWatched struct {
	kvwatch.Cache
	completed func(read kvwatch.Read)
}

func (c completionWatched) CompleteRead(read kvwatch.Read) {
	c.Cache.CompleteRead(read)
	c.completed(read)
}

// A claim that neither the session's interest record nor the copy names is renewed again once the
// scheduled read has put it back in the copy. The record is one this build cannot decode, so the
// registration's write replaces it with the registration's own topics, none of them the role's;
// the copy lost the claim just after a read passed it, so a registration before the next read
// renews nothing; that read puts the claim back, and the registration after it renews the claim.
// From the claim's last renewal to its next is at most the repair bound and one heartbeat, on the
// registry's clock, with registrations a heartbeat apart from the claim's own.
func TestARescanBringsBackAClaimNeitherTheRecordNorTheCopyNames(t *testing.T) {
	const (
		role   = "planner"
		holder = "ses_rescan_holder"
	)
	own := contracts.AgentTopicPrefix + holder
	rig := newRepairRig(t, nil)
	claimRoleAs(t, rig.reg, holder, "m1", role, roleClaimInstance)
	claimed := storedRoleClaim(t, rig.reg, role)
	lastRenewal := time.UnixMilli(claimed.LastAnsweredAt())

	if _, err := rig.interests.Put(holder, []byte(undecodableInterestRecord)); err != nil {
		t.Fatalf("store %s's interest record: %v", holder, err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, cached := rig.reg.Cached(holder)
		return !cached
	})
	rig.repair()
	loseRoleClaim(rig.reg, role)

	repairDue := rig.due()
	beat := lastRenewal.Add(contracts.SessionHeartbeat)
	if !beat.Before(repairDue) {
		t.Fatalf("the first registration after the loss comes at %s and the scheduled read at %s: the row needs a registration between the loss and the repair",
			beat, repairDue)
	}
	rig.clock.Set(beat)
	row := rig.register(holder, roleClaimInstance, own)
	if slices.Contains(row.Topics, contracts.RoleTopicPrefix+role) {
		t.Fatalf("the registration's row names %v, want no role topic: the record was to name no role", row.Topics)
	}
	if got := heldRoles(t, rig.reg, holder); len(got) != 0 {
		t.Fatalf("before the scheduled read RolesHeldBy(%s) = %v, want nothing: the copy lost the claim", holder, got)
	}
	if stored := storedRoleClaim(t, rig.reg, role); stored.RenewedAt != claimed.RenewedAt {
		t.Fatalf("a registration while neither the record nor the copy names the claim renewed it at %d, want it left at %d",
			stored.RenewedAt, claimed.RenewedAt)
	}

	repaired := rig.repair()
	if repaired.PutBack != 1 {
		t.Fatalf("the scheduled read's report = %+v, want the lost claim put back", repaired)
	}
	if got := heldRoles(t, rig.reg, holder); !slices.Equal(got, []string{role}) {
		t.Fatalf("after the scheduled read RolesHeldBy(%s) = %v, want [%s]", holder, got, role)
	}
	// The next registration is the first heartbeat at or after the repair.
	beat = beat.Add(contracts.SessionHeartbeat)
	for beat.Before(repaired.CompletedAt) {
		beat = beat.Add(contracts.SessionHeartbeat)
	}
	rig.clock.Set(beat)
	rig.register(holder, roleClaimInstance, own)
	renewed := storedRoleClaim(t, rig.reg, role)
	if renewed.RenewedAt != beat.UnixMilli() {
		t.Fatalf("the registration at %s after the repair left the claim renewed at %d, want %d: it did not renew the claim the repair put back",
			beat, renewed.RenewedAt, beat.UnixMilli())
	}
	if gap, limit := beat.Sub(lastRenewal), RoleClaimRepairBound+contracts.SessionHeartbeat; gap > limit {
		t.Fatalf("the claim was renewed %s after its last renewal, past the repair bound and one heartbeat (%s)", gap, limit)
	}
}

// RoleClaimRepairReport.Err ages the last whole read that completed against the bound on the
// registry's clock: a registry no read has completed for is reported so, a read no older than the
// bound is not late, one older is, naming its age and the bound, and the next read that completes,
// however late, resets the age.
func TestARoleClaimRepairOlderThanItsBoundIsReportedLate(t *testing.T) {
	if err := (&Registry{}).RoleClaimRepair().Err(); err == nil || err.Error() != "no whole read of the role claims has completed" {
		t.Fatalf("a registry no read has completed for reports the repair error %v, want that no whole read has completed", err)
	}
	rig := newRepairRig(t, nil)
	if err := rig.reg.RoleClaimRepair().Err(); err != nil {
		t.Fatalf("just after the start's read the repair error = %v, want nil", err)
	}
	rig.clock.Set(repairBase.Add(RoleClaimRepairBound))
	if err := rig.reg.RoleClaimRepair().Err(); err != nil {
		t.Fatalf("the bound after the start's read the repair error = %v, want nil", err)
	}
	late := repairBase.Add(RoleClaimRepairBound + time.Second)
	rig.clock.Set(late)
	err := rig.reg.RoleClaimRepair().Err()
	if want := "the last whole read of the role claims completed 2m1s ago, past the 2m0s bound"; err == nil || err.Error() != want {
		t.Fatalf("past the bound the repair error = %v, want %q", err, want)
	}

	repaired := rig.awaitRepair(rig.fire(late))
	if !repaired.CompletedAt.Equal(late) {
		t.Fatalf("the late scheduled read completed at %s, want %s", repaired.CompletedAt, late)
	}
	if err := rig.reg.RoleClaimRepair().Err(); err != nil {
		t.Fatalf("after the late scheduled read the repair error = %v, want nil", err)
	}
}
