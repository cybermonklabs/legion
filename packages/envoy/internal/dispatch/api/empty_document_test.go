package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// refusedWrite is the code and sentence a refused write answers.
type refusedWrite struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

// blankCreatePath is the upload route of issue key.
func blankCreatePath(key string) string { return "/api/v1/issues/" + key + "/artifacts" }

// createWithNoContent posts body, which carries a name and no content, to the issue's uploads.
func createWithNoContent(t *testing.T, handler http.Handler, key string, body map[string]any) refusedWrite {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, blankCreatePath(key), body, "alice")
	refusal := decodeBody[refusedWrite](t, response)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("a write with no content named %q: status=%d body=%s, want 400", body["name"], response.Code, response.Body.String())
	}
	return refusal
}

// A person makes a document by typing a title: a JSON body carrying a name and no content, and
// one whose content is blank, each make a new document holding nothing, which he then writes in
// Dispatch. Under the name or the slug of a document the owner already holds, such a body is
// refused and writes nothing, so a title typed a second time never empties the document that
// holds it. The refusals a write already read - a closed issue, an artifact of another kind -
// stand ahead of it, and a multipart body still needs its file.
func TestADocumentIsMadeWithNoContent(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	path := blankCreatePath(issue.Key)

	readText := func(what, slug string) string {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, path+"/"+slug+"/text", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read %s (%s): status=%d body=%s", what, slug, response.Code, response.Body.String())
		}
		return decodeBody[struct {
			Markdown string `json:"markdown"`
		}](t, response).Markdown
	}
	readDocument := func(what, slug string) model.Artifact {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, path+"/"+slug, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read %s (%s): status=%d body=%s", what, slug, response.Code, response.Body.String())
		}
		return decodeBody[model.Artifact](t, response)
	}

	for _, made := range []struct {
		name string
		body map[string]any
	}{
		{name: "Launch notes", body: map[string]any{"name": "Launch notes"}},
		{name: "Release notes", body: map[string]any{"name": "Release notes", "content": ""}},
	} {
		response := dispatchRequest(t, handler, http.MethodPost, path, made.body, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q with no content: status=%d body=%s", made.name, response.Code, response.Body.String())
		}
		document := decodeBody[uploadedArtifact](t, response)
		if document.Artifact.Kind != "doc" || document.Artifact.Name != made.name || len(document.Artifact.Versions) != 1 ||
			document.Version.Number != 1 || document.Version.Size == nil || *document.Version.Size != 0 {
			t.Fatalf("%q made = %#v version %#v, want one empty document of kind doc at version 1",
				made.name, document.Artifact, document.Version)
		}
		if markdown := readText(made.name, document.Artifact.Slug); markdown != "" {
			t.Fatalf("%q reads %q, want an empty document", made.name, markdown)
		}
	}

	held := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "Design notes", "# Design\n")
	if held.Artifact.Slug != "design-notes" {
		t.Fatalf("the document holding content = %#v, want the slug design-notes", held.Artifact)
	}
	for _, refused := range []struct {
		what, name, slug, holder, markdown string
	}{
		{what: "the name of a document holding content", name: "Design notes", slug: "design-notes", holder: "Design notes", markdown: "# Design\n"},
		{what: "the slug of that document", name: "design-notes", slug: "design-notes", holder: "Design notes", markdown: "# Design\n"},
		{what: "the name of a document whose only version is empty", name: "Launch notes", slug: "launch-notes", holder: "Launch notes"},
	} {
		refusal := createWithNoContent(t, handler, issue.Key, map[string]any{"name": refused.name})
		want := fmt.Sprintf("%q names artifact %s (%s), which exists; a write with no content makes no document and writes no version: edit it with dispatch doc-edit, or give the new document another name",
			refused.name, refused.slug, refused.holder)
		if refusal.Code != "ARTIFACT_INPUT" || refusal.Error != want {
			t.Fatalf("a write with no content under %s = %#v, want ARTIFACT_INPUT %q", refused.what, refusal, want)
		}
		if document := readDocument(refused.what, refused.slug); len(document.Versions) != 1 {
			t.Fatalf("%s holds %d versions after the refusal, want the one it had", refused.what, len(document.Versions))
		}
		if markdown := readText(refused.what, refused.slug); markdown != refused.markdown {
			t.Fatalf("%s reads %q after the refusal, want %q", refused.what, markdown, refused.markdown)
		}
	}

	// A closed issue refuses the write before it reaches the document: the refusal is the
	// issue's, not ARTIFACT_INPUT, and the document it names is untouched.
	closedResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "A closed issue",
	}, "alice")
	if closedResponse.Code != http.StatusCreated {
		t.Fatalf("create the issue to close: status=%d body=%s", closedResponse.Code, closedResponse.Body.String())
	}
	closed := decodeBody[model.Issue](t, closedResponse)
	uploadDocument(t, handler, "/api/v1/issues/"+closed.Key, "Design notes", "# Design\n")
	closeIssue(t, handler, closed.Key)
	onClosed := dispatchRequest(t, handler, http.MethodPost, blankCreatePath(closed.Key), map[string]any{"name": "Design notes"}, "alice")
	assertRefusedOnClosedIssue(t, "a write with no content", onClosed)
	if decodeBody[refusedWrite](t, onClosed).Code == "ARTIFACT_INPUT" {
		t.Fatalf("a write with no content to a closed issue reads ARTIFACT_INPUT: %s", onClosed.Body.String())
	}
	closedText := dispatchRequest(t, handler, http.MethodGet, blankCreatePath(closed.Key)+"/design-notes/text", nil, "alice")
	if closedText.Code != http.StatusOK || decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, closedText).Markdown != "# Design\n" {
		t.Fatalf("the closed issue's document after the refusal: status=%d body=%s", closedText.Code, closedText.Body.String())
	}

	// An artifact of another kind reads the kind refusal, as any upload under its name does.
	file := multipartRequest(t, handler, path, map[string]string{"name": "report.bin"}, "report.bin", "application/octet-stream", []byte("report"), "alice")
	if file.Code != http.StatusCreated {
		t.Fatalf("upload report.bin: status=%d body=%s", file.Code, file.Body.String())
	}
	stored := decodeBody[uploadedArtifact](t, file)
	onFile := createWithNoContent(t, handler, issue.Key, map[string]any{"name": "report.bin"})
	wantKind := fmt.Sprintf("%q names artifact %s (%s), whose kind is file; this upload's kind is doc, so it cannot be its next version: give it another name",
		"report.bin", stored.Artifact.Slug, stored.Artifact.Name)
	if onFile.Code != "ARTIFACT_KIND_MISMATCH" || onFile.Error != wantKind {
		t.Fatalf("a write with no content under a file's name = %#v, want ARTIFACT_KIND_MISMATCH %q", onFile, wantKind)
	}
	if document := readDocument("the file", stored.Artifact.Slug); document.Kind != "file" || len(document.Versions) != 1 {
		t.Fatalf("the file after the refusal = %#v with %d versions, want one version of kind file", document, len(document.Versions))
	}

	// A multipart body carries its bytes as its file part, and still needs one.
	noFile := multipartRequest(t, handler, path, map[string]string{"name": "No file"}, "", "", nil, "alice")
	refusal := decodeBody[refusedWrite](t, noFile)
	if noFile.Code != http.StatusBadRequest || refusal.Code != "INVALID_ARTIFACT" {
		t.Fatalf("a multipart body with no file part: status=%d body=%s, want 400 INVALID_ARTIFACT", noFile.Code, noFile.Body.String())
	}
}

// The documents Dispatch makes itself are its own: a create with no content whose minted slug is
// one of them is refused, however the title is spelled, because the slug decides and not the
// title. A title that gives another slug makes a document that plays no role, and the role
// document written with content is still the issue's.
func TestAContentFreeCreateTakesNoDocumentRole(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createArtifactIssue(t, handler)
	ctx := context.Background()
	documents := func(what string) int {
		t.Helper()
		var count int
		if err := database.Pool.QueryRow(ctx, `select count(*) from artifacts where issue_key = $1`, issue.Key).Scan(&count); err != nil {
			t.Fatalf("count the issue's documents %s: %v", what, err)
		}
		return count
	}

	before := documents("before the refusals")
	for _, name := range []string{"plan.md", "Plan.md", "Plan md", "plan_md"} {
		refusal := createWithNoContent(t, handler, issue.Key, map[string]any{"name": name})
		want := fmt.Sprintf("%q gives the slug plan-md, the slug of the issue's plan document, which Dispatch makes itself; give this document another name", name)
		if refusal.Code != "ARTIFACT_ROLE_NAME" || refusal.Error != want {
			t.Fatalf("a create with no content named %q = %#v, want ARTIFACT_ROLE_NAME %q", name, refusal, want)
		}
		if after := documents("after " + name); after != before {
			t.Fatalf("the issue holds %d documents after the refusal of %q, want %d", after, name, before)
		}
	}

	plainResponse := dispatchRequest(t, handler, http.MethodPost, blankCreatePath(issue.Key), map[string]any{"name": "Plan"}, "alice")
	if plainResponse.Code != http.StatusCreated {
		t.Fatalf(`create "Plan" with no content: status=%d body=%s`, plainResponse.Code, plainResponse.Body.String())
	}
	plain := decodeBody[uploadedArtifact](t, plainResponse)
	if plain.Artifact.Slug != "plan" || plain.Artifact.Role != "" {
		t.Fatalf(`"Plan" made = %#v, want the slug plan and no role`, plain.Artifact)
	}

	written := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "plan.md", "# Plan\n")
	if written.Artifact.Slug != "plan-md" || written.Artifact.Role != model.ArtifactRolePlan {
		t.Fatalf("plan.md written with content = %#v, want the slug plan-md playing the plan role", written.Artifact)
	}
	if roles := readIssueRoleDocuments(t, handler, issue.Key); roles.PlanArtifactID != written.Artifact.ID {
		t.Fatalf("the issue's plan document = %q, want %q, the one written with content", roles.PlanArtifactID, written.Artifact.ID)
	}
}

// A document made with no content is a document from the moment it is made: a node of the graph
// attached to its issue, on the issue's document list, with the row and the event an uploaded
// document has. Only its content differs, and a person writes that in Dispatch.
func TestANewEmptyDocumentIsANodeOfItsIssue(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createArtifactIssue(t, handler)
	owner := "/api/v1/issues/" + issue.Key
	ctx := context.Background()

	response := dispatchRequest(t, handler, http.MethodPost, owner+"/artifacts", map[string]any{"name": "Launch notes"}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create a document with no content: status=%d body=%s", response.Code, response.Body.String())
	}
	empty := decodeBody[uploadedArtifact](t, response)
	uploaded := uploadDocument(t, handler, owner, "Design notes", "# Design\n")

	issueRef := "dispatch://" + issue.Key
	emptyRef := issueRef + "/artifact/" + empty.Artifact.Slug
	node := readNode(t, handler, emptyRef)
	if node.Kind != "artifact" || node.ID != empty.Artifact.ID ||
		node.Parent == nil || *node.Parent != (model.NodeParent{Kind: "issue", ID: issue.Key, Ref: issueRef}) {
		t.Fatalf("the new document's node = %#v with parent %s, want an artifact whose parent is the issue", node, factsOf(node))
	}
	links := graphEdges(t, handler, url.Values{"from": {emptyRef}})
	if len(links.Edges) != 1 || links.Edges[0].Kind != "attached_to" || links.Edges[0].Direction != "out" ||
		links.Edges[0].Node.Ref != issueRef {
		t.Fatalf("the new document's edges = %#v, want one attached_to its issue", links.Edges)
	}
	attached := 0
	for _, edge := range graphEdges(t, handler, url.Values{"to": {issueRef}}).Edges {
		if edge.Kind == "attached_to" && edge.Node.ID == empty.Artifact.ID {
			attached++
		}
	}
	if attached != 1 {
		t.Fatalf("the issue holds %d attached_to edges from the new document, want one", attached)
	}

	list := dispatchRequest(t, handler, http.MethodGet, owner+"/artifacts", nil, "alice")
	if list.Code != http.StatusOK {
		t.Fatalf("list the issue's documents: status=%d body=%s", list.Code, list.Body.String())
	}
	listed := false
	for _, artifact := range decodeBody[[]model.Artifact](t, list) {
		if artifact.ID == empty.Artifact.ID {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("the issue's document list does not hold %s: %s", empty.Artifact.ID, list.Body.String())
	}

	// The two rows differ in what the documents hold and in nothing else. Each row is read as
	// JSON without the columns that name it (its id, slug, name, reference key and time) and
	// without the content: a version's markdown and the search vector built from it.
	rowOf := func(what, query, id string) string {
		t.Helper()
		var row string
		if err := database.Pool.QueryRow(ctx, query, id).Scan(&row); err != nil {
			t.Fatalf("read the %s row of %s: %v", what, id, err)
		}
		return row
	}
	const artifactRow = `select (to_jsonb(a) - 'id' - 'slug' - 'name' - 'ref_key' - 'created_at')::text from artifacts a where a.id = $1`
	const versionRow = `select (to_jsonb(v) - 'id' - 'artifact_id' - 'created_at' - 'markdown' - 'search')::text
		from artifact_versions v where v.artifact_id = $1`
	for _, row := range []struct {
		what, query string
	}{{what: "artifacts", query: artifactRow}, {what: "artifact_versions", query: versionRow}} {
		made := rowOf(row.what, row.query, empty.Artifact.ID)
		upload := rowOf(row.what, row.query, uploaded.Artifact.ID)
		if made != upload {
			t.Fatalf("the %s row of the document made with no content is\n%s\nand of the uploaded document\n%s", row.what, made, upload)
		}
	}

	for _, document := range []struct {
		what, id string
	}{{what: "the document made with no content", id: empty.Artifact.ID}, {what: "the uploaded document", id: uploaded.Artifact.ID}} {
		var events int
		if err := database.Pool.QueryRow(ctx, `
			select count(*) from events
			where issue_key = $1 and type = 'artifact.created' and payload->'artifact'->>'id' = $2
		`, issue.Key, document.id).Scan(&events); err != nil {
			t.Fatalf("count the artifact.created events of %s: %v", document.what, err)
		}
		if events != 1 {
			t.Fatalf("%s carries %d artifact.created events, want one", document.what, events)
		}
	}
}
