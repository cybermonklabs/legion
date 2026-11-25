package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAProjectsDefaultRepositoryShowsOnlyToItsReaders: a project's default repository is a member
// of the project row, so it reaches a reader only with a project the reader sees. carol, whom
// PRIV's list names, makes acme/privdefault PRIV's default and acme/pubdefault PUB's. alice, a full
// member PRIV's list does not name, lists PUB with acme/pubdefault and no PRIV, and no byte of her
// list names acme/privdefault; carol lists both, PRIV marked private. The project.updated each
// write appends carries the row as its writer reads it: PRIV's says visibility "private", so a
// reader of the event does not take PRIV for public, and PUB's carries no visibility member, as a
// public project's row has none.
//
// Controls: the project list read with no access.ProjectClause (alice then lists PRIV and its
// default), and loadProject reading no visibility (PRIV's event then says nothing of it, which a
// reader of the event takes for public). Each fails the row naming what it shows.
func TestAProjectsDefaultRepositoryShowsOnlyToItsReaders(t *testing.T) {
	seed := seedPrivateProject(t)
	for project, repository := range map[string]string{seedPrivate: "acme/privdefault", seedPublic: "acme/pubdefault"} {
		set := decodeBody[projectDefaultRepository](t, seed.mustRequest(t, callerCarol, http.MethodPut,
			"/api/v1/projects/"+project+"/default-repository", map[string]any{"repo": repository}, http.StatusOK))
		if set.Repo == nil || *set.Repo != repository {
			t.Fatalf("carol's write of %s's default answers %#v, want %s", project, set, repository)
		}
	}

	listed := func(caller privateCaller) (map[string]model.Project, string) {
		t.Helper()
		response := seed.mustRequest(t, caller, http.MethodGet, "/api/v1/projects", nil, http.StatusOK)
		projects := map[string]model.Project{}
		for _, project := range decodeBody[[]model.Project](t, response) {
			projects[project.Key] = project
		}
		return projects, response.Body.String()
	}
	repoOfProject := func(project model.Project) string {
		if project.DefaultRepo == nil {
			return "<none>"
		}
		return *project.DefaultRepo
	}

	alices, body := listed(callerAlice)
	if _, listedPriv := alices[seedPrivate]; listedPriv || strings.Contains(body, "acme/privdefault") {
		t.Errorf("alice's project list shows %s or its default acme/privdefault, which she does not see: %s", seedPrivate, body)
	}
	if pub, ok := alices[seedPublic]; !ok || repoOfProject(pub) != "acme/pubdefault" || pub.Visibility != "" {
		t.Errorf("alice's project list holds %s as %#v, want its default acme/pubdefault and no visibility", seedPublic, pub)
	}
	carols, _ := listed(callerCarol)
	if priv := carols[seedPrivate]; repoOfProject(priv) != "acme/privdefault" || priv.Visibility != model.ProjectPrivate {
		t.Errorf("carol's project list holds %s as %#v, want its default acme/privdefault, marked private", seedPrivate, priv)
	}

	privateEvent := lastProjectUpdate(t, seed.database, seedPrivate)
	if got := string(privateEvent["visibility"]); got != `"private"` {
		t.Errorf("%s's project.updated says visibility %s, want \"private\": %v", seedPrivate, got, privateEvent)
	}
	if got := string(privateEvent["default_repo"]); got != `"acme/privdefault"` {
		t.Errorf("%s's project.updated carries default_repo %s, want \"acme/privdefault\"", seedPrivate, got)
	}
	publicEvent := lastProjectUpdate(t, seed.database, seedPublic)
	if raw, present := publicEvent["visibility"]; present {
		var visibility string
		_ = json.Unmarshal(raw, &visibility)
		t.Errorf("%s's project.updated carries visibility %q; a public project's row carries none", seedPublic, visibility)
	}
}
