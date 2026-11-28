package record

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// wantEveryColumnCarried fails t unless every row of table at LEGION-208's generation 1 has a copy
// at generation 2 over every column of table but the generation and except. The columns are read
// from the table itself (information_schema), not from a list the test keeps, so a column a later
// migration adds is compared whatever its name, and the census cannot go stale. A copy that leaves
// a column with a default out writes that default, so each such column must hold another value in
// some seeded row, or the comparison could not see it left out: a fixture that does not seed a
// column added later fails here too, naming it. The control: the same comparison with the
// generation named, which the copy does change, finds every row different.
func wantEveryColumnCarried(t *testing.T, tx pgx.Tx, table string, minimum int, except ...string) {
	t.Helper()
	ctx := context.Background()
	if except == nil {
		except = []string{}
	}
	rows, err := tx.Query(ctx, `select column_name, coalesce(column_default, 'null') from information_schema.columns
		where table_schema = 'public' and table_name = $1 and column_name <> 'generation' and not (column_name = any($2))
		order by column_name`, table, except)
	must(t, err)
	type column struct{ name, fallback string }
	columns, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (column, error) {
		var c column
		err := row.Scan(&c.name, &c.fallback)
		return c, err
	})
	must(t, err)
	if len(columns) < minimum {
		t.Fatalf("%s has the columns %+v beside the generation and %q, want at least %d: the census read nothing", table, columns, except, minimum)
	}
	names := make([]string, len(columns))
	for i, c := range columns {
		names[i] = c.name
		var seeded int
		must(t, tx.QueryRow(ctx, fmt.Sprintf(`select count(*) from %s where issue = 'LEGION-208' and generation = 1 and %s is distinct from %s`,
			table, c.name, c.fallback)).Scan(&seeded))
		if seeded == 0 {
			t.Fatalf("no row of %s at generation 1 holds %s other than its default %s: a copy that left it out would pass; seed it", table, c.name, c.fallback)
		}
	}
	differ := func(list []string) int {
		t.Helper()
		var n int
		must(t, tx.QueryRow(ctx, fmt.Sprintf(`select count(*) from (
			select %[1]s from %[2]s where issue = 'LEGION-208' and generation = 1
			except select %[1]s from %[2]s where issue = 'LEGION-208' and generation = 2) d`, strings.Join(list, ", "), table)).Scan(&n))
		return n
	}
	var held int
	must(t, tx.QueryRow(ctx, fmt.Sprintf(`select count(*) from %s where issue = 'LEGION-208' and generation = 1`, table)).Scan(&held))
	if n := differ(names); n != 0 {
		t.Fatalf("%d rows of %s at generation 1 have no copy at generation 2 over the columns %q: CarryPlan leaves a column of %s behind", n, table, names, table)
	}
	if n := differ(append(names, "generation")); held == 0 || n != held {
		t.Fatalf("with the generation named, %d rows of %s differ, want all %d: the comparison cannot fail", n, table, held)
	}
}

// A carried plan holds every column of plan_steps that a step's start reads: CarryPlan copies each
// step to the new generation with every column but the generation, so a column added to the table
// and left out of the copy fails here, whatever its name (wantEveryColumnCarried). The case that
// found it is leave_session: the default steps' agent that took a key's name is written with the
// key's session to leave, and a plan carried to the next generation before that agent's first
// start must carry it, or the start resumes the key's session.
func TestACarriedPlanHoldsEveryColumnOfItsSteps(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	inTx(t, st, func(tx pgx.Tx) {
		for _, statement := range []string{
			`insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
				values ('LEGION-208', 1, 0, 'default', '', '', now())`,
			`insert into plan_tasks (issue, generation, task, plan_version, block_text, steps_by_default, state)
				values ('LEGION-208', 1, 'default', 0, 'The project''s default steps.', true, 'waiting')`,
			`insert into plan_steps (issue, generation, task, position, role, type, agent_key, agent, leave_session)
				values ('LEGION-208', 1, 'default', 1, 'tester', 'testing', '', 'tester', 'ses-key-k'),
				('LEGION-208', 1, 'default', 2, 'reviewer', 'review', 'k', 'reviewer', '')`,
		} {
			_, err := tx.Exec(ctx, statement)
			must(t, err)
		}
		must(t, NewStore().CarryPlan(ctx, tx, "LEGION-208", 1, 2, false))
		wantEveryColumnCarried(t, tx, "plan_steps", 8)
	})
}

// The rest of a carried plan is the same census over its other two tables (wantEveryColumnCarried):
// CarryPlan copies each version with every column but the generation, and so each task where the
// hand-over carries its run (carried). A plan carried from its start (not carried) starts each task
// again, so it copies every column of plan_tasks but the ones that say where the task stands and
// what it counted, which it writes as a task that waits at no act with every count 0. Those are the
// one list the census leaves out, so a column added to either table is copied or the test fails.
func TestACarriedPlanHoldsEveryColumnOfItsVersionsAndTasks(t *testing.T) {
	// restarted is every column of plan_tasks a plan carried from its start writes anew, with the
	// value each holds then.
	restarted := []struct{ column, value string }{
		{"state", "waiting"}, {"stopped_reason", ""}, {"running_kind", ""}, {"position", "0"}, {"pass", "0"},
		{"rounds", "0"}, {"conflict_rounds", "0"}, {"backward_moves", "{}"}, {"cap_told", "0"},
	}
	for _, tc := range []struct {
		name    string
		carried bool
	}{
		{"a hand-over that carries its run", true},
		{"a plan carried from its start", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := migratedStore(t)
			inTx(t, st, func(tx pgx.Tx) {
				for _, statement := range []string{
					`insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
						values ('LEGION-208', 1, 2, 'owner', 'artifact-plan', 'legion-legion-legion-207-architect', now() - interval '1 hour')`,
					`insert into plan_tasks (issue, generation, task, plan_version, block_text, needs, part_of, owner_role, merges, steps_by_default,
						state, stopped_reason, running_kind, position, pass, rounds, conflict_rounds, backward_moves, cap_told)
						values ('LEGION-208', 1, 'b', 2, 'Build the widget.', '{a}', 'a', 'architect', true, true,
						'stopped', 'the owner stopped it', 'step', 1, 2, 1, 1, '{"reviewing-implementing": 1}', 1)`,
					`insert into plan_steps (issue, generation, task, position, role, type)
						values ('LEGION-208', 1, 'b', 1, 'implementer', 'implementation')`,
				} {
					_, err := tx.Exec(ctx, statement)
					must(t, err)
				}
				must(t, NewStore().CarryPlan(ctx, tx, "LEGION-208", 1, 2, tc.carried))
				wantEveryColumnCarried(t, tx, "issue_plans", 6)
				if tc.carried {
					wantEveryColumnCarried(t, tx, "plan_tasks", 18)
					return
				}
				columns := make([]string, len(restarted))
				for i, r := range restarted {
					columns[i] = r.column
				}
				wantEveryColumnCarried(t, tx, "plan_tasks", 9, columns...)
				for _, r := range restarted {
					var got string
					must(t, tx.QueryRow(ctx, fmt.Sprintf(`select %s::text from plan_tasks where issue = 'LEGION-208' and generation = 2`, r.column)).Scan(&got))
					if got != r.value {
						t.Fatalf("the task carried from its start holds %s %q, want %q: it waits at no act with every count 0", r.column, got, r.value)
					}
				}
			})
		})
	}
}
