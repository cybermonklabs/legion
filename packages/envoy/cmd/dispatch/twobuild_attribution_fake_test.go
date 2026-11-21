//go:build twobuild

// The stand-in GitHub of the two-build suite's attribution row (twobuild_attribution_test.go): the
// pull requests that GitHub holds, one server for each build that counts the reads of that build
// apart, and the answer to the facts query.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// attributionFake is the stand-in GitHub of the attribution row. It holds one state. Each build
// calls a server of its own (serve), so the fake counts the reads of each build apart. It answers
// the installation and its token, GET /repos/{o}/{r}, …/commits/{sha}, …/commits/{sha}/pulls,
// …/pulls/{n}, …/pulls/{n}/commits, and the facts query (POST /graphql). It answers any other path
// with an empty list and records the path.
type attributionFake struct {
	mu         sync.Mutex
	pulls      map[string]map[int]*fakePull
	reads      map[string]map[string]int
	asked      map[string][]int
	unexpected map[string][]string
}

// fakePull is one pull request of the stand-in, as GitHub holds it now. commits is each commit
// that GitHub holds for it, oldest first: a read of the pull requests of any of them names this
// one. Each commit has one message, which names no issue.
type fakePull struct {
	number    int
	head      string
	branch    string
	title     string
	open      bool
	updatedAt time.Time
	commits   []string
}

func newAttributionFake() *attributionFake {
	return &attributionFake{
		pulls: map[string]map[int]*fakePull{}, reads: map[string]map[string]int{},
		asked: map[string][]int{}, unexpected: map[string][]string{},
	}
}

// put stores pull in repo, in place of the pull request of the same number.
func (f *attributionFake) put(repo string, pull fakePull) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pulls[repo] == nil {
		f.pulls[repo] = map[int]*fakePull{}
	}
	f.pulls[repo][pull.number] = &pull
}

// move pushes head to pull request number of repo, and gives it title and the update time at.
func (f *attributionFake) move(repo string, number int, head, title string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pull := f.pulls[repo][number]
	pull.head, pull.title, pull.updatedAt = head, title, at
	pull.commits = append(pull.commits, head)
}

// countLocked adds one read of kind by build. The caller holds f.mu.
func (f *attributionFake) countLocked(build, kind string) {
	if f.reads[build] == nil {
		f.reads[build] = map[string]int{}
	}
	f.reads[build][kind]++
}

// count adds one read of kind by build.
func (f *attributionFake) count(build, kind string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.countLocked(build, kind)
}

// readsOf is the reads of build by kind, and the paths no route answered.
func (f *attributionFake) readsOf(build string) (map[string]int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.reads[build]), slices.Clone(f.unexpected[build])
}

// askedOf is each pull request number that build's facts queries asked, in their order.
func (f *attributionFake) askedOf(build string) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked[build])
}

// pullsAtLocked is each pull request of repo that holds the commit sha, in number order. The
// caller holds f.mu.
func (f *attributionFake) pullsAtLocked(repo, sha string) []*fakePull {
	var pulls []*fakePull
	for _, pull := range f.pulls[repo] {
		if slices.Contains(pull.commits, sha) {
			pulls = append(pulls, pull)
		}
	}
	slices.SortFunc(pulls, func(a, b *fakePull) int { return a.number - b.number })
	return pulls
}

// pullOfLocked is the request's repository and its pull request number, or nil. The caller holds
// f.mu.
func (f *attributionFake) pullOfLocked(r *http.Request) (string, *fakePull) {
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		return repo, nil
	}
	return repo, f.pulls[repo][number]
}

// rest is the pull request as GitHub's REST API answers it.
func (p *fakePull) rest(repo string) map[string]any {
	state, merged, mergedAt := "open", false, any(nil)
	if !p.open {
		state, merged, mergedAt = "closed", true, p.updatedAt.Format(time.RFC3339)
	}
	return map[string]any{
		"number": p.number, "title": p.title, "body": "", "html_url": fmt.Sprintf("https://github.com/%s/pull/%d", repo, p.number),
		"state": state, "merged": merged, "merged_at": mergedAt, "updated_at": p.updatedAt.Format(time.RFC3339),
		"user": map[string]any{"login": "octocat"}, "labels": []any{},
		"head": map[string]any{"ref": p.branch, "sha": p.head, "repo": map[string]any{"full_name": repo}},
		"base": map[string]any{"ref": "main"},
	}
}

// node is the pull request as the facts query answers it (delivery's factsNode), with every
// commit message on its first page.
func (p *fakePull) node(repo string) map[string]any {
	state, mergedAt := "OPEN", any(nil)
	if !p.open {
		state, mergedAt = "MERGED", p.updatedAt.Format(time.RFC3339)
	}
	messages := make([]any, len(p.commits))
	for i := range p.commits {
		messages[i] = map[string]any{"commit": map[string]any{"message": fmt.Sprintf("Commit %d.", i+1)}}
	}
	return map[string]any{
		"number": p.number, "title": p.title, "body": "", "url": fmt.Sprintf("https://github.com/%s/pull/%d", repo, p.number),
		"state": state, "merged": !p.open, "mergedAt": mergedAt, "updatedAt": p.updatedAt.Format(time.RFC3339),
		"headRefName": p.branch, "headRefOid": p.head, "baseRefName": "main",
		"headRepository": map[string]any{"nameWithOwner": repo}, "mergeCommit": nil,
		"commits": map[string]any{"totalCount": len(p.commits), "nodes": messages},
	}
}

// writeGitHubJSON writes value as the body of a GitHub answer with status.
func writeGitHubJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode the answer of the stand-in GitHub: %v", err)
	}
}

// serve starts a server on the fake's state whose reads are build's, and returns its URL: the
// DISPATCH_GITHUB_API_BASE of build's processes.
func (f *attributionFake) serve(t *testing.T, build string) string {
	t.Helper()
	notFound := map[string]string{"message": "Not Found"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		f.count(build, "installation")
		id, installed := attributionInstallations[r.PathValue("owner")+"/"+r.PathValue("repo")]
		if !installed {
			writeGitHubJSON(t, w, http.StatusNotFound, notFound)
			return
		}
		writeGitHubJSON(t, w, http.StatusOK, map[string]any{
			"id": id, "app_slug": "twobuild", "permissions": map[string]string{"contents": "read", "pull_requests": "read"},
		})
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		f.count(build, "token")
		writeGitHubJSON(t, w, http.StatusCreated, map[string]string{
			"token": "ghs_twobuild_" + r.PathValue("id"), "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		f.count(build, "repository")
		writeGitHubJSON(t, w, http.StatusOK, map[string]string{
			"full_name": r.PathValue("owner") + "/" + r.PathValue("repo"), "default_branch": "main",
		})
	})
	// The commit itself: a result's record asks whether the repository holds it
	// (githubapp.Client.HasCommit), and only a 422 of this message proves that it does not.
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.countLocked(build, "commit")
		sha := r.PathValue("sha")
		if len(f.pullsAtLocked(r.PathValue("owner")+"/"+r.PathValue("repo"), sha)) == 0 {
			writeGitHubJSON(t, w, http.StatusUnprocessableEntity, map[string]string{"message": "No commit found for SHA: " + sha})
			return
		}
		_, _ = io.WriteString(w, sha)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.countLocked(build, "commit pulls")
		repo := r.PathValue("owner") + "/" + r.PathValue("repo")
		answer := []any{}
		for _, pull := range f.pullsAtLocked(repo, r.PathValue("sha")) {
			answer = append(answer, pull.rest(repo))
		}
		writeGitHubJSON(t, w, http.StatusOK, answer)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.countLocked(build, "pull")
		repo, pull := f.pullOfLocked(r)
		if pull == nil {
			writeGitHubJSON(t, w, http.StatusNotFound, notFound)
			return
		}
		writeGitHubJSON(t, w, http.StatusOK, pull.rest(repo))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.countLocked(build, "pull commits")
		_, pull := f.pullOfLocked(r)
		if pull == nil {
			writeGitHubJSON(t, w, http.StatusNotFound, notFound)
			return
		}
		answer := []any{}
		if page := r.URL.Query().Get("page"); page == "" || page == "1" {
			for i, sha := range pull.commits {
				at := map[string]string{"date": pull.updatedAt.Format(time.RFC3339)}
				answer = append(answer, map[string]any{"sha": sha, "commit": map[string]any{
					"message": fmt.Sprintf("Commit %d.", i+1), "author": at, "committer": at,
				}})
			}
		}
		writeGitHubJSON(t, w, http.StatusOK, answer)
	})
	mux.HandleFunc("POST /graphql", f.serveFacts(t, build))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.unexpected[build] = append(f.unexpected[build], r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		writeGitHubJSON(t, w, http.StatusOK, []any{})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

// serveFacts answers the facts query (delivery's readFactsBatch), whose text holds the fragment
// pullRequestFacts. A pull request the stand-in does not hold is GitHub's NOT_FOUND on its alias
// path, and a repository the App is not installed on is NOT_FOUND on its alias.
func (f *attributionFake) serveFacts(t *testing.T, build string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !strings.Contains(request.Query, "fragment pullRequestFacts") {
			f.mu.Lock()
			f.unexpected[build] = append(f.unexpected[build], "POST /graphql (not the facts query)")
			f.mu.Unlock()
			writeGitHubJSON(t, w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.countLocked(build, "graphql")
		data := map[string]any{}
		var refusals []any
		var current map[string]any
		var repoAlias, repo string
		for _, alias := range attributionFactsAlias.FindAllStringSubmatch(request.Query, -1) {
			if alias[1] != "" {
				owner, _ := request.Variables[alias[2]].(string)
				name, _ := request.Variables[alias[3]].(string)
				repoAlias, repo = alias[1], owner+"/"+name
				if _, installed := attributionInstallations[repo]; !installed {
					current, data[repoAlias] = nil, nil
					refusals = append(refusals, map[string]any{"type": "NOT_FOUND", "path": []any{repoAlias},
						"message": fmt.Sprintf("Could not resolve to a Repository with the name '%s'.", repo)})
					continue
				}
				current = map[string]any{}
				data[repoAlias] = current
				continue
			}
			if current == nil {
				continue
			}
			number, _ := strconv.Atoi(alias[5])
			f.asked[build] = append(f.asked[build], number)
			pull := f.pulls[repo][number]
			if pull == nil {
				current[alias[4]] = nil
				refusals = append(refusals, map[string]any{"type": "NOT_FOUND", "path": []any{repoAlias, alias[4]},
					"message": fmt.Sprintf("Could not resolve to a PullRequest with the number of %d.", number)})
				continue
			}
			current[alias[4]] = pull.node(repo)
		}
		answer := map[string]any{"data": data}
		if len(refusals) > 0 {
			answer["errors"] = refusals
		}
		writeGitHubJSON(t, w, http.StatusOK, answer)
	}
}
