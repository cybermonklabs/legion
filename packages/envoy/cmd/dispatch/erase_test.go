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

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// eraseTestWindow is the bound the erase waits out after its commit in these tests, in place of
// api.StoreTreesTimeout's 30 seconds: long enough for the test to plant a tree inside it once it
// has read the commit, short enough for a run.
const eraseTestWindow = 4 * time.Second

// The erase reaches three stores the acceptance driver's run does not (graph-erase.sh writes the
// secret into a document that is no issue's spec and whose live text still holds it), each held
// against a control that finds what it looks for before the erase, so an assertion that finds
// nothing after it is one that could have found something:
//
//   - the issue's own search vector, which holds its spec's text: the spec's version holding the
//     secret goes, and no issues.search holds the secret afterwards;
//   - a document whose live text an edit took the secret out of while its stored history still
//     holds the bytes: its history is replaced by one update, and no doc_updates row holds them;
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
	comment := writeEraseComment(t, server, issue, leak)

	// The controls: each store holds the secret before the erase, read by the very statement
	// that must read none of it afterwards.
	if held := issuesHolding(t, database, secret); held != 1 {
		t.Fatalf("before the erase %d issue search vector(s) hold the secret, want the spec's issue's one", held)
	}
	if held := updatesHolding(t, database, notes, secret); held == 0 {
		t.Fatalf("before the erase no stored update of the notes holds the secret's bytes, so the history check proves nothing")
	}
	var body string
	if err := database.Pool.QueryRow(ctx, `select body from comments where id = $1`, comment).Scan(&body); err != nil {
		t.Fatalf("read the comment's body: %v", err)
	}
	sum := sha256.Sum256([]byte(body))

	type erased struct {
		erasure *graphcensus.Erasure
		err     error
	}
	done := make(chan erased, 1)
	go func() {
		erasure, err := graphcensus.Erase(ctx, databaseURL, "comment", comment, true, eraseStores(eraseTestWindow))
		done <- erased{erasure, err}
	}()
	// The tree a read that began before the commit stores after it: planted the moment the
	// commit is visible, which is inside the window the erase then waits out.
	deadline := time.Now().Add(time.Minute)
	for {
		var records int
		if err := database.Pool.QueryRow(ctx, `select count(*) from graph_erasures where node_id = $1`, comment).Scan(&records); err != nil {
			t.Fatalf("read whether the erase committed: %v", err)
		}
		if records == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the erase of %s never committed", comment)
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
		t.Fatalf("erase %s: %v", comment, result.err)
	}
	erasure := result.erasure
	if !erasure.Applied {
		t.Fatalf("the erase of %s removed nothing", comment)
	}
	var report bytes.Buffer
	erasure.Write(&report)
	t.Logf("the erase reported:\n%s", report.String())

	// The spec's issue.
	if held := issuesHolding(t, database, secret); held != 0 {
		t.Errorf("after the erase %d issue search vector(s) still hold the secret", held)
	}
	if !strings.Contains(report.String(), "  redacted: issues 1\n") {
		t.Errorf("the report counts no rewritten issue vector:\n%s", report.String())
	}

	// The notes, whose live text held none of it.
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
	var notesListed, specListed bool
	for _, document := range erasure.Documents {
		switch {
		case strings.HasSuffix(document.Document, "/notes-md"):
			notesListed = true
			if document.Held || document.Left {
				t.Errorf("the notes are listed as holding the text in their live text (held %v, left %v), want the history alone",
					document.Held, document.Left)
			}
		case strings.HasSuffix(document.Document, "/spec"):
			specListed = true
			if !document.Held {
				t.Errorf("the spec is listed as not holding the text in its live text")
			}
		}
	}
	if !notesListed || !specListed {
		t.Errorf("the erase lists %+v, want the spec %s and the notes %s", erasure.Documents, spec, notes)
	}

	// The tree planted inside the window.
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

// A document whose live markdown spells the erased text otherwise than the comment writes it -
// here the comment's `2*3*` is literal asterisks in the document, which its markdown escapes -
// is listed as not redacted, and the erase refuses it, removing nothing, rather than report
// success over the bytes the seed of its history would keep. The control is the bytes
// themselves: the document's stored history holds them before the erase and still after it.
func TestTheEraseRefusesADocumentWhoseMarkdownSpellsTheTextOtherwise(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	databaseURL := database.Pool.Config().ConnString()
	server := startDispatchProcess(t, databaseURL)
	server.waitHealthy(t)
	server.call(t, http.MethodPost, "/api/v1/projects", `{"key":"ERE","name":"Erase"}`, http.StatusCreated)

	secret := "2*3*" + eraseTestSecret()
	issue, _ := createEraseIssue(t, server, "ERE", "A spec beside the notes", "# Spec\n\nNothing here.\n")
	notes := uploadEraseDocument(t, server, issue, "notes.md",
		"# Notes\n\n"+strings.ReplaceAll(secret, "*", `\*`)+"\n")
	if text := eraseDocumentText(t, server, notes); strings.Contains(text, secret) {
		t.Fatalf("the notes' markdown spells the text as the comment does, so this proves nothing: %q", text)
	}
	comment := writeEraseComment(t, server, issue, secret)
	if held := updatesHolding(t, database, notes, secret); held == 0 {
		t.Fatalf("no stored update of the notes holds the text's bytes, so the refusal proves nothing")
	}

	erasure, err := graphcensus.Erase(ctx, databaseURL, "comment", comment, true, eraseStores(eraseTestWindow))
	if err != nil {
		t.Fatalf("erase %s: %v", comment, err)
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
	var comments int
	if err := database.Pool.QueryRow(ctx, `select count(*) from comments where id = $1`, comment).Scan(&comments); err != nil {
		t.Fatalf("count the comment: %v", err)
	}
	if comments != 1 {
		t.Errorf("the refused erase removed the comment")
	}
	if held := updatesHolding(t, database, notes, secret); held == 0 {
		t.Errorf("the refused erase changed the notes' history")
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
