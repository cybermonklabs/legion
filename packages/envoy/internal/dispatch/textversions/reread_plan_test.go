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
// predicate in its migration is that list, word for word. On a log of 30,000 events of other
// types and 200 of those, analyzed, the generic plan a cached statement settles on, sequential
// scans left on, reads events through that index alone.
func TestTheReReadsLogReadUsesItsIndex(t *testing.T) {
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
		  from generate_series(1, 50) n`, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		select $1 || '-' || ((g - 1) % 50 + 1), (g - 1) / 50 + 1,
		       case when g % 151 = 0 then (array['comment.deleted', 'comment.edited', 'message.deleted', 'message.edited'])[g % 4 + 1]
		            else (array['issue.claimed', 'comment.resolved', 'message.delivery', 'comment.created'])[g % 4 + 1] end,
		       '{"kind":"user","id":"alice"}', jsonb_build_object('id', md5(g::text)::uuid::text, 'body', 'A note.'), false
		  from generate_series(1, 30200) g`,
	} {
		if _, err := database.Pool.Exec(ctx, statement, textversionstest.Project); err != nil {
			t.Fatalf("seed the log: %v", err)
		}
	}
	if _, err := database.Pool.Exec(ctx, `analyze events`); err != nil {
		t.Fatalf("analyze the log: %v", err)
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
