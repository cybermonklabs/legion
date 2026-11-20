package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// restrictedRepoRefusal is the whole answer to an issue write by a restricted member that names
// repo (refuseRepoOfRestrictedMember).
const restrictedRepoRefusal = `{"code":"RESTRICTED_MEMBER","error":"a restricted member cannot set or clear an issue's repo; only a full member names an issue's own repository"}`

// newIssueRepoAccessServer is newAccessServer with erin named on ACC's list, so she sees ACC, and
// acme/widgets mapped to ACC as its default repository, so an issue of ACC with no repository of
// its own resolves to it.
func newIssueRepoAccessServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	handler, database := newAccessServer(t)
	if named := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); named.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC: status=%d body=%s", named.Code, named.Body.String())
	}
	if mapped := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets",
		map[string]any{"project": "ACC", "default": true}, "alice"); mapped.Code != http.StatusOK {
		t.Fatalf("map acme/widgets to ACC as its default: status=%d body=%s", mapped.Code, mapped.Body.String())
	}
	return handler, database
}

// createdIssueKey files body as login and answers the new issue's key, failing unless it is 201.
func createdIssueKey(t *testing.T, handler http.Handler, login string, body map[string]any) string {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, login)
	if created.Code != http.StatusCreated {
		t.Fatalf("%s files %v: status=%d body=%s", login, body, created.Code, created.Body.String())
	}
	return decodeBody[model.Issue](t, created).Key
}

// issueWritesSnapshot is everything an issue write could have written: each issue's repository,
// title and status, and how many events, related-item records and near-copy records exist.
func issueWritesSnapshot(t *testing.T, database *store.Store) string {
	t.Helper()
	var snapshot string
	if err := database.Pool.QueryRow(context.Background(), `
		select concat_ws(' | ',
			(select count(*) from events), (select count(*) from related_checks), (select count(*) from near_copy_checks),
			(select coalesce(string_agg(concat_ws(',', key, coalesce(repo, '-'), title, status), '; ' order by key), '') from issues))
	`).Scan(&snapshot); err != nil {
		t.Fatalf("read what the issue writes wrote: %v", err)
	}
	return snapshot
}

// proxyShows answers whether the GitHub proxy shows login repository: its own read
// (routes' repositoryVisibleQuery, refs.RepositoryClause) for the viewer it resolves a cookie to
// (access.Resolve of model.BrowserActor).
func proxyShows(t *testing.T, database *store.Store, login, repository string) bool {
	t.Helper()
	viewer, err := access.Resolve(context.Background(), database.Pool, model.BrowserActor(login))
	if err != nil {
		t.Fatalf("resolve what %s sees: %v", login, err)
	}
	var shown bool
	if err := database.Pool.QueryRow(context.Background(), `select `+refs.RepositoryClause("$2", "$1"),
		refs.VisibilityArg(model.Actor{Viewer: &viewer}), repository).Scan(&shown); err != nil {
		t.Fatalf("read whether %s sees %s: %v", login, repository, err)
	}
	return shown
}

// TestARestrictedMemberNamesNoIssuesRepo: a restricted member's create or patch that names repo, a
// string or null, is 403 RESTRICTED_MEMBER, the same bytes whatever else the body names and
// whether the project or the issue exists, and nothing is written: no issue, no repository, no
// event, no related-item or near-copy record, no title, no close. That holds at their browser and
// for a personal token acting for them. So no project they see comes to own a repository through
// them, and the GitHub proxy still shows them none such. The same writes naming no repo are theirs
// to make.
func TestARestrictedMemberNamesNoIssuesRepo(t *testing.T) {
	handler, database := newIssueRepoAccessServer(t)
	ownRepo := createdIssueKey(t, handler, "alice", map[string]any{"project": "ACC", "title": "Alice names its repository", "repo": "acme/gadgets", "force": true})
	noRepo := createdIssueKey(t, handler, "alice", map[string]any{"project": "ACC", "title": "Alice names no repository", "force": true})
	createdIssueKey(t, handler, "alice", map[string]any{"project": "ACC", "external": "acme/widgets#7"})
	erinsToken := mintListed(t, handler, accessErin, "erin's laptop", nil).Token

	type call struct {
		name, method, path string
		body               map[string]any
	}
	var calls []call
	for _, body := range []map[string]any{
		{"project": "ACC", "title": "Erin names a repository", "repo": "acme/secret", "force": true},
		{"project": "ACC", "title": "Erin names a null repository", "repo": nil, "force": true},
		{"project": "ACC", "external": "acme/widgets#7", "repo": "acme/secret"},
		{"project": "NOPE", "title": "Erin names a repository in no project", "repo": "acme/secret"},
	} {
		calls = append(calls, call{fmt.Sprintf("create %v", body), http.MethodPost, "/api/v1/issues", body})
	}
	for _, patch := range []struct {
		key  string
		body map[string]any
	}{
		{noRepo, map[string]any{"repo": "acme/secret"}},
		{ownRepo, map[string]any{"repo": nil}},
		{ownRepo, map[string]any{"repo": "acme/secret"}},
		{noRepo, map[string]any{"repo": "acme/secret", "title": "Erin's new title"}},
		{noRepo, map[string]any{"repo": "acme/secret", "status": "done"}},
		{"ACC-999", map[string]any{"repo": "acme/secret"}},
	} {
		calls = append(calls, call{fmt.Sprintf("patch %s %v", patch.key, patch.body), http.MethodPatch, "/api/v1/issues/" + patch.key, patch.body})
	}
	for _, call := range calls {
		for _, caller := range []struct {
			name string
			send func(body map[string]any) *httptest.ResponseRecorder
		}{
			{"erin's browser", func(body map[string]any) *httptest.ResponseRecorder {
				return dispatchRequest(t, handler, call.method, call.path, body, accessErin)
			}},
			{"erin's personal token", func(body map[string]any) *httptest.ResponseRecorder {
				withActor := map[string]any{"actor": sessionActor()}
				for field, value := range body {
					withActor[field] = value
				}
				return agentRequest(t, handler, call.method, call.path, withActor, erinsToken)
			}},
		} {
			before := issueWritesSnapshot(t, database)
			response := caller.send(call.body)
			if got := strings.TrimSpace(response.Body.String()); response.Code != http.StatusForbidden || got != restrictedRepoRefusal {
				t.Errorf("%s: %s = %d %s, want 403 %s", caller.name, call.name, response.Code, got, restrictedRepoRefusal)
			}
			if after := issueWritesSnapshot(t, database); after != before {
				t.Errorf("%s: the refused %s wrote:\nbefore %s\nafter  %s", caller.name, call.name, before, after)
			}
		}
	}
	if proxyShows(t, database, accessErin, "acme/secret") {
		t.Errorf("the GitHub proxy shows erin acme/secret, which only her refused writes named")
	}
	// The instrument's own control: the same read shows erin ACC's default repository, which a
	// project she sees owns, so the false above is the clause's answer, not a read that shows nothing.
	if !proxyShows(t, database, accessErin, "acme/widgets") {
		t.Fatalf("the GitHub proxy hides acme/widgets, ACC's default, from erin, who sees ACC")
	}

	// The same writes naming no repo are hers: the refusal is of the field, not of the write.
	if created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "ACC", "title": "Erin files an issue", "force": true}, accessErin); created.Code != http.StatusCreated ||
		repoOf(decodeBody[model.Issue](t, created).Repo) != "<none>" || repoOf(decodeBody[model.Issue](t, created).ResolvedRepo) != "acme/widgets" {
		t.Errorf("erin files an issue naming no repo: status=%d body=%s, want 201 resolving to ACC's default", created.Code, created.Body.String())
	}
	if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+ownRepo,
		map[string]any{"title": "Erin renames it"}, accessErin); patched.Code != http.StatusOK ||
		repoOf(decodeBody[model.Issue](t, patched).Repo) != "acme/gadgets" {
		t.Errorf("erin retitles an issue with alice's repository: status=%d body=%s, want 200 keeping acme/gadgets", patched.Code, patched.Body.String())
	}
}

// repoOf is a repository an answer names, "<none>" for none.
func repoOf(repo *string) string {
	if repo == nil {
		return "<none>"
	}
	return *repo
}

// answerMasks are the parts of an answer that differ from one run to the next: a time, a uuid and a
// duration in milliseconds.
var answerMasks = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`), "<time>"},
	{regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`), "<uuid>"},
	{regexp.MustCompile(`"(\w+_ms)":\d+`), `"$1":<ms>`},
}

// maskedAnswer is an answer with answerMasks applied, the bytes two runs of one build share.
func maskedAnswer(response *httptest.ResponseRecorder) string {
	body := strings.TrimSpace(response.Body.String())
	for _, mask := range answerMasks {
		body = mask.pattern.ReplaceAllString(body, mask.replacement)
	}
	return fmt.Sprintf("%d %s", response.Code, body)
}

// TestAFullMembersIssueRepoAnswersAsBefore: a full member's create and patch that name repo, at
// their browser and through their personal token, are taken as they always were, in a deployment
// that holds a restricted member: the string is canonicalised and stored, null clears it, a value
// that is not owner/name is 400 INVALID_ISSUE and a closed issue 409 ISSUE_CLOSED, in the words
// the base answers them. Each answer is logged masked (maskedAnswer), so two builds' runs compare
// byte for byte.
func TestAFullMembersIssueRepoAnswersAsBefore(t *testing.T) {
	handler, database := newIssueRepoAccessServer(t)
	carolsToken := mintListed(t, handler, accessCarol, "carol's laptop", nil).Token
	for _, caller := range []struct {
		name string
		send func(method, path string, body map[string]any) *httptest.ResponseRecorder
	}{
		{"alice's browser", func(method, path string, body map[string]any) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, method, path, body, "alice")
		}},
		{"carol's personal token", func(method, path string, body map[string]any) *httptest.ResponseRecorder {
			withActor := map[string]any{"actor": sessionActor()}
			for field, value := range body {
				withActor[field] = value
			}
			return agentRequest(t, handler, method, path, withActor, carolsToken)
		}},
	} {
		answer := func(step string, response *httptest.ResponseRecorder) *httptest.ResponseRecorder {
			t.Logf("R9 %s %s: %s", caller.name, step, maskedAnswer(response))
			return response
		}
		created := answer("create naming Acme/Gadgets.git", caller.send(http.MethodPost, "/api/v1/issues",
			map[string]any{"project": "ACC", "title": "Named by " + caller.name, "repo": "Acme/Gadgets.git", "force": true}))
		issue := decodeBody[model.Issue](t, created)
		if created.Code != http.StatusCreated || repoOf(issue.Repo) != "acme/gadgets" || repoOf(issue.ResolvedRepo) != "acme/gadgets" {
			t.Fatalf("%s creates naming Acme/Gadgets.git: status=%d body=%s, want 201 with acme/gadgets its own", caller.name, created.Code, created.Body.String())
		}
		path := "/api/v1/issues/" + issue.Key
		changed := answer("patch to acme/other", caller.send(http.MethodPatch, path, map[string]any{"repo": "acme/other"}))
		if changed.Code != http.StatusOK || repoOf(decodeBody[model.Issue](t, changed).Repo) != "acme/other" {
			t.Fatalf("%s changes the repo: status=%d body=%s, want 200 with acme/other", caller.name, changed.Code, changed.Body.String())
		}
		cleared := answer("patch to null", caller.send(http.MethodPatch, path, map[string]any{"repo": nil}))
		if after := decodeBody[model.Issue](t, cleared); cleared.Code != http.StatusOK || repoOf(after.Repo) != "<none>" || repoOf(after.ResolvedRepo) != "acme/widgets" {
			t.Fatalf("%s clears the repo: status=%d body=%s, want 200 resolving to ACC's default", caller.name, cleared.Code, cleared.Body.String())
		}
		for _, refused := range []struct {
			step, method, path string
			body               map[string]any
			want               string
		}{
			{"create naming not-a-repo", http.MethodPost, "/api/v1/issues", map[string]any{"project": "ACC", "title": "Bad", "repo": "not-a-repo", "force": true},
				`400 {"code":"INVALID_ISSUE","error":"repo must be \"owner/name\" (got \"not-a-repo\")"}`},
			{"patch naming 7", http.MethodPatch, path, map[string]any{"repo": 7},
				`400 {"code":"INVALID_ISSUE","error":"repo must be \"owner/name\" or null"}`},
		} {
			before := issueWritesSnapshot(t, database)
			if got := maskedAnswer(answer(refused.step, caller.send(refused.method, refused.path, refused.body))); got != refused.want {
				t.Errorf("%s %s = %s, want %s", caller.name, refused.step, got, refused.want)
			}
			if after := issueWritesSnapshot(t, database); after != before {
				t.Errorf("%s: the refused %s wrote:\nbefore %s\nafter  %s", caller.name, refused.step, before, after)
			}
		}
		if closed := answer("close", caller.send(http.MethodPatch, path, map[string]any{"status": "done"})); closed.Code != http.StatusOK {
			t.Fatalf("%s closes the issue: status=%d body=%s", caller.name, closed.Code, closed.Body.String())
		}
		want := `409 {"code":"ISSUE_CLOSED","error":"issue is closed"}`
		if got := maskedAnswer(answer("patch a closed issue's repo", caller.send(http.MethodPatch, path, map[string]any{"repo": "acme/other"}))); got != want {
			t.Errorf("%s sets a closed issue's repo = %s, want %s", caller.name, got, want)
		}
	}
}
