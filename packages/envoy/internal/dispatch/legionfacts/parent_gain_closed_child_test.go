package legionfacts

import (
	"fmt"
	"testing"
	"time"
)

// A closed child with no reported tree whose parent gains Legion phase work is placed under that
// parent by the computation; its stored row must follow.
func TestAParentGainingPhaseWorkMovesAClosedChildsRoot(t *testing.T) {
	run := newConsumer(t, 3)
	project := run.fixture.Project
	var child string
	if err := run.database.Pool.QueryRow(t.Context(), `
		select key from issues
		where project_key = $1 and closed_at is not null and parent_key is not null and key = any($2::text[])
		order by key limit 1`, project, run.fixture.PhaseWork).Scan(&child); err != nil {
		t.Fatalf("no closed child with phase work: %v", err)
	}
	var number int
	if err := run.database.Pool.QueryRow(t.Context(),
		`select max(number) + 1 from issues where project_key = $1`, project).Scan(&number); err != nil {
		t.Fatal(err)
	}
	parent := fmt.Sprintf("%s-%d", project, number)
	run.exec(t, "insert a tracking parent", `
		insert into issues (key, project_key, number, title, status, rank, priority, parent_key, created_by, created_at, closed_at)
		select $2, project_key, $3, 'A tracking issue', status, rank || 'x', priority, null, created_by, created_at, closed_at
		from issues where key = $1`, child, parent, number)
	run.exec(t, "reparent the child", `update issues set parent_key = $2 where key = $1`, child, parent)
	run.exec(t, "drop the child's reported tree", `delete from legion_observations where issue_key = $1 and field = 'tree'`, child)

	run.pass(t)
	before := run.rows(t)[child]
	if before.RootKey != child {
		t.Fatalf("%s's root is %q before the parent gained work, want itself, so this test examined nothing", child, before.RootKey)
	}

	run.writeEvent(t, parent, "architect", "comment.created", `{}`, run.at.Add(-time.Hour))
	run.at = run.at.Add(time.Minute)
	run.pass(t)

	fresh, err := Compute(t.Context(), run.database.Pool, project, []string{child}, run.at)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 1 {
		t.Fatalf("compute %s answers %d rows", child, len(fresh))
	}
	if fresh[0].RootKey != parent {
		t.Fatalf("the computation places %s under %q, not under %s, so this test examined nothing", child, fresh[0].RootKey, parent)
	}
	stored := run.rows(t)[child]
	t.Logf("%s: stored root %q depth %d; computed root %q depth %d",
		child, stored.RootKey, stored.TreeDepth, fresh[0].RootKey, fresh[0].TreeDepth)
	if stored.RootKey != fresh[0].RootKey || stored.TreeDepth != fresh[0].TreeDepth {
		t.Errorf("closed %s's stored row places it under %q at depth %d; its computation places it under %q at depth %d",
			child, stored.RootKey, stored.TreeDepth, fresh[0].RootKey, fresh[0].TreeDepth)
	}
}
