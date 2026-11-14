package store

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The three migrations of the document-search table: 20261101182447 creates it with its GIN
// index, 20261101182454 adds the function and the trigger that keep it at each document's latest
// version, and 20261101182501 creates the one row that records how far the boot's fill of it has
// got (store.FillDocumentSearch).
const (
	documentSearchTable    = 20261101182447
	documentSearchTrigger  = 20261101182454
	documentSearchBackfill = 20261101182501
)

// The trigger on events that keeps session_titles (20261113082156) filled, recording each stamped
// title; 20261113090413 after it creates the one row that stands for the boot's fill of the events
// stored before it (store.FillSessionTitles).
const sessionTitlesTrigger = 20261113090412

// 20261101182454 and 20261113090412 are each a function and a trigger and nothing else, which is
// what lets them ship: the pre-deploy census refuses any migration that locks a table above its
// size limit, and each trigger's table is among the largest a deployment holds (artifact_versions
// is 5.2 GiB in production; events is larger). Neither statement reads a row, so the time the
// migration holds its lock does not grow with the table and the limit does not apply to it
// (pgmigrate tableSizeExemption), a constraint trigger included. The limit is injected at one byte
// so the test needs no gigabyte, over a database at the migration before each, where its table is
// already above that.
func TestCensusExemptsATriggerMigrationFromTheTableLimit(t *testing.T) {
	for _, migration := range []struct {
		name    string
		before  int
		trigger int
		table   string
	}{
		{"the document-search trigger", documentSearchTable, documentSearchTrigger, "artifact_versions"},
		{"the session titles trigger", sessionTitlesTrigger - 1, sessionTitlesTrigger, "events"},
	} {
		t.Run(migration.name, func(t *testing.T) {
			store := openEmptyTestStore(t)
			migrateThrough(t, store, migration.before)
			report, err := census(context.Background(), store.Pool.Config().ConnString(),
				migrationsThrough(t, migration.trigger), pgmigrate.CensusOptions{TableLimit: 1})
			if err != nil {
				t.Fatalf("census: %v", err)
			}
			if report.Refused() {
				t.Errorf("%s was refused: %v", migration.name, refusals(report))
			}
			if len(report.Pending) != 1 || report.Pending[0].TableSizeExemption != "create function and create trigger" {
				t.Errorf("pending = %+v, want %d alone, exempt as a function and a trigger", report.Pending, migration.trigger)
			}
			if out := reportText(report); !strings.Contains(out, "touches "+migration.table+": ") ||
				!strings.Contains(out, "from which the migration is exempt because its statements are only create function and create trigger") {
				t.Errorf("report:\n%s", out)
			}
		})
	}
}

// What each migration of the document-search table and of session_titles' trigger and fill
// locks above ACCESS SHARE, read as the census reads it. The document-search table's own
// migration declares no foreign key, since a `references` to artifacts or to artifact_versions
// would have the census size that table and refuse every deploy. Each trigger's migration touches
// the table it is created on alone (the function's own write is its body, which runs on later
// writes and not in the migration), and the test above is why it may; each fill's progress table
// names nothing but itself, because each fill reads its source outside every migration.
func TestTheTriggerMigrationsTouchOnlyWhatTheyMustLock(t *testing.T) {
	for _, migration := range []struct {
		name  string
		files string
		want  []string
	}{
		{"20261101182447, the table and its index", "20261101182447_document_search.up.sql", []string{"document_search"}},
		{"20261101182454, the function and the trigger", "20261101182454_artifact_versions_document_search_trigger.up.sql", []string{"artifact_versions"}},
		{"20261101182501, the fill's progress row", "20261101182501_document_search_backfill.up.sql", []string{"document_search_backfill"}},
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
