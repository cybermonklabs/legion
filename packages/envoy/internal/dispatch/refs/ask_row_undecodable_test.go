package refs

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// askThreadReply inserts a person's comment of CORE-1 in the thread of ask, written at at, as a
// reply in an ask's thread is stored (api's threadHeadOf): ask_id names the ask, and answers the
// comment the reply names where answers is not "". It indexes the comment's blocks as a write of
// this build does and returns its id.
func askThreadReply(t *testing.T, tx pgx.Tx, ask, answers, body string, at time.Time) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(context.Background(), `
		insert into comments (issue_key, author, body, created_at, ask_id, answers)
		values ('CORE-1', $1, $2, $3, $4::uuid, nullif($5, '')::uuid) returning id::text
	`, itemPerson, body, at, ask, answers).Scan(&id); err != nil {
		t.Fatalf("insert a reply in the thread of ask %s: %v", ask, err)
	}
	indexItemAsAWriteDoes(t, tx, ItemComment, id, body, itemBlockServer, at)
	return id
}

// An ask a stored value of which does not decode is left out of every read of the graph, and so is
// each of its parts (loadAskNodes, itemBlockNodes, R4 of dispatch://INFRA-1150), and the
// conversation of its issue leaves out with it every item that stands under it (ConversationNodes,
// leftOutThreads), so no walk of the issue's tree hangs such an item under the issue in its place.
// Of three asks of CORE-1 a person wrote, one row holds options that are valid JSON and no list of
// options, written by hand (asks_options_array admits any JSON array), and one an author that is no
// actor. Neither of the two resolves as a node, nor does its question part, while the third ask and
// its part do. A person replied in each ask's thread and then replied to that reply: the
// conversation lists the third ask with its part and its two replies, each under what it answers,
// and of the other two asks nothing - no ask, no part, and neither reply in either's thread.
func TestAnAskWhoseRowDoesNotDecodeIsLeftOutWithItsPartsAndItsThread(t *testing.T) {
	t.Parallel()
	tx := askPartFixture(t)
	ctx := context.Background()
	t1 := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	second := func(n int) time.Time { return t1.Add(time.Duration(n) * time.Second) }
	fine, finePart := conversationItem(t, tx, "asks", "CORE-1", itemPerson, "Which store?", second(0), "")
	options, optionsPart := conversationItem(t, tx, "asks", "CORE-1", itemPerson, "Which name?", second(1), "")
	author, authorPart := conversationItem(t, tx, "asks", "CORE-1", itemPerson, "Which region?", second(2), "")
	replies := map[string][2]string{}
	for index, ask := range []string{fine, options, author} {
		first := askThreadReply(t, tx, ask, "", "Why ask?", second(10+2*index))
		replies[ask] = [2]string{first, askThreadReply(t, tx, ask, first, "Because it is open.", second(11+2*index))}
	}
	for _, broken := range []struct{ what, statement, ask string }{
		{"options that are no list of options", `update asks set options = '["Postgres"]'::jsonb where id = $1::uuid`, options},
		{"an author that is no actor", `update asks set author = '"alice"'::jsonb where id = $1::uuid`, author},
	} {
		if tag, err := tx.Exec(ctx, broken.statement, broken.ask); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("write %s on the row of ask %s: %v", broken.what, broken.ask, err)
		}
	}

	for _, node := range []struct {
		what, kind, id string
		resolves       bool
	}{
		{"the ask whose row decodes", string(ItemAsk), fine, true},
		{"the question part of the ask whose row decodes", ItemAsk.BlockKind(), finePart, true},
		{"the ask whose options are no list of options", string(ItemAsk), options, false},
		{"the question part of the ask whose options are no list of options", ItemAsk.BlockKind(), optionsPart, false},
		{"the ask whose author is no actor", string(ItemAsk), author, false},
		{"the question part of the ask whose author is no actor", ItemAsk.BlockKind(), authorPart, false},
	} {
		_, found, err := Node(ctx, tx, model.Actor{}, node.kind, node.id, false)
		if err != nil {
			t.Fatalf("Node(%s %s), %s: %v", node.kind, node.id, node.what, err)
		}
		if found != node.resolves {
			t.Errorf("%s (%s %s) resolves %t, want %t", node.what, node.kind, node.id, found, node.resolves)
		}
	}

	nodes, err := ConversationNodes(ctx, tx, model.Actor{}, "CORE-1", second(30), "")
	if err != nil {
		t.Fatalf("ConversationNodes(CORE-1) = %v", err)
	}
	left := []string{options, author, replies[options][0], replies[options][1], replies[author][0], replies[author][1]}
	var items []string
	parents := map[string]string{}
	for _, excerpted := range nodes {
		node := excerpted.Node
		if node.Kind != BlockNodeKind {
			items = append(items, node.Kind+":"+node.ID)
		}
		if node.Parent != nil {
			parents[node.ID] = node.Parent.Kind + ":" + node.Parent.ID
		}
		for _, id := range left {
			if strings.Contains(node.ID, id) || (node.Parent != nil && strings.Contains(node.Parent.ID, id)) {
				t.Errorf("ConversationNodes lists %s:%s under %s, a node of %s, an ask whose row does not decode or a reply in its thread",
					node.Kind, node.ID, parents[node.ID], id)
			}
		}
	}
	first, again := replies[fine][0], replies[fine][1]
	if want := []string{"ask:" + fine, "comment:" + first, "comment:" + again}; !slices.Equal(items, want) {
		t.Errorf("ConversationNodes lists the items %v, want %v: the ask whose row decodes and the two replies in its thread", items, want)
	}
	for id, parent := range map[string]string{finePart: "ask:" + fine, first: "ask:" + fine, again: "comment:" + first} {
		if parents[id] != parent {
			t.Errorf("ConversationNodes lists %s under %q, want it under %s", id, parents[id], parent)
		}
	}
}
