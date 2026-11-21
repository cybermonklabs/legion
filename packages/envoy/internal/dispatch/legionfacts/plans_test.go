package legionfacts

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/explaintest"
)

// TestReadClosedInReadsTheClosedFactsIndex: ReadClosedIn's statement, as it sends it (wideSelect
// and closedInWhere), reads one project's issues closed in a window through issue_facts_closed
// (migration 20261117100000) and never scans issue_facts, on an analyzed store of five projects of
// 4,000 stored facts each, nine in ten closed, one a day back over a year: enough rows that the
// planner prices a scan of the whole table above the index read.
func TestReadClosedInReadsTheClosedFactsIndex(t *testing.T) {
	ctx := t.Context()
	pool := factsDatabase(t)
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		insert into issue_facts (project_key, issue_key, opened_at, closed_at)
		select project, project || '-' || n, $1::timestamptz - make_interval(days => n % 365 + 10),
		       case when n % 10 <> 0 then $1::timestamptz - make_interval(days => n % 365) end
		from unnest(array['FA', 'FB', 'FC', 'FD', 'FE']) as project, generate_series(1, 4000) as n
		order by n % 365 desc, project
	`, now); err != nil {
		t.Fatalf("seed the facts: %v", err)
	}
	if _, err := pool.Exec(ctx, `analyze issue_facts`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	from := now.AddDate(0, 0, -28)
	closed, err := ReadClosedIn(ctx, pool, "FC", from, now, now)
	if err != nil {
		t.Fatalf("ReadClosedIn: %v", err)
	}
	var raw []byte
	if err := pool.QueryRow(ctx, "explain (format json) "+wideSelect+closedInWhere, "FC", from, now).Scan(&raw); err != nil {
		t.Fatalf("explain ReadClosedIn: %v", err)
	}
	plan := explaintest.OnePlan(t, raw)
	if explaintest.SeqScansRelation(t, plan, "issue_facts") {
		t.Fatalf("ReadClosedIn scans issue_facts sequentially; want a range of issue_facts_closed:\n%s", plan)
	}
	scans := explaintest.IndexScans(t, plan)
	if !slices.ContainsFunc(scans, func(scan explaintest.IndexScan) bool { return scan.Index == "issue_facts_closed" }) {
		t.Fatalf("ReadClosedIn reads %v; want issue_facts_closed:\n%s", scans, plan)
	}
	t.Logf("ReadClosedIn answered %d issues of FC closed in 28 days, through %v", len(closed), scans)
}

// TestTheOtherBuildsRowsReadReadsTheWrittenBuildIndex: the read of the rows another build wrote
// (otherBuildsRowsQuery), which every first page of the build a project's record names makes,
// reaches issue_facts through issue_facts_written_build (migration 20261117100000) and never scans
// the table, on an analyzed store of five projects of 4,000 stored rows each, every project
// recorded under the running build and three rows of one written by another, as a roll's last
// write leaves them.
func TestTheOtherBuildsRowsReadReadsTheWrittenBuildIndex(t *testing.T) {
	ctx := t.Context()
	pool := factsDatabase(t)
	const running = "legion-envoy-running"
	if _, err := pool.Exec(ctx, `
		insert into projects (key, name, default_assignee)
		select project, project, 'alice' from unnest(array['FA', 'FB', 'FC', 'FD', 'FE']) as project;
		insert into issue_facts_computed (project_key, computed_at, computed_rows, computed_build)
		select project, now(), 4000, 'legion-envoy-running' from unnest(array['FA', 'FB', 'FC', 'FD', 'FE']) as project;
		insert into issue_facts (project_key, issue_key, opened_at, written_build)
		select project, project || '-' || n, now() - make_interval(days => n % 365),
		       case when project = 'FC' and n <= 3 then 'legion-envoy-older' else 'legion-envoy-running' end
		from unnest(array['FA', 'FB', 'FC', 'FD', 'FE']) as project, generate_series(1, 4000) as n;
		analyze issue_facts;
		analyze issue_facts_computed;
	`); err != nil {
		t.Fatalf("seed the facts: %v", err)
	}
	other, err := otherBuildsRows(ctx, pool, running)
	if err != nil {
		t.Fatalf("otherBuildsRows: %v", err)
	}
	if want := []string{"FC-1", "FC-2", "FC-3"}; len(other) != 1 || !slices.Equal(other["FC"], want) {
		t.Errorf("the rows another build wrote are %v, want %v of FC alone", other, want)
	}
	var raw []byte
	if err := pool.QueryRow(ctx, "explain (format json) "+otherBuildsRowsQuery, running).Scan(&raw); err != nil {
		t.Fatalf("explain otherBuildsRows: %v", err)
	}
	plan := explaintest.OnePlan(t, raw)
	if explaintest.SeqScansRelation(t, plan, "issue_facts") {
		t.Fatalf("the read of the rows another build wrote scans issue_facts sequentially; want ranges of issue_facts_written_build:\n%s", plan)
	}
	scans := explaintest.IndexScans(t, plan)
	if !slices.ContainsFunc(scans, func(scan explaintest.IndexScan) bool { return scan.Index == "issue_facts_written_build" }) {
		t.Fatalf("the read of the rows another build wrote reads %v; want issue_facts_written_build:\n%s", scans, plan)
	}
	t.Logf("of 20,000 rows of five projects recorded under the running build, the read answered %v through %v", other, scans)
}

// TestTheOtherBuildsFilingsReadReadsTheWrittenBuildIndex: the read of the filings another build
// wrote (otherBuildsFilingsQuery), which the same pages make, reaches issue_filings through
// issue_filings_written_build (migration 20261117100007) and never scans the table, on an analyzed
// store of five projects of 4,000 filings each, every project recorded under the running build and
// three filings of one written by another.
func TestTheOtherBuildsFilingsReadReadsTheWrittenBuildIndex(t *testing.T) {
	ctx := t.Context()
	pool := factsDatabase(t)
	const running = "legion-envoy-running"
	if _, err := pool.Exec(ctx, `
		insert into projects (key, name, default_assignee)
		select project, project, 'alice' from unnest(array['FA', 'FB', 'FC', 'FD', 'FE']) as project;
		insert into issue_facts_computed (project_key, computed_at, computed_rows, computed_build)
		select project, now(), 4000, 'legion-envoy-running' from unnest(array['FA', 'FB', 'FC', 'FD', 'FE']) as project;
		insert into issue_filings (issue_key, project_key, filed_at, filer_role, class, written_build)
		select project || '-' || n, project, now() - make_interval(hours => n), 'architect', 'out_of_tree',
		       case when project = 'FC' and n <= 3 then 'legion-envoy-older' else 'legion-envoy-running' end
		from unnest(array['FA', 'FB', 'FC', 'FD', 'FE']) as project, generate_series(1, 4000) as n;
		analyze issue_filings;
		analyze issue_facts_computed;
	`); err != nil {
		t.Fatalf("seed the filings: %v", err)
	}
	other, err := otherBuildsFilings(ctx, pool, running)
	if err != nil {
		t.Fatalf("otherBuildsFilings: %v", err)
	}
	if want := []string{"FC-1", "FC-2", "FC-3"}; len(other) != 1 || !slices.Equal(other["FC"], want) {
		t.Errorf("the filings another build wrote are %v, want %v of FC alone", other, want)
	}
	var raw []byte
	if err := pool.QueryRow(ctx, "explain (format json) "+otherBuildsFilingsQuery, running).Scan(&raw); err != nil {
		t.Fatalf("explain otherBuildsFilings: %v", err)
	}
	plan := explaintest.OnePlan(t, raw)
	if explaintest.SeqScansRelation(t, plan, "issue_filings") {
		t.Fatalf("the read of the filings another build wrote scans issue_filings sequentially; want ranges of issue_filings_written_build:\n%s", plan)
	}
	scans := explaintest.IndexScans(t, plan)
	if !slices.ContainsFunc(scans, func(scan explaintest.IndexScan) bool { return scan.Index == "issue_filings_written_build" }) {
		t.Fatalf("the read of the filings another build wrote reads %v; want issue_filings_written_build:\n%s", scans, plan)
	}
	t.Logf("of 20,000 filings of five projects recorded under the running build, the read answered %v through %v", other, scans)
}

// TestTheBoardsReadReadsItsRowsByKeyAndByRootAlone: Read, the Legion board's read of the facts,
// answers the rows of the keys it is given, of every issue of the trees it is given and of every
// open issue of the project, and no other row, and every arm of its statement (boardSelect and
// shownWhere) reaches issue_facts through an index keyed by its own condition: the primary key for
// the keys, issue_facts_root (migration 20261117100000) for the trees and issue_facts_closed
// (the same migration) for the open rows, under closed_at IS NULL; never a scan of the table
// nor a read of the project's whole range filtered after it (dispatch://INFRA-2916, k23).
//
// The store has the shape at which the planner prices a scan of the whole table no dearer than the
// open rows' index range: FC's 12,000 rows are most of it, beside four projects of 500, one row in
// three of each is open, as 4,272 of INFRA's 13,103 were on the build farm, the rows stand in trees
// of ten, and each row's bars has the shape a row Legion wrote holds, so a page holds four rows as
// INFRA's pages hold four or five. It is analyzed. With sequential scans on, the open arm's plan
// here is a Seq Scan of issue_facts with the values, whether the board names cards or none, so the
// plans below hold Read's transaction (readByIndex) to what keeps that arm on its index. The board
// of FC names 34 cards, 24 of them in the 12 trees it shows and 10 whose stored root is another,
// and a key no row holds; a board that names no card answers the open rows alone.
//
// Every plan is taken in readByIndex's transaction, the one Read runs its statement in, and under
// no setting of the test's own but the plan cache's mode: with the values, as the planner prices
// them, for both boards; and as the generic plan pgx's cached statement settles on, which knows no
// value of the lists.
func TestTheBoardsReadReadsItsRowsByKeyAndByRootAlone(t *testing.T) {
	ctx := t.Context()
	pool := factsDatabase(t)
	if _, err := pool.Exec(ctx, `
		insert into issue_facts (project_key, issue_key, opened_at, closed_at, root_key, tree_depth, bars)
		select project, project || '-' || n, now() - make_interval(days => n % 365 + 1),
		       case when n % 3 <> 0 then now() - make_interval(days => n % 365) end,
		       project || '-' || ((n - 1) / 10 * 10 + 1), case when n % 10 = 1 then 0 else 1 end, shape.bars
		from unnest(array['FA', 'FB', 'FC', 'FD', 'FE']) as project, generate_series(1, 12000) as n,
		     (select jsonb_object_agg(role, jsonb_build_object('hours', 0, 'holds_hours', false, 'segments',
		             jsonb_build_object('idle', 0, 'ci_wait', 0, 'in_a_turn', 0, 'waiting_on_person', 0, 'waiting_on_children', 0)))
		      from unnest(array['architect', 'planner', 'implementer', 'tester', 'reviewer', 'merger', 'no_role']) as role) as shape (bars)
		where n <= case when project = 'FC' then 12000 else 500 end
		order by n, project;
		analyze issue_facts;
	`); err != nil {
		t.Fatalf("seed the facts: %v", err)
	}
	var roots, keys []string
	want, open := map[string]bool{}, map[string]bool{}
	for tree := range 12 {
		root := 10*tree*3 + 1
		roots = append(roots, fmt.Sprintf("FC-%d", root))
		for n := root; n < root+10; n++ {
			want[fmt.Sprintf("FC-%d", n)] = true
		}
		keys = append(keys, fmt.Sprintf("FC-%d", root+tree%10), fmt.Sprintf("FC-%d", root+(tree+5)%10))
	}
	for n := 2001; n <= 2010; n++ {
		keys, want[fmt.Sprintf("FC-%d", n)] = append(keys, fmt.Sprintf("FC-%d", n)), true
	}
	keys = append(keys, "FC-99999")
	for n := 3; n <= 12000; n += 3 {
		want[fmt.Sprintf("FC-%d", n)], open[fmt.Sprintf("FC-%d", n)] = true, true
	}
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	rows, err := Read(ctx, pool, "FC", keys, roots, at)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got, wanted := slices.Sorted(maps.Keys(rows)), slices.Sorted(maps.Keys(want)); !slices.Equal(got, wanted) {
		t.Errorf("Read answered %d rows, want the %d of the keys, the trees and the open rows; it answered %v and missed %v",
			len(got), len(wanted), absent(got, want), absent(wanted, rows))
	}
	bare, err := Read(ctx, pool, "FC", nil, nil, at)
	if err != nil {
		t.Fatalf("Read with no card: %v", err)
	}
	if got, wanted := slices.Sorted(maps.Keys(bare)), slices.Sorted(maps.Keys(open)); !slices.Equal(got, wanted) {
		t.Errorf("Read with no card answered %d rows, want the %d open rows; it answered %v and missed %v",
			len(got), len(wanted), absent(got, open), absent(wanted, bare))
	}

	statement := boardSelect + shownWhere
	keyedBy := []struct{ index, cond string }{{"issue_facts_pkey", "issue_key"}, {"issue_facts_root", "root_key"}, {"issue_facts_closed", "closed_at IS NULL"}}
	judge := func(how string, plan json.RawMessage) []explaintest.IndexScan {
		t.Helper()
		if explaintest.SeqScansRelation(t, plan, "issue_facts") {
			t.Errorf("%s, the board's read scans issue_facts sequentially:\n%s", how, plan)
		}
		if unkeyed := explaintest.ScansWithoutIndexCond(t, plan); len(unkeyed) > 0 {
			t.Errorf("%s, the board's read reads %v under no index condition:\n%s", how, unkeyed, plan)
		}
		scans := explaintest.IndexScans(t, plan)
		for _, scan := range scans {
			if !slices.ContainsFunc(keyedBy, func(keyed struct{ index, cond string }) bool {
				return scan.Index == keyed.index && strings.Contains(scan.Cond, keyed.cond)
			}) {
				t.Errorf("%s, the board's read reads %s under %q; want each arm through its own index under its own condition, %v:\n%s", how, scan, scan.Cond, keyedBy, plan)
			}
		}
		for _, keyed := range keyedBy {
			if !slices.ContainsFunc(scans, func(scan explaintest.IndexScan) bool { return scan.Index == keyed.index }) {
				t.Errorf("%s, the board's read reads %v; want %s under %s:\n%s", how, scans, keyed.index, keyed.cond, plan)
			}
		}
		return scans
	}
	var withCards, noCard, generic json.RawMessage
	var lines []string
	if err := readByIndex(ctx, pool, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx, "explain (analyze, format json) "+statement, "FC", keys, roots).Scan(&raw); err != nil {
			return fmt.Errorf("explain the board's read: %w", err)
		}
		withCards = explaintest.OnePlan(t, raw)
		if err := tx.QueryRow(ctx, "explain (analyze, format json) "+statement, "FC", []string{}, []string{}).Scan(&raw); err != nil {
			return fmt.Errorf("explain the board's read with no card: %w", err)
		}
		noCard = explaintest.OnePlan(t, raw)
		text, err := tx.Query(ctx, "explain (analyze, buffers) "+statement, "FC", []string{}, []string{})
		if err != nil {
			return fmt.Errorf("explain the board's read as text: %w", err)
		}
		if lines, err = pgx.CollectRows(text, pgx.RowTo[string]); err != nil {
			return fmt.Errorf("explain the board's read as text: %w", err)
		}
		// The generic plan, in this transaction, so under readByIndex's settings and no setting of
		// the test's own: GenericPlan forces the mode, since a test never executes the statement
		// often enough to reach it.
		generic = explaintest.GenericPlan(t, ctx, tx, "board_facts", statement,
			[]string{"text", "text[]", "text[]"}, []string{"'FC'", "array['FC-1']", "array['FC-1']"})
		return nil
	}); err != nil {
		t.Fatalf("take the board's plans as Read runs it: %v", err)
	}
	cards := judge("with the board's values", withCards)
	none := judge("with no card", noCard)
	cached := judge("under the generic plan", generic)
	t.Logf("of 14,000 rows of five projects, 4,000 of FC's 12,000 open, Read answered %d of FC for %d keys and %d trees and %d for no card, through %v (with the cards), %v (with none) and %v (generic); the plan with no card:\n%s",
		len(rows), len(keys), len(roots), len(bare), cards, none, cached, strings.Join(lines, "\n"))
}

// absent is the keys of keys that set does not hold, at most ten of them, for a failure to name.
func absent[V any](keys []string, set map[string]V) []string {
	var missing []string
	for _, key := range keys {
		if _, held := set[key]; !held && len(missing) < 10 {
			missing = append(missing, key)
		}
	}
	return missing
}

// TestTheStoredFilingsReadKeepsItsIndexesUnderAGenericPlan: the read of a project's stored filings
// whose filed issue or filer's issue a pass recomputes (storedFilingsOfQuery) reaches issue_filings
// through its primary key and through issue_filings_filer (migration 20261117100007) under the
// generic plan pgx's cached statement settles on, which knows no value of the keys, and never
// scans the table: 6,000 filings of two projects, two in three by a phase worker, are analyzed
// first, and sequential scans are off, so the plan names only the indexes the statement can prove.
func TestTheStoredFilingsReadKeepsItsIndexesUnderAGenericPlan(t *testing.T) {
	ctx := t.Context()
	pool := factsDatabase(t)
	if _, err := pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('FILE', 'Filings', 'alice'), ('FOTH', 'Other', 'alice');
		insert into issue_filings (issue_key, project_key, filed_at, filer_role, filer_issue, class)
		select project || '-' || n, project, now() - make_interval(hours => n),
		       case when n % 3 = 0 then 'controller' else 'architect' end,
		       case when n % 3 = 0 then '' else project || '-' || (n / 10 + 1) end,
		       case when n % 3 = 0 then 'by_the_controller' else 'in_tree_at_design' end
		from unnest(array['FILE', 'FOTH']) as project, generate_series(1, 3000) as n;
		analyze issue_filings;
	`); err != nil {
		t.Fatalf("seed the filings: %v", err)
	}
	keys, err := filingsToCompute(ctx, pool, "FILE", []string{"FILE-7"}, nil)
	if err != nil {
		t.Fatalf("filingsToCompute: %v", err)
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plan: %v", err)
	}
	defer connection.Release()
	plan := explaintest.GenericPlan(t, ctx, connection, "stored_filings_of", storedFilingsOfQuery,
		[]string{"text", "text[]"}, []string{"'FILE'", "array['FILE-7']"}, "set local enable_seqscan = off")
	if explaintest.SeqScansRelation(t, plan, "issue_filings") {
		t.Fatalf("the generic plan of the stored filings read scans issue_filings:\n%s", plan)
	}
	scans := explaintest.IndexScans(t, plan)
	if !slices.ContainsFunc(scans, func(scan explaintest.IndexScan) bool { return scan.Index == "issue_filings_filer" }) {
		t.Fatalf("the generic plan of the stored filings read reads %v; want issue_filings_filer:\n%s", scans, plan)
	}
	t.Logf("FILE-7 is the filed or the filer's issue of %d filings; the generic plan reads %v", len(keys), scans)
}

// TestTheLostMergesReadProbesTheMergedFactsIndex: the read of the stored rows a pull request's
// change left with a merge no pull request backs (lostMergesQuery), which every page of changes
// holding a pull request's makes, reaches issue_facts through issue_facts_merged (migration
// 20261117100000), a probe a merge, and the pull requests through delivery_pull_requests_merged_at
// (migration 0080), and scans neither table, with the values and under the generic plan pgx's
// cached statement settles on, each with the planner's settings as the pass runs it. The store
// holds five projects of 4,000 stored rows each, nine in ten merged, each merge one pull request's,
// analyzed. Of the two merges read, one is still its issue's pull request's and the other's pull
// request names no issue any more, and the read answers that one alone.
func TestTheLostMergesReadProbesTheMergedFactsIndex(t *testing.T) {
	ctx := t.Context()
	pool := factsDatabase(t)
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		insert into issue_facts (project_key, issue_key, opened_at, closed_at, merged_at)
		select project, project || '-' || n, $1::timestamptz - make_interval(hours => n + 10),
		       case when n % 10 <> 0 then $1::timestamptz - make_interval(hours => n) end,
		       case when n % 10 <> 0 then $1::timestamptz - make_interval(mins => n * 7 + rank::int) end
		from unnest(array['FA', 'FB', 'FC', 'FD', 'FE']) with ordinality as given (project, rank),
		     generate_series(1, 4000) as n
	`, now); err != nil {
		t.Fatalf("seed the facts: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('FC', 'FC', 'alice');
		insert into issues (key, project_key, number, title, status, rank, created_by)
		values ('FC-1', 'FC', 1, 'A merged issue', 'done', '00000001', '{"kind":"user","id":"alice"}');
		insert into delivery_pull_requests (repo, number, title, url, author, merged_at, issue_key)
		select 'acme/' || lower(project_key), split_part(issue_key, '-', 2)::int, 'A pull request',
		       'https://example.test/' || issue_key, 'alice', merged_at,
		       case when issue_key = 'FC-1' then issue_key end
		from issue_facts where merged_at is not null;
		analyze issue_facts;
		analyze delivery_pull_requests;
	`); err != nil {
		t.Fatalf("seed the pull requests: %v", err)
	}
	var merges []time.Time
	if err := pool.QueryRow(ctx, `
		select array_agg(merged_at order by issue_key) from issue_facts where issue_key in ('FC-1', 'FC-2')
	`).Scan(&merges); err != nil {
		t.Fatalf("read the merges of FC-1 and FC-2: %v", err)
	}
	left, err := lostMerges(ctx, pool, merges)
	if err != nil {
		t.Fatalf("lostMerges: %v", err)
	}
	if want := []touch{{project: "FC", issue: "FC-2"}}; !slices.Equal(left, want) {
		t.Errorf("the rows left with a merge no pull request backs are %v, want %v: FC-1's pull request is still its own", left, want)
	}

	var raw []byte
	if err := pool.QueryRow(ctx, "explain (format json) "+lostMergesQuery, merges).Scan(&raw); err != nil {
		t.Fatalf("explain lostMerges: %v", err)
	}
	valued := explaintest.OnePlan(t, raw)
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plan: %v", err)
	}
	defer connection.Release()
	literal := fmt.Sprintf("array['%s', '%s']::timestamptz[]", merges[0].Format(time.RFC3339Nano), merges[1].Format(time.RFC3339Nano))
	generic := explaintest.GenericPlan(t, ctx, connection, "lost_merges", lostMergesQuery, []string{"timestamptz[]"}, []string{literal})
	for how, plan := range map[string]json.RawMessage{"with the values": valued, "under the generic plan": generic} {
		for _, relation := range []string{"issue_facts", "delivery_pull_requests"} {
			if explaintest.SeqScansRelation(t, plan, relation) {
				t.Errorf("%s, the lost merges read scans %s sequentially:\n%s", how, relation, plan)
			}
		}
		scans := explaintest.IndexScans(t, plan)
		for _, index := range []string{"issue_facts_merged", "delivery_pull_requests_merged_at"} {
			if !slices.ContainsFunc(scans, func(scan explaintest.IndexScan) bool { return scan.Index == index }) {
				t.Errorf("%s, the lost merges read reads %v; want %s:\n%s", how, scans, index, plan)
			}
		}
		t.Logf("%s, of 20,000 stored rows of five projects and 18,000 merged pull requests, the read of two merges answered %v through %v", how, left, scans)
	}
}

// TestEachDeletionOfStaleFactsReadsItsListOnce: each deletion of the stored rows and filings a
// pass computed none for (deleteStaleRows' staleRowsStatement, deleteStaleProjectRows'
// staleProjectRowsStatement and deleteStaleFilings' staleFilingsStatement, each as the delete and
// as a Yielded write's stamp) reads the list of computed keys it is given once, with the pass's
// values and under the generic plan a statement pgx caches can settle on after its fifth run, as
// explaintest.RequireListReadOnce holds it to: no SubPlan runs the list, no nested loop runs it
// again for each row of its other side, and the deletion filters by it as a SetOp Except or a
// hash or merge anti-join. Written as `not (issue_key = any($n))`, each stored row was compared
// with the whole list, which a generic plan cannot hash, so a whole pass of a project of INFRA's
// 13,000 issues compared each of its rows with up to 13,000 keys. The project here is past
// work_mem: 80,000 issues, each with a stored row and a filing, analyzed, and each statement is
// explained in a transaction of its own, as the pass's write (writePass) runs it, with the keys a
// pass sends that computed every issue but one.
func TestEachDeletionOfStaleFactsReadsItsListOnce(t *testing.T) {
	ctx := t.Context()
	pool := factsDatabase(t)
	const issues, build = 80000, "legion-envoy-running"
	if _, err := pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('PL', 'Plans', 'alice');
		insert into issues (key, project_key, number, title, status, rank, created_by)
		select 'PL-' || n, 'PL', n, 'An issue', 'triage', lpad(n::text, 8, '0'), '{"kind":"user","id":"alice"}'
		from generate_series(1, 80000) as n;
		insert into issue_facts (project_key, issue_key, opened_at, written_build)
		select 'PL', 'PL-' || n, now() - make_interval(hours => n), 'legion-envoy-running' from generate_series(1, 80000) as n;
		insert into issue_filings (issue_key, project_key, filed_at, filer_role, class, written_build)
		select 'PL-' || n, 'PL', now() - make_interval(hours => n), 'architect', 'out_of_tree', 'legion-envoy-running'
		from generate_series(1, 80000) as n;
		analyze issues;
		analyze issue_facts;
		analyze issue_filings;
	`); err != nil {
		t.Fatalf("seed the project's issues, rows and filings: %v", err)
	}
	keys := make([]string, issues)
	for n := range keys {
		keys[n] = fmt.Sprintf("PL-%d", n+1)
	}
	// A pass that stated every issue and computed every one but PL-1.
	computed := keys[1:]
	filings := make([]Filing, len(computed))
	for n, key := range computed {
		filings[n].Key = key
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the deletions: %v", err)
	}
	stated, err := deleteStaleRows(ctx, tx, ProjectWrite{Project: "PL", Stated: keys}, computed)
	if err != nil {
		t.Fatalf("deleteStaleRows: %v", err)
	}
	whole, err := deleteStaleProjectRows(ctx, tx, ProjectWrite{Project: "PL", Whole: true}, computed)
	if err != nil {
		t.Fatalf("deleteStaleProjectRows: %v", err)
	}
	if err := deleteStaleFilings(ctx, tx, FilingsWrite{Project: "PL", Filings: filings, Whole: true}); err != nil {
		t.Fatalf("deleteStaleFilings: %v", err)
	}
	var left int
	if err := tx.QueryRow(ctx, `select count(*) from issue_filings where project_key = 'PL'`).Scan(&left); err != nil {
		t.Fatalf("count the filings left: %v", err)
	}
	_ = tx.Rollback(ctx)
	if !slices.Equal(stated, []string{"PL-1"}) || len(whole) != 0 || left != issues-1 {
		t.Errorf("the deletions took %v of the stated keys, then %v of the project, and left %d filings; want PL-1, none (it went already) and %d",
			stated, whole, left, issues-1)
	}

	for _, deletion := range []struct {
		name                 string
		statement            string
		arguments            []any
		parameters, literals []string
	}{
		{"staleRowsStatement", staleRowsStatement(false), []any{"PL", keys, computed},
			[]string{"text", "text[]", "text[]"}, []string{"'PL'", "array['PL-1']", "array['PL-1']"}},
		{"staleRowsStatement, the stamp", staleRowsStatement(true), []any{"PL", keys, computed, build},
			[]string{"text", "text[]", "text[]", "text"}, []string{"'PL'", "array['PL-1']", "array['PL-1']", "'" + build + "'"}},
		{"staleProjectRowsStatement", staleProjectRowsStatement(false), []any{"PL", computed},
			[]string{"text", "text[]"}, []string{"'PL'", "array['PL-1']"}},
		{"staleProjectRowsStatement, the stamp", staleProjectRowsStatement(true), []any{"PL", computed, build},
			[]string{"text", "text[]", "text"}, []string{"'PL'", "array['PL-1']", "'" + build + "'"}},
		{"staleFilingsStatement of a whole read", staleFilingsStatement(false, false), []any{"PL", computed},
			[]string{"text", "text[]"}, []string{"'PL'", "array['PL-1']"}},
		{"staleFilingsStatement of a whole read, the stamp", staleFilingsStatement(false, true), []any{"PL", computed, build},
			[]string{"text", "text[]", "text"}, []string{"'PL'", "array['PL-1']", "'" + build + "'"}},
		{"staleFilingsStatement of the stated issues", staleFilingsStatement(true, false), []any{"PL", computed, keys},
			[]string{"text", "text[]", "text[]"}, []string{"'PL'", "array['PL-1']", "array['PL-1']"}},
		{"staleFilingsStatement of the stated issues, the stamp", staleFilingsStatement(true, true), []any{"PL", computed, keys, build},
			[]string{"text", "text[]", "text[]", "text"}, []string{"'PL'", "array['PL-1']", "array['PL-1']", "'" + build + "'"}},
	} {
		t.Run(deletion.name, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin %s's transaction: %v", deletion.name, err)
			}
			defer tx.Rollback(ctx)
			explaintest.RequireStatementReadsListOnce(t, ctx, tx, deletion.name, deletion.statement, deletion.arguments,
				deletion.parameters, deletion.literals)
		})
	}
}
