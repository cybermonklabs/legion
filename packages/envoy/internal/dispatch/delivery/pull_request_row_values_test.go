package delivery

// The check of an envelope's name (pullRequestNamed) is the one thing that keeps an envelope with
// values no row can hold out of the hand-back, since no failure of the owed record's statement
// ends an envelope (TestAnOwedRecordRefusedForAnyCauseLosesNoHead). So it must take nothing the
// tables refuse, and every row of pull_request_reads_owed and pull_request_heads must be built
// from values it takes, whichever caller builds it and wherever the value comes from.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Every value the check takes at its bounds is a row of both tables on a real Postgres: an owner
// of 39 characters, a name of 100, both at once, each character class GitHub's names hold at the
// first and the last place of the owner and of the name, and the first and the largest number.
// Every value just past a bound is refused where the row is built, before any statement, with the
// check's own error, so no caller stores a row of it and no failure of Postgres stands in for the
// check: an owner of 40, a name of 101, number 0 and 2147483648, "." and "..", a character no name
// holds, a third segment. The rows that reach the owed table through a renamed repository
// (renamedFrom) read the repository's canonical name from repo_projects, which a target stored
// before the settings route held it to the name rule (api's parseRepoRename), or by direct SQL,
// holds to nothing, so that name is checked where the row is built too: a rename to a name of 101
// characters, or of 3,500, which the btree of the owed table refused (SQLSTATE 54000), reaches no
// statement. The read is owed under the name given, which the check takes, so the reconcile reads
// it once the target is corrected (acceptance of cybermonklabs/legion#4209 at 3a21edb2c9, D1), and
// the head row of the refused name is refused where it is built.
func TestEachValueTheNameCheckTakesIsARowTheTablesTake(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	const (
		largest = 2147483647
		sha     = "4209420942094209420942094209420942094209"
	)
	for i, row := range []struct {
		name string
		// repo is the name that reaches the rows: the owed read's canonical name and the head
		// row's repository.
		repo   string
		number int
		// renamedFrom, where set, is the name the owed read is recorded under, which repo_projects
		// maps to repo.
		renamedFrom string
		// bound is why the check refuses the row, "" for a row it takes.
		bound string
	}{
		{name: "an_owner_of_39", repo: strings.Repeat("o", 39) + "/widgets", number: 1},
		{name: "a_name_of_100", repo: "acme/" + strings.Repeat("n", 100), number: 2},
		{name: "an_owner_of_39_and_a_name_of_100_and_the_largest_number", repo: strings.Repeat("o", 39) + "/" + strings.Repeat("n", 100), number: largest},
		{name: "the_first_number", repo: "acme/widgets", number: 1},
		{name: "the_largest_number", repo: "acme/widgets", number: largest},
		{name: "an_owner_upper_case_at_both_ends", repo: "AcmE/widgets", number: 3},
		{name: "an_owner_lower_case_at_both_ends", repo: "acme/widgets", number: 4},
		{name: "an_owner_digits_at_both_ends", repo: "0acme9/widgets", number: 5},
		{name: "an_owner_underscores_at_both_ends", repo: "_acme_/widgets", number: 6},
		{name: "an_owner_hyphens_at_both_ends", repo: "-acme-/widgets", number: 7},
		{name: "a_name_upper_case_at_both_ends", repo: "acme/WidgetS", number: 8},
		{name: "a_name_digits_at_both_ends", repo: "acme/0widgets9", number: 9},
		{name: "a_name_periods_at_both_ends", repo: "acme/.widgets.", number: 10},
		{name: "a_name_underscores_at_both_ends", repo: "acme/_widgets_", number: 11},
		{name: "a_name_hyphens_at_both_ends", repo: "acme/-widgets-", number: 12},
		{name: "a_rename_to_a_name_of_100", repo: "acme/" + strings.Repeat("r", 100), number: 13, renamedFrom: renamedAwayRepo},
		{name: "an_owner_of_40", repo: strings.Repeat("o", 40) + "/widgets", number: 20, bound: "39 is GitHub's limit for a login"},
		{name: "a_name_of_101", repo: "acme/" + strings.Repeat("n", 101), number: 21, bound: "100 is GitHub's limit for a repository name"},
		{name: "number_0", repo: "acme/widgets", number: 0, bound: "GitHub numbers pull requests from 1"},
		{name: "number_2147483648", repo: "acme/widgets", number: largest + 1, bound: "2147483647 is the largest value of the integer columns that key a pull request"},
		{name: "a_name_of_one_period", repo: "acme/.", number: 22, bound: `"." is a path segment, no repository name`},
		{name: "a_name_of_two_periods", repo: "acme/..", number: 23, bound: `".." is a path segment, no repository name`},
		{name: "a_period_in_the_owner", repo: "ac.me/widgets", number: 24, bound: "a login holds no period"},
		{name: "a_space_in_the_name", repo: "acme/wid gets", number: 25, bound: "a repository name holds letters, digits, hyphens, underscores and periods alone"},
		{name: "a_letter_outside_ascii_in_the_name", repo: "acme/widgéts", number: 26, bound: "a repository name holds ASCII letters alone"},
		{name: "a_nul_in_the_name", repo: "acme/wid\x00gets", number: 27, bound: "a repository name holds letters, digits, hyphens, underscores and periods alone, and Postgres text holds no NUL"},
		{name: "a_third_segment", repo: "acme/widgets/pulls", number: 28, bound: "a full name is owner/name"},
		{name: "a_rename_to_a_name_of_101", repo: "acme/" + strings.Repeat("r", 101), number: 29, renamedFrom: renamedAwayRepo, bound: "100 is GitHub's limit for a repository name"},
		{name: "a_rename_to_a_name_of_3500", repo: "acme/" + randomName(3500), number: 30, renamedFrom: renamedAwayRepo, bound: "100 is GitHub's limit for a repository name, and the btree of the owed table holds no entry past 2,704 bytes"},
	} {
		t.Run(row.name, func(t *testing.T) {
			owedUnder := row.repo
			if row.renamedFrom != "" {
				owedUnder = row.renamedFrom
				renameRepository(t, ctx, pool, row.renamedFrom, row.repo)
			}
			_, _, checked := pullRequestNamed(map[string]string{"repo": row.repo, "number": strconv.Itoa(row.number)})
			takes := checked == nil
			if takes != (row.bound == "") {
				t.Errorf("the check of %.50q#%d = %v, want %s", row.repo, row.number, checked,
					map[bool]string{true: "taken", false: "refused: " + row.bound}[row.bound == ""])
			}

			owedBefore, headsBefore := tableRows(t, pool, "pull_request_reads_owed"), tableRows(t, pool, "pull_request_heads")
			oweErr := owePullRequestRead(ctx, pool, owedUnder, row.number, owedFailure, "")
			owed := tableRows(t, pool, "pull_request_reads_owed") - owedBefore
			headErr := UpsertPullRequestHead(ctx, pool, PullRequestHead{
				Repo: row.repo, Number: row.number, HeadSHA: sha, HeadRef: "topic", BaseRef: "main", State: "open",
				Title: "Change " + strconv.Itoa(i), URL: "https://github.com/acme/widgets/pull/" + strconv.Itoa(i),
			})

			heads := tableRows(t, pool, "pull_request_heads") - headsBefore

			for _, write := range []struct {
				what string
				err  error
				rows int
				// takes is whether the check takes the row's name: an owed read of a renamed
				// repository is stored under the name given where the canonical one is refused.
				takes bool
			}{{"owed read", oweErr, owed, takes || row.renamedFrom != ""}, {"head row", headErr, heads, takes}} {
				if write.takes {
					if write.err != nil || write.rows != 1 {
						t.Errorf("the check takes %.50q#%d, and its %s was not stored (%d rows written): %v", row.repo, row.number, write.what, write.rows, write.err)
					}
					continue
				}
				var postgres *pgconn.PgError
				if !errors.Is(write.err, errNoPullRequestNamed) || errors.As(write.err, &postgres) || write.rows != 0 {
					t.Errorf("the check refuses %.50q#%d (%s), and its %s was not refused where it was built, before any statement: %d rows written, error %v", row.repo, row.number, row.bound, write.what, write.rows, write.err)
				}
			}
			if row.renamedFrom != "" && !takes {
				var under string
				if err := pool.QueryRow(ctx, `select repo from pull_request_reads_owed where number = $1`, row.number).Scan(&under); err != nil || under != row.renamedFrom {
					t.Errorf("the read of %s#%d, renamed to the refused %.50q, is owed under %q (%v), want under the name given, %s", row.renamedFrom, row.number, row.repo, under, err, row.renamedFrom)
				}
			}
		})
	}
}

// A head row holds every value an index of the write can hold, and refuses one past its bound
// where the row is built, before any statement (checkHeadRow, headRefBound). The values an index
// holds are the head commit (pull_request_heads_repo_head), the URL (pull_request_heads_url_lower)
// and, for a merge, the base branch and the merge commit (branch_commits' primary key), so each
// is taken at 1,024 bytes and refused at 1,025 with errNoRowHolds and no row written; the widest
// entry, a repository of 140 bytes with a base branch and a merge commit of 1,024 each, is
// written. The head branch is held by no index, so an honest one of 5,000 bytes is written. A
// title is a text of free form: one with a NUL is written, with U+FFFD in place of the NUL. The
// values are random letters and digits, which Postgres does not compress (randomName). At
// ef5112db43 no value was bounded, so each value past its bound was written; at def907052c the
// URL was bounded by nothing, so the URL of 1,025 bytes was written.
func TestAHeadRowTakesEveryValueAnIndexCanHoldAndRefusesOnePast(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	short := "4209420942094209420942094209420942094209"
	// The bound's own numbers, not headRefBound: a test that read the constant would move with it.
	at, past := randomName(1024), randomName(1025)
	longest := strings.Repeat("o", 39) + "/" + strings.Repeat("n", 100)
	urlOf := func(n int) string {
		prefix := "https://github.com/acme/widgets/pull/1?"
		return prefix + randomName(n-len(prefix))
	}
	for i, row := range []struct {
		name                  string
		repo                  string
		head, base, merge     string
		headRef, title, url   string
		merged, takes, seeded bool
	}{
		{name: "a_head_commit_at_the_bound", repo: "acme/widgets", head: at, base: "main", headRef: "topic", takes: true},
		{name: "a_base_branch_and_a_merge_commit_at_the_bound_with_the_longest_repository", repo: longest, head: short, base: at, merge: at, headRef: "topic", merged: true, takes: true, seeded: true},
		{name: "an_honest_head_branch_of_5000_bytes", repo: "acme/widgets", head: short, base: "main", headRef: randomName(5000), takes: true},
		{name: "a_url_at_the_bound", repo: "acme/widgets", head: short, base: "main", headRef: "topic", url: urlOf(1024), takes: true},
		{name: "a_title_with_a_nul", repo: "acme/widgets", head: short, base: "main", headRef: "topic", title: "Change\x00 of the notes", takes: true},
		{name: "a_head_commit_one_past_the_bound", repo: "acme/widgets", head: past, base: "main", headRef: "topic"},
		{name: "a_base_branch_one_past_the_bound", repo: "acme/widgets", head: short, base: past, merge: short, headRef: "topic", merged: true},
		{name: "a_merge_commit_one_past_the_bound", repo: "acme/widgets", head: short, base: "main", merge: past, headRef: "topic", merged: true},
		{name: "a_url_one_past_the_bound", repo: "acme/widgets", head: short, base: "main", headRef: "topic", url: urlOf(1025)},
	} {
		t.Run(row.name, func(t *testing.T) {
			state := "open"
			if row.merged {
				state = "merged"
			}
			title, url := row.title, row.url
			if title == "" {
				title = "Change " + strconv.Itoa(700+i)
			}
			if url == "" {
				url = "https://github.com/acme/widgets/pull/" + strconv.Itoa(700+i)
			}
			heads, seeds := tableRows(t, pool, "pull_request_heads"), tableRows(t, pool, "branch_commits")
			err := UpsertPullRequestHead(ctx, pool, PullRequestHead{
				Repo: row.repo, Number: 700 + i, HeadSHA: row.head, HeadRef: row.headRef, BaseRef: row.base,
				State: state, MergeCommitSHA: row.merge, Title: title, URL: url,
			})

			wroteHead, wroteSeed := tableRows(t, pool, "pull_request_heads")-heads, tableRows(t, pool, "branch_commits")-seeds
			if row.takes {
				if want := map[bool]int{true: 1, false: 0}[row.seeded]; err != nil || wroteHead != 1 || wroteSeed != want {
					t.Errorf("the head row was not stored: %d head rows and %d branch rows written (want 1 and %d): %v", wroteHead, wroteSeed, want, err)
				}
				want := strings.ReplaceAll(title, "\x00", "\uFFFD")
				var stored string
				if err := pool.QueryRow(ctx, `select title from pull_request_heads where repo = $1 and number = $2`, row.repo, 700+i).Scan(&stored); err != nil || stored != want {
					t.Errorf("the stored title = %q (%v), want %q", stored, err, want)
				}
				return
			}
			var postgres *pgconn.PgError
			if !errors.Is(err, errNoRowHolds) || errors.As(err, &postgres) || wroteHead != 0 || wroteSeed != 0 {
				t.Errorf("a value of 1,025 bytes, one past the bound, was not refused where the row is built, before any statement: %d head rows and %d branch rows written, error %v", wroteHead, wroteSeed, err)
			}
		})
	}
}

// tableRows is the number of rows table holds.
func tableRows(t *testing.T, pool *store.Pool, table string) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(t.Context(), "select count(*) from "+table).Scan(&rows); err != nil {
		t.Fatalf("count the rows of %s: %v", table, err)
	}
	return rows
}

// renamedAwayRepo is the repository the tests that need a rename map to a canonical name of their
// own (renameRepository).
const renamedAwayRepo = "acme/renamed-away-4209"

// renameRepository maps from to the canonical name to in repo_projects, under deliveryProject, as
// direct SQL or the settings route writes a rename, until the test ends. The table outlives the
// run and no truncate of deliveryTestPool clears it, so the mapping is deleted at the end.
func renameRepository(t *testing.T, ctx context.Context, pool *store.Pool, from, to string) {
	t.Helper()
	seedDeliveryProject(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		insert into repo_projects (repo, project, created_by, is_default, renamed_to)
		values ($1, $2, '{"kind":"system","id":"delivery-test"}'::jsonb, false, $3)
		on conflict (repo) do update set renamed_to = excluded.renamed_to
	`, from, deliveryProject, to); err != nil {
		t.Fatalf("map %s to %.50q: %v", from, to, err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.WithoutCancel(ctx), `delete from repo_projects where repo = $1`, from); err != nil {
			t.Errorf("drop the mapping of %s: %v", from, err)
		}
	})
}
