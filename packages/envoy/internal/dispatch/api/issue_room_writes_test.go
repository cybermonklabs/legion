package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// holdIssuesUnconverted makes a process that lists no issue_room live beside the test's server for
// the rest of the test - a server_processes row with no capabilities, as a build before the column
// registers none - so every issue the test creates is created as before the issue room, one room
// per document (decision 7 of the issue-tree plan), and answers the process's instance, which a
// test that holds it for one create takes back (holdUnconverted). The process registers this
// build's schema and this build's owner lock protocol, so it moves no other rule the fleet decides:
// the ask roll window, which a process of an older schema opens (docs' anotherBuildServes), stays
// shut as with this server alone, and the owner locks stay in this build's mode
// (requireNoLegacyOwnerLockMark).
func holdIssuesUnconverted(t *testing.T, database *store.Store) (instance string) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		insert into server_processes (instance, signing_keys, schema_version, schema_creatable, heartbeat_at, owner_lock_protocol)
		values (gen_random_uuid(), '{fingerprint}', $1, $2, now() + interval '1 day', $3) returning instance::text`,
		pmdoc.SchemaVersion(), pmdoc.SchemaCreatable(), store.OwnerLockProtocol).Scan(&instance); err != nil {
		t.Fatalf("add a live server process that lists no issue room: %v", err)
	}
	requireNoLegacyOwnerLockMark(t, database)
	return instance
}

// requireNoLegacyOwnerLockMark fails the test once the database holds the legacy owner lock mark
// (owner_lock_legacy_mark), which the insert of a server_processes row of owner lock protocol 0,
// the column's default, stamps: every owner lock of the database then runs in the
// advisory-and-row mode while the mark is younger than store.OwnerLockMarkWindow, an hour, after
// the row itself is gone. A process a test registers as this build, or as the release before,
// which registers the same protocol, names store.OwnerLockProtocol and stamps none.
func requireNoLegacyOwnerLockMark(t *testing.T, database *store.Store) {
	t.Helper()
	var marked bool
	if err := database.Pool.QueryRow(context.Background(), `select exists (select from owner_lock_legacy_mark)`).Scan(&marked); err != nil {
		t.Fatalf("read the legacy owner lock mark: %v", err)
	}
	if marked {
		t.Fatalf("the database holds the legacy owner lock mark, so every owner lock runs in the advisory-and-row mode for %s: a process the test registered names owner lock protocol 0, want %d", store.OwnerLockMarkWindow, store.OwnerLockProtocol)
	}
}

// createRoomIssue creates, as alice, an issue of project with spec, and answers it with its tree
// artifact's id, "" where it holds none.
func createRoomIssue(t *testing.T, handler http.Handler, database *store.Store, project, title, spec, parent string) (model.Issue, string) {
	t.Helper()
	body := map[string]any{"project": project, "title": title, "spec": spec, "force": true}
	if parent != "" {
		body["parent"] = parent
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue %q: status=%d body=%s", title, response.Code, response.Body.String())
	}
	issue := decodeBody[model.Issue](t, response)
	tree, err := issueTreeOf(context.Background(), database.Pool, issue.Key)
	if err != nil {
		t.Fatalf("read the tree of %s: %v", issue.Key, err)
	}
	return issue, tree
}

// createRoomProject creates project as alice.
func createRoomProject(t *testing.T, handler http.Handler, project string) {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": project, "name": project}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project %s: status=%d body=%s", project, response.Code, response.Body.String())
	}
}

// artifactTextOf reads GET /api/v1/artifacts/{id}/text's markdown.
func artifactTextOf(t *testing.T, handler http.Handler, id string) string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+id+"/text", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the text of %s: status=%d body=%s", id, response.Code, response.Body.String())
	}
	return decodeBody[artifactText](t, response).Markdown
}

// documentRoomOf is the room artifactID's document is edited in (docs.DocumentRoomOfStatement) and
// the document's fragment of that room, the room and fragment the artifact read answers
// (model.Artifact.Room, Fragment): a test that reads a document's stored history reads its room's,
// and renders the document through its own fragment.
func documentRoomOf(t *testing.T, q queryer, artifactID string) (room, fragment string) {
	t.Helper()
	if err := q.QueryRow(context.Background(), docs.DocumentRoomOfStatement, artifactID).Scan(&room); err != nil {
		t.Fatalf("read the room of %s: %v", artifactID, err)
	}
	if room == artifactID {
		return room, model.ArtifactFragmentOwn
	}
	return room, artifactID
}

// A criterion written through POST /api/v1/artifacts/{tree}/edits on a converted issue's tree
// artifact lands in the tree's own fragment of the issue's room and reads back through
// GET /api/v1/artifacts/{tree}/text and through the tree read, GET /api/v1/issues/{key}/tree, as
// the issue's criterion node keyed by the tree; the tree artifact reads as a document whose room is
// itself and whose fragment is the room's own, and the issue read's documents name no tree
// artifact (R8 of the issue-tree spec).
func TestACriterionWrittenThroughEditsOnTheTreeArtifactReadsBackInTheTreeRead(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createRoomProject(t, handler, "TREE")
	issue, tree := createRoomIssue(t, handler, database, "TREE", "An issue with a tree", "The spec's words.\n", "")
	if tree == "" {
		t.Fatalf("issue %s was created with no tree while no live process lacks the issue room", issue.Key)
	}

	const criterion = ":::criterion{#k1}\nI want the urgent asks first.\n:::\n"
	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+tree+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": criterion}},
	}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("write a criterion into the tree: status=%d body=%s", edited.Code, edited.Body.String())
	}
	if text := artifactTextOf(t, handler, tree); !strings.Contains(text, "I want the urgent asks first.") {
		t.Errorf("the tree's text reads %q, want the criterion it was written", text)
	}
	if text := artifactTextOf(t, handler, issue.PrimaryArtifactID); strings.Contains(text, "urgent asks") || !strings.Contains(text, "The spec's words.") {
		t.Errorf("the spec's text reads %q, want its own words and none of the tree's", text)
	}
	criterionNode := blockKeyOf(t, database, tree) + "#k1"
	if criteria := treeRead(t, handler, issue.Key, "", "alice", false).supertagged("criterion"); len(criteria) != 1 ||
		criteria[0].ID != criterionNode || !strings.Contains(criteria[0].Text, "I want the urgent asks first.") {
		t.Errorf("the tree read of %s answers the criteria %+v, want the one node %s holding the words it was written", issue.Key, criteria, criterionNode)
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+tree, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the tree artifact: status=%d body=%s", read.Code, read.Body.String())
	}
	if artifact := decodeBody[model.Artifact](t, read); artifact.Kind != "tree" || artifact.Room != tree || artifact.Fragment != model.ArtifactFragmentOwn {
		t.Errorf("the tree reads kind %q room %q fragment %q, want kind tree, room %s, fragment %s", artifact.Kind, artifact.Room, artifact.Fragment, tree, model.ArtifactFragmentOwn)
	}

	detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice")
	if detail.Code != http.StatusOK {
		t.Fatalf("read the issue: status=%d body=%s", detail.Code, detail.Body.String())
	}
	documents := decodeBody[struct {
		Artifacts []model.Artifact `json:"artifacts"`
	}](t, detail).Artifacts
	for _, document := range documents {
		if document.Kind == "tree" || document.ID == tree {
			t.Errorf("the issue read's documents name the tree artifact %s", document.ID)
		}
		if document.ID == issue.PrimaryArtifactID && (document.Room != tree || document.Fragment != document.ID) {
			t.Errorf("the spec reads room %q fragment %q, want the issue's room %s and its own fragment %s", document.Room, document.Fragment, tree, document.ID)
		}
	}
}

// An issue created while no live process lacks the issue room is born converted: its spec has its
// document_rooms row naming the issue's tree from its first write, which is stored in the room,
// none under the spec's own id; the spec is a query node of query spec; and the issue holds its
// decisions query node, a document with an empty version 1 whose role reads decisions, which
// the decisions-tree spec's decisions step does not select (decisions_documents()). One created
// while a process of the release before is live is created as today, with no tree and no
// document_rooms row (decision 7 of the issue-tree plan).
func TestAnIssueIsBornConvertedOnlyWhileEveryLiveProcessListsTheIssueRoom(t *testing.T) {
	ctx := context.Background()
	handler, database, _ := newTestServer(t, testServerOptions{})
	createRoomProject(t, handler, "BORN")
	// A live process of this build, which lists the issue room as a server registers it
	// (processes.Register), so the create reads a fleet that serves the room rather than an empty one.
	if _, err := database.Pool.Exec(ctx, `
		insert into server_processes (instance, signing_keys, schema_version, schema_creatable, heartbeat_at, capabilities, owner_lock_protocol)
		values (gen_random_uuid(), '{fingerprint}', $1, $2, now() + interval '1 day', $3::text[], $4)`,
		pmdoc.SchemaVersion(), pmdoc.SchemaCreatable(), processes.Capabilities, store.OwnerLockProtocol); err != nil {
		t.Fatalf("add a live server process that lists the issue room: %v", err)
	}
	requireNoLegacyOwnerLockMark(t, database)
	converted, tree := createRoomIssue(t, handler, database, "BORN", "Born converted", "The first spec.\n", "")
	if tree == "" {
		t.Fatalf("issue %s holds no tree", converted.Key)
	}
	var room *string
	var ownUpdates, roomUpdates int
	if err := database.Pool.QueryRow(ctx, `
		select (select room::text from document_rooms where artifact_id = $1::uuid),
		       (select count(*) from doc_updates where artifact_id = $1::uuid),
		       (select count(*) from doc_updates where artifact_id = $2::uuid)`,
		converted.PrimaryArtifactID, tree).Scan(&room, &ownUpdates, &roomUpdates); err != nil {
		t.Fatalf("read the spec's room: %v", err)
	}
	if room == nil || *room != tree || ownUpdates != 0 || roomUpdates == 0 {
		t.Errorf("the spec's room is %v with %d updates of its own and the tree %d, want %s and every update in the room", room, ownUpdates, roomUpdates, tree)
	}
	if text := artifactTextOf(t, handler, converted.PrimaryArtifactID); !strings.Contains(text, "The first spec.") {
		t.Errorf("the spec reads %q, want its first text", text)
	}
	specKey := converted.Key + "/spec"
	if query, held, err := graphstore.CurrentValue(ctx, database.Pool, graphstore.KindArtifact, specKey, graphstore.AttributeQuery); err != nil || !held || query != graphstore.QuerySpec {
		t.Errorf("the spec's node states query %q (held %t, %v), want %s", query, held, err, graphstore.QuerySpec)
	}
	var decisions, decisionsMarkdown string
	var decisionsRole *string
	if err := database.Pool.QueryRow(ctx, `
		select a.id::text, a.role, v.markdown from artifacts a join artifact_versions v on v.artifact_id = a.id and v.number = 1
		where a.issue_key = $1 and a.slug = $2`, converted.Key, issueDecisionsName).Scan(&decisions, &decisionsRole, &decisionsMarkdown); err != nil {
		t.Fatalf("read the decisions query node: %v", err)
	}
	if decisionsRole != nil || decisionsMarkdown != "" {
		t.Errorf("the decisions query's row holds role %v and version 1 %q, want no role and an empty version", decisionsRole, decisionsMarkdown)
	}
	readDecisions := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+decisions, nil, "alice")
	if artifact := decodeBody[model.Artifact](t, readDecisions); artifact.Role != graphstore.QueryDecisions || artifact.Room != decisions || artifact.Fragment != "" {
		t.Errorf("the decisions query reads role %q room %q fragment %q, want role decisions, no room of an issue and no fragment", artifact.Role, artifact.Room, artifact.Fragment)
	}
	var selected int
	if err := database.Pool.QueryRow(ctx, `select count(*) from decisions_documents() where issue_key = $1`, converted.Key).Scan(&selected); err != nil {
		t.Fatalf("read the decisions documents: %v", err)
	}
	if selected != 0 {
		t.Errorf("the decisions step selects %d documents of %s, want none", selected, converted.Key)
	}

	holdIssuesUnconverted(t, database)
	unconverted, unconvertedTree := createRoomIssue(t, handler, database, "BORN", "Created as before", "The second spec.\n", "")
	var rooms, ownSeed int
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from document_rooms d join artifacts a on a.id = d.artifact_id where a.issue_key = $1),
		       (select count(*) from doc_updates where artifact_id = $2::uuid)`, unconverted.Key, unconverted.PrimaryArtifactID).Scan(&rooms, &ownSeed); err != nil {
		t.Fatalf("read the unconverted issue's rooms: %v", err)
	}
	if unconvertedTree != "" || rooms != 0 || ownSeed == 0 {
		t.Errorf("issue %s created beside a process of the release before holds tree %q, %d document_rooms rows and %d updates of the spec's own, want no tree, none and its seed", unconverted.Key, unconvertedTree, rooms, ownSeed)
	}
}

// A plan.md uploaded to a converted issue lands in its own fragment of the issue's room, with its
// document_rooms row from its first write and no role in its row, the role read from its node's
// query value; its text reads back; a later upload of its name and an edit of it each reach that
// fragment alone, leaving the spec's text as it was; and the room's pass the upload runs causes
// nothing of the spec's blocks under the upload's event: an unchanged sibling fragment's facts
// append nothing.
func TestAPlanUploadedToAConvertedIssueLandsInItsFragment(t *testing.T) {
	ctx := context.Background()
	handler, database, _ := newTestServer(t, testServerOptions{})
	createRoomProject(t, handler, "PLAN")
	// The spec's blocks carry their ids from the create, so the create's own pass indexes them, and
	// what a later pass states of them is a restatement or nothing.
	issue, tree := createRoomIssue(t, handler, database, "PLAN", "A converted issue",
		":::criterion{#k1}\nThe spec paragraph.\n:::\n\n:::criterion{#k2}\nA second spec paragraph.\n:::\n", "")
	if tree == "" {
		t.Fatalf("issue %s holds no tree", issue.Key)
	}
	upload := func(content string) model.Artifact {
		t.Helper()
		return uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "plan.md", content).Artifact
	}
	plan := upload("The first plan.\n")
	if plan.Room != tree || plan.Fragment != plan.ID || plan.Role != model.ArtifactRolePlan {
		t.Errorf("the upload answers room %q fragment %q role %q, want room %s, fragment %s, role plan", plan.Room, plan.Fragment, plan.Role, tree, plan.ID)
	}
	var room *string
	var role *string
	var ownUpdates int
	if err := database.Pool.QueryRow(ctx, `
		select (select room::text from document_rooms where artifact_id = a.id), a.role,
		       (select count(*) from doc_updates where artifact_id = a.id)
		from artifacts a where a.id = $1::uuid`, plan.ID).Scan(&room, &role, &ownUpdates); err != nil {
		t.Fatalf("read the plan's room: %v", err)
	}
	if room == nil || *room != tree || role != nil || ownUpdates != 0 {
		t.Errorf("the plan's row holds room %v, role %v and %d updates of its own, want room %s, no role and none", room, role, ownUpdates, tree)
	}
	if text := artifactTextOf(t, handler, plan.ID); !strings.Contains(text, "The first plan.") {
		t.Errorf("the plan reads %q, want its first text", text)
	}

	// The upload's pass over the room states nothing of the spec's unchanged blocks.
	var event int64
	if err := database.Pool.QueryRow(ctx, `
		select id from events where type = 'artifact.created' and payload->'artifact'->>'id' = $1`, plan.ID).Scan(&event); err != nil {
		t.Fatalf("read the upload's event: %v", err)
	}
	var specFacts, specBlocks int
	var specKinds string
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from graph_tuples t
		        join issue_room_blocks b on t.from_id = $3 || '#' || b.block_id
		        where b.tree_artifact_id = $1::uuid and b.document_id = $2::uuid and t.source_event = $4),
		       (select coalesce(string_agg(distinct t.kind, ','), '') from graph_tuples t
		        join issue_room_blocks b on t.from_id = $3 || '#' || b.block_id
		        where b.tree_artifact_id = $1::uuid and b.document_id = $2::uuid and t.source_event = $4),
		       (select count(*) from issue_room_blocks b where b.tree_artifact_id = $1::uuid and b.document_id = $2::uuid)`,
		tree, issue.PrimaryArtifactID, issue.Key+"/tree", event).Scan(&specFacts, &specKinds, &specBlocks); err != nil {
		t.Fatalf("read what the upload's pass stated of the spec: %v", err)
	}
	if specBlocks == 0 || specFacts != 0 {
		t.Errorf("the room holds %d blocks of the spec and the upload's event caused %d facts of them (%s), want the spec's blocks and none", specBlocks, specFacts, specKinds)
	}

	again := upload("The plan, written again.\n")
	if again.ID != plan.ID {
		t.Fatalf("the second upload of plan.md made %s, want the plan %s's next version", again.ID, plan.ID)
	}
	if text := artifactTextOf(t, handler, plan.ID); !strings.Contains(text, "written again") || strings.Contains(text, "first plan") {
		t.Errorf("the plan reads %q after its second upload, want the second text alone", text)
	}
	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+plan.ID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "replace", "find": "written again", "with": "edited in place"}},
	}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("edit the plan: status=%d body=%s", edited.Code, edited.Body.String())
	}
	if text := artifactTextOf(t, handler, plan.ID); !strings.Contains(text, "edited in place") {
		t.Errorf("the plan reads %q after its edit, want the edit", text)
	}
	if text := artifactTextOf(t, handler, issue.PrimaryArtifactID); !strings.Contains(text, "The spec paragraph.") || strings.Contains(text, "plan") {
		t.Errorf("the spec reads %q, want its own text untouched by the plan's writes", text)
	}
}

// childCriterionUnderParent creates, on a room project of its own, a parent issue and its child,
// each born converted, and writes the criterion k1 into the child's own room as alice. It answers
// the handler, both issues, their tree artifacts and the store address of the criterion's node.
func childCriterionUnderParent(t *testing.T, project string) (handler http.Handler, parent, child model.Issue, parentTree, childTree, address string) {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{})
	createRoomProject(t, handler, project)
	parent, parentTree = createRoomIssue(t, handler, database, project, "The parent", "The parent's spec.\n", "")
	child, childTree = createRoomIssue(t, handler, database, project, "The child", "The child's spec.\n", parent.Key)
	if parentTree == "" || childTree == "" {
		t.Fatalf("the parent's tree is %q and the child's %q, want both", parentTree, childTree)
	}
	if written := editDocument(t, handler, childTree, insertAtEnd(":::criterion{#k1}\nThe child's criterion.\n:::\n")); written.Code != http.StatusOK {
		t.Fatalf("write the child's criterion: status=%d body=%s", written.Code, written.Body.String())
	}
	if err := database.Pool.QueryRow(context.Background(), `
		select n.id from graph_nodes n
		where n.kind = 'block' and n.id = $1 || '#k1'`, child.Key+"/tree").Scan(&address); err != nil {
		t.Fatalf("read the child's criterion node: %v", err)
	}
	return handler, parent, child, parentTree, childTree, address
}

// A document write addressed through a parent issue that names, by its store address, a node of
// its child issue - a criterion written into the child's own room - is refused 409
// NODE_IN_OTHER_ISSUE naming the child, and writes nothing (D3 of the issue-tree plan).
func TestAWriteThroughTheParentNamingAChildsNodeIsRefused(t *testing.T) {
	handler, parent, child, parentTree, childTree, address := childCriterionUnderParent(t, "FAM")
	before := artifactTextOf(t, handler, parentTree)
	refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+parentTree+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "delete", "block": address}},
	}, "alice")
	refusal := decodeBody[refusalAnswer](t, refused)
	if refused.Code != http.StatusConflict || refusal.Code != codeNodeInOtherIssue || !strings.Contains(refusal.Error, child.Key) {
		t.Errorf("a delete through %s naming %s answered %d %s %q, want 409 %s naming %s", parent.Key, address, refused.Code, refusal.Code, refusal.Error, codeNodeInOtherIssue, child.Key)
	}
	if after := artifactTextOf(t, handler, parentTree); after != before {
		t.Errorf("the parent's tree reads %q after the refused write, want %q", after, before)
	}
	if text := artifactTextOf(t, handler, childTree); !strings.Contains(text, "The child's criterion.") {
		t.Errorf("the child's tree reads %q, want its criterion still there", text)
	}
}

// A document of a converted issue that plays no role is a room of its own (R5 of the issue-tree
// spec), so it is judged for removal, and removed, as a document of an unconverted issue is, and
// the issue's room, which holds the spec and the plan, stands as it was: one a person typed in,
// whose edit no version holds yet, is refused as not untouched and keeps what was typed, while an
// untouched one goes with its own room's history, and the issue's room takes no update of the
// removal and keeps its documents and their text.
func TestARemovalOfAConvertedIssuesDocumentLeavesTheIssueRoomAsItStood(t *testing.T) {
	ctx := context.Background()
	// Settlement is held back, so the typed edit is still unsettled when the removal reads it.
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createRoomProject(t, handler, "GONE")
	issue, tree := createRoomIssue(t, handler, database, "GONE", "A converted issue", "The spec's words.\n", "")
	if tree == "" {
		t.Fatalf("issue %s holds no tree", issue.Key)
	}
	owner := "/api/v1/issues/" + issue.Key
	plan := uploadDocument(t, handler, owner, "plan.md", "The plan's words.\n").Artifact
	if plan.Room != tree || plan.Fragment != plan.ID {
		t.Fatalf("the plan reads room %q fragment %q, want the issue's room %s and its own fragment %s", plan.Room, plan.Fragment, tree, plan.ID)
	}
	sibling := uploadDocument(t, handler, owner, "Sibling notes", "The sibling's words.\n").Artifact
	typed := emptyDocument(t, handler, issue.Key, "Typed notes")
	untouched := emptyDocument(t, handler, issue.Key, "Untouched notes")
	for _, document := range []model.Artifact{sibling, typed, untouched} {
		requireOwnRoom(t, document)
	}
	if _, err := replaceDocumentText(database, deps.Docs, typed.ID, "The typed words.\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("type into the document's room: %v", err)
	}
	assertRemovalRefused(t, handler, "a document of its own room whose edit has not settled", typed,
		http.StatusConflict, "ARTIFACT_NOT_UNTOUCHED", notUntouchedSentence(typed, "an edit of it has not settled"))
	if text := artifactTextOf(t, handler, typed.ID); !strings.Contains(text, "The typed words.") {
		t.Errorf("the typed document reads %q after its refused removal, want what was typed", text)
	}

	roomUpdates := documentAppendCount(t, database, tree)
	removedDocument(t, handler, untouched.ID)
	assertNoRowNamesTheDocument(t, database, untouched.ID)
	if held := documentAppendCount(t, database, tree); held != roomUpdates {
		t.Errorf("the issue's room holds %d stored updates after the removal, %d before it: the removal of a document of its own room wrote into the issue's room", held, roomUpdates)
	}
	var joined []string
	rows, err := database.Pool.Query(ctx, `select artifact_id::text from document_rooms where room = $1::uuid order by artifact_id`, tree)
	if err != nil {
		t.Fatalf("read the room's documents: %v", err)
	}
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			t.Fatalf("read a document of the room: %v", err)
		}
		joined = append(joined, document)
	}
	rows.Close()
	if want := slices.Sorted(slices.Values([]string{issue.PrimaryArtifactID, plan.ID})); !slices.Equal(joined, want) {
		t.Errorf("the room's documents are %v after the removal, want the spec and the plan alone, %v", joined, want)
	}
	for document, want := range map[string]string{
		issue.PrimaryArtifactID: "The spec's words.", plan.ID: "The plan's words.", sibling.ID: "The sibling's words.", typed.ID: "The typed words.",
	} {
		if text := artifactTextOf(t, handler, document); !strings.Contains(text, want) {
			t.Errorf("%s reads %q after its sibling's removal, want %q", document, text, want)
		}
	}
}

// A document of a converted issue that plays no role is a room of its own (R5 of the issue-tree
// spec), and so is a document that cites it: an upload citing it, made while the spec's fragment of
// the issue's room holds an edit no version holds yet, is indexed by its own request, since no
// other fragment shares its room, so the removal of the cited document is refused at once for the
// citation, as on an unconverted issue, and never as one the issue room's settlement owes.
func TestARemovalOfADocumentCitedWhileTheIssueRoomHoldsAnUnsettledEditIsRefusedForTheCitation(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createRoomProject(t, handler, "CITE")
	issue, tree := createRoomIssue(t, handler, database, "CITE", "A converted issue", "The spec's words.\n", "")
	if tree == "" {
		t.Fatalf("issue %s holds no tree", issue.Key)
	}
	cited := emptyDocument(t, handler, issue.Key, "Cited notes")
	requireOwnRoom(t, cited)
	if _, err := replaceDocumentText(database, deps.Docs, issue.PrimaryArtifactID, "The spec's words, edited.\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("edit the spec's fragment: %v", err)
	}
	citing := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "Citing notes", "See dispatch://"+issue.Key+"/artifact/"+cited.Slug+" for the rest.\n").Artifact
	requireOwnRoom(t, citing)
	if indexed := blockIndexVersion(t, database, citing.ID); indexed == nil || *indexed != 1 {
		t.Fatalf("the citing upload's blocks are indexed at %v, want version 1, by its own request", indexed)
	}
	assertRemovalRefused(t, handler, "a document an upload cites while the issue's room holds an unsettled edit", cited,
		http.StatusConflict, "ARTIFACT_NOT_UNTOUCHED", notUntouchedSentence(cited, "something in Dispatch links to it"))
}

// An ask block of a converted document is that document's own ask: settlement indexes it keyed by
// the issue's tree, whose room holds the block, and every read of it names the document whose
// fragment holds the block. The ask read names the spec as its block's document; the spec's text
// read shows the block as the spec's own ask, as no copy of another document's (its asks member,
// where the text routes answer an ask's state and its copy: agentText); and an answer to it leaves
// no settlement owed under any document of the room, whose settlement is keyed by, and clears, the
// room's own row.
func TestAConvertedDocumentsAskIsItsOwnAndItsAnswerOwesNoCopy(t *testing.T) {
	ctx := context.Background()
	handler, database := blockAskHandler(t)
	issue := createInteractionIssue(t, handler, "OWN", "A converted ask",
		"Context\n\n:::ask{#ship-it urgency=\"med\" multiple=\"false\"}\nShip it?\n\n- Yes: Now.\n- No: Later.\n:::\n")
	tree, err := issueTreeOf(ctx, database.Pool, issue.Key)
	if err != nil || tree == "" {
		t.Fatalf("the tree of %s is %q (%v), want the issue born converted", issue.Key, tree, err)
	}
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "ship-it", "Ship it?")
	askID := blockAskID(t, handler, issue.Key, "ship-it")
	var key string
	if err := database.Pool.QueryRow(ctx, `select block_artifact_id::text from asks where id = $1`, askID).Scan(&key); err != nil || key != tree {
		t.Fatalf("the ask's row is keyed by %q (%v), want the issue's tree %s", key, err, tree)
	}
	if ask := readBlockAsk(t, handler, askID); ask.BlockArtifact == nil || ask.BlockArtifact.ID != issue.PrimaryArtifactID {
		t.Fatalf("the ask read names %#v as its block's document, want the spec %s", ask.BlockArtifact, issue.PrimaryArtifactID)
	}
	if text := agentText(t, handler, issue.PrimaryArtifactID); !strings.Contains(text, ":::ask{#ship-it") || strings.Contains(text, "copied_from") {
		t.Fatalf("the spec's text read shows its own ask as a copy, or not at all:\n%s", text)
	}
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{"selected": []string{"Yes"}}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer the ask: status=%d body=%s", answered.Code, answered.Body.String())
	}
	var owed []string
	if err := database.Pool.QueryRow(ctx, `
		select coalesce(array_agg(p.artifact_id::text order by p.artifact_id), '{}')
		from doc_settlements_pending p join artifacts a on a.id = p.artifact_id
		where a.issue_key = $1 and a.kind <> 'tree'`, issue.Key).Scan(&owed); err != nil {
		t.Fatalf("read the issue's pending settlements: %v", err)
	}
	if len(owed) != 0 {
		t.Fatalf("the answer left settlements owed under the documents %v, which the room's settlement never clears", owed)
	}
	if text := agentText(t, handler, issue.PrimaryArtifactID); !strings.Contains(text, `#ship-it state="answered"`) || strings.Contains(text, "copied_from") {
		t.Fatalf("the spec's text read after the answer shows no answered ask of its own:\n%s", text)
	}
}

// A copy of the spec uploaded into its converted issue plays no role, so it is a room of its own
// beside the issue's room, which holds the spec (R5 of the issue-tree spec): it keeps the spec's
// block ids, and its ask block is a copy of the spec's ask, which opens no ask of its own and shows
// its source's (TestAnUploadedCopyOfASpecOpensNoAskForItsCopiedBlocks reads the states), while the
// issue's room holds the spec alone.
func TestACopyOfASpecUploadedIntoItsConvertedIssueIsARoomOfItsOwnAndOpensNoAsk(t *testing.T) {
	ctx := context.Background()
	handler, database := blockAskHandler(t)
	spec := "Context\n\n:::ask{#rec-open urgency=\"med\" multiple=\"false\"}\nWhich region?\n:::\n"
	issue := createInteractionIssue(t, handler, "COPY", "A copy in its own room", spec)
	tree, err := issueTreeOf(ctx, database.Pool, issue.Key)
	if err != nil || tree == "" {
		t.Fatalf("the tree of %s is %q (%v), want the issue born converted", issue.Key, tree, err)
	}
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "rec-open", "Which region?")
	source := blockAskID(t, handler, issue.Key, "rec-open")
	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec-record.md", "content": artifactTextOf(t, handler, issue.PrimaryArtifactID),
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the copy: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	copied := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact
	requireOwnRoom(t, copied)
	// The copy's settlement writes its copied_from, which the text read answers in the block's
	// entry of its asks member (agentText).
	awaitTextShows(t, handler, copied.ID, "the copy of the spec's open ask", contains(
		`#rec-open state="open" copied_from="`+source+`"`,
	), nil)
	asks := decodeBody[[]model.Ask](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks?state=all", nil, "alice"))
	if len(asks) != 1 || asks[0].ID != source {
		t.Fatalf("the issue holds the asks %#v, want the spec's %s alone: the copy opens none of its own", asks, source)
	}
	var joined []string
	if err := database.Pool.QueryRow(ctx, `select coalesce(array_agg(artifact_id::text), '{}') from document_rooms where room = $1::uuid`, tree).Scan(&joined); err != nil {
		t.Fatalf("read the room's documents: %v", err)
	}
	if !slices.Equal(joined, []string{issue.PrimaryArtifactID}) {
		t.Errorf("the issue's room holds the documents %v, want the spec %s alone", joined, issue.PrimaryArtifactID)
	}
}

// An issue's decisions query (D6 of the issue-tree plan) renders the issue's decisions and is held
// by no fragment of any room, so nothing writes text into it: an upload under its name and an edit
// by its id and by its slug are 409 DECISIONS_QUERY and write no version and no update, the
// artifact read answers it no fragment, and a socket on it is refused 409 DECISIONS_QUERY before
// any room opens.
func TestTheDecisionsQueryTakesNoUploadNoEditAndNoSocket(t *testing.T) {
	ctx := context.Background()
	handler, database, deps := newTestServer(t, testServerOptions{browserPeers: true})
	createRoomProject(t, handler, "QRY")
	issue, tree := createRoomIssue(t, handler, database, "QRY", "The decisions query", "The spec.\n", "")
	if tree == "" {
		t.Fatalf("issue %s holds no tree, want it born converted", issue.Key)
	}
	var id, slug string
	if err := database.Pool.QueryRow(ctx, `select id::text, slug from artifacts where issue_key = $1 and name = $2`,
		issue.Key, issueDecisionsName).Scan(&id, &slug); err != nil {
		t.Fatalf("read the decisions query of %s: %v", issue.Key, err)
	}
	// Its empty version 1 is indexed in the create's own transaction, so no reconciliation owes it.
	if indexed := blockIndexVersion(t, database, id); indexed == nil || *indexed != 1 {
		t.Errorf("the decisions query's blocks stand at %v after its create, want version 1, indexed by the create", indexed)
	}
	refused := func(write string, response *httptest.ResponseRecorder) {
		t.Helper()
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), codeDecisionsQuery) {
			t.Errorf("%s of the decisions query: status=%d body=%s, want 409 %s", write, response.Code, response.Body.String(), codeDecisionsQuery)
		}
	}
	refused("an upload under its name", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts",
		map[string]any{"name": issueDecisionsName, "content": "Text written into the query.\n"}, "alice"))
	ops := map[string]any{"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": "More."}}, "summary": "edit"}
	refused("an edit by its slug", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts/"+slug+"/edits", ops, "alice"))
	refused("an edit by its id", dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+id+"/edits", ops, "alice"))
	var versions, updates int
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from artifact_versions where artifact_id = $1::uuid),
		       (select count(*) from doc_updates where artifact_id = $1::uuid)`, id).Scan(&versions, &updates); err != nil {
		t.Fatalf("read what the decisions query holds: %v", err)
	}
	if versions != 1 || updates != 0 {
		t.Errorf("the decisions query holds %d versions and %d updates after the refused writes, want its version 1 alone and no update", versions, updates)
	}
	if read := decodeBody[model.Artifact](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+id, nil, "alice")); read.Fragment != "" {
		t.Errorf("the decisions query reads the fragment %q, want none", read.Fragment)
	}

	// The document server as the browser peer reaches it (connectBrowserPeer), which proves the
	// browser's login.
	sockets := &servedSockets{finished: make(map[string]chan struct{})}
	server := httptest.NewServer(sockets.serve(deps.Docs.(*docs.Service).ServeHTTP))
	t.Cleanup(server.Close)
	socket, response, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws/doc/"+id,
		http.Header{"X-Dispatch-User": []string{"alice"}, socketHeader: []string{"1"}})
	if err == nil {
		_ = socket.Close()
		t.Fatalf("a socket to the decisions query %s connected, want it refused", id)
	}
	if response == nil {
		t.Fatalf("a socket to the decisions query failed with no answer: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusConflict || !strings.Contains(string(body), codeDecisionsQuery) {
		t.Errorf("a socket to the decisions query answers %d %q, want 409 %s", response.StatusCode, body, codeDecisionsQuery)
	}
}
