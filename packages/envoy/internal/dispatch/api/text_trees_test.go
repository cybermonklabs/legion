package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

// requireStoredTree fails unless the table holds the display tree of text, in the mode a read
// serves, as the bytes pmdoc.DisplayJSON answers: what the write stored is what a read would have
// built, so a read after a restart serves it in place of reading the markdown again.
func requireStoredTree(t *testing.T, database *store.Store, what, text string) {
	t.Helper()
	if text == "" {
		t.Fatalf("%s: the test's text is empty, which has no tree and stores no row", what)
	}
	stored, err := texttrees.New(database.Pool).Read(
		context.Background(), pmdoc.DisplayLatest, [][32]byte{pmdoc.TextSum(text)},
	)
	if err != nil {
		t.Fatalf("%s: read the stored tree: %v", what, err)
	}
	tree, held := stored[pmdoc.TextSum(text)]
	if !held {
		t.Fatalf("%s: the table holds no display tree of %q", what, text)
	}
	if want := pmdoc.DisplayJSON(text, pmdoc.DisplayLatest); !bytes.Equal(tree, want) {
		t.Errorf("%s: the stored tree of %q is\n\t%s\nwant\n\t%s", what, text, tree, want)
	}
}

// Every write of a stored text stores that text's display tree, in the one request that wrote it:
// a comment, its edit, a message, an ask with its options, an ask's edit, an answer, a
// resolution's reason, a review's reason and a document version's markdown. Each is driven through
// the routes a client calls, so what the row is compared with is what the write stored.
func TestEveryWriteOfAStoredTextStoresItsDisplayTree(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	const (
		commentBody   = "Check **this** before the deploy."
		editedBody    = "Check **that** before the deploy, and the *rollback* too."
		replyBody     = "Checked: the deploy is `green`."
		messageBody   = "The *staging* run is green."
		askQuestion   = "Ship the ~~draft~~ **release** today?"
		optionLabel   = "Yes, `ship it`"
		optionDetail  = "Ships the release *today*."
		editedOption  = "Yes, ship it **now**"
		editedAsk     = "Ship the **release** today, or wait?"
		answerText    = "Shipping **now**."
		resolvedAsk   = "Should the *rollback* be rehearsed?"
		resolveReason = "Rehearsed in the **last** deploy."
		reviewReason  = "The *rollout* section needs the rollback step."
		spec          = "# Spec\n\nThe **plan** is written here.\n"
		namedSummary  = "the plan as it stands"
	)
	issue := createInteractionIssue(t, handler, "TREES", "Stored display trees", spec)

	t.Run("a comment create", func(t *testing.T) {
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
			map[string]any{"body": commentBody}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create the comment: status=%d body=%s", created.Code, created.Body.String())
		}
		comment := decodeBody[model.Comment](t, created)
		requireStoredTree(t, database, "a comment create", comment.Body)

		t.Run("its edit", func(t *testing.T) {
			edited := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID,
				map[string]any{"body": editedBody}, "alice")
			if edited.Code != http.StatusOK {
				t.Fatalf("edit the comment: status=%d body=%s", edited.Code, edited.Body.String())
			}
			requireStoredTree(t, database, "a comment edit", decodeBody[model.Comment](t, edited).Body)
		})

		t.Run("a reply to it", func(t *testing.T) {
			replied := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
				map[string]any{"body": replyBody, "reply_to": comment.ID}, "bob")
			if replied.Code != http.StatusCreated {
				t.Fatalf("reply to the comment: status=%d body=%s", replied.Code, replied.Body.String())
			}
			requireStoredTree(t, database, "a comment reply", decodeBody[model.Comment](t, replied).Body)
		})
	})

	t.Run("a message create", func(t *testing.T) {
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages",
			map[string]any{"body": messageBody}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create the message: status=%d body=%s", created.Code, created.Body.String())
		}
		requireStoredTree(t, database, "a message create", decodeBody[model.Message](t, created).Body)
	})

	t.Run("an ask create, its edit and its answer", func(t *testing.T) {
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": askQuestion,
			"options":  []map[string]string{{"label": optionLabel, "description": optionDetail}},
		}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create the ask: status=%d body=%s", created.Code, created.Body.String())
		}
		ask := decodeBody[model.Ask](t, created)
		requireStoredTree(t, database, "an ask's question", ask.Question)
		requireStoredTree(t, database, "an ask option's label", ask.Options[0].Label)
		requireStoredTree(t, database, "an ask option's description", ask.Options[0].Description)

		edited := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+ask.ID, map[string]any{
			"question": editedAsk,
			"options":  []map[string]string{{"label": editedOption}},
		}, "alice")
		if edited.Code != http.StatusOK {
			t.Fatalf("edit the ask: status=%d body=%s", edited.Code, edited.Body.String())
		}
		reworded := decodeBody[model.Ask](t, edited)
		requireStoredTree(t, database, "an edited ask's question", reworded.Question)
		requireStoredTree(t, database, "an edited ask option's label", reworded.Options[0].Label)
		// The question and options the edit replaced hold no row of their own, and every read of
		// the ask's history is shown them.
		requireStoredTree(t, database, "the question an ask edit replaced", askQuestion)
		requireStoredTree(t, database, "the option label an ask edit replaced", optionLabel)

		// The answer names the wording it was given, as a person's browser does: an ask edited
		// since the reader read it is refused (ASK_EDITED).
		answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
			"selected": []string{reworded.Options[0].Label}, "text": answerText,
			"expected_edited_at": reworded.EditedAt,
		}, "alice")
		if answered.Code != http.StatusOK {
			t.Fatalf("answer the ask: status=%d body=%s", answered.Code, answered.Body.String())
		}
		requireStoredTree(t, database, "an ask answer's text", answerText)
	})

	t.Run("an ask resolve", func(t *testing.T) {
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks",
			map[string]any{"question": resolvedAsk}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create the ask to resolve: status=%d body=%s", created.Code, created.Body.String())
		}
		ask := decodeBody[model.Ask](t, created)
		resolved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/resolve",
			map[string]any{"kind": "resolved", "reason": resolveReason}, "alice")
		if resolved.Code != http.StatusOK {
			t.Fatalf("resolve the ask: status=%d body=%s", resolved.Code, resolved.Body.String())
		}
		requireStoredTree(t, database, "a resolution's reason", resolveReason)
	})

	t.Run("a review with a reason", func(t *testing.T) {
		reviewed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews",
			map[string]any{"state": "changes_requested", "reason": reviewReason}, "alice")
		if reviewed.Code != http.StatusCreated {
			t.Fatalf("review the document: status=%d body=%s", reviewed.Code, reviewed.Body.String())
		}
		requireStoredTree(t, database, "a review's reason", reviewReason)
	})

	t.Run("a document version write", func(t *testing.T) {
		// The issue's own spec document, written when the issue was created, and the named
		// version this writes over it: both are version writes of the same markdown, and the row
		// is the markdown the document stores, not the text the request sent.
		markdown := documentMarkdown(t, handler, issue.PrimaryArtifactID)
		requireStoredTree(t, database, "the spec a new issue stored", markdown)
		named := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions",
			map[string]string{"summary": namedSummary}, "alice")
		if named.Code != http.StatusCreated {
			t.Fatalf("write a named version: status=%d body=%s", named.Code, named.Body.String())
		}
		requireStoredTree(t, database, "a named version's markdown", documentMarkdown(t, handler, issue.PrimaryArtifactID))
	})

	t.Run("a document upload", func(t *testing.T) {
		uploaded := uploadIssueDocument(t, handler, issue.Key, "Design notes.md", "# Design\n\nThe **notes** are here.\n")
		requireStoredTree(t, database, "an uploaded document's markdown", documentMarkdown(t, handler, uploaded.ID))
	})
}

// A broadcast is one text however many recipients it reaches: every copy holds the body the send
// was made with, and one row holds its tree.
func TestABroadcastStoresTheOneTreeOfItsBody(t *testing.T) {
	t.Parallel()
	const body = "Status, please: the **deploy** is held."
	listener, _ := newBroadcastListener(t, broadcastSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	sent := postBroadcast(t, handler, map[string]any{
		"body": body, "delivery": "steer", "session_ids": []string{"planner", "tester"},
	}, "alice")
	if sent.Code != http.StatusCreated {
		t.Fatalf("send the broadcast: status=%d body=%s", sent.Code, sent.Body.String())
	}
	created := decodeBody[broadcastResponse](t, sent)
	awaitBroadcastDeliveries(t, handler, created.ID)
	requireStoredTree(t, database, "a broadcast's body", body)
	sum := pmdoc.TextSum(body)
	var rows int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from text_trees where sum = $1`, sum[:]).Scan(&rows); err != nil {
		t.Fatalf("count the broadcast's rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("a broadcast to two sessions stored %d rows for one body, want 1", rows)
	}
}

// The publish path stores the tree of every text an event's payload carries, under the keys a
// reader reads them by (eventDisplayTrees): an edit's previous question and its option labels
// have no row of their own, and a reader of the ask's history is shown them.
//
// It calls the publish path's store rather than a route, because a route's own store writes the
// same rows: this is the reading of the payload, and the shape it answers for is the shape a read
// of that event answers.
func TestThePublishPathStoresTheTreesOfAnEventsPayloadTexts(t *testing.T) {
	t.Parallel()
	_, database, deps := newTestServer(t, testServerOptions{})
	const (
		question    = "Ship the **release** today, as the event carries it?"
		label       = "Yes, `ship it`"
		previous    = "Ship the ~~draft~~ today?"
		previousTwo = "Wait for the *rehearsal*"
		answer      = "Shipping, as the event says."
	)
	directServer(deps).storeEventDisplayTrees(context.Background(), []model.Event{{
		Type: "ask.edited",
		Payload: map[string]any{
			"question": question,
			"options":  []map[string]string{{"label": label}},
			"previous": map[string]any{
				"question": previous,
				"options":  []map[string]string{{"label": previousTwo}},
			},
			"previous_answer": map[string]string{"text": answer},
		},
	}})
	for what, text := range map[string]string{
		"the question an event carries":         question,
		"an option label an event carries":      label,
		"the question an edit replaced":         previous,
		"the option label an edit replaced":     previousTwo,
		"the answer a second answer superseded": answer,
	} {
		requireStoredTree(t, database, what, text)
	}
}

// A write whose tree cannot be stored still succeeds: the store runs after the commit, and a
// failure is logged and changes no answer. The table is dropped, which is every failure the store
// can meet - a refused statement - as a caller sees it.
func TestAWriteWhoseTreeCannotBeStoredStillSucceeds(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "NOTREE", "A store that fails", "# Spec\n\nWritten here.\n")
	if _, err := database.Pool.Exec(context.Background(), `drop table text_trees`); err != nil {
		t.Fatalf("drop the table the store writes: %v", err)
	}
	const body = "The deploy is **held** until the rollback is rehearsed."
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{"body": body}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create a comment with no table to store its tree: status=%d body=%s", created.Code, created.Body.String())
	}
	if stored := decodeBody[model.Comment](t, created).Body; stored != body {
		t.Errorf("the comment stored %q, want %q", stored, body)
	}
	// And the read answers the tree all the same, built from the markdown as it always was.
	requireDisplayTree(t, handler, "/api/v1/issues/"+issue.Key+"/comments", "0.body", "0.body_tree")
}

// A delete takes a post's text off every read of it, and the text's display tree with it. The
// stored row of that tree stays, keyed by the text alone (texttrees), as does this process's copy
// of it (pmdoc's display cache), so each read is made under the header before the delete, which
// leaves both trees in the table and in the cache and each list's answer kept. After the delete,
// no read of the comment or of the message - its thread, its issue's list of its kind, and the
// issue's event log, whose events of the post are read masked (attachPostMarks) - carries a tree
// of the old text, or any word of it.
func TestNoReadOfADeletedPostServesTheTreeOfItsOldText(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	const (
		commentBody = "The **withdrawn** claim about the *rollout*."
		messageBody = "The **withdrawn** note on the *staging* run."
		oldWord     = "withdrawn"
	)
	issue := createInteractionIssue(t, handler, "TREES", "A deleted post's trees", "# Spec\n\nWritten here.\n")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": commentBody}, "alice")
	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": messageBody}, "alice")
	reads := []struct{ name, target, treePath string }{
		{"the comment's thread", "/api/v1/comments/" + comment.ID, "comment.body_tree"},
		{"the issue's comment list", "/api/v1/issues/" + issue.Key + "/comments", "0.body_tree"},
		{"the message's thread", "/api/v1/messages/" + message.ID, "message.body_tree"},
		{"the issue's message list", "/api/v1/issues/" + issue.Key + "/messages", "0.message.body_tree"},
	}
	for _, read := range reads {
		if _, held := valueAt(t, treesRequest(t, handler, read.target, true), read.treePath); !held {
			t.Fatalf("%s carries no %s before the delete", read.name, read.treePath)
		}
	}
	eventLog := "/api/v1/issues/" + issue.Key + "/events"
	treesRequest(t, handler, eventLog, true)

	deleteThreadComment(t, handler, comment.ID, "alice")
	deleteIssueMessage(t, handler, message.ID, "alice")
	requireStoredTree(t, database, "the deleted comment's old text", commentBody)
	requireStoredTree(t, database, "the deleted message's old text", messageBody)

	for _, read := range reads {
		answer := treesRequest(t, handler, read.target, true)
		if tree, held := valueAt(t, answer, read.treePath); held {
			t.Errorf("%s after the delete carries %s = %s, want none", read.name, read.treePath, tree)
		}
		if strings.Contains(answer.Body.String(), oldWord) {
			t.Errorf("%s after the delete carries the old text: %s", read.name, answer.Body.String())
		}
	}
	answer := treesRequest(t, handler, eventLog, true)
	var events []struct {
		Type  string                     `json:"type"`
		Trees map[string]json.RawMessage `json:"trees"`
	}
	if err := json.Unmarshal(answer.Body.Bytes(), &events); err != nil {
		t.Fatalf("decode the event log: %v", err)
	}
	for _, event := range events {
		if tree, held := event.Trees["payload.body"]; held {
			t.Errorf("the event log's %s after the delete carries trees[payload.body] = %s, want none", event.Type, tree)
		}
	}
	if strings.Contains(answer.Body.String(), oldWord) {
		t.Errorf("the event log after the delete carries the old text: %s", answer.Body.String())
	}
}

// A server runs at most half its background pool's connections of the display-tree stores its
// reads leave behind them - three of six - and a read that finds that many running skips its
// store, counted, and is answered all the same (treeStores, storeOf). A read of the texts a store
// is already storing starts none and is no skip: one store a set of texts. The stores are held at
// the table, which the test locks against every insert and leaves open to reads, so every store
// that starts is still running when the next read comes, and the three are seen waiting on the
// lock at once. Without the bound each read started one more, each to wait its 30 s on the
// background pool's acquire ahead of the delivery sweep's resume workers.
func TestAtMostHalfTheBackgroundPoolOfTreeStoresRunAtOnce(t *testing.T) {
	t.Parallel()
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	skips := &TreeStoreCounts{}
	served.deps.TreeStores = skips
	background, err := database.Pool.Background()
	if err != nil {
		t.Fatalf("open the background pool: %v", err)
	}
	limit := int(background.Stat().MaxConns()) / 2
	// The lock's transaction and the reads run on marks of their own: a read under a mark that
	// holds a connection stores nothing (store.HoldsPooledConnection).
	locking := store.WithTransactionTracking(context.Background())
	ctx := store.WithTransactionTracking(context.Background())
	held, err := database.Pool.Begin(locking)
	if err != nil {
		t.Fatalf("open the transaction that holds the stores: %v", err)
	}
	rolledBack := false
	letGo := func() {
		if !rolledBack {
			rolledBack = true
			_ = held.Rollback(context.Background())
		}
	}
	testcleanup.Add(t, letGo)
	if _, err := held.Exec(locking, `lock table text_trees in exclusive mode`); err != nil {
		t.Fatalf("lock text_trees against inserts: %v", err)
	}

	texts := make([]string, limit+4)
	for index := range texts {
		texts[index] = fmt.Sprintf("Tree store %d of **%s** at %d.", index, t.Name(), time.Now().UnixNano())
	}
	read := func(text string) {
		t.Helper()
		reader := served.readDisplayTrees(ctx, true)
		var field model.DisplayTree
		reader.text(&field, text)
		reader.resolve()
		if len(field) == 0 {
			t.Fatalf("the read of %q was answered no tree", text)
		}
	}

	read(texts[0])
	// The same texts again, while the first store runs: the display cache now holds their tree,
	// so a fresh read would not reach the store at all, and the store is handed it directly.
	again := served.readDisplayTrees(ctx, true)
	var field model.DisplayTree
	again.text(&field, texts[0])
	tree, built := pmdoc.HeldDisplayTree(pmdoc.TextSum(texts[0]), pmdoc.DisplayLatest)
	if !built {
		t.Fatal("the first read left no tree of its text in the display cache")
	}
	again.texts[0].tree = tree
	again.storeOf([]int{0})
	if running, skipped := treeStoresRunning(&served.treeStores), skips.Skipped(); running != 1 || skipped != 0 {
		t.Fatalf("a second store of the texts a store is storing left %d stores running and %d skipped, want 1 and 0: it started none", running, skipped)
	}

	for _, text := range texts[1:] {
		read(text)
	}
	if running, skipped := treeStoresRunning(&served.treeStores), skips.Skipped(); running != limit || skipped != uint64(len(texts)-limit) {
		t.Fatalf("%d reads that each found their tree missing left %d stores running and %d skipped, want %d running, half the background pool's %d connections, and the other %d skipped",
			len(texts), running, skipped, limit, background.Stat().MaxConns(), len(texts)-limit)
	}
	testwait.Until(t, fmt.Sprintf("%d stores waiting on the lock at once", limit), func() (int, bool) {
		var waiting int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from pg_locks where relation = 'text_trees'::regclass and not granted
			  and database = (select oid from pg_database where datname = current_database())
		`).Scan(&waiting); err != nil {
			t.Fatalf("read what waits on text_trees: %v", err)
		}
		return waiting, waiting == limit
	})

	letGo()
	testwait.Until(t, "every store to end once the lock is let go", func() (int, bool) {
		running := treeStoresRunning(&served.treeStores)
		return running, running == 0
	})
	for _, text := range texts[:limit] {
		requireStoredTree(t, database, "a store that ran", text)
	}
}

// treeStoresRunning is how many stores stores counts running now.
func treeStoresRunning(stores *treeStores) int {
	stores.mu.Lock()
	defer stores.mu.Unlock()
	return stores.running
}
