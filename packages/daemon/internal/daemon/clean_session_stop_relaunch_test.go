package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// listenerBehind is how far the relaunch tests' listener clock runs behind the daemon's: far enough
// that every registration it stamps during a row reads as written before the stop's moment, so
// the moment keeps none and the client instance id is the relaunch's only guard.
const listenerBehind = time.Minute

// The lines a clean stop ends on, one per stop (sessionStop.send).
const (
	stopStoppedLine     = "the Envoy session of a role whose process is gone was stopped"
	stopKeptLine        = "the Envoy listener kept the registration of a dead role's session"
	stopUnreadLine      = "the Envoy listener did not answer what a dead role's session registration is, so no clean stop was sent"
	stopUnconfirmedLine = "the Envoy listener did not confirm the clean stop of a dead role's session"
	stopNoEntryLine     = "the Envoy listener answered no registration of a dead role's session, so none was stopped"
)

// relaunchStopBudget is the clean stop's budget in these rows: the product's own budget for a
// stop's read and delete (CleanSessionStopBudget, 5 s in production), which the test sets, as the
// thing it tests, through sessionStopOverrides.budget. A row holds the stop's read at its listener
// until the relaunch has begun, so the read lasts as long as the machine takes to send the stop
// and begin the relaunch, which production's 5 s need not cover on a loaded machine. Two minutes
// is several times the longest whole row, boot included, that a passing run has measured (under
// 25 s on a development box loaded past 4,000), so the budget does not end the held read of a row
// that passes. A stall longer than it ends the held read at the budget, and the row then fails by
// name on the line the stop ended on.
const relaunchStopBudget = 2 * time.Minute

// relaunchStepBound bounds each of the two steps of a relaunch row on either side of the clean
// stop. Neither step has one product bound of its own; each waits on several:
//   - the set-up, before the row ends the claim's process: the daemon's boot, and the spawn,
//     registration and ready requests, each under d.client's 10 s;
//   - the relaunch's start, once the stop has ended: the decision's claim writes, each under the
//     machine's own claim-write bound (supervise's defaultClaimWrite, 5 s), and the answer to the
//     operator's request.
//
// So the minute is measured, not derived: more than twice the longest whole row that a passing
// run has measured (relaunchStopBudget). The three parts make one end (relaunchRowEnd), so a
// set-up longer than its minute leaves the rest of the row less time, and a row whose waits run
// past that end fails there, naming the wait it was in, while the rows after it still run. Once a
// row has ended the stops itself, a relaunch slower than the minute fails it (stopsEnded).
const relaunchStepBound = time.Minute

// relaunchRowEnd is the moment one row of the relaunch tests ends by, from what the row runs: its
// set-up, the clean stop, which relaunchStopBudget bounds, and the relaunch after it, each set-up
// and relaunch a relaunchStepBound. A row that fails at one of its waits therefore fails at its own
// bound, and the rows after it still run. The test binary's deadline is only the outer limit, less
// what must still fit once the row has given up its last wait: the daemon's own stop at the row's
// end, which d.stop waits for (stopBound of stopBudget). A deadline that leaves the row less than the
// clean stop's own budget (CleanSessionStopBudget) refuses the row by name: run on less, its stop
// would get less time than production gives one.
func relaunchRowEnd(t *testing.T) time.Time {
	t.Helper()
	end := time.Now().Add(relaunchStepBound + relaunchStopBudget + relaunchStepBound)
	deadline, ok := t.Deadline()
	if !ok {
		return end
	}
	after := stopBound(stopBudget)
	if left := time.Until(deadline) - after; left < CleanSessionStopBudget {
		t.Fatalf("the test binary's deadline, %s, leaves this row %s once the daemon's stop at its end (%s) is set aside, less than the clean stop's own budget of %s: run it with a longer -timeout",
			deadline.Format(time.RFC3339Nano), left, after, CleanSessionStopBudget)
	}
	if outer := deadline.Add(-after); outer.Before(end) {
		return outer
	}
	return end
}

// relaunchRegistering is the fake runtime whose Resume brings the relaunched agent up: once the
// runtime has started the process, that process registers the claim's session at the listener
// under a client instance of its own (register), and resumed is closed.
type relaunchRegistering struct {
	*fake.Runtime
	register func()
	resumed  chan struct{}
	once     sync.Once
}

func (r *relaunchRegistering) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	loc, err := r.Runtime.Resume(ctx, prev, spec)
	if err == nil {
		r.once.Do(func() {
			r.register()
			close(r.resumed)
		})
	}
	return loc, err
}

// relaunchRow is one way the daemon relaunches a claim in the same session: end ends the claim's
// process, which sends the clean stop of its session, and relaunch brings the claim back, or is
// nil where the daemon relaunches it unasked.
type relaunchRow struct {
	name     string
	end      func(r *relaunchRig)
	relaunch func(r *relaunchRig) <-chan operatorAnswer
}

// relaunchRows are the two relaunches the daemon starts in the same session: an operator's resume
// of a suspended claim, which starts the process through Machine.start as a wake's resume of a
// released claim does (R3 of the suspension's clean stop), and the relaunch a death decides in its
// own decision.
func relaunchRows() []relaunchRow {
	return []relaunchRow{
		{
			name: "the operator's resume of a suspended claim",
			end: func(r *relaunchRig) {
				r.answered("the operator's suspension", r.operator("/legion/v1/operator/claims/"+string(r.token)+"/suspend"))
			},
			relaunch: func(r *relaunchRig) <-chan operatorAnswer {
				return r.operator("/legion/v1/operator/claims/" + string(r.token) + "/resume")
			},
		},
		{
			name: "the relaunch a death decides",
			end: func(r *relaunchRig) {
				r.rt.Emit(runtime.Observation{Locator: *r.d.claim(r.token).Locator, Kind: runtime.Gone, Detail: "pane gone"})
			},
		},
	}
}

// relaunchRig is one row of the relaunch tests: the real daemon, a fake listener whose clock runs
// listenerBehind the daemon's and that holds the stop's read, a runtime whose resume registers
// the relaunch, and a ready claim. end is the moment the row ends by (relaunchRowEnd), and budget
// the budget the daemon's clean stops were given: relaunchStopBudget, or what was left before end
// when the row began where the test binary's deadline leaves less.
//
// The daemon's boot, the operator's requests (operator), the clean stop's read and delete, through
// its budget, and every wait of the row end by end, so none of them has a fixed time of its own
// that a loaded machine can pass before the row's own bound. The shared spawn and ready helpers
// (d.spawn, readyClaim) keep d.client's 10 s on each of their requests: they run before the row
// ends the claim's process, while no clean stop is in flight to hold them.
type relaunchRig struct {
	t      *testing.T
	d      *daemon
	envoy  *listenerRecorder
	rt     *relaunchRegistering
	logs   *syncBuffer
	token  claim.Token
	end    time.Time
	budget time.Duration
}

// startRelaunchRig boots the daemon against envoy, which the rig gives the ended process's
// registration (inst_ended) and the channels a row waits on, and readies a claim. stopsEnd is the
// daemon's sessionStopOverrides.end, nil for none. Whatever the row holds at the listener is let
// go when the row ends, before the daemon's stop, so a row that fails holding the read or the
// delete still ends at once.
func startRelaunchRig(t *testing.T, envoy *listenerRecorder, stopsEnd <-chan struct{}) *relaunchRig {
	t.Helper()
	end := relaunchRowEnd(t)
	r := &relaunchRig{t: t, envoy: envoy, logs: &syncBuffer{}, end: end, budget: min(relaunchStopBudget, time.Until(end))}
	envoy.held = storedRegistration{clientInstanceID: "inst_ended", lastSeen: time.Now().Add(-listenerBehind)}
	envoy.arrived, envoy.settled = make(chan struct{}), make(chan struct{})
	cfg := testConfig(t)
	cfg.EnvoyURL = envoy.server(t).URL
	r.rt = &relaunchRegistering{Runtime: fake.NewRuntime(), resumed: make(chan struct{})}
	r.rt.register = func() {
		envoy.register(storedRegistration{clientInstanceID: "inst_relaunched", lastSeen: time.Now().Add(-listenerBehind)})
	}
	o := fakeRuntime(r.rt, &built{})
	o.sessionStop = sessionStopOverrides{budget: r.budget, end: stopsEnd}
	r.d = startDaemonWithin(t, cfg, o, slog.New(slog.NewJSONHandler(r.logs, nil)), time.Until(end))
	t.Cleanup(envoy.releaseHeld)
	r.token = r.d.spawn(api.SpawnRequest{Tree: "LEGION-9", Issue: "LEGION-9", Role: claim.RoleArchitect, Prompt: "Reply ready and wait."})
	readyClaim(t, r.d, r.rt.Runtime, r.token)
	return r
}

// await waits for done until the row's end.
func (r *relaunchRig) await(what string, done <-chan struct{}) {
	r.t.Helper()
	select {
	case <-done:
	case <-time.After(time.Until(r.end)):
		r.t.Fatalf("the row's end came before %s; log:\n%s", what, r.logs)
	}
}

// operator sends the operator's request at path (operatorRequest). An operator's resume answers
// only once its decision has waited for the clean stop in flight
// (supervise.Machine.awaitSessionStops), so the request has no client timeout of its own (d.client's
// is 10 s): its context ends at the row's end.
func (r *relaunchRig) operator(path string) <-chan operatorAnswer {
	ctx, cancel := context.WithDeadline(context.Background(), r.end)
	r.t.Cleanup(cancel)
	return operatorRequest(ctx, &http.Client{Transport: r.d.transport}, r.d, http.MethodPost, path, nil)
}

// answered waits for the answer of the operator's request, what, which must be 200; a row with no
// request passes nil.
func (r *relaunchRig) answered(what string, answered <-chan operatorAnswer) {
	r.t.Helper()
	if answered == nil {
		return
	}
	select {
	case got := <-answered:
		if got.status != http.StatusOK {
			r.t.Fatalf("%s = %d (%v), want 200; log:\n%s", what, got.status, got.err, r.logs)
		}
	case <-time.After(time.Until(r.end)):
		r.t.Fatalf("the row's end came before %s was answered; log:\n%s", what, r.logs)
	}
}

// stopsEnded ends the daemon's clean stops in flight (sessionStopOverrides.end), as the end of their
// budget would. What the row then waits for is the relaunch's start alone, so its end comes a
// relaunchStepBound from now at the latest. A stop that went on past the end it was given holds
// the relaunch until its own budget (r.budget) runs out. Where that comes after the row's new end,
// the row fails there, still waiting for the relaunch. Where it comes first, as for a stop whose
// read was held for most of its budget, the relaunch starts at the budget, and the row fails once
// the stop's line names its context's deadline rather than the end the row gave it
// (TestAStopThatLandsAfterTheRelaunchsRegistrationKeepsIt).
func (r *relaunchRig) stopsEnded(end chan<- struct{}) {
	close(end)
	if bound := time.Now().Add(relaunchStepBound); bound.Before(r.end) {
		r.end = bound
	}
}

// stopSentAndRelaunched ends the claim's process, waits for its clean stop to reach the
// listener, sends the row's relaunch, and waits until the relaunch either comes up, which
// registers it while the stop's read is still held, or says that it waits for the stop in flight.
// It answers the relaunch request's answer, nil for a row with none.
func (r *relaunchRig) stopSentAndRelaunched(row relaunchRow) <-chan operatorAnswer {
	r.t.Helper()
	row.end(r)
	r.await("the clean stop reached the listener", r.envoy.arrived)
	var answered <-chan operatorAnswer
	if row.relaunch != nil {
		answered = row.relaunch(r)
	}
	testwait.WithinPolling(r.t, time.Until(r.end), testwait.Poll, "the relaunch to come up, or to wait for the clean stop in flight", func() bool {
		select {
		case <-r.rt.resumed:
			return true
		default:
		}
		return strings.Contains(r.logs.String(), `"msg":"supervise: the launch waits for the clean stop of the session the claim records"`)
	})
	return answered
}

// stopEndedOn waits for the line the clean stop ended on, requires it to be want, and answers the
// line's fields. A stop that ended on another line fails the row there, naming the budget the stop
// ran under: a read held past it ends at that budget, and the row says so rather than wait for a
// delete that will never come.
func (r *relaunchRig) stopEndedOn(want string) map[string]any {
	r.t.Helper()
	var ended map[string]any
	testwait.WithinPolling(r.t, time.Until(r.end), testwait.Poll, "the clean stop to log how it ended", func() bool {
		for _, line := range []string{stopStoppedLine, stopKeptLine, stopUnreadLine, stopUnconfirmedLine, stopNoEntryLine} {
			if lines := loggedLines(r.t, r.logs.String(), line); len(lines) > 0 {
				ended = lines[0]
				return true
			}
		}
		return false
	})
	if ended["msg"] != want {
		r.t.Fatalf("the clean stop ended on %q, under its budget of %s, want %q; log:\n%s", ended["msg"], loggedBudget(ended), want, r.logs)
	}
	return ended
}

// wantStopBudget requires the line the clean stop ended on to name the budget the row gave the
// daemon's stops: a stop given another is one the override (overrides.sessionStop) did not reach,
// which would end this row's held read at the real budget under load.
func (r *relaunchRig) wantStopBudget(ended map[string]any) {
	r.t.Helper()
	if budget, _ := ended["budget"].(float64); budget != float64(r.budget) {
		r.t.Fatalf("the clean stop ran under a budget of %s, want the %s the row gave the daemon's stops (overrides.sessionStop); log:\n%s",
			loggedBudget(ended), r.budget, r.logs)
	}
}

// loggedBudget is the budget a clean stop's line names, which the JSON log writes in nanoseconds.
func loggedBudget(line map[string]any) time.Duration {
	budget, _ := line["budget"].(float64)
	return time.Duration(budget)
}

// relaunchBegun is the step the first relaunch test notes at the listener (listenerRecorder.note)
// once the daemon has begun the relaunch, while the clean stop's read is still held there and has
// read nothing of the registry.
const relaunchBegun = "relaunch begun, the stop's read held unread"

// The relaunch that wakes a suspended claim, in the same session, is not removed by the clean
// stop of the process before it, whatever the listener's clock says (R3 of the suspension's clean
// stop), and neither is the relaunch a death decides at once. Each row makes the daemon start a
// relaunch whose runtime registers it the moment it starts, before the daemon's read of the
// registry: the listener holds the stop's read, unread, until the relaunch has come up or the
// daemon says it waits for the stop, and notes that moment (relaunchBegun). The listener's clock
// runs behind the daemon's (listenerBehind), so every registration it stamps reads as written
// before the stop's moment and the moment keeps none. Then the stop goes on.
//
// A daemon whose start did not wait would bring the relaunch up, and register it, before the
// read: the read would hand back the relaunch's own client instance, the delete would name it,
// and nothing would keep the relaunch's registration, which is what happens at 46795520, and what
// a start that skips its wait (Machine.awaitSessionStops) does here. The row checks the order at
// the listener first, so such a daemon fails by name: the relaunch registered before the read.
// The daemon starts no process of the claim while a clean stop of its session is in flight, so
// the relaunch it began registers only once the stop's read and delete have returned: the read
// answers the ended process's instance, the delete removes that registration alone, and the
// relaunch's stays. A relaunch this daemon starts cannot register before the read on any path:
// the stop's channel closes only once its send has returned, the wait has no bound of its own,
// and a read the stop gives up sends no delete (TestARelaunchWaitsForTheCleanStopHoweverLongItRuns
// and TestCleanSessionStopEndsAtItsOwnBudgetWhenTheListenerNeverAnswers).
//
// A process the daemon did not start that registers the session before the read is the one case
// left to the moment alone, which a listener clock behind the daemon's cannot keep: the client
// instance is minted inside the agent's own process, and nothing the daemon records of the claim
// names it, so the read is the stop's only way to learn it.
//
// The row's own waits, the daemon's boot, the operator's requests and the stop's read and delete
// all end by the row's own end (relaunchRowEnd), so a slow boot or a slow relaunch is not read as
// a daemon that never answered or a listener that never did, and a row that fails fails at it.
func TestARelaunchIsNotRemovedByTheStopBeforeItWhenTheListenersClockIsBehind(t *testing.T) {
	for _, row := range relaunchRows() {
		t.Run(row.name, func(t *testing.T) {
			envoy := &listenerRecorder{heldRead: make(chan struct{})}
			r := startRelaunchRig(t, envoy, nil)
			answered := r.stopSentAndRelaunched(row)
			envoy.note(relaunchBegun)
			envoy.releaseRead()
			r.answered("the request that relaunched the claim", answered)
			r.await("the relaunch came up", r.rt.resumed)
			ended := r.stopEndedOn(stopStoppedLine)
			r.await("the stop's delete was judged", envoy.settled)

			if got, want := envoy.happened(), []string{relaunchBegun, "read answered inst_ended", "delete judged", "registered inst_relaunched"}; !slices.Equal(got, want) {
				t.Fatalf("the listener saw %q, want %q: the relaunch the daemon began while the stop's read was held registered before that read, so the read named the relaunch's own instance; the daemon must start it only once the stop's read and delete have returned",
					got, want)
			}
			if held := envoy.holding(); held.absent || held.clientInstanceID != "inst_relaunched" {
				t.Fatalf("the listener holds %+v once the stop's delete %q was answered, want the relaunch's registration, inst_relaunched: the stop of the process before it removed it",
					held, envoy.delete.query)
			}
			if got := envoy.deleteConditions(t).Get("client_instance_id"); got != "inst_ended" {
				t.Fatalf("the stop's delete named client instance %q, want the ended process's own, inst_ended", got)
			}
			r.wantStopBudget(ended)
		})
	}
}

// A stop that lands after the relaunch's registration keeps it, whatever the listener's clock
// says (R3 of the suspension's clean stop: "is not removed by a stop that lands after its
// registration"). The daemon's wait for the stop ends when the stop does, and a stop can end with
// its delete still on its way to the listener: the budget ran out with the delete sent. Each row
// lets the relaunch register after the stop's read and before its delete lands: the listener
// answers the read, holds the delete, and the test then ends the stop's context
// (sessionStopOverrides.end), as the end of its budget would, so the relaunch starts and
// registers; only then does the listener judge the delete, with its clock behind the daemon's.
// The delete names the ended process's instance, which the read answered, so the listener keeps
// the relaunch's registration. At 46795520 the relaunch registers before the read is answered, the
// read hands back the relaunch's own instance, and the delete that lands removes it.
func TestAStopThatLandsAfterTheRelaunchsRegistrationKeepsIt(t *testing.T) {
	for _, row := range relaunchRows() {
		t.Run(row.name, func(t *testing.T) {
			envoy := &listenerRecorder{heldRead: make(chan struct{}), heldDelete: make(chan struct{}), deleteArrived: make(chan struct{})}
			stopsEnd := make(chan struct{})
			r := startRelaunchRig(t, envoy, stopsEnd)
			answered := r.stopSentAndRelaunched(row)
			envoy.releaseRead()
			r.await("the stop's delete reached the listener", envoy.deleteArrived)
			r.stopsEnded(stopsEnd)
			r.answered("the request that relaunched the claim", answered)
			r.await("the relaunch came up", r.rt.resumed)
			ended := r.stopEndedOn(stopUnconfirmedLine)
			if e, _ := ended["error"].(string); !strings.Contains(e, context.Canceled.Error()) {
				t.Fatalf("the clean stop ended with %q, want the end the row gave it (sessionStopOverrides.end), %q: a stop that ran on to its budget; log:\n%s", e, context.Canceled, r.logs)
			}
			envoy.releaseDelete()
			r.await("the stop's delete was judged", envoy.settled)

			if held := envoy.holding(); held.absent || held.clientInstanceID != "inst_relaunched" {
				t.Fatalf("the listener holds %+v once the stop's delete %q landed, want the relaunch's registration, inst_relaunched: the stop of the process before it removed it",
					held, envoy.delete.query)
			}
			if got := envoy.deleteConditions(t).Get("client_instance_id"); got != "inst_ended" {
				t.Fatalf("the stop's delete named client instance %q, want the ended process's own, inst_ended", got)
			}
			if got, want := envoy.happened(), []string{"read answered inst_ended", "registered inst_relaunched", "delete judged"}; !slices.Equal(got, want) {
				t.Fatalf("the listener saw %q, want %q: the relaunch registers after the stop's read and before its delete lands", got, want)
			}
			r.wantStopBudget(ended)
		})
	}
}
