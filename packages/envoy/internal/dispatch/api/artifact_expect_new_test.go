package api

import (
	"context"
	"net/http"
	"testing"
)

// TestAnUploadMadeACreateAloneMakesADocumentAndNeverWritesOverOne (review round 6, finding 3): the
// condition the command bar's in-process caller carries (goingCondition.ExpectNew) on every upload
// it sends but an undo's write back, which carries the content it expects instead, makes it
// a create alone, decided under the owner's lock: under a name
// the owner already holds it is refused, 409 DOCUMENT_EXISTS, and nothing is written - the
// document keeps its one version - and under a free name it makes the document. A row of a plan
// sheds the condition only at the write where its read before found the document it names
// (AgentListener.rowUpload); this one carries it, as every other upload the bar sends does. It
// holds for an issue's JSON upload, a project's, and a multipart upload, and it cannot be carried
// with an expected content (ExpectSHA256), which asks for the opposite.
func TestAnUploadMadeACreateAloneMakesADocumentAndNeverWritesOverOne(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "DEMO", "Uploads that only make", "The spec.\n").Key
	versions := func(t *testing.T, owner, name string) int {
		t.Helper()
		var count int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from artifact_versions v join artifacts a on a.id = v.artifact_id
			where a.name = $2 and (a.issue_key = $1 or (a.issue_key is null and a.project_key = $1))`, owner, name).Scan(&count); err != nil {
			t.Fatalf("count the versions of %s's %s: %v", owner, name, err)
		}
		return count
	}
	alone := goingCondition{ExpectNew: true}
	for _, owner := range []struct{ path, key string }{{"/api/v1/issues/" + issue, issue}, {"/api/v1/projects/DEMO", "DEMO"}} {
		t.Run(owner.path, func(t *testing.T) {
			uploadDocument(t, handler, owner.path, "held.md", "# Held\n\nThe first version.\n")
			status, answer := goingConditioned(t, handler, "bob", goingJSONSend(t, http.MethodPost, owner.path+"/artifacts",
				map[string]any{"name": "held.md", "content": "# Held\n\nWritten over.\n"}), alone)
			if status != http.StatusConflict || goingAnswerCode(t, answer) != "DOCUMENT_EXISTS" {
				t.Fatalf("an upload made a create alone over held.md answered %d %s, want 409 DOCUMENT_EXISTS", status, answer)
			}
			if count := versions(t, owner.key, "held.md"); count != 1 {
				t.Fatalf("held.md holds %d versions after the refused upload, want its one", count)
			}
			status, answer = goingConditioned(t, handler, "bob", goingJSONSend(t, http.MethodPost, owner.path+"/artifacts",
				map[string]any{"name": "fresh.md", "content": "# Fresh\n"}), alone)
			if status != http.StatusCreated {
				t.Fatalf("an upload made a create alone under a free name answered %d %s, want 201", status, answer)
			}
			status, answer = goingConditioned(t, handler, "bob", goingJSONSend(t, http.MethodPost, owner.path+"/artifacts",
				map[string]any{"name": "held.md", "content": "# Held\n"}), goingCondition{ExpectNew: true, ExpectSHA256: "00"})
			if status != http.StatusBadRequest {
				t.Fatalf("an upload made both a create alone and a write over a held content answered %d %s, want 400", status, answer)
			}
		})
	}

	t.Run("multipart", func(t *testing.T) {
		uploadDocument(t, handler, "/api/v1/issues/"+issue, "picture-notes.md", "# Notes\n")
		form := goingUpload{fields: map[string]string{"name": "picture-notes.md"}, filename: "picture-notes.md",
			contentType: "text/markdown", content: []byte("# Written over\n")}
		body, contentType, err := form.encode()
		if err != nil {
			t.Fatal(err)
		}
		status, answer := goingConditioned(t, handler, "bob", goingSend{method: http.MethodPost,
			path: "/api/v1/issues/" + issue + "/artifacts", contentType: contentType, body: body}, alone)
		if status != http.StatusConflict || goingAnswerCode(t, answer) != "DOCUMENT_EXISTS" {
			t.Fatalf("a multipart upload made a create alone over a held name answered %d %s, want 409 DOCUMENT_EXISTS", status, answer)
		}
		if count := versions(t, issue, "picture-notes.md"); count != 1 {
			t.Fatalf("picture-notes.md holds %d versions after the refused upload, want its one", count)
		}
	})
}
