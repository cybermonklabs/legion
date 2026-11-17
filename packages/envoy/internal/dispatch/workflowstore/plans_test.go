package workflowstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

// putPlan writes one plan in a transaction of its own and commits it, as the route does.
func putPlan(t *testing.T, pool *store.Pool, write PlanWrite, at time.Time) Plan {
	t.Helper()
	var written Plan
	if err := tryWrite(pool, func(tx pgx.Tx) error {
		var err error
		written, err = PutPlan(context.Background(), tx, write, daemon, at)
		return err
	}); err != nil {
		t.Fatalf("write plan %s: %v", write.ID, err)
	}
	return written
}

// planText is a plan as one line, so two plans compare whole and a difference prints whole.
func planText(plan Plan) string {
	return fmt.Sprintf("%s %s gen %d %s artifact %q version %d tasks %v", plan.ID, plan.Issue, plan.Generation,
		plan.Source, plan.ArtifactID, plan.Version, plan.Tasks)
}

// A plan is one node of kind plan, part of its issue by one relation tuple, holding its source, its
// run and its tasks, each attribute stated by one value tuple.
func TestAPlanIsANodePartOfItsIssue(t *testing.T) {
	pool := storetest.Open(t).Pool
	id := uuid.NewString()

	written := putPlan(t, pool, defaultPlan(id, "LEG-1", 1), minute(0))
	want := Plan{ID: id, Issue: "LEG-1", Source: PlanSourceDefault, Generation: 1,
		Tasks: []PlanTask{{ID: "default", Steps: defaultSteps}}}
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

// Each field of a plan is written once: a later write of the same id with other tasks is taken,
// changes nothing and appends nothing. An owner's plan names its document and version.
func TestAPlanWriteKeepsTheFirst(t *testing.T) {
	pool := storetest.Open(t).Pool
	id, document := uuid.NewString(), uuid.NewString()
	owner := PlanWrite{
		ID: id, Issue: "LEG-1", Generation: new(int64(2)), Source: new(PlanSourceOwner),
		ArtifactID: new(document), Version: new(3),
		Tasks: []PlanTask{{ID: "api", Steps: []string{"implementer", "reviewer:fresh"}}, {ID: "docs", Steps: []string{"implementer"}}},
	}
	first := putPlan(t, pool, owner, minute(0))
	before := wholeLog(t, pool)

	again := putPlan(t, pool, defaultPlan(id, "LEG-1", 2), minute(1))
	if planText(again) != planText(first) {
		t.Fatalf("a second write answers\n%s\nwant the first\n%s", planText(again), planText(first))
	}
	if after := wholeLog(t, pool); len(after) != len(before) {
		t.Fatalf("a second write appended %d tuples, want none", len(after)-len(before))
	}
	if first.ArtifactID != document || first.Version != 3 || len(first.Tasks) != 2 {
		t.Fatalf("the owner's plan answers %s, want its document %s at version 3 and two tasks", planText(first), document)
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
		"artifact_id of default":    func(write *PlanWrite) { write.ArtifactID = new(uuid.NewString()) },
		"version of default":        func(write *PlanWrite) { write.Version = new(1) },
		"artifact_id of owner none": func(write *PlanWrite) { write.Source, write.Version = new(PlanSourceOwner), new(1) },
		"artifact_id not a uuid": func(write *PlanWrite) {
			write.Source, write.ArtifactID, write.Version = new(PlanSourceOwner), new("plan-md"), new(1)
		},
		"version of owner none": func(write *PlanWrite) {
			write.Source, write.ArtifactID = new(PlanSourceOwner), new(uuid.NewString())
		},
		"id":    func(write *PlanWrite) { write.ID = "not-a-uuid" },
		"issue": func(write *PlanWrite) { write.Issue = "" },
	} {
		t.Run(name, func(t *testing.T) {
			write := defaultPlan(uuid.NewString(), "LEG-1", 1)
			change(&write)
			err := tryWrite(pool, func(tx pgx.Tx) error {
				_, err := PutPlan(context.Background(), tx, write, daemon, minute(0))
				return err
			})
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

// An id the plan of one issue holds is refused ErrPlanOtherIssue to a write naming another issue.
func TestAPlanIDOfAnotherIssueIsRefused(t *testing.T) {
	pool := storetest.Open(t).Pool
	id := uuid.NewString()
	putPlan(t, pool, defaultPlan(id, "LEG-1", 1), minute(0))
	before := wholeLog(t, pool)

	err := tryWrite(pool, func(tx pgx.Tx) error {
		_, err := PutPlan(context.Background(), tx, defaultPlan(id, "LEG-2", 1), daemon, minute(1))
		return err
	})
	if !errors.Is(err, ErrPlanOtherIssue) || !strings.Contains(err.Error(), "LEG-1") || !strings.Contains(err.Error(), "LEG-2") {
		t.Fatalf("the write answered %v, want %v naming LEG-1 and LEG-2", err, ErrPlanOtherIssue)
	}
	if after := wholeLog(t, pool); len(after) != len(before) {
		t.Fatalf("the refused write appended %d tuples, want none", len(after)-len(before))
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

// countingQueryer is a Queryer that counts the statements it is sent.
type countingQueryer struct {
	Queryer
	statements int
}

func (q *countingQueryer) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	q.statements++
	return q.Queryer.Query(ctx, sql, args...)
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

// issuePlanText is one issue of the read as one line, its times in UTC, so a difference prints
// whole.
func issuePlanText(issue IssuePlan) string {
	moment := func(at *time.Time) string {
		if at == nil {
			return "-"
		}
		return at.UTC().Format(time.RFC3339)
	}
	plan := "none"
	if issue.Plan != nil {
		plan = fmt.Sprintf("%s gen %d %v", issue.Plan.Source, issue.Plan.Generation, issue.Plan.Tasks)
	}
	ratings := make([]string, len(issue.Ratings))
	for index, rating := range issue.Ratings {
		ratings[index] = fmt.Sprintf("%d %q %s %s", rating.Rating, rating.Reason, rating.StepID, moment(rating.EndedAt))
	}
	return fmt.Sprintf("%s %q closed %s plan %s ratings %v from %s to %s failed %d", issue.Issue, issue.Title,
		issue.ClosedAt.UTC().Format(time.RFC3339), plan, ratings, moment(issue.StartedAt), moment(issue.EndedAt), issue.FailedSteps)
}

// The read of a project lists each issue of it that is done and closed, newest close first, with
// its plan, the rating of its retro, the first start and the last end of its records and its
// count of failed steps of kind step, all in one statement. An issue still open, an issue of
// another project and a record of another issue are not read into it; a finished issue Legion
// never ran is listed with no plan, no rating, no time and no failed step.
func TestProjectPlansListsEachFinishedIssueInOneStatement(t *testing.T) {
	pool := storetest.Open(t).Pool
	closedFirst, closedSecond, closedThird := t0.Add(time.Hour), t0.Add(2*time.Hour), t0.Add(3*time.Hour)
	seedIssues(t, pool, "LEG",
		seededIssue{"LEG-1", "done", &closedSecond},
		seededIssue{"LEG-2", "done", &closedThird},
		seededIssue{"LEG-3", "in_progress", nil},
		seededIssue{"LEG-4", "done", &closedFirst},
	)
	seedIssues(t, pool, "OTH", seededIssue{"OTH-1", "done", &closedThird})

	putPlan(t, pool, defaultPlan(uuid.NewString(), "LEG-1", 1), minute(0))
	stepRun(t, pool, "LEG-1", StepKindStep, "planner", 1, 1, new(minute(0)), new(minute(5)), ResultPassed)
	stepRun(t, pool, "LEG-1", StepKindStep, "reviewer", 4, 1, new(minute(10)), new(minute(15)), ResultFailed)
	retroOne := ratedRetro(t, pool, "LEG-1", 1, minute(20), minute(22), 4, "The tester found the bug.")
	stepRun(t, pool, "LEG-1", StepKindMerge, "merger", 0, 1, nil, new(minute(30)), ResultPassed)

	putPlan(t, pool, defaultPlan(uuid.NewString(), "LEG-2", 1), minute(60))
	stepRun(t, pool, "LEG-2", StepKindStep, "planner", 1, 1, new(minute(60)), new(minute(70)), ResultPassed)
	retroTwo := ratedRetro(t, pool, "LEG-2", 1, minute(71), minute(72), 5, "The plan fit the work.")
	stepRun(t, pool, "LEG-2", StepKindMerge, "merger", 0, 1, nil, new(minute(75)), ResultPassed)

	putPlan(t, pool, defaultPlan(uuid.NewString(), "LEG-3", 1), minute(80))
	stepRun(t, pool, "LEG-3", StepKindStep, "planner", 1, 1, new(minute(80)), new(minute(85)), ResultFailed)
	putPlan(t, pool, defaultPlan(uuid.NewString(), "OTH-1", 1), minute(80))
	stepRun(t, pool, "OTH-1", StepKindStep, "reviewer", 4, 1, new(minute(80)), new(minute(85)), ResultFailed)

	counted := &countingQueryer{Queryer: pool}
	read, err := ProjectPlans(context.Background(), counted, "LEG", nil, nil)
	if err != nil {
		t.Fatalf("read the plans of LEG: %v", err)
	}
	if counted.statements != 1 {
		t.Fatalf("the read sent %d statements, want one for the whole project", counted.statements)
	}
	plan := &Plan{Source: PlanSourceDefault, Generation: 1, Tasks: []PlanTask{{ID: "default", Steps: defaultSteps}}}
	want := []IssuePlan{
		{Issue: "LEG-2", Title: "Title of LEG-2", ClosedAt: closedThird, Plan: plan,
			Ratings:   []Rating{{Rating: 5, Reason: "The plan fit the work.", StepID: retroTwo, EndedAt: new(minute(72))}},
			StartedAt: new(minute(60)), EndedAt: new(minute(75)), FailedSteps: 0},
		{Issue: "LEG-1", Title: "Title of LEG-1", ClosedAt: closedSecond, Plan: plan,
			Ratings:   []Rating{{Rating: 4, Reason: "The tester found the bug.", StepID: retroOne, EndedAt: new(minute(22))}},
			StartedAt: new(minute(0)), EndedAt: new(minute(30)), FailedSteps: 1},
		{Issue: "LEG-4", Title: "Title of LEG-4", ClosedAt: closedFirst, Ratings: []Rating{}},
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
	for _, issue := range read {
		if issue.Ratings == nil {
			t.Fatalf("%s lists its ratings as nil, want an empty list", issue.Issue)
		}
	}
}

// The plan is the newest node of the issue, the one of the highest generation, and every retro's
// rating is listed, in the order the retros ran.
func TestProjectPlansTakesTheNewestPlanAndEveryRating(t *testing.T) {
	pool := storetest.Open(t).Pool
	closed := t0.Add(5 * time.Hour)
	seedIssues(t, pool, "LEG", seededIssue{"LEG-1", "done", &closed})
	document := uuid.NewString()
	putPlan(t, pool, PlanWrite{
		ID: uuid.NewString(), Issue: "LEG-1", Generation: new(int64(2)), Source: new(PlanSourceOwner),
		ArtifactID: new(document), Version: new(4), Tasks: []PlanTask{{ID: "api", Steps: []string{"implementer"}}},
	}, minute(100))
	putPlan(t, pool, defaultPlan(uuid.NewString(), "LEG-1", 1), minute(0))
	second := ratedRetro(t, pool, "LEG-1", 2, minute(120), minute(121), 2, "Two tasks were one.")
	first := ratedRetro(t, pool, "LEG-1", 1, minute(20), minute(22), 3, "The plan was late.")

	read, err := ProjectPlans(context.Background(), pool, "LEG", nil, nil)
	if err != nil {
		t.Fatalf("read the plans of LEG: %v", err)
	}
	if len(read) != 1 || read[0].Plan == nil {
		t.Fatalf("the read lists %d issues, want LEG-1 with a plan", len(read))
	}
	if plan := read[0].Plan; plan.Source != PlanSourceOwner || plan.Generation != 2 || plan.ArtifactID != document || plan.Version != 4 {
		t.Fatalf("the plan of LEG-1 is %s, want the owner's plan of generation 2", planText(*plan))
	}
	var ids []string
	for _, rating := range read[0].Ratings {
		ids = append(ids, rating.StepID)
	}
	if want := []string{first, second}; !slices.Equal(ids, want) {
		t.Fatalf("the ratings are of %v, want both retros in the order they ran, %v", ids, want)
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
	for name, window := range map[string]struct {
		since, until *time.Time
		want         []string
	}{
		"no bound":    {nil, nil, []string{"LEG-3", "LEG-2", "LEG-1"}},
		"since":       {&middle, nil, []string{"LEG-3", "LEG-2"}},
		"until":       {nil, &middle, []string{"LEG-1"}},
		"both bounds": {&early, &late, []string{"LEG-2", "LEG-1"}},
	} {
		t.Run(name, func(t *testing.T) {
			read, err := ProjectPlans(context.Background(), pool, "LEG", window.since, window.until)
			if err != nil {
				t.Fatalf("read the plans of LEG: %v", err)
			}
			var keys []string
			for _, issue := range read {
				keys = append(keys, issue.Issue)
			}
			if !slices.Equal(keys, window.want) {
				t.Fatalf("the read lists %v, want %v", keys, window.want)
			}
		})
	}
}
