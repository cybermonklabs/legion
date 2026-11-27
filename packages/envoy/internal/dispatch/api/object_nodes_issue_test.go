package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Every issue, document, ask and message is a node of the store that names its project, or, for
// an object no project owns, the parties it is between, so a read can be limited to what a
// reader's projects hold. Each node is written at its kind's own create site, inside that
// write's transaction and before its row, and the facts the node write states carry the event
// that caused the create.
// The tests of this file hold the issue and document writers to that; object_nodes_items_test.go
// holds the ask and message writers.

// requireNodeBeforeRow installs, in the test's own database, a trigger that refuses an insert into
// table whose object the store holds no node of yet: the node of kind, addressed by the row's
// column address. It is named to sort after every trigger the schema puts on the table, so it reads
// the row as those leave it - artifacts_ref_key fills ref_key before it runs. A writer that wrote
// its node after its row, or derived it afterwards from its event, fails the create here.
func requireNodeBeforeRow(t *testing.T, database *store.Store, table, kind, address string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		create or replace function test_node_before_row() returns trigger language plpgsql as $$
		begin
		  if not exists (
		    select 1 from graph_nodes where kind = tg_argv[0] and id = to_jsonb(new) ->> tg_argv[1]
		  ) then
		    raise exception 'the % row % was written before its % node', tg_table_name, to_jsonb(new) ->> tg_argv[1], tg_argv[0];
		  end if;
		  return new;
		end $$
	`); err != nil {
		t.Fatalf("create the node-first trigger function: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, fmt.Sprintf(
		`create trigger zz_node_before_row before insert on %s for each row execute function test_node_before_row(%s, %s)`,
		table, quoteLiteral(kind), quoteLiteral(address))); err != nil {
		t.Fatalf("create the node-first trigger on %s: %v", table, err)
	}
}

func quoteLiteral(value string) string { return "'" + value + "'" }

// requireObjectNode reads the node of kind and id and fails unless it is the object's node as its
// create writes it - the supertag of its kind, live, project-visible, in project or between
// parties - caused by event, with each base attribute its write stated carried by that event.
func requireObjectNode(t *testing.T, database *store.Store, kind, id, supertag, project string, parties []string, event int64) {
	t.Helper()
	ctx := context.Background()
	node, held, err := graphstore.ReadNode(ctx, database.Pool, kind, id)
	if err != nil || !held {
		t.Fatalf("the store holds no node %s/%s (held %t, err %v)", kind, id, held, err)
	}
	if node.Supertag != supertag || node.State != graphstore.StateLive || node.Visibility != graphstore.VisibilityProject ||
		node.Project != project || !slices.Equal(node.Parties, parties) || node.SourceEvent != event {
		t.Errorf("node %s/%s is %+v, want supertag %s, live, project-visible, project %q, parties %v, caused by event %d",
			kind, id, node, supertag, project, parties, event)
	}
	wantParties, err := graphstore.PartiesValue(parties)
	if err != nil {
		t.Fatalf("join the parties %v: %v", parties, err)
	}
	stated := map[string]string{
		"visibility": graphstore.VisibilityProject, "state": graphstore.StateLive,
		"project": project, "parties": wantParties,
	}
	for attribute, value := range stated {
		history, err := graphstore.History(ctx, database.Pool, kind, id, attribute)
		if err != nil {
			t.Fatalf("read the %s of %s/%s: %v", attribute, kind, id, err)
		}
		if value == "" {
			if len(history) != 0 {
				t.Errorf("node %s/%s states %s %+v, want nothing: it carries none", kind, id, attribute, history)
			}
			continue
		}
		if len(history) != 1 || history[0].ToID != value || history[0].SourceEvent != event {
			t.Errorf("node %s/%s states %s %+v, want %q once, caused by event %d", kind, id, attribute, history, value, event)
		}
	}
}

// eventOf answers the id of the one event of type the log holds for the owner key names.
func eventOf(t *testing.T, database *store.Store, eventType, ownerColumn, owner string) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `
		select id from events where type = $1 and `+ownerColumn+` = $2 order by id desc limit 1
	`, eventType, owner).Scan(&id); err != nil {
		t.Fatalf("read the %s event of %s: %v", eventType, owner, err)
	}
	return id
}

// TestAnIssueCreateStoresItsNodeBeforeItsRow holds POST /api/v1/issues to writing the issue's node
// and its spec's node before the rows that hold them, in the issue's project, caused by the
// issue.created event the create appends.
func TestAnIssueCreateStoresItsNodeBeforeItsRow(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	requireNodeBeforeRow(t, database, "issues", graphstore.KindIssue, "key")
	requireNodeBeforeRow(t, database, "artifacts", graphstore.KindArtifact, "ref_key")

	issue := createInteractionIssue(t, handler, "NODE", "An issue that names its project", "The spec.\n")
	created := eventOf(t, database, "issue.created", "issue_key", issue.Key)
	requireObjectNode(t, database, graphstore.KindIssue, issue.Key, graphstore.SupertagIssue, "NODE", nil, created)
	requireObjectNode(t, database, graphstore.KindArtifact, issue.Key+"/spec", graphstore.SupertagDocument, "NODE", nil, created)
}

// TestEveryUploadStoresItsDocumentsNode holds every artifact upload to writing the document's node
// before its row: an issue's document and file, a project's document, each in that project and
// caused by the artifact.created event, and a file or document sent in an agent's conversation,
// which no project owns, between the parties of that conversation and at the sentinel, since such
// an upload appends no event (artifactTarget.ownsEvents).
func TestEveryUploadStoresItsDocumentsNode(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	requireNodeBeforeRow(t, database, "artifacts", graphstore.KindArtifact, "ref_key")
	issue := createInteractionIssue(t, handler, "UPL", "Uploads name their project", "The spec.\n")

	refKeyOf := func(artifactID string) string {
		var refKey string
		if err := database.Pool.QueryRow(context.Background(),
			`select ref_key from artifacts where id = $1::uuid`, artifactID).Scan(&refKey); err != nil {
			t.Fatalf("read the ref_key of %s: %v", artifactID, err)
		}
		return refKey
	}
	createdEvent := func(artifactID string) int64 {
		var id int64
		if err := database.Pool.QueryRow(context.Background(), `
			select id from events where type = 'artifact.created' and payload->'artifact'->>'id' = $1
		`, artifactID).Scan(&id); err != nil {
			t.Fatalf("read the artifact.created event of %s: %v", artifactID, err)
		}
		return id
	}

	document := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "notes.md", "content": "# Notes\n\nThe plan stands.\n",
	}, "alice")
	if document.Code != http.StatusCreated {
		t.Fatalf("upload an issue document: status=%d body=%s", document.Code, document.Body.String())
	}
	issueDocument := decodeBody[artifactUploadResponse](t, document).Artifact
	requireObjectNode(t, database, graphstore.KindArtifact, refKeyOf(issueDocument.ID), graphstore.SupertagDocument,
		"UPL", nil, createdEvent(issueDocument.ID))

	file := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "shot.png"},
		"shot.png", "image/png", pngBytes("pixels"), "alice")
	if file.Code != http.StatusCreated {
		t.Fatalf("upload an issue file: status=%d body=%s", file.Code, file.Body.String())
	}
	issueFile := decodeBody[artifactUploadResponse](t, file).Artifact
	requireObjectNode(t, database, graphstore.KindArtifact, refKeyOf(issueFile.ID), graphstore.SupertagDocument,
		"UPL", nil, createdEvent(issueFile.ID))

	runbook := createProjectDocument(t, handler, "UPL", "Runbook.md", "# Runbook\n")
	requireObjectNode(t, database, graphstore.KindArtifact, refKeyOf(runbook.ID), graphstore.SupertagDocument,
		"UPL", nil, createdEvent(runbook.ID))

	// A conversation holds files and images, never a markdown document (agentOwnsFilesOnly).
	const session = "01a1e0fe-90a3-76fa-b47a-7d62f39b4521"
	for _, upload := range []struct{ name, contentType, content string }{
		{"photo.png", "image/png", string(pngBytes("conversation"))},
		{"trace.bin", "application/octet-stream", "\x00\x01 a trace sent in the conversation"},
	} {
		sent := multipartRequest(t, handler, "/api/v1/agents/"+session+"/artifacts", map[string]string{"name": upload.name},
			upload.name, upload.contentType, []byte(upload.content), "alice")
		if sent.Code != http.StatusCreated {
			t.Fatalf("upload %s to a conversation: status=%d body=%s", upload.name, sent.Code, sent.Body.String())
		}
		conversationFile := decodeBody[artifactUploadResponse](t, sent).Artifact
		requireObjectNode(t, database, graphstore.KindArtifact, refKeyOf(conversationFile.ID), graphstore.SupertagDocument,
			"", []string{graphstore.PartySession(session)}, graphstore.NoEvent)
	}
}

// TestABlockNodeCarriesItsDocumentsProject holds #3178's one block writer (refs.IndexBlocks, through
// blockNode) to giving every block node it writes its document's project: the blocks of an issue's
// spec and of a project's document, and the blocks a later version adds, changes or drops. A
// conversation holds no document, so no block of one is written (agentOwnsFilesOnly).
func TestABlockNodeCarriesItsDocumentsProject(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "BLK", "Blocks name their project", "First paragraph.\n\nSecond paragraph.\n")
	runbook := createProjectDocument(t, handler, "BLK", "Runbook.md", "# Runbook\n\nStep one.\n")

	// A later version of the spec changes one paragraph, adds one and drops one, so the index
	// writes a changed block, a new block and a gone block.
	revised := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": "spec.md", "content": "First paragraph, revised.\n\nA third paragraph.\n",
		"precondition": uploadBase(t, handler, issue.PrimaryArtifactID),
	}, "alice")
	if revised.Code != http.StatusCreated {
		t.Fatalf("upload a revised spec: status=%d body=%s", revised.Code, revised.Body.String())
	}

	for _, document := range []struct{ name, artifactID string }{
		{"the issue's spec", issue.PrimaryArtifactID},
		{"the project's runbook", runbook.ID},
	} {
		blocks := blockNodesOfDocument(t, database, document.artifactID)
		if len(blocks) == 0 {
			t.Fatalf("%s has no block node, so its blocks prove nothing", document.name)
		}
		for _, block := range blocks {
			if block.Project != "BLK" || block.Parties != nil {
				t.Errorf("block %s of %s carries project %q and parties %v, want BLK and none",
					block.ID, document.name, block.Project, block.Parties)
			}
		}
	}
	states := map[string]int{}
	for _, block := range blockNodesOfDocument(t, database, issue.PrimaryArtifactID) {
		states[block.State]++
	}
	if states[graphstore.StateGone] == 0 || states[graphstore.StateLive] < 2 {
		t.Fatalf("the revised spec's blocks are %v, want a gone block and at least two live ones: the revision proves nothing", states)
	}
}

// blockNodesOfDocument answers every block node of the document's ref_key.
func blockNodesOfDocument(t *testing.T, database *store.Store, artifactID string) []graphstore.Node {
	t.Helper()
	ctx := context.Background()
	var refKey string
	if err := database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1::uuid`, artifactID).Scan(&refKey); err != nil {
		t.Fatalf("read the ref_key of %s: %v", artifactID, err)
	}
	nodes, err := graphstore.NodesOfKind(ctx, database.Pool, "block")
	if err != nil {
		t.Fatalf("read the block nodes: %v", err)
	}
	var held []graphstore.Node
	for _, node := range nodes {
		if strings.HasPrefix(node.ID, refKey+"#") {
			held = append(held, node)
		}
	}
	return held
}
