// Package storesmovetest writes rows of the priority store and the workflow store in the shapes
// the release before dispatch://INFRA-1098's R32 wrote them: priority_nodes and priority_tuples as
// main's prioritystore wrote them, and workflow_nodes and workflow_tuples as main's workflowstore
// wrote them (putNode, appendTuples), statement for statement. This build writes neither table, so
// a test of the move at boot (internal/dispatch/storesmove) writes what it moves through here, on a
// database whose four names are still tables. It is an ordinary package rather than a _test.go
// file so the tests of every package that holds a part of the move can import it.
package storesmovetest

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Execer is what a write here goes through: a pool, a connection or a transaction.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// exec runs one statement of a fixture, failing t on its error.
func exec(t testing.TB, db Execer, what, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// encoded is value as JSON, failing t where it cannot be.
func encoded(t testing.TB, value any) []byte {
	t.Helper()
	text, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %v: %v", value, err)
	}
	return text
}

// Issue writes project, where no row of it stands, and the issue key of it, straight into their
// tables: the issue a run, a plan or an advances relation names, whose project the move reads.
func Issue(t testing.TB, db Execer, project, key string, number int) {
	t.Helper()
	exec(t, db, "write project "+project, `
		insert into projects (key, name, default_assignee) values ($1, $1, 'alice') on conflict do nothing
	`, project)
	exec(t, db, "write issue "+key, `
		insert into issues (key, project_key, number, title, status, created_by, rank)
		values ($1, $2, $3, $4, 'done', '{"kind":"user","id":"alice"}', $5)
	`, key, project, number, "Title of "+key, "U"+strconv.Itoa(number))
}

// valueTuples appends one value tuple of a priority for each of values, in the name order of the
// attributes, as main's attributeTuples ordered them.
func priorityValues(id string, values map[string]string) [][2]string {
	var stated [][2]string
	for _, name := range slices.Sorted(maps.Keys(values)) {
		stated = append(stated, [2]string{name, values[name]})
	}
	return stated
}

// appendPriorityTuples appends tuples out of the priority id, each a value tuple, in their order,
// as main's appendTuples did, in one statement.
func appendPriorityTuples(t testing.TB, db Execer, id string, values [][2]string, who model.Actor, at time.Time) {
	t.Helper()
	kinds, texts := make([]string, len(values)), make([]string, len(values))
	for index, value := range values {
		kinds[index], texts[index] = value[0], value[1]
	}
	exec(t, db, "append the tuples of priority "+id, `
		insert into priority_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		select 'priority', $1, stated.kind, 'value', stated.value, $4, $5::jsonb, 0, true
		from unnest($2::text[], $3::text[]) with ordinality as stated(kind, value, place)
		order by stated.place
	`, id, kinds, texts, at, encoded(t, who))
}

// CreatePriority writes a priority as main's prioritystore.Create wrote it: its node row, live and
// visible to the project, then its visibility, its state and each of values as value tuples.
func CreatePriority(t testing.TB, db Execer, id string, values map[string]string, who model.Actor, at time.Time) {
	t.Helper()
	exec(t, db, "write priority "+id, `
		insert into priority_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		values ('priority', $1, 'priority', 'project', 'live', $2, $3, $4, 0)
	`, id, encoded(t, values), at, encoded(t, who))
	appendPriorityTuples(t, db, id, append([][2]string{{"visibility", "project"}, {"state", "live"}}, priorityValues(id, values)...), who, at)
}

// SetPriorityAttributes moves a priority's attributes as main's prioritystore.SetAttributes did,
// a rename and a rank move among them: the row's attributes laid over, then a value tuple each.
func SetPriorityAttributes(t testing.TB, db Execer, id string, values map[string]string, who model.Actor, at time.Time) {
	t.Helper()
	exec(t, db, "move the attributes of priority "+id, `
		update priority_nodes set attributes = priority_nodes.attributes || $2::jsonb where kind = 'priority' and id = $1
	`, id, encoded(t, values))
	appendPriorityTuples(t, db, id, priorityValues(id, values), who, at)
}

// SetPriorityState moves a priority's state as main's prioritystore.SetState did, a delete among
// them: the row's state, then its state tuple.
func SetPriorityState(t testing.TB, db Execer, id, state string, who model.Actor, at time.Time) {
	t.Helper()
	exec(t, db, "move the state of priority "+id, `
		update priority_nodes set state = $2 where kind = 'priority' and id = $1
	`, id, state)
	appendPriorityTuples(t, db, id, [][2]string{{"state", state}}, who, at)
}

// Advance states, or with asserted false retracts, that issue advances the priority id, as main's
// prioritystore.Advance and Retract did: one tuple out of the issue.
func Advance(t testing.TB, db Execer, issue, id string, asserted bool, who model.Actor, at time.Time) {
	t.Helper()
	exec(t, db, "state that "+issue+" advances "+id, `
		insert into priority_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		values ('issue', $1, 'advances', 'priority', $2, $3, $4::jsonb, 0, $5)
	`, issue, id, at, encoded(t, who), asserted)
}

// Attribute is one field a write of a run or a plan states, as main's workflowstore holds one:
// its name, the value the node row holds, and the text its value tuple holds.
type Attribute struct {
	Name  string
	Value any
	Text  string
}

// Text is an attribute whose row value and tuple text are the one string.
func Text(name, value string) Attribute { return Attribute{Name: name, Value: value, Text: value} }

// Relation is one relation a write of a run states beside its attributes, a run's reports among
// them.
type Relation struct{ Kind, ToKind, ToID string }

// PutNode writes one run (kind step) or plan (kind plan) as main's workflowstore putNode wrote
// it: where no row of the node stands, it inserts the row, live, visible to the project, with no
// attribute; it then sets each of attributes the row does not hold yet, and appends, in one
// statement, a value tuple for each attribute it set, then the part_of to issue where it made the
// node, then each of relations. It writes nothing where the row holds every attribute.
func PutNode(t testing.TB, db Execer, kind, id, issue string, attributes []Attribute, relations []Relation, who model.Actor, at time.Time) {
	t.Helper()
	ctx := context.Background()
	writer := encoded(t, who)
	tag, err := db.Exec(ctx, `
		insert into workflow_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		values ($1, $2, $1, 'project', 'live', '{}', $3, $4, 0)
		on conflict (kind, id) do nothing
	`, kind, id, at, writer)
	if err != nil {
		t.Fatalf("write %s %s: %v", kind, id, err)
	}
	made := tag.RowsAffected() == 1
	var heldText []byte
	if err := db.QueryRow(ctx, `select attributes from workflow_nodes where kind = $1 and id = $2 for update`, kind, id).Scan(&heldText); err != nil {
		t.Fatalf("read %s %s: %v", kind, id, err)
	}
	var held map[string]json.RawMessage
	if err := json.Unmarshal(heldText, &held); err != nil {
		t.Fatalf("decode %s %s: %v", kind, id, err)
	}
	setting := map[string]any{}
	var kinds, toKinds, toIDs []string
	for _, attribute := range attributes {
		if _, holds := held[attribute.Name]; holds {
			continue
		}
		setting[attribute.Name] = attribute.Value
		kinds, toKinds, toIDs = append(kinds, attribute.Name), append(toKinds, "value"), append(toIDs, attribute.Text)
	}
	if made {
		kinds, toKinds, toIDs = append(kinds, "part_of"), append(toKinds, "issue"), append(toIDs, issue)
	}
	for _, relation := range relations {
		kinds, toKinds, toIDs = append(kinds, relation.Kind), append(toKinds, relation.ToKind), append(toIDs, relation.ToID)
	}
	if len(setting) == 0 {
		return
	}
	exec(t, db, "write the attributes of "+kind+" "+id, `
		update workflow_nodes set attributes = attributes || $3::jsonb where kind = $1 and id = $2
	`, kind, id, encoded(t, setting))
	exec(t, db, "append the tuples of "+kind+" "+id, `
		insert into workflow_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		select $7, $8, stated.kind, stated.to_kind, stated.to_id, $4, $5::jsonb, $6::bigint, true
		from unnest($1::text[], $2::text[], $3::text[]) with ordinality as stated(kind, to_kind, to_id, place)
		order by stated.place
	`, kinds, toKinds, toIDs, at, writer, 0, kind, id)
}

// Fixtures is what Write wrote: the issues, the priorities and the runs and plans, by id.
type Fixtures struct {
	Project           string
	Issues            []string
	Priorities        []string
	Deleted           string
	Runs, Plans       []string
	RunWithNoProject  string
	PlanWithNoProject string
	Handoff           string
	Who               model.Actor
	At                time.Time
}

// Write writes a corpus of both stores as the release before's routes write one: three priorities
// of two attributes or more, a rank move of one and the delete of another, the advances relations
// of two issues, one of them retracted, two runs of the steps of one issue, one of them with a
// report that names a handoff, the plan that issue ran from, with its tasks a JSON list, the
// retro's run rated, and one run and one plan whose rows hold no project attribute, as a daemon
// that named no project wrote them.
func Write(t testing.TB, db Execer) Fixtures {
	t.Helper()
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	minute := func(n int) time.Time { return at.Add(time.Duration(n) * time.Minute) }
	f := Fixtures{
		Project: "MOVE", Issues: []string{"MOVE-1", "MOVE-2", "MOVE-3"},
		Priorities: []string{"ship-the-thing", "keep-it-up", "drop-this"}, Deleted: "drop-this",
		Handoff: "handoff:0f1e2d3c-0000-4000-8000-000000000001",
		Who:     model.Actor{Kind: "user", ID: "alice@example.com", Proven: true},
		At:      at,
	}
	for index, key := range f.Issues {
		Issue(t, db, f.Project, key, index+1)
	}
	daemon := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	CreatePriority(t, db, "ship-the-thing", map[string]string{"title": "Ship the thing", "description": "All of it", "owner": "alice", "rank": "a0"}, f.Who, minute(0))
	CreatePriority(t, db, "keep-it-up", map[string]string{"title": "Keep it up", "rank": "a1"}, f.Who, minute(1))
	CreatePriority(t, db, "drop-this", map[string]string{"title": "Drop this", "rank": "a2"}, f.Who, minute(2))
	SetPriorityAttributes(t, db, "keep-it-up", map[string]string{"rank": "Zz"}, f.Who, minute(3))
	Advance(t, db, "MOVE-1", "ship-the-thing", true, f.Who, minute(4))
	Advance(t, db, "MOVE-2", "keep-it-up", true, f.Who, minute(5))
	Advance(t, db, "MOVE-2", "ship-the-thing", true, f.Who, minute(6))
	Advance(t, db, "MOVE-2", "ship-the-thing", false, f.Who, minute(7))
	SetPriorityState(t, db, "drop-this", "gone", f.Who, minute(8))

	plan := "0f1e2d3c-0000-4000-8000-0000000000a1"
	tasks := json.RawMessage(`[{"id":"default","steps":[{"role":"planner"},{"role":"implementer"}]}]`)
	PutNode(t, db, "plan", plan, "MOVE-1", []Attribute{
		Text("issue", "MOVE-1"), Text("project", f.Project),
		{Name: "generation", Value: 1, Text: "1"}, Text("source", "default"),
		Text("artifact_id", ""), {Name: "version", Value: 0, Text: "0"},
		{Name: "tasks", Value: tasks, Text: string(tasks)},
	}, nil, daemon, minute(10))
	planner := "0f1e2d3c-0000-4000-8000-0000000000b1"
	identity := func(role string, place int) []Attribute {
		return []Attribute{
			Text("issue", "MOVE-1"), Text("project", f.Project), {Name: "generation", Value: 1, Text: "1"},
			Text("task", "default"), {Name: "step", Value: place, Text: strconv.Itoa(place)}, Text("kind", "step"), Text("role", role),
		}
	}
	PutNode(t, db, "step", planner, "MOVE-1", append(identity("planner", 1),
		Text("started_at", minute(11).Format(time.RFC3339Nano))), nil, daemon, minute(11))
	PutNode(t, db, "step", planner, "MOVE-1", []Attribute{
		Text("ended_at", minute(20).Format(time.RFC3339Nano)), Text("result", "passed"), Text("report", f.Handoff),
	}, []Relation{{Kind: "reports", ToKind: "handoff", ToID: f.Handoff}}, daemon, minute(20))
	implementer := "0f1e2d3c-0000-4000-8000-0000000000b2"
	PutNode(t, db, "step", implementer, "MOVE-1", append(identity("implementer", 2),
		Text("started_at", minute(21).Format(time.RFC3339Nano)), Text("model", "a-model")), nil, daemon, minute(21))
	retro := "0f1e2d3c-0000-4000-8000-0000000000b3"
	PutNode(t, db, "step", retro, "MOVE-1", []Attribute{
		Text("issue", "MOVE-1"), Text("project", f.Project), {Name: "generation", Value: 1, Text: "1"},
		Text("task", "default"), {Name: "step", Value: 0, Text: "0"}, Text("kind", "retro"), Text("role", "implementer"),
		Text("ended_at", minute(30).Format(time.RFC3339Nano)), Text("result", "passed"),
	}, nil, daemon, minute(30))
	PutNode(t, db, "step", retro, "MOVE-1", []Attribute{
		{Name: "rating", Value: 4, Text: "4"}, Text("rating_reason", "It held."),
	}, nil, f.Who, minute(31))
	f.Runs, f.Plans = []string{planner, implementer, retro}, []string{plan}

	f.RunWithNoProject = "0f1e2d3c-0000-4000-8000-0000000000c1"
	PutNode(t, db, "step", f.RunWithNoProject, "MOVE-3", []Attribute{
		Text("issue", "MOVE-3"), {Name: "generation", Value: 1, Text: "1"}, Text("task", "default"),
		{Name: "step", Value: 1, Text: "1"}, Text("kind", "step"), Text("role", "planner"),
	}, nil, daemon, minute(40))
	f.PlanWithNoProject = "0f1e2d3c-0000-4000-8000-0000000000c2"
	PutNode(t, db, "plan", f.PlanWithNoProject, "MOVE-3", []Attribute{
		Text("issue", "MOVE-3"), {Name: "generation", Value: 1, Text: "1"}, Text("source", "default"),
		{Name: "tasks", Value: tasks, Text: string(tasks)},
	}, nil, daemon, minute(41))
	f.Runs, f.Plans = append(f.Runs, f.RunWithNoProject), append(f.Plans, f.PlanWithNoProject)
	return f
}

// KindsStatistics is what pg_stats_ext holds for graph_tuples_kinds
// (20261126000700_graph_tuples_kinds_statistics): whether the object stands, whether an ANALYZE has
// populated its MCV list, and whether that list holds (issue, advances, priority) and at what
// frequency.
type KindsStatistics struct {
	Stands       bool
	Analyzed     bool
	HasAdvances  bool
	AdvancesFreq float64
}

// String describes the reading in one phrase, for test logs.
func (k KindsStatistics) String() string {
	switch {
	case !k.Stands:
		return "graph_tuples_kinds does not stand"
	case !k.Analyzed:
		return "graph_tuples_kinds stands, most_common_vals is null"
	case !k.HasAdvances:
		return "graph_tuples_kinds stands, most_common_vals is populated without (issue, advances, priority)"
	default:
		return fmt.Sprintf("graph_tuples_kinds holds (issue, advances, priority) in most_common_vals at frequency %.6f", k.AdvancesFreq)
	}
}

// ReadKindsStatistics reads graph_tuples_kinds from pg_statistic_ext and pg_stats_ext in the
// current schema.
func ReadKindsStatistics(t testing.TB, db Execer) KindsStatistics {
	t.Helper()
	var got KindsStatistics
	var freq *float64
	if err := db.QueryRow(context.Background(), `
		select
			exists (select from pg_statistic_ext where stxname = 'graph_tuples_kinds' and stxnamespace = current_schema()::regnamespace),
			exists (select from pg_stats_ext where statistics_schemaname = current_schema() and statistics_name = 'graph_tuples_kinds' and most_common_vals is not null),
			(select s.most_common_freqs[i]
			 from pg_stats_ext s, generate_subscripts(s.most_common_vals, 1) as i
			 where s.statistics_schemaname = current_schema() and s.statistics_name = 'graph_tuples_kinds'
			   and s.most_common_vals[i][array_position(s.attnames, 'from_kind')] = 'issue'
			   and s.most_common_vals[i][array_position(s.attnames, 'kind')] = 'advances'
			   and s.most_common_vals[i][array_position(s.attnames, 'to_kind')] = 'priority'
			 limit 1)
	`).Scan(&got.Stands, &got.Analyzed, &freq); err != nil {
		t.Fatalf("read graph_tuples_kinds statistics: %v", err)
	}
	if freq != nil {
		got.HasAdvances, got.AdvancesFreq = true, *freq
	}
	return got
}
