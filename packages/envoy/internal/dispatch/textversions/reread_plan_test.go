package textversions

import (
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// rereadIndexMigration is the migration that builds events_post_text_changes, the index the
// re-read's read of the log goes through.
const rereadIndexMigration = "../store/migrations/20261121160853_events_post_text_changes.up.sql"

// TestTheReReadsLogReadUsesItsIndex holds the re-read's read of the log (rereadEventsQuery) to
// the partial index built for it, events_post_text_changes. Its type list (rereadLogTypes) is the
// types a re-read reads for every rules version below this build's (changeTypes), and the index's
// predicate in its migration is that list, word for word. On a log of four events of each of
// eight types, four of them the re-read's, with the statistics of a log of 30,000 events of the
// other four types and 200 of the re-read's stated (rereadLogStatistics), the generic plan a
// cached statement settles on, sequential scans left on, reads events through that index alone.
func TestTheReReadsLogReadUsesItsIndex(t *testing.T) {
	t.Parallel()
	for stored := range RulesVersion {
		types := currentRules.changeTypes(rereadTypes(stored, RulesVersion))
		if want := "'" + strings.Join(types, "', '") + "'"; rereadLogTypes != want {
			t.Errorf("rereadLogTypes is %s; a re-read after rules version %d reads %s", rereadLogTypes, stored, want)
		}
	}
	migration, err := os.ReadFile(rereadIndexMigration)
	if err != nil {
		t.Fatalf("read the index's migration: %v", err)
	}
	if predicate := "where type in (" + rereadLogTypes + ");"; !strings.Contains(string(migration), predicate) {
		t.Errorf("%s does not hold the predicate %q, the re-read's type list", rereadIndexMigration, predicate)
	}

	database := storetest.Open(t)
	textversionstest.SeedProject(t, database)
	ctx := t.Context()
	for _, statement := range []string{`
		insert into issues (key, project_key, number, title, created_by, rank)
		select $1 || '-' || n, $1, n, 'Rotate the signing keys of region ' || n, '{"kind":"user","id":"alice"}', 'A'
		  from generate_series(1, 4) n`, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		select $1 || '-' || ((g - 1) % 4 + 1), (g - 1) / 4 + 1,
		       (array['comment.deleted', 'comment.edited', 'message.deleted', 'message.edited',
		              'issue.claimed', 'comment.resolved', 'message.delivery', 'comment.created'])[(g - 1) % 8 + 1],
		       '{"kind":"user","id":"alice"}', jsonb_build_object('id', md5(g::text)::uuid::text, 'body', 'A note.'), false
		  from generate_series(1, 32) g`,
	} {
		if _, err := database.Pool.Exec(ctx, statement, textversionstest.Project); err != nil {
			t.Fatalf("seed the log: %v", err)
		}
	}
	for _, statement := range rereadLogStatistics {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("state the log's statistics (%s): %v", strings.TrimSpace(statement), err)
		}
	}
	connection, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plan: %v", err)
	}
	defer connection.Release()
	plan := explaintest.GenericPlan(t, ctx, connection, "reread_log", rereadEventsQuery,
		[]string{"bigint", "bigint", "bigint", "bigint", "text[]", "text[]"},
		[]string{"0", "9223372036854775807", "500", "2097152", "array[" + rereadLogTypes + "]", "null"})
	var events int
	for _, scan := range explaintest.Scans(t, plan) {
		if scan.Relation != "events" {
			continue
		}
		events++
		if scan.Index != "events_post_text_changes" {
			t.Errorf("the re-read's read of the log scans events by %q, not through events_post_text_changes:\n%s", scan.Index, plan)
		}
	}
	if events == 0 {
		t.Fatalf("the re-read's read of the log scans no events:\n%s", plan)
	}
}

// rereadLogStatistics states the statistics the plan of TestTheReReadsLogReadUsesItsIndex rests
// on, those ANALYZE leaves on a log of 30,200 events, 200 of them of the re-read's four types, 50
// of each, and 7,500 of each of four other types. The planner reads a relation's rows as its
// reltuples over its relpages times the pages it holds on disk
// (table_block_relation_estimate_size), so each relation's relpages is stated as the pages the
// test's few events take, and its reltuples as the log's: 30,200 rows of events, and 200 of
// events_post_text_changes. The most common values of events.type are the eight types, at those
// frequencies (7,500 and 50 of 30,200), as ANALYZE keeps every value of a column of so few.
var rereadLogStatistics = []string{`
	select pg_restore_relation_stats('schemaname', 'public', 'relname', 'events',
	       'relpages', greatest(pg_relation_size('public.events') / current_setting('block_size')::bigint, 1)::integer,
	       'reltuples', 30200::real, 'relallvisible', 0::integer)`, `
	select pg_restore_relation_stats('schemaname', 'public', 'relname', 'events_post_text_changes',
	       'relpages', greatest(pg_relation_size('public.events_post_text_changes') / current_setting('block_size')::bigint, 2)::integer,
	       'reltuples', 200::real, 'relallvisible', 0::integer)`, `
	select pg_restore_attribute_stats('schemaname', 'public', 'relname', 'events', 'attname', 'type', 'inherited', false,
	       'null_frac', 0::real, 'avg_width', 15::integer, 'n_distinct', 8::real,
	       'most_common_vals', '{issue.claimed,comment.resolved,message.delivery,comment.created,comment.deleted,comment.edited,message.deleted,message.edited}'::text,
	       'most_common_freqs', '{0.24834,0.24834,0.24834,0.24834,0.0016556,0.0016556,0.0016556,0.0016556}'::real[])`,
}
