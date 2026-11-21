package delivery

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// linkBackfillMigration is the migration that owes, once, the read of each pull request a link
// names whose head row Dispatch does not hold (20261120171308_pull_request_reads_owed_link_backfill).
const linkBackfillMigration = 20261120171308

// runLinkBackfill applies the backfill as the runner applies it: its version is forgotten and the
// store migrates, which applies that version alone, in one transaction with its record in
// schema_migrations (store.applyMigrationOnce), as a deployment's boot applies it once.
func runLinkBackfill(t *testing.T, ctx context.Context, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(ctx, `delete from schema_migrations where version = $1`, linkBackfillMigration); err != nil {
		t.Fatalf("forget version %d: %v", linkBackfillMigration, err)
	}
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("apply version %d: %v", linkBackfillMigration, err)
	}
}

// owedLinkReads is every owed read of the table as `repo#number reason`, with ` link` where the
// row's linked_revision is its revision, in order.
func owedLinkReads(t *testing.T, ctx context.Context, pool *store.Pool) []string {
	t.Helper()
	return owedRowsAs(t, ctx, pool, `repo || '#' || number || ' ' || reason || case when linked_revision = revision then ' link' else '' end`)
}

// owedRows is every owed read of the table as `repo#number reason reads=N refused_as=W
// refused_since=set|none`, with ` link` where the row's linked_revision is its revision, in
// order: each column a link write and the backfill both decide, and neither the revision's
// value nor owed_at, which a link write moves on a row owed already and the backfill leaves.
func owedRows(t *testing.T, ctx context.Context, pool *store.Pool) []string {
	t.Helper()
	return owedRowsAs(t, ctx, pool, `repo || '#' || number || ' ' || reason || ' reads=' || reads
		|| ' refused_as=' || coalesce(refused_as, 'none') || ' refused_since=' || case when refused_since is null then 'none' else 'set' end
		|| case when linked_revision = revision then ' link' else '' end`)
}

// owedStamps is every owed read of the table as `repo#number revision owed_at`, in order.
func owedStamps(t *testing.T, ctx context.Context, pool *store.Pool) []string {
	t.Helper()
	return owedRowsAs(t, ctx, pool, `repo || '#' || number || ' ' || revision || ' ' || owed_at::text`)
}

func owedRowsAs(t *testing.T, ctx context.Context, pool *store.Pool, row string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `select `+row+` from pull_request_reads_owed`)
	if err != nil {
		t.Fatalf("read the owed reads: %v", err)
	}
	defer rows.Close()
	var owed []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		owed = append(owed, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(owed)
	return owed
}

// refusalReads is how many times the reconcile has read the refusal owed for acme/widgets#5071.
func refusalReads(t *testing.T, ctx context.Context, pool *store.Pool) int {
	t.Helper()
	var reads int
	if err := pool.QueryRow(ctx, `select reads from pull_request_reads_owed where repo = 'acme/widgets' and number = 5071`).Scan(&reads); err != nil {
		t.Fatalf("read the refusal of acme/widgets#5071: %v", err)
	}
	return reads
}

// headRows is how many head rows of repo's pull request number are stored.
func headRows(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `select count(*) from pull_request_heads where repo = $1 and number = $2`, repo, number).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// renameOldcoWidgets records oldco/widgets as renamed to acme/widgets.
func renameOldcoWidgets(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		insert into repo_projects (repo, project, created_by, is_default, renamed_to)
		values ('oldco/widgets', $1, '{"kind":"system","id":"delivery-test"}'::jsonb, false, 'acme/widgets')
		on conflict (repo) do update set renamed_to = excluded.renamed_to
	`, deliveryProject); err != nil {
		t.Fatalf("record oldco/widgets as renamed to acme/widgets: %v", err)
	}
}

// backfillLinks seeds the issues and links the first two tests read, and the rows they stand
// beside: a head row of #5064, of #5065 (the link spells it in other letters) and of #5066 (the
// link names the repository by the name it had before a rename to acme/widgets).
func backfillLinks(t *testing.T, ctx context.Context, pool *store.Pool) map[string][]string {
	t.Helper()
	links := map[string][]string{
		"ATTR-901": {"https://github.com/acme/widgets/pull/5063"},
		"ATTR-902": {"https://github.com/acme/widgets/pull/5064"},
		"ATTR-903": {"https://GITHUB.com/ACME/Widgets/PULL/5065"},
		"ATTR-904": {"https://github.com/oldco/widgets/pull/5066", "https://github.com/oldco/widgets/pull/5067"},
		"ATTR-905": {"https://github.com/acme/widgets/issues/5068"},
		"ATTR-906": {"https://github.com/../widgets/pull/5069", "https://github.com/acme/widgets/pull/05070", "https://example.com/acme/widgets/pull/5073"},
		"ATTR-907": {"https://github.com/acme/widgets/pull/5071"},
		"ATTR-908": {"https://github.com/acme/widgets/pull/5072"},
		"ATTR-909": {"https://github.com/Acme/Widgets/pull/5072"},
	}
	keys := make([]string, 0, len(links))
	for key := range links {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	seedAttributionIssues(t, ctx, pool, keys, links)
	renameOldcoWidgets(t, ctx, pool)
	for _, number := range []int{5064, 5065, 5066} {
		if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{
			Repo: "acme/widgets", Number: number, HeadSHA: fakeSHA("5", number), HeadRef: "topic", BaseRef: "main", State: "merged",
		}); err != nil {
			t.Fatalf("store the head of acme/widgets#%d: %v", number, err)
		}
	}
	return links
}

// oweRefusal5071 owes a refusal of acme/widgets#5071, read three times: a read the backfill finds
// owed already. It is owed after any pass the test runs before the backfill, since a pass would
// read it and settle it, GitHub answering that no such pull request is there.
func oweRefusal5071(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		insert into pull_request_reads_owed (repo, number, reason, reads, refused_as, refused_since)
		values ('acme/widgets', 5071, 'refusal', 3, 'pull_request_not_found', now() - interval '1 hour')
	`); err != nil {
		t.Fatalf("owe a refusal of acme/widgets#5071: %v", err)
	}
}

// A link written before this release owes no read of the pull request it links, so a merged pull
// request with no event since, acme/widgets#5063 here, keeps no head row however many passes run,
// and the issue's checks read no head (red). The backfill owes, once, the read of each such link
// as a link write of this release owes it, and the reconcile's next pass reads it from GitHub and
// writes its head (green): #5063 as GitHub holds it, merged, and #5067, which the link names by
// the repository's name before its rename, under acme/widgets; and #5068, an /issues/ link,
// whose number GitHub holds no pull request of, which the pass settles. Then each linking issue
// lists its pull request's head: ATTR-901 #5063, and ATTR-904 #5067, linked by the old name (on
// the head before this test asserted it, ATTR-904 listed no head). A link whose pull request has
// a head row owes nothing (control): #5064, #5065 spelled in other letters, and #5066 named by the
// old name; nor does a link of no repository GitHub can have, of another host or with a zero
// before its number, nor a pull request whose read is owed already, which keeps its refusal and
// its three reads. Two links of #5072 in two letter cases owe one read. That the backfill owes
// what the link write owes for every shape of link is TestTheLinkBackfillOwesWhatALinkWriteOwes.
func TestTheLinkBackfillOwesTheReadOfEachLinkedPullRequestWithNoHead(t *testing.T) {
	fastPageRetries(t)
	database := storetest.Open(t)
	pool, ctx := database.Pool, t.Context()
	if err := PrepareAttribution(ctx, pool); err != nil {
		t.Fatal(err)
	}
	backfillLinks(t, ctx, pool)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for _, number := range []int{5063, 5067, 5072} {
		github.set("acme/widgets", number, "open", fakeSHA("6", number), "topic", "", at)
	}
	github.merge(5063)
	client := fake.newTestClient()

	NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)
	if rows := headRows(t, ctx, pool, "acme/widgets", 5063); rows != 0 {
		t.Fatalf("red: before the backfill a pass stored %d head rows of acme/widgets#5063, which a link written before this release names; want none", rows)
	}

	oweRefusal5071(t, ctx, pool)
	readsBefore := refusalReads(t, ctx, pool)
	runLinkBackfill(t, ctx, database)
	owed := owedLinkReads(t, ctx, pool)
	backfilled := slices.DeleteFunc(slices.Clone(owed), func(row string) bool { return !strings.HasSuffix(row, " link") })
	t.Logf("owed after the backfill: %v", owed)
	if want := []string{"Acme/Widgets#5072 failure link", "acme/widgets#5063 failure link", "acme/widgets#5067 failure link", "acme/widgets#5068 failure link"}; !slices.Equal(backfilled, want) {
		t.Fatalf("the backfill owes %v, want %v: #5063, #5067, #5068 and #5072 (as Acme/Widgets, first in byte order) alone", backfilled, want)
	}
	if !slices.Contains(owed, "acme/widgets#5071 refusal") {
		t.Fatalf("the owed refusal of acme/widgets#5071 is %v after the backfill, want it left as it was", owed)
	}
	if reads := refusalReads(t, ctx, pool); reads != readsBefore {
		t.Fatalf("the refusal of #5071 has %d reads after the backfill, want the %d it had before", reads, readsBefore)
	}

	NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)
	got := readHead(t, ctx, pool, "acme/widgets", 5063)
	if got.HeadSHA != fakeSHA("6", 5063) || got.State != "merged" {
		t.Fatalf("green: after the backfill's pass acme/widgets#5063 = head %s state %s, want GitHub's %s merged", got.HeadSHA, got.State, fakeSHA("6", 5063))
	}
	if rows := headRows(t, ctx, pool, "acme/widgets", 5067); rows != 1 {
		t.Fatalf("after the pass acme/widgets#5067, linked by the name before the rename, has %d head rows, want 1", rows)
	}
	for _, number := range []int{5063, 5067, 5068} {
		if n := owedCount(t, ctx, pool, "acme/widgets", number); n != 0 {
			t.Errorf("acme/widgets#%d is still owed after a pass GitHub answered", number)
		}
	}
	for issue, want := range map[string]int{"ATTR-901": 5063, "ATTR-904": 5067} {
		heads, err := HeadsAttributedTo(ctx, pool, issue)
		if err != nil {
			t.Fatal(err)
		}
		listed := slices.ContainsFunc(heads, func(head AttributedHead) bool { return head.Number == want && head.HeadKnown })
		if !listed {
			t.Errorf("after the backfill's pass %s lists %+v, want acme/widgets#%d with its head known", issue, heads, want)
		}
	}
}

// backfillCorpusHeads are the head rows the corpus stands beside, each as a release before this
// one stored it: under the name it was stored under, which for oldco/widgets is the name before
// its rename.
var backfillCorpusHeads = []struct {
	repo   string
	number int
}{{"acme/widgets", 102}, {"acme/widgets", 103}, {"oldco/widgets", 106}, {"oldco/widgets", 107}, {"ACME/WIDGETS", 137}}

// seedBackfillCorpus stores the corpus of the link rule (linkCorpus), one issue per link, with
// the rows it stands beside: oldco/widgets renamed to acme/widgets, badrename/x renamed to a
// target GitHub cannot have, the head rows of backfillCorpusHeads, a failure an event owed under
// acme/widgets#123 (the corpus links it as Acme/Widgets), a refusal of acme/widgets#124 read
// three times, and a failure owed under Acme/Gadgets#143 (the corpus links it as acme/gadgets). It
// answers the links in the order of their issues.
func seedBackfillCorpus(t *testing.T, ctx context.Context, pool *store.Pool) []string {
	t.Helper()
	if err := PrepareAttribution(ctx, pool); err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	links := map[string][]string{}
	for i, url := range linkCorpus {
		key := fmt.Sprintf("ATTR-%d", 600+i)
		keys = append(keys, key)
		links[key] = []string{url}
	}
	seedAttributionIssues(t, ctx, pool, keys, links)
	renameOldcoWidgets(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		insert into repo_projects (repo, project, created_by, is_default, renamed_to)
		values ('badrename/x', $1, '{"kind":"system","id":"delivery-test"}'::jsonb, false, 'not a repository name')
	`, deliveryProject); err != nil {
		t.Fatalf("record badrename/x as renamed: %v", err)
	}
	for _, head := range backfillCorpusHeads {
		if _, err := pool.Exec(ctx, `
			insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state)
			values ($1, $2, $3, 'topic', 'main', 'merged')`, head.repo, head.number, fakeSHA("9", head.number)); err != nil {
			t.Fatalf("store the head row of %s#%d: %v", head.repo, head.number, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		insert into pull_request_reads_owed (repo, number, reason, reads, refused_as, refused_since) values
			('acme/widgets', 123, 'failure', 1, null, null),
			('acme/widgets', 124, 'refusal', 3, 'pull_request_not_found', now() - interval '1 hour'),
			('Acme/Gadgets', 143, 'failure', 0, null, null)
	`); err != nil {
		t.Fatalf("owe the reads stored before the backfill: %v", err)
	}
	urls := make([]string, 0, len(keys))
	for _, key := range keys {
		urls = append(urls, links[key]...)
	}
	return urls
}

// The backfill owes what a link write of this release owes for the same links, row for row: the
// links of the corpus of the link rule (linkCorpus) are stored on two databases alike, with the
// same head rows, renames and owed reads; on one, the link write's own entry point owes them
// (OweLinkedPullRequestReads, in one transaction), and on the other the migration does. The two
// tables hold the same rows, each with its name, number, reason, count of reads, refusal and
// whether a link alone owes it. A row that was owed before keeps its revision and its time under
// the backfill, where a link write moves both: the way back deletes by that time, and no read is
// in flight while a migration runs. The rows are also pinned here, so a change of the link write
// shows in this test as well as in the comparison: among them an /issues/ link, a number of ten
// digits, an owner GitHub's sign-up refuses that the rule takes, two spellings of #122 owed once,
// a link of #123 and #143 in another letter case than the read owed already, which owes no second
// row, a head row stored under the name before a rename (#106 and #107), and the scheme, the host
// and `pull` in capitals (#133, #134, #142), while `.`, `..`, a zero before the number, another
// host and every other near miss of the corpus owe nothing.
func TestTheLinkBackfillOwesWhatALinkWriteOwes(t *testing.T) {
	writes := storetest.Open(t)
	ctx := t.Context()
	urls := seedBackfillCorpus(t, ctx, writes.Pool)
	tx, err := writes.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := OweLinkedPullRequestReads(ctx, tx, urls); err != nil {
		t.Fatalf("the link write of the corpus: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	want := owedRows(t, ctx, writes.Pool)

	backfill := storetest.Open(t)
	seedBackfillCorpus(t, ctx, backfill.Pool)
	before := owedStamps(t, ctx, backfill.Pool)
	runLinkBackfill(t, ctx, backfill)
	got := owedRows(t, ctx, backfill.Pool)
	t.Logf("the link write owes %d rows: %v", len(want), want)
	if !slices.Equal(got, want) {
		t.Fatalf("the backfill owes\n\t%s\nthe link write owes\n\t%s", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
	after := owedStamps(t, ctx, backfill.Pool)
	for _, stamp := range before {
		if !slices.Contains(after, stamp) {
			t.Errorf("the backfill changed the row owed before it, %s; want its revision and time left", stamp)
		}
	}

	long := strings.Repeat("o", 39) + "/" + strings.Repeat("n", 100)
	pinned := []string{
		"-acme/widgets#129 failure reads=0 refused_as=none refused_since=none link",
		"ACME/widgets#122 failure reads=0 refused_as=none refused_since=none link",
		"Acme/Gadgets#143 failure reads=0 refused_as=none refused_since=none",
		"Acme/Widgets#104 failure reads=0 refused_as=none refused_since=none link",
		"acme/..git#114 failure reads=0 refused_as=none refused_since=none link",
		"acme/.git#112 failure reads=0 refused_as=none refused_since=none link",
		"acme/widgets#1000000000 failure reads=0 refused_as=none refused_since=none link",
		"acme/widgets#101 failure reads=0 refused_as=none refused_since=none link",
		"acme/widgets#105 failure reads=0 refused_as=none refused_since=none link",
		"acme/widgets#108 failure reads=0 refused_as=none refused_since=none link",
		"acme/widgets#121 failure reads=0 refused_as=none refused_since=none link",
		"acme/widgets#123 failure reads=1 refused_as=none refused_since=none",
		"acme/widgets#124 refusal reads=3 refused_as=pull_request_not_found refused_since=set",
		"acme/widgets#133 failure reads=0 refused_as=none refused_since=none link",
		"acme/widgets#134 failure reads=0 refused_as=none refused_since=none link",
		"acme/widgets#136 failure reads=0 refused_as=none refused_since=none link",
		"acme/widgets#142 failure reads=0 refused_as=none refused_since=none link",
		"acme/widgets#2147483647 failure reads=0 refused_as=none refused_since=none link",
		"acme/x.git#113 failure reads=0 refused_as=none refused_since=none link",
		"acme_/w#130 failure reads=0 refused_as=none refused_since=none link",
		"badrename/x#109 failure reads=0 refused_as=none refused_since=none link",
		"gone/repo#140 failure reads=0 refused_as=none refused_since=none link",
		long + "#127 failure reads=0 refused_as=none refused_since=none link",
	}
	slices.Sort(pinned)
	if !slices.Equal(got, pinned) {
		t.Errorf("the backfill owes\n\t%s\nwant\n\t%s", strings.Join(got, "\n\t"), strings.Join(pinned, "\n\t"))
	}
}

// The way back deletes each owe the backfill wrote that no later owe changed, and nothing else:
// #5063's and #5068's owes as the backfill left them go; #5067's, which a link write of this
// release owed again after the backfill, stays, and so does #5072's, which an event owed again;
// the refusal of #5071 owed before the backfill stays. The control is the way back run with no
// record of the migration in schema_migrations, which deletes nothing.
func TestTheLinkBackfillsWayBackDeletesOnlyTheOwesItWrote(t *testing.T) {
	database := storetest.Open(t)
	pool, ctx := database.Pool, t.Context()
	if err := PrepareAttribution(ctx, pool); err != nil {
		t.Fatal(err)
	}
	backfillLinks(t, ctx, pool)
	oweRefusal5071(t, ctx, pool)
	runLinkBackfill(t, ctx, database)
	down, err := os.ReadFile("../store/migrations/20261120171308_pull_request_reads_owed_link_backfill.down.sql")
	if err != nil {
		t.Fatal(err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := OweLinkedPullRequestReads(ctx, tx, []string{"https://github.com/acme/widgets/pull/5067"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := owePullRequestRead(ctx, pool, "Acme/Widgets", 5072, owedFailure, ""); err != nil {
		t.Fatal(err)
	}
	before := owedLinkReads(t, ctx, pool)

	recorded, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recorded.Exec(ctx, `delete from schema_migrations where version = $1`, linkBackfillMigration); err != nil {
		t.Fatal(err)
	}
	if _, err := recorded.Exec(ctx, string(down)); err != nil {
		t.Fatalf("the way back with no record: %v", err)
	}
	var left int
	if err := recorded.QueryRow(ctx, `select count(*) from pull_request_reads_owed`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if err := recorded.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if left != len(before) {
		t.Fatalf("control: the way back with no record of the migration left %d of %d owed reads, want every one", left, len(before))
	}

	if _, err := pool.Exec(ctx, string(down)); err != nil {
		t.Fatalf("the way back: %v", err)
	}
	after := owedLinkReads(t, ctx, pool)
	t.Logf("owed before the way back: %v; after: %v", before, after)
	if want := []string{"Acme/Widgets#5072 failure", "acme/widgets#5067 failure link", "acme/widgets#5071 refusal"}; !slices.Equal(after, want) {
		t.Fatalf("after the way back the owed reads are %v, want %v", after, want)
	}
}
