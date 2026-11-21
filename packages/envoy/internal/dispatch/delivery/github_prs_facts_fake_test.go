package delivery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// fakeFactsFragment is the fragment that the batched query of facts names (factsFields). A POST
// /graphql whose body holds it is that query.
const fakeFactsFragment = "fragment pullRequestFacts"

// fakeFactsQueryHeader marks each REST request that the fake sends through its mux to answer a
// batched query of facts (factsThroughMux).
const fakeFactsQueryHeader = "X-Fake-Facts-Query"

// restRead reports whether r is a REST read that the code under test sent. It is false for a
// request that the fake sent through its mux to answer a batched query of facts. A test handler
// that counts the REST reads of a pull request counts r only where restRead(r) is true.
func restRead(r *http.Request) bool {
	return r.Header.Get(fakeFactsQueryHeader) == ""
}

// serveHTTP is the handler of the fake's server. It answers each batched query of facts before
// the mux sees it, and counts it in factsQueries. The test's facts handler answers it where the
// test set one, and else answerFactsQuery does. Every other request goes to the mux with its
// body.
func (f *fakeGitHub) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
		f.mux.ServeHTTP(w, r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("read a GraphQL request: %v", err)
		http.Error(w, `{"message":"Problems parsing JSON"}`, http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if !bytes.Contains(body, []byte(fakeFactsFragment)) {
		f.mux.ServeHTTP(w, r)
		return
	}
	f.factsQueries.Add(1)
	if f.facts != nil {
		f.facts(w, r)
		return
	}
	f.answerFactsQuery(w, r, body)
}

// fakeFactsOutcome is how the fake answers one pull request of a batched query of facts.
type fakeFactsOutcome int

const (
	// fakeFactsAnswered answers the pull request's node.
	fakeFactsAnswered fakeFactsOutcome = iota
	// fakeFactsNotFound answers null, with NOT_FOUND on the pull request's own alias path.
	fakeFactsNotFound
	// fakeFactsRefused answers null with no error: GitHub gave nothing for the pull request.
	fakeFactsRefused
	// fakeFactsLimited answers the whole query with GitHub's GraphQL rate limit.
	fakeFactsLimited
	// fakeFactsFailed answers the whole query with HTTP 502.
	fakeFactsFailed
)

// answerFactsQuery answers a batched query of facts from the routes that the test serves, in
// GitHub's shapes (factsFields, readFactsBatch). For a repository that serveGitHubPulls serves,
// the githubPulls state gives the answer, and factsAnswer counts each pull request in factsReads:
//   - limited answers the whole query with GitHub's GraphQL rate limit, and failing answers it
//     with HTTP 502.
//   - hidden, and a repository in notInstalled, answer a null repository with NOT_FOUND on the
//     path of its alias.
//   - a number that the repository does not hold answers null with NOT_FOUND on its own alias
//     path.
//   - commitsFailing answers one commit and no message, so the REST read of the messages after
//     the query fails as every read of the commits does.
//
// The test's own routes answer each pull request of any other repository through the mux
// (factsThroughMux). A repository in notInstalled answers a null repository there too.
func (f *fakeGitHub) answerFactsQuery(w http.ResponseWriter, r *http.Request, body []byte) {
	var request struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		f.t.Errorf("decode a facts query: %v", err)
		http.Error(w, `{"message":"Problems parsing JSON"}`, http.StatusBadRequest)
		return
	}
	data := map[string]any{}
	var refusals []any
	var current map[string]any
	var repoAlias, repo string
	var served *githubPulls
	for _, alias := range batchAlias.FindAllStringSubmatch(request.Query, -1) {
		if alias[1] != "" {
			owner, _ := request.Variables[alias[2]].(string)
			name, _ := request.Variables[alias[3]].(string)
			repoAlias, repo, served, current = alias[1], owner+"/"+name, nil, map[string]any{}
			_, unread := f.notInstalled.Load(repo)
			if held, ok := f.served.Load(repo); ok {
				served = held.(*githubPulls)
				served.mu.Lock()
				limited, failing, hidden := served.limited, served.failing, served.hidden
				served.mu.Unlock()
				switch {
				case limited:
					answerGraphQLRateLimit(w)
					return
				case failing:
					answerGraphQLFailure(w)
					return
				}
				unread = unread || hidden
			}
			if unread {
				data[repoAlias], current = nil, nil
				refusals = append(refusals, map[string]any{"type": "NOT_FOUND", "path": []any{repoAlias},
					"message": fmt.Sprintf("Could not resolve to a Repository with the name '%s'.", repo)})
				continue
			}
			data[repoAlias] = current
			continue
		}
		if current == nil {
			continue
		}
		number, _ := strconv.Atoi(alias[5])
		var node map[string]any
		var outcome fakeFactsOutcome
		if served != nil {
			node, outcome = served.factsAnswer(f.t, number)
		} else {
			node, outcome = f.factsThroughMux(r, repo, number)
		}
		switch outcome {
		case fakeFactsLimited:
			answerGraphQLRateLimit(w)
			return
		case fakeFactsFailed:
			answerGraphQLFailure(w)
			return
		case fakeFactsNotFound:
			current[alias[4]] = nil
			refusals = append(refusals, map[string]any{"type": "NOT_FOUND", "path": []any{repoAlias, alias[4]},
				"message": fmt.Sprintf("Could not resolve to a PullRequest with the number of %d.", number)})
		case fakeFactsRefused:
			current[alias[4]] = nil
		default:
			current[alias[4]] = node
		}
	}
	answer := map[string]any{"data": data}
	if len(refusals) > 0 {
		answer["errors"] = refusals
	}
	mustEncode(f.t, w, answer)
}

// factsAnswer is what the batched query of facts answers about pull request number of the
// repository: its node, or fakeFactsNotFound where GitHub does not hold it. It counts the pull
// request in factsReads.
func (g *githubPulls) factsAnswer(t *testing.T, number int) (map[string]any, fakeFactsOutcome) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.factsReads.Add(1)
	pull, known := g.pulls[number]
	if !known {
		return nil, fakeFactsNotFound
	}
	node, err := fakeFactsNode(number, pull, g.messages[number], g.commitsFailing)
	if err != nil {
		t.Errorf("the facts of pull request %d: %v", number, err)
		return nil, fakeFactsFailed
	}
	return node, fakeFactsAnswered
}

// factsThroughMux answers one pull request of a batched query of facts about a repository that
// no serveGitHubPulls serves. It asks the mux for GET /repos/{owner}/{repo}/pulls/{number} and
// for the first page of its commits. Each request carries the header X-Fake-Facts-Query, and the
// test's own routes answer it. A 404 or a 410 of the pull request is GitHub's NOT_FOUND. A 429, or
// a 403 with no requests remaining, is the GraphQL rate limit of the whole query. A status of 500
// or more fails the whole query, and any other status is no answer. Where the page of commits does
// not answer 200, the pull request has one commit and no message, as commitsFailing gives. The
// node holds the messages of that one page, and counts no more commits than those.
func (f *fakeGitHub) factsThroughMux(r *http.Request, repo string, number int) (map[string]any, fakeFactsOutcome) {
	read := func(path string) *httptest.ResponseRecorder {
		inner := httptest.NewRequestWithContext(r.Context(), http.MethodGet, path, nil)
		inner.Header.Set("Authorization", r.Header.Get("Authorization"))
		inner.Header.Set(fakeFactsQueryHeader, "1")
		answer := httptest.NewRecorder()
		f.mux.ServeHTTP(answer, inner)
		return answer
	}
	pullPath := fmt.Sprintf("/repos/%s/pulls/%d", repo, number)
	answer := read(pullPath)
	switch status := answer.Code; {
	case status == http.StatusNotFound || status == http.StatusGone:
		return nil, fakeFactsNotFound
	case status == http.StatusTooManyRequests || status == http.StatusForbidden && answer.Header().Get("X-RateLimit-Remaining") == "0":
		return nil, fakeFactsLimited
	case status >= http.StatusInternalServerError:
		return nil, fakeFactsFailed
	case status != http.StatusOK:
		return nil, fakeFactsRefused
	}
	var pull map[string]any
	if err := json.Unmarshal(answer.Body.Bytes(), &pull); err != nil {
		f.t.Errorf("decode the REST answer of %s#%d for a facts query: %v", repo, number, err)
		return nil, fakeFactsFailed
	}
	commits := read(pullPath + "/commits?per_page=100&page=1")
	var page []commitMessagePayload
	failing := commits.Code != http.StatusOK || json.Unmarshal(commits.Body.Bytes(), &page) != nil
	messages := make([]string, 0, len(page))
	for _, commit := range page {
		messages = append(messages, commit.Commit.Message)
	}
	node, err := fakeFactsNode(number, pull, messages, failing)
	if err != nil {
		f.t.Errorf("the facts of %s#%d: %v", repo, number, err)
		return nil, fakeFactsFailed
	}
	return node, fakeFactsAnswered
}

// fakeFactsNode is pull number, as GitHub's REST API answers it, in the shape of the batched query
// of facts (factsFields). The node holds its first 100 messages and the count of all of them.
// Where commitsFailing is set, the node holds one commit and no message, so the read of the
// messages over REST that follows fails as the commits do.
func fakeFactsNode(number int, pull map[string]any, messages []string, commitsFailing bool) (map[string]any, error) {
	encoded, err := json.Marshal(pull)
	if err != nil {
		return nil, err
	}
	var payload pullRequestPayload
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return nil, err
	}
	state := "OPEN"
	switch {
	case payload.State == "open":
	case payload.Merged || payload.MergedAt != nil:
		state = "MERGED"
	default:
		state = "CLOSED"
	}
	var mergedAt, updatedAt, headRepository, mergeCommit any
	if payload.MergedAt != nil {
		mergedAt = payload.MergedAt.UTC().Format(time.RFC3339Nano)
	}
	if payload.UpdatedAt != nil {
		updatedAt = payload.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	if payload.Head.Repo != nil {
		headRepository = map[string]any{"nameWithOwner": payload.Head.Repo.FullName}
	}
	if payload.MergeCommitSHA != nil {
		mergeCommit = map[string]any{"oid": *payload.MergeCommitSHA}
	}
	commits := map[string]any{"totalCount": 1, "nodes": []any{}}
	if !commitsFailing {
		first := messages[:min(factsPage, len(messages))]
		nodes := make([]any, len(first))
		for i, message := range first {
			nodes[i] = map[string]any{"commit": map[string]any{"message": message}}
		}
		commits = map[string]any{"totalCount": len(messages), "nodes": nodes}
	}
	return map[string]any{
		"number": number, "title": payload.Title, "body": payload.Body, "url": payload.HTMLURL,
		"state": state, "merged": state == "MERGED", "mergedAt": mergedAt, "updatedAt": updatedAt,
		"headRefName": payload.Head.Ref, "headRefOid": payload.Head.SHA, "baseRefName": payload.Base.Ref,
		"headRepository": headRepository, "mergeCommit": mergeCommit, "commits": commits,
	}, nil
}

// answerGraphQLRateLimit answers a query as GitHub answers one past its installation's GraphQL
// budget (githubapp.GraphQLRateLimit): HTTP 200, no requests remaining, the time the budget
// resets, and an errors entry of type RATE_LIMITED beside a null data.
func answerGraphQLRateLimit(w http.ResponseWriter) {
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(90*time.Second).Unix(), 10))
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded for installation"}]}`)
}

// answerGraphQLFailure answers a query as GitHub answers one that its server could not serve:
// HTTP 502.
func answerGraphQLFailure(w http.ResponseWriter) {
	w.WriteHeader(http.StatusBadGateway)
	fmt.Fprint(w, `{"message":"Server Error"}`)
}
