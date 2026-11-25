package api

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// stepsTask is one task of a read of an issue's steps as a person sees it: its id, what the plan
// says of it (each planned step as `<step> <name> <state>`), and the ids of its runs in the order
// the read lists them. Needs are kept as the read answers them, so null (nil) and [] (a pointer to
// an empty list) stay apart.
type stepsTask struct {
	Task    string
	Needs   *[]string
	Planned []string
	Runs    []string
}

// stepsTasks is each task of read.
func stepsTasks(read model.IssueSteps) []stepsTask {
	tasks := make([]stepsTask, len(read.Tasks))
	for index, task := range read.Tasks {
		tasks[index] = stepsTask{Task: task.Task, Needs: task.Needs}
		for _, step := range task.Planned {
			tasks[index].Planned = append(tasks[index].Planned, fmt.Sprintf("%d %s %s", step.Step, step.Name, step.State))
		}
		for _, step := range task.Steps {
			tasks[index].Runs = append(tasks[index].Runs, step.ID)
		}
	}
	return tasks
}

// notStarted is the planned steps names, each at its place and not started.
func notStarted(names ...string) []string {
	planned := make([]string, len(names))
	for index, name := range names {
		planned[index] = fmt.Sprintf("%d %s not started", index+1, name)
	}
	return planned
}

// startRun writes the start of a run of step place of task, in generation, at started, and
// answers its id.
func startRun(t *testing.T, handler http.Handler, project, issue, task, role string, place int, generation int, started time.Time) string {
	t.Helper()
	id := uuid.NewString()
	putStepOK(t, handler, issue, id, map[string]any{
		"actor": daemonSessionOf(project), "task": task, "step": place, "kind": "step", "role": role,
		"generation": generation, "started_at": started,
	})
	return id
}

// The read of an issue's steps lists the tasks of its newest plan first, in the plan's order, each
// with the tasks it needs and the steps the plan gives it, whether a step of it ran or not; then
// each other task with a run, by its first run. A task of the plan none of whose steps ran answers
// its runs as [], as the read answers its tasks, never null. The needs are the ids the plan node
// records: the read parses no document, so the node here names a document no artifact holds, whose
// every read fails. A node written with no needs (a daemon of the release before) answers them
// null, which is not [] (needs none). An issue with no plan lists its tasks by their first run. The
// faults are a task of the plan that a person cannot see, a task of the plan whose runs answer
// null, needs read from anywhere but the node, and a node that recorded no needs answered as
// needing none; the controls are the handler that lists runs alone (b is absent), the handler that
// builds a task of the plan with no list of runs (b's and the default task's runs are null), the
// handler that reads the needs from the document (its read fails), and a reader that answers [] for
// a node with none (the default task's needs are not null).
func TestTheStepsOfAnIssueListThePlansTasks(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	owner := createTestIssue(t, handler, "LEG", "A run of steps")
	defaults := createAdvancingIssue(t, handler, map[string]any{"project": "LEG", "title": "Another run of steps"}, "alice").Key
	none := createAdvancingIssue(t, handler, map[string]any{"project": "LEG", "title": "A third run of steps"}, "alice").Key
	daemon := daemonSessionOf("LEG")

	putPlanRecordOK(t, handler, owner, uuid.NewString(), map[string]any{
		"actor": daemon, "generation": 1, "source": "owner", "artifact_id": uuid.NewString(), "version": 2,
		"tasks": []map[string]any{
			{"id": "a", "steps": []string{"implementer", "reviewer"}, "needs": []string{}},
			{"id": "b", "steps": []string{"tester"}, "needs": []string{"a"}},
		},
	})
	earlier := startRun(t, handler, "LEG", owner, "earlier", "implementer", 1, 1, stepStart)
	implementing := startRun(t, handler, "LEG", owner, "a", "implementer", 1, 1, stepStart.Add(time.Minute))

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+owner+"/steps", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the steps of %s: status=%d body=%s, want 200", owner, response.Code, response.Body.String())
	}
	want := []stepsTask{
		{Task: "a", Needs: &[]string{}, Planned: []string{"1 implementer running", "2 reviewer not started"}, Runs: []string{implementing}},
		{Task: "b", Needs: &[]string{"a"}, Planned: notStarted("tester")},
		{Task: "earlier", Runs: []string{earlier}},
	}
	if got := stepsTasks(decodeBody[model.IssueSteps](t, response)); !reflect.DeepEqual(got, want) {
		t.Fatalf("the steps of %s =\n%+v\nwant\n%+v", owner, got, want)
	}
	body := response.Body.String()
	for _, member := range []string{`"task":"a","needs":[],`, `"task":"b","needs":["a"],"planned":[{"step":1,"name":"tester","state":"not started"}],"steps":[]}`, `"task":"earlier","needs":null,`} {
		if !strings.Contains(body, member) {
			t.Fatalf("the steps of %s answer %s, want it to hold %s", owner, body, member)
		}
	}

	putPlanRecordOK(t, handler, defaults, uuid.NewString(), defaultPlanBody(daemon, 1))
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+defaults+"/steps", nil, "alice")
	if got, want := stepsTasks(decodeBody[model.IssueSteps](t, read)), []stepsTask{{Task: "default", Planned: notStarted(defaultStepLine...)}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the steps of %s, whose plan node records no needs =\n%+v\nwant\n%+v", defaults, got, want)
	}
	if !strings.Contains(read.Body.String(), `"task":"default","needs":null,`) {
		t.Fatalf("the steps of %s answer %s, want the default task's needs null: its node recorded none", defaults, read.Body.String())
	}
	if body := read.Body.String(); !strings.HasSuffix(strings.TrimSpace(body), `],"steps":[]}]}`) {
		t.Fatalf("the steps of %s answer %s, want the default task's runs []: none of its steps ran", defaults, body)
	}

	later := startRun(t, handler, "LEG", none, "y", "implementer", 1, 1, stepStart.Add(time.Minute))
	first := startRun(t, handler, "LEG", none, "x", "implementer", 1, 1, stepStart)
	if got, want := stepsTasks(readSteps(t, handler, none)), []stepsTask{{Task: "x", Runs: []string{first}}, {Task: "y", Runs: []string{later}}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the steps of %s, which has no plan =\n%+v\nwant\n%+v", none, got, want)
	}
}

// A planned step stands where the records of the plan's own run stand: the newest run of the step
// in the generation of the issue's newest plan node, `not started` where there is none. Within one
// run a step that failed and then passed reads passed. After a re-admission the next run has a node
// of its own, and the read takes it, so its steps that did not run read not started, whatever the
// earlier run's records hold. The fault is a step of the new run that shows the result of the old
// run; the control is the state taken from the runs of every generation, under which the reviewer
// of generation 2 reads passed.
func TestAPlannedStepStandsWhereTheRunsOfItsOwnGenerationStand(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	daemon := daemonSessionOf("LEG")
	node := func(generation int, steps ...string) {
		putPlanRecordOK(t, handler, issue, uuid.NewString(), map[string]any{
			"actor": daemon, "generation": generation, "source": "default",
			"tasks": []map[string]any{{"id": "default", "steps": steps, "needs": []string{}}},
		})
	}
	ran := func(role string, place, generation, from int, result string) {
		id := startRun(t, handler, "LEG", issue, "default", role, place, generation, at(from))
		end := map[string]any{"actor": daemon, "task": "default", "step": place, "kind": "step", "role": role,
			"generation": generation, "result": result, "ended_at": at(from + 5)}
		if result == "failed" {
			end["returned_to"] = 1
		}
		putStepOK(t, handler, issue, id, end)
	}
	planned := func() []string {
		tasks := stepsTasks(readSteps(t, handler, issue))
		if len(tasks) == 0 {
			t.Fatalf("the steps of %s list no task", issue)
		}
		return tasks[0].Planned
	}

	node(1, "implementer", "reviewer")
	ran("implementer", 1, 1, 0, "passed")
	ran("reviewer", 2, 1, 10, "failed")
	ran("implementer", 1, 1, 20, "passed")
	ran("reviewer", 2, 1, 30, "passed")
	if got, want := planned(), []string{"1 implementer passed", "2 reviewer passed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the planned steps of run 1 = %q, want %q", got, want)
	}

	node(2, "implementer", "reviewer", "tester")
	startRun(t, handler, "LEG", issue, "default", "implementer", 1, 2, at(60))
	if got, want := planned(), []string{"1 implementer running", "2 reviewer not started", "3 tester not started"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the planned steps of run 2 = %q, want %q: its steps stand where the records of run 2 stand", got, want)
	}
}

// A later version of the plan in force can change a stopped task's steps within one run of the
// issue: version 1 is `implementer reviewer`, its implementer passed and its reviewer was stopped,
// and version 2 is `implementer tester reviewer`. The runs of version 1 stay in the generation, so
// place 2 holds the reviewer's stopped run while the plan names a tester there. A run counts for a
// planned step only when it is a run of that step, so the tester, which never ran, reads not started.
// The fault is a run of another step at the same place read as the step's; the control is the same
// sequence with that run's role the tester's, under which the tester reads stopped.
func TestAPlannedStepReadsOnlyItsOwnRunsAfterAVersionChangesItsSteps(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	daemon := daemonSessionOf("LEG")
	planned := func(issue, stoppedRole string) []string {
		t.Helper()
		id, artifact := uuid.NewString(), uuid.NewString()
		version := func(number int, steps ...string) {
			putPlanRecordOK(t, handler, issue, id, map[string]any{
				"actor": daemon, "generation": 1, "source": "owner", "artifact_id": artifact, "version": number,
				"tasks": []map[string]any{{"id": "a", "steps": steps, "needs": []string{}}},
			})
		}
		end := func(run, role string, place, minute int, result string) {
			putStepOK(t, handler, issue, run, map[string]any{"actor": daemon, "task": "a", "step": place, "kind": "step",
				"role": role, "generation": 1, "result": result, "ended_at": at(minute)})
		}
		version(1, "implementer", "reviewer")
		end(startRun(t, handler, "LEG", issue, "a", "implementer", 1, 1, at(0)), "implementer", 1, 5, "passed")
		end(startRun(t, handler, "LEG", issue, "a", stoppedRole, 2, 1, at(10)), stoppedRole, 2, 15, "stopped")
		version(2, "implementer", "tester", "reviewer")
		tasks := stepsTasks(readSteps(t, handler, issue))
		if len(tasks) == 0 {
			t.Fatalf("the steps of %s list no task", issue)
		}
		return tasks[0].Planned
	}

	changed := createTestIssue(t, handler, "LEG", "A plan whose later version adds a tester")
	if got, want := planned(changed, "reviewer"), []string{"1 implementer passed", "2 tester not started", "3 reviewer not started"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the planned steps after version 2 = %q, want %q: the reviewer's run at place 2 is no run of the tester", got, want)
	}
	control := createAdvancingIssue(t, handler, map[string]any{"project": "LEG", "title": "A plan whose tester was stopped"}, "alice").Key
	if got, want := planned(control, "tester"), []string{"1 implementer passed", "2 tester stopped", "3 reviewer not started"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the planned steps of the control = %q, want %q: a run of the tester at place 2 is the tester's", got, want)
	}
}

// A step the plan names `role:key` is run by an agent of that role, and its run records the role
// alone, since a run carries no agent key: the owner's acceptance, `acceptance:owner`, runs as a
// run of role acceptance. So a planned `acceptance:owner` whose run ended passed reads passed. The
// fault is a step matched by the whole word the plan writes, `acceptance:owner`, which no run's
// role is, so the step reads not started after it passed.
func TestAPlannedKeyedStepReadsTheRunsOfItsRole(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	daemon := daemonSessionOf("LEG")
	issue := createTestIssue(t, handler, "LEG", "A plan whose owner accepts the work")
	putPlanRecordOK(t, handler, issue, uuid.NewString(), map[string]any{
		"actor": daemon, "generation": 1, "source": "owner", "artifact_id": uuid.NewString(), "version": 1,
		"tasks": []map[string]any{{"id": "a", "steps": []string{"implementer", "acceptance:owner"}, "needs": []string{}}},
	})
	for place, role := range []string{"implementer", "acceptance"} {
		run := startRun(t, handler, "LEG", issue, "a", role, place+1, 1, at(10*place))
		putStepOK(t, handler, issue, run, map[string]any{"actor": daemon, "task": "a", "step": place + 1, "kind": "step",
			"role": role, "generation": 1, "result": "passed", "ended_at": at(10*place + 5)})
	}
	tasks := stepsTasks(readSteps(t, handler, issue))
	if len(tasks) == 0 {
		t.Fatalf("the steps of %s list no task", issue)
	}
	if got, want := tasks[0].Planned, []string{"1 implementer passed", "2 acceptance:owner passed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the planned steps = %q, want %q: the run of role acceptance at place 2 is the run of acceptance:owner", got, want)
	}
}

// A plan write records each task's needs as the ids it names, and answers them; a task written
// with no needs answers them null. A need that names no task of the plan is refused, and nothing
// is written. The fault is a node that holds a need no task of it answers to; the control is a
// write that stores the needs it is given unread.
func TestAPlanWriteRecordsEachTasksNeeds(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	daemon := daemonSessionOf("LEG")
	id := uuid.NewString()

	refused := putPlanRecord(t, handler, issue, id, map[string]any{
		"actor": daemon, "generation": 1, "source": "default",
		"tasks": []map[string]any{{"id": "a", "steps": []string{"implementer"}, "needs": []string{"x"}}},
	})
	expectRefusal(t, "a need of no task", refused, http.StatusBadRequest, codePlanInput, "task a", `"x"`)
	if tasks := readSteps(t, handler, issue).Tasks; len(tasks) != 0 {
		t.Fatalf("a refused plan write left the tasks %+v, want none", tasks)
	}

	written := putPlanRecordOK(t, handler, issue, id, map[string]any{
		"actor": daemon, "generation": 1, "source": "default",
		"tasks": []map[string]any{
			{"id": "a", "steps": []string{"implementer"}, "needs": []string{}},
			{"id": "b", "steps": []string{"tester"}, "needs": []string{"a"}},
			{"id": "c", "steps": []string{"reviewer"}},
		},
	})
	if got, want := []*[]string{written.Tasks[0].Needs, written.Tasks[1].Needs, written.Tasks[2].Needs}, []*[]string{{}, {"a"}, nil}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the written plan's needs = %v, %v, %v, want &[], &[a], <nil>", written.Tasks[0].Needs, written.Tasks[1].Needs, written.Tasks[2].Needs)
	}
}
