package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// TestAnArchitectureSourceHoldsItsRepositoryToTheProxysRule: an architecture source imports its
// repository's `.dispatch/architecture` prose into the project, which every reader of the project
// then reads, so a source write and a source sync hold the repository to the GitHub proxy's rule
// for the caller (refs.RepositoryClause, hiddenSourceRefusal). A repository the proxy would not
// show the caller answers, in status and bytes, exactly what acme/notinstalled, which the App is
// not installed on, answers, and GitHub is asked nothing about it.
//
// The App reads acme/internal, which no issue names, acme/priv, which PRIV's issue names, and
// acme/pub, which PUB's issue names. bob, a restricted member PRIV's list names, may write PRIV's
// source of acme/priv and no source of acme/internal; alice, an open viewer PRIV's list does not
// name, may write PUB's source of acme/pub and of acme/internal, and none of acme/priv. A source
// of acme/internal written into PRIV before the rule held its writes is not synced for bob: the
// single sync answers what a source of acme/notinstalled answers, and the project sync answers
// its row as it stood, importing nothing. With PRIV public, alice writes PUB's source of
// acme/priv from GitHub's answer and the write reads no statement of the repository rule (R9).
//
// Control: the source write and sync with no hiddenSourceRefusal. bob's write of acme/internal
// answers 200 and the row fails naming it.
func TestAnArchitectureSourceHoldsItsRepositoryToTheProxysRule(t *testing.T) {
	fake := &fakeGitHubApp{
		installations: map[string]string{"acme/internal": "read", "acme/priv": "read", "acme/pub": "read"},
		repos: map[string]*fakeArchitectureRepo{
			"acme/internal": {commit: shaArch, files: archFiles()},
			"acme/priv":     {commit: shaArch, files: archFiles()},
			"acme/pub":      {commit: shaInfra, files: infraFiles()},
		},
	}
	var mu sync.Mutex
	var asked []string
	served := fake.handler(t)
	trace := &existenceTrace{}
	seed := seedPrivateProjectWith(t, testServerOptions{
		store: storetest.TracedStore(t, storetest.Open(t), trace),
		app:   fakeGitHubAppConfig(t),
		githubAPIBase: githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			asked = append(asked, r.URL.Path)
			mu.Unlock()
			served.ServeHTTP(w, r)
		}}),
	})
	for project, repository := range map[string]string{seedPrivate: "acme/priv", seedPublic: "acme/pub"} {
		if _, err := seed.database.Pool.Exec(context.Background(), `update issues set repo = $2 where key = $1`,
			seed.issue[project], repository); err != nil {
			t.Fatalf("name %s on %s's issue: %v", repository, project, err)
		}
	}
	// askedOf is every GitHub path the requests since mark asked that names repository.
	mark := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(asked)
	}
	askedOf := func(since int, repository string) []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.DeleteFunc(slices.Clone(asked[since:]), func(path string) bool { return !strings.Contains(path, "/"+repository) })
	}
	// answer is caller's answer, its repository written <repo>, and the statements of the
	// repository rule the request read.
	answer := func(caller privateCaller, method, project, repository, suffix string, body any) (string, []string) {
		trace.take()
		response := seed.requestIn(t, context.WithValue(context.Background(), recordingKey{}, true), caller, method,
			"/api/v1/projects/"+project+"/architecture-sources/"+repository+suffix, body)
		rule := slices.DeleteFunc(trace.take(), func(statement string) bool { return !strings.Contains(statement, "as asked(repo)") })
		return strings.ReplaceAll(fmt.Sprintf("%d %s", response.Code, strings.TrimSpace(response.Body.String())), repository, "<repo>"), rule
	}
	put := map[string]string{"branch": "main"}
	uninstalled, _ := answer(callerBob, http.MethodPut, seedPrivate, "acme/notinstalled", "", put)
	if !strings.HasPrefix(uninstalled, `409 {"code":"SOURCE_ACCESS","error":"the GitHub App is not installed on <repo>`) {
		t.Fatalf("bob's source of acme/notinstalled answers %s, want 409 SOURCE_ACCESS: the comparison proves nothing", uninstalled)
	}
	refused := func(caller privateCaller, project, repository string) {
		t.Helper()
		before := mark()
		if got, _ := answer(caller, http.MethodPut, project, repository, "", put); got != uninstalled {
			t.Errorf("%s's source of %s in %s, a repository the proxy does not show them, answers\n\t%s\nwant what acme/notinstalled answers:\n\t%s",
				caller, repository, project, got, uninstalled)
		}
		if paths := askedOf(before, repository); len(paths) != 0 {
			t.Errorf("%s's source of %s asked GitHub %q, want nothing", caller, repository, paths)
		}
	}
	taken := func(caller privateCaller, project, repository string) {
		t.Helper()
		before := mark()
		if got, _ := answer(caller, http.MethodPut, project, repository, "", put); !strings.HasPrefix(got, "200 ") {
			t.Errorf("%s's source of %s in %s answers %s, want 200", caller, repository, project, got)
		}
		if paths := askedOf(before, repository); !slices.Contains(paths, "/repos/"+repository+"/installation") {
			t.Errorf("%s's source of %s asked GitHub %q, want its installation", caller, repository, paths)
		}
	}
	refused(callerBob, seedPrivate, "acme/internal")
	taken(callerBob, seedPrivate, "acme/priv")
	refused(callerAlice, seedPublic, "acme/priv")
	taken(callerAlice, seedPublic, "acme/pub")
	taken(callerAlice, seedPublic, "acme/internal")

	// A source of acme/internal in PRIV and one of acme/notinstalled, written before the rule
	// held source writes.
	for _, repository := range []string{"acme/internal", "acme/notinstalled"} {
		if _, err := seed.database.Pool.Exec(context.Background(), `
			insert into architecture_sources (project_key, repo, branch, enabled, installation_id, created_by)
			values ($1, $2, 'main', true, 77, '{"kind":"user","id":"carol"}'::jsonb)
		`, seedPrivate, repository); err != nil {
			t.Fatalf("store PRIV's source of %s: %v", repository, err)
		}
	}
	uninstalledSync, _ := answer(callerBob, http.MethodPost, seedPrivate, "acme/notinstalled", "/sync", nil)
	if !strings.HasPrefix(uninstalledSync, `409 {"code":"SOURCE_ACCESS","error":"the GitHub App is not installed on <repo>`) {
		t.Fatalf("bob's sync of PRIV's source of acme/notinstalled answers %s, want 409 SOURCE_ACCESS: the comparison proves nothing", uninstalledSync)
	}
	before := mark()
	if got, _ := answer(callerBob, http.MethodPost, seedPrivate, "acme/internal", "/sync", nil); got != uninstalledSync {
		t.Errorf("bob's sync of PRIV's source of acme/internal answers\n\t%s\nwant what acme/notinstalled's answers:\n\t%s", got, uninstalledSync)
	}
	if paths := askedOf(before, "acme/internal"); len(paths) != 0 {
		t.Errorf("bob's sync of acme/internal asked GitHub %q, want nothing", paths)
	}
	before = mark()
	response := seed.request(t, callerBob, http.MethodPost, "/api/v1/projects/"+seedPrivate+"/architecture-sources/sync", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("bob's sync of PRIV's sources = %d %s, want 200", response.Code, response.Body.String())
	}
	for _, source := range decodeStrict[[]architectureSourceResponse](t, response) {
		if source.Repo == "acme/internal" && (source.LastSyncAt != nil || source.LastCommit != nil || source.Components != 0) {
			t.Errorf("bob's sync of PRIV's sources imported acme/internal: %s", source)
		}
	}
	if paths := askedOf(before, "acme/internal"); len(paths) != 0 {
		t.Errorf("bob's sync of PRIV's sources asked GitHub %q about acme/internal, want nothing", paths)
	}

	// With no project private, an open viewer writes from GitHub's answer and reads no statement
	// of the repository rule.
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	before = mark()
	got, rule := answer(callerAlice, http.MethodPut, seedPublic, "acme/priv", "", put)
	if !strings.HasPrefix(got, "200 ") || !slices.Contains(askedOf(before, "acme/priv"), "/repos/acme/priv/installation") || len(rule) != 0 {
		t.Errorf("alice's source of acme/priv with no project private answers %s with %d statements of the repository rule read, want 200 from GitHub's answer and none read",
			got, len(rule))
	}
}

// TestAnArchitectureSourceFollowsNoRenameIntoAHiddenRepository: GitHub answers every read of
// acme/privold with a redirect to acme/privnew, as it answers the old name of a renamed
// repository, and PRIV's issue names acme/privnew. No issue names acme/privold, so the repository
// rule shows it to alice, an open viewer PRIV's list does not name. While PRIV is private every
// GitHub read of a source is held to its repository (the PUT's check, and the importer's reads
// that the syncs, the ticker and the push imports make):
//
//   - alice's PUT of PUB's source of acme/privold answers what her PUT of acme/notinstalled
//     answers, and no source is stored;
//   - a source of acme/privold stored in PUB before the rule is synced by alice as a source of a
//     repository the App is not installed on: the sync answers what acme/notinstalled's does, and
//     imports no commit and no component.
//
// With PRIV public, alice's PUT of acme/privold follows the redirect, as before (R9).
//
// Controls: the PUT's check not held (checkArchitectureSource), under which the PUT answers 200;
// the importer's reads not held (heldToItsRepository), under which the sync imports PRIV's commit.
// Each fails the row.
func TestAnArchitectureSourceFollowsNoRenameIntoAHiddenRepository(t *testing.T) {
	fake := &fakeGitHubApp{
		installations: map[string]string{"acme/privnew": "read"},
		repos:         map[string]*fakeArchitectureRepo{"acme/privnew": {commit: shaArch, files: archFiles()}},
	}
	served := fake.handler(t)
	seed := seedPrivateProjectWith(t, testServerOptions{
		app: fakeGitHubAppConfig(t),
		githubAPIBase: githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
			if rest, moved := strings.CutPrefix(r.URL.Path, "/repos/acme/privold"); moved {
				http.Redirect(w, r, "/repos/acme/privnew"+rest, http.StatusMovedPermanently)
				return
			}
			served.ServeHTTP(w, r)
		}}),
	})
	ctx := context.Background()
	if _, err := seed.database.Pool.Exec(ctx, `update issues set repo = 'acme/privnew' where key = $1`, seed.issue[seedPrivate]); err != nil {
		t.Fatalf("name acme/privnew on PRIV's issue: %v", err)
	}
	answer := func(method, repository, suffix string, body any) string {
		response := seed.request(t, callerAlice, method, "/api/v1/projects/"+seedPublic+"/architecture-sources/"+repository+suffix, body)
		return strings.ReplaceAll(fmt.Sprintf("%d %s", response.Code, strings.TrimSpace(response.Body.String())), repository, "<repo>")
	}
	stored := func(repository string) (count int, commit *string, components int) {
		t.Helper()
		if err := seed.database.Pool.QueryRow(ctx, `
			select count(*), max(last_commit), coalesce(sum((select count(*) from components c
				where c.project_key = s.project_key and c.source_id = s.id)), 0)
			from architecture_sources s where project_key = $1 and repo = $2`, seedPublic, repository).Scan(&count, &commit, &components); err != nil {
			t.Fatalf("read PUB's source of %s: %v", repository, err)
		}
		return count, commit, components
	}
	put := map[string]string{"branch": "main"}
	uninstalled := answer(http.MethodPut, "acme/notinstalled", "", put)
	if !strings.HasPrefix(uninstalled, `409 {"code":"SOURCE_ACCESS","error":"the GitHub App is not installed on <repo>`) {
		t.Fatalf("alice's source of acme/notinstalled answers %s, want 409 SOURCE_ACCESS: the comparison proves nothing", uninstalled)
	}
	if got := answer(http.MethodPut, "acme/privold", "", put); got != uninstalled {
		t.Errorf("alice's source of acme/privold, which GitHub redirects to PRIV's acme/privnew, answers\n\t%s\nwant what acme/notinstalled answers:\n\t%s", got, uninstalled)
	}
	if count, _, _ := stored("acme/privold"); count != 0 {
		t.Errorf("alice's refused PUT stored %d sources of acme/privold, want none", count)
	}

	// Sources of acme/privold and of acme/notinstalled stored in PUB before the rule.
	for _, repository := range []string{"acme/privold", "acme/notinstalled"} {
		if _, err := seed.database.Pool.Exec(ctx, `
			insert into architecture_sources (project_key, repo, branch, enabled, installation_id, created_by)
			values ($1, $2, 'main', true, 77, '{"kind":"user","id":"carol"}'::jsonb)`, seedPublic, repository); err != nil {
			t.Fatalf("store PUB's source of %s: %v", repository, err)
		}
	}
	uninstalledSync := answer(http.MethodPost, "acme/notinstalled", "/sync", nil)
	if !strings.HasPrefix(uninstalledSync, `409 {"code":"SOURCE_ACCESS","error":"the GitHub App is not installed on <repo>`) {
		t.Fatalf("alice's sync of acme/notinstalled answers %s, want 409 SOURCE_ACCESS: the comparison proves nothing", uninstalledSync)
	}
	if got := answer(http.MethodPost, "acme/privold", "/sync", nil); got != uninstalledSync {
		t.Errorf("alice's sync of PUB's source of acme/privold answers\n\t%s\nwant what acme/notinstalled's answers:\n\t%s", got, uninstalledSync)
	}
	if _, commit, components := stored("acme/privold"); commit != nil || components != 0 {
		imported := "none"
		if commit != nil {
			imported = *commit
		}
		t.Errorf("alice's sync of acme/privold imported commit %s and %d components, want none", imported, components)
	}

	// With no project private, the PUT follows GitHub's redirect, as before.
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	if _, err := seed.database.Pool.Exec(ctx, `delete from architecture_sources where project_key = $1`, seedPublic); err != nil {
		t.Fatalf("clear PUB's sources: %v", err)
	}
	if got := answer(http.MethodPut, "acme/privold", "", put); !strings.HasPrefix(got, "200 ") {
		t.Errorf("alice's source of acme/privold with no project private answers %s, want 200 from the redirect", got)
	}
}
