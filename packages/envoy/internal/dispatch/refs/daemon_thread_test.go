package refs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// R2's rule of the Legion daemon (R2 of the issue-tree spec), read through every reader of R2's
// test: no message of a thread whose root the daemon's session posted is a conversation node,
// whoever wrote it, a person's reply included, and neither is the daemon's own message in a thread
// a person began, while every other message of that thread is one.
//
// The issue holds two threads of messages. The first the session legion-daemon:CORE began: its
// READY, a person's reply to it, an agent's reply to it, and a person's reply to that agent's
// reply, two links from the root. The second a person began: her message, the daemon's reply to
// it, her reply to that reply, and an agent's reply to hers. Each message is read by
// IsConversationNode, itself and its first block, under every kind a reader names them by; by
// ConversationNodes; by ConversationPageNodes over a page of no comment; by the head projection's
// read of the same page (ConversationPageNodesOfHeads); by conversationTargets, which a write's
// pointers are resolved through; and by the resolver the document service refuses a parent
// pointer with (BlockTargets under pmdoc.CheckBlockPointers). Every reader answers the first
// thread's four messages as no conversation node, the daemon's reply in the second as none, and
// the three others of the second as conversation nodes: the test reads the root of a message's
// chain and the message itself, never a message between them.
//
// Then DaemonThreadParents, the same test read the other way round: of four part_of tuples into
// those messages - a block of the spec under the person's reply in the daemon's thread, one under
// a block of that reply, one under her reply in her own thread and a message's own, to the
// message it answers - it answers the first two alone, each with the seq of its part_of and the
// issue that holds it, in the order of their addresses, one at a time past a cursor as it answers
// them whole, and the criterion no longer once its part_of names its issue.
func TestNoMessageOfADaemonThreadIsAConversationNode(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fixture transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	spec, refKey := createReferenceArtifact(t, tx, "CORE-1", "CORE", "spec")

	t1 := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	second := func(n int) time.Time { return t1.Add(time.Duration(n) * time.Second) }
	ready, readyBlock := conversationItem(t, tx, "messages", "CORE-1", itemDaemon, "READY: the issue is claimed.", second(0), "")
	held, heldBlock := conversationItem(t, tx, "messages", "CORE-1", itemPerson, "Hold it until Friday.", second(1), ready)
	noted, notedBlock := conversationItem(t, tx, "messages", "CORE-1", itemAgent, "Held until Friday.", second(2), ready)
	deeper, deeperBlock := conversationItem(t, tx, "messages", "CORE-1", itemPerson, "And tell me when it runs.", second(3), noted)
	asked, askedBlock := conversationItem(t, tx, "messages", "CORE-1", itemPerson, "Is the issue claimed?", second(4), "")
	withdrawn, withdrawnBlock := conversationItem(t, tx, "messages", "CORE-1", itemDaemon, "Withdrawn.", second(5), asked)
	under, underBlock := conversationItem(t, tx, "messages", "CORE-1", itemPerson, "Why was it withdrawn?", second(6), withdrawn)
	agreed, agreedBlock := conversationItem(t, tx, "messages", "CORE-1", itemAgent, "It lost its claim.", second(7), under)
	at := second(60)

	messages := []struct {
		name      string
		id, block string
		want      bool
	}{
		{"the daemon's READY, the root of its thread", ready, readyBlock, false},
		{"a person's reply in the daemon's thread", held, heldBlock, false},
		{"an agent's reply in the daemon's thread", noted, notedBlock, false},
		{"a person's reply two links under the daemon's root", deeper, deeperBlock, false},
		{"a person's message, the root of her thread", asked, askedBlock, true},
		{"the daemon's reply in the person's thread", withdrawn, withdrawnBlock, false},
		{"a person's reply to the daemon's reply in her own thread", under, underBlock, true},
		{"an agent's reply in the person's thread", agreed, agreedBlock, true},
	}
	var passing []string
	for _, message := range messages {
		if message.want {
			passing = append(passing, "message:"+message.id)
		}
		for _, named := range []struct{ kind, id string }{
			{"message", message.id}, {"message_block", message.block}, {"block", message.block},
		} {
			got, err := IsConversationNode(ctx, tx, named.kind, named.id, at)
			if err != nil {
				t.Fatalf("IsConversationNode(%s %s) = %v", named.kind, named.id, err)
			}
			if got != message.want {
				t.Errorf("IsConversationNode of %s (%s %s) = %t, want %t", message.name, named.kind, named.id, got, message.want)
			}
		}
	}

	// The three listings: each answers the messages that pass, in the order they were written, each
	// followed by its blocks, and no message of the daemon's thread.
	itemsOf := func(reader string, nodes []ExcerptedNode) []string {
		t.Helper()
		var items []string
		for _, excerpted := range nodes {
			node := excerpted.Node
			if node.Kind != "block" {
				items = append(items, node.Kind+":"+node.ID)
				continue
			}
			if node.Parent == nil || len(items) == 0 || items[len(items)-1] != node.Parent.Kind+":"+node.Parent.ID {
				t.Errorf("%s lists block %s after %v under %+v, want it after the message it is a block of", reader, node.ID, items, node.Parent)
			}
		}
		return items
	}
	whole, err := ConversationNodes(ctx, tx, model.Actor{}, "CORE-1", at, "")
	if err != nil {
		t.Fatalf("ConversationNodes(CORE-1) = %v", err)
	}
	if items := itemsOf("ConversationNodes", whole); !slices.Equal(items, passing) {
		t.Errorf("ConversationNodes = %v, want %v: the messages of the person's thread but the daemon's reply, and none of the daemon's thread", items, passing)
	}
	paged, err := ConversationPageNodes(ctx, tx, model.Actor{}, "CORE-1", at, CommentPage{}, "")
	if err != nil {
		t.Fatalf("ConversationPageNodes(CORE-1) = %v", err)
	}
	if items := itemsOf("ConversationPageNodes", paged); !slices.Equal(items, passing) {
		t.Errorf("ConversationPageNodes = %v, want %v", items, passing)
	}
	headed, read, err := ConversationPageNodesOfHeads(ctx, tx, model.Actor{}, "CORE-1", at, CommentPage{}, &HeadTree{}, "")
	if err != nil || !read {
		t.Fatalf("ConversationPageNodesOfHeads(CORE-1) read %t, %v; want the page of no comment read off the head rows", read, err)
	}
	if items := itemsOf("ConversationPageNodesOfHeads", headed); !slices.Equal(items, passing) {
		t.Errorf("ConversationPageNodesOfHeads = %v, want %v", items, passing)
	}

	// conversationTargets, the read a write's pointers are resolved through, by the message and by
	// its first block.
	messageRef := func(id string) string { return "dispatch://CORE-1/message/" + id }
	var targets []text.Ref
	for _, message := range messages {
		for _, written := range []string{messageRef(message.id), itemBlockReference(message.block)} {
			ref, parsed := text.ParseRef(written, "")
			if !parsed {
				t.Fatalf("%s is no reference", written)
			}
			targets = append(targets, ref)
		}
	}
	resolved, err := conversationTargets(ctx, tx, targets, at)
	if err != nil {
		t.Fatalf("conversationTargets = %v", err)
	}
	for _, message := range messages {
		for _, node := range []string{message.id, message.block} {
			target, found := resolved[node]
			if !found || target.Issue != "CORE-1" {
				t.Errorf("conversationTargets holds %+v (found %t) of %s (%s), want it of CORE-1", target, found, message.name, node)
				continue
			}
			if target.Conversation != message.want {
				t.Errorf("conversationTargets reads %s (%s) a conversation node %t, want %t", message.name, node, target.Conversation, message.want)
			}
		}
	}

	// A criterion's parent, through the resolver docs' refuseBlockPointers calls.
	for _, message := range messages {
		for _, parent := range []string{messageRef(message.id), itemBlockReference(message.block)} {
			err := criterionUnder(t, tx, spec, parent)
			switch {
			case message.want && err != nil:
				t.Errorf("a criterion under %s, %s = %v, want it taken", parent, message.name, err)
			case !message.want && (!errors.Is(err, pmdoc.ErrBlockPointer) || !strings.Contains(err.Error(), "no conversation node")):
				t.Errorf("a criterion under %s, %s = %v, want it refused as no conversation node", parent, message.name, err)
			}
		}
	}

	// The same test read the other way round: the blocks of documents that stand under a message
	// it refuses. The fixture's tuples are written through the store's writer with no head row, as
	// a build before the rule left them.
	tuple := func(fromKind, fromID, toKind, toID string, asserted bool) graphstore.Tuple {
		return graphstore.Tuple{
			FromKind: fromKind, FromID: fromID, Kind: graphstore.TuplePartOf, ToKind: toKind, ToID: toID,
			At: at, Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent, Asserted: asserted,
		}
	}
	write := func(what string, tuples ...graphstore.Tuple) {
		t.Helper()
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueWrite(b, nil, tuples)
			return nil
		}); err != nil {
			t.Fatalf("write %s: %v", what, err)
		}
	}
	criterion, requirement, kept := refKey+"#k1", refKey+"#q1", refKey+"#k2"
	write("the placements a build before the rule left",
		tuple(BlockNodeKind, criterion, string(ItemMessage), held, true),
		tuple(BlockNodeKind, requirement, ItemMessage.BlockKind(), heldBlock, true),
		tuple(BlockNodeKind, kept, string(ItemMessage), under, true),
		tuple(string(ItemMessage), noted, string(ItemMessage), ready, true))
	seqOf := func(node string) int64 {
		t.Helper()
		var seq int64
		if err := tx.QueryRow(ctx, `select standing.seq from (`+BlockPlacementSQL(`$1::text`)+`) standing`, node).Scan(&seq); err != nil {
			t.Fatalf("read the placement of %s: %v", node, err)
		}
		return seq
	}
	want := []DaemonThreadParent{
		{Node: criterion, Parent: Target{Kind: string(ItemMessage), ID: held}, Seq: seqOf(criterion), Holder: "CORE-1", Issue: "CORE-1"},
		{Node: requirement, Parent: Target{Kind: ItemMessage.BlockKind(), ID: heldBlock}, Seq: seqOf(requirement), Holder: "CORE-1", Issue: "CORE-1"},
	}
	parents, err := DaemonThreadParents(ctx, tx, "", 0)
	if err != nil {
		t.Fatalf("DaemonThreadParents = %v", err)
	}
	if !slices.Equal(parents, want) {
		t.Errorf("DaemonThreadParents = %+v, want %+v: the two blocks of the spec under the person's reply in the daemon's thread, and neither the block under her reply in her own thread nor the agent's reply", parents, want)
	}
	first, err := DaemonThreadParents(ctx, tx, "", 1)
	if err != nil || !slices.Equal(first, want[:1]) {
		t.Errorf("DaemonThreadParents, the first one = %+v, %v; want %+v", first, err, want[:1])
	}
	next, err := DaemonThreadParents(ctx, tx, criterion, 1)
	if err != nil || !slices.Equal(next, want[1:]) {
		t.Errorf("DaemonThreadParents past %s = %+v, %v; want %+v", criterion, next, err, want[1:])
	}
	write("the criterion's move under its issue",
		tuple(BlockNodeKind, criterion, string(ItemMessage), held, false),
		tuple(BlockNodeKind, criterion, graphstore.KindIssue, "CORE-1", true))
	moved, err := DaemonThreadParents(ctx, tx, "", 0)
	if err != nil || !slices.Equal(moved, want[1:]) {
		t.Errorf("DaemonThreadParents once the criterion stands under its issue = %+v, %v; want %+v", moved, err, want[1:])
	}
}
