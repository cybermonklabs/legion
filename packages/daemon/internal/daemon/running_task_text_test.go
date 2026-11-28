// A later version of an owner's plan that changes only the text of a running task, through the
// whole daemon over the real Dispatch of this package's tests (ownerStepsRig,
// owner_steps_rig_test.go): Legion takes it, the task goes on where it stands, and every start
// task Legion sends for the task afterwards holds the new text, while the agent at work is sent
// nothing for the version.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// textPlan is a plan.md with one task block, id, whose steps attribute names steps (none for a
// block that names no steps) and whose text is text.
func textPlan(id, steps, text string) string {
	if steps == "" {
		return ":::plan_task{#" + id + "}\n" + text + "\n:::\n"
	}
	return ":::plan_task{#" + id + " steps=\"" + steps + "\"}\n" + text + "\n:::\n"
}

// editPlan is a person writing a later version of the document artifact through Dispatch's edit
// route with the one operation op, and fails unless Dispatch stored version want.
func (r *ownerStepsRig) editPlan(artifact string, op map[string]any, want int) {
	r.t.Helper()
	var edited struct {
		Version *struct {
			Number int `json:"number"`
		} `json:"version"`
	}
	r.person(http.MethodPost, "/api/v1/artifacts/"+artifact+"/edits", map[string]any{"ops": []map[string]any{op}}, http.StatusOK, &edited)
	if edited.Version == nil || edited.Version.Number != want {
		r.t.Fatalf("the edit of %s stored version %+v, want version %d", artifact, edited.Version, want)
	}
}

// takeText is c taking the daemon's next task, as take does, and answers that task's text.
func takeText(c *stepClaimant) string {
	c.r.t.Helper()
	prompt := c.receive()
	c.answer()
	c.send(shimwire.AgentStart{DeliveryID: prompt.DeliveryID})
	c.waitState(supervise.StateWorking)
	return prompt.Message
}

// holdsText fails the test unless task holds want and not was.
func holdsText(t *testing.T, who, task, want, was string) {
	t.Helper()
	if !strings.Contains(task, "\n"+want+"\n") || strings.Contains(task, was) {
		t.Fatalf("the %s's start task = %q,\nwant it to hold %q and not %q", who, task, want, was)
	}
}

// runningAct is one step of a running task as register_plan's answer names it (`running`), decoded
// here rather than as api.RunningStep, so this file builds on a daemon that answers none.
type runningAct struct {
	Kind  string `json:"kind"`
	Place int    `json:"place"`
	Role  string `json:"role"`
}

// registeredRunning is register_plan's answer of artifact at version, which must be 200, and the
// `running` of each task of it, the act a running task stands at, by task: nil for a task that does
// not run.
func (r *ownerStepsRig) registeredRunning(artifact string, version int) map[string][]runningAct {
	r.t.Helper()
	status, body := r.registerPlan(artifact, version)
	if status != http.StatusOK {
		r.t.Fatalf("register_plan of %s's version %d = %d %s, want 200", r.key, version, status, body)
	}
	var taken struct {
		Plan struct {
			Tasks []struct {
				ID      string       `json:"id"`
				Running []runningAct `json:"running"`
			} `json:"tasks"`
		} `json:"plan"`
	}
	if err := json.Unmarshal([]byte(body), &taken); err != nil {
		r.t.Fatalf("decode register_plan's answer %s: %v", body, err)
	}
	running := map[string][]runningAct{}
	for _, task := range taken.Plan.Tasks {
		running[task.ID] = task.Running
	}
	return running
}

// taskRow is where task a of the walk's issue stands in the daemon's store, with its counts.
func (r *ownerStepsRig) taskRow(task string) string {
	r.t.Helper()
	var state, kind, moves, capTold string
	var position, pass, rounds, conflicts int
	if err := r.st.Pool().QueryRow(context.Background(), `select state, running_kind, position, pass, rounds, conflict_rounds,
		backward_moves::text, cap_told::text from plan_tasks where issue = $1 and task = $2`, r.key, task).
		Scan(&state, &kind, &position, &pass, &rounds, &conflicts, &moves, &capTold); err != nil {
		r.t.Fatalf("read task %s of %s: %v", task, r.key, err)
	}
	return fmt.Sprintf("%s at %s %d, pass %d, rounds %d, conflict rounds %d, backward moves %s, cap told %s",
		state, kind, position, pass, rounds, conflicts, moves, capTold)
}

// entryTask is the stored start task of role's entry on the walk's issue (record.PhaseRow.EntryTask),
// which the send of an owed entry sends again.
func (r *ownerStepsRig) entryTask(role claim.Role) string {
	r.t.Helper()
	var task string
	if err := r.st.Pool().QueryRow(context.Background(), `select entry_task from phases where issue = $1 and role = $2`,
		r.key, string(role)).Scan(&task); err != nil {
		r.t.Fatalf("read the %s's stored start task of %s: %v", role, r.key, err)
	}
	return task
}

// launchCount is how many launches of method the runtime has made of token.
func (r *ownerStepsRig) launchCount(token claim.Token, method string) int {
	count := 0
	for _, call := range r.rt.CallsOf(method) {
		if call.Spec.Claim == token {
			count++
		}
	}
	return count
}

// processEnds is the end of c's process, which the runtime reports gone, and the wait for the
// daemon's next launch of its claim of method (Spawn or Resume).
func (r *ownerStepsRig) processEnds(c *stepClaimant, method string) {
	r.t.Helper()
	held := r.d.claim(c.token)
	if held.Locator == nil {
		r.t.Fatalf("%s records no process", c.token)
	}
	before := r.launchCount(c.token, method)
	// The pane's connection closes with its process, as reattach closes it; the rig's cleanup ends
	// the shim it handed the claimant.
	c.mu.Lock()
	_ = c.sh.conn.Close()
	c.mu.Unlock()
	r.rt.Emit(runtime.Observation{Locator: *held.Locator, Kind: runtime.Gone, At: time.Now(), Detail: "the driver ended the process"})
	testwait.WithinPolling(r.t, stepRecordsWait, 50*time.Millisecond, fmt.Sprintf("a %s of %s", method, c.token), func() bool {
		return r.launchCount(c.token, method) > before
	})
}

// Check c-text-change. Issue A runs its owner's task a, `implementer reviewer`, with the text
// "Text one". While the implementer works, the owner gives version 2, whose one change is the text
// "Text two": register_plan answers 200 and names step 1, the implementer; task a keeps its state,
// its place and its counts; and the implementer is sent nothing. Every start task Legion sends for
// task a after version 2 holds "Text two": the send of the implementer's stored start task, which
// still holds "Text one" as its start wrote it, does. The implementer's session is then lost, and
// the fresh session's first task holds "Text two". The process ends again and resumes
// its session, and the task sent to it again holds "Text two". So do the first reviewer's task, the
// implementer's after the review's return, the second reviewer's and the retro's, and none holds
// "Text one". While the second reviewer works, version 3 changes the steps of a, and Legion refuses
// it PLAN_REFUSED, naming task a and saying it runs. The step records count the starts of a run
// with no version 2. Issue B runs the project's default steps, and while its planner works the
// owner gives a plan.md whose one block, default, names no steps and holds "Text three":
// register_plan answers 200, and B's implementer's task holds "Text three" and the default steps.
func TestALaterVersionChangesTheTextOfARunningTaskForEveryLaterStart(t *testing.T) {
	t.Parallel()
	r := newOwnerStepsRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), 2, config.Project{})
	const one, two, three = "Text one", "Text two", "Text three"
	plan := r.document("plan.md", textPlan("a", "implementer reviewer", one))
	if status, body := r.registerPlan(plan, 1); status != http.StatusOK {
		t.Fatalf("register_plan of %s's version 1 = %d %s, want 200", r.key, status, body)
	}
	r.openGate(phase.Implementing)
	implementer := r.worker(claim.RoleImplementer)
	holdsText(t, "implementer", takeText(implementer), one, two)
	before := r.taskRow("a")

	r.editPlan(plan, map[string]any{"op": "replace", "find": one, "with": two}, 2)
	running := r.registeredRunning(plan, 2)
	if got, want := running["a"], []runningAct{{Kind: "step", Place: 1, Role: string(claim.RoleImplementer)}}; !slices.Equal(got, want) {
		t.Fatalf("register_plan of version 2 names task a running at %+v, want %+v: step 1, the implementer", got, want)
	}
	if after := r.taskRow("a"); after != before {
		t.Fatalf("task a after version 2 is %s, want it as it was: %s", after, before)
	}
	select {
	case prompt := <-implementer.prompts:
		t.Fatalf("the implementer at work was sent %q for version 2, want nothing", prompt.Message)
	case <-time.After(liveSettle):
	}
	// Nothing is written for version 2: the stored start task of the implementer's entry is the one
	// its start wrote, with "Text one", and every send of it carries "Text two", by the daemon's
	// send-time read of a held task (Deps.TaskText, taskText), which the send of an owed entry passes
	// through as every start does. This is where the check's second control fails: with the send's
	// rewrite taken out, the send of the stored task, and the fresh session's start below, carry
	// "Text one".
	stored := r.entryTask(claim.RoleImplementer)
	holdsText(t, "stored implementer", stored, one, two)
	sender := &workflowRuntime{pool: r.st.Pool(), records: r.records}
	sent, _, err := sender.taskText(context.Background(), supervise.Claim{Token: implementer.token, Issue: r.key},
		supervise.Delivery{Task: stored, Phase: phase.Implementing, Generation: 1})
	if err != nil {
		t.Fatalf("the send-time read of the implementer's stored start task: %v", err)
	}
	holdsText(t, "sent stored implementer", sent, two, one)

	// The implementer's session is lost: no store holds it, so the relaunch spawns a fresh agent.
	r.rt.ScriptResume(fake.SpawnResult{Err: fmt.Errorf("resume %s: %w: the session table holds no such session",
		implementer.token, runtime.ErrSessionFileMissing)})
	r.processEnds(implementer, "Spawn")
	if spawn := lastLaunch(t, r.rt, implementer.token); spawn.ResumeSessionFile != "" {
		t.Fatalf("the implementer's relaunch resumed %s, want a fresh session", spawn.ResumeSessionFile)
	}
	delete(r.claims, implementer.token)
	fresh := r.worker(claim.RoleImplementer)
	holdsText(t, "fresh implementer", takeText(fresh), two, one)

	// Its process ends again and resumes its own session: the task is sent to it again.
	r.processEnds(fresh, "Resume")
	r.reattach(fresh)
	holdsText(t, "resumed implementer", takeText(fresh), two, one)
	if !r.github.opened(r.number) {
		r.github.open("legion/"+r.key, r.number)
	}
	r.finish(fresh, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}, phase.Reviewing)

	reviewer := r.worker(claim.RoleReviewer)
	holdsText(t, "first reviewer", takeText(reviewer), two, one)
	reviewer.endTurn()
	r.publishReview(r.number, "CHANGES_REQUESTED")
	r.waitPhase(phase.Implementing)

	holdsText(t, "returned implementer", takeText(fresh), two, one)
	r.finish(fresh, "implement", api.HandoffCompleteRequest{Summary: "implemented again", Commit: stepRecordsHead}, phase.Reviewing)
	holdsText(t, "second reviewer", takeText(reviewer), two, one)

	r.editPlan(plan, map[string]any{"op": "retype", "block": "a", "type": "plan_task",
		"attributes": map[string]any{"steps": "implementer tester reviewer"}}, 3)
	status, body := r.registerPlan(plan, 3)
	var refusal api.PlanRefusal
	if err := json.Unmarshal([]byte(body), &refusal); err != nil || status != http.StatusConflict || refusal.Code != "PLAN_REFUSED" ||
		len(refusal.Reasons) != 1 || refusal.Reasons[0].Task != "a" || !strings.Contains(refusal.Reasons[0].Reason, "task a is running") {
		t.Fatalf("register_plan of version 3, which changes a's steps while it runs, = %d %s, want 409 PLAN_REFUSED naming task a as running", status, body)
	}
	reviewer.endTurn()
	r.publishReview(r.number, "APPROVED")
	r.waitPhase(phase.Retro)
	holdsText(t, "retro", takeText(fresh), two, one)

	// The step records in Dispatch count the starts and attempts of a run with no version 2: each
	// step of a twice, its first attempt and the one after the review's return, and the retro once.
	// The lost session and the resume start no attempt.
	r.waitSteps("task a's step records", func(read stepRecordsRead) error {
		want := []struct {
			kind          string
			step, attempt int
			role          string
		}{{"step", 1, 1, "implementer"}, {"step", 2, 1, "reviewer"}, {"step", 1, 2, "implementer"}, {"step", 2, 2, "reviewer"}, {"retro", 0, 1, "implementer"}}
		for _, w := range want {
			if got := read.find(w.kind, w.step, w.attempt); got.ID == "" || got.Task != "a" || got.Role != w.role {
				return fmt.Errorf("the read holds %+v for %s %d attempt %d, want task a's record of the %s", got, w.kind, w.step, w.attempt, w.role)
			}
		}
		if got := len(read.records()); got != len(want) {
			return fmt.Errorf("the read holds %d records, want %d", got, len(want))
		}
		return nil
	})

	r.on(r.issues[1].key)
	r.openGate(phase.Planning)
	planner := r.worker(claim.RolePlanner)
	takeText(planner)
	defaults := r.document("plan.md", textPlan("default", "", three))
	if got, want := r.registeredRunning(defaults, 1)["default"], []runningAct{{Kind: "step", Place: 1, Role: string(claim.RolePlanner)}}; !slices.Equal(got, want) {
		t.Fatalf("register_plan of %s's default block names it running at %+v, want %+v: step 1, the planner", r.key, got, want)
	}
	r.finish(planner, "plan", api.HandoffCompleteRequest{Summary: "planned"}, phase.Implementing)
	task := takeText(r.worker(claim.RoleImplementer))
	holdsText(t, r.key+"'s implementer", task, three, "The project's default steps.")
	if !strings.Contains(task, "Task: default. Step 2 of ") || !strings.Contains(task, "1 planner, 2 implementer") {
		t.Fatalf("%s's implementer's task %q does not run the project's default steps", r.key, task)
	}
}
