package docs

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// askInvalidValues is every `invalid` tuple the node of ask holds, in the order they were
// appended: each value and whether it asserts it.
func askInvalidValues(t *testing.T, service *Service, ask string) []graphstore.Tuple {
	t.Helper()
	tuples, err := graphstore.History(context.Background(), service.store.Pool, graphstore.KindAsk, ask, "invalid")
	if err != nil {
		t.Fatalf("read the invalid values of ask %s: %v", ask, err)
	}
	return tuples
}

// withAskQuestion is the browser's edit setting the question of the ask block blockID, its first
// paragraph, to question: "" leaves that paragraph holding no text, which no strict read accepts.
func withAskQuestion(t *testing.T, blockID, question string) func(*pmdoc.Node) *pmdoc.Node {
	return func(tree *pmdoc.Node) *pmdoc.Node {
		block := blockByID(tree, blockID)
		if block == nil || len(block.Children) == 0 || block.Children[0].Type != "paragraph" {
			t.Fatalf("no ask block %q opening with a paragraph in %#v", blockID, tree)
		}
		block.Children[0].Children = nil
		if question != "" {
			block.Children[0].Children = []*pmdoc.Node{{Type: "text", Text: question}}
		}
		return tree
	}
}

// Settlement keeps writing `invalid` into the live tree, and where the block has an ask row
// the reason is a value of the ask's node too. A block that fails the strict read is flagged in the
// live tree and its node holds the reason; once the block reads again the live tree's attribute is
// gone, so the repaired block shows no old reason, and the node's value is retracted. The issue's
// head projection holds the reason while it stands and drops it with the retraction. A block with
// no ask row - one that never read - is flagged in the live tree alone, and no node holds its
// reason. And on a fleet of one build - this test registers no process - a block that fails the
// strict read and claims an answer state and an answerer keeps none of the seven attributes a
// version can store in the live tree, as a block that reads keeps none.
func TestTheInvalidReasonIsWrittenAndClearedOnTheAskNode(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ship urgency=\"med\" multiple=\"false\"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n")
	settleCurrentGeneration(t, service, artifactID)
	ask := blockAskOn(t, service, artifactID, "ship")
	const reason = `ask block "ship" has an empty question`
	requireHeadInvalid := func(want string) {
		t.Helper()
		var got string
		if err := service.store.Pool.QueryRow(context.Background(), `
			select coalesce(value_texts[array_position(value_kinds, 'invalid')], '')
			from issue_head_nodes where issue_key = $1 and node_kind = $2 and node_address = $3
		`, ask.IssueKey, graphstore.KindAsk, ask.ID).Scan(&got); err != nil {
			t.Fatalf("read the head row of ask %s: %v", ask.ID, err)
		}
		if got != want {
			t.Errorf("the ask's head row holds invalid %q, want %q", got, want)
		}
	}
	requireHeadInvalid("")

	editLiveTree(t, service, artifactID, withAskQuestion(t, "ship", ""))
	settleCurrentGeneration(t, service, artifactID)
	if got := blockByID(liveTree(t, service, artifactID), "ship").Attrs["invalid"]; got != reason {
		t.Fatalf("the live tree's invalid on the block that fails the strict read = %#v, want %q", got, reason)
	}
	if value, held, err := graphstore.CurrentValue(context.Background(), service.store.Pool, graphstore.KindAsk, ask.ID, "invalid"); err != nil || !held || value != reason {
		t.Fatalf("the ask node's invalid = %q, held %v (%v), want %q", value, held, err, reason)
	}
	requireHeadInvalid(reason)

	editLiveTree(t, service, artifactID, withAskQuestion(t, "ship", "Should we ship?"))
	settleCurrentGeneration(t, service, artifactID)
	if got, held := blockByID(liveTree(t, service, artifactID), "ship").Attrs["invalid"]; held {
		t.Fatalf("the repaired block still shows invalid %#v in the live tree, want the old reason gone", got)
	}
	if value, held, err := graphstore.CurrentValue(context.Background(), service.store.Pool, graphstore.KindAsk, ask.ID, "invalid"); err != nil || held {
		t.Fatalf("the repaired ask's node holds invalid %q, held %v (%v), want it retracted", value, held, err)
	}
	requireHeadInvalid("")
	if values := askInvalidValues(t, service, ask.ID); len(values) != 2 || !values[0].Asserted || values[1].Asserted || values[1].ToID != reason {
		t.Fatalf("the ask node's invalid values = %+v, want the reason asserted and then retracted", values)
	}

	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children = append(tree.Children, &pmdoc.Node{
			Type:     "ask",
			Attrs:    pmdoc.Attrs{pmdoc.BlockIDAttr: "never-read", "urgency": "low", "multiple": false},
			Children: []*pmdoc.Node{{Type: "paragraph", Attrs: pmdoc.Attrs{}}},
		})
		return tree
	})
	settleCurrentGeneration(t, service, artifactID)
	if got := blockByID(liveTree(t, service, artifactID), "never-read").Attrs["invalid"]; got != `ask block "never-read" has an empty question` {
		t.Fatalf("the live tree's invalid on the block no ask row backs = %#v, want its reason", got)
	}
	var rows, values int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select (select count(*) from asks where block_artifact_id = $1 and block_id = 'never-read'),
		       (select count(*) from graph_tuples where from_kind = $2 and kind = 'invalid')
	`, artifactID, graphstore.KindAsk).Scan(&rows, &values); err != nil {
		t.Fatalf("count the never-read block's rows and the ask nodes' invalid values: %v", err)
	}
	if rows != 0 || values != 2 {
		t.Fatalf("the block no ask row backs has %d rows and the ask nodes hold %d invalid values, want no row and ship's two values alone", rows, values)
	}

	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		tree = withAskQuestion(t, "ship", "")(tree)
		block := blockByID(tree, "ship")
		block.Attrs["state"], block.Attrs["answered_by"] = "answered", "mallory"
		return tree
	})
	settleCurrentGeneration(t, service, artifactID)
	block := blockByID(liveTree(t, service, artifactID), "ship")
	if got := block.Attrs["invalid"]; got != reason {
		t.Fatalf("the forged block's invalid = %#v, want %q", got, reason)
	}
	for _, name := range storedAskAttributes() {
		if value, held := block.Attrs[name]; held {
			t.Errorf("on a fleet of one build the block that fails the strict read keeps %s=%v in the live tree, want none of the seven", name, value)
		}
	}
}

// On a fleet of one build a block that fails the strict read keeps of the seven attributes a
// version can store only what a version keeps of its ask as the ask's node holds it (R5,
// keptAskState): an answered ask's block a browser empties and forges another answerer onto keeps
// its node's state, answerer and time, and the forged answerer does not come back; a block no ask
// row backs keeps none of the seven, whatever it claims.
func TestABlockThatFailsTheStrictReadKeepsOnlyWhatItsAsksNodeHolds(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ship urgency=\"med\" multiple=\"false\"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n")
	settleCurrentGeneration(t, service, artifactID)
	ask := blockAskOn(t, service, artifactID, "ship")
	answerAskOnItsNode(t, service, ask.ID, model.AskAnswer{User: "alice", Selected: []string{"Ship"}, At: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)})
	settleCurrentGeneration(t, service, artifactID)

	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		tree = withAskQuestion(t, "ship", "")(tree)
		block := blockByID(tree, "ship")
		block.Attrs["answered_by"], block.Attrs["selected"] = "mallory", []any{"Hold"}
		tree.Children = append(tree.Children, &pmdoc.Node{
			Type: "ask",
			Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: "rowless", "urgency": "low", "multiple": false,
				"state": "answered", "answered_by": "mallory", "answered_at": "2026-11-20T09:00:00Z"},
			Children: []*pmdoc.Node{{Type: "paragraph", Attrs: pmdoc.Attrs{}}},
		})
		return tree
	})
	settleCurrentGeneration(t, service, artifactID)
	live := liveTree(t, service, artifactID)
	block := blockByID(live, "ship")
	if got := block.Attrs["invalid"]; got != `ask block "ship" has an empty question` {
		t.Fatalf("the emptied block's invalid = %#v, want its reason", got)
	}
	want := pmdoc.Attrs{"state": "answered", "answered_by": "alice", "answered_at": "2026-11-20T09:00:00Z"}
	for _, name := range storedAskAttributes() {
		if got, held := block.Attrs[name]; got != want[name] || held != (want[name] != nil) {
			t.Errorf("the emptied answered block holds %s=%#v (held %v), want %#v from its ask's node", name, got, held, want[name])
		}
	}
	rowless := blockByID(live, "rowless")
	for _, name := range storedAskAttributes() {
		if value, held := rowless.Attrs[name]; held {
			t.Errorf("the block no ask row backs keeps %s=%v, want none of the seven", name, value)
		}
	}
}
