package store

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// 20261128105250 puts the coverage counts' triggers on ci_check_results and replaces the truncate
// trigger of check_results. A deployment of its release can hold before it, pending: the two
// migrations of its own release that no earlier release holds, ci_check_results' table
// (20261116213930) and pull_request_heads' attribution inputs (20261120144431), and, where that
// release has not deployed yet, the coverage counts (20261124011834), whose functions the triggers
// call.
const (
	ciCheckResultsVersion        = 20261116213930
	attributionInputsVersion     = 20261120144431
	coverageCountsVersion        = 20261124011834
	resultsCoverageCountsVersion = 20261128105250
)

// The pre-deploy census admits 20261128105250 beside each set of migrations a deployment can hold
// pending before it, over a database that holds every other migration below it. The limit is one
// byte, so check_results, which every such database holds with rows, is above it, and only the
// exemption of a migration whose statements are only catalog writes (pgmigrate's
// tableSizeExemption) admits the migration. TestCensusRefusesTheResultsCoverageTriggersBesideBtreeGin
// is the control: it shows that the census judges the size of check_results here, so these
// admissions are the census's judgment of these migrations.
func TestCensusAdmitsTheResultsCoverageTriggersBesideWhatIsPendingBeforeThem(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name      string
		unapplied []int
	}{
		{"beside its release's two", []int{ciCheckResultsVersion, attributionInputsVersion}},
		{"beside its release's two and the coverage counts", []int{ciCheckResultsVersion, attributionInputsVersion, coverageCountsVersion}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			report, entry := resultsCoverageCensus(t, row.unapplied, nil)
			if len(entry.Refusals) != 0 || entry.TableSizeExemption == "" {
				t.Errorf("%s: refusals %q, exemption %q; want no refusal and the catalog-only exemption; report:\n%s",
					entry.Migration.Name, entry.Refusals, entry.TableSizeExemption, reportText(report))
			}
		})
	}
}

// The census refuses 20261128105250 beside btree_gin's migration, pending as well with its
// extension in place: its create extension is a migration whose effect the census cannot read, so
// it takes the catalog-only exemption from every later pending migration (pgmigrate.Census,
// laterEventTrigger), and the census then judges 20261128105250 on the size of check_results and
// names btree_gin's migration. This is why btree_gin's release deploys before every later
// migration.
func TestCensusRefusesTheResultsCoverageTriggersBesideBtreeGin(t *testing.T) {
	t.Parallel()
	report, entry := resultsCoverageCensus(t,
		[]int{ciCheckResultsVersion, attributionInputsVersion, coverageCountsVersion}, []int{btreeGinVersion})
	extension, want, refused := sizeRefusalBesideBtreeGin(t, report, entry.Refusals, "check_results")
	if entry.TableSizeExemption != "" || !refused {
		t.Errorf("%s beside the pending %s: exemption %q, refusals %q; want no exemption and a refusal on the size of check_results that holds each of %q",
			entry.Migration.Name, extension, entry.TableSizeExemption, entry.Refusals, want)
	}
}

// resultsCoverageCensus takes the census, at a table limit of one byte, of the embedded migrations
// through 20261128105250 over a database that holds every migration below it but unapplied, which
// it never ran, and forgotten, which it ran and whose version rows are gone, as btree_gin's would
// be pending with its extension in place. check_results holds one row, so it is above the limit.
// It fails the test unless the pending migrations are 20261128105250, unapplied and forgotten, and
// 20261128105250 touches check_results, with its row, and ci_check_results, which exists unless
// unapplied holds its table's migration. It answers the report and 20261128105250's entry.
func resultsCoverageCensus(t *testing.T, unapplied, forgotten []int) (*pgmigrate.Report, *pgmigrate.MigrationCensus) {
	t.Helper()
	ctx := context.Background()
	store := openMigratedTestStore(t, ciCheckResultsVersion-1)
	migrations := shippedMigrations(t)
	for _, migration := range migrations {
		if migration.Version < ciCheckResultsVersion || migration.Version >= resultsCoverageCountsVersion || slices.Contains(unapplied, migration.Version) {
			continue
		}
		if err := store.applyMigration(ctx, migrations, migration); err != nil {
			t.Fatalf("apply %s: %v", migration.Name, err)
		}
	}
	if _, err := store.Pool.Exec(ctx, `delete from schema_migrations where version = any($1)`, forgotten); err != nil {
		t.Fatalf("forget %v: %v", forgotten, err)
	}
	mustExec(t, store, `insert into projects (key, name, default_assignee) values ('CNSS', 'Census', 'alice')`)
	mustExec(t, store, `insert into issues (key, project_key, number, title, created_by, rank)
		values ('CNSS-1', 'CNSS', 1, 'An issue that holds a result', '{"kind":"user","id":"alice"}', 'A')`)
	mustExec(t, store, `insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('CNSS-1', 'CNSS', 'checks', 'checks.md', 'doc', '{"kind":"user","id":"alice"}')`)
	mustExec(t, store, `insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id,
			check_version, check_sha256, role, commit_sha, verdict, actor)
		select 'CNSS-1', 'CNSS-1', id, 'k1', 1, 'a-check-sha', 'tester', repeat('a', 40), 'pass', '{"kind":"user","id":"alice"}'::jsonb
		from artifacts where issue_key = 'CNSS-1'`)

	report, err := census(ctx, store.Pool.Config().ConnString(), migrationsThrough(t, resultsCoverageCountsVersion), pgmigrate.CensusOptions{TableLimit: 1})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	wantPending := slices.Sorted(slices.Values(append(append([]int{resultsCoverageCountsVersion}, unapplied...), forgotten...)))
	var pending []int
	var entry *pgmigrate.MigrationCensus
	for i := range report.Pending {
		pending = append(pending, report.Pending[i].Migration.Version)
		if report.Pending[i].Migration.Version == resultsCoverageCountsVersion {
			entry = &report.Pending[i]
		}
	}
	if !slices.Equal(pending, wantPending) || entry == nil {
		t.Fatalf("pending = %v, want %v; report:\n%s", pending, wantPending, reportText(report))
	}
	ciCheckResultsExists := !slices.Contains(unapplied, ciCheckResultsVersion)
	var touched []string
	for _, table := range entry.Tables {
		touched = append(touched, table.Name)
		switch {
		case table.Name == "check_results" && (!table.Exists || table.Bytes <= 1):
			t.Errorf("check_results exists %v at %d bytes, which leaves the limit nothing to judge", table.Exists, table.Bytes)
		case table.Name == "ci_check_results" && table.Exists != ciCheckResultsExists:
			t.Errorf("ci_check_results exists %v, want %v", table.Exists, ciCheckResultsExists)
		}
	}
	if !slices.Equal(touched, []string{"check_results", "ci_check_results"}) {
		t.Errorf("%s touches %v, want check_results and ci_check_results", entry.Migration.Name, touched)
	}
	return report, entry
}

// mustExec runs sql on store, failing the test on an error.
func mustExec(t *testing.T, store *Store, sql string, args ...any) {
	t.Helper()
	if _, err := store.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", strings.TrimSpace(sql), err)
	}
}
