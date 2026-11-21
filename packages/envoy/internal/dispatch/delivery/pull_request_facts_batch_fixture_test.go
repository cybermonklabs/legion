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
// answers the absence query (absenceQuery) on POST /graphql from the same pull requests, or with
// absenceFails where a test sets it. A repository can be on a second installation (install). The
// bearer token of each query names its installation (installationIDFromToken), and each query
// records it (queriesOf). GitHub's GraphQL rate limit answers each query of limitedInstallation.
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
	absenceFails        func(http.ResponseWriter)
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
// query of limitedInstallation, and absenceFails, where set, answers every query in place of
// GitHub's answer. It records each query (absenceQueries). Any other query is a test's mistake.
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
	fails := g.absenceFails
	gone := g.gone[repo]
	_, held := g.pulls[repo][int(number)]
	counted := map[string]any{"totalCount": len(g.pulls[repo])}
	g.mu.Unlock()
	switch {
	case fails != nil:
		fails(w)
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
