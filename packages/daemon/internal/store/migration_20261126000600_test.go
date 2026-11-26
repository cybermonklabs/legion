package store

import (
	"context"
	"strings"
	"testing"
)

// decisionsStepGoneVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const decisionsStepGoneVersion = 20261126000600

// 20261126000600 deletes what an earlier daemon wrote of the retired design-gate step `decisions`
// and narrows the three checks that admitted it to `criteria` and `spec`. The store holds, as an
// earlier daemon left it, one row of each kind with the step `decisions`: a design_gates row, the
// pending pair of a READY the gate refused on that step (step and version set together, as
// issues_ready_pending_shape requires), and a readmitted_approvals row. Beside them stand a
// design_gates row of the step spec and a READY refused on the step spec, which the migration
// keeps. After the migrations no row of the three kinds holds `decisions`, and each of the three
// checks refuses a write of `decisions`, named in the refusal.
func TestTheDaemonStoreAdmitsNoDecisionsStep(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, decisionsStepGoneVersion-1)
	exec := func(statement string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq,
			ready_pending_step, ready_pending_version)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'a root an earlier daemon gated on decisions', 'merging', 1, 'retro', 'A', 1,
			'decisions', 2)`)
	exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq,
			ready_pending_step, ready_pending_version)
		values ('LEGION-300', 'LEGION-300', 'LEGION', 'a root refused on its spec', 'merging', 1, 'retro', 'B', 1, 'spec', 4)`)
	exec(`insert into design_gates (issue, step, artifact_id, latest_version)
		values ('LEGION-208', 'decisions', 'artifact-decisions', 2), ('LEGION-208', 'spec', 'artifact-spec', 3)`)
	exec(`insert into readmitted_approvals (issue, step, approved_version, latest_version)
		values ('LEGION-208', 'decisions', 1, 2)`)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the deletion of the decisions step: %v", err)
	}

	left := func(what, statement string) {
		t.Helper()
		rows, err := store.Pool().Query(ctx, statement)
		if err != nil {
			t.Fatalf("read the %s left: %v", what, err)
		}
		defer rows.Close()
		var issues []string
		for rows.Next() {
			var issue string
			if err := rows.Scan(&issue); err != nil {
				t.Fatalf("read the %s left: %v", what, err)
			}
			issues = append(issues, issue)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read the %s left: %v", what, err)
		}
		if len(issues) > 0 {
			t.Errorf("a %s of the step decisions is left on %s, want none", what, strings.Join(issues, ", "))
		}
	}
	left("design_gates row", `select issue from design_gates where step = 'decisions' order by issue`)
	left("readmitted_approvals row", `select issue from readmitted_approvals where step = 'decisions' order by issue`)
	left("pending READY", `select key from issues where ready_pending_step = 'decisions' or
		(ready_pending_step is null) <> (ready_pending_version is null) order by key`)

	var specGates int
	if err := store.Pool().QueryRow(ctx, `select count(*) from design_gates where issue = 'LEGION-208' and step = 'spec'`).
		Scan(&specGates); err != nil || specGates != 1 {
		t.Errorf("the spec step's gate of LEGION-208 = %d rows, %v; want the one row the migration keeps", specGates, err)
	}
	// A null half reads as the empty step or version 0, so the message names the values.
	pending := func(issue string) (string, int, error) {
		var step string
		var version int
		err := store.Pool().QueryRow(ctx, `select coalesce(ready_pending_step, ''), coalesce(ready_pending_version, 0)
			from issues where key = $1`, issue).Scan(&step, &version)
		return step, version, err
	}
	if step, version, err := pending("LEGION-208"); err != nil || step != "" || version != 0 {
		t.Errorf("the pending READY of LEGION-208 = step %q version %d (%v), want the pair cleared together", step, version, err)
	}
	if step, version, err := pending("LEGION-300"); err != nil || step != "spec" || version != 4 {
		t.Errorf("the pending READY of LEGION-300 = step %q version %d (%v), want the spec step at version 4 as it was", step, version, err)
	}

	for _, refused := range []struct{ check, statement string }{
		{"design_gates_step_check", `insert into design_gates (issue, step, artifact_id, latest_version)
			values ('LEGION-300', 'decisions', 'artifact-decisions', 1)`},
		{"issues_ready_pending_step_check", `update issues set ready_pending_step = 'decisions' where key = 'LEGION-300'`},
		{"readmitted_approvals_step_check", `insert into readmitted_approvals (issue, step, approved_version, latest_version)
			values ('LEGION-300', 'decisions', 1, 2)`},
	} {
		_, err := store.Pool().Exec(ctx, refused.statement)
		if err == nil || !strings.Contains(err.Error(), refused.check) {
			t.Errorf("a write of the step decisions = %v, want %s to refuse it", err, refused.check)
			continue
		}
		t.Logf("%s refuses the step decisions: %v", refused.check, err)
	}
}
