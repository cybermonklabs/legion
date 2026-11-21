package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAMappingWriteReachesNoMappingOfAProjectItsWriterDoesNotSee: a repository mapping is a
// deployment row a full member writes, and a mapping of a private project's repository decides
// which repository that project's issues resolve to, so a write is held to the projects its writer
// sees (projectSeenBy, access.ProjectClause). carol maps acme/unowned, which no issue names and so
// is no repository the GitHub proxy hides, to PRIV, and acme/priv, which PRIV's issue names, as
// PRIV's default. alice, an open viewer PRIV's list does not name:
//
//   - a PUT moving PRIV's mapping of acme/unowned to PUB answers exactly what a PUT to a project
//     nobody holds answers, and the mapping stays PRIV's;
//   - a PUT of acme/pub to PRIV, a project she does not see, answers the same;
//   - a DELETE of PRIV's mapping of acme/unowned answers what a DELETE of a repository no mapping
//     holds answers (204), and the mapping stays;
//   - a PUT moving PRIV's default mapping of acme/priv answers what making acme/priv a default
//     answers where no mapping holds it, 403 REPOSITORY_NOT_AVAILABLE (refuseRepoHidden, asked of
//     the repository first), and the mapping stays.
//
// carol, whom PRIV's list names, removes the mapping. With PRIV public, alice moves a mapping of
// PRIV's, as before any project could be private (R9).
//
// Control: projectSeenBy answering true. alice's move of acme/unowned then answers 200 and her
// removal removes it, and the test fails naming each.
func TestAMappingWriteReachesNoMappingOfAProjectItsWriterDoesNotSee(t *testing.T) {
	seed := seedPrivateProject(t)
	ctx := context.Background()
	answer := func(caller privateCaller, method, repository string, body any) string {
		response := seed.request(t, caller, method, "/api/v1/settings/repo-projects/"+repository, body)
		return fmt.Sprintf("%d %s", response.Code, strings.TrimSpace(response.Body.String()))
	}
	mapping := func(repository string) string {
		var project string
		var isDefault bool
		err := seed.database.Pool.QueryRow(ctx, `select project, is_default from repo_projects where repo = $1`, repository).Scan(&project, &isDefault)
		if err != nil {
			return "none"
		}
		return fmt.Sprintf("%s default=%v", project, isDefault)
	}
	if _, err := seed.database.Pool.Exec(ctx, `update issues set repo = 'acme/priv' where key = $1`, seed.issue[seedPrivate]); err != nil {
		t.Fatalf("name acme/priv on PRIV's issue: %v", err)
	}
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/settings/repo-projects/acme/unowned",
		map[string]any{"project": seedPrivate}, http.StatusOK)
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/settings/repo-projects/acme/priv",
		map[string]any{"project": seedPrivate, "default": true}, http.StatusOK)

	missing := answer(callerAlice, http.MethodPut, "acme/other", map[string]any{"project": "NOPE"})
	if !strings.HasPrefix(missing, "404 ") {
		t.Fatalf("alice's mapping to NOPE, a project nobody holds, answers %s, want 404: the comparison proves nothing", missing)
	}
	if got := answer(callerAlice, http.MethodPut, "acme/unowned", map[string]any{"project": seedPublic}); got != missing {
		t.Errorf("alice's move of PRIV's mapping of acme/unowned to PUB answers %s, want what a project nobody holds answers: %s", got, missing)
	}
	if got := mapping("acme/unowned"); got != seedPrivate+" default=false" {
		t.Errorf("after alice's refused move, acme/unowned's mapping is %s, want PRIV's", got)
	}
	if got := answer(callerAlice, http.MethodPut, "acme/pub", map[string]any{"project": seedPrivate}); got != missing {
		t.Errorf("alice's mapping of acme/pub to PRIV, which she does not see, answers %s, want %s", got, missing)
	}
	if got := mapping("acme/pub"); got != "none" {
		t.Errorf("after alice's refused mapping, acme/pub's mapping is %s, want none", got)
	}
	unmapped := answer(callerAlice, http.MethodDelete, "acme/unmapped", nil)
	if got := answer(callerAlice, http.MethodDelete, "acme/unowned", nil); got != unmapped {
		t.Errorf("alice's removal of PRIV's mapping of acme/unowned answers %s, want what a repository no mapping holds answers: %s", got, unmapped)
	}
	if got := mapping("acme/unowned"); got != seedPrivate+" default=false" {
		t.Errorf("after alice's removal, acme/unowned's mapping is %s, want PRIV's still", got)
	}
	if got := answer(callerAlice, http.MethodPut, "acme/priv", map[string]any{"project": seedPublic, "default": true}); !strings.HasPrefix(got, `403 {"code":"REPOSITORY_NOT_AVAILABLE"`) {
		t.Errorf("alice's move of PRIV's default mapping of acme/priv answers %s, want 403 REPOSITORY_NOT_AVAILABLE", got)
	}
	if got := mapping("acme/priv"); got != seedPrivate+" default=true" {
		t.Errorf("after alice's refused move, acme/priv's mapping is %s, want PRIV's default", got)
	}

	// carol sees PRIV and removes its mapping.
	if got := answer(callerCarol, http.MethodDelete, "acme/unowned", nil); !strings.HasPrefix(got, "204") {
		t.Errorf("carol's removal of PRIV's mapping of acme/unowned answers %s, want 204", got)
	}
	if got := mapping("acme/unowned"); got != "none" {
		t.Errorf("after carol's removal, acme/unowned's mapping is %s, want none", got)
	}

	// With no project private, alice moves a mapping of PRIV's, as before projects could be private.
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/settings/repo-projects/acme/unowned",
		map[string]any{"project": seedPrivate}, http.StatusOK)
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	if got := answer(callerAlice, http.MethodPut, "acme/unowned", map[string]any{"project": seedPublic}); !strings.HasPrefix(got, "200 ") {
		t.Errorf("alice's move of PRIV's mapping with no project private answers %s, want 200", got)
	}
}
