package workflowstore

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// daemon is the one writer of a step record: the session of the project's Legion daemon.
var daemon = model.Actor{Kind: "session", ID: "legion-daemon:LEG"}

// t0 is the moment every test's clock starts at. A step's times live in the node's attributes
// and in its value tuples as text, so they come back as they were written.
var t0 = time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)

func minute(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

// implementer is the identity every write of the implementer's step of LEG-1 carries: task
// default, step 2, kind step, role implementer, type implementation, its agent and generation 1.
func implementer(id string) StepWrite {
	return StepWrite{
		ID:         id,
		Issue:      "LEG-1",
		Task:       new("default"),
		Place:      new(2),
		Kind:       new(StepKindStep),
		Role:       new("implementer"),
		Type:       new("implementation"),
		Agent:      new("legion-leg-leg-1-implementer"),
		Generation: new(int64(1)),
	}
}

// startOf is the write that starts a step: its identity and the moment it started.
func startOf(identity StepWrite, started time.Time) StepWrite {
	identity.StartedAt = new(started)
	return identity
}

// endOf is the write that ends a step: its identity, its result and the moment it ended.
func endOf(identity StepWrite, result string, ended time.Time) StepWrite {
	identity.Result = new(result)
	identity.EndedAt = new(ended)
	return identity
}

// A step's start is one node of kind step, part of its issue by one asserted relation tuple, and
// the read of that issue lists it, with attempt 1. The read of another issue lists nothing.
func TestAStepIsANodePartOfItsIssue(t *testing.T) {
	pool := storetest.Open(t).Pool
	id := uuid.NewString()

	written := put(t, pool, startOf(implementer(id), minute(0)), minute(0))
	if written.ID != id || written.Issue != "LEG-1" || written.Role != "implementer" {
		t.Fatalf("the write answered %+v, want the node it wrote", written)
	}

	steps := issueSteps(t, pool, "LEG-1")
	if len(steps) != 1 {
		t.Fatalf("LEG-1 lists %d steps, want the one written: %+v", len(steps), steps)
	}
	step := steps[0]
	if step.ID != id || step.Task != "default" || step.Place != 2 || step.Kind != StepKindStep ||
		step.Role != "implementer" || step.Type != "implementation" || step.Generation != 1 ||
		step.Agent != "legion-leg-leg-1-implementer" || step.Attempt != 1 {
		t.Errorf("LEG-1 lists %+v, want the implementer's step 2 of task default, attempt 1", step)
	}
	if step.StartedAt == nil || !step.StartedAt.Equal(minute(0)) || step.EndedAt != nil || step.Result != "" {
		t.Errorf("the step reads started %v ended %v result %q, want started %s and still running",
			step.StartedAt, step.EndedAt, step.Result, minute(0))
	}

	var relations int
	if err := pool.QueryRow(context.Background(), `
		select count(*) from workflow_tuples
		where from_kind = 'step' and from_id = $1 and kind = 'part_of'
			and to_kind = 'issue' and to_id = 'LEG-1' and asserted
	`, id).Scan(&relations); err != nil {
		t.Fatalf("count the part_of tuple: %v", err)
	}
	if relations != 1 {
		t.Errorf("the log holds %d asserted part_of tuples from the step to LEG-1, want 1", relations)
	}
	if other := issueSteps(t, pool, "LEG-2"); len(other) != 0 {
		t.Errorf("LEG-2 lists %+v, want no step: the one written is part of LEG-1", other)
	}
}

// The end of a step keeps every field its start wrote and adds its own: its result, its end, the
// place it returned to, the model that answered and the handoff it reported, which is one
// `reports` relation to that handoff.
func TestTheEndWriteKeepsTheStart(t *testing.T) {
	pool := storetest.Open(t).Pool
	id, report := uuid.NewString(), uuid.NewString()

	put(t, pool, startOf(implementer(id), minute(0)), minute(0))
	end := endOf(implementer(id), ResultFailed, minute(1))
	end.ReturnedTo = new(2)
	end.Model = new("anthropic/claude-opus-5")
	end.Report = new(report)
	answered := put(t, pool, end, minute(1))

	for name, step := range map[string]Step{"the write's answer": answered, "the read": issueSteps(t, pool, "LEG-1")[0].Step} {
		if step.Role != "implementer" || step.Agent != "legion-leg-leg-1-implementer" {
			t.Errorf("%s holds role %q agent %q, want the start's", name, step.Role, step.Agent)
		}
		if step.StartedAt == nil || !step.StartedAt.Equal(minute(0)) {
			t.Errorf("%s holds started_at %v, want the start's %s", name, step.StartedAt, minute(0))
		}
		if step.Result != ResultFailed || step.EndedAt == nil || !step.EndedAt.Equal(minute(1)) || step.ReturnedTo == nil || *step.ReturnedTo != 2 {
			t.Errorf("%s holds result %q ended %v returned_to %v, want failed at %s to step 2",
				name, step.Result, step.EndedAt, step.ReturnedTo, minute(1))
		}
		if step.Model != "anthropic/claude-opus-5" || step.Report != report {
			t.Errorf("%s holds model %q report %q, want anthropic/claude-opus-5 and %s", name, step.Model, step.Report, report)
		}
	}

	var reports int
	if err := pool.QueryRow(context.Background(), `
		select count(*) from workflow_tuples
		where from_kind = 'step' and from_id = $1 and kind = 'reports'
			and to_kind = 'handoff' and to_id = $2 and asserted
	`, id, report).Scan(&reports); err != nil {
		t.Fatalf("count the reports tuple: %v", err)
	}
	if reports != 1 {
		t.Errorf("the log holds %d reports tuples from the step to its handoff, want 1", reports)
	}
}

// A report that is empty states nothing, as an absent one: the end that carries one is taken
// with its other fields, the node holds no report and the log no report tuple and no `reports`
// relation, so the handoff a later write names is the run's report.
func TestAnEmptyReportStatesNone(t *testing.T) {
	pool := storetest.Open(t).Pool
	id, report := uuid.NewString(), uuid.NewString()

	put(t, pool, startOf(implementer(id), minute(0)), minute(0))
	end := endOf(implementer(id), ResultPassed, minute(1))
	end.Report = new("")
	if answered := put(t, pool, end, minute(1)); answered.Result != ResultPassed || answered.Report != "" {
		t.Fatalf("the end with an empty report holds result %q report %q, want passed and none", answered.Result, answered.Report)
	}
	var relations int
	if err := pool.QueryRow(context.Background(), `
		select count(*) from workflow_tuples where from_kind = 'step' and from_id = $1 and kind = 'reports'
	`, id).Scan(&relations); err != nil {
		t.Fatalf("count the reports tuples: %v", err)
	}
	if tuples := valueTuples(t, pool, id, AttributeReport); tuples != 0 || relations != 0 {
		t.Errorf("the log holds %d report tuples and %d reports relations after an empty report, want none", tuples, relations)
	}
	late := implementer(id)
	late.Report = new(report)
	if answered := put(t, pool, late, minute(2)); answered.Report != report {
		t.Errorf("the later write's report is held as %q, want %s", answered.Report, report)
	}
}

// An end whose start has not arrived — a start row still backing off in the daemon's outbox —
// makes the node itself, with the moment its start wrote, which the daemon's end writes again
// beside the session and the model it alone names (workflow's endStep). The start that arrives
// after it carries what the daemon's start writes, the step's identity and that one moment
// (workflow's startStep), so it changes nothing: the node keeps the end's moment, session and
// result, and the log gains no tuple.
func TestAnEndBeforeItsStartMakesTheNode(t *testing.T) {
	pool := storetest.Open(t).Pool
	id := uuid.NewString()

	end := endOf(implementer(id), ResultPassed, minute(5))
	end.StartedAt = new(minute(0))
	end.Session = new("s1")
	end.Model = new("anthropic/claude-opus-5")
	put(t, pool, end, minute(5))
	if nodes := countNodes(t, pool, id); nodes != 1 {
		t.Fatalf("the end wrote %d nodes, want the one it makes", nodes)
	}
	before := wholeLog(t, pool)

	put(t, pool, startOf(implementer(id), minute(0)), minute(6))

	if after := wholeLog(t, pool); !reflect.DeepEqual(after, before) {
		t.Errorf("the late start moved the log from %d tuples to %d, want no tuple: the end carried every field it writes", len(before), len(after))
	}
	steps := issueSteps(t, pool, "LEG-1")
	if len(steps) != 1 {
		t.Fatalf("LEG-1 lists %d steps, want the one node both writes wrote: %+v", len(steps), steps)
	}
	step := steps[0]
	if step.StartedAt == nil || !step.StartedAt.Equal(minute(0)) || step.Session != "s1" || step.Model != "anthropic/claude-opus-5" {
		t.Errorf("the step reads started_at %v session %q model %q, want the end's %s, s1 and anthropic/claude-opus-5", step.StartedAt, step.Session, step.Model, minute(0))
	}
	if step.Result != ResultPassed || step.EndedAt == nil || !step.EndedAt.Equal(minute(5)) {
		t.Errorf("the step reads result %q ended %v, want the end's passed at %s", step.Result, step.EndedAt, minute(5))
	}
}

// A second end of one step is taken and changes nothing: the first result and its time stand, and
// the log gains no tuple.
func TestAnEndAfterAnEndKeepsTheFirst(t *testing.T) {
	pool := storetest.Open(t).Pool
	id := uuid.NewString()

	put(t, pool, startOf(implementer(id), minute(0)), minute(0))
	put(t, pool, endOf(implementer(id), ResultStopped, minute(1)), minute(1))
	before := wholeLog(t, pool)
	put(t, pool, endOf(implementer(id), ResultPassed, minute(2)), minute(2))

	step := issueSteps(t, pool, "LEG-1")[0]
	if step.Result != ResultStopped || step.EndedAt == nil || !step.EndedAt.Equal(minute(1)) {
		t.Errorf("the step reads result %q ended %v, want the first end's stopped at %s", step.Result, step.EndedAt, minute(1))
	}
	if after := wholeLog(t, pool); !reflect.DeepEqual(after, before) {
		t.Errorf("the second end moved the log from %d tuples to %d, want it unchanged", len(before), len(after))
	}
}

// The result of a run and the step it returned to are one fact, the outcome one end reported, so
// only the write that sets the result sets its destination: a node that holds a result keeps the
// destination its own end named, none included, in the write's answer, in the issue's read and in
// the log. Which of the start and the end came first does not matter.
func TestARunKeepsTheDestinationOfTheEndThatSetItsResult(t *testing.T) {
	pool := storetest.Open(t).Pool
	ended := func(id, result string, at time.Time, returned *int) StepWrite {
		end := endOf(implementer(id), result, at)
		end.ReturnedTo = returned
		return end
	}
	for name, row := range map[string]struct {
		writes   func(id string) []StepWrite
		result   string
		returned *int
	}{
		"a later end does not add a destination": {
			writes: func(id string) []StepWrite {
				return []StepWrite{
					startOf(implementer(id), minute(0)),
					ended(id, ResultStopped, minute(1), nil),
					ended(id, ResultFailed, minute(2), new(2)),
				}
			},
			result: ResultStopped,
		},
		"an end before its start": {
			writes: func(id string) []StepWrite {
				return []StepWrite{
					ended(id, ResultFailed, minute(1), new(2)),
					startOf(implementer(id), minute(0)),
				}
			},
			result: ResultFailed, returned: new(2),
		},
		"an end repeated, then another destination": {
			writes: func(id string) []StepWrite {
				return []StepWrite{
					startOf(implementer(id), minute(0)),
					ended(id, ResultFailed, minute(1), new(2)),
					ended(id, ResultFailed, minute(1), new(2)),
					ended(id, ResultFailed, minute(2), new(3)),
					ended(id, ResultStopped, minute(3), new(0)),
				}
			},
			result: ResultFailed, returned: new(2),
		},
	} {
		t.Run(name, func(t *testing.T) {
			id := uuid.NewString()
			var answered Step
			for index, write := range row.writes(id) {
				answered = put(t, pool, write, minute(index))
			}
			read := stepOf(t, pool, "LEG-1", id).Step
			for what, step := range map[string]Step{"the last write's answer": answered, "the read": read} {
				if step.Result != row.result || !reflect.DeepEqual(step.ReturnedTo, row.returned) {
					t.Errorf("%s reads result %q returned_to %s, want %q returned_to %s",
						what, step.Result, destination(step.ReturnedTo), row.result, destination(row.returned))
				}
			}
			want := 0
			if row.returned != nil {
				want = 1
			}
			for field, count := range map[string]int{AttributeResult: 1, AttributeReturnedTo: want} {
				if got := valueTuples(t, pool, id, field); got != count {
					t.Errorf("the log holds %d %s tuples of the run, want %d", got, field, count)
				}
			}
		})
	}
}

// A write that names another value for a field the node holds leaves the held value, whichever of
// the start and the end came first, and appends no tuple for it: the first value of each field
// stands, in the write's answer and in the issue's read.
func TestAHeldFieldKeepsItsFirstValue(t *testing.T) {
	pool := storetest.Open(t).Pool
	first := func(id string) StepWrite {
		write := implementer(id)
		write.Session, write.Model = new("ses-first"), new("anthropic/claude-opus-5")
		return write
	}
	for name, row := range map[string]struct {
		other func(*StepWrite)
		held  func(Step) any
		want  any
	}{
		AttributeAgent:      {func(write *StepWrite) { write.Agent = new("legion-leg-leg-1-other") }, func(step Step) any { return step.Agent }, "legion-leg-leg-1-implementer"},
		AttributeRole:       {func(write *StepWrite) { write.Role = new("tester") }, func(step Step) any { return step.Role }, "implementer"},
		AttributeType:       {func(write *StepWrite) { write.Type = new("testing") }, func(step Step) any { return step.Type }, "implementation"},
		AttributeTask:       {func(write *StepWrite) { write.Task = new("docs") }, func(step Step) any { return step.Task }, "default"},
		AttributeStep:       {func(write *StepWrite) { write.Place = new(3) }, func(step Step) any { return step.Place }, 2},
		AttributeGeneration: {func(write *StepWrite) { write.Generation = new(int64(2)) }, func(step Step) any { return step.Generation }, int64(1)},
		AttributeSession:    {func(write *StepWrite) { write.Session = new("ses-second") }, func(step Step) any { return step.Session }, "ses-first"},
		AttributeModel:      {func(write *StepWrite) { write.Model = new("openai/gpt-5") }, func(step Step) any { return step.Model }, "anthropic/claude-opus-5"},
	} {
		for order, writes := range map[string]func(id string) (StepWrite, StepWrite){
			"start first": func(id string) (StepWrite, StepWrite) {
				second := endOf(first(id), ResultPassed, minute(1))
				row.other(&second)
				return startOf(first(id), minute(0)), second
			},
			"end first": func(id string) (StepWrite, StepWrite) {
				second := startOf(first(id), minute(0))
				row.other(&second)
				return endOf(first(id), ResultPassed, minute(1)), second
			},
		} {
			t.Run(name+"/"+order, func(t *testing.T) {
				id := uuid.NewString()
				written, again := writes(id)
				put(t, pool, written, minute(0))
				answered := put(t, pool, again, minute(1))
				read := stepOf(t, pool, "LEG-1", id).Step
				for what, step := range map[string]Step{"the second write's answer": answered, "the read": read} {
					if got := row.held(step); got != row.want {
						t.Errorf("%s holds %s %v, want the first write's %v", what, name, got, row.want)
					}
				}
				if count := valueTuples(t, pool, id, name); count != 1 {
					t.Errorf("the log holds %d %s tuples of the run, want the first write's one", count, name)
				}
			})
		}
	}
}

// A write that does not name a field the node holds leaves it: the report the daemon writes to a
// run that already ended names its identity and the report alone, and the run keeps the session
// and the model its end wrote, in the write's answer, in the issue's read and in the log. An empty
// session states nothing, as an absent one.
func TestALateReportKeepsTheEndedRunsSession(t *testing.T) {
	pool := storetest.Open(t).Pool
	for name, session := range map[string]*string{"no session": nil, "an empty session": new("")} {
		t.Run(name, func(t *testing.T) {
			id, report := uuid.NewString(), uuid.NewString()
			put(t, pool, startOf(implementer(id), minute(0)), minute(0))
			end := endOf(implementer(id), ResultPassed, minute(1))
			end.Session, end.Model = new("ses-first"), new("anthropic/claude-opus-5")
			if ended := put(t, pool, end, minute(1)); ended.Session != "ses-first" || ended.Model != "anthropic/claude-opus-5" {
				t.Fatalf("the end answers session %q model %q, want the pair it wrote", ended.Session, ended.Model)
			}

			late := implementer(id)
			late.Report, late.Session = new(report), session
			answered := put(t, pool, late, minute(2))
			read := stepOf(t, pool, "LEG-1", id).Step
			for what, step := range map[string]Step{"the report's answer": answered, "the read": read} {
				if step.Session != "ses-first" || step.Model != "anthropic/claude-opus-5" || step.Report != report {
					t.Errorf("%s holds session %q model %q report %q, want the end's ses-first and anthropic/claude-opus-5 beside report %s",
						what, step.Session, step.Model, step.Report, report)
				}
			}
			for _, field := range []string{AttributeSession, AttributeModel, AttributeReport} {
				if count := valueTuples(t, pool, id, field); count != 1 {
					t.Errorf("the log holds %d %s tuples of the run, want one", count, field)
				}
			}
		})
	}
}

// A write that makes a node names the step's whole identity: its task, kind, place, role and
// generation. One that leaves any of them out, or names a task, a kind or a role that is empty,
// which states nothing, is refused ErrStepInput and writes nothing; the same write with all of
// them makes the node, which is the control.
func TestAWriteThatMakesANodeNamesItsIdentity(t *testing.T) {
	pool := storetest.Open(t).Pool
	for name, missing := range map[string]struct {
		field  string
		change func(*StepWrite)
	}{
		"no kind":       {"kind", func(write *StepWrite) { write.Kind = nil }},
		"no task":       {"task", func(write *StepWrite) { write.Task = nil }},
		"no step":       {"step", func(write *StepWrite) { write.Place = nil }},
		"no role":       {"role", func(write *StepWrite) { write.Role = nil }},
		"no generation": {"generation", func(write *StepWrite) { write.Generation = nil }},
		"an empty kind": {"kind", func(write *StepWrite) { write.Kind = new("") }},
		"an empty task": {"task", func(write *StepWrite) { write.Task = new("") }},
		"an empty role": {"role", func(write *StepWrite) { write.Role = new("") }},
	} {
		t.Run(name, func(t *testing.T) {
			id := uuid.NewString()
			write := startOf(implementer(id), minute(0))
			missing.change(&write)
			_, err := tryPut(pool, write, minute(0))
			if !errors.Is(err, ErrStepInput) {
				t.Fatalf("a first write with %s answered %v, want %v", name, err, ErrStepInput)
			}
			if !strings.Contains(err.Error(), "does not name its "+missing.field+":") {
				t.Errorf("the refusal %q does not name the missing %s alone", err, missing.field)
			}
			if nodes := countNodes(t, pool, id); nodes != 0 {
				t.Errorf("the refused write left %d nodes, want none", nodes)
			}
		})
	}

	id := uuid.NewString()
	put(t, pool, startOf(implementer(id), minute(0)), minute(0))
	if nodes := countNodes(t, pool, id); nodes != 1 {
		t.Errorf("the write that names the whole identity left %d nodes, want the one it makes", nodes)
	}
}

// An id is one step of one issue: a write of that id naming another issue is refused
// ErrStepOtherIssue and moves neither the node nor the log.
func TestAnIDOfAnotherIssueIsRefused(t *testing.T) {
	pool := storetest.Open(t).Pool
	id := uuid.NewString()
	put(t, pool, startOf(implementer(id), minute(0)), minute(0))
	before := wholeLog(t, pool)

	elsewhere := endOf(implementer(id), ResultPassed, minute(1))
	elsewhere.Issue = "LEG-2"
	_, err := tryPut(pool, elsewhere, minute(1))
	if !errors.Is(err, ErrStepOtherIssue) {
		t.Fatalf("a write of LEG-1's step naming LEG-2 answered %v, want %v", err, ErrStepOtherIssue)
	}
	if after := wholeLog(t, pool); !reflect.DeepEqual(after, before) {
		t.Errorf("the refused write moved the log from %d tuples to %d", len(before), len(after))
	}
	if step := issueSteps(t, pool, "LEG-1")[0]; step.Result != "" {
		t.Errorf("the refused write ended LEG-1's step %q", step.Result)
	}
}

// The start and the end of one new step can reach Dispatch at one time. Each runs in its own
// transaction, released together: in every round the one node holds what both wrote, and the
// issue lists it once.
func TestTwoWritesOfOneIDAtOneTimeKeepBoth(t *testing.T) {
	pool := storetest.Open(t).Pool
	ctx := context.Background()
	const rounds = 20
	for round := range rounds {
		id := uuid.NewString()
		start := startOf(implementer(id), minute(round))
		end := endOf(implementer(id), ResultPassed, minute(round+1))
		end.Agent = nil
		end.Model = new("second/second-model")

		release := make(chan struct{})
		var begun, done sync.WaitGroup
		errs := make([]error, 2)
		for index, write := range []StepWrite{start, end} {
			begun.Add(1)
			done.Go(func() {
				tx, err := pool.Begin(ctx)
				begun.Done()
				if err != nil {
					errs[index] = err
					return
				}
				defer tx.Rollback(ctx)
				<-release
				if _, err := PutStep(ctx, tx, write, daemon, minute(round)); err != nil {
					errs[index] = err
					return
				}
				errs[index] = tx.Commit(ctx)
			})
		}
		begun.Wait()
		close(release)
		done.Wait()
		for index, err := range errs {
			if err != nil {
				t.Fatalf("round %d: write %d answered %v, want both taken", round, index, err)
			}
		}

		var listed []StepRead
		for _, step := range issueSteps(t, pool, "LEG-1") {
			if step.ID == id {
				listed = append(listed, step)
			}
		}
		if len(listed) != 1 {
			t.Fatalf("round %d: LEG-1 lists the step %d times, want once", round, len(listed))
		}
		step := listed[0]
		if step.StartedAt == nil || !step.StartedAt.Equal(minute(round)) || step.Agent != "legion-leg-leg-1-implementer" {
			t.Errorf("round %d: the node lost the start: started %v agent %q", round, step.StartedAt, step.Agent)
		}
		if step.Result != ResultPassed || step.EndedAt == nil || !step.EndedAt.Equal(minute(round+1)) ||
			step.Model != "second/second-model" {
			t.Errorf("round %d: the node lost the end: result %q ended %v model %q", round, step.Result, step.EndedAt, step.Model)
		}
	}
}

// Two ends of a run that started reach Dispatch at one time, neither naming the run's identity, so
// neither makes the node. The second waits for the first, then reads its end and leaves it: the run
// keeps the first end, and the log one tuple of each of its fields.
func TestTwoEndsOfAStartedRunAtOneTimeKeepTheFirst(t *testing.T) {
	pool := storetest.Open(t).Pool
	ctx := context.Background()
	id := uuid.NewString()
	put(t, pool, startOf(implementer(id), minute(0)), minute(0))
	end := func(result string, at time.Time) StepWrite {
		return StepWrite{ID: id, Issue: "LEG-1", Result: new(result), EndedAt: new(at)}
	}

	first, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the first end: %v", err)
	}
	defer first.Rollback(ctx)
	if _, err := PutStep(ctx, first, end(ResultStopped, minute(1)), daemon, minute(1)); err != nil {
		t.Fatalf("the first end answered %v", err)
	}

	backend := make(chan uint32, 1)
	answered := make(chan error, 1)
	go func() {
		second, err := pool.Begin(ctx)
		if err != nil {
			backend <- 0
			answered <- err
			return
		}
		defer second.Rollback(ctx)
		backend <- second.Conn().PgConn().PID()
		if _, err := PutStep(ctx, second, end(ResultPassed, minute(2)), daemon, minute(2)); err != nil {
			answered <- err
			return
		}
		answered <- second.Commit(ctx)
	}()
	pid := <-backend
	if pid == 0 {
		t.Fatalf("begin the second end: %v", <-answered)
	}
	if err, early := storetest.WaitForQueuedBehind(t, first, pid, answered); early {
		t.Fatalf("the second end answered %v before it waited for the first", err)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit the first end: %v", err)
	}
	if err := <-answered; err != nil {
		t.Fatalf("the second end answered %v, want it taken", err)
	}

	step := issueSteps(t, pool, "LEG-1")[0]
	if step.Result != ResultStopped || step.EndedAt == nil || !step.EndedAt.Equal(minute(1)) {
		t.Errorf("the run reads result %q ended %v, want the first end's stopped at %s", step.Result, step.EndedAt, minute(1))
	}
	for _, field := range []string{AttributeResult, AttributeEndedAt} {
		if count := valueTuples(t, pool, id, field); count != 1 {
			t.Errorf("the log holds %d %s tuples of the run, want the first end's one", count, field)
		}
	}
}

// The attempt is the place of a record among the records of the same task, step and kind,
// ordered by generation, then start (a record with no start first), then id. The retro and the
// merge are both step 0 and count apart, by their kind; two places of one task that a plan gives
// one role count apart, by their place; two tasks whose runs interleave at one place count them
// apart, and so do two issues.
func TestAttemptCountsWithinTaskStepAndKind(t *testing.T) {
	pool := storetest.Open(t).Pool
	ofKind := func(kind string, place int, role string, generation int64) StepWrite {
		write := implementer(uuid.NewString())
		write.Kind, write.Place, write.Role, write.Generation = new(kind), new(place), new(role), new(generation)
		if kind != StepKindStep {
			write.Type = nil
		}
		return write
	}
	inTask := func(task string, write StepWrite) StepWrite {
		write.Task = new(task)
		return write
	}
	written := []StepWrite{
		startOf(ofKind(StepKindStep, 2, "implementer", 1), minute(1)),
		inTask("docs", startOf(ofKind(StepKindStep, 2, "implementer", 1), minute(1).Add(30*time.Second))),
		startOf(ofKind(StepKindStep, 3, "tester", 1), minute(2)),
		// A plan may name one role at two places: each place counts its own runs.
		startOf(ofKind(StepKindStep, 4, "tester", 1), minute(2).Add(30*time.Second)),
		startOf(ofKind(StepKindRetro, 0, "implementer", 1), minute(3)),
		startOf(ofKind(StepKindMerge, 0, "merger", 1), minute(4)),
		// The merge returned the task.
		startOf(ofKind(StepKindStep, 2, "implementer", 1), minute(5)),
		inTask("docs", startOf(ofKind(StepKindStep, 2, "implementer", 1), minute(5).Add(30*time.Second))),
		startOf(ofKind(StepKindStep, 3, "tester", 1), minute(6)),
		startOf(ofKind(StepKindRetro, 0, "implementer", 1), minute(7)),
		startOf(ofKind(StepKindMerge, 0, "merger", 1), minute(8)),
		// A record of the next generation whose start never arrived: it is still the third.
		ofKind(StepKindStep, 2, "implementer", 2),
	}
	// Written last first, so the order the read answers comes from the records and not from the
	// order they were written in.
	for index := len(written) - 1; index >= 0; index-- {
		put(t, pool, written[index], minute(10))
	}
	elsewhere := startOf(implementer(uuid.NewString()), minute(0))
	elsewhere.Issue = "LEG-2"
	put(t, pool, elsewhere, minute(10))
	if other := issueSteps(t, pool, "LEG-2"); len(other) != 1 || other[0].ID != elsewhere.ID || other[0].Attempt != 1 {
		t.Errorf("LEG-2 lists %+v, want its one run, attempt 1", other)
	}

	attempts := map[string][]int{}
	var order []string
	for _, step := range issueSteps(t, pool, "LEG-1") {
		key := step.Task + "/" + step.Kind + "/" + strconv.Itoa(step.Place)
		attempts[key] = append(attempts[key], step.Attempt)
		order = append(order, step.ID)
	}
	want := map[string][]int{
		"default/step/2":  {1, 2, 3},
		"docs/step/2":     {1, 2},
		"default/step/3":  {1, 2},
		"default/step/4":  {1},
		"default/retro/0": {1, 2},
		"default/merge/0": {1, 2},
	}
	if !reflect.DeepEqual(attempts, want) {
		t.Errorf("the attempts read %v, want %v", attempts, want)
	}
	wantOrder := make([]string, len(written))
	for index, write := range written {
		wantOrder[index] = write.ID
	}
	if !slices.Equal(order, wantOrder) {
		t.Errorf("the read answers the records in the order %v, want generation then start: %v", order, wantOrder)
	}

	// The read of many issues (IssuesSteps, which the metrics' fallback timeline reads) answers
	// each issue's runs in the same order, and an issue Legion never ran with none.
	runs, err := IssuesSteps(context.Background(), pool, []string{"LEG-1", "LEG-2", "LEG-3"})
	if err != nil {
		t.Fatalf("read the steps of three issues: %v", err)
	}
	batched := make([]string, 0, len(runs["LEG-1"]))
	for _, step := range runs["LEG-1"] {
		batched = append(batched, step.ID)
	}
	if !slices.Equal(batched, wantOrder) || len(runs["LEG-2"]) != 1 || runs["LEG-2"][0].ID != elsewhere.ID || len(runs["LEG-3"]) != 0 {
		t.Errorf("the read of three issues answers LEG-1 %v, LEG-2 %+v and LEG-3 %+v; want %v, the one run %s, and none",
			batched, runs["LEG-2"], runs["LEG-3"], wantOrder, elsewhere.ID)
	}
}

// Within one generation a run whose start never arrived comes before every run whose start did,
// runs with one start, or with none, follow their ids, and a later generation follows the
// earlier whatever its start and id: the read answers the runs, and counts their attempts, in that
// order. The ids run against the order of the keys before them wherever they can.
func TestTheReadOrdersAnUnknownStartFirstThenTheStartThenTheID(t *testing.T) {
	pool := storetest.Open(t).Pool
	run := func(id string, generation int64, started *time.Time) StepWrite {
		write := implementer(id)
		write.Generation, write.StartedAt = new(generation), started
		return write
	}
	type listed struct {
		id      string
		attempt int
	}
	want := []listed{
		{"aaaaaaaa-0000-4000-8000-000000000000", 1}, // no start
		{"bbbbbbbb-0000-4000-8000-000000000000", 2}, // no start
		{"ffffffff-0000-4000-8000-000000000000", 3}, // minute 1
		{"11111111-0000-4000-8000-000000000000", 4}, // minute 2
		{"cccccccc-0000-4000-8000-000000000000", 5}, // minute 3
		{"dddddddd-0000-4000-8000-000000000000", 6}, // minute 3
		{"00000000-0000-4000-8000-000000000000", 7}, // generation 2, minute 0
	}
	written := []StepWrite{
		run(want[0].id, 1, nil),
		run(want[1].id, 1, nil),
		run(want[2].id, 1, new(minute(1))),
		run(want[3].id, 1, new(minute(2))),
		run(want[4].id, 1, new(minute(3))),
		run(want[5].id, 1, new(minute(3))),
		run(want[6].id, 2, new(minute(0))),
	}
	// Written last first, so the order the read answers comes from the records and not from the
	// order they were written in.
	for index := len(written) - 1; index >= 0; index-- {
		put(t, pool, written[index], minute(10))
	}

	var got []listed
	for _, step := range issueSteps(t, pool, "LEG-1") {
		got = append(got, listed{step.ID, step.Attempt})
	}
	if !slices.Equal(got, want) {
		t.Errorf("the read answers\n%v\nwant\n%v", got, want)
	}
}

// Nothing a write does updates or deletes a tuple: each write appends, above every seq before it,
// and every row an earlier write appended is still there with every column as it was.
func TestEveryWriteAppendsOnlyTuples(t *testing.T) {
	pool := storetest.Open(t).Pool
	id := uuid.NewString()
	late := implementer(id)
	late.Session = new("s1")
	end := endOf(implementer(id), ResultPassed, minute(2))
	end.Model = new("a/1")
	for index, write := range []StepWrite{startOf(implementer(id), minute(0)), late, end} {
		before := wholeLog(t, pool)
		put(t, pool, write, minute(index))
		after := wholeLog(t, pool)
		if len(after) <= len(before) {
			t.Fatalf("write %d left the log at %d tuples, want more than %d", index, len(after), len(before))
		}
		if !slices.Equal(after[:len(before)], before) {
			t.Errorf("write %d rewrote a tuple an earlier write appended", index)
		}
		var last int64
		if len(before) > 0 {
			last = before[len(before)-1].seq
		}
		for _, tuple := range after[len(before):] {
			if tuple.seq <= last {
				t.Errorf("write %d appended seq %d, not above the log's last %d", index, tuple.seq, last)
			}
		}
	}
}

// The two tables are the priority's node and tuple tables under another name: the same columns
// with the same types, nullability and defaults, the same primary keys, and the same three read
// indexes. So the day the graph store lands, the rows copy into it column for column.
func TestTheTablesAreThePriorityShape(t *testing.T) {
	pool := storetest.Open(t).Pool
	for _, pair := range [][2]string{{"priority_nodes", "workflow_nodes"}, {"priority_tuples", "workflow_tuples"}} {
		priority, workflow := tableShape(t, pool, pair[0]), tableShape(t, pool, pair[1])
		if len(priority.columns) == 0 || len(priority.indexes) == 0 {
			t.Fatalf("the schema holds no table %s to compare with", pair[0])
		}
		if !slices.Equal(workflow.columns, priority.columns) {
			t.Errorf("%s carries the columns\n%v\nwant %s's\n%v", pair[1], workflow.columns, pair[0], priority.columns)
		}
		if !slices.Equal(workflow.indexes, priority.indexes) {
			t.Errorf("%s carries the indexes\n%v\nwant %s's\n%v", pair[1], workflow.indexes, pair[0], priority.indexes)
		}
	}
}

// A value outside its form is refused ErrStepInput by the rule of the field the row names, whose
// refusal begins with that field, and writes nothing. A retro or a merge away from place 0 names
// no type, so only the rule of its place can refuse it.
func TestAStepWriteRefusesAValueOutsideItsForm(t *testing.T) {
	pool := storetest.Open(t).Pool
	for name, change := range map[string]func(*StepWrite){
		"kind":               func(write *StepWrite) { write.Kind = new("phase") },
		"result":             func(write *StepWrite) { write.Result, write.EndedAt = new("done"), new(minute(1)) },
		"ended_at":           func(write *StepWrite) { write.EndedAt = new(minute(1)) },
		"result without end": func(write *StepWrite) { write.Result = new(ResultPassed) },
		"type of a retro":    func(write *StepWrite) { write.Kind, write.Place = new(StepKindRetro), new(0) },
		"type of a merge":    func(write *StepWrite) { write.Kind, write.Place = new(StepKindMerge), new(0) },
		"step of a retro":    func(write *StepWrite) { write.Kind, write.Type = new(StepKindRetro), nil },
		"step of a merge":    func(write *StepWrite) { write.Kind, write.Type = new(StepKindMerge), nil },
		"returned_to": func(write *StepWrite) {
			write.ReturnedTo, write.Result, write.EndedAt = new(2), new(ResultPassed), new(minute(1))
		},
		"returned_to below 0": func(write *StepWrite) {
			write.ReturnedTo, write.Result, write.EndedAt = new(-1), new(ResultFailed), new(minute(1))
		},
		"returned_to with no result": func(write *StepWrite) {
			write.ReturnedTo = new(2)
		},
		"report":     func(write *StepWrite) { write.Report = new("not-a-uuid") },
		"id":         func(write *StepWrite) { write.ID = "not-a-uuid" },
		"task":       func(write *StepWrite) { write.Task = new("Default") },
		"role":       func(write *StepWrite) { write.Role = new("Implementer") },
		"type":       func(write *StepWrite) { write.Type = new("Review") },
		"generation": func(write *StepWrite) { write.Generation = new(int64(0)) },
		"issue":      func(write *StepWrite) { write.Issue = "" },
		// The agent, the session and the model are printable ASCII with no space, at most 128
		// characters (contracts.StepAgentTextPattern): `dispatch steps` joins a record's fields with
		// spaces, so a space or a newline in one would shift its columns or forge a line.
		"model":                func(write *StepWrite) { write.Model = new("first/model with spaces") },
		"model with a newline": func(write *StepWrite) { write.Model = new("first/model\ndefault 9 step forged") },
		"model over the bound": func(write *StepWrite) { write.Model = new("first/" + strings.Repeat("m", 123)) },
		"session":              func(write *StepWrite) { write.Session = new("ses\tone") },
		"agent":                func(write *StepWrite) { write.Agent = new("legion agent") },
	} {
		t.Run(name, func(t *testing.T) {
			write := startOf(implementer(uuid.NewString()), minute(0))
			change(&write)
			_, err := tryPut(pool, write, minute(0))
			if !errors.Is(err, ErrStepInput) {
				t.Fatalf("the write answered %v, want %v", err, ErrStepInput)
			}
			field := strings.Fields(name)[0]
			if detail := strings.TrimPrefix(err.Error(), ErrStepInput.Error()+": "); !strings.HasPrefix(detail, field+" ") {
				t.Errorf("the refusal %q is not the rule of %s", err, field)
			}
		})
	}
	if log := wholeLog(t, pool); len(log) != 0 {
		t.Errorf("the refused writes left %d tuples, want none", len(log))
	}
}

// The control of the agent text rows above: an agent, a session and a model inside the form, the
// model at its 128-character bound with the punctuation a model id carries, are taken and read
// back unchanged.
func TestAStepWriteKeepsAgentTextInsideItsForm(t *testing.T) {
	pool := storetest.Open(t).Pool
	write := startOf(implementer(uuid.NewString()), minute(0))
	model := "openrouter/vendor/model-4.1:beta_" + strings.Repeat("x", 128-len("openrouter/vendor/model-4.1:beta_"))
	write.Session, write.Model = new("01a1edce-23dd-726e-b804-626dd5bb3957"), new(model)
	if _, err := tryPut(pool, write, minute(0)); err != nil {
		t.Fatalf("a write whose agent text is inside its form answered %v, want it taken", err)
	}
	steps := issueSteps(t, pool, "LEG-1")
	if len(steps) != 1 {
		t.Fatalf("LEG-1 reads %d steps, want 1", len(steps))
	}
	if got := steps[0]; got.Agent != *write.Agent || got.Session != *write.Session || got.Model != model {
		t.Errorf("the step reads agent %q, session %q, model %q; want %q, %q, %q", got.Agent, got.Session, got.Model, *write.Agent, *write.Session, model)
	}
}

// A retro and a merge have no type, since no plan names them: a write that names one is refused,
// whether it names the kind itself or comes to a node that holds it, and the node keeps no type.
func TestARetroOrAMergeTakesNoType(t *testing.T) {
	pool := storetest.Open(t).Pool
	for _, kind := range []string{StepKindRetro, StepKindMerge} {
		t.Run(kind, func(t *testing.T) {
			id := uuid.NewString()
			start := startOf(implementer(id), minute(0))
			start.Kind, start.Place, start.Type = new(kind), new(0), nil
			put(t, pool, start, minute(0))
			before := wholeLog(t, pool)

			later := StepWrite{ID: id, Issue: "LEG-1", Type: new("review")}
			_, err := tryPut(pool, later, minute(1))
			if !errors.Is(err, ErrStepInput) || !strings.Contains(err.Error(), "type") {
				t.Fatalf("a type written to the %s answered %v, want %v naming the type", kind, err, ErrStepInput)
			}
			if after := wholeLog(t, pool); !reflect.DeepEqual(after, before) {
				t.Errorf("the refused write moved the log from %d tuples to %d", len(before), len(after))
			}
			if step := stepOf(t, pool, "LEG-1", id); step.Type != "" {
				t.Errorf("the %s holds type %q, want none", kind, step.Type)
			}
		})
	}
}

// A run keeps the experiment its issue was drawn into and the variant drawn, as it keeps every
// field, once: a start that names them sets both, an end that names none keeps them, and a later
// write naming another variant changes nothing and appends nothing. The two are stated together:
// a write naming one alone is refused, and so is a name outside the daemon's rule for the names its
// settings declare; each refusal names the field and writes nothing.
func TestAStepKeepsItsVariant(t *testing.T) {
	pool := storetest.Open(t).Pool
	id := uuid.NewString()
	variant := StepExperiment{Experiment: "planning", Variant: "b"}
	named := func(write StepWrite, experiment StepExperiment) StepWrite {
		write.Experiment = &experiment
		return write
	}

	if started := put(t, pool, named(startOf(implementer(id), minute(0)), variant), minute(0)); started.Experiment == nil || *started.Experiment != variant {
		t.Fatalf("the start answers the variant %+v, want %+v", started.Experiment, variant)
	}
	put(t, pool, endOf(implementer(id), ResultPassed, minute(5)), minute(5))
	if step := stepOf(t, pool, "LEG-1", id); step.Experiment == nil || *step.Experiment != variant {
		t.Fatalf("after an end that names none the run holds the variant %+v, want %+v", step.Experiment, variant)
	}
	before := wholeLog(t, pool)
	again := put(t, pool, named(implementer(id), StepExperiment{Experiment: "planning", Variant: "a"}), minute(9))
	if again.Experiment == nil || *again.Experiment != variant {
		t.Fatalf("a second variant answers %+v, want the first, %+v", again.Experiment, variant)
	}
	if after := wholeLog(t, pool); len(after) != len(before) {
		t.Fatalf("a second variant appended %d tuples, want none", len(after)-len(before))
	}
	if steps := issueSteps(t, pool, "LEG-1"); len(steps) != 1 || steps[0].Experiment == nil || *steps[0].Experiment != variant {
		t.Fatalf("the read lists %+v, want the one run with the variant %+v", steps, variant)
	}

	const together = "experiment and variant are stated together or not at all"
	for _, tc := range []struct {
		name       string
		experiment StepExperiment
		refusal    string
	}{
		{"an experiment without its variant", StepExperiment{Experiment: "planning"}, together},
		{"a variant without its experiment", StepExperiment{Variant: "b"}, together},
		{"an experiment outside the name rule", StepExperiment{Experiment: "Bad", Variant: "b"}, `experiment "Bad" must match ^[a-z0-9-]{1,32}$`},
		{"a variant outside the name rule", StepExperiment{Experiment: "planning", Variant: "b:c"}, `variant "b:c" must match ^[a-z0-9-]{1,32}$`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := wholeLog(t, pool)
			_, err := tryPut(pool, named(startOf(implementer(uuid.NewString()), minute(0)), tc.experiment), minute(0))
			if !errors.Is(err, ErrStepInput) || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("the write answered %v, want %v naming %q", err, ErrStepInput, tc.refusal)
			}
			if after := wholeLog(t, pool); len(after) != len(before) {
				t.Fatalf("the refused write appended %d tuples, want none", len(after)-len(before))
			}
		})
	}
}

// put writes one step in a transaction of its own and commits it, as the route does.
func put(t *testing.T, pool *store.Pool, write StepWrite, at time.Time) Step {
	t.Helper()
	written, err := tryPut(pool, write, at)
	if err != nil {
		t.Fatalf("write step %s: %v", write.ID, err)
	}
	return written
}

// committed runs write in a transaction of its own, commits it when the write is taken, and rolls
// it back and answers the refusal when it is not, as the route does.
func committed[T any](pool *store.Pool, write func(ctx context.Context, tx pgx.Tx) (T, error)) (T, error) {
	ctx := context.Background()
	var none T
	tx, err := pool.Begin(ctx)
	if err != nil {
		return none, err
	}
	defer tx.Rollback(ctx)
	written, err := write(ctx, tx)
	if err != nil {
		return none, err
	}
	return written, tx.Commit(ctx)
}

// tryPut runs one step write in a transaction of its own (committed).
func tryPut(pool *store.Pool, write StepWrite, at time.Time) (Step, error) {
	return committed(pool, func(ctx context.Context, tx pgx.Tx) (Step, error) { return PutStep(ctx, tx, write, daemon, at) })
}

func issueSteps(t *testing.T, pool *store.Pool, issue string) []StepRead {
	t.Helper()
	steps, err := IssueSteps(context.Background(), pool, issue)
	if err != nil {
		t.Fatalf("read the steps of %s: %v", issue, err)
	}
	return steps
}

// stepOf is the run id as the read of issue lists it; a read that does not list it fails the test.
func stepOf(t *testing.T, pool *store.Pool, issue, id string) StepRead {
	t.Helper()
	steps := issueSteps(t, pool, issue)
	index := slices.IndexFunc(steps, func(step StepRead) bool { return step.ID == id })
	if index < 0 {
		t.Fatalf("%s does not list the run %s", issue, id)
	}
	return steps[index]
}

func countNodes(t *testing.T, pool *store.Pool, id string) int {
	t.Helper()
	var nodes int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from workflow_nodes where id = $1`, id).Scan(&nodes); err != nil {
		t.Fatalf("count the nodes of %s: %v", id, err)
	}
	return nodes
}

// loggedTuple is a tuple as the log holds it, read straight from the table rather than through
// this package, for the checks that no write rewrote or removed one.
type loggedTuple struct {
	seq                                  int64
	fromKind, fromID, kind, toKind, toID string
	at                                   time.Time
	who                                  string
	sourceEvent                          int64
	asserted                             bool
}

// wholeLog is every tuple row of the database in the order it was appended.
func wholeLog(t *testing.T, pool *store.Pool) []loggedTuple {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		select seq, from_kind, from_id, kind, to_kind, to_id, at, who::text, source_event, asserted
		from workflow_tuples order by seq
	`)
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	defer rows.Close()
	var log []loggedTuple
	for rows.Next() {
		var tuple loggedTuple
		if err := rows.Scan(&tuple.seq, &tuple.fromKind, &tuple.fromID, &tuple.kind, &tuple.toKind,
			&tuple.toID, &tuple.at, &tuple.who, &tuple.sourceEvent, &tuple.asserted); err != nil {
			t.Fatalf("read the log: %v", err)
		}
		log = append(log, tuple)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the log: %v", err)
	}
	return log
}

// shape is one table's columns and indexes as the catalogue holds them, each written with the
// table's own prefix taken out, so two tables of one shape under two names compare equal.
type shape struct {
	columns []string
	indexes []string
}

func tableShape(t *testing.T, pool *store.Pool, table string) shape {
	t.Helper()
	ctx := context.Background()
	prefix, _, _ := strings.Cut(table, "_")
	read := func(query string) []string {
		rows, err := pool.Query(ctx, query, table)
		if err != nil {
			t.Fatalf("read the shape of %s: %v", table, err)
		}
		values, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the shape of %s: %v", table, err)
		}
		for index, value := range values {
			values[index] = strings.ReplaceAll(value, prefix+"_", "<table>_")
		}
		return values
	}
	return shape{
		columns: read(`
			select column_name || ' ' || data_type || ' ' || is_nullable || ' ' || coalesce(column_default, '')
			from information_schema.columns
			where table_schema = 'public' and table_name = $1
			order by ordinal_position
		`),
		indexes: read(`
			select indexdef from pg_indexes
			where schemaname = 'public' and tablename = $1
			order by indexname
		`),
	}
}

// valueTuples counts the value tuples the log holds of one attribute of one step.
func valueTuples(t *testing.T, pool *store.Pool, id, attribute string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `
		select count(*) from workflow_tuples
		where from_kind = 'step' and from_id = $1 and kind = $2 and to_kind = 'value'
	`, id, attribute).Scan(&count); err != nil {
		t.Fatalf("count the %s tuples of step %s: %v", attribute, id, err)
	}
	return count
}

// destination is a run's returned_to as a failure message prints it.
func destination(returned *int) string {
	if returned == nil {
		return "none"
	}
	return strconv.Itoa(*returned)
}
