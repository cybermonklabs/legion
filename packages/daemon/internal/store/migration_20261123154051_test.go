package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// checkOfEachMergeVersion is the version of the migration this file tests, so a test can migrate
// through everything before it whatever lands beside it.
const checkOfEachMergeVersion = 20261123154051

// checkOfEachMergeRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const checkOfEachMergeRelease = "the release that keeps a production check for each merge"

// 20261123154051 keys a production check by its issue and the moment it opened, and gives it the
// run and the task of its merge. A check the release before wrote reads after it with run 0 and
// no task, every other field as that release left it, and the read of the issue's check answers
// it. The two columns are unknown before the migration, which is what shows the fixture is the
// old shape.
func TestTheCheckOfTheReleaseBeforeReadsWithGenerationZeroAndNoTask(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, checkOfEachMergeVersion-1)
	opened := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	for _, seed := range []string{
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'done', 4, 'done', 'A', 1)`,
		`insert into production_checks (issue, project, tree, repo, pr_number, merge_sha, opened_at, bound_at, role, looks, look_open)
			values ('LEGION-208', 'LEGION', 'LEGION-208', 'sjawhar/legion', 1208, 'm1', '2026-11-17T08:00:00Z', '2026-12-01T08:00:00Z', 'tester', 1, true)`,
	} {
		if _, err := store.Pool().Exec(ctx, seed); err != nil {
			t.Fatalf("record before the migration: %v", err)
		}
	}
	for _, column := range []string{"generation = 1", "task = 'a'"} {
		_, err := store.Pool().Exec(ctx, "update production_checks set "+column)
		if err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("update production_checks set %s before the migration = %v, want the column unknown: the fixture is not the old shape", column, err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the check of each merge: %v", err)
	}
	var check *record.ProductionCheck
	if err := pgx.BeginFunc(ctx, store.Pool(), func(tx pgx.Tx) error {
		var err error
		check, err = record.NewStore().ProductionCheckOf(ctx, tx, "LEGION-208", time.Time{})
		return err
	}); err != nil {
		t.Fatalf("read the check of LEGION-208 after the migration: %v", err)
	}
	if check == nil || check.Number != 1208 || check.MergeSHA != "m1" || !check.OpenedAt.Equal(opened) || check.Looks != 1 || !check.LookOpen {
		t.Fatalf("the check after the migration = %+v, want the one the release before left", check)
	}
	if check.Generation != 0 || check.Task != "" {
		t.Fatalf("the check after the migration is of run %d and task %q, want run 0 and no task: the release before ran no plan", check.Generation, check.Task)
	}
}

// The way back of the check of each merge, the last paragraph of the migration's header
// (migrations.WayBack), run as its one transaction, leaves the table as the release before reads
// and writes it: each issue keeps its newest check alone, the one that release reads, the key is
// the issue again, the run and the task of a check are gone, and so is the migration's version
// row. So the roll forward's Migrate applies the file again, and the key and the two columns stand
// again.
func TestTheWayBackOfTheCheckOfEachMergeRestoresTheKeyByIssue(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	opened := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	closed := opened.Add(30 * time.Minute)
	twice, once := "LEGION-401", "LEGION-402"
	if err := pgx.BeginFunc(ctx, store.Pool(), func(tx pgx.Tx) error {
		records := record.NewStore()
		for _, key := range []string{twice, once} {
			if err := records.PutIssue(ctx, tx, record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: key,
				Project: "LEGION", Title: key, Phase: "done", Generation: 3, Status: "done", Rank: "U"}); err != nil {
				return err
			}
		}
		for _, check := range []record.ProductionCheck{
			{Issue: twice, Project: "LEGION", Tree: twice, Repo: "sjawhar/legion", Number: 1, Role: claim.RoleTester, OpenedAt: opened, BoundAt: opened.Add(time.Hour),
				ClosedAt: &closed, Outcome: record.CheckNotRun, Generation: 3, Task: "a"},
			{Issue: twice, Project: "LEGION", Tree: twice, Repo: "sjawhar/legion", Number: 2, Role: claim.RoleTester, OpenedAt: opened.Add(time.Hour), BoundAt: opened.Add(2 * time.Hour),
				Looks: 1, LookOpen: true, Generation: 3, Task: "b"},
			{Issue: once, Project: "LEGION", Tree: once, Repo: "sjawhar/legion", Number: 3, Role: claim.RoleTester, OpenedAt: opened, BoundAt: opened.Add(time.Hour), Looks: 1},
		} {
			if err := records.PutProductionCheck(ctx, tx, check); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("record two checks of %s and one of %s: %v", twice, once, err)
	}

	if _, err := store.Pool().Exec(ctx, wayBackOf(t, checkOfEachMergeVersion, checkOfEachMergeRelease), pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("run the way back of the check of each merge as its one transaction: %v", err)
	}
	const kept = `select row(issue, pr_number)::text from production_checks order by issue`
	if got, want := rowsAsText(t, store, kept), []string{"(" + twice + ",2)", "(" + once + ",3)"}; !slices.Equal(got, want) {
		t.Fatalf("the checks after the way back = %v, want each issue's newest alone: %v", got, want)
	}
	if key := rowsAsText(t, store, productionCheckKey); !slices.Equal(key, []string{"issue"}) {
		t.Fatalf("the key of production_checks after the way back = %v, want (issue)", key)
	}
	if columns := rowsAsText(t, store, checkRunColumns); len(columns) != 0 {
		t.Fatalf("production_checks after the way back holds %v, want neither generation nor task", columns)
	}
	var recorded bool
	if err := store.Pool().QueryRow(ctx, `select exists (select 1 from schema_version where version = $1)`, checkOfEachMergeVersion).Scan(&recorded); err != nil {
		t.Fatalf("read the version row after the way back: %v", err)
	}
	if recorded {
		t.Fatalf("schema_version still holds %d after the way back, so the roll forward would not apply it again", checkOfEachMergeVersion)
	}

	applied, err := store.Migrate(ctx)
	if err != nil || applied != 1 {
		t.Fatalf("the roll forward applied %d migrations, %v; want 1, the check of each merge again", applied, err)
	}
	if key := rowsAsText(t, store, productionCheckKey); !slices.Equal(key, []string{"issue", "opened_at"}) {
		t.Fatalf("the key of production_checks after the roll forward = %v, want (issue, opened_at)", key)
	}
	if columns := rowsAsText(t, store, checkRunColumns); !slices.Equal(columns, []string{"generation", "task"}) {
		t.Fatalf("production_checks after the roll forward holds %v of the two columns, want both", columns)
	}
}

// productionCheckKey reads the columns of production_checks' primary key, in key order.
const productionCheckKey = `select a.attname::text from pg_index i
	join pg_attribute a on a.attrelid = i.indrelid and a.attnum = any(i.indkey)
	where i.indrelid = 'production_checks'::regclass and i.indisprimary
	order by array_position(i.indkey::int2[], a.attnum)`

// checkRunColumns reads which of production_checks' generation and task stand, in name order.
const checkRunColumns = `select column_name::text from information_schema.columns
	where table_schema = current_schema() and table_name = 'production_checks' and column_name in ('generation', 'task')
	order by column_name`
