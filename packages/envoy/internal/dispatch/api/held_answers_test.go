package api

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A held write's answer is the answer its post would give, whole (INFRA-3231's R5): each test
// below makes one kind of write twice, held, and as the copy its writer's person typed, which
// posts at once with no hold (Decision 26) and gets the answer its route gives every write it
// posts. It compares the two answers as each went on the wire, every field and every value, less
// the held object and what each write makes its own (requireHeldAnswerAsPosted), so a field a held
// answer leaves out, or states otherwise than its post's answer, fails its kind's test, whichever
// field it is. A decision block's hold answers no write: the document write that holds it has
// answered before its settlement opens the block (decisionBlockReviewer.AskBlockOpened).

// heldAnswerPostOnly are the fields of a write's answer that only its post makes, which a held
// answer leaves out, each a dotted path into the answer: an anchor's mark id, the ask.opened
// event's sequence, an issue's rank, its document, its event sequence and the revision of its own
// relations, and the deliveries a post sends.
var heldAnswerPostOnly = []string{
	"anchor.mark_id", "opened_event_id", "rank", "primary_artifact_id", "last_seq", "advances_revision", "deliveries",
}

// heldAnswerOwn are the fields each of two writes fills with a value of its own: when it was
// written, and an issue's number.
var heldAnswerOwn = []string{"created_at", "updated_at", "number"}

// requireHeldAnswerAsPosted fails t unless held, a held write's answer, and posted, the answer of
// the same write posted at once, carry the same status and the same fields with the same values,
// but for the held object, the fields only a post makes (heldAnswerPostOnly), the values each
// write fills with its own (heldAnswerOwn), which both must carry, and the ids heldIDs and
// postedIDs map, each read as the name it maps to: the write's own id, the text it answers.
func requireHeldAnswerAsPosted(t *testing.T, what string, held, posted *httptest.ResponseRecorder, heldIDs, postedIDs map[string]string) {
	t.Helper()
	requireHeldAnswerAsPostedLess(t, what, held, posted, heldIDs, postedIDs, nil)
}

// requireHeldAnswerAsPostedLess is requireHeldAnswerAsPosted with postOnly, more dotted paths that
// only this write's post makes, left out of both answers.
func requireHeldAnswerAsPostedLess(t *testing.T, what string, held, posted *httptest.ResponseRecorder, heldIDs, postedIDs map[string]string, postOnly []string) {
	t.Helper()
	if strings.Contains(posted.Body.String(), `"held"`) {
		t.Fatalf("%s, posted at once: %d %s, want no held object", what, posted.Code, posted.Body.String())
	}
	heldAnswer, postedAnswer := comparedAnswer(t, held, heldIDs, postOnly), comparedAnswer(t, posted, postedIDs, postOnly)
	if held.Code != posted.Code || heldAnswer != postedAnswer {
		t.Errorf("%s: the held answer is not the posted one:\nheld   %d %s\nposted %d %s", what, held.Code, heldAnswer, posted.Code, postedAnswer)
	}
}

// comparedAnswer is response's answer as requireHeldAnswerAsPosted compares it, less postOnly.
func comparedAnswer(t *testing.T, response *httptest.ResponseRecorder, ids map[string]string, postOnly []string) string {
	t.Helper()
	var answer map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", response.Body.String(), err)
	}
	delete(answer, "held")
	for _, path := range append(slices.Clone(heldAnswerPostOnly), postOnly...) {
		withoutField(answer, strings.Split(path, "."))
	}
	for _, field := range heldAnswerOwn {
		if _, carried := answer[field]; carried {
			answer[field] = "its own"
		}
	}
	return asJSON(t, renamedIDs(answer, ids))
}

// withoutField deletes the field at path from object, where object holds it.
func withoutField(object map[string]any, path []string) {
	if len(path) == 1 {
		delete(object, path[0])
		return
	}
	if child, isObject := object[path[0]].(map[string]any); isObject {
		withoutField(child, path[1:])
	}
}

// renamedIDs is node with each string in it that ids maps read as the name it maps to.
func renamedIDs(node any, ids map[string]string) any {
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			value[key] = renamedIDs(child, ids)
		}
	case []any:
		for index, child := range value {
			value[index] = renamedIDs(child, ids)
		}
	case string:
		if name, named := ids[value]; named {
			return name
		}
	}
	return node
}

// postedID is the id of a write posted at once: its id, an issue's key.
func postedID(t *testing.T, posted *httptest.ResponseRecorder) string {
	t.Helper()
	answer := decodeBody[heldWriteAnswer](t, posted)
	return cmp.Or(answer.ID, answer.Key)
}

// A held ask's whole answer is its post's: the question, the options, the anchor less the mark id,
// the document the anchor quotes, whom the ask waits on, and the advice.
func TestAHeldAsksWholeAnswerIsItsPostsAnswer(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Asks: ReviewHold, WriterWindow: time.Minute}, nil)
	rig.settleTermStats("RVW")
	asks := "/api/v1/issues/" + rig.issue + "/asks"
	ask := map[string]any{
		"question": "Ship the reviewer?\n" + vacuousPhrase,
		"options":  []map[string]string{{"label": "Ship it", "description": "Release it today."}, {"label": "Do not file", "description": "Wait."}},
		"multiple": true, "urgency": "high",
		"anchor": map[string]any{"artifact": specOf(t, rig.database, rig.issue), "quote": "Seed paragraph"},
	}
	held := rig.asWriter(http.MethodPost, asks, ask)
	id := rig.held(held, "the held ask").ID
	rig.waitHeld(id, heldAwaitingWriter)
	posted := rig.asWriterTyped(http.MethodPost, asks, ask)
	requireHeldAnswerAsPosted(t, "the ask", held, posted, map[string]string{id: "the ask"}, map[string]string{postedID(t, posted): "the ask"})
}

// A held comment's whole answer is its post's: a comment on a quote, its anchor less the mark id;
// a suggestion, what it suggests; and a reply on an ask, its turn and whom the ask waits on once
// it posts (ask_waiting_on); each with its advice.
func TestAHeldCommentsWholeAnswerIsItsPostsAnswer(t *testing.T) {
	rig := newReviewRig(t, holdComments(), nil)
	asked := rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{"question": "Which port should the node use?"})
	if asked.Code != http.StatusCreated {
		t.Fatalf("the writer's ask: %d %s", asked.Code, asked.Body.String())
	}
	quote := map[string]any{"artifact": specOf(t, rig.database, rig.issue), "quote": "Seed paragraph"}
	comments := "/api/v1/issues/" + rig.issue + "/comments"
	for _, comment := range []struct {
		name string
		body map[string]any
	}{
		{"on a quote", map[string]any{"body": "Status\nWe spin up the node. " + vacuousPhrase, "anchor": quote}},
		{"a suggestion", map[string]any{"body": "Status\nName the seed. " + vacuousPhrase, "anchor": quote, "suggestion": map[string]any{"replace_with": "Seed text"}}},
		{"on an ask", map[string]any{"body": "Status\nThe node answers on 8080. " + vacuousPhrase, "ask_id": decodeBody[model.Ask](t, asked).ID}},
	} {
		held := rig.asWriter(http.MethodPost, comments, comment.body)
		id := rig.held(held, "the held comment "+comment.name).ID
		rig.waitHeld(id, heldAwaitingWriter)
		posted := rig.asWriterTyped(http.MethodPost, comments, comment.body)
		requireHeldAnswerAsPosted(t, "the comment "+comment.name, held, posted, map[string]string{id: "the comment"}, map[string]string{postedID(t, posted): "the comment"})
		// The next comment would wait for its turn behind this one and count it in its advice, as
		// its post finds it posted (TestAHeldTextWaitingForItsTurnAnswersAsItsPost): posted now, it
		// is counted by the held answer and its typed copy alike.
		rig.postedAsWritten(id)
	}
}

// A held message's whole answer is its post's: the message it replies to, the block it names, and
// its advice.
func TestAHeldMessagesWholeAnswerIsItsPostsAnswer(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	messages := "/api/v1/issues/" + rig.issue + "/messages"
	root := dispatchRequest(t, rig.handler, http.MethodPost, messages, map[string]any{"body": "One.\n\nTwo."}, "alice")
	if root.Code != http.StatusCreated {
		t.Fatalf("alice's message: %d %s", root.Code, root.Body.String())
	}
	message := map[string]any{
		"body": "Status\nWe spin up the node. " + vacuousPhrase, "in_reply_to": decodeBody[model.Message](t, root).ID, "in_reply_to_block": "b2",
	}
	held := rig.asWriter(http.MethodPost, messages, message)
	id := rig.held(held, "the held message").ID
	rig.waitHeld(id, heldAwaitingWriter)
	posted := rig.asWriterTyped(http.MethodPost, messages, message)
	requireHeldAnswerAsPosted(t, "the message", held, posted, map[string]string{id: "the message"}, map[string]string{postedID(t, posted): "the message"})
}

// A held issue's whole answer is its post's: its title, parent, labels, priority, assignee,
// blockers, repository and priorities it advances, each as the posted create answers it, in
// triage, with its advice.
func TestAHeldIssuesWholeAnswerIsItsPostsAnswer(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, WriterWindow: time.Minute}, nil)
	seedPeople(t, rig.database, "bob")
	createTestPriority(t, rig.handler, map[string]any{"id": "ship-the-daemon", "title": "Ship the daemon", "owner": "alice"}, "alice")
	blocker := requireCreatedIssue(t, createIssueRequest(t, rig.handler, map[string]any{
		"project": "RVW", "title": "The exporter's transport of record", "spec": "The exporter sends its batches over a queue.",
	}))
	rig.settleTermStats("RVW")
	create := map[string]any{
		"project": "RVW", "title": "Astrolabe gnomon reseat offsets",
		"spec":   "The astrolabe reads a constant offset after each gnomon reseat.\n\n" + vacuousPhrase,
		"parent": rig.issue, "labels": []string{"reviewer", "astrolabe"}, "priority": 2, "assignee": "bob",
		"blocked_by": []string{blocker.Key}, "repo": "acme/widgets", "advances": []string{"ship-the-daemon"},
	}
	held := rig.asWriter(http.MethodPost, "/api/v1/issues", create)
	key := rig.held(held, "the held issue").ID
	rig.waitHeld(key, heldAwaitingWriter)
	posted := rig.asWriterTyped(http.MethodPost, "/api/v1/issues", create)
	requireHeldAnswerAsPosted(t, "the issue", held, posted, map[string]string{key: "the issue"}, map[string]string{postedID(t, posted): "the issue"})
}

// A held reply to a mention in an ask's thread answers as its post's answer does: the ask it joins,
// the comment it answers, its turn and whom the ask waits on once it posts. Sent again while held,
// it answers as the route answers a reply sent again on an answered attempt: the reply as it posts,
// with no ask_waiting_on, which names whom the ask waits on once a reply is its newest.
func TestAHeldCommentRepliesWholeAnswerIsItsPostsAnswer(t *testing.T) {
	rig := newReviewRig(t, holdComments(), nil)
	asked := rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{"question": "Which port should the node use?"})
	if asked.Code != http.StatusCreated {
		t.Fatalf("the writer's ask: %d %s", asked.Code, asked.Body.String())
	}
	ask := decodeBody[model.Ask](t, asked).ID
	mention := func(body string) string {
		t.Helper()
		comment := decodeMentionedComment(t, postMentionedComment(t, rig.handler, rig.issue, map[string]any{
			"body": body, "ask_id": ask, "mentions": []map[string]any{{"target": "session:" + heldWriter}},
		}))
		settledCommentDeliveries(t, rig.database, comment.ID)
		return comment.ID
	}
	first, second := mention("Is port 8080 free?"), mention("Is port 8081 free?")
	reply := map[string]any{"body": "Status\nThe node answers on 8080. " + vacuousPhrase, "attempt": 1}
	held := rig.asWriter(http.MethodPost, "/api/v1/comments/"+first+"/reply", reply)
	id := rig.held(held, "the held reply").ID
	rig.waitHeld(id, heldAwaitingWriter)
	posted := rig.asWriterTyped(http.MethodPost, "/api/v1/comments/"+second+"/reply", reply)
	heldIDs := map[string]string{id: "the reply", first: "the mention"}
	postedIDs := map[string]string{postedID(t, posted): "the reply", second: "the mention"}
	requireHeldAnswerAsPosted(t, "the reply", held, posted, heldIDs, postedIDs)
	requireHeldAnswerAsPosted(t, "the reply sent again",
		rig.asWriter(http.MethodPost, "/api/v1/comments/"+first+"/reply", reply),
		rig.asWriterTyped(http.MethodPost, "/api/v1/comments/"+second+"/reply", reply), heldIDs, postedIDs)
}

// A held reply to a message answers as its post's answer does: the message it answers and its
// target; sent again while held, as the route answers a reply sent again on an answered attempt,
// marked duplicate; and a held follow-up once the reply posted, the reply it is filed under.
func TestAHeldMessageRepliesWholeAnswerIsItsPostsAnswer(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	first, second := rig.messageToWriter("Is the first node up?"), rig.messageToWriter("Is the second node up?")
	reply := map[string]any{"body": "Status\nWe spin up the node. " + vacuousPhrase, "attempt": 1}
	held := rig.asWriter(http.MethodPost, "/api/v1/messages/"+first+"/reply", reply)
	id := rig.held(held, "the held reply").ID
	rig.waitHeld(id, heldAwaitingWriter)
	posted := rig.asWriterTyped(http.MethodPost, "/api/v1/messages/"+second+"/reply", reply)
	heldIDs := map[string]string{id: "the reply", first: "the message"}
	postedIDs := map[string]string{postedID(t, posted): "the reply", second: "the message"}
	requireHeldAnswerAsPosted(t, "the reply", held, posted, heldIDs, postedIDs)
	requireHeldAnswerAsPosted(t, "the reply sent again",
		rig.asWriter(http.MethodPost, "/api/v1/messages/"+first+"/reply", reply),
		rig.asWriterTyped(http.MethodPost, "/api/v1/messages/"+second+"/reply", reply), heldIDs, postedIDs)
	rig.postedAsWritten(id)
	followUp := map[string]any{"body": "Status\nWe spin up the second node. " + vacuousPhrase, "attempt": 1}
	heldFollowUp := rig.asWriter(http.MethodPost, "/api/v1/messages/"+first+"/reply?follow_up=true", followUp)
	heldIDs[rig.held(heldFollowUp, "the held follow-up").ID] = "the follow-up"
	postedFollowUp := rig.asWriterTyped(http.MethodPost, "/api/v1/messages/"+second+"/reply?follow_up=true", followUp)
	postedIDs[postedID(t, postedFollowUp)] = "the follow-up"
	requireHeldAnswerAsPosted(t, "the follow-up", heldFollowUp, postedFollowUp, heldIDs, postedIDs)
}

// A held ask, comment, message or reply beside a held parent runs no dry run before that parent
// posts, and a held issue under one runs its dry run with no parent and none of the held issues it
// waits on among its blockers, of which it keeps only its spec as its seed renders it and the
// blocks its advice reads off that spec (heldIssueCreateError, issueCreate.beside). Its answer is
// still its post's answer, whole, as each test below but one holds it: the parent is held, the
// write is held beside it, the parent posts and the write waits on its writer, and the writer's
// typed copy of the write, which posts at once once the parent is posted, gets the answer the held
// one must equal (TestAHeldIssueUnderAHeldIssueReadsItsSpecAsItsSeedRendersIt leaves out its
// related items). TestAHeldIssueUnderAHeldIssueAnswersItsPostsAdvice posts nothing and compares
// no typed copy: it checks the held child's answer alone, its parent and the advice of a new issue
// in triage that Legion owns, against what it wants. Whom it waits on, its thread, the block it
// names in a held text, an issue's assignee and its advice, but for the blocks of an issue's spec,
// are read off the held texts it waits on, as its post will find them once they have posted
// (heldBeside).

// A held ask on a held issue answers as its post's answer does: whom it waits on, the assignee
// the issue's post writes, and the advice of the issue the issue's post files, in triage.
func TestAHeldAskOnAHeldIssueAnswersAsItsPost(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, Asks: ReviewHold, WriterWindow: time.Minute}, nil)
	rig.settleTermStats("RVW")
	issue := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Astrolabe gnomon reseat offsets",
		"spec": "The astrolabe reads a constant offset after each gnomon reseat.\n\n" + vacuousPhrase,
	}), "the held issue").ID
	asks := "/api/v1/issues/" + issue + "/asks"
	ask := map[string]any{
		"question": "Reseat the gnomon before the next reading?\n" + vacuousPhrase,
		"options":  []map[string]string{{"label": "Reseat it", "description": "Reseat it today."}, {"label": "Wait", "description": "Read first."}},
		"urgency":  "high",
	}
	held := rig.asWriter(http.MethodPost, asks, ask)
	id := rig.held(held, "the held ask on the held issue").ID
	rig.postedAsWritten(issue)
	rig.waitHeld(id, heldAwaitingWriter)
	posted := rig.asWriterTyped(http.MethodPost, asks, ask)
	requireHeldAnswerAsPosted(t, "the ask on the held issue", held, posted, map[string]string{id: "the ask"}, map[string]string{postedID(t, posted): "the ask"})
}

// A held comment that answers a held ask answers as its post's answer does: the ask it joins, its
// turn, whom the ask waits on once it posts (ask_waiting_on), and the advice of its issue once the
// ask has posted, the ask among the session's open asks and counted among its writes.
func TestAHeldCommentOnAHeldAskAnswersAsItsPost(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Asks: ReviewHold, Comments: ReviewHold, WriterWindow: time.Minute}, nil)
	ask := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{
		"question": "Which port should the node use?\n" + vacuousPhrase,
	}), "the held ask").ID
	comments := "/api/v1/issues/" + rig.issue + "/comments"
	comment := map[string]any{"body": "Status\nThe node answers on 8080. " + vacuousPhrase, "ask_id": ask}
	held := rig.asWriter(http.MethodPost, comments, comment)
	id := rig.held(held, "the held comment on the held ask").ID
	rig.postedAsWritten(ask)
	rig.waitHeld(id, heldAwaitingWriter)
	posted := rig.asWriterTyped(http.MethodPost, comments, comment)
	requireHeldAnswerAsPosted(t, "the comment on the held ask", held, posted, map[string]string{id: "the comment"}, map[string]string{postedID(t, posted): "the comment"})
}

// A held reply to a held comment answers as its post's answer does: the comment it answers, the
// head of that comment's thread as its reply_to, and the advice of its issue once that comment
// has posted, counted among the session's writes.
func TestAHeldReplyToAHeldCommentAnswersAsItsPost(t *testing.T) {
	rig := newReviewRig(t, holdComments(), nil)
	comments := "/api/v1/issues/" + rig.issue + "/comments"
	root := rig.held(rig.asWriter(http.MethodPost, comments, map[string]any{"body": "Status\nWe spin up the node. " + vacuousPhrase}), "the held comment").ID
	reply := map[string]any{"body": "Status\nThe node answers on 8080. " + vacuousPhrase, "reply_to": root}
	held := rig.asWriter(http.MethodPost, comments, reply)
	id := rig.held(held, "the held reply to the held comment").ID
	rig.postedAsWritten(root)
	rig.waitHeld(id, heldAwaitingWriter)
	posted := rig.asWriterTyped(http.MethodPost, comments, reply)
	requireHeldAnswerAsPosted(t, "the reply to the held comment", held, posted, map[string]string{id: "the reply"}, map[string]string{postedID(t, posted): "the reply"})
}

// A held message on a held issue answers as its post's answer does, with the advice of the issue
// the issue's post files, in triage.
func TestAHeldMessageOnAHeldIssueAnswersAsItsPost(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	issue := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Astrolabe gnomon reseat offsets",
		"spec": "The astrolabe reads a constant offset after each gnomon reseat.\n\n" + vacuousPhrase,
	}), "the held issue").ID
	messages := "/api/v1/issues/" + issue + "/messages"
	message := map[string]any{"body": "Status\nWe reseat the gnomon. " + vacuousPhrase}
	held := rig.asWriter(http.MethodPost, messages, message)
	id := rig.held(held, "the held message on the held issue").ID
	rig.postedAsWritten(issue)
	rig.waitHeld(id, heldAwaitingWriter)
	posted := rig.asWriterTyped(http.MethodPost, messages, message)
	requireHeldAnswerAsPosted(t, "the message on the held issue", held, posted, map[string]string{id: "the message"}, map[string]string{postedID(t, posted): "the message"})
}

// A held issue whose parent is a held issue answers the advice its post answers, a new issue's in
// triage: no writes and no open asks of the session on it yet, the blocks of its spec as its post
// reads them (decision_blocks), and Legion owning its status where the held parent carries the
// legion label, which the label walk reads off the parent's record while no issues row holds it.
func TestAHeldIssueUnderAHeldIssueAnswersItsPostsAdvice(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, WriterWindow: time.Minute}, nil)
	parent := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Astrolabe gnomon reseat offsets", "labels": []string{"legion"},
		"spec": "The astrolabe reads a constant offset after each gnomon reseat.\n\n" + vacuousPhrase,
	}), "the held parent").ID
	child := rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Bellows valve chatter at idle", "parent": parent,
		"spec": "The bellows valve chatters while the pump idles.\n\n" + vacuousPhrase,
	})
	rig.held(child, "the held child of the held issue")
	var answer struct {
		Parent *string `json:"parent"`
		Advice struct {
			IssueStatus             string      `json:"issue_status"`
			SessionWritesSinceHuman int         `json:"session_writes_since_human"`
			YourOpenAsks            []adviceAsk `json:"your_open_asks"`
			LegionManagedStatus     *bool       `json:"legion_managed_status"`
			DecisionBlocks          *int        `json:"decision_blocks"`
		} `json:"advice"`
	}
	if err := json.Unmarshal(child.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the held child's answer %s: %v", child.Body.String(), err)
	}
	advice := answer.Advice
	if answer.Parent == nil || *answer.Parent != parent || advice.IssueStatus != model.IssueStatusTriage || advice.SessionWritesSinceHuman != 0 ||
		advice.YourOpenAsks == nil || len(advice.YourOpenAsks) != 0 || advice.LegionManagedStatus == nil || !*advice.LegionManagedStatus ||
		advice.DecisionBlocks == nil || *advice.DecisionBlocks != 0 {
		t.Errorf("the held child of a held legion issue answers %s, want its parent, and the advice of a new issue in triage that Legion owns, with its spec's blocks", child.Body.String())
	}
}

// A held issue whose parent is a held issue answers the advice its post answers off its spec as
// the seed renders it, not as sent: a spec written with CR LF line endings around an ask block,
// which the seed reads as the ask block it is (decision_blocks), answers as its post's answer
// does, but for the related items, which the held answer's check, run while the parent is held,
// cannot find the parent among: only the parent's post indexes it.
func TestAHeldIssueUnderAHeldIssueReadsItsSpecAsItsSeedRendersIt(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, WriterWindow: time.Minute}, nil)
	rig.settleTermStats("RVW")
	parent := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Astrolabe gnomon reseat offsets",
		"spec": "The astrolabe reads a constant offset after each gnomon reseat.\n\n" + vacuousPhrase,
	}), "the held parent").ID
	child := map[string]any{
		"project": "RVW", "title": "Bellows valve chatter at idle", "parent": parent,
		"spec": "The bellows valve chatters while the pump idles.\r\n\r\n" +
			":::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\r\nReseat the valve first?\r\n:::\r\n\r\n" + vacuousPhrase,
	}
	held := rig.asWriter(http.MethodPost, "/api/v1/issues", child)
	id := rig.held(held, "the held child of the held issue").ID
	rig.postedAsWritten(parent)
	rig.waitHeld(id, heldAwaitingWriter)
	posted := rig.asWriterTyped(http.MethodPost, "/api/v1/issues", child)
	requireHeldAnswerAsPostedLess(t, "the child of the held issue", held, posted, map[string]string{id: "the issue"}, map[string]string{postedID(t, posted): "the issue"},
		[]string{"advice.suggestions"})
}

// A held text that waits only for its turn behind its writer's earlier held text at its place
// answers as its post's answer does: its post finds that text posted, counted among the session's
// writes in its advice.
func TestAHeldTextWaitingForItsTurnAnswersAsItsPost(t *testing.T) {
	rig := newReviewRig(t, holdComments(), nil)
	comments := "/api/v1/issues/" + rig.issue + "/comments"
	first := rig.held(rig.asWriter(http.MethodPost, comments, map[string]any{"body": "Status\nWe spin up the node. " + vacuousPhrase}), "the first held comment").ID
	comment := map[string]any{"body": "Status\nThe node answers on 8080. " + vacuousPhrase}
	held := rig.asWriter(http.MethodPost, comments, comment)
	answer := rig.held(held, "the comment held behind it")
	if answer.Held.WaitsOn != first {
		t.Fatalf("the second comment waits on %q, want its turn behind %s", answer.Held.WaitsOn, first)
	}
	rig.waitHeld(answer.ID, heldAwaitingWriter)
	rig.postedAsWritten(first)
	posted := rig.asWriterTyped(http.MethodPost, comments, comment)
	requireHeldAnswerAsPosted(t, "the comment behind the held one", held, posted, map[string]string{answer.ID: "the comment"}, map[string]string{postedID(t, posted): "the comment"})
}

// Two held comments on one held issue both wait on that issue, and the second posts after the
// first: the second's held answer counts the first among the session's writes in its advice, as
// its post finds it posted.
func TestTheSecondHeldCommentOnAHeldIssueAnswersAsItsPost(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, Comments: ReviewHold, WriterWindow: time.Minute}, nil)
	issue := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Astrolabe gnomon reseat offsets",
		"spec": "The astrolabe reads a constant offset after each gnomon reseat.\n\n" + vacuousPhrase,
	}), "the held issue").ID
	comments := "/api/v1/issues/" + issue + "/comments"
	first := rig.held(rig.asWriter(http.MethodPost, comments, map[string]any{"body": "Status\nWe reseat the gnomon. " + vacuousPhrase}), "the first held comment").ID
	comment := map[string]any{"body": "Status\nThe offset is gone. " + vacuousPhrase}
	held := rig.asWriter(http.MethodPost, comments, comment)
	answer := rig.held(held, "the second held comment")
	if answer.Held.WaitsOn != issue {
		t.Fatalf("the second comment waits on %q, want the held issue %s", answer.Held.WaitsOn, issue)
	}
	rig.postedAsWritten(issue)
	rig.postedAsWritten(first)
	rig.waitHeld(answer.ID, heldAwaitingWriter)
	posted := rig.asWriterTyped(http.MethodPost, comments, comment)
	requireHeldAnswerAsPosted(t, "the second comment on the held issue", held, posted, map[string]string{answer.ID: "the comment"}, map[string]string{postedID(t, posted): "the comment"})
}

// A held ask on a held issue whose anchor quotes that issue's spec answers as its post's answer
// does: the quote and the document it quotes (anchor_artifact), less what that issue's post makes,
// the document's id, the block and the version.
func TestAHeldAnchoredAskOnAHeldIssueAnswersAsItsPost(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, Asks: ReviewHold, WriterWindow: time.Minute}, nil)
	rig.settleTermStats("RVW")
	issue := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Astrolabe gnomon reseat offsets",
		"spec": "The astrolabe reads a constant offset after each gnomon reseat.\n\n" + vacuousPhrase,
	}), "the held issue").ID
	asks := "/api/v1/issues/" + issue + "/asks"
	ask := map[string]any{
		"question": "Reseat the gnomon before the next reading?\n" + vacuousPhrase,
		"anchor":   map[string]any{"artifact": "spec", "quote": "constant offset"},
	}
	held := rig.asWriter(http.MethodPost, asks, ask)
	id := rig.held(held, "the held anchored ask on the held issue").ID
	rig.postedAsWritten(issue)
	rig.waitHeld(id, heldAwaitingWriter)
	posted := rig.asWriterTyped(http.MethodPost, asks, ask)
	requireHeldAnswerAsPostedLess(t, "the anchored ask on the held issue", held, posted, map[string]string{id: "the ask"}, map[string]string{postedID(t, posted): "the ask"},
		[]string{"anchor.artifact_id", "anchor.block_id", "anchor.version"})
}

// A held comment on a held issue whose anchor quotes that issue's spec answers as its post's
// answer does: the quote its mark covers, not orphaned, less what that issue's post makes, the
// document's id, the block and the version.
func TestAHeldAnchoredCommentOnAHeldIssueAnswersAsItsPost(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, Comments: ReviewHold, WriterWindow: time.Minute}, nil)
	issue := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Astrolabe gnomon reseat offsets",
		"spec": "The astrolabe reads a constant offset after each gnomon reseat.\n\n" + vacuousPhrase,
	}), "the held issue").ID
	comments := "/api/v1/issues/" + issue + "/comments"
	comment := map[string]any{
		"body":   "Status\nThe offset is the gnomon's. " + vacuousPhrase,
		"anchor": map[string]any{"artifact": "spec", "quote": "constant offset"},
	}
	held := rig.asWriter(http.MethodPost, comments, comment)
	id := rig.held(held, "the held anchored comment on the held issue").ID
	rig.postedAsWritten(issue)
	rig.waitHeld(id, heldAwaitingWriter)
	posted := rig.asWriterTyped(http.MethodPost, comments, comment)
	requireHeldAnswerAsPostedLess(t, "the anchored comment on the held issue", held, posted, map[string]string{id: "the comment"}, map[string]string{postedID(t, posted): "the comment"},
		[]string{"anchor.artifact_id", "anchor.block_id", "anchor.version"})
}

// A held ask on a held issue whose anchor's quote differs from the text it covers in that issue's
// spec, by the inline markdown its search reads against the text's marks, answers the text its
// post's mark covers as the anchor's quote, as its post's answer does.
func TestAHeldAskOnAHeldIssueAnswersTheQuoteItsMarkCovers(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, Asks: ReviewHold, WriterWindow: time.Minute}, nil)
	rig.settleTermStats("RVW")
	issue := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Astrolabe gnomon reseat offsets",
		"spec": "The astrolabe reads a `constant` offset after each gnomon reseat.\n\n" + vacuousPhrase,
	}), "the held issue").ID
	asks := "/api/v1/issues/" + issue + "/asks"
	ask := map[string]any{
		"question": "Reseat the gnomon before the next reading?\n" + vacuousPhrase,
		"anchor":   map[string]any{"artifact": "spec", "quote": "a `constant` offset"},
	}
	held := rig.asWriter(http.MethodPost, asks, ask)
	id := rig.held(held, "the held ask quoting inline markdown").ID
	if quote := decodeBody[model.Ask](t, held).Anchor; quote == nil || quote.Quote != "a constant offset" {
		t.Errorf("the held ask's anchor = %#v, want the text its mark covers, \"a constant offset\"", quote)
	}
	rig.postedAsWritten(issue)
	rig.waitHeld(id, heldAwaitingWriter)
	posted := rig.asWriterTyped(http.MethodPost, asks, ask)
	requireHeldAnswerAsPostedLess(t, "the ask quoting inline markdown", held, posted, map[string]string{id: "the ask"}, map[string]string{postedID(t, posted): "the ask"},
		[]string{"anchor.artifact_id", "anchor.block_id", "anchor.version"})
}

// A held follow-up that waits on its writer's held reply to the same delivery answers as its
// post's answer does: filed under that reply, on the delivery's issue and to its target.
func TestAHeldFollowUpBesideAHeldReplyAnswersAsItsPost(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	first, second := rig.messageToWriter("Is the first node up?"), rig.messageToWriter("Is the second node up?")
	reply := map[string]any{"body": "Status\nWe spin up the node. " + vacuousPhrase, "attempt": 1}
	followUp := map[string]any{"body": "Status\nWe spin up the second node. " + vacuousPhrase, "attempt": 1}
	heldReply := rig.held(rig.asWriter(http.MethodPost, "/api/v1/messages/"+first+"/reply", reply), "the held reply").ID
	held := rig.asWriter(http.MethodPost, "/api/v1/messages/"+first+"/reply?follow_up=true", followUp)
	answer := rig.held(held, "the held follow-up beside the held reply")
	if answer.Held.WaitsOn != heldReply {
		t.Fatalf("the held follow-up waits on %q, want the held reply %s", answer.Held.WaitsOn, heldReply)
	}
	postedReply := rig.asWriterTyped(http.MethodPost, "/api/v1/messages/"+second+"/reply", reply)
	posted := rig.asWriterTyped(http.MethodPost, "/api/v1/messages/"+second+"/reply?follow_up=true", followUp)
	requireHeldAnswerAsPosted(t, "the follow-up beside the held reply", held, posted,
		map[string]string{answer.ID: "the follow-up", heldReply: "the reply", first: "the message"},
		map[string]string{postedID(t, posted): "the follow-up", postedID(t, postedReply): "the reply", second: "the message"})
}

// A held follow-up beside its writer's held reply to the same delivery is answered off the one
// reading of the held texts it waits on that chose to read what its post writes rather than run
// its dry run (heldWaitedBeside): the reply posting between that reading and the answer
// (heldBeside) still has the answer file the follow-up under that reply, to the delivery's target,
// as its post files it, not the hold's own answer alone.
func TestAHeldFollowUpIsAnsweredOffTheReadingThatChoseToReadIt(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	delivered := rig.messageToWriter("Is the first node up?")
	reply := rig.held(rig.asWriter(http.MethodPost, "/api/v1/messages/"+delivered+"/reply", map[string]any{
		"body": "Status\nWe spin up the node. " + vacuousPhrase, "attempt": 1,
	}), "the held reply").ID
	followUp := rig.held(rig.asWriter(http.MethodPost, "/api/v1/messages/"+delivered+"/reply?follow_up=true", map[string]any{
		"body": "Status\nWe spin up the second node. " + vacuousPhrase, "attempt": 1,
	}), "the held follow-up").ID
	row := rig.heldRow(followUp)
	write, err := row.write()
	if err != nil {
		t.Fatalf("read the held follow-up's write: %v", err)
	}
	ctx := context.Background()
	waited, err := rig.served.heldWaitedBeside(ctx, write, reply)
	if err != nil || waited == nil {
		t.Fatalf("the held texts the follow-up waits on: %v, %v; want the held reply read", waited, err)
	}
	rig.postedAsWritten(reply)
	rig.waitHeld(reply, heldPosted)
	var target *string
	if err := rig.database.Pool.QueryRow(ctx, `select target from messages where id = $1`, delivered).Scan(&target); err != nil {
		t.Fatalf("read the delivered message's target: %v", err)
	}
	dry, err := rig.served.heldBeside(ctx, write, row.actor, followUp, *waited)
	if err != nil || dry == nil || dry.message == nil || deref(dry.message.InReplyTo) != reply || target == nil || deref(dry.message.Target) != *target {
		t.Errorf("the follow-up read beside the reply that posted meanwhile: %#v, %v; want it filed under %s to %v", dry, err, reply, target)
	}
}

// A new issue held beside its held parent holds one connection of the shared pool at a time: the
// dry run of every check of its post (heldIssueCreateError), which begins a transaction of its own
// on the post's context (dryRunHeld), runs before the hold's read transaction opens (heldBeside).
// A request that holds one connection while it waits for another can wait on the pool for ever,
// and store.ErrNestedAcquire cannot refuse the dry run's, whose context carries a mark of its own.
// So the hold is read again on a server whose pool holds one connection, with the request and the
// server's lifetime, which the post's context is made from (postContext), both ending at the
// deadline below: a hold that asked for the dry run's connection while its transaction held that
// one would wait for itself until then, which a hold taking the one connection in turn is nowhere
// near.
func TestAHeldIssueBesideAHeldParentHoldsOneConnectionAtATime(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, WriterWindow: time.Minute}, nil)
	parent := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Astrolabe gnomon reseat offsets",
		"spec": "The astrolabe reads a constant offset after each gnomon reseat.\n\n" + vacuousPhrase,
	}), "the held parent").ID
	child := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Bellows valve chatter at idle", "parent": parent,
		"spec": "The bellows valve chatters while the pump idles.\n\n" + vacuousPhrase,
	}), "the held child of the held issue").ID
	row := rig.heldRow(child)
	write, err := row.write()
	if err != nil {
		t.Fatalf("read the held child's write: %v", err)
	}
	oneConnection, _ := oneConnectionStore(t, rig.served.deps)
	deps := rig.served.deps
	deps.Store = oneConnection
	lifetime, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	served := directServer(deps)
	served.lifetime = lifetime
	ctx := store.WithTransactionTracking(lifetime)
	waited, err := served.heldWaitedBeside(ctx, write, parent)
	if err != nil || waited == nil {
		t.Fatalf("the held texts the child waits on: %v, %v; want its held parent read", waited, err)
	}
	dry, err := served.heldBeside(ctx, write, row.actor, child, *waited)
	if err != nil {
		t.Fatalf("the child's hold read on a pool of one connection: %v; want it read with that connection, taken in turn", err)
	}
	if dry == nil || !strings.Contains(dry.markdown, "The bellows valve chatters while the pump idles.") {
		t.Errorf("the child's hold read %#v, want its spec as its seed renders it", dry)
	}
}

// heldBlockPostOnly are the fields of a reply's answer that only the post of the held text whose
// block it names makes, which a held reply beside that text leaves out: the block's node id, which
// that text's index makes, and the node id the reply stores (replies_to_block).
var heldBlockPostOnly = []string{"reply_block.node", "replies_to_block"}

// heldSecondParagraph is the body of a held text whose second paragraph, block b2, a reply beside
// it names, and heldSecondParagraphText that block's text.
const (
	heldSecondParagraph     = "Status\nWe spin up the node.\n\n" + heldSecondParagraphText
	heldSecondParagraphText = "The node answers on 8080. " + vacuousPhrase
)

// requireRefusedAsPosted fails t unless refused, a write beside a held text answered at its hold,
// and posted, the same write sent once that text has posted, are one refusal, a 4xx with the same
// body: a write its post refuses is refused at once, as its post refuses it.
func requireRefusedAsPosted(t *testing.T, what string, refused, posted *httptest.ResponseRecorder) {
	t.Helper()
	if refused.Code < http.StatusBadRequest || refused.Code >= http.StatusInternalServerError ||
		refused.Code != posted.Code || refused.Body.String() != posted.Body.String() {
		t.Errorf("%s: answered at its hold %d %s; its post answers %d %s", what, refused.Code, refused.Body.String(), posted.Code, posted.Body.String())
	}
}

// A held reply that names the second paragraph of a held comment answers as its post's answer
// does: the block's id, its reference and its text, numbered as that comment's post indexes it,
// less the block's node id, which only that index makes. One naming a block the comment does not
// hold is refused at once.
func TestAHeldReplyNamingABlockOfAHeldCommentAnswersAsItsPost(t *testing.T) {
	rig := newReviewRig(t, holdComments(), nil)
	comments := "/api/v1/issues/" + rig.issue + "/comments"
	root := rig.held(rig.asWriter(http.MethodPost, comments, map[string]any{"body": heldSecondParagraph}), "the held comment").ID
	reply := map[string]any{"body": "Status\nIt answers on 8081 now. " + vacuousPhrase, "reply_to": root, "reply_to_block": "b2"}
	held := rig.asWriter(http.MethodPost, comments, reply)
	id := rig.held(held, "the held reply to a block of the held comment").ID
	missing := map[string]any{"body": reply["body"], "reply_to": root, "reply_to_block": "b3"}
	refused := rig.asWriter(http.MethodPost, comments, missing)
	rig.postedAsWritten(root)
	rig.waitHeld(id, heldAwaitingWriter)
	posted := rig.asWriterTyped(http.MethodPost, comments, reply)
	requireReplyBlock(t, "the typed copy", decodeBody[model.Comment](t, posted).ReplyBlock, &model.ReplyBlock{
		Node: "comment/" + root + "#b2", Ref: "dispatch://" + rig.issue + "/comment/" + root + "#b-b2", BlockID: "b2", Text: heldSecondParagraphText,
	})
	requireHeldAnswerAsPostedLess(t, "the reply to a block of the held comment", held, posted,
		map[string]string{id: "the reply"}, map[string]string{postedID(t, posted): "the reply"}, heldBlockPostOnly)
	requireRefusedAsPosted(t, "a reply naming a block the held comment does not hold", refused, rig.asWriterTyped(http.MethodPost, comments, missing))
}

// A held reply that names the second paragraph of a held message answers as its post's answer
// does: the block's id, its reference and its text, numbered as that message's post indexes it,
// less the block's node id, which only that index makes. One naming a block the message does not
// hold is refused at once.
func TestAHeldReplyNamingABlockOfAHeldMessageAnswersAsItsPost(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	messages := "/api/v1/issues/" + rig.issue + "/messages"
	root := rig.held(rig.asWriter(http.MethodPost, messages, map[string]any{"body": heldSecondParagraph}), "the held message").ID
	reply := map[string]any{"body": "Status\nIt answers on 8081 now. " + vacuousPhrase, "in_reply_to": root, "in_reply_to_block": "b2"}
	held := rig.asWriter(http.MethodPost, messages, reply)
	id := rig.held(held, "the held reply to a block of the held message").ID
	missing := map[string]any{"body": reply["body"], "in_reply_to": root, "in_reply_to_block": "b3"}
	refused := rig.asWriter(http.MethodPost, messages, missing)
	rig.postedAsWritten(root)
	rig.waitHeld(id, heldAwaitingWriter)
	posted := rig.asWriterTyped(http.MethodPost, messages, reply)
	requireReplyBlock(t, "the typed copy", decodeBody[model.Message](t, posted).ReplyBlock, &model.ReplyBlock{
		Node: "message/" + root + "#b2", Ref: "dispatch://" + rig.issue + "/message/" + root + "#b-b2", BlockID: "b2", Text: heldSecondParagraphText,
	})
	requireHeldAnswerAsPostedLess(t, "the reply to a block of the held message", held, posted,
		map[string]string{id: "the reply"}, map[string]string{postedID(t, posted): "the reply"}, heldBlockPostOnly)
	requireRefusedAsPosted(t, "a reply naming a block the held message does not hold", refused, rig.asWriterTyped(http.MethodPost, messages, missing))
}

// A held comment that names a part of a held ask, its first option, answers as its post's answer
// does: the part's id, its reference and its text, numbered as that ask's post indexes its
// question and options, less the part's node id, which only that index makes. One naming a part
// the ask does not hold is refused at once.
func TestAHeldReplyNamingAPartOfAHeldAskAnswersAsItsPost(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Asks: ReviewHold, Comments: ReviewHold, WriterWindow: time.Minute}, nil)
	ask := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{
		"question": "Which port should the node use?\n" + vacuousPhrase,
		"options":  []map[string]string{{"label": "Port 8080", "description": "The port it answers on now."}, {"label": "Port 8081", "description": "A free port."}},
	}), "the held ask").ID
	comments := "/api/v1/issues/" + rig.issue + "/comments"
	comment := map[string]any{"body": "Status\nThe node answers on 8080. " + vacuousPhrase, "ask_id": ask, "reply_to_block": "b2"}
	held := rig.asWriter(http.MethodPost, comments, comment)
	id := rig.held(held, "the held comment on a part of the held ask").ID
	missing := map[string]any{"body": comment["body"], "ask_id": ask, "reply_to_block": "b4"}
	refused := rig.asWriter(http.MethodPost, comments, missing)
	rig.postedAsWritten(ask)
	rig.waitHeld(id, heldAwaitingWriter)
	posted := rig.asWriterTyped(http.MethodPost, comments, comment)
	requireReplyBlock(t, "the typed copy", decodeBody[model.Comment](t, posted).ReplyBlock, &model.ReplyBlock{
		Node: "ask/" + ask + "#b2", Ref: askPartRef(rig.issue, ask, "b2"), BlockID: "b2", Text: "Port 8080 — The port it answers on now.",
	})
	requireHeldAnswerAsPostedLess(t, "the comment on a part of the held ask", held, posted,
		map[string]string{id: "the comment"}, map[string]string{postedID(t, posted): "the comment"}, heldBlockPostOnly)
	requireRefusedAsPosted(t, "a comment naming a part the held ask does not hold", refused, rig.asWriterTyped(http.MethodPost, comments, missing))
}

// besideWrite is a write held beside a held text that its post would refuse: what it is, its
// route and its body.
type besideWrite struct {
	what, path string
	body       map[string]any
}

// requireRefusedBeside sends each of writes beside the held texts the rig holds, posts held, each
// as written and in order, and sends each write again as its writer's typed copy, which posts at
// once: each must be refused at its hold as its typed copy is refused once those texts have
// posted (requireRefusedAsPosted).
func (rig *reviewRig) requireRefusedBeside(writes []besideWrite, held ...string) {
	rig.t.Helper()
	refused := make([]*httptest.ResponseRecorder, len(writes))
	for index, write := range writes {
		refused[index] = rig.asWriter(http.MethodPost, write.path, write.body)
	}
	for _, id := range held {
		rig.postedAsWritten(id)
	}
	for index, write := range writes {
		requireRefusedAsPosted(rig.t, write.what, refused[index], rig.asWriterTyped(http.MethodPost, write.path, write.body))
	}
}

// A comment held beside a held text, which runs no dry run, is refused at once wherever its post
// would refuse it: thread fields that contradict each other or name no item, a held comment or ask
// on another issue, a held text that is no comment, a turn its thread takes none of or of no kind,
// and an anchor in a held issue's spec that names another document, a mark, a quote the spec does
// not hold, or nothing.
func TestACommentBesideAHeldTextIsRefusedAtItsHoldAsItsPost(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, Asks: ReviewHold, Comments: ReviewHold, Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	rig.settleTermStats("RVW")
	issue := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Astrolabe gnomon reseat offsets",
		"spec": "The astrolabe reads a constant offset after each gnomon reseat.\n\n" + vacuousPhrase,
	}), "the held issue").ID
	onIssue := "/api/v1/issues/" + rig.issue
	comment := rig.held(rig.asWriter(http.MethodPost, onIssue+"/comments", map[string]any{"body": "Status\nWe spin up the node. " + vacuousPhrase}), "the held comment").ID
	ask := rig.held(rig.asWriter(http.MethodPost, onIssue+"/asks", map[string]any{"question": "Which port should the node use?\n" + vacuousPhrase}), "the held ask").ID
	message := rig.held(rig.asWriter(http.MethodPost, onIssue+"/messages", map[string]any{"body": "Status\nWe reseat the gnomon. " + vacuousPhrase}), "the held message").ID
	body := "Status\nThe node answers on 8080. " + vacuousPhrase
	onHeld := "/api/v1/issues/" + issue + "/comments"
	rig.requireRefusedBeside([]besideWrite{
		{"a reply naming a comment and an ask", onIssue + "/comments", map[string]any{"body": body, "reply_to": comment, "ask_id": ask}},
		{"a reply carrying an anchor", onIssue + "/comments", map[string]any{"body": body, "reply_to": comment, "anchor": map[string]any{"artifact": "spec", "quote": "Seed paragraph"}}},
		{"a reply carrying a suggestion", onIssue + "/comments", map[string]any{"body": body, "reply_to": comment, "suggestion": map[string]any{"replace_with": "Seed text"}}},
		{"a block named with no item", onHeld, map[string]any{"body": body, "reply_to_block": "b1"}},
		{"a reply_to that is no comment id", onHeld, map[string]any{"body": body, "reply_to": "not-a-comment"}},
		{"an ask_id that is no ask id", onHeld, map[string]any{"body": body, "ask_id": "not-an-ask"}},
		{"a reply to a held comment on another issue", onHeld, map[string]any{"body": body, "reply_to": comment}},
		{"a reply to a held ask on another issue", onHeld, map[string]any{"body": body, "ask_id": ask}},
		{"a reply to a held message", onIssue + "/comments", map[string]any{"body": body, "reply_to": message}},
		{"a turn in no ask's thread", onIssue + "/comments", map[string]any{"body": body, "reply_to": comment, "turn": "human"}},
		{"a turn of no kind", onIssue + "/comments", map[string]any{"body": body, "ask_id": ask, "turn": "sideways"}},
		{"an anchor on a document the held issue's post makes none of", onHeld, map[string]any{"body": body, "anchor": map[string]any{"artifact": "notes", "quote": "constant offset"}}},
		{"an anchor quoting what the held issue's spec does not hold", onHeld, map[string]any{"body": body, "anchor": map[string]any{"artifact": "spec", "quote": "no such words"}}},
		{"an anchor naming a mark", onHeld, map[string]any{"body": body, "anchor": map[string]any{"artifact": "spec", "mark_id": "m1"}}},
		{"an anchor naming no document", onHeld, map[string]any{"body": body, "anchor": map[string]any{"artifact": " ", "quote": "constant offset"}}},
	}, issue, comment, ask, message)
}

// An ask held beside a held issue is refused at once wherever its post would refuse its anchor in
// that issue's spec: another document, a quote the spec does not hold or holds twice, a mark, and
// an anchor naming both a quote and a mark.
func TestAnAskOnAHeldIssueIsRefusedAtItsHoldAsItsPost(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, Asks: ReviewHold, WriterWindow: time.Minute}, nil)
	rig.settleTermStats("RVW")
	issue := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Astrolabe gnomon reseat offsets",
		"spec": "The gnomon offset is constant.\n\nThe offset drifts after each reseat.\n\n" + vacuousPhrase,
	}), "the held issue").ID
	asks := "/api/v1/issues/" + issue + "/asks"
	anchored := func(anchor map[string]any) map[string]any {
		return map[string]any{"question": "Reseat the gnomon before the next reading?\n" + vacuousPhrase, "anchor": anchor}
	}
	rig.requireRefusedBeside([]besideWrite{
		{"an anchor on a document the held issue's post makes none of", asks, anchored(map[string]any{"artifact": "notes", "quote": "gnomon offset"})},
		{"an anchor quoting what the spec does not hold", asks, anchored(map[string]any{"artifact": "spec", "quote": "no such words"})},
		{"an anchor quoting what the spec holds twice", asks, anchored(map[string]any{"artifact": "spec", "quote": "offset"})},
		{"an anchor naming a mark", asks, anchored(map[string]any{"artifact": "spec", "mark_id": "m1"})},
		{"an anchor naming a quote and a mark", asks, anchored(map[string]any{"artifact": "spec", "quote": "gnomon offset", "mark_id": "m1"})},
	}, issue)
}

// A text held beside a held issue whose anchor quotes text in that issue's spec where the schema
// allows no mark of its kind - an ask's in a code block, a comment's in the front matter - is
// refused at once, with the 400 its post answers once that issue has posted: the mark its post
// writes is checked at its hold as that post writes it, not only its quote's search.
func TestAMarkTheSpecAllowsNowhereIsRefusedAtItsHoldAsItsPost(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, Asks: ReviewHold, Comments: ReviewHold, WriterWindow: time.Minute}, nil)
	rig.settleTermStats("RVW")
	issue := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Astrolabe gnomon reseat offsets",
		"spec": "---\ntitle: Gnomon offsets\n---\n\nThe astrolabe reads a constant offset.\n\n```\nreseat the gnomon\n```\n\n" + vacuousPhrase,
	}), "the held issue").ID
	rig.requireRefusedBeside([]besideWrite{
		{"an ask quoting a code block", "/api/v1/issues/" + issue + "/asks", map[string]any{
			"question": "Reseat the gnomon before the next reading?\n" + vacuousPhrase,
			"anchor":   map[string]any{"artifact": "spec", "quote": "reseat the gnomon"},
		}},
		{"a comment quoting the front matter", "/api/v1/issues/" + issue + "/comments", map[string]any{
			"body":   "Status\nThe title names the offsets. " + vacuousPhrase,
			"anchor": map[string]any{"artifact": "spec", "quote": "Gnomon offsets"},
		}},
	}, issue)
}

// An ask anchored in a document a write kept beside a held issue makes, whose anchor its hold
// leaves to its post, is failed at its post where that document holds its quote only where an
// ask's mark may not stand: the post's 400 INVALID_ANCHOR is a refusal no later post clears, so the
// held ask fails and its writer is told, as for any other refusal of a post.
func TestAHeldAskWhoseMarkMayNotStandFailsAtItsPostAndItsWriterIsTold(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, Asks: ReviewHold, WriterWindow: time.Minute}, nil)
	rig.settleTermStats("RVW")
	issue := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Astrolabe gnomon reseat offsets",
		"spec": "The astrolabe reads a constant offset after each gnomon reseat.\n\n" + vacuousPhrase,
	}), "the held issue").ID
	upload := rig.held(rig.asKeptWriter(http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", map[string]any{
		"name": "notes", "content": "The gnomon notes.\n\n```\nreseat the gnomon\n```\n",
	}), "the upload kept beside the held issue").ID
	ask := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+issue+"/asks", map[string]any{
		"question": "Reseat the gnomon before the next reading?\n" + vacuousPhrase,
		"anchor":   map[string]any{"artifact": "notes", "quote": "reseat the gnomon"},
	}), "the ask anchored in a code block of the kept document").ID
	rig.postedAsWritten(issue)
	rig.waitHeld(upload, heldPosted)
	rig.waitHeld(ask, heldAwaitingWriter)
	rig.postHeldAsWritten(ask)
	if failed := rig.waitHeld(ask, heldFailed); !strings.Contains(failed.detail, "the post was refused: INVALID_ANCHOR") || !strings.HasSuffix(failed.detail, "; its writer was told") {
		t.Fatalf("the failed ask's detail reads %q", failed.detail)
	}
	if body := rig.waitOneFrameAbout(ask, "did not post"); !strings.Contains(body, "Its post was refused: INVALID_ANCHOR") {
		t.Errorf("the failed ask's frame reads %q", body)
	}
}

// A document uploaded to a held issue is kept beside it and posts right after it, before an ask
// held beside that issue later: the ask anchored in that document, which no record of the hold
// holds, is held, its anchor left to its post, and it posts anchored in the document the upload
// wrote.
func TestAnAskAnchoredInADocumentKeptBesideAHeldIssueIsHeldAndPosts(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, Asks: ReviewHold, WriterWindow: time.Minute}, nil)
	rig.settleTermStats("RVW")
	issue := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Astrolabe gnomon reseat offsets",
		"spec": "The astrolabe reads a constant offset after each gnomon reseat.\n\n" + vacuousPhrase,
	}), "the held issue").ID
	upload := rig.held(rig.asKeptWriter(http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", map[string]any{
		"name": "notes", "content": "The gnomon sits a degree off true.\n",
	}), "the upload kept beside the held issue").ID
	ask := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+issue+"/asks", map[string]any{
		"question": "Reseat the gnomon before the next reading?\n" + vacuousPhrase,
		"anchor":   map[string]any{"artifact": "notes", "quote": "a degree off true"},
	}), "the ask anchored in the kept document").ID
	rig.postedAsWritten(issue)
	rig.waitHeld(upload, heldPosted)
	rig.postedAsWritten(ask)
	rig.waitHeld(ask, heldPosted)
	read := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/asks/"+ask, nil, "alice")
	var posted struct {
		Ask struct {
			Anchor *model.Anchor `json:"anchor"`
		} `json:"ask"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &posted); err != nil {
		t.Fatalf("decode the posted ask %s: %v", read.Body.String(), err)
	}
	var notes string
	if err := rig.database.Pool.QueryRow(context.Background(), `select id::text from artifacts where issue_key = $1 and slug = 'notes'`, issue).Scan(&notes); err != nil {
		t.Fatalf("read the kept document's id: %v", err)
	}
	if anchor := posted.Ask.Anchor; read.Code != http.StatusOK || anchor == nil || anchor.ArtifactID != notes || anchor.Quote != "a degree off true" {
		t.Errorf("the posted ask: %d %s, want it anchored on \"a degree off true\" in %s", read.Code, read.Body.String(), notes)
	}
}

// A message held beside a held text is refused at once wherever its post would refuse the message
// it answers: a held message on another issue, and an id that is no message's.
func TestAMessageBesideAHeldTextIsRefusedAtItsHoldAsItsPost(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	rig.settleTermStats("RVW")
	issue := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Astrolabe gnomon reseat offsets",
		"spec": "The astrolabe reads a constant offset after each gnomon reseat.\n\n" + vacuousPhrase,
	}), "the held issue").ID
	message := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/messages", map[string]any{"body": "Status\nWe reseat the gnomon. " + vacuousPhrase}), "the held message").ID
	onHeld := "/api/v1/issues/" + issue + "/messages"
	body := "Status\nThe gnomon sits square. " + vacuousPhrase
	rig.requireRefusedBeside([]besideWrite{
		{"a reply to a held message on another issue", onHeld, map[string]any{"body": body, "in_reply_to": message}},
		{"a reply to an id that is no message's", onHeld, map[string]any{"body": body, "in_reply_to": "not-a-message"}},
	}, issue, message)
}

// A new issue held beside its held parent is refused at once wherever its post would refuse it: a
// parent in another project, a blocker that does not exist, and a parent so deep, counting the
// held parent above a chain of posted issues, that the issue would sit past the depth cap.
func TestAnIssueUnderAHeldIssueIsRefusedAtItsHoldAsItsPost(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, WriterWindow: time.Minute}, nil)
	createTestProject(t, rig.handler, "OTH")
	rig.settleTermStats("RVW")
	rig.settleTermStats("OTH")
	parent := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Astrolabe gnomon reseat offsets",
		"spec": "The astrolabe reads a constant offset after each gnomon reseat.\n\n" + vacuousPhrase,
	}), "the held parent").ID
	// The deepest posted issue sits parentDepthCap-1 links below its root, so the held issue
	// under it sits at the cap, and an issue under that one past it.
	chain := createChain(t, rig.handler, "RVW", rig.issue, parentDepthCap-1)
	deep := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Escapement wheel slips after each winding", "parent": chain[len(chain)-1],
		"spec": "The escapement wheel slips a tooth after each winding.\n\n" + vacuousPhrase,
	}), "the held issue at the depth cap").ID
	rig.requireRefusedBeside([]besideWrite{
		{"a parent in another project", "/api/v1/issues", map[string]any{"project": "OTH", "title": "Bellows valve chatter at idle", "parent": parent}},
		{"a blocker that does not exist", "/api/v1/issues", map[string]any{"project": "RVW", "title": "Ratchet pawl slips under load", "parent": parent, "blocked_by": []string{"RVW-999"}}},
		{"a parent past the depth cap", "/api/v1/issues", map[string]any{"project": "RVW", "title": "Balance spring loses its set", "parent": deep}},
	}, parent, deep)
}

// A follow-up held beside its writer's held reply to the same delivery is refused at once wherever
// its post would refuse it: one that names a block of the delivered message, filed under that reply
// as it is, and one on an attempt the delivery never made, which no held reply answers and which
// runs its dry run.
func TestAFollowUpBesideAHeldReplyIsRefusedAtItsHoldAsItsPost(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	delivered := rig.messageToWriter("Is the first node up?")
	reply := rig.held(rig.asWriter(http.MethodPost, "/api/v1/messages/"+delivered+"/reply", map[string]any{
		"body": "Status\nWe spin up the node. " + vacuousPhrase, "attempt": 1,
	}), "the held reply").ID
	followUp := "/api/v1/messages/" + delivered + "/reply?follow_up=true"
	rig.requireRefusedBeside([]besideWrite{
		{"a follow-up naming a block", followUp, map[string]any{"body": "Status\nThe node answers on 8080. " + vacuousPhrase, "attempt": 1, "in_reply_to_block": "b1"}},
		{"a follow-up on an attempt never made", followUp, map[string]any{"body": "Status\nThe node answers on 8081. " + vacuousPhrase, "attempt": 9}},
	}, reply)
}
