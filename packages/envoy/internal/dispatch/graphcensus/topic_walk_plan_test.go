package graphcensus

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// topicWalkRows is how many rows the seed of TestEveryTopicPassWalksOneBatchThroughAnIndex writes
// for each walk, every one owed by its pass: a hundred batches of each (backfillBatch), enough that
// reading every row a walk could owe, and sorting it, costs the planner more than reading one batch
// through an index in the walk's order, as it does on any deployment's tables.
const topicWalkRows = 20000

// topicWalk is the walk of one topic pass (backfillPasses) over a table: the batch's statement
// read from the middle of the seed, the relation and the index each batch must read from its
// cursor on, the statement that drops that index for the control, and how many rows of the table
// past one batch the walk's key reads (slack): none where the index keys each row the walk answers.
type topicWalk struct {
	name, table, index, drop string
	slack                    float64
	statement                explaintest.Statement
}

// TestEveryTopicPassWalksOneBatchThroughAnIndex holds each walk of a topic pass that comes after
// the projected tables' passes (backfillPasses) to the rule TestTheBackfillsWalksReadOnlyTheirBatch
// holds the block and the block-reference walks to: each batch reads one batch of its table through
// an index, from the last batch's key on, so a run takes time growing with the rows and not with
// their square. The walks are those of the issue links (issueLinkRowsStatement), the components,
// their dependencies and an issue's components (componentRowsStatement,
// componentDependsRowsStatement, issueComponentMemberRowsStatement), the check results and their
// retractions (missingResultKeysStatement, owedRetractionKeysStatement), the versions
// (missingVersionKeysStatement), a document's references, its rows owing their tuple and its
// tuples owing their retraction (owedReferenceRowsStatement, orphanedReferenceTuplesStatement), the
// comments' anchors (owedAnchorRowsStatement) and the issues the head fill walks
// (unmarkedIssuesAfterStatement). Each reads an index its table's own migration built in its
// transaction, a primary or unique key or artifacts_id_text, which every store holds, so none is
// one of BackfillIndexes, the indexes a migration builds concurrently and the run refuses to start
// without.
//
// The seed (seedTopicWalks) writes topicWalkRows rows owed to each walk and analyzes the tables,
// leaving node_kind of the tables a release added it to after their analysis (topicWalkedColumns)
// with no statistics, as the deploy leaves them; the run's own analysis (analyzeWalkedColumns),
// which the backfill runs before its passes, must then give each of them its statistics. Each
// batch is read from the key of the row in the middle of the seed, as a run reads a batch halfway
// through: it must answer a whole batch. Then, with hash and merge joins off and under each
// plan-cache mode, every read of the walk's table in the analyzed plan must be a probe of the
// walk's index by its first key, and the reads must answer no more than one batch of rows
// (relationReads). The control is the same batch with that index dropped inside the explaining
// transaction: it must plan, run, and read more than a batch of the table, other than through it.
func TestEveryTopicPassWalksOneBatchThroughAnIndex(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	pool := database.Pool
	seedTopicWalks(t, pool)
	if before := nodeKindStatistics(t, pool); before != 0 {
		t.Fatalf("node_kind has statistics on %d of %v before the run's analysis, so this test cannot hold the walks to that analysis", before, topicWalkedColumns)
	}
	if err := analyzeWalkedColumns(ctx, database); err != nil {
		t.Fatalf("the run's analysis: %v", err)
	}
	if after := nodeKindStatistics(t, pool); after != len(topicWalkedColumns) {
		t.Fatalf("node_kind has statistics on %d of %v after the run's analysis, want every one", after, topicWalkedColumns)
	}
	keys := explaintest.NewIndexKeys(pool)
	for _, walk := range topicWalks(t, pool) {
		t.Run(walk.name, func(t *testing.T) {
			if answered := countRows(t, pool, walk.statement); answered != backfillBatch {
				t.Fatalf("%s from the middle of the seed answered %d rows, want a whole batch, %d", walk.name, answered, backfillBatch)
			}
			dropped := []explaintest.Statement{{SQL: "set local lock_timeout = '10s'"}, {SQL: walk.drop}}
			for _, mode := range explaintest.PlanCacheModes {
				reads := relationReads(t, keys, explaintest.ExplainAfter(t, ctx, pool, nil, walk.statement, mode, "json"), walk.table, walk.index)
				switch {
				case len(reads.unprobed) > 0 || reads.probeLoops == 0:
					t.Errorf("%s under %s reads %s other than from its cursor through %s:\n  %s\nplan:\n%s", walk.name, mode, walk.table, walk.index,
						strings.Join(slices.Concat(reads.unprobed, reads.probed), "\n  "), explaintest.ExplainAfter(t, ctx, pool, nil, walk.statement, mode, "text"))
				case reads.rows > backfillBatch+walk.slack:
					t.Errorf("%s under %s reads %.0f rows of %s, want one batch, %.0f at most:\n  %s\nplan:\n%s", walk.name, mode, reads.rows, walk.table,
						backfillBatch+walk.slack, strings.Join(reads.probed, "\n  "), explaintest.ExplainAfter(t, ctx, pool, nil, walk.statement, mode, "text"))
				default:
					t.Logf("%s under %s reads %.0f rows of %s:\n  %s", walk.name, mode, reads.rows, walk.table, strings.Join(reads.probed, "\n  "))
				}

				control := relationReads(t, keys, explaintest.ExplainAfter(t, ctx, pool, dropped, walk.statement, mode, "json"), walk.table, walk.index)
				if len(control.unprobed) == 0 || control.rows <= backfillBatch {
					t.Errorf("%s with %s dropped, the control, reads %.0f rows of %s under %s, so the check cannot tell a read of every row from a batch:\n%s",
						walk.name, walk.index, control.rows, walk.table, mode, explaintest.ExplainAfter(t, ctx, pool, dropped, walk.statement, mode, "text"))
				} else {
					t.Logf("%s with %s dropped under %s reads %.0f rows of %s and fails the check, as the control must:\n  %s",
						walk.name, walk.index, mode, control.rows, walk.table, strings.Join(control.unprobed, "\n  "))
				}
			}
		})
	}
}

// topicWalks are the walks TestEveryTopicPassWalksOneBatchThroughAnIndex holds, each bound to the
// key of the seed's middle row in its own order (middleKey), as its pass binds the last key of the
// batch before.
func topicWalks(t *testing.T, pool *store.Pool) []topicWalk {
	t.Helper()
	middle := func(query string) []any { return middleKey(t, pool, query) }
	link := middle(`select issue_key, target_key from issue_links order by issue_key, target_key`)
	component := middle(`select project_key, id from components order by project_key, id`)
	dependency := middle(`select project_key, from_id, to_id from component_depends order by project_key, from_id, to_id`)
	member := middle(`select issue_key, component_id from issue_component_members order by issue_key, component_id`)
	result := middle(`select r.id::text from check_results r
		where not exists (select 1 from check_result_retractions x where x.result_id = r.id) order by r.id`)
	retraction := middle(`select result_id::text from check_result_retractions order by result_id`)
	version := middle(`select v.artifact_id::text, v.number from artifact_versions v order by v.artifact_id, v.number`)
	reference := middle(`select from_id, to_kind, to_id from refs where from_kind = 'artifact' order by from_id, to_kind, to_id`)
	orphan := middle(`select a.id::text, t.to_kind, t.to_id from artifacts a
		join graph_tuples t on t.from_kind = 'artifact' and t.from_id = a.ref_key and t.kind = 'mentions' order by 1, 2, 3`)
	comment := middle(`select id::text from comments order by id`)
	issue := middle(`select key from issues order by key`)
	return []topicWalk{
		{name: "issue links", table: "issue_links", index: "issue_links_pkey",
			drop:      "alter table issue_links drop constraint issue_links_pkey",
			statement: explaintest.Statement{SQL: issueLinkRowsStatement, Args: append(link, backfillBatch)}},
		{name: "components", table: "components", index: "components_pkey",
			drop:      "alter table components drop constraint components_pkey cascade",
			statement: explaintest.Statement{SQL: componentRowsStatement, Args: append(component, backfillBatch)}},
		{name: "component dependencies", table: "component_depends", index: "component_depends_pkey",
			drop:      "alter table component_depends drop constraint component_depends_pkey",
			statement: explaintest.Statement{SQL: componentDependsRowsStatement, Args: append(dependency, backfillBatch)}},
		{name: "issue components", table: "issue_component_members", index: "issue_component_members_pkey",
			drop:      "alter table issue_component_members drop constraint issue_component_members_pkey",
			statement: explaintest.Statement{SQL: issueComponentMemberRowsStatement, Args: append(member, backfillBatch)}},
		{name: "check results", table: "check_results", index: "check_results_pkey",
			drop:      "alter table check_results drop constraint check_results_pkey cascade",
			statement: explaintest.Statement{SQL: missingResultKeysStatement, Args: append(result, backfillBatch)}},
		{name: "check result retractions", table: "check_result_retractions", index: "check_result_retractions_pkey",
			drop:      "alter table check_result_retractions drop constraint check_result_retractions_pkey",
			statement: explaintest.Statement{SQL: owedRetractionKeysStatement, Args: append(retraction, backfillBatch)}},
		{name: "versions", table: "artifact_versions", index: "artifact_versions_artifact_id_number_key",
			drop:      "alter table artifact_versions drop constraint artifact_versions_artifact_id_number_key cascade",
			statement: explaintest.Statement{SQL: missingVersionKeysStatement, Args: append(version, backfillBatch)}},
		{name: "reference rows", table: "refs", index: "refs_pkey",
			drop:      "alter table refs drop constraint refs_pkey",
			statement: explaintest.Statement{SQL: owedReferenceRowsStatement, Args: append(reference, backfillBatch)}},
		// The orphaned references' index keys a document, not each of its mentions, so a batch reads
		// the cursor's own document again, whose later mentions it may owe, and the document past its
		// last, which the sort by mention reads to end that document's group.
		{name: "orphaned references", table: "artifacts", index: "artifacts_id_text", slack: 2,
			drop:      "drop index artifacts_id_text",
			statement: explaintest.Statement{SQL: orphanedReferenceTuplesStatement, Args: append(orphan, backfillBatch)}},
		{name: "comment anchors", table: "comments", index: "comments_pkey",
			drop:      "alter table comments drop constraint comments_pkey cascade",
			statement: explaintest.Statement{SQL: owedAnchorRowsStatement, Args: append(comment, backfillBatch)}},
		{name: "head fill", table: "issues", index: "issues_pkey",
			drop:      "alter table issues drop constraint issues_pkey cascade",
			statement: explaintest.Statement{SQL: unmarkedIssuesAfterStatement, Args: append(issue, backfillBatch)}},
	}
}

// middleKey answers the columns of the row in the middle of the seed's topicWalkRows rows that
// query, a select of a walk's key in its order, reads.
func middleKey(t *testing.T, pool *store.Pool, query string) []any {
	t.Helper()
	rows, err := pool.Query(context.Background(), query+` offset $1 limit 1`, topicWalkRows/2)
	if err != nil {
		t.Fatalf("read the middle key of %q: %v", query, err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([]any, error) { return row.Values() })
	if err != nil || len(found) != 1 {
		t.Fatalf("read the middle key of %q: %d row(s), %v", query, len(found), err)
	}
	return found[0]
}

// countRows runs statement and answers how many rows it answered.
func countRows(t *testing.T, pool *store.Pool, statement explaintest.Statement) int {
	t.Helper()
	rows, err := pool.Query(context.Background(), statement.SQL, statement.Args...)
	if err != nil {
		t.Fatalf("run the batch: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("run the batch: %v", err)
	}
	return count
}

// seedTopicWalks writes the seed TestEveryTopicPassWalksOneBatchThroughAnIndex reads, at least
// topicWalkRows rows owed to each walk: topicWalkRows issues of project TWK and one more, none
// marked for the head fill, each with one document at version 1, whose version no node stands for;
// a link naming no node from each issue but the last to the next; components of one snapshot, a
// dependency naming no node from each but the last to the next, and each issue's one component;
// for each document a check result whose node the store does not hold, and another, numbered after
// all of those, that holds its node and a retraction the node does not state; for each document its
// node, named on its row, a refs row no tuple states, to its issue, and an asserted mentions tuple
// no refs row projects, to the next issue; and for each document a comment naming its node whose
// anchor names a block no anchored_in states. It then analyzes the tables the walks plan over, all
// but node_kind of topicWalkedColumns' tables, whose statistics target it sets to 0 for the analysis
// and back to the default after it, so those columns hold no statistics, as the deploy leaves them.
func seedTopicWalks(t *testing.T, pool *store.Pool) {
	t.Helper()
	ctx := context.Background()
	unanalyzed := func(target int) {
		for _, table := range topicWalkedColumns {
			if _, err := pool.Exec(ctx, fmt.Sprintf(`alter table %s alter column node_kind set statistics %d`, table, target)); err != nil {
				t.Fatalf("set the statistics target of %s.node_kind to %d: %v", table, target, err)
			}
		}
	}
	unanalyzed(0)
	defer unanalyzed(-1)
	for _, step := range []struct {
		what, sql string
		args      []any
	}{
		{"seed project TWK", `insert into projects (key, name, default_assignee) values ('TWK', 'Topic walks', 'alice')`, nil},
		{"seed the issues", `
			insert into issues (key, project_key, number, title, created_by, rank)
			select 'TWK-' || i, 'TWK', i, 'Issue ' || i, '{"kind":"user","id":"alice"}', 'U'
			from generate_series(1, $1::int + 1) i`, []any{topicWalkRows}},
		{"seed each issue's document", `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			select key, 'TWK', 'spec', 'spec.md', 'doc', false, '{"kind":"user","id":"alice"}'
			from issues where project_key = 'TWK'`, nil},
		{"write version 1 of each document", `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			select id, 1, 'Version 1.', '[{"kind":"user","id":"alice"}]' from artifacts where project_key = 'TWK'`, nil},
		{"link each issue to the next", `
			insert into issue_links (issue_key, kind, target_key)
			select 'TWK-' || i, 'blocked_by', 'TWK-' || (i + 1) from generate_series(1, $1::int) i`, []any{topicWalkRows}},
		{"seed the architecture source", `
			insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
			values ('TWK', 'twk/arch', 'main', 1, '{"kind":"user","id":"alice"}')`, nil},
		{"seed the snapshot", `
			insert into architecture_snapshots (project_key, commit, files) values ('TWK', 'one', '{}')`, nil},
		{"seed the components", `
			insert into components (project_key, id, title, prose, snapshot_id, source_id)
			select 'TWK', 'c' || lpad(i::text, 5, '0'), 'Component ' || i, 'Prose.', s.id, src.id
			from generate_series(1, $1::int + 1) i
			cross join (select id from architecture_snapshots where project_key = 'TWK') s
			cross join (select id from architecture_sources where project_key = 'TWK') src`, []any{topicWalkRows}},
		{"make each component depend on the next", `
			insert into component_depends (project_key, from_id, to_id)
			select 'TWK', 'c' || lpad(i::text, 5, '0'), 'c' || lpad((i + 1)::text, 5, '0') from generate_series(1, $1::int) i`, []any{topicWalkRows}},
		{"give each issue its components", `
			insert into issue_components (issue_key, mode) select key, 'explicit' from issues where project_key = 'TWK'`, nil},
		{"give each issue one component", `
			insert into issue_component_members (issue_key, project_key, component_id)
			select 'TWK-' || i, 'TWK', 'c' || lpad(i::text, 5, '0') from generate_series(1, $1::int) i`, []any{topicWalkRows}},
		{"record the results whose node the store does not hold", `
			insert into check_results (id, issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
				check_sha256, role, commit_sha, verdict, failed, actor)
			select ('00000000-0000-4000-8000-' || lpad(i.number::text, 12, '0'))::uuid, a.issue_key, a.issue_key, a.id, 'c1', 1,
			       'sum', 'implementer', '1111111111111111111111111111111111111111', 'fail', 1, '{"kind":"user","id":"alice"}'
			from artifacts a join issues i on i.key = a.issue_key
			where a.project_key = 'TWK'`, nil},
		{"record the retracted results, numbered after them", `
			insert into check_results (id, issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
				check_sha256, role, commit_sha, verdict, failed, actor)
			select ('80000000-0000-4000-8000-' || lpad(i.number::text, 12, '0'))::uuid, a.issue_key, a.issue_key, a.id, 'c1', 1,
			       'sum', 'implementer', '1111111111111111111111111111111111111111', 'fail', 1, '{"kind":"user","id":"alice"}'
			from artifacts a join issues i on i.key = a.issue_key
			where a.project_key = 'TWK'`, nil},
		{"give each retracted result its node", `
			insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
			select 'result', r.id::text, 'result', 'project', 'live', '{}'::jsonb, now(), '{"kind":"system","id":"topic-walks"}'::jsonb, 0
			from check_results r where r.id >= '80000000-0000-4000-8000-000000000000'::uuid`, nil},
		{"retract each of them", `
			insert into check_result_retractions (result_id, reason, actor)
			select id, 'The run measured the wrong build.', '{"kind":"user","id":"alice"}'
			from check_results where id >= '80000000-0000-4000-8000-000000000000'::uuid`, nil},
		{"give each document its node", `
			insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
			select 'artifact', ref_key, 'artifact', 'project', 'live', '{}'::jsonb, now(), '{"kind":"system","id":"topic-walks"}'::jsonb, 0
			from artifacts where project_key = 'TWK'`, nil},
		{"name each document's node on its row", `
			update artifacts set node_kind = 'artifact', node_address = ref_key where project_key = 'TWK'`, nil},
		{"write a reference no tuple states from each document to its issue", `
			insert into refs (from_kind, from_id, to_kind, to_id)
			select 'artifact', id::text, 'issue', issue_key from artifacts where project_key = 'TWK'`, nil},
		{"state a mention no reference projects from each document to the next issue", `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
			select 'artifact', a.ref_key, 'mentions', 'issue', 'TWK-' || (i.number + 1), now(), '{"kind":"system","id":"topic-walks"}'::jsonb, 0
			from artifacts a join issues i on i.key = a.issue_key where a.project_key = 'TWK'`, nil},
		{"write a comment anchored to a block of each document", `
			insert into comments (issue_key, author, body, anchor)
			select a.issue_key, '{"kind":"user","id":"alice"}', 'A remark.', jsonb_build_object('artifact_id', a.id::text, 'block_id', 'b1')
			from artifacts a where a.project_key = 'TWK'`, nil},
		{"give each comment its node", `
			insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
			select 'comment', c.id::text, 'comment', 'project', 'live', '{}'::jsonb, now(), '{"kind":"system","id":"topic-walks"}'::jsonb, 0
			from comments c join issues i on i.key = c.issue_key where i.project_key = 'TWK'`, nil},
		{"name each comment's node on its row", `
			update comments c set node_kind = 'comment', node_address = c.id::text
			from issues i where i.key = c.issue_key and i.project_key = 'TWK'`, nil},
		{"analyze the walked tables", `analyze issues, issue_links, components, component_depends, issue_components,
			issue_component_members, check_results, check_result_retractions, artifacts, artifact_versions, refs, comments,
			graph_nodes, graph_tuples, issue_head_marks`, nil},
	} {
		if _, err := pool.Exec(ctx, step.sql, step.args...); err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
	}
}

// topicWalkedColumns are the tables whose node_kind the topic walks read by, the issue links, the
// components, their dependencies, an issue's components and the documents (migration
// 20261116092210_issue_tree_markers), and the comments (migration 20261109022248), each of which
// the run's analysis gives its statistics (walkedColumnsAnalysis).
var topicWalkedColumns = []string{"issue_links", "components", "component_depends", "issue_component_members", "artifacts", "comments"}

// nodeKindStatistics answers how many of topicWalkedColumns' tables hold statistics of node_kind.
func nodeKindStatistics(t *testing.T, pool *store.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `
		select count(*) from pg_stats
		where schemaname = current_schema() and attname = 'node_kind' and tablename = any($1)
	`, topicWalkedColumns).Scan(&count); err != nil {
		t.Fatalf("read the statistics of node_kind: %v", err)
	}
	return count
}
