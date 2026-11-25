// The record of each step names the models that answered it, or says that it does not know them.
// A record names the step's set model and each model that answered, with its calls, most first.
// Where the daemon cannot know those calls, the record's list of answers is null.
//
// Each test boots the whole daemon over the real Dispatch (stepRecordsRig). The rig's stand-in
// shim counts each answer of its agent as the real shim does, and it answers the daemon's reads of
// those counts. A test reads each record back from Dispatch, and each read a step used from the
// daemon's own log.
//
// Four walks run their reads on a held clock (heldClock), whose bounds fire only when the walk
// fires them. In TestAStepRecordCountsTheCallsOfItsAgent, a planner's completion arms its bound and
// stops it. In TestAStepRecordSaysWhenItDoesNotKnow, a read of a disconnected agent fails at once,
// and a read of an agent that answers no read fails at the bound. In
// TestATotalsReadEndsAtItsBoundWhileItsAgentsWriterIsHeld, a read behind a held writer fails at the
// bound.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// stepRecordsModelB is the second model an agent of these walks answers with, beside
// stepRecordsModel.
const stepRecordsModelB = "stand-in/model-b"

// plannedRound is the planner's completion, as plan completes it.
var plannedRound = api.HandoffCompleteRequest{Summary: "planned"}

// implementedRound is the implementer's completion of a round on the pull request's head, as
// implement completes it.
var implementedRound = api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}

// A step's record counts the calls of its agent: its set model, and each model that answered with
// its calls, most first. The calls are those the launch made between the read the step's start used
// and the read its end used.
func TestAStepRecordCountsTheCallsOfItsAgent(t *testing.T) {
	t.Parallel()
	// The planner answers stepRecordsModel three times and stepRecordsModelB once, and completes. On
	// a held read clock, the completion's read arms its bound and stops it once the read returns.
	t.Run("a completion counts the calls of two models", func(t *testing.T) {
		r := newStepRecordsRig(t, 1)
		clock := holdReadClock(r)
		planner := twoModelsAnswer(r)
		_, before := clock.armed(streamDrainBound)
		r.finish(planner, "plan", plannedRound, phase.Implementing)
		if _, after := clock.armed(streamDrainBound); after <= before {
			t.Fatalf("the planner's completion armed no bound for its read of the planner's calls: %d bounds armed before it, %d after", before, after)
		}
		if !testwait.Holds(stepRecordsWait, 20*time.Millisecond, func() bool { return readBoundsArmed(clock) == 0 }) {
			live, total := clock.armed(streamDrainBound)
			t.Fatalf("%d of the %d bounds armed for a read stay armed, want each stopped once its reads returned, with none fired", live, total)
		}
		twoModelsCounted(r, planner, "passed", "agent")
	})
	// The planner makes the same calls, and the tree's architect stops the task while the planner is
	// connected. The stop's own fact reads the planner's calls and ends planning stopped.
	t.Run("a stop by the tree's architect counts the calls of the agent it stops", func(t *testing.T) {
		r := newStepRecordsRig(t, 1)
		planner := twoModelsAnswer(r)
		stopTheTask(r)
		twoModelsCounted(r, planner, "stopped", "TaskStopped")
	})
	// The implementer's first run counts each call of its launch. Its second run counts from the
	// read its start used, two calls past the five of the first run.
	t.Run("a second run counts from the read its start used", func(t *testing.T) {
		r := newStepRecordsRig(t, 1)
		implementer := implementAgain(r)
		implementer.take()
		implementer.answer()
		r.finish(implementer, "implement", implementedRound, phase.Testing)
		r.waitModels(r.key, 2, 1, stepModels{result: "passed", setModel: stepRecordsModel,
			answers: answerList(answerOf(stepRecordsModel, 5)), model: stepRecordsModel})
		r.waitModels(r.key, 2, 2, stepModels{result: "passed", setModel: stepRecordsModel,
			answers: answerList(answerOf(stepRecordsModel, 2)), model: stepRecordsModel})
	})
	// The daemon restarts in the implementer's second run. The pane and the counts of its shim
	// outlive the daemon, so the run counts the calls on both sides of the restart.
	t.Run("a run counts across a restart of the daemon", func(t *testing.T) {
		r := newStepRecordsRig(t, 1)
		implementer := implementAgain(r)
		implementer.take()
		implementer.answer()
		r.restart()
		// A restarted daemon knows no model until the agent answers again (supervise.StreamAnswered).
		implementer.ownTurn()
		r.finish(implementer, "implement", implementedRound, phase.Testing)
		r.waitModels(r.key, 2, 2, stepModels{result: "passed", setModel: stepRecordsModel,
			answers: answerList(answerOf(stepRecordsModel, 3)), model: stepRecordsModel})
	})
	// The planner takes its task with no call of its model, and completes. The record knows its
	// counts, so it names an empty list, and no model answered it.
	t.Run("a run with no call names an empty list", func(t *testing.T) {
		r := newStepRecordsRig(t, 1)
		planner := r.worker(claim.RolePlanner)
		planner.takeQuietly()
		r.finish(planner, "plan", plannedRound, phase.Implementing)
		r.waitModels(r.key, 1, 1, stepModels{result: "passed", setModel: stepRecordsModel, answers: answerList(), model: ""})
	})
}

// A step's record says that it does not know its calls wherever the daemon cannot count them: its
// list of answers is null. It still names the set model and the model that answered it last.
func TestAStepRecordSaysWhenItDoesNotKnow(t *testing.T) {
	t.Parallel()
	// The implementer's launch exits in its second run with no final totals, and its claim is
	// launched again. The calls the first launch made after the start's read are lost.
	t.Run("a launch that exited with no final totals", func(t *testing.T) {
		r := newStepRecordsRig(t, 1)
		implementer := implementAgain(r)
		implementer.take()
		implementer.exit(false)
		implementer.relaunched()
		implementer.take()
		r.finish(implementer, "implement", implementedRound, phase.Testing)
		r.waitModels(r.key, 2, 2, stepModels{result: "passed", setModel: stepRecordsModel, model: stepRecordsModel})
	})
	// The implementer's shim loses its connection in the second run while its process lives on.
	// The architect's stop reads no totals of it, at once, with no bound fired. The stop answers
	// although no bound ever fires on the held clock, and no bound stays armed after it.
	t.Run("an agent with no connection when its step stops", func(t *testing.T) {
		r := newStepRecordsRig(t, 1)
		clock := holdReadClock(r)
		implementer := implementAgain(r)
		implementer.take()
		implementer.disconnect()
		stopTheTask(r)
		if !testwait.Holds(stepRecordsWait, 20*time.Millisecond, func() bool { return readBoundsArmed(clock) == 0 }) {
			t.Fatalf("%d bounds of a read stay armed after the stop, want none: a read of %s with no connection waits on no bound", readBoundsArmed(clock), implementer.token)
		}
		if read := r.waitEmptyRead(implementer.token, "no connection"); read.Waited != 0 {
			t.Fatalf("the read of %s with no connection waited %s on the read's own held clock, want 0", implementer.token, read.Waited)
		}
		r.waitModels(r.key, 2, 2, stepModels{result: "stopped", setModel: stepRecordsModel, model: stepRecordsModel})
	})
	// The implementer answers no read of its calls, and completes in its second run. The completion
	// waits for its read until the walk fires the read's bound on the held clock.
	t.Run("an agent that answers no read within the bound", func(t *testing.T) {
		r := newStepRecordsRig(t, 1)
		clock := holdReadClock(r)
		implementer := implementAgain(r)
		implementer.take()
		implementer.holdReads.Store(true)
		completeAtTheBound(r, clock, implementer, "implement", implementedRound, "the implementer answered no read")
		// A later fact's read of the implementer is answered again, so no later fact waits for a
		// bound the walk never fires.
		implementer.holdReads.Store(false)
		if read := r.waitEmptyRead(implementer.token, "no answer within the bound"); read.Waited != streamDrainBound {
			t.Fatalf("the read of %s that met its bound waited %s on the held clock, want exactly the bound, %s", implementer.token, read.Waited, streamDrainBound)
		}
		implementer.endTurn()
		r.waitPhase(phase.Testing)
		r.waitModels(r.key, 2, 2, stepModels{result: "passed", setModel: stepRecordsModel, model: stepRecordsModel})
	})
	// The planner's model answers once under a name with a space, outside the form a model's name
	// takes. The shim counts that call outside the form, so no list names it.
	t.Run("a model whose name is outside the form", func(t *testing.T) {
		r := newStepRecordsRig(t, 1)
		planner := r.worker(claim.RolePlanner)
		planner.take()
		planner.model = "stand-in/model with space"
		planner.answer()
		planner.model = stepRecordsModel
		planner.answer()
		r.finish(planner, "plan", plannedRound, phase.Implementing)
		r.waitModels(r.key, 1, 1, stepModels{result: "passed", setModel: stepRecordsModel, model: stepRecordsModel})
	})
	// Seventeen models answer the planner, one more than a record names (steprecord.AnswersMax).
	t.Run("more models than a record names", func(t *testing.T) {
		r := newStepRecordsRig(t, 1)
		planner := r.worker(claim.RolePlanner)
		planner.take()
		for n := range 16 {
			planner.model = fmt.Sprintf("stand-in/m-%02d", n+1)
			planner.answer()
		}
		r.finish(planner, "plan", plannedRound, phase.Implementing)
		r.waitModels(r.key, 1, 1, stepModels{result: "passed", setModel: stepRecordsModel, model: "stand-in/m-16"})
	})
	// The review that asks for changes reads the implementer's launch before its transaction. The
	// launch then exits with no final totals and is made again. So the fact's only read of the
	// implementer is of a launch gone by its transaction. The second run's start has no usable read.
	t.Run("a launch that exited after the fact's read", func(t *testing.T) {
		r := newStepRecordsRig(t, 1)
		r.plan()
		r.implement()
		r.test()
		reviewer := r.worker(claim.RoleReviewer)
		reviewer.take()
		reviewer.endTurn()
		implementer := r.worker(claim.RoleImplementer)
		// The hook holds the fact's goroutine while the walk's own goroutine ends the launch and
		// connects the next one. The rig's steps may fail the test, which only its own goroutine may do.
		reached, done := make(chan struct{}), make(chan struct{})
		relaunchDone := sync.OnceFunc(func() { close(done) })
		t.Cleanup(relaunchDone)
		var acted atomic.Bool
		// read is the generation of the held fact's read of the implementer, which the hook writes
		// before it closes reached.
		var read uint64
		r.onRead(func(_ []claim.Token, reads workflow.FactReads) {
			key, _, ok := readOf(reads, implementer.token)
			if !ok || !acted.CompareAndSwap(false, true) {
				return
			}
			read = key.Generation
			close(reached)
			<-done
		})
		gone := r.d.claim(implementer.token).Generation
		r.publishReview(r.number, "CHANGES_REQUESTED")
		select {
		case <-reached:
		case <-time.After(stepRecordsWait):
			t.Fatalf("no fact read the calls of %s within %s of the request for changes", implementer.token, stepRecordsWait)
		}
		if read != gone {
			t.Fatalf("the held fact read launch %d of %s, want launch %d, the one about to exit", read, implementer.token, gone)
		}
		implementer.exit(false)
		implementer.relaunched()
		// The claim held no task when its launch exited. So the daemon tells the next launch what the
		// exit ended in a delivery of its own, before the held fact can send the task.
		if told := implementer.takeQuietly(); !(supervise.Delivery{ID: told.DeliveryID}).Relaunch() {
			t.Fatalf("the next launch of %s was first prompted with delivery %q, want the daemon's own delivery of what its relaunch ended", implementer.token, told.DeliveryID)
		}
		implementer.endTurn()
		relaunchDone()
		r.waitPhase(phase.Implementing)
		r.onRead(nil)
		implementer.take()
		r.finish(implementer, "implement", implementedRound, phase.Testing)
		again := r.waitModels(r.key, 2, 2, stepModels{result: "passed", setModel: stepRecordsModel, model: stepRecordsModel})
		// The control: the log names this step where it used a read, since its end used a read of the
		// next launch. Its start used none, since a read of a launch no longer live is not used.
		r.waitUsedRead("the read of "+string(implementer.token)+" that the end of "+again.ID+" used", func(line usedRead) bool {
			return line.Side == "end" && line.Step == again.ID && line.Claim == string(implementer.token) && line.Generation > gone
		})
		for _, line := range r.usedReads() {
			if line.Side == "start" && line.Step == again.ID {
				t.Fatalf("the start of %s used the read %+v, want none: the fact read only launch %d of %s, which had exited by its transaction", again.ID, line, gone, implementer.token)
			}
		}
	})
	// A person's done on the root lingers its tree, which stops a child's step. That fact reads the
	// root's agents and never the child's implementer, so the child's step ends with no end read.
	t.Run("a child's step stopped by a fact that reads only its root's agents", func(t *testing.T) {
		r := newStepRecordsRig(t, 1)
		child, number := r.seedChildInReviewing()
		own := r.tokenOf(child, claim.RoleImplementer)
		if _, err := r.st.AdmitClaim(context.Background(), supervise.Claim{
			Token: own, Project: r.project, Tree: r.key, Issue: child, Role: claim.RoleImplementer,
			Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
		}); err != nil {
			t.Fatalf("record the unfinished launch of %s's implementer: %v", child, err)
		}
		writePrompt(t, r.cfg, own)
		r.restart()
		childImpl := r.workerOf(child, claim.RoleImplementer)
		r.publishReview(number, "CHANGES_REQUESTED")
		r.waitPhaseOf(child, phase.Implementing)
		childImpl.take()
		r.personMoves(record.DoneStatus, "issue.closed", r.title)
		stopped := r.waitModels(child, 2, 1, stepModels{result: "stopped", setModel: stepRecordsModel, model: stepRecordsModel})
		// The control: the step's start used a read of the child's implementer. So the log does name
		// the record's id where the step used a read.
		r.waitUsedRead("the read of "+string(own)+" that the start of "+stopped.ID+" used", func(line usedRead) bool {
			return line.Side == "start" && line.Step == stopped.ID && line.Claim == string(own)
		})
		for _, line := range r.usedReads() {
			if line.Side == "end" && line.Step == stopped.ID {
				t.Fatalf("the end of %s, stopped by the root's done, used the read %+v, want none: that fact reads only the root's agents", stopped.ID, line)
			}
		}
	})
	// GitHub's merge ends the merge step while the merger's launch is still starting. A try of the
	// launch was written and its process may run, but no read of it can be made. So the merge's
	// record names no set model and does not know its counts.
	t.Run("a merge whose merger is still starting", func(t *testing.T) {
		r := newStepRecordsRig(t, 1)
		r.plan()
		r.implement()
		r.test()
		r.review("APPROVED", phase.Accepting)
		r.accept()
		release := r.holdSpawns()
		r.retro()
		merger := r.token(claim.RoleMerger)
		r.awaitLaunch(merger)
		r.github.merged(r.number)
		r.publishMergeOf(r.number, r.key, stepRecordsHead, "main")
		r.waitPhase(phase.Done)
		r.waitKindModels(r.key, "merge", 0, 1, stepModels{result: "passed"})
		// The control: the merge's end was written while the merger's launch was still starting.
		if state := r.d.claim(merger).State; state != string(supervise.StateLaunching) {
			t.Fatalf("%s is %s once the merge's record ended, want %s: the walk holds its Spawn", merger, state, supervise.StateLaunching)
		}
		release()
	})
}

// A step whose agent's launch exited and was made again adds the calls of both launches. The first
// launch's final totals count from the start's read, and the second launch counts from zero.
func TestAStepRecordAddsTheCallsOfTwoLaunches(t *testing.T) {
	t.Parallel()
	// The first launch exits with its final totals, one call past the start's read, and the next
	// launch answers once.
	t.Run("the final totals of one launch and the reads of the next", func(t *testing.T) {
		r := newStepRecordsRig(t, 1)
		implementer := implementAgain(r)
		implementer.take()
		implementer.exit(true)
		implementer.relaunched()
		implementer.take()
		r.finish(implementer, "implement", implementedRound, phase.Testing)
		r.waitModels(r.key, 2, 2, stepModels{result: "passed", setModel: stepRecordsModel,
			answers: answerList(answerOf(stepRecordsModel, 2)), model: stepRecordsModel})
	})
	// The next launch starts on another set model, which it answers with. The two launches name two
	// set models, so the record names none, and it counts one call of each model.
	t.Run("two launches on two set models", func(t *testing.T) {
		r := newStepRecordsRig(t, 1)
		implementer := implementAgain(r)
		implementer.take()
		implementer.exit(true)
		// serve reads the set model under the claimant's lock.
		implementer.mu.Lock()
		implementer.setModel = stepRecordsModelB
		implementer.mu.Unlock()
		implementer.model = stepRecordsModelB
		implementer.relaunched()
		implementer.take()
		r.finish(implementer, "implement", implementedRound, phase.Testing)
		r.waitModels(r.key, 2, 2, stepModels{result: "passed", setModel: "",
			answers: answerList(answerOf(stepRecordsModel, 1), answerOf(stepRecordsModelB, 1)), model: stepRecordsModelB})
	})
}

// The read a step's start uses is the newest the daemon holds of the launch. Two children of the
// root stand in reviewing, each on its own pull request, and the tree's architect owns both.
//
// A person approves the first, whose acceptance starts. Its acceptance hands the architect a task,
// whose confirmation reads the architect too, so the walk waits for that fact first. The approval
// of the second then reads the architect and is held before its transaction. Meanwhile the
// architect answers twice and signs the first off, and that sign-off's newer read is stored. The
// second acceptance then starts from the stored read, so it counts only the call of its own
// sign-off. Had it started from the held fact's own read, it would count three.
func TestAStoredReadNewerThanTheFactsReadIsTheOneUsed(t *testing.T) {
	t.Parallel()
	t.Run("a sign-off's newer read starts the acceptance whose fact read before it", func(t *testing.T) {
		r := newStepRecordsRig(t, 1)
		first := r.seedChildOn(r.number+1, "Amber lantern polish")
		second := r.seedChildOn(r.number+2, "Cobalt kettle repair")
		architect := r.worker(claim.RoleArchitect)
		worked := architect.workedAlone()
		r.publishReview(r.number+1, "APPROVED")
		r.waitPhaseOf(first, phase.Accepting)
		architect.awaitAlone(worked + 1)
		reached, release := make(chan struct{}), make(chan struct{})
		let := sync.OnceFunc(func() { close(release) })
		t.Cleanup(let)
		var holding atomic.Bool
		// heldRead is the number of the held fact's own read of the architect, which the hook writes
		// before it closes reached.
		var heldRead uint64
		r.onRead(func(_ []claim.Token, reads workflow.FactReads) {
			_, held, ok := readOf(reads, architect.token)
			if !ok || !holding.CompareAndSwap(false, true) {
				return
			}
			heldRead = held.Totals.Read
			close(reached)
			<-release
		})
		r.publishReview(r.number+2, "APPROVED")
		select {
		case <-reached:
		case <-time.After(stepRecordsWait):
			t.Fatalf("no fact read the calls of %s within %s of the approval of %s", architect.token, stepRecordsWait, second)
		}
		if got := r.d.state().Issues[second]; string(got.Phase) != string(phase.Reviewing) {
			t.Fatalf("%s stands in %q while the fact of its approval is held before its transaction, want reviewing", second, got.Phase)
		}
		architect.answer()
		r.signOff(first)
		let()
		r.waitPhaseOf(second, phase.Accepting)
		r.onRead(nil)
		r.signOff(second)
		accepted := r.waitModels(first, 5, 1, stepModels{result: "passed", setModel: stepRecordsModel,
			answers: answerList(answerOf(stepRecordsModel, 2)), model: stepRecordsModel})
		later := r.waitModels(second, 5, 1, stepModels{result: "passed", setModel: stepRecordsModel,
			answers: answerList(answerOf(stepRecordsModel, 1)), model: stepRecordsModel})
		// The read the second acceptance's start used is the one the first sign-off stored. A fact
		// that read again after the hold would use a read numbered past it.
		stored := r.waitUsedRead("the read of the architect that the end of "+accepted.ID+" used", func(line usedRead) bool {
			return line.Side == "end" && line.Step == accepted.ID && line.Claim == string(architect.token)
		})
		started := r.waitUsedRead("the read of the architect that the start of "+later.ID+" used", func(line usedRead) bool {
			return line.Side == "start" && line.Step == later.ID && line.Claim == string(architect.token)
		})
		if started.Read != stored.Read || started.Read <= heldRead {
			t.Fatalf("the start of %s used read %d of %s, want read %d, which the sign-off of %s stored, past the held fact's own read %d",
				later.ID, started.Read, architect.token, stored.Read, first, heldRead)
		}
	})
}

// A read of an agent's calls ends at its bound while the daemon's writer to that agent is held. The
// planner's shim reads 4096 bytes of a prompt just under a frame's limit and then reads nothing. The
// daemon's write of that prompt stays unfinished, and the completion's read waits behind it. Once
// the walk fires the read's bound on the held clock, the completion answers and planning ends with
// no counts.
func TestATotalsReadEndsAtItsBoundWhileItsAgentsWriterIsHeld(t *testing.T) {
	t.Parallel()
	t.Run("the completion answers once the read's bound fires", func(t *testing.T) {
		proof := newLiveProofOn(t, workflowNATS(t))
		// The prompt the held socket never takes must not end on the worker stream's own bound.
		proof.cfg.WorkerRPCTimeout = time.Hour
		r := newStepRecordsRigWith(t, proof, startScratchDispatchOn(t, scratchDispatchOptions{}), 1)
		clock := holdReadClock(r)
		planner := r.worker(claim.RolePlanner)
		planner.take()
		planner.holdTheSocket()
		r.built.mu.Lock()
		conn, ok := r.built.conns.Conn(planner.token)
		r.built.mu.Unlock()
		if !ok {
			t.Fatalf("the daemon holds no connection of %s", planner.token)
		}
		asked, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		_, err := conn.GetState(asked)
		cancel()
		if err != nil {
			t.Fatalf("get_state on the connection of %s: %v", planner.token, err)
		}
		delivery := "held-writer-" + randomSuffix(t)[:6]
		message := strings.Repeat("x", shimwire.MaxFrameBytes-4096)
		if size, err := shimwire.FrameLen(shimwire.Prompt{ID: delivery, DeliveryID: delivery, Message: message}); err != nil || size >= shimwire.MaxFrameBytes {
			t.Fatalf("the prompt's frame is %d bytes (%v), want one under the limit of %d", size, err, shimwire.MaxFrameBytes)
		}
		ctx := t.Context()
		go func() { _ = conn.Prompt(ctx, delivery, message) }()
		planner.mu.Lock()
		held := planner.held
		planner.mu.Unlock()
		select {
		case <-held:
		case <-time.After(stepRecordsWait):
			t.Fatalf("the shim of %s never read the head of the prompt within %s", planner.token, stepRecordsWait)
		}
		completeAtTheBound(r, clock, planner, "plan", plannedRound, "the daemon's writer to the planner was held behind a prompt the planner does not read")
		if read := r.waitEmptyRead(planner.token, "no answer within the bound"); read.Waited != streamDrainBound {
			t.Fatalf("the read of %s behind the held writer waited %s on the held clock, want exactly the bound, %s", planner.token, read.Waited, streamDrainBound)
		}
		r.waitSteps("the end of planning past the held writer", func(read stepRecordsRead) error {
			switch got := read.find("step", 1, 1); {
			case got.ID == "" || got.EndedAt == nil || got.Result != "passed":
				return fmt.Errorf("planning reads %+v, want it ended passed", got)
			case got.Answers != nil || got.Model != stepRecordsModel:
				return fmt.Errorf("planning names answers %s and model %q, want null and %s", answersText(got.Answers), got.Model, stepRecordsModel)
			}
			return nil
		})
	})
}

// implementAgain walks the issue to its implementer's second run. The planner plans, the
// implementer answers five times and completes, the tester passes and a review asks for changes.
// It answers the implementer, whose second run's start used a read of its live launch.
func implementAgain(r *stepRecordsRig) *stepClaimant {
	r.t.Helper()
	r.plan()
	implementer := r.worker(claim.RoleImplementer)
	implementer.take()
	for range 4 {
		implementer.answer()
	}
	r.finish(implementer, "implement", implementedRound, phase.Testing)
	r.test()
	r.review("CHANGES_REQUESTED", phase.Implementing)
	five := []shimwire.ModelCount{{Model: stepRecordsModel, Calls: 5}}
	r.waitUsedRead("the read of the implementer's five calls that its second run's start used", func(line usedRead) bool {
		return line.Side == "start" && line.Claim == string(implementer.token) && line.Place == 2 && slices.Equal(line.Counts, five)
	})
	return implementer
}

// twoModelsAnswer is the planner taking its task and answering stepRecordsModel three times in all,
// then stepRecordsModelB once. It answers the planner.
func twoModelsAnswer(r *stepRecordsRig) *stepClaimant {
	r.t.Helper()
	planner := r.worker(claim.RolePlanner)
	planner.take()
	planner.answer()
	planner.answer()
	planner.model = stepRecordsModelB
	planner.answer()
	return planner
}

// twoModelsCounted checks planning once twoModelsAnswer's calls end it with result. It names set
// model stepRecordsModel, three calls of it and one of stepRecordsModelB, which answered last. The
// read its end used counts the same calls, and the fact that ended it is endedBy.
func twoModelsCounted(r *stepRecordsRig, planner *stepClaimant, result, endedBy string) {
	r.t.Helper()
	planning := r.waitModels(r.key, 1, 1, stepModels{result: result, setModel: stepRecordsModel,
		answers: answerList(answerOf(stepRecordsModel, 3), answerOf(stepRecordsModelB, 1)), model: stepRecordsModelB})
	end := r.waitUsedRead("the read of the planner that the end of "+planning.ID+" used", func(line usedRead) bool {
		return line.Side == "end" && line.Claim == string(planner.token) && line.Step == planning.ID
	})
	want := []shimwire.ModelCount{{Model: stepRecordsModel, Calls: 3}, {Model: stepRecordsModelB, Calls: 1}}
	if end.EndedBy != endedBy || !slices.Equal(end.Counts, want) {
		r.t.Fatalf("the end of planning used the read %+v, want one ended by %s counting %v", end, endedBy, want)
	}
}

// holdReadClock puts r's reads of its agents' calls on a held clock, whose time stands still from
// now. A read's bound then fires only when the walk fires it.
func holdReadClock(r *stepRecordsRig) *heldClock {
	clock := &heldClock{}
	r.readClock.use(clock)
	clock.advance(0)
	return clock
}

// stopTheTask is the tree's architect stopping the walk's task default through its own route
// (stop_task, as stopAtTesting does), which must answer 200.
func stopTheTask(r *stepRecordsRig) {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/tasks/stop", map[string]any{
		"grantId": r.worker(claim.RoleArchitect).grant(), "issue": r.key, "task": "default"}, false)
	if status != http.StatusOK {
		r.t.Fatalf("the architect's stop of %s's task default = %d %s, want 200", r.key, status, body)
	}
}

// completeAtTheBound sends c's completion req, written as finish writes it, while no read of c's
// calls can be answered. The completion must wait until its read's bound is armed on clock. Then
// the walk moves clock past the bound and fires it, and the completion must answer 200 within 30 s.
// held says why the read cannot be answered, for the failure.
func completeAtTheBound(r *stepRecordsRig, clock *heldClock, c *stepClaimant, word string, req api.HandoffCompleteRequest, held string) {
	t := r.t
	t.Helper()
	req.HandoffID, req.HandoffVersion = r.writeHandoff(c.session, word)
	req.GrantID = c.grant()
	if !testwait.Holds(stepRecordsWait, 20*time.Millisecond, func() bool { return readBoundsArmed(clock) == 0 }) {
		t.Fatalf("a bound of an earlier read stays armed before the %s's completion", c.role)
	}
	answered := postAside(r, "/legion/v1/handoff/complete", req)
	if !testwait.Holds(stepRecordsWait, 20*time.Millisecond, func() bool { return readBoundsArmed(clock) > 0 }) {
		t.Fatalf("the %s's completion armed no bound for its read of the %s's calls within %s", c.role, c.role, stepRecordsWait)
	}
	select {
	case got := <-answered:
		t.Fatalf("the %s's completion answered %d %s (%v) before its read's bound fired, though %s", c.role, got.status, got.body, got.err, held)
	default:
	}
	clock.advance(streamDrainBound)
	if fired := clock.fire(streamDrainBound); fired == 0 {
		t.Fatalf("no bound of a read was armed when the walk fired it, though %s", held)
	}
	select {
	case got := <-answered:
		if got.err != nil || got.status != http.StatusOK {
			t.Fatalf("the %s's completion after its read's bound fired = %d %s (%v), want 200", c.role, got.status, got.body, got.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the %s's completion did not answer within 30 s after the read's bound fired, while %s", c.role, held)
	}
}

// asideAnswer is the answer to a request sent on a goroutine of its own (postAside): its status and
// body, or the error the request met.
type asideAnswer struct {
	status int
	body   []byte
	err    error
}

// postAside posts body to path on r's daemon from a goroutine of its own, and answers the channel
// its answer arrives on. Its client waits up to stepRecordsWait, since a walk holds such a request
// longer than the daemon client's own bound.
func postAside(r *stepRecordsRig, path string, body any) <-chan asideAnswer {
	r.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		r.t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, r.d.base+path, bytes.NewReader(encoded))
	if err != nil {
		r.t.Fatal(err)
	}
	client := &http.Client{Transport: r.d.transport, Timeout: stepRecordsWait}
	answered := make(chan asideAnswer, 1)
	go func() {
		response, err := client.Do(request)
		if err != nil {
			answered <- asideAnswer{err: err}
			return
		}
		defer response.Body.Close()
		read, err := io.ReadAll(response.Body)
		answered <- asideAnswer{status: response.StatusCode, body: read, err: err}
	}()
	return answered
}

// readBoundsArmed is how many bounds of a read of an agent's calls stand armed on clock now.
func readBoundsArmed(clock *heldClock) int {
	live, _ := clock.armed(streamDrainBound)
	return live
}

// readOf is the read reads hold of a launch of token's claim, and false where they hold none. A
// fact reads one launch of each claim, the one its machine runs (readTotals).
func readOf(reads workflow.FactReads, token claim.Token) (record.LaunchKey, record.LaunchRead, bool) {
	for key, read := range reads {
		if key.Claim == token {
			return key, read, true
		}
	}
	return record.LaunchKey{}, record.LaunchRead{}, false
}

// stepModels is what a record of a step must say of its models. It holds the record's result, its
// set model, the calls of each model and the model that answered last.
type stepModels struct {
	result, setModel string
	// answers is the list the record must name, in order, and nil where it must not know the calls.
	answers *[]stepAnswerRead
	model   string
}

// waitModels waits for issue's record of step place, attempt attempt, to end saying want, and
// answers the record. The failure names the last difference the read showed.
func (r *stepRecordsRig) waitModels(issue string, place, attempt int, want stepModels) stepRecordRead {
	r.t.Helper()
	return r.waitKindModels(issue, "step", place, attempt, want)
}

// waitKindModels is waitModels for issue's record of kind: a step, a retro or a merge.
func (r *stepRecordsRig) waitKindModels(issue, kind string, place, attempt int, want stepModels) stepRecordRead {
	r.t.Helper()
	var found stepRecordRead
	r.waitStepsOf(issue, fmt.Sprintf("the models of %s %d attempt %d of %s", kind, place, attempt, issue), func(read stepRecordsRead) error {
		found = read.find(kind, place, attempt)
		return modelsHold(found, want)
	})
	return found
}

// modelsHold says whether got ended saying want, and names the first difference.
func modelsHold(got stepRecordRead, want stepModels) error {
	switch {
	case got.ID == "":
		return errors.New("the read holds no such record")
	case got.EndedAt == nil || got.Result != want.result:
		return fmt.Errorf("record %s ended %v with result %q, want it ended %s", got.ID, got.EndedAt, got.Result, want.result)
	case got.SetModel != want.setModel || got.Model != want.model || !sameAnswers(got.Answers, want.answers):
		return fmt.Errorf("record %s names set model %q, answers %s and model %q, want %q, %s and %q",
			got.ID, got.SetModel, answersText(got.Answers), got.Model, want.setModel, answersText(want.answers), want.model)
	}
	return nil
}

// sameAnswers says whether two lists of answers are both null, or both lists of the same answers in
// the same order.
func sameAnswers(got, want *[]stepAnswerRead) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return slices.Equal(*got, *want)
}

// answersText is answers as the read carries them: null, or the list in its order.
func answersText(answers *[]stepAnswerRead) string {
	encoded, err := json.Marshal(answers)
	if err != nil {
		return fmt.Sprint(answers)
	}
	return string(encoded)
}

// answerList is the list of answers a record must name, in order. With no answer it is an empty
// list, which a record that knows it had no call names, not null.
func answerList(answers ...stepAnswerRead) *[]stepAnswerRead {
	list := append([]stepAnswerRead{}, answers...)
	return &list
}

// answerOf is one answer of a record: model and the calls it answered.
func answerOf(model string, calls int64) stepAnswerRead {
	return stepAnswerRead{Model: model, Calls: calls}
}

// waitUsedRead waits for the daemon's log to hold a line of a read a step used that satisfies
// holds, and answers the first such line. The failure names what each of those lines says.
func (r *stepRecordsRig) waitUsedRead(what string, holds func(usedRead) bool) usedRead {
	r.t.Helper()
	var found usedRead
	waitRead(r, what, "the daemon's log", 50*time.Millisecond, func() ([]usedRead, error) { return r.usedReads(), nil }, func(lines []usedRead) error {
		i := slices.IndexFunc(lines, holds)
		if i < 0 {
			return fmt.Errorf("the reads its steps used are %+v", lines)
		}
		found = lines[i]
		return nil
	})
	return found
}

// waitEmptyRead waits for the daemon's log to say that a read of token's calls gave no totals for
// reason, and answers the first such line. The failure names each such line the log holds.
func (r *stepRecordsRig) waitEmptyRead(token claim.Token, reason string) emptyRead {
	r.t.Helper()
	var found emptyRead
	what := fmt.Sprintf("a read of %s that gave no totals for %q", token, reason)
	waitRead(r, what, "the daemon's log", 50*time.Millisecond, func() ([]emptyRead, error) { return r.emptyReads(), nil }, func(lines []emptyRead) error {
		i := slices.IndexFunc(lines, func(line emptyRead) bool { return line.Claim == string(token) && line.Reason == reason })
		if i < 0 {
			return fmt.Errorf("the reads that gave none are %+v", lines)
		}
		found = lines[i]
		return nil
	})
	return found
}
