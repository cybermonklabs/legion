package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// linkPullRequests replaces the external links of issue with one link to each pull request URL
// given, through the issue PATCH a person or an agent links them with.
func linkPullRequests(t *testing.T, handler http.Handler, issue string, urls ...string) {
	t.Helper()
	links := make([]map[string]string, 0, len(urls))
	for _, url := range urls {
		links = append(links, map[string]string{"url": url})
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue,
		map[string]any{"external_links": links}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("link %s to %v: status=%d body=%s", issue, urls, response.Code, response.Body.String())
	}
}

// storedCommitRepo is the commit_repo the record route wrote with a result, nil where it wrote
// none.
func storedCommitRepo(t *testing.T, database *store.Store, result model.CheckResult) *string {
	t.Helper()
	var repo *string
	if err := database.Pool.QueryRow(context.Background(),
		`select commit_repo from check_results where id = $1::uuid`, result.ID).Scan(&repo); err != nil {
		t.Fatalf("read the repository stored with result %s: %v", result.ID, err)
	}
	return repo
}

// recordStored records one pass of check at commit on issue and fails the test unless it is
// stored.
func recordStored(t *testing.T, handler http.Handler, issue, check, commit string) model.CheckResult {
	t.Helper()
	recorded := recordResult(t, handler, issue, resultBody(check, "pass", commit, nil))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("a result of %s at %s: status=%d body=%s", check, commit, recorded.Code, recorded.Body.String())
	}
	return decodeBody[recordedResult](t, recorded).Result
}

// A read of the issue's own repository that did not answer — a rate limit, a 5xx — does not keep
// the record from the links. A result at a commit only a linked repository holds is stored with
// that repository, as it is where GitHub proves the issue's own repository does not hold the
// commit, and its pull requests are read through that repository; so the results read compares
// the `after_merge` result with that repository's branches and it stands at one. Stored with
// none, it would be compared in the issue's own repository, where GitHub answers 404 for the
// commit, and read unknown with read_failed for good. A result at a commit no linked repository
// holds is stored with none and read through the issue's own repository, as it was before the
// links were read, and nothing is refused: GitHub proved nothing about the issue's own
// repository.
func TestAFailedReadOfTheIssuesOwnRepositoryStillReadsTheLinks(t *testing.T) {
	t.Parallel()
	const (
		// toolsMerge is the commit acme/tools#9 merged into main as, and unheldCommit one no
		// repository holds.
		toolsMerge   = "9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a"
		unheldCommit = "9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b"
	)
	linked := &linkedGitHub{
		held:     map[string][]string{"acme/tools": {toolsMerge}},
		pulls:    map[string]string{"acme/tools#9": "merged"},
		branches: map[string][]string{"acme/tools:main": {toolsMerge}},
		failing:  []string{"acme/widgets"},
	}
	handler, database := linkedRepositoriesGitHub(t, linked)
	issue := issueInRepository(t, handler, "RELF", "acme/widgets", "Own repository unread").Key
	uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", afterMergeAndDefaultChecks)
	linkPullRequests(t, handler, issue, "https://github.com/acme/tools/pull/9")
	// acme/tools#9 as the attribution leaves it: the issue's, since the issue links it.
	writeHeadOnBase(t, database, "acme/tools", 9, headCommit, "merged", issue, "main")

	_, toolsAsksBefore := linked.counts("acme/tools")
	_, widgetsAsksBefore := linked.counts("acme/widgets")
	atLinked := recordStored(t, handler, issue, "k4", toolsMerge)
	if repo := storedCommitRepo(t, database, atLinked); repo == nil || *repo != "acme/tools" {
		t.Fatalf("the result at a commit only acme/tools holds stored commit_repo %v, want acme/tools", repo)
	}
	_, toolsAsks := linked.counts("acme/tools")
	_, widgetsAsks := linked.counts("acme/widgets")
	if toolsAsks-toolsAsksBefore != 1 || widgetsAsks-widgetsAsksBefore != 0 {
		t.Errorf("the record read the commit's pull requests through acme/tools %d times and acme/widgets %d times, want once and never",
			toolsAsks-toolsAsksBefore, widgetsAsks-widgetsAsksBefore)
	}

	unheld := recordStored(t, handler, issue, "k4", unheldCommit)
	if repo := storedCommitRepo(t, database, unheld); repo != nil {
		t.Fatalf("the result at a commit no linked repository holds stored commit_repo %q, want none", *repo)
	}
	_, toolsAsksAfter := linked.counts("acme/tools")
	_, widgetsAsksAfter := linked.counts("acme/widgets")
	if toolsAsksAfter-toolsAsks != 0 || widgetsAsksAfter-widgetsAsks != 1 {
		t.Errorf("the record read the unheld commit's pull requests through acme/tools %d times and acme/widgets %d times, want never and once",
			toolsAsksAfter-toolsAsks, widgetsAsksAfter-widgetsAsks)
	}

	stands := standsAt(t, readResults(t, handler, issue), "k4", atLinked.ID,
		"an after_merge result at the linked merge commit recorded while the issue's own repository could not be read",
		model.CommitAtBasis, false)
	if basis := stands.Basis; basis == nil || basis.Branch != "main" || basis.Repo != "acme/tools" {
		t.Fatalf("that result stands at %+v, want the branch main of acme/tools", basis)
	}
}

// A `before_merge` result whose commit only a linked repository holds is read through that
// repository when the results are read, as the record reads it through. A pull request the
// issue links there that opens at that commit after the record, with no event reaching Dispatch
// for it, is learned at the next read, and the result stands at its head, which names the linked
// repository; the issue's own pull request names none. Asked of the issue's own repository, the
// read would name no pull request for such a commit on any read, and the result would stand at
// another commit for good.
func TestABeforeMergeResultAtALinkedCommitIsReadThroughThatRepository(t *testing.T) {
	t.Parallel()
	// pushedCommit is a commit acme/tools holds and acme/widgets does not.
	const pushedCommit = "9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c"
	linked := &linkedGitHub{
		held:  map[string][]string{"acme/widgets": {headCommit}, "acme/tools": {pushedCommit}},
		pulls: map[string]string{"acme/tools#7": "open"},
		heads: map[string]int{},
	}
	handler, database := linkedRepositoriesGitHub(t, linked)
	issue := issueInRepository(t, handler, "RELR", "acme/widgets", "Read through the linked repository").Key
	uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", twoChecks)
	linkPullRequests(t, handler, issue, "https://github.com/acme/tools/pull/7")
	// Dispatch holds the issue's own pull request, and nothing of the linked one yet.
	writeHead(t, database, "acme/widgets", 5, headCommit, "open", issue)

	result := recordStored(t, handler, issue, "k1", pushedCommit)
	if repo := storedCommitRepo(t, database, result); repo == nil || *repo != "acme/tools" {
		t.Fatalf("the result at a commit only acme/tools holds stored commit_repo %v, want acme/tools", repo)
	}

	// acme/tools#7 opens at the result's commit, and no event tells Dispatch of it.
	linked.mu.Lock()
	linked.heads["acme/tools@"+pushedCommit] = 7
	linked.mu.Unlock()
	_, toolsAsksBefore := linked.counts("acme/tools")
	_, widgetsAsksBefore := linked.counts("acme/widgets")

	read := readResults(t, handler, issue)
	stands := standsAt(t, read, "k1", result.ID,
		"a before_merge result at a commit the linked pull request's head reached after the record",
		model.CommitAtBasis, false)
	if basis := stands.Basis; basis == nil || basis.Kind != model.CheckBasisPullRequest || basis.Repo != "acme/tools" ||
		basis.PullRequest == nil || *basis.PullRequest != 7 || basis.Commit != pushedCommit {
		t.Fatalf("that result stands at %+v, want acme/tools#7 at %s", basis, pushedCommit)
	}
	_, toolsAsks := linked.counts("acme/tools")
	_, widgetsAsks := linked.counts("acme/widgets")
	if toolsAsks-toolsAsksBefore != 1 || widgetsAsks-widgetsAsksBefore != 0 {
		t.Errorf("the read read the commit's pull requests through acme/tools %d times and acme/widgets %d times, want once and never",
			toolsAsks-toolsAsksBefore, widgetsAsks-widgetsAsksBefore)
	}
	for _, basis := range checkOf(t, read, "k1").ComparedWith {
		if want := map[int]string{5: "", 7: "acme/tools"}[*basis.PullRequest]; basis.Repo != want {
			t.Errorf("k1 compares with %+v, want #%d to name the repository %q", basis, *basis.PullRequest, want)
		}
	}
}

// A `before_merge` result whose commit a linked repository holds is decided by that repository
// when it heads none of the pull requests compared with, as its record and its read-through are:
// the read asked acme/tools for the commit's pull requests and GitHub named none, so the result
// is at another commit. The issue's own repository, which the App cannot read, has nothing to
// say about a commit of acme/tools, and read through it the result would stand unknown, with the
// reason that Dispatch cannot read the repository, for good.
func TestABeforeMergeResultAtALinkedCommitIsDecidedByThatRepository(t *testing.T) {
	t.Parallel()
	// oldCommit is a commit acme/tools holds that no pull request heads any longer.
	const oldCommit = "9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e"
	linked := &linkedGitHub{
		held:        map[string][]string{"acme/tools": {oldCommit, headCommit}},
		pulls:       map[string]string{"acme/tools#7": "open"},
		heads:       map[string]int{},
		uninstalled: []string{"acme/widgets"},
	}
	handler, database := linkedRepositoriesGitHub(t, linked)
	issue := issueInRepository(t, handler, "RELO", "acme/widgets", "Decided by the linked repository").Key
	uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", twoChecks)
	linkPullRequests(t, handler, issue, "https://github.com/acme/tools/pull/7")
	writeHead(t, database, "acme/tools", 7, headCommit, "open", issue)

	result := recordStored(t, handler, issue, "k1", oldCommit)
	if repo := storedCommitRepo(t, database, result); repo == nil || *repo != "acme/tools" {
		t.Fatalf("the result at a commit only acme/tools holds stored commit_repo %v, want acme/tools", repo)
	}

	stands := standsAt(t, readResults(t, handler, issue), "k1", result.ID,
		"a before_merge result at a commit of the linked repository that heads no pull request",
		model.CommitOtherCommit, false)
	if stands.Reason != "" {
		t.Fatalf("that result stands at another commit with the reason %q, want none", stands.Reason)
	}
}

// One record reads at most linkedPullRequestReads linked pull requests, so an issue's links,
// which nothing bounds, cannot spend a GitHub call each on one record. An issue linking an open
// pull request in each of one repository more than the bound, the last of which holds the
// commit, has the bound's worth read, one pull request and one commit read of each repository,
// and the last left unread: the result is refused, the refusal names the repositories it asked
// and not the one it left, and says how many linked pull requests the bound left unread.
func TestARecordReadsAtMostTheBoundOfLinkedPullRequests(t *testing.T) {
	t.Parallel()
	const commit = "9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d"
	linked := &linkedGitHub{held: map[string][]string{}, pulls: map[string]string{}}
	var repos, urls []string
	for index := 1; index <= linkedPullRequestReads+1; index++ {
		repo := fmt.Sprintf("acme/linked-%02d", index)
		repos = append(repos, repo)
		urls = append(urls, "https://github.com/"+repo+"/pull/1")
		linked.pulls[repo+"#1"] = "open"
	}
	last := repos[len(repos)-1]
	linked.held[last] = []string{commit}
	handler, database := linkedRepositoriesGitHub(t, linked)
	issue := issueInRepository(t, handler, "RELN", "acme/widgets", "Many links").Key
	uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", twoChecks)
	linkPullRequests(t, handler, issue, urls...)
	eventsBefore := resultEventsOn(t, database, issue)

	recorded := recordResult(t, handler, issue, resultBody("k1", "pass", commit, nil))

	code, text := refusalOf(t, recorded)
	if recorded.Code != http.StatusBadRequest || code != "RESULT_COMMIT_UNKNOWN" {
		t.Fatalf("status=%d body=%s, want 400 RESULT_COMMIT_UNKNOWN", recorded.Code, recorded.Body.String())
	}
	if unread := fmt.Sprintf("1 linked pull request was left unread, past the %d one record reads", linkedPullRequestReads); !strings.Contains(text, unread) {
		t.Errorf("the refusal reads %q, want it to say %q", text, unread)
	}
	for _, repo := range repos[:len(repos)-1] {
		if !strings.Contains(text, repo) {
			t.Errorf("the refusal reads %q, want %s, which it asked about the commit, named", text, repo)
		}
	}
	if strings.Contains(text, last) {
		t.Errorf("the refusal reads %q, want %s, which the bound left unread, not named", text, last)
	}
	if stored := resultEventsOn(t, database, issue) - eventsBefore; stored != 0 {
		t.Errorf("the refused result appended %d result.recorded events, want none", stored)
	}
	read := 0
	for _, repo := range repos {
		reads, _ := linked.counts(repo)
		read += reads
		if repo == last && reads != 0 {
			t.Errorf("the record read the state of %s's linked pull request %d times, want none past the bound", repo, reads)
		}
	}
	if read != linkedPullRequestReads {
		t.Errorf("the record read the state of %d linked pull requests, want %d", read, linkedPullRequestReads)
	}
}

// R17 of the spec of several open pull requests: a repository Dispatch cannot read stores the result.
// An issue linking twelve pull requests of one repository the App is not installed on: GitHub's
// answer about the first is its answer about every pull request of that repository, so Dispatch
// cannot say whether that repository holds the commit, however many links name it. The result at
// a commit acme/widgets does not hold is stored, taken to be acme/widgets', and no pull request
// of acme/elsewhere is read past that one answer.
func TestManyLinksOfARepositoryDispatchCannotReadStoreTheResult(t *testing.T) {
	t.Parallel()
	const (
		commit = "9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e"
		links  = linkedPullRequestReads + 2
	)
	linked := &linkedGitHub{
		held:        map[string][]string{"acme/widgets": {headCommit}},
		pulls:       map[string]string{},
		uninstalled: []string{"acme/elsewhere"},
	}
	var urls []string
	for number := 101; number < 101+links; number++ {
		urls = append(urls, fmt.Sprintf("https://github.com/acme/elsewhere/pull/%d", number))
	}
	handler, database := linkedRepositoriesGitHub(t, linked)
	issue := issueInRepository(t, handler, "RELC", "acme/widgets", "Many links of one repository the App cannot read").Key
	uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", twoChecks)
	linkPullRequests(t, handler, issue, urls...)
	eventsBefore := resultEventsOn(t, database, issue)

	recorded := recordResult(t, handler, issue, resultBody("k1", "pass", commit, nil))

	storedAtOwnRepository(t, database, recorded, issue, eventsBefore)
	if reads, _ := linked.counts("acme/elsewhere"); reads != 0 {
		t.Errorf("the record read the state of %d of acme/elsewhere's pull requests, want none: the App is not installed there", reads)
	}
}

// A linked pull request of a repository whose name GitHub's naming rule refuses is set aside and
// named. The issue links a pull request of `acme/.git`, which the links' own pattern takes, since
// GitHub's names take `.git`, and which refs.SplitGitHubRepo refuses, since the name is empty
// once a trailing `.git` is trimmed: so Dispatch asks GitHub nothing about it, and the repository
// counts for nothing, as one Dispatch cannot read does. The commit no repository holds is refused
// 400 RESULT_COMMIT_UNKNOWN, and the refusal names the repository it set aside and that GitHub's
// naming rule refuses its name, and does not name it as a repository Dispatch could not read,
// which a grant of the App would not mend.
//
// The negative control is the refusal as linkCannotCount left it, which read the name rule's
// answer as one about a repository Dispatch cannot read and then kept only such repositories, so
// the refusal said nothing of the link at all.
func TestARefusalNamesALinkedRepositoryGitHubsNamingRuleRefuses(t *testing.T) {
	t.Parallel()
	const commit = "9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f"
	linked := &linkedGitHub{held: map[string][]string{"acme/widgets": {headCommit}}, pulls: map[string]string{}}
	handler, database := linkedRepositoriesGitHub(t, linked)
	issue := issueInRepository(t, handler, "RELG", "acme/widgets", "A link GitHub's naming rule refuses").Key
	uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", twoChecks)
	linkPullRequests(t, handler, issue, "https://github.com/acme/.git/pull/3")
	eventsBefore := resultEventsOn(t, database, issue)

	recorded := recordResult(t, handler, issue, resultBody("k1", "pass", commit, nil))

	code, text := refusalOf(t, recorded)
	if recorded.Code != http.StatusBadRequest || code != "RESULT_COMMIT_UNKNOWN" {
		t.Fatalf("status=%d body=%s, want 400 RESULT_COMMIT_UNKNOWN", recorded.Code, recorded.Body.String())
	}
	if want := "; Dispatch set aside the links to pull requests of acme/.git, which GitHub's naming rule refuses as a repository's name"; !strings.Contains(text, want) {
		t.Errorf("the refusal reads %q, want it to say %q", text, want)
	}
	if strings.Contains(text, "could not read") {
		t.Errorf("the refusal reads %q, want acme/.git, which the naming rule refuses, not named as a repository Dispatch could not read", text)
	}
	if asked, _, _ := strings.Cut(text, "; "); strings.Contains(asked, "acme/.git") {
		t.Errorf("the refusal reads %q, want acme/.git, which it never asked about the commit, not named as asked", text)
	}
	if reads, asks := linked.counts("acme/.git"); reads != 0 || asks != 0 {
		t.Errorf("the record asked GitHub about acme/.git %d and %d times, want never", reads, asks)
	}
	if stored := resultEventsOn(t, database, issue) - eventsBefore; stored != 0 {
		t.Errorf("the refused result appended %d result.recorded events, want none", stored)
	}
}

// A result's dry run judges a commit of a linked repository as the record does, because the two
// share one judgement (readResultRequest): a commit only the linked repository holds is taken, a
// commit no repository holds is refused 400 RESULT_COMMIT_UNKNOWN, and a linked read that did not
// answer and a linked repository the App cannot read each take the result, taken to be the
// issue's own repository's. Each dry run answers what the record of the same body then answers,
// word for word, and stores nothing. A dry run that skipped the linked reads would answer a line
// where the record refuses, and tell a role its result will be taken when it will not.
//
// The negative control is a dry run that asks no repository about the commit: it answers 200 in
// the refused row.
func TestAResultsDryRunJudgesALinkedCommitAsTheRecordDoes(t *testing.T) {
	t.Parallel()
	// linkedCommit is a commit acme/tools holds and acme/widgets does not.
	const linkedCommit = "9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f"
	for _, row := range []struct {
		name string
		// github is how GitHub answers, beside the open pull request acme/tools#7.
		github func(*linkedGitHub)
		// status and code are the record's answer; the dry run answers 200 where the record 201s.
		// repo is the commit_repo the record stores where it takes the result, "" for none.
		status int
		code   string
		repo   string
	}{
		{"a commit only the linked repository holds", func(*linkedGitHub) {}, http.StatusCreated, "", "acme/tools"},
		{"a commit no repository holds", func(linked *linkedGitHub) { linked.held["acme/tools"] = nil },
			http.StatusBadRequest, "RESULT_COMMIT_UNKNOWN", ""},
		// R17 of the spec of several open pull requests: a repository Dispatch cannot read stores the result.
		{"a linked read GitHub answered 500", func(linked *linkedGitHub) { linked.erroring = []string{"acme/tools"} },
			http.StatusCreated, "", ""},
		// R17 of the spec of several open pull requests: a repository Dispatch cannot read stores the result.
		{"a linked repository the App cannot read", func(linked *linkedGitHub) { linked.refusing = []string{"acme/tools"} },
			http.StatusCreated, "", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			linked := &linkedGitHub{
				held:  map[string][]string{"acme/widgets": {headCommit}, "acme/tools": {linkedCommit}},
				pulls: map[string]string{"acme/tools#7": "open"},
			}
			row.github(linked)
			handler, database := linkedRepositoriesGitHub(t, linked)
			issue := issueInRepository(t, handler, "RELD", "acme/widgets", "A dry run at a linked commit").Key
			uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", twoChecks)
			linkPullRequests(t, handler, issue, "https://github.com/acme/tools/pull/7")
			body := resultBody("k1", "pass", linkedCommit, nil)
			eventsBefore := resultEventsOn(t, database, issue)

			dry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/results/dry-run", body, "alice")
			if stored := resultEventsOn(t, database, issue) - eventsBefore; stored != 0 {
				t.Errorf("the dry run appended %d result.recorded events, want none", stored)
			}
			if rows := resultRowsOn(t, database, issue); rows != 0 {
				t.Errorf("the dry run left %d check_results rows, want none", rows)
			}
			recorded := recordResult(t, handler, issue, body)

			if recorded.Code != row.status {
				t.Fatalf("the record: status=%d body=%s, want %d", recorded.Code, recorded.Body.String(), row.status)
			}
			if row.code == "" {
				if dry.Code != http.StatusOK {
					t.Fatalf("the dry run: status=%d body=%s, want 200 where the record is taken", dry.Code, dry.Body.String())
				}
				line := decodeBody[resultDryRun](t, dry).Line
				if want := "pass at " + linkedCommit; !strings.Contains(line, want) {
					t.Errorf("the dry run's line = %q, want it to say %q", line, want)
				}
				stored := ""
				if repo := storedCommitRepo(t, database, decodeBody[recordedResult](t, recorded).Result); repo != nil {
					stored = *repo
				}
				if stored != row.repo {
					t.Errorf("the record stored commit_repo %q, want %q (\"\" is none)", stored, row.repo)
				}
				return
			}
			if dry.Code != row.status {
				t.Fatalf("the dry run: status=%d body=%s, want the record's %d", dry.Code, dry.Body.String(), row.status)
			}
			dryCode, dryText := refusalOf(t, dry)
			code, text := refusalOf(t, recorded)
			if dryCode != row.code || code != row.code {
				t.Errorf("the dry run refused %s and the record %s, want both %s", dryCode, code, row.code)
			}
			if dryText != text {
				t.Errorf("the dry run's refusal reads %q, want the record's %q", dryText, text)
			}
		})
	}
}
