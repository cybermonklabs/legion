package api

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A delivery reply's quote is checked above the route's row locks, with the block it names, and
// its refusal is held back to where the block's refusal is raised (deferredRefusal): after the
// mention and the attempt are checked, so no answer of either delivery-reply route moves. Each
// attempt the route refuses - one no delivery has, one sent to another session, and on the comment
// route a target the comment does not mention - answers a reply whose quote its block does not
// hold, past its last match, or held twice with no occurrence, byte for byte as it answers a reply
// naming a block the post does not hold. So does an attempt the session already answered, which
// hands back the reply it holds. The control: an attempt the route takes answers the quote's own
// refusal, which is not the block's.
func TestADeliveryReplysQuoteRefusalComesWhereItsBlockRefusalComes(t *testing.T) {
	t.Parallel()
	handler, _, _ := newQuoteHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Quote refusals in their place", "before").Key
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue, map[string]any{
		"body": quotedTwice, "mentions": []map[string]any{{"target": "session:" + quoteSession}},
	})).ID
	targeted := createIssueMessage(t, handler, issue, map[string]any{
		"body": quotedTwice, "target": "session:" + quoteSession, "delivery": "btw",
	}, "alice").ID

	for _, route := range []struct {
		name, path, field, code string
		// refused are the attempts the route refuses, each as the fields that name it.
		refused map[string]map[string]any
	}{
		{"POST /api/v1/comments/{id}/reply", "/api/v1/comments/" + comment + "/reply", "reply_to_block", "INVALID_COMMENT", map[string]map[string]any{
			"an attempt no delivery has":            {"attempt": 7},
			"an attempt sent to another session":    {"actor": map[string]any{"kind": "session", "id": "s2"}},
			"a target the comment does not mention": {"target": "session:s9"},
		}},
		{"POST /api/v1/messages/{id}/reply", "/api/v1/messages/" + targeted + "/reply", "in_reply_to_block", "INVALID_MESSAGE", map[string]map[string]any{
			"an attempt no delivery has":         {"attempt": 7},
			"an attempt sent to another session": {"actor": map[string]any{"kind": "session", "id": "s2"}},
		}},
	} {
		t.Run(route.name, func(t *testing.T) {
			send := func(attempt, named map[string]any) *httptest.ResponseRecorder {
				t.Helper()
				input := callbackAs(quoteSession, map[string]any{"body": "On the second alpha."})
				maps.Copy(input, attempt)
				maps.Copy(input, named)
				return bearerRequest(t, handler, http.MethodPost, route.path, input)
			}
			badBlock := map[string]any{route.field: "b9"}
			badQuotes := map[string]map[string]any{
				"a quote the block does not hold":       withQuote(map[string]any{}, route.field, "b1", "zarquonabsent", -1),
				"an occurrence past the last match":     withQuote(map[string]any{}, route.field, "b1", quotedWord, 2),
				"a word held twice, with no occurrence": withQuote(map[string]any{}, route.field, "b1", quotedWord, -1),
			}
			requireSameAnswer := func(step string, attempt map[string]any) {
				t.Helper()
				block := send(attempt, badBlock)
				for name, quote := range badQuotes {
					quoted := send(attempt, quote)
					if quoted.Code != block.Code || quoted.Body.String() != block.Body.String() {
						t.Fatalf("%s, %s: status=%d body=%s, want what a bad block answers: status=%d body=%s",
							step, name, quoted.Code, quoted.Body.String(), block.Code, block.Body.String())
					}
				}
			}
			for name, attempt := range route.refused {
				requireSameAnswer(name, attempt)
			}

			requireRefusal(t, "the control, a bad block on an attempt the route takes", send(nil, badBlock), route.code, route.field+" b9")
			for name, quote := range badQuotes {
				refused := send(nil, quote)
				if refused.Code == http.StatusBadRequest || !strings.Contains(refused.Body.String(), `"code":"TARGET_`) {
					t.Fatalf("the control, %s on an attempt the route takes: status=%d body=%s, want the quote's own refusal",
						name, refused.Code, refused.Body.String())
				}
			}

			createdID(t, send(nil, nil))
			requireSameAnswer("an attempt already answered", nil)
		})
	}
}

// A reply naming a block of an item owed its index reads that block from the index its own
// transaction plans (refs.PlanItemIfOwed), so its quote is checked on the block's whole text there
// (refs.ItemRepair.BlockText): not on the 480-rune excerpt the plan keeps, nor on a text an older
// build never recorded. Each answered post holds the quoted word past its block's 480th character:
// a comment and a message an older build wrote, with no block state, answered on the comment and
// the message routes, whose write phase queues the repair, and a comment and a message whose
// blocks are marked owed, answered on the two delivery-reply routes, which write it at once.
func TestAReplyQuoteOnAnItemThatOwesItsIndexIsCheckedOnItsWholeText(t *testing.T) {
	t.Parallel()
	handler, database, _ := newQuoteHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Quotes of owed posts", "before").Key
	far := strings.Repeat("lorem ipsum ", 45) + "zarquonfar stands past the excerpt."
	older := func(table string) string {
		t.Helper()
		var id string
		if err := database.Pool.QueryRow(context.Background(), `
			insert into `+table+` (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', $2) returning id::text
		`, issue, far).Scan(&id); err != nil {
			t.Fatalf("insert into %s as an older build: %v", table, err)
		}
		return id
	}
	olderComment, olderMessage := older("comments"), older("messages")
	mentioned := decodeMentionedComment(t, postMentionedComment(t, handler, issue, map[string]any{
		"body": far, "mentions": []map[string]any{{"target": "session:" + quoteSession}},
	})).ID
	oweCommentIndex(t, database, mentioned)
	targeted := createIssueMessage(t, handler, issue, map[string]any{
		"body": far, "target": "session:" + quoteSession, "delivery": "btw",
	}, "alice").ID
	oweMessageIndex(t, database, targeted)

	const (
		commentState = `select body_sha256, owed_at is null from comment_block_state where comment_id = $1`
		messageState = `select body_sha256, owed_at is null from message_block_state where message_id = $1`
	)
	for _, row := range []struct {
		name, path, state, item string
		input                   map[string]any
		bearer                  bool
	}{
		{"a comment an older build wrote, on POST /api/v1/issues/{key}/comments", "/api/v1/issues/" + issue + "/comments",
			commentState, olderComment, map[string]any{"reply_to": olderComment, "reply_to_block": "b1"}, false},
		{"a message an older build wrote, on POST /api/v1/issues/{key}/messages", "/api/v1/issues/" + issue + "/messages",
			messageState, olderMessage, map[string]any{"in_reply_to": olderMessage, "in_reply_to_block": "b1"}, false},
		{"a comment owed its index, on POST /api/v1/comments/{id}/reply", "/api/v1/comments/" + mentioned + "/reply",
			commentState, mentioned, map[string]any{"reply_to_block": "b1"}, true},
		{"a message owed its index, on POST /api/v1/messages/{id}/reply", "/api/v1/messages/" + targeted + "/reply",
			messageState, targeted, map[string]any{"in_reply_to_block": "b1"}, true},
	} {
		input := row.input
		input["body"], input["quote"] = "On the far word.", "zarquonfar"
		var written *httptest.ResponseRecorder
		if row.bearer {
			written = bearerRequest(t, handler, http.MethodPost, row.path, callbackAs(quoteSession, input))
		} else {
			written = dispatchRequest(t, handler, http.MethodPost, row.path, input, "alice")
		}
		requireQuoted(t, row.name, written.Body.Bytes(), createdID(t, written), quoting("zarquonfar", 0))
		requireItemIndexed(t, database, row.name, row.state, row.item, far)
	}
}
