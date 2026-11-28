package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/kvwatch/kvwatchtest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// applyRoleClaim applies one record as the role claim watcher delivers it (roleCache.Apply), the
// way these tests reach the holder view without a bucket write.
func (r *Registry) applyRoleClaim(entry natsgo.KeyValueEntry) {
	roleCache{r}.Apply(kvwatch.LegacyEntry(entry), kvwatch.Read{})
}

// resetRoleHolders empties the holder view and its fence, as a listener start leaves it.
func (r *Registry) resetRoleHolders() {
	r.holdersMu.Lock()
	defer r.holdersMu.Unlock()
	r.roleHolders = newRoleHolderCopy(r)
}

// holderOf is the session the view names for role, empty when it names none.
func holderOf(r *Registry, role string) string {
	r.holdersMu.RLock()
	defer r.holdersMu.RUnlock()
	holder, _ := r.roleHolders.Get(role)
	return holder
}

// servableView reports whether the role claim view can be served, which is what RolesHeldBy
// answers from (kvwatch.Watcher.Servable).
func servableView(r *Registry) bool { return r.roleWatcher.Servable() == nil }

// gatedRoleClaims is roleCache with hold called before each record reaches the view and began
// called at each read's start, so a test can hold one delivery while it asks the view, and see a
// recreated bucket's read begin (kvwatch.Cache).
type gatedRoleClaims struct {
	r     *Registry
	hold  func(key string)
	began func(read kvwatch.Read)
}

func (g gatedRoleClaims) BeginRead(read kvwatch.Read) {
	if g.began != nil {
		g.began(read)
	}
	roleCache{g.r}.BeginRead(read)
}

func (g gatedRoleClaims) Apply(entry kvwatch.Entry, read kvwatch.Read) {
	if g.hold != nil {
		g.hold(entry.Key)
	}
	roleCache{g.r}.Apply(entry, read)
}

func (g gatedRoleClaims) CompleteRead(read kvwatch.Read) { roleCache{g.r}.CompleteRead(read) }

// stallFirstKV stalls the first watch it hands out after `after` entries, as a first scan that
// delivers part of the bucket and then nothing does (earlyEndKV), and hands the real bucket's
// watcher to every watch after it: the watcher reads the bucket again itself once its first read
// times out, so the view becomes servable with no Rewatch.
type stallFirstKV struct {
	jetstream.KeyValue

	after   int
	stalled atomic.Bool
}

func (k *stallFirstKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	if k.stalled.Swap(true) {
		return k.KeyValue.Watch(ctx, keys, opts...)
	}
	return earlyEndKV{KeyValue: k.KeyValue, after: k.after}.Watch(ctx, keys, opts...)
}

// heldRoles is RolesHeldBy that fails the test when the view cannot answer.
func heldRoles(t *testing.T, reg *Registry, sessionID string) []string {
	t.Helper()
	roles, err := reg.RolesHeldBy(sessionID)
	if err != nil {
		t.Fatalf("RolesHeldBy(%s): %v", sessionID, err)
	}
	return roles
}

// The registry's view of the role claims follows the role bucket, whoever writes it: a claim
// stored before Open and one written after give their holders the role, a claim that supersedes
// one moves the role, a deleted claim leaves the role to nobody, and a claim this build cannot
// decode gives it to nobody either and is logged at WARN once. A claim this registry writes itself
// is in the view as soon as its write answers, and leaves it as soon as its release answers: the
// watcher's deliveries of that claim are held until the test has asked. Interest topics play no
// part.
func TestRolesHeldByFollowsTheRoleClaims(t *testing.T) {
	t.Parallel()
	logs, logger := ownLogs()
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	rawRoles, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create the role bucket: %v", err)
	}
	// The registry's clock, which the claim stored before Open is stamped on as well.
	clock := clocktest.New(registryClockStart)
	putRoleClaim(t, rawRoles, "restored", "ses_a", clock.Now)

	reg := openRegistry(t, conn, WithClock(clock.Now), WithLogger(logger))
	if err := reg.WaitForCacheReady(testwait.Context(t)); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}
	// A role topic on a record says nothing about who holds the role.
	if _, err := reg.Upsert(Interest{SessionID: "ses_b", MachineID: "m1"}, []string{"notifications.role.restored"}); err != nil {
		t.Fatalf("upsert ses_b: %v", err)
	}
	if got := heldRoles(t, reg, "ses_a"); !slices.Equal(got, []string{"restored"}) {
		t.Fatalf("the holder of a claim stored before Open holds %v, want [restored]", got)
	}
	if got := heldRoles(t, reg, "ses_b"); len(got) != 0 {
		t.Fatalf("a session whose record carries a role topic but whose claim names another holds %v, want none", got)
	}

	held := func(sessionID string, want ...string) func() bool {
		return func() bool {
			got, err := reg.RolesHeldBy(sessionID)
			return err == nil && slices.Equal(got, append([]string{}, want...))
		}
	}
	putRoleClaim(t, rawRoles, "reviewer", "ses_a", reg.now)
	waitFor(t, held("ses_a", "restored", "reviewer"))

	putRoleClaim(t, rawRoles, "reviewer", "ses_b", reg.now)
	waitFor(t, held("ses_b", "reviewer"))
	waitFor(t, held("ses_a", "restored"))

	if err := rawRoles.Delete("reviewer"); err != nil {
		t.Fatalf("delete reviewer's claim: %v", err)
	}
	waitFor(t, held("ses_b"))

	if _, err := rawRoles.Put("restored", []byte(`{"holder_session_id":""}`)); err != nil {
		t.Fatalf("store a claim naming no holder: %v", err)
	}
	waitFor(t, held("ses_a"))
	logtest.AwaitLogged(t, logs, "the WARN that reports the claim naming no holder", "role claim watcher found a claim it cannot read")
	if n := strings.Count(logs.String(), "role claim watcher found a claim it cannot read"); n != 1 {
		t.Fatalf("one undecodable claim logged %d times, want once:\n%s", n, logs.String())
	}

	// From here the watcher holds every delivery of planner's claim until the test has asked, so
	// the view can hold the claim, and lose it, only through the registry's own writes.
	gate := make(chan struct{})
	var openOnce sync.Once
	open := func() { openOnce.Do(func() { close(gate) }) }
	t.Cleanup(open)
	reg.roleWatcher.Stop()
	reg.roleWatcher = kvwatch.New("role claims", reg.roles(), gatedRoleClaims{r: reg, hold: func(key string) {
		if key == "planner" {
			<-gate
		}
	}}, kvwatch.WithLogger(logger))
	reg.roleWatcher.Start()
	waitFor(t, func() bool {
		return servableView(reg)
	})
	claimRoleAs(t, reg, "ses_c", "m1", "planner", roleClaimInstance)
	if got := heldRoles(t, reg, "ses_c"); !slices.Equal(got, []string{"planner"}) {
		t.Fatalf("a claim this registry wrote is not in its view as the write answers: %v", got)
	}
	if _, err := reg.Remove(context.Background(), "ses_c", []string{"notifications.role.planner"}, releasedByItsProcess); err != nil {
		t.Fatalf("release planner: %v", err)
	}
	if got := heldRoles(t, reg, "ses_c"); len(got) != 0 {
		t.Fatalf("a claim this registry released is still in its view as the release answers: %v", got)
	}
	open()
}

// A view that cannot answer says so rather than answering that a session holds nothing: the role
// claim watcher has ended and nothing has replaced it, or no watcher follows the claims at all.
// The watcher ends here as it ends when its bucket goes away: the updates of its watch close, and
// the watcher records its end (kvwatch.Watcher.Err), which RolesHeldBy refuses on
// (Registry.RoleViewErr). Its watch closes them at the record the case writes after the watcher's
// read completed (earlyEndKV, ended), where a watch of a deleted bucket has nats.go close them only
// once its ordered consumer has missed two of its 5 s idle heartbeats; from the close on, the code
// that answers RolesHeldBy is the same.
func TestRolesHeldByRefusesWhenItsViewCannotAnswer(t *testing.T) {
	t.Parallel()
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg := openRegistry(t, conn)
	if err := reg.WaitForCacheReady(testwait.Context(t)); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}
	if _, err := reg.RolesHeldBy("ses_a"); err != nil {
		t.Fatalf("a ready view refused: %v", err)
	}
	// The role bucket holds no claim, so a watch's read of it is its one nil: the watch below hands
	// that on, so the read completes and the view answers, and closes its updates at the record
	// after it.
	roles := reg.roles()
	reg.roleWatcher.Stop()
	ending := bus.NewKeyValue(roles.Legacy(), earlyEndKV{KeyValue: roles.WatchHandle(), after: 1, ended: true}, roles.Streams())
	reg.roleWatcher = kvwatch.New("role claims", ending, roleCache{reg})
	reg.roleWatcher.Start()
	testwait.Until(t, "the view to answer once its watcher's read of the empty bucket completes", func() (string, bool) {
		_, err := reg.RolesHeldBy("ses_a")
		return fmt.Sprintf("RolesHeldBy(ses_a) refused with %v", err), err == nil
	})
	putRoleClaim(t, roles.Legacy(), "reviewer", "ses_b", reg.now)
	testwait.Until(t, "RolesHeldBy to refuse once the role claim watcher's updates closed", func() (string, bool) {
		held, err := reg.RolesHeldBy("ses_a")
		return fmt.Sprintf("RolesHeldBy(ses_a) = %v, %v", held, err), errors.Is(err, ErrRoleViewNotReady)
	})
	if reg.WatchErr() == nil {
		t.Fatal("the registry reports no watcher error while its role claim view cannot answer")
	}

	if _, err := (&Registry{}).RolesHeldBy("ses_a"); !errors.Is(err, ErrRoleViewNotReady) {
		t.Fatalf("a registry no watcher feeds answered RolesHeldBy with %v, want ErrRoleViewNotReady", err)
	}
}

// heldOnceComplete reads RolesHeldBy(sessionID) through the shared wait (testwait.Until) until it
// answers want, and fails the test the first time it answers anything else: a view that is not
// complete refuses (ErrRoleViewNotReady), and never reports the session without its roles. The
// first refusal it reads calls release, when release is not nil, which lets the view complete (a
// re-read the case holds), so that refusal is read while the view cannot answer. It returns how
// many refusals came first.
func heldOnceComplete(t *testing.T, reg *Registry, sessionID string, want []string, release func()) int {
	t.Helper()
	refusals := 0
	testwait.Until(t, fmt.Sprintf("RolesHeldBy(%s) to answer %v", sessionID, want), func() (string, bool) {
		roles, err := reg.RolesHeldBy(sessionID)
		switch {
		case err == nil && slices.Equal(roles, want):
			return fmt.Sprintf("%v after %d refusals", roles, refusals), true
		case err == nil:
			t.Fatalf("RolesHeldBy(%s) answered %v, want %v or ErrRoleViewNotReady: a holder reported without its role", sessionID, roles, want)
		case !errors.Is(err, ErrRoleViewNotReady):
			t.Fatalf("RolesHeldBy(%s) refused with %v, want ErrRoleViewNotReady", sessionID, err)
		}
		refusals++
		if refusals == 1 && release != nil {
			release()
		}
		return fmt.Sprintf("refused %d times, last with %v", refusals, err), false
	})
	return refusals
}

// waitForHeld waits through the shared wait (testwait.Until) until the view answers that
// sessionID holds exactly want, so a test that then starts the view again knows the claims reached
// it before it did. A give-up prints the view's last answer.
func waitForHeld(t *testing.T, reg *Registry, sessionID string, want ...string) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the role claim view to name %s the holder of %v", sessionID, want), func() (string, bool) {
		roles, err := reg.RolesHeldBy(sessionID)
		return fmt.Sprintf("RolesHeldBy(%s) = %v, %v", sessionID, roles, err), err == nil && slices.Equal(roles, want)
	})
}

// restartRoleViewTimingOutAfter puts the role claim view where a listener start leaves it when a
// first read's own idle bound ends its scan: the view is emptied and followed again by a watcher
// whose first read delivers delivered claims and then stalls until that bound ends it. The
// watcher reads the bucket again itself after such a read (stallFirstKV), so the view becomes
// servable without a Rewatch, as the product's does. It takes conn because the stalling handle is
// built on the bucket's own jetstream handles (bus.WrapKeyValue), which a raw nats.go handle has
// no route to.
//
// The watcher arms its waits on a fake of the test's own and reads its time there
// (kvwatch.WithAfterFunc, kvwatch.WithNow), and runs no stream probe
// (kvwatch.WithStreamProbeInterval), so nothing fires one of them but the test. This moves the fake
// past the stalled scan's idle wait (kvwatch.ScanIdleTimeout) once the view holds the claims that
// scan delivered, so the scan times out after them, and returns reread, which fires the re-read the
// timed-out scan arms (kvwatch's rereadDelay after it): the case calls it once it has read the view
// between the two.
func restartRoleViewTimingOutAfter(t *testing.T, conn *natsgo.Conn, reg *Registry, rawRoles natsgo.KeyValue, delivered int) (reread func()) {
	t.Helper()
	reg.roleWatcher.Stop()
	reg.resetRoleHolders()
	roles, err := bus.WrapKeyValue(conn, rawRoles)
	if err != nil {
		t.Fatalf("wrap the role bucket: %v", err)
	}
	stalling := bus.NewKeyValue(rawRoles, &stallFirstKV{KeyValue: roles.WatchHandle(), after: delivered}, roles.Streams())
	waits := clocktest.New(registryClockStart)
	reg.roleWatcher = kvwatch.New("role claims", stalling, roleCache{reg},
		kvwatch.WithAfterFunc(func(d time.Duration, f func()) kvwatch.Timer { return waits.AfterFunc(d, f) }),
		kvwatch.WithNow(waits.Now), kvwatch.WithStreamProbeInterval(0))
	reg.roleWatcher.Start()
	// The idle wait counts from the scan's last entry, which the watcher reads its time for before
	// it applies the entry: once the view holds what the scan delivered, a move past the wait ends
	// the scan rather than re-arming the wait for an entry read after it.
	testwait.Until(t, fmt.Sprintf("the stalled first read to deliver %d claims", delivered), func() (string, bool) {
		reg.holdersMu.RLock()
		held := reg.roleHolders.Len()
		reg.holdersMu.RUnlock()
		return fmt.Sprintf("the view holds %d claims", held), held == delivered
	})
	waits.AwaitArmedAt(t, "the stalled scan's idle wait", registryClockStart.Add(kvwatch.ScanIdleTimeout))
	waits.Advance(kvwatch.ScanIdleTimeout)
	return func() {
		t.Helper()
		// The timed-out scan's end disarmed its idle wait, and no probe runs, so the re-read is
		// the one wait armed.
		armed := waits.AwaitArmed(t, "the re-read the timed-out scan armed", 1)
		waits.Set(armed[0])
	}
}

// A role claim view whose first scan kvwatch's own idle wait ended (kvwatch.ScanIdleTimeout)
// holds only the claims that scan delivered, so it is no answer: RolesHeldBy refuses it as not
// ready rather than report the holder of a claim the scan did not deliver without its roles. The
// watcher reads the bucket again itself, a re-read of the timed-out scan (kvwatch's rereadDelay
// after it), and the view answers once that read completes. The re-read waits on the test's fake
// until the view has refused (restartRoleViewTimingOutAfter, heldOnceComplete), so the view refuses
// before it answers on every run.
func TestARoleViewWhoseFirstScanTimedOutAnswersOnlyOnceItsRescanCompletes(t *testing.T) {
	t.Parallel()
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg := openRegistry(t, conn)
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	rawRoles, err := js.KeyValue(bus.RoleClaimBucket)
	if err != nil {
		t.Fatalf("open the role bucket: %v", err)
	}
	putRoleClaim(t, rawRoles, "architect", "ses_other", reg.now)
	putRoleClaim(t, rawRoles, "reviewer", "ses_holder", reg.now)
	putRoleClaim(t, rawRoles, "planner", "ses_holder", reg.now)
	// The registry's own view reaches the claims first, so the view the test then starts again
	// is the only one short of them.
	waitForHeld(t, reg, "ses_holder", "planner", "reviewer")

	reread := restartRoleViewTimingOutAfter(t, conn, reg, rawRoles, 1)
	waitFor(t, reg.roleWatcher.Ready)
	if roles, err := reg.RolesHeldBy("ses_holder"); !errors.Is(err, ErrRoleViewNotReady) {
		t.Fatalf("after a first scan the idle timer ended, RolesHeldBy(ses_holder) = %v, %v; want ErrRoleViewNotReady", roles, err)
	}
	if refusals := heldOnceComplete(t, reg, "ses_holder", []string{"planner", "reviewer"}, reread); refusals == 0 {
		t.Fatal("the view answered at once; the test needs it refused until the rescan completes")
	}
	if got := heldRoles(t, reg, "ses_other"); !slices.Equal(got, []string{"architect"}) {
		t.Fatalf("after the rescan ses_other holds %v, want [architect]", got)
	}
}

// The listener's startup gate is this wait, so it decides whether GET /v1/sessions and
// GET /v1/registry/<session_id> are served before the role claim view can answer. Readiness is no
// gate for them: a first scan that its idle wait ended (kvwatch.ScanIdleTimeout) releases it with
// the view short of the bucket, and RolesHeldBy goes on refusing (ErrRoleViewNotReady) until the
// watcher's re-read completes, which both routes answer a live client as the bus-unavailable 503
// while the bus is healthy. So WaitForCacheReady returns only once that view is complete, and
// RolesHeldBy answers the moment it returns, with no retry and no poll. The re-read waits on the
// test's fake (restartRoleViewTimingOutAfter) until WaitForCacheReady waits on the role claim view
// with the view's short reading taken (doneCounted) and the test has read the view's refusal, and
// the view is read the moment WaitForCacheReady returns.
func TestWaitForCacheReadyHoldsUntilTheRoleClaimViewAnswers(t *testing.T) {
	t.Parallel()
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg := openRegistry(t, conn)
	if _, err := reg.SetRole("ses_holder", "m1", "reviewer", false); err != nil {
		t.Fatalf("claim reviewer: %v", err)
	}
	if _, err := reg.SetRole("ses_holder", "m1", "planner", false); err != nil {
		t.Fatalf("claim planner: %v", err)
	}
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	rawRoles, err := js.KeyValue(bus.RoleClaimBucket)
	if err != nil {
		t.Fatalf("open the role bucket: %v", err)
	}
	waitForHeld(t, reg, "ses_holder", "planner", "reviewer")

	reread := restartRoleViewTimingOutAfter(t, conn, reg, rawRoles, 1)

	// What WaitForCacheReady answered, and what the view answered the moment it returned.
	type readiness struct {
		err   error
		roles []string
		read  error
	}
	ctx := &doneCounted{Context: testwait.Context(t)}
	returned := make(chan readiness, 1)
	go func() {
		err := reg.WaitForCacheReady(ctx)
		roles, read := reg.RolesHeldBy("ses_holder")
		returned <- readiness{err: err, roles: roles, read: read}
	}()
	if roles, err := reg.RolesHeldBy("ses_holder"); !errors.Is(err, ErrRoleViewNotReady) {
		t.Fatalf("between the timed-out scan and its re-read, RolesHeldBy(ses_holder) = %v, %v; want ErrRoleViewNotReady", roles, err)
	}
	// WaitForCacheReady reads its context's Done once in the interest watcher's WaitReady, whose
	// select takes it as it begins, and again in the role claim watcher's WaitComplete only once
	// that has read the view short and taken the channel the view's completion closes
	// (completeChanged). So once Done has been called twice, WaitForCacheReady waits in
	// WaitComplete with the view's short reading taken and its wake channel in hand, and the
	// re-read's completion is what wakes it. A WaitForCacheReady that returned with the view short
	// never calls Done a second time, and fails here.
	testwait.Until(t, "WaitForCacheReady to wait on the role claim view, its context's Done called twice", func() (string, bool) {
		if len(returned) != 0 {
			t.Fatalf("WaitForCacheReady returned before the re-read, its context's Done called %d times", ctx.calls.Load())
		}
		calls := ctx.calls.Load()
		return fmt.Sprintf("Done called %d times", calls), calls >= 2
	})
	reread()
	ready := testwait.Receive(t, "WaitForCacheReady to return once the re-read completes", returned)
	if ready.err != nil {
		t.Fatalf("WaitForCacheReady: %v", ready.err)
	}
	if ready.read != nil {
		t.Fatalf("RolesHeldBy(ses_holder) refused the read right after WaitForCacheReady returned: %v", ready.read)
	}
	if !slices.Equal(ready.roles, []string{"planner", "reviewer"}) {
		t.Fatalf("right after WaitForCacheReady returned, ses_holder holds %v, want [planner reviewer]", ready.roles)
	}
}

// doneCounted is a context whose every call of Done is counted, so a test knows how far a wait
// that calls ctx.Done() at known points has gone.
type doneCounted struct {
	context.Context

	calls atomic.Int64
}

func (c *doneCounted) Done() <-chan struct{} {
	c.calls.Add(1)
	return c.Context.Done()
}

// A recreated role bucket's reset empties the view, and the refill after it holds only the claims
// it has delivered so far, so it is no answer until its scan completes: RolesHeldBy refuses it as
// not ready rather than report a holder whose claim the refill has not reached without its role.
// The refill is held at the holder's claim, after another claim, so the test asks while it is
// partial. The reset empties the holders and their revisions both: once the refill completes, a
// claim only the old bucket had (planner's) gives nobody its role, and a claim the new bucket
// writes at a lower revision than the old bucket's claim of the same role (architect's) gives its
// holder the role.
func TestARecreatedRoleBucketsViewAnswersOnlyOnceItsRefillCompletes(t *testing.T) {
	t.Parallel()
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg := openRegistry(t, conn)
	if _, err := reg.SetRole("ses_holder", "m1", "reviewer", false); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	old, err := js.KeyValue(bus.RoleClaimBucket)
	if err != nil {
		t.Fatalf("open the role bucket: %v", err)
	}
	putRoleClaim(t, old, "planner", "ses_holder", reg.now)
	oldArchitect := putRoleClaim(t, old, "architect", "ses_other", reg.now)
	waitFor(t, func() bool {
		holder, errHolder := reg.RolesHeldBy("ses_holder")
		other, errOther := reg.RolesHeldBy("ses_other")
		return errHolder == nil && errOther == nil &&
			slices.Equal(holder, []string{"planner", "reviewer"}) && slices.Equal(other, []string{"architect"})
	})

	var refilling atomic.Bool
	held := make(chan struct{})
	var holdOnce, releaseOnce sync.Once
	released := make(chan struct{})
	release := func() { releaseOnce.Do(func() { close(released) }) }
	t.Cleanup(release)
	cache := gatedRoleClaims{
		r: reg,
		hold: func(key string) {
			if refilling.Load() && key == "reviewer" {
				holdOnce.Do(func() { close(held) })
				<-released
			}
		},
		began: func(read kvwatch.Read) {
			if read.Recreated {
				refilling.Store(true)
			}
		},
	}
	reg.roleWatcher.Stop()
	reg.roleWatcher = kvwatch.New("role claims", reg.roles(), cache)
	reg.roleWatcher.Start()
	waitFor(t, reg.roleWatcher.Ready)
	heldOnceComplete(t, reg, "ses_holder", []string{"planner", "reviewer"}, nil)

	if err := js.DeleteKeyValue(bus.RoleClaimBucket); err != nil {
		t.Fatalf("delete the role bucket: %v", err)
	}
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Replicas: 1, Storage: natsgo.FileStorage})
	if newArchitect := putRoleClaim(t, recreated, "architect", "ses_other", reg.now); newArchitect >= oldArchitect {
		t.Fatalf("the new bucket's architect claim is at revision %d, the old one's at %d; the test needs the new one lower", newArchitect, oldArchitect)
	}
	putRoleClaim(t, recreated, "reviewer", "ses_holder", reg.now)
	if err := reg.Rewatch(context.Background(), conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	testwait.Receive(t, "the refill to reach the holder's claim", held)
	// The copy keeps every holder through the read and removes what the read found no evidence of
	// only once it completes (roleCache), so a read of it while the refill is short of the
	// holder's claim answers the copy the view had, never a part of the new bucket.
	if got := heldRoles(t, reg, "ses_holder"); !slices.Equal(got, []string{"planner", "reviewer"}) {
		t.Fatalf("with the refill short of the holder's claim, ses_holder holds %v, want the copy it had, [planner reviewer]", got)
	}
	release()
	kvwatchtest.AwaitFollowing(t, "the refill of the recreated bucket to complete", reg.roleWatcher)
	if got := heldRoles(t, reg, "ses_holder"); !slices.Equal(got, []string{"reviewer"}) {
		t.Fatalf("after the refill ses_holder holds %v, want [reviewer]: a claim only the old bucket had outlived the reset", got)
	}
	if got := heldRoles(t, reg, "ses_other"); !slices.Equal(got, []string{"architect"}) {
		t.Fatalf("after the refill ses_other holds %v, want [architect]: the old bucket's revision fenced out the new bucket's claim", got)
	}
}

// endingGetKeyValue ends the request (end) when key's interest record is read, and answers that
// read only once the test lets it (answer), which it does once the call that made the request
// has returned: the request has seen its end before the read answers. answered closes once that
// read has answered.
type endingGetKeyValue struct {
	natsgo.KeyValue

	key       string
	end       context.CancelFunc
	answer    <-chan struct{}
	answered  chan struct{}
	answering sync.Once
}

func (kv *endingGetKeyValue) Get(key string) (natsgo.KeyValueEntry, error) {
	if key != kv.key {
		return kv.KeyValue.Get(key)
	}
	defer kv.answering.Do(func() { close(kv.answered) })
	kv.end()
	<-kv.answer
	return kv.KeyValue.Get(key)
}

// A claim whose request ends while its old holder's role topic is being removed has landed, so it
// is a success. The cleanup the request's end cut short failed at nothing: it is logged at INFO,
// and nothing is logged at WARN. The topic it leaves gives the old holder no role. The read the
// request's end cut short answers only once the claim has returned, and the lines are read once it
// has: it is the claim's one piece of work still running then, and every line the claim writes is
// written before it returns.
func TestAClaimWhoseRequestEndsDuringItsCleanupSucceedsAndLogsNoFailure(t *testing.T) {
	t.Parallel()
	logs, logger := ownLogs()
	conn, cleanup := connectNATS(t)
	defer cleanup()
	const (
		oldSession = "ses_old"
		newSession = "ses_new"
		role       = "legion-controller"
		roleTopic  = "notifications.role.legion-controller"
	)
	reg, kv := coldRegistry(t, conn)
	reg.log = logger
	putInterest(t, kv, Interest{SessionID: oldSession, MachineID: "example-host", Topics: []string{roleTopic, "notifications.slack.>"}}, reg.now)
	if _, err := reg.roleKV.Put(role, []byte(oldSession)); err != nil {
		t.Fatalf("seed role claim: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	answer := make(chan struct{})
	var letting sync.Once
	letAnswer := func() { letting.Do(func() { close(answer) }) }
	t.Cleanup(letAnswer)
	ending := &endingGetKeyValue{KeyValue: kv, key: oldSession, end: cancel, answer: answer, answered: make(chan struct{})}
	interests := reg.watcher.KV()
	reg.watcher = kvwatch.New("interest registry", bus.NewKeyValue(ending, interests.WatchHandle(), interests.Streams()), interestCache{reg}, kvwatch.WithLogger(logger))

	claim := RoleClaimRequest{SessionID: newSession, MachineID: "example-host", ClientInstanceID: roleClaimInstance, Role: role}
	var err error
	testwait.Returns(t, "the claim to answer once its request ended during the cleanup", func() {
		_, err = reg.SetRoleWithPrevious(ctx, claim)
	})
	if err != nil {
		t.Fatalf("a claim that landed returned %v when its request ended during the cleanup, want success", err)
	}
	assertRoleHolder(t, reg, role, newSession)
	letAnswer()
	testwait.Receive(t, "the read of the old holder's record the request's end cut short to answer", ending.answered)
	out := logs.String()
	if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "the request ended first") {
		t.Fatalf("the cut-short cleanup was not logged at INFO:\n%s", out)
	}
	if strings.Contains(out, "level=WARN") {
		t.Fatalf("a cleanup the request's end cut short was logged at WARN:\n%s", out)
	}
	t.Logf("cut-short cleanup: %s", strings.TrimSpace(out))
}

// A claim write this registry made on the old role bucket, whose answer arrives after a watch has
// installed the recreated bucket, must not land in the new bucket's view: its revision is the old
// bucket's, and as the view's fence it would drop every lower revision the new bucket delivers,
// leaving the role with the old write's holder (or none) until the new bucket's numbering passes
// it. The control is the same write through the view's own handle: it applies, and fences out an
// older delivery.
func TestAClaimWriteAnsweredAcrossARecreatedRoleBucketLeavesTheNewBucketsView(t *testing.T) {
	t.Parallel()
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg := openRegistry(t, conn)
	// The watcher is stopped so only this test writes to the view; its epoch is still the one a
	// write through its handle carries (kvwatch.Watcher.Handle).
	reg.roleWatcher.Stop()
	epoch := reg.roleWatcher.Epoch()

	reg.setOwnRoleHolder("reviewer", "ses_old_bucket", 7, epoch+1)
	reg.applyRoleClaim(roleClaimEntry("reviewer", "ses_new_bucket", 1))
	if got := holderOf(reg, "reviewer"); got != "ses_new_bucket" {
		t.Fatalf("after a watch moved the epoch, reviewer's holder = %q, want ses_new_bucket: the old bucket's claim write at revision 7 fenced out the new bucket's revision 1", got)
	}

	reg.setOwnRoleHolder("planner", "ses_written", 7, epoch)
	reg.applyRoleClaim(roleClaimEntry("planner", "ses_delayed", 6))
	if got := holderOf(reg, "planner"); got != "ses_written" {
		t.Fatalf("with no recreate, planner's holder = %q, want ses_written: a delayed delivery at revision 6 undid the registry's own write at 7", got)
	}
}

// roleClaimEntry is a record of the role bucket the watcher could have delivered: role's claim
// naming holder, at revision.
func roleClaimEntry(role, holder string, revision uint64) natsgo.KeyValueEntry {
	return watchedRoleClaim{role: role, value: []byte(`{"holder_session_id":"` + holder + `"}`), revision: revision}
}

type watchedRoleClaim struct {
	role     string
	value    []byte
	revision uint64
}

func (e watchedRoleClaim) Bucket() string               { return bus.RoleClaimBucket }
func (e watchedRoleClaim) Key() string                  { return e.role }
func (e watchedRoleClaim) Value() []byte                { return e.value }
func (e watchedRoleClaim) Revision() uint64             { return e.revision }
func (e watchedRoleClaim) Created() time.Time           { return time.Time{} }
func (e watchedRoleClaim) Delta() uint64                { return 0 }
func (e watchedRoleClaim) Operation() natsgo.KeyValueOp { return natsgo.KeyValuePut }
