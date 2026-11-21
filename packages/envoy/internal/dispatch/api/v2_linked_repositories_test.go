package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The facts #4426 taught the /api/v1 results and repository routes, shown on their /api/v2
// successors: a result at a commit of a repository the issue links a pull request in is recorded
// and read through /api/v2 with that repository named; a linked read that did not answer is 502
// GITHUB_UNREADABLE on the /api/v2 record and dry run alike; a repository path GitHub's names
// refuse is 400 INVALID_REPO_PROJECT on the /api/v2 mapping write and app-installation read; the
// /api/v2 mapping removal removes only the mapping stored under its path's own spelling; and the
// mention rules the dispatch_comment tool quotes are the /api/v2 comment create's own.

// readChecksV2 is GET /api/v2/issues/{issue}/checks, its page read as the /api/v1 read's checks so
// the results tests' helpers (checkOf, standsAt) read it.
func readChecksV2(t *testing.T, handler http.Handler, issue string) issueResults {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v2/issues/"+issue+"/checks", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v2/issues/%s/checks: status=%d body=%s", issue, response.Code, response.Body.String())
	}
	return issueResults{Issue: issue, Checks: decodeBody[page[model.CheckBlock]](t, response).Items}
}

// A result at the head of a pull request the issue links in another repository is taken by POST
// /api/v2/issues/{key}/results, and its dry run answers the line first, as on /api/v1
// (TestABeforeMergeResultAtALinkedCommitIsReadThroughThatRepository): the record keeps the linked
// repository with the row, and GET /api/v2/issues/{key}/checks compares the result with the
// linked pull request's head, the basis and the compared head naming that repository as repo,
// while the issue's own pull request names none. Recorded against the issue's own repository
// alone, the commit would be 400 RESULT_COMMIT_UNKNOWN, since acme/widgets does not hold it.
func TestV2RecordsAndReadsAResultAtALinkedRepositorysCommit(t *testing.T) {
	// linkedCommit is a commit acme/tools holds and acme/widgets does not.
	const linkedCommit = "9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d"
	linked := &linkedGitHub{
		held:  map[string][]string{"acme/widgets": {headCommit}, "acme/tools": {linkedCommit}},
		pulls: map[string]string{"acme/tools#7": "open"},
		heads: map[string]int{},
	}
	handler, database := linkedRepositoriesGitHub(t, linked)
	issue := issueInRepository(t, handler, "RELV", "acme/widgets", "A linked result on /api/v2").Key
	uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", twoChecks)
	linkPullRequests(t, handler, issue, "https://github.com/acme/tools/pull/7")
	writeHead(t, database, "acme/widgets", 5, headCommit, "open", issue)
	body := resultBody("k1", "pass", linkedCommit, nil)

	dry := dispatchRequest(t, handler, http.MethodPost, "/api/v2/issues/"+issue+"/results/dry-run", body, "alice")
	if dry.Code != http.StatusOK {
		t.Fatalf("the /api/v2 dry run at the linked commit: status=%d body=%s, want 200", dry.Code, dry.Body.String())
	}
	if line, want := decodeBody[resultDryRun](t, dry).Line, "pass at "+linkedCommit; !strings.Contains(line, want) {
		t.Errorf("the /api/v2 dry run's line = %q, want it to say %q", line, want)
	}
	recorded := dispatchRequest(t, handler, http.MethodPost, "/api/v2/issues/"+issue+"/results", body, "alice")
	if recorded.Code != http.StatusCreated {
		t.Fatalf("the /api/v2 record at the linked commit: status=%d body=%s, want 201", recorded.Code, recorded.Body.String())
	}
	result := decodeBody[recordedResult](t, recorded).Result
	if repo := storedCommitRepo(t, database, result); repo == nil || *repo != "acme/tools" {
		t.Fatalf("the /api/v2 record stored commit_repo %v, want acme/tools", repo)
	}

	// acme/tools#7 opens at the result's commit, and no event tells Dispatch of it.
	linked.mu.Lock()
	linked.heads["acme/tools@"+linkedCommit] = 7
	linked.mu.Unlock()
	read := readChecksV2(t, handler, issue)
	stands := standsAt(t, read, "k1", result.ID, "a before_merge result at the linked pull request's head, read through /api/v2",
		model.CommitAtBasis, false)
	if basis := stands.Basis; basis == nil || basis.Kind != model.CheckBasisPullRequest || basis.Repo != "acme/tools" ||
		basis.PullRequest == nil || *basis.PullRequest != 7 || basis.Commit != linkedCommit {
		t.Fatalf("that result stands at %+v, want acme/tools#7 at %s", basis, linkedCommit)
	}
	compared := checkOf(t, read, "k1").ComparedWith
	if len(compared) != 2 {
		t.Fatalf("k1 compares with %+v on /api/v2, want the issue's own #5 and the linked acme/tools#7", compared)
	}
	for _, basis := range compared {
		if want := map[int]string{5: "", 7: "acme/tools"}[*basis.PullRequest]; basis.Repo != want {
			t.Errorf("k1 compares with %+v on /api/v2, want #%d to name the repository %q", basis, *basis.PullRequest, want)
		}
	}
}

// A read of a linked pull request that did not answer, where no repository is shown to hold the
// commit, refuses the /api/v2 record and its dry run alike with 502 GITHUB_UNREADABLE, and nothing
// is stored; a 500 is refused in the same words by both, and a rate limit names GitHub's wait in
// Retry-After on both. The record after a rate-limited dry run words its refusal by the limit
// Dispatch then holds, sending GitHub nothing, so its words are not the dry run's. The negative
// control is the same body where GitHub answers the linked read: the record takes it.
func TestV2RefusesAResultWhoseLinkedReadDidNotAnswer(t *testing.T) {
	const linkedCommit = "9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9e"
	wait := strconv.Itoa(int(rateLimitWait / time.Second))
	for _, row := range []struct {
		name       string
		github     func(*linkedGitHub)
		status     int
		retryAfter string
		sameWords  bool
	}{
		{"a linked read GitHub answered 500", func(linked *linkedGitHub) { linked.erroring = []string{"acme/tools"} },
			http.StatusBadGateway, "", true},
		{"a linked read GitHub rate-limited", func(linked *linkedGitHub) { linked.rateLimited = []string{"acme/tools"} },
			http.StatusBadGateway, wait, false},
		{"a linked read GitHub answered", func(*linkedGitHub) {}, http.StatusCreated, "", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			linked := &linkedGitHub{
				held:  map[string][]string{"acme/widgets": {headCommit}, "acme/tools": {linkedCommit}},
				pulls: map[string]string{"acme/tools#7": "open"},
			}
			row.github(linked)
			handler, database := linkedRepositoriesGitHub(t, linked)
			issue := issueInRepository(t, handler, "RELU", "acme/widgets", "An unanswered linked read on /api/v2").Key
			uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", twoChecks)
			linkPullRequests(t, handler, issue, "https://github.com/acme/tools/pull/7")
			body := resultBody("k1", "pass", linkedCommit, nil)

			dry := dispatchRequest(t, handler, http.MethodPost, "/api/v2/issues/"+issue+"/results/dry-run", body, "alice")
			recorded := dispatchRequest(t, handler, http.MethodPost, "/api/v2/issues/"+issue+"/results", body, "alice")
			if recorded.Code != row.status {
				t.Fatalf("the /api/v2 record: status=%d body=%s, want %d", recorded.Code, recorded.Body.String(), row.status)
			}
			if row.status == http.StatusCreated {
				if dry.Code != http.StatusOK {
					t.Fatalf("the /api/v2 dry run: status=%d body=%s, want 200 where the record is taken", dry.Code, dry.Body.String())
				}
				return
			}
			if dry.Code != row.status {
				t.Fatalf("the /api/v2 dry run: status=%d body=%s, want the record's %d", dry.Code, dry.Body.String(), row.status)
			}
			dryCode, dryText := refusalOf(t, dry)
			code, text := refusalOf(t, recorded)
			if dryCode != "GITHUB_UNREADABLE" || code != "GITHUB_UNREADABLE" || (row.sameWords && dryText != text) {
				t.Errorf("the dry run refused %s %q and the record %s %q, want both GITHUB_UNREADABLE (in one sentence: %t)",
					dryCode, dryText, code, text, row.sameWords)
			}
			for _, answer := range []struct {
				name     string
				response *httptest.ResponseRecorder
				text     string
			}{{"dry run", dry, dryText}, {"record", recorded, text}} {
				if !strings.Contains(answer.text, "acme/tools") || !strings.Contains(answer.text, "record the result again") {
					t.Errorf("the /api/v2 %s's refusal reads %q, want it to name acme/tools and say to record the result again", answer.name, answer.text)
				}
				if header := answer.response.Header().Get("Retry-After"); header != row.retryAfter {
					t.Errorf("the /api/v2 %s carries Retry-After %q, want %q", answer.name, header, row.retryAfter)
				}
			}
			if rows := resultRowsOn(t, database, issue); rows != 0 {
				t.Errorf("the refused /api/v2 record left %d check_results rows, want none", rows)
			}
		})
	}
}

// The /api/v2 mapping write and the /api/v2 app-installation read hold the path's repository to
// the names GitHub allows, as their /api/v1 predecessors do (TestRepoProjectMappingPathIsHeldToTheNamesGitHubAllows,
// TestAppInstallationPathIsHeldToTheNamesGitHubAllows): `...git` and `.git` leave `..` or an
// empty name once `.git` is trimmed, and each is 400 INVALID_REPO_PROJECT, the read asking GitHub
// nothing and the write storing nothing. A renamed_to the rule refuses is refused the same way.
// The negative control is a path GitHub allows, which the read asks GitHub about.
func TestV2RepositoryPathsAreHeldToTheNamesGitHubAllows(t *testing.T) {
	var asked atomic.Int32
	handler := appInstallationServer(t, func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	})
	createTestProject(t, handler, "CORE")
	refused := func(what string, response *httptest.ResponseRecorder) {
		t.Helper()
		if code, _ := refusalOf(t, response); response.Code != http.StatusBadRequest || code != "INVALID_REPO_PROJECT" {
			t.Errorf("%s: status=%d body=%s, want 400 INVALID_REPO_PROJECT", what, response.Code, response.Body.String())
		}
	}
	for _, path := range []string{"acme/...git", "acme/.git", "acme/.git.git", "acme/...git.git"} {
		refused("GET /api/v2/repositories/"+path+"/app-installation",
			dispatchRequest(t, handler, http.MethodGet, "/api/v2/repositories/"+path+"/app-installation", nil, "alice"))
		refused("PUT /api/v2/repositories/"+path,
			dispatchRequest(t, handler, http.MethodPut, "/api/v2/repositories/"+path, map[string]any{"project": "CORE", "default": true}, "alice"))
	}
	if requests := asked.Load(); requests != 0 {
		t.Fatalf("the refused paths sent GitHub %d requests, want none", requests)
	}
	for _, rename := range []string{"acme/...git", "acme/.git"} {
		refused("PUT /api/v2/repositories/acme/tools renamed_to "+rename,
			dispatchRequest(t, handler, http.MethodPut, "/api/v2/repositories/acme/tools", map[string]any{"project": "CORE", "renamed_to": rename}, "alice"))
	}
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v2/repositories", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list the mappings: status=%d body=%s", listed.Code, listed.Body.String())
	}
	if mappings := decodeBody[page[model.RepoProject]](t, listed).Items; len(mappings) != 0 {
		t.Fatalf("the refused writes stored %+v, want no mapping", mappings)
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v2/repositories/acme/tools/app-installation", nil, "alice")
	if read.Code != http.StatusOK || decodeBody[appInstallationResponse](t, read).Installed || asked.Load() == 0 {
		t.Fatalf("GET /api/v2/repositories/acme/tools/app-installation: status=%d body=%s after %d GitHub requests, want 200 not installed, GitHub asked",
			read.Code, read.Body.String(), asked.Load())
	}
}

// DELETE /api/v2/repositories/{owner}/{repo} removes only the mapping stored under its path's own
// spelling, as the /api/v1 removal does (TestRepoProjectDeleteRemovesOnlyTheMappingItsPathSpells),
// and answers a spelling no mapping is stored under 404 REPOSITORY_NOT_FOUND, its own refusal of a
// repository no mapping names: the PUT of `acme/x.git.git` stores `acme/x.git` beside `acme/x`;
// the DELETE of `acme/x.git.git` finds nothing; that of `acme/x.git` removes `acme/x.git` alone;
// that of `Acme/X.git`, which the canonical reading reads as `acme/x`, finds nothing; a mapping
// direct SQL stored as `Acme/X` is removed by that spelling, and the same DELETE sent again finds
// nothing; and that of `acme/x` removes `acme/x`.
func TestV2RepositoryDeleteRemovesOnlyTheMappingItsPathSpells(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "CORE")
	for _, path := range []string{"acme/x.git.git", "acme/x"} {
		if response := dispatchRequest(t, handler, http.MethodPut, "/api/v2/repositories/"+path,
			map[string]string{"project": "CORE"}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("map %s: status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	stored := func() []string {
		t.Helper()
		listed := dispatchRequest(t, handler, http.MethodGet, "/api/v2/repositories", nil, "alice")
		if listed.Code != http.StatusOK {
			t.Fatalf("list the mappings: status=%d body=%s", listed.Code, listed.Body.String())
		}
		var repos []string
		for _, mapping := range decodeBody[page[model.RepoProject]](t, listed).Items {
			repos = append(repos, mapping.Repo)
		}
		slices.Sort(repos)
		return repos
	}
	deleteLeaving := func(path string, status int, want ...string) {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodDelete, "/api/v2/repositories/"+path, nil, "alice")
		if response.Code != status {
			t.Fatalf("DELETE /api/v2/repositories/%s: status=%d body=%s, want %d", path, response.Code, response.Body.String(), status)
		}
		if status == http.StatusNotFound {
			if code, _ := refusalOf(t, response); code != "REPOSITORY_NOT_FOUND" {
				t.Fatalf("DELETE /api/v2/repositories/%s refused %s, want REPOSITORY_NOT_FOUND", path, code)
			}
		}
		if got := stored(); !slices.Equal(got, want) {
			t.Fatalf("after DELETE /api/v2/repositories/%s the mappings are %q, want %q", path, got, want)
		}
	}

	if got := stored(); !slices.Equal(got, []string{"acme/x", "acme/x.git"}) {
		t.Fatalf("the /api/v2 PUTs stored %q, want acme/x and acme/x.git", got)
	}
	deleteLeaving("acme/x.git.git", http.StatusNotFound, "acme/x", "acme/x.git")
	deleteLeaving("acme/x.git", http.StatusNoContent, "acme/x")
	deleteLeaving("Acme/X.git", http.StatusNotFound, "acme/x")
	if _, err := database.Pool.Exec(t.Context(), `
		insert into repo_projects (repo, project, created_by) values ('Acme/X', 'CORE', '{"kind":"system","id":"test"}'::jsonb)
	`); err != nil {
		t.Fatalf("store the mapping Acme/X by SQL: %v", err)
	}
	deleteLeaving("Acme/X", http.StatusNoContent, "acme/x")
	deleteLeaving("Acme/X", http.StatusNotFound, "acme/x")
	deleteLeaving("acme/x", http.StatusNoContent)
}

// The mention rules the dispatch_comment tool quotes in Dispatch's words (the contracts'
// COMMENT_MENTION_*_RULE, validateCommentMentions) are POST /api/v2/issues/{key}/comments's own:
// a target of another form, a target named twice and an eleventh target are each 400
// MENTION_INPUT in the same words as POST /api/v1/issues/{key}/comments, and no comment is
// written by either.
func TestV2CommentMentionsAreHeldToTheRulesTheCommentToolQuotes(t *testing.T) {
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "MENV", "Mentions on /api/v2")
	eleven := make([]map[string]any, 11)
	for index := range eleven {
		eleven[index] = map[string]any{"target": fmt.Sprintf("session:s%d", index)}
	}
	for _, row := range []struct {
		name     string
		mentions []map[string]any
		words    string
	}{
		{"a target of another form", []map[string]any{{"target": "architect"}}, "mention target must be role:<name> or session:<id>"},
		{"a target named twice", []map[string]any{{"target": "role:architect"}, {"target": "role:architect"}}, "mentions must not repeat a target"},
		{"an eleventh target", eleven, "mentions allow at most 10 targets"},
	} {
		body := map[string]any{"body": "Over to you.", "mentions": row.mentions}
		v1 := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments", body, "alice")
		v2 := dispatchRequest(t, handler, http.MethodPost, "/api/v2/issues/"+issue+"/comments", body, "alice")
		v1Code, v1Text := refusalOf(t, v1)
		v2Code, v2Text := refusalOf(t, v2)
		if v1.Code != http.StatusBadRequest || v2.Code != http.StatusBadRequest || v1Code != "MENTION_INPUT" || v2Code != "MENTION_INPUT" ||
			!strings.Contains(v1Text, row.words) || v2Text != v1Text {
			t.Errorf("%s: /api/v1 %d %s %q, /api/v2 %d %s %q, want both 400 MENTION_INPUT saying %q",
				row.name, v1.Code, v1Code, v1Text, v2.Code, v2Code, v2Text, row.words)
		}
	}
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v2/issues/"+issue+"/comments", nil, "alice")
	if listed.Code != http.StatusOK || decodeBody[page[map[string]any]](t, listed).Total != 0 {
		t.Fatalf("GET /api/v2/issues/%s/comments: status=%d body=%s, want no comment written", issue, listed.Code, listed.Body.String())
	}
}
