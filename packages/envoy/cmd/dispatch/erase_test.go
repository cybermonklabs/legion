package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/grapherase"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// eraseTestWindow is the bound the erase waits out after its commit in these tests, in place of
// api.StoreTreesTimeout's 30 seconds: long enough for the test to plant a tree inside it once it
// has read the commit, short enough for a run.
const eraseTestWindow = 4 * time.Second

// confirmErase is what an operator runs: the erase of kind:id without --confirm, which reads its
// listing and removes nothing, then with the digest that run printed.
func confirmErase(ctx context.Context, databaseURL, kind, id string, window time.Duration) (*grapherase.Erasure, error) {
	listing, err := grapherase.Erase(ctx, databaseURL, kind, id, "", 0)
	if err != nil {
		return nil, fmt.Errorf("erase --node %s:%s without --confirm: %w", kind, id, err)
	}
	if listing.Applied {
		return nil, fmt.Errorf("erase --node %s:%s without --confirm removed something", kind, id)
	}
	return grapherase.Erase(ctx, databaseURL, kind, id, listing.Digest, window)
}

// blockHolding is the node address of the block of document whose text, at the latest version
// that recorded one holding text, holds it - a block a later version dropped included - as
// ref_key, '#' and the block id (migration 0121), after reading that the store holds its node. A
// converted document's blocks are keyed by its issue's room (refs.DocumentRoomSQL), each in the
// document its fragment holds it in (refs.BlockDocumentOrSQL), and its address is the room's. A
// block a version dropped has no issue_room_blocks row left, and its document is the one its
// node's last asserted in_document names, as the erase reads it.
func blockHolding(t *testing.T, database *store.Store, document, text string) string {
	t.Helper()
	var address string
	if err := database.Pool.QueryRow(context.Background(), `
		select a.ref_key || '#' || t.block_id from document_block_texts t join artifacts a on a.id = t.artifact_id
		where t.artifact_id = `+refs.DocumentRoomSQL("$1::uuid")+`
		  and `+refs.BlockDocumentOrSQL("t", `coalesce((select named.id from graph_tuples g join artifacts named on named.ref_key = g.to_id
		      where g.from_kind = 'block' and g.from_id = a.ref_key || '#' || t.block_id and g.kind = '`+refs.TupleInDocument+`'
		        and g.to_kind = 'artifact' and g.asserted
		      order by g.seq desc limit 1), t.artifact_id)`)+` = $1::uuid
		  and position($2 in t.text) > 0
		  and exists (select 1 from graph_nodes n where n.kind = 'block' and n.id = a.ref_key || '#' || t.block_id)
		order by t.version desc limit 1`, document, text).Scan(&address); err != nil {
		t.Fatalf("read the block node of %s whose text holds %q: %v", document, text, err)
	}
	return address
}

// roomAddress is the ref_key of the room document is edited in (refs.DocumentRoomSQL): its
// issue's tree for a converted document, which holds the room's one stored history, and the
// document itself otherwise. An erase that replaces a document's history lists that room.
func roomAddress(t *testing.T, database *store.Store, document string) string {
	t.Helper()
	var address string
	if err := database.Pool.QueryRow(context.Background(),
		`select ref_key from artifacts where id = `+refs.DocumentRoomSQL("$1::uuid"), document).Scan(&address); err != nil {
		t.Fatalf("read the room of %s: %v", document, err)
	}
	return address
}

// The erase of a block reaches the stores of its own document the acceptance driver's run does
// not (graph-erase.sh erases a block of a document that is no issue's spec and whose live text
// still holds it), each held against a control that finds what it looks for before the erase, so
// an assertion that finds nothing after it is one that could have found something:
//
//   - the issue's own search vector, which holds its spec's text: the spec's block holding the
//     secret is erased, the spec's version holding it goes, and no issues.search holds the secret
//     afterwards;
//   - a document whose live text an edit took the secret out of while its stored history still
//     holds the bytes: the block the edit dropped is erased, its history is replaced by one
//     update, and no doc_updates row holds them;
//   - the display tree of an erased text stored inside the window after the commit, as a read
//     that began before the commit stores one: the sweep after the window deletes it, and the
//     report counts it.
//
// A converted issue's documents share its room's one stored history, which the erase replaces
// whole, so the notes are a document of an issue of their own: in the spec's room the seed of
// the notes' erase would hold the spec's live line, which repeats the secret.
func TestTheEraseReachesASpecAnEditedHistoryAndATreeStoredAfterItsCommit(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	databaseURL := database.Pool.Config().ConnString()
	server := startDispatchProcess(t, databaseURL)
	server.waitHealthy(t)
	server.call(t, http.MethodPost, "/api/v1/projects", `{"key":"ERS","name":"Erase"}`, http.StatusCreated)

	secret := eraseTestSecret()
	leak := "The passphrase is " + secret + " and nobody else may read it."
	_, spec := createEraseIssue(t, server, "ERS", "A spec that held a secret", "# Spec\n\n"+leak+"\n")
	notesIssue, _ := createEraseIssue(t, server, "ERS", "Notes of their own", "# Spec\n\nNothing here.\n")
	notes := uploadEraseDocument(t, server, notesIssue, "notes.md", "# Notes\n\n"+leak+"\n")
	uploadEraseDocument(t, server, notesIssue, "notes.md", "# Notes\n\nNothing secret is left here.\n")
	if text := eraseDocumentText(t, server, notes); strings.Contains(text, secret) {
		t.Fatalf("the edit left the secret in the live text of the notes: %q", text)
	}

	// The controls: each store holds the secret before the erase, read by the very statement
	// that must read none of it afterwards.
	if held := issuesHolding(t, database, secret); held != 1 {
		t.Fatalf("before the erase %d issue search vector(s) hold the secret, want the spec's issue's one", held)
	}
	if held := updatesHolding(t, database, notes, secret); held == 0 {
		t.Fatalf("before the erase no stored update of the notes holds the secret's bytes, so the history check proves nothing")
	}
	specBlock, notesBlock := blockHolding(t, database, spec, secret), blockHolding(t, database, notes, secret)

	// The notes, whose live text holds none of it: the block the edit dropped is what held it.
	erased, err := confirmErase(ctx, databaseURL, "block", notesBlock, time.Millisecond)
	if err != nil || !erased.Applied {
		t.Fatalf("erase %s: applied %t, %v", notesBlock, erased != nil && erased.Applied, err)
	}
	if held := updatesHolding(t, database, notes, secret); held != 0 {
		t.Errorf("after the erase %d stored update(s) of the notes still hold the secret's bytes", held)
	}
	var updates int
	if err := database.Pool.QueryRow(ctx, `select count(*) from doc_updates where artifact_id = `+refs.DocumentRoomSQL("$1::uuid"), notes).Scan(&updates); err != nil {
		t.Fatalf("count the notes' stored updates: %v", err)
	}
	if updates != 1 {
		t.Errorf("the notes hold %d stored update(s) after the erase, want the one seed that replaced the history", updates)
	}
	if text := eraseDocumentText(t, server, notes); !strings.Contains(text, "Nothing secret is left here.") {
		t.Errorf("the notes no longer read their own text after the erase: %q", text)
	}
	if len(erased.Documents) != 1 || erased.Documents[0].Document != roomAddress(t, database, notes) ||
		!erased.Documents[0].Read || erased.Documents[0].Held || erased.Documents[0].Left {
		t.Errorf("the erase of the notes' block lists the documents %+v, want the notes' room alone, their live text holding none of the text", erased.Documents)
	}

	// The spec, with a tree a read that began before the commit stores after it: planted the
	// moment the commit is visible, which is inside the window the erase then waits out.
	sum := sha256.Sum256([]byte(leak))
	type answer struct {
		erasure *grapherase.Erasure
		err     error
	}
	done := make(chan answer, 1)
	go func() {
		erasure, err := confirmErase(ctx, databaseURL, "block", specBlock, eraseTestWindow)
		done <- answer{erasure, err}
	}()
	deadline := time.Now().Add(time.Minute)
	for {
		var records int
		if err := database.Pool.QueryRow(ctx, `select count(*) from graph_erasures where node_id = $1`, specBlock).Scan(&records); err != nil {
			t.Fatalf("read whether the erase committed: %v", err)
		}
		if records == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the erase of %s never committed", specBlock)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into text_trees (sum, mode, version, tree) values ($1, 0, 1, '\x7b7d')
		on conflict do nothing`, sum[:]); err != nil {
		t.Fatalf("plant the tree of the erased text inside the window: %v", err)
	}
	if planted := treesUnder(t, database, sum); planted != 1 {
		t.Fatalf("the planted tree reads %d row(s), want 1, so the sweep check proves nothing", planted)
	}
	result := <-done
	if result.err != nil {
		t.Fatalf("erase %s: %v", specBlock, result.err)
	}
	erasure := result.erasure
	if !erasure.Applied {
		t.Fatalf("the erase of %s removed nothing", specBlock)
	}
	var report bytes.Buffer
	erasure.Write(&report)
	t.Logf("the erase reported:\n%s", report.String())
	if held := issuesHolding(t, database, secret); held != 0 {
		t.Errorf("after the erase %d issue search vector(s) still hold the secret", held)
	}
	if !strings.Contains(report.String(), "  redacted: issues 1\n") {
		t.Errorf("the report counts no rewritten issue vector:\n%s", report.String())
	}
	if len(erasure.Documents) != 1 || erasure.Documents[0].Document != roomAddress(t, database, spec) || !erasure.Documents[0].Held {
		t.Errorf("the erase of the spec's block lists the documents %+v, want the spec's room, its live text holding the text", erasure.Documents)
	}
	if planted := treesUnder(t, database, sum); planted != 0 {
		t.Errorf("the tree stored inside the window after the commit is still there")
	}
	if erasure.Swept < 1 {
		t.Errorf("the sweep after the commit counted %d tree(s), want the one planted", erasure.Swept)
	}
	if !strings.Contains(report.String(), fmt.Sprintf("  text_trees swept after the commit: %d\n", erasure.Swept)) {
		t.Errorf("the report has no sweep line:\n%s", report.String())
	}
}

// A document whose live markdown spells the erased text otherwise than the block's recorded text
// writes it - here the block's text is recorded with literal asterisks, as a build that recorded
// a block's text unescaped would leave it, where the markdown escapes them - is listed as not
// redacted, and the erase refuses it, removing nothing, rather than report success over the bytes
// the seed of its history would keep. The control is the bytes themselves: the document's stored
// history holds them before the erase and still after it.
func TestTheEraseRefusesADocumentWhoseMarkdownSpellsTheTextOtherwise(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	databaseURL := database.Pool.Config().ConnString()
	server := startDispatchProcess(t, databaseURL)
	server.waitHealthy(t)
	server.call(t, http.MethodPost, "/api/v1/projects", `{"key":"ERE","name":"Erase"}`, http.StatusCreated)

	secret := "2*3*" + eraseTestSecret()
	escaped := strings.ReplaceAll(secret, "*", `\*`)
	issue, _ := createEraseIssue(t, server, "ERE", "A spec beside the notes", "# Spec\n\nNothing here.\n")
	notes := uploadEraseDocument(t, server, issue, "notes.md", "# Notes\n\n"+escaped+"\n")
	if text := eraseDocumentText(t, server, notes); strings.Contains(text, secret) {
		t.Fatalf("the notes' markdown spells the text as the block does, so this proves nothing: %q", text)
	}
	block := blockHolding(t, database, notes, escaped)
	if tag, err := database.Pool.Exec(ctx, `
		update document_block_texts set text = replace(text, $2, $3)
		where artifact_id = `+refs.DocumentRoomSQL("$1::uuid")+` and position($2 in text) > 0`,
		notes, escaped, secret); err != nil || tag.RowsAffected() == 0 {
		t.Fatalf("record the block's text unescaped: %d rows, %v", tag.RowsAffected(), err)
	}
	if held := updatesHolding(t, database, notes, secret); held == 0 {
		t.Fatalf("no stored update of the notes holds the text's bytes, so the refusal proves nothing")
	}

	erasure, err := confirmErase(ctx, databaseURL, "block", block, eraseTestWindow)
	if err != nil {
		t.Fatalf("erase %s: %v", block, err)
	}
	var report bytes.Buffer
	erasure.Write(&report)
	t.Logf("the erase reported:\n%s", report.String())
	if erasure.Applied {
		t.Fatalf("the erase removed what it could not redact")
	}
	room := roomAddress(t, database, notes)
	if unredacted := erasure.Unredacted(); len(unredacted) != 1 || unredacted[0] != room {
		t.Errorf("the erase names %v as not redacted, want the notes' room %s", unredacted, room)
	}
	if !strings.Contains(report.String(), room+" of "+issue+": not redacted: ") {
		t.Errorf("the report lists no not-redacted line for the notes:\n%s", report.String())
	}
	var nodes int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_nodes where kind = 'block' and id = $1`, block).Scan(&nodes); err != nil {
		t.Fatalf("count the block's node: %v", err)
	}
	if nodes != 1 {
		t.Errorf("the refused erase removed the block's node")
	}
	if held := updatesHolding(t, database, notes, secret); held == 0 {
		t.Errorf("the refused erase changed the notes' history")
	}
}

// R5: a block's erase takes its whole text out of every row that derives from it, not the first
// refs.BlockExcerptRunes runes its document_blocks excerpt holds. The block is a paragraph of more
// than 480 runes whose secret stands after rune 480, so the excerpt holds none of the secret while
// the document's stored history does (the controls). After the erase no stored update holds it,
// the live text holds it nowhere, and the search of every column of every table finds it in no
// row, none other holding those bytes.
func TestTheBlockEraseTakesATextPastItsExcerpt(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	databaseURL := database.Pool.Config().ConnString()
	server := startDispatchProcess(t, databaseURL)
	server.waitHealthy(t)
	server.call(t, http.MethodPost, "/api/v1/projects", `{"key":"ERX","name":"Erase"}`, http.StatusCreated)

	secret := eraseTestSecret()
	paragraph := strings.Repeat("word ", 100) + "and past the excerpt the passphrase is " + secret + "."
	issue, _ := createEraseIssue(t, server, "ERX", "A long paragraph", "# Spec\n\nNothing here.\n")
	document := uploadEraseDocument(t, server, issue, "long.md", "# Long\n\n"+paragraph+"\n")
	block := blockHolding(t, database, document, secret)
	var excerpt string
	if err := database.Pool.QueryRow(ctx, `select excerpt from document_blocks where node_address = $1`, block).Scan(&excerpt); err != nil {
		t.Fatalf("read the excerpt of %s: %v", block, err)
	}
	if strings.Contains(excerpt, secret) {
		t.Fatalf("the excerpt of %s holds the secret, so this proves nothing past it", block)
	}
	if held := updatesHolding(t, database, document, secret); held == 0 {
		t.Fatalf("no stored update of %s holds the secret, so the history check proves nothing", document)
	}

	erasure, err := confirmErase(ctx, databaseURL, "block", block, time.Millisecond)
	if err != nil || !erasure.Applied {
		t.Fatalf("erase %s: applied %t, %v", block, erasure != nil && erasure.Applied, err)
	}
	if held := updatesHolding(t, database, document, secret); held != 0 {
		t.Errorf("after the erase %d stored update(s) of %s still hold the secret past the excerpt", held, document)
	}
	if text := eraseDocumentText(t, server, document); strings.Contains(text, secret) || !strings.Contains(text, "# Long") {
		t.Errorf("the live text after the erase reads %q, want its heading and no secret", text)
	}
	if places, searched := columnsHolding(t, database, secret); len(places) > 0 {
		t.Errorf("after the erase the secret is still in %s (%d columns searched)", strings.Join(places, ", "), searched)
	}
}

// eraseTestSecret is one word of letters and digits no other run writes, which a search vector
// holds as one lexeme (graph-erase.sh's secret, for the same reason).
func eraseTestSecret() string {
	return fmt.Sprintf("passphrase%dx%dhunter2", time.Now().UnixNano(), os.Getpid())
}

// createEraseIssue creates an issue of project whose spec is spec, past the near-copy gate, and
// answers its key and its spec document's id.
func createEraseIssue(t *testing.T, server *dispatchProcess, project, title, spec string) (string, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"project": project, "title": title, "spec": spec, "force": true})
	if err != nil {
		t.Fatalf("encode the issue: %v", err)
	}
	created := server.call(t, http.MethodPost, "/api/v1/issues", string(body), http.StatusCreated)
	var issue struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	if err := json.Unmarshal(created, &issue); err != nil || issue.Key == "" || issue.PrimaryArtifactID == "" {
		t.Fatalf("create the issue %q: %v: %s", title, err, created)
	}
	return issue.Key, issue.PrimaryArtifactID
}

// eraseDocumentText is the live markdown GET /api/v1/artifacts/{id}/text answers for document.
func eraseDocumentText(t *testing.T, server *dispatchProcess, document string) string {
	t.Helper()
	answer := server.call(t, http.MethodGet, "/api/v1/artifacts/"+document+"/text", "", http.StatusOK)
	var text struct {
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal(answer, &text); err != nil {
		t.Fatalf("read the text of %s: %v: %s", document, err, answer)
	}
	return text.Markdown
}

// uploadEraseDocument uploads markdown as the document name of issue, which writes it a version,
// and answers its id.
func uploadEraseDocument(t *testing.T, server *dispatchProcess, issue, name, markdown string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"name": name, "content": markdown})
	if err != nil {
		t.Fatalf("encode the document: %v", err)
	}
	answer := server.send(t, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", string(body), asUser(dispatchTestLogin))
	if answer.status != http.StatusCreated && answer.status != http.StatusOK {
		t.Fatalf("upload %s: %s", name, answer)
	}
	var uploaded struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(answer.body, &uploaded); err != nil || uploaded.Artifact.ID == "" {
		t.Fatalf("read the upload of %s back: %v: %s", name, err, answer.body)
	}
	return uploaded.Artifact.ID
}

// writeEraseComment writes body as a comment of issue, as the test's person, and answers its id.
func writeEraseComment(t *testing.T, server *dispatchProcess, issue, body string) string {
	t.Helper()
	input, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		t.Fatalf("encode the comment: %v", err)
	}
	created := server.call(t, http.MethodPost, "/api/v1/issues/"+issue+"/comments", string(input), http.StatusCreated)
	var comment struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &comment); err != nil || comment.ID == "" {
		t.Fatalf("read the comment back: %v: %s", err, created)
	}
	return comment.ID
}

// issuesHolding counts the issues whose search vector holds text, read in its text form, where a
// vector spells each lexeme.
func issuesHolding(t *testing.T, database *store.Store, text string) int {
	t.Helper()
	var held int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from issues where position($1 in search::text) > 0`, text).Scan(&held); err != nil {
		t.Fatalf("count the issue vectors holding the text: %v", err)
	}
	return held
}

// updatesHolding counts the stored updates of the room document is edited in
// (refs.DocumentRoomSQL: its issue's room for a converted document) whose bytes hold text's.
func updatesHolding(t *testing.T, database *store.Store, document, text string) int {
	t.Helper()
	var held int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from doc_updates
		where artifact_id = `+refs.DocumentRoomSQL("$1::uuid")+` and position(convert_to($2, 'UTF8') in update) > 0`, document, text).Scan(&held); err != nil {
		t.Fatalf("count the stored updates holding the text: %v", err)
	}
	return held
}

// treesUnder counts the stored display trees keyed by sum, in any mode and at any version.
func treesUnder(t *testing.T, database *store.Store, sum [32]byte) int {
	t.Helper()
	var trees int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from text_trees where sum = $1`, sum[:]).Scan(&trees); err != nil {
		t.Fatalf("count the trees under the sum: %v", err)
	}
	return trees
}
