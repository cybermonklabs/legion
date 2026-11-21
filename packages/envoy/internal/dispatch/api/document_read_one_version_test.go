package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// versionReadStatements are the statements a text read of a document takes its version from, the
// first of which the split-read hook puts a write in front of: the read of the version by the
// markdown the read answers (versionOfText), and the newest version's number, which a build that
// does not hold R13 reads on a statement of its own (latestVersionNumber), so this test run against
// such a build puts the write where that build splits its answer.
var versionReadStatements = []string{
	"artifact_id = $1 and markdown = $2 order by number desc limit 1",
	"select max(number) from artifact_versions where artifact_id = $1",
}

// splitReadHook is the test store's query tracer: armed for one document, it runs a write once,
// just before the first statement a text read of that document takes its version from
// (versionReadStatements), on the goroutine that reads, so the write commits after the read
// rendered its text and before it reads the version.
type splitReadHook struct {
	mu       sync.Mutex
	artifact string
	write    func()
	fired    string
}

// arm makes the next version read of artifactID run write first.
func (h *splitReadHook) arm(artifactID string, write func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.artifact, h.write, h.fired = artifactID, write, ""
}

// firedOn is the statement the armed write ran in front of, empty while it has not run.
func (h *splitReadHook) firedOn() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fired
}

func (h *splitReadHook) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	h.mu.Lock()
	write := h.write
	if write == nil || len(data.Args) == 0 || fmt.Sprint(data.Args[0]) != h.artifact ||
		!slices.ContainsFunc(versionReadStatements, func(text string) bool { return strings.Contains(data.SQL, text) }) {
		h.mu.Unlock()
		return ctx
	}
	h.write, h.fired = nil, data.SQL
	h.mu.Unlock()
	write()
	return ctx
}

func (h *splitReadHook) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// documentTextRead is one of the reads of a document's text that answer its version: the two
// /api/v1 text reads and the two /api/v2 document reads with include=text, each with and without
// the blocks.
type documentTextRead struct {
	name   string
	path   string
	v2     bool
	blocks bool
}

var documentTextReads = []documentTextRead{
	{name: "v1_text_with_blocks", path: "/api/v1/artifacts/%s/text?blocks=true", blocks: true},
	{name: "v1_text", path: "/api/v1/artifacts/%s/text"},
	{name: "v2_include_text_and_blocks", path: "/api/v2/artifacts/%s?include=text,blocks", v2: true, blocks: true},
	{name: "v2_include_text", path: "/api/v2/artifacts/%s?include=text", v2: true},
}

// documentTextAnswer is what every documentTextRead answers of the text. Unsettled is a pointer
// so an answer that leaves the member out reads apart from one that says false.
type documentTextAnswer struct {
	Markdown  string `json:"markdown"`
	Token     string `json:"token"`
	Version   *int   `json:"version"`
	Unsettled *bool  `json:"unsettled"`
	Blocks    *[]any `json:"blocks"`
}

// oneVersionReadFixture is a server on the traced test store, with the document service's test
// hooks on for the write held between its commit and its publish.
type oneVersionReadFixture struct {
	handler   http.Handler
	database  *store.Store
	documents *docs.Service
	hook      *splitReadHook
}

func newOneVersionReadFixture(t *testing.T) *oneVersionReadFixture {
	t.Helper()
	hook := &splitReadHook{}
	database := storetest.TracedStore(t, storetest.Open(t), hook)
	// No settlement runs unasked, so no room loads behind a case that reads the stored document and
	// no version is written that a case did not make.
	documents := docs.New(docs.Deps{
		Store: database, Identity: headerIdentity(database), ServerURL: testServerURL, Settle: time.Hour, TestHooks: true,
	})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut the document service down: %v", err)
		}
	})
	seedPeople(t, database, "alice", "bob")
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: headerIdentity(database), AgentTokens: sharedAgentTokens(t, "agent-token"),
		Docs: documents, ServerURL: testServerURL,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), deps).Wait)
	if response := dispatchRequest(t, mux, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "READ", "name": "Read project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	return &oneVersionReadFixture{handler: mux, database: database, documents: documents, hook: hook}
}

// textReadDocument is a document a case reads: its issue, and the ask its ask block opened.
type textReadDocument struct {
	issueKey, artifactID, askID string
}

// readDocumentSpec is how a case's document starts: version 1 holds old line, after it an ask
// block where the case answers an ask, and a version holding no token where the case reads a
// version written before versions stored one.
type readDocumentSpec struct {
	inItsRoom, ask, noToken bool
}

// readAsk is the ask block a document whose case answers an ask holds.
const readAsk = ":::ask{#transport urgency=\"high\" multiple=\"false\"}\nWhich transport?\n\n" +
	"- REST: Matches the platform\n- gRPC: Adds streaming\n:::\n"

// document is a new issue's document as spec says, read from the store (no room of it in this
// process) or with its room open in this process (a browser connected to it).
func (f *oneVersionReadFixture) document(t *testing.T, spec readDocumentSpec) textReadDocument {
	t.Helper()
	text := "old line\n"
	if spec.ask {
		text += "\n" + readAsk
	}
	created := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "READ", "title": "One version read", "spec": text, "force": true,
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created)
	document := textReadDocument{issueKey: issue.Key, artifactID: issue.PrimaryArtifactID}
	if spec.ask {
		// The created spec's ask block becomes an ask once its document settles.
		f.documents.SettleNow(document.artifactID)
		awaitIndexedAskBlock(t, f.handler, document.artifactID, "transport", "Which transport?")
		document.askID = blockAskID(t, f.handler, document.issueKey, "transport")
	}
	if spec.noToken {
		if _, err := f.database.Pool.Exec(context.Background(), `
			update artifact_versions set head_version = null, doc_update_row = null, document_token = null, renderer_version = null
			where artifact_id = $1
		`, document.artifactID); err != nil {
			t.Fatalf("take the document's versions' tokens away: %v", err)
		}
	}
	if spec.inItsRoom {
		peer := connectBrowserPeer(t, f.documents, document.artifactID)
		t.Cleanup(peer.close)
		if !slices.Contains(f.documents.ResidentRooms(), document.artifactID) {
			t.Fatal("the browser is connected and this process holds no room of the document")
		}
	} else {
		if err := f.documents.Evict(context.Background(), document.artifactID); err != nil {
			t.Fatalf("close the document's room: %v", err)
		}
		if slices.Contains(f.documents.ResidentRooms(), document.artifactID) {
			t.Fatal("this process holds a room of the document, so the read would not be the stored one")
		}
	}
	if markdown := f.storedMarkdown(t, document.artifactID, f.newestVersion(t, document.artifactID)); !strings.HasPrefix(markdown, "old line\n") {
		t.Fatalf("the document's newest version holds %q, want the issue's spec", markdown)
	}
	return document
}

// edit is bob's edit of the document through the edit route: "old line" becomes "new line", and the
// edit writes a version of it in its own transaction.
func (f *oneVersionReadFixture) edit(artifactID string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits",
		strings.NewReader(`{"ops":[{"op":"replace","find":"old line","with":"new line"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "bob")
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

// editBetweenTheReads is an armed hook's write: bob's edit, settled to the version after the
// newest.
func (f *oneVersionReadFixture) editBetweenTheReads(t *testing.T, artifactID string) func() {
	next := f.newestVersion(t, artifactID) + 1
	return func() {
		edited := f.edit(artifactID)
		t.Logf("write between the reads: edit status=%d", edited.Code)
		if edited.Code != http.StatusOK {
			t.Fatalf("bob's edit between the reads: status=%d body=%s", edited.Code, edited.Body.String())
		}
		settledVersion(t, f.handler, f.documents, artifactID, next)
		t.Logf("write between the reads: version %d settled", next)
	}
}

// answerBetweenTheReads is an armed hook's write: bob answers the document's ask, which writes the
// answer into its block, and the document settles to the version after the newest, whose markdown
// holds the answered block.
func (f *oneVersionReadFixture) answerBetweenTheReads(t *testing.T, document textReadDocument) func() {
	next := f.newestVersion(t, document.artifactID) + 1
	return func() {
		answered := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/asks/"+document.askID+"/answer", map[string]any{
			"selected": []string{"REST"}, "expected_edited_at": nil,
		}, "bob")
		t.Logf("write between the reads: answer status=%d", answered.Code)
		if answered.Code != http.StatusOK {
			t.Fatalf("bob's answer between the reads: status=%d body=%s", answered.Code, answered.Body.String())
		}
		settledVersion(t, f.handler, f.documents, document.artifactID, next)
		t.Logf("write between the reads: version %d settled", next)
	}
}

func (f *oneVersionReadFixture) read(t *testing.T, read documentTextRead, artifactID string) documentTextAnswer {
	t.Helper()
	response := dispatchRequest(t, f.handler, http.MethodGet, fmt.Sprintf(read.path, artifactID), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the document's text: status=%d body=%s", response.Code, response.Body.String())
	}
	answer := decodeBody[documentTextAnswer](t, response)
	if read.blocks != (answer.Blocks != nil) {
		t.Fatalf("the read answered blocks %v, want blocks only where it asked for them (%t)", answer.Blocks, read.blocks)
	}
	if answer.Version == nil {
		t.Fatalf("the read named no version: %s", response.Body.String())
	}
	return answer
}

// readWithAWriteBetween reads the document with write armed in front of the read's version
// statement, and fails the case where the read made no such statement.
func (f *oneVersionReadFixture) readWithAWriteBetween(t *testing.T, read documentTextRead, artifactID string, write func()) documentTextAnswer {
	t.Helper()
	f.hook.arm(artifactID, write)
	answer := f.read(t, read, artifactID)
	fired := f.hook.firedOn()
	if fired == "" {
		f.hook.arm("", nil)
		t.Fatal("the read sent no statement the hook writes in front of, so no write came between its reads")
	}
	t.Logf("the hook wrote in front of: %s", strings.Join(strings.Fields(fired), " "))
	return answer
}

// storedMarkdown is the markdown version number of the document stored.
func (f *oneVersionReadFixture) storedMarkdown(t *testing.T, artifactID string, number int) string {
	t.Helper()
	var markdown string
	if err := f.database.Pool.QueryRow(context.Background(), `
		select markdown from artifact_versions where artifact_id = $1 and number = $2
	`, artifactID, number).Scan(&markdown); err != nil {
		t.Fatalf("read the stored markdown of version %d: %v", number, err)
	}
	return markdown
}

// newestVersion is the number of the document's newest version.
func (f *oneVersionReadFixture) newestVersion(t *testing.T, artifactID string) int {
	t.Helper()
	var number int
	if err := f.database.Pool.QueryRow(context.Background(), `
		select max(number) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&number); err != nil {
		t.Fatalf("read the document's newest version: %v", err)
	}
	return number
}

// newestVersionHolding is the newest version of the document whose stored markdown is markdown,
// nil where none is.
func (f *oneVersionReadFixture) newestVersionHolding(t *testing.T, artifactID, markdown string) *int {
	t.Helper()
	var number *int
	if err := f.database.Pool.QueryRow(context.Background(), `
		select max(number) from artifact_versions where artifact_id = $1 and markdown = $2
	`, artifactID, markdown).Scan(&number); err != nil {
		t.Fatalf("read the versions holding the answered markdown: %v", err)
	}
	return number
}

// currentToken is the document's token now, as GET /api/v1/artifacts/{id}/text answers it.
func (f *oneVersionReadFixture) currentToken(t *testing.T, artifactID string) string {
	t.Helper()
	return f.read(t, documentTextReads[1], artifactID).Token
}

// readWant is what a case's read must answer and let its reader do: unsettled, whether the text
// it answers holds edits no version holds; taken, whether the review naming its version and the
// edit holding its token must both be taken, which they are where no write followed the read.
type readWant struct {
	unsettled, taken bool
}

// requireTheVersionOfItsText holds what a reader can do with answer, a read of the document. A
// settled answer names the newest version whose stored markdown is the markdown it answered, and a
// review naming that version records an approval of that text, or is refused writing nothing
// because a newer version settled meanwhile (REVIEW_VERSION_MOVED). An unsettled answer names the
// newest version, and no version stores its markdown. Either way an edit holding its token applies
// while that token is still the document's and is refused once it is not.
func (f *oneVersionReadFixture) requireTheVersionOfItsText(t *testing.T, read documentTextRead, artifactID string, answer documentTextAnswer, want readWant) {
	t.Helper()
	number := *answer.Version
	stored := f.storedMarkdown(t, artifactID, number)
	holding := f.newestVersionHolding(t, artifactID, answer.Markdown)
	newest := f.newestVersion(t, artifactID)
	said := "absent"
	if answer.Unsettled != nil {
		said = strconv.FormatBool(*answer.Unsettled)
	}
	t.Logf("answer: version=%d unsettled=%s markdown=%q token=%s; stored markdown of version %d: %q; newest version %d",
		number, said, answer.Markdown, answer.Token, number, stored, newest)
	unsettled := answer.Unsettled != nil && *answer.Unsettled
	// A split and a missing unsettled fail the case without ending it, so the review below still
	// shows what a reader who took the answer at its word would have approved.
	if answer.Unsettled == nil {
		t.Error("the answer leaves unsettled out")
	} else if unsettled != want.unsettled {
		t.Fatalf("the answer says unsettled %t, want %t", unsettled, want.unsettled)
	}
	if unsettled {
		if holding != nil {
			t.Fatalf("the answer says no version holds its markdown %q, and version %d stores it", answer.Markdown, *holding)
		}
		if number != newest {
			t.Fatalf("the unsettled answer names version %d, want the newest, %d", number, newest)
		}
	} else {
		if answer.Markdown != stored {
			t.Errorf("SPLIT: the answer labels markdown %q as version %d, whose stored markdown is %q", answer.Markdown, number, stored)
		}
		switch {
		case holding == nil:
			t.Errorf("the answer names version %d, and no version stores its markdown", number)
		case *holding != number:
			t.Errorf("the answer names version %d, want the newest version holding its markdown, %d", number, *holding)
		}
		f.requireTheReviewRecordsTheTextRead(t, read, artifactID, answer, want)
	}

	edits := "/api/v1/artifacts/" + artifactID + "/edits"
	if read.v2 {
		edits = "/api/v2/artifacts/" + artifactID + "/edits"
	}
	current := f.currentToken(t, artifactID)
	firstLine, _, _ := strings.Cut(answer.Markdown, "\n")
	edited := dispatchRequest(t, f.handler, http.MethodPost, edits, map[string]any{
		"ops":          []map[string]string{{"op": "replace", "find": firstLine, "with": "edited line"}},
		"precondition": map[string]string{"document": answer.Token},
	}, "alice")
	t.Logf("edit with the answer's token, the document's token now %s: status=%d %s", current, edited.Code, strings.TrimSpace(edited.Body.String()))
	refusal := decodeBody[struct {
		Code string `json:"code"`
	}](t, edited)
	switch {
	case current == answer.Token && edited.Code != http.StatusOK:
		t.Fatalf("the edit holding the document's own token: status=%d, want 200", edited.Code)
	case current != answer.Token && (edited.Code != http.StatusConflict || refusal.Code != "PRECONDITION_FAILED"):
		t.Fatalf("the edit holding a token the document has moved past: status=%d code=%s, want 409 PRECONDITION_FAILED", edited.Code, refusal.Code)
	case want.taken && current != answer.Token:
		t.Fatalf("with no write after the read the document's token is %s, want the answer's %s", current, answer.Token)
	}
}

// requireTheReviewRecordsTheTextRead records a review naming answer's version and holds that it
// approves the text the read gave the reviewer, or is refused writing nothing.
func (f *oneVersionReadFixture) requireTheReviewRecordsTheTextRead(t *testing.T, read documentTextRead, artifactID string, answer documentTextAnswer, want readWant) {
	t.Helper()
	number := *answer.Version
	var review *httptest.ResponseRecorder
	if read.v2 {
		review = dispatchRequest(t, f.handler, http.MethodPost, "/api/v2/artifacts/"+artifactID+"/reviews",
			map[string]any{"state": "approved", "version": number}, "alice")
	} else {
		review = dispatchRequest(t, f.handler, http.MethodPost, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/reviews", artifactID, number),
			map[string]any{"state": "approved"}, "alice")
	}
	t.Logf("review naming the answer's version %d: status=%d %s", number, review.Code, strings.TrimSpace(review.Body.String()))
	refusal := decodeBody[struct {
		Code string `json:"code"`
	}](t, review)
	switch {
	case review.Code == http.StatusCreated:
		recorded := decodeBody[model.ArtifactReview](t, review).Version
		if text := f.storedMarkdown(t, artifactID, recorded); text != answer.Markdown {
			t.Fatalf("the approval records version %d, whose text is %q; the text this read gave the reviewer is %q", recorded, text, answer.Markdown)
		}
	case !want.taken && review.Code == http.StatusConflict && refusal.Code == "REVIEW_VERSION_MOVED":
		var reviews int
		if err := f.database.Pool.QueryRow(context.Background(), `select count(*) from artifact_reviews where artifact_id = $1`, artifactID).Scan(&reviews); err != nil {
			t.Fatalf("count the document's reviews: %v", err)
		}
		if reviews != 0 {
			t.Fatalf("the refused review left %d reviews of the document, want none", reviews)
		}
	default:
		t.Fatalf("the review naming version %d answered %d %s, want it recorded, or refused as moved after a write", number, review.Code, review.Body.String())
	}
}

// A read of a document's text names the version of the text it answers (R13): the newest version
// whose stored markdown is the markdown it answers, read after the text is rendered and by that
// text, so a version minted while the read runs, which stores other text, is not named over it.
// The write between the reads is made from the test store's query tracer (splitReadHook) just
// before the statement the read takes its version from: bob's edit, settled; or bob's answer to the
// document's ask, which writes the answered block and leaves the document's token as it was, so a
// match by the token would name the answer's version over the text read before it. The control
// makes none. Each read runs on a document read from the store and on one whose room is open in
// this process, and on a document whose versions hold no token, as a version written before
// versions stored one. Text no version holds - a browser's typing in the room - is answered with
// the newest version and unsettled true; a comment anchored after a version changes the
// document's token and not its markdown, and is answered with that version and unsettled false.
// A room read held between a write's commit and its publish reads a tree that lacks the write
// while the store already holds the write's version, and names the version of the text it read.
func TestADocumentTextReadNamesTheVersionOfItsText(t *testing.T) {
	f := newOneVersionReadFixture(t)
	for _, read := range documentTextReads {
		t.Run(read.name, func(t *testing.T) {
			for _, place := range []struct {
				name      string
				inItsRoom bool
			}{{"stored", false}, {"room", true}} {
				t.Run(place.name, func(t *testing.T) {
					for _, stamp := range []struct {
						name    string
						noToken bool
					}{{"stamped", false}, {"no_token", true}} {
						t.Run(stamp.name, func(t *testing.T) {
							spec := readDocumentSpec{inItsRoom: place.inItsRoom, noToken: stamp.noToken}
							t.Run("control_no_write_between_the_reads", func(t *testing.T) {
								document := f.document(t, spec)
								answer := f.read(t, read, document.artifactID)
								f.requireTheVersionOfItsText(t, read, document.artifactID, answer, readWant{taken: true})
							})
							t.Run("a_write_between_the_reads", func(t *testing.T) {
								document := f.document(t, spec)
								answer := f.readWithAWriteBetween(t, read, document.artifactID, f.editBetweenTheReads(t, document.artifactID))
								f.requireTheVersionOfItsText(t, read, document.artifactID, answer, readWant{})
							})
						})
					}
					t.Run("an_ask_answered_between_the_reads", func(t *testing.T) {
						document := f.document(t, readDocumentSpec{inItsRoom: place.inItsRoom, ask: true})
						answer := f.readWithAWriteBetween(t, read, document.artifactID, f.answerBetweenTheReads(t, document))
						f.requireTheVersionOfItsText(t, read, document.artifactID, answer, readWant{})
					})
					t.Run("a_comment_anchored_after_the_version", func(t *testing.T) {
						document := f.document(t, readDocumentSpec{inItsRoom: place.inItsRoom})
						version := f.newestVersion(t, document.artifactID)
						var versionToken string
						if err := f.database.Pool.QueryRow(context.Background(), `
							select document_token from artifact_versions where artifact_id = $1 and number = $2
						`, document.artifactID, version).Scan(&versionToken); err != nil {
							t.Fatalf("read the version's token: %v", err)
						}
						commented := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+document.issueKey+"/comments", map[string]any{
							"body": "Is this the line?", "anchor": map[string]any{"artifact": "spec", "quote": "old line"},
						}, "alice")
						if commented.Code != http.StatusCreated {
							t.Fatalf("anchor a comment: status=%d body=%s", commented.Code, commented.Body.String())
						}
						if newest := f.newestVersion(t, document.artifactID); newest != version {
							t.Fatalf("the comment wrote version %d, so nothing here is tested", newest)
						}
						answer := f.read(t, read, document.artifactID)
						if answer.Token == versionToken {
							t.Fatalf("the commented document's token is version %d's own, %s, so nothing here is tested", version, versionToken)
						}
						f.requireTheVersionOfItsText(t, read, document.artifactID, answer, readWant{taken: true})
					})
				})
			}
			t.Run("room/edits_no_version_holds", func(t *testing.T) {
				document := f.document(t, readDocumentSpec{})
				peer := connectBrowserPeer(t, f.documents, document.artifactID)
				t.Cleanup(peer.close)
				peer.appendParagraph(t, "typed line\n")
				peer.barrier(t)
				answer := f.read(t, read, document.artifactID)
				if !strings.Contains(answer.Markdown, "typed line") {
					t.Fatalf("the read answered %q, want the browser's typing the room holds", answer.Markdown)
				}
				f.requireTheVersionOfItsText(t, read, document.artifactID, answer, readWant{unsettled: true, taken: true})
			})
			t.Run("room/held_between_a_writes_commit_and_its_publish", func(t *testing.T) {
				document := f.document(t, readDocumentSpec{inItsRoom: true})
				artifactID := document.artifactID
				if err := f.documents.ArmLiveWritePause(artifactID, docs.PauseAfterCommit); err != nil {
					t.Fatalf("arm the pause after the commit: %v", err)
				}
				t.Cleanup(func() { _, _ = f.documents.ReleaseLiveWritePause(artifactID, docs.PauseAfterCommit) })
				edited := make(chan *httptest.ResponseRecorder, 1)
				go func() { edited <- f.edit(artifactID) }()
				parked, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := f.documents.AwaitLiveWriteParked(parked, artifactID, docs.PauseAfterCommit); err != nil {
					t.Fatalf("bob's edit never parked between its commit and its publish: %v", err)
				}
				answer := f.read(t, read, artifactID)
				if released, err := f.documents.ReleaseLiveWritePause(artifactID, docs.PauseAfterCommit); err != nil || !released {
					t.Fatalf("release bob's parked edit = %t, %v", released, err)
				}
				select {
				case response := <-edited:
					if response.Code != http.StatusOK {
						t.Fatalf("bob's edit: status=%d body=%s", response.Code, response.Body.String())
					}
				case <-time.After(10 * time.Second):
					t.Fatal("bob's edit did not finish once released")
				}
				f.requireTheVersionOfItsText(t, read, artifactID, answer, readWant{})
			})
		})
	}
}
