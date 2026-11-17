package workflowstore

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
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
		if step.Result != ResultFailed || step.EndedAt == nil || !step.EndedAt.Equal(minute(1)) || step.ReturnedTo != 2 {
			t.Errorf("%s holds result %q ended %v returned_to %d, want failed at %s to step 2",
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

// An end whose start never arrived — a step that started under the release before, or whose
// start row is still backing off — makes the node itself, with the start time the daemon holds.
// The start that arrives after it keeps that start time, fills what the end did not carry, and
// leaves the result.
func TestAnEndBeforeItsStartMakesTheNode(t *testing.T) {
	pool := storetest.Open(t).Pool
	id := uuid.NewString()

	end := endOf(implementer(id), ResultPassed, minute(5))
	end.StartedAt = new(minute(0))
	put(t, pool, end, minute(5))
	if nodes := countNodes(t, pool, id); nodes != 1 {
		t.Fatalf("the end wrote %d nodes, want the one it makes", nodes)
	}

	late := startOf(implementer(id), minute(1))
	late.Session = new("s1")
	put(t, pool, late, minute(6))

	steps := issueSteps(t, pool, "LEG-1")
	if len(steps) != 1 {
		t.Fatalf("LEG-1 lists %d steps, want the one node both writes wrote: %+v", len(steps), steps)
	}
	step := steps[0]
	if step.StartedAt == nil || !step.StartedAt.Equal(minute(0)) {
		t.Errorf("started_at reads %v, want the end's %s: the late start does not move it", step.StartedAt, minute(0))
	}
	if step.Session != "s1" {
		t.Errorf("session reads %q, want the late start's s1", step.Session)
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

// A write that makes a node names the step's whole identity: its task, kind, place, role and
// generation. One that leaves any of them out is refused ErrStepInput and writes nothing; the
// same write with all of them makes the node, which is the control.
func TestAWriteThatMakesANodeNamesItsIdentity(t *testing.T) {
	pool := storetest.Open(t).Pool
	for name, without := range map[string]func(*StepWrite){
		"kind":       func(write *StepWrite) { write.Kind = nil },
		"task":       func(write *StepWrite) { write.Task = nil },
		"step":       func(write *StepWrite) { write.Place = nil },
		"role":       func(write *StepWrite) { write.Role = nil },
		"generation": func(write *StepWrite) { write.Generation = nil },
	} {
		t.Run("no "+name, func(t *testing.T) {
			id := uuid.NewString()
			write := startOf(implementer(id), minute(0))
			without(&write)
			err := tryWrite(pool, func(tx pgx.Tx) error {
				_, err := PutStep(context.Background(), tx, write, daemon, minute(0))
				return err
			})
			if !errors.Is(err, ErrStepInput) {
				t.Fatalf("a first write with no %s answered %v, want %v", name, err, ErrStepInput)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("the refusal %q does not name the missing %s", err, name)
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
	err := tryWrite(pool, func(tx pgx.Tx) error {
		_, err := PutStep(context.Background(), tx, elsewhere, daemon, minute(1))
		return err
	})
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
			done.Add(1)
			go func() {
				defer done.Done()
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
			}()
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

// The attempt is the place of a record among the records of the same task, step and kind,
// ordered by generation, then start (a record with no start first), then id. The retro and the
// merge are both step 0 and count apart, by their kind.
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
	written := []StepWrite{
		startOf(ofKind(StepKindStep, 2, "implementer", 1), minute(1)),
		startOf(ofKind(StepKindStep, 3, "tester", 1), minute(2)),
		startOf(ofKind(StepKindRetro, 0, "implementer", 1), minute(3)),
		startOf(ofKind(StepKindMerge, 0, "merger", 1), minute(4)),
		// The merge returned the task.
		startOf(ofKind(StepKindStep, 2, "implementer", 1), minute(5)),
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

	attempts := map[string][]int{}
	var order []string
	for _, step := range issueSteps(t, pool, "LEG-1") {
		key := step.Kind + "/" + step.Role
		attempts[key] = append(attempts[key], step.Attempt)
		order = append(order, step.ID)
	}
	want := map[string][]int{
		"step/implementer":  {1, 2, 3},
		"step/tester":       {1, 2},
		"retro/implementer": {1, 2},
		"merge/merger":      {1, 2},
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

// A value outside its form is refused ErrStepInput, naming what is wrong, and writes nothing.
func TestAStepWriteRefusesAValueOutsideItsForm(t *testing.T) {
	pool := storetest.Open(t).Pool
	for name, change := range map[string]func(*StepWrite){
		"kind":               func(write *StepWrite) { write.Kind = new("phase") },
		"result":             func(write *StepWrite) { write.Result, write.EndedAt = new("done"), new(minute(1)) },
		"ended_at":           func(write *StepWrite) { write.EndedAt = new(minute(1)) },
		"result without end": func(write *StepWrite) { write.Result = new(ResultPassed) },
		"returned_to": func(write *StepWrite) {
			write.ReturnedTo, write.Result, write.EndedAt = new(2), new(ResultPassed), new(minute(1))
		},
		"report":     func(write *StepWrite) { write.Report = new("not-a-uuid") },
		"id":         func(write *StepWrite) { write.ID = "not-a-uuid" },
		"task":       func(write *StepWrite) { write.Task = new("Default") },
		"role":       func(write *StepWrite) { write.Role = new("Implementer") },
		"type":       func(write *StepWrite) { write.Type = new("Review") },
		"step":       func(write *StepWrite) { write.Kind = new(StepKindRetro) },
		"generation": func(write *StepWrite) { write.Generation = new(int64(0)) },
		"issue":      func(write *StepWrite) { write.Issue = "" },
	} {
		t.Run(name, func(t *testing.T) {
			write := startOf(implementer(uuid.NewString()), minute(0))
			change(&write)
			err := tryWrite(pool, func(tx pgx.Tx) error {
				_, err := PutStep(context.Background(), tx, write, daemon, minute(0))
				return err
			})
			if !errors.Is(err, ErrStepInput) {
				t.Fatalf("the write answered %v, want %v", err, ErrStepInput)
			}
			if field := strings.Fields(name)[0]; !strings.Contains(err.Error(), field) {
				t.Errorf("the refusal %q does not name %s", err, field)
			}
		})
	}
	if log := wholeLog(t, pool); len(log) != 0 {
		t.Errorf("the refused writes left %d tuples, want none", len(log))
	}
}

// put writes one step in a transaction of its own and commits it, as the route does.
func put(t *testing.T, pool *store.Pool, write StepWrite, at time.Time) Step {
	t.Helper()
	var written Step
	if err := tryWrite(pool, func(tx pgx.Tx) error {
		var err error
		written, err = PutStep(context.Background(), tx, write, daemon, at)
		return err
	}); err != nil {
		t.Fatalf("write step %s: %v", write.ID, err)
	}
	return written
}

// tryWrite runs one write in a transaction of its own, commits it when the write is taken, and
// rolls it back and answers the refusal when it is not.
func tryWrite(pool *store.Pool, write func(pgx.Tx) error) error {
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := write(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func issueSteps(t *testing.T, pool *store.Pool, issue string) []StepRead {
	t.Helper()
	steps, err := IssueSteps(context.Background(), pool, issue)
	if err != nil {
		t.Fatalf("read the steps of %s: %v", issue, err)
	}
	return steps
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
