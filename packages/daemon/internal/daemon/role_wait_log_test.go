package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The lines a row that waits for its role writes (dispatch://INFRA-4131/spec#b-r-logged-once),
// as the daemon's log carries them.
const (
	wantWaitStartLine   = "outbox row waits"
	wantTookNothingLine = "outbox row's role is ready and took nothing for 30 minutes"
	wantWaitedLongLine  = "outbox row has waited 30 minutes"
	wantWaitEndedLine   = "outbox row's wait ended"
)

// roleWaitRuns is how many runs each test drives its row through while it waits: past the
// 30-minute line, which the capped backoff reaches at the row's fifteenth run (its runs at 0, 1, 3,
// 7, 15, 31, 63, 127, 255 and 511 s, then every 300 s, the fifteenth at 2011 s), and on to the
// cap's runs after it.
const roleWaitRuns = 20

// roleWaitLongAttempts is the attempts the 30-minute line names on that schedule: the row's
// fifteenth run, after fourteen.
const roleWaitLongAttempts = 14

// roleWaitRig drives one outbox row that waits for its role, one run at a time, each at the moment
// the row is due, on clock: the outbox's clock (outbox.now) and its claims' machines' clock
// (supervise.Deps.Clock), so a claim's ready is dated on the clock the row's wait is read against.
// logs holds every line the runner writes, Debug included.
type roleWaitRig struct {
	t      *testing.T
	runner *outbox
	pool   *store.Pool
	clock  *heldClock
	logs   *syncBuffer
	id     int64
	// runs is how many times the rig has run the row.
	runs int
}

// roleWaitClock is a clock held at the test's start, to the microsecond the store keeps a moment
// at, which the outbox and the claims' machines share.
func roleWaitClock() *heldClock {
	return &heldClock{at: time.Now().UTC().Truncate(time.Microsecond)}
}

// debugLogTo is a JSON logger to logs that writes Debug lines too.
func debugLogTo(logs *syncBuffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// newRoleWaitRig drives row id of runner, whose clock and log it takes over.
func newRoleWaitRig(t *testing.T, runner *outbox, pool *store.Pool, clock *heldClock, id int64) *roleWaitRig {
	t.Helper()
	logs := &syncBuffer{}
	runner.now, runner.log = clock.Now, debugLogTo(logs)
	return &roleWaitRig{t: t, runner: runner, pool: pool, clock: clock, logs: logs, id: id}
}

// queueRow writes row to the outbox and answers its id.
func queueRow(t *testing.T, pool *store.Pool, records record.Store, row record.OutboxRow) int64 {
	t.Helper()
	var id int64
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = records.EnqueueID(ctx, tx, row)
		return err
	}); err != nil {
		t.Fatalf("queue the row: %v", err)
	}
	return id
}

// roleWaitClaim creates c on sup, in project legion, and answers its machine.
func roleWaitClaim(t *testing.T, sup *supervisor, c supervise.Claim) *supervise.Machine {
	t.Helper()
	c.Project = "legion"
	machine, _, err := sup.Create(context.Background(), c, "")
	if err != nil {
		t.Fatalf("create the claim %s: %v", c.Token, err)
	}
	return machine
}

// state is the row's attempts and when it is due, and whether the outbox still holds it.
func (r *roleWaitRig) state() (attempts int, due time.Time, queued bool) {
	r.t.Helper()
	err := r.pool.QueryRow(context.Background(), "select attempts, next_at from outbox where id = $1", r.id).Scan(&attempts, &due)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, false
	}
	if err != nil {
		r.t.Fatalf("read row %d: %v", r.id, err)
	}
	return attempts, due, true
}

// run moves the clock to the moment the row is due and drains the outbox there, which runs the row
// once.
func (r *roleWaitRig) run() {
	r.t.Helper()
	attempts, due, queued := r.state()
	if !queued {
		r.t.Fatalf("row %d is no longer in the outbox, want it waiting", r.id)
	}
	if now := r.clock.Now(); due.After(now) {
		r.clock.advance(due.Sub(now))
	}
	if err := r.runner.RunOnce(context.Background()); err != nil {
		r.t.Fatalf("run row %d: %v", r.id, err)
	}
	r.runs++
	if after, _, still := r.state(); still && after != attempts+1 {
		r.t.Fatalf("the drain at row %d's moment left it at %d attempts, want %d: the row did not run", r.id, after, attempts+1)
	}
}

// runTo runs the row until it has waited through n runs.
func (r *roleWaitRig) runTo(n int) {
	r.t.Helper()
	for {
		attempts, _, queued := r.state()
		if !queued {
			r.t.Fatalf("row %d left the outbox after %d attempts, want it waiting through %d", r.id, attempts, n)
		}
		if attempts >= n {
			return
		}
		r.run()
	}
}

// endsAtItsNextRun runs the row once more and fails the test unless that run ended it.
func (r *roleWaitRig) endsAtItsNextRun() {
	r.t.Helper()
	r.run()
	if attempts, _, queued := r.state(); queued {
		r.t.Fatalf("row %d is still queued after %d attempts, want the run that was taken to end it", r.id, attempts)
	}
}

// waitLog is what a waiting row's lines say beside their messages: the level of its 30-minute
// line, the attempts that line names, what each wait line names the row waits for (key and value),
// whether they name the listener's answer, and how the row ended: "" while it still waits, "run"
// for a run that ended it, after the executor's own line saying how (ended, "" for none), and
// "deleted" for a delete that took it out of the outbox.
type waitLog struct {
	level    slog.Level
	attempts int
	waitsFor []any
	answer   bool
	// longAnswer overrides answer for the 30-minute line alone when the start run and the 30-minute
	// run differ in whether they published (for example, a registered not-ready claim publishes once
	// on its first run and then waits without publishing through the 30-minute line). nil uses answer.
	longAnswer *bool
	end        string
	ended      string
}

// rowLines is every line of logs that names row id: by its "row" field, or by its key where a line
// names the publish (record.OutboxKey).
func rowLines(t *testing.T, logs string, id int64) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range loggedLines(t, logs, `"msg"`) {
		if line["row"] == float64(id) || line["key"] == record.OutboxKey(id) {
			lines = append(lines, line)
		}
	}
	return lines
}

// wantWaitLogged fails the test unless the lines above Debug that name row id are its wait's
// alone, in order: one Info line when the wait started, one line at want.level when it had waited
// 30 minutes, and, where it ended, the executor's own line of how a run ended it and one Info line
// that the wait ended; and unless every other run the row waited through, of runs in all, wrote its
// wait at Debug. The wait lines name the row's issue and kind, what it waits for and, where there
// is one, the listener's answer; the 30-minute line names the attempts before it; the end names
// the attempts and how long the row waited.
func wantWaitLogged(t *testing.T, logs string, id int64, issue string, kind record.OutboxKind, runs int, want waitLog) {
	t.Helper()
	long := wantWaitedLongLine
	if want.level == slog.LevelError {
		long = wantTookNothingLine
	}
	wanted := []string{"INFO " + wantWaitStartLine, want.level.String() + " " + long}
	if want.ended != "" {
		wanted = append(wanted, "INFO "+want.ended)
	}
	if want.end != "" {
		wanted = append(wanted, "INFO "+wantWaitEndedLine)
	}
	var above []map[string]any
	var got []string
	debug := 0
	for _, line := range rowLines(t, logs, id) {
		if line["level"] == slog.LevelDebug.String() {
			if line["msg"] == wantWaitStartLine {
				debug++
			}
			continue
		}
		above = append(above, line)
		got = append(got, fmt.Sprint(line["level"], " ", line["msg"]))
	}
	if !slices.Equal(got, wanted) {
		t.Fatalf("the lines above Debug naming row %d = %q, want exactly %q:\n%s", id, got, wanted, logs)
	}
	for i, line := range above[:2] {
		if line["issue"] != issue || line["kind"] != string(kind) {
			t.Errorf("the wait line %v names issue %v and kind %v, want %s and %s", line, line["issue"], line["kind"], issue, kind)
		}
		for j := 0; j < len(want.waitsFor); j += 2 {
			if key := want.waitsFor[j].(string); line[key] != want.waitsFor[j+1] {
				t.Errorf("the wait line %v names %s %v, want %v", line, key, line[key], want.waitsFor[j+1])
			}
		}
		wantAns := want.answer
		if i == 1 && want.longAnswer != nil {
			wantAns = *want.longAnswer
		}
		if answer, _ := line["answer"].(string); (answer != "") != wantAns {
			t.Errorf("the wait line %v names the listener's answer %q, want one named: %t", line, answer, wantAns)
		}
	}
	if above[1]["attempts"] != float64(want.attempts) {
		t.Errorf("the 30-minute line names %v attempts, want %d", above[1]["attempts"], want.attempts)
	}
	if want.end != "" {
		end := above[len(above)-1]
		if _, named := end["attempts"].(float64); !named || end["waited"] == nil || end["endedBy"] == nil {
			t.Errorf("the end line %v names no attempts, no time waited or nothing that ended it", end)
		}
	}
	waited := runs
	if want.end == "run" {
		waited--
	}
	if debug != waited-2 {
		t.Errorf("the row wrote its wait at Debug on %d runs, want every one of its %d waiting runs but the first and the 30-minute one, %d:\n%s",
			debug, waited, waited-2, logs)
	}
}

// R4: a row whose role's claim said it was ready while the listener took nothing logs its wait
// once when it starts, once at Error when it has waited 30 minutes, and once when it ends, and
// every other run at Debug alone, over more than 20 runs on the capped backoff. The Error line is
// for a ready role that has had its chance: a turn started since its ready, or the 30 seconds a
// ready session may take to claim its role's name passed with no turn (roleNameBound). Each row is
// refused at every run until it has waited through twenty, then taken.
func TestRoleWaitLoggedOnceAtErrorForAReadyRoleThatTakesNothing(t *testing.T) {
	t.Parallel()
	t.Run("a role message refused after a turn since its role's ready", func(t *testing.T) {
		t.Parallel()
		clock := roleWaitClock()
		start := clock.Now()
		token := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
		publisher := &outboxPublisher{err: confirmedAnswer(notify.ReasonDeliveryFailed, "ses-implementer")}
		runner, sup, pool := runnerIn(t, phase.Implementing, "in_progress", publisher, quietLogger())
		sup.deps.Clock = clock
		roleWaitClaim(t, sup, supervise.Claim{Token: token, Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleImplementer,
			Generation: 1, State: supervise.StateWorking, Session: "ses-implementer", SessionFile: t.TempDir() + "/session.jsonl",
			LastReady: start.Add(-10 * time.Minute), LastTurnStart: start.Add(-5 * time.Minute)})
		id := queueRow(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", record.RoleMessage{Role: string(token), Body: "CI settled at head sha-1"}, start))
		rig := newRoleWaitRig(t, runner, pool, clock, id)

		rig.runTo(roleWaitRuns)
		publisher.refuseWith(nil)
		rig.endsAtItsNextRun()
		wantWaitLogged(t, rig.logs.String(), id, "LEGION-208", record.OutboxKindRoleMessage, rig.runs, waitLog{level: slog.LevelError,
			attempts: roleWaitLongAttempts, waitsFor: []any{"role", string(token)}, answer: true, end: "run"})
	})
	t.Run("a merge forward refused while its role is ready with no turn, past the bound for its name", func(t *testing.T) {
		t.Parallel()
		clock := roleWaitClock()
		start := clock.Now()
		token := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
		ask := record.MergeForwardMessage{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: "sha-1", Base: "main",
			Phase: phase.Testing, PullRequest: 86, Why: record.ConflictCheckNotGreen}
		publisher := &holderPublisher{}
		publisher.setAbsent(notify.RoleTopicPrefix + string(token))
		runner, sup, pool, _ := mergeForwardRunner(t, publisher, &record.MergeForward{Head: "sha-1", Base: "main", Phase: phase.Testing,
			Why: record.ConflictCheckNotGreen, AskedAt: start})
		sup.deps.Clock = clock
		roleWaitClaim(t, sup, supervise.Claim{Token: token, Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleImplementer,
			Generation: 1, State: supervise.StateReady, Session: "ses-implementer", SessionFile: t.TempDir() + "/session.jsonl",
			LastReady: start.Add(-roleNameBound - time.Second)})
		id := queueRow(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", ask, start))
		rig := newRoleWaitRig(t, runner, pool, clock, id)

		rig.runTo(roleWaitRuns)
		publisher.setAbsent()
		publisher.holdBy(func() string { return "ses-implementer" })
		rig.endsAtItsNextRun()
		wantWaitLogged(t, rig.logs.String(), id, "LEGION-208", record.OutboxKindMergeForward, rig.runs, waitLog{level: slog.LevelError,
			attempts: roleWaitLongAttempts, waitsFor: []any{"role", string(token)}, answer: true, end: "run",
			ended: "outbox merge-forward message delivered"})
	})
	// A notice the listener could not forward to its architect's session (delivery_failed) and one
	// that session did not confirm (receipt_timeout) each keep their row, which waits for its ready
	// architect (dispatch://INFRA-4131/spec#b-r-never-given-up).
	for _, answer := range []struct{ name, reason string }{
		{"a notice refused delivery_failed while its architect is ready", notify.ReasonDeliveryFailed},
		{"a notice answered receipt_timeout at each publish while its architect is ready", notify.ReasonReceiptTimeout},
	} {
		t.Run(answer.name, func(t *testing.T) {
			t.Parallel()
			clock := roleWaitClock()
			start := clock.Now()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			sup.deps.Clock = clock
			architect := mustClaimToken(t, "LEGION-1", claim.RoleArchitect)
			roleWaitClaim(t, sup, supervise.Claim{Token: architect, Tree: "LEGION-1", Issue: "LEGION-1", Role: claim.RoleArchitect,
				Generation: 1, State: supervise.StateWorking, Session: "ses_architect", SessionFile: t.TempDir() + "/session.jsonl",
				LastReady: start.Add(-10 * time.Minute), LastTurnStart: start.Add(-5 * time.Minute)})
			publisher := &outboxPublisher{err: confirmedAnswer(answer.reason, "ses_architect")}
			runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion"}
			id := queueRow(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect,
				Reason: "review_round_cap"}, start))
			rig := newRoleWaitRig(t, runner, pool, clock, id)

			rig.runTo(roleWaitRuns)
			publisher.refuseWith(nil)
			rig.endsAtItsNextRun()
			wantWaitLogged(t, rig.logs.String(), id, "LEGION-2", record.OutboxKindNotice, rig.runs, waitLog{level: slog.LevelError,
				attempts: roleWaitLongAttempts, waitsFor: []any{"role", string(architect)}, answer: true, end: "run",
				ended: "outbox notice published"})
		})
	}
}

// R4: a row whose wait is no fault of a ready role logs its wait once when it starts, once at WARN
// when it has waited 30 minutes, and once when it ends, and every other run at Debug alone, over
// more than 20 runs on the capped backoff. Three such waits: a role whose claim has not said it is
// ready (R1); a ready session whose 30 seconds to claim its role's name (roleNameBound) have not
// passed at the run that writes the 30-minute line, which is the name not taken yet rather than a
// role taking nothing; and the controller's role topic under `controller: operator`, which no claim
// of this daemon holds, so no ready of the daemon's record can say its role took nothing. Each row
// is refused at every run until it has waited through twenty, then taken.
func TestRoleWaitLoggedOnceAtWarnForAClaimThatIsNotReady(t *testing.T) {
	t.Parallel()
	t.Run("a merge forward to a role whose claim is not ready", func(t *testing.T) {
		t.Parallel()
		clock := roleWaitClock()
		start := clock.Now()
		token := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
		ask := record.MergeForwardMessage{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: "sha-1", Base: "main",
			Phase: phase.Testing, PullRequest: 86, Why: record.ConflictCheckNotGreen}
		publisher := &holderPublisher{}
		publisher.setAbsent(notify.RoleTopicPrefix + string(token))
		runner, sup, pool, _ := mergeForwardRunner(t, publisher, &record.MergeForward{Head: "sha-1", Base: "main", Phase: phase.Testing,
			Why: record.ConflictCheckNotGreen, AskedAt: start})
		sup.deps.Clock = clock
		machine := roleWaitClaim(t, sup, supervise.Claim{Token: token, Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleImplementer,
			Generation: 1, State: supervise.StateRegistered, Session: "ses-implementer", SessionFile: t.TempDir() + "/session.jsonl"})
		id := queueRow(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", ask, start))
		rig := newRoleWaitRig(t, runner, pool, clock, id)

		rig.runTo(roleWaitRuns)
		// The first run published once to generation 1; runs 2..20 waited at the gate without
		// publishing. Bringing the claim ready lets the next run publish and end the row.
		if err := machine.Handle(context.Background(), supervise.RequestReady{Claim: token, Generation: 1, Session: "ses-implementer"}); err != nil {
			t.Fatalf("the implementer's ready: %v", err)
		}
		machine.Wait()
		publisher.setAbsent()
		publisher.holdBy(func() string { return "ses-implementer" })
		rig.endsAtItsNextRun()
		noAnswer := false
		wantWaitLogged(t, rig.logs.String(), id, "LEGION-208", record.OutboxKindMergeForward, rig.runs, waitLog{level: slog.LevelWarn,
			attempts: roleWaitLongAttempts, waitsFor: []any{"role", string(token)}, answer: true, longAnswer: &noAnswer, end: "run",
			ended: "outbox merge-forward message delivered"})
	})
	t.Run("a notice refused inside the ready session's 30 seconds to claim its role's name", func(t *testing.T) {
		t.Parallel()
		clock := roleWaitClock()
		start := clock.Now()
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		noticeTree(t, pool, records, false)
		sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
		sup.deps.Clock = clock
		architect := mustClaimToken(t, "LEGION-1", claim.RoleArchitect)
		machine := roleWaitClaim(t, sup, supervise.Claim{Token: architect, Tree: "LEGION-1", Issue: "LEGION-1", Role: claim.RoleArchitect,
			Generation: 1, State: supervise.StateRegistered, Session: "ses_architect", SessionFile: t.TempDir() + "/session.jsonl"})
		publisher := &holderPublisher{}
		publisher.setAbsent(architectTopic(t, "LEGION-1"))
		runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion"}
		id := queueRow(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect,
			Reason: "review_round_cap"}, start))
		rig := newRoleWaitRig(t, runner, pool, clock, id)

		// The architect says it is ready ten seconds before the row's run at 30 minutes, and starts
		// no turn: that run's refusal is inside the 30 seconds its session has to claim the role's
		// name, and the row runs next at their end.
		rig.runTo(roleWaitLongAttempts)
		_, due, _ := rig.state()
		clock.advance(due.Add(-10 * time.Second).Sub(clock.Now()))
		if err := machine.Handle(context.Background(), supervise.RequestReady{Claim: architect, Generation: 1, Session: "ses_architect"}); err != nil {
			t.Fatalf("the architect's ready: %v", err)
		}
		machine.Wait()
		ready := machine.View().LastReady
		if !ready.Equal(clock.Now()) {
			t.Fatalf("the architect's ready is dated %s, want the rig's clock, %s", ready, clock.Now())
		}
		rig.run()
		if _, next, _ := rig.state(); !next.Equal(ready.Add(roleNameBound)) {
			t.Fatalf("after the refusal inside the bound the row is due at %s, want %s, the bound's end after the ready", next, ready.Add(roleNameBound))
		}
		rig.runTo(roleWaitRuns)
		publisher.setAbsent()
		rig.endsAtItsNextRun()
		wantWaitLogged(t, rig.logs.String(), id, "LEGION-2", record.OutboxKindNotice, rig.runs, waitLog{level: slog.LevelWarn,
			attempts: roleWaitLongAttempts, waitsFor: []any{"role", string(architect)}, answer: true, end: "run",
			ended: "outbox notice published"})
	})
	t.Run("a controller notice under controller: operator whose publish the listener refuses", func(t *testing.T) {
		t.Parallel()
		clock := roleWaitClock()
		start := clock.Now()
		denied := fmt.Errorf("publish notice: listener returned 403 Forbidden: {\"error\":\"envoy: the message bus denied the publish\"}: %w",
			notify.ErrPublishDenied{Message: "envoy: the message bus denied the publish under this listener's own permissions"})
		// The registered controller session holds the role live at every read before the publish
		// (controllerHolds), so each run reaches the publish the listener refuses.
		publisher := &outboxPublisher{err: denied}
		runner, _, pool := runnerIn(t, phase.Implementing, "in_progress", publisher, quietLogger())
		registerProjectControllerAs(t, pool.Config().ConnString(), "LEGION", "ses_controller")
		publisher.holds(legionController, "ses_controller")
		id := queueRow(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", record.ControllerNotice{Kind: "held", Role: claim.RolePlanner,
			Phase: phase.Planning}, start))
		rig := newRoleWaitRig(t, runner, pool, clock, id)

		rig.runTo(roleWaitRuns)
		publisher.refuseWith(nil)
		rig.endsAtItsNextRun()
		wantWaitLogged(t, rig.logs.String(), id, "LEGION-208", record.OutboxKindControllerNotice, rig.runs, waitLog{level: slog.LevelWarn,
			attempts: roleWaitLongAttempts, waitsFor: []any{"topic", controllerRoleTopic}, answer: true, end: "run"})
	})
}

// R4 and S15: the wait's start, its step and its 30-minute line are the row's own, so a daemon
// restarted after the 30-minute line writes neither the start nor that line again: a new outbox
// over the same store runs the row on at the backoff's cap and writes Debug alone until the row is
// taken, and then its one end line.
func TestRoleWaitLoggedOnceAcrossARestart(t *testing.T) {
	t.Parallel()
	clock := roleWaitClock()
	start := clock.Now()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Clock = clock
	architect := mustClaimToken(t, "LEGION-1", claim.RoleArchitect)
	roleWaitClaim(t, sup, supervise.Claim{Token: architect, Tree: "LEGION-1", Issue: "LEGION-1", Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateWorking, Session: "ses_architect", SessionFile: t.TempDir() + "/session.jsonl",
		LastReady: start.Add(-10 * time.Minute), LastTurnStart: start.Add(-5 * time.Minute)})
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-1"))
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion"}
	id := queueRow(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect,
		Reason: "review_round_cap"}, start))
	rig := newRoleWaitRig(t, runner, pool, clock, id)

	rig.runTo(roleWaitLongAttempts + 1)
	before := rig.logs.String()
	if lines := loggedMessages(t, before, wantTookNothingLine); len(lines) != 1 {
		t.Fatalf("before the restart the row wrote %d 30-minute lines, want one:\n%s", len(lines), before)
	}

	// The restart: a new outbox over the same store and claims, with a log of its own.
	restarted := &outbox{pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion"}
	again := newRoleWaitRig(t, restarted, pool, clock, id)
	again.run()
	if _, next, _ := again.state(); !next.Equal(clock.Now().Add(record.RoleWaitBackoffMost)) {
		t.Fatalf("the restarted daemon's first run made the row due at %s, want %s: the backoff's step is the row's, at its cap",
			next, clock.Now().Add(record.RoleWaitBackoffMost))
	}
	again.runTo(roleWaitRuns)
	publisher.setAbsent()
	again.endsAtItsNextRun()
	after := again.logs.String()
	for _, msg := range []string{wantTookNothingLine, wantWaitedLongLine} {
		if lines := loggedMessages(t, after, msg); len(lines) != 0 {
			t.Fatalf("the restarted daemon wrote %q again: %v", msg, lines)
		}
	}
	wantWaitLogged(t, before+after, id, "LEGION-2", record.OutboxKindNotice, rig.runs+again.runs, waitLog{level: slog.LevelError,
		attempts: roleWaitLongAttempts, waitsFor: []any{"role", string(architect)}, answer: true, end: "run",
		ended: "outbox notice published"})
}

// R4: a row another transaction deletes while it waits ends its wait there, with no run of its own,
// so that transaction writes the wait's end line once it commits: a newer catch-up's drop of the
// issue's catch-ups (DropCatchUps), the clear of a changes request's generation (DropQueuedChanges),
// and the cancel of a release that deletes a wake's start (store.CancelIssueRelease). The two
// notices wait through twenty runs refused by the listener before the delete. No executor of a
// start returns an event wait until the daemon's own events are waited for
// (dispatch://INFRA-4131/spec#b-r-wait-event), so the wake's start is given its wait in the store.
func TestRoleWaitLoggedEndOfARowAnotherTransactionDeletes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		queue func(t *testing.T, pool *store.Pool, records record.Store, at time.Time) int64
		drop  func(ctx context.Context, tx pgx.Tx, records projectRecords, id int64, now time.Time) error
	}{
		{name: "a catch-up DropCatchUps deletes",
			queue: func(t *testing.T, pool *store.Pool, records record.Store, at time.Time) int64 {
				return pacedCatchUp(t, pool, records, "LEGION-1", 1, at, at)
			},
			drop: func(ctx context.Context, tx pgx.Tx, records projectRecords, _ int64, now time.Time) error {
				_, err := records.DropCatchUps(ctx, tx, "LEGION-1", now)
				return err
			}},
		{name: "a queued changes-request notice DropQueuedChanges deletes",
			queue: func(t *testing.T, pool *store.Pool, records record.Store, at time.Time) int64 {
				return queueRow(t, pool, records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "design-changes-requested", Version: 3,
					Reason: "name the rollback path", Actor: "user:alice", Ref: "dispatch://LEGION-1/spec",
					Review: record.EventMark{Seq: 8, At: at}, Artifact: "artifact-1"}, at))
			},
			drop: func(ctx context.Context, tx pgx.Tx, records projectRecords, id int64, _ time.Time) error {
				_, err := records.DropQueuedChanges(ctx, tx, []int64{id})
				return err
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := roleWaitClock()
			start := clock.Now()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			sup.deps.Clock = clock
			architect := mustClaimToken(t, "LEGION-1", claim.RoleArchitect)
			roleWaitClaim(t, sup, supervise.Claim{Token: architect, Tree: "LEGION-1", Issue: "LEGION-1", Role: claim.RoleArchitect,
				Generation: 1, State: supervise.StateWorking, Session: "ses_architect", SessionFile: t.TempDir() + "/session.jsonl",
				LastReady: start.Add(-10 * time.Minute), LastTurnStart: start.Add(-5 * time.Minute)})
			publisher := &holderPublisher{}
			publisher.setAbsent(architectTopic(t, "LEGION-1"))
			runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion"}
			id := tc.queue(t, pool, records, start)
			rig := newRoleWaitRig(t, runner, pool, clock, id)

			rig.runTo(roleWaitRuns)
			deleter := projectRecords{Store: records, project: "LEGION", log: rig.runner.log}
			if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
				return tc.drop(ctx, tx, deleter, id, clock.Now())
			}); err != nil {
				t.Fatalf("delete the waiting row: %v", err)
			}
			if _, _, queued := rig.state(); queued {
				t.Fatalf("row %d is still queued after its delete", id)
			}
			wantWaitLogged(t, rig.logs.String(), id, "LEGION-1", record.OutboxKindNotice, rig.runs, waitLog{level: slog.LevelError,
				attempts: roleWaitLongAttempts, waitsFor: []any{"role", string(architect)}, answer: true, end: "deleted"})
		})
	}
	t.Run("a wake's start the release cancel deletes", func(t *testing.T) {
		t.Parallel()
		rig := newIdleRig(t)
		ctx := context.Background()
		rig.w.releaseIdlePod(ctx, rig.issue.Key)
		holdTeardownBack(t, rig)
		rig.drain(t)
		if got := rig.machine.Claim(); !got.Released() || !rig.marked(t) {
			t.Fatalf("the claim = %s by %s and the mark stands = %t, want it released under a release still running",
				got.State, got.SuspendedBy, rig.marked(t))
		}
		// A settlement's wake of the implementer while the release stands, whose start has waited
		// 45 minutes over 24 attempts.
		if taken, err := rig.st.TakePodWake(ctx, "legion", rig.issue.Key, rig.token, pods.WakeChecks, time.Now().UTC(), 0); err != nil || !taken {
			t.Fatalf("take the settlement's wake = %t, %v; want it taken", taken, err)
		}
		wake := mustOutboxRow(t, rig.issue.Key, record.SuperviseRequest{Op: "start", Tree: rig.issue.Tree, Role: claim.RoleImplementer,
			Generation: 1, Task: workflow.WakeTask(pods.WakeChecks, "head1 failed build", workflow.StoppedByRelease),
			WakeKind: pods.WakeChecks, WakeDetail: "head1 failed build"}, time.Now().UTC())
		var id int64
		if err := rig.pool.QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token, wait_since)
			values ($1, $2, $3, 24, now(), '', '', now() - interval '45 minutes') returning id`,
			string(wake.Kind), rig.issue.Key, string(wake.Payload)).Scan(&id); err != nil {
			t.Fatalf("queue the wake's start: %v", err)
		}
		logs := &syncBuffer{}
		rig.runner.log = debugLogTo(logs)

		// The issue's run moves on, so the teardown cancels the release, which deletes the wake's
		// start with the wake.
		if _, err := rig.pool.Exec(ctx, `update issues set generation = 2 where key = $1`, rig.issue.Key); err != nil {
			t.Fatalf("move the issue's generation on: %v", err)
		}
		var teardown record.OutboxRow
		if err := rig.pool.QueryRow(ctx, `select id, kind, issue, payload from outbox where issue = $1 and kind = 'issue_release'`,
			rig.issue.Key).Scan(&teardown.ID, &teardown.Kind, &teardown.Issue, &teardown.Payload); err != nil {
			t.Fatalf("read the teardown row: %v", err)
		}
		if err := rig.runner.execute(ctx, teardown); err != nil {
			t.Fatalf("run the teardown: %v", err)
		}
		var left int
		if err := rig.pool.QueryRow(ctx, `select count(*) from outbox where id = $1`, id).Scan(&left); err != nil || left != 0 {
			t.Fatalf("the wake's start is queued %d times (%v) after the cancel, want it deleted", left, err)
		}
		var got []string
		var ended map[string]any
		for _, line := range rowLines(t, logs.String(), id) {
			if line["level"] != slog.LevelDebug.String() {
				got = append(got, fmt.Sprint(line["level"], " ", line["msg"]))
				ended = line
			}
		}
		if want := []string{"INFO " + wantWaitEndedLine}; !slices.Equal(got, want) {
			t.Fatalf("the lines above Debug naming the deleted start = %q, want exactly %q:\n%s", got, want, logs.String())
		}
		if ended["attempts"] != float64(24) || ended["kind"] != string(record.OutboxKindSupervise) || ended["issue"] != rig.issue.Key ||
			ended["waited"] == nil || ended["endedBy"] == nil {
			t.Fatalf("the end line %v, want it to name the start's 24 attempts, its kind and issue, how long it waited and what ended it", ended)
		}
	})
}
