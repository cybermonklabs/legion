package store

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The trigger on events that keeps session_titles (20261113082156) filled, recording each stamped
// title; 20261113090413 after it creates the one row that stands for the boot's fill of the events
// stored before it (store.FillSessionTitles).
const sessionTitlesTrigger = 20261113090412

// 20261113090412 is a function and a trigger and nothing else, which is what lets it ship: the
// pre-deploy census refuses any migration that locks a table above its size limit, and events is
// the largest table a deployment holds. Neither statement reads a row, so the time the migration
// holds its lock does not grow with the table and the limit does not apply to it (pgmigrate
// tableSizeExemption), a constraint trigger included. The limit is injected at one byte so the
// test needs no gigabyte, over a database at the migration before it, where events is already
// above that.
func TestCensusExemptsTheSessionTitlesTriggerFromTheTableLimit(t *testing.T) {
	store := openEmptyTestStore(t)
	migrateThrough(t, store, sessionTitlesTrigger-1)
	report, err := census(context.Background(), store.Pool.Config().ConnString(),
		migrationsThrough(t, sessionTitlesTrigger), pgmigrate.CensusOptions{TableLimit: 1})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if report.Refused() {
		t.Errorf("the session titles trigger was refused: %v", refusals(report))
	}
	if len(report.Pending) != 1 || report.Pending[0].TableSizeExemption != "create function and create trigger" {
		t.Errorf("pending = %+v, want 20261113090412 alone, exempt as a function and a trigger", report.Pending)
	}
	if out := reportText(report); !strings.Contains(out, "touches events: ") {
		t.Errorf("report:\n%s", out)
	}
}

// What each of the two migrations locks above ACCESS SHARE, read as the census reads it: the
// trigger's migration touches events alone, the table it is created on (the function's own insert
// into session_titles is its body, which runs on later inserts and not in the migration), and the
// fill's progress table names nothing but itself, because the fill reads events outside every
// migration.
func TestTheSessionTitlesMigrationsTouchOnlyWhatTheyMustLock(t *testing.T) {
	for _, migration := range []struct {
		name  string
		files string
		want  []string
	}{
		{"20261113090412, the function and the trigger", "20261113090412_events_session_titles.up.sql", []string{"events"}},
		{"20261113090413, the fill's progress row", "20261113090413_session_titles_backfill.up.sql", []string{"session_titles_backfill"}},
	} {
		t.Run(migration.name, func(t *testing.T) {
			sql, err := migrationFiles.ReadFile("migrations/" + migration.files)
			if err != nil {
				t.Fatalf("read %s: %v", migration.files, err)
			}
			if touched := pgmigrate.TouchedTables(string(sql)); !slices.Equal(touched, migration.want) {
				t.Fatalf("%s touches %v, want %v", migration.files, touched, migration.want)
			}
		})
	}
}
