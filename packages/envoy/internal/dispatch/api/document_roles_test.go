package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// issueRoleDocuments is what an issue read names by role: its spec, and its document of each
// role, "" where it holds none. DecisionsArtifactID is the old member a page of the release
// before reads.
type issueRoleDocuments struct {
	PrimaryArtifactID      string `json:"primary_artifact_id"`
	VerificationArtifactID string `json:"verification_artifact_id"`
	PlanArtifactID         string `json:"plan_artifact_id"`
	DecisionsArtifactID    string `json:"decisions_artifact_id"`
}

func readIssueRoleDocuments(t *testing.T, handler http.Handler, key string) issueRoleDocuments {
	t.Helper()
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the issue %s: status=%d body=%s", key, read.Code, read.Body.String())
	}
	return decodeBody[issueRoleDocuments](t, read)
}

// A document an issue holds under one of the reserved names plays that role from the moment
// it is created, the issue read names both documents by it, and nothing else on the issue takes a
// role. The role is the document's for life: a second upload of the same name is its next
// version, under the row that already holds the role.
func TestIssueDocumentsTakeTheirRoleFromTheirName(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "ROLE", "Roles", "A spec\n")
	owner := "/api/v1/issues/" + issue.Key

	verification := uploadDocument(t, handler, owner, "verification.md", "# Verification\n\nOne.\n")
	plan := uploadDocument(t, handler, owner, "plan.md", "# Plan\n\nOne.\n")
	notes := uploadDocument(t, handler, owner, "notes.md", "# Notes\n\nOne.\n")
	if verification.Artifact.Role != "verification" || plan.Artifact.Role != "plan" || notes.Artifact.Role != "" {
		t.Fatalf("roles = %q, %q, %q; want verification, plan and none",
			verification.Artifact.Role, plan.Artifact.Role, notes.Artifact.Role)
	}
	if again := uploadDocument(t, handler, owner, "plan.md", "# Plan\n\nTwo.\n"); again.Artifact.ID != plan.Artifact.ID || again.Artifact.Role != "plan" || again.Version.Number != 2 {
		t.Fatalf("second upload of plan.md = %#v version %d, want version 2 of the plan", again.Artifact, again.Version.Number)
	}

	// The spec keeps is_primary and takes no role.
	spec := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID, nil, "alice")
	if spec.Code != http.StatusOK {
		t.Fatalf("read the spec: status=%d body=%s", spec.Code, spec.Body.String())
	}
	if read := decodeBody[model.Artifact](t, spec); !read.Primary || read.Role != "" {
		t.Fatalf("spec = primary %v role %q, want the primary document with no role", read.Primary, read.Role)
	}

	read := dispatchRequest(t, handler, http.MethodGet, owner, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the issue: status=%d body=%s", read.Code, read.Body.String())
	}
	detail := decodeBody[struct {
		PrimaryArtifactID      string `json:"primary_artifact_id"`
		VerificationArtifactID string `json:"verification_artifact_id"`
		PlanArtifactID         string `json:"plan_artifact_id"`
	}](t, read)
	if detail.PrimaryArtifactID != issue.PrimaryArtifactID ||
		detail.VerificationArtifactID != verification.Artifact.ID ||
		detail.PlanArtifactID != plan.Artifact.ID {
		t.Fatalf("issue read = %#v, want the spec, the Verification document and the plan", detail)
	}

	// Another issue holds its own pair: the role is unique within an issue, not across the project.
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "ROLE", "title": "A second issue", "spec": "Another spec\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create a second issue: status=%d body=%s", created.Code, created.Body.String())
	}
	other := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if second := uploadDocument(t, handler, "/api/v1/issues/"+other.Key, "plan.md", "# Plan\n\nOne.\n"); second.Artifact.Role != "plan" {
		t.Fatalf("the second issue's plan = %#v, want its own plan role", second.Artifact)
	}

	// A project document takes no role: a role belongs to an issue.
	if project := uploadDocument(t, handler, "/api/v1/projects/ROLE", "plan.md", "# Plan\n\nOne.\n"); project.Artifact.Role != "" {
		t.Fatalf("project document = %#v, want no role", project.Artifact)
	}
}

// A decision is a `decision` block of the spec, so a new document named decisions.md, or of any
// name whose slug is decisions-md, on an issue is refused 409 DECISIONS_ARE_NODES with a message
// that says to write a `decision` block in the spec: an upload, a create with no content, and a
// rename of another document to that name. A document of
// any other name is taken, with no role, and a project document of that name is no issue's.
// Where the issue still holds a decisions document an earlier release stored, an upload of its
// name is that document's next version, as a client of the release before writes it, and the
// write gives a roleless one no role. Once that document is retired, its name is refused as a
// new one is, rather than as a retired document, and so is an edit of it by its id or its slug.
func TestADecisionsDocumentIsRefusedByNameAndOneThatStandsTakesItsNextVersion(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "DEC", "Decisions", "A spec\n")
	owner := "/api/v1/issues/" + issue.Key

	refused := func(what string, response *httptest.ResponseRecorder) {
		t.Helper()
		body := response.Body.String()
		if response.Code != http.StatusConflict || !strings.Contains(body, codeDecisionsAreNodes) ||
			!strings.Contains(body, "`decision` block") || !strings.Contains(body, "spec") {
			t.Fatalf("%s: status=%d body=%s, want 409 %s saying to write a `decision` block in the spec",
				what, response.Code, body, codeDecisionsAreNodes)
		}
	}
	for _, name := range []string{"decisions.md", "Decisions.MD", "decisions-md"} {
		refused("an upload of "+name, dispatchRequest(t, handler, http.MethodPost, owner+"/artifacts",
			map[string]string{"name": name, "content": "# Decisions\n\n## Transport\n\nHTTP it is.\n"}, "alice"))
	}
	refused("a create of decisions.md with no content",
		dispatchRequest(t, handler, http.MethodPost, owner+"/artifacts", map[string]string{"name": "decisions.md"}, "alice"))

	notes := uploadDocument(t, handler, owner, "notes.md", "# Notes\n\nOne.\n")
	if notes.Artifact.Role != "" {
		t.Fatalf("notes.md = %#v, want a document of its own name taken with no role", notes.Artifact)
	}
	refused("a rename of notes.md to decisions.md", dispatchRequest(t, handler, http.MethodPatch,
		"/api/v1/artifacts/"+notes.Artifact.ID, map[string]string{"name": "decisions.md"}, "alice"))
	if project := uploadDocument(t, handler, "/api/v1/projects/DEC", "decisions.md", "# Decisions\n\nOne.\n"); project.Artifact.Role != "" {
		t.Fatalf("project document = %#v, want it taken with no role: the refusal is an issue's", project.Artifact)
	}
	var stored int
	if err := database.Pool.QueryRow(ctx, `select count(*) from artifacts where issue_key = $1 and slug like 'decisions%'`, issue.Key).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("the refused writes stored %d decisions documents on %s (%v), want none", stored, issue.Key, err)
	}

	// An issue that still holds a decisions document the release before stored, whose role a
	// build older than the role left null: an upload of its name is its next version.
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "DEC", "title": "Decided before the release", "spec": "Another spec\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create a second issue: status=%d body=%s", created.Code, created.Body.String())
	}
	other := decodeBody[model.Issue](t, created)
	standing := seedDecisionsDocument(t, database, deps.Docs.(*docs.Service), other.Key, "# Decisions\n\n## Transport\n\nHTTP it is.\n")
	if _, err := database.Pool.Exec(ctx, `update artifacts set role = null where id = $1`, standing); err != nil {
		t.Fatalf("clear the stored role: %v", err)
	}
	next := uploadDocument(t, handler, "/api/v1/issues/"+other.Key, "decisions.md", "# Decisions\n\n## Transport\n\nHTTP/2 it is.\n")
	if next.Artifact.ID != standing || next.Version.Number != 2 || next.Artifact.Role != "" {
		t.Fatalf("an upload of decisions.md while one stands = %#v version %d, want version 2 of %s with no role written",
			next.Artifact, next.Version.Number, standing)
	}

	// Retired, the document takes no upload of its name, which is refused as a new one is.
	retireTestDocument(t, database, standing)
	refused("an upload of decisions.md once it is retired", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+other.Key+"/artifacts", map[string]string{"name": "decisions.md", "content": "# Decisions\n\nThree.\n"}, "alice"))
	ops := map[string]any{"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": "Four."}}, "summary": "edit"}
	refused("an edit of the retired decisions document by its id", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+standing+"/edits", ops, "alice"))
	refused("an edit of the retired decisions document by its slug", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+other.Key+"/artifacts/decisions-md/edits", ops, "alice"))
}
