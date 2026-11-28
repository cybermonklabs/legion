package delivery

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The readers of the timeline plan the statement for the pull requests of each call
// (attributedIssues). A connection that has run the statement prepares it, and from its sixth run
// Postgres may run it by a generic plan, priced for an array of 10 keys, whose joins over the
// keys' rows are nested loops: a window of 3,000 pull requests took 1.50 s by it and 0.23 s by a
// plan of its own keys. On a pool of one connection, ten reads of ten pull requests run the
// prepared statement; the window's read and the waiting read of 300 pull requests after them run
// no plan of it: its counts of generic and custom plans in pg_prepared_statements stay where the
// ten reads left them. The control in the same test: the ten reads move those counts by ten, so
// the counts read are the statement's.
func TestTheTimelinesReadersPlanTheStatementForTheirOwnPullRequests(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	storeTextsRows(t, ctx, pool, 1, 300)
	merged := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		insert into delivery_pull_requests (repo, number, title, url, author, created_at, merged_at)
		select h.repo, h.number, h.title, h.url, 'octocat', $1::timestamptz - interval '1 hour', $1::timestamptz + make_interval(secs => h.number)
		from pull_request_heads h where h.repo = 'acme/widgets'
	`, merged); err != nil {
		t.Fatalf("store the timeline rows: %v", err)
	}
	config := pool.Config()
	config.MaxConns, config.MinConns, config.MinIdleConns = 1, 0, 0
	opened, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open a pool of one connection: %v", err)
	}
	t.Cleanup(opened.Close)
	single := store.NewPool(opened)
	statement := attributionStatementsFor(readingSchema(t, ctx, pool), refs.Renames{}).pullRequestIssues
	plans := func() (generic, custom int64) {
		t.Helper()
		if err := single.QueryRow(ctx, `select coalesce(sum(generic_plans), 0), coalesce(sum(custom_plans), 0)
			from pg_prepared_statements where statement = $1`, statement).Scan(&generic, &custom); err != nil {
			t.Fatalf("read the plans of the prepared statement: %v", err)
		}
		return generic, custom
	}

	ten := make([]PullRequestKey, 10)
	for i := range ten {
		ten[i] = PullRequestKey{Repo: "acme/widgets", Number: i + 1}
	}
	for range 10 {
		if _, err := unrenamed().PullRequestIssues(ctx, single, ten); err != nil {
			t.Fatal(err)
		}
	}
	generic, custom := plans()
	t.Logf("ten reads of ten pull requests: %d generic and %d custom plans of the prepared statement", generic, custom)
	if generic+custom != 10 {
		t.Fatalf("control: ten reads of ten pull requests ran %d generic and %d custom plans of the prepared statement, want ten in all: the counts read are not the statement's", generic, custom)
	}

	at := merged.Add(time.Hour)
	window, err := unrenamed().ListPullRequestsInWindow(ctx, single, merged, at)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := unrenamed().ListPullRequestsWaitingAt(ctx, single, "acme/widgets", Production{JobNames: []string{"deploy"}}, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(window) != 300 || len(waiting) != 300 {
		t.Fatalf("the window lists %d pull requests and the waiting read %d, want 300 each", len(window), len(waiting))
	}
	afterGeneric, afterCustom := plans()
	if afterGeneric != generic || afterCustom != custom {
		t.Errorf("the window's read and the waiting read of 300 pull requests ran %d generic and %d custom plans of the connection's prepared statement, want none: each is planned for its own pull requests",
			afterGeneric-generic, afterCustom-custom)
	}
}

// widgetsRenames is the rename pairs of the plan tests below: oldco/widgets renamed to
// acme/widgets, and gone/r<n> renamed to kept/r<n> for each n from 1 to others, as the boot reads
// them (refs.NewRenames).
func widgetsRenames(others int) refs.Renames {
	pairs := []store.RepoRename{{From: "oldco/widgets", To: "acme/widgets"}}
	for n := 1; n <= others; n++ {
		pairs = append(pairs, store.RepoRename{From: fmt.Sprintf("gone/r%d", n), To: fmt.Sprintf("kept/r%d", n)})
	}
	return refs.NewRenames(pairs)
}

// renameTermLoops is the loops of the nodes of a plan that scan the statement's terms of the
// renaming rule (renameTerms: renames and renamed_names), and how many such nodes there are.
func renameTermLoops(plan []explaintest.Node) (loops float64, nodes int) {
	for _, node := range plan {
		if node.CTE == "renames" || node.CTE == "renamed_names" {
			loops += node.Loops
			nodes++
		}
	}
	return loops, nodes
}

// The statement reads the rename pairs once, however many there are (renameTerms): they are
// literals of its text, one materialized relation, and no node of its plan reads repo_projects.
// Over 20 pull requests with texts, the loops of the nodes that scan that relation add up to the
// same count with 2 pairs and with 41. Where each renamed name's canonical name was compared for
// every pull request, the loops of the nodes that read the pairs grew by 20 for each renamed
// repository, and the statement over 10 pull requests took 996 ms with 500 of them. The controls
// in the same test: the plan scans the relation in some node, the statement answers each of the
// 20, and the links of a pull request still hold its forms under the name renamed to its
// repository.
func TestTheStatementReadsTheRenamesOnceForEachPullRequestWhateverTheirNumber(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	storeTextsRows(t, ctx, pool, 1, 20)
	schema := readingSchema(t, ctx, pool)
	repos := make([]string, 20)
	numbers := make([]int, 20)
	for i := range repos {
		repos[i], numbers[i] = "acme/widgets", i+1
	}
	loops := func(renames refs.Renames, what string) float64 {
		t.Helper()
		statement := attributionStatementsFor(schema, renames).pullRequestIssues
		if strings.Contains(statement, "repo_projects") {
			t.Fatalf("the statement %s names repo_projects: the pairs are literals of its text", what)
		}
		var raw []byte
		if err := pool.QueryRow(ctx, `explain (analyze, format json) `+statement, repos, numbers).Scan(&raw); err != nil {
			t.Fatalf("explain the statement %s: %v", what, err)
		}
		plan := explaintest.Nodes(t, explaintest.OnePlan(t, raw))
		if root := plan[0]; root.Rows != 20 {
			t.Fatalf("the statement %s answers %v pull requests, want the 20", what, root.Rows)
		}
		for _, node := range plan {
			if node.Relation == "repo_projects" {
				t.Errorf("the statement %s reads repo_projects in %s", what, node)
			}
		}
		sum, _ := renameTermLoops(plan)
		return sum
	}

	few := loops(widgetsRenames(1), "with 2 renamed repositories")
	many := loops(widgetsRenames(40), "with 41 renamed repositories")
	t.Logf("loops of the nodes that scan the rename terms: %v with 2 renamed repositories, %v with 41", few, many)
	if few == 0 {
		t.Fatalf("control: the plan scans the rename terms in no node, so its loops say nothing")
	}
	if many != few {
		t.Errorf("the nodes that scan the rename terms ran %v loops with 41 renamed repositories and %v with 2, want the same: the statement reads the renames once for each pull request", many, few)
	}

	var links []string
	if err := pool.QueryRow(ctx, `select `+PullRequestLinksSQL(widgetsRenames(40), "$1::text", "$2::int"), "acme/widgets", 7).Scan(&links); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprint([]string{"https://github.com/acme/widgets/issues/7", "https://github.com/acme/widgets/pull/7",
		"https://github.com/oldco/widgets/issues/7", "https://github.com/oldco/widgets/pull/7"})
	if got := fmt.Sprint(slices.Sorted(slices.Values(links))); got != want {
		t.Errorf("the links of acme/widgets#7 are %s, want %s", got, want)
	}
}

// api's link check, its lock and the lookup by reference read the links of one pull request
// through PullRequestLinksSQL, which takes the names of its repository from the renaming rule's
// inverse over the pairs the boot read (refs.NamesSQL): the array holds the forms of the pull
// request under its canonical name and under each old name of a pair renamed to it, and under no
// other pair's name, with 41 pairs as with 2; asked by an old name, it answers the forms of the
// canonical name's pull request. The pairs are literals of the text, so the statement reads no
// table: no node of its plan reads a relation. The control in the same test: the same walk of a
// plan that reads issue_external_links finds that relation, so a plan that read one would be
// seen.
func TestPullRequestLinksSQLReadsOnlyTheRenamesOfItsRepository(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	forms := func(names ...string) string {
		var all []string
		for _, name := range names {
			all = append(all, "https://github.com/"+name+"/issues/7", "https://github.com/"+name+"/pull/7")
		}
		return fmt.Sprint(slices.Sorted(slices.Values(all)))
	}
	relations := func(what, sql string, args ...any) []string {
		t.Helper()
		var raw []byte
		if err := pool.QueryRow(ctx, `explain (analyze, format json) `+sql, args...).Scan(&raw); err != nil {
			t.Fatalf("explain %s: %v", what, err)
		}
		var read []string
		for _, node := range explaintest.Nodes(t, explaintest.OnePlan(t, raw)) {
			if node.Relation != "" {
				read = append(read, node.Relation)
			}
		}
		return read
	}
	if read := relations("the control, a read of the links", `select count(*) from issue_external_links`); !slices.Contains(read, "issue_external_links") {
		t.Fatalf("control: the plan of a read of issue_external_links reads %v, so the walk does not see a relation read", read)
	}
	for _, others := range []int{1, 40} {
		renames := widgetsRenames(others)
		what := fmt.Sprintf("with %d renamed repositories", others+1)
		query := `select ` + PullRequestLinksSQL(renames, "$1::text", "$2::int")
		for repo, want := range map[string]string{
			"acme/widgets":  forms("acme/widgets", "oldco/widgets"),
			"Oldco/Widgets": forms("acme/widgets", "oldco/widgets"),
			"kept/r1":       forms("kept/r1", "gone/r1"),
			"acme/gadgets":  forms("acme/gadgets"),
		} {
			var links []string
			if err := pool.QueryRow(ctx, query, repo, 7).Scan(&links); err != nil {
				t.Fatalf("read the links of %s#7 %s: %v", repo, what, err)
			}
			if got := fmt.Sprint(slices.Sorted(slices.Values(links))); got != want {
				t.Errorf("the links of %s#7 %s are %s, want %s", repo, what, got, want)
			}
		}
		if read := relations("the links of acme/widgets#7 "+what, query, "acme/widgets", 7); len(read) != 0 {
			t.Errorf("the links of acme/widgets#7 %s read the relations %v, want none: the pairs are literals of the text", what, read)
		}
	}
}

// The statement reads the renaming rule once, however many pull requests it judges
// (renameTerms): the nodes that scan its terms of the rule run as many loops for 20 pull requests
// as for one. Where every comparison of a name read the rule from repo_projects, a scan of the
// table for each name, those nodes ran about 12 loops a pull request, 240 for 20, and a window
// read of 1,000 pull requests on the 43 mappings production held on 22 November spent about 40 ms
// of its 170 ms in them; where the subquery run for each cited URL joined renames
// (citedPullRequest), a nested loop rescanned it for each URL, 47 loops for 20 and 9 for one. The
// controls in the same test: the plan scans the terms in some node, the statement answers each
// pull request it was given, and the same count over a read of the relation once for each of 20
// names counts 20 loops, so the count sees a read made once a row.
func TestTheStatementReadsTheRenamingRuleOnceWhateverItsPullRequests(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	storeTextsRows(t, ctx, pool, 1, 20)
	renames := widgetsRenames(1)
	statement := attributionStatementsFor(readingSchema(t, ctx, pool), renames).pullRequestIssues
	explain := func(what, sql string, args ...any) []explaintest.Node {
		t.Helper()
		var raw []byte
		if err := pool.QueryRow(ctx, `explain (analyze, format json) `+sql, args...).Scan(&raw); err != nil {
			t.Fatalf("explain %s: %v", what, err)
		}
		return explaintest.Nodes(t, explaintest.OnePlan(t, raw))
	}
	read := func(count int) float64 {
		t.Helper()
		repos := make([]string, count)
		numbers := make([]int, count)
		for i := range repos {
			repos[i], numbers[i] = "acme/widgets", i+1
		}
		what := fmt.Sprintf("the statement over %d pull requests", count)
		plan := explain(what, statement, repos, numbers)
		if plan[0].Rows != float64(count) {
			t.Fatalf("%s answers %v of them, want each", what, plan[0].Rows)
		}
		loops, nodes := renameTermLoops(plan)
		if nodes == 0 {
			t.Fatalf("control: %s scans the terms of the renaming rule in no node, so its loops say nothing", what)
		}
		return loops
	}

	one, twenty := read(1), read(20)
	t.Logf("loops of the nodes that scan the terms of the renaming rule: %v for 1 pull request, %v for 20", one, twenty)
	if twenty != one {
		t.Errorf("the nodes that scan the terms of the renaming rule ran %v loops for 20 pull requests and %v for one, want the same: the statement reads the rule once, not once for each pull request", twenty, one)
	}

	names := make([]string, 20)
	for i := range names {
		names[i] = fmt.Sprintf("acme/widgets-%d", i)
	}
	control, _ := renameTermLoops(explain("a read of the relation for each of 20 names",
		`with renames as materialized (`+refs.RenamesSQL(renames)+`)
		select (select r.rename_target from renames r where r.rename_source = lower(k.name)) from unnest($1::text[]) k(name)`, names))
	if control != 20 {
		t.Fatalf("control: a read of the renames relation for each of 20 names counts %v loops, want 20: the count does not see a read made once a row", control)
	}
}
