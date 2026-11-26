package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// commentPointerKinds are the relation kinds a comment's write states of what it answers: what it
// replies to, what it answers and where it sits. A comment's other relations - the artifacts it
// names, who wrote it, an attempt's delivery - are other facts of the write, and are not read here.
var commentPointerKinds = []string{graphstore.TupleAnswers, graphstore.TuplePartOf, graphstore.TupleRepliesTo}

// A comment reads back with exactly the tuples its row's pointers are, and the one part_of to what
// it answers, from the one write, caused by the event that stored it and dated by its row, through
// the real handlers: a root comment states none; a reply to a comment replies to its thread's root
// and answers the comment it names, under which it sits; a reply naming a block of a reply replies
// to that block's node too; a comment on an ask replies to the ask and sits under it, and one
// naming the ask's question replies to that part's node; a reply under an ask's thread replies to
// the ask and answers the comment it names; and a session's callback reply to a mention states its
// pointers as the comment route does. Each row holds tuples_stored from its own insert.
//
// graph_edges answers each relation once: from the tuples for a row that holds tuples_stored,
// rendering a reply to a block as the comment_block or the ask_block address form, and from the old
// arms over comments for a row an older build wrote, which holds none. A retraction of one tuple
// takes its edge out of the view while the row's column still names it, which is the store's arm
// answering it. refs.BacklinkCounts counts each node both ways answer at exactly as the view lists
// them.
func TestACommentWritesItsPointersAsTuples(t *testing.T) {
	t.Parallel()
	const sessions = `[{"session_id":"s1","title":"planner","capabilities":["btw","steer"]}]`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			writeSessionRecord(t, w, sessions, "s1")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(listener.Close)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "CPTR", "Comment pointers", "The spec.\n")
	comment := func(id string) string { return graphstore.KindComment + ":" + id }
	block := func(address string) string { return graphstore.KindBlock + ":" + address }

	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "The root."}, "alice")
	requireCommentWrite(t, database, root.ID, "comment.created", "a root comment")

	reply := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Alpha.\n\nBeta.", "reply_to": root.ID}, "bob")
	requireCommentWrite(t, database, reply.ID, "comment.created", "a reply to a comment",
		"replies_to>"+comment(root.ID), "answers>"+comment(root.ID), "part_of>"+comment(root.ID))

	beta := "comment/" + reply.ID + "#b2"
	leaf := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "On beta.", "reply_to": reply.ID, "reply_to_block": "b2",
	}, "alice")
	requireCommentWrite(t, database, leaf.ID, "comment.created", "a comment that answers a reply and names its block",
		"replies_to>"+comment(root.ID), "answers>"+comment(reply.ID), "replies_to>"+block(beta), "part_of>"+comment(reply.ID))

	ask := createPartAsk(t, handler, issue.Key, "Which day do we ship?", map[string]string{"label": "Monday"})
	onAsk := graphstore.KindAsk + ":" + ask.ID
	asked := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Monday works.", "ask_id": ask.ID}, "alice")
	requireCommentWrite(t, database, asked.ID, "comment.created", "a comment on an ask",
		"replies_to>"+onAsk, "part_of>"+onAsk)

	question := "ask/" + ask.ID + "#b1"
	onQuestion := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "On the question itself.", "ask_id": ask.ID, "reply_to_block": "b1",
	}, "bob")
	requireCommentWrite(t, database, onQuestion.ID, "comment.created", "a comment naming an ask's question",
		"replies_to>"+onAsk, "replies_to>"+block(question), "part_of>"+onAsk)

	underAsk := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Monday then.", "reply_to": asked.ID}, "bob")
	requireCommentWrite(t, database, underAsk.ID, "comment.created", "a reply in an ask's thread",
		"replies_to>"+onAsk, "answers>"+comment(asked.ID), "part_of>"+comment(asked.ID))

	mentioned := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can this ship, @s1?", "reply_to": root.ID,
		"mentions": []map[string]any{{"target": "session:s1"}},
	}))
	requireCommentWrite(t, database, mentioned.ID, "comment.created", "a reply that mentions a session",
		"replies_to>"+comment(root.ID), "answers>"+comment(root.ID), "part_of>"+comment(root.ID))
	answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+mentioned.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Yes, it can ship.",
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("answer the mention: status=%d body=%s", answered.Code, answered.Body.String())
	}
	callback := decodeBody[mentionedCommentRead](t, answered)
	requireCommentWrite(t, database, callback.ID, "comment.answered", "a session's callback reply",
		"replies_to>"+comment(root.ID), "answers>"+comment(mentioned.ID), "part_of>"+comment(mentioned.ID))

	// Two replies as the release before writes them: rows that hold no tuples_stored and name no
	// node, of which the store holds nothing.
	alpha := "comment/" + reply.ID + "#b1"
	olderReply := insertOlderComment(t, database, `
		insert into comments (issue_key, author, body, reply_to, answers, replies_to_block)
		values ($1, '{"kind":"user","id":"alice"}', 'On alpha, by the release before.', $2::uuid, $3::uuid, $4)
		returning id::text
	`, issue.Key, root.ID, reply.ID, alpha)
	olderOnQuestion := insertOlderComment(t, database, `
		insert into comments (issue_key, author, body, ask_id, replies_to_block)
		values ($1, '{"kind":"user","id":"alice"}', 'On the question, by the release before.', $2::uuid, $3)
		returning id::text
	`, issue.Key, ask.ID, question)

	for _, want := range []struct {
		what, id string
		edges    []string
	}{
		{"a root comment", root.ID, nil},
		{"a reply to a comment", reply.ID, []string{"replies_to>comment:" + root.ID, "answers>comment:" + root.ID}},
		{"a comment that answers a reply and names its block", leaf.ID,
			[]string{"replies_to>comment:" + root.ID, "answers>comment:" + reply.ID, "replies_to>comment_block:" + beta}},
		{"a comment on an ask", asked.ID, []string{"replies_to>ask:" + ask.ID}},
		{"a comment naming an ask's question", onQuestion.ID, []string{"replies_to>ask:" + ask.ID, "replies_to>ask_block:" + question}},
		{"a reply in an ask's thread", underAsk.ID, []string{"replies_to>ask:" + ask.ID, "answers>comment:" + asked.ID}},
		{"a reply that mentions a session", mentioned.ID, []string{"replies_to>comment:" + root.ID, "answers>comment:" + root.ID}},
		{"a session's callback reply", callback.ID, []string{"replies_to>comment:" + root.ID, "answers>comment:" + mentioned.ID}},
		{"an older build's reply", olderReply,
			[]string{"replies_to>comment:" + root.ID, "answers>comment:" + reply.ID, "replies_to>comment_block:" + alpha}},
		{"an older build's comment on the question", olderOnQuestion,
			[]string{"replies_to>ask:" + ask.ID, "replies_to>ask_block:" + question}},
	} {
		requireCommentPointersAnsweredOnce(t, database, want.what, want.id, want.edges)
	}
	var placements int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_edges where from_kind = 'comment' and kind = $1
	`, graphstore.TuplePartOf).Scan(&placements); err != nil {
		t.Fatalf("count the part_of edges out of a comment: %v", err)
	}
	if placements != 0 {
		t.Fatalf("graph_edges answers %d part_of edges out of a comment, want none: no arm answered a comment's placement before", placements)
	}

	// Each node the pointers reach is counted as the view lists it, the store's answers and the
	// old arms' alike; least is how many of the pointers above point at it.
	for _, count := range []struct {
		kind, id string
		least    int
	}{
		{graphstore.KindComment, root.ID, 5 + 2},
		{graphstore.KindComment, reply.ID, 2 + 2},
		{graphstore.KindComment, asked.ID, 1},
		{graphstore.KindComment, mentioned.ID, 1},
		{graphstore.KindAsk, ask.ID, 4 + 2},
		{"comment_block", beta, 1},
		{"comment_block", alpha, 1},
		{"ask_block", question, 2},
	} {
		counted, err := refs.BacklinkCounts(ctx, database.Pool, model.Actor{}, count.kind, []string{count.id}, nil)
		if err != nil {
			t.Fatalf("count the %s backlinks of %s: %v", count.kind, count.id, err)
		}
		listed := viewBacklinks(t, database, count.kind, count.id)
		if counted[count.id] != listed || listed < count.least {
			t.Errorf("the %s %s is counted %d and listed %d times by graph_edges, want the two equal and at least %d",
				count.kind, count.id, counted[count.id], listed, count.least)
		}
	}

	// The leaf's answers, retracted in the store, leaves the view and the count while its row's
	// answers column still names the reply: the store's arm is what answered it.
	retractCommentPointer(t, database, leaf.ID, graphstore.TupleAnswers, graphstore.KindComment, reply.ID)
	requireCommentPointersAnsweredOnce(t, database, "the leaf once its answers is retracted", leaf.ID,
		[]string{"replies_to>comment:" + root.ID, "replies_to>comment_block:" + beta})
	counted, err := refs.BacklinkCounts(ctx, database.Pool, model.Actor{}, graphstore.KindComment, []string{reply.ID}, nil)
	if err != nil {
		t.Fatalf("count the backlinks of the reply after the retraction: %v", err)
	}
	if listed := viewBacklinks(t, database, graphstore.KindComment, reply.ID); counted[reply.ID] != listed {
		t.Errorf("after the retraction the reply is counted %d and listed %d times, want them equal", counted[reply.ID], listed)
	}
}

// A comment on an ask names that ask by its one id however the request spells it: the comment
// route reads ask_id with uuid.Parse, which takes capitals, braces, no hyphens and the urn form as
// well as the canonical spelling, and the comment's replies_to and part_of then point at the ask's
// own node, and the comment answers that one spelling. A comment that mentions a session reads the
// ask's routes before its transaction (readCommentRoutes), by that same one spelling.
func TestACommentNamesItsAskByOneSpellingOfItsID(t *testing.T) {
	t.Parallel()
	listener, _ := newMentionListener(t)
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL, envoyTimeout: time.Second})
	issue := createInteractionIssue(t, handler, "CASK", "Ask id spellings", "The spec.\n")
	ask := createPartAsk(t, handler, issue.Key, "Which day do we ship?", map[string]string{"label": "Monday"})
	onAsk := graphstore.KindAsk + ":" + ask.ID
	for _, spelling := range []struct {
		name, askID string
		mentions    bool
	}{
		{"in capitals", strings.ToUpper(ask.ID), false},
		{"in braces", "{" + ask.ID + "}", false},
		{"without hyphens", strings.ReplaceAll(ask.ID, "-", ""), false},
		{"in the urn form", "urn:uuid:" + ask.ID, false},
		{"in the urn form, with a mention", "urn:uuid:" + ask.ID, true},
	} {
		t.Run(spelling.name, func(t *testing.T) {
			body := map[string]any{"body": "Monday, " + spelling.name + ".", "ask_id": spelling.askID}
			if spelling.mentions {
				body["mentions"] = []map[string]any{{"target": "session:s1"}}
			}
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", body, "alice")
			if response.Code != http.StatusCreated {
				t.Fatalf("comment on the ask with ask_id %q: status=%d body=%s", spelling.askID, response.Code, response.Body.String())
			}
			comment := decodeBody[model.Comment](t, response)
			if got := deref(comment.AskID); got != ask.ID {
				t.Errorf("the comment with ask_id %q answers ask_id %q, want the ask's own id %s", spelling.askID, got, ask.ID)
			}
			requireCommentWrite(t, database, comment.ID, "comment.created", "a comment naming its ask "+spelling.name,
				"replies_to>"+onAsk, "part_of>"+onAsk)
		})
	}
}

// requireCommentWrite fails unless comment id reads back as this build writes a comment: its node,
// its row naming that node and holding tuples_stored, and, out of the node, exactly the relations
// want lists of the kinds a comment's write states of what it answers (commentPointerKinds), each
// written kind>to_kind:to_id, dated by the row's created_at and caused by the event of eventType
// that stored the comment.
func requireCommentWrite(t *testing.T, database *store.Store, id, eventType, what string, want ...string) {
	t.Helper()
	ctx := context.Background()
	if _, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindComment, id); err != nil || !held {
		t.Fatalf("%s: the store holds no node of comment %s (err %v)", what, id, err)
	}
	var nodeKind, nodeAddress *string
	var stored bool
	var createdAt time.Time
	if err := database.Pool.QueryRow(ctx, `
		select node_kind, node_address, tuples_stored, created_at from comments where id = $1::uuid
	`, id).Scan(&nodeKind, &nodeAddress, &stored, &createdAt); err != nil {
		t.Fatalf("%s: read the row of comment %s: %v", what, id, err)
	}
	if nodeKind == nil || nodeAddress == nil || *nodeKind != graphstore.KindComment || *nodeAddress != id || !stored {
		t.Fatalf("%s: the row of comment %s names the node %v/%v with tuples_stored %t, want %s/%s and true",
			what, id, nodeKind, nodeAddress, stored, graphstore.KindComment, id)
	}
	event := eventNaming(t, database, eventType, id)
	stated, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindComment, id, commentPointerKinds)
	if err != nil {
		t.Fatalf("%s: read the relations out of comment %s: %v", what, id, err)
	}
	got := make([]string, 0, len(stated))
	for _, tuple := range stated {
		got = append(got, tuple.Kind+">"+tuple.ToKind+":"+tuple.ToID)
		if tuple.SourceEvent != event || !tuple.At.Equal(createdAt) {
			t.Errorf("%s: %s %s>%s:%s is caused by event %d at %s, want the %s event %d at the row's %s",
				what, id, tuple.Kind, tuple.ToKind, tuple.ToID, tuple.SourceEvent, tuple.At, eventType, event, createdAt)
		}
	}
	slices.Sort(got)
	want = slices.Sorted(slices.Values(want))
	if !slices.Equal(got, want) {
		t.Fatalf("%s: comment %s states %v, want %v", what, id, got, want)
	}
}

// insertOlderComment writes a comment as a build of the release before writes one, by statement
// with args bound, and fails unless the row holds no tuples_stored and the store holds nothing out
// of it. It answers the comment's id.
func insertOlderComment(t *testing.T, database *store.Store, statement string, args ...any) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := database.Pool.QueryRow(ctx, statement, args...).Scan(&id); err != nil {
		t.Fatalf("write a comment as the release before writes one: %v", err)
	}
	var stored bool
	if err := database.Pool.QueryRow(ctx, `select tuples_stored from comments where id = $1::uuid`, id).Scan(&stored); err != nil || stored {
		t.Fatalf("the older build's comment %s holds tuples_stored %t (err %v), want false", id, stored, err)
	}
	if held, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindComment, id, nil); err != nil || len(held) != 0 {
		t.Fatalf("the store holds %v out of the older build's comment %s (err %v), want nothing", held, id, err)
	}
	return id
}

// requireCommentPointersAnsweredOnce fails unless graph_edges answers exactly the replies_to and
// answers edges want lists out of comment id, each written kind>to_kind:to_id, once each and dated
// by the row's own created_at, whichever arm answers it.
func requireCommentPointersAnsweredOnce(t *testing.T, database *store.Store, what, id string, want []string) {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select g.kind || '>' || g.to_kind || ':' || g.to_id, g.created_at = c.created_at
		from graph_edges g join comments c on c.id::text = g.from_id
		where g.from_kind = 'comment' and g.from_id = $1 and g.kind in ($2, $3)
	`, id, graphstore.TupleRepliesTo, graphstore.TupleAnswers)
	if err != nil {
		t.Fatalf("%s: read the pointer edges of %s: %v", what, id, err)
	}
	defer rows.Close()
	got := []string{}
	for rows.Next() {
		var edge string
		var dated bool
		if err := rows.Scan(&edge, &dated); err != nil {
			t.Fatalf("%s: scan a pointer edge of %s: %v", what, id, err)
		}
		if !dated {
			t.Errorf("%s: graph_edges dates %s's %s otherwise than its row", what, id, edge)
		}
		got = append(got, edge)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: read the pointer edges of %s: %v", what, id, err)
	}
	slices.Sort(got)
	want = slices.Sorted(slices.Values(append([]string{}, want...)))
	if !slices.Equal(got, want) {
		t.Fatalf("%s: graph_edges answers %s's pointers %v, want %v once each", what, id, got, want)
	}
}

// retractCommentPointer appends the retraction of comment id's relation kind to toKind:toID, as a
// later write that ends it would, at the sentinel, which no event of this test records.
func retractCommentPointer(t *testing.T, database *store.Store, id, kind, toKind, toID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the retraction of %s's %s: %v", id, kind, err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{{
		FromKind: graphstore.KindComment, FromID: id, Kind: kind, ToKind: toKind, ToID: toID,
		At: time.Now().UTC(), Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent, Asserted: false,
	}}); err != nil {
		t.Fatalf("retract %s's %s: %v", id, kind, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the retraction of %s's %s: %v", id, kind, err)
	}
}

// R4: a person a comment's edit newly mentions takes part in the comment's thread from it
// (editMentionParticipation), and a reply's thread is the head it replies to (inboxwrite's
// threadOf). A reply a build of the release before wrote states none of its pointers as tuples
// (comments.tuples_stored false) until the backfill takes it, so its row's reply_to is the one
// record of that head.
//
// alice writes a comment through this build, and bob a reply to it as the release before writes
// one: the reply's node, which that release writes beside the row, and the row naming that node
// and the head it answers, with no tuple of what it answers. bob then edits his reply through this
// build to name dave: dave takes part in alice's comment, the thread's root, under the edit's
// event, and in no thread whose root is the reply. Its control is threadOf reading the reply's
// tuples alone (before INFRA-1152's review C16): dave takes part in the reply itself, a thread no
// later reply to alice's comment reaches.
func TestAnEditOfAnOlderBuildsReplyMakesWhomItNamesTakePartInItsHeadsThread(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "OLDR", "An older reply edited", "The spec.\n")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Is the copy final?"}, "alice")
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the older build's reply: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select set_config($1, 'off', true)`, store.StoreMarkerWriterSetting); err != nil {
		t.Fatalf("write as the release before: %v", err)
	}
	var reply string
	var at time.Time
	if err := tx.QueryRow(ctx, `select gen_random_uuid()::text, now()`).Scan(&reply, &at); err != nil {
		t.Fatalf("draw the older build's reply's id: %v", err)
	}
	node, tuples := graphstore.CommentFacts(nil, reply, model.Actor{Kind: "user", ID: "bob"}, at, graphstore.NoEvent, "", "", "OLDR", nil)
	if err := graphstore.Write(ctx, tx, []graphstore.Node{node}, tuples); err != nil {
		t.Fatalf("write the older build's reply's node: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into comments (id, issue_key, author, body, reply_to, node_kind, node_address, created_at)
		values ($1::uuid, $2, '{"kind":"user","id":"bob"}', 'Nearly.', $3::uuid, $4, $1, $5)
	`, reply, issue.Key, root.ID, graphstore.KindComment, at); err != nil {
		t.Fatalf("write the older build's reply: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the older build's reply: %v", err)
	}
	var stored bool
	if err := database.Pool.QueryRow(ctx, `select tuples_stored from comments where id = $1::uuid`, reply).Scan(&stored); err != nil || stored {
		t.Fatalf("the older build's reply %s holds tuples_stored %t (err %v), want false: this test proves nothing", reply, stored, err)
	}
	if held, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindComment, reply, commentPointerKinds); err != nil || len(held) != 0 {
		t.Fatalf("the store holds %v of what the older build's reply %s answers (err %v), want nothing: this test proves nothing", held, reply, err)
	}

	const dave = "dave@example.com"
	if edited := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+reply,
		map[string]any{"body": "Nearly: over to [Dave](dispatch://person/" + dave + ") to check."}, "bob"); edited.Code != http.StatusOK {
		t.Fatalf("bob edits his reply to name dave: status=%d body=%s", edited.Code, edited.Body.String())
	}
	event := writeEventOf(t, database, "comment.edited", reply)
	taking, err := readStrings(ctx, database.Pool, `
		select to_kind || ':' || to_id || ' under ' || source_event::text from graph_tuples
		where kind = $1 and asserted and from_kind = $2 and from_id = $3
		order by seq
	`, graphstore.TupleParticipatesIn, graphstore.KindPerson, dave)
	if err != nil {
		t.Fatalf("read dave's participation: %v", err)
	}
	if want := []string{graphstore.KindComment + ":" + root.ID + " under " + fmt.Sprint(event)}; !slices.Equal(taking, want) {
		t.Errorf("after bob's edit of the older build's reply %s dave takes part in %v, want %v: the thread of alice's comment, which the reply answers",
			reply, taking, want)
	}
}
