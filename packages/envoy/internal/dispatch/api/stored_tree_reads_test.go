package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
)

// unshownTexts counts the texts unshownText has made, so each one differs from every text this
// process read before it.
var unshownTexts atomic.Int64

// unshownText is text with this process's next number on it, which makes it a text no reader of
// this process has been shown: the state a restart leaves every text in. The process's display
// cache is keyed by the text itself (pmdoc.TextSum and the mode) and keeps every tree it is
// handed for the process's life, so a test that read a fixed text would find, when it runs again
// in the same process (go test -count), the tree its first run left there: the read would build
// nothing, read no stored tree and store none, whatever the test counts.
func unshownText(text string) string {
	return text + " (" + strconv.FormatInt(unshownTexts.Add(1), 10) + ")"
}

// storedTreeOf is the table's display tree of text, in the mode and at the version a read serves,
// and whether the table holds one at all. It reads no markdown of its own, so a test can ask
// whether a row exists without putting that text's tree in the process's display cache - which
// requireStoredTree's comparison does, and which every test below has to do without until it has
// counted the builds its read cost.
func storedTreeOf(t *testing.T, database *store.Store, text string) (json.RawMessage, bool) {
	t.Helper()
	return storedTreeIn(t, database, pmdoc.DisplayLatest, text)
}

// storedTreeIn is storedTreeOf in one named mode: a past version's tree is stored in the version
// mode and every other in the latest one, and one text's two modes are two rows.
func storedTreeIn(t *testing.T, database *store.Store, mode pmdoc.DisplayMode, text string) (json.RawMessage, bool) {
	t.Helper()
	stored, err := texttrees.New(database.Pool).Read(
		context.Background(), mode, [][32]byte{pmdoc.TextSum(text)},
	)
	if err != nil {
		t.Fatalf("read the stored tree of %q: %v", text, err)
	}
	tree, held := stored[pmdoc.TextSum(text)]
	return tree, held
}

// requireAnsweredTree fails unless the answer carries, at treePath, the tree ReadForDisplay makes
// of text. It builds that tree itself, so a test that counts builds calls it only after it has
// counted them.
func requireAnsweredTree(t *testing.T, response *httptest.ResponseRecorder, treePath, text string) {
	t.Helper()
	got, held := valueAt(t, response, treePath)
	if !held {
		t.Fatalf("the answer carries no %s:\n\t%s", treePath, response.Body.String())
	}
	want := sameShape(t, pmdoc.DisplayJSON(text, pmdoc.DisplayLatest))
	if !bytes.Equal(got, want) {
		t.Errorf("%s is\n\t%s\nwant ReadForDisplay's tree of %q\n\t%s", treePath, got, text, want)
	}
}

// commentingIssue is an issue with one comment on it, written through the route a client calls so
// the comment's display tree is stored exactly as a production write stores it.
func commentingIssue(t *testing.T, handler http.Handler, project, body string) string {
	t.Helper()
	issue := createInteractionIssue(t, handler, project, "Stored display tree reads", "A spec")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{"body": body}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the comment: status=%d body=%s", created.Code, created.Body.String())
	}
	return issue.Key
}

// A comment list read after a restart makes no display tree: every tree it answers is the bytes
// the write stored, copied out of text_trees by the request's display tree reader and into the
// process's display cache, and pmdoc.DisplayTreeBuilds - which counts readings of markdown and
// nothing else - does not move.
//
// The restart is the cache's state for this text and not the process's age: the cache is keyed by
// the text itself (its SHA-256 and the mode), so a text no reader of this process has been shown
// stands in exactly the state a restart leaves every text in. The body below is written by this
// test alone and made unshown (unshownText), and the write that stored its tree read it through
// pmdoc.DisplayTreeToStore, which fills no cache.
func TestACommentListAfterARestartMakesNoDisplayTree(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	commentBody := unshownText("The **restart** read's own body: stored once, read back as bytes.")
	key := commentingIssue(t, handler, "RSTRT", commentBody)
	if _, held := storedTreeOf(t, database, commentBody); !held {
		t.Fatalf("the write stored no display tree of %q, so the read has nothing to copy", commentBody)
	}

	builds := pmdoc.DisplayTreeBuilds()
	response := treesRequest(t, handler, "/api/v1/issues/"+key+"/comments", true)
	if built := pmdoc.DisplayTreeBuilds() - builds; built != 0 {
		t.Errorf("the comment list built %d display trees with every tree stored, want none", built)
	}
	requireAnsweredTree(t, response, "0.body_tree", commentBody)
}

// A text the table holds no tree for is read for display as it always was, answered, and stored
// by the request that answered it; the next read of that text builds nothing. The rows are
// dropped after the write to stand for a text written before there was a table, which is what
// every text on the server is until the backfill reaches it.
func TestAReadOfAnUnstoredTextBuildsItsTreeAndStoresIt(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	commentBody := unshownText("A body *no* row holds a tree of yet, read the long way.")
	key := commentingIssue(t, handler, "UNSTR", commentBody)
	if _, err := database.Pool.Exec(context.Background(), `delete from text_trees`); err != nil {
		t.Fatalf("drop the stored trees: %v", err)
	}

	builds := pmdoc.DisplayTreeBuilds()
	response := treesRequest(t, handler, "/api/v1/issues/"+key+"/comments", true)
	if built := pmdoc.DisplayTreeBuilds() - builds; built != 1 {
		t.Errorf("the comment list built %d display trees for one unstored body, want 1", built)
	}
	// The read hands its store to the server's background work and answers without waiting for
	// it (displayTreeReader.storeOf), so the table is asked once that work is done.
	awaitBackgroundWrites(t, served)
	stored, held := storedTreeOf(t, database, commentBody)
	if !held {
		t.Fatalf("the read answered %q and stored no tree of it", commentBody)
	}

	builds = pmdoc.DisplayTreeBuilds()
	again := treesRequest(t, handler, "/api/v1/issues/"+key+"/comments", true)
	if built := pmdoc.DisplayTreeBuilds() - builds; built != 0 {
		t.Errorf("a second read of the same body built %d display trees, want none", built)
	}
	requireAnsweredTree(t, response, "0.body_tree", commentBody)
	requireAnsweredTree(t, again, "0.body_tree", commentBody)
	if want := pmdoc.DisplayJSON(commentBody, pmdoc.DisplayLatest); !bytes.Equal(stored, want) {
		t.Errorf("the tree the read stored is\n\t%s\nwant the one it answered\n\t%s", stored, want)
	}
}

// A cold read of a text the table holds no tree for stores the tree it built on the background
// pool (treeStorePool), which is what keeps a weight-1 pinned GET to the one request-pool
// connection it pins: the store runs beside the request on a goroutine of the server's background
// work, and on the request pool it would be a second connection while the pin is held. So the
// cold read takes exactly one request-pool connection, its pin - the caller's identity read
// (identity.HeaderIdentity, the first statement) and every read of the route run on it - the
// background pool hands out the store's, and the tree is stored. The wait for the store is the
// server's detached work alone (detached.wait): awaitBackgroundWrites reads the projects on the
// request pool itself, which would count as the request's.
func TestAColdPinnedReadTakesNoRequestPoolConnectionBeyondItsPin(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	const commentBody = "A body the *cold* read stores on the background pool."
	key := commentingIssue(t, handler, "COLDP", commentBody)
	if _, err := database.Pool.Exec(context.Background(), `delete from text_trees`); err != nil {
		t.Fatalf("drop the stored trees: %v", err)
	}
	awaitBackgroundWrites(t, served)
	target := "/api/v1/issues/" + key + "/comments"

	requests := settledAcquires(t, database, store.RequestsPool)
	background := settledAcquires(t, database, store.BackgroundPool)
	builds := pmdoc.DisplayTreeBuilds()
	cold := treesRequest(t, handler, target, true)
	served.detached.wait()
	coldRequests := poolAcquires(database, store.RequestsPool) - requests
	coldBackground := poolAcquires(database, store.BackgroundPool) - background

	if built := pmdoc.DisplayTreeBuilds() - builds; built != 1 {
		t.Fatalf("the cold read built %d display trees for one unstored body, want 1", built)
	}
	if coldRequests != 1 {
		t.Errorf("the cold read took %d request-pool connection(s), want 1, its pin: its store of the tree it built runs on the background pool, not beside the pin", coldRequests)
	}
	if coldBackground < 1 {
		t.Errorf("the cold read took %d background-pool connection(s), want its store's", coldBackground)
	}
	if _, held := storedTreeOf(t, database, commentBody); !held {
		t.Fatalf("the cold read answered %q and stored no tree of it", commentBody)
	}
	requireAnsweredTree(t, cold, "0.body_tree", commentBody)
}

// A tree stored at another display parser version is never served: the reading the stored bytes
// hold is not the reading this build makes, so the read builds its own and stores it beside the
// row it would not take (texttrees.Read keys by pmdoc.DisplayTreeVersionNow).
func TestAReadOfATreeStoredAtAnotherVersionBuildsItsOwn(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	commentBody := unshownText("A body whose stored tree another **parser** version read.")
	key := commentingIssue(t, handler, "OLDVR", commentBody)
	if _, held := storedTreeOf(t, database, commentBody); !held {
		t.Fatalf("the write stored no display tree of %q", commentBody)
	}

	restore := pmdoc.SetDisplayTreeVersionForTest(pmdoc.DisplayTreeVersion + 1)
	defer restore()
	builds := pmdoc.DisplayTreeBuilds()
	response := treesRequest(t, handler, "/api/v1/issues/"+key+"/comments", true)
	if built := pmdoc.DisplayTreeBuilds() - builds; built != 1 {
		t.Errorf("the comment list built %d display trees at a version the table does not hold, want 1", built)
	}
	awaitBackgroundWrites(t, served)
	if _, held := storedTreeOf(t, database, commentBody); !held {
		t.Errorf("the read at version %d stored no tree of its own", pmdoc.DisplayTreeVersionNow())
	}
	requireAnsweredTree(t, response, "0.body_tree", commentBody)
}

// countedTreesRead reads target with the display trees asked for, on a request a roundTrips
// counts (countedRead, issue_read_batch_test.go).
func countedTreesRead(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.WithValue(context.Background(), countedRead{}, true)
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	request.Header.Set("Dispatch-Trees", "1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	return response
}

// A comment list reads the stored trees of its whole page in one statement, however many comments
// it answers, and stores nothing when the table held every one of them: the cost of the trees on
// a cold list is one round trip, not one per comment and not one markdown parse per comment.
func TestACommentListReadsItsStoredTreesInOneStatement(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	counter := &roundTrips{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	handler, _, _ := newTestServer(t, testServerOptions{
		store: traced,
		// The caller's identity is resolved on the untraced pool, so the count is the read's own
		// statements and not the sign-in lookup every request makes.
		identity: headerIdentity(database),
		// A settlement of the document is database work of its own on the same pool; a delay no
		// test run reaches keeps it out of the count.
		settle: time.Hour,
	})
	bodies := []string{
		unshownText("The **first** of several bodies this list answers."),
		unshownText("The *second*, with a `code span` in it."),
		unshownText("The third, which [links](https://example.test) somewhere."),
		unshownText("The fourth, whose body is plain words."),
		unshownText("The fifth and last, ending the page."),
	}
	issue := createInteractionIssue(t, handler, "ONEQ", "One query for the whole page", "A spec")
	for _, body := range bodies {
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
			map[string]any{"body": body}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create the comment %q: status=%d body=%s", body, created.Code, created.Body.String())
		}
	}
	counter.take()

	response := countedTreesRead(t, handler, "/api/v1/issues/"+issue.Key+"/comments")
	counted := counter.take()
	var reads, writes int
	for _, statement := range counted {
		switch {
		case strings.Contains(statement, "from text_trees"):
			reads++
		case strings.Contains(statement, "insert into text_trees"):
			writes++
		}
	}
	if reads != 1 || writes != 0 {
		t.Fatalf("the list of %d comments made %d reads and %d writes of text_trees, want 1 and 0:\n%s",
			len(bodies), reads, writes, strings.Join(roundTripLines(counted), "\n"))
	}
	for index, body := range bodies {
		requireAnsweredTree(t, response, strconv.Itoa(index)+".body_tree", body)
	}
}

// A read inside a read-only transaction stores nothing and reads nothing: it holds a pooled
// connection already, and a second one taken while one is held is the deadlock
// store.ErrNestedAcquire refuses (docs loads a document and compacts it inside such a
// transaction). Every text is read where it stands, and the shaping answers its trees as it
// always did. Outside the transaction the same store writes, which is what makes the skip the
// held connection's doing and not the text's.
func TestAReadInsideAReadOnlyTransactionStoresNoTree(t *testing.T) {
	t.Parallel()
	_, database, deps := newTestServer(t, testServerOptions{})
	served := directServer(deps)
	const body = "A body shaped inside a **read-only** transaction."

	held := store.WithTransactionTracking(context.Background())
	tx, err := database.Pool.BeginTx(held, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("open the read-only transaction: %v", err)
	}
	reader := served.readDisplayTrees(held, true)
	read := shapedComment{Comment: model.Comment{Body: body}}
	read.shape(reader)
	reader.resolve()
	if read.BodyTree == nil {
		t.Errorf("the shaping inside the read-only transaction answered no tree of %q", body)
	}
	if _, stored := storedTreeOf(t, database, body); stored {
		t.Errorf("the read inside the read-only transaction stored a tree of %q", body)
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatalf("roll the read-only transaction back: %v", err)
	}

	served.storeDisplayTrees(context.Background(), "the same texts outside it", body)
	if _, stored := storedTreeOf(t, database, body); !stored {
		t.Errorf("the same store outside the transaction stored no tree of %q", body)
	}
}

// A broadcast read is one of the routes that draw a stored text, and it reads that text's tree
// the way every other route does: from the table where the table holds it, and from the
// markdown where it does not, storing what it read. The two arms are two sends, because the
// first read leaves its body's tree in this process's display cache, which stands in front of
// the table for every later read of that text.
func TestABroadcastReadTakesItsStoredTreeAndStoresAMiss(t *testing.T) {
	storedBody := unshownText("The **deploy** is held: send status, please.")
	unstoredBody := unshownText("The *rollback* is rehearsed: carry on, all of you.")
	listener, _ := newBroadcastListener(t, broadcastSessions)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
	send := func(body string) string {
		t.Helper()
		sent := postBroadcast(t, handler, map[string]any{
			"body": body, "delivery": "steer", "session_ids": []string{"planner", "tester"},
		}, "alice")
		if sent.Code != http.StatusCreated {
			t.Fatalf("send the broadcast %q: status=%d body=%s", body, sent.Code, sent.Body.String())
		}
		created := decodeBody[broadcastResponse](t, sent)
		awaitBroadcastDeliveries(t, handler, created.ID)
		return created.ID
	}
	stored, unstored := send(storedBody), send(unstoredBody)
	if _, held := storedTreeOf(t, database, storedBody); !held {
		t.Fatalf("the send stored no display tree of %q, so the read has nothing to copy", storedBody)
	}

	builds := pmdoc.DisplayTreeBuilds()
	response := treesRequest(t, handler, "/api/v1/broadcasts/"+stored, true)
	if built := pmdoc.DisplayTreeBuilds() - builds; built != 0 {
		t.Errorf("the broadcast read made %d display trees with its body's tree stored, want none", built)
	}
	requireAnsweredTree(t, response, "body_tree", storedBody)

	// The same read of a text the table holds no tree for reads that text once - the body and
	// every recipient's copy of it are one text - and stores what it read.
	if _, err := database.Pool.Exec(context.Background(), `delete from text_trees`); err != nil {
		t.Fatalf("drop the stored trees: %v", err)
	}
	builds = pmdoc.DisplayTreeBuilds()
	answered := treesRequest(t, handler, "/api/v1/broadcasts/"+unstored, true)
	if built := pmdoc.DisplayTreeBuilds() - builds; built != 1 {
		t.Errorf("the broadcast read made %d display trees for one unstored body, want 1", built)
	}
	awaitBackgroundWrites(t, served)
	if _, held := storedTreeOf(t, database, unstoredBody); !held {
		t.Fatalf("the read answered %q and stored no tree of it", unstoredBody)
	}
	requireAnsweredTree(t, answered, "body_tree", unstoredBody)
	requireAnsweredTree(t, answered, "recipients.0.message.body_tree", unstoredBody)
}

// A past version is read in the version mode, which keeps the attributes the server wrote, and
// no write stores that mode: the first read of a version is where its tree reaches the table,
// and the latest mode's row of the same text is a row of its own.
func TestAVersionsDisplayTreeIsStoredOnItsFirstRead(t *testing.T) {
	t.Parallel()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "VRTR", "A version's own tree", unshownText("# Head\n\nIts *own* text."))
	target := "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/versions/1"

	first := treesRequest(t, handler, target, true)
	version := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, first)
	if version.Markdown == "" {
		t.Fatalf("the version read answered no markdown:\n\t%s", first.Body.String())
	}
	awaitBackgroundWrites(t, served)
	stored, held := storedTreeIn(t, database, pmdoc.DisplayVersion, version.Markdown)
	if !held {
		t.Fatalf("the first read of the version stored no tree of %q", version.Markdown)
	}
	if want := sameShape(t, pmdoc.DisplayJSON(version.Markdown, pmdoc.DisplayVersion)); !bytes.Equal(sameShape(t, stored), want) {
		t.Errorf("the stored version tree is\n\t%s\nwant the version mode's reading\n\t%s", stored, want)
	}
	if _, latest := storedTreeIn(t, database, pmdoc.DisplayLatest, version.Markdown); !latest {
		t.Errorf("the version write stored no latest-mode tree of %q, which the document's own reads answer", version.Markdown)
	}
}

// A cut a read answers carries its tree and stores none: a backlink's excerpt - here the head of
// the ask an outgoing edge reaches, cut at 240 runes - and an Inbox row's subject - the thread
// root's first line, cut at 160 - are read where they stand (graphEdgeReads, listMyReplies), so
// the reads answer both trees under the header and write no text_trees row, where the request's
// reader would have asked the table for each and stored one row for every distinct cut.
func TestACutAReadAnswersCarriesItsTreeAndStoresNone(t *testing.T) {
	t.Parallel()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "CUTS", "A cut stores no tree", "A spec")
	question := strings.Repeat("Should the *cut* be stored? ", 12)
	asked := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": question, "options": []map[string]string{{"label": "Yes"}, {"label": "No"}},
	}, "alice")
	if asked.Code != http.StatusCreated {
		t.Fatalf("create the ask: status=%d body=%s", asked.Code, asked.Body.String())
	}
	askRef := "dispatch://" + issue.Key + "/ask/" + decodeBody[model.Ask](t, asked).ID
	cited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages",
		map[string]any{"body": "Decided in " + askRef}, "alice")
	if cited.Code != http.StatusCreated {
		t.Fatalf("create the message: status=%d body=%s", cited.Code, cited.Body.String())
	}
	messageRef := "dispatch://" + issue.Key + "/message/" + decodeBody[model.Message](t, cited).ID
	root := strings.Repeat("A **long** first line of the thread. ", 6)
	rooted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{"body": root}, "alice")
	if rooted.Code != http.StatusCreated {
		t.Fatalf("create the thread's root: status=%d body=%s", rooted.Code, rooted.Body.String())
	}
	sessionComment(t, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "A reply, so alice's Inbox has the thread.", "reply_to": decodeBody[model.Comment](t, rooted).ID,
	})
	awaitBackgroundWrites(t, served)
	rowsOf := func() int {
		t.Helper()
		var count int
		if err := database.Pool.QueryRow(context.Background(), `select count(*) from text_trees`).Scan(&count); err != nil {
			t.Fatalf("count the stored trees: %v", err)
		}
		return count
	}
	before := rowsOf()

	references := treesRequest(t, handler, "/api/v1/references?from="+url.QueryEscape(messageRef), true)
	replies := treesRequest(t, handler, "/api/v1/me/replies", true)
	awaitBackgroundWrites(t, served)
	if after := rowsOf(); after != before {
		t.Errorf("the two reads left %d text_trees rows, want the %d before them", after, before)
	}
	for _, read := range []struct {
		name, textPath, treePath, whole string
		response                        *httptest.ResponseRecorder
	}{
		{"a backlink's excerpt", "edges.0.excerpt.text", "edges.0.excerpt.text_tree", question, references},
		{"an Inbox row's subject", "rows.0.subject", "rows.0.subject_tree", strings.TrimSpace(root), replies},
	} {
		raw, held := valueAt(t, read.response, read.textPath)
		var text string
		if !held || json.Unmarshal(raw, &text) != nil || text == "" || text == read.whole {
			t.Fatalf("%s is %s, want a cut of a longer text:\n\t%s", read.name, raw, read.response.Body.String())
		}
		if _, stored := storedTreeOf(t, database, text); stored {
			t.Errorf("%s %q has a text_trees row; a cut is read where it stands", read.name, text)
		}
		requireAnsweredTree(t, read.response, read.treePath, text)
	}
}
