package daemon

// The silence sweep: a phase worker whose process is up and between turns, which holds no task
// and whose agent has not stepped for longer than `worker_silence_minutes`, is one nothing will
// prompt, and the daemon sends its phase's task again and tells its architect. These tests drive
// the real supervision through the pod-replacement rig (podupdate_test.go) — real machines and a
// real workflow engine over a real Postgres, with the runtime faked — because the worker this
// sweep is for is most often one whose pod was replaced: the daemon sends a phase task as one
// outbox row, the claim machine retires it at the end of the first turn that confirmed it, and
// what carries the worker through the rest of its phase is a timer inside its own process, which
// dies with that process (dispatch://INFRA-3163).
//
// One sweep is one call of tellSilentPhaseWorkers over the claims the store holds now, as the
// orphan sweep's tick makes it (sweepForSilence). What the sweep writes is a fact, and what the
// fact writes is a start row and a notice; the rig's own outbox runner then runs those rows, so
// the prompt a test reads at the worker's connection is the one the daemon's lifecycle lane
// really sends.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// silenceAssignment is what the issue's phase row already names for the role that works its
// phase: the assignment the worker was last started under, and the one the repair hands it again
// rather than minting a second (workflow's ResumeSlottedWorker).
const silenceAssignment = "A1"

// silentWorker is the worker these tests are about: a registered worker of the issue's pod, on
// this daemon's contract, that has retired a confirmed task of the issue's one run — which is
// every phase worker that has been given its phase's work and taken it.
func silentWorker(role claim.Role) podUpdateRole {
	worker := readyWorker(role)
	worker.serving = 1
	return worker
}

// assigns records the phase row of role with the assignment the issue's phase already gave it,
// as the start that put the worker to work wrote it (workflow's assign). The repair reads that
// row and nothing else to decide whether it can send a task at all.
func (r *podUpdateRig) assigns(role claim.Role, assignment string) {
	r.t.Helper()
	token := r.tokens[role]
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: podUpdateIssue, Role: role, Claim: token, Assignment: assignment})
	}); err != nil {
		r.t.Fatalf("record the phase row of the %s: %v", role, err)
	}
}

// stepped records a step of role's agent at the clock's moment, straight through the store's
// narrow call, which is where the daemon's own step writer lands (supervise's putStep). It is
// how a claim no turn can be driven into — one suspended, failed or retired — is given the one
// thing the sweep judges it by, so a case that tests another term of the rule differs from the
// silent worker in that term alone.
func (r *podUpdateRig) stepped(role claim.Role) {
	r.t.Helper()
	known, err := r.st.PutClaimStep(r.ctx, r.tokens[role], supervise.StepRecord{TurnStartedAt: r.clock.Now()})
	if err != nil || !known {
		r.t.Fatalf("record a step of the %s: known %v, %v", role, known, err)
	}
}

// lastStep is the moment role's claim last moved, as the store holds it and as the sweep reads
// it (supervise.StepRecord.LastStep). It is read from the store and not from the machine: the
// step record is no part of a machine's state, and a claim the machine hands out carries the
// record as it stood when that machine was built (supervise's putStep).
func (r *podUpdateRig) lastStep(role claim.Role) time.Time {
	r.t.Helper()
	step, err := r.sup.Step(r.ctx, r.tokens[role])
	if err != nil {
		r.t.Fatalf("read the step record of the %s: %v", role, err)
	}
	return step.LastStep()
}

// goesQuiet is the worker taking one turn and falling silent: its turn starts, which is the
// moment the record judges it from, the turn ends, and the clock moves past the bound. The
// moment is the turn's own start because a claim whose agent sends no tool frame is judged from
// that (supervise.StepRecord.LastStep).
func (r *podUpdateRig) goesQuiet(role claim.Role) {
	r.t.Helper()
	token := r.tokens[role]
	r.handle(role, supervise.StreamTurnStart{Claim: token})
	r.handle(role, supervise.StreamTurnEnd{Claim: token})
	r.awaitClaim(role, "its turn to end", func(c supervise.Claim) bool { return c.State != supervise.StateWorking })
	r.clock.advance(podUpdateSilence + time.Minute)
}

// sweepForSilence is one silence sweep over the claims the store holds now, exactly as the
// orphan sweep's tick makes it (reconcileOrphans).
func (r *podUpdateRig) sweepForSilence() {
	r.t.Helper()
	claims, err := r.sup.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the claims for the sweep: %v", err)
	}
	r.s.tellSilentPhaseWorkers(r.ctx, claims)
}

// appliesASilenceOf puts one intake.WorkerSilent to the workflow naming role's claim as it
// stands now: the run its worker is on, the moment of its last step as the store holds it, and
// the bound this deployment gives a phase worker. It is how a test here reads the rule's own
// silence term, which is the handler's to judge (workflow.(*Engine).workerSilent): the sweep
// judges that term itself before it mints anything (tellSilentPhaseWorkers), so every fact a
// sweep of this rig can write already carries a moment past the bound, and only a fact written
// here can carry a worker that has just stepped.
func (r *podUpdateRig) appliesASilenceOf(role claim.Role) {
	r.t.Helper()
	c := r.claimOf(role)
	last := r.lastStep(role)
	if err := r.s.applySweepFacts(r.ctx, []sweptFact{{
		ID: workerSilentEventID(c.Token, c.Generation, last),
		Fact: intake.WorkerSilent{Issue: podUpdateIssue, Role: role, Generation: c.ServingRun(),
			LastStep: last, Bound: podUpdateSilence},
	}}); err != nil {
		r.t.Fatalf("apply the silence of the %s: %v", role, err)
	}
}

// issue is the issue as the record holds it now, which is what the task the repair writes is
// built from (workflow.ResumePhaseTask).
func (r *podUpdateRig) issue() record.Issue {
	r.t.Helper()
	var issue *record.Issue
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		issue, err = r.records.Issue(r.ctx, tx, podUpdateIssue)
		return err
	}); err != nil || issue == nil {
		r.t.Fatalf("read the record of %s: %+v, %v", podUpdateIssue, issue, err)
	}
	return *issue
}

// starts is every supervise row of the issue, oldest first, as its payload holds it. It reads
// every operation and not starts alone, so a case that asserts the sweep wrote nothing is held
// to the whole table rather than to one kind of row in it.
func (r *podUpdateRig) starts() []record.SuperviseRequest {
	r.t.Helper()
	var requests []record.SuperviseRequest
	for _, row := range r.rows(record.OutboxKindSupervise) {
		var request record.SuperviseRequest
		if err := json.Unmarshal(row.Payload, &request); err != nil {
			r.t.Fatalf("decode supervise row %d: %v", row.ID, err)
		}
		requests = append(requests, request)
	}
	return requests
}

// silenceNotices is every notice of the issue the outbox holds, oldest first, whatever its kind:
// a test that wants the one notice a silence writes is held to the notices beside it too.
func (r *podUpdateRig) silenceNotices() []record.Notice {
	r.t.Helper()
	var notices []record.Notice
	for _, row := range r.rows(record.OutboxKindNotice) {
		var notice record.Notice
		if err := json.Unmarshal(row.Payload, &notice); err != nil {
			r.t.Fatalf("decode notice row %d: %v", row.ID, err)
		}
		if row.Issue != podUpdateIssue {
			r.t.Fatalf("notice %d is on %s, want %s", row.ID, row.Issue, podUpdateIssue)
		}
		notices = append(notices, notice)
	}
	return notices
}

// drain runs the rows the silence fact wrote, on the outbox over the rig's own Postgres,
// supervisor and store, and answers what they published: the start row goes through the
// worker's machine, which is how its task reaches its connection, and the notice row is routed
// to the issue's architect and published. The runner's clock is the rig's, so a row the engine
// wrote at the moment the sweep ran is due.
//
// It runs six passes, as the idle rig's drain does, which is more than a silence's two rows
// need: RunOnce lists what is due once, so a row that another row's finish makes claimable runs
// on the next pass rather than that one.
func (r *podUpdateRig) drain() *outboxPublisher {
	r.t.Helper()
	publisher := &outboxPublisher{}
	runner := &outbox{pool: r.pool, records: r.records, project: podUpdateProject, dispatchProject: "LEGION",
		supervisor: r.sup, trees: r.st, notices: publisher, log: quietLogger(), now: r.clock.Now}
	for range 6 {
		if err := runner.RunOnce(r.ctx); err != nil {
			r.t.Fatalf("run the outbox: %v", err)
		}
	}
	return publisher
}

// aSilentPhaseWorker is the fixture every case below starts from: the issue's implementer, which
// works the phase the issue stands in, with the assignment its phase row already names. beside
// is every other role claim of the same issue the case holds, which is how a case stands a
// worker the sweep must leave alone next to the one it must find.
func aSilentPhaseWorker(t *testing.T, beside ...podUpdateRole) *podUpdateRig {
	t.Helper()
	rig := newPodUpdateRig(t, append([]podUpdateRole{silentWorker(claim.RoleImplementer)}, beside...)...)
	rig.assigns(claim.RoleImplementer, silenceAssignment)
	return rig
}

// assertPromptedWithItsPhasesTask waits for the task the repair sent to reach the worker's
// connection and holds it to the task the record builds for the issue, with the assignment the
// phase row names on the row that carried it.
func assertPromptedWithItsPhasesTask(t *testing.T, rig *podUpdateRig, conn *fake.Conn, before int) {
	t.Helper()
	want := workflow.ResumePhaseTask(rig.issue())
	testwait.Eventually(t, "the silent worker to be prompted with its phase's task", func() bool {
		return len(conn.Prompts()) > before
	})
	sent := conn.Prompts()[before:]
	if len(sent) != 1 || sent[0].Message != want {
		t.Fatalf("the silent worker was sent %+v, want the one prompt %q", sent, want)
	}
}

// takesRelaunchSentence is role's replacement, relaunched holding nothing, reading the daemon's
// own delivery of the sentence of what its pod's end took (dispatch://INFRA-3276 R3), which its
// ready sent it at once, and saying says: Oh My Pi's acknowledgement, the turn it starts, its end,
// and the prompt's result behind it. That turn is all the delivery asks of it, and its result
// retires the delivery, so the worker holds nothing again and is the one nothing will prompt.
func (r *podUpdateRig) takesRelaunchSentence(role claim.Role, says string) {
	r.t.Helper()
	token, conn := r.tokens[role], r.workers[role]
	testwait.Eventually(r.t, "the relaunched "+string(role)+" to be sent the relaunch's sentence", func() bool {
		return len(conn.Prompts()) > 0
	})
	prompts := conn.Prompts()
	sent := prompts[len(prompts)-1]
	if held := r.claimOf(role).Pending; held == nil || !held.Relaunch() || held.ID != sent.DeliveryID || !strings.Contains(sent.Message, says) {
		r.t.Fatalf("the relaunched %s holds %+v and was sent %+v, want the relaunch's sentence saying %q", role, held, sent, says)
	}
	r.awaitClaim(role, "the relaunch's sentence to be acknowledged", func(c supervise.Claim) bool {
		return c.Pending != nil && !c.Pending.DeliveredAt.IsZero()
	})
	r.handle(role, supervise.StreamTurnStart{Claim: token})
	r.handle(role, supervise.StreamTurnEnd{Claim: token})
	r.handle(role, supervise.StreamPromptResult{Claim: token, DeliveryID: sent.DeliveryID, AgentInvoked: true})
	r.awaitClaim(role, "the sentence's turn to retire its delivery", func(c supervise.Claim) bool {
		return c.Pending == nil && c.State == supervise.StateIdle && c.EndedWork.Told
	})
}

// A phase worker whose pod was replaced comes back with its session and nothing to do: the round
// that replaced the pod retired every task it held, the pod round is over, and no timer of the
// daemon's and none of the worker's own process will prompt it again. The sweep is what finds
// it, and every link of the chain is asserted here, not only the last: the fact writes the start
// row, the lifecycle lane carries that row's task to the worker's connection, and the turn that
// task starts moves the claim from ready to working. The other half of that truth — that the
// end of the round itself prompts nobody — is
// TestPodUpdateTellsItsWorkerNothingHoweverTheRoundEnds.
//
// Beside the silent worker stands one the sweep must leave alone: the issue's tester, which
// went through the same round and stepped inside the bound, so it is a worker of the same pod,
// up, between turns and holding nothing, differing from the silent one in the silence itself
// and in the phase its role works. It is prompted nothing, which is what holds the sweep to the
// one claim it is owed: a pass that read a pod and prompted the workers in it — for the drift
// that pod carried, or for the silence of the one worker beside them — would prompt this one
// too, and its architect would get a task for work no worker of its phase is doing.
//
// The silence itself is read at the end, put to the one claim of this issue it can decide,
// which is the issue's own phase worker. The sweep judges the silence before it mints anything
// (tellSilentPhaseWorkers), so no fact a sweep writes can name a worker that is not silent; the
// handler re-reads the record and asks the rule again with the moment the fact carries, and
// that is where a rule with the silence term dropped sends a second task to a worker that has
// stepped this moment (workflow.SilentPhaseWorker's last term, workflow.(*Engine).workerSilent).
func TestSilentPhaseWorkerGetsItsTaskAgainAfterAPodReplacement(t *testing.T) {
	worker, healthy := claim.RoleImplementer, claim.RoleTester
	rig := aSilentPhaseWorker(t, readyWorker(healthy))
	token := rig.tokens[worker]
	rig.drifts(podUpdateDrift)
	rig.tick()
	for _, role := range []claim.Role{worker, healthy} {
		rig.awaitRound(role, podUpdateRound)
		rig.signal(role, "a1b2c3d")
		// The turn the worker answered from ends, which retires the round's own ask: from here
		// it holds nothing at all.
		answered := rig.claimOf(role).Pending.ID
		rig.handle(role, supervise.StreamTurnStart{Claim: rig.tokens[role]})
		rig.endTurn(role, answered)
		rig.awaitClaim(role, "the round's own ask to retire with its turn",
			func(c supervise.Claim) bool { return c.Pending == nil })
	}
	rig.awaitReplacement(1, "once both its workers had answered")
	rig.awaitSwept(worker, "its round to end with the pod", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	// The pod that replaced it brings the worker back on its own session, which is what the
	// daemon's displaced path does, and the replacement registers and readies.
	// Its session said at that turn's end that nothing of its own runs, so the replacement is
	// told of nothing its pod's end took.
	rig.reportsNothingRunning(worker)
	rig.displacedInto(worker, podUpdateReplacementPod)
	conn, beside := rig.workers[worker], rig.workers[healthy]
	before, besideBefore := len(conn.Prompts()), len(beside.Prompts())
	rig.clock.advance(podUpdateSilence + time.Minute)
	// The worker beside it moves at the clock the sweep is about to read, so the last step the
	// sweep judges it by is this moment and no silence stands over it.
	rig.stepped(healthy)

	rig.sweepForSilence()

	want := []record.SuperviseRequest{{Op: "start", Tree: podUpdateIssue, Role: worker, Generation: 1,
		Phase: phase.Implementing, Task: workflow.ResumePhaseTask(rig.issue()), Assignment: silenceAssignment, NeedsSlot: true}}
	if got := rig.starts(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("the supervise rows of %s = %+v, want %+v: the worker comes back on one start row carrying its phase's resume task",
			podUpdateIssue, got, want)
	}

	rig.drain()

	assertPromptedWithItsPhasesTask(t, rig, conn, before)
	if sent := beside.Prompts()[besideBefore:]; len(sent) != 0 {
		t.Fatalf("the %s was sent %+v, want nothing: it stepped inside the bound, and nothing of this sweep is owed to a worker that is not silent",
			healthy, sent)
	}
	if c := rig.claimOf(worker); c.State != supervise.StateReady || c.Pending == nil {
		t.Fatalf("the worker is %s holding %+v, want it ready holding the task the start row carried", c.State, c.Pending)
	}
	sent := rig.claimOf(worker).Pending.ID
	rig.handle(worker, supervise.StreamTurnStart{Claim: token})
	rig.awaitClaim(worker, "the turn its task started", func(c supervise.Claim) bool { return c.State == supervise.StateWorking })

	// That turn ends, which retires the task the repair sent and leaves the worker up, between
	// turns, holding nothing — a claim every term of the rule is true of but the silence, its
	// last step being the turn it has just taken. The silence a sweep without that term would
	// have read of it is applied here, and the record is left exactly as the repair left it.
	rig.endTurn(worker, sent)
	rig.awaitClaim(worker, "the task the repair sent to retire with its turn",
		func(c supervise.Claim) bool { return c.Pending == nil })
	rows, notices := rig.starts(), rig.silenceNotices()

	rig.appliesASilenceOf(worker)

	if got := rig.starts(); len(got) != len(rows) {
		t.Fatalf("the supervise rows of %s = %+v, want the %+v the repair left: the worker stepped this moment, and a second task would be the one its architect must never get",
			podUpdateIssue, got, rows)
	}
	if got := rig.silenceNotices(); len(got) != len(notices) {
		t.Fatalf("the notices of %s = %+v, want the %+v the repair left: a worker that has just stepped is no silence to tell anyone of",
			podUpdateIssue, got, notices)
	}
}

// The same worker, with no update round anywhere in the story: its process was reported gone and
// the daemon started it again in another pod, which is how a pod that went for a failure of its
// own takes its workers with it. This is the half of the fleet a rule keyed on the update round
// would never see, and the sweep reads it from the claim alone.
func TestSilentPhaseWorkerGetsItsTaskAgainWithNoUpdateRound(t *testing.T) {
	rig := aSilentPhaseWorker(t)
	worker := claim.RoleImplementer
	rig.goesQuiet(worker)
	rig.reportsNothingRunning(worker)
	rig.displacedInto(worker, podUpdateReplacementPod)
	conn := rig.workers[worker]
	before := len(conn.Prompts())
	if c := rig.claimOf(worker); c.UpdateRound != "" {
		t.Fatalf("the worker is in the round %q, which this case is the absence of", c.UpdateRound)
	}

	rig.sweepForSilence()
	rig.drain()

	assertPromptedWithItsPhasesTask(t, rig, conn, before)
}

// The architect of the issue is the one reader that can act on a worker nothing will prompt, so
// one silence writes it one notice, and the notice carries the whole record the daemon read:
// which issue and role, the phase, the moment of the last step, how long the silence has stood,
// and what the daemon did about it. An operator reads the other half, so the same record goes to
// the daemon's log beside the notice.
func TestSilentPhaseWorkerIsToldToItsArchitect(t *testing.T) {
	rig := aSilentPhaseWorker(t)
	worker := claim.RoleImplementer
	last := rig.clock.Now().UTC()
	rig.goesQuiet(worker)

	rig.sweepForSilence()

	notices := rig.silenceNotices()
	if len(notices) != 1 {
		t.Fatalf("the notices of %s = %+v, want the one its architect is owed", podUpdateIssue, notices)
	}
	notice := notices[0]
	if notice.Kind != "worker-silent" || notice.Role != worker || notice.Phase != phase.Implementing {
		t.Fatalf("the notice is %+v, want a worker-silent notice of the implementer of implementing", notice)
	}
	for _, part := range []string{podUpdateIssue, string(worker), string(phase.Implementing),
		last.Format(time.RFC3339), (podUpdateSilence + time.Minute).String(),
		"wrote the start row that sends the phase's task again, under the assignment " + silenceAssignment} {
		if !strings.Contains(notice.Reason, part) {
			t.Errorf("the notice's reason %q names no %q, which its architect needs to act on it", notice.Reason, part)
		}
	}

	publisher := rig.drain()

	architect := mustClaimToken(t, podUpdateIssue, claim.RoleArchitect)
	topics := publisher.topics()
	if len(topics) != 1 || topics[0] != notify.RoleTopicPrefix+string(architect) {
		t.Fatalf("the notice was published to %v, want the one topic of %s", topics, architect)
	}
	lines := loggedLines(t, rig.logs.String(), "a phase worker nothing will prompt was found silent")
	if len(lines) != 1 {
		t.Fatalf("the daemon logged the silence %d times, want once: %+v", len(lines), lines)
	}
	if lines[0]["record"] != notice.Reason {
		t.Errorf("the log's record is %q, want the notice's own %q: the two halves are one record", lines[0]["record"], notice.Reason)
	}
}

// A phase row that names no assignment is resumed under none, as admission's own repair of a
// slotted worker resumes one, and a silence whose repair the store refused is told to nobody at
// all. The notice, the start row and the mark that the fact was applied commit together, so a
// notice saying the start row was written is a notice whose start row is there, and a repair
// that failed leaves the fact unapplied for the next sweep to read again (workflow's
// workerSilent).
func TestSilentPhaseWorkerWhoseRepairTheRecordShapesOrRefuses(t *testing.T) {
	t.Run("the phase row names no assignment", func(t *testing.T) {
		rig := newPodUpdateRig(t, silentWorker(claim.RoleImplementer))
		rig.assigns(claim.RoleImplementer, "")
		rig.goesQuiet(claim.RoleImplementer)

		rig.sweepForSilence()

		notices := rig.silenceNotices()
		if len(notices) != 1 {
			t.Fatalf("the notices of %s = %+v, want the one its architect is owed", podUpdateIssue, notices)
		}
		if !strings.Contains(notices[0].Reason, "wrote the start row that sends the phase's task again") {
			t.Errorf("the notice's reason %q does not say the daemon wrote the start row", notices[0].Reason)
		}
		if strings.Contains(notices[0].Reason, "under the assignment") {
			t.Errorf("the notice's reason %q names an assignment the phase row does not hold", notices[0].Reason)
		}
		want := []record.SuperviseRequest{{Op: "start", Tree: podUpdateIssue, Role: claim.RoleImplementer, Generation: 1,
			Phase: phase.Implementing, Task: workflow.ResumePhaseTask(rig.issue()), NeedsSlot: true}}
		if got := rig.starts(); len(got) != 1 || got[0] != want[0] {
			t.Fatalf("the supervise rows of %s = %+v, want %+v: a row naming no assignment is resumed under none",
				podUpdateIssue, got, want)
		}
	})

	t.Run("the store refuses the start row", func(t *testing.T) {
		rig := aSilentPhaseWorker(t)
		rig.refusesTheStartRow()
		rig.goesQuiet(claim.RoleImplementer)
		last := rig.lastStep(claim.RoleImplementer)

		rig.sweepForSilence()

		if got := rig.starts(); len(got) != 0 {
			t.Fatalf("the supervise rows of %s = %+v, want none: the store refused the one the repair wrote", podUpdateIssue, got)
		}
		if notices := rig.silenceNotices(); len(notices) != 0 {
			t.Fatalf("the notices of %s = %+v, want none: a notice saying the task was sent again would be the last word on a repair that failed",
				podUpdateIssue, notices)
		}
		id := workerSilentEventID(rig.tokens[claim.RoleImplementer], rig.claimOf(claim.RoleImplementer).Generation, last)
		applied, err := intake.Applied(rig.ctx, rig.pool, terminalSource, []string{id})
		if err != nil {
			t.Fatalf("read whether the silence is applied: %v", err)
		}
		if applied[id] {
			t.Fatalf("the silence %s is recorded as applied, so no later sweep would read it again; the repair it stands for never happened", id)
		}

		// The store takes the start row again, and the next sweep reads the same silence and
		// applies the identical fact, which is the retry the rolled-back transaction left open.
		// It is the control under the reading above: the id this sweep applies is the id that
		// read asked after.
		rig.s.applySweepFacts = podUpdateWorkflow(rig.pool, rig.records, rig.clock, rig.log).applySweepFacts
		rig.sweepForSilence()

		if got := rig.starts(); len(got) != 1 {
			t.Fatalf("the supervise rows of %s = %+v, want the one the retry wrote", podUpdateIssue, got)
		}
		if applied, err = intake.Applied(rig.ctx, rig.pool, terminalSource, []string{id}); err != nil || !applied[id] {
			t.Fatalf("the silence %s is applied = %v (%v), want it recorded as applied by the sweep that repaired it", id, applied[id], err)
		}
	})
}

// One silence tells the architect one time, however many of the minute's sweeps read it: the
// fact's identity is the claim token, its launch and the moment of the last step
// (workerSilentEventID), and a duplicate applies nothing. A worker that steps and then goes
// silent again is a second silence, with a moment of its own, and it is told again. That is R5
// of dispatch://INFRA-3163.
//
// What stops the second notice is the identity and nothing else, and it stops it twice before
// any handler runs: the sweep's own read of which ids this daemon has applied
// (applySweepFacts) passes the id over, and ApplyFact answers Duplicate from its processed
// mark before the handler loop. The start row the repair left would refuse it too, where a
// handler reached it, but no handler of the second sweep ever does. What this test proves
// beside the one notice is the other direction: a worker which moves and falls silent again is
// a silence of its own, with a moment of its own, and is told again.
func TestSilentPhaseWorkerIsToldOncePerSilence(t *testing.T) {
	t.Run("the silence the daemon repaired", func(t *testing.T) {
		rig := aSilentPhaseWorker(t)
		worker := claim.RoleImplementer
		first := rig.clock.Now().UTC()
		rig.goesQuiet(worker)

		rig.sweepForSilence()
		rig.sweepForSilence()

		notices := rig.silenceNotices()
		if len(notices) != 1 || !strings.Contains(notices[0].Reason, first.Format(time.RFC3339)) {
			t.Fatalf("two sweeps over one silence wrote %+v, want the one notice of the silence that began at %s",
				notices, first.Format(time.RFC3339))
		}

		// The worker is sent the task the first silence repaired it with, takes its turn and
		// ends it — which is a worker moving again, and what retires that task — and then stops
		// for as long once more. The step the turn's start recorded is the second silence's own
		// moment, so the fact the sweep mints for it is a fact of its own. The step record is no
		// part of the machine's state: it is written through the store's own narrow call and
		// read back from there (supervise's putStep, supervisor.Step), which is where the sweep
		// reads it too.
		//
		// The drain is what tells the architect the first notice and finishes its row, so what
		// the outbox holds after it is the second silence's notice alone.
		rig.drain()
		rig.awaitClaim(worker, "the task the repair sent", func(c supervise.Claim) bool { return c.Pending != nil })
		sent := rig.claimOf(worker).Pending.ID
		second := rig.clock.Now().UTC()
		rig.handle(worker, supervise.StreamTurnStart{Claim: rig.tokens[worker]})
		rig.endTurn(worker, sent)
		rig.awaitClaim(worker, "the task to retire with its turn", func(c supervise.Claim) bool { return c.Pending == nil })
		if last := rig.lastStep(worker); !last.Equal(second) {
			t.Fatalf("the claim's last step is %s, want the turn it has just taken at %s", last, second)
		}
		rig.clock.advance(podUpdateSilence + time.Minute)

		rig.sweepForSilence()

		told := rig.silenceNotices()
		if len(told) != 1 || !strings.Contains(told[0].Reason, second.Format(time.RFC3339)) {
			t.Fatalf("the second silence left %+v in the outbox, want the one notice of the silence that began at %s: a worker that stepped and stopped again is told again",
				told, second.Format(time.RFC3339))
		}
	})
}

// Every reading that says there is nothing to do leaves the record exactly as it stood: no start
// row and no notice. Each case below differs from the silent worker in one term, and the term is
// what it proves. The first terms are the sweep's own, read off the live claim, and the rest are
// the handler's, read off the record inside the fact's transaction; a test here holds the chain
// to the answer either way, because what an architect must never get is a second task for work
// its worker already holds.
func TestSilentPhaseWorkerExits(t *testing.T) {
	for name, tc := range map[string]struct {
		roles []podUpdateRole
		// quiet is the role that is given a step and falls silent, "" for a case whose claims are
		// all given one.
		quiet claim.Role
		set   func(*podUpdateRig)
	}{
		"the claim is in a turn": {
			set: func(r *podUpdateRig) {
				r.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: r.tokens[claim.RoleImplementer]})
				r.awaitClaim(claim.RoleImplementer, "its turn to start",
					func(c supervise.Claim) bool { return c.State == supervise.StateWorking })
			},
		},
		"the claim holds a task it still works": {
			set: func(r *podUpdateRig) {
				r.handle(claim.RoleImplementer, supervise.RequestDeliver{Claim: r.tokens[claim.RoleImplementer],
					ID: "outbox:7", Task: "Carry on.", Phase: phase.Implementing, Generation: 1})
				r.awaitClaim(claim.RoleImplementer, "the task it holds",
					func(c supervise.Claim) bool { return c.Pending != nil })
			},
		},
		// The worker ended its turn with an async job or a subagent of its own still running,
		// which its role prompt allows and its phase reminder stays quiet for: it takes no step
		// until that work ends, and its own process is what prompts it then. The report is the
		// session's own (POST /legion/v1/claims/activity), written through the store's narrow
		// call as the route writes it, and the idle release holds a pod on the same reading
		// (workflow's IdleHoldBackgroundWork).
		"the claim's session reported background work of its own": {
			set: func(r *podUpdateRig) {
				known, err := r.st.PutClaimActivity(r.ctx, r.tokens[claim.RoleImplementer], r.claimOf(claim.RoleImplementer).Generation, claim.SessionActivity{
					At:             r.clock.Now(),
					BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"async-job"}},
				})
				if err != nil || !known {
					r.t.Fatalf("report the background work of the implementer: known %v, %v", known, err)
				}
			},
		},
		"a start for the phase is already queued": {
			set: func(r *podUpdateRig) {
				issue := r.issue()
				if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
					return workflow.StartOrOwe(r.ctx, tx, r.records, podUpdateIssue, record.SuperviseRequest{Op: "start",
						Tree: podUpdateIssue, Role: claim.RoleImplementer, Generation: 1, Phase: phase.Implementing,
						Task: workflow.ResumePhaseTask(issue), Assignment: silenceAssignment}, r.clock.Now())
				}); err != nil {
					r.t.Fatalf("queue the start that is already on its way: %v", err)
				}
			},
		},
		"the claim is in an update round": {
			set: func(r *podUpdateRig) {
				r.drifts(podUpdateDrift)
				r.tick()
				r.awaitRound(claim.RoleImplementer, podUpdateRound)
			},
		},
		"the issue is not at work": {
			set: func(r *podUpdateRig) { r.movesTo(phase.Held) },
		},
		// The merger merged the pull request, which ends its phase, and the root stands in
		// merging, where record.AtWork holds it at work, until the rest of its tree leaves the
		// workflow. That merger is owed no turn, and only the record's pull request says so
		// (workflow.PhaseFinished): its silence is the ordinary shape of a finished merger.
		"the merge its phase is for is already recorded": {
			roles: []podUpdateRole{silentWorker(claim.RoleMerger)},
			set: func(r *podUpdateRig) {
				r.movesTo(phase.Merging)
				r.recordsItsPullRequest(record.PullRequestMerged)
			},
		},
		"the role does not work the issue's phase": {
			roles: []podUpdateRole{silentWorker(claim.RoleTester)},
			quiet: claim.RoleTester,
		},
		"the claim is suspended": {
			roles: []podUpdateRole{suspendedWorker(claim.RoleImplementer, supervise.StateSuspended)},
		},
		"the claim has failed": {
			roles: []podUpdateRole{suspendedWorker(claim.RoleImplementer, supervise.StateFailed)},
		},
		"the claim is retired": {
			roles: []podUpdateRole{suspendedWorker(claim.RoleImplementer, supervise.StateRetired)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			roles := tc.roles
			if roles == nil {
				roles = []podUpdateRole{silentWorker(claim.RoleImplementer)}
			}
			rig := newPodUpdateRig(t, roles...)
			quiet := tc.quiet
			if quiet == "" {
				quiet = roles[0].role
			}
			rig.assigns(quiet, silenceAssignment)
			if tc.set != nil {
				tc.set(rig)
			}
			// The moment the rule judges from is written after the case has been set up and
			// before the clock moves, so every case has the one old step and differs from the
			// silent worker in the term it is named for: a turn this case started would
			// otherwise record a step of its own and answer the sweep on the clock instead.
			rig.stepped(quiet)
			rig.clock.advance(podUpdateSilence + time.Minute)
			before := rig.starts()

			rig.sweepForSilence()

			if got := rig.starts(); len(got) != len(before) {
				t.Errorf("the supervise rows of %s = %+v, want the %+v this case left: this reading says there is nothing to do",
					podUpdateIssue, got, before)
			}
			if notices := rig.silenceNotices(); len(notices) != 0 {
				t.Errorf("the notices of %s = %+v, want none: a reading that shows the worker needs nothing tells nobody",
					podUpdateIssue, notices)
			}
		})
	}
}

// The pass is nothing unless the sweep calls it, so this test starts the sweep itself, with a
// period a test can wait out, and holds one silent phase worker in front of it. It fails if the
// pass is written and never reached from the loop that rides every claim read (reconcileOrphans).
func TestReconcileOrphansSweepsForSilentPhaseWorkers(t *testing.T) {
	rig := aSilentPhaseWorker(t)
	rig.goesQuiet(claim.RoleImplementer)
	rig.s.plan.orphanSweep = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(rig.ctx)
	defer cancel()
	go rig.s.reconcileOrphans(ctx)

	testwait.Eventually(t, "the orphan sweep to tell the workflow of the silent phase worker", func() bool {
		return len(rig.starts()) > 0
	})

	if notices := rig.silenceNotices(); len(notices) != 1 {
		t.Fatalf("the notices of %s = %+v, want the one the sweep's own fact wrote", podUpdateIssue, notices)
	}
}

// suspendedWorker is a worker of the issue's pod in a state no task reaches: suspended, failed or
// retired. A failed or retired claim records no process, as the machine leaves one (letGo).
func suspendedWorker(role claim.Role, state supervise.ClaimState) podUpdateRole {
	worker := silentWorker(role)
	worker.state = state
	if state != supervise.StateSuspended {
		worker.inPod = false
	}
	if state == supervise.StateSuspended {
		worker.suspendedBy = supervise.SuspendedByOperator
	}
	return worker
}

// movesTo writes the issue into the phase named, as a transition of the workflow's own leaves it.
func (r *podUpdateRig) movesTo(to phase.Phase) {
	r.t.Helper()
	issue := r.issue()
	issue.Phase = to
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.PutIssue(r.ctx, tx, issue)
	}); err != nil {
		r.t.Fatalf("move %s to %s: %v", podUpdateIssue, to, err)
	}
}

// recordsItsPullRequest records the issue's pull request in the state named, as the daemon's own
// reading of GitHub leaves it (record.Store.PutPullRequest).
func (r *podUpdateRig) recordsItsPullRequest(state record.PullRequestState) {
	r.t.Helper()
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.PutPullRequest(r.ctx, tx, record.PullRequest{State: state, Issue: podUpdateIssue,
			Repo: "acme/widgets", Number: 42, Branch: "legion/" + podUpdateIssue, HeadSHA: "head", Failing: []string{}})
	}); err != nil {
		r.t.Fatalf("record the pull request of %s as %s: %v", podUpdateIssue, state, err)
	}
}

// refusesTheStartRow gives the silence fact an engine over a record store with one write
// refused: the enqueue of the supervise start row its repair writes. Every other call is the
// real store's, so what the handler reads is the record the rig seeded, and what fails is the
// one write the repair depends on.
func (r *podUpdateRig) refusesTheStartRow() {
	r.s.applySweepFacts = podUpdateWorkflow(r.pool, refusingStartStore{Store: r.records}, r.clock, r.log).applySweepFacts
}

// refusedStart is the error the staged store answers that write with.
var refusedStart = errors.New("the store refused the start row")

type refusingStartStore struct {
	record.Store
}

func (s refusingStartStore) Enqueue(ctx context.Context, tx pgx.Tx, row record.OutboxRow) error {
	if row.Kind == record.OutboxKindSupervise {
		return refusedStart
	}
	return s.Store.Enqueue(ctx, tx, row)
}

// podUpdateBackground is the longer bound the sweep reads a worker's own report of background
// work against, as legion.yaml's worker_background_hours sets it
// (config.Config.WorkerBackground). It is the shipped default, and well past podUpdateSilence,
// which the loader refuses a value at or under (config's checkWorkerBackground).
const podUpdateBackground = 4 * time.Hour

// aBackgroundStalledWorker is the silent phase worker above with this deployment's background
// bound registered on the supervision the sweep reads it from: the fixture of a worker whose
// own session says it is waiting on work of its own.
func aBackgroundStalledWorker(t *testing.T, beside ...podUpdateRole) *podUpdateRig {
	t.Helper()
	rig := aSilentPhaseWorker(t, beside...)
	rig.s.cfg.WorkerBackground = podUpdateBackground
	return rig
}

// reportsBackgroundWork is role's own session reporting, at the clock's moment, that background
// work of its own is running, straight through the store's narrow call, which is where the
// route's own write lands (api's claims activity route, store's PutClaimActivity), onto the
// claim's current launch as the route writes it. A session sends such a report only when its
// reading changes, so one report stands until the work it names ends.
func (r *podUpdateRig) reportsBackgroundWork(role claim.Role, kinds ...string) time.Time {
	r.t.Helper()
	at := r.clock.Now().UTC()
	known, err := r.st.PutClaimActivity(r.ctx, r.tokens[role], r.claimOf(role).Generation, claim.SessionActivity{
		At:             at,
		BackgroundWork: claim.BackgroundWork{Running: true, Kinds: kinds},
	})
	if err != nil || !known {
		r.t.Fatalf("report the background work of the %s: known %v, %v", role, known, err)
	}
	return at
}

// reportsNothingRunning is role's own session reporting, at its turn's end as Oh My Pi's and a
// Claude Code worker's Stop hook do, that nothing of its own is running, onto the claim's current
// launch through the store's narrow call. A process that reported so and was in no turn ends no
// work when it is let go, so its relaunch records nothing and tells nothing (supervise's
// recordEnd).
func (r *podUpdateRig) reportsNothingRunning(role claim.Role) {
	r.t.Helper()
	known, err := r.st.PutClaimActivity(r.ctx, r.tokens[role], r.claimOf(role).Generation, claim.SessionActivity{
		At: r.clock.Now().UTC()})
	if err != nil || !known {
		r.t.Fatalf("report that nothing of the %s runs: known %v, %v", role, known, err)
	}
}

// A worker waiting on background work of its own that has stood past the longer bound is named
// to its architect once and sent nothing at all. It is the worker the silence repair must leave
// alone — its session says work of its own is running, which no bound can tell from a job that
// hung — so the daemon tells and decides nothing, and that it told is no reason to prompt: the
// sweep reads the same claim for both halves in the same minute, and the start row the silence
// half would write is exactly what an architect must never get for a worker that is busy.
//
// A second sweep adds neither notice nor row: the fact's identity is the claim token, its launch
// and the moment of the report (workerBackgroundEventID), a session reports only when its reading
// changes, and a duplicate applies nothing. That is R6 of dispatch://INFRA-3163.
func TestWorkerBackgroundStalledIsToldToItsArchitectOnceAndSentNothing(t *testing.T) {
	rig := aBackgroundStalledWorker(t)
	worker := claim.RoleImplementer
	rig.goesQuiet(worker)
	conn := rig.workers[worker]
	before := len(conn.Prompts())
	reported := rig.reportsBackgroundWork(worker, "async-job", "subagent")
	rig.clock.advance(podUpdateBackground + time.Minute)

	rig.sweepForSilence()

	if got := rig.starts(); len(got) != 0 {
		t.Fatalf("the supervise rows of %s = %+v, want none: a worker whose session says work of its own is running is told of and never prompted",
			podUpdateIssue, got)
	}
	notices := rig.silenceNotices()
	if len(notices) != 1 {
		t.Fatalf("the notices of %s = %+v, want the one its architect is owed", podUpdateIssue, notices)
	}
	notice := notices[0]
	if notice.Kind != "worker-background-stalled" || notice.Role != worker || notice.Phase != phase.Implementing {
		t.Fatalf("the notice is %+v, want a worker-background-stalled notice of the implementer of implementing", notice)
	}
	for _, part := range []string{podUpdateIssue, string(worker), string(phase.Implementing), "async-job, subagent",
		reported.Format(time.RFC3339), (podUpdateBackground + time.Minute).String(),
		"sent that worker nothing and will send it nothing for this"} {
		if !strings.Contains(notice.Reason, part) {
			t.Errorf("the notice's reason %q names no %q, which its architect needs to act on it", notice.Reason, part)
		}
	}

	rig.sweepForSilence()

	if got, rows := rig.silenceNotices(), rig.starts(); len(got) != 1 || len(rows) != 0 {
		t.Fatalf("a second sweep over the same report left %+v and %+v, want the one notice and no row: one standing report is one telling",
			got, rows)
	}

	publisher := rig.drain()

	architect := mustClaimToken(t, podUpdateIssue, claim.RoleArchitect)
	topics := publisher.topics()
	if len(topics) != 1 || topics[0] != notify.RoleTopicPrefix+string(architect) {
		t.Fatalf("the notice was published to %v, want the one topic of %s", topics, architect)
	}
	if sent := conn.Prompts()[before:]; len(sent) != 0 {
		t.Fatalf("the worker was sent %+v, want nothing at all", sent)
	}
	lines := loggedLines(t, rig.logs.String(), "is waiting on background work that has stood past the bound")
	if len(lines) != 1 {
		t.Fatalf("the daemon logged the standing report %d times, want once: %+v", len(lines), lines)
	}
	if lines[0]["record"] != notice.Reason {
		t.Errorf("the log's record is %q, want the notice's own %q: the two halves are one record", lines[0]["record"], notice.Reason)
	}
}

// A report inside the bound earns nothing from either half of the sweep: the background half
// judges the report too young to name, and the silence half leaves every worker whose session
// reports work of its own alone, however old its last step is (workflow.SilentPhaseWorker). The
// worker here has been silent for longer than the silence bound and is waiting on a job that
// has run for minutes, which is the ordinary shape of a healthy worker.
func TestWorkerBackgroundInsideTheBoundTellsNobodyAndPromptsNobody(t *testing.T) {
	rig := aBackgroundStalledWorker(t)
	worker := claim.RoleImplementer
	rig.goesQuiet(worker)
	reported := rig.reportsBackgroundWork(worker, "async-job")
	rig.clock.advance(podUpdateBackground - time.Minute)

	rig.sweepForSilence()

	if got := rig.starts(); len(got) != 0 {
		t.Fatalf("the supervise rows of %s = %+v, want none: the worker's own process ends the wait it reported",
			podUpdateIssue, got)
	}
	if notices := rig.silenceNotices(); len(notices) != 0 {
		t.Fatalf("the notices of %s = %+v, want none: the work it reported has not stood past the bound",
			podUpdateIssue, notices)
	}
	// The sweep minted nothing either, which is this pass's own bound term rather than the
	// handler's: a fact the background handler answers nothing for is still recorded as applied
	// (intake.ApplyFact marks every fact no handler leaves unspent, and that handler never
	// does), so an id this read does not find is an id the sweep never wrote.
	id := workerBackgroundEventID(rig.tokens[worker], rig.claimOf(worker).Generation, reported)
	applied, err := intake.Applied(rig.ctx, rig.pool, terminalSource, []string{id})
	if err != nil {
		t.Fatalf("read whether the report is applied: %v", err)
	}
	if applied[id] {
		t.Fatalf("the sweep applied %s for a report inside the bound; the bound is what this pass reads", id)
	}
}

// The work the report named ended and other work began, so the session sent a new reading: that
// is a report of its own, and once it has stood past the bound the architect is told again. It
// is what the fact's identity rests on (workerBackgroundEventID): keyed on the claim's last
// step, which a worker waiting on background work does not move either, this second telling
// would be read as the first one's duplicate and the architect would hear nothing of the job
// that is hanging now.
func TestWorkerBackgroundStalledIsToldAgainWhenTheSessionsReportMoves(t *testing.T) {
	rig := aBackgroundStalledWorker(t)
	worker := claim.RoleImplementer
	rig.goesQuiet(worker)
	first := rig.reportsBackgroundWork(worker, "async-job")
	rig.clock.advance(podUpdateBackground + time.Minute)

	rig.sweepForSilence()

	if notices := rig.silenceNotices(); len(notices) != 1 || !strings.Contains(notices[0].Reason, first.Format(time.RFC3339)) {
		t.Fatalf("the first sweep wrote %+v, want the one notice of the report made at %s", notices, first.Format(time.RFC3339))
	}
	// The drain tells the architect that first notice and finishes its row, so what the outbox
	// holds after it is the second report's notice alone.
	rig.drain()
	second := rig.reportsBackgroundWork(worker, "subagent")
	rig.clock.advance(podUpdateBackground + time.Minute)

	rig.sweepForSilence()

	told := rig.silenceNotices()
	if len(told) != 1 || !strings.Contains(told[0].Reason, second.Format(time.RFC3339)) {
		t.Fatalf("the second report left %+v in the outbox, want the one notice of the report made at %s: a reading that moved is a telling of its own",
			told, second.Format(time.RFC3339))
	}
	if !strings.Contains(told[0].Reason, "subagent") {
		t.Errorf("the notice's reason %q does not name the kind the session reported second", told[0].Reason)
	}
	if got := rig.starts(); len(got) != 0 {
		t.Fatalf("the supervise rows of %s = %+v, want none: neither telling sends that worker anything", podUpdateIssue, got)
	}
}

// A merged root's merger whose own session's report of background work has stood past the bound
// tells nobody: the merge ends its phase (workflow.PhaseFinished), the root stands in merging
// until the rest of its tree leaves the workflow, and an architect told of that worker's
// background work would be asked to decide about a worker of a phase that is over. The handler
// of this fact reads the same record the silence handler reads (workflow's phaseWorkerRecord),
// and this case holds that record to the exclusion for this fact too.
func TestWorkerBackgroundOfAMergedRootsMergerTellsNobody(t *testing.T) {
	worker := claim.RoleMerger
	rig := newPodUpdateRig(t, silentWorker(worker))
	rig.s.cfg.WorkerBackground = podUpdateBackground
	rig.assigns(worker, silenceAssignment)
	rig.movesTo(phase.Merging)
	rig.recordsItsPullRequest(record.PullRequestMerged)
	rig.goesQuiet(worker)
	rig.reportsBackgroundWork(worker, "async-job")
	rig.clock.advance(podUpdateBackground + time.Minute)

	rig.sweepForSilence()

	if got := rig.starts(); len(got) != 0 {
		t.Fatalf("the supervise rows of %s = %+v, want none: a merged root's merger is owed no turn", podUpdateIssue, got)
	}
	if notices := rig.silenceNotices(); len(notices) != 0 {
		t.Fatalf("the notices of %s = %+v, want none: the merge ended the phase whose worker the report is of",
			podUpdateIssue, notices)
	}
}

// A worker whose completion of its phase entry the workflow accepted is owed no turn
// (workflow.PhaseWorkerOwedTurn), and the background telling leaves it alone as the silence
// repair does: its architect would be asked to decide about work a finished worker is waiting
// on. The case differs from TestWorkerBackgroundStalledIsToldToItsArchitectOnceAndSentNothing in
// the completion moment of the worker's phase row alone.
func TestWorkerBackgroundOfARoleWhoseCompletionWasAcceptedTellsNobody(t *testing.T) {
	rig := aBackgroundStalledWorker(t)
	worker := claim.RoleImplementer
	if err := pgx.BeginFunc(rig.ctx, rig.pool, func(tx pgx.Tx) error {
		return rig.records.PutPhase(rig.ctx, tx, record.PhaseRow{Issue: podUpdateIssue, Role: worker, Claim: rig.tokens[worker],
			Assignment: silenceAssignment, CompletedAt: rig.clock.Now()})
	}); err != nil {
		t.Fatalf("record the completion of the %s: %v", worker, err)
	}
	rig.goesQuiet(worker)
	rig.reportsBackgroundWork(worker, "async-job")
	rig.clock.advance(podUpdateBackground + time.Minute)

	rig.sweepForSilence()

	if got := rig.starts(); len(got) != 0 {
		t.Fatalf("the supervise rows of %s = %+v, want none: a worker whose completion was accepted is owed no turn", podUpdateIssue, got)
	}
	if notices := rig.silenceNotices(); len(notices) != 0 {
		t.Fatalf("the notices of %s = %+v, want none: the worker the report is of has finished its phase entry",
			podUpdateIssue, notices)
	}
}

// A worker whose process reported background work of its own and then died with its pod comes
// back in a replacement process that reports nothing until its first turn ends: a Claude Code
// worker reports its activity at the Stop hook alone (packages/claude-envoy/src/legion-phase-hook.ts,
// sendActivity), and nothing prompts it to a first turn. The report the dead process left is
// then the claim's only activity record, and it describes a job the pod replacement killed. The
// worker is the one this issue is about — up, between turns, holding no task, quiet past the
// bound — so the sweep sends it its phase's task again, well inside the background bound: a
// rule that read the stale report as work still running would leave it for the four hours of
// worker_background_hours and then only tell its architect, which is the incident of
// dispatch://INFRA-3163 with a longer fuse.
func TestSilentPhaseWorkerWhoseDeadProcessLeftABackgroundReportGetsItsTaskAgain(t *testing.T) {
	rig := aBackgroundStalledWorker(t)
	worker := claim.RoleImplementer
	token := rig.tokens[worker]
	rig.handle(worker, supervise.StreamTurnStart{Claim: token})
	rig.handle(worker, supervise.StreamTurnEnd{Claim: token})
	rig.awaitClaim(worker, "its turn to end", func(c supervise.Claim) bool { return c.State != supervise.StateWorking })
	// The turn ends with the session's own background task running, which the Stop hook reports.
	rig.reportsBackgroundWork(worker, "claude-background-task")
	// The pod goes with that task in it, and the replacement registers and readies. It reports
	// nothing: the first report a Claude Code session makes is at the end of its first turn.
	rig.displacedInto(worker, podUpdateReplacementPod)
	// The replacement is told first that the task its dead process reported died with the pod
	// (dispatch://INFRA-3276 R3), and reads it in a turn; then nothing prompts it.
	rig.takesRelaunchSentence(worker, "Each Claude Code background task that you started stopped with that process.")
	conn := rig.workers[worker]
	before := len(conn.Prompts())
	rig.clock.advance(podUpdateSilence + time.Minute)

	rig.sweepForSilence()

	if got := rig.starts(); len(got) != 1 || got[0].Op != "start" || got[0].Task != workflow.ResumePhaseTask(rig.issue()) {
		t.Fatalf("the supervise rows of %s = %+v, want one start row carrying the phase's resume task: the background work the dead process reported died with its pod, so nothing will prompt this worker",
			podUpdateIssue, got)
	}
	rig.drain()
	assertPromptedWithItsPhasesTask(t, rig, conn, before)
}

// The same worker, with one sweep more: the silence bound passes while the dead process's report
// still says its background task is running, so a sweep reads the worker as busy before the pod
// goes. That sweep must write nothing, since the worker is busy; and it must spend nothing
// either. The launch writes no step, so a silence keyed on the token and the last step alone,
// refused on the background term and marked applied by that sweep, would be the same id after
// the replacement, skipped as applied, and the worker never repaired nor its architect told.
// The sweep mints no silence fact for a claim whose report says work is running, and the id
// carries the claim's launch (workerSilentEventID), each of which closes that.
func TestSilentPhaseWorkerReadAsBusyBeforeItsPodWentGetsItsTaskAgain(t *testing.T) {
	rig := aBackgroundStalledWorker(t)
	worker := claim.RoleImplementer
	token := rig.tokens[worker]
	rig.handle(worker, supervise.StreamTurnStart{Claim: token})
	rig.handle(worker, supervise.StreamTurnEnd{Claim: token})
	rig.awaitClaim(worker, "its turn to end", func(c supervise.Claim) bool { return c.State != supervise.StateWorking })
	rig.reportsBackgroundWork(worker, "claude-background-task")
	rig.clock.advance(podUpdateSilence + time.Minute)

	rig.sweepForSilence()

	if got := rig.starts(); len(got) != 0 {
		t.Fatalf("the supervise rows of %s = %+v, want none: the worker's own session says its background task is running", podUpdateIssue, got)
	}
	if notices := rig.silenceNotices(); len(notices) != 0 {
		t.Fatalf("the notices of %s = %+v, want none: the worker is busy and its report is inside the background bound", podUpdateIssue, notices)
	}

	rig.displacedInto(worker, podUpdateReplacementPod)
	rig.takesRelaunchSentence(worker, "Each Claude Code background task that you started stopped with that process.")
	conn := rig.workers[worker]
	before := len(conn.Prompts())
	rig.clock.advance(podUpdateSilence + time.Minute)

	rig.sweepForSilence()

	if got := rig.starts(); len(got) != 1 || got[0].Op != "start" || got[0].Task != workflow.ResumePhaseTask(rig.issue()) {
		t.Fatalf("the starts after the replacement's sweep = %+v, want one start row carrying the phase's resume task: the background work the dead process reported died with its pod",
			got)
	}
	notices := rig.silenceNotices()
	if len(notices) != 1 || notices[0].Kind != "worker-silent" {
		t.Fatalf("the notices of %s = %+v, want the one worker-silent notice its architect is owed", podUpdateIssue, notices)
	}
	rig.drain()
	assertPromptedWithItsPhasesTask(t, rig, conn, before)
}

// The worker's session reports between the sweep's read of the claims and the fact's
// transaction: its two-minute phase reminder re-armed, so its report says a reminder is due later
// than now. The handler refuses the claim for that report, which explains the quiet for now; but
// that refusal must not spend the fact's id. The reminder can come due and start no turn, and
// then neither the last step nor the launch the id is keyed on has moved (workerSilentEventID):
// a spent id would be skipped at every later sweep, and the worker never repaired nor its
// architect told. The refusal leaves the fact unspent (intake.Result.Unspent), so the sweep after
// the reminder came due applies the same id and repairs the worker.
func TestSilentPhaseWorkerWhoseReminderReArmedAfterTheSweepReadGetsItsTaskOnceItComesDue(t *testing.T) {
	rig := aSilentPhaseWorker(t)
	worker := claim.RoleImplementer
	rig.goesQuiet(worker)
	conn := rig.workers[worker]
	before := len(conn.Prompts())
	// The sweep reads the claims while the worker's report explains nothing.
	claims, err := rig.sup.Claims(rig.ctx)
	if err != nil {
		t.Fatalf("read the claims for the sweep: %v", err)
	}
	// The reminder re-arms before the sweep's facts are applied.
	due := rig.clock.Now().UTC().Add(2 * time.Minute)
	known, err := rig.st.PutClaimActivity(rig.ctx, rig.tokens[worker], rig.claimOf(worker).Generation,
		claim.SessionActivity{At: rig.clock.Now().UTC(), Phase: claim.PhaseActivity{NextReminderAt: &due}})
	if err != nil || !known {
		t.Fatalf("report the re-armed reminder of the %s: known %v, %v", worker, known, err)
	}

	rig.s.tellSilentPhaseWorkers(rig.ctx, claims)

	if got := rig.starts(); len(got) != 0 {
		t.Fatalf("the supervise rows of %s = %+v, want none: the worker's own reminder is due later than now", podUpdateIssue, got)
	}
	if notices := rig.silenceNotices(); len(notices) != 0 {
		t.Fatalf("the notices of %s = %+v, want none: the worker's own reminder is due later than now", podUpdateIssue, notices)
	}

	// The reminder comes due and starts no turn: the report now explains nothing.
	rig.clock.advance(3 * time.Minute)

	rig.sweepForSilence()

	if got := rig.starts(); len(got) != 1 || got[0].Op != "start" || got[0].Task != workflow.ResumePhaseTask(rig.issue()) {
		t.Fatalf("the supervise rows of %s = %+v, want one start row carrying the phase's resume task: the refusal for the reminder spent the silence's id",
			podUpdateIssue, got)
	}
	notices := rig.silenceNotices()
	if len(notices) != 1 || notices[0].Kind != "worker-silent" {
		t.Fatalf("the notices of %s = %+v, want the one worker-silent notice its architect is owed", podUpdateIssue, notices)
	}
	rig.drain()
	assertPromptedWithItsPhasesTask(t, rig, conn, before)
}

// A dead process's report of background work can reach the store after the relaunch that
// replaced it: the route authenticated it just before the launch and writes it just after the
// launch's clear. Written, it would stand on the new launch as the replacement's own report, and
// a Claude Code replacement, which reports only at its Stop hook and is prompted to no first
// turn, would never be repaired, its architect told instead, at the background bound, of a job
// the pod replacement killed. The store writes a report only onto the launch it belongs to
// (store's PutClaimActivity), so the sweep reads the replacement as a session that has reported
// nothing and sends it its phase's task at the silence bound.
func TestSilentPhaseWorkerWhoseDeadProcessReportLandedAfterItsRelaunchGetsItsTaskAgain(t *testing.T) {
	rig := aBackgroundStalledWorker(t)
	worker := claim.RoleImplementer
	token := rig.tokens[worker]
	rig.handle(worker, supervise.StreamTurnStart{Claim: token})
	rig.handle(worker, supervise.StreamTurnEnd{Claim: token})
	rig.awaitClaim(worker, "its turn to end", func(c supervise.Claim) bool { return c.State != supervise.StateWorking })
	old := rig.claimOf(worker).Generation
	rig.displacedInto(worker, podUpdateReplacementPod)
	// No report of the dead launch had landed when its pod went, so the replacement is told that
	// the daemon has none (dispatch://INFRA-3276 R3), and reads it in a turn.
	rig.takesRelaunchSentence(worker, "The daemon has no report of your background work in that process.")
	// The dead process's Stop-hook report, authenticated under its own launch, lands after the
	// launch's clear.
	if _, err := rig.st.PutClaimActivity(rig.ctx, token, old, claim.SessionActivity{
		At:             rig.clock.Now().UTC(),
		BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"claude-background-task"}},
	}); err != nil {
		t.Fatalf("write the dead process's report: %v", err)
	}
	conn := rig.workers[worker]
	before := len(conn.Prompts())
	rig.clock.advance(podUpdateSilence + time.Minute)

	rig.sweepForSilence()

	if got := rig.starts(); len(got) != 1 || got[0].Op != "start" || got[0].Task != workflow.ResumePhaseTask(rig.issue()) {
		t.Fatalf("the supervise rows of %s = %+v, want one start row carrying the phase's resume task: the report is the dead launch's, not the replacement's",
			podUpdateIssue, got)
	}
	notices := rig.silenceNotices()
	if len(notices) != 1 || notices[0].Kind != "worker-silent" {
		t.Fatalf("the notices of %s = %+v, want the one worker-silent notice its architect is owed", podUpdateIssue, notices)
	}
	rig.drain()
	assertPromptedWithItsPhasesTask(t, rig, conn, before)

	rig.clock.advance(podUpdateBackground)

	rig.sweepForSilence()

	for _, notice := range rig.silenceNotices() {
		if notice.Kind == "worker-background-stalled" {
			t.Fatalf("the sweep past the background bound wrote %+v: the background work it names is the dead launch's", notice)
		}
	}
}
