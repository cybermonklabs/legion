package delivery

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The repositories of the statement's tests: each project's default, so its issues resolve there.
const (
	statementRepo = "acme/stmt"
	otherRepo     = "acme/other"
	renamedRepo   = "acme/old"
)

// statementIssues makes each key, an issue of the project its prefix names: ACME of statementRepo,
// OTHR of otherRepo, RENM of renamedRepo.
func statementIssues(t *testing.T, ctx context.Context, pool *store.Pool, keys ...string) {
	t.Helper()
	repos := map[string]string{"ACME": statementRepo, "OTHR": otherRepo, "RENM": renamedRepo}
	for _, key := range keys {
		project, number, _ := strings.Cut(key, "-")
		seedProjectRepository(t, ctx, pool, project, project, repos[project])
		if _, err := pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, status, created_by, rank)
			values ($1, $2, $3::int, $1, 'todo', '{"kind":"system","id":"test"}', $1) on conflict do nothing`, key, project, number); err != nil {
			t.Fatalf("seed issue %s: %v", key, err)
		}
	}
}

// statementRow is one pull request as a test stores it: its results row, and its texts row
// where texts is set. Head is the results row's head, "" for one made from the number. Read
// false leaves github_read_at null.
type statementRow struct {
	repo     string
	number   int
	branch   string
	title    string
	stored   *string
	unread   bool
	url      string
	texts    *statementTexts
	timeline *statementTimeline
	noHead   bool
}

type statementTexts struct {
	body     string
	messages []string
	headSHA  string // "" for the results row's head
	headRepo *string
}

type statementTimeline struct{ issue *string }

func headOf(number int) string { return fmt.Sprintf("%040x", number) }

func storeStatementRow(t *testing.T, ctx context.Context, pool *store.Pool, row statementRow) {
	t.Helper()
	if row.repo == "" {
		row.repo = statementRepo
	}
	if row.url == "" {
		row.url = fmt.Sprintf("https://github.com/%s/pull/%d", row.repo, row.number)
	}
	at := time.Date(2026, 11, 1, 0, 0, row.number%60, 0, time.UTC)
	if !row.noHead {
		readAt := "now()"
		if row.unread {
			readAt = "null"
		}
		if _, err := pool.Exec(ctx, `
			insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source, title, url, event_at, checked_at, github_read_at)
			values ($1, $2, $3, $4, 'main', 'merged', $5, 'none', $6, $7, $8, now(), `+readAt+`)`,
			row.repo, row.number, headOf(row.number), row.branch, row.stored, row.title, row.url, at); err != nil {
			t.Fatalf("store the results row of %s#%d: %v", row.repo, row.number, err)
		}
	}
	if texts := row.texts; texts != nil {
		sha := texts.headSHA
		if sha == "" {
			sha = headOf(row.number)
		}
		headRepo := row.repo
		if texts.headRepo != nil {
			headRepo = *texts.headRepo
		}
		if _, err := pool.Exec(ctx, `
			insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title, body, commit_messages, updated_at)
			values ($1, $2, $3, $4, $5, 'main', $6, $7, $8, $9)`,
			row.repo, row.number, sha, row.branch, headRepo, row.title, texts.body, texts.messages, at); err != nil {
			t.Fatalf("store the texts of %s#%d: %v", row.repo, row.number, err)
		}
	}
	if timeline := row.timeline; timeline != nil {
		if _, err := pool.Exec(ctx, `insert into delivery_pull_requests (repo, number, title, url, author, issue_key) values ($1, $2, $3, $4, 'octocat', $5)`,
			row.repo, row.number, row.title, row.url, timeline.issue); err != nil {
			t.Fatalf("store the timeline row of %s#%d: %v", row.repo, row.number, err)
		}
	}
}

// readTexts is a texts row whose messages were read and hold no key.
func readTexts(body string) *statementTexts { return &statementTexts{body: body, messages: []string{}} }

// answerOf is what PullRequestIssues answers for repo#number: "<key> from <source>", "nobody
// from <source>", each with ", no facts" where HasFacts is false, or "absent".
func answerOf(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) string {
	t.Helper()
	answers, err := PullRequestIssues(ctx, pool, []PullRequestKey{{Repo: repo, Number: number}})
	if err != nil {
		t.Fatalf("PullRequestIssues(%s#%d): %v", repo, number, err)
	}
	answer, found := answers[PullRequestKey{Repo: repo, Number: number}]
	if !found {
		return "absent"
	}
	return describeAttribution(answer)
}

func describeAttribution(answer PullRequestAttribution) string {
	issue := "nobody"
	if answer.IssueKey != nil {
		issue = *answer.IssueKey
	}
	line := issue + " from " + answer.Source
	if !answer.HasFacts {
		line += ", no facts"
	}
	return line
}

// listedFor is what HeadsAttributedTo lists for issue: each number, with "?" after one whose
// head Dispatch has not read.
func listedFor(t *testing.T, ctx context.Context, pool *store.Pool, issue string) []string {
	t.Helper()
	heads, err := HeadsAttributedTo(ctx, pool, issue)
	if err != nil {
		t.Fatalf("HeadsAttributedTo(%s): %v", issue, err)
	}
	listed := []string{}
	for _, head := range heads {
		entry := "#" + strconv.Itoa(head.Number)
		if !head.HeadKnown {
			entry += "?"
			if head.HeadSHA != "" {
				t.Errorf("HeadsAttributedTo(%s) lists #%d with HeadKnown false and the head %q, want no head", issue, head.Number, head.HeadSHA)
			}
		}
		listed = append(listed, entry)
	}
	return listed
}

// readersOf is what the three readers by issue that need a head answer for issue: whether sha is
// its head, the newest head's repository and number, and the base branch of its merged head.
func readersOf(t *testing.T, ctx context.Context, pool *store.Pool, issue, sha string) string {
	t.Helper()
	at, err := HeadAttributedTo(ctx, pool, issue, sha)
	if err != nil {
		t.Fatalf("HeadAttributedTo(%s): %v", issue, err)
	}
	newest, found, err := NewestPullRequestHead(ctx, pool, issue)
	if err != nil {
		t.Fatalf("NewestPullRequestHead(%s): %v", issue, err)
	}
	branch, err := BaseBranchMergedFor(ctx, pool, issue)
	if err != nil {
		t.Fatalf("BaseBranchMergedFor(%s): %v", issue, err)
	}
	newestLine := "none"
	if found {
		newestLine = fmt.Sprintf("%s#%d", newest.Repo, newest.Number)
	}
	return fmt.Sprintf("at head %v, newest %s, base %q", at, newestLine, branch)
}

func wantAnswer(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int, want string) {
	t.Helper()
	if got := answerOf(t, ctx, pool, repo, number); got != want {
		t.Errorf("%s#%d answers %q, want %q", repo, number, got, want)
	}
}

func wantListed(t *testing.T, ctx context.Context, pool *store.Pool, issue string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if got := listedFor(t, ctx, pool, issue); !slices.Equal(got, want) {
		t.Errorf("HeadsAttributedTo(%s) lists %v, want %v", issue, got, want)
	}
}

func wantReaders(t *testing.T, ctx context.Context, pool *store.Pool, issue, sha, want string) {
	t.Helper()
	if got := readersOf(t, ctx, pool, issue, sha); got != want {
		t.Errorf("the readers of %s at %s answer %q, want %q", issue, sha, got, want)
	}
}

// The five sources in their order: the issue that links the pull request, the issue that links
// a GitHub issue its body cites, a key in its branch, a key in its title or body, a key in its
// commit messages. Each row drops the source above it.
func TestTheStatementTakesEverySourceInOrder(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-41", "ACME-42", "ACME-43", "ACME-44", "ACME-45")
	if _, err := pool.Exec(ctx, `insert into issue_external_links (issue_key, url, kind) values
		('ACME-42', 'https://github.com/acme/stmt/pull/40', 'github'), ('ACME-43', 'https://github.com/acme/stmt/issues/143', 'github')`); err != nil {
		t.Fatal(err)
	}
	for i, step := range []struct {
		branch, title, body string
		messages            []string
		want                string
	}{
		{"legion/ACME-44", "feat: ACME-41", "Fixes #143.", []string{"ACME-45"}, "ACME-42 from external_link"},
		{"legion/ACME-44", "feat: ACME-41", "Fixes #143.", []string{"ACME-45"}, "ACME-43 from github_issue_link"},
		{"legion/ACME-44", "feat: ACME-41", "", []string{"ACME-45"}, "ACME-44 from branch"},
		{"main", "feat: ACME-41", "", []string{"ACME-45"}, "ACME-41 from title_body"},
		{"main", "feat: none", "", []string{"ACME-45"}, "ACME-45 from commit_message"},
		{"main", "feat: none", "", []string{}, "nobody from none"},
	} {
		number := 40 + i
		storeStatementRow(t, ctx, pool, statementRow{number: number, branch: step.branch, title: step.title,
			texts: &statementTexts{body: step.body, messages: step.messages}})
		wantAnswer(t, ctx, pool, statementRepo, number, step.want)
	}
}

// A candidate counts only where its issue resolves to the pull request's repository: a branch
// that names an issue of another repository is passed over for the title's.
func TestTheStatementHoldsACandidateToThePullRequestsRepository(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "OTHR-1", "ACME-51")
	storeStatementRow(t, ctx, pool, statementRow{number: 51, branch: "legion/OTHR-1", title: "feat: ACME-51", texts: readTexts("")})
	wantAnswer(t, ctx, pool, statementRepo, 51, "ACME-51 from title_body")
}

// The statement reads the first 25 candidates, as the base does: a held key after 25 that are
// not held names nobody, and after 24 it names its issue.
func TestTheStatementReadsTwentyFiveCandidates(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-61")
	var foreign []string
	for n := 1; n <= 25; n++ {
		foreign = append(foreign, fmt.Sprintf("OTHR-%d", n))
	}
	statementIssues(t, ctx, pool, foreign...)
	storeStatementRow(t, ctx, pool, statementRow{number: 61, branch: "main", title: strings.Join(foreign, " "), texts: readTexts("Then ACME-61.")})
	storeStatementRow(t, ctx, pool, statementRow{number: 62, branch: "main", title: strings.Join(foreign[:24], " "), texts: readTexts("Then ACME-61.")})
	wantAnswer(t, ctx, pool, statementRepo, 61, "nobody from none")
	wantAnswer(t, ctx, pool, statementRepo, 62, "ACME-61 from title_body")
}

// A pull request whose head is a fork's, or a deleted fork's (no head repository), credits
// nobody once it has facts. Control in the same test: the same texts with the head in the pull
// request's own repository credit the branch's issue.
func TestTheStatementCreditsNobodyForAForksHead(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-71")
	for number, headRepo := range map[int]string{71: "fork/x", 72: "", 73: statementRepo} {
		storeStatementRow(t, ctx, pool, statementRow{number: number, branch: "legion/ACME-71", title: "feat: work",
			texts: &statementTexts{messages: []string{}, headRepo: new(headRepo)}})
	}
	wantAnswer(t, ctx, pool, statementRepo, 71, "nobody from none")
	wantAnswer(t, ctx, pool, statementRepo, 72, "nobody from none")
	wantAnswer(t, ctx, pool, statementRepo, 73, "ACME-71 from branch")
}

// GitHub spells an owner and a repository as their owners do: a link and a citation match in
// any case.
func TestTheStatementMatchesLinksAndCitationsWithoutCase(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-81", "ACME-82")
	if _, err := pool.Exec(ctx, `insert into issue_external_links (issue_key, url, kind) values
		('ACME-81', 'https://github.com/ACME/STMT/pull/81', 'github'), ('ACME-82', 'https://github.com/Acme/Stmt/issues/9', 'github')`); err != nil {
		t.Fatal(err)
	}
	storeStatementRow(t, ctx, pool, statementRow{number: 81, branch: "main", title: "no key", texts: readTexts("")})
	storeStatementRow(t, ctx, pool, statementRow{number: 82, branch: "main", title: "no key", texts: readTexts("Fixes https://GITHUB.com/acme/stmt/issues/9.")})
	wantAnswer(t, ctx, pool, statementRepo, 81, "ACME-81 from external_link")
	wantAnswer(t, ctx, pool, statementRepo, 82, "ACME-82 from github_issue_link")
}

// setStoredReading puts the texts row of acme/stmt#number on reading with the branch keys keys,
// with the trigger off, as a row a release of another reading stored stands.
func setStoredReading(t *testing.T, ctx context.Context, pool *store.Pool, number int, reading string, keys []string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `set local session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `update pull_request_texts set reading = $2, branch_keys = $3 where repo = 'acme/stmt' and number = $1`, number, reading, keys); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// Stored keys of another reading are read from the texts in the statement. Control in the same
// test: the same stale keys on the reading that runs now are the answer, so the test can fail.
func TestTheStatementComputesKeysOfAnotherReading(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-91", "ACME-92")
	storeStatementRow(t, ctx, pool, statementRow{number: 91, branch: "legion/ACME-91", title: "no key", texts: readTexts("")})
	setStoredReading(t, ctx, pool, 91, "old", []string{"ACME-92"})
	wantAnswer(t, ctx, pool, statementRepo, 91, "ACME-91 from branch")
	setStoredReading(t, ctx, pool, 91, currentReading(t, ctx, pool, readingSchema(t, ctx, pool)), []string{"ACME-92"})
	wantAnswer(t, ctx, pool, statementRepo, 91, "ACME-92 from branch")
}

// Texts of another head than the results row's are no facts: the pull request answers its
// fallback, the results row's stored issue.
func TestTextsOfAnotherHeadAreNoFacts(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-101", "ACME-102")
	storeStatementRow(t, ctx, pool, statementRow{number: 101, branch: "legion/ACME-102", title: "no key", stored: new("ACME-101"),
		texts: &statementTexts{messages: []string{}, headSHA: headOf(9101)}})
	wantAnswer(t, ctx, pool, statementRepo, 101, "ACME-101 from none, no facts")
	wantListed(t, ctx, pool, "ACME-101", "#101")
	wantListed(t, ctx, pool, "ACME-102")
}

// A pull request with no facts answers the results row's stored answer, read or unread, also
// when that is nobody; only a pull request with no results row takes its timeline row's issue.
func TestTheFallbackIsTheResultsRowsAnswerThenTheTimelines(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-111", "ACME-112", "ACME-113")
	timeline := &statementTimeline{issue: new("ACME-111")}
	storeStatementRow(t, ctx, pool, statementRow{number: 111, branch: "main", title: "t", stored: new("ACME-112"), timeline: timeline})
	storeStatementRow(t, ctx, pool, statementRow{number: 112, branch: "main", title: "t", timeline: timeline})
	storeStatementRow(t, ctx, pool, statementRow{number: 113, branch: "main", title: "t", stored: new("ACME-113"), unread: true, timeline: timeline})
	storeStatementRow(t, ctx, pool, statementRow{number: 114, title: "t", noHead: true, timeline: timeline})
	storeStatementRow(t, ctx, pool, statementRow{number: 115, title: "t", noHead: true, timeline: &statementTimeline{}})
	wantAnswer(t, ctx, pool, statementRepo, 111, "ACME-112 from none, no facts")
	wantAnswer(t, ctx, pool, statementRepo, 112, "nobody from none, no facts")
	wantAnswer(t, ctx, pool, statementRepo, 113, "ACME-113 from none, no facts")
	wantAnswer(t, ctx, pool, statementRepo, 114, "ACME-111 from none, no facts")
	wantAnswer(t, ctx, pool, statementRepo, 115, "nobody from none, no facts")
	wantAnswer(t, ctx, pool, statementRepo, 116, "absent")
	wantListed(t, ctx, pool, "ACME-112", "#111")
	wantListed(t, ctx, pool, "ACME-113", "#113")
	wantListed(t, ctx, pool, "ACME-111", "#114?")
	wantReaders(t, ctx, pool, "ACME-112", headOf(111), `at head true, newest acme/stmt#111, base "main"`)
	wantReaders(t, ctx, pool, "ACME-113", headOf(113), `at head true, newest acme/stmt#113, base "main"`)
	wantReaders(t, ctx, pool, "ACME-111", headOf(111), `at head false, newest none, base ""`)
}

// An unread results row whose texts describe its head answers the statement, not its mark.
func TestAnUnreadRowWithValidTextsAnswersTheStatement(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-121", "ACME-129")
	storeStatementRow(t, ctx, pool, statementRow{number: 121, branch: "legion/ACME-121", title: "no key", stored: new("ACME-129"), unread: true, texts: readTexts("")})
	wantAnswer(t, ctx, pool, statementRepo, 121, "ACME-121 from branch")
	wantListed(t, ctx, pool, "ACME-121", "#121")
	wantReaders(t, ctx, pool, "ACME-121", headOf(121), `at head true, newest acme/stmt#121, base "main"`)
	wantListed(t, ctx, pool, "ACME-129")
	wantReaders(t, ctx, pool, "ACME-129", headOf(121), `at head false, newest none, base ""`)
}

// A pull request with a timeline row only is listed for its issue with no head, and the readers
// that need a head answer nothing for it.
func TestHeadsAttributedToNamesAPullRequestWhoseHeadIsNotRead(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-131")
	storeStatementRow(t, ctx, pool, statementRow{number: 131, title: "t", noHead: true, timeline: &statementTimeline{issue: new("ACME-131")}})
	wantListed(t, ctx, pool, "ACME-131", "#131?")
	wantReaders(t, ctx, pool, "ACME-131", headOf(131), `at head false, newest none, base ""`)
}

// No reader answers a stored issue for a pull request with facts, on either of its rows.
func TestNoReaderAnswersAStoredIssueForAPullRequestWithFacts(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-141", "ACME-149")
	stored := &statementTimeline{issue: new("ACME-149")}
	storeStatementRow(t, ctx, pool, statementRow{number: 141, branch: "legion/ACME-141", title: "no key", stored: new("ACME-149"), texts: readTexts(""), timeline: stored})
	storeStatementRow(t, ctx, pool, statementRow{number: 142, branch: "work/none", title: "no key", stored: new("ACME-149"), texts: readTexts(""), timeline: stored})
	wantAnswer(t, ctx, pool, statementRepo, 141, "ACME-141 from branch")
	wantAnswer(t, ctx, pool, statementRepo, 142, "nobody from none")
	wantListed(t, ctx, pool, "ACME-149")
	wantReaders(t, ctx, pool, "ACME-149", headOf(141), `at head false, newest none, base ""`)
}

// renameStatementRepo renames renamedRepo to acme/new.
func renameStatementRepo(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `update repo_projects set renamed_to = 'acme/new' where repo = $1`, renamedRepo); err != nil {
		t.Fatalf("rename %s: %v", renamedRepo, err)
	}
}

// Every side of the statement reads a repository through the renaming rule: rows stored under
// the old name answer under both names, the newest head names the canonical name, and the
// timeline row's issue is no answer for a pull request with facts.
func TestTheStatementReadsEverySideThroughTheRenamingRule(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "RENM-5", "RENM-4")
	storeStatementRow(t, ctx, pool, statementRow{repo: renamedRepo, number: 5, branch: "legion/RENM-5", title: "the change", texts: readTexts(""),
		timeline: &statementTimeline{issue: new("RENM-4")}})
	renameStatementRepo(t, ctx, pool)
	wantAnswer(t, ctx, pool, renamedRepo, 5, "RENM-5 from branch")
	wantAnswer(t, ctx, pool, "acme/new", 5, "RENM-5 from branch")
	wantListed(t, ctx, pool, "RENM-5", "#5")
	wantReaders(t, ctx, pool, "RENM-5", headOf(5), `at head true, newest acme/new#5, base "main"`)
	wantListed(t, ctx, pool, "RENM-4")
}

// Two results rows of one pull request under the old and the new name give one answer, the row
// under the canonical name's, and the pull request is listed once.
func TestTwoResultsRowsUnderTwoNamesGiveOneAnswer(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "RENM-6", "RENM-7")
	renameStatementRepo(t, ctx, pool)
	storeStatementRow(t, ctx, pool, statementRow{repo: renamedRepo, number: 6, branch: "legion/RENM-6", title: "old", texts: readTexts("")})
	storeStatementRow(t, ctx, pool, statementRow{repo: "acme/new", number: 6, branch: "legion/RENM-7", title: "new", texts: readTexts("")})
	wantAnswer(t, ctx, pool, renamedRepo, 6, "RENM-7 from branch")
	wantAnswer(t, ctx, pool, "acme/new", 6, "RENM-7 from branch")
	wantListed(t, ctx, pool, "RENM-7", "#6")
	wantListed(t, ctx, pool, "RENM-6")
}

// A reader by issue finds a pull request whose body cites, in another case, a GitHub issue that
// the issue links.
func TestAReaderByIssueMatchesACitationInAnotherCase(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-8")
	if _, err := pool.Exec(ctx, `insert into issue_external_links (issue_key, url, kind) values ('ACME-8', 'https://github.com/Acme/Stmt/issues/77', 'github')`); err != nil {
		t.Fatal(err)
	}
	storeStatementRow(t, ctx, pool, statementRow{number: 8, branch: "work/none", title: "no key", texts: readTexts("Closes https://github.com/ACME/stmt/issues/77.")})
	wantListed(t, ctx, pool, "ACME-8", "#8")
	wantAnswer(t, ctx, pool, statementRepo, 8, "ACME-8 from github_issue_link")
}

// A link matches a pull request's own URL of at most 1,024 bytes, the bound of the index on
// lower(url); a longer URL is no URL of GitHub, and no link matches it. Control in the same test:
// the statement without the bound on its two URL arms names the link's issue for 2,000 bytes.
func TestALinkMatchesAURLOfAtMostTheBound(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	urls := map[int]string{}
	for number, length := range map[int]int{21: 1024, 22: 1025, 23: 2000} {
		issue := fmt.Sprintf("ACME-%d", number)
		statementIssues(t, ctx, pool, issue)
		prefix := fmt.Sprintf("https://github.com/acme/stmt/pull/%d#", number)
		urls[number] = prefix + strings.Repeat("a", length-len(prefix))
		if _, err := pool.Exec(ctx, `insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'github')`, issue, urls[number]); err != nil {
			t.Fatal(err)
		}
		storeStatementRow(t, ctx, pool, statementRow{number: number, branch: "main", title: "no key", url: urls[number], texts: readTexts("")})
	}
	wantAnswer(t, ctx, pool, statementRepo, 21, "ACME-21 from external_link")
	wantAnswer(t, ctx, pool, statementRepo, 22, "nobody from none")
	wantAnswer(t, ctx, pool, statementRepo, 23, "nobody from none")
	wantListed(t, ctx, pool, "ACME-21", "#21")
	wantListed(t, ctx, pool, "ACME-22")
	wantListed(t, ctx, pool, "ACME-23")

	unbounded := strings.ReplaceAll(pullRequestIssuesStatement(readingSchema(t, ctx, pool)), urlBound("src.url"), "true")
	var issue *string
	if err := pool.QueryRow(ctx, unbounded, []string{statementRepo}, []int{23}).Scan(new(string), new(int), &issue, new(string), new(bool), new(bool)); err != nil {
		t.Fatalf("the control's statement: %v", err)
	}
	if issue == nil || *issue != "ACME-23" {
		t.Errorf("the control: the statement without the bound names %v for #23, want ACME-23", issue)
	}
}

// Two issues that link one pull request in two letter cases give it one issue, the lesser key,
// in either order of the rows. Control in the same test: one issue alone that links a pull
// request in another case is its answer.
func TestTwoLinksThatDifferOnlyInCaseGiveOneIssue(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-31", "ACME-32")
	storeStatementRow(t, ctx, pool, statementRow{number: 31, branch: "main", title: "no key", texts: readTexts("")})
	storeStatementRow(t, ctx, pool, statementRow{number: 33, branch: "main", title: "no key", texts: readTexts("")})
	for _, order := range [][2]string{{"ACME-32", "ACME-31"}, {"ACME-31", "ACME-32"}} {
		if _, err := pool.Exec(ctx, `delete from issue_external_links where url ilike 'https://github.com/acme/stmt/pull/31'`); err != nil {
			t.Fatal(err)
		}
		spelling := map[string]string{"ACME-32": "https://github.com/ACME/stmt/pull/31", "ACME-31": "https://github.com/acme/stmt/pull/31"}
		for _, issue := range order {
			if _, err := pool.Exec(ctx, `insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'github')`, issue, spelling[issue]); err != nil {
				t.Fatal(err)
			}
		}
		wantAnswer(t, ctx, pool, statementRepo, 31, "ACME-31 from external_link")
		wantListed(t, ctx, pool, "ACME-31", "#31")
		wantListed(t, ctx, pool, "ACME-32")
	}
	if _, err := pool.Exec(ctx, `insert into issue_external_links (issue_key, url, kind) values ('ACME-32', 'https://github.com/ACME/Stmt/pull/33', 'github')`); err != nil {
		t.Fatal(err)
	}
	wantAnswer(t, ctx, pool, statementRepo, 33, "ACME-32 from external_link")
}

// Messages that were not read decide nothing: a higher source that names a held issue gives the
// facts. With none, the answer is not known: no issue, and not the issue that the row stores.
func TestMessagesNotReadDecideNothingAndHideNothing(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-9", "ACME-3")
	storeStatementRow(t, ctx, pool, statementRow{number: 9, branch: "legion/ACME-9", title: "no key", texts: &statementTexts{}})
	storeStatementRow(t, ctx, pool, statementRow{number: 10, branch: "work/none", title: "no key", stored: new("ACME-3"), texts: &statementTexts{}})
	wantAnswer(t, ctx, pool, statementRepo, 9, "ACME-9 from branch")
	wantKnown(t, ctx, pool, statementRepo, 9, true)
	wantAnswer(t, ctx, pool, statementRepo, 10, "nobody from none, no facts")
	wantKnown(t, ctx, pool, statementRepo, 10, false)
	wantListed(t, ctx, pool, "ACME-3")
}

// wantKnown fails the test where PullRequestIssues answers repo#number with another Known than
// want, or answers it not at all.
func wantKnown(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int, want bool) {
	t.Helper()
	key := PullRequestKey{Repo: repo, Number: number}
	answers, err := PullRequestIssues(ctx, pool, []PullRequestKey{key})
	if err != nil {
		t.Fatalf("PullRequestIssues(%s#%d): %v", repo, number, err)
	}
	answer, found := answers[key]
	if !found {
		t.Fatalf("PullRequestIssues answers nothing for %s#%d", repo, number)
	}
	if answer.Known != want {
		t.Errorf("%s#%d answers %q with Known %v, want Known %v", repo, number, describeAttribution(answer), answer.Known, want)
	}
}

// A pull request whose texts are current, whose messages are not read and whose texts name no
// held issue has an answer that is not known. #581's branch names PRNT-581, a child of PRNT-180,
// whose repository is the pull request's, so the branch's issue is held. Then the results row
// stores PRNT-581, as the sync or a release before stores it, and PRNT-581 loses its parent. No
// candidate is held now. The answer is nobody with Known false, and no reader lists #581 under the
// stored issue. Control in the same test: the same rows with the messages read and empty answer
// nobody with Known true, so the test can tell the two states apart.
func TestAMoveAwayFromAPullRequestWhoseAnswerIsNotKnown(t *testing.T) {
	for _, read := range []bool{false, true} {
		name := map[bool]string{false: "messages_not_read", true: "control_messages_read"}[read]
		t.Run(name, func(t *testing.T) {
			pool, ctx := deliveryTestPool(t)
			seedProjectRepository(t, ctx, pool, "PRNT", "PRNT", "acme/prnt-main")
			mapRepository(t, ctx, pool, "acme/prnt-side", "PRNT", false, nil)
			insertProjectIssue(t, ctx, pool, "PRNT", "PRNT-180", 180)
			insertProjectIssue(t, ctx, pool, "PRNT", "PRNT-581", 581)
			execDispatchChange(t, ctx, pool, "give PRNT-180 its own repository", `update issues set repo = 'acme/prnt-side' where key = 'PRNT-180'`)
			execDispatchChange(t, ctx, pool, "make PRNT-581 a child of PRNT-180", `update issues set parent_key = 'PRNT-180' where key = 'PRNT-581'`)
			texts := &statementTexts{}
			if read {
				texts = readTexts("")
			}
			storeStatementRow(t, ctx, pool, statementRow{repo: "acme/prnt-side", number: 581, branch: "legion/PRNT-581", title: "no key", texts: texts})
			wantAnswer(t, ctx, pool, "acme/prnt-side", 581, "PRNT-581 from branch")
			wantKnown(t, ctx, pool, "acme/prnt-side", 581, true)
			execDispatchChange(t, ctx, pool, "store the answer as the sync stores it",
				`update pull_request_heads set issue_key = 'PRNT-581', attribution_source = 'branch' where repo = 'acme/prnt-side' and number = 581`)
			execDispatchChange(t, ctx, pool, "move PRNT-581 away from PRNT-180", `update issues set parent_key = null where key = 'PRNT-581'`)
			if read {
				wantAnswer(t, ctx, pool, "acme/prnt-side", 581, "nobody from none")
				wantKnown(t, ctx, pool, "acme/prnt-side", 581, true)
			} else {
				wantAnswer(t, ctx, pool, "acme/prnt-side", 581, "nobody from none, no facts")
				wantKnown(t, ctx, pool, "acme/prnt-side", 581, false)
			}
			wantListed(t, ctx, pool, "PRNT-581")
		})
	}
}

// The trigger and the statement call the reading alike: for each text of the corpus, the lists
// the statement computes from a row on another reading are the lists the trigger stored. Control
// in the same test: a statement that passes an empty body computes other lists.
func TestTheTriggerAndTheStatementCallTheReadingAlike(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	cases := readingCorpus(t)
	for i, c := range cases {
		number := 5001 + i
		if _, err := pool.Exec(ctx, `insert into pull_request_heads (repo, number, head_sha, head_ref, state) values ($1, $2, 'aaaa', $3, 'merged')`, c.Repo, number, c.HeadRef); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title, body, commit_messages)
			values ($1, $2, 'aaaa', $3, $1, 'main', $4, $5, $6)`, c.Repo, number, c.HeadRef, c.Title, c.Body, c.Messages); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `set local session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `update pull_request_texts set reading = 'other'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	schema := readingSchema(t, ctx, pool)
	differing := func(statement string) int {
		t.Helper()
		rows, err := pool.Query(ctx, statement+`
			select count(*) filter (where (b.title_keys, b.cited_issues, b.branch_keys, b.commit_keys)
				is distinct from (t.title_keys, t.cited_issues, t.branch_keys, t.commit_keys)), count(*)
			from batch b join pull_request_texts t on t.number = b.number`)
		if err != nil {
			t.Fatalf("the statement's lists: %v", err)
		}
		defer rows.Close()
		var differ, read int
		for rows.Next() {
			if err := rows.Scan(&differ, &read); err != nil {
				t.Fatal(err)
			}
		}
		if read != len(cases) {
			t.Fatalf("the statement read %d rows, want %d", read, len(cases))
		}
		return differ
	}
	terms := attributedTerms(schema, `select repo, number from pull_request_texts`)
	if differ := differing(terms); differ != 0 {
		t.Errorf("the statement computed other lists than the trigger stored for %d of %d texts", differ, len(cases))
	}
	const call = "attribution_keys(t.repo, t.head_ref, t.title, t.body, t.commit_messages)"
	if strings.Count(terms, call) != 1 {
		t.Fatalf("the statement calls %s %d times, want 1", call, strings.Count(terms, call))
	}
	if differ := differing(strings.Replace(terms, call, "attribution_keys(t.repo, t.head_ref, t.title, '', t.commit_messages)", 1)); differ == 0 {
		t.Error("the control: a statement that passes '' for the body computed the trigger's lists for every text")
	}
}
