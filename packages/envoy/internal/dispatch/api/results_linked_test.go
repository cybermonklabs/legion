package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
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
		held:  map[string][]string{"acme/tools": {toolsMerge}},
		pulls: map[string]string{"acme/tools#9": "merged"},
		merged: map[string]linkedMerge{"acme/tools#9": {title: "The linked work", headRef: "work", headSHA: headCommit,
			base: "main", mergeCommit: toolsMerge, mergedAt: "2026-11-20T12:00:00Z"}},
		branches: map[string][]string{"acme/tools:main": {toolsMerge}},
		failing:  []string{"acme/widgets"},
	}
	handler, database := linkedRepositoriesGitHub(t, linked)
	issue := issueInRepository(t, handler, "RELF", "acme/widgets", "Own repository unread").Key
	uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", afterMergeAndDefaultChecks)
	linkPullRequests(t, handler, issue, "https://github.com/acme/tools/pull/9")
	writeHeadOnBase(t, database, "acme/tools", 9, headCommit, "merged", "", "main")

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
// issue links there whose head moves to that commit after the record, with no event reaching
// Dispatch for it, is learned at the next read, and the result stands at its head, which names
// the linked repository; the issue's own pull request names none. Asked of the issue's own
// repository, the read would name no pull request for such a commit on any read, and the result
// would stand at another commit for good. A first read of the results, before the record, reads
// the linked pull request at its head then (server.readLinkedHeads), so the read after the move
// holds a row of it that owes no read, and only the read-through of the result's commit can
// learn the move.
func TestABeforeMergeResultAtALinkedCommitIsReadThroughThatRepository(t *testing.T) {
	t.Parallel()
	const (
		// pushedCommit is a commit acme/tools holds and acme/widgets does not, and earlierCommit
		// the head of acme/tools#7 before the push.
		pushedCommit  = "9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c"
		earlierCommit = "9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d"
	)
	linked := &linkedGitHub{
		held:  map[string][]string{"acme/widgets": {headCommit}, "acme/tools": {pushedCommit, earlierCommit}},
		pulls: map[string]string{"acme/tools#7": "open"},
		heads: map[string]int{"acme/tools@" + earlierCommit: 7},
	}
	handler, database := linkedRepositoriesGitHub(t, linked)
	issue := issueInRepository(t, handler, "RELR", "acme/widgets", "Read through the linked repository").Key
	uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", twoChecks)
	linkPullRequests(t, handler, issue, "https://github.com/acme/tools/pull/7")
	// Dispatch holds the issue's own pull request, and nothing of the linked one yet.
	writeHead(t, database, "acme/widgets", 5, headCommit, "open", issue)
	readResults(t, handler, issue)
	var heldAtEarlier int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from pull_request_heads where repo = 'acme/tools' and number = 7 and head_sha = $1`,
		earlierCommit).Scan(&heldAtEarlier); err != nil {
		t.Fatalf("read the rows of acme/tools#7: %v", err)
	}
	if owed := owedReadsOf(t, database, "acme/tools", 7); heldAtEarlier != 1 || owed != 0 {
		t.Fatalf("after the first read Dispatch holds %d rows of acme/tools#7 at %s owing %d reads, want one owing none",
			heldAtEarlier, earlierCommit, owed)
	}

	result := recordStored(t, handler, issue, "k1", pushedCommit)
	if repo := storedCommitRepo(t, database, result); repo == nil || *repo != "acme/tools" {
		t.Fatalf("the result at a commit only acme/tools holds stored commit_repo %v, want acme/tools", repo)
	}

	// acme/tools#7's head moves to the result's commit, and no event tells Dispatch of it.
	linked.mu.Lock()
	delete(linked.heads, "acme/tools@"+earlierCommit)
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
	// R10 and R19 of the spec of several open pull requests: a linked pull request is the issue's
	// once its texts are stored, as a read of GitHub stores them; a row with no texts keeps the
	// issue it stores, here none.
	storeLinkedHead(t, database, "acme/tools", 7, headCommit)

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

	assertStoredAsOwnRepositorys(t, database, issue, eventsBefore, recorded)
	if reads, _ := linked.counts("acme/elsewhere"); reads != 0 {
		t.Errorf("the record read the state of %d of acme/elsewhere's pull requests, want none: the App is not installed there", reads)
	}
}

// A linked pull request is read under a name GitHub can hold, so no record sets a linked
// repository aside for GitHub's naming rule. The link rule refuses a link that spells such a name
// (delivery.linkedPullRequest holds a link's own name to refs.SplitGitHubRepo), and a rename pair
// whose new name is one, `acme/.git` here, which only SQL by hand writes, is dropped when the
// server reads the pairs (refs.NewRenames): the issue links oldco/gizmo#3, the pair of
// oldco/gizmo names `acme/.git`, and the record reads #3 under oldco/gizmo, its name today, which
// GitHub answers 404 for. The commit no repository holds is refused 400 RESULT_COMMIT_UNKNOWN,
// naming acme/widgets, which it asked, and neither acme/.git nor a repository set aside. A second
// issue links acme/.git#4 itself, a link the rule refuses: it names no repository, so its refusal
// sets nothing aside and asks GitHub nothing about acme/.git either.
//
// A server that holds a pair stores every link under its repository's new name
// (refs.Renames.CanonicalURL), so the link by the old name is written first, on a server built
// with no pair, as a release before stored it. The pair is then seeded by SQL, as the last
// release left it, and the results are recorded on a server built after the seed, which reads
// the pair once, as it is built.
//
// The negative control is the start before it dropped such a pair: the record called the link's
// repository `acme/.git`, asked GitHub nothing about it and named it as set aside.
func TestALinkWhoseRenamePairGitHubCannotHoldIsReadUnderItsOwnName(t *testing.T) {
	t.Parallel()
	const commit = "9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f"
	linked := &linkedGitHub{held: map[string][]string{"acme/widgets": {headCommit}}, pulls: map[string]string{}}
	options := linkedRepositoriesOptions(t, linked)
	handler, database, _ := newTestServer(t, options)
	issue := issueInRepository(t, handler, "RELG", "acme/widgets", "A link whose rename pair GitHub cannot hold").Key
	uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", twoChecks)
	linkPullRequests(t, handler, issue, "https://github.com/oldco/gizmo/pull/3")
	var stored int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from issue_external_links where issue_key = $1 and url = 'https://github.com/oldco/gizmo/pull/3'
	`, issue).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("SETUP: %s holds %d links under oldco/gizmo (%v), want 1: the case is a link stored under the old name", issue, stored, err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into repo_projects (repo, project, created_by, renamed_to)
		values ('oldco/gizmo', 'RELG', '{"kind":"system","id":"api-test"}'::jsonb, 'acme/.git')`); err != nil {
		t.Fatalf("store oldco/gizmo as renamed to acme/.git: %v", err)
	}
	options.store, options.renames = storetest.SecondProcess(t, database), readRenames(t, database)
	if dropped := options.renames.Dropped(); len(dropped) != 1 || options.renames.Canonical("oldco/gizmo") != "oldco/gizmo" {
		t.Fatalf("the server's pairs dropped %v and call oldco/gizmo %q, want the pair to acme/.git dropped and oldco/gizmo its own", dropped, options.renames.Canonical("oldco/gizmo"))
	}
	handler, _, _ = newTestServer(t, options)
	eventsBefore := resultEventsOn(t, database, issue)

	recorded := recordResult(t, handler, issue, resultBody("k1", "pass", commit, nil))

	code, text := refusalOf(t, recorded)
	if recorded.Code != http.StatusBadRequest || code != "RESULT_COMMIT_UNKNOWN" {
		t.Fatalf("status=%d body=%s, want 400 RESULT_COMMIT_UNKNOWN", recorded.Code, recorded.Body.String())
	}
	if strings.Contains(text, "set aside") || strings.Contains(text, "acme/.git") || strings.Contains(text, "could not read") {
		t.Errorf("the refusal reads %q, want no repository set aside or unread, and no word of acme/.git", text)
	}
	if reads, _ := linked.counts("oldco/gizmo"); reads != 1 {
		t.Errorf("the record read the state of oldco/gizmo#3 %d times, want once: under its own name, the name it has with the pair dropped", reads)
	}
	if stored := resultEventsOn(t, database, issue) - eventsBefore; stored != 0 {
		t.Errorf("the refused result appended %d result.recorded events, want none", stored)
	}

	direct := issueOfProject(t, handler, "RELG", "Zulu refused link", twoChecks)
	linkPullRequests(t, handler, direct, "https://github.com/acme/.git/pull/4")
	recorded = recordResult(t, handler, direct, resultBody("k1", "pass", commit, nil))
	code, text = refusalOf(t, recorded)
	if recorded.Code != http.StatusBadRequest || code != "RESULT_COMMIT_UNKNOWN" {
		t.Fatalf("the link acme/.git#4: status=%d body=%s, want 400 RESULT_COMMIT_UNKNOWN", recorded.Code, recorded.Body.String())
	}
	if strings.Contains(text, "set aside") || strings.Contains(text, "acme/.git") {
		t.Errorf("the refusal of a result of %s reads %q, want no repository set aside: its link names no pull request", direct, text)
	}
	if reads, asks := linked.counts("acme/.git"); reads != 0 || asks != 0 {
		t.Errorf("the records asked GitHub about acme/.git %d and %d times, want never", reads, asks)
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

// storeLinkedHead stores repo's pull request number open at sha through the writer of a read of
// GitHub, with texts that describe that head and name no issue, its head branch in repo: a
// row whose issue the statement reads from the links alone.
func storeLinkedHead(t *testing.T, database *store.Store, repo string, number int, sha string) {
	t.Helper()
	storeLinkedHeadOn(t, database, repo, number, sha, "open", "main")
}

// storeLinkedHeadOn is storeLinkedHead for a pull request in state on the base branch base: a
// merged one is a row delivery.Attributor.MergeIn answers the issue's merges from. No caller's
// server holds a rename pair, so it writes with unrenamed.
func storeLinkedHeadOn(t *testing.T, database *store.Store, repo string, number int, sha, state, base string) {
	t.Helper()
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	if err := unrenamed().UpsertPullRequestHead(context.Background(), database.Pool, delivery.PullRequestHead{
		Repo: repo, Number: number, HeadSHA: sha, HeadRef: "topic", BaseRef: base, State: state,
		Title: "A change", URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number), EventAt: &at,
		Texts: &delivery.PullRequestTexts{HeadRepo: repo, BaseRef: base, Title: "A change", CommitMessages: []string{}, MessagesRead: true},
	}); err != nil {
		t.Fatalf("store the head of %s#%d: %v", repo, number, err)
	}
}

// A pull request an issue links by its URL is that issue's in any repository, the /issues/N form
// of the URL as much as /pull/N, so a commit its repository holds is one the issue's result may
// name (R17 of the spec of several open pull requests). The issue of acme/widgets links
// acme/tools#7 in its /issues/7 form, which the record's own reading of the links (the /pull/
// form alone) does not name. A result at the head Dispatch read of #7 is taken with no read of
// GitHub at all and kept with acme/tools: acme/tools' commit reads answer 503 here, so a record
// that asked GitHub would store the result with no repository. A result at another commit
// acme/tools holds is asked of acme/tools, the repository of the issue's pull request, and kept
// with it. The control is the same record with no head of #7 stored, where the link alone names
// no repository: GitHub proves acme/widgets does not hold the commit, and it is refused
// RESULT_COMMIT_UNKNOWN.
func TestAResultAtACommitOfAPullRequestTheIssueLinksInAnotherRepositoryIsKeptWithThatRepository(t *testing.T) {
	t.Parallel()
	const (
		toolsHead  = "9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a"
		toolsOlder = "9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b"
	)
	for _, headStored := range []bool{true, false} {
		t.Run(map[bool]string{true: "the_head_is_stored", false: "control_no_head_stored"}[headStored], func(t *testing.T) {
			linked := &linkedGitHub{held: map[string][]string{"acme/widgets": {headCommit}, "acme/tools": {toolsHead, toolsOlder}}, pulls: map[string]string{}}
			if headStored {
				linked.failing = []string{"acme/tools"}
			}
			handler, database := linkedRepositoriesGitHub(t, linked)
			issue := issueInRepository(t, handler, "RELI", "acme/widgets", "A pull request linked in its issues form").Key
			uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", twoChecks)
			linkPullRequests(t, handler, issue, "https://github.com/acme/tools/issues/7")
			if !headStored {
				recorded := recordResult(t, handler, issue, resultBody("k1", "pass", toolsOlder, nil))
				if code, _ := refusalOf(t, recorded); recorded.Code != http.StatusBadRequest || code != "RESULT_COMMIT_UNKNOWN" {
					t.Fatalf("a result at a commit of acme/tools with no head of #7 stored: status=%d body=%s, want 400 RESULT_COMMIT_UNKNOWN",
						recorded.Code, recorded.Body.String())
				}
				return
			}
			storeLinkedHead(t, database, "acme/tools", 7, toolsHead)

			atHead := recordStored(t, handler, issue, "k1", toolsHead)
			if repo := storedCommitRepo(t, database, atHead); repo == nil || *repo != "acme/tools" {
				t.Fatalf("the result at #7's head is kept with %v, want acme/tools, taken from the head's row with no read of GitHub", repo)
			}
			linked.failing = nil
			atOlder := recordStored(t, handler, issue, "k1", toolsOlder)
			if repo := storedCommitRepo(t, database, atOlder); repo == nil || *repo != "acme/tools" {
				t.Fatalf("the result at another commit of acme/tools is kept with %v, want acme/tools", repo)
			}
		})
	}
}

// A result at the head of an open or merged pull request of the check's issue, as Dispatch last
// read it, is stored with no read of GitHub (R17 of the spec of several open pull requests,
// refuseUnknownCommit): the head's row says which repository holds the commit. Here GitHub would
// answer that no repository holds the commit, so a record that asked GitHub about it would be
// refused with 400 RESULT_COMMIT_UNKNOWN; the result at the head of the issue's #41 is stored all
// the same. The control is a commit that heads no pull request of the issue, which GitHub's same
// answer refuses.
func TestAResultAtAHeadOfTheIssueDispatchHoldsIsStoredWithNoReadOfGitHub(t *testing.T) {
	t.Parallel()
	linked := &linkedGitHub{held: map[string][]string{}, pulls: map[string]string{}}
	handler, database := linkedRepositoriesGitHub(t, linked)
	issue := issueInRepository(t, handler, "KNWN", "acme/tools", "A head Dispatch holds")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	writeReadMergedRow(t, database, "acme/tools", 41, headCommit, issue.Key)
	if stored := recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)); stored.Code != http.StatusCreated {
		t.Fatalf("a result at the head of %s's #41, which GitHub would say no repository holds: status=%d body=%s; want 201, taken from the head's row with no read of GitHub",
			issue.Key, stored.Code, stored.Body.String())
	}
	other := strings.Repeat("e", 40)
	refused := recordResult(t, handler, issue.Key, resultBody("k1", "pass", other, nil))
	if refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), `"code":"RESULT_COMMIT_UNKNOWN"`) {
		t.Fatalf("control: a result at %s, which heads no pull request of %s and which GitHub says no repository holds: status=%d body=%s; want 400 RESULT_COMMIT_UNKNOWN",
			other, issue.Key, refused.Code, refused.Body.String())
	}
}
