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

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fleetalert"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
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
	// unreadable is whether Envoy's list answers no read at all.
	unreadable bool
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

// listUnreadable sets whether Envoy's list answers no read.
func (d *durableReach) listUnreadable(failing bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.unreadable = failing
}

// options is a keeper's options over the database: Envoy's list holds one live session, none of
// the claims' (none at all while noLiveSessions says so, and no answer while listUnreadable says
// so), and the reach record is written by put.
func (d *durableReach) options(daemonController bool, put func(context.Context, claim.Token, supervise.Reach) (bool, error)) reachKeeperOptions {
	return reachKeeperOptions{
		log: quietLogger(), project: "LEGION", boot: reachBoot, daemonController: daemonController, now: d.clock,
		list: func(context.Context) ([]notify.ListedSession, error) {
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.unreadable {
				return nil, errors.New("the listener did not answer")
			}
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

// relaunching is a keeper over the database built from o whose relaunch is the daemon's own
// (relaunchByDaemon), through the supervision of claims it is booted with.
func (d *durableReach) relaunching(o reachKeeperOptions, claims ...supervise.Claim) *reachKeeper {
	d.t.Helper()
	var s *supervision
	o.relaunch = func(ctx context.Context, c supervise.Claim, at time.Time) (supervise.Claim, error) {
		return s.relaunchByDaemon(ctx, c, at)
	}
	k := newReachKeeper(o)
	s = d.daemon(k, nil, claims...)
	return k
}

// relaunchedOnce has the first daemon's sweep mark worker out of reach and relaunch it, and
// answers the claim as the database holds it once that daemon has done all it does: put is its
// write of the reach record, and reached, where it is not nil, is closed when that daemon reaches
// the write it ends inside, since its relaunch then never returns; otherwise the relaunch returns.
// Either way the relaunch's launch, which writes the claim more than once, is over: a claim read
// at its first write is written again under the next daemon.
func (d *durableReach) relaunchedOnce(worker supervise.Claim, put func(context.Context, claim.Token, supervise.Reach) (bool, error), reached <-chan struct{}) supervise.Claim {
	d.t.Helper()
	d.put(worker)
	generation := d.claim(worker.Token).Generation
	d.report(worker.Token, generation)
	k := d.relaunching(d.options(false, put), worker)
	d.at(21 * time.Minute)
	k.sweep(context.Background(), d.stored())
	if reached == nil {
		k.wait()
	} else {
		select {
		case <-reached:
		case <-time.After(time.Minute):
			d.t.Fatal("the first daemon did not reach its write of the relaunch's reach record within a minute")
		}
	}
	if d.claim(worker.Token).Generation <= generation {
		d.t.Fatalf("the first daemon's relaunch left the worker at generation %d, want its launch past %d", d.claim(worker.Token).Generation, generation)
	}
	if !d.claim(worker.Token).Reach.Unreachable() {
		d.t.Fatal("the first daemon did not mark the worker out of reach")
	}
	return d.claim(worker.Token)
}

// refusedAfterRestart boots a second daemon onto the database 3 hours after the first booted and
// checks that its sweeps, through the 20 minutes counted from its own boot and the minute that
// marks worker, do not relaunch worker a second time. worker's relaunched process is working, its
// session registered after the relaunch, which ended that episode, and it is out of reach again,
// with no relaunch in its reach record: the 6 hours are read from the claim's own row
// (supervise.Claim.LastRelaunch), which the relaunch's launch wrote, and R6's line says it is out
// of reach a second time within them.
func (d *durableReach) refusedAfterRestart(worker supervise.Claim) {
	d.t.Helper()
	restarted := d.claim(worker.Token)
	restarted.State = supervise.StateWorking
	d.put(restarted)
	d.report(worker.Token, restarted.Generation)
	if known, err := d.st.PutClaimReach(context.Background(), worker.Token, supervise.Reach{}); err != nil || !known {
		d.t.Fatalf("end the first relaunch's episode = %t, %v", known, err)
	}
	d.at(3 * time.Hour)
	logged := &testlog.Buffer{}
	o := d.options(false, d.st.PutClaimReach)
	o.boot = d.clock()
	o.alerts = fleetalert.New(slog.New(slog.NewTextHandler(logged, nil)), "LEGION")
	k := d.relaunching(o, d.claim(worker.Token))
	d.sweepEach(180, 201, k)
	k.wait()
	if !d.claim(worker.Token).Reach.Unreachable() {
		d.t.Fatalf("the next daemon's sweeps 21 minutes after its boot left the worker reading %+v, want it marked", d.claim(worker.Token).Reach)
	}
	if got := d.claim(worker.Token).Generation; got != restarted.Generation {
		d.t.Fatalf("the next daemon's sweep 3 hours after the first relaunch left the worker at generation %d, want %d: no second relaunch within 6 hours",
			got, restarted.Generation)
	}
	if line := logged.String(); !strings.Contains(line, "case="+fleetalert.ClaimUnreachable.Word) || !strings.Contains(line, "a second time within") {
		d.t.Fatalf("the next daemon wrote %q, want R6's line that the worker is out of reach a second time within 6 hours", line)
	}
}

// A relaunch whose reach write the store refuses is still refused a second time within 6 hours by
// the daemon that boots next: the record is written with the launch itself.
func TestARelaunchIsRefusedForSixHoursAcrossARefusedReachWriteAndARestart(t *testing.T) {
	d := newDurableReach(t)
	worker := waitingQuietly(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer"))
	lift := d.refuseReachWrites("relaunchedAt")
	relaunched := d.relaunchedOnce(worker, d.st.PutClaimReach, nil)
	lift()
	if !relaunched.Reach.RelaunchedAt.IsZero() || relaunched.Reach.Relaunched {
		t.Fatalf("the keeper's record of the relaunch reached the store: %+v; want the store to have refused it", relaunched.Reach)
	}
	d.refusedAfterRestart(worker)
}

// A daemon that ends between the relaunch's launch and its reach write leaves the next daemon the
// 6-hour refusal all the same.
func TestARelaunchIsRefusedForSixHoursAcrossADaemonThatEndsBeforeItsReachWrite(t *testing.T) {
	d := newDurableReach(t)
	worker := waitingQuietly(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer"))
	ended, reached := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(ended) })
	put := func(ctx context.Context, token claim.Token, reach supervise.Reach) (bool, error) {
		if !reach.RelaunchedAt.IsZero() {
			// The daemon ends here: this write is never made, and nothing after it runs. Its
			// relaunch has returned, so its launch has written the claim for the last time.
			close(reached)
			<-ended
			return false, errors.New("the daemon ended before it made this write")
		}
		return d.st.PutClaimReach(ctx, token, reach)
	}
	relaunched := d.relaunchedOnce(worker, put, reached)
	if !relaunched.Reach.RelaunchedAt.IsZero() || relaunched.Reach.Relaunched {
		t.Fatalf("the keeper's record of the relaunch reached the store: %+v; want the daemon to have ended first", relaunched.Reach)
	}
	d.refusedAfterRestart(worker)
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
		return d.relaunching(d.options(true, d.st.PutClaimReach), d.claim(token))
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

// sweepEach runs one sweep a minute of k from minute from to minute to, both included.
func (d *durableReach) sweepEach(from, to int, k *reachKeeper) {
	d.t.Helper()
	for minute := from; minute <= to; minute++ {
		d.at(time.Duration(minute) * time.Minute)
		k.sweep(context.Background(), d.stored())
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
// operator's controller, the list's and the controller's where no list is read, whatever record
// the controller's claim carries), and a case it judges and finds ended (a claim back in reach, or
// gone from the claims). So a switch of the controller's mode and back, or a rollback to a release
// that closed its cases in memory alone and a roll forward, leaves no line open that would silence
// the next line of its case (R6). A case that still holds keeps its line.
func TestAStoredLineWhoseCaseNoLongerHoldsIsClosedByTheFirstSweep(t *testing.T) {
	controller := storedController().Token
	worker := waitingQuietly(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer"))
	workerLines := []fleetalert.Line{{Word: fleetalert.ClaimUnreachable.Word, Claim: string(worker.Token)},
		{Word: fleetalert.ClaimRelaunchRefused.Word, Claim: string(worker.Token)}}
	for _, tc := range []struct {
		name             string
		stored           []fleetalert.Line
		daemonController bool
		readsList        bool
		claims           []supervise.Claim
		markedReach      bool
		markedWorker     bool
		open             bool
	}{
		{name: "the controller's line under an operator's controller", daemonController: false, readsList: true,
			stored: []fleetalert.Line{{Word: fleetalert.ControllerUnreachable.Word, Claim: string(controller)},
				{Word: fleetalert.ControllerRelaunchRefused.Word, Claim: string(controller)}}},
		{name: "the list's lines where no list is read", daemonController: true, readsList: false,
			stored: []fleetalert.Line{{Word: fleetalert.NoLiveSession.Word}, {Word: fleetalert.SessionListUnreadable.Word}}},
		{name: "the controller's line where no list is read, its stored record marked", daemonController: true, readsList: false,
			stored:      []fleetalert.Line{{Word: fleetalert.ControllerUnreachable.Word, Claim: string(controller)}},
			markedReach: true},
		{name: "the controller's line of a controller back in reach", daemonController: true, readsList: true,
			stored: []fleetalert.Line{{Word: fleetalert.ControllerUnreachable.Word, Claim: string(controller)}},
			claims: []supervise.Claim{storedController()}},
		{name: "the controller's line of a controller gone from the claims", daemonController: true, readsList: true,
			stored: []fleetalert.Line{{Word: fleetalert.ControllerUnreachable.Word, Claim: string(controller)}}},
		{name: "the controller's line of a controller still out of reach", daemonController: true, readsList: true,
			stored:      []fleetalert.Line{{Word: fleetalert.ControllerUnreachable.Word, Claim: string(controller)}},
			markedReach: true, open: true},
		{name: "a workflow claim's lines of a claim back in reach", readsList: true, stored: workerLines,
			claims: []supervise.Claim{worker}},
		{name: "a workflow claim's lines of a claim gone from the claims", readsList: true, stored: workerLines},
		{name: "a workflow claim's lines of a claim still out of reach", readsList: true, stored: workerLines,
			markedWorker: true, open: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDurableReach(t)
			if tc.markedReach {
				d.markController()
			}
			if tc.markedWorker {
				d.markRelaunched(worker)
			}
			for _, c := range tc.claims {
				d.put(c)
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

// storedController is the project controller's claim as a daemon before this one left it, with no
// relaunch: idle on its session at generation 2, last ready an hour before the boot.
func storedController() supervise.Claim {
	project, _ := claim.ProjectToken("legion")
	controller := claim.ControllerToken(project)
	return supervise.Claim{Token: controller, Project: project, Role: claim.RoleController, Generation: 2,
		State: supervise.StateIdle, Session: "ses_controller", LastReady: reachBoot.Add(-time.Hour),
		Locator: pluginTestLocator(controller), ActivityAt: reachBoot}
}

// markController is markRelaunched of storedController.
func (d *durableReach) markController() supervise.Claim {
	d.t.Helper()
	return d.markRelaunched(storedController())
}

// markRelaunched stores c relaunched by the daemon before this one 25 minutes before the boot,
// past notBackAfter, at the generation it runs, and its reach record marked out of reach since 30
// minutes before the boot with that relaunch taken in, as that daemon left them, and returns the
// claim. So a sweep that reads its case as holding opens its line, which an open line keeps it
// from writing again.
func (d *durableReach) markRelaunched(c supervise.Claim) supervise.Claim {
	d.t.Helper()
	c.LastRelaunch, c.LastRelaunchGeneration = reachBoot.Add(-25*time.Minute), c.Generation
	d.put(c)
	marked := supervise.Reach{Session: c.Session, FirstReady: c.LastReady, Since: reachBoot.Add(-30 * time.Minute),
		Relaunched: true, RelaunchedAt: c.LastRelaunch}
	if known, err := d.st.PutClaimReach(context.Background(), c.Token, marked); err != nil || !known {
		d.t.Fatalf("mark %s out of reach = %t, %v", c.Token, known, err)
	}
	return c
}

// A read of Envoy's list that fails right after a boot ends no case: the controller's line the
// store holds open, for a controller whose stored reach record is marked out of reach, is kept
// through it, so the next read that answers, which finds the case still holding, writes no second
// line (R6). Before any read has answered the keeper's memory holds no record of the controller,
// so the case is read from the record the claim carries from the store (recordOf).
func TestAFailedListReadAfterABootKeepsTheControllersStoredLine(t *testing.T) {
	ctx := context.Background()
	d := newDurableReach(t)
	controller := d.markController().Token
	line := store.FleetAlertLine{Project: "LEGION", Case: fleetalert.ControllerUnreachable.Word, Claim: string(controller)}
	if err := d.st.PutFleetAlertLine(ctx, line); err != nil {
		t.Fatalf("store the open line %+v: %v", line, err)
	}
	logged := &testlog.Buffer{}
	k := d.bootedAs(logged, true, true)

	d.listUnreadable(true)
	d.at(time.Minute)
	k.sweep(ctx, d.stored())
	k.wait()
	// An Errorf, not a Fatalf: the line count below is the consequence this pins, and it is
	// reached whether or not the failed read kept the line.
	if open, err := d.st.FleetAlertLines(ctx, "LEGION"); err != nil || !slices.Equal(open, []store.FleetAlertLine{line}) {
		t.Errorf("after a failed read the store holds the open lines %+v, %v; want the controller's line kept", open, err)
	}
	d.listUnreadable(false)
	d.at(2 * time.Minute)
	k.sweep(ctx, d.stored())
	k.wait()

	if written := strings.Count(logged.String(), "case="+fleetalert.ControllerUnreachable.Word); written != 0 {
		t.Errorf("the booted daemon wrote the controller's line %d times over a failed read and one that answered, want none: the daemon before it wrote it, and its case held throughout", written)
	}
	if open, err := d.st.FleetAlertLines(ctx, "LEGION"); err != nil || !slices.Equal(open, []store.FleetAlertLine{line}) {
		t.Fatalf("after the read that answered the store holds the open lines %+v, %v; want the controller's line alone", open, err)
	}
}

// R6's line is written once until its case ends, and a restart ends no case (R6): a run of lists
// with no live session that holds through a restart writes its line once, a lock-out line the last
// daemon wrote is ended by the next one's worker stream as it starts (stream.Listen), and a case
// that ended and came back writes its line again.
func TestALineIsWrittenOnceUntilItsCaseEndsAcrossARestart(t *testing.T) {
	d := newDurableReach(t)
	logged := &testlog.Buffer{}
	lines := func(word string) int { return strings.Count(logged.String(), "case="+word) }
	d.noLiveSessions(true)
	first := d.booted(logged)
	first.alerts.Open(fleetalert.NewerProcessRefused, "legion-legion-legion-209-implementer")
	d.sweepEach(1, 25, first)
	first.wait()
	if got := lines(fleetalert.NoLiveSession.Word); got != 1 {
		t.Fatalf("the first daemon wrote the no-live line %d times in 25 minutes, want once", got)
	}

	next := d.booted(logged)
	listening, stopListening := context.WithCancel(context.Background())
	t.Cleanup(stopListening)
	refuse := func(string) (claim.Token, uint64, bool, bool, error) { return "", 0, false, false, nil }
	if _, err := stream.Listen(listening, "tcp://127.0.0.1:0", refuse, stream.Options{RPCTimeout: time.Second, Log: quietLogger(), Alerts: next.alerts}); err != nil {
		t.Fatalf("the next daemon's worker stream: %v", err)
	}
	d.sweepEach(26, 50, next)
	next.wait()
	if got := lines(fleetalert.NoLiveSession.Word); got != 1 {
		t.Fatalf("the no-live line was written %d times across a restart its case held through, want once", got)
	}
	if open, err := d.st.FleetAlertLines(context.Background(), "LEGION"); err != nil ||
		!slices.Equal(open, []store.FleetAlertLine{{Project: "LEGION", Case: fleetalert.NoLiveSession.Word}}) {
		t.Fatalf("the open lines after the next daemon's sweeps = %+v, %v; want the no-live line alone, the lock-out the next daemon's stream holds none of ended", open, err)
	}

	d.noLiveSessions(false)
	d.sweepEach(51, 51, next)
	next.wait()
	d.noLiveSessions(true)
	d.sweepEach(52, 75, d.booted(logged))
	if got := lines(fleetalert.NoLiveSession.Word); got != 2 {
		t.Fatalf("a run of lists with no live session after one that held a live session wrote %d lines in all, want 2", got)
	}
}
