package legionfacts

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/explaintest"
)

// TestReadClosedInReadsTheClosedFactsIndex: ReadClosedIn's statement, as it sends it (rowsSelect
// and closedInWhere), reads one project's issues closed in a window through issue_facts_closed
// (migration 20261113082157) and never scans issue_facts, on an analyzed store of five projects of
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
	closed, err := ReadClosedIn(ctx, pool, "FC", from, now)
	if err != nil {
		t.Fatalf("ReadClosedIn: %v", err)
	}
	var raw []byte
	if err := pool.QueryRow(ctx, "explain (format json) "+rowsSelect+closedInWhere, "FC", from, now).Scan(&raw); err != nil {
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

// TestTheStoredFilingsReadKeepsItsIndexesUnderAGenericPlan: the read of a project's stored filings
// whose filed issue or filer's issue a pass recomputes (storedFilingsOfQuery) reaches issue_filings
// through its primary key and through issue_filings_filer (migration 20261113082155) under the
// generic plan pgx's cached statement settles on, which knows no value of the keys, and never
// scans the table: 6,000 filings of two projects, two in three by a phase worker, are analyzed
// first, and sequential scans are off, so the plan names only the indexes the statement can prove.
func TestTheStoredFilingsReadKeepsItsIndexesUnderAGenericPlan(t *testing.T) {
	ctx := t.Context()
	pool := factsDatabase(t)
	if _, err := pool.Exec(ctx, `
		insert into projects (key, name) values ('FILE', 'Filings'), ('FOTH', 'Other');
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
