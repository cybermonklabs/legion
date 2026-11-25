package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// productionCheckReportedAtVersion is the version of the migration this file tests, so the test
// can migrate through everything before it whatever lands beside it.
const productionCheckReportedAtVersion = 20261122011615

// 20261122011615 adds production_checks.reported_at, when the daemon recorded the check's latest
// pending report (record.ProductionCheck.ReportedAt). Before it, a write of the column is refused:
// the column is not there. A check the release before recorded, with a pending report's wake
// standing, reads back after it with every field it held and no report's time, so the look that
// wake starts names the summary and no time, as that release's look did. A time written after it
// reads back as written, and a write of the record with none, as a look nobody reported makes,
// clears it.
func TestAProductionCheckRecordedBefore20261122011615HasNoReportTime(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, productionCheckReportedAtVersion-1)
	opened := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	wake := opened.Add(5 * time.Hour)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'merged root', 'done', 1, 'done', 'A', 1)`); err != nil {
		t.Fatalf("record the closed issue: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into production_checks
		(issue, project, tree, repo, pr_number, merge_sha, opened_at, bound_at, role, looks, look_open,
		 look_wake_at, last_wake_kind, last_wake_detail, last_wake_at, wake_at)
		values ('LEGION-208', 'LEGION', 'LEGION-208', 'sjawhar/legion', 1300, 'd4e5f6', $1, $2, 'tester', 1, false,
		 $1, 'merge', 'd4e5f6', $1, $3)`, opened, opened.Add(336*time.Hour), wake); err != nil {
		t.Fatalf("record the check with a pending report's wake standing, as the release before leaves it: %v", err)
	}
	_, err := store.Pool().Exec(ctx, `update production_checks set reported_at = now()`)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a write of reported_at before the migration = %v, want it refused: the column is the migration's", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the time of a check's pending report: %v", err)
	}
	read := func(when string) *record.ProductionCheck {
		t.Helper()
		var check *record.ProductionCheck
		if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			check, err = record.NewStore().ProductionCheck(ctx, tx, "LEGION-208")
			return err
		}); err != nil || check == nil {
			t.Fatalf("%s: read the production check: %+v, %v", when, check, err)
		}
		return check
	}
	write := func(check record.ProductionCheck) {
		t.Helper()
		if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			return record.NewStore().PutProductionCheck(ctx, tx, check)
		}); err != nil {
			t.Fatalf("write the production check: %v", err)
		}
	}
	check := read("after the migration")
	if check.ReportedAt != nil || check.Role != claim.RoleTester || check.Looks != 1 || check.LookOpen ||
		check.LastWakeKind != "merge" || check.WakeAt == nil || !check.WakeAt.Equal(wake) || !check.OpenedAt.Equal(opened) {
		t.Fatalf("the check after the migration = %+v, want look 1 closed with its wake at %s, as the release before wrote it, and no report's time",
			check, wake)
	}

	reported := opened.Add(2*time.Hour + 13*time.Minute)
	check.ReportedAt = &reported
	write(*check)
	if got := read("after a write of the report's time").ReportedAt; got == nil || !got.Equal(reported) {
		t.Fatalf("the check's report time = %v, want %s as written", got, reported)
	}
	check.ReportedAt = nil
	write(*check)
	if got := read("after a write with no report's time").ReportedAt; got != nil {
		t.Fatalf("the check's report time = %s, want none: the write carried none", got)
	}
}
