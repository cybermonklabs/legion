package store

import (
	"context"
	"strings"
	"testing"
)

// issueWorkflowVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const issueWorkflowVersion = 20261116035434

// An issue's architect chooses the line of phases the issue walks and says why
// (dispatch://INFRA-3570), and the issue record keeps both. Before this migration the issues
// table has no column for either, which is the negative control read first: the daemon could
// record no choice at all, so every issue walked the one compiled line.
//
// After the migration both columns are empty on the row that stood across it, which is the truth
// about it — no architect chose a line for it, and an issue past admitted ran the compiled default
// — and a written choice reads back as written.
func TestAnIssueRecordsItsWorkflowWith20261116035434(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, issueWorkflowVersion-1)
	if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'implementing', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatalf("seed LEGION-208: %v", err)
	}

	var workflow, reason string
	err := store.Pool().QueryRow(ctx, `select workflow, workflow_reason from issues where key = 'LEGION-208'`).Scan(&workflow, &reason)
	if err == nil || !strings.Contains(err.Error(), "workflow") {
		t.Fatalf("reading an issue's workflow before the migration = %v, want the column to be missing: it is what the migration exists for", err)
	}
	t.Logf("before the migration the issue records no workflow: %v", err)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the issue's workflow: %v", err)
	}

	if err := store.Pool().QueryRow(ctx, `select workflow, workflow_reason from issues where key = 'LEGION-208'`).Scan(&workflow, &reason); err != nil {
		t.Fatalf("read the issue the migration found standing: %v", err)
	}
	if workflow != "" || reason != "" {
		t.Fatalf("the issue standing across the migration = workflow %q, reason %q, want both empty: nothing backfills a choice no architect made", workflow, reason)
	}
	t.Logf("after the migration the issue standing across it names no workflow and no reason")

	if _, err := store.Pool().Exec(ctx, `update issues set workflow = 'light', workflow_reason = 'a one-line fix' where key = 'LEGION-208'`); err != nil {
		t.Fatalf("write the issue's workflow: %v", err)
	}
	if err := store.Pool().QueryRow(ctx, `select workflow, workflow_reason from issues where key = 'LEGION-208'`).Scan(&workflow, &reason); err != nil {
		t.Fatalf("read the written workflow: %v", err)
	}
	if workflow != "light" || reason != "a one-line fix" {
		t.Fatalf("the issue's workflow = %q, reason %q, want light and the reason written", workflow, reason)
	}
	t.Logf("after the migration a written workflow and its reason read back as written")
}
