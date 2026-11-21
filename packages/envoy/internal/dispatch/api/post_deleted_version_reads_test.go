package api

// post_deleted_version_reads_test.go holds R3 of dispatch://INFRA-1523 on the reads of a
// document's versions the census found no test of after the delete of an accepted suggestion: the
// slug reads of a document (row 6.3), the document lists (6.4), the issue read (6.5), the one
// version (6.6) and a citing document's outgoing references (6.7). An accept names the version it
// makes after the suggestion's body (acceptedVersionName), so every one of them gives that body
// as a version's name until the delete renames it.
//
// The row numbers in subtest names are the rows of the issue's surface census,
// dispatch://INFRA-1523/artifact/surface-census-md.

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// versionSummaries is every version name an answer carries, wherever its shape holds one.
func versionSummaries(t *testing.T, read string, body []byte) []string {
	t.Helper()
	var summaries []string
	eachAnswerObject(t, read, body, func(_ string, node map[string]any) {
		if summary, ok := node["summary"].(string); ok {
			summaries = append(summaries, summary)
		}
	})
	return summaries
}

// Every read of a document's versions, made while a version of it is named after an accepted
// suggestion's body and again after the suggestion's author deletes the suggestion. One
// suggestion is accepted on an issue's spec and one on a project document, which a second project
// document cites. Before the delete each read gives the body as the version's name; after it each
// gives none of the body, and the version under the name an accept gives a suggestion with no
// body.
func TestEveryReadOfADocumentsVersionsNamesTheAcceptedVersionAsTheDeleteRenamedIt(t *testing.T) {
	_, handler, _ := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Versions an accept named", "The quick brown fox")
	ledger := createProjectDocument(t, handler, "TEST", "Ledger", "# Ledger\n\nkilo lima mike\n")
	citer := createProjectDocument(t, handler, "TEST", "Citer", "# Citer\n\ndispatch://TEST/artifact/"+ledger.Slug+"\n")
	type accepted struct{ phrase, renamed string }
	spec := accepted{"zarquonspecversion", `Accepted suggestion: "quick" → "slow"`}
	notes := accepted{"zarquonledgerversion", `Accepted suggestion: "kilo" → "november"`}
	specComment := acceptSuggestion(t, handler, "/api/v1/issues/"+issue.Key+"/comments", "spec",
		"Say this instead, "+spec.phrase+".", "quick", "slow")
	ledgerComment := acceptSuggestion(t, handler, "/api/v1/artifacts/"+ledger.ID+"/comments", ledger.Slug,
		"Say this instead, "+notes.phrase+".", "kilo", "november")
	specVersion := strconv.Itoa(versionNamed(t, documentVersionNames(t, handler, issue.PrimaryArtifactID), spec.phrase))
	ledgerVersion := strconv.Itoa(versionNamed(t, documentVersionNames(t, handler, ledger.ID), notes.phrase))

	// Every row reads artifact_versions.summary, which renameAcceptedVersion rewrites in place in
	// the delete's transaction: the delete renames the version the accept recorded.
	rows := []struct {
		read, path string
		version    accepted
	}{
		// Read by getArtifact.
		{"6.3 GET /api/v1/issues/{key}/artifacts/{slug}", "/api/v1/issues/" + issue.Key + "/artifacts/spec", spec},
		// Read by getArtifact.
		{"6.3 GET /api/v1/projects/{key}/artifacts/{slug}", "/api/v1/projects/TEST/artifacts/" + ledger.Slug, notes},
		// Read by listArtifacts.
		{"6.4 GET /api/v1/issues/{key}/artifacts", "/api/v1/issues/" + issue.Key + "/artifacts", spec},
		// Read by listProjectArtifacts.
		{"6.4 GET /api/v1/projects/{key}/artifacts", "/api/v1/projects/TEST/artifacts", notes},
		// Read by getIssue (readIssue's artifacts and their versions).
		{"6.5 GET /api/v1/issues/{key}", "/api/v1/issues/" + issue.Key, spec},
		// Read by getArtifactVersion.
		{"6.6 GET /api/v1/artifacts/{id}/versions/{number}", "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/versions/" + specVersion, spec},
		// Read by getArtifactVersion.
		{"6.6 GET /api/v1/issues/{key}/artifacts/{slug}/versions/{number}", "/api/v1/issues/" + issue.Key + "/artifacts/spec/versions/" + specVersion, spec},
		// Read by getArtifactVersion.
		{"6.6 GET /api/v1/projects/{key}/artifacts/{slug}/versions/{number}", "/api/v1/projects/TEST/artifacts/" + ledger.Slug + "/versions/" + ledgerVersion, notes},
		// Read by getArtifactReferences (refs.Outgoing, the cited document's versions).
		{"6.7 GET /api/v1/artifacts/{id}/references, a cited document's versions", "/api/v1/artifacts/" + citer.ID + "/references", notes},
	}
	for _, row := range rows {
		response := dispatchRequest(t, handler, http.MethodGet, row.path, nil, "alice")
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), row.version.phrase) {
			t.Fatalf("%s before the delete: status=%d body=%s, want 200 naming a version %q, so the read after it can fail",
				row.read, response.Code, response.Body.String(), row.version.phrase)
		}
	}

	deleteThreadComment(t, handler, specComment, "alice")
	deleteThreadComment(t, handler, ledgerComment, "alice")

	for _, row := range rows {
		t.Run(row.read, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodGet, row.path, nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("%s after the delete: status=%d body=%s", row.read, response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), row.version.phrase) {
				t.Fatalf("%s after the delete gives the deleted suggestion's text %q: %s", row.read, row.version.phrase, response.Body.String())
			}
			if summaries := versionSummaries(t, row.read, response.Body.Bytes()); !slices.Contains(summaries, row.version.renamed) {
				t.Fatalf("%s after the delete names the versions %q, want one %q", row.read, summaries, row.version.renamed)
			}
		})
	}
}
