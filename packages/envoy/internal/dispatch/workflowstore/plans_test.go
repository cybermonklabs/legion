package workflowstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The plan of an issue's run, a node of kind plan part of its issue, and the read of a project's
// finished issues: each with its plan, the rating of each retro with its reason, its time and its
// count of failed steps.

// defaultSteps is the line of steps of an issue whose owner gave no plan.
var defaultSteps = []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}

// defaultPlan is the plan the daemon writes when the work of issue's generation starts and its
// owner gave none: the source default and one task default holding the default steps.
func defaultPlan(id, issue string, generation int64) PlanWrite {
	return PlanWrite{
		ID: id, Issue: issue, Generation: new(generation), Source: new(PlanSourceDefault),
		Tasks: []PlanTask{{ID: "default", Steps: slices.Clone(defaultSteps)}},
	}
}

// defaultPlanRead is the plan defaultPlan writes, as the store answers it: the source default, no
// document, one task default holding the default steps. It is written out, not read from the store,
// so a test compares the store's answer with a plan the store did not make.
func defaultPlanRead(id, issue string, generation int64) Plan {
	return Plan{ID: id, Issue: issue, Source: PlanSourceDefault, Generation: generation,
		Tasks: []PlanTask{{ID: "default", Steps: defaultSteps}}}
}

// putPlan writes one plan in a transaction of its own and commits it, as the route does.
func putPlan(t *testing.T, pool *store.Pool, write PlanWrite, at time.Time) Plan {
	t.Helper()
	written, err := tryPutPlan(pool, write, at)
	if err != nil {
		t.Fatalf("write plan %s: %v", write.ID, err)
	}
	return written
}

// tryPutPlan runs one plan write in a transaction of its own (committed).
func tryPutPlan(pool *store.Pool, write PlanWrite, at time.Time) (Plan, error) {
	return committed(pool, func(ctx context.Context, tx pgx.Tx) (Plan, error) { return PutPlan(ctx, tx, write, daemon, at) })
}

// planText is a plan as one line, every field of it, so two plans compare whole and a difference
// prints whole.
func planText(plan Plan) string {
	document := "none"
	if plan.Document != nil {
		document = fmt.Sprintf("%q version %d", plan.Document.ArtifactID, plan.Document.Version)
	}
	return fmt.Sprintf("%s %s project %q gen %d %s document %s tasks %v", plan.ID, plan.Issue, plan.Project,
		plan.Generation, plan.Source, document, plan.Tasks)
}

// A plan is one node of kind plan, part of its issue by one relation tuple, holding its source, its
// run and its tasks, each attribute stated by one value tuple.
func TestAPlanIsANodePartOfItsIssue(t *testing.T) {
	pool := storetest.Open(t).Pool
	id := uuid.NewString()

	written := putPlan(t, pool, defaultPlan(id, "LEG-1", 1), minute(0))
	want := defaultPlanRead(id, "LEG-1", 1)
	if planText(written) != planText(want) {
		t.Fatalf("the write answers\n%s\nwant\n%s", planText(written), planText(want))
	}

	var kind, supertag string
	if err := pool.QueryRow(context.Background(), `select kind, supertag from workflow_nodes where id = $1`, id).Scan(&kind, &supertag); err != nil {
		t.Fatalf("read the plan's node: %v", err)
	}
	if kind != KindPlan || supertag != KindPlan {
		t.Fatalf("the plan's node is kind %q supertag %q, want %q for both", kind, supertag, KindPlan)
	}
	var partOf, tasks int
	for _, tuple := range wholeLog(t, pool) {
		if tuple.fromKind != KindPlan || tuple.fromID != id {
			t.Fatalf("the log holds a tuple from %s %s, want only the plan's", tuple.fromKind, tuple.fromID)
		}
		switch {
		case tuple.kind == RelationPartOf && tuple.toKind == KindIssue && tuple.toID == "LEG-1":
			partOf++
		case tuple.kind == AttributeTasks && tuple.toKind == KindValue:
			tasks++
			if want := `[{"id":"default","steps":["planner","implementer","tester","reviewer","acceptance:owner"]}]`; tuple.toID != want {
				t.Fatalf("the tasks' tuple holds %s, want the JSON text %s", tuple.toID, want)
			}
		}
	}
	if partOf != 1 || tasks != 1 {
		t.Fatalf("the log holds %d part_of tuples and %d tasks tuples, want one of each", partOf, tasks)
	}
}

// PlanOfIssue reads the plan an issue holds under an id as the write answered it, and no plan for
// an id no plan has, nor for the id of another issue's plan.
func TestPlanOfIssueReadsThePlanItsIssueHolds(t *testing.T) {
	pool := storetest.Open(t).Pool
	id := uuid.NewString()
	written := putPlan(t, pool, defaultPlan(id, "LEG-1", 1), minute(0))

	held, found, err := PlanOfIssue(context.Background(), pool, "LEG-1", id)
	if err != nil || !found || planText(held) != planText(written) {
		t.Fatalf("the read of %s of LEG-1 answers %s (found %t, error %v), want %s", id, planText(held), found, err, planText(written))
	}
	for name, read := range map[string]struct{ issue, id string }{
		"the plan of another issue": {"LEG-2", id},
		"an id no plan has":         {"LEG-1", uuid.NewString()},
	} {
		if held, found, err := PlanOfIssue(context.Background(), pool, read.issue, read.id); err != nil || found {
			t.Errorf("the read of %s answers %s (found %t, error %v), want no plan", name, planText(held), found, err)
		}
	}
}

// Each field of a plan is written once: a later write of the same id with another generation,
// another source and other tasks is taken, changes nothing and appends nothing. An owner's plan
// names its document and version.
func TestAPlanWriteKeepsTheFirst(t *testing.T) {
	pool := storetest.Open(t).Pool
	id, document := uuid.NewString(), uuid.NewString()
	owner := PlanWrite{
		ID: id, Issue: "LEG-1", Generation: new(int64(2)), Source: new(PlanSourceOwner),
		Document: PlanDocument{ArtifactID: document, Version: 3},
		Tasks:    []PlanTask{{ID: "api", Steps: []string{"implementer", "reviewer:fresh"}}, {ID: "docs", Steps: []string{"implementer"}}},
	}
	first := putPlan(t, pool, owner, minute(0))
	before := wholeLog(t, pool)

	again := putPlan(t, pool, defaultPlan(id, "LEG-1", 10), minute(1))
	if planText(again) != planText(first) {
		t.Fatalf("a second write answers\n%s\nwant the first\n%s", planText(again), planText(first))
	}
	if after := wholeLog(t, pool); len(after) != len(before) {
		t.Fatalf("a second write appended %d tuples, want none", len(after)-len(before))
	}
	if first.Generation != 2 || first.Document == nil || *first.Document != (PlanDocument{ArtifactID: document, Version: 3}) || len(first.Tasks) != 2 {
		t.Fatalf("the owner's plan answers %s, want generation 2 and its document %s at version 3 and two tasks", planText(first), document)
	}
}

// A plan made with the source default holds no document, and a later write of the same id that
// names the source owner, a document and a version is taken, changes nothing and appends nothing:
// the write, a second write of the same default plan and the read of the project all answer the
// default plan with no document.
func TestADefaultPlanTakesNoDocumentFromALaterWrite(t *testing.T) {
	pool := storetest.Open(t).Pool
	closed := t0.Add(time.Hour)
	seedIssues(t, pool, "LEG", seededIssue{"LEG-1", "done", &closed})
	id := uuid.NewString()
	first := putPlan(t, pool, defaultPlan(id, "LEG-1", 1), minute(0))
	want := defaultPlanRead(id, "LEG-1", 1)
	if planText(first) != planText(want) {
		t.Fatalf("the default plan answers\n%s\nwant\n%s", planText(first), planText(want))
	}
	before := wholeLog(t, pool)

	owner := putPlan(t, pool, PlanWrite{
		ID: id, Issue: "LEG-1", Generation: new(int64(1)), Source: new(PlanSourceOwner),
		Document: PlanDocument{ArtifactID: uuid.NewString(), Version: 3},
		Tasks:    []PlanTask{{ID: "ui", Steps: []string{"implementer"}}, {ID: "api", Steps: []string{"implementer", "reviewer"}}},
	}, minute(1))
	if planText(owner) != planText(want) {
		t.Fatalf("a later owner's write answers\n%s\nwant the default plan\n%s", planText(owner), planText(want))
	}
	if again := putPlan(t, pool, defaultPlan(id, "LEG-1", 1), minute(2)); planText(again) != planText(want) {
		t.Fatalf("the default plan written again answers\n%s\nwant\n%s", planText(again), planText(want))
	}
	if after := wholeLog(t, pool); len(after) != len(before) {
		t.Fatalf("the later writes appended %d tuples, want none", len(after)-len(before))
	}

	read, err := ProjectPlans(context.Background(), pool, "LEG", Window{})
	if err != nil {
		t.Fatalf("read the plans of LEG: %v", err)
	}
	if len(read) != 1 || read[0].Plan == nil || planText(*read[0].Plan) != planText(want) {
		t.Fatalf("the read lists %+v, want LEG-1 with the default plan\n%s", read, planText(want))
	}
}

// A value outside its form is refused ErrPlanInput, naming the field, and writes nothing.
func TestAPlanWriteRefusesAValueOutsideItsForm(t *testing.T) {
	pool := storetest.Open(t).Pool
	for name, change := range map[string]func(*PlanWrite){
		"source mine":               func(write *PlanWrite) { write.Source = new("mine") },
		"source left out":           func(write *PlanWrite) { write.Source = nil },
		"generation 0":              func(write *PlanWrite) { write.Generation = new(int64(0)) },
		"generation left out":       func(write *PlanWrite) { write.Generation = nil },
		"tasks left out":            func(write *PlanWrite) { write.Tasks = nil },
		"tasks empty":               func(write *PlanWrite) { write.Tasks = []PlanTask{} },
		"task Default":              func(write *PlanWrite) { write.Tasks[0].ID = "Default" },
		"task twice":                func(write *PlanWrite) { write.Tasks = append(write.Tasks, write.Tasks[0]) },
		"steps empty":               func(write *PlanWrite) { write.Tasks[0].Steps = nil },
		"steps Planner":             func(write *PlanWrite) { write.Tasks[0].Steps[0] = "Planner" },
		"steps with a blank key":    func(write *PlanWrite) { write.Tasks[0].Steps[4] = "acceptance:" },
		"artifact_id of default":    func(write *PlanWrite) { write.Document = PlanDocument{ArtifactID: uuid.NewString()} },
		"version of default":        func(write *PlanWrite) { write.Document = PlanDocument{Version: 1} },
		"artifact_id of owner none": func(write *PlanWrite) { write.Source, write.Document = new(PlanSourceOwner), PlanDocument{Version: 1} },
		"artifact_id not a uuid": func(write *PlanWrite) {
			write.Source, write.Document = new(PlanSourceOwner), PlanDocument{ArtifactID: "plan-md", Version: 1}
		},
		"version of owner none": func(write *PlanWrite) {
			write.Source, write.Document = new(PlanSourceOwner), PlanDocument{ArtifactID: uuid.NewString()}
		},
		"id":    func(write *PlanWrite) { write.ID = "not-a-uuid" },
		"issue": func(write *PlanWrite) { write.Issue = "" },
	} {
		t.Run(name, func(t *testing.T) {
			write := defaultPlan(uuid.NewString(), "LEG-1", 1)
			change(&write)
			_, err := tryPutPlan(pool, write, minute(0))
			if !errors.Is(err, ErrPlanInput) {
				t.Fatalf("the write answered %v, want %v", err, ErrPlanInput)
			}
			if field := strings.Fields(name)[0]; !strings.Contains(err.Error(), field) {
				t.Errorf("the refusal %q does not name %s", err, field)
			}
		})
	}
	if log := wholeLog(t, pool); len(log) != 0 {
		t.Fatalf("the refused writes left %d tuples, want none", len(log))
	}
}

// A step of a plan names the role of the step records its runs make, the part before its `:key`,
// so a plan takes a step where a record takes its role and refuses it where a record refuses that
// role: a role of 32 characters is taken by both, one of 33 or one outside the record's grammar
// is refused by both, the plan's refusal ErrPlanInput naming the step and its role.
func TestAPlanStepTakesTheRolesAStepRecordTakes(t *testing.T) {
	pool := storetest.Open(t).Pool
	longest, over := strings.Repeat("r", 32), strings.Repeat("r", 33)
	for _, row := range []struct{ step, role string }{
		{longest, longest},
		{longest + ":owner", longest},
		{over, over},
		{over + ":owner", over},
		{"9planner", "9planner"},
		{"Planner:owner", "Planner"},
	} {
		t.Run(row.step, func(t *testing.T) {
			plan := defaultPlan(uuid.NewString(), "LEG-1", 1)
			plan.Tasks[0].Steps = []string{row.step}
			_, planRefusal := tryPutPlan(pool, plan, minute(0))
			record := implementer(uuid.NewString())
			record.Role = new(row.role)
			_, recordRefusal := tryPut(pool, record, minute(0))
			if recordRefusal != nil && !errors.Is(recordRefusal, ErrStepInput) {
				t.Fatalf("a record of role %q answered %v, want nil or %v", row.role, recordRefusal, ErrStepInput)
			}
			if (planRefusal == nil) != (recordRefusal == nil) {
				t.Fatalf("a plan of step %q answered %v and a record of its role %q %v: want both taken or both refused",
					row.step, planRefusal, row.role, recordRefusal)
			}
			if planRefusal == nil {
				return
			}
			if message := planRefusal.Error(); !errors.Is(planRefusal, ErrPlanInput) ||
				!strings.Contains(message, strconv.Quote(row.step)) || !strings.Contains(message, strconv.Quote(row.role)) {
				t.Fatalf("a plan of step %q answered %v, want %v naming the step and its role %q", row.step, planRefusal, ErrPlanInput, row.role)
			}
		})
	}
}

// An id the plan of one issue holds is refused ErrPlanOtherIssue to a write naming another issue.
func TestAPlanIDOfAnotherIssueIsRefused(t *testing.T) {
	pool := storetest.Open(t).Pool
	id := uuid.NewString()
	putPlan(t, pool, defaultPlan(id, "LEG-1", 1), minute(0))
	before := wholeLog(t, pool)

	_, err := tryPutPlan(pool, defaultPlan(id, "LEG-2", 1), minute(1))
	if !errors.Is(err, ErrPlanOtherIssue) || !strings.Contains(err.Error(), "LEG-1") || !strings.Contains(err.Error(), "LEG-2") {
		t.Fatalf("the write answered %v, want %v naming LEG-1 and LEG-2", err, ErrPlanOtherIssue)
	}
	if after := wholeLog(t, pool); len(after) != len(before) {
		t.Fatalf("the refused write appended %d tuples, want none", len(after)-len(before))
	}
}

// A plan is part of its issue as each step record is, and the read of the issue's steps lists the
// records alone: the plan of each of its two runs is no step.
func TestTheStepsOfAnIssueListNoPlan(t *testing.T) {
	pool := storetest.Open(t).Pool
	putPlan(t, pool, defaultPlan(uuid.NewString(), "LEG-1", 1), minute(0))
	planner := stepRun(t, pool, "LEG-1", StepKindStep, "planner", 1, 1, new(minute(0)), new(minute(5)), ResultPassed)
	putPlan(t, pool, defaultPlan(uuid.NewString(), "LEG-1", 2), minute(10))
	reviewer := stepRun(t, pool, "LEG-1", StepKindStep, "reviewer", 4, 2, new(minute(10)), nil, "")

	steps := issueSteps(t, pool, "LEG-1")
	listed := make([]string, len(steps))
	for index, step := range steps {
		listed[index] = step.Kind + " " + step.Role + " " + step.ID
	}
	if want := []string{"step planner " + planner, "step reviewer " + reviewer}; !slices.Equal(listed, want) {
		t.Fatalf("the steps of LEG-1 are %v, want the two step records alone, %v", listed, want)
	}
}

// seedIssues writes project and its issues straight into the tables, each with its status and, for
// one that is closed, the moment it closed.
func seedIssues(t *testing.T, pool *store.Pool, project string, issues ...seededIssue) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ($1, $1, 'alice') on conflict do nothing`, project); err != nil {
		t.Fatalf("seed project %s: %v", project, err)
	}
	for _, issue := range issues {
		_, number, _ := strings.Cut(issue.key, "-")
		if _, err := pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, status, closed_at, created_by, rank)
			values ($1, $2, $3::int, $4, $5, $6, '{"kind":"user","id":"alice"}', 'U' || $3)
		`, issue.key, project, number, "Title of "+issue.key, issue.status, issue.closed); err != nil {
			t.Fatalf("seed issue %s: %v", issue.key, err)
		}
	}
}

type seededIssue struct {
	key, status string
	closed      *time.Time
}

// stepRun writes one run of one step of issue: its identity, and the start and end that are not nil.
func stepRun(t *testing.T, pool *store.Pool, issue, kind, role string, place int, generation int64,
	started, ended *time.Time, result string) string {
	t.Helper()
	id := uuid.NewString()
	write := StepWrite{
		ID: id, Issue: issue, Task: new("default"), Place: new(place), Kind: new(kind), Role: new(role),
		Generation: new(generation), StartedAt: started, EndedAt: ended,
	}
	if result != "" {
		write.Result = new(result)
	}
	if result == ResultFailed {
		write.ReturnedTo = new(2)
	}
	put(t, pool, write, minute(0))
	return id
}

// ratedRetro writes the retro of issue's generation, from start to end, rated with its reason.
func ratedRetro(t *testing.T, pool *store.Pool, issue string, generation int64, started, ended time.Time, rating int, reason string) string {
	t.Helper()
	id := stepRun(t, pool, issue, StepKindRetro, "implementer", 0, generation, &started, &ended, ResultPassed)
	write := rated(retroOf(id), rating, reason)
	write.Issue, write.Generation = issue, new(generation)
	put(t, pool, write, ended)
	return id
}

// issuePlanText is one issue of the read as one line, every field of it, its plan whole and its
// times in UTC, so two issues compare whole and a difference prints whole.
func issuePlanText(issue IssuePlan) string {
	moment := func(at *time.Time) string {
		if at == nil {
			return "-"
		}
		return at.UTC().Format(time.RFC3339Nano)
	}
	plan := "none"
	if issue.Plan != nil {
		plan = planText(*issue.Plan)
	}
	ratings := make([]string, len(issue.Ratings))
	for index, rating := range issue.Ratings {
		ratings[index] = fmt.Sprintf("%d %q %s %s", rating.Rating, rating.Reason, rating.StepID, moment(rating.EndedAt))
	}
	return fmt.Sprintf("%s %q closed %s plan %s ratings %v from %s to %s failed %d", issue.Issue, issue.Title,
		issue.ClosedAt.UTC().Format(time.RFC3339Nano), plan, ratings, moment(issue.StartedAt), moment(issue.EndedAt), issue.FailedSteps)
}

// The read of a project lists each issue of it that is done and closed, newest close first, with
// its plan, the rating of its retro, the first start and the last end of its records and its
// count of failed steps of kind step, all in one statement: a failed retro and a failed merge are
// no failed step. An issue still open, an issue done with no close, an issue closed that is not
// done, an issue of another project, LEGX's among them though its key starts with LEG, and a
// record of another issue are not read into it; a finished issue Legion never ran is listed with
// no plan, no rating, no time and no failed step.
func TestProjectPlansListsEachFinishedIssueInOneStatement(t *testing.T) {
	pool := storetest.Open(t).Pool
	closedFirst, closedSecond, closedThird := t0.Add(time.Hour), t0.Add(2*time.Hour), t0.Add(3*time.Hour)
	seedIssues(t, pool, "LEG",
		seededIssue{"LEG-1", "done", &closedSecond},
		seededIssue{"LEG-2", "done", &closedThird},
		seededIssue{"LEG-3", "in_progress", nil},
		seededIssue{"LEG-4", "done", &closedFirst},
		seededIssue{"LEG-5", "done", nil},
		seededIssue{"LEG-6", "in_progress", &closedThird},
	)
	seedIssues(t, pool, "OTH", seededIssue{"OTH-1", "done", &closedThird})
	seedIssues(t, pool, "LEGX", seededIssue{"LEGX-1", "done", &closedSecond})

	planOne, planTwo := uuid.NewString(), uuid.NewString()
	putPlan(t, pool, defaultPlan(planOne, "LEG-1", 1), minute(0))
	stepRun(t, pool, "LEG-1", StepKindStep, "planner", 1, 1, new(minute(0)), new(minute(5)), ResultPassed)
	stepRun(t, pool, "LEG-1", StepKindStep, "tester", 3, 1, new(minute(6)), new(minute(9)), ResultFailed)
	stepRun(t, pool, "LEG-1", StepKindStep, "reviewer", 4, 1, new(minute(10)), new(minute(15)), ResultFailed)
	stepRun(t, pool, "LEG-1", StepKindRetro, "implementer", 0, 1, new(minute(16)), new(minute(18)), ResultFailed)
	retroOne := ratedRetro(t, pool, "LEG-1", 1, minute(20), minute(22), 4, "The tester found the bug.")
	stepRun(t, pool, "LEG-1", StepKindMerge, "merger", 0, 1, nil, new(minute(25)), ResultFailed)
	stepRun(t, pool, "LEG-1", StepKindMerge, "merger", 0, 1, nil, new(minute(30)), ResultPassed)

	putPlan(t, pool, defaultPlan(planTwo, "LEG-2", 1), minute(60))
	stepRun(t, pool, "LEG-2", StepKindStep, "planner", 1, 1, new(minute(60)), new(minute(70)), ResultPassed)
	retroTwo := ratedRetro(t, pool, "LEG-2", 1, minute(71), minute(72), 5, "The plan fit the work.")
	stepRun(t, pool, "LEG-2", StepKindMerge, "merger", 0, 1, nil, new(minute(75)), ResultPassed)

	putPlan(t, pool, defaultPlan(uuid.NewString(), "LEG-3", 1), minute(80))
	stepRun(t, pool, "LEG-3", StepKindStep, "planner", 1, 1, new(minute(80)), new(minute(85)), ResultFailed)
	putPlan(t, pool, defaultPlan(uuid.NewString(), "OTH-1", 1), minute(80))
	stepRun(t, pool, "OTH-1", StepKindStep, "reviewer", 4, 1, new(minute(80)), new(minute(85)), ResultFailed)
	putPlan(t, pool, defaultPlan(uuid.NewString(), "LEGX-1", 1), minute(90))
	stepRun(t, pool, "LEGX-1", StepKindStep, "reviewer", 4, 1, new(minute(90)), new(minute(95)), ResultFailed)

	counter := &storetest.StatementCounter{}
	read, err := ProjectPlans(context.Background(), storetest.Traced(t, pool, counter, nil), "LEG", Window{})
	if err != nil {
		t.Fatalf("read the plans of LEG: %v", err)
	}
	if sent := counter.Count(); sent != 1 {
		t.Fatalf("the read sent %d statements, want one for the whole project", sent)
	}
	want := []IssuePlan{
		{Issue: "LEG-2", Title: "Title of LEG-2", ClosedAt: closedThird,
			Plan:      new(defaultPlanRead(planTwo, "LEG-2", 1)),
			Ratings:   []Rating{{Rating: 5, Reason: "The plan fit the work.", StepID: retroTwo, EndedAt: new(minute(72))}},
			StartedAt: new(minute(60)), EndedAt: new(minute(75)), FailedSteps: 0},
		{Issue: "LEG-1", Title: "Title of LEG-1", ClosedAt: closedSecond,
			Plan:      new(defaultPlanRead(planOne, "LEG-1", 1)),
			Ratings:   []Rating{{Rating: 4, Reason: "The tester found the bug.", StepID: retroOne, EndedAt: new(minute(22))}},
			StartedAt: new(minute(0)), EndedAt: new(minute(30)), FailedSteps: 2},
		{Issue: "LEG-4", Title: "Title of LEG-4", ClosedAt: closedFirst},
	}
	got := make([]string, len(read))
	for index, issue := range read {
		got[index] = issuePlanText(issue)
	}
	wanted := make([]string, len(want))
	for index, issue := range want {
		wanted[index] = issuePlanText(issue)
	}
	if !slices.Equal(got, wanted) {
		t.Fatalf("the read lists\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wanted, "\n"))
	}

	other, err := ProjectPlans(context.Background(), pool, "LEGX", Window{})
	if err != nil {
		t.Fatalf("read the plans of LEGX: %v", err)
	}
	if len(other) != 1 || other[0].Issue != "LEGX-1" || other[0].FailedSteps != 1 {
		t.Fatalf("the read of LEGX lists %+v, want LEGX-1 alone with its failed step", other)
	}
}

// Two issues of one project that closed at one moment are each listed once and whole: each with
// its own plan, its own rating, its own time and its own count of failed steps, however the rows of
// the two come out of the statement. The read is compared by issue, since the contract orders only
// by the close. The owner's plan of LEG-2 names its document at version 1, the first version, and
// three tasks in neither order of their ids, which the write answers and the read lists as they were
// written; each expected plan is written out here, not taken from what the store answered.
func TestProjectPlansListsTwoIssuesClosedAtOneMomentWhole(t *testing.T) {
	pool := storetest.Open(t).Pool
	closed := t0.Add(5 * time.Hour)
	seedIssues(t, pool, "LEG", seededIssue{"LEG-1", "done", &closed}, seededIssue{"LEG-2", "done", &closed})

	planOne, planTwo, document := uuid.NewString(), uuid.NewString(), uuid.NewString()
	putPlan(t, pool, defaultPlan(planOne, "LEG-1", 1), minute(0))
	stepRun(t, pool, "LEG-1", StepKindStep, "planner", 1, 1, new(minute(0)), new(minute(5)), ResultPassed)
	stepRun(t, pool, "LEG-1", StepKindStep, "reviewer", 4, 1, new(minute(6)), new(minute(9)), ResultFailed)
	retroOne := ratedRetro(t, pool, "LEG-1", 1, minute(10), minute(12), 3, "The review came late.")
	stepRun(t, pool, "LEG-1", StepKindMerge, "merger", 0, 1, nil, new(minute(15)), ResultPassed)

	ownersPlan := Plan{ID: planTwo, Issue: "LEG-2", Source: PlanSourceOwner, Generation: 2, Document: &PlanDocument{ArtifactID: document, Version: 1},
		Tasks: []PlanTask{{ID: "ui", Steps: []string{"implementer", "tester"}}, {ID: "api", Steps: []string{"implementer"}}, {ID: "docs", Steps: []string{"writer", "implementer"}}}}
	written := putPlan(t, pool, PlanWrite{
		ID: planTwo, Issue: "LEG-2", Generation: new(int64(2)), Source: new(PlanSourceOwner), Document: PlanDocument{ArtifactID: document, Version: 1},
		Tasks: []PlanTask{{ID: "ui", Steps: []string{"implementer", "tester"}}, {ID: "api", Steps: []string{"implementer"}}, {ID: "docs", Steps: []string{"writer", "implementer"}}},
	}, minute(20))
	if planText(written) != planText(ownersPlan) {
		t.Errorf("the owner's plan write answers\n%s\nwant\n%s", planText(written), planText(ownersPlan))
	}
	stepRun(t, pool, "LEG-2", StepKindStep, "implementer", 1, 2, new(minute(20)), new(minute(25)), ResultPassed)
	stepRun(t, pool, "LEG-2", StepKindStep, "tester", 2, 2, new(minute(26)), new(minute(28)), ResultFailed)
	stepRun(t, pool, "LEG-2", StepKindStep, "tester", 2, 2, new(minute(29)), new(minute(31)), ResultFailed)
	retroTwo := ratedRetro(t, pool, "LEG-2", 2, minute(32), minute(33), 5, "Two tasks fit the work.")
	stepRun(t, pool, "LEG-2", StepKindMerge, "merger", 0, 2, nil, new(minute(40)), ResultPassed)

	read, err := ProjectPlans(context.Background(), pool, "LEG", Window{})
	if err != nil {
		t.Fatalf("read the plans of LEG: %v", err)
	}
	got := make([]string, len(read))
	for index, issue := range read {
		got[index] = issuePlanText(issue)
	}
	slices.Sort(got)
	want := []string{
		issuePlanText(IssuePlan{Issue: "LEG-1", Title: "Title of LEG-1", ClosedAt: closed,
			Plan:      new(defaultPlanRead(planOne, "LEG-1", 1)),
			Ratings:   []Rating{{Rating: 3, Reason: "The review came late.", StepID: retroOne, EndedAt: new(minute(12))}},
			StartedAt: new(minute(0)), EndedAt: new(minute(15)), FailedSteps: 1}),
		issuePlanText(IssuePlan{Issue: "LEG-2", Title: "Title of LEG-2", ClosedAt: closed, Plan: &ownersPlan,
			Ratings:   []Rating{{Rating: 5, Reason: "Two tasks fit the work.", StepID: retroTwo, EndedAt: new(minute(33))}},
			StartedAt: new(minute(20)), EndedAt: new(minute(40)), FailedSteps: 2}),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the read lists\n%s\nwant each issue once\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The plan is the newest node of the issue, the one of the highest generation, whatever its source,
// whatever order the plans were made and written in and however their generations sort as text and
// their ids sort, and every retro's rating is listed, in the order the retros ran: three, whose
// scores and reasons are each in neither ascending nor descending order that way, so no sort of
// the ratings by either is the run order.
func TestProjectPlansTakesTheNewestPlanAndEveryRating(t *testing.T) {
	pool := storetest.Open(t).Pool
	closed := t0.Add(5 * time.Hour)
	seedIssues(t, pool, "LEG", seededIssue{"LEG-1", "done", &closed})
	// Four plans, written in the order of generations 10, 40, 2 and 9. As written, their
	// generations, their generations as text ("10", "40", "2", "9"), the minutes they were made
	// (75, 50, 100, 0) and their ids are each in neither ascending nor descending order, and the
	// newest, generation 40, is the first or the last of none of these orders but its generation's,
	// so a read that took the plan made first or last, the least or the greatest id, or the first
	// or the last generation as text lists another plan. The newest is the issue's one default
	// plan, of a run whose owner gave none, and the three older plans are its owner's, each with
	// tasks of its own, so a read that took an owner's plan over a default one lists another too.
	newestID := "50000000-0000-4000-8000-000000000000"
	putPlan(t, pool, PlanWrite{
		ID: "30000000-0000-4000-8000-000000000000", Issue: "LEG-1", Generation: new(int64(10)), Source: new(PlanSourceOwner),
		Document: PlanDocument{ArtifactID: uuid.NewString(), Version: 3}, Tasks: []PlanTask{{ID: "docs", Steps: []string{"writer"}}},
	}, minute(75))
	putPlan(t, pool, defaultPlan(newestID, "LEG-1", 40), minute(50))
	putPlan(t, pool, PlanWrite{
		ID: "90000000-0000-4000-8000-000000000000", Issue: "LEG-1", Generation: new(int64(2)), Source: new(PlanSourceOwner),
		Document: PlanDocument{ArtifactID: uuid.NewString(), Version: 1}, Tasks: []PlanTask{{ID: "api", Steps: []string{"implementer"}}},
	}, minute(100))
	putPlan(t, pool, PlanWrite{
		ID: "10000000-0000-4000-8000-000000000000", Issue: "LEG-1", Generation: new(int64(9)), Source: new(PlanSourceOwner),
		Document: PlanDocument{ArtifactID: uuid.NewString(), Version: 2},
		Tasks:    []PlanTask{{ID: "ui", Steps: []string{"implementer", "reviewer"}}, {ID: "api", Steps: []string{"implementer"}}},
	}, minute(0))
	third := ratedRetro(t, pool, "LEG-1", 9, minute(200), minute(201), 4, "Two tasks were one.")
	second := ratedRetro(t, pool, "LEG-1", 2, minute(120), minute(121), 1, "The plan was late.")
	first := ratedRetro(t, pool, "LEG-1", 1, minute(20), minute(22), 3, "The review came late.")

	read, err := ProjectPlans(context.Background(), pool, "LEG", Window{})
	if err != nil {
		t.Fatalf("read the plans of LEG: %v", err)
	}
	if len(read) != 1 || read[0].Plan == nil {
		t.Fatalf("the read lists %d issues, want LEG-1 with a plan", len(read))
	}
	newest := defaultPlanRead(newestID, "LEG-1", 40)
	if plan := read[0].Plan; planText(*plan) != planText(newest) {
		t.Fatalf("the plan of LEG-1 is\n%s\nwant the default plan of generation 40\n%s", planText(*plan), planText(newest))
	}
	want := IssuePlan{Issue: "LEG-1", Title: "Title of LEG-1", ClosedAt: closed, Plan: &newest,
		Ratings: []Rating{
			{Rating: 3, Reason: "The review came late.", StepID: first, EndedAt: new(minute(22))},
			{Rating: 1, Reason: "The plan was late.", StepID: second, EndedAt: new(minute(121))},
			{Rating: 4, Reason: "Two tasks were one.", StepID: third, EndedAt: new(minute(201))},
		},
		StartedAt: new(minute(20)), EndedAt: new(minute(201))}
	if got := issuePlanText(read[0]); got != issuePlanText(want) {
		t.Fatalf("the read lists\n%s\nwant each retro's own rating, reason and end, in the order they ran\n%s", got, issuePlanText(want))
	}
}

// Of plans of one generation the read takes the one made last, and of those made at one moment
// the one of the greatest id. The daemon writes one plan of a generation, under the id it derives
// from the issue and the generation (the workflow's PlanID), so it never writes two; the route
// takes any uuid, so the read holds to one plan of a generation whatever the store holds, and these
// plans, each with the default plan's body (defaultPlan) under an id no derivation gives, are the
// store's case and not the daemon's. LEG-1's four plans were made at minutes in neither ascending
// nor descending order as written, and its plan made last has neither the least nor the greatest
// id, so a read that took the plan made first, or the least or the greatest id where the moments
// differ, lists another plan; LEG-2's four, made at one moment, have ids in neither order as
// written, so a read that took the least id lists another. The order the statement answers the
// rows in is the database's, so this test cannot set which plan is the first or the last row;
// TestAPlanIsNewerByGenerationThenMadeThenID holds the comparison for every pair of plans.
func TestProjectPlansTakesThePlanMadeLastThenTheGreatestID(t *testing.T) {
	pool := storetest.Open(t).Pool
	seedIssues(t, pool, "LEG",
		seededIssue{"LEG-1", "done", new(t0.Add(5 * time.Hour))},
		seededIssue{"LEG-2", "done", new(t0.Add(6 * time.Hour))},
	)
	madeLast := "3a000000-0000-4000-8000-000000000000"
	putPlan(t, pool, defaultPlan("5a000000-0000-4000-8000-000000000000", "LEG-1", 2), minute(40))
	putPlan(t, pool, defaultPlan(madeLast, "LEG-1", 2), minute(60))
	putPlan(t, pool, defaultPlan("9a000000-0000-4000-8000-000000000000", "LEG-1", 2), minute(30))
	putPlan(t, pool, defaultPlan("1a000000-0000-4000-8000-000000000000", "LEG-1", 2), minute(50))
	greatestID := "9b000000-0000-4000-8000-000000000000"
	putPlan(t, pool, defaultPlan("3b000000-0000-4000-8000-000000000000", "LEG-2", 3), minute(70))
	putPlan(t, pool, defaultPlan(greatestID, "LEG-2", 3), minute(70))
	putPlan(t, pool, defaultPlan("1b000000-0000-4000-8000-000000000000", "LEG-2", 3), minute(70))
	putPlan(t, pool, defaultPlan("5b000000-0000-4000-8000-000000000000", "LEG-2", 3), minute(70))

	read, err := ProjectPlans(context.Background(), pool, "LEG", Window{})
	if err != nil {
		t.Fatalf("read the plans of LEG: %v", err)
	}
	got := make([]string, len(read))
	for index, issue := range read {
		got[index] = issuePlanText(issue)
	}
	want := []string{
		issuePlanText(IssuePlan{Issue: "LEG-2", Title: "Title of LEG-2", ClosedAt: t0.Add(6 * time.Hour), Plan: new(defaultPlanRead(greatestID, "LEG-2", 3))}),
		issuePlanText(IssuePlan{Issue: "LEG-1", Title: "Title of LEG-1", ClosedAt: t0.Add(5 * time.Hour), Plan: new(defaultPlanRead(madeLast, "LEG-1", 2))}),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the read lists\n%s\nwant LEG-2's plan of the greatest id and LEG-1's plan made last\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A plan is newer than one of a lower generation, then, of one generation, than one made earlier,
// then, made at one moment, than one of a lesser id, whatever the keys after the first that differs
// say and whatever the plans' sources. Of these plans, listed oldest first, each is newer than
// every plan before it, than none after it and not than itself, and newer than nil, the read's plan
// before its first row; so the read takes one plan of an issue whatever order its rows come in. The
// oldest plan and the one of generation 2 made at minute 30 with the lesser id are owners' plans,
// the rest default plans, so a rule that took an owner's plan over a default one, before the
// generation or as a tie of it, finds an owner's plan newer than a plan after it.
func TestAPlanIsNewerByGenerationThenMadeThenID(t *testing.T) {
	plans := []madePlan{
		{plan: Plan{ID: "9c000000-0000-4000-8000-000000000000", Generation: 1, Source: PlanSourceOwner}, made: minute(50)},
		{plan: Plan{ID: "5c000000-0000-4000-8000-000000000000", Generation: 2, Source: PlanSourceDefault}, made: minute(10)},
		{plan: Plan{ID: "1c000000-0000-4000-8000-000000000000", Generation: 2, Source: PlanSourceOwner}, made: minute(30)},
		{plan: Plan{ID: "3c000000-0000-4000-8000-000000000000", Generation: 2, Source: PlanSourceDefault}, made: minute(30)},
		{plan: Plan{ID: "2c000000-0000-4000-8000-000000000000", Generation: 3, Source: PlanSourceDefault}, made: minute(0)},
	}
	text := func(plan madePlan) string {
		return fmt.Sprintf("generation %d made %s id %s source %s", plan.plan.Generation, plan.made.Format(time.RFC3339), plan.plan.ID, plan.plan.Source)
	}
	for older := range plans {
		if !plans[older].newerThan(nil) {
			t.Errorf("the plan of %s is not newer than nil", text(plans[older]))
		}
		for newer := range plans {
			if got, want := plans[newer].newerThan(&plans[older]), newer > older; got != want {
				t.Errorf("the plan of %s newer than the plan of %s: %t, want %t", text(plans[newer]), text(plans[older]), got, want)
			}
		}
	}
}

// since and until bound the close: an issue closed at since is listed and one closed at until is
// not.
func TestProjectPlansFiltersOnClosedAt(t *testing.T) {
	pool := storetest.Open(t).Pool
	early, middle, late := t0.Add(time.Hour), t0.Add(2*time.Hour), t0.Add(3*time.Hour)
	seedIssues(t, pool, "LEG",
		seededIssue{"LEG-1", "done", &early},
		seededIssue{"LEG-2", "done", &middle},
		seededIssue{"LEG-3", "done", &late},
	)
	for name, bounded := range map[string]struct {
		window Window
		want   []string
	}{
		"no bound":    {Window{}, []string{"LEG-3", "LEG-2", "LEG-1"}},
		"since":       {Window{Since: &middle}, []string{"LEG-3", "LEG-2"}},
		"until":       {Window{Until: &middle}, []string{"LEG-1"}},
		"both bounds": {Window{Since: &early, Until: &late}, []string{"LEG-2", "LEG-1"}},
	} {
		t.Run(name, func(t *testing.T) {
			read, err := ProjectPlans(context.Background(), pool, "LEG", bounded.window)
			if err != nil {
				t.Fatalf("read the plans of LEG: %v", err)
			}
			var keys []string
			for _, issue := range read {
				keys = append(keys, issue.Issue)
			}
			if !slices.Equal(keys, bounded.want) {
				t.Fatalf("the read lists %v, want %v", keys, bounded.want)
			}
		})
	}
}

// A bound finer than the microsecond PostgreSQL holds a close to keeps its meaning for every caller
// of the read, not the route alone: an issue closed at …00.123456 is before …00.123456001, so a
// since at that bound leaves it out and an until at it keeps it. pgx encodes a timestamptz by
// truncating it to the microsecond, which would move each comparison onto the close itself.
func TestProjectPlansKeepsASubMicrosecondBoundsMeaning(t *testing.T) {
	pool := storetest.Open(t).Pool
	closed := time.Date(2026, time.November, 3, 0, 0, 0, 123456000, time.UTC)
	seedIssues(t, pool, "LEG", seededIssue{"LEG-1", "done", &closed})
	before, after, later := closed.Add(-time.Nanosecond), closed.Add(time.Nanosecond), closed.Add(999*time.Nanosecond)
	for _, bounded := range []struct {
		name   string
		window Window
		want   []string
	}{
		{"since at the close", Window{Since: &closed}, []string{"LEG-1"}},
		{"since a nanosecond after the close", Window{Since: &after}, []string{}},
		{"until at the close", Window{Until: &closed}, []string{}},
		{"until a nanosecond after the close", Window{Until: &after}, []string{"LEG-1"}},
		{"a nanosecond either side of the close", Window{Since: &before, Until: &after}, []string{"LEG-1"}},
		{"both after the close in its microsecond", Window{Since: &after, Until: &later}, []string{}},
	} {
		read, err := ProjectPlans(context.Background(), pool, "LEG", bounded.window)
		if err != nil {
			t.Fatalf("%s: read the plans of LEG: %v", bounded.name, err)
		}
		keys := []string{}
		for _, issue := range read {
			keys = append(keys, issue.Issue)
		}
		if !slices.Equal(keys, bounded.want) {
			t.Errorf("%s lists %v, want %v", bounded.name, keys, bounded.want)
		}
	}
}
