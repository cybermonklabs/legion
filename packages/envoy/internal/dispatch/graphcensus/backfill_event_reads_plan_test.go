package graphcensus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The seed of TestTheBackfillsEventReadsProbeTheirIndexes: eventPlanIssues issues, each with one
// document at eventPlanVersions versions and eventPlanBlocksPerVersion block rows naming no node
// changed at each version, and the artifact.version event of each version; eventPlanComments
// comments naming no node, each with the comment.created event that stored it; and
// eventPlanFiller events of another type. The blocks and the comments each fit in one batch
// (backfillBatch), so the first batch of each read reaches every one of them, and the filler makes a
// read of events whole cost the planner more than a probe of an index's key, as it does on any
// deployment's events.
const (
	eventPlanIssues           = 20
	eventPlanVersions         = 3
	eventPlanBlocksPerVersion = 2
	eventPlanComments         = 50
	eventPlanFiller           = 5000
)

// eventPlanRead is one of the backfill's reads of events, the index it must probe for each event it
// reads, and how many times the plan must run that probe for the seed: once for each event the
// first batch resolves.
type eventPlanRead struct {
	name, index string
	statement   explaintest.Statement
	probes      float64
}

// TestTheBackfillsEventReadsProbeTheirIndexes holds the two reads of events the backfill makes for
// each batch to a probe of an index of their own: the block batch's read of the artifact.version
// event of each (document, version) of its rows (BlockRowsStatement, through
// events_artifact_version_number, migration 20261119020700), and the comment batch's read of the
// comment.created event of each of its comments (CommentRowsStatement, through
// events_comment_created_id, migration 20261119020800). Without them each such read is a read of
// events whole, once per document version or comment of the run, which took the block pass 43.9
// minutes over a seeded database of 270,529 blocks and 64,460 events, against 20.7 seconds over a
// tenth of that.
//
// The seed is read first, and each read must answer every block and comment it seeded with the
// event that recorded it, so the plans below are of reads that find their events. Then, with hash
// and merge joins off and under each plan-cache mode, every read of events in the analyzed plan
// must be a probe of the read's index by its first key, run once for each event the batch resolves
// (relationReads). The control is the same read with that index dropped inside the explaining
// transaction, as the database stood before its migration: it must plan, run, and fail the check
// under both modes with a read of events that ran. The block batch is explained from the empty
// address, as the backfill starts it, which explaintest once wrote as null, an empty batch whose
// events read never ran: the count of probes is what tells the two apart.
func TestTheBackfillsEventReadsProbeTheirIndexes(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	pool := database.Pool
	seedEventPlan(t, pool)

	wantBlockEvents := eventsByKey(t, pool, `
		select d.artifact_id::text || ' ' || d.block_id, e.id
		from document_blocks d
		join events e on e.type = 'artifact.version' and e.payload->>'artifact_id' = d.artifact_id::text
			and e.payload->'version'->>'number' = d.changed_version::text`)
	blocks, err := readBlockRows(ctx, database, "")
	if err != nil {
		t.Fatalf("read the first block batch: %v", err)
	}
	gotBlockEvents := map[string]int64{}
	for _, row := range blocks {
		gotBlockEvents[row.artifactID+" "+row.blockID] = row.event
	}
	requireSameEvents(t, "the block batch", gotBlockEvents, wantBlockEvents, eventPlanIssues*eventPlanVersions*eventPlanBlocksPerVersion)

	wantCommentEvents := eventsByKey(t, pool, `select payload->>'id', id from events where type = 'comment.created'`)
	commentIDs, causes, _, err := readCommentRows(ctx, database, "00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("read the first comment batch: %v", err)
	}
	gotCommentEvents := map[string]int64{}
	for i, id := range commentIDs {
		gotCommentEvents[id] = causes[i]
	}
	requireSameEvents(t, "the comment batch", gotCommentEvents, wantCommentEvents, eventPlanComments)

	keys := explaintest.NewIndexKeys(pool)
	for _, read := range []eventPlanRead{{
		name:      "the block batch's read of each version's artifact.version event",
		index:     "events_artifact_version_number",
		statement: explaintest.Statement{SQL: BlockRowsStatement, Args: []any{"", backfillBatch, graphstore.NoEvent}},
		probes:    eventPlanIssues * eventPlanVersions,
	}, {
		name:      "the comment batch's read of each comment's comment.created event",
		index:     "events_comment_created_id",
		statement: explaintest.Statement{SQL: CommentRowsStatement, Args: []any{"00000000-0000-0000-0000-000000000000", backfillBatch, graphstore.NoEvent}},
		probes:    eventPlanComments,
	}} {
		t.Run(read.index, func(t *testing.T) {
			dropped := []explaintest.Statement{{SQL: "set local lock_timeout = '10s'"}, {SQL: "drop index " + read.index}}
			for _, mode := range explaintest.PlanCacheModes {
				plan := explaintest.ExplainAfter(t, ctx, pool, nil, read.statement, mode, "json")
				reads := relationReads(t, keys, plan, "events", read.index)
				switch {
				case len(reads.unprobed) > 0:
					t.Errorf("%s under %s reads events other than by a probe of %s:\n  %s\nplan:\n%s", read.name, mode, read.index,
						strings.Join(reads.unprobed, "\n  "), explaintest.ExplainAfter(t, ctx, pool, nil, read.statement, mode, "text"))
				case reads.probeLoops != read.probes:
					t.Errorf("%s under %s probes %s %.0f time(s), want %.0f, once for each event its batch resolves:\n  %s\nplan:\n%s",
						read.name, mode, read.index, reads.probeLoops, read.probes, strings.Join(reads.probed, "\n  "),
						explaintest.ExplainAfter(t, ctx, pool, nil, read.statement, mode, "text"))
				default:
					t.Logf("%s under %s:\n  %s", read.name, mode, strings.Join(reads.probed, "\n  "))
				}

				control := relationReads(t, keys, explaintest.ExplainAfter(t, ctx, pool, dropped, read.statement, mode, "json"), "events", read.index)
				if len(control.unprobed) == 0 || control.unprobedLoops == 0 {
					t.Errorf("%s with %s dropped, the control, reads events by no other way that ran under %s, so the check cannot tell a read of events whole from a probe:\n%s",
						read.name, read.index, mode, explaintest.ExplainAfter(t, ctx, pool, dropped, read.statement, mode, "text"))
				} else {
					t.Logf("%s with %s dropped under %s fails the check, as the control must:\n  %s", read.name, read.index, mode, strings.Join(control.unprobed, "\n  "))
				}
			}
		})
	}
}

// planReads is what relationReads found of one relation's reads in a plan: each probe and each
// other read, as a failure or a log names it; how many times the probes and the other reads ran
// in all; and how many rows the reads answered in all, over every time each ran.
type planReads struct {
	probed, unprobed          []string
	probeLoops, unprobedLoops float64
	rows                      float64
}

// relationReads judges every node of plan, one "Plan" node of EXPLAIN (ANALYZE, FORMAT JSON),
// that reads relation. A probe is an index scan of index, or a bitmap heap scan whose bitmap is one
// Bitmap Index Scan of index, whose condition names that index's first key column
// (explaintest.CondNamesFirstKey); anything else - a sequential scan, a scan of another index, an
// index read end to end - reads relation other than by the key the statement asks for.
func relationReads(t *testing.T, keys *explaintest.IndexKeys, plan json.RawMessage, relation, index string) planReads {
	t.Helper()
	var reads planReads
	bitmaps := explaintest.BitmapHeapScans(t, plan, relation)
	for _, node := range explaintest.Nodes(t, plan) {
		if node.Relation != relation {
			continue
		}
		read := fmt.Sprintf("%s ran %.0f time(s), %.0f row(s) each", node, node.Loops, node.Rows)
		through, cond := node.Index, node.IndexCond
		probe := node.Type == "Index Scan" || node.Type == "Index Only Scan"
		if node.Type == "Bitmap Heap Scan" {
			scan := bitmaps[0]
			bitmaps = bitmaps[1:]
			if probe = scan.Bitmap.Type == "Bitmap Index Scan" && len(scan.BitmapIndexScans) == 1; probe {
				through, cond = scan.BitmapIndexScans[0].Index, scan.BitmapIndexScans[0].IndexCond
				read += " through " + through
			}
		}
		reads.rows += node.Rows * node.Loops
		if !probe || through != index || cond == "" || !explaintest.CondNamesFirstKey(cond, keys.FirstKey(t, index)) {
			reads.unprobed = append(reads.unprobed, read+": "+relation+" is read other than by a probe of "+index+"'s key")
			reads.unprobedLoops += node.Loops
			continue
		}
		reads.probed = append(reads.probed, read+" under "+cond)
		reads.probeLoops += node.Loops
	}
	return reads
}

// seedEventPlan writes the seed TestTheBackfillsEventReadsProbeTheirIndexes reads (the constants
// above say what it holds) and analyzes the tables its reads plan over.
func seedEventPlan(t *testing.T, pool *store.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('BEV', 'Backfill events', 'alice')
	`); err != nil {
		t.Fatalf("seed project BEV: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		select 'BEV-' || i, 'BEV', i, 'Issue ' || i, '{"kind":"user","id":"alice"}', 'U'
		from generate_series(1, $1::int) i
	`, eventPlanIssues); err != nil {
		t.Fatalf("seed the issues: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		select key, 'BEV', 'spec', 'spec.md', 'doc', false, '{"kind":"user","id":"alice"}'
		from issues where project_key = 'BEV'
	`); err != nil {
		t.Fatalf("seed each issue's document: %v", err)
	}
	for version := 1; version <= eventPlanVersions; version++ {
		if _, err := pool.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			select id, $1::int, 'Version ' || $1::int || '.', '[{"kind":"user","id":"alice"}]'
			from artifacts where project_key = 'BEV'
		`, version); err != nil {
			t.Fatalf("write version %d of each document: %v", version, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		select a.id, 'b' || v || '-' || k, a.ref_key || '#b' || v || '-' || k, 'paragraph', '', 'Block ' || v || '-' || k, v, now()
		from artifacts a cross join generate_series(1, $1::int) v cross join generate_series(1, $2::int) k
		where a.project_key = 'BEV'
	`, eventPlanVersions, eventPlanBlocksPerVersion); err != nil {
		t.Fatalf("write the block rows naming no node: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		select a.issue_key, v, 'artifact.version', '{"kind":"user","id":"alice"}',
		       jsonb_build_object('artifact_id', a.id::text, 'version', jsonb_build_object('number', v)), false
		from artifacts a cross join generate_series(1, $1::int) v
		where a.project_key = 'BEV'
	`, eventPlanVersions); err != nil {
		t.Fatalf("record each version's artifact.version event: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into comments (issue_key, author, body)
		select 'BEV-' || (1 + i % $1::int), '{"kind":"user","id":"alice"}', 'Comment ' || i
		from generate_series(1, $2::int) i
	`, eventPlanIssues, eventPlanComments); err != nil {
		t.Fatalf("write the comments naming no node: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		select c.issue_key, 100 + row_number() over (partition by c.issue_key order by c.id), 'comment.created',
		       '{"kind":"user","id":"alice"}', jsonb_build_object('id', c.id::text), false
		from comments c where c.issue_key like 'BEV-%'
	`); err != nil {
		t.Fatalf("record each comment's comment.created event: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		select 'BEV-' || (1 + i % $2::int), 1000 + i, 'issue.updated', '{"kind":"user","id":"alice"}', jsonb_build_object('n', i), false
		from generate_series(1, $1::int) i
	`, eventPlanFiller, eventPlanIssues); err != nil {
		t.Fatalf("record the filler events: %v", err)
	}
	if _, err := pool.Exec(ctx, `analyze events, document_blocks, artifacts, comments, issues`); err != nil {
		t.Fatalf("analyze the seeded tables: %v", err)
	}
}

// eventsByKey reads query, a select of a key and an event id, into a map.
func eventsByKey(t *testing.T, pool *store.Pool, query string) map[string]int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("read the seeded events: %v", err)
	}
	defer rows.Close()
	events := map[string]int64{}
	for rows.Next() {
		var key string
		var id int64
		if err := rows.Scan(&key, &id); err != nil {
			t.Fatalf("read a seeded event: %v", err)
		}
		events[key] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the seeded events: %v", err)
	}
	return events
}

// requireSameEvents fails unless got, what batch answered for each row, holds exactly want, the
// event the seed recorded for each, and want holds the count rows the seed wrote: a batch that
// answered the sentinel, or a seed that wrote no event, would leave the plans below about reads
// that find nothing.
func requireSameEvents(t *testing.T, batch string, got, want map[string]int64, count int) {
	t.Helper()
	if len(want) != count {
		t.Fatalf("the seed recorded an event for %d rows of %s, want %d", len(want), batch, count)
	}
	if len(got) != len(want) {
		t.Errorf("%s answered %d rows, want %d", batch, len(got), len(want))
	}
	for key, event := range want {
		if got[key] != event {
			t.Errorf("%s answered event %d for %s, want %d, the event that recorded it", batch, got[key], key, event)
		}
	}
}
