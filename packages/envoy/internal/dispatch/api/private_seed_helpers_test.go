package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Test helpers the S3c-documents tests read; the rest of this file is S3c-issues's.

// readerRequest is a request as caller, as request sends one, for a test that calls a server
// method on it directly.
func (s privateSeed) readerRequest(t *testing.T, caller privateCaller) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/issues", nil)
	if login := caller.login(); login != "" {
		request.Header.Set("X-Dispatch-User", login)
	} else {
		request.Header.Set("Authorization", "Bearer "+s.bearer(t, caller))
	}
	return request
}

// documentObjects are the objects of one project a document read names: the project, its issue,
// a document of the project and one of the issue, each document with its slug. The missing
// objects are names nobody holds, each spelled as a real one is, so the answer for a hidden
// object and the answer for a missing one differ by the name alone.
type documentObjects struct {
	project, issue                   string
	document, documentSlug           string
	issueDocument, issueDocumentSlug string
}

// architectureRepo is the repository project's architecture source is read from, which the fake
// GitHub App the test server reads holds.
func architectureRepo(project string) string {
	return "acme/" + strings.ToLower(project) + "-arch"
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
