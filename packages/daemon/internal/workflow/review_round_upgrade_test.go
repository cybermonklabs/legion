package workflow

import (
	"context"
	"math"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// phaseReviewedHeadMigration is the version of the migration that keeps the commit the reviewer's
// newest recorded review read (20261116234034_phase_reviewed_head.up.sql).
const phaseReviewedHeadMigration = 20261116234034

// storeBeforeTheReviewedHead is a store on a database of its own carrying every embedded migration
// but the reviewed commit's, as the release before it leaves the database, and a pool on the same
// database for the package's helpers. Each migration is applied with its version row as
// store.apply applies it, the version column widened at the first timestamped one, as
// internal/daemon's preEntryStore does: the record's own writes name phases.reviewed_head, which
// this database does not have yet, so the rows the test seeds are written in SQL.
func storeBeforeTheReviewedHead(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	dsn := testdb.Database(t, "legion_workflow_reviewed_head_upgrade")
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open the database of the release before the reviewed commit: %v", err)
	}
	t.Cleanup(st.Close)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	widened := false
	for _, migration := range all {
		if migration.Version == phaseReviewedHeadMigration {
			continue
		}
		if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, migration.SQL); err != nil {
				return err
			}
			if migration.Version > math.MaxInt32 && !widened {
				if _, err := tx.Exec(ctx, "alter table schema_version alter column version type bigint"); err != nil {
					return err
				}
				widened = true
			}
			_, err := tx.Exec(ctx, "insert into schema_version (version) values ($1::bigint)", migration.Version)
			return err
		}); err != nil {
			t.Fatalf("apply %s: %v", migration.Name, err)
		}
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open a pool on the database of the release before: %v", err)
	}
	t.Cleanup(pool.Close)
	return st, pool
}

// The release before this one stores the reviewer's task with neither sentence of the review round,
// and this release sends a stored task as it was stored (record.SuperviseRequest.RoundNamed), so
// the migration that adds the reviewed commit appends the round's two sentences to each task a
// reviewer's row stores: the commit `none`, since the new column is empty on every row, and the
// row's entry. Its text is the one namingTheReviewRound writes for that row, so the two cannot
// drift. A reviewer's row that stores no task and a row of another role are left as they were.
// The owed entry sent after the upgrade (SendOwedEntry) delivers the stored task word for word,
// ending with the two sentences.
func TestTheReviewedHeadMigrationGivesEachStoredReviewerTaskTheRoundsSentences(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, pool := storeBeforeTheReviewedHead(t)
	const (
		reviewerTask = "Continue root. Issue: LEGION-208. Phase: reviewing. Handoff: test version 1 " +
			"(dispatch handoff-read --issue LEGION-208 --phase test --version 1). Tester's verdict: pass. Pull request: #42."
		testerTask = "Continue root. Issue: LEGION-208. Phase: testing. Resume the existing phase work."
	)
	if _, err := pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'reviewing', 1, 'needs_review', 'A', 1),
		('LEGION-209', 'LEGION-209', 'LEGION', 'other', 'reviewing', 1, 'needs_review', 'B', 1)`); err != nil {
		t.Fatalf("record the issues as the release before writes them: %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry, entry_task) values
		('LEGION-208', 'reviewer', 'legion-LEGION-208-reviewer', '', 0, '', 2, $1),
		('LEGION-208', 'tester', 'legion-LEGION-208-tester', '', 0, '', 1, $2),
		('LEGION-209', 'reviewer', 'legion-LEGION-209-reviewer', '', 0, '', 3, '')`, reviewerTask, testerTask); err != nil {
		t.Fatalf("record the phases rows as the release before writes them: %v", err)
	}

	applied, err := st.Migrate(ctx)
	if err != nil {
		t.Fatalf("migrate through the reviewed commit: %v", err)
	}
	if applied != 1 {
		t.Fatalf("the upgrade applied %d migrations, want 1: the reviewed commit's alone", applied)
	}

	reviewer := reviewerRow(t, pool)
	want := namingTheReviewRound(reviewerTask, claim.RoleReviewer, phase.Reviewing,
		record.PhaseRow{Role: claim.RoleReviewer, Entry: 2, EntryTask: reviewerTask}, nil)
	if reviewer.EntryTask != want || want != reviewerTask+reviewedPartOf("none")+reviewEntryPartOf(2) {
		t.Errorf("the reviewer's stored task after the upgrade = %q, want %q: the task the release before stored, "+
			"then the two sentences namingTheReviewRound writes for its row", reviewer.EntryTask, want)
	}
	if got := roleRow(t, pool, claim.RoleTester).EntryTask; got != testerTask {
		t.Fatalf("the tester's stored task after the upgrade = %q, want %q as the release before stored it", got, testerTask)
	}
	var empty string
	if err := pool.QueryRow(ctx, `select entry_task from phases where issue = 'LEGION-209' and role = 'reviewer'`).Scan(&empty); err != nil || empty != "" {
		t.Fatalf("the stored task of a reviewer's row that stored none = %q (%v), want none", empty, err)
	}

	issue := issueRecord(t, pool, "LEGION-208")
	engine := testEngine(config.DesignGateRootIssues, nil)
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		return engine.SendOwedEntry(ctx, tx, *issue, claim.RoleReviewer, reviewClaim, reviewer)
	})
	if delivered := deliveredTask(t, pool, "the owed entry after the upgrade"); delivered != want {
		t.Fatalf("the owed entry after the upgrade is delivered as %q, want %q word for word: it ends with the daemon's two sentences", delivered, want)
	}
}
