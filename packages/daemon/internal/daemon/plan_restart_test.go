// A restart of the daemon in the middle of an owner's plan: a step that had started goes on in the
// run its start began, and a boot resumes neither a stopped task's step nor a parent held for its
// children. Each test boots the whole daemon over the real Dispatch of this package's tests, walks
// an issue from its owner's plan (ownerStepsRig, owner_steps_rig_test.go), stops the daemon and
// boots a second one on the same store, the same Dispatch and the same ports, each pane outliving
// the daemon as a tmux pane does (stepRecordsRig's restartWith).
package daemon

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The daemon restarts between the start of a step and its completion. An issue's
// owner gives the plan `implementer reviewer`, and the implementer starts: one launch, one start of
// task a's step 1 (step_starts) and its start record. The daemon stops and a second one boots on the
// same store: it launches no implementer again, raises no new entry, counts no second start, and the
// record is the same one. The implementer completes, under the key that names task a and pass 0;
// the reviewer starts once, and the implementer's record ends passed under the id its start wrote.
// The control deletes the issue's plan rows before the second boot, and the same check must fail:
// the issue then walks today's line, where implementing is the task default's step 2, and the
// second boot opens that run, which no start of this release counted (workflow's
// OpenUncountedRuns), beside task a's.
func TestARestartBetweenAStepsStartAndItsCompletion(t *testing.T) {
	rig := func(t *testing.T) *ownerStepsRig {
		return newOwnerStepsRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), 1, config.Project{})
	}
	t.Run("the restart", func(t *testing.T) {
		r := rig(t)
		if err := r.restartBetweenStartAndCompletion(func() {}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("control: the plan rows deleted before the second boot", func(t *testing.T) {
		r := rig(t)
		err := r.restartBetweenStartAndCompletion(r.deletePlanRows)
		if err == nil {
			t.Fatalf("the restart of %s with its plan rows deleted passed the check of the plan implementer reviewer", r.key)
		}
		if want := "default step 2 starts 0 run open"; !strings.Contains(err.Error(), want) {
			t.Fatalf("the check of %s with its plan rows deleted says %q, want it to name %s", r.key, err, want)
		}
		t.Logf("control: the check of %s with its plan rows deleted fails: %v", r.key, err)
	})
}

// restartBetweenStartAndCompletion is the walk of the test above, with between run on the store
// while no daemon runs. A fault of the daemon fails the test; an answer the check does not hold is
// the error.
func (r *ownerStepsRig) restartBetweenStartAndCompletion(between func()) error {
	t := r.t
	t.Helper()
	r.givePlan("implementer", "reviewer")
	r.openGate(phase.Implementing)
	implementer := r.worker(claim.RoleImplementer)
	implementer.take()
	var started stepRecordRead
	r.waitSteps("the start of task a's step 1", func(got stepRecordsRead) error {
		started = got.find("step", 1, 1)
		if started.ID == "" || started.Task != "a" || started.Role != "implementer" || started.StartedAt == nil || started.EndedAt != nil {
			return fmt.Errorf("task a's step 1 reads %+v, want the implementer's run started and not ended", started)
		}
		return nil
	})
	oneStart := []string{"a step 1 starts 1 run open"}
	if launches, starts := r.launchesOfRole(claim.RoleImplementer), r.stepStarts(); len(launches) != 1 || !reflect.DeepEqual(starts, oneStart) {
		t.Fatalf("before the restart %s's implementer was launched %v and its step starts read %v, want one launch and %v", r.key, launches, starts, oneStart)
	}
	entry := r.phaseOf(claim.RoleImplementer).Entry

	r.restartWith(between)
	// The boot's own resume by rule has run once its fact is applied, and a start it would make of
	// the implementer is a launch of the second daemon's runtime, which is watched for a while.
	r.waitBoots(2)
	for deadline := time.Now().Add(liveSettle); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if launches := r.launchesOfRole(claim.RoleImplementer); len(launches) != 0 {
			return fmt.Errorf("the second boot launched %s's implementer again (%v), want no launch of a step whose run had started", r.key, launches)
		}
	}
	if starts := r.stepStarts(); !reflect.DeepEqual(starts, oneStart) {
		return fmt.Errorf("after the restart %s's step starts read %v, want %v", r.key, starts, oneStart)
	}
	if after := r.phaseOf(claim.RoleImplementer).Entry; after != entry {
		return fmt.Errorf("after the restart %s's implementer is on entry %d, want %d, the entry its start raised", r.key, after, entry)
	}

	r.github.open("legion/"+r.key, r.number)
	handoff, version := r.writeHandoff(implementer.session, "implement")
	if status, body := r.complete(implementer, api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead,
		HandoffID: handoff, HandoffVersion: version}); status != http.StatusOK {
		t.Fatalf("the implementer's completion after the restart = %d %s, want 200", status, body)
	}
	implementer.endTurn()
	var stands string
	testwait.WithinPolling(t, stepRecordsWait, 100*time.Millisecond, r.key+" to leave implementing", func() bool {
		stands = string(r.d.state().Issues[r.key].Phase)
		return stands != string(phase.Implementing)
	})
	if stands != string(phase.Reviewing) {
		return fmt.Errorf("after the implementer's completion %s stands in %s, want reviewing, the step after the implementer in its owner's plan", r.key, stands)
	}
	r.awaitLaunch(r.token(claim.RoleReviewer))
	if launches := r.launchesOfRole(claim.RoleReviewer); len(launches) != 1 {
		return fmt.Errorf("%s's reviewer was launched %v, want once", r.key, launches)
	}
	if testers := r.launchesOfRole(claim.RoleTester); len(testers) != 0 {
		return fmt.Errorf("%s's tester was launched %v, want none: the plan names no tester", r.key, testers)
	}
	r.waitSteps("the end of task a's step 1 under the id its start wrote", func(got stepRecordsRead) error {
		if ended := got.find("step", 1, 1); ended.ID != started.ID || ended.EndedAt == nil || ended.Result != "passed" {
			return fmt.Errorf("task a's step 1 reads %+v, want %s ended passed", ended, started.ID)
		}
		return nil
	})
	var keyed int
	if err := r.st.Pool().QueryRow(context.Background(), `select count(*) from processed_events where source = 'api' and event_id like $1`,
		"handoff:"+r.key+":%:implementing:a:0:%").Scan(&keyed); err != nil {
		t.Fatal(err)
	}
	if keyed != 1 {
		return fmt.Errorf("the record holds %d completions of %s's implementing keyed with task a and pass 0, want 1", keyed, r.key)
	}
	return nil
}

// waitBoots waits for the record to hold count boots' own facts as applied (intake.DaemonBooted):
// each is a boot's resume by rule and its admission pass.
func (r *ownerStepsRig) waitBoots(count int) {
	r.t.Helper()
	var applied int
	testwait.WithinPolling(r.t, stepRecordsWait, 100*time.Millisecond, fmt.Sprintf("%d boots of the daemon to apply their facts", count), func() bool {
		if err := r.st.Pool().QueryRow(context.Background(), `select count(*) from processed_events where source = 'daemon' and event_id like $1`,
			"boot:"+r.cfg.Project+":%").Scan(&applied); err != nil {
			r.t.Fatalf("count the boots applied: %v", err)
		}
		return applied >= count
	})
}

// The real boot: a task stopped at testing, whose tester the outbox has suspended
// (step_stop), and a parent held before its retro for the child beneath it. The daemon stops and a
// second one boots on the same store: once its boot has applied its resume by rule, neither issue
// has a start queued or a launch, the tester is still suspended by the daemon, and each hold stands.
// The control clears the stopped task's hold with SQL before the second boot, and the boot's resume
// by rule then starts the tester again.
func TestABootResumesNoStoppedStepAndNoHeldParent(t *testing.T) {
	t.Run("the boot", func(t *testing.T) {
		r := newOwnerStepsRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), 2, config.Project{})
		stopped := r.stopAtTesting()
		r.on(r.issues[1].key)
		parent := r.holdForChildren()
		r.restartWith(func() {})
		r.waitBoots(2)
		for deadline := time.Now().Add(liveSettle); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			for _, key := range []string{stopped, parent} {
				if launches := r.issueLaunches(key); len(launches) != 0 {
					t.Fatalf("the second boot launched %v on %s, want nothing: it is held", launches, key)
				}
				if starts := r.startsQueued(key); starts != 0 {
					t.Fatalf("the second boot queued %d starts on %s, want none: it is held", starts, key)
				}
			}
		}
		r.holds(stopped, record.HoldTaskStopped)
		r.holds(parent, record.HoldForChildren)
		if tester := r.d.claim(r.tokenOf(stopped, claim.RoleTester)); tester.State != string(supervise.StateSuspended) || tester.SuspendedBy != string(supervise.SuspendedByDaemon) {
			t.Fatalf("after the second boot %s's tester is %s by %q, want suspended by the daemon", stopped, tester.State, tester.SuspendedBy)
		}
	})

	t.Run("control: the hold cleared by SQL before the second boot", func(t *testing.T) {
		r := newOwnerStepsRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), 1, config.Project{})
		stopped := r.stopAtTesting()
		r.restartWith(func() {
			r.exec("update issues set phase = 'testing', held_from = null where key = $1", stopped)
		})
		r.waitBoots(2)
		r.awaitLaunch(r.tokenOf(stopped, claim.RoleTester))
		t.Logf("control: with the hold cleared, the second boot launched %s's tester: %v", stopped, r.launchesOfRole(claim.RoleTester))
	})
}

// stopAtTesting walks the walk's issue on the plan `implementer tester` to testing, has its tester
// take its task, and has its owner stop task a there (stop_task). Once the tester's turn ends, the
// outbox's step_stop suspends it, by the daemon, and the issue stands held from testing. The
// tester's pane has stopped, so the walk forgets its claimant. It answers the issue's key.
func (r *ownerStepsRig) stopAtTesting() string {
	t := r.t
	t.Helper()
	r.givePlan("implementer", "tester")
	r.openGate(phase.Implementing)
	r.runStep("implementer", answerPass, phase.Testing)
	tester := r.worker(claim.RoleTester)
	tester.take()
	status, body := r.d.request(http.MethodPost, "/legion/v1/tasks/stop", map[string]any{
		"grantId": r.worker(claim.RoleArchitect).grant(), "issue": r.key, "task": "a"}, false)
	if status != http.StatusOK {
		t.Fatalf("stop_task of %s's task a = %d %s, want 200", r.key, status, body)
	}
	tester.endTurn()
	var last api.OperatorClaim
	testwait.WithinPolling(t, stepRecordsWait, 100*time.Millisecond, "the step_stop of "+r.key+"'s tester", func() bool {
		last = r.d.claim(tester.token)
		return last.State == string(supervise.StateSuspended) && last.SuspendedBy == string(supervise.SuspendedByDaemon)
	})
	r.holds(r.key, record.HoldTaskStopped)
	delete(r.claims, tester.token)
	return r.key
}

// holdForChildren walks the walk's issue on the plan `implementer reviewer`, with a child beneath it
// in reviewing (seedChildInReviewing), through its implementer and an approving review: it passes
// its last step, its plan has no acceptance, and the child is still in the workflow, so Legion holds
// it before its retro. It answers the issue's key.
func (r *ownerStepsRig) holdForChildren() string {
	r.t.Helper()
	r.givePlan("implementer", "reviewer")
	r.openGate(phase.Implementing)
	r.seedChildInReviewing()
	r.runStep("implementer", answerPass, phase.Reviewing)
	r.runStep("reviewer", answerPass, phase.Held)
	r.holds(r.key, record.HoldForChildren)
	return r.key
}

// holds fails the test unless key stands held for reason.
func (r *ownerStepsRig) holds(key string, reason record.HoldReason) {
	r.t.Helper()
	if got := r.d.state().Issues[key]; string(got.Phase) != string(phase.Held) || got.HoldReason != string(reason) {
		r.t.Fatalf("%s stands in %s held for %q, want held for %q", key, got.Phase, got.HoldReason, reason)
	}
}

// issueLaunches is every launch of a claim of key the daemon running now has made.
func (r *ownerStepsRig) issueLaunches(key string) []string {
	var launches []string
	for _, call := range r.rt.Calls() {
		if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Issue == key {
			launches = append(launches, call.Method+" "+string(call.Spec.Role))
		}
	}
	return slices.Clip(launches)
}

// startsQueued is how many supervise starts the daemon's outbox holds for key.
func (r *ownerStepsRig) startsQueued(key string) int {
	r.t.Helper()
	var count int
	if err := r.st.Pool().QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start'`, key).Scan(&count); err != nil {
		r.t.Fatalf("count the starts queued for %s: %v", key, err)
	}
	return count
}
