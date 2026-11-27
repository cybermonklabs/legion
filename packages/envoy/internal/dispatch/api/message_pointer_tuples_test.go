package api

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// messagePointerKinds are the relation kinds a message's write states out of its node: what it
// answers, where it sits, where it was sent and who wrote it. Every other relation out of a
// message - an attempt's delivery among them - is another writer's, and is not read here.
var messagePointerKinds = []string{
	graphstore.TupleAddressedTo, graphstore.TupleAuthoredBy, graphstore.TuplePartOf, graphstore.TupleRepliesTo,
}

// A message, a broadcast's copy and a session's reply each read back as their node and the
// tuples their row's pointers are, caused by the event that stored them and dated by their row,
// through the real handlers: a root message on an issue sits on that issue whoever it was sent to,
// a reply sits under and replies to the message it answers, a reply naming a block replies to that
// block's node too, a direct message and a broadcast's copy sit on the page of the session they
// were sent to while it claimed no role and on its role's page once it claimed one, every
// targeted message is addressed to its session, a copy carries its broadcast as a value, and each
// names who wrote it. Each row names its node in its own insert.
//
// graph_edges answers each reply's replies_to once: from the tuples for a row that names its node,
// rendering a reply to a block of a message as the message_block address form, and from the old
// arms over messages for a row an older build wrote, which names none. refs.BacklinkCounts counts
// the message both ways answer at, and at its blocks, exactly as the view lists them.
func TestAMessageWritesItsPointersAsTuples(t *testing.T) {
	t.Parallel()
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "PTRS", "Message pointers", "The spec.\n")
	alice := graphstore.KindPerson + ":alice"
	session := graphstore.KindSession + ":s1"
	page := graphstore.KindPage + ":" + graphstore.SessionPageID("s1")

	onIssue := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "One.\n\nTwo.", "target": "session:s1", "delivery": "btw",
	}, "alice")
	requireMessageWrite(t, database, onIssue.ID, "message.created", "a message on an issue sent to a session",
		"part_of>issue:"+issue.Key, "addressed_to>"+session, "authored_by>"+alice)

	// A person's reply that names no target continues its thread the way the root was sent.
	reply := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Noted.", "in_reply_to": onIssue.ID}, "alice")
	if reply.Target == nil || *reply.Target != "session:s1" {
		t.Fatalf("the reply's target = %v, want the root's session:s1", reply.Target)
	}
	requireMessageWrite(t, database, reply.ID, "message.answered", "a reply on an issue",
		"replies_to>message:"+onIssue.ID, "part_of>message:"+onIssue.ID, "addressed_to>"+session, "authored_by>"+alice)

	block := "message/" + onIssue.ID + "#b2"
	blockReply := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "On two.", "in_reply_to": onIssue.ID, "in_reply_to_block": "b2",
	}, "alice")
	requireMessageWrite(t, database, blockReply.ID, "message.answered", "a reply naming a block",
		"replies_to>message:"+onIssue.ID, "replies_to>"+graphstore.KindBlock+":"+block,
		"part_of>message:"+onIssue.ID, "addressed_to>"+session, "authored_by>"+alice)

	direct := postAgentMessage(t, handler, "s1", map[string]any{"body": "Where is the dashboard?", "delivery": "aside"}, "alice")
	if direct.Code != http.StatusCreated {
		t.Fatalf("a direct message: status=%d body=%s", direct.Code, direct.Body.String())
	}
	directMessage := decodeBody[model.Message](t, direct)
	requireMessageWrite(t, database, directMessage.ID, "message.created", "a direct message",
		"part_of>"+page, "addressed_to>"+session, "authored_by>"+alice)
	if node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindPage, graphstore.SessionPageID("s1")); err != nil || !held ||
		node.State != graphstore.StateLive {
		t.Fatalf("the page of s1 is %+v (held %t, err %v), want a live node the direct message hangs from", node, held, err)
	}

	answered := replyTo(t, handler, directMessage.ID, "On it.")
	if answered.Code != http.StatusCreated {
		t.Fatalf("the session's reply: status=%d body=%s", answered.Code, answered.Body.String())
	}
	sessionReply := decodeBody[model.Message](t, answered)
	requireMessageWrite(t, database, sessionReply.ID, "message.answered", "a session's reply",
		"replies_to>message:"+directMessage.ID, "part_of>message:"+directMessage.ID, "addressed_to>"+session,
		"authored_by>"+session)

	broadcast := dispatchRequest(t, handler, http.MethodPost, "/api/v1/broadcasts", map[string]any{
		"body": "To everyone.", "delivery": "btw", "session_ids": []string{"s1"},
	}, "alice")
	if broadcast.Code != http.StatusCreated {
		t.Fatalf("a broadcast: status=%d body=%s", broadcast.Code, broadcast.Body.String())
	}
	var copyID, broadcastID string
	if err := database.Pool.QueryRow(ctx,
		`select id::text, broadcast_id::text from messages where broadcast_id is not null`).Scan(&copyID, &broadcastID); err != nil {
		t.Fatalf("read the broadcast's copy: %v", err)
	}
	requireMessageWrite(t, database, copyID, "message.created", "a broadcast's copy",
		"part_of>"+page, "addressed_to>"+session, "authored_by>"+alice)
	if value, held, err := graphstore.CurrentValue(ctx, database.Pool, graphstore.KindMessage, copyID, graphstore.TupleBroadcastID); err != nil ||
		!held || value != broadcastID {
		t.Fatalf("the copy's broadcast_id = %q (held %t, err %v), want %s", value, held, err, broadcastID)
	}

	// A session that claimed a role carries no page of its own: the role's page is its page, as
	// the publisher records it (graphstore.ClaimedRoles), so a direct message to it hangs from that
	// page and never from one the publisher would not write.
	const role = "pointers-role"
	rolePage := graphstore.KindPage + ":" + graphstore.RolePageID(role)
	claimRoleAsThePublisher(t, database, role, "s1")
	toHolder := postAgentMessage(t, handler, "s1", map[string]any{"body": "Still on the role?", "delivery": "aside"}, "alice")
	if toHolder.Code != http.StatusCreated {
		t.Fatalf("a direct message to a session that holds a role: status=%d body=%s", toHolder.Code, toHolder.Body.String())
	}
	requireMessageWrite(t, database, decodeBody[model.Message](t, toHolder).ID, "message.created",
		"a direct message to a session that holds a role", "part_of>"+rolePage, "addressed_to>"+session, "authored_by>"+alice)

	// An older build's reply to the same message, naming one of its blocks: a row that names no
	// node, of which the store holds nothing.
	var older string
	if err := database.Pool.QueryRow(ctx, `
		insert into messages (issue_key, author, body, in_reply_to, replies_to_block)
		values ($1, '{"kind":"user","id":"alice"}', 'Written by the release before.', $2::uuid, $3)
		returning id::text
	`, issue.Key, onIssue.ID, "message/"+onIssue.ID+"#b1").Scan(&older); err != nil {
		t.Fatalf("write a reply as the release before writes one: %v", err)
	}
	if stated, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindMessage, older, nil); err != nil || len(stated) != 0 {
		t.Fatalf("the store holds %v out of the older build's reply (err %v), want nothing", stated, err)
	}

	for _, want := range []struct {
		what, id string
		edges    []string
	}{
		{"a message on an issue", onIssue.ID, nil},
		{"a reply", reply.ID, []string{"message:" + onIssue.ID}},
		{"a reply naming a block", blockReply.ID, []string{"message:" + onIssue.ID, "message_block:" + block}},
		{"a direct message", directMessage.ID, nil},
		{"a session's reply", sessionReply.ID, []string{"message:" + directMessage.ID}},
		{"a broadcast's copy", copyID, nil},
		{"an older build's reply", older, []string{"message:" + onIssue.ID, "message_block:message/" + onIssue.ID + "#b1"}},
	} {
		requireRepliesAnsweredOnce(t, database, want.what, want.id, want.edges)
	}
	var placements int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_edges where from_kind = 'message' and kind in ($1, $2, $3, $4)
	`, graphstore.TuplePartOf, graphstore.TupleAddressedTo, graphstore.TupleAuthoredBy, graphstore.TupleBroadcastID).Scan(&placements); err != nil {
		t.Fatalf("count the message edges graph_edges renders no arm of: %v", err)
	}
	if placements != 0 {
		t.Fatalf("graph_edges answers %d part_of, addressed_to, authored_by or broadcast_id edges out of a message, want none: the view renders a message's replies_to alone", placements)
	}

	// Three replies answer the root - two from the tuples, one from the old arm - and two answer
	// its blocks, b2 from the tuples and b1 from the old arm, which the count reads as the root's,
	// beside the part_of of each of its two blocks.
	for _, count := range []struct {
		kind, id string
		want     int
	}{
		{"message", onIssue.ID, 3 + 2 + 2},
		{"message_block", block, 1},
		{"message_block", "message/" + onIssue.ID + "#b1", 1},
	} {
		counted, err := refs.BacklinkCounts(ctx, database.Pool, model.Actor{}, count.kind, []string{count.id}, nil)
		if err != nil {
			t.Fatalf("count the %s backlinks of %s: %v", count.kind, count.id, err)
		}
		listed := viewBacklinks(t, database, count.kind, count.id)
		if counted[count.id] != count.want || listed != count.want {
			t.Errorf("the %s %s is counted %d and listed %d times by graph_edges, want %d", count.kind, count.id, counted[count.id], listed, count.want)
		}
	}
}

// requireMessageWrite fails unless message id reads back as this build writes a message: its
// node, its row naming that node, and, out of the node, exactly the relations want lists of the
// kinds a message's write states (messagePointerKinds), each written kind>to_kind:to_id, dated by
// the row's created_at and caused by the event of eventType that stored the message.
func requireMessageWrite(t *testing.T, database *store.Store, id, eventType, what string, want ...string) {
	t.Helper()
	ctx := context.Background()
	if _, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindMessage, id); err != nil || !held {
		t.Fatalf("%s: the store holds no node of message %s (err %v)", what, id, err)
	}
	var nodeKind, nodeAddress *string
	var createdAt time.Time
	if err := database.Pool.QueryRow(ctx, `
		select node_kind, node_address, created_at from messages where id = $1::uuid
	`, id).Scan(&nodeKind, &nodeAddress, &createdAt); err != nil {
		t.Fatalf("%s: read the row of message %s: %v", what, id, err)
	}
	if nodeKind == nil || nodeAddress == nil || *nodeKind != graphstore.KindMessage || *nodeAddress != id {
		t.Fatalf("%s: the row of message %s names the node %v/%v, want %s/%s", what, id, nodeKind, nodeAddress, graphstore.KindMessage, id)
	}
	event := eventNaming(t, database, eventType, id)
	stated, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindMessage, id, messagePointerKinds)
	if err != nil {
		t.Fatalf("%s: read the relations out of message %s: %v", what, id, err)
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
		t.Fatalf("%s: message %s states %v, want %v", what, id, got, want)
	}
}

// requireRepliesAnsweredOnce fails unless graph_edges answers exactly the replies_to edges want
// lists out of message id, each written to_kind:to_id, once each and dated by the row's own
// created_at, whichever arm answers it.
func requireRepliesAnsweredOnce(t *testing.T, database *store.Store, what, id string, want []string) {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select g.to_kind || ':' || g.to_id, g.created_at = m.created_at
		from graph_edges g join messages m on m.id::text = g.from_id
		where g.from_kind = 'message' and g.from_id = $1 and g.kind = $2
	`, id, graphstore.TupleRepliesTo)
	if err != nil {
		t.Fatalf("%s: read the replies_to edges of %s: %v", what, id, err)
	}
	defer rows.Close()
	got := []string{}
	for rows.Next() {
		var edge string
		var dated bool
		if err := rows.Scan(&edge, &dated); err != nil {
			t.Fatalf("%s: scan a replies_to edge of %s: %v", what, id, err)
		}
		if !dated {
			t.Errorf("%s: graph_edges dates %s's replies_to %s otherwise than its row", what, id, edge)
		}
		got = append(got, edge)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: read the replies_to edges of %s: %v", what, id, err)
	}
	slices.Sort(got)
	want = slices.Sorted(slices.Values(append([]string{}, want...)))
	if !slices.Equal(got, want) {
		t.Fatalf("%s: graph_edges answers %s's replies_to %v, want %v once each", what, id, got, want)
	}
}

// itemBlockReads are, for each item kind whose blocks a list of its backlinks shows as the
// item's own, the blocks table, its column naming the item and the address form graph_edges
// names such a block by.
var itemBlockReads = map[string]struct{ table, item, form string }{
	graphstore.KindMessage: {"message_blocks", "message_id", "message_block"},
	graphstore.KindComment: {"comment_blocks", "comment_id", "comment_block"},
	graphstore.KindAsk:     {"ask_blocks", "ask_id", "ask_block"},
}

// viewBacklinks is how many rows graph_edges lists at the node kind/id, and, for a message, a
// comment or an ask, at each of its current blocks, which a list of the item's backlinks shows as
// the item's own: the reading refs.BacklinkCounts is held to (refs' backlinkCountOracle).
func viewBacklinks(t *testing.T, database *store.Store, kind, id string) int {
	t.Helper()
	statement := `select count(*) from graph_edges where to_kind = $1 and to_id = $2`
	if blocks, ok := itemBlockReads[kind]; ok {
		statement = `select (` + statement + `) + (
			select count(*) from graph_edges g join ` + blocks.table + ` part on part.node_id = g.to_id and part.position is not null
			where g.to_kind = '` + blocks.form + `' and part.` + blocks.item + `::text = $2)`
	}
	var listed int
	if err := database.Pool.QueryRow(context.Background(), statement, kind, id).Scan(&listed); err != nil {
		t.Fatalf("count the graph_edges rows at %s %s: %v", kind, id, err)
	}
	return listed
}

// claimRoleAsThePublisher states what the publisher's record of a role's resolution states
// (outbox.deliveredGraph): the role and its page, the page part_of the role, the role held_by the
// session holding it, and that session part_of the role.
func claimRoleAsThePublisher(t *testing.T, database *store.Store, role, session string) {
	t.Helper()
	ctx := context.Background()
	who := model.Actor{Kind: "system", ID: "dispatch-outbox"}
	at := time.Now().UTC()
	page := graphstore.RolePageID(role)
	relation := func(fromKind, fromID, kind, toKind, toID string) graphstore.Tuple {
		return graphstore.Tuple{
			FromKind: fromKind, FromID: fromID, Kind: kind, ToKind: toKind, ToID: toID,
			At: at, Who: who, SourceEvent: graphstore.NoEvent, Asserted: true,
		}
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the claim of %s: %v", role, err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx,
		[]graphstore.Node{graphstore.PageNode(page, who, at, graphstore.NoEvent), graphstore.RoleNode(role, who, at, graphstore.NoEvent)},
		[]graphstore.Tuple{
			relation(graphstore.KindPage, page, graphstore.TuplePartOf, graphstore.KindRole, role),
			relation(graphstore.KindRole, role, graphstore.TupleHeldBy, graphstore.KindSession, session),
			relation(graphstore.KindSession, session, graphstore.TuplePartOf, graphstore.KindRole, role),
		}); err != nil {
		t.Fatalf("state %s held by %s: %v", role, session, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the claim of %s: %v", role, err)
	}
}
