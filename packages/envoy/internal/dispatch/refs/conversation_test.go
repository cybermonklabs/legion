package refs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// conversationItem inserts an item of issueKey into table (asks, comments or messages) as author
// wrote it at at, replying to replyTo where it is not "" (a comment's reply_to, a message's
// in_reply_to), indexes its blocks as a write of this build does, and returns its id and the node
// id of its first block.
func conversationItem(t *testing.T, tx pgx.Tx, table, issueKey, author, body string, at time.Time, replyTo string) (string, string) {
	t.Helper()
	ctx := context.Background()
	var id string
	var err error
	switch table {
	case "asks":
		err = tx.QueryRow(ctx, `insert into asks (issue_key, author, question, created_at) values ($1, $2, $3, $4) returning id::text`,
			issueKey, author, body, at).Scan(&id)
	case "comments":
		err = tx.QueryRow(ctx, `insert into comments (issue_key, author, body, created_at, reply_to) values ($1, $2, $3, $4, nullif($5, '')::uuid) returning id::text`,
			issueKey, author, body, at, replyTo).Scan(&id)
	case "messages":
		err = tx.QueryRow(ctx, `insert into messages (issue_key, author, body, created_at, in_reply_to) values ($1, $2, $3, $4, nullif($5, '')::uuid) returning id::text`,
			issueKey, author, body, at, replyTo).Scan(&id)
	}
	if err != nil {
		t.Fatalf("insert into %s: %v", table, err)
	}
	switch table {
	case "asks":
		ask := model.Ask{ID: id, IssueKey: &issueKey, Question: body, State: "open"}
		if _, err := IndexAskParts(ctx, tx, NewReader(ctx, tx, "ask", id, itemBlockServer), ask, at); err != nil {
			t.Fatalf("index the parts of ask %s: %v", id, err)
		}
	case "comments":
		if _, err := ReplaceCountedItem(ctx, tx, ItemComment, id, body, itemBlockServer, at); err != nil {
			t.Fatalf("index comment %s: %v", id, err)
		}
	case "messages":
		if _, err := ReplaceCountedItem(ctx, tx, ItemMessage, id, body, itemBlockServer, at); err != nil {
			t.Fatalf("index message %s: %v", id, err)
		}
	}
	var block string
	tables := map[string]string{"asks": "ask_blocks where ask_id", "comments": "comment_blocks where comment_id", "messages": "message_blocks where message_id"}
	if err := tx.QueryRow(ctx, `select node_id from `+tables[table]+` = $1::uuid and position is not null order by position limit 1`, id).Scan(&block); err != nil {
		t.Fatalf("read the first block of %s %s: %v", table, id, err)
	}
	return id, block
}

// R2's test (dispatch://INFRA-1253/spec R2), read through both of its functions. An ask, a
// person's comment, an agent's reply in a person's thread, an agent's status comment, a person's
// message and a message of the Legion daemon's session - one alone and one in the person's
// thread - are each read by IsConversationNode, themselves and their first blocks, under every
// kind a reader names them by, and by ConversationNodes, which lists every one that passes with
// its blocks, in the order they were written, and no other. An agent's thread is read before and
// after a person replies in it: it is no conversation before that reply, and is one from then on,
// read as of the later time alone. And a write's parent at the status comment or the daemon's
// message is refused, through the resolver the document service calls (BlockTargets), while one at
// the person's comment, itself or its block, is taken.
func TestAConversationNodeIsWhatRTwoSaysItIs(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fixture transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	spec, _ := createReferenceArtifact(t, tx, "CORE-1", "CORE", "spec")

	const (
		person = `{"kind":"user","id":"alice"}`
		agent  = `{"kind":"session","id":"architect-1"}`
		daemon = `{"kind":"session","id":"legion-daemon:CORE"}`
	)
	t1 := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	second := func(n int) time.Time { return t1.Add(time.Duration(n) * time.Second) }
	ask, askPart := conversationItem(t, tx, "asks", "CORE-1", agent, "Which order first?", second(0), "")
	said, saidBlock := conversationItem(t, tx, "comments", "CORE-1", person, "I want the urgent asks first.\n\nThen the rest.", second(1), "")
	status, statusBlock := conversationItem(t, tx, "comments", "CORE-1", agent, "Round 2 is done.", second(2), "")
	wrote, wroteBlock := conversationItem(t, tx, "messages", "CORE-1", person, "Ship it on Friday.", second(3), "")
	lifecycle, lifecycleBlock := conversationItem(t, tx, "messages", "CORE-1", daemon, "READY: the issue is claimed.", second(4), "")
	thread, threadBlock := conversationItem(t, tx, "comments", "CORE-1", agent, "A note on the plan.", second(5), "")
	reply, replyBlock := conversationItem(t, tx, "comments", "CORE-1", agent, "Done: the urgent asks come first.", second(10), said)
	answer, answerBlock := conversationItem(t, tx, "messages", "CORE-1", daemon, "Withdrawn.", second(11), wrote)
	followUp, _ := conversationItem(t, tx, "comments", "CORE-1", agent, "And a second note.", second(12), thread)
	before := second(20)
	joined, _ := conversationItem(t, tx, "comments", "CORE-1", person, "Then size the plan.", second(30), thread)
	after := second(40)

	for _, test := range []struct {
		name          string
		kind, id      string
		before, after bool
	}{
		{"an agent's ask", "ask", ask, true, true},
		{"a part of the ask", "ask_block", askPart, true, true},
		{"a part of the ask, as a block node", "block", askPart, true, true},
		{"a person's comment", "comment", said, true, true},
		{"a block of the person's comment", "comment_block", saidBlock, true, true},
		{"a block of the person's comment, as a block node", "block", saidBlock, true, true},
		{"an agent's reply in the person's thread", "comment", reply, true, true},
		{"a block of that reply", "comment_block", replyBlock, true, true},
		{"an agent's status comment", "comment", status, false, false},
		{"a block of the status comment", "comment_block", statusBlock, false, false},
		{"a person's message", "message", wrote, true, true},
		{"a block of the person's message", "message_block", wroteBlock, true, true},
		{"a message of the daemon's session", "message", lifecycle, false, false},
		{"a block of that message", "message_block", lifecycleBlock, false, false},
		{"a message of the daemon's session in the person's thread", "message", answer, false, false},
		{"a block of that reply", "message_block", answerBlock, false, false},
		{"an agent's thread a person replies in later", "comment", thread, false, true},
		{"a block of that thread's head", "comment_block", threadBlock, false, true},
		{"an agent's reply in that thread", "comment", followUp, false, true},
		{"the person's reply in that thread", "comment", joined, false, true},
		{"an item of no conversation kind", "issue", "CORE-1", false, false},
		{"a comment no row holds", "comment", "77777777-7777-4777-8777-777777777777", false, false},
		{"a block a comment does not hold", "comment_block", "comment/" + said + "#b99", false, false},
		{"a comment's block named under a message's kind", "message_block", saidBlock, false, false},
	} {
		for _, at := range []struct {
			when time.Time
			want bool
		}{{before, test.before}, {after, test.after}} {
			got, err := IsConversationNode(ctx, tx, test.kind, test.id, at.when)
			if err != nil {
				t.Fatalf("IsConversationNode(%s %s, %s) = %v", test.kind, test.id, at.when, err)
			}
			if got != at.want {
				t.Errorf("IsConversationNode of %s (%s %s) at %s = %t, want %t", test.name, test.kind, test.id, at.when.Format(time.TimeOnly), got, at.want)
			}
		}
	}
	// An item that did not stand at a time is no conversation node then.
	if got, err := IsConversationNode(ctx, tx, "comment", joined, before); err != nil || got {
		t.Errorf("IsConversationNode of the person's later reply before it was written = %t, %v; want false", got, err)
	}

	listed := func(at time.Time) ([]string, map[string]string) {
		t.Helper()
		nodes, err := ConversationNodes(ctx, tx, model.Actor{}, "CORE-1", at)
		if err != nil {
			t.Fatalf("ConversationNodes(CORE-1, %s) = %v", at, err)
		}
		var items []string
		blocks := map[string]string{}
		for index, node := range nodes {
			if node.Kind != "block" {
				items = append(items, node.Kind+":"+node.ID)
				continue
			}
			// Each block follows its item, directly or after the item's earlier blocks.
			if node.Parent == nil || len(items) == 0 || items[len(items)-1] != node.Parent.Kind+":"+node.Parent.ID {
				t.Errorf("node %d, block %s, follows %v and names the parent %+v, want the item it is a block of", index, node.ID, items, node.Parent)
			}
			blocks[node.ID] = node.Parent.Kind + ":" + node.Parent.ID
		}
		return items, blocks
	}
	items, blocks := listed(before)
	if want := []string{"ask:" + ask, "comment:" + said, "message:" + wrote, "comment:" + reply}; !slices.Equal(items, want) {
		t.Errorf("ConversationNodes before the person joins the agent's thread = %v, want %v", items, want)
	}
	for block, item := range map[string]string{askPart: "ask:" + ask, saidBlock: "comment:" + said, wroteBlock: "message:" + wrote, replyBlock: "comment:" + reply} {
		if blocks[block] != item {
			t.Errorf("ConversationNodes lists block %s under %q, want it under %s", block, blocks[block], item)
		}
	}
	items, _ = listed(after)
	if want := []string{"ask:" + ask, "comment:" + said, "message:" + wrote, "comment:" + thread, "comment:" + reply, "comment:" + followUp, "comment:" + joined}; !slices.Equal(items, want) {
		t.Errorf("ConversationNodes after the person joins the agent's thread = %v, want %v", items, want)
	}

	// A criterion's parent, through the resolver docs.refuseBlockPointers calls.
	check := func(parent string) error {
		t.Helper()
		tree, err := pmdoc.Parse(":::criterion{#k1 parent=\"" + parent + "\"}\nI want it.\n:::\n")
		if err != nil {
			t.Fatalf("parse a criterion under %s: %v", parent, err)
		}
		return pmdoc.CheckBlockPointers(tree, nil, func(targets []text.Ref) (pmdoc.BlockWriting, map[string]pmdoc.BlockTarget, error) {
			return BlockTargets(ctx, tx, spec, nil, targets)
		})
	}
	blockRef := func(item, nodeID string) string {
		_, id, block, _ := ParseItemBlockNode(nodeID)
		return "dispatch://CORE-1/" + item + "/" + id + "#b-" + text.EncodeBlockID(block)
	}
	for _, taken := range []string{blockRef("comment", saidBlock), "dispatch://CORE-1/comment/" + said, "dispatch://CORE-1/ask/" + ask, blockRef("comment", replyBlock)} {
		if err := check(taken); err != nil {
			t.Errorf("a criterion under %s = %v, want it taken", taken, err)
		}
	}
	for _, refused := range []string{blockRef("comment", statusBlock), "dispatch://CORE-1/comment/" + status, blockRef("message", lifecycleBlock), "dispatch://CORE-1/message/" + lifecycle} {
		err := check(refused)
		if !errors.Is(err, pmdoc.ErrBlockPointer) || !strings.Contains(err.Error(), `"parent"`) || !strings.Contains(err.Error(), "no conversation node") {
			t.Errorf("a criterion under %s = %v, want it refused as no conversation node, naming the attribute", refused, err)
		}
	}
}
