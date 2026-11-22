package daemon

// The daemon's own controller under `controller: daemon` (R5, R6): the keeper relaunches it once in
// 6 hours under the relaunch guard, and writes R6's line for each case nobody else can act on,
// beside judgeController's rules. The rig is reach_test.go's.

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fleetalert"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Where the daemon runs the controller, it relaunches its own unreachable controller once in 6
// hours, may replace its pod, and writes R6's line when the relaunch does not bring it back, when
// the guard refuses it, and when it is out of reach a second time within the 6 hours. The
// controller is told nothing of itself.
func TestTheDaemonRelaunchesItsOwnControllerOnceInSixHours(t *testing.T) {
	r := newReachRig(t, true)
	project, _ := claim.ProjectToken("legion")
	controller := supervise.Claim{Token: claim.ControllerToken(project), Project: project, Role: claim.RoleController,
		Generation: 1, State: supervise.StateIdle, Session: "ses_controller", LastReady: reachBoot.Add(-time.Hour),
		Locator: pluginTestLocator(claim.ControllerToken(project)), ActivityAt: reachBoot}
	r.at(21 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
	r.sweep(controller)
	if len(r.relaunched) != 1 {
		t.Fatalf("the daemon relaunched its controller %d times, want once", len(r.relaunched))
	}
	if len(r.told) != 0 {
		t.Fatalf("the controller was told of itself: %+v", r.told)
	}
	controller.Generation++
	for minute := 22; minute <= 40; minute++ {
		r.at(time.Duration(minute) * time.Minute)
		r.sweep(r.with(controller))
	}
	if got := r.lines(fleetalert.ControllerUnreachable.Word); got != 0 {
		t.Fatalf("the line was written %d times inside the 20 minutes after the relaunch", got)
	}
	for minute := 41; minute <= 50; minute++ {
		r.at(time.Duration(minute) * time.Minute)
		r.sweep(r.with(controller))
	}
	if got := r.lines(fleetalert.ControllerUnreachable.Word); got != 1 || len(r.relaunched) != 1 {
		t.Fatalf("20 minutes after the relaunch: %d lines and %d relaunches, want one line and no second relaunch", got, len(r.relaunched))
	}

	// It is back: its session registers, says ready after the relaunch and holds the controller's
	// role. Then it is out of reach a second time within 6 hours: no relaunch, a line.
	r.at(51 * time.Minute)
	r.roles["ses_controller"] = []string{string(controller.Token)}
	r.listing(map[string]time.Duration{"ses_controller": 51 * time.Minute})
	controller.LastReady = reachBoot.Add(51 * time.Minute)
	r.sweep(r.with(controller))
	if r.reach(controller.Token).Unreachable() {
		t.Fatalf("a controller ready after its relaunch and holding its role reads %+v, want its episode over", r.reach(controller.Token))
	}
	r.at(75 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 75 * time.Minute, "ses_controller": 51 * time.Minute})
	r.sweep(r.with(controller))
	if got := r.lines(fleetalert.ControllerUnreachable.Word); got != 2 || len(r.relaunched) != 1 {
		t.Fatalf("out of reach again within 6 hours: %d lines and %d relaunches, want a second line and no relaunch", got, len(r.relaunched))
	}
	if !r.reach(controller.Token).Unreachable() {
		t.Fatal("the controller is not marked out of reach")
	}
}

// A controller the daemon relaunched is back only once it is ready after that relaunch and holds
// the controller's Envoy role (R6). A session that registers with Envoy and goes no further, or
// says ready and never takes the role, keeps the episode, and R6's line is written once, 20
// minutes after the relaunch.
func TestARelaunchedControllerThatRegistersButIsNotBackWritesTheLine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state supervise.ClaimState
		ready time.Duration
		roles []string
		back  bool
	}{
		{name: "registered and never ready", state: supervise.StateRegistered, ready: -time.Hour, roles: []string{"legion-legion-controller"}},
		{name: "ready without its role", state: supervise.StateIdle, ready: 23 * time.Minute, roles: []string{"some-other-role"}},
		// Its role held and ready, but by a ready from before the relaunch, as a read taken before
		// the relaunch's launch rewrote the claim shows it: no ready of the relaunched process.
		{name: "its role held with a ready from before the relaunch", state: supervise.StateIdle, ready: 20 * time.Minute, roles: []string{"legion-legion-controller"}},
		{name: "ready with its role", state: supervise.StateIdle, ready: 23 * time.Minute, roles: []string{"legion-legion-controller"}, back: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReachRig(t, true)
			project, _ := claim.ProjectToken("legion")
			controller := supervise.Claim{Token: claim.ControllerToken(project), Project: project, Role: claim.RoleController,
				Generation: 1, State: supervise.StateIdle, Session: "ses_controller", LastReady: reachBoot.Add(-time.Hour),
				Locator: pluginTestLocator(claim.ControllerToken(project)), ActivityAt: reachBoot}
			r.at(21 * time.Minute)
			r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
			r.sweep(controller)
			if len(r.relaunched) != 1 {
				t.Fatalf("the daemon relaunched its controller %d times, want once", len(r.relaunched))
			}

			// From the next minute its session registers with Envoy every minute.
			controller.Generation++
			controller.State, controller.LastReady = tc.state, reachBoot.Add(tc.ready)
			r.roles["ses_controller"] = tc.roles
			for minute := 22; minute <= 50; minute++ {
				r.at(time.Duration(minute) * time.Minute)
				r.listing(map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute, "ses_controller": time.Duration(minute) * time.Minute})
				r.sweep(r.with(controller))
			}
			want := 1
			if tc.back {
				want = 0
			}
			if got := r.lines(fleetalert.ControllerUnreachable.Word); got != want || len(r.relaunched) != 1 {
				t.Fatalf("a relaunched controller %s: %d lines and %d relaunches, want %d lines and no second relaunch", tc.name, got, len(r.relaunched), want)
			}
			if got := r.reach(controller.Token); got.Unreachable() == tc.back {
				t.Fatalf("a relaunched controller %s reads %+v, want its episode over %t", tc.name, got, tc.back)
			}
		})
	}
}

// A relaunch whose resume finds no session starts its controller on a fresh one, and that launch
// is the relaunch's own: the episode lasts through the session change, so R6's line is written 20
// minutes after the relaunch for a controller that never says ready there, that says ready without
// its Envoy role, or whose launch has recorded no session yet, and not for one that is back.
func TestARelaunchedControllerOnAFreshSessionWritesTheLine(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session string
		state   supervise.ClaimState
		ready   time.Duration
		roles   []string
		back    bool
	}{
		{name: "a fresh session that never says ready", session: "ses_fresh", state: supervise.StateRegistered, ready: -time.Hour,
			roles: []string{"legion-legion-controller"}},
		{name: "a fresh session ready without its role", session: "ses_fresh", state: supervise.StateIdle, ready: 23 * time.Minute,
			roles: []string{"some-other-role"}},
		{name: "no session recorded while its launch runs", session: "", state: supervise.StateLaunching, ready: -time.Hour},
		{name: "a fresh session ready with its role", session: "ses_fresh", state: supervise.StateIdle, ready: 23 * time.Minute,
			roles: []string{"legion-legion-controller"}, back: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReachRig(t, true)
			project, _ := claim.ProjectToken("legion")
			controller := supervise.Claim{Token: claim.ControllerToken(project), Project: project, Role: claim.RoleController,
				Generation: 1, State: supervise.StateIdle, Session: "ses_controller", LastReady: reachBoot.Add(-time.Hour),
				Locator: pluginTestLocator(claim.ControllerToken(project)), ActivityAt: reachBoot}
			r.at(21 * time.Minute)
			r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
			r.sweep(controller)
			if len(r.relaunched) != 1 {
				t.Fatalf("the daemon relaunched its controller %d times, want once", len(r.relaunched))
			}

			// The relaunch's own launch, at generation 2, runs on a fresh session.
			controller.Generation = 2
			controller.LastRelaunch, controller.LastRelaunchGeneration = reachBoot.Add(21*time.Minute), 2
			controller.Session, controller.State, controller.LastReady = tc.session, tc.state, reachBoot.Add(tc.ready)
			if tc.session != "" {
				r.roles[tc.session] = tc.roles
			}
			for minute := 22; minute <= 50; minute++ {
				seen := map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute}
				if tc.session != "" {
					seen[tc.session] = time.Duration(minute) * time.Minute
				}
				r.at(time.Duration(minute) * time.Minute)
				r.listing(seen)
				r.sweep(r.with(controller))
			}
			want := 1
			if tc.back {
				want = 0
			}
			if got := r.lines(fleetalert.ControllerUnreachable.Word); got != want || len(r.relaunched) != 1 {
				t.Fatalf("a relaunched controller on %s: %d lines and %d relaunches, want %d lines and no second relaunch", tc.name, got, len(r.relaunched), want)
			}
		})
	}
}

// The daemon's relaunch of its own controller, whose path to Envoy stays blocked, does not bring
// it back: each boot fails its Envoy claim and exits, the claim fails once its launch failures run
// out and is launched again later, all on the same session. R6's line still says, once, 20
// minutes after the relaunch, that it did not bring it back (w5's launcher fresh run wrote no line
// in the 25 minutes after the relaunch at 11:26:37Z, its controller failed from 11:26:48Z).
func TestARelaunchedControllerWhoseBootsFailStillWritesTheLineOnce(t *testing.T) {
	r := newReachRig(t, true)
	project, _ := claim.ProjectToken("legion")
	controller := supervise.Claim{Token: claim.ControllerToken(project), Project: project, Role: claim.RoleController,
		Generation: 1, State: supervise.StateIdle, Session: "ses_controller", LastReady: reachBoot.Add(-time.Hour),
		Locator: pluginTestLocator(claim.ControllerToken(project)), ActivityAt: reachBoot}
	r.at(21 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
	r.sweep(controller)
	if len(r.relaunched) != 1 {
		t.Fatalf("the daemon relaunched its controller %d times, want once", len(r.relaunched))
	}
	controller.Generation++
	states := []supervise.ClaimState{supervise.StateLaunching, supervise.StateRegistered, supervise.StateFailed, supervise.StateFailed}
	for minute := 22; minute <= 50; minute++ {
		r.at(time.Duration(minute) * time.Minute)
		r.listing(map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute})
		c := r.with(controller)
		c.State = states[minute%len(states)]
		r.sweep(c)
		if minute == 40 {
			if got := r.lines(fleetalert.ControllerUnreachable.Word); got != 0 {
				t.Fatalf("the line was written %d times inside the 20 minutes after the relaunch", got)
			}
		}
	}
	if got := r.lines(fleetalert.ControllerUnreachable.Word); got != 1 || len(r.relaunched) != 1 {
		t.Fatalf("20 minutes after a relaunch whose boots fail: %d lines and %d relaunches, want one line and no second relaunch", got, len(r.relaunched))
	}
}

func TestARefusedRelaunchOfTheControllerWritesTheLineAndUsesNoAllowance(t *testing.T) {
	r := newReachRig(t, true)
	project, _ := claim.ProjectToken("legion")
	controller := supervise.Claim{Token: claim.ControllerToken(project), Project: project, Role: claim.RoleController,
		Generation: 1, State: supervise.StateWorking, Session: "ses_controller", LastReady: reachBoot.Add(-time.Hour),
		Locator: pluginTestLocator(claim.ControllerToken(project)), ActivityAt: reachBoot, ActivityBackground: true,
		ActivityKinds: []string{"async-job"}}
	for minute := 21; minute <= 30; minute++ {
		r.at(time.Duration(minute) * time.Minute)
		r.listing(map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute})
		r.sweep(r.with(controller))
	}
	if len(r.relaunched) != 0 || r.lines(fleetalert.ControllerRelaunchRefused.Word) != 1 {
		t.Fatalf("a controller whose job runs: %d relaunches and %d lines, want none and one", len(r.relaunched),
			r.lines(fleetalert.ControllerRelaunchRefused.Word))
	}
	// The job ends; 20 minutes after its last step the guard lets it through and the daemon
	// relaunches it, its 6 hours unspent by the refusals.
	controller.ActivityAt, controller.ActivityBackground, controller.ActivityKinds = reachBoot.Add(30*time.Minute), false, nil
	r.at(31 * time.Minute)
	r.sweep(r.with(controller))
	if len(r.relaunched) != 1 || r.reach(controller.Token).RelaunchedAt.IsZero() {
		t.Fatalf("relaunches = %d, reach %+v, want the controller relaunched once the guard lets it through", len(r.relaunched), r.reach(controller.Token))
	}

	// A relaunch whose process outlived its stop is refused too, and writes the same line.
	r2 := newReachRig(t, true)
	r2.relaunch = runtime.ErrProcessOutlivedStop
	controller.Reach = supervise.Reach{}
	controller.ActivityAt = reachBoot
	r2.at(21 * time.Minute)
	r2.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
	r2.sweep(controller)
	if r2.lines(fleetalert.ControllerRelaunchRefused.Word) != 1 || !r2.reach(controller.Token).RelaunchedAt.IsZero() {
		t.Fatalf("a stop the process outlived: %d lines, reach %+v, want the line and no allowance used",
			r2.lines(fleetalert.ControllerRelaunchRefused.Word), r2.reach(controller.Token))
	}
}

// inTurnWaiting is c in a turn since reachBoot, its one step the open `wait` that turn began with,
// and its session's report of that turn made at reportedAt, naming nothing running: a `wait` on a
// named `bash` service, which the pinned Oh My Pi blocks in with no step and no report names.
func inTurnWaiting(c supervise.Claim, reportedAt time.Time) supervise.Claim {
	c.State, c.LastTurnStart, c.LastTurnEnd = supervise.StateWorking, reachBoot, reachBoot.Add(-2*time.Hour)
	c.Step = supervise.StepRecord{At: reachBoot, TurnStartedAt: reachBoot, Tool: "wait", CallID: "toolu_wait_0", StartedAt: reachBoot}
	c.ActivityAt, c.ActivityBackground, c.ActivityKinds = reportedAt, false, nil
	return c
}

// The notice that the guard would now let a refused claim through reads the guard's own rule, its
// turn heartbeat included (R5): while the claim's session sends its report every 5 minutes in its
// turn, no notice goes, and the notice goes 20 minutes after the last report, once the process has
// frozen.
func TestTheRelaunchReadyNoticeWaitsUntilTheTurnsReportsStopFor20Minutes(t *testing.T) {
	r := newReachRig(t, false)
	worker := inTurnWaiting(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer"), reachBoot)
	r.at(21 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
	worker.ActivityAt = reachBoot.Add(20 * time.Minute)
	r.sweep(worker)
	if err := r.k.guardRefused(context.Background(), worker.Token, reachBoot.Add(21*time.Minute)); err != nil {
		t.Fatalf("record the guard's refusal: %v", err)
	}
	for minute := 22; minute <= 60; minute++ {
		if minute%5 == 0 {
			worker.ActivityAt = reachBoot.Add(time.Duration(minute) * time.Minute)
		}
		r.at(time.Duration(minute) * time.Minute)
		r.sweep(r.with(worker))
	}
	if ready := r.notices(record.RelaunchReadyNotice); len(ready) != 0 {
		t.Fatalf("relaunch-ready notices = %+v while the session still reports in its turn, want none", ready)
	}
	// The process froze after its report at 60 minutes: nothing arrives from then on.
	for minute := 61; minute <= 79; minute++ {
		r.at(time.Duration(minute) * time.Minute)
		r.sweep(r.with(worker))
	}
	if ready := r.notices(record.RelaunchReadyNotice); len(ready) != 0 {
		t.Fatalf("relaunch-ready notices = %+v 19 minutes after the turn's last report, want none", ready)
	}
	r.at(80 * time.Minute)
	r.sweep(r.with(worker))
	if ready := r.notices(record.RelaunchReadyNotice); len(ready) != 1 || ready[0].notice.Claim != string(worker.Token) {
		t.Fatalf("relaunch-ready notices = %+v, want one 20 minutes after the turn's last report", ready)
	}
}

// The daemon's own relaunch of its controller reads the same rule: a controller whose session
// reports in its turn is not relaunched, and is 20 minutes after its last report.
func TestTheDaemonDoesNotRelaunchAControllerThatReportsInItsTurn(t *testing.T) {
	r := newReachRig(t, true)
	project, _ := claim.ProjectToken("legion")
	controller := inTurnWaiting(supervise.Claim{Token: claim.ControllerToken(project), Project: project,
		Role: claim.RoleController, Generation: 1, Session: "ses_controller", LastReady: reachBoot.Add(-time.Hour),
		Locator: pluginTestLocator(claim.ControllerToken(project))}, reachBoot.Add(20*time.Minute))
	for minute := 21; minute <= 40; minute++ {
		if minute%5 == 0 {
			controller.ActivityAt = reachBoot.Add(time.Duration(minute) * time.Minute)
		}
		r.at(time.Duration(minute) * time.Minute)
		r.listing(map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute})
		r.sweep(r.with(controller))
	}
	if len(r.relaunched) != 0 || r.lines(fleetalert.ControllerRelaunchRefused.Word) != 1 {
		t.Fatalf("a controller that reports in its turn: %d relaunches and %d lines, want none and one", len(r.relaunched),
			r.lines(fleetalert.ControllerRelaunchRefused.Word))
	}
	// It froze after its report at 40 minutes.
	for minute := 41; minute <= 60; minute++ {
		r.at(time.Duration(minute) * time.Minute)
		r.listing(map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute})
		r.sweep(r.with(controller))
	}
	if len(r.relaunched) != 1 {
		t.Fatalf("relaunches = %d, want the controller relaunched 20 minutes after its turn's last report", len(r.relaunched))
	}
}
