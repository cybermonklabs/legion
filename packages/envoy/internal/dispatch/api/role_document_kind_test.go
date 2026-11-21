package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// roleDocumentSpec is the spec of an issue whose role documents the test stores: one requirement,
// which the plan task of the plan's Markdown serves.
const roleDocumentSpec = ":::requirement{#r1}\nA plan is always a document.\n:::\n"

// roleArtifactRead is what the test reads of an artifact on either version: the upload's answer
// carries it as its artifact, and GET /api/v1/artifacts/{id} and GET /api/v2/artifacts/{id} answer
// it at the top level.
type roleArtifactRead struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Role string `json:"role"`
}

// TestARolesDocumentIsAlwaysADocument is the server half of dispatch://INFRA-4071's check
// c-role-document-kind (spec R15): a plan, a Verification document and a decisions document are
// always documents, on /api/v1 and /api/v2 alike, since the two share the upload and the plan
// review. Each version runs on issues of its own. R3.7 names the four /api/v1 answers this
// changes, and each is held here: a file or an image under the name or the slug of a document that
// plays a role goes from 201, or from 400 ARTIFACT_KIND_MISMATCH where that document already is
// one, to 400 ARTIFACT_ROLE_NAME; a Markdown upload onto a file or an image at such a slug goes from
// 400 ARTIFACT_KIND_MISMATCH to 201, and the artifact becomes that document with its earlier
// versions still readable; and a plan review of a plan that is not a document goes from 201 to 409
// NOT_PLAN_DOCUMENT, naming its kind. The plan the conversion starts from is the shape
// dispatch://BET-1328/artifact/plan-md was left in: plan-md stored as a file with no role, a plan
// review approving its version 1.
func TestARolesDocumentIsAlwaysADocument(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		t.Run(version, func(t *testing.T) {
			api := "/api/" + version
			// With a file store the refusal is made before the object is written
			// (refuseUploadBeforeItsObject), and with none by the upload's transaction
			// (storeArtifact): the two refuse alike.
			t.Run("a file or an image under a role's name is refused, with a file store", func(t *testing.T) {
				refuseFilesUnderRoleNames(t, api, filestest.NewMemory())
			})
			t.Run("a file or an image under a role's name is refused, with no file store", func(t *testing.T) {
				refuseFilesUnderRoleNames(t, api, nil)
			})
			t.Run("a plan stored as a file becomes a document", func(t *testing.T) {
				convertAPlanStoredAsAFile(t, api)
			})
			t.Run("a Verification document stored as a picture becomes a document", func(t *testing.T) {
				convertAVerificationDocumentStoredAsAPicture(t, api)
			})
		})
	}
}

// refuseFilesUnderRoleNames uploads a file or an image under each name and slug of an issue's role
// documents, none of which exists yet, through api's upload, and holds each to 400
// ARTIFACT_ROLE_NAME naming the role and Markdown, with no artifact and no object left behind. The
// controls are a file under another name on the issue and a file under plan.md on a project, whose
// plan.md plays no role: both are stored as files.
func refuseFilesUnderRoleNames(t *testing.T, api string, memory *filestest.Memory) {
	t.Helper()
	options := testServerOptions{}
	if memory != nil {
		options.files = memory
	}
	handler, database, _ := newTestServer(t, options)
	issue := createInteractionIssue(t, handler, "ROLE", "Role documents", roleDocumentSpec)
	uploads := api + "/issues/" + issue.Key + "/artifacts"
	text := []byte("A plan, typed in an editor and saved as a text file.\n")
	for _, upload := range []struct {
		name, filename, contentType string
		content                     []byte
		role                        string
	}{
		{"plan.md", "plan.txt", "text/plain", text, model.ArtifactRolePlan},
		{"plan-md", "plan.txt", "text/plain", text, model.ArtifactRolePlan},
		{"verification.md", "verification.txt", "text/plain", text, model.ArtifactRoleVerification},
		{"decisions.md", "decisions.txt", "text/plain", text, model.ArtifactRoleDecisions},
		{"plan.md", "plan.png", "image/png", pngBytes("a plan, drawn"), model.ArtifactRolePlan},
	} {
		response := multipartRequest(t, handler, uploads, map[string]string{"name": upload.name}, upload.filename, upload.contentType, upload.content, "alice")
		code, refusal := refusalOf(t, response)
		if response.Code != http.StatusBadRequest || code != "ARTIFACT_ROLE_NAME" ||
			!strings.Contains(refusal, "the issue's "+upload.role+" document") || !strings.Contains(refusal, "Markdown") {
			t.Errorf("a %s upload under %q: status=%d body=%s; want 400 ARTIFACT_ROLE_NAME naming the issue's %s document and Markdown",
				upload.contentType, upload.name, response.Code, response.Body.String(), upload.role)
		}
	}
	var stored int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from artifacts where issue_key = $1 and not is_primary`, issue.Key).Scan(&stored); err != nil {
		t.Fatalf("count the issue's artifacts: %v", err)
	}
	if stored != 0 {
		t.Errorf("the issue holds %d artifacts besides its spec after the refused uploads, want none", stored)
	}
	if memory != nil && memory.Len() != 0 {
		t.Errorf("the file store holds %d objects after the refused uploads, want none", memory.Len())
	}

	for _, control := range []struct{ owner, name string }{
		{api + "/issues/" + issue.Key, "notes.txt"},
		{api + "/projects/ROLE", "plan.md"},
	} {
		response := multipartRequest(t, handler, control.owner+"/artifacts", map[string]string{"name": control.name}, "upload.txt", "text/plain", text, "alice")
		answer := decodeBody[struct {
			Artifact roleArtifactRead `json:"artifact"`
		}](t, response)
		if response.Code != http.StatusCreated || answer.Artifact.Kind != "file" || answer.Artifact.Role != "" {
			t.Errorf("a text/plain upload under %q to %s: status=%d body=%s; want 201 a file that plays no role",
				control.name, control.owner, response.Code, response.Body.String())
		}
	}
}

// convertAPlanStoredAsAFile seeds an issue's plan-md as BET-1328's was stored, refuses a plan
// review of it, and then stores a Markdown upload under plan.md as its next version, which makes
// it the issue's plan: its version 1 still reads as the file it was, its later writes and its plan
// review are a document's, and the coverage read counts its plan task.
func convertAPlanStoredAsAFile(t *testing.T, api string) {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := createInteractionIssue(t, handler, "ROLE", "A plan stored as a file", roleDocumentSpec)
	uploads := api + "/issues/" + issue.Key + "/artifacts"
	planText := []byte("The plan, typed in an editor and saved as plan.txt.\n")
	const planType = "text/plain; charset=utf-8"
	plan := seedRoleDocumentAsFile(t, database, issue.Key, "plan.md", "file", planType, planText)
	approveTheFileByPlanReview(t, database, deps, plan)

	reviews := api + "/artifacts/" + plan + "/plan-reviews"
	refused := sessionRequest(t, handler, http.MethodPost, reviews, map[string]any{"actor": sessionActor(), "version": 1})
	if code, refusal := refusalOf(t, refused); refused.Code != http.StatusConflict || code != "NOT_PLAN_DOCUMENT" || !strings.Contains(refusal, "is a file") {
		t.Errorf("a plan review of the plan stored as a file: status=%d body=%s; want 409 NOT_PLAN_DOCUMENT naming its kind, a file",
			refused.Code, refused.Body.String())
	}

	markdown := "# Plan\n\n" + planTaskServing("t1", "dispatch://"+issue.Key+"/spec#b-r1")
	converted := dispatchRequest(t, handler, http.MethodPost, uploads, map[string]string{
		"name": "plan.md", "content": markdown, "summary": "The plan as Markdown",
	}, "alice")
	if converted.Code != http.StatusCreated {
		t.Fatalf("a Markdown upload under plan.md onto the plan stored as a file: status=%d body=%s; want 201", converted.Code, converted.Body.String())
	}
	answer := decodeBody[struct {
		Artifact roleArtifactRead `json:"artifact"`
		Version  model.Version    `json:"version"`
	}](t, converted)
	if answer.Artifact.ID != plan || answer.Artifact.Kind != "doc" || answer.Artifact.Role != model.ArtifactRolePlan || answer.Version.Number != 2 {
		t.Fatalf("the Markdown upload answered artifact %+v version %d; want %s as the issue's plan document, version 2",
			answer.Artifact, answer.Version.Number, plan)
	}
	read := dispatchRequest(t, handler, http.MethodGet, api+"/artifacts/"+plan, nil, "alice")
	if artifact := decodeBody[roleArtifactRead](t, read); read.Code != http.StatusOK || artifact.Kind != "doc" || artifact.Role != model.ArtifactRolePlan {
		t.Fatalf("the plan reads status=%d body=%s; want kind doc and role plan", read.Code, read.Body.String())
	}
	if api == "/api/v2" {
		// The row's facts of its newest version are the Markdown's, and it counts the file's.
		facts := newestVersionFactsOf(t, decodeBody[map[string]json.RawMessage](t, read))
		if facts.Number == nil || *facts.Number != 2 || facts.Count == nil || *facts.Count != 2 || facts.Size == nil || answer.Version.Size == nil ||
			*facts.Size != *answer.Version.Size || facts.At == nil || !facts.At.Equal(answer.Version.CreatedAt) {
			t.Fatalf("the plan's row is %s; want version 2 of 2 in latest_version, version_count, latest_version_size and "+
				"latest_version_at, written at %s storing the upload's bytes (%s)", read.Body.String(), answer.Version.CreatedAt, converted.Body.String())
		}
	}
	requireEarlierFileVersion(t, handler, plan, issue.Key, "plan-md", planType, planText)
	for _, route := range []string{"/api/v1/artifacts/" + plan, "/api/v2/artifacts/" + plan, "/api/v1/issues/" + issue.Key + "/artifacts/plan-md"} {
		second := dispatchRequest(t, handler, http.MethodGet, route+"/versions/2", nil, "alice")
		if text := decodeBody[struct {
			Markdown string `json:"markdown"`
		}](t, second).Markdown; second.Code != http.StatusOK || !strings.Contains(text, "plan_task") || !strings.Contains(text, "Do the work.") {
			t.Fatalf("version 2 at %s: status=%d body=%s; want the plan's Markdown", route, second.Code, second.Body.String())
		}
	}

	// A plan review naming version 1, the file the plan was, still records: R15 refuses a review
	// of a plan that is not a document, and this plan is one now. It approved no text, so it
	// carries nothing to the versions written after it (below).
	fileReview := sessionRequest(t, handler, http.MethodPost, reviews, map[string]any{"actor": sessionActor(), "version": 1})
	if fileReview.Code != http.StatusCreated || decodeBody[model.ArtifactReview](t, fileReview).Version != 1 {
		t.Fatalf("a plan review of the converted plan's version 1: status=%d body=%s; want 201 a review of version 1",
			fileReview.Code, fileReview.Body.String())
	}

	// The converted plan takes every later write a document takes. The upload replaces the
	// document the conversion made, so it names the token of the version it replaces (#3000).
	if again := dispatchRequest(t, handler, http.MethodPost, uploads, map[string]any{
		"name": "plan.md", "content": markdown + "\nThe plan's second draft.\n",
		"precondition": uploadBase(t, handler, plan),
	}, "alice"); again.Code != http.StatusCreated || decodeBody[uploadedArtifact](t, again).Version.Number != 3 {
		t.Fatalf("a later Markdown upload under plan.md: status=%d body=%s; want 201 version 3", again.Code, again.Body.String())
	}
	edited := sessionRequest(t, handler, http.MethodPost, api+"/artifacts/"+plan+"/edits", map[string]any{
		"actor": sessionActor(), "ops": []map[string]string{replaceOp("Do the work.", "Do the work, then check it.")},
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("an edit of the converted plan: status=%d body=%s; want 200", edited.Code, edited.Body.String())
	}
	waitForArtifactVersion(t, handler, plan, 4)
	// Neither later write carried the approval of the file: the plan's reviews are the two of
	// version 1, and its approval is stale at version 4.
	if planReviews := listReviews(t, handler, plan); len(planReviews) != 2 ||
		slices.ContainsFunc(planReviews, func(review model.ArtifactReview) bool { return review.Version != 1 }) {
		t.Fatalf("the plan's reviews after the later writes = %+v; want the two of version 1, the approval of the file carrying nothing", planReviews)
	}
	if read := readApproval(t, handler, plan); read.Approval == nil || read.Approval.State != "stale" || read.Approval.LatestVersion != 4 {
		t.Fatalf("the plan's approval after the later writes = %+v; want the approval of the file stale at version 4", read.Approval)
	}
	if reviewed := sessionRequest(t, handler, http.MethodPost, reviews, map[string]any{"actor": sessionActor(), "version": 2}); reviewed.Code != http.StatusCreated {
		t.Fatalf("a plan review of the converted plan's version 2: status=%d body=%s; want 201", reviewed.Code, reviewed.Body.String())
	}

	coverageRead := dispatchRequest(t, handler, http.MethodGet, api+"/issues/"+issue.Key+"/coverage", nil, "alice")
	if coverageRead.Code != http.StatusOK {
		t.Fatalf("read the coverage: status=%d body=%s", coverageRead.Code, coverageRead.Body.String())
	}
	coverage := decodeBody[model.IssueCoverage](t, coverageRead)
	if requirement := requirementOf(t, coverage, "r1"); !coverage.HasPlan || requirement.PlanTasks != 1 || slices.Contains(coverage.Gaps, "r1 "+noPlanTaskReason) {
		t.Fatalf("the coverage reads has_plan %t, r1 %+v and gaps %q; want the plan task of the plan's Markdown counted",
			coverage.HasPlan, requirement, coverage.Gaps)
	}

	// The plan is a document now, so a file under its name reads the refusal of a role's name and
	// not that of a kind it does not have.
	file := multipartRequest(t, handler, uploads, map[string]string{"name": "plan.md"}, "plan.txt", "text/plain", planText, "alice")
	if code, refusal := refusalOf(t, file); file.Code != http.StatusBadRequest || code != "ARTIFACT_ROLE_NAME" || !strings.Contains(refusal, "the issue's plan document") {
		t.Fatalf("a text/plain file under plan.md once plan-md is a document: status=%d body=%s; want 400 ARTIFACT_ROLE_NAME", file.Code, file.Body.String())
	}
}

// convertAVerificationDocumentStoredAsAPicture seeds an issue's verification-md as a picture, with
// no review, and stores a Markdown upload under verification.md as its next version, which makes
// it the issue's Verification document; its version 1 is still served as the picture it was.
func convertAVerificationDocumentStoredAsAPicture(t *testing.T, api string) {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := createInteractionIssue(t, handler, "ROLE", "A Verification document stored as a picture", roleDocumentSpec)
	picture := pngBytes("the Verification document, drawn")
	verification := seedRoleDocumentAsFile(t, database, issue.Key, "verification.md", "image", "image/png", picture)

	converted := dispatchRequest(t, handler, http.MethodPost, api+"/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "verification.md", "content": checkServing("k1", "dispatch://"+issue.Key+"/spec#b-r1"),
	}, "alice")
	answer := decodeBody[struct {
		Artifact roleArtifactRead `json:"artifact"`
		Version  model.Version    `json:"version"`
	}](t, converted)
	if converted.Code != http.StatusCreated || answer.Artifact.ID != verification || answer.Artifact.Kind != "doc" ||
		answer.Artifact.Role != model.ArtifactRoleVerification || answer.Version.Number != 2 {
		t.Fatalf("a Markdown upload under verification.md onto the picture: status=%d body=%s; want 201 version 2 of %s, "+
			"the issue's Verification document", converted.Code, converted.Body.String(), verification)
	}
	requireEarlierFileVersion(t, handler, verification, issue.Key, "verification-md", "image/png", picture)
}

// requireEarlierFileVersion holds version 1 of the document artifactID, the slug of issueKey, to
// the file it was stored as on the version routes of both versions: its bytes, under its own type.
func requireEarlierFileVersion(t *testing.T, handler http.Handler, artifactID, issueKey, slug, contentType string, content []byte) {
	t.Helper()
	for _, route := range []string{"/api/v1/artifacts/" + artifactID, "/api/v2/artifacts/" + artifactID, "/api/v1/issues/" + issueKey + "/artifacts/" + slug} {
		first := dispatchRequest(t, handler, http.MethodGet, route+"/versions/1", nil, "alice")
		if first.Code != http.StatusOK || first.Header().Get("Content-Type") != contentType || !bytes.Equal(first.Body.Bytes(), content) {
			t.Fatalf("version 1 at %s: status=%d Content-Type=%q body=%q; want the %q bytes it was stored as",
				route, first.Code, first.Header().Get("Content-Type"), first.Body.String(), contentType)
		}
	}
}

// seedRoleDocumentAsFile stores, by SQL through the test store, the artifact name gives on
// issueKey as kind (file or image) with no role, and its version 1 holding content under
// contentType: the row an upload of a file under a role's name left before it was refused. It
// returns the artifact's id.
func seedRoleDocumentAsFile(t *testing.T, database *store.Store, issueKey, name, kind, contentType string, content []byte) string {
	t.Helper()
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		select key, project_key, $2, $3, $4, '{"kind":"user","id":"alice"}' from issues where key = $1
		returning id::text`, issueKey, model.ArtifactSlug(name), name, kind).Scan(&id); err != nil {
		t.Fatalf("store %s as a %s: %v", name, kind, err)
	}
	seedExec(t, database, name+"'s version 1", `
		insert into artifact_versions (artifact_id, number, content, mime, size, sha256, authors)
		values ($1, 1, $2, $3, $4, $5, '[{"kind":"user","id":"alice"}]')`,
		id, content, contentType, len(content), files.SHA256(content))
	return id
}

// approveTheFileByPlanReview records a plan review approving version 1 of artifactID through
// docs.QueueReview, as the plan review route did of BET-1328's plan while it was a file.
func approveTheFileByPlanReview(t *testing.T, database *store.Store, deps Deps, artifactID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan review: %v", err)
	}
	defer tx.Rollback(ctx)
	artifact, err := loadArtifactRowByID(ctx, tx, artifactID)
	if err != nil {
		t.Fatalf("read the plan: %v", err)
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		return docs.QueueReview(b, deps.Events, artifact, 1, "approved", docs.ReviewKindPlanReview,
			model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil, nil, func(model.ArtifactReview, model.Event) {})
	}); err != nil {
		t.Fatalf("record the plan review of the file: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the plan review: %v", err)
	}
}
