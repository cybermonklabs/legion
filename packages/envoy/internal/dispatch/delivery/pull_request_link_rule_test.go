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
// #146 is a host spelled with U+0130, which lower() folds to the ASCII i: only the rule's test
// that every character is ASCII refuses it, in SQL as in Go. #147 to #149 are owners and names no
// repository of GitHub can have (refs.SplitGitHubRepo).
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
	"https://github.com/_acme/widgets/pull/147",
	"https://github.com/acme/...git/pull/148",
	"https://github.com/acme/.GIT/pull/149",
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
	renameOldcoWidgets(t, ctx, pool)
	renames := renamesOf(t, ctx, pool)
	attributor := NewAttributor(renames)
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
		owed, err := attributor.owedName(repo, number)
		if err != nil {
			t.Fatal(err)
		}
		var listed bool
		if err := pool.QueryRow(ctx, `select lower($1::text) = any(`+PullRequestLinksSQL(renames, "$2::text", "$3::int")+`)`,
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
	if err := pool.QueryRow(ctx, `select `+PullRequestLinksSQL(renames, "'acme/widgets'", "105")).Scan(&links); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprint([]string{"https://github.com/acme/widgets/issues/105", "https://github.com/acme/widgets/pull/105",
		"https://github.com/oldco/widgets/issues/105", "https://github.com/oldco/widgets/pull/105"})
	if got := fmt.Sprint(slices.Sorted(slices.Values(links))); got != want {
		t.Errorf("the links of acme/widgets#105 are %s, want %s", got, want)
	}
}

// A link of a repository GitHub cannot have names no pull request, by the rule every read of
// GitHub splits a name by (refs.SplitGitHubRepo): an owner that starts with `-` or `_`, and a name
// that is empty, `.` or `..` once it is lowercased and one trailing `.git` is trimmed. No read can
// reach such a repository, so the link write owes no read of it that each pass would make and
// refuse (review N5 of #4209 at 4ccee6c7e6). The controls are names GitHub has: a name that keeps
// a name once `.git` is trimmed, an owner that ends in `_`, and a managed user's login.
func TestALinkOfARepositoryGitHubCannotHaveNamesNoPullRequest(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	for link, want := range map[string]string{
		"https://github.com/-acme/widgets/pull/129":       "",
		"https://github.com/_acme/widgets/pull/147":       "",
		"https://github.com/acme/.git/pull/112":           "",
		"https://github.com/acme/..git/pull/114":          "",
		"https://github.com/acme/...git/issues/148":       "",
		"https://github.com/acme/.GIT/pull/149":           "",
		"https://github.com/acme/x.git/pull/113":          "acme/x.git#113",
		"https://github.com/acme_/w/pull/130":             "acme_/w#130",
		"https://github.com/mona-cat_octo/widgets/pull/7": "mona-cat_octo/widgets#7",
	} {
		got := ""
		if repo, number, ok := linkedPullRequest(link); ok {
			got = fmt.Sprintf("%s#%d", repo, number)
		}
		if got != want {
			t.Errorf("%s: Go takes %q, want %q", link, got, want)
		}
		var sqlRepo string
		var sqlNumber int
		err := pool.QueryRow(ctx, `select lp.repo, lp.number from (select $1::text as u) x cross join lateral `+
			LinkedPullRequestSQL("x.u")+` lp`, link).Scan(&sqlRepo, &sqlNumber)
		sqlGot := ""
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			t.Fatalf("%s: %v", link, err)
		default:
			sqlGot = fmt.Sprintf("%s#%d", sqlRepo, sqlNumber)
		}
		if sqlGot != want {
			t.Errorf("%s: SQL takes %q, want %q", link, sqlGot, want)
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := unrenamed().OweLinkedPullRequestReads(ctx, tx, []string{"https://github.com/-acme/widgets/pull/129",
		"https://github.com/acme/.git/pull/112", "https://github.com/acme/..git/pull/114"}); err != nil {
		t.Fatal(err)
	}
	var owed int
	if err := tx.QueryRow(ctx, `select count(*) from pull_request_reads_owed`).Scan(&owed); err != nil {
		t.Fatal(err)
	}
	if owed != 0 {
		t.Errorf("the link write of three links of repositories GitHub cannot have owes %d reads, want none", owed)
	}
}
