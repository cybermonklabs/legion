package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/grapherase"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions"
)

// eraseTestWindow is the bound the erase waits out after its commit in these tests, in place of
// api.StoreTreesTimeout's 30 seconds: long enough for the test to plant a tree inside it once it
// has read the commit, short enough for a run.
const eraseTestWindow = 4 * time.Second

// confirmTries is how many listings confirmErase confirms, in all, before it gives up on a
// listing that keeps moving.
const confirmTries = 3

// confirmErase is what an operator runs: the erase of kind:id without --confirm, which reads its
// listing and removes nothing, then with the digest that run printed. These tests run it against a
// live server whose ticks write rows that derive from the node - a comment's windows among them -
// so a tick between the two runs moves the listing, and the confirmed run refuses it (Mismatch),
// as it refuses an operator's. The operator then reads the listing again and confirms that digest,
// and so does confirmErase, confirmTries times in all; when the last confirmed run still reads
// another listing it answers an error naming both digests. Any other refusal it answers as the
// erase does, the refused Erasure with no error.
func confirmErase(ctx context.Context, databaseURL, kind, id string, window time.Duration) (*grapherase.Erasure, error) {
	for try := 1; ; try++ {
		listing, err := grapherase.Erase(ctx, databaseURL, kind, id, "", 0)
		if err != nil {
			return nil, fmt.Errorf("erase --node %s:%s without --confirm: %w", kind, id, err)
		}
		if listing.Applied {
			return nil, fmt.Errorf("erase --node %s:%s without --confirm removed something", kind, id)
		}
		erasure, err := grapherase.Erase(ctx, databaseURL, kind, id, listing.Digest, window)
		if err != nil || !erasure.Mismatch() {
			return erasure, err
		}
		if try == confirmTries {
			return erasure, fmt.Errorf("erase --node %s:%s refused each of %d listings it was given: the last confirmed run was given %s and read a listing of digest %s",
				kind, id, confirmTries, erasure.Confirmed, erasure.Digest)
		}
	}
}

// blockHolding is the node address of the block of document whose text, at the latest version
// that recorded one holding text, holds it - a block a later version dropped included - as
// ref_key, '#' and the block id (migration 0121), after reading that the store holds its node.
func blockHolding(t *testing.T, database *store.Store, document, text string) string {
	t.Helper()
	var address string
	if err := database.Pool.QueryRow(context.Background(), `
		select a.ref_key || '#' || t.block_id from document_block_texts t join artifacts a on a.id = t.artifact_id
		where t.artifact_id = $1::uuid and position($2 in t.text) > 0
		  and exists (select 1 from graph_nodes n where n.kind = 'block' and n.id = a.ref_key || '#' || t.block_id)
		order by t.version desc limit 1`, document, text).Scan(&address); err != nil {
		t.Fatalf("read the block node of %s whose text holds %q: %v", document, text, err)
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
func TestTheEraseReachesASpecAnEditedHistoryAndATreeStoredAfterItsCommit(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	databaseURL := database.Pool.Config().ConnString()
	server := startDispatchProcess(t, databaseURL)
	server.waitHealthy(t)
	server.call(t, http.MethodPost, "/api/v1/projects", `{"key":"ERS","name":"Erase"}`, http.StatusCreated)

	secret := eraseTestSecret()
	leak := "The passphrase is " + secret + " and nobody else may read it."
	issue, spec := createEraseIssue(t, server, "ERS", "A spec that held a secret", "# Spec\n\n"+leak+"\n")
	notes := uploadEraseDocument(t, server, issue, "notes.md", "# Notes\n\n"+leak+"\n")
	uploadEraseDocument(t, server, issue, "notes.md", "# Notes\n\nNothing secret is left here.\n")
	if text := eraseDocumentText(t, server, notes); strings.Contains(text, secret) {
		t.Fatalf("the edit left the secret in the live text of the notes: %q", text)
	}
	// The uploads owe the notes a settlement (doc_settlements_pending), and the issue's creation
	// owes the spec one, each run after the settle delay. Run after an erase, it would write a
	// version of its own from the live text the erase left - for the spec a version holding the
	// marker, with its event - so each erase is read against the settled document, which nothing
	// writes to again.
	waitForSettlementOwed(t, database, notes, false)
	waitForSettlementOwed(t, database, spec, false)

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
	// Whatever the document still owes runs before the checks below, so a settlement left owed
	// across the erase is seen writing, not missed because it ran after them.
	waitForSettlementOwed(t, database, notes, false)
	if held := updatesHolding(t, database, notes, secret); held != 0 {
		t.Errorf("after the erase %d stored update(s) of the notes still hold the secret's bytes", held)
	}
	var updates int
	if err := database.Pool.QueryRow(ctx, `select count(*) from doc_updates where artifact_id = $1`, notes).Scan(&updates); err != nil {
		t.Fatalf("count the notes' stored updates: %v", err)
	}
	if updates != 1 {
		t.Errorf("the notes hold %d stored update(s) after the erase, want the one seed that replaced the history", updates)
	}
	if text := eraseDocumentText(t, server, notes); !strings.Contains(text, "Nothing secret is left here.") {
		t.Errorf("the notes no longer read their own text after the erase: %q", text)
	}
	if len(erased.Documents) != 1 || !strings.HasSuffix(erased.Documents[0].Document, "/notes-md") ||
		!erased.Documents[0].Read || erased.Documents[0].Held || erased.Documents[0].Left {
		t.Errorf("the erase of the notes' block lists the documents %+v, want the notes alone, their live text holding none of the text", erased.Documents)
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
	// Whatever the document still owes runs before the checks below, so a settlement left owed
	// across the erase is seen writing, not missed because it ran after them.
	waitForSettlementOwed(t, database, spec, false)
	var report bytes.Buffer
	erasure.Write(&report)
	t.Logf("the erase reported:\n%s", report.String())
	if held := issuesHolding(t, database, secret); held != 0 {
		t.Errorf("after the erase %d issue search vector(s) still hold the secret", held)
	}
	if !strings.Contains(report.String(), "  redacted: issues 1\n") {
		t.Errorf("the report counts no rewritten issue vector:\n%s", report.String())
	}
	if len(erasure.Documents) != 1 || !strings.HasSuffix(erasure.Documents[0].Document, "/spec") || !erasure.Documents[0].Held {
		t.Errorf("the erase of the spec's block lists the documents %+v, want the spec, its live text holding the text", erasure.Documents)
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
		update document_block_texts set text = replace(text, $2, $3) where artifact_id = $1::uuid and position($2 in text) > 0`,
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
	if unredacted := erasure.Unredacted(); len(unredacted) != 1 || !strings.HasSuffix(unredacted[0], "/notes-md") {
		t.Errorf("the erase names %v as not redacted, want the notes", unredacted)
	}
	if !strings.Contains(report.String(), "/notes-md of "+issue+": not redacted: ") {
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
	// The upload owes the document a settlement (doc_settlements_pending), which runs after the
	// settle delay. Run after the erase, it would write a version of its own from the live text the
	// erase left - a version holding the marker, with its event - so the erase is read against the
	// settled document, which nothing writes to again.
	waitForSettlementOwed(t, database, document, false)

	erasure, err := confirmErase(ctx, databaseURL, "block", block, time.Millisecond)
	if err != nil || !erasure.Applied {
		t.Fatalf("erase %s: applied %t, %v", block, erasure != nil && erasure.Applied, err)
	}
	// Whatever the document still owes runs before the checks below, so a settlement left owed
	// across the erase is seen writing, not missed because it ran after them.
	waitForSettlementOwed(t, database, document, false)
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

// R5: the keyword fill writes a comment's keyword version (search_text_versions) from its event,
// on its own, while the server serves: the comment's text in its body and its words in its
// vector, which derive from the comment. A comment holding a word no other row holds is written
// through the API, and the test waits for the server's own fill to write its version, which holds
// the word (the control). The comment's erase takes that version, and no row of the table holds
// the word afterwards. The table is then emptied, as the fill empties it for a log that was reset,
// and the fill runs again over the whole log (textversions.FillToHorizon): the erase redacted the
// comment's events in place, so every event decodes, and no row the fill writes again holds the
// word. The test logs what it wrote again of the erased comment.
func TestTheCommentEraseTakesItsKeywordVersionsAndTheFillWritesNoneOfItsTextAgain(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	databaseURL := database.Pool.Config().ConnString()
	server := startDispatchProcess(t, databaseURL)
	server.waitHealthy(t)
	server.call(t, http.MethodPost, "/api/v1/projects", `{"key":"ERK","name":"Erase keywords"}`, http.StatusCreated)

	secret := eraseTestSecret()
	issue, _ := createEraseIssue(t, server, "ERK", "A comment that held a secret", "# Spec\n\nNothing secret is written here.\n")
	comment := writeEraseComment(t, server, issue, "The passphrase is "+secret+" and nobody else may read it.")
	deadline := time.Now().Add(time.Minute)
	for len(eraseKeywordVersions(t, database, comment)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the server's keyword fill wrote no keyword version of comment %s within a minute", comment)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if held := eraseKeywordVersionsHolding(t, database, secret); len(held) == 0 {
		t.Fatalf("before the erase no keyword version holds the secret, so this test proves nothing: comment %s's versions read %q",
			comment, eraseKeywordVersions(t, database, comment))
	}

	erasure, err := confirmErase(ctx, databaseURL, "comment", comment, time.Millisecond)
	if err != nil || !erasure.Applied {
		t.Fatalf("erase comment:%s: applied %t, %v", comment, erasure != nil && erasure.Applied, err)
	}
	var report bytes.Buffer
	erasure.Write(&report)
	t.Logf("the erase reported:\n%s", report.String())
	if !strings.Contains(report.String(), "    delete search_text_versions comment "+comment+" event_seq ") {
		t.Errorf("the listing names no keyword version of comment %s as a row the run deletes:\n%s", comment, report.String())
	}
	if versions := eraseKeywordVersions(t, database, comment); len(versions) != 0 {
		t.Errorf("after the erase the keyword table holds %d version(s) of comment %s, want none: %q", len(versions), comment, versions)
	}
	if held := eraseKeywordVersionsHolding(t, database, secret); len(held) != 0 {
		t.Errorf("after the erase the keyword versions %q hold the secret", held)
	}

	// The fill again, from no row, over the log the erase redacted, with the server stopped so
	// the test's fill is the one that writes.
	server.Terminate(t)
	server.WaitExit(t, "SIGTERM")
	if _, err := database.Pool.Exec(ctx, `delete from search_text_versions; delete from search_text_versions_fill`); err != nil {
		t.Fatalf("empty the keyword table and its cursor row: %v", err)
	}
	if err := textversions.FillToHorizon(ctx, database, events.NewHorizon(database.Pool)); err != nil {
		t.Fatalf("fill the keyword table again over the log the erase redacted: %v", err)
	}
	if held := eraseKeywordVersionsHolding(t, database, secret); len(held) != 0 {
		t.Errorf("after the fill ran again the keyword versions %q hold the secret", held)
	}
	t.Logf("the fill wrote again %d keyword version(s) of the erased comment %s: %q",
		len(eraseKeywordVersions(t, database, comment)), comment, eraseKeywordVersions(t, database, comment))
}

// R5: an event carries a comment's body whole up to contracts.DispatchEventBodyMax (32,000 UTF-16
// units) and only its first part past that (model.EventBody), cut at the last blank line within
// the bound, else at the last line break, else at the last character. A block holds up to 32,000
// units as well, so a body whose first block fills the bound and breaks its lines with no blank
// line between them is cut inside that block: the event holds the block's head, which no erased
// text matches whole. The comment here is that body, its secret on the first line, written
// through the API, and the test reads its comment.created event cut inside the first block with
// the secret in it (the control). The comment's erase takes every byte of the cut body out of its
// events, and no column of the database holds the secret afterwards. The keyword table is then
// emptied, as the fill empties it for a log that was reset, and the fill runs again over the
// whole log (textversions.FillToHorizon): the comment has no row and no blocks left, so the fill
// reads its cut body from the event (body_from 'cut'), and no version it writes holds the secret.
func TestTheCommentEraseTakesTheCutHeadOfALongBodyOutOfItsEvents(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	databaseURL := database.Pool.Config().ConnString()
	server := startDispatchProcess(t, databaseURL)
	server.waitHealthy(t)
	server.call(t, http.MethodPost, "/api/v1/projects", `{"key":"ERC","name":"Erase a cut body"}`, http.StatusCreated)

	secret := eraseTestSecret()
	first := cutBodyBlock("The passphrase is "+secret+" and nobody else may read it.", contracts.DispatchEventBodyMax)
	body := first + "\n\nA second block of the same comment."
	issue, _ := createEraseIssue(t, server, "ERC", "A comment past the event's bound", "# Spec\n\nNothing secret is written here.\n")
	comment := writeEraseComment(t, server, issue, body)
	var cut string
	var truncated bool
	if err := database.Pool.QueryRow(ctx, `
		select payload->>'body', coalesce((payload->>'body_truncated')::boolean, false)
		from events where type = 'comment.created' and payload->>'id' = $1`, comment).Scan(&cut, &truncated); err != nil {
		t.Fatalf("read comment %s's comment.created event: %v", comment, err)
	}
	if !truncated || !strings.Contains(cut, secret) || len(cut) >= len(first) || !strings.HasPrefix(first, cut) {
		t.Fatalf("comment %s's comment.created event carries %d bytes of its %d-byte first block, truncated %t, holding the secret %t; want the head of that block alone, cut inside it with the secret in it, so this test would prove nothing",
			comment, len(cut), len(first), truncated, strings.Contains(cut, secret))
	}

	erasure, err := confirmErase(ctx, databaseURL, "comment", comment, time.Millisecond)
	if err != nil || !erasure.Applied {
		t.Fatalf("erase comment:%s: applied %t, %v", comment, erasure != nil && erasure.Applied, err)
	}
	var held int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from events
		where (payload->>'id' = $1 or payload->>'comment_id' = $1) and position($2 in payload::text) > 0`,
		comment, secret).Scan(&held); err != nil {
		t.Fatalf("read comment %s's events after the erase: %v", comment, err)
	}
	if held != 0 {
		t.Errorf("after the erase %d event(s) of comment %s still hold the secret in the head of the body their event cut", held, comment)
	}
	if places, searched := columnsHolding(t, database, secret); len(places) > 0 {
		t.Errorf("after the erase the secret is still in %s (%d columns searched)", strings.Join(places, ", "), searched)
	}

	// The fill again, from no row, over the log the erase redacted, with the server stopped so
	// the test's fill is the one that writes.
	server.Terminate(t)
	server.WaitExit(t, "SIGTERM")
	if _, err := database.Pool.Exec(ctx, `delete from search_text_versions; delete from search_text_versions_fill`); err != nil {
		t.Fatalf("empty the keyword table and its cursor row: %v", err)
	}
	if err := textversions.FillToHorizon(ctx, database, events.NewHorizon(database.Pool)); err != nil {
		t.Fatalf("fill the keyword table again over the log the erase redacted: %v", err)
	}
	if held := eraseKeywordVersionsHolding(t, database, secret); len(held) != 0 {
		t.Errorf("after the fill ran again the keyword versions %q hold the secret", held)
	}
	t.Logf("the fill wrote again %q of the erased comment %s", eraseKeywordVersions(t, database, comment), comment)
}

// cutBodyBlock is one paragraph of exactly units ASCII characters whose lines end in a single line
// break and never in a blank line, its first line opening with line, and its last character none
// of white space, so the block's stored text is the whole of it: a block that fills an event's
// bound, which an event's cut (model.EventBody) can only split at one of its own line breaks.
func cutBodyBlock(line string, units int) string {
	var block strings.Builder
	block.WriteString(line)
	for number := 2; block.Len() < units; number++ {
		block.WriteString(fmt.Sprintf("\nLine %d of a long paragraph.", number))
	}
	cut := []byte(block.String()[:units])
	if last := len(cut) - 1; cut[last] == '\n' || cut[last] == ' ' {
		cut[last] = 'x'
	}
	return string(cut)
}

// eraseKeywordVersions is every keyword version (search_text_versions) of comment, as
// `event_seq <n> body_from <source>: <body>`, in event_seq order.
func eraseKeywordVersions(t *testing.T, database *store.Store, comment string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select 'event_seq ' || event_seq || ' body_from ' || body_from || ': ' || body
		from search_text_versions where kind = 'comment' and id = $1 order by event_seq`, comment)
	if err != nil {
		t.Fatalf("read the keyword versions of comment %s: %v", comment, err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the keyword versions of comment %s: %v", comment, err)
	}
	return versions
}

// eraseKeywordVersionsHolding names, by key, every keyword version whose body or vector holds
// text, the vector read in its text form, which spells each lexeme.
func eraseKeywordVersionsHolding(t *testing.T, database *store.Store, text string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select kind || ' ' || id || ' event_seq ' || event_seq from search_text_versions
		where position($1 in body) > 0 or position($1 in search::text) > 0 order by kind, id, event_seq`, text)
	if err != nil {
		t.Fatalf("read the keyword versions holding the text: %v", err)
	}
	held, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the keyword versions holding the text: %v", err)
	}
	return held
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
	body := server.documentUpload(t, issue, name, markdown, dispatchTestLogin)
	answer := server.send(t, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", body, asUser(dispatchTestLogin))
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

// documentUpload is the JSON body of an upload of content as the document issue holds under name,
// sent as login. An upload replacing a document names the token of the version it replaces, which
// Dispatch refuses every replacement without (docs.ErrPreconditionRequired), so the body names the
// token the document carries now; a document's first version replaces nothing and names none.
func (p *dispatchProcess) documentUpload(t *testing.T, issue, name, content, login string) string {
	t.Helper()
	upload := map[string]any{"name": name, "content": content}
	answer := p.send(t, http.MethodGet, "/api/v1/issues/"+issue+"/artifacts/"+url.PathEscape(name)+"/text", "", asUser(login))
	switch answer.status {
	case http.StatusNotFound:
	case http.StatusOK:
		var document struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(answer.body, &document); err != nil || document.Token == "" {
			t.Fatalf("read the token of %s's document %s: %v: %s", issue, name, err, answer)
		}
		upload["precondition"] = map[string]string{"document": document.Token}
	default:
		t.Fatalf("read %s's document %s: %s", issue, name, answer)
	}
	body, err := json.Marshal(upload)
	if err != nil {
		t.Fatalf("encode the upload of %s: %v", name, err)
	}
	return string(body)
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

// updatesHolding counts the stored updates of document whose bytes hold text's.
func updatesHolding(t *testing.T, database *store.Store, document, text string) int {
	t.Helper()
	var held int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from doc_updates
		where artifact_id = $1 and position(convert_to($2, 'UTF8') in update) > 0`, document, text).Scan(&held); err != nil {
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
