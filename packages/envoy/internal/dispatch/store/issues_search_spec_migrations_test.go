package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The pair that puts an issue's primary document in the issue's own keyword vector:
// 20261105145242 redefines the issues trigger's function and re-indexes the issues stored before
// it, and 20261105145243 adds the trigger on artifact_versions that keeps the vector current.
// 20261124034909 makes that trigger re-vector an issue once for a statement's versions, and
// 20261128062654 makes 0140's trigger beside it enqueue the issue's embedding text once.
const (
	beforeIssuesSearchSpec  = 20261103070013
	issuesSearchSpec        = 20261105145242
	issuesSearchSpecTrig    = 20261105145243
	issuesSearchLatest      = 20261124034909
	embeddingsEnqueueLatest = 20261128062654
)

// seedIssueWithSpec writes one issue, its primary document and one version of it, at whatever
// schema the caller has migrated to, and returns the issue's key.
func seedIssueWithSpec(t *testing.T, ctx context.Context, store *Store, number int, title, spec string) string {
	t.Helper()
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name)
		select 'CORE', 'Core' where not exists (select 1 from projects where key = 'CORE')
	`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	key := fmt.Sprintf("CORE-%d", number)
	if _, err := store.Pool.Exec(ctx, `
		with i as (
			insert into issues (key, project_key, number, title, created_by, rank)
			values ($1, 'CORE', $2, $3, '{"kind":"user","id":"alice"}', $4) returning key
		),
		a as (
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			select $1, 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}' from i returning id
		)
		insert into artifact_versions (artifact_id, number, markdown, authors) select id, 1, $5, '[]' from a
	`, key, number, title, fmt.Sprintf("%d", number), spec); err != nil {
		t.Fatalf("seed issue %s: %v", key, err)
	}
	return key
}

// matchesIssue reads whether one issue's keyword vector matches query.
func matchesIssue(t *testing.T, ctx context.Context, store *Store, key, query string) bool {
	t.Helper()
	var matches bool
	if err := store.Pool.QueryRow(ctx,
		`select search @@ websearch_to_tsquery('english', $2) from issues where key = $1`, key, query,
	).Scan(&matches); err != nil {
		t.Fatalf("match %q against %s: %v", query, key, err)
	}
	return matches
}

// rankIssue reads ts_rank_cd of one issue's keyword vector against query, the rank api/search.go
// orders the issue arm by.
func rankIssue(t *testing.T, ctx context.Context, store *Store, key, query string) float32 {
	t.Helper()
	var rank float32
	if err := store.Pool.QueryRow(ctx,
		`select ts_rank_cd(search, websearch_to_tsquery('english', $2)) from issues where key = $1`, key, query,
	).Scan(&rank); err != nil {
		t.Fatalf("rank %q against %s: %v", query, key, err)
	}
	return rank
}

// 20261105145242 on a database that already holds issues: an issue whose title shares no word
// with its primary document is found by that document's words once the migration has re-indexed
// it, and the words of its key and title still outrank them, so the spec rides at a lower weight
// rather than at the title's.
func TestIssuesSearchSpecMigrationReindexesTheIssuesStoredBeforeIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openMigratedTestStore(t, beforeIssuesSearchSpec)
	const spec = "The rhumb lines of the Tyrrhenian portolan cross at Capo Testa.\n"
	key := seedIssueWithSpec(t, ctx, store, 1, "Tide tables for the equinox", spec)
	bare := seedIssueWithSpec(t, ctx, store, 2, "Sailing directions", "   \n")

	if matchesIssue(t, ctx, store, key, "Tyrrhenian") {
		t.Fatal("the issue already matched a word only its spec holds before the migration")
	}
	var beforeBare string
	if err := store.Pool.QueryRow(ctx, `select search::text from issues where key = $1`, bare).Scan(&beforeBare); err != nil {
		t.Fatalf("read the spec-less issue's vector: %v", err)
	}

	nameADefaultAssigneeOnEveryProject(t, store)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("apply the issue-spec search migrations: %v", err)
	}

	if !matchesIssue(t, ctx, store, key, "Tyrrhenian") {
		t.Error("the re-index did not give the issue the words of its primary document")
	}
	if !matchesIssue(t, ctx, store, key, "equinox") {
		t.Error("the re-index dropped the issue's own title from its vector")
	}
	if spec, title := rankIssue(t, ctx, store, key, "Tyrrhenian"), rankIssue(t, ctx, store, key, "equinox"); spec >= title {
		t.Errorf("a spec word ranks %v and a title word %v on the same issue; the spec must rank below the title", spec, title)
	}
	var afterBare string
	if err := store.Pool.QueryRow(ctx, `select search::text from issues where key = $1`, bare).Scan(&afterBare); err != nil {
		t.Fatalf("read the spec-less issue's vector: %v", err)
	}
	if afterBare != beforeBare {
		t.Errorf("an issue whose document holds no text was re-indexed:\n before %s\n after  %s", beforeBare, afterBare)
	}
}

// 20261105145243's trigger re-indexes an issue when its primary document gains a version, which
// is the only write that changes the issue's vector without touching the issue's own row.
func TestTheIssuesSearchTriggerIndexesAVersionWrittenAfterIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openMigratedTestStore(t, latestVersion)
	seedCoreWithADefaultAssignee(t, store)
	key := seedIssueWithSpec(t, ctx, store, 1, "Tide tables for the equinox", "Nothing of note.\n")
	if matchesIssue(t, ctx, store, key, "Tyrrhenian") {
		t.Fatal("the issue matched a word no version of its document holds")
	}

	if _, err := store.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select a.id, 2, 'The rhumb lines of the Tyrrhenian portolan cross at Capo Testa.', '[]'
		from artifacts a where a.issue_key = $1 and a.is_primary
	`, key); err != nil {
		t.Fatalf("write a second version: %v", err)
	}

	if !matchesIssue(t, ctx, store, key, "Tyrrhenian") {
		t.Error("the version's words did not reach the issue's vector")
	}
	if matchesIssue(t, ctx, store, key, "Nothing of note") {
		t.Error("the issue's vector still holds the version the new one replaced")
	}
}

// One statement that inserts many versions of an issue's primary document re-vectors the issue
// once (20261124034909), where 20261105145243's trigger re-vectored it for each version, every
// time over the same latest version. The issue ends with the vector the schema before the change
// wrote, byte for byte, so the change is the count alone. Each re-vector is an update of the
// issue's search, which 20261101182522's trigger records in search_term_queue, so the queue
// counts them. The second case's latest version holds no markdown (an image), which fires no
// trigger; the version below it still re-vectors the issue, over that image version as before.
// The third inserts a version below one the document already holds: it re-vectors nothing, where
// the old body rewrote the vector the stored latest version already gave, so the vector is the
// same only because it was current; no writer inserts below the latest.
func TestAMultiVersionInsertReVectorsItsIssueOnce(t *testing.T) {
	for _, test := range []struct {
		name, setup, versions string
		want, wantAfter       int
	}{
		{"eight versions with markdown", "", `
			select a.id, n, case when n = 9 then 'The rhumb lines of the Tyrrhenian portolan.' else 'Draft ' || n || '.' end, null, '[]'
			from artifacts a cross join generate_series(2, 9) n where a.issue_key = $1 and a.is_primary`, 8, 1},
		{"seven versions with markdown under an image", "", `
			select a.id, n, case when n < 9 then 'Draft ' || n || ' of the Tyrrhenian portolan.' end, case when n = 9 then 'image/png' end, '[]'
			from artifacts a cross join generate_series(2, 9) n where a.issue_key = $1 and a.is_primary`, 7, 1},
		{"a version below the latest the document holds", `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			select a.id, 3, 'Version three of the Tyrrhenian portolan.', '[]' from artifacts a where a.issue_key = $1 and a.is_primary`, `
			select a.id, 2, 'Version two.', null, '[]' from artifacts a where a.issue_key = $1 and a.is_primary`, 1, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			insert := func(t *testing.T, version int) (vector string, reVectors int) {
				t.Helper()
				store := openMigratedTestStore(t, version)
				seedCoreWithADefaultAssignee(t, store)
				key := seedIssueWithSpec(t, ctx, store, 1, "Tide tables for the equinox", "Nothing of note.\n")
				if test.setup != "" {
					if _, err := store.Pool.Exec(ctx, test.setup, key); err != nil {
						t.Fatalf("write the version the insert lands below: %v", err)
					}
				}
				var queued int
				count := `select count(*) from search_term_queue where kind = 'issue' and issue_key = $1`
				if err := store.Pool.QueryRow(ctx, count, key).Scan(&queued); err != nil {
					t.Fatalf("count the issue's queued re-vectors: %v", err)
				}
				if _, err := store.Pool.Exec(ctx,
					`insert into artifact_versions (artifact_id, number, markdown, mime, authors)`+test.versions, key); err != nil {
					t.Fatalf("insert the versions in one statement: %v", err)
				}
				var after int
				if err := store.Pool.QueryRow(ctx, count, key).Scan(&after); err != nil {
					t.Fatalf("count the issue's queued re-vectors: %v", err)
				}
				var current bool
				if err := store.Pool.QueryRow(ctx, `
					select search::text, search = issue_search_vector(key, title, issue_embedding_spec(key))
					from issues where key = $1`, key).Scan(&vector, &current); err != nil {
					t.Fatalf("read the issue's vector: %v", err)
				}
				if !current {
					t.Errorf("the issue's vector %s is not the one its latest version gives", vector)
				}
				return vector, after - queued
			}
			before, beforeCount := insert(t, issuesSearchLatest-1)
			after, afterCount := insert(t, latestVersion)
			if beforeCount != test.want {
				t.Errorf("before 20261124034909 the insert re-vectored the issue %d times, want %d, one for each version with markdown: the count this test reads does not count what it means to", beforeCount, test.want)
			}
			if afterCount != test.wantAfter {
				t.Errorf("the insert re-vectored the issue %d times, want %d", afterCount, test.wantAfter)
			}
			if after != before {
				t.Errorf("the issue's vector after the insert is\n%s\nwant the one the schema before 20261124034909 wrote,\n%s", after, before)
			}
		})
	}
}

// countEmbeddingsEnqueues makes every later call of embeddings_enqueue in store's database leave a
// row in embedding_enqueues and then do what it did before: the function the triggers call by name
// is renamed, and a function of its old name records the call and calls the renamed one. The table
// is an ordinary one of the test's own database, not a temporary one, which would belong to the
// one connection that made it, where the triggers run on whichever connection of the pool the
// insert takes.
func countEmbeddingsEnqueues(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	if _, err := store.Pool.Exec(ctx, `
		create table embedding_enqueues (kind text not null, id text not null);
		alter function embeddings_enqueue(text, text, text) rename to embeddings_enqueue_uncounted;
		create function embeddings_enqueue(p_kind text, p_id text, p_text text) returns void
		language plpgsql as $$
		begin
		  insert into embedding_enqueues (kind, id) values (p_kind, p_id);
		  perform embeddings_enqueue_uncounted(p_kind, p_id, p_text);
		end $$`); err != nil {
		t.Fatalf("count the calls of embeddings_enqueue: %v", err)
	}
}

// 20261128062654 makes one statement that inserts many versions of an issue's primary document
// enqueue the issue's embedding text once, where 0140's trigger enqueued it for each version, every
// time over the same latest version, and the census admits it to ship. The test opens two
// databases, one at the schema before the migration and one at the latest, and its subtests run in
// order on them, so the template of the schema before, which whichever test asks for it first
// builds, is paid in this setup and in no subtest. The two databases are the test's own and it
// touches nothing another test shares, so it runs in parallel with the package's other tests.
//
// The census comes first, on the schema before, where the migration is the one pending. It is one
// function's body and nothing else, so with the limit injected at one byte, above which every table
// of the schema lies, the census finds no table of it to size or to wait on, refuses nothing, and
// says the migration touches no existing table. It still reads the statements as create function
// alone, the kind that would exempt the migration from the limit had it named a table, so a
// statement added to it that reads rows shows here before a deploy meets it.
//
// Each insert case then writes an issue of its own in both databases, and counts the calls of
// embeddings_enqueue its insert makes for that issue (countEmbeddingsEnqueues). The issue's
// embeddings row ends with the text and the hash the schema before left, so the change is the
// count alone. 0140's trigger has no `when`, so in the second case the latest version, an image,
// fires it as the seven below it do: before, each of the eight enqueued the text the image leaves,
// the title alone; after, the image's firing alone does. The third inserts a version below one the
// document already holds: it enqueues nothing, where the old body enqueued the stored latest
// version's text again, which embeddings_enqueue skipped since its hash was unchanged; no writer
// inserts below the latest.
func TestTheEnqueueOnceMigrationPassesTheCensusAndEnqueuesAMultiVersionInsertOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	before := openMigratedTestStore(t, embeddingsEnqueueLatest-1)
	after := openMigratedTestStore(t, latestVersion)
	for _, store := range []*Store{before, after} {
		seedCoreWithADefaultAssignee(t, store)
		countEmbeddingsEnqueues(t, ctx, store)
	}

	t.Run("the census admits the migration at any table size", func(t *testing.T) {
		report, err := census(ctx, before.Pool.Config().ConnString(),
			migrationsThrough(t, embeddingsEnqueueLatest), pgmigrate.CensusOptions{TableLimit: 1})
		if err != nil {
			t.Fatalf("census: %v", err)
		}
		if report.Refused() {
			t.Errorf("the issue-enqueue body was refused: %v", refusals(report))
		}
		if len(report.Pending) != 1 || report.Pending[0].Migration.Version != embeddingsEnqueueLatest ||
			len(report.Pending[0].Tables) != 0 || report.Pending[0].TableSizeExemption != "create function" {
			t.Errorf("pending = %+v, want 20261128062654 alone, touching no table, its statements create function alone", report.Pending)
		}
		const name = "20261128062654_embeddings_enqueue_issue_spec_latest"
		if out := reportText(report); !strings.Contains(out, "census: "+name+".up.sql touches no existing table\n") ||
			!strings.Contains(out, "census: "+name+".up.sql census ("+name+".census.sql): 0\n") ||
			!strings.HasSuffix(out, "census: ok\n") {
			t.Errorf("report:\n%s", out)
		}
	})

	type embedding struct{ text, hash string }
	for i, test := range []struct {
		name, setup, versions string
		want, wantAfter       int
	}{
		{"eight versions with markdown", "", `
			select a.id, n, case when n = 9 then 'The rhumb lines of the Tyrrhenian portolan.' else 'Draft ' || n || '.' end, null, '[]'
			from artifacts a cross join generate_series(2, 9) n where a.issue_key = $1 and a.is_primary`, 8, 1},
		{"seven versions with markdown under an image", "", `
			select a.id, n, case when n < 9 then 'Draft ' || n || ' of the Tyrrhenian portolan.' end, case when n = 9 then 'image/png' end, '[]'
			from artifacts a cross join generate_series(2, 9) n where a.issue_key = $1 and a.is_primary`, 8, 1},
		{"a version below the latest the document holds", `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			select a.id, 3, 'Version three of the Tyrrhenian portolan.', '[]' from artifacts a where a.issue_key = $1 and a.is_primary`, `
			select a.id, 2, 'Version two.', null, '[]' from artifacts a where a.issue_key = $1 and a.is_primary`, 1, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			insert := func(t *testing.T, store *Store) (row embedding, enqueues int) {
				t.Helper()
				key := seedIssueWithSpec(t, ctx, store, i+1, "Tide tables for the equinox", "Nothing of note.\n")
				if test.setup != "" {
					if _, err := store.Pool.Exec(ctx, test.setup, key); err != nil {
						t.Fatalf("write the version the insert lands below: %v", err)
					}
				}
				// The seed and the setup call embeddings_enqueue too, so the count is the difference
				// across the insert.
				count := `select count(*) from embedding_enqueues where kind = 'issue' and id = $1`
				var queued int
				if err := store.Pool.QueryRow(ctx, count, key).Scan(&queued); err != nil {
					t.Fatalf("count the issue's enqueues: %v", err)
				}
				if _, err := store.Pool.Exec(ctx,
					`insert into artifact_versions (artifact_id, number, markdown, mime, authors)`+test.versions, key); err != nil {
					t.Fatalf("insert the versions in one statement: %v", err)
				}
				if err := store.Pool.QueryRow(ctx, count, key).Scan(&enqueues); err != nil {
					t.Fatalf("count the issue's enqueues: %v", err)
				}
				var current bool
				if err := store.Pool.QueryRow(ctx, `
					select e.text_snapshot, e.content_hash, e.text_snapshot = issue_embedding_text(i.title, issue_embedding_spec(i.key))
					from embeddings e join issues i on i.key = e.id
					where e.kind = 'issue' and e.id = $1`, key).Scan(&row.text, &row.hash, &current); err != nil {
					t.Fatalf("read the issue's embeddings row: %v", err)
				}
				if !current {
					t.Errorf("the issue's embedding text %q is not the one its latest version gives", row.text)
				}
				return row, enqueues - queued
			}
			beforeRow, beforeCount := insert(t, before)
			afterRow, afterCount := insert(t, after)
			if beforeCount != test.want {
				t.Errorf("before 20261128062654 the insert enqueued the issue %d times, want %d, one for each version: the count this test reads does not count what it means to", beforeCount, test.want)
			}
			if afterCount != test.wantAfter {
				t.Errorf("the insert enqueued the issue %d times, want %d", afterCount, test.wantAfter)
			}
			if afterRow != beforeRow {
				t.Errorf("the issue's embeddings row after the insert is\n%+v\nwant the one the schema before 20261128062654 left,\n%+v", afterRow, beforeRow)
			}
		})
	}
}

// 20261105145243 is a function and a trigger and nothing else, which is what lets it ship: the
// pre-deploy census refuses a migration that locks a table above its size limit, and the trigger's
// table, artifact_versions, is 5.2 GiB in production. Neither statement reads a row, so the time
// the migration holds its lock does not grow with the table and the limit does not apply to it
// (pgmigrate tableSizeExemption). This is why the re-index of the rows already stored is
// 20261105145242's and not this migration's: an update is not such a statement, and one in this
// file would cost the exemption and refuse every deploy carrying it. The limit is injected at one
// byte so the test needs no gigabyte.
func TestCensusExemptsTheIssuesSearchTriggerFromTheTableLimit(t *testing.T) {
	t.Parallel()
	store := openMigratedTestStore(t, issuesSearchSpec)
	report, err := census(context.Background(), store.Pool.Config().ConnString(),
		migrationsThrough(t, issuesSearchSpecTrig), pgmigrate.CensusOptions{TableLimit: 1})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if report.Refused() {
		t.Errorf("the issues-search trigger was refused: %v", refusals(report))
	}
	if len(report.Pending) != 1 || report.Pending[0].TableSizeExemption != "create function and create trigger" {
		t.Errorf("pending = %+v, want 20261105145243 alone, exempt as a function and a trigger", report.Pending)
	}
	if out := reportText(report); !strings.Contains(out, "touches artifact_versions: ") ||
		!strings.Contains(out, "from which the migration is exempt because its statements are only create function and create trigger") {
		t.Errorf("report:\n%s", out)
	}
}

// What each of the migrations locks above ACCESS SHARE, read as the census reads it. The
// re-index names issues alone: it reads each issue's latest primary version through 0140's
// issue_embedding_spec, under ACCESS SHARE, which the census leaves out of a migration's tables on
// purpose, so no deploy is refused over the size of artifact_versions. 20261124034909 replaces the
// trigger's function body and 20261128062654 replaces 0140's, and neither names a table.
func TestTheIssuesSearchSpecMigrationsTouchOnlyWhatTheyMustLock(t *testing.T) {
	t.Parallel()
	for _, migration := range []struct {
		name string
		file string
		want []string
	}{
		{"20261105145242, the expression and the re-index", "20261105145242_issues_search_spec.up.sql", []string{"issues"}},
		{"20261105145243, the function and the trigger", "20261105145243_artifact_versions_issues_search_trigger.up.sql", []string{"artifact_versions"}},
		{"20261124034909, the function alone", "20261124034909_artifact_versions_issues_search_latest.up.sql", nil},
		{"20261128062654, the enqueue function alone", "20261128062654_embeddings_enqueue_issue_spec_latest.up.sql", nil},
	} {
		t.Run(migration.name, func(t *testing.T) {
			sql, err := migrationFiles.ReadFile("migrations/" + migration.file)
			if err != nil {
				t.Fatalf("read %s: %v", migration.file, err)
			}
			if touched := pgmigrate.TouchedTables(string(sql)); !slices.Equal(touched, migration.want) {
				t.Fatalf("%s touches %v, want %v", migration.file, touched, migration.want)
			}
		})
	}
}
