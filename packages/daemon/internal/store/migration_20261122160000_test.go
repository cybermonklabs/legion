package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// creditAtTurnStartVersion is the version of the migration this file tests:
// 20261122160000_design_news_credit_at_turn_start, which writes design_news_told_in again so a
// delivery's copy of the news counts as a credit only once its take's turn has started.
const creditAtTurnStartVersion = 20261122160000

// branchOnlyMigrations are the two versions this branch embeds that main's set does not:
// the task numbers, which add pending_task_deliveries.turn_started, and the credit at the take's
// turn start, which reads it. A database main migrated has applied every other version,
// 20261119203047_design_news among them, whose design_news_told_in counts a copy at confirmed_at.
var branchOnlyMigrations = []int{taskNumbersVersion, creditAtTurnStartVersion}

// The credit at the take's turn start holds however the store came to this release. On a fresh
// database the store applies every migration in version order: the task numbers, then
// 20261119203047_design_news, which creates design_news_told_in, then this migration, which writes
// it again. On a database main migrated, design_news has already run; the upgrade applies the task
// numbers and then this migration. In both, a delivery confirmed at its accepted take, its copy
// written and its turn not started, credits nothing: the child's holder still holds version 3 and
// owes the opening, and the root's architect knows no version of the Verification document. Once
// the delivery records its turn started, the same copy is the credit: version 5 to the holder with
// the opening cleared, version 2 of the Verification document to the architect.
//
// The negative control is this migration's absence: with it left out of the set, the first read
// finds the copy credited at the accept (version 5, no opening owed), as design_news's text counts
// it, and the case fails there.
//
// The way back, the last paragraph of the migration's header, is run in each case after that:
// the same delivery, turn started or not, then counts at the confirmation, as design_news's text
// counts it, the function carries no comment, and the version row is gone, so a second Migrate
// runs this file again and the credit waits for the turn once more.
func TestADeliverysCopyCreditsOnlyOnceItsTakesTurnStartedHoweverTheStoreWasMigrated(t *testing.T) {
	t.Parallel()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	var mains []migrations.Migration
	for _, migration := range all {
		if !slices.Contains(branchOnlyMigrations, migration.Version) {
			mains = append(mains, migration)
		}
	}
	if len(mains) != len(all)-len(branchOnlyMigrations) {
		t.Fatalf("the embedded set holds %d migrations and %d of them are main's, want all but %v", len(all), len(mains), branchOnlyMigrations)
	}

	for _, tc := range []struct {
		name     string
		mainsRun bool
	}{
		{name: "a fresh database"},
		{name: "a database main migrated, upgraded", mainsRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := emptyStore(t)
			if tc.mainsRun {
				if _, err := store.migrate(ctx, mains); err != nil {
					t.Fatalf("migrate the database as main did: %v", err)
				}
			}
			if _, err := store.Migrate(ctx); err != nil {
				t.Fatalf("migrate the database to this release: %v", err)
			}
			r := toldRigOn(t, store)
			r.seedTold()

			taken := r.newsTask("outbox:7")
			taken.ConfirmedAt = r.at.Add(time.Minute)
			if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, taken); err != nil {
				t.Fatalf("accept the take: %v", err)
			}
			wantNoCredit := func(why string) {
				t.Helper()
				r.wantTold(why, toldChild, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 3}, true)
				r.wantTold(why, toldRoot, record.AudienceArchitect, map[record.DesignStep]int{record.DesignStepSpec: 5}, false)
			}
			wantCredit := func(why string) {
				t.Helper()
				r.wantTold(why, toldChild, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 5}, false)
				r.wantTold(why, toldRoot, record.AudienceArchitect,
					map[record.DesignStep]int{record.DesignStepSpec: 5, record.DesignStepCriteria: 2}, false)
			}
			wantNoCredit("after the accepted take, before its turn")

			started := taken
			started.TurnStarted = true
			if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, started); err != nil {
				t.Fatalf("record the take's turn started: %v", err)
			}
			wantCredit("once the take's turn started")

			if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, taken); err != nil {
				t.Fatalf("put the take back to await its turn: %v", err)
			}
			wantNoCredit("after the take was put back")

			r.exec(wayBackOf(t, creditAtTurnStartVersion, "20261122160000_design_news_credit_at_turn_start"))
			wantCredit("after the way back, the turn not started")
			var comment *string
			if err := r.store.Pool().QueryRow(ctx, `select obj_description('design_news_told_in(text)'::regprocedure, 'pg_proc')`).Scan(&comment); err != nil {
				t.Fatalf("read the comment on design_news_told_in: %v", err)
			}
			if comment != nil {
				t.Errorf("design_news_told_in carries the comment %q after the way back, want none, as design_news wrote it", *comment)
			}
			var recorded int
			if err := r.store.Pool().QueryRow(ctx, `select count(*) from schema_version where version = $1::bigint`,
				creditAtTurnStartVersion).Scan(&recorded); err != nil {
				t.Fatalf("read whether %d is recorded: %v", creditAtTurnStartVersion, err)
			}
			if recorded != 0 {
				t.Fatalf("schema_version records %d %d times after the way back, want none, so the roll forward runs it again", creditAtTurnStartVersion, recorded)
			}

			applied, err := store.Migrate(ctx)
			if err != nil {
				t.Fatalf("roll forward after the way back: %v", err)
			}
			if applied != 1 {
				t.Errorf("the roll forward applied %d migrations, want 1, this one", applied)
			}
			wantNoCredit("after the roll forward, the turn not started")
		})
	}
}
