package graphcensus

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The seed of TestTheBlockBatchAnswersTheSameRowsWithItsLookupsMaterializedOrNot: materializedIssues
// issues, each with one document, and materializedProjectDocs documents of the project itself,
// each at materializedVersions versions with materializedBlocksPerVersion block rows naming no
// node changed at each version. Version 1 of each document has its artifact.version event, version
// 2 two of them (the lookup answers the newer), and version 3 none, beside a decoy: an
// artifact.version event naming the document and that version, written to another issue's log,
// which the lookup's owner clause leaves out.
const (
	materializedIssues           = 6
	materializedProjectDocs      = 3
	materializedVersions         = 3
	materializedBlocksPerVersion = 3
)

// TestTheBlockBatchAnswersTheSameRowsWithItsLookupsMaterializedOrNot holds BlockRowsStatement's
// `caused as materialized (` to changing its plan and not its rows: the word is one of the marking
// statements' (docs' markingStatementDigests), and refs.BlockIndexMarkingRule stays at the rule it
// was because the rows the backfill selects, and so the documents it marks, are the same with it
// or without it. The seed (the constants above) is walked whole from the empty address, as the
// backfill walks it, in batches of 7, which splits a document's version across batches, and of
// backfillBatch, under each plan-cache mode, by the statement as it stands and by the same
// statement with the word taken out: every batch of the two must answer the same rows, in the same
// order, and each walk must answer every seeded block once, a third of them with the sentinel. The
// control is the statement with its lookup reversed to the oldest event of a version, which
// answers other rows for version 2: the comparison must see it differ, so it can tell two
// statements that answer differently apart.
func TestTheBlockBatchAnswersTheSameRowsWithItsLookupsMaterializedOrNot(t *testing.T) {
	database := storetest.Open(t)
	pool := database.Pool
	seedMaterializedWalk(t, pool)
	inlined := strings.Replace(BlockRowsStatement, "caused as materialized (", "caused as (", 1)
	if inlined == BlockRowsStatement {
		t.Fatalf("BlockRowsStatement holds no `caused as materialized (`, so this test compares it with itself")
	}
	reversed := strings.Replace(BlockRowsStatement, "order by e.id desc limit 1", "order by e.id limit 1", 1)
	if reversed == BlockRowsStatement {
		t.Fatalf("BlockRowsStatement holds no `order by e.id desc limit 1`, so the control is the statement itself")
	}
	blocks := (materializedIssues + materializedProjectDocs) * materializedVersions * materializedBlocksPerVersion
	for _, mode := range explaintest.PlanCacheModes {
		for _, batch := range []int{7, backfillBatch} {
			materialized := walkBlockRows(t, pool, BlockRowsStatement, mode, batch)
			if len(materialized) != blocks {
				t.Fatalf("under %s in batches of %d the statement answered %d rows, want every one of the %d seeded blocks", mode, batch, len(materialized), blocks)
			}
			sentinels := 0
			for _, row := range materialized {
				if strings.HasSuffix(row, fmt.Sprintf(" event=%d", graphstore.NoEvent)) {
					sentinels++
				}
			}
			if sentinels != blocks/materializedVersions {
				t.Errorf("under %s in batches of %d the statement answered the sentinel for %d rows, want %d, version 3's", mode, batch, sentinels, blocks/materializedVersions)
			}
			if got := walkBlockRows(t, pool, inlined, mode, batch); !slices.Equal(got, materialized) {
				t.Errorf("under %s in batches of %d the statement without `materialized` answers other rows:\n%s", mode, batch, firstDifference(materialized, got))
			} else {
				t.Logf("under %s in batches of %d both answer the same %d rows", mode, batch, len(got))
			}
			if got := walkBlockRows(t, pool, reversed, mode, batch); slices.Equal(got, materialized) {
				t.Errorf("under %s in batches of %d the control, the lookup reversed, answers the same rows, so this comparison cannot tell two statements that answer differently apart", mode, batch)
			} else {
				t.Logf("under %s in batches of %d the control answers other rows, as it must:\n%s", mode, batch, firstDifference(materialized, got))
			}
		}
	}
}

// walkBlockRows walks the block rows naming no node with statement, BlockRowsStatement or a
// variant of it, from the empty address in batches of batch, as readBlockRows reads them, in one
// transaction under plan_cache_mode mode, and answers each row it read as text, batch by batch.
// pgx prepares the statement, so force_generic_plan plans it once for every batch.
func walkBlockRows(t *testing.T, pool *store.Pool, statement, mode string, batch int) []string {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the walk: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local plan_cache_mode = "+mode); err != nil {
		t.Fatalf("set plan_cache_mode = %s: %v", mode, err)
	}
	var walked []string
	for after := ""; ; {
		rows, err := tx.Query(ctx, statement, after, batch, graphstore.NoEvent)
		if err != nil {
			t.Fatalf("read a batch after %q: %v", after, err)
		}
		read := 0
		for rows.Next() {
			// The statement also answers each block's project and conversation, the scope its node
			// carries (readBlockRows reads them too); both lookups must answer those alike as well.
			var artifact, block, address, refKey, project, session string
			var created time.Time
			var event int64
			if err := rows.Scan(&artifact, &block, &address, &refKey, &created, &event, &project, &session); err != nil {
				t.Fatalf("read a row after %q: %v", after, err)
			}
			// The event stays last: the sentinel count below reads it as the row's suffix.
			walked = append(walked, fmt.Sprintf("%s %s %s %s %s project=%s session=%s event=%d", artifact, block, address, refKey,
				created.UTC().Format(time.RFC3339Nano), project, session, event))
			after = address
			read++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read a batch after %q: %v", after, err)
		}
		if read < batch {
			return walked
		}
	}
}

// firstDifference describes where two walks first part.
func firstDifference(want, got []string) string {
	for i := range min(len(want), len(got)) {
		if want[i] != got[i] {
			return fmt.Sprintf("  row %d: %q, against %q", i, got[i], want[i])
		}
	}
	return fmt.Sprintf("  %d rows against %d", len(got), len(want))
}

// seedMaterializedWalk writes the seed TestTheBlockBatchAnswersTheSameRowsWithItsLookupsMaterializedOrNot
// reads (the constants above say what it holds) and analyzes the tables its statement plans over.
func seedMaterializedWalk(t *testing.T, pool *store.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, step := range []struct {
		what, sql string
		args      []any
	}{
		{"seed project BMT", `insert into projects (key, name, default_assignee) values ('BMT', 'Backfill materialized', 'alice')`, nil},
		{"seed the issues", `
			insert into issues (key, project_key, number, title, created_by, rank)
			select 'BMT-' || i, 'BMT', i, 'Issue ' || i, '{"kind":"user","id":"alice"}', 'U'
			from generate_series(1, $1::int) i`, []any{materializedIssues}},
		{"seed each issue's document", `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			select key, 'BMT', 'spec', 'spec.md', 'doc', false, '{"kind":"user","id":"alice"}'
			from issues where project_key = 'BMT'`, nil},
		{"seed the project's own documents", `
			insert into artifacts (project_key, slug, name, kind, created_by)
			select 'BMT', 'page-' || p, 'page-' || p, 'doc', '{"kind":"user","id":"alice"}'
			from generate_series(1, $1::int) p`, []any{materializedProjectDocs}},
		{"write each document's versions", `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			select a.id, v, 'Version ' || v || '.', '[{"kind":"user","id":"alice"}]'
			from artifacts a cross join generate_series(1, $1::int) v
			where a.project_key = 'BMT'`, []any{materializedVersions}},
		{"write the block rows naming no node", `
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
			select a.id, 'b' || v || '-' || k, a.ref_key || '#b' || v || '-' || k, 'paragraph', '', 'Block ' || v || '-' || k, v, now()
			from artifacts a cross join generate_series(1, $1::int) v cross join generate_series(1, $2::int) k
			where a.project_key = 'BMT'`, []any{materializedVersions, materializedBlocksPerVersion}},
		// Version 1 one event, version 2 two, version 3 none, each in the log of the document's
		// owner: its issue's, or the document's own where it belongs to the project alone.
		{"record versions 1 and 2's artifact.version events", `
			insert into events (issue_key, artifact_id, seq, type, actor, payload, notify)
			select a.issue_key, case when a.issue_key is null then a.id end, recorded.seq, 'artifact.version',
			       '{"kind":"user","id":"alice"}',
			       jsonb_build_object('artifact_id', a.id::text, 'version', jsonb_build_object('number', recorded.version)), false
			from artifacts a cross join (values (1, 10), (2, 20), (2, 21)) recorded (version, seq)
			where a.project_key = 'BMT'
			order by recorded.seq, a.id`, nil},
		{"record a decoy of each issue document's version 3 in the next issue's log", `
			insert into events (issue_key, seq, type, actor, payload, notify)
			select 'BMT-' || (1 + i.number % $1::int), 30, 'artifact.version', '{"kind":"user","id":"alice"}',
			       jsonb_build_object('artifact_id', a.id::text, 'version', jsonb_build_object('number', 3)), false
			from artifacts a join issues i on i.key = a.issue_key
			where a.project_key = 'BMT'`, []any{materializedIssues}},
		{"analyze the seeded tables", `analyze events, document_blocks, artifacts, issues`, nil},
	} {
		if _, err := pool.Exec(ctx, step.sql, step.args...); err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
	}
}
