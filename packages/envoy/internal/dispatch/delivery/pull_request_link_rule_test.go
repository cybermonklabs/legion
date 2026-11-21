package delivery

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// linkCorpus is the links the tests of the link rule read, one shape each: links of a pull
// request the rule takes, in each form, letter case and name, and the near misses it refuses.
// The last is a host spelled with U+0130, which lower() folds to the ASCII i: only the rule's
// test that every character is ASCII refuses it, in SQL as in Go.
var linkCorpus = []string{
	"https://github.com/acme/widgets/pull/101",
	"https://github.com/acme/widgets/pull/102",
	"https://GITHUB.COM/Acme/Widgets/PULL/103",
	"https://github.com/Acme/Widgets/pull/104",
	"https://github.com/oldco/widgets/pull/105",
	"https://github.com/oldco/widgets/pull/106",
	"https://github.com/acme/widgets/pull/107",
	"https://github.com/OLDCO/Widgets/pull/108",
	"https://github.com/badrename/x/pull/109",
	"https://github.com/acme/widgets/pull/1000000000",
	"https://github.com/acme/widgets/pull/2147483647",
	"https://github.com/acme/widgets/pull/2147483648",
	"https://github.com/acme/widgets/pull/3000000000",
	"https://github.com/acme/widgets/pull/99999999999999999999",
	"https://github.com/acme/.git/pull/112",
	"https://github.com/acme/x.git/pull/113",
	"https://github.com/acme/..git/pull/114",
	"https://github.com/acme/widgets/pull/115/",
	"https://github.com/acme/widgets/pull/116?x=1",
	"https://github.com/acme/widgets/pull/117#discussion",
	"https://www.github.com/acme/widgets/pull/118",
	"http://github.com/acme/widgets/pull/119",
	"https://github.com/acme/widgets/pull/+120",
	"https://github.com/acme/widgets/issues/121",
	"https://github.com/ACME/widgets/pull/122",
	"https://github.com/acme/WIDGETS/pull/122",
	"https://github.com/Acme/Widgets/pull/123",
	"https://github.com/acme/widgets/pull/124",
	"https://github.com/acme/w\u00eddgets/pull/125",
	"https://github.com/acme/wor\u212aflows/pull/125",
	"https://github.com/acme/widgets/pull/126\n",
	"https://github.com/" + strings.Repeat("o", 39) + "/" + strings.Repeat("n", 100) + "/pull/127",
	"https://github.com/acme/" + strings.Repeat("n", 101) + "/pull/128",
	"https://github.com/" + strings.Repeat("o", 40) + "/widgets/pull/128",
	"https://github.com/-acme/widgets/pull/129",
	"https://github.com/acme_/w/pull/130",
	"https://github.com/acme/widgets/pull/0",
	"https://github.com//widgets/pull/131",
	"https://github.com/acme/widgets/pull/132 ",
	"HTTPS://github.com/acme/widgets/pull/133",
	"https://github.com/acme/widgets/Pull/134",
	"https://github.com/acme/widgets/pulls/135",
	"https://github.com/acme/widgets/IssueS/136",
	"https://github.com/oldco/widgets/pull/137",
	"https://github.com/acme/widgets/pull/0138",
	"https://github.com/acme/./pull/139",
	"https://github.com/acme/../pull/141",
	"https://github.com/gone/repo/pull/140",
	"HTTPS://GitHub.COM/acme/widgets/PULL/142",
	"https://github.com/acme/gadgets/pull/143",
	"https://github.com/acme/widgets/pull/144/files",
	"https://github.com/acme/widgets",
	"https://github.com/",
	"github.com/acme/widgets/pull/145",
	"https://g\u0130thub.com/acme/widgets/pull/146",
}

// The link rule in SQL is the link rule in Go: for each link of the corpus, LinkedPullRequestSQL
// gives the repository and the number linkedPullRequest gives, or no row where linkedPullRequest
// takes no pull request from it. And each link the rule takes is among the links of its pull
// request under the canonical name (PullRequestLinksSQL), which oldco/widgets, renamed to
// acme/widgets, makes the old name's links of acme/widgets' pull requests, and no link it refuses
// is. The controls are in the corpus: a link of each shape the rule refuses, which must give no
// row.
func TestTheLinkRuleInSQLIsTheLinkRuleInGo(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliveryProject(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		insert into repo_projects (repo, project, created_by, is_default, renamed_to)
		values ('oldco/widgets', $1, '{"kind":"system","id":"delivery-test"}'::jsonb, false, 'acme/widgets')
		on conflict (repo) do update set renamed_to = excluded.renamed_to
	`, deliveryProject); err != nil {
		t.Fatalf("record oldco/widgets as renamed to acme/widgets: %v", err)
	}
	taken, refused := 0, 0
	for _, link := range linkCorpus {
		repo, number, ok := linkedPullRequest(link)
		var sqlRepo string
		var sqlNumber int
		err := pool.QueryRow(ctx, `select lp.repo, lp.number from (select $1::text as u) x cross join lateral `+
			LinkedPullRequestSQL("x.u")+` lp`, link).Scan(&sqlRepo, &sqlNumber)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if ok {
				t.Errorf("%q: Go takes %s#%d, SQL takes no pull request", link, repo, number)
			}
			refused++
			continue
		case err != nil:
			t.Fatalf("%q: %v", link, err)
		}
		if !ok {
			t.Errorf("%q: SQL takes %s#%d, Go takes no pull request", link, sqlRepo, sqlNumber)
			continue
		}
		if sqlRepo != repo || sqlNumber != number {
			t.Errorf("%q: SQL takes %s#%d, Go takes %s#%d", link, sqlRepo, sqlNumber, repo, number)
		}
		taken++
		owed, err := owedName(ctx, pool, repo, number)
		if err != nil {
			t.Fatal(err)
		}
		var listed bool
		if err := pool.QueryRow(ctx, `select lower($1::text) = any(`+PullRequestLinksSQL("$2::text", "$3::int")+`)`,
			link, owed, number).Scan(&listed); err != nil {
			t.Fatalf("%q: %v", link, err)
		}
		if !listed {
			t.Errorf("%q names %s#%d, and is not among the links of %s#%d", link, owed, number, owed, number)
		}
	}
	t.Logf("the corpus holds %d links the rule takes and %d it refuses", taken, refused)
	if taken == 0 || refused == 0 {
		t.Fatalf("the corpus holds %d links the rule takes and %d it refuses; each must hold some", taken, refused)
	}

	var links []string
	if err := pool.QueryRow(ctx, `select `+PullRequestLinksSQL("'acme/widgets'", "105")).Scan(&links); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprint([]string{"https://github.com/acme/widgets/issues/105", "https://github.com/acme/widgets/pull/105",
		"https://github.com/oldco/widgets/issues/105", "https://github.com/oldco/widgets/pull/105"})
	if got := fmt.Sprint(slices.Sorted(slices.Values(links))); got != want {
		t.Errorf("the links of acme/widgets#105 are %s, want %s", got, want)
	}
}
