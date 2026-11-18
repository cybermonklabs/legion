package delivery

// The facts line of the head pass (readFactsLine) holds the pull requests whose facts Dispatch
// does not hold. The batched query (readFactsBatch) reads them from GitHub, at most 500 of each
// installation in one pass. Each test drives the line, or the whole head pass, against
// batchGitHub. Then it reads what the pass wrote and what the count route answers
// (ReadAttributionCounts).

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// renamedToRepo is the new name of renamedRepo, acme/old (renameAcme).
const renamedToRepo = "acme/new"

// lineEventsFrom is the event time of the rows that storeRowsWithNoFacts stores, before the
// seconds of each number. GitHub's answers in these tests are newer.
var lineEventsFrom = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// storeRowsWithNoFacts stores a merged results row of each number of repo, with no texts, as a
// release before this build stored it. Each row holds the stored issue, a read of GitHub, and
// title(number) as its title. The event of a higher number is newer.
func storeRowsWithNoFacts(t *testing.T, ctx context.Context, pool *store.Pool, repo string, numbers []int, stored *string, title func(number int) string) {
	t.Helper()
	titles := make([]string, len(numbers))
	for i, number := range numbers {
		titles[i] = title(number)
	}
	if _, err := pool.Exec(ctx, `
		insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source, title, url, event_at, checked_at, github_read_at)
		select $1::text, u.n, lpad(to_hex(u.n), 40, '0'), 'work/change-' || u.n, 'main', 'merged', $4::text, 'none', u.title,
			'https://github.com/' || $1::text || '/pull/' || u.n, $5::timestamptz + make_interval(secs => u.n), now(), now()
		from unnest($2::int[], $3::text[]) as u(n, title)`, repo, numbers, titles, stored, lineEventsFrom); err != nil {
		t.Fatalf("store %d merged rows of %s with no facts: %v", len(numbers), repo, err)
	}
}

// storeOpenRow stores an open results row of repo#number, with current texts whose messages a
// read stored. A read of GitHub wrote the row and checked it at readAt. An insert owes no read, as
// an update of the state does, so only the open arm reads the row.
func storeOpenRow(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int, readAt time.Time) {
	t.Helper()
	at := lineEventsFrom.Add(time.Duration(number) * time.Second)
	title := changeTitle(number)
	if _, err := pool.Exec(ctx, `
		insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, attribution_source, title, url, event_at, checked_at, github_read_at)
		values ($1, $2, $3, 'work/none', 'main', 'open', 'none', $4, $5, $6, $7, $7)`,
		repo, number, headOf(number), title, fmt.Sprintf("https://github.com/%s/pull/%d", repo, number), at, readAt); err != nil {
		t.Fatalf("store the open row of %s#%d: %v", repo, number, err)
	}
	if _, err := pool.Exec(ctx, `
		insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title, body, commit_messages, updated_at)
		values ($1, $2, $3, 'work/none', $1, 'main', $4, 'The body before the rename.', '{}', $5)`,
		repo, number, headOf(number), title, at); err != nil {
		t.Fatalf("store the texts of %s#%d: %v", repo, number, err)
	}
}

// changeTitle is the title that openPull gives pull request number.
func changeTitle(number int) string { return fmt.Sprintf("Change %d", number) }

// mergedPull is openPull, merged.
func mergedPull(number int, updatedAt time.Time) batchPull {
	pull := openPull(number, updatedAt)
	pull.state = "merged"
	return pull
}

// renameAcme maps renamedToRepo to project ATTR as its default. It maps renamedRepo to the same
// project, renamed to renamedToRepo. So both names read as renamedToRepo (refs.CanonicalRepo).
func renameAcme(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	seedProjectRepository(t, ctx, pool, "ATTR", "ATTR", renamedToRepo)
	mapRepository(t, ctx, pool, renamedRepo, "ATTR", false, new(renamedToRepo))
}

// renamedGitHub is GitHub after the rename of renamedRepo: it serves renamedToRepo only. The App
// has no installation on the old name, and a query of the old name answers a null repository.
func renamedGitHub(t *testing.T) (*fakeGitHub, *batchGitHub) {
	t.Helper()
	fake := newFakeGitHub(t)
	fake.notInstalled.Store(renamedRepo, true)
	github := serveBatchGitHub(t, fake)
	github.gone[renamedRepo] = true
	return fake, github
}

// readFactsLineOrFail runs one facts line of reconcile, as a head pass runs it, and fails the
// test where it answers an error.
func readFactsLineOrFail(t *testing.T, ctx context.Context, reconcile *Reconcile) FactsLine {
	t.Helper()
	line, err := reconcile.readFactsLine(ctx, &passInstallations{}, &passRateLimit{})
	if err != nil {
		t.Fatalf("readFactsLine: %v", err)
	}
	return line
}

// attributionCountsOrFail is what the count route answers now (ReadAttributionCounts).
func attributionCountsOrFail(t *testing.T, ctx context.Context, pool *store.Pool) AttributionCounts {
	t.Helper()
	counts, err := ReadAttributionCounts(ctx, pool)
	if err != nil {
		t.Fatalf("ReadAttributionCounts: %v", err)
	}
	return counts
}

// readNumbers is each number of repo whose texts row holds its messages, in order: each pull
// request whose facts a read stored.
func readNumbers(t *testing.T, ctx context.Context, pool *store.Pool, repo string) []int {
	t.Helper()
	rows, err := pool.Query(ctx, `select number from pull_request_texts where repo = $1 and commit_messages is not null order by number`, repo)
	if err != nil {
		t.Fatalf("read the texts rows of %s: %v", repo, err)
	}
	defer rows.Close()
	var numbers []int
	for rows.Next() {
		var number int
		if err := rows.Scan(&number); err != nil {
			t.Fatalf("scan a texts row of %s: %v", repo, err)
		}
		numbers = append(numbers, number)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the texts rows of %s: %v", repo, err)
	}
	return numbers
}

// headRowsOf counts the results rows stored under repo#number.
func headRowsOf(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(ctx, `select count(*) from pull_request_heads where repo = $1 and number = $2`, repo, number).Scan(&rows); err != nil {
		t.Fatalf("count the results rows of %s#%d: %v", repo, number, err)
	}
	return rows
}

// timelineRowsOf counts the timeline rows stored under repo#number.
func timelineRowsOf(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(ctx, `select count(*) from delivery_pull_requests where repo = $1 and number = $2`, repo, number).Scan(&rows); err != nil {
		t.Fatalf("count the timeline rows of %s#%d: %v", repo, number, err)
	}
	return rows
}

// headStampsOf reports whether the results row of repo#number holds a refusal
// (attribution_refused_at) and a proof of absence (attribution_absent_at).
func headStampsOf(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) (refused, absent bool) {
	t.Helper()
	if err := pool.QueryRow(ctx, `
		select attribution_refused_at is not null, attribution_absent_at is not null
		from pull_request_heads where repo = $1 and number = $2`, repo, number).Scan(&refused, &absent); err != nil {
		t.Fatalf("read the stamps of the results row of %s#%d: %v", repo, number, err)
	}
	return refused, absent
}

// timelineStampsOf reports whether the timeline row of repo#number holds a refusal
// (facts_refused_at) and the mark that GitHub can no longer give it (unfetchable_at).
func timelineStampsOf(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) (refused, unfetchable bool) {
	t.Helper()
	if err := pool.QueryRow(ctx, `
		select facts_refused_at is not null, unfetchable_at is not null
		from delivery_pull_requests where repo = $1 and number = $2`, repo, number).Scan(&refused, &unfetchable); err != nil {
		t.Fatalf("read the stamps of the timeline row of %s#%d: %v", repo, number, err)
	}
	return refused, unfetchable
}

// attributionLines is each record of the head pass's line among records.
func attributionLines(records []map[string]any) []map[string]any {
	var lines []map[string]any
	for _, record := range records {
		if record["msg"] == "dispatch delivery: pull request attribution" {
			lines = append(lines, record)
		}
	}
	return lines
}

// attributionMovesOn is each issue.pull_request_attribution_changed event of issue, oldest first,
// as "<repo>#<number> <from> -> <to>".
func attributionMovesOn(t *testing.T, ctx context.Context, pool *store.Pool, issue string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `select payload from events where issue_key = $1 and type = $2 order by id`, issue, AttributionEventType)
	if err != nil {
		t.Fatalf("read the attribution events of %s: %v", issue, err)
	}
	defer rows.Close()
	var moves []string
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan an attribution event of %s: %v", issue, err)
		}
		var payload IssuePullRequestAttributionEventPayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode an attribution event of %s: %v", issue, err)
		}
		moves = append(moves, fmt.Sprintf("%s#%d %s -> %s", payload.Repo, payload.Number, describeIssue(payload.From), describeIssue(payload.To)))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the attribution events of %s: %v", issue, err)
	}
	return moves
}

// The facts line reads at most FactsQueriesPerPass queries of FactsBatchSize pull requests of
// each installation in one pass: 500 of each. acme/widgets is installation 1 and other/tools is
// installation 2. Each holds 520 merged rows with no facts. Want 500 rows of each read, 25 queries
// of 20 pull requests for each installation, and Left 40. A line that shares one budget between
// the installations, or that reads past the budget, fails (readFactsLine, steps 3 and 4).
func TestTheFactsLineReadsFiveHundredRowsOfEachInstallation(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	fake := newFakeGitHub(t)
	github := serveBatchGitHub(t, fake)
	github.install(2, "other/tools")
	client := fake.newTestClient()
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	repos, numbers := []string{"acme/widgets", "other/tools"}, numbersFrom(1, 520)
	for _, repo := range repos {
		storeRowsWithNoFacts(t, ctx, pool, repo, numbers, nil, changeTitle)
		for _, number := range numbers {
			github.set(repo, number, mergedPull(number, second))
		}
	}

	line := readFactsLineOrFail(t, ctx, NewReconcile(pool, client, events.NewBroker()))

	for _, repo := range repos {
		if read := len(readNumbers(t, ctx, pool, repo)); read != FactsQueriesPerPass*FactsBatchSize {
			t.Errorf("the facts line stored the facts of %d rows of %s, want %d", read, repo, FactsQueriesPerPass*FactsBatchSize)
		}
	}
	for id := int64(1); id <= 2; id++ {
		if got := github.queriesOf(id); got != FactsQueriesPerPass {
			t.Errorf("installation %d was asked %d queries, want %d", id, got, FactsQueriesPerPass)
		}
	}
	for _, query := range github.asked() {
		if len(query.numbers) != FactsBatchSize {
			t.Errorf("a query of installation %d asked %d pull requests, want %d", query.installation, len(query.numbers), FactsBatchSize)
		}
	}
	if got := fake.factsQueries.Load(); got != 2*FactsQueriesPerPass {
		t.Errorf("the facts line sent %d queries, want %d", got, 2*FactsQueriesPerPass)
	}
	if want := (FactsLine{LeftBefore: 1040, Left: 40, ReadsLeft: 40}); line != want {
		t.Errorf("the facts line = %+v, want %+v", line, want)
	}
}

// The facts line first reads the rows whose stored issue is likely wrong. For such a row, the
// statement over the keys of its title and branch alone differs from its fallback. 600 merged rows
// of acme/stmt store ACME-1. The title of 20 of them names ACME-2, and the titles of the others
// name ACME-1. Ten of the 20 are the oldest rows, and ten are the newest. So no order by age or by
// number puts all 20 in the first 500. Want all 20 among the 500 that the line reads. A line that
// does not put those rows first fails (readFactsLine, step 2).
func TestTheFactsLineReadsTheLikelyWrongRowsFirst(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-1", "ACME-2")
	numbers := numbersFrom(1, 600)
	likelyWrong := append(numbersFrom(1, 10), numbersFrom(591, 10)...)
	title := func(number int) string {
		if slices.Contains(likelyWrong, number) {
			return fmt.Sprintf("feat: ACME-2 change %d", number)
		}
		return fmt.Sprintf("feat: ACME-1 change %d", number)
	}
	storeRowsWithNoFacts(t, ctx, pool, statementRepo, numbers, new("ACME-1"), title)
	fake := newFakeGitHub(t)
	github := serveBatchGitHub(t, fake)
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for _, number := range numbers {
		pull := mergedPull(number, second)
		pull.title = title(number)
		github.set(statementRepo, number, pull)
	}

	readFactsLineOrFail(t, ctx, NewReconcile(pool, fake.newTestClient(), events.NewBroker()))

	read := readNumbers(t, ctx, pool, statementRepo)
	if len(read) != FactsQueriesPerPass*FactsBatchSize {
		t.Errorf("the facts line stored the facts of %d rows, want %d", len(read), FactsQueriesPerPass*FactsBatchSize)
	}
	var missed []int
	for _, number := range likelyWrong {
		if !slices.Contains(read, number) {
			missed = append(missed, number)
		}
	}
	if len(missed) > 0 {
		t.Errorf("the 500 rows that the facts line read leave out %v, whose titles name ACME-2 beside their stored ACME-1", missed)
	}
}

// The facts line reads a results row under a repository's old name under the canonical name. The
// write keeps one row, under the canonical name (revision 8, BL1). The repository acme/old has the
// new name acme/new. GitHub serves acme/new only, and the App has no installation on acme/old. In
// the first case acme/old#5 is the only row. In the second, acme/new#6 has texts and acme/old#6 has
// none. After one pass only the row under acme/new exists, with GitHub's texts. facts_left and
// reads_left are 0, and the next pass sends no query. A line that reads or writes under the stored
// name fails (readFactsLine, steps 1 and 4: upsertHeadUnderCanonicalName with StoredUnder).
func TestTheFactsLineWritesARowUnderAnOldNameUnderTheCanonicalName(t *testing.T) {
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for _, kase := range []struct {
		name         string
		number       int
		canonicalRow bool
	}{
		{name: "a_row_under_the_old_name_alone", number: 5},
		{name: "a_row_under_each_name", number: 6, canonicalRow: true},
	} {
		t.Run(kase.name, func(t *testing.T) {
			pool, ctx := deliveryTestPool(t)
			renameAcme(t, ctx, pool)
			storeStatementRow(t, ctx, pool, statementRow{repo: renamedRepo, number: kase.number, branch: "work/none", title: changeTitle(kase.number)})
			if kase.canonicalRow {
				storeStatementRow(t, ctx, pool, statementRow{repo: renamedToRepo, number: kase.number, branch: "work/none", title: changeTitle(kase.number), texts: readTexts("")})
			}
			fake, github := renamedGitHub(t)
			pull := mergedPull(kase.number, second)
			github.set(renamedToRepo, kase.number, pull)
			reconcile := NewReconcile(pool, fake.newTestClient(), events.NewBroker())

			readFactsLineOrFail(t, ctx, reconcile)

			if old, canonical := headRowsOf(t, ctx, pool, renamedRepo, kase.number), headRowsOf(t, ctx, pool, renamedToRepo, kase.number); old != 0 || canonical != 1 {
				t.Errorf("after one pass #%d has %d results rows under %s and %d under %s, want 0 and 1", kase.number, old, renamedRepo, canonical, renamedToRepo)
			}
			if texts, found := storedTextsOf(t, ctx, pool, renamedToRepo, kase.number); !found || !texts.messagesRead || texts.body != pull.body {
				t.Errorf("the texts of %s#%d = %+v (found %v), want GitHub's body %q with the messages read", renamedToRepo, kase.number, texts, found, pull.body)
			}
			if counts := attributionCountsOrFail(t, ctx, pool); counts.FactsLeft != 0 || counts.ReadsLeft != 0 {
				t.Errorf("after one pass facts_left = %d and reads_left = %d, want 0 and 0", counts.FactsLeft, counts.ReadsLeft)
			}
			for _, asked := range github.pullsAsked() {
				if strings.HasPrefix(asked, renamedRepo+"#") {
					t.Errorf("a query asked about %s: GitHub serves the canonical name only", asked)
				}
			}
			queries := fake.factsQueries.Load()
			readFactsLineOrFail(t, ctx, reconcile)
			if asked := fake.factsQueries.Load() - queries; asked != 0 {
				t.Errorf("the next pass's facts line sent %d queries, want none: the pull request left the line", asked)
			}
		})
	}
}

// The open arm reads an open row under a repository's old name under the canonical name. It
// writes one row, under the canonical name (revision 8, BL1). A read of GitHub wrote acme/old#7 10
// minutes ago. The row is open, with current texts, so only the open arm reads it. After one head
// pass one row acme/new#7 holds the texts of GitHub's answer, and no row is under acme/old.
// ListStalePullRequestHeads then offers no row of #7. An open arm that reads or writes under the
// stored name fails (reconcilePullRequestHeads, the open arm with StoredUnder).
func TestTheOpenArmWritesARowUnderAnOldNameUnderTheCanonicalName(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	renameAcme(t, ctx, pool)
	const number = 7
	storeOpenRow(t, ctx, pool, renamedRepo, number, time.Now().Add(-10*time.Minute))
	if owed := owedCount(t, ctx, pool, renamedRepo, number) + owedCount(t, ctx, pool, renamedToRepo, number); owed != 0 {
		t.Fatalf("the fixture owes %d reads of #%d, want none: only the open arm reads it", owed, number)
	}
	if counts := attributionCountsOrFail(t, ctx, pool); counts.ReadsLeft != 0 {
		t.Fatalf("the facts line holds %d entries before the pass, want none: only the open arm reads #%d", counts.ReadsLeft, number)
	}
	fake, github := renamedGitHub(t)
	pull := openPull(number, time.Now().UTC().Add(-time.Minute).Truncate(time.Second))
	pull.body = "The body that GitHub answers now."
	github.set(renamedToRepo, number, pull)

	if err := NewReconcile(pool, fake.newTestClient(), events.NewBroker()).HeadPass(ctx); err != nil {
		t.Fatalf("HeadPass: %v", err)
	}

	if old, canonical := headRowsOf(t, ctx, pool, renamedRepo, number), headRowsOf(t, ctx, pool, renamedToRepo, number); old != 0 || canonical != 1 {
		t.Errorf("after one pass #%d has %d results rows under %s and %d under %s, want 0 and 1", number, old, renamedRepo, canonical, renamedToRepo)
	}
	if texts, found := storedTextsOf(t, ctx, pool, renamedToRepo, number); !found || texts.body != pull.body || !texts.messagesRead {
		t.Errorf("the texts of %s#%d = %+v (found %v), want GitHub's body %q with the messages read", renamedToRepo, number, texts, found, pull.body)
	}
	if asked := github.pullsAsked(); !slices.Contains(asked, fmt.Sprintf("%s#%d", renamedToRepo, number)) || github.restReads() != 0 {
		t.Errorf("the pass asked the queries %v and read %d pull requests over REST, want a query of %s#%d and no REST read", asked, github.restReads(), renamedToRepo, number)
	}
	stale, err := ListStalePullRequestHeads(ctx, pool, PullRequestHeadBatch, time.Now())
	if err != nil {
		t.Fatalf("ListStalePullRequestHeads: %v", err)
	}
	for _, ref := range stale {
		if ref.Number == number {
			t.Errorf("after the pass ListStalePullRequestHeads offers %s/%s#%d stored under %s, want no row of #%d", ref.Owner, ref.Repo, ref.Number, ref.StoredUnder, number)
		}
	}
}

// The facts line reads a timeline row under a repository's old name, with no results row, once,
// under the canonical name (revision 8, BL1). acme/old#8 is a timeline row of ATTR-81 only. After
// one pass a results row acme/new#8 holds texts, and the timeline row stays under acme/old.
// facts_left is 0, and the next pass sends no query. The control in the same test: before the
// pass, a timeline row acme/old#9 whose results row is under acme/new is not in the line. A line
// that finds no results row of a timeline row under another name reads #8 every pass, and #9 too
// (readFactsLine, step 1).
func TestTheFactsLineReadsATimelineRowUnderAnOldNameOnce(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	renameAcme(t, ctx, pool)
	insertProjectIssue(t, ctx, pool, "ATTR", "ATTR-81", 81)
	timeline := &statementTimeline{issue: new("ATTR-81")}
	storeStatementRow(t, ctx, pool, statementRow{repo: renamedRepo, number: 8, title: changeTitle(8), noHead: true, timeline: timeline})
	storeStatementRow(t, ctx, pool, statementRow{repo: renamedToRepo, number: 9, branch: "work/none", title: changeTitle(9), texts: readTexts("")})
	storeStatementRow(t, ctx, pool, statementRow{repo: renamedRepo, number: 9, title: changeTitle(9), noHead: true, timeline: timeline})
	fake, github := renamedGitHub(t)
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	github.set(renamedToRepo, 8, mergedPull(8, second))
	github.set(renamedToRepo, 9, mergedPull(9, second))
	reconcile := NewReconcile(pool, fake.newTestClient(), events.NewBroker())
	t.Run("control_a_timeline_row_whose_results_row_is_under_the_canonical_name", func(t *testing.T) {
		if counts := attributionCountsOrFail(t, ctx, pool); counts.ReadsLeft != 1 || counts.FactsLeft != 1 {
			t.Errorf("before the pass reads_left = %d and facts_left = %d, want 1 and 1: only #8. The results row of acme/old#9 is acme/new#9, with facts", counts.ReadsLeft, counts.FactsLeft)
		}
	})

	readFactsLineOrFail(t, ctx, reconcile)

	if texts, found := storedTextsOf(t, ctx, pool, renamedToRepo, 8); !found || !texts.messagesRead {
		t.Errorf("the texts of %s#8 = %+v (found %v), want GitHub's facts with the messages read", renamedToRepo, texts, found)
	}
	if rows := headRowsOf(t, ctx, pool, renamedRepo, 8); rows != 0 {
		t.Errorf("after the pass %s#8 has %d results rows, want none: the read writes under the canonical name", renamedRepo, rows)
	}
	if old, canonical := timelineRowsOf(t, ctx, pool, renamedRepo, 8), timelineRowsOf(t, ctx, pool, renamedToRepo, 8); old != 1 || canonical != 0 {
		t.Errorf("after the pass #8 has %d timeline rows under %s and %d under %s, want 1 and 0: no statement rewrites a stored row to another name", old, renamedRepo, canonical, renamedToRepo)
	}
	if counts := attributionCountsOrFail(t, ctx, pool); counts.FactsLeft != 0 || counts.ReadsLeft != 0 {
		t.Errorf("after the pass facts_left = %d and reads_left = %d, want 0 and 0", counts.FactsLeft, counts.ReadsLeft)
	}
	for _, asked := range github.pullsAsked() {
		if strings.HasSuffix(asked, "#9") {
			t.Errorf("a query asked about %s, which has facts under %s", asked, renamedToRepo)
		}
	}
	queries := fake.factsQueries.Load()
	readFactsLineOrFail(t, ctx, reconcile)
	if asked := fake.factsQueries.Load() - queries; asked != 0 {
		t.Errorf("the next pass's facts line sent %d queries, want none: #8 left the line", asked)
	}
}

// A value that no row can hold refuses only its own row (BL2). One query reads #31 and #32, and
// GitHub answers the head branch of #31 with a NUL. Want attribution_refused_at on #31, the facts
// of #32 written, and #31 counted in facts_refused, still in the line. A line that ends at the
// refused write, or that writes no refusal, fails (readFactsLine, step 4: errNoRowHolds).
func TestTheFactsLineGoesOnPastARefusedValue(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	const repo, refused, written = "acme/widgets", 31, 32
	storeRowsWithNoFacts(t, ctx, pool, repo, []int{refused, written}, nil, changeTitle)
	fake := newFakeGitHub(t)
	github := serveBatchGitHub(t, fake)
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	poisoned := mergedPull(refused, second)
	poisoned.branch = "work/change\x0031"
	github.set(repo, refused, poisoned)
	github.set(repo, written, mergedPull(written, second))

	line := readFactsLineOrFail(t, ctx, NewReconcile(pool, fake.newTestClient(), events.NewBroker()))

	if asked := github.asked(); len(asked) != 1 || len(asked[0].numbers) != 2 {
		t.Errorf("the facts line sent the queries %+v, want one query of #%d and #%d", asked, refused, written)
	}
	if refusedAt, absentAt := headStampsOf(t, ctx, pool, repo, refused); !refusedAt || absentAt {
		t.Errorf("#%d holds attribution_refused_at %v and attribution_absent_at %v, want a refusal only: GitHub answered its branch with a NUL", refused, refusedAt, absentAt)
	}
	if _, found := storedTextsOf(t, ctx, pool, repo, refused); found {
		t.Errorf("#%d has a texts row, want none: no row holds its branch", refused)
	}
	if texts, found := storedTextsOf(t, ctx, pool, repo, written); !found || !texts.messagesRead {
		t.Errorf("the texts of #%d = %+v (found %v), want GitHub's facts: a refusal of another row ends no read", written, texts, found)
	}
	if counts := attributionCountsOrFail(t, ctx, pool); counts.FactsRefused != 1 || counts.ReadsLeft != 1 || counts.FactsLeft != 1 {
		t.Errorf("after the pass facts_refused = %d, reads_left = %d and facts_left = %d, want 1, 1 and 1: #%d", counts.FactsRefused, counts.ReadsLeft, counts.FactsLeft, refused)
	}
	if line.Refused != 1 {
		t.Errorf("the facts line = %+v, want Refused 1", line)
	}
}

// Only proof of absence ends a row (R4). An alias that GitHub answers null with NOT_FOUND, in a
// readable repository, goes to pullRequestAbsent, and its proof sets attribution_absent_at. A
// null repository proves nothing: it sets attribution_refused_at, and the row stays in the line.
// The same holds for a timeline row with no results row: facts_refused_at on a refusal, and
// unfetchable_at only on proof. A line that ends a row on a refusal, or that asks GitHub for no
// proof, fails (readFactsLine, steps 5 and 6).
func TestTheFactsLineEndsARowOnlyOnProofOfAbsence(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	const readable, gone = "acme/widgets", "acme/gone"
	storeRowsWithNoFacts(t, ctx, pool, readable, []int{41}, nil, changeTitle)
	storeRowsWithNoFacts(t, ctx, pool, gone, []int{42}, nil, changeTitle)
	storeStatementRow(t, ctx, pool, statementRow{repo: readable, number: 43, title: changeTitle(43), noHead: true, timeline: &statementTimeline{}})
	storeStatementRow(t, ctx, pool, statementRow{repo: gone, number: 44, title: changeTitle(44), noHead: true, timeline: &statementTimeline{}})
	fake := newFakeGitHub(t)
	github := serveBatchGitHub(t, fake)
	github.set(readable, 40, mergedPull(40, time.Now().UTC().Add(-time.Hour).Truncate(time.Second)))
	github.gone[gone] = true

	line := readFactsLineOrFail(t, ctx, NewReconcile(pool, fake.newTestClient(), events.NewBroker()))

	if refused, absent := headStampsOf(t, ctx, pool, readable, 41); refused || !absent {
		t.Errorf("%s#41 holds attribution_refused_at %v and attribution_absent_at %v, want the proof of absence only", readable, refused, absent)
	}
	if refused, absent := headStampsOf(t, ctx, pool, gone, 42); !refused || absent {
		t.Errorf("%s#42 holds attribution_refused_at %v and attribution_absent_at %v, want a refusal only: a null repository proves nothing", gone, refused, absent)
	}
	if refused, unfetchable := timelineStampsOf(t, ctx, pool, readable, 43); refused || !unfetchable {
		t.Errorf("the timeline row %s#43 holds facts_refused_at %v and unfetchable_at %v, want unfetchable_at only", readable, refused, unfetchable)
	}
	if refused, unfetchable := timelineStampsOf(t, ctx, pool, gone, 44); !refused || unfetchable {
		t.Errorf("the timeline row %s#44 holds facts_refused_at %v and unfetchable_at %v, want facts_refused_at only", gone, refused, unfetchable)
	}
	asked := github.absenceQueries()
	slices.Sort(asked)
	if want := []string{readable + "#41", readable + "#43"}; !slices.Equal(asked, want) {
		t.Errorf("the absence query asked about %v, want %v: only an alias with NOT_FOUND on its own path", asked, want)
	}
	if counts := attributionCountsOrFail(t, ctx, pool); counts.ReadsLeft != 2 || counts.FactsRefused != 2 {
		t.Errorf("after the pass reads_left = %d and facts_refused = %d, want 2 and 2: #42 and #44 stay in the line", counts.ReadsLeft, counts.FactsRefused)
	}
	if line.ReadsLeft != 2 || line.Refused != 2 {
		t.Errorf("the facts line = %+v, want ReadsLeft 2 and Refused 2", line)
	}
}

// A rate limit of one installation ends the facts line of that installation only. GitHub answers
// each query of installation 1 (acme/widgets) with its GraphQL rate limit, and answers the queries
// of installation 2 (other/tools). The client keeps GitHub's limits, as the server's does. After
// one head pass, the pass has read the 30 rows of installation 2 and none of installation 1. The
// pass answers the GraphQL rate limit of installation 1, and its line names that scope in
// rate_limit_scope. A line that ends every installation at the first limit fails (readFactsLine,
// step 5).
func TestALimitOfOneInstallationLeavesTheOtherInstallationsFactsLine(t *testing.T) {
	logs := captureLogsAt(t, slog.LevelDebug)
	pool, ctx := deliveryTestPool(t)
	fake := newFakeGitHub(t)
	github := serveBatchGitHub(t, fake)
	github.install(2, "other/tools")
	github.limitInstallation(1)
	client := fake.newTestClient(githubapp.WithLimits(ratelimit.New(pool)))
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	numbers := numbersFrom(1, 30)
	for _, repo := range []string{"acme/widgets", "other/tools"} {
		storeRowsWithNoFacts(t, ctx, pool, repo, numbers, nil, changeTitle)
		for _, number := range numbers {
			github.set(repo, number, mergedPull(number, second))
		}
	}

	err := NewReconcile(pool, client, events.NewBroker()).HeadPass(ctx)

	scope := githubapp.GraphQLInstallationScope(githubapp.InstallationScope(1))
	if limited, ok := githubapp.AsRateLimit(err); !ok || limited.Scope != scope {
		t.Errorf("the head pass answered %v, want the GraphQL rate limit of installation 1, under %s", err, scope)
	}
	if read := len(readNumbers(t, ctx, pool, "other/tools")); read != len(numbers) {
		t.Errorf("the pass stored the facts of %d rows of other/tools, want all %d: a limit of installation 1 holds no other", read, len(numbers))
	}
	if read := len(readNumbers(t, ctx, pool, "acme/widgets")); read != 0 {
		t.Errorf("the pass stored the facts of %d rows of acme/widgets, want none: GitHub answered each query with a rate limit", read)
	}
	if limitedQueries, otherQueries := github.queriesOf(1), github.queriesOf(2); limitedQueries != 1 || otherQueries != 2 {
		t.Errorf("installation 1 was asked %d queries and installation 2 %d, want 1 and 2: the limit ends the line of installation 1", limitedQueries, otherQueries)
	}
	lines := attributionLines(logs())
	if len(lines) != 1 || lines[0]["rate_limit_scope"] != scope {
		t.Errorf("the head pass logged the lines %v, want one that names rate_limit_scope %s", lines, scope)
	}
}

// The head pass logs one line in each pass, also when every count is 0 (Global Constraints). Want
// msg="dispatch delivery: pull request attribution" with each key of the line, each 0, and no
// rate_limit_scope, since no limit ended a read. The control in the same test: a pass that reads
// one row logs its counts, so a line of constants fails it. A pass that logs only where a count
// is above 0 fails the first step (reconcilePullRequestHeads).
func TestTheHeadPassLogsItsLineEveryPass(t *testing.T) {
	logs := captureLogsAt(t, slog.LevelDebug)
	pool, ctx := deliveryTestPool(t)
	fake := newFakeGitHub(t)
	github := serveBatchGitHub(t, fake)
	reconcile := NewReconcile(pool, fake.newTestClient(), events.NewBroker())

	if err := reconcile.HeadPass(ctx); err != nil {
		t.Fatalf("HeadPass: %v", err)
	}

	lines := attributionLines(logs())
	if len(lines) != 1 {
		t.Fatalf("a head pass with nothing to read logged %d attribution lines, want 1: %v", len(lines), lines)
	}
	for _, key := range []string{"facts_left_before", "facts_left", "reads_left", "facts_refused", "changes", "events", "moves_queued"} {
		if value, found := lines[0][key]; !found || value != float64(0) {
			t.Errorf("the line holds %s = %v (found %v), want 0: %v", key, value, found, lines[0])
		}
	}
	if value, found := lines[0]["rate_limit_scope"]; found {
		t.Errorf("the line names rate_limit_scope %v, want none: no limit ended a read", value)
	}
	t.Run("control_a_pass_that_reads_a_row", func(t *testing.T) {
		const repo, number = "acme/widgets", 51
		storeRowsWithNoFacts(t, ctx, pool, repo, []int{number}, nil, changeTitle)
		github.set(repo, number, mergedPull(number, time.Now().UTC().Add(-time.Hour).Truncate(time.Second)))
		if err := reconcile.HeadPass(ctx); err != nil {
			t.Fatalf("HeadPass: %v", err)
		}
		lines := attributionLines(logs())
		if len(lines) != 2 {
			t.Fatalf("two head passes logged %d attribution lines, want 2: %v", len(lines), lines)
		}
		for key, want := range map[string]float64{"facts_left_before": 1, "facts_left": 0, "reads_left": 0} {
			if value := lines[1][key]; value != want {
				t.Errorf("the line of the pass that read #%d holds %s = %v, want %v: %v", number, key, value, want, lines[1])
			}
		}
	})
}

// The facts line reads the messages of a pull request that has current texts and messages not
// read (B4, state S12). Its branch names a held issue, so it has facts: it is in reads_left and
// not in facts_left (review 4, note 5). The next facts line reads it with its messages, and then it
// is out of reads_left. Its answer stays the branch's issue all along. A line that holds only the
// pull requests with no facts fails (readFactsLine, step 1: messages_read).
func TestTheFactsLineReadsMessagesThatAreNotRead(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-3")
	const number = 33
	storeStatementRow(t, ctx, pool, statementRow{number: number, branch: "legion/ACME-3", title: "feat: the work", texts: &statementTexts{}})
	wantAnswer(t, ctx, pool, statementRepo, number, "ACME-3 from branch")
	if counts := attributionCountsOrFail(t, ctx, pool); counts.ReadsLeft != 1 || counts.FactsLeft != 0 {
		t.Errorf("before the read reads_left = %d and facts_left = %d, want 1 and 0", counts.ReadsLeft, counts.FactsLeft)
	}
	fake := newFakeGitHub(t)
	github := serveBatchGitHub(t, fake)
	pull := mergedPull(number, time.Now().UTC().Add(-time.Hour).Truncate(time.Second))
	pull.branch, pull.title, pull.messages = "legion/ACME-3", "feat: the work", []string{"Work on the change."}
	github.set(statementRepo, number, pull)

	readFactsLineOrFail(t, ctx, NewReconcile(pool, fake.newTestClient(), events.NewBroker()))

	if asked := github.pullsAsked(); !slices.Contains(asked, fmt.Sprintf("%s#%d", statementRepo, number)) {
		t.Errorf("the facts line asked about %v, want %s#%d", asked, statementRepo, number)
	}
	if texts, found := storedTextsOf(t, ctx, pool, statementRepo, number); !found || !texts.messagesRead || !slices.Equal(texts.messages, pull.messages) {
		t.Errorf("the texts of #%d = %+v (found %v), want GitHub's messages %q, read", number, texts, found, pull.messages)
	}
	if counts := attributionCountsOrFail(t, ctx, pool); counts.ReadsLeft != 0 || counts.FactsLeft != 0 {
		t.Errorf("after the read reads_left = %d and facts_left = %d, want 0 and 0", counts.ReadsLeft, counts.FactsLeft)
	}
	wantAnswer(t, ctx, pool, statementRepo, number, "ACME-3 from branch")
}

// The first read of a pull request with a timeline row only moves it from the timeline's issue
// (review 3, note). acme/stmt#61 is a timeline row of ACME-11 only. The facts line reads it, and
// its title names ACME-12. After two head passes, want one move from ACME-11 to ACME-12: one event
// on each of the two issues, and no move queued. A new results row that does not start with the
// issue of its timeline row moves from nobody, and fails (SyncAttributions, from_key).
func TestTheFirstReadOfATimelineOnlyPullRequestMovesFromItsTimelineIssue(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-11", "ACME-12")
	const number, title = 61, "feat: ACME-12 the work"
	storeStatementRow(t, ctx, pool, statementRow{number: number, title: title, noHead: true, timeline: &statementTimeline{issue: new("ACME-11")}})
	wantAnswer(t, ctx, pool, statementRepo, number, "ACME-11 from none, no facts")
	fake := newFakeGitHub(t)
	github := serveBatchGitHub(t, fake)
	pull := mergedPull(number, time.Now().UTC().Add(-time.Hour).Truncate(time.Second))
	pull.branch, pull.title = "work/none", title
	github.set(statementRepo, number, pull)
	reconcile := NewReconcile(pool, fake.newTestClient(), events.NewBroker())

	for pass := 1; pass <= 2; pass++ {
		if err := reconcile.HeadPass(ctx); err != nil {
			t.Fatalf("head pass %d: %v", pass, err)
		}
	}

	wantAnswer(t, ctx, pool, statementRepo, number, "ACME-12 from title_body")
	want := []string{fmt.Sprintf("%s#%d ACME-11 -> ACME-12", statementRepo, number)}
	for _, issue := range []string{"ACME-11", "ACME-12"} {
		if moves := attributionMovesOn(t, ctx, pool, issue); !slices.Equal(moves, want) {
			t.Errorf("%s carries the attribution events %q, want %q", issue, moves, want)
		}
	}
	if counts := attributionCountsOrFail(t, ctx, pool); counts.MovesQueued != 0 {
		t.Errorf("after two passes %d moves are queued, want none: each pass appends what it queues", counts.MovesQueued)
	}
}
