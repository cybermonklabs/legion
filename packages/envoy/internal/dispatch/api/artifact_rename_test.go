package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// correctTitle sends PATCH /api/v1/artifacts/{id} as alice, the person who made the document.
func correctTitle(t *testing.T, handler http.Handler, artifactID, name string) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/artifacts/"+artifactID, map[string]any{"name": name}, "alice")
}

// correctedTitle is the document as the accepted correction answers it.
func correctedTitle(t *testing.T, handler http.Handler, artifactID, name string) model.Artifact {
	t.Helper()
	response := correctTitle(t, handler, artifactID, name)
	if response.Code != http.StatusOK {
		t.Fatalf("correct the title to %q: status=%d body=%s", name, response.Code, response.Body.String())
	}
	return decodeBody[model.Artifact](t, response)
}

// readDocument is GET /api/v1/artifacts/{id}, the read a correction answers the shape of.
func readDocument(t *testing.T, handler http.Handler, artifactID string) model.Artifact {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read document %s: status=%d body=%s", artifactID, response.Code, response.Body.String())
	}
	return decodeBody[model.Artifact](t, response)
}

// titleIssue is another issue of the test project, so each correction below has a document list
// and an event log of its own. The project itself is made once, by the first issue. The create
// is forced past the near-copy gate: these issues are alike by design, and what each is for is
// its documents.
func titleIssue(t *testing.T, handler http.Handler, title string) model.Issue {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "TEST", "title": title, "force": true,
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create the issue %q: status=%d body=%s", title, response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

// renamedEvent is the payload an artifact.renamed event carries: the document as it now is, and
// the title it held before.
type renamedEvent struct {
	Artifact     model.Artifact `json:"artifact"`
	PreviousName string         `json:"previous_name"`
}

// renameEvents is every artifact.renamed event of an issue, as the event log carries them.
func renameEvents(t *testing.T, handler http.Handler, issueKey string) []renamedEvent {
	t.Helper()
	renamed := []renamedEvent{}
	for _, event := range issueEvents(t, handler, issueKey) {
		if event.Type != "artifact.renamed" {
			continue
		}
		var payload renamedEvent
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode the artifact.renamed payload %s: %v", event.Payload, err)
		}
		renamed = append(renamed, payload)
	}
	return renamed
}

// A person corrects a document's title and the document keeps its address: its slug, the
// reference key that slug gives and the role it plays are what the first title fixed, so every
// reference, every anchored comment and the issue's own reads still name the same row. The
// corrections Dispatch refuses - a name the owner holds, a name reserved for a document Dispatch
// makes itself, the issue's spec, an artifact that is no document, and a name that is empty or
// too long - each leave the document as it was.
func TestADocumentTitleIsCorrectedWithoutMovingItsAddress(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	spec := createInteractionIssue(t, handler, "TEST", "A spec of its own", "# Spec\n\nA passage.\n")

	t.Run("the title is the new one and the address is the first title's", func(t *testing.T) {
		issue := titleIssue(t, handler, "A corrected title")
		owner := "/api/v1/issues/" + issue.Key
		document := uploadDocument(t, handler, owner, "Lauch notes", "# Notes\n\nA passage.\n").Artifact
		if document.Slug != "lauch-notes" {
			t.Fatalf("the document made under the first title = %#v, want the slug lauch-notes", document)
		}
		comment := createThreadComment(t, handler, issue.Key, map[string]any{
			"body": "A remark on the notes.", "anchor": map[string]any{"artifact": document.Slug, "quote": "A passage."},
		}, "alice")

		corrected := correctedTitle(t, handler, document.ID, "Launch notes")
		if corrected.Name != "Launch notes" || corrected.Slug != document.Slug || corrected.RefKey != document.RefKey ||
			corrected.ID != document.ID || corrected.Role != document.Role || corrected.Primary != document.Primary {
			t.Fatalf("the corrected document = %#v, want %q under the slug %s and the reference key %s",
				corrected, "Launch notes", document.Slug, document.RefKey)
		}
		if len(corrected.Versions) != 1 {
			t.Fatalf("the corrected document holds %d versions, want the one it had", len(corrected.Versions))
		}
		if stored := readDocument(t, handler, document.ID); stored.Name != "Launch notes" || stored.Slug != document.Slug {
			t.Fatalf("the document read back = %#v, want %q under the slug %s", stored, "Launch notes", document.Slug)
		}

		// The reference the first title gave still answers this document, by the node the
		// graph reads and by the issue's own read of that slug.
		reference := "dispatch://" + issue.Key + "/artifact/" + document.Slug
		if node := readNode(t, handler, reference); node.Kind != "artifact" || node.ID != document.ID {
			t.Fatalf("%s answers %#v, want the corrected document %s", reference, node, document.ID)
		}
		bySlug := dispatchRequest(t, handler, http.MethodGet, owner+"/artifacts/"+document.Slug, nil, "alice")
		if bySlug.Code != http.StatusOK {
			t.Fatalf("read %s by its first title's slug: status=%d body=%s", document.ID, bySlug.Code, bySlug.Body.String())
		}
		if read := decodeBody[model.Artifact](t, bySlug); read.ID != document.ID || read.Name != "Launch notes" {
			t.Fatalf("the read by slug = %#v, want the corrected document %s", read, document.ID)
		}

		// The comment anchored in the document still resolves to it.
		anchored := readThreadComment(t, handler, comment.ID)
		if anchored.Anchor == nil || anchored.Anchor.ArtifactID != document.ID || anchored.Anchor.Orphaned {
			t.Fatalf("the anchored comment after the correction = %#v, want its anchor in %s", anchored.Anchor, document.ID)
		}
		comments := dispatchRequest(t, handler, http.MethodGet, owner+"/comments", nil, "alice")
		if comments.Code != http.StatusOK || !strings.Contains(comments.Body.String(), comment.ID) {
			t.Fatalf("the issue's comments after the correction: status=%d body=%s", comments.Code, comments.Body.String())
		}

		renamed := renameEvents(t, handler, issue.Key)
		if len(renamed) != 1 {
			t.Fatalf("the issue's event log holds %d artifact.renamed events, want one", len(renamed))
		}
		if renamed[0].PreviousName != "Lauch notes" || renamed[0].Artifact.Name != "Launch notes" ||
			renamed[0].Artifact.ID != document.ID || renamed[0].Artifact.Slug != document.Slug {
			t.Fatalf("the artifact.renamed event = %#v with previous name %q, want both titles of %s",
				renamed[0].Artifact, renamed[0].PreviousName, document.ID)
		}
	})

	t.Run("the role does not move, and the role's own name is this document's to take", func(t *testing.T) {
		issue := titleIssue(t, handler, "A plan with a corrected title")
		plan := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "plan.md", "# Plan\n").Artifact
		if plan.Slug != "plan-md" || plan.Role != model.ArtifactRolePlan {
			t.Fatalf("the plan document = %#v, want the slug plan-md playing the plan role", plan)
		}

		corrected := correctedTitle(t, handler, plan.ID, "The plan")
		if corrected.Name != "The plan" || corrected.Slug != "plan-md" || corrected.Role != model.ArtifactRolePlan {
			t.Fatalf("the corrected plan = %#v, want it under the slug plan-md still playing the plan role", corrected)
		}
		if roles := readIssueRoleDocuments(t, handler, issue.Key); roles.PlanArtifactID != plan.ID {
			t.Fatalf("the issue's plan document = %q, want %q, the row whose title was corrected", roles.PlanArtifactID, plan.ID)
		}

		// The document that plays the role takes the role's own name: the refusal below is for
		// a document that is not it.
		back := correctedTitle(t, handler, plan.ID, "plan.md")
		if back.Name != "plan.md" || back.Slug != "plan-md" || back.Role != model.ArtifactRolePlan {
			t.Fatalf("the plan corrected back to plan.md = %#v, want it under the slug plan-md playing the plan role", back)
		}
	})

	t.Run("a title Dispatch holds to no name rule is the person's to give", func(t *testing.T) {
		issue := titleIssue(t, handler, "A freely titled document")
		document := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "Launch notes", "# Notes\n").Artifact

		// A corrected title is held to two rules alone: it is not empty, and it is within what
		// a name may hold. An extension that is not Markdown, and a number a reader takes for
		// one, are titles a person gives.
		for _, name := range []string{"rows.jsonl", "Release 1.2"} {
			corrected := correctedTitle(t, handler, document.ID, name)
			if corrected.Name != name || corrected.Slug != document.Slug || corrected.RefKey != document.RefKey {
				t.Fatalf("the document corrected to %q = %#v, want that title under the slug %s and the reference key %s",
					name, corrected, document.Slug, document.RefKey)
			}
			if stored := readDocument(t, handler, document.ID); stored.Name != name || stored.Slug != document.Slug {
				t.Fatalf("the document read back after the correction to %q = %#v, want that title under the slug %s",
					name, stored, document.Slug)
			}
		}
	})

	t.Run("each refusal leaves the document as it was", func(t *testing.T) {
		issue := titleIssue(t, handler, "Refused corrections")
		owner := "/api/v1/issues/" + issue.Key
		notes := uploadDocument(t, handler, owner, "Launch notes", "# Notes\n").Artifact
		design := uploadDocument(t, handler, owner, "Design notes", "# Design\n").Artifact
		picture := uploadImage(t, handler, issue.Key, "diagram.png", "pixels").Artifact
		longName := strings.Repeat("a", maxNewArtifactKeyBytes+1)

		for _, refused := range []struct {
			what, artifactID, name, code string
			status                       int
			sentence                     string
		}{
			{
				what: "a name this owner holds as a name", artifactID: notes.ID, name: "Design notes",
				code: "ARTIFACT_NAME_TAKEN", status: http.StatusConflict,
				sentence: fmt.Sprintf("%q names artifact %s (%s) of this owner; a document's name is its own here: give it another name",
					"Design notes", design.Slug, design.Name),
			},
			{
				what: "a name this owner holds as a slug", artifactID: notes.ID, name: "design-notes",
				code: "ARTIFACT_NAME_TAKEN", status: http.StatusConflict,
				sentence: fmt.Sprintf("%q names artifact %s (%s) of this owner; a document's name is its own here: give it another name",
					"design-notes", design.Slug, design.Name),
			},
			{
				what: "the name of a document Dispatch makes itself", artifactID: notes.ID, name: "plan.md",
				code: "ARTIFACT_ROLE_NAME", status: http.StatusBadRequest,
				sentence: fmt.Sprintf("%q gives the slug plan-md, the slug of the issue's %s document, which this document is not; give it another name",
					"plan.md", model.ArtifactRolePlan),
			},
			{
				what: "the issue's spec", artifactID: spec.PrimaryArtifactID, name: "The specification",
				code: "ARTIFACT_PRIMARY", status: http.StatusBadRequest,
				sentence: "this document is the issue's spec, and its name is how the spec is written whole; the spec's name does not change",
			},
			{
				what: "a picture", artifactID: picture.ID, name: "The diagram",
				code: "ARTIFACT_KIND_MISMATCH", status: http.StatusBadRequest,
				sentence: fmt.Sprintf("artifact %s (%s) is a %s; only a document's name changes",
					picture.Slug, picture.Name, picture.Kind),
			},
			{
				what: "an empty name", artifactID: notes.ID, name: "   ",
				code: "INVALID_ARTIFACT", status: http.StatusBadRequest,
				sentence: "artifact name is required",
			},
			{
				what: "a name past what a name may hold", artifactID: notes.ID, name: longName,
				code: "INVALID_ARTIFACT", status: http.StatusBadRequest,
				sentence: fmt.Sprintf("artifact name is %d bytes, past the %d bytes a new artifact's name may hold; give it a shorter name",
					len(longName), maxNewArtifactKeyBytes),
			},
		} {
			before := readDocument(t, handler, refused.artifactID)
			response := correctTitle(t, handler, refused.artifactID, refused.name)
			refusal := decodeBody[refusedWrite](t, response)
			if response.Code != refused.status || refusal.Code != refused.code || refusal.Error != refused.sentence {
				t.Fatalf("the correction to %s: status=%d body=%s, want %d %s %q",
					refused.what, response.Code, response.Body.String(), refused.status, refused.code, refused.sentence)
			}
			after := readDocument(t, handler, refused.artifactID)
			if after.Name != before.Name || after.Slug != before.Slug || after.RefKey != before.RefKey {
				t.Fatalf("the document after the refusal of %s = %#v, want it as it was: %#v", refused.what, after, before)
			}
		}
		if stored := readDocument(t, handler, notes.ID); stored.Name != notes.Name || stored.Slug != notes.Slug {
			t.Fatalf("the refused document = %#v, want %q under the slug %s", stored, notes.Name, notes.Slug)
		}
		if stored := readDocument(t, handler, picture.ID); stored.Name != picture.Name {
			t.Fatalf("the refused picture = %#v, want %q", stored, picture.Name)
		}
		if renamed := renameEvents(t, handler, issue.Key); len(renamed) != 0 {
			t.Fatalf("the refusals left %d artifact.renamed events, want none", len(renamed))
		}
	})

	t.Run("a closed issue refuses the correction before the name is read", func(t *testing.T) {
		issue := titleIssue(t, handler, "A closed issue")
		document := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "Launch notes", "# Notes\n").Artifact
		closeIssue(t, handler, issue.Key)

		assertRefusedOnClosedIssue(t, "a title correction", correctTitle(t, handler, document.ID, "Lauch notes"))
		if stored := readDocument(t, handler, document.ID); stored.Name != "Launch notes" {
			t.Fatalf("the closed issue's document = %#v, want %q", stored, "Launch notes")
		}
	})

	t.Run("the person scope decides who corrects a title", func(t *testing.T) {
		issue := titleIssue(t, handler, "Who corrects a title")
		document := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "Launch notes", "# Notes\n").Artifact

		shared := sessionRequest(t, handler, http.MethodPatch, "/api/v1/artifacts/"+document.ID, map[string]any{"name": "An agent's title"})
		if shared.Code != http.StatusForbidden || !strings.Contains(shared.Body.String(), `"code":"`+codeScopeRequired+`"`) {
			t.Fatalf("the shared token's correction: status=%d body=%s, want 403 %s", shared.Code, shared.Body.String(), codeScopeRequired)
		}
		if stored := readDocument(t, handler, document.ID); stored.Name != "Launch notes" {
			t.Fatalf("the document after the refused correction = %#v, want %q", stored, "Launch notes")
		}

		// A credential that holds the person scope corrects it, and so does another person who
		// may write to the issue and did not make the document.
		personal := personalTokenHolding(t, database, model.ScopePerson)
		byToken := agentRequest(t, handler, http.MethodPatch, "/api/v1/artifacts/"+document.ID, map[string]any{"name": "A token's title"}, personal)
		if byToken.Code != http.StatusOK {
			t.Fatalf("a personal token holding the person scope: status=%d body=%s", byToken.Code, byToken.Body.String())
		}
		if corrected := decodeBody[model.Artifact](t, byToken); corrected.Name != "A token's title" {
			t.Fatalf("the token's correction = %#v, want %q", corrected, "A token's title")
		}
		byOther := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/artifacts/"+document.ID, map[string]any{"name": "Bob's title"}, "bob")
		if byOther.Code != http.StatusOK {
			t.Fatalf("another person's correction: status=%d body=%s", byOther.Code, byOther.Body.String())
		}
		if stored := readDocument(t, handler, document.ID); stored.Name != "Bob's title" || stored.Slug != document.Slug {
			t.Fatalf("the document after both corrections = %#v, want %q under the slug %s", stored, "Bob's title", document.Slug)
		}
	})

	t.Run("two corrections at once under one title", func(t *testing.T) {
		issue := titleIssue(t, handler, "Two corrections at once")
		owner := "/api/v1/issues/" + issue.Key
		first := uploadDocument(t, handler, owner, "First notes", "# First\n").Artifact
		second := uploadDocument(t, handler, owner, "Second notes", "# Second\n").Artifact

		start := make(chan struct{})
		responses := make(chan *httptest.ResponseRecorder, 2)
		for _, document := range []model.Artifact{first, second} {
			go func() {
				<-start
				responses <- correctTitle(t, handler, document.ID, "Launch notes")
			}()
		}
		close(start)
		accepted, refused := 0, 0
		for range 2 {
			response := awaitResponse(t, responses)
			switch response.Code {
			case http.StatusOK:
				accepted++
			case http.StatusConflict:
				refused++
				if code := decodeBody[refusedWrite](t, response).Code; code != "ARTIFACT_NAME_TAKEN" {
					t.Fatalf("the refused correction answered %q, want ARTIFACT_NAME_TAKEN", code)
				}
			default:
				t.Fatalf("a correction at once: status=%d body=%s", response.Code, response.Body.String())
			}
		}
		if accepted != 1 || refused != 1 {
			t.Fatalf("two corrections under one title: %d accepted and %d refused, want one of each", accepted, refused)
		}
		named := 0
		for _, document := range []model.Artifact{first, second} {
			if readDocument(t, handler, document.ID).Name == "Launch notes" {
				named++
			}
		}
		if named != 1 {
			t.Fatalf("%d documents hold the title, want the one the accepted correction took", named)
		}
	})

	t.Run("the next upload goes by the new title", func(t *testing.T) {
		issue := titleIssue(t, handler, "Uploads after a correction")
		owner := "/api/v1/issues/" + issue.Key
		document := uploadDocument(t, handler, owner, "Lauch notes", "# Notes\n").Artifact
		correctedTitle(t, handler, document.ID, "Launch notes")

		next := uploadDocument(t, handler, owner, "Launch notes", "# Notes\n\nMore.\n")
		if next.Artifact.ID != document.ID || next.Version.Number != 2 {
			t.Fatalf("the upload under the new title = %#v version %d, want version 2 of %s", next.Artifact, next.Version.Number, document.ID)
		}
		under := uploadDocument(t, handler, owner, "Lauch notes", "# The first title\n")
		if under.Artifact.ID == document.ID || under.Version.Number != 1 || under.Artifact.Slug == document.Slug {
			t.Fatalf("the upload under the first title = %#v version %d, want a second document", under.Artifact, under.Version.Number)
		}
	})
}
