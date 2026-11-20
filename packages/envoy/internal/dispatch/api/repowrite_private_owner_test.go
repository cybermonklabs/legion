package api

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// repositoryPrivateRefusal is the whole answer to a write that makes a repository a private project
// the writer does not see owns an issue's own or a project's default (refuseRepoHeld).
const repositoryPrivateRefusal = `{"code":"REPOSITORY_PRIVATE","error":"a private project the caller does not see owns this repository; only a person who sees that project may make it an issue's repo or a project's default repository"}`

// seedRepoWrite is the private seed with a repository PRIV owns for each way an issue makes one its
// project's, each written by carol, who sees PRIV, through the routes: PRIV-1 names acme/priv;
// PRIV-2 names acme/privdone and is done; PRIV-3 names none and resolves to PRIV's default,
// acme/privdefault. The server settles no document while a test runs, so no write but the test's
// own adds a row the test counts.
func seedRepoWrite(t *testing.T, options testServerOptions) privateSeed {
	t.Helper()
	options.settle = time.Hour
	seed := seedPrivateProjectWith(t, options)
	private := "/api/v1/issues/" + seed.issue[seedPrivate]
	seed.mustRequest(t, callerCarol, http.MethodPatch, private, map[string]any{"repo": "acme/priv"}, http.StatusOK)
	done := decodeBody[model.Issue](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": seedPrivate, "title": "Finished in its own repository", "repo": "acme/privdone", "force": true}, http.StatusCreated))
	seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+done.Key, map[string]any{"status": "done"}, http.StatusOK)
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/settings/repo-projects/acme/privdefault",
		map[string]any{"project": seedPrivate, "default": true}, http.StatusOK)
	inherits := decodeBody[model.Issue](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": seedPrivate, "title": "Works in its project's default", "force": true}, http.StatusCreated))
	if repoOf(inherits.ResolvedRepo) != "acme/privdefault" {
		t.Fatalf("%s resolves to %s, want PRIV's default acme/privdefault", inherits.Key, repoOf(inherits.ResolvedRepo))
	}
	return seed
}

// sendAs makes one write as caller, the body naming the session a token acts for, as a token's
// write must.
func (s privateSeed) sendAs(t *testing.T, caller privateCaller, method, target string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	if caller.login() == "" {
		withActor := map[string]any{"actor": sessionActor()}
		maps.Copy(withActor, body)
		body = withActor
	}
	return s.request(t, caller, method, target, body)
}

// repoWriteSnapshot is issueWritesSnapshot with every repository mapping, its project and whether
// it is that project's default: everything a refused issue or mapping write could have written.
func repoWriteSnapshot(t *testing.T, database *store.Store) string {
	t.Helper()
	var mappings string
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(string_agg(concat_ws(',', repo, project, is_default), '; ' order by repo), '') from repo_projects
	`).Scan(&mappings); err != nil {
		t.Fatalf("read the repository mappings: %v", err)
	}
	return issueWritesSnapshot(t, database) + " | " + mappings
}

// TestAWriterCannotNameARepositoryAPrivateProjectTheyDoNotSeeOwns: alice, a full member who sees
// PUB and not private PRIV, and carol's token that lists no project, which sees PUB alone, cannot
// make a repository PRIV owns PUB's: a create of a PUB issue naming it, a patch of PUB-1 naming
// it and a PUT that maps it as PUB's default are each 403 REPOSITORY_PRIVATE, the same bytes for
// every spelling and every way PRIV owns it (an issue's own repository, a done issue's, PRIV's
// default), naming no project and no issue, and nothing is written: no issue, no repository, no
// event, no mapping. The GitHub proxy still hides each one from alice. carol, who sees PRIV, makes
// the same three writes; alice names acme/pub, which no private project owns, and clears PUB-1's
// repository.
func TestAWriterCannotNameARepositoryAPrivateProjectTheyDoNotSeeOwns(t *testing.T) {
	seed := seedRepoWrite(t, testServerOptions{})
	public := "/api/v1/issues/" + seed.issue[seedPublic]
	for _, named := range []string{seedPublic, seedPrivate, seed.issue[seedPublic], seed.issue[seedPrivate]} {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(named) + `\b`).MatchString(repositoryPrivateRefusal) {
			t.Fatalf("the refusal names %s", named)
		}
	}
	held := []string{"acme/priv", "ACME/Priv", "acme/privdone", "acme/privdefault"}
	for _, caller := range []privateCaller{callerAlice, callerCarolToken} {
		for _, repository := range held {
			for _, write := range []struct {
				name, method, target string
				body                 map[string]any
			}{
				{"create a PUB issue", http.MethodPost, "/api/v1/issues",
					map[string]any{"project": seedPublic, "title": "Works in " + repository, "repo": repository, "force": true}},
				{"patch PUB-1", http.MethodPatch, public, map[string]any{"repo": repository}},
				{"map it as PUB's default", http.MethodPut, "/api/v1/settings/repo-projects/" + repository,
					map[string]any{"project": seedPublic, "default": true}},
			} {
				before := repoWriteSnapshot(t, seed.database)
				response := seed.sendAs(t, caller, write.method, write.target, write.body)
				if got := strings.TrimSpace(response.Body.String()); response.Code != http.StatusForbidden || got != repositoryPrivateRefusal {
					t.Errorf("%s: %s naming %s = %d %s, want 403 %s", caller, write.name, repository, response.Code, got, repositoryPrivateRefusal)
				}
				if after := repoWriteSnapshot(t, seed.database); after != before {
					t.Errorf("%s: the refused %s naming %s wrote:\nbefore %s\nafter  %s", caller, write.name, repository, before, after)
				}
			}
		}
	}
	for _, repository := range held {
		if proxyShows(t, seed.database, seedAlice, repository) {
			t.Errorf("the GitHub proxy shows alice %s, which only PRIV owns", repository)
		}
	}
	// The instrument's own control: the same read shows carol, who sees PRIV, what it hides from
	// alice, so the false above is the clause's answer, not a read that shows nothing.
	if !proxyShows(t, seed.database, seedCarol, "acme/priv") {
		t.Fatalf("the GitHub proxy hides acme/priv from carol, who sees PRIV")
	}

	// A repository no private project owns is alice's to name, and clearing one is never refused.
	for _, write := range []struct {
		name, method, target string
		body                 map[string]any
		status               int
		repo                 string
	}{
		{"patch PUB-1 to acme/pub", http.MethodPatch, public, map[string]any{"repo": "acme/pub"}, http.StatusOK, "acme/pub"},
		{"clear PUB-1's repo", http.MethodPatch, public, map[string]any{"repo": nil}, http.StatusOK, "<none>"},
		{"create a PUB issue naming acme/pub", http.MethodPost, "/api/v1/issues",
			map[string]any{"project": seedPublic, "title": "Works in the public repository", "repo": "acme/pub", "force": true}, http.StatusCreated, "acme/pub"},
	} {
		response := seed.request(t, callerAlice, write.method, write.target, write.body)
		if response.Code != write.status || repoOf(decodeBody[model.Issue](t, response).Repo) != write.repo {
			t.Errorf("alice: %s = %d %s, want %d with repo %s", write.name, response.Code, response.Body.String(), write.status, write.repo)
		}
	}
	if mapped := seed.request(t, callerAlice, http.MethodPut, "/api/v1/settings/repo-projects/acme/pub",
		map[string]any{"project": seedPublic, "default": true}); mapped.Code != http.StatusOK {
		t.Errorf("alice maps acme/pub as PUB's default = %d %s, want 200", mapped.Code, mapped.Body.String())
	}

	// carol sees PRIV, so a repository it owns is hers to give PUB too.
	created := seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": seedPublic, "title": "Shares the private repository", "repo": "ACME/Priv", "force": true})
	if created.Code != http.StatusCreated || repoOf(decodeBody[model.Issue](t, created).Repo) != "acme/priv" {
		t.Errorf("carol creates a PUB issue naming ACME/Priv = %d %s, want 201 with acme/priv", created.Code, created.Body.String())
	}
	patched := seed.request(t, callerCarol, http.MethodPatch, public, map[string]any{"repo": "acme/privdone"})
	if patched.Code != http.StatusOK || repoOf(decodeBody[model.Issue](t, patched).Repo) != "acme/privdone" {
		t.Errorf("carol patches PUB-1 to acme/privdone = %d %s, want 200 with acme/privdone", patched.Code, patched.Body.String())
	}
	if mapped := seed.request(t, callerCarol, http.MethodPut, "/api/v1/settings/repo-projects/acme/priv",
		map[string]any{"project": seedPublic, "default": true}); mapped.Code != http.StatusOK {
		t.Errorf("carol maps acme/priv as PUB's default = %d %s, want 200", mapped.Code, mapped.Body.String())
	}
}

// TestARepositoryWriteAtAHiddenObjectAnswersAsAtAMissingOne: the repository check runs after the
// write reads its issue and its project as the writer reads them, so alice's patch of PRIV-1 and
// her create in PRIV, each naming acme/priv, which PRIV owns, answer exactly as the same write at
// an issue and a project nobody holds, the key substituted.
func TestARepositoryWriteAtAHiddenObjectAnswersAsAtAMissingOne(t *testing.T) {
	seed := seedRepoWrite(t, testServerOptions{})
	hiddenIssue, missingIssue := seed.issue[seedPrivate], seedPrivate+"-999"
	for _, write := range []struct {
		name, method         string
		hidden, missing      string
		hiddenBody, nameless map[string]any
		hiddenKey, missKey   string
	}{
		{"patch", http.MethodPatch, "/api/v1/issues/" + hiddenIssue, "/api/v1/issues/" + missingIssue,
			map[string]any{"repo": "acme/priv"}, map[string]any{"repo": "acme/priv"}, hiddenIssue, missingIssue},
		{"create", http.MethodPost, "/api/v1/issues", "/api/v1/issues",
			map[string]any{"project": seedPrivate, "title": "Inside the private project", "repo": "acme/priv", "force": true},
			map[string]any{"project": "NOPE", "title": "Inside the private project", "repo": "acme/priv", "force": true}, seedPrivate, "NOPE"},
	} {
		before := repoWriteSnapshot(t, seed.database)
		hidden := seed.request(t, callerAlice, write.method, write.hidden, write.hiddenBody)
		missing := seed.request(t, callerAlice, write.method, write.missing, write.nameless)
		hiddenBody := strings.ReplaceAll(strings.TrimSpace(hidden.Body.String()), write.hiddenKey, write.missKey)
		if hidden.Code != missing.Code || hiddenBody != strings.TrimSpace(missing.Body.String()) || hidden.Code == http.StatusForbidden {
			t.Errorf("alice's %s naming acme/priv at a hidden object = %d %s, at a missing one %d %s: want one answer, not the repository's refusal",
				write.name, hidden.Code, hidden.Body.String(), missing.Code, missing.Body.String())
		}
		if after := repoWriteSnapshot(t, seed.database); after != before {
			t.Errorf("alice's refused %s wrote:\nbefore %s\nafter  %s", write.name, before, after)
		}
	}
}

// TestRepositoryWritesAnswerAsBeforeWithNoPrivateProject (R9): once PRIV is public again, the
// deployment holds no private project, and alice's create and patch naming acme/priv, her PUT that
// maps it as PUB's default, and her token that lists PRIV, which is not open, creating and
// patching PRIV's issues with a repository, are each taken and make no statement of the
// repository check (no statement names access.HiddenPrivateProjectClause). Each answer is logged
// masked (maskedAnswer), so two builds' runs compare byte for byte.
func TestRepositoryWritesAnswerAsBeforeWithNoPrivateProject(t *testing.T) {
	recorder := &statementRecorder{}
	seed := seedRepoWrite(t, testServerOptions{tracer: recorder})
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	for _, write := range []struct {
		name         string
		caller       privateCaller
		method, path string
		body         map[string]any
		status       int
	}{
		{"alice creates a PUB issue naming acme/priv", callerAlice, http.MethodPost, "/api/v1/issues",
			map[string]any{"project": seedPublic, "title": "Works in the formerly private repository", "repo": "acme/priv", "force": true}, http.StatusCreated},
		{"alice patches PUB-1 to acme/priv", callerAlice, http.MethodPatch, "/api/v1/issues/" + seed.issue[seedPublic],
			map[string]any{"repo": "acme/priv"}, http.StatusOK},
		{"alice maps acme/priv as PUB's default", callerAlice, http.MethodPut, "/api/v1/settings/repo-projects/acme/priv",
			map[string]any{"project": seedPublic, "default": true}, http.StatusOK},
		{"alice's token that lists PRIV creates a PRIV issue naming acme/privdone", callerAlicePrivToken, http.MethodPost, "/api/v1/issues",
			map[string]any{"project": seedPrivate, "title": "Filed through a listing token", "repo": "acme/privdone", "force": true}, http.StatusCreated},
		{"alice's token that lists PRIV patches PRIV-1 to acme/privdefault", callerAlicePrivToken, http.MethodPatch, "/api/v1/issues/" + seed.issue[seedPrivate],
			map[string]any{"repo": "acme/privdefault"}, http.StatusOK},
	} {
		var response *httptest.ResponseRecorder
		statements := recorder.record(func() {
			body := write.body
			if write.caller.login() == "" {
				body = map[string]any{"actor": sessionActor()}
				maps.Copy(body, write.body)
			}
			response = seed.requestIn(t, context.WithValue(context.Background(), recordingKey{}, true), write.caller, write.method, write.path, body)
		})
		t.Logf("R9 %s: %s", write.name, maskedAnswer(response))
		if response.Code != write.status {
			t.Errorf("%s = %d %s, want %d", write.name, response.Code, response.Body.String(), write.status)
		}
		if len(statements) == 0 {
			t.Fatalf("%s recorded no statement, so the record cannot show the check's", write.name)
		}
		for _, statement := range statements {
			if strings.Contains(statement.SQL, access.Marker+"HiddenPrivateProjectClause") {
				t.Errorf("%s, with no private project, made a statement of the repository check:\n%s", write.name, oneLine(statement.SQL))
			}
		}
	}
}
