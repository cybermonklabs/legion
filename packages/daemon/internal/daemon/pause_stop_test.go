package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// pauseIssue is the one issue of the pause rigs: LEGION-208, its own tree's root, in implementing
// at generation 1, so its implementer is the open phase's worker and its tester is not.
const pauseIssue = "LEGION-208"

// pauseOperator is the operator, who pauses and resumes in these tests.
var pauseOperator = store.PauseActor{Kind: store.PauseByOperator}

// pauseRig is project LEGION on a database of its own as a pause's stop finds it: LEGION-208
// recorded in implementing, a claim of each role the test names live on it, and the daemon's half
// of the pause over that store (projectPause) with an hour's backstop, on the wall clock, writing
// a JSON log the tests read. Its pause service has no Effects: these tests make the stops the
// machines would make by hand.
type pauseRig struct {
	t      *testing.T
	ctx    context.Context
	pool   *store.Pool
	st     *store.Store
	pauses *projectPause
	logs   *testlog.Buffer
}

func newPauseRig(t *testing.T, roles ...claim.Role) *pauseRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	st := outboxTreeStore(t, pool)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: pauseIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: pauseIssue, Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress",
		Rank: "U", LastDispatchSeq: 5})
	r := &pauseRig{t: t, ctx: ctx, pool: pool, st: st, logs: &testlog.Buffer{}}
	for _, role := range roles {
		if err := st.PutClaim(ctx, r.claim(role, supervise.StateIdle)); err != nil {
			t.Fatalf("seed the %s's claim: %v", role, err)
		}
	}
	log := slog.New(slog.NewJSONHandler(r.logs, nil))
	podRecords, err := st.PodRecordsOf("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	r.pauses = &projectPause{store: st, records: projectRecords{Store: records, project: "LEGION"}, pods: podRecords, project: "LEGION", token: "legion",
		backstop: time.Hour, now: time.Now, log: log, workflow: &workflowRuntime{}}
	if r.pauses.service, err = pause.New(pause.Options{Store: st, Records: records, Project: "LEGION", Config: config.Config{}, Log: log}); err != nil {
		t.Fatal(err)
	}
	return r
}

// claim is role's claim of LEGION-208 in state, as the machine writes it; a suspended one carries
// the source a pause's stop suspends with.
func (r *pauseRig) claim(role claim.Role, state supervise.ClaimState) supervise.Claim {
	c := supervise.Claim{Token: mustClaimToken(r.t, pauseIssue, role), Project: "legion", Tree: pauseIssue, Issue: pauseIssue,
		Role: role, Generation: 1, ServingGeneration: 1, State: state, Session: "ses-" + string(role)}
	if state == supervise.StateSuspended {
		c.SuspendedBy = supervise.SuspendedByReleased
	}
	return c
}

// pauseLegion pauses LEGION through the store's own transaction with the pause service's stop
// rows, as the service's Pause does, sets the service's flag from it as the outbox's sweep does
// (pause.Service.Refresh), and answers the pause.
func (r *pauseRig) pauseLegion() store.Pause {
	r.t.Helper()
	held, _, err := r.st.Pause(r.ctx, store.PauseRequest{Project: "LEGION", By: pauseOperator, Reason: "the model provider is down",
		At: time.Now(), StopRow: pause.StopRow})
	if err != nil {
		r.t.Fatalf("pause LEGION: %v", err)
	}
	if err := r.pauses.service.Refresh(r.ctx); err != nil {
		r.t.Fatalf("set the pause's flag: %v", err)
	}
	return held
}

// supervisor is an outbox's supervisor whose machines read the rig's pause flag
// (supervise.Deps.Paused), as the daemon's own read the pause service's: a `pause_stop` row's stop
// is decided against the pause that stands when the machine decides it.
func (r *pauseRig) supervisor() (*supervisor, *fake.Runtime) {
	r.t.Helper()
	sup, rt := newOutboxSupervisor(r.t, "legion", r.t.TempDir())
	sup.deps.Paused = r.pauses.service.Paused
	return sup, rt
}

// resume ends LEGION's pause through the pause service, which logs each row the pause held.
func (r *pauseRig) resume() store.Pause {
	r.t.Helper()
	ended, _, err := r.pauses.service.Resume(r.ctx, pauseOperator)
	if err != nil {
		r.t.Fatalf("resume LEGION: %v", err)
	}
	return ended
}

// mustStop runs the stop transaction for c, as the machine's write of a pause's suspension does.
func (r *pauseRig) mustStop(c supervise.Claim, stop supervise.PauseStop) {
	r.t.Helper()
	if err := r.pauses.stopped(r.ctx, c, stop); err != nil {
		r.t.Fatalf("the stop transaction of %s for pause %d: %v", c.Token, stop.Pause, err)
	}
}

// count is the one number sql counts.
func (r *pauseRig) count(sql string, args ...any) int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(r.ctx, sql, args...).Scan(&n); err != nil {
		r.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// pauseStopRecord is one stop row of a pause.
type pauseStopRecord struct {
	end  store.PauseStopEnd
	owed bool
}

// stops is every stop row pause holds of role's claim, oldest first.
func (r *pauseRig) stops(pause int64, role claim.Role) []pauseStopRecord {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select coalesce(ended_as, ''), launch_owed from project_pause_stops
		where pause_id = $1 and claim = $2 order by id`, pause, string(mustClaimToken(r.t, pauseIssue, role)))
	if err != nil {
		r.t.Fatalf("read the stops of pause %d: %v", pause, err)
	}
	defer rows.Close()
	var stops []pauseStopRecord
	for rows.Next() {
		var s pauseStopRecord
		var end string
		if err := rows.Scan(&end, &s.owed); err != nil {
			r.t.Fatal(err)
		}
		s.end = store.PauseStopEnd(end)
		stops = append(stops, s)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the stops of pause %d: %v", pause, err)
	}
	return stops
}

// heldStart is one start row of a role of LEGION-208, with the wake and the pause it names.
type heldStart struct {
	id    int64
	wake  string
	pause int64
	task  string
}

// starts is every start row of role, oldest first.
func (r *pauseRig) starts(role claim.Role) []heldStart {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select id, coalesce(payload->>'wakeKind', ''), coalesce((payload->>'pause')::bigint, 0),
		coalesce(payload->>'task', '') from outbox
		where kind = 'supervise' and issue = $1 and payload->>'op' = 'start' and payload->>'role' = $2 order by id`, pauseIssue, string(role))
	if err != nil {
		r.t.Fatalf("read the starts of the %s: %v", role, err)
	}
	defer rows.Close()
	var starts []heldStart
	for rows.Next() {
		var s heldStart
		if err := rows.Scan(&s.id, &s.wake, &s.pause, &s.task); err != nil {
			r.t.Fatal(err)
		}
		starts = append(starts, s)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the starts of the %s: %v", role, err)
	}
	return starts
}

// heldWakes counts role's resumes a pause holds (pod_resumes.pause_id), and open the ones of them
// no first turn stamped and nothing closed.
func (r *pauseRig) heldWakes(pause int64, role claim.Role) (all, open int) {
	r.t.Helper()
	all = r.count(`select count(*) from pod_resumes where issue = $1 and role = $2 and pause_id = $3`, pauseIssue, string(role), pause)
	open = r.count(`select count(*) from pod_resumes where issue = $1 and role = $2 and pause_id = $3
		and first_turn_at is null and closed_at is null`, pauseIssue, string(role), pause)
	return all, open
}

// releasedLines is every `pause: a held row released` line the rig's log holds, decoded.
func (r *pauseRig) releasedLines() []map[string]any {
	r.t.Helper()
	return r.lines(pause.HeldRowReleasedMessage)
}

// lines is every line of the rig's log whose message is msg, decoded.
func (r *pauseRig) lines(msg string) []map[string]any {
	r.t.Helper()
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(r.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			r.t.Fatalf("decode the log line %q: %v", line, err)
		}
		if entry["msg"] == msg {
			lines = append(lines, entry)
		}
	}
	return lines
}

// wantReleasedOnce fails unless the log names start row as released once, by pause.
func (r *pauseRig) wantReleasedOnce(row, pause int64) {
	r.t.Helper()
	lines := r.releasedLines()
	if len(lines) != 1 || int64(lines[0]["row"].(float64)) != row || int64(lines[0]["pause"].(float64)) != pause {
		r.t.Fatalf("released held rows logged = %v, want start row %d once, by pause %d", lines, row, pause)
	}
}

// The machine owes a stop whose commit's answer was lost and makes it again with its next claim
// write (supervise.Deps.PauseStopped), so the stop transaction made twice for one pause and claim
// writes each of its records once. The implementer, the open phase's worker, is owed no launch:
// its stop arms its backstop, and the backstop has fired by the time the stop is made again, which
// a second arming would not notice. The tester is owed a launch: its stop takes a held wake with
// its start row, and the second run would close that wake as the role's open resume and take
// another. The pod is held from the first stop of the pause on, and that hold has ended by the
// second run, which a second open would not notice either. The first stop also makes the
// implementer's `pause_stop` row, waiting on its bounded retry, due at once.
func TestAPauseStopMadeAgainWritesItsRecordsOnce(t *testing.T) {
	r := newPauseRig(t, claim.RoleImplementer, claim.RoleTester)
	held := r.pauseLegion()
	if _, err := r.pool.Exec(r.ctx, `update outbox set next_at = now() + interval '1 hour'
		where payload->>'op' = 'pause_stop' and payload->>'role' = 'implementer'`); err != nil {
		t.Fatal(err)
	}

	implementer := r.claim(claim.RoleImplementer, supervise.StateSuspended)
	r.mustStop(implementer, supervise.PauseStop{Pause: held.ID})
	if due := r.count(`select count(*) from outbox where payload->>'op' = 'pause_stop' and payload->>'role' = 'implementer'
		and next_at <= now()`); due != 1 {
		t.Fatalf("the implementer's pause_stop rows due = %d, want its waiting row made due by the stop", due)
	}
	if _, err := r.pool.Exec(r.ctx, `update pod_wake_backstops set fired = true where issue = $1`, pauseIssue); err != nil {
		t.Fatal(err)
	}
	r.mustStop(implementer, supervise.PauseStop{Pause: held.ID})

	tester := r.claim(claim.RoleTester, supervise.StateSuspended)
	owed := supervise.PauseStop{Pause: held.ID, LaunchOwed: true}
	r.mustStop(tester, owed)
	if _, err := r.st.EndPausePodHold(r.ctx, "LEGION", pauseIssue, store.PausePodHoldReady, time.Now()); err != nil {
		t.Fatal(err)
	}
	r.mustStop(tester, owed)

	if got := r.stops(held.ID, claim.RoleImplementer); len(got) != 1 || got[0] != (pauseStopRecord{end: store.PauseStopStopped}) {
		t.Fatalf("the implementer's stops = %+v, want one stopped and owed no launch", got)
	}
	if got := r.count(`select count(*) from pod_wake_backstops where issue = $1 and role = 'implementer'`, pauseIssue); got != 1 {
		t.Fatalf("the implementer's backstops = %d, want the one its first stop armed", got)
	}
	if got := r.stops(held.ID, claim.RoleTester); len(got) != 1 || got[0] != (pauseStopRecord{end: store.PauseStopStopped, owed: true}) {
		t.Fatalf("the tester's stops = %+v, want one stopped and owed its launch", got)
	}
	if all, open := r.heldWakes(held.ID, claim.RoleTester); all != 1 || open != 1 {
		t.Fatalf("the tester's held wakes = %d, %d open; want the one its first stop took, still open", all, open)
	}
	starts := r.starts(claim.RoleTester)
	if len(starts) != 1 || starts[0].wake != pods.WakePause || starts[0].pause != held.ID || starts[0].task != "" {
		t.Fatalf("the tester's starts = %+v, want one wake start of pause %d carrying no task", starts, held.ID)
	}
	if got := r.starts(claim.RoleImplementer); len(got) != 0 {
		t.Fatalf("the implementer's starts = %+v, want none: it was owed no launch", got)
	}
	if got := r.count(`select count(*) from project_pause_pod_holds where issue = $1`, pauseIssue); got != 1 {
		t.Fatalf("holds of %s's pod = %d, want the one the first stop opened", pauseIssue, got)
	}
	if _, found, err := r.st.PausePodHoldOf(r.ctx, "LEGION", pauseIssue); err != nil || found {
		t.Fatalf("a hold of %s's pod stands = %t, %v; want the ended one alone", pauseIssue, found, err)
	}
}

// A stop still owed reaches the store with a later write of the claim, which can be a claim
// running again (supervise's putStops): the resume let its held launch out, say. The stop is
// recorded with the claim, and nothing a stopped role is given: no held wake and no start row for
// the launch it was owed, which is running, no backstop for the open phase's worker, no pod hold,
// and the role's open resume left open, since it is the wake that launch is answering. That wake
// is taken before the pause, so it is no wake of the pause: a wake taken while the project is
// paused is the open pause's (store.PodRecords.PauseOfWake).
func TestAStopOwedToAClaimRunningAgainRecordsTheStopAlone(t *testing.T) {
	r := newPauseRig(t, claim.RoleImplementer, claim.RoleTester)
	if taken, err := r.st.TakePodWake(r.ctx, "legion", pauseIssue, mustClaimToken(t, pauseIssue, claim.RoleTester),
		pods.WakeChecks, time.Now(), 0); err != nil || !taken {
		t.Fatalf("take the tester's wake = %t, %v", taken, err)
	}
	held := r.pauseLegion()

	r.mustStop(r.claim(claim.RoleImplementer, supervise.StateLaunching), supervise.PauseStop{Pause: held.ID})
	r.mustStop(r.claim(claim.RoleTester, supervise.StateLaunching), supervise.PauseStop{Pause: held.ID, LaunchOwed: true})

	if got := r.stops(held.ID, claim.RoleImplementer); len(got) != 1 || got[0].end != store.PauseStopStopped {
		t.Fatalf("the implementer's stops = %+v, want its stop recorded", got)
	}
	if got := r.stops(held.ID, claim.RoleTester); len(got) != 1 || got[0] != (pauseStopRecord{end: store.PauseStopStopped, owed: true}) {
		t.Fatalf("the tester's stops = %+v, want its stop recorded with the launch it was owed", got)
	}
	if got := r.count(`select count(*) from claims where issue = $1 and state = 'launching'`, pauseIssue); got != 2 {
		t.Fatalf("claims written launching = %d, want both claims written as they are", got)
	}
	if all, _ := r.heldWakes(held.ID, claim.RoleTester); all != 0 {
		t.Fatalf("the tester's held wakes = %d, want none: its launch runs", all)
	}
	if got := r.count(`select count(*) from pod_resumes where issue = $1 and role = 'tester' and first_turn_at is null and closed_at is null`, pauseIssue); got != 1 {
		t.Fatalf("the tester's open resumes = %d, want its wake's resume left open", got)
	}
	if got := len(r.starts(claim.RoleTester)) + len(r.starts(claim.RoleImplementer)); got != 0 {
		t.Fatalf("start rows = %d, want none", got)
	}
	if got := r.count(`select count(*) from pod_wake_backstops where issue = $1`, pauseIssue); got != 0 {
		t.Fatalf("backstops = %d, want none: the open phase's worker runs", got)
	}
	if got := r.count(`select count(*) from project_pause_pod_holds where issue = $1`, pauseIssue); got != 0 {
		t.Fatalf("holds of %s's pod = %d, want none: its roles run", pauseIssue, got)
	}
}

// A stop made while its pause stands holds the pod whatever the pod's other roles do: the tester
// still runs here, and an update round past its bound or an idle rule would otherwise take the pod
// with the stopped implementer's unpushed work on its disk. A stop that ends after the resume holds
// the pod only where no role of it is live then (decision 6 of dispatch://INFRA-3759): the
// implementer's stop, with the tester live, holds nothing, and the tester's, the pod's last live
// role, holds it.
func TestAStopHoldsThePodWhileItsPauseStandsAndAfterTheResumeOnlyWithNoRoleLive(t *testing.T) {
	t.Run("while the pause stands", func(t *testing.T) {
		r := newPauseRig(t, claim.RoleImplementer, claim.RoleTester)
		held := r.pauseLegion()
		r.mustStop(r.claim(claim.RoleImplementer, supervise.StateSuspended), supervise.PauseStop{Pause: held.ID})
		if hold, found, err := r.st.PausePodHoldOf(r.ctx, "LEGION", pauseIssue); err != nil || !found || hold.Pause != held.ID {
			t.Fatalf("%s's hold = %+v, %t, %v; want the implementer's stop to hold the pod for pause %d with the tester live",
				pauseIssue, hold, found, err, held.ID)
		}
	})
	t.Run("after the resume", func(t *testing.T) {
		r := newPauseRig(t, claim.RoleImplementer, claim.RoleTester)
		held := r.pauseLegion()
		r.resume()
		r.mustStop(r.claim(claim.RoleImplementer, supervise.StateSuspended), supervise.PauseStop{Pause: held.ID})
		if hold, found, err := r.st.PausePodHoldOf(r.ctx, "LEGION", pauseIssue); err != nil || found {
			t.Fatalf("%s's hold = %+v, %t, %v; want none: the stop ended after the resume, with the tester live", pauseIssue, hold, found, err)
		}
		r.mustStop(r.claim(claim.RoleTester, supervise.StateSuspended), supervise.PauseStop{Pause: held.ID})
		if hold, found, err := r.st.PausePodHoldOf(r.ctx, "LEGION", pauseIssue); err != nil || !found || hold.Pause != held.ID {
			t.Fatalf("%s's hold = %+v, %t, %v; want the tester's stop, the pod's last live role's, to hold it", pauseIssue, hold, found, err)
		}
	})
}

// The boot of a paused project asks again, at once, the stops the pause still waits on
// (projectPause.askBootStops): a stop its claim held for a turn waits on its row's bounded retry,
// and the hold itself was in the stopped daemon's memory, so the row is made due and its stop is
// asked of the restarted machine as soon as the outbox runs. Left waiting, a turn that ended
// inside the wait would leave the role idle and unstopped. A stop already asked is asked of no
// second row.
func TestTheBootOfAPausedProjectAsksItsWaitingStopsAgainAtOnce(t *testing.T) {
	r := newPauseRig(t, claim.RoleImplementer)
	held := r.pauseLegion()
	if _, err := r.pool.Exec(r.ctx, `update outbox set next_at = now() + interval '10 minutes'
		where payload->>'op' = 'pause_stop'`); err != nil {
		t.Fatal(err)
	}
	waiting := `select count(*) from outbox where payload->>'op' = 'pause_stop' and next_at > now()`
	if got := r.count(waiting); got != 1 {
		t.Fatalf("pause_stop rows waiting = %d, want the implementer's, held for its turn", got)
	}

	if err := r.pauses.askBootStops(r.ctx, held); err != nil {
		t.Fatalf("the boot's pass: %v", err)
	}

	if got := r.count(waiting); got != 0 {
		t.Fatalf("pause_stop rows still waiting after the boot's pass = %d, want none: each is asked again at once", got)
	}
	if got := r.count(`select count(*) from outbox where payload->>'op' = 'pause_stop'`); got != 1 {
		t.Fatalf("pause_stop rows = %d, want the one the pause queued: the boot asks no stop twice", got)
	}
}

// A live claim of another project's issue that the pause has not stopped — one an earlier daemon
// launched before the pause reached it, which the operator's spawn made under this project's
// token — is a session this daemon runs, so the boot of a paused project asks its stop as it asks
// every other live role's (askBootStops), and the catch-up after a pause would too (pausable): a
// stop row under the pause's project, and a `pause_stop` row keyed by the project, which this
// daemon's outbox runs, naming the claim's issue. The executor reads the claim from that issue and
// stops it, and the stop transaction records the stop under the pause's project and holds the
// claim's pod there.
func TestTheBootOfAPausedProjectStopsALiveClaimOfAnotherProjectsIssue(t *testing.T) {
	r := newPauseRig(t)
	sup, rt := r.supervisor()
	sup.deps.PauseStopped = r.pauses.stopped
	r.pauses.supervisor = sup
	const other = "INFRA-5"
	foreign := launchedClaim(t, sup, other, other, claim.RoleImplementer)
	machine, _ := sup.Machine(foreign)
	held := r.pauseLegion()
	live := machine.Claim()
	if err := r.st.PutClaim(r.ctx, live); err != nil {
		t.Fatalf("store the live claim of %s: %v", other, err)
	}
	if !pausable(live) {
		t.Fatalf("the catch-up would ask no stop of %s, a live claim the daemon runs", foreign)
	}

	if err := r.pauses.askBootStops(r.ctx, held); err != nil {
		t.Fatalf("the boot's pass with a live claim of another project's issue: %v", err)
	}
	asked := r.lines("pause: the boot asked the stop of the live roles the project's pause has not stopped")
	if len(asked) != 1 {
		t.Fatalf("the boot's lines that it asked stops = %d, want one; log %s", len(asked), r.logs.String())
	}
	if got, ok := asked[0]["claims"].([]any); !ok || len(got) != 1 || got[0] != string(foreign) {
		t.Fatalf("the boot asked the stops of %v, want %s's", asked[0]["claims"], foreign)
	}
	if got := r.count(`select count(*) from outbox where issue = $1`, other); got != 0 {
		t.Fatalf("outbox rows keyed by %s = %d, want none: this daemon's outbox runs only LEGION's keys", other, got)
	}
	row := record.OutboxRow{Kind: record.OutboxKindSupervise}
	if err := r.pool.QueryRow(r.ctx, `select id, issue, payload from outbox where kind = 'supervise' and issue = 'LEGION'
		and payload->>'op' = 'pause_stop' and payload->>'issue' = $1`, other).Scan(&row.ID, &row.Issue, &row.Payload); err != nil {
		t.Fatalf("read the stop row of %s keyed by LEGION: %v", foreign, err)
	}

	runner := &outbox{pool: r.pool, records: record.NewStore(), supervisor: sup, trees: r.st, project: "legion",
		dispatchProject: "LEGION", log: quietLogger(), now: time.Now}
	if err := runner.execute(r.ctx, row); err != nil {
		t.Fatalf("run the stop of %s: %v", foreign, err)
	}
	if c := machine.Claim(); c.State != supervise.StateSuspended || c.SuspendedBy != supervise.SuspendedByReleased {
		t.Fatalf("%s is %s by %s after its stop row ran, want it stopped for the pause", foreign, c.State, c.SuspendedBy)
	}
	if got := len(rt.CallsOf("Suspend")); got != 1 {
		t.Fatalf("the runtime was asked to stop %d processes, want the one of %s", got, foreign)
	}
	var project, end string
	if err := r.pool.QueryRow(r.ctx, `select project, coalesce(ended_as, '') from project_pause_stops where pause_id = $1 and claim = $2`,
		held.ID, string(foreign)).Scan(&project, &end); err != nil || project != "LEGION" || end != string(store.PauseStopStopped) {
		t.Fatalf("the stop of %s = project %q ended %q, %v; want LEGION's stop ended stopped", foreign, project, end, err)
	}
	if hold, found, err := r.st.PausePodHoldOf(r.ctx, "LEGION", other); err != nil || !found || hold.Pause != held.ID {
		t.Fatalf("the hold of %s's pod = %+v, %t, %v; want pause %d's", other, hold, found, err, held.ID)
	}
}

// waitingOnThePauseLock is whether a backend waits for issue's pause lock (store.LockIssueForPause),
// read by its two keys in pg_locks.
func waitingOnThePauseLock(t *testing.T, pool *store.Pool, issue string) bool {
	t.Helper()
	var waiting bool
	if err := pool.QueryRow(context.Background(), `select exists (select 1 from pg_locks
		where locktype = 'advisory' and not granted and objsubid = 2
		and database = (select oid from pg_database where datname = current_database())
		and classid = hashtext('legion.pause.issue')::oid and objid = hashtext($1)::oid)`, issue).Scan(&waiting); err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	return waiting
}

// waitingOnARowLock is whether a backend of this database waits for a lock another transaction
// holds, as a resume does on the pause's row a stop transaction holds for share.
func waitingOnARowLock(t *testing.T, pool *store.Pool) bool {
	t.Helper()
	var waiting bool
	if err := pool.QueryRow(context.Background(), `select exists (select 1 from pg_stat_activity
		where datname = current_database() and wait_event_type = 'Lock')`).Scan(&waiting); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	return waiting
}

// Two stops of one pod's roles that overlap after the resume run one after the other on the
// issue's lock: the implementer's stop, still uncommitted, reads the tester live and holds no
// pod; the tester's stop waits for it, then reads the implementer stopped and holds the pod the
// first left to it. Without the lock the second would read the first's claim as it stood before
// and hold nothing, and the pod would stand released with no role in it and no hold. The
// tester's stop is owed a launch, and its start, which no pause holds any more, is logged as a
// released held row.
func TestTheLaterOfTwoOverlappingStopsHoldsThePodTheEarlierLeftIt(t *testing.T) {
	r := newPauseRig(t, claim.RoleImplementer, claim.RoleTester)
	held := r.pauseLegion()
	r.resume()

	first, err := r.st.BeginTx(r.ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Rollback(r.ctx) }()
	if _, err := r.pauses.stopTx(r.ctx, first, r.claim(claim.RoleImplementer, supervise.StateSuspended), supervise.PauseStop{Pause: held.ID}, time.Now()); err != nil {
		t.Fatalf("the implementer's stop: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- r.pauses.stopped(r.ctx, r.claim(claim.RoleTester, supervise.StateSuspended), supervise.PauseStop{Pause: held.ID, LaunchOwed: true})
	}()
	testwait.Eventually(t, "the tester's stop to wait on LEGION-208's pause lock", func() bool {
		return waitingOnThePauseLock(t, r.pool, pauseIssue)
	})
	select {
	case err := <-done:
		t.Fatalf("the tester's stop finished (%v) while the implementer's held the issue's lock", err)
	default:
	}
	if err := first.Commit(r.ctx); err != nil {
		t.Fatalf("commit the implementer's stop: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the tester's stop: %v", err)
		}
	case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
		t.Fatal("the tester's stop never finished after the implementer's committed")
	}

	hold, found, err := r.st.PausePodHoldOf(r.ctx, "LEGION", pauseIssue)
	if err != nil || !found || hold.Pause != held.ID {
		t.Fatalf("%s's hold = %+v, %t, %v; want the tester's stop to hold the pod for pause %d", pauseIssue, hold, found, err, held.ID)
	}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		if got := r.stops(held.ID, role); len(got) != 1 || got[0].end != store.PauseStopStopped {
			t.Fatalf("the %s's stops = %+v, want the cancelled stop recorded stopped", role, got)
		}
	}
	starts := r.starts(claim.RoleTester)
	if len(starts) != 1 {
		t.Fatalf("the tester's starts = %+v, want the one its owed launch queued", starts)
	}
	r.wantReleasedOnce(starts[0].id, held.ID)
}

// A resume waits for a stop transaction under way, on the pause's row the stop holds for share:
// the resume commits after the stop, so it reads the stop's records as committed. The
// implementer's stop ends its stop row stopped, and the resume cancels the tester's alone; the
// start the implementer is owed is one of the rows the resume read as held and logs; and the
// resume starts that held wake's stuck clock at its own moment. Without the row lock the resume
// would read the held rows before the stop committed and leave its start out of the log.
func TestAResumeWaitsForAStopTransactionUnderWay(t *testing.T) {
	r := newPauseRig(t, claim.RoleImplementer, claim.RoleTester)
	held := r.pauseLegion()

	first, err := r.st.BeginTx(r.ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Rollback(r.ctx) }()
	if _, err := r.pauses.stopTx(r.ctx, first, r.claim(claim.RoleImplementer, supervise.StateSuspended),
		supervise.PauseStop{Pause: held.ID, LaunchOwed: true}, time.Now()); err != nil {
		t.Fatalf("the implementer's stop: %v", err)
	}
	type resumed struct {
		ended store.Pause
		held  []record.PauseHeldRow
		err   error
	}
	done := make(chan resumed, 1)
	go func() {
		ended, rows, err := r.pauses.service.Resume(r.ctx, pauseOperator)
		done <- resumed{ended, rows, err}
	}()
	testwait.Eventually(t, "the resume to wait on the pause's row", func() bool { return waitingOnARowLock(t, r.pool) })
	select {
	case got := <-done:
		t.Fatalf("the resume finished (%v) while the implementer's stop held the pause's row", got.err)
	default:
	}
	if err := first.Commit(r.ctx); err != nil {
		t.Fatalf("commit the implementer's stop: %v", err)
	}
	var got resumed
	select {
	case got = <-done:
	case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
		t.Fatal("the resume never finished after the stop committed")
	}
	if got.err != nil {
		t.Fatalf("resume: %v", got.err)
	}

	if stops := r.stops(held.ID, claim.RoleImplementer); len(stops) != 1 || stops[0] != (pauseStopRecord{end: store.PauseStopStopped, owed: true}) {
		t.Fatalf("the implementer's stops = %+v, want its stop recorded before the resume", stops)
	}
	if stops := r.stops(held.ID, claim.RoleTester); len(stops) != 1 || stops[0].end != store.PauseStopCancelled {
		t.Fatalf("the tester's stops = %+v, want the one the resume cancelled", stops)
	}
	starts := r.starts(claim.RoleImplementer)
	if len(starts) != 1 || len(got.held) != 1 || got.held[0].ID != starts[0].id {
		t.Fatalf("the implementer's starts = %+v and the resume read %+v as held, want its owed start among them", starts, got.held)
	}
	r.wantReleasedOnce(starts[0].id, held.ID)
	var clock time.Time
	if err := r.pool.QueryRow(r.ctx, `select clock_at from pod_resumes where issue = $1 and role = 'implementer' and pause_id = $2`,
		pauseIssue, held.ID).Scan(&clock); err != nil {
		t.Fatal(err)
	}
	if !clock.Equal(got.ended.EndedAt) {
		t.Fatalf("the held wake's stuck clock = %s, want the resume's moment %s", clock, got.ended.EndedAt)
	}
}

// A pause, its resume, and a second pause while a stop of the first is still running: the late
// stop writes the first pause's records all the same — its stop, the pod's hold, the held wake
// and its start — and finds the second pause open, so it logs nothing as released. The second
// pause holds that start in the outbox, its own stop of the role finds the role stopped and ends
// taken over, and the second pause's resume alone logs the start as released.
func TestALateStopOfAPauseIsHeldByTheNextPauseAndLoggedByItsResume(t *testing.T) {
	r := newPauseRig(t, claim.RoleImplementer)
	earlier := r.pauseLegion()
	r.resume()
	later := r.pauseLegion()

	r.mustStop(r.claim(claim.RoleImplementer, supervise.StateSuspended), supervise.PauseStop{Pause: earlier.ID, LaunchOwed: true})
	if got := r.stops(earlier.ID, claim.RoleImplementer); len(got) != 1 || got[0] != (pauseStopRecord{end: store.PauseStopStopped, owed: true}) {
		t.Fatalf("the earlier pause's stops = %+v, want its cancelled stop recorded stopped with the launch owed", got)
	}
	if hold, found, err := r.st.PausePodHoldOf(r.ctx, "LEGION", pauseIssue); err != nil || !found || hold.Pause != earlier.ID {
		t.Fatalf("%s's hold = %+v, %t, %v; want the earlier pause's", pauseIssue, hold, found, err)
	}
	if _, open := r.heldWakes(earlier.ID, claim.RoleImplementer); open != 1 {
		t.Fatalf("the earlier pause's open held wakes = %d, want its owed launch's", open)
	}
	starts := r.starts(claim.RoleImplementer)
	if len(starts) != 1 || starts[0].pause != earlier.ID {
		t.Fatalf("the implementer's starts = %+v, want the earlier pause's owed start", starts)
	}
	if lines := r.releasedLines(); len(lines) != 0 {
		t.Fatalf("released held rows logged = %v, want none while the later pause holds the start", lines)
	}
	var heldRows []record.PauseHeldRow
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		heldRows, err = record.NewStore().PauseHeldRows(r.ctx, tx, "LEGION", time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(heldRows) != 1 || heldRows[0].ID != starts[0].id {
		t.Fatalf("rows the later pause holds = %+v, want the earlier pause's start", heldRows)
	}

	sup, _ := r.supervisor()
	releasedClaim(t, sup, claim.RoleImplementer)
	runner := &outbox{pool: r.pool, records: record.NewStore(), supervisor: sup, trees: r.st, project: "legion",
		dispatchProject: "LEGION", log: quietLogger(), now: time.Now}
	if err := runner.execute(r.ctx, mustOutboxRow(t, pauseIssue, record.SuperviseRequest{Op: "pause_stop", Tree: pauseIssue,
		Role: claim.RoleImplementer, Generation: 1, Pause: later.ID, Reason: "project LEGION is paused"}, time.Now())); err != nil {
		t.Fatalf("run the later pause's stop of the implementer: %v", err)
	}
	if got := r.stops(later.ID, claim.RoleImplementer); len(got) != 1 || got[0].end != store.PauseStopTakenOver {
		t.Fatalf("the later pause's stops = %+v, want its stop ended: the earlier pause's late stop stopped the role", got)
	}

	r.resume()
	r.wantReleasedOnce(starts[0].id, later.ID)
}

// A stop that commits after its pause has ended, with no later pause open, owes the role a launch
// even where it met none and holds no task: the machine decided the suspension while the resume's
// transaction was in flight, with the flag still set, and its stop transaction then waited on the
// pause's row and committed after the resume. The role was live when the resume ended the pause,
// so the resume's promise is that it runs again (R4 of dispatch://INFRA-3759). An idle architect,
// which is not its issue's open phase's worker and so is armed no backstop, is the sharpest case:
// its stop is recorded owing a launch, it takes a held wake of the pause, and its start row is
// queued and logged as a released held row, which the outbox runs at once through the pacing.
func TestAStopThatCommitsAfterItsPauseEndedOwesTheRoleALaunch(t *testing.T) {
	r := newPauseRig(t, claim.RoleArchitect)
	held := r.pauseLegion()
	r.resume()

	r.mustStop(r.claim(claim.RoleArchitect, supervise.StateSuspended), supervise.PauseStop{Pause: held.ID})

	if got := r.stops(held.ID, claim.RoleArchitect); len(got) != 1 || got[0] != (pauseStopRecord{end: store.PauseStopStopped, owed: true}) {
		t.Fatalf("the architect's stops = %+v, want its cancelled stop recorded stopped and owing a launch", got)
	}
	starts := r.starts(claim.RoleArchitect)
	if len(starts) != 1 || starts[0].wake != pods.WakePause || starts[0].pause != held.ID {
		t.Fatalf("the architect's starts = %+v, want one wake start of pause %d for the launch it is owed", starts, held.ID)
	}
	if all, open := r.heldWakes(held.ID, claim.RoleArchitect); all != 1 || open != 1 {
		t.Fatalf("the architect's held wakes = %d, %d open; want the one its stop took, open", all, open)
	}
	r.wantReleasedOnce(starts[0].id, held.ID)
}

// An operator's spawn — a claim of an issue the daemon keeps no record of — whose stop commits
// after its pause has ended, with no later pause open, is owed its launch as every such stop is,
// and no start row can carry it: the outbox's start reads the issue's record. So once the stop's
// transaction commits it is resumed by rule (supervise.RequestRuleResume), which the wake pacing
// paces as it paces every resume of the rule, and the claim runs again.
func TestAStopOfAnOperatorsSpawnAfterItsPauseEndedResumesItByRule(t *testing.T) {
	r := newPauseRig(t)
	sup, rt := r.supervisor()
	r.pauses.supervisor = sup
	const spawned = "LEGION-77"
	stopped := supervise.Claim{Token: mustClaimToken(t, spawned, claim.RoleArchitect), Project: "legion", Tree: spawned, Issue: spawned,
		Role: claim.RoleArchitect, Generation: 1, ServingGeneration: 1, State: supervise.StateSuspended,
		SuspendedBy: supervise.SuspendedByReleased, Session: "ses-operator", SessionFile: t.TempDir() + "/session.jsonl"}
	machine, _, err := sup.Create(r.ctx, stopped, "")
	if err != nil {
		t.Fatalf("create the operator's spawn: %v", err)
	}
	held := r.pauseLegion()
	r.resume()

	r.mustStop(stopped, supervise.PauseStop{Pause: held.ID})

	if !testwait.Holds(testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll, func() bool {
		return len(resumesOf(rt, stopped.Token)) == 1
	}) {
		c := machine.Claim()
		t.Fatalf("the operator's spawn is %s by %s after its stop committed once its pause had ended, resumed %d times; want it resumed by rule once",
			c.State, c.SuspendedBy, len(resumesOf(rt, stopped.Token)))
	}
	if got := r.count(`select count(*) from project_pause_stops where pause_id = $1 and claim = $2 and ended_as = 'stopped' and launch_owed`,
		held.ID, string(stopped.Token)); got != 1 {
		t.Fatalf("the spawn's stops recorded stopped owing a launch = %d, want 1", got)
	}
	if got := r.count(`select count(*) from outbox where issue = $1`, spawned); got != 0 {
		t.Fatalf("outbox rows of %s = %d, want none: no start row can carry the launch of an issue with no record", spawned, got)
	}
}

// The `pause_stop` executor ends the pause's stop row by what the claim answers: a row whose
// pause has ended finishes and asks nothing; a role with no machine ends `failed`; a queued claim,
// which runs nothing and launches nothing while the pause holds its start, ends `stopped` with no
// launch owed; a claim a park suspended ends `taken_over`; and a launch whose outcome is uncertain
// asks again after the bounded wait.
func TestAPauseStopEndsItsStopRowByWhatTheClaimAnswers(t *testing.T) {
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger}
	r := newPauseRig(t, roles...)
	held := r.pauseLegion()
	sup, _ := r.supervisor()
	for role, state := range map[claim.Role]supervise.ClaimState{claim.RoleImplementer: supervise.StateQueued,
		claim.RoleReviewer: supervise.StateSuspended, claim.RoleMerger: supervise.StateLaunchUncertain} {
		c := r.claim(role, state)
		if state == supervise.StateSuspended {
			c.SuspendedBy = supervise.SuspendedByDaemon
		}
		if _, _, err := sup.Create(r.ctx, c, ""); err != nil {
			t.Fatalf("create the %s's machine: %v", role, err)
		}
	}
	now := time.Now()
	runner := &outbox{pool: r.pool, records: record.NewStore(), supervisor: sup, trees: r.st, project: "legion",
		dispatchProject: "LEGION", log: quietLogger(), now: func() time.Time { return now }}
	stopOf := func(pause int64, role claim.Role) error {
		return runner.execute(r.ctx, mustOutboxRow(t, pauseIssue, record.SuperviseRequest{Op: "pause_stop", Tree: pauseIssue,
			Role: role, Generation: 1, Pause: pause, Reason: "project LEGION is paused"}, now))
	}

	for role, want := range map[claim.Role]store.PauseStopEnd{claim.RoleImplementer: store.PauseStopStopped, claim.RoleTester: store.PauseStopFailed,
		claim.RoleReviewer: store.PauseStopTakenOver} {
		if err := stopOf(held.ID, role); err != nil {
			t.Fatalf("the %s's stop: %v", role, err)
		}
		if got := r.stops(held.ID, role); len(got) != 1 || got[0] != (pauseStopRecord{end: want}) {
			t.Fatalf("the %s's stops = %+v, want one ended %s and owed no launch", role, got, want)
		}
	}
	err := stopOf(held.ID, claim.RoleMerger)
	if at, ok := wait.UntilAt(err); !errors.Is(err, wait.ErrWaiting) || !ok || !at.Equal(now.UTC().Add(pauseStopWait)) {
		t.Fatalf("the merger's stop = %v, want a wait until %s", err, now.UTC().Add(pauseStopWait))
	}
	if got := r.stops(held.ID, claim.RoleMerger); len(got) != 1 || got[0].end != "" {
		t.Fatalf("the merger's stops = %+v, want its stop still open", got)
	}

	ended := r.resume()
	if err := stopOf(ended.ID, claim.RoleMerger); err != nil {
		t.Fatalf("the merger's stop once the pause ended: %v", err)
	}
	if got := r.stops(held.ID, claim.RoleMerger); len(got) != 1 || got[0].end != store.PauseStopCancelled {
		t.Fatalf("the merger's stops after the resume = %+v, want the resume's cancel alone", got)
	}
	m, _ := sup.Machine(mustClaimToken(t, pauseIssue, claim.RoleMerger))
	if state := m.Claim().State; state != supervise.StateLaunchUncertain {
		t.Fatalf("the merger is %s after its ended pause's stop, want it untouched", state)
	}
}

// pauseReadThenResumed is the store as the `pause_stop` executor meets it when the resume commits
// right after the executor has read its pause open (PauseByID): resumed runs there, before the
// executor asks the claim's machine to stop.
type pauseReadThenResumed struct {
	outboxTrees
	resumed func()
}

func (s pauseReadThenResumed) PauseByID(ctx context.Context, id int64) (store.Pause, error) {
	held, err := s.outboxTrees.PauseByID(ctx, id)
	s.resumed()
	return held, err
}

// The resume can commit between the `pause_stop` executor's read of its pause, which finds it
// open, and the machine's decision of the stop: a row under a live lease outlives the resume's
// delete, the flag is cleared, and the resume's event reaches the machine first. The stop is then
// decided for a pause that has ended and stops nothing (supervise.ErrPauseEnded). A ready
// architect, which holds nothing a wake would bring back, is the sharpest case: stopped there, it
// would stay stopped after the resume until its next wake. No process is stopped, no stop
// transaction records it stopped, its stop row stays the one the resume cancelled, and the row
// finishes without acting, saying so as a row that reads its pause ended does.
func TestAPauseStopTheResumeOvertakesStopsNothing(t *testing.T) {
	r := newPauseRig(t, claim.RoleArchitect)
	sup, rt := r.supervisor()
	sup.deps.PauseStopped = r.pauses.stopped
	architect := launchedClaim(t, sup, pauseIssue, pauseIssue, claim.RoleArchitect)
	machine, _ := sup.Machine(architect)
	held := r.pauseLegion()
	logs := &testlog.Buffer{}
	runner := &outbox{pool: r.pool, records: record.NewStore(), supervisor: sup, project: "legion", dispatchProject: "LEGION",
		log: slog.New(slog.NewJSONHandler(logs, nil)), now: time.Now,
		trees: pauseReadThenResumed{outboxTrees: r.st, resumed: func() {
			ended := r.resume()
			if err := machine.Handle(r.ctx, supervise.ResumePause{Claim: architect, Pause: ended.ID}); err != nil {
				t.Errorf("the resume's event: %v", err)
			}
		}}}

	if err := runner.execute(r.ctx, mustOutboxRow(t, pauseIssue, record.SuperviseRequest{Op: "pause_stop", Tree: pauseIssue,
		Role: claim.RoleArchitect, Generation: 1, Pause: held.ID, Reason: "project LEGION is paused"}, time.Now())); err != nil {
		t.Fatalf("the stop the resume overtook = %v, want it finished without acting", err)
	}

	if c := machine.Claim(); c.State != supervise.StateReady {
		t.Fatalf("the architect is %s by %s after the stop the resume overtook, want it still ready", c.State, c.SuspendedBy)
	}
	if got := len(rt.CallsOf("Suspend")); got != 0 {
		t.Fatalf("the runtime was asked to stop %d processes, want none", got)
	}
	if got := r.stops(held.ID, claim.RoleArchitect); len(got) != 1 || got[0] != (pauseStopRecord{end: store.PauseStopCancelled}) {
		t.Fatalf("the architect's stops = %+v, want the one the resume cancelled, and none recorded stopped", got)
	}
	if got := strings.Count(logs.String(), `"msg":"outbox pause stop finished without acting: its pause has ended"`); got != 1 {
		t.Fatalf("the row's lines that it finished without acting = %d, want 1; log:\n%s", got, logs.String())
	}
}

// The resume wakes every lane at once (projectPause.Resumed), so a held notice of a role whose
// stop took the pause's held wake can run before that wake's start row. The notice finds its
// architect released, and the delivery wake's take is refused by the role's open resume, which is
// the held wake: the pause's owed launch, not a resume no process came behind. The daemon says so
// at Info, writes no Warn that the resume stands open with no process behind it, and resumes the
// role through the wake pacing; the held wake's start row, which runs next, finds the role
// running, and the role launches once.
func TestANoticeThatMeetsThePausesHeldWakeAtTheResumeWarnsOfNoStuckResume(t *testing.T) {
	const stuck = `"msg":"wake: a released pod's resume stands open with no process behind it; this wake resumes it"`
	const heldWake = `"msg":"wake: a released role's open resume is its pause's held wake; this wake resumes the role through the wake pacing"`
	r := newPauseRig(t, claim.RoleArchitect)
	sup, rt := r.supervisor()
	machine := releasedClaim(t, sup, claim.RoleArchitect)
	architect := machine.Claim().Token
	held := r.pauseLegion()
	r.mustStop(r.claim(claim.RoleArchitect, supervise.StateSuspended), supervise.PauseStop{Pause: held.ID, LaunchOwed: true})
	if _, open := r.heldWakes(held.ID, claim.RoleArchitect); open != 1 {
		t.Fatalf("the architect's open held wakes = %d, want the one its stop took for the launch it is owed", open)
	}
	if starts := r.starts(claim.RoleArchitect); len(starts) != 1 || starts[0].pause != held.ID {
		t.Fatalf("the architect's starts = %+v, want the held wake's start of pause %d", starts, held.ID)
	}
	ended := r.resume()
	if err := machine.Handle(r.ctx, supervise.ResumePause{Claim: architect, Pause: ended.ID}); err != nil {
		t.Fatalf("the resume's event: %v", err)
	}
	if got := machine.Claim(); !got.Released() {
		t.Fatalf("the architect is %s by %s after the resume's event, want it still released: its launch is the held wake's",
			got.State, got.SuspendedBy)
	}
	logs := &testlog.Buffer{}
	runner := &outbox{pool: r.pool, records: record.NewStore(), supervisor: sup, trees: r.st, project: "legion",
		dispatchProject: "LEGION", notices: &outboxPublisher{}, log: slog.New(slog.NewJSONHandler(logs, nil)), now: time.Now}

	notice := leasedOutboxRow(t, r.pool, runner.records, mustOutboxRow(t, pauseIssue,
		record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "review_round_cap"}, time.Now()))
	if err := runner.execute(r.ctx, notice); !errors.Is(err, errNoticeWaits) {
		t.Fatalf("the notice that met the held wake = %v, want it waiting for the architect it woke", err)
	}

	if lines := loggedLines(t, logs.String(), stuck); len(lines) != 0 {
		t.Fatalf("the wake that met the pause's held wake logged %v, want no Warn of a resume no process came behind", lines)
	}
	lines := loggedLines(t, logs.String(), heldWake)
	if len(lines) != 1 || lines[0]["level"] != "INFO" || int64(lines[0]["pause"].(float64)) != held.ID {
		t.Fatalf("the held wake's lines = %v, want one at INFO naming pause %d; log:\n%s", lines, held.ID, logs.String())
	}
	if got := machine.Claim(); got.Released() {
		t.Fatalf("the architect is %s by %s after the notice's wake, want it resumed", got.State, got.SuspendedBy)
	}
	if err := runner.RunOnce(r.ctx); err != nil {
		t.Fatalf("run the held wake's start: %v", err)
	}
	if starts := r.starts(claim.RoleArchitect); len(starts) != 0 {
		t.Fatalf("the architect's starts left = %+v, want the held wake's start run", starts)
	}
	if got := len(resumesOf(rt, architect)); got != 1 {
		t.Fatalf("the architect was resumed %d times, want once: the notice's wake and the held wake's start are one launch", got)
	}
}

// A wake's start finishes without acting once a park, a close or a moved phase took its role over
// while the row waited — the pause holds a wake's row for as long as it stands (decision 10): the
// claim the daemon suspended stays suspended by the daemon. The same row over a role still
// released resumes it, which is what tells the fence from a row that starts nothing anyway.
func TestAWakeStartOfARoleTheDaemonTookOverFinishesWithoutActing(t *testing.T) {
	ctx := context.Background()
	runner, sup, _ := hookRunner(t, &outboxPublisher{})
	parked := releasedClaim(t, sup, claim.RoleImplementer)
	if err := parked.Handle(ctx, supervise.RequestSuspend{Claim: parked.Claim().Token, By: supervise.SuspendedByDaemon,
		Reason: "the tree was parked"}); err != nil {
		t.Fatalf("park the implementer: %v", err)
	}
	released := releasedClaim(t, sup, claim.RoleTester)
	issue := record.Issue{Key: pauseIssue, Tree: pauseIssue, Generation: 1}
	wake := func(role claim.Role) record.OutboxRow {
		return mustOutboxRow(t, pauseIssue, workflow.PauseWakeRow(issue, role, 7), time.Now())
	}

	if err := runner.execute(ctx, wake(claim.RoleImplementer)); err != nil {
		t.Fatalf("the implementer's wake start: %v", err)
	}
	if got := parked.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByDaemon {
		t.Fatalf("the parked implementer is %s by %s after its wake start, want it left suspended by the daemon", got.State, got.SuspendedBy)
	}
	if err := runner.execute(ctx, wake(claim.RoleTester)); err != nil {
		t.Fatalf("the tester's wake start: %v", err)
	}
	if got := released.Claim(); got.State == supervise.StateSuspended {
		t.Fatalf("the released tester is %s by %s after its wake start, want it resumed", got.State, got.SuspendedBy)
	}
}

// A `suspend` row takes one role of a released pod over and drops that role's backstop alone
// (decision 17): another role's backstop is all that brings that role back. An issue's close
// still drops every backstop of the issue, since the issue's work is over.
func TestASuspendRowDropsOnlyTheBackstopOfTheRoleItStops(t *testing.T) {
	ctx := context.Background()
	runner, sup, pool := hookRunner(t, &outboxPublisher{})
	releasedClaim(t, sup, claim.RoleImplementer)
	releasedClaim(t, sup, claim.RoleArchitect)
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleArchitect} {
		if _, err := pool.Exec(ctx, `insert into pod_wake_backstops (project, issue, role, due_at)
			values ('legion', $1, $2, now() + interval '1 hour')`, pauseIssue, string(role)); err != nil {
			t.Fatalf("arm the %s's backstop: %v", role, err)
		}
	}
	armed := func() []string {
		t.Helper()
		rows, err := pool.Query(ctx, `select role from pod_wake_backstops where issue = $1 and not fired order by role`, pauseIssue)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var roles []string
		for rows.Next() {
			var role string
			if err := rows.Scan(&role); err != nil {
				t.Fatal(err)
			}
			roles = append(roles, role)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return roles
	}

	if err := runner.execute(ctx, mustOutboxRow(t, pauseIssue, record.SuperviseRequest{Op: "suspend", Tree: pauseIssue,
		Role: claim.RoleArchitect, Generation: 1, Reason: "the tree was parked"}, time.Now())); err != nil {
		t.Fatalf("the architect's suspend: %v", err)
	}
	if got := armed(); len(got) != 1 || got[0] != string(claim.RoleImplementer) {
		t.Fatalf("backstops armed after the architect's suspend = %v, want the implementer's alone", got)
	}
	if err := runner.execute(ctx, mustOutboxRow(t, pauseIssue, record.SuperviseRequest{Op: "issue_close", Tree: pauseIssue,
		Role: claim.RoleImplementer, Generation: 1}, time.Now())); err != nil {
		t.Fatalf("the implementer's issue close: %v", err)
	}
	if got := armed(); len(got) != 0 {
		t.Fatalf("backstops armed after the issue's close = %v, want none", got)
	}
}
