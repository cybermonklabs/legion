package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// openAsksQueryText is the statement the dropped-ask refusals read the open asks with
// (docs.openAskBlocks), by a run of its text no other statement holds, so a counter says whether a
// batch read the open asks of a document at all.
const openAsksQueryText = "block_artifact_id = $1 and state = 'open'"

// refuseEdit sends ops to the document and returns the refusal's body, failing the test when the
// route took the edit instead.
func refuseEdit(t *testing.T, handler http.Handler, artifactID string, ops []map[string]any) errorBody {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"ops": ops,
	}, "alice")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("edit %v: status=%d body=%s, want 400", ops, response.Code, response.Body.String())
	}
	return decodeBody[errorBody](t, response)
}

// R5: a question a person can still answer is not lost to an edit. The batch is refused whatever
// operation drops the block, with nothing of the batch written and the ask still open. A `delete`
// drives it, since no `replace` reaches this refusal (measured 2026-11-10, named in the
// Verification document of dispatch://INFRA-1520): a quote-anchored match stays inside one
// textblock, a list written over an ask's question lands inside the ask, a `with` that rewrites
// the ask under its own id keeps it, a `with` that rewrites the block holding it writes that id
// twice and is refused by the repeated-id guard, and blockReplacement refuses the lift-out of a
// typed block.
func TestAnEditDroppingABlockAnOpenAskPointsAtIsRefused(t *testing.T) {
	handler, _ := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Dropped decision block", "decision", transportAsk, "Which transport?")
	before := documentMarkdown(t, handler, issue.PrimaryArtifactID)
	versions := len(documentVersions(t, handler, issue.PrimaryArtifactID))

	refusal := refuseEdit(t, handler, issue.PrimaryArtifactID, []map[string]any{
		{"op": "insert", "after": "end", "markdown": "A tail paragraph."},
		{"op": "delete", "block": "decision"},
	})
	if refusal.Code != "INVALID_ASK_BLOCK" || !strings.Contains(refusal.Error, "decision") {
		t.Fatalf("refusal = %#v, want INVALID_ASK_BLOCK naming the block decision", refusal)
	}
	if after := documentMarkdown(t, handler, issue.PrimaryArtifactID); after != before {
		t.Fatalf("the refused batch left the document %q, want it unchanged at %q", after, before)
	}
	if now := len(documentVersions(t, handler, issue.PrimaryArtifactID)); now != versions {
		t.Fatalf("the refused batch left %d versions, want the %d it started from", now, versions)
	}
	if ask := readBlockAsk(t, handler, askID); ask.State != "open" {
		t.Fatalf("the ask of the refused batch's block reads %q, want it still open", ask.State)
	}
}

// The `replace` half: a `with` that rewrites the block holding an open ask under that block's own
// id writes the id twice, which the repeated-id guard refuses before any ask check, and the ask
// stays open.
func TestAReplaceRewritingTheBlockOfAnOpenAskUnderItsOwnIDIsRefused(t *testing.T) {
	handler, _ := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Rewritten decision block", "decision", transportAsk, "Which transport?")
	before := documentMarkdown(t, handler, issue.PrimaryArtifactID)

	refusal := refuseEdit(t, handler, issue.PrimaryArtifactID, []map[string]any{{
		"op":   "replace",
		"find": "Context",
		"with": ":::ask{#decision urgency=\"high\" multiple=\"false\"}\nWhich transport ships first?\n:::",
	}})
	if refusal.Code != "INVALID_OP" || !strings.Contains(refusal.Error, `"decision" would name two blocks`) {
		t.Fatalf("refusal = %#v, want INVALID_OP from the repeated-id guard", refusal)
	}
	if after := documentMarkdown(t, handler, issue.PrimaryArtifactID); after != before {
		t.Fatalf("the refused replace left the document %q, want it unchanged at %q", after, before)
	}
	if ask := readBlockAsk(t, handler, askID); ask.State != "open" {
		t.Fatalf("the ask of the refused replace's block reads %q, want it still open", ask.State)
	}
}

// R5, the other half: a decision block a `replace` writes opens its ask, as the same text through
// an `insert` does. Settlement opens the ask after the edit commits, so each half waits for the
// block to be indexed and then reads the issue's asks.
func TestAReplaceWritingAnAskBlockOpensItsAskAsAnInsertDoes(t *testing.T) {
	const question = "Which store?"
	const block = ":::ask{#written urgency=\"med\" multiple=\"false\"}\n" + question + "\n\n" +
		"- Postgres: The one we run\n- SQLite: One file\n:::"
	for _, test := range []struct {
		name string
		op   map[string]any
	}{
		{name: "a replace", op: map[string]any{"op": "replace", "find": "Placeholder.", "with": block}},
		{name: "an insert", op: map[string]any{"op": "insert", "after": "end", "markdown": block}},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, _ := blockAskHandler(t)
			issue := createInteractionIssue(t, handler, "TEST", "Ask block from "+test.name, "Context\n\nPlaceholder.\n")
			written := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]any{test.op},
			}, "alice")
			if written.Code != http.StatusOK {
				t.Fatalf("write the ask block: status=%d body=%s", written.Code, written.Body.String())
			}
			awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "written", question)

			listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks?state=all", nil, "alice")
			if listed.Code != http.StatusOK {
				t.Fatalf("list asks: status=%d body=%s", listed.Code, listed.Body.String())
			}
			asks := decodeBody[[]model.Ask](t, listed)
			if len(asks) != 1 {
				t.Fatalf("the issue holds %d asks, want the one the block opened: %#v", len(asks), asks)
			}
			ask := asks[0]
			if ask.BlockID == nil || *ask.BlockID != "written" || ask.State != "open" || ask.Question != question {
				t.Fatalf("the opened ask = %#v, want it open on block written with the block's question", ask)
			}
			if len(ask.Options) != 2 || ask.Options[0].Label != "Postgres" || ask.Options[1].Label != "SQLite" {
				t.Fatalf("the opened ask's options = %#v, want the block's two", ask.Options)
			}
		})
	}
}

// The read of the open asks costs a statement, so a batch pays for it only where it may refuse:
// both trees are in hand, the comparison of their ask block ids is in memory, and only a batch
// whose result drops one reads the table. The same counter sees the statement the refused batch
// below sends, so a count of zero is the absence of the read and not of the counter.
func TestOnlyABatchDroppingAnAskBlockReadsTheOpenAsks(t *testing.T) {
	counter := &statementCounter{}
	counted := storetest.TracedStore(t, storetest.Open(t), counter)
	handler, _, _ := newTestServer(t, testServerOptions{store: counted})
	issue := createInteractionIssue(t, handler, "TEST", "Open ask reads", "Context\n\nPlaceholder.\n")
	written := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": transportAsk}},
	}, "alice")
	if written.Code != http.StatusOK {
		t.Fatalf("write the ask block: status=%d body=%s", written.Code, written.Body.String())
	}
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "decision", "Which transport?")

	kept := counter.mark()
	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "replace", "find": "Placeholder.", "with": "- first\n- second"}},
	}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("edit that drops no ask block: status=%d body=%s", edited.Code, edited.Body.String())
	}
	if reads := counter.count(kept, openAsksQueryText, issue.PrimaryArtifactID); reads != 0 {
		t.Fatalf("a batch that drops no ask block read the open asks %d times, want none", reads)
	}

	dropping := counter.mark()
	refuseEdit(t, handler, issue.PrimaryArtifactID, []map[string]any{{"op": "delete", "block": "decision"}})
	if reads := counter.count(dropping, openAsksQueryText, issue.PrimaryArtifactID); reads != 1 {
		t.Fatalf("the batch that drops the ask block read the open asks %d times, want one", reads)
	}
}

// What a `replace` writes into an ask is held to the batch's own ask check
// (validateEditedAskBlocks), which reads the block as settlement reads it: a code block written
// into the ask's question is refused as a broken ask block, by the reason settlement would have
// flagged it with, and not as an invalid `with`, while a bullet list written under the question
// stands as the ask's options. The operation alone cannot reach that check
// (docs.TestApplyOperationReplaceWritesBlocksInsideTheBlockThatHoldsThem runs it alone), so the
// batch drives both halves here.
func TestAReplaceIntoAnAsksQuestionIsHeldToTheAsksContentRule(t *testing.T) {
	t.Run("a code block is refused as a broken ask block", func(t *testing.T) {
		handler, _ := blockAskHandler(t)
		issue, askID := seedBlockAsk(t, handler, "Code in a decision", "decision", transportAsk, "Which transport?")
		before := documentMarkdown(t, handler, issue.PrimaryArtifactID)

		refusal := refuseEdit(t, handler, issue.PrimaryArtifactID, []map[string]any{{
			"op":   "replace",
			"find": "Which transport?",
			"with": "Which transport?\n\n```go\nrpc()\n```",
		}})
		if refusal.Code != "INVALID_ASK_BLOCK" ||
			!strings.Contains(refusal.Error, `ask block "decision"`) || !strings.Contains(refusal.Error, "code_block") {
			t.Fatalf("refusal = %#v, want INVALID_ASK_BLOCK naming the ask and the code block in it", refusal)
		}
		if after := documentMarkdown(t, handler, issue.PrimaryArtifactID); after != before {
			t.Fatalf("the refused replace left the document %q, want it unchanged at %q", after, before)
		}
		if ask := readBlockAsk(t, handler, askID); ask.State != "open" {
			t.Fatalf("the ask of the refused replace reads %q, want it still open", ask.State)
		}
	})

	t.Run("a bullet list stands as the ask's options", func(t *testing.T) {
		handler, _ := blockAskHandler(t)
		issue, askID := seedBlockAsk(t, handler, "Options into a decision", "decision",
			":::ask{#decision urgency=\"high\" multiple=\"false\"}\nWhich transport?\n:::\n", "Which transport?")

		edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
			"ops": []map[string]any{{
				"op":   "replace",
				"find": "Which transport?",
				"with": "Which transport?\n\n- REST: Matches the platform\n- gRPC: Adds streaming",
			}},
		}, "alice")
		if edited.Code != http.StatusOK {
			t.Fatalf("write the options: status=%d body=%s", edited.Code, edited.Body.String())
		}
		settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-options")
		ask := readBlockAsk(t, handler, askID)
		if ask.State != "open" || ask.Question != "Which transport?" {
			t.Fatalf("the ask after the options were written = %#v, want it open with its question", ask)
		}
		if len(ask.Options) != 2 || ask.Options[0].Label != "REST" || ask.Options[1].Label != "gRPC" {
			t.Fatalf("the ask's options = %#v, want the two the replace wrote", ask.Options)
		}
	})
}
