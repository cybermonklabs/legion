package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// removeDocument sends DELETE /api/v1/artifacts/{id} as alice, the person who made the document.
func removeDocument(t *testing.T, handler http.Handler, artifactID string) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodDelete, "/api/v1/artifacts/"+artifactID, nil, "alice")
}

// removedDocument removes a document that must go, and fails the test when it does not.
func removedDocument(t *testing.T, handler http.Handler, artifactID string) {
	t.Helper()
	response := removeDocument(t, handler, artifactID)
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("remove document %s: status=%d body=%s, want 204 and no body", artifactID, response.Code, response.Body.String())
	}
}

// settleDocumentVersion versions a live document as settlement does: inside a transaction joined
// with Docs.Join, the only way a document write runs (replaceDocumentText). Over a document
// nobody typed in it writes nothing, and says so in the result it answers.
func settleDocumentVersion(database *store.Store, documents docs.API, artifactID string, actor model.Actor) (docs.VersionResult, error) {
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return docs.VersionResult{}, err
	}
	defer tx.Rollback(ctx)
	joined, ledger := documents.Join(ctx, tx)
	defer ledger.Discard()
	version, err := documents.SnapshotVersion(joined, artifactID, actor)
	if err != nil {
		return version, err
	}
	return version, ledger.Commit(ctx)
}

// emptyDocument makes one of the issue's documents by typing a title: the JSON create with no
// content, which is the only way a document this route removes comes about.
func emptyDocument(t *testing.T, handler http.Handler, issueKey, name string) model.Artifact {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/artifacts",
		map[string]any{"name": name}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("make %q with no content: status=%d body=%s", name, response.Code, response.Body.String())
	}
	made := decodeBody[uploadedArtifact](t, response)
	if made.Artifact.Kind != "doc" || len(made.Artifact.Versions) != 1 {
		t.Fatalf("%q made = %#v, want one empty document of kind doc", name, made.Artifact)
	}
	return made.Artifact
}

// assertRemovalRefused drives a removal that must be refused and reads the document still whole
// afterwards: the same row, with the same versions, as the read before the refusal gave.
func assertRemovalRefused(t *testing.T, handler http.Handler, what string, document model.Artifact, status int, code, sentence string) {
	t.Helper()
	before, err := json.Marshal(readDocument(t, handler, document.ID))
	if err != nil {
		t.Fatalf("encode %s before its refusal: %v", what, err)
	}
	response := removeDocument(t, handler, document.ID)
	refusal := decodeBody[refusedWrite](t, response)
	if response.Code != status || refusal.Code != code || refusal.Error != sentence {
		t.Fatalf("remove %s: status=%d body=%s, want %d %s %q", what, response.Code, response.Body.String(), status, code, sentence)
	}
	after, err := json.Marshal(readDocument(t, handler, document.ID))
	if err != nil {
		t.Fatalf("encode %s after its refusal: %v", what, err)
	}
	if string(after) != string(before) {
		t.Fatalf("%s after the refusal is\n%s\nand before it was\n%s", what, after, before)
	}
}

// notUntouchedSentence is the sentence a removal refused for reason answers.
func notUntouchedSentence(document model.Artifact, reason string) string {
	return fmt.Sprintf("artifact %s (%s) is not untouched: %s; a document is removed only while it holds nothing",
		document.Slug, document.Name, reason)
}

// artifactColumn is one column of the catalogue that can name an artifact: the table holding it
// and its own name. The census below reads them rather than naming a table by hand, so a table a
// later change adds fails this test rather than leaking rows.
type artifactColumn struct {
	table, column string
}

func (c artifactColumn) String() string { return c.table + "." + c.column }

// catalogueColumns runs one catalogue reading: list is the statement naming each table and
// column, count the statement counting what list must have reached. A reading that reached only
// part of them fails here rather than passing as emptiness later.
func catalogueColumns(t *testing.T, database *store.Store, what, list, count string) []artifactColumn {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, list)
	if err != nil {
		t.Fatalf("list %s: %v", what, err)
	}
	defer rows.Close()
	read := []artifactColumn{}
	for rows.Next() {
		var column artifactColumn
		if err := rows.Scan(&column.table, &column.column); err != nil {
			t.Fatalf("scan one of %s: %v", what, err)
		}
		read = append(read, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list %s: %v", what, err)
	}
	var held int
	if err := database.Pool.QueryRow(ctx, count).Scan(&held); err != nil {
		t.Fatalf("count %s: %v", what, err)
	}
	if len(read) != held {
		t.Fatalf("the census read %d of %s and the catalogue holds %d; the reading must reach every one of them",
			len(read), what, held)
	}
	return read
}

// everyArtifactKey is every foreign key of the catalogue that points at artifacts(id), by the
// table and the column holding it. A key over more than one column is left out of the listing and
// counted all the same, so a composite key a later change adds fails the count rather than being
// read as its first column.
func everyArtifactKey(t *testing.T, database *store.Store) []artifactColumn {
	t.Helper()
	return catalogueColumns(t, database, "the foreign keys to artifacts(id)", `
		select k.conrelid::regclass::text,
		       (select a.attname from pg_attribute a where a.attrelid = k.conrelid and a.attnum = k.conkey[1])
		from pg_constraint k
		where k.contype = 'f' and k.confrelid = 'artifacts'::regclass and array_length(k.conkey, 1) = 1
		order by 1, 2
	`, `
		select count(*)::int from pg_constraint
		where contype = 'f' and confrelid = 'artifacts'::regclass
	`)
}

// everyArtifactIDColumn is every uuid column of the catalogue whose name ends in artifact_id.
// Several of those carry no foreign key on purpose, so that the pre-deploy census does not size
// the table a key would point at, and they are the reason this reading stands beside the keys.
func everyArtifactIDColumn(t *testing.T, database *store.Store) []artifactColumn {
	t.Helper()
	const where = `
		from information_schema.columns c
		join information_schema.tables t
		  on t.table_schema = c.table_schema and t.table_name = c.table_name and t.table_type = 'BASE TABLE'
		where c.table_schema = 'public' and c.column_name like '%artifact\_id' and c.data_type = 'uuid'
	`
	return catalogueColumns(t, database, "the uuid columns named artifact_id",
		`select c.table_name, c.column_name `+where+` order by 1, 2`,
		`select count(*)::int `+where)
}

// assertNoRowNamesTheDocument is the census: no row of the catalogue names the removed document.
// It reads the catalogue twice - every foreign key to artifacts(id), and every uuid column whose
// name ends in artifact_id - and then refs, whose two ends are text and carry no key at all.
func assertNoRowNamesTheDocument(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	ctx := context.Background()
	keys := everyArtifactKey(t, database)
	columns := everyArtifactIDColumn(t, database)
	if len(keys) == 0 || len(columns) < len(keys) {
		t.Fatalf("the census read %d foreign keys and %d uuid artifact_id columns; every key's column is one of those columns",
			len(keys), len(columns))
	}
	for _, column := range slices.Concat(keys, columns) {
		var held bool
		if err := database.Pool.QueryRow(ctx,
			`select exists(select 1 from `+column.table+` where `+column.column+` = $1)`, artifactID).Scan(&held); err != nil {
			t.Fatalf("read %s for the removed document: %v", column, err)
		}
		if held {
			t.Fatalf("%s still holds a row for the removed document %s", column, artifactID)
		}
	}
	var cited bool
	if err := database.Pool.QueryRow(ctx,
		`select exists(select 1 from refs where from_id = $1 or to_id = $1)`, artifactID).Scan(&cited); err != nil {
		t.Fatalf("read refs for the removed document: %v", err)
	}
	if cited {
		t.Fatalf("refs still holds a row for the removed document %s", artifactID)
	}
}

// removedEvent is the payload an artifact.removed event carries: the document as it was.
type removedEvent struct {
	Artifact model.Artifact `json:"artifact"`
}

// A person removes a document of an issue that is still untouched: one made by typing a title and
// never written in. It goes whole - no row of the catalogue names it afterwards - and the issue
// keeps its own record of it, because an issue document's events are the issue's and carry no
// artifact id. A document that holds anything, or that anything names, is refused in words that
// say which test it failed and is still there after the refusal; a project's document is not
// removed at all; and the removal closes the document's editor room once its transaction has
// committed.
func TestAnUntouchedDocumentIsRemovedWholeAndATouchedOneIsRefused(t *testing.T) {
	// The settlement is held back for the test's duration, so an edit written into a room is
	// still unsettled when the removal reads it. The room test below runs a server of its own,
	// which settles.
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	spec := createInteractionIssue(t, handler, "TEST", "A spec of its own", "# Spec\n\nA passage.\n")
	ctx := context.Background()

	t.Run("the document goes whole and the issue keeps its record of it", func(t *testing.T) {
		issue := titleIssue(t, handler, "A document made by mistake")
		owner := "/api/v1/issues/" + issue.Key
		document := emptyDocument(t, handler, issue.Key, "Lauch notes")
		reference := "dispatch://" + issue.Key + "/artifact/" + document.Slug

		removedDocument(t, handler, document.ID)

		// The issue's document list no longer holds it, and its reference answers nothing: not
		// the read by id, not the read by slug, and not the node the graph resolves.
		list := dispatchRequest(t, handler, http.MethodGet, owner+"/artifacts", nil, "alice")
		if list.Code != http.StatusOK {
			t.Fatalf("list the issue's documents: status=%d body=%s", list.Code, list.Body.String())
		}
		for _, listed := range decodeBody[[]model.Artifact](t, list) {
			if listed.ID == document.ID {
				t.Fatalf("the issue's document list still holds %s: %s", document.ID, list.Body.String())
			}
		}
		for _, gone := range []struct{ what, path string }{
			{what: "the read by id", path: "/api/v1/artifacts/" + document.ID},
			{what: "the read by slug", path: owner + "/artifacts/" + document.Slug},
		} {
			response := dispatchRequest(t, handler, http.MethodGet, gone.path, nil, "alice")
			if response.Code != http.StatusNotFound || decodeBody[refusedWrite](t, response).Code != "ARTIFACT_NOT_FOUND" {
				t.Fatalf("%s of the removed document: status=%d body=%s, want 404 ARTIFACT_NOT_FOUND",
					gone.what, response.Code, response.Body.String())
			}
		}
		if node := graphRequest(t, handler, url.Values{"to": {reference}}); node.Code == http.StatusOK {
			t.Fatalf("%s still answers a node: %s", reference, node.Body.String())
		}

		// The issue's log holds the removal, carrying the document's name and slug, beside the
		// creation it still holds. Both are the issue's own rows, which carry no artifact id, so
		// the feed reads them with no artifact row behind them.
		var removals []removedEvent
		types := map[string]int{}
		for _, event := range issueEvents(t, handler, issue.Key) {
			types[event.Type]++
			if event.Type != "artifact.removed" {
				continue
			}
			var payload removedEvent
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("decode the artifact.removed payload %s: %v", event.Payload, err)
			}
			removals = append(removals, payload)
		}
		if len(removals) != 1 || removals[0].Artifact.Name != "Lauch notes" || removals[0].Artifact.Slug != document.Slug ||
			removals[0].Artifact.ID != document.ID {
			t.Fatalf("the issue's artifact.removed events = %#v, want one carrying the name and slug of %s", removals, document.ID)
		}
		if types["artifact.created"] == 0 {
			t.Fatalf("the issue's log no longer holds artifact.created: %v", types)
		}
		var ownRows int
		if err := database.Pool.QueryRow(ctx, `
			select count(*)::int from events
			where issue_key = $1 and type in ('artifact.created', 'artifact.removed')
			  and payload->'artifact'->>'id' = $2 and artifact_id is null
		`, issue.Key, document.ID).Scan(&ownRows); err != nil {
			t.Fatalf("count the removed document's own events: %v", err)
		}
		if ownRows != 2 {
			t.Fatalf("the issue holds %d issue-owned events of %s carrying no artifact id, want 2", ownRows, document.ID)
		}

		assertNoRowNamesTheDocument(t, database, document.ID)
	})

	t.Run("a document that holds something, or that something names, is refused", func(t *testing.T) {
		issue := titleIssue(t, handler, "Refused removals")
		owner := "/api/v1/issues/" + issue.Key

		written := uploadDocument(t, handler, owner, "Written notes", "# Notes\n\nA passage.\n").Artifact
		// A document somebody typed in: the write goes into its room, and nothing settles it
		// into a version while this server's settlement is held back, so what it holds is a
		// doc_updates row above the version's cursor - the durable terms the removal reads a
		// room in.
		typed := emptyDocument(t, handler, issue.Key, "Typed notes")
		if _, err := replaceDocumentText(database, deps.Docs, typed.ID, "# Typed\n\nA passage.\n",
			model.Actor{Kind: "user", ID: "alice"}); err != nil {
			t.Fatalf("type into the document's room: %v", err)
		}
		commented := emptyDocument(t, handler, issue.Key, "Commented notes")
		asked := emptyDocument(t, handler, issue.Key, "Asked notes")
		approved := emptyDocument(t, handler, issue.Key, "Approved notes")
		reviewed := emptyDocument(t, handler, issue.Key, "Reviewed notes")
		checked := emptyDocument(t, handler, issue.Key, "Checked notes")
		read := emptyDocument(t, handler, issue.Key, "Read notes")
		cited := emptyDocument(t, handler, issue.Key, "Cited notes")
		plan := uploadDocument(t, handler, owner, "plan.md", "# Plan\n").Artifact
		picture := uploadImage(t, handler, issue.Key, "diagram.png", "pixels").Artifact

		// A comment and an ask of an issue name one of its documents through their anchor and,
		// for an ask written back as a question block, through block_artifact_id: the artifact_id
		// column of each is for a project's documents (comments_one_owner, asks_one_owner), which
		// this route does not remove.
		seedExec(t, database, "a comment anchored in a document", `
			insert into comments (issue_key, author, body, anchor)
			values ($1, '{"kind":"user","id":"alice"}', 'A remark on the notes.', jsonb_build_object('artifact_id', $2::text))
		`, issue.Key, commented.ID)
		seedExec(t, database, "an ask written back as a question block", `
			insert into asks (issue_key, author, question, block_artifact_id, block_id)
			values ($1, '{"kind":"user","id":"alice"}', 'Which way?', $2::uuid, 'q1')
		`, issue.Key, asked.ID)
		if requested := sessionRequest(t, handler, http.MethodPost,
			"/api/v1/artifacts/"+approved.ID+"/approval-requests", map[string]any{"actor": sessionActor()}); requested.Code != http.StatusCreated {
			t.Fatalf("request approval of the empty document: status=%d body=%s", requested.Code, requested.Body.String())
		}
		seedExec(t, database, "a review of a document", `
			insert into artifact_reviews (artifact_id, version, state, actor)
			values ($1::uuid, 1, 'approved', '{"kind":"user","id":"alice"}')
		`, reviewed.ID)
		seedExec(t, database, "a check result naming a document", `
			insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id,
			                           check_version, check_sha256, role, commit_sha, verdict, actor)
			values ($1, $1, $2::uuid, 'c-removal', 1, repeat('0', 64), 'implementer', repeat('a', 40), 'pass',
			        '{"kind":"user","id":"alice"}')
		`, issue.Key, checked.ID)
		if marked := dispatchRequest(t, handler, http.MethodPut,
			"/api/v1/me/documents/"+read.ID+"/read", map[string]any{"version": 1}, "alice"); marked.Code != http.StatusOK &&
			marked.Code != http.StatusNoContent {
			t.Fatalf("mark the document read: status=%d body=%s", marked.Code, marked.Body.String())
		}
		createThreadComment(t, handler, issue.Key, map[string]any{
			"body": "See dispatch://" + issue.Key + "/artifact/" + cited.Slug + " for the rest.",
		}, "alice")

		// A document whose block cites another document's, written through block_refs: the citing
		// document's own markdown holds the reference, so the row is the one its indexing wrote.
		blockCited := emptyDocument(t, handler, issue.Key, "Block cited notes")
		uploadDocument(t, handler, owner, "Citing notes",
			"# Citing\n\nSee dispatch://"+issue.Key+"/artifact/"+blockCited.Slug+".\n")

		for _, refused := range []struct {
			what     string
			document model.Artifact
			status   int
			code     string
			sentence string
		}{
			{
				what: "a document whose version holds words", document: written,
				status: http.StatusConflict, code: "ARTIFACT_NOT_UNTOUCHED",
				sentence: notUntouchedSentence(written, "its first version is not empty"),
			},
			{
				what: "a document whose edit has not settled", document: typed,
				status: http.StatusConflict, code: "ARTIFACT_NOT_UNTOUCHED",
				sentence: notUntouchedSentence(typed, "an edit of it has not settled"),
			},
			{
				what: "a document a comment points at", document: commented,
				status: http.StatusConflict, code: "ARTIFACT_NOT_UNTOUCHED",
				sentence: notUntouchedSentence(commented, "a comment points at it"),
			},
			{
				what: "a document an ask points at", document: asked,
				status: http.StatusConflict, code: "ARTIFACT_NOT_UNTOUCHED",
				sentence: notUntouchedSentence(asked, "an ask points at it"),
			},
			{
				what: "a document an approval request points at", document: approved,
				status: http.StatusConflict, code: "ARTIFACT_NOT_UNTOUCHED",
				sentence: notUntouchedSentence(approved, "an approval request points at it"),
			},
			{
				what: "a document a review points at", document: reviewed,
				status: http.StatusConflict, code: "ARTIFACT_NOT_UNTOUCHED",
				sentence: notUntouchedSentence(reviewed, "a review points at it"),
			},
			{
				what: "a document a check result points at", document: checked,
				status: http.StatusConflict, code: "ARTIFACT_NOT_UNTOUCHED",
				sentence: notUntouchedSentence(checked, "a check result points at it"),
			},
			{
				what: "a document a person has read", document: read,
				status: http.StatusConflict, code: "ARTIFACT_NOT_UNTOUCHED",
				sentence: notUntouchedSentence(read, "a person has read it"),
			},
			{
				what: "a document a comment cites", document: cited,
				status: http.StatusConflict, code: "ARTIFACT_NOT_UNTOUCHED",
				sentence: notUntouchedSentence(cited, "another document cites it"),
			},
			{
				what: "a document another document's block cites", document: blockCited,
				status: http.StatusConflict, code: "ARTIFACT_NOT_UNTOUCHED",
				sentence: notUntouchedSentence(blockCited, "another document cites it"),
			},
			{
				what: "a document that plays a role", document: plan,
				status: http.StatusConflict, code: "ARTIFACT_NOT_UNTOUCHED",
				sentence: notUntouchedSentence(plan, "it plays the "+model.ArtifactRolePlan+" role"),
			},
			{
				what: "a picture", document: picture,
				status: http.StatusConflict, code: "ARTIFACT_NOT_UNTOUCHED",
				sentence: notUntouchedSentence(picture, "it is a "+picture.Kind+" and not a document"),
			},
		} {
			assertRemovalRefused(t, handler, refused.what, refused.document, refused.status, refused.code, refused.sentence)
		}

		specDocument := readDocument(t, handler, spec.PrimaryArtifactID)
		assertRemovalRefused(t, handler, "the issue's spec", specDocument,
			http.StatusConflict, "ARTIFACT_NOT_UNTOUCHED", notUntouchedSentence(specDocument, "it is the issue's spec"))
	})

	t.Run("a project's document is not removed", func(t *testing.T) {
		document := createProjectDocument(t, handler, "TEST", "A project runbook", "# Runbook\n")
		assertRemovalRefused(t, handler, "a project's document", document, http.StatusBadRequest, "ARTIFACT_PROJECT_DOCUMENT",
			fmt.Sprintf("artifact %s (%s) belongs to project %s; a project's document is not removed",
				document.Slug, document.Name, document.Project))
	})

	t.Run("the owner's refusals and the person scope come first", func(t *testing.T) {
		issue := titleIssue(t, handler, "A closed issue's document")
		closed := emptyDocument(t, handler, issue.Key, "Closed notes")
		open := titleIssue(t, handler, "Who removes a document")
		scoped := emptyDocument(t, handler, open.Key, "Scoped notes")

		shared := sessionRequest(t, handler, http.MethodDelete, "/api/v1/artifacts/"+scoped.ID, nil)
		if shared.Code != http.StatusForbidden || decodeBody[refusedWrite](t, shared).Code != codeScopeRequired {
			t.Fatalf("a shared token's removal: status=%d body=%s, want 403 %s", shared.Code, shared.Body.String(), codeScopeRequired)
		}
		if held := readDocument(t, handler, scoped.ID); held.ID != scoped.ID {
			t.Fatalf("the document after the refused removal = %#v, want %s", held, scoped.ID)
		}

		closeIssue(t, handler, issue.Key)
		assertRefusedOnClosedIssue(t, "a removal", removeDocument(t, handler, closed.ID))
		if held := readDocument(t, handler, closed.ID); held.ID != closed.ID {
			t.Fatalf("the closed issue's document after the refusal = %#v, want %s", held, closed.ID)
		}
	})

	t.Run("the removal closes the document's room", func(t *testing.T) {
		// A server that settles, so the version pass below is the one settlement makes.
		roomHandler, roomDatabase, deps := newTestServer(t, testServerOptions{})
		documents, ok := deps.Docs.(*docs.Service)
		if !ok {
			t.Fatalf("the test server's document service is %T, which holds no rooms to read", deps.Docs)
		}
		issue := createInteractionIssue(t, roomHandler, "ROOM", "A room of its own", "# Spec\n")
		document := emptyDocument(t, roomHandler, issue.Key, "Room notes")

		// The room loads, and a version pass over it writes nothing, because the document reads
		// as it did: the settlement a typed document gets, over one nobody typed in.
		if text, err := documents.Text(ctx, document.ID); err != nil || text != "" {
			t.Fatalf("read the live document = %q, %v; want the empty document it was made as", text, err)
		}
		version, err := settleDocumentVersion(roomDatabase, documents, document.ID, model.Actor{Kind: "user", ID: "alice"})
		if err != nil || version.Wrote {
			t.Fatalf("the settlement pass over the untouched document wrote %#v (%v); want nothing written", version, err)
		}
		// The room is read live before the removal, so what is said of it after is said of a room
		// that existed.
		if rooms := documents.ResidentRooms(); !slices.Contains(rooms, document.ID) {
			t.Fatalf("this process holds the rooms %v, and not %s, whose room the removal must close", rooms, document.ID)
		}

		removedDocument(t, roomHandler, document.ID)

		if rooms := documents.ResidentRooms(); slices.Contains(rooms, document.ID) {
			t.Fatalf("this process still holds the room of the removed document %s: %v", document.ID, rooms)
		}
		// Nothing settles against the row that has gone: the eviction ended what the room had
		// armed, so a settle window passes with no room loaded again and nothing said about the
		// document in the service's log. The log is taken from after the removal, so the
		// request's own line is not one of the lines read.
		logs := captureLogs(t)
		if testwait.Holds(time.Second, 10*time.Millisecond, func() bool {
			return slices.Contains(documents.ResidentRooms(), document.ID) || strings.Contains(logs.String(), document.ID)
		}) {
			t.Fatalf("the service read the removed document %s again: rooms=%v log=%s",
				document.ID, documents.ResidentRooms(), logs.Lines(document.ID))
		}
		assertNoRowNamesTheDocument(t, roomDatabase, document.ID)
	})
}
