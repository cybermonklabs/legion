package store

import (
	"strings"
	"testing"
)

func TestTaskAgentPhaseRollbackRefusesLossAndPreservesASingleReconstructibleRow(t *testing.T) {
	st := migratedStore(t)
	ctx := t.Context()
	openRunningTree(t, st)
	_, err := st.Pool().Exec(ctx, `insert into phases (issue, role, task, agent, step_task, step_place, claim, handoff_commit, rounds, verdict, entry)
		values ($1, 'implementer', 'a', 'implementer', 'a', 1, 'legion-legion-legion-208-implementer', 'kept', 3, '', 7),
		($1, 'implementer', 'b', 'implementer-2', 'b', 1, 'legion-legion-legion-208-implementer-2', 'other', 5, '', 9)`, rootIssue)
	if err != nil {
		t.Fatal(err)
	}
	back := heredocWayBackOf(t, 20261125055751)
	if err := runWayBack(t, st, back); err == nil || !strings.Contains(err.Error(), "more than one task agent") {
		t.Fatalf("a rollback with two task agents answered %v, want a refusal naming their collision", err)
	}
	if _, err := st.Pool().Exec(ctx, `delete from phases where issue = $1 and task = 'b'`, rootIssue); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `update phases set step_task = '' where issue = $1`, rootIssue); err != nil {
		t.Fatal(err)
	}
	if err := runWayBack(t, st, back); err == nil || !strings.Contains(err.Error(), "Cannot reconstruct") {
		t.Fatalf("a rollback with durable task identity absent from the generation answered %v, want a refusal", err)
	}
	if _, err := st.Pool().Exec(ctx, `update phases set step_task = task where issue = $1`, rootIssue); err != nil {
		t.Fatal(err)
	}
	if err := runWayBack(t, st, back); err != nil {
		t.Fatalf("roll back a reconstructible row: %v", err)
	}
	var commit string
	var rounds, entry int
	if err := st.Pool().QueryRow(ctx, `select handoff_commit, rounds, entry from phases where issue = $1 and role = 'implementer'`, rootIssue).Scan(&commit, &rounds, &entry); err != nil || commit != "kept" || rounds != 3 || entry != 7 {
		t.Fatalf("the earlier schema kept commit %q, rounds %d, entry %d, error %v; want kept/3/7", commit, rounds, entry, err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("roll forward again: %v", err)
	}
	var task, agent string
	if err := st.Pool().QueryRow(ctx, `select task, agent from phases where issue = $1 and role = 'implementer'`, rootIssue).Scan(&task, &agent); err != nil || task != "a" || agent != "implementer" {
		t.Fatalf("the roll forward restored task %q and agent %q, error %v; want a/implementer", task, agent, err)
	}
}

func TestTaskReleaseClaimsCutoverAndRollbackRefuseAnUnfinishedSuspendedSet(t *testing.T) {
	st := emptyStore(t)
	ctx := t.Context()
	migrateThrough(t, st, 20261125114614-1)
	openRunningTree(t, st)
	_, err := st.Pool().Exec(ctx, `insert into issue_releases (project, issue, generation, first_row, boundary_row, last_row, started_row, role, suspended)
		values ('legion', $1, 1, 18, 20, 22, 0, 'architect', array['implementer'])`, rootIssue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "Finish in-flight pod releases") {
		t.Fatalf("cut over a role-only suspended set: %v, want an explicit refusal", err)
	}
	if _, err := st.Pool().Exec(ctx, `delete from issue_releases where issue = $1`, rootIssue); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("cut over after the earlier release ended: %v", err)
	}
	markRelease(t, st, 18, 20, 22, 0, "implementer")
	back := heredocWayBackOf(t, 20261125114614)
	if err := runWayBack(t, st, back); err == nil || !strings.Contains(err.Error(), "Finish in-flight pod releases") {
		t.Fatalf("roll back an unfinished exact-claim set: %v, want a refusal", err)
	}
	if _, err := st.Pool().Exec(ctx, `delete from issue_releases where issue = $1`, rootIssue); err != nil {
		t.Fatal(err)
	}
	if err := runWayBack(t, st, back); err != nil {
		t.Fatalf("roll back after the release ended: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the rollback: %v", err)
	}
	var hasColumn bool
	if err := st.Pool().QueryRow(ctx, `select exists (select 1 from information_schema.columns where table_schema = 'public' and table_name = 'issue_releases' and column_name = 'suspended_claims')`).Scan(&hasColumn); err != nil || !hasColumn {
		t.Fatalf("restored exact-claim column = %t, error %v", hasColumn, err)
	}
}
