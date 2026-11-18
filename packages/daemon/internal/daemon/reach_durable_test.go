package daemon

// The reach keeper's records outlive the daemon at the two boundaries where an act and its record
// meet the store (R5): a relaunch and the record that refuses a second one within 6 hours, and a
// notice and the record that says it was told. Each test runs the keeper over a real database,
// interrupts one boundary — a write the store refuses, or a daemon that ends before its second
// write — and boots a second daemon onto the same database, which reads only what the first left
// there.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fleetalert"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// anyTreeStore is the store with the tree lifecycle's launch check answered yes: the claims these
// tests relaunch stand in no tree the database has opened, and what they read back is the store's
// own record of each claim and its launch.
type anyTreeStore struct{ *store.Store }

func (anyTreeStore) CheckLaunch(context.Context, supervise.Claim) error { return nil }

// durableReach is one database and the daemons booted onto it, with the keeper's clock.
type durableReach struct {
	t    *testing.T
	pool *store.Pool
	st   *store.Store

	mu  sync.Mutex
	now time.Time
	// noLive is whether Envoy's list holds no live session at all.
	noLive bool
}

func newDurableReach(t *testing.T) *durableReach {
	t.Helper()
	pool := isolatedOutboxPool(t)
	st, err := store.Open(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	return &durableReach{t: t, pool: pool, st: st, now: reachBoot}
}

func (d *durableReach) clock() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.now
}

// at moves the keeper's clock to boot plus after.
func (d *durableReach) at(after time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.now = reachBoot.Add(after)
}

// noLiveSessions sets whether Envoy's list holds no live session at all, Legion's or any other.
func (d *durableReach) noLiveSessions(none bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.noLive = none
}

// options is a keeper's options over the database: Envoy's list holds one live session, none of
// the claims' (none at all while noLiveSessions says so), and the reach record is written by put.
func (d *durableReach) options(daemonController bool, put func(context.Context, claim.Token, supervise.Reach) (bool, error)) reachKeeperOptions {
	return reachKeeperOptions{
		log: quietLogger(), project: "LEGION", boot: reachBoot, daemonController: daemonController, now: d.clock,
		list: func(context.Context) ([]notify.ListedSession, error) {
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.noLive {
				return nil, nil
			}
			return []notify.ListedSession{{Session: "ses_other", State: notify.StateLive, LastSeen: d.now}}, nil
		},
		put: put, alerts: fleetalert.New(quietLogger(), "legion"),
	}
}

// daemon is one daemon's supervision of claims over the database: a bare supervisor on the fake
// runtime, the store as its claims' store, and a quiet activity report of every launch, which the
// relaunch guard lets through (activity, where it is not nil, answers it instead).
func (d *durableReach) daemon(k *reachKeeper, activity *supervise.ActivityReport, claims ...supervise.Claim) *supervision {
	d.t.Helper()
	sup, _ := newPluginSupervisor(d.t, api.DaemonAPIVersion, true)
	sup.deps.Store = anyTreeStore{d.st}
	report := supervise.ActivityReport{At: reachBoot.Add(-50 * time.Minute)}
	if activity != nil {
		report = *activity
	}
	sup.deps.ClaimActivity = func(context.Context, claim.Token, uint64) (supervise.ActivityReport, bool, error) {
		return report, true, nil
	}
	if _, err := sup.restore(context.Background(), claims); err != nil {
		d.t.Fatalf("restore the claims: %v", err)
	}
	return &supervision{log: quietLogger(), supervisor: sup, reach: k}
}

// put writes c as the store's claim.
func (d *durableReach) put(c supervise.Claim) {
	d.t.Helper()
	if err := d.st.PutClaim(context.Background(), c); err != nil {
		d.t.Fatalf("put the claim %s: %v", c.Token, err)
	}
}

// stored is every claim the database holds, read as a booting daemon and its minute sweep read
// them.
func (d *durableReach) stored() []supervise.Claim {
	d.t.Helper()
	claims, err := d.st.Claims(context.Background())
	if err != nil {
		d.t.Fatalf("read the claims: %v", err)
	}
	return claims
}

// claim is token's claim as the database holds it.
func (d *durableReach) claim(token claim.Token) supervise.Claim {
	d.t.Helper()
	for _, c := range d.stored() {
		if c.Token == token {
			return c
		}
	}
	d.t.Fatalf("the database holds no claim %s", token)
	return supervise.Claim{}
}

// report writes a quiet activity report of token's launch generation, as its session sends one.
func (d *durableReach) report(token claim.Token, generation uint64) {
	d.t.Helper()
	reported := reachBoot.Add(-50 * time.Minute)
	if known, err := d.st.PutClaimActivity(context.Background(), token, generation, claim.SessionActivity{At: reported}, reported); err != nil || !known {
		d.t.Fatalf("report the activity of %s at generation %d = %t, %v", token, generation, known, err)
	}
}

// refuseReachWrites makes the database refuse every write of a claim's reach record that changes
// field, as a store that fails at that one write does, until the function it answers lifts it. It
// refuses the write whatever writes it: the keeper's own write of the record, or a write of the
// record in the transaction of something else.
func (d *durableReach) refuseReachWrites(field string) func() {
	d.t.Helper()
	ctx := context.Background()
	if _, err := d.pool.Exec(ctx, fmt.Sprintf(`create function refuse_reach_write() returns trigger language plpgsql as $$
		begin
			if new.reach->'%[1]s' is distinct from old.reach->'%[1]s' then
				raise exception 'the store refuses this write of the reach record''s %[1]s';
			end if;
			return new;
		end $$;
		create trigger refuse_reach_write before update of reach on claims for each row execute function refuse_reach_write();`, field)); err != nil {
		d.t.Fatalf("make the store refuse writes of %s: %v", field, err)
	}
	return func() {
		d.t.Helper()
		if _, err := d.pool.Exec(ctx, `drop trigger refuse_reach_write on claims; drop function refuse_reach_write()`); err != nil {
			d.t.Fatalf("lift the refusal of %s: %v", field, err)
		}
	}
}

// failAfter is the store's write of a reach record, refused once the record carries what ended
// says it carries: the write of a daemon that ended before it made it.
func (d *durableReach) failAfter(ended func(supervise.Reach) bool) func(context.Context, claim.Token, supervise.Reach) (bool, error) {
	return func(ctx context.Context, token claim.Token, reach supervise.Reach) (bool, error) {
		if ended(reach) {
			return false, errors.New("the daemon ended before it made this write")
		}
		return d.st.PutClaimReach(ctx, token, reach)
	}
}

// workflow is the workflow's half the keeper's notices are written through, over the database: a
// registered controller and the admission that queues its notices.
func (d *durableReach) workflow() *workflowRuntime {
	d.t.Helper()
	ctx := context.Background()
	token, err := claim.ProjectToken("LEGION")
	if err != nil {
		d.t.Fatal(err)
	}
	generation, err := d.st.MintController(ctx, token, []byte("capability"))
	if err != nil {
		d.t.Fatalf("mint the controller capability: %v", err)
	}
	if ok, err := d.st.RegisterController(ctx, token, generation, "ses-controller", []byte("secret"), time.Now(), 0); err != nil || !ok {
		d.t.Fatalf("register the controller = %t, %v", ok, err)
	}
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	admission := admit.New(records, engine, 1, "LEGION", quietLogger())
	return &workflowRuntime{pool: d.pool, records: records, admission: admission, engine: engine,
		handlers: []intake.Handler{engine, admission}, dispatchProject: "LEGION", log: quietLogger()}
}

// The notices the tests count: each a notice of one kind, which is queued under the outbox kind
// that notice kind takes (record.ControllerNotice.OutboxKind, record.Notice.OutboxKind).
var (
	unreachableList = record.ControllerNotice{Kind: record.UnreachableClaimsNotice}
	unreachableExit = record.ControllerNotice{Kind: record.UnreachableExitNotice}
	noLiveSession   = record.ControllerNotice{Kind: record.EnvoyNoLiveSessionNotice}
	lockedOut       = record.ControllerNotice{Kind: record.WorkerLockedOutNotice}
	architectToldOf = record.Notice{Kind: record.ClaimOutOfReachNotice}
)

// delivered is how many rows of notice's kind the outbox holds, under the outbox kind that notice
// kind is queued in, which it then finishes, as their delivery does: a controller notice of the
// same kind waiting unsent holds back the next one (admit's wakeController), and a delivered one
// does not.
func (d *durableReach) delivered(notice record.OutboxPayload) int {
	d.t.Helper()
	var kind record.NoticeKind
	switch n := notice.(type) {
	case record.ControllerNotice:
		kind = n.Kind
	case record.Notice:
		kind = n.Kind
	default:
		d.t.Fatalf("count %T rows: not a notice", notice)
	}
	tag, err := d.pool.Exec(context.Background(), `delete from outbox where kind = $1 and payload->>'kind' = $2`,
		string(notice.OutboxKind()), string(kind))
	if err != nil {
		d.t.Fatalf("finish the %s notices: %v", kind, err)
	}
	return int(tag.RowsAffected())
}

// relaunchedOnce relaunches worker once on the first daemon, marked out of reach by its sweep, and
// answers the claim as the database holds it once the relaunch's launch is written. put is that
// daemon's write of the reach record; relaunch runs the controller's relaunch, which a daemon that
// ends inside it never returns from.
func (d *durableReach) relaunchedOnce(worker supervise.Claim, put func(context.Context, claim.Token, supervise.Reach) (bool, error), relaunch func(func() error)) supervise.Claim {
	d.t.Helper()
	d.put(worker)
	k := newReachKeeper(d.options(false, put))
	s := d.daemon(k, nil, worker)
	d.at(21 * time.Minute)
	k.sweep(context.Background(), d.stored())
	if !d.claim(worker.Token).Reach.Unreachable() {
		d.t.Fatal("the first daemon did not mark the worker out of reach")
	}
	m, _ := s.supervisor.Machine(worker.Token)
	generation := m.Claim().Generation
	relaunch(func() error { return s.relaunch(context.Background(), m) })
	testwait.Eventually(d.t, "the relaunch's launch to be written", func() bool { return d.claim(worker.Token).Generation > generation })
	return d.claim(worker.Token)
}

// relaunchAfterRestart boots a second daemon onto the database 3 hours after the first booted,
// with worker's relaunched process working, and answers its controller's relaunch of worker.
func (d *durableReach) relaunchAfterRestart(worker supervise.Claim) error {
	d.t.Helper()
	restarted := d.claim(worker.Token)
	restarted.State = supervise.StateWorking
	d.put(restarted)
	d.at(3 * time.Hour)
	s := d.daemon(newReachKeeper(d.options(false, d.st.PutClaimReach)), nil, d.claim(worker.Token))
	m, _ := s.supervisor.Machine(worker.Token)
	return s.relaunch(context.Background(), m)
}

// A relaunch whose reach write the store refuses is still refused a second time within 6 hours by
// the daemon that boots next: the record is written with the launch itself.
func TestARelaunchIsRefusedForSixHoursAcrossARefusedReachWriteAndARestart(t *testing.T) {
	d := newDurableReach(t)
	worker := waitingQuietly(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer"))
	var lift func()
	relaunched := d.relaunchedOnce(worker, d.st.PutClaimReach, func(relaunch func() error) {
		lift = d.refuseReachWrites("relaunchedAt")
		if err := relaunch(); err != nil {
			t.Fatalf("the controller's relaunch of the marked worker = %v", err)
		}
	})
	lift()
	if !relaunched.Reach.RelaunchedAt.IsZero() || relaunched.Reach.Relaunched {
		t.Fatalf("the keeper's record of the relaunch reached the store: %+v; want the store to have refused it", relaunched.Reach)
	}
	if err := d.relaunchAfterRestart(worker); !errors.Is(err, supervise.ErrRelaunchedRecently) {
		t.Fatalf("a second relaunch 3 hours after the first, whose reach write the store refused, on the next daemon = %v, want ErrRelaunchedRecently", err)
	}
}

// A daemon that ends between the relaunch's launch and its reach write leaves the next daemon the
// 6-hour refusal all the same.
func TestARelaunchIsRefusedForSixHoursAcrossADaemonThatEndsBeforeItsReachWrite(t *testing.T) {
	d := newDurableReach(t)
	worker := waitingQuietly(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer"))
	ended := make(chan struct{})
	t.Cleanup(func() { close(ended) })
	put := func(ctx context.Context, token claim.Token, reach supervise.Reach) (bool, error) {
		if !reach.RelaunchedAt.IsZero() {
			// The daemon ends here: this write is never made, and nothing after it runs.
			<-ended
			return false, errors.New("the daemon ended before it made this write")
		}
		return d.st.PutClaimReach(ctx, token, reach)
	}
	relaunched := d.relaunchedOnce(worker, put, func(relaunch func() error) { go relaunch() })
	if !relaunched.Reach.RelaunchedAt.IsZero() || relaunched.Reach.Relaunched {
		t.Fatalf("the keeper's record of the relaunch reached the store: %+v; want the daemon to have ended first", relaunched.Reach)
	}
	if err := d.relaunchAfterRestart(worker); !errors.Is(err, supervise.ErrRelaunchedRecently) {
		t.Fatalf("a second relaunch 3 hours after the first, whose daemon ended before its reach write, on the next daemon = %v, want ErrRelaunchedRecently", err)
	}
}

// The daemon relaunches its own controller at most once in 6 hours, across a reach write the store
// refused and a restart.
func TestTheDaemonRelaunchesItsControllerOnceAcrossARefusedReachWriteAndARestart(t *testing.T) {
	d := newDurableReach(t)
	project, _ := claim.ProjectToken("legion")
	token := claim.ControllerToken(project)
	d.put(supervise.Claim{Token: token, Project: project, Role: claim.RoleController, Generation: 1,
		State: supervise.StateIdle, Session: "ses_controller", LastReady: reachBoot.Add(-time.Hour),
		Locator: pluginTestLocator(token), PluginContract: api.DaemonAPIVersion})
	d.report(token, 1)
	boot := func() *reachKeeper {
		k := newReachKeeper(d.options(true, d.st.PutClaimReach))
		k.relaunchController = d.daemon(k, nil, d.claim(token)).relaunchOwnController
		return k
	}

	first := boot()
	lift := d.refuseReachWrites("relaunchedAt")
	d.at(21 * time.Minute)
	first.sweep(context.Background(), d.stored())
	first.wait()
	lift()
	if got := d.claim(token).Generation; got != 2 {
		t.Fatalf("the controller is at generation %d after the first daemon's sweep, want its relaunch at 2", got)
	}

	// Its relaunched process came back and reported; it is out of reach again on the next daemon.
	restarted := d.claim(token)
	restarted.State = supervise.StateIdle
	d.put(restarted)
	d.report(token, 2)
	second := boot()
	d.at(25 * time.Minute)
	second.sweep(context.Background(), d.stored())
	second.wait()
	if got := d.claim(token).Generation; got != 2 {
		t.Fatalf("the controller is at generation %d after the next daemon's sweep, want no second relaunch within 6 hours", got)
	}
}

// notifying is a keeper whose controller notices are written through w, and whose reach record is
// written by put.
func (d *durableReach) notifying(w *workflowRuntime, put func(context.Context, claim.Token, supervise.Reach) (bool, error)) *reachKeeper {
	o := d.options(false, put)
	o.tell = w.tellControllerQueued
	return newReachKeeper(o)
}

// sweeps runs one sweep a minute from minute on each keeper in turn, a restart between each two,
// and counts the notices of notice's kind each left in the outbox.
func (d *durableReach) sweeps(minute int, notice record.OutboxPayload, keepers ...*reachKeeper) int {
	d.t.Helper()
	told := 0
	for _, k := range keepers {
		for range 2 {
			d.at(time.Duration(minute) * time.Minute)
			k.sweep(context.Background(), d.stored())
			told += d.delivered(notice)
			minute++
		}
	}
	return told
}

// A claim reported out of reach is not reported again by the next daemon, across a write of its
// reported mark the store refused: while the mark cannot be written, neither can the list.
func TestAnUnreachableClaimIsListedOnceAcrossARefusedReportedWriteAndARestart(t *testing.T) {
	d := newDurableReach(t)
	w := d.workflow()
	d.put(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer"))

	lift := d.refuseReachWrites("reported")
	refused := d.sweeps(21, unreachableList, d.notifying(w, d.st.PutClaimReach))
	lift()
	if refused != 0 {
		t.Fatalf("the controller was told %d lists while the store refused their mark, want none", refused)
	}
	told := d.sweeps(23, unreachableList,
		d.notifying(w, d.st.PutClaimReach), d.notifying(w, d.st.PutClaimReach))
	if told != 1 {
		t.Fatalf("the controller was told %d lists naming the claim across two restarts once its mark could be written, want 1", told)
	}
}

// A daemon that ends between a list's commit and its reported mark leaves the next daemon nothing
// to report again.
func TestAnUnreachableClaimIsListedOnceAcrossADaemonThatEndsBeforeItsReportedWrite(t *testing.T) {
	d := newDurableReach(t)
	w := d.workflow()
	d.put(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer"))

	ended := d.failAfter(func(reach supervise.Reach) bool { return reach.Reported })
	if told := d.sweeps(21, unreachableList, d.notifying(w, ended)); told != 1 {
		t.Fatalf("the first daemon told %d lists naming the claim, want the one it told before it ended", told)
	}
	if told := d.sweeps(23, unreachableList, d.notifying(w, d.st.PutClaimReach)); told != 0 {
		t.Fatalf("the next daemon told %d lists naming the claim the first had told before it ended, want none", told)
	}
}

// A per-claim notice, the exit of a marked claim owing no task, is told once across a write of its
// mark the store refused and a restart; and one the outbox did not take is told at the next sweep
// rather than lost with the launch it was of.
func TestAnUnreachableExitIsToldOnceAcrossARefusedMarkWriteAndARestart(t *testing.T) {
	d := newDurableReach(t)
	w := d.workflow()
	planner := reachClaim(t, "LEGION-208", "LEGION-210", claim.RolePlanner, "ses_planner")
	d.put(planner)
	d.sweeps(21, unreachableExit, d.notifying(w, d.st.PutClaimReach))

	// Its process exits at its Envoy role claim, and the daemon's own supervision launches it again.
	planner = d.claim(planner.Token)
	planner.Generation++
	planner.State = supervise.StateLaunching
	d.put(planner)
	lift := d.refuseReachWrites("exitTold")
	refused := d.sweeps(23, unreachableExit, d.notifying(w, d.st.PutClaimReach))
	lift()
	if refused != 0 {
		t.Fatalf("the controller was told %d exits while the store refused their mark, want none", refused)
	}
	told := d.sweeps(25, unreachableExit,
		d.notifying(w, d.st.PutClaimReach), d.notifying(w, d.st.PutClaimReach))
	if told != 1 {
		t.Fatalf("the controller was told %d exits of the planner across two restarts once its mark could be written, want 1", told)
	}
}

// architectNotified is a keeper whose architects' notices are written through w, and whose reach
// record is written by put.
func (d *durableReach) architectNotified(w *workflowRuntime, put func(context.Context, claim.Token, supervise.Reach) (bool, error)) *reachKeeper {
	o := d.options(false, put)
	o.enqueue = w.enqueueReachNotice
	return newReachKeeper(o)
}

// The tree architect's notice of a claim out of reach is told once across a write of its mark the
// store refused and a restart.
func TestTheArchitectIsToldOfAnUnreachableClaimOnceAcrossARefusedMarkWriteAndARestart(t *testing.T) {
	d := newDurableReach(t)
	w := d.workflow()
	d.put(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer"))

	lift := d.refuseReachWrites("architectTold")
	refused := d.sweeps(21, architectToldOf, d.architectNotified(w, d.st.PutClaimReach))
	lift()
	if refused != 0 {
		t.Fatalf("the architect was told %d times while the store refused the notice's mark, want none", refused)
	}
	told := d.sweeps(23, architectToldOf,
		d.architectNotified(w, d.st.PutClaimReach), d.architectNotified(w, d.st.PutClaimReach))
	if told != 1 {
		t.Fatalf("the architect was told %d times of the implementer across two restarts once the mark could be written, want 1", told)
	}
}

// A daemon that ends between the architect's notice and its mark leaves the next daemon nothing
// to tell it again.
func TestTheArchitectIsToldOfAnUnreachableClaimOnceAcrossADaemonThatEndsBeforeItsMarkWrite(t *testing.T) {
	d := newDurableReach(t)
	w := d.workflow()
	d.put(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer"))

	ended := d.failAfter(func(reach supervise.Reach) bool { return reach.ArchitectTold })
	if told := d.sweeps(21, architectToldOf, d.architectNotified(w, ended)); told != 1 {
		t.Fatalf("the first daemon told the architect %d times, want the once it told it before it ended", told)
	}
	if told := d.sweeps(23, architectToldOf, d.architectNotified(w, d.st.PutClaimReach)); told != 0 {
		t.Fatalf("the next daemon told the architect %d times of a claim the first had told it of before it ended, want none", told)
	}
}

// A guard's refusal whose record the store refused is answered as a failure: the notice that the
// guard would now let the claim through counts from that record, so nothing may come of it.
func TestAGuardRefusalTheStoreDidNotRecordIsAnsweredAsAFailure(t *testing.T) {
	d := newDurableReach(t)
	worker := waitingQuietly(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer"))
	d.put(worker)
	k := newReachKeeper(d.options(false, d.st.PutClaimReach))
	s := d.daemon(k, &supervise.ActivityReport{At: reachBoot.Add(-50 * time.Minute), Background: true, Kinds: []string{"subagent"}}, worker)
	d.at(21 * time.Minute)
	k.sweep(context.Background(), d.stored())

	lift := d.refuseReachWrites("guardRefusedAt")
	defer lift()
	m, _ := s.supervisor.Machine(worker.Token)
	err := s.relaunch(context.Background(), m)
	if !errors.Is(err, supervise.ErrRelaunchGuard) || !strings.Contains(err.Error(), "could not write the claim's reach record") {
		t.Fatalf("a guard's refusal the store did not record = %v, want the refusal and that its record was not written", err)
	}
}

// The relaunch's launch records the generation it left the claim at beside its moment, so the next
// daemon, taking in a relaunch the first never wrote its record of, counts the claim's launches
// from that generation: a claim that exited at its Envoy role claim and was launched again in that
// gap is told as an exit once, and the relaunch's own launch is told as none.
func TestAnExitInTheGapAfterARelaunchIsToldOnceByTheNextDaemon(t *testing.T) {
	d := newDurableReach(t)
	w := d.workflow()
	planner := reachClaim(t, "LEGION-208", "LEGION-210", claim.RolePlanner, "ses_planner")
	d.put(planner)
	first := d.notifying(w, d.st.PutClaimReach)
	s := d.daemon(first, nil, planner)
	d.sweeps(21, unreachableExit, first)
	if !d.claim(planner.Token).Reach.Unreachable() {
		t.Fatal("the first daemon did not mark the planner out of reach")
	}

	m, _ := s.supervisor.Machine(planner.Token)
	lift := d.refuseReachWrites("relaunchedAt")
	if err := s.relaunch(context.Background(), m); err != nil {
		t.Fatalf("the controller's relaunch of the marked planner = %v", err)
	}
	lift()
	relaunched := d.claim(planner.Token)
	if relaunched.Generation != planner.Generation+1 || !relaunched.Reach.RelaunchedAt.IsZero() {
		t.Fatalf("after the relaunch the claim is at generation %d with the keeper's record %+v; want %d and the record unwritten",
			relaunched.Generation, relaunched.Reach, planner.Generation+1)
	}
	// In the gap the relaunched process exits at its Envoy role claim, and the daemon's own
	// supervision launches it again.
	exited := relaunched
	exited.Generation++
	exited.State = supervise.StateLaunching
	d.put(exited)

	if told := d.sweeps(30, unreachableExit, d.notifying(w, d.st.PutClaimReach), d.notifying(w, d.st.PutClaimReach)); told != 1 {
		t.Fatalf("the next daemons told %d exits of the planner launched again after its relaunch, want 1", told)
	}
}

// sweepEach runs one sweep a minute of k from minute from to minute to, both included, and counts
// the notices of notice's kind it left in the outbox.
func (d *durableReach) sweepEach(from, to int, notice record.OutboxPayload, k *reachKeeper) int {
	d.t.Helper()
	told := 0
	for minute := from; minute <= to; minute++ {
		d.at(time.Duration(minute) * time.Minute)
		k.sweep(context.Background(), d.stored())
		told += d.delivered(notice)
	}
	return told
}

// projectNotifying is notifying with the project's own notice records in the database.
func (d *durableReach) projectNotifying(w *workflowRuntime) *reachKeeper {
	k := d.notifying(w, d.st.PutClaimReach)
	k.notices = d.st
	return k
}

// The notice that Envoy lists no live session is told once for a run of such lists, across a
// restart: its told mark is a record of the project's, written with it. Once a list holds a live
// session again, the next run of such lists is told once more.
func TestANoLiveSessionNoticeIsToldOnceARunAcrossARestart(t *testing.T) {
	d := newDurableReach(t)
	w := d.workflow()
	d.noLiveSessions(true)
	if told := d.sweepEach(1, 25, noLiveSession, d.projectNotifying(w)); told != 1 {
		t.Fatalf("the first daemon told %d notices of 25 minutes of lists with no live session, want 1", told)
	}
	next := d.projectNotifying(w)
	if told := d.sweepEach(26, 50, noLiveSession, next); told != 0 {
		t.Fatalf("the next daemon told %d notices of the same run of lists with no live session, want none", told)
	}

	d.noLiveSessions(false)
	d.sweepEach(51, 51, noLiveSession, next)
	d.noLiveSessions(true)
	if told := d.sweepEach(52, 75, noLiveSession, next); told != 1 {
		t.Fatalf("a run of lists with no live session after one that held a live session was told %d times, want 1", told)
	}
	if told := d.sweepEach(76, 100, noLiveSession, d.projectNotifying(w)); told != 0 {
		t.Fatalf("the daemon after that told %d notices of its run, want none", told)
	}
}

// booted is a keeper of a daemon that has just booted onto d's database, whose R6 writer logs to
// logged and has read the lines the daemon before it left open, as openSupervision loads it.
func (d *durableReach) booted(logged *testlog.Buffer) *reachKeeper {
	d.t.Helper()
	return d.bootedAs(logged, false, true)
}

// bootedAs is booted in a configuration of its own: whether the daemon runs its controller, and
// whether it reads Envoy's list at all (a deployment with no Envoy URL reads none).
func (d *durableReach) bootedAs(logged *testlog.Buffer, daemonController, readsList bool) *reachKeeper {
	d.t.Helper()
	o := d.options(daemonController, d.st.PutClaimReach)
	if !readsList {
		o.list = nil
	}
	o.alerts = fleetalert.New(slog.New(slog.NewTextHandler(logged, nil)), "LEGION")
	if err := o.alerts.Load(context.Background(), fleetAlertLines{st: d.st, project: "LEGION"}); err != nil {
		d.t.Fatalf("load the open fleet alert lines: %v", err)
	}
	return newReachKeeper(o)
}

// A line the store holds open is closed by the first sweep of a daemon that boots onto it when its
// case does not hold now: a case this configuration does not judge (the controller's under an
// operator's controller, the list's where no list is read), and a case it judges and finds ended
// (the controller back in reach). So a switch of the controller's mode and back, or a rollback to
// a release that closed its cases in memory alone and a roll forward, leaves no line open that
// would silence the next line of its case (R6). A case that still holds keeps its line.
func TestAStoredLineWhoseCaseNoLongerHoldsIsClosedByTheFirstSweep(t *testing.T) {
	project, _ := claim.ProjectToken("legion")
	controller := claim.ControllerToken(project)
	controllerClaim := func(marked bool) supervise.Claim {
		c := supervise.Claim{Token: controller, Project: project, Role: claim.RoleController, Generation: 2,
			State: supervise.StateIdle, Session: "ses_controller", LastReady: reachBoot.Add(-time.Hour),
			Locator: pluginTestLocator(controller), ActivityAt: reachBoot}
		if marked {
			c.LastRelaunch, c.LastRelaunchGeneration = reachBoot.Add(-10*time.Minute), 2
		}
		return c
	}
	for _, tc := range []struct {
		name             string
		stored           []fleetalert.Line
		daemonController bool
		readsList        bool
		claims           []supervise.Claim
		markedReach      bool
		open             bool
	}{
		{name: "the controller's line under an operator's controller", daemonController: false, readsList: true,
			stored: []fleetalert.Line{{Word: fleetalert.ControllerUnreachable.Word, Claim: string(controller)},
				{Word: fleetalert.ControllerRelaunchRefused.Word, Claim: string(controller)}}},
		{name: "the list's lines where no list is read", daemonController: true, readsList: false,
			stored: []fleetalert.Line{{Word: fleetalert.NoLiveSession.Word}, {Word: fleetalert.SessionListUnreadable.Word}}},
		{name: "the controller's line of a controller back in reach", daemonController: true, readsList: true,
			stored: []fleetalert.Line{{Word: fleetalert.ControllerUnreachable.Word, Claim: string(controller)}},
			claims: []supervise.Claim{controllerClaim(false)}},
		{name: "the controller's line of a controller gone from the claims", daemonController: true, readsList: true,
			stored: []fleetalert.Line{{Word: fleetalert.ControllerUnreachable.Word, Claim: string(controller)}}},
		{name: "the controller's line of a controller still out of reach", daemonController: true, readsList: true,
			stored: []fleetalert.Line{{Word: fleetalert.ControllerUnreachable.Word, Claim: string(controller)}},
			claims: []supervise.Claim{controllerClaim(true)}, markedReach: true, open: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDurableReach(t)
			for _, c := range tc.claims {
				d.put(c)
				if tc.markedReach {
					marked := supervise.Reach{Session: c.Session, FirstReady: c.LastReady, Since: reachBoot.Add(-30 * time.Minute),
						Generation: 2, Relaunched: true, RelaunchedAt: c.LastRelaunch}
					if known, err := d.st.PutClaimReach(context.Background(), c.Token, marked); err != nil || !known {
						t.Fatalf("mark the controller out of reach = %t, %v", known, err)
					}
				}
			}
			for _, l := range tc.stored {
				if err := d.st.PutFleetAlertLine(context.Background(), store.FleetAlertLine{Project: "LEGION", Case: l.Word, Claim: l.Claim}); err != nil {
					t.Fatalf("store the open line %+v: %v", l, err)
				}
			}
			logged := &testlog.Buffer{}
			k := d.bootedAs(logged, tc.daemonController, tc.readsList)
			d.at(time.Minute)
			k.sweep(context.Background(), d.stored())
			k.wait()
			open, err := d.st.FleetAlertLines(context.Background(), "LEGION")
			if err != nil {
				t.Fatalf("read the open lines: %v", err)
			}
			if got := len(open) == len(tc.stored); got != tc.open {
				t.Fatalf("after the first sweep the store holds the open lines %+v, want them kept %t", open, tc.open)
			}

			// The next line of that key: written for a case that ended, not for one that holds.
			next := d.bootedAs(logged, tc.daemonController, tc.readsList)
			for _, l := range tc.stored {
				c := fleetalert.Case{Word: l.Word, Sentence: "the next case"}
				if wrote := next.alerts.Open(c, l.Claim); wrote == tc.open {
					t.Fatalf("the next daemon's line of %+v written %t, want %t", l, wrote, !tc.open)
				}
			}
		})
	}
}

// R6's line is written once until its case ends, and a restart ends no case (R6): a run of lists
// with no live session that holds through a restart writes its line once, a lock-out line the last
// daemon wrote is ended by the next one, whose worker stream holds no such lock-out, and a case
// that ended and came back writes its line again.
func TestALineIsWrittenOnceUntilItsCaseEndsAcrossARestart(t *testing.T) {
	d := newDurableReach(t)
	logged := &testlog.Buffer{}
	lines := func(word string) int { return strings.Count(logged.String(), "case="+word) }
	d.noLiveSessions(true)
	first := d.booted(logged)
	first.alerts.Open(fleetalert.NewerProcessRefused, "legion-legion-legion-209-implementer")
	d.sweepEach(1, 25, noLiveSession, first)
	first.wait()
	if got := lines(fleetalert.NoLiveSession.Word); got != 1 {
		t.Fatalf("the first daemon wrote the no-live line %d times in 25 minutes, want once", got)
	}

	next := d.booted(logged)
	// The next daemon's minute sweep resends its lock-out notices, whose stream holds no lock-out.
	newLockOutNotices(lockOutOptions{log: quietLogger(), project: "LEGION", alerts: next.alerts}).resend(context.Background())
	d.sweepEach(26, 50, noLiveSession, next)
	next.wait()
	if got := lines(fleetalert.NoLiveSession.Word); got != 1 {
		t.Fatalf("the no-live line was written %d times across a restart its case held through, want once", got)
	}
	if open, err := d.st.FleetAlertLines(context.Background(), "LEGION"); err != nil ||
		!slices.Equal(open, []store.FleetAlertLine{{Project: "LEGION", Case: fleetalert.NoLiveSession.Word}}) {
		t.Fatalf("the open lines after the next daemon's sweeps = %+v, %v; want the no-live line alone, the lock-out the next daemon's stream holds none of ended", open, err)
	}

	d.noLiveSessions(false)
	d.sweepEach(51, 51, noLiveSession, next)
	next.wait()
	d.noLiveSessions(true)
	d.sweepEach(52, 75, noLiveSession, d.booted(logged))
	if got := lines(fleetalert.NoLiveSession.Word); got != 2 {
		t.Fatalf("a run of lists with no live session after one that held a live session wrote %d lines in all, want 2", got)
	}
}

// lockOutOf is the lock-out of claim the worker stream reads as open: out, while it is of claim.
func lockOutOf(out stream.LockOut) func(claim.Token) (stream.LockOut, bool) {
	return func(token claim.Token) (stream.LockOut, bool) { return out, token == out.Claim }
}

// lockOutsOf is one daemon's lock-out notices over the database, told through w, whose worker
// stream reads out as open, and the supervision that reports out to them.
func (d *durableReach) lockOutsOf(w *workflowRuntime, out stream.LockOut, claims ...supervise.Claim) (*lockOutNotices, *supervision) {
	d.t.Helper()
	l := newLockOutNotices(lockOutOptions{log: quietLogger(), project: "LEGION", tell: w.tellControllerQueued, records: d.st,
		lockedOut: lockOutOf(out), alerts: fleetalert.New(quietLogger(), "legion")})
	s := d.daemon(d.projectNotifying(w), nil, claims...)
	s.lockOuts = l
	l.claimOf = supervisedClaimOf(s.supervisor)
	return l, s
}

// resendEach runs the minute sweep's resend of l once a minute from minute from to minute to, both
// included, and counts the notices of notice's kind it left in the outbox.
func (d *durableReach) resendEach(from, to int, notice record.OutboxPayload, l *lockOutNotices) int {
	d.t.Helper()
	told := 0
	for minute := from; minute <= to; minute++ {
		d.at(time.Duration(minute) * time.Minute)
		l.resend(context.Background())
		told += d.delivered(notice)
	}
	return told
}

// A lock-out's notice is told once across a restart: the next daemon's worker stream reports the
// same lock-out when the older process binds first again, and its told mark is a record of the
// project's, written with it.
func TestALockOutNoticeIsToldOnceAcrossARestart(t *testing.T) {
	d := newDurableReach(t)
	w := d.workflow()
	worker := reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer")
	d.put(worker)
	out := stream.LockOut{Claim: worker.Token, Newer: 4, Older: 3, Since: reachBoot}
	reported := func() int {
		_, s := d.lockOutsOf(w, out, d.claim(worker.Token))
		s.lockOuts.report(out)
		return d.delivered(lockedOut)
	}
	if told := reported(); told != 1 {
		t.Fatalf("the first daemon told %d notices of the lock-out, want 1", told)
	}
	if told := reported(); told != 0 {
		t.Fatalf("the next daemon told %d notices of the same lock-out, want none", told)
	}
}

// A lock-out notice the controller's lane did not queue is kept as a record of the project's, so
// the next daemon, whose worker stream still refuses the newer process, tells it at its first
// sweep, once, and its own stream's report of the lock-out tells nothing more.
func TestAKeptLockOutNoticeIsToldOnceByTheNextDaemon(t *testing.T) {
	d := newDurableReach(t)
	w := d.workflow()
	worker := reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer")
	d.put(worker)
	out := stream.LockOut{Claim: worker.Token, Newer: 4, Older: 3, Since: reachBoot}

	first, s := d.lockOutsOf(w, out, d.claim(worker.Token))
	first.tell = func(context.Context, record.ControllerNotice, string, []reachMark) (bool, error) { return false, nil }
	s.lockOuts.report(out)
	if told := d.delivered(lockedOut); told != 0 {
		t.Fatalf("the first daemon told %d notices its controller's lane refused, want none", told)
	}

	next, s := d.lockOutsOf(w, out, d.claim(worker.Token))
	if told := d.resendEach(1, 1, lockedOut, next); told != 1 {
		t.Fatalf("the next daemon's first sweep told %d notices of the lock-out the first kept, want 1", told)
	}
	s.lockOuts.report(out)
	told := d.delivered(lockedOut) + d.resendEach(2, 3, lockedOut, next)
	third, s := d.lockOutsOf(w, out, d.claim(worker.Token))
	s.lockOuts.report(out)
	if told += d.delivered(lockedOut) + d.resendEach(4, 5, lockedOut, third); told != 0 {
		t.Fatalf("the lock-out was told %d more times after the next daemon told it, want none", told)
	}
}
