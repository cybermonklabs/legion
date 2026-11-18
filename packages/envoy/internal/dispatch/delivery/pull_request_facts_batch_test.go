package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// batchGitHub is GitHub as the batched read of facts reads it (readFactsBatch): the GraphQL
// query of aliased repositories and pull requests, the REST read of one pull request, and the
// REST pages of its commits. It holds the pull requests of each repository a test sets. A
// repository in gone answers null with NOT_FOUND on its own alias. A number the repository does
// not hold answers null with NOT_FOUND on its own alias path, which is the response key of the
// repository and then of the pull request. oversize, where set, says whether the answer to a
// query of those numbers is above the client's 1 MiB: such an answer carries 2 MiB of padding.
// It records each query, each REST read of a pull request, and each page of commits asked.
//
// The fake answers the batched query before its mux sees it (fakeGitHub.facts). answerAbsence
// answers the absence query (absenceQuery) on POST /graphql from the same pull requests. A
// repository can be on a second installation (install). The bearer token of each query names its
// installation (installationIDFromToken), and each query records it (queriesOf). GitHub's GraphQL
// rate limit answers each query of limitedInstallation.
type batchGitHub struct {
	t    *testing.T
	fake *fakeGitHub

	mu                  sync.Mutex
	pulls               map[string]map[int]batchPull
	gone                map[string]bool
	oversize            func(numbers []int) bool
	failCommits         bool
	limitedInstallation int64
	queries             []batchQuery
	absenceAsked        []string
	pullReads           map[string]int
	commitPages         []int
	commitBearer        []string
}

// batchPull is one pull request batchGitHub holds. state is open, closed or merged. headRepo is
// the repository of the head branch, "" for the pull request's own repository. queryMessages,
// where above 0, is the count of messages the query answers, fewer than GitHub's first 100. The
// query answers the number of an undecodable pull request as a JSON string, which no factsNode
// decodes.
type batchPull struct {
	state, head, branch, title, body, headRepo string
	updatedAt                                  time.Time
	messages                                   []string
	queryMessages                              int
	undecodable                                bool
}

// batchQuery is one GraphQL query that batchGitHub was asked. It holds its text, its bearer and
// that token's installation. It holds the numbers it asked for in the order of its aliases, and
// each as "owner/repo#number". It holds the count of its repository aliases, and whether the fake
// answered it with a rate limit.
type batchQuery struct {
	text, bearer string
	installation int64
	numbers      []int
	pulls        []string
	repositories int
	limited      bool
}

// batchAlias reads the aliases of a facts query in their order: a repository alias and the names
// of its two variables, or a pull request alias and its number.
var batchAlias = regexp.MustCompile(`(r\d+): repository\(owner: \$(\w+), name: \$(\w+)\)|(p\d+): pullRequest\(number: (\d+)\)`)

func serveBatchGitHub(t *testing.T, fake *fakeGitHub) *batchGitHub {
	t.Helper()
	g := &batchGitHub{t: t, fake: fake, pulls: map[string]map[int]batchPull{}, gone: map[string]bool{}, pullReads: map[string]int{}}
	fake.facts = g.answerQuery
	fake.handle("POST /graphql", g.answerAbsence)
	fake.handle("GET /repos/{owner}/{repo}/pulls/{number}", g.answerPull)
	fake.handle("GET /repos/{owner}/{repo}/pulls/{number}/commits", g.answerCommits)
	return g
}

// install puts each of repos on installation id, which GET /repos/{owner}/{repo}/installation
// then answers (fakeGitHub.installationOf). Call it before newTestClient.
func (g *batchGitHub) install(id int64, repos ...string) {
	if g.fake.installationOf == nil {
		g.fake.installationOf = map[string]int64{}
	}
	for _, repo := range repos {
		g.fake.installationOf[repo] = id
	}
}

// limitInstallation makes GitHub answer each GraphQL query of installation id with its GraphQL
// rate limit.
func (g *batchGitHub) limitInstallation(id int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limitedInstallation = id
}

// queriesOf is the count of the batched queries sent with a token of installation id.
func (g *batchGitHub) queriesOf(id int64) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	count := 0
	for _, query := range g.queries {
		if query.installation == id {
			count++
		}
	}
	return count
}

// absenceQueries is each pull request the absence query asked about, as "owner/repo#number".
func (g *batchGitHub) absenceQueries() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.absenceAsked)
}

// pullsAsked is each pull request that a batched query asked about, as "owner/repo#number", in
// the order of the queries and of their aliases.
func (g *batchGitHub) pullsAsked() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var pulls []string
	for _, query := range g.queries {
		pulls = append(pulls, query.pulls...)
	}
	return pulls
}

// set makes GitHub hold pull request number of repo.
func (g *batchGitHub) set(repo string, number int, pull batchPull) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pulls[repo] == nil {
		g.pulls[repo] = map[int]batchPull{}
	}
	g.pulls[repo][number] = pull
}

// openPull is an open pull request of repo whose head is in repo, with one commit.
func openPull(number int, updatedAt time.Time) batchPull {
	return batchPull{
		state: "open", head: fakeSHA("h", number), branch: fmt.Sprintf("work/change-%d", number),
		title: fmt.Sprintf("Change %d", number), body: fmt.Sprintf("The body of change %d.", number),
		updatedAt: updatedAt, messages: []string{fmt.Sprintf("Work on change %d.", number)},
	}
}

func (g *batchGitHub) asked() []batchQuery {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.queries)
}

func (g *batchGitHub) restReadsOf(repo string, number int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pullReads[fmt.Sprintf("%s#%d", repo, number)]
}

func (g *batchGitHub) restReads() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	total := 0
	for _, n := range g.pullReads {
		total += n
	}
	return total
}

func (g *batchGitHub) pagesAsked() []int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.commitPages)
}

func (g *batchGitHub) bearersOfPages() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.commitBearer)
}

func (g *batchGitHub) answerQuery(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		g.t.Errorf("decode a GraphQL request: %v", err)
		http.Error(w, `{"message":"Problems parsing JSON"}`, http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	query := batchQuery{text: request.Query, bearer: r.Header.Get("Authorization"), installation: installationIDFromToken(r)}
	data := map[string]any{}
	var refusals []any
	var current map[string]any
	var repoAlias, repo string
	for _, alias := range batchAlias.FindAllStringSubmatch(request.Query, -1) {
		if alias[1] != "" {
			owner, _ := request.Variables[alias[2]].(string)
			name, _ := request.Variables[alias[3]].(string)
			repoAlias, repo = alias[1], owner+"/"+name
			query.repositories++
			if g.gone[repo] {
				current = nil
				data[repoAlias] = nil
				refusals = append(refusals, map[string]any{"type": "NOT_FOUND", "path": []any{repoAlias},
					"message": fmt.Sprintf("Could not resolve to a Repository with the name '%s'.", repo)})
				continue
			}
			current = map[string]any{}
			data[repoAlias] = current
			continue
		}
		number, _ := strconv.Atoi(alias[5])
		query.numbers = append(query.numbers, number)
		query.pulls = append(query.pulls, fmt.Sprintf("%s#%d", repo, number))
		if current == nil {
			continue
		}
		pull, held := g.pulls[repo][number]
		if !held {
			current[alias[4]] = nil
			refusals = append(refusals, map[string]any{"type": "NOT_FOUND", "path": []any{repoAlias, alias[4]},
				"message": fmt.Sprintf("Could not resolve to a PullRequest with the number of %d.", number)})
			continue
		}
		current[alias[4]] = pull.node(repo, number)
	}
	query.limited = g.limitedInstallation != 0 && query.installation == g.limitedInstallation
	g.queries = append(g.queries, query)
	if query.limited {
		answerGraphQLRateLimit(w)
		return
	}
	answer := map[string]any{"data": data}
	if len(refusals) > 0 {
		answer["errors"] = refusals
	}
	if g.oversize != nil && g.oversize(query.numbers) {
		answer["extensions"] = map[string]any{"padding": strings.Repeat("x", 2<<20)}
	}
	mustEncode(g.t, w, answer)
}

// answerAbsence answers the absence query (absenceQuery) from the pull requests that the fake
// holds, in GitHub's shapes (pullRequestAbsent's doc comment). A repository in gone answers null
// with NOT_FOUND on the path repository. Any other repository answers the count of its pull
// requests beside the pull request. For a number it does not hold, it answers null beside the
// count, with NOT_FOUND on the path repository.pullRequest. GitHub's GraphQL rate limit answers a
// query of limitedInstallation. It records each query (absenceQueries). Any other query is a
// test's mistake.
func (g *batchGitHub) answerAbsence(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		g.t.Errorf("decode a GraphQL request: %v", err)
		http.Error(w, `{"message":"Problems parsing JSON"}`, http.StatusBadRequest)
		return
	}
	if !strings.Contains(request.Query, "pullRequest(number: $number)") {
		g.t.Errorf("POST /graphql asked %q; this fake answers the facts query and the absence query only", request.Query)
		http.Error(w, `{"message":"unexpected query"}`, http.StatusBadRequest)
		return
	}
	owner, _ := request.Variables["owner"].(string)
	name, _ := request.Variables["name"].(string)
	number, _ := request.Variables["number"].(float64)
	repo := owner + "/" + name
	g.mu.Lock()
	g.absenceAsked = append(g.absenceAsked, fmt.Sprintf("%s#%d", repo, int(number)))
	limited := g.limitedInstallation != 0 && installationIDFromToken(r) == g.limitedInstallation
	gone := g.gone[repo]
	_, held := g.pulls[repo][int(number)]
	counted := map[string]any{"totalCount": len(g.pulls[repo])}
	g.mu.Unlock()
	switch {
	case limited:
		answerGraphQLRateLimit(w)
	case gone:
		mustEncode(g.t, w, nullRepositoryAnswer(repo))
	case held:
		mustEncode(g.t, w, map[string]any{"data": map[string]any{"repository": map[string]any{
			"pullRequests": counted, "pullRequest": map[string]any{"number": int(number)}}}})
	default:
		mustEncode(g.t, w, map[string]any{
			"data": map[string]any{"repository": map[string]any{"pullRequests": counted, "pullRequest": nil}},
			"errors": []any{map[string]any{"type": "NOT_FOUND", "path": []any{"repository", "pullRequest"},
				"message": fmt.Sprintf("Could not resolve to a PullRequest with the number of %d.", int(number))}},
		})
	}
}

func (g *batchGitHub) answerPull(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	number, _ := strconv.Atoi(r.PathValue("number"))
	g.mu.Lock()
	g.pullReads[fmt.Sprintf("%s#%d", repo, number)]++
	pull, held := g.pulls[repo][number]
	g.mu.Unlock()
	if !held {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
		return
	}
	mustEncode(g.t, w, pull.rest(repo, number))
}

// answerCommits pages the commits of a pull request as GitHub pages them: per_page of them from
// page, oldest first.
func (g *batchGitHub) answerCommits(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	number, _ := strconv.Atoi(r.PathValue("number"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	g.mu.Lock()
	g.commitPages = append(g.commitPages, page)
	g.commitBearer = append(g.commitBearer, r.Header.Get("Authorization"))
	messages, failing := g.pulls[repo][number].messages, g.failCommits
	g.mu.Unlock()
	if failing {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"Server Error"}`)
		return
	}
	if page < 1 || perPage < 1 {
		g.t.Errorf("a page of commits of %s#%d asked page=%q per_page=%q", repo, number, r.URL.Query().Get("page"), r.URL.Query().Get("per_page"))
	}
	from, to := min(len(messages), (page-1)*perPage), min(len(messages), page*perPage)
	commits := make([]map[string]any, 0, to-from)
	for i, message := range messages[from:to] {
		commits = append(commits, map[string]any{"sha": fmt.Sprintf("%040d", from+i+1), "commit": map[string]any{"message": message}})
	}
	mustEncode(g.t, w, commits)
}

func (p batchPull) headRepository(repo string) string {
	if p.headRepo == "" {
		return repo
	}
	return p.headRepo
}

// node is the pull request as GitHub's GraphQL API answers the fields of the facts query.
func (p batchPull) node(repo string, number int) map[string]any {
	answered := p.messages[:min(factsPage, len(p.messages))]
	if p.queryMessages > 0 {
		answered = answered[:min(p.queryMessages, len(answered))]
	}
	nodes := make([]any, 0, len(answered))
	for _, message := range answered {
		nodes = append(nodes, map[string]any{"commit": map[string]any{"message": message}})
	}
	var numberField any = number
	if p.undecodable {
		numberField = strconv.Itoa(number)
	}
	var mergedAt, mergeCommit any
	if p.state == "merged" {
		mergedAt = p.updatedAt.UTC().Format(time.RFC3339)
		mergeCommit = map[string]any{"oid": fakeSHA("m", number)}
	}
	return map[string]any{
		"number": numberField, "title": p.title, "body": p.body,
		"url":   fmt.Sprintf("https://github.com/%s/pull/%d", repo, number),
		"state": strings.ToUpper(p.state), "merged": p.state == "merged", "mergedAt": mergedAt,
		"updatedAt":   p.updatedAt.UTC().Format(time.RFC3339),
		"headRefName": p.branch, "headRefOid": p.head, "baseRefName": "main",
		"headRepository": map[string]any{"nameWithOwner": p.headRepository(repo)},
		"mergeCommit":    mergeCommit,
		"commits":        map[string]any{"totalCount": len(p.messages), "nodes": nodes},
	}
}

// rest is the pull request as GET /repos/{owner}/{repo}/pulls/{number} answers it.
func (p batchPull) rest(repo string, number int) map[string]any {
	state := "open"
	if p.state != "open" {
		state = "closed"
	}
	var mergedAt, mergeCommit any
	if p.state == "merged" {
		mergedAt = p.updatedAt.UTC().Format(time.RFC3339)
		mergeCommit = fakeSHA("m", number)
	}
	return map[string]any{
		"number": number, "title": p.title, "body": p.body, "state": state,
		"merged": p.state == "merged", "merged_at": mergedAt, "merge_commit_sha": mergeCommit,
		"html_url":   fmt.Sprintf("https://github.com/%s/pull/%d", repo, number),
		"updated_at": p.updatedAt.UTC().Format(time.RFC3339),
		"head":       map[string]any{"ref": p.branch, "sha": p.head, "repo": map[string]any{"full_name": p.headRepository(repo)}},
		"base":       map[string]any{"ref": "main"},
	}
}

// batchRefs is a ref of each number of repo, stored under the canonical name.
func batchRefs(repo string, numbers ...int) []PullRequestRef {
	owner, name, _ := strings.Cut(repo, "/")
	refs := make([]PullRequestRef, len(numbers))
	for i, number := range numbers {
		refs[i] = PullRequestRef{Owner: owner, Repo: name, Number: number, StoredUnder: repo}
	}
	return refs
}

// numbersFrom is count numbers from first on.
func numbersFrom(first, count int) []int {
	numbers := make([]int, count)
	for i := range numbers {
		numbers[i] = first + i
	}
	return numbers
}

// batchToken is the installation token of repo, which the fake mints for installation 1.
func batchToken(t *testing.T, ctx context.Context, client *githubapp.Client, repo string) string {
	t.Helper()
	owner, name, _ := strings.Cut(repo, "/")
	token, err := client.RepositoryToken(ctx, owner, name)
	if err != nil {
		t.Fatalf("the installation token of %s: %v", repo, err)
	}
	return token
}

// readBatchOrFail reads refs with readFactsBatch, and fails the test where it answers an error.
func readBatchOrFail(t *testing.T, ctx context.Context, pool *store.Pool, client *githubapp.Client, token string, refs []PullRequestRef) map[PullRequestRef]batchAnswer {
	t.Helper()
	answers, err := readFactsBatch(ctx, pool, client, nil, token, refs, time.Now())
	if err != nil {
		t.Fatalf("readFactsBatch of %d pull requests: %v", len(refs), err)
	}
	return answers
}

// describeBatchAnswer is what an answer holds, as a failure names it: the facts of its head row
// and texts, or the kind of answer that has none.
func describeBatchAnswer(answer batchAnswer, found bool) string {
	switch {
	case !found:
		return "no answer"
	case answer.Absent:
		return "absent"
	case answer.Head == nil:
		return fmt.Sprintf("stored %v, with no head row", answer.Stored)
	}
	head, texts := answer.Head, answer.Head.Texts
	line := fmt.Sprintf("%s#%d head=%s branch=%s base=%s state=%s title=%q url=%s merge=%s",
		head.Repo, head.Number, head.HeadSHA, head.HeadRef, head.BaseRef, head.State, head.Title, head.URL, head.MergeCommitSHA)
	if texts == nil {
		return line + " texts=none"
	}
	return line + fmt.Sprintf(" head_repo=%s body=%q messages=%q read=%v", texts.HeadRepo, texts.Body, texts.CommitMessages, texts.MessagesRead)
}

// wantBatchHead is describeBatchAnswer of the answer pull's facts give, read by a query of repo.
func wantBatchHead(repo string, number int, pull batchPull) string {
	merge, messages := "", pull.messages
	if pull.state == "merged" {
		merge = fakeSHA("m", number)
	}
	if pull.headRepo != "" {
		messages = []string{}
	}
	return fmt.Sprintf("%s#%d head=%s branch=%s base=main state=%s title=%q url=https://github.com/%s/pull/%d merge=%s head_repo=%s body=%q messages=%q read=true",
		repo, number, pull.head, pull.branch, pull.state, pull.title, repo, number, merge, pull.headRepository(repo), pull.body, messages)
}

// One GraphQL query reads the facts of twenty pull requests of three repositories of one
// installation (Decision E). Each answer holds the facts of its pull request and the head row
// they make, keyed by the canonical name. A merged one holds its merge commit, and a fork's holds
// its messages read and empty, as every writer stores them (storePullRequestFacts). Two
// repositories hold a #7, and each answer is its own. The query is sent once, with the token the
// caller gave, and reads no REST page.
func TestOneQueryReadsTheFactsOfTwentyPullRequests(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	fake := newFakeGitHub(t)
	github := serveBatchGitHub(t, fake)
	client := fake.newTestClient()
	token := batchToken(t, ctx, client, "acme/widgets")
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	numbers := map[string][]int{
		"acme/widgets": append([]int{7}, numbersFrom(101, 7)...),
		"acme/gadgets": append([]int{7}, numbersFrom(201, 5)...),
		"acme/tools":   numbersFrom(301, 6),
	}
	var refs []PullRequestRef
	for _, repo := range []string{"acme/widgets", "acme/gadgets", "acme/tools"} {
		for _, number := range numbers[repo] {
			pull := openPull(number, second)
			if repo == "acme/gadgets" && number == 7 {
				pull.head, pull.title = fakeSHA("g", 7), "Another change 7"
			}
			github.set(repo, number, pull)
		}
		refs = append(refs, batchRefs(repo, numbers[repo]...)...)
	}
	merged := openPull(101, second)
	merged.state = "merged"
	github.set("acme/widgets", 101, merged)
	closed := openPull(201, second)
	closed.state = "closed"
	github.set("acme/gadgets", 201, closed)
	fork := openPull(301, second)
	fork.headRepo = "someone/tools-fork"
	github.set("acme/tools", 301, fork)
	if len(refs) != 20 {
		t.Fatalf("the fixture holds %d refs, want 20", len(refs))
	}

	answers := readBatchOrFail(t, ctx, pool, client, token, refs)

	if len(answers) != 20 {
		t.Errorf("readFactsBatch answered %d pull requests, want 20", len(answers))
	}
	for _, ref := range refs {
		repo := ref.Owner + "/" + ref.Repo
		answer, found := answers[ref]
		want := wantBatchHead(repo, ref.Number, github.pulls[repo][ref.Number])
		if got := describeBatchAnswer(answer, found); got != want {
			t.Errorf("the answer of %s#%d:\n got %s\nwant %s", repo, ref.Number, got, want)
			continue
		}
		if answer.Stored || answer.Facts == nil || answer.Facts.Canonical != repo || answer.Facts.Token != token || answer.Facts.Payload.Number != ref.Number {
			t.Errorf("the facts of %s#%d = %+v, stored %v; want the canonical name, the query's token and the number, not stored", repo, ref.Number, answer.Facts, answer.Stored)
		}
		if head := answer.Head; head.GitHubReadOrder == nil || head.GitHubReadAt == nil || head.CheckedAt == nil || head.EventAt == nil || !head.EventAt.Equal(second) {
			t.Errorf("the head row of %s#%d carries place %v, read at %v, checked at %v, event at %v; want the query's place and time, the read's time and GitHub's updated_at %s",
				repo, ref.Number, head.GitHubReadOrder, head.GitHubReadAt, head.CheckedAt, head.EventAt, second)
		}
	}
	asked := github.asked()
	if len(asked) != 1 {
		t.Fatalf("readFactsBatch sent %d queries, want 1", len(asked))
	}
	if got := asked[0]; len(got.numbers) != 20 || got.repositories != 3 || got.bearer != "Bearer "+token {
		t.Errorf("the query asked %d pull requests of %d repositories with %q, want 20 of 3 with the caller's token", len(got.numbers), got.repositories, got.bearer)
	}
	for _, field := range []string{"number", "title", "body", "url", "state", "merged", "mergedAt", "updatedAt", "headRefName", "headRefOid",
		"baseRefName", "headRepository { nameWithOwner }", "mergeCommit { oid }", "commits(first: 100)", "totalCount", "nodes { commit { message } }"} {
		if !strings.Contains(asked[0].text, field) {
			t.Errorf("the query reads no %s:\n%s", field, asked[0].text)
		}
	}
	if reads, pages := github.restReads(), github.pagesAsked(); reads != 0 || len(pages) != 0 {
		t.Errorf("readFactsBatch read %d pull requests and %d pages of commits over REST, want none", reads, len(pages))
	}
}

// One pull request whose answer is above the client's 1 MiB alone is read by the refresh, the
// one REST read that fetches and stores a pull request (RefreshPullRequestHead). Its answer is
// stored, and the others are answers to write. A ref whose stored row is under the repository's
// old name gives the refresh that name, so the write keeps one row, under the canonical name
// (revision 8, BL1).
func TestAPullRequestStillTooLargeAloneIsReadByTheRefresh(t *testing.T) {
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	t.Run("one_number_of_twenty", func(t *testing.T) {
		fastPageRetries(t)
		pool, ctx := deliveryTestPool(t)
		fake := newFakeGitHub(t)
		github := serveBatchGitHub(t, fake)
		const repo, large = "acme/widgets", 613
		numbers := numbersFrom(601, 20)
		for _, number := range numbers {
			github.set(repo, number, openPull(number, second))
		}
		github.oversize = func(asked []int) bool { return slices.Contains(asked, large) }
		client := fake.newTestClient()
		refs := batchRefs(repo, numbers...)

		answers := readBatchOrFail(t, ctx, pool, client, batchToken(t, ctx, client, repo), refs)

		if len(answers) != 20 {
			t.Errorf("readFactsBatch answered %d pull requests, want 20", len(answers))
		}
		for _, ref := range refs {
			answer := answers[ref]
			switch {
			case ref.Number == large && (!answer.Stored || answer.Facts == nil || answer.Facts.Payload.Number != large || answer.Head != nil):
				t.Errorf("the answer of #%d = %+v, want the facts the refresh stored, with no head row to write", large, answer)
			case ref.Number != large && (answer.Stored || answer.Head == nil):
				t.Errorf("the answer of #%d = %+v, want a head row to write", ref.Number, answer)
			}
		}
		if got := github.restReadsOf(repo, large); got != 1 {
			t.Errorf("GET /repos/%s/pulls/%d was asked %d times, want once", repo, large, got)
		}
		if got := github.restReads(); got != 1 {
			t.Errorf("readFactsBatch read %d pull requests over REST, want only #%d", got, large)
		}
		if head := readHead(t, ctx, pool, repo, large); head.HeadSHA != fakeSHA("h", large) {
			t.Errorf("the stored head of #%d = %s, want GitHub's %s", large, head.HeadSHA, fakeSHA("h", large))
		}
		alone := false
		for _, query := range github.asked() {
			alone = alone || slices.Equal(query.numbers, []int{large})
		}
		if !alone {
			t.Errorf("no query asked #%d alone before the refresh: %+v", large, github.asked())
		}
	})
	t.Run("a_row_under_the_old_name", func(t *testing.T) {
		fastPageRetries(t)
		pool, ctx := deliveryTestPool(t)
		const old, canonical, number = "acme/old", "acme/new", 3
		seedProjectRepository(t, ctx, pool, "ATTR", "ATTR", canonical)
		mapRepository(t, ctx, pool, old, "ATTR", false, new(canonical))
		storeStatementRow(t, ctx, pool, statementRow{repo: old, number: number, branch: "work/none", title: "Change 3"})
		fake := newFakeGitHub(t)
		github := serveBatchGitHub(t, fake)
		github.set(canonical, number, openPull(number, second))
		github.oversize = func([]int) bool { return true }
		client := fake.newTestClient()
		ref := PullRequestRef{Owner: "acme", Repo: "new", Number: number, StoredUnder: old}

		answers := readBatchOrFail(t, ctx, pool, client, batchToken(t, ctx, client, canonical), []PullRequestRef{ref})

		if answer := answers[ref]; !answer.Stored || answer.Facts == nil || answer.Facts.Canonical != canonical {
			t.Errorf("the answer of %s#%d stored under %s = %+v, want the facts the refresh stored under %s", canonical, number, old, answer, canonical)
		}
		if got, under := github.restReadsOf(canonical, number), github.restReadsOf(old, number); got != 1 || under != 0 {
			t.Errorf("GET pulls/%d was asked %d times under %s and %d times under %s, want once under the canonical name", number, got, canonical, under, old)
		}
		for repo, want := range map[string]int{canonical: 1, old: 0} {
			var rows int
			if err := pool.QueryRow(ctx, `select count(*) from pull_request_heads where repo = $1 and number = $2`, repo, number).Scan(&rows); err != nil || rows != want {
				t.Errorf("results rows of %s#%d after the read = %d (%v), want %d", repo, number, rows, err, want)
			}
		}
	})
}

// An answer above the client's 1 MiB splits the batch into two halves, and each half is asked
// again: GitHub answers 2 MiB for twenty numbers and less for ten.
func TestAnAnswerAboveTheLimitSplitsTheBatch(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	fake := newFakeGitHub(t)
	github := serveBatchGitHub(t, fake)
	const repo = "acme/widgets"
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	numbers := numbersFrom(401, 20)
	for _, number := range numbers {
		github.set(repo, number, openPull(number, second))
	}
	github.oversize = func(asked []int) bool { return len(asked) > 10 }
	client := fake.newTestClient()
	refs := batchRefs(repo, numbers...)

	answers := readBatchOrFail(t, ctx, pool, client, batchToken(t, ctx, client, repo), refs)

	asked := github.asked()
	sizes := make([]int, len(asked))
	var halves []int
	for i, query := range asked {
		sizes[i] = len(query.numbers)
		if i > 0 {
			halves = append(halves, query.numbers...)
		}
	}
	slices.Sort(halves)
	if !slices.Equal(sizes, []int{20, 10, 10}) || !slices.Equal(halves, numbers) {
		t.Errorf("the queries asked %v pull requests, the halves %v; want 20, then two requests of 10 that hold each number once", sizes, halves)
	}
	for _, ref := range refs {
		if answer := answers[ref]; answer.Stored || answer.Head == nil || answer.Head.HeadSHA != fakeSHA("h", ref.Number) {
			t.Errorf("the answer of #%d = %s, want its head row from a half's query", ref.Number, describeBatchAnswer(answer, true))
		}
	}
	if got := github.restReads(); got != 0 {
		t.Errorf("readFactsBatch read %d pull requests over REST, want none", got)
	}
}

// Each batched query takes its own place in the order of every read of GitHub, just before it is
// sent (review 17, note 1). The ten rows of the first half carry one place, and the ten of the
// second half a higher one, each above the value of the sequence before the call, and each row
// carries the time of its read. A readFactsBatch that takes no place writes github_read_order
// null, and the trigger pull_request_heads_older_read could then order its answer with no other
// read.
func TestEachBatchedQueryTakesItsOwnPlace(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	fake := newFakeGitHub(t)
	github := serveBatchGitHub(t, fake)
	const repo = "acme/widgets"
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	numbers := numbersFrom(501, 20)
	for _, number := range numbers {
		github.set(repo, number, openPull(number, second))
	}
	github.oversize = func(asked []int) bool { return len(asked) > 10 }
	client := fake.newTestClient()
	refs := batchRefs(repo, numbers...)
	var before int64
	if err := pool.QueryRow(ctx, `select nextval('pull_request_reads_owed_revision_seq')`).Scan(&before); err != nil {
		t.Fatalf("read the sequence of the places: %v", err)
	}

	answers := readBatchOrFail(t, ctx, pool, client, batchToken(t, ctx, client, repo), refs)
	for _, ref := range refs {
		answer := answers[ref]
		if answer.Head == nil {
			t.Fatalf("the answer of #%d holds no head row to write: %+v", ref.Number, answer)
		}
		if _, err := upsertHeadUnderCanonicalName(ctx, pool, *answer.Head, ref.StoredUnder); err != nil {
			t.Fatalf("write the answer of #%d: %v", ref.Number, err)
		}
	}

	places := map[int]*int64{}
	rows, err := pool.Query(ctx, `
		select number, github_read_order, github_read_at is not null from pull_request_heads
		where repo = $1 and number between $2 and $3`, repo, numbers[0], numbers[len(numbers)-1])
	if err != nil {
		t.Fatalf("read the places of the written rows: %v", err)
	}
	for rows.Next() {
		var number int
		var place *int64
		var readAt bool
		if err := rows.Scan(&number, &place, &readAt); err != nil {
			t.Fatalf("scan a place: %v", err)
		}
		places[number] = place
		if !readAt {
			t.Errorf("github_read_at of #%d is null, want the time its query was sent", number)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the places of the written rows: %v", err)
	}
	asked := github.asked()
	if len(asked) != 3 {
		t.Fatalf("readFactsBatch sent %d queries, want 3: the twenty, and two halves of ten", len(asked))
	}
	half := func(query batchQuery) int64 {
		t.Helper()
		var shared *int64
		for _, number := range query.numbers {
			place := places[number]
			switch {
			case place == nil:
				t.Fatalf("github_read_order of #%d is null: its query took no place", number)
			case *place <= before:
				t.Fatalf("github_read_order of #%d = %d, want above %d, the value of the sequence before the call", number, *place, before)
			case shared != nil && *place != *shared:
				t.Fatalf("the rows of one query carry the places %d and %d, want one", *shared, *place)
			}
			shared = place
		}
		return *shared
	}
	if firstPlace, secondPlace := half(asked[1]), half(asked[2]); secondPlace <= firstPlace {
		t.Errorf("the second half's rows carry place %d, the first half's %d; want a higher place for the later query", secondPlace, firstPlace)
	}
}

// A pull request of more than 100 commits pages the rest of its messages over REST, from the
// second page, with the query's token, and its head row keeps the place of its query. The
// control: where GitHub answers 500 to the rest, the facts are stored with the messages not read
// (T5's rule), and the other pull request of the query keeps its messages.
func TestAPullRequestOfMoreThanAHundredCommitsReadsTheRest(t *testing.T) {
	for _, failing := range []bool{false, true} {
		name := map[bool]string{false: "the_rest_answers", true: "control_the_rest_answers_500"}[failing]
		t.Run(name, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			fake := newFakeGitHub(t)
			github := serveBatchGitHub(t, fake)
			const repo, long, short = "acme/widgets", 701, 702
			second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			pull := openPull(long, second)
			pull.messages = make([]string, 250)
			for i := range pull.messages {
				pull.messages[i] = fmt.Sprintf("Commit %d of the long change.", i+1)
			}
			github.set(repo, long, pull)
			github.set(repo, short, openPull(short, second))
			github.failCommits = failing
			client := fake.newTestClient()
			token := batchToken(t, ctx, client, repo)
			refs := batchRefs(repo, long, short)

			answers := readBatchOrFail(t, ctx, pool, client, token, refs)

			longAnswer, shortAnswer := answers[refs[0]], answers[refs[1]]
			if longAnswer.Head == nil || shortAnswer.Head == nil || longAnswer.Facts == nil {
				t.Fatalf("the answers = %s and %s, want two head rows", describeBatchAnswer(longAnswer, true), describeBatchAnswer(shortAnswer, true))
			}
			texts := longAnswer.Head.Texts
			wantPages := []int{2, 3}
			switch {
			case failing:
				wantPages = []int{2}
				if texts.MessagesRead || texts.CommitMessages != nil || longAnswer.Facts.Texts.MessagesRead || longAnswer.Head.HeadSHA != pull.head {
					t.Errorf("#%d after a failed read of the rest = %s, facts read %v; want its head with the messages not read", long, describeBatchAnswer(longAnswer, true), longAnswer.Facts.Texts.MessagesRead)
				}
			case !texts.MessagesRead || !slices.Equal(texts.CommitMessages, pull.messages):
				t.Errorf("#%d holds %d messages, read %v; want the 250 of GitHub, in order", long, len(texts.CommitMessages), texts.MessagesRead)
			}
			if got := shortAnswer.Head.Texts; !got.MessagesRead || len(got.CommitMessages) != 1 {
				t.Errorf("#%d holds the messages %q, read %v; want its one message from the query", short, got.CommitMessages, got.MessagesRead)
			}
			if pages := github.pagesAsked(); !slices.Equal(pages, wantPages) {
				t.Errorf("the pages of commits asked = %v, want %v: the query answers the first 100", pages, wantPages)
			}
			for _, bearer := range github.bearersOfPages() {
				if bearer != "Bearer "+token {
					t.Errorf("a page of commits was asked with %q, want the query's token", bearer)
				}
			}
			if l, s := longAnswer.Head.GitHubReadOrder, shortAnswer.Head.GitHubReadOrder; l == nil || s == nil || *l != *s {
				t.Errorf("the head rows of #%d and #%d carry the places %v and %v, want the one place of their query", long, short, l, s)
			}
		})
	}
}

// A pull request alias that GitHub answers null, with NOT_FOUND on the alias's own path, in a
// repository that resolved, is absent: its read goes to pullRequestAbsent (T7). The control in
// the same test: a repository that answers null is not absent, whatever its pull requests, since
// GitHub answers so alike for a repository the token does not read and for one that does not
// exist.
func TestAnAbsentAliasIsAbsent(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	fake := newFakeGitHub(t)
	github := serveBatchGitHub(t, fake)
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	github.set("acme/widgets", 8, openPull(8, second))
	github.gone["acme/gone"] = true
	client := fake.newTestClient()
	refs := append(batchRefs("acme/widgets", 7, 8), batchRefs("acme/gone", 5)...)

	answers := readBatchOrFail(t, ctx, pool, client, batchToken(t, ctx, client, "acme/widgets"), refs)

	if answer, found := answers[refs[0]]; !found || !answer.Absent || answer.Facts != nil || answer.Head != nil || answer.Stored {
		t.Errorf("the answer of acme/widgets#7 = %s, want absent: GitHub answered its alias null with NOT_FOUND on its own path", describeBatchAnswer(answer, found))
	}
	if answer, found := answers[refs[1]]; answer.Head == nil || answer.Head.HeadSHA != fakeSHA("h", 8) {
		t.Errorf("the answer of acme/widgets#8 = %s, want its head row", describeBatchAnswer(answer, found))
	}
	t.Run("control_a_null_repository", func(t *testing.T) {
		if answer, found := answers[refs[2]]; answer.Absent || answer.Facts != nil || answer.Head != nil || answer.Stored {
			t.Errorf("the answer of acme/gone#5 = %s, want no answer: its repository answered null", describeBatchAnswer(answer, found))
		}
	})
}

// The query can answer fewer than 100 messages of a pull request of more than 100 commits. Then
// the pages of the query and of REST do not line up. So the read takes every page from the first
// and keeps no message of the query: it holds GitHub's 250 messages, each once, in order. The
// control in the same test: a query that answers the first 100 messages reads the rest from the
// second page. A read that always starts at the second page fails the first case. A read that
// always starts at the first page fails the control (factsRead.answer).
func TestAQueryOfFewerThanAHundredMessagesReadsEveryPageFromTheFirst(t *testing.T) {
	for _, kase := range []struct {
		name          string
		queryMessages int
		pages         []int
	}{
		{name: "the_query_answers_40_messages", queryMessages: 40, pages: []int{1, 2, 3}},
		{name: "control_the_query_answers_the_first_100", pages: []int{2, 3}},
	} {
		t.Run(kase.name, func(t *testing.T) {
			pool, ctx := deliveryTestPool(t)
			fake := newFakeGitHub(t)
			github := serveBatchGitHub(t, fake)
			const repo, number = "acme/widgets", 801
			pull := openPull(number, time.Now().UTC().Add(-time.Hour).Truncate(time.Second))
			pull.messages = make([]string, 250)
			for i := range pull.messages {
				pull.messages[i] = fmt.Sprintf("Commit %d of the long change.", i+1)
			}
			pull.queryMessages = kase.queryMessages
			github.set(repo, number, pull)
			client := fake.newTestClient()
			refs := batchRefs(repo, number)

			answers := readBatchOrFail(t, ctx, pool, client, batchToken(t, ctx, client, repo), refs)

			answer := answers[refs[0]]
			if answer.Head == nil || answer.Head.Texts == nil {
				t.Fatalf("the answer of #%d = %s, want its head row with its texts", number, describeBatchAnswer(answer, true))
			}
			if texts := answer.Head.Texts; !texts.MessagesRead || !slices.Equal(texts.CommitMessages, pull.messages) {
				t.Errorf("#%d holds %d messages, read %v; want the 250 of GitHub, each once, in order", number, len(texts.CommitMessages), texts.MessagesRead)
			}
			if pages := github.pagesAsked(); !slices.Equal(pages, kase.pages) {
				t.Errorf("the pages of commits asked = %v, want %v", pages, kase.pages)
			}
		})
	}
}

// A pull request whose answer alone is above the client's 1 MiB goes to the refresh
// (RefreshPullRequestHead). GitHub can refuse that REST read with a 404. Then the ref is out of
// the answers, as for a null alias: no answer, no error and no stored row. The other pull request
// of the batch keeps its answer. The control in the same test: where the REST read answers, the
// ref's answer is the stored facts. A read that gives a refused refresh an answer, or that ends
// with an error, fails the first case (factsRead.refresh).
func TestARefusedRefreshLeavesAPullRequestTooLargeAloneOutOfTheAnswers(t *testing.T) {
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for _, held := range []bool{false, true} {
		name := map[bool]string{false: "the_refresh_answers_404", true: "control_the_refresh_answers"}[held]
		t.Run(name, func(t *testing.T) {
			pool, ctx := deliveryTestPool(t)
			fake := newFakeGitHub(t)
			github := serveBatchGitHub(t, fake)
			const repo, large, small = "acme/widgets", 901, 902
			if held {
				github.set(repo, large, openPull(large, second))
			}
			github.set(repo, small, openPull(small, second))
			github.oversize = func(asked []int) bool { return slices.Contains(asked, large) }
			client := fake.newTestClient()
			refs := batchRefs(repo, large, small)

			answers := readBatchOrFail(t, ctx, pool, client, batchToken(t, ctx, client, repo), refs)

			answer, found := answers[refs[0]]
			switch {
			case held && (!found || !answer.Stored || answer.Facts == nil || answer.Head != nil):
				t.Errorf("the answer of #%d = %s, want the facts that the refresh stored", large, describeBatchAnswer(answer, found))
			case !held && found:
				t.Errorf("the answer of #%d = %s, want no answer: GitHub refused the refresh with a 404", large, describeBatchAnswer(answer, found))
			}
			if answer := answers[refs[1]]; answer.Head == nil || answer.Head.HeadSHA != fakeSHA("h", small) {
				t.Errorf("the answer of #%d = %s, want its head row", small, describeBatchAnswer(answer, true))
			}
			if got := github.restReadsOf(repo, large); got != 1 {
				t.Errorf("GET /repos/%s/pulls/%d was asked %d times, want once: the refresh", repo, large, got)
			}
			want := map[bool]int{false: 0, true: 1}[held]
			var rows int
			if err := pool.QueryRow(ctx, `select count(*) from pull_request_heads where repo = $1 and number = $2`, repo, large).Scan(&rows); err != nil || rows != want {
				t.Errorf("results rows of #%d after the read = %d (%v), want %d", large, rows, err, want)
			}
		})
	}
}

// An alias whose answer does not decode ends the read with an error that names it. The answers
// before it come back with the error. These are the ten of the first half's request, and the
// aliases of the second half's request before it. No alias after it has an answer, and no request
// follows. The read takes the aliases of a request in their order (factsRead.decode). The control
// in the same test: the same twenty, with every alias decodable, answer twenty and no error. A
// read that drops the answers it had, or that goes on past the alias, fails the first case
// (factsRead.query).
func TestAnAliasThatDoesNotDecodeEndsTheRead(t *testing.T) {
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for _, garbled := range []bool{true, false} {
		name := map[bool]string{true: "alias_515_does_not_decode", false: "control_every_alias_decodes"}[garbled]
		t.Run(name, func(t *testing.T) {
			pool, ctx := deliveryTestPool(t)
			fake := newFakeGitHub(t)
			github := serveBatchGitHub(t, fake)
			const repo, bad = "acme/widgets", 515
			numbers := numbersFrom(501, 20)
			for _, number := range numbers {
				pull := openPull(number, second)
				pull.undecodable = garbled && number == bad
				github.set(repo, number, pull)
			}
			github.oversize = func(asked []int) bool { return len(asked) > 10 }
			client := fake.newTestClient()
			refs := batchRefs(repo, numbers...)

			answers, err := readFactsBatch(ctx, pool, client, nil, batchToken(t, ctx, client, repo), refs, time.Now())

			if asked := len(github.asked()); asked != 3 {
				t.Errorf("readFactsBatch sent %d queries, want 3: the twenty, and two halves of ten", asked)
			}
			if !garbled {
				if err != nil || len(answers) != len(refs) {
					t.Errorf("readFactsBatch answered %d of %d pull requests and the error %v, want each and no error", len(answers), len(refs), err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%s#%d", repo, bad)) {
				t.Errorf("readFactsBatch answered the error %v, want one that names %s#%d, whose answer does not decode", err, repo, bad)
			}
			for _, ref := range refs {
				answer, found := answers[ref]
				if want := ref.Number < bad; found != want || found && answer.Head == nil {
					t.Errorf("the answer of #%d = %s, want an answer with a head row %v: only an alias before #%d has one", ref.Number, describeBatchAnswer(answer, found), want, bad)
				}
			}
		})
	}
}
