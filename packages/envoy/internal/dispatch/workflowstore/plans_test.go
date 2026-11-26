package workflowstore

import (
	"context"
	"encoding/json"
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

// heldPlan reads plan id back from its node row, the row the read of a project takes it from.
func heldPlan(t *testing.T, pool *store.Pool, id string) Plan {
	t.Helper()
	var attributes []byte
	if err := pool.QueryRow(context.Background(),
		`select attributes from workflow_nodes where kind = $1 and id = $2`, KindPlan, id).Scan(&attributes); err != nil {
		t.Fatalf("read the node of plan %s: %v", id, err)
	}
	plan, err := planFrom(id, attributes)
	if err != nil {
		t.Fatal(err)
	}
	return plan
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

// The source, the document, the version and the tasks of a plan are one fact. A write of a version
// above the one the node holds replaces all four in one write, appending a value tuple for each,
// and a write at or below it changes nothing, appends nothing and answers the node as it stands,
// whatever generation, source and tasks it names. A default plan, which has no version, counts as
// version 0. The issue, the project and the generation are each set once, whatever the version.
// Each write is read back from the node.
func TestAPlanWriteOfAHigherVersionReplacesItsFact(t *testing.T) {
	pool := storetest.Open(t).Pool
	document := uuid.NewString()
	first := []PlanTask{{ID: "api", Steps: []string{"implementer", "reviewer"}}}
	second := []PlanTask{{ID: "api", Steps: []string{"implementer", "reviewer:fresh"}}, {ID: "docs", Steps: []string{"implementer"}}}
	other := []PlanTask{{ID: "docs", Steps: []string{"implementer", "tester"}}}

	owner := func(id string, version int, tasks []PlanTask) PlanWrite {
		return PlanWrite{
			ID: id, Issue: "LEG-1", Project: new("LEG"), Generation: new(int64(1)), Source: new(PlanSourceOwner),
			Document: PlanDocument{ArtifactID: document, Version: version}, Tasks: tasks,
		}
	}
	ownerHeld := func(id string, version int, tasks []PlanTask) Plan {
		return Plan{ID: id, Issue: "LEG-1", Project: "LEG", Generation: 1, Source: PlanSourceOwner,
			Document: &PlanDocument{ArtifactID: document, Version: version}, Tasks: tasks}
	}
	byDefault := func(id string) PlanWrite {
		write := defaultPlan(id, "LEG-1", 1)
		write.Project = new("LEG")
		return write
	}
	defaultHeld := func(id string) Plan {
		return Plan{ID: id, Issue: "LEG-1", Project: "LEG", Generation: 1, Source: PlanSourceDefault,
			Tasks: []PlanTask{{ID: "default", Steps: defaultSteps}}}
	}
	// planStep is one write of a case and the plan its node holds after it. replaced is whether the
	// write replaced the fact, appending the four value tuples of the plan it wants; a write after
	// the first that did not replace it appends nothing.
	type planStep struct {
		write    PlanWrite
		want     Plan
		replaced bool
	}

	for name, steps := range map[string]func(id string) []planStep{
		"version 2 after version 1 replaces the four fields": func(id string) []planStep {
			return []planStep{
				{write: owner(id, 1, first), want: ownerHeld(id, 1, first)},
				{write: owner(id, 2, second), want: ownerHeld(id, 2, second), replaced: true},
			}
		},
		"version 1 after version 2 changes nothing": func(id string) []planStep {
			return []planStep{
				{write: owner(id, 1, first), want: ownerHeld(id, 1, first)},
				{write: owner(id, 2, second), want: ownerHeld(id, 2, second), replaced: true},
				{write: owner(id, 1, other), want: ownerHeld(id, 2, second)},
			}
		},
		"the same version again changes nothing, also with other tasks": func(id string) []planStep {
			return []planStep{
				{write: owner(id, 1, first), want: ownerHeld(id, 1, first)},
				{write: owner(id, 2, second), want: ownerHeld(id, 2, second), replaced: true},
				{write: owner(id, 2, second), want: ownerHeld(id, 2, second)},
				{write: owner(id, 2, other), want: ownerHeld(id, 2, second)},
			}
		},
		"a default plan and then an owner's version 1": func(id string) []planStep {
			return []planStep{
				{write: byDefault(id), want: defaultHeld(id)},
				{write: owner(id, 1, first), want: ownerHeld(id, 1, first), replaced: true},
			}
		},
		"an owner's plan and then a default write of another generation changes nothing": func(id string) []planStep {
			elsewhere := byDefault(id)
			elsewhere.Generation = new(int64(10))
			return []planStep{
				{write: byDefault(id), want: defaultHeld(id)},
				{write: owner(id, 1, first), want: ownerHeld(id, 1, first), replaced: true},
				{write: elsewhere, want: ownerHeld(id, 1, first)},
			}
		},
		"a later version leaves the generation and the project the first write set": func(id string) []planStep {
			later := owner(id, 2, second)
			later.Generation, later.Project = new(int64(3)), new("OTHER")
			return []planStep{
				{write: owner(id, 1, first), want: ownerHeld(id, 1, first)},
				{write: later, want: ownerHeld(id, 2, second), replaced: true},
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			id := uuid.NewString()
			for index, step := range steps(id) {
				before := len(planLog(t, pool, id))
				at := minute(index)
				if answer := putPlan(t, pool, step.write, at); planText(answer) != planText(step.want) {
					t.Fatalf("write %d answers\n%s\nwant\n%s", index+1, planText(answer), planText(step.want))
				}
				if held := heldPlan(t, pool, id); planText(held) != planText(step.want) {
					t.Fatalf("after write %d the node holds\n%s\nwant\n%s", index+1, planText(held), planText(step.want))
				}
				if index == 0 {
					continue
				}
				var appended []string
				for _, tuple := range planLog(t, pool, id)[before:] {
					appended = append(appended, fmt.Sprintf("%s %s/%s at %s", tuple.kind, tuple.toKind, tuple.toID, tuple.at.UTC().Format(time.RFC3339)))
				}
				var want []string
				if step.replaced {
					tasks, err := json.Marshal(step.want.Tasks)
					if err != nil {
						t.Fatal(err)
					}
					var held PlanDocument
					if step.want.Document != nil {
						held = *step.want.Document
					}
					for _, value := range [][2]string{
						{AttributeSource, step.want.Source}, {AttributeArtifactID, held.ArtifactID},
						{AttributeVersion, strconv.Itoa(held.Version)}, {AttributeTasks, string(tasks)},
					} {
						want = append(want, fmt.Sprintf("%s %s/%s at %s", value[0], KindValue, value[1], at.Format(time.RFC3339)))
					}
				}
				if !slices.Equal(appended, want) {
					t.Fatalf("write %d appended %q, want %q", index+1, appended, want)
				}
			}
		})
	}
}

// A plan made with the source default holds its document and version empty, and holds no
// document, so a later owner's write of the same id leaves no part of the default beside its own
// fact: the owner's version, above the default's 0, replaces the source, the document, the version
// and the tasks whole (TestAPlanWriteOfAHigherVersionReplacesItsFact), and the write, a later write
// of the same default plan and the read of the project all answer the owner's plan as it was
// written, with no default task and no default source left in it.
func TestADefaultPlanTakesALaterOwnersPlanWhole(t *testing.T) {
	pool := storetest.Open(t).Pool
	closed := t0.Add(time.Hour)
	seedIssues(t, pool, "LEG", seededIssue{"LEG-1", "done", &closed})
	id := uuid.NewString()
	first := putPlan(t, pool, defaultPlan(id, "LEG-1", 1), minute(0))
	if want := defaultPlanRead(id, "LEG-1", 1); planText(first) != planText(want) {
		t.Fatalf("the default plan answers\n%s\nwant\n%s", planText(first), planText(want))
	}

	ownersPlan := PlanWrite{
		ID: id, Issue: "LEG-1", Generation: new(int64(1)), Source: new(PlanSourceOwner),
		Document: PlanDocument{ArtifactID: uuid.NewString(), Version: 3},
		Tasks:    []PlanTask{{ID: "ui", Steps: []string{"implementer"}}, {ID: "api", Steps: []string{"implementer", "reviewer"}}},
	}
	want := Plan{ID: id, Issue: "LEG-1", Source: PlanSourceOwner, Generation: 1, Document: &ownersPlan.Document, Tasks: ownersPlan.Tasks}
	if owner := putPlan(t, pool, ownersPlan, minute(1)); planText(owner) != planText(want) {
		t.Fatalf("a later owner's write answers\n%s\nwant the owner's plan whole\n%s", planText(owner), planText(want))
	}
	// A default write after the owner's plan is at a lower version: it changes nothing and
	// appends no tuple.
	before := wholeLog(t, pool)
	if again := putPlan(t, pool, defaultPlan(id, "LEG-1", 1), minute(2)); planText(again) != planText(want) {
		t.Fatalf("the default plan written again answers\n%s\nwant the owner's\n%s", planText(again), planText(want))
	}
	if after := wholeLog(t, pool); len(after) != len(before) {
		t.Fatalf("the default write after the owner's plan appended %d tuples, want none", len(after)-len(before))
	}

	read, err := ProjectPlans(context.Background(), pool, "LEG", Window{}, PlansFilter{})
	if err != nil {
		t.Fatalf("read the plans of LEG: %v", err)
	}
	if len(read.Issues) != 1 || read.Issues[0].Plan == nil || planText(*read.Issues[0].Plan) != planText(want) {
		t.Fatalf("the read lists %+v, want LEG-1 with the owner's plan\n%s", read.Issues, planText(want))
	}
}

// planLog is every tuple of the log stated about plan id, in the order it was appended.
func planLog(t *testing.T, pool *store.Pool, id string) []loggedTuple {
	t.Helper()
	var stated []loggedTuple
	for _, tuple := range wholeLog(t, pool) {
		if tuple.fromKind == KindPlan && tuple.fromID == id {
			stated = append(stated, tuple)
		}
	}
	return stated
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
	return fmt.Sprintf("%s %q closed %s plan %s ratings %v from %s to %s failed %d variant %s", issue.Issue, issue.Title,
		issue.ClosedAt.UTC().Format(time.RFC3339Nano), plan, ratings, moment(issue.StartedAt), moment(issue.EndedAt), issue.FailedSteps,
		variantText(issue.Experiment))
}

// variantText is an experiment and its variant as experiment/variant, or none.
func variantText(drawn *StepExperiment) string {
	if drawn == nil {
		return "none"
	}
	return drawn.Experiment + "/" + drawn.Variant
}

// The read of a project lists each issue of it that is done and closed, newest close first, with
// its plan, the rating of its retro, the first start and the last end of its records, its count of
// failed steps of kind step, and the experiment and the variant the records of its newest run name,
// all in one statement: a failed retro and a failed merge are no failed step, and no record here
// names an experiment, so each issue reads none. An issue still open, an issue done with no close,
// an issue closed that is not done, an issue of another project, LEGX's among them though its key
// starts with LEG, and a record of another issue are not read into it; a finished issue Legion
// never ran is listed with no plan, no rating, no time and no failed step.
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
	read, err := ProjectPlans(context.Background(), storetest.Traced(t, pool, counter, nil), "LEG", Window{}, PlansFilter{})
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
	got := make([]string, len(read.Issues))
	for index, issue := range read.Issues {
		got[index] = issuePlanText(issue)
	}
	wanted := make([]string, len(want))
	for index, issue := range want {
		wanted[index] = issuePlanText(issue)
	}
	if !slices.Equal(got, wanted) {
		t.Fatalf("the read lists\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wanted, "\n"))
	}

	other, err := ProjectPlans(context.Background(), pool, "LEGX", Window{}, PlansFilter{})
	if err != nil {
		t.Fatalf("read the plans of LEGX: %v", err)
	}
	if len(other.Issues) != 1 || other.Issues[0].Issue != "LEGX-1" || other.Issues[0].FailedSteps != 1 {
		t.Fatalf("the read of LEGX lists %+v, want LEGX-1 alone with its failed step", other.Issues)
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

	read, err := ProjectPlans(context.Background(), pool, "LEG", Window{}, PlansFilter{})
	if err != nil {
		t.Fatalf("read the plans of LEG: %v", err)
	}
	got := make([]string, len(read.Issues))
	for index, issue := range read.Issues {
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

	read, err := ProjectPlans(context.Background(), pool, "LEG", Window{}, PlansFilter{})
	if err != nil {
		t.Fatalf("read the plans of LEG: %v", err)
	}
	if len(read.Issues) != 1 || read.Issues[0].Plan == nil {
		t.Fatalf("the read lists %d issues, want LEG-1 with a plan", len(read.Issues))
	}
	newest := defaultPlanRead(newestID, "LEG-1", 40)
	if plan := read.Issues[0].Plan; planText(*plan) != planText(newest) {
		t.Fatalf("the plan of LEG-1 is\n%s\nwant the default plan of generation 40\n%s", planText(*plan), planText(newest))
	}
	want := IssuePlan{Issue: "LEG-1", Title: "Title of LEG-1", ClosedAt: closed, Plan: &newest,
		Ratings: []Rating{
			{Rating: 3, Reason: "The review came late.", StepID: first, EndedAt: new(minute(22))},
			{Rating: 1, Reason: "The plan was late.", StepID: second, EndedAt: new(minute(121))},
			{Rating: 4, Reason: "Two tasks were one.", StepID: third, EndedAt: new(minute(201))},
		},
		StartedAt: new(minute(20)), EndedAt: new(minute(201))}
	if got := issuePlanText(read.Issues[0]); got != issuePlanText(want) {
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

	read, err := ProjectPlans(context.Background(), pool, "LEG", Window{}, PlansFilter{})
	if err != nil {
		t.Fatalf("read the plans of LEG: %v", err)
	}
	got := make([]string, len(read.Issues))
	for index, issue := range read.Issues {
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
	plans := []MadePlan{
		{Plan: Plan{ID: "9c000000-0000-4000-8000-000000000000", Generation: 1, Source: PlanSourceOwner}, Made: minute(50)},
		{Plan: Plan{ID: "5c000000-0000-4000-8000-000000000000", Generation: 2, Source: PlanSourceDefault}, Made: minute(10)},
		{Plan: Plan{ID: "1c000000-0000-4000-8000-000000000000", Generation: 2, Source: PlanSourceOwner}, Made: minute(30)},
		{Plan: Plan{ID: "3c000000-0000-4000-8000-000000000000", Generation: 2, Source: PlanSourceDefault}, Made: minute(30)},
		{Plan: Plan{ID: "2c000000-0000-4000-8000-000000000000", Generation: 3, Source: PlanSourceDefault}, Made: minute(0)},
	}
	text := func(plan MadePlan) string {
		return fmt.Sprintf("generation %d made %s id %s source %s", plan.Plan.Generation, plan.Made.Format(time.RFC3339), plan.Plan.ID, plan.Plan.Source)
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

// A finished issue names the experiment and the variant the records of its newest run name, its
// highest generation, and none where those records name none. LEG-1's first run names planning/a
// and its second planning/b, the second's record written first, so a read that took the variant of
// an older run, or of the first record it met, answers planning/a. LEG-2's records name none, and
// LEG-3's first run names one while its newest names none yet, as a run whose variant still waits
// for Dispatch's route does: it names none, never the older run's.
func TestAFinishedIssueNamesItsVariant(t *testing.T) {
	pool := storetest.Open(t).Pool
	seedIssues(t, pool, "LEG", seededIssue{"LEG-1", "done", new(t0.Add(5 * time.Hour))},
		seededIssue{"LEG-2", "done", new(t0.Add(4 * time.Hour))}, seededIssue{"LEG-3", "done", new(t0.Add(3 * time.Hour))})
	// run writes the start of the implementer's step of issue's generation, with the variant drawn
	// where it names one.
	run := func(issue string, generation int64, drawn *StepExperiment) {
		t.Helper()
		id := stepRun(t, pool, issue, StepKindStep, "implementer", 2, generation, new(minute(int(generation)*10)), nil, "")
		if drawn != nil {
			put(t, pool, StepWrite{ID: id, Issue: issue, Experiment: drawn}, minute(int(generation)*10))
		}
	}
	run("LEG-1", 2, &StepExperiment{Experiment: "planning", Variant: "b"})
	run("LEG-1", 1, &StepExperiment{Experiment: "planning", Variant: "a"})
	run("LEG-2", 1, nil)
	run("LEG-3", 1, &StepExperiment{Experiment: "planning", Variant: "a"})
	run("LEG-3", 2, nil)

	read, err := ProjectPlans(context.Background(), pool, "LEG", Window{}, PlansFilter{})
	if err != nil {
		t.Fatalf("read the plans of LEG: %v", err)
	}
	got := make([]string, len(read.Issues))
	for index, issue := range read.Issues {
		got[index] = issue.Issue + " " + variantText(issue.Experiment)
	}
	if want := []string{"LEG-1 planning/b", "LEG-2 none", "LEG-3 none"}; !slices.Equal(got, want) {
		t.Fatalf("the read names the variants %v, want %v", got, want)
	}
}

// The read of some issues' plans answers each issue's plans with the moment each node was made,
// oldest first, and nothing for an issue Legion never ran or one it was not asked about.
func TestIssuesPlansAnswersEachIssuesPlansOldestFirst(t *testing.T) {
	pool := storetest.Open(t).Pool
	second := putPlan(t, pool, defaultPlan(uuid.NewString(), "LEG-1", 2), minute(10))
	first := putPlan(t, pool, defaultPlan(uuid.NewString(), "LEG-1", 1), minute(20))
	putPlan(t, pool, defaultPlan(uuid.NewString(), "LEG-2", 1), minute(5))
	got, err := IssuesPlans(context.Background(), pool, []string{"LEG-1", "LEG-3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got["LEG-1"]) != 2 {
		t.Fatalf("the read answered %v, want two plans of LEG-1 and nothing else", got)
	}
	if older, newer := got["LEG-1"][0], got["LEG-1"][1]; older.Plan.ID != first.ID || newer.Plan.ID != second.ID ||
		!older.Made.Equal(minute(20)) || !newer.Made.Equal(minute(10)) {
		t.Fatalf("the plans of LEG-1 are %s made %s, then %s made %s, want generation 1's, then generation 2's",
			planText(older.Plan), older.Made, planText(newer.Plan), newer.Made)
	}
}

// One run keeps one plan node, whose made_at is its first write's, and a later version replaces the
// node's tasks whole, with a value tuple of the new tasks at the moment of its write. So the read
// answers each version of the run's plan at the moment its own write made it, oldest first: the
// metrics' fallback timeline then reads each mark by the version in force at its time, not the
// whole run by the newest.
func TestIssuesPlansAnswersEachVersionOfARunsPlanAtItsOwnTime(t *testing.T) {
	pool := storetest.Open(t).Pool
	id, document := uuid.NewString(), uuid.NewString()
	version := func(n int, steps ...string) PlanWrite {
		return PlanWrite{ID: id, Issue: "LEG-1", Generation: new(int64(1)), Source: new(PlanSourceOwner),
			Document: PlanDocument{ArtifactID: document, Version: n}, Tasks: []PlanTask{{ID: "a", Steps: steps}}}
	}
	putPlan(t, pool, version(1, "implementer", "tester", "reviewer"), minute(10))
	putPlan(t, pool, version(2, "implementer", "reviewer", "tester"), minute(30))
	got, err := IssuesPlans(context.Background(), pool, []string{"LEG-1"})
	if err != nil {
		t.Fatal(err)
	}
	plans := got["LEG-1"]
	if len(plans) != 2 {
		t.Fatalf("the read answered %d plans of LEG-1 (%v), want both versions of its one run", len(plans), plans)
	}
	for i, want := range []struct {
		made    time.Time
		version int
		steps   []string
	}{
		{minute(10), 1, []string{"implementer", "tester", "reviewer"}},
		{minute(30), 2, []string{"implementer", "reviewer", "tester"}},
	} {
		plan := plans[i]
		if plan.Plan.ID != id || !plan.Made.Equal(want.made) || plan.Plan.Document == nil || plan.Plan.Document.Version != want.version ||
			len(plan.Plan.Tasks) != 1 || !slices.Equal(plan.Plan.Tasks[0].Steps, want.steps) {
			t.Fatalf("plan %d of LEG-1 is %s made %s, want version %d with steps %v made %s", i, planText(plan.Plan), plan.Made, want.version, want.steps, want.made)
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
			read, err := ProjectPlans(context.Background(), pool, "LEG", bounded.window, PlansFilter{})
			if err != nil {
				t.Fatalf("read the plans of LEG: %v", err)
			}
			var keys []string
			for _, issue := range read.Issues {
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
		read, err := ProjectPlans(context.Background(), pool, "LEG", bounded.window, PlansFilter{})
		if err != nil {
			t.Fatalf("%s: read the plans of LEG: %v", bounded.name, err)
		}
		keys := []string{}
		for _, issue := range read.Issues {
			keys = append(keys, issue.Issue)
		}
		if !slices.Equal(keys, bounded.want) {
			t.Errorf("%s lists %v, want %v", bounded.name, keys, bounded.want)
		}
	}
}

// rowCounter is a Queryer that counts the rows its statements answer, so a test sees what the
// database sent back and not only what the read kept of it.
type rowCounter struct {
	Queryer
	rows int
}

func (c *rowCounter) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := c.Queryer.Query(ctx, sql, args...)
	if err != nil {
		return rows, err
	}
	return &countedRows{Rows: rows, count: &c.rows}, nil
}

// countedRows adds one to count for each row it answers.
type countedRows struct {
	pgx.Rows
	count *int
}

func (r *countedRows) Next() bool {
	next := r.Rows.Next()
	if next {
		*r.count++
	}
	return next
}

// seedLearnedFrom writes LEG's twelve finished issues, LEG-n closed n hours after t0, of which four
// have a plan or a rating: LEG-2 a plan alone, LEG-5 a rated retro alone, LEG-7 a plan, a step and
// a rated retro, and LEG-11 a plan and a rated retro. LEG-4 ran a failed step and LEG-9 a retro
// that gave no rating, so each has runs and neither a plan nor a rating; LEG-12, the newest, Legion
// never ran. LEG-13, still in progress, has a plan, and OTH-1, of another project and closed after
// all of them, has a plan and a rated retro. It answers the four with a plan or a rating as the
// read lists each, newest close first.
func seedLearnedFrom(t *testing.T, pool *store.Pool) []IssuePlan {
	t.Helper()
	closed := func(hours int) time.Time { return t0.Add(time.Duration(hours) * time.Hour) }
	issues := make([]seededIssue, 0, 13)
	for n := 1; n <= 12; n++ {
		issues = append(issues, seededIssue{"LEG-" + strconv.Itoa(n), "done", new(closed(n))})
	}
	seedIssues(t, pool, "LEG", append(issues, seededIssue{"LEG-13", "in_progress", nil})...)
	seedIssues(t, pool, "OTH", seededIssue{"OTH-1", "done", new(closed(20))})

	planTwo, planSeven, planEleven := uuid.NewString(), uuid.NewString(), uuid.NewString()
	putPlan(t, pool, defaultPlan(planTwo, "LEG-2", 1), minute(0))
	stepRun(t, pool, "LEG-4", StepKindStep, "tester", 3, 1, new(minute(10)), new(minute(12)), ResultFailed)
	retroFive := ratedRetro(t, pool, "LEG-5", 1, minute(20), minute(21), 2, "The plan left the tests out.")
	putPlan(t, pool, defaultPlan(planSeven, "LEG-7", 1), minute(30))
	stepRun(t, pool, "LEG-7", StepKindStep, "planner", 1, 1, new(minute(30)), new(minute(35)), ResultPassed)
	retroSeven := ratedRetro(t, pool, "LEG-7", 1, minute(36), minute(37), 5, "The plan fit the work.")
	stepRun(t, pool, "LEG-9", StepKindRetro, "implementer", 0, 1, new(minute(40)), new(minute(41)), ResultPassed)
	putPlan(t, pool, defaultPlan(planEleven, "LEG-11", 1), minute(50))
	retroEleven := ratedRetro(t, pool, "LEG-11", 1, minute(51), minute(52), 4, "The review came late.")
	putPlan(t, pool, defaultPlan(uuid.NewString(), "LEG-13", 1), minute(60))
	putPlan(t, pool, defaultPlan(uuid.NewString(), "OTH-1", 1), minute(70))
	ratedRetro(t, pool, "OTH-1", 1, minute(71), minute(72), 3, "Enough.")

	return []IssuePlan{
		{Issue: "LEG-11", Title: "Title of LEG-11", ClosedAt: closed(11), Plan: new(defaultPlanRead(planEleven, "LEG-11", 1)),
			Ratings:   []Rating{{Rating: 4, Reason: "The review came late.", StepID: retroEleven, EndedAt: new(minute(52))}},
			StartedAt: new(minute(51)), EndedAt: new(minute(52))},
		{Issue: "LEG-7", Title: "Title of LEG-7", ClosedAt: closed(7), Plan: new(defaultPlanRead(planSeven, "LEG-7", 1)),
			Ratings:   []Rating{{Rating: 5, Reason: "The plan fit the work.", StepID: retroSeven, EndedAt: new(minute(37))}},
			StartedAt: new(minute(30)), EndedAt: new(minute(37))},
		{Issue: "LEG-5", Title: "Title of LEG-5", ClosedAt: closed(5),
			Ratings:   []Rating{{Rating: 2, Reason: "The plan left the tests out.", StepID: retroFive, EndedAt: new(minute(21))}},
			StartedAt: new(minute(20)), EndedAt: new(minute(21))},
		{Issue: "LEG-2", Title: "Title of LEG-2", ClosedAt: closed(2), Plan: new(defaultPlanRead(planTwo, "LEG-2", 1))},
	}
}

// With PlannedOrRated and a Limit the read lists the newest Limit of a project's finished issues
// that have a plan or a rating, each whole, and the statement keeps them itself: one statement
// whose rows are those of the listed issues alone, the two nodes of LEG-11, the three of LEG-7 and
// the one of LEG-5, beside Total, the four issues of the window with a plan or a rating, and
// FinishedTotal, its twelve finished issues. The newest finished issue has neither a plan nor a
// rating, and so do most of the others, so a read that cut the issues to the limit before it kept
// those with a plan or a rating lists others, and one that read every issue and kept them after
// answers more rows.
func TestProjectPlansKeepsTheNewestPlannedOrRatedUpToTheLimitInTheStatement(t *testing.T) {
	pool := storetest.Open(t).Pool
	kept := seedLearnedFrom(t, pool)

	counter := &storetest.StatementCounter{}
	rows := &rowCounter{Queryer: storetest.Traced(t, pool, counter, nil)}
	read, err := ProjectPlans(context.Background(), rows, "LEG", Window{}, PlansFilter{PlannedOrRated: true, Limit: 3})
	if err != nil {
		t.Fatalf("read the plans of LEG: %v", err)
	}
	got := make([]string, len(read.Issues))
	for index, issue := range read.Issues {
		got[index] = issuePlanText(issue)
	}
	wanted := make([]string, 3)
	for index, issue := range kept[:3] {
		wanted[index] = issuePlanText(issue)
	}
	if !slices.Equal(got, wanted) {
		t.Fatalf("the read lists\n%s\nwant the three newest with a plan or a rating\n%s", strings.Join(got, "\n"), strings.Join(wanted, "\n"))
	}
	if read.Total != 4 || read.FinishedTotal != 12 {
		t.Errorf("the read counts %d with a plan or a rating of %d finished, want 4 of 12", read.Total, read.FinishedTotal)
	}
	if sent := counter.Count(); sent != 1 {
		t.Errorf("the read sent %d statements, want one", sent)
	}
	if rows.rows != 6 {
		t.Errorf("the statement answered %d rows, want 6: the nodes of LEG-11, LEG-7 and LEG-5 and no row of another issue", rows.rows)
	}
}

// PlannedOrRated and Limit each narrow the read alone, and the window narrows the two counts as it
// narrows the issues: Total counts the issues of the window the filter keeps, which with no filter
// is every finished one, and FinishedTotal every finished issue of the window. A window that holds
// no issue with a plan or a rating lists none and still counts its finished issues. The zero
// filter lists every finished issue, as the read did before it had one.
func TestProjectPlansFilterAndLimitEachNarrowTheRead(t *testing.T) {
	pool := storetest.Open(t).Pool
	seedLearnedFrom(t, pool)
	hours := func(n int) *time.Time { return new(t0.Add(time.Duration(n) * time.Hour)) }
	everyKey := []string{"LEG-12", "LEG-11", "LEG-10", "LEG-9", "LEG-8", "LEG-7", "LEG-6", "LEG-5", "LEG-4", "LEG-3", "LEG-2", "LEG-1"}
	plannedOrRated := []string{"LEG-11", "LEG-7", "LEG-5", "LEG-2"}
	for _, bounded := range []struct {
		name            string
		window          Window
		filter          PlansFilter
		want            []string
		total, finished int
	}{
		{"no filter", Window{}, PlansFilter{}, everyKey, 12, 12},
		{"a plan or a rating alone", Window{}, PlansFilter{PlannedOrRated: true}, plannedOrRated, 4, 12},
		{"a limit alone", Window{}, PlansFilter{Limit: 2}, []string{"LEG-12", "LEG-11"}, 12, 12},
		{"a limit above what is kept", Window{}, PlansFilter{PlannedOrRated: true, Limit: 10}, plannedOrRated, 4, 12},
		{"both within a window", Window{Until: hours(7)}, PlansFilter{PlannedOrRated: true, Limit: 1}, []string{"LEG-5"}, 2, 6},
		{"a window with none kept", Window{Until: hours(2)}, PlansFilter{PlannedOrRated: true, Limit: 3}, []string{}, 0, 1},
	} {
		read, err := ProjectPlans(context.Background(), pool, "LEG", bounded.window, bounded.filter)
		if err != nil {
			t.Fatalf("%s: read the plans of LEG: %v", bounded.name, err)
		}
		keys := []string{}
		for _, issue := range read.Issues {
			keys = append(keys, issue.Issue)
		}
		if !slices.Equal(keys, bounded.want) || read.Total != bounded.total || read.FinishedTotal != bounded.finished {
			t.Errorf("%s lists %v and counts %d of %d finished, want %v and %d of %d", bounded.name, keys, read.Total,
				read.FinishedTotal, bounded.want, bounded.total, bounded.finished)
		}
	}
}
