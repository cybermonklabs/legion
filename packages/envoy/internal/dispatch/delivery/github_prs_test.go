package delivery

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// fakeInstallation is one entry fakeGitHub's installations field lists, for a test that exercises
// more than the single default installation (ListInstallations/ListInstallationRepositoriesByID).
type fakeInstallation struct {
	id    int64
	repos []string
}

// fakeGitHub is a minimal httptest stand-in for the GitHub App and REST APIs this package's
// fetchers call: installation resolution, token minting, and whatever read handlers a test
// installs on mux. Shared by github_prs_test.go and github_runs_test.go. Defaults to a full
// permission set (contents/actions/pull_requests all "read") and a single-repository
// installation ("acme/widgets") so an ordinary test needs no boilerplate; a test exercising a
// missing permission or a multi-repository installation overrides permissions/installationRepos
// before calling newTestClient. The fake answers the batched query of facts (readFactsBatch)
// before its mux sees the request (serveHTTP).
type fakeGitHub struct {
	t                 *testing.T
	mux               *http.ServeMux
	installID         int64
	tokenMints        atomic.Int64
	permissions       map[string]string
	installationRepos []string
	// installations, when set, lists every installation GET /app/installations answers and backs
	// GET /installation/repositories' per-installation routing; nil means the single-installation
	// default (installID/installationRepos) everywhere, matching every test before this field
	// existed. Round 5's errgroup-concurrent cross-installation search mints several
	// installations' tokens at once, so tokenMints is atomic rather than a bare int.
	installations []fakeInstallation
	// tokenLifetime is how long a minted installation token lives, as expires_at tells the client;
	// zero is GitHub's hour. tokenExpiry records each minted token's expiry for tokenExpired.
	tokenLifetime time.Duration
	tokenExpiry   sync.Map
	// installationOf names the installation GET /repos/{owner}/{repo}/installation answers for a
	// repository other than installID's, set before newTestClient; notInstalled holds each
	// repository ("owner/repo") the App is not installed on, which that lookup answers 404 for
	// while it is held. lookups counts that lookup for each repository (lookupsOf).
	installationOf map[string]int64
	notInstalled   sync.Map
	lookups        sync.Map
	// served holds each repository serveGitHubPulls answers for, by "owner/repo", which POST
	// /graphql answers the absence query about (answerAbsenceQuery); the route is registered with
	// the first of them (absenceRoute).
	served       sync.Map
	absenceRoute sync.Once
	// facts, where a test sets it before newTestClient, answers each batched query of facts in
	// place of answerFactsQuery. factsQueries counts each such query, whoever answers it.
	facts        http.HandlerFunc
	factsQueries atomic.Int32
}

// lookupsOf is how many times GET /repos/{owner}/{repo}/installation was asked about repo.
func (f *fakeGitHub) lookupsOf(repo string) int32 {
	counter, _ := f.lookups.LoadOrStore(repo, &atomic.Int32{})
	return counter.(*atomic.Int32).Load()
}

// tokenExpired reports whether r's bearer is an installation token this fake minted whose expiry
// has passed: GitHub answers such a request 401 Bad credentials.
func (f *fakeGitHub) tokenExpired(r *http.Request) bool {
	expires, ok := f.tokenExpiry.Load(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	return ok && !time.Now().Before(expires.(time.Time))
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		t: t, mux: http.NewServeMux(), installID: 1,
		permissions:       map[string]string{"contents": "read", "actions": "read", "pull_requests": "read"},
		installationRepos: []string{"acme/widgets"},
	}
	f.mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("owner") + "/" + r.PathValue("repo")
		counter, _ := f.lookups.LoadOrStore(name, &atomic.Int32{})
		counter.(*atomic.Int32).Add(1)
		if _, missing := f.notInstalled.Load(name); missing {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		id := f.installID
		if other, ok := f.installationOf[name]; ok {
			id = other
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id":          id,
			"app_slug":    "delivery-test",
			"permissions": f.permissions,
		}); err != nil {
			f.t.Errorf("encode installation: %v", err)
		}
	})
	f.mux.HandleFunc("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		installations := f.installations
		if installations == nil {
			installations = []fakeInstallation{{id: f.installID, repos: f.installationRepos}}
		}
		payload := make([]map[string]any, len(installations))
		for i, inst := range installations {
			payload[i] = map[string]any{
				"id": inst.id, "app_slug": "delivery-test",
				"account":     map[string]any{"login": fmt.Sprintf("account-%d", inst.id)},
				"permissions": f.permissions,
			}
		}
		mustEncode(t, w, payload)
	})
	f.mux.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		repos := f.installationRepos
		if f.installations != nil {
			id := installationIDFromToken(r)
			for _, inst := range f.installations {
				if inst.id == id {
					repos = inst.repos
					break
				}
			}
		}
		payload := make([]map[string]any, len(repos))
		for i, name := range repos {
			payload[i] = map[string]any{"full_name": name}
		}
		mustEncode(t, w, map[string]any{"total_count": len(payload), "repositories": payload})
	})
	f.mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		mints := f.tokenMints.Add(1)
		lifetime := f.tokenLifetime
		if lifetime == 0 {
			lifetime = time.Hour
		}
		token, expires := fmt.Sprintf("ghs_fake_%s_%d", r.PathValue("id"), mints), time.Now().Add(lifetime)
		f.tokenExpiry.Store(token, expires)
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(map[string]any{"token": token, "expires_at": expires.Format(time.RFC3339Nano)}); err != nil {
			f.t.Errorf("encode token: %v", err)
		}
	})
	return f
}

// installationIDFromToken recovers the installation id POST /app/installations/{id}/access_tokens
// encoded into its own fake token ("ghs_fake_<id>_<mint counter>"), so GET
// /installation/repositories can answer the asking installation's own repos rather than always
// the default one.
func installationIDFromToken(r *http.Request) int64 {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(token, "_")
	if len(parts) < 3 {
		return 0
	}
	id, _ := strconv.ParseInt(parts[2], 10, 64)
	return id
}

func (f *fakeGitHub) handle(pattern string, handler http.HandlerFunc) {
	f.mux.HandleFunc(pattern, handler)
}

func (f *fakeGitHub) newTestClient(options ...githubapp.Option) *githubapp.Client {
	t := f.t
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	base := githubtest.Server(t, githubtest.Spec{Handle: f.serveHTTP})
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.testclient", PEM: pemText}, base, options...)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if client == nil {
		t.Fatal("new client: got nil for a configured app")
	}
	return client
}

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

func mustEncode(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func ptrTime(s string) *time.Time {
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &v
}

func TestFetchPullRequestMapsEveryField(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/42", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"number":           42,
			"title":            "Add retries",
			"html_url":         "https://github.com/acme/widgets/pull/42",
			"user":             map[string]any{"login": "octocat"},
			"labels":           []map[string]any{{"name": "bug"}, {"name": "needs-review"}},
			"body":             "Fixes #7",
			"created_at":       "2024-01-01T00:00:00Z",
			"merged_at":        "2024-01-02T00:00:00Z",
			"merge_commit_sha": "deadbeef",
			"additions":        12,
			"deletions":        3,
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/42/commits", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("per_page"); got != "1" {
			t.Errorf("commits request per_page = %q, want 1", got)
		}
		mustEncode(t, w, []map[string]any{
			{
				"sha": "c0ffee",
				"commit": map[string]any{
					"author":    map[string]any{"date": "2023-12-30T10:00:00Z"},
					"committer": map[string]any{"date": "2023-12-30T11:00:00Z"},
				},
			},
			{
				"sha": "later",
				"commit": map[string]any{
					"author": map[string]any{"date": "2023-12-31T10:00:00Z"},
				},
			},
		})
	})
	client := fake.newTestClient()

	pr, err := FetchPullRequest(t.Context(), client, "acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPullRequest: %v", err)
	}
	want := FetchedPullRequest{
		Number:         42,
		Title:          "Add retries",
		URL:            "https://github.com/acme/widgets/pull/42",
		Author:         "octocat",
		Labels:         []string{"bug", "needs-review"},
		CreatedAt:      *ptrTime("2024-01-01T00:00:00Z"),
		MergedAt:       ptrTime("2024-01-02T00:00:00Z"),
		MergeCommitSHA: new("deadbeef"),
		Additions:      new(12),
		Deletions:      new(3),
		Body:           "Fixes #7",
		FirstCommitAt:  ptrTime("2023-12-30T10:00:00Z"),
	}
	assertPullRequestsEqual(t, pr, want)
}

func TestFetchPullRequestUsesCommitterDateWhenAuthorIsAbsent(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"number": 7, "title": "x", "html_url": "u", "user": map[string]any{"login": "a"},
			"created_at": "2024-01-01T00:00:00Z", "merged_at": "2024-01-02T00:00:00Z",
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/7/commits", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, []map[string]any{
			{"sha": "nouser", "commit": map[string]any{"committer": map[string]any{"date": "2023-12-29T00:00:00Z"}}},
		})
	})
	client := fake.newTestClient()

	pr, err := FetchPullRequest(t.Context(), client, "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchPullRequest: %v", err)
	}
	if pr.FirstCommitAt == nil || !pr.FirstCommitAt.Equal(*ptrTime("2023-12-29T00:00:00Z")) {
		t.Fatalf("FirstCommitAt = %v, want the committer date", pr.FirstCommitAt)
	}
}

func TestFetchPullRequestNoCommitsIsAnError(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"number": 9, "title": "x", "html_url": "u", "user": map[string]any{"login": "a"}})
	})
	fake.handle("GET /repos/acme/widgets/pulls/9/commits", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, []map[string]any{})
	})
	client := fake.newTestClient()

	_, err := FetchPullRequest(t.Context(), client, "acme", "widgets", 9)
	if err == nil {
		t.Fatal("FetchPullRequest: err = nil, want a malformed-commits error")
	}
}

func TestFetchPullRequestUpstream404IsWrappedNotPanic(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/404", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	})
	client := fake.newTestClient()

	_, err := FetchPullRequest(t.Context(), client, "acme", "widgets", 404)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("FetchPullRequest: err = %v, want an error naming the 404 status", err)
	}
}

func TestFetchPullRequestUpstream500IsWrapped(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/500", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"boom"}`)
	})
	client := fake.newTestClient()

	_, err := FetchPullRequest(t.Context(), client, "acme", "widgets", 500)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("FetchPullRequest: err = %v, want an error naming the 500 status", err)
	}
}

// decodeGraphQLRequest reads a POST /graphql request's {query, variables} body, the shape every
// search test below asserts against instead of REST search's URL query parameters.
func decodeGraphQLRequest(t *testing.T, r *http.Request) (query string, variables map[string]any) {
	t.Helper()
	var decoded struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode GraphQL request body: %v", err)
	}
	return decoded.Query, decoded.Variables
}

func searchNodeJSON(number int, title, author string, createdAt, mergedAt string) map[string]any {
	return map[string]any{
		"number":    number,
		"title":     title,
		"url":       fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
		"author":    map[string]any{"login": author},
		"createdAt": createdAt,
		"mergedAt":  mergedAt,
		"body":      "",
		"labels":    map[string]any{"nodes": []any{}},
	}
}

// searchResponseJSON wraps nodes in the GraphQL `data.search` envelope fetchSearchPage decodes.
func searchResponseJSON(issueCount int, nodes []map[string]any, hasNextPage bool, endCursor string) map[string]any {
	return map[string]any{
		"data": map[string]any{
			"search": map[string]any{
				"issueCount": issueCount,
				"pageInfo":   map[string]any{"hasNextPage": hasNextPage, "endCursor": endCursor},
				"nodes":      nodes,
			},
		},
	}
}

func TestSearchMergedPullRequestsBuildsTheQuery(t *testing.T) {
	fake := newFakeGitHub(t)
	var gotQuery string
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		var variables map[string]any
		_, variables = decodeGraphQLRequest(t, r)
		gotQuery, _ = variables["q"].(string)
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	client := fake.newTestClient()

	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	if _, err := SearchMergedPullRequests(t.Context(), client, "acme", "widgets", []string{"alice", "bob"}, since, until); err != nil {
		t.Fatalf("SearchMergedPullRequests: %v", err)
	}

	want := "repo:acme/widgets is:pr is:merged merged:2024-01-01T00:00:00Z..2024-01-02T00:00:00Z author:alice author:bob"
	if gotQuery != want {
		t.Fatalf("search query = %q, want %q", gotQuery, want)
	}
}

func TestSearchMergedPullRequestsAcrossInstallationScopesToInstallationRepos(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.installationRepos = []string{"acme/widgets", "acme/other-widgets"}
	var gotQuery string
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		_, variables := decodeGraphQLRequest(t, r)
		gotQuery, _ = variables["q"].(string)
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	client := fake.newTestClient()

	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	// The fake's single default installation (fake.installationRepos) covers acme/widgets and
	// acme/other-widgets: the query itself must carry an explicit repo: qualifier for every
	// repository the installation's GET /installation/repositories lists, since an unqualified
	// query is NOT scoped by the authenticating token for public-repository content (it searches
	// all of public GitHub).
	if _, err := searchAllInstallations(t.Context(), client, []string{"alice", "bob"}, since, until); err != nil {
		t.Fatalf("SearchMergedPullRequestsAcrossInstallation: %v", err)
	}

	want := "repo:acme/widgets repo:acme/other-widgets is:pr is:merged merged:2024-01-01T00:00:00Z..2024-01-02T00:00:00Z author:alice author:bob"
	if gotQuery != want {
		t.Fatalf("search query = %q, want %q", gotQuery, want)
	}
}

// TestSearchMergedPullRequestsAcrossInstallationSearchesEveryInstallation proves S4: the search
// covers every installation the App has, not only the one tokenOwner/tokenRepo resolves to --
// LEGION-294's population rule names no installation boundary, and Rev measured roughly a quarter
// of the real population living outside the deploy repository's own installation.
func TestSearchMergedPullRequestsAcrossInstallationSearchesEveryInstallation(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.installations = []fakeInstallation{
		{id: 1, repos: []string{"acme/widgets"}},
		{id: 2, repos: []string{"other-org/other-repo"}},
	}
	var mu sync.Mutex
	var queriesSeen []string
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		_, variables := decodeGraphQLRequest(t, r)
		q, _ := variables["q"].(string)
		mu.Lock()
		queriesSeen = append(queriesSeen, q)
		mu.Unlock()
		if strings.Contains(q, "other-org/other-repo") {
			node := searchNodeJSON(99, "feat: from the second installation", "alice", "2024-01-01T00:00:00Z", "2024-01-01T01:00:00Z")
			mustEncode(t, w, searchResponseJSON(1, []map[string]any{node}, false, ""))
			return
		}
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	client := fake.newTestClient()

	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	results, err := searchAllInstallations(t.Context(), client, []string{"alice"}, since, until)
	if err != nil {
		t.Fatalf("SearchMergedPullRequestsAcrossInstallation: %v", err)
	}

	if len(queriesSeen) != 2 {
		t.Fatalf("queries made = %d, want 2 (one per installation), got %v", len(queriesSeen), queriesSeen)
	}
	if len(results) != 1 || results[0].Number != 99 {
		t.Fatalf("results = %+v, want exactly the PR found under the second installation", results)
	}
}

func TestSearchMergedPullRequestsPaginatesAcrossPages(t *testing.T) {
	fake := newFakeGitHub(t)
	var cursorsSeen []string
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		_, variables := decodeGraphQLRequest(t, r)
		after, _ := variables["after"].(string)
		cursorsSeen = append(cursorsSeen, after)
		switch after {
		case "":
			mustEncode(t, w, searchResponseJSON(150, repeatSearchNodes(100, 1), true, "cursor-100"))
		case "cursor-100":
			mustEncode(t, w, searchResponseJSON(150, repeatSearchNodes(50, 101), false, ""))
		default:
			t.Fatalf("unexpected cursor %q", after)
		}
	})
	client := fake.newTestClient()

	results, err := SearchMergedPullRequests(t.Context(), client, "acme", "widgets", []string{"alice"},
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("SearchMergedPullRequests: %v", err)
	}
	if len(results) != 150 {
		t.Fatalf("len(results) = %d, want 150", len(results))
	}
	if len(cursorsSeen) != 2 || cursorsSeen[0] != "" || cursorsSeen[1] != "cursor-100" {
		t.Fatalf("cursors fetched = %v, want [\"\" cursor-100]", cursorsSeen)
	}
	if results[0].Number != 1 || results[149].Number != 150 {
		t.Fatalf("results not in page order: first=%d last=%d", results[0].Number, results[149].Number)
	}
	if mints := fake.tokenMints.Load(); mints != 1 {
		t.Fatalf("minted %d installation tokens over two pages, want 1", mints)
	}
}

// TestFetchCommitMessagesAsksForATokenPerPage: a pull request's commit pages, up to
// maxCommitPages of them, each ask for a token, so a page answered after the previous page's
// token expired is asked for with a new one.
func TestFetchCommitMessagesAsksForATokenPerPage(t *testing.T) {
	fake := newFakeGitHub(t)
	refuse, expired := expiringTokens(t, fake)
	fake.handle("GET /repos/acme/widgets/pulls/7/commits", func(w http.ResponseWriter, r *http.Request) {
		if refuse(w, r) {
			return
		}
		count := 100
		if r.URL.Query().Get("page") == "1" {
			time.Sleep(400 * time.Millisecond)
		} else {
			count = 5
		}
		commits := make([]map[string]any, count)
		for i := range commits {
			commits[i] = map[string]any{"commit": map[string]any{"message": "feat: a step"}}
		}
		mustEncode(t, w, commits)
	})
	messages, err := fetchCommitMessages(t.Context(), fake.newTestClient(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("fetchCommitMessages across a token's expiry: %v", err)
	}
	if len(messages) != 105 || expired.Load() != 0 {
		t.Fatalf("read %d commit messages, want 105; %d requests carried an expired token, want none", len(messages), expired.Load())
	}
}

// TestSearchMergedPullRequestsAcrossInstallationAsksForATokenPerPage: the reconcile's merged-PR
// search (SearchMergedPullRequestsAcrossInstallation, through searchOneInstallation's
// client.Token) reconciles each window's pull requests between pages, so it asks for a token per
// page, and a page answered after the previous page's token expired is asked for with a new one.
func TestSearchMergedPullRequestsAcrossInstallationAsksForATokenPerPage(t *testing.T) {
	fake := newFakeGitHub(t)
	refuse, expired := expiringTokens(t, fake)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		if refuse(w, r) {
			return
		}
		_, variables := decodeGraphQLRequest(t, r)
		if after, _ := variables["after"].(string); after == "" {
			time.Sleep(400 * time.Millisecond)
			mustEncode(t, w, searchResponseJSON(150, repeatSearchNodes(100, 1), true, "cursor-100"))
			return
		}
		mustEncode(t, w, searchResponseJSON(150, repeatSearchNodes(50, 101), false, ""))
	})
	results, err := searchAllInstallations(t.Context(), fake.newTestClient(), []string{"alice"},
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("SearchMergedPullRequestsAcrossInstallation across a token's expiry: %v", err)
	}
	if len(results) != 150 || expired.Load() != 0 {
		t.Fatalf("found %d pull requests, want 150; %d requests carried an expired token, want none", len(results), expired.Load())
	}
}

func repeatSearchNodes(n, startNumber int) []map[string]any {
	nodes := make([]map[string]any, n)
	for i := range n {
		number := startNumber + i
		nodes[i] = searchNodeJSON(number, "t", "alice", "2024-01-01T00:00:00Z", "2024-01-01T01:00:00Z")
	}
	return nodes
}

func TestSearchMergedPullRequestsHalvesOnOverflowWithoutGapOrOverlap(t *testing.T) {
	fake := newFakeGitHub(t)
	type window struct{ since, until string }
	var windows []window
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		_, variables := decodeGraphQLRequest(t, r)
		q, _ := variables["q"].(string)
		// q contains "merged:<since>..<until>"; extract it for the window assertion.
		const marker = "merged:"
		idx := strings.Index(q, marker)
		if idx < 0 {
			t.Fatalf("query %q has no merged: qualifier", q)
		}
		rangeText := strings.Fields(q[idx+len(marker):])[0]
		parts := strings.SplitN(rangeText, "..", 2)
		if len(parts) != 2 {
			t.Fatalf("merged range %q is not a..b", rangeText)
		}
		windows = append(windows, window{parts[0], parts[1]})

		if rangeText == "2024-01-01T00:00:00Z..2024-01-02T00:00:00Z" {
			// The original, full window: answer with an overflowing issueCount so the caller
			// halves and recurses instead of paginating past 1,000.
			mustEncode(t, w, searchResponseJSON(1001, repeatSearchNodes(100, 1), false, ""))
			return
		}
		// Either half answers a small, final result.
		mustEncode(t, w, searchResponseJSON(1, repeatSearchNodes(1, 1), false, ""))
	})
	client := fake.newTestClient()

	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	results, err := SearchMergedPullRequests(t.Context(), client, "acme", "widgets", []string{"alice"}, since, until)
	if err != nil {
		t.Fatalf("SearchMergedPullRequests: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2 (one per half)", len(results))
	}
	if len(windows) != 3 {
		t.Fatalf("requests made = %d, want 3 (original + two halves)", len(windows))
	}
	// GitHub's merged:A..B qualifier is inclusive on BOTH ends, so the second half must start one
	// second after the midpoint (GitHub's own query granularity), not at the midpoint itself --
	// otherwise a PR merged exactly at the midpoint would be counted in both halves.
	mid := since.Add(until.Sub(since) / 2)
	first, second := windows[1], windows[2]
	if first.since != since.UTC().Format(time.RFC3339) || first.until != mid.UTC().Format(time.RFC3339) {
		t.Fatalf("first half = %+v, want [%s, %s]", first, since.UTC().Format(time.RFC3339), mid.UTC().Format(time.RFC3339))
	}
	wantSecondSince := mid.Add(time.Second).UTC().Format(time.RFC3339)
	if second.since != wantSecondSince || second.until != until.UTC().Format(time.RFC3339) {
		t.Fatalf("second half = %+v, want [%s, %s]", second, wantSecondSince, until.UTC().Format(time.RFC3339))
	}
}

func assertPullRequestsEqual(t *testing.T, got, want FetchedPullRequest) {
	t.Helper()
	if got.Number != want.Number || got.Title != want.Title || got.URL != want.URL || got.Author != want.Author ||
		got.Body != want.Body || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("got = %+v, want = %+v", got, want)
	}
	if (got.MergedAt == nil) != (want.MergedAt == nil) || (got.MergedAt != nil && !got.MergedAt.Equal(*want.MergedAt)) {
		t.Fatalf("MergedAt = %v, want %v", got.MergedAt, want.MergedAt)
	}
	if (got.MergeCommitSHA == nil) != (want.MergeCommitSHA == nil) || (got.MergeCommitSHA != nil && *got.MergeCommitSHA != *want.MergeCommitSHA) {
		t.Fatalf("MergeCommitSHA = %v, want %v", got.MergeCommitSHA, want.MergeCommitSHA)
	}
	if (got.Additions == nil) != (want.Additions == nil) || (got.Additions != nil && *got.Additions != *want.Additions) {
		t.Fatalf("Additions = %v, want %v", got.Additions, want.Additions)
	}
	if (got.Deletions == nil) != (want.Deletions == nil) || (got.Deletions != nil && *got.Deletions != *want.Deletions) {
		t.Fatalf("Deletions = %v, want %v", got.Deletions, want.Deletions)
	}
	if (got.FirstCommitAt == nil) != (want.FirstCommitAt == nil) || (got.FirstCommitAt != nil && !got.FirstCommitAt.Equal(*want.FirstCommitAt)) {
		t.Fatalf("FirstCommitAt = %v, want %v", got.FirstCommitAt, want.FirstCommitAt)
	}
	if len(got.Labels) != len(want.Labels) {
		t.Fatalf("Labels = %v, want %v", got.Labels, want.Labels)
	}
	for i := range got.Labels {
		if got.Labels[i] != want.Labels[i] {
			t.Fatalf("Labels = %v, want %v", got.Labels, want.Labels)
		}
	}
}

func TestSearchMergedPullRequestsLeavesCompletingFieldsNil(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(1, []map[string]any{searchNodeJSON(5, "t", "alice", "2024-01-01T00:00:00Z", "2024-01-01T01:00:00Z")}, false, ""))
	})
	client := fake.newTestClient()

	results, err := SearchMergedPullRequests(t.Context(), client, "acme", "widgets", []string{"alice"},
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("SearchMergedPullRequests: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	pr := results[0]
	if pr.FirstCommitAt != nil || pr.MergeCommitSHA != nil || pr.Additions != nil || pr.Deletions != nil {
		t.Fatalf("search result carries a completing-fetch-only field: %+v", pr)
	}
	if pr.MergedAt == nil {
		t.Fatal("search result MergedAt is nil, want the pull_request.merged_at value")
	}
}

// TestSearchMergedPullRequestsNormalizesABotAuthorToRESTForm proves the fix for a bot-authored
// population PR reconcile was deleting on every pass: GraphQL's author.login for a bot-created
// pull request is the bare account name ("sjawhar-agent"), carrying no "[bot]" suffix, while
// REST's identical pull_request.user.login for the same account is "sjawhar-agent[bot]" --
// IsPopulationPR and delivery_settings.population_authors are both written and compared in
// REST's form, so an un-normalized GraphQL result never matches and reconcile.reconcilePullRequest
// calls DeletePullRequest on every bot-authored population PR the search finds.
func TestSearchMergedPullRequestsNormalizesABotAuthorToRESTForm(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		node := map[string]any{
			"number":    60,
			"title":     "feat: agent-authored change",
			"url":       "https://github.com/acme/widgets/pull/60",
			"author":    map[string]any{"login": "acme-agent", "__typename": "Bot"},
			"createdAt": "2024-01-01T00:00:00Z",
			"mergedAt":  "2024-01-01T01:00:00Z",
			"labels":    map[string]any{"nodes": []any{}},
		}
		mustEncode(t, w, searchResponseJSON(1, []map[string]any{node}, false, ""))
	})
	client := fake.newTestClient()

	results, err := SearchMergedPullRequests(t.Context(), client, "acme", "widgets", []string{"acme-agent[bot]"},
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("SearchMergedPullRequests: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Author != "acme-agent[bot]" {
		t.Fatalf("Author = %q, want the REST form %q (GitHub's GraphQL login carries no [bot] suffix)", results[0].Author, "acme-agent[bot]")
	}
}

// searchAllInstallations is SearchMergedPullRequestsAcrossInstallation for a test that wants
// every result at once, from one window start, rather than per-installation windows with their
// own recorded progress (reconcile_test.go's subject).
func searchAllInstallations(ctx context.Context, client *githubapp.Client, authors []string, since, until time.Time) ([]FetchedPullRequest, error) {
	var mu sync.Mutex
	var found []FetchedPullRequest
	err := SearchMergedPullRequestsAcrossInstallation(ctx, client, authors,
		func(int64) (time.Time, error) { return since, nil }, until, nil,
		func(_ githubapp.Installation, _ time.Time, prs []FetchedPullRequest) error {
			mu.Lock()
			defer mu.Unlock()
			found = append(found, prs...)
			return nil
		})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// GitHub's error of a search page it answered 200 is shown to the bound a body is shown to
// (githubapp.ShownBody): the reconcile's search logs the page's error, and a message of 1 MiB in
// GitHub's errors array was a line of 1 MiB (the deep audit of cybermonklabs/legion#4209 at
// 3a21edb2c9, note 1). The start of the message, and its size, are kept.
func TestASearchPageGitHubAnswersWithAnErrorShowsItToABound(t *testing.T) {
	fake := newFakeGitHub(t)
	const start = "The search could not be completed "
	fake.handle("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, map[string]any{"data": nil, "errors": []any{map[string]any{"message": start + strings.Repeat("y", 1<<20-200)}}})
	})
	_, err := fetchSearchPage(t.Context(), fake.newTestClient(), "ghs_test", "repo:acme/widgets is:pr is:merged", "")
	if err == nil {
		t.Fatal("a search page GitHub answered with an error answered no error")
	}
	text := err.Error()
	t.Logf("bytes=%d", len(text))
	if len(text) > 1024 || !strings.Contains(text, start) || !strings.Contains(text, "bytes)") {
		t.Errorf("the search page's error is %d bytes and begins %.120q, want at most 1,024 that keep the start of GitHub's message and its size", len(text), text)
	}
}

// The cursor a search page names is GitHub's own text, the endCursor of the page before it, so each
// error of the page shows it to the bound a body is shown to (githubapp.ShownBody), as it shows
// GitHub's message (TestASearchPageGitHubAnswersWithAnErrorShowsItToABound): an endCursor as long
// as an answer may hold (githubapp's 1 MiB limit) was a line of that size in each of the five
// errors that name the cursor. The start of the cursor, and its size, are kept.
func TestEachErrorOfASearchPageShowsGitHubsCursorToABound(t *testing.T) {
	const start = "Y3Vyc29yOnYyOpK0"
	cursor := start + strings.Repeat("Q", 1<<20-4096)
	for _, page := range []struct {
		name   string
		answer http.HandlerFunc
	}{
		{name: "github_answers_errors", answer: func(w http.ResponseWriter, _ *http.Request) {
			mustEncode(t, w, map[string]any{"data": nil, "errors": []any{map[string]any{"message": "Something went wrong"}}})
		}},
		{name: "github_refuses_the_page", answer: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"Validation Failed"}`)
		}},
		{name: "the_page_does_not_decode", answer: func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "not json")
		}},
		{name: "the_connection_closes", answer: func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack the page's connection: %v", err)
				return
			}
			conn.Close()
		}},
	} {
		t.Run(page.name, func(t *testing.T) {
			fastPageRetries(t)
			fake := newFakeGitHub(t)
			fake.handle("POST /graphql", page.answer)
			_, err := fetchSearchPage(t.Context(), fake.newTestClient(), "ghs_test", "repo:acme/widgets is:pr is:merged", cursor)
			requireCursorShownToABound(t, err, start)
		})
	}
	t.Run("the_next_pages_token_is_not_minted", func(t *testing.T) {
		fake := newFakeGitHub(t)
		fake.handle("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
			node := searchNodeJSON(5, "t", "alice", "2024-01-01T00:00:00Z", "2024-01-01T01:00:00Z")
			mustEncode(t, w, searchResponseJSON(2, []map[string]any{node}, true, cursor))
		})
		var minted atomic.Int32
		token := func() (string, error) {
			if minted.Add(1) > 1 {
				return "", errors.New("the test mints no second token")
			}
			return "ghs_test", nil
		}
		err := searchMergedPullRequests(t.Context(), fake.newTestClient(), token, []string{"acme/widgets"}, []string{"alice"},
			time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
			func(time.Time, []FetchedPullRequest) error { return nil })
		requireCursorShownToABound(t, err, start)
	})
}

// requireCursorShownToABound fails t unless err is an error of at most 1,024 bytes that keeps the
// start of the cursor and its size.
func requireCursorShownToABound(t *testing.T, err error, start string) {
	t.Helper()
	if err == nil {
		t.Fatal("the page answered no error")
	}
	text := err.Error()
	t.Logf("bytes=%d", len(text))
	if len(text) > 1024 || !strings.Contains(text, start) || !strings.Contains(text, "bytes)") {
		t.Errorf("the page's error is %d bytes and begins %.160q, want at most 1,024 that keep the start of GitHub's cursor and its size", len(text), text)
	}
}
