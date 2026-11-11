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

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
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

// assertCitedFromAnAskPartAlone reads the citation of document as the tables hold it: one row of
// ask_block_refs, where a link an ask's option or its answer holds stands, and no row of refs or
// block_refs, which is where every other citation is. A citation in either of those two would
// refuse the removal whatever ask_block_refs held, so this is what makes the case below a test
// of an ask's parts.
func assertCitedFromAnAskPartAlone(t *testing.T, database *store.Store, document model.Artifact) {
	t.Helper()
	var parts, others int
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*)::int from ask_block_refs where to_kind = 'artifact' and to_id = $1),
		       (select count(*)::int from refs where to_kind = 'artifact' and to_id = $1)
		     + (select count(*)::int from block_refs where to_kind = 'artifact' and to_id = $1)
	`, document.RefKey).Scan(&parts, &others); err != nil {
		t.Fatalf("read what cites %s: %v", document.Slug, err)
	}
	if parts != 1 || others != 0 {
		t.Fatalf("%s is cited by %d ask parts and %d rows of refs and block_refs, want one ask part and nothing else",
			document.Slug, parts, others)
	}
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

		// A document an ask's option cites: an ask's own refs row holds the links of its question
		// alone (refs/ask_blocks.go), so a link written in an option or in the answer is a row of
		// ask_block_refs and of no other table.
		askCited := emptyDocument(t, handler, issue.Key, "Ask cited notes")
		opened := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": "Which way?",
			"options": []map[string]string{
				{"label": "The notes' way", "description": "As dispatch://" + issue.Key + "/artifact/" + askCited.Slug + " has it."},
				{"label": "Another way"},
			},
		}, "alice")
		if opened.Code != http.StatusCreated {
			t.Fatalf("open the ask citing %s: status=%d body=%s", askCited.Slug, opened.Code, opened.Body.String())
		}
		assertCitedFromAnAskPartAlone(t, database, askCited)

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
				what: "a document an ask's option cites", document: askCited,
				status: http.StatusConflict, code: "ARTIFACT_NOT_UNTOUCHED",
				sentence: notUntouchedSentence(askCited, "another document cites it"),
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

	t.Run("an append in flight while the removal runs refuses it", func(t *testing.T) {
		issue := titleIssue(t, handler, "A document written in mid-removal")
		document := emptyDocument(t, handler, issue.Key, "Raced notes")

		// What a room's batch of browser updates holds on any process while it appends: the
		// document's lock, taken here as the transaction-level form of the session lock that
		// batch's opening pipeline takes (docs/room_appends.go), and the doc_updates row it has
		// written and not committed. Such a writer takes no owner row, so the issue's row the
		// removal holds does not order it: the room's lock is what does.
		appending, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the append in flight: %v", err)
		}
		defer appending.Rollback(context.Background())
		if _, err := appending.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1))`, document.ID); err != nil {
			t.Fatalf("hold the document's room lock: %v", err)
		}
		// The update is the document's own first one over again, which decodes and adds nothing:
		// what the removal reads of it is its version, above the one the document's version was
		// stamped at (writeDocumentVersion), which is an edit that has not settled.
		appended, err := appending.Exec(ctx, `
			insert into doc_updates (artifact_id, version, update, content_changed)
			select $1::uuid, max(version) + 1, (array_agg(update order by version desc))[1], true
			from doc_updates where artifact_id = $1::uuid
		`, document.ID)
		if err != nil {
			t.Fatalf("append to the document while the removal runs: %v", err)
		}
		if appended.RowsAffected() != 1 {
			t.Fatalf("the append in flight stored %d rows, want the one row a browser's keystroke stores", appended.RowsAffected())
		}

		removals := make(chan *httptest.ResponseRecorder, 1)
		go func() { removals <- removeDocument(t, handler, document.ID) }()
		// The removal takes the issue's row, then this room's lock, where it waits.
		waitForDatabaseLocks(t, appending, 1)
		select {
		case answered := <-removals:
			t.Fatalf("the removal answered status=%d body=%s while an append of the document was in flight; it read the document without the room's lock",
				answered.Code, answered.Body.String())
		default:
		}

		if err := appending.Commit(ctx); err != nil {
			t.Fatalf("commit the append in flight: %v", err)
		}
		response := <-removals
		refusal := decodeBody[refusedWrite](t, response)
		sentence := notUntouchedSentence(document, "an edit of it has not settled")
		if response.Code != http.StatusConflict || refusal.Code != "ARTIFACT_NOT_UNTOUCHED" || refusal.Error != sentence {
			t.Fatalf("the removal after the append committed: status=%d body=%s, want 409 ARTIFACT_NOT_UNTOUCHED %q",
				response.Code, response.Body.String(), sentence)
		}
		if held := readDocument(t, handler, document.ID); held.ID != document.ID {
			t.Fatalf("the document after the refused removal = %#v, want %s", held, document.ID)
		}
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

// Two document services run against one database, as two Dispatch processes do, and both hold a
// room of the same untouched document. The removal goes through the first of them, and both
// rooms close: the removal's own transaction announces it to every process
// (docs.AnnounceDocumentRemoved), so the second process closes its room of a row that has gone
// rather than keeping one whose next append would meet a foreign key that no longer resolves
// (R10). A build that evicts only on the removing process fails here.
//
// The close carries no content: the removal is refused for anything but an untouched document,
// which the test reads as the document's one empty version before it removes anything.
func TestARemovedDocumentsRoomClosesOnEveryProcess(t *testing.T) {
	ctx := context.Background()
	handler, database, deps := newTestServer(t, testServerOptions{})
	removing, ok := deps.Docs.(*docs.Service)
	if !ok {
		t.Fatalf("the test server's document service is %T, which holds no rooms to read", deps.Docs)
	}
	runDocumentRoomSync(t, database, removing)
	// The second process: a document service of its own on a second pool, as cmd/dispatch runs
	// one, hearing the first's announcements over the database alone.
	secondStore := storetest.SecondProcess(t, database)
	holding := docs.New(docs.Deps{Store: secondStore, Identity: headerIdentity(secondStore), Settle: time.Hour})
	t.Cleanup(func() { _ = holding.Shutdown(context.Background()) })
	runDocumentRoomSync(t, secondStore, holding)

	issue := createInteractionIssue(t, handler, "ROOM", "A document two processes hold", "# Spec\n")
	document := emptyDocument(t, handler, issue.Key, "Room notes")

	// The document is untouched, which is the only document this route removes: it holds one
	// version and that version is empty, so neither room below holds anything a person typed.
	var versions int
	var written string
	if err := database.Pool.QueryRow(ctx, `
		select count(*)::int, coalesce(string_agg(coalesce(markdown, ''), ''), '')
		from artifact_versions where artifact_id = $1::uuid
	`, document.ID).Scan(&versions, &written); err != nil {
		t.Fatalf("read the document's versions: %v", err)
	}
	if versions != 1 || written != "" {
		t.Fatalf("the document holds %d versions carrying %q; want the one empty version typing a title makes", versions, written)
	}

	// A person opens the document on the second process, which loads its room there: the room a
	// removal on the other process must close, and the person R10 leaves holding nothing live
	// over a row that has gone. Opening it types nothing, so the document is still untouched.
	secondServer := httptest.NewServer(http.HandlerFunc(holding.ServeHTTP))
	t.Cleanup(secondServer.Close)
	connection, wsResponse, err := gws.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(secondServer.URL, "http")+"/ws/doc/"+document.ID,
		http.Header{"X-Dispatch-User": []string{"alice"}})
	if err != nil {
		t.Fatalf("open the document on the second process: response=%#v err=%v", wsResponse, err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	// Each process is read holding a room of the document, so what is said of the two rooms
	// after the removal is said of rooms that existed. The removing process loaded its own when
	// the document was made.
	for _, process := range []struct {
		what    string
		service *docs.Service
	}{
		{what: "the removing process", service: removing},
		{what: "the second process", service: holding},
	} {
		if text, err := process.service.Text(ctx, document.ID); err != nil || text != "" {
			t.Fatalf("%s reads the live document as %q, %v; want the empty document it was made as", process.what, text, err)
		}
		if !testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
			return slices.Contains(process.service.ResidentRooms(), document.ID)
		}) {
			t.Fatalf("%s holds the rooms %v, and not %s, whose room the removal must close",
				process.what, process.service.ResidentRooms(), document.ID)
		}
	}

	removedDocument(t, handler, document.ID)

	// The second process hears the removal over the database and closes its room. The wait is
	// for the notification's trip, not for a retry: nothing announces this twice.
	if !testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
		return !slices.Contains(holding.ResidentRooms(), document.ID)
	}) {
		t.Fatalf("the second process still holds the room of the removed document %s: %v; its next append meets a foreign key that no longer resolves",
			document.ID, holding.ResidentRooms())
	}
	if rooms := removing.ResidentRooms(); slices.Contains(rooms, document.ID) {
		t.Fatalf("the removing process still holds the room of %s: %v", document.ID, rooms)
	}
	// The person who held it open on the second process is left holding no live room: the close
	// takes their socket with it, and their browser reconnects to a document that has gone.
	waitForDocumentConnectionClose(t, connection)
	// No append of either room reached the database after the removal, and neither process takes
	// the room up again: a settle window passes with doc_updates holding nothing of the document
	// and no room of it resident anywhere.
	if testwait.Holds(time.Second, testwait.Poll, func() bool {
		return documentAppendCount(t, database, document.ID) > 0 ||
			slices.Contains(holding.ResidentRooms(), document.ID) ||
			slices.Contains(removing.ResidentRooms(), document.ID)
	}) {
		t.Fatalf("after the removal of %s the store holds %d appends of it, the second process the rooms %v and the removing process %v",
			document.ID, documentAppendCount(t, database, document.ID), holding.ResidentRooms(), removing.ResidentRooms())
	}
	assertNoRowNamesTheDocument(t, database, document.ID)
}

// documentAppendCount is how many stored appends the database holds of a document's room. After
// the document's removal it is zero: the rows went with the row they are keyed by, and an append
// a room left open over that row made afterwards could not be stored at all.
func documentAppendCount(t *testing.T, database *store.Store, artifactID string) int {
	t.Helper()
	var appends int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*)::int from doc_updates where artifact_id = $1::uuid`, artifactID).Scan(&appends); err != nil {
		t.Fatalf("count the document's stored appends: %v", err)
	}
	return appends
}
