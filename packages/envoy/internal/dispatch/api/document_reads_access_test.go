package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// documentObjects are the objects of one project a document read names: the project, its issue,
// a document of the project and one of the issue, each document with its slug. The missing
// objects are names nobody holds, each spelled as a real one is, so the answer for a hidden
// object and the answer for a missing one differ by the name alone.
type documentObjects struct {
	project, issue                   string
	document, documentSlug           string
	issueDocument, issueDocumentSlug string
}

// names are the values a route path carries, longest first, so a normalization that replaces
// one never replaces a part of another (an issue key holds its project's key).
func (objects documentObjects) names() []string {
	return []string{objects.document, objects.issueDocument, objects.issue, objects.project}
}

// documentRead is one read of the routes this task binds, addressed at one project's objects.
type documentRead struct {
	method string
	path   func(documentObjects) string
	body   any
}

// documentReads is every read of a document, a project and its related items that names its
// object in its path, as each route is mounted.
var documentReads = []documentRead{
	{http.MethodGet, func(o documentObjects) string { return "/api/v1/artifacts/" + o.document }, nil},
	{http.MethodGet, func(o documentObjects) string { return "/api/v1/artifacts/" + o.issueDocument }, nil},
	{http.MethodGet, func(o documentObjects) string { return "/api/v1/artifacts/" + o.document + "/text" }, nil},
	{http.MethodGet, func(o documentObjects) string { return "/api/v1/artifacts/" + o.document + "/text?blocks=true" }, nil},
	{http.MethodGet, func(o documentObjects) string { return "/api/v1/artifacts/" + o.document + "/blocks" }, nil},
	// A block id written short is matched among the blocks of the document the path names, so
	// a hidden document must name no blocks to match among.
	{http.MethodGet, func(o documentObjects) string { return "/api/v1/artifacts/" + o.document + "/blocks/abcdef12" }, nil},
	{http.MethodGet, func(o documentObjects) string { return "/api/v1/artifacts/" + o.document + "/versions/1" }, nil},
	// What changed between two versions is read from both versions' stored text, so a hidden
	// document must answer as missing before either version is read.
	{http.MethodGet, func(o documentObjects) string { return "/api/v1/artifacts/" + o.document + "/changes?from=1&to=2" }, nil},
	{http.MethodGet, func(o documentObjects) string { return "/api/v1/artifacts/" + o.document + "/reviews" }, nil},
	{http.MethodGet, func(o documentObjects) string { return "/api/v1/artifacts/" + o.document + "/asks" }, nil},
	{http.MethodGet, func(o documentObjects) string { return "/api/v1/artifacts/" + o.document + "/comments" }, nil},
	{http.MethodGet, func(o documentObjects) string { return "/api/v1/artifacts/" + o.document + "/events" }, nil},
	// An issue's document read through a project document's routes is refused naming its issue,
	// which a caller that does not see the issue must not learn.
	{http.MethodGet, func(o documentObjects) string { return "/api/v1/artifacts/" + o.issueDocument + "/asks" }, nil},
	{http.MethodGet, func(o documentObjects) string { return "/api/v1/projects/" + o.project + "/artifacts" }, nil},
	{http.MethodGet, func(o documentObjects) string {
		return "/api/v1/projects/" + o.project + "/artifacts/" + o.documentSlug
	}, nil},
	{http.MethodGet, func(o documentObjects) string {
		return "/api/v1/projects/" + o.project + "/artifacts/" + o.documentSlug + "/text"
	}, nil},
	{http.MethodGet, func(o documentObjects) string {
		return "/api/v1/projects/" + o.project + "/artifacts/" + o.documentSlug + "/blocks"
	}, nil},
	{http.MethodGet, func(o documentObjects) string {
		return "/api/v1/projects/" + o.project + "/artifacts/" + o.documentSlug + "/versions/1"
	}, nil},
	{http.MethodGet, func(o documentObjects) string {
		return "/api/v1/projects/" + o.project + "/artifacts/" + o.documentSlug + "/summary"
	}, nil},
	{http.MethodGet, func(o documentObjects) string { return "/api/v1/issues/" + o.issue + "/artifacts" }, nil},
	{http.MethodGet, func(o documentObjects) string {
		return "/api/v1/issues/" + o.issue + "/artifacts/" + o.issueDocumentSlug
	}, nil},
	{http.MethodGet, func(o documentObjects) string { return "/api/v1/projects/" + o.project + "/architecture-sources" }, nil},
	{http.MethodGet, func(o documentObjects) string { return "/api/v1/projects/" + o.project + "/architecture-source" }, nil},
	{http.MethodGet, func(o documentObjects) string { return "/api/v1/projects/" + o.project + "/architecture" }, nil},
	{http.MethodGet, func(o documentObjects) string {
		return "/api/v1/projects/" + o.project + "/related?" + url.Values{"title": {"Runbook steps"}}.Encode()
	}, nil},
	{http.MethodPost, func(o documentObjects) string { return "/api/v1/projects/" + o.project + "/related" },
		map[string]any{"title": "Runbook steps"}},
	{http.MethodGet, func(o documentObjects) string { return "/api/v1/related-checks?project=" + o.project }, nil},
}

// TestADocumentOfAPrivateProjectAnswersAsMissing: every read of a document, of a project and of
// its related items answers a caller that does not see the project exactly as it answers an id or
// a key nobody holds, status and bytes, and answers a caller that sees it with the object; the
// lists hold only what the caller sees; and a write's related-item check offers nothing of a
// project its writer does not see. Each caller of verification v16's seed reads it.
func TestADocumentOfAPrivateProjectAnswersAsMissing(t *testing.T) {
	t.Parallel()
	embedder := newCrossProjectEmbedder()
	for _, text := range crossProjectQueryTexts() {
		embedder.vectors[text] = queryVector()
	}
	github := &fakeGitHubApp{installations: map[string]string{}, repos: map[string]*fakeArchitectureRepo{}}
	for _, project := range []string{seedPublic, seedPrivate} {
		github.installations[architectureRepo(project)] = "read"
		github.repos[architectureRepo(project)] = &fakeArchitectureRepo{commit: shaOne, files: archFiles()}
	}
	seed := seedPrivateProjectWith(t, testServerOptions{
		embedder: embedder, app: fakeGitHubAppConfig(t),
		githubAPIBase: githubtest.Server(t, githubtest.Spec{Handle: github.handler(t).ServeHTTP}),
	})
	objects := map[string]documentObjects{}
	for _, project := range []string{seedPublic, seedPrivate} {
		objects[project] = seedDocumentObjects(t, seed, project)
	}
	missing := documentObjects{
		project: "MISS", issue: "MISS-1",
		document: "0f0f0f0f-0000-4000-8000-0000000000a1", documentSlug: objects[seedPublic].documentSlug,
		issueDocument: "0f0f0f0f-0000-4000-8000-0000000000a2", issueDocumentSlug: objects[seedPublic].issueDocumentSlug,
	}
	offered := seedPrivateAnsweredQuestion(t, seed, embedder)

	for _, caller := range privateCallers {
		t.Run(string(caller), func(t *testing.T) {
			sees := caller.sees()
			// What a missing object answers does not turn on the project read beside it, so each
			// read of one is made once.
			absents := make([]*httptest.ResponseRecorder, len(documentReads))
			for index, read := range documentReads {
				absents[index] = seed.request(t, caller, read.method, read.path(missing), read.body)
			}
			for _, project := range []string{seedPublic, seedPrivate} {
				for index, read := range documentReads {
					hidden := seed.request(t, caller, read.method, read.path(objects[project]), read.body)
					absent := absents[index]
					route := read.method + " " + read.path(objects[project])
					same := hidden.Code == absent.Code &&
						normalizedBody(hidden, objects[project]) == normalizedBody(absent, missing)
					switch {
					case hidden.Code >= http.StatusInternalServerError:
						t.Errorf("%s: %s answers %d %s, the server's error", caller, route, hidden.Code, hidden.Body.String())
					case slices.Contains(sees, project) && same:
						t.Errorf("%s: %s answers %d %s, as for a missing one; %s sees %s", caller, route,
							hidden.Code, hidden.Body.String(), caller, project)
					case !slices.Contains(sees, project) && !same:
						t.Errorf("%s: %s answers %d %s, and %s answers %d %s; %s does not see %s, which must answer as missing",
							caller, route, hidden.Code, hidden.Body.String(), read.path(missing), absent.Code, absent.Body.String(),
							caller, project)
					}
				}
			}
			requireDocumentListsSee(t, seed, caller, sees, objects)
		})
	}

	// An exclude_issue of a project the caller does not see is refused as an issue nobody holds,
	// not as an issue of another project.
	excluded := func(caller privateCaller, issue string) *httptest.ResponseRecorder {
		return seed.request(t, caller, http.MethodGet, "/api/v1/projects/"+seedPublic+"/related?"+url.Values{
			"title": {"Runbook steps"}, "exclude_issue": {issue},
		}.Encode(), nil)
	}
	hidden, absent := excluded(callerAlice, objects[seedPrivate].issue), excluded(callerAlice, missing.issue)
	if hidden.Code != absent.Code || normalizedBody(hidden, objects[seedPrivate]) != normalizedBody(absent, missing) {
		t.Errorf("alice: exclude_issue %s answers %d %s, and %s answers %d %s; want them alike", objects[seedPrivate].issue,
			hidden.Code, hidden.Body.String(), missing.issue, absent.Code, absent.Body.String())
	}
	if seen := excluded(callerCarol, objects[seedPrivate].issue); seen.Code != http.StatusBadRequest ||
		!strings.Contains(seen.Body.String(), "is not an issue of project "+seedPublic) {
		t.Errorf("carol: exclude_issue %s answers %d %s, want it refused as an issue of another project",
			objects[seedPrivate].issue, seen.Code, seen.Body.String())
	}

	// A write that asks a question is offered the answered questions of the other projects its
	// writer sees, and nothing of one it does not: carol's question on PUB is offered PRIV's answer,
	// alice's, the same words on the same issue, nothing of PRIV.
	for _, want := range []struct {
		caller  privateCaller
		offered []string
	}{{callerCarol, []string{offered}}, {callerAlice, []string{}}} {
		created := crossAdvice(t, string(want.caller)+"'s ask", seed.request(t, want.caller, http.MethodPost,
			"/api/v1/issues/"+objects[seedPublic].issue+"/asks", map[string]any{"question": crossProjectQuestion}))
		if ids := crossProjectItemIDs(created.OtherProjects); !slices.Equal(ids, want.offered) {
			t.Errorf("%s's question on %s was offered %v of the other projects, want %v", want.caller, seedPublic, ids, want.offered)
		}
		posted := crossRouteSuggestions(t, string(want.caller)+"'s related check", seed.request(t, want.caller, http.MethodPost,
			"/api/v1/projects/"+seedPublic+"/related", map[string]any{"title": crossProjectQuestion}))
		if ids := crossProjectItemIDs(posted.OtherProjects); !slices.Equal(ids, want.offered) {
			t.Errorf("%s's related check on %s offered %v of the other projects, want %v", want.caller, seedPublic, ids, want.offered)
		}
	}

	// A run of a going answer is read by the credential that asked it only while that credential
	// sees the project it was asked of: bob asks of PRIV and reads his run's stream, and once
	// carol takes him off PRIV's list his run answers exactly as a run nobody asked. Last, since
	// it takes PRIV from bob.
	run := decodeBody[struct {
		ID string `json:"id"`
	}](t, seed.mustRequest(t, callerBob, http.MethodPost, "/api/v1/going/requests",
		map[string]any{"words": "What do the runbook steps say?", "project": seedPrivate}, http.StatusCreated))
	stream := func(id string) *httptest.ResponseRecorder {
		return seed.request(t, callerBob, http.MethodGet, "/api/v1/going/requests/"+id+"/stream", nil)
	}
	if read := stream(run.ID); read.Code != http.StatusOK {
		t.Fatalf("bob: the stream of his own PRIV run answers %d %s, want 200", read.Code, read.Body.String())
	}
	seed.mustRequest(t, callerCarol, http.MethodDelete, "/api/v1/projects/"+seedPrivate+"/access/people/"+seedBob, nil, http.StatusOK)
	hiddenRun, absentRun := stream(run.ID), stream("0f0f0f0f-0000-4000-8000-0000000000a3")
	if hiddenRun.Code != absentRun.Code || hiddenRun.Body.String() != absentRun.Body.String() {
		t.Errorf("bob, off PRIV's list: GET /api/v1/going/requests/{id}/stream of his PRIV run answers %d %s, and a run nobody asked %d %s; want them alike",
			hiddenRun.Code, hiddenRun.Body.String(), absentRun.Code, absentRun.Body.String())
	}
}

// requireDocumentListsSee holds every list this task binds to what caller sees: the projects, with
// a private one marked so and a public one carrying no visibility at all; the default assignees,
// to a caller holding the person scope; the repository mappings; the architecture sources; and the
// related-check report's count.
func requireDocumentListsSee(t *testing.T, seed privateSeed, caller privateCaller, sees []string, objects map[string]documentObjects) {
	t.Helper()
	listed := seed.mustRequest(t, caller, http.MethodGet, "/api/v1/projects", nil, http.StatusOK)
	var projects []map[string]json.RawMessage
	if err := json.Unmarshal(listed.Body.Bytes(), &projects); err != nil {
		t.Fatalf("%s: decode the project list: %v", caller, err)
	}
	keys := []string{}
	for _, project := range projects {
		var key string
		if err := json.Unmarshal(project["key"], &key); err != nil {
			t.Fatalf("%s: decode a project's key: %v", caller, err)
		}
		keys = append(keys, key)
		visibility, marked := project["visibility"]
		switch {
		case key == seedPrivate && string(visibility) != `"`+model.ProjectPrivate+`"`:
			t.Errorf("%s: GET /api/v1/projects lists %s with visibility %s, want %q", caller, key, visibility, model.ProjectPrivate)
		case key != seedPrivate && marked:
			t.Errorf("%s: GET /api/v1/projects lists the public %s with visibility %s, want none", caller, key, visibility)
		}
	}
	requireSameKeys(t, caller, "GET /api/v1/projects", keys, sees)

	if caller.login() != "" {
		defaults := decodeBody[[]struct {
			Project string `json:"project"`
		}](t, seed.mustRequest(t, caller, http.MethodGet, "/api/v1/settings/project-default-assignees", nil, http.StatusOK))
		keys := []string{}
		for _, entry := range defaults {
			keys = append(keys, entry.Project)
		}
		requireSameKeys(t, caller, "GET /api/v1/settings/project-default-assignees", keys, sees)
	}

	mappings := decodeBody[[]model.RepoProject](t, seed.mustRequest(t, caller, http.MethodGet, "/api/v1/settings/repo-projects", nil, http.StatusOK))
	keys = []string{}
	for _, mapping := range mappings {
		keys = append(keys, mapping.Project)
	}
	requireSameKeys(t, caller, "GET /api/v1/settings/repo-projects", keys, sees)

	sources := decodeBody[[]model.ArchitectureSource](t, seed.mustRequest(t, caller, http.MethodGet, "/api/v1/settings/architecture-sources", nil, http.StatusOK))
	keys = []string{}
	for _, source := range sources {
		keys = append(keys, source.Project)
	}
	requireSameKeys(t, caller, "GET /api/v1/settings/architecture-sources", keys, sees)

	want := 0
	for _, project := range sees {
		var checks int
		if err := seed.database.Pool.QueryRow(context.Background(),
			`select count(*) from related_checks where project_key = $1`, objects[project].project).Scan(&checks); err != nil {
			t.Fatalf("count %s's related checks: %v", project, err)
		}
		want += checks
	}
	report := decodeBody[model.RelatedChecksReport](t, seed.mustRequest(t, caller, http.MethodGet, "/api/v1/related-checks", nil, http.StatusOK))
	if report.Total != want {
		t.Errorf("%s: GET /api/v1/related-checks counts %d checks, want %d, those of %v", caller, report.Total, want, sees)
	}
}

// requireSameKeys fails unless a list names exactly the projects caller sees.
func requireSameKeys(t *testing.T, caller privateCaller, route string, keys, sees []string) {
	t.Helper()
	slices.Sort(keys)
	keys = slices.Compact(keys)
	if !slices.Equal(keys, sees) {
		t.Errorf("%s: %s lists the projects %v, want %v", caller, route, keys, sees)
	}
}

// normalizedBody is a response's bytes with each of objects' names written as one placeholder, so
// the answer for one object and the answer for another compare by everything but the name.
func normalizedBody(response *httptest.ResponseRecorder, objects documentObjects) string {
	body := response.Body.String()
	for _, name := range objects.names() {
		body = strings.ReplaceAll(body, name, "<name>")
	}
	return body
}

// seedDocumentObjects writes, as carol, who owns both projects, what project's document reads
// read: a document of the project and one of its issue, and an architecture source, each through
// its route, and a repository mapping as the last release left one (storeRepoMapping), which no
// route writes now.
func seedDocumentObjects(t *testing.T, seed privateSeed, project string) documentObjects {
	t.Helper()
	objects := documentObjects{project: project, issue: seed.issue[project]}
	upload := func(target string) (id, slug string) {
		uploaded := decodeBody[artifactUploadResponse](t, seed.mustRequest(t, callerCarol, http.MethodPost, target,
			map[string]any{"name": "Runbook.md", "content": "# Runbook\n\nThe runbook steps.\n"}, http.StatusCreated))
		return uploaded.Artifact.ID, uploaded.Artifact.Slug
	}
	objects.document, objects.documentSlug = upload("/api/v1/projects/" + project + "/artifacts")
	objects.issueDocument, objects.issueDocumentSlug = upload("/api/v1/issues/" + objects.issue + "/artifacts")
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+project+"/architecture-sources/"+architectureRepo(project),
		map[string]string{"branch": "main"}, http.StatusOK)
	storeRepoMapping(t, seed.database, "acme/"+strings.ToLower(project)+"-app", project, false)
	return objects
}

// architectureRepo is the repository project's architecture source is read from, which the fake
// GitHub App the test server reads holds.
func architectureRepo(project string) string {
	return "acme/" + strings.ToLower(project) + "-arch"
}

// seedPrivateAnsweredQuestion writes, as carol, an answered question on PRIV's issue whose every
// window stands near crossProjectQuestion, embeds what the seed wrote, and answers the question's
// id: what a related-item check offers a question write of another project.
func seedPrivateAnsweredQuestion(t *testing.T, seed privateSeed, embedder *crossProjectEmbedder) string {
	t.Helper()
	asked := decodeBody[model.Ask](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPrivate]+"/asks",
		map[string]any{
			"question": "When the backlog grows, which store do we empty first?",
			"options":  []map[string]string{{"label": "Yes"}, {"label": "No"}},
		}, http.StatusCreated))
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/asks/"+asked.ID+"/answer",
		map[string]any{"text": "Empty the page store first; the index store costs more to rebuild.", "expected_edited_at": nil},
		http.StatusOK)
	embedWindowsAs(t, seed.database, embedder.fakeEmbedder, "ask_part", "ask/"+asked.ID, angledVector(0.9, 2))
	refreshTermStats(t, seed.database, seedPublic)
	processAllPending(t, seed.database, embedder)
	return asked.ID
}

// fakeGitHubAppConfig is a GitHub App credential with a key of its own, which a test server whose
// GitHub is a fake (githubtest.Server) signs its App requests with.
func fakeGitHubAppConfig(t *testing.T) *auth.AppConfig {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the App's key: %v", err)
	}
	return &auth.AppConfig{ClientID: "Iv1.test", PEM: string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))}
}
